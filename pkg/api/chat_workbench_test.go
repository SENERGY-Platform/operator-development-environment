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
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// Moving a conversation to another workbench (PUT /chat/sessions/{id}/workbench).
//
// The route is the one place two surfaces meet: the workbench is looked up in
// pkg/repo and the session is written in pkg/chat, so what is tested here is the
// pairing — that a workbench which is not this developer's is refused before the
// session is touched, and that the route is not served at all where there are no
// workbenches to move between.

// workbenchService is a repository surface with nothing but its store working.
//
// Opening, listing and reading a workbench are store-only operations — nothing is
// cloned and no pod is touched (see repo.CreateWorkbench) — so the kernel this is
// built with points at an address nothing listens on. A test that made it reach for
// the pod would fail loudly rather than pass on a fake.
func workbenchService(t *testing.T) *repo.Service {
	t.Helper()

	workspace, err := kernel.New(kernel.Options{
		BaseURL:        "http://127.0.0.1:1",
		Token:          "service-token",
		WorkspacePath:  "data/ode",
		RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("kernel.New: %v", err)
	}
	t.Cleanup(workspace.Close)

	sealer, err := repo.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	service, err := repo.New(repo.Deps{
		Workspace: workspace,
		Store:     repo.NewMemoryStore(),
		Sealer:    sealer,
		Options: repo.Options{
			ClientID:     "client",
			ClientSecret: "secret",
			RedirectURI:  "http://localhost:5173/github/callback",
		},
	})
	if err != nil {
		t.Fatalf("repo.New: %v", err)
	}
	return service
}

// newMoveHarness is the chat harness with a repository surface beside it, which is
// the only configuration where the workbench route exists.
func newMoveHarness(t *testing.T) (*chatHarness, *repo.Service) {
	t.Helper()
	benches := workbenchService(t)
	return newChatHarnessWith(t, func(deps *api.Deps) { deps.Repo = benches }), benches
}

// theCaller is the subject mintToken signs for, and therefore whose workbenches the
// route reads.
const theCaller = "user-123"

func TestMovingASessionOverHTTPRePointsItAndLeavesANote(t *testing.T) {
	h, benches := newMoveHarness(t)
	ctx := context.Background()

	first, err := benches.CreateWorkbench(ctx, theCaller, "wind forecast")
	if err != nil {
		t.Fatalf("CreateWorkbench: %v", err)
	}
	second, err := benches.CreateWorkbench(ctx, theCaller, "pv yield")
	if err != nil {
		t.Fatalf("CreateWorkbench: %v", err)
	}

	created := h.do(t, http.MethodPost, "/chat/sessions",
		map[string]any{"workbench_id": first.ID}, "developer")
	if created.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", created.Code, created.Body.String())
	}
	id := decodeBody(t, created)["id"].(string)

	moved := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/workbench",
		map[string]any{"workbench_id": second.ID}, "developer")
	if moved.Code != http.StatusOK {
		t.Fatalf("move: %d %s", moved.Code, moved.Body.String())
	}
	if got := decodeBody(t, moved)["workbench_id"]; got != second.ID {
		t.Errorf("answered workbench_id = %v, want %q", got, second.ID)
	}

	// Read back through the route the SPA reads, so the note is checked where a
	// developer would actually see it.
	detail := h.do(t, http.MethodGet, "/chat/sessions/"+id, nil, "developer")
	if detail.Code != http.StatusOK {
		t.Fatalf("read session: %d %s", detail.Code, detail.Body.String())
	}
	body := decodeBody(t, detail)
	session, ok := body["session"].(map[string]any)
	if !ok {
		t.Fatalf("no session in the detail answer: %s", detail.Body.String())
	}
	if got := session["workbench_id"]; got != second.ID {
		t.Errorf("stored workbench_id = %v, want %q", got, second.ID)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %v, want the one note the move left", body["messages"])
	}
	note := messages[0].(map[string]any)
	if note["origin"] != "ode" {
		t.Errorf("the note is attributed to %v, want ode: the developer did not type it", note["origin"])
	}
	if !strings.Contains(moved.Body.String(), second.ID) {
		t.Errorf("the answer does not carry the new workbench: %s", moved.Body.String())
	}
}

func TestClearingASessionsWorkbenchOverHTTP(t *testing.T) {
	h, benches := newMoveHarness(t)
	bench, err := benches.CreateWorkbench(context.Background(), theCaller, "wind forecast")
	if err != nil {
		t.Fatalf("CreateWorkbench: %v", err)
	}

	created := h.do(t, http.MethodPost, "/chat/sessions",
		map[string]any{"workbench_id": bench.ID}, "developer")
	if created.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", created.Code, created.Body.String())
	}
	id := decodeBody(t, created)["id"].(string)

	// An empty id is a value, not a missing field: it says "wherever I am working",
	// which is the state every session written before workbenches is in.
	cleared := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/workbench",
		map[string]any{"workbench_id": ""}, "developer")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", cleared.Code, cleared.Body.String())
	}
	// Omitted from the JSON when empty, which is what the field's tag says and what
	// the SPA reads as "my only workbench".
	if got, present := decodeBody(t, cleared)["workbench_id"]; present && got != "" {
		t.Errorf("workbench_id = %v, want it cleared", got)
	}
}

func TestMovingASessionToAWorkbenchThatIsNotTheCallersIs404(t *testing.T) {
	h, benches := newMoveHarness(t)
	ctx := context.Background()

	mine, err := benches.CreateWorkbench(ctx, theCaller, "wind forecast")
	if err != nil {
		t.Fatalf("CreateWorkbench: %v", err)
	}
	// A workbench that exists and belongs to somebody else. 404 rather than 403,
	// because an id in a URL must not be enough to learn that it exists.
	theirs, err := benches.CreateWorkbench(ctx, "someone-else", "their operator")
	if err != nil {
		t.Fatalf("CreateWorkbench: %v", err)
	}

	created := h.do(t, http.MethodPost, "/chat/sessions",
		map[string]any{"workbench_id": mine.ID}, "developer")
	if created.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", created.Code, created.Body.String())
	}
	id := decodeBody(t, created)["id"].(string)

	for name, target := range map[string]string{
		"another developer's": theirs.ID,
		"no workbench at all": "bench-that-never-existed",
	} {
		recorder := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/workbench",
			map[string]any{"workbench_id": target}, "developer")
		if recorder.Code != http.StatusNotFound {
			t.Errorf("moving to %s gave %d, want 404: %s", name, recorder.Code, recorder.Body.String())
		}
	}

	// And the session stayed where it was: a refused move must not be a half-done
	// one, which is why the workbench is checked before the session is written.
	detail := decodeBody(t, h.do(t, http.MethodGet, "/chat/sessions/"+id, nil, "developer"))
	if got := detail["session"].(map[string]any)["workbench_id"]; got != mine.ID {
		t.Errorf("workbench_id = %v, want the original %q", got, mine.ID)
	}
	if messages, ok := detail["messages"].([]any); ok && len(messages) != 0 {
		t.Errorf("a refused move left %d messages in the conversation", len(messages))
	}
}

func TestMovingAnotherUsersSessionIs404(t *testing.T) {
	h, benches := newMoveHarness(t)
	ctx := context.Background()
	bench, err := benches.CreateWorkbench(ctx, theCaller, "wind forecast")
	if err != nil {
		t.Fatalf("CreateWorkbench: %v", err)
	}

	// mintToken always signs for user-123, so the session belonging to someone else
	// is made directly and the route is checked against it.
	other, err := h.engine.CreateSession(ctx, "someone-else", chat.CreateRequest{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	recorder := h.do(t, http.MethodPut, "/chat/sessions/"+other.ID+"/workbench",
		map[string]any{"workbench_id": bench.ID}, "developer")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("moving another user's session gave %d, want 404", recorder.Code)
	}
}

func TestTheWorkbenchRouteIsAbsentWithoutARepositorySurface(t *testing.T) {
	h := newChatHarness(t)
	id := h.createSession(t, "")

	// Not served rather than served and always refusing: without a repository
	// surface there are no workbenches at all, and a route that answered 404 to every
	// id there is would say the wrong thing about why.
	recorder := h.do(t, http.MethodPut, "/chat/sessions/"+id+"/workbench",
		map[string]any{"workbench_id": "bench-a"}, "developer")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("the workbench route answered %d without a repository surface, want 404",
			recorder.Code)
	}
	// The rest of the chat surface is unaffected, so the 404 is about this route.
	if got := h.do(t, http.MethodGet, "/chat/sessions/"+id, nil, "developer").Code; got != http.StatusOK {
		t.Errorf("reading the session gave %d, want 200", got)
	}
}
