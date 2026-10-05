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
	"fmt"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// fakeRepo records what the tools asked for. The service's own refusals — a path
// that leaves the repository, a write into .git — are pkg/repo's tests; these are
// about the tools.
type fakeRepo struct {
	writes []struct {
		Request repo.Request
		Path    string
		Content string
	}
	err error

	// tree is what Files answers, and files what ReadFile answers, by the path the
	// tool asked for. reads records the paths that reached the service, so a refusal
	// the tool made itself can be told from one it forwarded.
	tree  repo.FileTree
	files map[string]repo.File
	reads []string

	// status is what Status answers, and statusErr its error — kept separate from
	// err above so a test can make Status succeed while Log fails (or the reverse)
	// without the two tangling.
	status     repo.Status
	statusErr  error
	statusReqs []repo.StatusRequest

	// commits is what Log answers, and logErr its error.
	commits  []repo.Commit
	logErr   error
	logReqs  []repo.Request
	logLimit []int
}

func (f *fakeRepo) Status(_ context.Context, req repo.StatusRequest) (repo.Status, error) {
	f.statusReqs = append(f.statusReqs, req)
	if f.statusErr != nil {
		return repo.Status{}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeRepo) Log(_ context.Context, req repo.Request, limit int) ([]repo.Commit, error) {
	f.logReqs = append(f.logReqs, req)
	f.logLimit = append(f.logLimit, limit)
	if f.logErr != nil {
		return nil, f.logErr
	}
	return f.commits, nil
}

func (f *fakeRepo) Files(_ context.Context, _ repo.Request) (repo.FileTree, error) {
	if f.err != nil {
		return repo.FileTree{}, f.err
	}
	return f.tree, nil
}

func (f *fakeRepo) ReadFile(
	_ context.Context, _ repo.Request, path string,
) (repo.File, error) {
	f.reads = append(f.reads, path)
	if f.err != nil {
		return repo.File{}, f.err
	}
	file, found := f.files[path]
	if !found {
		return repo.File{}, fmt.Errorf("%w: %s is not in the working copy",
			repo.ErrInvalidRequest, path)
	}
	return file, nil
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

// ---- list_files and read_file ----

func readSurface(t *testing.T, fake *fakeRepo) *Registry {
	t.Helper()
	// A small budget, so a window and its continuation can be asserted without a
	// test fixture of eight thousand bytes.
	registry, err := NewSurface(Deps{Repo: fake, RepoMaxReadBytes: 40})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

func dispatchTool(t *testing.T, registry *Registry, name string, input map[string]any) Result {
	t.Helper()
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer developer-token", UserSub: "user-1", Tier: L0},
		Call{ID: "call-1", Name: name, Input: encoded})
}

// Both read tools sit at L0 with no confirmation, and that is the whole point of
// them: reading the working copy is what run_code was being confirmed for.
func TestTheReadToolsAreAvailableAtL0WithoutAConfirmation(t *testing.T) {
	registry := readSurface(t, &fakeRepo{})
	for _, name := range []string{"list_files", "read_file"} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%s is not in the registry", name)
		}
		if definition.MinTier != L0 || definition.Confirm {
			t.Errorf("%s = tier %s confirm %v, want L0 without confirmation",
				name, definition.MinTier, definition.Confirm)
		}
		if !definition.Implemented() {
			t.Errorf("%s has no executor", name)
		}
	}
}

// The paths a listing reports are the paths the other two tools take. The walk
// carries workspace-relative ones — the checkout directory is in them — and a
// model handed those would send one back to read_file, which takes a path relative
// to the repository root.
func TestListFilesReportsRepositoryRelativePaths(t *testing.T) {
	fake := &fakeRepo{tree: repo.FileTree{
		Root:     "franzmueller/operator-test",
		Excluded: []string{".git"},
		Tree: kernel.Node{
			Name: "operator-test", Path: "franzmueller/operator-test", Type: "directory",
			Children: []kernel.Node{
				{Name: "op.py", Path: "franzmueller/operator-test/op.py", Type: "file", Size: 812},
				{Name: "tests", Path: "franzmueller/operator-test/tests", Type: "directory",
					Children: []kernel.Node{{
						Name: "test_op.py",
						Path: "franzmueller/operator-test/tests/test_op.py",
						Type: "file", Size: 300,
					}}},
			},
		},
	}}

	result := dispatchTool(t, readSurface(t, fake), "list_files", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	listed, ok := result.Content.(ListFilesResult)
	if !ok {
		t.Fatalf("content = %T, want a ListFilesResult", result.Content)
	}
	if listed.Count != 2 {
		t.Fatalf("files = %+v, want the two files and neither directory", listed.Files)
	}
	want := map[string]int64{"op.py": 812, "tests/test_op.py": 300}
	for _, file := range listed.Files {
		size, expected := want[file.Path]
		if !expected {
			t.Errorf("listed %q, which is not repository-relative", file.Path)
			continue
		}
		if file.Size != size {
			t.Errorf("%s size = %d, want %d", file.Path, file.Size, size)
		}
	}
	if listed.Repository != "franzmueller/operator-test" {
		t.Errorf("repository = %q, want the checkout the paths belong to", listed.Repository)
	}
}

// A walk that stopped early must not read as a complete repository, because a
// model that thinks it has seen every file proposes changes as if it had.
func TestListFilesSaysWhenTheTreeIsIncomplete(t *testing.T) {
	fake := &fakeRepo{tree: repo.FileTree{
		Root: "owner/repo",
		Tree: kernel.Node{
			Name: "repo", Path: "owner/repo", Type: "directory", Elided: 12,
			Children: []kernel.Node{
				{Name: "op.py", Path: "owner/repo/op.py", Type: "file", Size: 1},
			},
		},
	}}
	result := dispatchTool(t, readSurface(t, fake), "list_files", map[string]any{})
	listed, ok := result.Content.(ListFilesResult)
	if !ok {
		t.Fatalf("content = %T", result.Content)
	}
	if !listed.Truncated || listed.Hint == "" {
		t.Errorf("an elided walk was reported as complete: %+v", listed)
	}
}

// A full read has to be byte-identical to the file, because the model edits what
// it read and sends the whole thing back to write_file.
func TestReadFileReturnsTheFileAsItIs(t *testing.T) {
	const source = "import op\n\ndef train():\n    return 1\n"
	fake := &fakeRepo{files: map[string]repo.File{
		"op.py": {Path: "op.py", Text: source, Size: int64(len(source)), Language: "python"},
	}}

	result := dispatchTool(t, readSurface(t, fake), "read_file",
		map[string]any{"path": "op.py"})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	read, ok := result.Content.(ReadFileResult)
	if !ok {
		t.Fatalf("content = %T, want a ReadFileResult", result.Content)
	}
	if read.Text != source {
		t.Errorf("text = %q, want the file unchanged", read.Text)
	}
	if read.TotalLines != 4 || read.Lines != 4 || read.FromLine != 1 {
		t.Errorf("window = %+v, want all four lines from the first", read)
	}
	if read.Truncated {
		t.Error("a file that fits was reported as truncated")
	}
}

// Over the budget the answer is a window that names where to continue, and the
// window ends on a line boundary: half a statement that looks like the file's own
// text is worse than a shorter answer.
func TestReadFileWindowsALongFileAndSaysWhereToContinue(t *testing.T) {
	lines := []string{}
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("line %02d ....", i))
	}
	source := strings.Join(lines, "\n") + "\n"
	fake := &fakeRepo{files: map[string]repo.File{
		"long.py": {Path: "long.py", Text: source, Size: int64(len(source))},
	}}
	registry := readSurface(t, fake)

	first := dispatchTool(t, registry, "read_file", map[string]any{"path": "long.py"})
	read, ok := first.Content.(ReadFileResult)
	if !ok {
		t.Fatalf("content = %T", first.Content)
	}
	if !read.Truncated || read.Hint == "" {
		t.Fatalf("a windowed read did not say so: %+v", read)
	}
	if read.TotalLines != 12 {
		t.Errorf("total_lines = %d, want 12 whatever the window was", read.TotalLines)
	}
	if read.Lines == 0 || read.Lines >= 12 {
		t.Fatalf("lines = %d, want a window shorter than the file", read.Lines)
	}
	for _, line := range strings.Split(strings.TrimSuffix(read.Text, "\n"), "\n") {
		if !strings.HasPrefix(line, "line ") || len(line) != len("line 01 ....") {
			t.Errorf("window holds a cut line: %q", line)
		}
	}

	// Continuing where it said to, the rest arrives and the last window carries the
	// file's own trailing newline.
	next := read.FromLine + read.Lines
	second := dispatchTool(t, registry, "read_file",
		map[string]any{"path": "long.py", "from_line": next, "max_lines": 12})
	rest, ok := second.Content.(ReadFileResult)
	if !ok {
		t.Fatalf("content = %T", second.Content)
	}
	if rest.FromLine != next {
		t.Errorf("from_line = %d, want %d", rest.FromLine, next)
	}
	if !strings.HasPrefix(rest.Text, fmt.Sprintf("line %02d", next)) {
		t.Errorf("the second window does not start at line %d: %q", next, rest.Text)
	}
}

// An empty file is a file. `__init__.py` is empty in most Python packages and the
// scaffold writes one, so this must not answer with the past-the-end refusal —
// which a model would read as "the file is not there".
func TestReadFileAnswersForAnEmptyFile(t *testing.T) {
	fake := &fakeRepo{files: map[string]repo.File{
		"__init__.py": {Path: "__init__.py", Text: "", Size: 0},
	}}
	result := dispatchTool(t, readSurface(t, fake), "read_file",
		map[string]any{"path": "__init__.py"})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	read, ok := result.Content.(ReadFileResult)
	if !ok {
		t.Fatalf("content = %T", result.Content)
	}
	if read.Text != "" || read.TotalLines != 0 || read.Truncated {
		t.Errorf("result = %+v, want an empty file reported as empty", read)
	}
	if read.Hint == "" {
		t.Error("an empty answer with no hint reads as a failed read")
	}
}

// Past the end is a mistake, and an empty window would read to a model as an empty
// file. Only one of those is true.
func TestReadFileRefusesALineBeyondTheEnd(t *testing.T) {
	fake := &fakeRepo{files: map[string]repo.File{
		"op.py": {Path: "op.py", Text: "one\ntwo\n"},
	}}
	result := dispatchTool(t, readSurface(t, fake), "read_file",
		map[string]any{"path": "op.py", "from_line": 9})
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want invalid input", result.Outcome)
	}
}

/*
A credential is refused by the tool, before the service is asked.

read_file answers into a conversation that is stored, and it asks nobody first, so
a repository's own `.env` is the one file this tool must not hand over. The list is
pkg/plaincode's, shared rather than copied — it is the same decision about the same
names, and two copies of it would drift apart.

That the service is never called is the assertion that matters: a check on the way
out would already have the contents in memory.
*/
func TestReadFileRefusesACredentialPathWithoutAsking(t *testing.T) {
	fake := &fakeRepo{files: map[string]repo.File{}}
	registry := readSurface(t, fake)

	for _, path := range []string{
		".env", "config/.env", ".ssh/id_rsa", "deploy/credentials",
		"/var/run/secrets/kubernetes.io/serviceaccount/token",
	} {
		result := dispatchTool(t, registry, "read_file", map[string]any{"path": path})
		if result.Outcome != OutcomeInvalidInput {
			t.Errorf("reading %q: outcome = %q, want a refusal", path, result.Outcome)
		}
	}
	if len(fake.reads) != 0 {
		t.Errorf("the service was asked for a credential path: %v", fake.reads)
	}

	// A file whose name merely resembles one is ordinary and is read.
	fake.files["tokenizer.py"] = repo.File{Path: "tokenizer.py", Text: "x = 1\n"}
	if result := dispatchTool(t, registry, "read_file",
		map[string]any{"path": "tokenizer.py"}); result.Outcome != OutcomeOK {
		t.Errorf("tokenizer.py: outcome = %q, want it read", result.Outcome)
	}
}

// A listing hides nothing, and that is the opposite decision from the one above,
// on purpose: a name is not a credential, and a model shown an edited tree would
// propose changes to a repository that does not exist.
func TestListFilesHidesNothing(t *testing.T) {
	fake := &fakeRepo{tree: repo.FileTree{
		Root: "owner/repo",
		Tree: kernel.Node{
			Name: "repo", Path: "owner/repo", Type: "directory",
			Children: []kernel.Node{
				{Name: ".env", Path: "owner/repo/.env", Type: "file", Size: 40},
				{Name: "op.py", Path: "owner/repo/op.py", Type: "file", Size: 10},
			},
		},
	}}
	result := dispatchTool(t, readSurface(t, fake), "list_files", map[string]any{})
	listed, ok := result.Content.(ListFilesResult)
	if !ok {
		t.Fatalf("content = %T", result.Content)
	}
	found := false
	for _, file := range listed.Files {
		if file.Path == ".env" {
			found = true
		}
	}
	if !found {
		t.Errorf("the listing hid a file: %+v", listed.Files)
	}
}

// Without a repo service both tools are declared-but-unavailable, the same
// degradation write_file and run_code have.
func TestTheReadToolsStayUnavailableWithoutARepoService(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, name := range []string{"list_files", "read_file"} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%s left the surface", name)
		}
		if definition.Implemented() {
			t.Errorf("%s has an executor without a repo service behind it", name)
		}
		if definition.Unavailable == "" {
			t.Errorf("%s does not say why it cannot be called", name)
		}
		for _, available := range registry.Available(L0) {
			if available.Name == name {
				t.Errorf("%s was offered to a provider without a repo service", name)
			}
		}
	}
}

// The service's error travels rather than being flattened, so a model that read
// from an unselected repository is told to ask the developer to select one.
func TestReadToolsForwardTheServicesRefusal(t *testing.T) {
	for _, name := range []string{"list_files", "read_file"} {
		fake := &fakeRepo{err: repo.ErrNoRepository}
		result := dispatchTool(t, readSurface(t, fake), name,
			map[string]any{"path": "op.py"})
		if result.Outcome == OutcomeOK || !result.IsError {
			t.Errorf("%s: outcome = %q, want a failure the model can read",
				name, result.Outcome)
		}
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

// ---- git_status ----

func gitStatusSurface(t *testing.T, fake *fakeRepo) *Registry {
	t.Helper()
	registry, err := NewSurface(Deps{Repo: fake})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

func TestGitStatusIsAvailableAtL0WithoutAConfirmation(t *testing.T) {
	registry := gitStatusSurface(t, &fakeRepo{})
	definition, found := registry.Lookup("git_status")
	if !found {
		t.Fatal("git_status is not in the registry")
	}
	if definition.MinTier != L0 || definition.Confirm {
		t.Errorf("git_status = tier %s confirm %v, want L0 without confirmation",
			definition.MinTier, definition.Confirm)
	}
	if !definition.Implemented() {
		t.Fatal("git_status has no executor")
	}
	if definition.Unavailable != "" {
		t.Errorf("unavailable = %q, want it cleared once the executor is there",
			definition.Unavailable)
	}
}

// Without a repo service git_status is declared-but-unavailable, the same
// degradation the other repository tools have.
func TestGitStatusStaysUnavailableWithoutARepoService(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, found := registry.Lookup("git_status")
	if !found {
		t.Fatal("git_status left the documented surface")
	}
	if definition.Implemented() {
		t.Error("git_status has an executor without a repo service behind it")
	}
	if definition.Unavailable == "" {
		t.Error("git_status does not say why it cannot be called")
	}
	for _, available := range registry.Available(L0) {
		if available.Name == "git_status" {
			t.Error("git_status was offered to a provider without a repo service")
		}
	}
}

// The normal case: a cloned, dirty working copy with a few commits behind it.
func TestGitStatusReportsTheWorkingCopy(t *testing.T) {
	fake := &fakeRepo{
		status: repo.Status{
			Cloned:      true,
			Branch:      "main",
			Upstream:    "origin/main",
			Ahead:       2,
			Behind:      1,
			Head:        "abc123",
			HeadSubject: "fix: something",
			HeadDate:    "2026-09-20T10:00:00Z",
			Remote:      "https://github.com/jonah/pv-forecast.git",
			Dirty:       true,
			Changes: []repo.Change{
				{Path: "op.py", Kind: "modified", Unstaged: true},
			},
			Fetched: false,
			Scaffold: repo.ScaffoldState{
				Present: []string{"op.py"}, Missing: []string{"evaluation.yaml"}, Complete: false,
			},
		},
		commits: []repo.Commit{
			{SHA: "abc123", Subject: "fix: something", Date: "2026-09-20T10:00:00Z", Author: "jonah"},
		},
	}
	result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	status, ok := result.Content.(GitStatusResult)
	if !ok {
		t.Fatalf("content = %T, want a GitStatusResult", result.Content)
	}
	if !status.Cloned || status.Branch != "main" || status.Upstream != "origin/main" {
		t.Errorf("status = %+v", status)
	}
	if status.Ahead != 2 || status.Behind != 1 {
		t.Errorf("ahead/behind = %d/%d, want 2/1", status.Ahead, status.Behind)
	}
	if status.Head != "abc123" || status.HeadSubject != "fix: something" {
		t.Errorf("head = %+v", status)
	}
	// A remote with no credential in it passes through unchanged, so the
	// redaction below is not mistaken for a general rewrite of the field.
	if status.Remote != "https://github.com/jonah/pv-forecast.git" {
		t.Errorf("remote = %q, want the origin unchanged", status.Remote)
	}
	if status.Fetched {
		t.Error("fetched = true, want false: git_status must never fetch")
	}
	if len(status.Changes) != 1 || status.ChangesCount != 1 || status.Changes[0].Path != "op.py" {
		t.Errorf("changes = %+v, want the one change", status.Changes)
	}
	if status.Scaffold.Complete || len(status.Scaffold.Missing) != 1 {
		t.Errorf("scaffold = %+v, want the incomplete scaffold forwarded", status.Scaffold)
	}
	if len(status.Commits) != 1 || status.Commits[0].SHA != "abc123" {
		t.Errorf("commits = %+v, want the one commit", status.Commits)
	}

	// Fetch: false has to reach the service on every call — that is the one
	// network side effect this tool must never introduce.
	if len(fake.statusReqs) != 1 || fake.statusReqs[0].Fetch {
		t.Errorf("status request = %+v, want Fetch: false", fake.statusReqs)
	}
	if len(fake.logLimit) != 1 || fake.logLimit[0] != gitStatusLogLimit {
		t.Errorf("log limit = %v, want [%d]", fake.logLimit, gitStatusLogLimit)
	}
}

func TestGitStatusReportsAnUnclonedWorkingCopy(t *testing.T) {
	fake := &fakeRepo{status: repo.Status{Cloned: false}}
	result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	status := result.Content.(GitStatusResult)
	if status.Cloned {
		t.Error("cloned = true, want false for a PVC with no checkout")
	}
}

// A freshly scaffolded repository is unborn: a branch with no commits yet.
func TestGitStatusReportsAnUnbornBranch(t *testing.T) {
	fake := &fakeRepo{status: repo.Status{Cloned: true, Branch: "main", Unborn: true}}
	result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	status := result.Content.(GitStatusResult)
	if !status.Unborn {
		t.Error("unborn = false, want true")
	}
	if len(status.Commits) != 0 {
		t.Errorf("commits = %+v, want none for a branch with nothing on it yet", status.Commits)
	}
}

// A working copy with more changes than the cap must say so rather than
// silently showing a partial list as if it were complete.
func TestGitStatusTruncatesALongChangeList(t *testing.T) {
	changes := make([]repo.Change, maxGitStatusChanges+37)
	for i := range changes {
		changes[i] = repo.Change{Path: fmt.Sprintf("file%03d.py", i), Kind: "modified", Unstaged: true}
	}
	fake := &fakeRepo{status: repo.Status{Cloned: true, Branch: "main", Changes: changes}}
	result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	status := result.Content.(GitStatusResult)
	if !status.ChangesTruncated {
		t.Error("changes_truncated = false, want true past the cap")
	}
	if len(status.Changes) != maxGitStatusChanges {
		t.Errorf("changes = %d, want the cap of %d", len(status.Changes), maxGitStatusChanges)
	}
	if status.ChangesCount != len(changes) {
		t.Errorf("changes_count = %d, want %d", status.ChangesCount, len(changes))
	}
	if status.Hint == "" {
		t.Error("a truncated change list carries no hint")
	}
}

// A log that fails after the status already succeeded must not cost the rest
// of the answer.
func TestGitStatusReportsWhenTheLogFailsButStatusSucceeds(t *testing.T) {
	fake := &fakeRepo{
		status: repo.Status{Cloned: true, Branch: "main"},
		logErr: kernel.ErrBusy,
	}
	result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v, want the status half to still answer",
			result.Outcome, result.Content)
	}
	status := result.Content.(GitStatusResult)
	if status.Branch != "main" {
		t.Errorf("branch = %q, want the status half unaffected by the log failure", status.Branch)
	}
	if len(status.Commits) != 0 {
		t.Errorf("commits = %+v, want none when the log failed", status.Commits)
	}
	if status.Hint == "" {
		t.Error("a failed log carries no hint explaining why there are no commits")
	}
}

// A status that fails outright is a failure — unlike the log, there is no
// good half of the answer to salvage.
func TestGitStatusForwardsAFailedStatus(t *testing.T) {
	fake := &fakeRepo{statusErr: repo.ErrNoRepository}
	result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
	if result.Outcome == OutcomeOK || !result.IsError {
		t.Fatalf("outcome = %q, want a failure the model can read", result.Outcome)
	}
	if len(fake.logReqs) != 0 {
		t.Error("Log was called after Status had already failed")
	}
}

// Finding 2. A remote a developer set up outside ODE can carry a credential in
// its userinfo — redact (pkg/repo/git.go) never sees it, because that only
// strips ODE's own token — and this tool's answer joins a stored conversation
// nobody confirms first. The credential must not survive into the result.
func TestGitStatusStripsACredentialFromTheRemoteURL(t *testing.T) {
	fake := &fakeRepo{status: repo.Status{
		Cloned: true, Branch: "main",
		Remote: "https://x-access-token:ghp_secrettoken@github.com/jonah/pv-forecast.git",
	}}
	result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	status := result.Content.(GitStatusResult)
	if strings.Contains(status.Remote, "ghp_secrettoken") || strings.Contains(status.Remote, "x-access-token") {
		t.Fatalf("remote = %q, still carries the credential", status.Remote)
	}
	if status.Remote != "https://github.com/jonah/pv-forecast.git" {
		t.Errorf("remote = %q, want the origin with only the credential removed", status.Remote)
	}

	// The same check on the raw JSON the model actually receives, in case a
	// field the struct assertion above does not cover carried the value too.
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "ghp_secrettoken") {
		t.Fatalf("encoded result = %s, still carries the credential", encoded)
	}
}

// git's scp-like syntax ("user@host:path") is not a URL and net/url refuses it,
// but it is the ordinary shape of an ssh remote and its user name is a login
// rather than a credential — so it survives. A remote that is neither a URL nor
// that shape is dropped rather than passed through half-filtered, and so is an
// scp remote whose userinfo carries a password: losing the origin is a smaller
// cost than risking a credential this tool failed to recognise as one.
func TestGitStatusKeepsAnSSHRemoteAndDropsWhatItCannotRead(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		remote string
		want   string
	}{
		{"scp syntax", "git@github.com:jonah/pv-forecast.git", "git@github.com:jonah/pv-forecast.git"},
		{"scp syntax with a password", "git:ghp_secrettoken@github.com:jonah/pv-forecast.git", ""},
		{"neither a URL nor scp syntax", "http://%%zz/not a url", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeRepo{status: repo.Status{
				Cloned: true, Branch: "main", Remote: testCase.remote,
			}}
			result := dispatchTool(t, gitStatusSurface(t, fake), "git_status", map[string]any{})
			if result.Outcome != OutcomeOK {
				t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
			}
			status := result.Content.(GitStatusResult)
			if status.Remote != testCase.want {
				t.Errorf("remote = %q, want %q", status.Remote, testCase.want)
			}
		})
	}
}
