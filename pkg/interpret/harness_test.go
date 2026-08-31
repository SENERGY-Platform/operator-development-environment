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

package interpret_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments/experimentstest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/interpret"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// The harness runs the real thing on both sides of the milestone.
//
// The experiment half is pkg/experiments' own: a real git working copy in a
// temporary directory standing in for the PVC, a real `git archive` carried back
// through a real python3 in the kerneltest pod, and Ray and MLflow as doubles
// because neither can be had in a unit test. The chat half is a real
// chat.Engine over a real tools.Dispatcher and a real admin.Service, with only the
// model scripted — so the exposure tier, the §3.3 cap and the one-exchange-at-a-
// time rule are the production ones rather than a fake's.
//
// What that buys: "a completed run produces an interpretation and a concrete next
// proposal" is asserted end to end, from `git archive` to the message the developer
// reads, and the criteria in the summary come from a real evaluation.yaml at a real
// commit.

const (
	testUserSub  = "user-1"
	testUsername = "jonah"
)

type harness struct {
	t           *testing.T
	interpret   *interpret.Service
	experiments *experiments.Service
	chat        *chat.Engine
	provider    *scriptedProvider
	ray         *experimentstest.Ray
	mlflow      *experimentstest.MLflow
	repo        *repo.Service
	store       *experiments.MemoryStore
	decisions   *interpret.MemoryStore
	limits      *admin.Service
	workspace   string
	session     chat.Session
}

func newHarness(t *testing.T, replies ...string) *harness {
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
			ClientID: "client", ClientSecret: "secret",
			APIURL: github.URL(), WebURL: github.URL(),
			RedirectURI:    "http://localhost:5173/github/callback",
			CommandTimeout: 120 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("repo.New: %v", err)
	}

	ray := experimentstest.NewRay(t)
	mlflow := experimentstest.NewMLflow(t)
	experimentStore := experiments.NewMemoryStore()
	experimentService, err := experiments.New(experiments.Deps{
		Access:    allowAllPermissions{},
		Workspace: kernelService,
		Repo:      repoService,
		Store:     experimentStore,
		IDs:       newSequentialIDs("exp"),
		Options: experiments.Options{
			RayURL: ray.URL(), MLflowURL: mlflow.URL(),
			RayClientURL: "auto", DefaultEntrypoint: "uv run python train.py",
			PyExecutable:        "uv run",
			TimescaleWrapperURL: "https://platform.example.org/db/v3",
			CommandTimeout:      120 * time.Second, RequestTimeout: 30 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("experiments.New: %v", err)
	}

	// The chat half, with only the model scripted.
	adminService, err := admin.New(admin.NewMemoryStore(), llm.NewPricing("EUR",
		llm.ModelPrice{Model: "fake-model", InputPerMTok: 1000, OutputPerMTok: 1000}))
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	registry, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, adminService, newSequentialIDs("call"))
	if err != nil {
		t.Fatalf("tools.NewDispatcher: %v", err)
	}
	provider := newScriptedProvider("fake", replies...)
	providers, err := llm.NewRegistry(provider)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	chatStore := chat.NewMemoryStore()
	engine, err := chat.New(t.Context(), providers, dispatcher, chatStore, adminService,
		newSequentialIDs("chat"), chat.Options{})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	decisions := interpret.NewMemoryStore()
	service, err := interpret.New(interpret.Deps{
		Experiments: experimentService,
		Chat:        engine,
		Store:       decisions,
		IDs:         newSequentialIDs("decision"),
		Options: interpret.Options{
			RetryInterval: 10 * time.Millisecond,
			TurnTimeout:   30 * time.Second,
			MaxPending:    10,
		},
	})
	if err != nil {
		t.Fatalf("interpret.New: %v", err)
	}

	session, err := engine.CreateSession(t.Context(), testUserSub, chat.CreateRequest{
		Title: "PV forecast", Provider: "fake", Model: "fake-model",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	h := &harness{
		t: t, interpret: service, experiments: experimentService, chat: engine,
		provider: provider, ray: ray, mlflow: mlflow, repo: repoService,
		store: experimentStore, decisions: decisions, limits: adminService,
		workspace: workspace, session: session,
	}
	h.connect()
	return h
}

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

// ready is a scaffolded repository with one commit, which is what a launch needs.
func (h *harness) ready() {
	h.t.Helper()
	if _, err := h.repo.Create(context.Background(), repo.CreateRequest{
		Request: h.repoRequest(), Name: "pv-forecast", Scaffold: true,
	}); err != nil {
		h.t.Fatalf("create: %v", err)
	}
	h.commit("Scaffold the operator")
}

func (h *harness) commit(message string) string {
	h.t.Helper()
	committed, err := h.repo.Commit(context.Background(), repo.CommitRequest{
		Request: h.repoRequest(), Message: message,
	})
	if err != nil {
		h.t.Fatalf("commit: %v", err)
	}
	return committed.SHA
}

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

func (h *harness) repoRequest() repo.Request {
	return repo.Request{
		Bearer:  unsignedToken(),
		UserSub: testUserSub,
		Author:  repo.Author{Name: "Jonah", Email: "jonah@example.org", Sub: testUserSub},
	}
}

func (h *harness) request() experiments.Request {
	return experiments.Request{
		Bearer:    unsignedToken(),
		UserSub:   testUserSub,
		Username:  testUsername,
		SessionID: h.session.ID,
		Author:    h.repoRequest().Author,
	}
}

// launch submits an experiment from the chat session.
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

func (h *harness) launch() experiments.LaunchResult {
	h.t.Helper()
	result, err := h.experiments.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})
	if err != nil {
		h.t.Fatalf("launch: %v", err)
	}
	return result
}

// finish makes a run terminal in both Ray and MLflow, which is what "the job
// ended" means to ODE — reconcile takes both.
func (h *harness) finish(result experiments.LaunchResult, metrics map[string]float64) {
	h.t.Helper()
	h.mlflow.Finish(h.t, result.RunID, "FINISHED", metrics)
	h.ray.SetStatus(result.SubmissionID, experiments.StatusSucceeded)
}

// connectDeveloper registers a live credential, the way the WebSocket handler
// does, and returns the function that withdraws it.
func (h *harness) connectDeveloper() func() {
	return h.interpret.Connected(testUserSub, chat.StaticToken(unsignedToken()))
}

// messages is the conversation as the developer would read it.
func (h *harness) messages() []chat.StoredMessage {
	h.t.Helper()
	stored, err := h.chat.Messages(context.Background(), testUserSub, h.session.ID)
	if err != nil {
		h.t.Fatalf("messages: %v", err)
	}
	return stored
}

// injectedText is what the model was actually handed on the last turn.
//
// Read from the provider's own recorded request rather than from the store, so an
// assertion about "what the assistant was told" cannot be satisfied by something
// the route recomputed afterwards.
func (h *harness) injectedText(t *testing.T) string {
	t.Helper()
	request := h.provider.lastRequest(t)
	builder := &strings.Builder{}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

// deliver runs one pass of the delivery loop, which is what the ticker would do.
func (h *harness) deliver() {
	h.interpret.Deliver(context.Background())
}

// poll runs one poller tick against the real service and this sink.
func (h *harness) poll() {
	h.t.Helper()
	poller, err := experiments.NewPoller(h.experiments, h.interpret,
		experiments.PollerOptions{
			Interval: time.Hour, Window: time.Hour, Batch: 50, Timeout: 30 * time.Second,
		})
	if err != nil {
		h.t.Fatalf("NewPoller: %v", err)
	}
	poller.Tick(context.Background())
}

// --- fakes ---

// scriptedProvider answers with a fixed sequence of replies, so a whole turn runs
// with no network and no API key. Past the script it answers plainly rather than
// hanging: a test that loops more than expected should fail on an assertion.
type scriptedProvider struct {
	name string

	mux      sync.Mutex
	replies  []string
	turn     int
	requests []llm.Request
	// hold makes the next stream wait before it answers, so a test can keep an
	// exchange open while it asserts on what happens beside it.
	hold chan struct{}
}

func newScriptedProvider(name string, replies ...string) *scriptedProvider {
	return &scriptedProvider{name: name, replies: replies}
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		Tools: true, Streaming: true, System: true, Models: []string{"fake-model"},
	}
}

// block makes the next stream wait until release is closed.
func (p *scriptedProvider) block(release chan struct{}) {
	p.mux.Lock()
	defer p.mux.Unlock()
	p.hold = release
}

func (p *scriptedProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mux.Lock()
	p.requests = append(p.requests, req)
	hold := p.hold
	p.hold = nil
	reply := "(no further scripted replies)"
	if p.turn < len(p.replies) {
		reply = p.replies[p.turn]
	}
	p.turn++
	p.mux.Unlock()

	out := make(chan llm.Event, 2)
	go func() {
		defer close(out)
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return
			}
		}
		events := []llm.Event{
			llm.TextEvent(reply),
			llm.DoneEvent("end_turn", llm.Usage{
				InputTokens: 100, OutputTokens: 50,
				Provider: p.name, Model: "fake-model",
			}),
		}
		for _, event := range events {
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// lastRequest is what the model was actually given, which is how a test asserts
// on the injected message rather than on the reply to it.
func (p *scriptedProvider) lastRequest(t *testing.T) llm.Request {
	t.Helper()
	p.mux.Lock()
	defer p.mux.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("the provider received no request")
	}
	return p.requests[len(p.requests)-1]
}

func (p *scriptedProvider) turns() int {
	p.mux.Lock()
	defer p.mux.Unlock()
	return len(p.requests)
}

// sequentialIDs mints predictable ids, unique per harness and per process.
//
// The uniqueness is not cosmetic: an experiment's submission id becomes the job
// archive's path in the pod (`/tmp/ode-experiment-<id>.zip`), and in these tests
// the "pod" is the machine running them. Ids that restarted at 1 in every process
// would let two concurrent runs race on one file in the real /tmp and fail with
// what looks like a product fault.
type sequentialIDs struct {
	prefix string
	mux    sync.Mutex
	next   int
}

// harnessSequence numbers the harnesses inside one test binary.
var harnessSequence atomic.Int64

func newSequentialIDs(kind string) *sequentialIDs {
	return &sequentialIDs{
		prefix: fmt.Sprintf("%s-%d-%d", kind, os.Getpid(), harnessSequence.Add(1)),
	}
}

func (s *sequentialIDs) NewID() string {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.next++
	return s.prefix + "-" + strconv.Itoa(s.next)
}

// unsignedToken is the developer's platform token as ODE reads it: well formed
// rather than valid, because the gateway is what validates one (§3.1 step 2).
func unsignedToken() string {
	claims, _ := json.Marshal(map[string]any{
		"sub":                testUserSub,
		"preferred_username": testUsername,
		"realm_access":       map[string][]string{"roles": {"developer"}},
	})
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	encode := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf("%s.%s.", encode(header), encode(claims))
}

// allowAllPermissions authorizes every input topic. The refusal path has its own
// tests; here it stands in for a developer who may read what they named.
type allowAllPermissions struct{}

func (allowAllPermissions) UserHasExecuteAccess(string, []string, string) (bool, error) {
	return true, nil
}
