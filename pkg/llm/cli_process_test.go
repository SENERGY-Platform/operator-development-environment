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

package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The CLI provider is the one provider that starts a process, so the properties
// worth testing about it are properties of that process: what its command line
// exposes, and what is left behind when a turn ends early. Both need a real child,
// so these tests install a stand-in for the `claude` binary as a shell script.
//
// Kept in its own file because it is the only part of the package that needs a
// Unix shell, and skipping it has to be visible rather than buried.

func requireUnixShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("this test needs a Unix shell to stand in for the claude binary")
	}
}

// fakeCLI writes an executable script that behaves like the CLI's stream-json
// mode and returns its path.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake CLI: %v", err)
	}
	return path
}

// probedCLIProvider is a provider that has been told the binary works, so Stream
// takes the MCP path rather than the degraded one.
func probedCLIProvider(t *testing.T, binary string, timeout time.Duration) *AnthropicCLIProvider {
	t.Helper()
	provider := NewAnthropicCLIProvider("claude-cli", CLIOptions{
		Binary: binary, Timeout: timeout, ProbeTimeout: 5 * time.Second,
	}, NewPricing("EUR"))
	provider.set(Capabilities{Streaming: true, System: true, Tools: true, ToolsOutOfBand: true})
	return provider
}

func testToolEndpoint(token string) *ToolEndpoint {
	return &ToolEndpoint{
		URL: "https://ode.example.org/mcp", Token: token, SessionID: "sess-1",
		AllowedTools: []string{"list_devices"},
	}
}

// The developer's Keycloak access token is the whole of their platform
// authorisation: §3.1 item 3 and D5 make every device and timeseries read happen
// on behalf of whoever presents it. A command line is not a private channel —
// /proc/<pid>/cmdline is world-readable and `ps auxww` prints it — so a token
// passed as an argument is readable by anything else in the ODE container, which
// runs LLM-authored code, for as long as the token lives.
func TestTheDevelopersTokenIsNeverPassedOnAChildProcessCommandLine(t *testing.T) {
	requireUnixShell(t)

	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	configPath := filepath.Join(dir, "config")

	binary := fakeCLI(t, `
: > "$ODE_TEST_ARGV"
prev=""
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$ODE_TEST_ARGV"
  if [ "$prev" = "--mcp-config" ] && [ -f "$arg" ]; then
    cp -p "$arg" "$ODE_TEST_CONFIG"
  fi
  prev="$arg"
done
printf '%s\n' '{"type":"result","subtype":"success","usage":{"input_tokens":7,"output_tokens":3}}'
`)
	t.Setenv("ODE_TEST_ARGV", argvPath)
	t.Setenv("ODE_TEST_CONFIG", configPath)

	const token = "eyJhbGciOiJSUzI1NiJ9.the-developers-access-token.signature"
	provider := probedCLIProvider(t, binary, 30*time.Second)
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("which devices are there?")}, ToolEndpoint: testToolEndpoint(token),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainEvents(t, stream)

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("the fake CLI recorded no argv: %v", err)
	}
	if strings.Contains(string(argv), token) {
		t.Errorf("the access token is in the child's command line:\n%s", argv)
	}
	if strings.Contains(string(argv), "Authorization") {
		t.Errorf("the Authorization header is in the child's command line:\n%s", argv)
	}

	// It still has to reach the CLI, or the MCP server refuses every tool call and
	// the fix has traded a leak for a broken provider.
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("no MCP configuration reached the CLI: %v", err)
	}
	if !strings.Contains(string(config), token) {
		t.Errorf("the MCP configuration does not carry the token:\n%s", config)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Copied with its mode by the fake CLI: the file holds a bearer token and must
	// not be readable by anything else running in the container.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the MCP configuration file is mode %#o, want no group or other access", mode)
	}

	// And nothing is left on disk once the turn is over.
	for _, line := range strings.Split(strings.TrimSpace(string(argv)), "\n") {
		if !strings.HasSuffix(line, ".json") || !filepath.IsAbs(line) {
			continue
		}
		if _, err := os.Stat(line); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after the turn (stat error: %v)", line, err)
		}
	}
}

// A turn that ends before the CLI does used to abandon the process: cancel() kills
// it but nothing reaps it, so the entry stays in the process table as a zombie, the
// stdout pipe stays open, and the two goroutines os/exec runs for the process — the
// context watcher and the stderr copier — never return. A few hundred cancelled
// turns exhaust the file descriptor limit and the process table with no recovery
// short of restarting ODE.
func TestAPrematurelyEndedCLITurnLeavesNoZombieAndNoGoroutines(t *testing.T) {
	requireUnixShell(t)

	// Reports an error result and then keeps running, which is what a real agent
	// loop does: the turn is over for ODE long before the process is.
	binary := fakeCLI(t, `
printf '%s\n' '{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":11,"output_tokens":4}}}'
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the agent loop failed"}'
exec sleep 60
`)
	provider := probedCLIProvider(t, binary, 60*time.Second)

	settle(t)
	before := runtime.NumGoroutine()

	const turns = 3
	for i := 0; i < turns; i++ {
		stream, err := provider.Stream(context.Background(), Request{
			Messages: []Message{UserText("go")}, ToolEndpoint: testToolEndpoint("Bearer t"),
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		drainEvents(t, stream)
	}

	// Polled rather than measured once: reaping is asynchronous, and a test that
	// insisted on an instant is a flake waiting to happen.
	deadline := time.Now().Add(10 * time.Second)
	after := 0
	zombies := 0
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		zombies = countZombieChildren(t)
		if after <= before && zombies == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after > before {
		t.Errorf("goroutines %d -> %d after %d prematurely ended turns; "+
			"the process was never waited for", before, after, turns)
	}
	if zombies > 0 {
		t.Errorf("%d zombie child processes after %d prematurely ended turns", zombies, turns)
	}
}

// The CLI is an agent loop: it runs a bash tool and stdio MCP servers as its own
// children, and those inherit the stderr pipe os/exec created for it. One of them
// outliving the CLI keeps the write end open, and Wait waits for that pipe to
// close — for ever, unless it is bounded. Since the event channel is closed after
// Wait, an unbounded wait does not merely leak: the exchange never ends, the
// session stays marked as busy, and nothing the developer can do releases it.
func TestATurnEndsEvenWhenTheCLILeavesAChildHoldingItsStderr(t *testing.T) {
	requireUnixShell(t)

	binary := fakeCLI(t, `
sleep 120 &
printf '%s\n' '{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":11,"output_tokens":4}}}'
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the agent loop failed"}'
exec sleep 120
`)
	// Its own temporary directory, so the leftover check below sees this turn's
	// files and nothing else on the machine.
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)

	provider := probedCLIProvider(t, binary, 60*time.Second)
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("go")}, ToolEndpoint: testToolEndpoint("Bearer t"),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	closed := make(chan []Event, 1)
	go func() {
		out := []Event{}
		for event := range stream {
			out = append(out, event)
		}
		closed <- out
	}()

	select {
	case events := <-closed:
		if len(eventsOfType(events, EventDone)) != 1 {
			t.Errorf("done events = %d, want 1", len(eventsOfType(events, EventDone)))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the provider never closed its event channel; a surviving grandchild " +
			"held the stderr pipe open and the turn hung on Wait")
	}

	// And the configuration file, which holds the developer's token, is gone —
	// its removal is deferred behind the same Wait.
	leftovers, err := filepath.Glob(filepath.Join(temp, "ode-mcp-*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) > 0 {
		t.Errorf("MCP configuration files left on disk: %v", leftovers)
	}
}

// A CLI turn that ends early has still been billed: the assistant messages it
// produced were paid for. Reporting no done event leaves turn.usage zero in the
// chat engine and skips the §3.3 accounting altogether, which makes an
// interrupted turn free and repeatable.
func TestAFailedCLITurnStillReportsWhatItSpent(t *testing.T) {
	requireUnixShell(t)

	binary := fakeCLI(t, `
printf '%s\n' '{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":11,"output_tokens":4}}}'
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the agent loop failed"}'
`)
	provider := probedCLIProvider(t, binary, 30*time.Second)
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("go")}, ToolEndpoint: testToolEndpoint("Bearer t"),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainEvents(t, stream)

	done := eventsOfType(events, EventDone)
	if len(done) != 1 {
		t.Fatalf("done events = %d, want 1 even though the turn failed", len(done))
	}
	if got := done[0].Usage.InputTokens + done[0].Usage.OutputTokens; got != 15 {
		t.Errorf("reported tokens = %d, want the 15 the turn actually spent", got)
	}
	// Nothing downstream may read this as a clean finish.
	if done[0].StopReason == "end_turn" || done[0].StopReason == "" {
		t.Errorf("stop reason = %q; a failed turn must be distinguishable from a finished one",
			done[0].StopReason)
	}
	if len(eventsOfType(events, EventError)) != 1 {
		t.Errorf("error events = %d, want the failure still reported",
			len(eventsOfType(events, EventError)))
	}
	// The done event has to arrive before the error, because a consumer stops
	// reading at an error and would otherwise never see what the turn cost.
	if indexOfType(events, EventDone) > indexOfType(events, EventError) {
		t.Error("the done event came after the error, where a consumer will not see it")
	}
}

// The same for a turn the developer stops: the tokens were spent whether or not
// anyone waited for the answer.
func TestACancelledCLITurnStillReportsWhatItSpent(t *testing.T) {
	requireUnixShell(t)

	binary := fakeCLI(t, `
printf '%s\n' '{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":11,"output_tokens":4}}}'
exec sleep 60
`)
	provider := probedCLIProvider(t, binary, 60*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := provider.Stream(ctx, Request{
		Messages: []Message{UserText("go")}, ToolEndpoint: testToolEndpoint("Bearer t"),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	events := []Event{}
	for event := range stream {
		events = append(events, event)
		if event.Type == EventTextDelta {
			cancel()
		}
	}

	done := eventsOfType(events, EventDone)
	if len(done) != 1 {
		t.Fatalf("done events = %d, want 1 even though the turn was cancelled", len(done))
	}
	if got := done[0].Usage.InputTokens + done[0].Usage.OutputTokens; got != 15 {
		t.Errorf("reported tokens = %d, want the 15 the turn had already spent", got)
	}
	if done[0].StopReason != StopReasonCancelled {
		t.Errorf("stop reason = %q, want %q", done[0].StopReason, StopReasonCancelled)
	}
}

// --- helpers ---

func drainEvents(t *testing.T, stream <-chan Event) []Event {
	t.Helper()
	out := []Event{}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event, open := <-stream:
			if !open {
				return out
			}
			out = append(out, event)
		case <-deadline:
			t.Fatalf("the turn did not finish within 30s (%d events so far)", len(out))
			return out
		}
	}
}

func eventsOfType(events []Event, kind EventType) []Event {
	out := []Event{}
	for _, event := range events {
		if event.Type == kind {
			out = append(out, event)
		}
	}
	return out
}

func indexOfType(events []Event, kind EventType) int {
	for i, event := range events {
		if event.Type == kind {
			return i
		}
	}
	return len(events)
}

// settle waits for goroutines started by earlier work to finish, so a baseline
// count means what it says.
func settle(t *testing.T) {
	t.Helper()
	previous := -1
	for i := 0; i < 40; i++ {
		current := runtime.NumGoroutine()
		if current == previous {
			return
		}
		previous = current
		time.Sleep(25 * time.Millisecond)
	}
}

// countZombieChildren reads /proc for children of this process that have exited
// and not been reaped. This is how the leak shows in the process table, which is
// the resource that runs out.
func countZombieChildren(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	zombies := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// The command name is parenthesised and may itself contain spaces, so the
		// fields after it are read from the last closing parenthesis.
		end := strings.LastIndex(string(raw), ")")
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(raw)[end+1:])
		if len(fields) < 2 {
			continue
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil || parent != self {
			continue
		}
		if fields[0] == "Z" {
			zombies++
		}
	}
	return zombies
}
