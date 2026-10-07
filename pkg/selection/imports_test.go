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
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/v3/lib/model"
	dsmodel "github.com/SENERGY-Platform/device-selection/v2/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
)

const (
	weatherInstanceID = "urn:infai:ses:import:weather-1"
	weatherTopic      = "urn_infai_ses_import_weather-1"
	weatherTypeID     = "urn:infai:ses:import-type:open-meteo"
)

// --- fake ---

type fakeImports struct {
	mutex sync.Mutex

	serve    []imports.Selectable
	queryErr error
	// criteria records every criterion the resolver actually sent, which is what
	// the AND/OR asymmetry tests assert on.
	criteria []drmodel.FilterCriteria

	instances   []idmodel.Instance
	instanceErr error
	listedIDs   []string

	history      imports.History
	historyCalls int
	// histories overrides history for the instances it names, so two candidates
	// can differ in what is stored of them.
	histories map[string]imports.History

	// The catalogue half. types is what import-repository answers for a matching
	// criterion; typeCriteria records what it was asked, which is where the aspect
	// subtree expansion is asserted.
	types        []dsmodel.ImportType
	typeErr      error
	typeCriteria []imports.TypeCriterion
}

func (f *fakeImports) QueryImports(_ context.Context, _ string, criteria []drmodel.FilterCriteria) ([]imports.Selectable, error) {
	f.mutex.Lock()
	f.criteria = append(f.criteria, criteria...)
	f.mutex.Unlock()
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	// Answer only for the function the fixtures put on the import type, so a
	// resolution that sends several criteria does not get the same rows back for
	// each and hide a merge bug.
	for _, criterion := range criteria {
		if criterion.FunctionId == "fn-temperature" || criterion.FunctionId == "" {
			return f.serve, nil
		}
	}
	return []imports.Selectable{}, nil
}

func (f *fakeImports) ListTypes(_ context.Context, _ string, opts imports.TypeListOptions) (imports.TypeListResult, error) {
	f.mutex.Lock()
	f.typeCriteria = append(f.typeCriteria, opts.Criteria...)
	f.mutex.Unlock()
	if f.typeErr != nil {
		return imports.TypeListResult{}, f.typeErr
	}
	// Answers for the function the fixtures describe, like QueryImports, so a
	// resolution sending several criteria cannot hide a merge bug behind the same
	// rows arriving twice.
	for _, criterion := range opts.Criteria {
		if criterion.FunctionID == "fn-temperature" || criterion.FunctionID == "" {
			return imports.TypeListResult{Types: f.types, Total: int64(len(f.types))}, nil
		}
	}
	return imports.TypeListResult{Types: []dsmodel.ImportType{}}, nil
}

func (f *fakeImports) List(_ context.Context, _ string, opts imports.InstanceListOptions) (imports.ListResult, error) {
	f.mutex.Lock()
	f.listedIDs = append(f.listedIDs, opts.IDs...)
	f.mutex.Unlock()
	if f.instanceErr != nil {
		return imports.ListResult{}, f.instanceErr
	}
	return imports.ListResult{Instances: f.instances, Total: int64(len(f.instances))}, nil
}

func (f *fakeImports) Histories(_ context.Context, _ string, ids []string) map[string]imports.History {
	f.mutex.Lock()
	f.historyCalls++
	f.mutex.Unlock()
	out := make(map[string]imports.History, len(ids))
	for _, id := range ids {
		out[id] = f.history
		if history, found := f.histories[id]; found {
			out[id] = history
		}
	}
	return out
}

func weatherSelectable(path string) imports.Selectable {
	characteristic := "ch-celsius"
	return imports.Selectable{
		InstanceID:     weatherInstanceID,
		InstanceName:   "Leipzig weather",
		KafkaTopic:     weatherTopic,
		ImportTypeID:   weatherTypeID,
		ImportTypeName: "Open-Meteo history",
		Path:           path,
		// The path arrives message-relative because the query is asked with
		// import_path_trim_first_element; see imports.SelectionClient.
		CharacteristicID: &characteristic,
		Type:             string(models.Float),
		FunctionID:       "fn-temperature",
		AspectIDs:        []string{"kitchen"},
	}
}

func runningInstance() idmodel.Instance {
	return idmodel.Instance{
		Id:           weatherInstanceID,
		Name:         "Leipzig weather",
		ImportTypeId: weatherTypeID,
		KafkaTopic:   weatherTopic,
		Status:       &idmodel.InstanceStatus{Running: true},
	}
}

func newImportHarness(t *testing.T, imp *fakeImports) *harness {
	t.Helper()
	return newHarnessImports(t, Options{}, false, imp)
}

func fullImports() *fakeImports {
	return &fakeImports{
		serve:     []imports.Selectable{weatherSelectable("value.temperature_2m")},
		instances: []idmodel.Instance{runningInstance()},
		history: imports.History{
			State:    imports.HistoryExported,
			ExportID: "export-1",
			Columns: []imports.HistoryColumn{
				{VariablePath: "value.temperature_2m", Column: "temperature_2m"},
			},
			Reason: "an export writes this import to timescale",
		},
	}
}

func resolve(t *testing.T, h *harness, req Request) Result {
	t.Helper()
	result, err := h.resolver.Resolve(context.Background(), testToken, req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return result
}

func hasNoteContaining(notes []string, substring string) bool {
	for _, note := range notes {
		if strings.Contains(note, substring) {
			return true
		}
	}
	return false
}

// --- the import half is part of one answer ---

func TestAnIntentResolvesImportsBesideDevices(t *testing.T) {
	h := newImportHarness(t, fullImports())
	result := resolve(t, h, Request{Intent: "temperature kitchen"})

	if len(result.ImportSelectables) != 1 {
		t.Fatalf("import_selectables = %d, want the weather import's temperature path", len(result.ImportSelectables))
	}
	selectable := result.ImportSelectables[0]
	if selectable.Path != "value.temperature_2m" {
		t.Errorf("path = %q, want the message-relative path", selectable.Path)
	}
	if selectable.KafkaTopic != weatherTopic {
		t.Errorf("kafka_topic = %q, want %q: it is half the wiring", selectable.KafkaTopic, weatherTopic)
	}
	if len(result.ImportCandidates) != 1 {
		t.Fatalf("import_candidates = %d, want one instance", len(result.ImportCandidates))
	}
	if result.ImportCandidates[0].Series != 1 {
		t.Errorf("series = %d, want the one path this instance contributes", result.ImportCandidates[0].Series)
	}
}

// The unit has to come from the same place a device's does, or the two halves of
// one answer describe the same characteristic differently.
func TestAnImportVariableResolvesItsUnitFromTheOntology(t *testing.T) {
	h := newImportHarness(t, fullImports())
	result := resolve(t, h, Request{Intent: "temperature kitchen"})

	if len(result.ImportSelectables) == 0 {
		t.Fatal("no import selectable to check")
	}
	if got := result.ImportSelectables[0].Unit; got != "°C" {
		t.Errorf("unit = %q, want °C from characteristic ch-celsius", got)
	}
	aspects := result.ImportSelectables[0].Aspects
	if len(aspects) != 1 || aspects[0].ID != "kitchen" || aspects[0].Name != "Kitchen" {
		t.Errorf("aspects = %+v, want [{kitchen Kitchen}]: device-selection resolves only the id for "+
			"an import path, so ODE names it from its own snapshot", aspects)
	}
}

// The whole reason the import half runs before the device half's early returns.
func TestAnImportOnlyPlatformStillAnswers(t *testing.T) {
	h := newImportHarness(t, fullImports())
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable { return nil }

	result := resolve(t, h, Request{Intent: "temperature kitchen"})

	if len(result.Selectables) != 0 {
		t.Fatalf("expected no device selectables, got %d", len(result.Selectables))
	}
	if len(result.ImportSelectables) != 1 {
		t.Errorf("import_selectables = %d, want the import to survive a device-side dead end: "+
			"an intent this platform can only satisfy from an import is still answerable",
			len(result.ImportSelectables))
	}
	if hasNoteContaining(result.Notes, "nothing of this kind on this platform") {
		t.Error("the device-side note still claims the platform has nothing, which is now wrong")
	}
}

// --- the asymmetries the plan recorded ---

func TestADeviceClassCriterionExcludesImportsAndSaysSo(t *testing.T) {
	imp := fullImports()
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{
		Intent:         "temperature kitchen",
		DeviceClassIDs: []string{"dc-meter"},
	})

	if len(result.ImportSelectables) != 0 {
		t.Errorf("import_selectables = %d, want none: an import type has no device class, so a "+
			"class-narrowed criterion cannot be expressed for it", len(result.ImportSelectables))
	}
	imp.mutex.Lock()
	sent := len(imp.criteria)
	imp.mutex.Unlock()
	if sent != 0 {
		t.Errorf("sent %d criteria to the import half; a class criterion with the class dropped "+
			"would silently widen the query", sent)
	}
	if !hasNoteContaining(result.Notes, "device class") {
		t.Errorf("notes do not explain why imports are absent: %v", result.Notes)
	}
}

// One criterion per request is a requirement rather than a style: device-selection
// ANDs a criteria list for devices and ORs it for imports.
func TestTheImportHalfIsAskedOneCriterionAtATime(t *testing.T) {
	imp := fullImports()
	h := newImportHarness(t, imp)

	// Two functions and one aspect resolve to two combinations.
	resolve(t, h, Request{
		Intent:      "temperature and power generation kitchen",
		FunctionIDs: []string{"fn-power", "fn-temperature"},
		AspectIDs:   []string{"kitchen"},
	})

	imp.mutex.Lock()
	defer imp.mutex.Unlock()
	if len(imp.criteria) < 2 {
		t.Fatalf("sent %d criteria, expected one per combination", len(imp.criteria))
	}
	for _, criterion := range imp.criteria {
		if criterion.Interaction != "" {
			t.Errorf("criterion carries interaction %q; an import path is always an event and "+
				"import-repository has no interaction dimension to match", criterion.Interaction)
		}
		if criterion.DeviceClassId != "" {
			t.Errorf("criterion carries device class %q, which an import type cannot have",
				criterion.DeviceClassId)
		}
	}
}

func TestAnUnconfiguredImportHalfIsReportedNotHidden(t *testing.T) {
	h := newImportHarness(t, nil)
	result := resolve(t, h, Request{Intent: "temperature kitchen"})

	if len(result.ImportSelectables) != 0 {
		t.Errorf("import_selectables = %d, want none without a configured service", len(result.ImportSelectables))
	}
	if !hasNoteContaining(result.Notes, "imports were not searched") {
		t.Errorf("an unsearched import half has to say so; an empty list reads as an empty "+
			"platform. notes: %v", result.Notes)
	}
}

func TestSkipImportsIsHonouredAndStated(t *testing.T) {
	imp := fullImports()
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{Intent: "temperature kitchen", SkipImports: true})

	if len(result.ImportSelectables) != 0 {
		t.Errorf("import_selectables = %d, want none when skipped", len(result.ImportSelectables))
	}
	imp.mutex.Lock()
	sent := len(imp.criteria)
	imp.mutex.Unlock()
	if sent != 0 {
		t.Errorf("sent %d criteria despite skip_imports", sent)
	}
	if !hasNoteContaining(result.Notes, "skipped on request") {
		t.Errorf("notes should record the skip: %v", result.Notes)
	}
}

// --- the two things a selectable cannot answer ---

func TestARunningImportIsDistinguishedFromAnUnknownOne(t *testing.T) {
	imp := fullImports()
	result := resolve(t, newImportHarness(t, imp), Request{Intent: "temperature kitchen"})
	if len(result.ImportCandidates) != 1 {
		t.Fatalf("import_candidates = %d", len(result.ImportCandidates))
	}
	candidate := result.ImportCandidates[0]
	if !candidate.Running || !candidate.RunningKnown {
		t.Errorf("running = %v known = %v, want a running instance to read as running",
			candidate.Running, candidate.RunningKnown)
	}

	// The same import with no status: not running is a different claim from
	// not known, and only the first would tell a developer to go start it.
	silent := fullImports()
	silent.instances = []idmodel.Instance{{
		Id: weatherInstanceID, Name: "Leipzig weather", ImportTypeId: weatherTypeID,
		KafkaTopic: weatherTopic,
	}}
	result = resolve(t, newImportHarness(t, silent), Request{Intent: "temperature kitchen"})
	candidate = result.ImportCandidates[0]
	if candidate.RunningKnown {
		t.Error("an instance with no status must not report a known running state")
	}
	if candidate.Running {
		t.Error("an unknown status must not read as running")
	}
}

func TestAnUnreadableInstanceListDoesNotFailTheResolution(t *testing.T) {
	imp := fullImports()
	imp.instanceErr = errors.New("import-deploy is down")

	result := resolve(t, newImportHarness(t, imp), Request{Intent: "temperature kitchen"})

	if len(result.ImportSelectables) != 1 {
		t.Errorf("import_selectables = %d, want the paths to survive a status failure: an import "+
			"whose status did not arrive is still a wireable input", len(result.ImportSelectables))
	}
	if len(result.ImportCandidates) != 1 {
		t.Fatalf("import_candidates = %d", len(result.ImportCandidates))
	}
	if result.ImportCandidates[0].RunningKnown {
		t.Error("a failed status read must not report a known running state")
	}
	if result.ImportCandidates[0].StatusNote == "" {
		t.Error("a failed status read has to say so")
	}
}

func TestAQueryFailureFailsTheResolution(t *testing.T) {
	imp := fullImports()
	imp.queryErr = errors.New("device-selection is down")

	_, err := newImportHarness(t, imp).resolver.Resolve(
		context.Background(), testToken, Request{Intent: "temperature kitchen"})
	if err == nil {
		t.Fatal("expected the resolution to fail: a result silently missing every import of a " +
			"matching type has no field that could honestly say so")
	}
}

func TestTheHistoryStateReachesTheCandidate(t *testing.T) {
	imp := fullImports()
	imp.history = imports.History{
		State:  imports.HistoryLiveOnly,
		Reason: "no export exists for this import",
	}
	result := resolve(t, newImportHarness(t, imp), Request{Intent: "temperature kitchen"})

	if len(result.ImportCandidates) != 1 {
		t.Fatalf("import_candidates = %d", len(result.ImportCandidates))
	}
	if got := result.ImportCandidates[0].History.State; got != imports.HistoryLiveOnly {
		t.Errorf("history state = %q, want live_only to reach the caller: it is the difference "+
			"between an operator that can be backtested and one that cannot", got)
	}
}

// The import half must not read a value, for the same reason the device half must
// not: the whole operation is exposure tier L0.
func TestTheImportHalfReadsNoValues(t *testing.T) {
	result := resolve(t, newImportHarness(t, fullImports()), Request{Intent: "temperature kitchen"})
	if result.Reads.Values != 0 {
		t.Errorf("reads.values = %d, want zero: semantic selection is tier L0 by construction",
			result.Reads.Values)
	}
	if result.Reads.ImportSelectables == 0 {
		t.Error("the import selectables requests are not counted, so the cost of the answer is invisible")
	}
}

// Neither import-deploy nor analytics-serving can filter by what is being asked,
// so both are one wide read for the whole shortlist. A call per candidate would
// re-read the same listing once per candidate.
func TestTheShortlistIsAskedAboutOnce(t *testing.T) {
	imp := fullImports()
	imp.serve = []imports.Selectable{
		weatherSelectable("value.temperature_2m"),
		weatherSelectable("value.pressure_msl"),
	}
	result := resolve(t, newImportHarness(t, imp), Request{Intent: "temperature kitchen"})

	if len(result.ImportSelectables) != 2 {
		t.Fatalf("import_selectables = %d", len(result.ImportSelectables))
	}
	imp.mutex.Lock()
	calls := imp.historyCalls
	imp.mutex.Unlock()
	if calls != 1 {
		t.Errorf("the export listing was read %d times, want once for the whole shortlist", calls)
	}
	if result.Reads.ImportExports != 1 || result.Reads.ImportInstances != 1 {
		t.Errorf("reads = %+v, want one of each", result.Reads)
	}
}

// --- the catalogue half: what could be deployed ---

// deployableType is an import type nobody has an instance of, described against
// the aspect below the one a resolution asks for.
func deployableType(id, name string) dsmodel.ImportType {
	return dsmodel.ImportType{
		Id:   id,
		Name: name,
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
						FunctionId: "fn-temperature", AspectId: "kitchen"},
				}},
			},
		},
	}
}

// The case the whole catalogue half exists for: the platform describes the
// wanted signal and nobody has deployed an import for it, which is
// indistinguishable from "this platform has nothing of the kind" in an empty
// selectables answer.
func TestATypeWithNoInstanceIsReportedAsDeployable(t *testing.T) {
	imp := &fakeImports{types: []dsmodel.ImportType{deployableType(weatherTypeID, "Open-Meteo history")}}
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{
		Intent: "temperature kitchen", FunctionIDs: []string{"fn-temperature"}, AspectIDs: []string{"kitchen"},
	})

	if len(result.ImportCandidates) != 0 {
		t.Fatalf("import_candidates = %d, want none: nothing is deployed", len(result.ImportCandidates))
	}
	if len(result.DeployableImportTypes) != 1 {
		t.Fatalf("deployable_import_types = %+v, want the matching type", result.DeployableImportTypes)
	}
	deployable := result.DeployableImportTypes[0]
	if deployable.ImportTypeID != weatherTypeID {
		t.Errorf("import_type_id = %q, want %q", deployable.ImportTypeID, weatherTypeID)
	}
	if !deployable.Deployable {
		t.Errorf("a type with no credential config deploys from a chat: %+v", deployable)
	}
	if len(deployable.MatchingVariables) != 1 ||
		deployable.MatchingVariables[0].Path != "value.temperature_2m" {
		t.Errorf("matching_variables = %+v, want the payload leaf that carries the criteria",
			deployable.MatchingVariables)
	}
	// The same two profiler functions the device and import selectables use, so a
	// unit means one thing across all three lists.
	if deployable.MatchingVariables[0].Unit != "°C" {
		t.Errorf("unit = %q, want the characteristic's", deployable.MatchingVariables[0].Unit)
	}
	if len(deployable.RequiredConfigs) != 1 || deployable.RequiredConfigs[0] != "station" {
		t.Errorf("required_configs = %v, want the config with no default", deployable.RequiredConfigs)
	}
	// The note is the point: an empty import half now says which of its two causes
	// this is.
	if !hasNoteContaining(result.Notes, "deployable_import_types") {
		t.Errorf("notes should send the reader to the list: %v", result.Notes)
	}
	if result.Reads.ImportTypes == 0 {
		t.Error("the catalogue read is a cost of the answer and belongs in reads")
	}
}

// The network query sends the bare aspect id and relies on import-repository's
// and_combine_criteria_aspect_ids to expand it server-side (see
// TestTheCatalogueIsAskedWithTheBareAspectID). That parameter cannot help the
// *local* re-check of which variable satisfied the query, though: the type
// below is described against "inverter", a child of the requested "pv", and
// deployableImportType recomputes MatchingVariables entirely from the
// already-fetched import type with no further platform call. Without expanding
// the subtree for that local check too, this type would match (the server said
// so) and then show an empty matching_variables list — correct Deployable=true,
// wrong and confusing reason.
func TestADeployableTypeDescribedAgainstAChildAspectStillShowsItsMatchingVariable(t *testing.T) {
	childType := deployableType(weatherTypeID, "Open-Meteo history")
	childType.Output.SubContentVariables[2].SubContentVariables[0].AspectId = "inverter"
	imp := &fakeImports{types: []dsmodel.ImportType{childType}}
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{
		Intent: "temperature pv system", FunctionIDs: []string{"fn-temperature"}, AspectIDs: []string{"pv"},
	})

	if len(result.DeployableImportTypes) != 1 {
		t.Fatalf("deployable_import_types = %+v, want the type matched via the child aspect", result.DeployableImportTypes)
	}
	deployable := result.DeployableImportTypes[0]
	if len(deployable.MatchingVariables) != 1 || deployable.MatchingVariables[0].Path != "value.temperature_2m" {
		t.Errorf("matching_variables = %+v, want the variable described against the child aspect",
			deployable.MatchingVariables)
	}
	if strings.Contains(deployable.Note, "no single variable") {
		t.Errorf("note = %q, want it not to claim no variable matched: one did", deployable.Note)
	}
}

func TestATypeThatIsAlreadyDeployedIsNotOfferedForDeployment(t *testing.T) {
	// It is in import_candidates, where it carries a topic and a running status.
	// Repeating it here would invite a second container for data the platform
	// already pulls.
	imp := fullImports()
	imp.types = []dsmodel.ImportType{deployableType(weatherTypeID, "Open-Meteo history")}
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{Intent: "temperature kitchen", FunctionIDs: []string{"fn-temperature"}})

	if len(result.ImportCandidates) != 1 {
		t.Fatalf("import_candidates = %+v, want the running import", result.ImportCandidates)
	}
	if len(result.DeployableImportTypes) != 0 {
		t.Errorf("deployable_import_types = %+v, want none: this type is already deployed",
			result.DeployableImportTypes)
	}
}

func TestAnUndeployableTypeSaysWhyRatherThanBeingDropped(t *testing.T) {
	withCredential := deployableType(weatherTypeID, "Open-Meteo history")
	withCredential.Configs = append(withCredential.Configs,
		dsmodel.ImportTypeConfig{Name: "api_key", Type: models.String})
	imp := &fakeImports{types: []dsmodel.ImportType{withCredential}}
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{Intent: "temperature kitchen", FunctionIDs: []string{"fn-temperature"}})

	if len(result.DeployableImportTypes) != 1 {
		t.Fatalf("deployable_import_types = %+v, want the type reported", result.DeployableImportTypes)
	}
	deployable := result.DeployableImportTypes[0]
	if deployable.Deployable {
		t.Error("a credential config with no default cannot be deployed from a chat")
	}
	if len(deployable.BlockingCredentials) != 1 || deployable.BlockingCredentials[0] != "api_key" {
		t.Errorf("blocking_credentials = %v, want [api_key]", deployable.BlockingCredentials)
	}
	// Dropping it would tell a developer the platform cannot do this at all, when
	// what it needs is one step in the platform's own import dialog.
	if !strings.Contains(deployable.Note, "import dialog") {
		t.Errorf("note should name the route that works: %q", deployable.Note)
	}
}

// The catalogue criterion carries the bare requested id, unexpanded. The device
// repository covers a criterion's whole subtree on its own; import-repository
// does not, but ODE no longer expands it locally either — the import-repository
// client asks for and_combine_criteria_aspect_ids, which ANDs a criterion's
// aspect ids on one variable and expands each one's own subtree server-side
// (see pkg/imports/client.go and its own test for that parameter).
func TestTheCatalogueIsAskedWithTheBareAspectID(t *testing.T) {
	imp := &fakeImports{}
	h := newImportHarness(t, imp)

	resolve(t, h, Request{
		Intent: "power generation", FunctionIDs: []string{"fn-temperature"}, AspectIDs: []string{"pv"},
	})

	imp.mutex.Lock()
	defer imp.mutex.Unlock()
	if len(imp.typeCriteria) == 0 {
		t.Fatal("the catalogue was not asked at all")
	}
	for _, criterion := range imp.typeCriteria {
		if len(criterion.AspectIDs) != 1 || criterion.AspectIDs[0] != "pv" {
			t.Errorf("aspect_ids = %v, want exactly [pv], unexpanded", criterion.AspectIDs)
		}
	}
}

func TestADeviceClassCriterionIsNotSentToTheCatalogue(t *testing.T) {
	// An import type has no device class, so the criterion cannot be expressed;
	// sending it with the field dropped would widen the query instead.
	imp := &fakeImports{}
	h := newImportHarness(t, imp)

	resolve(t, h, Request{
		Intent:         "temperature kitchen",
		FunctionIDs:    []string{"fn-temperature"},
		DeviceClassIDs: []string{"dc-meter"},
	})

	imp.mutex.Lock()
	defer imp.mutex.Unlock()
	if len(imp.typeCriteria) != 0 {
		t.Errorf("sent %+v to the catalogue, want nothing", imp.typeCriteria)
	}
}

func TestAnEmptyCatalogueSaysThereIsNothingToDeploy(t *testing.T) {
	// The distinction the note exists for: this platform describes nothing of the
	// kind, which is a different answer from "nothing is running".
	imp := &fakeImports{}
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{Intent: "temperature kitchen", FunctionIDs: []string{"fn-temperature"}})

	if !hasNoteContaining(result.Notes, "nothing to deploy") {
		t.Errorf("notes should state that the catalogue was read and was empty: %v", result.Notes)
	}
}

func TestAnUnreadableCatalogueDegradesRatherThanFailingTheResolution(t *testing.T) {
	// Unlike the selectables half. That one is the answer; this one says what
	// could additionally be deployed, and an answer without it is still complete
	// about what exists — as long as it says so.
	imp := fullImports()
	imp.typeErr = errors.New("import-repository unreachable")
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{Intent: "temperature kitchen", FunctionIDs: []string{"fn-temperature"}})

	if len(result.ImportCandidates) != 1 {
		t.Errorf("the import half should be unaffected: %+v", result.ImportCandidates)
	}
	if len(result.DeployableImportTypes) != 0 {
		t.Errorf("deployable_import_types = %+v, want none", result.DeployableImportTypes)
	}
	if !hasNoteContaining(result.Notes, "could not be read") {
		t.Errorf("a failed catalogue read must not read as an empty catalogue: %v", result.Notes)
	}
}

func TestTheCatalogueIsNotAskedWhenTheImportHalfIsSkipped(t *testing.T) {
	imp := fullImports()
	imp.types = []dsmodel.ImportType{deployableType("type-2", "Another")}
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{Intent: "temperature kitchen", SkipImports: true})

	imp.mutex.Lock()
	defer imp.mutex.Unlock()
	if len(imp.typeCriteria) != 0 {
		t.Errorf("asked the catalogue despite skip_imports: %+v", imp.typeCriteria)
	}
	if len(result.DeployableImportTypes) != 0 {
		t.Errorf("deployable_import_types = %+v, want none", result.DeployableImportTypes)
	}
}

func TestADeploymentWithoutAnImportRepositorySaysTheTwoCausesAreIndistinguishable(t *testing.T) {
	// pkg/imports refuses the catalogue read with ErrInvalidRequest when no
	// import-repository is configured. That is a permanent property of the
	// deployment rather than a service that did not answer, and quoting it as an
	// outage would send somebody to check a service that was never called.
	imp := &fakeImports{typeErr: fmt.Errorf("%w: no import-repository is configured", imports.ErrInvalidRequest)}
	h := newImportHarness(t, imp)

	result := resolve(t, h, Request{Intent: "temperature kitchen", FunctionIDs: []string{"fn-temperature"}})

	if hasNoteContaining(result.Notes, "could not be read") {
		t.Errorf("a missing configuration must not read as an unreachable service: %v", result.Notes)
	}
	if !hasNoteContaining(result.Notes, "import_repo_url") {
		t.Errorf("the note should name the configuration that is missing: %v", result.Notes)
	}
}

// --- import candidates are ordered by what their export stores ---

const (
	stationInstanceID = "urn:infai:ses:import:weather-2"
	weatherPath       = "value.temperature_2m"
)

// twoWeatherImports is the case this order exists for: two imports of the same
// type carrying the same signal, discovered in the order given, differing only in
// their histories.
func twoWeatherImports(first, second imports.History) *fakeImports {
	other := weatherSelectable(weatherPath)
	other.InstanceID = stationInstanceID
	other.InstanceName = "Leipzig weather, second"
	other.KafkaTopic = "urn_infai_ses_import_weather-2"
	return &fakeImports{
		serve:     []imports.Selectable{weatherSelectable(weatherPath), other},
		instances: []idmodel.Instance{runningInstance()},
		histories: map[string]imports.History{weatherInstanceID: first, stationInstanceID: second},
	}
}

func exportedHistory(exportID string, paths ...string) imports.History {
	columns := []imports.HistoryColumn{}
	for _, path := range paths {
		columns = append(columns, imports.HistoryColumn{
			VariablePath: path, Column: strings.TrimPrefix(path, "value."), Type: "float",
		})
	}
	return imports.History{State: imports.HistoryExported, ExportID: exportID, Columns: columns}
}

func candidateOrder(candidates []ImportCandidate) []string {
	out := []string{}
	for _, candidate := range candidates {
		out = append(out, candidate.InstanceID)
	}
	return out
}

func TestImportCandidatesAreOrderedByTheRowsTheirExportStores(t *testing.T) {
	imp := twoWeatherImports(
		exportedHistory("export-thin", weatherPath),
		exportedHistory("export-deep", weatherPath),
	)
	h := newHarnessImports(t, Options{}, true, imp)
	deepFirst := testNow.AddDate(-3, 0, 0)
	h.timeseries.exports = map[string]storedRows{
		"export-thin": {rows: 48, first: testNow.AddDate(0, 0, -2)},
		"export-deep": {rows: 26280, first: deepFirst},
	}

	result := h.resolve(t, Request{Intent: "temperature kitchen"})

	order := candidateOrder(result.ImportCandidates)
	if fmt.Sprint(order) != fmt.Sprint([]string{stationInstanceID, weatherInstanceID}) {
		t.Fatalf("order = %v, want the import whose export holds years first", order)
	}
	deep := result.ImportCandidates[0].Stored
	if deep == nil {
		t.Fatal("the exported candidate carries no stored extent")
	}
	if rows, _ := deep.Rows.Get(); rows != 26280 {
		t.Errorf("rows = %v, want the counted 26280", deep.Rows)
	}
	if first, _ := deep.FirstRow.Get(); !first.Equal(deepFirst) {
		t.Errorf("first_row = %v, want %s", deep.FirstRow, deepFirst)
	}
	// A count and a first-row lookup per exported candidate, none of them a value.
	if result.Reads.ImportCounts != 4 {
		t.Errorf("import_counts = %d, want 4", result.Reads.ImportCounts)
	}
	if result.Reads.Values != 0 {
		t.Errorf("values = %d, want 0: counting an export reads no value", result.Reads.Values)
	}
}

func TestAnImportWithoutStoredRowsIsOrderedBehindOneThatMayHaveThem(t *testing.T) {
	cases := []struct {
		name     string
		weak     imports.History
		stored   map[string]storedRows
		uncovers bool
	}{
		{name: "no export", weak: imports.History{State: imports.HistoryLiveOnly}},
		{
			name:   "an export with no row",
			weak:   exportedHistory("export-empty", weatherPath),
			stored: map[string]storedRows{"export-empty": {}},
		},
		{
			name:     "an export without the selected path",
			weak:     exportedHistory("export-other", "value.pressure_msl"),
			uncovers: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The weak import is discovered first, and the other one's history could
			// not be established — which still ranks above knowing nothing is stored.
			imp := twoWeatherImports(tc.weak, imports.History{State: imports.HistoryUnknown})
			h := newHarnessImports(t, Options{}, true, imp)
			h.timeseries.exports = tc.stored

			result := h.resolve(t, Request{Intent: "temperature kitchen"})

			order := candidateOrder(result.ImportCandidates)
			if fmt.Sprint(order) != fmt.Sprint([]string{stationInstanceID, weatherInstanceID}) {
				t.Fatalf("order = %v, want the import with nothing stored last", order)
			}
			weak := result.ImportCandidates[1]
			if tc.uncovers {
				if fmt.Sprint(weak.UncoveredPaths) != fmt.Sprint([]string{weatherPath}) {
					t.Errorf("uncovered_paths = %v, want the selected path the export lacks", weak.UncoveredPaths)
				}
				if weak.Stored != nil {
					t.Errorf("stored = %+v, want none: there was no column to count", weak.Stored)
				}
			}
		})
	}
}

func TestAFailedCountLeavesTheCandidateInPlaceAndSaysWhy(t *testing.T) {
	imp := twoWeatherImports(
		imports.History{State: imports.HistoryLiveOnly},
		exportedHistory("export-broken", weatherPath),
	)
	h := newHarnessImports(t, Options{}, true, imp)
	h.timeseries.exports = map[string]storedRows{"export-broken": {err: errors.New("timescale is down")}}

	result := h.resolve(t, Request{Intent: "temperature kitchen"})

	if order := candidateOrder(result.ImportCandidates); order[0] != stationInstanceID {
		t.Fatalf("order = %v, want an export that could not be counted ahead of no export", order)
	}
	stored := result.ImportCandidates[0].Stored
	if stored == nil || stored.Rows.IsComputed() {
		t.Fatalf("stored = %+v, want rows not_computed", stored)
	}
	if status := stored.Rows.Status(); !strings.Contains(status.Detail, "timescale is down") {
		t.Errorf("rows status = %+v, want the platform's error in it", status)
	}
}

func TestTheImportCountStopsAtTheTrainingEnd(t *testing.T) {
	imp := twoWeatherImports(
		exportedHistory("export-a", weatherPath),
		imports.History{State: imports.HistoryLiveOnly},
	)
	h := newHarnessImports(t, Options{}, true, imp)
	h.timeseries.exports = map[string]storedRows{"export-a": {rows: 10, first: testNow.AddDate(-1, 0, 0)}}
	trainingEnd := testNow.AddDate(0, -1, 0)
	split := &exposure.Split{TrainingEnd: trainingEnd, TestEnd: testNow}

	h.resolve(t, Request{Intent: "temperature kitchen", Split: split})

	if len(h.timeseries.counted) == 0 {
		t.Fatal("no count was asked")
	}
	for _, element := range h.timeseries.counted {
		end, err := time.Parse(time.RFC3339, *element.Time.End)
		if err != nil || end.After(trainingEnd) {
			t.Errorf("a count runs to %v, want no later than the training end %s", *element.Time.End, trainingEnd)
		}
	}
}

func TestUnrankedImportCandidatesKeepTheirOrderAndSaySo(t *testing.T) {
	imp := twoWeatherImports(
		imports.History{State: imports.HistoryLiveOnly},
		exportedHistory("export-a", weatherPath),
	)
	h := newHarnessImports(t, Options{}, true, imp)

	result := h.resolve(t, Request{Intent: "temperature kitchen", SkipRanking: true})

	if order := candidateOrder(result.ImportCandidates); order[0] != weatherInstanceID {
		t.Errorf("order = %v, want discovery order when ranking is skipped", order)
	}
	if len(h.timeseries.counted) != 0 {
		t.Errorf("%d count(s) were asked although ranking was skipped", len(h.timeseries.counted))
	}
	if !hasNoteContaining(result.Notes, "unordered by what their exports hold") {
		t.Errorf("notes = %v, want one saying the import order is not by stored history", result.Notes)
	}
}

// An export lacking a selected path is read from Kafka once that path is mapped,
// so the import whose export covers both paths goes first despite fewer rows.
func TestAnExportCoveringEverySelectedPathOutranksOneWithMoreRows(t *testing.T) {
	const pressurePath = "value.pressure_msl"
	imp := twoWeatherImports(
		exportedHistory("export-partial", weatherPath),
		exportedHistory("export-full", weatherPath, pressurePath),
	)
	for _, instanceID := range []string{weatherInstanceID, stationInstanceID} {
		extra := weatherSelectable(pressurePath)
		extra.InstanceID = instanceID
		imp.serve = append(imp.serve, extra)
	}
	h := newHarnessImports(t, Options{}, true, imp)
	h.timeseries.exports = map[string]storedRows{
		"export-partial": {rows: 50000, first: testNow.AddDate(-4, 0, 0)},
		"export-full":    {rows: 100, first: testNow.AddDate(0, -1, 0)},
	}

	result := h.resolve(t, Request{Intent: "temperature kitchen"})

	order := candidateOrder(result.ImportCandidates)
	if fmt.Sprint(order) != fmt.Sprint([]string{stationInstanceID, weatherInstanceID}) {
		t.Fatalf("order = %v, want the fully covering export first", order)
	}
	if fmt.Sprint(result.ImportCandidates[1].UncoveredPaths) != fmt.Sprint([]string{pressurePath}) {
		t.Errorf("uncovered_paths = %v, want %s", result.ImportCandidates[1].UncoveredPaths, pressurePath)
	}
}
