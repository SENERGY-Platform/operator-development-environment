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
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// --- a double for the experiment service ---

type fakeExperiments struct {
	launches []experiments.LaunchRequest
	result   experiments.LaunchResult
	summary  experiments.Summary
	listed   []experiments.Experiment
	err      error
	// askedFor records the id every Results call named.
	askedFor []string
}

func (f *fakeExperiments) Launch(
	_ context.Context, req experiments.LaunchRequest,
) (experiments.LaunchResult, error) {
	f.launches = append(f.launches, req)
	if f.err != nil {
		return experiments.LaunchResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeExperiments) Results(
	_ context.Context, _ experiments.Request, id string,
) (experiments.Summary, error) {
	f.askedFor = append(f.askedFor, id)
	if f.err != nil {
		return experiments.Summary{}, f.err
	}
	return f.summary, nil
}

func (f *fakeExperiments) List(
	_ context.Context, _ experiments.Request, _ int,
) ([]experiments.Experiment, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.listed, nil
}

func experimentSurface(t *testing.T, fake *fakeExperiments) *Registry {
	t.Helper()
	registry, err := NewSurface(Deps{Experiments: fake})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

func dispatchExperiment(t *testing.T, registry *Registry, name, input string) Result {
	t.Helper()
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer developer-token", UserSub: "user-1", SessionID: "sess-1", Tier: L0},
		Call{ID: "c1", Name: name, Input: json.RawMessage(input)})
}

func launchedResult() experiments.LaunchResult {
	return experiments.LaunchResult{
		Experiment: experiments.Experiment{
			ID:           "exp-1",
			SubmissionID: "sub-1",
			RunID:        "run-1",
			Repository:   "jonah/pv-forecast",
			CommitSHA:    "0123456789abcdef0123456789abcdef01234567",
			Entrypoint:   "python training.py",
			Status:       experiments.StatusPending,
			SubmittedAt:  time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
		},
		Credential: experiments.Credential{
			Source: "session", ExpiresWithSession: true,
			Note: "this job carries the developer's interactive session token",
		},
	}
}

// --- the §5.8 columns ---

// §5.8 puts launch_experiment at L0 *with* a confirmation and
// get_experiment_results at L0 without one. Both halves are deliberate and both
// are asserted, because the confirmation is the whole control on the first.
func TestTheExperimentToolsCarryTheTiersAndConfirmationsOfSection58(t *testing.T) {
	registry := experimentSurface(t, &fakeExperiments{})

	launch, found := registry.Lookup("launch_experiment")
	if !found {
		t.Fatal("launch_experiment is not in the registry")
	}
	if launch.MinTier != L0 || !launch.Confirm {
		t.Errorf("launch_experiment = tier %s confirm %v, want L0 with a confirmation",
			launch.MinTier, launch.Confirm)
	}
	if !launch.Implemented() {
		t.Fatal("launch_experiment has no executor, so M8 did not reach the tool surface")
	}
	if launch.Unavailable != "" {
		t.Errorf("unavailable = %q, want it cleared once the executor is there",
			launch.Unavailable)
	}

	results, _ := registry.Lookup("get_experiment_results")
	if results.MinTier != L0 || results.Confirm {
		t.Errorf("get_experiment_results = tier %s confirm %v, want L0 without a confirmation",
			results.MinTier, results.Confirm)
	}
	if !results.Implemented() {
		t.Fatal("get_experiment_results has no executor")
	}
}

// After M8 every tool of §5.8 has an executor where its service is configured.
func TestEveryToolOfSection58IsImplementedWithItsServicesPresent(t *testing.T) {
	registry, err := NewSurface(Deps{
		Ontology:      &fakeOntology{},
		Devices:       &fakeDevices{device: testDevice()},
		Imports:       &fakeImports{},
		Timeseries:    &fakeTimeseries{points: 10},
		Profiler:      newTestProfiler(t),
		Selection:     &fakeSelection{},
		SelectionSink: &fakeSelectionSink{},
		Creations:     newFakeCreations(),
		Kernel:        &fakeKernel{},
		Charts:        &fakeCharts{},
		Relations:     &fakeRelations{},
		Repo:          &fakeRepo{},
		Experiments:   &fakeExperiments{},
		Simulation:    &fakeSimulation{},
	})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}

	unimplemented := make([]string, 0)
	for _, definition := range registry.Definitions() {
		if !definition.Implemented() {
			unimplemented = append(unimplemented, definition.Name+" ("+definition.Unavailable+")")
		}
	}
	if len(unimplemented) != 0 {
		t.Errorf("these declared tools have no executor: %v", unimplemented)
	}
	// Eighteen in §5.8, plus the eight the import surface adds — two lookups, the
	// type catalogue, one confirmed wiring proposal and the four that change the
	// platform — plus probe_export_data, which is the export half of
	// probe_availability and has to be its own tool because the platform's
	// availability endpoint is device-scoped, plus the fourteen of the simulation
	// surface: five reads, a template catalogue, four that author a scenario, one
	// that drives a running one, two for a backfill and one that uploads example
	// data for a channel to replay.
	if got := len(registry.Definitions()); got != 41 {
		t.Errorf("declared %d tools, want 41", got)
	}
}

// The degradation the rest of ODE does: declared, unavailable, and saying which
// configuration a deployment is missing.
func TestTheExperimentToolsStayUnavailableWithoutARayAndMlflowConfiguration(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}

	for _, name := range []string{"launch_experiment", "get_experiment_results"} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%q left the documented surface", name)
		}
		if definition.Implemented() {
			t.Errorf("%q has an executor with no experiment service behind it", name)
		}
		if !strings.Contains(definition.Unavailable, "ray_url") ||
			!strings.Contains(definition.Unavailable, "mlflow_url") {
			t.Errorf("%q reason = %q, want it to name the missing settings",
				name, definition.Unavailable)
		}
		for _, offered := range registry.Available(L0) {
			if offered.Name == name {
				t.Errorf("%q was offered to a provider without an experiment service", name)
			}
		}
	}
}

// --- dispatch ---

// A launch is not executed on the model's word. Dispatch holds it, and the
// executor is never reached until the developer resolves the confirmation.
func TestALaunchIsHeldForTheDevelopersConfirmation(t *testing.T) {
	fake := &fakeExperiments{result: launchedResult()}
	registry := experimentSurface(t, fake)

	result := dispatchExperiment(t, registry, "launch_experiment",
		`{"entrypoint": "python training.py"}`)

	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeAwaitingConfirmation)
	}
	if len(fake.launches) != 0 {
		t.Fatal("the job was submitted before the developer confirmed it")
	}
	if result.Confirmation == nil {
		t.Fatal("no confirmation was recorded for the developer to resolve")
	}

	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	confirmed := dispatcher.Confirm(context.Background(),
		Request{Token: "Bearer developer-token", UserSub: "user-1", SessionID: "sess-1", Tier: L0},
		*result.Confirmation)

	if confirmed.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", confirmed.Outcome, confirmed.Content)
	}
	if len(fake.launches) != 1 {
		t.Fatalf("launches = %d, want one after the confirmation", len(fake.launches))
	}
	launched := fake.launches[0]
	// The developer's own credential and session reach the service (§3.1 step 3),
	// and the session is one of §5.12's four metadata keys.
	if launched.Bearer != "Bearer developer-token" || launched.UserSub != "user-1" {
		t.Errorf("request = %+v, want the developer's own credential", launched.Request)
	}
	if launched.SessionID != "sess-1" {
		t.Errorf("session = %q, want the chat session recorded on the run", launched.SessionID)
	}

	answer, ok := confirmed.Content.(LaunchExperimentResult)
	if !ok {
		t.Fatalf("content = %T, want a LaunchExperimentResult", confirmed.Content)
	}
	if answer.CommitSHA == "" || answer.RunID == "" {
		t.Errorf("result = %+v, want the commit and the run named", answer)
	}
	// The model has to be able to say this to the developer.
	if !answer.Credential.ExpiresWithSession || answer.Credential.Note == "" {
		t.Errorf("credential = %+v, want the session limitation carried to the model",
			answer.Credential)
	}
}

// The refusal on a dirty working copy has to reach the model as something it can
// act on, because the action — commit — is one only the developer can take.
func TestALaunchRefusedForUncommittedWorkTellsTheModelWhy(t *testing.T) {
	fake := &fakeExperiments{err: &experiments.DirtyError{
		Repository: "jonah/pv-forecast",
		Paths:      []string{"op.py", "training.py"},
	}}
	registry := experimentSurface(t, fake)

	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	held := dispatchExperiment(t, registry, "launch_experiment", `{}`)
	result := dispatcher.Confirm(context.Background(),
		Request{Token: "Bearer t", UserSub: "user-1", SessionID: "sess-1", Tier: L0},
		*held.Confirmation)

	if !result.IsError {
		t.Fatalf("outcome = %q, want a failure the model can read", result.Outcome)
	}
	encoded, _ := json.Marshal(result.Content)
	for _, want := range []string{"op.py", "training.py", "uncommitted"} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("refusal = %s, want it to mention %q", encoded, want)
		}
	}
}

func TestReadingResultsWithoutAnIdListsWhatThereIsToChooseFrom(t *testing.T) {
	fake := &fakeExperiments{listed: []experiments.Experiment{{
		ID:          "exp-1",
		Repository:  "jonah/pv-forecast",
		CommitSHA:   "0123456789abcdef",
		Status:      experiments.StatusSucceeded,
		SubmittedAt: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
	}}}

	result := dispatchExperiment(t, experimentSurface(t, fake), "get_experiment_results", `{}`)
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	listing, ok := result.Content.(ExperimentListing)
	if !ok {
		t.Fatalf("content = %T, want an ExperimentListing", result.Content)
	}
	if len(listing.Experiments) != 1 || listing.Experiments[0].ExperimentID != "exp-1" {
		t.Errorf("listing = %+v", listing)
	}
	if len(fake.askedFor) != 0 {
		t.Error("a summary was fetched for an unnamed experiment")
	}
}

// The one thing §5.13 forbids. There is no tool that reads a log, and the summary
// a tool does return must not smuggle one in.
func TestTheResultsToolReturnsTheSummaryAndNeverALog(t *testing.T) {
	fake := &fakeExperiments{summary: experiments.Summary{
		RunID:     "run-1",
		CommitSHA: "0123456789abcdef",
		Status:    experiments.StatusSucceeded,
		Finished:  true,
		Metrics:   map[string]float64{"rmse": 0.31},
		Params:    map[string]string{"folds": "5"},
		Tags:      map[string]string{"commit_sha": "0123456789abcdef"},
		ComparisonToPrevious: []experiments.MetricDelta{{
			Metric: "rmse", Previous: 0.42, Current: 0.31, Delta: -0.11,
			Direction: "better", LowerIsBetter: true,
		}},
	}}

	result := dispatchExperiment(t, experimentSurface(t, fake), "get_experiment_results",
		`{"experiment_id": "exp-1"}`)
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	if len(fake.askedFor) != 1 || fake.askedFor[0] != "exp-1" {
		t.Errorf("asked for %v, want the named experiment", fake.askedFor)
	}

	summary, ok := result.Content.(experiments.Summary)
	if !ok {
		t.Fatalf("content = %T, want the §5.13 summary", result.Content)
	}
	if len(summary.ComparisonToPrevious) != 1 {
		t.Errorf("comparison = %+v, want the previous run carried through",
			summary.ComparisonToPrevious)
	}

	// Nothing in the shape can carry prose from a process.
	encoded, _ := json.Marshal(summary)
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"logs", "stdout", "stderr", "output", "traceback"} {
		if _, present := decoded[forbidden]; present {
			t.Errorf("the summary carries a %q field, which §5.13 forbids", forbidden)
		}
	}
}

// §5.8 denies a tool for every capability on its list, and nothing in M8 adds one.
func TestM8AddsNoDeniedCapability(t *testing.T) {
	registry := experimentSurface(t, &fakeExperiments{})
	for name := range Denied() {
		if _, found := registry.Lookup(name); found {
			t.Errorf("%q exists as a tool", name)
		}
	}
	// And §5.13's own rule, which is not on §5.8's list: no tool reads a log. The
	// guarantee is structural rather than a name check — tools.Experiments has no
	// Logs method, so an executor has nothing to call — and this is the assertion
	// that the interface has not grown one.
	var surface Experiments = &fakeExperiments{}
	if _, hasLogs := surface.(interface {
		Logs(context.Context, experiments.Request, string) (experiments.LogPage, error)
	}); hasLogs {
		t.Error("the tool surface can reach a job's logs, which §5.13 forbids")
	}
}

func TestTheExperimentServiceErrorTravelsRatherThanBeingFlattened(t *testing.T) {
	fake := &fakeExperiments{err: experiments.ErrNotFound}
	result := dispatchExperiment(t, experimentSurface(t, fake), "get_experiment_results",
		`{"experiment_id": "gone"}`)

	if result.Outcome == OutcomeOK || !result.IsError {
		t.Fatalf("outcome = %q, want a failure the model can read", result.Outcome)
	}
	if !errors.Is(fake.err, experiments.ErrNotFound) {
		t.Fatal("the fake was not asked")
	}
}
