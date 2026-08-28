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

package experiments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MLflow's REST API, spoken directly (§5.12, D17).
//
// Five endpoints: find or create the per-user experiment, create the run, read it
// back, and search the experiment for the previous one. All of them are
// `/api/2.0/mlflow/...` and all of them are plain JSON, so this costs no
// dependency — which matters, because the official client is Python.
//
// One decision is load-bearing and is not obvious from the endpoint list: **the
// run is created with its tags in the same request.** MLflow's `runs/create`
// accepts a tag list, and using it rather than five follow-up `set-tag` calls
// means there is no window in which a run exists without its commit_sha tag.
// That tag is M8's acceptance criterion and §5.11 item 7's whole claim; a run that
// carries it only after four more round trips is a run that a crash between them
// leaves permanently unreproducible, and nothing downstream could tell.

// mlflowClient is the tracking server, as this package needs it.
type mlflowClient struct {
	baseURL string
	token   string
	http    *http.Client
	timeout time.Duration
}

// errExperimentAbsent is get-by-name's 404, as a value. Not exported: it is a
// step in ensureExperiment rather than an answer any caller of this package sees.
var errExperimentAbsent = errors.New("mlflow: the experiment does not exist")

// mlflowTag is MLflow's key/value pair, used for run tags and experiment tags.
type mlflowTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// mlflowRun is the subset of MLflow's Run this package reads.
type mlflowRun struct {
	Info struct {
		RunID        string `json:"run_id"`
		RunUUID      string `json:"run_uuid"`
		ExperimentID string `json:"experiment_id"`
		Status       string `json:"status"`
		StartTime    int64  `json:"start_time"`
		EndTime      int64  `json:"end_time"`
		LifecycleSt  string `json:"lifecycle_stage"`
	} `json:"info"`
	Data struct {
		Metrics []struct {
			Key       string  `json:"key"`
			Value     float64 `json:"value"`
			Timestamp int64   `json:"timestamp"`
			Step      int64   `json:"step"`
		} `json:"metrics"`
		Params []mlflowTag `json:"params"`
		Tags   []mlflowTag `json:"tags"`
	} `json:"data"`
}

// runID answers with whichever field this server populated. MLflow has carried
// both `run_id` and the older `run_uuid` for years and different versions favour
// different ones; taking either is cheaper than pinning a server version.
func (r mlflowRun) runID() string {
	if r.Info.RunID != "" {
		return r.Info.RunID
	}
	return r.Info.RunUUID
}

// experimentByName finds the per-user experiment (D17).
func (m *mlflowClient) experimentByName(ctx context.Context, name string) (string, error) {
	var answer struct {
		Experiment struct {
			ExperimentID string `json:"experiment_id"`
			Name         string `json:"name"`
		} `json:"experiment"`
	}
	query := url.Values{"experiment_name": []string{name}}
	err := m.call(ctx, http.MethodGet, "/api/2.0/mlflow/experiments/get-by-name", query,
		"experiments/get-by-name", nil, &answer)
	if err != nil {
		var upstream *UpstreamError
		if errors.As(err, &upstream) && upstream.Code == http.StatusNotFound {
			return "", errExperimentAbsent
		}
		return "", err
	}
	return answer.Experiment.ExperimentID, nil
}

// createExperiment creates it, tagged with who it belongs to.
//
// The tags are what makes the name recoverable: the name carries the developer's
// Hub username because that is what a human reads in MLflow's own UI, and the
// username can change, so the Keycloak subject — which cannot — is a tag.
func (m *mlflowClient) createExperiment(
	ctx context.Context, name string, tags []mlflowTag,
) (string, error) {
	var answer struct {
		ExperimentID string `json:"experiment_id"`
	}
	payload := map[string]any{"name": name}
	if len(tags) > 0 {
		payload["tags"] = tags
	}
	err := m.call(ctx, http.MethodPost, "/api/2.0/mlflow/experiments/create", nil,
		"experiments/create", payload, &answer)
	if err != nil {
		return "", err
	}
	if answer.ExperimentID == "" {
		return "", &UpstreamError{
			Service: "mlflow", Resource: "experiments/create", Code: http.StatusOK,
			Message: "the server created the experiment but named no id",
		}
	}
	return answer.ExperimentID, nil
}

// ensureExperiment is get-by-name, then create when it is absent.
//
// The create is retried once through get-by-name on a conflict, because two
// launches by the same developer at the same moment both see "absent" and both
// create — and the loser of that race should join the winner's experiment rather
// than fail a launch over a millisecond.
func (m *mlflowClient) ensureExperiment(
	ctx context.Context, name string, tags []mlflowTag,
) (string, error) {
	id, err := m.experimentByName(ctx, name)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, errExperimentAbsent) {
		return "", err
	}

	id, err = m.createExperiment(ctx, name, tags)
	if err == nil {
		return id, nil
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) &&
		(upstream.Code == http.StatusBadRequest || upstream.Code == http.StatusConflict) {
		// RESOURCE_ALREADY_EXISTS arrives as a 400 from most MLflow versions and a 409
		// from some, so both are read as "somebody else won the race".
		if existing, lookupErr := m.experimentByName(ctx, name); lookupErr == nil {
			return existing, nil
		}
	}
	return "", err
}

// createRun creates the run ODE owns, with its tags in the same request.
func (m *mlflowClient) createRun(
	ctx context.Context, experimentID, runName string, start time.Time, tags []mlflowTag,
) (string, error) {
	var answer struct {
		Run mlflowRun `json:"run"`
	}
	payload := map[string]any{
		"experiment_id": experimentID,
		"start_time":    start.UnixMilli(),
		"tags":          tags,
	}
	if runName != "" {
		payload["run_name"] = runName
	}
	if err := m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/create", nil,
		"runs/create", payload, &answer); err != nil {
		return "", err
	}
	runID := answer.Run.runID()
	if runID == "" {
		return "", &UpstreamError{
			Service: "mlflow", Resource: "runs/create", Code: http.StatusOK,
			Message: "the server created the run but named no id",
		}
	}
	return runID, nil
}

// setTag writes one tag onto an existing run.
//
// The launch path tags at creation and does not use this — it exists for the one
// case that cannot be tagged in advance: Ray accepting a submission under a
// different id than ODE asked for, which makes the run's ray_submission_id tag
// wrong the moment it is written. Kept to that, deliberately: a general "write
// anything onto a run" is how a tag set stops being trustworthy.
func (m *mlflowClient) setTag(ctx context.Context, runID, key, value string) error {
	return m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/set-tag", nil, "runs/set-tag",
		map[string]string{"run_id": runID, "key": key, "value": value}, nil)
}

// MLflow's own run statuses, which are not Ray's. Only the three ODE ever writes
// are named: it closes a run it opened, and it never invents a state a job did not
// reach.
const (
	mlflowFinished = "FINISHED"
	mlflowFailed   = "FAILED"
	mlflowKilled   = "KILLED"
)

// updateRun closes a run ODE opened.
//
// It exists for one thing M8 left undone and M9 cannot leave undone: a submission
// the cluster refuses happens *after* the run was created, so without this the run
// stays open and MLflow's UI shows it RUNNING forever beside an ODE record that
// says the launch failed. Deleting it would be wrong — a run that existed and
// failed to launch is a fact about the developer's day, and MLflow's own delete is
// a developer action — so it is closed with a status and an end time instead.
//
// Kept as narrow as setTag is, and for the same reason: this is the only writer of
// a run's lifecycle besides the job itself, and a general "set anything on a run"
// is how a tracking server stops being trustworthy. ODE writes a run's status when
// it opened the run and the job never got the chance to.
func (m *mlflowClient) updateRun(ctx context.Context, runID, status string, end time.Time) error {
	payload := map[string]any{"run_id": runID, "status": status}
	if !end.IsZero() {
		payload["end_time"] = end.UnixMilli()
	}
	return m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/update", nil, "runs/update",
		payload, nil)
}

// run reads one run back.
func (m *mlflowClient) run(ctx context.Context, runID string) (mlflowRun, error) {
	var answer struct {
		Run mlflowRun `json:"run"`
	}
	query := url.Values{"run_id": []string{runID}}
	err := m.call(ctx, http.MethodGet, "/api/2.0/mlflow/runs/get", query, "runs/get", nil, &answer)
	return answer.Run, err
}

// searchRuns lists an experiment's runs, newest first.
//
// Used for one thing: finding the run before this one, so §5.13's
// comparison_to_previous has something to compare against. The ordering is asked
// for explicitly rather than assumed, because MLflow's default order is by
// start_time ascending on some versions and unspecified on others.
func (m *mlflowClient) searchRuns(
	ctx context.Context, experimentID string, maxResults int,
) ([]mlflowRun, error) {
	var answer struct {
		Runs []mlflowRun `json:"runs"`
	}
	payload := map[string]any{
		"experiment_ids": []string{experimentID},
		"max_results":    maxResults,
		"order_by":       []string{"attributes.start_time DESC"},
		// Deleted runs are excluded rather than filtered afterwards: a developer who
		// deleted a run meant it not to count, and comparing against one would be
		// comparing against something they cannot see.
		"run_view_type": "ACTIVE_ONLY",
	}
	err := m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/search", nil, "runs/search",
		payload, &answer)
	return answer.Runs, err
}

// call issues one request and decodes the answer.
func (m *mlflowClient) call(
	ctx context.Context, method, path string, query url.Values, resource string, payload, out any,
) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var body io.Reader
	contentType := ""
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		body, contentType = bytes.NewReader(encoded), "application/json"
	}

	target := m.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	// The service account of §3.1 item 5, for the reason Ray's is one: a tracking
	// server has no per-user identity, and D17 makes namespacing the isolation.
	if m.token != "" {
		request.Header.Set("Authorization", "Bearer "+m.token)
	}

	response, err := m.http.Do(request)
	if err != nil {
		return &UpstreamError{Service: "mlflow", Resource: resource, Err: err}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return mlflowUpstream(resource, response)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return &UpstreamError{
			Service: "mlflow", Resource: resource, Code: response.StatusCode,
			Message: "the answer was not readable", Err: err,
		}
	}
	return nil
}

// mlflowUpstream turns a non-2xx into this package's error.
//
// MLflow's failures carry a JSON body with `error_code` and `message`, and the
// error_code is the useful half: RESOURCE_DOES_NOT_EXIST arriving as a 404 and as
// a 200-with-error-body are the same condition on different versions.
func mlflowUpstream(resource string, response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	var structured struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}
	message := strings.TrimSpace(string(raw))
	if err := json.Unmarshal(raw, &structured); err == nil && structured.ErrorCode != "" {
		message = structured.ErrorCode
		if structured.Message != "" {
			message += ": " + structured.Message
		}
	}
	if len(message) > 400 {
		message = message[:400] + "…"
	}
	code := response.StatusCode
	if structured.ErrorCode == "RESOURCE_DOES_NOT_EXIST" {
		// Normalised, so ensureExperiment has one condition to recognise rather than
		// one per MLflow version.
		code = http.StatusNotFound
	}
	return &UpstreamError{
		Service: "mlflow", Resource: resource, Code: code, Message: message,
	}
}

// mlflowTime converts MLflow's epoch milliseconds, with zero meaning "not yet".
func mlflowTime(millis int64) *time.Time {
	if millis <= 0 {
		return nil
	}
	converted := time.UnixMilli(millis).UTC()
	return &converted
}
