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
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// twoProviders is a harness with a second transport to move to, which the single
// provider in newHarness cannot express. "native" is an ordinary provider with two
// models; "cli" stands in for §5.7's out-of-band one — its tools run over MCP and
// it has no model list of its own, which is the deployment shape that produced the
// empty model in the pane header in the first place.
type twoProviderHarness struct {
	engine *Engine
	native *scriptedProvider
	cli    *scriptedProvider
	admin  *admin.Service
}

func newTwoProviderHarness(t *testing.T, cliTurns ...[]llm.Event) *twoProviderHarness {
	t.Helper()

	registry, err := testTools(&ranTools{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	adminStore := admin.NewMemoryStore()
	adminService, err := admin.New(adminStore, llm.NewPricing("EUR"))
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, adminService, &fixedIDs{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}

	native := newScriptedProvider("native")
	native.capabilities = llm.Capabilities{
		Tools: true, Streaming: true, System: true, ModelRequired: true,
		Models: []string{"big-model", "small-model"},
	}
	cli := newScriptedProvider("cli", cliTurns...)
	cli.capabilities = llm.Capabilities{
		Tools: true, Streaming: true, System: true, ToolsOutOfBand: true,
	}

	providers, err := llm.NewRegistry(native, cli)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	engine, err := New(context.Background(), providers, dispatcher, NewMemoryStore(),
		adminService, &fixedIDs{}, Options{MCPEndpoint: "http://ode.test/mcp"})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return &twoProviderHarness{engine: engine, native: native, cli: cli, admin: adminService}
}

func (h *twoProviderHarness) session(t *testing.T, provider, model string) Session {
	t.Helper()
	session, err := h.engine.CreateSession(context.Background(), testUser, CreateRequest{
		Provider: provider, Model: model,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session
}

// TestSetModelWithinAProvider is the common case: the developer started cheap and
// wants the bigger model on the same transport, without losing the conversation
// that established what the problem is.
func TestSetModelWithinAProvider(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "native", "small-model")

	moved, err := h.engine.SetModel(context.Background(), testUser, session.ID, "", "big-model")
	if err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if moved.Provider != "native" || moved.Model != "big-model" {
		t.Errorf("session is on %s/%s, want native/big-model", moved.Provider, moved.Model)
	}

	// Written, not merely returned: the next turn reads the store.
	reread, err := h.engine.Session(context.Background(), testUser, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if reread.Model != "big-model" {
		t.Errorf("the stored model is %q, want big-model", reread.Model)
	}
}

// TestSetModelKeepsTheModelWhenOnlyTheProviderIsNamed guards the one asymmetry in
// the empty-string handling. On the same provider an empty model means "leave it";
// on a different one it means "take the new provider's default", because the model
// the session holds belongs to the provider it is leaving.
func TestSetModelKeepsTheModelWhenOnlyTheProviderIsNamed(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "native", "small-model")

	same, err := h.engine.SetModel(context.Background(), testUser, session.ID, "native", "")
	if err != nil {
		t.Fatalf("SetModel onto the same provider: %v", err)
	}
	if same.Model != "small-model" {
		t.Errorf("naming only the provider changed the model to %q, want small-model kept", same.Model)
	}

	// The CLI declares no models and does not require one, so its default resolves
	// to the empty string — the provider chooses. What must not survive is
	// "small-model", which the CLI knows nothing about.
	moved, err := h.engine.SetModel(context.Background(), testUser, session.ID, "cli", "")
	if err != nil {
		t.Fatalf("SetModel onto the CLI: %v", err)
	}
	if moved.Provider != "cli" {
		t.Errorf("provider is %q, want cli", moved.Provider)
	}
	if moved.Model != "" {
		t.Errorf("model is %q, want the CLI's own default (empty)", moved.Model)
	}
}

// TestSetModelTakesTheDefaultOfTheProviderItMovesTo is the other half of that: a
// provider that *requires* a model answers the empty string with its first.
func TestSetModelTakesTheDefaultOfTheProviderItMovesTo(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "cli", "")

	moved, err := h.engine.SetModel(context.Background(), testUser, session.ID, "native", "")
	if err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if moved.Model != "big-model" {
		t.Errorf("model is %q, want the provider's first, big-model", moved.Model)
	}
}

func TestSetModelRejectsWhatTheProviderDoesNotOffer(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "native", "small-model")

	if _, err := h.engine.SetModel(
		context.Background(), testUser, session.ID, "", "no-such-model"); !errors.Is(err, llm.ErrNoSuchModel) {
		t.Errorf("error is %v, want ErrNoSuchModel", err)
	}
	if _, err := h.engine.SetModel(
		context.Background(), testUser, session.ID, "no-such-provider", ""); !errors.Is(err, llm.ErrNoSuchProvider) {
		t.Errorf("error is %v, want ErrNoSuchProvider", err)
	}

	// Refused means unchanged, not half applied.
	reread, err := h.engine.Session(context.Background(), testUser, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if reread.Provider != "native" || reread.Model != "small-model" {
		t.Errorf("a refused change left the session on %s/%s", reread.Provider, reread.Model)
	}
}

// TestSetModelHonoursTheAdminAllowList: the picker filters, but the route is what
// enforces. A developer who names a forbidden model in a hand-written request is
// refused here, exactly as CreateSession refuses one.
func TestSetModelHonoursTheAdminAllowList(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "native", "small-model")

	if err := h.admin.SetLimits(context.Background(), testUser, admin.Limits{
		AllowedModels: []string{"small-model"},
	}, "test"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	_, err := h.engine.SetModel(context.Background(), testUser, session.ID, "", "big-model")
	if err == nil {
		t.Fatal("a model outside the user's allow-list was accepted")
	}
	if !strings.Contains(err.Error(), "big-model") {
		t.Errorf("the refusal does not name the model: %v", err)
	}
}

func TestSetModelIsANoOpWhenNothingChanges(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "native", "small-model")

	same, err := h.engine.SetModel(
		context.Background(), testUser, session.ID, "native", "small-model")
	if err != nil {
		t.Fatalf("re-selecting the current entry was refused: %v", err)
	}
	if !same.UpdatedAt.Equal(session.UpdatedAt) {
		t.Error("a no-op change touched UpdatedAt")
	}
}

func TestSetModelIsRefusedWhileAnExchangeRuns(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "native", "small-model")

	// begin registers the exchange the way start does, which is what Attach reports.
	exchange := h.engine.begin(testUser, session.ID)
	defer h.engine.finish(exchange)

	_, err := h.engine.SetModel(context.Background(), testUser, session.ID, "", "big-model")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error is %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestSetModelRefusesAnotherUsersSession(t *testing.T) {
	h := newTwoProviderHarness(t)
	session := h.session(t, "native", "small-model")

	if _, err := h.engine.SetModel(
		context.Background(), "sub-mallory", session.ID, "", "big-model"); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("error is %v, want ErrNoSuchSession", err)
	}
}

// TestSetModelCarriesAnOutOfBandHistoryToANativeProvider is the risk the change
// actually runs: a CLI session ran its tool loop over MCP, so its stored history
// holds tool_use blocks ODE never dispatched. Moved to a native provider, that
// history has to arrive as something the protocol accepts — every tool_use
// answered — or the session is permanently unusable on the other side.
func TestSetModelCarriesAnOutOfBandHistoryToANativeProvider(t *testing.T) {
	h := newTwoProviderHarness(t, []llm.Event{
		llm.ToolCallEvent(llm.ToolCall{
			ID: "call-1", Name: "l0_tool", Input: json.RawMessage(`{}`),
		}),
		llm.TextEvent("read it over MCP"),
		llm.DoneEvent("end_turn", llm.Usage{InputTokens: 5, OutputTokens: 5, Provider: "cli"}),
	})
	session := h.session(t, "cli", "")

	exchange, err := h.engine.Send(
		context.Background(), StaticToken("t"), testUser, session.ID, "have a look")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, exchange)

	if _, err := h.engine.SetModel(
		context.Background(), testUser, session.ID, "native", "big-model"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	stored, err := h.engine.Messages(context.Background(), testUser, session.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	replayed := conversation(stored)

	answered := map[string]bool{}
	for _, message := range replayed {
		for _, block := range message.Content {
			if block.Type == llm.ContentToolResult {
				answered[block.ToolUseID] = true
			}
		}
	}
	calls := 0
	for _, message := range replayed {
		for _, block := range message.Content {
			if block.Type != llm.ContentToolUse {
				continue
			}
			calls++
			if !answered[block.ToolUseID] {
				t.Errorf("tool_use %q reaches the new provider unanswered; "+
					"both native protocols reject that with a 400", block.ToolUseID)
			}
		}
	}
	if calls == 0 {
		t.Fatal("the out-of-band turn stored no tool_use, so this proves nothing")
	}
}
