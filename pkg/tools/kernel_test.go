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

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// fakeKernel answers with a fixed event stream, so these tests are about the
// projection run_code makes rather than about the kernel protocol — pkg/kernel
// tests that against a hub double of its own.
type fakeKernel struct {
	events []kernel.ExecutionEvent
	code   []string
	err    error
}

func (f *fakeKernel) Workspace() string { return "data/ode" }

func (f *fakeKernel) RunQueued(_ context.Context, _ kernel.Ref, code string) (<-chan kernel.ExecutionEvent, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.code = append(f.code, code)
	out := make(chan kernel.ExecutionEvent, len(f.events))
	for _, event := range f.events {
		out <- event
	}
	close(out)
	return out, nil
}

func runCodeSurface(t *testing.T, fake *fakeKernel, budget int) *Registry {
	t.Helper()
	registry, err := NewSurface(Deps{Kernel: fake, RunCodeMaxOutputBytes: budget})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

func dispatchRunCode(t *testing.T, registry *Registry, token, code string) Result {
	t.Helper()
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	input, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Confirm rather than Dispatch: run_code is a confirmed tool, so Dispatch only
	// ever holds it. Reaching the executor at all means the developer said yes.
	return dispatcher.Confirm(context.Background(),
		Request{Token: token, UserSub: "user-1", Tier: L0},
		PendingConfirmation{CallID: "call-1", Tool: "run_code", Input: input, Tier: L0})
}

func TestRunCodeIsHeldForConfirmationBeforeAnythingRuns(t *testing.T) {
	fake := &fakeKernel{}
	registry := runCodeSurface(t, fake, 0)
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", Tier: L0},
		Call{ID: "c1", Name: "run_code", Input: json.RawMessage(`{"code":"print(1)"}`)})

	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeAwaitingConfirmation)
	}
	if len(fake.code) != 0 {
		t.Errorf("code ran before the developer confirmed: %v", fake.code)
	}
}

func TestRunCodeProjectsTheStreamToWhatAModelCanRead(t *testing.T) {
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindStatus, State: "busy"},
		{Kind: kernel.KindStream, Stream: "stdout", Text: "loaded 42 rows\n"},
		{Kind: kernel.KindStream, Stream: "stderr", Text: "a warning\n"},
		{Kind: kernel.KindResult, Text: "<Figure>", MIME: map[string]string{
			"image/png": strings.Repeat("A", 4096),
		}},
		{Kind: kernel.KindDone, Status: kernel.StatusOK},
	}}
	result := dispatchRunCode(t, runCodeSurface(t, fake, 0), "Bearer token", "load()")

	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	out, ok := result.Content.(RunCodeResult)
	if !ok {
		t.Fatalf("content = %T, want RunCodeResult", result.Content)
	}
	if out.Stdout != "loaded 42 rows\n" || out.Stderr != "a warning\n" {
		t.Errorf("streams = %+v, want stdout and stderr kept apart", out)
	}
	if out.Status != kernel.StatusOK || out.Workspace != "data/ode" {
		t.Errorf("result = %+v, want the status and the workspace", out)
	}
	// The image is named, not carried: a model cannot read a base64 PNG, and §5.9
	// makes a chart a declarative spec rather than an image.
	if len(out.Displays) != 1 || out.Displays[0] != "image/png" {
		t.Errorf("displays = %v, want the media type only", out.Displays)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), strings.Repeat("A", 64)) {
		t.Error("the image payload reached the model")
	}
}

func TestRunCodeReportsAnExceptionWithoutFailingTheCall(t *testing.T) {
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindError, ErrorName: "KeyError", ErrorValue: "'missing'",
			Text: "Traceback...\nKeyError: 'missing'"},
		{Kind: kernel.KindDone, Status: kernel.StatusError},
	}}
	result := dispatchRunCode(t, runCodeSurface(t, fake, 0), "Bearer token", "d['missing']")

	// The tool worked; the code did not. Reporting this as a tool failure would
	// teach a model that run_code is unreliable rather than that its code was wrong.
	if result.Outcome != OutcomeOK || result.IsError {
		t.Fatalf("outcome = %q is_error = %v, want a successful call carrying the error",
			result.Outcome, result.IsError)
	}
	out := result.Content.(RunCodeResult)
	if out.ErrorName != "KeyError" || out.Status != kernel.StatusError {
		t.Errorf("result = %+v, want the exception reported", out)
	}
}

func TestRunCodeKeepsTheWholeAnswerInsideOneBudget(t *testing.T) {
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindStream, Stream: "stdout", Text: strings.Repeat("x", 100)},
		{Kind: kernel.KindStream, Stream: "stderr", Text: strings.Repeat("y", 100)},
		{Kind: kernel.KindDone, Status: kernel.StatusOK},
	}}
	result := dispatchRunCode(t, runCodeSurface(t, fake, 50), "Bearer token", "spam()")
	out := result.Content.(RunCodeResult)

	// One budget for the answer, not one per stream: a cell that floods stderr
	// would otherwise still cost the model a full stdout budget on top.
	if total := len(out.Stdout) + len(out.Stderr); total > 50 {
		t.Errorf("returned %d bytes, want at most the 50-byte budget", total)
	}
	if !out.Truncated || out.Hint == "" {
		t.Error("truncation was not reported, so a partial answer reads as a whole one")
	}
}

// The platform token is installed in the kernel (§5.6 item 4), so a cell that
// prints its environment while debugging would otherwise put a live credential
// into the conversation — which is persisted to Postgres.
//
// This is hygiene against the accidental case, not a boundary: code that
// deliberately encodes the token defeats it, which is why the developer's
// confirmation is the actual control.
func TestRunCodeRedactsThePlatformTokenFromWhatItReturns(t *testing.T) {
	token := "eyJhbGciOiJSUzI1NiJ9.aaaaaaaaaaaaaaaaaaaaaaaa.bbbb"
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindStream, Stream: "stdout", Text: "SENERGY_TOKEN=" + token + "\n"},
		{Kind: kernel.KindDone, Status: kernel.StatusOK},
	}}
	result := dispatchRunCode(t, runCodeSurface(t, fake, 0), "Bearer "+token, "print(os.environ)")
	out := result.Content.(RunCodeResult)

	if strings.Contains(out.Stdout, token) {
		t.Errorf("the platform token survived into the tool result: %q", out.Stdout)
	}
	if !strings.Contains(out.Stdout, "redacted") {
		t.Errorf("stdout = %q, want the redaction to be visible rather than silent", out.Stdout)
	}
}

func TestRunCodeIsDeclaredButUnavailableWithoutAnExecutionBackend(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, found := registry.Lookup("run_code")
	if !found {
		t.Fatal("run_code is not declared")
	}
	if definition.Implemented() {
		t.Error("run_code has an executor with no jupyterhub configured")
	}
	if !strings.Contains(definition.Unavailable, "jupyterhub_url") {
		t.Errorf("unavailable = %q, want it to name the missing configuration",
			definition.Unavailable)
	}
}

func TestRunCodeRefusesAnEmptyCell(t *testing.T) {
	fake := &fakeKernel{}
	result := dispatchRunCode(t, runCodeSurface(t, fake, 0), "Bearer t", "   ")

	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeInvalidInput)
	}
	if len(fake.code) != 0 {
		t.Errorf("an empty cell reached the kernel: %v", fake.code)
	}
}
