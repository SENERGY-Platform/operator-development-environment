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
	"fmt"
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
	// refs records what each execution asked for, so the containment flag can be
	// followed from the tool's argument through to the kernel.
	refs []kernel.Ref
	err  error

	// files is what the workspace holds, by path, for upload_simulation_dataset.
	files map[string]kernel.FileContent
	// readErr fails a read, so a tool that has to refuse rather than upload
	// something it could not read can be tested.
	readErr error
	// read records the paths that were asked for.
	read []string
}

func (f *fakeKernel) Workspace() string { return "data/ode" }

func (f *fakeKernel) ReadFile(_ context.Context, _ kernel.Ref, path string, _ int) (kernel.FileContent, error) {
	f.read = append(f.read, path)
	if f.readErr != nil {
		return kernel.FileContent{}, f.readErr
	}
	content, found := f.files[path]
	if !found {
		return kernel.FileContent{}, fmt.Errorf("no such file: %s", path)
	}
	return content, nil
}

func (f *fakeKernel) RunQueued(_ context.Context, ref kernel.Ref, code string) (<-chan kernel.ExecutionEvent, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.code = append(f.code, code)
	f.refs = append(f.refs, ref)
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

// --- containment ---

/*
Where cells are contained, the confirmation moves from the code to the credential.

That is the whole change and it is worth stating as a property rather than a
setting: a cell that did not ask for the platform token runs without anybody being
asked — including one full of `subprocess`, which the recogniser would have
refused and which is now beside the point, because a kernel with no token in it
does not become more dangerous for containing the word. A cell that *did* ask is
asking for precisely the authority the confirmation exists to check, so it is
never waived, and no configuration makes it waivable.

The third test is the rollback: with the option off, nothing about auto mode moves.
*/
func containedSurface(t *testing.T, fake *fakeKernel, contain bool) *Registry {
	t.Helper()
	registry, err := NewSurface(Deps{Kernel: fake, ContainCells: contain})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

func dispatchAuto(t *testing.T, registry *Registry, input string) (Result, *Dispatcher) {
	t.Helper()
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer " + strings.Repeat("t", 40), UserSub: "u", SessionID: "s",
			Tier: L0, AutoRun: true},
		Call{ID: "c1", Name: "run_code", Input: json.RawMessage(input)}), dispatcher
}

func TestAContainedCellRunsWithoutBeingConfirmed(t *testing.T) {
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindDone, Status: kernel.StatusOK},
	}}
	registry := containedSurface(t, fake, true)

	// Not a dull cell by any reading, and that is the point: the recogniser is no
	// longer what decides.
	result, _ := dispatchAuto(t, registry, `{"code":"import subprocess\nsubprocess.run(['ls'])"}`)
	if result.Outcome != OutcomeOK {
		t.Fatalf("a contained cell was held: outcome = %q, content %v", result.Outcome, result.Content)
	}
	if len(fake.refs) != 1 {
		t.Fatalf("the cell did not reach the kernel: %d executions", len(fake.refs))
	}
	if fake.refs[0].WithPlatformToken {
		t.Error("a cell that did not ask for the token was given one")
	}
}

func TestACellThatAsksForTheTokenIsAlwaysConfirmed(t *testing.T) {
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindDone, Status: kernel.StatusOK},
	}}
	registry := containedSurface(t, fake, true)

	// Dull by the recogniser's own reckoning, so this is not the code being
	// refused — it is the request for the credential.
	result, _ := dispatchAuto(t, registry, `{"code":"df.head()","needs_platform_token":true}`)
	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Fatalf("a cell asking for the token skipped the confirmation: %q", result.Outcome)
	}
	if len(fake.code) != 0 {
		t.Fatal("it reached the kernel before the developer answered")
	}
}

func TestWithoutContainmentTheRecogniserStillDecides(t *testing.T) {
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindDone, Status: kernel.StatusOK},
	}}
	registry := containedSurface(t, fake, false)

	result, _ := dispatchAuto(t, registry, `{"code":"import subprocess\nsubprocess.run(['ls'])"}`)
	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Errorf("with containment off, an unrecognised cell ran unasked: %q", result.Outcome)
	}

	fake2 := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindDone, Status: kernel.StatusOK},
	}}
	result, _ = dispatchAuto(t, containedSurface(t, fake2, false), `{"code":"df.head()"}`)
	if result.Outcome != OutcomeOK {
		t.Errorf("with containment off, a recognised cell was held: %q", result.Outcome)
	}
}

/*
The failure that turns a contained run into a confirmed one.

Without this hint the model is handed a cell that failed for a reason it has no
way to act on, and the likeliest thing it does next is rewrite code that was
already correct. The hint is the only path from "ran contained" to "ask the
developer", so it is load-bearing rather than a nicety.
*/
func TestAContainedCellThatNeededTheTokenSaysHowToAskForIt(t *testing.T) {
	fake := &fakeKernel{events: []kernel.ExecutionEvent{
		{Kind: kernel.KindError, ErrorName: "RuntimeError",
			ErrorValue: "SENERGY_TOKEN is not set: this kernel was not started by ODE, " +
				"or the session has not pushed a token yet"},
		{Kind: kernel.KindDone, Status: "error"},
	}}
	registry := containedSurface(t, fake, true)

	result, _ := dispatchAuto(t, registry, `{"code":"ode_platform.token()"}`)
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q, content %v", result.Outcome, result.Content)
	}
	payload, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got RunCodeResult
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got.Hint, "needs_platform_token") {
		t.Errorf("the failure does not say how to ask for the token: %q", got.Hint)
	}

	// And a cell that already asked is not told to ask again.
	fake2 := &fakeKernel{events: fake.events}
	result, _ = dispatchAuto(t, containedSurface(t, fake2, true),
		`{"code":"ode_platform.token()","needs_platform_token":true}`)
	if result.Outcome != OutcomeAwaitingConfirmation {
		t.Fatalf("expected the confirmation, got %q", result.Outcome)
	}
}
