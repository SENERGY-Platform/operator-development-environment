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

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// --- fakes ---

// scriptedProvider answers with a fixed script of turns, so a whole tool loop runs
// with no network and no API key. It also records the requests it received, which
// is what lets the tier tests assert on what the model was *offered* rather than
// only on what it was refused.
type scriptedProvider struct {
	name         string
	capabilities llm.Capabilities

	mux      sync.Mutex
	turns    [][]llm.Event
	turn     int
	requests []llm.Request
}

func newScriptedProvider(name string, turns ...[]llm.Event) *scriptedProvider {
	return &scriptedProvider{
		name:         name,
		capabilities: llm.Capabilities{Tools: true, Streaming: true, System: true, Models: []string{"fake-model"}},
		turns:        turns,
	}
}

func (p *scriptedProvider) Name() string                   { return p.name }
func (p *scriptedProvider) Capabilities() llm.Capabilities { return p.capabilities }

func (p *scriptedProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mux.Lock()
	p.requests = append(p.requests, req)
	var events []llm.Event
	if p.turn < len(p.turns) {
		events = p.turns[p.turn]
	} else {
		// Past the script, answer plainly rather than hanging: a test that loops more
		// than expected should fail on an assertion, not on a deadlock.
		events = []llm.Event{
			llm.TextEvent("(no further scripted turns)"),
			llm.DoneEvent("end_turn", llm.Usage{InputTokens: 1, OutputTokens: 1, Provider: p.name, Model: "fake-model"}),
		}
	}
	p.turn++
	p.mux.Unlock()

	out := make(chan llm.Event, len(events))
	go func() {
		defer close(out)
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

func (p *scriptedProvider) lastRequest(t *testing.T) llm.Request {
	t.Helper()
	p.mux.Lock()
	defer p.mux.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("the provider received no request")
	}
	return p.requests[len(p.requests)-1]
}

func (p *scriptedProvider) callCount() int {
	p.mux.Lock()
	defer p.mux.Unlock()
	return len(p.requests)
}

type fixedIDs struct {
	mux sync.Mutex
	n   int
}

func (f *fixedIDs) NewID() string {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.n++
	return fmt.Sprintf("id-%d", f.n)
}

// ranTools records which executors were actually reached, which is the property
// the tier gate is about — not what was returned, but what ran.
type ranTools struct {
	mux    sync.Mutex
	called []string
	// presented is the credential each call carried. It is what shows a token to be
	// read per call rather than captured for the whole turn.
	presented []string
}

func (r *ranTools) executor(name string) tools.Executor {
	return func(_ context.Context, req tools.Request) (any, error) {
		r.mux.Lock()
		defer r.mux.Unlock()
		r.called = append(r.called, name)
		r.presented = append(r.presented, req.Token)
		return map[string]any{"ran": name}, nil
	}
}

func (r *ranTools) tokens() []string {
	r.mux.Lock()
	defer r.mux.Unlock()
	return append([]string{}, r.presented...)
}

func (r *ranTools) was(name string) bool {
	r.mux.Lock()
	defer r.mux.Unlock()
	for _, called := range r.called {
		if called == name {
			return true
		}
	}
	return false
}

const (
	testUser  = "sub-alice"
	testToken = "Bearer test"
)

// harness is a complete engine over fakes.
type harness struct {
	engine   *Engine
	provider *scriptedProvider
	tracker  *ranTools
	admin    *admin.Service
	store    *hookedStore
	registry *tools.Registry
}

// testTools is one tool per tier plus one needing confirmation, built through the
// exported registry API so the executors are wired exactly as production wires
// them.
func testTools(tracker *ranTools) (*tools.Registry, error) {
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	declare := func(name string, tier tools.Tier, confirm bool) tools.Definition {
		return tools.NewDefinition(tools.Definition{
			Name: name, Description: "test tool " + name, Effect: "test",
			MinTier: tier, Confirm: confirm, Schema: schema,
		}, tracker.executor(name))
	}
	return tools.NewRegistry(
		declare("l0_tool", tools.L0, false),
		declare("l1_tool", tools.L1, false),
		declare("l2_tool", tools.L2, false),
		declare("confirmed_tool", tools.L0, true),
	)
}

// hookedStore wraps the memory store so a test can act between two iterations of
// the tool loop. The hook lives here rather than on MemoryStore: a production type
// should not carry a field that exists only for a test.
type hookedStore struct {
	*MemoryStore
	mux            sync.Mutex
	onMessagesRead func()
}

func newHookedStore() *hookedStore {
	return &hookedStore{MemoryStore: NewMemoryStore()}
}

func (s *hookedStore) setHook(hook func()) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.onMessagesRead = hook
}

func (s *hookedStore) Messages(ctx context.Context, sessionID string) ([]StoredMessage, error) {
	s.mux.Lock()
	hook := s.onMessagesRead
	s.mux.Unlock()
	if hook != nil {
		hook()
	}
	return s.MemoryStore.Messages(ctx, sessionID)
}

func newHarness(t *testing.T, turns ...[]llm.Event) *harness {
	t.Helper()

	tracker := &ranTools{}
	registry, err := testTools(tracker)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	adminStore := admin.NewMemoryStore()
	pricing := llm.NewPricing("EUR", llm.ModelPrice{
		Model: "fake-model", InputPerMTok: 1000, OutputPerMTok: 1000,
	})
	adminService, err := admin.New(adminStore, pricing)
	if err != nil {
		t.Fatalf("admin: %v", err)
	}

	dispatcher, err := tools.NewDispatcher(registry, adminService, &fixedIDs{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}

	provider := newScriptedProvider("fake", turns...)
	providers, err := llm.NewRegistry(provider)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}

	store := newHookedStore()
	engine, err := New(context.Background(), providers, dispatcher, store, adminService, &fixedIDs{}, Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	return &harness{
		engine: engine, provider: provider, tracker: tracker,
		admin: adminService, store: store, registry: registry,
	}
}

func (h *harness) session(t *testing.T, tier tools.Tier) Session {
	t.Helper()
	session, err := h.engine.CreateSession(context.Background(), testUser, CreateRequest{Tier: tier})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session
}

// drain subscribes to an exchange and collects it to completion.
//
// The subscription replays from the start, so a test that drains after the exchange
// has already finished still sees every event — which is the property that makes a
// detached exchange safe to observe late.
func drain(t *testing.T, exchange *Exchange) []Event {
	t.Helper()
	if exchange == nil {
		t.Fatal("no exchange was returned")
	}
	events, detach := exchange.Subscribe()
	defer detach()

	out := []Event{}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event, open := <-events:
			if !open {
				return out
			}
			out = append(out, event)
		case <-deadline:
			t.Fatalf("the exchange did not finish within 20s (%d events so far)", len(out))
			return out
		}
	}
}

func find(events []Event, kind EventType) []Event {
	out := []Event{}
	for _, event := range events {
		if event.Type == kind {
			out = append(out, event)
		}
	}
	return out
}

// toolTurn scripts a turn that calls one tool.
func toolTurn(id, name string) []llm.Event {
	return []llm.Event{
		llm.ToolCallEvent(llm.ToolCall{ID: id, Name: name, Input: json.RawMessage(`{}`)}),
		llm.DoneEvent("tool_use", llm.Usage{
			InputTokens: 10, OutputTokens: 5, Provider: "fake", Model: "fake-model",
		}),
	}
}

func textTurn(text string) []llm.Event {
	return []llm.Event{
		llm.TextEvent(text),
		llm.DoneEvent("end_turn", llm.Usage{
			InputTokens: 10, OutputTokens: 5, Provider: "fake", Model: "fake-model",
		}),
	}
}

// --- the tool loop ---

func TestToolLoopRunsAndConcludes(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "l0_tool"),
		textTurn("The tool said it ran."),
	)
	session := h.session(t, tools.L0)

	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	if !h.tracker.was("l0_tool") {
		t.Error("the L0 tool never ran")
	}
	if got := len(find(collected, EventToolCall)); got != 1 {
		t.Errorf("tool_call events = %d, want 1", got)
	}
	if got := len(find(collected, EventToolResult)); got != 1 {
		t.Errorf("tool_result events = %d, want 1", got)
	}
	done := find(collected, EventDone)
	if len(done) != 1 || done[0].StopReason != "end_turn" {
		t.Errorf("done = %+v, want one end_turn", done)
	}
	if h.provider.callCount() != 2 {
		t.Errorf("provider calls = %d, want 2 (the tool turn and the conclusion)", h.provider.callCount())
	}

	// The history must carry the tool call and its result structurally, or a
	// resumed session replays as prose and the model loses what it already read.
	messages, err := h.engine.Messages(context.Background(), testUser, session.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	sawToolUse, sawToolResult := false, false
	for _, message := range messages {
		for _, content := range message.Content {
			switch content.Type {
			case llm.ContentToolUse:
				sawToolUse = true
			case llm.ContentToolResult:
				sawToolResult = true
			}
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Errorf("stored history lost its tool blocks: tool_use=%v tool_result=%v",
			sawToolUse, sawToolResult)
	}
}

// TestMaxIterationsStopsARunawayLoop checks that a model which never concludes is
// stopped by control flow rather than left to run until the spend cap catches it.
func TestMaxIterationsStopsARunawayLoop(t *testing.T) {
	turns := make([][]llm.Event, 20)
	for i := range turns {
		turns[i] = toolTurn(fmt.Sprintf("call-%d", i), "l0_tool")
	}

	h := newHarness(t, turns...)
	h.engine.opts.MaxIterations = 3
	session := h.session(t, tools.L0)

	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "loop")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	if h.provider.callCount() != 3 {
		t.Errorf("provider calls = %d, want the 3 the cap allows", h.provider.callCount())
	}
	done := find(collected, EventDone)
	if len(done) != 1 || done[0].StopReason != "max_iterations" {
		t.Errorf("done = %+v, want a max_iterations stop", done)
	}
}

// --- the tier gate, end to end ---

// TestL0BlocksValueBearingToolsThroughTheEngine is the exit criterion at the level
// a developer experiences it: the assistant asks for an L2 tool, it does not run,
// and the refusal reaches the stream.
func TestL0BlocksValueBearingToolsThroughTheEngine(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "l2_tool"),
		textTurn("I was refused, so I will ask the developer."),
	)
	session := h.session(t, tools.L0)

	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "show me values")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	if h.tracker.was("l2_tool") {
		t.Fatal("an L2 tool ran in an L0 session")
	}

	results := find(collected, EventToolResult)
	if len(results) != 1 {
		t.Fatalf("tool_result events = %d, want 1", len(results))
	}
	if results[0].ToolResult.Outcome != tools.OutcomeBlockedByTier {
		t.Errorf("outcome = %q, want %q", results[0].ToolResult.Outcome, tools.OutcomeBlockedByTier)
	}

	encoded, _ := json.Marshal(results[0].ToolResult.Content)
	var refusal map[string]any
	_ = json.Unmarshal(encoded, &refusal)
	if refusal["blocked_by_tier"] != "L0" || refusal["required"] != "L2" {
		t.Errorf("refusal = %s, want §3.2's blocked_by_tier/required pair", encoded)
	}
}

// TestOnlyPermittedToolsAreOffered checks the other half: at L0 the model is not
// even shown the higher-tier tools, so it does not spend context discovering them.
func TestOnlyPermittedToolsAreOffered(t *testing.T) {
	for _, tc := range []struct {
		tier      tools.Tier
		wantTools []string
	}{
		{tools.L0, []string{"confirmed_tool", "l0_tool"}},
		{tools.L1, []string{"confirmed_tool", "l0_tool", "l1_tool"}},
		{tools.L2, []string{"confirmed_tool", "l0_tool", "l1_tool", "l2_tool"}},
	} {
		t.Run(tc.tier.String(), func(t *testing.T) {
			h := newHarness(t, textTurn("hello"))
			session := h.session(t, tc.tier)

			events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "hi")
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			drain(t, events)

			offered := []string{}
			for _, definition := range h.provider.lastRequest(t).Tools {
				offered = append(offered, definition.Name)
			}
			if strings.Join(offered, ",") != strings.Join(tc.wantTools, ",") {
				t.Errorf("offered %v, want %v", offered, tc.wantTools)
			}
		})
	}
}

// TestSystemPromptNamesTheTierAndWhatIsAbove is what makes §3.2's "ask the
// developer to raise it" possible: the model cannot ask for something it does not
// know exists.
func TestSystemPromptNamesTheTierAndWhatIsAbove(t *testing.T) {
	h := newHarness(t, textTurn("hello"))
	session := h.session(t, tools.L0)

	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "hi")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	prompt := h.provider.lastRequest(t).System
	if !strings.Contains(prompt, "L0") {
		t.Error("the system prompt does not state the session's tier")
	}
	for _, name := range []string{"l1_tool", "l2_tool"} {
		if !strings.Contains(prompt, name) {
			t.Errorf("the system prompt does not mention %q, so the model cannot ask for it", name)
		}
	}
	if !strings.Contains(prompt, "cannot change") && !strings.Contains(prompt, "developer controls") {
		t.Error("the system prompt should say the tier is the developer's to change")
	}
}

// TestLoweringTheTierMidExchangeTakesEffect checks that the tier is re-read per
// iteration rather than captured once at the start of the exchange.
func TestLoweringTheTierMidExchangeTakesEffect(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "l1_tool"),
		toolTurn("call-2", "l1_tool"),
		textTurn("done"),
	)
	session := h.session(t, tools.L1)

	// Lowered part-way through the exchange, the way a developer clicking the
	// control would. The second Messages read is the start of the second iteration.
	var reads int
	h.store.setHook(func() {
		reads++
		// The first read is iteration one, which dispatches at L1. Lowering here
		// means iteration two re-reads the session and finds L0.
		if reads == 1 {
			if _, err := h.engine.SetTier(context.Background(), testUser, session.ID, tools.L0); err != nil {
				t.Errorf("SetTier: %v", err)
			}
		}
	})

	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "profile it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	blocked := 0
	for _, event := range find(collected, EventToolResult) {
		if event.ToolResult.Outcome == tools.OutcomeBlockedByTier {
			blocked++
		}
	}
	if blocked == 0 {
		t.Error("lowering the tier mid-exchange did not block the next call")
	}
}

// --- confirmation (D11) ---

func TestConfirmationHoldsTheExchange(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "confirmed_tool"),
		textTurn("Thank you for confirming."),
	)
	session := h.session(t, tools.L0)

	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "do the thing")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	if h.tracker.was("confirmed_tool") {
		t.Fatal("a tool needing confirmation ran without one")
	}
	asked := find(collected, EventConfirmation)
	if len(asked) != 1 {
		t.Fatalf("confirmation events = %d, want 1", len(asked))
	}
	done := find(collected, EventDone)
	if len(done) != 1 || done[0].StopReason != "awaiting_confirmation" {
		t.Errorf("done = %+v, want awaiting_confirmation", done)
	}

	// The exchange must not have continued past the hold.
	if h.provider.callCount() != 1 {
		t.Errorf("provider calls = %d, want 1: the exchange should pause", h.provider.callCount())
	}

	pending, err := h.engine.PendingConfirmations(context.Background(), testUser, session.ID)
	if err != nil {
		t.Fatalf("PendingConfirmations: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending confirmations = %d, want 1", len(pending))
	}

	// Approving runs the tool and continues.
	resumed, err := h.engine.Confirm(context.Background(), StaticToken(testToken), testUser,
		session.ID, pending[0].ID, true)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	drain(t, resumed)

	if !h.tracker.was("confirmed_tool") {
		t.Error("the approved tool never ran")
	}
	if h.provider.callCount() != 2 {
		t.Errorf("provider calls = %d, want 2 after resuming", h.provider.callCount())
	}
}

func TestRejectingAConfirmationDoesNotRunTheTool(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "confirmed_tool"),
		textTurn("Understood."),
	)
	session := h.session(t, tools.L0)

	events, _ := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "do it")
	drain(t, events)

	pending, _ := h.engine.PendingConfirmations(context.Background(), testUser, session.ID)
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}

	resumed, err := h.engine.Confirm(context.Background(), StaticToken(testToken), testUser,
		session.ID, pending[0].ID, false)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	drain(t, resumed)

	if h.tracker.was("confirmed_tool") {
		t.Fatal("a declined tool ran anyway")
	}

	// And it must be resolved, so it cannot be replayed.
	if _, err := h.engine.Confirm(context.Background(), StaticToken(testToken), testUser,
		session.ID, pending[0].ID, true); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("re-confirming gave %v, want ErrAlreadyResolved", err)
	}
}

// TestConfirmedCallProducesOneToolResultOnly is the protocol constraint: a
// tool_use block must be answered exactly once. The held call is answered when the
// exchange pauses, so the decision has to arrive as a user turn, not a second
// tool_result.
func TestConfirmedCallProducesOneToolResultOnly(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "confirmed_tool"),
		textTurn("done"),
	)
	session := h.session(t, tools.L0)

	events, _ := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "do it")
	drain(t, events)
	pending, _ := h.engine.PendingConfirmations(context.Background(), testUser, session.ID)
	resumed, _ := h.engine.Confirm(context.Background(), StaticToken(testToken), testUser, session.ID, pending[0].ID, true)
	drain(t, resumed)

	messages, err := h.engine.Messages(context.Background(), testUser, session.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	perCallID := map[string]int{}
	for _, message := range messages {
		for _, content := range message.Content {
			if content.Type == llm.ContentToolResult {
				perCallID[content.ToolUseID]++
			}
		}
	}
	for callID, count := range perCallID {
		if count > 1 {
			t.Errorf("tool_use %q was answered %d times; both native protocols reject that",
				callID, count)
		}
	}
}

// --- provider swap (exit criterion) ---

// TestProviderSwapNeedsNoCallSiteChange runs the identical exchange against two
// unrelated providers, chosen only by the name stored on the session. Nothing in
// this test — and nothing in the engine — names a concrete provider.
func TestProviderSwapNeedsNoCallSiteChange(t *testing.T) {
	first := newScriptedProvider("provider-one",
		toolTurn("c1", "l0_tool"), textTurn("one is done"))
	second := newScriptedProvider("provider-two",
		toolTurn("c1", "l0_tool"), textTurn("two is done"))

	tracker := &ranTools{}
	registry, err := testTools(tracker)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	adminService, err := admin.New(admin.NewMemoryStore(), llm.NewPricing("EUR"))
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, adminService, &fixedIDs{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	providers, err := llm.NewRegistry(first, second)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	engine, err := New(context.Background(), providers, dispatcher, NewMemoryStore(), adminService, &fixedIDs{}, Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	for _, name := range []string{"provider-one", "provider-two"} {
		t.Run(name, func(t *testing.T) {
			session, err := engine.CreateSession(context.Background(), testUser, CreateRequest{
				Provider: name,
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if session.Provider != name {
				t.Fatalf("session provider = %q, want %q", session.Provider, name)
			}

			events, err := engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "go")
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			collected := drain(t, events)

			// The same observable behaviour from both: a tool ran and the turn ended.
			if got := len(find(collected, EventToolResult)); got != 1 {
				t.Errorf("tool_result events = %d, want 1", got)
			}
			done := find(collected, EventDone)
			if len(done) != 1 || done[0].StopReason != "end_turn" {
				t.Errorf("done = %+v, want one end_turn", done)
			}
		})
	}

	if first.callCount() == 0 || second.callCount() == 0 {
		t.Error("one of the two providers was never used")
	}

	// An unregistered name is refused rather than silently falling back to the
	// default, which would hide a typo in a deployment's configuration.
	if _, err := engine.CreateSession(context.Background(), testUser, CreateRequest{
		Provider: "provider-three",
	}); !errors.Is(err, llm.ErrNoSuchProvider) {
		t.Errorf("unknown provider gave %v, want ErrNoSuchProvider", err)
	}
}

// TestTextOnlyProviderOffersNoTools covers §5.7's degraded mode: a provider that
// cannot invoke tools is given none, and the prompt says so rather than leaving
// the model to call tools that never fire.
func TestTextOnlyProviderOffersNoTools(t *testing.T) {
	h := newHarness(t, textTurn("I have no tools."))
	h.provider.capabilities.Tools = false
	h.provider.capabilities.Degraded = true

	session := h.session(t, tools.L2)
	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "hi")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	request := h.provider.lastRequest(t)
	if len(request.Tools) != 0 {
		t.Errorf("a text-only provider was offered %d tools", len(request.Tools))
	}
	if !strings.Contains(request.System, "no tools") {
		t.Error("the system prompt should tell a text-only assistant it has no tools")
	}
}

// --- spend caps in the loop (§3.3) ---

func TestCapRefusesTheExchangeBeforeAnyProviderCall(t *testing.T) {
	h := newHarness(t, textTurn("should never run"))
	ctx := context.Background()

	if err := h.admin.SetLimits(ctx, testUser, admin.Limits{
		Period: "24h", TokenCap: int64Ptr(10),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	h.admin.RecordUsage(ctx, testUser, "other", llm.Usage{
		InputTokens: 100, Provider: "fake", Model: "fake-model",
	})

	session := h.session(t, tools.L0)
	_, err := h.engine.Send(ctx, StaticToken(testToken), testUser, session.ID, "hello")

	var limitErr *admin.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("Send gave %v, want a *admin.LimitError", err)
	}
	if h.provider.callCount() != 0 {
		t.Error("the provider was called despite the cap being exhausted")
	}
}

// TestCapStopsAToolLoopMidWay checks the cap is re-checked per provider call: a
// loop makes several, and one that only checked the first would be trivially
// exceeded.
func TestCapStopsAToolLoopMidWay(t *testing.T) {
	turns := make([][]llm.Event, 10)
	for i := range turns {
		turns[i] = toolTurn(fmt.Sprintf("c-%d", i), "l0_tool")
	}
	h := newHarness(t, turns...)
	ctx := context.Background()

	// A cap that the first turn's own usage will exceed. Each scripted turn reports
	// 15 tokens, and the engine records them when the exchange ends — but Check
	// reads recorded spend, so seed it just under the cap and let the loop's own
	// accounting take it over.
	if err := h.admin.SetLimits(ctx, testUser, admin.Limits{
		Period: "24h", TokenCap: int64Ptr(20),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	h.admin.RecordUsage(ctx, testUser, "prior", llm.Usage{
		InputTokens: 19, Provider: "fake", Model: "fake-model",
	})

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(ctx, StaticToken(testToken), testUser, session.ID, "loop")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	// The exchange started (19 < 20) and then hit the cap on a later iteration.
	if h.provider.callCount() == 0 {
		t.Fatal("the exchange never started")
	}
	if h.provider.callCount() >= 10 {
		t.Errorf("provider calls = %d: the cap did not stop the loop", h.provider.callCount())
	}
	if got := len(find(collected, EventLimit)); got != 1 {
		t.Errorf("limit_exceeded events = %d, want 1", got)
	}
}

func TestUsageIsRecordedForTheExchange(t *testing.T) {
	h := newHarness(t, toolTurn("c1", "l0_tool"), textTurn("done"))
	ctx := context.Background()

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(ctx, StaticToken(testToken), testUser, session.ID, "go")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	usage := find(collected, EventUsage)
	if len(usage) != 1 {
		t.Fatalf("usage events = %d, want 1 for the exchange", len(usage))
	}
	// Two provider calls at 15 tokens each.
	if got := usage[0].Usage.InputTokens + usage[0].Usage.OutputTokens; got != 30 {
		t.Errorf("tokens = %d, want 30 summed across both turns", got)
	}

	spend, err := h.admin.Spend(ctx, testUser, 0)
	if err != nil {
		t.Fatalf("Spend: %v", err)
	}
	if spend.Tokens != 30 {
		t.Errorf("recorded tokens = %d, want 30", spend.Tokens)
	}
}

// --- tier control and its audit trail (§3.2) ---

func TestTierChangesAreAudited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.session(t, tools.L0)

	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L1); err != nil {
		t.Fatalf("SetTier: %v", err)
	}
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L2); err != nil {
		t.Fatalf("SetTier: %v", err)
	}
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L0); err != nil {
		t.Fatalf("SetTier: %v", err)
	}

	changes, err := h.engine.TierChanges(ctx, testUser, session.ID)
	if err != nil {
		t.Fatalf("TierChanges: %v", err)
	}
	// The initial tier plus three changes: the trail starts at session creation, so
	// it is complete rather than beginning at the first change.
	if len(changes) != 4 {
		t.Fatalf("audit entries = %d, want 4 (creation plus three changes)", len(changes))
	}
	wantTo := []tools.Tier{tools.L0, tools.L1, tools.L2, tools.L0}
	for i, want := range wantTo {
		if changes[i].To != want {
			t.Errorf("entry %d to = %v, want %v", i, changes[i].To, want)
		}
		if changes[i].UserSub != testUser {
			t.Errorf("entry %d lost the user, which §3.2 requires", i)
		}
		if changes[i].At.IsZero() {
			t.Errorf("entry %d has no timestamp, which §3.2 requires", i)
		}
	}
}

func TestTierCannotExceedTheAdminCeiling(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.admin.SetLimits(ctx, admin.GlobalSubject, admin.Limits{
		MaxTier: tierPtr(tools.L1),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	session := h.session(t, tools.L0)
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L1); err != nil {
		t.Errorf("L1 was refused under an L1 ceiling: %v", err)
	}
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L2); err == nil {
		t.Error("L2 was permitted under an L1 ceiling")
	}

	// Lowering is always allowed: a developer may always expose less.
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L0); err != nil {
		t.Errorf("lowering the tier was refused: %v", err)
	}
}

func TestCreateSessionRefusesATierAboveTheCeiling(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.admin.SetLimits(ctx, admin.GlobalSubject, admin.Limits{
		MaxTier: tierPtr(tools.L0),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	if _, err := h.engine.CreateSession(ctx, testUser, CreateRequest{Tier: tools.L2}); err == nil {
		t.Error("a session was created above the admin tier ceiling")
	}
}

// --- ownership ---

// TestSessionsAreNotReadableByOtherUsers checks that a session id in a URL is not
// enough, and that the refusal does not confirm the id exists.
func TestSessionsAreNotReadableByOtherUsers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.session(t, tools.L0)

	if _, err := h.engine.Session(ctx, "sub-bob", session.ID); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("bob reading alice's session gave %v, want ErrNoSuchSession", err)
	}
	if _, err := h.engine.SetTier(ctx, "sub-bob", session.ID, tools.L2); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("bob changing alice's tier gave %v, want ErrNoSuchSession", err)
	}
	if _, err := h.engine.Messages(ctx, "sub-bob", session.ID); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("bob reading alice's messages gave %v, want ErrNoSuchSession", err)
	}
	if _, err := h.engine.TierFor(ctx, "sub-bob", session.ID); err == nil {
		t.Error("bob resolved a tier from alice's session over the MCP path")
	}
}

// --- the session cap ---

func TestConcurrentSessionCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.admin.SetLimits(ctx, testUser, admin.Limits{
		MaxConcurrentSessions: intPtr(1),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	if _, err := h.engine.CreateSession(ctx, testUser, CreateRequest{}); err != nil {
		t.Fatalf("the first session was refused: %v", err)
	}
	if _, err := h.engine.CreateSession(ctx, testUser, CreateRequest{}); err == nil {
		t.Error("a second session was created under a cap of 1")
	}
}

// --- the selection sink ---

func TestProposedSelectionLandsOnTheSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.session(t, tools.L0)

	proposal := tools.ProposedSelection{
		Rationale: "power is the target",
		Series: []tools.ProposedSeries{
			{DeviceID: "d1", ServiceID: "s1", VariablePath: "value.power", Role: "target"},
		},
	}
	if err := h.engine.PutProposedSelection(ctx, session.ID, proposal); err != nil {
		t.Fatalf("PutProposedSelection: %v", err)
	}

	updated, err := h.engine.Session(ctx, testUser, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if updated.Selection == nil || len(updated.Selection.Series) != 1 {
		t.Fatalf("selection = %+v, want the one proposed series", updated.Selection)
	}

	// And the assistant is told about it on the next turn, or the confirmation
	// would have no effect on its behaviour.
	h.provider.turns = [][]llm.Event{textTurn("noted")}
	h.provider.turn = 0
	events, err := h.engine.Send(ctx, StaticToken(testToken), testUser, session.ID, "what did I pick?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	if !strings.Contains(h.provider.lastRequest(t).System, "value.power") {
		t.Error("the confirmed selection is not in the system prompt")
	}
}

// --- guards ---

func TestEngineRequiresItsCollaborators(t *testing.T) {
	providers, _ := llm.NewRegistry(newScriptedProvider("p"))
	adminService, _ := admin.New(admin.NewMemoryStore(), llm.NewPricing("EUR"))
	registry, _ := testTools(&ranTools{})
	dispatcher, _ := tools.NewDispatcher(registry, adminService, &fixedIDs{})
	store := NewMemoryStore()
	ids := &fixedIDs{}

	empty, _ := llm.NewRegistry()
	if _, err := New(context.Background(), empty, dispatcher, store, adminService, ids, Options{}); err == nil {
		t.Error("an engine was built with no providers")
	}
	if _, err := New(context.Background(), providers, nil, store, adminService, ids, Options{}); err == nil {
		t.Error("an engine was built with no dispatcher")
	}
	// The admin service is required rather than optional: §3.3's caps are not an
	// opt-in extra, and an engine without one could not enforce them.
	if _, err := New(context.Background(), providers, dispatcher, store, nil, ids, Options{}); err == nil {
		t.Error("an engine was built with no admin service, so no cap could be enforced")
	}
}

func TestEmptyMessageIsRefused(t *testing.T) {
	h := newHarness(t)
	session := h.session(t, tools.L0)
	if _, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "   "); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("an empty message gave %v, want ErrInvalidRequest", err)
	}
}

func int64Ptr(v int64) *int64          { return &v }
func intPtr(v int) *int                { return &v }
func tierPtr(v tools.Tier) *tools.Tier { return &v }

// TestLoweringTheCeilingClampsAnOpenSession is the gap an end-to-end probe found:
// the ceiling was checked only when a developer *raised* the tier, so a session
// already at L2 kept its L2 tools after an admin lowered the maximum. A policy that
// only applies to future sessions is not a policy.
func TestLoweringTheCeilingClampsAnOpenSession(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "l2_tool"),
		textTurn("refused"),
	)
	ctx := context.Background()

	// The session is legitimately raised to L2 while no ceiling exists.
	session := h.session(t, tools.L0)
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L2); err != nil {
		t.Fatalf("SetTier: %v", err)
	}

	// The admin then lowers the maximum. Nothing touches the session.
	if err := h.admin.SetLimits(ctx, admin.GlobalSubject, admin.Limits{
		MaxTier: tierPtr(tools.L0),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	// The effective tier is now L0, even though L2 is what is stored.
	read, err := h.engine.Session(ctx, testUser, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if read.Tier != tools.L0 {
		t.Errorf("effective tier = %v, want L0 after the ceiling was lowered", read.Tier)
	}

	// And the L2 tool no longer runs.
	events, err := h.engine.Send(ctx, StaticToken(testToken), testUser, session.ID, "preview it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	if h.tracker.was("l2_tool") {
		t.Fatal("an L2 tool ran after the admin ceiling was lowered to L0")
	}
	blocked := false
	for _, event := range find(collected, EventToolResult) {
		if event.ToolResult.Outcome == tools.OutcomeBlockedByTier {
			blocked = true
		}
	}
	if !blocked {
		t.Error("the call was not refused by the tier gate")
	}

	// The MCP path must agree, or it would be a way around the clamp.
	tier, err := h.engine.TierFor(ctx, testUser, session.ID)
	if err != nil {
		t.Fatalf("TierFor: %v", err)
	}
	if tier != tools.L0 {
		t.Errorf("MCP tier = %v, want L0", tier)
	}
}

// TestLoweringAClampedSessionIsNotANoOp is the subtlety in SetTier: the comparison
// has to be against the stored tier, not the clamped one, or a developer could not
// bring a clamped session's stored value back down.
func TestLoweringAClampedSessionIsNotANoOp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.session(t, tools.L0)
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L2); err != nil {
		t.Fatalf("SetTier: %v", err)
	}
	if err := h.admin.SetLimits(ctx, admin.GlobalSubject, admin.Limits{
		MaxTier: tierPtr(tools.L0),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L0); err != nil {
		t.Fatalf("lowering a clamped session: %v", err)
	}

	// The audit records it as L2 → L0, from what was stored, rather than skipping it
	// as an L0 → L0 no-op.
	changes, err := h.engine.TierChanges(ctx, testUser, session.ID)
	if err != nil {
		t.Fatalf("TierChanges: %v", err)
	}
	last := changes[len(changes)-1]
	if last.From != tools.L2 || last.To != tools.L0 {
		t.Errorf("last audit entry = %v → %v, want L2 → L0", last.From, last.To)
	}
}

// --- the credential of a detached turn (TokenSource) ---

// An exchange is detached from the request that started it: twelve iterations of
// profiling take minutes, and the access token that started them expires inside
// that window. A token captured at Send would be stale for every tool call after
// the first, and the reads would fail with a 401 the model then has to explain.
func TestEachToolCallReadsTheCurrentToken(t *testing.T) {
	h := newHarness(t,
		toolTurn("c1", "l0_tool"),
		toolTurn("c2", "l0_tool"),
		textTurn("done"),
	)

	// A source that answers with a new token every time it is read. If the engine
	// captured one, both calls would show the same.
	reads := 0
	mux := sync.Mutex{}
	source := TokenSource(func() string {
		mux.Lock()
		defer mux.Unlock()
		reads++
		return fmt.Sprintf("Bearer token-%d", reads)
	})

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(context.Background(), source, testUser, session.ID, "go")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	presented := h.tracker.tokens()
	want := []string{"Bearer token-1", "Bearer token-2"}
	if len(presented) != len(want) {
		t.Fatalf("tool calls = %v, want two", presented)
	}
	for i := range want {
		if presented[i] != want[i] {
			t.Errorf("call %d presented %q, want %q", i+1, presented[i], want[i])
		}
	}
}

// The confirmation path is where the wait is longest: a developer approves when
// they get round to it, and a tool dispatched on their decision must use the
// credential of that moment rather than of the message that proposed it.
func TestAConfirmedToolUsesTheTokenOfTheDecision(t *testing.T) {
	h := newHarness(t,
		toolTurn("call-1", "confirmed_tool"),
		textTurn("Understood."),
	)
	session := h.session(t, tools.L0)

	events, err := h.engine.Send(context.Background(), StaticToken("Bearer at-send"), testUser,
		session.ID, "do it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	pending, err := h.engine.PendingConfirmations(context.Background(), testUser, session.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, err = %v; want one held call", pending, err)
	}

	resumed, err := h.engine.Confirm(context.Background(), StaticToken("Bearer at-decision"), testUser,
		session.ID, pending[0].ID, true)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	drain(t, resumed)

	presented := h.tracker.tokens()
	if len(presented) != 1 {
		t.Fatalf("tool calls = %v, want the one confirmed call", presented)
	}
	if presented[0] != "Bearer at-decision" {
		t.Errorf("the confirmed tool presented %q, want the token of the decision", presented[0])
	}
}

// --- an unanswered tool_use, and what a cancelled turn costs ---

// failingStore fails one store operation on demand, so the two paths that can
// leave an assistant tool_use unanswered can be reached without a real database.
type failingStore struct {
	*MemoryStore
	mux sync.Mutex
	// putConfirmation fails PutConfirmation while set.
	putConfirmation error
	// toolResults fails AppendMessages for a message carrying tool results while
	// set, which is the write that answers the assistant's tool_use.
	toolResults error
}

func newFailingStore() *failingStore {
	return &failingStore{MemoryStore: NewMemoryStore()}
}

func (s *failingStore) failPutConfirmation(err error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.putConfirmation = err
}

func (s *failingStore) failToolResults(err error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.toolResults = err
}

func (s *failingStore) PutConfirmation(ctx context.Context, confirmation Confirmation) error {
	s.mux.Lock()
	err := s.putConfirmation
	s.mux.Unlock()
	if err != nil {
		return err
	}
	return s.MemoryStore.PutConfirmation(ctx, confirmation)
}

func (s *failingStore) AppendMessages(ctx context.Context, sessionID string, messages ...StoredMessage) error {
	s.mux.Lock()
	err := s.toolResults
	s.mux.Unlock()
	if err != nil {
		for _, message := range messages {
			for _, content := range message.Content {
				if content.Type == llm.ContentToolResult {
					return err
				}
			}
		}
	}
	return s.MemoryStore.AppendMessages(ctx, sessionID, messages...)
}

// orphanedToolUses reports the tool_use ids in a history that nothing answers.
// Anthropic and OpenAI both refuse such a conversation with a 400, so a session
// that has one is unusable until it is repaired.
func orphanedToolUses(messages []llm.Message) []string {
	answered := map[string]bool{}
	called := []string{}
	for _, message := range messages {
		for _, content := range message.Content {
			switch content.Type {
			case llm.ContentToolUse:
				called = append(called, content.ToolUseID)
			case llm.ContentToolResult:
				answered[content.ToolUseID] = true
			}
		}
	}
	orphans := []string{}
	for _, id := range called {
		if !answered[id] {
			orphans = append(orphans, id)
		}
	}
	return orphans
}

func storedConversation(t *testing.T, h *harness, sessionID string) []llm.Message {
	t.Helper()
	stored, err := h.engine.store.Messages(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	out := make([]llm.Message, 0, len(stored))
	for _, message := range stored {
		out = append(out, message.Message())
	}
	return out
}

// newFailingHarness is newHarness with a store that can be made to fail.
func newFailingHarness(t *testing.T, store *failingStore, turns ...[]llm.Event) *harness {
	t.Helper()

	tracker := &ranTools{}
	registry, err := testTools(tracker)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	adminStore := admin.NewMemoryStore()
	pricing := llm.NewPricing("EUR", llm.ModelPrice{
		Model: "fake-model", InputPerMTok: 1000, OutputPerMTok: 1000,
	})
	adminService, err := admin.New(adminStore, pricing)
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, adminService, &fixedIDs{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	provider := newScriptedProvider("fake", turns...)
	providers, err := llm.NewRegistry(provider)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	engine, err := New(context.Background(), providers, dispatcher, store, adminService, &fixedIDs{}, Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return &harness{
		engine: engine, provider: provider, tracker: tracker,
		admin: adminService, registry: registry,
	}
}

// A confirmation that cannot be recorded used to leave the assistant's tool_use
// with no tool_result and let the loop run on. Both native protocols answer such a
// history with a 400, and it is stored, so every later turn on that session fails
// the same way with no repair short of deleting it.
func TestAConfirmationThatCannotBeRecordedStillAnswersTheToolUse(t *testing.T) {
	store := newFailingStore()
	h := newFailingHarness(t, store,
		toolTurn("call-1", "confirmed_tool"),
		textTurn("carrying on regardless"),
	)
	store.failPutConfirmation(errors.New("the confirmation store is unavailable"))

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "do it")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	if orphans := orphanedToolUses(storedConversation(t, h, session.ID)); len(orphans) > 0 {
		t.Errorf("stored history has tool_use ids nothing answers: %v", orphans)
	}
	// And it says so: a caller that read "awaiting_confirmation" here would wait for
	// a decision on a request that was never recorded.
	done := find(drain(t, events), EventDone)
	if len(done) != 1 || done[0].StopReason != StopConfirmationUnavailable {
		t.Errorf("done = %+v, want one with stop reason %q", done, StopConfirmationUnavailable)
	}
	// The developer will never be asked, so the exchange has to stop rather than
	// call the provider again on a history it cannot accept.
	if calls := h.provider.callCount(); calls != 1 {
		t.Errorf("provider calls = %d, want 1: the exchange must not continue past a "+
			"confirmation it could not record", calls)
	}
}

// The same property for the other path: the results were produced but the write
// that answers the tool_use failed. The stored history is beyond saving in that
// case, so what has to hold is that the *next* turn is still possible — the
// conversation handed to the provider answers every call.
func TestAFailedToolResultWriteLeavesTheSessionReplayable(t *testing.T) {
	store := newFailingStore()
	h := newFailingHarness(t, store,
		toolTurn("call-1", "l0_tool"),
		textTurn("first"),
		textTurn("second"),
	)
	store.failToolResults(errors.New("the database is unavailable"))

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "go")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)
	if len(find(collected, EventError)) == 0 {
		t.Fatal("the failed write was not reported to the developer")
	}
	// The stored history really is broken — otherwise this test would pass for the
	// wrong reason and prove nothing about reading it back.
	if orphans := orphanedToolUses(storedConversation(t, h, session.ID)); len(orphans) != 1 {
		t.Fatalf("stored orphans = %v, want the one the failed write left behind", orphans)
	}

	// The database comes back, and the developer sends again.
	store.failToolResults(nil)
	resumed, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "again")
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	drain(t, resumed)

	sent := h.provider.lastRequest(t).Messages
	if orphans := orphanedToolUses(sent); len(orphans) > 0 {
		t.Errorf("the provider was sent tool_use ids nothing answers: %v; "+
			"both native protocols refuse that with a 400", orphans)
	}
}

// failOnceStore fails the first write of a turn's tool results and accepts the
// next, which is what a cancelled context or a momentary database outage looks
// like from here.
type failOnceStore struct {
	*MemoryStore
	mux    sync.Mutex
	failed bool
}

func (s *failOnceStore) AppendMessages(ctx context.Context, sessionID string, messages ...StoredMessage) error {
	carriesResults := false
	for _, message := range messages {
		for _, content := range message.Content {
			if content.Type == llm.ContentToolResult {
				carriesResults = true
			}
		}
	}
	if carriesResults {
		s.mux.Lock()
		first := !s.failed
		s.failed = true
		s.mux.Unlock()
		if first {
			return context.Canceled
		}
	}
	return s.MemoryStore.AppendMessages(ctx, sessionID, messages...)
}

// The write that answers a tool call fails for reasons that say nothing about
// whether the store would accept it — the exchange's own deadline, or the
// developer pressing stop. Retrying without that deadline is what keeps the
// session from being left with a tool call nothing answers.
func TestAToolResultWriteRefusedByACancelledContextIsRetried(t *testing.T) {
	store := &failOnceStore{MemoryStore: NewMemoryStore()}
	h := newFailingHarness(t, &failingStore{MemoryStore: store.MemoryStore})
	// The harness above only exists for its collaborators; rebuild the engine over
	// the store under test.
	engine, err := New(context.Background(), h.engine.providers, h.engine.dispatcher, store,
		h.engine.limits, &fixedIDs{}, Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h.engine = engine
	h.provider.turns = [][]llm.Event{toolTurn("call-1", "l0_tool"), textTurn("done")}

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "go")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	collected := drain(t, events)

	if errs := find(collected, EventError); len(errs) > 0 {
		t.Errorf("the retry did not settle the write: %q", errs[0].Error)
	}
	if orphans := orphanedToolUses(storedConversation(t, h, session.ID)); len(orphans) > 0 {
		t.Errorf("stored history has tool_use ids nothing answers: %v", orphans)
	}
}

// writesThenFailsStore stores the message and then reports an error, which is
// what a transaction that commits and an acknowledgement that never comes back
// look like from the caller's side.
type writesThenFailsStore struct {
	*MemoryStore
	mux    sync.Mutex
	failed bool
}

func (s *writesThenFailsStore) AppendMessages(ctx context.Context, sessionID string, messages ...StoredMessage) error {
	carriesResults := false
	for _, message := range messages {
		for _, content := range message.Content {
			if content.Type == llm.ContentToolResult {
				carriesResults = true
			}
		}
	}
	if err := s.MemoryStore.AppendMessages(ctx, sessionID, messages...); err != nil {
		return err
	}
	if !carriesResults {
		return nil
	}
	s.mux.Lock()
	first := !s.failed
	s.failed = true
	s.mux.Unlock()
	if first {
		return context.Canceled
	}
	return nil
}

// The retry that saves a cancelled write must not answer the same tool call
// twice. An unmatched tool_result is refused by both protocols just as an
// unanswered tool_use is, so a retry that duplicated the write would produce the
// very failure it exists to prevent.
func TestTheToolResultRetryDoesNotAnswerTheSameCallTwice(t *testing.T) {
	store := &writesThenFailsStore{MemoryStore: NewMemoryStore()}
	h := newFailingHarness(t, &failingStore{MemoryStore: store.MemoryStore})
	engine, err := New(context.Background(), h.engine.providers, h.engine.dispatcher, store,
		h.engine.limits, &fixedIDs{}, Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h.engine = engine
	h.provider.turns = [][]llm.Event{toolTurn("call-1", "l0_tool"), textTurn("done")}

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "go")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	answers := 0
	for _, message := range storedConversation(t, h, session.ID) {
		for _, content := range message.Content {
			if content.Type == llm.ContentToolResult && content.ToolUseID == "call-1" {
				answers++
			}
		}
	}
	if answers != 1 {
		t.Errorf("tool_result blocks for call-1 = %d, want exactly 1", answers)
	}
}

// An out-of-band provider (§5.7's CLI) runs ODE's tools itself over MCP and
// reports what they returned. Those results were never stored, so every such turn
// left the assistant's tool_use blocks unanswered — a history both native
// protocols refuse, and one that a repair on read can only describe as lost. The
// results are in hand; they belong in the history.
func TestAnOutOfBandProvidersToolResultsAreStoredBesideItsCalls(t *testing.T) {
	h := newHarness(t, []llm.Event{
		llm.ToolCallEvent(llm.ToolCall{
			ID: "call-1", Name: "list_devices", Input: json.RawMessage(`{}`),
		}),
		llm.ToolResultEvent(llm.ToolResult{
			CallID: "call-1", Name: "list_devices",
			Content: json.RawMessage(`{"devices":["oven"]}`),
		}),
		llm.TextEvent("you have one device."),
		llm.DoneEvent("end_turn", llm.Usage{
			InputTokens: 10, OutputTokens: 5, Provider: "fake", Model: "fake-model",
		}),
	}, textTurn("and it is an oven."))
	h.provider.capabilities = llm.Capabilities{
		Tools: true, ToolsOutOfBand: true, Streaming: true, System: true,
		Models: []string{"fake-model"},
	}

	session := h.session(t, tools.L0)
	events, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser,
		session.ID, "which devices are there?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, events)

	if orphans := orphanedToolUses(storedConversation(t, h, session.ID)); len(orphans) > 0 {
		t.Errorf("stored history has tool_use ids nothing answers: %v", orphans)
	}

	// The next turn must carry what the tool actually returned. Reporting it as
	// lost would be ODE telling the model, in the developer's voice, that a call it
	// has the answer to did not complete.
	resumed, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser,
		session.ID, "which one?")
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	drain(t, resumed)

	replayed := ""
	for _, message := range h.provider.lastRequest(t).Messages {
		for _, content := range message.Content {
			if content.Type == llm.ContentToolResult && content.ToolUseID == "call-1" {
				replayed = content.ToolResult
			}
		}
	}
	if !strings.Contains(replayed, "oven") {
		t.Errorf("the replayed result for call-1 is %q, want what the tool returned", replayed)
	}
}

// cancellingProvider streams a first piece of text, waits for the exchange to be
// cancelled, and only then finishes — reporting what the turn cost, which is what
// a real adapter does once it stops discarding its accumulated usage.
type cancellingProvider struct {
	name  string
	usage llm.Usage
}

func (p *cancellingProvider) Name() string { return p.name }
func (p *cancellingProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{Tools: true, Streaming: true, System: true, Models: []string{"fake-model"}}
}

func (p *cancellingProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	out := make(chan llm.Event, 16)
	go func() {
		defer close(out)
		out <- llm.TextEvent("the answer begins")
		<-ctx.Done()
		// Ordered so the done event sits behind an event the consumer sees after the
		// cancellation, which is the arrangement that used to lose it.
		out <- llm.TextEvent(" and is cut off")
		out <- llm.DoneEvent(llm.StopReasonCancelled, p.usage)
	}()
	return out, nil
}

// A turn the developer stops has still been paid for: the provider billed the
// input it read and the output it produced. Recording nothing for it makes
// cancellation a free, repeatable way past §3.3's caps — send a large context,
// read the streamed answer, stop before the end.
func TestACancelledTurnIsAccountedForWhatItAlreadySpent(t *testing.T) {
	provider := &cancellingProvider{name: "cancelling", usage: llm.Usage{
		InputTokens: 200000, OutputTokens: 120, Provider: "cancelling", Model: "fake-model",
	}}
	providers, err := llm.NewRegistry(provider)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	tracker := &ranTools{}
	registry, err := testTools(tracker)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	adminStore := admin.NewMemoryStore()
	pricing := llm.NewPricing("EUR", llm.ModelPrice{
		Model: "fake-model", InputPerMTok: 1000, OutputPerMTok: 1000,
	})
	adminService, err := admin.New(adminStore, pricing)
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, adminService, &fixedIDs{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	engine, err := New(context.Background(), providers, dispatcher, NewMemoryStore(),
		adminService, &fixedIDs{}, Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	ctx := context.Background()
	session, err := engine.CreateSession(ctx, testUser, CreateRequest{Tier: tools.L0})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	exchange, err := engine.Send(ctx, StaticToken(testToken), testUser, session.ID, "a very long question")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	events, detach := exchange.Subscribe()
	defer detach()
	for event := range events {
		if event.Type == EventTextDelta {
			if err := engine.CancelExchange(ctx, testUser, session.ID); err != nil {
				t.Fatalf("CancelExchange: %v", err)
			}
			break
		}
	}
	select {
	case <-exchange.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled exchange did not finish")
	}

	spend, err := adminService.Spend(ctx, testUser, 0)
	if err != nil {
		t.Fatalf("Spend: %v", err)
	}
	if spend.Tokens == 0 {
		t.Errorf("recorded spend = %+v; a cancelled turn was billed by the provider and "+
			"must be accounted, or §3.3's caps are bypassed by stopping every turn", spend)
	}
}

// Where the repaired tool_result lands matters as much as that it exists: both
// protocols want every tool call answered at the start of the turn that follows
// the assistant's, and Anthropic in particular will not take two user turns in a
// row. So an existing user turn is extended rather than preceded by a new one.
func TestRepairingAnUnansweredToolCallKeepsTheTurnStructureValid(t *testing.T) {
	assistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{
		{Type: llm.ContentToolUse, ToolUseID: "call-1", ToolName: "l0_tool"},
		{Type: llm.ContentToolUse, ToolUseID: "call-2", ToolName: "l1_tool"},
	}}

	t.Run("merged into the user turn that follows", func(t *testing.T) {
		repaired := repairUnansweredToolCalls([]llm.Message{
			llm.UserText("go"),
			assistant,
			// Only one of the two calls was answered.
			{Role: llm.RoleUser, Content: []llm.Content{
				{Type: llm.ContentToolResult, ToolUseID: "call-2", ToolResult: `{"ran":true}`},
				{Type: llm.ContentText, Text: "and what about the other one?"},
			}},
		})
		if len(repaired) != 3 {
			t.Fatalf("messages = %d, want 3: the answer belongs in the existing user turn", len(repaired))
		}
		if orphans := orphanedToolUses(repaired); len(orphans) > 0 {
			t.Errorf("still unanswered: %v", orphans)
		}
		if got := repaired[2].Content[0]; got.Type != llm.ContentToolResult || !got.IsError {
			t.Errorf("first block of the user turn = %+v, want the repaired tool result", got)
		}
		// The answer that was already there is kept as it was.
		answers := map[string]string{}
		for _, content := range repaired[2].Content {
			if content.Type == llm.ContentToolResult {
				answers[content.ToolUseID] = content.ToolResult
			}
		}
		if answers["call-2"] != `{"ran":true}` {
			t.Errorf("the real result for call-2 was replaced: %q", answers["call-2"])
		}
		if !strings.Contains(answers["call-1"], "not known") {
			t.Errorf("the repaired result for call-1 claims to know something: %q", answers["call-1"])
		}
	})

	t.Run("added as its own turn when nothing follows", func(t *testing.T) {
		repaired := repairUnansweredToolCalls([]llm.Message{llm.UserText("go"), assistant})
		if len(repaired) != 3 {
			t.Fatalf("messages = %d, want a user turn appended", len(repaired))
		}
		if repaired[2].Role != llm.RoleUser {
			t.Errorf("the repair turn has role %q, want user: both protocols carry a tool "+
				"result on a user turn", repaired[2].Role)
		}
		if orphans := orphanedToolUses(repaired); len(orphans) > 0 {
			t.Errorf("still unanswered: %v", orphans)
		}
	})

	t.Run("a healthy conversation is left alone", func(t *testing.T) {
		healthy := []llm.Message{
			llm.UserText("go"),
			{Role: llm.RoleAssistant, Content: []llm.Content{
				{Type: llm.ContentToolUse, ToolUseID: "call-1", ToolName: "l0_tool"},
			}},
			{Role: llm.RoleUser, Content: []llm.Content{
				{Type: llm.ContentToolResult, ToolUseID: "call-1", ToolResult: `{"ran":true}`},
			}},
			llm.AssistantText("done"),
		}
		repaired := repairUnansweredToolCalls(healthy)
		if len(repaired) != len(healthy) {
			t.Fatalf("messages = %d, want %d unchanged", len(repaired), len(healthy))
		}
		for i := range healthy {
			if len(repaired[i].Content) != len(healthy[i].Content) {
				t.Errorf("message %d was rewritten: %+v", i, repaired[i])
			}
		}
	})
}
