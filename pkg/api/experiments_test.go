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
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments/experimentstest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/identifiers"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/interpret"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// The M8 routes, over the same doubles pkg/experiments uses: a real git working
// copy in a directory standing in for the PVC, a real python3 carrying the archive
// back, and Ray and MLflow faked. So these tests are about the HTTP surface — the
// status codes, the shapes, the role gate — on top of behaviour exercised for real
// one layer down.

type experimentHarness struct {
	server *httptest.Server
	ray    *experimentstest.Ray
	mlflow *experimentstest.MLflow
	repo   *repo.Service
	// The M9 half: a real chat engine over a scripted model, and the interpretation
	// service on top of both. Built here rather than in a harness of its own because
	// §5.13's routes hang off the experiment group and every one of them needs a run
	// that a real `git archive` produced.
	chat *chat.Engine
	// experiments is the service the router was built over, so a test can drive the
	// poller against the same store the routes read.
	experiments *experiments.Service
	interpret   *interpret.Service
	session     chat.Session
}

func newExperimentHarness(t *testing.T) *experimentHarness {
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
	experimentService, err := experiments.New(experiments.Deps{
		Workspace: kernelService,
		Repo:      repoService,
		Store:     experiments.NewMemoryStore(),
		IDs:       identifiers.New(),
		Options: experiments.Options{
			RayURL:            ray.URL(),
			MLflowURL:         mlflow.URL(),
			RayClientURL:      "auto",
			TsConn:            "postgresql://ode:secret@timescale.example.org/postgres",
			DefaultEntrypoint: "uv run python train.py",
			PyExecutable:      "uv run",
			CommandTimeout:    120 * time.Second,
			RequestTimeout:    30 * time.Second,
			EmbedProbeTimeout: 2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("experiments.New: %v", err)
	}

	// M9 (§5.13). A real engine and a real dispatcher; only the model is scripted.
	adminService, err := admin.New(admin.NewMemoryStore(), llm.NewPricing("EUR"))
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	toolRegistry, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(toolRegistry, adminService, identifiers.New())
	if err != nil {
		t.Fatalf("tools.NewDispatcher: %v", err)
	}
	providers, err := llm.NewRegistry(&interpretingProvider{})
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	engine, err := chat.New(t.Context(), providers, dispatcher, chat.NewMemoryStore(),
		adminService, identifiers.New(), chat.Options{})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	interpretService, err := interpret.New(interpret.Deps{
		Experiments: experimentService,
		Chat:        engine,
		Store:       interpret.NewMemoryStore(),
		IDs:         identifiers.New(),
		Options:     interpret.Options{TurnTimeout: 30 * time.Second},
	})
	if err != nil {
		t.Fatalf("interpret.New: %v", err)
	}
	session, err := engine.CreateSession(t.Context(), testSubject, chat.CreateRequest{
		Title: "PV forecast", Provider: "interpreting", Model: "fake-model",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer"},
		api.Deps{
			Kernel: kernelService, Repo: repoService, Experiments: experimentService,
			Chat: engine, Admin: adminService, Interpretations: interpretService,
		},
	)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &experimentHarness{
		server: server, ray: ray, mlflow: mlflow, repo: repoService,
		chat: engine, experiments: experimentService, interpret: interpretService,
		session: session,
	}
}

func (h *experimentHarness) call(
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

func (h *experimentHarness) decode(t *testing.T, response *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// prepare gets a developer to the state a launch requires: connected, a
// repository, and one commit.
func (h *experimentHarness) prepare(t *testing.T) {
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

	response = h.call(t, http.MethodPost, "/repo/repositories",
		map[string]any{"name": "pv-forecast"}, "developer")
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create = %d: %s", response.StatusCode, body)
	}
}

func (h *experimentHarness) commit(t *testing.T, message string) {
	t.Helper()
	response := h.call(t, http.MethodPost, "/repo/commit",
		map[string]string{"message": message}, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("commit = %d: %s", response.StatusCode, body)
	}
}

// testInputTopics is the one input every launch in these tests trains from. A
// launch carries input topics or it is refused, because a run with none reads no
// history and fails inside train().
func testInputTopics() []experiments.InputTopic {
	return []experiments.InputTopic{{
		Name:        "urn_infai_ses_service_9ba92218-37d8-4c80-ad3d-bb3eb5c8457d",
		FilterType:  "DeviceId",
		FilterValue: "urn:infai:ses:device:2ac5436e-5538-4eb3-a448-2d77de68e915",
		Mappings:    []experiments.TopicMapping{{Dest: "value", Source: "value.power.value"}},
	}}
}

// launch posts a launch body, supplying the input topics when the caller's body
// does not name any. A test about the launch route is not about the topics, and
// every one of them would otherwise repeat the same block.
func (h *experimentHarness) launch(t *testing.T, body any) experiments.LaunchResult {
	t.Helper()
	body = withInputTopics(t, body)
	response := h.call(t, http.MethodPost, "/experiments", body, "developer")
	if response.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("launch = %d: %s", response.StatusCode, raw)
	}
	var result experiments.LaunchResult
	h.decode(t, response, &result)
	return result
}

// mintTokenFor is mintToken for a subject other than the harness's own, which is
// the only way to ask "what does another developer see" without a second harness.
func mintTokenFor(sub string, roles []string) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "gateway"}
	claims := map[string]any{
		"sub":                sub,
		"preferred_username": sub,
		"realm_access":       map[string]any{"roles": roles},
	}
	return segment(header) + "." + segment(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("signature-checked-at-the-gateway"))
}

func TestTheExperimentRoutesAreBehindTheDeveloperRole(t *testing.T) {
	h := newExperimentHarness(t)

	if response := h.call(t, http.MethodGet, "/experiments", nil); response.StatusCode !=
		http.StatusUnauthorized {
		t.Errorf("without a token = %d, want 401", response.StatusCode)
	}
	if response := h.call(t, http.MethodGet, "/experiments", nil, "analyst"); response.StatusCode !=
		http.StatusForbidden {
		t.Errorf("without the developer role = %d, want 403", response.StatusCode)
	}
}

func TestTheExperimentRoutesAreNotServedWithoutARayCluster(t *testing.T) {
	// The M0 harness has no experiment service, which is a deployment with no
	// ray_url. The routes must be absent rather than panicking on nil.
	h := newHarness(t)

	for _, path := range []string{"/experiments", "/experiments/embed"} {
		if response := h.get(t, path, "developer"); response.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 when no Ray cluster is configured", path, response.Code)
		}
	}
}

// The reproducibility guard of §5.11 item 7, at the HTTP boundary: 409 with what
// the developer has to do, not 500 and not a silent launch.
func TestLaunchingFromAnUncommittedWorkingCopyIs409WithThePaths(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)

	response := h.call(t, http.MethodPost, "/experiments", nil, "developer")
	if response.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("launch = %d: %s, want 409", response.StatusCode, body)
	}
	var refusal map[string]any
	h.decode(t, response, &refusal)
	if refusal["needs"] != "commit" {
		t.Errorf("body = %v, want the next step named so the pane can offer it", refusal)
	}
	if refusal["unborn"] != true {
		t.Errorf("body = %v, want the no-commit-at-all case distinguished", refusal)
	}
	if len(h.ray.Jobs()) != 0 {
		t.Error("a job was submitted from an uncommitted working copy")
	}
}

func TestALaunchRecordsTheCommitAndIsReadableBack(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")

	launched := h.launch(t, map[string]any{
		"entrypoint": "python training.py --folds 5",
		"env_vars":   map[string]string{"TRAINING_WINDOW_DAYS": "90"},
	})
	if launched.CommitSHA == "" || launched.RunID == "" {
		t.Fatalf("launch = %+v, want a commit and an MLflow run", launched)
	}
	// The credential note has to reach the SPA, because it decides what a long run
	// survives (§3.1 item 6).
	if !launched.Credential.ExpiresWithSession || launched.Credential.Note == "" {
		t.Errorf("credential = %+v, want the session limitation stated", launched.Credential)
	}

	response := h.call(t, http.MethodGet, "/experiments/"+launched.ID, nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", response.StatusCode)
	}
	var record experiments.Experiment
	h.decode(t, response, &record)
	if record.ID != launched.ID || record.Entrypoint != "python training.py --folds 5" {
		t.Errorf("record = %+v", record)
	}

	response = h.call(t, http.MethodGet, "/experiments", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", response.StatusCode)
	}
	var listing struct {
		Experiments []experiments.Experiment `json:"experiments"`
		Count       int                      `json:"count"`
		RayURL      string                   `json:"ray_url"`
		MLflowURL   string                   `json:"mlflow_url"`
	}
	h.decode(t, response, &listing)
	if listing.Count != 1 || listing.RayURL == "" || listing.MLflowURL == "" {
		t.Errorf("listing = %+v, want one experiment and the two links a pane needs", listing)
	}
}

// No route takes a user parameter, and the store's read is keyed by subject — so
// another developer's experiment is 404 rather than 403, and nothing reveals that
// it exists.
func TestAnotherDevelopersExperimentIs404(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.launch(t, nil)

	request, err := http.NewRequest(http.MethodGet,
		h.server.URL+"/experiments/"+launched.ID, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+mintTokenFor("someone-else", []string{"developer"}))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Errorf("another developer's experiment = %d, want 404", response.StatusCode)
	}
}

func TestTheResultsRouteAnswersTheStructuredSummaryAndTheLogsRouteTheLogs(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.launch(t, nil)

	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)
	h.ray.SetLogs(launched.SubmissionID, "Traceback (most recent call last):")

	response := h.call(t, http.MethodGet, "/experiments/"+launched.ID+"/results", nil, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("results = %d: %s", response.StatusCode, body)
	}
	raw, _ := io.ReadAll(response.Body)
	if bytes.Contains(raw, []byte("Traceback")) {
		t.Errorf("the summary carries log output: %s", raw)
	}
	var summary experiments.Summary
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if summary.Metrics["rmse"] != 0.31 || !summary.Finished {
		t.Errorf("summary = %+v", summary)
	}

	// The developer's own route has them, which is the point of the split.
	response = h.call(t, http.MethodGet, "/experiments/"+launched.ID+"/logs", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("logs = %d", response.StatusCode)
	}
	var page experiments.LogPage
	h.decode(t, response, &page)
	if page.Logs == "" {
		t.Error("the logs route returned nothing")
	}
}

func TestStoppingIsARouteOfItsOwn(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.launch(t, nil)

	response := h.call(t, http.MethodPost, "/experiments/"+launched.ID+"/stop", nil, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("stop = %d: %s", response.StatusCode, body)
	}
	var stopped experiments.Experiment
	h.decode(t, response, &stopped)
	if stopped.Status != experiments.StatusStopped {
		t.Errorf("status = %q, want it read back from Ray", stopped.Status)
	}
}

// D6's backend half. The static /embed segment has to win over the :id wildcard,
// which is the reason it is registered first.
func TestTheEmbedRouteAnswersPerServiceAndDoesNotCollideWithTheIdWildcard(t *testing.T) {
	h := newExperimentHarness(t)

	response := h.call(t, http.MethodGet, "/experiments/embed", nil, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("embed = %d: %s", response.StatusCode, body)
	}
	var report experiments.EmbedReport
	h.decode(t, response, &report)
	if len(report.Services) != 2 {
		t.Fatalf("services = %+v, want one probe per configured service", report.Services)
	}
	for _, probe := range report.Services {
		switch probe.Embeddable {
		case experiments.EmbedYes, experiments.EmbedNo, experiments.EmbedUnknown:
		default:
			t.Errorf("%s embeddable = %q, want one of the three D6 values",
				probe.Service, probe.Embeddable)
		}
		if probe.Reason == "" {
			t.Errorf("%s has a verdict with no reason", probe.Service)
		}
	}
	if report.Cached {
		t.Error("the first probe was served from a cache")
	}
	// And the second one is, which is D6's "cache".
	response = h.call(t, http.MethodGet, "/experiments/embed", nil, "developer")
	h.decode(t, response, &report)
	if !report.Cached {
		t.Error("the second probe was not cached")
	}
}

func TestTheSessionRouteReportsTheExperimentFeatureAndItsLinks(t *testing.T) {
	h := newExperimentHarness(t)

	response := h.call(t, http.MethodGet, "/session", nil, "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var session struct {
		Features    map[string]bool `json:"features"`
		Experiments struct {
			RayURL         string `json:"ray_url"`
			MLflowURL      string `json:"mlflow_url"`
			ScopedJobToken bool   `json:"scoped_job_token"`
		} `json:"experiments"`
	}
	h.decode(t, response, &session)
	if !session.Features["experiments"] {
		t.Error("the experiments feature is not reported")
	}
	if session.Experiments.RayURL == "" || session.Experiments.MLflowURL == "" {
		t.Errorf("experiments = %+v, want the two links a pane opens", session.Experiments)
	}
	if session.Experiments.ScopedJobToken {
		t.Error("no token exchange is configured, so the session must not claim one")
	}
}

// TestWriteExperimentContractFixtures emits the M8 documents the frontend's
// contract check assigns to its declared types.
//
// Emitted from the real handlers for the reason the M3 to M7 emitters are: the
// field sets are the backend's own marshalling. The split here is the same one M7
// has — the launch, the record and the listing come from a real git working copy
// and a real archive, so the commit SHA and the package size in them are git's own
// answers; the run's metrics and the Ray statuses are a double's, because neither
// a cluster nor a tracking server can be had in a test.
//
//	ODE_WRITE_CONTRACT=$PWD/frontend/src/__contract__ go test ./pkg/api/ -run ContractFixtures
func TestWriteExperimentContractFixtures(t *testing.T) {
	dir := os.Getenv("ODE_WRITE_CONTRACT")
	if dir == "" {
		t.Skip("set ODE_WRITE_CONTRACT to the fixture directory to regenerate")
	}
	h := newExperimentHarness(t)
	h.prepare(t)
	// Real criteria, so the fixture carries both arms of §5.13's `met`: the primary
	// metric is one these runs log and is a bare boolean, and the secondary one is a
	// metric they do not log, which is the explicit non-result D24 requires. A
	// fixture with only one of the two would let the frontend's type drift on the
	// other.
	h.writeFile(t, "evaluation.yaml", `# The evaluation criteria for this operator.
metric: rmse
goal: minimise
threshold: 0.35

secondary_metrics:
  - name: mape
    threshold: 5.0
    goal: minimise
`)
	h.commit(t, "Scaffold the operator")

	// Two runs, because the interesting half of §5.13's summary is the comparison
	// against the previous one — a fixture with a single run could not carry it.
	// Launched from the chat session, because §5.13's interpretation is injected
	// into the conversation a run came from and a fixture of one launched outside a
	// conversation could not carry it.
	first := h.launch(t, map[string]any{"run_name": "baseline", "session_id": h.session.ID})
	h.mlflow.SetParam(t, first.RunID, "folds", "5")
	h.mlflow.SetParam(t, first.RunID, "lookback_days", "90")
	h.mlflow.Finish(t, first.RunID, "FINISHED", map[string]float64{"rmse": 0.42, "r2": 0.71})
	h.ray.SetStatus(first.SubmissionID, experiments.StatusSucceeded)
	if response := h.call(t, http.MethodGet, "/experiments/"+first.ID, nil,
		"developer"); response.StatusCode != http.StatusOK {
		t.Fatalf("refresh the first run = %d", response.StatusCode)
	}

	// A second commit, so the second run is from a different code state — which is
	// what §5.11 item 7 is about and what makes the fixture's two commit SHAs differ.
	h.writeFile(t, "op.py", "# a wider lookback\n")
	h.commit(t, "Widen the lookback")
	second := h.launch(t, map[string]any{
		"run_name": "wider lookback", "session_id": h.session.ID})
	h.mlflow.SetParam(t, second.RunID, "folds", "5")
	h.mlflow.SetParam(t, second.RunID, "lookback_days", "180")
	h.mlflow.Finish(t, second.RunID, "FINISHED", map[string]float64{"rmse": 0.31, "r2": 0.78})
	h.ray.SetStatus(second.SubmissionID, experiments.StatusSucceeded)
	h.ray.SetLogs(second.SubmissionID,
		"2026-08-24 09:14:02 INFO fold 5/5 rmse=0.31\n2026-08-24 09:14:02 INFO done\n")

	cases := []struct {
		file   string
		method string
		path   string
	}{
		{"experiments.json", http.MethodGet, "/experiments"},
		{"experiment.json", http.MethodGet, "/experiments/" + second.ID},
		{"experiment_results.json", http.MethodGet, "/experiments/" + second.ID + "/results"},
		{"experiment_logs.json", http.MethodGet, "/experiments/" + second.ID + "/logs"},
		{"experiment_embed.json", http.MethodGet, "/experiments/embed"},
	}
	for _, tc := range cases {
		response := h.call(t, tc.method, tc.path, nil, "developer")
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("%s: %d: %s", tc.path, response.StatusCode, body)
		}
		writeFixture(t, dir, tc.file, response)
	}

	// M9 (§5.13): the interpretation of the second run and a decision on what it
	// proposed. Driven through the real poller and the real chat engine, so the
	// injected summary in the conversation and the proposal in this fixture are the
	// backend's own — only the model's wording is scripted.
	disconnect := h.interpret.Connected(testSubject,
		chat.StaticToken("Bearer "+mintToken([]string{"developer"})))
	defer disconnect()
	poller, err := experiments.NewPoller(h.experiments, h.interpret, experiments.PollerOptions{
		Interval: time.Hour, Window: time.Hour, Batch: 50, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	poller.Tick(t.Context())
	h.interpret.Deliver(t.Context())

	interpretationPath := "/experiments/" + second.ID + "/interpretation"
	response := h.call(t, http.MethodGet, interpretationPath, nil, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("%s: %d: %s", interpretationPath, response.StatusCode, body)
	}
	var current interpret.Interpretation
	h.decode(t, response, &current)
	writeFixtureValue(t, dir, "experiment_interpretation.json", current)

	decision := h.call(t, http.MethodPost, interpretationPath+"/decision", map[string]any{
		"proposal_id": current.Proposal.ID,
		"decision":    interpret.DecisionEdited,
		"edited":      "raise lookback_days to 270 rather than 365; the series does not go back further",
		"note":        "the window is bounded by what the device has recorded",
	}, "developer")
	if decision.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(decision.Body)
		t.Fatalf("decision: %d: %s", decision.StatusCode, body)
	}
	writeFixture(t, dir, "experiment_interpretation_decided.json", decision)

	// The launch result too, which needs a third commit to have something new to
	// submit — a launch from a commit already submitted would be a legitimate second
	// run, but a fixture that reused the package would not show the field that says
	// whether it did.
	h.writeFile(t, "training.py", "# a third state\n")
	h.commit(t, "Adjust the training task")
	launch := h.call(t, http.MethodPost, "/experiments",
		map[string]any{"run_name": "third", "env_vars": map[string]string{"FOLDS": "10"}},
		"developer")
	if launch.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(launch.Body)
		t.Fatalf("launch = %d: %s", launch.StatusCode, body)
	}
	writeFixture(t, dir, "experiment_launch.json", launch)
}

// writeFile puts a file into the working copy through the repo routes, so the
// fixture's own history is made the way a developer's would be.
func (h *experimentHarness) writeFile(t *testing.T, path, content string) {
	t.Helper()
	response := h.call(t, http.MethodPut, "/repo/files/content",
		map[string]string{"path": path, "content": content}, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("write %s = %d: %s", path, response.StatusCode, body)
	}
}

func writeFixture(t *testing.T, dir, file string, response *http.Response) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	encoded, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	t.Logf("wrote %s", file)
}

// writeFixtureValue writes a value that has already been decoded, for the one
// fixture the emitter reads before it writes: the decision needs the proposal id
// out of the interpretation, so the interpretation's body is consumed on the way.
func writeFixtureValue(t *testing.T, dir, file string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	t.Logf("wrote %s", file)
}

// A launch body sent with chunked transfer encoding must not be silently
// discarded.
//
// M8 guarded the bind with `if c.Request.ContentLength > 0`, and a chunked request
// has a ContentLength of -1. Every field of the body — the entrypoint, the
// environment, the run name, the session the run belongs to — was dropped without a
// word, and the job ran the deployment's default entrypoint instead. Silent, and
// wrong in the direction that costs cluster time: the developer sees a 201 with an
// experiment id and finds out from the Ray dashboard.
//
// Chunked is not exotic here. Any client that streams a body sends it — an SPA
// posting a ReadableStream, curl with `-H "Transfer-Encoding: chunked"`, and a
// proxy that re-frames a request on the way through.
func TestALaunchBodySentChunkedIsNotDiscarded(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")

	encoded, err := json.Marshal(map[string]any{
		"entrypoint":   "python training.py --folds 10",
		"run_name":     "chunked",
		"session_id":   "sess-chunked",
		"input_topics": testInputTopics(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// A reader whose length Go cannot know, which is what makes the request chunked.
	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/experiments",
		struct{ io.Reader }{bytes.NewReader(encoded)})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+mintToken([]string{"developer"}))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("launch = %d: %s", response.StatusCode, raw)
	}
	var result experiments.LaunchResult
	h.decode(t, response, &result)

	if result.Entrypoint != "python training.py --folds 10" {
		t.Errorf("entrypoint = %q, want the one in the body; a chunked request's body "+
			"was dropped and the deployment default ran instead", result.Entrypoint)
	}
	if result.SessionID != "sess-chunked" {
		t.Errorf("session_id = %q, want the one in the body: §5.13's summary is "+
			"injected into the session a launch came from, and a dropped one loses that",
			result.SessionID)
	}
}

// A launch that names only its inputs still runs the deployment default, which is
// the common case: "train what is committed, on these topics".
//
// It used to be a launch with no body at all. Input topics ended that — a run with
// none reads no history and fails inside train() — so the body is no longer
// optional, and the part worth keeping is that everything *else* in it still is.
func TestALaunchWithOnlyItsInputsRunsTheDefaultEntrypoint(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")

	result := h.launch(t, nil)
	if result.Entrypoint != "uv run python train.py" {
		t.Errorf("entrypoint = %q, want the deployment default", result.Entrypoint)
	}
}

// The refusal itself: no inputs, no run.
func TestALaunchWithNoInputTopicsIsRefused(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")

	response := h.call(t, http.MethodPost, "/experiments",
		map[string]any{"input_topics": []any{}}, "developer")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("launch = %d, want 400: a run with no inputs reads no history",
			response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), "propose_data_selection") {
		t.Errorf("error = %s, want it to name the tool that fixes it", raw)
	}
}

// A body that is not JSON is still a 400 rather than a silent default.
func TestALaunchWithAnUnreadableBodyIsRefused(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")

	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/experiments",
		bytes.NewReader([]byte("{not json")))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+mintToken([]string{"developer"}))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a body that could not be read",
			response.StatusCode)
	}
}

// withInputTopics re-encodes a launch body with input_topics added when it has
// none. It goes through JSON rather than through a struct because the callers
// pass maps, structs and nil interchangeably — and one of them deliberately
// passes a raw string to test a chunked body.
func withInputTopics(t *testing.T, body any) any {
	t.Helper()
	if body == nil {
		return map[string]any{"input_topics": testInputTopics()}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return body // not JSON-shaped: a test about malformed input, leave it alone
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return body
	}
	if _, named := fields["input_topics"]; !named {
		fields["input_topics"] = testInputTopics()
	}
	return fields
}
