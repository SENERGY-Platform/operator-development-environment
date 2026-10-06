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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
)

// D37: what a model reads of a run's own metrics is what the run's own timestamps
// clear, whatever the metrics are called.
//
// One rule, applied only inside MaskedFor and laid over the real boundary Operator
// Lib carries (ending the training run before the replay, dropping MLFLOW_RUN_ID):
// a phase filter that withholds a metric once the run's own
// operator_lib.training_ended_at tag says it was logged at or after that instant,
// and withholds every metric of a split run whose cutoff is unusable. It is not
// presented here as more than that — see the comment beside keepMetric in
// failure.go. A name allowlist out of evaluation.yaml used to sit on top; the
// tests below that keep an undeclared metric are what removed it.

// trainingEndedAtTag mirrors the private constant in summary.go — this test
// package cannot see it, and the tag's name is Operator Lib's own, not a value a
// test should be free to invent.
const trainingEndedAtTag = "operator_lib.training_ended_at"

// The developer's own route (svc.Results, unfiltered) carries every metric the run
// reported; only a MaskedFor copy is cut, and the cut is counted rather than named.
// Finish stamps its metrics at the run's end, past the cutoff; LogMetric at step 0
// stamps the run's start, before it.
func TestMaskedForWithholdsAMetricLoggedAfterTrainingEndedAndCountsIt(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()

	start := h.mlflow.Run(t, launched.RunID).StartTime
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.LogMetric(t, launched.RunID, "weather_pairs", 0, 0)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"extra_metric": 42})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if summary.Metrics["extra_metric"] != 42 || summary.WithheldMetrics != 0 {
		t.Fatalf("the developer's own summary = %+v, want every metric the run reported "+
			"and nothing marked withheld", summary)
	}

	masked := summary.MaskedFor(exposure.L0)
	if _, present := masked.Metrics["extra_metric"]; present {
		t.Errorf("metrics = %v, want the post-training key removed", masked.Metrics)
	}
	if _, present := masked.Metrics["weather_pairs"]; !present {
		t.Errorf("metrics = %v, want the training-phase key kept although "+
			"evaluation.yaml never names it", masked.Metrics)
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

// The case the allowlist was removed for: under a split with a usable cutoff, a
// diagnostic the training logged reaches the model without the developer having
// to declare it, and the replay's own write is still withheld.
func TestASplitRunShowsATrainingPhaseMetricNoCriterionNames(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	})

	start := h.mlflow.Run(t, launched.RunID).StartTime
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.LogMetric(t, launched.RunID, "weather_pairs", 0, 0)  // start, training
	h.mlflow.LogMetric(t, launched.RunID, "replay_rmse", 0.01, 1) // start+1000, replay
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if _, present := masked.Metrics["weather_pairs"]; !present {
		t.Errorf("metrics = %v, want the training-phase diagnostic kept", masked.Metrics)
	}
	if _, present := masked.Metrics["replay_rmse"]; present {
		t.Errorf("metrics = %v, want the replay's write withheld", masked.Metrics)
	}
	if masked.WithheldMetrics != 1 {
		t.Errorf("withheld_metrics = %d, want 1", masked.WithheldMetrics)
	}
}

// Without a split and without a cutoff nothing ran against a test window, so the
// criterion, every secondary metric and every metric no file names all pass.
func TestMaskedForKeepsEveryMetricOfARunWithoutASplitOrACutoff(t *testing.T) {
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
	for _, name := range []string{"rmse", "mae", "mape", "extra_metric"} {
		if _, present := masked.Metrics[name]; !present {
			t.Errorf("metrics = %v, want %q kept", masked.Metrics, name)
		}
	}
	if masked.WithheldMetrics != 0 {
		t.Errorf("withheld_metrics = %d, want 0", masked.WithheldMetrics)
	}
	if strings.Contains(masked.Note, "withheld from this summary") {
		t.Errorf("note = %q, want no withheld sentence when nothing was withheld", masked.Note)
	}
}

// A repository with no evaluation.yaml at all used to show a model no metric
// whatsoever. It declares nothing, and nothing has to be declared any more.
func TestMaskedForWithNoEvaluationYAMLStillShowsTheRunsMetrics(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.removeFile("evaluation.yaml")
	h.commit("Drop the evaluation criteria")
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"rmse": 0.31, "mae": 1.2})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if masked.Metrics["rmse"] != 0.31 || masked.Metrics["mae"] != 1.2 {
		t.Errorf("metrics = %v, want both kept", masked.Metrics)
	}
	if masked.WithheldMetrics != 0 {
		t.Errorf("withheld_metrics = %d, want 0", masked.WithheldMetrics)
	}
}

// The early-return trap: MaskedFor used to return immediately when Failure was
// nil, which is the ordinary, successful case — exactly the one the metric filter
// is for. A successful run's summary must still be filtered.
func TestMaskedForFiltersMetricsEvenWhenTheRunDidNotFail(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	start := h.mlflow.Run(t, launched.RunID).StartTime
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"extra_metric": 42})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if summary.Failure != nil {
		t.Fatalf("failure = %+v on a run that succeeded", summary.Failure)
	}
	masked := summary.MaskedFor(exposure.L0)
	if _, present := masked.Metrics["extra_metric"]; present {
		t.Error("a post-training metric survived masking on a run with no Failure block — " +
			"the early return before the metric filter is back")
	}
}

// comparison_to_previous carries a metric's own value in its Current field, so
// filtering Metrics alone would let a withheld value return through the delta.
func TestMaskedForFiltersComparisonToPreviousByTheSamePhaseFilter(t *testing.T) {
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
	start := h.mlflow.Run(t, second.RunID).StartTime
	h.mlflow.SetTag(t, second.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.LogMetric(t, second.RunID, "rmse", 0.31, 0) // before the cutoff
	h.mlflow.Finish(t, second.RunID, "FINISHED", map[string]float64{"r2": 0.78})
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
		t.Errorf("comparison = %+v, want the post-training metric's delta removed too",
			masked.ComparisonToPrevious)
	}
	if _, present := deltas["rmse"]; !present {
		t.Errorf("comparison = %+v, want the training-phase delta kept",
			masked.ComparisonToPrevious)
	}
}

// --- the phase filter (D37): withheld by when, never by name ---

// A metric under a declared name, logged at or after the run's own training-ended
// tag, is still a test-window value — a name says nothing about when it was
// written, which is why the filter is on the phase.
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

// Without the tag and without a split, the phase filter has nothing to filter by
// and nothing ran against a test window, so every metric passes. A missing tag
// under a split is the other case, pinned below; it is already reported once, by
// splitReport's "not confirmed by the run".
func TestMaskedForWithNoTrainingEndedTagAndNoSplitWithholdsNothing(t *testing.T) {
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
	if masked.Metrics["rmse"] != 0.31 || masked.Metrics["extra_metric"] != 9 {
		t.Errorf("metrics = %v, want both kept", masked.Metrics)
	}
	if masked.WithheldMetrics != 0 {
		t.Errorf("withheld_metrics = %d, want 0", masked.WithheldMetrics)
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
// an unusable cutoff withholds everything instead of keeping everything.

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

// D37 addendum: the post-replay metric's own four params cross the same boundary,
// by exact name — never a prefix, the same rule the four params above and the
// four metric names already carry.
func TestASplitRunShowsAModelTheFourMetricParamsButNotANearMiss(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	})

	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "3.5")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_n", "120")
	// A near miss: not one of the four exact names, and must not ride in on the
	// "evaluation." prefix the four legitimate ones share.
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value_extra", "should not pass")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	for name, want := range map[string]string{
		"evaluation.metric_status": "computed",
		"evaluation.metric_name":   "mae",
		"evaluation.metric_value":  "3.5",
		"evaluation.metric_n":      "120",
	} {
		if masked.Params[name] != want {
			t.Errorf("params[%q] = %q, want %q", name, masked.Params[name], want)
		}
	}
	if _, present := masked.Params["evaluation.metric_value_extra"]; present {
		t.Errorf("params = %v, want the near-miss name removed: the match is exact, "+
			"not a prefix", masked.Params)
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

// --- the two fields that carry a value without being the Metrics map ---

// The leak this pair of tests exists for: filtering Metrics alone was not enough.
// evaluation_criteria is graded in buildSummary from the unfiltered metrics, so a
// declared metric logged from the replay was withheld from the map and delivered
// anyway as the criterion's own value and verdict — no forged timestamp, no
// undeclared name, just the metric the developer asked to be judged on.
func TestMaskedForWithholdsTheCriterionOfAMetricLoggedAfterTrainingEnded(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")
	launched := h.launch()

	start := h.mlflow.Run(t, launched.RunID).StartTime
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	// Under the declared name, at step 1 — timestamp start+1000, past the cutoff.
	h.mlflow.LogMetric(t, launched.RunID, "rmse", 0.01, 1)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if summary.EvaluationCriteria.Value == nil || *summary.EvaluationCriteria.Value != 0.01 {
		t.Fatalf("the developer's own criterion = %+v, want the value the run logged",
			summary.EvaluationCriteria)
	}

	masked := summary.MaskedFor(exposure.L0)
	if masked.EvaluationCriteria.Value != nil {
		t.Errorf("evaluation_criteria.value = %v, want no value: the metric behind it "+
			"was logged after training ended", *masked.EvaluationCriteria.Value)
	}
	if masked.EvaluationCriteria.Met.Known() {
		t.Errorf("evaluation_criteria.met = %v, want an explicit non-result rather than "+
			"a verdict computed from a value this reader may not have",
			masked.EvaluationCriteria.Met.IsMet())
	}
	if reason := masked.EvaluationCriteria.Met.Status().Reason; reason != experiments.ReasonMetricWithheld {
		t.Errorf("reason = %q, want %q", reason, experiments.ReasonMetricWithheld)
	}
	// The developer's own summary is untouched by the copy the model read.
	if summary.EvaluationCriteria.Value == nil {
		t.Error("MaskedFor wrote through to the caller's own criterion")
	}
}

// The D37 addendum's own trap, found the same way as the pair above: a criterion
// whose value came from Operator Lib's post-replay param, not from an actual run
// metric, has no entry in MetricTimes at all — the library never logs the metric
// itself, only the param. Before withholdCriterion's own exception, that read as
// "no timestamp, so withhold", which blanked the library's own number back out of
// the exact copy a model reads, in the ordinary case where the developer's code
// logs no metric under the criterion's name.
func TestMaskedForKeepsALibrarySourcedCriterionWithNoMatchingRunMetric(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\ngoal: minimise\nthreshold: 5.0\n")
	h.commit("State the real criterion")

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	launched := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	h.mlflow.SetTag(t, launched.RunID, "operator_lib.history_end",
		split.TrainingEnd.Format(time.RFC3339))
	h.mlflow.SetTag(t, launched.RunID, "operator_lib.test_end",
		split.TestEnd.Format(time.RFC3339))
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, "0")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "3.5")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_n", "120")
	// No "mae" metric logged at all — only the param, which is the ordinary case
	// for this channel.
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	masked := summary.MaskedFor(exposure.L0)
	if masked.EvaluationCriteria.Value == nil || *masked.EvaluationCriteria.Value != 3.5 {
		t.Fatalf("masked value = %v, want the library's own 3.5 kept: a param carries no "+
			"timestamp for the phase filter to test", masked.EvaluationCriteria.Value)
	}
	if !masked.EvaluationCriteria.Met.IsMet() {
		t.Errorf("masked met = %+v, want it kept as a real verdict",
			masked.EvaluationCriteria.Met)
	}
	if masked.Params["evaluation.metric_value"] != "3.5" {
		t.Errorf("params = %v, want the raw param visible too, agreeing with the verdict",
			masked.Params)
	}
}

// The same door, one field over: peak_memory_mb is one float taken from whichever
// of three memory metrics the job reported, assembled before MaskedFor runs.
func TestMaskedForWithholdsAPeakMemoryFigureTakenFromAWithheldMetric(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	start := h.mlflow.Run(t, launched.RunID).StartTime
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"peak_memory_mb": 1234})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if summary.ResourceUsage.PeakMemoryMB != 1234 {
		t.Fatalf("the developer's own resource usage = %+v, want the figure the run "+
			"reported", summary.ResourceUsage)
	}

	masked := summary.MaskedFor(exposure.L0)
	if masked.ResourceUsage.PeakMemoryMB != 0 || masked.ResourceUsage.PeakMemorySource != "" {
		t.Errorf("resource_usage = %+v, want the figure dropped: peak_memory_mb was "+
			"logged after training ended", masked.ResourceUsage)
	}
	if masked.ResourceUsage.DurationSeconds != summary.ResourceUsage.DurationSeconds {
		t.Errorf("duration_s = %v, want it kept: it comes from the run's own start and "+
			"end times, not from anything the job logged",
			masked.ResourceUsage.DurationSeconds)
	}
}

// A memory metric logged before the cutoff is an ordinary reading and stays, so
// the filter above is not a blanket removal of the figure.
func TestMaskedForKeepsAPeakMemoryFigureLoggedBeforeTrainingEnded(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	start := h.mlflow.Run(t, launched.RunID).StartTime
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.LogMetric(t, launched.RunID, "peak_memory_mb", 1234, 0)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0)
	if masked.ResourceUsage.PeakMemoryMB != 1234 {
		t.Errorf("resource_usage = %+v, want the training-phase figure kept",
			masked.ResourceUsage)
	}
}

// --- a criteria file that could not be read or parsed at all (D38, second review) ---
//
// yamlsubset.go's own parse errors quote the raw line that stopped them with %q,
// on purpose: readCriteria wraps that error into criteria_unparseable's Detail,
// and the developer's own unmasked route (svc.Results) needs the line to fix it.
// criteria_unreadable's Detail can equally name whatever readCriteria's own
// callers returned. Both left Metric empty, which is exactly what
// withholdCriterion's ordinary shortcut let straight through unexamined — so the
// raw line reached a model through summary.EvaluationCriteria and
// get_experiment_results, the same class of leak D38 already closed for
// read_file, on a route nobody had pointed the same fix at.

// The value itself, not the wording of any message, is what has to be gone from
// the masked JSON — the same standard the report that found this asked for.
func TestMaskedForReplacesAnUnparseableCriteriaFilesRawLineWithFixedWords(t *testing.T) {
	h := newHarness(t)
	h.ready()
	// An unclosed quote around the target series: yamlsubset.go's unquoteYAML
	// reports "line 2: %q opens a quote it does not close" with the broken value
	// itself as the %q argument.
	h.write("evaluation.yaml", "metric: rmse\ntarget_series: \"sensor.ENERGY.Power\n")
	h.commit("Break the quote around the target series")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	status := summary.EvaluationCriteria.Met.Status()
	if status.Reason != experiments.ReasonCriteriaUnparseable {
		t.Fatalf("reason = %q, want criteria_unparseable", status.Reason)
	}
	if !strings.Contains(status.Detail, "sensor.ENERGY.Power") {
		t.Fatalf("this fixture does not exercise the leak: the unmasked detail = %q, "+
			"want it to still carry the raw line", status.Detail)
	}

	for _, tier := range []exposure.Tier{exposure.L0, exposure.L1, exposure.L2} {
		masked := summary.MaskedFor(tier)
		if masked.EvaluationCriteria.Met.Known() {
			t.Errorf("%s: met = %v, want no verdict from a file that was not read",
				tier, masked.EvaluationCriteria.Met)
		}
		if reason := masked.EvaluationCriteria.Met.Status().Reason; reason != experiments.ReasonCriteriaUnparseable {
			t.Errorf("%s: reason = %q, want it kept as criteria_unparseable — only the "+
				"Detail is fixed text, not the reason", tier, reason)
		}
		encoded, err := json.Marshal(masked)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tier, err)
		}
		if strings.Contains(string(encoded), "sensor.ENERGY.Power") {
			t.Errorf("%s: masked JSON = %s, want the raw line gone — this is what "+
				"get_experiment_results hands the model, at every tier, unconditionally",
				tier, encoded)
		}
	}
}

// The developer's own route is unaffected: readCriteria's unmasked return still
// carries the raw line, because the fix lives in withholdCriterion/MaskedFor, not
// in readCriteria or yamlsubset.go — the two of which TestAnUnparseableCriteriaFileSaysWhatStoppedIt
// (criteria_test.go) already pins for the "tab" case, kept green. This is the
// same property for the target-series-shaped fixture above, stated as its own
// test rather than folded into the masking assertions.
func TestTheUnmaskedCriteriaSummaryStillCarriesAnUnparseableFilesRawLine(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ntarget_series: \"sensor.ENERGY.Power\n")
	h.commit("Break the quote around the target series")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if !strings.Contains(summary.EvaluationCriteria.Met.Status().Detail, "sensor.ENERGY.Power") {
		t.Errorf("unmasked detail = %q, want the developer's own route unaffected by the "+
			"MaskedFor fix", summary.EvaluationCriteria.Met.Status().Detail)
	}
}

// D37 lets a model learn how many metrics were withheld, never which. A criterion
// on a metric the run never logged used to list every name the run did log in
// its Detail, withheld ones included, and stop after twelve — so a model read the
// names and guessed the rest.
func TestANotReportedCriterionNamesNoWithheldMetric(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\ngoal: minimise\nthreshold: 30\n")
	h.commit("State the real criterion")
	launched := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	})
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"validation_mae": 25, "timing.fit.seconds": 18})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if own := summary.EvaluationCriteria.Met.Status().Detail; !strings.Contains(own, "validation_mae") {
		t.Errorf("the developer's own detail = %q, want the names the run logged, "+
			"which is what makes a misspelt criterion repairable", own)
	}

	masked := summary.MaskedFor(exposure.L0)
	status := masked.EvaluationCriteria.Met.Status()
	if status.Reason != experiments.ReasonMetricNotReported {
		t.Fatalf("reason = %q, want metric_not_reported", status.Reason)
	}
	for _, name := range []string{"validation_mae", "timing.fit.seconds"} {
		if strings.Contains(status.Detail, name) {
			t.Errorf("detail = %q, want %q withheld", status.Detail, name)
		}
	}
	if !strings.Contains(status.Detail, "2 other metric(s), all withheld") {
		t.Errorf("detail = %q, want the count of what was withheld", status.Detail)
	}
}

// The visible names stay, so a misspelt criterion is still repairable from the
// model's copy; only the withheld ones become a count.
func TestANotReportedCriterionStillListsTheMetricsTheSummaryCarries(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")
	launched := h.launch()
	start := h.mlflow.Run(t, launched.RunID).StartTime
	h.mlflow.SetTag(t, launched.RunID, trainingEndedAtTag, strconv.FormatInt(start+500, 10))
	h.mlflow.LogMetric(t, launched.RunID, "val_rmse", 0.31, 0) // before the cutoff
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"extra_metric": 9.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	detail := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0).
		EvaluationCriteria.Met.Status().Detail
	if !strings.Contains(detail, "val_rmse") || !strings.Contains(detail, "1 more were withheld") {
		t.Errorf("detail = %q, want the kept name listed and the other counted", detail)
	}
	if strings.Contains(detail, "extra_metric") {
		t.Errorf("detail = %q, want the post-training name withheld", detail)
	}
}

// Under a split with no usable cutoff, every metric is withheld, so the note has
// to name that rule: the timestamp rule sends the developer looking for a late
// write that is not there.
func TestTheWithheldNoteNamesTheMissingTrainingEndUnderASplit(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	})
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"baseline": 0.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	note := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0).Note
	if !strings.Contains(note, trainingEndedAtTag) {
		t.Errorf("note = %q, want it to name the missing training end", note)
	}
	if strings.Contains(note, "a model reads only a metric logged before training ended") {
		t.Errorf("note = %q, want the timestamp rule left out: it did not withhold "+
			"anything here", note)
	}
}
