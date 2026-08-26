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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Ray Job Submission API, spoken directly (SPEC §5.12).
//
// Plain JSON over HTTP against the dashboard's `/api/jobs` and `/api/packages`
// routes, for the reason pkg/timeseries gives for not using timescale-wrapper's
// own client: the alternative is a Python SDK this service cannot call, and the
// surface ODE needs is five endpoints.
//
// The package flow is the part worth understanding. Ray's runtime environment
// takes a `working_dir` that may be a `gcs://` URI naming a zip already in the
// cluster's object store, and the name of that zip is by convention
// `_ray_pkg_<hash>.zip`. So ODE hashes the archive it built, asks whether the
// cluster already has that name, and uploads only when it does not — which makes
// a second launch from the same commit skip the upload entirely, because
// `git archive` of one commit is byte-identical every time.
//
// The credential is a service account (§3.1 item 5). That is the one place in ODE
// where a service account is legitimate: a Ray cluster has no per-user identity to
// act as, and D18 rules out building one.

// rayClient is the Ray dashboard, as this package needs it.
type rayClient struct {
	baseURL string
	token   string
	http    *http.Client
	timeout time.Duration
	// upload bounds the one request that moves the whole archive, separately from
	// timeout, for the reason pkg/timeseries splits its two: a bound that fits a
	// status read cannot fit a multi-megabyte upload, and one figure for both means
	// either the probe hangs or the upload is cut off mid-body.
	upload time.Duration
}

// jobSubmission is the body of POST /api/jobs/.
type jobSubmission struct {
	Entrypoint   string            `json:"entrypoint"`
	SubmissionID string            `json:"submission_id"`
	RuntimeEnv   jobRuntimeEnv     `json:"runtime_env"`
	Metadata     map[string]string `json:"metadata"`
}

// jobRuntimeEnv is the subset of Ray's runtime environment ODE sets. Everything
// else — pip, conda, resources — belongs to the repository's own code, which is
// the point of shipping the repository rather than a command line.
type jobRuntimeEnv struct {
	WorkingDir string            `json:"working_dir"`
	EnvVars    map[string]string `json:"env_vars,omitempty"`
}

// jobDetails is GET /api/jobs/{submission_id}.
//
// StartTime and EndTime are epoch milliseconds in Ray's own encoding, which is
// why they are int64 here and converted at the edge rather than being given a
// time.Time tag that would not parse.
type jobDetails struct {
	SubmissionID string            `json:"submission_id"`
	Status       string            `json:"status"`
	Message      string            `json:"message"`
	ErrorType    string            `json:"error_type"`
	StartTime    int64             `json:"start_time"`
	EndTime      int64             `json:"end_time"`
	Metadata     map[string]string `json:"metadata"`
}

// packageName is the object-store name for an archive's contents.
//
// The hash is of the zip bytes, so the name *is* the content: two launches whose
// archives differ in one byte get two names, and two launches from the same commit
// get one. Ray's own convention for the prefix is followed rather than invented,
// because a name outside it would still work but would be unrecognisable to
// anyone reading the cluster's package store.
func packageName(archive []byte) string {
	sum := sha256.Sum256(archive)
	return "_ray_pkg_" + hex.EncodeToString(sum[:]) + ".zip"
}

// packageURI is what runtime_env.working_dir is set to.
func packageURI(name string) string { return "gcs://" + name }

// packageExists asks whether the cluster already holds this package.
//
// Ray answers an existing package with an empty 200 and a missing one with 404,
// so this is cheap — but the body is discarded through a bounded reader anyway,
// because a future Ray that answered with the package itself would otherwise pull
// the whole archive back across the network to learn one bit.
func (r *rayClient) packageExists(ctx context.Context, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	request, err := r.newRequest(ctx, http.MethodGet, "/api/packages/gcs/"+name, "", nil)
	if err != nil {
		return false, err
	}
	response, err := r.http.Do(request)
	if err != nil {
		return false, &UpstreamError{Service: "ray", Resource: "packages", Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode == http.StatusNotFound:
		return false, nil
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return true, nil
	default:
		return false, r.upstream("packages", response)
	}
}

// uploadPackage puts the archive in the cluster's object store.
func (r *rayClient) uploadPackage(ctx context.Context, name string, archive []byte) error {
	// The upload deadline is its own, and generous: this is the one request in the
	// package that moves megabytes, and bounding it by the same figure that bounds a
	// status read would fail every launch of a repository with any weight to it.
	ctx, cancel := context.WithTimeout(ctx, r.uploadTimeout())
	defer cancel()

	request, err := r.newRequest(ctx, http.MethodPut, "/api/packages/gcs/"+name,
		"application/zip", bytes.NewReader(archive))
	if err != nil {
		return err
	}
	request.ContentLength = int64(len(archive))
	response, err := r.http.Do(request)
	if err != nil {
		return &UpstreamError{Service: "ray", Resource: "packages", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Redacted, like every request that carried a body: this one's body is the
		// developer's own source tree.
		return r.upstreamRedacted("packages", response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}

// submit posts the job.
func (r *rayClient) submit(ctx context.Context, job jobSubmission) (string, error) {
	var answer struct {
		SubmissionID string `json:"submission_id"`
		JobID        string `json:"job_id"`
	}
	// redactAnswer, because this is the body that carries the job's credential.
	if err := r.call(ctx, http.MethodPost, "/api/jobs/", "jobs", job, &answer,
		redactAnswer); err != nil {
		return "", err
	}
	if answer.SubmissionID == "" {
		// Ray echoes the submission id it accepted. A cluster that answered without
		// one has accepted something ODE cannot then address, and reporting the id
		// ODE asked for would hide that.
		return "", &UpstreamError{
			Service: "ray", Resource: "jobs", Code: http.StatusOK,
			Message: "the cluster accepted the job but named no submission id",
		}
	}
	return answer.SubmissionID, nil
}

// details reads one job's state.
func (r *rayClient) details(ctx context.Context, submissionID string) (jobDetails, error) {
	var out jobDetails
	err := r.call(ctx, http.MethodGet, "/api/jobs/"+submissionID, "jobs", nil, &out,
		repeatAnswer)
	return out, err
}

// stop asks Ray to stop a job. Ray answers a job that has already finished with
// a 200 and stopped:false rather than an error, and that is passed through: "it
// was already done" is an answer to the request, not a failure of it.
func (r *rayClient) stop(ctx context.Context, submissionID string) (bool, error) {
	var answer struct {
		Stopped bool `json:"stopped"`
	}
	// repeatAnswer: the body is an empty object, so there is nothing in it to leak
	// and Ray's own words are the only diagnosis a failed stop has.
	err := r.call(ctx, http.MethodPost, "/api/jobs/"+submissionID+"/stop", "jobs",
		struct{}{}, &answer, repeatAnswer)
	return answer.Stopped, err
}

// logs reads a job's driver output.
//
// Never part of a tool result (§5.13). It is here for the developer's own route,
// and the cap is applied by the caller rather than here so that the bound is a
// configuration rather than a constant buried in a client.
func (r *rayClient) logs(ctx context.Context, submissionID string) (string, error) {
	var answer struct {
		Logs string `json:"logs"`
	}
	err := r.call(ctx, http.MethodGet, "/api/jobs/"+submissionID+"/logs", "jobs", nil,
		&answer, repeatAnswer)
	return answer.Logs, err
}

// call issues one request and decodes the answer.
// Whether a refusal may repeat what the cluster said.
//
// Named constants at the call site rather than a rule inferred from "the request
// had a body", because that rule had a false positive with a real cost: stopping a
// job sends an empty object and carries nothing to leak, and redacting it threw
// away Ray's own reason for refusing while claiming the answer had been withheld
// to protect a credential that was never in it. A wrong explanation is worse than
// a missing one.
const (
	repeatAnswer = false
	redactAnswer = true
)

func (r *rayClient) call(
	ctx context.Context, method, path, resource string, payload, out any, redact bool,
) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
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

	request, err := r.newRequest(ctx, method, path, contentType, body)
	if err != nil {
		return err
	}
	response, err := r.http.Do(request)
	if err != nil {
		return &UpstreamError{Service: "ray", Resource: resource, Err: err}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if redact {
			return r.upstreamRedacted(resource, response)
		}
		return r.upstream(resource, response)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return &UpstreamError{
			Service: "ray", Resource: resource, Code: response.StatusCode,
			Message: "the answer was not readable", Err: err,
		}
	}
	return nil
}

// newRequest builds one request. The deadline is the caller's: every caller wraps
// ctx with r.timeout and defers the cancel until after the body is read, because
// cancelling here would close the context while the response was still being
// decoded.
func (r *rayClient) newRequest(
	ctx context.Context, method, path, contentType string, body io.Reader,
) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	// The service account of §3.1 item 5. Ray has no per-user identity to act as,
	// and D18 rules out building one.
	if r.token != "" {
		request.Header.Set("Authorization", "Bearer "+r.token)
	}
	return request, nil
}

// uploadTimeout is the package upload's own bound.
func (r *rayClient) uploadTimeout() time.Duration {
	if r.upload > 0 {
		return r.upload
	}
	return 5 * time.Minute
}

// upstreamRedacted turns a non-2xx into this package's error **without repeating
// the response body**, and it is what every request that sent a body uses.
//
// The reason is that Ray's dashboard is a FastAPI application and FastAPI's
// validation handler renders the rejected request back into its own 422, under
// `input`. The body ODE sends to `/api/jobs/` carries `runtime_env.env_vars`, and
// that map holds SENERGY_TOKEN — the credential minted for the job on the
// developer's behalf (§3.1 item 6). Repeating that body put the credential into
// `ode_experiments.message`, into the 502 the API answers with, and into every log
// line that formatted the error. A token in a database column and in an HTTP
// response is not a smaller problem than a token in a log.
//
// Two requests use it and they are the two that send something worth protecting:
// the submission, whose body holds the credential, and the package upload, whose
// body is the developer's whole source tree. Everything else repeats the cluster's
// words, because for those requests the words are the only diagnosis there is.
//
// The diagnosis is not lost for the submission either: a job Ray accepted and then
// failed to set up reports its reason on the job record, which is read with no
// request body at all.
func (r *rayClient) upstreamRedacted(resource string, response *http.Response) error {
	// Drained rather than left, so the connection is reusable; discarded rather than
	// read into a string, so there is no copy of it to leak by accident later.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	return &UpstreamError{
		Service: "ray", Resource: resource, Code: response.StatusCode,
		Message: "the cluster refused it; the answer is not repeated here because the " +
			"request carried the job's environment, which holds its platform credential",
	}
}

// upstream turns a non-2xx into this package's error, with the body's first line
// as the message.
//
// Only the first line, and capped: Ray answers some failures with a whole Python
// traceback, and an error that carries one is unreadable wherever it lands.
func (r *rayClient) upstream(resource string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	message := strings.TrimSpace(string(body))
	if index := strings.IndexByte(message, '\n'); index > 0 {
		message = message[:index]
	}
	if len(message) > 400 {
		message = message[:400] + "…"
	}
	return &UpstreamError{
		Service: "ray", Resource: resource, Code: response.StatusCode, Message: message,
	}
}

// rayTime converts Ray's epoch milliseconds. Zero means "not yet", which is a
// nil time rather than 1970.
func rayTime(millis int64) *time.Time {
	if millis <= 0 {
		return nil
	}
	converted := time.UnixMilli(millis).UTC()
	return &converted
}
