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

package imports

import (
	"context"
	"errors"
	"strings"
	"testing"

	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
)

// exportService is newService with an import type and an instance behind the
// export, which is what the semantics half needs.
func exportService(t *testing.T, exports *fakeExports, types *fakeTypes) *Service {
	t.Helper()
	deps := Deps{
		Selectables: &fakeSelectables{},
		Instances: &fakeInstances{serve: []idmodel.Instance{{
			Id: testInstanceID, ImportTypeId: testTypeID, KafkaTopic: testTopic,
		}}},
		Exports: exports,
	}
	if types != nil {
		deps.Types = types
	}
	service, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

func TestAnExportDefinitionCarriesItsColumnsAndTheImportTypesSemantics(t *testing.T) {
	service := exportService(t,
		&fakeExports{serve: []Export{importExport()}, total: 1},
		&fakeTypes{importType: weatherType()})

	definition, err := service.ExportDefinition(context.Background(), testToken, "export-1")
	if err != nil {
		t.Fatalf("ExportDefinition: %v", err)
	}
	if definition.ExportID != "export-1" || definition.Source != FilterTypeImportExport {
		t.Errorf("definition = %+v, want the export and its import filter", definition)
	}
	if len(definition.Columns) != 2 {
		t.Fatalf("columns = %d, want one per export value", len(definition.Columns))
	}

	byColumn := map[string]ExportColumnInfo{}
	for _, column := range definition.Columns {
		byColumn[column.Column] = column
	}
	temp, found := byColumn["temp_c"]
	if !found {
		t.Fatalf("the temperature column is missing: %+v", definition.Columns)
	}
	// The column and the path are both carried, because neither substitutes for
	// the other: the query takes the column, and a developer knows the path.
	if temp.VariablePath != "value.temperature_2m" {
		t.Errorf("variable path = %q, want the message-relative path the export stores", temp.VariablePath)
	}
	if temp.Type != "float" {
		t.Errorf("type = %q, want the export worker's own vocabulary", temp.Type)
	}
	if station := byColumn["station"]; !station.Tag {
		t.Errorf("station = %+v, want it reported as a tag", station)
	}
	if len(definition.Notes) != 0 {
		t.Errorf("notes = %v, want none when everything resolved", definition.Notes)
	}
}

// The semantics are best-effort: an import type that cannot be read costs units,
// not the answer. What must not happen is silence — an export profiled without
// units has to say why.
func TestAnUnreadableImportTypeLeavesANoteRatherThanFailing(t *testing.T) {
	service := exportService(t,
		&fakeExports{serve: []Export{importExport()}, total: 1},
		&fakeTypes{err: errors.New("import-repository is down")})

	definition, err := service.ExportDefinition(context.Background(), testToken, "export-1")
	if err != nil {
		t.Fatalf("ExportDefinition: %v", err)
	}
	if len(definition.Columns) != 2 {
		t.Errorf("columns = %d, want the export's own columns regardless", len(definition.Columns))
	}
	for _, column := range definition.Columns {
		if column.CharacteristicID != nil {
			t.Errorf("column %s carries a characteristic, but the type could not be read", column.Column)
		}
	}
	if len(definition.Notes) == 0 || !strings.Contains(definition.Notes[0], "import-repository is down") {
		t.Errorf("notes = %v, want the platform's own error in one", definition.Notes)
	}
}

// A device export is read exactly the same way; it simply carries no semantics,
// and says so rather than looking like an import export with none.
func TestADeviceExportIsReadableAndSaysWhyItHasNoSemantics(t *testing.T) {
	service := exportService(t, &fakeExports{serve: []Export{{
		ID: "export-9", FilterType: "deviceId", Filter: "urn:infai:ses:device:x",
		Values: []ExportValue{{Name: "power", Path: "value.power", Type: "float"}},
	}}, total: 1}, &fakeTypes{importType: weatherType()})

	definition, err := service.ExportDefinition(context.Background(), testToken, "export-9")
	if err != nil {
		t.Fatalf("ExportDefinition: %v", err)
	}
	if len(definition.Columns) != 1 || definition.Columns[0].Column != "power" {
		t.Errorf("columns = %+v, want the device export's own column", definition.Columns)
	}
	if len(definition.Notes) == 0 || !strings.Contains(definition.Notes[0], "deviceId") {
		t.Errorf("notes = %v, want one naming the filter that has no semantics", definition.Notes)
	}
}

// The bound is the only thing that separates "this export does not exist" from
// "the listing did not reach it", and only the first means the id is wrong.
func TestAnExportBeyondTheListingBoundIsNotReportedAsAbsent(t *testing.T) {
	page := make([]Export, exportListLimit)
	for i := range page {
		page[i] = Export{ID: "other-" + string(rune('a'+i%26))}
	}
	service := exportService(t, &fakeExports{serve: page, total: exportListLimit + 5}, nil)

	_, err := service.ExportDefinition(context.Background(), testToken, "export-1")
	if err == nil {
		t.Fatal("expected an error for an export the listing did not reach")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "may exist and not have been reached") {
		t.Errorf("error = %v, want it to distinguish the bound from an absence", err)
	}
}

func TestAnAbsentExportIsRefusedAsAbsent(t *testing.T) {
	service := exportService(t, &fakeExports{serve: []Export{importExport()}, total: 1}, nil)

	_, err := service.ExportDefinition(context.Background(), testToken, "export-404")
	if err == nil || !strings.Contains(err.Error(), "no export") {
		t.Errorf("error = %v, want it to say the export is not visible", err)
	}
}

func TestNoServingConfiguredRefusesAnExportDefinition(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{}, nil)

	_, err := service.ExportDefinition(context.Background(), testToken, "export-1")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	// The refusal has to say why the column names matter, because "no
	// analytics-serving" does not obviously prevent reading a timescale table.
	if !strings.Contains(err.Error(), "column names") {
		t.Errorf("error = %v, want it to name what is missing", err)
	}
}

// A path that is not a payload leaf gets no semantics and is still a column: the
// export worker wrote it, so it exists in the table.
func TestAColumnWhoseVariablePathIsNotAPayloadLeafKeepsItsColumn(t *testing.T) {
	export := importExport()
	export.Values = append(export.Values, ExportValue{
		Name: "arrived_at", Path: "time", Type: "string",
	})
	service := exportService(t, &fakeExports{serve: []Export{export}, total: 1},
		&fakeTypes{importType: dsmodel.ImportType{Id: testTypeID, Output: weatherType().Output}})

	definition, err := service.ExportDefinition(context.Background(), testToken, "export-1")
	if err != nil {
		t.Fatalf("ExportDefinition: %v", err)
	}
	found := false
	for _, column := range definition.Columns {
		if column.Column == "arrived_at" {
			found = true
			if column.CharacteristicID != nil {
				t.Error("an envelope path must not be given a characteristic")
			}
		}
	}
	if !found {
		t.Errorf("columns = %+v, want the envelope-derived column reported too", definition.Columns)
	}
}
