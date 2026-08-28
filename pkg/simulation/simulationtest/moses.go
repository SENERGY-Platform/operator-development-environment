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

// Package simulationtest is MOSES's environment api, in memory.
//
// It lives outside the test files for the reason kerneltest and experimentstest
// do: two packages need it — pkg/simulation tests the client against it and
// pkg/tools tests the executors on top — and a second copy of the same double
// would drift from the first.
//
// What it fakes is the *contract*, and it is deliberately strict about the parts
// of that contract a client gets wrong quietly:
//
//   - It assigns ids where the document left them empty and provisions a device
//     id for every asset that names a type and carries no reference, which is what
//     MOSES does. A client that strips external_ref on the way out therefore sees a
//     second device appear here, in a test, rather than in somebody's inventory.
//   - It counts versions and refuses a stale write with 409, in MOSES's own
//     message shape.
//   - It never lets a client set external_managed or external_graph_ref, and
//     records every attempt so a test can assert that ODE sent neither.
//
// What it does not do is simulate anything: no environment ever runs here unless
// a test says it does, which is the state a freshly written document is really in.
package simulationtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// MOSES is the environment api.
type MOSES struct {
	server *httptest.Server

	mux sync.Mutex
	// environments is the store, by id.
	environments map[string]map[string]any
	// order records ids in creation order, so a listing is stable.
	order []string
	// datasets is the upload store, by id.
	datasets map[string]map[string]any
	// datasetContent is what was uploaded, so a test can assert the bytes that
	// travelled rather than only that a call happened.
	datasetContent map[string][]byte
	// deviceTypes is the catalogue. Empty until a test sets one, which is the
	// honest default: a platform with no simulatable device type is a real
	// deployment and the answer to it should be a refusal, not a panic.
	deviceTypes []any
	// running names the environments this instance simulates. A document that was
	// just written is not among them.
	running map[string]bool
	// state is the live state per running environment.
	state map[string]map[string]any
	// backfills is the job per environment.
	backfills map[string]map[string]any

	// Tokens records the Authorization header of every request, so a test can
	// assert the developer's own token was forwarded and no service account was
	// invented.
	Tokens []string
	// Requests counts calls by "METHOD /path", for asserting that a
	// read-modify-write read before it wrote.
	Requests map[string]int
	// EchoedServerOwned records any attempt to set a field MOSES reconciles. It
	// should stay empty for every write ODE makes.
	EchoedServerOwned []string

	// nextID numbers assigned ids so they are readable in a failure message.
	nextID int
	// Fail, when set, makes the next matching request answer with this code and
	// body. Keyed by "METHOD /path"; the entry is consumed.
	Fail map[string]FakeFailure
}

// FakeFailure is a canned refusal.
type FakeFailure struct {
	Code int
	Body string
}

// New starts the fake and registers its shutdown.
func New(t *testing.T) *MOSES {
	t.Helper()
	m := &MOSES{
		environments:   map[string]map[string]any{},
		datasets:       map[string]map[string]any{},
		datasetContent: map[string][]byte{},
		running:        map[string]bool{},
		state:          map[string]map[string]any{},
		backfills:      map[string]map[string]any{},
		Requests:       map[string]int{},
		Fail:           map[string]FakeFailure{},
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

// URL is what a client is configured with.
func (m *MOSES) URL() string { return m.server.URL }

// SetDeviceTypes installs the catalogue.
func (m *MOSES) SetDeviceTypes(types ...any) {
	m.mux.Lock()
	defer m.mux.Unlock()
	m.deviceTypes = types
}

// SetRunning marks an environment as simulated here, with the live state it holds.
func (m *MOSES) SetRunning(id string, state map[string]any) {
	m.mux.Lock()
	defer m.mux.Unlock()
	m.running[id] = true
	m.state[id] = state
}

// PutRaw stores a document verbatim, for the cases a client cannot produce: a
// document from a newer MOSES carrying a field ODE does not know.
func (m *MOSES) PutRaw(id string, document map[string]any) {
	m.mux.Lock()
	defer m.mux.Unlock()
	document["id"] = id
	if _, known := m.environments[id]; !known {
		m.order = append(m.order, id)
	}
	m.environments[id] = document
}

// Stored is the document as this fake holds it, which is what a client's write
// actually produced.
func (m *MOSES) Stored(id string) (map[string]any, bool) {
	m.mux.Lock()
	defer m.mux.Unlock()
	document, found := m.environments[id]
	return document, found
}

// DatasetContent is what was uploaded under an id.
func (m *MOSES) DatasetContent(id string) ([]byte, bool) {
	m.mux.Lock()
	defer m.mux.Unlock()
	content, found := m.datasetContent[id]
	return content, found
}

// Count is how often a route was called.
func (m *MOSES) Count(method, path string) int {
	m.mux.Lock()
	defer m.mux.Unlock()
	return m.Requests[method+" "+path]
}

func (m *MOSES) handle(writer http.ResponseWriter, request *http.Request) {
	m.mux.Lock()
	m.Tokens = append(m.Tokens, request.Header.Get("Authorization"))
	key := request.Method + " " + request.URL.Path
	m.Requests[key]++
	if failure, planned := m.Fail[key]; planned {
		delete(m.Fail, key)
		m.mux.Unlock()
		writer.WriteHeader(failure.Code)
		_, _ = writer.Write([]byte(failure.Body))
		return
	}
	m.mux.Unlock()

	path := strings.Trim(request.URL.Path, "/")
	segments := strings.Split(path, "/")
	switch {
	case segments[0] == "device-types" && len(segments) == 1:
		m.writeJSON(writer, http.StatusOK, m.catalogue())
	case segments[0] == "datasets":
		m.handleDatasets(writer, request, segments)
	case segments[0] == "environments":
		m.handleEnvironments(writer, request, segments)
	default:
		http.Error(writer, "no such route in the fake: "+path, http.StatusNotFound)
	}
}

func (m *MOSES) catalogue() []any {
	m.mux.Lock()
	defer m.mux.Unlock()
	if m.deviceTypes == nil {
		return []any{}
	}
	return m.deviceTypes
}

func (m *MOSES) handleEnvironments(writer http.ResponseWriter, request *http.Request, segments []string) {
	switch {
	case len(segments) == 1 && request.Method == http.MethodGet:
		m.writeJSON(writer, http.StatusOK, m.list())
	case len(segments) == 1 && request.Method == http.MethodPost:
		m.create(writer, request)
	case len(segments) == 2 && request.Method == http.MethodGet:
		m.read(writer, segments[1])
	case len(segments) == 2 && request.Method == http.MethodPut:
		m.replace(writer, request, segments[1])
	case len(segments) == 2 && request.Method == http.MethodDelete:
		m.remove(writer, segments[1])
	case len(segments) == 3 && segments[2] == "state" && request.Method == http.MethodGet:
		m.readState(writer, segments[1])
	case len(segments) == 3 && segments[2] == "state" && request.Method == http.MethodPatch:
		m.patchState(writer, request, segments[1])
	case len(segments) == 3 && segments[2] == "backfill" && request.Method == http.MethodPost:
		m.startBackfill(writer, request, segments[1])
	case len(segments) == 3 && segments[2] == "backfill" && request.Method == http.MethodGet:
		m.readBackfill(writer, segments[1])
	default:
		http.Error(writer, "no such route in the fake", http.StatusNotFound)
	}
}

func (m *MOSES) list() []map[string]any {
	m.mux.Lock()
	defer m.mux.Unlock()
	out := []map[string]any{}
	for _, id := range m.order {
		if document, found := m.environments[id]; found {
			out = append(out, document)
		}
	}
	return out
}

func (m *MOSES) read(writer http.ResponseWriter, id string) {
	m.mux.Lock()
	document, found := m.environments[id]
	m.mux.Unlock()
	if !found {
		http.Error(writer, "no such environment", http.StatusNotFound)
		return
	}
	m.writeJSON(writer, http.StatusOK, document)
}

func (m *MOSES) create(writer http.ResponseWriter, request *http.Request) {
	document, ok := m.decodeBody(writer, request)
	if !ok {
		return
	}
	m.mux.Lock()
	defer m.mux.Unlock()

	m.noteServerOwned(document, "create")
	m.nextID++
	id := fmt.Sprintf("env-%d", m.nextID)
	document["id"] = id
	document["version"] = float64(1)
	// MOSES decides both on every write, whatever arrived.
	document["external_graph_ref"] = "graph-" + id
	m.assignIDs(document)
	m.provision(document, nil)

	m.environments[id] = document
	m.order = append(m.order, id)
	m.writeJSON(writer, http.StatusCreated, document)
}

func (m *MOSES) replace(writer http.ResponseWriter, request *http.Request, id string) {
	document, ok := m.decodeBody(writer, request)
	if !ok {
		return
	}
	m.mux.Lock()
	defer m.mux.Unlock()

	stored, found := m.environments[id]
	if !found {
		http.Error(writer, "no such environment", http.StatusNotFound)
		return
	}
	storedVersion := numberOf(stored["version"])
	carried := numberOf(document["version"])
	// Zero means the client does not take part, which MOSES allows. ODE always
	// carries one, and a test that finds this branch taken has found ODE dropping
	// the version somewhere.
	if carried != 0 && carried != storedVersion {
		writer.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintf(writer,
			"version conflict on %s: expected version %d, stored version is %d",
			id, int64(carried), int64(storedVersion))
		return
	}

	m.noteServerOwned(document, "replace")
	document["id"] = id
	document["version"] = storedVersion + 1
	document["external_graph_ref"] = stored["external_graph_ref"]
	m.assignIDs(document)
	m.provision(document, stored)

	m.environments[id] = document
	m.writeJSON(writer, http.StatusOK, document)
}

func (m *MOSES) remove(writer http.ResponseWriter, id string) {
	m.mux.Lock()
	defer m.mux.Unlock()
	delete(m.environments, id)
	delete(m.running, id)
	delete(m.state, id)
	delete(m.backfills, id)
	writer.WriteHeader(http.StatusNoContent)
}

func (m *MOSES) readState(writer http.ResponseWriter, id string) {
	m.mux.Lock()
	_, found := m.environments[id]
	running := m.running[id]
	state := m.state[id]
	m.mux.Unlock()
	if !found {
		http.Error(writer, "no such environment", http.StatusNotFound)
		return
	}
	answer := map[string]any{
		"running": running,
		"as_of":   time.Now().UTC().Format(time.RFC3339),
		"context": map[string]any{},
		"zones":   map[string]any{},
		"assets":  map[string]any{},
	}
	for key, value := range state {
		answer[key] = value
	}
	m.writeJSON(writer, http.StatusOK, answer)
}

func (m *MOSES) patchState(writer http.ResponseWriter, request *http.Request, id string) {
	change, ok := m.decodeBody(writer, request)
	if !ok {
		return
	}
	m.mux.Lock()
	defer m.mux.Unlock()
	if _, found := m.environments[id]; !found {
		http.Error(writer, "no such environment", http.StatusNotFound)
		return
	}
	if !m.running[id] {
		http.Error(writer, "the environment is not running here", http.StatusNotFound)
		return
	}
	if m.state[id] == nil {
		m.state[id] = map[string]any{}
	}
	for key, value := range change {
		m.state[id][key] = value
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (m *MOSES) startBackfill(writer http.ResponseWriter, request *http.Request, id string) {
	window, ok := m.decodeBody(writer, request)
	if !ok {
		return
	}
	m.mux.Lock()
	defer m.mux.Unlock()
	if _, found := m.environments[id]; !found {
		http.Error(writer, "no such environment", http.StatusNotFound)
		return
	}
	if !m.running[id] {
		http.Error(writer, "the environment is not running here", http.StatusNotFound)
		return
	}
	if existing, running := m.backfills[id]; running && existing["state"] == "running" {
		http.Error(writer, "a backfill of this environment is already running", http.StatusConflict)
		return
	}
	status := map[string]any{
		"environment_id": id,
		"state":          "running",
		"from":           window["from"],
		"to":             window["to"],
		"started_at":     time.Now().UTC().Format(time.RFC3339),
		"channels_total": float64(0),
		"channels_done":  float64(0),
		"published":      float64(0),
		"channels":       []any{},
	}
	m.backfills[id] = status
	m.writeJSON(writer, http.StatusAccepted, status)
}

// FinishBackfill moves a job to a finished state with the channel report a test
// wants to see read back.
func (m *MOSES) FinishBackfill(id string, status map[string]any) {
	m.mux.Lock()
	defer m.mux.Unlock()
	status["environment_id"] = id
	m.backfills[id] = status
}

func (m *MOSES) readBackfill(writer http.ResponseWriter, id string) {
	m.mux.Lock()
	status, found := m.backfills[id]
	m.mux.Unlock()
	if !found {
		http.Error(writer, "nothing is known about a backfill of this environment", http.StatusNotFound)
		return
	}
	m.writeJSON(writer, http.StatusOK, status)
}

func (m *MOSES) handleDatasets(writer http.ResponseWriter, request *http.Request, segments []string) {
	switch {
	case len(segments) == 1 && request.Method == http.MethodGet:
		m.mux.Lock()
		out := []map[string]any{}
		for _, dataset := range m.datasets {
			out = append(out, dataset)
		}
		m.mux.Unlock()
		m.writeJSON(writer, http.StatusOK, out)
	case len(segments) == 1 && request.Method == http.MethodPost:
		m.uploadDataset(writer, request)
	case len(segments) == 2 && request.Method == http.MethodGet:
		m.mux.Lock()
		dataset, found := m.datasets[segments[1]]
		m.mux.Unlock()
		if !found {
			http.Error(writer, "no such dataset", http.StatusNotFound)
			return
		}
		m.writeJSON(writer, http.StatusOK, dataset)
	case len(segments) == 2 && request.Method == http.MethodDelete:
		m.mux.Lock()
		delete(m.datasets, segments[1])
		delete(m.datasetContent, segments[1])
		m.mux.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	default:
		http.Error(writer, "no such route in the fake", http.StatusNotFound)
	}
}

// uploadDataset parses just enough of a CSV to answer the way MOSES does: the
// column names and the span each covers.
//
// Enough, and no more. A test that needs the dialect detection tested is testing
// MOSES rather than ODE, and a fake that reimplemented it would be asserting
// against its own reimplementation. What is real here is the refusal: a body that
// is not a header line plus rows comes back as a 400 with a line number, which is
// the answer ODE has to relay.
func (m *MOSES) uploadDataset(writer http.ResponseWriter, request *http.Request) {
	body := make([]byte, 0)
	buffer := make([]byte, 4096)
	for {
		n, err := request.Body.Read(buffer)
		body = append(body, buffer[:n]...)
		if err != nil {
			break
		}
	}
	name := request.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		http.Error(writer, "the dataset needs a name", http.StatusBadRequest)
		return
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) < 2 {
		http.Error(writer, "line 1: a dataset needs a header line and at least one row",
			http.StatusBadRequest)
		return
	}
	header := strings.Split(strings.TrimSpace(lines[0]), ",")
	if len(header) < 2 {
		http.Error(writer, "line 1: a dataset needs a time column and at least one value column",
			http.StatusBadRequest)
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()
	m.nextID++
	id := fmt.Sprintf("dataset-%d", m.nextID)
	columns := []any{}
	for _, column := range header[1:] {
		columns = append(columns, map[string]any{
			"name":      strings.TrimSpace(column),
			"points":    float64(len(lines) - 1),
			"from_unix": float64(0),
			"to_unix":   float64((len(lines) - 2) * 3600),
		})
	}
	timezone := request.URL.Query().Get("tz")
	if timezone == "" {
		timezone = "Europe/Berlin"
	}
	dataset := map[string]any{
		"id":           id,
		"name":         name,
		"timezone":     timezone,
		"columns":      columns,
		"size_bytes":   float64(len(body)),
		"created_unix": float64(time.Now().Unix()),
	}
	m.datasets[id] = dataset
	m.datasetContent[id] = body
	m.writeJSON(writer, http.StatusCreated, dataset)
}

// noteServerOwned records an attempt to set what MOSES reconciles.
func (m *MOSES) noteServerOwned(document map[string]any, at string) {
	if ref, set := document["external_graph_ref"].(string); set && ref != "" {
		m.EchoedServerOwned = append(m.EchoedServerOwned, at+": external_graph_ref="+ref)
	}
	walkAssets(document, func(asset map[string]any) {
		if managed, set := asset["external_managed"].(bool); set && managed {
			m.EchoedServerOwned = append(m.EchoedServerOwned,
				at+": external_managed on "+stringOf(asset["name"]))
		}
	})
}

// assignIDs fills in an id wherever the document left one empty, as MOSES does.
func (m *MOSES) assignIDs(document map[string]any) {
	if stringOf(document["id"]) == "" {
		m.nextID++
		document["id"] = fmt.Sprintf("env-%d", m.nextID)
	}
	walkAssets(document, func(asset map[string]any) {
		if stringOf(asset["id"]) == "" {
			m.nextID++
			asset["id"] = fmt.Sprintf("asset-%d", m.nextID)
		}
		for _, channel := range channelsOf(asset) {
			if stringOf(channel["id"]) == "" {
				m.nextID++
				channel["id"] = fmt.Sprintf("channel-%d", m.nextID)
			}
		}
	})
}

// provision creates a device id for every asset that names a type and carries no
// reference, and decides external_managed from the stored document rather than
// from what arrived. Both are what MOSES does, and both are what a client gets
// wrong by echoing.
func (m *MOSES) provision(document map[string]any, stored map[string]any) {
	previous := map[string]map[string]any{}
	if stored != nil {
		walkAssets(stored, func(asset map[string]any) {
			previous[stringOf(asset["id"])] = asset
		})
	}
	walkAssets(document, func(asset map[string]any) {
		if stringOf(asset["external_ref"]) == "" && stringOf(asset["external_type_id"]) != "" {
			m.nextID++
			asset["external_ref"] = fmt.Sprintf("device-%d", m.nextID)
			asset["external_managed"] = true
			return
		}
		before, known := previous[stringOf(asset["id"])]
		asset["external_managed"] = known &&
			stringOf(asset["external_ref"]) != "" &&
			stringOf(before["external_ref"]) == stringOf(asset["external_ref"]) &&
			boolOf(before["external_managed"])
	})
}

// Devices is every platform device this fake provisioned for an environment,
// which is what a test asserts against when it wants to know that an edit created
// no second device for an asset that already had one.
func (m *MOSES) Devices(id string) []string {
	m.mux.Lock()
	defer m.mux.Unlock()
	out := []string{}
	document, found := m.environments[id]
	if !found {
		return out
	}
	walkAssets(document, func(asset map[string]any) {
		if ref := stringOf(asset["external_ref"]); ref != "" {
			out = append(out, ref)
		}
	})
	return out
}

func (m *MOSES) decodeBody(writer http.ResponseWriter, request *http.Request) (map[string]any, bool) {
	document := map[string]any{}
	if err := json.NewDecoder(request.Body).Decode(&document); err != nil {
		http.Error(writer, "unable to read the request body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return document, true
}

func (m *MOSES) writeJSON(writer http.ResponseWriter, code int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(body)
}

// walkAssets visits every asset of a document, in nested zones as well.
func walkAssets(document map[string]any, visit func(asset map[string]any)) {
	var walkZones func(zones any)
	walkZones = func(zones any) {
		list, ok := zones.([]any)
		if !ok {
			return
		}
		for _, entry := range list {
			zone, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			walkZones(zone["zones"])
			assets, ok := zone["assets"].([]any)
			if !ok {
				continue
			}
			for _, assetEntry := range assets {
				if asset, ok := assetEntry.(map[string]any); ok {
					visit(asset)
				}
			}
		}
	}
	walkZones(document["zones"])
}

func channelsOf(asset map[string]any) []map[string]any {
	out := []map[string]any{}
	list, ok := asset["channels"].([]any)
	if !ok {
		return out
	}
	for _, entry := range list {
		if channel, ok := entry.(map[string]any); ok {
			out = append(out, channel)
		}
	}
	return out
}

func stringOf(value any) string {
	text, _ := value.(string)
	return text
}

func boolOf(value any) bool {
	flag, _ := value.(bool)
	return flag
}

func numberOf(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int64:
		return float64(typed)
	case string:
		parsed, _ := strconv.ParseFloat(typed, 64)
		return parsed
	}
	return 0
}
