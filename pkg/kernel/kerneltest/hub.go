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

// Package kerneltest is a JupyterHub and one singleuser server, in memory.
//
// It lives outside the test files because two packages need it: pkg/kernel tests
// the client against it, and pkg/api tests the routes and the WebSocket relay
// that sit on top. Duplicating a few hundred lines of protocol double in both
// would guarantee the two drift.
package kerneltest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Hub is a JupyterHub and one singleuser server, enough of both to run the
// whole M4 path without a cluster.
//
// It speaks the real protocol rather than a simplification of it — a spawn that
// is pending before it is ready, a scoped token, an execute that produces busy /
// input / stream / reply / idle in that order — because the parts of pkg/kernel
// worth testing are exactly the ones that depend on that ordering. Where it
// diverges it says so.
type Hub struct {
	server *httptest.Server

	mux sync.Mutex
	// SpawnsBeforeReady is how many status reads after a spawn request still
	// report it pending, so the poll loop is exercised rather than skipped.
	SpawnsBeforeReady int
	pollsSinceSpawn   int
	spawned           bool
	Ready             bool

	// Scopes and Kind are what GET /hub/api/user reports. Set them, like Ready and
	// SpawnsBeforeReady, before the first request.
	Scopes []string
	Kind   string

	// Recorded for assertions.
	StartedServers []string
	SpawnProfiles  []string
	MintedTokens   []MintedRequest
	Activity       []string
	CreatedKernels []CreatedKernel
	Directories    []string
	Interrupts     []string
	DeletedKernels []string
	Executed       []string
	ServiceTokens  []string

	// The knobs below change while the hub is serving, so they are set through
	// methods rather than fields: a test goroutine writing a field a handler
	// goroutine reads is a data race, whichever way the test is written.
	nextKernelID    string
	kernelSeq       int
	deadKernels     map[string]bool
	issuedKernels   []string
	stopped         bool
	keepStopped     bool
	failNextExecute bool
	streamChunks    int
	hangExecute     chan struct{}
	responder       func(code string) (stdout string, stderr string, handled bool)
}

type MintedRequest struct {
	User      string
	ExpiresIn int
	Scopes    []string
}

type CreatedKernel struct {
	User string
	Name string
	Path string
}

func NewHub(t testing.TB) *Hub {
	t.Helper()
	hub := &Hub{
		Ready:  true,
		Kind:   "service",
		Scopes: []string{"servers", "tokens", "access:servers", "users:activity", "read:users"},

		streamChunks: 1,
		deadKernels:  map[string]bool{},
	}
	hub.server = httptest.NewServer(http.HandlerFunc(hub.route))
	t.Cleanup(hub.server.Close)
	return hub
}

func (f *Hub) URL() string { return f.server.URL }

// ArmFailure makes the next cell raise a ValueError.
func (f *Hub) ArmFailure() {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.failNextExecute = true
}

// Hang holds every execute_reply back until the channel is closed, which is how
// a long-running cell is simulated. Pass nil to stop hanging.
func (f *Hub) Hang(release chan struct{}) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.hangExecute = release
}

// SetStreamChunks splits the canned answer across n stream messages instead of
// one.
//
// It is how a test makes a consumer's buffers fill: a cell nobody is reading
// blocks its forwarding goroutine once the channels between the socket and the
// caller are full, which is the state a browser on a slow connection puts an ODE
// session in and the one where "the cell is over" and "the caller has seen it"
// come apart.
func (f *Hub) SetStreamChunks(n int) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.streamChunks = n
}

// SetNextKernelID names the kernel the next create returns.
// OnExecute installs a responder for execute_request.
//
// Without one the fake answers every cell with the same canned stream and result,
// which is all the M4 tests need. The workspace operations of §5.11 need more:
// they send a request and read the answer back off stdout, so a fake that always
// prints "hello" cannot exercise them at all. A responder that reports handled
// replaces the canned output with its own.
func (f *Hub) OnExecute(responder func(code string) (stdout string, stderr string, handled bool)) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.responder = responder
}

// PythonExecutor runs the cell with the local python3, in the given HOME.
//
// It is the responder the workspace tests use, and it is deliberately not a
// second implementation of the workspace protocol in Go: the cell ODE sends is
// executed by a real interpreter against a real filesystem, so the Python in
// kernel/workspace.go is what the test covers rather than a Go paraphrase of it
// that could agree with the test while disagreeing with the pod.
//
// Skips the test when there is no python3 to run.
func PythonExecutor(t testing.TB, home string) func(string) (string, string, bool) {
	t.Helper()
	binary, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed, so the workspace helper cannot be executed")
	}
	return func(code string) (string, string, bool) {
		command := exec.Command(binary, "-c", code)
		// HOME is what the helper resolves the workspace against, so pointing it at
		// a temporary directory is what makes the test's filesystem the pod's.
		command.Env = append(os.Environ(), "HOME="+home)
		command.Dir = home
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		_ = command.Run()
		return stdout.String(), stderr.String(), true
	}
}

func (f *Hub) SetNextKernelID(id string) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.nextKernelID = id
}

// KillKernel makes a kernel answer 404, which is how a culled and respawned pod
// looks to a client that remembers the old one.
func (f *Hub) KillKernel(id string) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.deadKernels[id] = true
}

// StopServer models the pod going away underneath ODE: culled by the idle
// culler, or shut down from the JupyterHub UI while ODE still holds the session.
//
// The Hub keeps answering, and that is the point. With no pod behind
// /user/{name}/, the proxy's fallback route is the Hub itself, which serves a
// rendered page rather than an API answer — 403 to anything that changes state,
// because the Hub applies XSRF protection to its own handlers and an API client
// sends no _xsrf. Divergence: the real Hub redirects a GET to /hub/user/{name}/
// first, and this answers it where it arrives.
//
// The next spawn clears it, which is what a respawn looks like from here.
func (f *Hub) StopServer() {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.stopped = true
	f.Ready = false
	f.spawned = false
	f.pollsSinceSpawn = 0
	// The kernels lived in that pod, so nothing that was in it answers any more.
	for _, id := range f.issuedKernels {
		f.deadKernels[id] = true
	}
}

// StopServerAndStayStopped is StopServer, plus a spawn that reports the server
// ready again while /user/{name}/ still has nothing behind it.
//
// That combination is real, if normally brief: the Hub marks a server ready
// before the proxy route to it is in place. Here it is permanent, which is what
// makes the error ODE reports observable instead of something a respawn hides.
func (f *Hub) StopServerAndStayStopped() {
	f.StopServer()
	f.mux.Lock()
	defer f.mux.Unlock()
	f.keepStopped = true
}

func (f *Hub) serverStopped() bool {
	f.mux.Lock()
	defer f.mux.Unlock()
	return f.stopped
}

// Calls is what the hub recorded, copied out under its lock.
type Calls struct {
	StartedServers []string
	// SpawnProfiles is the KubeSpawner profile each spawn asked for, empty for
	// the deployment default.
	SpawnProfiles  []string
	MintedTokens   []MintedRequest
	Activity       []string
	CreatedKernels []CreatedKernel
	Directories    []string
	Interrupts     []string
	DeletedKernels []string
	Executed       []string
	ServiceTokens  []string
}

func (f *Hub) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/hub/api/user":
		f.recordServiceToken(r)
		f.writeJSON(w, map[string]any{
			"name": "ode", "kind": f.Kind, "scopes": f.Scopes, "roles": []string{"ode"},
		})

	case strings.HasSuffix(r.URL.Path, "/server"):
		f.recordServiceToken(r)
		f.handleServer(w, r)

	case strings.HasSuffix(r.URL.Path, "/tokens"):
		f.recordServiceToken(r)
		f.handleTokens(w, r)

	case strings.HasSuffix(r.URL.Path, "/activity"):
		f.recordServiceToken(r)
		f.handleActivity(w, r)

	case strings.HasPrefix(r.URL.Path, "/hub/api/users/"):
		f.recordServiceToken(r)
		f.handleUser(w, r)

	case strings.HasPrefix(r.URL.Path, "/user/") && f.serverStopped():
		f.serveHubPage(w, r)

	case strings.Contains(r.URL.Path, "/api/kernels"):
		f.handleKernels(w, r)

	case strings.Contains(r.URL.Path, "/api/contents"):
		f.handleContents(w, r)

	default:
		http.NotFound(w, r)
	}
}

// recordServiceToken captures the credential the Hub calls arrived with, so a
// test can assert ODE never sends a developer's token to the Hub API.
func (f *Hub) recordServiceToken(r *http.Request) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.ServiceTokens = append(f.ServiceTokens, r.Header.Get("Authorization"))
}

func (f *Hub) handleUser(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/hub/api/users/")

	f.mux.Lock()
	pending := ""
	ready := f.Ready
	if !ready && f.spawned {
		f.pollsSinceSpawn++
		if f.pollsSinceSpawn > f.SpawnsBeforeReady {
			f.Ready, ready = true, true
		} else {
			pending = "spawn"
		}
	}
	f.mux.Unlock()

	server := map[string]any{"ready": ready, "url": "/user/" + name + "/"}
	if pending != "" {
		server["pending"] = pending
	}
	f.writeJSON(w, map[string]any{
		"name": name, "servers": map[string]any{"": server},
	})
}

func (f *Hub) handleServer(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/hub/api/users/"), "/server")
	var options struct {
		Profile string `json:"profile"`
	}
	_ = json.NewDecoder(r.Body).Decode(&options)

	f.mux.Lock()
	f.SpawnProfiles = append(f.SpawnProfiles, options.Profile)
	f.StartedServers = append(f.StartedServers, name)
	f.spawned = true
	if !f.keepStopped {
		f.stopped = false
	}
	f.mux.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (f *Hub) handleTokens(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/hub/api/users/"), "/tokens")
	var body struct {
		ExpiresIn int      `json:"expires_in"`
		Scopes    []string `json:"scopes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mux.Lock()
	f.MintedTokens = append(f.MintedTokens, MintedRequest{
		User: name, ExpiresIn: body.ExpiresIn, Scopes: body.Scopes,
	})
	count := len(f.MintedTokens)
	f.mux.Unlock()

	expiry := time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	f.writeJSON(w, map[string]any{
		"token":      fmt.Sprintf("user-token-%d", count),
		"id":         fmt.Sprintf("t%d", count),
		"expires_at": expiry.UTC().Format(time.RFC3339),
		"scopes":     body.Scopes,
	})
}

func (f *Hub) handleActivity(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/hub/api/users/"), "/activity")
	f.mux.Lock()
	f.Activity = append(f.Activity, name)
	f.mux.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *Hub) handleContents(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/user/")
	_, path, _ = strings.Cut(path, "/api/contents")
	path = strings.Trim(path, "/")

	if r.Method == http.MethodPut {
		f.mux.Lock()
		f.Directories = append(f.Directories, path)
		f.mux.Unlock()
		f.writeJSON(w, map[string]any{"name": path, "path": path, "type": "directory"})
		return
	}
	f.writeJSON(w, map[string]any{
		"name": path, "path": path, "type": "directory",
		"content": []map[string]any{
			{"name": "marker.txt", "path": path + "/marker.txt", "type": "file", "size": 12},
		},
	})
}

func (f *Hub) handleKernels(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimPrefix(r.URL.Path, "/user/")
	user, rest, _ := strings.Cut(user, "/api/kernels")
	rest = strings.Trim(rest, "/")

	switch {
	case rest == "" && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mux.Lock()
		f.CreatedKernels = append(f.CreatedKernels, CreatedKernel{User: user, Name: body.Name, Path: body.Path})
		// Numbered, because a singleuser server really does run several kernels at
		// once — one per workbench — and a double that gave them all one id could not
		// represent the thing it exists to stand in for. SetNextKernelID overrides
		// the next one and is consumed by it, which is how a test names the kernel a
		// restart lands on.
		id := f.nextKernelID
		if id == "" {
			f.kernelSeq++
			id = fmt.Sprintf("kernel-%d", f.kernelSeq)
		} else {
			f.nextKernelID = ""
		}
		f.issuedKernels = append(f.issuedKernels, id)
		// A kernel that is handed out now is alive, whatever became of the one that
		// carried this id before a StopServer.
		delete(f.deadKernels, id)
		f.mux.Unlock()
		f.writeJSON(w, map[string]any{"id": id, "name": body.Name, "execution_state": "idle"})

	case strings.HasSuffix(rest, "/channels"):
		f.serveChannels(w, r, strings.TrimSuffix(rest, "/channels"))

	case strings.HasSuffix(rest, "/interrupt"):
		f.mux.Lock()
		f.Interrupts = append(f.Interrupts, strings.TrimSuffix(rest, "/interrupt"))
		f.mux.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodDelete:
		f.mux.Lock()
		f.DeletedKernels = append(f.DeletedKernels, rest)
		f.mux.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet:
		f.mux.Lock()
		dead := f.deadKernels[rest]
		f.mux.Unlock()
		if dead {
			http.Error(w, `{"message":"Kernel does not exist"}`, http.StatusNotFound)
			return
		}
		f.writeJSON(w, map[string]any{"id": rest, "name": "python3", "execution_state": "idle"})

	default:
		http.NotFound(w, r)
	}
}

// serveHubPage is the Hub answering for a route that has no pod behind it.
func (f *Hub) serveHubPage(w http.ResponseWriter, r *http.Request) {
	status := http.StatusServiceUnavailable
	if r.Method != http.MethodGet {
		status = http.StatusForbidden
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, "<!DOCTYPE HTML>\n<html lang=\"en\">\n<head>\n<title>JupyterHub</title>\n"+
		"<link rel=\"stylesheet\" href=\"/hub/static/css/style.min.css\" type=\"text/css\"/>\n</head>\n"+
		"<body>\n<div class=\"ajax-error\">'_xsrf' argument missing from POST</div>\n</body>\n</html>\n")
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// serveChannels is the kernel WebSocket: a message loop that answers
// kernel_info_request and execute_request in the order a real kernel does.
func (f *Hub) serveChannels(w http.ResponseWriter, r *http.Request, kernelID string) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "token ") {
		http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		var incoming map[string]any
		if err := conn.ReadJSON(&incoming); err != nil {
			return
		}
		header, _ := incoming["header"].(map[string]any)
		msgType, _ := header["msg_type"].(string)
		msgID, _ := header["msg_id"].(string)
		content, _ := incoming["content"].(map[string]any)

		switch msgType {
		case "kernel_info_request":
			f.reply(conn, msgID, "kernel_info_reply", "shell", map[string]any{"status": "ok"})

		case "execute_request":
			code, _ := content["code"].(string)
			silent, _ := content["silent"].(bool)
			f.mux.Lock()
			f.Executed = append(f.Executed, code)
			failing := f.failNextExecute
			f.failNextExecute = false
			hang := f.hangExecute
			responder := f.responder
			chunks := f.streamChunks
			f.mux.Unlock()

			f.reply(conn, msgID, "status", "iopub", map[string]any{"execution_state": "busy"})
			if !silent {
				f.reply(conn, msgID, "execute_input", "iopub",
					map[string]any{"code": code, "execution_count": 1})
			}

			if hang != nil {
				<-hang
			}

			if failing {
				f.reply(conn, msgID, "error", "iopub", map[string]any{
					"ename": "ValueError", "evalue": "deliberate",
					"traceback": []string{"\x1b[31mValueError\x1b[0m: deliberate"},
				})
				f.reply(conn, msgID, "execute_reply", "shell",
					map[string]any{"status": "error", "ename": "ValueError"})
				f.reply(conn, msgID, "status", "iopub", map[string]any{"execution_state": "idle"})
				continue
			}

			if handled := f.respond(conn, msgID, code, responder); !handled && !silent {
				for range max(chunks, 1) {
					f.reply(conn, msgID, "stream", "iopub",
						map[string]any{"name": "stdout", "text": "hello\n"})
				}
				f.reply(conn, msgID, "execute_result", "iopub", map[string]any{
					"execution_count": 1,
					"data": map[string]any{
						"text/plain": "42",
						"image/png":  base64.StdEncoding.EncodeToString([]byte("not-really-a-png")),
					},
				})
			}
			f.reply(conn, msgID, "execute_reply", "shell",
				map[string]any{"status": "ok", "execution_count": 1})
			f.reply(conn, msgID, "status", "iopub", map[string]any{"execution_state": "idle"})
		}
	}
}

// respond streams a responder's answer, in the two messages a real kernel would
// use. Reports whether it handled the cell at all.
func (f *Hub) respond(
	conn *websocket.Conn, msgID, code string,
	responder func(string) (string, string, bool),
) bool {
	if responder == nil {
		return false
	}
	stdout, stderr, handled := responder(code)
	if !handled {
		return false
	}
	if stdout != "" {
		f.reply(conn, msgID, "stream", "iopub", map[string]any{"name": "stdout", "text": stdout})
	}
	if stderr != "" {
		f.reply(conn, msgID, "stream", "iopub", map[string]any{"name": "stderr", "text": stderr})
	}
	return true
}

func (f *Hub) reply(conn *websocket.Conn, parentID, msgType, channel string, content map[string]any) {
	_ = conn.WriteJSON(map[string]any{
		"header":        map[string]any{"msg_id": "reply-" + parentID, "msg_type": msgType},
		"parent_header": map[string]any{"msg_id": parentID},
		"metadata":      map[string]any{},
		"content":       content,
		"channel":       channel,
	})
}

func (f *Hub) writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// Calls copies everything the hub recorded, under its lock.
func (f *Hub) Calls() Calls {
	f.mux.Lock()
	defer f.mux.Unlock()
	return Calls{
		StartedServers: append([]string{}, f.StartedServers...),
		SpawnProfiles:  append([]string{}, f.SpawnProfiles...),
		MintedTokens:   append([]MintedRequest{}, f.MintedTokens...),
		Activity:       append([]string{}, f.Activity...),
		CreatedKernels: append([]CreatedKernel{}, f.CreatedKernels...),
		Directories:    append([]string{}, f.Directories...),
		Interrupts:     append([]string{}, f.Interrupts...),
		DeletedKernels: append([]string{}, f.DeletedKernels...),
		Executed:       append([]string{}, f.Executed...),
		ServiceTokens:  append([]string{}, f.ServiceTokens...),
	}
}
