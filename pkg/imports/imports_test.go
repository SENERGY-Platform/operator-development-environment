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

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	"github.com/SENERGY-Platform/device-selection/pkg/model/devicemodel"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

const (
	testToken      = "Bearer test"
	testInstanceID = "urn:infai:ses:import:weather-1"
	testTopic      = "urn_infai_ses_import_weather-1"
	testTypeID     = "urn:infai:ses:import-type:open-meteo"
)

// --- fakes ---

type fakeSelectables struct {
	serve []dsmodel.Selectable
	err   error
	calls int
}

func (f *fakeSelectables) QueryImports(context.Context, string, []drmodel.FilterCriteria) ([]dsmodel.Selectable, error) {
	f.calls++
	return f.serve, f.err
}

type fakeInstances struct {
	serve []idmodel.Instance
	total int64
	err   error
	opts  []InstanceListOptions
}

func (f *fakeInstances) ListInstances(_ context.Context, _ string, opts InstanceListOptions) ([]idmodel.Instance, int64, error) {
	f.opts = append(f.opts, opts)
	return f.serve, f.total, f.err
}

func (f *fakeInstances) ReadInstance(_ context.Context, _ string, id string) (idmodel.Instance, error) {
	if f.err != nil {
		return idmodel.Instance{}, f.err
	}
	for _, instance := range f.serve {
		if instance.Id == id {
			return instance, nil
		}
	}
	return idmodel.Instance{}, &UpstreamError{Resource: "/instances/" + id, Code: 404, Err: errors.New("not found")}
}

type fakeExports struct {
	serve []Export
	total int64
	err   error
}

func (f *fakeExports) ListExports(context.Context, string, int64, int64) ([]Export, int64, error) {
	return f.serve, f.total, f.err
}

func weatherSelectable() dsmodel.Selectable {
	return dsmodel.Selectable{
		Import: &dsmodel.Import{
			Id:           testInstanceID,
			Name:         "Leipzig weather",
			ImportTypeId: testTypeID,
			KafkaTopic:   testTopic,
		},
		ImportType: &dsmodel.ImportType{Id: testTypeID, Name: "Open-Meteo history"},
		// Keyed by the import type id rather than a service id: an import has no
		// services, and device-selection reuses the same map for both.
		ServicePathOptions: map[string][]dsmodel.PathOption{
			testTypeID: {
				{
					Path:             "value.temperature_2m",
					CharacteristicId: "ch-celsius",
					FunctionId:       "fn-temperature",
					AspectNode:       devicemodel.AspectNode{Id: "kitchen"},
					Type:             models.Float,
					Interaction:      devicemodel.EVENT,
				},
				{
					Path:       "value.pressure_msl",
					FunctionId: "fn-pressure",
					AspectNode: devicemodel.AspectNode{Id: "kitchen"},
					Type:       models.Float,
				},
			},
		},
	}
}

func newService(t *testing.T, sel *fakeSelectables, inst *fakeInstances, exp *fakeExports) *Service {
	t.Helper()
	deps := Deps{Selectables: sel, Instances: inst}
	if exp != nil {
		deps.Exports = exp
	}
	service, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

// --- construction ---

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := New(Deps{Instances: &fakeInstances{}}); err == nil {
		t.Error("expected an error without a selectables client")
	}
	if _, err := New(Deps{Selectables: &fakeSelectables{}}); err == nil {
		t.Error("expected an error without an instance client: discovery carries no container " +
			"status, so a service that could not read one would rank a stopped import as live")
	}
}

// --- discovery ---

func TestQueryImportsFlattensPathOptionsPerInstance(t *testing.T) {
	sel := &fakeSelectables{serve: []dsmodel.Selectable{weatherSelectable()}}
	service := newService(t, sel, &fakeInstances{}, nil)

	found, err := service.QueryImports(context.Background(), testToken,
		[]drmodel.FilterCriteria{{FunctionId: "fn-temperature"}})
	if err != nil {
		t.Fatalf("QueryImports: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("selectables = %d, want one per path option", len(found))
	}
	// Sorted by path, so the answer is reproducible despite the map upstream.
	if found[0].Path != "value.pressure_msl" || found[1].Path != "value.temperature_2m" {
		t.Errorf("paths = %q, %q; want them sorted so two identical requests agree",
			found[0].Path, found[1].Path)
	}
	if found[0].KafkaTopic != testTopic {
		t.Errorf("kafka_topic = %q, want %q", found[0].KafkaTopic, testTopic)
	}
	if found[0].InstanceID != testInstanceID || found[0].ImportTypeID != testTypeID {
		t.Errorf("instance/type = %q/%q, want the ids from the selectable",
			found[0].InstanceID, found[0].ImportTypeID)
	}
}

// A declared characteristic and an absent one are different answers; a fabricated
// id would authorise a wrong unit conversion.
func TestAnAbsentCharacteristicStaysNil(t *testing.T) {
	sel := &fakeSelectables{serve: []dsmodel.Selectable{weatherSelectable()}}
	service := newService(t, sel, &fakeInstances{}, nil)

	found, err := service.QueryImports(context.Background(), testToken,
		[]drmodel.FilterCriteria{{FunctionId: "fn-temperature"}})
	if err != nil {
		t.Fatalf("QueryImports: %v", err)
	}
	for _, selectable := range found {
		switch selectable.Path {
		case "value.pressure_msl":
			if selectable.CharacteristicID != nil {
				t.Errorf("pressure declares no characteristic but reports %q", *selectable.CharacteristicID)
			}
		case "value.temperature_2m":
			if selectable.CharacteristicID == nil || *selectable.CharacteristicID != "ch-celsius" {
				t.Errorf("temperature characteristic = %v, want ch-celsius", selectable.CharacteristicID)
			}
		}
	}
}

func TestQueryImportsRefusesAnEmptyCriteriaList(t *testing.T) {
	sel := &fakeSelectables{}
	service := newService(t, sel, &fakeInstances{}, nil)

	if _, err := service.QueryImports(context.Background(), testToken, nil); !errors.Is(err, ErrNoCriteria) {
		t.Errorf("err = %v, want ErrNoCriteria: an empty list is a match on everything upstream, "+
			"not an empty filter", err)
	}
	if sel.calls != 0 {
		t.Error("the request was sent anyway")
	}
}

// A device or a group in the answer is not a selectable this package can wire up.
func TestANonImportSelectableIsSkipped(t *testing.T) {
	sel := &fakeSelectables{serve: []dsmodel.Selectable{
		{Device: &dsmodel.PermSearchDevice{}},
		weatherSelectable(),
	}}
	service := newService(t, sel, &fakeInstances{}, nil)

	found, err := service.QueryImports(context.Background(), testToken,
		[]drmodel.FilterCriteria{{FunctionId: "fn-temperature"}})
	if err != nil {
		t.Fatalf("QueryImports: %v", err)
	}
	for _, selectable := range found {
		if selectable.InstanceID == "" {
			t.Error("a selectable with no instance id reached the caller, who would try to wire it")
		}
	}
	if len(found) != 2 {
		t.Errorf("selectables = %d, want only the import's two paths", len(found))
	}
}

// --- instances ---

func TestListRefusesAnEmptyIdListAndCapsTheLimit(t *testing.T) {
	inst := &fakeInstances{}
	service := newService(t, &fakeSelectables{}, inst, nil)

	if _, err := service.List(context.Background(), testToken,
		InstanceListOptions{IDs: []string{}}); !errors.Is(err, ErrInvalidRequest) {
		t.Error("an empty non-nil id list must be refused: upstream reads it as match-nothing")
	}
	if _, err := service.List(context.Background(), testToken,
		InstanceListOptions{Limit: MaxLimit + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Error("a limit above the ceiling must be refused")
	}
	if len(inst.opts) != 0 {
		t.Error("a refused request was sent anyway")
	}
}

func TestListAppliesTheDefaultLimit(t *testing.T) {
	inst := &fakeInstances{serve: []idmodel.Instance{{Id: testInstanceID}}, total: 1}
	service := newService(t, &fakeSelectables{}, inst, nil)

	result, err := service.List(context.Background(), testToken, InstanceListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result.Limit != DefaultLimit {
		t.Errorf("limit = %d, want the default %d", result.Limit, DefaultLimit)
	}
	if len(inst.opts) != 1 || inst.opts[0].Limit != DefaultLimit {
		t.Errorf("the default limit did not reach the client: %+v", inst.opts)
	}
}

func TestInstancesOfTypesJoinsClientSide(t *testing.T) {
	inst := &fakeInstances{serve: []idmodel.Instance{
		{Id: "a", ImportTypeId: testTypeID},
		{Id: "b", ImportTypeId: "urn:infai:ses:import-type:other"},
		{Id: "c", ImportTypeId: testTypeID},
	}}
	service := newService(t, &fakeSelectables{}, inst, nil)

	found, err := service.InstancesOfTypes(context.Background(), testToken, []string{testTypeID})
	if err != nil {
		t.Fatalf("InstancesOfTypes: %v", err)
	}
	if len(found) != 2 || found[0].Id != "a" || found[1].Id != "c" {
		t.Errorf("joined = %v, want the two instances of the requested type", found)
	}
	if len(inst.opts) != 1 || inst.opts[0].Limit != instanceListLimit {
		t.Errorf("the join has to read a full listing, because import-deploy offers no filter "+
			"by import type: %+v", inst.opts)
	}
}

func TestInstancesOfTypesWithNoTypesQueriesNothing(t *testing.T) {
	inst := &fakeInstances{}
	service := newService(t, &fakeSelectables{}, inst, nil)

	found, err := service.InstancesOfTypes(context.Background(), testToken, nil)
	if err != nil {
		t.Fatalf("InstancesOfTypes: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("found = %v, want none", found)
	}
	if len(inst.opts) != 0 {
		t.Error("a full listing was read to answer a question with no candidates")
	}
}

func TestRunningIsThreeValued(t *testing.T) {
	if _, known := Running(idmodel.Instance{}); known {
		t.Error("an instance with no status must not report a known running state")
	}
	running, known := Running(idmodel.Instance{Status: &idmodel.InstanceStatus{Running: true}})
	if !running || !known {
		t.Errorf("running = %v known = %v, want both true", running, known)
	}
	running, known = Running(idmodel.Instance{Status: &idmodel.InstanceStatus{Running: false}})
	if running || !known {
		t.Errorf("running = %v known = %v, want a known stop", running, known)
	}
}

func TestTransitionMessageNamesATransition(t *testing.T) {
	got := TransitionMessage(idmodel.Instance{Status: &idmodel.InstanceStatus{Transitioning: true}})
	if got == "" {
		t.Error("a transitioning instance has to say so: it is neither up nor down")
	}
	if TransitionMessage(idmodel.Instance{}) != "" {
		t.Error("an absent status must not invent a message")
	}
}

func TestGetTypeWithoutARepositoryExplainsItself(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{}, nil)
	_, err := service.GetType(context.Background(), testToken, testTypeID)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "semantic selection") {
		t.Errorf("the error should point at the route that does work: %v", err)
	}
}

// --- the type catalogue ---

func typeService(t *testing.T, types *fakeTypes) *Service {
	t.Helper()
	service, err := New(Deps{Selectables: &fakeSelectables{}, Instances: &fakeInstances{}, Types: types})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

func TestListTypesWithoutARepositoryExplainsItself(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{}, nil)
	_, err := service.ListTypes(context.Background(), testToken, TypeListOptions{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	// The refusal has to say why discovery is not a substitute, because it looks
	// like one right up to the point where the wanted type has no instance.
	if !strings.Contains(err.Error(), "already has one") {
		t.Errorf("the error should say what discovery cannot answer: %v", err)
	}
}

func TestListTypesDefaultsAndCapsTheLimit(t *testing.T) {
	types := &fakeTypes{listed: []dsmodel.ImportType{{Id: testTypeID}}, total: 1}
	service := typeService(t, types)

	result, err := service.ListTypes(context.Background(), testToken, TypeListOptions{})
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	if result.Limit != DefaultLimit {
		t.Errorf("limit = %d, want the default %d", result.Limit, DefaultLimit)
	}

	if _, err := service.ListTypes(context.Background(), testToken,
		TypeListOptions{Limit: MaxLimit + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want a refusal above the cap", err)
	}
}

func TestListTypesRefusesAnEmptyIDList(t *testing.T) {
	// Upstream reads `ids=` as match-nothing, so an accidental empty list would
	// report an empty catalogue rather than an error.
	service := typeService(t, &fakeTypes{})
	_, err := service.ListTypes(context.Background(), testToken, TypeListOptions{IDs: []string{}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestListTypesNeverAnswersWithANilSlice(t *testing.T) {
	service := typeService(t, &fakeTypes{listed: nil, total: -1})
	result, err := service.ListTypes(context.Background(), testToken, TypeListOptions{})
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	if result.Types == nil {
		t.Error("an empty catalogue marshals as [] rather than null")
	}
	if result.Total != -1 {
		t.Errorf("total = %d, want the unknown upstream reported", result.Total)
	}
}
