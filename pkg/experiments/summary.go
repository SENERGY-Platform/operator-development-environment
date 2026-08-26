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

// memoryMetrics are the metric names a job may use to report peak memory, in the
// order they are looked for. Absent is absent: §5.4.6's rule against a null read
// as a zero applies here too, so ResourceUsage.PeakMemoryMB stays out of the JSON
// entirely rather than reporting a run that used no memory.
var memoryMetrics = []string{"peak_memory_mb", "peak_memory", "max_memory_mb"}

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
	metrics := latestMetrics(run)

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
		Tags:         tags,
		StartedAt:    mlflowTime(run.Info.StartTime),
		EndedAt:      mlflowTime(run.Info.EndTime),
	}
	summary.Finished = Terminal(summary.Status)
	if !summary.Finished {
		summary.Note = "the run has not finished; these metrics are a snapshot rather " +
			"than a result"
	}

	summary.ResourceUsage = resourceUsage(run, metrics)
	summary.EvaluationCriteria, summary.SecondaryCriteria = criteria.ApplyTo(
		metrics, tags, record.CommitSHA, problem)

	if previous == nil {
		summary.ComparisonToPrevious = []MetricDelta{}
		if summary.Note == "" {
			summary.Note = "this is the first run of this experiment, so there is nothing " +
				"to compare it against"
		}
		return summary
	}

	summary.PreviousRunID = previous.runID()
	summary.ComparisonToPrevious = compare(metrics, latestMetrics(*previous))
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

// latestMetrics reduces MLflow's metric history to one value per key.
//
// MLflow returns the last logged value per key from runs/get already, but
// runs/search on some versions returns every step — so the reduction is done here
// rather than trusted, keyed on step first and timestamp second, which is the
// order MLflow itself defines "latest" by.
func latestMetrics(run mlflowRun) map[string]float64 {
	type point struct {
		value     float64
		step      int64
		timestamp int64
	}
	newest := make(map[string]point, len(run.Data.Metrics))
	for _, metric := range run.Data.Metrics {
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
	return out
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
			usage.PeakMemorySource = "metric " + name
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
		source := fmt.Sprintf("%s at %s, which is the developer's own (SPEC §5.8: no "+
			"tool may modify it)", EvaluationCriteriaPath, shortSHA(commitSHA))
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
