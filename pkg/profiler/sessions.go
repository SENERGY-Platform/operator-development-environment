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
)

const (
	// otsuBins is the histogram resolution for the idle/active split. 256 is
	// enough to place a threshold and coarse enough that a few thousand points
	// still fill it.
	otsuBins = 256
	// weakSeparability is where Otsu's between-class variance stops supporting
	// a two-population reading. Below it the series is one population and
	// calling part of it "active" would be arbitrary.
	weakSeparability = 0.5
	// kdeGrid is the resolution of the density estimate used to look for two
	// modes and the valley between them.
	kdeGrid = 512
	// minSessionPoints is the fewest raw points a session can be built from.
	minSessionPoints = 2
	// maxSessions bounds the stored session list.
	maxSessions = 100000
	// dutyCycleContinuous: a series active more than this share of the time is
	// continuous with variation rather than a sequence of sessions.
	dutyCycleContinuous = 0.5
	// exemplarCount is how many sessions the profile carries inline (D26); the
	// rest live behind the paginated resource.
	exemplarCount     = 5
	minActivityPoints = 20
)

// DefaultSessionParams sizes the session detector against the series' own
// sampling interval. Every parameter is developer-adjustable (§5.4.13 item 7),
// which is why they travel in the profile rather than staying in the code.
func DefaultSessionParams(intervalSeconds float64) SessionParams {
	minDuration := 2 * intervalSeconds
	if minDuration < 60 {
		minDuration = 60
	}
	mergeGap := 3 * intervalSeconds
	if mergeGap < 300 {
		mergeGap = 300
	}
	return SessionParams{
		MinDurationS:   roundTo(minDuration, 1),
		MergeGapS:      roundTo(mergeGap, 1),
		HysteresisFrac: 0.1,
	}
}

// activityInput is what detector 7 needs about a series.
type activityInput struct {
	Times      []time.Time
	Values     []float64
	Interval   float64
	Kind       ValueKind
	Regularity Regularity
	Params     SessionParams
	ProfileID  string
}

// detectActivity is detector 7 of §5.4.13: the idle/active split, hysteresis, a
// minimum duration and sub-threshold gap merging.
//
// Raw or fine buckets only. A coarse bucket averages a short session into the
// idle level and it disappears — a 40-minute dishwasher cycle inside a one-hour
// mean is a slightly warm hour.
//
// A cumulative counter is differenced first: activity in a meter reading is in
// its rate of change, and thresholding the reading itself would classify the
// whole series as active from the moment the counter passes the threshold once.
func detectActivity(input activityInput) (Value[ActivityPattern], []Session) {
	switch input.Kind {
	case KindCategorical, KindStatus:
		return Computed(ActivityPattern{
			Classification:           ActivityStatus,
			ClassificationConfidence: Likely,
			ThresholdMethod:          "none",
			ThresholdParams:          input.Params,
			SessionStats: Uncomputablef[SessionStats](ReasonWrongKind,
				"a %s series has states, not sessions", input.Kind),
			SessionExemplars: []SessionExemplar{},
			SessionsRef:      SessionsPath(input.ProfileID),
		}), nil
	}

	times, values := input.Times, input.Values
	if input.Kind == KindCumulativeCounter {
		times, values = differenceRate(times, values)
	}
	if len(values) < minActivityPoints {
		return Uncomputablef[ActivityPattern](ReasonInsufficientCoverage,
			"%d usable points, need at least %d to separate idle from active", len(values), minActivityPoints), nil
	}

	threshold, method, separability, ok := activeThreshold(values)
	if !ok {
		return Uncomputable[ActivityPattern](ReasonInsufficientCoverage,
			"the values do not vary, so there is no idle/active split to find"), nil
	}

	idle := idleLevel(values, threshold)
	pattern := ActivityPattern{
		IdleLevel:       roundTo(idle, 4),
		ActiveThreshold: roundTo(threshold, 4),
		ThresholdMethod: method,
		ThresholdParams: input.Params,
		SessionsRef:     SessionsPath(input.ProfileID),
	}

	// A weakly separable histogram means one population. Reporting sessions
	// from it would invent boundaries a developer would then be asked to
	// confirm, which is worse than saying the series is continuous.
	if separability < weakSeparability {
		pattern.Classification = ActivityContinuous
		pattern.ClassificationConfidence = Likely
		pattern.SessionStats = Uncomputablef[SessionStats](ReasonWrongKind,
			"the value histogram is not bimodal (separability %.2f < %.2f), so there are no sessions to count",
			separability, weakSeparability)
		pattern.SessionExemplars = []SessionExemplar{}
		return Computed(pattern), nil
	}

	sessions := buildSessions(times, values, threshold, input.Params)
	activeSeconds := 0.0
	for _, session := range sessions {
		activeSeconds += session.DurationS
	}
	totalSeconds := times[len(times)-1].Sub(times[0]).Seconds()
	dutyCycle := 0.0
	if totalSeconds > 0 {
		dutyCycle = activeSeconds / totalSeconds
	}

	switch {
	case len(sessions) == 0:
		pattern.Classification = ActivityContinuous
	case input.Regularity == Irregular:
		// The series reports sporadically rather than running and resting, which
		// is a different shape from a session and wants a different model.
		pattern.Classification = ActivityIntermittent
	case dutyCycle >= dutyCycleContinuous:
		pattern.Classification = ActivityContinuous
	default:
		pattern.Classification = ActivitySessionBased
	}
	pattern.ClassificationConfidence = Likely
	if separability < weakSeparability*1.4 || len(sessions) < 3 {
		pattern.ClassificationConfidence = Uncertain
	}

	pattern.SessionStats = sessionStats(sessions)
	pattern.SessionExemplars = exemplars(sessions)
	return Computed(pattern), sessions
}

// differenceRate converts a cumulative counter into the rate of change between
// readings, which is the quantity a threshold can be applied to. Counter resets
// are dropped rather than differenced, since the negative step is an artefact.
func differenceRate(times []time.Time, values []float64) ([]time.Time, []float64) {
	outTimes := make([]time.Time, 0, len(times))
	outValues := make([]float64, 0, len(values))
	for i := 1; i < len(values); i++ {
		seconds := times[i].Sub(times[i-1]).Seconds()
		if seconds <= 0 {
			continue
		}
		delta := values[i] - values[i-1]
		if delta < 0 {
			continue
		}
		outTimes = append(outTimes, times[i])
		outValues = append(outValues, delta/seconds)
	}
	return outTimes, outValues
}

// activeThreshold finds the idle/active split, preferring the valley between two
// density modes and falling back to Otsu.
//
// Both methods are implemented because they fail differently. Otsu always
// returns a threshold, including for a single-population series, and its
// separability is what detects that case. The KDE valley only exists when the
// density really has two modes, so where it finds one it is the better boundary —
// Otsu places the split to balance variance, which on a skewed load profile sits
// above the true idle band.
func activeThreshold(values []float64) (threshold float64, method string, separability float64, ok bool) {
	clean := finite(values)
	if len(clean) < minActivityPoints {
		return 0, "", 0, false
	}
	otsu, separability, ok := otsuThreshold(clean)
	if !ok {
		return 0, "", 0, false
	}
	if valley, found := kdeValley(clean); found {
		return valley, "kde_valley", separability, true
	}
	return otsu, "otsu", separability, true
}

// otsuThreshold maximises the between-class variance of a two-class split of the
// histogram, and reports the ratio of that variance to the total as the
// separability.
func otsuThreshold(values []float64) (threshold float64, separability float64, ok bool) {
	low, high := minOf(values, 0), maxOf(values, 0)
	if !(high > low) {
		return 0, 0, false
	}

	width := (high - low) / otsuBins
	counts := make([]float64, otsuBins)
	for _, v := range values {
		bin := int((v - low) / width)
		if bin < 0 {
			bin = 0
		}
		if bin >= otsuBins {
			bin = otsuBins - 1
		}
		counts[bin]++
	}

	total := float64(len(values))
	binCentre := func(bin int) float64 { return low + (float64(bin)+0.5)*width }

	var totalMean float64
	for bin, count := range counts {
		totalMean += binCentre(bin) * count / total
	}

	var weightLow, meanLow, bestBetween float64
	bestBin := -1
	for bin, count := range counts {
		weightLow += count / total
		if weightLow <= 0 || weightLow >= 1 {
			meanLow += binCentre(bin) * count / total
			continue
		}
		meanLow += binCentre(bin) * count / total
		lowMean := meanLow / weightLow
		highMean := (totalMean - meanLow) / (1 - weightLow)
		between := weightLow * (1 - weightLow) * (lowMean - highMean) * (lowMean - highMean)
		if between > bestBetween {
			bestBetween, bestBin = between, bin
		}
	}
	if bestBin < 0 {
		return 0, 0, false
	}

	variance := stddev(values)
	variance *= variance
	if variance <= 0 {
		return 0, 0, false
	}
	return binCentre(bestBin), bestBetween / variance, true
}

// kdeValley estimates the density with a Gaussian kernel on a fixed grid and
// returns the minimum between the two highest modes.
//
// Bandwidth is Silverman's rule with the robust scale estimate, so a long active
// tail does not oversmooth the idle peak away.
func kdeValley(values []float64) (float64, bool) {
	low, high := minOf(values, 0), maxOf(values, 0)
	if !(high > low) {
		return 0, false
	}

	sorted := sortedCopy(values)
	iqr := percentile(sorted, 0.75) - percentile(sorted, 0.25)
	scale := stddev(values)
	if iqr > 0 && iqr/1.34 < scale {
		scale = iqr / 1.34
	}
	if scale <= 0 {
		return 0, false
	}
	bandwidth := 0.9 * scale * math.Pow(float64(len(values)), -0.2)
	if bandwidth <= 0 {
		return 0, false
	}

	step := (high - low) / float64(kdeGrid-1)

	// Binned rather than direct evaluation: the data goes into a histogram on the
	// same grid the density is reported on, and the kernel is convolved with the
	// histogram instead of with every point.
	//
	// The direct form is grid × points, which at the raw point limit is fifty
	// million exponentials per variable and turns a profile into a wait. This is
	// grid × grid regardless of how many points arrived, and the quantisation it
	// introduces is one grid step — the same resolution the threshold is reported
	// at, and far below the bandwidth that smooths it.
	histogram := make([]float64, kdeGrid)
	for _, v := range values {
		bin := int(math.Round((v - low) / step))
		if bin < 0 {
			bin = 0
		}
		if bin >= kdeGrid {
			bin = kdeGrid - 1
		}
		histogram[bin]++
	}

	// The kernel is symmetric, so it is evaluated once per offset and reused.
	kernel := make([]float64, kdeGrid)
	for offset := range kernel {
		z := float64(offset) * step / bandwidth
		kernel[offset] = math.Exp(-0.5 * z * z)
	}

	density := make([]float64, kdeGrid)
	for i := range density {
		var sum float64
		for bin, count := range histogram {
			if count == 0 {
				continue
			}
			offset := i - bin
			if offset < 0 {
				offset = -offset
			}
			sum += count * kernel[offset]
		}
		density[i] = sum
	}

	// The boundaries count as peaks. A load profile idles at its own minimum, so
	// the idle mode sits exactly on the left edge of the grid: excluding it leaves
	// only maxima inside the active band, and the "valley" between two of those is
	// a threshold in the middle of the working load.
	peaks := []int{}
	if density[0] > density[1] {
		peaks = append(peaks, 0)
	}
	for i := 1; i < kdeGrid-1; i++ {
		if density[i] > density[i-1] && density[i] >= density[i+1] {
			peaks = append(peaks, i)
		}
	}
	if density[kdeGrid-1] > density[kdeGrid-2] {
		peaks = append(peaks, kdeGrid-1)
	}
	if len(peaks) < 2 {
		return 0, false
	}
	sort.SliceStable(peaks, func(a, b int) bool { return density[peaks[a]] > density[peaks[b]] })

	first, second := peaks[0], peaks[1]
	if first > second {
		first, second = second, first
	}
	// A second mode carrying almost no density is a ripple, not a population.
	if density[second] < 0.05*density[first] && density[first] < 0.05*density[second] {
		return 0, false
	}

	valley, valleyIndex := math.Inf(1), -1
	for i := first; i <= second; i++ {
		if density[i] < valley {
			valley, valleyIndex = density[i], i
		}
	}
	if valleyIndex < 0 {
		return 0, false
	}
	return low + float64(valleyIndex)*step, true
}

func idleLevel(values []float64, threshold float64) float64 {
	below := make([]float64, 0, len(values))
	for _, v := range values {
		if v <= threshold {
			below = append(below, v)
		}
	}
	if len(below) == 0 {
		return minOf(values, 0)
	}
	return median(below)
}

// buildSessions walks the series with hysteresis, merges sessions separated by
// less than MergeGapS and drops those shorter than MinDurationS.
//
// Hysteresis is what stops a value sitting on the threshold from producing a
// session per sample: entry needs the value a band above the threshold, exit
// needs it a band below.
//
// The band is taken from the magnitude of the threshold, not from the signed
// value. Scaling by (1±frac) puts entry *below* exit whenever the threshold is
// negative — which a bidirectional meter reaches easily, since a battery that
// idles near zero and charges at −2 kW splits at about −1 kW — and a series
// oscillating inside the band then enters and leaves on the same sample. That is
// the chatter the hysteresis exists to prevent, arrived at by the hysteresis
// itself. A zero threshold gives a zero band, which is the strict crossing a
// series idling at exactly zero needs.
func buildSessions(times []time.Time, values []float64, threshold float64, params SessionParams) []Session {
	band := math.Abs(threshold) * params.HysteresisFrac
	enter := threshold + band
	exit := threshold - band

	raw := []Session{}
	active := false
	start := 0

	closeAt := func(end int) {
		if end < start {
			return
		}
		if end-start+1 < minSessionPoints {
			return
		}
		raw = append(raw, buildSession(times[start:end+1], values[start:end+1]))
	}

	for i, v := range values {
		switch {
		case !active && v > enter:
			active = true
			start = i
		case active && v < exit:
			closeAt(i - 1)
			active = false
		}
		if len(raw) >= maxSessions {
			return raw
		}
	}
	if active {
		closeAt(len(values) - 1)
	}

	merged := mergeSessions(raw, params.MergeGapS)
	out := make([]Session, 0, len(merged))
	for _, session := range merged {
		if session.DurationS < params.MinDurationS {
			continue
		}
		out = append(out, session)
	}
	return out
}

func buildSession(times []time.Time, values []float64) Session {
	session := Session{
		From:   times[0],
		To:     times[len(times)-1],
		Points: len(values),
		Peak:   maxOf(values, 0),
	}
	session.DurationS = roundTo(session.To.Sub(session.From).Seconds(), 3)
	// Trapezoidal integration, so energy is the value integrated over time: for
	// a power series in W over seconds that is joules, and for anything else it
	// is the series' own unit multiplied by seconds.
	var energy float64
	for i := 1; i < len(values); i++ {
		seconds := times[i].Sub(times[i-1]).Seconds()
		energy += 0.5 * (values[i] + values[i-1]) * seconds
	}
	session.Energy = roundTo(energy, 4)
	session.Peak = roundTo(session.Peak, 4)
	return session
}

// mergeSessions joins neighbours separated by less than mergeGap. A machine that
// dips below the threshold mid-cycle is one session, not two, and a spin cycle
// pause is exactly that dip.
func mergeSessions(sessions []Session, mergeGap float64) []Session {
	if len(sessions) < 2 || mergeGap <= 0 {
		return sessions
	}
	out := []Session{sessions[0]}
	for _, session := range sessions[1:] {
		last := &out[len(out)-1]
		if session.From.Sub(last.To).Seconds() <= mergeGap {
			last.To = session.To
			last.DurationS = roundTo(last.To.Sub(last.From).Seconds(), 3)
			last.Energy = roundTo(last.Energy+session.Energy, 4)
			last.Points += session.Points
			if session.Peak > last.Peak {
				last.Peak = session.Peak
			}
			continue
		}
		out = append(out, session)
	}
	return out
}

func sessionStats(sessions []Session) Value[SessionStats] {
	if len(sessions) == 0 {
		// The detector ran and found nothing, which is a count of zero rather
		// than an unknown (D24).
		return Computed(SessionStats{})
	}

	durations := make([]float64, 0, len(sessions))
	energies := make([]float64, 0, len(sessions))
	arrivals := make([]float64, 0, len(sessions))
	for i, session := range sessions {
		durations = append(durations, session.DurationS)
		energies = append(energies, session.Energy)
		if i > 0 {
			arrivals = append(arrivals, session.From.Sub(sessions[i-1].From).Seconds())
		}
	}

	stats := SessionStats{
		Count:           len(sessions),
		MedianDurationS: roundTo(median(durations), 1),
		MedianEnergy:    roundTo(median(energies), 4),
	}
	if len(arrivals) > 0 {
		stats.InterArrivalMedianS = roundTo(median(arrivals), 1)
	}
	return Computed(stats)
}

// exemplars picks a spread rather than the first few, so the LLM sees a short
// session, a typical one and a long one instead of whatever happened first.
func exemplars(sessions []Session) []SessionExemplar {
	out := []SessionExemplar{}
	if len(sessions) == 0 {
		return out
	}

	byDuration := append([]Session{}, sessions...)
	sort.SliceStable(byDuration, func(i, j int) bool { return byDuration[i].DurationS < byDuration[j].DurationS })

	count := exemplarCount
	if count > len(byDuration) {
		count = len(byDuration)
	}
	for i := 0; i < count; i++ {
		index := 0
		if count > 1 {
			index = i * (len(byDuration) - 1) / (count - 1)
		}
		session := byDuration[index]
		out = append(out, SessionExemplar{
			From:      session.From,
			To:        session.To,
			DurationS: session.DurationS,
			Energy:    session.Energy,
			Peak:      session.Peak,
		})
	}
	return out
}
