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

// Package repotest is GitHub's API and one repository, in memory and on disk.
//
// It lives outside the test files for the reason kerneltest does: two packages
// need it — pkg/repo tests the service against it and pkg/api tests the routes
// on top — and a second copy of the same double would drift from the first.
//
// What is *not* faked is the part that would matter most: the remote is a real
// bare git repository in a temporary directory, so a clone, a push and a fetch in
// a test are git doing the actual thing. Only the API around it is a double,
// because api.github.com cannot be had locally.
package repotest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Token is the access token the fake exchanges every code for.
const Token = "gho_testtoken0123456789"

// GitHub is the API surface pkg/repo uses: the OAuth exchange, the viewer, the
// repository list, repository creation, and the two endpoints the Operator Lib
// pin is resolved from.
type GitHub struct {
	server   *httptest.Server
	cloneURL string

	mux sync.Mutex
	// created records what repository creation was asked for, so a test can assert
	// auto_init stayed false — the scaffold path depends on the repository being
	// empty.
	created []map[string]any
	calls   []string

	// repositories is what the listing answers with. Nil answers with one
	// repository pointing at the fake's own remote.
	repositories []map[string]any
	// latestTag is what the Operator Lib pin resolves to. Empty makes the tags
	// endpoint answer nothing, which is the path that falls back to a commit.
	latestTag     string
	latestCommit  string
	tokenScopes   string
	defaultBranch string
	// revoked answers every authenticated call with 401, whatever the credential.
	revoked bool
	// rateLimited answers 403, which GitHub uses for a rate limit and for a grant too
	// narrow — neither of which means the credential should be replaced.
	rateLimited bool
}

// NewGitHub starts the double. cloneURL is what every repository reports as its
// clone URL — normally Remote's file:// path.
func NewGitHub(t testing.TB, cloneURL string) *GitHub {
	t.Helper()
	fake := &GitHub{
		cloneURL:      cloneURL,
		latestTag:     "v1.3.1",
		latestCommit:  "0123456789abcdef0123456789abcdef01234567",
		tokenScopes:   "repo, workflow",
		defaultBranch: "main",
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.route))
	t.Cleanup(fake.server.Close)
	return fake
}

// URL is the API base, which the fake also serves the OAuth endpoints from.
func (f *GitHub) URL() string { return f.server.URL }

// Client is an HTTP client that reaches it.
func (f *GitHub) Client() *http.Client { return f.server.Client() }

// SetRevoked makes every authenticated call answer 401, which is how a test
// reproduces the one thing ODE cannot see from its own store: a token the developer
// revoked on GitHub, or one whose authorisation expired. The stored row is
// untouched and still says "connected", exactly as in the real case.
func (f *GitHub) SetRevoked(revoked bool) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.revoked = revoked
}

// SetRateLimited makes every authenticated call answer 403, the way GitHub answers a
// rate limit. Distinct from SetRevoked because ODE must not read the two as the same
// thing: one is a credential to replace, the other is a wait.
func (f *GitHub) SetRateLimited(limited bool) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.rateLimited = limited
}

// SetTokenScopes changes what the credential is reported to hold, which is how a
// test reproduces a developer narrowing the grant on the consent screen.
func (f *GitHub) SetTokenScopes(scopes string) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.tokenScopes = scopes
}

// SetLatestTag changes what the Operator Lib pin resolves to.
func (f *GitHub) SetLatestTag(tag string) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.latestTag = tag
}

// SetRepositories replaces the listing. Each entry is a full name.
func (f *GitHub) SetRepositories(fullNames ...string) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.repositories = nil
	for _, fullName := range fullNames {
		f.repositories = append(f.repositories, f.repositoryLocked(fullName))
	}
}

// Created is what repository creation was asked for, in order.
func (f *GitHub) Created() []map[string]any {
	f.mux.Lock()
	defer f.mux.Unlock()
	return append([]map[string]any(nil), f.created...)
}

// Calls is every request the fake answered, as "METHOD /path".
func (f *GitHub) Calls() []string {
	f.mux.Lock()
	defer f.mux.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *GitHub) repositoryLocked(fullName string) map[string]any {
	owner, name, _ := strings.Cut(fullName, "/")
	return map[string]any{
		"full_name":      fullName,
		"name":           name,
		"owner":          map[string]any{"login": owner},
		"private":        false,
		"default_branch": f.defaultBranch,
		"clone_url":      f.cloneURL,
		"html_url":       "https://github.test/" + fullName,
		"permissions":    map[string]any{"push": true},
	}
}

func (f *GitHub) route(w http.ResponseWriter, r *http.Request) {
	f.mux.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	scopes := f.tokenScopes
	revoked := f.revoked
	limited := f.rateLimited
	f.mux.Unlock()

	// A revoked credential is refused everywhere except the OAuth exchange, which
	// is what reconnecting uses to get a working one.
	if revoked && r.URL.Path != "/login/oauth/access_token" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	if limited && r.URL.Path != "/login/oauth/access_token" {
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		return
	}

	switch {
	case r.URL.Path == "/login/oauth/access_token":
		f.write(w, map[string]any{
			"access_token": Token, "token_type": "bearer", "scope": scopes,
		})

	case r.URL.Path == "/user" && r.Method == http.MethodGet:
		// The credential is required here, because a token GitHub will not answer
		// this with is one ODE must not store.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"message":"Requires authentication"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-OAuth-Scopes", scopes)
		f.write(w, map[string]any{"login": "jonah", "name": "Jonah", "avatar_url": ""})

	case r.URL.Path == "/user/repos" && r.Method == http.MethodGet:
		f.mux.Lock()
		listed := f.repositories
		if listed == nil {
			listed = []map[string]any{f.repositoryLocked("jonah/existing-operator")}
		}
		f.mux.Unlock()
		// One page only: the client stops paginating on a short page, and answering
		// the same page forever would loop until its own cap.
		if r.URL.Query().Get("page") != "1" {
			listed = nil
		}
		f.write(w, listed)

	case r.URL.Path == "/user/repos" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		f.mux.Lock()
		f.created = append(f.created, body)
		created := f.repositoryLocked("jonah/" + name)
		f.mux.Unlock()
		w.WriteHeader(http.StatusCreated)
		f.write(w, created)

	case strings.HasSuffix(r.URL.Path, "/tags"):
		f.mux.Lock()
		tag := f.latestTag
		f.mux.Unlock()
		if tag == "" {
			f.write(w, []map[string]any{})
			return
		}
		f.write(w, []map[string]any{{"name": tag}})

	case strings.HasSuffix(r.URL.Path, "/commits"):
		f.mux.Lock()
		commit := f.latestCommit
		f.mux.Unlock()
		f.write(w, []map[string]any{{"sha": commit}})

	case strings.HasPrefix(r.URL.Path, "/repos/"):
		f.mux.Lock()
		repository := f.repositoryLocked(strings.TrimPrefix(r.URL.Path, "/repos/"))
		f.mux.Unlock()
		f.write(w, repository)

	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

func (f *GitHub) write(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// RequireGit skips the test when there is no git to run. Loudly, because a green
// run without it proves nothing about the operations under test.
func RequireGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed, so the repository operations cannot be exercised")
	}
}

// Remote creates an empty bare repository and returns its path. Its default
// branch is `main`, matching what GitHub creates today.
func Remote(t testing.TB) string {
	t.Helper()
	RequireGit(t)
	path := filepath.Join(t.TempDir(), "operator.git")
	Git(t, "", "init", "--bare", "--initial-branch=main", path)
	return path
}

// Git runs git on the test's own filesystem, for the remote's side of a fixture.
//
// The two GIT_CONFIG_* variables matter: without them a developer's own global
// configuration — a commit template, a signing key, a different default branch —
// changes what these tests do.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.org",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.org",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
