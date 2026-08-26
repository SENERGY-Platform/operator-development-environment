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

package api_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
)

// The M7 routes, over the same doubles pkg/repo uses: a real git against a real
// bare remote, in a directory standing in for the PVC, with GitHub's API faked.
// So these tests are about the HTTP surface — the status codes, the shapes, the
// role gate — on top of behaviour that is exercised for real one layer down.

type repoHarness struct {
	server    *httptest.Server
	github    *repotest.GitHub
	workspace string
	remote    string
}

func newRepoHarness(t *testing.T) *repoHarness {
	t.Helper()
	repotest.RequireGit(t)

	home := t.TempDir()
	workspace := filepath.Join(home, "data", "ode")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	hub := kerneltest.NewHub(t)
	hub.OnExecute(kerneltest.PythonExecutor(t, home))
	kernelService, err := kernel.New(kernel.Options{
		BaseURL:        hub.URL(),
		Token:          "service-token",
		WorkspacePath:  "data/ode",
		SpawnTimeout:   10 * time.Second,
		RequestTimeout: 10 * time.Second,
		ExecuteTimeout: 60 * time.Second,
		MaxOutputBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("kernel.New: %v", err)
	}
	t.Cleanup(kernelService.Close)

	remote := repotest.Remote(t)
	github := repotest.NewGitHub(t, "file://"+remote)
	sealer, err := repo.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	service, err := repo.New(repo.Deps{
		Workspace:  kernelService,
		Store:      repo.NewMemoryStore(),
		Sealer:     sealer,
		HTTPClient: github.Client(),
		Options: repo.Options{
			ClientID:       "client",
			ClientSecret:   "secret",
			APIURL:         github.URL(),
			WebURL:         github.URL(),
			RedirectURI:    "http://localhost:5173/github/callback",
			CommandTimeout: 60 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("repo.New: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer"},
		api.Deps{Kernel: kernelService, Repo: service},
	)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &repoHarness{server: server, github: github, workspace: workspace, remote: remote}
}

func (h *repoHarness) call(
	t *testing.T, method, path string, body any, roles ...string,
) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if roles != nil {
		request.Header.Set("Authorization", "Bearer "+mintToken(roles))
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (h *repoHarness) decode(t *testing.T, response *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// connect completes the OAuth flow through the routes, which is also the only
// test that the two halves of it fit together.
func (h *repoHarness) connect(t *testing.T) {
	t.Helper()
	response := h.call(t, http.MethodPost, "/repo/connection/authorize", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authorize = %d", response.StatusCode)
	}
	var authorize repo.AuthorizeRequest
	h.decode(t, response, &authorize)

	response = h.call(t, http.MethodPost, "/repo/connection",
		map[string]string{"code": "code-1", "state": authorize.State}, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("connect = %d: %s", response.StatusCode, body)
	}
}

// create is the state the file and commit tests start from.
func (h *repoHarness) create(t *testing.T) repo.Status {
	t.Helper()
	response := h.call(t, http.MethodPost, "/repo/repositories",
		map[string]any{"name": "pv-forecast", "description": "Forecast PV generation"},
		"developer")
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create = %d: %s", response.StatusCode, body)
	}
	var status repo.Status
	h.decode(t, response, &status)
	return status
}

func TestTheRepoRoutesAreBehindTheDeveloperRole(t *testing.T) {
	h := newRepoHarness(t)

	if response := h.call(t, http.MethodGet, "/repo/connection", nil); response.StatusCode !=
		http.StatusUnauthorized {
		t.Errorf("without a token = %d, want 401", response.StatusCode)
	}
	if response := h.call(t, http.MethodGet, "/repo/connection", nil, "analyst"); response.StatusCode !=
		http.StatusForbidden {
		t.Errorf("without the developer role = %d, want 403", response.StatusCode)
	}
}

func TestTheRepoRoutesAreNotServedWithoutAGithubApp(t *testing.T) {
	// The M0 harness has no repo service, which is a deployment with no
	// github_client_id. The routes must be absent rather than panicking on nil.
	h := newHarness(t)

	if response := h.get(t, "/repo", "developer"); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no GitHub app is configured", response.Code)
	}
	if response := h.get(t, "/repo/files", "developer"); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.Code)
	}
}

// A developer who has not connected GitHub gets 409 with what is missing, not 500
// and not 404: the request was fine and the answer is a step they have not taken.
func TestWithoutAConnectionTheRoutesSayWhatIsMissing(t *testing.T) {
	h := newRepoHarness(t)

	response := h.call(t, http.MethodGet, "/repo/connection", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var connection struct {
		Connected       bool     `json:"connected"`
		ScopesRequested []string `json:"scopes_requested"`
	}
	h.decode(t, response, &connection)
	if connection.Connected || len(connection.ScopesRequested) != 2 {
		t.Errorf("connection = %+v, want disconnected and the two scopes", connection)
	}

	response = h.call(t, http.MethodGet, "/repo/repositories", nil, "developer")
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.StatusCode)
	}
	var refusal map[string]any
	h.decode(t, response, &refusal)
	if refusal["needs"] != "github_connection" {
		t.Errorf("body = %v, want what is missing named", refusal)
	}

	// And with a connection but no repository, the other one.
	h.connect(t)
	response = h.call(t, http.MethodGet, "/repo", nil, "developer")
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.StatusCode)
	}
	h.decode(t, response, &refusal)
	if refusal["needs"] != "repository" {
		t.Errorf("body = %v, want a repository named as missing", refusal)
	}
}

func TestCreateScaffoldsAndTheStatusRouteReportsIt(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)

	created := h.create(t)
	if !created.Cloned || !created.Scaffold.Complete || !created.Dirty {
		t.Fatalf("status = %+v, want a scaffolded, uncommitted checkout", created)
	}
	if created.Head != "" {
		t.Error("the create route committed something")
	}

	response := h.call(t, http.MethodGet, "/repo?fetch=true", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var status repo.Status
	h.decode(t, response, &status)
	if !status.Fetched || status.Link.FullName != "jonah/pv-forecast" {
		t.Errorf("status = %+v", status)
	}
	if status.Workspace != "data/ode" || status.Link.Path != "jonah/pv-forecast" {
		t.Errorf("the answer does not say where the checkout is: %+v", status)
	}
}

func TestTheCommitAndPushRoutesAreTheOnlyThingThatPublishes(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)
	h.create(t)

	response := h.call(t, http.MethodPost, "/repo/commit",
		map[string]string{"message": "Scaffold the operator"}, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("commit = %d: %s", response.StatusCode, body)
	}
	var committed repo.CommitResult
	h.decode(t, response, &committed)
	if committed.SHA == "" {
		t.Fatalf("commit = %+v", committed)
	}

	// A second commit with nothing to commit is 409 with the fact, not a 500 with
	// git's wording.
	response = h.call(t, http.MethodPost, "/repo/commit",
		map[string]string{"message": "again"}, "developer")
	if response.StatusCode != http.StatusConflict {
		t.Errorf("second commit = %d, want 409", response.StatusCode)
	}

	response = h.call(t, http.MethodPost, "/repo/push", nil, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("push = %d: %s", response.StatusCode, body)
	}
	var pushed repo.PushResult
	h.decode(t, response, &pushed)
	if pushed.HeadSHA != committed.SHA {
		t.Errorf("push = %+v, want the commit just made", pushed)
	}
	if head := repotest.Git(t, h.remote, "rev-parse",
		"refs/heads/main"); pushed.HeadSHA != trimNewline(head) {
		t.Errorf("the remote is at %q, want %s", head, pushed.HeadSHA)
	}
}

func TestTheCodePaneCanReadAndWriteEveryFile(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)
	h.create(t)

	response := h.call(t, http.MethodGet, "/repo/files", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("files = %d", response.StatusCode)
	}
	var tree repo.FileTree
	h.decode(t, response, &tree)
	if tree.Root != "jonah/pv-forecast" || len(tree.Tree.Children) == 0 {
		t.Fatalf("tree = %+v", tree)
	}

	// The workflow file is the case D14 is about, and the one jupyter_server's
	// contents API would have refused.
	const workflow = "/repo/files/content?path=.github/workflows/build.yml"
	response = h.call(t, http.MethodGet, workflow, nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read = %d", response.StatusCode)
	}
	var file repo.File
	h.decode(t, response, &file)
	if file.Language != "yaml" || file.Text == "" {
		t.Errorf("file = %+v", file)
	}

	response = h.call(t, http.MethodPut, "/repo/files/content",
		map[string]string{
			"path":    ".github/workflows/build.yml",
			"content": "name: edited\n",
		}, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("write = %d: %s", response.StatusCode, body)
	}

	response = h.call(t, http.MethodGet, workflow, nil, "developer")
	h.decode(t, response, &file)
	if file.Text != "name: edited\n" {
		t.Errorf("the file was not written: %+v", file)
	}

	// A path that leaves the repository is 400, and the answer says why.
	response = h.call(t, http.MethodGet, "/repo/files/content?path=../../etc/passwd",
		nil, "developer")
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("escaping read = %d, want 400", response.StatusCode)
	}
}

func TestDiscardingNeedsAnExplicitConfirmation(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)
	h.create(t)

	response := h.call(t, http.MethodPost, "/repo/discard", map[string]bool{}, "developer")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfirmed discard = %d, want 400", response.StatusCode)
	}
	// The scaffold is untracked, so a confirmed discard removes it — which is
	// exactly why the route requires the flag.
	response = h.call(t, http.MethodPost, "/repo/discard",
		map[string]bool{"confirm": true}, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("discard = %d: %s", response.StatusCode, body)
	}
	var status repo.Status
	h.decode(t, response, &status)
	if status.Dirty {
		t.Errorf("status = %+v, want a clean tree", status.Changes)
	}
}

func TestTheSessionRouteReportsTheRepoFeatureAndItsScopes(t *testing.T) {
	h := newRepoHarness(t)

	response := h.call(t, http.MethodGet, "/session", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var session struct {
		Features map[string]bool `json:"features"`
		Repo     struct {
			Scopes []string `json:"scopes"`
		} `json:"repo"`
	}
	h.decode(t, response, &session)
	if !session.Features["repo"] {
		t.Error("the repo feature is not reported")
	}
	if len(session.Repo.Scopes) != 2 {
		t.Errorf("scopes = %v, want the two the consent screen will ask for", session.Repo.Scopes)
	}
}

// TestWriteRepoContractFixtures emits the M7 documents the frontend's contract
// check assigns to its declared types.
//
// Emitted from the real handlers for the reason the M3 to M6 emitters are: the
// field sets are the backend's own marshalling. Here rather less of it is a fake's
// than usual — the status, the tree and the file come from a real git working copy
// — and only the GitHub identity and the repository list are invented.
//
//	ODE_WRITE_CONTRACT=frontend/src/__contract__ go test ./pkg/api/ -run ContractFixtures
func TestWriteRepoContractFixtures(t *testing.T) {
	dir := os.Getenv("ODE_WRITE_CONTRACT")
	if dir == "" {
		t.Skip("set ODE_WRITE_CONTRACT to the fixture directory to regenerate")
	}
	h := newRepoHarness(t)
	h.connect(t)
	h.create(t)
	if response := h.call(t, http.MethodPost, "/repo/commit",
		map[string]string{"message": "Scaffold the operator"}, "developer"); response.StatusCode !=
		http.StatusOK {
		t.Fatalf("commit = %d", response.StatusCode)
	}

	cases := []struct {
		file   string
		method string
		path   string
		body   any
	}{
		{"repo_connection.json", http.MethodGet, "/repo/connection", nil},
		{"repo_repositories.json", http.MethodGet, "/repo/repositories", nil},
		{"repo_status.json", http.MethodGet, "/repo?fetch=true", nil},
		{"repo_tree.json", http.MethodGet, "/repo/files", nil},
		{"repo_file.json", http.MethodGet, "/repo/files/content?path=op.py", nil},
		{"repo_scaffold.json", http.MethodPost, "/repo/scaffold", nil},
		{"repo_push.json", http.MethodPost, "/repo/push", nil},
	}
	for _, tc := range cases {
		response := h.call(t, tc.method, tc.path, tc.body, "developer")
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("%s: %d: %s", tc.path, response.StatusCode, body)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		var parsed any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		encoded, err := json.MarshalIndent(parsed, "", "  ")
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if err := os.WriteFile(filepath.Join(dir, tc.file), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		t.Logf("wrote %s", tc.file)
	}

	// The commit result too, which needs a change to commit first.
	if response := h.call(t, http.MethodPut, "/repo/files/content",
		map[string]string{"path": "op.py", "content": "# edited\n"}, "developer"); response.StatusCode !=
		http.StatusOK {
		t.Fatalf("write = %d", response.StatusCode)
	}
	response := h.call(t, http.MethodPost, "/repo/commit",
		map[string]string{"message": "Adjust the operator"}, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("commit = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("commit: %v", err)
	}
	encoded, _ := json.MarshalIndent(parsed, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "repo_commit.json"),
		append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("repo_commit.json: %v", err)
	}
	t.Logf("wrote repo_commit.json")
}

func trimNewline(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}
