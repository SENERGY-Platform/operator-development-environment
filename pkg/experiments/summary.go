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

package experiments

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
)

// §5.13's compact structured summary.
//
// The rule this file exists to enforce is one sentence of the spec: "builds a
// compact structured summary (never raw logs)". Everything a model is told about
// a finished run comes from here, and everything here comes from MLflow's params,
// metrics and tags — which are what the developer's own code chose to record,
// bounded, named and numeric. A log is unbounded prose from a process, and putting
// one in a model's context is the same category of mistake as putting a raw series
// in it (§4).

// Tag keys ODE writes on every run. Exported because the tool result quotes them
// and because a developer reading MLflow should be able to look them up.
const (
	TagCommitSHA    = "commit_sha"
	TagSessionID    = "session_id"
	TagUserSub      = "user_sub"
	TagExperimentID = "ode_experiment_id"
	TagRepository   = "repository"
	TagBranch       = "branch"
	TagEntrypoint   = "entrypoint"
	TagSubmissionID = "ray_submission_id"
	// TagSource marks a run ODE created, so a run made by hand in the same
	// experiment is distinguishable from one this service is accountable for.
	TagSource = "ode_source"
)

// evaluationTags are the two keys a job may set to report the criterion it was
// graded against. ODE does not invent them and does not read evaluation.yaml —
// the criteria are the developer's (§5.8 denies every tool that touches them),
// and turning the file into a verdict is M9's work. If the run reports one, it is
// carried through; if not, the field is absent rather than guessed.
const (
	tagEvaluationMetric    = "evaluation_metric"
	tagEvaluationThreshold = "evaluation_threshold"
	// tagEvaluationGoal lets a run say which direction it was judged in, so a tagged
	// criterion is not left guessing from the metric's name the way M8 had to.
	tagEvaluationGoal = "evaluation_goal"
)

// operatorLibSplitTags are the two tags Operator Lib v1.7.0 sets on the active run
// once the evaluation replay finishes (op_ml.py's __evaluate, in a `finally`): the
// bound the training actually read under and the test window's end. Operator
// Lib's own names, not ODE's — read, never written here — because they are what a
// scoring script and this confirmation both check the run against.
const (
	operatorHistoryEndTag = "operator_lib.history_end"
	operatorTestEndTag    = "operator_lib.test_end"
)

// operatorTrainingEndedAtTag is the tag Operator Lib v1.7.0 sets at the moment
// training ends and the replay begins — before the first `infer()` line, so it
// is on the run before anything the replay could write is. The value is the
// Unix milliseconds MLflow itself stamps a metric with, which is what lets
// MaskedFor compare the two directly (D37). Read here, never written: the
// library's own name, the same as the two tags above.
const operatorTrainingEndedAtTag = "operator_lib.training_ended_at"

// evaluationParams are the four params the same replay logs beside the tags:
// how many rows it saw and produced a result for, and the window it replayed.
// Also Operator Lib's own names.
const (
	paramEvaluationMessages    = "evaluation.messages"
	paramEvaluationResults     = "evaluation.results"
	paramEvaluationWindowStart = "evaluation.window_start"
	paramEvaluationWindowEnd   = "evaluation.window_end"
)

// memoryMetrics are the metric names a job may use to report peak memory, in the
// order they are looked for. Absent is absent: §5.4.6's rule against a null read
// as a zero applies here too, so ResourceUsage.PeakMemoryMB stays out of the JSON
// entirely rather than reporting a run that used no memory.
var memoryMetrics = []string{"peak_memory_mb", "peak_memory", "max_memory_mb"}

// peakMemorySourcePrefix is how ResourceUsage.PeakMemorySource names the metric it
// took the figure from. A constant rather than a literal in two places, because
// MaskedFor reads the name back out of it to decide whether the figure may be
// carried — see memorySourceMetric.
const peakMemorySourcePrefix = "metric "

// lowerIsBetterMarkers are the substrings that make a metric one where a smaller
// number is an improvement.
//
// A naming convention, and named as one: §5.13 wants a direction and ODE has no
// other source for it without the developer's evaluation criteria. Every delta
// carries LowerIsBetter beside Direction so a reader can see which rule was
// applied rather than trusting a verdict.
var lowerIsBetterMarkers = []string{
	"loss", "error", "mae", "mse", "rmse", "mape", "smape", "rmsle", "perplexity",
}

// buildSummary turns a run and its predecessor into §5.13's shape.
//
// criteria is the developer's own evaluation.yaml where it could be read, and
// problem is why it could not where it could not. Exactly one of the two is set,
// and the pair is a parameter rather than something this function fetches because
// fetching needs a developer's token and building a summary must not: the poller
// builds one the moment a run is terminal, with the service credential §3.1 item 5
// permits, and the criteria are read when the developer is next connected.
func buildSummary(
	record Experiment, run mlflowRun, previous *mlflowRun,
	criteria CriteriaDocument, problem *NotComputed,
) Summary {
	params := pairs(run.Data.Params)
	tags := pairs(run.Data.Tags)
	metrics, metricTimes := latestMetrics(run)

	// ODE's own user_sub tag is dropped on the way into the summary.
	//
	// This document is handed to a third-party model provider, and the tag is the
	// developer's Keycloak subject — an identifier for a person, of no use whatever
	// to a model reading metrics. It stays on the run in MLflow, where it is what
	// makes a run attributable; it does not need to leave the platform to do that.
	// The same Datensparsamkeit §3.2 argues for tiers, applied to the one field here
	// that identifies anybody.
	delete(tags, TagUserSub)

	summary := Summary{
		RunID:        record.RunID,
		ExperimentID: record.ID,
		SubmissionID: record.SubmissionID,
		CommitSHA:    firstNonEmpty(tags[TagCommitSHA], record.CommitSHA),
		Repository:   firstNonEmpty(tags[TagRepository], record.Repository),
		Entrypoint:   record.Entrypoint,
		Status:       reconcile(record.Status, run.Info.Status),
		Params:       params,
		Metrics:      metrics,
		MetricTimes:  metricTimes,
		Tags:         tags,
		StartedAt:    mlflowTime(run.Info.StartTime),
		EndedAt:      mlflowTime(run.Info.EndTime),
		// From the stored record, not recomputed: see InputTopics' own comment for
		// why that is the field that has to be trusted here.
		InputTopics: record.InputTopics,
	}
	summary.Finished = Terminal(summary.Status)
	if !summary.Finished {
		summary.Note = "the run has not finished; these metrics are a snapshot rather " +
			"than a result"
	}

	summary.ResourceUsage = resourceUsage(run, metrics)
	summary.EvaluationCriteria, summary.SecondaryCriteria = criteria.ApplyTo(
		metrics, tags, record.CommitSHA, problem)

	// Read from the same tags map EvaluationCriteria was just graded against,
	// after TagUserSub was dropped and before anything else touches it — the split
	// confirmation is step 16's guard against a cluster image whose Operator Lib
	// is older than v1.7.0, and it reads the run exactly the way a criterion tag
	// does: as what the job itself reported, not as what ODE asked for.
	summary.Split = splitReport(record.Split, tags, params, summary.Finished)
	if summary.Split != nil && summary.Split.Confirmed == splitNotConfirmed {
		summary.Note = strings.TrimSpace(summary.Note +
			" This run's data split was not confirmed by its own tags; see the " +
			"data_split block before treating it as an evaluation.")
	}

	if previous == nil {
		summary.ComparisonToPrevious = []MetricDelta{}
		if summary.Note == "" {
			summary.Note = "this is the first run of this experiment, so there is nothing " +
				"to compare it against"
		}
		return summary
	}

	summary.PreviousRunID = previous.runID()
	previousMetrics, _ := latestMetrics(*previous)
	summary.ComparisonToPrevious = compare(metrics, previousMetrics)
	return summary
}

// reconcile decides which status the summary reports when Ray and MLflow disagree.
//
// They routinely do, and neither is simply right. MLflow's run status is written
// by the job's own code, so a job killed by the cluster leaves it at RUNNING
// forever; Ray's job status is the process's, so a job whose driver exited zero
// after failing every fold reads SUCCEEDED. The rule is: Ray decides whether the
// run is over, because only Ray can see the process end; MLflow's FAILED is
// respected over Ray's SUCCEEDED, because a job that recorded its own failure knew
// something the exit code did not.
func reconcile(rayStatus, mlflowStatus string) string {
	switch strings.ToUpper(mlflowStatus) {
	case "FAILED", "KILLED":
		return StatusFailed
	}
	if rayStatus != "" {
		return rayStatus
	}
	switch strings.ToUpper(mlflowStatus) {
	case "FINISHED":
		return StatusSucceeded
	case "RUNNING", "SCHEDULED":
		return StatusRunning
	default:
		return StatusPending
	}
}

// latestMetrics reduces MLflow's metric history to one value per key, and
// separately to the *maximum* timestamp logged for that key over its whole
// history — not only the timestamp of the point the first reduction selects.
//
// MLflow returns the last logged value per key from runs/get already, but
// runs/search on some versions returns every step — so the value reduction is
// done here rather than trusted, keyed on step first and timestamp second,
// which is the order MLflow itself defines "latest" by.
//
// The timestamp reduction is kept apart from the value reduction on purpose
// (D37). `MlflowClient.log_metric` takes both step and timestamp from its
// caller, and the value reduction resolves ties by step first — so a write
// carrying a high step and a backdated timestamp is exactly the point that
// reduction would select, and reading only its timestamp would miss that the
// same key was also written for real, later, under a lower step. Taking the
// maximum over the whole history instead means a key touched during training
// and again from the replay reads as touched after training even when the
// replay's own point is the one disguised as earlier. It does not catch a
// write whose only point forges its own timestamp — see MaskedFor and
// docs/experiments.md for the limit that leaves.
func latestMetrics(run mlflowRun) (map[string]float64, map[string]int64) {
	type point struct {
		value     float64
		step      int64
		timestamp int64
	}
	newest := make(map[string]point, len(run.Data.Metrics))
	latestTimestamp := make(map[string]int64, len(run.Data.Metrics))
	for _, metric := range run.Data.Metrics {
		if metric.Timestamp > latestTimestamp[metric.Key] {
			latestTimestamp[metric.Key] = metric.Timestamp
		}
		current, seen := newest[metric.Key]
		if seen && (metric.Step < current.step ||
			(metric.Step == current.step && metric.Timestamp < current.timestamp)) {
			continue
		}
		newest[metric.Key] = point{
			value: metric.Value, step: metric.Step, timestamp: metric.Timestamp,
		}
	}
	out := make(map[string]float64, len(newest))
	for key, value := range newest {
		out[key] = value.value
	}
	return out, latestTimestamp
}

// compare produces §5.13's comparison_to_previous, in metric-name order.
//
// Sorted rather than map-ordered because this lands in a model's context and in a
// contract fixture, and a payload that reshuffles between identical calls makes
// both harder to read and impossible to diff.
func compare(current, previous map[string]float64) []MetricDelta {
	keys := make([]string, 0, len(current))
	for key := range current {
		if _, both := previous[key]; both {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	deltas := make([]MetricDelta, 0, len(keys))
	for _, key := range keys {
		now, before := current[key], previous[key]
		lower := lowerIsBetter(key)
		delta := now - before
		direction := "worse"
		switch {
		case delta == 0:
			// Neither better nor worse, and saying "worse" for an unchanged metric
			// would be a finding where there is none.
			direction = "unchanged"
		case (delta < 0) == lower:
			direction = "better"
		}
		deltas = append(deltas, MetricDelta{
			Metric:        key,
			Previous:      before,
			Current:       now,
			Delta:         delta,
			Direction:     direction,
			LowerIsBetter: lower,
		})
	}
	return deltas
}

// lowerIsBetter applies the naming convention.
func lowerIsBetter(metric string) bool {
	lowered := strings.ToLower(metric)
	for _, marker := range lowerIsBetterMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// resourceUsage fills §5.13's block from what the run actually reports.
func resourceUsage(run mlflowRun, metrics map[string]float64) ResourceUsage {
	usage := ResourceUsage{}
	if run.Info.StartTime > 0 && run.Info.EndTime > run.Info.StartTime {
		usage.DurationSeconds = float64(run.Info.EndTime-run.Info.StartTime) / 1000
	}
	for _, name := range memoryMetrics {
		if value, reported := metrics[name]; reported {
			usage.PeakMemoryMB = value
			usage.PeakMemorySource = peakMemorySourcePrefix + name
			break
		}
	}
	return usage
}

// ApplyTo grades a run against the criteria, and is where the four ways a
// criterion can fail to be evaluable become four different answers.
//
// The precedence is the developer's file first, the run's own tags second. §5.8
// makes the criteria the developer's own definition of success and denies every
// tool that could touch them, and the file is that definition; a tag is whatever
// the training code happened to write, which is useful as a fallback and is not
// the same authority. Where there is neither, the criterion says so — it is never
// absent, and it is never `met: false`.
func (d CriteriaDocument) ApplyTo(
	metrics map[string]float64, tags map[string]string, commitSHA string,
	problem *NotComputed,
) (Criterion, []Criterion) {
	if d.Primary != nil {
		source := fmt.Sprintf("%s at %s, which is the developer's own (no tool may "+
			"modify it)", EvaluationCriteriaPath, shortSHA(commitSHA))
		primary := grade(*d.Primary, metrics, source)
		secondary := make([]Criterion, 0, len(d.Secondary))
		for _, spec := range d.Secondary {
			secondary = append(secondary, grade(spec, metrics, source))
		}
		if len(secondary) == 0 {
			secondary = nil
		}
		return primary, secondary
	}

	// No criterion in the file, or no file. The run's own tags are the fallback M8
	// built, kept because a job that reported what it was graded against is telling
	// the truth about itself.
	if spec, ok := taggedCriterion(tags); ok {
		return grade(spec, metrics,
			"the run's own "+tagEvaluationMetric+" and "+tagEvaluationThreshold+" tags, "+
				"because "+EvaluationCriteriaPath+" named no metric"), nil
	}

	if problem != nil {
		return Criterion{Met: Verdict{status: *problem}, Source: EvaluationCriteriaPath}, nil
	}
	return Criterion{
		Met: NotEvaluated(ReasonNoCriterionStated,
			"%s at %s parsed but names no metric to judge the run on, and neither did the "+
				"run's own tags", EvaluationCriteriaPath, shortSHA(commitSHA)),
		Source: EvaluationCriteriaPath,
	}, nil
}

// taggedCriterion is M8's fallback: a criterion the run itself reported.
//
// Only when the run reported both keys. A partial one is dropped rather than
// half-filled: "threshold 0, met true" is a sentence a model would repeat.
func taggedCriterion(tags map[string]string) (CriterionSpec, bool) {
	metric := strings.TrimSpace(tags[tagEvaluationMetric])
	if metric == "" {
		return CriterionSpec{}, false
	}
	threshold, err := strconv.ParseFloat(strings.TrimSpace(tags[tagEvaluationThreshold]), 64)
	if err != nil {
		return CriterionSpec{}, false
	}
	spec := CriterionSpec{
		Metric: metric, Threshold: threshold, HasThreshold: true,
		LowerIsBetter: lowerIsBetter(metric),
	}
	if lower, ok := directionOf(tags[tagEvaluationGoal]); ok {
		spec.LowerIsBetter, spec.GoalStated = lower, true
	}
	return spec, true
}

// pairs flattens MLflow's key/value list.
func pairs(list []mlflowTag) map[string]string {
	out := make(map[string]string, len(list))
	for _, item := range list {
		out[item.Key] = item.Value
	}
	return out
}

// The three answers splitReport's Confirmed field carries. Constants rather than
// literals scattered across this file and its test, so a rename cannot desync them.
const (
	splitConfirmed    = "confirmed"
	splitNotConfirmed = "not confirmed by the run"
	splitPending      = "pending"
)

// splitReport is step 16's confirmation, built from a run's own tags and params
// rather than trusted from what ODE asked the run to do.
//
// Absence of the two tags is not, by itself, evidence that Operator Lib ignored
// the split: a run still in progress has not written them yet either, which is
// why an unfinished run reads "pending" rather than "not confirmed". Only a
// *terminal* run without matching tags means the split was silently dropped —
// simple_struct reads declared keys only, so an Operator Lib older than v1.7.0
// has no training_end or test_end attribute on Config at all and trains
// unbounded, exactly as if no split had been set.
func splitReport(split *exposure.Split, tags, params map[string]string, finished bool) *SplitReport {
	if split == nil {
		return nil
	}
	report := &SplitReport{TrainingEnd: split.TrainingEnd, TestEnd: split.TestEnd}

	historyEnd := strings.TrimSpace(tags[operatorHistoryEndTag])
	testEnd := strings.TrimSpace(tags[operatorTestEndTag])
	// Echoed back only once they parse, and re-rendered from the parsed instant
	// rather than passed through as the run wrote them.
	//
	// Everything on a run is writable by whoever holds its id, and the operator's
	// own code can hold it: op.py is imported before init(), so
	// `RUN = os.environ.get("MLFLOW_RUN_ID")` survives the unset, and an atexit
	// handler registered from infer() writes after the evaluation's own finally
	// block has run. A raw tag string reaching these fields is therefore a string
	// the operator chose, in a document a model reads — so a value that is not a
	// timestamp does not travel at all, and Confirmed below already carries the
	// verdict that something was wrong with it.
	report.RunHistoryEnd = renderedTime(historyEnd)
	report.RunTestEnd = renderedTime(testEnd)

	switch runSplit, ok := parseTaggedSplit(historyEnd, testEnd); {
	case ok && runSplit.Equal(*split):
		report.Confirmed = splitConfirmed
	case !finished:
		report.Confirmed = splitPending
	default:
		report.Confirmed = splitNotConfirmed
		report.Note = "the run recorded no split bounds, or different ones; a cluster " +
			"image with an Operator Lib older than v1.7.0 drops both fields silently " +
			"and the run then trained unbounded and skipped the evaluation; see " +
			"docs/operator-lib-versions.md"
	}

	if value, err := strconv.ParseInt(params[paramEvaluationMessages], 10, 64); err == nil {
		report.Messages = &value
	}
	if value, err := strconv.ParseInt(params[paramEvaluationResults], 10, 64); err == nil {
		report.Results = &value
	}
	// Same rule as the two tags above: these are the run's own account of the
	// window it replayed, and a param the operator wrote first is a param that
	// reaches a model. Only a parseable instant travels.
	report.WindowStart = renderedTime(params[paramEvaluationWindowStart])
	report.WindowEnd = renderedTime(params[paramEvaluationWindowEnd])

	return report
}

// renderedTime echoes a timestamp a run wrote only if it is one, re-rendered from
// the parsed instant so that nothing of the original string's own shape survives.
// Anything else becomes empty: the field is read by a model, and the run is not a
// trustworthy writer (see splitReport).
func renderedTime(raw string) string {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

// parseTaggedSplit reads the two tags Operator Lib's evaluation replay writes
// back into a Split, so it can be compared with Equal.
//
// time.RFC3339 rather than time.RFC3339Nano: Go's time.Parse accepts an optional
// fractional-second field even when the layout does not show one, so this reads
// both "...+00:00" and "...123456+00:00" — which is what Python's
// datetime.isoformat() produces for a UTC instant either with or without
// microseconds.
func parseTaggedSplit(historyEnd, testEnd string) (exposure.Split, bool) {
	if historyEnd == "" || testEnd == "" {
		return exposure.Split{}, false
	}
	trainingEnd, err := time.Parse(time.RFC3339, historyEnd)
	if err != nil {
		return exposure.Split{}, false
	}
	end, err := time.Parse(time.RFC3339, testEnd)
	if err != nil {
		return exposure.Split{}, false
	}
	return exposure.Split{TrainingEnd: trainingEnd, TestEnd: end}, true
}
