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

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

type detectionInput struct {
	device    models.ExtendedDevice
	service   models.Service
	variables []Variable
	rawSet    timeseries.ResultSet
	aggregate map[string]aggregatedSeries
	analysis  Window
	raw       RawWindow
	// cacheRaw is the raw window that was *requested*, which is what the cache key
	// is built from (D25). It is not raw.Window: the point budget narrows that one
	// to the span actually read, and a key built from the narrowed window can
	// never match the one the lookup used, so an identical repeat re-read the
	// platform every time while reporting the profiles as cached.
	cacheRaw  Window
	groupTime string
	bucket    float64
	index     *OntologyIndex
	params    *SessionParams
	// rawAvailable is what /data-availability said. False means retention has left
	// only aggregated buckets, which is the difference between "this series is
	// broken" and "there is nothing unbucketed left to read". Not computed means the
	// probe itself failed, which is a third thing again (D24).
	rawAvailable Value[bool]
}

type computedProfile struct {
	profile  SeriesProfile
	sessions []Session
}

// detect runs the detectors in the order §5.4.13 sets out, in two passes over
// the variables.
//
// The first pass establishes each variable's series and value kind, because the
// cross-variable checks and the sibling list both need every variable's kind
// before any one profile can be assembled. The second pass builds the profiles.
func (p *Profiler) detect(input detectionInput) []computedProfile {
	series := make(map[string]variableSeries, len(input.variables))
	sampling := make(map[string]Value[Sampling], len(input.variables))
	intervals := make(map[string]float64, len(input.variables))
	coverage := make(map[string]Value[Coverage], len(input.variables))
	kinds := make(map[string]kindResult, len(input.variables))

	// Columns are extracted once and reused. Column() walks the whole row set and
	// copies the non-null values, so calling it again per variable in the second
	// pass costs another full copy of every point read.
	columns := make(map[string]timeseries.Column, len(input.variables))
	present := make(map[string]bool, len(input.variables))

	for _, variable := range input.variables {
		column, found := input.rawSet.Column(variable.Path)
		if !found {
			column = timeseries.Column{Name: variable.Path}
		}
		columns[variable.Path] = column
		present[variable.Path] = found

		detected, interval := detectSampling(column.Times)
		sampling[variable.Path] = detected
		intervals[variable.Path] = interval
		coverage[variable.Path] = computeCoverage(column.Len(), interval, input.raw.Window)

		kind := detectValueKind(column, variable.Type)
		kinds[variable.Path] = kind

		times, values, _ := column.Numeric()
		resolvedKind, _ := kind.Kind.Get()
		series[variable.Path] = variableSeries{
			Variable: variable, Times: times, Values: values, Kind: resolvedKind,
		}
	}

	siblings := siblingSummaries(input.variables, kinds)
	out := make([]computedProfile, 0, len(input.variables))

	// One sorted union of sibling arrival times, built once. Gap classification
	// binary-searches it, and rebuilding and re-sorting it per variable would cost
	// more than every detector put together at the raw point limit.
	siblingTimes := make(map[string][]time.Time, len(input.variables))
	for _, variable := range input.variables {
		siblingTimes[variable.Path] = siblingTimestamps(series, variable.Path)
	}

	for _, variable := range input.variables {
		out = append(out, p.profileVariable(input, variable, variableContext{
			series:       series,
			column:       columns[variable.Path],
			columnFound:  present[variable.Path],
			siblingTimes: siblingTimes[variable.Path],
			sampling:     sampling[variable.Path],
			interval:     intervals[variable.Path],
			coverage:     coverage[variable.Path],
			kind:         kinds[variable.Path],
			siblings:     siblings,
			allKinds:     kinds,
		}))
	}
	return out
}

type variableContext struct {
	series map[string]variableSeries
	// column is this variable's raw column, extracted once in the first pass.
	column      timeseries.Column
	columnFound bool
	// siblingTimes is the sorted union of every other variable's arrival times.
	siblingTimes []time.Time
	sampling     Value[Sampling]
	interval     float64
	coverage     Value[Coverage]
	kind         kindResult
	siblings     []SiblingVariable
	allKinds     map[string]kindResult
}

func (p *Profiler) profileVariable(input detectionInput, variable Variable, ctx variableContext) computedProfile {
	prov := Provenance{}
	ref := SeriesRef{
		DeviceID:     input.device.Id,
		ServiceID:    input.service.Id,
		VariablePath: variable.Path,
	}
	cacheKey := CacheKey(ref, input.analysis, input.cacheRaw, DetectorVersion)

	own := ctx.series[variable.Path]
	aggregate := input.aggregate[variable.Path]

	// What the platform actually returned, before any detector had an opinion
	// about it. This is what turns "no sampling interval" from a symptom into a
	// diagnosis.
	summary := ReadSummary{
		RawAvailable:      input.rawAvailable,
		RawRows:           input.rawSet.Rows(),
		AggregatedBuckets: len(aggregate.Mean),
	}
	if ctx.columnFound {
		summary.ValuesPresent = ctx.column.Len()
		summary.NullRows = ctx.column.NullRows
	}
	summary.Diagnosis = summary.Diagnose(variable.Queryable, variable.Numeric(), variable.Reason)

	// Detector 3: units, from the ontology, no read.
	semantics := ResolveUnits(variable, input.index, prov)
	semantics.Kind = ctx.kind.Kind
	semantics.KindConfidence = ctx.kind.Confidence
	semantics.KindEvidence = ctx.kind.Evidence
	semantics.CounterResets = ctx.kind.Resets
	semantics.RangeViolationRatio = detectRangeViolation(own.Values, semantics.DeclaredRange)
	prov.FromRaw(FieldValueKind, "value_semantics_v1", input.raw.Window)
	prov.FromRaw(FieldCounterResets, "counter_reset_v1", input.raw.Window)
	prov.FromRaw(FieldRangeViolation, "range_violation_v1", input.raw.Window)

	// Detectors 1 and 4: sampling and gap classification, raw pass.
	samplingValue := ctx.sampling
	if detected, ok := samplingValue.Get(); ok {
		detected.Gaps = classifyGaps(detected.Gaps, ctx.siblingTimes,
			input.device.ConnectionState, input.raw.To)
		samplingValue = Computed(detected)
	}
	prov.FromRaw(FieldSamplingInterval, "sampling_v1", input.raw.Window)
	prov.FromRaw(FieldSamplingGaps, "gap_v1", input.raw.Window)
	prov.FromRaw(FieldCoverage, "coverage_v1", input.raw.Window)

	// Distribution: aggregated over the full range, with constant runs from the
	// raw pass because bucketing destroys them.
	distribution := detectDistribution(aggregate, ctx.coverage)
	distributionMode := ReadAggregated
	if !distribution.IsComputed() && len(aggregate.Mean) == 0 && len(own.Values) >= minDistributionBuckets {
		// The aggregated pass produced nothing for this variable. A distribution
		// over the raw window is a smaller claim than none at all, as long as the
		// provenance says which window it describes.
		distribution = distributionFromRaw(own.Values)
		distributionMode = ReadRaw
	}
	constantRuns := detectConstantRuns(own.Times, own.Values)
	if computed, ok := distribution.Get(); ok {
		if runs, ok := constantRuns.Get(); ok {
			computed.ConstantRuns = runs
		}
		distribution = Computed(computed)
	}
	switch distributionMode {
	case ReadRaw:
		prov.Set(FieldDistribution, ProvenanceEntry{
			ReadMode: ReadRaw, Source: SourceDetector, Detector: "distribution_v1",
			Window: &input.raw.Window,
			Note:   "the aggregated pass returned nothing for this variable, so the distribution describes the raw window only",
		})
	default:
		prov.Set(FieldDistribution, ProvenanceEntry{
			ReadMode: ReadAggregated, Source: SourceDetector, Detector: "distribution_v1",
			Window: &input.analysis, GroupTime: input.groupTime,
			Note: "min and max come from min/max buckets and are exact; the percentiles are percentiles of bucket means",
		})
	}
	prov.FromRaw(FieldConstantRuns, "constant_run_v1", input.raw.Window)

	// Detector 6 and the trend and stationarity tests: aggregated pass.
	//
	// A cumulative counter is differenced first. Its level is a monotone ramp, so
	// its autocorrelation is near one at every lag, its spectrum is dominated by
	// the trend, and its unit root is a foregone conclusion — all three detectors
	// would report something true of every counter and nothing about this one. The
	// rate of change is where a counter's daily shape actually lives.
	temporal := TemporalStructure{}
	temporalSeries := aggregate
	differenced := false
	if kindForTemporal, ok := ctx.kind.Kind.Get(); ok && kindForTemporal == KindCumulativeCounter {
		temporalSeries = differenceAggregated(aggregate, input.bucket)
		differenced = true
	}

	if variable.Numeric() {
		temporal.DominantPeriodsS, temporal.PeriodEvidence =
			detectPeriodicity(temporalSeries, input.analysis, input.bucket, ctx.coverage)
		temporal.Trend = detectTrend(temporalSeries.Times, temporalSeries.Mean, ctx.coverage)
		temporal.Stationarity = detectStationarity(temporalSeries.Mean, ctx.coverage)
	} else {
		reason := "the variable is not numeric, so it has no temporal structure to measure"
		temporal.DominantPeriodsS = Uncomputable[[]float64](ReasonWrongKind, reason)
		temporal.PeriodEvidence = Uncomputable[[]PeriodEvidence](ReasonWrongKind, reason)
		temporal.Trend = Uncomputable[Trend](ReasonWrongKind, reason)
		temporal.Stationarity = Uncomputable[Stationarity](ReasonWrongKind, reason)
	}
	// Differencing changes what the three fields describe, so it has to be on the
	// record: the trend of a differenced counter is a change in *rate* per day,
	// not a change in the reading.
	differencedNote := ""
	if differenced {
		differencedNote = "computed on the differenced counter, so this describes its rate of change rather than its level"
	}
	prov.Set(FieldPeriods, ProvenanceEntry{
		ReadMode: ReadAggregated, Source: SourceDetector, Detector: "periodicity_v1",
		Window: &input.analysis, GroupTime: input.groupTime, Note: differencedNote,
	})
	prov.Set(FieldTrend, ProvenanceEntry{
		ReadMode: ReadAggregated, Source: SourceDetector, Detector: "trend_ols_v1",
		Window: &input.analysis, GroupTime: input.groupTime, Note: differencedNote,
	})
	stationarityNote := "augmented Dickey-Fuller with a constant, asymptotic critical values, p-value bracketed rather than interpolated"
	if differenced {
		stationarityNote += "; " + differencedNote
	}
	prov.Set(FieldStationarity, ProvenanceEntry{
		ReadMode: ReadAggregated, Source: SourceDetector, Detector: "adf_v1",
		Window: &input.analysis, GroupTime: input.groupTime, Note: stationarityNote,
	})

	// Detector 7: sessions, raw pass.
	params := DefaultSessionParams(ctx.interval)
	if input.params != nil {
		params = *input.params
	}
	kind, _ := ctx.kind.Kind.Get()
	regularity := Mixed
	if detected, ok := samplingValue.Get(); ok {
		regularity = detected.Regularity
	}
	activity, sessions := detectActivity(activityInput{
		Times: own.Times, Values: own.Values, Interval: ctx.interval,
		Kind: kind, Regularity: regularity, Params: params, ProfileID: cacheKey,
	})
	prov.FromRaw(FieldActivity, "session_v1", input.raw.Window)
	prov.FromRaw(FieldSessionStats, "session_v1", input.raw.Window)

	// Detector 8: cross-variable relationships, from the service-scoped batch.
	relationships := detectRelationships(own, siblingSeries(ctx.series, variable.Path))
	prov.FromRaw(FieldRelationships, "relationship_v1", input.raw.Window)

	// Detector 9: quality flags.
	flags := detectQualityFlags(qualityInput{
		Variable: variable, Sampling: samplingValue, Coverage: ctx.coverage,
		Semantics: semantics, ConstantRuns: constantRuns, RawValues: own.Values,
		Relationships: relationships, Window: input.analysis,
		Interval: ctx.interval, LocalZone: p.localZone,
	})
	prov.FromRaw(FieldQualityFlags, "quality_v1", input.raw.Window)

	exclusions := aggregatedGaps(aggregate, input.analysis, input.bucket)
	recommendations := recommend(recommendInput{
		interval: ctx.interval, kind: ctx.kind.Kind, sampling: samplingValue,
		analysis: input.analysis, exclusions: exclusions,
	})
	prov.FromAggregated(FieldExclusions, "aggregated_gap_v1", input.analysis, input.groupTime)
	prov.FromAggregated(FieldUsableRange, "usable_range_v1", input.analysis, input.groupTime)

	profile := SeriesProfile{
		ProfileID:       cacheKey,
		CacheKey:        cacheKey,
		Tier:            TierFull,
		SeriesRef:       ref,
		DetectorVersion: DetectorVersion,
		AnalysisWindow:  input.analysis,
		RawWindow:       input.raw,
		ComputedAt:      p.now(),
		ServiceContext: ServiceContext{
			ServiceID:        input.service.Id,
			Interaction:      string(input.service.Interaction),
			SiblingVariables: excludeSelf(ctx.siblings, variable.Path),
			Relationships:    relationships,
		},
		ReadSummary:       summary,
		Coverage:          explainCoverage(ctx.coverage, summary),
		Sampling:          samplingValue,
		ValueSemantics:    semantics,
		Distribution:      distribution,
		TemporalStructure: temporal,
		ActivityPattern:   activity,
		QualityFlags:      flags,
		Recommendations:   recommendations,
		Provenance:        prov,
	}
	return computedProfile{profile: profile, sessions: sessions}
}

// explainCoverage replaces a bare non-result with one that names the cause.
//
// "no sampling interval, so no expected point count" is true and useless: it
// describes what the detector lacked, not why. The read summary knows why, and a
// developer staring at an empty profile should not have to work back from the
// symptom to the retention policy that caused it.
func explainCoverage(coverage Value[Coverage], summary ReadSummary) Value[Coverage] {
	if coverage.IsComputed() || summary.Diagnosis == "" {
		return coverage
	}
	status := coverage.Status()
	return Uncomputablef[Coverage](status.Reason, "%s — %s", status.Detail, summary.Diagnosis)
}

// differenceAggregated turns a counter's bucketed level into its rate of change
// per second, keeping the grid uniform so the ACF and the FFT still apply.
//
// A negative step is a reset rather than negative consumption, and one large
// negative outlier would dominate the spectrum, so it is clamped to zero. The
// resets themselves are already reported by their own detector and by a quality
// flag; suppressing them here loses nothing and keeps this from becoming a
// finding about the reset instead of about the series.
func differenceAggregated(series aggregatedSeries, bucketSeconds float64) aggregatedSeries {
	if len(series.Mean) < 2 || bucketSeconds <= 0 {
		return aggregatedSeries{}
	}
	out := aggregatedSeries{
		Times: make([]time.Time, 0, len(series.Times)-1),
		Mean:  make([]float64, 0, len(series.Mean)-1),
	}
	for i := 1; i < len(series.Mean) && i < len(series.Times); i++ {
		delta := series.Mean[i] - series.Mean[i-1]
		if delta < 0 {
			delta = 0
		}
		out.Times = append(out.Times, series.Times[i])
		out.Mean = append(out.Mean, delta/bucketSeconds)
	}
	return out
}

// distributionFromRaw is the fallback when the aggregated pass returned nothing.
// Every figure is exact for the raw window, which is the point: it is a narrower
// claim rather than a weaker one.
func distributionFromRaw(values []float64) Value[Distribution] {
	clean := finite(values)
	if len(clean) < minDistributionBuckets {
		return Uncomputablef[Distribution](ReasonInsufficientCoverage,
			"%d raw points, need at least %d for a distribution", len(clean), minDistributionBuckets)
	}
	sorted := sortedCopy(clean)
	zeros := 0
	for _, v := range clean {
		if v == 0 {
			zeros++
		}
	}
	return Computed(Distribution{
		Min:          roundTo(sorted[0], 4),
		Max:          roundTo(sorted[len(sorted)-1], 4),
		Mean:         roundTo(mean(clean), 4),
		Median:       roundTo(percentile(sorted, 0.5), 4),
		P01:          roundTo(percentile(sorted, 0.01), 4),
		P99:          roundTo(percentile(sorted, 0.99), 4),
		StdDev:       roundTo(stddev(clean), 4),
		ZeroRatio:    round2(float64(zeros) / float64(len(clean))),
		ConstantRuns: []ConstantRun{},
	})
}

func siblingSummaries(variables []Variable, kinds map[string]kindResult) []SiblingVariable {
	out := make([]SiblingVariable, 0, len(variables))
	for _, variable := range variables {
		kind := ""
		if resolved, ok := kinds[variable.Path].Kind.Get(); ok {
			kind = string(resolved)
		}
		out = append(out, SiblingVariable{
			Path:             variable.Path,
			CharacteristicID: variable.CharacteristicID,
			Kind:             kind,
		})
	}
	return out
}

func excludeSelf(siblings []SiblingVariable, path string) []SiblingVariable {
	out := make([]SiblingVariable, 0, len(siblings))
	for _, sibling := range siblings {
		if sibling.Path != path {
			out = append(out, sibling)
		}
	}
	return out
}

func siblingSeries(all map[string]variableSeries, path string) []variableSeries {
	out := make([]variableSeries, 0, len(all))
	paths := make([]string, 0, len(all))
	for key := range all {
		if key != path {
			paths = append(paths, key)
		}
	}
	// Sorted so the relationship list does not depend on map iteration order.
	sort.Strings(paths)
	for _, key := range paths {
		out = append(out, all[key])
	}
	return out
}

// siblingTimestamps is the union of every other variable's arrival times, which
// is what tells a dead channel from a device that was offline.
func siblingTimestamps(all map[string]variableSeries, path string) []time.Time {
	out := []time.Time{}
	for key, series := range all {
		if key == path {
			continue
		}
		out = append(out, series.Times...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// aggregatedGaps finds the holes in the analysis window from the buckets the
// server did not return.
//
// This is the one gap signal that covers the whole analysis window rather than
// just the bounded raw read: an absent bucket means no data fell in it, at bucket
// resolution, for free. It is coarser than the raw gap list and complements it
// rather than replacing it.
func aggregatedGaps(series aggregatedSeries, analysis Window, bucket float64) []Exclusion {
	if len(series.Times) == 0 {
		return []Exclusion{}
	}
	// Sized from the window divided by the bucket, so the bound is checked before
	// the allocation rather than after it — the same exposure onUniformGrid had to
	// a group_time the model chose.
	buckets, sized := gridBuckets(analysis, bucket)
	if !sized {
		return []Exclusion{}
	}

	present := make([]bool, buckets)
	for _, at := range series.Times {
		index := int(math.Floor(at.Sub(analysis.From).Seconds() / bucket))
		if index >= 0 && index < buckets {
			present[index] = true
		}
	}

	out := []Exclusion{}
	run := -1
	flush := func(end int) {
		if run < 0 || end-run+1 < minExclusionBuckets {
			return
		}
		out = append(out, Exclusion{
			From:   analysis.From.Add(time.Duration(float64(run) * bucket * float64(time.Second))),
			To:     analysis.From.Add(time.Duration(float64(end+1) * bucket * float64(time.Second))),
			Reason: "no data in the aggregated buckets covering this range",
		})
	}
	for i, filled := range present {
		if filled {
			flush(i - 1)
			run = -1
			continue
		}
		if run < 0 {
			run = i
		}
	}
	flush(buckets - 1)
	return out
}

type recommendInput struct {
	interval   float64
	kind       Value[ValueKind]
	sampling   Value[Sampling]
	analysis   Window
	exclusions []Exclusion
}

// recommend fills the advisory block (D28). Nothing downstream reads it: these
// values become binding only when a developer promotes them explicitly, and the
// promotion is a separate recorded action. A threshold heuristic setting the
// resampling policy with nobody deciding is the autonomous behaviour this design
// rejects.
func recommend(input recommendInput) Recommendations {
	out := Recommendations{Advisory: true, Exclusions: input.exclusions}

	if input.interval > 0 {
		out.ResampleToS = Computed(roundTo(bucketSecondsOf(chooseGroupTime(input.analysis, input.interval)), 1))
	} else {
		out.ResampleToS = Uncomputable[float64](ReasonInsufficientCoverage,
			"no sampling interval was detected to resample against")
	}

	kind, kindKnown := input.kind.Get()
	regularity := Mixed
	if sampling, ok := input.sampling.Get(); ok {
		regularity = sampling.Regularity
	}
	switch {
	case !kindKnown:
		out.InterpolationStrategy = Uncomputable[string](ReasonWrongKind,
			"the value kind is unknown, and the right filling depends on it")
	case kind == KindCumulativeCounter:
		// Carrying the last reading forward is the only filling that keeps a
		// counter monotonic; interpolating invents consumption that did not
		// happen.
		out.InterpolationStrategy = Computed(InterpolationFFill)
	case kind == KindBinary || kind == KindCategorical || kind == KindStatus:
		out.InterpolationStrategy = Computed(InterpolationFFill)
	case regularity == Regular:
		out.InterpolationStrategy = Computed(InterpolationLinear)
	default:
		out.InterpolationStrategy = Computed(InterpolationNone)
	}

	out.UsableRange = Computed(trimExclusions(input.analysis, input.exclusions))
	return out
}

// trimExclusions pulls the usable range in past a hole at either end. Interior
// holes stay as exclusions: cutting the range at the first of them would throw
// away everything after it.
func trimExclusions(analysis Window, exclusions []Exclusion) Window {
	out := analysis
	for _, exclusion := range exclusions {
		if !exclusion.From.After(out.From) && exclusion.To.After(out.From) {
			out.From = exclusion.To
		}
	}
	for i := len(exclusions) - 1; i >= 0; i-- {
		exclusion := exclusions[i]
		if !exclusion.To.Before(out.To) && exclusion.From.Before(out.To) {
			out.To = exclusion.From
		}
	}
	if !out.Valid() {
		return analysis
	}
	return out
}
