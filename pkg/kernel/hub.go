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
	"sort"
	"strings"
	"time"
)

// Hub is the JupyterHub REST client. It speaks as ODE's registered service, not
// as any developer: the Hub credential is a deployment secret, and what keeps it
// from becoming a way to act as an arbitrary user is that every call names the
// user explicitly and the API layer only ever passes the caller's own name.
type Hub struct {
	baseURL string
	token   string
	http    *http.Client
	timeout time.Duration
}

// RequiredScopes is what ODE's Hub credential has to hold, and why:
//
//	servers        - POST /users/{name}/server, the spawn of §5.6
//	tokens         - POST /users/{name}/tokens, the per-user credential the
//	                 kernel API is called with
//	access:servers - reaching /user/{name}/api/* at all
//	users:activity - POST /users/{name}/activity, the keep-alive that stops the
//	                 idle culler killing kernel state mid-task (§5.6 item 3)
//
// Checked at startup rather than discovered on someone's first spawn. A partial
// grant is a deployment fault and fails fast, by the deployment decision behind
// this milestone: there is no fallback path that quietly does less.
var RequiredScopes = []string{"servers", "tokens", "access:servers", "users:activity"}

func newHub(baseURL, token string, httpClient *http.Client, timeout time.Duration) *Hub {
	if httpClient == nil {
		// No client-level Timeout: every request carries a context deadline instead,
		// and a spawn poll legitimately outlives a metadata call. This is the same
		// reasoning pkg/timeseries records.
		httpClient = &http.Client{}
	}
	return &Hub{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    httpClient,
		timeout: timeout,
	}
}

// Identity is what GET /hub/api/user says about the credential ODE holds.
type Identity struct {
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Admin  bool     `json:"admin"`
	Roles  []string `json:"roles"`
	Scopes []string `json:"scopes"`
}

// hubServer is one entry of a user's servers map.
type hubServer struct {
	Name         string     `json:"name"`
	Ready        bool       `json:"ready"`
	Pending      string     `json:"pending"`
	URL          string     `json:"url"`
	Started      *time.Time `json:"started"`
	LastActivity *time.Time `json:"last_activity"`
}

type hubUser struct {
	Name    string               `json:"name"`
	Servers map[string]hubServer `json:"servers"`
	// Pending and ServerURL are the pre-named-server fields; JupyterHub still
	// sends them and they are the only thing populated for a user whose servers
	// map the caller may not read.
	Pending   string `json:"pending"`
	ServerURL string `json:"server"`
}

// Whoami reads the credential's own identity and scopes.
func (h *Hub) Whoami(ctx context.Context) (Identity, error) {
	var identity Identity
	if err := h.do(ctx, http.MethodGet, "/hub/api/user", nil, &identity); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

// CheckScopes verifies the credential can do the job, and reports what is only
// half true.
//
// Scopes arrive expanded and filtered — `servers!user=jonah` rather than
// `servers` — so the comparison is on the base name. A filtered grant is not an
// error, because a developer's own token is exactly that and is how this is
// tried locally; it is returned as a warning, so a deployment that accidentally
// configured one developer's token as the service credential is told rather than
// finding out when the second developer spawns.
func (h *Hub) CheckScopes(ctx context.Context) (Identity, []string, error) {
	identity, err := h.Whoami(ctx)
	if err != nil {
		return Identity{}, nil, err
	}

	held := map[string]struct{}{}
	restricted := map[string]string{}
	for _, scope := range identity.Scopes {
		name, filter, hasFilter := strings.Cut(scope, "!")
		held[name] = struct{}{}
		if hasFilter {
			restricted[name] = filter
		}
	}

	var missing []string
	for _, required := range RequiredScopes {
		if _, ok := held[required]; !ok {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return identity, nil, &ScopeError{
			Missing: missing,
			Held:    identity.Scopes,
			Kind:    identity.Kind,
		}
	}

	var warnings []string
	if identity.Kind != "service" {
		warnings = append(warnings, fmt.Sprintf(
			"the jupyterhub credential is a %s token for %q, not a service token; "+
				"every spawn and kernel will be attributed to that account",
			identity.Kind, identity.Name))
	}
	for _, required := range RequiredScopes {
		if filter, ok := restricted[required]; ok {
			warnings = append(warnings, fmt.Sprintf(
				"scope %s is restricted to %s, so ODE can only serve that user", required, filter))
		}
	}
	sort.Strings(warnings)
	return identity, warnings, nil
}

// ServerState is what ODE needs to know about a developer's singleuser server.
type ServerState struct {
	Ready bool `json:"ready"`
	// Pending is "spawn" or "stop" while the Hub is working, empty otherwise.
	Pending string `json:"pending,omitempty"`
	// URL is the server's path under the Hub, e.g. /user/jonah/.
	URL          string     `json:"url,omitempty"`
	Started      *time.Time `json:"started,omitempty"`
	LastActivity *time.Time `json:"last_activity,omitempty"`
}

// ServerState reads the current state of a user's default server.
func (h *Hub) ServerState(ctx context.Context, name string) (ServerState, error) {
	if err := validateUsername(name); err != nil {
		return ServerState{}, err
	}
	var user hubUser
	if err := h.do(ctx, http.MethodGet, "/hub/api/users/"+url.PathEscape(name), nil, &user); err != nil {
		return ServerState{}, err
	}
	if server, ok := user.Servers[""]; ok {
		return ServerState{
			Ready:        server.Ready,
			Pending:      server.Pending,
			URL:          server.URL,
			Started:      server.Started,
			LastActivity: server.LastActivity,
		}, nil
	}
	// No servers map, or no default server in it. The legacy fields still say
	// whether one is up, and a user who has never spawned has neither.
	return ServerState{
		Ready:   user.ServerURL != "" && user.Pending == "",
		Pending: user.Pending,
		URL:     user.ServerURL,
	}, nil
}

// StartServer asks the Hub to spawn the user's default server.
//
// 201 means it is already up, 202 that it is starting. Both are success: the
// caller polls ServerState. A 400 is also treated as success when the server is
// in fact running, because that is what the Hub answers for "already started"
// and failing on it would make a second ODE tab an error.
//
// profile names a KubeSpawner profile slug, and matters whenever the deployment
// offers more than one. §5.6 item 1 puts the ODE image behind an additional
// profile rather than replacing the default, so a spawn that says nothing gets
// the plain notebook image — without Operator Lib, Ray or pandas — which is not
// what a developer opening ODE is asking for.
func (h *Hub) StartServer(ctx context.Context, name, profile string) error {
	if err := validateUsername(name); err != nil {
		return err
	}
	// The body becomes KubeSpawner's user_options. An empty object takes the
	// default profile, which is right when the deployment declares none.
	body := map[string]any{}
	if profile != "" {
		body["profile"] = profile
	}
	err := h.do(ctx, http.MethodPost, "/hub/api/users/"+url.PathEscape(name)+"/server", body, nil)
	var upstream *UpstreamError
	if errors.As(err, &upstream) && upstream.Code == http.StatusBadRequest {
		if state, stateErr := h.ServerState(ctx, name); stateErr == nil && (state.Ready || state.Pending != "") {
			return nil
		}
	}
	return err
}

// StopServer shuts the user's server down. ODE never calls this on its own
// initiative — a pod is the developer's, and stopping it loses in-memory state -
// so it exists for an explicit developer action.
func (h *Hub) StopServer(ctx context.Context, name string) error {
	if err := validateUsername(name); err != nil {
		return err
	}
	return h.do(ctx, http.MethodDelete, "/hub/api/users/"+url.PathEscape(name)+"/server", nil, nil)
}

type mintedToken struct {
	Token     string     `json:"token"`
	ID        string     `json:"id"`
	ExpiresAt *time.Time `json:"expires_at"`
	Scopes    []string   `json:"scopes"`
}

// MintToken issues a short-lived token that reaches exactly one user's server.
//
// Two properties are deliberate. It is scoped to `access:servers!user={name}`
// rather than inheriting ODE's whole grant, so a token that leaks into a log or a
// pod cannot spawn or stop anything. And it expires, because ODE mints one per
// developer per session and the Hub would otherwise accumulate them forever.
func (h *Hub) MintToken(ctx context.Context, name string, ttl time.Duration) (HubToken, time.Time, error) {
	if err := validateUsername(name); err != nil {
		return "", time.Time{}, err
	}
	body := map[string]any{
		"expires_in": int(ttl.Seconds()),
		"note":       "ode: kernel access for " + name,
		"scopes":     []string{"access:servers!user=" + name},
	}
	var minted mintedToken
	if err := h.do(ctx, http.MethodPost,
		"/hub/api/users/"+url.PathEscape(name)+"/tokens", body, &minted); err != nil {
		return "", time.Time{}, err
	}
	if minted.Token == "" {
		return "", time.Time{}, &UpstreamError{
			Resource: "/hub/api/users/" + name + "/tokens",
			Err:      errors.New("the hub returned no token"),
		}
	}
	expiry := time.Now().Add(ttl)
	if minted.ExpiresAt != nil {
		expiry = *minted.ExpiresAt
	}
	return HubToken(minted.Token), expiry, nil
}

// ReportActivity is the keep-alive of §5.6 item 3.
//
// The idle culler kills a server whose last activity is older than its timeout,
// and it counts activity, not liveness: a developer thinking for twenty minutes
// between cells looks exactly like an abandoned pod. The PVC keeps their files
// either way, but the kernel's in-memory state — a loaded dataframe, a fitted
// model — is gone, which is the failure this prevents.
func (h *Hub) ReportActivity(ctx context.Context, name string, at time.Time) error {
	if err := validateUsername(name); err != nil {
		return err
	}
	body := map[string]any{"last_activity": at.UTC().Format(time.RFC3339Nano)}
	return h.do(ctx, http.MethodPost,
		"/hub/api/users/"+url.PathEscape(name)+"/activity", body, nil)
}

// do issues one Hub request under ODE's service credential.
func (h *Hub) do(ctx context.Context, method, path string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reader)
	if err != nil {
		return &UpstreamError{Resource: path, Err: err}
	}
	// "token", not "Bearer": JupyterHub's own scheme for API tokens.
	request.Header.Set("Authorization", "token "+h.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := h.http.Do(request)
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

// hubMessage extracts the Hub's own error text, which is far more useful than
// the status line — a missing scope is reported there by name.
func hubMessage(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	var payload struct {
		Message string `json:"message"`
		Status  int    `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Message != "" {
		return errors.New(payload.Message)
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return errors.New(http.StatusText(response.StatusCode))
	}
	return errors.New(text)
}
