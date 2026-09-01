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
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// Regressions for the defects M8 shipped with. Each one fails against the code as
// M8 left it and passes after the fix, and each is named for the consequence
// rather than for the line that caused it.

// --- the per-user MLflow namespace (D17, §5.13) ---

// A launch from the Experiments pane and a launch from the chat must land in the
// same MLflow experiment.
//
// They did not. The pane's request carries the Hub username, because the HTTP
// route reads it from the validated token; the chat's does not, because
// tools.Request has no field for one — so experimentName fell back to the Keycloak
// subject and produced a second experiment for the same developer and the same
// repository. D17 asks for one experiment per user per project, and §5.13's
// comparison_to_previous searches within one experiment, so the split made every
// chat-launched run report itself as the first: an assistant told "there is
// nothing to compare against" would then interpret a false finding, which is worse
// than an absent one.
func TestAChatLaunchAndAPaneLaunchLandInTheSameMLflowExperiment(t *testing.T) {
	h := newHarness(t)
	h.ready()

	// The pane: the username comes from the route's validated token.
	fromPane := h.launch()

	h.write("op.py", "# a second state\n")
	h.commit("Adjust the operator")

	// The chat: the tool surface builds the request from the token and the subject
	// alone, because that is all a dispatch carries.
	fromChat := h.launch(func(req *experiments.LaunchRequest) {
		req.Username = ""
	})

	if fromPane.MLflowExperimentName != fromChat.MLflowExperimentName {
		t.Errorf("the pane launched into %q and the chat into %q; D17 makes that one "+
			"experiment per developer per repository, and §5.13's comparison searches "+
			"within one experiment",
			fromPane.MLflowExperimentName, fromChat.MLflowExperimentName)
	}
	if names := h.mlflow.Experiments(); len(names) != 1 {
		t.Errorf("mlflow holds %v, want one experiment for one developer on one repository", names)
	}
}

// The consequence, asserted from the answer a model would read: the second run has
// something to compare against.
func TestAChatLaunchComparesAgainstAPaneLaunch(t *testing.T) {
	h := newHarness(t)
	h.ready()

	first := h.launch()
	h.mlflow.Finish(t, first.RunID, "FINISHED", map[string]float64{"rmse": 0.42})
	h.ray.SetStatus(first.SubmissionID, experiments.StatusSucceeded)
	if _, err := h.service.Get(t.Context(), h.request(), first.ID); err != nil {
		t.Fatalf("refresh the first run: %v", err)
	}

	h.write("op.py", "# a second state\n")
	h.commit("Adjust the operator")
	second := h.launch(func(req *experiments.LaunchRequest) { req.Username = "" })
	h.mlflow.Finish(t, second.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(second.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(t.Context(), h.request(), second.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(summary.ComparisonToPrevious) == 0 {
		t.Fatalf("the summary reports nothing to compare against (%q); the previous run "+
			"exists and is finished, and an empty comparison reads as \"first run\"",
			summary.Note)
	}
	if summary.ComparisonToPrevious[0].Metric != "rmse" ||
		summary.ComparisonToPrevious[0].Direction != "better" {
		t.Errorf("comparison = %+v, want rmse improving", summary.ComparisonToPrevious[0])
	}
}

// --- the run handover (D17, Operator Lib) ---

// The experiment ODE creates its run in has to be the one Operator Lib selects.
//
// It was not. ODE named its experiment `{prefix}/{user}/{repository}` while
// MLOperator.init() calls set_experiment(model_id) with
// `pipeline-{pipeline_id}_operator-{operator_id}`, and mlflow's fluent start_run
// refuses to resume a run that lives in an experiment other than the active one.
// So every launch died inside operator.init() with "Cannot start run ... because
// active experiment ID does not match environment run ID" — before a line of the
// developer's own code ran, and with a traceback whose deepest readable frame is
// the scaffold's op.py, which is where it looks like the developer's fault.
//
// Asserted against the ids the job is actually given rather than against a literal,
// because the defect was precisely that two derivations of the same pair drifted
// apart.
func TestTheRunLandsInTheExperimentOperatorLibSelects(t *testing.T) {
	h := newHarness(t)
	h.ready()

	result := h.launch()
	job := h.ray.LastJob(t)

	want := "pipeline-" + job.RuntimeEnv.EnvVars["PIPELINE_ID"] +
		"_operator-" + job.RuntimeEnv.EnvVars["OPERATOR_ID"]

	if result.MLflowExperimentName != want {
		t.Errorf("the run was created in %q and MLOperator selects %q; start_run "+
			"refuses the handover in MLFLOW_RUN_ID when the two differ",
			result.MLflowExperimentName, want)
	}
	if names := h.mlflow.Experiments(); len(names) != 1 || names[0] != want {
		t.Errorf("mlflow holds %v, want only %q — a second experiment here is the "+
			"failed launch, not litter", names, want)
	}
}

// --- a submission the cluster has forgotten (§5.12) ---

// Ray keeps a finished job only as long as its own retention allows, and forgets
// every submission when the cluster restarts. M8 reported that as a message on the
// record and returned the record unchanged — never persisted, and never terminal.
//
// Both halves matter. Unpersisted, the message is recomputed on every read and the
// record in the database still says RUNNING. Non-terminal, anything that polls the
// store for unfinished runs polls this one forever, and the answer is a 404 every
// time.
func TestARunTheClusterHasForgottenBecomesTerminalAndIsPersisted(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()

	// The job finished and its own code closed the run; then the cluster restarted.
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.Forget(launched.SubmissionID)

	record, err := h.service.Get(t.Context(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !experiments.Terminal(record.Status) {
		t.Errorf("status = %q, want a terminal one: a submission the cluster has "+
			"forgotten cannot change again, and a poller would read this forever",
			record.Status)
	}
	// MLflow still knows how it ended, and that is a better answer than "stopped".
	if record.Status != experiments.StatusSucceeded {
		t.Errorf("status = %q, want SUCCEEDED: the run's own code recorded FINISHED, "+
			"which is what ODE has left to go on", record.Status)
	}
	if record.Message == "" {
		t.Error("nothing says the cluster no longer knows the submission")
	}

	stored, found, err := h.store.Get(t.Context(), testUserSub, launched.ID)
	if err != nil || !found {
		t.Fatalf("stored = %v %v", found, err)
	}
	if stored.Status != record.Status {
		t.Errorf("the store still says %q while the answer said %q; the reconciliation "+
			"was never written back", stored.Status, record.Status)
	}
}

// A forgotten submission whose run MLflow cannot place either. There is nothing
// left to go on, so it is terminal and says why — never SUCCEEDED or FAILED, which
// would be a verdict ODE has no basis for.
func TestAForgottenSubmissionWithAnOpenRunIsTerminalWithoutAVerdict(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.ray.Forget(launched.SubmissionID)

	record, err := h.service.Get(t.Context(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !experiments.Terminal(record.Status) {
		t.Errorf("status = %q, want terminal", record.Status)
	}
	if record.Status == experiments.StatusSucceeded || record.Status == experiments.StatusFailed {
		t.Errorf("status = %q: nothing here knows whether the job succeeded, and "+
			"claiming either is a finding ODE has no basis for", record.Status)
	}
	if !strings.Contains(record.Message, "no longer knows") {
		t.Errorf("message = %q, want it to say the cluster forgot the submission",
			record.Message)
	}
}

// --- the job credential and what a refusal may repeat (§3.1 item 6) ---

// Ray's job route is FastAPI, and FastAPI renders the rejected request body into
// its own 422. The submission body carries runtime_env.env_vars, and that map holds
// SENERGY_TOKEN — the credential minted for the job on the developer's behalf.
//
// M8 put the first line of that body into UpstreamError.Message, which went into
// ode_experiments.message, into the 502 the API answers with, and into every log
// line that formatted the error. A credential in a database column and in an HTTP
// response is not a smaller problem than one in a log.
func TestNoJobCredentialReachesARefusalFromTheCluster(t *testing.T) {
	fake := newKeycloak(t)
	h := newHarness(t, withKeycloak(fake))
	h.ready()

	h.ray.EchoNext("/api/jobs/", http.StatusUnprocessableEntity)

	// A canary sorting ahead of every variable ODE sets, so the assertion is about
	// the submission body being repeated at all rather than about where a length cap
	// happens to fall. The credential is one field of that body; a cap that hides it
	// today hides it only until the body is shorter or the token is longer.
	const canary = "AAA_CANARY_VALUE_NOT_FOR_A_RESPONSE"
	_, err := h.service.Launch(t.Context(), experiments.LaunchRequest{
		Request:     h.request(),
		InputTopics: testInputTopics(),
		EnvVars:     map[string]string{"AAA_CANARY": canary},
	})
	if err == nil {
		t.Fatal("the launch succeeded; the double was told to refuse it")
	}

	secrets := []string{canary, "a-token-minted-for-the-job", "a-client-secret", "SENERGY_TOKEN"}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error repeats %q from the submission body: %v", secret, err)
		}
	}
	// The refusal still has to be diagnosable, or the fix is a silence.
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("error = %v, want the status the cluster refused with", err)
	}

	// And the same body must not have been written to the record that survives.
	listed, err := h.service.List(t.Context(), h.request(), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d experiments, want the refused one recorded", len(listed))
	}
	if listed[0].Status != experiments.StatusFailed {
		t.Errorf("status = %q, want FAILED", listed[0].Status)
	}
	encoded, _ := json.Marshal(listed)
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("the stored record carries %q: %s", secret, encoded)
		}
	}
}

// A submission the cluster refused leaves an MLflow run that ODE created and
// nothing ever closed, so MLflow's UI shows it RUNNING forever while ODE's own
// record says FAILED. M8 named the wart and left it; M9 reads run state, so it is
// closed here.
func TestARefusedSubmissionClosesTheMLflowRunItOpened(t *testing.T) {
	h := newHarness(t)
	h.ready()

	h.ray.FailNext("/api/jobs/", http.StatusServiceUnavailable)
	_, err := h.service.Launch(t.Context(), experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})
	if err == nil {
		t.Fatal("the launch succeeded; the double was told to refuse it")
	}

	listed, err := h.service.List(t.Context(), h.request(), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].RunID == "" {
		t.Fatalf("listed = %+v, want the record with the run it created", listed)
	}
	run := h.mlflow.Run(t, listed[0].RunID)
	if run.Status == "RUNNING" {
		t.Error("the run is still open in MLflow; it would read RUNNING forever beside " +
			"an ODE record that says the submission failed")
	}
	if run.Status != "FAILED" {
		t.Errorf("run status = %q, want FAILED", run.Status)
	}
	if run.EndTime == 0 {
		t.Error("the run has no end time, so MLflow reports no duration for it")
	}
}

// --- the listing (§5.12) ---

// A listing refreshes the runs that have not finished, and M8 did that one at a
// time with no bound on how many. A developer with two dozen queued runs against a
// cluster answering in 50ms waited over a second for their own list; against a
// cluster that had stopped answering, they waited the request timeout per run.
func TestAListingOfManyRunningJobsIsBoundedRatherThanSerial(t *testing.T) {
	h := newHarness(t)
	h.ready()

	// Records the store knows are running, pointing at submissions the cluster does
	// not have. Put in directly: this is about the refresh loop, and twenty-four real
	// launches would be twenty-four git archives.
	const running = 24
	for index := range running {
		if err := h.store.Put(t.Context(), experiments.Experiment{
			ID:                 "queued-" + strconv.Itoa(index),
			UserSub:            testUserSub,
			SubmissionID:       "sub-" + strconv.Itoa(index),
			MLflowExperimentID: "1",
			Status:             experiments.StatusRunning,
			SubmittedAt:        time.Now().UTC().Add(-time.Duration(index) * time.Minute),
			UpdatedAt:          time.Now().UTC(),
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	h.ray.Delay(50 * time.Millisecond)

	started := time.Now()
	listed, err := h.service.List(t.Context(), h.request(), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	elapsed := time.Since(started)

	if len(listed) < running {
		t.Errorf("listed %d, want every record: an unrefreshed record is still the "+
			"truth about what was submitted", len(listed))
	}
	// Serial would be at least running*50ms. The bound is deliberately loose: the
	// assertion is that the loop is not serial, not that it hits a particular figure.
	if budget := time.Duration(running) * 50 * time.Millisecond / 2; elapsed > budget {
		t.Errorf("the listing took %v against a %v budget; the refresh is still serial",
			elapsed, budget)
	}
}

// A caller may not ask for an unbounded listing. The limit is the caller's up to a
// ceiling, and zero takes the ceiling rather than everything.
func TestAListingIsCappedWhateverTheCallerAsksFor(t *testing.T) {
	h := newHarness(t)

	for index := range 150 {
		if err := h.store.Put(t.Context(), experiments.Experiment{
			ID:          "done-" + strconv.Itoa(index),
			UserSub:     testUserSub,
			Status:      experiments.StatusSucceeded,
			SubmittedAt: time.Now().UTC().Add(-time.Duration(index) * time.Minute),
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	listed, err := h.service.List(t.Context(), h.request(), 100000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) > 100 {
		t.Errorf("listed %d for a caller asking for 100000; the ceiling is what stops "+
			"one request becoming an unbounded read", len(listed))
	}
}

