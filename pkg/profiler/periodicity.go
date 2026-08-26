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
	"sort"
	"strings"
	"time"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	// acfPeakThreshold is the autocorrelation a lag needs before it counts as a
	// period rather than as noise.
	acfPeakThreshold = 0.2
	// maxReportedPeriods keeps the list to what a reader can act on.
	maxReportedPeriods = 3
	// minCyclesObserved: a period cannot be claimed from less than two full
	// cycles. Reporting a weekly cycle from ten days of data is asserting a
	// pattern from one repetition.
	minCyclesObserved = 2
	// minPeriodicityBuckets is the shortest usable grid.
	minPeriodicityBuckets = 24
	// minGroupTimeSeconds is the finest aggregation bucket the profiler acts on.
	//
	// It exists because group_time is a free-form string in the schema published
	// to the model (§5.8) and the detectors size their grid from the window
	// divided by the bucket, before any data is looked at.
	minGroupTimeSeconds = 1.0
	// maxAggregatedBuckets bounds that grid. One float64 and one bool per bucket
	// puts half a million at about 4.5 MB per variable — generous beside the 4000
	// buckets the aggregated pass aims for, and far short of the size at which the
	// allocation is the failure. "1ms" over a year asks for 31.5 billion, which is
	// not a slow profile but a dead process: nothing in pkg/ recovers from a panic.
	maxAggregatedBuckets = 500000
	// residualFloor is where a residual stops being a series and becomes
	// floating-point dust. A cycle that explains the grid to within a millionth of
	// its energy explains it entirely, and the autocorrelation of what is left
	// then describes rounding rather than the sensor.
	residualFloor = 1e-6
)

// detectPeriodicity is detector 6 of §5.4.13: ACF peaks plus FFT, with the daily
// and weekly cycles reported by name.
//
// Aggregated pass, over the full range. Both methods need a uniform grid, which
// is what a groupTime bucket produces — and a period shorter than twice the
// bucket cannot be seen at all, so the bucket width is recorded in the
// provenance entry beside the result.
func detectPeriodicity(series aggregatedSeries, window Window, bucketSeconds float64, coverage Value[Coverage]) (Value[[]float64], Value[[]PeriodEvidence]) {
	if block, blocked := coverageTooLow(coverage); blocked {
		return uncomputableFrom[[]float64](block), uncomputableFrom[[]PeriodEvidence](block)
	}
	if bucketSeconds <= 0 {
		reason := Uncomputable[[]float64](ReasonOutOfScope, "no aggregation bucket width")
		return reason, Uncomputable[[]PeriodEvidence](ReasonOutOfScope, "no aggregation bucket width")
	}
	// The grid is sized from the window and the bucket, so an absurd bucket is
	// refused before anything is allocated rather than after. ProfileService
	// rejects such a group_time at the boundary; this is the second line, for the
	// callers that do not come through it.
	if count := math.Floor(window.Duration().Seconds() / bucketSeconds); count > maxAggregatedBuckets {
		detail := "a bucket of %gs over %s implies %.0f grid points, above the %d the detectors will build"
		return Uncomputablef[[]float64](ReasonOutOfScope, detail, bucketSeconds, window.String(), count, maxAggregatedBuckets),
			Uncomputablef[[]PeriodEvidence](ReasonOutOfScope, detail, bucketSeconds, window.String(), count, maxAggregatedBuckets)
	}

	grid := onUniformGrid(series, window, bucketSeconds)
	if len(grid) < minPeriodicityBuckets {
		detail := "only %d buckets on the uniform grid, need at least %d"
		return Uncomputablef[[]float64](ReasonInsufficientSpan, detail, len(grid), minPeriodicityBuckets),
			Uncomputablef[[]PeriodEvidence](ReasonInsufficientSpan, detail, len(grid), minPeriodicityBuckets)
	}

	evidence := []PeriodEvidence{}
	evidence = append(evidence, acfPeriods(grid, bucketSeconds)...)
	evidence = append(evidence, fftPeriods(grid, bucketSeconds)...)
	evidence = append(evidence, namedCycles(grid, bucketSeconds)...)

	sort.SliceStable(evidence, func(i, j int) bool { return evidence[i].Strength > evidence[j].Strength })

	periods := []float64{}
	seen := map[int]bool{}
	kept := []PeriodEvidence{}
	for _, candidate := range evidence {
		// Round to the bucket, since two methods finding the same cycle should
		// report one period rather than two that differ in the third decimal.
		key := int(math.Round(candidate.PeriodS / bucketSeconds))
		if key == 0 || seen[key] {
			continue
		}
		// Two methods finding one cycle report one period, not two that differ by
		// less than either of them can resolve. An FFT bin is wide: at 720 buckets
		// bin 4 is 180h with its neighbours at 144h and 240h, so it and the exact
		// 168h the ACF found are the same finding stated twice — and reported
		// separately they read as a 7-day and a 7.5-day cycle.
		if index, overlaps := withinResolution(kept, candidate); overlaps {
			kept[index] = finerOf(kept[index], candidate)
			periods[index] = kept[index].PeriodS
			continue
		}
		if isHarmonicOf(candidate, periods) {
			continue
		}
		seen[key] = true
		kept = append(kept, candidate)
		periods = append(periods, candidate.PeriodS)
		if len(periods) >= maxReportedPeriods {
			break
		}
	}

	if len(periods) == 0 {
		// This is the case D24 exists for: "no period found" and "could not
		// look" must not read the same. Here the detector did run, so the answer
		// is a genuine empty list rather than not_computed.
		return Computed([]float64{}), Computed([]PeriodEvidence{})
	}

	sort.Float64s(periods)
	return Computed(periods), Computed(kept)
}

// withinResolution finds a kept period the candidate cannot be told apart from,
// judged by the coarser of the two resolutions.
func withinResolution(kept []PeriodEvidence, candidate PeriodEvidence) (int, bool) {
	for i, existing := range kept {
		tolerance := math.Max(existing.ResolutionS, candidate.ResolutionS)
		if tolerance > 0 && math.Abs(existing.PeriodS-candidate.PeriodS) <= tolerance {
			return i, true
		}
	}
	return -1, false
}

// finerOf merges a candidate into the entry it agrees with: the strength and the
// method stay with the stronger finding — the list is walked strongest first, so
// that is the one already kept — while the period, its label and its resolution
// come from whichever of the two located the cycle more precisely.
//
// Keeping the stronger entry's period would report the weekly cycle as 180h with
// no label, having just discarded the exact 168h that says what it is.
func finerOf(kept, candidate PeriodEvidence) PeriodEvidence {
	if kept.ResolutionS <= candidate.ResolutionS {
		return kept
	}
	kept.PeriodS = candidate.PeriodS
	kept.Label = candidate.Label
	kept.ResolutionS = candidate.ResolutionS
	return kept
}

// harmonicTolerance is how close to an exact multiple counts as a harmonic.
const harmonicTolerance = 0.05

// isHarmonicOf reports whether a candidate is an integer multiple of a period
// already being reported.
//
// The autocorrelation of a daily cycle peaks again at two days and three days,
// and reporting those as separate findings invites the reader to model a
// three-day cycle that does not exist. The named cycles are exempt: a week
// really is seven days, and it is a distinct pattern in this domain rather than
// an artefact of the daily one, which is why §5.4.13 asks for both by name.
func isHarmonicOf(candidate PeriodEvidence, kept []float64) bool {
	if candidate.Label != "" {
		return false
	}
	for _, fundamental := range kept {
		if fundamental <= 0 || candidate.PeriodS <= fundamental {
			continue
		}
		multiple := candidate.PeriodS / fundamental
		nearest := math.Round(multiple)
		if nearest >= 2 && math.Abs(multiple-nearest)/nearest <= harmonicTolerance {
			return true
		}
	}
	return false
}

// onUniformGrid places bucket means onto the regular grid the analysis window
// implies, mean-centred, with missing buckets at zero.
//
// The server omits buckets that held no data rather than emitting an empty one,
// so the response is not uniformly spaced and neither the ACF nor the FFT can be
// applied to it directly. Missing buckets become zero *after* centring, which is
// the mean — the least-committal filling available, and the one that neither
// invents a peak nor suppresses one.
func onUniformGrid(series aggregatedSeries, window Window, bucketSeconds float64) []float64 {
	// Both checks come before the two allocations, which is the whole of the
	// ordering: sizing a slice from window-divided-by-bucket and only afterwards
	// noticing there was no data is how a model-authored "1ms" reached makeslice.
	if len(series.Times) == 0 || len(series.Mean) == 0 {
		return nil
	}
	buckets, sized := gridBuckets(window, bucketSeconds)
	if !sized {
		return nil
	}

	present := make([]bool, buckets)
	grid := make([]float64, buckets)
	observed := make([]float64, 0, len(series.Mean))

	for i, at := range series.Times {
		if i >= len(series.Mean) {
			break
		}
		value := series.Mean[i]
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		index := int(math.Floor(at.Sub(window.From).Seconds() / bucketSeconds))
		if index < 0 || index >= buckets {
			continue
		}
		grid[index] = value
		present[index] = true
		observed = append(observed, value)
	}

	if len(observed) == 0 {
		return nil
	}
	centre := mean(observed)
	for i := range grid {
		if present[i] {
			grid[i] -= centre
		} else {
			grid[i] = 0
		}
	}
	return grid
}

// gridBuckets is how many uniform buckets a window divides into, and whether
// that is a number worth allocating.
//
// The comparison is made in floating point on purpose: window-divided-by-bucket
// can exceed what an int holds, and converting first is undefined rather than
// merely large. The NaN case falls out of it, since no comparison with NaN holds.
func gridBuckets(window Window, bucketSeconds float64) (int, bool) {
	if !window.Valid() || bucketSeconds <= 0 {
		return 0, false
	}
	count := math.Floor(window.Duration().Seconds() / bucketSeconds)
	if !(count >= 1) || count > maxAggregatedBuckets {
		return 0, false
	}
	return int(count), true
}

// autocorrelation is the normalised autocovariance at each lag.
//
// gonum has no AutoCorrelation despite §5.4.14 listing one, and the definition
// is four lines, so it is here. The series is assumed already centred.
func autocorrelation(centred []float64, maxLag int) []float64 {
	var variance float64
	for _, v := range centred {
		variance += v * v
	}
	if variance == 0 {
		return nil
	}

	out := make([]float64, maxLag+1)
	out[0] = 1
	for lag := 1; lag <= maxLag; lag++ {
		var sum float64
		for i := lag; i < len(centred); i++ {
			sum += centred[i] * centred[i-lag]
		}
		out[lag] = sum / variance
	}
	return out
}

func acfPeriods(grid []float64, bucketSeconds float64) []PeriodEvidence {
	maxLag := len(grid) / minCyclesObserved
	if maxLag < 2 {
		return nil
	}
	acf := autocorrelation(grid, maxLag)
	if len(acf) == 0 {
		return nil
	}

	out := []PeriodEvidence{}
	for lag := 2; lag < len(acf)-1; lag++ {
		if acf[lag] < acfPeakThreshold {
			continue
		}
		if acf[lag] <= acf[lag-1] || acf[lag] < acf[lag+1] {
			continue
		}
		period := float64(lag) * bucketSeconds
		out = append(out, PeriodEvidence{
			PeriodS:  roundTo(period, 1),
			Method:   "acf",
			Strength: round2(acf[lag]),
			Label:    labelPeriod(period),
			// A lag is quantised to the bucket, so the peak could sit anywhere
			// within half a bucket either side of it.
			ResolutionS: roundTo(bucketSeconds/2, 3),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Strength > out[j].Strength })
	if len(out) > maxReportedPeriods {
		out = out[:maxReportedPeriods]
	}
	return out
}

// fftPeriods reads the dominant frequencies off the power spectrum.
//
// It catches what peak-picking on the ACF misses: a strong cycle whose ACF peak
// is broad enough that no single lag stands out above its neighbours. Strength
// is the share of total spectral power in the peak, which is on a different
// scale from an autocorrelation — hence the method being recorded per entry.
func fftPeriods(grid []float64, bucketSeconds float64) []PeriodEvidence {
	n := len(grid)
	if n < minPeriodicityBuckets {
		return nil
	}

	fft := fourier.NewFFT(n)
	coefficients := fft.Coefficients(nil, grid)

	power := make([]float64, len(coefficients))
	var total float64
	for i, c := range coefficients {
		power[i] = real(c)*real(c) + imag(c)*imag(c)
		if i > 0 {
			total += power[i]
		}
	}
	if total == 0 {
		return nil
	}

	type peak struct {
		index int
		power float64
	}
	peaks := []peak{}
	// Bin 0 is the mean, which centring already removed. The upper bound keeps
	// only frequencies with at least minCyclesObserved cycles in the window,
	// since anything slower is one repetition dressed as a period.
	maxIndex := n / minCyclesObserved
	for i := 1; i < len(power) && i <= maxIndex; i++ {
		peaks = append(peaks, peak{index: i, power: power[i]})
	}
	sort.SliceStable(peaks, func(i, j int) bool { return peaks[i].power > peaks[j].power })

	out := []PeriodEvidence{}
	for _, p := range peaks {
		share := p.power / total
		if share < 0.05 {
			break
		}
		frequency := fft.Freq(p.index)
		if frequency <= 0 {
			continue
		}
		period := bucketSeconds / frequency
		resolution := binResolutionS(p.index, n, bucketSeconds)
		out = append(out, PeriodEvidence{
			PeriodS:     roundTo(period, 1),
			Method:      "fft",
			Strength:    round2(share),
			Label:       labelWithinResolution(period, resolution),
			ResolutionS: roundTo(resolution, 1),
		})
		if len(out) >= maxReportedPeriods {
			break
		}
	}
	return out
}

// binResolutionS is half the range of periods an FFT bin covers.
//
// A bin is a frequency band, and the period is the reciprocal of a frequency, so
// the band is far wider in period at the low-frequency end: at 720 buckets bin 30
// resolves a day to within twenty-four minutes, while bin 4 covers everything
// from 160h to 206h. Taking the bin edges at index ± ½ is what makes that
// asymmetry come out right.
func binResolutionS(index, n int, bucketSeconds float64) float64 {
	if index < 1 || n < 1 || bucketSeconds <= 0 {
		return 0
	}
	span := bucketSeconds * float64(n)
	upper := span / (float64(index) - 0.5)
	lower := span / (float64(index) + 0.5)
	return (upper - lower) / 2
}

// labelWithinResolution names a period only where the method can tell it apart
// from a period that would be named differently.
//
// Without it a 180h FFT bin was stamped "weekly" by labelPeriod's ±10 % window
// and then skipped by the harmonic filter, which exempts labelled candidates —
// so the profile carried two entries called weekly and the model read a 7-day
// and a 7.5-day cycle off one series.
func labelWithinResolution(seconds, resolution float64) string {
	label := labelPeriod(seconds)
	if label == "" || resolution <= 0 {
		return label
	}
	if labelPeriod(seconds-resolution) != label || labelPeriod(seconds+resolution) != label {
		return ""
	}
	return label
}

// namedCycles tests the daily and weekly lags directly, because §5.4.13 asks for
// both to be reported explicitly and because a cycle that matters to the
// developer should not depend on winning a peak-picking contest against its own
// harmonics.
func namedCycles(grid []float64, bucketSeconds float64) []PeriodEvidence {
	out := []PeriodEvidence{}
	dailyLag := int(math.Round(24 * 3600 / bucketSeconds))
	for _, cycle := range []struct {
		label   string
		seconds float64
	}{
		{"daily", 24 * 3600},
		{"weekly", 7 * 24 * 3600},
	} {
		lag := int(math.Round(cycle.seconds / bucketSeconds))
		if lag < 2 || lag > len(grid)/minCyclesObserved {
			continue
		}

		tested, method := grid, "acf"
		if cycle.label == "weekly" && dailyLag >= 2 {
			// A week is an exact multiple of a day, so the autocorrelation of a
			// purely daily series is naturally high at 168h — and because
			// isHarmonicOf exempts named cycles, nothing downstream caught it. The
			// evidence was not merely uninformative but the wrong way round: a
			// sensor with no weekly structure scored *higher* on "weekly" than one
			// with a real weekend effect, and a model reads weekly seasonality off
			// that and proposes a model for it.
			//
			// So the question asked is the one worth asking: whether anything
			// weekly is left once the daily shape is accounted for. The strength
			// is a residual autocorrelation and is on a different footing from a
			// raw one, which is why the method says so.
			residual := withoutCycle(grid, dailyLag)
			if energy(residual) < residualFloor*energy(grid) {
				// The daily cycle explains the grid entirely. What remains is
				// rounding, and its autocorrelation is not evidence of a week.
				continue
			}
			tested, method = residual, "acf_daily_residual"
		}

		acf := autocorrelation(tested, lag)
		if len(acf) <= lag || acf[lag] < acfPeakThreshold {
			continue
		}
		out = append(out, PeriodEvidence{
			PeriodS:     cycle.seconds,
			Method:      method,
			Strength:    round2(acf[lag]),
			Label:       cycle.label,
			ResolutionS: roundTo(bucketSeconds/2, 3),
		})
	}
	return out
}

// withoutCycle subtracts the mean value at each phase of a cycle, leaving what
// that cycle does not explain. The grid is already centred and its missing
// buckets are zero, so the residual stays centred.
func withoutCycle(grid []float64, lag int) []float64 {
	if lag < 2 || lag >= len(grid) {
		return grid
	}
	sums := make([]float64, lag)
	counts := make([]float64, lag)
	for i, v := range grid {
		sums[i%lag] += v
		counts[i%lag]++
	}
	out := make([]float64, len(grid))
	for i, v := range grid {
		if phase := i % lag; counts[phase] > 0 {
			v -= sums[phase] / counts[phase]
		}
		out[i] = v
	}
	return out
}

// energy is the sum of squares, which is the scale a residual is judged against.
func energy(values []float64) float64 {
	var total float64
	for _, v := range values {
		total += v * v
	}
	return total
}

// bucketSecondsOf parses a groupTime the profiler itself produced, so the
// detectors can size themselves against it.
func bucketSecondsOf(groupTime string) float64 {
	duration, err := parseGroupTime(groupTime)
	if err != nil {
		return 0
	}
	return duration.Seconds()
}

// finestBucketSeconds is the finest aggregation bucket an analysis window
// admits: never below one second, and never so fine that the window divides into
// more grid points than the detectors will build.
func finestBucketSeconds(analysis Window) float64 {
	finest := minGroupTimeSeconds
	if analysis.Valid() {
		if implied := math.Ceil(analysis.Duration().Seconds() / maxAggregatedBuckets); implied > finest {
			finest = implied
		}
	}
	return finest
}

// validateGroupTime refuses a bucket the detectors would size themselves against
// and fall over.
//
// group_time is a free-form string in the schema published to the model
// (pkg/tools/surface.go), and the aggregated detectors allocate one grid slot per
// bucket of the analysis window before they look at any data. "1ms" over a year
// is 31.5 billion slots and "1ns" is beyond what a slice length holds, so what a
// model can author here is not a slow profile but a dead process — nothing in
// pkg/ recovers from a panic, and the chat exchange goroutine least of all.
//
// The refusal names the finest bucket that would have worked. This string comes
// from a model, and something it can act on is the difference between correcting
// itself and giving up.
func validateGroupTime(groupTime string, analysis Window) error {
	if strings.TrimSpace(groupTime) == "" {
		return nil
	}
	seconds := bucketSecondsOf(groupTime)
	if seconds <= 0 {
		// Unparsable here, which the server may still accept: the aggregated
		// detectors then report not_computed for want of a bucket width and the
		// rest of the profile stands. That is the behaviour this check found and
		// is deliberately not the behaviour it changes.
		return nil
	}

	finest := finestBucketSeconds(analysis)
	if seconds >= finest {
		return nil
	}
	return fmt.Errorf("%w: group_time %q is a bucket of %gs. It must be at least %gs, and coarse enough "+
		"that the analysis window %s divides into at most %d buckets — here that means %s or wider. "+
		"Leaving group_time empty derives one from the detected sampling interval, which is the better answer "+
		"unless you mean to override it",
		ErrInvalidRequest, groupTime, seconds, minGroupTimeSeconds, analysis.String(),
		maxAggregatedBuckets, formatGroupTime(finest))
}

// parseGroupTime understands the subset of interval syntax ODE emits. It is not
// a general parser for the server's grammar: it only has to read back what
// chooseGroupTime wrote.
func parseGroupTime(groupTime string) (time.Duration, error) {
	return time.ParseDuration(groupTime)
}
