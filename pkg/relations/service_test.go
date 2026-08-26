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

package relations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// --- platform stand-ins ---

// fakeTimeseries answers a grouped read by *evaluating* the fixture signal over the
// window and bucket each element asks for, rather than replaying a canned response.
// A fake that ignored the request could not show that the members were asked for on
// one grid, which is the property Align exists to establish.
type fakeTimeseries struct {
	mux      sync.Mutex
	calls    int
	elements [][]timeseries.QueryElement
	token    string
	err      error
	// signal answers with a value and whether the bucket carried one at all.
	signal func(deviceID, path string, at time.Time) (float64, bool)
}

func (f *fakeTimeseries) Query(
	_ context.Context, token string, elements []timeseries.QueryElement, _ timeseries.QueryOptions,
) ([]timeseries.QueryResult, error) {
	f.mux.Lock()
	f.calls++
	f.token = token
	f.elements = append(f.elements, elements)
	f.mux.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	out := make([]timeseries.QueryResult, 0, len(elements))
	for index, element := range elements {
		step := 15 * time.Minute
		if element.GroupTime != nil {
			if seconds := timeseries.BucketSeconds(*element.GroupTime); seconds > 0 {
				step = time.Duration(seconds) * time.Second
			}
		}
		from, to := fixtureStart, fixtureStart.Add(testDays*24*time.Hour)
		if element.Time != nil && element.Time.Start != nil && element.Time.End != nil {
			if parsed, err := time.Parse(time.RFC3339, *element.Time.Start); err == nil {
				from = parsed
			}
			if parsed, err := time.Parse(time.RFC3339, *element.Time.End); err == nil {
				to = parsed
			}
		}

		// One sub-series per requested column, in request order — the shape
		// /queries/v2 produces for a device and service, and the one DecodeResults
		// insists on.
		data := make([][][]any, 0, len(element.Columns))
		for _, column := range element.Columns {
			rows := [][]any{}
			for at := from; at.Before(to); at = at.Add(step) {
				value, present := f.signal(*element.DeviceId, column.Name, at)
				if !present {
					continue
				}
				rows = append(rows, []any{
					at.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
					json.Number(strconv.FormatFloat(value, 'f', -1, 64)),
				})
			}
			data = append(data, rows)
		}
		out = append(out, timeseries.QueryResult{
			RequestIndex: index,
			DeviceId:     element.DeviceId,
			ServiceId:    element.ServiceId,
			ColumnNames:  []string{element.Columns[0].Name},
			Data:         data,
		})
	}
	return out, nil
}

// kitchenSignal is the fixture of relate_test.go as a series: the oven runs every
// evening and briefly every morning, the lights only in the evening.
func kitchenSignal(deviceID, _ string, at time.Time) (float64, bool) {
	evening := at.Hour() >= 19 && at.Hour() < 22
	morning := at.Hour() == 10 && at.Minute() < 30

	switch deviceID {
	case "dev-oven":
		if evening || morning {
			return 2000, true
		}
		return 5, true
	default:
		if evening {
			return 60, true
		}
		return 3, true
	}
}

type fakeDevices struct {
	mux     sync.Mutex
	actions []drmodel.AuthAction
	token   string
	err     error
}

func (f *fakeDevices) Get(token string, id string, action drmodel.AuthAction) (models.ExtendedDevice, error) {
	f.mux.Lock()
	f.token = token
	f.actions = append(f.actions, action)
	f.mux.Unlock()
	if f.err != nil {
		return models.ExtendedDevice{}, f.err
	}
	name := "Kitchen lights"
	service := "svc-lights"
	if id == "dev-oven" {
		name, service = "Oven", "svc-oven"
	}
	return models.ExtendedDevice{
		Device:      models.Device{Id: id, Name: name, DeviceTypeId: "dt-plug"},
		Permissions: models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: "dt-plug", Name: "Smart plug",
			Services: []models.Service{{
				Id: service, Name: "readings", Interaction: models.EVENT,
				// The output matters: a graph reaches devices the aspect resolution never
				// saw, and their series are enumerated from the device type rather than
				// from a selectables answer. A fake without outputs would make that path
				// look broken when it is the fake that is empty.
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: "value", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{{
							Id: "cv-power", Name: "power", Type: models.Float,
							CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectId: "kitchen",
						}},
					},
				}},
			}},
		},
	}, nil
}

// fakeProfiler answers with a constructed profile per member rather than running the
// real detectors. The profiler has its own fixture suite; what matters here is what
// the relational pass does with an activity_pattern, so the pattern is the input.
type fakeProfiler struct {
	mux   sync.Mutex
	calls []string
	err   error
	// thresholds is per device, because a detected idle/active split is a property of
	// the series: the oven draws two kilowatts and the lights sixty watts, and one
	// shared threshold would make the lights permanently idle.
	thresholds map[string]float64
	threshold  float64
	interval   float64
	// classification, when set, replaces session_based for every profile.
	classification profiler.ActivityClassification
	// overrides is merged into each resolved profile's resolution map.
	overrides map[string]profiler.Resolution
}

func (f *fakeProfiler) ProfileService(
	_ context.Context, _ string, req profiler.ProfileRequest,
) (profiler.ProfileResult, error) {
	f.mux.Lock()
	f.calls = append(f.calls, req.Device.Id+"|"+req.ServiceID)
	f.mux.Unlock()
	if f.err != nil {
		return profiler.ProfileResult{}, f.err
	}

	classification := f.classification
	if classification == "" {
		classification = profiler.ActivitySessionBased
	}
	resolution := map[string]profiler.Resolution{}
	for path, override := range f.overrides {
		resolution[path] = override
	}

	threshold := f.threshold
	if named, found := f.thresholds[req.Device.Id]; found {
		threshold = named
	}

	profile := profiler.SeriesProfile{
		ProfileID: "prof-" + req.Device.Id,
		SeriesRef: profiler.SeriesRef{
			DeviceID: req.Device.Id, ServiceID: req.ServiceID, VariablePath: "value.power",
		},
		ValueSemantics: profiler.ValueSemantics{
			Kind: profiler.Computed(profiler.KindInstantaneous),
			Unit: "W",
		},
		Sampling: profiler.Computed(profiler.Sampling{DetectedIntervalS: f.interval}),
		ActivityPattern: profiler.Computed(profiler.ActivityPattern{
			Classification:  classification,
			IdleLevel:       5,
			ActiveThreshold: threshold,
			ThresholdMethod: "otsu",
			ThresholdParams: profiler.SessionParams{HysteresisFrac: 0.1},
		}),
	}
	return profiler.ProfileResult{
		Profiles: []profiler.ResolvedProfile{{SeriesProfile: profile, Resolution: resolution}},
		Reads:    profiler.ReadCounts{Values: 2},
	}, nil
}

type fakeOntology struct {
	mux        sync.Mutex
	nodes      []models.AspectNode
	groups     []models.DeviceGroup
	groupCalls []ontology.DeviceGroupOptions
	groupErr   error

	graphs     []models.Graph
	graphCalls []ontology.GraphOptions
	graphErr   error
}

func (f *fakeOntology) Snapshot(context.Context, string) (*ontology.Snapshot, error) {
	return &ontology.Snapshot{AspectNodes: f.nodes}, nil
}

func (f *fakeOntology) ListDeviceGroups(
	_ context.Context, _ string, opts ontology.DeviceGroupOptions,
) ([]models.DeviceGroup, error) {
	f.mux.Lock()
	f.groupCalls = append(f.groupCalls, opts)
	f.mux.Unlock()
	if f.groupErr != nil {
		return nil, f.groupErr
	}
	return f.groups, nil
}

func (f *fakeOntology) ListGraphs(
	_ context.Context, _ string, opts ontology.GraphOptions,
) ([]models.Graph, error) {
	f.mux.Lock()
	f.graphCalls = append(f.graphCalls, opts)
	f.mux.Unlock()
	if f.graphErr != nil {
		return nil, f.graphErr
	}
	return f.graphs, nil
}

type fakeSelection struct {
	mux      sync.Mutex
	requests []selection.Request
	result   selection.Result
	err      error
}

func (f *fakeSelection) Resolve(
	_ context.Context, _ string, req selection.Request,
) (selection.Result, error) {
	f.mux.Lock()
	f.requests = append(f.requests, req)
	f.mux.Unlock()
	if f.err != nil {
		return selection.Result{}, f.err
	}
	return f.result, nil
}

type sequentialIDs struct {
	mux sync.Mutex
	n   int
}

func (s *sequentialIDs) NewID() string {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.n++
	return fmt.Sprintf("dec-%d", s.n)
}

// --- harness ---

type harness struct {
	service    *Service
	timeseries *fakeTimeseries
	devices    *fakeDevices
	profiler   *fakeProfiler
	ontology   *fakeOntology
	selection  *fakeSelection
	store      Store
}

var fixtureNow = fixtureStart.Add(testDays * 24 * time.Hour)

func newHarness(t *testing.T, adjust ...func(*Deps)) *harness {
	t.Helper()

	h := &harness{
		timeseries: &fakeTimeseries{signal: kitchenSignal},
		devices:    &fakeDevices{},
		profiler: &fakeProfiler{
			threshold:  100,
			thresholds: map[string]float64{"dev-oven": 100, "dev-lights": 30},
			interval:   900,
		},
		ontology: &fakeOntology{nodes: []models.AspectNode{
			{Id: "building", Name: "Building", RootId: "building"},
			{Id: "kitchen", Name: "Kitchen", ParentId: "building", RootId: "building"},
			{Id: "kitchen-ceiling", Name: "Kitchen Ceiling", ParentId: "kitchen", RootId: "building"},
		}},
		selection: &fakeSelection{result: kitchenResolution()},
		store:     NewMemoryStore(0),
	}

	deps := Deps{
		Timeseries:    h.timeseries,
		Devices:       h.devices,
		Ontology:      h.ontology,
		Selection:     h.selection,
		Profiler:      h.profiler,
		OntologyIndex: staticIndex{index: wattIndex()},
		Store:         h.store,
		IDs:           &sequentialIDs{},
		Now:           func() time.Time { return fixtureNow },
	}
	for _, apply := range adjust {
		apply(&deps)
	}
	h.store = deps.Store

	service, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.service = service
	return h
}

// staticIndex answers with one fixed ontology index, which is all a graph
// neighbour's unit resolution needs.
type staticIndex struct{ index *profiler.OntologyIndex }

func (s staticIndex) Ontology(context.Context, string) (*profiler.OntologyIndex, error) {
	return s.index, nil
}

func wattIndex() *profiler.OntologyIndex {
	watt := models.Characteristic{Id: "ch-watt", Name: "Watt", DisplayUnit: "W"}
	return profiler.NewOntologyIndex(
		[]models.Characteristic{watt},
		[]models.ConceptWithCharacteristics{{
			Id: "concept-power", BaseCharacteristicId: "ch-watt",
			Characteristics: []models.Characteristic{watt},
		}},
		[]models.Function{{Id: "fn-power", ConceptId: "concept-power"}},
	)
}

func kitchenMembers() []SeriesMember {
	return []SeriesMember{
		{Ref: ovenRef(), Label: "the oven"},
		{Ref: lightsRef(), Label: "the kitchen lights"},
	}
}

func fixtureWindow() profiler.Window {
	return profiler.Window{From: fixtureStart, To: fixtureNow}
}

// kitchenResolution is what pkg/selection answers for the aspect "Kitchen": one
// device type with a power variable, and two devices of it.
func kitchenResolution() selection.Result {
	characteristic := "ch-watt"
	return selection.Result{
		Selectables: []selection.Selectable{{
			DeviceTypeID:     "dt-plug",
			DeviceTypeName:   "Smart plug",
			ServiceID:        "svc-oven",
			ServiceName:      "readings",
			Path:             "value.power",
			CharacteristicID: &characteristic,
			Unit:             "W",
			UnitSource:       profiler.UnitFromCharacteristic,
			FunctionID:       "fn-power",
			AspectID:         "kitchen",
			AspectName:       "Kitchen",
			Queryable:        true,
		}},
		CandidateDevices: []selection.CandidateDevice{
			{DeviceID: "dev-oven", Name: "Oven", DeviceTypeID: "dt-plug", DeviceTypeName: "Smart plug",
				ConnectionState: models.ConnectionStateOnline},
			{DeviceID: "dev-lights", Name: "Kitchen lights", DeviceTypeID: "dt-plug",
				DeviceTypeName: "Smart plug", ConnectionState: models.ConnectionStateOnline},
		},
		OntologyGaps: []selection.OntologyGap{},
		Notes:        []string{},
	}
}

// --- Relate, end to end ---

func TestARelationalPassProducesTheOvenAndLightsRule(t *testing.T) {
	h := newHarness(t)

	profile, err := h.service.Relate(context.Background(), "bearer-token", Request{
		Members: kitchenMembers(),
		Window:  fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}

	if profile.RelationID == "" || profile.RelationID != profile.CacheKey {
		t.Errorf("relation_id %q and cache_key %q should be the same value",
			profile.RelationID, profile.CacheKey)
	}
	if profile.Tier != TierRelation {
		t.Errorf("tier = %q, want %q", profile.Tier, TierRelation)
	}
	if profile.GridSeconds != 900 {
		t.Errorf("grid = %vs, want 900 from the members' sampling interval", profile.GridSeconds)
	}

	rule, found := findRule(profile.CandidateRules, "the oven", "the kitchen lights", StateActive)
	if !found {
		t.Fatalf("the rule did not surface; got %v", statements(profile.CandidateRules))
	}
	if len(rule.Exceptions) == 0 {
		t.Error("the rule carries no exception, but the morning run is one")
	}

	// The read claim of §5.5: one batched query for the alignment, whatever the member
	// count. The profile passes are counted separately because only the first figure is
	// this package's own.
	if profile.Reads.Aligned != 1 {
		t.Errorf("aligned reads = %d, want exactly 1", profile.Reads.Aligned)
	}
	if profile.Reads.Profiles != 4 {
		t.Errorf("profile reads = %d, want 4 (two passes for each of two services)",
			profile.Reads.Profiles)
	}
	if profile.Reads.Values != profile.Reads.Aligned+profile.Reads.Profiles {
		t.Errorf("values = %d, want the sum of %d and %d",
			profile.Reads.Values, profile.Reads.Aligned, profile.Reads.Profiles)
	}
	// The device reads are metadata and are counted apart from the values, one per
	// service the pass profiled.
	if profile.Reads.Devices != 2 {
		t.Errorf("device reads = %d, want one per service", profile.Reads.Devices)
	}

	// Every read is on behalf of the caller (D5).
	if h.timeseries.token != "bearer-token" || h.devices.token != "bearer-token" {
		t.Errorf("tokens: timeseries %q devices %q, want the caller's",
			h.timeseries.token, h.devices.token)
	}
	// Execute, not Read: the pass is about to read the devices' data (§5.1).
	for _, action := range h.devices.actions {
		if action != models.Execute {
			t.Errorf("device read under %v, want Execute", action)
		}
	}
}

// Alignment is a property of the request. Two elements with different bucket widths
// would produce two sets of boundaries and every co-occurrence after that would be
// an artefact of the mismatch.
func TestAlignmentIsOneBatchedQueryWithOneBucketForEveryMember(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(),
		Window:  fixtureWindow(),
	}); err != nil {
		t.Fatalf("Relate: %v", err)
	}

	// The profiler is a fake here, so the only query this suite issues is the aligned
	// one.
	if h.timeseries.calls != 1 {
		t.Fatalf("queries = %d, want 1", h.timeseries.calls)
	}
	elements := h.timeseries.elements[0]
	if len(elements) != 2 {
		t.Fatalf("elements = %d, want one per (device, service)", len(elements))
	}

	buckets := map[string]bool{}
	for _, element := range elements {
		if element.GroupTime == nil {
			t.Fatal("an element carried no groupTime; the buckets would not line up")
		}
		buckets[*element.GroupTime] = true
		if element.Time == nil || element.Time.Start == nil || element.Time.End == nil {
			t.Error("an element carried no time range")
		}
	}
	if len(buckets) != 1 {
		t.Errorf("the elements asked for %d different buckets, want 1: %v", len(buckets), buckets)
	}
}

// Two members of one service cost one element and one profile pass between them,
// which is the same economy §5.4.1 applies to a service-scoped batch.
func TestMembersOfOneServiceShareAnElementAndAProfilePass(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Relate(context.Background(), "token", Request{
		Members: []SeriesMember{
			{Ref: profiler.SeriesRef{DeviceID: "dev-oven", ServiceID: "svc-oven", VariablePath: "value.power"}},
			{Ref: profiler.SeriesRef{DeviceID: "dev-oven", ServiceID: "svc-oven", VariablePath: "value.energy"}},
		},
		Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}

	if len(h.profiler.calls) != 1 {
		t.Errorf("profile passes = %d, want 1 for two variables of one service: %v",
			len(h.profiler.calls), h.profiler.calls)
	}
	elements := h.timeseries.elements[0]
	if len(elements) != 1 {
		t.Fatalf("elements = %d, want 1", len(elements))
	}
	if len(elements[0].Columns) != 2 {
		t.Errorf("columns = %d, want both variables in the one element", len(elements[0].Columns))
	}
}

// A rule about a meter is a rule about its rate, and the differencing is the
// platform's work rather than ODE's (§5.3.1).
func TestACumulativeCounterIsDifferencedByTheServer(t *testing.T) {
	h := newHarness(t)
	h.profiler.mux.Lock()
	h.profiler.mux.Unlock()

	requests := []alignRequest{
		{Ref: ovenRef(), Kind: profiler.KindCumulativeCounter},
		{Ref: lightsRef(), Kind: profiler.KindInstantaneous},
	}
	frame, err := h.service.Align(context.Background(), "token", requests, fixtureWindow(), 900)
	if err != nil {
		t.Fatalf("Align: %v", err)
	}

	if frame.Columns[0].GroupType != timeseries.GroupDiffMean {
		t.Errorf("a counter was read as %q, want %q",
			frame.Columns[0].GroupType, timeseries.GroupDiffMean)
	}
	if frame.Columns[1].GroupType != timeseries.GroupMean {
		t.Errorf("an instantaneous series was read as %q, want %q",
			frame.Columns[1].GroupType, timeseries.GroupMean)
	}
}

// Rows are matched to buckets by their timestamp, not by position. A device that was
// offline for a week returns fewer rows, and pairing positionally would slide one
// member's week against the other's.
func TestAMissingRowLeavesItsBucketUnobservedRatherThanShiftingTheRest(t *testing.T) {
	h := newHarness(t)
	h.timeseries.signal = func(deviceID, path string, at time.Time) (float64, bool) {
		// The lights report nothing for the first ten days.
		if deviceID == "dev-lights" && at.Before(fixtureStart.Add(10*24*time.Hour)) {
			return 0, false
		}
		return kitchenSignal(deviceID, path, at)
	}

	frame, err := h.service.Align(context.Background(), "token", []alignRequest{
		{Ref: ovenRef(), Kind: profiler.KindInstantaneous},
		{Ref: lightsRef(), Kind: profiler.KindInstantaneous},
	}, fixtureWindow(), 900)
	if err != nil {
		t.Fatalf("Align: %v", err)
	}

	gap := int(10 * 24 * time.Hour / (15 * time.Minute))
	for i := 0; i < gap; i++ {
		if frame.Columns[1].Present[i] {
			t.Fatalf("bucket %d of the lights is marked present, but nothing was reported for it", i)
		}
	}
	if !frame.Columns[1].Present[gap] {
		t.Fatalf("bucket %d of the lights is missing, but reporting resumed there", gap)
	}
	// The oven is unaffected: its own rows land in its own buckets.
	if !frame.Columns[0].Present[0] {
		t.Error("the oven's first bucket is missing; one member's gap moved another's data")
	}
	// And the evening pattern still lines up after the gap, which is what positional
	// pairing would have destroyed.
	evening := frame.Times[gap]
	for offset, at := range frame.Times[gap:] {
		if at.Hour() == 20 {
			evening = at
			if frame.Columns[1].Values[gap+offset] < 50 {
				t.Errorf("at %v the lights read %v, want the evening value: the columns are out of step",
					evening, frame.Columns[1].Values[gap+offset])
			}
			break
		}
	}
}

func TestARelationalPassNeedsTwoDistinctMembers(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Relate(context.Background(), "token", Request{
		Members: []SeriesMember{{Ref: ovenRef()}},
		Window:  fixtureWindow(),
	})
	if !errors.Is(err, ErrTooFewMembers) {
		t.Errorf("one member gave %v, want ErrTooFewMembers", err)
	}

	// The same series twice is one member, not two: relating a series to itself would
	// answer at confidence 1.0 and mean nothing.
	_, err = h.service.Relate(context.Background(), "token", Request{
		Members: []SeriesMember{{Ref: ovenRef()}, {Ref: ovenRef()}},
		Window:  fixtureWindow(),
	})
	if !errors.Is(err, ErrTooFewMembers) {
		t.Errorf("the same series twice gave %v, want ErrTooFewMembers", err)
	}
}

func TestTooManyMembersIsRefusedBeforeAnythingIsRead(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.MaxMembers = 2 })

	members := append(kitchenMembers(), SeriesMember{Ref: profiler.SeriesRef{
		DeviceID: "dev-fridge", ServiceID: "svc-fridge", VariablePath: "value.power",
	}})
	_, err := h.service.Relate(context.Background(), "token", Request{
		Members: members, Window: fixtureWindow(),
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if h.timeseries.calls != 0 || len(h.profiler.calls) != 0 {
		t.Errorf("the platform was read %d/%d times for a request that was refused",
			h.timeseries.calls, len(h.profiler.calls))
	}
}

func TestAHalfSpecifiedWindowIsRefusedRatherThanCompleted(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(),
		Window:  profiler.Window{From: fixtureStart},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNoWindowMeansTheDefaultLookback(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.DefaultLookback = 7 * 24 * time.Hour })

	profile, err := h.service.Relate(context.Background(), "token", Request{Members: kitchenMembers()})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if !profile.Window.To.Equal(fixtureNow) {
		t.Errorf("window to = %v, want now", profile.Window.To)
	}
	if want := fixtureNow.Add(-7 * 24 * time.Hour); !profile.Window.From.Equal(want) {
		t.Errorf("window from = %v, want %v", profile.Window.From, want)
	}
}

// A caller-supplied grid used to skip chooseGrid and its MaxBuckets widening
// entirely, leaving only a note behind. Align checks nothing but that the grid is
// positive, and grid() allocates one time.Time per bucket before the first read —
// so `{"window": 26 years, "grid_seconds": 1}` over POST /relations was an
// allocation of 820 million elements, about 19 GB, made before a single query went
// out. The bound has to hold for an override for the same reason it holds for a
// derived grid.
func TestACallerSuppliedGridIsRoundedOntoTheLadderAndBoundedByMaxBuckets(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.MaxBuckets = 30 })
	window := profiler.Window{From: fixtureStart, To: fixtureStart.Add(time.Hour)}

	profile, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: window, GridSeconds: 1,
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}

	// One second is not on the ladder, and 60s — the step it rounds up to — still
	// puts 60 buckets in an hour where 30 are allowed, so the widening moves it on
	// to 300s.
	if profile.GridSeconds != 300 {
		t.Errorf("grid = %vs, want 300 — the requested 1s rounded onto the ladder and widened to fit",
			profile.GridSeconds)
	}
	if profile.Buckets > 30 {
		t.Errorf("buckets = %d, want no more than the 30 this deployment allows", profile.Buckets)
	}

	// A developer who asked for one-second buckets and silently got five-minute ones
	// is worse off than one who is told: every count in the document is per bucket.
	if !containsSubstring(profile.Notes, "300") || !containsSubstring(profile.Notes, "1s") {
		t.Errorf("notes = %v, want one naming both the requested 1s grid and the 300s the pass ran at",
			profile.Notes)
	}
}

// Beyond this span even the coarsest bucket on the ladder cannot fit the window
// into MaxBuckets, so no grid choice can bound the allocation and the window
// itself has to be refused. Refused rather than shortened, for the reason the grid
// is widened rather than the window truncated: a truncated read looks like the
// whole window.
func TestAWindowTooWideForTheCoarsestBucketIsRefusedBeforeAnythingIsRead(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.MaxBuckets = 60 })

	// 60 buckets of the ladder's widest 43200s step cover 30 days exactly.
	_, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(),
		Window:  profiler.Window{From: fixtureStart, To: fixtureStart.Add(31 * 24 * time.Hour)},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if h.timeseries.calls != 0 || len(h.profiler.calls) != 0 || len(h.devices.actions) != 0 {
		t.Errorf("the platform was read %d/%d/%d times for a window that was refused",
			h.timeseries.calls, len(h.profiler.calls), len(h.devices.actions))
	}

	// The bound is the widest window that can still be aligned, not a round number
	// below it: 30 days at the same cap is computed rather than refused.
	fits := newHarness(t, func(d *Deps) { d.MaxBuckets = 60 })
	profile, err := fits.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(),
		Window:  profiler.Window{From: fixtureStart, To: fixtureStart.Add(30 * 24 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("a window exactly at the bound was refused: %v", err)
	}
	if profile.Buckets != 60 {
		t.Errorf("buckets = %d, want the 60 the cap allows", profile.Buckets)
	}
}

// One service failing to profile must not take the finding with it: the
// oven-and-lights rule does not depend on the third device having usable data.
func TestOneUnprofilableServiceLeavesTheOtherMembersRelated(t *testing.T) {
	h := newHarness(t)
	h.profiler.err = nil

	// A third member whose service cannot be profiled.
	failing := SeriesMember{Ref: profiler.SeriesRef{
		DeviceID: "dev-fridge", ServiceID: "svc-fridge", VariablePath: "value.power",
	}}
	h.profiler = &fakeProfiler{
		threshold:  100,
		thresholds: map[string]float64{"dev-oven": 100, "dev-lights": 30},
		interval:   900,
	}
	service, err := New(Deps{
		Timeseries: h.timeseries, Devices: h.devices, Ontology: h.ontology,
		Selection: h.selection, Store: h.store, IDs: &sequentialIDs{},
		OntologyIndex: staticIndex{index: wattIndex()},
		Now:           func() time.Time { return fixtureNow },
		Profiler:      &partialProfiler{inner: h.profiler, failFor: "svc-fridge"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	profile, err := service.Relate(context.Background(), "token", Request{
		Members: append(kitchenMembers(), failing),
		Window:  fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if _, found := findRule(profile.CandidateRules, "the oven", "the kitchen lights", StateActive); !found {
		t.Errorf("the rule was lost because a third service failed: %v",
			statements(profile.CandidateRules))
	}

	var fridge *Member
	for i := range profile.Members {
		if profile.Members[i].Ref.DeviceID == "dev-fridge" {
			fridge = &profile.Members[i]
		}
	}
	if fridge == nil {
		t.Fatal("the unprofilable member was dropped from the document rather than reported")
	}
	if fridge.State.Usable {
		t.Error("a member with no profile was reported usable")
	}
	if fridge.State.Reason.Reason != profiler.ReasonReadFailed {
		t.Errorf("reason = %q, want %q", fridge.State.Reason.Reason, profiler.ReasonReadFailed)
	}
	if !containsSubstring(profile.Notes, "svc-fridge") {
		t.Errorf("notes = %v, want one naming the service that could not be profiled", profile.Notes)
	}
}

// partialProfiler fails one service and delegates the rest.
type partialProfiler struct {
	inner   *fakeProfiler
	failFor string
}

func (p *partialProfiler) ProfileService(
	ctx context.Context, token string, req profiler.ProfileRequest,
) (profiler.ProfileResult, error) {
	if req.ServiceID == p.failFor {
		return profiler.ProfileResult{}, errors.New("service has no queryable variables")
	}
	return p.inner.ProfileService(ctx, token, req)
}

func TestAContinuousMemberTakesPartInNoRuleAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.profiler.classification = profiler.ActivityContinuous

	profile, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if len(profile.CandidateRules) != 0 {
		t.Errorf("rules = %v, want none from two continuous members",
			statements(profile.CandidateRules))
	}
	if !containsSubstring(profile.Notes, "needs two") {
		t.Errorf("notes = %v, want one saying there were not two usable members", profile.Notes)
	}
	for _, member := range profile.Members {
		if member.State.Usable {
			t.Errorf("%s was reported usable while continuous", member.Label)
		}
	}
}

func TestASecondPassOverTheSameWindowIsServedFromTheStore(t *testing.T) {
	h := newHarness(t)

	first, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	second, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if first.RelationID != second.RelationID {
		t.Errorf("the same request produced two relation ids: %s and %s",
			first.RelationID, second.RelationID)
	}
	if first.ComputedAt != second.ComputedAt {
		t.Error("the second pass replaced the stored document rather than returning it (D21)")
	}
}

// The cache key has to move when anything that changes the content moves, or a
// sharpened threshold would leave a stale document with nothing to notice it by (D25).
func TestTheCacheKeyCoversTheWindowTheGridAndTheParams(t *testing.T) {
	h := newHarness(t)

	base := Request{Members: kitchenMembers(), Window: fixtureWindow()}
	baseline, err := h.service.Relate(context.Background(), "token", base)
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}

	narrower := base
	narrower.Window = profiler.Window{From: fixtureStart.Add(24 * time.Hour), To: fixtureNow}
	changed, err := h.service.Relate(context.Background(), "token", narrower)
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if changed.RelationID == baseline.RelationID {
		t.Error("a different window produced the same relation id")
	}

	stricter := base
	stricter.Params = RuleParams{MinConfidence: 0.95}
	changed, err = h.service.Relate(context.Background(), "token", stricter)
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if changed.RelationID == baseline.RelationID {
		t.Error("different rule params produced the same relation id")
	}
}

func TestPhasesAreReportedAsThePassProceeds(t *testing.T) {
	h := newHarness(t)
	seen := []string{}

	if _, err := h.service.Relate(context.Background(), "token", Request{
		Members:  kitchenMembers(),
		Window:   fixtureWindow(),
		Progress: func(phase Phase) { seen = append(seen, phase.Stage) },
	}); err != nil {
		t.Fatalf("Relate: %v", err)
	}

	for _, want := range []string{PhaseProfiles, PhaseAlign, PhaseStates, PhaseRelate, PhaseStore} {
		found := false
		for _, stage := range seen {
			if stage == want {
				found = true
			}
		}
		if !found {
			t.Errorf("phase %q was never reported; got %v", want, seen)
		}
	}
}

func TestACancelledPassStopsReading(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.service.Relate(ctx, "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err == nil {
		t.Fatal("a cancelled pass returned a profile")
	}
	if h.timeseries.calls != 0 {
		t.Errorf("the aligned read ran %d times after cancellation", h.timeseries.calls)
	}
}

// --- decisions ---

func TestADecisionIsRecordedAgainstTheDeveloperAndReInjected(t *testing.T) {
	h := newHarness(t)

	profile, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	rule, found := findRule(profile.CandidateRules, "the oven", "the kitchen lights", StateActive)
	if !found {
		t.Fatal("the rule did not surface")
	}

	decision, err := h.service.Decide(DecisionRequest{
		RelationID: profile.RelationID,
		RuleID:     rule.RuleID,
		Action:     ActionConfirm,
		Note:       "matches how the kitchen is used",
		UserSub:    "user-123",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.CreatedBy != "user-123" {
		t.Errorf("created_by = %q, want the authenticated developer", decision.CreatedBy)
	}
	if decision.Computed.Statement != rule.Statement {
		t.Errorf("computed statement = %q, want the rule as the detector stated it: %q",
			decision.Computed.Statement, rule.Statement)
	}
	if decision.Computed.Confidence != rule.Confidence {
		t.Errorf("computed confidence = %v, want %v — 'the detector said this and the developer "+
			"confirmed it' is the finding", decision.Computed.Confidence, rule.Confidence)
	}
	if decision.DetectorVersion != DetectorVersion {
		t.Errorf("detector_version = %q, want %q", decision.DetectorVersion, DetectorVersion)
	}

	// Reading the relation back carries the verdict.
	reread, err := h.service.Get(profile.RelationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	decided, _ := findRule(reread.CandidateRules, "the oven", "the kitchen lights", StateActive)
	if decided.Decision == nil {
		t.Fatal("the rule came back without the decision")
	}
	if decided.Decision.Action != ActionConfirm {
		t.Errorf("action = %q, want %q", decided.Decision.Action, ActionConfirm)
	}
}

// A decision has to survive the rule being recomputed, which is the whole reason it
// is keyed by a fingerprint rather than by the relation id.
func TestADecisionSurvivesARecomputationOverADifferentWindow(t *testing.T) {
	h := newHarness(t)

	first, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	rule, _ := findRule(first.CandidateRules, "the oven", "the kitchen lights", StateActive)
	if _, err := h.service.Decide(DecisionRequest{
		RelationID: first.RelationID, RuleID: rule.RuleID,
		Action: ActionReject, Note: "the morning run is intentional", UserSub: "user-123",
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// A different window: a new relation id, the same claim.
	second, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(),
		Window:  profiler.Window{From: fixtureStart.Add(48 * time.Hour), To: fixtureNow},
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if second.RelationID == first.RelationID {
		t.Fatal("the fixture depends on the second pass being a different relation")
	}

	recomputed, found := findRule(second.CandidateRules, "the oven", "the kitchen lights", StateActive)
	if !found {
		t.Fatal("the rule did not surface on the second pass")
	}
	if recomputed.Decision == nil {
		t.Fatal("the recomputed rule lost the developer's decision — the fingerprint is not stable")
	}
	if recomputed.Decision.Action != ActionReject {
		t.Errorf("action = %q, want the rejection to carry forward", recomputed.Decision.Action)
	}
}

func TestADecisionOnARuleTheRelationDoesNotCarryIsRefused(t *testing.T) {
	h := newHarness(t)
	profile, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}

	_, err = h.service.Decide(DecisionRequest{
		RelationID: profile.RelationID, RuleID: "rule-typo", Action: ActionConfirm, UserSub: "user-1",
	})
	if !errors.Is(err, ErrUnknownRule) {
		t.Errorf("err = %v, want ErrUnknownRule: a decision nothing can read back is a record of nothing", err)
	}

	_, err = h.service.Decide(DecisionRequest{
		RelationID: "rel-nope", RuleID: "rule-1", Action: ActionConfirm, UserSub: "user-1",
	})
	if !errors.Is(err, ErrRelationNotFound) {
		t.Errorf("err = %v, want ErrRelationNotFound", err)
	}
}

func TestACorrectedRuleKeepsBothForms(t *testing.T) {
	h := newHarness(t)
	profile, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	rule, _ := findRule(profile.CandidateRules, "the oven", "the kitchen lights", StateActive)

	decision, err := h.service.Decide(DecisionRequest{
		RelationID: profile.RelationID, RuleID: rule.RuleID, Action: ActionCorrect,
		Confirmed: &DecidedRule{
			Statement: "the oven active after 18:00 → the kitchen lights active",
			Exceptions: []Exception{{
				Dimension: DimensionHourOfDay, Bucket: "06:00-12:00", FromHour: 6, ToHour: 12,
			}},
		},
		UserSub: "user-123",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Confirmed == nil {
		t.Fatal("the correction lost the developer's own form of the rule")
	}
	if decision.Computed.Statement == decision.Confirmed.Statement {
		t.Error("computed and confirmed are the same; the diff is the empirical record (§5.4.3)")
	}

	log := h.service.Decisions(rule.RuleID)
	if len(log) != 1 {
		t.Fatalf("log = %d entries, want 1", len(log))
	}
}

// From a real run: six members, three of them called "Licht EG value" because one
// device had three services whose variables all end in `value`. A rule over those
// reads "Licht EG value active → Licht EG value active", which is unconfirmable.
func TestCollidingMemberLabelsAreDisambiguated(t *testing.T) {
	h := newHarness(t)
	result := kitchenResolution()
	// Two more services of the oven's device type, all with a leaf named `value` —
	// which is what the platform's device types actually look like.
	for _, service := range []string{"svc-oven-2", "svc-oven-3"} {
		result.Selectables = append(result.Selectables, selection.Selectable{
			DeviceTypeID: "dt-plug", ServiceID: service, ServiceName: service,
			Path: "value.value", Unit: "A", UnitSource: profiler.UnitInferred,
			AspectID: "kitchen", Queryable: true,
		})
	}
	result.Selectables[0].ServiceName = "svc-oven-1"
	result.Selectables[0].Path = "value.value"
	h.selection.result = result

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) == 0 {
		t.Fatalf("no set proposed; notes %v", proposal.Notes)
	}

	for _, set := range proposal.Sets {
		seen := map[string]string{}
		for _, member := range set.Members {
			if previous, clash := seen[member.Label]; clash {
				t.Errorf("set %q has two members labelled %q (%s and %s); a rule over them "+
					"would name neither", set.Name, member.Label, previous, member.Ref.VariablePath)
			}
			seen[member.Label] = member.Ref.VariablePath
		}
	}

	// The suffix is the service name where that separates them, because it reads.
	labelled := false
	for _, set := range proposal.Sets {
		for _, member := range set.Members {
			if strings.Contains(member.Label, "(svc-oven-") {
				labelled = true
			}
		}
	}
	if !labelled {
		t.Error("no label carries a service-name suffix, so the collision was resolved by " +
			"something less readable than it could be")
	}
}

// The same protection inside a pass, because a caller may send colliding labels of
// its own — an LLM writing them by hand, or a proposal made before this was fixed.
func TestARelationalPassDisambiguatesTheLabelsItWasGiven(t *testing.T) {
	h := newHarness(t)

	profile, err := h.service.Relate(context.Background(), "token", Request{
		Members: []SeriesMember{
			{Ref: ovenRef(), Label: "Licht EG value"},
			{Ref: lightsRef(), Label: "Licht EG value"},
		},
		Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}
	if profile.Members[0].Label == profile.Members[1].Label {
		t.Fatalf("both members are labelled %q; every rule statement would name neither",
			profile.Members[0].Label)
	}
	for _, rule := range profile.CandidateRules {
		if rule.Antecedent.Label == rule.Consequent.Label {
			t.Errorf("rule %q names the same label on both sides", rule.Statement)
		}
	}
}

// An unusable member has to say whether the read came back empty or came back full
// and unsplittable. Without the count the two are indistinguishable — both report
// 0 active, 0 idle and every bucket unknown.
func TestAnUnusableMemberStillReportsWhatTheReadReturned(t *testing.T) {
	h := newHarness(t)
	// Continuous, so every member is rejected *after* a successful read.
	h.profiler.classification = profiler.ActivityContinuous

	profile, err := h.service.Relate(context.Background(), "token", Request{
		Members: kitchenMembers(), Window: fixtureWindow(),
	})
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}

	for _, member := range profile.Members {
		if member.State.Usable {
			t.Fatalf("%s is usable; the fixture depends on it not being", member.Label)
		}
		if member.State.ObservedBuckets == 0 {
			t.Errorf("%s reports 0 observed buckets, but the aligned read returned data for "+
				"it — that is the number that separates an empty read from an unsplittable one",
				member.Label)
		}
		if member.State.ObservedBuckets > profile.Buckets {
			t.Errorf("%s observed %d of %d buckets", member.Label,
				member.State.ObservedBuckets, profile.Buckets)
		}
	}
}

// --- ProposeRelatedSets ---

func TestProposeRelatedSetsFindsTheKitchenPairFromTheAspect(t *testing.T) {
	h := newHarness(t)

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}

	if proposal.AspectName != "Kitchen" {
		t.Errorf("aspect_name = %q, want Kitchen", proposal.AspectName)
	}
	if len(proposal.Sets) == 0 {
		t.Fatalf("no set was proposed; notes: %v", proposal.Notes)
	}
	for _, set := range proposal.Sets {
		if set.Devices < 2 {
			t.Errorf("set %q spans %d device(s); a single-device set has no conditional pattern in it",
				set.Name, set.Devices)
		}
		if set.SetID == "" {
			t.Errorf("set %q has no id", set.Name)
		}
		if set.Rationale == "" {
			t.Errorf("set %q has no rationale; a developer cannot judge a grouping without one", set.Name)
		}
	}

	// Tier L0: no value was read, and the answer says so.
	if proposal.Reads.Values != 0 {
		t.Errorf("values read = %d, want 0 — this operation is tier L0 (§5.8)", proposal.Reads.Values)
	}
	// The graphs are consulted, and the listing is narrowed to the devices already
	// resolved rather than enumerating the platform's whole topology.
	if len(h.ontology.graphCalls) != 1 {
		t.Fatalf("graphs were listed %d times, want once", len(h.ontology.graphCalls))
	}
	if len(h.ontology.graphCalls[0].DeviceIDs) != 2 {
		t.Errorf("the graph listing filtered on %d device ids, want 2",
			len(h.ontology.graphCalls[0].DeviceIDs))
	}
	// No graph in this fixture, and an empty answer says which of the two reasons
	// applies rather than leaving the aspect sets to be read as the whole story.
	if !containsSubstring(proposal.Notes, "no device relationship graph") {
		t.Errorf("notes = %v, want one saying no graph names these devices", proposal.Notes)
	}

	// The resolution asks the ontology for the aspect and skips the ranking, which is
	// what keeps it read-free.
	if len(h.selection.requests) != 1 {
		t.Fatalf("selection was called %d times, want once", len(h.selection.requests))
	}
	request := h.selection.requests[0]
	if len(request.AspectIDs) != 1 || request.AspectIDs[0] != "kitchen" {
		t.Errorf("aspect ids = %v, want [kitchen]", request.AspectIDs)
	}
	if !request.SkipRanking {
		t.Error("the resolution ranked the candidates, which costs one availability call per device")
	}
}

// §5.5 asks for existing groupings to be checked before new ones are constructed.
func TestAnExistingDeviceGroupIsProposedFirst(t *testing.T) {
	h := newHarness(t)
	h.ontology.groups = []models.DeviceGroup{{
		Id: "dg-kitchen", Name: "Kitchen appliances",
		DeviceIds: []string{"dev-oven", "dev-lights"},
	}}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) == 0 {
		t.Fatal("no set was proposed")
	}
	if proposal.Sets[0].Origin != OriginDeviceGroup {
		t.Errorf("the first set's origin is %q, want %q: somebody has already asserted that grouping",
			proposal.Sets[0].Origin, OriginDeviceGroup)
	}
	if proposal.Sets[0].DeviceGroupID != "dg-kitchen" {
		t.Errorf("device_group_id = %q, want dg-kitchen", proposal.Sets[0].DeviceGroupID)
	}

	// The listing is narrowed to the devices already resolved rather than enumerating
	// the platform.
	if len(h.ontology.groupCalls) != 1 {
		t.Fatalf("device groups were listed %d times, want once", len(h.ontology.groupCalls))
	}
	if len(h.ontology.groupCalls[0].DeviceIDs) != 2 {
		t.Errorf("the listing filtered on %d device ids, want 2",
			len(h.ontology.groupCalls[0].DeviceIDs))
	}
	if !h.ontology.groupCalls[0].IgnoreGenerated {
		t.Error("generated groups were included; one is a by-product of another feature rather " +
			"than a grouping worth preferring")
	}
}

// A group naming devices the developer cannot read, or devices elsewhere, must not
// produce a set that fails at the first read.
func TestADeviceGroupIsIntersectedWithWhatTheCallerCanRead(t *testing.T) {
	h := newHarness(t)
	h.ontology.groups = []models.DeviceGroup{{
		Id: "dg-wide", Name: "Everything",
		DeviceIds: []string{"dev-oven", "dev-lights", "dev-somebody-elses"},
	}}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}

	group := proposal.Sets[0]
	if group.Devices != 2 {
		t.Errorf("the set spans %d devices, want the 2 that resolved", group.Devices)
	}
	for _, member := range group.Members {
		if member.Ref.DeviceID == "dev-somebody-elses" {
			t.Error("a device the caller cannot read made it into the set")
		}
	}
	if !containsSubstring(group.Notes, "readable by this developer") {
		t.Errorf("notes = %v, want one saying the group was narrowed", group.Notes)
	}
}

// A failure listing groups must not take the aspect-derived sets with it: they are
// what this operation is really about.
func TestAFailedDeviceGroupListingIsANoteRatherThanAnError(t *testing.T) {
	h := newHarness(t)
	h.ontology.groupErr = errors.New("device-repository unavailable")

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) == 0 {
		t.Error("the aspect-derived sets were lost when the group listing failed")
	}
	if !containsSubstring(proposal.Notes, "could not be listed") {
		t.Errorf("notes = %v, want one saying the group listing failed", proposal.Notes)
	}
}

// The device repository expands an aspect criterion to the whole subtree upstream, so
// a caller who asked for one node has to be given one node.
func TestIncludeDescendantsDecidesWhetherASiblingNodeCounts(t *testing.T) {
	h := newHarness(t)
	// The lights declare the child node rather than the kitchen itself, which is the
	// case §5.5 names: an oven on "Kitchen" and lights on "Kitchen Ceiling".
	result := kitchenResolution()
	result.Selectables = append(result.Selectables, selection.Selectable{
		DeviceTypeID: "dt-lamp", DeviceTypeName: "Lamp", ServiceID: "svc-lights",
		Path: "value.power", Unit: "W", UnitSource: profiler.UnitFromCharacteristic,
		AspectID: "kitchen-ceiling", AspectName: "Kitchen Ceiling", Queryable: true,
	})
	result.CandidateDevices = []selection.CandidateDevice{
		{DeviceID: "dev-oven", Name: "Oven", DeviceTypeID: "dt-plug"},
		{DeviceID: "dev-lights", Name: "Kitchen lights", DeviceTypeID: "dt-lamp"},
	}
	h.selection.result = result

	// Without descendants there is one device under "Kitchen" itself and nothing to
	// relate — and the answer has to say which of the several causes applies.
	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) != 0 {
		t.Errorf("sets = %d without descendants, want none: only the oven declares Kitchen itself",
			len(proposal.Sets))
	}
	if len(proposal.Notes) == 0 {
		t.Error("an empty answer with no note reads as 'no such pattern exists'")
	}

	// With them, the pair is proposed.
	proposal, err = h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen", IncludeDescendants: true,
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) == 0 {
		t.Fatalf("no set with descendants included; notes: %v", proposal.Notes)
	}
	found := false
	for _, set := range proposal.Sets {
		if set.Origin == OriginAspectSubtree && set.Devices == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("no subtree set spanning both devices; got %v", setSummaries(proposal.Sets))
	}
	// The subtree of "Kitchen" is the node itself plus its one child; "Building" sits
	// above it and is not part of it.
	if len(proposal.Subtree) != 2 {
		t.Errorf("subtree = %d nodes, want 2", len(proposal.Subtree))
	}
}

func TestAnUnknownAspectIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "no-such-aspect",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
	if _, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v with no aspect id, want ErrInvalidRequest", err)
	}
}

// One series per device before a second of any device: the oven-and-lights case is
// one series each, and a cap spent on eight channels of one meter proposes a set with
// nothing to relate.
func TestTheMemberCapTakesOneSeriesPerDeviceFirst(t *testing.T) {
	h := newHarness(t)
	result := kitchenResolution()
	for _, path := range []string{"value.voltage", "value.current", "value.energy"} {
		result.Selectables = append(result.Selectables, selection.Selectable{
			DeviceTypeID: "dt-plug", ServiceID: "svc-oven", Path: path,
			Unit: "V", UnitSource: profiler.UnitInferred, AspectID: "kitchen", Queryable: true,
		})
	}
	h.selection.result = result

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen", MaxMembers: 2,
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) == 0 {
		t.Fatalf("no set proposed; notes: %v", proposal.Notes)
	}
	set := proposal.Sets[0]
	if set.Devices != 2 {
		t.Errorf("the capped set spans %d devices, want 2", set.Devices)
	}
	if !set.Truncated {
		t.Error("the cap applied but the set does not say so; a silent truncation reads as completeness")
	}
	if len(set.Notes) == 0 {
		t.Error("the truncation has no note")
	}
}

func TestAnUnqueryablePathIsLeftOutAndCounted(t *testing.T) {
	h := newHarness(t)
	result := kitchenResolution()
	result.Selectables = append(result.Selectables, selection.Selectable{
		DeviceTypeID: "dt-plug", ServiceID: "svc-oven", Path: "value.mode",
		AspectID: "kitchen", Queryable: false, Reason: "not a service output",
	})
	h.selection.result = result

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	for _, set := range proposal.Sets {
		for _, member := range set.Members {
			if member.Ref.VariablePath == "value.mode" {
				t.Error("an unreadable path was offered as a member")
			}
		}
	}
	if !containsSubstring(proposal.Notes, "not readable as a scalar series") {
		t.Errorf("notes = %v, want one counting what was left out", proposal.Notes)
	}
}

// --- construction ---

func TestTheServiceRefusesToBuildWithoutItsHalves(t *testing.T) {
	base := func() Deps {
		return Deps{
			Timeseries:    &fakeTimeseries{signal: kitchenSignal},
			Devices:       &fakeDevices{},
			Ontology:      &fakeOntology{},
			Selection:     &fakeSelection{},
			Profiler:      &fakeProfiler{},
			OntologyIndex: staticIndex{index: wattIndex()},
			IDs:           &sequentialIDs{},
		}
	}
	for name, break_ := range map[string]func(*Deps){
		"no timeseries":     func(d *Deps) { d.Timeseries = nil },
		"no devices":        func(d *Deps) { d.Devices = nil },
		"no ontology":       func(d *Deps) { d.Ontology = nil },
		"no selection":      func(d *Deps) { d.Selection = nil },
		"no profiler":       func(d *Deps) { d.Profiler = nil },
		"no ontology index": func(d *Deps) { d.OntologyIndex = nil },
		"no ids":            func(d *Deps) { d.IDs = nil },
	} {
		deps := base()
		break_(&deps)
		if _, err := New(deps); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}

	// A store is the one dependency with a sensible default.
	if _, err := New(base()); err != nil {
		t.Errorf("New with no store: %v", err)
	}
}

// --- helpers ---

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func setSummaries(sets []CandidateSet) []string {
	out := make([]string, 0, len(sets))
	for _, set := range sets {
		out = append(out, fmt.Sprintf("%s/%s(%d devices)", set.Origin, set.Name, set.Devices))
	}
	return out
}

// A negative grid used to fall through to the derived path, which answered a
// request nobody made: the profile came back on a grid the caller never asked for,
// with nothing in it saying their own value had been discarded. It cannot allocate
// anything, so this is not a bound — it is the difference between refusing an
// impossible request and quietly substituting a different one.
func TestANegativeGridIsRefusedRatherThanQuietlyReplacedByADerivedOne(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Relate(context.Background(), "token", Request{
		Members:     kitchenMembers(),
		Window:      profiler.Window{From: fixtureStart, To: fixtureStart.Add(24 * time.Hour)},
		GridSeconds: -300,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if h.timeseries.calls != 0 || len(h.profiler.calls) != 0 {
		t.Errorf("the platform was read %d/%d times for a request that was refused",
			h.timeseries.calls, len(h.profiler.calls))
	}

	// Zero still means "derive one", which is what an omitted field marshals to and
	// is the normal case rather than an error.
	derived := newHarness(t)
	if _, err := derived.service.Relate(context.Background(), "token", Request{
		Members:     kitchenMembers(),
		Window:      profiler.Window{From: fixtureStart, To: fixtureStart.Add(24 * time.Hour)},
		GridSeconds: 0,
	}); err != nil {
		t.Fatalf("an omitted grid was refused: %v", err)
	}
}
