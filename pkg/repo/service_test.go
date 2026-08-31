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

package repo_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
)

// M7's acceptance criterion, as one test: a compliant operator repository is
// created, scaffolded, committed and pushed.
func TestCreateScaffoldCommitAndPush(t *testing.T) {
	h := newHarness(t)
	h.connect()

	status, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Description: "Forecast PV generation",
		Scaffold: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The repository GitHub was asked for had to be empty, or the developer's first
	// commit would sit on top of one they did not make.
	if len(h.github.Created()) != 1 || h.github.Created()[0]["auto_init"] != false {
		t.Fatalf("created with %v, want auto_init false", h.github.Created())
	}

	if !status.Cloned || status.Link.Path != "jonah/pv-forecast" {
		t.Fatalf("status = %+v, want a checkout at pv-forecast", status)
	}
	// An empty repository's clone is on an unborn branch, and it has to be the
	// branch GitHub will serve rather than the local default.
	if !status.Unborn || status.Branch != "main" {
		t.Errorf("status.Unborn = %v on branch %q, want an unborn main",
			status.Unborn, status.Branch)
	}
	if !status.Scaffold.Complete {
		t.Errorf("scaffold is missing %v", status.Scaffold.Missing)
	}
	if !status.Dirty || len(status.Changes) == 0 {
		t.Error("the scaffold produced no uncommitted changes, so nothing was written")
	}

	// Nothing is committed yet: that is §5.11 item 5, and it is the property that
	// makes the scaffold reviewable.
	if status.Head != "" {
		t.Errorf("head = %q, want no commit before the developer makes one", status.Head)
	}

	for _, path := range repo.ScaffoldPaths() {
		if _, err := os.Stat(h.path("jonah", "pv-forecast", path)); err != nil {
			t.Errorf("%s did not reach the working copy: %v", path, err)
		}
	}
	// The pin of D15 is recorded, not merely rendered.
	if status.Link.OperatorLibRef != "v1.3.1" {
		t.Errorf("operator lib ref = %q, want the resolved tag", status.Link.OperatorLibRef)
	}
	pyproject := h.read(t, "jonah/pv-forecast/pyproject.toml")
	if !strings.Contains(pyproject, "@v1.3.1") {
		t.Errorf("pyproject does not carry the pin:\n%s", pyproject)
	}

	committed, err := h.service.Commit(context.Background(), repo.CommitRequest{
		Request: h.request(), Message: "Scaffold the operator",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if committed.SHA == "" || committed.Branch != "main" || committed.Files == 0 {
		t.Fatalf("commit = %+v", committed)
	}

	pushed, err := h.service.Push(context.Background(), repo.PushRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushed.Branch != "main" || pushed.HeadSHA != committed.SHA {
		t.Fatalf("push = %+v, want main at the commit just made", pushed)
	}

	// The remote is a real repository, so "pushed" is checkable rather than
	// asserted: its main branch now names the developer's commit.
	remoteHead := strings.TrimSpace(repotest.Git(t, h.remote, "rev-parse", "refs/heads/main"))
	if remoteHead != committed.SHA {
		t.Errorf("the remote is at %s, want %s", remoteHead, committed.SHA)
	}
	if author := repotest.Git(t, h.remote, "log", "-1", "--pretty=%an <%ae>"); !strings.Contains(
		author, "jonah@example.org") {
		t.Errorf("the commit is by %q, want the developer's own identity", strings.TrimSpace(author))
	}

	// And the working copy is clean afterwards, with the divergence reported as
	// resolved rather than remembered.
	after, err := h.service.Status(context.Background(), repo.StatusRequest{
		Request: h.request(), Fetch: true,
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if after.Dirty || after.Ahead != 0 || after.Behind != 0 || after.Unborn {
		t.Errorf("status after push = %+v, want a clean tree level with origin", after)
	}
	if after.Head != committed.SHA || after.HeadSubject != "Scaffold the operator" {
		t.Errorf("head = %s %q", after.Head, after.HeadSubject)
	}
}

// The generated Python has to be Python. py_compile is a syntax check rather than
// an import check, which is the part that can be verified without the platform's
// own libraries installed.
func TestTheScaffoldedPythonCompiles(t *testing.T) {
	h := newHarness(t)
	h.connect()
	if _, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Scaffold: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	for _, name := range []string{"main.py", "op.py", "training.py", "tests/test_op.py"} {
		command := exec.Command(python, "-m", "py_compile", h.path("jonah", "pv-forecast", name))
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("%s does not compile: %v\n%s", name, err, output)
		}
	}
}

func TestSelectingAnExistingRepositoryReusesTheCheckout(t *testing.T) {
	h := newHarness(t)
	h.connect()

	// A remote with history, which is what an existing repository is.
	seed := filepath.Join(t.TempDir(), "seed")
	repotest.Git(t, "", "clone", h.remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "op.py"), []byte("# theirs\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	repotest.Git(t, seed, "add", "op.py")
	repotest.Git(t, seed, "commit", "-m", "Their own operator")
	repotest.Git(t, seed, "push", "origin", "HEAD:refs/heads/main")

	first, err := h.service.Select(context.Background(), repo.SelectRequest{
		Request: h.request(), FullName: "jonah/existing-operator",
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !first.Cloned || first.Head == "" {
		t.Fatalf("status = %+v, want a populated checkout", first)
	}
	// Selecting an existing repository does not scaffold over it: the developer's
	// own op.py survives and the compliance report says what is missing instead.
	if h.read(t, "jonah/existing-operator/op.py") != "# theirs\n" {
		t.Error("the developer's own file was overwritten")
	}
	if first.Scaffold.Complete || len(first.Scaffold.Missing) == 0 {
		t.Errorf("scaffold state = %+v, want the missing files reported", first.Scaffold)
	}

	// Uncommitted work, and a second selection of the same repository. §5.11 item 5
	// and item 6 together: reuse the checkout, and do not touch what is in it.
	scratch := h.path("jonah", "existing-operator", "scratch.py")
	if err := os.WriteFile(scratch, []byte("# mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	second, err := h.service.Select(context.Background(), repo.SelectRequest{
		Request: h.request(), FullName: "jonah/existing-operator",
	})
	if err != nil {
		t.Fatalf("Select again: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("the uncommitted file was lost by the second selection: %v", err)
	}
	if !second.Dirty {
		t.Error("the uncommitted change was not reported on reopen")
	}
	var untracked bool
	for _, change := range second.Changes {
		if change.Path == "scratch.py" && change.Kind == "untracked" {
			untracked = true
		}
	}
	if !untracked {
		t.Errorf("changes = %+v, want scratch.py as untracked", second.Changes)
	}
}

func TestStatusReportsDivergenceInBothDirections(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	if _, err := h.service.Push(context.Background(),
		repo.PushRequest{Request: h.request()}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Someone else pushes, and the developer commits locally. That is the case
	// §5.11 item 5 exists for: a checkout that is both ahead and behind.
	other := filepath.Join(t.TempDir(), "other")
	repotest.Git(t, "", "clone", h.remote, other)
	if err := os.WriteFile(filepath.Join(other, "colleague.py"), []byte("# theirs\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	repotest.Git(t, other, "add", "colleague.py")
	repotest.Git(t, other, "commit", "-m", "A colleague's change")
	repotest.Git(t, other, "push", "origin", "HEAD:refs/heads/main")

	if err := os.WriteFile(h.path("jonah", "pv-forecast", "mine.py"), []byte("# mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := h.service.Commit(context.Background(), repo.CommitRequest{
		Request: h.request(), Message: "My change",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Without a fetch the counts are what ODE last knew, and the response says so
	// rather than presenting a stale zero as agreement.
	stale, err := h.service.Status(context.Background(), repo.StatusRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if stale.Fetched || stale.Behind != 0 {
		t.Errorf("status without a fetch = fetched %v behind %d, want an unfetched zero",
			stale.Fetched, stale.Behind)
	}

	fetched, err := h.service.Fetch(context.Background(), repo.FetchRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !fetched.Fetched || fetched.Ahead != 1 || fetched.Behind != 1 || !fetched.Diverged {
		t.Errorf("status = ahead %d behind %d diverged %v, want 1/1 and diverged",
			fetched.Ahead, fetched.Behind, fetched.Diverged)
	}
}

func TestUncommittedChangesCanBeStashedOrDiscardedButNeverSilentlyReset(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	if err := os.WriteFile(h.path("jonah", "pv-forecast", "op.py"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "scratch.py"), []byte("# new\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Discarding is the destructive answer of the three, so it has to be confirmed.
	if _, err := h.service.Discard(context.Background(),
		repo.DiscardRequest{Request: h.request()}); !errors.Is(err, repo.ErrInvalidRequest) {
		t.Fatalf("unconfirmed discard error = %v, want a refusal", err)
	}
	if h.read(t, "jonah/pv-forecast/op.py") != "# changed\n" {
		t.Fatal("the refused discard changed the working copy anyway")
	}

	// Stash first: reversible, and it takes the untracked file with it.
	stashed, err := h.service.Stash(context.Background(), repo.StashRequest{
		Request: h.request(), Message: "before the experiment",
	})
	if err != nil {
		t.Fatalf("Stash: %v", err)
	}
	if stashed.Dirty {
		t.Errorf("status after the stash = %+v, want a clean tree", stashed.Changes)
	}
	if _, err := os.Stat(h.path("jonah", "pv-forecast", "scratch.py")); !os.IsNotExist(err) {
		t.Error("the untracked file was left behind by the stash")
	}
	if stash := repotest.Git(t, h.path("jonah", "pv-forecast"), "stash", "list"); !strings.Contains(
		stash, "before the experiment") {
		t.Errorf("stash list = %q, want the developer's message", stash)
	}

	// Then discard, confirmed.
	repotest.Git(t, h.path("jonah", "pv-forecast"), "stash", "pop")
	discarded, err := h.service.Discard(context.Background(), repo.DiscardRequest{
		Request: h.request(), Confirm: true,
	})
	if err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if discarded.Dirty {
		t.Errorf("status after the discard = %+v, want a clean tree", discarded.Changes)
	}
	if content := h.read(t, "jonah/pv-forecast/op.py"); content == "# changed\n" {
		t.Error("the discard left the modification in place")
	}
}

func TestCommitOnACleanTreeIsAnAnswerRatherThanAFailure(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	_, err := h.service.Commit(context.Background(), repo.CommitRequest{
		Request: h.request(), Message: "Nothing changed",
	})
	if !errors.Is(err, repo.ErrNothingToCommit) {
		t.Fatalf("error = %v, want ErrNothingToCommit", err)
	}
}

func TestTheFileTreeShowsEveryFileAndNoObjectDatabase(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	tree, err := h.service.Files(context.Background(), h.request())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	paths := map[string]bool{}
	flatten(tree, func(path string) {
		paths[strings.TrimPrefix(path, "jonah/pv-forecast/")] = true
	})

	// D14: the workflow file and the gitignore are files of the repository like any
	// other, which is exactly what jupyter_server's contents API would have hidden.
	for _, wanted := range []string{
		".github/workflows/build.yml", ".gitignore", "op.py", "tests/test_op.py",
	} {
		if !paths[wanted] {
			t.Errorf("%s is not in the tree", wanted)
		}
	}
	if paths[".git"] || paths[".git/config"] {
		t.Error("the object database is in the tree")
	}
}

func TestWritingAFileChangesTheWorkingCopyAndNothingElse(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	written, err := h.service.WriteFile(context.Background(), h.request(),
		"op.py", []byte("# rewritten by the assistant\n"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if written.Committed {
		t.Error("a write reported itself as committed")
	}
	if h.read(t, "jonah/pv-forecast/op.py") != "# rewritten by the assistant\n" {
		t.Error("the file was not written")
	}

	status, err := h.service.Status(context.Background(), repo.StatusRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Dirty {
		t.Fatal("the write is not visible as an uncommitted change")
	}
	head := strings.TrimSpace(repotest.Git(t, h.path("jonah", "pv-forecast"), "log", "-1", "--pretty=%s"))
	if head != "Scaffold the operator" {
		t.Errorf("head is %q, so the write committed something", head)
	}

	// Reading it back carries the editor's language hint, so the pane and the tool
	// agree on what the file is.
	file, err := h.service.ReadFile(context.Background(), h.request(), "op.py")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if file.Language != "python" || file.Text != "# rewritten by the assistant\n" {
		t.Errorf("file = %+v", file)
	}
}

func TestPathsThatLeaveTheRepositoryAreRefused(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	// The first two leave the repository while staying inside the workspace, which
	// is the case the kernel's own check cannot catch.
	for _, path := range []string{"../notebooks/scratch.py", "../../etc/passwd", "/etc/passwd"} {
		if _, err := h.service.WriteFile(context.Background(), h.request(),
			path, []byte("x")); !errors.Is(err, repo.ErrInvalidRequest) {
			t.Errorf("writing %q: error = %v, want a refusal", path, err)
		}
	}
	// .git is the one exception to "every file": it is git's storage, not source.
	if _, err := h.service.WriteFile(context.Background(), h.request(),
		".git/config", []byte("x")); !errors.Is(err, repo.ErrInvalidRequest) {
		t.Error("a write into the object database was allowed")
	}
	// A workflow file, by contrast, is a file of the repository (D14).
	if _, err := h.service.WriteFile(context.Background(), h.request(),
		".github/workflows/build.yml", []byte("name: edited\n")); err != nil {
		t.Errorf("writing the workflow file: %v", err)
	}
}

func TestOperationsWithoutACredentialOrARepositorySayWhichIsMissing(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.Repositories(context.Background(), h.request()); !errors.Is(
		err, repo.ErrNotConnected) {
		t.Errorf("error = %v, want ErrNotConnected", err)
	}
	h.connect()
	if _, err := h.service.Status(context.Background(),
		repo.StatusRequest{Request: h.request()}); !errors.Is(err, repo.ErrNoRepository) {
		t.Errorf("error = %v, want ErrNoRepository", err)
	}
}

// A second scaffold must not move the pin: the developer is on the library version
// their repository was built against until they decide otherwise (D15).
func TestASecondScaffoldKeepsTheOriginalPin(t *testing.T) {
	h := newHarness(t)
	h.connect()
	if _, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Scaffold: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	h.github.SetLatestTag("v2.0.0")
	if err := os.Remove(h.path("jonah", "pv-forecast", "op.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	result, err := h.service.Scaffold(context.Background(),
		repo.ScaffoldRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if result.OperatorLibRef != "v1.3.1" {
		t.Errorf("pin = %q, want the ref the repository was scaffolded with", result.OperatorLibRef)
	}
	if len(result.Written) != 1 || result.Written[0] != "op.py" {
		t.Errorf("written = %v, want only the missing file", result.Written)
	}
	if len(result.Skipped) != len(repo.ScaffoldPaths())-1 {
		t.Errorf("skipped %d of %d files", len(result.Skipped), len(repo.ScaffoldPaths()))
	}
}

// The lock is the reason this changed at all. The scaffolded README used to ask the
// developer to run `uv lock` before their first experiment, and the pod could not:
// no image in the chain shipped uv. Even with uv installed it is a step that has to
// be remembered to keep a run's recorded SHA meaning what it says, so ODE runs it —
// and what this asserts is the whole point of doing so, that the lock is in the
// developer's first commit without them having been asked for anything.
func TestTheScaffoldLocksTheDependenciesAndTheLockReachesTheFirstCommit(t *testing.T) {
	h := newHarness(t)
	h.connect()

	status, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Scaffold: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !status.Scaffold.Complete {
		t.Fatalf("scaffold is missing %v", status.Scaffold.Missing)
	}
	if lock := h.read(t, "jonah/pv-forecast/"+repo.LockFile); !strings.Contains(lock, "version = 1") {
		t.Errorf("%s is not what uv wrote:\n%s", repo.LockFile, lock)
	}

	if _, err := h.service.Commit(context.Background(), repo.CommitRequest{
		Request: h.request(), Message: "Scaffold the operator",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	committed := repotest.Git(t, h.path("jonah", "pv-forecast"),
		"show", "--name-only", "--pretty=format:", "HEAD")
	if !strings.Contains(committed, repo.LockFile) {
		t.Errorf("the first commit does not carry %s:\n%s", repo.LockFile, committed)
	}
}

// An image built before uv was added to it. The scaffold has already written eleven
// correct files by the time the lock runs, and the failure of the twelfth must not
// take them down — a developer who ends up with nothing has a worse problem than
// one who ends up with a repository and a sentence telling them to run `uv lock`.
func TestAScaffoldWithoutUvKeepsEverythingElseAndSaysWhatIsMissing(t *testing.T) {
	h := newHarness(t)
	h.connect()
	repotest.WithoutUV(t)

	status, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Scaffold: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, path := range repo.ScaffoldPaths() {
		_, err := os.Stat(h.path("jonah", "pv-forecast", path))
		if path == repo.LockFile {
			if err == nil {
				t.Errorf("%s exists although there is no uv to write it", path)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s did not reach the working copy: %v", path, err)
		}
	}
	if status.Scaffold.Complete {
		t.Error("the scaffold reports complete without a lock file")
	}

	// And it is the *reported* reason that has to name uv, because that sentence is
	// the whole repair path: it reaches the developer in the pane and the model in
	// the chat, and neither can act on "the scaffold half worked".
	result, err := h.service.Scaffold(context.Background(),
		repo.ScaffoldRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if !strings.Contains(result.LockError, "uv") {
		t.Errorf("lock error = %q, want it to name uv", result.LockError)
	}
}

// uv says what it was doing and then why it stopped. The second is the part that
// names the repair, so it is the part that has to survive the cut.
func TestAFailedLockIsReportedWithTheLineThatNamesTheFault(t *testing.T) {
	h := newHarness(t)
	h.connect()
	repotest.StubUV(t, repotest.FailingUV)

	if _, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Scaffold: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	result, err := h.service.Scaffold(context.Background(),
		repo.ScaffoldRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if !strings.Contains(result.LockError, "Git operation failed") {
		t.Errorf("lock error = %q, want uv's own complaint", result.LockError)
	}
	for _, written := range result.Written {
		if written == repo.LockFile {
			t.Errorf("%s is reported written although uv refused", repo.LockFile)
		}
	}
}

// A lock the developer resolved themselves is theirs, exactly like a file they
// wrote in place of one of ours. Re-running the scaffold to recover a deleted file
// must not quietly re-resolve their dependencies.
func TestASecondScaffoldDoesNotReplaceTheDevelopersLock(t *testing.T) {
	h := newHarness(t)
	h.connect()
	if _, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Scaffold: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mine := "version = 1\n# resolved by hand\n"
	if err := os.WriteFile(h.path("jonah", "pv-forecast", repo.LockFile),
		[]byte(mine), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := os.Remove(h.path("jonah", "pv-forecast", "op.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	result, err := h.service.Scaffold(context.Background(),
		repo.ScaffoldRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if got := h.read(t, "jonah/pv-forecast/"+repo.LockFile); got != mine {
		t.Errorf("%s was rewritten:\n%s", repo.LockFile, got)
	}
	var skipped bool
	for _, path := range result.Skipped {
		if path == repo.LockFile {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("skipped = %v, want %s among them", result.Skipped, repo.LockFile)
	}
	if result.LockError != "" {
		t.Errorf("lock error = %q on a scaffold that did not need to lock", result.LockError)
	}
}

// createAndCommit is the state most tests start from: a scaffolded repository with
// one commit and nothing pushed.
func (h *harness) createAndCommit(t *testing.T, name, message string) {
	t.Helper()
	if _, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: name, Scaffold: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := h.service.Commit(context.Background(), repo.CommitRequest{
		Request: h.request(), Message: message,
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func (h *harness) read(t *testing.T, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(h.workspace, relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(content)
}

// flatten walks a file tree, reporting every file path.
func flatten(tree repo.FileTree, visit func(string)) {
	var walk func(node kernel.Node)
	walk = func(node kernel.Node) {
		if node.Type == "file" {
			visit(node.Path)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(tree.Tree)
}

// The "start again under my own account" flow, which is the one that sent a
// developer's work to the wrong repository: two repositories of the same name
// under different owners are two repositories, and one directory cannot be both.
func TestARepositoryUnderADifferentOwnerGetsItsOwnCheckout(t *testing.T) {
	h := newHarness(t)
	h.connect()

	first, err := h.service.Select(context.Background(), repo.SelectRequest{
		Request: h.request(), FullName: "institut/pump-detector",
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if first.Link.Path != "institut/pump-detector" {
		t.Fatalf("checkout path = %q, want the owner in it: without it the next "+
			"repository of the same name lands in this directory", first.Link.Path)
	}
	if err := os.WriteFile(h.path("institut", "pump-detector", "notes.md"),
		[]byte("# mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	created, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pump-detector", Scaffold: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Link.FullName != "jonah/pump-detector" ||
		created.Link.Path != "jonah/pump-detector" {
		t.Fatalf("link = %s at %q, want the developer's own repository in its own directory",
			created.Link.FullName, created.Link.Path)
	}

	// The scaffold went into the new checkout, and the image reference says whose
	// repository it belongs to.
	workflow := h.read(t, "jonah/pump-detector/.github/workflows/build.yml")
	if !strings.Contains(workflow, "ghcr.io/jonah/pump-detector") {
		t.Errorf("the workflow does not push to the new owner's image:\n%s", workflow)
	}
	// And the institute's checkout is untouched — neither scaffolded over nor
	// stripped of the uncommitted work in it.
	if h.read(t, "institut/pump-detector/notes.md") != "# mine\n" {
		t.Error("the uncommitted work in the previous checkout was lost")
	}
	if _, err := os.Stat(h.path("institut", "pump-detector", "op.py")); !os.IsNotExist(err) {
		t.Error("the scaffold was written into the previous repository's checkout")
	}
}

// A checkout whose origin is not the selected repository is refused rather than
// committed into. RemoteMismatch was already reported in the status; a warning in
// the pane does not stop a push, and a push is what puts the work somewhere it
// cannot be taken back from.
func TestCommitAndPushRefuseACheckoutThatPointsAtADifferentRepository(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	// Wherever the checkout is, this is the developer repointing it — the same
	// state the create-under-a-new-owner flow used to produce by reusing one
	// directory for two repositories.
	status, err := h.service.Status(context.Background(), repo.StatusRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	checkout := filepath.Join(h.workspace, filepath.FromSlash(status.Link.Path))

	elsewhere := repotest.Remote(t)
	repotest.Git(t, checkout, "remote", "set-url", "origin", "file://"+elsewhere)
	if err := os.WriteFile(filepath.Join(checkout, "op.py"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := h.service.Commit(context.Background(), repo.CommitRequest{
		Request: h.request(), Message: "Into whose repository?",
	}); !errors.Is(err, repo.ErrRemoteMismatch) {
		t.Errorf("Commit error = %v, want ErrRemoteMismatch", err)
	}
	if _, err := h.service.Push(context.Background(),
		repo.PushRequest{Request: h.request()}); !errors.Is(err, repo.ErrRemoteMismatch) {
		t.Errorf("Push error = %v, want ErrRemoteMismatch", err)
	}
	if refs := strings.TrimSpace(repotest.Git(t, elsewhere, "for-each-ref")); refs != "" {
		t.Errorf("the push reached the repository the checkout points at:\n%s", refs)
	}
}

// Checkouts made before the owner was part of the path are named by the
// repository alone. They are adopted, not replaced: a re-clone into the new
// directory would leave the developer's uncommitted work somewhere nothing points
// at any more, which is the silent loss §5.11 item 6 forbids.
func TestACheckoutFromBeforeTheOwnerWasInThePathIsReusedRatherThanAbandoned(t *testing.T) {
	h := newHarness(t)
	h.connect()

	repotest.Git(t, h.workspace, "clone", "file://"+h.remote, "existing-operator")
	if err := os.WriteFile(h.path("existing-operator", "scratch.py"),
		[]byte("# mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	status, err := h.service.Select(context.Background(), repo.SelectRequest{
		Request: h.request(), FullName: "jonah/existing-operator",
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if status.Link.Path != "existing-operator" {
		t.Errorf("checkout path = %q, want the directory the work is already in",
			status.Link.Path)
	}
	if _, err := os.Stat(h.path("jonah", "existing-operator")); err == nil {
		t.Error("a second checkout was cloned beside the developer's own")
	}
	if h.read(t, "existing-operator/scratch.py") != "# mine\n" {
		t.Error("the uncommitted work was lost")
	}
	if !status.Dirty {
		t.Error("the uncommitted change was not reported on reopen")
	}
}

// busyOn is a pod that refuses any call carrying a git subcommand it was told to
// refuse, the way a kernel that has become busy since the last call would.
//
// It stands in for the race a developer produces without trying: they press
// Discard while a cell is running, and the claims the operation takes land on
// either side of the cell starting. Armed after the fixture is in place, because
// the fixture needs the same subcommands to work.
type busyOn struct {
	pod        repo.Workspace
	subcommand string

	mux   sync.Mutex
	armed bool
}

func (w *busyOn) arm() {
	w.mux.Lock()
	defer w.mux.Unlock()
	w.armed = true
}

func (w *busyOn) refuses(argv []string) bool {
	w.mux.Lock()
	defer w.mux.Unlock()
	if !w.armed {
		return false
	}
	for _, argument := range argv {
		if argument == w.subcommand {
			return true
		}
	}
	return false
}

func (w *busyOn) Command(
	ctx context.Context, ref kernel.Ref, cmd kernel.Command,
) (kernel.CommandResult, error) {
	if w.refuses(cmd.Argv) {
		return kernel.CommandResult{}, kernel.ErrBusy
	}
	return w.pod.Command(ctx, ref, cmd)
}

func (w *busyOn) CommandBatch(
	ctx context.Context, ref kernel.Ref, cmds []kernel.Command,
) ([]kernel.CommandResult, error) {
	// A batch is one claim, so a busy kernel refuses the whole of it — which is
	// the property the sequence is batched for.
	for _, cmd := range cmds {
		if w.refuses(cmd.Argv) {
			return nil, kernel.ErrBusy
		}
	}
	return w.pod.CommandBatch(ctx, ref, cmds)
}

func (w *busyOn) Tree(ctx context.Context, ref kernel.Ref, req kernel.TreeRequest) (kernel.Node, error) {
	return w.pod.Tree(ctx, ref, req)
}

func (w *busyOn) ReadFile(
	ctx context.Context, ref kernel.Ref, path string, maxBytes int,
) (kernel.FileContent, error) {
	return w.pod.ReadFile(ctx, ref, path, maxBytes)
}

func (w *busyOn) WriteFile(
	ctx context.Context, ref kernel.Ref, path string, content []byte,
) (kernel.Node, error) {
	return w.pod.WriteFile(ctx, ref, path, content)
}

func (w *busyOn) MakeDir(ctx context.Context, ref kernel.Ref, path string) (kernel.Node, error) {
	return w.pod.MakeDir(ctx, ref, path)
}

func (w *busyOn) Remove(ctx context.Context, ref kernel.Ref, path string, recursive bool) error {
	return w.pod.Remove(ctx, ref, path, recursive)
}

func (w *busyOn) Workspace() string { return w.pod.Workspace() }

// Discard is the one destructive operation in this package, and §5.11 item 6
// requires it not to take partial effect silently. Run as two claims it does: the
// reset wins its claim and the clean loses the next one, so the developer is told
// the kernel is busy while their tracked changes are already gone.
func TestADiscardRefusedPartWayThroughChangesNothing(t *testing.T) {
	var busy *busyOn
	h := newHarnessWith(t, func(pod repo.Workspace) repo.Workspace {
		busy = &busyOn{pod: pod, subcommand: "clean"}
		return busy
	})
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	if err := os.WriteFile(h.path("jonah", "pv-forecast", "op.py"),
		[]byte("# changed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "scratch.py"),
		[]byte("# new\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	busy.arm()

	_, err := h.service.Discard(context.Background(), repo.DiscardRequest{
		Request: h.request(), Confirm: true,
	})
	if !errors.Is(err, kernel.ErrBusy) {
		t.Fatalf("Discard error = %v, want ErrBusy", err)
	}
	// The answer said the operation did not run, so nothing may have run.
	if content := h.read(t, "jonah/pv-forecast/op.py"); content != "# changed\n" {
		t.Error("the tracked modification was reset by a discard that answered ErrBusy")
	}
	if _, err := os.Stat(h.path("jonah", "pv-forecast", "scratch.py")); err != nil {
		t.Errorf("the untracked file is gone: %v", err)
	}
}

// The same for Commit. Recoverable rather than destructive — nothing is lost by a
// stage — but a developer told the kernel was busy does not expect to find their
// working copy staged.
func TestACommitRefusedPartWayThroughStagesNothing(t *testing.T) {
	var busy *busyOn
	h := newHarnessWith(t, func(pod repo.Workspace) repo.Workspace {
		busy = &busyOn{pod: pod, subcommand: "commit"}
		return busy
	})
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	if err := os.WriteFile(h.path("jonah", "pv-forecast", "op.py"),
		[]byte("# changed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	busy.arm()

	_, err := h.service.Commit(context.Background(), repo.CommitRequest{
		Request: h.request(), Message: "Refused halfway",
	})
	if !errors.Is(err, kernel.ErrBusy) {
		t.Fatalf("Commit error = %v, want ErrBusy", err)
	}
	staged := strings.TrimSpace(repotest.Git(t, h.path("jonah", "pv-forecast"),
		"diff", "--cached", "--name-only"))
	if staged != "" {
		t.Errorf("the refused commit left %q staged", staged)
	}
}
