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

	kind := KindInstantaneous
	confidence := Likely
	switch {
	case declaredType == models.Boolean || isBinaryValued(values, len(distinct)):
		kind = KindBinary
	case len(deltas) > 0 && evidence.MonotonicRatio >= monotonicThreshold && rises(values):
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
		result.Resets = Computed(detectCounterResets(times, values))
	} else {
		result.Resets = Uncomputablef[[]time.Time](ReasonWrongKind,
			"the series is %s, not a cumulative counter", kind)
	}
	return result
}

// rises guards against calling a flat series a counter. A constant series has a
// monotonic ratio of 1.0 because every delta is zero, which is a frozen sensor
// rather than a meter.
func rises(values []float64) bool {
	return len(values) > 1 && values[len(values)-1] > values[0]
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

// detectCounterResets finds the steps where a counter went backwards far enough
// to be a rollover or a replacement rather than jitter.
//
// This is what makes a counter usable at all: differencing across an unflagged
// reset produces a large negative energy figure, and summing that into a daily
// total silently loses a day.
func detectCounterResets(times []time.Time, values []float64) []time.Time {
	resets := []time.Time{}
	for i := 1; i < len(values); i++ {
		previous, current := values[i-1], values[i]
		if current >= previous {
			continue
		}
		if previous <= 0 {
			// Below zero there is no meaningful drop ratio; a decrease from a
			// non-positive counter reading is reported as a reset because a
			// counter should not be there in the first place.
			resets = append(resets, times[i])
			continue
		}
		if current < previous*(1-resetDropRatio) {
			resets = append(resets, times[i])
		}
	}
	return resets
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
