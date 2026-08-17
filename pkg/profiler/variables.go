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
	"regexp"
	"sort"

	"github.com/SENERGY-Platform/models/go/models"
)

// Variable is one addressable series discovered from a device type's service
// outputs — the leaf of D19's {device_id, service_id, variable_path}.
type Variable struct {
	ServiceID   string             `json:"service_id"`
	ServiceName string             `json:"service_name"`
	Interaction models.Interaction `json:"interaction"`

	// Path is the dotted ContentVariable name chain, and is what
	// timescale-wrapper wants as columns[].name.
	Path string      `json:"path"`
	Name string      `json:"name"`
	Type models.Type `json:"type"`

	CharacteristicID string `json:"characteristic_id"`
	UnitReference    string `json:"unit_reference,omitempty"`
	FunctionID       string `json:"function_id,omitempty"`
	AspectID         string `json:"aspect_id,omitempty"`
	Void             bool   `json:"void,omitempty"`

	// Queryable is false when the column exists in the database but cannot be
	// read as a scalar series. Reason says which case it is; reporting it beats
	// omitting the variable, because a developer looking for it needs to know it
	// was seen and why it is not on offer.
	Queryable bool   `json:"queryable"`
	Reason    string `json:"reason,omitempty"`
}

// Numeric says whether the declared type supports the statistical detectors.
// Booleans count: a binary sensor has a duty cycle and a distribution, and the
// value-semantics detector classifies it as binary rather than instantaneous.
func (v Variable) Numeric() bool {
	return v.Type == models.Float || v.Type == models.Integer || v.Type == models.Boolean
}

// Streamed says whether a series exists at all (§5.4.13 item 5). A service with
// interaction "request" is polled on demand rather than streamed to Kafka, so
// treating its output as a time series is a category error.
func (v Variable) Streamed() bool {
	return v.Interaction == models.EVENT || v.Interaction == models.EVENT_AND_REQUEST
}

// columnName is the pattern timescale-wrapper validates columns[].name against.
// A path outside it is rejected with a bare 400, so ODE checks it here and
// reports the variable as unqueryable instead of offering a series that cannot
// be read.
var columnName = regexp.MustCompile(`^[a-zA-Z0-9.\-_]+$`)

const (
	reasonListColumn  = "list_column: stored as JSONB, not addressable as a scalar series"
	reasonNotScalar   = "not a scalar leaf"
	reasonBadPath     = "path contains characters timescale-wrapper rejects in a column name"
	reasonNotStreamed = "service interaction is request-only, so nothing is streamed to the database"
)

// DeviceTypeVariables enumerates every addressable variable of a device type,
// in a stable order.
func DeviceTypeVariables(dt models.DeviceType) []Variable {
	out := []Variable{}
	services := append([]models.Service{}, dt.Services...)
	sort.SliceStable(services, func(i, j int) bool { return services[i].Id < services[j].Id })
	for _, service := range services {
		out = append(out, ServiceVariables(service)...)
	}
	return out
}

// ServiceVariables enumerates the variables of one service's outputs.
//
// The path construction mirrors timescale-tableworker's parseContentVariable,
// which is what actually creates the database columns: names are joined with
// dots starting at the output's root variable, structures recurse in id order,
// and a list whose first member is "*" becomes a single JSONB column rather than
// one column per element. Deriving the path any other way produces names that
// look right and match no column.
//
// Long names are hashed into a 62-character hex column name by the table
// worker, but timescale-wrapper applies the same hash to the name it is given,
// so ODE sends the plain path either way.
func ServiceVariables(service models.Service) []Variable {
	out := []Variable{}
	for _, output := range service.Outputs {
		out = append(out, walkContentVariable(service, output.ContentVariable, "")...)
	}
	return out
}

func walkContentVariable(service models.Service, cv models.ContentVariable, prefix string) []Variable {
	path := cv.Name
	if prefix != "" {
		path = prefix + "." + cv.Name
	}

	switch cv.Type {
	case models.String, models.Boolean, models.Float, models.Integer:
		return []Variable{newVariable(service, cv, path)}

	case models.Structure:
		subs := append([]models.ContentVariable{}, cv.SubContentVariables...)
		sort.SliceStable(subs, func(i, j int) bool { return subs[i].Id < subs[j].Id })
		out := []Variable{}
		for _, sub := range subs {
			out = append(out, walkContentVariable(service, sub, path)...)
		}
		return out

	case models.List:
		if len(cv.SubContentVariables) > 0 && cv.SubContentVariables[0].Name == "*" {
			v := newVariable(service, cv, path)
			v.Queryable = false
			v.Reason = reasonListColumn
			return []Variable{v}
		}
		subs := append([]models.ContentVariable{}, cv.SubContentVariables...)
		sort.SliceStable(subs, func(i, j int) bool { return subs[i].Id < subs[j].Id })
		out := []Variable{}
		for _, sub := range subs {
			out = append(out, walkContentVariable(service, sub, path)...)
		}
		return out

	default:
		// An unknown type is neither silently dropped nor offered: the device
		// type is incomplete in a way D16 asks ODE to report at runtime.
		v := newVariable(service, cv, path)
		v.Queryable = false
		v.Reason = reasonNotScalar
		return []Variable{v}
	}
}

func newVariable(service models.Service, cv models.ContentVariable, path string) Variable {
	v := Variable{
		ServiceID:        service.Id,
		ServiceName:      service.Name,
		Interaction:      service.Interaction,
		Path:             path,
		Name:             cv.Name,
		Type:             cv.Type,
		CharacteristicID: cv.CharacteristicId,
		UnitReference:    cv.UnitReference,
		FunctionID:       cv.FunctionId,
		AspectID:         cv.AspectId,
		Void:             cv.IsVoid,
		Queryable:        true,
	}
	switch {
	case !columnName.MatchString(path):
		v.Queryable = false
		v.Reason = reasonBadPath
	case !v.Streamed():
		v.Queryable = false
		v.Reason = reasonNotStreamed
	}
	return v
}

// FindVariable locates one variable of a device type by service and path, which
// is how a SeriesRef is resolved back to its ontology metadata.
func FindVariable(dt models.DeviceType, serviceID, path string) (Variable, bool) {
	for _, service := range dt.Services {
		if service.Id != serviceID {
			continue
		}
		for _, v := range ServiceVariables(service) {
			if v.Path == path {
				return v, true
			}
		}
	}
	return Variable{}, false
}
