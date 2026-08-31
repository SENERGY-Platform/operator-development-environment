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

// Package experimentstest is a Ray cluster and an MLflow tracking server, in
// memory.
//
// It lives outside the test files for the reason kerneltest and repotest do: two
// packages need it — pkg/experiments tests the service against it and pkg/api
// tests the routes on top — and a second copy of the same double would drift from
// the first.
//
// What is *not* faked is the half that would matter most. The job package these
// servers receive was built by a real `git archive` over a real working copy in a
// temporary directory, carried back through a real python3 in the kerneltest pod,
// so a test that asserts the package is uploaded once and reused is asserting
// something about git's output rather than about a fixture. Only the two HTTP
// surfaces are doubles, because neither Ray nor MLflow can be had in a unit test.
package experimentstest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ray is the Job Submission API: the package store and the four job endpoints.
type Ray struct {
	server *httptest.Server

	mux sync.Mutex
	// packages is the object store, keyed by the name Ray was asked to hold the
	// archive under. The bytes are kept so a test can unzip what was actually sent.
	packages map[string][]byte
	// uploads counts PUTs rather than packages, which is the difference the
	// "uploaded once, reused on the second launch" test turns on.
	uploads int
	// jobs is what has been submitted, by submission id.
	jobs map[string]*Job
	// order records submission ids in the order they arrived.
	order []string
	// status is what a job reports next. Empty leaves a submitted job RUNNING.
	status map[string]string
	logs   map[string]string
	fail   map[string]int
	// echo is FailNext's sibling: the same one-shot refusal, but answering with the
	// request body inside it, the way FastAPI's 422 handler does.
	echo    map[string]int
	stopped []string
	calls   int
	// delay is slept before every answer, so a test can model a cluster that is
	// reachable but slow — which is the condition a serial refresh loop turns into a
	// listing that never arrives.
	delay time.Duration
	// Token is what the Authorization header carried, so a test can assert the
	// service account of §3.1 item 5 reached the cluster.
	Tokens []string
}

// Job is one submitted job, as the double recorded it.
type Job struct {
	SubmissionID string            `json:"submission_id"`
	Entrypoint   string            `json:"entrypoint"`
	Metadata     map[string]string `json:"metadata"`
	RuntimeEnv   struct {
		WorkingDir string `json:"working_dir"`
		// How Ray starts worker processes, which has to match how the entrypoint
		// starts the driver. Asserted on, so the double has to carry it.
		PyExecutable string            `json:"py_executable"`
		EnvVars      map[string]string `json:"env_vars"`
	} `json:"runtime_env"`
	Status    string
	StartTime int64
	EndTime   int64
}

// NewRay starts the double.
func NewRay(t testing.TB) *Ray {
	t.Helper()
	fake := &Ray{
		packages: map[string][]byte{},
		jobs:     map[string]*Job{},
		status:   map[string]string{},
		logs:     map[string]string{},
		fail:     map[string]int{},
		echo:     map[string]int{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.route))
	t.Cleanup(fake.server.Close)
	return fake
}

func (r *Ray) URL() string { return r.server.URL }

// Calls is how many requests the cluster has answered. A test that starts and
// stops a background loop uses it to say "it was working" and "it stopped"
// without either claim resting on a sleep.
func (r *Ray) Calls() int {
	r.mux.Lock()
	defer r.mux.Unlock()
	return r.calls
}

// Uploads is how many times an archive was PUT.
func (r *Ray) Uploads() int {
	r.mux.Lock()
	defer r.mux.Unlock()
	return r.uploads
}

// Packages is the names currently held.
func (r *Ray) Packages() []string {
	r.mux.Lock()
	defer r.mux.Unlock()
	names := make([]string, 0, len(r.packages))
	for name := range r.packages {
		names = append(names, name)
	}
	return names
}

// Package is the bytes held under one name.
func (r *Ray) Package(name string) ([]byte, bool) {
	r.mux.Lock()
	defer r.mux.Unlock()
	archive, found := r.packages[name]
	return archive, found
}

// Jobs is every submission, in arrival order.
func (r *Ray) Jobs() []Job {
	r.mux.Lock()
	defer r.mux.Unlock()
	out := make([]Job, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, *r.jobs[id])
	}
	return out
}

// LastJob is the most recent submission.
func (r *Ray) LastJob(t testing.TB) Job {
	t.Helper()
	jobs := r.Jobs()
	if len(jobs) == 0 {
		t.Fatal("no job was submitted")
	}
	return jobs[len(jobs)-1]
}

// Stopped is the submission ids a stop was requested for.
func (r *Ray) Stopped() []string {
	r.mux.Lock()
	defer r.mux.Unlock()
	return append([]string(nil), r.stopped...)
}

// SetStatus is what a job reports from now on. A terminal status also stamps an
// end time, because a summary's duration comes from the pair.
func (r *Ray) SetStatus(submissionID, status string) {
	r.mux.Lock()
	defer r.mux.Unlock()
	r.status[submissionID] = status
	if job, found := r.jobs[submissionID]; found {
		job.Status = status
		switch status {
		case "SUCCEEDED", "FAILED", "STOPPED":
			if job.EndTime == 0 {
				job.EndTime = job.StartTime + 60_000
			}
		}
	}
}

// SetLogs is what the log route answers with.
func (r *Ray) SetLogs(submissionID, logs string) {
	r.mux.Lock()
	defer r.mux.Unlock()
	r.logs[submissionID] = logs
}

// Forget drops a submission, the way a Ray cluster forgets one when it restarts
// or when its retention window passes. The job's own history in MLflow survives;
// only the cluster's memory of it goes.
func (r *Ray) Forget(submissionID string) {
	r.mux.Lock()
	defer r.mux.Unlock()
	delete(r.jobs, submissionID)
	delete(r.status, submissionID)
	for index, id := range r.order {
		if id == submissionID {
			r.order = append(r.order[:index:index], r.order[index+1:]...)
			break
		}
	}
}

// Delay makes every answer take this long. A reachable but slow cluster is the
// condition that separates a bounded concurrent refresh from a serial one.
func (r *Ray) Delay(d time.Duration) {
	r.mux.Lock()
	defer r.mux.Unlock()
	r.delay = d
}

// FailNext makes the next request to a path answer with a status code, once.
// The path is matched by suffix, e.g. "/api/jobs/".
func (r *Ray) FailNext(pathSuffix string, code int) {
	r.mux.Lock()
	defer r.mux.Unlock()
	r.fail[pathSuffix] = code
}

func (r *Ray) route(writer http.ResponseWriter, request *http.Request) {
	r.mux.Lock()
	r.calls++
	if delay := r.delay; delay > 0 {
		r.mux.Unlock()
		select {
		case <-time.After(delay):
		case <-request.Context().Done():
			return
		}
		r.mux.Lock()
	}
	if token := request.Header.Get("Authorization"); token != "" {
		r.Tokens = append(r.Tokens, token)
	}
	for suffix, code := range r.fail {
		if strings.HasSuffix(request.URL.Path, suffix) {
			delete(r.fail, suffix)
			r.mux.Unlock()
			http.Error(writer, "the double was told to fail this one", code)
			return
		}
	}
	for suffix, code := range r.echo {
		if strings.HasSuffix(request.URL.Path, suffix) {
			delete(r.echo, suffix)
			r.mux.Unlock()
			body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
			var input any
			if err := json.Unmarshal(body, &input); err != nil {
				input = string(body)
			}
			// FastAPI's shape, because that is what a real Ray dashboard answers with.
			writeJSON(writer, code, map[string]any{
				"detail": []any{map[string]any{
					"loc": []string{"body", "entrypoint"}, "msg": "field required",
					"type": "value_error.missing", "input": input,
				}},
			})
			return
		}
	}
	r.mux.Unlock()

	path := request.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/packages/gcs/"):
		r.handlePackage(writer, request, strings.TrimPrefix(path, "/api/packages/gcs/"))
	case path == "/api/jobs/" && request.Method == http.MethodPost:
		r.handleSubmit(writer, request)
	case strings.HasSuffix(path, "/stop") && request.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/jobs/"), "/stop")
		r.handleStop(writer, id)
	case strings.HasSuffix(path, "/logs"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/jobs/"), "/logs")
		r.mux.Lock()
		logs := r.logs[id]
		r.mux.Unlock()
		writeJSON(writer, http.StatusOK, map[string]string{"logs": logs})
	case strings.HasPrefix(path, "/api/jobs/"):
		r.handleDetails(writer, strings.TrimPrefix(path, "/api/jobs/"))
	default:
		http.NotFound(writer, request)
	}
}

// handlePackage is Ray's own contract: an empty 200 when the package is held, a
// 404 when it is not, and a PUT that stores the body.
func (r *Ray) handlePackage(writer http.ResponseWriter, request *http.Request, name string) {
	r.mux.Lock()
	defer r.mux.Unlock()

	switch request.Method {
	case http.MethodGet:
		if _, found := r.packages[name]; !found {
			http.Error(writer, "package does not exist", http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusOK)
	case http.MethodPut:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		r.packages[name] = body
		r.uploads++
		writer.WriteHeader(http.StatusOK)
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *Ray) handleSubmit(writer http.ResponseWriter, request *http.Request) {
	var job Job
	if err := json.NewDecoder(request.Body).Decode(&job); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	r.mux.Lock()
	defer r.mux.Unlock()

	if _, exists := r.jobs[job.SubmissionID]; exists {
		// Ray refuses a duplicate submission id, which is the property that makes
		// ODE minting it an idempotency guard rather than decoration.
		http.Error(writer, "submission id already exists", http.StatusBadRequest)
		return
	}
	if _, held := r.packages[strings.TrimPrefix(job.RuntimeEnv.WorkingDir, "gcs://")]; !held {
		http.Error(writer, "the working_dir package is not in the object store",
			http.StatusBadRequest)
		return
	}

	job.Status = "PENDING"
	job.StartTime = time.Now().UnixMilli()
	r.jobs[job.SubmissionID] = &job
	r.order = append(r.order, job.SubmissionID)
	writeJSON(writer, http.StatusOK, map[string]string{"submission_id": job.SubmissionID})
}

func (r *Ray) handleDetails(writer http.ResponseWriter, submissionID string) {
	r.mux.Lock()
	defer r.mux.Unlock()

	job, found := r.jobs[submissionID]
	if !found {
		http.Error(writer, "job not found", http.StatusNotFound)
		return
	}
	status := job.Status
	if override, set := r.status[submissionID]; set {
		status = override
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"submission_id": submissionID,
		"status":        status,
		"metadata":      job.Metadata,
		"start_time":    job.StartTime,
		"end_time":      job.EndTime,
	})
}

func (r *Ray) handleStop(writer http.ResponseWriter, submissionID string) {
	r.mux.Lock()
	job, found := r.jobs[submissionID]
	if found {
		r.stopped = append(r.stopped, submissionID)
		job.Status = "STOPPED"
		job.EndTime = time.Now().UnixMilli()
		r.status[submissionID] = "STOPPED"
	}
	r.mux.Unlock()

	if !found {
		http.Error(writer, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"stopped": true})
}

func writeJSON(writer http.ResponseWriter, code int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(body)
}

// EchoNext makes the next request to a path answer with a status code *and the
// request body echoed back*, once.
//
// It exists because that is what Ray actually does. `/api/jobs/` is a FastAPI
// route, and FastAPI's own 422 handler renders the rejected request body into the
// response under `input` — so a submission Ray refuses on shape comes back
// carrying `runtime_env.env_vars`, and that map holds the job's platform
// credential. A double that answered with a fixed sentence could not tell whether
// ODE lets such a body reach an error string, a stored record or an HTTP response.
func (r *Ray) EchoNext(pathSuffix string, code int) {
	r.mux.Lock()
	defer r.mux.Unlock()
	r.echo[pathSuffix] = code
}
