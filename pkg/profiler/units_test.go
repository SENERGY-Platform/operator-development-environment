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

	"github.com/SENERGY-Platform/models/go/models"
)

// M1b acceptance: units resolve from characteristics.
func TestAUnitResolvesFromItsCharacteristic(t *testing.T) {
	prov := Provenance{}
	semantics := ResolveUnits(Variable{
		Path: "value.power", Type: models.Float,
		CharacteristicID: "ch-watt", FunctionID: "fn-power",
	}, powerOntology(), prov)

	if semantics.Unit != "W" {
		t.Errorf("unit = %q, want W", semantics.Unit)
	}
	if semantics.UnitSource != UnitFromCharacteristic {
		t.Errorf("unit_source = %s, want characteristic", semantics.UnitSource)
	}
	if semantics.CharacteristicID == nil || *semantics.CharacteristicID != "ch-watt" {
		t.Errorf("characteristic_id = %v, want ch-watt: it is canonical, the unit is derived", semantics.CharacteristicID)
	}
	if entry, recorded := prov[FieldUnit]; !recorded || entry.Source != SourceOntology {
		t.Errorf("provenance for the unit = %+v, want an ontology source", entry)
	}
	if entry := prov[FieldUnit]; entry.ReadMode != ReadNone {
		t.Errorf("read_mode = %s, want none: unit resolution reads no values", entry.ReadMode)
	}
}

func TestADeclaredRangeComesFromTheCharacteristic(t *testing.T) {
	semantics := ResolveUnits(Variable{
		Path: "value.power", Type: models.Float, CharacteristicID: "ch-watt",
	}, powerOntology(), Provenance{})

	min := mustGet(t, semantics.DeclaredRange.Min, "declared minimum")
	max := mustGet(t, semantics.DeclaredRange.Max, "declared maximum")
	if min != 0 || max != 10000 {
		t.Errorf("range = %v..%v, want 0..10000", min, max)
	}
}

// A characteristic that declares no bound must not report zero as one: a zero
// minimum on a signed quantity would flag every export reading as a violation.
func TestAnAbsentBoundIsNotComputedRatherThanZero(t *testing.T) {
	semantics := ResolveUnits(Variable{
		Path: "value.total", Type: models.Float, CharacteristicID: "ch-watthour",
	}, powerOntology(), Provenance{})

	if semantics.DeclaredRange.Max.IsComputed() {
		t.Fatal("a maximum was reported for a characteristic that declares none")
	}
	if status := semantics.DeclaredRange.Max.Status(); status.Status != "not_computed" {
		t.Errorf("status = %+v, want an explicit not_computed", status)
	}
}

// D29: never fabricate a characteristic id. Where none is declared, the field is
// null and the unit is unknown — a fabricated id would silently authorise a wrong
// server-side conversion.
func TestAVariableWithNoCharacteristicKeepsANullIdAndAnUnknownUnit(t *testing.T) {
	semantics := ResolveUnits(Variable{Path: "value.raw", Type: models.Float}, powerOntology(), Provenance{})

	if semantics.CharacteristicID != nil {
		t.Errorf("characteristic_id = %v, want null", *semantics.CharacteristicID)
	}
	if semantics.UnitSource != UnitUnknown {
		t.Errorf("unit_source = %s, want unknown", semantics.UnitSource)
	}
	if semantics.Unit != "" {
		t.Errorf("unit = %q, want empty rather than guessed", semantics.Unit)
	}
}

// unit_reference names a sibling variable that carries the unit in the message,
// so there is nothing to resolve statically — and saying so beats guessing,
// because reading it would be a value read this detector must not make.
func TestAUnitReferenceIsReportedAsSuchRatherThanResolved(t *testing.T) {
	prov := Provenance{}
	semantics := ResolveUnits(Variable{
		Path: "value.reading", Type: models.Float, UnitReference: "value.unit",
	}, powerOntology(), prov)

	if semantics.UnitSource != UnitFromUnitReference {
		t.Errorf("unit_source = %s, want unit_reference", semantics.UnitSource)
	}
	if prov[FieldUnit].Note == "" {
		t.Error("no provenance note explains that the unit travels in the message")
	}
}

// The one unit conflict the ontology can prove: the function declares a concept,
// the concept lists its characteristics, and a characteristic from outside that
// list means the two disagree about what the value is.
func TestACharacteristicOutsideTheFunctionsConceptIsAConflict(t *testing.T) {
	semantics := ResolveUnits(Variable{
		Path: "value.power", Type: models.Float,
		CharacteristicID: "ch-watthour", // energy
		FunctionID:       "fn-power",    // but the function measures power
	}, powerOntology(), Provenance{})

	if semantics.UnitSource != UnitConflict {
		t.Errorf("unit_source = %s, want conflict", semantics.UnitSource)
	}
}

func TestACharacteristicMissingFromTheOntologyIsRecordedWithoutTrustingItsUnit(t *testing.T) {
	semantics := ResolveUnits(Variable{
		Path: "value.power", Type: models.Float, CharacteristicID: "ch-does-not-exist",
	}, powerOntology(), Provenance{})

	if semantics.CharacteristicID == nil || *semantics.CharacteristicID != "ch-does-not-exist" {
		t.Errorf("characteristic_id = %v, want the declared id kept", semantics.CharacteristicID)
	}
	if semantics.UnitSource != UnitUnknown {
		t.Errorf("unit_source = %s, want unknown", semantics.UnitSource)
	}
}

// Conversions are a shortest-path walk, not a scan of direct edges: W→kW and
// kW→MW can exist without a W→MW edge, and the useful answer is that MW is
// reachable at distance 2 (§5.4.11: prefer the lowest distance).
func TestConversionsFollowTheGraphAndAreOrderedByDistance(t *testing.T) {
	semantics := ResolveUnits(Variable{
		Path: "value.power", Type: models.Float, CharacteristicID: "ch-watt", FunctionID: "fn-power",
	}, powerOntology(), Provenance{})

	if len(semantics.AvailableConversions) != 2 {
		t.Fatalf("conversions = %+v, want kW and MW", semantics.AvailableConversions)
	}
	first, second := semantics.AvailableConversions[0], semantics.AvailableConversions[1]
	if first.ToCharacteristicID != "ch-kilowatt" || first.Distance != 1 || first.ToUnit != "kW" {
		t.Errorf("first conversion = %+v, want kW at distance 1", first)
	}
	if second.ToCharacteristicID != "ch-megawatt" || second.Distance != 2 {
		t.Errorf("second conversion = %+v, want MW at distance 2 through kW", second)
	}
}

func TestAConceptWithoutConversionsOffersNone(t *testing.T) {
	semantics := ResolveUnits(Variable{
		Path: "value.total", Type: models.Float, CharacteristicID: "ch-watthour", FunctionID: "fn-energy",
	}, powerOntology(), Provenance{})

	if len(semantics.AvailableConversions) != 0 {
		t.Errorf("conversions = %+v, want none", semantics.AvailableConversions)
	}
}

// The detector-fed fields are not_computed until the raw pass runs, rather than
// zero-valued: the whole point of D24 is that a reader can tell the difference.
func TestTheDetectorFieldsStartOutNotComputed(t *testing.T) {
	semantics := ResolveUnits(Variable{
		Path: "value.power", Type: models.Float, CharacteristicID: "ch-watt",
	}, powerOntology(), Provenance{})

	for name, value := range map[string]bool{
		"kind":                  semantics.Kind.IsComputed(),
		"kind_confidence":       semantics.KindConfidence.IsComputed(),
		"kind_evidence":         semantics.KindEvidence.IsComputed(),
		"range_violation_ratio": semantics.RangeViolationRatio.IsComputed(),
		"counter_resets":        semantics.CounterResets.IsComputed(),
	} {
		if value {
			t.Errorf("%s is computed before any read happened", name)
		}
	}
}

// The characteristic-not-found branches must produce an empty list too. The
// service-level fixture only exercises the resolved path, so this covers the
// three that fall through — and it is where the null actually came from.
func TestEveryUnitResolutionPathMarshalsConversionsAsAList(t *testing.T) {
	for name, variable := range map[string]Variable{
		"no characteristic":      {Path: "value.raw", Type: models.Float},
		"unknown characteristic": {Path: "value.raw", Type: models.Float, CharacteristicID: "ch-absent"},
		"unit reference":         {Path: "value.raw", Type: models.Float, UnitReference: "value.unit"},
		"resolved without conversions": {
			Path: "value.total", Type: models.Float, CharacteristicID: "ch-watthour", FunctionID: "fn-energy",
		},
	} {
		semantics := ResolveUnits(variable, powerOntology(), Provenance{})
		encoded, err := json.Marshal(semantics)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(string(encoded), `"available_conversions":null`) {
			t.Errorf("%s: available_conversions marshalled as null, want []", name)
		}
		if semantics.AvailableConversions == nil {
			t.Errorf("%s: available_conversions is a nil slice", name)
		}
	}
}
