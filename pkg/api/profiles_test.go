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

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

const (
	testDeviceID  = "urn:infai:ses:device:1"
	testServiceID = "urn:infai:ses:service:11111111-1111-1111-1111-111111111111"
	powerPath     = "value.power"
)

var (
	apiNow      = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	apiFrom     = apiNow.Add(-90 * 24 * time.Hour)
	apiRawFrom  = apiNow.Add(-14 * 24 * time.Hour)
	apiColumns  = []string{powerPath}
	quarterHour = 15 * time.Minute
)

// --- platform stand-ins ---

type fakeTimeseries struct {
	gotToken  string
	queries   int
	availErr  error
	rawSeries []timeseries.QueryResult
	buckets   []timeseries.QueryResult
}

func (f *fakeTimeseries) DataAvailability(_ context.Context, token string, _ string) ([]timeseries.Availability, error) {
	f.gotToken = token
	if f.availErr != nil {
		return nil, f.availErr
	}
	from, to := apiFrom, apiNow
	return []timeseries.Availability{{ServiceId: testServiceID, From: &from, To: &to}}, nil
}

func (f *fakeTimeseries) DeviceUsage(_ context.Context, token string, deviceIDs []string) ([]timeseries.Usage, error) {
	f.gotToken = token
	out := []timeseries.Usage{}
	for _, id := range deviceIDs {
		out = append(out, timeseries.Usage{DeviceId: id, Bytes: 1 << 20, BytesPerDay: 8640})
	}
	return out, nil
}

func (f *fakeTimeseries) ExportUsage(_ context.Context, token string, exportIDs []string) ([]timeseries.Usage, error) {
	f.gotToken = token
	out := []timeseries.Usage{}
	for _, id := range exportIDs {
		out = append(out, timeseries.Usage{ExportId: id, Bytes: 1 << 20, BytesPerDay: 8640})
	}
	return out, nil
}

func (f *fakeTimeseries) Query(_ context.Context, token string, _ []timeseries.QueryElement, _ timeseries.QueryOptions) ([]timeseries.QueryResult, error) {
	f.gotToken = token
	f.queries++
	if f.queries == 1 {
		return f.rawSeries, nil
	}
	return f.buckets, nil
}

type staticOntology struct{ index *profiler.OntologyIndex }

func (s staticOntology) Ontology(context.Context, string) (*profiler.OntologyIndex, error) {
	return s.index, nil
}

// --- fixtures ---

func apiOntology() *profiler.OntologyIndex {
	watt := models.Characteristic{Id: "ch-watt", Name: "Watt", DisplayUnit: "W", MinValue: 0.0, MaxValue: 10000.0}
	return profiler.NewOntologyIndex(
		[]models.Characteristic{watt},
		[]models.ConceptWithCharacteristics{{
			Id: "concept-power", BaseCharacteristicId: "ch-watt",
			Characteristics: []models.Characteristic{watt},
		}},
		[]models.Function{{Id: "fn-power", ConceptId: "concept-power"}},
	)
}

func apiDevice() models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: testDeviceID, Name: "PV Meter", DeviceTypeId: "dt-meter"},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: "dt-meter", Name: "Meter",
			Services: []models.Service{{
				Id: testServiceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: "value", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{{
							Id: "cv-power", Name: "power", Type: models.Float,
							// The aspect is declared because the device repository derives
							// its selectables index from these same content variables: a
							// variable with no aspect could never be matched by an aspect
							// criterion, so a fake that omits it contradicts itself.
							CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectId: "kitchen",
						}},
					},
				}},
			}},
		},
	}
}

func apiSeries() (raw []timeseries.QueryResult, buckets []timeseries.QueryResult) {
	rawRows := [][]any{}
	for at := apiRawFrom; at.Before(apiNow); at = at.Add(quarterHour) {
		hours := float64(at.Unix()%86400) / 3600
		watts := 400 + 300*math.Sin(2*math.Pi*hours/24)
		rawRows = append(rawRows, []any{
			at.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			json.Number(strconv.FormatFloat(watts, 'f', -1, 64)),
		})
	}

	bucketRows := [][]any{}
	for at := apiFrom; at.Before(apiNow); at = at.Add(time.Hour) {
		hours := float64(at.Unix()%86400) / 3600
		watts := 400 + 300*math.Sin(2*math.Pi*hours/24)
		bucketRows = append(bucketRows, []any{
			at.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			json.Number(strconv.FormatFloat(watts, 'f', -1, 64)),
		})
	}

	device, service := testDeviceID, testServiceID
	element := func(index int) timeseries.QueryResult {
		return timeseries.QueryResult{
			RequestIndex: index, DeviceId: &device, ServiceId: &service,
			ColumnNames: apiColumns, Data: [][][]any{bucketRows},
		}
	}
	raw = []timeseries.QueryResult{{
		RequestIndex: 0, DeviceId: &device, ServiceId: &service,
		ColumnNames: apiColumns, Data: [][][]any{rawRows},
	}}
	return raw, []timeseries.QueryResult{element(0), element(1), element(2)}
}

// --- harness ---

type profileHarness struct {
	router     http.Handler
	devices    *fakeDeviceClient
	timeseries *fakeTimeseries
	profiler   *profiler.Profiler
}

func newProfileHarness(t *testing.T) *profileHarness {
	t.Helper()
	return newProfileHarnessWith(t, nil)
}

// newProfileHarnessWith lets a test substitute the profiler's timeseries client —
// the WebSocket tests need one that can block, because a read that returns
// instantly cannot be cancelled.
func newProfileHarnessWith(t *testing.T, client profiler.TimeseriesClient) *profileHarness {
	t.Helper()

	raw, buckets := apiSeries()
	fakeTs := &fakeTimeseries{rawSeries: raw, buckets: buckets}
	deviceClient := &fakeDeviceClient{serve: []models.ExtendedDevice{apiDevice()}}

	// The thin passthrough routes always use the fixture client; only the profiler
	// sees the substitute.
	profilerClient := profiler.TimeseriesClient(fakeTs)
	if client != nil {
		profilerClient = client
	}

	prof, err := profiler.New(profilerClient, staticOntology{index: apiOntology()}, profiler.NewMemoryStore(),
		profiler.Options{Now: func() time.Time { return apiNow }})
	if err != nil {
		t.Fatalf("profiler.New: %v", err)
	}

	ontologyRepo := ontology.New(func(string) ontology.Client { return fakeOntologyClient{} }, ontology.Options{})
	deviceService := devices.New(deviceClient)

	// The profiler is the ranker (§5.2: candidates are ranked by QuickProfile), so
	// this harness exercises the full resolution — including the read counter that
	// makes the tier-L0 claim checkable.
	resolver, err := selection.New(ontologyRepo, staticOntology{index: apiOntology()}, deviceService, prof, nil,
		selection.Options{})
	if err != nil {
		t.Fatalf("selection.New: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer", Debug: false},
		api.Deps{
			Ontology:   ontologyRepo,
			Devices:    deviceService,
			Timeseries: fakeTs,
			Profiler:   prof,
			Selection:  resolver,
		},
	)
	return &profileHarness{router: router, devices: deviceClient, timeseries: fakeTs, profiler: prof}
}

func (h *profileHarness) do(t *testing.T, method, path string, body any, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if roles != nil {
		req.Header.Set("Authorization", "Bearer "+mintToken(roles))
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// createProfiles runs the M1b path and returns the id of the power profile.
func (h *profileHarness) createProfiles(t *testing.T) string {
	t.Helper()
	w := h.do(t, http.MethodPost, "/profiles", map[string]any{
		"device_id": testDeviceID, "service_id": testServiceID,
	}, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /profiles = %d; body %s", w.Code, w.Body.String())
	}

	var response struct {
		Profiles []struct {
			ProfileID string `json:"profile_id"`
			SeriesRef struct {
				VariablePath string `json:"variable_path"`
			} `json:"series_ref"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, profile := range response.Profiles {
		if profile.SeriesRef.VariablePath == powerPath {
			return profile.ProfileID
		}
	}
	t.Fatalf("no profile for %s in %s", powerPath, w.Body.String())
	return ""
}

// --- tests ---

func TestTheProfilerRoutesRejectAnAnonymousCaller(t *testing.T) {
	h := newProfileHarness(t)
	for _, path := range []string{
		"/timeseries/availability?device_id=" + testDeviceID,
		"/timeseries/usage?device_ids=" + testDeviceID,
		"/quick-profiles",
		"/profiles/abc",
		"/profiles/abc/projection",
		"/profiles/abc/sessions",
	} {
		if w := h.do(t, http.MethodGet, path, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, w.Code)
		}
	}
}

func TestTheProfilerRoutesRejectATokenWithoutTheDeveloperRole(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodGet, "/quick-profiles", nil, "offline_access"); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// A deployment without a timescale-wrapper URL keeps the M0 surface rather than
// panicking on the first request.
func TestTheProfilerRoutesAreAbsentWhenTheProfilerIsNotConfigured(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/quick-profiles", "/timeseries/availability?device_id=x"} {
		if w := h.get(t, path, "developer"); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 when unconfigured", path, w.Code)
		}
	}
}

func TestAvailabilityReadsOnBehalfOfTheCaller(t *testing.T) {
	h := newProfileHarness(t)
	token := mintToken([]string{"developer"})

	req := httptest.NewRequest(http.MethodGet, "/timeseries/availability?device_id="+testDeviceID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	if h.timeseries.gotToken != "Bearer "+token {
		t.Errorf("upstream token = %q, want the caller's own", h.timeseries.gotToken)
	}
}

func TestAvailabilityNeedsADeviceId(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodGet, "/timeseries/availability", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestUsageNeedsDeviceIds(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodGet, "/timeseries/usage", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// M1a over HTTP: candidates ranked from metadata, and the response says how many
// value reads it took so the property is checkable from the answer.
func TestQuickProfilesRanksCandidatesWithoutReadingValues(t *testing.T) {
	h := newProfileHarness(t)

	w := h.do(t, http.MethodGet, "/quick-profiles", nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}

	var response struct {
		Candidates []struct {
			Tier      string `json:"tier"`
			SeriesRef struct {
				VariablePath string `json:"variable_path"`
			} `json:"series_ref"`
		} `json:"candidates"`
		Reads struct {
			Values int `json:"values"`
		} `json:"reads"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(response.Candidates) != 1 || response.Candidates[0].SeriesRef.VariablePath != powerPath {
		t.Fatalf("candidates = %+v, want the one power series", response.Candidates)
	}
	if response.Candidates[0].Tier != "quick" {
		t.Errorf("tier = %q, want quick", response.Candidates[0].Tier)
	}
	if response.Reads.Values != 0 {
		t.Errorf("value reads = %d, want 0", response.Reads.Values)
	}
	if h.timeseries.queries != 0 {
		t.Errorf("the timeseries client was queried %d times for a metadata-only listing", h.timeseries.queries)
	}
}

// §5.1: Read governs metadata, Execute governs reading data. Listing candidates
// under Read would offer series ODE cannot read.
func TestQuickProfilesListsDevicesUnderExecuteWithTheirDeviceType(t *testing.T) {
	h := newProfileHarness(t)

	if w := h.do(t, http.MethodGet, "/quick-profiles", nil, "developer"); w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	if h.devices.gotListOptions.Permission != models.Execute {
		t.Errorf("permission = %q, want execute", string(h.devices.gotListOptions.Permission))
	}
	if !h.devices.gotListOptions.FullDt {
		t.Error("the device list was requested without its device type, so no variable can be enumerated")
	}
}

func TestQuickProfilesRejectsAMalformedWindow(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodGet, "/quick-profiles?from=yesterday&to=today", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCreatingProfilesReadsTheDeviceUnderExecute(t *testing.T) {
	h := newProfileHarness(t)
	h.createProfiles(t)

	if h.devices.gotAction != models.Execute {
		t.Errorf("action = %q, want execute: the request is about to read the device's data",
			string(h.devices.gotAction))
	}
	if h.timeseries.queries != 2 {
		t.Errorf("queries = %d, want the raw and the aggregated pass", h.timeseries.queries)
	}
}

func TestCreatingProfilesNeedsADeviceAndAService(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/profiles", map[string]any{"device_id": testDeviceID}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAnUnknownServiceIsRefusedAsABadRequestNotAnOutage(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/profiles", map[string]any{
		"device_id": testDeviceID, "service_id": "urn:infai:ses:service:99999999-9999-9999-9999-999999999999",
	}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestAHalfSpecifiedWindowIsRefused(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/profiles", map[string]any{
		"device_id": testDeviceID, "service_id": testServiceID,
		"analysis_window": map[string]any{"from": apiFrom.Format(time.RFC3339)},
	}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAStoredProfileIsServedWithItsResolutionMap(t *testing.T) {
	h := newProfileHarness(t)
	id := h.createProfiles(t)

	w := h.do(t, http.MethodGet, "/profiles/"+id, nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	if body["profile_id"] != id {
		t.Errorf("profile_id = %v, want %s", body["profile_id"], id)
	}
	if _, present := body["resolution"]; !present {
		t.Error("the response carries no resolution map")
	}
	// D22: provenance stays on the stored form and is only dropped by the
	// projection.
	if _, present := body["provenance"]; !present {
		t.Error("the stored profile carries no provenance")
	}
}

func TestAnUnknownProfileIsNotFound(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodGet, "/profiles/nope", nil, "developer"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestTheProjectionDropsProvenanceAndRecordsElisions(t *testing.T) {
	h := newProfileHarness(t)
	id := h.createProfiles(t)

	w := h.do(t, http.MethodGet, "/profiles/"+id+"/projection", nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	if _, present := body["provenance"]; present {
		t.Error("the projection carries provenance")
	}
	if _, present := body["elided"]; !present {
		t.Error("the projection carries no elided block")
	}
}

func TestTheProjectionRejectsANegativeTokenBudget(t *testing.T) {
	h := newProfileHarness(t)
	id := h.createProfiles(t)

	if w := h.do(t, http.MethodGet, "/profiles/"+id+"/projection?token_budget=-5", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestTheSessionResourceIsServedForAStoredProfile(t *testing.T) {
	h := newProfileHarness(t)
	id := h.createProfiles(t)

	w := h.do(t, http.MethodGet, "/profiles/"+id+"/sessions?limit=10", nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	if _, present := body["total"]; !present {
		t.Error("the page carries no total")
	}
}

func TestSessionsOfAnUnknownProfileAreNotFound(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodGet, "/profiles/nope/sessions", nil, "developer"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSessionsRejectAMalformedTimestamp(t *testing.T) {
	h := newProfileHarness(t)
	id := h.createProfiles(t)

	if w := h.do(t, http.MethodGet, "/profiles/"+id+"/sessions?from=yesterday", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// The override is a developer action, and the author comes from the token rather
// than from the request body.
func TestAnOverrideIsRecordedAgainstTheAuthenticatedDeveloper(t *testing.T) {
	h := newProfileHarness(t)
	id := h.createProfiles(t)

	w := h.do(t, http.MethodPost, "/profiles/"+id+"/overrides", map[string]any{
		"field_path": "value_semantics.unit", "action": "correct",
		"computed_value": "W", "confirmed_value": "kW", "note": "the meter reports kilowatts",
	}, "developer")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", w.Code, w.Body.String())
	}

	var response struct {
		Override struct {
			OverrideID string `json:"override_id"`
			CreatedBy  string `json:"created_by"`
			SeriesRef  struct {
				VariablePath string `json:"variable_path"`
			} `json:"series_ref"`
		} `json:"override"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Override.CreatedBy != "user-123" {
		t.Errorf("created_by = %q, want the token subject", response.Override.CreatedBy)
	}
	if response.Override.SeriesRef.VariablePath != powerPath {
		t.Errorf("series_ref = %+v, want the profile's own series", response.Override.SeriesRef)
	}

	// And it applies on the next read of the profile.
	projection := h.do(t, http.MethodGet, "/profiles/"+id+"/projection", nil, "developer")
	var view struct {
		ValueSemantics struct {
			Unit string `json:"unit"`
		} `json:"value_semantics"`
	}
	if err := json.Unmarshal(projection.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	if view.ValueSemantics.Unit != "kW" {
		t.Errorf("projected unit = %q, want the confirmed kW", view.ValueSemantics.Unit)
	}
}

// A typo in a field path is refused rather than accepted and silently ignored for
// the rest of the project.
func TestAnOverrideOnANonConfirmableFieldIsRefused(t *testing.T) {
	h := newProfileHarness(t)
	id := h.createProfiles(t)

	w := h.do(t, http.MethodPost, "/profiles/"+id+"/overrides", map[string]any{
		"field_path": "distribution.mean", "action": "correct", "confirmed_value": 5,
	}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestAnOverrideOnAnUnknownProfileIsNotFound(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/profiles/nope/overrides", map[string]any{
		"field_path": "value_semantics.unit", "action": "confirm",
	}, "developer")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
