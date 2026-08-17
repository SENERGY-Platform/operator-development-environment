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
	"math"
	"time"
)

const (
	// minConstantRunPoints is the shortest run of identical values worth
	// recording. Two equal readings in a row is ordinary; three is a pattern.
	minConstantRunPoints = 3
	maxConstantRuns      = 1000
	// minDistributionBuckets is the fewest buckets that make percentiles mean
	// anything at all.
	minDistributionBuckets = 10
)

// aggregatedSeries is the three views of one variable the aggregated pass reads:
// the mean, minimum and maximum per bucket.
//
// Three columns rather than one because a distribution taken from mean buckets
// alone understates both tails — the maximum of a series is not the maximum of
// its bucket means. Asking the server for min and max buckets as well costs two
// more elements in the same batched request and makes those two figures true.
type aggregatedSeries struct {
	Times []time.Time
	Mean  []float64
	Min   []float64
	Max   []float64
}

// detectDistribution computes the distribution from the aggregated pass over
// the full analysis window (§5.3.2).
//
// min and max are exact, because they come from min and max buckets. mean is the
// mean of bucket means, which equals the true mean for equal-width buckets
// holding equal point counts and is close otherwise. The percentiles are
// percentiles *of bucket means*: bucketing pulls them towards the centre, and
// the provenance entry records that so nobody reads p99 as a true 99th
// percentile of readings.
func detectDistribution(series aggregatedSeries, coverage Value[Coverage]) Value[Distribution] {
	if block, blocked := coverageTooLow(coverage); blocked {
		return uncomputableFrom[Distribution](block)
	}

	means := finite(series.Mean)
	if len(means) < minDistributionBuckets {
		return Uncomputablef[Distribution](ReasonInsufficientCoverage,
			"%d aggregated buckets, need at least %d for a distribution", len(means), minDistributionBuckets)
	}

	sorted := sortedCopy(means)
	zeros := 0
	for _, v := range means {
		if v == 0 {
			zeros++
		}
	}

	distribution := Distribution{
		Mean:      roundTo(mean(means), 4),
		Median:    roundTo(percentile(sorted, 0.5), 4),
		P01:       roundTo(percentile(sorted, 0.01), 4),
		P99:       roundTo(percentile(sorted, 0.99), 4),
		StdDev:    roundTo(stddev(means), 4),
		ZeroRatio: round2(float64(zeros) / float64(len(means))),
		// ConstantRuns comes from the raw pass and is attached separately.
		ConstantRuns: []ConstantRun{},
	}
	distribution.Min = roundTo(minOf(finite(series.Min), sorted[0]), 4)
	distribution.Max = roundTo(maxOf(finite(series.Max), sorted[len(sorted)-1]), 4)

	return Computed(distribution)
}

// detectConstantRuns finds stretches where the value did not change at all.
//
// Raw pass only. A bucketed mean of a jittering sensor is never exactly
// constant, so an aggregated read turns a frozen sensor into a merely quiet one
// and the quality flag never fires.
func detectConstantRuns(times []time.Time, values []float64) Value[[]ConstantRun] {
	if len(values) < minConstantRunPoints {
		return Uncomputablef[[]ConstantRun](ReasonInsufficientCoverage,
			"%d raw points, need at least %d", len(values), minConstantRunPoints)
	}

	runs := []ConstantRun{}
	start := 0
	flush := func(end int) {
		points := end - start + 1
		if points < minConstantRunPoints || len(runs) >= maxConstantRuns {
			return
		}
		runs = append(runs, ConstantRun{
			From:      times[start],
			To:        times[end],
			Value:     values[start],
			DurationS: roundTo(times[end].Sub(times[start]).Seconds(), 3),
			Points:    points,
		})
	}

	for i := 1; i < len(values); i++ {
		if values[i] == values[start] {
			continue
		}
		flush(i - 1)
		start = i
	}
	flush(len(values) - 1)

	return Computed(runs)
}

// longestRun is the evidence behind the frozen-sensor flag.
func longestRun(runs []ConstantRun) (ConstantRun, bool) {
	if len(runs) == 0 {
		return ConstantRun{}, false
	}
	longest := runs[0]
	for _, run := range runs[1:] {
		if run.DurationS > longest.DurationS {
			longest = run
		}
	}
	return longest, true
}

// coverageBlock is one not_computed decision that applies to several fields at
// once, so the same wording is not rebuilt per detector.
type coverageBlock struct {
	reason NotComputedReason
	detail string
}

func uncomputableFrom[T any](block coverageBlock) Value[T] {
	return Uncomputable[T](block.reason, block.detail)
}

// coverageTooLow decides whether the statistical detectors may report a number.
//
// A mean over 30% of a window is a mean over whatever happened to arrive, and
// nothing downstream can discount it — which is why the answer is
// insufficient_coverage with the ratio in the detail rather than a number with a
// caveat nobody reads.
func coverageTooLow(coverage Value[Coverage]) (coverageBlock, bool) {
	value, ok := coverage.Get()
	if !ok {
		status := coverage.Status()
		return coverageBlock{reason: status.Reason, detail: "coverage unknown: " + status.Detail}, true
	}
	if value.CompletenessRatio < minCoverageForStatistics {
		return coverageBlock{
			reason: ReasonInsufficientCoverage,
			detail: fmt.Sprintf("completeness_ratio %.2f < %.2f",
				value.CompletenessRatio, minCoverageForStatistics),
		}, true
	}
	return coverageBlock{}, false
}

func minOf(values []float64, fallback float64) float64 {
	if len(values) == 0 {
		return fallback
	}
	out := math.Inf(1)
	for _, v := range values {
		if v < out {
			out = v
		}
	}
	return out
}

func maxOf(values []float64, fallback float64) float64 {
	if len(values) == 0 {
		return fallback
	}
	out := math.Inf(-1)
	for _, v := range values {
		if v > out {
			out = v
		}
	}
	return out
}
