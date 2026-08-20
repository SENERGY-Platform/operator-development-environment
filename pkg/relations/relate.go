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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// Relate computes the pairwise contingency and the candidate rules (§5.5).
//
// Pure: no I/O, no clock, no platform. Everything it needs is in the state series
// and the grid, which is what makes the rule logic testable against fixtures with
// known answers the way the profiler's detectors are (§5.4.14) — an association
// rule found in synthetic oven-and-lights data is a check; one found in the
// platform's kitchen is an anecdote.
//
// Only usable members take part. An unusable one keeps its place in Members, with
// the reason it could not be derived, so a developer sees that the light was
// considered rather than wondering why it is absent.
func Relate(times []time.Time, series []StateSeries, params RuleParams, conditioning Conditioning) (
	pairs []PairRelation, rules []CandidateRule, observed int, notes []string,
) {
	params = params.withDefaults()
	pairs, rules, notes = []PairRelation{}, []CandidateRule{}, []string{}

	usable := []int{}
	for i, s := range series {
		if s.Summary.Usable {
			usable = append(usable, i)
		}
	}
	if len(usable) < 2 {
		notes = append(notes, fmt.Sprintf(
			"%d of %d members yielded a state series, and a pairwise relation needs two",
			len(usable), len(series)))
		return pairs, rules, 0, notes
	}

	// Observed counts the buckets in which *every* usable member had a reading. It
	// is not what a pair's contingency is computed over — a pair uses its own
	// overlap, which is larger — and the difference is worth stating: a member that
	// was offline for a month should not shrink the evidence behind a rule about the
	// other two.
	for i := range times {
		complete := true
		for _, index := range usable {
			if series[index].States[i] == StateUnknown {
				complete = false
				break
			}
		}
		if complete {
			observed++
		}
	}

	for ai := 0; ai < len(usable); ai++ {
		for bi := ai + 1; bi < len(usable); bi++ {
			a, b := usable[ai], usable[bi]
			pair := PairRelation{
				A:          a,
				B:          b,
				Overall:    tabulate(series[a].States, series[b].States, nil),
				Conditions: conditions(times, series[a].States, series[b].States, params, conditioning),
			}
			pairs = append(pairs, pair)
			rules = append(rules, candidates(pair, series, params)...)
		}
	}

	// Strongest first: a developer confirming rules works down the list, and the
	// projection handed to a model is the head of it (D26).
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Lift != rules[j].Lift {
			return rules[i].Lift > rules[j].Lift
		}
		if rules[i].Support != rules[j].Support {
			return rules[i].Support > rules[j].Support
		}
		return rules[i].RuleID < rules[j].RuleID
	})
	return pairs, rules, observed, notes
}

// tabulate builds the 2×2 table over the buckets both members were observed in.
//
// include, when set, restricts the table to the buckets it marks — which is how a
// conditioned contingency is computed without copying the state series.
func tabulate(a, b []State, include []bool) Contingency {
	table := Contingency{}
	for i := range a {
		if i >= len(b) {
			break
		}
		if include != nil && (i >= len(include) || !include[i]) {
			continue
		}
		if a[i] == StateUnknown || b[i] == StateUnknown {
			continue
		}
		switch {
		case a[i] == StateActive && b[i] == StateActive:
			table.ActiveActive++
		case a[i] == StateActive && b[i] == StateIdle:
			table.ActiveIdle++
		case a[i] == StateIdle && b[i] == StateActive:
			table.IdleActive++
		default:
			table.IdleIdle++
		}
	}
	table.Observed = table.ActiveActive + table.ActiveIdle + table.IdleActive + table.IdleIdle
	if table.Observed > 0 {
		table.ActiveRateA = roundTo(float64(table.ActiveActive+table.ActiveIdle)/float64(table.Observed), 4)
		table.ActiveRateB = roundTo(float64(table.ActiveActive+table.IdleActive)/float64(table.Observed), 4)
	}
	return table
}

// conditions slices a pair by the conditioning dimensions (§5.5).
func conditions(
	times []time.Time, a, b []State, params RuleParams, conditioning Conditioning,
) []ConditionedContingency {
	out := []ConditionedContingency{}
	if conditioning.HourOfDay {
		width := 24 / params.HourBuckets
		if width < 1 {
			width = 1
		}
		for from := 0; from < 24; from += width {
			to := from + width
			if to > 24 {
				to = 24
			}
			mask := make([]bool, len(times))
			for i, at := range times {
				// UTC throughout, as everything computed in this repository is
				// (§5.4.13). The hour a rule's exception names is a UTC hour, and the
				// frontend is what renders it in the developer's zone.
				hour := at.UTC().Hour()
				mask[i] = hour >= from && hour < to
			}
			out = append(out, ConditionedContingency{
				Dimension:   DimensionHourOfDay,
				Bucket:      fmt.Sprintf("%02d:00-%02d:00", from, to%24),
				Contingency: tabulate(a, b, mask),
			})
		}
	}
	if conditioning.WeekdayWeekend {
		for _, weekend := range []bool{false, true} {
			mask := make([]bool, len(times))
			for i, at := range times {
				day := at.UTC().Weekday()
				isWeekend := day == time.Saturday || day == time.Sunday
				mask[i] = isWeekend == weekend
			}
			bucket := "weekday"
			if weekend {
				bucket = "weekend"
			}
			out = append(out, ConditionedContingency{
				Dimension:   DimensionWeekdayWeekend,
				Bucket:      bucket,
				Contingency: tabulate(a, b, mask),
			})
		}
	}
	return out
}

// candidates proposes rules from one pair's tables.
//
// Four directed rules are examined per pair — each member as antecedent, each in
// its active state, against both states of the other. The oven case is one of
// them: *while the oven is active, the kitchen lights are active*, whose violation
// is the anomaly the developer described. The reverse direction is examined too
// rather than assumed symmetric, because it is not: lights on with the oven off is
// an ordinary evening, and only one of the two directions is a finding.
//
// An idle antecedent is deliberately not examined. "While the oven is idle, the
// lights are idle" is true most of the night in any kitchen, and it holds at high
// confidence for reasons that have nothing to do with the pair — the base rate
// does the work, and lift is not enough of a filter to keep the rule list readable.
func candidates(pair PairRelation, series []StateSeries, params RuleParams) []CandidateRule {
	out := []CandidateRule{}
	directions := []struct {
		antecedent, consequent int
	}{{pair.A, pair.B}, {pair.B, pair.A}}

	for _, direction := range directions {
		for _, consequentState := range []State{StateActive, StateIdle} {
			rule, ok := candidate(pair, direction.antecedent, direction.consequent, consequentState, series, params)
			if ok {
				out = append(out, rule)
			}
		}
	}
	return out
}

func candidate(
	pair PairRelation, antecedent, consequent int, consequentState State,
	series []StateSeries, params RuleParams,
) (CandidateRule, bool) {
	overall, ok := association(pair, antecedent, consequent, consequentState)
	if !ok {
		return CandidateRule{}, false
	}
	if overall.samples < params.MinSamples ||
		overall.support < params.MinSupport ||
		overall.confidence < params.MinConfidence ||
		overall.lift < params.MinLift {
		return CandidateRule{}, false
	}

	antecedentLabel := label(series, antecedent)
	consequentLabel := label(series, consequent)

	rule := CandidateRule{
		RuleID: RuleFingerprint(
			series[antecedent].Member.Ref, StateActive,
			series[consequent].Member.Ref, consequentState),
		Antecedent: RuleTerm{Member: antecedent, Label: antecedentLabel, State: StateActive},
		Consequent: RuleTerm{Member: consequent, Label: consequentLabel, State: consequentState},
		// No copula, and that is deliberate rather than terse. A label is an arbitrary
		// noun phrase supplied by a developer, an LLM or a device name — "the oven", "the
		// kitchen lights", "PV inverter 3" — and English subject-verb agreement cannot be
		// inferred from one, so "the kitchen lights is active" is what a templated verb
		// produces. This string sits beside the confirm button, and the one place the
		// design asks a human to read a derived claim carefully is the wrong place for
		// visibly broken grammar.
		Statement: fmt.Sprintf("%s active → %s %s",
			antecedentLabel, consequentLabel, consequentState),
		Anomaly: fmt.Sprintf("%s active while %s %s",
			antecedentLabel, consequentLabel, opposite(consequentState)),
		Support:    roundTo(overall.support, 4),
		Confidence: roundTo(overall.confidence, 4),
		Lift:       roundTo(overall.lift, 4),
		Samples:    overall.samples,
		Violations: overall.samples - overall.hits,
		Strength:   strength(overall, params),
		Exceptions: exceptions(pair, antecedent, consequent, consequentState, overall.confidence, params),
		Advisory:   advisoryNote,
	}
	return rule, true
}

// measure is one directed association, before it is dressed as a document.
type measure struct {
	samples    int
	hits       int
	support    float64
	confidence float64
	lift       float64
}

// association computes support, confidence and lift for one direction.
//
// Lift is confidence divided by the consequent's own base rate. It is what
// separates a finding from a coincidence of base rates: a light that is on 95% of
// the time yields confidence 0.95 for every rule with it as consequent, and a lift
// of 1.0 says so.
func association(pair PairRelation, antecedent, consequent int, consequentState State) (measure, bool) {
	table := pair.Overall
	if table.Observed == 0 {
		return measure{}, false
	}

	// The table is keyed by A then B, so a direction with B as antecedent reads the
	// transpose. Transposing here rather than tabulating twice keeps one table as
	// the single source of the counts a reader can check the ratios against.
	if antecedent == pair.B && consequent == pair.A {
		table.ActiveIdle, table.IdleActive = table.IdleActive, table.ActiveIdle
		table.ActiveRateA, table.ActiveRateB = table.ActiveRateB, table.ActiveRateA
	} else if antecedent != pair.A || consequent != pair.B {
		return measure{}, false
	}

	samples := table.ActiveActive + table.ActiveIdle
	if samples == 0 {
		return measure{}, false
	}
	hits := table.ActiveActive
	baseRate := table.ActiveRateB
	if consequentState == StateIdle {
		hits = table.ActiveIdle
		baseRate = 1 - table.ActiveRateB
	}

	confidence := float64(hits) / float64(samples)
	m := measure{
		samples:    samples,
		hits:       hits,
		support:    float64(hits) / float64(table.Observed),
		confidence: confidence,
	}
	// A consequent state that never occurred has no base rate for a lift to be
	// measured against. Zero rather than an error, and the distinction matters in both
	// directions: as a *rule* that is vacuous and MinLift is what rejects it, but as a
	// *condition on an existing rule* a confidence of zero is the finding — the lights
	// are never on during the morning run, which is precisely the exception §5.5 asks
	// for. Refusing to compute it here would make the exception undetectable.
	if baseRate > 0 {
		m.lift = confidence / baseRate
	}
	return m, true
}

// strength is the detector's own certainty, ordinal because D23 forbids a number.
//
// It rests on the two things that make an association trustworthy and are not the
// confidence itself: how much evidence there is, and how far above the base rate
// the pattern sits. Both have to hold — a near-deterministic pattern over forty
// buckets and a marginal one over forty thousand are each uncertain for their own
// reason.
//
// `certain` is unreachable by construction. That level is reserved for
// ontology-derived and developer-confirmed values (D23), and a detected
// co-occurrence is neither until somebody confirms it — at which point the
// decision, not this field, is what says so.
func strength(m measure, params RuleParams) profiler.Confidence {
	strongEvidence := m.samples >= 10*params.MinSamples
	strongLift := m.lift >= 2*params.MinLift
	if strongEvidence && strongLift {
		return profiler.Likely
	}
	return profiler.Uncertain
}

// exceptions finds the conditions in which a rule does not hold (§5.5).
//
// This is the "except at certain times of day" of the motivating case, and it is
// the reason conditioning exists rather than being an optional extra. A condition
// is an exception when it has enough samples of its own to be worth believing and
// its confidence falls materially below the rule's overall confidence.
//
// The sample floor is a quarter of the rule's, not the rule's own: a four-hour
// window holds a sixth of the day, so demanding the full floor per bucket would
// make an exception undetectable in exactly the case it matters.
func exceptions(
	pair PairRelation, antecedent, consequent int, consequentState State,
	overallConfidence float64, params RuleParams,
) []Exception {
	out := []Exception{}
	floor := params.MinSamples / 4
	if floor < 5 {
		floor = 5
	}

	for _, condition := range pair.Conditions {
		conditioned := PairRelation{A: pair.A, B: pair.B, Overall: condition.Contingency}
		m, ok := association(conditioned, antecedent, consequent, consequentState)
		if !ok || m.samples < floor {
			continue
		}
		drop := overallConfidence - m.confidence
		if drop < params.ExceptionDrop {
			continue
		}
		exception := Exception{
			Dimension:  condition.Dimension,
			Bucket:     condition.Bucket,
			Samples:    m.samples,
			Confidence: roundTo(m.confidence, 4),
			Drop:       roundTo(drop, 4),
		}
		if condition.Dimension == DimensionHourOfDay {
			exception.FromHour, exception.ToHour = parseHourBucket(condition.Bucket)
		}
		out = append(out, exception)
	}

	// Biggest drop first: the exception that most nearly refutes the rule is the one
	// a developer needs to see, and a long tail of marginal ones buries it.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Drop != out[j].Drop {
			return out[i].Drop > out[j].Drop
		}
		return out[i].Bucket < out[j].Bucket
	})
	return out
}

// parseHourBucket reads "06:00-12:00" back into its bounds.
//
// The string is generated a few lines above and parsed here rather than the bounds
// being threaded through, because the bucket label is what a reader sees and the
// two must not be able to disagree. A label that fails to parse yields a zero
// range, which the dimension already makes readable.
func parseHourBucket(bucket string) (from, to int) {
	parts := strings.Split(bucket, "-")
	if len(parts) != 2 {
		return 0, 0
	}
	read := func(value string) int {
		hour := 0
		if _, err := fmt.Sscanf(value, "%d:", &hour); err != nil {
			return 0
		}
		return hour
	}
	from, to = read(parts[0]), read(parts[1])
	if to == 0 && from != 0 {
		to = 24
	}
	return from, to
}

// RuleFingerprint is the identity of what a rule says.
//
// It covers the two references, their states and the direction, and nothing else.
// Not the window, not the grid, not the detector version — a decision a developer
// made about "while the oven runs the lights are on" is a decision about that
// claim, and it has to survive the same claim being recomputed next month over a
// longer window by a sharper detector. This is the same reasoning that keys
// ProfileOverride by series reference rather than by profile id (D21).
func RuleFingerprint(
	antecedent profiler.SeriesRef, antecedentState State,
	consequent profiler.SeriesRef, consequentState State,
) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		antecedent.String(), string(antecedentState),
		consequent.String(), string(consequentState),
	}, "\x00")))
	return "rule-" + hex.EncodeToString(sum[:])[:24]
}

func opposite(state State) State {
	if state == StateActive {
		return StateIdle
	}
	return StateActive
}

func label(series []StateSeries, index int) string {
	if index < 0 || index >= len(series) {
		return "an unknown series"
	}
	return series[index].Member.Label
}

// roundTo keeps a document readable. A confidence printed to fifteen decimal
// places invites the false precision D23 argues against, and the counts beside it
// are what a reader checks anyway.
func roundTo(value float64, decimals int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	scale := math.Pow(10, float64(decimals))
	return math.Round(value*scale) / scale
}
