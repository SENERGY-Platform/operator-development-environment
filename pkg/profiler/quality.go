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
	"time"
)

const (
	// frozenRunMultiple: a value unchanged for this many sampling intervals is a
	// frozen sensor rather than a quiet one.
	frozenRunMultiple = 100
	// minFrozenSeconds keeps a fast series from being flagged for a minute of
	// stillness.
	minFrozenSeconds  = 3600
	maxDSTTransitions = 10
)

// qualityInput is everything detector 9 needs, gathered from the detectors that
// ran before it.
type qualityInput struct {
	Variable      Variable
	Sampling      Value[Sampling]
	Coverage      Value[Coverage]
	Semantics     ValueSemantics
	ConstantRuns  Value[[]ConstantRun]
	RawValues     []float64
	Relationships []Relationship
	Window        Window
	Interval      float64
	LocalZone     *time.Location
}

// detectQualityFlags is detector 9 of §5.4.13.
//
// On confidence: §5.4.12's illustrative JSON shows a frozen-sensor flag as
// `certain`, but D23 reserves certain for ontology-derived and
// developer-confirmed values, and a locked decision outranks an example. So the
// measured facts are exact and travel as evidence, while the conclusion drawn
// from them is `likely` — except for the range violation, whose bound comes from
// the ontology and whose comparison is exact, which is precisely D23's carve-out.
func detectQualityFlags(input qualityInput) []QualityFlag {
	flags := []QualityFlag{}

	if !input.Variable.Streamed() {
		// Detector 5: a request-only service is polled, not streamed, so
		// whatever is in the database did not arrive as a time series.
		flags = append(flags, QualityFlag{
			Flag:       FlagNotStreamed,
			Confidence: Certain,
			Evidence: map[string]any{
				"interaction": string(input.Variable.Interaction),
			},
		})
	}

	if runs, ok := input.ConstantRuns.Get(); ok && input.Interval > 0 {
		if longest, found := longestRun(runs); found {
			threshold := frozenRunMultiple * input.Interval
			if threshold < minFrozenSeconds {
				threshold = minFrozenSeconds
			}
			if longest.DurationS > threshold {
				flags = append(flags, QualityFlag{
					Flag:       FlagFrozenSensor,
					Confidence: Likely,
					Evidence: map[string]any{
						"longest_constant_run_s": longest.DurationS,
						"value":                  longest.Value,
						"from":                   longest.From,
						"to":                     longest.To,
						"threshold_s":            roundTo(threshold, 1),
					},
				})
			}
		}
	}

	if ratio, ok := input.Semantics.RangeViolationRatio.Get(); ok && ratio > 0 {
		evidence := map[string]any{"range_violation_ratio": ratio}
		if min, ok := input.Semantics.DeclaredRange.Min.Get(); ok {
			evidence["declared_min"] = min
		}
		if max, ok := input.Semantics.DeclaredRange.Max.Get(); ok {
			evidence["declared_max"] = max
		}
		if input.Semantics.CharacteristicID != nil {
			evidence["characteristic_id"] = *input.Semantics.CharacteristicID
		}
		flags = append(flags, QualityFlag{
			Flag: FlagRangeViolation,
			// The bound is declared by the ontology and the comparison is exact.
			Confidence: Certain,
			Evidence:   evidence,
		})
	}

	if kind, ok := input.Semantics.Kind.Get(); ok && kind == KindCumulativeCounter {
		negatives := 0
		for _, v := range input.RawValues {
			if v < 0 {
				negatives++
			}
		}
		if negatives > 0 {
			flags = append(flags, QualityFlag{
				Flag:       FlagNegativeOnUnsigned,
				Confidence: Likely,
				Evidence: map[string]any{
					"negative_values": negatives,
					"points":          len(input.RawValues),
					"reason":          "a cumulative counter should not hold a negative reading",
				},
			})
		}
		if resets, ok := input.Semantics.CounterResets.Get(); ok && len(resets) > 0 {
			shown := resets
			if len(shown) > 5 {
				shown = shown[:5]
			}
			flags = append(flags, QualityFlag{
				Flag:       FlagUnflaggedReset,
				Confidence: Likely,
				Evidence: map[string]any{
					"resets": len(resets),
					"first":  shown,
					"reason": "the platform carries no reset marker, so differencing across these points must exclude them",
				},
			})
		}
	}

	if coverage, ok := input.Coverage.Get(); ok && coverage.CompletenessRatio < minCoverageForStatistics {
		flags = append(flags, QualityFlag{
			Flag:       FlagSparseCoverage,
			Confidence: Likely,
			Evidence: map[string]any{
				"completeness_ratio": coverage.CompletenessRatio,
				"n_points":           coverage.NPoints,
				"expected_points":    coverage.ExpectedPoints,
				"threshold":          minCoverageForStatistics,
			},
		})
	}

	for _, relationship := range input.Relationships {
		if relationship.Type != RelationInconsistentWith {
			continue
		}
		flags = append(flags, QualityFlag{
			Flag:       FlagInconsistentSibling,
			Confidence: relationship.Confidence,
			Evidence: map[string]any{
				"other_path":     relationship.OtherPath,
				"correlation":    relationship.Evidence.Correlation,
				"residual_ratio": relationship.Evidence.ResidualRatio,
				"overlap_points": relationship.Evidence.OverlapPoints,
			},
		})
	}

	if transitions := dstTransitions(input.Window, input.LocalZone); len(transitions) > 0 {
		flags = append(flags, QualityFlag{
			Flag:       FlagDSTAmbiguity,
			Confidence: Certain,
			Evidence: map[string]any{
				"zone":        input.LocalZone.String(),
				"transitions": transitions,
				"reason":      "local-time bucketing across these instants repeats or skips an hour",
			},
		})
	}

	return flags
}

// dstTransitions lists the days in the window on which the local UTC offset
// changed.
//
// Time handling is not optional in this domain (§5.4.13): silent DST bugs in
// 15-minute meter data are a recurring failure mode, because one local day has
// 23 hours and another 25, and a naive daily sum is wrong twice a year. ODE
// stores and computes in UTC, so the flag is a warning about interpreting the
// series in local time rather than a fault in the data.
func dstTransitions(window Window, zone *time.Location) []time.Time {
	if zone == nil || zone == time.UTC || !window.Valid() {
		return nil
	}

	out := []time.Time{}
	previous := window.From.In(zone)
	_, previousOffset := previous.Zone()

	for at := window.From.Add(24 * time.Hour); at.Before(window.To); at = at.Add(24 * time.Hour) {
		local := at.In(zone)
		_, offset := local.Zone()
		if offset != previousOffset {
			out = append(out, at.UTC())
			previousOffset = offset
			if len(out) >= maxDSTTransitions {
				break
			}
		}
	}
	return out
}
