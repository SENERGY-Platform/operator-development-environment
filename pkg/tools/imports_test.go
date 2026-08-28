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
	"fmt"
	"strings"
	"testing"

	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
)

const (
	testSessionID      = "session-imports"
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

	// The catalogue half: what a type listing answers, the total upstream reported
	// for it, and what the listing was asked — the criteria are half of what this
	// tool has to get right.
	types        []dsmodel.ImportType
	typeTotal    int64
	typeListErr  error
	typeListOpts []imports.TypeListOptions

	// The write half. created/createdExport are what the fake would return; the
	// *Req fields record what it was asked for, because half of what these tools
	// have to get right is the request they build rather than the answer they give.
	created         imports.CreatedInstance
	createErr       error
	createReq       *imports.CreateInstanceRequest
	deletedInstance []string
	deleteErr       error

	createdExport   imports.CreatedExport
	createExportErr error
	createExportReq *imports.CreateExportRequest
	deletedExports  []string
	deleteExportErr error

	exportDefaults imports.ExportDefaults
}

func (f *fakeImports) CreateInstance(_ context.Context, _ string, req imports.CreateInstanceRequest) (imports.CreatedInstance, error) {
	f.createReq = &req
	if f.createErr != nil {
		return imports.CreatedInstance{}, f.createErr
	}
	return f.created, nil
}

func (f *fakeImports) DeleteInstance(_ context.Context, _ string, id string) error {
	f.deletedInstance = append(f.deletedInstance, id)
	return f.deleteErr
}

func (f *fakeImports) CreateExport(_ context.Context, _ string, req imports.CreateExportRequest) (imports.CreatedExport, error) {
	f.createExportReq = &req
	if f.createExportErr != nil {
		return imports.CreatedExport{}, f.createExportErr
	}
	return f.createdExport, nil
}

func (f *fakeImports) DeleteExport(_ context.Context, _ string, id string) error {
	f.deletedExports = append(f.deletedExports, id)
	return f.deleteExportErr
}

func (f *fakeImports) ExportDefaults() imports.ExportDefaults { return f.exportDefaults }

// fakeCreations is the session creation log the two delete tools check.
type fakeCreations struct {
	recorded  map[string][]Creation
	recordErr error
	readErr   error
}

func newFakeCreations(entries ...Creation) *fakeCreations {
	log := &fakeCreations{recorded: map[string][]Creation{}}
	for _, entry := range entries {
		log.recorded[testSessionID] = append(log.recorded[testSessionID], entry)
	}
	return log
}

func (f *fakeCreations) RecordCreation(_ context.Context, sessionID string, created Creation) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded[sessionID] = append(f.recorded[sessionID], created)
	return nil
}

func (f *fakeCreations) Creations(_ context.Context, sessionID string) ([]Creation, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.recorded[sessionID], nil
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

func (f *fakeImports) ListTypes(_ context.Context, _ string, opts imports.TypeListOptions) (imports.TypeListResult, error) {
	f.typeListOpts = append(f.typeListOpts, opts)
	if f.typeListErr != nil {
		return imports.TypeListResult{}, f.typeListErr
	}
	return imports.TypeListResult{Types: f.types, Total: f.typeTotal, Limit: opts.Limit}, nil
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
	return dispatchImportToolWith(t, Deps{Imports: imp, DeviceLimit: 10}, name, input)
}

// dispatchImportToolWith is the same for a tool that needs more than the import
// service — list_import_types reads the ontology to expand an aspect subtree,
// which import-repository will not do for it.
func dispatchImportToolWith(t *testing.T, deps Deps, name string, input any) Result {
	t.Helper()
	definition, dispatcher := executorFor(t, deps, name)
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

// --- the four tools that change the platform ---

// Every one of them is held. This is the property the whole write surface rests
// on, so it is asserted directly rather than inferred from the table test.
func TestTheWriteToolsAreAllHeldForConfirmation(t *testing.T) {
	registry, err := NewSurface(Deps{Imports: &fakeImports{}, Creations: newFakeCreations()})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, name := range []string{
		"create_import_instance", "create_export", "delete_import_instance", "delete_export",
	} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%s is not declared", name)
		}
		if !definition.Confirm {
			t.Errorf("%s is not confirmed, so the model could change the platform on its own word", name)
		}
		if definition.MinTier != L0 {
			t.Errorf("%s min tier = %s, want L0: it exposes no values", name, definition.MinTier)
		}
	}
}

// The gate, dispatched rather than called: a held call that the developer never
// agreed to must not have reached the platform.
func TestAWriteToolDoesNothingUntilTheDeveloperAgrees(t *testing.T) {
	imp := &fakeImports{}
	registry, err := NewSurface(Deps{Imports: imp, Creations: newFakeCreations(), DeviceLimit: 10})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	input, _ := json.Marshal(map[string]any{
		"import_type_id": testImportType, "name": "x", "rationale": "because",
	})
	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", UserSub: "sub-1", SessionID: testSessionID, Tier: L0},
		Call{ID: "c1", Name: "create_import_instance", Input: input})

	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Fatalf("outcome = %s, want the call held", result.Outcome)
	}
	if imp.createReq != nil {
		t.Error("import-deploy was called before the developer agreed")
	}
}

func TestCreateImportInstanceRecordsWhatItCreated(t *testing.T) {
	imp := &fakeImports{created: imports.CreatedInstance{
		Instance:  runningImport(),
		Defaulted: []string{"lat"},
	}}
	log := newFakeCreations()

	answer := callWriteTool(t, imp, log, "create_import_instance", map[string]any{
		"import_type_id": testImportType,
		"name":           "Leipzig weather",
		"rationale":      "the platform carries no outdoor temperature for this site",
		"configs":        []any{map[string]any{"name": "station", "value": "leipzig"}},
	})
	if answer["instance"] == nil {
		t.Fatalf("answer carries no instance: %v", answer)
	}
	if imp.createReq == nil || imp.createReq.ImportTypeID != testImportType {
		t.Fatalf("create request = %+v", imp.createReq)
	}
	if len(imp.createReq.Configs) != 1 || imp.createReq.Configs[0].Name != "station" {
		t.Errorf("configs = %+v, want the one the model set", imp.createReq.Configs)
	}

	recorded := log.recorded[testSessionID]
	if len(recorded) != 1 {
		t.Fatalf("recorded %d creations, want one", len(recorded))
	}
	if recorded[0].Kind != CreatedImportInstance || recorded[0].ID != testImportInstance {
		t.Errorf("recorded %+v, want the created instance", recorded[0])
	}
	// Without the record the object exists and nothing in chat can remove it, so
	// the answer has to say so rather than the developer discovering it later.
	if recorded[0].At.IsZero() {
		t.Error("the creation was recorded without a time")
	}
}

// The record is what makes the undo possible, and failing to write it must not be
// reported as the creation having failed — the import exists either way.
func TestCreateImportInstanceWarnsWhenItCouldNotBeRecorded(t *testing.T) {
	imp := &fakeImports{created: imports.CreatedInstance{Instance: runningImport()}}
	log := newFakeCreations()
	log.recordErr = errors.New("postgres is down")

	answer := callWriteTool(t, imp, log, "create_import_instance", map[string]any{
		"import_type_id": testImportType, "name": "x", "rationale": "because",
	})
	warnings := strings.Join(stringsOf(answer["warnings"]), " ")
	if !strings.Contains(warnings, "cannot be removed from chat") {
		t.Errorf("warnings = %v, want the failed record reported", answer["warnings"])
	}
	if !strings.Contains(warnings, testImportInstance) {
		t.Error("the warning does not carry the id the developer needs to remove it by hand")
	}
}

func TestCreateExportReportsColumnsAndDerivedFields(t *testing.T) {
	imp := &fakeImports{createdExport: imports.CreatedExport{
		Export: imports.Export{
			ID: "export-1", Name: "Leipzig weather history",
			Values: []imports.ExportValue{{Name: "temperature", Path: "value.temperature_2m", Type: "float"}},
		},
		Derived: map[string]string{"export_database_id": "configuration"},
	}}
	log := newFakeCreations()

	answer := callWriteTool(t, imp, log, "create_export", map[string]any{
		"instance_id": testImportInstance,
		"name":        "Leipzig weather history",
		"rationale":   "the operator needs a year of history and the import is live only",
		"values":      []any{map[string]any{"variable_path": "value.temperature_2m", "column": "temperature"}},
	})
	if answer["export_id"] != "export-1" {
		t.Fatalf("export_id = %v", answer["export_id"])
	}

	columns, ok := answer["columns"].([]any)
	if !ok || len(columns) != 1 {
		t.Fatalf("columns = %v", answer["columns"])
	}
	column := columns[0].(map[string]any)
	// Reported as the export stores it, which is how everything else the model has
	// seen about this import is keyed.
	if column["variable_path"] != "value.temperature_2m" || column["column"] != "temperature" {
		t.Errorf("column = %v, want the path as the model knows it beside the column name", column)
	}
	if answer["derived"] == nil {
		t.Error("the answer does not say where the deployment-specific fields came from")
	}
	if imp.createExportReq.TimePath != "" || imp.createExportReq.TimestampFormat != "" {
		t.Errorf("request = %+v, want both left to the deployment when the model named neither",
			imp.createExportReq)
	}
	if recorded := log.recorded[testSessionID]; len(recorded) != 1 || recorded[0].Kind != CreatedExport {
		t.Errorf("recorded %+v, want the created export", recorded)
	}
}

// An import that backfills the past needs its own time path, and the format that
// parses it belongs with it. Both reach the request rather than being dropped
// between the tool schema and the service.
func TestCreateExportPassesThePerExportTimeFields(t *testing.T) {
	imp := &fakeImports{createdExport: imports.CreatedExport{
		Export: imports.Export{ID: "export-1", Name: "Open-Meteo history"},
	}}

	callWriteTool(t, imp, newFakeCreations(), "create_export", map[string]any{
		"instance_id":      testImportInstance,
		"name":             "Open-Meteo history",
		"rationale":        "the backfill replays years in minutes, so the envelope time is useless",
		"time_path":        "value.weather_time",
		"timestamp_format": "%Y-%m-%dT%H:%M",
		"values":           []any{map[string]any{"variable_path": "value.temperature_2m"}},
	})
	if imp.createExportReq.TimePath != "value.weather_time" {
		t.Errorf("time path = %q, want the requested one", imp.createExportReq.TimePath)
	}
	if imp.createExportReq.TimestampFormat != "%Y-%m-%dT%H:%M" {
		t.Errorf("timestamp format = %q, want the requested one", imp.createExportReq.TimestampFormat)
	}
}

// The rule that lets the delete tools exist at all: an id this session did not
// create is refused, and nothing upstream is called.
func TestDeleteRefusesAnythingThisSessionDidNotCreate(t *testing.T) {
	imp := &fakeImports{}
	log := newFakeCreations(Creation{
		Kind: CreatedImportInstance, ID: testImportInstance, Name: "Leipzig weather",
	})

	result := dispatchWriteTool(t, imp, log, "delete_import_instance", map[string]any{
		"instance_id": "urn:infai:ses:import:someone-elses",
		"rationale":   "tidying up",
	})
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %s, want a refusal", result.Outcome)
	}
	if len(imp.deletedInstance) != 0 {
		t.Fatal("import-deploy was asked to delete an instance this session did not create")
	}
	// The refusal names what the session did create, so the model corrects itself
	// rather than trying the same id again.
	failure, _ := json.Marshal(result.Content)
	if !strings.Contains(string(failure), testImportInstance) {
		t.Errorf("refusal = %s, want it to say what this session did create", failure)
	}
}

func TestDeleteRemovesWhatThisSessionCreated(t *testing.T) {
	imp := &fakeImports{}
	log := newFakeCreations(
		Creation{Kind: CreatedImportInstance, ID: testImportInstance, Name: "Leipzig weather"},
		Creation{Kind: CreatedExport, ID: "export-1", Name: "history"},
	)

	answer := callWriteTool(t, imp, log, "delete_import_instance", map[string]any{
		"instance_id": testImportInstance, "rationale": "the wrong station",
	})
	if answer["deleted"] == nil {
		t.Fatalf("answer = %v", answer)
	}
	if len(imp.deletedInstance) != 1 || imp.deletedInstance[0] != testImportInstance {
		t.Errorf("deleted %v, want the instance this session created", imp.deletedInstance)
	}

	answer = callWriteTool(t, imp, log, "delete_export", map[string]any{
		"export_id": "export-1", "rationale": "the wrong columns",
	})
	if answer["deleted"] == nil {
		t.Fatalf("answer = %v", answer)
	}
	if len(imp.deletedExports) != 1 || imp.deletedExports[0] != "export-1" {
		t.Errorf("deleted %v, want the export this session created", imp.deletedExports)
	}
}

// An export id is not an instance id. Deleting by the wrong kind would remove
// something the session did create, and not the thing that was named.
func TestDeleteDoesNotCrossTheTwoKinds(t *testing.T) {
	imp := &fakeImports{}
	log := newFakeCreations(Creation{Kind: CreatedExport, ID: "export-1", Name: "history"})

	result := dispatchWriteTool(t, imp, log, "delete_import_instance", map[string]any{
		"instance_id": "export-1", "rationale": "confusing the two",
	})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %s, want a refusal", result.Outcome)
	}
	if len(imp.deletedInstance) != 0 {
		t.Error("an export id was passed to import-deploy as an instance")
	}
}

// "Could not check" must not become "went ahead".
func TestDeleteRefusesWhenTheCreationLogCannotBeRead(t *testing.T) {
	imp := &fakeImports{}
	log := newFakeCreations()
	log.readErr = errors.New("postgres is down")

	result := dispatchWriteTool(t, imp, log, "delete_export", map[string]any{
		"export_id": "export-1", "rationale": "cleanup",
	})
	if result.Outcome == OutcomeOK {
		t.Fatal("an export was deleted although what this session created could not be read")
	}
	if len(imp.deletedExports) != 0 {
		t.Error("analytics-serving was called anyway")
	}
}

// Every one of the four is confirmed, and a confirmation the developer cannot
// argue about is worth nothing — so all four require the argument.
func TestTheWriteToolsRequireARationale(t *testing.T) {
	imp := &fakeImports{created: imports.CreatedInstance{Instance: runningImport()}}
	log := newFakeCreations(
		Creation{Kind: CreatedImportInstance, ID: testImportInstance},
		Creation{Kind: CreatedExport, ID: "export-1"},
	)

	for name, input := range map[string]map[string]any{
		"create_import_instance": {"import_type_id": testImportType, "name": "x"},
		"create_export": {"instance_id": testImportInstance, "name": "x",
			"values": []any{map[string]any{"variable_path": "value.temperature_2m"}}},
		"delete_import_instance": {"instance_id": testImportInstance},
		"delete_export":          {"export_id": "export-1"},
	} {
		result := dispatchWriteTool(t, imp, log, name, input)
		if result.Outcome != OutcomeInvalidInput {
			t.Errorf("%s: outcome = %s, want a refusal without a rationale", name, result.Outcome)
		}
	}
}

// A refusal from pkg/imports is the model's mistake, not the platform's, and the
// outcome has to say which so the audit trail can tell them apart.
func TestAnInvalidWriteRequestIsRecordedAsInvalidInput(t *testing.T) {
	imp := &fakeImports{createErr: fmt.Errorf("%w: config value of wrong type",
		imports.ErrInvalidRequest)}

	result := dispatchWriteTool(t, imp, newFakeCreations(), "create_import_instance", map[string]any{
		"import_type_id": testImportType, "name": "x", "rationale": "because",
	})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %s, want invalid_input for a request pkg/imports refused",
			result.Outcome)
	}
}

// Without a chat store there is no memory of what a session created, so there is
// nothing for a delete to check — and the tool is not advertised at all.
func TestTheDeleteToolsNeedTheCreationLog(t *testing.T) {
	registry, err := NewSurface(Deps{Imports: &fakeImports{}})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, name := range []string{"delete_import_instance", "delete_export"} {
		definition, _ := registry.Lookup(name)
		if definition.Implemented() {
			t.Errorf("%s has an executor with no record of what the session created", name)
		}
	}
	for _, name := range []string{"create_import_instance", "create_export"} {
		definition, _ := registry.Lookup(name)
		if !definition.Implemented() {
			t.Errorf("%s should still be callable: it creates, and only the undo needs the log", name)
		}
	}
}

func callWriteTool(t *testing.T, imp *fakeImports, log *fakeCreations, name string, input any) map[string]any {
	t.Helper()
	result := dispatchWriteTool(t, imp, log, name, input)
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

// dispatchWriteTool goes through Dispatch and then Confirm, the way a developer
// agreeing does. Calling the executor directly would test everything except the
// gate that makes these tools acceptable.
func dispatchWriteTool(t *testing.T, imp *fakeImports, log *fakeCreations, name string, input any) Result {
	t.Helper()
	definition, dispatcher := executorFor(t,
		Deps{Imports: imp, Creations: log, DeviceLimit: 10}, name)
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := Request{Token: "Bearer t", UserSub: "sub-1", SessionID: testSessionID, Tier: L0}
	result := dispatcher.Dispatch(context.Background(), request,
		Call{ID: "c1", Name: name, Input: raw})
	if !definition.Confirm {
		t.Fatalf("%s is not confirmed", name)
	}
	if result.Outcome != OutcomeAwaitingConfirmation {
		return result
	}
	return dispatcher.Confirm(context.Background(), request, *result.Confirmation)
}

func stringsOf(value any) []string {
	entries, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if text, ok := entry.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// --- list_import_types ---

// catalogueType is an import type described against `inverter`, a child of `pv`,
// with one config that has a default and one that does not.
func catalogueType(id, name string) dsmodel.ImportType {
	return dsmodel.ImportType{
		Id: id, Name: name, Description: "hourly weather from Open-Meteo",
		Configs: []dsmodel.ImportTypeConfig{
			{Name: "lat", Type: models.Float, DefaultValue: 51.34},
			{Name: "station", Type: models.String},
		},
		Output: dsmodel.ImportContentVariable{
			Name: "root", Type: models.Structure,
			SubContentVariables: []dsmodel.ImportContentVariable{
				{Name: "import_id", Type: models.String},
				{Name: "time", Type: models.String},
				{Name: "value", Type: models.Structure, SubContentVariables: []dsmodel.ImportContentVariable{
					{Name: "temperature_2m", Type: models.Float, CharacteristicId: "ch-celsius",
						FunctionId: "fn-temperature", AspectId: "inverter"},
					{Name: "station_name", Type: models.String},
				}},
			},
		},
	}
}

func catalogueSnapshot() *ontology.Snapshot {
	return &ontology.Snapshot{AspectNodes: []models.AspectNode{
		{Id: "pv", Name: "PV System", RootId: "pv"},
		{Id: "inverter", Name: "Inverter", ParentId: "pv", RootId: "pv"},
	}}
}

func callCatalogue(t *testing.T, imp *fakeImports, input any) map[string]any {
	t.Helper()
	deps := Deps{Imports: imp, DeviceLimit: 10, Ontology: &fakeOntology{snapshot: catalogueSnapshot()}}
	result := dispatchImportToolWith(t, deps, "list_import_types", input)
	if result.Outcome != OutcomeOK {
		t.Fatalf("list_import_types: outcome %s: %+v", result.Outcome, result.Content)
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

// The aspect subtree is the caller's to send here, unlike everywhere the device
// repository is involved. Without it a type described against a child aspect is
// missing and nothing says so.
func TestListImportTypesSendsTheAspectSubtree(t *testing.T) {
	imp := &fakeImports{types: []dsmodel.ImportType{catalogueType(testImportType, "Open-Meteo")}, typeTotal: 1}

	callCatalogue(t, imp, map[string]any{"function_id": "fn-temperature", "aspect_id": "pv"})

	if len(imp.typeListOpts) != 1 {
		t.Fatalf("sent %d listings, want one", len(imp.typeListOpts))
	}
	criteria := imp.typeListOpts[0].Criteria
	if len(criteria) != 1 {
		t.Fatalf("sent %+v, want one criterion: upstream ANDs them", criteria)
	}
	found := map[string]bool{}
	for _, id := range criteria[0].AspectIDs {
		found[id] = true
	}
	if !found["pv"] || !found["inverter"] {
		t.Errorf("aspect_ids = %v, want the node and its descendant", criteria[0].AspectIDs)
	}
	if criteria[0].FunctionID != "fn-temperature" {
		t.Errorf("function_id = %q", criteria[0].FunctionID)
	}
}

func TestListImportTypesReportsTheVariablesThatMatched(t *testing.T) {
	imp := &fakeImports{types: []dsmodel.ImportType{catalogueType(testImportType, "Open-Meteo")}, typeTotal: 1}

	answer := callCatalogue(t, imp, map[string]any{"function_id": "fn-temperature", "aspect_id": "pv"})

	listed, _ := answer["import_types"].([]any)
	if len(listed) != 1 {
		t.Fatalf("import_types = %v, want one", answer["import_types"])
	}
	row, _ := listed[0].(map[string]any)
	variables, _ := row["matching_variables"].([]any)
	if len(variables) != 1 {
		t.Fatalf("matching_variables = %v, want the one payload leaf that matched", row["matching_variables"])
	}
	variable, _ := variables[0].(map[string]any)
	// Addressable, not the raw output path: an operator mapping is relative to the
	// message, and the envelope leaves are not variables at all.
	if variable["variable_path"] != "value.temperature_2m" {
		t.Errorf("variable_path = %v, want the message-relative form", variable["variable_path"])
	}
	if row["deployable"] != true {
		t.Errorf("deployable = %v, want true for a type with no credential config", row["deployable"])
	}
	required, _ := row["required_configs"].([]any)
	if len(required) != 1 || required[0] != "station" {
		t.Errorf("required_configs = %v, want the config with no default", row["required_configs"])
	}
}

func TestListImportTypesCountsVariablesWhenNothingNarrowedTheQuery(t *testing.T) {
	// A browse over the catalogue must not pay for every leaf of every type. The
	// answer to "which variable do I want" is get_import_type_metadata, one type
	// at a time, and the note says so.
	imp := &fakeImports{types: []dsmodel.ImportType{catalogueType(testImportType, "Open-Meteo")}, typeTotal: 1}

	answer := callCatalogue(t, imp, map[string]any{"search": "meteo"})

	if len(imp.typeListOpts) != 1 || len(imp.typeListOpts[0].Criteria) != 0 {
		t.Fatalf("criteria = %+v, want none", imp.typeListOpts)
	}
	if imp.typeListOpts[0].Search != "meteo" {
		t.Errorf("search = %q, want it passed upstream", imp.typeListOpts[0].Search)
	}
	listed, _ := answer["import_types"].([]any)
	row, _ := listed[0].(map[string]any)
	if _, listedVariables := row["matching_variables"]; listedVariables {
		t.Errorf("an unnarrowed listing counts variables rather than listing them: %v", row)
	}
	if row["variables"] != float64(2) {
		t.Errorf("variables = %v, want the two payload leaves", row["variables"])
	}
	if !strings.Contains(answer["note"].(string), "get_import_type_metadata") {
		t.Errorf("the note should name the tool that lists them: %v", answer["note"])
	}
}

// A type that cannot be deployed from a chat is reported with the reason, not
// dropped: what it needs is one step in the platform's own dialog, and silence
// would read as "this platform cannot do it".
func TestListImportTypesNamesACredentialThatBlocksDeployment(t *testing.T) {
	withCredential := catalogueType(testImportType, "Open-Meteo")
	withCredential.Configs = append(withCredential.Configs,
		dsmodel.ImportTypeConfig{Name: "api_key", Type: models.String})
	imp := &fakeImports{types: []dsmodel.ImportType{withCredential}, typeTotal: 1}

	answer := callCatalogue(t, imp, map[string]any{"search": "meteo"})

	listed, _ := answer["import_types"].([]any)
	row, _ := listed[0].(map[string]any)
	if row["deployable"] != false {
		t.Errorf("deployable = %v, want false", row["deployable"])
	}
	blocking, _ := row["blocking_credentials"].([]any)
	if len(blocking) != 1 || blocking[0] != "api_key" {
		t.Errorf("blocking_credentials = %v, want [api_key]", row["blocking_credentials"])
	}
	if reason, _ := row["reason"].(string); !strings.Contains(reason, "import dialog") {
		t.Errorf("reason should name the route that works: %q", reason)
	}
}

// The catalogue is about what could be deployed, and a model reading it is one
// step away from deploying a second import for data the platform already pulls.
func TestListImportTypesSaysToCheckForAnExistingInstance(t *testing.T) {
	imp := &fakeImports{types: []dsmodel.ImportType{catalogueType(testImportType, "Open-Meteo")}, typeTotal: 1}

	answer := callCatalogue(t, imp, map[string]any{})

	note, _ := answer["note"].(string)
	if !strings.Contains(note, "list_import_instances") {
		t.Errorf("note should send the reader to the instance listing first: %q", note)
	}
	if !strings.Contains(note, "not running") && !strings.Contains(note, "nothing here is running") {
		t.Errorf("note has to say these are blueprints rather than data: %q", note)
	}
}

func TestListImportTypesReportsAnUnknownTotalRatherThanTruncation(t *testing.T) {
	imp := &fakeImports{types: []dsmodel.ImportType{catalogueType(testImportType, "Open-Meteo")}, typeTotal: -1}

	answer := callCatalogue(t, imp, map[string]any{})

	if _, claimed := answer["truncated"]; claimed {
		t.Errorf("an unknown total must not be reported as a complete page: %v", answer)
	}
	notes, _ := answer["notes"].([]any)
	if len(notes) == 0 {
		t.Error("an unknown total is worth saying: a caller cannot otherwise tell a short page from an exhausted one")
	}
}

func TestListImportTypesWithoutAnOntologySaysTheSubtreeWasNotExpanded(t *testing.T) {
	// Degraded rather than refused: matching the aspect alone still answers the
	// question that was asked, narrowly. Silence would be the wrong half of that.
	imp := &fakeImports{types: []dsmodel.ImportType{}, typeTotal: 0}
	result := dispatchImportToolWith(t, Deps{Imports: imp, DeviceLimit: 10},
		"list_import_types", map[string]any{"aspect_id": "pv"})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome %s: %+v", result.Outcome, result.Content)
	}

	if len(imp.typeListOpts) != 1 {
		t.Fatalf("sent %d listings, want one", len(imp.typeListOpts))
	}
	criteria := imp.typeListOpts[0].Criteria
	if len(criteria) != 1 || len(criteria[0].AspectIDs) != 1 || criteria[0].AspectIDs[0] != "pv" {
		t.Fatalf("aspect_ids = %+v, want the bare aspect", criteria)
	}

	encoded, _ := json.Marshal(result.Content)
	if !strings.Contains(string(encoded), "subtree") {
		t.Errorf("the answer should say the subtree was not read: %s", encoded)
	}
}
