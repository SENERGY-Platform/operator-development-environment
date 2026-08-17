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
	"math"
	"sort"
	"time"

	"gonum.org/v1/gonum/stat"
)

// Numerics come from gonum where gonum has them (D30, §5.4.14). Two entries in
// that table do not exist in the library as described: there is no
// stat.AutoCorrelation and no stat/regression package. The autocorrelation is a
// few lines of plain Go (see periodicity.go) and the OLS goes through gonum/mat,
// which is the same dependency by a different door.

func sortedCopy(xs []float64) []float64 {
	out := append([]float64{}, xs...)
	sort.Float64s(out)
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return stat.Mean(xs, nil)
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	return stat.StdDev(xs, nil)
}

// percentile takes an already sorted slice. LinInterp rather than Empirical:
// on the p01 and p99 of a few thousand points the empirical quantile snaps to a
// sample value, which reads as a suspiciously round distribution tail.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	return stat.Quantile(p, stat.LinInterp, sorted, nil)
}

func median(xs []float64) float64 {
	return percentile(sortedCopy(xs), 0.5)
}

// deltasSeconds is the inter-arrival series. Times must be ascending, which
// DecodeResults guarantees.
func deltasSeconds(times []time.Time) []float64 {
	if len(times) < 2 {
		return nil
	}
	out := make([]float64, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		out = append(out, times[i].Sub(times[i-1]).Seconds())
	}
	return out
}

// modalBucketRatio is the width of the relative buckets the modal interval is
// found in: 5% either side.
//
// Relative rather than absolute because sampling intervals in this domain span
// seconds to hours, and a fixed bucket that resolves a 1-second series merges
// every hourly one. Jitter on a nominally regular series is proportional, so
// relative buckets are also the shape of the noise.
const modalBucketRatio = 1.05

// modal returns the most common value under relative bucketing, refined to the
// median of its bucket, together with the share of values that fell in it.
func modal(values []float64) (mode float64, share float64, ok bool) {
	if len(values) == 0 {
		return 0, 0, false
	}

	buckets := map[int][]float64{}
	for _, v := range values {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		key := int(math.Round(math.Log(v) / math.Log(modalBucketRatio)))
		buckets[key] = append(buckets[key], v)
	}
	if len(buckets) == 0 {
		return 0, 0, false
	}

	bestKey, bestCount := 0, -1
	for key, members := range buckets {
		// Ties break on the smaller key so the result does not depend on map
		// iteration order.
		if len(members) > bestCount || (len(members) == bestCount && key < bestKey) {
			bestKey, bestCount = key, len(members)
		}
	}
	return median(buckets[bestKey]), float64(bestCount) / float64(len(values)), true
}

// withinRatio reports whether a is within tolerance (relative) of b.
func withinRatio(a, b, tolerance float64) bool {
	if b == 0 {
		return a == 0
	}
	return math.Abs(a-b)/math.Abs(b) <= tolerance
}

// round2 keeps evidence numbers legible. Detector output is read by people and
// by an LLM, and fifteen significant digits on a ratio invites both to read
// precision that is not there.
func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func roundTo(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	factor := math.Pow(10, float64(places))
	return math.Round(v*factor) / factor
}

// signif rounds to a number of significant figures rather than decimal places.
// A conversion factor can be 3600 or its reciprocal, and fixed decimals turn
// 0.000277 into 0.0003 — which reads back as a factor of 3333 and makes a unit
// error look like a different unit error.
func signif(v float64, digits int) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	magnitude := math.Pow(10, float64(digits)-math.Ceil(math.Log10(math.Abs(v))))
	return math.Round(v*magnitude) / magnitude
}

// finite filters the values the detectors can work with. A NaN reaching a
// percentile makes the whole distribution NaN, which is a hard failure to read
// back to a cause.
func finite(xs []float64) []float64 {
	out := make([]float64, 0, len(xs))
	for _, v := range xs {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out = append(out, v)
		}
	}
	return out
}
