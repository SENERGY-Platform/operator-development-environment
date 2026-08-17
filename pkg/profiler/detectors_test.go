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
	"math/rand"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"
)

// --- detector 1: sampling ---

func TestSamplingFindsTheModalIntervalOfAQuarterHourlySeries(t *testing.T) {
	times, _ := regularSeries(fixtureStart, quarterHour, 500, func(int) float64 { return 1 })

	sampling, interval := detectSampling(times)
	detected := mustGet(t, sampling, "sampling")

	if interval != 900 {
		t.Errorf("interval = %v, want 900", interval)
	}
	if detected.Regularity != Regular {
		t.Errorf("regularity = %s, want regular (irregularity %.2f)", detected.Regularity, detected.IrregularityRatio)
	}
	if len(detected.Gaps) != 0 {
		t.Errorf("gaps = %v, want none in an unbroken series", detected.Gaps)
	}
}

func TestSamplingReportsAnInjectedGapWithItsDuration(t *testing.T) {
	times, values := regularSeries(fixtureStart, quarterHour, 500, func(int) float64 { return 1 })
	// Drop six hours: indices 100 to 124 at a quarter hour each.
	times, _ = dropRange(times, values, 100, 124)

	sampling, _ := detectSampling(times)
	detected := mustGet(t, sampling, "sampling")

	if len(detected.Gaps) != 1 {
		t.Fatalf("gaps = %d, want exactly the injected one: %+v", len(detected.Gaps), detected.Gaps)
	}
	gap := detected.Gaps[0]
	if gap.DurationS != 25*900 {
		t.Errorf("gap duration = %vs, want %vs (25 missing quarter hours)", gap.DurationS, 25*900)
	}
	if !gap.From.Equal(fixtureStart.Add(99 * quarterHour)) {
		t.Errorf("gap starts at %s, want the last point before the hole", gap.From)
	}
	// Unclassified until classifyGaps has context: absence and negation must stay
	// distinguishable, and "unknown" is not "fine".
	if gap.Classification != GapUnknown {
		t.Errorf("classification = %s, want unknown before classification", gap.Classification)
	}
}

func TestSamplingNeedsThreePointsBeforeItReportsAnything(t *testing.T) {
	times, _ := regularSeries(fixtureStart, quarterHour, 2, func(int) float64 { return 1 })

	sampling, interval := detectSampling(times)
	if sampling.IsComputed() {
		t.Fatal("two points produced an interval; want not_computed")
	}
	if status := sampling.Status(); status.Reason != ReasonInsufficientCoverage {
		t.Errorf("reason = %s, want insufficient_coverage", status.Reason)
	}
	if interval != 0 {
		t.Errorf("interval = %v, want 0", interval)
	}
}

func TestAnIrregularSeriesIsNotCalledRegular(t *testing.T) {
	times := []time.Time{}
	at := fixtureStart
	for i := 0; i < 200; i++ {
		// Deltas alternate between 1 and 10 minutes: no schedule at all.
		step := time.Minute
		if i%2 == 0 {
			step = 10 * time.Minute
		}
		at = at.Add(step)
		times = append(times, at)
	}

	sampling, _ := detectSampling(times)
	detected := mustGet(t, sampling, "sampling")
	if detected.Regularity == Regular {
		t.Errorf("regularity = regular, want irregular or mixed (irregularity %.2f)", detected.IrregularityRatio)
	}
}

// --- detector 4: gap classification ---

func TestAGapWithSiblingTrafficIsASensorFault(t *testing.T) {
	gap := Gap{
		From:      fixtureStart,
		To:        fixtureStart.Add(6 * time.Hour),
		DurationS: 6 * 3600,
	}
	siblings := []time.Time{fixtureStart.Add(2 * time.Hour), fixtureStart.Add(3 * time.Hour)}

	classified := classifyGaps([]Gap{gap}, siblings, models.ConnectionStateOnline, gap.To)
	if classified[0].Classification != GapSensorFault {
		t.Errorf("classification = %s, want sensor_fault: the device reported, this channel did not",
			classified[0].Classification)
	}
}

func TestATrailingGapOnAnOfflineDeviceIsTheDeviceBeingOffline(t *testing.T) {
	windowEnd := fixtureStart.Add(24 * time.Hour)
	gap := Gap{From: fixtureStart.Add(18 * time.Hour), To: windowEnd, DurationS: 6 * 3600}

	classified := classifyGaps([]Gap{gap}, nil, models.ConnectionStateOffline, windowEnd)
	if classified[0].Classification != GapDeviceOffline {
		t.Errorf("classification = %s, want device_offline", classified[0].Classification)
	}
}

func TestAnInteriorGapWithNoEvidenceStaysUnknown(t *testing.T) {
	windowEnd := fixtureStart.Add(24 * time.Hour)
	gap := Gap{From: fixtureStart.Add(2 * time.Hour), To: fixtureStart.Add(5 * time.Hour), DurationS: 3 * 3600}

	classified := classifyGaps([]Gap{gap}, nil, models.ConnectionStateOffline, windowEnd)
	if classified[0].Classification != GapUnknown {
		t.Errorf("classification = %s, want unknown: nothing here shows what caused it",
			classified[0].Classification)
	}
}

// --- detector 2: value semantics ---

// The acceptance criterion of M1b: counter versus instantaneous is never
// misclassified. Misreading a cumulative kWh counter as instantaneous power
// produces silent garbage.
func TestACumulativeCounterIsNotReadAsInstantaneous(t *testing.T) {
	times, values := counterWithResets(fixtureStart, quarterHour, 400, 12, nil)

	result := detectValueKind(column("value.total", times, values), models.Float)
	if kind := mustGet(t, result.Kind, "kind"); kind != KindCumulativeCounter {
		t.Fatalf("kind = %s, want cumulative_counter", kind)
	}
	evidence := mustGet(t, result.Evidence, "kind evidence")
	if evidence.MonotonicRatio < monotonicThreshold {
		t.Errorf("monotonic_ratio = %v, want at least %v as the evidence behind the verdict",
			evidence.MonotonicRatio, monotonicThreshold)
	}
}

func TestAnInstantaneousSeriesIsNotReadAsACounter(t *testing.T) {
	times, values := regularSeries(fixtureStart, quarterHour, 400, func(i int) float64 {
		return 500 + 400*math.Sin(float64(i)/8)
	})

	result := detectValueKind(column("value.power", times, values), models.Float)
	if kind := mustGet(t, result.Kind, "kind"); kind != KindInstantaneous {
		t.Fatalf("kind = %s, want instantaneous", kind)
	}
}

// A flat series has a monotonic ratio of 1.0 because every delta is zero. That
// is a frozen sensor, not a meter.
func TestAConstantSeriesIsNotACounter(t *testing.T) {
	times, values := regularSeries(fixtureStart, quarterHour, 400, func(int) float64 { return 42 })

	result := detectValueKind(column("value.power", times, values), models.Float)
	if kind := mustGet(t, result.Kind, "kind"); kind == KindCumulativeCounter {
		t.Error("a constant series was read as a cumulative counter")
	}
}

func TestTwoCounterResetsAreFoundAtTheirTimestamps(t *testing.T) {
	times, values := counterWithResets(fixtureStart, quarterHour, 400, 12, []int{150, 300})

	result := detectValueKind(column("value.total", times, values), models.Float)
	if kind := mustGet(t, result.Kind, "kind"); kind != KindCumulativeCounter {
		t.Fatalf("kind = %s, want cumulative_counter despite the resets", kind)
	}
	resets := mustGet(t, result.Resets, "counter resets")
	if len(resets) != 2 {
		t.Fatalf("resets = %v, want two", resets)
	}
	if !resets[0].Equal(times[150]) || !resets[1].Equal(times[300]) {
		t.Errorf("resets at %v, want %v and %v", resets, times[150], times[300])
	}
}

func TestCounterResetsAreNotComputedForANonCounter(t *testing.T) {
	times, values := regularSeries(fixtureStart, quarterHour, 400, func(i int) float64 {
		return 500 + 400*math.Sin(float64(i)/8)
	})

	result := detectValueKind(column("value.power", times, values), models.Float)
	if result.Resets.IsComputed() {
		t.Fatal("counter resets were computed for an instantaneous series")
	}
	if status := result.Resets.Status(); status.Reason != ReasonWrongKind {
		t.Errorf("reason = %s, want wrong_kind", status.Reason)
	}
}

func TestABooleanSeriesIsBinary(t *testing.T) {
	times, values := regularSeries(fixtureStart, quarterHour, 400, func(i int) float64 {
		return float64(i % 2)
	})

	result := detectValueKind(column("value.on", times, values), models.Boolean)
	if kind := mustGet(t, result.Kind, "kind"); kind != KindBinary {
		t.Errorf("kind = %s, want binary", kind)
	}
}

func TestAFewNamedStatesAreStatusRatherThanAMeasurement(t *testing.T) {
	times, _ := regularSeries(fixtureStart, quarterHour, 400, func(int) float64 { return 0 })
	states := make([]string, 0, len(times))
	for i := range times {
		states = append(states, []string{"idle", "running", "error"}[i%3])
	}

	result := detectValueKind(textColumn("value.state", times, states), models.String)
	if kind := mustGet(t, result.Kind, "kind"); kind != KindStatus {
		t.Errorf("kind = %s, want status", kind)
	}
}

// --- coverage and the not_computed contract ---

// M1b acceptance: a low-coverage series yields not_computed rather than a number.
func TestALowCoverageSeriesYieldsNotComputedRatherThanANumber(t *testing.T) {
	window := Window{From: fixtureStart, To: fixtureStart.Add(14 * 24 * time.Hour)}
	// A fortnight at a quarter hour is 1344 points; 400 is well under the
	// threshold.
	coverage := computeCoverage(400, 900, window)
	computed := mustGet(t, coverage, "coverage")
	if computed.CompletenessRatio >= minCoverageForStatistics {
		t.Fatalf("completeness_ratio = %v, fixture should be sparse", computed.CompletenessRatio)
	}

	series := aggregatedSeries{}
	for i := 0; i < 400; i++ {
		series.Times = append(series.Times, fixtureStart.Add(time.Duration(i)*quarterHour))
		series.Mean = append(series.Mean, float64(i))
	}

	distribution := detectDistribution(series, coverage)
	if distribution.IsComputed() {
		t.Fatal("a distribution was computed over a sparse series")
	}
	status := distribution.Status()
	if status.Reason != ReasonInsufficientCoverage {
		t.Errorf("reason = %s, want insufficient_coverage", status.Reason)
	}
	// The detail has to carry the numbers, so a reader can see how far short it
	// fell rather than only that it did.
	if status.Detail == "" {
		t.Error("detail is empty; it must carry the ratio and the threshold")
	}
}

func TestCoverageComparesPointsAgainstTheDetectedInterval(t *testing.T) {
	window := Window{From: fixtureStart, To: fixtureStart.Add(24 * time.Hour)}
	coverage := mustGet(t, computeCoverage(96, 900, window), "coverage")

	if coverage.ExpectedPoints != 96 {
		t.Errorf("expected_points = %d, want 96 quarter hours in a day", coverage.ExpectedPoints)
	}
	if coverage.CompletenessRatio != 1 {
		t.Errorf("completeness_ratio = %v, want 1", coverage.CompletenessRatio)
	}
}

// --- distribution and constant runs ---

func TestDistributionTakesMinAndMaxFromTheBucketExtremes(t *testing.T) {
	series := aggregatedSeries{}
	for i := 0; i < 100; i++ {
		series.Times = append(series.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		series.Mean = append(series.Mean, 100)
		series.Min = append(series.Min, 10)
		series.Max = append(series.Max, 900)
	}
	coverage := Computed(Coverage{NPoints: 100, ExpectedPoints: 100, CompletenessRatio: 1})

	distribution := mustGet(t, detectDistribution(series, coverage), "distribution")
	// The maximum of a series is not the maximum of its bucket means, which is
	// why the aggregated pass asks for three columns.
	if distribution.Min != 10 || distribution.Max != 900 {
		t.Errorf("min/max = %v/%v, want 10/900 from the min and max buckets", distribution.Min, distribution.Max)
	}
	if distribution.Mean != 100 {
		t.Errorf("mean = %v, want 100", distribution.Mean)
	}
}

func TestAFrozenStretchBecomesAConstantRun(t *testing.T) {
	times, values := regularSeries(fixtureStart, quarterHour, 200, func(i int) float64 {
		if i >= 50 && i < 150 {
			return 7
		}
		return float64(i)
	})

	runs := mustGet(t, detectConstantRuns(times, values), "constant runs")
	longest, found := longestRun(runs)
	if !found {
		t.Fatal("no constant run found in a fixture with a hundred identical points")
	}
	if longest.Points != 100 || longest.Value != 7 {
		t.Errorf("longest run = %d points at %v, want 100 at 7", longest.Points, longest.Value)
	}
}

// --- detector 6: periodicity ---

func TestADailyCycleIsFoundAndNamed(t *testing.T) {
	const bucket = 3600.0
	days := 40
	series := aggregatedSeries{}
	for i := 0; i < days*24; i++ {
		series.Times = append(series.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		series.Mean = append(series.Mean, 100+50*math.Sin(2*math.Pi*float64(i)/24))
	}
	window := Window{From: fixtureStart, To: fixtureStart.Add(time.Duration(days) * 24 * time.Hour)}
	coverage := Computed(Coverage{NPoints: days * 24, ExpectedPoints: days * 24, CompletenessRatio: 1})

	periods, evidence := detectPeriodicity(series, window, bucket, coverage)
	found := mustGet(t, periods, "dominant periods")

	daily := false
	for _, period := range found {
		if math.Abs(period-86400) < 3600 {
			daily = true
		}
	}
	if !daily {
		t.Errorf("periods = %v, want a daily cycle among them", found)
	}

	labelled := false
	for _, e := range mustGet(t, evidence, "period evidence") {
		if e.Label == "daily" {
			labelled = true
		}
	}
	if !labelled {
		t.Error("no evidence entry is labelled daily; §5.4.13 asks for it by name")
	}
}

// Absence and negation must be distinguishable (D24). A series with no cycle
// returns an empty list, not not_computed — the detector did run.
func TestAnAperiodicSeriesReturnsAnEmptyListNotNotComputed(t *testing.T) {
	const bucket = 3600.0
	random := rand.New(rand.NewSource(1))
	series := aggregatedSeries{}
	for i := 0; i < 24*40; i++ {
		series.Times = append(series.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		series.Mean = append(series.Mean, random.NormFloat64())
	}
	window := Window{From: fixtureStart, To: fixtureStart.Add(40 * 24 * time.Hour)}
	coverage := Computed(Coverage{NPoints: 24 * 40, ExpectedPoints: 24 * 40, CompletenessRatio: 1})

	periods, _ := detectPeriodicity(series, window, bucket, coverage)
	found, ok := periods.Get()
	if !ok {
		t.Fatalf("periods are not_computed (%s); white noise should produce an empty list",
			periods.Status().Detail)
	}
	if len(found) != 0 {
		t.Errorf("periods = %v, want none in white noise", found)
	}
}

func TestPeriodicityNeedsMoreThanOneCycleOfHistory(t *testing.T) {
	const bucket = 3600.0
	series := aggregatedSeries{}
	for i := 0; i < 10; i++ {
		series.Times = append(series.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		series.Mean = append(series.Mean, float64(i))
	}
	window := Window{From: fixtureStart, To: fixtureStart.Add(10 * time.Hour)}
	coverage := Computed(Coverage{NPoints: 10, ExpectedPoints: 10, CompletenessRatio: 1})

	periods, _ := detectPeriodicity(series, window, bucket, coverage)
	if periods.IsComputed() {
		t.Fatal("a period was reported from ten hours of data")
	}
	if status := periods.Status(); status.Reason != ReasonInsufficientSpan {
		t.Errorf("reason = %s, want insufficient_span", status.Reason)
	}
}

// --- trend and stationarity ---

func TestARisingSeriesHasASignificantPositiveTrend(t *testing.T) {
	times, values := regularSeries(fixtureStart, time.Hour, 500, func(i int) float64 {
		return 10 + 0.5*float64(i)
	})
	coverage := Computed(Coverage{NPoints: 500, ExpectedPoints: 500, CompletenessRatio: 1})

	trend := mustGet(t, detectTrend(times, values, coverage), "trend")
	if !trend.Significant {
		t.Errorf("trend is not significant (t = %v) on a clean ramp", trend.TStat)
	}
	// 0.5 per hour is 12 per day.
	if math.Abs(trend.SlopePerDay-12) > 0.1 {
		t.Errorf("slope_per_day = %v, want 12", trend.SlopePerDay)
	}
}

func TestWhiteNoiseIsStationaryAndARandomWalkIsNot(t *testing.T) {
	coverage := Computed(Coverage{NPoints: 3000, ExpectedPoints: 3000, CompletenessRatio: 1})
	random := rand.New(rand.NewSource(42))

	noise := make([]float64, 3000)
	walk := make([]float64, 3000)
	for i := range noise {
		noise[i] = random.NormFloat64()
		if i > 0 {
			walk[i] = walk[i-1] + random.NormFloat64()
		}
	}

	stationary := mustGet(t, detectStationarity(noise, coverage), "stationarity of white noise")
	if !stationary.Stationary {
		t.Errorf("white noise reported as non-stationary: adf %v against %v",
			stationary.ADFStat, stationary.CriticalValues["5pct"])
	}
	if stationary.PValueBracket.Upper > 0.05 {
		t.Errorf("p bracket = %+v, want an upper bound at or below 0.05", stationary.PValueBracket)
	}

	nonStationary := mustGet(t, detectStationarity(walk, coverage), "stationarity of a random walk")
	if nonStationary.Stationary {
		t.Errorf("a random walk reported as stationary: adf %v against %v",
			nonStationary.ADFStat, nonStationary.CriticalValues["5pct"])
	}
	if nonStationary.PValueBracket.Lower < 0.05 {
		t.Errorf("p bracket = %+v, want a lower bound at or above 0.05", nonStationary.PValueBracket)
	}
}

// The asymptotic critical values only hold at scale, so below the sample floor
// the test reports not_computed rather than a number compared against the wrong
// threshold (§5.4.14: do not fake it).
func TestStationarityIsNotComputedBelowTheSampleFloor(t *testing.T) {
	coverage := Computed(Coverage{NPoints: 200, ExpectedPoints: 200, CompletenessRatio: 1})
	values := make([]float64, 200)
	random := rand.New(rand.NewSource(7))
	for i := range values {
		values[i] = random.NormFloat64()
	}

	stationarity := detectStationarity(values, coverage)
	if stationarity.IsComputed() {
		t.Fatal("ADF reported a result on 200 observations")
	}
	if status := stationarity.Status(); status.Reason != ReasonInsufficientSpan {
		t.Errorf("reason = %s, want insufficient_span", status.Reason)
	}
}

// --- detector 7: sessions ---

func TestAWashingMachineLoadYieldsSessionsPerCycle(t *testing.T) {
	// Ten cycles: eight hours idle, then two hours running, at a five-minute
	// interval.
	times, values := washingMachineLoad(fixtureStart, 5*time.Minute, 10, 96, 24)

	activity, sessions := detectActivity(activityInput{
		Times: times, Values: values, Interval: 300,
		Kind: KindInstantaneous, Regularity: Regular,
		Params: DefaultSessionParams(300), ProfileID: "profile-1",
	})
	pattern := mustGet(t, activity, "activity pattern")

	if pattern.Classification != ActivitySessionBased {
		t.Errorf("classification = %s, want session_based", pattern.Classification)
	}
	if len(sessions) != 10 {
		t.Fatalf("sessions = %d, want one per cycle: %+v", len(sessions), sessions)
	}
	stats := mustGet(t, pattern.SessionStats, "session stats")
	if stats.Count != 10 {
		t.Errorf("session count = %d, want 10", stats.Count)
	}
	// A two-hour cycle at five minutes is 24 points, so the duration is about
	// 23 intervals.
	if stats.MedianDurationS < 60*60 || stats.MedianDurationS > 2.5*3600 {
		t.Errorf("median duration = %vs, want roughly two hours", stats.MedianDurationS)
	}
	if pattern.ActiveThreshold <= 10 || pattern.ActiveThreshold >= 1800 {
		t.Errorf("threshold = %v, want it between the idle draw and the working load", pattern.ActiveThreshold)
	}
}

// A single-population series has no idle/active split to find, and inventing one
// would produce boundaries a developer would then be asked to confirm.
func TestAContinuousSeriesReportsNoSessions(t *testing.T) {
	times, values := regularSeries(fixtureStart, 5*time.Minute, 2000, func(i int) float64 {
		return 500 + 20*math.Sin(float64(i)/20)
	})

	activity, sessions := detectActivity(activityInput{
		Times: times, Values: values, Interval: 300,
		Kind: KindInstantaneous, Regularity: Regular,
		Params: DefaultSessionParams(300), ProfileID: "profile-1",
	})
	pattern := mustGet(t, activity, "activity pattern")

	if pattern.Classification != ActivityContinuous {
		t.Errorf("classification = %s, want continuous", pattern.Classification)
	}
	if len(sessions) != 0 {
		t.Errorf("sessions = %d, want none", len(sessions))
	}
}

func TestAStatusSeriesHasStatesRatherThanSessions(t *testing.T) {
	times, values := regularSeries(fixtureStart, 5*time.Minute, 500, func(i int) float64 { return float64(i % 3) })

	activity, sessions := detectActivity(activityInput{
		Times: times, Values: values, Interval: 300,
		Kind: KindStatus, Regularity: Regular,
		Params: DefaultSessionParams(300), ProfileID: "profile-1",
	})
	pattern := mustGet(t, activity, "activity pattern")

	if pattern.Classification != ActivityStatus {
		t.Errorf("classification = %s, want status", pattern.Classification)
	}
	if pattern.SessionStats.IsComputed() {
		t.Error("session statistics were computed for a status series")
	}
	if status := pattern.SessionStats.Status(); status.Reason != ReasonWrongKind {
		t.Errorf("reason = %s, want wrong_kind", status.Reason)
	}
	if len(sessions) != 0 {
		t.Errorf("sessions = %d, want none", len(sessions))
	}
}

// A counter has to be differenced before a threshold means anything, or the
// whole series is "active" from the moment the reading passes the threshold once.
func TestSessionsOnACounterAreFoundInItsRateOfChange(t *testing.T) {
	times := []time.Time{}
	values := []float64{}
	at := fixtureStart
	total := 0.0
	for cycle := 0; cycle < 6; cycle++ {
		for i := 0; i < 96; i++ { // idle: the counter barely moves
			total += 0.01
			times, values = append(times, at), append(values, total)
			at = at.Add(5 * time.Minute)
		}
		for i := 0; i < 24; i++ { // running: it climbs fast
			total += 150
			times, values = append(times, at), append(values, total)
			at = at.Add(5 * time.Minute)
		}
	}

	activity, sessions := detectActivity(activityInput{
		Times: times, Values: values, Interval: 300,
		Kind: KindCumulativeCounter, Regularity: Regular,
		Params: DefaultSessionParams(300), ProfileID: "profile-1",
	})
	pattern := mustGet(t, activity, "activity pattern")

	if pattern.Classification != ActivitySessionBased {
		t.Errorf("classification = %s, want session_based from the differenced counter", pattern.Classification)
	}
	if len(sessions) != 6 {
		t.Errorf("sessions = %d, want one per cycle", len(sessions))
	}
}

// --- detector 8: relationships ---

// The check that motivates the service-scoped batch (§5.4.1): differencing an
// energy counter and comparing it with integrated power.
func TestACounterIsRecognisedAsTheIntegralOfItsPowerSibling(t *testing.T) {
	interval := 5 * time.Minute
	times := []time.Time{}
	power := []float64{}
	energy := []float64{}
	total := 0.0
	at := fixtureStart
	for i := 0; i < 300; i++ {
		watts := 400 + 300*math.Sin(float64(i)/10)
		times = append(times, at)
		power = append(power, watts)
		energy = append(energy, total)
		// Watt-seconds, so the implied scale between the two is 1.
		total += watts * interval.Seconds()
		at = at.Add(interval)
	}

	counter := variableSeries{
		Variable: Variable{Path: "value.total"}, Times: times, Values: energy, Kind: KindCumulativeCounter,
	}
	rate := variableSeries{
		Variable: Variable{Path: "value.power"}, Times: times, Values: power, Kind: KindInstantaneous,
	}

	relationships := detectRelationships(counter, []variableSeries{rate})
	if len(relationships) != 1 {
		t.Fatalf("relationships = %+v, want one", relationships)
	}
	if relationships[0].Type != RelationIntegralOf {
		t.Errorf("type = %s, want integral_of", relationships[0].Type)
	}
	if relationships[0].OtherPath != "value.power" {
		t.Errorf("other_path = %s, want value.power", relationships[0].OtherPath)
	}
	if relationships[0].Evidence.Correlation < strongShapeMatch {
		t.Errorf("correlation = %v, want at least %v", relationships[0].Evidence.Correlation, strongShapeMatch)
	}
}

// A unit mismatch must not read as inconsistency: watt-hours against watts
// differ by 3600, and the shape still matches. The factor is reported instead.
func TestAUnitFactorBetweenACounterAndItsRateIsReportedNotRejected(t *testing.T) {
	interval := time.Hour
	times := []time.Time{}
	power := []float64{}
	energy := []float64{}
	total := 0.0
	at := fixtureStart
	for i := 0; i < 300; i++ {
		watts := 400 + 300*math.Sin(float64(i)/10)
		times = append(times, at)
		power = append(power, watts)
		energy = append(energy, total)
		total += watts // watt-hours at an hourly interval
		at = at.Add(interval)
	}

	relationships := detectRelationships(
		variableSeries{Variable: Variable{Path: "value.total"}, Times: times, Values: energy, Kind: KindCumulativeCounter},
		[]variableSeries{{Variable: Variable{Path: "value.power"}, Times: times, Values: power, Kind: KindInstantaneous}},
	)
	if len(relationships) != 1 || relationships[0].Type != RelationIntegralOf {
		t.Fatalf("relationships = %+v, want one integral_of", relationships)
	}
	// The rate is integrated in seconds, so the implied scale is 1/3600.
	scale := relationships[0].Evidence.ImpliedScale
	if scale <= 0 || math.Abs(1/scale-3600) > 100 {
		t.Errorf("implied_scale = %v, want about 1/3600 so a unit error is legible", scale)
	}
}

func TestADeadChannelBesideALiveCounterIsInconsistent(t *testing.T) {
	interval := 5 * time.Minute
	times := []time.Time{}
	power := []float64{}
	energy := []float64{}
	total := 0.0
	at := fixtureStart
	for i := 0; i < 300; i++ {
		times = append(times, at)
		// The power channel is dead while the counter keeps climbing.
		power = append(power, 0)
		energy = append(energy, total)
		total += 500 * interval.Seconds()
		at = at.Add(interval)
	}

	relationships := detectRelationships(
		variableSeries{Variable: Variable{Path: "value.total"}, Times: times, Values: energy, Kind: KindCumulativeCounter},
		[]variableSeries{{Variable: Variable{Path: "value.power"}, Times: times, Values: power, Kind: KindInstantaneous}},
	)
	if len(relationships) != 1 || relationships[0].Type != RelationInconsistentWith {
		t.Fatalf("relationships = %+v, want one inconsistent_with", relationships)
	}
}

func TestTwoCopiesOfOneMeasurementAreRedundant(t *testing.T) {
	times, values := regularSeries(fixtureStart, 5*time.Minute, 300, func(i int) float64 {
		return 100 + 50*math.Sin(float64(i)/7)
	})

	relationships := detectRelationships(
		variableSeries{Variable: Variable{Path: "value.a"}, Times: times, Values: values, Kind: KindInstantaneous},
		[]variableSeries{{Variable: Variable{Path: "value.b"}, Times: times, Values: values, Kind: KindInstantaneous}},
	)
	if len(relationships) != 1 || relationships[0].Type != RelationRedundantWith {
		t.Fatalf("relationships = %+v, want one redundant_with", relationships)
	}
}

// --- detector 9: quality ---

func TestALongUnchangingStretchIsFlaggedAsAFrozenSensor(t *testing.T) {
	times, values := regularSeries(fixtureStart, quarterHour, 500, func(i int) float64 {
		if i >= 100 {
			return 7
		}
		return float64(i)
	})
	runs := detectConstantRuns(times, values)

	flags := detectQualityFlags(qualityInput{
		Variable:     Variable{Path: "value.power", Interaction: models.EVENT},
		ConstantRuns: runs,
		Interval:     900,
		Window:       Window{From: fixtureStart, To: times[len(times)-1]},
		LocalZone:    time.UTC,
	})

	if !hasFlag(flags, FlagFrozenSensor) {
		t.Errorf("flags = %+v, want a frozen_sensor flag", flags)
	}
	// D23: certain is for ontology-derived and developer-confirmed values, so a
	// conclusion drawn from a run length is likely, with the run length as
	// evidence.
	for _, flag := range flags {
		if flag.Flag == FlagFrozenSensor {
			if flag.Confidence != Likely {
				t.Errorf("confidence = %s, want likely", flag.Confidence)
			}
			if flag.Evidence["longest_constant_run_s"] == nil {
				t.Error("the flag carries no run length as evidence")
			}
		}
	}
}

func TestAValueOutsideTheDeclaredRangeIsFlaggedWithOntologyBasis(t *testing.T) {
	semantics := ValueSemantics{
		DeclaredRange:       DeclaredRange{Min: Computed(0.0), Max: Computed(100.0)},
		RangeViolationRatio: Computed(0.2),
		CharacteristicID:    stringValue("ch-watt"),
	}

	flags := detectQualityFlags(qualityInput{
		Variable:  Variable{Path: "value.power", Interaction: models.EVENT},
		Semantics: semantics,
		Window:    Window{From: fixtureStart, To: fixtureStart.Add(24 * time.Hour)},
		LocalZone: time.UTC,
	})

	found := false
	for _, flag := range flags {
		if flag.Flag != FlagRangeViolation {
			continue
		}
		found = true
		// The bound comes from the ontology and the comparison is exact, which is
		// D23's own carve-out for certain.
		if flag.Confidence != Certain {
			t.Errorf("confidence = %s, want certain: the bound is declared, not guessed", flag.Confidence)
		}
	}
	if !found {
		t.Errorf("flags = %+v, want a range_violation flag", flags)
	}
}

func TestARequestOnlyServiceIsFlaggedAsNotStreamed(t *testing.T) {
	flags := detectQualityFlags(qualityInput{
		Variable:  Variable{Path: "value.power", Interaction: models.REQUEST},
		Window:    Window{From: fixtureStart, To: fixtureStart.Add(24 * time.Hour)},
		LocalZone: time.UTC,
	})
	if !hasFlag(flags, FlagNotStreamed) {
		t.Errorf("flags = %+v, want not_streamed for a request-only service", flags)
	}
}

// Silent DST bugs in 15-minute meter data are a recurring failure mode, so a
// window spanning a transition says so.
func TestAWindowSpanningADstTransitionIsFlagged(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("Europe/Berlin did not load, although the package embeds the zone database: %v", err)
	}
	window := Window{
		From: time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC),
	}

	flags := detectQualityFlags(qualityInput{
		Variable:  Variable{Path: "value.power", Interaction: models.EVENT},
		Window:    window,
		LocalZone: berlin,
	})
	if !hasFlag(flags, FlagDSTAmbiguity) {
		t.Errorf("flags = %+v, want dst_ambiguity across the March transition", flags)
	}
}

func TestNoDstFlagForAWindowInsideOneOffset(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("Europe/Berlin did not load, although the package embeds the zone database: %v", err)
	}
	window := Window{
		From: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
	}

	flags := detectQualityFlags(qualityInput{
		Variable:  Variable{Path: "value.power", Interaction: models.EVENT},
		Window:    window,
		LocalZone: berlin,
	})
	if hasFlag(flags, FlagDSTAmbiguity) {
		t.Errorf("flags = %+v, want no dst_ambiguity in June", flags)
	}
}

func hasFlag(flags []QualityFlag, name string) bool {
	for _, flag := range flags {
		if flag.Flag == name {
			return true
		}
	}
	return false
}

func stringValue(s string) *string { return &s }

// --- regressions found by running the profiler against a live backend ---

// A noiseless series gave "stationary, ADF -5e14": with no residual variance the
// t statistic is rounding error divided by rounding error. It is not a smaller
// number than it should be, it is not a number at all.
func TestADeterministicSeriesReportsNoStationarityRatherThanAnAbsurdStatistic(t *testing.T) {
	coverage := Computed(Coverage{NPoints: 3000, ExpectedPoints: 3000, CompletenessRatio: 1})
	// A pure sine, exactly as a synthetic fixture or a perfectly repeating sensor
	// would produce.
	values := make([]float64, 3000)
	for i := range values {
		values[i] = 900 + 800*math.Sin(2*math.Pi*float64(i)/24)
	}

	stationarity := detectStationarity(values, coverage)
	if computed, ok := stationarity.Get(); ok {
		t.Fatalf("a statistic was reported for a deterministic series: adf %v, stationary %v",
			computed.ADFStat, computed.Stationary)
	}
	if status := stationarity.Status(); status.Reason != ReasonOutOfScope {
		t.Errorf("reason = %s, want out_of_scope", status.Reason)
	}
}

// Noise on top of the same signal is a real series again, and the test runs.
func TestASeriesWithRealNoiseStillGetsAStationarityVerdict(t *testing.T) {
	coverage := Computed(Coverage{NPoints: 3000, ExpectedPoints: 3000, CompletenessRatio: 1})
	random := rand.New(rand.NewSource(11))
	values := make([]float64, 3000)
	for i := range values {
		values[i] = 900 + 800*math.Sin(2*math.Pi*float64(i)/24) + 40*random.NormFloat64()
	}

	if _, ok := detectStationarity(values, coverage).Get(); !ok {
		t.Errorf("no verdict for a noisy series: %s", detectStationarity(values, coverage).Status().Detail)
	}
}

// The autocorrelation of a daily cycle peaks again at two and three days.
// Reporting those invites a reader to model a three-day cycle that is not there.
func TestHarmonicsOfTheDailyCycleAreNotReportedAsSeparatePeriods(t *testing.T) {
	const bucket = 3600.0
	days := 60
	series := aggregatedSeries{}
	for i := 0; i < days*24; i++ {
		series.Times = append(series.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		series.Mean = append(series.Mean, 100+50*math.Sin(2*math.Pi*float64(i)/24))
	}
	window := Window{From: fixtureStart, To: fixtureStart.Add(time.Duration(days) * 24 * time.Hour)}
	coverage := Computed(Coverage{NPoints: days * 24, ExpectedPoints: days * 24, CompletenessRatio: 1})

	periods := mustGet(t, first(detectPeriodicity(series, window, bucket, coverage)), "dominant periods")

	for _, period := range periods {
		for multiple := 2; multiple <= 4; multiple++ {
			expected := 86400.0 * float64(multiple)
			if math.Abs(period-expected) < 0.05*expected {
				t.Errorf("periods = %v, want no %d-day harmonic of the daily cycle", periods, multiple)
			}
		}
	}
}

// A week is not an artefact of a day: both are real cycles in this domain, and
// §5.4.13 asks for both by name, so harmonic suppression must not eat the weekly.
func TestTheWeeklyCycleSurvivesHarmonicSuppression(t *testing.T) {
	const bucket = 3600.0
	weeks := 12
	series := aggregatedSeries{}
	for i := 0; i < weeks*7*24; i++ {
		daily := 50 * math.Sin(2*math.Pi*float64(i)/24)
		weekly := 90 * math.Sin(2*math.Pi*float64(i)/(24*7))
		series.Times = append(series.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		series.Mean = append(series.Mean, 100+daily+weekly)
	}
	window := Window{From: fixtureStart, To: fixtureStart.Add(time.Duration(weeks) * 7 * 24 * time.Hour)}
	coverage := Computed(Coverage{NPoints: weeks * 7 * 24, ExpectedPoints: weeks * 7 * 24, CompletenessRatio: 1})

	periods := mustGet(t, first(detectPeriodicity(series, window, bucket, coverage)), "dominant periods")

	weekly := false
	for _, period := range periods {
		if math.Abs(period-604800) < 0.05*604800 {
			weekly = true
		}
	}
	if !weekly {
		t.Errorf("periods = %v, want the weekly cycle kept", periods)
	}
}

// A counter's level is a monotone ramp: its autocorrelation is near one at every
// lag and its spectrum is the trend. Differencing first is what makes the daily
// shape of its rate visible instead.
func TestACountersTemporalStructureComesFromItsRateNotItsLevel(t *testing.T) {
	const bucket = 3600.0
	hours := 90 * 24
	level := aggregatedSeries{}
	total := 0.0
	for i := 0; i < hours; i++ {
		rate := 900 + 800*math.Sin(2*math.Pi*float64(i)/24)
		level.Times = append(level.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		level.Mean = append(level.Mean, total)
		total += rate * bucket
	}

	rate := differenceAggregated(level, bucket)
	if len(rate.Mean) != hours-1 {
		t.Fatalf("differenced length = %d, want %d", len(rate.Mean), hours-1)
	}

	window := Window{From: fixtureStart, To: fixtureStart.Add(time.Duration(hours) * time.Hour)}
	coverage := Computed(Coverage{NPoints: hours, ExpectedPoints: hours, CompletenessRatio: 1})

	fromRate := mustGet(t, first(detectPeriodicity(rate, window, bucket, coverage)), "periods of the rate")
	daily := false
	for _, period := range fromRate {
		if math.Abs(period-86400) < 3600 {
			daily = true
		}
	}
	if !daily {
		t.Errorf("periods of the rate = %v, want the daily cycle", fromRate)
	}
}

// A reset is a negative step in a counter, and one large negative outlier would
// dominate the spectrum of the differenced series.
func TestDifferencingACounterClampsAResetRatherThanEmittingASpike(t *testing.T) {
	level := aggregatedSeries{}
	for i, value := range []float64{100, 200, 300, 0, 100, 200} {
		level.Times = append(level.Times, fixtureStart.Add(time.Duration(i)*time.Hour))
		level.Mean = append(level.Mean, value)
	}

	rate := differenceAggregated(level, 3600)
	for i, value := range rate.Mean {
		if value < 0 {
			t.Errorf("rate[%d] = %v, want the reset clamped to zero rather than a negative spike", i, value)
		}
	}
}

// first is a readability helper for the two-value periodicity detector.
func first[A any, B any](a A, _ B) A { return a }

// A series denser than its modal interval implies is bursty, not over-complete.
// Seen live as `completeness_ratio: 4.58`, which reads as 458% coverage and would
// sail past the statistical gate as an unusually strong reading.
func TestCoverageIsCappedAtOneForASeriesDenserThanItsInterval(t *testing.T) {
	window := Window{From: fixtureStart, To: fixtureStart.Add(24 * time.Hour)}
	// A day at a quarter hour expects 96 points; this series delivered five times
	// that, because it bursts.
	coverage := mustGet(t, computeCoverage(480, 900, window), "coverage")

	if coverage.CompletenessRatio > 1 {
		t.Errorf("completeness_ratio = %v, want it capped at 1", coverage.CompletenessRatio)
	}
	// The exact counts still carry the signal, so capping hides nothing.
	if coverage.NPoints != 480 || coverage.ExpectedPoints != 96 {
		t.Errorf("counts = %d of %d, want the measured 480 of 96 kept",
			coverage.NPoints, coverage.ExpectedPoints)
	}
}
