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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/interpret"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
)

// A reply in the shape the injected message asks for: a reading of the numbers,
// then one concrete adjustment on a marked last line.
const interpretingReply = `The run finished and beat the criterion: rmse came in at 0.31
against your threshold of 0.35, and it is down from 0.42 on the previous run — a 26%
improvement from the wider lookback alone. r2 moved with it, from 0.71 to 0.78.

What is not settled is whether the gain is the lookback or the extra folds, because both
changed between the two runs.

NEXT STEP: hold folds at 5 and raise lookback_days from 180 to 365, so the next run
isolates the window.`

// firstRunReply is what the assistant says about a run with nothing to compare
// against, which is the other half of the pair the acceptance test needs.
const firstRunReply = `This is the first run of this experiment, so there is nothing to
compare it against yet. rmse 0.42 is above your threshold of 0.35.

NEXT STEP: widen lookback_days from 90 to 180 and run again.`

// --- the acceptance criterion of M9 ---

// "A completed run produces an interpretation and a concrete next proposal."
//
// End to end, from a real `git archive` of a real commit to the message the
// developer reads: the run finishes, nobody is watching, the poller notices, the
// summary is built with ODE's own credential, the developer comes back, the turn
// runs on *their* token, and the conversation carries both halves of §5.13's last
// two sentences.
func TestACompletedRunProducesAnInterpretationAndAConcreteNextProposal(t *testing.T) {
	// Two replies, because both runs in this test finish and both belong to the
	// session — §5.13 interprets each of them, which is itself the behaviour.
	h := newHarness(t, firstRunReply, interpretingReply)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the criterion")

	// A first run, so §5.13's comparison has something to compare against.
	first := h.launch()
	h.finish(first, map[string]float64{"rmse": 0.42, "r2": 0.71})
	if _, err := h.experiments.Get(context.Background(), h.request(), first.ID); err != nil {
		t.Fatalf("settle the first run: %v", err)
	}

	h.write("op.py", "# a wider lookback\n")
	h.commit("Widen the lookback")
	second := h.launch()

	// The job ends with nobody connected. Nothing has read its status.
	h.finish(second, map[string]float64{"rmse": 0.31, "r2": 0.78})

	h.poll()
	h.deliver()

	// The summary exists already, built with the service credential.
	messages := h.messages()
	if len(messages) != 0 {
		t.Fatalf("the conversation has %d messages with nobody connected; the "+
			"interpretation turn must not run without a developer's own token", len(messages))
	}

	// The developer comes back.
	defer h.connectDeveloper()()
	h.deliver()

	messages = h.messages()
	if len(messages) < 4 {
		t.Fatalf("the conversation has %d messages, want an injected summary and an "+
			"answer for each of the two runs", len(messages))
	}

	// The summary for the run under test, found by what it is about rather than by
	// position — the conversation carries both runs.
	var injected *chat.StoredMessage
	for index := range messages {
		if messages[index].Injected() && messages[index].Subject == second.ID {
			injected = &messages[index]
		}
	}
	if injected == nil {
		t.Fatalf("no ODE-injected message is about %s; the developer would have no "+
			"way to tell which run a summary belongs to", second.ID)
	}
	if injected.Role != llm.RoleUser {
		t.Errorf("role = %q, want the role a model reads as input", injected.Role)
	}
	for _, message := range messages {
		if message.Injected() && message.Origin != chat.OriginODE {
			t.Errorf("origin = %q, want it marked as ODE's own", message.Origin)
		}
	}

	result, err := h.interpret.Interpretation(context.Background(), h.request(), second.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}

	// An interpretation.
	if result.Pending() {
		t.Fatal("the run is still marked as not interpreted")
	}
	if !strings.Contains(result.Interpretation, "0.31") {
		t.Errorf("interpretation = %q, want the assistant's reading of the run",
			result.Interpretation)
	}

	// And a concrete next proposal.
	if !result.Proposal.Stated() {
		t.Fatalf("no proposal was extracted (%s: %s)",
			result.Proposal.Reason, result.Proposal.Detail)
	}
	if !strings.Contains(result.Proposal.Text, "lookback_days") {
		t.Errorf("proposal = %q, want the concrete adjustment", result.Proposal.Text)
	}
	if result.Proposal.ID == "" {
		t.Error("the proposal has no id, so a decision could not be keyed to it")
	}

	// The summary the assistant read is §5.13's, including the developer's own
	// criterion graded against the run and the comparison to the previous one.
	if !result.Summary.EvaluationCriteria.Met.IsMet() {
		t.Errorf("criterion = %+v, want rmse 0.31 under 0.35 read as met",
			result.Summary.EvaluationCriteria)
	}
	if len(result.Summary.ComparisonToPrevious) == 0 {
		t.Error("the summary compares against nothing, although a previous run finished")
	}

	// What the *model* was actually given, which is a different question from what
	// the route answers with and is the one that decides whether the interpretation
	// is worth anything.
	given := h.injectedText(t)
	for _, want := range []string{`"evaluation_criteria"`, `"comparison_to_previous"`,
		interpret.ProposalMarker} {
		if !strings.Contains(given, want) {
			t.Errorf("the injected message does not carry %q", want)
		}
	}

	// The developer's own criterion, graded, in the document the model read.
	//
	// This is the assertion that matters and the one an earlier version of this test
	// got wrong: it checked `result.Summary`, which the route *recomputes* with the
	// caller's token, so it passed while the model was being handed a summary whose
	// criterion said `no_developer_credential` — a permanently unevaluated criterion
	// beside a pane showing the real verdict.
	if !strings.Contains(given, `"met": true`) {
		t.Errorf("the model was not told the criterion was met. The injected summary "+
			"was:\n%s", given)
	}
	if !strings.Contains(given, `"metric": "rmse"`) {
		t.Error("the model was not told which metric the developer judges the run on")
	}
	if strings.Contains(given, "no_developer_credential") {
		t.Error("the model was given the poller's ungraded summary rather than one " +
			"built with the developer's own token")
	}
	if strings.Contains(given, "logs") && strings.Contains(given, "Traceback") {
		t.Error("a log reached the model's context (§5.13: never raw logs)")
	}
}

// The message ODE injects does not tell the model the run produced no output.
//
// §5.13 keeps logs out of a model's context. It does not make them stop existing:
// the job's driver output is on the developer's own route, which this test reads to
// prove it. The message used to say "there are no logs and there is no tool that
// would fetch them", and a model passed that on to a developer who had the log pane
// open beside the conversation — a false statement about ODE, in ODE's own voice.
func TestTheInjectedMessageDoesNotDenyThatTheRunProducedLogs(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	const output = "Traceback (most recent call last): RuntimeError: the run's own output"
	h.ray.SetLogs(launched.SubmissionID, output)
	h.finish(launched, map[string]float64{"rmse": 0.31})

	h.poll()
	defer h.connectDeveloper()()
	h.deliver()

	given := h.injectedText(t)

	// The constraint §5.13 does state, unchanged: not a character of it in context.
	if strings.Contains(given, output) {
		t.Error("the job's output reached the model's context (§5.13: never raw logs)")
	}

	// And the output exists, where the developer reads it.
	page, err := h.experiments.Logs(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if page.Logs != output {
		t.Fatalf("logs = %q, want the job's own output on the developer's route", page.Logs)
	}

	// So the message must not deny it.
	for _, denial := range []string{"There are no logs", "there are no logs",
		"no logs and there is no tool"} {
		if strings.Contains(given, denial) {
			t.Errorf("the injected message tells the model %q, although the developer's "+
				"own route answers with %d characters of them", denial, len(page.Logs))
		}
	}
	if !strings.Contains(given, "The developer has it") {
		t.Error("the injected message does not say who does have the run's output, so a " +
			"model that cannot explain a run from the numbers has nowhere to point")
	}
}

// A failed run is interpreted from its exception, and the exception is masked at
// the session's own tier (D34).
//
// This is the second of the two paths a summary takes into a model's context, and
// the mask is applied at this boundary — so this test is what fails if the call
// goes missing here while pkg/tools keeps its own.
func TestAFailedRunsExceptionIsInjectedMaskedAtTheSessionsTier(t *testing.T) {
	const output = `loading 43200 rows from urn:infai:ses:export:9f2c1b7e
Traceback (most recent call last):
  File "/tmp/ray/session_1/runtime_resources/working_dir_files/_ray_pkg_9c1/train.py", line 39, in train_once
    model.fit(X, y)
ValueError: Input X contains NaN in column 'power_kw' at 3 of 43200 rows
`

	for _, expect := range []struct {
		tier   exposure.Tier
		masked bool
	}{{exposure.L0, true}, {exposure.L1, true}, {exposure.L2, false}} {
		t.Run(expect.tier.String(), func(t *testing.T) {
			h := newHarness(t, interpretingReply)
			h.ready()
			if _, err := h.chat.SetTier(context.Background(), testUserSub, h.session.ID,
				expect.tier); err != nil {
				t.Fatalf("SetTier: %v", err)
			}

			launched := h.launch()
			h.ray.SetLogs(launched.SubmissionID, output)
			h.mlflow.Finish(h.t, launched.RunID, "FAILED", map[string]float64{})
			h.ray.SetStatus(launched.SubmissionID, experiments.StatusFailed)

			h.poll()
			defer h.connectDeveloper()()
			h.deliver()

			given := h.injectedText(t)

			// The exception reaches the model, because for a failed run it is the whole
			// of what the summary has to say.
			if !strings.Contains(given, "ValueError") ||
				!strings.Contains(given, "Input X contains NaN") {
				t.Fatalf("the injected message carries no exception:\n%s", given)
			}
			// And the frame, which is what makes a next step actionable.
			if !strings.Contains(given, "train.py") || !strings.Contains(given, "39") {
				t.Error("the injected message names no file and line")
			}
			// Never the log itself, at any tier.
			for _, forbidden := range []string{"loading", "urn:infai:ses:export", "Traceback"} {
				if strings.Contains(given, forbidden) {
					t.Errorf("a log line reached the model's context: %q", forbidden)
				}
			}

			carriesValue := strings.Contains(given, "power_kw")
			if expect.masked {
				if carriesValue {
					t.Errorf("a value from the developer's series reached a %s session:\n%s",
						expect.tier, given)
				}
				// And the model is told what the placeholder means, so it does not report
				// `[value]` as the text the exception carried.
				if !strings.Contains(given, "[value]") ||
					!strings.Contains(given, "a value was withheld here") {
					t.Error("the message masks literals without saying that it did")
				}
			} else if !carriesValue {
				t.Errorf("L2 exposes values already, and the message was masked anyway:\n%s",
					given)
			}
		})
	}
}

// A run that failed with no traceback says so, and asks for evidence rather than a
// guess at a cause.
func TestAFailedRunWithNoTracebackAsksForEvidenceRatherThanACause(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.ray.SetLogs(launched.SubmissionID,
		"(raylet) node ran out of memory, killing worker\n")
	h.mlflow.Finish(h.t, launched.RunID, "FAILED", map[string]float64{})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusFailed)

	h.poll()
	defer h.connectDeveloper()()
	h.deliver()

	given := h.injectedText(t)
	if !strings.Contains(given, string(experiments.ReasonNoTraceback)) {
		t.Errorf("the injected message does not say the run left no exception:\n%s", given)
	}
	if !strings.Contains(given, "log pane") {
		t.Error("the model is not told who can read the output it cannot")
	}
	if strings.Contains(given, "raylet") {
		t.Error("a log line reached the model's context")
	}
}

// --- the token, which is the point of the design ---

// A run that finishes while nobody is connected is summarised anyway, and waits.
// The turn runs when the developer returns — not before, and not skipped.
func TestASummaryIsBuiltWithNobodyConnectedAndTheTurnRunsWhenTheyReturn(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})

	h.poll()
	h.deliver()

	// Built: the record is terminal and the summary is available with no developer
	// behind the request at all.
	record, found, err := h.store.Get(context.Background(), testUserSub, launched.ID)
	if err != nil || !found {
		t.Fatalf("stored = %v %v", found, err)
	}
	if !experiments.Terminal(record.Status) {
		t.Errorf("status = %q, want the poller to have settled it", record.Status)
	}
	summary, err := h.experiments.Summarise(context.Background(), record)
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if summary.Metrics["rmse"] != 0.31 {
		t.Errorf("summary = %+v, want the run's metrics", summary.Metrics)
	}
	// And the criterion says why it has no verdict, rather than saying the run failed.
	if summary.EvaluationCriteria.Met.Known() {
		t.Errorf("met = %v, want no verdict: nothing read the developer's file",
			summary.EvaluationCriteria.Met)
	}
	if reason := summary.EvaluationCriteria.Met.Status().Reason; reason !=
		experiments.ReasonNoDeveloperCredential {
		t.Errorf("reason = %q, want no_developer_credential", reason)
	}

	// Waiting: nothing was injected and no turn ran.
	if h.provider.turns() != 0 {
		t.Errorf("the provider was called %d times with nobody connected; an "+
			"interpretation turn dispatches tools on the developer's behalf and must "+
			"not run without their token (§3.1 item 3)", h.provider.turns())
	}
	if len(h.messages()) != 0 {
		t.Error("something was written into the conversation with nobody connected")
	}

	// The interpretation reads as not-yet rather than as absent.
	pendingView, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if !pendingView.Pending() {
		t.Error("the run reports itself as interpreted before any turn ran")
	}
	if pendingView.Proposal.Reason != interpret.ReasonNotInterpreted {
		t.Errorf("reason = %q, want not_interpreted_yet — a turn that has not run is a "+
			"different fact from an assistant that proposed nothing",
			pendingView.Proposal.Reason)
	}

	// The developer returns.
	defer h.connectDeveloper()()
	h.deliver()

	if h.provider.turns() != 1 {
		t.Fatalf("the provider was called %d times after the developer returned, want one",
			h.provider.turns())
	}
	after, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if after.Pending() || !after.Proposal.Stated() {
		t.Errorf("the developer came back to a conversation that skipped the "+
			"interpretation: %+v", after.Proposal)
	}
}

// Connecting nudges the loop rather than waiting for the next tick, so a developer
// who opens the tab does not sit looking at a conversation that has not caught up.
func TestConnectingRunsTheWaitingTurnWithoutWaitingForATick(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.interpret.Start(ctx)

	disconnect := h.connectDeveloper()
	defer disconnect()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.messages()) >= 2 {
			cancel()
			select {
			case <-h.interpret.Stopped():
			case <-time.After(5 * time.Second):
				t.Fatal("the delivery loop did not return after its context ended")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing was delivered after the developer connected")
}

// --- an automated turn is still a turn ---

// A session already running an exchange refuses the automated turn, and the run
// stays pending rather than being wedged into the middle of the conversation.
//
// The shape matters as much as the refusal: appending a summary between an
// assistant's tool call and its result is a protocol error both native APIs
// reject, so nothing may be stored when the turn cannot start.
func TestAnAutomatedTurnIsRefusedWhileTheSessionIsBusyAndIsRetried(t *testing.T) {
	h := newHarness(t, "thinking about it", interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()

	// The developer is mid-turn. blocking holds the exchange open until released.
	release := make(chan struct{})
	h.provider.block(release)

	defer h.connectDeveloper()()
	exchange, err := h.chat.Send(context.Background(), chat.StaticToken(unsignedToken()),
		testUserSub, h.session.ID, "how is it going?")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	h.deliver()

	// Nothing was injected into a conversation that is mid-turn.
	for _, message := range h.messages() {
		if message.Injected() {
			t.Fatal("a summary was appended while an exchange was running; that leaves " +
				"the history in a shape both native tool protocols reject")
		}
	}

	close(release)
	select {
	case <-exchange.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the developer's own turn did not finish")
	}

	// Retried, and delivered.
	h.deliver()
	found := false
	for _, message := range h.messages() {
		if message.Injected() && message.Subject == launched.ID {
			found = true
		}
	}
	if !found {
		t.Error("the run was dropped rather than retried once the session was free")
	}
}

// The §3.3 spend cap is checked on an automated turn exactly as on a typed one.
// An automated turn is accounted, capped and tier-gated like any other, and there
// is no path into the tool loop that skips limits.Check.
func TestTheSpendCapIsCheckedOnAnAutomatedTurn(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()

	// A cap the developer has already reached.
	tokenCap := int64(10)
	if err := h.limits.SetLimits(context.Background(), testUserSub,
		admin.Limits{Period: "24h", TokenCap: &tokenCap}, "test"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	h.limits.RecordUsage(context.Background(), testUserSub, h.session.ID, llm.Usage{
		InputTokens: 100000, OutputTokens: 100000, Provider: "fake", Model: "fake-model",
	})

	defer h.connectDeveloper()()
	h.deliver()

	if h.provider.turns() != 0 {
		t.Errorf("the provider was called %d times over a spent cap; §3.3 is enforced "+
			"before dispatch and an automated turn is not an exception", h.provider.turns())
	}
	for _, message := range h.messages() {
		if message.Injected() {
			t.Error("a summary was stored although the cap refused the turn; the check " +
				"happens before anything is written")
		}
	}

	// And it is not lost: with the cap lifted, the same run is delivered.
	lifted := int64(10_000_000)
	if err := h.limits.SetLimits(context.Background(), testUserSub,
		admin.Limits{Period: "24h", TokenCap: &lifted}, "test"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	h.deliver()
	if h.provider.turns() != 1 {
		t.Errorf("the run was not retried once the cap allowed it (turns = %d)",
			h.provider.turns())
	}
}

// The same run offered twice produces one summary in the conversation. The poller
// re-offers on every tick and after every restart, so the delivery has to be
// idempotent from what is durable — which is the injected message itself.
func TestARunOfferedRepeatedlyIsInjectedOnce(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})

	defer h.connectDeveloper()()
	for range 4 {
		h.poll()
		h.deliver()
	}

	injected := 0
	for _, message := range h.messages() {
		if message.Injected() && message.Subject == launched.ID {
			injected++
		}
	}
	if injected != 1 {
		t.Errorf("the summary was injected %d times; the poller offers a finished run "+
			"on every tick and the delivery has to be idempotent", injected)
	}
}

// --- the proposal, and the developer's answer ---

// A reply that reads the run and names no next step produces an explicit
// non-result, never an empty proposal. An empty string would read as "nothing to
// change", which is a finding nobody made.
func TestAReplyWithNoNextStepReportsThatRatherThanAnEmptyProposal(t *testing.T) {
	h := newHarness(t, "The run finished and the numbers look fine to me.")
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()
	h.deliver()

	result, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if result.Proposal.Stated() {
		t.Fatalf("a proposal was invented from a reply that named none: %q",
			result.Proposal.Text)
	}
	if result.Proposal.Status != experiments.NotComputedStatus {
		t.Errorf("status = %q, want an explicit non-result", result.Proposal.Status)
	}
	if result.Proposal.Reason != interpret.ReasonNoProposal {
		t.Errorf("reason = %q, want no_proposal_stated", result.Proposal.Reason)
	}
	// And it is distinguishable from a turn that has not run yet.
	if result.Pending() {
		t.Error("a reply that named no next step reads as an interpretation that never ran")
	}
}

// The three answers of §5.13's last sentence, each recorded.
func TestTheDeveloperAcceptsEditsOrRejectsAndEachIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
		edited   string
		wantErr  bool
	}{
		{"accepted", interpret.DecisionAccepted, "", false},
		{"edited", interpret.DecisionEdited, "raise lookback_days to 270 instead", false},
		{"rejected", interpret.DecisionRejected, "", false},
		{"an edit with no edited form", interpret.DecisionEdited, "", true},
		{"a decision that is not one of the three", "maybe", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, interpretingReply)
			h.ready()
			launched := h.launch()
			h.finish(launched, map[string]float64{"rmse": 0.31})
			h.poll()
			defer h.connectDeveloper()()
			h.deliver()

			before, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
			if err != nil {
				t.Fatalf("Interpretation: %v", err)
			}

			after, err := h.interpret.Decide(context.Background(), h.request(), launched.ID,
				interpret.DecisionRequest{
					ProposalID: before.Proposal.ID,
					Decision:   tc.decision,
					Edited:     tc.edited,
				})
			if tc.wantErr {
				if err == nil {
					t.Fatal("the decision was recorded")
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if after.Decision == nil {
				t.Fatal("nothing was recorded")
			}
			if after.Decision.Decision != tc.decision {
				t.Errorf("decision = %q, want %q", after.Decision.Decision, tc.decision)
			}
			if after.Decision.Proposed != before.Proposal.Text {
				t.Error("the record does not carry what was actually proposed, so it " +
					"stops being readable once the interpretation is recomputed")
			}
			if tc.decision == interpret.DecisionEdited && after.Decision.Edited != tc.edited {
				t.Errorf("edited = %q, want the developer's own form", after.Decision.Edited)
			}
			// D28: recording an answer is never a promotion.
			if after.Decision.Binding {
				t.Error("a decision was recorded as binding")
			}
		})
	}
}

// A rejected proposal stays rejected when the same run is interpreted again.
//
// This is what keying the decision on the *proposal's* fingerprint buys, and it is
// the same choice relations.RuleDecision makes: a verdict tied to the reading it
// appeared in would silently stop applying the moment anything recomputed.
func TestARejectedProposalStaysRejectedAcrossARecomputation(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()
	h.deliver()

	first, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if _, err := h.interpret.Decide(context.Background(), h.request(), launched.ID,
		interpret.DecisionRequest{
			ProposalID: first.Proposal.ID,
			Decision:   interpret.DecisionRejected,
			Note:       "the window is already as wide as the data goes",
		}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Recomputed: a fresh read rebuilds the summary from MLflow and re-derives the
	// proposal from the conversation. Nothing of the decision is in either.
	again, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if again.Proposal.ID != first.Proposal.ID {
		t.Fatalf("the proposal's id changed on a recomputation (%s then %s), so a "+
			"decision could not survive one", first.Proposal.ID, again.Proposal.ID)
	}
	if again.Decision == nil {
		t.Fatal("the rejection was lost; the proposal comes back as though nobody had " +
			"been asked")
	}
	if again.Decision.Decision != interpret.DecisionRejected {
		t.Errorf("decision = %q, want it to still be a rejection", again.Decision.Decision)
	}
}

// A developer who changes their mind adds a record rather than replacing one, and
// the newest is what stands.
func TestChangingAMindAppendsRatherThanReplaces(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()
	h.deliver()

	view, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	for _, decision := range []string{interpret.DecisionRejected, interpret.DecisionAccepted} {
		if _, err := h.interpret.Decide(context.Background(), h.request(), launched.ID,
			interpret.DecisionRequest{ProposalID: view.Proposal.ID, Decision: decision}); err != nil {
			t.Fatalf("Decide(%s): %v", decision, err)
		}
	}

	final, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if len(final.Decisions) != 2 {
		t.Errorf("decisions = %d, want both records: the log is append-only and "+
			"\"rejected then accepted\" must not read as \"accepted\"", len(final.Decisions))
	}
	if final.Decision == nil || final.Decision.Decision != interpret.DecisionAccepted {
		t.Errorf("standing decision = %+v, want the newest", final.Decision)
	}
}

// Deciding on a proposal that has since changed is refused rather than recorded as
// agreement with something the developer never read.
func TestADecisionOnAStaleProposalIsRefused(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()
	h.deliver()

	_, err := h.interpret.Decide(context.Background(), h.request(), launched.ID,
		interpret.DecisionRequest{
			ProposalID: "a-proposal-from-an-older-read", Decision: interpret.DecisionAccepted,
		})
	if err == nil {
		t.Fatal("a decision on a proposal that does not stand was recorded")
	}
	var stale *interpret.StaleProposalError
	if !errorsAs(err, &stale) {
		t.Errorf("error = %v, want a StaleProposalError so the route can answer 409", err)
	}
}

// Another developer's interpretation is not readable, and the answer is the same
// 404 the experiment routes give: whether an id exists is itself information.
func TestAnotherDevelopersInterpretationIsNotFound(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()
	h.deliver()

	other := h.request()
	other.UserSub = "someone-else"
	if _, err := h.interpret.Interpretation(context.Background(), other, launched.ID); err == nil {
		t.Fatal("another developer read this interpretation")
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// Deliver is exported, so it may be called while the loop is running one. Two
// overlapping passes must not both inject the same summary.
func TestConcurrentDeliveryPassesInjectOnce(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()

	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			h.interpret.Deliver(context.Background())
		}()
	}
	wait.Wait()

	injected := 0
	for _, message := range h.messages() {
		if message.Injected() && message.Subject == launched.ID {
			injected++
		}
	}
	if injected != 1 {
		t.Errorf("the summary was injected %d times across concurrent passes", injected)
	}
}

// A run whose session stays wedged is let go of rather than retried forever — and
// letting go is not the same as claiming it was interpreted.
//
// The distinction is what keeps the guarantee: the run is dropped from this
// process's queue so it stops costing a conversation read on every tick, and the
// poller offers it again while it is recent, at which point it is delivered.
func TestARunThatKeepsBeingRefusedIsLetGoOfAndOfferedAgain(t *testing.T) {
	h := newHarness(t, interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()

	// A developer's own turn, held open for the whole test.
	release := make(chan struct{})
	defer close(release)
	h.provider.block(release)
	if _, err := h.chat.Send(context.Background(), chat.StaticToken(unsignedToken()),
		testUserSub, h.session.ID, "how is it going?"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Every pass is refused, and after MaxAttempts the run leaves the queue.
	for range 12 {
		h.deliver()
	}
	for _, message := range h.messages() {
		if message.Injected() {
			t.Fatal("a summary was injected into a session that was mid-turn")
		}
	}

	// Offered again by the poller, because it was dropped and not marked
	// interpreted — the guarantee is that a developer does not lose the
	// interpretation, only that this process stops holding it.
	h.poll()
	pending := false
	for _, item := range h.interpret.PendingExperiments() {
		if item == launched.ID {
			pending = true
		}
	}
	if !pending {
		t.Error("the run was not offered again after being dropped; a developer who " +
			"was mid-conversation when it finished would lose the interpretation")
	}
}

// A conversation carrying two runs must not let the second one's interpretation
// stand in for the first's.
//
// The scan for "has this summary been answered" runs forward from the injected
// message, and without a stop at the *next* injected summary a run whose own turn
// said nothing would be reported as interpreted by whatever the assistant said
// about the run after it — an interpretation attributed to the wrong run.
func TestASecondRunsAnswerDoesNotCountAsTheFirstRunsInterpretation(t *testing.T) {
	// The first turn says nothing at all; the second interprets normally.
	h := newHarness(t, "   ", interpretingReply)
	h.ready()

	first := h.launch()
	h.finish(first, map[string]float64{"rmse": 0.42})

	h.write("op.py", "# a second state\n")
	h.commit("Adjust the operator")
	second := h.launch()
	h.finish(second, map[string]float64{"rmse": 0.31})

	defer h.connectDeveloper()()
	h.poll()
	h.deliver()

	firstView, err := h.interpret.Interpretation(context.Background(), h.request(), first.ID)
	if err != nil {
		t.Fatalf("Interpretation(first): %v", err)
	}
	if strings.Contains(firstView.Interpretation, "lookback_days") {
		t.Errorf("the first run's interpretation is the second run's text: %q",
			firstView.Interpretation)
	}
	if firstView.Proposal.Stated() {
		t.Errorf("the first run carries a proposal its own turn never made: %q",
			firstView.Proposal.Text)
	}
	// The distinguishing assertion. Reading forward past the second run's summary
	// finds an assistant turn with real text in it and reports the first run as
	// interpreted — by words about a different run. It has to read as not
	// interpreted, which is both true and what makes a later pass run the turn again.
	if !firstView.Pending() {
		t.Errorf("the first run reports itself interpreted at %v, although its own "+
			"turn said nothing; the answer counted was the *second* run's",
			firstView.InterpretedAt)
	}
	if firstView.Proposal.Reason != interpret.ReasonNotInterpreted {
		t.Errorf("reason = %q, want not_interpreted_yet", firstView.Proposal.Reason)
	}

	secondView, err := h.interpret.Interpretation(context.Background(), h.request(), second.ID)
	if err != nil {
		t.Fatalf("Interpretation(second): %v", err)
	}
	if !secondView.Proposal.Stated() {
		t.Fatalf("the second run has no proposal (%s)", secondView.Proposal.Detail)
	}
	if !strings.Contains(secondView.Proposal.Text, "lookback_days") {
		t.Errorf("proposal = %q, want the second run's own", secondView.Proposal.Text)
	}
}

// A turn that runs and says nothing must not retire the run as interpreted.
//
// The failure it guards against is silent and permanent: the provider errors or
// returns an empty completion, nothing is stored as an assistant message, and the
// run is marked done anyway — leaving the developer with a summary in their
// conversation that nothing ever read, and no later pass willing to look at it.
func TestATurnThatSaysNothingIsRetriedRatherThanRetired(t *testing.T) {
	// Empty on the first turn, a real interpretation on the second.
	h := newHarness(t, "", interpretingReply)
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()

	h.deliver()
	if h.provider.turns() != 1 {
		t.Fatalf("turns = %d, want the first one to have run", h.provider.turns())
	}
	after, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if !after.Pending() {
		t.Error("a turn that produced no reply was recorded as an interpretation")
	}

	// Retried, and this time it answers.
	h.deliver()
	if h.provider.turns() != 2 {
		t.Fatalf("turns = %d, want the run to have been tried again", h.provider.turns())
	}
	final, err := h.interpret.Interpretation(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("Interpretation: %v", err)
	}
	if final.Pending() || !final.Proposal.Stated() {
		t.Errorf("the run was never interpreted: %+v", final.Proposal)
	}
	// And the summary was not injected a second time on the retry.
	injected := 0
	for _, message := range h.messages() {
		if message.Injected() && message.Subject == launched.ID {
			injected++
		}
	}
	if injected != 1 {
		t.Errorf("the summary was injected %d times across the retry", injected)
	}
}

// A model that keeps saying nothing is let go of rather than re-prompted forever.
// Each of those turns is a provider call charged to the developer.
func TestARepeatedlySilentTurnStopsBeingRetried(t *testing.T) {
	h := newHarness(t, "", "", "", "", "", "")
	h.ready()
	launched := h.launch()
	h.finish(launched, map[string]float64{"rmse": 0.31})
	h.poll()
	defer h.connectDeveloper()()

	for range 8 {
		h.deliver()
	}
	if turns := h.provider.turns(); turns > 4 {
		t.Errorf("the assistant was re-prompted %d times after answering nothing; each "+
			"one is charged to the developer", turns)
	}
	if turns := h.provider.turns(); turns < 2 {
		t.Errorf("turns = %d, want it retried at least once before being let go of", turns)
	}
}
