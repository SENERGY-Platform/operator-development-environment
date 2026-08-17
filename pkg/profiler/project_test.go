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
	"strings"
	"testing"
	"time"
)

// projectable is a profile with the unbounded arrays the projection exists to
// collapse: a washing machine's worth of sessions, hundreds of gaps and constant
// runs, and a counter with more resets than anyone needs listed.
func projectable() SeriesProfile {
	gaps := make([]Gap, 0, 400)
	for i := 0; i < 400; i++ {
		from := fixtureStart.Add(time.Duration(i) * time.Hour)
		gaps = append(gaps, Gap{
			From: from, To: from.Add(time.Duration(i+1) * time.Minute),
			DurationS: float64((i + 1) * 60), Classification: GapUnknown,
		})
	}
	runs := make([]ConstantRun, 0, 50)
	for i := 0; i < 50; i++ {
		from := fixtureStart.Add(time.Duration(i) * time.Hour)
		runs = append(runs, ConstantRun{
			From: from, To: from.Add(time.Duration(i+1) * time.Minute),
			DurationS: float64((i + 1) * 60), Value: 7, Points: i + 3,
		})
	}
	resets := make([]time.Time, 0, 20)
	for i := 0; i < 20; i++ {
		resets = append(resets, fixtureStart.Add(time.Duration(i)*24*time.Hour))
	}
	exemplars := make([]SessionExemplar, 0, 12)
	for i := 0; i < 12; i++ {
		from := fixtureStart.Add(time.Duration(i) * 6 * time.Hour)
		exemplars = append(exemplars, SessionExemplar{
			From: from, To: from.Add(time.Hour), DurationS: 3600, Energy: 1.5, Peak: 1800,
		})
	}
	periodEvidence := make([]PeriodEvidence, 0, 3)
	for i := 0; i < 3; i++ {
		periodEvidence = append(periodEvidence, PeriodEvidence{
			PeriodS: float64(86400 * (i + 1)), Method: "acf", Strength: 0.5, Label: "daily",
		})
	}

	analysis := Window{From: fixtureStart, To: fixtureStart.Add(90 * 24 * time.Hour)}
	return SeriesProfile{
		ProfileID: "profile-1", CacheKey: "profile-1", Tier: TierFull,
		SeriesRef: testRef(), DetectorVersion: DetectorVersion,
		AnalysisWindow: analysis,
		RawWindow:      RawWindow{Window: Window{From: analysis.To.Add(-14 * 24 * time.Hour), To: analysis.To}},
		Coverage:       Computed(Coverage{NPoints: 1344, ExpectedPoints: 1344, CompletenessRatio: 1}),
		Sampling: Computed(Sampling{
			DetectedIntervalS: 900, Regularity: Regular, Confidence: Likely, Gaps: gaps,
		}),
		ValueSemantics: ValueSemantics{
			Kind: Computed(KindCumulativeCounter), KindConfidence: Computed(Likely),
			Unit: "W", UnitSource: UnitInferred,
			CounterResets: Computed(resets),
			DeclaredRange: DeclaredRange{
				Min: Uncomputable[float64](ReasonOutOfScope, "none declared"),
				Max: Uncomputable[float64](ReasonOutOfScope, "none declared"),
			},
			RangeViolationRatio: Uncomputable[float64](ReasonOutOfScope, "no range"),
		},
		Distribution: Computed(Distribution{
			Min: 1, Max: 1800, Mean: 400, Median: 380, P01: 2, P99: 1790, ConstantRuns: runs,
		}),
		TemporalStructure: TemporalStructure{
			DominantPeriodsS: Computed([]float64{86400}),
			PeriodEvidence:   Computed(periodEvidence),
			Trend:            Uncomputable[Trend](ReasonInsufficientCoverage, "too sparse"),
			Stationarity:     Uncomputable[Stationarity](ReasonInsufficientSpan, "289 observations, need 500"),
		},
		ActivityPattern: Computed(ActivityPattern{
			Classification: ActivitySessionBased, ClassificationConfidence: Likely,
			IdleLevel: 2, ActiveThreshold: 900, ThresholdMethod: "otsu",
			SessionStats:     Computed(SessionStats{Count: 1847, MedianDurationS: 3600}),
			SessionExemplars: exemplars,
			SessionsRef:      SessionsPath("profile-1"),
		}),
		QualityFlags: []QualityFlag{{
			Flag: FlagFrozenSensor, Confidence: Likely,
			Evidence: map[string]any{"longest_constant_run_s": 3000.0},
		}},
		Recommendations: Recommendations{Advisory: true, Exclusions: []Exclusion{}},
		Provenance: Provenance{
			FieldSamplingGaps: {ReadMode: ReadRaw, Source: SourceDetector, Detector: "gap_v1"},
			FieldUnit:         {ReadMode: ReadNone, Source: SourceOntology, Ref: "characteristic:ch-watt"},
		},
	}
}

// A washing machine over two years produces thousands of sessions; unelided that
// alone exceeds any sane context budget.
func TestSessionsCollapseToStatisticsExemplarsAndAFetchReference(t *testing.T) {
	view := Project(Resolve(projectable(), nil), 0)

	activity := mustGet(t, view.ActivityPattern, "activity")
	if len(activity.SessionExemplars) != exemplarsShown {
		t.Errorf("exemplars = %d, want %d", len(activity.SessionExemplars), exemplarsShown)
	}
	stats := mustGet(t, activity.SessionStats, "session stats")
	if stats.Count != 1847 {
		t.Errorf("count = %d, want the full total kept", stats.Count)
	}

	elision := findElision(t, view, pathKey(FieldActivity, "sessions"))
	if elision.Total != 1847 || elision.Shown != exemplarsShown {
		t.Errorf("elision = %+v, want 1847 total and %d shown", elision, exemplarsShown)
	}
	if elision.Fetch != SessionsPath("profile-1") {
		t.Errorf("fetch = %q, want the paginated resource", elision.Fetch)
	}
}

func TestGapsCollapseToACountATotalAndTheLargestFew(t *testing.T) {
	view := Project(Resolve(projectable(), nil), 0)

	sampling := mustGet(t, view.Sampling, "sampling")
	if sampling.Gaps.Count != 400 {
		t.Errorf("gap count = %d, want 400", sampling.Gaps.Count)
	}
	if len(sampling.Gaps.Largest) != largestGapsShown {
		t.Errorf("largest = %d, want %d", len(sampling.Gaps.Largest), largestGapsShown)
	}
	if sampling.Gaps.Largest[0].DurationS < sampling.Gaps.Largest[1].DurationS {
		t.Error("the largest gaps are not ordered longest first")
	}
	if sampling.Gaps.TotalDurationS <= 0 {
		t.Error("no total gap duration was reported")
	}
	if sampling.Gaps.ByClassification[GapUnknown] != 400 {
		t.Errorf("by_classification = %v, want all 400 counted", sampling.Gaps.ByClassification)
	}
	findElision(t, view, FieldSamplingGaps)
}

func TestConstantRunsCollapseToACountAndTheLongest(t *testing.T) {
	view := Project(Resolve(projectable(), nil), 0)

	distribution := mustGet(t, view.Distribution, "distribution")
	if distribution.ConstantRuns.Count != 50 {
		t.Errorf("count = %d, want 50", distribution.ConstantRuns.Count)
	}
	if distribution.ConstantRuns.Longest == nil {
		t.Fatal("no longest run was reported")
	}
	if distribution.ConstantRuns.Longest.DurationS != 3000 {
		t.Errorf("longest = %vs, want the 50th run at 3000s", distribution.ConstantRuns.Longest.DurationS)
	}
}

func TestCounterResetsCollapseToACountAndTheFirstFew(t *testing.T) {
	view := Project(Resolve(projectable(), nil), 0)

	if view.ValueSemantics.CounterResets.Count != 20 {
		t.Errorf("count = %d, want 20", view.ValueSemantics.CounterResets.Count)
	}
	if len(view.ValueSemantics.CounterResets.First) != resetsShown {
		t.Errorf("shown = %d, want %d", len(view.ValueSemantics.CounterResets.First), resetsShown)
	}
	findElision(t, view, FieldCounterResets)
}

// Read modes and detector names are ODE's bookkeeping, not the model's.
func TestProvenanceIsDroppedFromTheProjection(t *testing.T) {
	encoded, err := json.Marshal(Project(Resolve(projectable(), nil), 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "provenance") {
		t.Error("the projection carries provenance")
	}
	if strings.Contains(string(encoded), "gap_v1") {
		t.Error("the projection carries detector names")
	}
}

// A not_computed field has to survive the projection intact: this is where an LLM
// would otherwise read absence as a negative finding.
func TestNotComputedFieldsSurviveTheProjection(t *testing.T) {
	view := Project(Resolve(projectable(), nil), 0)

	if view.TemporalStructure.Stationarity.IsComputed() {
		t.Fatal("stationarity became computed in the projection")
	}
	status := view.TemporalStructure.Stationarity.Status()
	if status.Reason != ReasonInsufficientSpan || status.Detail == "" {
		t.Errorf("status = %+v, want the reason and detail carried through", status)
	}

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"stationarity":null`) {
		t.Error("stationarity marshalled as null")
	}
}

// The developer's correction wins, and the field is marked confirmed so the model
// reads it as settled rather than as another estimate.
func TestADeveloperCorrectionReplacesTheComputedUnitInTheProjection(t *testing.T) {
	profile := projectable()
	resolved := Resolve(profile, []ProfileOverride{{
		OverrideID: "ovr-1", SeriesRef: profile.SeriesRef, ProfileID: profile.ProfileID,
		CreatedBy: "user-123", CreatedAt: fixtureStart,
		FieldPath: FieldUnit, Action: ActionCorrect, ComputedValue: "W", ConfirmedValue: "kW",
	}})

	view := Project(resolved, 0)
	if view.ValueSemantics.Unit != "kW" {
		t.Errorf("unit = %q, want the confirmed kW", view.ValueSemantics.Unit)
	}
	if view.ValueSemantics.UnitSource != UnitSource("developer") {
		t.Errorf("unit_source = %s, want developer", view.ValueSemantics.UnitSource)
	}
	confirmed := false
	for _, path := range view.ValueSemantics.Confirmed {
		if path == FieldUnit {
			confirmed = true
		}
	}
	if !confirmed {
		t.Errorf("confirmed = %v, want the unit named", view.ValueSemantics.Confirmed)
	}

	// The correction is also listed, so the model sees computed next to confirmed.
	if len(view.Overrides) != 1 || view.Overrides[0].ComputedValue != "W" {
		t.Errorf("overrides = %+v, want the computed W beside the confirmed kW", view.Overrides)
	}
}

func TestAConfirmedValueKindIsCertainInTheProjection(t *testing.T) {
	profile := projectable()
	resolved := Resolve(profile, []ProfileOverride{{
		OverrideID: "ovr-1", SeriesRef: profile.SeriesRef, CreatedBy: "user-123",
		FieldPath: FieldValueKind, Action: ActionCorrect, ConfirmedValue: string(KindInstantaneous),
	}})

	view := Project(resolved, 0)
	if kind := mustGet(t, view.ValueSemantics.Kind, "kind"); kind != KindInstantaneous {
		t.Errorf("kind = %s, want the corrected instantaneous", kind)
	}
	// D23: developer-confirmed is the one thing a detector's output can become
	// certain about.
	if confidence := mustGet(t, view.ValueSemantics.KindConfidence, "kind confidence"); confidence != Certain {
		t.Errorf("confidence = %s, want certain after a developer decision", confidence)
	}
}

// Under budget pressure, detail is dropped in a fixed order and each drop is
// recorded: the model must never have to guess which parts it is seeing.
func TestATightBudgetDropsDetailAndRecordsIt(t *testing.T) {
	full := Project(Resolve(projectable(), nil), 0)
	fullSize := len(mustMarshal(t, full))

	reduced := Project(Resolve(projectable(), nil), fullSize/8/bytesPerToken)
	reducedSize := len(mustMarshal(t, reduced))

	if reducedSize >= fullSize {
		t.Fatalf("the reduced view is %d bytes against %d full; nothing was dropped", reducedSize, fullSize)
	}
	if len(reduced.Elided) <= len(full.Elided) {
		t.Errorf("elisions went from %d to %d; every drop must be recorded",
			len(full.Elided), len(reduced.Elided))
	}
	// Period evidence goes first, before anything a reader needs to judge the
	// series.
	if reduced.TemporalStructure.PeriodEvidence.IsComputed() {
		t.Error("period evidence survived a very tight budget")
	}
	if !reduced.Coverage.IsComputed() {
		t.Error("coverage was dropped; it is never a candidate for elision")
	}
}

func TestAGenerousBudgetChangesNothing(t *testing.T) {
	full := Project(Resolve(projectable(), nil), 0)
	generous := Project(Resolve(projectable(), nil), 1_000_000)

	if len(generous.Elided) != len(full.Elided) {
		t.Errorf("elisions = %d, want the same %d as with no budget at all",
			len(generous.Elided), len(full.Elided))
	}
}

func findElision(t *testing.T, view LLMProfileView, field string) Elision {
	t.Helper()
	for _, elision := range view.Elided {
		if elision.Field == field {
			return elision
		}
	}
	t.Fatalf("no elision recorded for %s: %+v", field, view.Elided)
	return Elision{}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}
