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

package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The slice of GitHub's REST API this package needs, written by hand.
//
// Four calls — who am I, which repositories, create one, what is the latest
// Operator Lib tag — do not justify a client library and its transitive tree. The
// git operations are git's, not the API's, and the scaffold is committed by the
// developer rather than through the git-data API, so nothing here grows.

const (
	githubAccept     = "application/vnd.github+json"
	githubAPIVersion = "2022-11-28"
	// maxRepositoryPages bounds the repository listing. A developer with more than
	// three hundred repositories is picking from a search box, not a list, and an
	// unbounded pagination loop against someone else's API is a bad idea in a
	// request handler.
	maxRepositoryPages = 3
	repositoryPageSize = 100
)

// githubClient is one developer's GitHub, under their own token.
type githubClient struct {
	baseURL string
	token   string
	http    *http.Client
	timeout time.Duration
}

func (s *Service) githubClient(token string) *githubClient {
	return &githubClient{
		baseURL: strings.TrimSuffix(s.opts.APIURL, "/"),
		token:   token,
		http:    s.http,
		timeout: s.opts.RequestTimeout,
	}
}

type githubUser struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Email     string `json:"email"`
}

type githubRepository struct {
	FullName    string `json:"full_name"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Private     bool   `json:"private"`
	Owner       struct {
		Login string `json:"login"`
	} `json:"owner"`
	DefaultBranch string     `json:"default_branch"`
	CloneURL      string     `json:"clone_url"`
	HTMLURL       string     `json:"html_url"`
	PushedAt      *time.Time `json:"pushed_at"`
	Size          int        `json:"size"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

func (r githubRepository) toRepository() Repository {
	return Repository{
		FullName:      r.FullName,
		Name:          r.Name,
		Owner:         r.Owner.Login,
		Description:   r.Description,
		Private:       r.Private,
		DefaultBranch: r.DefaultBranch,
		CloneURL:      r.CloneURL,
		HTMLURL:       r.HTMLURL,
		PushedAt:      r.PushedAt,
		CanPush:       r.Permissions.Push,
		// A repository with no pushes has no commits, which is what makes a clone of
		// it land on an unborn branch. Reported because the pane says different
		// things about an empty repository and a populated one.
		Empty: r.PushedAt == nil,
	}
}

// Viewer is `GET /user`, plus the scopes GitHub reports for the token.
func (c *githubClient) Viewer(ctx context.Context) (Identity, []string, error) {
	identity, scopes, _, err := c.viewer(ctx)
	return identity, scopes, err
}

// viewer is Viewer plus whether GitHub sent the scopes header at all, which is not
// the same question as whether the list is empty.
//
// It tells an OAuth app's token from a GitHub App's user token, and that difference
// decides how a deployment behaves: an OAuth app's token carries scopes and does not
// expire, a GitHub App's user token carries no scopes — no header — expires in hours
// unless the app is configured otherwise, and reaches only the repositories the app
// is installed on. A missing header read as "no scopes granted" would report the
// second as a consent screen the developer did not complete.
func (c *githubClient) viewer(ctx context.Context) (Identity, []string, bool, error) {
	var user githubUser
	header, err := c.do(ctx, http.MethodGet, "/user", nil, &user)
	if err != nil {
		return Identity{}, nil, false, err
	}
	return Identity{
		Login:     user.Login,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
	}, splitScopes(header.Get("X-OAuth-Scopes")), len(header.Values("X-OAuth-Scopes")) > 0, nil
}

// Repositories lists what the developer can work on, most recently pushed first.
//
// `affiliation` includes collaborator and organisation membership because an
// operator repository frequently belongs to the institute rather than to the
// person. Whether they may actually push is carried per repository instead of
// being filtered here, so a read-only repository is visible and explained.
func (c *githubClient) Repositories(ctx context.Context) ([]Repository, error) {
	var all []Repository
	for page := 1; page <= maxRepositoryPages; page++ {
		query := url.Values{
			"per_page":    {fmt.Sprint(repositoryPageSize)},
			"page":        {fmt.Sprint(page)},
			"sort":        {"pushed"},
			"direction":   {"desc"},
			"affiliation": {"owner,collaborator,organization_member"},
		}
		var batch []githubRepository
		if _, err := c.do(ctx, http.MethodGet, "/user/repos?"+query.Encode(), nil, &batch); err != nil {
			return nil, err
		}
		for _, repository := range batch {
			all = append(all, repository.toRepository())
		}
		if len(batch) < repositoryPageSize {
			break
		}
	}
	return all, nil
}

// Repository is `GET /repos/{owner}/{name}`.
func (c *githubClient) Repository(ctx context.Context, owner, name string) (Repository, error) {
	var repository githubRepository
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &repository); err != nil {
		return Repository{}, err
	}
	return repository.toRepository(), nil
}

// createRepositoryRequest is `POST /user/repos`.
//
// `auto_init` is false, and that is the decision the whole scaffold path rests
// on: GitHub's own initial commit would put a README of its choosing on the
// default branch, and the developer's first commit would then be a second one on
// top of it. An empty repository means the scaffold *is* the first commit, made by
// the developer.
type createRepositoryRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Private     bool   `json:"private"`
	AutoInit    bool   `json:"auto_init"`
}

func (c *githubClient) CreateRepository(
	ctx context.Context, name, description string, private bool, organisation string,
) (Repository, error) {
	path := "/user/repos"
	if organisation != "" {
		path = "/orgs/" + url.PathEscape(organisation) + "/repos"
	}
	var created githubRepository
	body := createRepositoryRequest{
		Name: name, Description: description, Private: private, AutoInit: false,
	}
	if _, err := c.do(ctx, http.MethodPost, path, body, &created); err != nil {
		return Repository{}, err
	}
	return created.toRepository(), nil
}

// LatestRef resolves what D15 calls "latest at scaffold time" for a repository.
//
// The newest tag if the project publishes tags, otherwise the newest commit on
// the default branch. A tag is preferred because a pin a human can read is worth
// more in a pyproject than a bare SHA — but Operator Lib is explicitly tracked at
// latest with no stability guarantee, so a SHA is a legitimate answer and not a
// fallback to apologise for.
func (c *githubClient) LatestRef(ctx context.Context, fullName string) (string, error) {
	owner, name, err := splitFullName(fullName)
	if err != nil {
		return "", err
	}
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)

	var tags []struct {
		Name string `json:"name"`
	}
	if _, err := c.do(ctx, http.MethodGet, base+"/tags?per_page=1", nil, &tags); err == nil &&
		len(tags) > 0 && tags[0].Name != "" {
		return tags[0].Name, nil
	}

	var commits []struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, http.MethodGet, base+"/commits?per_page=1", nil, &commits); err != nil {
		return "", err
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", &UpstreamError{
			Resource: base + "/commits",
			Message:  "the repository reports neither a tag nor a commit to pin",
		}
	}
	return commits[0].SHA, nil
}

func (c *githubClient) do(
	ctx context.Context, method, path string, body any, out any,
) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, &UpstreamError{Resource: path, Err: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", githubAccept)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, &UpstreamError{Resource: path, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	if response.StatusCode >= 400 {
		return response.Header, &UpstreamError{
			Resource: path,
			Code:     response.StatusCode,
			Message:  githubMessage(response),
		}
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return response.Header, nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return response.Header, &UpstreamError{
			Resource: path, Code: response.StatusCode, Err: err,
		}
	}
	return response.Header, nil
}

// githubMessage reads GitHub's error body.
//
// The `errors` array is the part worth carrying: "name already exists on this
// account" arrives in there while `message` says only "Repository creation
// failed", and the first is the one the developer can act on.
func githubMessage(response *http.Response) string {
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil || len(raw) == 0 {
		return response.Status
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return strings.TrimSpace(string(raw))
	}
	details := make([]string, 0, len(body.Errors))
	for _, item := range body.Errors {
		detail := firstNonEmpty(item.Message, item.Code)
		if item.Field != "" && detail != "" {
			detail = item.Field + ": " + detail
		}
		if detail != "" {
			details = append(details, detail)
		}
	}
	if len(details) == 0 {
		return firstNonEmpty(body.Message, response.Status)
	}
	return firstNonEmpty(body.Message, response.Status) + " (" + strings.Join(details, "; ") + ")"
}

func splitFullName(fullName string) (string, string, error) {
	owner, name, found := strings.Cut(strings.TrimSpace(fullName), "/")
	if !found || owner == "" || name == "" {
		return "", "", fmt.Errorf("%w: %q is not an owner/name repository", ErrInvalidRequest, fullName)
	}
	if strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%w: %q is not an owner/name repository", ErrInvalidRequest, fullName)
	}
	return owner, name, nil
}
