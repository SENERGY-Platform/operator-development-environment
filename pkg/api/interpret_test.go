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
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/interpret"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
)

// The M9 routes (§5.13), on top of the same doubles the M8 ones use.

// testSubject is the subject mintToken issues for, so the harness's chat session
// belongs to the developer the routes will be called as.
const testSubject = "user-123"

// interpretingProvider answers in the shape the injected message asks for: a
// reading of the run, then one concrete adjustment on a marked last line.
type interpretingProvider struct {
	mux   sync.Mutex
	turns int
}

func (p *interpretingProvider) Name() string { return "interpreting" }

func (p *interpretingProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		Tools: true, Streaming: true, System: true, Models: []string{"fake-model"},
	}
}

func (p *interpretingProvider) Stream(
	ctx context.Context, _ llm.Request,
) (<-chan llm.Event, error) {
	p.mux.Lock()
	p.turns++
	p.mux.Unlock()

	out := make(chan llm.Event, 3)
	out <- llm.TextEvent("rmse came in at 0.31, under the 0.35 you asked for, and down " +
		"from 0.42 on the previous run. The gain could be the wider window or the extra " +
		"folds — both moved.\n\nNEXT STEP: hold folds at 5 and raise lookback_days from " +
		"180 to 365, so the next run isolates the window.")
	out <- llm.DoneEvent("end_turn", llm.Usage{
		InputTokens: 100, OutputTokens: 50, Provider: "interpreting", Model: "fake-model",
	})
	close(out)
	return out, nil
}

// interpreted drives a run all the way to an interpretation in the conversation:
// launch, finish, notice, deliver.
func (h *experimentHarness) interpreted(t *testing.T) experiments.LaunchResult {
	t.Helper()
	launched := h.launch(t, map[string]any{"session_id": h.session.ID})
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31, "r2": 0.78})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	// The developer is connected, as they are whenever a pane is open. This is the
	// credential the interpretation turn runs on — ODE never has one of its own.
	disconnect := h.interpret.Connected(testSubject,
		chat.StaticToken("Bearer "+mintToken([]string{"developer"})))
	t.Cleanup(disconnect)

	// The poller's two phases, driven directly so the test does not wait on a timer:
	// settle the record from Ray, then offer it.
	poller, err := experiments.NewPoller(h.experiments, h.interpret, experiments.PollerOptions{
		Interval: time.Hour, Window: time.Hour, Batch: 50, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	poller.Tick(context.Background())
	h.interpret.Deliver(context.Background())
	return launched
}

// The route answers §5.13's document: the summary, the reading of it, and the
// concrete next proposal.
func TestTheInterpretationRouteAnswersTheReadingAndTheProposal(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.interpreted(t)

	response := h.call(t, http.MethodGet,
		"/experiments/"+launched.ID+"/interpretation", nil, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("interpretation = %d: %s", response.StatusCode, body)
	}
	var result interpret.Interpretation
	h.decode(t, response, &result)

	if result.Interpretation == "" {
		t.Error("the assistant's reading of the run is missing")
	}
	if !result.Proposal.Stated() {
		t.Fatalf("no proposal (%s: %s)", result.Proposal.Reason, result.Proposal.Detail)
	}
	if !strings.Contains(result.Proposal.Text, "lookback_days") {
		t.Errorf("proposal = %q", result.Proposal.Text)
	}
	if result.Summary.RunID != launched.RunID {
		t.Errorf("summary is for %q, want the run", result.Summary.RunID)
	}
	if result.Decisions == nil {
		t.Error("decisions is null rather than an empty list, which a client would " +
			"have to special-case")
	}
}

// The decision route records §5.13's last sentence, and answers 201 with the
// interpretation as it now stands.
func TestTheDecisionRouteRecordsTheDevelopersAnswer(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.interpreted(t)

	var before interpret.Interpretation
	h.decode(t, h.call(t, http.MethodGet, "/experiments/"+launched.ID+"/interpretation",
		nil, "developer"), &before)

	response := h.call(t, http.MethodPost,
		"/experiments/"+launched.ID+"/interpretation/decision", map[string]any{
			"proposal_id": before.Proposal.ID,
			"decision":    interpret.DecisionEdited,
			"edited":      "raise lookback_days to 270 rather than 365",
			"note":        "365 is more than the data covers",
		}, "developer")
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("decision = %d: %s", response.StatusCode, body)
	}
	var after interpret.Interpretation
	h.decode(t, response, &after)

	if after.Decision == nil || after.Decision.Decision != interpret.DecisionEdited {
		t.Fatalf("decision = %+v", after.Decision)
	}
	if after.Decision.Edited == "" {
		t.Error("the developer's own form of the adjustment was not recorded")
	}
	if after.Decision.Binding {
		t.Error("a decision was recorded as binding (D28)")
	}
}

// A decision on a proposal that no longer stands is a 409 with the repair, not a
// silently recorded agreement with something the developer never read.
func TestADecisionOnAStaleProposalIsAConflict(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.interpreted(t)

	response := h.call(t, http.MethodPost,
		"/experiments/"+launched.ID+"/interpretation/decision", map[string]any{
			"proposal_id": "from-an-older-read",
			"decision":    interpret.DecisionAccepted,
		}, "developer")
	if response.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 409: %s", response.StatusCode, body)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "reread") {
		t.Errorf("body = %s, want it to name the repair", body)
	}
}

// An edit with no edited form is a 400: an edit whose content was not recorded is
// a rejection with extra steps.
func TestAnEditWithNoEditedFormIsRefused(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.interpreted(t)

	var current interpret.Interpretation
	h.decode(t, h.call(t, http.MethodGet, "/experiments/"+launched.ID+"/interpretation",
		nil, "developer"), &current)

	response := h.call(t, http.MethodPost,
		"/experiments/"+launched.ID+"/interpretation/decision", map[string]any{
			"proposal_id": current.Proposal.ID,
			"decision":    interpret.DecisionEdited,
		}, "developer")
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", response.StatusCode)
	}
}

// Another developer's interpretation is a 404, the same answer the experiment
// routes give: whether an id exists is itself information about someone else.
func TestAnotherDevelopersInterpretationIs404(t *testing.T) {
	h := newExperimentHarness(t)
	h.prepare(t)
	h.commit(t, "Scaffold the operator")
	launched := h.interpreted(t)

	request, err := http.NewRequest(http.MethodGet,
		h.server.URL+"/experiments/"+launched.ID+"/interpretation", nil)
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
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
}

// The routes are behind the developer realm role like every other secured route.
func TestTheInterpretationRoutesAreBehindTheDeveloperRole(t *testing.T) {
	h := newExperimentHarness(t)
	for _, path := range []string{
		"/experiments/anything/interpretation",
	} {
		response := h.call(t, http.MethodGet, path, nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, response.StatusCode)
		}
		response = h.call(t, http.MethodGet, path, nil, "someone")
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("%s without the role = %d, want 403", path, response.StatusCode)
		}
	}
}
