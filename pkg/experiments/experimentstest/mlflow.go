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

package experimentstest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// MLflow is the tracking server's REST API, in memory.
//
// It answers a missing experiment the way MLflow does — 404 with
// RESOURCE_DOES_NOT_EXIST rather than an empty result — because that path is what
// ensureExperiment is built around, and a double that answered 200 with nothing
// would leave the create branch untested.
type MLflow struct {
	server *httptest.Server

	mux         sync.Mutex
	experiments map[string]*MLflowExperiment // by name
	byID        map[string]*MLflowExperiment
	runs        map[string]*MLflowRun
	nextID      int
	// Created records experiment names in creation order, so a test can assert D17's
	// namespacing produced one experiment per user and repository rather than one
	// per launch.
	Created []string
}

// MLflowExperiment is one experiment.
type MLflowExperiment struct {
	ID   string
	Name string
	Tags map[string]string
	// Runs is the run ids in creation order.
	Runs []string
}

// MLflowRun is one run, with everything a summary reads.
type MLflowRun struct {
	ID           string
	ExperimentID string
	Name         string
	Status       string
	StartTime    int64
	EndTime      int64
	Tags         map[string]string
	Params       map[string]string
	// Metrics is every logged point, so a test can check that the summary reduces a
	// history to the latest value per key rather than taking the first it sees.
	Metrics []MLflowMetric
}

// MLflowMetric is one logged point.
type MLflowMetric struct {
	Key       string
	Value     float64
	Step      int64
	Timestamp int64
}

// NewMLflow starts the double.
func NewMLflow(t testing.TB) *MLflow {
	t.Helper()
	fake := &MLflow{
		experiments: map[string]*MLflowExperiment{},
		byID:        map[string]*MLflowExperiment{},
		runs:        map[string]*MLflowRun{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.route))
	t.Cleanup(fake.server.Close)
	return fake
}

func (m *MLflow) URL() string { return m.server.URL }

// Run is one run as the double holds it.
func (m *MLflow) Run(t testing.TB, runID string) MLflowRun {
	t.Helper()
	m.mux.Lock()
	defer m.mux.Unlock()
	run, found := m.runs[runID]
	if !found {
		t.Fatalf("no run %q; the double holds %v", runID, m.runIDs())
	}
	return *run
}

// Experiments is every experiment name, sorted.
func (m *MLflow) Experiments() []string {
	m.mux.Lock()
	defer m.mux.Unlock()
	names := make([]string, 0, len(m.experiments))
	for name := range m.experiments {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *MLflow) runIDs() []string {
	ids := make([]string, 0, len(m.runs))
	for id := range m.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Finish is what a job's own code would do at the end of a run: log metrics and
// close it. Used by the tests that build a summary.
func (m *MLflow) Finish(t testing.TB, runID, status string, metrics map[string]float64) {
	t.Helper()
	m.mux.Lock()
	defer m.mux.Unlock()

	run, found := m.runs[runID]
	if !found {
		t.Fatalf("no run %q", runID)
	}
	run.Status = status
	run.EndTime = run.StartTime + 90_000
	for key, value := range metrics {
		run.Metrics = append(run.Metrics, MLflowMetric{
			Key: key, Value: value, Timestamp: run.EndTime,
		})
	}
}

// LogMetric appends one point, so a history with several steps can be built.
func (m *MLflow) LogMetric(t testing.TB, runID, key string, value float64, step int64) {
	t.Helper()
	m.mux.Lock()
	defer m.mux.Unlock()
	run, found := m.runs[runID]
	if !found {
		t.Fatalf("no run %q", runID)
	}
	run.Metrics = append(run.Metrics, MLflowMetric{
		Key: key, Value: value, Step: step, Timestamp: run.StartTime + step*1000,
	})
}

// SetParam records a parameter, the way a job's own code would.
func (m *MLflow) SetParam(t testing.TB, runID, key, value string) {
	t.Helper()
	m.mux.Lock()
	defer m.mux.Unlock()
	run, found := m.runs[runID]
	if !found {
		t.Fatalf("no run %q", runID)
	}
	run.Params[key] = value
}

// SetTag records a tag, the way a job's own code would.
func (m *MLflow) SetTag(t testing.TB, runID, key, value string) {
	t.Helper()
	m.mux.Lock()
	defer m.mux.Unlock()
	run, found := m.runs[runID]
	if !found {
		t.Fatalf("no run %q", runID)
	}
	run.Tags[key] = value
}

func (m *MLflow) route(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/api/2.0/mlflow/experiments/get-by-name":
		m.handleGetByName(writer, request)
	case "/api/2.0/mlflow/experiments/create":
		m.handleCreateExperiment(writer, request)
	case "/api/2.0/mlflow/runs/create":
		m.handleCreateRun(writer, request)
	case "/api/2.0/mlflow/runs/get":
		m.handleGetRun(writer, request)
	case "/api/2.0/mlflow/runs/search":
		m.handleSearchRuns(writer, request)
	case "/api/2.0/mlflow/runs/set-tag":
		m.handleSetTag(writer, request)
	case "/api/2.0/mlflow/runs/update":
		m.handleUpdateRun(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

// notFound is MLflow's own shape for a missing resource.
func notFound(writer http.ResponseWriter, message string) {
	writeJSON(writer, http.StatusNotFound, map[string]string{
		"error_code": "RESOURCE_DOES_NOT_EXIST",
		"message":    message,
	})
}

func (m *MLflow) handleGetByName(writer http.ResponseWriter, request *http.Request) {
	m.mux.Lock()
	defer m.mux.Unlock()

	name := request.URL.Query().Get("experiment_name")
	experiment, found := m.experiments[name]
	if !found {
		notFound(writer, "No Experiment with name="+name+" exists")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"experiment": map[string]any{
			"experiment_id": experiment.ID,
			"name":          experiment.Name,
		},
	})
}

func (m *MLflow) handleCreateExperiment(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name string `json:"name"`
		Tags []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"tags"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	if _, exists := m.experiments[body.Name]; exists {
		// MLflow's own answer, and a 400 rather than a 409 on the versions this was
		// written against.
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error_code": "RESOURCE_ALREADY_EXISTS",
			"message":    "Experiment '" + body.Name + "' already exists",
		})
		return
	}

	m.nextID++
	experiment := &MLflowExperiment{
		ID:   strconv.Itoa(m.nextID),
		Name: body.Name,
		Tags: map[string]string{},
	}
	for _, tag := range body.Tags {
		experiment.Tags[tag.Key] = tag.Value
	}
	m.experiments[body.Name] = experiment
	m.byID[experiment.ID] = experiment
	m.Created = append(m.Created, body.Name)

	writeJSON(writer, http.StatusOK, map[string]string{"experiment_id": experiment.ID})
}

func (m *MLflow) handleCreateRun(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ExperimentID string `json:"experiment_id"`
		RunName      string `json:"run_name"`
		StartTime    int64  `json:"start_time"`
		Tags         []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"tags"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	experiment, found := m.byID[body.ExperimentID]
	if !found {
		notFound(writer, "No Experiment with id="+body.ExperimentID+" exists")
		return
	}

	start := body.StartTime
	if start == 0 {
		start = time.Now().UnixMilli()
	}
	run := &MLflowRun{
		ID:           "run-" + strconv.Itoa(len(m.runs)+1),
		ExperimentID: experiment.ID,
		Name:         body.RunName,
		Status:       "RUNNING",
		StartTime:    start,
		Tags:         map[string]string{},
		Params:       map[string]string{},
	}
	for _, tag := range body.Tags {
		run.Tags[tag.Key] = tag.Value
	}
	m.runs[run.ID] = run
	experiment.Runs = append(experiment.Runs, run.ID)

	writeJSON(writer, http.StatusOK, map[string]any{"run": m.marshalRun(run)})
}

func (m *MLflow) handleGetRun(writer http.ResponseWriter, request *http.Request) {
	m.mux.Lock()
	defer m.mux.Unlock()

	run, found := m.runs[request.URL.Query().Get("run_id")]
	if !found {
		notFound(writer, "Run does not exist")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"run": m.marshalRun(run)})
}

func (m *MLflow) handleSearchRuns(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ExperimentIDs []string `json:"experiment_ids"`
		MaxResults    int      `json:"max_results"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	matched := make([]*MLflowRun, 0, len(m.runs))
	for _, id := range body.ExperimentIDs {
		experiment, found := m.byID[id]
		if !found {
			continue
		}
		for _, runID := range experiment.Runs {
			matched = append(matched, m.runs[runID])
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].StartTime > matched[j].StartTime
	})
	if body.MaxResults > 0 && len(matched) > body.MaxResults {
		matched = matched[:body.MaxResults]
	}

	out := make([]any, 0, len(matched))
	for _, run := range matched {
		out = append(out, m.marshalRun(run))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"runs": out})
}

func (m *MLflow) handleSetTag(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		RunID string `json:"run_id"`
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	run, found := m.runs[body.RunID]
	if !found {
		notFound(writer, "Run does not exist")
		return
	}
	run.Tags[body.Key] = body.Value
	writeJSON(writer, http.StatusOK, map[string]any{})
}

// handleUpdateRun closes a run, which is what ODE does to a run it opened and
// could not submit a job for.
func (m *MLflow) handleUpdateRun(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		RunID   string `json:"run_id"`
		Status  string `json:"status"`
		EndTime int64  `json:"end_time"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	run, found := m.runs[body.RunID]
	if !found {
		notFound(writer, "Run does not exist")
		return
	}
	if body.Status != "" {
		run.Status = body.Status
	}
	if body.EndTime != 0 {
		run.EndTime = body.EndTime
	}
	writeJSON(writer, http.StatusOK, map[string]any{"run_info": m.marshalRun(run)["info"]})
}

// marshalRun renders MLflow's Run shape. Called with the lock held.
func (m *MLflow) marshalRun(run *MLflowRun) map[string]any {
	tags := make([]map[string]string, 0, len(run.Tags))
	for _, key := range sortedKeys(run.Tags) {
		tags = append(tags, map[string]string{"key": key, "value": run.Tags[key]})
	}
	params := make([]map[string]string, 0, len(run.Params))
	for _, key := range sortedKeys(run.Params) {
		params = append(params, map[string]string{"key": key, "value": run.Params[key]})
	}
	metrics := make([]map[string]any, 0, len(run.Metrics))
	for _, metric := range run.Metrics {
		metrics = append(metrics, map[string]any{
			"key":       metric.Key,
			"value":     metric.Value,
			"step":      metric.Step,
			"timestamp": metric.Timestamp,
		})
	}
	return map[string]any{
		"info": map[string]any{
			"run_id":          run.ID,
			"run_uuid":        run.ID,
			"experiment_id":   run.ExperimentID,
			"status":          run.Status,
			"start_time":      run.StartTime,
			"end_time":        run.EndTime,
			"lifecycle_stage": "active",
		},
		"data": map[string]any{
			"metrics": metrics,
			"params":  params,
			"tags":    tags,
		},
	}
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
