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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/gorilla/websocket"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/identifiers"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// The M6 surface, end to end through the routes and over the *real* profiler
// (§5.5).
//
// The profiler is not faked here on purpose. The relational pass rests on
// activity_pattern — a detected threshold, a hysteresis band and a classification —
// and a fake pattern would let the two halves agree with each other while disagreeing
// with what the developer sees in the profiler view. So the fixture is a series the
// real detectors read as session-based, and the rule that comes out is the one §6's
// acceptance criterion asks for.

const (
	lightsDeviceID = "urn:infai:ses:device:2"
	// siteDeviceID is the meter the graph reaches and the aspect does not: it is not
	// "in the kitchen", which is exactly why a graph-derived set may not be intersected
	// with the aspect's own devices.
	siteDeviceID   = "urn:infai:ses:device:3"
	kitchenGroupID = "urn:infai:ses:device-group:1"
	kitchenGraphID = "urn:infai:ses:graph:1"
	// testUserSub is the subject mintToken puts in every test token. A decision has to
	// be recorded against it rather than against anything in the request body.
	testUserSub = "user-123"
)

// relationTimeseries answers both the profiler's passes and the aligned read by
// evaluating the fixture over whatever window and bucket the element asks for.
type relationTimeseries struct {
	mux      sync.Mutex
	queries  int
	elements [][]timeseries.QueryElement
	token    string
}

func (r *relationTimeseries) DataAvailability(
	_ context.Context, token string, _ string,
) ([]timeseries.Availability, error) {
	r.mux.Lock()
	r.token = token
	r.mux.Unlock()
	from, to := apiFrom, apiNow
	return []timeseries.Availability{{ServiceId: testServiceID, From: &from, To: &to}}, nil
}

func (r *relationTimeseries) DeviceUsage(
	_ context.Context, token string, deviceIDs []string,
) ([]timeseries.Usage, error) {
	r.mux.Lock()
	r.token = token
	r.mux.Unlock()
	out := []timeseries.Usage{}
	for _, id := range deviceIDs {
		out = append(out, timeseries.Usage{DeviceId: id, Bytes: 1 << 20, BytesPerDay: 8640})
	}
	return out, nil
}

func (r *relationTimeseries) ExportUsage(
	_ context.Context, token string, exportIDs []string,
) ([]timeseries.Usage, error) {
	r.mux.Lock()
	r.token = token
	r.mux.Unlock()
	out := []timeseries.Usage{}
	for _, id := range exportIDs {
		out = append(out, timeseries.Usage{ExportId: id, Bytes: 1 << 20, BytesPerDay: 8640})
	}
	return out, nil
}

func (r *relationTimeseries) Query(
	_ context.Context, token string, elements []timeseries.QueryElement, _ timeseries.QueryOptions,
) ([]timeseries.QueryResult, error) {
	r.mux.Lock()
	r.queries++
	r.token = token
	r.elements = append(r.elements, elements)
	r.mux.Unlock()

	out := make([]timeseries.QueryResult, 0, len(elements))
	for index, element := range elements {
		// A raw element carries no groupTime, which is how the profiler's structural
		// pass is told apart from every bucketed read.
		step := quarterHour
		if element.GroupTime != nil {
			if seconds := timeseries.BucketSeconds(*element.GroupTime); seconds > 0 {
				step = time.Duration(seconds) * time.Second
			}
		}
		from, to := apiRawFrom, apiNow
		if element.Time != nil && element.Time.Start != nil && element.Time.End != nil {
			if parsed, err := time.Parse(time.RFC3339, *element.Time.Start); err == nil {
				from = parsed
			}
			if parsed, err := time.Parse(time.RFC3339, *element.Time.End); err == nil {
				to = parsed
			}
		}

		data := make([][][]any, 0, len(element.Columns))
		for range element.Columns {
			rows := [][]any{}
			for at := from; at.Before(to); at = at.Add(step) {
				rows = append(rows, []any{
					at.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
					json.Number(strconv.FormatFloat(kitchenValue(*element.DeviceId, at), 'f', -1, 64)),
				})
			}
			data = append(data, rows)
		}
		out = append(out, timeseries.QueryResult{
			RequestIndex: index,
			DeviceId:     element.DeviceId,
			ServiceId:    element.ServiceId,
			ColumnNames:  apiColumns,
			Data:         data,
		})
	}
	return out, nil
}

// kitchenValue is the motivating case of §5.5 as two power series: the oven runs
// 19:00–22:00 every evening and 10:00–10:30 every morning; the kitchen lights are on
// for the evening run only. Both are strongly bimodal, so the real session detector
// finds an idle/active split, and both have a duty cycle well under half, so neither
// is classified continuous.
func kitchenValue(deviceID string, at time.Time) float64 {
	evening := at.Hour() >= 19 && at.Hour() < 22
	morning := at.Hour() == 10 && at.Minute() < 30

	if deviceID == lightsDeviceID {
		if evening {
			return 60
		}
		return 3
	}
	if evening || morning {
		return 2000
	}
	return 5
}

// kitchenDevices are the devices of one device type — smart plugs of the same model,
// which is why they share a service id.
//
// The site meter is included so a graph neighbour can be *read*, but the fake device
// client's list is filtered below so it does not appear in the aspect resolution: a
// graph reaching outside the aspect is the property under test, and a fixture where
// every device is already in the aspect would prove nothing.
func kitchenDevices() []models.ExtendedDevice {
	oven := apiDevice()
	oven.Name = "Oven"

	lights := apiDevice()
	lights.Device.Id = lightsDeviceID
	lights.Name = "Kitchen lights"

	site := apiDevice()
	site.Device.Id = siteDeviceID
	site.Name = "Site meter"

	return []models.ExtendedDevice{oven, lights, site}
}

// aspectDevices are the two the Kitchen aspect resolves to.
func aspectDevices() []models.ExtendedDevice {
	return kitchenDevices()[:2]
}

type relationHarness struct {
	router     http.Handler
	timeseries *relationTimeseries
	profiler   *profiler.Profiler
	relations  *relations.Service
}

func newRelationHarness(t *testing.T) *relationHarness {
	t.Helper()

	client := &relationTimeseries{}
	deviceClient := &fakeDeviceClient{serve: kitchenDevices(), list: aspectDevices()}
	deviceService := devices.New(deviceClient)
	ontologyRepo := ontology.New(func(string) ontology.Client { return fakeOntologyClient{} }, ontology.Options{})
	index := staticOntology{index: apiOntology()}

	prof, err := profiler.New(client, index, profiler.NewMemoryStore(),
		profiler.Options{Now: func() time.Time { return apiNow }})
	if err != nil {
		t.Fatalf("profiler.New: %v", err)
	}
	resolver, err := selection.New(ontologyRepo, index, deviceService, prof, nil, selection.Options{})
	if err != nil {
		t.Fatalf("selection.New: %v", err)
	}

	service, err := relations.New(relations.Deps{
		Timeseries: client,
		Devices:    deviceService,
		Ontology:   ontologyRepo,
		Selection:  resolver,
		Profiler:   prof,
		// The same index the profiler and selection use: a graph reaches devices the
		// aspect never listed, and their units come from here rather than from a
		// selectables answer.
		OntologyIndex: index,
		Store:         relations.NewMemoryStore(0),
		IDs:           identifiers.New(),
		Now:           func() time.Time { return apiNow },
	})
	if err != nil {
		t.Fatalf("relations.New: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer"},
		api.Deps{
			Ontology:   ontologyRepo,
			Devices:    deviceService,
			Timeseries: client,
			Profiler:   prof,
			Selection:  resolver,
			Relations:  service,
		},
	)
	return &relationHarness{router: router, timeseries: client, profiler: prof, relations: service}
}

func (h *relationHarness) do(t *testing.T, method, path string, body any, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	swapped := &profileHarness{router: h.router}
	return swapped.do(t, method, path, body, roles...)
}

// relationBody is one pass over the fixture window.
func relationRequestBody() map[string]any {
	return map[string]any{
		"members": []any{
			map[string]any{
				"device_id": testDeviceID, "service_id": testServiceID,
				"variable_path": powerPath, "label": "the oven",
			},
			map[string]any{
				"device_id": lightsDeviceID, "service_id": testServiceID,
				"variable_path": powerPath, "label": "the kitchen lights",
			},
		},
		"window": map[string]any{
			"from": apiNow.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
			"to":   apiNow.Format(time.RFC3339),
		},
	}
}

// relationDocument is the slice of the response these tests assert on.
type relationDocument struct {
	RelationID  string  `json:"relation_id"`
	Tier        string  `json:"tier"`
	GroupTime   string  `json:"group_time"`
	GridSeconds float64 `json:"grid_seconds"`
	Buckets     int     `json:"buckets"`
	Observed    int     `json:"observed"`
	Members     []struct {
		Label string `json:"label"`
		State struct {
			Usable          bool    `json:"usable"`
			Threshold       float64 `json:"threshold"`
			ThresholdSource string  `json:"threshold_source"`
			Classification  string  `json:"classification"`
		} `json:"state"`
	} `json:"members"`
	CandidateRules []struct {
		RuleID     string  `json:"rule_id"`
		Statement  string  `json:"statement"`
		Anomaly    string  `json:"anomaly"`
		Confidence float64 `json:"confidence"`
		Lift       float64 `json:"lift"`
		Samples    int     `json:"samples"`
		Violations int     `json:"violations"`
		Strength   string  `json:"strength"`
		Advisory   string  `json:"advisory"`
		Antecedent struct {
			Label string `json:"label"`
			State string `json:"state"`
		} `json:"antecedent"`
		Consequent struct {
			Label string `json:"label"`
			State string `json:"state"`
		} `json:"consequent"`
		Exceptions []struct {
			Dimension  string  `json:"dimension"`
			Bucket     string  `json:"bucket"`
			FromHour   int     `json:"from_hour"`
			ToHour     int     `json:"to_hour"`
			Confidence float64 `json:"confidence"`
		} `json:"exceptions"`
		Decision *struct {
			Action    string `json:"action"`
			CreatedBy string `json:"created_by"`
			Note      string `json:"note"`
		} `json:"decision"`
	} `json:"candidate_rules"`
	Pairs []json.RawMessage `json:"pairs"`
	Reads struct {
		Aligned  int `json:"aligned"`
		Profiles int `json:"profiles"`
		Values   int `json:"values"`
	} `json:"reads"`
	Notes []string `json:"notes"`
}

func (h *relationHarness) createRelation(t *testing.T) relationDocument {
	t.Helper()
	w := h.do(t, http.MethodPost, "/relations", relationRequestBody(), "developer")
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /relations = %d; body %s", w.Code, w.Body.String())
	}
	var document relationDocument
	if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return document
}

func ovenRule(t *testing.T, document relationDocument) int {
	t.Helper()
	for i, rule := range document.CandidateRules {
		if rule.Antecedent.Label == "the oven" &&
			rule.Consequent.Label == "the kitchen lights" &&
			rule.Consequent.State == "active" {
			return i
		}
	}
	statements := []string{}
	for _, rule := range document.CandidateRules {
		statements = append(statements, rule.Statement)
	}
	t.Fatalf("the oven/lights rule is absent; got %v and notes %v", statements, document.Notes)
	return -1
}

// --- tests ---

func TestTheRelationRoutesRejectAnAnonymousCaller(t *testing.T) {
	h := newRelationHarness(t)
	for _, path := range []string{
		"/relations/candidate-sets?aspect_id=kitchen",
		"/relations/x",
		"/relations/rule-decisions?rule_id=rule-1",
	} {
		if w := h.do(t, http.MethodGet, path, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, w.Code)
		}
	}
	if w := h.do(t, http.MethodPost, "/relations", relationRequestBody()); w.Code != http.StatusUnauthorized {
		t.Errorf("POST /relations without a token = %d, want 401", w.Code)
	}
}

func TestTheRelationRoutesRejectATokenWithoutTheDeveloperRole(t *testing.T) {
	h := newRelationHarness(t)
	if w := h.do(t, http.MethodGet, "/relations/candidate-sets?aspect_id=kitchen", nil, "analyst"); w.Code != http.StatusForbidden {
		t.Errorf("candidate sets without the developer role = %d, want 403", w.Code)
	}
	if w := h.do(t, http.MethodPost, "/relations", relationRequestBody(), "analyst"); w.Code != http.StatusForbidden {
		t.Errorf("POST /relations without the developer role = %d, want 403", w.Code)
	}
}

// A deployment without a timescale-wrapper has no profiler and therefore no
// relational profiler, and it answers 404 rather than panicking on the first request.
func TestTheRelationRoutesAreAbsentWhenTheServiceIsNotConfigured(t *testing.T) {
	h := newProfileHarness(t)
	for _, path := range []string{"/relations/candidate-sets?aspect_id=kitchen", "/relations/x"} {
		if w := h.do(t, http.MethodGet, path, nil, "developer"); w.Code != http.StatusNotFound {
			t.Errorf("GET %s with no relational profiler = %d, want 404", path, w.Code)
		}
	}
}

func TestCandidateSetsProposeTheKitchenPairWithoutReadingValues(t *testing.T) {
	h := newRelationHarness(t)

	w := h.do(t, http.MethodGet, "/relations/candidate-sets?aspect_id=kitchen", nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET candidate-sets = %d; body %s", w.Code, w.Body.String())
	}

	var proposal struct {
		AspectID   string `json:"aspect_id"`
		AspectName string `json:"aspect_name"`
		Sets       []struct {
			SetID     string `json:"set_id"`
			Origin    string `json:"origin"`
			Name      string `json:"name"`
			Rationale string `json:"rationale"`
			GraphID   string `json:"graph_id"`
			GraphName string `json:"graph_name"`
			GraphNode string `json:"graph_node"`
			Devices   int    `json:"devices"`
			Members   []struct {
				Ref struct {
					DeviceID     string `json:"device_id"`
					VariablePath string `json:"variable_path"`
				} `json:"ref"`
				Label      string `json:"label"`
				DeviceName string `json:"device_name"`
				Unit       string `json:"unit"`
				FromAspect bool   `json:"from_aspect"`
				Graph      *struct {
					Role    string `json:"role"`
					Weight  int    `json:"weight"`
					ViaName string `json:"via_name"`
					Depth   int    `json:"depth"`
				} `json:"graph"`
			} `json:"members"`
			Notes []string `json:"notes"`
		} `json:"sets"`
		CandidateDevices []json.RawMessage `json:"candidate_devices"`
		Reads            struct {
			Values int `json:"values"`
		} `json:"reads"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &proposal); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if proposal.AspectName != "Kitchen" {
		t.Errorf("aspect_name = %q, want Kitchen", proposal.AspectName)
	}
	if len(proposal.Sets) == 0 {
		t.Fatalf("no set proposed; notes %v", proposal.Notes)
	}
	// The topology comes first, then the asserted membership, then the shared label
	// (§5.5). A graph edge carries direction and share as well as membership, which is
	// more than either of the other two says.
	if proposal.Sets[0].Origin != relations.OriginGraphSiblings {
		t.Errorf("the first set's origin = %q, want %q",
			proposal.Sets[0].Origin, relations.OriginGraphSiblings)
	}
	origins := []string{}
	for _, set := range proposal.Sets {
		origins = append(origins, set.Origin)
	}
	for _, want := range []string{
		relations.OriginGraphSiblings, relations.OriginGraphFlow,
		relations.OriginDeviceGroup, relations.OriginAspectNode,
	} {
		found := false
		for _, origin := range origins {
			if origin == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q set; got %v", want, origins)
		}
	}

	// The sub-metering case over HTTP: the site meter is downstream of the kitchen
	// circuit and is not under the Kitchen aspect, so a proposal that intersected the
	// graph with the aspect's devices would have dropped it.
	var flow *struct {
		SetID     string `json:"set_id"`
		Origin    string `json:"origin"`
		Name      string `json:"name"`
		Rationale string `json:"rationale"`
		GraphID   string `json:"graph_id"`
		GraphName string `json:"graph_name"`
		GraphNode string `json:"graph_node"`
		Devices   int    `json:"devices"`
		Members   []struct {
			Ref struct {
				DeviceID     string `json:"device_id"`
				VariablePath string `json:"variable_path"`
			} `json:"ref"`
			Label      string `json:"label"`
			DeviceName string `json:"device_name"`
			Unit       string `json:"unit"`
			FromAspect bool   `json:"from_aspect"`
			Graph      *struct {
				Role    string `json:"role"`
				Weight  int    `json:"weight"`
				ViaName string `json:"via_name"`
				Depth   int    `json:"depth"`
			} `json:"graph"`
		} `json:"members"`
		Notes []string `json:"notes"`
	}
	for i := range proposal.Sets {
		if proposal.Sets[i].Origin == relations.OriginGraphFlow {
			flow = &proposal.Sets[i]
		}
	}
	if flow == nil {
		t.Fatalf("no flow set; got %v", origins)
	}
	if flow.GraphID != kitchenGraphID {
		t.Errorf("graph_id = %q, want %q", flow.GraphID, kitchenGraphID)
	}
	site := false
	for _, member := range flow.Members {
		if member.Ref.DeviceID != siteDeviceID {
			continue
		}
		site = true
		if member.FromAspect {
			t.Error("the site meter is marked as coming from the aspect")
		}
		if member.Graph == nil || member.Graph.Role != relations.RoleDownstream {
			t.Errorf("the site meter's graph block is %+v, want the downstream role", member.Graph)
		}
		if member.Unit != "W" {
			t.Errorf("the site meter's unit = %q, want it resolved from the ontology", member.Unit)
		}
	}
	if !site {
		t.Errorf("the site meter is absent from the flow set; members %+v", flow.Members)
	}
	for _, set := range proposal.Sets {
		if set.Devices < 2 {
			t.Errorf("set %q spans %d device(s), want at least 2", set.Name, set.Devices)
		}
		if set.Rationale == "" {
			t.Errorf("set %q carries no rationale", set.Name)
		}
	}
	// Tier L0: the answer reports its own read count, which is what makes the claim
	// checkable rather than asserted.
	if proposal.Reads.Values != 0 {
		t.Errorf("values read = %d, want 0", proposal.Reads.Values)
	}
	if h.timeseries.queries != 0 {
		t.Errorf("%d value queries were issued for a tier-L0 operation", h.timeseries.queries)
	}
}

func TestCandidateSetsNeedAnAspectThatExists(t *testing.T) {
	h := newRelationHarness(t)
	if w := h.do(t, http.MethodGet, "/relations/candidate-sets", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("no aspect_id = %d, want 400", w.Code)
	}
	if w := h.do(t, http.MethodGet, "/relations/candidate-sets?aspect_id=nope", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown aspect = %d, want 400", w.Code)
	}
	if w := h.do(t, http.MethodGet, "/relations/candidate-sets?aspect_id=kitchen&include_descendants=maybe",
		nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed include_descendants = %d, want 400", w.Code)
	}
}

// The acceptance criterion of §6, M6: the oven/lights rule surfaces from the aspect
// tree with its exceptions, and is confirmable.
func TestARelationalPassSurfacesTheOvenAndLightsRuleWithItsException(t *testing.T) {
	h := newRelationHarness(t)
	document := h.createRelation(t)

	if document.Tier != "L1" {
		t.Errorf("tier = %q, want L1", document.Tier)
	}
	if document.GridSeconds != 900 {
		t.Errorf("grid = %vs, want the detected 900s sampling interval", document.GridSeconds)
	}
	if document.Buckets == 0 || document.Observed == 0 {
		t.Fatalf("buckets = %d, observed = %d; the pass read nothing",
			document.Buckets, document.Observed)
	}

	// Both members were derived from a real activity_pattern, not a fake one.
	for _, member := range document.Members {
		if !member.State.Usable {
			t.Fatalf("%s yielded no state series", member.Label)
		}
		if member.State.Classification != string(profiler.ActivitySessionBased) {
			t.Errorf("%s was classified %q, want session_based",
				member.Label, member.State.Classification)
		}
		if member.State.ThresholdSource != "detector" {
			t.Errorf("%s used a %q threshold, want the detector's",
				member.Label, member.State.ThresholdSource)
		}
		if member.State.Threshold <= 0 {
			t.Errorf("%s has a threshold of %v", member.Label, member.State.Threshold)
		}
	}

	rule := document.CandidateRules[ovenRule(t, document)]
	if rule.Confidence < 0.7 {
		t.Errorf("confidence = %v, want at least the 0.7 default floor", rule.Confidence)
	}
	if rule.Lift <= 1.2 {
		t.Errorf("lift = %v, want well above the 1.2 floor", rule.Lift)
	}
	if rule.Violations == 0 {
		t.Error("violations = 0, but the morning oven run is the anomaly this rule defines")
	}
	if !strings.Contains(rule.Statement, "the oven") {
		t.Errorf("statement = %q, want it to read as a rule about the oven", rule.Statement)
	}
	if !strings.Contains(rule.Advisory, "candidate") {
		t.Errorf("advisory = %q, want it to say the rule is a candidate", rule.Advisory)
	}
	if rule.Strength == "certain" {
		t.Error("strength = certain; that level is reserved for confirmed values (D23)")
	}

	morning := false
	for _, exception := range rule.Exceptions {
		if exception.Dimension == "hour_of_day" && exception.FromHour == 6 && exception.ToHour == 12 {
			morning = true
			if exception.Confidence >= rule.Confidence {
				t.Errorf("the exception's confidence %v is not below the rule's %v",
					exception.Confidence, rule.Confidence)
			}
		}
	}
	if !morning {
		t.Errorf("no morning exception; got %+v", rule.Exceptions)
	}

	// The read claim: one batched query aligns every member however many there are.
	if document.Reads.Aligned != 1 {
		t.Errorf("aligned reads = %d, want 1", document.Reads.Aligned)
	}
	if document.Reads.Profiles == 0 {
		t.Error("profile reads = 0; the state series have to come from somewhere")
	}
	// Every read is on the caller's behalf (D5).
	if h.timeseries.token == "" {
		t.Error("the platform was read without a token")
	}
}

func TestTheAlignedReadIsOneElementPerDeviceWithOneBucket(t *testing.T) {
	h := newRelationHarness(t)
	h.createRelation(t)

	// The aligned read is the last query the pass makes.
	elements := h.timeseries.elements[len(h.timeseries.elements)-1]
	if len(elements) != 2 {
		t.Fatalf("the aligned read sent %d elements, want one per device", len(elements))
	}
	buckets := map[string]bool{}
	for _, element := range elements {
		if element.GroupTime == nil {
			t.Fatal("an aligned element carried no groupTime")
		}
		buckets[*element.GroupTime] = true
	}
	if len(buckets) != 1 {
		t.Errorf("the aligned elements asked for %d buckets, want 1: %v", len(buckets), buckets)
	}
}

func TestARelationalPassNeedsTwoMembersAndAWholeWindow(t *testing.T) {
	h := newRelationHarness(t)

	body := relationRequestBody()
	body["members"] = []any{map[string]any{
		"device_id": testDeviceID, "service_id": testServiceID, "variable_path": powerPath,
	}}
	if w := h.do(t, http.MethodPost, "/relations", body, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("one member = %d, want 400; body %s", w.Code, w.Body.String())
	}

	body = relationRequestBody()
	body["window"] = map[string]any{"from": apiFrom.Format(time.RFC3339)}
	if w := h.do(t, http.MethodPost, "/relations", body, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("a half-specified window = %d, want 400", w.Code)
	}

	body = relationRequestBody()
	body["members"] = []any{
		map[string]any{"device_id": testDeviceID, "service_id": testServiceID},
		map[string]any{"device_id": lightsDeviceID, "service_id": testServiceID, "variable_path": powerPath},
	}
	if w := h.do(t, http.MethodPost, "/relations", body, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("a member with no variable path = %d, want 400", w.Code)
	}
}

func TestAStoredRelationIsServedByIdAndAnUnknownOneIsNotFound(t *testing.T) {
	h := newRelationHarness(t)
	document := h.createRelation(t)

	w := h.do(t, http.MethodGet, "/relations/"+document.RelationID, nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /relations/%s = %d; body %s", document.RelationID, w.Code, w.Body.String())
	}
	var reread relationDocument
	if err := json.Unmarshal(w.Body.Bytes(), &reread); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reread.RelationID != document.RelationID {
		t.Errorf("relation_id = %q, want %q", reread.RelationID, document.RelationID)
	}
	// The full document keeps the pairwise tables the projection drops.
	if len(reread.Pairs) == 0 {
		t.Error("the stored document carries no pairs; the developer's view is not the projection")
	}

	if w := h.do(t, http.MethodGet, "/relations/rel-nope", nil, "developer"); w.Code != http.StatusNotFound {
		t.Errorf("an unknown relation = %d, want 404", w.Code)
	}
}

// §5.10: a candidate rule is confirmable, the record says who confirmed it, and the
// next read of the relation carries the verdict.
func TestARuleDecisionIsRecordedAgainstTheAuthenticatedDeveloper(t *testing.T) {
	h := newRelationHarness(t)
	document := h.createRelation(t)
	rule := document.CandidateRules[ovenRule(t, document)]

	w := h.do(t, http.MethodPost, "/relations/"+document.RelationID+"/rule-decisions", map[string]any{
		"rule_id": rule.RuleID,
		"action":  "confirm",
		"note":    "matches how the kitchen is used",
	}, "developer")
	if w.Code != http.StatusCreated {
		t.Fatalf("POST rule-decisions = %d; body %s", w.Code, w.Body.String())
	}

	var decision struct {
		DecisionID string `json:"decision_id"`
		CreatedBy  string `json:"created_by"`
		RuleID     string `json:"rule_id"`
		RelationID string `json:"relation_id"`
		Action     string `json:"action"`
		Computed   struct {
			Statement  string  `json:"statement"`
			Confidence float64 `json:"confidence"`
		} `json:"computed"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decision); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decision.CreatedBy != testUserSub {
		t.Errorf("created_by = %q, want the token's subject %q", decision.CreatedBy, testUserSub)
	}
	if decision.Computed.Statement != rule.Statement {
		t.Errorf("computed statement = %q, want the rule as computed %q",
			decision.Computed.Statement, rule.Statement)
	}
	if decision.Computed.Confidence != rule.Confidence {
		t.Errorf("computed confidence = %v, want %v", decision.Computed.Confidence, rule.Confidence)
	}

	// Reading the relation back re-injects it.
	w = h.do(t, http.MethodGet, "/relations/"+document.RelationID, nil, "developer")
	var reread relationDocument
	if err := json.Unmarshal(w.Body.Bytes(), &reread); err != nil {
		t.Fatalf("decode: %v", err)
	}
	decided := reread.CandidateRules[ovenRule(t, reread)]
	if decided.Decision == nil {
		t.Fatal("the rule came back without the decision")
	}
	if decided.Decision.Action != "confirm" || decided.Decision.CreatedBy != testUserSub {
		t.Errorf("decision = %+v, want a confirm by %q", decided.Decision, testUserSub)
	}

	// And the history is readable on its own.
	w = h.do(t, http.MethodGet, "/relations/rule-decisions?rule_id="+rule.RuleID, nil, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("GET rule-decisions = %d", w.Code)
	}
	var log struct {
		RuleID    string            `json:"rule_id"`
		Decisions []json.RawMessage `json:"decisions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &log); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(log.Decisions) != 1 {
		t.Errorf("decisions = %d, want 1", len(log.Decisions))
	}
}

func TestAMalformedRuleDecisionIsRefused(t *testing.T) {
	h := newRelationHarness(t)
	document := h.createRelation(t)
	rule := document.CandidateRules[ovenRule(t, document)]
	path := "/relations/" + document.RelationID + "/rule-decisions"

	if w := h.do(t, http.MethodPost, path, map[string]any{
		"rule_id": rule.RuleID, "action": "maybe",
	}, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown action = %d, want 400", w.Code)
	}
	if w := h.do(t, http.MethodPost, path, map[string]any{
		"rule_id": rule.RuleID, "action": "correct",
	}, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("a correction with no corrected rule = %d, want 400", w.Code)
	}
	if w := h.do(t, http.MethodPost, path, map[string]any{
		"rule_id": "rule-typo", "action": "confirm",
	}, "developer"); w.Code != http.StatusNotFound {
		t.Errorf("a rule the relation does not carry = %d, want 404", w.Code)
	}
	if w := h.do(t, http.MethodPost, "/relations/rel-nope/rule-decisions", map[string]any{
		"rule_id": rule.RuleID, "action": "confirm",
	}, "developer"); w.Code != http.StatusNotFound {
		t.Errorf("an unknown relation = %d, want 404", w.Code)
	}
	if w := h.do(t, http.MethodGet, "/relations/rule-decisions", nil, "developer"); w.Code != http.StatusBadRequest {
		t.Errorf("a decision log with no rule_id = %d, want 400", w.Code)
	}
}

// The relational pass is also on the WebSocket, because it profiles every
// participating service before it reads and a closed tab has to stop paying for that.
func TestTheWebSocketRunsARelationalPassAndReportsItsPhases(t *testing.T) {
	h := newRelationHarness(t)
	server := httptest.NewServer(h.router)
	defer server.Close()

	conn := dialRelationSocket(t, server.URL)
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type": "relate", "id": "op-1", "payload": relationRequestBody(),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	phases := []string{}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("deadline: %v", err)
		}
		var message struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
			Error   string          `json:"error"`
		}
		if err := conn.ReadJSON(&message); err != nil {
			t.Fatalf("read: %v (phases so far %v)", err, phases)
		}
		switch message.Type {
		case "accepted":
			continue
		case "event":
			var phase relations.Phase
			if err := json.Unmarshal(message.Payload, &phase); err != nil {
				t.Fatalf("decode phase: %v", err)
			}
			phases = append(phases, phase.Stage)
		case "error":
			t.Fatalf("relate failed: %s", message.Error)
		case "result":
			var document relationDocument
			if err := json.Unmarshal(message.Payload, &document); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			ovenRule(t, document)
			if len(phases) == 0 {
				t.Error("the pass reported no phase; a multi-minute operation has to show it is alive")
			}
			return
		}
	}
}

func dialRelationSocket(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws"
	header := http.Header{"Authorization": []string{"Bearer " + mintToken([]string{"developer"})}}
	conn, response, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	return conn
}

// TestWriteRelationContractFixtures emits the M6 documents the frontend's contract
// check assigns to its declared types.
//
// Emitted from the handlers rather than captured, for the reason the fixture README
// gives: this route needs two devices in one room with a month of bimodal data behind
// them, which is more setup than a capture script should own, and the guarantee — that
// the backend and the SPA agree on the shape — holds either way, because it is still
// the backend marshalling its own types.
//
//	ODE_WRITE_CONTRACT=frontend/src/__contract__ go test ./pkg/api/ -run ContractFixtures
func TestWriteRelationContractFixtures(t *testing.T) {
	dir := os.Getenv("ODE_WRITE_CONTRACT")
	if dir == "" {
		t.Skip("set ODE_WRITE_CONTRACT to the fixture directory to regenerate")
	}
	h := newRelationHarness(t)

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

	write("relation_sets.json",
		h.do(t, http.MethodGet, "/relations/candidate-sets?aspect_id=kitchen", nil, "developer"))

	document := h.createRelation(t)
	rule := document.CandidateRules[ovenRule(t, document)]
	write("relation.json", h.do(t, http.MethodGet, "/relations/"+document.RelationID, nil, "developer"))
	write("relation_decision.json",
		h.do(t, http.MethodPost, "/relations/"+document.RelationID+"/rule-decisions", map[string]any{
			"rule_id": rule.RuleID, "action": "confirm",
			"note": "matches how the kitchen is used",
		}, "developer"))
	write("relation_decisions.json",
		h.do(t, http.MethodGet, "/relations/rule-decisions?rule_id="+rule.RuleID, nil, "developer"))
}
