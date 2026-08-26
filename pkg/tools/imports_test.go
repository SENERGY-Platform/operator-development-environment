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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
)

const (
	testImportInstance = "urn:infai:ses:import:weather-1"
	testImportTopic    = "urn_infai_ses_import_weather-1"
	testImportType     = "urn:infai:ses:import-type:open-meteo"
)

type fakeImports struct {
	instances    []idmodel.Instance
	importType   dsmodel.ImportType
	history      imports.History
	listErr      error
	getErr       error
	typeErr      error
	listOpts     []imports.InstanceListOptions
	historyCalls int
}

func (f *fakeImports) List(_ context.Context, _ string, opts imports.InstanceListOptions) (imports.ListResult, error) {
	f.listOpts = append(f.listOpts, opts)
	if f.listErr != nil {
		return imports.ListResult{}, f.listErr
	}
	return imports.ListResult{
		Instances: f.instances, Total: int64(len(f.instances)), Limit: opts.Limit,
	}, nil
}

func (f *fakeImports) Get(_ context.Context, _ string, id string) (idmodel.Instance, error) {
	if f.getErr != nil {
		return idmodel.Instance{}, f.getErr
	}
	for _, instance := range f.instances {
		if instance.Id == id {
			return instance, nil
		}
	}
	return idmodel.Instance{}, errors.New("no such import instance")
}

func (f *fakeImports) GetType(context.Context, string, string) (dsmodel.ImportType, error) {
	if f.typeErr != nil {
		return dsmodel.ImportType{}, f.typeErr
	}
	return f.importType, nil
}

func (f *fakeImports) History(context.Context, string, string) imports.History {
	f.historyCalls++
	return f.history
}

func (f *fakeImports) Histories(_ context.Context, _ string, ids []string) map[string]imports.History {
	f.historyCalls++
	out := make(map[string]imports.History, len(ids))
	for _, id := range ids {
		out[id] = f.history
	}
	return out
}

func runningImport() idmodel.Instance {
	return idmodel.Instance{
		Id: testImportInstance, Name: "Leipzig weather", ImportTypeId: testImportType,
		KafkaTopic: testImportTopic, Status: &idmodel.InstanceStatus{Running: true},
	}
}

// The default output shape: the type describes the whole message, so import_id
// and time sit beside the payload.
func weatherImportType() dsmodel.ImportType {
	return dsmodel.ImportType{
		Id:   testImportType,
		Name: "Open-Meteo history",
		Configs: []dsmodel.ImportTypeConfig{
			{Name: "lat", Type: models.Float, DefaultValue: 51.34},
		},
		Output: dsmodel.ImportContentVariable{
			Name: "root", Type: models.Structure,
			SubContentVariables: []dsmodel.ImportContentVariable{
				{Name: "import_id", Type: models.String},
				{Name: "time", Type: models.String, CharacteristicId: "ch-timestamp"},
				{Name: "value", Type: models.Structure, SubContentVariables: []dsmodel.ImportContentVariable{
					{Name: "temperature_2m", Type: models.Float,
						CharacteristicId: "ch-celsius", FunctionId: "fn-temperature", AspectId: "kitchen"},
					{Name: "units", Type: models.Structure, SubContentVariables: []dsmodel.ImportContentVariable{
						{Name: "temperature_2m", Type: models.String},
					}},
				}},
			},
		},
	}
}

// callImportTool dispatches through the real dispatcher rather than the executor,
// so a confirmed tool goes through the confirmation the developer would give.
func callImportTool(t *testing.T, imp *fakeImports, name string, input any) map[string]any {
	t.Helper()
	result := dispatchImportTool(t, imp, name, input)
	if result.Outcome != OutcomeOK {
		t.Fatalf("%s: outcome %s: %+v", name, result.Outcome, result.Content)
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var answer map[string]any
	if err := json.Unmarshal(encoded, &answer); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	return answer
}

func dispatchImportTool(t *testing.T, imp *fakeImports, name string, input any) Result {
	t.Helper()
	definition, dispatcher := executorFor(t, Deps{Imports: imp, DeviceLimit: 10}, name)
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := Request{Token: "Bearer t", UserSub: "sub-1", SessionID: "sess-1", Tier: L0}
	result := dispatcher.Dispatch(context.Background(), request,
		Call{ID: "c1", Name: name, Input: raw})

	if definition.Confirm {
		if result.Outcome != OutcomeAwaitingConfirmation {
			return result
		}
		// Nothing is applied on the model's word alone, so the test agrees the way a
		// developer would rather than reaching past the gate.
		return dispatcher.Confirm(context.Background(), request, *result.Confirmation)
	}
	return result
}

func callImportToolExpectingRefusal(t *testing.T, imp *fakeImports, name string, input any) Result {
	t.Helper()
	result := dispatchImportTool(t, imp, name, input)
	if result.Outcome == OutcomeOK {
		t.Fatalf("%s: expected a refusal, got %+v", name, result.Content)
	}
	return result
}

// --- list_import_instances ---

// Three states, not a boolean: discovery never sees a status, and "stopped" is a
// claim ODE would not have established.
func TestListImportInstancesReportsRunningInThreeStates(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{
		runningImport(),
		{Id: "b", Name: "stopped one", ImportTypeId: testImportType,
			Status: &idmodel.InstanceStatus{Running: false}},
		{Id: "c", Name: "silent one", ImportTypeId: testImportType},
	}}

	answer := callImportTool(t, imp, "list_import_instances", map[string]any{})
	listed, ok := answer["instances"].([]any)
	if !ok || len(listed) != 3 {
		t.Fatalf("instances = %v", answer["instances"])
	}
	states := map[string]string{}
	for _, entry := range listed {
		row := entry.(map[string]any)
		states[row["instance_id"].(string)] = row["running"].(string)
	}
	if states[testImportInstance] != "running" {
		t.Errorf("running instance reported as %q", states[testImportInstance])
	}
	if states["b"] != "stopped" {
		t.Errorf("stopped instance reported as %q", states["b"])
	}
	if states["c"] != "unknown" {
		t.Errorf("statusless instance reported as %q, want unknown: telling a developer to "+
			"restart something that may be running fine is worse than saying nothing",
			states["c"])
	}
}

func TestListImportInstancesSaysHistoryWasNotChecked(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{runningImport()}}

	answer := callImportTool(t, imp, "list_import_instances", map[string]any{})
	row := answer["instances"].([]any)[0].(map[string]any)
	if _, present := row["history"]; present {
		t.Error("history was reported although it was not asked for; it is a call per instance")
	}
	if !strings.Contains(answer["note"].(string), "include_history") {
		t.Errorf("the note has to say the history question was skipped: %q", answer["note"])
	}
}

// One export listing for the page, not one per row.
func TestListImportInstancesAsksAboutHistoryOnce(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{
		runningImport(),
		{Id: "b", ImportTypeId: testImportType},
		{Id: "c", ImportTypeId: testImportType},
	}}

	answer := callImportTool(t, imp, "list_import_instances", map[string]any{"include_history": true})
	if len(answer["instances"].([]any)) != 3 {
		t.Fatalf("instances = %v", answer["instances"])
	}
	if imp.historyCalls != 1 {
		t.Errorf("the export listing was read %d times, want once for the page: "+
			"analytics-serving cannot filter by import", imp.historyCalls)
	}
	for _, entry := range answer["instances"].([]any) {
		if entry.(map[string]any)["history"] == nil {
			t.Error("a row is missing its history although it was asked for")
		}
	}
}

// A type filter makes `total` count something other than the page, so truncation
// has to be recorded rather than inferred from it.
func TestListImportInstancesDoesNotClaimAFalseTruncation(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{
		runningImport(),
		{Id: "b", ImportTypeId: "urn:infai:ses:import-type:other"},
	}}

	answer := callImportTool(t, imp, "list_import_instances",
		map[string]any{"import_type_ids": []string{testImportType}})

	if answer["truncated"] != false {
		t.Errorf("truncated = %v, want false: one of two instances was filtered out, not cut off",
			answer["truncated"])
	}
}

func TestListImportInstancesFiltersByTypeAndSaysItWasLocal(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{
		runningImport(),
		{Id: "b", ImportTypeId: "urn:infai:ses:import-type:other"},
	}}

	answer := callImportTool(t, imp, "list_import_instances",
		map[string]any{"import_type_ids": []string{testImportType}})

	listed := answer["instances"].([]any)
	if len(listed) != 1 {
		t.Fatalf("instances = %d, want only the matching type", len(listed))
	}
	if !strings.Contains(answer["note"].(string), "filtered by ODE") {
		t.Errorf("the note has to admit the filter was not applied upstream, because `total` "+
			"then counts something else: %q", answer["note"])
	}
	if len(imp.listOpts) != 1 || imp.listOpts[0].Limit != imports.MaxLimit {
		t.Errorf("a type filter has to read a wide listing, since import-deploy offers no such "+
			"filter: %+v", imp.listOpts)
	}
}

// --- get_import_type_metadata ---

func TestImportTypeMetadataSeparatesSeriesFromEnvelope(t *testing.T) {
	imp := &fakeImports{importType: weatherImportType()}

	answer := callImportTool(t, imp, "get_import_type_metadata",
		map[string]any{"import_type_id": testImportType})

	variables := answer["variables"].([]any)
	byPath := map[string]map[string]any{}
	for _, entry := range variables {
		row := entry.(map[string]any)
		byPath[row["path"].(string)] = row
	}

	temperature, found := byPath["root.value.temperature_2m"]
	if !found {
		t.Fatalf("the payload variable is missing: %v", byPath)
	}
	if temperature["is_series"] != true {
		t.Error("a payload variable has to read as a series")
	}
	if temperature["variable_path"] != "value.temperature_2m" {
		t.Errorf("variable_path = %v, want the addressable form", temperature["variable_path"])
	}

	for _, envelope := range []string{"root.import_id", "root.time"} {
		row, found := byPath[envelope]
		if !found {
			t.Errorf("%s was dropped; a model hunting for it needs to learn it was seen", envelope)
			continue
		}
		if row["is_series"] != false {
			t.Errorf("%s reads as a series, but it is the message envelope", envelope)
		}
		if row["reason"] == nil {
			t.Errorf("%s carries no reason for not being a series", envelope)
		}
		if _, present := row["variable_path"]; present {
			t.Errorf("%s was given an addressable path it cannot have", envelope)
		}
	}

	// A structure is a container; only its leaves are addressable.
	if _, present := byPath["root.value"]; present {
		t.Error("the payload structure itself was listed as a variable")
	}
	if nested, found := byPath["root.value.units.temperature_2m"]; !found {
		t.Error("a nested payload variable was dropped")
	} else if nested["variable_path"] != "value.units.temperature_2m" {
		t.Errorf("nested variable_path = %v, want the whole subtree preserved", nested["variable_path"])
	}
}

func TestImportTypeMetadataResolvesTheTypeOfAnInstance(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{runningImport()}, importType: weatherImportType()}

	answer := callImportTool(t, imp, "get_import_type_metadata",
		map[string]any{"instance_id": testImportInstance})

	if answer["import_type_id"] != testImportType {
		t.Errorf("import_type_id = %v, want the instance's type", answer["import_type_id"])
	}
	if answer["instance"] == nil {
		t.Error("the instance that was read is not reported back")
	}
}

func TestImportTypeMetadataNeedsAnId(t *testing.T) {
	result := callImportToolExpectingRefusal(t, &fakeImports{}, "get_import_type_metadata", map[string]any{})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %s, want invalid_input", result.Outcome)
	}
}

// --- propose_operator_input ---

func TestProposeOperatorInputEmitsTheFlowEngineShape(t *testing.T) {
	imp := &fakeImports{
		instances: []idmodel.Instance{runningImport()},
		history: imports.History{
			State: imports.HistoryExported, ExportID: "export-1",
			Columns: []imports.HistoryColumn{
				{VariablePath: "value.temperature_2m", Column: "temp_c"},
			},
		},
	}

	answer := callImportTool(t, imp, "propose_operator_input", map[string]any{
		"instance_id": testImportInstance,
		"rationale":   "the forecast needs an exogenous temperature",
		"bindings": []map[string]any{
			{"input_name": "temperature", "variable_path": "value.temperature_2m"},
		},
	})

	input := answer["node_input"].(map[string]any)
	if input["filterType"] != "ImportId" {
		t.Errorf("filterType = %v, want ImportId exactly", input["filterType"])
	}
	if input["filterIds"] != testImportInstance {
		t.Errorf("filterIds = %v, want the instance id", input["filterIds"])
	}
	if input["topicName"] != testImportTopic {
		t.Errorf("topicName = %v, want the instance's topic read from import-deploy rather than "+
			"derived", input["topicName"])
	}
	values := input["values"].([]any)
	if len(values) != 1 || values[0].(map[string]any)["path"] != "value.temperature_2m" {
		t.Errorf("values = %v", values)
	}
	if warnings := answer["warnings"].([]any); len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for a running, exported import", warnings)
	}
}

// The failure this tool exists to prevent: an envelope path deploys cleanly and
// receives nothing.
func TestProposeOperatorInputRefusesAnEnvelopePath(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{runningImport()}}

	result := callImportToolExpectingRefusal(t, imp, "propose_operator_input", map[string]any{
		"instance_id": testImportInstance,
		"rationale":   "because",
		"bindings":    []map[string]any{{"input_name": "t", "variable_path": "root.time"}},
	})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %s, want invalid_input", result.Outcome)
	}
}

func TestProposeOperatorInputWarnsAboutAStoppedImport(t *testing.T) {
	imp := &fakeImports{
		instances: []idmodel.Instance{{
			Id: testImportInstance, KafkaTopic: testImportTopic,
			ImportTypeId: testImportType, Status: &idmodel.InstanceStatus{Running: false},
		}},
		history: imports.History{State: imports.HistoryLiveOnly, Reason: "no export exists"},
	}

	answer := callImportTool(t, imp, "propose_operator_input", map[string]any{
		"instance_id": testImportInstance,
		"rationale":   "because",
		"bindings":    []map[string]any{{"input_name": "t", "variable_path": "value.temperature_2m"}},
	})

	warnings := answer["warnings"].([]any)
	if len(warnings) < 2 {
		t.Fatalf("warnings = %v, want the stopped container and the missing history", warnings)
	}
	joined := ""
	for _, warning := range warnings {
		joined += warning.(string) + "\n"
	}
	if !strings.Contains(joined, "not running") {
		t.Errorf("a stopped import has to be warned about: %s", joined)
	}
	if !strings.Contains(joined, "provide_historic_data") {
		t.Errorf("a live-only import has to say what the operator can still do: %s", joined)
	}
}

// An export carries only the variables it was created with, so a bound variable
// may have no stored history although the import does.
func TestProposeOperatorInputWarnsAboutAnUnexportedVariable(t *testing.T) {
	imp := &fakeImports{
		instances: []idmodel.Instance{runningImport()},
		history: imports.History{
			State: imports.HistoryExported, ExportID: "export-1",
			Columns: []imports.HistoryColumn{
				{VariablePath: "value.pressure_msl", Column: "pressure"},
			},
		},
	}

	answer := callImportTool(t, imp, "propose_operator_input", map[string]any{
		"instance_id": testImportInstance,
		"rationale":   "because",
		"bindings": []map[string]any{
			{"input_name": "temperature", "variable_path": "value.temperature_2m"},
		},
	})

	warnings := answer["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("no warning for a variable the export does not carry")
	}
	if !strings.Contains(warnings[0].(string), "value.temperature_2m") {
		t.Errorf("the warning should name the variable: %v", warnings[0])
	}
}

func TestProposeOperatorInputRequiresARationale(t *testing.T) {
	imp := &fakeImports{instances: []idmodel.Instance{runningImport()}}
	result := callImportToolExpectingRefusal(t, imp, "propose_operator_input", map[string]any{
		"instance_id": testImportInstance,
		"bindings":    []map[string]any{{"input_name": "t", "variable_path": "value.temperature_2m"}},
	})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %s, want a refusal: the developer confirms this and cannot confirm "+
			"what is not argued", result.Outcome)
	}
}

// The degradation the rest of ODE does.
func TestTheImportToolsStayUnavailableWithoutAnImportConfiguration(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, name := range []string{"list_import_instances", "get_import_type_metadata", "propose_operator_input"} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%s is not declared", name)
		}
		if definition.Implemented() {
			t.Errorf("%s has an executor without an import service", name)
		}
		if definition.Unavailable == "" {
			t.Errorf("%s does not say which configuration is missing", name)
		}
	}
}
