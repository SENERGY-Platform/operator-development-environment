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
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
)

// The path is what timescale-wrapper wants as columns[].name, and it has to match
// the column timescale-tableworker created: the ContentVariable names joined with
// dots, starting at the output's root variable. A path derived any other way
// looks right and matches no column.
func TestVariablePathsJoinNamesFromTheOutputRoot(t *testing.T) {
	service := models.Service{
		Id: "urn:infai:ses:service:1", Name: "readings", Interaction: models.EVENT,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{
				Id: "a", Name: "value", Type: models.Structure,
				SubContentVariables: []models.ContentVariable{
					{
						Id: "b", Name: "reading", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{
							{Id: "c", Name: "power", Type: models.Float},
							{Id: "d", Name: "state", Type: models.String},
						},
					},
					{Id: "e", Name: "timestamp", Type: models.Integer},
				},
			},
		}},
	}

	got := map[string]models.Type{}
	for _, variable := range ServiceVariables(service) {
		got[variable.Path] = variable.Type
	}

	want := map[string]models.Type{
		"value.reading.power": models.Float,
		"value.reading.state": models.String,
		"value.timestamp":     models.Integer,
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Errorf("path %s has type %v, want %v", path, got[path], kind)
		}
	}
}

// A structure's children are walked in id order, matching the table worker, so
// two runs cannot disagree about a service's column set.
func TestStructureChildrenAreWalkedInIdOrder(t *testing.T) {
	service := models.Service{
		Id: "urn:infai:ses:service:1", Interaction: models.EVENT,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{
				Id: "root", Name: "value", Type: models.Structure,
				SubContentVariables: []models.ContentVariable{
					{Id: "z", Name: "zulu", Type: models.Float},
					{Id: "a", Name: "alpha", Type: models.Float},
				},
			},
		}},
	}

	variables := ServiceVariables(service)
	if len(variables) != 2 {
		t.Fatalf("variables = %+v, want two", variables)
	}
	if variables[0].Path != "value.alpha" || variables[1].Path != "value.zulu" {
		t.Errorf("order = %s, %s; want id order, which is alpha then zulu",
			variables[0].Path, variables[1].Path)
	}
}

// A list whose first member is "*" becomes one JSONB column. It exists, so it is
// reported, but it is not addressable as a scalar series.
func TestAWildcardListIsReportedAsUnqueryable(t *testing.T) {
	service := models.Service{
		Id: "urn:infai:ses:service:1", Interaction: models.EVENT,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{
				Id: "root", Name: "value", Type: models.Structure,
				SubContentVariables: []models.ContentVariable{{
					Id: "list", Name: "readings", Type: models.List,
					SubContentVariables: []models.ContentVariable{
						{Id: "star", Name: "*", Type: models.Float},
					},
				}},
			},
		}},
	}

	variables := ServiceVariables(service)
	if len(variables) != 1 {
		t.Fatalf("variables = %+v, want the list column itself", variables)
	}
	if variables[0].Queryable {
		t.Error("a JSONB list column was offered as a scalar series")
	}
	if variables[0].Reason == "" {
		t.Error("the variable carries no reason, so a developer cannot tell why it is missing")
	}
}

// A fixed-length list is one column per member, exactly as the table worker
// creates them.
func TestAFixedListBecomesOneColumnPerMember(t *testing.T) {
	service := models.Service{
		Id: "urn:infai:ses:service:1", Interaction: models.EVENT,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{
				Id: "list", Name: "phases", Type: models.List,
				SubContentVariables: []models.ContentVariable{
					{Id: "l1", Name: "l1", Type: models.Float},
					{Id: "l2", Name: "l2", Type: models.Float},
				},
			},
		}},
	}

	paths := []string{}
	for _, variable := range ServiceVariables(service) {
		paths = append(paths, variable.Path)
	}
	if len(paths) != 2 || paths[0] != "phases.l1" || paths[1] != "phases.l2" {
		t.Errorf("paths = %v, want phases.l1 and phases.l2", paths)
	}
}

// timescale-wrapper validates column names against a character class and answers
// a bare 400 outside it. Reporting the variable as unqueryable here beats
// offering a series whose read cannot succeed.
func TestAPathWithCharactersTheServerRejectsIsUnqueryable(t *testing.T) {
	service := models.Service{
		Id: "urn:infai:ses:service:1", Interaction: models.EVENT,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{
				Id: "root", Name: "value", Type: models.Structure,
				SubContentVariables: []models.ContentVariable{
					{Id: "ok", Name: "power", Type: models.Float},
					{Id: "space", Name: "active power", Type: models.Float},
				},
			},
		}},
	}

	for _, variable := range ServiceVariables(service) {
		switch variable.Path {
		case "value.power":
			if !variable.Queryable {
				t.Errorf("value.power is unqueryable: %s", variable.Reason)
			}
		case "value.active power":
			if variable.Queryable {
				t.Error("a path with a space was offered, and the server rejects it with 400")
			}
		default:
			t.Errorf("unexpected path %q", variable.Path)
		}
	}
}

// Detector 5: a request-only service is polled rather than streamed, so treating
// its output as a time series is a category error.
func TestARequestOnlyServiceIsNotOfferedAsASeries(t *testing.T) {
	service := models.Service{
		Id: "urn:infai:ses:service:1", Interaction: models.REQUEST,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{Id: "a", Name: "value", Type: models.Float},
		}},
	}

	variables := ServiceVariables(service)
	if len(variables) != 1 {
		t.Fatalf("variables = %+v, want one", variables)
	}
	if variables[0].Queryable {
		t.Error("a request-only service's output was offered as a series")
	}
	if variables[0].Streamed() {
		t.Error("Streamed() is true for interaction request")
	}
}

func TestEventAndRequestCountsAsStreamed(t *testing.T) {
	service := models.Service{
		Id: "urn:infai:ses:service:1", Interaction: models.EVENT_AND_REQUEST,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{Id: "a", Name: "value", Type: models.Float},
		}},
	}
	variables := ServiceVariables(service)
	if !variables[0].Queryable || !variables[0].Streamed() {
		t.Errorf("variable = %+v, want it treated as streamed", variables[0])
	}
}

func TestFindVariableResolvesASeriesRefBackToItsMetadata(t *testing.T) {
	device := meterDevice("urn:infai:ses:device:1", "urn:infai:ses:service:1")

	variable, found := FindVariable(*device.DeviceType, "urn:infai:ses:service:1", "value.total")
	if !found {
		t.Fatal("value.total was not found on the meter device type")
	}
	if variable.CharacteristicID != "ch-watthour" {
		t.Errorf("characteristic = %s, want ch-watthour", variable.CharacteristicID)
	}
}
