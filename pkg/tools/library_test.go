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
	"github.com/SENERGY-Platform/operator-development-environment/pkg/library"
)

// fakeLibrary records what the tools asked for. pkg/library's own containment
// and mapping behaviour (a path that escapes the package, the envelope
// decoding) is pkg/library's own tests; these are about the tools.
type fakeLibrary struct {
	listing    library.Listing
	listingErr error
	filesRefs  []kernel.Ref

	files     map[string]library.File
	readErr   error
	readCalls []struct {
		Ref      kernel.Ref
		Path     string
		MaxBytes int
	}
}

func (f *fakeLibrary) Files(_ context.Context, ref kernel.Ref) (library.Listing, error) {
	f.filesRefs = append(f.filesRefs, ref)
	if f.listingErr != nil {
		return library.Listing{}, f.listingErr
	}
	return f.listing, nil
}

func (f *fakeLibrary) ReadFile(
	_ context.Context, ref kernel.Ref, path string, maxBytes int,
) (library.File, error) {
	f.readCalls = append(f.readCalls, struct {
		Ref      kernel.Ref
		Path     string
		MaxBytes int
	}{ref, path, maxBytes})
	if f.readErr != nil {
		return library.File{}, f.readErr
	}
	file, found := f.files[path]
	if !found {
		return library.File{}, fmt.Errorf("%w: %s", library.ErrNotFound, path)
	}
	// Cut the way the pod's helper does (_read in pkg/library), so a tool that
	// passes too small a bound sees the same prefix it would in production.
	if maxBytes > 0 && len(file.Text) > maxBytes {
		file.Text = file.Text[:maxBytes]
		file.Truncated = true
	}
	return file, nil
}

func librarySurface(t *testing.T, fake *fakeLibrary, maxReadBytes int) *Registry {
	t.Helper()
	registry, err := NewSurface(Deps{Library: fake, RepoMaxReadBytes: maxReadBytes})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

// ---- declarations ----

func TestTheLibraryToolsAreAvailableAtL0WithoutAConfirmation(t *testing.T) {
	registry := librarySurface(t, &fakeLibrary{}, 0)
	for _, name := range []string{"list_lib_files", "read_lib_file"} {
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
		if definition.Unavailable != "" {
			t.Errorf("%s unavailable = %q, want it cleared once the executor is there",
				name, definition.Unavailable)
		}
	}
}

// A deployment without a Hub has no library service, and both tools have to be
// declared-but-unavailable rather than registered and broken — the same
// degradation run_code and the repository tools already have.
func TestTheLibraryToolsStayUnavailableWithoutALibraryService(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, name := range []string{"list_lib_files", "read_lib_file"} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%s left the documented surface", name)
		}
		if definition.Implemented() {
			t.Errorf("%s has an executor without a library service behind it", name)
		}
		if definition.Unavailable == "" {
			t.Errorf("%s does not say why it cannot be called", name)
		}
		for _, available := range registry.Available(L0) {
			if available.Name == name {
				t.Errorf("%s was offered to a provider without a library service", name)
			}
		}
	}
}

// ---- list_lib_files ----

func TestListLibFilesReturnsTheTreeAsListed(t *testing.T) {
	fake := &fakeLibrary{listing: library.Listing{
		Package: "operator_lib",
		Version: "1.7.0",
		Root:    "/opt/env/operator_lib",
		Files: []library.Entry{
			{Path: "__init__.py", Size: 12},
			{Path: "core.py", Size: 340},
		},
		Count: 2,
	}}
	result := dispatchTool(t, librarySurface(t, fake, 0), "list_lib_files", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	listed, ok := result.Content.(ListLibFilesResult)
	if !ok {
		t.Fatalf("content = %T, want a ListLibFilesResult", result.Content)
	}
	if listed.Package != "operator_lib" || listed.Version != "1.7.0" ||
		listed.Root != "/opt/env/operator_lib" {
		t.Errorf("listing = %+v", listed)
	}
	if len(listed.Files) != 2 || listed.Count != 2 || listed.Truncated {
		t.Errorf("files = %+v", listed)
	}

	// operator_lib is not platform data, so the ref carries no request for the
	// developer's platform token — the same rule runCode's own contained cells
	// follow, applied here unconditionally.
	if len(fake.filesRefs) != 1 || fake.filesRefs[0].WithPlatformToken {
		t.Errorf("ref = %+v, want WithPlatformToken false", fake.filesRefs)
	}
	if fake.filesRefs[0].Bearer != "Bearer developer-token" {
		t.Errorf("ref.Bearer = %q, want the developer's own token", fake.filesRefs[0].Bearer)
	}
}

// pkg/library's own truncation (its Count exceeds len(Files)) travels through
// unchanged.
func TestListLibFilesForwardsAnUpstreamTruncation(t *testing.T) {
	fake := &fakeLibrary{listing: library.Listing{
		Package: "operator_lib", Version: "1.7.0", Root: "/opt/env/operator_lib",
		Files: []library.Entry{{Path: "a.py", Size: 1}}, Count: 900, Truncated: true,
	}}
	result := dispatchTool(t, librarySurface(t, fake, 0), "list_lib_files", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	listed := result.Content.(ListLibFilesResult)
	if !listed.Truncated || listed.Count != 900 || listed.Hint == "" {
		t.Errorf("listing = %+v, want the upstream truncation forwarded with a hint", listed)
	}
}

// The tool applies its own cap on top of whatever pkg/library returned, the
// same cap list_files already uses — so a change to pkg/library's own
// MaxEntries cannot silently hand the model a longer listing than the
// repository tool would.
func TestListLibFilesCutsAnOversizedTreeToTheSameCapAsListFiles(t *testing.T) {
	files := make([]library.Entry, maxListedFiles+10)
	for i := range files {
		files[i] = library.Entry{Path: fmt.Sprintf("mod%03d.py", i), Size: 1}
	}
	fake := &fakeLibrary{listing: library.Listing{
		Package: "operator_lib", Version: "1.7.0", Root: "/opt/env/operator_lib",
		Files: files, Count: len(files), Truncated: false,
	}}
	result := dispatchTool(t, librarySurface(t, fake, 0), "list_lib_files", map[string]any{})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	listed := result.Content.(ListLibFilesResult)
	if !listed.Truncated {
		t.Error("truncated = false, want true once the tool's own cap applies")
	}
	if len(listed.Files) != maxListedFiles {
		t.Errorf("files = %d, want the cap of %d", len(listed.Files), maxListedFiles)
	}
}

// not_installed is a deployment characteristic discovered at call time, not a
// mistake in the model's arguments — list_lib_files takes none — so it is a
// plain failure rather than invalid input.
func TestListLibFilesRefusesWhenNotInstalled(t *testing.T) {
	fake := &fakeLibrary{listingErr: library.ErrNotInstalled}
	result := dispatchTool(t, librarySurface(t, fake, 0), "list_lib_files", map[string]any{})
	if result.Outcome != OutcomeFailed || !result.IsError {
		t.Fatalf("outcome = %q, want a failure the model can read", result.Outcome)
	}
	failure, _ := json.Marshal(result.Content)
	if !strings.Contains(string(failure), "no operator_lib installed") {
		t.Errorf("refusal = %s, want it to say the pod has no operator_lib", failure)
	}
}

// ---- read_lib_file ----

func TestReadLibFileReturnsTheFileAsItIs(t *testing.T) {
	const source = "def run():\n    return 42\n"
	fake := &fakeLibrary{files: map[string]library.File{
		"core.py": {
			Package: "operator_lib", Version: "1.7.0", Path: "core.py",
			Text: source, Size: int64(len(source)),
		},
	}}
	result := dispatchTool(t, librarySurface(t, fake, 4096), "read_lib_file",
		map[string]any{"path": "core.py"})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	read, ok := result.Content.(ReadLibFileResult)
	if !ok {
		t.Fatalf("content = %T, want a ReadLibFileResult", result.Content)
	}
	if read.Text != source || read.Version != "1.7.0" || read.Package != "operator_lib" {
		t.Errorf("read = %+v", read)
	}
	if read.TotalLines != 2 || read.Lines != 2 || read.FromLine != 1 {
		t.Errorf("window = %+v, want both lines from the first", read)
	}
	if read.Truncated {
		t.Error("a file that fits was reported as truncated")
	}

	if len(fake.readCalls) != 1 || fake.readCalls[0].MaxBytes != 0 {
		t.Errorf("read calls = %+v, want pkg/library's own ceiling for the raw read, "+
			"not the window budget", fake.readCalls)
	}
	if fake.readCalls[0].Ref.WithPlatformToken {
		t.Error("the library read asked for the developer's platform token")
	}
}

// Over the budget the answer is a window that names where to continue, on the
// same terms as read_file — windowLines is the same helper, called with the
// same shape of arguments.
func TestReadLibFileWindowsALongFileAndSaysWhereToContinue(t *testing.T) {
	var lines []string
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("line %02d ....", i))
	}
	source := strings.Join(lines, "\n") + "\n"
	fake := &fakeLibrary{files: map[string]library.File{
		"core.py": {Package: "operator_lib", Version: "1.7.0", Path: "core.py",
			Text: source, Size: int64(len(source))},
	}}
	registry := librarySurface(t, fake, 40)

	first := dispatchTool(t, registry, "read_lib_file", map[string]any{"path": "core.py"})
	read, ok := first.Content.(ReadLibFileResult)
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

	next := read.FromLine + read.Lines
	second := dispatchTool(t, registry, "read_lib_file",
		map[string]any{"path": "core.py", "from_line": next, "max_lines": 12})
	rest, ok := second.Content.(ReadLibFileResult)
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

// A file just over the window budget: the first answer stops one line short and
// the continuation must hand back that whole last line as the end of the file.
// With the raw read cut at the window budget, total_lines counted only the
// lines of that prefix and the continuation returned the same cut fragment.
func TestReadLibFileContinuationReachesTheRealLastLine(t *testing.T) {
	var lines []string
	for i := 1; i <= 4; i++ {
		lines = append(lines, fmt.Sprintf("line %02d ....", i))
	}
	source := strings.Join(lines, "\n") + "\n"
	fake := &fakeLibrary{files: map[string]library.File{
		"core.py": {Package: "operator_lib", Version: "1.7.0", Path: "core.py",
			Text: source, Size: int64(len(source))},
	}}
	registry := librarySurface(t, fake, 45)

	first := dispatchTool(t, registry, "read_lib_file", map[string]any{"path": "core.py"})
	read := first.Content.(ReadLibFileResult)
	if read.TotalLines != 4 || read.Lines != 3 || !read.Truncated {
		t.Fatalf("first window = %+v, want lines 1-3 of 4", read)
	}

	second := dispatchTool(t, registry, "read_lib_file",
		map[string]any{"path": "core.py", "from_line": read.FromLine + read.Lines})
	rest := second.Content.(ReadLibFileResult)
	if rest.Text != "line 04 ....\n" || rest.Truncated || rest.Hint != "" {
		t.Errorf("last window = %+v, want the whole fourth line and nothing left", rest)
	}
}

func TestReadLibFileReportsBinary(t *testing.T) {
	fake := &fakeLibrary{files: map[string]library.File{
		"weights.bin": {Package: "operator_lib", Version: "1.7.0", Path: "weights.bin",
			Size: 19, Binary: true},
	}}
	result := dispatchTool(t, librarySurface(t, fake, 4096), "read_lib_file",
		map[string]any{"path": "weights.bin"})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q: %+v", result.Outcome, result.Content)
	}
	read := result.Content.(ReadLibFileResult)
	if !read.Binary || read.Text != "" || read.Hint == "" {
		t.Errorf("read = %+v, want a binary file reported with no text", read)
	}
}

func TestReadLibFileNeedsAPath(t *testing.T) {
	result := dispatchTool(t, librarySurface(t, &fakeLibrary{}, 4096), "read_lib_file",
		map[string]any{"path": "   "})
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want invalid input", result.Outcome)
	}
}

func TestReadLibFileRefusesALineBeyondTheEnd(t *testing.T) {
	fake := &fakeLibrary{files: map[string]library.File{
		"core.py": {Path: "core.py", Text: "one\ntwo\n"},
	}}
	result := dispatchTool(t, librarySurface(t, fake, 4096), "read_lib_file",
		map[string]any{"path": "core.py", "from_line": 9})
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want invalid input", result.Outcome)
	}
}

// not_installed is a deployment characteristic, the same as for
// list_lib_files: a plain failure, not invalid input.
func TestReadLibFileRefusesWhenNotInstalled(t *testing.T) {
	fake := &fakeLibrary{readErr: library.ErrNotInstalled}
	result := dispatchTool(t, librarySurface(t, fake, 4096), "read_lib_file",
		map[string]any{"path": "core.py"})
	if result.Outcome != OutcomeFailed || !result.IsError {
		t.Fatalf("outcome = %q, want a failure the model can read", result.Outcome)
	}
	failure, _ := json.Marshal(result.Content)
	if !strings.Contains(string(failure), "no operator_lib installed") {
		t.Errorf("refusal = %s, want it to say the pod has no operator_lib", failure)
	}
}

// A path that resolves outside operator_lib is the model's own mistake —
// invalid input — and the refusal names the path so the model can see what it
// sent.
func TestReadLibFileRefusesAPathOutsideThePackage(t *testing.T) {
	fake := &fakeLibrary{readErr: fmt.Errorf("%w: %s", library.ErrOutsidePackage, "../secret.txt")}
	result := dispatchTool(t, librarySurface(t, fake, 4096), "read_lib_file",
		map[string]any{"path": "../secret.txt"})
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want invalid input: the model sent a path outside the package",
			result.Outcome)
	}
	failure, _ := json.Marshal(result.Content)
	if !strings.Contains(string(failure), "../secret.txt") {
		t.Errorf("refusal = %s, want it to name the path that was refused", failure)
	}
}

// A path Files would not have listed is refused pointing back at
// list_lib_files, not at pkg/library's own Go-oriented "call Files" wording.
func TestReadLibFileRefusesAMissingFileAndPointsToTheListing(t *testing.T) {
	fake := &fakeLibrary{readErr: fmt.Errorf("%w: %s", library.ErrNotFound, "nope.py")}
	result := dispatchTool(t, librarySurface(t, fake, 4096), "read_lib_file",
		map[string]any{"path": "nope.py"})
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("outcome = %q, want invalid input", result.Outcome)
	}
	failure, _ := json.Marshal(result.Content)
	if !strings.Contains(string(failure), "list_lib_files") {
		t.Errorf("refusal = %s, want it to point at list_lib_files", failure)
	}
}
