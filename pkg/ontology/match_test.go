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

package ontology

import (
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
)

// matchSnapshot is a small ontology with the traps that matter: two functions
// sharing a word, an aspect whose name is an abbreviation, and a function whose
// only label is a camel-case platform name.
func matchSnapshot() *Snapshot {
	return &Snapshot{
		MeasuringFunctions: []models.Function{
			{Id: "fn-generation", Name: "getPowerGenerationFunction", DisplayName: "Power Generation",
				ConceptId: "concept-power", RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
			{Id: "fn-consumption", Name: "getPowerConsumptionFunction", DisplayName: "Power Consumption",
				ConceptId: "concept-power", RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
			{Id: "fn-temperature", Name: "getTemperatureFunction", DisplayName: "Temperature",
				ConceptId: "concept-temperature", RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
			{Id: "fn-humidity", Name: "getHumiditySensorsFunction",
				RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
		},
		ControllingFunctions: []models.Function{
			{Id: "fn-set-temperature", Name: "setTemperatureFunction", DisplayName: "Set Temperature",
				RdfType: models.SES_ONTOLOGY_CONTROLLING_FUNCTION},
		},
		AspectNodes: []models.AspectNode{
			node("pv", "PV System", ""),
			node("inverter", "Inverter", "pv"),
			node("kitchen", "Kitchen", ""),
		},
		DeviceClasses: []models.DeviceClass{
			{Id: "dc-meter", Name: "Energy Meter"},
			{Id: "dc-lamp", Name: "Lamp"},
		},
	}
}

func functionIDs(matches []FunctionMatch) []string {
	out := []string{}
	for _, m := range matches {
		out = append(out, m.Id)
	}
	return out
}

func aspectIDs(matches []AspectMatch) []string {
	out := []string{}
	for _, m := range matches {
		out = append(out, m.Id)
	}
	return out
}

// The example SPEC §5.2 is written around: "forecast PV generation for this
// site" resolves to a measuring function and an aspect subtree, and to nothing
// else. Power Consumption sharing the word "power" must not come along — it is
// the mismatch that would silently profile the wrong series.
func TestMatchIntentResolvesTheSpecExample(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "forecast PV generation for this site"})

	if got := functionIDs(match.Functions); len(got) != 1 || got[0] != "fn-generation" {
		t.Errorf("functions = %v, want only fn-generation", got)
	}
	if got := aspectIDs(match.Aspects); len(got) != 1 || got[0] != "pv" {
		t.Errorf("aspects = %v, want only pv", got)
	}
	if len(match.Functions) > 0 {
		matched := match.Functions[0].Matched
		if matched.Basis != BasisDisplayName {
			t.Errorf("basis = %q, want %q", matched.Basis, BasisDisplayName)
		}
		if len(matched.Terms) != 1 || matched.Terms[0] != "generation" {
			t.Errorf("matched terms = %v, want [generation] — the caller's own word", matched.Terms)
		}
		if matched.Score <= 0 || matched.Score > 1 {
			t.Errorf("score = %v, want it inside (0, 1]", matched.Score)
		}
	}
}

// An aspect match always covers the subtree, because the device repository
// expands an aspect criterion to the node plus its descendants. Reporting it as
// a field keeps the caller from expanding it a second time, which would AND a
// parent with its child and match nothing.
func TestMatchedAspectsAlwaysIncludeDescendants(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "kitchen"})
	if len(match.Aspects) != 1 {
		t.Fatalf("aspects = %v, want one", aspectIDs(match.Aspects))
	}
	if !match.Aspects[0].DescendantsIncluded {
		t.Error("descendants_included = false, want true")
	}
}

// Platform function names arrive as getPowerConsumptionFunction. Without
// camel-case splitting every one of them is a single unsearchable token, and an
// ontology whose display names are unset would resolve nothing at all.
func TestMatchIntentSplitsCamelCasePlatformNames(t *testing.T) {
	snap := matchSnapshot()
	// Strip the display names, leaving only the platform spelling.
	for i := range snap.MeasuringFunctions {
		snap.MeasuringFunctions[i].DisplayName = ""
	}

	match := MatchIntent(snap, Intent{Text: "power consumption"})
	if got := functionIDs(match.Functions); len(got) != 1 || got[0] != "fn-consumption" {
		t.Fatalf("functions = %v, want only fn-consumption", got)
	}
	if basis := match.Functions[0].Matched.Basis; basis != BasisName {
		t.Errorf("basis = %q, want %q", basis, BasisName)
	}
}

// A plural in either the intent or the ontology must not break the match. The
// fold is crude and applied to both sides, which is what makes it safe.
func TestMatchIntentFoldsPlurals(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "humidity sensor readings"})
	if got := functionIDs(match.Functions); len(got) != 1 || got[0] != "fn-humidity" {
		t.Errorf("functions = %v, want fn-humidity from a singular intent", got)
	}
}

// The function a developer named in full outranks the one that merely shares a
// word with it. This is the ranking the whole matcher exists to produce: both are
// offered, in the order that puts the likely one first.
func TestAFullyNamedFunctionOutranksAPartialMatch(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "power consumption"})
	ids := functionIDs(match.Functions)
	if len(ids) != 2 {
		t.Fatalf("functions = %v, want both power functions offered", ids)
	}
	if ids[0] != "fn-consumption" {
		t.Errorf("first = %v, want fn-consumption: the intent names both of its words", ids)
	}
	if match.Functions[0].Matched.Score != 1 {
		t.Errorf("score = %v, want 1 for a fully named term", match.Functions[0].Matched.Score)
	}
	if match.Functions[1].Matched.Score != 0.5 {
		t.Errorf("score = %v, want 0.5 for one word of two", match.Functions[1].Matched.Score)
	}
}

// The honest half of a matcher with no thesaurus. "Photovoltaik" is a real word
// for this domain and is not in the ontology, so it is reported as unmatched
// rather than stretched onto the nearest term.
func TestUnmatchedTermsAreReported(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "Photovoltaik Erzeugung"})
	if len(match.Functions) != 0 || len(match.Aspects) != 0 {
		t.Fatalf("expected no match, got functions %v aspects %v",
			functionIDs(match.Functions), aspectIDs(match.Aspects))
	}
	if len(match.UnmatchedTerms) != 2 {
		t.Errorf("unmatched = %v, want both terms", match.UnmatchedTerms)
	}
	if len(match.Terms) != 2 {
		t.Errorf("terms = %v, want the intent as the matcher read it", match.Terms)
	}
}

// A term the matcher used is not unmatched, and one it did not is — the two
// lists together are what let a caller see which half of an intent landed.
func TestUnmatchedTermsExcludeTheOnesAMatchUsed(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "generation forecast"})
	if len(match.Functions) == 0 {
		t.Fatal("expected the generation function to match")
	}
	for _, term := range match.UnmatchedTerms {
		if term == "generation" {
			t.Error("generation is reported unmatched although a match used it")
		}
	}
	found := false
	for _, term := range match.UnmatchedTerms {
		if term == "forecast" {
			found = true
		}
	}
	if !found {
		t.Errorf("unmatched = %v, want the task word forecast among them", match.UnmatchedTerms)
	}
}

// Controlling functions are off by default: a series is something measured, and
// offering "Set Temperature" as a data source would be a category error.
func TestControllingFunctionsAreOptedInto(t *testing.T) {
	snap := matchSnapshot()

	closed := MatchIntent(snap, Intent{Text: "temperature"})
	for _, m := range closed.Functions {
		if m.Id == "fn-set-temperature" {
			t.Error("a controlling function matched without being asked for")
		}
	}

	open := MatchIntent(snap, Intent{Text: "temperature", IncludeControlling: true})
	found := false
	for _, m := range open.Functions {
		if m.Id == "fn-set-temperature" {
			found = true
		}
	}
	if !found {
		t.Errorf("functions = %v, want the controlling one included", functionIDs(open.Functions))
	}
}

func TestMatchLimitTruncatesTheWeakestMatches(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{
		Text: "power consumption and generation", Limit: 1,
	})
	if len(match.Functions) != 1 {
		t.Fatalf("functions = %v, want one at limit 1", functionIDs(match.Functions))
	}
	if match.Functions[0].Id != "fn-consumption" {
		t.Errorf("kept %s, want the strongest match", match.Functions[0].Id)
	}
}

// A negative MinScore is how a caller asks to see what was nearly matched, which
// is the difference between "no such thing" and "you used another word for it".
//
// "humidity" is one of the three words of getHumiditySensorsFunction, so it
// scores 0.33 and the default floor drops it.
func TestANegativeMinScoreKeepsWeakMatches(t *testing.T) {
	strict := MatchIntent(matchSnapshot(), Intent{Text: "humidity"})
	if len(strict.Functions) != 0 {
		t.Fatalf("functions = %v, want none above the default floor", functionIDs(strict.Functions))
	}

	loose := MatchIntent(matchSnapshot(), Intent{Text: "humidity", MinScore: -1})
	if got := functionIDs(loose.Functions); len(got) != 1 || got[0] != "fn-humidity" {
		t.Errorf("functions = %v, want fn-humidity admitted by the lower floor", got)
	}
}

func TestAnEmptyIntentMatchesNothing(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "   "})
	if len(match.Functions) != 0 || len(match.Aspects) != 0 || len(match.DeviceClasses) != 0 {
		t.Error("an empty intent produced matches")
	}
	// Empty rather than nil: a caller iterating the answer should not have to
	// special-case JSON null (the same reasoning as D24's shape).
	if match.Functions == nil || match.Terms == nil || match.UnmatchedTerms == nil {
		t.Error("empty lists arrived as nil")
	}
}

func TestMatchIntentIsStableAcrossCalls(t *testing.T) {
	snap := matchSnapshot()
	first := MatchIntent(snap, Intent{Text: "power"})
	for range 5 {
		again := MatchIntent(snap, Intent{Text: "power"})
		if len(again.Functions) != len(first.Functions) {
			t.Fatalf("match count changed between calls: %d then %d",
				len(first.Functions), len(again.Functions))
		}
		for i := range again.Functions {
			if again.Functions[i].Id != first.Functions[i].Id {
				t.Fatalf("order changed between calls: %v then %v",
					functionIDs(first.Functions), functionIDs(again.Functions))
			}
		}
	}
}

// Ties break on name, so two equally scoring matches are ordered the same way
// every time rather than by map iteration.
func TestEqualScoresOrderByName(t *testing.T) {
	match := MatchIntent(matchSnapshot(), Intent{Text: "power"})
	ids := functionIDs(match.Functions)
	if len(ids) != 2 {
		t.Fatalf("functions = %v, want the two power functions", ids)
	}
	if ids[0] != "fn-consumption" || ids[1] != "fn-generation" {
		t.Errorf("order = %v, want Power Consumption before Power Generation", ids)
	}
}

// --- explicit ids ---

func TestExplicitFunctionsResolveWithoutMatching(t *testing.T) {
	matches, unknown := ExplicitFunctions(matchSnapshot(), []string{"fn-temperature"})
	if len(unknown) != 0 {
		t.Fatalf("unknown = %v, want none", unknown)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %v, want one", matches)
	}
	if matches[0].Matched.Basis != BasisExplicitID || matches[0].Matched.Score != 1 {
		t.Errorf("evidence = %+v, want an explicit id at score 1", matches[0].Matched)
	}
	if matches[0].Name != "Temperature" || matches[0].ConceptId != "concept-temperature" {
		t.Errorf("match = %+v, want the snapshot's own name and concept", matches[0])
	}
}

// An id the snapshot does not know is reported and kept. The snapshot can be
// older than the platform, so refusing it would turn a stale cache into a failed
// request; dropping it silently would answer a different question than the one
// asked.
func TestExplicitIdsUnknownToTheSnapshotAreReportedAndKept(t *testing.T) {
	matches, unknown := ExplicitFunctions(matchSnapshot(), []string{"fn-brand-new"})
	if len(unknown) != 1 || unknown[0] != "fn-brand-new" {
		t.Errorf("unknown = %v, want the id reported", unknown)
	}
	if len(matches) != 1 || matches[0].Id != "fn-brand-new" {
		t.Errorf("matches = %v, want the id kept for the query", matches)
	}
}

func TestExplicitAspectsAndDeviceClassesResolve(t *testing.T) {
	aspects, unknown := ExplicitAspects(matchSnapshot(), []string{"inverter"})
	if len(unknown) != 0 || len(aspects) != 1 || aspects[0].Name != "Inverter" {
		t.Errorf("aspects = %v, unknown = %v", aspects, unknown)
	}
	if !aspects[0].DescendantsIncluded {
		t.Error("an explicit aspect must still report that its subtree is included")
	}

	classes, unknown := ExplicitDeviceClasses(matchSnapshot(), []string{"dc-lamp"})
	if len(unknown) != 0 || len(classes) != 1 || classes[0].Name != "Lamp" {
		t.Errorf("classes = %v, unknown = %v", classes, unknown)
	}
}

func TestMatchIntentToleratesAMissingSnapshot(t *testing.T) {
	if match := MatchIntent(nil, Intent{Text: "power"}); len(match.Functions) != 0 {
		t.Error("a nil snapshot produced matches")
	}
	if _, unknown := ExplicitFunctions(nil, []string{"fn-power"}); len(unknown) != 1 {
		t.Errorf("unknown = %v, want the id reported when there is no snapshot", unknown)
	}
}
