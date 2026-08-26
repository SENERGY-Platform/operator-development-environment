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

package profiler

import (
	"fmt"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

const (
	// monotonicThreshold is where a numeric series counts as a cumulative
	// counter (§5.4.13 item 2).
	monotonicThreshold = 0.95
	// resetDropRatio: a negative step that lands below this fraction of the
	// previous value is a counter reset rather than noise. Half is deliberately
	// blunt — a meter rollover or a replacement drops to near zero, while sensor
	// jitter on a counter moves it by parts per thousand.
	resetDropRatio = 0.5
	// maxResetShare is how many resets a counter may take beside the number of
	// times it rose.
	//
	// It is what tells a register from a load that switches on and off, and the two
	// populations sit an order of magnitude either side of it: a register rolling
	// over daily and read every quarter hour comes to about 0.01, while a load that
	// falls back to idle after every rise comes to 1. Both clear the monotonic
	// threshold, because a long idle stretch is a run of deltas that are exactly
	// zero rather than negative.
	maxResetShare = 0.1
	// statusDistinctMax separates a small set of named states from a larger
	// labelled vocabulary.
	statusDistinctMax = 10
	// minKindPoints is the fewest points that can support a kind judgement.
	minKindPoints = 3
	// confidentKindSize is where the monotonic ratio stops being a coincidence.
	confidentKindSize = 50
)

// kindResult is what detector 2 produces. It is returned as one struct because
// the four fields are one judgement: the kind, its confidence, the evidence
// behind it, and the resets that only mean anything if the kind is a counter.
type kindResult struct {
	Kind       Value[ValueKind]
	Confidence Value[Confidence]
	Evidence   Value[KindEvidence]
	Resets     Value[[]time.Time]
}

// detectValueKind is detector 2 of §5.4.13 and the highest-impact detector in
// the profiler.
//
// Misreading a cumulative kWh counter as instantaneous power produces silent
// garbage: every statistic is computed over a monotonically rising number, the
// mean is meaningless, the distribution is a ramp, and nothing about the output
// looks wrong. So the evidence travels with the verdict (D23) and the kind is
// developer-correctable (see ConfirmablePaths).
//
// Raw pass only. An aggregated read of a counter with groupType mean smooths the
// steps and can hide a reset entirely.
func detectValueKind(column timeseries.Column, declaredType models.Type) kindResult {
	n := column.Len()
	if n < minKindPoints {
		reason := ReasonInsufficientCoverage
		detail := fmt.Sprintf("read %d points, need at least %d", n, minKindPoints)
		return kindResult{
			Kind:       Uncomputable[ValueKind](reason, detail),
			Confidence: Uncomputable[Confidence](reason, detail),
			Evidence:   Uncomputable[KindEvidence](reason, detail),
			Resets:     Uncomputable[[]time.Time](reason, detail),
		}
	}

	distinct := map[string]struct{}{}
	for _, v := range column.Values {
		distinct[timeseries.DistinctKey(v)] = struct{}{}
	}

	times, values, dropped := column.Numeric()
	evidence := KindEvidence{
		DistinctValues:  len(distinct),
		NonNumericRatio: round2(float64(dropped) / float64(n)),
	}

	// A mostly non-numeric column is a label, not a measurement. Ordering it or
	// taking its mean would be meaningless, so no monotonic ratio is reported.
	if evidence.NonNumericRatio > 0.5 {
		kind := KindCategorical
		if len(distinct) <= statusDistinctMax {
			kind = KindStatus
		}
		return kindResult{
			Kind:       Computed(kind),
			Confidence: Computed(Likely),
			Evidence:   Computed(evidence),
			Resets: Uncomputablef[[]time.Time](ReasonWrongKind,
				"%s values are not numeric, so there is nothing to count", kind),
		}
	}

	deltas := diffFloats(values)
	nonNegative := 0
	for _, delta := range deltas {
		if delta >= 0 {
			nonNegative++
		} else {
			evidence.NegativeDeltas++
		}
	}
	if len(deltas) > 0 {
		evidence.MonotonicRatio = round2(float64(nonNegative) / float64(len(deltas)))
	}

	resets := counterResetIndices(values)
	kind := KindInstantaneous
	confidence := Likely
	switch {
	case declaredType == models.Boolean || isBinaryValued(values, len(distinct)):
		kind = KindBinary
	case len(deltas) > 0 && evidence.MonotonicRatio >= monotonicThreshold && risesLikeACounter(values, resets):
		kind = KindCumulativeCounter
	}
	if n < confidentKindSize {
		confidence = Uncertain
	}
	// A ratio sitting on the threshold is exactly the case not to be confident
	// about: it is a counter with unflagged resets or a noisy rising signal, and
	// which one matters.
	if kind == KindCumulativeCounter && evidence.MonotonicRatio < 0.99 {
		confidence = Uncertain
	}

	result := kindResult{
		Kind:       Computed(kind),
		Confidence: Computed(confidence),
		Evidence:   Computed(evidence),
	}
	if kind == KindCumulativeCounter {
		result.Resets = Computed(resetTimes(times, resets))
	} else {
		result.Resets = Uncomputablef[[]time.Time](ReasonWrongKind,
			"the series is %s, not a cumulative counter", kind)
	}
	return result
}

// risesLikeACounter asks the two questions that separate a register from
// everything else that clears the monotonic threshold: does it make forward
// progress at all, and are the drops it takes exceptional rather than part of how
// it works.
//
// Neither is what the endpoints answer. Comparing the last reading against the
// first put a meter replaced or rolled over late in the raw window below where it
// started, and reported a register whose monotonic ratio is 1.00 as
// instantaneous — evidence contradicting its own verdict, the opposite of what
// D23 asks for, and the misclassification M1b lists as must-never-happen. D25
// anchors the raw window at the most recent data, so it landed preferentially on
// the freshly replaced meters someone is actually looking at, and a register with
// a daily rollover met it about half the time.
//
// Counting instead is what keeps an on/off load out. Long idle stretches make
// almost every delta exactly zero — an oven idling at 5 W for twenty-three hours
// a day has a monotonic ratio of 0.98 — so the monotonic threshold alone does not
// exclude it, and it does rise between its own switch-offs. What it does not do is
// rise more often than it falls back.
func risesLikeACounter(values []float64, resets []int) bool {
	increases := 0
	for i := 1; i < len(values); i++ {
		if values[i] > values[i-1] {
			increases++
		}
	}
	// A series that never rises is a frozen sensor rather than a meter, which is
	// the case the monotonic ratio of a constant column reads as 1.0.
	if increases == 0 {
		return false
	}
	return float64(len(resets)) <= maxResetShare*float64(increases)
}

func isBinaryValued(values []float64, distinct int) bool {
	if distinct > 2 || len(values) == 0 {
		return false
	}
	for _, v := range values {
		if v != 0 && v != 1 {
			return false
		}
	}
	return true
}

// counterResetIndices finds the steps where a counter went backwards far enough
// to be a rollover or a replacement rather than jitter, in ascending order.
//
// This is what makes a counter usable at all: differencing across an unflagged
// reset produces a large negative energy figure, and summing that into a daily
// total silently loses a day. It is also what the kind detector needs before it
// has a kind, which is why the indices and their timestamps are separate steps.
func counterResetIndices(values []float64) []int {
	resets := []int{}
	for i := 1; i < len(values); i++ {
		previous, current := values[i-1], values[i]
		if current >= previous {
			continue
		}
		if previous <= 0 {
			// Below zero there is no meaningful drop ratio; a decrease from a
			// non-positive counter reading is reported as a reset because a
			// counter should not be there in the first place.
			resets = append(resets, i)
			continue
		}
		if current < previous*(1-resetDropRatio) {
			resets = append(resets, i)
		}
	}
	return resets
}

// resetTimes puts the reset indices back on the clock.
func resetTimes(times []time.Time, resets []int) []time.Time {
	out := make([]time.Time, 0, len(resets))
	for _, index := range resets {
		if index < len(times) {
			out = append(out, times[index])
		}
	}
	return out
}

// detectRangeViolation is the share of values outside the range the
// characteristic declares (§5.4.11) — a quality signal with an ontology basis
// rather than a heuristic.
func detectRangeViolation(values []float64, declared DeclaredRange) Value[float64] {
	min, hasMin := declared.Min.Get()
	max, hasMax := declared.Max.Get()
	if !hasMin && !hasMax {
		return Uncomputable[float64](ReasonOutOfScope,
			"the characteristic declares no range to check against")
	}
	if len(values) == 0 {
		return Uncomputable[float64](ReasonInsufficientCoverage, "no numeric values were read")
	}

	violations := 0
	for _, v := range values {
		if hasMin && v < min {
			violations++
			continue
		}
		if hasMax && v > max {
			violations++
		}
	}
	return Computed(round2(float64(violations) / float64(len(values))))
}

func diffFloats(values []float64) []float64 {
	if len(values) < 2 {
		return nil
	}
	out := make([]float64, 0, len(values)-1)
	for i := 1; i < len(values); i++ {
		out = append(out, values[i]-values[i-1])
	}
	return out
}
