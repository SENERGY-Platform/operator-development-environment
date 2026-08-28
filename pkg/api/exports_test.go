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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

const testExportID = "urn:infai:ses:export:1"

type staticExports struct{ definition profiler.ExportDefinition }

func (s staticExports) ExportDefinition(
	_ context.Context, _ string, exportID string,
) (profiler.ExportDefinition, error) {
	definition := s.definition
	if definition.ExportID == "" {
		definition.ExportID = exportID
	}
	return definition, nil
}

// exportHarness is the profile harness with an export source behind the
// profiler. The export's column is named as the device fixture's variable path
// is, so the same series fixture serves both — which is the point: an export is
// read through the same passes.
type exportHarness struct {
	router     http.Handler
	timeseries *exportTimeseries
}

// exportTimeseries answers the export path's three reads in order: the counting
// probe, the raw pass, then the aggregated one. The device fixture's client
// cannot serve it — it answers every read after the first with the aggregated
// triple, and the export path has one more read before that.
type exportTimeseries struct {
	*fakeTimeseries
	responses [][]timeseries.QueryResult
	calls     int
}

func (e *exportTimeseries) Query(
	_ context.Context, _ string, _ []timeseries.QueryElement, _ timeseries.QueryOptions,
) ([]timeseries.QueryResult, error) {
	e.calls++
	if e.calls <= len(e.responses) {
		return e.responses[e.calls-1], nil
	}
	return nil, nil
}

func newExportHarness(t *testing.T) *exportHarness {
	t.Helper()

	raw, buckets := apiSeries()
	fakeTs := &exportTimeseries{
		fakeTimeseries: &fakeTimeseries{rawSeries: raw, buckets: buckets},
		// The count comes back in the same one-element shape a single-column read
		// does, which is what a bucketed count of one column actually is.
		responses: [][]timeseries.QueryResult{raw, raw, buckets},
	}
	characteristic := "ch-watt"
	prof, err := profiler.New(fakeTs, staticOntology{index: apiOntology()}, profiler.NewMemoryStore(),
		profiler.Options{
			Now: func() time.Time { return apiNow },
			Exports: staticExports{definition: profiler.ExportDefinition{
				ExportID: testExportID,
				Name:     "meter history",
				Source:   "import_id",
				SourceID: "urn:infai:ses:import:1",
				Columns: []profiler.ExportColumn{{
					Column: powerPath, Type: "float", VariablePath: "value.power",
					CharacteristicID: &characteristic, FunctionID: "fn-power",
				}},
			}},
		})
	if err != nil {
		t.Fatalf("profiler.New: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer", Debug: false},
		api.Deps{
			Devices:    devices.New(&fakeDeviceClient{serve: []models.ExtendedDevice{apiDevice()}}),
			Timeseries: fakeTs,
			Profiler:   prof,
		},
	)
	return &exportHarness{router: router, timeseries: fakeTs}
}

func (h *exportHarness) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+mintToken([]string{"developer"}))
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *exportHarness) post(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+mintToken([]string{"developer"}))
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func TestExportDataReportsWhetherTheExportHoldsRows(t *testing.T) {
	harness := newExportHarness(t)

	w := harness.get(t, "/timeseries/export-data?export_id="+testExportID)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /timeseries/export-data = %d; body %s", w.Code, w.Body.String())
	}

	var fill struct {
		ExportID string `json:"export_id"`
		State    string `json:"state"`
		Reason   string `json:"reason"`
		Rows     int    `json:"rows"`
		Columns  []struct {
			Column  string `json:"column"`
			Rows    int    `json:"rows"`
			Empty   bool   `json:"empty"`
			Counted bool   `json:"counted"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fill); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fill.ExportID != testExportID {
		t.Errorf("export_id = %q, want the one asked for", fill.ExportID)
	}
	if fill.State != string(profiler.ExportFilled) {
		t.Errorf("state = %q (%s), want filled", fill.State, fill.Reason)
	}
	if fill.Rows == 0 {
		t.Error("rows = 0 beside state filled, which cannot both be true")
	}
	if len(fill.Columns) != 1 || fill.Columns[0].Column != powerPath || !fill.Columns[0].Counted {
		t.Errorf("columns = %+v, want the export's counted column", fill.Columns)
	}
}

func TestExportDataRefusesWithoutAnExportId(t *testing.T) {
	harness := newExportHarness(t)
	if w := harness.get(t, "/timeseries/export-data"); w.Code != http.StatusBadRequest {
		t.Errorf("GET without export_id = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestProfilesTakesAnExportIdInsteadOfADevice(t *testing.T) {
	harness := newExportHarness(t)

	w := harness.post(t, "/profiles", map[string]any{"export_id": testExportID})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /profiles = %d; body %s", w.Code, w.Body.String())
	}

	var response struct {
		Profiles []struct {
			ProfileID string `json:"profile_id"`
			SeriesRef struct {
				DeviceID     string `json:"device_id"`
				ServiceID    string `json:"service_id"`
				ExportID     string `json:"export_id"`
				VariablePath string `json:"variable_path"`
			} `json:"series_ref"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Profiles) != 1 {
		t.Fatalf("profiles = %d, want one per readable column: %s", len(response.Profiles), w.Body.String())
	}
	ref := response.Profiles[0].SeriesRef
	if ref.ExportID != testExportID || ref.VariablePath != powerPath {
		t.Errorf("series_ref = %+v, want the export and its column", ref)
	}
	// The device fields are omitted rather than empty strings, so a reader cannot
	// mistake an export profile for a device profile with lost ids.
	if ref.DeviceID != "" || ref.ServiceID != "" {
		t.Errorf("series_ref = %+v, want no device or service", ref)
	}

	// The stored profile is fetchable under its id like any other.
	got := harness.get(t, "/profiles/"+response.Profiles[0].ProfileID)
	if got.Code != http.StatusOK {
		t.Errorf("GET /profiles/{id} = %d; body %s", got.Code, got.Body.String())
	}
}

func TestProfilesRefusesAnExportAndADeviceTogether(t *testing.T) {
	harness := newExportHarness(t)

	w := harness.post(t, "/profiles", map[string]any{
		"export_id": testExportID, "device_id": testDeviceID, "service_id": testServiceID,
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /profiles with both = %d, want 400; body %s", w.Code, w.Body.String())
	}
}
