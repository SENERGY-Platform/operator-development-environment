/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The singleuser server's own API, proxied by the Hub at /user/{name}/. It is
// jupyter_server, not JupyterHub, and the two differ in one way that matters:
// authorisation here is the minted per-user token of hub.MintToken, so a bug in
// this file cannot reach another developer's pod.

// serverAPI is one developer's jupyter_server, reachable through the Hub proxy.
type serverAPI struct {
	baseURL string // e.g. https://hub.example/user/devuser
	token   HubToken
	http    *http.Client
	timeout time.Duration
}

// kernelModel is jupyter_server's kernel resource.
type kernelModel struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	LastActivity   *time.Time `json:"last_activity"`
	ExecutionState string     `json:"execution_state"`
	Connections    int        `json:"connections"`
}

// FileEntry is one item of the workspace listing.
//
// The listing exists for one reason: M4's acceptance is that a file written in
// one session is present in the next, and a developer has to be able to see that
// without opening JupyterLab in another tab. It is not the Code pane of §5.11 -
// that is M7, with a full tree and write access on every file.
type FileEntry struct {
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	Type         string     `json:"type"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"last_modified"`
}

type contentsModel struct {
	Name         string          `json:"name"`
	Path         string          `json:"path"`
	Type         string          `json:"type"`
	Size         int64           `json:"size"`
	LastModified *time.Time      `json:"last_modified"`
	Content      []contentsModel `json:"content"`
}

// createKernel starts a kernel whose working directory is the workspace.
//
// `path` is what makes the acceptance criterion hold. jupyter_server derives the
// kernel's cwd from it, so `open("notes.txt", "w")` in the developer's code lands
// on the per-user PVC rather than in the pod's ephemeral home, and is still there
// after the pod is culled and respawned.
func (s *serverAPI) createKernel(ctx context.Context, name, path string) (kernelModel, error) {
	var kernel kernelModel
	body := map[string]any{"name": name}
	if path != "" {
		body["path"] = path
	}
	if err := s.do(ctx, http.MethodPost, "/api/kernels", body, &kernel); err != nil {
		return kernelModel{}, err
	}
	if kernel.ID == "" {
		return kernelModel{}, &UpstreamError{
			Resource: "/api/kernels", Err: errors.New("the server returned no kernel id"),
		}
	}
	return kernel, nil
}

func (s *serverAPI) getKernel(ctx context.Context, id string) (kernelModel, error) {
	var kernel kernelModel
	err := s.do(ctx, http.MethodGet, "/api/kernels/"+url.PathEscape(id), nil, &kernel)
	return kernel, err
}

func (s *serverAPI) interruptKernel(ctx context.Context, id string) error {
	return s.do(ctx, http.MethodPost, "/api/kernels/"+url.PathEscape(id)+"/interrupt", struct{}{}, nil)
}

func (s *serverAPI) deleteKernel(ctx context.Context, id string) error {
	return s.do(ctx, http.MethodDelete, "/api/kernels/"+url.PathEscape(id), nil, nil)
}

// ensureDirectory creates the workspace, one segment at a time.
//
// The contents API creates a single directory, not a path, so a two-segment
// workspace needs two calls. Creating one that exists is not an error, which is
// what makes this safe to run on every session open.
func (s *serverAPI) ensureDirectory(ctx context.Context, path string) error {
	clean, err := cleanWorkspacePath(path)
	if err != nil {
		return err
	}
	if clean == "" {
		return nil
	}
	var built string
	for _, segment := range strings.Split(clean, "/") {
		if built == "" {
			built = segment
		} else {
			built += "/" + segment
		}
		var model contentsModel
		err := s.do(ctx, http.MethodPut, "/api/contents/"+escapePath(built),
			map[string]any{"type": "directory"}, &model)
		if err != nil {
			return err
		}
	}
	return nil
}

// listDirectory reads one directory of the workspace.
func (s *serverAPI) listDirectory(ctx context.Context, path string) ([]FileEntry, error) {
	clean, err := cleanWorkspacePath(path)
	if err != nil {
		return nil, err
	}
	var model contentsModel
	target := "/api/contents"
	if clean != "" {
		target += "/" + escapePath(clean)
	}
	if err := s.do(ctx, http.MethodGet, target+"?content=1", nil, &model); err != nil {
		return nil, err
	}
	if model.Type != "directory" {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInvalidRequest, clean)
	}
	entries := make([]FileEntry, 0, len(model.Content))
	for _, item := range model.Content {
		entries = append(entries, FileEntry{
			Name:         item.Name,
			Path:         item.Path,
			Type:         item.Type,
			Size:         item.Size,
			LastModified: item.LastModified,
		})
	}
	return entries, nil
}

func (s *serverAPI) do(ctx context.Context, method, path string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return &UpstreamError{Resource: path, Err: err}
	}
	request.Header.Set("Authorization", "token "+string(s.token))
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := s.http.Do(request)
	if err != nil {
		return &UpstreamError{Resource: path, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	if response.StatusCode >= 400 {
		return &UpstreamError{Resource: path, Code: response.StatusCode, Err: hubMessage(response)}
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return &UpstreamError{Resource: path, Code: response.StatusCode, Err: err}
	}
	return nil
}

// usernamePattern is what ODE will put in a URL path.
//
// The Hub username comes from a token claim, and a claim is external input even
// though the gateway validated the signature around it. Rejecting anything
// outside this set is what keeps a username from becoming a path traversal into
// another user's server, and it is stricter than JupyterHub's own normalisation
// on purpose: a name ODE cannot address is better refused than escaped.
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]{0,62}$`)

func validateUsername(name string) error {
	if name == "" {
		return fmt.Errorf("%w: no jupyterhub username could be resolved from the token", ErrInvalidRequest)
	}
	if !usernamePattern.MatchString(name) {
		return fmt.Errorf(
			"%w: %q is not a usable jupyterhub username; ODE will not construct a path from it",
			ErrInvalidRequest, name)
	}
	return nil
}

// cleanWorkspacePath normalises a workspace-relative path and refuses to leave
// the workspace. The developer's own code may write anywhere their pod allows -
// that is their pod — but a path ODE builds from a request parameter may not
// climb out of the directory ODE said it was listing.
func cleanWorkspacePath(path string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return "", nil
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: %q is not a valid workspace path", ErrInvalidRequest, path)
		}
	}
	return trimmed, nil
}

// escapePath escapes each segment while keeping the separators, which
// url.PathEscape alone would not.
func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
