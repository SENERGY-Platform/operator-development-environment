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
	"sort"
	"time"

	"github.com/SENERGY-Platform/models/go/models"
)

const (
	// gapMultiple: an inter-arrival delta longer than this many modal intervals
	// is a gap rather than jitter. Three is loose enough that a late message is
	// not a gap and tight enough that a missed pair of readings is.
	gapMultiple = 3.0
	// regularityTolerance is how far a delta may sit from the modal interval and
	// still count as on schedule.
	regularityTolerance = 0.10
	regularThreshold    = 0.05
	irregularThreshold  = 0.30
	// minSamplingPoints: two deltas is the least that can distinguish a regular
	// series from a single coincidence.
	minSamplingPoints = 3
	// confidentSampleSize is where the irregularity ratio stops being an
	// accident of a short window.
	confidentSampleSize = 100
	// maxGapsRecorded bounds the gap list. The projection collapses it for the
	// LLM anyway, and an unbounded list on a badly broken series is a memory
	// problem rather than a finding.
	maxGapsRecorded = 5000
)

// detectSampling is detector 1 of §5.4.13: modal inter-arrival delta,
// irregularity ratio and gap list, from the raw pass.
//
// It must be the raw pass. With groupTime set the server fills or smooths every
// bucket, so the deltas become the bucket width and both the irregularity and
// the gaps disappear (§5.3.2) — the detector would report a perfectly regular
// series with no gaps whatever the data looked like.
//
// The returned interval is the modal one, and is what the rest of the profile
// sizes itself against: the aggregation bucket, the expected point count and
// the session detector's minimum duration all derive from it.
func detectSampling(times []time.Time) (Value[Sampling], float64) {
	if len(times) < minSamplingPoints {
		return Uncomputablef[Sampling](ReasonInsufficientCoverage,
			"%d points read, need at least %d to establish an interval", len(times), minSamplingPoints), 0
	}

	deltas := deltasSeconds(times)
	interval, share, ok := modal(deltas)
	if !ok || interval <= 0 {
		return Uncomputable[Sampling](ReasonInsufficientCoverage,
			"no positive inter-arrival delta: the timestamps do not advance"), 0
	}

	onSchedule := 0
	for _, delta := range deltas {
		if withinRatio(delta, interval, regularityTolerance) {
			onSchedule++
		}
	}
	irregularity := 1 - float64(onSchedule)/float64(len(deltas))

	sampling := Sampling{
		DetectedIntervalS: roundTo(interval, 3),
		IrregularityRatio: round2(irregularity),
		Gaps:              detectGaps(times, deltas, interval),
	}

	switch {
	case irregularity <= regularThreshold:
		sampling.Regularity = Regular
	case irregularity >= irregularThreshold:
		sampling.Regularity = Irregular
	default:
		sampling.Regularity = Mixed
	}

	// Confidence stays ordinal (D23) and never reaches certain: certain is for
	// ontology-derived and developer-confirmed values only. The evidence for
	// this judgement is the irregularity ratio and the modal share, both of
	// which are in the profile.
	if len(deltas) >= confidentSampleSize && (share >= 0.8 || irregularity >= irregularThreshold) {
		sampling.Confidence = Likely
	} else {
		sampling.Confidence = Uncertain
	}

	return Computed(sampling), interval
}

// detectGaps finds the deltas that are too long to be jitter. Classification is
// left to classifyGaps, which needs context this function does not have.
func detectGaps(times []time.Time, deltas []float64, interval float64) []Gap {
	threshold := gapMultiple * interval
	gaps := make([]Gap, 0)
	for i, delta := range deltas {
		if delta <= threshold {
			continue
		}
		if len(gaps) >= maxGapsRecorded {
			break
		}
		gaps = append(gaps, Gap{
			From:           times[i],
			To:             times[i+1],
			DurationS:      roundTo(delta, 3),
			Classification: GapUnknown,
		})
	}
	return gaps
}

// classifyGaps is detector 4 of §5.4.13. A gap while the device was offline is
// expected rather than a sensor fault, and separating the two materially reduces
// false quality flags.
//
// §5.4.13 suggests correlating against connection-state history. The platform's
// device repository serves only the *current* state — history lives in a
// separate connection-log service that ODE does not consume and that §5.3 does
// not list. So this uses two signals it can actually stand behind:
//
//   - Siblings. If other variables of the same service carry values inside the
//     gap, the device was reporting and this channel was not, which is a sensor
//     fault. This is stronger evidence than connection state for an interior
//     gap, and the service-scoped batch makes it free.
//   - The current state, for the trailing gap only. A gap that runs to the end
//     of the window on a device that is offline now is the device being offline.
//
// Everything else stays `unknown`, which is the honest answer and, by D24, is
// distinguishable from "not a fault".
func classifyGaps(gaps []Gap, siblingTimes []time.Time, state models.ConnectionState, windowEnd time.Time) []Gap {
	if len(gaps) == 0 {
		return gaps
	}
	out := make([]Gap, len(gaps))
	copy(out, gaps)

	for i := range out {
		if out[i].Classification == "" {
			// An unset classification is not a valid one, and letting it through
			// would read as a fourth, undocumented state.
			out[i].Classification = GapUnknown
		}
		if siblingsReportedDuring(siblingTimes, out[i]) {
			out[i].Classification = GapSensorFault
			continue
		}
		trailing := !windowEnd.IsZero() && !out[i].To.Before(windowEnd.Add(-time.Second))
		if trailing && state == models.ConnectionStateOffline {
			out[i].Classification = GapDeviceOffline
		}
	}
	return out
}

// siblingsReportedDuring reports whether any sibling variable produced a value
// strictly inside the gap. Strictly, because the gap's own endpoints are
// timestamps this variable produced, and a sibling sharing them says nothing.
//
// siblingTimes must be sorted ascending, which is what makes this a search rather
// than a scan. It matters at the raw point limit: a linear scan is
// gaps × sibling points, and at a hundred thousand points across three siblings
// with a few thousand gaps that is billions of comparisons for a field nobody
// would wait for.
func siblingsReportedDuring(siblingTimes []time.Time, gap Gap) bool {
	next := sort.Search(len(siblingTimes), func(i int) bool {
		return siblingTimes[i].After(gap.From)
	})
	return next < len(siblingTimes) && siblingTimes[next].Before(gap.To)
}

// computeCoverage compares the points actually read against the points the
// detected interval implies for the window.
//
// It is computed over the raw window, not the analysis window: the aggregated
// pass returns one row per bucket whether or not the bucket held any data, so a
// completeness ratio taken from it would be 1.0 for a series with a six-month
// hole in it.
func computeCoverage(points int, interval float64, window Window) Value[Coverage] {
	if interval <= 0 {
		return Uncomputable[Coverage](ReasonInsufficientCoverage,
			"no sampling interval, so no expected point count")
	}
	if !window.Valid() {
		return Uncomputable[Coverage](ReasonInsufficientSpan, "the read window is empty")
	}

	expected := int(window.Duration().Seconds() / interval)
	if expected <= 0 {
		return Uncomputablef[Coverage](ReasonInsufficientSpan,
			"the window spans less than one interval of %.3fs", interval)
	}

	// The ratio is capped at 1. More points than the modal interval implies does
	// not mean a series is 458% complete — it means the interval understates its
	// density, which is a bursty or irregular series rather than an over-complete
	// one. Reporting the excess as completeness would put a nonsensical percentage
	// in front of a developer and, worse, past the statistical gate as if it were
	// a strong reading.
	//
	// Nothing is lost by capping: the exact counts are right here, and the
	// irregularity ratio next door is the field that describes the density.
	ratio := float64(points) / float64(expected)
	if ratio > 1 {
		ratio = 1
	}
	return Computed(Coverage{
		NPoints:           points,
		ExpectedPoints:    expected,
		CompletenessRatio: round2(ratio),
	})
}

// minCoverageForStatistics is the completeness a series needs before the
// distribution and temporal detectors report a number rather than
// insufficient_coverage.
//
// The threshold exists because those detectors cannot tell a sparse series from
// a dense one — a mean over 30% of a year's data is a mean over whatever
// happened to arrive, and an LLM reading it has no way to discount it. 0.8 is
// the figure §5.4.6's own example uses.
const minCoverageForStatistics = 0.8
