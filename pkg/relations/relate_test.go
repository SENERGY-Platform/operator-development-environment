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

package relations

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// The rule detector is checked against fixtures with known answers rather than
// against the platform, for the reason SPEC §5.4.14 gives about the profiler's
// detectors: an association found in synthetic oven-and-lights data is a check, and
// one found in the platform's kitchen is an anecdote.
//
// The fixture is the motivating case of §5.5, built so every figure in it can be
// worked out by hand. Over 30 days at a 15-minute grid:
//
//   - the oven runs 19:00–22:00 every evening (12 buckets a day) and 10:00–10:30
//     every morning (2 buckets a day)
//   - the kitchen lights are on for the evening run and not for the morning one
//
// So "the oven active → the kitchen lights active" holds 12 times in 14, which is a
// confidence of 6/7, and it fails entirely in the 06:00–12:00 bucket — the "except at
// certain times of day" the developer described.

const (
	testDays    = 30
	perDay      = 96 // buckets of 15 minutes
	ovenActiveH = 12 + 2
)

var fixtureStart = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func ovenRef() profiler.SeriesRef {
	return profiler.SeriesRef{DeviceID: "dev-oven", ServiceID: "svc-oven", VariablePath: "value.power"}
}

func lightsRef() profiler.SeriesRef {
	return profiler.SeriesRef{DeviceID: "dev-lights", ServiceID: "svc-lights", VariablePath: "value.power"}
}

// kitchenFixture builds the grid and the two state series.
func kitchenFixture() ([]time.Time, []StateSeries) {
	times := make([]time.Time, 0, testDays*perDay)
	for i := 0; i < testDays*perDay; i++ {
		times = append(times, fixtureStart.Add(time.Duration(i)*15*time.Minute))
	}

	oven := make([]State, len(times))
	lights := make([]State, len(times))
	for i, at := range times {
		evening := at.Hour() >= 19 && at.Hour() < 22
		morning := at.Hour() == 10 && at.Minute() < 30

		oven[i] = StateIdle
		if evening || morning {
			oven[i] = StateActive
		}
		lights[i] = StateIdle
		if evening {
			lights[i] = StateActive
		}
	}

	return times, []StateSeries{
		stateSeries("the oven", ovenRef(), oven),
		stateSeries("the kitchen lights", lightsRef(), lights),
	}
}

// stateSeries wraps a state slice as a usable member, with the bucket counts filled
// in the way DeriveState would.
func stateSeries(label string, ref profiler.SeriesRef, states []State) StateSeries {
	summary := StateSummary{
		Usable:          true,
		Method:          MethodActivityThreshold,
		Threshold:       100,
		ThresholdSource: ThresholdDetector,
		Classification:  profiler.ActivitySessionBased,
	}
	for _, state := range states {
		switch state {
		case StateActive:
			summary.ActiveBuckets++
		case StateIdle:
			summary.IdleBuckets++
		default:
			summary.UnknownBuckets++
		}
	}
	member := Member{Ref: ref, Label: label, State: summary}
	return StateSeries{Member: member, States: states, Summary: summary}
}

func findRule(rules []CandidateRule, antecedent, consequent string, state State) (CandidateRule, bool) {
	for _, rule := range rules {
		if rule.Antecedent.Label == antecedent &&
			rule.Consequent.Label == consequent &&
			rule.Consequent.State == state {
			return rule, true
		}
	}
	return CandidateRule{}, false
}

func TestTheOvenAndLightsRuleSurfacesWithItsExceptionWindow(t *testing.T) {
	times, series := kitchenFixture()

	_, rules, observed, notes := Relate(times, series, RuleParams{}, DefaultConditioning())

	if observed != testDays*perDay {
		t.Fatalf("observed = %d, want %d: every bucket carries a reading in this fixture",
			observed, testDays*perDay)
	}

	rule, found := findRule(rules, "the oven", "the kitchen lights", StateActive)
	if !found {
		t.Fatalf("the oven/lights rule did not surface; got %d rules: %v", len(rules), statements(rules))
	}

	// 12 of every 14 oven-active buckets have the lights on: 6/7 to four places.
	if want := 0.8571; rule.Confidence != want {
		t.Errorf("confidence = %v, want %v (12 of 14 oven-active buckets)", rule.Confidence, want)
	}
	if want := testDays * ovenActiveH; rule.Samples != want {
		t.Errorf("samples = %d, want %d oven-active buckets", rule.Samples, want)
	}
	if want := testDays * 2; rule.Violations != want {
		t.Errorf("violations = %d, want %d — the morning run is the anomaly this rule defines",
			rule.Violations, want)
	}
	// The lights are on 12 buckets in 96, so a base rate of 0.125 and a lift near 6.9.
	if rule.Lift < 6.8 || rule.Lift > 7.0 {
		t.Errorf("lift = %v, want about 6.86 (confidence over a 0.125 base rate)", rule.Lift)
	}

	if !strings.Contains(rule.Statement, "the oven") ||
		!strings.Contains(rule.Statement, "the kitchen lights") ||
		!strings.Contains(rule.Statement, "active") {
		t.Errorf("statement %q does not name both members and the state", rule.Statement)
	}
	// No templated verb: a label is an arbitrary noun phrase and "the kitchen lights
	// is active" is what one produces.
	if strings.Contains(rule.Statement, " is ") {
		t.Errorf("statement %q templates a verb onto a label whose number is unknown", rule.Statement)
	}
	if !strings.Contains(rule.Anomaly, "idle") {
		t.Errorf("anomaly %q should describe the violation, which is the lights being idle", rule.Anomaly)
	}

	// The exception is the point of the whole fixture.
	var morning *Exception
	for i := range rule.Exceptions {
		if rule.Exceptions[i].Dimension == DimensionHourOfDay && rule.Exceptions[i].FromHour == 6 {
			morning = &rule.Exceptions[i]
		}
	}
	if morning == nil {
		t.Fatalf("no 06:00-12:00 exception; got %v", rule.Exceptions)
	}
	if morning.Confidence != 0 {
		t.Errorf("the morning exception's confidence = %v, want 0: the lights are never on then",
			morning.Confidence)
	}
	if morning.ToHour != 12 {
		t.Errorf("the morning exception runs to hour %d, want 12", morning.ToHour)
	}
	if morning.Samples != testDays*2 {
		t.Errorf("the morning exception rests on %d samples, want %d", morning.Samples, testDays*2)
	}

	// The evening bucket is where the rule holds, so it must not be reported as one.
	for _, exception := range rule.Exceptions {
		if exception.Dimension == DimensionHourOfDay && exception.FromHour == 18 {
			t.Errorf("18:00-24:00 was reported as an exception, but the rule holds there: %+v", exception)
		}
	}

	if rule.Advisory == "" || !strings.Contains(rule.Advisory, "candidate") {
		t.Errorf("advisory = %q, want it to state that the rule is a candidate (D28)", rule.Advisory)
	}
	if rule.Decision != nil {
		t.Error("a freshly computed rule carries a decision; nobody has decided anything yet")
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: both members are usable here", notes)
	}
}

// The inverse direction is a different claim and has to be evaluated separately: the
// lights being on says the oven is on in this fixture, which is a finding, while the
// oven being on says only that the lights probably are.
func TestBothDirectionsOfAPairAreExamined(t *testing.T) {
	times, series := kitchenFixture()
	_, rules, _, _ := Relate(times, series, RuleParams{}, DefaultConditioning())

	reverse, found := findRule(rules, "the kitchen lights", "the oven", StateActive)
	if !found {
		t.Fatalf("the reverse rule did not surface; got %v", statements(rules))
	}
	if reverse.Confidence != 1 {
		t.Errorf("confidence = %v, want 1: the lights are only ever on while the oven runs",
			reverse.Confidence)
	}
	if len(reverse.Exceptions) != 0 {
		t.Errorf("the reverse rule holds in every condition, so it should carry no exceptions; got %v",
			reverse.Exceptions)
	}
}

// A rule whose consequent is nearly always true is what confidence alone cannot
// reject, and it is the most common false finding in association mining.
func TestAConsequentThatIsAlwaysTrueIsRejectedByLift(t *testing.T) {
	times, series := kitchenFixture()
	// A fridge, on in all but a handful of buckets. Every rule with it as consequent
	// has a confidence near 1.0 and tells nobody anything.
	fridge := make([]State, len(times))
	for i := range fridge {
		fridge[i] = StateActive
		if i%500 == 0 {
			fridge[i] = StateIdle
		}
	}
	series = append(series, stateSeries("the fridge", profiler.SeriesRef{
		DeviceID: "dev-fridge", ServiceID: "svc-fridge", VariablePath: "value.power",
	}, fridge))

	_, rules, _, _ := Relate(times, series, RuleParams{}, DefaultConditioning())

	if rule, found := findRule(rules, "the oven", "the fridge", StateActive); found {
		t.Errorf("a rule about an always-on fridge survived at confidence %v and lift %v; "+
			"lift is what should have rejected it", rule.Confidence, rule.Lift)
	}
	// The genuine finding must still be there — the filter has to reject the vacuous
	// rule without taking the real one with it.
	if _, found := findRule(rules, "the oven", "the kitchen lights", StateActive); !found {
		t.Error("the oven/lights rule was lost when a third member was added")
	}
}

// The mistake this guards against would make every rule in the package wrong in the
// same direction: a bucket with no reading is not a bucket where the device was off.
func TestABucketWithNoReadingIsNeitherActiveNorIdle(t *testing.T) {
	times, series := kitchenFixture()

	// The lights go dark for the second half of the window — the device is offline, not
	// switched off.
	half := len(times) / 2
	for i := half; i < len(times); i++ {
		series[1].States[i] = StateUnknown
	}
	series[1] = stateSeries(series[1].Member.Label, series[1].Member.Ref, series[1].States)

	pairs, rules, observed, _ := Relate(times, series, RuleParams{}, DefaultConditioning())

	if observed != half {
		t.Errorf("observed = %d, want %d: only the first half has a reading from both members",
			observed, half)
	}
	if len(pairs) != 1 {
		t.Fatalf("pairs = %d, want 1", len(pairs))
	}
	if pairs[0].Overall.Observed != half {
		t.Errorf("the pair's table covers %d buckets, want %d — an unknown bucket must not be counted",
			pairs[0].Overall.Observed, half)
	}

	// The rule is unchanged: half the evidence, the same ratio. Had the unknown buckets
	// been read as idle, the confidence would have halved.
	rule, found := findRule(rules, "the oven", "the kitchen lights", StateActive)
	if !found {
		t.Fatalf("the rule did not survive the gap; got %v", statements(rules))
	}
	if want := 0.8571; rule.Confidence != want {
		t.Errorf("confidence = %v, want %v: a gap changes how much evidence there is, not the ratio",
			rule.Confidence, want)
	}
}

func TestAPairWithOnlyOneUsableMemberProducesNoRuleAndSaysWhy(t *testing.T) {
	times, series := kitchenFixture()
	series[1].Summary.Usable = false
	series[1].Member.State = StateSummary{Usable: false, Method: MethodNone}

	pairs, rules, _, notes := Relate(times, series, RuleParams{}, DefaultConditioning())

	if len(pairs) != 0 || len(rules) != 0 {
		t.Errorf("pairs = %d and rules = %d, want none when only one member is usable",
			len(pairs), len(rules))
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "needs two") {
		t.Errorf("notes = %v, want one saying a pairwise relation needs two members", notes)
	}
}

func TestConditioningCoversBothDimensions(t *testing.T) {
	times, series := kitchenFixture()
	pairs, _, _, _ := Relate(times, series, RuleParams{HourBuckets: 4}, DefaultConditioning())

	if len(pairs) != 1 {
		t.Fatalf("pairs = %d, want 1", len(pairs))
	}
	hours, weekdays := 0, 0
	for _, condition := range pairs[0].Conditions {
		switch condition.Dimension {
		case DimensionHourOfDay:
			hours++
		case DimensionWeekdayWeekend:
			weekdays++
		}
	}
	if hours != 4 {
		t.Errorf("hour-of-day buckets = %d, want 4", hours)
	}
	if weekdays != 2 {
		t.Errorf("weekday/weekend buckets = %d, want 2", weekdays)
	}

	// Conditioning on nothing is a legitimate request and must not silently become
	// the default.
	pairs, _, _, _ = Relate(times, series, RuleParams{}, Conditioning{})
	if len(pairs[0].Conditions) != 0 {
		t.Errorf("conditions = %d with conditioning off, want 0", len(pairs[0].Conditions))
	}
}

// A fingerprint that moved with the window would silently drop every developer
// decision the first time a relation was recomputed over a different range.
func TestARuleFingerprintDependsOnlyOnWhatTheRuleSays(t *testing.T) {
	first := RuleFingerprint(ovenRef(), StateActive, lightsRef(), StateActive)
	again := RuleFingerprint(ovenRef(), StateActive, lightsRef(), StateActive)
	if first != again {
		t.Fatalf("the same claim fingerprinted differently: %s and %s", first, again)
	}

	reversed := RuleFingerprint(lightsRef(), StateActive, ovenRef(), StateActive)
	if reversed == first {
		t.Error("the two directions of a pair share a fingerprint; they are different claims")
	}
	negated := RuleFingerprint(ovenRef(), StateActive, lightsRef(), StateIdle)
	if negated == first {
		t.Error("a rule and its negation share a fingerprint")
	}

	// The same rule computed over a different window and grid, which is what a
	// recomputation looks like, has to keep its identity.
	times, series := kitchenFixture()
	_, wide, _, _ := Relate(times, series, RuleParams{}, DefaultConditioning())
	_, narrow, _, _ := Relate(times[:len(times)/2], []StateSeries{
		stateSeries("the oven", ovenRef(), series[0].States[:len(times)/2]),
		stateSeries("the kitchen lights", lightsRef(), series[1].States[:len(times)/2]),
	}, RuleParams{MinSamples: 5}, DefaultConditioning())

	wideRule, _ := findRule(wide, "the oven", "the kitchen lights", StateActive)
	narrowRule, _ := findRule(narrow, "the oven", "the kitchen lights", StateActive)
	if wideRule.RuleID != narrowRule.RuleID {
		t.Errorf("the same rule over a narrower window has a different id: %s and %s — "+
			"a decision would stop applying", wideRule.RuleID, narrowRule.RuleID)
	}
}

func TestStrengthIsOrdinalAndNeverCertain(t *testing.T) {
	times, series := kitchenFixture()
	_, rules, _, _ := Relate(times, series, RuleParams{}, DefaultConditioning())

	for _, rule := range rules {
		switch rule.Strength {
		case profiler.Likely, profiler.Uncertain:
		case profiler.Certain:
			t.Errorf("rule %q is reported certain; that level is reserved for ontology-derived "+
				"and developer-confirmed values (D23)", rule.Statement)
		default:
			t.Errorf("rule %q has strength %q, which is not one of the three levels",
				rule.Statement, rule.Strength)
		}
	}
}

func TestTheStrongestRuleComesFirst(t *testing.T) {
	times, series := kitchenFixture()
	_, rules, _, _ := Relate(times, series, RuleParams{}, DefaultConditioning())
	for i := 1; i < len(rules); i++ {
		if rules[i-1].Lift < rules[i].Lift {
			t.Fatalf("rule %d has a higher lift than rule %d; the list is not ordered", i, i-1)
		}
	}
}

func statements(rules []CandidateRule) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.Statement)
	}
	return out
}

// --- DeriveState ---

// sessionProfile is a profile whose activity_pattern says "session based, active
// above the threshold".
func sessionProfile(threshold float64, kind profiler.ValueKind) profiler.ResolvedProfile {
	return profiler.ResolvedProfile{
		SeriesProfile: profiler.SeriesProfile{
			ProfileID: "prof-1",
			SeriesRef: ovenRef(),
			ValueSemantics: profiler.ValueSemantics{
				Kind: profiler.Computed(kind),
				Unit: "W",
			},
			ActivityPattern: profiler.Computed(profiler.ActivityPattern{
				Classification:  profiler.ActivitySessionBased,
				IdleLevel:       5,
				ActiveThreshold: threshold,
				ThresholdMethod: "otsu",
				ThresholdParams: profiler.SessionParams{HysteresisFrac: 0.1},
			}),
		},
		Resolution: map[string]profiler.Resolution{},
	}
}

func column(values []float64, present []bool) AlignedColumn {
	return AlignedColumn{Ref: ovenRef(), Values: values, Present: present}
}

func repeatPattern(count int, pattern ...float64) ([]float64, []bool) {
	values := make([]float64, 0, count)
	present := make([]bool, 0, count)
	for i := 0; i < count; i++ {
		values = append(values, pattern[i%len(pattern)])
		present = append(present, true)
	}
	return values, present
}

func TestDeriveStateUsesTheProfilersOwnThreshold(t *testing.T) {
	values, present := repeatPattern(40, 2000, 5)
	series := DeriveState(column(values, present), Member{Label: "the oven"},
		sessionProfile(100, profiler.KindInstantaneous))

	if !series.Summary.Usable {
		t.Fatalf("state series unusable: %+v", series.Summary.Reason)
	}
	if series.Summary.Threshold != 100 {
		t.Errorf("threshold = %v, want the profiler's 100", series.Summary.Threshold)
	}
	if series.Summary.ThresholdSource != ThresholdDetector {
		t.Errorf("threshold_source = %q, want %q", series.Summary.ThresholdSource, ThresholdDetector)
	}
	if series.Summary.ActiveBuckets != 20 || series.Summary.IdleBuckets != 20 {
		t.Errorf("active/idle = %d/%d, want 20/20",
			series.Summary.ActiveBuckets, series.Summary.IdleBuckets)
	}
	if series.Summary.DutyCycle != 0.5 {
		t.Errorf("duty_cycle = %v, want 0.5", series.Summary.DutyCycle)
	}
}

// This is what makes §5.10 a mechanism rather than a record: a threshold the
// developer corrected has to drive the rules, not merely sit in a log beside them.
func TestAConfirmedThresholdOverridesTheDetectedOne(t *testing.T) {
	values, present := repeatPattern(40, 300, 5)
	profile := sessionProfile(1000, profiler.KindInstantaneous)

	// At the detected threshold of 1000 nothing is ever active.
	detected := DeriveState(column(values, present), Member{}, profile)
	if detected.Summary.ActiveBuckets != 0 {
		t.Fatalf("active buckets = %d at the detected threshold, want 0 — the fixture depends on it",
			detected.Summary.ActiveBuckets)
	}

	profile.Resolution[profiler.FieldActiveThreshold] = profiler.Resolution{
		FieldPath:      profiler.FieldActiveThreshold,
		Action:         profiler.ActionCorrect,
		ComputedValue:  1000.0,
		ConfirmedValue: 100.0,
	}
	confirmed := DeriveState(column(values, present), Member{}, profile)

	if confirmed.Summary.Threshold != 100 {
		t.Errorf("threshold = %v, want the developer's 100", confirmed.Summary.Threshold)
	}
	if confirmed.Summary.ThresholdSource != ThresholdConfirmed {
		t.Errorf("threshold_source = %q, want %q — a rule computed against a corrected threshold "+
			"is a different claim and has to say so",
			confirmed.Summary.ThresholdSource, ThresholdConfirmed)
	}
	if confirmed.Summary.ActiveBuckets != 20 {
		t.Errorf("active buckets = %d, want 20 under the confirmed threshold",
			confirmed.Summary.ActiveBuckets)
	}
}

func TestAConfirmationWithoutAValueLeavesTheThresholdAlone(t *testing.T) {
	values, present := repeatPattern(40, 2000, 5)
	profile := sessionProfile(100, profiler.KindInstantaneous)
	profile.Resolution[profiler.FieldActiveThreshold] = profiler.Resolution{
		FieldPath: profiler.FieldActiveThreshold,
		Action:    profiler.ActionConfirm,
		// A confirm agrees with the detector and carries no value of its own.
	}

	series := DeriveState(column(values, present), Member{}, profile)
	if series.Summary.Threshold != 100 {
		t.Errorf("threshold = %v, want the detected 100 to stand", series.Summary.Threshold)
	}
	if series.Summary.ThresholdSource != ThresholdDetector {
		t.Errorf("threshold_source = %q, want %q: a confirm adds no number",
			series.Summary.ThresholdSource, ThresholdDetector)
	}
}

func TestANotComputedActivityPatternCarriesItsReasonThrough(t *testing.T) {
	values, present := repeatPattern(40, 2000, 5)
	profile := profiler.ResolvedProfile{
		SeriesProfile: profiler.SeriesProfile{
			ActivityPattern: profiler.Uncomputable[profiler.ActivityPattern](
				profiler.ReasonInsufficientCoverage, "11 usable points, need at least 20"),
		},
		Resolution: map[string]profiler.Resolution{},
	}

	series := DeriveState(column(values, present), Member{}, profile)
	if series.Summary.Usable {
		t.Fatal("a series with no activity_pattern was reported usable")
	}
	if series.Summary.Reason.Reason != profiler.ReasonInsufficientCoverage {
		t.Errorf("reason = %q, want the profiler's own %q",
			series.Summary.Reason.Reason, profiler.ReasonInsufficientCoverage)
	}
	if !strings.Contains(series.Summary.Reason.Detail, "11 usable points") {
		t.Errorf("detail = %q, want the profiler's own detail carried through",
			series.Summary.Reason.Detail)
	}
	for i, state := range series.States {
		if state != StateUnknown {
			t.Fatalf("state %d = %q, want every bucket unknown", i, state)
		}
	}
}

// omitempty does not apply to a struct, so a reason held by value would put an empty
// reason object beside every usable member — "there is a reason and it is blank",
// which is the reading D24 exists to prevent one level down.
func TestAUsableMemberCarriesNoReasonAtAll(t *testing.T) {
	values, present := repeatPattern(40, 2000, 5)
	usable := DeriveState(column(values, present), Member{},
		sessionProfile(100, profiler.KindInstantaneous))
	if usable.Summary.Reason != nil {
		t.Errorf("a usable member carries reason %+v, want none", usable.Summary.Reason)
	}

	encoded, err := json.Marshal(usable.Summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "reason") {
		t.Errorf("a usable member marshals a reason field: %s", encoded)
	}

	// And the unusable case still says why, which is the half that matters.
	unusableSeries := DeriveState(column([]float64{1}, []bool{true}), Member{},
		sessionProfile(100, profiler.KindInstantaneous))
	if unusableSeries.Summary.Reason == nil {
		t.Fatal("an unusable member carries no reason")
	}
	if unusableSeries.Summary.Reason.Reason == "" {
		t.Error("the reason is present but empty")
	}
}

func TestAContinuousSeriesYieldsNoStateSeries(t *testing.T) {
	values, present := repeatPattern(40, 2000, 1900)
	profile := sessionProfile(100, profiler.KindInstantaneous)
	activity, _ := profile.ActivityPattern.Get()
	activity.Classification = profiler.ActivityContinuous
	profile.ActivityPattern = profiler.Computed(activity)

	series := DeriveState(column(values, present), Member{}, profile)
	if series.Summary.Usable {
		t.Error("a continuous series was split into idle and active; the split would be a " +
			"property of the threshold rather than of the device")
	}
	if series.Summary.Reason.Reason != profiler.ReasonWrongKind {
		t.Errorf("reason = %q, want %q", series.Summary.Reason.Reason, profiler.ReasonWrongKind)
	}
}

func TestAStatusSeriesYieldsNoStateSeries(t *testing.T) {
	values, present := repeatPattern(40, 1, 2, 3)
	profile := sessionProfile(100, profiler.KindCategorical)
	activity, _ := profile.ActivityPattern.Get()
	activity.Classification = profiler.ActivityStatus
	profile.ActivityPattern = profiler.Computed(activity)

	series := DeriveState(column(values, present), Member{}, profile)
	if series.Summary.Usable {
		t.Error("a status series was thresholded; this detector relates activity, not category labels")
	}
}

func TestHysteresisHoldsAValueHoveringAtTheThreshold(t *testing.T) {
	// Alternating either side of the threshold, but inside the 10% band. Without
	// hysteresis this is twenty transitions; with it, one.
	values, present := repeatPattern(40, 101, 95)
	series := DeriveState(column(values, present), Member{},
		sessionProfile(100, profiler.KindInstantaneous))

	if series.Summary.ActiveBuckets != 40 {
		t.Errorf("active buckets = %d, want 40: 95 is inside the 10%% hysteresis band below 100",
			series.Summary.ActiveBuckets)
	}
}

// Hysteresis must not become an assumption about what happened while nobody was
// looking.
func TestAGapClearsTheHysteresisState(t *testing.T) {
	values := []float64{2000, 2000, 0, 95, 95}
	present := []bool{true, true, false, true, true}
	// Pad to the usable floor with clearly active buckets.
	for i := 0; i < 20; i++ {
		values = append(values, 2000)
		present = append(present, true)
	}

	series := DeriveState(column(values, present),
		Member{}, sessionProfile(100, profiler.KindInstantaneous))

	if series.States[2] != StateUnknown {
		t.Errorf("state 2 = %q, want unknown: that bucket has no reading", series.States[2])
	}
	if series.States[3] != StateIdle {
		t.Errorf("state 3 = %q, want idle: after a gap, 95 is below the threshold rather than "+
			"inside a band carried across it", series.States[3])
	}
}

func TestTooFewObservedBucketsIsReportedRatherThanGuessed(t *testing.T) {
	values, present := repeatPattern(10, 2000, 5)
	series := DeriveState(column(values, present), Member{},
		sessionProfile(100, profiler.KindInstantaneous))

	if series.Summary.Usable {
		t.Error("ten buckets produced a usable state series")
	}
	if series.Summary.Reason.Reason != profiler.ReasonInsufficientCoverage {
		t.Errorf("reason = %q, want %q", series.Summary.Reason.Reason, profiler.ReasonInsufficientCoverage)
	}
}

// --- the grid ---

// A grid finer than the slowest member is the subtle failure of the whole package:
// the slow series has nothing to say in most buckets, and every empty bucket looks
// like an idle device.
func TestTheGridFollowsTheCoarsestMember(t *testing.T) {
	window := profiler.Window{From: fixtureStart, To: fixtureStart.Add(30 * 24 * time.Hour)}

	seconds, widened := chooseGrid(window, []float64{5, 60, 900}, 0)
	if seconds != 900 {
		t.Errorf("grid = %vs, want 900 — the coarsest member's interval, not the finest", seconds)
	}
	if widened {
		t.Error("the grid was reported widened when the cap did not bite")
	}

	// A cap that bites widens rather than shortening the window.
	seconds, widened = chooseGrid(window, []float64{900}, 100)
	if !widened {
		t.Error("the grid was not reported widened when the bucket cap applied")
	}
	if seconds <= 900 {
		t.Errorf("grid = %vs, want something coarser than 900 to fit 100 buckets", seconds)
	}
	if buckets := window.Duration().Seconds() / seconds; buckets > 100 {
		t.Errorf("%v buckets at %vs, want at most 100", buckets, seconds)
	}

	// No member reported an interval: the platform's meter cadence rather than a
	// second of guessing.
	if seconds, _ := chooseGrid(window, nil, 0); seconds != 900 {
		t.Errorf("grid with no intervals = %vs, want the 900s meter cadence", seconds)
	}
}

func TestTheGridIsRoundedUpTheLadder(t *testing.T) {
	window := profiler.Window{From: fixtureStart, To: fixtureStart.Add(24 * time.Hour)}
	if seconds, _ := chooseGrid(window, []float64{61}, 0); seconds != 300 {
		t.Errorf("grid = %vs for a 61s interval, want the next rung at 300s", seconds)
	}
}

// --- Project ---

func TestTheProjectionDropsThePairsAndBoundsTheRuleList(t *testing.T) {
	times, series := kitchenFixture()
	pairs, rules, observed, _ := Relate(times, series, RuleParams{}, DefaultConditioning())

	profile := RelationProfile{
		RelationID:     "rel-1",
		Tier:           TierRelation,
		Pairs:          pairs,
		CandidateRules: rules,
		Observed:       observed,
		Members:        []Member{series[0].Member, series[1].Member},
		Notes:          []string{"a note"},
	}

	view := Project(profile, 1, 0)

	if len(view.CandidateRules) != 1 {
		t.Errorf("rules = %d, want the cap of 1", len(view.CandidateRules))
	}
	// A model that cannot tell which pairs it is missing must not be given a sample
	// of them, so they are elided entirely and the elision says where they live.
	pairsElided := false
	for _, elision := range view.Elided {
		if elision.Field == "pairs" {
			pairsElided = true
			if elision.Total != len(pairs) || elision.Shown != 0 {
				t.Errorf("pairs elision = %+v, want all %d elided", elision, len(pairs))
			}
			if elision.Fetch != "/relations/rel-1" {
				t.Errorf("fetch = %q, want the route that serves the full document", elision.Fetch)
			}
		}
	}
	if !pairsElided {
		t.Error("the pairwise tables were dropped without an elision recording it (D26)")
	}

	truncationNoted := false
	for _, note := range view.Notes {
		if strings.Contains(note, "strongest") {
			truncationNoted = true
		}
	}
	if !truncationNoted {
		t.Errorf("notes = %v, want one saying the list was truncated: silence reads as completeness",
			view.Notes)
	}
}

func TestTheProjectionShrinksToFitATokenBudget(t *testing.T) {
	times, series := kitchenFixture()
	pairs, rules, _, _ := Relate(times, series, RuleParams{}, DefaultConditioning())
	profile := RelationProfile{RelationID: "rel-1", Pairs: pairs, CandidateRules: rules}

	unbounded := Project(profile, 0, 0)
	bounded := Project(profile, 0, 60)

	if len(bounded.CandidateRules) >= len(unbounded.CandidateRules) {
		t.Errorf("a 60-token budget kept %d of %d rules; nothing was dropped",
			len(bounded.CandidateRules), len(unbounded.CandidateRules))
	}
	if len(bounded.CandidateRules) == 0 {
		t.Error("every rule was dropped; a budget should leave the strongest one")
	}
	if len(bounded.Elided) == 0 {
		t.Error("detail was dropped with nothing recording it (D26)")
	}
}

// --- the store ---

func TestTheDecisionLogIsAppendOnly(t *testing.T) {
	store := NewMemoryStore(0)
	ruleID := RuleFingerprint(ovenRef(), StateActive, lightsRef(), StateActive)

	first, err := store.AppendDecision(RuleDecision{
		RuleID: ruleID, CreatedBy: "user-1", Action: ActionConfirm,
		Computed:  DecidedRule{Statement: "the oven active → the kitchen lights active"},
		CreatedAt: fixtureStart,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	second, err := store.AppendDecision(RuleDecision{
		RuleID: ruleID, CreatedBy: "user-1", Action: ActionReject,
		Computed:  DecidedRule{Statement: "the oven active → the kitchen lights active"},
		CreatedAt: fixtureStart.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if first.DecisionID == second.DecisionID {
		t.Error("two decisions share an id")
	}

	log := store.Decisions([]string{ruleID})[ruleID]
	if len(log) != 2 {
		t.Fatalf("log = %d entries, want 2: changing your mind appends rather than replaces", len(log))
	}
	if log[0].Action != ActionConfirm || log[1].Action != ActionReject {
		t.Errorf("log is not oldest-first: %q then %q", log[0].Action, log[1].Action)
	}
	if standing := latest(log); standing == nil || standing.Action != ActionReject {
		t.Errorf("the standing decision is %+v, want the newest one", standing)
	}

	if _, found := store.Decisions([]string{"rule-nobody-decided"})["rule-nobody-decided"]; found {
		t.Error("an undecided rule is present in the map; it should be absent rather than empty")
	}
}

func TestADecisionMustSayWhoMadeItAndACorrectionMustCarryARule(t *testing.T) {
	store := NewMemoryStore(0)

	if _, err := store.AppendDecision(RuleDecision{RuleID: "rule-1", Action: ActionConfirm}); err == nil {
		t.Error("a decision with no author was accepted; the author is the whole evidentiary value")
	}
	if _, err := store.AppendDecision(RuleDecision{
		RuleID: "rule-1", CreatedBy: "user-1", Action: ActionCorrect,
	}); err == nil {
		t.Error("a correction with no corrected rule was accepted")
	}
	if _, err := store.AppendDecision(RuleDecision{
		RuleID: "rule-1", CreatedBy: "user-1", Action: "maybe",
	}); err == nil {
		t.Error("an unknown action was accepted")
	}
}

func TestAStoredRelationProfileIsImmutable(t *testing.T) {
	store := NewMemoryStore(0)
	first, created, err := store.Put(RelationProfile{RelationID: "rel-1", Observed: 100})
	if err != nil || !created {
		t.Fatalf("put: created=%v err=%v", created, err)
	}
	again, created, err := store.Put(RelationProfile{RelationID: "rel-1", Observed: 200})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if created {
		t.Error("the second put reported a creation")
	}
	if again.Observed != first.Observed {
		t.Errorf("observed = %d, want the stored %d: a recomputation must not alter what a "+
			"developer has already read and decided against (D21)", again.Observed, first.Observed)
	}
}

// The discriminator is chosen by what separates the group, not by a fixed field: two
// services of one device differ by service, two devices by device, and a pathological
// group falls back to the reference, which is unique by construction.
func TestLabelSuffixesPickWhatActuallySeparatesTheGroup(t *testing.T) {
	cases := map[string]struct {
		options [][]string
		want    []string
	}{
		"service name separates them": {
			options: [][]string{
				{"switch", "Licht EG", "value.value", "d1|s1|value.value"},
				{"meter", "Licht EG", "value.value", "d1|s2|value.value"},
			},
			want: []string{"switch", "meter"},
		},
		"same service name, so the device does": {
			options: [][]string{
				{"readings", "Licht EG", "value.power", "d1|s1|value.power"},
				{"readings", "Licht OG", "value.power", "d2|s1|value.power"},
			},
			want: []string{"Licht EG", "Licht OG"},
		},
		"neither, so the path does": {
			options: [][]string{
				{"readings", "Licht EG", "value.power", "d1|s1|value.power"},
				{"readings", "Licht EG", "value.total", "d1|s1|value.total"},
			},
			want: []string{"value.power", "value.total"},
		},
		"an empty candidate disqualifies its position": {
			options: [][]string{
				{"", "Licht EG", "value.power", "d1|s1|value.power"},
				{"meter", "Licht OG", "value.power", "d2|s2|value.power"},
			},
			want: []string{"Licht EG", "Licht OG"},
		},
		"nothing separates them, so the reference does": {
			options: [][]string{
				{"readings", "Licht EG", "value.power", "d1|s1|value.power"},
				{"readings", "Licht EG", "value.power", "d2|s1|value.power"},
			},
			want: []string{"d1|s1|value.power", "d2|s1|value.power"},
		},
	}

	for name, test := range cases {
		got := labelSuffixes(test.options)
		if len(got) != len(test.want) {
			t.Errorf("%s: got %d suffixes, want %d", name, len(got), len(test.want))
			continue
		}
		for i := range got {
			if got[i] != test.want[i] {
				t.Errorf("%s: suffix %d = %q, want %q", name, i, got[i], test.want[i])
			}
		}
	}

	if labelSuffixes(nil) != nil {
		t.Error("no members should yield no suffixes")
	}
}
