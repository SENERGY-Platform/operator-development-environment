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
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// fakeCharts records what render_chart asked the exploration pane to store.
type fakeCharts struct {
	requests []charts.CreateRequest
	token    string
}

func (f *fakeCharts) Create(
	_ context.Context, token string, req charts.CreateRequest,
) (charts.Created, error) {
	f.token = token
	f.requests = append(f.requests, req)
	spec := charts.Spec{
		ChartID:   "chart-1",
		Title:     req.Title,
		Series:    req.Series,
		Window:    req.Window,
		GroupTime: req.GroupTime,
		Author:    req.Author,
		CreatedBy: req.UserSub,
		SessionID: req.SessionID,
	}
	return charts.Created{
		Spec: spec,
		Series: []charts.SeriesResolution{{
			Index: 0, Ref: req.Series[0].Ref, Transform: req.Series[0].Transform,
			Unit: charts.Unit{Unit: "W", UnitSource: profiler.UnitFromCharacteristic},
		}},
		Axis:  charts.Axis{Unit: charts.Unit{Unit: "W"}, From: "series"},
		Notes: []string{},
	}, nil
}

const chartCall = `{
  "title": "PV generation",
  "series": [{"device_id": "d1", "service_id": "s1", "variable_path": "value.power",
              "transform": "resample:900s", "profile_id": "p1", "label": "PV"}],
  "annotations": [{"from": "2026-03-01T00:00:00Z", "to": "2026-03-01T06:00:00Z",
                   "label": "night", "severity": "info", "series_index": 0,
                   "confirmable": true, "field_path": "activity_pattern.sessions"}],
  "markers": [{"at": "2026-03-02T00:00:00Z", "label": "counter reset"}],
  "window": {"from": "2026-03-01T00:00:00Z", "to": "2026-03-08T00:00:00Z"},
  "group_time": "15m"
}`

// The tier assignment of render_chart is the interesting part of M5, so it is
// asserted rather than assumed: L1 because a chart shows values, and no values in
// the result because the developer's browser is what reads them.
func TestRenderChartEmitsASpecificationAndNoValues(t *testing.T) {
	sink := &fakeCharts{}
	definition, dispatcher := executorFor(t, Deps{Charts: sink}, "render_chart")
	if definition.MinTier != L1 {
		t.Errorf("render_chart min tier = %v, §5.8 says L1", definition.MinTier)
	}

	out := dispatchJSON(t, dispatcher, L1, "render_chart", chartCall)

	if out["chart_id"] != "chart-1" {
		t.Errorf("chart_id = %v, want the stored chart", out["chart_id"])
	}
	if out["values_read"] != float64(0) {
		t.Errorf("values_read = %v, want 0 — the model must be able to check the claim", out["values_read"])
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{`"points"`, `"values"`, `"v":`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the result carries %s; render_chart must not return values", forbidden)
		}
	}

	if len(sink.requests) != 1 {
		t.Fatalf("%d specifications stored, want 1", len(sink.requests))
	}
	request := sink.requests[0]
	if request.Author != charts.AuthorLLM {
		t.Errorf("author = %q, want llm — the dispatcher stamps it, not the model", request.Author)
	}
	if request.UserSub != "sub-1" || request.SessionID != "sess-1" {
		t.Errorf("stored for %q in %q, want the dispatching developer and session",
			request.UserSub, request.SessionID)
	}
	if sink.token != "Bearer t" {
		t.Errorf("token = %q, want the caller's: every platform read is on behalf of the developer",
			sink.token)
	}
	if len(request.Annotations) != 1 || !request.Annotations[0].Confirmable {
		t.Errorf("annotations = %+v, want the confirmable band the call asked for", request.Annotations)
	}
	if request.Annotations[0].From.IsZero() || request.Markers[0].At.IsZero() {
		t.Error("the RFC3339 timestamps did not survive decoding")
	}
	if request.Series[0].ProfileID != "p1" {
		t.Error("the profile the annotations come from was dropped")
	}
}

func TestRenderChartIsBlockedAtL0(t *testing.T) {
	sink := &fakeCharts{}
	_, dispatcher := executorFor(t, Deps{Charts: sink}, "render_chart")

	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", UserSub: "sub-1", Tier: L0},
		Call{ID: "c1", Name: "render_chart", Input: json.RawMessage(chartCall)})

	if result.Outcome != OutcomeBlockedByTier {
		t.Errorf("outcome = %v, want %v", result.Outcome, OutcomeBlockedByTier)
	}
	if len(sink.requests) != 0 {
		t.Error("the specification was stored although the tier gate refused the call")
	}
}

func TestRenderChartIsUnavailableWithoutTheExplorationBackend(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, found := registry.Lookup("render_chart")
	if !found {
		t.Fatal("render_chart is not declared")
	}
	if definition.Implemented() {
		t.Error("render_chart has an executor with no charts service behind it")
	}
	if !strings.Contains(definition.Unavailable, "timescale_wrapper_url") {
		t.Errorf("unavailable = %q, want it to name what this deployment is missing", definition.Unavailable)
	}
}

func TestRenderChartRefusesACallWithNoSeries(t *testing.T) {
	_, dispatcher := executorFor(t, Deps{Charts: &fakeCharts{}}, "render_chart")
	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", UserSub: "sub-1", Tier: L1},
		Call{ID: "c1", Name: "render_chart", Input: json.RawMessage(`{"series": []}`)})
	if result.Outcome != OutcomeInvalidInput {
		t.Errorf("outcome = %v, want an error the model can correct", result.Outcome)
	}
}
