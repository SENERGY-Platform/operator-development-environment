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
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// The poller (M9): noticing a run finished with no developer watching.

// recordingSink is a TerminalSink that remembers what it was offered. It returns
// immediately, which is what the interface requires of a real one.
type recordingSink struct {
	mux      sync.Mutex
	finished []experiments.Experiment
}

func (s *recordingSink) RunFinished(_ context.Context, record experiments.Experiment) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.finished = append(s.finished, record)
}

func (s *recordingSink) ids() []string {
	s.mux.Lock()
	defer s.mux.Unlock()
	out := make([]string, 0, len(s.finished))
	for _, record := range s.finished {
		out = append(out, record.ID)
	}
	return out
}

func newPoller(t *testing.T, h *harness, sink experiments.TerminalSink) *experiments.Poller {
	t.Helper()
	poller, err := experiments.NewPoller(h.service, sink, experiments.PollerOptions{
		Interval: 10 * time.Millisecond,
		Window:   time.Hour,
		Batch:    50,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return poller
}

// The milestone's first half: a job ends and ODE notices without being asked.
func TestThePollerSettlesARunNobodyLookedAtAndOffersIt(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()

	// The job finishes. Nothing has read its status, so ODE's record still says
	// PENDING — exactly the state a developer who closed the tab leaves behind.
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	sink := &recordingSink{}
	newPoller(t, h, sink).Tick(t.Context())

	stored, found, err := h.store.Get(t.Context(), testUserSub, launched.ID)
	if err != nil || !found {
		t.Fatalf("stored = %v %v", found, err)
	}
	if stored.Status != experiments.StatusSucceeded {
		t.Errorf("status = %q, want the poller to have written back what Ray said",
			stored.Status)
	}
	if ids := sink.ids(); len(ids) != 1 || ids[0] != launched.ID {
		t.Errorf("offered = %v, want the finished run", ids)
	}
}

// A run that is still going is not offered, and a run with no chat session is not
// offered either — there is nowhere to inject §5.13's summary into.
func TestThePollerOffersOnlyFinishedSessionBoundRuns(t *testing.T) {
	h := newHarness(t)
	h.ready()

	stillGoing := h.launch()

	h.write("op.py", "# a second state\n")
	h.commit("Adjust the operator")
	noSession := h.launch(func(req *experiments.LaunchRequest) { req.SessionID = "" })
	h.mlflow.Finish(t, noSession.RunID, "FINISHED", map[string]float64{"rmse": 0.4})
	h.ray.SetStatus(noSession.SubmissionID, experiments.StatusSucceeded)

	sink := &recordingSink{}
	newPoller(t, h, sink).Tick(t.Context())

	for _, id := range sink.ids() {
		if id == stillGoing.ID {
			t.Error("a run that has not finished was offered for interpretation")
		}
		if id == noSession.ID {
			t.Error("a run launched outside a conversation was offered; there is no " +
				"session to inject a summary into")
		}
	}
}

// A run that has been finished for longer than the window is not news. Without the
// bound the second phase would grow with the table and re-offer a year of history
// on every tick.
func TestThePollerDoesNotOfferARunThatFinishedLongAgo(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	// Settle it, then age it past the window.
	sink := &recordingSink{}
	poller, err := experiments.NewPoller(h.service, sink, experiments.PollerOptions{
		Interval: time.Hour, Window: time.Minute, Batch: 50, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	poller.Tick(t.Context())
	if len(sink.ids()) != 1 {
		t.Fatalf("offered = %v, want the run once it finished", sink.ids())
	}

	stored, _, err := h.store.Get(t.Context(), testUserSub, launched.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	aged := time.Now().UTC().Add(-2 * time.Hour)
	stored.UpdatedAt = aged
	stored.EndedAt = &aged
	if err := h.store.Put(t.Context(), stored); err != nil {
		t.Fatalf("put: %v", err)
	}

	before := len(sink.ids())
	poller.Tick(t.Context())
	if after := len(sink.ids()); after != before {
		t.Errorf("a run that finished two hours ago was offered again under a "+
			"one-minute window (%d then %d)", before, after)
	}
}

// A submission the cluster has forgotten must not be polled forever.
//
// This is the M8 defect and the poller is what it would have cost: a record that
// never became terminal is a record this loop reads on every tick, for the life of
// the deployment, and answers a 404 to every time.
func TestThePollerStopsPollingAForgottenSubmission(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.Forget(launched.SubmissionID)

	sink := &recordingSink{}
	poller := newPoller(t, h, sink)
	poller.Tick(t.Context())

	running, err := h.store.Running(t.Context(), 50)
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	for _, record := range running {
		if record.ID == launched.ID {
			t.Fatal("the forgotten submission is still in the poller's work list; it " +
				"would be read on every tick for the life of the deployment")
		}
	}
	if ids := sink.ids(); len(ids) != 1 || ids[0] != launched.ID {
		t.Errorf("offered = %v, want the run: MLflow still knows how it ended", ids)
	}
}

// The lifecycle: started from the service, stopped by the process's own context,
// with nothing left running. The same idiom kernel.Service.Start uses.
func TestThePollerShutsDownCleanlyAndLeavesNoGoroutine(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.launch()

	sink := &recordingSink{}
	poller := newPoller(t, h, sink)

	runtime.GC()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	poller.Start(ctx)

	// Let it tick a few times, so the assertion is about a loop that was working
	// rather than one that never started.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.ray.Calls() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if h.ray.Calls() < 2 {
		t.Fatal("the poller never reached the cluster, so shutting it down proves nothing")
	}

	cancel()
	select {
	case <-poller.Stopped():
	case <-time.After(5 * time.Second):
		t.Fatal("the poller's goroutine did not return after its context ended")
	}

	// And it is not merely asked to stop: no further cluster call happens.
	settled := h.ray.Calls()
	time.Sleep(100 * time.Millisecond)
	if after := h.ray.Calls(); after != settled {
		t.Errorf("the cluster was called %d more times after shutdown", after-settled)
	}

	// The goroutine count is back where it started. Approximate on purpose — the
	// http client keeps idle connections — so the tolerance is small and the real
	// proof is Stopped() above.
	runtime.GC()
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines = %d before and %d after shutdown", before, after)
	}
}

// A poller with nowhere to deliver a finished run is a loop that costs cluster
// calls and discards the answer, so it is refused rather than built.
func TestAPollerWithoutASinkIsRefused(t *testing.T) {
	h := newHarness(t)
	if _, err := experiments.NewPoller(h.service, nil, experiments.PollerOptions{}); err == nil {
		t.Error("a poller with no sink was accepted")
	}
	if _, err := experiments.NewPoller(nil, &recordingSink{}, experiments.PollerOptions{}); err == nil {
		t.Error("a poller with no service was accepted")
	}
}
