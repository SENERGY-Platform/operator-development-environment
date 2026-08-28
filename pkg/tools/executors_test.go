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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// --- fakes ---

type fakeOntology struct {
	snapshot *ontology.Snapshot

	// deviceTypes is the whole-device-type answer, by id, and is what the backfill
	// precondition reads. Empty is the honest default for most tests: a device type
	// the repository does not return leaves the verdict unknown rather than no.
	deviceTypes map[string]models.DeviceType
	// deviceTypeErr makes the repository refuse, so the "unknown is not no" path can
	// be reached.
	deviceTypeErr error
	// deviceTypeCalls records the id sets asked for, which is how a test checks that
	// one request covers the whole catalogue.
	deviceTypeCalls [][]string
}

func (f *fakeOntology) Snapshot(context.Context, string) (*ontology.Snapshot, error) {
	return f.snapshot, nil
}

func (f *fakeOntology) DeviceTypesByID(
	_ context.Context, _ string, ids []string,
) (map[string]models.DeviceType, error) {
	f.deviceTypeCalls = append(f.deviceTypeCalls, ids)
	if f.deviceTypeErr != nil {
		return nil, f.deviceTypeErr
	}
	out := map[string]models.DeviceType{}
	for _, id := range ids {
		if deviceType, found := f.deviceTypes[id]; found {
			out[id] = deviceType
		}
	}
	return out, nil
}

// fakeDevices records the permission each call asked for, which is what the
// Read-versus-Execute test asserts on.
type fakeDevices struct {
	mux         sync.Mutex
	listOptions []drmodel.ExtendedDeviceListOptions
	getActions  []drmodel.AuthAction
	device      models.ExtendedDevice
}

func (f *fakeDevices) List(_ string, options drmodel.ExtendedDeviceListOptions) (devices.ListResult, error) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.listOptions = append(f.listOptions, options)
	return devices.ListResult{Devices: []models.ExtendedDevice{f.device}, Total: 1}, nil
}

func (f *fakeDevices) Get(_ string, _ string, action drmodel.AuthAction) (models.ExtendedDevice, error) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.getActions = append(f.getActions, action)
	return f.device, nil
}

type fakeTimeseries struct {
	mux      sync.Mutex
	elements []timeseries.QueryElement
	// points is how many rows Query answers with.
	points      int
	usage       []timeseries.Usage
	exportUsage []timeseries.Usage
	// availability is what /data-availability answers with. Nil means the
	// platform knows no window for the service, which is what most of these tests
	// want; the tools that read data need one.
	availability []timeseries.Availability
}

func (f *fakeTimeseries) DataAvailability(context.Context, string, string) ([]timeseries.Availability, error) {
	if f.availability != nil {
		return f.availability, nil
	}
	return []timeseries.Availability{}, nil
}

// serviceWindow is one raw window plus one materialised aggregate for a service,
// the shape /data-availability returns (one element per service and per
// materialised aggregate).
func serviceWindow(serviceID string, from, to time.Time) []timeseries.Availability {
	fromCopy, toCopy := from, to
	bucket, groupType := "1 day", "mean"
	return []timeseries.Availability{
		{ServiceId: serviceID, From: &fromCopy, To: &toCopy},
		{
			ServiceId: serviceID, From: &fromCopy, To: &toCopy,
			GroupTime: &bucket, GroupType: &groupType,
		},
	}
}

func (f *fakeTimeseries) DeviceUsage(_ context.Context, _ string, ids []string) ([]timeseries.Usage, error) {
	if f.usage != nil {
		return f.usage, nil
	}
	out := make([]timeseries.Usage, 0, len(ids))
	for _, id := range ids {
		out = append(out, timeseries.Usage{DeviceId: id, Bytes: 1_000_000, BytesPerDay: 100_000})
	}
	return out, nil
}

func (f *fakeTimeseries) ExportUsage(_ context.Context, _ string, ids []string) ([]timeseries.Usage, error) {
	if f.exportUsage != nil {
		return f.exportUsage, nil
	}
	out := make([]timeseries.Usage, 0, len(ids))
	for _, id := range ids {
		out = append(out, timeseries.Usage{ExportId: id, Bytes: 1_000_000, BytesPerDay: 100_000})
	}
	return out, nil
}

func (f *fakeTimeseries) Query(
	_ context.Context, _ string, elements []timeseries.QueryElement, _ timeseries.QueryOptions,
) ([]timeseries.QueryResult, error) {
	f.mux.Lock()
	f.elements = append(f.elements, elements...)
	points := f.points
	f.mux.Unlock()

	// One element, one series, `points` rows of [timestamp, value] — the shape
	// DecodeResults expects.
	rows := make([][]interface{}, 0, points)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < points; i++ {
		rows = append(rows, []interface{}{
			base.Add(time.Duration(i) * time.Hour).Format("2006-01-02T15:04:05.000Z07:00"),
			float64(i),
		})
	}
	return []timeseries.QueryResult{{RequestIndex: 0, Data: [][][]interface{}{rows}}}, nil
}

func (f *fakeTimeseries) lastElement(t *testing.T) timeseries.QueryElement {
	t.Helper()
	f.mux.Lock()
	defer f.mux.Unlock()
	if len(f.elements) == 0 {
		t.Fatal("no query was issued")
	}
	return f.elements[len(f.elements)-1]
}

func testDevice() models.ExtendedDevice {
	return models.ExtendedDevice{
		Device: models.Device{
			Id: "device-1", Name: "Meter 1", DeviceTypeId: "dt-1",
		},
		DisplayName:    "Meter 1",
		DeviceTypeName: "Power Meter",
		DeviceType: &models.DeviceType{
			Id: "dt-1", Name: "Power Meter",
			Services: []models.Service{{
				Id: "svc-1", Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: "value", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{{
							Id: "cv-power", Name: "power", Type: models.Float,
							CharacteristicId: "char-watt",
						}},
					},
				}},
			}},
		},
	}
}

func executorFor(t *testing.T, deps Deps, name string) (Definition, *Dispatcher) {
	t.Helper()
	registry, err := NewSurface(deps)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, found := registry.Lookup(name)
	if !found {
		t.Fatalf("%q is not in the surface", name)
	}
	if !definition.Implemented() {
		t.Fatalf("%q has no executor with these dependencies: %s", name, definition.Unavailable)
	}
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return definition, dispatcher
}

func dispatchJSON(t *testing.T, dispatcher *Dispatcher, tier Tier, name, input string) map[string]any {
	t.Helper()
	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", UserSub: "sub-1", SessionID: "sess-1", Tier: tier},
		Call{ID: "c1", Name: name, Input: json.RawMessage(input)})

	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	if result.Outcome != OutcomeOK {
		t.Fatalf("%s failed (%s): %s", name, result.Outcome, encoded)
	}
	return decoded
}

// --- Read versus Execute (§5.1) ---

// TestMetadataToolsUseReadAndDataToolsUseExecute pins the distinction the README
// calls out: models.Read governs device metadata, models.Execute governs reading a
// device's data. Listing candidates under Read would offer series the caller
// cannot read, and the failure would surface later, at query time.
func TestMetadataToolsUseReadAndDataToolsUseExecute(t *testing.T) {
	device := testDevice()

	t.Run("list_devices uses Read", func(t *testing.T) {
		fake := &fakeDevices{device: device}
		_, dispatcher := executorFor(t, Deps{Devices: fake}, "list_devices")
		dispatchJSON(t, dispatcher, L0, "list_devices", `{}`)

		if len(fake.listOptions) != 1 {
			t.Fatalf("list calls = %d", len(fake.listOptions))
		}
		if fake.listOptions[0].Permission != models.Read {
			t.Errorf("permission = %v, want Read: this tool serves metadata",
				fake.listOptions[0].Permission)
		}
	})

	t.Run("quick_profile uses Execute", func(t *testing.T) {
		fake := &fakeDevices{device: device}
		prof := newTestProfiler(t)
		_, dispatcher := executorFor(t, Deps{Devices: fake, Profiler: prof}, "quick_profile")
		dispatchJSON(t, dispatcher, L0, "quick_profile", `{}`)

		if len(fake.listOptions) != 1 {
			t.Fatalf("list calls = %d", len(fake.listOptions))
		}
		if fake.listOptions[0].Permission != models.Execute {
			t.Errorf("permission = %v, want Execute: this tool offers series to read",
				fake.listOptions[0].Permission)
		}
		if !fake.listOptions[0].FullDt {
			t.Error("fulldt was not requested; the variable enumeration walks the device type")
		}
	})

	t.Run("get_device_metadata uses Read", func(t *testing.T) {
		fake := &fakeDevices{device: device}
		_, dispatcher := executorFor(t, Deps{Devices: fake}, "get_device_metadata")
		dispatchJSON(t, dispatcher, L0, "get_device_metadata", `{"device_id":"device-1"}`)

		if len(fake.getActions) != 1 || fake.getActions[0] != models.Read {
			t.Errorf("actions = %v, want one Read", fake.getActions)
		}
	})
}

func TestGetDeviceMetadataEnumeratesAddressableSeries(t *testing.T) {
	fake := &fakeDevices{device: testDevice()}
	_, dispatcher := executorFor(t, Deps{Devices: fake}, "get_device_metadata")

	decoded := dispatchJSON(t, dispatcher, L0, "get_device_metadata", `{"device_id":"device-1"}`)

	services, ok := decoded["services"].([]any)
	if !ok || len(services) != 1 {
		t.Fatalf("services = %v, want one", decoded["services"])
	}
	service := services[0].(map[string]any)
	variables, ok := service["variables"].([]any)
	if !ok || len(variables) == 0 {
		t.Fatalf("variables = %v, want the addressable paths", service["variables"])
	}
	variable := variables[0].(map[string]any)
	// {device_id, service_id, variable_path} is the addressable unit (D19), and the
	// path is what a model needs to name a series at all.
	if variable["variable_path"] != "value.power" {
		t.Errorf("variable_path = %v, want value.power", variable["variable_path"])
	}
	if variable["characteristic_id"] != "char-watt" {
		t.Errorf("characteristic_id = %v; it is canonical and must not be dropped (D29)",
			variable["characteristic_id"])
	}
}

// --- preview_series: the §4 safeguard ---

// TestPreviewCapsPointsAndWidensTheBucket is the guard that keeps "downsampled
// preview" from becoming a raw series read. A preview large enough to compute
// statistics from would let a model do exactly that while nominally respecting the
// tier.
func TestPreviewCapsPointsAndWidensTheBucket(t *testing.T) {
	fake := &fakeTimeseries{points: 5000}
	_, dispatcher := executorFor(t, Deps{Timeseries: fake, PreviewMaxPoints: 50}, "preview_series")

	decoded := dispatchJSON(t, dispatcher, L2, "preview_series", `{
		"device_id":"device-1","service_id":"svc-1","variable_path":"value.power",
		"from":"2026-01-01T00:00:00Z","to":"2026-08-01T00:00:00Z"
	}`)

	points, ok := decoded["points"].([]any)
	if !ok {
		t.Fatalf("points = %v", decoded["points"])
	}
	if len(points) > 50 {
		t.Errorf("returned %d points against a cap of 50", len(points))
	}
	if decoded["max_points"].(float64) != 50 {
		t.Errorf("max_points = %v, want the cap reported", decoded["max_points"])
	}
	// Seven months at the requested resolution would not fit, so the bucket must be
	// widened rather than the window truncated: a truncated preview shows the first
	// slice and looks like the whole of it.
	if decoded["group_time"] == "" {
		t.Error("no group_time was chosen")
	}
	if !strings.Contains(decoded["note"].(string), "Do not compute statistics") {
		t.Errorf("note = %v, want the §4 instruction", decoded["note"])
	}
}

func TestPreviewRequiresTheFullSeriesReference(t *testing.T) {
	fake := &fakeTimeseries{points: 1}
	_, dispatcher := executorFor(t, Deps{Timeseries: fake}, "preview_series")

	result := dispatcher.Dispatch(context.Background(),
		Request{Tier: L2}, Call{ID: "c", Name: "preview_series",
			Input: json.RawMessage(`{"device_id":"device-1"}`)})

	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %q, want %q for a partial series reference",
			result.Outcome, OutcomeInvalidInput)
	}
}

func TestPreviewRejectsAnUnknownGroupType(t *testing.T) {
	fake := &fakeTimeseries{points: 1}
	_, dispatcher := executorFor(t, Deps{Timeseries: fake}, "preview_series")

	result := dispatcher.Dispatch(context.Background(), Request{Tier: L2},
		Call{ID: "c", Name: "preview_series", Input: json.RawMessage(`{
			"device_id":"d","service_id":"s","variable_path":"v","group_type":"nonsense"
		}`)})

	// Checked locally, because the server answers an invalid aggregate with a bare
	// 400 that says nothing about which field was wrong.
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeInvalidInput)
	}
}

func TestPreviewSendsTheAggregateAsAsked(t *testing.T) {
	fake := &fakeTimeseries{points: 10}
	_, dispatcher := executorFor(t, Deps{Timeseries: fake, PreviewMaxPoints: 100}, "preview_series")

	dispatchJSON(t, dispatcher, L2, "preview_series", `{
		"device_id":"device-1","service_id":"svc-1","variable_path":"value.power",
		"group_type":"difference-last","group_time":"1h",
		"from":"2026-08-01T00:00:00Z","to":"2026-08-02T00:00:00Z"
	}`)

	element := fake.lastElement(t)
	if len(element.Columns) != 1 {
		t.Fatalf("columns = %d, want 1", len(element.Columns))
	}
	if element.Columns[0].GroupType == nil || *element.Columns[0].GroupType != "difference-last" {
		t.Errorf("group type = %v, want difference-last", element.Columns[0].GroupType)
	}
	if element.GroupTime == nil || *element.GroupTime != "1h" {
		t.Errorf("group time = %v, want the requested 1h to be respected when it fits",
			element.GroupTime)
	}
}

func TestPreviewBucketLadder(t *testing.T) {
	window := profiler.Window{
		From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), // 24 hours
	}

	// 24 one-hour buckets fit a cap of 24.
	if bucket, widened := previewBucket("", window, 24); bucket != "1h" || widened {
		t.Errorf("bucket = %q widened=%v, want 1h without widening", bucket, widened)
	}
	// A cap of 2 needs a much wider bucket.
	bucket, _ := previewBucket("", window, 2)
	if bucket != "12h" && bucket != "1d" {
		t.Errorf("bucket = %q, want at least 12h for a cap of 2 over a day", bucket)
	}
	// A requested bucket that fits is respected.
	if bucket, widened := previewBucket("1h", window, 100); bucket != "1h" || widened {
		t.Errorf("bucket = %q widened=%v, want the request honoured", bucket, widened)
	}
	// One that does not fit is widened, and the caller is told.
	if bucket, widened := previewBucket("1m", window, 10); bucket == "1m" || !widened {
		t.Errorf("bucket = %q widened=%v, want a widened bucket and the flag set", bucket, widened)
	}
	// An unparseable bucket is passed through: timescale-wrapper accepts forms Go's
	// parser does not.
	if bucket, _ := previewBucket("1day", window, 10); bucket != "1day" {
		t.Errorf("bucket = %q, want the unparseable form passed through", bucket)
	}
}

// --- estimate_read_cost (§5.3.3) ---

func TestEstimateReadCostIsLabelledOrderOfMagnitude(t *testing.T) {
	fake := &fakeTimeseries{}
	_, dispatcher := executorFor(t, Deps{Timeseries: fake}, "estimate_read_cost")

	decoded := dispatchJSON(t, dispatcher, L0, "estimate_read_cost", `{
		"device_ids":["device-1"],
		"from":"2026-01-01T00:00:00Z","to":"2026-01-11T00:00:00Z"
	}`)

	estimates := decoded["estimates"].([]any)
	if len(estimates) != 1 {
		t.Fatalf("estimates = %d, want 1", len(estimates))
	}
	estimate := estimates[0].(map[string]any)

	// 100 000 bytes/day over ten days.
	if got := estimate["estimated_window_bytes"].(float64); got != 1_000_000 {
		t.Errorf("estimated window bytes = %v, want 1e6", got)
	}
	// §5.4.2 is explicit that an interval derived from bytes per day is
	// order-of-magnitude only and must never drive a resampling decision, so the
	// label travels with the number rather than living only in the docs.
	if estimate["confidence"] != "uncertain" {
		t.Errorf("confidence = %v, want uncertain", estimate["confidence"])
	}
	if !strings.Contains(decoded["caveat"].(string), "resampling") {
		t.Errorf("caveat = %v, want the resampling warning", decoded["caveat"])
	}
	// The L0 promise, stated in the answer so it is checkable.
	reads := decoded["reads"].(map[string]any)
	if reads["values"].(float64) != 0 {
		t.Errorf("value reads = %v, want 0: this is a tier L0 tool", reads["values"])
	}
}

func TestEstimateReadCostRequiresDevices(t *testing.T) {
	fake := &fakeTimeseries{}
	_, dispatcher := executorFor(t, Deps{Timeseries: fake}, "estimate_read_cost")
	result := dispatcher.Dispatch(context.Background(), Request{Tier: L0},
		Call{ID: "c", Name: "estimate_read_cost", Input: json.RawMessage(`{}`)})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeInvalidInput)
	}
}

// --- probe_availability ---

func TestProbeAvailabilityReportsZeroValueReads(t *testing.T) {
	fake := &fakeTimeseries{}
	_, dispatcher := executorFor(t, Deps{Timeseries: fake}, "probe_availability")

	decoded := dispatchJSON(t, dispatcher, L0, "probe_availability", `{"device_id":"device-1"}`)
	reads := decoded["reads"].(map[string]any)
	if reads["values"].(float64) != 0 {
		t.Errorf("value reads = %v, want 0", reads["values"])
	}
}

// --- window parsing ---

func TestParseWindowRejectsAnInvertedRange(t *testing.T) {
	if _, err := parseWindow("2026-08-02T00:00:00Z", "2026-08-01T00:00:00Z"); err == nil {
		t.Error("a window ending before it starts was accepted")
	}
	if _, err := parseWindow("not-a-time", ""); err == nil {
		t.Error("an unparseable timestamp was accepted")
	}
	// Both empty is legal: it means the service's own default lookback.
	window, err := parseWindow("", "")
	if err != nil {
		t.Errorf("an empty window was refused: %v", err)
	}
	if window.Valid() {
		t.Error("an empty window should not claim to be valid")
	}
}

// --- search_ontology ---

func TestSearchOntologyReportsUnmatchedTerms(t *testing.T) {
	snapshot := &ontology.Snapshot{
		MeasuringFunctions: []models.Function{
			{Id: "fn-power", Name: "Get Power Consumption", DisplayName: "Power"},
		},
		AspectNodes: []models.AspectNode{{Id: "asp-1", Name: "Building"}},
	}
	_, dispatcher := executorFor(t, Deps{Ontology: &fakeOntology{snapshot: snapshot}}, "search_ontology")

	decoded := dispatchJSON(t, dispatcher, L0, "search_ontology", `{"query":"power quibbleflux"}`)

	// The honest half of a matcher with no thesaurus: a word the ontology has no
	// wording for is a fact about the ontology, and a model that never sees it will
	// assume its vocabulary matched.
	unmatched, ok := decoded["unmatched_terms"].([]any)
	if !ok {
		t.Fatalf("unmatched_terms = %v", decoded["unmatched_terms"])
	}
	found := false
	for _, term := range unmatched {
		if term == "quibbleflux" {
			found = true
		}
	}
	if !found {
		t.Errorf("unmatched terms = %v, want the unknown word reported", unmatched)
	}
}

// The matcher keeps five matches per entity list by default and used to drop the
// rest without saying how many there were. A tool result carrying five functions
// and no total tells the model it has seen the ontology's whole answer, so it
// stops looking — the elision, not the truncation, is what D26 requires here.
func TestSearchOntologyReportsWhatTheMatchLimitElided(t *testing.T) {
	snapshot := &ontology.Snapshot{
		MeasuringFunctions: []models.Function{
			{Id: "fn-consumption", DisplayName: "Power Consumption"},
			{Id: "fn-generation", DisplayName: "Power Generation"},
		},
	}
	_, dispatcher := executorFor(t, Deps{Ontology: &fakeOntology{snapshot: snapshot}}, "search_ontology")

	decoded := dispatchJSON(t, dispatcher, L0, "search_ontology",
		`{"query":"power consumption and generation","limit":1}`)

	elided, ok := decoded["elided"].([]any)
	if !ok || len(elided) != 1 {
		t.Fatalf("elided = %v, want one entry counting the functions that were cut", decoded["elided"])
	}
	entry, _ := elided[0].(map[string]any)
	if entry["field"] != ontology.FieldMatchedFunctions || entry["total"] != float64(2) ||
		entry["shown"] != float64(1) {
		t.Errorf("elided[0] = %v, want matched_functions total 2 shown 1", entry)
	}
}

// resolve_semantic_selection projects the candidate list and passes the rest of
// the document through by embedding, and a field declared at the shallower depth
// of LLMResult wins the JSON tag. The ontology elisions must survive that, or the
// truncation is recorded everywhere except where the model reads.
func TestResolveSemanticSelectionCarriesTheOntologyElisions(t *testing.T) {
	_, dispatcher := executorFor(t, Deps{Selection: &fakeSelection{
		matchElided: []ontology.Elision{{Field: ontology.FieldMatchedFunctions, Total: 40, Shown: 5}},
	}}, "resolve_semantic_selection")

	decoded := dispatchJSON(t, dispatcher, L0, "resolve_semantic_selection", `{"intent":"pv power"}`)

	elided, ok := decoded["match_elided"].([]any)
	if !ok || len(elided) != 1 {
		t.Fatalf("match_elided = %v, want the ontology elision carried through the projection",
			decoded["match_elided"])
	}
	entry, _ := elided[0].(map[string]any)
	if entry["total"] != float64(40) || entry["shown"] != float64(5) {
		t.Errorf("match_elided[0] = %v, want total 40 shown 5", entry)
	}
}

func TestSearchOntologyRequiresAQuery(t *testing.T) {
	_, dispatcher := executorFor(t, Deps{Ontology: &fakeOntology{snapshot: &ontology.Snapshot{}}},
		"search_ontology")
	result := dispatcher.Dispatch(context.Background(), Request{Tier: L0},
		Call{ID: "c", Name: "search_ontology", Input: json.RawMessage(`{}`)})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeInvalidInput)
	}
}

// --- get_sessions ---

// TestGetSessionsRefusesAnUnknownProfile checks the distinction that matters to a
// reader: "no such profile" and "this series has no sessions" are different
// answers, and an empty page would be read as the second.
func TestGetSessionsRefusesAnUnknownProfile(t *testing.T) {
	prof := newTestProfiler(t)
	_, dispatcher := executorFor(t, Deps{Profiler: prof}, "get_sessions")

	result := dispatcher.Dispatch(context.Background(), Request{Tier: L1},
		Call{ID: "c", Name: "get_sessions", Input: json.RawMessage(`{"profile_id":"nope"}`)})

	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeInvalidInput)
	}
	encoded, _ := json.Marshal(result.Content)
	if !strings.Contains(string(encoded), "no profile") {
		t.Errorf("refusal = %s, want it to say the profile is unknown", encoded)
	}
}

// --- degradation ---

// TestSurfaceDegradesPerMissingDependency checks that each tool's availability
// tracks the service it reads through, rather than all-or-nothing.
func TestSurfaceDegradesPerMissingDependency(t *testing.T) {
	// Only the device repository: the metadata tools work, the timeseries ones do
	// not, and each says which configuration is missing.
	registry, err := NewSurface(Deps{Devices: &fakeDevices{device: testDevice()}})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}

	available := names(registry.Available(L2))
	for _, want := range []string{"list_devices", "get_device_metadata"} {
		if !contains(available, want) {
			t.Errorf("%q should be available with a device repository; got %v", want, available)
		}
	}
	for _, absent := range []string{"probe_availability", "preview_series", "quick_profile"} {
		if contains(available, absent) {
			t.Errorf("%q is offered with no timeseries client", absent)
		}
		definition, _ := registry.Lookup(absent)
		if definition.Unavailable == "" {
			t.Errorf("%q gives no reason for being unavailable", absent)
		}
		if !strings.Contains(definition.Unavailable, "timescale_wrapper_url") {
			t.Errorf("%q reason = %q, want it to name the missing setting",
				absent, definition.Unavailable)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// newTestProfiler builds a real profiler over the fake timeseries client, so the
// tools that delegate to it are exercised through the actual code path.
func newTestProfiler(t *testing.T) *profiler.Profiler {
	t.Helper()
	return newTestProfilerWith(t, &fakeTimeseries{points: 10})
}

// newTestProfilerWith is newTestProfiler over a timeseries fake the test
// configured — an availability window, say, which every tool that reads data
// needs before it gets as far as reading.
func newTestProfilerWith(t *testing.T, ts *fakeTimeseries) *profiler.Profiler {
	t.Helper()
	prof, err := profiler.New(
		ts,
		&fakeOntologySource{},
		profiler.NewMemoryStore(),
		profiler.Options{},
	)
	if err != nil {
		t.Fatalf("profiler.New: %v", err)
	}
	return prof
}

type fakeOntologySource struct{}

func (f *fakeOntologySource) Ontology(context.Context, string) (*profiler.OntologyIndex, error) {
	return profiler.NewOntologyIndex(nil, nil, nil), nil
}

// --- what a tool response may cost the model (D26) ---

// testDeviceWithVariables is one service carrying n readable variables, which is
// the breadth a per-item budget cannot bound: eighty candidates or twenty
// profiles are each many times the budget written to bound one of them.
func testDeviceWithVariables(n int) models.ExtendedDevice {
	device := testDevice()
	// Execute is what governs reading a device's data (§5.1), and both tools under
	// test here get as far as the data.
	device.Permissions = models.Permissions{Read: true, Execute: true}
	subs := make([]models.ContentVariable, 0, n)
	for i := 0; i < n; i++ {
		subs = append(subs, models.ContentVariable{
			Id:               fmt.Sprintf("cv-%d", i),
			Name:             fmt.Sprintf("power_%d", i),
			Type:             models.Float,
			CharacteristicId: "char-watt",
		})
	}
	device.DeviceType.Services[0].Outputs[0].ContentVariable.SubContentVariables = subs
	return device
}

// profilableTimeseries answers with a year of history and ten points per read,
// which is what the tools that read data need before they get that far.
func profilableTimeseries() *fakeTimeseries {
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return &fakeTimeseries{
		points:       10,
		availability: serviceWindow("svc-1", to.AddDate(-1, 0, 0), to),
	}
}

// quick_profile ranks a shortlist, and its own answer must not cost more than
// reading the profiles it is shortlisting: unprojected, eighty candidates are
// around forty-eight thousand tokens, two thirds of it bookkeeping.
func TestQuickProfileAnswersWithTheProjectionAndNotTheStoredForm(t *testing.T) {
	fake := &fakeDevices{device: testDeviceWithVariables(3)}
	prof := newTestProfilerWith(t, profilableTimeseries())
	_, dispatcher := executorFor(t, Deps{Devices: fake, Profiler: prof}, "quick_profile")

	decoded := dispatchJSON(t, dispatcher, L0, "quick_profile", `{}`)

	if decoded["tier"] != profiler.TierQuick {
		t.Errorf("tier = %v, want %q stated once for the list", decoded["tier"], profiler.TierQuick)
	}
	if caveat, _ := decoded["caveat"].(string); !strings.Contains(caveat, "order of magnitude") {
		t.Errorf("caveat = %v, want the per-candidate constants stated once", decoded["caveat"])
	}
	if decoded["devices_listed"] != float64(1) {
		t.Errorf("devices_listed = %v, want the listing to survive the projection", decoded["devices_listed"])
	}

	candidates, ok := decoded["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		t.Fatalf("candidates = %v", decoded["candidates"])
	}
	first, ok := candidates[0].(map[string]any)
	if !ok {
		t.Fatalf("candidate = %v", candidates[0])
	}
	if _, present := first["provenance"]; present {
		t.Error("the candidate still carries the provenance sidecar")
	}
	availability, ok := first["availability"].(map[string]any)
	if !ok {
		t.Fatalf("availability = %v", first["availability"])
	}
	if _, present := availability["aggregates"]; present {
		t.Error("the list of materialised aggregates is ODE's read strategy, not the model's")
	}
	if _, present := availability["aggregates_available"]; !present {
		t.Error("whether pre-aggregated variants exist should still be answerable")
	}
}

// ProfileTokenBudget bounds one projection. profile_series profiles every
// variable of a service, so without a bound on the response a twenty-variable
// service answers with twenty times that budget.
func TestProfileSeriesCarriesAtMostTheConfiguredNumberOfProfiles(t *testing.T) {
	fake := &fakeDevices{device: testDeviceWithVariables(5)}
	prof := newTestProfilerWith(t, profilableTimeseries())
	_, dispatcher := executorFor(t,
		Deps{Devices: fake, Profiler: prof, ProfileMaxProfiles: 2}, "profile_series")

	decoded := dispatchJSON(t, dispatcher, L1, "profile_series",
		`{"device_id":"device-1","service_id":"svc-1"}`)

	profiles, ok := decoded["profiles"].([]any)
	if !ok {
		t.Fatalf("profiles = %v", decoded["profiles"])
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles = %d, want the cap of 2", len(profiles))
	}

	notShown, ok := decoded["variables_not_shown"].([]any)
	if !ok || len(notShown) != 3 {
		t.Fatalf("variables_not_shown = %v, want the three that were left out", decoded["variables_not_shown"])
	}
	elided, ok := decoded["elided"].([]any)
	if !ok || len(elided) != 1 {
		t.Fatalf("elided = %v, want one entry counting the profiles", decoded["elided"])
	}
	if !strings.Contains(decoded["note"].(string), "variable_paths") {
		t.Errorf("note = %v, want it to name the way to ask for the rest", decoded["note"])
	}
}

func TestProfileSeriesReturnsOnlyTheVariablePathsThatWereAskedFor(t *testing.T) {
	fake := &fakeDevices{device: testDeviceWithVariables(5)}
	prof := newTestProfilerWith(t, profilableTimeseries())
	_, dispatcher := executorFor(t,
		Deps{Devices: fake, Profiler: prof, ProfileMaxProfiles: 2}, "profile_series")

	decoded := dispatchJSON(t, dispatcher, L1, "profile_series",
		`{"device_id":"device-1","service_id":"svc-1","variable_paths":["value.power_3"]}`)

	profiles, ok := decoded["profiles"].([]any)
	if !ok || len(profiles) != 1 {
		t.Fatalf("profiles = %v, want the one path that was asked for", decoded["profiles"])
	}
	ref := profiles[0].(map[string]any)["series_ref"].(map[string]any)
	if ref["variable_path"] != "value.power_3" {
		t.Errorf("variable_path = %v, want value.power_3", ref["variable_path"])
	}
	if _, present := decoded["elided"]; present {
		t.Error("nothing was cut, so nothing should be reported as elided")
	}
}

// A mistyped path and a service that has no such variable look identical in an
// empty response, and the paths the service does have are what makes either
// fixable.
func TestProfileSeriesRefusesAnUnknownVariablePathAndNamesTheOnesItHas(t *testing.T) {
	fake := &fakeDevices{device: testDeviceWithVariables(3)}
	prof := newTestProfilerWith(t, profilableTimeseries())
	_, dispatcher := executorFor(t, Deps{Devices: fake, Profiler: prof}, "profile_series")

	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", UserSub: "sub-1", SessionID: "sess-1", Tier: L1},
		Call{ID: "c1", Name: "profile_series", Input: json.RawMessage(
			`{"device_id":"device-1","service_id":"svc-1","variable_paths":["value.nonsense"]}`)})

	if result.Outcome == OutcomeOK {
		t.Fatal("an unknown variable_path was answered rather than refused")
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), "value.power_0") {
		t.Errorf("the refusal does not name the paths the service has: %s", encoded)
	}
}

// resolve_semantic_selection returns the same QuickProfiles quick_profile does,
// and it is the tool that starts the flow: unprojected, the first call of a
// conversation would spend tens of thousands of tokens on a shortlist.
func TestResolveSemanticSelectionAnswersWithProjectedCandidates(t *testing.T) {
	candidates := []profiler.QuickProfile{}
	for i := 0; i < 6; i++ {
		candidates = append(candidates, profiler.QuickProfile{
			SeriesRef: profiler.SeriesRef{
				DeviceID: "urn:infai:ses:device:1", ServiceID: "svc-1",
				VariablePath: fmt.Sprintf("value.power_%d", i),
			},
			Device:     profiler.DeviceInfo{Name: "Meter 1", DeviceTypeName: "Power Meter"},
			Tier:       profiler.TierQuick,
			Provenance: profiler.Provenance{"availability": profiler.ProvenanceEntry{Source: profiler.SourceAPI}},
		})
	}
	_, dispatcher := executorFor(t,
		Deps{Selection: &fakeSelection{candidates: candidates}}, "resolve_semantic_selection")

	decoded := dispatchJSON(t, dispatcher, L0, "resolve_semantic_selection",
		`{"intent":"pv power"}`)

	list, ok := decoded["candidates"].([]any)
	if !ok || len(list) != len(candidates) {
		t.Fatalf("candidates = %v, want all %d projected", decoded["candidates"], len(candidates))
	}
	if _, present := list[0].(map[string]any)["provenance"]; present {
		t.Error("the candidates were passed through rather than projected")
	}
	if caveat, _ := decoded["caveat"].(string); caveat == "" {
		t.Error("the projection's caveat is missing")
	}
}

type fakeSelection struct {
	candidates  []profiler.QuickProfile
	matchElided []ontology.Elision
}

func (f *fakeSelection) Resolve(
	context.Context, string, selection.Request,
) (selection.Result, error) {
	return selection.Result{
		Intent:      "pv power",
		Candidates:  f.candidates,
		MatchElided: f.matchElided,
		Notes:       []string{},
	}, nil
}

// --- the export half (§5.3) ---

type fakeExportSource struct {
	definition profiler.ExportDefinition
	asked      []string
}

func (f *fakeExportSource) ExportDefinition(
	_ context.Context, _ string, exportID string,
) (profiler.ExportDefinition, error) {
	f.asked = append(f.asked, exportID)
	definition := f.definition
	if definition.ExportID == "" {
		definition.ExportID = exportID
	}
	return definition, nil
}

func exportDefinition() profiler.ExportDefinition {
	characteristic := "char-watt"
	return profiler.ExportDefinition{
		ExportID: "export-1",
		Name:     "weather history",
		Source:   "import_id",
		SourceID: "import-1",
		Columns: []profiler.ExportColumn{{
			Column: "power", Type: "float", VariablePath: "value.power",
			CharacteristicID: &characteristic,
		}},
	}
}

func exportProfiler(t *testing.T, ts *fakeTimeseries, exports profiler.ExportSource) *profiler.Profiler {
	t.Helper()
	prof, err := profiler.New(ts, &fakeOntologySource{}, profiler.NewMemoryStore(),
		profiler.Options{Exports: exports})
	if err != nil {
		t.Fatalf("profiler.New: %v", err)
	}
	return prof
}

// The tier claim is the point of this tool, and it is checkable from the answer:
// what it asks the platform for is counts.
func TestProbeExportDataIsAvailableAtL0AndReportsNoValueRead(t *testing.T) {
	prof := exportProfiler(t, &fakeTimeseries{points: 10}, &fakeExportSource{definition: exportDefinition()})
	_, dispatcher := executorFor(t, Deps{Profiler: prof}, "probe_export_data")

	decoded := dispatchJSON(t, dispatcher, L0, "probe_export_data", `{"export_id":"export-1"}`)

	export, ok := decoded["export"].(map[string]any)
	if !ok {
		t.Fatalf("export = %v", decoded["export"])
	}
	if export["state"] != string(profiler.ExportFilled) {
		t.Errorf("state = %v, want filled", export["state"])
	}
	if export["reason"] == "" {
		t.Error("every state has to say why")
	}
	reads, ok := decoded["reads"].(map[string]any)
	if !ok || reads["values"] != float64(0) {
		t.Errorf("reads = %v, want values: 0 stated in the answer", decoded["reads"])
	}
	if reads["counts"] != float64(1) {
		t.Errorf("counts = %v, want the one counting query", reads["counts"])
	}
	if note, _ := decoded["note"].(string); !strings.Contains(note, "partly_filled") {
		t.Errorf("note = %v, want it to distinguish the states", decoded["note"])
	}
}

func TestProbeExportDataIsUnavailableWithoutAnalyticsServing(t *testing.T) {
	registry, err := NewSurface(Deps{Timeseries: &fakeTimeseries{}})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, found := registry.Lookup("probe_export_data")
	if !found {
		t.Fatal("probe_export_data is not declared")
	}
	if !strings.Contains(definition.Unavailable, "analytics_serving_url") {
		t.Errorf("reason = %q, want it to name the missing setting", definition.Unavailable)
	}
}

func TestProfileSeriesProfilesAnExportUnderAnExportSeriesRef(t *testing.T) {
	prof := exportProfiler(t, &fakeTimeseries{points: 10}, &fakeExportSource{definition: exportDefinition()})
	_, dispatcher := executorFor(t, Deps{Devices: &fakeDevices{}, Profiler: prof, ProfileMaxProfiles: 5},
		"profile_series")

	decoded := dispatchJSON(t, dispatcher, L1, "profile_series", `{"export_id":"export-1"}`)

	profiles, ok := decoded["profiles"].([]any)
	if !ok || len(profiles) != 1 {
		t.Fatalf("profiles = %v, want one per readable column", decoded["profiles"])
	}
	ref := profiles[0].(map[string]any)["series_ref"].(map[string]any)
	if ref["export_id"] != "export-1" || ref["variable_path"] != "power" {
		t.Errorf("series_ref = %v, want the export and its column", ref)
	}
	if _, present := ref["device_id"]; present {
		t.Errorf("series_ref = %v, want no device id on an export profile", ref)
	}
}

// One table or the other. An input naming both is a mistake rather than a
// narrower request, and answering it would have to pick one silently.
func TestProfileSeriesRefusesAnExportAndADeviceInOneCall(t *testing.T) {
	prof := exportProfiler(t, &fakeTimeseries{points: 10}, &fakeExportSource{definition: exportDefinition()})
	_, dispatcher := executorFor(t, Deps{Devices: &fakeDevices{device: testDevice()}, Profiler: prof},
		"profile_series")

	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", UserSub: "sub-1", SessionID: "sess-1", Tier: L1},
		Call{ID: "c1", Name: "profile_series", Input: json.RawMessage(
			`{"export_id":"export-1","device_id":"device-1","service_id":"svc-1"}`)})

	if result.Outcome == OutcomeOK {
		t.Fatal("a call naming both an export and a device was answered rather than refused")
	}
}

func TestPreviewSeriesAddressesAnExportByItsColumn(t *testing.T) {
	ts := &fakeTimeseries{points: 5}
	_, dispatcher := executorFor(t, Deps{Timeseries: ts, PreviewMaxPoints: 100}, "preview_series")

	decoded := dispatchJSON(t, dispatcher, L2, "preview_series",
		`{"export_id":"export-1","variable_path":"power"}`)

	ref := decoded["series_ref"].(map[string]any)
	if ref["export_id"] != "export-1" {
		t.Errorf("series_ref = %v, want the export", ref)
	}
	element := ts.lastElement(t)
	if element.ExportId == nil || *element.ExportId != "export-1" {
		t.Errorf("element = %+v, want it addressed at the export", element)
	}
	if element.DeviceId != nil || element.ServiceId != nil {
		t.Errorf("element = %+v, want no device beside the export", element)
	}
}

// estimate_read_cost takes both halves of the platform, and the export half has
// one absence that must not read as a zero.
func TestEstimateReadCostTakesExportsAndSaysWhenTheAccountingHasNoRow(t *testing.T) {
	ts := &fakeTimeseries{exportUsage: []timeseries.Usage{}}
	_, dispatcher := executorFor(t, Deps{Timeseries: ts, DeviceLimit: 10}, "estimate_read_cost")

	decoded := dispatchJSON(t, dispatcher, L0, "estimate_read_cost", `{"export_ids":["export-1"]}`)

	missing, ok := decoded["exports_not_accounted"].([]any)
	if !ok || len(missing) != 1 || missing[0] != "export-1" {
		t.Fatalf("exports_not_accounted = %v, want the export with no usage row", decoded["exports_not_accounted"])
	}
	note, _ := decoded["exports_not_accounted_note"].(string)
	if !strings.Contains(note, "not evidence") {
		t.Errorf("note = %q, want it to say an absent row proves nothing", note)
	}
	if !strings.Contains(note, "probe_export_data") {
		t.Errorf("note = %q, want it to name the tool that answers the question", note)
	}
}
