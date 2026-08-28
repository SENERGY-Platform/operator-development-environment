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

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// The point of these tests: the MCP transport must not be a second, weaker door to
// the same tools. Every assertion here has a twin in pkg/tools — the difference is
// that these go over HTTP with a real MCP client.

type fakeSessions struct {
	mux         sync.RWMutex
	tiers       map[string]tools.Tier
	owner       map[string]string
	workbenches map[string]string
	autoRun     map[string]bool

	// hold stands in for the chat engine's confirmation hold. Two things a test
	// sets: whether a call is held at all, and what the developer answers.
	holdable bool
	approve  bool
	// held records the calls that reached the hold, so a test can tell "held and
	// approved" from "ran without asking".
	held       []string
	dispatcher *tools.Dispatcher
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{
		tiers: map[string]tools.Tier{}, owner: map[string]string{},
		workbenches: map[string]string{}, autoRun: map[string]bool{},
	}
}

// Hold answers the way the engine does: refuse when nothing could ask the
// developer, otherwise run the call the decision permits.
func (s *fakeSessions) Hold(
	ctx context.Context, req tools.Request, call tools.Call,
) (tools.Result, bool, error) {
	s.mux.Lock()
	holdable, approve := s.holdable, s.approve
	if holdable {
		s.held = append(s.held, call.Name)
	}
	dispatcher := s.dispatcher
	s.mux.Unlock()

	if !holdable {
		return tools.Result{}, false, nil
	}
	if !approve {
		return tools.Result{
			CallID: call.ID, Tool: call.Name, Outcome: tools.Outcome("rejected"),
			Content: map[string]any{"rejected": true},
		}, true, nil
	}

	// Through the dispatcher's confirmed path, so the test exercises the same gate
	// the engine goes through rather than fabricating a result.
	result := dispatcher.Dispatch(ctx, req, call)
	if result.Confirmation == nil {
		return result, true, nil
	}
	return dispatcher.Confirm(ctx, req, *result.Confirmation), true, nil
}

func (s *fakeSessions) wasHeld(name string) bool {
	s.mux.RLock()
	defer s.mux.RUnlock()
	for _, held := range s.held {
		if held == name {
			return true
		}
	}
	return false
}

func (s *fakeSessions) add(sessionID, userSub string, tier tools.Tier) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.tiers[sessionID] = tier
	s.owner[sessionID] = userSub
}

// setWork records what the session acts in and whether it holds the standing
// answer, which the transport has to carry as faithfully as the tier.
func (s *fakeSessions) setWork(sessionID, workbench string, autoRun bool) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.workbenches[sessionID] = workbench
	s.autoRun[sessionID] = autoRun
}

func (s *fakeSessions) State(_ context.Context, userSub, sessionID string) (
	tools.Tier, string, bool, error,
) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	if s.owner[sessionID] != userSub {
		return tools.DefaultTier, "", false, errors.New("no such session for this user")
	}
	tier, found := s.tiers[sessionID]
	if !found {
		return tools.DefaultTier, "", false, errors.New("no such session")
	}
	return tier, s.workbenches[sessionID], s.autoRun[sessionID], nil
}

type ran struct {
	mux    sync.Mutex
	called []string
}

func (r *ran) executor(name string) tools.Executor {
	return func(_ context.Context, req tools.Request) (any, error) {
		r.mux.Lock()
		defer r.mux.Unlock()
		r.called = append(r.called, name)
		return map[string]any{
			"ran": name, "tier": req.Tier.String(), "session": req.SessionID,
			// Reported so a test can see what the transport carried, rather than only
			// that the call arrived.
			"workbench": req.WorkbenchID, "auto_run": req.AutoRun,
		}, nil
	}
}

func (r *ran) was(name string) bool {
	r.mux.Lock()
	defer r.mux.Unlock()
	for _, called := range r.called {
		if called == name {
			return true
		}
	}
	return false
}

type ids struct {
	mux sync.Mutex
	n   int
}

func (i *ids) NewID() string {
	i.mux.Lock()
	defer i.mux.Unlock()
	i.n++
	return "conf-1"
}

func testRegistry(tracker *ran) (*tools.Registry, error) {
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	declare := func(name string, tier tools.Tier, confirm bool) tools.Definition {
		return tools.NewDefinition(tools.Definition{
			Name: name, Description: "test " + name, Effect: "test",
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

// harness is a live MCP server over httptest.
type harness struct {
	server   *httptest.Server
	sessions *fakeSessions
	tracker  *ran
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	tracker := &ran{}
	registry, err := testRegistry(tracker)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, nil, &ids{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	sessions := newFakeSessions()
	sessions.dispatcher = dispatcher

	server, err := New(dispatcher, sessions, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The authenticator stands in for api.AuthenticateMCP: a bearer token whose
	// value is the user's sub, so the ownership check has something to check.
	authenticate := func(request *http.Request) (string, string, error) {
		header := request.Header.Get("Authorization")
		sub := strings.TrimPrefix(header, "Bearer ")
		if sub == "" {
			return "", "", errors.New("missing auth token")
		}
		return sub, header, nil
	}

	httpServer := httptest.NewServer(server.Handler(authenticate))
	t.Cleanup(httpServer.Close)

	return &harness{server: httpServer, sessions: sessions, tracker: tracker}
}

// connect opens a real MCP client session.
func (h *harness) connect(t *testing.T, userSub, sessionID string) *sdk.ClientSession {
	t.Helper()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint: h.server.URL,
		HTTPClient: &http.Client{
			Transport: &headerTransport{
				userSub:   userSub,
				sessionID: sessionID,
			},
			Timeout: 10 * time.Second,
		},
		// A stateless server answers GET with 405, so the standalone stream would
		// only produce noise.
		DisableStandaloneSSE: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// headerTransport puts the auth and session headers on every request, the way the
// CLI provider's mcpConfig does.
type headerTransport struct {
	userSub   string
	sessionID string
}

func (h *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+h.userSub)
	if h.sessionID != "" {
		clone.Header.Set(llm.SessionHeader, h.sessionID)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

func toolNames(t *testing.T, session *sdk.ClientSession) []string {
	t.Helper()
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		out = append(out, tool.Name)
	}
	sort.Strings(out)
	return out
}

func callText(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	parts := []string{}
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "")
}

// --- the tier gate over MCP ---

// TestMCPAdvertisesOnlyThePermittedTier is the analogue of the registry test, over
// the wire: at L0 an MCP client cannot even see the value-bearing tools.
func TestMCPAdvertisesOnlyThePermittedTier(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	h.sessions.add("sess-l2", "alice", tools.L2)

	l0 := toolNames(t, h.connect(t, "alice", "sess-l0"))
	if strings.Join(l0, ",") != "confirmed_tool,l0_tool" {
		t.Errorf("L0 advertises %v, want only the L0 tools", l0)
	}

	l2 := toolNames(t, h.connect(t, "alice", "sess-l2"))
	if len(l2) != 4 {
		t.Errorf("L2 advertises %v, want all four", l2)
	}
}

// TestMCPRefusesAToolAboveTheTier is the one that matters most: even asked
// directly by name, a tool above the session's tier does not run.
func TestMCPRefusesAToolAboveTheTier(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	session := h.connect(t, "alice", "sess-l0")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "l2_tool", Arguments: map[string]any{},
	})

	// The SDK may refuse an unadvertised name itself, or the call may reach the
	// dispatcher and be refused there. Either is correct; what must not happen is
	// the tool running.
	if h.tracker.was("l2_tool") {
		t.Fatal("an L2 tool ran through MCP in an L0 session")
	}
	if err == nil && !result.IsError {
		t.Errorf("the call was neither refused nor errored: %s", callText(t, result))
	}
}

func TestMCPRunsAPermittedTool(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	session := h.connect(t, "alice", "sess-l0")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "l0_tool", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("a permitted tool was refused: %s", callText(t, result))
	}
	if !h.tracker.was("l0_tool") {
		t.Error("the permitted tool never ran")
	}

	// The dispatch must carry the session's tier and id, or a tool that shapes its
	// answer by tier would see the wrong one.
	var payload map[string]any
	if err := json.Unmarshal([]byte(callText(t, result)), &payload); err != nil {
		t.Fatalf("result was not JSON: %v", err)
	}
	if payload["tier"] != "L0" || payload["session"] != "sess-l0" {
		t.Errorf("dispatch context = %v, want tier L0 and session sess-l0", payload)
	}
}

// TestMCPTierComesFromTheSessionNotTheClient closes the obvious hole: a client
// must not be able to name its own tier.
func TestMCPTierComesFromTheSessionNotTheClient(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)

	client := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint:             h.server.URL,
		DisableStandaloneSSE: true,
		HTTPClient: &http.Client{
			// A client that asserts L2 in a header it invented.
			Transport: &tierClaimingTransport{userSub: "alice", sessionID: "sess-l0"},
			Timeout:   10 * time.Second,
		},
	}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	names := toolNames(t, session)
	for _, name := range names {
		if name == "l2_tool" || name == "l1_tool" {
			t.Errorf("a client-supplied tier header raised the exposure tier: got %v", names)
		}
	}
}

type tierClaimingTransport struct {
	userSub   string
	sessionID string
}

func (t *tierClaimingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+t.userSub)
	clone.Header.Set(llm.SessionHeader, t.sessionID)
	clone.Header.Set("X-ODE-Tier", "L2")
	clone.Header.Set("Tier", "L2")
	return http.DefaultTransport.RoundTrip(clone)
}

// --- confirmed tools ---

// TestMCPRefusesAConfirmedToolWithNobodyToAsk is the case the refusal is still
// for: no turn in flight means the request would never reach a developer, so the
// call says so rather than waiting for an answer that cannot come.
func TestMCPRefusesAConfirmedToolWithNobodyToAsk(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	h.sessions.holdable = false
	session := h.connect(t, "alice", "sess-l0")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "confirmed_tool", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Error("a tool needing confirmation was not refused")
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("a tool needing confirmation ran with no confirmation")
	}
	if !strings.Contains(callText(t, result), "confirmation_unavailable") {
		t.Errorf("refusal = %s, want confirmation_unavailable", callText(t, result))
	}
}

/*
The transport carries the whole session, not the parts it remembered.

Two gates read a session field that this handler used to leave at its zero value,
and both failures are on the CLI provider only, because it is the one that reaches
the tools through here rather than through the engine's own loop:

  - the workbench, without which write_file answers "2 workbenches are open, so the
    request has to name the one it means" and run_code lands in whichever kernel an
    unnamed workbench resolves to — the worse of the two, because it succeeds;
  - the standing answer of D33, without which auto mode did nothing at all on this
    transport and every recognised `run_code` was still put to the developer.
*/
func TestMCPCarriesTheSessionsWorkbenchAndStandingAnswer(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	h.sessions.setWork("sess-l0", "wb-two", true)
	session := h.connect(t, "alice", "sess-l0")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "l0_tool", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := callText(t, result)
	if !strings.Contains(text, `"workbench":"wb-two"`) {
		t.Errorf("result = %s, want the session's workbench", text)
	}
	if !strings.Contains(text, `"auto_run":true`) {
		t.Errorf("result = %s, want the session's standing answer", text)
	}
}

// And a session that holds neither says so, rather than the handler having a
// default of its own.
func TestMCPCarriesTheAbsenceOfBothToo(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	session := h.connect(t, "alice", "sess-l0")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "l0_tool", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := callText(t, result)
	if !strings.Contains(text, `"workbench":""`) || !strings.Contains(text, `"auto_run":false`) {
		t.Errorf("result = %s, want an empty workbench and no standing answer", text)
	}
}

// TestMCPHoldsAConfirmedToolAndRunsWhatIsApproved is the property the refusal used
// to make impossible: on this transport a confirmed tool is held for the
// developer, and their approval is what runs it.
func TestMCPHoldsAConfirmedToolAndRunsWhatIsApproved(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	h.sessions.holdable = true
	h.sessions.approve = true
	session := h.connect(t, "alice", "sess-l0")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "confirmed_tool", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Errorf("an approved call was reported as an error: %s", callText(t, result))
	}
	if !h.sessions.wasHeld("confirmed_tool") {
		t.Error("the call was not held for the developer")
	}
	if !h.tracker.was("confirmed_tool") {
		t.Error("an approved call did not run")
	}
	if !strings.Contains(callText(t, result), `"ran":"confirmed_tool"`) {
		t.Errorf("result = %s, want the tool's own answer", callText(t, result))
	}
}

// TestMCPReturnsARejectionAsAnAnswer checks that declining is reported as an
// outcome rather than as a failure. A rejection the model reads as an error is one
// it learns to route around, and the tool exists to be asked about.
func TestMCPReturnsARejectionAsAnAnswer(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("sess-l0", "alice", tools.L0)
	h.sessions.holdable = true
	h.sessions.approve = false
	session := h.connect(t, "alice", "sess-l0")

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "confirmed_tool", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Errorf("a rejection was reported as an error: %s", callText(t, result))
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("a declined call ran anyway")
	}
	if !strings.Contains(callText(t, result), `"rejected":true`) {
		t.Errorf("result = %s, want the rejection", callText(t, result))
	}
}

// --- authentication and ownership ---

func TestMCPRequiresAToken(t *testing.T) {
	h := newHarness(t)
	response, err := http.Post(h.server.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no token", response.StatusCode)
	}
}

func TestMCPRequiresASessionHeader(t *testing.T) {
	h := newHarness(t)
	request, _ := http.NewRequest(http.MethodPost, h.server.URL, strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer alice")
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without a session header: there would be no tier to enforce",
			response.StatusCode)
	}
}

// TestMCPChecksSessionOwnership stops a developer inheriting someone else's tier
// by naming their session id.
func TestMCPChecksSessionOwnership(t *testing.T) {
	h := newHarness(t)
	h.sessions.add("alice-session", "alice", tools.L2)

	request, _ := http.NewRequest(http.MethodPost, h.server.URL, strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer bob")
	request.Header.Set(llm.SessionHeader, "alice-session")
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404: bob must not reach alice's session tier",
			response.StatusCode)
	}
}

// --- the endpoint helper ---

func TestEndpointJoinsCleanly(t *testing.T) {
	for base, want := range map[string]string{
		"https://ode.example.org":  "https://ode.example.org/mcp",
		"https://ode.example.org/": "https://ode.example.org/mcp",
	} {
		if got := Endpoint(base); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestNewRequiresCollaborators(t *testing.T) {
	if _, err := New(nil, newFakeSessions(), "v"); err == nil {
		t.Error("New accepted a nil dispatcher")
	}
	registry, _ := tools.NewRegistry()
	dispatcher, _ := tools.NewDispatcher(registry, nil, &ids{})
	if _, err := New(dispatcher, nil, "v"); err == nil {
		t.Error("New accepted a nil session source")
	}
}
