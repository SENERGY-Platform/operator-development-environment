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

package library_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/library"
)

// fakeWorkspace answers Command with one prepared result (or error), and records
// the last call so a test can inspect exactly what crossed into Argv — which is
// the only way to check, from outside the package, that a requested path never
// ends up inside the program text.
type fakeWorkspace struct {
	result kernel.CommandResult
	err    error
	last   kernel.Command
	calls  int
}

func (f *fakeWorkspace) Command(
	_ context.Context, _ kernel.Ref, cmd kernel.Command,
) (kernel.CommandResult, error) {
	f.last = cmd
	f.calls++
	return f.result, f.err
}

func ok(stdout string) kernel.CommandResult {
	return kernel.CommandResult{ExitCode: 0, Stdout: stdout}
}

var testRef = kernel.Ref{Bearer: "token", Workbench: "wb-1"}

func TestFilesReturnsTheTreeAsListed(t *testing.T) {
	ws := &fakeWorkspace{result: ok(
		`{"package":"operator_lib","version":"1.7.0","root":"/opt/env/operator_lib",` +
			`"files":[{"path":"__init__.py","size":12},{"path":"core.py","size":340}],` +
			`"count":2,"truncated":false}`,
	)}
	svc := library.New(ws, library.Options{})

	listing, err := svc.Files(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := library.Listing{
		Package: "operator_lib",
		Version: "1.7.0",
		Root:    "/opt/env/operator_lib",
		Files: []library.Entry{
			{Path: "__init__.py", Size: 12},
			{Path: "core.py", Size: 340},
		},
		Count:     2,
		Truncated: false,
	}
	if listing.Package != want.Package || listing.Version != want.Version ||
		listing.Root != want.Root || listing.Count != want.Count || listing.Truncated != want.Truncated {
		t.Fatalf("listing = %+v, want %+v", listing, want)
	}
	if len(listing.Files) != 2 || listing.Files[0] != want.Files[0] || listing.Files[1] != want.Files[1] {
		t.Fatalf("listing.Files = %+v, want %+v", listing.Files, want.Files)
	}

	// The subcommand and the configured budget travel as their own argv elements,
	// and the requested MaxEntries default (400) is the one actually sent.
	if got := ws.last.Argv; len(got) != 5 || got[3] != "tree" || got[4] != "400" {
		t.Fatalf("argv = %v, want [... tree 400]", got)
	}
}

func TestFilesReportsATruncatedTree(t *testing.T) {
	ws := &fakeWorkspace{result: ok(
		`{"package":"operator_lib","version":"","root":"/opt/env/operator_lib",` +
			`"files":[{"path":"a.py","size":1}],"count":900,"truncated":true}`,
	)}
	svc := library.New(ws, library.Options{MaxEntries: 1})

	listing, err := svc.Files(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if !listing.Truncated || listing.Count != 900 || len(listing.Files) != 1 {
		t.Fatalf("listing = %+v, want truncated with count 900 and one file returned", listing)
	}
	if got := ws.last.Argv; got[len(got)-1] != "1" {
		t.Fatalf("argv = %v, want the configured MaxEntries (1) as the last element", got)
	}
}

func TestReadFileReturnsTextAndCarriesTheVersion(t *testing.T) {
	ws := &fakeWorkspace{result: ok(
		`{"package":"operator_lib","version":"1.7.0","path":"core.py","size":25,` +
			`"text":"def run():\n    return 42\n","binary":false,"truncated":false}`,
	)}
	svc := library.New(ws, library.Options{})

	file, err := svc.ReadFile(context.Background(), testRef, "core.py", 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if file.Package != "operator_lib" || file.Version != "1.7.0" || file.Path != "core.py" ||
		file.Size != 25 || file.Binary || file.Truncated ||
		!strings.Contains(file.Text, "return 42") {
		t.Fatalf("file = %+v", file)
	}
}

func TestReadFileArgvCarriesThePathNotTheProgramText(t *testing.T) {
	ws := &fakeWorkspace{result: ok(
		`{"package":"operator_lib","version":"","path":"a/b.py","size":1,` +
			`"text":"x","binary":false,"truncated":false}`,
	)}
	svc := library.New(ws, library.Options{})

	// A path deliberately shaped like an escape attempt: if it ever reached the
	// program text instead of staying a separate argv element, this would either
	// break the Python source or (worse) get accepted as code.
	const suspicious = `../../etc/passwd"; import os; os.system("echo pwned`
	if _, err := svc.ReadFile(context.Background(), testRef, suspicious, 100); err != nil {
		// The fake answers unconditionally; a decode error here would mean the
		// test fixture itself is wrong, not the code under test.
		t.Fatalf("ReadFile: %v", err)
	}

	argv := ws.last.Argv
	if len(argv) == 0 {
		t.Fatal("Command was never called")
	}
	program := argv[2]
	if strings.Contains(program, suspicious) {
		t.Fatalf("the requested path was interpolated into the program text: %q", program)
	}
	found := false
	for _, element := range argv {
		if element == suspicious {
			found = true
		}
	}
	if !found {
		t.Fatalf("argv = %v, want the path as its own element", argv)
	}
}

func TestReadFileMapsOutsidePackage(t *testing.T) {
	ws := &fakeWorkspace{result: ok(`{"error":"outside_package","path":"../secret.txt"}`)}
	svc := library.New(ws, library.Options{})

	_, err := svc.ReadFile(context.Background(), testRef, "../secret.txt", 100)
	if !errors.Is(err, library.ErrOutsidePackage) {
		t.Fatalf("err = %v, want ErrOutsidePackage", err)
	}
}

func TestReadFileMapsNotFound(t *testing.T) {
	ws := &fakeWorkspace{result: ok(`{"error":"not_found","path":"nope.py"}`)}
	svc := library.New(ws, library.Options{})

	_, err := svc.ReadFile(context.Background(), testRef, "nope.py", 100)
	if !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFilesMapsNotInstalled(t *testing.T) {
	ws := &fakeWorkspace{result: ok(`{"error":"not_installed"}`)}
	svc := library.New(ws, library.Options{})

	_, err := svc.Files(context.Background(), testRef)
	if !errors.Is(err, library.ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
}

func TestReadFileReportsBinary(t *testing.T) {
	ws := &fakeWorkspace{result: ok(
		`{"package":"operator_lib","version":"","path":"blob.bin","size":19,` +
			`"text":"","binary":true,"truncated":false}`,
	)}
	svc := library.New(ws, library.Options{})

	file, err := svc.ReadFile(context.Background(), testRef, "blob.bin", 100)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !file.Binary || file.Text != "" {
		t.Fatalf("file = %+v, want binary with no text", file)
	}
}

func TestReadFileReportsATruncatedRead(t *testing.T) {
	ws := &fakeWorkspace{result: ok(
		`{"package":"operator_lib","version":"","path":"core.py","size":25,` +
			`"text":"def r","binary":false,"truncated":true}`,
	)}
	svc := library.New(ws, library.Options{})

	file, err := svc.ReadFile(context.Background(), testRef, "core.py", 5)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !file.Truncated || file.Text != "def r" || file.Size != 25 {
		t.Fatalf("file = %+v, want a truncated read that still reports the full size", file)
	}
	if got := ws.last.Argv; got[len(got)-1] != strconv.Itoa(5) {
		t.Fatalf("argv = %v, want the requested maxBytes (5) as the last element", got)
	}
}

func TestReadFileRefusesAnEmptyPathWithoutCallingTheWorkspace(t *testing.T) {
	ws := &fakeWorkspace{result: ok(`{"error":"not_found"}`)}
	svc := library.New(ws, library.Options{})

	_, err := svc.ReadFile(context.Background(), testRef, "", 100)
	if !errors.Is(err, library.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if ws.calls != 0 {
		t.Fatalf("the workspace was called %d times, want zero for an empty path", ws.calls)
	}
}

func TestRunReportsANonZeroExitAsItsOwnError(t *testing.T) {
	ws := &fakeWorkspace{result: kernel.CommandResult{
		ExitCode: 1,
		Stderr:   "Traceback (most recent call last):\n  ...\nPermissionError: [Errno 13] denied",
	}}
	svc := library.New(ws, library.Options{})

	_, err := svc.Files(context.Background(), testRef)
	if err == nil {
		t.Fatal("Files: want an error on a non-zero exit")
	}
	for _, sentinel := range []error{
		library.ErrNotInstalled, library.ErrNotFound, library.ErrOutsidePackage, library.ErrInvalidRequest,
	} {
		if errors.Is(err, sentinel) {
			t.Fatalf("err = %v, matched %v — a crashed helper must not look like a named condition", err, sentinel)
		}
	}
	if !strings.Contains(err.Error(), "PermissionError") {
		t.Fatalf("err = %v, want it to quote stderr", err)
	}
}

func TestRunReportsUnparsableStdoutAsItsOwnError(t *testing.T) {
	ws := &fakeWorkspace{result: ok("not json at all")}
	svc := library.New(ws, library.Options{})

	_, err := svc.Files(context.Background(), testRef)
	if err == nil {
		t.Fatal("Files: want an error when the helper's stdout does not parse")
	}
}

func TestFilesPassesThroughABusyKernelUntouched(t *testing.T) {
	ws := &fakeWorkspace{err: kernel.ErrBusy}
	svc := library.New(ws, library.Options{})

	_, err := svc.Files(context.Background(), testRef)
	if !errors.Is(err, kernel.ErrBusy) {
		t.Fatalf("err = %v, want kernel.ErrBusy to still be visible via errors.Is", err)
	}
}

func TestNewFillsInDefaults(t *testing.T) {
	ws := &fakeWorkspace{result: ok(
		`{"package":"operator_lib","version":"","root":"/x","files":[],"count":0,"truncated":false}`,
	)}
	svc := library.New(ws, library.Options{})

	if _, err := svc.Files(context.Background(), testRef); err != nil {
		t.Fatalf("Files: %v", err)
	}
	if ws.last.Timeout <= 0 {
		t.Errorf("Timeout = %v, want the default filled in", ws.last.Timeout)
	}
	if ws.last.MaxOutputBytes != 1<<20 {
		t.Errorf("MaxOutputBytes = %d, want the 1<<20 default", ws.last.MaxOutputBytes)
	}
	if ws.last.Dir != "" {
		t.Errorf("Dir = %q, want empty — this package has no reason to set one", ws.last.Dir)
	}
	if len(ws.last.Env) != 0 {
		t.Errorf("Env = %v, want empty — operator_lib is not platform data", ws.last.Env)
	}
}
