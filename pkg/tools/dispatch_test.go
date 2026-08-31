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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixtures ---

type sequentialIDs struct {
	mux sync.Mutex
	n   int
}

func (s *sequentialIDs) NewID() string {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.n++
	return "conf-" + itoa(s.n)
}

func itoa(n int) string { return string(rune('0' + n)) }

type recordingAudit struct {
	mux     sync.Mutex
	records []ToolCallRecord
}

func (r *recordingAudit) RecordToolCall(_ context.Context, entry ToolCallRecord) {
	r.mux.Lock()
	defer r.mux.Unlock()
	r.records = append(r.records, entry)
}

// ran tracks whether an executor was reached. Every tier test asserts on this
// rather than only on the returned content: the guarantee of §3.2 is that the
// tool does not *execute*, and a refusal returned after the platform had already
// been read would satisfy a content assertion while breaking the property.
type ran struct {
	mux    sync.Mutex
	called []string
}

func (r *ran) executor(name string) Executor {
	return func(_ context.Context, _ Request) (any, error) {
		r.mux.Lock()
		defer r.mux.Unlock()
		r.called = append(r.called, name)
		return map[string]any{"ok": name}, nil
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

const emptySchema = `{"type":"object"}`

// testSurface is one tool per tier plus a confirmed one, which is the smallest
// set that exercises every gate.
func testSurface(t *testing.T, tracker *ran) (*Registry, *Dispatcher, *recordingAudit) {
	t.Helper()
	registry, err := NewRegistry(
		Definition{Name: "l0_tool", Description: "d", MinTier: L0,
			Schema: json.RawMessage(emptySchema), executor: tracker.executor("l0_tool")},
		Definition{Name: "l1_tool", Description: "d", MinTier: L1,
			Schema: json.RawMessage(emptySchema), executor: tracker.executor("l1_tool")},
		Definition{Name: "l2_tool", Description: "d", MinTier: L2,
			Schema: json.RawMessage(emptySchema), executor: tracker.executor("l2_tool")},
		Definition{Name: "confirmed_tool", Description: "d", MinTier: L0, Confirm: true,
			Schema: json.RawMessage(emptySchema), executor: tracker.executor("confirmed_tool")},
		// A confirmed tool that can recognise its own input, the way run_code does.
		// "dull" is recognised; anything else is not.
		Definition{Name: "recognising_tool", Description: "d", MinTier: L0, Confirm: true,
			Schema: json.RawMessage(emptySchema), executor: tracker.executor("recognising_tool"),
			AutoApprove: func(input json.RawMessage) (bool, string) {
				return string(input) == `{"code":"dull"}`, "it is not the dull one"
			}},
		Definition{Name: "future_tool", Description: "d", MinTier: L0,
			Schema: json.RawMessage(emptySchema), Unavailable: "requires a thing this deployment has not got"},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	audit := &recordingAudit{}
	dispatcher, err := NewDispatcher(registry, audit, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return registry, dispatcher, audit
}

func call(name string) Call { return Call{ID: "call-1", Name: name} }

func request(tier Tier) Request {
	return Request{Token: "Bearer t", UserSub: "sub-1", SessionID: "sess-1", Tier: tier}
}

// --- the exit criterion: L0 provably blocks value-bearing tools ---

func TestTierGate(t *testing.T) {
	cases := []struct {
		name    string
		tier    Tier
		tool    string
		outcome Outcome
		// executes says whether the executor should have been reached at all.
		executes bool
	}{
		{"L0 permits an L0 tool", L0, "l0_tool", OutcomeOK, true},
		{"L0 blocks an L1 tool", L0, "l1_tool", OutcomeBlockedByTier, false},
		{"L0 blocks an L2 tool", L0, "l2_tool", OutcomeBlockedByTier, false},
		{"L1 permits an L1 tool", L1, "l1_tool", OutcomeOK, true},
		{"L1 still blocks an L2 tool", L1, "l2_tool", OutcomeBlockedByTier, false},
		{"L2 permits everything", L2, "l2_tool", OutcomeOK, true},
		{"L2 permits a lower tier too", L2, "l0_tool", OutcomeOK, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := &ran{}
			_, dispatcher, _ := testSurface(t, tracker)

			result := dispatcher.Dispatch(context.Background(), request(tc.tier), call(tc.tool))

			if result.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q", result.Outcome, tc.outcome)
			}
			if got := tracker.was(tc.tool); got != tc.executes {
				t.Errorf("executor reached = %v, want %v — the tier gate must precede execution",
					got, tc.executes)
			}
		})
	}
}

// TestTierRefusalShape pins §3.2's wire shape. The LLM is expected to relay these
// two fields, so a rename is a spec break rather than a refactor.
func TestTierRefusalShape(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	result := dispatcher.Dispatch(context.Background(), request(L0), call("l1_tool"))

	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatalf("marshal refusal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal refusal: %v", err)
	}

	if decoded["blocked_by_tier"] != "L0" {
		t.Errorf(`blocked_by_tier = %v, want "L0"`, decoded["blocked_by_tier"])
	}
	if decoded["required"] != "L1" {
		t.Errorf(`required = %v, want "L1"`, decoded["required"])
	}
	if !result.IsError {
		t.Error("a tier refusal must be marked as an error result for the provider")
	}
}

// TestDeniedToolsHaveNoTool is §5.8's "Denied, enforced server-side — no tool
// exists". The guarantee is absence, so the test asserts absence: both that the
// real surface does not register one, and that the constructor would refuse.
func TestDeniedToolsHaveNoTool(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}

	for name, reason := range Denied() {
		if _, found := registry.Lookup(name); found {
			t.Errorf("%q is registered as a tool, but §5.8 denies it: %s", name, reason)
		}

		_, err := NewRegistry(Definition{
			Name: name, Description: "d", MinTier: L0,
			Schema: json.RawMessage(emptySchema), executor: func(context.Context, Request) (any, error) {
				return nil, nil
			},
		})
		if err == nil {
			t.Errorf("NewRegistry accepted the denied tool %q; it must refuse", name)
		}
	}
}

// TestDeniedToolLooksUnknown checks that a denied name is refused as unknown
// rather than as forbidden. Naming it forbidden describes the boundary and
// invites the model to argue with it.
func TestDeniedToolLooksUnknown(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	for name := range Denied() {
		result := dispatcher.Dispatch(context.Background(), request(L2), call(name))
		if result.Outcome != OutcomeUnknownTool {
			t.Errorf("%q: outcome = %q, want %q", name, result.Outcome, OutcomeUnknownTool)
		}
		encoded, _ := json.Marshal(result.Content)
		if strings.Contains(strings.ToLower(string(encoded)), "denied") ||
			strings.Contains(strings.ToLower(string(encoded)), "forbidden") {
			t.Errorf("%q: refusal describes the capability boundary: %s", name, encoded)
		}
	}
}

// --- confirmation (D11) ---

func TestConfirmedToolDoesNotExecuteUntilConfirmed(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	result := dispatcher.Dispatch(context.Background(), request(L0), call("confirmed_tool"))

	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeAwaitingConfirmation)
	}
	if tracker.was("confirmed_tool") {
		t.Fatal("a tool needing confirmation ran on the model's word alone")
	}
	if result.Confirmation == nil {
		t.Fatal("no pending confirmation returned")
	}
	if result.IsError {
		t.Error("awaiting confirmation is not an error; marking it one teaches the model to avoid the tool")
	}

	confirmed := dispatcher.Confirm(context.Background(), request(L0), *result.Confirmation)
	if confirmed.Outcome != OutcomeOK {
		t.Fatalf("after confirmation: outcome = %q, want %q", confirmed.Outcome, OutcomeOK)
	}
	if !tracker.was("confirmed_tool") {
		t.Error("the confirmed tool never ran")
	}
}

// TestConfirmationRechecksTier is the subtle one: a developer may propose at a
// high tier, lower it, and only then confirm. Trusting the tier recorded on the
// pending confirmation would make confirmation a way to run an L2 tool at L0.
func TestConfirmationRechecksTier(t *testing.T) {
	tracker := &ran{}
	registry, err := NewRegistry(
		Definition{Name: "confirmed_l2", Description: "d", MinTier: L2, Confirm: true,
			Schema: json.RawMessage(emptySchema), executor: tracker.executor("confirmed_l2")},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	// Proposed while the session was at L2.
	proposed := dispatcher.Dispatch(context.Background(), request(L2), call("confirmed_l2"))
	if proposed.Outcome != OutcomeAwaitingConfirmation {
		t.Fatalf("outcome = %q, want %q", proposed.Outcome, OutcomeAwaitingConfirmation)
	}
	if proposed.Confirmation.Tier != L2 {
		t.Errorf("recorded tier = %v, want L2", proposed.Confirmation.Tier)
	}

	// The developer lowers the tier before confirming.
	result := dispatcher.Confirm(context.Background(), request(L0), *proposed.Confirmation)

	if result.Outcome != OutcomeBlockedByTier {
		t.Errorf("outcome = %q, want %q — a lowered tier must revoke the authorisation",
			result.Outcome, OutcomeBlockedByTier)
	}
	if tracker.was("confirmed_l2") {
		t.Error("an L2 tool ran at L0 through a stale confirmation")
	}
}

// --- advertising ---

func TestAvailableExcludesHigherTiersAndUnbuiltTools(t *testing.T) {
	tracker := &ran{}
	registry, _, _ := testSurface(t, tracker)

	// Named rather than counted: adding a tool to the surface should say which
	// one turned up unexpectedly, not that a number moved.
	available := names(registry.Available(L0))
	wantAvailable := []string{"confirmed_tool", "l0_tool", "recognising_tool"}
	if !slices.Equal(available, wantAvailable) {
		t.Fatalf("L0 advertises %v, want %v", available, wantAvailable)
	}
	for _, name := range available {
		if name == "l1_tool" || name == "l2_tool" {
			t.Errorf("L0 advertises %q, which is above the tier", name)
		}
		if name == "future_tool" {
			t.Error("an unimplemented tool is advertised; it can never succeed")
		}
	}

	beyond := names(registry.Beyond(L0))
	if len(beyond) != 2 {
		t.Errorf("Beyond(L0) = %v, want the L1 and L2 tools so the model can ask for a raise", beyond)
	}

	// The declaration still lists everything: that is the published §5.8 table.
	if got := len(registry.Definitions()); got != 6 {
		t.Errorf("Definitions() = %d entries, want all 6 declared", got)
	}
}

func TestUnimplementedToolRefusesWithItsReason(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	result := dispatcher.Dispatch(context.Background(), request(L0), call("future_tool"))

	if result.Outcome != OutcomeNotImplemented {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeNotImplemented)
	}
	refusal, ok := result.Content.(NotImplemented)
	if !ok {
		t.Fatalf("content is %T, want NotImplemented", result.Content)
	}
	if refusal.Reason != "requires a thing this deployment has not got" {
		t.Errorf("reason = %q, want the definition's own — the refusal has to say why it "+
			"cannot be called", refusal.Reason)
	}
}

// --- registry invariants ---

func TestRegistryRejectsMalformedDefinitions(t *testing.T) {
	valid := func() Executor {
		return func(context.Context, Request) (any, error) { return nil, nil }
	}

	cases := []struct {
		name       string
		definition Definition
	}{
		{"no name", Definition{MinTier: L0, Schema: json.RawMessage(emptySchema), executor: valid()}},
		{"no schema", Definition{Name: "t", MinTier: L0, executor: valid()}},
		{"unparseable schema", Definition{Name: "t", MinTier: L0,
			Schema: json.RawMessage(`{"type":`), executor: valid()}},
		{"invalid tier", Definition{Name: "t", MinTier: Tier(9),
			Schema: json.RawMessage(emptySchema), executor: valid()}},
		{"no executor and no reason", Definition{Name: "t", MinTier: L0,
			Schema: json.RawMessage(emptySchema)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRegistry(tc.definition); err == nil {
				t.Error("NewRegistry accepted it")
			}
		})
	}

	t.Run("duplicate", func(t *testing.T) {
		definition := Definition{Name: "t", MinTier: L0,
			Schema: json.RawMessage(emptySchema), executor: valid()}
		if _, err := NewRegistry(definition, definition); err == nil {
			t.Error("NewRegistry accepted a duplicate name")
		}
	})
}

// TestSurfaceDeclaresTheWholeAllowList checks the published table against §5.8,
// including the tier and confirmation columns. This table is the published
// account of what the surface can do, so a drift here is a drift in that account.
func TestSurfaceDeclaresTheWholeAllowList(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}

	expected := map[string]struct {
		tier    Tier
		confirm bool
	}{
		"search_ontology":            {L0, false},
		"resolve_semantic_selection": {L0, false},
		"list_devices":               {L0, false},
		"get_device_metadata":        {L0, false},
		"list_import_instances":      {L0, false},
		"get_import_type_metadata":   {L0, false},
		// The catalogue, which is a read of what could be deployed rather than a
		// second way to search for data — see §5.8 and the file comment in imports.go.
		"list_import_types":  {L0, false},
		"probe_availability": {L0, false},
		// The export-side probe. §5.8 lists probe_availability for a device, and
		// there is no availability endpoint for an export — so the same question
		// about an export is a second tool rather than a parameter, and it is at L0
		// on the same footing: it counts rows and reads no value.
		"probe_export_data":      {L0, false},
		"estimate_read_cost":     {L0, false},
		"quick_profile":          {L0, false},
		"profile_series":         {L1, false},
		"get_sessions":           {L1, false},
		"propose_related_sets":   {L0, false},
		"relate_series":          {L1, false},
		"preview_series":         {L2, false},
		"render_chart":           {L1, false},
		"propose_data_selection": {L0, true},
		// Confirmed for the reason propose_data_selection is: it produces something
		// the developer deploys, and a wiring nobody agreed to is a pipeline nobody
		// asked for.
		"propose_operator_input": {L0, true},
		// The four that change the platform. Confirmed for the same reason and one
		// more: they are the only tools in ODE whose effect outlives the session, so
		// the developer agreeing to them is the whole of the control.
		"create_import_instance": {L0, true},
		"create_export":          {L0, true},
		"delete_import_instance": {L0, true},
		"delete_export":          {L0, true},
		// The working copy. All three at L0 with no confirmation, and the two reads
		// are weaker than the write beside them: the developer's own code on their own
		// storage, no platform data in it, and no git operation anywhere in the
		// interface behind them.
		"list_files":             {L0, false},
		"read_file":              {L0, false},
		"write_file":             {L0, false},
		"run_code":               {L0, true},
		"launch_experiment":      {L0, true},
		"get_experiment_results": {L0, false},
		// The simulation surface (docs/simulation.md).
		// The reads are structure and sit at L0; get_simulation_state is the one that
		// reads values and sits at L1 for the same reason every other value read does.
		"list_simulations":             {L0, false},
		"get_simulation":               {L0, false},
		"list_simulation_templates":    {L0, false},
		"list_simulation_device_types": {L0, false},
		"list_simulation_datasets":     {L0, false},
		"get_backfill_status":          {L0, false},
		"get_simulation_state":         {L1, false},
		// Confirmed for the reason the four import writes are, plus one of their own:
		// a simulated asset is a device in the device repository, so it is inventory
		// other people's applications see until somebody removes it. backfill_simulation
		// is confirmed because a backfilled row is indistinguishable from a live one
		// once it is in timescale, and the window is what the developer should see
		// before it happens.
		"create_simulation":         {L0, true},
		"add_simulated_asset":       {L0, true},
		"set_channel_source":        {L0, true},
		"set_simulation_context":    {L0, true},
		"delete_simulation":         {L0, true},
		"backfill_simulation":       {L0, true},
		"upload_simulation_dataset": {L0, true},
	}

	declared := registry.Definitions()
	if len(declared) != len(expected) {
		t.Errorf("declared %d tools, the allow-list holds %d", len(declared), len(expected))
	}
	for _, definition := range declared {
		want, listed := expected[definition.Name]
		if !listed {
			t.Errorf("%q is declared but is not in the allow-list", definition.Name)
			continue
		}
		if definition.MinTier != want.tier {
			t.Errorf("%q min tier = %v, §5.8 says %v", definition.Name, definition.MinTier, want.tier)
		}
		if definition.Confirm != want.confirm {
			t.Errorf("%q confirm = %v, §5.8 says %v", definition.Name, definition.Confirm, want.confirm)
		}
		delete(expected, definition.Name)
	}
	for name := range expected {
		t.Errorf("%q is in §5.8 but is not declared", name)
	}
}

// TestSurfaceWithoutDepsAdvertisesNothing checks the degradation path: a
// deployment with no platform services declares the surface but offers no tool,
// rather than registering executors that would panic on the first call.
func TestSurfaceWithoutDepsAdvertisesNothing(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	if got := registry.Available(L2); len(got) != 0 {
		t.Errorf("Available(L2) = %v with no dependencies, want none", names(got))
	}
}

// --- audit ---

func TestEveryDispatchIsAudited(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, audit := testSurface(t, tracker)

	dispatcher.Dispatch(context.Background(), request(L0), call("l0_tool"))
	dispatcher.Dispatch(context.Background(), request(L0), call("l1_tool"))
	dispatcher.Dispatch(context.Background(), request(L0), call("nonexistent"))

	if len(audit.records) != 3 {
		t.Fatalf("audited %d calls, want 3 — a refusal is as much a record as a success",
			len(audit.records))
	}
	outcomes := []Outcome{OutcomeOK, OutcomeBlockedByTier, OutcomeUnknownTool}
	for i, want := range outcomes {
		if audit.records[i].Outcome != want {
			t.Errorf("record %d outcome = %q, want %q", i, audit.records[i].Outcome, want)
		}
		if audit.records[i].UserSub != "sub-1" || audit.records[i].SessionID != "sess-1" {
			t.Errorf("record %d lost its identity: %+v", i, audit.records[i])
		}
	}
}

// --- executor error classification ---

func TestInvalidInputIsDistinguishedFromFailure(t *testing.T) {
	registry, err := NewRegistry(
		Definition{Name: "bad_input", Description: "d", MinTier: L0,
			Schema: json.RawMessage(emptySchema),
			executor: func(context.Context, Request) (any, error) {
				return nil, errors.New("wrapped: " + ErrInvalidInput.Error())
			}},
		Definition{Name: "real_invalid", Description: "d", MinTier: L0,
			Schema: json.RawMessage(emptySchema),
			executor: func(context.Context, Request) (any, error) {
				return nil, errors.New("upstream exploded")
			}},
		Definition{Name: "wrapped_invalid", Description: "d", MinTier: L0,
			Schema: json.RawMessage(emptySchema),
			executor: func(context.Context, Request) (any, error) {
				return nil, errors.Join(ErrInvalidInput, errors.New("device_id is required"))
			}},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if got := dispatcher.Dispatch(context.Background(), request(L0), call("real_invalid")).Outcome; got != OutcomeFailed {
		t.Errorf("a platform failure classified as %q, want %q", got, OutcomeFailed)
	}
	if got := dispatcher.Dispatch(context.Background(), request(L0), call("wrapped_invalid")).Outcome; got != OutcomeInvalidInput {
		t.Errorf("bad model input classified as %q, want %q", got, OutcomeInvalidInput)
	}
}

// --- tier plumbing ---

func TestTierJSONRoundTrip(t *testing.T) {
	for _, tier := range Tiers() {
		encoded, err := json.Marshal(tier)
		if err != nil {
			t.Fatalf("marshal %v: %v", tier, err)
		}
		if want := `"` + tier.String() + `"`; string(encoded) != want {
			t.Errorf("marshal %v = %s, want %s", tier, encoded, want)
		}
		var decoded Tier
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", encoded, err)
		}
		if decoded != tier {
			t.Errorf("round trip: %v became %v", tier, decoded)
		}
	}

	// A bare integer is refused: tiers are named levels, and accepting 1 invites
	// an off-by-one from a caller that thinks they are 1-based.
	var tier Tier
	if err := json.Unmarshal([]byte(`1`), &tier); err == nil {
		t.Error("unmarshalling a bare integer as a tier succeeded")
	}
	if err := json.Unmarshal([]byte(`"L3"`), &tier); err == nil {
		t.Error("unmarshalling L3 succeeded; there are three tiers")
	}
}

func TestTierPermitsIsOrdered(t *testing.T) {
	if !L2.Permits(L0) || !L2.Permits(L1) || !L2.Permits(L2) {
		t.Error("L2 must permit every tier")
	}
	if L0.Permits(L1) || L0.Permits(L2) {
		t.Error("L0 must permit neither L1 nor L2")
	}
	if !L0.Permits(L0) {
		t.Error("a tier must permit its own level")
	}
}

func TestDispatcherRequiresRegistryAndIDs(t *testing.T) {
	if _, err := NewDispatcher(nil, nil, &sequentialIDs{}); err == nil {
		t.Error("NewDispatcher accepted a nil registry")
	}
	registry, _ := NewRegistry()
	if _, err := NewDispatcher(registry, nil, nil); err == nil {
		t.Error("NewDispatcher accepted a nil id source")
	}
}

// deadline guards against a dispatch that blocks: every gate is synchronous and
// none of them may wait on anything.
func TestDispatchDoesNotBlock(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatcher.Dispatch(context.Background(), request(L0), call("l1_tool"))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a tier refusal took longer than two seconds")
	}
}

// --- auto mode ---

/*
Auto mode is a standing answer to one tool's question, not a waiver of the rule.

Three properties, and the middle one is the one that would be dangerous to get
wrong: a session that asked for it skips the prompt on input the tool recognises;
a confirmed tool with no predicate is untouched however the session is configured;
and unrecognised input is still confirmed. The second is what makes it safe to
have at all — `create_export` and `delete_import_instance` cannot be swept in by a
setting, because they carry no AutoApprove and nothing can give them one at
runtime.
*/
func TestAutoModeSkipsTheConfirmationOnlyForRecognisedInput(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	auto := Request{UserSub: "u", SessionID: "s", Tier: L0, AutoRun: true}

	result := dispatcher.Dispatch(context.Background(), auto,
		Call{ID: "c1", Name: "recognising_tool", Input: json.RawMessage(`{"code":"dull"}`)})
	if result.Outcome != OutcomeOK {
		t.Errorf("recognised input was not run: outcome = %q", result.Outcome)
	}
	if !tracker.was("recognising_tool") {
		t.Error("recognised input did not reach the executor")
	}

	// Not recognised: the developer is still asked, which is the whole of the
	// safety story here — the subset is small and everything outside it prompts.
	result = dispatcher.Dispatch(context.Background(), auto,
		Call{ID: "c2", Name: "recognising_tool", Input: json.RawMessage(`{"code":"import os"}`)})
	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Errorf("unrecognised input skipped the confirmation: outcome = %q", result.Outcome)
	}
}

// The property that bounds the blast radius of the whole feature.
func TestAutoModeCannotWaiveAToolThatHasNoPredicate(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	result := dispatcher.Dispatch(context.Background(),
		Request{UserSub: "u", SessionID: "s", Tier: L0, AutoRun: true},
		Call{ID: "c1", Name: "confirmed_tool", Input: json.RawMessage(`{}`)})

	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Errorf("auto mode waived a tool with no way to recognise its input: %q", result.Outcome)
	}
	if tracker.was("confirmed_tool") {
		t.Fatal("a tool with no predicate ran without the developer being asked")
	}
}

// And a session that did not ask for it is unaffected, predicate or not.
func TestWithoutAutoModeEvenRecognisedInputIsConfirmed(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	result := dispatcher.Dispatch(context.Background(),
		Request{UserSub: "u", SessionID: "s", Tier: L0},
		Call{ID: "c1", Name: "recognising_tool", Input: json.RawMessage(`{"code":"dull"}`)})

	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Errorf("a session that did not ask for auto mode got it anyway: %q", result.Outcome)
	}
	if tracker.was("recognising_tool") {
		t.Fatal("recognised input ran in a session with auto mode off")
	}
}
