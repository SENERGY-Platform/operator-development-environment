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
	"encoding/json"
	"sort"
)

// LLMProfileView is the one projection of a profile that reaches a model
// (D26). There is one stored form and one projection function; anything that
// needs a different shape derives it from this.
//
// Three things happen here. Unbounded arrays collapse to a summary plus a few
// exemplars — a washing machine over two years produces thousands of sessions,
// and unelided that alone exceeds any sane context budget. Provenance is dropped,
// because read modes and detector names are ODE's bookkeeping. And what was
// collapsed is *recorded*, so the model knows it is reading a summary and where
// the rest is.
type LLMProfileView struct {
	ProfileID       string    `json:"profile_id"`
	Tier            string    `json:"tier"`
	SeriesRef       SeriesRef `json:"series_ref"`
	DetectorVersion string    `json:"detector_version"`
	AnalysisWindow  Window    `json:"analysis_window"`
	RawWindow       RawWindow `json:"raw_window"`

	ServiceContext ServiceContext `json:"service_context"`

	Coverage       Value[Coverage]         `json:"coverage"`
	Sampling       Value[SamplingView]     `json:"sampling"`
	ValueSemantics ValueSemanticsView      `json:"value_semantics"`
	Distribution   Value[DistributionView] `json:"distribution"`

	TemporalStructure TemporalStructure   `json:"temporal_structure"`
	ActivityPattern   Value[ActivityView] `json:"activity_pattern"`
	QualityFlags      []QualityFlag       `json:"quality_flags"`
	Recommendations   Recommendations     `json:"recommendations"`

	// Overrides is what the developer decided, so the model can see the
	// correction next to the computation rather than only its result.
	Overrides []Resolution `json:"overrides"`
	Elided    []Elision    `json:"elided"`
}

// Elision records an array that was summarised, with where to fetch the rest.
type Elision struct {
	Field string `json:"field"`
	Total int    `json:"total"`
	Shown int    `json:"shown"`
	Fetch string `json:"fetch,omitempty"`
}

type SamplingView struct {
	DetectedIntervalS float64    `json:"detected_interval_s"`
	Regularity        Regularity `json:"regularity"`
	Confidence        Confidence `json:"confidence"`
	IrregularityRatio float64    `json:"irregularity_ratio"`
	Gaps              GapSummary `json:"gaps"`
}

type GapSummary struct {
	Count            int                       `json:"count"`
	TotalDurationS   float64                   `json:"total_duration_s"`
	ByClassification map[GapClassification]int `json:"by_classification"`
	Largest          []Gap                     `json:"largest"`
}

type DistributionView struct {
	Min          float64          `json:"min"`
	Max          float64          `json:"max"`
	Mean         float64          `json:"mean"`
	Median       float64          `json:"median"`
	P01          float64          `json:"p01"`
	P99          float64          `json:"p99"`
	StdDev       float64          `json:"std_dev"`
	ZeroRatio    float64          `json:"zero_ratio"`
	ConstantRuns ConstantRunsView `json:"constant_runs"`
}

type ConstantRunsView struct {
	Count   int          `json:"count"`
	Longest *ConstantRun `json:"longest,omitempty"`
}

type ValueSemanticsView struct {
	Kind                 Value[ValueKind]    `json:"kind"`
	KindConfidence       Value[Confidence]   `json:"kind_confidence"`
	KindEvidence         Value[KindEvidence] `json:"kind_evidence"`
	CharacteristicID     *string             `json:"characteristic_id"`
	Unit                 string              `json:"unit"`
	UnitSource           UnitSource          `json:"unit_source"`
	DeclaredRange        DeclaredRange       `json:"declared_range"`
	RangeViolationRatio  Value[float64]      `json:"range_violation_ratio"`
	CounterResets        CounterResetsView   `json:"counter_resets"`
	AvailableConversions []Conversion        `json:"available_conversions"`
	// Confirmed marks the fields a developer has decided on, which is what
	// raises their confidence to certain (D23).
	Confirmed []string `json:"confirmed,omitempty"`
}

type CounterResetsView struct {
	Status NotComputed `json:"status,omitempty"`
	Count  int         `json:"count"`
	First  []string    `json:"first,omitempty"`
}

type ActivityView struct {
	Classification           ActivityClassification `json:"classification"`
	ClassificationConfidence Confidence             `json:"classification_confidence"`
	IdleLevel                float64                `json:"idle_level"`
	ActiveThreshold          float64                `json:"active_threshold"`
	ThresholdMethod          string                 `json:"threshold_method"`
	SessionStats             Value[SessionStats]    `json:"session_stats"`
	SessionExemplars         []SessionExemplar      `json:"session_exemplars"`
	SessionsRef              string                 `json:"sessions_ref"`
}

const (
	// largestGapsShown and exemplarsShown are what §5.4.8 asks for: gaps as a
	// count plus the largest three, sessions as statistics plus three to five
	// exemplars.
	largestGapsShown = 3
	exemplarsShown   = 5
	resetsShown      = 5
	// tokensPerByte is the crude estimate used to decide whether a further
	// reduction is needed. JSON runs around four bytes to the token; being
	// approximate here is fine, because the consequence of a miss is a slightly
	// larger or smaller view, not a wrong one.
	bytesPerToken = 4
)

// Project collapses a resolved profile into the model-facing view.
//
// tokenBudget of zero means no budget pressure. When a budget is given and the
// view does not fit, detail is dropped in a fixed order — period evidence, then
// quality-flag evidence, then session exemplars, then the largest-gap list —
// and every drop is recorded as an elision. Dropping in a fixed order matters:
// the model should never have to guess which parts of a profile it is seeing.
func Project(resolved ResolvedProfile, tokenBudget int) LLMProfileView {
	profile := resolved.SeriesProfile
	view := LLMProfileView{
		ProfileID:         profile.ProfileID,
		Tier:              profile.Tier,
		SeriesRef:         profile.SeriesRef,
		DetectorVersion:   profile.DetectorVersion,
		AnalysisWindow:    profile.AnalysisWindow,
		RawWindow:         profile.RawWindow,
		ServiceContext:    profile.ServiceContext,
		Coverage:          profile.Coverage,
		TemporalStructure: profile.TemporalStructure,
		QualityFlags:      profile.QualityFlags,
		Recommendations:   profile.Recommendations,
		Overrides:         resolutions(resolved),
		Elided:            []Elision{},
	}

	view.Sampling, view.Elided = projectSampling(profile, view.Elided)
	view.Distribution, view.Elided = projectDistribution(profile, view.Elided)
	view.ValueSemantics, view.Elided = projectSemantics(resolved, view.Elided)
	view.ActivityPattern, view.Elided = projectActivity(resolved, view.Elided)

	if tokenBudget > 0 {
		view = reduce(view, tokenBudget)
	}
	return view
}

func projectSampling(profile SeriesProfile, elided []Elision) (Value[SamplingView], []Elision) {
	sampling, ok := profile.Sampling.Get()
	if !ok {
		status := profile.Sampling.Status()
		return Uncomputable[SamplingView](status.Reason, status.Detail), elided
	}

	summary := GapSummary{
		Count:            len(sampling.Gaps),
		ByClassification: map[GapClassification]int{},
		Largest:          []Gap{},
	}
	for _, gap := range sampling.Gaps {
		summary.TotalDurationS += gap.DurationS
		summary.ByClassification[gap.Classification]++
	}
	summary.TotalDurationS = roundTo(summary.TotalDurationS, 1)

	largest := append([]Gap{}, sampling.Gaps...)
	sort.SliceStable(largest, func(i, j int) bool { return largest[i].DurationS > largest[j].DurationS })
	shown := largestGapsShown
	if shown > len(largest) {
		shown = len(largest)
	}
	summary.Largest = largest[:shown]

	if len(sampling.Gaps) > shown {
		elided = append(elided, Elision{
			Field: FieldSamplingGaps, Total: len(sampling.Gaps), Shown: shown,
		})
	}

	return Computed(SamplingView{
		DetectedIntervalS: sampling.DetectedIntervalS,
		Regularity:        sampling.Regularity,
		Confidence:        sampling.Confidence,
		IrregularityRatio: sampling.IrregularityRatio,
		Gaps:              summary,
	}), elided
}

func projectDistribution(profile SeriesProfile, elided []Elision) (Value[DistributionView], []Elision) {
	distribution, ok := profile.Distribution.Get()
	if !ok {
		status := profile.Distribution.Status()
		return Uncomputable[DistributionView](status.Reason, status.Detail), elided
	}

	runs := ConstantRunsView{Count: len(distribution.ConstantRuns)}
	if longest, found := longestRun(distribution.ConstantRuns); found {
		copied := longest
		runs.Longest = &copied
	}
	if len(distribution.ConstantRuns) > 1 {
		elided = append(elided, Elision{
			Field: FieldConstantRuns, Total: len(distribution.ConstantRuns), Shown: 1,
		})
	}

	return Computed(DistributionView{
		Min: distribution.Min, Max: distribution.Max, Mean: distribution.Mean,
		Median: distribution.Median, P01: distribution.P01, P99: distribution.P99,
		StdDev: distribution.StdDev, ZeroRatio: distribution.ZeroRatio,
		ConstantRuns: runs,
	}), elided
}

func projectSemantics(resolved ResolvedProfile, elided []Elision) (ValueSemanticsView, []Elision) {
	semantics := resolved.ValueSemantics
	view := ValueSemanticsView{
		Kind:                 semantics.Kind,
		KindConfidence:       semantics.KindConfidence,
		KindEvidence:         semantics.KindEvidence,
		CharacteristicID:     semantics.CharacteristicID,
		Unit:                 semantics.Unit,
		UnitSource:           semantics.UnitSource,
		DeclaredRange:        semantics.DeclaredRange,
		RangeViolationRatio:  semantics.RangeViolationRatio,
		AvailableConversions: semantics.AvailableConversions,
	}

	// Developer decisions win over the detector, and the field is marked as
	// confirmed so the model reads it as settled rather than as another estimate.
	if unit, overridden := effectiveString(resolved, FieldUnit, semantics.Unit); overridden {
		view.Unit = unit
		view.UnitSource = UnitSource("developer")
		view.Confirmed = append(view.Confirmed, FieldUnit)
	}
	if id, overridden := effectiveString(resolved, FieldCharacteristic, derefString(semantics.CharacteristicID)); overridden {
		if id == "" {
			view.CharacteristicID = nil
		} else {
			copied := id
			view.CharacteristicID = &copied
		}
		view.Confirmed = append(view.Confirmed, FieldCharacteristic)
	}
	if kind, overridden := effectiveString(resolved, FieldValueKind, ""); overridden && kind != "" {
		view.Kind = Computed(ValueKind(kind))
		view.KindConfidence = Computed(Certain)
		view.Confirmed = append(view.Confirmed, FieldValueKind)
	}

	resets := CounterResetsView{}
	if timestamps, ok := semantics.CounterResets.Get(); ok {
		resets.Count = len(timestamps)
		shown := resetsShown
		if shown > len(timestamps) {
			shown = len(timestamps)
		}
		for _, at := range timestamps[:shown] {
			resets.First = append(resets.First, at.UTC().Format("2006-01-02T15:04:05Z"))
		}
		if len(timestamps) > shown {
			elided = append(elided, Elision{
				Field: FieldCounterResets, Total: len(timestamps), Shown: shown,
			})
		}
	} else {
		resets.Status = semantics.CounterResets.Status()
	}
	view.CounterResets = resets

	return view, elided
}

func projectActivity(resolved ResolvedProfile, elided []Elision) (Value[ActivityView], []Elision) {
	activity, ok := resolved.ActivityPattern.Get()
	if !ok {
		status := resolved.ActivityPattern.Status()
		return Uncomputable[ActivityView](status.Reason, status.Detail), elided
	}

	view := ActivityView{
		Classification:           activity.Classification,
		ClassificationConfidence: activity.ClassificationConfidence,
		IdleLevel:                activity.IdleLevel,
		ActiveThreshold:          activity.ActiveThreshold,
		ThresholdMethod:          activity.ThresholdMethod,
		SessionStats:             activity.SessionStats,
		SessionExemplars:         activity.SessionExemplars,
		SessionsRef:              activity.SessionsRef,
	}

	if classification, overridden := effectiveString(resolved, FieldActivityClass, string(activity.Classification)); overridden {
		view.Classification = ActivityClassification(classification)
		view.ClassificationConfidence = Certain
	}

	if len(view.SessionExemplars) > exemplarsShown {
		view.SessionExemplars = view.SessionExemplars[:exemplarsShown]
	}
	if stats, ok := activity.SessionStats.Get(); ok && stats.Count > len(view.SessionExemplars) {
		elided = append(elided, Elision{
			Field: pathKey(FieldActivity, "sessions"),
			Total: stats.Count, Shown: len(view.SessionExemplars),
			Fetch: activity.SessionsRef,
		})
	}
	return Computed(view), elided
}

func resolutions(resolved ResolvedProfile) []Resolution {
	out := make([]Resolution, 0, len(resolved.Resolution))
	for _, resolution := range resolved.Resolution {
		out = append(out, resolution)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FieldPath < out[j].FieldPath })
	return out
}

// effectiveString applies an override whose value is a string, which covers
// every confirmable field a projection substitutes. A rejection clears the
// value rather than keeping the computed one.
func effectiveString(resolved ResolvedProfile, fieldPath string, computed string) (string, bool) {
	resolution, overridden := resolved.Resolution[fieldPath]
	if !overridden {
		return computed, false
	}
	switch resolution.Action {
	case ActionCorrect:
		if text, ok := resolution.ConfirmedValue.(string); ok {
			return text, true
		}
		return computed, true
	case ActionReject:
		return "", true
	default:
		return computed, true
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// reduce drops detail until the view fits the budget, in a fixed order, and
// records each drop.
func reduce(view LLMProfileView, tokenBudget int) LLMProfileView {
	fits := func(candidate LLMProfileView) bool {
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return true
		}
		return len(encoded)/bytesPerToken <= tokenBudget
	}
	if fits(view) {
		return view
	}

	if evidence, ok := view.TemporalStructure.PeriodEvidence.Get(); ok && len(evidence) > 0 {
		view.TemporalStructure.PeriodEvidence = Uncomputablef[[]PeriodEvidence](ReasonOutOfScope,
			"dropped to fit a %d token budget; %d entries elided", tokenBudget, len(evidence))
		view.Elided = append(view.Elided, Elision{
			Field: pathKey(FieldPeriods, "evidence"), Total: len(evidence), Shown: 0,
		})
		if fits(view) {
			return view
		}
	}

	if len(view.QualityFlags) > 0 {
		stripped := make([]QualityFlag, 0, len(view.QualityFlags))
		for _, flag := range view.QualityFlags {
			stripped = append(stripped, QualityFlag{Flag: flag.Flag, Confidence: flag.Confidence})
		}
		view.QualityFlags = stripped
		view.Elided = append(view.Elided, Elision{
			Field: pathKey(FieldQualityFlags, "evidence"), Total: len(stripped), Shown: 0,
		})
		if fits(view) {
			return view
		}
	}

	if activity, ok := view.ActivityPattern.Get(); ok && len(activity.SessionExemplars) > 1 {
		total := len(activity.SessionExemplars)
		activity.SessionExemplars = activity.SessionExemplars[:1]
		view.ActivityPattern = Computed(activity)
		view.Elided = append(view.Elided, Elision{
			Field: pathKey(FieldActivity, "session_exemplars"), Total: total, Shown: 1,
			Fetch: activity.SessionsRef,
		})
		if fits(view) {
			return view
		}
	}

	if sampling, ok := view.Sampling.Get(); ok && len(sampling.Gaps.Largest) > 0 {
		total := sampling.Gaps.Count
		sampling.Gaps.Largest = []Gap{}
		view.Sampling = Computed(sampling)
		view.Elided = append(view.Elided, Elision{
			Field: FieldSamplingGaps, Total: total, Shown: 0,
		})
	}
	return view
}
