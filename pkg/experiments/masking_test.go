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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
)

// D37: what a model reads of a run's own metrics is declared, not arbitrary.
//
// Two rules, both applied only inside MaskedFor, both hygiene laid over the real
// boundary Operator Lib carries (ending the training run before the replay,
// dropping MLFLOW_RUN_ID): a name allowlist, out of the developer's own
// evaluation.yaml plus the four fixed evaluation.* param names, and a phase filter
// that withholds a metric regardless of its name once the run's own
// operator_lib.training_ended_at tag says it was logged at or after that instant.
// Neither is presented here as more than that — see the comments beside
// permittedMetricNames and keepMetric in failure.go.

// The developer's own route (svc.Results, unfiltered) carries every metric the run
// reported; only a MaskedFor copy is cut, and the cut is counted rather than named.
func TestMaskedForRemovesAnUndeclaredMetricAndCountsIt(t *testing.T) {
	h := newHarness(t)
	h.ready() // the scaffold's own evaluation.yaml declares "baseline" and nothing else
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"baseline": 0.9, "extra_metric": 42})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if summary.Metrics["extra_metric"] != 42 || summary.WithheldMetrics != 0 {
		t.Fatalf("the developer's own summary = %+v, want every metric the run reported "+
			"and nothing marked withheld", summary)
	}

	masked := summary.MaskedFor(exposure.L0)
	if _, present := masked.Metrics["extra_metric"]; present {
		t.Errorf("metrics = %v, want the undeclared key removed", masked.Metrics)
	}
	if masked.Metrics["baseline"] != 0.9 {
		t.Errorf("metrics = %v, want the declared key kept", masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1", masked.WithheldMetrics)
	}
	if !strings.Contains(masked.Note, "1 metric") {
		t.Errorf("note = %q, want it to say a metric was withheld", masked.Note)
	}
	if strings.Contains(masked.Note, "extra_metric") {
		t.Errorf("note = %q, want the name withheld too — a name is already "+
			"information out of the run", masked.Note)
	}

	// And the developer's own copy is exactly what it was: MaskedFor built a new
	// map rather than deleting from the shared one.
	if summary.Metrics["extra_metric"] != 42 {
		t.Error("MaskedFor mutated the caller's own summary")
	}
}

// The developer's own criterion and every secondary metric survive: a name off
// the allowlist is withheld, not every name the job did not explain.
func TestMaskedForKeepsTheDeclaredCriterionAndEverySecondaryMetric(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml",
		"metric: rmse\ngoal: minimise\nthreshold: 0.35\nsecondary_metrics: [mae, mape]\n")
	h.commit("State the real criteria")
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{
		"rmse": 0.31, "mae": 1.2, "mape": 3.4, "extra_metric": 9.9,
	})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	for _, declared := range []string{"rmse", "mae", "mape"} {
		if _, present := masked.Metrics[declared]; !present {
			t.Errorf("metrics = %v, want %q kept: it is the criterion or a secondary one",
				masked.Metrics, declared)
		}
	}
	if _, present := masked.Metrics["extra_metric"]; present {
		t.Errorf("metrics = %v, want the undeclared key removed", masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1", masked.WithheldMetrics)
	}
}

// The four evaluation.* names are matched exactly, never as a prefix — a job
// cannot buy its way onto the allowlist with "evaluation.anything_else".
func TestMaskedForKeepsTheFourEvaluationParamNamesButNotAPrefix(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.removeFile("evaluation.yaml") // no criterion at all: only the four fixed names remain
	h.commit("Drop the evaluation criteria")
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{
		"evaluation.messages":       42,
		"evaluation.something_else": 7,
	})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if masked.Metrics["evaluation.messages"] != 42 {
		t.Errorf("metrics = %v, want the exact fixed name kept", masked.Metrics)
	}
	if _, present := masked.Metrics["evaluation.something_else"]; present {
		t.Errorf("metrics = %v, want the near-miss removed: the match is exact, not a prefix",
			masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1", masked.WithheldMetrics)
	}
}

// A repository with no evaluation.yaml at all declares nothing: the fixed
// evaluation.* names stay on the allowlist, but none of them is a name a job's own
// metrics actually use, so a model reads no metric whatsoever — the intended
// reading of "declared", stated in the note rather than left for a reader to work
// out from an empty map.
func TestMaskedForWithAnEmptyAllowlistWithholdsEveryMetricAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.removeFile("evaluation.yaml")
	h.commit("Drop the evaluation criteria")
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"rmse": 0.31, "mae": 1.2})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if len(masked.Metrics) != 0 {
		t.Errorf("metrics = %v, want none: nothing was declared", masked.Metrics)
	}
	if masked.WithheldMetrics != 2 {
		t.Errorf("withheld_metrics = %d, want 2", masked.WithheldMetrics)
	}
	if masked.Note == "" {
		t.Error("note is empty, want it to say metrics were withheld")
	}
}

// The early-return trap: MaskedFor used to return immediately when Failure was
// nil, which is the ordinary, successful case — exactly the one the metric filter
// is for. A successful run's summary must still be filtered.
func TestMaskedForFiltersMetricsEvenWhenTheRunDidNotFail(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"baseline": 0.9, "extra_metric": 42})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if summary.Failure != nil {
		t.Fatalf("failure = %+v on a run that succeeded", summary.Failure)
	}
	masked := summary.MaskedFor(exposure.L0)
	if _, present := masked.Metrics["extra_metric"]; present {
		t.Error("an undeclared metric survived masking on a run with no Failure block — " +
			"the early return before the metric filter is back")
	}
}

// comparison_to_previous carries a metric's own value in its Current field, so
// filtering Metrics alone would let a withheld value return through the delta.
func TestMaskedForFiltersComparisonToPreviousByTheSameAllowlist(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")

	first := h.launch()
	h.mlflow.Finish(t, first.RunID, "FINISHED", map[string]float64{"rmse": 0.42, "r2": 0.71})
	h.ray.SetStatus(first.SubmissionID, experiments.StatusSucceeded)
	if _, err := h.service.Get(context.Background(), h.request(), first.ID); err != nil {
		t.Fatalf("refresh the first run: %v", err)
	}

	second := h.launch()
	h.mlflow.Finish(t, second.RunID, "FINISHED", map[string]float64{"rmse": 0.31, "r2": 0.78})
	h.ray.SetStatus(second.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, second.ID)
	if len(summary.ComparisonToPrevious) != 2 {
		t.Fatalf("comparison = %+v, want both shared metrics on the unfiltered summary",
			summary.ComparisonToPrevious)
	}

	masked := summary.MaskedFor(exposure.L0)
	deltas := map[string]experiments.MetricDelta{}
	for _, delta := range masked.ComparisonToPrevious {
		deltas[delta.Metric] = delta
	}
	if _, present := deltas["r2"]; present {
		t.Errorf("comparison = %+v, want the undeclared metric's delta removed too",
			masked.ComparisonToPrevious)
	}
	if _, present := deltas["rmse"]; !present {
		t.Errorf("comparison = %+v, want the declared criterion's delta kept",
			masked.ComparisonToPrevious)
	}
}

// --- the phase filter (D37): withheld by when, not only by name ---

// trainingEndedAtTag mirrors the private constant in summary.go — this test
// package cannot see it, and the tag's name is Operator Lib's own, not a value a
// test should be free to invent.
const trainingEndedAtTag = "operator_lib.training_ended_at"

// A metric under a declared name, logged at or after the run's own training-ended
// tag, is still a test-window value — a name says nothing about when it was
// written, which is exactly why the allowlist alone is hygiene and not a boundary.
func TestMaskedForWithholdsADeclaredMetricLoggedAtOrAfterTrainingEnded(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")
	launched := h.launch()

	start := h.mlflow.Run(t, launched.RunID).StartTime
	cutoff := start + 500
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(cutoff, 10))
	// step 1's timestamp is start+1000, at and past the cutoff — logged from the
	// replay, under the exact name the developer declared.
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.10, 1)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if _, present := masked.Metrics["rmse"]; present {
		t.Errorf("metrics = %v, want the declared metric withheld: it was logged after "+
			"training ended", masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1", masked.WithheldMetrics)
	}
}

// The same declared metric, logged only before the cutoff, is the ordinary case
// and must survive.
func TestMaskedForKeepsADeclaredMetricLoggedBeforeTrainingEnded(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")
	launched := h.launch()

	start := h.mlflow.Run(t, launched.RunID).StartTime
	cutoff := start + 500
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(cutoff, 10))
	// step 0's timestamp is start+0, before the cutoff.
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.31, 0)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if masked.Metrics["rmse"] != 0.31 {
		t.Errorf("metrics = %v, want the metric kept: it was logged before training ended",
			masked.Metrics)
	}
	if masked.WithheldMetrics != 0 {
		t.Errorf("withheld_metrics = %d, want 0", masked.WithheldMetrics)
	}
}

// Without the tag, the phase filter has nothing to filter by, and MaskedFor falls
// back to the name allowlist alone — a cluster image whose Operator Lib predates
// v1.7.0 never writes the tag, and that is already reported once, by
// splitReport's "not confirmed by the run"; MaskedFor does not report it again.
func TestMaskedForWithNoTrainingEndedTagBehavesLikeTheNameAllowlistAlone(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")
	launched := h.launch()
	// No operator_lib.training_ended_at tag at all.
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"rmse": 0.31, "extra_metric": 9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if masked.Metrics["rmse"] != 0.31 {
		t.Errorf("metrics = %v, want the declared metric kept", masked.Metrics)
	}
	if _, present := masked.Metrics["extra_metric"]; present {
		t.Errorf("metrics = %v, want the undeclared metric removed by the name allowlist "+
			"alone", masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1 (the name allowlist alone)", masked.WithheldMetrics)
	}
}

// The trap this whole field exists to guard against: a finished run's summary is
// kept as encoded JSON (store.go's PutSummary/GetSummary) and served from there on
// a later read. An unexported carrier for the per-metric timestamps would survive
// the first build and vanish on exactly that reload — this run's second Results
// call is answered from the store, not rebuilt, so it is the reload this test
// means to exercise.
func TestMaskedForStillWithholdsAPostTrainingMetricAfterTheSummarysJSONRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.ready() // scaffold's own evaluation.yaml declares "baseline"
	launched := h.launch()

	start := h.mlflow.Run(t, launched.RunID).StartTime
	cutoff := start + 500
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(cutoff, 10))
	h.mlflow.LogMetric(t, launched.RunID, "baseline", 0.9, 1) // start+1000, past the cutoff
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	first := summaryOf(t, h, launched.ID)
	if first.MetricTimes["baseline"] == 0 {
		t.Fatal("metric_times carries nothing for baseline before any round trip")
	}

	// The second call is answered from the store (summarySettled kept the first),
	// so this is genuinely the encoded document read back, not the same value handed
	// out twice.
	second := summaryOf(t, h, launched.ID)
	masked := second.MaskedFor(exposure.L0)
	if _, present := masked.Metrics["baseline"]; present {
		t.Errorf("metrics = %v, want the post-training metric still withheld after the "+
			"summary was kept and reloaded as JSON", masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1", masked.WithheldMetrics)
	}
	// A model never reads a timestamp back, even though it was on the wire between
	// the store and this process.
	if masked.MetricTimes != nil {
		t.Errorf("metric_times = %v on a masked summary, want it cleared", masked.MetricTimes)
	}
}

// --- what a write to the run must not be able to arrange (D37) ---
//
// Everything on a run is writable by whoever holds its id, and the operator's own
// code can hold it: op.py is imported before init(), so a module-level
// `RUN = os.environ.get("MLFLOW_RUN_ID")` survives the unset the phase boundary
// performs. So the cutoff tag the phase filter reads is itself attacker-controlled,
// and the three tests below pin the one thing that keeps that from being a switch:
// whether a split ran is read from ODE's own experiment record, and under a split
// an unusable cutoff withholds everything instead of falling back to the name.

// A cutoff tag rewritten to something that is not a number does not turn the phase
// filter off. Before this, one `set_tag(RUN, ..., "not-a-number")` from inside
// infer() put a declared metric back in front of a model.
func TestASplitRunWithAnUnreadableCutoffWithholdsEveryMetric(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")
	launched := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	})

	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, "not-a-number")
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.31, 0)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if len(masked.Metrics) != 0 {
		t.Errorf("metrics = %v, want none: a split ran and the run's own cutoff is "+
			"unreadable, so nothing separates training from replay", masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1", masked.WithheldMetrics)
	}
}

// The same, for a cutoff that parses but names an instant the run never existed
// at — the other shape of the same rewrite, and the one that would otherwise put
// every metric before the cutoff and so keep all of them.
func TestASplitRunWithACutoffOutsideItsOwnClockWithholdsEveryMetric(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")
	launched := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	})

	far := time.Now().UTC().AddDate(100, 0, 0).UnixMilli()
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(far, 10))
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.31, 0)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if len(masked.Metrics) != 0 {
		t.Errorf("metrics = %v, want none: the cutoff names an instant outside the "+
			"run's own start and end, so it is not a phase transition that happened",
			masked.Metrics)
	}
}

// Params are cut to the replay's own four under a split, because a param is as
// writable as a metric and carries no timestamp for the phase filter to test.
func TestASplitRunShowsAModelOnlyTheEvaluationParams(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	})

	h.mlflow.SetParam(t, launched.RunID, "evaluation.messages", "1200")
	h.mlflow.SetParam(t, launched.RunID, "smuggled", "the-test-window-value")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	full := summaryOf(t, h, launched.ID)
	if _, present := full.Params["smuggled"]; !present {
		t.Fatal("the developer's own summary lost a param, which is not what this cuts")
	}

	masked := full.MaskedFor(exposure.L0)
	if _, present := masked.Params["smuggled"]; present {
		t.Errorf("params = %v, want the undeclared param withheld", masked.Params)
	}
	if masked.Params["evaluation.messages"] != "1200" {
		t.Errorf("params = %v, want the replay's own four kept", masked.Params)
	}
}

// Without a split nothing ran against a test window, and params pass as they
// always did.
func TestARunWithoutASplitStillShowsItsParams(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()

	h.mlflow.SetParam(t, launched.RunID, "folds", "5")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if masked.Params["folds"] != "5" {
		t.Errorf("params = %v, want them untouched without a split", masked.Params)
	}
}
