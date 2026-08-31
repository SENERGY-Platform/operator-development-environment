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

package experiments_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments/experimentstest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
)

// The harness runs the real thing wherever one exists, which here means git and
// python3.
//
// The job package in these tests is produced by a real `git archive` over a real
// working copy, carried back through the real Python helper of pkg/kernel in a
// pod modelled by kerneltest, and handed to a Ray double that stores the bytes.
// So "the package is uploaded once and reused" is an assertion about git's output
// being deterministic, not about a fixture — and "a dirty working copy is refused"
// is an assertion about `git status` rather than about a flag a fake set.
//
// The consequence, stated as pkg/repo states it: these tests skip rather than pass
// where git or python3 is missing. A green run without them proves nothing.

const (
	testUserSub  = "user-1"
	testUsername = "jonah"
)

type harness struct {
	t         *testing.T
	service   *experiments.Service
	ray       *experimentstest.Ray
	mlflow    *experimentstest.MLflow
	repo      *repo.Service
	store     *experiments.MemoryStore
	workspace string
	remote    string
}

// options lets one test change one thing about the service without every test
// restating the whole construction.
type options func(*experiments.Deps)

func newHarness(t *testing.T, apply ...options) *harness {
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
		ExecuteTimeout: 120 * time.Second,
		MaxOutputBytes: 64 << 20,
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
	repoService, err := repo.New(repo.Deps{
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
			CommandTimeout: 120 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("repo.New: %v", err)
	}

	ray := experimentstest.NewRay(t)
	mlflow := experimentstest.NewMLflow(t)
	store := experiments.NewMemoryStore()

	deps := experiments.Deps{
		Workspace: kernelService,
		Repo:      repoService,
		Store:     store,
		IDs:       newSequentialIDs(),
		Options: experiments.Options{
			RayURL:            ray.URL(),
			RayToken:          "ray-service-token",
			MLflowURL:         mlflow.URL(),
			DefaultEntrypoint: "uv run python train.py",
			PyExecutable:      "uv run",
			// The deployment config a run carries. RayClientURL is what Operator Lib
			// hands to ray.init() and is not RayURL, which is the dashboard's HTTP API.
			RayClientURL:      "auto",
			TsConn:            "postgresql://ode:secret@timescale.example.org/postgres",
			CommandTimeout:    120 * time.Second,
			RequestTimeout:    30 * time.Second,
			EmbedProbeTimeout: 2 * time.Second,
			Environment: map[string]string{
				"SENERGY_TIMESCALE_URL": "https://platform.example.org/db/v3",
			},
		},
	}
	for _, change := range apply {
		change(&deps)
	}

	service, err := experiments.New(deps)
	if err != nil {
		t.Fatalf("experiments.New: %v", err)
	}

	h := &harness{
		t: t, service: service, ray: ray, mlflow: mlflow, repo: repoService,
		store: store, workspace: workspace, remote: remote,
	}
	h.connect()
	return h
}

// connect completes the OAuth flow against the GitHub double.
func (h *harness) connect() {
	h.t.Helper()
	authorize, err := h.repo.Authorize(testUserSub)
	if err != nil {
		h.t.Fatalf("authorize: %v", err)
	}
	if _, err := h.repo.Connect(context.Background(), testUserSub, "code-1",
		authorize.State); err != nil {
		h.t.Fatalf("connect: %v", err)
	}
}

// createRepository creates and scaffolds a working copy. It leaves it
// *uncommitted*, which is the state a launch has to refuse.
func (h *harness) createRepository() repo.Status {
	h.t.Helper()
	status, err := h.repo.Create(context.Background(), repo.CreateRequest{
		Request:  h.repoRequest(),
		Name:     "pv-forecast",
		Scaffold: true,
	})
	if err != nil {
		h.t.Fatalf("create: %v", err)
	}
	return status
}

// commit makes the working copy clean, which is what a launch requires.
func (h *harness) commit(message string) repo.CommitResult {
	h.t.Helper()
	committed, err := h.repo.Commit(context.Background(), repo.CommitRequest{
		Request: h.repoRequest(),
		Message: message,
	})
	if err != nil {
		h.t.Fatalf("commit: %v", err)
	}
	return committed
}

// ready is the state most tests start from: a scaffolded repository with one
// commit, so HEAD names a real tree.
func (h *harness) ready() string {
	h.t.Helper()
	h.createRepository()
	return h.commit("Scaffold the operator").SHA
}

func (h *harness) repoRequest() repo.Request {
	return repo.Request{
		Bearer:  unsignedToken(testUsername),
		UserSub: testUserSub,
		Author:  repo.Author{Name: "Jonah", Email: "jonah@example.org", Sub: testUserSub},
	}
}

func (h *harness) request() experiments.Request {
	return experiments.Request{
		Bearer:    unsignedToken(testUsername),
		UserSub:   testUserSub,
		Username:  testUsername,
		SessionID: "sess-1",
		Author:    h.repoRequest().Author,
	}
}

// testInputTopics is the one input every launch in these tests trains from.
//
// A launch carries input topics or it is refused, because an experiment with none
// reads no history and fails inside train(). The tests that are about the refusal
// itself override this with nil.
func testInputTopics() []experiments.InputTopic {
	return []experiments.InputTopic{{
		Name:        "urn_infai_ses_service_9ba92218-37d8-4c80-ad3d-bb3eb5c8457d",
		FilterType:  "DeviceId",
		FilterValue: "urn:infai:ses:device:2ac5436e-5538-4eb3-a448-2d77de68e915",
		Mappings:    []experiments.TopicMapping{{Dest: "value", Source: "value.power.value"}},
	}}
}

func (h *harness) launch(extra ...func(*experiments.LaunchRequest)) experiments.LaunchResult {
	h.t.Helper()
	req := experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()}
	for _, change := range extra {
		change(&req)
	}
	result, err := h.service.Launch(context.Background(), req)
	if err != nil {
		h.t.Fatalf("launch: %v", err)
	}
	return result
}

// write puts a file in the working copy without going through git, which is how a
// test makes the tree dirty.
//
// The checkout's own path is asked for rather than assumed: pkg/repo derives it
// from the repository's owner and name, and a test that hardcoded one directory
// would silently write somewhere else the day that changes.
func (h *harness) write(path, content string) {
	h.t.Helper()
	status, err := h.repo.Status(context.Background(),
		repo.StatusRequest{Request: h.repoRequest()})
	if err != nil {
		h.t.Fatalf("status: %v", err)
	}
	full := filepath.Join(h.workspace, filepath.FromSlash(status.Link.Path), path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", path, err)
	}
}

// unsignedToken is the developer's platform token as the kernel reads it.
//
// Unsigned, because ODE does not validate signatures: the platform API gateway
// does that (§3.1 step 2) and this service parses the claims. So a test needs a
// well-formed token rather than a valid one, which is also the honest shape of
// what ODE trusts.
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

// sequentialIDs mints predictable ids, so a failure names the experiment it was
// about rather than a random string.
//
// Predictable *within* a harness and unique *between* harnesses, which is not
// fussiness. The submission id becomes the archive's path in the pod —
// `/tmp/ode-experiment-<id>.zip`, see packagePath — and in these tests the "pod"
// is the machine running them. Ids that restarted at 1 in every process meant two
// concurrent runs of this package raced on one file in the real /tmp: one run's
// `git archive` wrote it and the other run's reader unlinked it, and the loser
// failed with "reading the job package back from the pod failed" — which reads as
// a product fault and is a collision between two tests. The prefix carries both
// the process and the harness, so neither can happen.
type sequentialIDs struct {
	prefix string
	mux    sync.Mutex
	next   int
}

// harnessSequence numbers the harnesses inside one test binary.
var harnessSequence atomic.Int64

func newSequentialIDs() *sequentialIDs {
	return &sequentialIDs{
		prefix: fmt.Sprintf("id-%d-%d-", os.Getpid(), harnessSequence.Add(1)),
	}
}

func (s *sequentialIDs) NewID() string {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.next++
	return s.prefix + strconv.Itoa(s.next)
}

func mustFind(t *testing.T, values map[string]string, key string) string {
	t.Helper()
	value, found := values[key]
	if !found {
		t.Fatalf("%q is missing; the map holds %v", key, keysOf(values))
	}
	return value
}

func keysOf(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// removeFile deletes a file from the working copy, which is how a test makes a
// repository that has no evaluation.yaml.
func (h *harness) removeFile(path string) {
	h.t.Helper()
	status, err := h.repo.Status(context.Background(),
		repo.StatusRequest{Request: h.repoRequest()})
	if err != nil {
		h.t.Fatalf("status: %v", err)
	}
	full := filepath.Join(h.workspace, filepath.FromSlash(status.Link.Path), path)
	if err := os.Remove(full); err != nil {
		h.t.Fatalf("remove %s: %v", path, err)
	}
}
