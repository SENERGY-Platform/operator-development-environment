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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// --- harness ---

type wsHarness struct {
	server *httptest.Server
	*profileHarness
	// release, started and blocker belong to the blocking client: a cancellation
	// needs something in flight to cancel, so the read waits until released or
	// until its context is done.
	release chan struct{}
	started chan struct{}
	blocker *blockingTimeseries
}

// cancelled fires when the blocked read observed its context being cancelled,
// which is the only honest way to assert that the work actually stopped.
func (h *wsHarness) cancelled(t *testing.T) <-chan struct{} {
	t.Helper()
	if h.blocker == nil {
		t.Fatal("this harness has no blocking client")
	}
	return h.blocker.noticed
}

// blockingTimeseries wraps the fixture client so a read can be held open. This is
// what makes cancellation testable: the point of the WebSocket is that a slow
// platform read can be abandoned, and a read that returns instantly cannot be.
type blockingTimeseries struct {
	*fakeTimeseries
	started chan struct{}
	release chan struct{}
	noticed chan struct{}
	once    sync.Once
}

func (b *blockingTimeseries) Query(ctx context.Context, token string, elements []timeseries.QueryElement, opts timeseries.QueryOptions) ([]timeseries.QueryResult, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return b.fakeTimeseries.Query(ctx, token, elements, opts)
	case <-ctx.Done():
		// Exactly what a cancelled HTTP request to the platform does.
		b.once.Do(func() { close(b.noticed) })
		return nil, ctx.Err()
	}
}

func newWSHarness(t *testing.T, blocking bool) *wsHarness {
	t.Helper()
	inner := newProfileHarness(t)

	harness := &wsHarness{
		profileHarness: inner,
		release:        make(chan struct{}),
		started:        make(chan struct{}, 1),
	}
	if blocking {
		harness.blocker = &blockingTimeseries{
			fakeTimeseries: inner.timeseries,
			started:        harness.started,
			release:        harness.release,
			noticed:        make(chan struct{}),
		}
		harness.profileHarness = newProfileHarnessWith(t, harness.blocker)
	}

	harness.server = httptest.NewServer(harness.router)
	t.Cleanup(harness.server.Close)
	return harness
}

func (h *wsHarness) dial(t *testing.T, roles ...string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws"
	header := http.Header{}
	if roles != nil {
		header.Set("Authorization", "Bearer "+mintToken(roles))
	}
	conn, response, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, message map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(message); err != nil {
		t.Fatalf("write: %v", err)
	}
}

type wsFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
	Error   string          `json:"error"`
	Status  int             `json:"status"`
}

// await reads until a frame of one of the wanted types arrives, so a test does not
// depend on how many progress or acknowledgement frames precede it.
func await(t *testing.T, conn *websocket.Conn, wanted ...string) wsFrame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set deadline: %v", err)
		}
		var frame wsFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read (waiting for %v): %v", wanted, err)
		}
		for _, want := range wanted {
			if frame.Type == want {
				return frame
			}
		}
	}
}

// --- tests ---

func TestTheWebSocketRefusesAnAnonymousCaller(t *testing.T) {
	harness := newWSHarness(t, false)
	url := "ws" + strings.TrimPrefix(harness.server.URL, "http") + "/ws"

	conn, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the upgrade succeeded without a token")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", response)
	}
}

// The role check is ODE's own authorisation decision and has to apply here too,
// not only on the HTTP routes (D5).
func TestTheWebSocketRefusesATokenWithoutTheDeveloperRole(t *testing.T) {
	harness := newWSHarness(t, false)
	url := "ws" + strings.TrimPrefix(harness.server.URL, "http") + "/ws"
	header := http.Header{"Authorization": []string{"Bearer " + mintToken([]string{"offline_access"})}}

	conn, response, err := websocket.DefaultDialer.Dial(url, header)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the upgrade succeeded without the developer role")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", response)
	}
}

// A browser cannot set an Authorization header on a WebSocket handshake, so the
// subprotocol has to work.
func TestTheWebSocketAcceptsATokenAsASubprotocol(t *testing.T) {
	harness := newWSHarness(t, false)
	url := "ws" + strings.TrimPrefix(harness.server.URL, "http") + "/ws"

	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{"ode.bearer.token." + mintToken([]string{"developer"})}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial with a subprotocol token: %v", err)
	}
	defer conn.Close()

	send(t, conn, map[string]any{"type": "ping", "id": "p1"})
	if frame := await(t, conn, "pong"); frame.ID != "p1" {
		t.Errorf("pong id = %q, want p1", frame.ID)
	}
}

func TestQuickProfilesOverTheWebSocketReturnsTheSameDocumentAsHTTP(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{"type": "quick_profiles", "id": "q1"})
	if frame := await(t, conn, "accepted"); frame.ID != "q1" {
		t.Errorf("accepted id = %q, want q1", frame.ID)
	}

	frame := await(t, conn, "result", "error")
	if frame.Type != "result" {
		t.Fatalf("frame = %s: %s", frame.Type, frame.Error)
	}

	var overWS map[string]any
	if err := json.Unmarshal(frame.Payload, &overWS); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	overHTTP := decode(t, harness.do(t, http.MethodGet, "/quick-profiles", nil, "developer"))

	// The WebSocket exists to escape an HTTP timeout, not to answer differently.
	for _, key := range []string{"candidates", "skipped", "reads", "coverage_window", "device_limit"} {
		if _, present := overWS[key]; !present {
			t.Errorf("%s is missing from the WebSocket result", key)
		}
		if _, present := overHTTP[key]; !present {
			t.Errorf("%s is missing from the HTTP result", key)
		}
	}
	wsCandidates, _ := overWS["candidates"].([]any)
	httpCandidates, _ := overHTTP["candidates"].([]any)
	if len(wsCandidates) != len(httpCandidates) {
		t.Errorf("candidates = %d over the socket against %d over HTTP",
			len(wsCandidates), len(httpCandidates))
	}
}

func TestAProfileOverTheWebSocketComputes(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{
		"type": "profile", "id": "p1",
		"payload": map[string]any{"device_id": testDeviceID, "service_id": testServiceID},
	})
	await(t, conn, "accepted")

	frame := await(t, conn, "result", "error")
	if frame.Type != "result" {
		t.Fatalf("frame = %s: %s", frame.Type, frame.Error)
	}
	var result struct {
		Profiles []struct {
			SeriesRef struct {
				VariablePath string `json:"variable_path"`
			} `json:"series_ref"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(frame.Payload, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Profiles) == 0 {
		t.Fatal("no profiles came back")
	}
}

// The reason the surface exists: a cancel has to reach the platform read and stop
// it, not merely stop the client waiting for it.
func TestACancelStopsAPlatformReadInFlight(t *testing.T) {
	harness := newWSHarness(t, true)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{
		"type": "profile", "id": "p1",
		"payload": map[string]any{"device_id": testDeviceID, "service_id": testServiceID},
	})
	await(t, conn, "accepted")

	// Wait until the read is genuinely in flight, so this is not a race that
	// happens to cancel before the work starts.
	select {
	case <-harness.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the platform read never started")
	}

	send(t, conn, map[string]any{"type": "cancel", "id": "p1"})

	frame := await(t, conn, "cancelled", "result", "error")
	if frame.Type != "cancelled" {
		t.Fatalf("frame = %s (%s), want cancelled — a cancelled read must not be "+
			"reported as a platform failure", frame.Type, frame.Error)
	}
}

// Dropping the connection is the common case — a closed tab, a reload — and it has
// to stop the work too, or the backend keeps paying for reads nobody will read.
func TestClosingTheConnectionCancelsItsWork(t *testing.T) {
	harness := newWSHarness(t, true)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{
		"type": "profile", "id": "p1",
		"payload": map[string]any{"device_id": testDeviceID, "service_id": testServiceID},
	})
	await(t, conn, "accepted")
	select {
	case <-harness.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the platform read never started")
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The blocked read returns only when its context is cancelled, so the fake
	// observing a cancellation is the assertion. Nothing releases it.
	select {
	case <-harness.cancelled(t):
	case <-time.After(5 * time.Second):
		t.Fatal("the read was still running after the connection closed")
	}
}

func TestARequestWithoutAnIdIsRefusedBecauseItCouldNotBeCancelled(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{"type": "quick_profiles"})
	frame := await(t, conn, "error")
	if !strings.Contains(frame.Error, "id") {
		t.Errorf("error = %q, want it to name the missing id", frame.Error)
	}
	if frame.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", frame.Status)
	}
}

func TestAnUnknownMessageTypeIsRefused(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{"type": "launch_experiment", "id": "x1"})
	frame := await(t, conn, "error")
	if frame.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", frame.Status)
	}
}

// A bad request over the socket carries the same classification it would over
// HTTP, so a client does not have to guess whether to retry.
func TestABadRequestOverTheWebSocketCarriesItsStatus(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{
		"type": "profile", "id": "p1",
		"payload": map[string]any{"device_id": testDeviceID, "service_id": "urn:infai:ses:service:absent"},
	})
	await(t, conn, "accepted")

	frame := await(t, conn, "error", "result")
	if frame.Type != "error" {
		t.Fatalf("frame = %s, want error", frame.Type)
	}
	if frame.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", frame.Status)
	}
}

func TestTwoRequestsWithTheSameIdAreRefused(t *testing.T) {
	harness := newWSHarness(t, true)
	conn := harness.dial(t, "developer")

	body := map[string]any{"device_id": testDeviceID, "service_id": testServiceID}
	send(t, conn, map[string]any{"type": "profile", "id": "dup", "payload": body})
	await(t, conn, "accepted")
	select {
	case <-harness.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first read never started")
	}

	send(t, conn, map[string]any{"type": "profile", "id": "dup", "payload": body})
	frame := await(t, conn, "error")
	if frame.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409 for a duplicate id", frame.Status)
	}
}

// Cancelling something already finished is normal: the client cannot know the
// result was in flight when it changed its mind.
func TestCancellingAFinishedRequestIsNotAnError(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{"type": "quick_profiles", "id": "q1"})
	await(t, conn, "accepted")
	await(t, conn, "result", "error")

	send(t, conn, map[string]any{"type": "cancel", "id": "q1"})
	if frame := await(t, conn, "cancelled", "error"); frame.Type != "cancelled" {
		t.Errorf("frame = %s (%s), want a plain cancelled", frame.Type, frame.Error)
	}
}

// Semantic selection runs over the socket for the same reason the candidate
// listing does: a resolution expands devices, and availability is one call per
// device that a client changing its mind should be able to stop.
func TestSelectionOverTheWebSocketReturnsTheSameDocument(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{
		"type": "resolve_selection", "id": "s1",
		"payload": map[string]any{"intent": testIntent},
	})
	await(t, conn, "accepted")

	frame := await(t, conn, "result", "error")
	if frame.Type != "result" {
		t.Fatalf("frame = %s (%s), want a result", frame.Type, frame.Error)
	}

	var payload struct {
		Candidates []struct {
			SeriesRef struct {
				VariablePath string `json:"variable_path"`
			} `json:"series_ref"`
		} `json:"candidates"`
		Reads struct {
			Values int `json:"values"`
		} `json:"reads"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Candidates) != 1 || payload.Candidates[0].SeriesRef.VariablePath != powerPath {
		t.Errorf("candidates = %+v, want the one resolved series", payload.Candidates)
	}
	if payload.Reads.Values != 0 {
		t.Errorf("reads.values = %d, want 0 over this surface too", payload.Reads.Values)
	}
}

// The socket classifies errors through the same function the HTTP routes use, so
// a malformed selection payload carries 400 rather than leaving the client to
// guess whether the platform is down.
func TestAMalformedSelectionOverTheWebSocketCarries400(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{
		"type": "resolve_selection", "id": "s1",
		"payload": map[string]any{"intent": testIntent, "window": map[string]any{"from": "not-a-timestamp", "to": "also-not"}},
	})
	await(t, conn, "accepted")

	frame := await(t, conn, "error", "result")
	if frame.Type != "error" {
		t.Fatalf("frame = %s, want error", frame.Type)
	}
	if frame.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", frame.Status)
	}
}

// The Profiler view lists candidates over the socket, not over the HTTP route, so
// the device block has to be on this surface too. It is the same struct through the
// same operation — this test exists because "same struct" is exactly the kind of
// assumption that stops being true, and the symptom is a table of URNs.
func TestCandidatesOverTheWebSocketCarryTheirDeviceNames(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dial(t, "developer")

	send(t, conn, map[string]any{"type": "quick_profiles", "id": "q1", "payload": map[string]any{"limit": 10}})
	await(t, conn, "accepted")

	frame := await(t, conn, "result", "error")
	if frame.Type != "result" {
		t.Fatalf("frame = %s (%s), want a result", frame.Type, frame.Error)
	}

	var payload struct {
		Candidates []struct {
			SeriesRef struct {
				DeviceID string `json:"device_id"`
			} `json:"series_ref"`
			Device struct {
				Name           string `json:"name"`
				DeviceTypeID   string `json:"device_type_id"`
				DeviceTypeName string `json:"device_type_name"`
			} `json:"device"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	for _, candidate := range payload.Candidates {
		if candidate.Device.Name == "" {
			t.Errorf("candidate %s carries no device name; the list would read as URNs",
				candidate.SeriesRef.DeviceID)
		}
		if candidate.Device.DeviceTypeName == "" {
			t.Errorf("candidate %s carries no device type name", candidate.SeriesRef.DeviceID)
		}
	}
}

// --- token refresh: the connection outlives the token ---

// mintTokenAs is mintToken with a chosen subject and a nonce. The nonce is what
// makes a refresh testable: two tokens with identical claims are the same string,
// and a test could not then tell which one a read presented.
func mintTokenAs(sub, nonce string, roles []string) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "gateway"}
	claims := map[string]any{
		"sub":                sub,
		"jti":                nonce,
		"preferred_username": "dev",
		"realm_access":       map[string]any{"roles": roles},
	}
	return segment(header) + "." + segment(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("signature-checked-at-the-gateway"))
}

func (h *wsHarness) dialWith(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/ws"
	conn, response, err := websocket.DefaultDialer.Dial(url,
		http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A handshake authenticates once and the connection then lives as long as the tab.
// The access token does not: the SPA refreshes it on a thirty-second horizon, and a
// connection that kept its handshake copy would present an expired credential on
// every read after that — a 401 from the platform that looks like a platform fault
// and disappears on reload.
func TestARefreshedTokenIsWhatTheNextOperationPresents(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dialWith(t, mintTokenAs("user-123", "handshake", []string{"developer"}))

	send(t, conn, map[string]any{"type": "quick_profiles", "id": "q1"})
	if frame := await(t, conn, "result", "error"); frame.Type != "result" {
		t.Fatalf("the first operation failed: %s", frame.Error)
	}

	refreshed := mintTokenAs("user-123", "refreshed", []string{"developer"})
	send(t, conn, map[string]any{
		"type": "auth", "id": "a1", "payload": map[string]any{"token": refreshed},
	})
	if frame := await(t, conn, "result", "error"); frame.Type != "result" {
		t.Fatalf("the refresh was refused: %s", frame.Error)
	}

	send(t, conn, map[string]any{"type": "quick_profiles", "id": "q2"})
	if frame := await(t, conn, "result", "error"); frame.Type != "result" {
		t.Fatalf("the second operation failed: %s", frame.Error)
	}
	if got, want := harness.devices.token(), "Bearer "+refreshed; got != want {
		t.Errorf("upstream token = %q, want the refreshed one", got)
	}
}

// ODE reads claims without verifying them — the gateway verifies (§3.1) — so `sub`
// is the only thing tying this connection's identity, its chat sessions and its
// spend against the §3.3 cap, to the credential its reads are made with. Adopting
// another subject's token would silently attribute one user's reads to another.
func TestARefreshedTokenForAnotherSubjectIsRefusedAndTheOldOneKept(t *testing.T) {
	harness := newWSHarness(t, false)
	original := mintTokenAs("user-123", "handshake", []string{"developer"})
	conn := harness.dialWith(t, original)

	send(t, conn, map[string]any{
		"type": "auth", "id": "a1",
		"payload": map[string]any{"token": mintTokenAs("user-999", "stolen", []string{"developer"})},
	})
	frame := await(t, conn, "result", "error")
	if frame.Type != "error" || frame.Status != http.StatusForbidden {
		t.Fatalf("frame = %+v, want a 403 error", frame)
	}

	send(t, conn, map[string]any{"type": "quick_profiles", "id": "q1"})
	if frame := await(t, conn, "result", "error"); frame.Type != "result" {
		t.Fatalf("the connection stopped working after a refused refresh: %s", frame.Error)
	}
	if got, want := harness.devices.token(), "Bearer "+original; got != want {
		t.Errorf("upstream token = %q, want the connection's original", got)
	}
}

// The realm role is ODE's own authorisation decision (D5). A role revoked
// while the tab was open must end the connection's authority rather than survive
// in a socket nobody re-authorised.
func TestARefreshedTokenWithoutTheDeveloperRoleIsRefused(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dialWith(t, mintTokenAs("user-123", "handshake", []string{"developer"}))

	send(t, conn, map[string]any{
		"type": "auth", "id": "a1",
		"payload": map[string]any{"token": mintTokenAs("user-123", "demoted", []string{"offline_access"})},
	})
	frame := await(t, conn, "result", "error")
	if frame.Type != "error" || frame.Status != http.StatusForbidden {
		t.Fatalf("frame = %+v, want a 403 error", frame)
	}
}

func TestAnUnparseableRefreshedTokenIsRefused(t *testing.T) {
	harness := newWSHarness(t, false)
	conn := harness.dialWith(t, mintTokenAs("user-123", "handshake", []string{"developer"}))

	send(t, conn, map[string]any{
		"type": "auth", "id": "a1", "payload": map[string]any{"token": "not-a-jwt"},
	})
	frame := await(t, conn, "result", "error")
	if frame.Type != "error" || frame.Status != http.StatusUnauthorized {
		t.Fatalf("frame = %+v, want a 401 error", frame)
	}
}
