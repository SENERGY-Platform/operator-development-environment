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
	"errors"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// fakeRepo records what write_file asked for. The service's own refusals — a path
// that leaves the repository, a write into .git — are pkg/repo's tests; these are
// about the tool.
type fakeRepo struct {
	writes []struct {
		Request repo.Request
		Path    string
		Content string
	}
	err error
}

func (f *fakeRepo) WriteFile(
	_ context.Context, req repo.Request, path string, content []byte,
) (repo.WriteResult, error) {
	if f.err != nil {
		return repo.WriteResult{}, f.err
	}
	f.writes = append(f.writes, struct {
		Request repo.Request
		Path    string
		Content string
	}{req, path, string(content)})
	return repo.WriteResult{
		Path: path, Size: int64(len(content)), Repository: "jonah/pv-forecast",
	}, nil
}

func writeFileSurface(t *testing.T, fake *fakeRepo) *Registry {
	t.Helper()
	registry, err := NewSurface(Deps{Repo: fake})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

func dispatchWriteFile(t *testing.T, registry *Registry, path, content string) Result {
	t.Helper()
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	input, err := json.Marshal(map[string]string{"path": path, "content": content})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer developer-token", UserSub: "user-1", Tier: L0},
		Call{ID: "call-1", Name: "write_file", Input: input})
}

// §5.8 puts write_file at L0 with no confirmation. Both halves are asserted here
// because both are deliberate: the tool carries no platform data, and it cannot
// publish anything.
func TestWriteFileIsAvailableAtL0WithoutAConfirmation(t *testing.T) {
	registry := writeFileSurface(t, &fakeRepo{})

	definition, found := registry.Lookup("write_file")
	if !found {
		t.Fatal("write_file is not in the registry")
	}
	if definition.MinTier != L0 || definition.Confirm {
		t.Errorf("definition = tier %s confirm %v, want L0 without confirmation",
			definition.MinTier, definition.Confirm)
	}
	if !definition.Implemented() {
		t.Fatal("write_file has no executor, so M7 did not reach the tool surface")
	}
	if definition.Unavailable != "" {
		t.Errorf("unavailable = %q, want it cleared once the executor is there",
			definition.Unavailable)
	}
}

func TestWriteFileWritesTheWorkingCopyAndSaysItIsNotCommitted(t *testing.T) {
	fake := &fakeRepo{}
	result := dispatchWriteFile(t, writeFileSurface(t, fake), "op.py", "# new operator\n")

	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %+v, want one", fake.writes)
	}
	write := fake.writes[0]
	if write.Path != "op.py" || write.Content != "# new operator\n" {
		t.Errorf("write = %+v", write)
	}
	// The developer's own token and subject reach the service, because the working
	// copy is in their pod and the write is on their behalf (§3.1 step 3).
	if write.Request.Bearer != "Bearer developer-token" || write.Request.UserSub != "user-1" {
		t.Errorf("request = %+v, want the developer's own credential", write.Request)
	}

	written, ok := result.Content.(WriteFileResult)
	if !ok {
		t.Fatalf("content = %T, want a WriteFileResult", result.Content)
	}
	if written.Committed {
		t.Error("the tool told the model the write was committed")
	}
	if written.Bytes != len("# new operator\n") || written.Repository != "jonah/pv-forecast" {
		t.Errorf("result = %+v", written)
	}
	if written.Hint == "" {
		t.Error("the result carries no hint about what happens next")
	}
}

func TestWriteFileNeedsAPath(t *testing.T) {
	result := dispatchWriteFile(t, writeFileSurface(t, &fakeRepo{}), "  ", "content")
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want invalid input", result.Outcome)
	}
}

// A deployment without a GitHub app has no repo service, and the tool has to be
// declared-but-unavailable rather than registered and broken — the same
// degradation run_code does without a Hub.
func TestWriteFileStaysUnavailableWithoutARepoService(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, found := registry.Lookup("write_file")
	if !found {
		t.Fatal("write_file left the documented surface")
	}
	if definition.Implemented() {
		t.Error("write_file has an executor without a repo service behind it")
	}
	if definition.Unavailable == "" {
		t.Error("write_file does not say why it cannot be called")
	}
	for _, available := range registry.Available(L0) {
		if available.Name == "write_file" {
			t.Error("write_file was offered to a provider without a repo service")
		}
	}
}

// §5.8 denies "modifying evaluation criteria" with no tool at all, and a tool that
// can write every file of the repository would be one. The refusal is the tool's,
// not the service's — the developer's own routes write that file happily.
func TestWriteFileWillNotTouchTheEvaluationCriteria(t *testing.T) {
	fake := &fakeRepo{}
	registry := writeFileSurface(t, fake)

	for _, path := range []string{"evaluation.yaml", "./evaluation.yaml", "Evaluation.yaml"} {
		result := dispatchWriteFile(t, registry, path, "metric: accuracy\nthreshold: 0.99\n")
		if result.Outcome != OutcomeInvalidInput {
			t.Errorf("writing %q: outcome = %q, want a refusal", path, result.Outcome)
		}
	}
	if len(fake.writes) != 0 {
		t.Fatalf("the criteria file was written: %+v", fake.writes)
	}
}

// The service's error travels rather than being flattened, so a model that wrote
// into an unselected repository is told to ask the developer to select one.
func TestWriteFileForwardsTheServicesRefusal(t *testing.T) {
	fake := &fakeRepo{err: repo.ErrNoRepository}
	result := dispatchWriteFile(t, writeFileSurface(t, fake), "op.py", "x")
	if result.Outcome == OutcomeOK || !result.IsError {
		t.Fatalf("outcome = %q, want a failure the model can read", result.Outcome)
	}
	if !errors.Is(fake.err, repo.ErrNoRepository) {
		t.Fatal("the fake was not asked")
	}
}
