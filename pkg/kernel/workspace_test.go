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

package kernel_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
)

// The workspace operations are tested against a real python3 running the cell ODE
// sends, in a temporary directory standing in for the PVC. A Go reimplementation
// of the helper would agree with the test and could still disagree with the pod.

// workspaceService returns a service whose fake pod is a real directory.
func workspaceService(t *testing.T) (*kernel.Service, string, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "data", "ode"), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	hub := kerneltest.NewHub(t)
	hub.OnExecute(kerneltest.PythonExecutor(t, home))
	service := newService(t, hub, func(opts *kernel.Options) {
		opts.ExecuteTimeout = 30 * time.Second
		opts.MaxOutputBytes = 1 << 20
	})
	return service, unsignedToken("jonah"), filepath.Join(home, "data", "ode")
}

func TestWorkspaceWritesAndReadsAFileTheContentsAPICouldNot(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	// A dotted directory on purpose: this is the path jupyter_server's contents API
	// refuses with allow_hidden false, and D14 says every file.
	if _, err := service.WriteFile(context.Background(), ref(bearer),
		".github/workflows/build.yml", []byte("name: build\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(workspace, ".github/workflows/build.yml"))
	if err != nil {
		t.Fatalf("the file did not reach the workspace: %v", err)
	}
	if string(onDisk) != "name: build\n" {
		t.Errorf("on disk = %q", onDisk)
	}

	content, err := service.ReadFile(context.Background(), ref(bearer),
		".github/workflows/build.yml", 1<<20)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content.Text != "name: build\n" || content.Binary || content.Truncated {
		t.Errorf("read back %+v", content)
	}
	if content.Path != ".github/workflows/build.yml" {
		t.Errorf("path = %q", content.Path)
	}
}

func TestWorkspaceReadReportsBinaryAndTruncationRatherThanCorruptingEither(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	if err := os.WriteFile(filepath.Join(workspace, "model.bin"),
		[]byte{0x00, 0x01, 0x02, 0xff}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	binary, err := service.ReadFile(context.Background(), ref(bearer), "model.bin", 1<<20)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !binary.Binary || binary.Text != "" {
		t.Errorf("binary file read as %+v, want binary with no text", binary)
	}

	if err := os.WriteFile(filepath.Join(workspace, "long.txt"),
		[]byte(strings.Repeat("a", 100)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	short, err := service.ReadFile(context.Background(), ref(bearer), "long.txt", 10)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !short.Truncated || len(short.Text) != 10 || short.Size != 100 {
		t.Errorf("truncated read = %+v, want 10 of 100 bytes and the flag set", short)
	}
}

func TestWorkspaceRefusesToLeaveTheWorkspace(t *testing.T) {
	service, bearer, _ := workspaceService(t)

	for _, path := range []string{"../escape.txt", "a/../../escape.txt", "a/./../../x"} {
		if _, err := service.ReadFile(context.Background(), ref(bearer), path, 1024); err == nil {
			t.Errorf("reading %q was allowed", path)
		} else if !errors.Is(err, kernel.ErrInvalidRequest) {
			t.Errorf("reading %q: error = %v, want ErrInvalidRequest", path, err)
		}
	}

	// An absolute path is not refused but re-read as workspace-relative, which is
	// what cleanWorkspacePath has always done for the listing route. What matters is
	// that it cannot name a file outside: /etc/passwd exists on the host and must
	// still come back as missing.
	if content, err := service.ReadFile(context.Background(), ref(bearer), "/etc/passwd", 1024); err == nil {
		t.Errorf("an absolute path was resolved outside the workspace: %+v", content)
	} else if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// A symlink is the case the Go-side path check cannot see: every segment is
// legal, and only the pod knows where the link points.
func TestWorkspaceRefusesASymlinkOutOfTheWorkspace(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	outside := filepath.Join(filepath.Dir(filepath.Dir(workspace)), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "link.txt")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	if content, err := service.ReadFile(context.Background(), ref(bearer), "link.txt", 1024); err == nil {
		t.Fatalf("the symlink was followed out of the workspace: %+v", content)
	}
}

func TestWorkspaceTreeWalksRecursivelyAndExcludesWhatItIsTold(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	for _, path := range []string{"repo/.git/config", "repo/op.py", "repo/tests/test_op.py"} {
		full := filepath.Join(workspace, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	tree, err := service.Tree(context.Background(), ref(bearer), kernel.TreeRequest{
		Path: "repo", Recursive: true, Exclude: []string{".git"},
	})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	paths := map[string]string{}
	var walk func(kernel.Node)
	walk = func(node kernel.Node) {
		paths[node.Path] = node.Type
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(tree)

	if paths["repo/op.py"] != "file" || paths["repo/tests"] != "directory" ||
		paths["repo/tests/test_op.py"] != "file" {
		t.Errorf("tree = %v, want the checkout's files", paths)
	}
	if _, present := paths["repo/.git"]; present {
		t.Error("the excluded .git directory was walked")
	}
}

func TestWorkspaceTreeReportsWhatTheBudgetElided(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tree, err := service.Tree(context.Background(), ref(bearer), kernel.TreeRequest{MaxEntries: 2})
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Children) != 2 || tree.Elided != 3 {
		t.Errorf("tree listed %d children and elided %d, want 2 and 3",
			len(tree.Children), tree.Elided)
	}
}

func TestWorkspaceCommandRunsWithoutAShellAndReportsTheExitCode(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	// The argument is a shell metacharacter salad. Run through a shell it would
	// create a file; run as argv it can only be echoed.
	result, err := service.Command(context.Background(), ref(bearer), kernel.Command{
		Argv: []string{"python3", "-c", "import sys; print(sys.argv[1])", "; touch pwned"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "; touch pwned" {
		t.Fatalf("result = %+v, want the argument echoed verbatim", result)
	}
	if _, err := os.Stat(filepath.Join(workspace, "pwned")); !os.IsNotExist(err) {
		t.Error("the argument was interpreted by a shell")
	}
}

func TestWorkspaceCommandCarriesTheEnvironmentAndTheWorkingDirectory(t *testing.T) {
	service, bearer, workspace := workspaceService(t)
	if err := os.MkdirAll(filepath.Join(workspace, "repo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	result, err := service.Command(context.Background(), ref(bearer), kernel.Command{
		Argv: []string{"python3", "-c", "import os; print(os.environ['ODE_TEST']); print(os.getcwd())"},
		Dir:  "repo",
		Env:  map[string]string{"ODE_TEST": "carried"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	lines := strings.Fields(result.Stdout)
	if len(lines) != 2 || lines[0] != "carried" || !strings.HasSuffix(lines[1], "repo") {
		t.Errorf("result = %+v, want the variable and the directory", result)
	}
}

func TestWorkspaceCommandReportsAFailureRatherThanRaising(t *testing.T) {
	service, bearer, _ := workspaceService(t)

	result, err := service.Command(context.Background(), ref(bearer), kernel.Command{
		Argv: []string{"python3", "-c", "import sys; sys.stderr.write('nope\\n'); sys.exit(3)"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if result.ExitCode != 3 || !strings.Contains(result.Stderr, "nope") {
		t.Errorf("result = %+v, want exit 3 and the message", result)
	}
}

func TestWorkspaceCommandStopsAProgramThatWillNotFinish(t *testing.T) {
	service, bearer, _ := workspaceService(t)

	result, err := service.Command(context.Background(), ref(bearer), kernel.Command{
		Argv:    []string{"python3", "-c", "import time; time.sleep(30)"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !result.TimedOut {
		t.Errorf("result = %+v, want a timeout", result)
	}
}

func TestWorkspaceRemoveNeedsRecursiveForADirectory(t *testing.T) {
	service, bearer, workspace := workspaceService(t)
	if err := os.MkdirAll(filepath.Join(workspace, "cache", "inner"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := service.Remove(context.Background(), ref(bearer), "cache", false); err == nil {
		t.Fatal("a directory was removed without the recursive flag")
	}
	if err := service.Remove(context.Background(), ref(bearer), "cache", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "cache")); !os.IsNotExist(err) {
		t.Error("the directory is still there")
	}
}

func TestWorkspaceReadReportsAMissingFileAsMissing(t *testing.T) {
	service, bearer, _ := workspaceService(t)

	_, err := service.ReadFile(context.Background(), ref(bearer), "nothing/here.py", 1024)
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// A batch is one cell, and one cell is one claim: that is what lets a caller run
// a destructive sequence without a second caller taking the kernel between its
// halves. The observable half of that here is that the whole sequence ran with a
// single execution.
func TestACommandBatchRunsTheWholeSequenceInOneExecution(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	results, err := service.CommandBatch(context.Background(), ref(bearer), []kernel.Command{
		{Argv: []string{"python3", "-c", "open('first.txt', 'w').write('1')"}},
		{Argv: []string{"python3", "-c", "open('second.txt', 'w').write('2')"}},
	})
	if err != nil {
		t.Fatalf("CommandBatch: %v", err)
	}
	if len(results) != 2 || results[0].ExitCode != 0 || results[1].ExitCode != 0 {
		t.Fatalf("results = %+v, want both commands run", results)
	}
	for _, name := range []string{"first.txt", "second.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

// The sequence stops where it broke, and says how far it got. A commit after a
// stage that failed would record something other than what the developer asked
// for, so continuing is not an option — and reporting only "it failed" would
// leave the caller unable to tell what already happened.
func TestACommandBatchStopsAtTheFirstFailureAndReportsHowFarItGot(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	results, err := service.CommandBatch(context.Background(), ref(bearer), []kernel.Command{
		{Argv: []string{"python3", "-c", "open('before.txt', 'w').write('1')"}},
		{Argv: []string{"python3", "-c", "import sys; sys.stderr.write('nope\\n'); sys.exit(3)"}},
		{Argv: []string{"python3", "-c", "open('after.txt', 'w').write('3')"}},
	})
	if err != nil {
		t.Fatalf("CommandBatch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want the two that ran", results)
	}
	if results[1].ExitCode != 3 || !strings.Contains(results[1].Stderr, "nope") {
		t.Errorf("second result = %+v, want exit 3 and the message", results[1])
	}
	if _, err := os.Stat(filepath.Join(workspace, "after.txt")); !os.IsNotExist(err) {
		t.Error("the batch carried on past a command that failed")
	}
}

// The property kernel.Command's own comment rests on, which a batch must not
// quietly relax: argv is a list all the way into subprocess, so a commit message
// or a branch name from an HTTP request cannot become a second command.
func TestACommandBatchNeverPassesAnArgumentThroughAShell(t *testing.T) {
	service, bearer, workspace := workspaceService(t)

	results, err := service.CommandBatch(context.Background(), ref(bearer), []kernel.Command{
		{Argv: []string{"python3", "-c", "import sys; print(sys.argv[1])", "; touch pwned"}},
		{Argv: []string{"python3", "-c", "import sys; print(sys.argv[1])", "$(touch pwned-too)"}},
	})
	if err != nil {
		t.Fatalf("CommandBatch: %v", err)
	}
	if len(results) != 2 || strings.TrimSpace(results[0].Stdout) != "; touch pwned" ||
		strings.TrimSpace(results[1].Stdout) != "$(touch pwned-too)" {
		t.Fatalf("results = %+v, want both arguments echoed verbatim", results)
	}
	for _, name := range []string{"pwned", "pwned-too"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists, so an argument was interpreted by a shell", name)
		}
	}
}

// Each entry carries its own working directory and environment: a batch is not
// required to be one command repeated, and a git sequence run in a checkout
// depends on both.
func TestACommandBatchCarriesEachCommandsOwnDirectoryAndEnvironment(t *testing.T) {
	service, bearer, workspace := workspaceService(t)
	if err := os.MkdirAll(filepath.Join(workspace, "repo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	results, err := service.CommandBatch(context.Background(), ref(bearer), []kernel.Command{
		{
			Argv: []string{"python3", "-c", "import os; print(os.environ['ODE_TEST']); print(os.getcwd())"},
			Dir:  "repo",
			Env:  map[string]string{"ODE_TEST": "carried"},
		},
		{Argv: []string{"python3", "-c", "import os; print(os.getcwd())"}},
	})
	if err != nil {
		t.Fatalf("CommandBatch: %v", err)
	}
	first := strings.Fields(results[0].Stdout)
	if len(first) != 2 || first[0] != "carried" || !strings.HasSuffix(first[1], "repo") {
		t.Errorf("first result = %+v, want the variable and the directory", results[0])
	}
	if strings.HasSuffix(strings.TrimSpace(results[1].Stdout), "repo") {
		t.Errorf("second result = %+v, want the workspace root rather than the first "+
			"command's directory", results[1])
	}
}

// The reload case, which is the one that made the wait in claim necessary. The
// Code pane asks for the repository status, the file tree and the open file at
// once, and each of those is a cell; before the wait, two of the three answered
// 409 and the pane came up with the tree and the editor both showing an error.
func TestConcurrentWorkspaceOperationsAllAnswerRatherThanRefusingEachOther(t *testing.T) {
	service, bearer, workspace := workspaceService(t)
	if err := os.WriteFile(filepath.Join(workspace, "op.py"), []byte("print('op')\n"), 0o644); err != nil {
		t.Fatalf("seed the workspace: %v", err)
	}

	// Brought up first so that the pod spawn is not what the three below are timed
	// against: the bring-up is serialised by the session mutex, and the property
	// under test is about the claim after it.
	if _, err := service.Tree(context.Background(), ref(bearer), kernel.TreeRequest{}); err != nil {
		t.Fatalf("the bring-up read failed: %v", err)
	}

	operations := map[string]func() error{
		"tree": func() error {
			_, err := service.Tree(context.Background(), ref(bearer),
				kernel.TreeRequest{Recursive: true})
			return err
		},
		"file": func() error {
			_, err := service.ReadFile(context.Background(), ref(bearer), "op.py", 1<<20)
			return err
		},
		"status": func() error {
			_, err := service.Command(context.Background(), ref(bearer),
				kernel.Command{Argv: []string{"python3", "-c", "print('status')"}})
			return err
		},
	}

	start := make(chan struct{})
	failures := make(chan string, len(operations))
	var running sync.WaitGroup
	for name, operation := range operations {
		running.Add(1)
		go func() {
			defer running.Done()
			<-start
			if err := operation(); err != nil {
				failures <- name + ": " + err.Error()
			}
		}()
	}
	close(start)
	running.Wait()
	close(failures)

	for failure := range failures {
		t.Errorf("a concurrent workspace operation failed — %s", failure)
	}
}

// The other half of the same decision: the wait absorbs ODE's own collisions and
// nothing more. A kernel held by a cell of unknown length is still reported as
// busy, because that is the answer a developer can act on.
func TestAWorkspaceOperationStillReportsAKernelHeldByARunningCell(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, func(opts *kernel.Options) {
		opts.WorkspaceWait = 100 * time.Millisecond
	})
	bearer := unsignedToken("devuser")

	// Brought up first, so the hang below lands on the developer's cell rather than
	// on the hidden environment push that precedes it.
	if _, err := service.Ensure(context.Background(), ref(bearer)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	release := make(chan struct{})
	hub.Hang(release)

	cell, err := service.Run(context.Background(), ref(bearer), "train()")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	started := time.Now()
	_, err = service.ReadFile(context.Background(), ref(bearer), "op.py", 1<<20)
	waited := time.Since(started)
	if !errors.Is(err, kernel.ErrBusy) {
		t.Errorf("ReadFile behind a running cell = %v, want ErrBusy", err)
	}
	if waited < 100*time.Millisecond {
		t.Errorf("waited %v, want at least the configured wait before giving up", waited)
	}

	close(release)
	hub.Hang(nil)
	collect(t, cell)

	// And the refusal is not a state the session stays in: the next read reaches
	// the kernel. What it finds there is this fake's canned output rather than a
	// workspace answer, so only the refusal is asserted against.
	if _, err := service.ReadFile(context.Background(), ref(bearer), "op.py", 1<<20); errors.Is(err, kernel.ErrBusy) {
		t.Error("ReadFile after the cell finished = ErrBusy, so the release did not wake the claim")
	}
}
