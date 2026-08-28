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
	"errors"
	"log/slog"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// Holding a confirmed tool call open for a transport that cannot ask for a
// decision itself.
//
// The case is the CLI provider (§5.7). It runs its own tool loop over ODE's MCP
// endpoint, so a confirmed tool arrives here as an HTTP request rather than as a
// call in the engine's own loop — and the engine's loop is where D11's pause
// lives. Refusing it, which is what this used to do, made `run_code`,
// `propose_operator_input`, `propose_data_selection` and `launch_experiment`
// permanently unreachable on that provider, and told the developer to run them
// "from the ODE interface", which dispatches no tool by hand.
//
// What makes holding correct rather than a workaround is where the waiting
// happens. The CLI is not asked to surface a decision; it sees one slow tool call.
// ODE waits, because ODE is what already has the pending-confirmation store, the
// live Exchange the developer is watching, and Dispatcher.Confirm.
//
// A hold is therefore a wait *inside a running exchange*, which is what makes it a
// different shape from Confirm: Confirm refuses to run while an exchange is live
// and starts one of its own, because the native providers pause their turn and
// resume it from the developer's answer. Nothing here resumes anything — the CLI's
// loop never stopped.

// ErrHeldOutOfBand is returned by Confirm for a confirmation a transport is
// holding open. Answering it starts no turn, so it goes through Decide.
var ErrHeldOutOfBand = errors.New("chat: this confirmation is held open by the " +
	"provider's own tool call and is answered in place")

// ErrNotHeld is returned by Decide for a confirmation nothing is waiting on.
// Phrased for the developer, because it is what the SPA shows them: the usual
// cause is that the call they are deciding about gave up waiting.
var ErrNotHeld = errors.New("the call waiting for this decision has ended, so " +
	"nothing ran; ask again if you still want it")

// defaultConfirmationTimeout bounds how long a held call waits for the developer.
//
// It has to stay comfortably below the CLI provider's own turn timeout: the turn
// dying underneath a hold would leave the developer approving something whose
// caller has gone.
const defaultConfirmationTimeout = 5 * time.Minute

// confirmationCallMargin is how much longer the provider is told one tool call
// may take than ODE will actually hold it. It exists so the wait ends on ODE's
// timer, which knows it was a confirmation nobody answered, rather than on the
// provider's, which only knows a tool call did not come back.
const confirmationCallMargin = 30 * time.Second

// hold is one call waiting for a decision.
type hold struct {
	// decided carries the developer's answer. Buffered, so Decide never blocks on
	// a waiter that has already given up on the timer.
	decided chan bool
	// sessionID is checked in Decide: a confirmation id alone must not be enough to
	// answer a hold belonging to another conversation.
	sessionID string
	userSub   string
}

// Hold dispatches a call that needs the developer's confirmation and waits for it.
//
// The bool reports whether the call was held at all. False means nobody would ever
// have been asked — there is no exchange in flight to show the request on, or it
// could not be recorded — and the caller should refuse rather than wait, because a
// wait that nothing can end is worse than an honest refusal.
//
// Note what is *not* done here: nothing is persisted into the conversation and no
// tool result is published. The CLI reports both its own tool_use and its result on
// its event stream, and run stores them at the end of the turn
// (see outOfBandResultMessage). Publishing here would show the developer the same
// result twice.
func (e *Engine) Hold(ctx context.Context, req tools.Request, call tools.Call) (tools.Result, bool, error) {
	exchange, running := e.Attach(req.SessionID)
	if !running {
		return tools.Result{}, false, nil
	}

	// Through Dispatch, not around it. Every gate of §3.2 precedes the hold, so a
	// tool the tier forbids is refused here exactly as it is in the engine's own
	// loop, and the audit trail records the same two entries.
	result := e.dispatcher.Dispatch(ctx, req, call)
	if result.Outcome != tools.OutcomeAwaitingConfirmation || result.Confirmation == nil {
		return result, true, nil
	}

	confirmation := Confirmation{
		PendingConfirmation: *result.Confirmation,
		UserSub:             req.UserSub,
	}
	if err := e.store.PutConfirmation(ctx, confirmation); err != nil {
		// The same reasoning as the native path: a confirmation that was not
		// recorded is one the developer will never be asked about, so waiting for
		// their answer would wait for ever. The caller refuses instead.
		return tools.Result{}, false, err
	}

	waiting := &hold{
		decided:   make(chan bool, 1),
		sessionID: req.SessionID,
		userSub:   req.UserSub,
	}
	// Registered before the state is published, so nothing can observe "waiting"
	// on a hold that was refused.
	if !e.beginHold(confirmation.ID, waiting) {
		return tools.Result{}, false, errors.New("chat: this confirmation is already held")
	}
	// Re-derived rather than assumed on the way out: this hold may have ended
	// because the developer decided — the turn then carries on — or because the
	// turn itself is over. Deferred before endHold so it runs after it and reads
	// the registry without this hold in it.
	defer func() {
		e.publishActivity(req.UserSub, req.SessionID, e.stateOf(req.UserSub, req.SessionID))
	}()
	defer e.endHold(confirmation.ID)
	// A held call keeps its exchange running, so without this the panel would show
	// a conversation as busy for exactly as long as it sat waiting for an answer.
	e.publishActivity(req.UserSub, req.SessionID, ActivityWaiting)

	// OutOfBand is set on the copy the developer sees, not on the stored row — see
	// the field's own comment.
	described := confirmation
	described.OutOfBand = true
	exchange.publish(Event{Type: EventConfirmation, Confirmation: described.Describe()})

	timeout := e.opts.ConfirmationTimeout
	if timeout <= 0 {
		timeout = defaultConfirmationTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// giveUp settles the three ways a hold can end without a decision.
	//
	// It claims the waiter first, and that is not tidiness. Decide removes the
	// waiter under the same lock before it sends, so losing the claim means a
	// decision is already on its way — and returning "nobody answered" then would
	// tell the model one thing while the developer had been told another. Whoever
	// takes the waiter owns the outcome.
	giveUp := func(reason, told string) (tools.Result, bool, error) {
		if !e.claimHold(confirmation.ID) {
			return e.resolveHold(ctx, req, confirmation, exchange, <-waiting.decided), true, nil
		}
		e.abandonHold(ctx, exchange, confirmation, reason)
		return heldFailure(confirmation, told), true, nil
	}

	select {
	case approve := <-waiting.decided:
		return e.resolveHold(ctx, req, confirmation, exchange, approve), true, nil

	case <-exchange.Done():
		// The turn was abandoned — the developer pressed stop, or the provider's own
		// timeout ended it. Whatever answer this returns is going nowhere, but the
		// confirmation must not be left pending or its card would outlive the turn
		// it belongs to.
		return giveUp("the turn ended before the developer decided",
			"the exchange ended before the developer decided, so this did not run")

	case <-timer.C:
		return giveUp("the request timed out",
			"the developer did not decide within "+timeout.String()+", so this did not run")

	case <-ctx.Done():
		// The provider's tool call has gone. Nothing will read the answer, so the
		// confirmation is retired rather than left for a developer to approve into
		// a caller that no longer exists.
		result, held, _ := giveUp("the call that asked for it was cancelled",
			"the call that needed this decision was cancelled, so it did not run")
		if !held {
			return tools.Result{}, false, ctx.Err()
		}
		return result, true, ctx.Err()
	}
}

// resolveHold runs or refuses the held call once the developer has answered.
func (e *Engine) resolveHold(
	ctx context.Context, req tools.Request, confirmation Confirmation,
	exchange *Exchange, approve bool,
) tools.Result {
	now := e.now()
	resolved := confirmation
	resolved.ResolvedAt = &now
	resolved.Decision = DecisionRejected
	if approve {
		resolved.Decision = DecisionApproved
	}
	if err := e.store.PutConfirmation(ctx, resolved); err != nil {
		slog.ErrorContext(ctx, "could not record a confirmation decision",
			"confirmation", confirmation.ID, "error", err)
	}
	publishResolution(exchange, resolved)

	if !approve {
		return tools.Result{
			CallID: confirmation.CallID, Tool: confirmation.Tool,
			Outcome: tools.Outcome("rejected"), IsError: false,
			Content: map[string]any{
				"rejected": true,
				"hint": "the developer declined this. Do not retry it; ask what they would " +
					"prefer instead.",
			},
		}
	}

	// The tier is re-read rather than taken from req, for the reason
	// Dispatcher.Confirm documents: a developer may propose at L2, lower the tier
	// while the card is on screen, and only then approve. req.Tier is what the
	// session was at when the model asked, minutes ago.
	tier := req.Tier
	if current, err := e.TierFor(ctx, req.UserSub, req.SessionID); err == nil {
		tier = current
	} else {
		slog.WarnContext(ctx, "could not re-read the tier for a held call; using the recorded one",
			"session", req.SessionID, "error", err)
	}

	return e.dispatcher.Confirm(ctx, tools.Request{
		Token: req.Token, UserSub: req.UserSub, SessionID: req.SessionID, Tier: tier,
		// Carried from the held call rather than re-read: an approval acts in the
		// workbench the model asked about, even if the developer has since opened
		// another one.
		WorkbenchID: req.WorkbenchID,
		// Progress reaches the developer the same way it does natively, which for an
		// approved run_code or launch_experiment is the difference between a visible
		// multi-minute step and a UI that has gone quiet.
		Report: func(progress tools.Progress) {
			exchange.publish(Event{Type: EventProgress, Progress: &progress})
		},
	}, confirmation.PendingConfirmation)
}

// publishResolution tells every view of an exchange that a confirmation is
// settled, whoever settled it and however.
//
// Deliberately published on all three endings — approved, rejected, and retired
// without an answer — rather than only on the two the developer causes. A card
// that cannot be answered any more is exactly as wrong to leave on screen as one
// that already was, and the reload that used to clear it only ran when the turn
// ended, which a held call does not do.
func publishResolution(exchange *Exchange, confirmation Confirmation) {
	exchange.publish(Event{
		Type: EventConfirmationResolved, Confirmation: confirmation.Describe(),
	})
}

// abandonHold retires a confirmation nobody can answer any more.
//
// Recorded as rejected rather than left pending, because pending is what the SPA
// draws a card for: a hold whose caller has gone would otherwise leave the
// developer an approve button that runs a tool into nothing.
func (e *Engine) abandonHold(
	ctx context.Context, exchange *Exchange, confirmation Confirmation, reason string,
) {
	now := e.now()
	resolved := confirmation
	resolved.ResolvedAt = &now
	resolved.Decision = DecisionRejected
	// Before the store write, and unconditionally. A card whose caller has gone is
	// the one case where the developer is most likely to be looking at a second
	// window, because the usual reason a hold expires is that nobody was at the
	// first one. publish is a no-op once the exchange has closed, which is one of
	// the three ways this is reached.
	publishResolution(exchange, resolved)

	// Detached from ctx: one of the ways this is reached is that very context being
	// cancelled, and the write says nothing about whether the store would take it.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := e.store.PutConfirmation(writeCtx, resolved); err != nil {
		slog.ErrorContext(ctx, "could not retire an abandoned confirmation",
			"confirmation", confirmation.ID, "error", err)
	}
	// The note is why this function is more than a store write.
	//
	// The developer was not there — that is the usual reason a hold expires — so
	// without it the only trace is a tool call marked failed. Reopening the session
	// an hour later shows a refusal with nothing saying who refused it or why, and
	// the confirmation it belonged to is gone from the panel by then too.
	//
	// Appended rather than sent: this is not a question, and starting a turn to have
	// it answered would spend the developer's budget on the word "understood". The
	// next turn reads it as input, the same way a workbench move's note is read.
	notice := abandonedNotice(confirmation, reason)
	notice.SessionID = confirmation.SessionID
	if err := e.store.AppendMessages(writeCtx, confirmation.SessionID, notice); err != nil {
		// Logged, not returned: the call is already refused and the model has already
		// been told. At error rather than warn, because the developer is now the only
		// party with no record of it.
		slog.ErrorContext(ctx, "a held tool call was abandoned without a note in its history",
			"confirmation", confirmation.ID, "session", confirmation.SessionID, "error", err)
	}

	slog.InfoContext(ctx, "a held tool call was abandoned",
		"confirmation", confirmation.ID, "tool", confirmation.Tool, "reason", reason)
}

// abandonedSubjectPrefix marks an abandonment note's subject, for the same reason
// moveSubjectPrefix exists: this field is read elsewhere as a bare experiment id.
const abandonedSubjectPrefix = "abandoned:"

// abandonedNotice is what an unanswered confirmation leaves behind.
//
// In ODE's voice, not the developer's. They did not decline it — they never saw
// it — and a refusal written in their words would put a decision in their mouth
// that nobody made.
func abandonedNotice(confirmation Confirmation, reason string) StoredMessage {
	text := "ODE did not run " + confirmation.Tool + ", because " + reason +
		". It needed the developer's confirmation and never got one, so nothing" +
		" happened: no code ran, nothing was written and no state changed. If it is" +
		" still what you want, ask for it again rather than assuming it took effect."

	return StoredMessage{
		Role:    llm.RoleUser,
		Content: []llm.Content{{Type: llm.ContentText, Text: text}},
		Origin:  OriginODE,
		Subject: abandonedSubjectPrefix + confirmation.ID,
	}
}

// heldFailure is what the model is told when a hold ended without a decision.
func heldFailure(confirmation Confirmation, why string) tools.Result {
	return tools.Result{
		CallID: confirmation.CallID, Tool: confirmation.Tool,
		Outcome: tools.OutcomeFailed, IsError: true,
		Content: map[string]any{
			"error": why,
			"hint": "it did not run. Tell the developer what you were about to do and let " +
				"them ask again; do not retry it by yourself.",
		},
	}
}

// Decide answers a confirmation a transport is holding open.
//
// Distinct from Confirm, and not a variant of it. Confirm resumes a turn that
// paused; this one resolves a call inside a turn that never stopped, so it starts
// nothing and returns no exchange — the events the developer is already watching
// carry the result.
func (e *Engine) Decide(ctx context.Context, sub, sessionID, confirmationID string, approve bool) error {
	if _, err := e.Session(ctx, sub, sessionID); err != nil {
		return err
	}

	e.holdMux.Lock()
	waiting, held := e.holds[confirmationID]
	// Ownership is checked against what the hold recorded, not against the id in
	// the request: a confirmation id must not be enough to answer a call held for
	// another conversation, or another developer. Checked before the claim, so a
	// stranger cannot cancel a hold merely by naming it.
	if held && (waiting.sessionID != sessionID || waiting.userSub != sub) {
		e.holdMux.Unlock()
		return ErrNoSuchConfirmation
	}
	// Removed under the same lock Hold registers under and gives up under, so a
	// second decision — or a timeout racing this one — finds nothing.
	if held {
		delete(e.holds, confirmationID)
	}
	e.holdMux.Unlock()

	if !held {
		return ErrNotHeld
	}

	waiting.decided <- approve
	return nil
}

// beginHold registers a waiter. False means one is already registered, which
// would mean two calls waiting on one confirmation id.
func (e *Engine) beginHold(id string, waiting *hold) bool {
	e.holdMux.Lock()
	defer e.holdMux.Unlock()
	if _, exists := e.holds[id]; exists {
		return false
	}
	e.holds[id] = waiting
	return true
}

// claimHold removes a waiter and reports whether this caller got there first.
// Exactly one of Decide and the hold's own give-up paths can succeed, which is
// what stops a decision and a timeout from both deciding the same call.
func (e *Engine) claimHold(id string) bool {
	e.holdMux.Lock()
	defer e.holdMux.Unlock()
	if _, exists := e.holds[id]; !exists {
		return false
	}
	delete(e.holds, id)
	return true
}

// endHold deregisters a waiter. Safe to call for one Decide has already removed.
func (e *Engine) endHold(id string) {
	e.holdMux.Lock()
	defer e.holdMux.Unlock()
	delete(e.holds, id)
}

// heldOutOfBand reports whether a call is being held open for this confirmation
// right now.
func (e *Engine) heldOutOfBand(id string) bool {
	e.holdMux.Lock()
	defer e.holdMux.Unlock()
	_, held := e.holds[id]
	return held
}
