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
	"encoding/json"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// --- the run ODE owns (§5.11 item 7, §5.12) ---

// The acceptance criterion of M8, asserted where it is decided: the run carries
// the commit SHA because ODE tagged it at creation, not because the job's own code
// remembered to.
func TestTheRunIsTaggedWithTheCommitShaBeforeTheJobIsSubmitted(t *testing.T) {
	h := newHarness(t)
	h.ready()

	result := h.launch()
	run := h.mlflow.Run(t, result.RunID)

	if run.Tags[experiments.TagCommitSHA] != result.CommitSHA {
		t.Errorf("commit_sha tag = %q, want the commit the package was built from %q",
			run.Tags[experiments.TagCommitSHA], result.CommitSHA)
	}
	if run.Tags[experiments.TagSessionID] != "sess-1" {
		t.Errorf("session_id tag = %q", run.Tags[experiments.TagSessionID])
	}
	if run.Tags[experiments.TagUserSub] != testUserSub {
		t.Errorf("user_sub tag = %q", run.Tags[experiments.TagUserSub])
	}
	if run.Tags[experiments.TagExperimentID] != result.ID {
		t.Errorf("ode_experiment_id tag = %q, want %q",
			run.Tags[experiments.TagExperimentID], result.ID)
	}
	if run.Tags[experiments.TagRepository] != "jonah/pv-forecast" {
		t.Errorf("repository tag = %q", run.Tags[experiments.TagRepository])
	}
	// The tags exist at creation, which is what makes the claim hold even if the job
	// never starts: nothing here logged into the run afterwards.
	if run.Status != "RUNNING" {
		t.Errorf("run status = %q, want a run MLflow considers open", run.Status)
	}
}

// D17: one experiment per developer per repository, not one per launch.
func TestTheMLflowExperimentIsNamespacedPerUserAndRepository(t *testing.T) {
	h := newHarness(t)
	h.ready()

	first := h.launch()
	second := h.launch()

	if first.MLflowExperimentID != second.MLflowExperimentID {
		t.Errorf("two launches produced experiments %q and %q; D17 namespaces per user "+
			"and project, not per run", first.MLflowExperimentID, second.MLflowExperimentID)
	}
	names := h.mlflow.Experiments()
	if len(names) != 1 {
		t.Fatalf("experiments = %v, want exactly one", names)
	}
	if want := "pipeline-ode-jonah_operator-jonah-pv-forecast"; names[0] != want {
		t.Errorf("experiment name = %q, want %q — deterministic from the developer and "+
			"the repository so it is the same one next session, and Operator Lib's own "+
			"model_id so that MLOperator selects this experiment and not a second one",
			names[0], want)
	}
}

// --- the summary (§5.13) ---

func TestASummaryComparesTheRunAgainstThePreviousOne(t *testing.T) {
	h := newHarness(t)
	h.ready()

	// A finished first run.
	first := h.launch()
	h.mlflow.Finish(t, first.RunID, "FINISHED", map[string]float64{
		"rmse": 0.42, "r2": 0.71,
	})
	h.ray.SetStatus(first.SubmissionID, experiments.StatusSucceeded)
	if _, err := h.service.Get(context.Background(), h.request(), first.ID); err != nil {
		t.Fatalf("refresh the first run: %v", err)
	}

	// Then a second, better one.
	second := h.launch()
	h.mlflow.SetParam(t, second.RunID, "folds", "5")
	h.mlflow.Finish(t, second.RunID, "FINISHED", map[string]float64{
		"rmse": 0.31, "r2": 0.78,
	})
	h.ray.SetStatus(second.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), second.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}

	if summary.PreviousRunID != first.RunID {
		t.Errorf("previous run = %q, want the earlier finished run %q",
			summary.PreviousRunID, first.RunID)
	}
	deltas := map[string]experiments.MetricDelta{}
	for _, delta := range summary.ComparisonToPrevious {
		deltas[delta.Metric] = delta
	}
	if len(deltas) != 2 {
		t.Fatalf("comparison = %+v, want both shared metrics", summary.ComparisonToPrevious)
	}

	// rmse fell, and rmse is a metric where falling is an improvement.
	rmse := deltas["rmse"]
	if !rmse.LowerIsBetter || rmse.Direction != "better" {
		t.Errorf("rmse = %+v, want it read as an improvement", rmse)
	}
	if delta := rmse.Current - rmse.Previous; rmse.Delta != delta {
		t.Errorf("rmse delta = %v, want %v", rmse.Delta, delta)
	}
	// r2 rose, and r2 is a metric where rising is an improvement. Getting both from
	// one rule is what the naming convention buys, and carrying LowerIsBetter is
	// what keeps it honest.
	r2 := deltas["r2"]
	if r2.LowerIsBetter || r2.Direction != "better" {
		t.Errorf("r2 = %+v, want it read as an improvement without the lower-is-better rule", r2)
	}

	if summary.CommitSHA != second.CommitSHA {
		t.Errorf("commit_sha = %q, want the run's own tag", summary.CommitSHA)
	}
	if summary.Params["folds"] != "5" {
		t.Errorf("params = %v, want the run's own parameters", summary.Params)
	}
	if !summary.Finished {
		t.Error("a succeeded run is not reported as finished")
	}
	if summary.ResourceUsage.DurationSeconds <= 0 {
		t.Errorf("duration = %v, want it derived from the run's timestamps",
			summary.ResourceUsage.DurationSeconds)
	}
	if summary.ResourceUsage.PeakMemorySource != "" {
		t.Error("a peak memory figure was reported for a run that logged none")
	}
}

func TestAFirstRunSaysThereIsNothingToCompareAgainst(t *testing.T) {
	h := newHarness(t)
	h.ready()

	only := h.launch()
	h.mlflow.Finish(t, only.RunID, "FINISHED", map[string]float64{"rmse": 0.5})
	h.ray.SetStatus(only.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), only.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(summary.ComparisonToPrevious) != 0 {
		t.Errorf("comparison = %+v, want none", summary.ComparisonToPrevious)
	}
	if !strings.Contains(summary.Note, "first run") {
		t.Errorf("note = %q, want an empty comparison read as a first run rather than "+
			"as no change", summary.Note)
	}
	// Serialised as [] rather than null, for the reason the contract fixtures caught
	// once already: a frontend that maps over null crashes.
	encoded, _ := json.Marshal(summary)
	if !strings.Contains(string(encoded), `"comparison_to_previous":[]`) {
		t.Errorf("payload = %s, want an empty array rather than null", encoded)
	}
}

// §5.13's summary is params, metrics and tags. No line of a log is in it.
//
// This test used to read "nothing in it may be a log", and D34 narrowed that by one
// field: a failed run carries its last exception, extracted rather than excerpted.
// The property that did not change is the one asserted here — the summary is not a
// window onto the output. What the job printed, what it loaded, what the cluster
// said about the exit code: none of it has a field to travel in, and the whole of
// it is still on the developer's own route.
func TestASummaryCarriesNoLogLines(t *testing.T) {
	h := newHarness(t)
	h.ready()

	launched := h.launch()
	const output = `loading 43200 rows from urn:infai:ses:export:9f2c1b7e
Traceback (most recent call last):
  File "train.py", line 61, in <module>
    frame.astype(float)
ValueError: could not convert string to float: '24,7 kWh'
2026-09-01 03:14:31 ERROR job_supervisor.py:196 -- Job entrypoint command failed with exit code 1
`
	h.ray.SetLogs(launched.SubmissionID, output)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.5})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusFailed)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	encoded, _ := json.Marshal(summary)
	for _, forbidden := range []string{
		"loading", "urn:infai:ses:export", "job_supervisor", "exit code",
		"Traceback", "astype",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the summary carries the log line with %q in it: %s", forbidden, encoded)
		}
	}

	// The developer's own route still has all of them, which is the point of the
	// split — and the reason the extract may be bounded as hard as it is.
	page, err := h.service.Logs(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if page.Logs != output {
		t.Errorf("logs = %q, want the job's own output on the developer's route", page.Logs)
	}
}

// MLflow's status is written by the job's own code and Ray's by the process, so
// they disagree in both directions. A job the cluster reports as succeeded while
// the run recorded its own failure is the case that matters.
func TestAFailedRunOutranksASucceededJob(t *testing.T) {
	h := newHarness(t)
	h.ready()

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FAILED", map[string]float64{"rmse": 9.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.Status != experiments.StatusFailed {
		t.Errorf("status = %q, want the run's own recorded failure to win over the "+
			"driver's exit code", summary.Status)
	}
}

func TestAMetricHistoryIsReducedToItsLatestValue(t *testing.T) {
	h := newHarness(t)
	h.ready()

	launched := h.launch()
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.9, 1)
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.4, 3)
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.6, 2)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.Metrics["rmse"] != 0.4 {
		t.Errorf("rmse = %v, want the value at the highest step rather than the last "+
			"one the server happened to list", summary.Metrics["rmse"])
	}
}

// M9 reads the developer's own criteria and grades the run against them, which is
// what M8 deferred. The property under test is the one D24 exists for: a criterion
// that *could not be evaluated* is not a criterion the run failed.
//
// The scaffold's evaluation.yaml names `baseline` as the metric it judges on, and
// this run logs `rmse`. A bool would have said `met: false` — "the run missed the
// developer's target" — for a criterion nothing ever compared.
func TestACriterionTheRunNeverLoggedIsReportedAsSuchRatherThanAsUnmet(t *testing.T) {
	h := newHarness(t)
	h.ready()

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.2})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}

	criterion := summary.EvaluationCriteria
	if criterion.Metric != "baseline" {
		t.Errorf("metric = %q, want the one the scaffold's evaluation.yaml names",
			criterion.Metric)
	}
	if criterion.Met.Known() {
		t.Fatalf("met = %v, want no verdict: the run logged no baseline, and saying it "+
			"missed the target would be a finding nothing computed", criterion.Met)
	}
	status := criterion.Met.Status()
	if status.Reason != experiments.ReasonMetricNotReported {
		t.Errorf("reason = %q, want metric_not_reported", status.Reason)
	}
	if !strings.Contains(status.Detail, "rmse") {
		t.Errorf("detail = %q, want it to name what the run did log, so the developer "+
			"can see a mismatched metric name", status.Detail)
	}
	// And on the wire the distinction survives: an object, never `false`.
	encoded, err := json.Marshal(criterion)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"met":{"status":"not_computed"`) {
		t.Errorf("encoded = %s, want met to carry the non-result rather than a verdict",
			encoded)
	}
	if strings.Contains(string(encoded), `"met":false`) {
		t.Errorf("encoded = %s: an un-evaluable criterion marshalled as a failed one",
			encoded)
	}
	if criterion.Source == "" || !strings.Contains(criterion.Source, "evaluation.yaml") {
		t.Errorf("source = %q, want it to name the developer's own file",
			criterion.Source)
	}
}

// The developer's file wins over anything the run tagged itself with.
//
// §5.8 makes the criteria the developer's definition of success and denies every
// tool that could touch them. A tag is whatever the training code happened to
// write, which is a reasonable fallback and is not the same authority — so when
// both exist, the file decides.
func TestTheDevelopersFileOutranksACriterionTheRunTaggedItselfWith(t *testing.T) {
	h := newHarness(t)
	h.ready()

	launched := h.launch()
	h.mlflow.SetTag(t, launched.RunID, "evaluation_metric", "rmse")
	h.mlflow.SetTag(t, launched.RunID, "evaluation_threshold", "0.3")
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"rmse": 0.2, "baseline": 0.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.EvaluationCriteria.Metric != "baseline" {
		t.Errorf("metric = %q, want the file's rather than the run's own tag",
			summary.EvaluationCriteria.Metric)
	}
	// baseline 0.9 against the scaffold's threshold of 0.0, minimising: not met, and
	// that *is* a verdict rather than a non-result.
	if !summary.EvaluationCriteria.Met.Known() {
		t.Fatalf("met = %v, want a real verdict: the metric and the threshold are both here",
			summary.EvaluationCriteria.Met)
	}
	if summary.EvaluationCriteria.Met.IsMet() {
		t.Error("baseline 0.9 against a minimised threshold of 0.0 was read as met")
	}
}

// The run's own tags are the fallback, and only the fallback: they grade the run
// when the developer's file names no metric.
func TestTheRunsOwnTagsGradeItWhenTheFileNamesNoMetric(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "# The criteria are still being decided.\nrationale: not yet\n")
	h.commit("Empty the criteria while they are decided")

	launched := h.launch()
	h.mlflow.SetTag(t, launched.RunID, "evaluation_metric", "rmse")
	h.mlflow.SetTag(t, launched.RunID, "evaluation_threshold", "0.3")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.2})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Metric != "rmse" {
		t.Fatalf("metric = %q, want the run's own tag as the fallback", criterion.Metric)
	}
	if !criterion.Met.IsMet() {
		t.Errorf("criteria = %+v, want rmse 0.2 under a 0.3 threshold read as met", criterion)
	}
	if !strings.Contains(criterion.Source, "tags") {
		t.Errorf("source = %q, want it to say the criterion came from the run itself",
			criterion.Source)
	}
}

// --- ownership ---

// Every route resolves the developer from their own token, and the store's read
// is keyed by subject — so another developer's experiment is not found rather than
// forbidden, and nothing reveals that it exists.
func TestAnotherDevelopersExperimentIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.ready()
	mine := h.launch()

	other := h.request()
	other.UserSub = "user-2"

	if _, err := h.service.Get(context.Background(), other, mine.ID); err == nil ||
		!strings.Contains(err.Error(), "no such experiment") {
		t.Errorf("error = %v, want a not-found rather than a forbidden", err)
	}
	listed, err := h.service.List(context.Background(), other, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("another developer's list = %+v, want empty", listed)
	}
}

func TestStoppingAJobAsksRayAndReadsTheStatusBack(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()

	stopped, err := h.service.Stop(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(h.ray.Stopped()) != 1 || h.ray.Stopped()[0] != launched.SubmissionID {
		t.Errorf("stopped = %v, want the submission", h.ray.Stopped())
	}
	if stopped.Status != experiments.StatusStopped {
		t.Errorf("status = %q, want it read back from Ray rather than assumed",
			stopped.Status)
	}
}
