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
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
)

// mintToken builds a token with an arbitrary signature. The API gateway
// validates signatures, expiry and audience before a request reaches this
// service, so the suite only needs well-formed claims.
func mintToken(roles []string) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "gateway"}
	claims := map[string]any{
		"sub":                "user-123",
		"preferred_username": "dev",
		"email":              "dev@example.org",
		"realm_access":       map[string]any{"roles": roles},
	}
	return segment(header) + "." + segment(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("signature-checked-at-the-gateway"))
}

func segment(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// --- platform stand-ins ---

type fakeOntologyClient struct{}

func (fakeOntologyClient) GetAspectNodes() ([]models.AspectNode, error, int) {
	return []models.AspectNode{
		{Id: "building", Name: "Building", RootId: "building"},
		{Id: "kitchen", Name: "Kitchen", ParentId: "building", RootId: "building"},
	}, nil, 200
}

func (fakeOntologyClient) GetFunctionsByType(rdfType string) ([]models.Function, error, int) {
	if rdfType == models.SES_ONTOLOGY_MEASURING_FUNCTION {
		return []models.Function{{Id: "fn-power", Name: "power generation", RdfType: rdfType}}, nil, 200
	}
	return []models.Function{{Id: "fn-switch", Name: "set on", RdfType: rdfType}}, nil, 200
}

func (fakeOntologyClient) ListCharacteristics(drmodel.CharacteristicListOptions) ([]models.Characteristic, int64, error, int) {
	return []models.Characteristic{{Id: "ch-w", Name: "Watt", DisplayUnit: "W"}}, 1, nil, 200
}

func (fakeOntologyClient) ListConceptsWithCharacteristics(drmodel.ConceptListOptions) ([]models.ConceptWithCharacteristics, int64, error, int) {
	return []models.ConceptWithCharacteristics{{Id: "concept-power", Name: "Power"}}, 1, nil, 200
}

func (fakeOntologyClient) GetDeviceClasses() ([]models.DeviceClass, error, int) {
	return []models.DeviceClass{{Id: "dc-meter", Name: "Meter"}}, nil, 200
}

func (fakeOntologyClient) GetLastUpdateTimestamps(string, string) ([]drmodel.LastUpdateTimestamp, error, int) {
	return []drmodel.LastUpdateTimestamp{{Collection: "aspects", UnixTimestamp: 1000}}, nil, 200
}

// GetDeviceTypeSelectablesV2 answers for the one device type in this suite, and
// only when the criterion actually addresses it — so a test can tell a resolution
// that found something from one that found nothing.
func (fakeOntologyClient) GetDeviceTypeSelectablesV2(
	query []drmodel.FilterCriteria, _ string, _ bool, _ bool,
) ([]drmodel.DeviceTypeSelectable, error, int) {
	for _, criterion := range query {
		if criterion.FunctionId != "" && criterion.FunctionId != "fn-power" {
			return []drmodel.DeviceTypeSelectable{}, nil, 200
		}
		if criterion.AspectId != "" && criterion.AspectId != "kitchen" {
			return []drmodel.DeviceTypeSelectable{}, nil, 200
		}
		if criterion.Interaction != "" && criterion.Interaction != models.EVENT {
			return []drmodel.DeviceTypeSelectable{}, nil, 200
		}
	}

	device := apiDevice()
	return []drmodel.DeviceTypeSelectable{{
		DeviceTypeId: device.DeviceTypeId,
		Services:     device.DeviceType.Services,
		ServicePathOptions: map[string][]drmodel.ServicePathOption{
			testServiceID: {{
				ServiceId:        testServiceID,
				Path:             powerPath,
				CharacteristicId: "ch-watt",
				FunctionId:       "fn-power",
				AspectNode:       models.AspectNode{Id: "kitchen", Name: "Kitchen", RootId: "building"},
				Type:             models.Float,
				Interaction:      models.EVENT,
			}},
		},
	}}, nil, 200
}

type fakeDeviceClient struct {
	// mux guards gotToken alone. The WebSocket tests read it from the test
	// goroutine while a session goroutine writes it, which the HTTP tests never do.
	mux      sync.Mutex
	gotToken string
	err      error
	code     int

	// serve overrides the canned single device, for the tests that need a device
	// carrying its type and permissions.
	serve []models.ExtendedDevice
	// gotListOptions and gotAction record what the handler asked for, which is how
	// the Read-versus-Execute distinction of §5.1 is checked.
	gotListOptions drmodel.ExtendedDeviceListOptions
	gotAction      drmodel.AuthAction
}

// token is the credential the last call presented.
func (f *fakeDeviceClient) token() string {
	f.mux.Lock()
	defer f.mux.Unlock()
	return f.gotToken
}

func (f *fakeDeviceClient) recordToken(token string) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.gotToken = token
}

func (f *fakeDeviceClient) ListExtendedDevices(token string, options drmodel.ExtendedDeviceListOptions) ([]models.ExtendedDevice, int64, error, int) {
	f.recordToken(token)
	f.gotListOptions = options
	if f.err != nil {
		return nil, 0, f.err, f.code
	}
	if f.serve != nil {
		return f.serve, int64(len(f.serve)), nil, 200
	}
	return []models.ExtendedDevice{{Device: models.Device{Id: "device-1", Name: "PV Meter"}}}, 1, nil, 200
}

func (f *fakeDeviceClient) ReadExtendedDevice(id string, token string, action drmodel.AuthAction, _ bool) (models.ExtendedDevice, error, int) {
	f.recordToken(token)
	f.gotAction = action
	if f.err != nil {
		return models.ExtendedDevice{}, f.err, f.code
	}
	for _, device := range f.serve {
		if device.Id == id {
			return device, nil, 200
		}
	}
	return models.ExtendedDevice{Device: models.Device{Id: id, Name: "PV Meter"}}, nil, 200
}

// --- harness ---

type harness struct {
	router  http.Handler
	devices *fakeDeviceClient
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	deviceClient := &fakeDeviceClient{}
	ontologyRepo := ontology.New(func(string) ontology.Client { return fakeOntologyClient{} }, ontology.Options{})
	deviceService := devices.New(deviceClient)

	// No ranker: this harness has no profiler, which is the deployment without a
	// timescale-wrapper URL. Semantic selection still resolves an intent to series
	// there, so the route is wired and the missing ranking is a note.
	resolver, err := selection.New(ontologyRepo, staticOntology{index: apiOntology()}, deviceService, nil,
		selection.Options{})
	if err != nil {
		t.Fatalf("selection.New: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer", Debug: false},
		api.Deps{
			Ontology:  ontologyRepo,
			Devices:   deviceService,
			Selection: resolver,
		},
	)
	return &harness{router: router, devices: deviceClient}
}

func (h *harness) get(t *testing.T, path string, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if roles != nil {
		req.Header.Set("Authorization", "Bearer "+mintToken(roles))
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *harness) post(t *testing.T, path string, body any, roles ...string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if roles != nil {
		req.Header.Set("Authorization", "Bearer "+mintToken(roles))
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return body
}

// --- tests ---

// Health must not touch the platform: a device-repository outage should not
// take ODE's pods down with it.
func TestHealthNeedsNoToken(t *testing.T) {
	h := newHarness(t)
	w := h.get(t, "/health")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// The platform's developer-swagger-api collects this without a developer token,
// so it has to answer an anonymous caller — and it has to be the specification
// this build actually serves, not a stub.
func TestDocServesTheSpecificationWithoutAToken(t *testing.T) {
	h := newHarness(t)
	w := h.get(t, "/doc")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var spec struct {
		Swagger string `json:"swagger"`
		Info    struct {
			Title string `json:"title"`
		} `json:"info"`
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("the body is not valid json: %v", err)
	}
	if spec.Swagger == "" {
		t.Errorf("no swagger version in the specification")
	}
	if spec.Info.Title == "" {
		t.Errorf("no title in the specification")
	}
	// A route from each milestone, so that a generation run which silently covered
	// only part of pkg/api fails here rather than shipping a partial specification.
	for _, path := range []string{
		"/health", "/session", "/devices/{id}", "/selection", "/profiles",
		"/chat/sessions", "/kernel", "/admin/limits", "/ws",
	} {
		if _, found := spec.Paths[path]; !found {
			t.Errorf("%s is missing from the specification", path)
		}
	}
}

func TestOntologyRoutesRejectAnAnonymousCaller(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/ontology/aspect-tree", "/ontology/aspect-nodes", "/ontology/functions",
		"/ontology/characteristics", "/ontology/concepts", "/ontology/device-classes",
		"/devices", "/devices/device-1", "/session",
	} {
		if w := h.get(t, path); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, w.Code)
		}
	}
}

// SPEC D5: the developer realm role is what separates a platform user from
// someone allowed to use ODE.
func TestRoutesRejectATokenWithoutTheDeveloperRole(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/ontology/aspect-tree", "/devices", "/session"} {
		if w := h.get(t, path, "offline_access"); w.Code != http.StatusForbidden {
			t.Errorf("GET %s without the developer role = %d, want 403", path, w.Code)
		}
	}
}

// M0 acceptance: the aspect tree loads.
func TestAspectTreeLoadsAsAHierarchy(t *testing.T) {
	h := newHarness(t)
	w := h.get(t, "/ontology/aspect-tree", "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}

	tree, ok := decode(t, w)["tree"].([]any)
	if !ok || len(tree) != 1 {
		t.Fatalf("tree = %v, want one root", decode(t, w)["tree"])
	}
	root := tree[0].(map[string]any)
	if root["name"] != "Building" {
		t.Errorf("root name = %v, want Building", root["name"])
	}
	children, ok := root["children"].([]any)
	if !ok || len(children) != 1 {
		t.Fatalf("children = %v, want one", root["children"])
	}
	if children[0].(map[string]any)["name"] != "Kitchen" {
		t.Errorf("child = %v, want Kitchen", children[0])
	}
}

// M0 acceptance: functions load.
func TestFunctionsDefaultToMeasuring(t *testing.T) {
	h := newHarness(t)
	w := h.get(t, "/ontology/functions", "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := decode(t, w)
	if body["rdf_type"] != "measuring" {
		t.Errorf("rdf_type = %v, want measuring", body["rdf_type"])
	}
	fns := body["functions"].([]any)
	if len(fns) != 1 || fns[0].(map[string]any)["id"] != "fn-power" {
		t.Errorf("functions = %v, want the measuring function", fns)
	}
}

func TestFunctionsCanReturnControllingFunctions(t *testing.T) {
	h := newHarness(t)
	w := h.get(t, "/ontology/functions?rdf_type=controlling", "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	fns := decode(t, w)["functions"].([]any)
	if len(fns) != 1 || fns[0].(map[string]any)["id"] != "fn-switch" {
		t.Errorf("functions = %v, want the controlling function", fns)
	}
}

func TestFunctionsRejectAnUnknownRdfType(t *testing.T) {
	h := newHarness(t)
	if w := h.get(t, "/ontology/functions?rdf_type=nonsense", "developer"); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRemainingOntologyRoutesServeTheirSlice(t *testing.T) {
	h := newHarness(t)
	for path, key := range map[string]string{
		"/ontology/aspect-nodes":    "aspect_nodes",
		"/ontology/characteristics": "characteristics",
		"/ontology/concepts":        "concepts",
		"/ontology/device-classes":  "device_classes",
	} {
		w := h.get(t, path, "developer")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
			continue
		}
		if items, ok := decode(t, w)[key].([]any); !ok || len(items) == 0 {
			t.Errorf("GET %s returned no %s", path, key)
		}
	}
}

// M0 acceptance, and the core of D5: the device read must go out under the
// caller's own token, never a service account.
func TestDeviceListReadsOnBehalfOfTheCaller(t *testing.T) {
	h := newHarness(t)
	token := mintToken([]string{"developer"})

	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	if h.devices.token() != "Bearer "+token {
		t.Errorf("upstream token = %q, want the caller's own token", h.devices.token())
	}
}

func TestDeviceListRejectsABadLimit(t *testing.T) {
	h := newHarness(t)
	if w := h.get(t, "/devices?limit=nope", "developer"); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// A 403 from the platform means "you may not see that device". Flattening it
// to 500 would tell the developer ODE is broken instead.
func TestUpstreamForbiddenIsForwardedAsForbidden(t *testing.T) {
	h := newHarness(t)
	h.devices.err = errors.New("forbidden")
	h.devices.code = http.StatusForbidden

	if w := h.get(t, "/devices", "developer"); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// An upstream 401 must not reach the SPA as 401: the caller is authenticated
// with ODE, so it means ODE failed to authenticate upstream. Forwarding it
// sends the SPA into a re-login loop and hides the real fault.
func TestUpstreamUnauthorizedBecomesBadGatewayNotUnauthorized(t *testing.T) {
	h := newHarness(t)
	h.devices.err = errors.New("unauthorized")
	h.devices.code = http.StatusUnauthorized

	w := h.get(t, "/devices", "developer")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "authenticate") {
		t.Errorf("body = %s, want it to name the real cause", w.Body.String())
	}
}

func TestUpstreamNotFoundIsForwarded(t *testing.T) {
	h := newHarness(t)
	h.devices.err = errors.New("no such device")
	h.devices.code = http.StatusNotFound

	if w := h.get(t, "/devices/missing", "developer"); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestUpstreamFailureBecomesBadGateway(t *testing.T) {
	h := newHarness(t)
	h.devices.err = errors.New("connection refused")
	h.devices.code = http.StatusInternalServerError

	if w := h.get(t, "/devices", "developer"); w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestSessionReportsTheAuthenticatedUserAndDefaultTier(t *testing.T) {
	h := newHarness(t)
	w := h.get(t, "/session", "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := decode(t, w)
	if body["user_id"] != "user-123" {
		t.Errorf("user_id = %v, want user-123", body["user_id"])
	}
	// SPEC §3.2: L0 is the default and must be visible to the SPA.
	if body["exposure_tier"] != "L0" {
		t.Errorf("exposure_tier = %v, want L0", body["exposure_tier"])
	}
	if body["is_admin"] != false {
		t.Errorf("is_admin = %v, want false", body["is_admin"])
	}
}
