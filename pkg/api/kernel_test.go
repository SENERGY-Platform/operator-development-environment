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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
)

type kernelHarness struct {
	server *httptest.Server
	hub    *kerneltest.Hub
}

func newKernelHarness(t *testing.T) *kernelHarness {
	t.Helper()
	hub := kerneltest.NewHub(t)

	service, err := kernel.New(kernel.Options{
		BaseURL:        hub.URL(),
		Token:          "service-token",
		WorkspacePath:  "data/ode",
		SpawnTimeout:   5 * time.Second,
		RequestTimeout: 5 * time.Second,
		ExecuteTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("kernel.New: %v", err)
	}
	t.Cleanup(service.Close)

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer"},
		api.Deps{Kernel: service},
	)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &kernelHarness{server: server, hub: hub}
}

func (h *kernelHarness) do(t *testing.T, method, path string, roles ...string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if roles != nil {
		request.Header.Set("Authorization", "Bearer "+mintToken(roles))
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (h *kernelHarness) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{"ode.bearer.token." + mintToken([]string{"developer"})},
	}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestTheKernelRoutesAreBehindTheDeveloperRole(t *testing.T) {
	h := newKernelHarness(t)

	if response := h.do(t, http.MethodGet, "/kernel"); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a token = %d, want 401", response.StatusCode)
	}
	if response := h.do(t, http.MethodGet, "/kernel", "analyst"); response.StatusCode != http.StatusForbidden {
		t.Errorf("without the developer role = %d, want 403", response.StatusCode)
	}
}

func TestTheKernelRoutesAreNotServedWithoutAJupyterhubUrl(t *testing.T) {
	// The M0 harness has no kernel service, which is a deployment with no
	// jupyterhub_url. The route must be absent rather than panicking on nil.
	h := newHarness(t)

	if response := h.get(t, "/kernel", "developer"); response.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no execution backend is configured", response.Code)
	}
}

func TestEnsureBringsThePodUpAndReportsTheWorkspace(t *testing.T) {
	h := newKernelHarness(t)

	response := h.do(t, http.MethodPost, "/kernel", "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var status kernel.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !status.ServerReady || status.KernelID == "" {
		t.Errorf("status = %+v, want a ready server and a kernel", status)
	}
	if status.Workspace != "data/ode" {
		t.Errorf("workspace = %q, want the configured persistent path", status.Workspace)
	}

	// The pod belongs to the caller and to nobody they can name: the route takes
	// no user, so the only name that reaches the Hub is the one on the token.
	if created := h.hub.Calls().CreatedKernels; len(created) != 1 || created[0].User != "dev" {
		t.Errorf("created kernels = %+v, want one for the caller", created)
	}
}

func TestFilesListsTheWorkspaceOverHttp(t *testing.T) {
	h := newKernelHarness(t)

	response := h.do(t, http.MethodGet, "/kernel/files", "developer")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var body struct {
		Workspace string             `json:"workspace"`
		Entries   []kernel.FileEntry `json:"entries"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Workspace != "data/ode" || len(body.Entries) != 1 {
		t.Errorf("body = %+v, want the workspace and its entries", body)
	}
}

func TestInterruptingWithNoKernelIs404RatherThanAFailure(t *testing.T) {
	h := newKernelHarness(t)

	response := h.do(t, http.MethodPost, "/kernel/interrupt", "developer")
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when nothing is running", response.StatusCode)
	}
}

// anyOf matches a frame of one of the given types, for readUntil.
func anyOf(types ...string) func(wsFrame) bool {
	return func(frame wsFrame) bool {
		for _, want := range types {
			if frame.Type == want {
				return true
			}
		}
		return false
	}
}

func TestExecutingOverTheWebsocketStreamsOutputAndEnds(t *testing.T) {
	h := newKernelHarness(t)
	conn := h.dial(t)

	if err := conn.WriteJSON(map[string]any{
		"type": "kernel_execute", "id": "cell-1",
		"payload": map[string]any{"code": "print('hello')"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	frames, _ := readUntil(t, conn, anyOf("done", "error"))

	var stdout string
	var finished string
	for _, frame := range frames {
		if frame.Type == "error" {
			t.Fatalf("the execution failed: %s", frame.Error)
		}
		if frame.Type != "event" {
			continue
		}
		var event kernel.ExecutionEvent
		if err := json.Unmarshal(frame.Payload, &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		switch event.Kind {
		case kernel.KindStream:
			stdout += event.Text
		case kernel.KindDone:
			finished = event.Status
		}
	}
	if stdout != "hello\n" {
		t.Errorf("stdout = %q, want the streamed output", stdout)
	}
	if finished != kernel.StatusOK {
		t.Errorf("done status = %q, want ok", finished)
	}
}

func TestCancellingAnExecutionInterruptsTheCell(t *testing.T) {
	h := newKernelHarness(t)
	conn := h.dial(t)

	// The first cell brings the session up with nothing hanging, so the hang below
	// lands on the developer's code rather than on the hidden environment push.
	if err := conn.WriteJSON(map[string]any{
		"type": "kernel_execute", "id": "warm",
		"payload": map[string]any{"code": "pass"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, conn, anyOf("done", "error"))

	release := make(chan struct{})
	defer close(release)
	h.hub.Hang(release)

	if err := conn.WriteJSON(map[string]any{
		"type": "kernel_execute", "id": "cell-2",
		"payload": map[string]any{"code": "while True: pass"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, conn, anyOf("accepted"))

	if err := conn.WriteJSON(map[string]any{"type": "cancel", "id": "cell-2"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The developer is still told how their cell ended. Going quiet instead would
	// read like a lost connection rather than a stopped cell.
	frames, _ := readUntil(t, conn, anyOf("done", "error"))
	var finished string
	for _, frame := range frames {
		if frame.Type != "event" {
			continue
		}
		var event kernel.ExecutionEvent
		if err := json.Unmarshal(frame.Payload, &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if event.Kind == kernel.KindDone {
			finished = event.Status
		}
	}
	if finished != kernel.StatusInterrupted {
		t.Errorf("final status = %q, want %q", finished, kernel.StatusInterrupted)
	}

	// And the cell is stopped in the pod, not merely stopped being watched: one
	// left running would hold the kernel against the next.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.hub.Calls().Interrupts) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("no interrupt reached the kernel after the execution was cancelled")
}

func TestARefreshedTokenIsInstalledInTheRunningKernel(t *testing.T) {
	h := newKernelHarness(t)
	conn := h.dial(t)

	if err := conn.WriteJSON(map[string]any{
		"type": "kernel_execute", "id": "warm",
		"payload": map[string]any{"code": "pass"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, conn, anyOf("done", "error"))

	before := len(h.hub.Calls().Executed)

	// A different token for the same subject, which is what a refresh looks like.
	refreshed := mintToken([]string{"developer", "analyst"})
	if err := conn.WriteJSON(map[string]any{
		"type": "auth", "id": "refresh", "payload": map[string]any{"token": refreshed},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, conn, anyOf("result", "error"))

	// §5.6 item 4: spawn-time environment cannot be refreshed, so a live kernel is
	// told by executing into it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.hub.Calls().Executed) > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the refreshed token was never pushed into the kernel, so the pod keeps the old one")
}

// TestWriteKernelContractFixtures emits the M4 documents the frontend's contract
// check assigns to its declared types.
//
// Separate from the M3 emitter in chat_test.go because it needs a different
// harness: these routes exist only when an execution backend is configured, and
// that harness has none. Same rule as the rest — emitted from the real handlers,
// so the field sets are the backend's own marshalling; the values are a fake's.
//
//	ODE_WRITE_CONTRACT=frontend/src/__contract__ go test ./pkg/api/ -run ContractFixtures
func TestWriteKernelContractFixtures(t *testing.T) {
	dir := os.Getenv("ODE_WRITE_CONTRACT")
	if dir == "" {
		t.Skip("set ODE_WRITE_CONTRACT to the fixture directory to regenerate")
	}
	h := newKernelHarness(t)

	cases := []struct {
		file   string
		method string
		path   string
	}{
		{"kernel_status.json", http.MethodPost, "/kernel"},
		{"kernel_files.json", http.MethodGet, "/kernel/files"},
	}
	for _, tc := range cases {
		response := h.do(t, tc.method, tc.path, "developer")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", tc.path, response.StatusCode)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		var parsed any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		encoded, err := json.MarshalIndent(parsed, "", "  ")
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if err := os.WriteFile(filepath.Join(dir, tc.file), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		t.Logf("wrote %s", tc.file)
	}
}
