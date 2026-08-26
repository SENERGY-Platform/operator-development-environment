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

// The developer's evaluation criteria (§5.13, M9).
//
// Two layers, tested separately on purpose. ParseCriteria is a pure function over
// a string and is tested as one, including against the file §5.11 item 3 actually
// scaffolds. The grading is tested through the service, over a real git working
// copy, because "read at the run's commit" is a claim about git rather than about
// a fixture.

// --- the parser ---

// The shape the scaffold writes, which is the shape almost every repository will
// have. A parser that could not read this one would be decoration.
func TestTheScaffoldedCriteriaFileParses(t *testing.T) {
	document, err := experiments.ParseCriteria(`# The evaluation criteria for this operator.
#
# Yours. ODE has no tool that writes this file.

# The metric a run is judged on.
metric: baseline

# The direction that counts as better, and the value that counts as good enough.
goal: minimise
threshold: 0.0

# What else to watch.
secondary_metrics: []

rationale: >
  Replace the metric and threshold with the ones this operator is actually for.
  The scaffold's values exist so the file parses.
`)
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary == nil {
		t.Fatal("no primary criterion was read")
	}
	if document.Primary.Metric != "baseline" {
		t.Errorf("metric = %q", document.Primary.Metric)
	}
	if !document.Primary.HasThreshold || document.Primary.Threshold != 0 {
		t.Errorf("threshold = %v (stated %v)",
			document.Primary.Threshold, document.Primary.HasThreshold)
	}
	if !document.Primary.LowerIsBetter || !document.Primary.GoalStated {
		t.Errorf("goal = %+v, want minimise stated by the file", document.Primary)
	}
	if len(document.Secondary) != 0 {
		t.Errorf("secondary = %+v, want none for an empty list", document.Secondary)
	}
	if !strings.Contains(document.Rationale, "Replace the metric") {
		t.Errorf("rationale = %q, want the folded block scalar", document.Rationale)
	}
}

// A developer who restructured their own file. §5.11 item 3 scaffolds it and then
// it is theirs, so the parser has to follow rather than insist.
func TestARestructuredCriteriaFileStillReads(t *testing.T) {
	document, err := experiments.ParseCriteria(`
criteria:
  - metric: val_rmse
    threshold: 0.35
    direction: minimize
  - metric: r2
    target: 0.8
    goal: maximise

secondary_metrics:
  - mae
  - name: training_seconds
    threshold: 900
    goal: min
`)
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary == nil || document.Primary.Metric != "val_rmse" {
		t.Fatalf("primary = %+v, want the first entry of the list", document.Primary)
	}
	if !document.Primary.LowerIsBetter {
		t.Error("`direction: minimize` was not read")
	}
	names := make([]string, 0, len(document.Secondary))
	for _, spec := range document.Secondary {
		names = append(names, spec.Metric)
	}
	want := []string{"r2", "mae", "training_seconds"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("secondary = %v, want %v", names, want)
	}
	for _, spec := range document.Secondary {
		switch spec.Metric {
		case "r2":
			if !spec.HasThreshold || spec.Threshold != 0.8 || spec.LowerIsBetter {
				t.Errorf("r2 = %+v", spec)
			}
		case "mae":
			if spec.HasThreshold {
				t.Errorf("mae = %+v, want a metric to watch with no threshold", spec)
			}
			if !spec.LowerIsBetter {
				t.Error("mae was not read as a metric where lower is better")
			}
		case "training_seconds":
			if !spec.HasThreshold || spec.Threshold != 900 {
				t.Errorf("training_seconds = %+v", spec)
			}
		}
	}
}

// A sequence at the key's own indentation, which YAML allows and which a reader
// that only understood the indented form would silently drop.
func TestASequenceAtTheKeysOwnIndentationReads(t *testing.T) {
	document, err := experiments.ParseCriteria("metric: rmse\nthreshold: 0.4\nsecondary_metrics:\n- mae\n- mape\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if len(document.Secondary) != 2 {
		t.Fatalf("secondary = %+v, want two", document.Secondary)
	}
}

// What the parser refuses, and that it refuses rather than half-reads. Each of
// these becomes criteria_unparseable with the line that stopped it, which is a
// repair the developer can act on.
func TestTheParserRefusesWhatItDoesNotUnderstand(t *testing.T) {
	cases := []struct {
		name   string
		source string
		says   string
	}{
		{"a tab for indentation", "metric: rmse\nnested:\n\tkey: value\n", "tab"},
		{"an anchor", "base: &defaults\n  metric: rmse\nrun: *defaults\n", "anchors"},
		{"a duplicate key", "metric: rmse\nmetric: mae\n", "twice"},
		{"an unclosed quote", "metric: \"rmse\n", "quote"},
		{"a nested inline collection", "criteria: [[a, b]]\n", "nested"},
		{"a second document", "metric: rmse\n---\nmetric: mae\n", "more than one"},
		{"a line that is not a mapping entry", "metric: rmse\njust a sentence\n", "key: value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := experiments.ParseCriteria(tc.source)
			if err == nil {
				t.Fatal("the parser accepted it; a half-read criteria file grades a run " +
					"against something the developer did not write")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %v, want it to name what stopped it (%q)", err, tc.says)
			}
		})
	}
}

// A `#` inside a value is not a comment, which is YAML's own rule and the one that
// keeps a metric name or a note intact.
func TestAHashInsideAValueIsNotAComment(t *testing.T) {
	document, err := experiments.ParseCriteria("metric: loss#1\nthreshold: 1\nrationale: \"a # inside a quoted string\"\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary.Metric != "loss#1" {
		t.Errorf("metric = %q", document.Primary.Metric)
	}
	if !strings.Contains(document.Rationale, "#") {
		t.Errorf("rationale = %q, want the hash kept inside the quotes", document.Rationale)
	}
}

// A threshold that is not a number is not a threshold. Left unset rather than
// defaulted to zero, so the criterion reports no_threshold instead of being graded
// against a number nobody wrote.
func TestANonNumericThresholdIsNotDefaultedToZero(t *testing.T) {
	document, err := experiments.ParseCriteria("metric: rmse\nthreshold: as low as possible\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary == nil {
		t.Fatal("no criterion was read")
	}
	if document.Primary.HasThreshold {
		t.Errorf("threshold = %v, want none: grading against a defaulted zero is a "+
			"verdict nobody asked for", document.Primary.Threshold)
	}
}

// --- the grading, over a real working copy ---

// A criterion the run met, which is the ordinary case and the one §5.13 documents
// as `"met": false` — a bare boolean on the wire, not an object.
func TestAMetCriterionIsATrueOnTheWire(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the real criterion")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if !criterion.Met.IsMet() {
		t.Errorf("criterion = %+v, want rmse 0.31 under 0.35 read as met", criterion)
	}
	if criterion.Value == nil || *criterion.Value != 0.31 {
		t.Errorf("value = %v, want the run's own figure beside the verdict", criterion.Value)
	}
	if !criterion.GoalStated {
		t.Error("goal_stated is false although the file said `goal: minimise`")
	}
	encoded, _ := json.Marshal(criterion)
	if !strings.Contains(string(encoded), `"met":true`) {
		t.Errorf("encoded = %s, want §5.13's bare boolean for a real verdict", encoded)
	}
}

// A criterion the run missed. A verdict, and visibly a different fact from one
// that could not be evaluated.
func TestAnUnmetCriterionIsAFalseOnTheWire(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.25\n")
	h.commit("State the real criterion")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if !criterion.Met.Known() {
		t.Fatalf("met = %v, want a verdict", criterion.Met)
	}
	if criterion.Met.IsMet() {
		t.Error("rmse 0.31 against a minimised 0.25 was read as met")
	}
	encoded, _ := json.Marshal(criterion)
	if !strings.Contains(string(encoded), `"met":false`) {
		t.Errorf("encoded = %s, want a bare false", encoded)
	}
}

// A commit with no evaluation.yaml. Not a failed criterion, and distinguishable
// from one whose file could not be reached.
func TestACommitWithNoCriteriaFileSaysSo(t *testing.T) {
	h := newHarness(t)
	h.createRepository()
	h.removeFile("evaluation.yaml")
	h.commit("Start without criteria")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Met.Known() {
		t.Fatalf("met = %v, want no verdict where there is no criterion", criterion.Met)
	}
	if reason := criterion.Met.Status().Reason; reason != experiments.ReasonNoCriteriaFile {
		t.Errorf("reason = %q, want no_criteria_file", reason)
	}
	if criterion.Metric != "" {
		t.Errorf("metric = %q, want none: there was no criterion to name", criterion.Metric)
	}
}

// A file ODE read whole and could not parse. The third un-evaluable case, and the
// one whose detail has to be actionable — it is the developer's own file and no
// tool of ODE's may fix it (§5.8).
func TestAnUnparseableCriteriaFileSaysWhatStoppedIt(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\nnested:\n\tthreshold: 0.3\n")
	h.commit("Reformat the criteria with a tab")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Met.Known() {
		t.Fatalf("met = %v, want no verdict from a file that was not read", criterion.Met)
	}
	status := criterion.Met.Status()
	if status.Reason != experiments.ReasonCriteriaUnparseable {
		t.Errorf("reason = %q, want criteria_unparseable", status.Reason)
	}
	if !strings.Contains(status.Detail, "tab") {
		t.Errorf("detail = %q, want the line that stopped the parse", status.Detail)
	}
}

// The three un-evaluable cases must be told apart from each other, not merely from
// a verdict. Each has a different repair, and a single "unknown" would collapse
// them back into the thing D24 forbids.
func TestTheUnEvaluableCasesAreDistinguishable(t *testing.T) {
	seen := map[experiments.CriterionReason]bool{}
	for _, reason := range []experiments.CriterionReason{
		experiments.ReasonNoCriteriaFile,
		experiments.ReasonCriteriaUnparseable,
		experiments.ReasonMetricNotReported,
		experiments.ReasonNoThreshold,
		experiments.ReasonNoDeveloperCredential,
		experiments.ReasonCriteriaUnreadable,
		experiments.ReasonNoCriterionStated,
	} {
		if seen[reason] {
			t.Errorf("%q is used for two different facts", reason)
		}
		seen[reason] = true
	}
}

// A criterion naming a metric with nothing to compare it against. The value is
// still reported — it is a real reading — and only the verdict is withheld.
func TestACriterionWithNoThresholdReportsTheValueAndNoVerdict(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\n")
	h.commit("Name the metric before the threshold is decided")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Met.Known() {
		t.Fatalf("met = %v, want no verdict without a threshold", criterion.Met)
	}
	if criterion.Met.Status().Reason != experiments.ReasonNoThreshold {
		t.Errorf("reason = %q, want no_threshold", criterion.Met.Status().Reason)
	}
	if criterion.Value == nil || *criterion.Value != 0.31 {
		t.Errorf("value = %v, want the reading reported even without a verdict",
			criterion.Value)
	}
}

// The criteria are read at the run's commit, not at HEAD.
//
// A criterion is part of the code state a run came from (§5.11 item 7). Grading a
// six-hour run against a threshold the developer tightened while it ran would be
// judging it by a rule it never had — and the developer would see a run they
// watched succeed reported as a failure.
func TestTheCriteriaAreReadAtTheRunsCommitRatherThanAtHead(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the criterion the run is submitted under")

	launched := h.launch()

	// The developer tightens the criterion while the job runs.
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.10\n")
	h.commit("Tighten the criterion for the next run")

	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Threshold == nil || *criterion.Threshold != 0.35 {
		t.Errorf("threshold = %v, want the one committed with the run (0.35) rather "+
			"than the one at HEAD (0.10)", criterion.Threshold)
	}
	if !criterion.Met.IsMet() {
		t.Errorf("criterion = %+v, want met against the threshold the run was submitted "+
			"under", criterion)
	}
	if !strings.Contains(criterion.Source, launched.CommitSHA[:7]) {
		t.Errorf("source = %q, want it to name the commit the criterion was read at",
			criterion.Source)
	}
}

// A summary built with no developer behind it says exactly that, and does not say
// the criterion failed. This is the shape every summary the poller builds has.
func TestASummaryBuiltWithoutADeveloperSaysTheCriteriaWereNotRead(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	record, err := h.service.Get(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	summary, err := h.service.Summarise(context.Background(), record)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}

	criterion := summary.EvaluationCriteria
	if criterion.Met.Known() {
		t.Fatalf("met = %v, want no verdict: nothing read the developer's file",
			criterion.Met)
	}
	status := criterion.Met.Status()
	if status.Reason != experiments.ReasonNoDeveloperCredential {
		t.Errorf("reason = %q, want no_developer_credential", status.Reason)
	}
	if !strings.Contains(status.Detail, "3.1 item 3") {
		t.Errorf("detail = %q, want it to name why a background summary has no token",
			status.Detail)
	}
	// The rest of §5.13's summary is complete: only the criteria waited.
	if summary.Metrics["rmse"] != 0.31 || !summary.Finished {
		t.Errorf("summary = %+v, want the metrics and the status built without a "+
			"developer connected", summary)
	}
}

// The criteria read is memoised per commit, and the memo must not turn a passing
// condition into a permanent answer.
//
// A commit's tree is immutable, so "this commit has no evaluation.yaml" and "it has
// one and it says X" are safe to keep. "There was no developer credential on that
// request" is not a fact about the commit at all, and a cache that kept it would
// leave every later read reporting a criterion nobody would ever grade.
func TestATransientCriteriaFailureIsNotRemembered(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: rmse\ngoal: minimise\nthreshold: 0.35\n")
	h.commit("State the criterion")

	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	// First, the way the poller sees it: no developer behind the request.
	record, err := h.service.Get(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	background, err := h.service.Summarise(context.Background(), record)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if background.EvaluationCriteria.Met.Status().Reason !=
		experiments.ReasonNoDeveloperCredential {
		t.Fatalf("reason = %q, want no_developer_credential",
			background.EvaluationCriteria.Met.Status().Reason)
	}

	// Then, with one. The criterion is graded rather than answered from the memo.
	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if !summary.EvaluationCriteria.Met.IsMet() {
		t.Errorf("criterion = %+v, want it graded once a developer's token was "+
			"available; a cached \"no credential\" would never be graded at all",
			summary.EvaluationCriteria)
	}

	// And the immutable answer is stable across reads.
	again, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if !again.EvaluationCriteria.Met.IsMet() ||
		again.EvaluationCriteria.Threshold == nil ||
		*again.EvaluationCriteria.Threshold != *summary.EvaluationCriteria.Threshold {
		t.Errorf("second read = %+v, want the same criterion", again.EvaluationCriteria)
	}
}

// --- the verdict on the wire ---

// §5.13 documents `met` as a boolean, and D24 forbids an un-evaluable field from
// being one. Both hold because a Verdict marshals as a bare boolean when there is
// a verdict and as a not_computed object when there is not — and because the zero
// value is the second kind, not `false`.
func TestAVerdictMarshalsAsABooleanOrAnExplicitNonResultAndNeverAsAFalsehood(t *testing.T) {
	cases := []struct {
		name    string
		verdict experiments.Verdict
		want    string
	}{
		{"met", experiments.Met(), `true`},
		{"unmet", experiments.Unmet(), `false`},
		{
			"not evaluated",
			experiments.NotEvaluated(experiments.ReasonNoCriteriaFile, "the commit has none"),
			`{"status":"not_computed","reason":"no_criteria_file","detail":"the commit has none"}`,
		},
		{
			// The property that matters most: a Verdict nobody set is not a failure.
			"the zero value",
			experiments.Verdict{},
			`{"status":"not_computed","reason":"no_criterion_stated","detail":"nothing evaluated this criterion"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.verdict)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != tc.want {
				t.Errorf("encoded = %s, want %s", encoded, tc.want)
			}

			// And it survives a round trip, so a fixture or a stored document reads back
			// as the same three-way answer rather than collapsing to a boolean.
			var back experiments.Verdict
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.Known() != tc.verdict.Known() {
				t.Errorf("Known() = %v after a round trip, want %v",
					back.Known(), tc.verdict.Known())
			}
			if back.IsMet() != tc.verdict.IsMet() {
				t.Errorf("IsMet() = %v after a round trip, want %v",
					back.IsMet(), tc.verdict.IsMet())
			}
			if !tc.verdict.Known() && back.Status().Reason != tc.verdict.Status().Reason {
				t.Errorf("reason = %q after a round trip, want %q",
					back.Status().Reason, tc.verdict.Status().Reason)
			}
		})
	}
}

// A null in a hand-edited fixture is the one input that could reintroduce the
// confusion the type exists to prevent. It reads as a non-result, never as false.
func TestANullVerdictReadsAsANonResultRatherThanAsUnmet(t *testing.T) {
	var verdict experiments.Verdict
	if err := json.Unmarshal([]byte(`null`), &verdict); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if verdict.Known() {
		t.Fatalf("a null was read as a verdict (met = %v)", verdict.IsMet())
	}
	if !strings.Contains(verdict.Status().Detail, "null") {
		t.Errorf("detail = %q, want it to say the verdict was null on read",
			verdict.Status().Detail)
	}
}

// A criteria file whose keys are metadata rather than a criterion must produce no
// criterion at all.
//
// `name` and `value` were accepted as a metric and a threshold, so an
// `evaluation.yaml` carrying the operator's own name and any numeric field
// produced a criterion the developer never wrote — and, because the file outranks
// the run's own tags, that invented criterion *displaced* a real one the job had
// reported. Guessing a metric out of a metadata key is exactly what §5.8 makes
// impossible for a tool and must not happen in a reader either.
func TestMetadataKeysDoNotBecomeACriterion(t *testing.T) {
	document, err := experiments.ParseCriteria(
		"name: pv-forecast\nvalue: 12\ndescription: the operator's own metadata\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary != nil {
		t.Errorf("a criterion was invented from metadata: %+v", document.Primary)
	}
	if len(document.Secondary) != 0 {
		t.Errorf("secondary criteria were invented from metadata: %+v", document.Secondary)
	}
}

// Inside a list of criteria, `- name: rmse` *is* the ordinary way to name one, and
// the position is what makes the difference.
func TestNameIsAMetricInsideAListOfCriteria(t *testing.T) {
	document, err := experiments.ParseCriteria(
		"criteria:\n  - name: rmse\n    threshold: 0.4\n    goal: minimise\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary == nil || document.Primary.Metric != "rmse" {
		t.Fatalf("primary = %+v, want the list item's name", document.Primary)
	}
	if !document.Primary.HasThreshold || document.Primary.Threshold != 0.4 {
		t.Errorf("threshold = %+v", document.Primary)
	}
}

// A file that names no criterion leaves the run's own tags in charge, rather than
// being displaced by something read out of a metadata key.
func TestMetadataOnlyCriteriaFileLeavesTheRunsTagsInCharge(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "name: pv-forecast\nvalue: 12\n")
	h.commit("Leave only metadata in the criteria file")

	launched := h.launch()
	h.mlflow.SetTag(t, launched.RunID, "evaluation_metric", "rmse")
	h.mlflow.SetTag(t, launched.RunID, "evaluation_threshold", "0.35")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.EvaluationCriteria.Metric != "rmse" {
		t.Errorf("metric = %q, want the run's own tag: the file named no criterion",
			summary.EvaluationCriteria.Metric)
	}
	if !summary.EvaluationCriteria.Met.IsMet() {
		t.Errorf("criterion = %+v, want rmse 0.31 under 0.35 read as met",
			summary.EvaluationCriteria)
	}
}

// A criterion that names no threshold carries none, rather than a fabricated zero.
//
// The zero is not harmless: `"threshold": 0` beside a metric with no target reads
// as a target of zero, and the scaffold's own file ships `threshold: 0.0`, so a
// reader cannot tell the invented one from the real one by its value.
func TestACriterionWithNoThresholdCarriesNoneOnTheWire(t *testing.T) {
	document, err := experiments.ParseCriteria("metric: rmse\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	criterion, _ := document.ApplyTo(map[string]float64{"rmse": 0.31}, nil, "abc1234", nil)
	if criterion.Threshold != nil {
		t.Errorf("threshold = %v, want none: the file named one nowhere", *criterion.Threshold)
	}
	encoded, err := json.Marshal(criterion)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"threshold"`) {
		t.Errorf("encoded = %s, want no threshold field at all", encoded)
	}

	// And a real threshold of zero — the scaffold's own — is still reported.
	document, err = experiments.ParseCriteria("metric: rmse\ngoal: minimise\nthreshold: 0.0\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	criterion, _ = document.ApplyTo(map[string]float64{"rmse": 0.31}, nil, "abc1234", nil)
	if criterion.Threshold == nil || *criterion.Threshold != 0 {
		t.Fatalf("threshold = %v, want the zero the developer wrote", criterion.Threshold)
	}
	encoded, _ = json.Marshal(criterion)
	if !strings.Contains(string(encoded), `"threshold":0`) {
		t.Errorf("encoded = %s, want the developer's own zero", encoded)
	}
}

// The summary is handed to a third-party model provider, so the one field in it
// that identifies a person does not travel with it.
//
// The tag stays on the run in MLflow, which is what makes a run attributable. It
// has no use to a model reading metrics, and §3.2's argument for exposure tiers is
// the same argument here: what does not need to leave the platform should not.
func TestTheDevelopersSubjectDoesNotTravelInTheSummary(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if value, present := summary.Tags[experiments.TagUserSub]; present {
		t.Errorf("the summary carries the developer's subject (%q) to the model provider",
			value)
	}
	encoded, _ := json.Marshal(summary)
	if strings.Contains(string(encoded), testUserSub) {
		t.Errorf("the subject appears in the summary: %s", encoded)
	}

	// It is still on the run itself, which is where it does its job.
	run := h.mlflow.Run(t, launched.RunID)
	if run.Tags[experiments.TagUserSub] != testUserSub {
		t.Errorf("user_sub tag on the run = %q, want it kept in MLflow",
			run.Tags[experiments.TagUserSub])
	}
	// And the tags a model does need are untouched.
	if summary.Tags[experiments.TagCommitSHA] == "" {
		t.Error("the commit_sha tag was dropped with it")
	}
}

// A run that tagged half a criterion has not stated one.
//
// "threshold 0, met true" is a sentence a model would repeat, and it would be
// about a target nobody set — so a tag pair with only one half of it is dropped
// rather than completed with a default.
func TestAHalfTaggedCriterionIsNotCompletedWithADefault(t *testing.T) {
	for _, tc := range []struct{ name, metric, threshold string }{
		{"only the metric", "rmse", ""},
		{"only the threshold", "", "0.35"},
		{"a threshold that is not a number", "rmse", "as low as possible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.ready()
			// A criteria file that names no metric, so the run's own tags are what would
			// be used if they were usable.
			h.write("evaluation.yaml", "rationale: still being decided\n")
			h.commit("Empty the criteria")

			launched := h.launch()
			if tc.metric != "" {
				h.mlflow.SetTag(t, launched.RunID, "evaluation_metric", tc.metric)
			}
			if tc.threshold != "" {
				h.mlflow.SetTag(t, launched.RunID, "evaluation_threshold", tc.threshold)
			}
			h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
			h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

			summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
			if err != nil {
				t.Fatalf("results: %v", err)
			}
			criterion := summary.EvaluationCriteria
			if criterion.Met.Known() {
				t.Fatalf("met = %v, want no verdict from half a criterion", criterion.Met)
			}
			if criterion.Threshold != nil {
				t.Errorf("threshold = %v, want none invented", *criterion.Threshold)
			}
			if criterion.Met.Status().Reason != experiments.ReasonNoCriterionStated {
				t.Errorf("reason = %q, want no_criterion_stated",
					criterion.Met.Status().Reason)
			}
		})
	}
}

// An object with a status this package never writes is normalised rather than
// carried through, so a reader cannot end up switching on a word ODE does not use.
func TestAnUnrecognisedVerdictObjectIsNormalised(t *testing.T) {
	var verdict experiments.Verdict
	if err := json.Unmarshal([]byte(`{"status":"whatever","reason":"made_up"}`),
		&verdict); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if verdict.Known() {
		t.Fatal("an unrecognised object was read as a verdict")
	}
	if verdict.Status().Status != experiments.NotComputedStatus {
		t.Errorf("status = %q, want it normalised", verdict.Status().Status)
	}
}
