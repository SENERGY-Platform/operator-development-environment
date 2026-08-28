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
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// What these are about: a confirmed tool reached from a provider that runs its own
// tool loop (§5.7's CLI, over MCP) must go through the same D11 decision as one
// reached from the engine's loop — and must not be refused for want of a place to
// ask, which is what used to happen and made four tools unreachable there.

// blockingProvider keeps a turn alive until the test lets it end, so a hold has a
// live exchange to publish its request on — which is exactly the situation an
// out-of-band provider creates while its own tool loop runs.
type blockingProvider struct {
	name    string
	release chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{name: "blocking", release: make(chan struct{})}
}

func (p *blockingProvider) Name() string { return p.name }

func (p *blockingProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		Tools: true, ToolsOutOfBand: true, Streaming: true, System: true,
		Models: []string{"fake-model"},
	}
}

func (p *blockingProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	out := make(chan llm.Event, 4)
	go func() {
		defer close(out)
		out <- llm.TextEvent("working")
		select {
		case <-p.release:
		case <-ctx.Done():
		}
		out <- llm.DoneEvent("end_turn", llm.Usage{
			InputTokens: 1, OutputTokens: 1, Provider: p.name, Model: "fake-model",
		})
	}()
	return out, nil
}

// heldHarness is the engine with a turn running on it, which is the precondition
// for every hold.
type heldHarness struct {
	*harness
	provider *blockingProvider
	session  Session
	exchange *Exchange
}

func newHeldHarness(t *testing.T, tier tools.Tier) *heldHarness {
	t.Helper()

	base := newHarness(t)
	// The ceiling defaults below L2, and a session clamped on read would make the
	// tier assertions below test the clamp rather than the hold.
	if err := base.admin.SetLimits(context.Background(), admin.GlobalSubject, admin.Limits{
		MaxTier: tierPtr(tools.L2),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	provider := newBlockingProvider()
	providers, err := llm.NewRegistry(provider)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	base.engine.providers = providers

	session, err := base.engine.CreateSession(context.Background(), testUser, CreateRequest{
		Tier: tier, Provider: provider.Name(), Model: "fake-model",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	exchange, err := base.engine.Send(context.Background(), StaticToken(testToken), testUser,
		session.ID, "do the thing")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	held := &heldHarness{harness: base, provider: provider, session: session, exchange: exchange}
	t.Cleanup(held.stop)
	return held
}

// stop lets the blocked turn finish. Safe to call twice, because a test that ends
// the turn on purpose still has the cleanup behind it.
func (h *heldHarness) stop() {
	select {
	case <-h.provider.release:
	default:
		close(h.provider.release)
	}
	select {
	case <-h.exchange.Done():
	case <-time.After(20 * time.Second):
	}
}

// hold runs Hold on its own goroutine and hands back the channel its result will
// arrive on, because the call blocks until somebody decides.
func (h *heldHarness) hold(tier tools.Tier, tool string) <-chan tools.Result {
	done := make(chan tools.Result, 1)
	go func() {
		result, held, err := h.engine.Hold(context.Background(), tools.Request{
			Token: testToken, UserSub: testUser, SessionID: h.session.ID, Tier: tier,
		}, tools.Call{ID: "call-1", Name: tool, Input: json.RawMessage(`{}`)})
		if !held || err != nil {
			done <- tools.Result{Outcome: tools.Outcome("not_held")}
			return
		}
		done <- result
	}()
	return done
}

// awaitConfirmation waits for the request to reach the developer, which is what a
// test has to see before it can answer on their behalf.
func (h *heldHarness) awaitConfirmation(t *testing.T) string {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		pending, err := h.engine.PendingConfirmations(context.Background(), testUser, h.session.ID)
		if err != nil {
			t.Fatalf("PendingConfirmations: %v", err)
		}
		for _, confirmation := range pending {
			if confirmation.OutOfBand {
				return confirmation.ID
			}
		}
		select {
		case <-deadline:
			t.Fatal("no held confirmation appeared within 20s")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func awaitResult(t *testing.T, results <-chan tools.Result) tools.Result {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(20 * time.Second):
		t.Fatal("the held call did not return within 20s")
		return tools.Result{}
	}
}

// The property the refusal used to make impossible.
func TestAHeldCallRunsWhenTheDeveloperApproves(t *testing.T) {
	h := newHeldHarness(t, tools.L0)
	results := h.hold(tools.L0, "confirmed_tool")

	id := h.awaitConfirmation(t)
	if err := h.engine.Decide(context.Background(), testUser, h.session.ID, id, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	result := awaitResult(t, results)
	if result.Outcome != tools.OutcomeOK {
		t.Errorf("outcome = %q, want %q (result %+v)", result.Outcome, tools.OutcomeOK, result)
	}
	if !h.tracker.was("confirmed_tool") {
		t.Error("an approved call did not run")
	}
}

// A decline is an answer, not a fault: marking it an error teaches the model to
// avoid a tool whose whole purpose is to ask.
func TestAHeldCallDoesNotRunWhenTheDeveloperDeclines(t *testing.T) {
	h := newHeldHarness(t, tools.L0)
	results := h.hold(tools.L0, "confirmed_tool")

	id := h.awaitConfirmation(t)
	if err := h.engine.Decide(context.Background(), testUser, h.session.ID, id, false); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	result := awaitResult(t, results)
	if result.IsError {
		t.Errorf("a decline was reported as an error: %+v", result)
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("a declined call ran anyway")
	}
}

// The tier is re-read at decision time, not taken from the call that proposed it.
// The gap is real: the card can sit on screen while the developer lowers the
// session, and trusting the recorded tier would make a pending confirmation a way
// to run an L2 tool at L0.
func TestAHeldCallReReadsTheTierWhenItIsApproved(t *testing.T) {
	h := newHeldHarness(t, tools.L2)
	results := h.hold(tools.L2, "confirmed_l2_tool")

	id := h.awaitConfirmation(t)
	if _, err := h.engine.SetTier(context.Background(), testUser, h.session.ID, tools.L0); err != nil {
		t.Fatalf("SetTier: %v", err)
	}
	if err := h.engine.Decide(context.Background(), testUser, h.session.ID, id, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	result := awaitResult(t, results)
	if result.Outcome != tools.OutcomeBlockedByTier {
		t.Errorf("outcome = %q, want %q — the tier at approval is what counts",
			result.Outcome, tools.OutcomeBlockedByTier)
	}
	if h.tracker.was("confirmed_l2_tool") {
		t.Fatal("an L2 tool ran after the session was lowered to L0")
	}
}

// With no turn in flight there is nobody to ask, so the call is refused rather
// than left waiting for an answer that cannot arrive.
func TestAConfirmedCallIsNotHeldWithNoTurnInFlight(t *testing.T) {
	h := newHarness(t)
	session := h.session(t, tools.L0)

	_, held, err := h.engine.Hold(context.Background(), tools.Request{
		Token: testToken, UserSub: testUser, SessionID: session.ID, Tier: tools.L0,
	}, tools.Call{ID: "call-1", Name: "confirmed_tool", Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if held {
		t.Error("a call was held on a session with no exchange to ask on")
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("a confirmed tool ran with nobody asked")
	}
}

// The gates come before the hold. A tool the tier forbids is refused by the
// dispatcher exactly as it is in the engine's own loop, rather than being put to
// the developer as something they could approve their way past.
func TestAHeldCallIsStillSubjectToTheTierGate(t *testing.T) {
	h := newHeldHarness(t, tools.L0)

	result, held, err := h.engine.Hold(context.Background(), tools.Request{
		Token: testToken, UserSub: testUser, SessionID: h.session.ID, Tier: tools.L0,
	}, tools.Call{ID: "call-1", Name: "confirmed_l2_tool", Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !held {
		t.Fatal("the call was not answered at all")
	}
	if result.Outcome != tools.OutcomeBlockedByTier {
		t.Errorf("outcome = %q, want %q", result.Outcome, tools.OutcomeBlockedByTier)
	}

	pending, err := h.engine.PendingConfirmations(context.Background(), testUser, h.session.ID)
	if err != nil {
		t.Fatalf("PendingConfirmations: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a tier refusal was put to the developer as a confirmation: %+v", pending)
	}
}

// A turn that ends takes its held calls with it. Leaving the confirmation pending
// would leave the developer an approve button for a call that has gone.
func TestAHeldCallEndsWithTheTurnThatWasHoldingIt(t *testing.T) {
	h := newHeldHarness(t, tools.L0)
	results := h.hold(tools.L0, "confirmed_tool")

	h.awaitConfirmation(t)
	h.stop()

	result := awaitResult(t, results)
	if !result.IsError {
		t.Errorf("an abandoned call was not reported as failed: %+v", result)
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("an unanswered call ran anyway")
	}

	pending, err := h.engine.PendingConfirmations(context.Background(), testUser, h.session.ID)
	if err != nil {
		t.Fatalf("PendingConfirmations: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("the abandoned confirmation is still pending: %+v", pending)
	}
}

// The wait is bounded. A developer who never answers must not pin the call, the
// goroutine and the provider's tool slot until the turn's own ceiling.
func TestAHeldCallGivesUpWhenNobodyDecides(t *testing.T) {
	h := newHeldHarness(t, tools.L0)
	h.engine.opts.ConfirmationTimeout = 50 * time.Millisecond

	result := awaitResult(t, h.hold(tools.L0, "confirmed_tool"))
	if !result.IsError {
		t.Errorf("an unanswered call was not reported as failed: %+v", result)
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("an unanswered call ran anyway")
	}
}

// Confirm and Decide are not interchangeable. Confirm would dispatch the tool a
// second time and start a turn beside the one still streaming, so a held call is
// refused there and answered where it waits.
func TestConfirmRefusesACallThatIsBeingHeld(t *testing.T) {
	h := newHeldHarness(t, tools.L0)
	results := h.hold(tools.L0, "confirmed_tool")
	id := h.awaitConfirmation(t)

	_, err := h.engine.Confirm(context.Background(), StaticToken(testToken), testUser,
		h.session.ID, id, true)
	if !errors.Is(err, ErrHeldOutOfBand) {
		t.Errorf("Confirm error = %v, want ErrHeldOutOfBand", err)
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("Confirm ran a call that was being held elsewhere")
	}

	// And the hold is still answerable, which is the point of refusing rather than
	// resolving it.
	if err := h.engine.Decide(context.Background(), testUser, h.session.ID, id, true); err != nil {
		t.Fatalf("Decide after a refused Confirm: %v", err)
	}
	if result := awaitResult(t, results); result.Outcome != tools.OutcomeOK {
		t.Errorf("outcome = %q, want %q", result.Outcome, tools.OutcomeOK)
	}
}

// A decision is answered once. The second attempt finds nothing waiting, which is
// what stops a double-click from running an approved tool twice.
func TestASecondDecisionFindsNothingHolding(t *testing.T) {
	h := newHeldHarness(t, tools.L0)
	results := h.hold(tools.L0, "confirmed_tool")
	id := h.awaitConfirmation(t)

	if err := h.engine.Decide(context.Background(), testUser, h.session.ID, id, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	awaitResult(t, results)

	err := h.engine.Decide(context.Background(), testUser, h.session.ID, id, true)
	if !errors.Is(err, ErrNotHeld) {
		t.Errorf("second Decide error = %v, want ErrNotHeld", err)
	}
}

// A confirmation id must not be enough to answer a call held for someone else.
func TestOnlyTheOwnerCanDecideAHeldCall(t *testing.T) {
	h := newHeldHarness(t, tools.L0)
	h.hold(tools.L0, "confirmed_tool")
	id := h.awaitConfirmation(t)

	err := h.engine.Decide(context.Background(), "sub-mallory", h.session.ID, id, true)
	if err == nil {
		t.Fatal("another user answered a held call")
	}
	if h.tracker.was("confirmed_tool") {
		t.Fatal("another user's approval ran the tool")
	}
}

