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
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The access log is written after the handler returns, so the connection is closed and the
// log polled before asserting.
func TestTheAccessLogDoesNotCarryTheAccessTokenQueryParameter(t *testing.T) {
	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	harness := newWSHarness(t, false)
	token := mintToken([]string{"developer"})
	url := "ws" + strings.TrimPrefix(harness.server.URL, "http") + "/ws?access_token=" + token + "&other=keep"

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial with an access_token parameter: %v", err)
	}
	send(t, conn, map[string]any{"type": "ping", "id": "p1"})
	if frame := await(t, conn, "pong"); frame.ID != "p1" {
		t.Errorf("pong id = %q, want p1", frame.ID)
	}
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "other=keep") {
		if time.Now().After(deadline) {
			t.Fatalf("no access log line for the request within 5s, got: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := logs.String(); strings.Contains(out, token) || strings.Contains(out, "signature-checked-at-the-gateway") {
		t.Errorf("access log contains the access token: %s", out)
	}
}
