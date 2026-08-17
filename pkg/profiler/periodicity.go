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
	if !window.Valid() || bucketSeconds <= 0 {
		return nil
	}
	buckets := int(math.Floor(window.Duration().Seconds() / bucketSeconds))
	if buckets <= 0 {
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
		out = append(out, PeriodEvidence{
			PeriodS:  roundTo(period, 1),
			Method:   "fft",
			Strength: round2(share),
			Label:    labelPeriod(period),
		})
		if len(out) >= maxReportedPeriods {
			break
		}
	}
	return out
}

// namedCycles tests the daily and weekly lags directly, because §5.4.13 asks for
// both to be reported explicitly and because a cycle that matters to the
// developer should not depend on winning a peak-picking contest against its own
// harmonics.
func namedCycles(grid []float64, bucketSeconds float64) []PeriodEvidence {
	out := []PeriodEvidence{}
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
		acf := autocorrelation(grid, lag)
		if len(acf) <= lag || acf[lag] < acfPeakThreshold {
			continue
		}
		out = append(out, PeriodEvidence{
			PeriodS:  cycle.seconds,
			Method:   "acf",
			Strength: round2(acf[lag]),
			Label:    cycle.label,
		})
	}
	return out
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

// parseGroupTime understands the subset of interval syntax ODE emits. It is not
// a general parser for the server's grammar: it only has to read back what
// chooseGroupTime wrote.
func parseGroupTime(groupTime string) (time.Duration, error) {
	return time.ParseDuration(groupTime)
}
