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

package charts

import (
	"fmt"
	"sort"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// Unit is what a chart axis is drawn against, and it is deliberately more than a
// string.
//
// §5.4.11 (D29): the characteristic id is canonical and authoritative, the unit
// string is derived and advisory. A unit string cannot be converted; a
// characteristic can. So the axis carries both, plus where the string came from,
// plus what the developer has said about it — because the one thing the pane must
// never do is print "W" with the same authority whether the ontology said so or a
// heuristic guessed.
type Unit struct {
	Unit       string              `json:"unit"`
	UnitSource profiler.UnitSource `json:"unit_source"`
	// CharacteristicID is nil where none resolved. Never fabricated: a made-up id
	// would silently authorise a wrong server-side conversion (D29).
	CharacteristicID     *string               `json:"characteristic_id"`
	AvailableConversions []profiler.Conversion `json:"available_conversions"`

	// Confirmable says the developer's confirmation would add something: the unit
	// was inferred, is unknown, is in conflict, or travels in the message. Where
	// the ontology answered it outright there is nothing to confirm, which is
	// §5.10's own point — the ontology reduces how often confirmation is needed.
	Confirmable bool `json:"confirmable"`
	// Confirmed and the two fields under it are the override overlay made visible.
	// ComputedUnit is what the resolver said before the developer spoke, so the
	// pane can show "W → kW, confirmed by …" rather than only the answer (§5.4.3).
	Confirmed    bool   `json:"confirmed"`
	ComputedUnit string `json:"computed_unit,omitempty"`
	ConfirmedBy  string `json:"confirmed_by,omitempty"`
	// Note explains an unusual state — a conflict, a rejection, a unit that only
	// exists per message.
	Note string `json:"note,omitempty"`
}

// resolveUnit reads the ontology half of a variable's value semantics.
//
// It calls the profiler's own resolver rather than restating the resolution order
// of §5.4.11: the axis of a chart and the unit field of a profile must not be able
// to disagree about the same variable, and two implementations would eventually
// do exactly that. The provenance sidecar it fills is discarded here — a chart
// axis reports its source through UnitSource — but it is what carries the reason
// in the profile, so it is worth knowing the two share a code path.
func resolveUnit(variable profiler.Variable, index *profiler.OntologyIndex) Unit {
	semantics := profiler.ResolveUnits(variable, index, profiler.Provenance{})

	unit := Unit{
		Unit:                 semantics.Unit,
		UnitSource:           semantics.UnitSource,
		CharacteristicID:     semantics.CharacteristicID,
		AvailableConversions: semantics.AvailableConversions,
		ComputedUnit:         semantics.Unit,
	}
	if unit.AvailableConversions == nil {
		unit.AvailableConversions = []profiler.Conversion{}
	}

	switch semantics.UnitSource {
	case profiler.UnitFromCharacteristic:
		// The ontology answered. Confirmation adds nothing unless the string is
		// missing, in which case the id is canonical and only the display unit is a
		// gap.
		unit.Confirmable = semantics.Unit == ""
		if unit.Confirmable {
			unit.Note = "the characteristic resolves but declares no display unit"
		}
	case profiler.UnitConflict:
		unit.Confirmable = true
		unit.Note = "the function's concept and the variable's characteristic disagree about what is being measured"
	case profiler.UnitFromUnitReference:
		unit.Confirmable = true
		unit.Note = "the unit travels in the message rather than in the device type, so it cannot be resolved without reading a value"
	default:
		unit.Confirmable = true
		unit.Note = "the device type declares neither a characteristic nor a unit reference"
	}
	return unit
}

// applyConfirmations merges the override overlay onto a resolved unit (§5.4.3).
//
// The overlay is append-only and merged at read time only, so this never writes:
// it walks the overrides in order and lets a later one supersede an earlier one,
// which is how a developer changes their mind without destroying the record that
// they did.
//
// A correction to the *characteristic* is treated as the stronger statement of the
// two, and deliberately so. Correcting the unit string fixes the label; correcting
// the characteristic fixes the canonical key, and that is what makes a server-side
// conversion possible at all (D29). So a corrected characteristic re-resolves the
// unit and the conversion list from the ontology.
func applyConfirmations(unit Unit, overrides []profiler.ProfileOverride, index *profiler.OntologyIndex) Unit {
	applicable := make([]profiler.ProfileOverride, 0, len(overrides))
	for _, override := range overrides {
		if override.FieldPath == profiler.FieldUnit || override.FieldPath == profiler.FieldCharacteristic {
			applicable = append(applicable, override)
		}
	}
	sort.SliceStable(applicable, func(i, j int) bool {
		if applicable[i].CreatedAt.Equal(applicable[j].CreatedAt) {
			return applicable[i].OverrideID < applicable[j].OverrideID
		}
		return applicable[i].CreatedAt.Before(applicable[j].CreatedAt)
	})

	for _, override := range applicable {
		unit.Confirmed = true
		unit.ConfirmedBy = override.CreatedBy
		switch {
		case override.Action == profiler.ActionReject:
			unit.Unit = ""
			unit.UnitSource = profiler.UnitUnknown
			unit.Note = "the developer rejected the resolved unit"
			if override.FieldPath == profiler.FieldCharacteristic {
				unit.CharacteristicID = nil
				unit.AvailableConversions = []profiler.Conversion{}
			}

		case override.Action == profiler.ActionCorrect && override.FieldPath == profiler.FieldUnit:
			if corrected, ok := override.ConfirmedValue.(string); ok && corrected != "" {
				unit.Unit = corrected
				unit.Note = "the unit is the developer's, not the ontology's"
			}

		case override.Action == profiler.ActionCorrect && override.FieldPath == profiler.FieldCharacteristic:
			if corrected, ok := override.ConfirmedValue.(string); ok && corrected != "" {
				id := corrected
				unit.CharacteristicID = &id
				unit.AvailableConversions = index.Conversions(id)
				if unit.AvailableConversions == nil {
					unit.AvailableConversions = []profiler.Conversion{}
				}
				if characteristic, found := index.Characteristic(id); found {
					if characteristic.DisplayUnit != "" {
						unit.Unit = characteristic.DisplayUnit
					}
					unit.UnitSource = profiler.UnitFromCharacteristic
					unit.Note = "the characteristic is the developer's; the unit and the conversions follow from it"
				} else {
					unit.Note = "the developer named a characteristic the ontology snapshot does not contain, " +
						"so no conversion can be offered against it"
				}
			}

		default:
			// A confirmation agrees with what was resolved. The value does not
			// change; its standing does — D23 reserves `certain` for
			// ontology-derived and developer-confirmed values.
			unit.Note = "confirmed by the developer"
		}
	}
	if unit.Confirmed {
		// There is nothing left to ask once a decision is on record. A developer who
		// wants to revise one appends another; the pane offers that separately.
		unit.Confirmable = false
	}
	return unit
}

// convertTarget validates a convert: transform and returns what the query needs.
//
// Two refusals, and both matter more than the convenience of letting the read
// through. Without a source characteristic there is nothing to convert *from*,
// and inventing one is the failure D29 names explicitly. And a target outside the
// concept's own conversion graph cannot be reached: the server would evaluate a
// formula that does not exist, or none at all, and the axis would then be
// labelled with a unit the values are not in.
func convertTarget(unit Unit, target string, index *profiler.OntologyIndex) (source, concept string, err error) {
	if unit.CharacteristicID == nil || *unit.CharacteristicID == "" {
		return "", "", fmt.Errorf(
			"%w: convert: needs a source characteristic and this variable has none — "+
				"its unit came from %s. Confirm the characteristic first, which is what makes conversion possible",
			ErrInvalidSpec, unit.UnitSource)
	}
	source = *unit.CharacteristicID
	if source == target {
		return "", "", fmt.Errorf("%w: convert: target %q is the variable's own characteristic", ErrInvalidSpec, target)
	}

	reachable := false
	for _, conversion := range unit.AvailableConversions {
		if conversion.ToCharacteristicID == target {
			reachable = true
			break
		}
	}
	if !reachable {
		return "", "", fmt.Errorf(
			"%w: characteristic %q is not reachable from %q through the concept's conversion graph. "+
				"available_conversions on the series lists what is",
			ErrInvalidSpec, target, source)
	}

	// timescale-wrapper rejects a column carrying targetCharacteristicId without a
	// conceptId or a criteria (QueriesRequestElementColumn.Valid), so the concept
	// is not optional here even though the conversion graph is keyed by
	// characteristic.
	found, ok := index.Concept(source)
	if !ok || found.Id == "" {
		return "", "", fmt.Errorf(
			"%w: characteristic %q belongs to no concept in the ontology snapshot, and the platform "+
				"needs the concept to evaluate a conversion",
			ErrInvalidSpec, source)
	}
	return source, found.Id, nil
}

// targetUnit names the unit a conversion lands in, for the axis label.
func targetUnit(unit Unit, target string) Unit {
	converted := unit
	converted.CharacteristicID = &target
	converted.Note = "converted server-side from " + unit.Unit
	for _, conversion := range unit.AvailableConversions {
		if conversion.ToCharacteristicID == target {
			converted.Unit = conversion.ToUnit
			break
		}
	}
	// A conversion is only offered against a resolved characteristic, so the target
	// is as canonical as the source was. The source of the *string* is still the
	// ontology's characteristic.
	converted.UnitSource = profiler.UnitFromCharacteristic
	converted.Confirmable = false
	return converted
}
