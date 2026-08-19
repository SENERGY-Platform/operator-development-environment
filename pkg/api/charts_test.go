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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/identifiers"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// chartTimeseries answers a chart read: one result per element sent, which is the
// shape /queries/v2 returns and the shape DecodeResults insists on.
type chartTimeseries struct {
	fakeTimeseries
	elements []timeseries.QueryElement
}

func (c *chartTimeseries) Query(
	_ context.Context, token string, elements []timeseries.QueryElement, _ timeseries.QueryOptions,
) ([]timeseries.QueryResult, error) {
	c.gotToken = token
	c.queries++
	c.elements = elements

	device, service := testDeviceID, testServiceID
	out := make([]timeseries.QueryResult, 0, len(elements))
	for i, element := range elements {
		// Bucketed rows inside the window the element asked for, rather than a fixed
		// set: a fake that ignores the window cannot show that a narrower request
		// reads less, which is what the point cap and the zoom both rest on.
		from, to := apiFrom, apiNow
		if element.Time != nil && element.Time.Start != nil && element.Time.End != nil {
			if parsed, err := time.Parse(time.RFC3339, *element.Time.Start); err == nil {
				from = parsed
			}
			if parsed, err := time.Parse(time.RFC3339, *element.Time.End); err == nil {
				to = parsed
			}
		}
		step := 24 * time.Hour
		if element.GroupTime != nil {
			if seconds := timeseries.BucketSeconds(*element.GroupTime); seconds > 0 {
				step = time.Duration(seconds) * time.Second
			}
		}
		rows := [][]any{}
		for at := from; at.Before(to); at = at.Add(step) {
			rows = append(rows, []any{at.UTC().Format(time.RFC3339Nano), json.Number("120.5")})
		}
		out = append(out, timeseries.QueryResult{
			RequestIndex: i, DeviceId: &device, ServiceId: &service,
			ColumnNames: apiColumns, Data: [][][]any{rows},
		})
	}
	return out, nil
}

type chartHarness struct {
	*profileHarness
	router     http.Handler
	timeseries *chartTimeseries
	charts     *charts.Service
}

func newChartHarness(t *testing.T) *chartHarness {
	t.Helper()

	base := newProfileHarness(t)
	chartTs := &chartTimeseries{}
	deviceService := devices.New(&fakeDeviceClient{serve: []models.ExtendedDevice{apiDevice()}})

	service, err := charts.New(charts.Deps{
		Timeseries: chartTs,
		Devices:    deviceService,
		Ontology:   staticOntology{index: apiOntology()},
		// The profiler's own store, so a profile created over the M1b route is the
		// profile a chart annotates and confirms against.
		Profiles: base.profiler.Store(),
		Store:    charts.NewMemoryStore(0),
		IDs:      identifiers.New(),
		Now:      func() time.Time { return apiNow },
	})
	if err != nil {
		t.Fatalf("charts.New: %v", err)
	}

	ontologyRepo := ontology.New(func(string) ontology.Client { return fakeOntologyClient{} }, ontology.Options{})
	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer"},
		api.Deps{
			Ontology:   ontologyRepo,
			Devices:    deviceService,
			Timeseries: chartTs,
			Profiler:   base.profiler,
			Charts:     service,
		},
	)
	return &chartHarness{profileHarness: base, router: router, timeseries: chartTs, charts: service}
}

// do sends against the chart router rather than the profiler harness's.
func (h *chartHarness) do(t *testing.T, method, path string, body any, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	swapped := &profileHarness{router: h.router}
	return swapped.do(t, method, path, body, roles...)
}

func (h *chartHarness) createChart(t *testing.T, body any) map[string]any {
	t.Helper()
	w := h.do(t, http.MethodPost, "/charts", body, "developer")
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /charts = %d; body %s", w.Code, w.Body.String())
	}
	return decode(t, w)
}

func chartSpecBody(profileID string) map[string]any {
	series := map[string]any{
		"ref": map[string]any{
			"device_id": testDeviceID, "service_id": testServiceID, "variable_path": powerPath,
		},
		"label": "PV power",
	}
	if profileID != "" {
		series["profile_id"] = profileID
	}
	return map[string]any{
		"title":   "PV generation",
		"caption": "the week the inverter was replaced",
		"series":  []any{series},
		"window": map[string]any{
			"from": apiFrom.Format(time.RFC3339), "to": apiNow.Format(time.RFC3339),
		},
	}
}

// otherDeveloperToken is a second developer. mintToken always carries the same
// subject, and ownership is a property of the subject, so the check needs one of
// its own.
func otherDeveloperToken() string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "gateway"}
	claims := map[string]any{
		"sub":                "user-456",
		"preferred_username": "other",
		"realm_access":       map[string]any{"roles": []string{"developer"}},
	}
	return segment(header) + "." + segment(claims) + ".signature-checked-at-the-gateway"
}

// request sends with a token this test minted itself, rather than one derived from
// a role list.
func (h *chartHarness) request(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// --- tests ---

func TestTheChartRoutesRejectAnAnonymousCaller(t *testing.T) {
	h := newChartHarness(t)
	for _, path := range []string{"/charts", "/charts/x", "/charts/x/data"} {
		if w := h.do(t, http.MethodGet, path, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, w.Code)
		}
	}
	if w := h.do(t, http.MethodPost, "/charts", chartSpecBody(""), "analyst"); w.Code != http.StatusForbidden {
		t.Errorf("POST /charts without the developer role = %d, want 403", w.Code)
	}
}

func TestTheChartRoutesAreAbsentWithoutAnExplorationBackend(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodGet, "/charts", nil, "developer"); w.Code != http.StatusNotFound {
		t.Errorf("GET /charts on a deployment with no charts service = %d, want 404", w.Code)
	}
}

func TestCreatingAChartResolvesItsUnitWithoutReadingValues(t *testing.T) {
	h := newChartHarness(t)
	created := h.createChart(t, chartSpecBody(""))

	if h.timeseries.queries != 0 {
		t.Errorf("%d value queries while creating a specification, want none", h.timeseries.queries)
	}
	axis, ok := created["y_axis"].(map[string]any)
	if !ok || axis["unit"] != "W" {
		t.Errorf("y_axis = %v, want W resolved from the ontology", created["y_axis"])
	}
	spec, ok := created["spec"].(map[string]any)
	if !ok || spec["chart_id"] == "" {
		t.Fatalf("spec = %v, want a stored chart", created["spec"])
	}
	if spec["author"] != "developer" {
		t.Errorf("author = %v, want developer on the HTTP route", spec["author"])
	}
}

func TestCreatingAChartRefusesATransformItCannotResolve(t *testing.T) {
	h := newChartHarness(t)
	body := chartSpecBody("")
	body["series"].([]any)[0].(map[string]any)["transform"] = "convert:urn:infai:ses:characteristic:celsius"

	w := h.do(t, http.MethodPost, "/charts", body, "developer")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST /charts with an unreachable conversion = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestChartDataCarriesValuesAndTheProfilersAnnotations(t *testing.T) {
	h := newChartHarness(t)
	profileID := h.profileHarness.createProfiles(t)
	created := h.createChart(t, chartSpecBody(profileID))
	chartID := created["spec"].(map[string]any)["chart_id"].(string)

	w := h.do(t, http.MethodGet, "/charts/"+chartID+"/data", nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET data = %d; body %s", w.Code, w.Body.String())
	}
	data := decode(t, w)

	series, ok := data["series"].([]any)
	if !ok || len(series) != 1 {
		t.Fatalf("series = %v, want one", data["series"])
	}
	points, ok := series[0].(map[string]any)["points"].([]any)
	if !ok || len(points) == 0 {
		t.Fatal("the chart carries no points, so there is nothing to draw")
	}
	if h.timeseries.queries != 1 {
		t.Errorf("%d queries for one chart, want one batched read", h.timeseries.queries)
	}
	if _, present := data["annotations"]; !present {
		t.Error("no annotations field; a chart of a profiled series should carry its detections")
	}
	if data["reads"] == nil {
		t.Error("no read counters, so the cost of the chart cannot be checked from the answer")
	}
}

func TestAnotherDeveloperCannotReadTheChart(t *testing.T) {
	h := newChartHarness(t)
	created := h.createChart(t, chartSpecBody(""))
	chartID := created["spec"].(map[string]any)["chart_id"].(string)

	w := h.request(t, http.MethodGet, "/charts/"+chartID, otherDeveloperToken())
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /charts/{id} as another developer = %d, want 404 — an owner-scoped id must not "+
			"distinguish someone else's chart from a missing one", w.Code)
	}
}

func TestConfirmingFromAChartWritesTheProfilersOverlay(t *testing.T) {
	h := newChartHarness(t)
	created := h.createChart(t, chartSpecBody(""))
	chartID := created["spec"].(map[string]any)["chart_id"].(string)

	w := h.do(t, http.MethodPost, "/charts/"+chartID+"/confirmations", map[string]any{
		"series_index":    0,
		"field_path":      "value_semantics.unit",
		"action":          "correct",
		"confirmed_value": "kW",
		"note":            "the inverter reports kilowatts",
	}, "developer")
	if w.Code != http.StatusCreated {
		t.Fatalf("POST confirmations = %d; body %s", w.Code, w.Body.String())
	}
	confirmed := decode(t, w)
	override, ok := confirmed["override"].(map[string]any)
	if !ok || override["computed_value"] != "W" {
		t.Errorf("override = %v, want the resolver's W recorded as the computed value", confirmed["override"])
	}

	// The same overlay the profiler reads: a confirmation taken from a chart has to
	// reach the next profile of that series (§5.10).
	overrides := h.profileHarness.profiler.Store().Overrides(profiler.SeriesRef{
		DeviceID: testDeviceID, ServiceID: testServiceID, VariablePath: powerPath,
	})
	if len(overrides) != 1 {
		t.Fatalf("%d overrides in the profiler's overlay, want the one made from the chart", len(overrides))
	}

	data := decode(t, h.do(t, http.MethodGet, "/charts/"+chartID+"/data", nil, "developer"))
	axis := data["y_axis"].(map[string]any)
	if axis["unit"] != "kW" || axis["confirmed"] != true {
		t.Errorf("axis = %v, want a confirmed kW", axis)
	}
}

func TestConfirmingAComputedStatisticIsRefused(t *testing.T) {
	h := newChartHarness(t)
	created := h.createChart(t, chartSpecBody(""))
	chartID := created["spec"].(map[string]any)["chart_id"].(string)

	w := h.do(t, http.MethodPost, "/charts/"+chartID+"/confirmations", map[string]any{
		"series_index": 0, "field_path": "distribution.mean", "action": "confirm",
	}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Errorf("confirming distribution.mean = %d, want 400: §5.10 fixes the confirmable set", w.Code)
	}
}

func TestListingAndDiscardingCharts(t *testing.T) {
	h := newChartHarness(t)
	created := h.createChart(t, chartSpecBody(""))
	chartID := created["spec"].(map[string]any)["chart_id"].(string)

	listed := decode(t, h.do(t, http.MethodGet, "/charts", nil, "developer"))
	if listed["count"] != float64(1) {
		t.Errorf("count = %v, want 1", listed["count"])
	}
	if w := h.do(t, http.MethodDelete, "/charts/"+chartID, nil, "developer"); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	if w := h.do(t, http.MethodGet, "/charts/"+chartID, nil, "developer"); w.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", w.Code)
	}
}

func TestSessionReportsTheChartCapability(t *testing.T) {
	h := newChartHarness(t)
	session := decode(t, h.do(t, http.MethodGet, "/session", nil, "developer"))
	features, ok := session["features"].(map[string]any)
	if !ok || features["charts"] != true {
		t.Errorf("features = %v, want charts true", session["features"])
	}
}

// TestWriteChartContractFixtures emits the M5 documents the frontend's contract
// check assigns to its declared types.
//
// Emitted from the handlers rather than captured, for the reason the fixture README
// gives: these routes need a platform with data behind them, and the guarantee this
// check provides — that the backend and the SPA agree on the shape — holds either
// way, because it is still the backend marshalling its own types.
//
//	ODE_WRITE_CONTRACT=frontend/src/__contract__ go test ./pkg/api/ -run ContractFixtures
func TestWriteChartContractFixtures(t *testing.T) {
	dir := os.Getenv("ODE_WRITE_CONTRACT")
	if dir == "" {
		t.Skip("set ODE_WRITE_CONTRACT to the fixture directory to regenerate")
	}
	h := newChartHarness(t)
	profileID := h.profileHarness.createProfiles(t)
	created := h.createChart(t, chartSpecBody(profileID))
	chartID := created["spec"].(map[string]any)["chart_id"].(string)

	write := func(file string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("%s: %d; body %s", file, w.Code, w.Body.String())
		}
		var parsed any
		if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		encoded, err := json.MarshalIndent(parsed, "", "  ")
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		t.Logf("wrote %s", file)
	}

	write("chart_created.json", h.do(t, http.MethodPost, "/charts", chartSpecBody(profileID), "developer"))
	write("charts.json", h.do(t, http.MethodGet, "/charts", nil, "developer"))
	// A narrow window and a coarse bucket on purpose: the fixture is a shape check,
	// and a thousand points would make it a large file that says nothing more than a
	// dozen do.
	narrow := "?from=" + apiNow.Add(-72*time.Hour).Format(time.RFC3339) +
		"&to=" + apiNow.Format(time.RFC3339) + "&group_time=6h"
	write("chart_data.json", h.do(t, http.MethodGet, "/charts/"+chartID+"/data"+narrow, nil, "developer"))
	write("chart_confirmation.json", h.do(t, http.MethodPost, "/charts/"+chartID+"/confirmations",
		map[string]any{
			"series_index": 0, "field_path": "value_semantics.unit",
			"action": "correct", "confirmed_value": "kW", "note": "the inverter reports kilowatts",
		}, "developer"))
}
