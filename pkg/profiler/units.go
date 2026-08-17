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
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// ResolveUnits fills the ontology-derived half of ValueSemantics. It reads no
// series — detector 3 of §5.4.13 is pure ontology — so it runs identically for a
// QuickProfile and for a full one, and still answers when the read failed.
//
// Resolution order is §5.4.11: Characteristic.display_unit, then
// ContentVariable.unit_reference, then unknown. characteristic_id stays
// canonical and is never fabricated: a made-up id would silently authorise a
// wrong server-side conversion, which is worse in every case than an honest
// null.
func ResolveUnits(v Variable, index *OntologyIndex, prov Provenance) ValueSemantics {
	semantics := ValueSemantics{
		UnitSource: UnitUnknown,
		// An empty list rather than nil, which would marshal as JSON null and make
		// every consumer special-case "no conversions" against "not a list".
		AvailableConversions: []Conversion{},
		DeclaredRange: DeclaredRange{
			Min: Uncomputable[float64](ReasonOutOfScope, "no characteristic declares a minimum"),
			Max: Uncomputable[float64](ReasonOutOfScope, "no characteristic declares a maximum"),
		},
		// Detectors that need the raw pass fill these in; until then they are
		// explicitly not computed rather than zero.
		Kind:                Uncomputable[ValueKind](ReasonOutOfScope, "value kind needs the raw pass"),
		KindConfidence:      Uncomputable[Confidence](ReasonOutOfScope, "value kind needs the raw pass"),
		KindEvidence:        Uncomputable[KindEvidence](ReasonOutOfScope, "value kind needs the raw pass"),
		RangeViolationRatio: Uncomputable[float64](ReasonOutOfScope, "range violations need the raw pass"),
		CounterResets:       Uncomputable[[]time.Time](ReasonOutOfScope, "counter resets need the raw pass"),
	}

	characteristic, found := index.Characteristic(v.CharacteristicID)
	switch {
	case v.CharacteristicID != "" && found:
		id := v.CharacteristicID
		semantics.CharacteristicID = &id
		semantics.Unit = characteristic.DisplayUnit
		semantics.UnitSource = UnitFromCharacteristic
		semantics.DeclaredRange = declaredRange(characteristic.MinValue, characteristic.MaxValue)
		semantics.AvailableConversions = index.Conversions(id)
		prov.FromOntology(FieldUnit, "characteristic:"+id)
		prov.FromOntology(FieldCharacteristic, "characteristic:"+id)
		prov.FromOntology(FieldDeclaredRange, "characteristic:"+id)

		if characteristic.DisplayUnit == "" {
			// The characteristic exists but names no unit. The id is still
			// canonical and conversions still work, so this is a gap in the
			// display string alone.
			semantics.UnitSource = UnitFromCharacteristic
			prov.Set(FieldUnit, ProvenanceEntry{
				ReadMode: ReadNone, Source: SourceOntology, Ref: "characteristic:" + id,
				Note: "characteristic declares no display_unit",
			})
		}

		if conflict, detail := unitConflict(v, id, index); conflict {
			semantics.UnitSource = UnitConflict
			prov.Set(FieldUnit, ProvenanceEntry{
				ReadMode: ReadNone, Source: SourceOntology, Ref: "characteristic:" + id, Note: detail,
			})
		}

	case v.CharacteristicID != "" && !found:
		// The device type references a characteristic the ontology does not
		// have. Recording the id without trusting its unit keeps D16's runtime
		// completeness reporting honest.
		id := v.CharacteristicID
		semantics.CharacteristicID = &id
		semantics.UnitSource = UnitUnknown
		prov.Set(FieldUnit, ProvenanceEntry{
			ReadMode: ReadNone, Source: SourceOntology, Ref: "characteristic:" + id,
			Note: "characteristic id is not present in the ontology snapshot",
		})

	case v.UnitReference != "":
		// unit_reference names a sibling variable that carries the unit in the
		// message itself, so there is no unit to resolve statically. Saying so
		// is more useful than guessing: the unit is per message, and reading it
		// is a value read that this detector is specified not to make.
		semantics.UnitSource = UnitFromUnitReference
		prov.FromOntology(FieldUnit, "unit_reference:"+v.UnitReference)
		prov.Set(FieldUnit, ProvenanceEntry{
			ReadMode: ReadNone, Source: SourceOntology, Ref: "unit_reference:" + v.UnitReference,
			Note: "the unit travels in the message; it cannot be resolved without a value read",
		})

	default:
		semantics.UnitSource = UnitUnknown
		prov.FromOntology(FieldUnit, "")
		prov.Set(FieldUnit, ProvenanceEntry{
			ReadMode: ReadNone, Source: SourceOntology,
			Note: "the device type declares neither a characteristic nor a unit reference",
		})
	}

	return semantics
}

// unitConflict reports a variable whose characteristic does not belong to the
// concept its function declares.
//
// This is the one unit conflict the ontology can prove: the function says which
// concept is being measured, the concept lists its characteristics, and a
// characteristic from outside that list means the two disagree about what the
// value is. §5.4.11 offers magnitude inference as the other route, but that
// needs values, and inferring "W rather than kW" from magnitude alone asserts
// something the data cannot support. Where nothing resolves, the unit stays
// unknown and the developer confirms it (§5.10).
func unitConflict(v Variable, characteristicID string, index *OntologyIndex) (bool, string) {
	if v.FunctionID == "" {
		return false, ""
	}
	function, ok := index.Function(v.FunctionID)
	if !ok || function.ConceptId == "" {
		return false, ""
	}
	concept, ok := index.Concept(characteristicID)
	if !ok {
		return false, ""
	}
	if concept.Id == function.ConceptId {
		return false, ""
	}
	return true, fmt.Sprintf(
		"function %s measures concept %s, but characteristic %s belongs to concept %s",
		v.FunctionID, function.ConceptId, characteristicID, concept.Id)
}

// declaredRange reads a characteristic's bounds, which are untyped in the model
// and frequently absent.
func declaredRange(min, max any) DeclaredRange {
	out := DeclaredRange{
		Min: Uncomputable[float64](ReasonOutOfScope, "characteristic declares no minimum"),
		Max: Uncomputable[float64](ReasonOutOfScope, "characteristic declares no maximum"),
	}
	if min != nil {
		if f, ok := timeseries.ToFloat(min); ok {
			out.Min = Computed(f)
		} else {
			out.Min = Uncomputablef[float64](ReasonWrongKind, "minimum %v is not numeric", min)
		}
	}
	if max != nil {
		if f, ok := timeseries.ToFloat(max); ok {
			out.Max = Computed(f)
		} else {
			out.Max = Uncomputablef[float64](ReasonWrongKind, "maximum %v is not numeric", max)
		}
	}
	return out
}
