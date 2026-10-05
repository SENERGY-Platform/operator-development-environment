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
	"time"

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

// --- the D37 addendum's three document-level keys ---

// The flat form: target_series, prediction_field and resolution beside an
// ordinary flat metric/threshold/goal.
func TestTheThreeEvaluationKeysReadInFlatForm(t *testing.T) {
	document, err := experiments.ParseCriteria(`metric: mae
goal: minimise
threshold: 30.0

target_series: sensor.ENERGY.Power
prediction_field: prediction
resolution: 1h
`)
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.TargetSeries != "sensor.ENERGY.Power" {
		t.Errorf("target_series = %q", document.TargetSeries)
	}
	if document.PredictionField != "prediction" {
		t.Errorf("prediction_field = %q", document.PredictionField)
	}
	if document.Resolution != "1h" {
		t.Errorf("resolution = %q", document.Resolution)
	}
}

// The three keys are document-level, so they still read when the criterion
// itself is written in the restructured *list* form — a developer who moved
// their metric into a `criteria:` list had no reason to move these three too.
func TestTheThreeEvaluationKeysReadBesideAListFormCriterion(t *testing.T) {
	document, err := experiments.ParseCriteria(`criteria:
  - metric: val_rmse
    threshold: 0.35
    direction: minimize

target_series: sensor.ENERGY.Power
prediction_field: prediction
resolution: 1h
`)
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary == nil || document.Primary.Metric != "val_rmse" {
		t.Fatalf("primary = %+v, want the list form's own entry", document.Primary)
	}
	if document.TargetSeries != "sensor.ENERGY.Power" {
		t.Errorf("target_series = %q, want it read regardless of the criterion's own form",
			document.TargetSeries)
	}
	if document.PredictionField != "prediction" {
		t.Errorf("prediction_field = %q", document.PredictionField)
	}
	if document.Resolution != "1h" {
		t.Errorf("resolution = %q", document.Resolution)
	}
}

// Alternate spellings, the same rule the rest of this file applies to every other
// field: a developer who renamed a key should not be read as having named none.
func TestTheThreeEvaluationKeysReadUnderAlternateSpellings(t *testing.T) {
	document, err := experiments.ParseCriteria(
		"metric: mae\ntarget_topic: sensor.ENERGY.Power\nprediction_key: prediction\n" +
			"join_resolution: 1h\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.TargetSeries != "sensor.ENERGY.Power" {
		t.Errorf("target_series = %q, want the target_topic spelling read", document.TargetSeries)
	}
	if document.PredictionField != "prediction" {
		t.Errorf("prediction_field = %q, want the prediction_key spelling read",
			document.PredictionField)
	}
	if document.Resolution != "1h" {
		t.Errorf("resolution = %q, want the join_resolution spelling read", document.Resolution)
	}
}

// target_series must never be read from the `target` spelling: that spelling is
// already thresholdKeys' own, and a file setting both a threshold and a target
// series is exactly the case that would go silently wrong if the two lists ever
// shared a word.
func TestTargetSeriesDoesNotCollideWithTheThresholdSpellingTarget(t *testing.T) {
	document, err := experiments.ParseCriteria(
		"metric: r2\ntarget: 0.8\ngoal: maximise\ntarget_series: sensor.ENERGY.Power\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.Primary == nil || !document.Primary.HasThreshold || document.Primary.Threshold != 0.8 {
		t.Fatalf("primary = %+v, want `target: 0.8` read as the threshold, untouched",
			document.Primary)
	}
	if document.TargetSeries != "sensor.ENERGY.Power" {
		t.Errorf("target_series = %q, want the explicit key read", document.TargetSeries)
	}
}

// The reverse of the same collision: a file with no target_series at all must not
// read its threshold's `target` spelling as one.
func TestAThresholdNamedTargetIsNotMisreadAsATargetSeries(t *testing.T) {
	document, err := experiments.ParseCriteria("metric: r2\ntarget: 0.8\ngoal: maximise\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.TargetSeries != "" {
		t.Errorf("target_series = %q, want empty: only `target` was set, and that is the "+
			"threshold spelling", document.TargetSeries)
	}
}

// Missing is not an error and is not defaulted to anything — the same rule a
// missing threshold gets.
func TestTheThreeEvaluationKeysAreEmptyRatherThanDefaultedWhenAbsent(t *testing.T) {
	document, err := experiments.ParseCriteria("metric: mae\nthreshold: 30\n")
	if err != nil {
		t.Fatalf("ParseCriteria: %v", err)
	}
	if document.TargetSeries != "" || document.PredictionField != "" || document.Resolution != "" {
		t.Errorf("document = %+v, want all three empty rather than guessed", document)
	}
}

// --- RenderForAssistant, so the developer's target series never reaches the
// assistant while everything else in the file still does ---
//
// The six tests named "Finding N" below are regression tests for an adversarial
// review of the textual redaction this function replaced (see RenderForAssistant's
// own comment in criteria.go for what each finding was). Every one of them
// searches the rendered output for the actual value, never for the wording of an
// error — a test that only checked a message could pass while the value itself
// still leaked.

// The flat form with every field set: the value is gone, the marker is where it
// stood, and every other field is still there with its own value —
// RenderForAssistant has no business near metric, goal, threshold,
// prediction_field or resolution.
func TestRenderForAssistantRendersTheFlatFormWithoutTheTargetSeries(t *testing.T) {
	source := "metric: mae\ngoal: minimise\nthreshold: 30.0\n" +
		"target_series: sensor.ENERGY.Power\nprediction_field: prediction\nresolution: 1h\n"
	rendered, err := experiments.RenderForAssistant(source)
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(rendered, "sensor.ENERGY.Power") {
		t.Errorf("rendered = %q, still carries the value", rendered)
	}
	if !strings.Contains(rendered, experiments.WithheldTargetSeries) {
		t.Errorf("rendered = %q, want the marker where the value stood", rendered)
	}
	for _, want := range []string{
		"metric: mae", "goal: minimise", "threshold: 30",
		"prediction_field: prediction", "resolution: 1h",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered = %q, lost %q", rendered, want)
		}
	}
}

// The other two spellings are the same key as far as ParseCriteria is concerned,
// so they render exactly like target_series does.
func TestRenderForAssistantCoversAllThreeTargetSeriesSpellings(t *testing.T) {
	base, err := experiments.RenderForAssistant("metric: mae\ntarget_series: sensor.ENERGY.Power\n")
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	for _, key := range []string{"target_topic", "ground_truth_series"} {
		t.Run(key, func(t *testing.T) {
			rendered, err := experiments.RenderForAssistant(
				"metric: mae\n" + key + ": sensor.ENERGY.Power\n")
			if err != nil {
				t.Fatalf("RenderForAssistant: %v", err)
			}
			if rendered != base {
				t.Errorf("%s rendered = %q, want the same document target_series produces: %q",
					key, rendered, base)
			}
		})
	}
}

// Finding 2: targetSeriesKeyColon used to compare a key case-sensitively, and
// because ParseCriteria itself is case-sensitive too, a key spelled Target_Series
// was never read as target_series by either the old redaction or the parser it
// shadowed — the value under it never became CriteriaDocument.TargetSeries at
// all. RenderForAssistant only ever writes what ParseCriteria understood, so a
// value sitting under a key the parser does not recognise has no field to reach
// the output through, by construction rather than by a case fix.
func TestRenderForAssistantHidesAValueUnderAKeyTheParserDoesNotRecognise(t *testing.T) {
	rendered, err := experiments.RenderForAssistant(
		"metric: mae\nTarget_Series: sensor.ENERGY.Power\n")
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(rendered, "sensor.ENERGY.Power") {
		t.Errorf("rendered = %q, a wrongly-cased key's value reached the assistant", rendered)
	}
}

// Finding 3: a target_series written inside a secondary_metrics list item is not
// read as a metric — specOf finds none of itemMetricKeys there — and it is also
// not the document-level TargetSeries, which ParseCriteria only ever reads at
// the document's root. Neither reading puts the value into the parsed document,
// so there is nothing for RenderForAssistant to echo.
func TestRenderForAssistantHidesAValueNestedInsideASecondaryMetricsItem(t *testing.T) {
	rendered, err := experiments.RenderForAssistant(
		"metric: mae\nsecondary_metrics:\n  - target_series: sensor.ENERGY.Power\n")
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(rendered, "sensor.ENERGY.Power") {
		t.Errorf("rendered = %q, a value nested under secondary_metrics reached the assistant",
			rendered)
	}
}

// Finding 4: the developer's own line wrap splits the value across a folded
// rationale's line break. ParseCriteria folds the two lines with the single
// space YAML itself inserts at that break, so the parsed rationale never holds
// the value as one contiguous run in the first place — rendering from the parsed
// document rather than the raw bytes is what makes that true, not a search
// clever enough to bridge a line break a raw-text scan never could.
func TestRenderForAssistantHidesAValueSplitAcrossAFoldedRationaleLineBreak(t *testing.T) {
	source := "metric: mae\ntarget_series: sensor.ENERGY.Power\n" +
		"rationale: >\n  This operator targets sensor.ENERGY.\n  Power specifically.\n"
	rendered, err := experiments.RenderForAssistant(source)
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(rendered, "sensor.ENERGY.Power") {
		t.Errorf("rendered = %q, the split value reassembled in the output", rendered)
	}
}

// Finding 5: the value pass is case-insensitive, so a differently-cased
// repetition in rationale is caught too — the raw-text value pass this replaced
// compared case-sensitively and missed exactly this.
func TestRenderForAssistantValuePassIsCaseInsensitive(t *testing.T) {
	source := "metric: mae\ntarget_series: sensor.ENERGY.Power\n" +
		"rationale: >\n  Formerly known as SENSOR.ENERGY.POWER on the old bus.\n"
	rendered, err := experiments.RenderForAssistant(source)
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(strings.ToLower(rendered), "sensor.energy.power") {
		t.Errorf("rendered = %q, a differently-cased repetition of the value survived", rendered)
	}
	if !strings.Contains(rendered, "Formerly known as") {
		t.Errorf("rendered = %q, the rest of the sentence was lost with it", rendered)
	}
}

// Finding 6: the value pass only replaces a target series at a word boundary, so
// a metric name that merely contains it as a substring is untouched —
// target_series: power must not turn metric: power_mae into
// metric: <withheld…>_mae, which the raw-text ReplaceAll this replaced did.
func TestRenderForAssistantValuePassRespectsWordBoundaries(t *testing.T) {
	rendered, err := experiments.RenderForAssistant("metric: power_mae\ntarget_series: power\n")
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if !strings.Contains(rendered, "metric: power_mae") {
		t.Errorf("rendered = %q, want power_mae unmutilated: power is a substring of it, "+
			"not a whole word beside it", rendered)
	}
}

// A second adversarial review found the word-boundary rule's own gap: `.`, `-`,
// `:` and `/` used to continue an identifier or a dotted platform path
// unconditionally, so a value immediately followed by a sentence-ending period —
// "… is sensor.ENERGY.Power." — was read as adjoining a continuation character
// and left unmasked, in the developer's own document worded exactly that way.
// wordBoundaryChar now treats those four as a continuation only when the
// character beyond them, one step further in the same direction, is itself an
// identifier character; `_` stays an unconditional continuation, unaffected
// (TestRenderForAssistantValuePassRespectsWordBoundaries above still has to pass
// with power_mae unmutilated). Every case here checks the value itself in the
// rendered text, not the wording of the masking marker.
func TestRenderForAssistantValuePassAtASentencesPunctuationAndAtTheFieldsEdges(t *testing.T) {
	const target = "sensor.ENERGY.Power"
	cases := []struct {
		name      string
		rationale string
		masked    bool
	}{
		{"a sentence-ending period", "The series graded is sensor.ENERGY.Power.", true},
		{"a dotted continuation after the value",
			"The series graded is sensor.ENERGY.Power.Max.", false},
		{"a longer identifier with no separator",
			"The series graded is sensor.ENERGY.PowerMax.", false},
		{"the value at the very end of the field",
			"The series graded is sensor.ENERGY.Power", true},
		{"the value at the very start of the field",
			"sensor.ENERGY.Power is the series graded.", true},
		{"parenthesised", "The series graded is (sensor.ENERGY.Power).", true},
		{"semicolon-terminated",
			"The series graded is sensor.ENERGY.Power; nothing else.", true},
		{"quoted", `The series graded is "sensor.ENERGY.Power".`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := "metric: mae\ntarget_series: " + target +
				"\nrationale: >\n  " + tc.rationale + "\n"
			rendered, err := experiments.RenderForAssistant(source)
			if err != nil {
				t.Fatalf("RenderForAssistant: %v", err)
			}
			present := strings.Contains(rendered, target)
			if tc.masked && present {
				t.Errorf("rendered = %q, want %q masked", rendered, target)
			}
			if !tc.masked && !present {
				t.Errorf("rendered = %q, want %q left unmutilated", rendered, target)
			}
		})
	}
}

// The three-character floor named in valuePass's own comment: a target series
// shorter than three characters is left alone rather than searched for, because a
// short fragment turns up inside ordinary prose too often to redact safely
// without doing more damage than it prevents. Pinned by its own test, named as
// what it is — a deliberate, documented limit — so a future reader does not read
// "pv" surviving in rationale as a bug and tighten the boundary check to catch it.
func TestRenderForAssistantLeavesATargetSeriesUnderThreeCharactersUnmasked(t *testing.T) {
	source := "metric: mae\ntarget_series: pv\nrationale: >\n  The series graded is pv.\n"
	rendered, err := experiments.RenderForAssistant(source)
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if !strings.Contains(rendered, "The series graded is pv.") {
		t.Errorf("rendered = %q, want the two-character target left in rationale: this is "+
			"a named limit (see valuePass's own comment), not a bug to fix here", rendered)
	}
}

// The positive case beside finding 6: a value that stands as a whole word, with
// nothing joined to either side of it, is exactly what the value pass exists to
// catch. Ended with a comma deliberately, so this test stays independent of the
// sentence-end case above
// (TestRenderForAssistantValuePassAtASentencesPunctuationAndAtTheFieldsEdges): a
// comma was never one of the characters wordBoundaryChar treats as a possible
// continuation, so it proves nothing about the conditional `.-:/` rule that case
// exercises.
func TestRenderForAssistantReplacesAWholeWordOccurrenceInRationale(t *testing.T) {
	source := "metric: mae\ntarget_series: sensor.ENERGY.Power\n" +
		"rationale: >\n  This operator's baseline is graded against sensor.ENERGY.Power, " +
		"the platform's main meter.\n"
	rendered, err := experiments.RenderForAssistant(source)
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(rendered, "sensor.ENERGY.Power") {
		t.Errorf("rendered = %q, the value survived a whole-word mention in rationale", rendered)
	}
	if !strings.Contains(rendered, experiments.WithheldTargetSeries) {
		t.Errorf("rendered = %q, want the marker where rationale named the value", rendered)
	}
}

// The scaffold's own evaluation.yaml names no target series at all. The marker
// has to stand there anyway: "set" and "not set" must not be distinguishable
// from where the assistant sits, or the absence itself becomes the answer to the
// choice §5.2 leaves to it (D38).
func TestRenderForAssistantMarksTheTargetSeriesEvenWhenTheFileNamesNone(t *testing.T) {
	source := "metric: baseline\ngoal: minimise\nthreshold: 0.0\n" +
		"secondary_metrics: []\ntarget_series:\nprediction_field: prediction\nresolution: 1h\n"
	rendered, err := experiments.RenderForAssistant(source)
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if !strings.Contains(rendered, "target_series: "+experiments.WithheldTargetSeries) {
		t.Errorf("rendered = %q, want the marker even though the file set nothing", rendered)
	}
}

// A criterion with no threshold carries no threshold line, rather than a
// defaulted zero that would read as a value nobody wrote.
func TestRenderForAssistantOmitsThresholdWhenTheFileNamesNone(t *testing.T) {
	rendered, err := experiments.RenderForAssistant("metric: rmse\ngoal: minimise\n")
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(rendered, "threshold:") {
		t.Errorf("rendered = %q, want no threshold line: the file named none", rendered)
	}
}

// A direction that is only inferred from the metric's name, never stated by the
// file, does not appear either — an inferred goal would read as the developer's
// own statement, which GoalStated exists to tell apart.
func TestRenderForAssistantOmitsGoalWhenOnlyInferred(t *testing.T) {
	rendered, err := experiments.RenderForAssistant("metric: rmse\nthreshold: 0.4\n")
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if strings.Contains(rendered, "goal:") {
		t.Errorf("rendered = %q, want no goal line: rmse's direction is inferred, not stated",
			rendered)
	}
}

// The list form of a criterion renders exactly like the flat form that states
// the same thing — RenderForAssistant reads the parsed CriteriaDocument, which
// does not remember which shape produced it.
func TestRenderForAssistantRendersTheListFormLikeTheFlatForm(t *testing.T) {
	flat, err := experiments.RenderForAssistant("metric: rmse\ngoal: minimise\nthreshold: 0.4\n")
	if err != nil {
		t.Fatalf("RenderForAssistant(flat): %v", err)
	}
	list, err := experiments.RenderForAssistant(
		"criteria:\n  - metric: rmse\n    goal: minimise\n    threshold: 0.4\n")
	if err != nil {
		t.Fatalf("RenderForAssistant(list): %v", err)
	}
	if flat != list {
		t.Errorf("list form rendered = %q, want the same as the flat form %q", list, flat)
	}
}

// A file ParseCriteria cannot read is refused whole rather than rendered in
// part, the same fail-closed rule this package applies to a status elsewhere
// (criteriaGitFailure) — there is no safe partial answer once the document is
// not known.
func TestRenderForAssistantFailsClosedOnAnUnparseableFile(t *testing.T) {
	rendered, err := experiments.RenderForAssistant("metric: rmse\nnested:\n\tthreshold: 0.3\n")
	if err == nil {
		t.Fatal("an unparseable file was rendered instead of refused")
	}
	if rendered != "" {
		t.Errorf("rendered = %q, want empty alongside the error", rendered)
	}
}

// RenderForAssistant's own output has to be readable by the function it stands
// in front of — otherwise a read_file call would hand the assistant YAML that
// ODE itself could not read back on the next one.
func TestRenderForAssistantOutputParsesAgain(t *testing.T) {
	source := "metric: mae\ngoal: minimise\nthreshold: 30.0\n" +
		"secondary_metrics:\n  - metric: mape\n    threshold: 0.2\n    goal: minimise\n" +
		"target_series: sensor.ENERGY.Power\nprediction_field: prediction\nresolution: 1h\n" +
		"rationale: >\n  Replace the metric and threshold with the ones this operator is\n" +
		"  actually for.\n"
	rendered, err := experiments.RenderForAssistant(source)
	if err != nil {
		t.Fatalf("RenderForAssistant: %v", err)
	}
	if _, err := experiments.ParseCriteria(rendered); err != nil {
		t.Fatalf("ParseCriteria(RenderForAssistant(x)) = %v, want it to read its own output "+
			"back without error\nrendered:\n%s", err, rendered)
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
	if !strings.Contains(status.Detail, "on their behalf") ||
		!strings.Contains(status.Detail, "next connected") {
		t.Errorf("detail = %q, want it to say why a background summary has no token: the "+
			"file is read on the developer's behalf, so it waits for them", status.Detail)
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

// --- the D37 addendum: grading prefers Operator Lib's own post-replay value ---

// Each test below sets operator_lib.history_end / operator_lib.test_end to match
// the launch's own split, which is the same recipe split_test.go's
// TestASummaryConfirmsASplitWhoseRunTagsMatch uses to get summary.Split.Confirmed
// to read "confirmed".

// All three conditions holding: the split is confirmed, the run says it computed
// exactly this criterion's metric, and the value parses. The library's own number
// wins over the run's ordinary metrics map, and the Source line says so.
func TestGradeUsesTheLibrarysValueWhenConfirmedComputedAndNameMatches(t *testing.T) {
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
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "3.5")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_n", "120")
	// The run's own metrics map disagrees on purpose: if this value shows up
	// instead, the override did not take.
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"mae": 999})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Value == nil || *criterion.Value != 3.5 {
		t.Fatalf("value = %v, want the library's own 3.5, not the run's metrics map",
			criterion.Value)
	}
	if !criterion.Met.IsMet() {
		t.Errorf("criterion = %+v, want 3.5 under a minimised 5.0 read as met", criterion)
	}
	if !strings.Contains(criterion.Source, "post-replay") ||
		!strings.Contains(criterion.Source, "120") {
		t.Errorf("source = %q, want it to say the value came from the library's own "+
			"replay and to name the sample size", criterion.Source)
	}
}

// The name does not match: Operator Lib computed a different metric than the one
// this criterion names, so its number is not this criterion's answer.
func TestGradeFallsBackToMetricsMapWhenTheMetricNameDoesNotMatch(t *testing.T) {
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
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "rmse")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "3.5")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"mae": 4.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Value == nil || *criterion.Value != 4.9 {
		t.Fatalf("value = %v, want the run's own mae (4.9): the library computed rmse, "+
			"not this criterion's metric", criterion.Value)
	}
	if strings.Contains(criterion.Source, "post-replay") {
		t.Errorf("source = %q, want the ordinary file source: the name did not match",
			criterion.Source)
	}
}

// The status is not "computed": whatever value the run carries under
// evaluation.metric_value is not one to trust, whatever its name says.
func TestGradeFallsBackToMetricsMapWhenTheStatusIsNotComputed(t *testing.T) {
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
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "target_series_ambiguous")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "3.5")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"mae": 4.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Value == nil || *criterion.Value != 4.9 {
		t.Fatalf("value = %v, want the run's own mae (4.9): the library did not report "+
			"\"computed\"", criterion.Value)
	}
}

// The split was not confirmed by the run's own tags — a repository whose Operator
// Lib pin is older than v1.7.0, or a rewritten tag. Even a run whose params
// claim "computed" under the right name must not be trusted here: the params are
// as writable as the tags are, and the confirmation is the one check that reads
// something the job cannot forge into matching (D37).
func TestGradeFallsBackToMetricsMapWhenTheSplitIsNotConfirmed(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\ngoal: minimise\nthreshold: 5.0\n")
	h.commit("State the real criterion")

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	launched := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	// No operator_lib.history_end / operator_lib.test_end tags: exactly the older-
	// library symptom split_test.go already covers for the confirmation itself.
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "3.5")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"mae": 4.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.Split == nil || summary.Split.Confirmed == "confirmed" {
		t.Fatalf("split = %+v, want it not confirmed: the test set no matching tags",
			summary.Split)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Value == nil || *criterion.Value != 4.9 {
		t.Fatalf("value = %v, want the run's own mae (4.9): an unconfirmed split must not "+
			"trust the run's own claim of having computed anything", criterion.Value)
	}
}

// A run with no split at all: hasConfirmedSplit is false by construction (there is
// no SplitReport to confirm), so even a run that happens to carry all four params
// — unusual, but not impossible for a hand-crafted fixture — is graded from the
// metrics map exactly as before this channel existed.
func TestGradeFallsBackToMetricsMapWithoutASplitEvenIfTheParamsAreThere(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\ngoal: minimise\nthreshold: 5.0\n")
	h.commit("State the real criterion")

	launched := h.launch()
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "3.5")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"mae": 4.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.Split != nil {
		t.Fatalf("split = %+v, want nil: this launch carried none", summary.Split)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Value == nil || *criterion.Value != 4.9 {
		t.Fatalf("value = %v, want the run's own mae (4.9)", criterion.Value)
	}
}

// A value that does not parse is a contradiction in what the run reported, not a
// reason to grade nothing — the metrics map is still there.
func TestGradeFallsBackToMetricsMapWhenTheLibraryValueDoesNotParse(t *testing.T) {
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
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "not-a-number")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"mae": 4.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	criterion := summary.EvaluationCriteria
	if criterion.Value == nil || *criterion.Value != 4.9 {
		t.Fatalf("value = %v, want the run's own mae (4.9): evaluation.metric_value did "+
			"not parse", criterion.Value)
	}
}

// A secondary metric gets the same preference rule, not only the primary one —
// grade's own precondition is "this criterion's own metric name", not "the
// primary criterion".
func TestGradePrefersTheLibrarysValueForASecondaryMetricToo(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml",
		"metric: rmse\ngoal: minimise\nthreshold: 5.0\nsecondary_metrics: [mae]\n")
	h.commit("Watch mae beside the real criterion")

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	launched := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	h.mlflow.SetTag(t, launched.RunID, "operator_lib.history_end",
		split.TrainingEnd.Format(time.RFC3339))
	h.mlflow.SetTag(t, launched.RunID, "operator_lib.test_end",
		split.TestEnd.Format(time.RFC3339))
	// The library computed mae, not the primary rmse: only the secondary's value is
	// overridden.
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_status", "computed")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_name", "mae")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.metric_value", "1.1")
	h.mlflow.Finish(t, launched.RunID, "FINISHED",
		map[string]float64{"rmse": 2.0, "mae": 999})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.EvaluationCriteria.Value == nil || *summary.EvaluationCriteria.Value != 2.0 {
		t.Errorf("primary value = %v, want the run's own rmse (2.0): the library did not "+
			"compute rmse", summary.EvaluationCriteria.Value)
	}
	if len(summary.SecondaryCriteria) != 1 {
		t.Fatalf("secondary = %+v, want exactly one", summary.SecondaryCriteria)
	}
	if v := summary.SecondaryCriteria[0].Value; v == nil || *v != 1.1 {
		t.Errorf("secondary mae value = %v, want the library's own 1.1, not the run's "+
			"metrics map (999)", v)
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
	criterion, _ := document.ApplyTo(
		map[string]float64{"rmse": 0.31}, nil, nil, "abc1234", nil, false)
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
	criterion, _ = document.ApplyTo(
		map[string]float64{"rmse": 0.31}, nil, nil, "abc1234", nil, false)
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
