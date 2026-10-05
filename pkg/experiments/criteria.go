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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// The developer's evaluation criteria, read and applied (§5.13, M9).
//
// M8 left this deliberately undone and said why: the criteria are the developer's,
// §5.8 denies every tool that could modify them, and turning the file into a
// verdict was M9's work. This is that work, and two properties bound it.
//
//   - **Read, never written.** §5.8 lists "modifying evaluation criteria" among the
//     capabilities with no tool at all, and pkg/tools/repo.go already refuses
//     `write_file` on this path. Nothing here is a way around that: this package
//     has a read-only view of the repository (see the Repository interface), the
//     file is fetched with `git show`, and no code path in ODE writes it. D28 is the
//     same rule from the other side — a recommendation becomes binding only when a
//     developer promotes it into this file themselves.
//
//   - **A criterion that could not be evaluated is not a criterion that failed.**
//     This is D24 applied outside the profiler, and it is the whole reason `Met` is
//     a Verdict rather than a bool. A missing file, a file outside the subset ODE
//     reads, a metric the run never logged and a criterion with no threshold are
//     four different facts, and every one of them would have been flattened to
//     `met: false` by a bool. An assistant reading `met: false` says the run missed
//     the developer's target; an assistant reading `not_computed` with a reason asks
//     for the thing that is missing. The first is a fabricated finding.

// EvaluationCriteriaPath is the file, relative to the repository root. The same
// constant pkg/tools refuses to write, named once here so the two cannot drift.
const EvaluationCriteriaPath = "evaluation.yaml"

// maxCriteriaBytes bounds the read. Two orders of magnitude above the scaffold's
// own file, and small enough that a repository with a large file at this path is
// refused rather than pulled through the kernel.
const maxCriteriaBytes = 64 << 10

// NotComputedStatus is the marker of an explicit non-result.
//
// The same word pkg/profiler writes, on purpose: an assistant reading an ODE
// document should meet one vocabulary for "this could not be determined", not one
// per package. The *reasons* are this domain's own, because the profiler's closed
// set is about series and these are about a file in a repository.
const NotComputedStatus = "not_computed"

// CriterionReason is the closed set of reasons a criterion has no verdict.
// Each names a different repair, which is what makes them worth telling apart.
type CriterionReason string

const (
	// ReasonNoCriteriaFile is a commit with no evaluation.yaml in it. The repair is
	// to scaffold one, and the run is not thereby a failure.
	ReasonNoCriteriaFile CriterionReason = "no_criteria_file"
	// ReasonCriteriaUnreadable is a file ODE could not fetch: no checkout, a
	// checkout of another repository, a commit the working copy no longer has, or
	// git refusing. Distinct from a missing file because nothing here knows whether
	// there is one.
	ReasonCriteriaUnreadable CriterionReason = "criteria_unreadable"
	// ReasonCriteriaUnparseable is a file read whole and outside the subset ODE
	// reads. The detail names the line, so the repair is a specific edit.
	ReasonCriteriaUnparseable CriterionReason = "criteria_unparseable"
	// ReasonNoCriterionStated is a file that parses and names no metric.
	ReasonNoCriterionStated CriterionReason = "no_criterion_stated"
	// ReasonNoThreshold is a criterion naming a metric with nothing to compare it
	// against. The value is still reported; only the verdict is withheld.
	ReasonNoThreshold CriterionReason = "no_threshold"
	// ReasonMetricNotReported is a criterion whose metric the run never logged.
	// The scaffold's own comment warns about exactly this, and it is the case a
	// bool would have turned into "the run missed the target".
	ReasonMetricNotReported CriterionReason = "metric_not_reported"
	// ReasonMetricWithheld is a criterion whose own metric this copy of the summary
	// may not carry: the run logged it after its training phase ended, or under a
	// name the criteria do not declare (D37). The value exists and the developer's
	// own results route has it; what is missing is permission for *this* reader,
	// which is a different fact from a metric the run never logged
	// (ReasonMetricNotReported) and must not be flattened into a failed criterion.
	ReasonMetricWithheld CriterionReason = "metric_withheld"
	// ReasonNoDeveloperCredential is a summary built with no developer token —
	// which is every summary the poller builds, because a background poller has no
	// token and §3.1 item 3 does not let it acquire one. The criteria are read when
	// the developer returns.
	ReasonNoDeveloperCredential CriterionReason = "no_developer_credential"
)

// NotComputed is the explicit non-result, in the shape pkg/profiler writes it.
type NotComputed struct {
	Status string          `json:"status"`
	Reason CriterionReason `json:"reason"`
	Detail string          `json:"detail"`
}

func notComputed(reason CriterionReason, format string, args ...any) NotComputed {
	return NotComputed{
		Status: NotComputedStatus, Reason: reason, Detail: fmt.Sprintf(format, args...),
	}
}

// Verdict is §5.13's `met`: true, false, or an explicit non-result.
//
// It marshals as a bare `true` or `false` when there is a verdict, so §5.13's
// documented shape is what a reader sees in the ordinary case, and as a
// `not_computed` object when there is not. There is deliberately no way to build
// one that says "no verdict" without also saying why — the zero value marshals as
// not_computed rather than as false, so a criterion nobody graded cannot be read
// as a criterion that failed.
type Verdict struct {
	met    bool
	known  bool
	status NotComputed
}

// Met and Unmet are the two verdicts.
func Met() Verdict   { return Verdict{met: true, known: true} }
func Unmet() Verdict { return Verdict{known: true} }

// NotEvaluated is the third answer, which a bool could not carry.
func NotEvaluated(reason CriterionReason, format string, args ...any) Verdict {
	return Verdict{status: notComputed(reason, format, args...)}
}

// Known reports whether there is a verdict at all.
func (v Verdict) Known() bool { return v.known }

// IsMet is the verdict, and is only meaningful when Known.
func (v Verdict) IsMet() bool { return v.known && v.met }

// Status describes the non-result. An unset Verdict reports out of scope rather
// than an empty reason, for the reason profiler.Value does: a field nobody
// populated is exactly that, and saying so beats saying nothing.
func (v Verdict) Status() NotComputed {
	if v.known {
		return NotComputed{}
	}
	if v.status.Status == "" {
		return notComputed(ReasonNoCriterionStated, "nothing evaluated this criterion")
	}
	return v.status
}

func (v Verdict) MarshalJSON() ([]byte, error) {
	if v.known {
		return json.Marshal(v.met)
	}
	return json.Marshal(v.Status())
}

func (v *Verdict) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.Equal(trimmed, []byte("true")):
		*v = Met()
		return nil
	case bytes.Equal(trimmed, []byte("false")):
		*v = Unmet()
		return nil
	case bytes.Equal(trimmed, []byte("null")):
		// Nothing in ODE writes null. A hand-edited fixture might, and reading it as
		// a computed false is the exact confusion this type exists to prevent.
		*v = NotEvaluated(ReasonNoCriterionStated, "the verdict was null on read")
		return nil
	}
	var status NotComputed
	if err := json.Unmarshal(trimmed, &status); err != nil {
		return err
	}
	if status.Status != NotComputedStatus {
		// Only a hand-written fixture can get here, and normalising it is still worth
		// doing: a status this package does not write is a document nobody can rely on,
		// and carrying it through unchanged would let a reader switch on a word ODE
		// never produces.
		status = notComputed(ReasonNoCriterionStated,
			"the verdict was an object with no recognised status (%q)", status.Status)
	}
	*v = Verdict{status: status}
	return nil
}

// Goal is which direction counts as better, as the criteria file states it.
const (
	GoalMinimise = "minimise"
	GoalMaximise = "maximise"
)

// CriteriaDocument is `evaluation.yaml` as ODE reads it.
type CriteriaDocument struct {
	// Primary is the metric a run is judged on — §5.13's `evaluation_criteria`.
	Primary *CriterionSpec
	// Secondary are the metrics the file asks to watch beside it. The scaffold's own
	// file has a `secondary_metrics` key, and dropping it silently would lose part of
	// what the developer wrote.
	Secondary []CriterionSpec
	// Rationale is the developer's own words about why the numbers are what they
	// are. Carried because an assistant proposing a change to a run should be able to
	// read what the criterion is *for* before proposing to miss it.
	Rationale string
	// TargetSeries, PredictionField and Resolution are what Operator Lib's own
	// post-replay metric needs once a data split is confirmed (D36, D37): the
	// platform path of the ground truth series, the key infer()'s return dict
	// carries the forecast under, and the bucket both sides are averaged to before
	// the join. All three travel into the deployment config as
	// evaluation_target_series, evaluation_prediction_field and
	// evaluation_resolution (see deployment.go).
	//
	// Empty when the file does not say — nothing here defaults any of the three, the
	// same rule ParseCriteria already keeps for a missing threshold: a guessed
	// series or field would be a value graded against something the developer never
	// named, and the library's own fallback for "one of these is missing" is to
	// compute nothing and say why (evaluation.metric_status).
	TargetSeries    string
	PredictionField string
	Resolution      string
}

// CriterionSpec is one criterion as the file states it, before any run is graded.
type CriterionSpec struct {
	Metric string
	// Threshold is only meaningful when HasThreshold. A criterion naming a metric
	// with no threshold is a legitimate thing to write — "watch this" — and grading
	// it against a defaulted zero would be a verdict nobody asked for.
	Threshold    float64
	HasThreshold bool
	// LowerIsBetter is the direction, and GoalStated says whether the file said so
	// or whether it was inferred from the metric's name. The pair travels into the
	// Criterion, so a reader can see which rule produced the verdict.
	LowerIsBetter bool
	GoalStated    bool
}

// Goal renders the direction for a reader.
func (s CriterionSpec) Goal() string {
	if s.LowerIsBetter {
		return GoalMinimise
	}
	return GoalMaximise
}

// criteriaKeys are the spellings each field is accepted under.
//
// Several rather than one, because §5.11 item 3 scaffolds this file and then it is
// the developer's: they rename, they restructure, and a reader that only knew the
// scaffold's exact words would report a perfectly clear file as having no
// criterion. What is *not* guessed is a threshold or a metric that is not there.
var (
	metricKeys    = []string{"metric", "primary_metric", "target_metric"}
	thresholdKeys = []string{"threshold", "target", "limit"}
	goalKeys      = []string{"goal", "direction", "objective", "optimise", "optimize"}
	criteriaKeys  = []string{"criteria", "evaluation_criteria", "criterion"}
	secondaryKeys = []string{"secondary_metrics", "secondary", "watch", "also_report"}

	// targetSeriesKeys, predictionFieldKeys and resolutionKeys are the spellings
	// for the three settings Operator Lib's own post-replay metric needs (D36,
	// D37). Read the same way as everything else above: several spellings, nothing
	// guessed.
	//
	// target_series deliberately excludes "target" — that spelling is already
	// thresholdKeys' own, read as the number a metric is compared against, and
	// reusing it here would read a developer's threshold as a series path with no
	// test able to see the collision, because most files set only one of the two at
	// a time. Every spelling added to any of the three lists below has to be
	// checked against metricKeys, thresholdKeys, goalKeys, criteriaKeys and
	// secondaryKeys first, for the same reason.
	targetSeriesKeys    = []string{"target_series", "target_topic", "ground_truth_series"}
	predictionFieldKeys = []string{"prediction_field", "prediction_key", "predicted_field"}
	resolutionKeys      = []string{"resolution", "bucket", "join_resolution"}
)

// itemMetricKeys are the spellings a metric may have inside a *list* of criteria,
// where `- name: rmse` is the ordinary way to write one.
//
// `name` is here and deliberately not in metricKeys, because the two positions mean
// different things. At the top level of the document `name:` is the operator's own
// name — the scaffold's operator.yaml has one — and reading it as a metric invented
// a criterion out of a metadata field, which then *displaced* the run's own
// evaluation tags. Inside a list item there is nothing else it could be.
//
// `value` is nowhere. It is the most generic key in YAML and reading it as a
// threshold turned any `value: 12` into a target the developer never set.
var itemMetricKeys = append(append([]string{}, metricKeys...), "name")

// minimiseWords and maximiseWords are how a developer writes a direction.
var (
	minimiseWords = []string{"min", "minimise", "minimize", "lower", "lower_is_better",
		"decrease", "down", "less"}
	maximiseWords = []string{"max", "maximise", "maximize", "higher", "higher_is_better",
		"increase", "up", "more", "greater"}
)

// ParseCriteria reads the document. Exported because the parse is the interesting
// half and is worth testing without a pod, a repository or a cluster behind it.
func ParseCriteria(source string) (CriteriaDocument, error) {
	root, err := parseYAML(source)
	if err != nil {
		return CriteriaDocument{}, err
	}
	if root.kind != yamlMapping {
		return CriteriaDocument{}, fmt.Errorf(
			"the file is not a mapping of keys to values, so there is nothing to read a " +
				"metric and a threshold out of")
	}

	document := CriteriaDocument{
		Rationale: firstText(root, "rationale", "note", "why"),
		// Document-level settings, read once regardless of whether the criterion
		// itself is the flat form or the list form below — target_series,
		// prediction_field and resolution are never per-criterion. Absent stays
		// empty; ParseCriteria defaults nothing here either.
		TargetSeries:    firstText(root, targetSeriesKeys...),
		PredictionField: firstText(root, predictionFieldKeys...),
		Resolution:      firstText(root, resolutionKeys...),
	}

	// A list form first, because a developer who restructured into one meant it to
	// be the whole answer, and the flat keys beside it would then be leftovers.
	for _, key := range criteriaKeys {
		items := root.items(key)
		if len(items) == 0 {
			continue
		}
		for _, item := range items {
			spec, ok := specOf(item, itemMetricKeys)
			if !ok {
				continue
			}
			if document.Primary == nil {
				primary := spec
				document.Primary = &primary
				continue
			}
			document.Secondary = append(document.Secondary, spec)
		}
		break
	}

	if document.Primary == nil {
		// The flat form. metricKeys rather than itemMetricKeys: at the top level a
		// `name:` belongs to the operator, not to a metric.
		if spec, ok := specOf(root, metricKeys); ok {
			primary := spec
			document.Primary = &primary
		}
	}

	for _, key := range secondaryKeys {
		items := root.items(key)
		if len(items) == 0 {
			continue
		}
		for _, item := range items {
			spec, ok := specOf(item, itemMetricKeys)
			if !ok {
				continue
			}
			if document.Primary != nil && spec.Metric == document.Primary.Metric {
				continue
			}
			document.Secondary = append(document.Secondary, spec)
		}
		break
	}

	return document, nil
}

// WithheldTargetSeries stands in place of the developer's target series in every
// RenderForAssistant output, whether or not the file names one at all.
//
// That last clause is the point of a marker over an empty value. An empty value
// would read two different facts as the same text: "the developer has not set
// this yet" — the scaffold's own default — and "something was withheld". A model
// that had learned to tell the two apart by watching for a while would have
// learned the developer's own answer to §5.2 by watching for absence instead of
// asking, which is the exact shortcut this field exists to close. So the marker
// is unconditional: RenderForAssistant writes it into every document it produces,
// named or not, and "set" and "not set" are indistinguishable from where the
// model sits (D38).
const WithheldTargetSeries = "<withheld: the developer's target series>"

// RenderForAssistant renders evaluation.yaml the way ODE understood it — the
// parsed CriteriaDocument, written back out — rather than the bytes on disk, with
// the developer's target series left out of every field it could appear in.
//
// A prior version of this function redacted the raw text instead: a key pass that
// blanked whatever followed a target-series key, and a value pass that replaced
// remaining literal occurrences of the parsed series. Both passes had to
// reimplement, by hand and against raw text, distinctions ParseCriteria had
// already made correctly once — and an adversarial review found five places
// where the hand-rolled version disagreed with the parser it was supposed to be
// shadowing: a key matched case-sensitively where ParseCriteria reads
// `Target_Series:` the same as `target_series:`; a leading `- ` in a list item
// was never stripped, so a target series written inside `secondary_metrics:`
// passed through whole; a folded block scalar's value spans a line break in the
// raw text, which a substring search across the unfolded string never sees as
// one contiguous run; the value pass itself was case-sensitive, so a differently
// cased repetition of the series in rationale survived; and it knew no word
// boundary, so `target_series: power` turned `metric: power_mae` into
// `metric: <withheld…>_mae` — mutilating a field D38 promises is untouched.
//
// Every one of those five is a property of running a second, informal parser
// over text the real one had already read. Rendering from the parsed
// CriteriaDocument instead does not close that list of cases; it removes the
// list, because there is no second reading left to disagree with the first: what
// ParseCriteria did not understand cannot appear in the output at all, whatever
// spelling, nesting or case it used, and rationale is one normalised string by
// the time it reaches this function rather than raw lines a search has to find
// its way across.
//
// What is still a textual search is the value pass below, and deliberately so:
// TargetSeries collapses to one string in the parsed document, and the file can
// say it more than once — in whichever key actually named it, and again in
// rationale, where a developer explaining a decision routinely repeats the value
// they are explaining. valuePass runs case-insensitively and only at a word
// boundary (see its own comment for what counts as one), which is what the
// review's last two findings above asked for and the old value pass did not do.
//
// A file ParseCriteria cannot read is refused whole: there is no safe partial
// rendering of a document this function does not have, the same fail-closed rule
// criteriaGitFailure applies to a status rather than a rendering.
func RenderForAssistant(text string) (string, error) {
	document, err := ParseCriteria(text)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(document.TargetSeries)

	var out strings.Builder
	out.WriteString("# ODE's own reading of evaluation.yaml, not its bytes: comments and\n" +
		"# formatting are not part of this.\n")

	if document.Primary != nil {
		fmt.Fprintf(&out, "metric: %s\n", valuePass(document.Primary.Metric, target))
		if document.Primary.GoalStated {
			fmt.Fprintf(&out, "goal: %s\n", document.Primary.Goal())
		}
		if document.Primary.HasThreshold {
			fmt.Fprintf(&out, "threshold: %s\n", formatThreshold(document.Primary.Threshold))
		}
	}

	if len(document.Secondary) > 0 {
		out.WriteString("secondary_metrics:\n")
		for _, spec := range document.Secondary {
			fmt.Fprintf(&out, "  - metric: %s\n", valuePass(spec.Metric, target))
			if spec.GoalStated {
				fmt.Fprintf(&out, "    goal: %s\n", spec.Goal())
			}
			if spec.HasThreshold {
				fmt.Fprintf(&out, "    threshold: %s\n", formatThreshold(spec.Threshold))
			}
		}
	} else {
		out.WriteString("secondary_metrics: []\n")
	}

	// Always, whether or not the file names one at all (see WithheldTargetSeries).
	fmt.Fprintf(&out, "target_series: %s\n", WithheldTargetSeries)
	fmt.Fprintf(&out, "prediction_field: %s\n", valuePass(document.PredictionField, target))
	fmt.Fprintf(&out, "resolution: %s\n", valuePass(document.Resolution, target))

	if rationale := normaliseWhitespace(document.Rationale); rationale != "" {
		fmt.Fprintf(&out, "rationale: >\n  %s\n", valuePass(rationale, target))
	}

	return out.String(), nil
}

// normaliseWhitespace collapses a parsed scalar to the single line
// RenderForAssistant always writes it back out as. ParseCriteria already does
// this for a folded (`>`) block scalar; a literal (`|`) one keeps its own line
// breaks, and writing those straight into a single continuation line under `>`
// would hand the result back to ParseCriteria as a dedented line the block
// scalar does not own, which fails exactly the round-trip RenderForAssistant is
// required to keep (see its own tests). Collapsing here rather than special
// casing the two block styles is also what backs the package comment's claim
// that rationale is one normalised string once it has been through this
// function: a word-boundary search across it is then reliable in a way it never
// was across the raw file, where the same value could straddle a line break.
func normaliseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// formatThreshold renders a threshold the way a developer would type it.
// strconv.FormatFloat with 'g' would slide into scientific notation past a
// handful of digits, for a number nobody in this file ever writes that way; 'f'
// with no fixed precision prints exactly the digits ParseCriteria's own
// strconv.ParseFloat produced, no trailing noise added.
func formatThreshold(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// valuePass replaces every case-insensitive, word-bounded occurrence of target in
// text with WithheldTargetSeries. It is RenderForAssistant's only textual search,
// run over one already-parsed field at a time rather than over the raw file (see
// RenderForAssistant's own comment for why that distinction is the whole fix).
//
// Case-insensitive, because a developer explaining a decision in rationale does
// not necessarily repeat a series name in the exact case they configured it in.
// Word-bounded, because a substring match inside a longer identifier —
// `target_series: power` beside `metric: power_mae` — would mutilate a field
// D38 promises stays untouched: a match only counts when neither the character
// immediately before it nor the one immediately after is a letter, a digit, `_`,
// or one of `.-:/` *followed by* another identifier character in the same
// direction (see wordBoundaryChar). `_` is a continuation unconditionally;
// `.-:/` are not, so a sentence-ending period after the value — "… is
// sensor.ENERGY.Power." — is a boundary, while the same dot inside
// "sensor.ENERGY.Power.Max" is not. The start and the end of the field are
// themselves always boundaries, so a value that is the whole field still
// matches.
//
// A target shorter than three characters is left alone rather than searched for:
// a two-character fragment turns up inside ordinary prose often enough that
// redacting every occurrence would do more damage to rationale than it prevents.
// This is a named limit, not a claim that nothing shorter can ever leak.
func valuePass(text, target string) string {
	if text == "" || utf8.RuneCountInString(target) < 3 {
		return text
	}

	haystack := []rune(text)
	lowerHaystack := []rune(strings.ToLower(text))
	needle := []rune(strings.ToLower(target))

	var out []rune
	for i := 0; i < len(haystack); {
		end := i + len(needle)
		if end <= len(lowerHaystack) && runesEqual(lowerHaystack[i:end], needle) &&
			!wordBoundaryChar(haystack, i-1, -1) && !wordBoundaryChar(haystack, end, +1) {
			out = append(out, []rune(WithheldTargetSeries)...)
			i = end
			continue
		}
		out = append(out, haystack[i])
		i++
	}
	return string(out)
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// wordBoundaryChar reports whether runes[i] continues an identifier or a dotted
// platform path, approached from dir (-1 for the character before a match, +1
// for the one after), so that a match adjoining it is a fragment rather than a
// whole word. An index before the first rune or at/after the last is never one
// of these characters — the start and the end of a field are boundaries by
// definition, with nothing there to check.
//
// A letter, a digit or `_` is always a continuation, unconditionally — `_` has
// to be, or a target series of `power` would leave `power_mae` half-withheld
// (found by the first adversarial review). `.`, `-`, `:` and `/` continue only
// when the character *beyond* them, one step further in the same direction, is
// itself an identifier character: `sensor.ENERGY.Power.Max` keeps the dot as a
// continuation because `Max` follows it, while `… is sensor.ENERGY.Power.` does
// not, because nothing follows the sentence-ending period. Before this, every
// one of the four was a continuation unconditionally, so a value at a sentence's
// end was never masked at all — found by a second adversarial review. Checked
// once per side of a match, with dir carrying which side, rather than as one
// direction-blind rule: the character before a match and the one after it are
// examined by looking further in opposite directions.
func wordBoundaryChar(runes []rune, i, dir int) bool {
	if i < 0 || i >= len(runes) {
		return false
	}
	r := runes[i]
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return true
	}
	switch r {
	case '.', '-', ':', '/':
		return identifierChar(runes, i+dir)
	default:
		return false
	}
}

// identifierChar reports whether runes[i] is a letter, a digit or `_` — the
// characters wordBoundaryChar looks for beyond a `.`, `-`, `:` or `/` to decide
// whether that punctuation continues an identifier or ends one. Out of range is
// never one of these, the same as wordBoundaryChar's own out-of-range answer.
func identifierChar(runes []rune, i int) bool {
	if i < 0 || i >= len(runes) {
		return false
	}
	r := runes[i]
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// specOf reads one criterion out of a node, which may be a mapping or a bare
// metric name.
func specOf(node *yamlNode, metricNames []string) (CriterionSpec, bool) {
	if node == nil {
		return CriterionSpec{}, false
	}
	if node.kind == yamlScalar {
		// A bare name in `secondary_metrics`. A metric with no threshold, which is a
		// real thing to write and is graded as such: reported, never judged.
		metric := strings.TrimSpace(node.scalar)
		if metric == "" {
			return CriterionSpec{}, false
		}
		return CriterionSpec{Metric: metric, LowerIsBetter: lowerIsBetter(metric)}, true
	}
	if node.kind != yamlMapping {
		return CriterionSpec{}, false
	}

	metric := firstText(node, metricNames...)
	if metric == "" {
		return CriterionSpec{}, false
	}
	spec := CriterionSpec{Metric: metric}

	for _, key := range thresholdKeys {
		raw := node.text(key)
		if raw == "" {
			continue
		}
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			// A threshold that is not a number is not a threshold. Left unset rather
			// than defaulted, so the criterion reports no_threshold instead of being
			// graded against zero.
			continue
		}
		spec.Threshold, spec.HasThreshold = parsed, true
		break
	}

	stated := firstText(node, goalKeys...)
	if lower, ok := directionOf(stated); ok {
		spec.LowerIsBetter, spec.GoalStated = lower, true
	} else {
		spec.LowerIsBetter = lowerIsBetter(metric)
	}
	return spec, true
}

// directionOf reads a stated goal.
func directionOf(stated string) (lowerIsBetter, ok bool) {
	value := strings.ToLower(strings.TrimSpace(stated))
	if value == "" {
		return false, false
	}
	for _, word := range minimiseWords {
		if value == word {
			return true, true
		}
	}
	for _, word := range maximiseWords {
		if value == word {
			return false, true
		}
	}
	return false, false
}

func firstText(node *yamlNode, keys ...string) string {
	for _, key := range keys {
		if value := node.text(key); value != "" {
			return value
		}
	}
	return ""
}

// grade turns one criterion and one run's metrics into §5.13's block.
//
// The metric's value travels with the verdict whether or not there is one, because
// a reader told "the criterion could not be evaluated" and shown the number can
// often see why; and the direction rule travels with it for the reason MetricDelta
// carries LowerIsBetter — a verdict whose rule is invisible reads as a judgement.
func grade(spec CriterionSpec, metrics map[string]float64, source string) Criterion {
	criterion := Criterion{
		Metric:        spec.Metric,
		Goal:          spec.Goal(),
		GoalStated:    spec.GoalStated,
		LowerIsBetter: spec.LowerIsBetter,
		Source:        source,
	}
	if spec.HasThreshold {
		threshold := spec.Threshold
		criterion.Threshold = &threshold
	}

	value, reported := metrics[spec.Metric]
	if reported {
		criterion.Value = &value
	}

	switch {
	case !reported:
		criterion.Met = NotEvaluated(ReasonMetricNotReported,
			"the run logged no %s; it logged %s. A criterion whose metric was never "+
				"recorded is not a criterion the run missed",
			spec.Metric, reportedMetrics(metrics))
	case !spec.HasThreshold:
		criterion.Met = NotEvaluated(ReasonNoThreshold,
			"%s names no threshold for %s, so there is a value (%g) and nothing to "+
				"compare it against", EvaluationCriteriaPath, spec.Metric, value)
	case spec.LowerIsBetter && value <= spec.Threshold,
		!spec.LowerIsBetter && value >= spec.Threshold:
		criterion.Met = Met()
	default:
		criterion.Met = Unmet()
	}
	return criterion
}

// reportedMetrics names what the run did log, in a bounded, ordered list. The
// point is a repair: a criterion on `rmse` against a run logging `val_rmse` is a
// typo the developer can see the moment the names are side by side.
func reportedMetrics(metrics map[string]float64) string {
	if len(metrics) == 0 {
		return "no metrics at all"
	}
	names := make([]string, 0, len(metrics))
	for name := range metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	const listed = 12
	elided := 0
	if len(names) > listed {
		elided = len(names) - listed
		names = names[:listed]
	}
	joined := strings.Join(names, ", ")
	if elided > 0 {
		joined = fmt.Sprintf("%s and %d more", joined, elided)
	}
	return joined
}

// criteriaFor fetches and parses the developer's criteria for one run's commit.
//
// It needs the developer's own token, and that is the crux of M9's design rather
// than an inconvenience. The file lives in a working copy on the developer's PVC,
// reachable only through their Hub pod, and §3.1 item 3 says every read on their
// behalf uses their credential — a background poller has none and must never mint
// one. So a summary built without a token carries `no_developer_credential`, which
// is a fact about the summary rather than a fact about the criterion, and the read
// happens when the developer is back.
//
// Read at the run's commit, not at HEAD. A criterion is part of the code state a
// run came from (§5.11 item 7): grading a six-hour run against a threshold the
// developer edited while it ran would be judging it by a rule it never had.
func (s *Service) criteriaFor(
	ctx context.Context, req Request, record Experiment,
) (CriteriaDocument, *NotComputed) {
	if cached, found := s.criteria.get(req.UserSub, record); found {
		return cached.document, cached.problem
	}
	document, problem := s.readCriteria(ctx, req, record)
	s.criteria.put(req.UserSub, record, document, problem)
	return document, problem
}

// readCriteria is the uncached read: a repository status and a `git show` in the
// developer's pod, then a parse.
func (s *Service) readCriteria(
	ctx context.Context, req Request, record Experiment,
) (CriteriaDocument, *NotComputed) {
	if strings.TrimSpace(req.Bearer) == "" {
		problem := notComputed(ReasonNoDeveloperCredential,
			"this summary was built without a developer credential, so %s was not read; "+
				"every repository read is on behalf of the developer "+
				"and it is read when they are next connected",
			EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}
	if record.CommitSHA == "" {
		problem := notComputed(ReasonCriteriaUnreadable,
			"the run records no commit, so there is no code state to read %s from",
			EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}

	// The workbench the run itself came from, not whichever one the request names:
	// an interpretation may arrive from the poller with no workbench at all, and
	// reading this run's evaluation.yaml out of a different operator's checkout
	// would be worse than not reading it.
	status, err := s.repo.Status(ctx, repo.StatusRequest{
		Request: repo.Request{
			Bearer: req.Bearer, UserSub: req.UserSub, Author: req.Author,
			WorkbenchID: record.WorkbenchID,
		},
	})
	if err != nil {
		problem := notComputed(ReasonCriteriaUnreadable,
			"the working copy could not be read: %v", err)
		return CriteriaDocument{}, &problem
	}
	if !status.Cloned {
		problem := notComputed(ReasonCriteriaUnreadable,
			"there is no working copy on this developer's workspace to read %s from",
			EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}
	if record.Repository != "" && status.Link.FullName != record.Repository {
		// The developer has moved on to another repository. Reported rather than read
		// from whatever is checked out now: an evaluation.yaml from a different project
		// is not this run's criterion, and grading against one would be worse than not
		// grading at all.
		problem := notComputed(ReasonCriteriaUnreadable,
			"this run is from %s and the workspace now holds %s, so its %s is not here",
			record.Repository, status.Link.FullName, EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}

	// `git show <commit>:<path>`, which reads the committed state directly and needs
	// no checkout of it — so a developer who has moved to another branch since the
	// launch still gets the criterion the run was submitted with.
	result, err := s.workspace.Command(ctx, kernel.Ref{
		Bearer: req.Bearer, Workbench: status.Link.WorkbenchID,
	}, kernel.Command{
		Argv:           []string{"git", "show", record.CommitSHA + ":" + EvaluationCriteriaPath},
		Dir:            status.Link.Path,
		Timeout:        s.opts.CommandTimeout,
		MaxOutputBytes: maxCriteriaBytes,
	})
	if err != nil {
		problem := notComputed(ReasonCriteriaUnreadable,
			"%s could not be read from the workspace: %v", EvaluationCriteriaPath, err)
		return CriteriaDocument{}, &problem
	}
	if result.ExitCode != 0 || result.TimedOut {
		problem := criteriaGitFailure(record.CommitSHA, result)
		return CriteriaDocument{}, &problem
	}
	if result.Truncated {
		problem := notComputed(ReasonCriteriaUnreadable,
			"%s at %s is larger than the %d bytes ODE reads, so it was not parsed rather "+
				"than parsed in part",
			EvaluationCriteriaPath, shortSHA(record.CommitSHA), maxCriteriaBytes)
		return CriteriaDocument{}, &problem
	}

	document, err := ParseCriteria(result.Stdout)
	if err != nil {
		problem := notComputed(ReasonCriteriaUnparseable,
			"%s at %s is outside the YAML subset ODE reads (%v). It is your file and ODE "+
				"does not write it; simplifying the shape is what makes it readable",
			EvaluationCriteriaPath, shortSHA(record.CommitSHA), err)
		return CriteriaDocument{}, &problem
	}
	return document, nil
}

// criteriaGitFailure tells a missing file apart from a commit that is not here.
//
// The two look the same from an exit code and are different facts with different
// repairs, which is the distinction D24 asks to keep. git's own wording is what
// separates them, so it is matched rather than guessed at — and where it matches
// neither, the answer is "unreadable" with git's message rather than a guess at
// "missing".
func criteriaGitFailure(commitSHA string, result kernel.CommandResult) NotComputed {
	stderr := strings.ToLower(firstLine(result.Stderr))
	switch {
	case result.TimedOut:
		return notComputed(ReasonCriteriaUnreadable,
			"reading %s at %s from the workspace timed out",
			EvaluationCriteriaPath, shortSHA(commitSHA))
	case strings.Contains(stderr, "does not exist"),
		strings.Contains(stderr, "exists on disk, but not in"),
		strings.Contains(stderr, "path '"+EvaluationCriteriaPath+"' does not exist"):
		return notComputed(ReasonNoCriteriaFile,
			"the commit %s has no %s. The scaffold writes one; until there is one, "+
				"ODE has no criterion to grade this run against and does not invent one",
			shortSHA(commitSHA), EvaluationCriteriaPath)
	case strings.Contains(stderr, "unknown revision"),
		strings.Contains(stderr, "bad object"),
		strings.Contains(stderr, "invalid object name"),
		strings.Contains(stderr, "not a valid object name"):
		return notComputed(ReasonCriteriaUnreadable,
			"the working copy does not have commit %s, so its %s cannot be read; the "+
				"branch it was on may have been deleted or rewritten",
			shortSHA(commitSHA), EvaluationCriteriaPath)
	default:
		return notComputed(ReasonCriteriaUnreadable,
			"git could not read %s at %s: %s",
			EvaluationCriteriaPath, shortSHA(commitSHA), firstLine(result.Stderr))
	}
}

// criteriaCache memoises what a commit's evaluation.yaml says.
//
// It exists because reading one is not cheap: a repository status and a `git show`,
// both of them commands executed in the developer's Hub pod, and §5.13's summary is
// now read by a pane, by a tool call and by every interpretation. Four pod commands
// per read of a document that cannot have changed is a cost with nothing to show
// for it.
//
// What makes a cache correct here rather than a source of stale answers is that the
// key is a **commit**. The tree at a commit is immutable by construction, so the
// file at that path either was there or was not, and it either parsed or did not.
// The developer editing evaluation.yaml produces a new commit and a new key; a
// force-push that removes the commit produces a read failure, which is not cached.
//
// So only the immutable outcomes are kept: the parsed document, a commit that has
// no such file, and a file outside the subset ODE reads. Everything under
// `criteria_unreadable` and `no_developer_credential` is a fact about *this
// moment* — no checkout yet, another repository selected, the pod not up, no token
// on the request — and caching one would turn a transient condition into a
// permanent answer.
type criteriaCache struct {
	mux     sync.Mutex
	entries map[string]criteriaEntry
}

type criteriaEntry struct {
	document CriteriaDocument
	problem  *NotComputed
}

// maxCachedCriteria bounds it. One entry per developer per commit they have
// launched from, which in practice is a handful; the cap is what stops a long-lived
// process accumulating one per commit in a repository's history.
const maxCachedCriteria = 256

func (c *criteriaCache) key(userSub string, record Experiment) string {
	// The subject is in the key, not merely checked: two developers on the same
	// commit of the same repository legitimately have different working copies, and
	// a key without it would serve one of them the other's file.
	return userSub + "\x00" + record.Repository + "\x00" + record.CommitSHA
}

func (c *criteriaCache) get(userSub string, record Experiment) (criteriaEntry, bool) {
	if record.CommitSHA == "" {
		return criteriaEntry{}, false
	}
	c.mux.Lock()
	defer c.mux.Unlock()
	entry, found := c.entries[c.key(userSub, record)]
	return entry, found
}

func (c *criteriaCache) put(
	userSub string, record Experiment, document CriteriaDocument, problem *NotComputed,
) {
	if record.CommitSHA == "" {
		return
	}
	if problem != nil && !cacheableProblem(problem.Reason) {
		return
	}

	c.mux.Lock()
	defer c.mux.Unlock()
	if c.entries == nil {
		c.entries = map[string]criteriaEntry{}
	}
	if len(c.entries) >= maxCachedCriteria {
		// Cleared rather than evicted one by one. A miss costs a re-read of a file
		// that is a few hundred bytes, and an LRU here would be machinery in aid of
		// nothing — the working set is a developer's recent commits.
		c.entries = map[string]criteriaEntry{}
	}
	c.entries[c.key(userSub, record)] = criteriaEntry{document: document, problem: problem}
}

// cacheableProblem is the distinction the cache turns on: a property of the
// commit, or a property of this moment.
func cacheableProblem(reason CriterionReason) bool {
	switch reason {
	case ReasonNoCriteriaFile, ReasonCriteriaUnparseable, ReasonNoCriterionStated:
		return true
	default:
		return false
	}
}
