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

package repo_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
)

// The harness runs the real thing wherever it can.
//
// git is a real git, the remote is a real bare repository on disk, the working copy
// is a real directory, and the cell ODE sends into the pod is executed by a real
// python3 — so a clone, a commit and a push in these tests are the same operations
// the pod performs. Only GitHub's API and JupyterHub are doubles, because neither
// can be had locally.
//
// The consequence worth knowing: these tests skip rather than pass when git or
// python3 is missing. A green run on a machine without them proves nothing, which
// is why the skip is loud.

const testUserSub = "user-1"

type harness struct {
	t         *testing.T
	service   *repo.Service
	github    *repotest.GitHub
	store     *repo.MemoryStore
	workspace string // the directory standing in for the PVC workspace
	remote    string // the bare repository standing in for GitHub's copy
	bearer    string
	sealer    *repo.Sealer
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, nil)
}

// newHarnessWith builds the same harness with the pod wrapped, which is how a
// test stages a kernel that becomes busy partway through a sequence of git
// commands. wrap may be nil.
func newHarnessWith(t *testing.T, wrap func(repo.Workspace) repo.Workspace) *harness {
	t.Helper()
	repotest.RequireGit(t)
	// The scaffold ends by running `uv lock` in the pod. A real uv would resolve the
	// Operator Lib pin over the network, so these tests supply their own — the point
	// under test is that ODE runs the lock and reports what it did, not that uv
	// resolves. A test that wants the other half calls stubUV itself.
	repotest.StubUV(t, repotest.LockingUV)

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
	store := repo.NewMemoryStore()

	var workspaceForRepo repo.Workspace = kernelService
	if wrap != nil {
		workspaceForRepo = wrap(workspaceForRepo)
	}

	service, err := repo.New(repo.Deps{
		Workspace:  workspaceForRepo,
		Store:      store,
		Sealer:     sealer,
		HTTPClient: github.Client(),
		Options: repo.Options{
			ClientID:       "client",
			ClientSecret:   "secret",
			APIURL:         github.URL(),
			WebURL:         github.URL(),
			RedirectURI:    "http://localhost:5173/github/callback",
			CommandTimeout: 60 * time.Second,
			OperatorLib:    "SENERGY-Platform/analytics-operator-lib-python",
		},
	})
	if err != nil {
		t.Fatalf("repo.New: %v", err)
	}

	return &harness{
		t: t, service: service, github: github, store: store,
		workspace: workspace, remote: remote,
		bearer: unsignedToken("jonah"), sealer: sealer,
	}
}

// request is the developer, as every operation needs them.
func (h *harness) request() repo.Request {
	return repo.Request{
		Bearer:  h.bearer,
		UserSub: testUserSub,
		Author:  repo.Author{Name: "Jonah", Email: "jonah@example.org", Sub: testUserSub},
	}
}

// connect stores a GitHub credential the way the OAuth flow would.
func (h *harness) connect() repo.Identity {
	h.t.Helper()
	authorize, err := h.service.Authorize(testUserSub)
	if err != nil {
		h.t.Fatalf("Authorize: %v", err)
	}
	identity, err := h.service.Connect(context.Background(), testUserSub, "code-1", authorize.State)
	if err != nil {
		h.t.Fatalf("Connect: %v", err)
	}
	return identity
}

// path is a path inside the working copy, on the test's filesystem.
func (h *harness) path(parts ...string) string {
	return filepath.Join(append([]string{h.workspace}, parts...)...)
}

// unsignedToken is a platform token with the claims pkg/kernel reads. The gateway
// validates signatures, not ODE (§3.1), so an unsigned one is what a unit test
// legitimately carries.
func unsignedToken(username string) string {
	claims, _ := json.Marshal(map[string]any{
		"sub":                testUserSub,
		"preferred_username": username,
		"realm_access":       map[string][]string{"roles": {"developer"}},
	})
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	encode := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf("%s.%s.", encode(header), encode(claims))
}
