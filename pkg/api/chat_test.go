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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// --- an M3 harness ---

type chatHarness struct {
	router http.Handler
	engine *chat.Engine
	admin  *admin.Service
}

type stubProvider struct {
	mux   sync.Mutex
	turns int
}

func (s *stubProvider) Name() string { return "stub" }
func (s *stubProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{Tools: true, Streaming: true, System: true, Models: []string{"stub-model"}}
}

func (s *stubProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	s.mux.Lock()
	s.turns++
	s.mux.Unlock()

	out := make(chan llm.Event, 3)
	out <- llm.TextEvent("hello from the stub")
	out <- llm.DoneEvent("end_turn", llm.Usage{
		InputTokens: 5, OutputTokens: 3, Provider: "stub", Model: "stub-model",
	})
	close(out)
	return out, nil
}

type stubIDs struct {
	mux sync.Mutex
	n   int
}

func (s *stubIDs) NewID() string {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.n++
	return fmt.Sprintf("id-%d", s.n)
}

func newChatHarness(t *testing.T) *chatHarness {
	t.Helper()

	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	noop := func(context.Context, tools.Request) (any, error) {
		return map[string]any{"ok": true}, nil
	}
	registry, err := tools.NewRegistry(
		tools.NewDefinition(tools.Definition{
			Name: "l0_tool", Description: "d", MinTier: tools.L0, Schema: schema,
		}, noop),
		tools.NewDefinition(tools.Definition{
			Name: "l2_tool", Description: "d", MinTier: tools.L2, Schema: schema,
		}, noop),
	)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	adminService, err := admin.New(admin.NewMemoryStore(), llm.NewPricing("EUR"))
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, adminService, &stubIDs{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	providers, err := llm.NewRegistry(&stubProvider{})
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	engine, err := chat.New(context.Background(), providers, dispatcher, chat.NewMemoryStore(),
		adminService, &stubIDs{}, chat.Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer", Debug: false},
		api.Deps{Chat: engine, Admin: adminService},
	)
	return &chatHarness{router: router, engine: engine, admin: adminService}
}

func (h *chatHarness) do(t *testing.T, method, path string, body any, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	if roles != nil {
		request.Header.Set("Authorization", "Bearer "+mintToken(roles))
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body was not JSON (%d): %s", recorder.Code, recorder.Body.String())
	}
	return decoded
}

// createSession returns a new session's id.
func (h *chatHarness) createSession(t *testing.T, tier string) string {
	t.Helper()
	body := map[string]any{}
	if tier != "" {
		body["exposure_tier"] = tier
	}
	recorder := h.do(t, http.MethodPost, "/chat/sessions", body, "developer")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", recorder.Code, recorder.Body.String())
	}
	return decodeBody(t, recorder)["id"].(string)
}

// --- the tool table (§5.8) ---

func TestToolTablePublishesTiersAndDenials(t *testing.T) {
	h := newChatHarness(t)
	recorder := h.do(t, http.MethodGet, "/llm/tools", nil, "developer")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody(t, recorder)

	published, ok := body["tools"].([]any)
	if !ok || len(published) != 2 {
		t.Fatalf("tools = %v, want the two declared", body["tools"])
	}
	first := published[0].(map[string]any)
	for _, field := range []string{"name", "min_tier", "confirm", "implemented"} {
		if _, present := first[field]; !present {
			t.Errorf("a published tool is missing %q", field)
		}
	}

	// The denied list is part of the published surface: "no tool exists" is a design
	// claim, and a reader should be able to check it against the table.
	denied, ok := body["denied"].(map[string]any)
	if !ok || len(denied) == 0 {
		t.Fatal("the denied capabilities are not published")
	}
	if _, present := denied["set_exposure_tier"]; !present {
		t.Error("changing the exposure tier is not listed as denied")
	}

	// And the per-tier lists, so the UI can show what raising the tier buys.
	tiers, ok := body["tiers"].([]any)
	if !ok || len(tiers) != 3 {
		t.Fatalf("tiers = %v, want three", body["tiers"])
	}
	l0 := tiers[0].(map[string]any)
	if l0["tier"] != "L0" {
		t.Errorf("first tier = %v, want L0", l0["tier"])
	}
	available := l0["available"].([]any)
	for _, name := range available {
		if name == "l2_tool" {
			t.Error("L0's available list includes an L2 tool")
		}
	}
}

func TestToolTableNeedsTheDeveloperRole(t *testing.T) {
	h := newChatHarness(t)
	if got := h.do(t, http.MethodGet, "/llm/tools", nil).Code; got != http.StatusUnauthorized {
		t.Errorf("no token gave %d, want 401", got)
	}
	if got := h.do(t, http.MethodGet, "/llm/tools", nil, "someone-else").Code; got != http.StatusForbidden {
		t.Errorf("wrong role gave %d, want 403", got)
	}
}

// --- sessions and the tier control ---

func TestSessionDefaultsToL0(t *testing.T) {
	h := newChatHarness(t)
	recorder := h.do(t, http.MethodPost, "/chat/sessions", map[string]any{}, "developer")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if tier := decodeBody(t, recorder)["exposure_tier"]; tier != "L0" {
		t.Errorf("tier = %v, want L0 by default (§3.2)", tier)
	}
}

func TestSetTierAndReadItsAudit(t *testing.T) {
	h := newChatHarness(t)
	id := h.createSession(t, "")

	recorder := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/tier",
		map[string]any{"exposure_tier": "L2"}, "developer")
	if recorder.Code != http.StatusOK {
		t.Fatalf("set tier: %d %s", recorder.Code, recorder.Body.String())
	}
	if tier := decodeBody(t, recorder)["exposure_tier"]; tier != "L2" {
		t.Errorf("tier = %v, want L2", tier)
	}

	recorder = h.do(t, http.MethodGet, "/chat/sessions/"+id+"/tier-changes", nil, "developer")
	if recorder.Code != http.StatusOK {
		t.Fatalf("audit: %d", recorder.Code)
	}
	changes := decodeBody(t, recorder)["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("audit entries = %d, want creation plus the change", len(changes))
	}
	last := changes[1].(map[string]any)
	if last["to"] != "L2" || last["from"] != "L0" {
		t.Errorf("last change = %v, want L0 → L2", last)
	}
	if last["user_sub"] == nil || last["at"] == nil {
		t.Error("the audit entry lacks the user or timestamp §3.2 requires")
	}
}

func TestSetTierRejectsAnInvalidTier(t *testing.T) {
	h := newChatHarness(t)
	id := h.createSession(t, "")
	if got := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/tier",
		map[string]any{"exposure_tier": "L9"}, "developer").Code; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

// TestTierCeilingIsEnforcedOverHTTP checks the admin bound reaches the route.
func TestTierCeilingIsEnforcedOverHTTP(t *testing.T) {
	h := newChatHarness(t)
	if err := h.admin.SetLimits(context.Background(), admin.GlobalSubject, admin.Limits{
		MaxTier: func() *tools.Tier { tier := tools.L0; return &tier }(),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	id := h.createSession(t, "")

	if got := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/tier",
		map[string]any{"exposure_tier": "L2"}, "developer").Code; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 above the admin ceiling", got)
	}
}

// --- SSE ---

// --- ownership over HTTP ---

func TestAnotherUsersSessionIs404(t *testing.T) {
	h := newChatHarness(t)
	id := h.createSession(t, "")

	// mintToken always uses sub user-123, so the engine is asked directly for a
	// session belonging to someone else and the route is checked against it.
	other, err := h.engine.CreateSession(context.Background(), "someone-else", chat.CreateRequest{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if got := h.do(t, http.MethodGet, "/chat/sessions/"+other.ID, nil, "developer").Code; got != http.StatusNotFound {
		t.Errorf("reading another user's session gave %d, want 404", got)
	}
	// And the caller's own still works, so the 404 is about ownership rather than
	// the route being broken.
	if got := h.do(t, http.MethodGet, "/chat/sessions/"+id, nil, "developer").Code; got != http.StatusOK {
		t.Errorf("reading the caller's own session gave %d, want 200", got)
	}
}

// --- confirmations ---

// --- the admin surface (§3.3) ---

// TestAdminRoutesRequireTheAdminRole is the security boundary of §3.3: the
// `developer` role is not enough to change the limits that bound it.
func TestAdminRoutesRequireTheAdminRole(t *testing.T) {
	h := newChatHarness(t)

	routes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/admin/limits", nil},
		{http.MethodPut, "/admin/limits", map[string]any{"token_cap": 10}},
		{http.MethodGet, "/admin/limits/user-123", nil},
		{http.MethodPut, "/admin/limits/user-123", map[string]any{"token_cap": 10}},
		{http.MethodGet, "/admin/usage", nil},
		{http.MethodGet, "/admin/tool-calls", nil},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			// A developer without the admin role.
			if got := h.do(t, route.method, route.path, route.body, "developer").Code; got != http.StatusForbidden {
				t.Errorf("as developer: %d, want 403", got)
			}
			// With it.
			got := h.do(t, route.method, route.path, route.body, "developer", "admin").Code
			if got != http.StatusOK {
				t.Errorf("as admin: %d, want 200", got)
			}
		})
	}
}

func TestAdminCanSetAndReadLimits(t *testing.T) {
	h := newChatHarness(t)

	recorder := h.do(t, http.MethodPut, "/admin/limits/user-123", map[string]any{
		"period":    "24h",
		"token_cap": 5000,
		"max_tier":  "L1",
	}, "developer", "admin")
	if recorder.Code != http.StatusOK {
		t.Fatalf("put limits: %d %s", recorder.Code, recorder.Body.String())
	}

	effective := decodeBody(t, recorder)["effective"].(map[string]any)
	if effective["token_cap"].(float64) != 5000 {
		t.Errorf("token cap = %v, want 5000", effective["token_cap"])
	}
	if effective["max_tier"] != "L1" {
		t.Errorf("max tier = %v, want L1", effective["max_tier"])
	}

	// And the listing reports what is enforced versus merely stored, so an admin
	// setting a kernel cap is not misled into thinking it binds.
	recorder = h.do(t, http.MethodGet, "/admin/limits", nil, "developer", "admin")
	body := decodeBody(t, recorder)
	if _, present := body["enforced"]; !present {
		t.Error("the limits surface does not say which fields are enforced")
	}
	declared, ok := body["declared"].(map[string]any)
	if !ok || len(declared) == 0 {
		t.Error("the limits surface does not say which fields are declared but not yet enforced")
	}
	if _, present := declared["kernel_cpu_max"]; !present {
		t.Error("the kernel caps should be listed as declared-not-enforced until M4")
	}
}

func TestAdminRejectsAMalformedPolicy(t *testing.T) {
	h := newChatHarness(t)
	if got := h.do(t, http.MethodPut, "/admin/limits",
		map[string]any{"period": "not-a-duration"}, "developer", "admin").Code; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

// --- /session ---

func TestSessionReportsCapabilitiesAndCeiling(t *testing.T) {
	h := newChatHarness(t)
	recorder := h.do(t, http.MethodGet, "/session", nil, "developer")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := decodeBody(t, recorder)

	if body["exposure_tier"] != "L0" {
		t.Errorf("default tier = %v, want L0", body["exposure_tier"])
	}
	if body["max_exposure_tier"] != "L2" {
		t.Errorf("max tier = %v, want L2 with no ceiling configured", body["max_exposure_tier"])
	}
	features := body["features"].(map[string]any)
	if features["chat"] != true {
		t.Error("chat is not reported as available")
	}
	providers, ok := body["providers"].([]any)
	if !ok || len(providers) != 1 {
		t.Errorf("providers = %v, want the one configured", body["providers"])
	}
}

// TestSessionOmitsM3FieldsWhenChatIsAbsent covers the degradation path: a
// deployment with no provider configured serves M0–M2 and says so, rather than
// advertising a chat surface that is not mounted.
func TestSessionOmitsM3FieldsWhenChatIsAbsent(t *testing.T) {
	h := newHarness(t) // the M0–M2 harness, no chat
	recorder := h.get(t, "/session", "developer")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	features := body["features"].(map[string]any)
	if features["chat"] != false {
		t.Error("chat is reported as available with no engine configured")
	}
	if _, present := body["providers"]; present {
		t.Error("providers are reported with no engine configured")
	}
}

// TestChatRoutesAbsentWithoutAnEngine checks the routes are not mounted at all,
// rather than mounted and panicking on the first call.
func TestChatRoutesAbsentWithoutAnEngine(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/chat/sessions", "/llm/tools", "/llm/providers", "/admin/limits"} {
		if got := h.get(t, path, "developer").Code; got != http.StatusNotFound {
			t.Errorf("%s gave %d with no engine configured, want 404", path, got)
		}
	}
}

// --- contract fixtures ---

// TestWriteContractFixtures emits the JSON the frontend's contract check assigns
// to its declared types (frontend/src/__contract__).
//
// Emitted from the real handlers rather than hand-written, which is the whole
// point: a hand-written fixture would only prove the frontend agrees with itself.
// It follows the precedent set for M2, whose fixture came from this harness for
// want of platform access — the values are a fake's, the field sets are the
// backend's own marshalling of its own types.
//
// Off by default so the suite stays read-only. Run with:
//
//	ODE_WRITE_CONTRACT=frontend/src/__contract__ go test ./pkg/api/ -run ContractFixtures
func TestWriteContractFixtures(t *testing.T) {
	dir := os.Getenv("ODE_WRITE_CONTRACT")
	if dir == "" {
		t.Skip("set ODE_WRITE_CONTRACT to the fixture directory to regenerate")
	}

	h := newChatHarness(t)
	ctx := context.Background()

	// A session that has been used, so the fixtures carry a real exchange rather
	// than an empty shell: a message, an assistant reply, and a tier change.
	//
	// Driven through the engine rather than over HTTP, because sending a message is
	// a WebSocket operation now — and these fixtures exist to pin the shape of the
	// REST documents, which is unaffected.
	id := h.createSession(t, "")
	exchange, err := h.engine.Send(ctx, chat.StaticToken("Bearer test"), "user-123", id, "which devices are there?")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-exchange.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the exchange did not finish")
	}
	if got := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/tier",
		map[string]any{"exposure_tier": "L1"}, "developer").Code; got != http.StatusOK {
		t.Fatalf("set tier: %d", got)
	}

	// Some accounting and audit rows, so those fixtures are not empty arrays.
	h.admin.RecordUsage(ctx, "user-123", id, llm.Usage{
		InputTokens: 120, OutputTokens: 45, Provider: "stub", Model: "stub-model",
		CostEUR: 0.0021, CostEstimated: true,
	})
	h.admin.RecordToolCall(ctx, tools.ToolCallRecord{
		UserSub: "user-123", SessionID: id, Tool: "l0_tool",
		Tier: tools.L0, Outcome: tools.OutcomeOK,
	})
	h.admin.RecordToolCall(ctx, tools.ToolCallRecord{
		UserSub: "user-123", SessionID: id, Tool: "l2_tool",
		Tier: tools.L0, Outcome: tools.OutcomeBlockedByTier,
	})

	cases := []struct {
		file   string
		method string
		path   string
		body   any
		roles  []string
	}{
		{"chat_providers.json", http.MethodGet, "/llm/providers", nil, []string{"developer"}},
		{"chat_tools.json", http.MethodGet, "/llm/tools", nil, []string{"developer"}},
		{"chat_sessions.json", http.MethodGet, "/chat/sessions", nil, []string{"developer"}},
		{"chat_session.json", http.MethodGet, "/chat/sessions/" + id, nil, []string{"developer"}},
		{"chat_tier_changes.json", http.MethodGet, "/chat/sessions/" + id + "/tier-changes", nil,
			[]string{"developer"}},
		{"session.json", http.MethodGet, "/session", nil, []string{"developer", "admin"}},
		{"admin_limits.json", http.MethodGet, "/admin/limits", nil, []string{"developer", "admin"}},
		{"admin_usage.json", http.MethodGet, "/admin/usage", nil, []string{"developer", "admin"}},
		{"admin_tool_calls.json", http.MethodGet, "/admin/tool-calls", nil, []string{"developer", "admin"}},
	}

	for _, tc := range cases {
		recorder := h.do(t, tc.method, tc.path, tc.body, tc.roles...)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.path, recorder.Code, recorder.Body.String())
		}

		// Re-indented so a diff is readable when the shape changes on purpose.
		var parsed any
		if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
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
}

// --- SSE liveness ---

// slowToolProvider asks for one tool on its first turn, then concludes. Paired with
// a deliberately slow executor, it reproduces the shape of an LLM-initiated profile:
// a long gap between the tool_call event and its result. Used by ws_chat_test.go.
type slowToolProvider struct {
	mux  sync.Mutex
	turn int
}

func (p *slowToolProvider) Name() string { return "slow" }
func (p *slowToolProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{Tools: true, Streaming: true, System: true, Models: []string{"slow-model"}}
}

func (p *slowToolProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Event, error) {
	p.mux.Lock()
	turn := p.turn
	p.turn++
	p.mux.Unlock()

	out := make(chan llm.Event, 4)
	if turn == 0 {
		out <- llm.ToolCallEvent(llm.ToolCall{
			ID: "c1", Name: "slow_tool", Input: json.RawMessage(`{}`),
		})
		out <- llm.DoneEvent("tool_use", llm.Usage{
			InputTokens: 1, OutputTokens: 1, Provider: "slow", Model: "slow-model",
		})
	} else {
		out <- llm.TextEvent("done")
		out <- llm.DoneEvent("end_turn", llm.Usage{
			InputTokens: 1, OutputTokens: 1, Provider: "slow", Model: "slow-model",
		})
	}
	close(out)
	return out, nil
}
