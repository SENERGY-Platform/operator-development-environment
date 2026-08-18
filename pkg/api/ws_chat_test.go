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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// The chat surface over the WebSocket, and the property the move was made for: the
// turn is detached, so dropping the connection does not stop it.

type wsChatHarness struct {
	server  *httptest.Server
	engine  *chat.Engine
	admin   *admin.Service
	release func()
}

// newWSChatHarness wires a real server whose one tool blocks until released, so a
// test can hold an exchange open and act on the connection meanwhile.
func newWSChatHarness(t *testing.T) *wsChatHarness {
	t.Helper()

	gate := make(chan struct{})
	var released bool

	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	registry, err := tools.NewRegistry(tools.NewDefinition(tools.Definition{
		Name: "slow_tool", Description: "d", MinTier: tools.L0, Schema: schema,
	}, func(ctx context.Context, req tools.Request) (any, error) {
		req.Progress("working", "holding until the test releases it")
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Second):
		}
		return map[string]any{"ok": true}, nil
	}))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	adminService, err := admin.New(admin.NewMemoryStore(), llm.NewPricing("EUR"))
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	dispatcher, err := tools.NewDispatcher(registry, adminService, &stubIDs{})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	providers, err := llm.NewRegistry(&slowToolProvider{})
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	engine, err := chat.New(context.Background(), providers, dispatcher,
		chat.NewMemoryStore(), adminService, &stubIDs{}, chat.Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer"},
		api.Deps{Chat: engine, Admin: adminService},
	)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &wsChatHarness{
		server: server, engine: engine, admin: adminService,
		release: func() {
			if !released {
				released = true
				close(gate)
			}
		},
	}
}

func (h *wsChatHarness) session(t *testing.T) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/chat/sessions",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+mintToken([]string{"developer"}))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer response.Body.Close()

	var session struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return session.ID
}

// dial opens a client connection, carrying the token as the subprotocol the way a
// browser must.
func (h *wsChatHarness) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws"
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{"ode.bearer.token." + mintToken([]string{"developer"})},
	}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// writeFrame builds on send() from ws_test.go; wsFrame is declared there too.
func writeFrame(t *testing.T, conn *websocket.Conn, kind, id string, payload any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	send(t, conn, map[string]any{
		"type": kind, "id": id, "payload": json.RawMessage(encoded),
	})
}

// readUntil collects frames until one matches, or the deadline passes.
func readUntil(t *testing.T, conn *websocket.Conn, match func(wsFrame) bool) ([]wsFrame, wsFrame) {
	t.Helper()
	seen := []wsFrame{}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var frame wsFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read (after %d frames): %v", len(seen), err)
		}
		seen = append(seen, frame)
		if match(frame) {
			return seen, frame
		}
	}
}

func eventType(t *testing.T, frame wsFrame) string {
	t.Helper()
	if frame.Type != "event" {
		return ""
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame.Payload, &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return event.Type
}

// --- the exchange over the socket ---

func TestChatExchangeStreamsOverWebSocket(t *testing.T) {
	h := newWSChatHarness(t)
	sessionID := h.session(t)
	conn := h.dial(t)

	writeFrame(t, conn, "chat_send", "r1", map[string]any{
		"session_id": sessionID, "message": "profile it",
	})

	// The tool blocks, so progress must arrive before the result — which is the
	// whole reason for reporting it.
	frames, _ := readUntil(t, conn, func(f wsFrame) bool {
		return eventType(t, f) == "progress"
	})
	kinds := []string{}
	for _, frame := range frames {
		if kind := eventType(t, frame); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	if len(frames) == 0 || frames[0].Type != "accepted" {
		t.Errorf("first frame = %+v, want accepted", frames[0])
	}
	if !contains(kinds, "tool_call") {
		t.Errorf("events so far = %v, want a tool_call before the progress", kinds)
	}

	h.release()

	_, done := readUntil(t, conn, func(f wsFrame) bool { return f.Type == "done" })
	if done.ID != "r1" {
		t.Errorf("done carries id %q, want r1", done.ID)
	}
}

// TestExchangeSurvivesTheConnectionDropping is the point of the change. Before it,
// a profile that outlived the connection was lost with it.
func TestExchangeSurvivesTheConnectionDropping(t *testing.T) {
	h := newWSChatHarness(t)
	sessionID := h.session(t)

	first := h.dial(t)
	writeFrame(t, first, "chat_send", "r1", map[string]any{
		"session_id": sessionID, "message": "profile it",
	})
	readUntil(t, first, func(f wsFrame) bool { return eventType(t, f) == "progress" })

	// The developer closes the tab while the tool is still running.
	_ = first.Close()

	// The exchange must still be running: a view went away, not the work.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, running := h.engine.Attach(sessionID); running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the exchange stopped when the connection closed; it is supposed to be detached")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// They come back and attach to the turn in flight.
	second := h.dial(t)
	writeFrame(t, second, "chat_attach", "r2", map[string]any{"session_id": sessionID})

	// The replay means the reattached view sees the whole turn, not just the rest.
	frames, _ := readUntil(t, second, func(f wsFrame) bool {
		return eventType(t, f) == "progress"
	})
	kinds := []string{}
	for _, frame := range frames {
		if kind := eventType(t, frame); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	if !contains(kinds, "tool_call") {
		t.Errorf("reattached view saw %v, want the earlier tool_call replayed", kinds)
	}

	h.release()
	readUntil(t, second, func(f wsFrame) bool { return f.Type == "done" })

	// And the result is persisted, so the answer is there whether or not anyone was
	// watching when it arrived.
	messages, err := h.engine.Messages(context.Background(), "user-123", sessionID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	sawResult := false
	for _, message := range messages {
		for _, content := range message.Content {
			if content.Type == llm.ContentToolResult {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Error("the tool result was not persisted, so a reconnect would not find the answer")
	}
}

// TestAttachWithNothingRunningIsNotAnError: the SPA asks on every reconnect, and
// usually there is no turn in flight.
func TestAttachWithNothingRunningIsNotAnError(t *testing.T) {
	h := newWSChatHarness(t)
	sessionID := h.session(t)
	conn := h.dial(t)

	writeFrame(t, conn, "chat_attach", "r1", map[string]any{"session_id": sessionID})

	_, done := readUntil(t, conn, func(f wsFrame) bool {
		return f.Type == "done" || f.Type == "error"
	})
	if done.Type != "done" {
		t.Fatalf("attaching with nothing running gave %+v, want done", done)
	}
	var payload struct {
		Attached bool `json:"attached"`
	}
	if err := json.Unmarshal(done.Payload, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Attached {
		t.Error("attached=true with no exchange running")
	}
}

// TestChatCancelStopsTheWork is the other half of the distinction: closing a view
// leaves the turn alone, whereas cancelling abandons it.
func TestChatCancelStopsTheWork(t *testing.T) {
	h := newWSChatHarness(t)
	sessionID := h.session(t)
	conn := h.dial(t)

	writeFrame(t, conn, "chat_send", "r1", map[string]any{
		"session_id": sessionID, "message": "profile it",
	})
	readUntil(t, conn, func(f wsFrame) bool { return eventType(t, f) == "progress" })

	writeFrame(t, conn, "chat_cancel", "r2", map[string]any{"session_id": sessionID})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, running := h.engine.Attach(sessionID); !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("chat_cancel did not stop the exchange")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- refusals over the socket ---

func TestWSChatRefusalsCarryAStatus(t *testing.T) {
	h := newWSChatHarness(t)
	sessionID := h.session(t)
	conn := h.dial(t)

	cases := []struct {
		name    string
		kind    string
		payload map[string]any
		status  int
	}{
		{"unknown session", "chat_send",
			map[string]any{"session_id": "nope", "message": "hi"}, http.StatusNotFound},
		{"empty message", "chat_send",
			map[string]any{"session_id": sessionID, "message": "   "}, http.StatusBadRequest},
		{"unknown confirmation", "chat_confirm",
			map[string]any{"session_id": sessionID, "confirmation_id": "nope", "approve": true},
			http.StatusNotFound},
		{"confirmation without a decision", "chat_confirm",
			map[string]any{"session_id": sessionID, "confirmation_id": "x"},
			http.StatusForbidden},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("e%d", i)
			writeFrame(t, conn, tc.kind, id, tc.payload)
			_, frame := readUntil(t, conn, func(f wsFrame) bool {
				return f.ID == id && (f.Type == "error" || f.Type == "accepted")
			})
			if frame.Type != "error" {
				t.Fatalf("got %+v, want an error", frame)
			}
			if frame.Status != tc.status {
				t.Errorf("status = %d, want %d (%s)", frame.Status, tc.status, frame.Error)
			}
		})
	}
}

// TestWSCapBreachCarries429 keeps §3.3's structured refusal legible on the socket:
// the SPA has to be able to tell a spend cap from any other failure.
func TestWSCapBreachCarries429(t *testing.T) {
	h := newWSChatHarness(t)
	ctx := context.Background()
	sessionID := h.session(t)

	cap := int64(1)
	if err := h.admin.SetLimits(ctx, admin.GlobalSubject, admin.Limits{
		Period: "24h", TokenCap: &cap,
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	h.admin.RecordUsage(ctx, "user-123", sessionID, llm.Usage{
		InputTokens: 100, Provider: "slow", Model: "slow-model",
	})

	conn := h.dial(t)
	writeFrame(t, conn, "chat_send", "r1", map[string]any{
		"session_id": sessionID, "message": "hello",
	})

	_, frame := readUntil(t, conn, func(f wsFrame) bool {
		return f.Type == "error" || f.Type == "accepted"
	})
	if frame.Type != "error" {
		t.Fatalf("got %+v, want an error", frame)
	}
	if frame.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429; message was %q", frame.Status, frame.Error)
	}
}

// TestOneExchangePerSession: two concurrent turns would interleave their assistant
// messages into one history and leave it unreadable by either.
func TestOneExchangePerSession(t *testing.T) {
	h := newWSChatHarness(t)
	sessionID := h.session(t)
	conn := h.dial(t)

	writeFrame(t, conn, "chat_send", "r1", map[string]any{
		"session_id": sessionID, "message": "first",
	})
	readUntil(t, conn, func(f wsFrame) bool { return eventType(t, f) == "progress" })

	writeFrame(t, conn, "chat_send", "r2", map[string]any{
		"session_id": sessionID, "message": "second",
	})
	_, frame := readUntil(t, conn, func(f wsFrame) bool {
		return f.ID == "r2" && (f.Type == "error" || f.Type == "accepted")
	})
	if frame.Type != "error" {
		t.Errorf("a second concurrent exchange was accepted: %+v", frame)
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
