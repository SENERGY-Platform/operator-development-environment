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

package experiments_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// --- what a model may read: its own session's runs (D40) ---

// inSession launches from the given chat session; "" is the Experiments pane.
func inSession(session string) func(*experiments.LaunchRequest) {
	return func(req *experiments.LaunchRequest) { req.SessionID = session }
}

// asSession is the harness's request, read from the given chat session.
func asSession(h *harness, session string) experiments.Request {
	req := h.request()
	req.SessionID = session
	return req
}

// finish settles a run with the given metrics, and refreshes it so the store holds
// it as terminal — which is what makes it a candidate for a later run's comparison.
func finish(t *testing.T, h *harness, launched experiments.LaunchResult, metrics map[string]float64) {
	t.Helper()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", metrics)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)
	if _, err := h.service.Get(context.Background(), h.request(), launched.ID); err != nil {
		t.Fatalf("refresh %s: %v", launched.ID, err)
	}
}

func TestASessionListsOnlyTheRunsItLaunched(t *testing.T) {
	h := newHarness(t)
	h.ready()

	mine := h.launch(inSession("sess-a"))
	other := h.launch(inSession("sess-b"))
	pane := h.launch(inSession(""))

	listed, err := h.service.SessionList(context.Background(), asSession(h, "sess-a"), 10)
	if err != nil {
		t.Fatalf("session list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != mine.ID {
		ids := []string{}
		for _, record := range listed {
			ids = append(ids, record.ID)
		}
		t.Errorf("listed %v, want only %s — not %s from another session nor %s from the pane",
			ids, mine.ID, other.ID, pane.ID)
	}

	// The developer's own listing is unchanged: all three.
	all, err := h.service.List(context.Background(), h.request(), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("developer's listing has %d runs, want all 3", len(all))
	}
}

func TestARunFromAnotherSessionOrNoneIsNotFoundFromThisOne(t *testing.T) {
	h := newHarness(t)
	h.ready()

	other := h.launch(inSession("sess-b"))
	finish(t, h, other, map[string]float64{"rmse": 0.4})
	pane := h.launch(inSession(""))
	finish(t, h, pane, map[string]float64{"rmse": 0.3})

	var refusals []string
	for _, id := range []string{other.ID, pane.ID, "no-such-run"} {
		_, err := h.service.SessionResults(context.Background(), asSession(h, "sess-a"), id)
		if !errors.Is(err, experiments.ErrNotFound) {
			t.Fatalf("%s: err = %v, want not found", id, err)
		}
		refusals = append(refusals, err.Error())
	}
	// The same words for a run that exists elsewhere as for one that does not, apart
	// from the id itself: the refusal must not say which ids exist.
	for i, id := range []string{other.ID, pane.ID, "no-such-run"} {
		if want := experiments.ErrNotFound.Error() + ": " + id +
			" was not launched in this conversation"; refusals[i] != want {
			t.Errorf("refusal = %q, want %q", refusals[i], want)
		}
	}

	// The developer still reads both.
	for _, id := range []string{other.ID, pane.ID} {
		if _, err := h.service.Results(context.Background(), h.request(), id); err != nil {
			t.Errorf("developer's results for %s: %v", id, err)
		}
	}
}

func TestARequestWithoutASessionIsRefusedRatherThanReadAsEverySession(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch(inSession(""))
	finish(t, h, launched, map[string]float64{"rmse": 0.4})

	if _, err := h.service.SessionList(context.Background(), asSession(h, ""), 10); !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Errorf("session list without a session: err = %v, want invalid request", err)
	}
	if _, err := h.service.SessionResults(context.Background(), asSession(h, ""), launched.ID); !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Errorf("session results without a session: err = %v, want invalid request", err)
	}
}

// Runs A1, B1 and A2 of one repository, in that order. The developer's comparison
// for A2 is against B1, the newest finished run; a model in session A reads it
// against A1, and never sees B1's id or its metrics — including after the
// developer's read has kept the B1-compared summary.
func TestAModelsComparisonIsAgainstThePreviousRunOfItsOwnSession(t *testing.T) {
	h := newHarness(t)
	h.ready()

	a1 := h.launch(inSession("sess-a"))
	finish(t, h, a1, map[string]float64{"rmse": 0.50})
	b1 := h.launch(inSession("sess-b"))
	finish(t, h, b1, map[string]float64{"rmse": 0.20})
	a2 := h.launch(inSession("sess-a"))
	finish(t, h, a2, map[string]float64{"rmse": 0.40})

	developer, err := h.service.Results(context.Background(), h.request(), a2.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if developer.PreviousRunID != b1.RunID {
		t.Fatalf("developer's previous run = %q, want %q from the other session",
			developer.PreviousRunID, b1.RunID)
	}
	if _, kept, _ := h.store.GetSummary(context.Background(), testUserSub, a2.ID); !kept {
		t.Fatal("the developer's summary was not kept, so this does not exercise the cache")
	}

	model, err := h.service.SessionResults(context.Background(), asSession(h, "sess-a"), a2.ID)
	if err != nil {
		t.Fatalf("session results: %v", err)
	}
	if model.PreviousRunID != a1.RunID {
		t.Errorf("model's previous run = %q, want %q from its own session",
			model.PreviousRunID, a1.RunID)
	}
	if len(model.ComparisonToPrevious) != 1 {
		t.Fatalf("model's comparison = %+v, want rmse", model.ComparisonToPrevious)
	}
	if rmse := model.ComparisonToPrevious[0]; rmse.Previous != 0.50 || rmse.Current != 0.40 {
		t.Errorf("model's rmse = %+v, want 0.50 to 0.40 — A1's value, not B1's 0.20", rmse)
	}

	// The model's read did not change what the developer reads.
	again, err := h.service.Results(context.Background(), h.request(), a2.ID)
	if err != nil {
		t.Fatalf("results again: %v", err)
	}
	if again.PreviousRunID != b1.RunID {
		t.Errorf("developer's previous run after the model's read = %q, want %q",
			again.PreviousRunID, b1.RunID)
	}
}

// A1 of session A follows B1 of session B. The developer reads A1 against B1; a
// model in session A has nothing in its own session to compare against, and is
// told that in words about its conversation rather than about the repository.
func TestAModelsFirstRunInItsSessionHasNoComparisonEvenAfterAnotherSessionsRun(t *testing.T) {
	h := newHarness(t)
	h.ready()

	b1 := h.launch(inSession("sess-b"))
	finish(t, h, b1, map[string]float64{"rmse": 0.20})
	a1 := h.launch(inSession("sess-a"))
	finish(t, h, a1, map[string]float64{"rmse": 0.40})

	model, err := h.service.SessionResults(context.Background(), asSession(h, "sess-a"), a1.ID)
	if err != nil {
		t.Fatalf("session results: %v", err)
	}
	if model.PreviousRunID != "" || len(model.ComparisonToPrevious) != 0 {
		t.Errorf("model's comparison = %q %+v, want none", model.PreviousRunID,
			model.ComparisonToPrevious)
	}
	if model.Note != "this is the first run of this experiment in this conversation, so "+
		"there is nothing to compare it against" {
		t.Errorf("note = %q, want it to say first in this conversation", model.Note)
	}
}

// Summarise is what the interpretation injects into the run's own session, so it
// compares within that session from the start.
func TestTheInjectedSummaryComparesWithinTheRunsOwnSession(t *testing.T) {
	h := newHarness(t)
	h.ready()

	a1 := h.launch(inSession("sess-a"))
	finish(t, h, a1, map[string]float64{"rmse": 0.50})
	b1 := h.launch(inSession("sess-b"))
	finish(t, h, b1, map[string]float64{"rmse": 0.20})
	a2 := h.launch(inSession("sess-a"))
	finish(t, h, a2, map[string]float64{"rmse": 0.40})

	record, err := h.service.Get(context.Background(), h.request(), a2.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	summary, err := h.service.Summarise(context.Background(), record)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if summary.PreviousRunID != a1.RunID {
		t.Errorf("injected summary's previous run = %q, want %q, not %q from session B",
			summary.PreviousRunID, a1.RunID, b1.RunID)
	}
}

// D37 on the other side of a delta: Previous is the predecessor's value, so a key
// the predecessor logged after its own training ended must not reach a model
// through it, even where the current run logged the same key during training and
// the developer's previous run is the session's own.
func TestAModelsComparisonDropsAKeyThePreviousRunLoggedAfterItsTrainingEnded(t *testing.T) {
	h := newHarness(t)
	h.ready()

	a1 := h.launch(inSession("sess-a"))
	start := h.mlflow.Run(t, a1.RunID).StartTime
	h.mlflow.SetTag(t, a1.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.LogMetric(t, a1.RunID, "rmse", 0.50, 0)  // training
	finish(t, h, a1, map[string]float64{"foo": 0.01}) // run end, past the cutoff
	a2 := h.launch(inSession("sess-a"))
	finish(t, h, a2, map[string]float64{"rmse": 0.40, "foo": 0.02})

	developer, err := h.service.Results(context.Background(), h.request(), a2.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(developer.ComparisonToPrevious) != 2 {
		t.Fatalf("developer's comparison = %+v, want rmse and foo",
			developer.ComparisonToPrevious)
	}

	model, err := h.service.SessionResults(context.Background(), asSession(h, "sess-a"), a2.ID)
	if err != nil {
		t.Fatalf("session results: %v", err)
	}
	if len(model.ComparisonToPrevious) != 1 || model.ComparisonToPrevious[0].Metric != "rmse" {
		t.Fatalf("model's comparison = %+v, want rmse only: A1 logged foo after its "+
			"training ended", model.ComparisonToPrevious)
	}
	if rmse := model.ComparisonToPrevious[0]; rmse.Previous != 0.50 {
		t.Errorf("model's rmse = %+v, want A1's training-phase 0.50", rmse)
	}
}

// Where the developer's previous run is the session's own, the model reads the
// same comparison the developer does.
func TestAModelsComparisonMatchesTheDevelopersWhenThePreviousRunIsItsOwn(t *testing.T) {
	h := newHarness(t)
	h.ready()

	a1 := h.launch(inSession("sess-a"))
	finish(t, h, a1, map[string]float64{"rmse": 0.50})
	a2 := h.launch(inSession("sess-a"))
	finish(t, h, a2, map[string]float64{"rmse": 0.40})

	developer, err := h.service.Results(context.Background(), h.request(), a2.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	model, err := h.service.SessionResults(context.Background(), asSession(h, "sess-a"), a2.ID)
	if err != nil {
		t.Fatalf("session results: %v", err)
	}
	if model.PreviousRunID != a1.RunID || developer.PreviousRunID != a1.RunID {
		t.Errorf("previous runs = model %q, developer %q, want both %q",
			model.PreviousRunID, developer.PreviousRunID, a1.RunID)
	}
	if !reflect.DeepEqual(model.ComparisonToPrevious, developer.ComparisonToPrevious) ||
		model.Note != developer.Note {
		t.Errorf("model's comparison = %+v %q, want the developer's %+v %q",
			model.ComparisonToPrevious, model.Note, developer.ComparisonToPrevious, developer.Note)
	}
}
