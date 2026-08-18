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

package selection

import (
	"sort"
	"strings"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// reasonNotAnOutput explains a selected path that is not a series.
//
// The device repository indexes service *inputs* as well as outputs — a
// controlling function's target, a configurable — and they look exactly like a
// variable path in the answer. They are not readable series: the database columns
// come from the outputs. Reporting the path with a reason beats dropping it,
// because the developer who searched for it would otherwise conclude it does not
// exist.
const reasonNotAnOutput = "not a service output: the ontology indexes service inputs too, and an input is not a stored series"

// deviceTypeMatch accumulates what several criteria said about one device type.
//
// One resolution issues one request per criteria combination (see buildCriteria),
// and the same device type can come back from several of them — matched on its
// power function by one and on its aspect by another. Merging by device type,
// service and path is what turns those overlapping answers into one list without
// duplicates.
type deviceTypeMatch struct {
	id       string
	services map[string]models.Service
	options  map[string]drmodel.ServicePathOption
}

func newDeviceTypeMatch(id string) *deviceTypeMatch {
	return &deviceTypeMatch{
		id:       id,
		services: map[string]models.Service{},
		options:  map[string]drmodel.ServicePathOption{},
	}
}

func (m *deviceTypeMatch) merge(selectable drmodel.DeviceTypeSelectable) {
	for _, service := range selectable.Services {
		if _, known := m.services[service.Id]; !known {
			m.services[service.Id] = service
		}
	}
	for serviceID, options := range selectable.ServicePathOptions {
		for _, option := range options {
			if option.ServiceId == "" {
				option.ServiceId = serviceID
			}
			key := option.ServiceId + "|" + option.Path
			if _, known := m.options[key]; !known {
				m.options[key] = option
			}
		}
	}
}

// selectables resolves the merged path options into ODE's view of a variable:
// unit, declared range source and ontology completeness.
//
// The variables come from profiler.ServiceVariables rather than from the path
// options alone, because that walk is what decides which paths are addressable
// columns at all — the same enumeration a QuickProfile is built from. Using it
// here means a selectable and the candidate that later carries it agree about
// whether the variable can be read, instead of two walks drifting apart.
func (m *deviceTypeMatch) selectables(index *profiler.OntologyIndex) []Selectable {
	outputs := map[string]map[string]profiler.Variable{}
	for id, service := range m.services {
		byPath := map[string]profiler.Variable{}
		for _, variable := range profiler.ServiceVariables(service) {
			byPath[variable.Path] = variable
		}
		outputs[id] = byPath
	}

	out := make([]Selectable, 0, len(m.options))
	for _, option := range m.options {
		variable, found := outputs[option.ServiceId][option.Path]
		if !found {
			// Either an input path, or a device type whose services the query did not
			// return. Both are reported rather than silently dropped; the constructed
			// variable carries no unit_reference, so a unit that travels in the
			// message reads as absent here.
			//
			// The option's characteristic *is* taken here, unlike in
			// withOptionIdentity below. There the device type declares the variable and
			// declaring no characteristic is an answer; here there is no declaration to
			// respect at all, and the path is unqueryable either way — so nothing can
			// act on the unit, and naming it beats an unexplained blank.
			variable = profiler.Variable{
				ServiceID:        option.ServiceId,
				Path:             option.Path,
				Name:             lastSegment(option.Path),
				Type:             option.Type,
				Interaction:      option.Interaction,
				CharacteristicID: option.CharacteristicId,
				FunctionID:       option.FunctionId,
				AspectID:         option.AspectNode.Id,
				Void:             option.IsVoid,
				Queryable:        false,
				Reason:           reasonNotAnOutput,
			}
		}

		variable = withOptionIdentity(variable, option)

		semantics := profiler.ResolveUnits(variable, index, profiler.Provenance{})
		selectable := Selectable{
			DeviceTypeID:         m.id,
			ServiceID:            option.ServiceId,
			ServiceName:          m.services[option.ServiceId].Name,
			Path:                 option.Path,
			CharacteristicID:     semantics.CharacteristicID,
			Unit:                 semantics.Unit,
			UnitSource:           semantics.UnitSource,
			Interaction:          variable.Interaction,
			Type:                 variable.Type,
			FunctionID:           variable.FunctionID,
			AspectID:             variable.AspectID,
			AspectName:           option.AspectNode.Name,
			Queryable:            variable.Queryable,
			Reason:               variable.Reason,
			OntologyCompleteness: profiler.VariableCompleteness(variable, index),
		}
		out = append(out, selectable)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ServiceID != out[j].ServiceID {
			return out[i].ServiceID < out[j].ServiceID
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// withOptionIdentity fills the fields the repository matched this path on, where
// the walk over the device type left them empty.
//
// The two views cannot genuinely disagree: the repository builds its criteria
// index from the same content variables the walk reads. Reconciling them anyway
// matters because the alternative is a document that contradicts itself — a
// selectable reporting the aspect it was found by while its completeness claims no
// aspect is declared, which is exactly the kind of gap report nobody can act on.
//
// The characteristic is deliberately not filled in. It decides the unit and the
// declared range, and §5.4.11 makes the device type's own declaration the only
// authority for those: adopting one from elsewhere would report a unit that
// nothing declares.
func withOptionIdentity(variable profiler.Variable, option drmodel.ServicePathOption) profiler.Variable {
	if variable.FunctionID == "" {
		variable.FunctionID = option.FunctionId
	}
	if variable.AspectID == "" {
		variable.AspectID = option.AspectNode.Id
	}
	if variable.Type == "" {
		variable.Type = option.Type
	}
	if variable.Interaction == "" {
		variable.Interaction = option.Interaction
	}
	return variable
}

func lastSegment(path string) string {
	if index := strings.LastIndex(path, "."); index >= 0 {
		return path[index+1:]
	}
	return path
}

func sortedDeviceTypes(matched map[string]*deviceTypeMatch) []*deviceTypeMatch {
	out := make([]*deviceTypeMatch, 0, len(matched))
	for _, entry := range matched {
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// ontologyGaps aggregates the per-variable completeness of a resolution into
// §5.2's per-device-type report (D16).
//
// It aggregates rather than re-derives, which is the point: every gap here is a
// gap a QuickProfile reports too, in the same words. Grouping is by device type
// *and consequence*, so a device type missing units on two paths and aspects on a
// third produces two entries a caller can act on separately, rather than one row
// whose "missing" list belongs to no particular path.
func ontologyGaps(selectables []Selectable) []OntologyGap {
	type key struct {
		deviceTypeID string
		consequence  string
	}
	grouped := map[key]*OntologyGap{}

	for _, selectable := range selectables {
		if selectable.OntologyCompleteness.Status != profiler.CompletenessPartial {
			continue
		}
		consequence := selectable.OntologyCompleteness.Consequence
		if consequence == "" {
			// completeness only names a consequence for the cases it can describe;
			// stating the bare fact is better than an empty field, which reads as
			// "no consequence".
			consequence = "the device type declares no " +
				strings.Join(selectable.OntologyCompleteness.Missing, ", ")
		}

		id := key{deviceTypeID: selectable.DeviceTypeID, consequence: consequence}
		gap, known := grouped[id]
		if !known {
			gap = &OntologyGap{
				DeviceTypeID: selectable.DeviceTypeID,
				Consequence:  consequence,
				Missing:      []string{},
				Paths:        []string{},
			}
			grouped[id] = gap
		}
		gap.Missing = appendDistinct(gap.Missing, selectable.OntologyCompleteness.Missing...)
		gap.Paths = appendDistinct(gap.Paths, selectable.Path)
	}

	out := make([]OntologyGap, 0, len(grouped))
	for _, gap := range grouped {
		sort.Strings(gap.Missing)
		sort.Strings(gap.Paths)
		out = append(out, *gap)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DeviceTypeID != out[j].DeviceTypeID {
			return out[i].DeviceTypeID < out[j].DeviceTypeID
		}
		return out[i].Consequence < out[j].Consequence
	})
	return out
}

func appendDistinct(existing []string, values ...string) []string {
	for _, value := range values {
		found := false
		for _, present := range existing {
			if present == value {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, value)
		}
	}
	return existing
}
