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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// --- fixtures ---

const (
	meterTypeID     = "dt-meter"
	plugTypeID      = "dt-plug"
	meterServiceID  = "svc-readings"
	plugServiceID   = "svc-plug"
	powerPath       = "value.power"
	temperaturePath = "value.temperature"
	samplesPath     = "value.samples"
	setpointPath    = "value.setpoint"
	wattsPath       = "reading.watts"
	ampsPath        = "reading.amps"

	testToken = "Bearer caller"
)

var (
	testNow  = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	testFrom = testNow.Add(-90 * 24 * time.Hour)
)

// meterService carries the three shapes that matter: a selectable numeric
// variable, a second one under a different function, and a list column that
// exists in the database but is not a readable scalar series.
func meterService() models.Service {
	return models.Service{
		Id: meterServiceID, Name: "readings", Interaction: models.EVENT,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{
				Id: "cv-root", Name: "value", Type: models.Structure,
				SubContentVariables: []models.ContentVariable{
					{
						Id: "cv-power", Name: "power", Type: models.Float,
						CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectId: "kitchen",
					},
					{
						Id: "cv-temperature", Name: "temperature", Type: models.Float,
						CharacteristicId: "ch-celsius", FunctionId: "fn-temperature", AspectId: "kitchen",
					},
					{
						Id: "cv-samples", Name: "samples", Type: models.List,
						FunctionId: "fn-power", AspectId: "kitchen",
						SubContentVariables: []models.ContentVariable{
							{Id: "cv-sample", Name: "*", Type: models.Float},
						},
					},
				},
			},
		}},
	}
}

// plugService is the incomplete device type D16 exists for: one variable with no
// characteristic at all, one referencing a characteristic the ontology does not
// have.
func plugService() models.Service {
	return models.Service{
		Id: plugServiceID, Name: "plug", Interaction: models.EVENT,
		Outputs: []models.Content{{
			ContentVariable: models.ContentVariable{
				Id: "cv-plug-root", Name: "reading", Type: models.Structure,
				SubContentVariables: []models.ContentVariable{
					{
						Id: "cv-watts", Name: "watts", Type: models.Float,
						FunctionId: "fn-power", AspectId: "kitchen",
					},
					{
						Id: "cv-amps", Name: "amps", Type: models.Float,
						CharacteristicId: "ch-unknown-to-the-snapshot",
						FunctionId:       "fn-power", AspectId: "kitchen",
					},
				},
			},
		}},
	}
}

func testSnapshot() *ontology.Snapshot {
	return &ontology.Snapshot{
		MeasuringFunctions: []models.Function{
			{Id: "fn-power", Name: "getPowerGenerationFunction", DisplayName: "Power Generation",
				ConceptId: "concept-power", RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
			{Id: "fn-temperature", Name: "getTemperatureFunction", DisplayName: "Temperature",
				ConceptId: "concept-temperature", RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
		},
		AspectNodes: []models.AspectNode{
			{Id: "kitchen", Name: "Kitchen", RootId: "building"},
			{Id: "pv", Name: "PV System", RootId: "pv", DescendentIds: []string{"inverter"}},
			{Id: "inverter", Name: "Inverter", ParentId: "pv", RootId: "pv"},
		},
		DeviceClasses: []models.DeviceClass{{Id: "dc-meter", Name: "Energy Meter"}},
	}
}

func testIndex() *profiler.OntologyIndex {
	watt := models.Characteristic{Id: "ch-watt", Name: "Watt", DisplayUnit: "W", MinValue: 0.0, MaxValue: 10000.0}
	celsius := models.Characteristic{Id: "ch-celsius", Name: "Celsius", DisplayUnit: "°C"}
	return profiler.NewOntologyIndex(
		[]models.Characteristic{watt, celsius},
		[]models.ConceptWithCharacteristics{{
			Id: "concept-power", BaseCharacteristicId: "ch-watt",
			Characteristics: []models.Characteristic{watt},
		}},
		[]models.Function{
			{Id: "fn-power", ConceptId: "concept-power"},
			{Id: "fn-temperature", ConceptId: "concept-temperature"},
		},
	)
}

func meterDevice(id string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: id, Name: "PV Meter", DeviceTypeId: meterTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: meterTypeID, Name: "Meter", Services: []models.Service{meterService()},
		},
	}
}

func plugDevice(id string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: id, Name: "Smart Plug", DeviceTypeId: plugTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: plugTypeID, Name: "Plug", Services: []models.Service{plugService()},
		},
	}
}

func option(serviceID, path, characteristicID, functionID, aspectID string) drmodel.ServicePathOption {
	return drmodel.ServicePathOption{
		ServiceId:        serviceID,
		Path:             path,
		CharacteristicId: characteristicID,
		FunctionId:       functionID,
		AspectNode:       models.AspectNode{Id: aspectID, Name: "Kitchen"},
		Type:             models.Float,
		Interaction:      models.EVENT,
	}
}

// meterSelectables answers a single criterion the way the device repository
// would: only the paths whose function and aspect the criterion actually names.
func meterSelectables(criterion drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
	if criterion.AspectId != "" && criterion.AspectId != "kitchen" {
		return []drmodel.DeviceTypeSelectable{}
	}
	if criterion.Interaction != "" && criterion.Interaction != models.EVENT {
		return []drmodel.DeviceTypeSelectable{}
	}

	options := []drmodel.ServicePathOption{}
	if criterion.FunctionId == "" || criterion.FunctionId == "fn-power" {
		options = append(options, option(meterServiceID, powerPath, "ch-watt", "fn-power", "kitchen"))
	}
	if criterion.FunctionId == "" || criterion.FunctionId == "fn-temperature" {
		options = append(options, option(meterServiceID, temperaturePath, "ch-celsius", "fn-temperature", "kitchen"))
	}
	if len(options) == 0 {
		return []drmodel.DeviceTypeSelectable{}
	}
	return []drmodel.DeviceTypeSelectable{{
		DeviceTypeId:       meterTypeID,
		Services:           []models.Service{meterService()},
		ServicePathOptions: map[string][]drmodel.ServicePathOption{meterServiceID: options},
	}}
}

// --- platform stand-ins ---

type fakeOntology struct {
	mux sync.Mutex

	snap    *ontology.Snapshot
	snapErr error

	// calls records every criteria list as sent, which is how the tests check the
	// one thing about this endpoint that is easy to get wrong: the list is ANDed
	// upstream, so ODE must send one criterion per request.
	calls  [][]drmodel.FilterCriteria
	answer func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable
	err    error
}

func (f *fakeOntology) Snapshot(context.Context, string) (*ontology.Snapshot, error) {
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return f.snap, nil
}

func (f *fakeOntology) DeviceTypeSelectables(
	_ context.Context, _ string, criteria []drmodel.FilterCriteria, _ ontology.SelectableOptions,
) ([]drmodel.DeviceTypeSelectable, error) {
	f.mux.Lock()
	f.calls = append(f.calls, criteria)
	f.mux.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	if f.answer == nil || len(criteria) != 1 {
		return []drmodel.DeviceTypeSelectable{}, nil
	}
	return f.answer(criteria[0]), nil
}

func (f *fakeOntology) sentCriteria() [][]drmodel.FilterCriteria {
	f.mux.Lock()
	defer f.mux.Unlock()
	return append([][]drmodel.FilterCriteria{}, f.calls...)
}

type fakeDevices struct {
	mux     sync.Mutex
	options []drmodel.ExtendedDeviceListOptions
	serve   []models.ExtendedDevice
	total   int64
	err     error
}

func (f *fakeDevices) List(_ string, options drmodel.ExtendedDeviceListOptions) (devices.ListResult, error) {
	f.mux.Lock()
	f.options = append(f.options, options)
	f.mux.Unlock()

	if f.err != nil {
		return devices.ListResult{}, f.err
	}
	total := f.total
	if total == 0 {
		total = int64(len(f.serve))
	}
	return devices.ListResult{Devices: f.serve, Total: total, Limit: options.Limit}, nil
}

// fakeTimeseries answers the two read-free endpoints and fails the test on a
// value read. The zero-read property of tier L0 is enforced here rather than
// asserted afterwards: a Query that happens is a failed test wherever it came
// from.
type fakeTimeseries struct {
	t        *testing.T
	availErr error
}

func (f *fakeTimeseries) DataAvailability(_ context.Context, _ string, _ string) ([]timeseries.Availability, error) {
	if f.availErr != nil {
		return nil, f.availErr
	}
	from, to := testFrom, testNow
	return []timeseries.Availability{
		{ServiceId: meterServiceID, From: &from, To: &to},
		{ServiceId: plugServiceID, From: &from, To: &to},
	}, nil
}

func (f *fakeTimeseries) DeviceUsage(_ context.Context, _ string, deviceIDs []string) ([]timeseries.Usage, error) {
	out := []timeseries.Usage{}
	for _, id := range deviceIDs {
		out = append(out, timeseries.Usage{DeviceId: id, Bytes: 1 << 20, BytesPerDay: 8640})
	}
	return out, nil
}

func (f *fakeTimeseries) Query(context.Context, string, []timeseries.QueryElement, timeseries.QueryOptions) ([]timeseries.QueryResult, error) {
	f.t.Error("a value was read during semantic selection, which breaks exposure tier L0")
	return nil, errors.New("no value read is permitted here")
}

type staticIndex struct{ index *profiler.OntologyIndex }

func (s staticIndex) Ontology(context.Context, string) (*profiler.OntologyIndex, error) {
	return s.index, nil
}

// --- harness ---

type harness struct {
	resolver *Resolver
	ontology *fakeOntology
	devices  *fakeDevices
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	return newHarnessWith(t, opts, true)
}

func newHarnessWith(t *testing.T, opts Options, ranked bool) *harness {
	t.Helper()

	ont := &fakeOntology{snap: testSnapshot(), answer: meterSelectables}
	dev := &fakeDevices{serve: []models.ExtendedDevice{meterDevice("device-1")}}

	var ranker Ranker
	if ranked {
		prof, err := profiler.New(&fakeTimeseries{t: t}, staticIndex{index: testIndex()},
			profiler.NewMemoryStore(), profiler.Options{Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatalf("profiler.New: %v", err)
		}
		ranker = prof
	}

	resolver, err := New(ont, staticIndex{index: testIndex()}, dev, ranker, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{resolver: resolver, ontology: ont, devices: dev}
}

func (h *harness) resolve(t *testing.T, req Request) Result {
	t.Helper()
	result, err := h.resolver.Resolve(context.Background(), testToken, req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return result
}

func criteriaPairs(sent [][]drmodel.FilterCriteria) []string {
	out := []string{}
	for _, call := range sent {
		for _, criterion := range call {
			out = append(out, criterion.FunctionId+"|"+criterion.AspectId)
		}
	}
	sort.Strings(out)
	return out
}

func hasNote(notes []string, fragment string) bool {
	for _, note := range notes {
		if strings.Contains(note, fragment) {
			return true
		}
	}
	return false
}

func paths(selectables []Selectable) []string {
	out := []string{}
	for _, s := range selectables {
		out = append(out, s.Path)
	}
	return out
}

func candidatePaths(candidates []profiler.QuickProfile) []string {
	out := []string{}
	for _, c := range candidates {
		out = append(out, c.SeriesRef.VariablePath)
	}
	return out
}

// --- the criteria decomposition ---

// The most consequential test in this package. The device repository ANDs a
// criteria list: sending two functions in one list asks for a device type
// carrying both, which for two unrelated functions is none at all — an empty
// answer that looks like an empty platform. Alternatives must therefore be
// separate requests.
func TestEachRequestCarriesExactlyOneCriterion(t *testing.T) {
	h := newHarness(t, Options{})
	h.resolve(t, Request{Intent: "power generation and temperature in the PV system and the kitchen"})

	sent := h.ontology.sentCriteria()
	if len(sent) != 4 {
		t.Fatalf("requests = %d, want 4 (two functions × two aspects)", len(sent))
	}
	for i, call := range sent {
		if len(call) != 1 {
			t.Errorf("request %d carried %d criteria, want exactly 1: a list is ANDed upstream", i, len(call))
		}
	}

	want := []string{
		"fn-power|kitchen", "fn-power|pv",
		"fn-temperature|kitchen", "fn-temperature|pv",
	}
	got := criteriaPairs(sent)
	if len(got) != len(want) {
		t.Fatalf("pairs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pairs = %v, want %v", got, want)
			break
		}
	}
}

// An empty criteria list is substituted upstream with one empty criterion, which
// matches every device type on the platform. Resolving an intent the ontology has
// no words for must therefore query nothing at all.
func TestAnUnresolvedIntentQueriesNothing(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "Photovoltaik Erzeugung"})

	if calls := h.ontology.sentCriteria(); len(calls) != 0 {
		t.Errorf("the platform was queried %d times for an unresolved intent", len(calls))
	}
	if len(h.devices.options) != 0 {
		t.Error("devices were listed for an unresolved intent")
	}
	if len(result.Criteria) != 0 || len(result.Selectables) != 0 || len(result.Candidates) != 0 {
		t.Error("an unresolved intent produced a non-empty resolution")
	}
	if !hasNote(result.Notes, "matches every device type") {
		t.Errorf("notes = %v, want the reason nothing was queried", result.Notes)
	}
	if len(result.UnmatchedTerms) != 2 {
		t.Errorf("unmatched terms = %v, want both words reported", result.UnmatchedTerms)
	}
}

// The device repository expands an aspect criterion to the node plus its
// descendants. Expanding it here as well would AND a parent with its child and
// match nothing.
func TestAspectDescendantsAreNotSentAsCriteria(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "PV system"})

	sent := h.ontology.sentCriteria()
	if len(sent) != 1 {
		t.Fatalf("requests = %d, want 1 for one aspect and no function", len(sent))
	}
	if sent[0][0].AspectId != "pv" {
		t.Errorf("aspect = %q, want pv", sent[0][0].AspectId)
	}
	for _, call := range sent {
		if call[0].AspectId == "inverter" {
			t.Error("a descendant aspect was sent as its own criterion")
		}
	}
	if len(result.MatchedAspects) != 1 || !result.MatchedAspects[0].DescendantsIncluded {
		t.Errorf("matched aspects = %+v, want one covering its subtree", result.MatchedAspects)
	}
}

func TestCriteriaCarryTheDefaultEventInteraction(t *testing.T) {
	h := newHarness(t, Options{})
	h.resolve(t, Request{Intent: "power generation kitchen"})

	sent := h.ontology.sentCriteria()
	if len(sent) == 0 {
		t.Fatal("nothing was queried")
	}
	if sent[0][0].Interaction != models.EVENT {
		t.Errorf("interaction = %q, want %q: a request-only service streams nothing",
			sent[0][0].Interaction, models.EVENT)
	}
}

func TestInteractionAnyLiftsTheFilter(t *testing.T) {
	h := newHarness(t, Options{})
	h.resolve(t, Request{Intent: "power generation kitchen", Interaction: InteractionAny})

	sent := h.ontology.sentCriteria()
	if len(sent) == 0 {
		t.Fatal("nothing was queried")
	}
	if sent[0][0].Interaction != "" {
		t.Errorf("interaction = %q, want it absent", sent[0][0].Interaction)
	}
}

func TestCriteriaAreCappedWithANote(t *testing.T) {
	h := newHarness(t, Options{MaxCriteria: 2})
	result := h.resolve(t, Request{Intent: "power generation and temperature in the PV system and the kitchen"})

	if len(result.Criteria) != 2 {
		t.Fatalf("criteria = %d, want the cap of 2", len(result.Criteria))
	}
	if len(h.ontology.sentCriteria()) != 2 {
		t.Errorf("requests = %d, want only the capped criteria to be sent", len(h.ontology.sentCriteria()))
	}
	if !hasNote(result.Notes, "dropped") {
		t.Errorf("notes = %v, want the truncation reported — a silent cap reads as completeness", result.Notes)
	}
}

// buildCriteria is unit-tested directly for the ordering, because the cap only
// makes sense if what survives it is the strongest combination.
func TestBuildCriteriaKeepsTheStrongestCombinations(t *testing.T) {
	functions := []ontology.FunctionMatch{
		{Id: "fn-weak", Matched: ontology.Matched{Score: 0.5}},
		{Id: "fn-strong", Matched: ontology.Matched{Score: 1}},
	}
	aspects := []ontology.AspectMatch{
		{Id: "as-weak", Matched: ontology.Matched{Score: 0.5}},
		{Id: "as-strong", Matched: ontology.Matched{Score: 1}},
	}

	criteria, dropped := buildCriteria(functions, aspects, nil, models.EVENT, 2)
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2 of the four combinations", dropped)
	}
	if len(criteria) != 2 {
		t.Fatalf("criteria = %v, want 2", criteria)
	}
	if criteria[0].FunctionID != "fn-strong" || criteria[0].AspectID != "as-strong" {
		t.Errorf("first = %+v, want the two strongest matches paired", criteria[0])
	}
}

func TestBuildCriteriaRefusesToAskForEverything(t *testing.T) {
	criteria, dropped := buildCriteria(nil, nil, nil, models.EVENT, 12)
	if len(criteria) != 0 || dropped != 0 {
		t.Errorf("criteria = %v, want none: an unconstrained criterion matches the whole platform", criteria)
	}
}

func TestBuildCriteriaLeavesUnresolvedDimensionsOpen(t *testing.T) {
	criteria, _ := buildCriteria(
		[]ontology.FunctionMatch{{Id: "fn-power", Matched: ontology.Matched{Score: 1}}},
		nil, nil, models.EVENT, 12)

	if len(criteria) != 1 {
		t.Fatalf("criteria = %v, want one", criteria)
	}
	if criteria[0].AspectID != "" {
		t.Errorf("aspect = %q, want it left empty so the platform does not filter on it", criteria[0].AspectID)
	}
}

// --- explicit ids ---

func TestExplicitIdsResolveWithoutAnIntent(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{FunctionIDs: []string{"fn-temperature"}})

	sent := h.ontology.sentCriteria()
	if len(sent) != 1 || sent[0][0].FunctionId != "fn-temperature" {
		t.Fatalf("criteria = %v, want the explicit function", sent)
	}
	if len(result.MatchedFunctions) != 1 ||
		result.MatchedFunctions[0].Matched.Basis != ontology.BasisExplicitID {
		t.Errorf("matched = %+v, want one explicit id", result.MatchedFunctions)
	}
	if got := paths(result.Selectables); len(got) != 1 || got[0] != temperaturePath {
		t.Errorf("selectables = %v, want only the temperature path", got)
	}
}

func TestAnExplicitIdOutsideTheSnapshotIsReportedAndUsed(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{FunctionIDs: []string{"fn-brand-new"}})

	if !hasNote(result.Notes, "fn-brand-new") {
		t.Errorf("notes = %v, want the unknown id reported", result.Notes)
	}
	sent := h.ontology.sentCriteria()
	if len(sent) != 1 || sent[0][0].FunctionId != "fn-brand-new" {
		t.Errorf("criteria = %v, want the id queried anyway: the snapshot can be stale", sent)
	}
}

// A lexically matched device class narrows by ANDing, and a wrong one empties the
// result with no error anywhere. It is reported and not used, which the notes say.
func TestAMatchedDeviceClassIsReportedButNotApplied(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "energy meter power generation kitchen"})

	if len(result.MatchedDeviceClasses) == 0 {
		t.Fatal("expected the device class to be matched")
	}
	for _, criterion := range result.Criteria {
		if criterion.DeviceClassID != "" {
			t.Errorf("criterion %+v narrowed by a lexically matched device class", criterion)
		}
	}
	if !hasNote(result.Notes, "device_class_ids") {
		t.Errorf("notes = %v, want the deliberate-narrowing hint", result.Notes)
	}
}

func TestAnExplicitDeviceClassIsApplied(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation kitchen", DeviceClassIDs: []string{"dc-meter"}})

	if len(result.Criteria) == 0 {
		t.Fatal("nothing was queried")
	}
	for _, criterion := range result.Criteria {
		if criterion.DeviceClassID != "dc-meter" {
			t.Errorf("criterion %+v, want the explicit device class applied", criterion)
		}
	}
}

// --- selectables and the join to variables ---

func TestSelectablesResolveUnitsFromTheOntology(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.Selectables) != 1 {
		t.Fatalf("selectables = %v, want one", paths(result.Selectables))
	}
	selectable := result.Selectables[0]
	if selectable.Unit != "W" {
		t.Errorf("unit = %q, want W from the characteristic", selectable.Unit)
	}
	if selectable.CharacteristicID == nil || *selectable.CharacteristicID != "ch-watt" {
		t.Errorf("characteristic = %v, want ch-watt and never fabricated", selectable.CharacteristicID)
	}
	if selectable.UnitSource != profiler.UnitFromCharacteristic {
		t.Errorf("unit source = %q, want %q", selectable.UnitSource, profiler.UnitFromCharacteristic)
	}
	if !selectable.Queryable {
		t.Errorf("queryable = false with reason %q, want a readable series", selectable.Reason)
	}
	if selectable.OntologyCompleteness.Status != profiler.CompletenessComplete {
		t.Errorf("completeness = %+v, want complete", selectable.OntologyCompleteness)
	}
}

// The same device type comes back from several criteria — matched on its function
// by one and on its aspect by another. The merge is what stops one variable being
// offered twice.
func TestTheSameDeviceTypeFromSeveralCriteriaIsMergedOnce(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation and temperature in the kitchen"})

	if len(h.ontology.sentCriteria()) != 2 {
		t.Fatalf("requests = %d, want one per function", len(h.ontology.sentCriteria()))
	}
	seen := map[string]int{}
	for _, s := range result.Selectables {
		seen[s.ServiceID+"|"+s.Path]++
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("%s appears %d times, want once", key, count)
		}
	}
	if len(result.Selectables) != 2 {
		t.Errorf("selectables = %v, want both paths once each", paths(result.Selectables))
	}
}

// The device repository indexes service inputs as well as outputs, and they look
// like any other path. An input is not a stored series, so it is reported with a
// reason rather than offered or dropped.
func TestAPathThatIsNotAnOutputIsReportedUnreadable(t *testing.T) {
	h := newHarness(t, Options{})
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		return []drmodel.DeviceTypeSelectable{{
			DeviceTypeId: meterTypeID,
			Services:     []models.Service{meterService()},
			ServicePathOptions: map[string][]drmodel.ServicePathOption{
				meterServiceID: {
					option(meterServiceID, powerPath, "ch-watt", "fn-power", "kitchen"),
					option(meterServiceID, setpointPath, "ch-watt", "fn-power", "kitchen"),
				},
			},
		}}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	var setpoint *Selectable
	for i := range result.Selectables {
		if result.Selectables[i].Path == setpointPath {
			setpoint = &result.Selectables[i]
		}
	}
	if setpoint == nil {
		t.Fatalf("selectables = %v, want the input path reported", paths(result.Selectables))
	}
	if setpoint.Queryable {
		t.Error("an input path was offered as a readable series")
	}
	if !strings.Contains(setpoint.Reason, "not a service output") {
		t.Errorf("reason = %q, want it to name the cause", setpoint.Reason)
	}
	if got := candidatePaths(result.Candidates); len(got) != 1 || got[0] != powerPath {
		t.Errorf("candidates = %v, want only the real series", got)
	}
}

// A selectable that reported the aspect it was found by while claiming no aspect
// is declared would be a document contradicting itself, and a gap nobody can act
// on. The identity the criterion matched on is reconciled before completeness is
// judged.
func TestTheMatchedIdentityIsReconciledBeforeGapsAreJudged(t *testing.T) {
	h := newHarness(t, Options{})
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		// A device type whose content variable carries no aspect, matched by a
		// criterion that names one.
		service := meterService()
		service.Outputs[0].ContentVariable.SubContentVariables[0].AspectId = ""
		return []drmodel.DeviceTypeSelectable{{
			DeviceTypeId: meterTypeID,
			Services:     []models.Service{service},
			ServicePathOptions: map[string][]drmodel.ServicePathOption{
				meterServiceID: {option(meterServiceID, powerPath, "ch-watt", "fn-power", "kitchen")},
			},
		}}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.Selectables) != 1 {
		t.Fatalf("selectables = %v, want one", paths(result.Selectables))
	}
	selectable := result.Selectables[0]
	if selectable.AspectID != "kitchen" {
		t.Errorf("aspect = %q, want the one the criterion matched on", selectable.AspectID)
	}
	if selectable.OntologyCompleteness.Status != profiler.CompletenessComplete {
		t.Errorf("completeness = %+v, want complete: the aspect is known", selectable.OntologyCompleteness)
	}
	if len(result.OntologyGaps) != 0 {
		t.Errorf("gaps = %+v, want none", result.OntologyGaps)
	}
}

// The characteristic is the one field not reconciled: it decides the unit and the
// declared range, and §5.4.11 makes the device type's own declaration the only
// authority. Adopting one from elsewhere would report a unit nothing declares.
func TestACharacteristicIsNeverAdoptedFromThePathOption(t *testing.T) {
	h := newHarness(t, Options{})
	h.devices.serve = []models.ExtendedDevice{plugDevice("device-plug")}
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		return []drmodel.DeviceTypeSelectable{{
			DeviceTypeId: plugTypeID,
			Services:     []models.Service{plugService()},
			ServicePathOptions: map[string][]drmodel.ServicePathOption{
				// The option claims a characteristic the content variable does not
				// declare.
				plugServiceID: {option(plugServiceID, wattsPath, "ch-watt", "fn-power", "kitchen")},
			},
		}}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.Selectables) != 1 {
		t.Fatalf("selectables = %v, want one", paths(result.Selectables))
	}
	if result.Selectables[0].CharacteristicID != nil {
		t.Errorf("characteristic = %v, want none: the device type declares none",
			*result.Selectables[0].CharacteristicID)
	}
	if result.Selectables[0].Unit != "" {
		t.Errorf("unit = %q, want it unknown rather than borrowed", result.Selectables[0].Unit)
	}
	if len(result.OntologyGaps) != 1 {
		t.Errorf("gaps = %+v, want the missing characteristic reported", result.OntologyGaps)
	}
}

// The device-type half of the answer is addressed by id upstream, so it is labelled
// from the devices that were listed — otherwise every table in the UI reads as a
// column of URNs.
func TestDeviceTypesAreNamedFromTheListedDevices(t *testing.T) {
	h := newHarness(t, Options{})
	device := meterDevice("device-1")
	device.DisplayName = "Kitchen Meter"
	device.DeviceTypeName = "SmartMeter Modbus"
	h.devices.serve = []models.ExtendedDevice{device}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.Selectables) != 1 {
		t.Fatalf("selectables = %v, want one", paths(result.Selectables))
	}
	if result.Selectables[0].DeviceTypeName != "SmartMeter Modbus" {
		t.Errorf("selectable device type name = %q, want the listed device's",
			result.Selectables[0].DeviceTypeName)
	}
	if len(result.CandidateDevices) != 1 {
		t.Fatalf("candidate devices = %+v, want one", result.CandidateDevices)
	}
	if result.CandidateDevices[0].Name != "Kitchen Meter" {
		t.Errorf("device name = %q, want the display name", result.CandidateDevices[0].Name)
	}
	if result.CandidateDevices[0].DeviceTypeName != "SmartMeter Modbus" {
		t.Errorf("device type name = %q, want it beside the device name",
			result.CandidateDevices[0].DeviceTypeName)
	}
	if result.Candidates[0].Device.Name != "Kitchen Meter" {
		t.Errorf("candidate device name = %q, want the display name",
			result.Candidates[0].Device.Name)
	}
}

// A gap is reported per device type, so it needs the same label as the selectables
// it summarises.
func TestOntologyGapsCarryTheDeviceTypeName(t *testing.T) {
	h := newHarness(t, Options{})
	device := plugDevice("device-plug")
	device.DeviceTypeName = "Smart Plug v2"
	h.devices.serve = []models.ExtendedDevice{device}
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		return []drmodel.DeviceTypeSelectable{{
			DeviceTypeId: plugTypeID,
			Services:     []models.Service{plugService()},
			ServicePathOptions: map[string][]drmodel.ServicePathOption{
				plugServiceID: {option(plugServiceID, wattsPath, "", "fn-power", "kitchen")},
			},
		}}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.OntologyGaps) != 1 {
		t.Fatalf("gaps = %+v, want one", result.OntologyGaps)
	}
	if result.OntologyGaps[0].DeviceTypeName != "Smart Plug v2" {
		t.Errorf("gap device type name = %q, want the listed device's",
			result.OntologyGaps[0].DeviceTypeName)
	}
}

// A device type nobody can reach a device of has no name available — reporting it as
// empty lets the reader fall back to the id rather than showing a blank.
func TestAnUnreachableDeviceTypeKeepsAnEmptyName(t *testing.T) {
	h := newHarness(t, Options{})
	h.devices.serve = []models.ExtendedDevice{}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.Selectables) == 0 {
		t.Fatal("the ontology resolution was dropped")
	}
	if result.Selectables[0].DeviceTypeName != "" {
		t.Errorf("device type name = %q, want it empty when no device carries the type",
			result.Selectables[0].DeviceTypeName)
	}
}

// --- candidates ---

// The resolution answers an intent, so a device's other forty columns are not
// part of the answer even though QuickProfiles enumerates them.
func TestCandidatesAreLimitedToTheSelectedVariables(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if got := candidatePaths(result.Candidates); len(got) != 1 || got[0] != powerPath {
		t.Errorf("candidates = %v, want only the selected variable", got)
	}
}

// A selected variable that cannot be read as a series is kept and ranked last: the
// developer asked for it, and "it exists but is a JSONB list column" is the answer.
func TestAnUnreadableSelectedVariableIsKeptAndRankedLast(t *testing.T) {
	h := newHarness(t, Options{})
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		return []drmodel.DeviceTypeSelectable{{
			DeviceTypeId: meterTypeID,
			Services:     []models.Service{meterService()},
			ServicePathOptions: map[string][]drmodel.ServicePathOption{
				meterServiceID: {
					option(meterServiceID, samplesPath, "", "fn-power", "kitchen"),
					option(meterServiceID, powerPath, "ch-watt", "fn-power", "kitchen"),
				},
			},
		}}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	got := candidatePaths(result.Candidates)
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want both the readable and the unreadable one", got)
	}
	if got[0] != powerPath || got[1] != samplesPath {
		t.Errorf("candidates = %v, want the unreadable one last", got)
	}
	if result.Candidates[1].Queryable {
		t.Error("the list column was reported as queryable")
	}
}

func TestCandidateDevicesCountTheSeriesTheyContribute(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation and temperature in the kitchen"})

	if len(result.CandidateDevices) != 1 {
		t.Fatalf("candidate devices = %+v, want one", result.CandidateDevices)
	}
	if result.CandidateDevices[0].Series != 2 {
		t.Errorf("series = %d, want 2", result.CandidateDevices[0].Series)
	}
	if !result.CandidateDevices[0].Permissions.Execute {
		t.Error("permissions did not travel with the candidate device")
	}
}

// §5.1: models.Read governs metadata, models.Execute governs reading data. A
// resolution offers series to read, so listing under Read would offer series the
// caller cannot read and fail at query time instead of here.
func TestDevicesAreListedForExecuteWithTheirDeviceType(t *testing.T) {
	h := newHarness(t, Options{})
	h.resolve(t, Request{Intent: "power generation kitchen", DeviceLimit: 25})

	if len(h.devices.options) != 1 {
		t.Fatalf("device listings = %d, want one batched call", len(h.devices.options))
	}
	options := h.devices.options[0]
	if options.Permission != models.Execute {
		t.Errorf("permission = %q, want %q", options.Permission, models.Execute)
	}
	if !options.FullDt {
		t.Error("full device type = false: the variable enumeration needs the device type")
	}
	if len(options.DeviceTypeIds) != 1 || options.DeviceTypeIds[0] != meterTypeID {
		t.Errorf("device type ids = %v, want the matched type", options.DeviceTypeIds)
	}
	if options.Limit != 25 {
		t.Errorf("limit = %d, want the requested 25", options.Limit)
	}
}

func TestDeviceLimitFallsBackToTheResolverDefault(t *testing.T) {
	h := newHarness(t, Options{DeviceLimit: 7})
	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if h.devices.options[0].Limit != 7 {
		t.Errorf("limit = %d, want the resolver default of 7", h.devices.options[0].Limit)
	}
	if result.DeviceLimit != 7 {
		t.Errorf("reported limit = %d, want 7", result.DeviceLimit)
	}
}

// --- the tier L0 claim ---

// M2's acceptance criterion, checkable from the answer: an intent resolves to
// concrete series entirely at tier L0. The value counter is part of the document,
// and the timeseries fake fails the test if a value read is even attempted.
func TestResolutionReadsNoValues(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if result.Reads.Values != 0 {
		t.Errorf("value reads = %d, want 0", result.Reads.Values)
	}
	if result.Reads.Selectables != len(result.Criteria) {
		t.Errorf("selectables reads = %d, want one per criterion (%d)",
			result.Reads.Selectables, len(result.Criteria))
	}
	if result.Reads.DeviceLists != 1 {
		t.Errorf("device lists = %d, want 1", result.Reads.DeviceLists)
	}
	if result.Reads.Availability != 1 {
		t.Errorf("availability reads = %d, want one per device", result.Reads.Availability)
	}
	if result.Reads.Usage != 1 {
		t.Errorf("usage reads = %d, want one batched call", result.Reads.Usage)
	}
	if len(result.Candidates) == 0 {
		t.Error("no candidate series: the resolution proved nothing")
	}
}

func TestSkipRankingCostsNoPerDeviceRead(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation and temperature in the kitchen", SkipRanking: true})

	if result.Reads.Availability != 0 || result.Reads.Usage != 0 {
		t.Errorf("reads = %+v, want no per-device reads when ranking is skipped", result.Reads)
	}
	// The series count comes from the selection, not from the ranking: zero here
	// would say this device contributes nothing, which is a different claim from
	// "nothing was ranked".
	if len(result.CandidateDevices) != 1 || result.CandidateDevices[0].Series != 2 {
		t.Errorf("candidate devices = %+v, want the two selected variables counted",
			result.CandidateDevices)
	}
	if len(result.Candidates) != 0 {
		t.Errorf("candidates = %v, want none", candidatePaths(result.Candidates))
	}
	if len(result.Selectables) == 0 || len(result.CandidateDevices) == 0 {
		t.Error("skipping the ranking dropped the ontology resolution too")
	}
	if !hasNote(result.Notes, "skipped on request") {
		t.Errorf("notes = %v, want the skip reported", result.Notes)
	}
}

// A deployment without a timescale-wrapper URL has no profiler. The ontology half
// of this answer still stands, and the missing order is stated rather than
// silently absent.
func TestWithoutARankerTheResolutionStillResolves(t *testing.T) {
	h := newHarnessWith(t, Options{}, false)
	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.Selectables) == 0 {
		t.Error("no selectables without a ranker")
	}
	if len(result.Candidates) != 0 {
		t.Error("candidates appeared without a ranker")
	}
	if !hasNote(result.Notes, "ranking is unavailable") {
		t.Errorf("notes = %v, want the missing ranking reported", result.Notes)
	}
}

// --- ontology gaps (D16) ---

func TestOntologyGapsGroupByDeviceTypeAndConsequence(t *testing.T) {
	h := newHarness(t, Options{})
	h.devices.serve = []models.ExtendedDevice{plugDevice("device-plug")}
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		return []drmodel.DeviceTypeSelectable{{
			DeviceTypeId: plugTypeID,
			Services:     []models.Service{plugService()},
			ServicePathOptions: map[string][]drmodel.ServicePathOption{
				plugServiceID: {
					option(plugServiceID, wattsPath, "", "fn-power", "kitchen"),
					option(plugServiceID, ampsPath, "ch-unknown-to-the-snapshot", "fn-power", "kitchen"),
				},
			},
		}}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.OntologyGaps) != 2 {
		t.Fatalf("gaps = %+v, want one per distinct consequence", result.OntologyGaps)
	}
	for _, gap := range result.OntologyGaps {
		if gap.DeviceTypeID != plugTypeID {
			t.Errorf("gap device type = %q, want %q", gap.DeviceTypeID, plugTypeID)
		}
		if gap.Consequence == "" {
			t.Error("a gap with no consequence reads as no consequence")
		}
		if len(gap.Missing) == 0 || len(gap.Paths) == 0 {
			t.Errorf("gap = %+v, want both what is missing and where", gap)
		}
	}

	// The variable with no characteristic at all loses unit conversion and the
	// declared range check; the one whose characteristic the ontology does not know
	// loses only the unit.
	byPath := map[string]OntologyGap{}
	for _, gap := range result.OntologyGaps {
		byPath[gap.Paths[0]] = gap
	}
	watts, found := byPath[wattsPath]
	if !found {
		t.Fatalf("gaps = %+v, want one for %s", result.OntologyGaps, wattsPath)
	}
	if !contains(watts.Missing, "characteristic_id") || !contains(watts.Missing, "unit") {
		t.Errorf("missing = %v, want both the characteristic and the unit", watts.Missing)
	}
	if !strings.Contains(watts.Consequence, "conversion") {
		t.Errorf("consequence = %q, want the conversion loss named", watts.Consequence)
	}
}

// The gaps must be the same judgement the candidate reports, not a second
// vocabulary for the same fact.
func TestGapsAgreeWithTheCandidateCompleteness(t *testing.T) {
	h := newHarness(t, Options{})
	h.devices.serve = []models.ExtendedDevice{plugDevice("device-plug")}
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		return []drmodel.DeviceTypeSelectable{{
			DeviceTypeId: plugTypeID,
			Services:     []models.Service{plugService()},
			ServicePathOptions: map[string][]drmodel.ServicePathOption{
				plugServiceID: {option(plugServiceID, wattsPath, "", "fn-power", "kitchen")},
			},
		}}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.Candidates) != 1 || len(result.OntologyGaps) != 1 {
		t.Fatalf("candidates = %d, gaps = %d, want one each",
			len(result.Candidates), len(result.OntologyGaps))
	}
	candidate := result.Candidates[0].OntologyCompleteness
	gap := result.OntologyGaps[0]
	if candidate.Consequence != gap.Consequence {
		t.Errorf("consequence differs between the candidate (%q) and the gap (%q)",
			candidate.Consequence, gap.Consequence)
	}
	for _, missing := range candidate.Missing {
		if !contains(gap.Missing, missing) {
			t.Errorf("gap misses %q, which the candidate reports", missing)
		}
	}
}

func TestACompleteDeviceTypeReportsNoGaps(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "power generation kitchen"})

	if len(result.OntologyGaps) != 0 {
		t.Errorf("gaps = %+v, want none for a fully declared variable", result.OntologyGaps)
	}
}

// --- failure handling ---

// A resolution silently missing one function's device types is worse than an
// error: nothing in the document could honestly say "partially answered".
func TestASelectablesFailureFailsTheResolution(t *testing.T) {
	h := newHarness(t, Options{})
	h.ontology.err = errors.New("upstream is down")

	if _, err := h.resolver.Resolve(context.Background(), testToken,
		Request{Intent: "power generation kitchen"}); err == nil {
		t.Fatal("expected the resolution to fail")
	}
}

func TestADeviceListingFailureFailsTheResolution(t *testing.T) {
	h := newHarness(t, Options{})
	h.devices.err = errors.New("forbidden")

	if _, err := h.resolver.Resolve(context.Background(), testToken,
		Request{Intent: "power generation kitchen"}); err == nil {
		t.Fatal("expected the resolution to fail")
	}
}

func TestNoMatchingDeviceIsReportedRatherThanEmpty(t *testing.T) {
	h := newHarness(t, Options{})
	h.devices.serve = []models.ExtendedDevice{}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})
	if len(result.Selectables) == 0 {
		t.Error("the ontology resolution was dropped along with the devices")
	}
	if !hasNote(result.Notes, "execute permission") {
		t.Errorf("notes = %v, want the permission cause named", result.Notes)
	}
}

func TestNoMatchingDeviceTypeIsReported(t *testing.T) {
	h := newHarness(t, Options{})
	h.ontology.answer = func(drmodel.FilterCriteria) []drmodel.DeviceTypeSelectable {
		return []drmodel.DeviceTypeSelectable{}
	}

	result := h.resolve(t, Request{Intent: "power generation kitchen"})
	if len(result.Criteria) == 0 {
		t.Error("the criteria were not reported")
	}
	if len(h.devices.options) != 0 {
		t.Error("devices were listed although no device type matched")
	}
	if !hasNote(result.Notes, "no device type") {
		t.Errorf("notes = %v, want the empty match reported", result.Notes)
	}
}

// An availability failure is not fatal: the profiler reports volume and windows as
// not_computed, and a candidate list without them still beats no answer.
func TestAnAvailabilityFailureStillYieldsCandidates(t *testing.T) {
	ont := &fakeOntology{snap: testSnapshot(), answer: meterSelectables}
	dev := &fakeDevices{serve: []models.ExtendedDevice{meterDevice("device-1")}}
	prof, err := profiler.New(&fakeTimeseries{t: t, availErr: errors.New("no availability")},
		staticIndex{index: testIndex()}, profiler.NewMemoryStore(),
		profiler.Options{Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("profiler.New: %v", err)
	}
	resolver, err := New(ont, staticIndex{index: testIndex()}, dev, prof, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := resolver.Resolve(context.Background(), testToken, Request{Intent: "power generation kitchen"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Errorf("candidates = %v, want the series despite the availability failure",
			candidatePaths(result.Candidates))
	}
}

func TestASnapshotFailureFailsTheResolution(t *testing.T) {
	h := newHarness(t, Options{})
	h.ontology.snapErr = errors.New("device repository is down")

	if _, err := h.resolver.Resolve(context.Background(), testToken,
		Request{Intent: "power generation kitchen"}); err == nil {
		t.Fatal("expected the resolution to fail without an ontology")
	}
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := New(nil, staticIndex{}, &fakeDevices{}, nil, Options{}); err == nil {
		t.Error("expected an error without an ontology")
	}
	if _, err := New(&fakeOntology{}, nil, &fakeDevices{}, nil, Options{}); err == nil {
		t.Error("expected an error without an ontology index")
	}
	if _, err := New(&fakeOntology{}, staticIndex{}, nil, nil, Options{}); err == nil {
		t.Error("expected an error without a device lister")
	}
}

// --- shape ---

// Empty rather than nil, for the same reason D24 gives for fields: a consumer
// iterating the answer should not have to tell "none" from "not a list".
func TestResultListsAreNeverNil(t *testing.T) {
	h := newHarness(t, Options{})
	result := h.resolve(t, Request{Intent: "nothing the ontology knows about"})

	if result.Terms == nil || result.UnmatchedTerms == nil ||
		result.MatchedFunctions == nil || result.MatchedAspects == nil ||
		result.MatchedDeviceClasses == nil || result.Criteria == nil ||
		result.Selectables == nil || result.CandidateDevices == nil ||
		result.OntologyGaps == nil || result.Candidates == nil ||
		result.Skipped == nil || result.Notes == nil {
		t.Errorf("a list arrived as nil: %+v", result)
	}
}

func TestResolutionIsStableAcrossCalls(t *testing.T) {
	h := newHarness(t, Options{})
	first := h.resolve(t, Request{Intent: "power generation and temperature in the kitchen"})
	for range 5 {
		again := h.resolve(t, Request{Intent: "power generation and temperature in the kitchen"})
		if strings.Join(paths(again.Selectables), ",") != strings.Join(paths(first.Selectables), ",") {
			t.Fatalf("selectable order changed: %v then %v", paths(first.Selectables), paths(again.Selectables))
		}
		if strings.Join(candidatePaths(again.Candidates), ",") !=
			strings.Join(candidatePaths(first.Candidates), ",") {
			t.Fatalf("candidate order changed: %v then %v",
				candidatePaths(first.Candidates), candidatePaths(again.Candidates))
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
