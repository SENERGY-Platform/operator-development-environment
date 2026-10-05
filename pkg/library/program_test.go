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

// Finding 4. library_test.go drives operatorLibProgram exclusively against
// fakeWorkspace, whose answers are JSON wired up by hand — TestReadFileMapsOutsidePackage
// there only checks that the Go side translates an "outside_package" envelope
// correctly, and would stay green even if _read's own containment check in
// Python were broken. This file is package library rather than library_test so
// it can reach operatorLibProgram directly, and runs it through a real python3
// against a real directory tree — the same route Service.run drives through the
// kernel, minus the kernel — so the containment check under test is the one
// that actually runs in a pod.
package library

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requirePython3 finds an interpreter or skips with a reason: there is nothing
// this file can prove about the containment check without one.
func requirePython3(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed; skipping the tests that run the operator_lib helper for real")
	}
	return path
}

// runHelper executes operatorLibProgram exactly as Service.run invokes it —
// `python3 -c <program> <args...>` — with pythonPath as PYTHONPATH, so
// `import operator_lib` resolves against the fixture rather than whatever is
// actually installed on the machine running the test. dir is the process's
// working directory, a scratch one distinct from the fixture: if a path meant
// to look like code injection (see below) ever executed instead of being read
// as a string, anything it wrote would land there rather than in the fixture
// or the repository checkout.
//
// The helper's own contract is that it always exits 0 and reports a failure it
// recognises through the "error" field of its one JSON line (see the comment on
// operatorLibProgram in library.go) — so unlike Service.run, this treats a
// non-zero exit or unparsable stdout as a test failure outright rather than as
// one more condition to assert on.
func runHelper(t *testing.T, python, pythonPath, dir string, args ...string) envelope {
	t.Helper()
	full := append([]string{"-c", operatorLibProgram}, args...)
	cmd := exec.Command(python, full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonPath)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the operator_lib helper: %v\nstderr: %s", err, errBuf.String())
	}
	var env envelope
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil {
		t.Fatalf("decoding the helper's stdout: %v\nstdout: %q\nstderr: %s",
			err, out.String(), errBuf.String())
	}
	return env
}

// buildPackageFixture lays out a normal operator_lib package:
//
//	<root>/operator_lib/__init__.py
//	<root>/operator_lib/core.py
//	<root>/operator_lib/sub/module.py
//
// root is what PYTHONPATH is set to, so find_spec resolves operator_lib the
// same way it would against a real site-packages entry; pkgDir is the package
// directory itself, for fixtures that add more to it.
func buildPackageFixture(t *testing.T) (root, pkgDir string) {
	t.Helper()
	root = t.TempDir()
	pkgDir = filepath.Join(root, "operator_lib")
	if err := os.MkdirAll(filepath.Join(pkgDir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir package: %v", err)
	}
	files := map[string]string{
		"__init__.py":   "# operator_lib\n",
		"core.py":       "def run():\n    return 42\n",
		"sub/module.py": "VALUE = 1\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(pkgDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root, pkgDir
}

// The legitimate case first: a file inside a subdirectory of the package reads
// back as itself.
func TestHelperReadsALegitimateSubpath(t *testing.T) {
	python := requirePython3(t)
	root, _ := buildPackageFixture(t)
	work := t.TempDir()

	env := runHelper(t, python, root, work, "read", "sub/module.py", "1000")
	if env.Error != "" {
		t.Fatalf("reading sub/module.py: error = %q (%s)", env.Error, env.Message)
	}
	if env.Path != "sub/module.py" || !strings.Contains(env.Text, "VALUE = 1") {
		t.Errorf("env = %+v, want sub/module.py's own content", env)
	}
}

// A ".." that reaches a file one directory above the package root.
func TestHelperRefusesADotDotEscalation(t *testing.T) {
	python := requirePython3(t)
	root, _ := buildPackageFixture(t)
	work := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "secret.txt"),
		[]byte("not for operator_lib\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	env := runHelper(t, python, root, work, "read", "../secret.txt", "1000")
	if env.Error != "outside_package" {
		t.Fatalf("error = %q, want outside_package", env.Error)
	}
}

// An absolute path. _read's own comment explains why this is not a special
// case: os.path.join drops root when the argument is absolute, but the
// resolved path still will not start with root, so the same check refuses it.
func TestHelperRefusesAnAbsolutePath(t *testing.T) {
	python := requirePython3(t)
	root, _ := buildPackageFixture(t)
	work := t.TempDir()

	env := runHelper(t, python, root, work, "read", "/definitely-not-operator-lib/secret.txt", "1000")
	if env.Error != "outside_package" {
		t.Fatalf("error = %q, want outside_package", env.Error)
	}
}

// A symlink inside the package pointing outside it. Finding 5 rides along here
// rather than in its own test: the same fixture is exactly what shows both
// halves of the inconsistency — _read must refuse the target, and _tree must
// not list the link as if it were an ordinary file with a size that belongs to
// what it points at.
func TestHelperRefusesAndHidesASymlinkThatEscapesThePackage(t *testing.T) {
	python := requirePython3(t)
	root, pkgDir := buildPackageFixture(t)
	work := t.TempDir()

	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("not part of the package\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(pkgDir, "escape.py")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are not available in this environment: %v", err)
	}

	read := runHelper(t, python, root, work, "read", "escape.py", "1000")
	if read.Error != "outside_package" {
		t.Fatalf("reading a symlink out of the package: error = %q, want outside_package", read.Error)
	}

	tree := runHelper(t, python, root, work, "tree", "1000")
	if tree.Error != "" {
		t.Fatalf("tree: error = %q (%s)", tree.Error, tree.Message)
	}
	for _, entry := range tree.Files {
		if entry.Path == "escape.py" {
			t.Errorf("tree lists %q, a symlink whose target is outside the package (finding 5)",
				entry.Path)
		}
	}
}

// A sibling directory that merely starts with the package's own name as a
// string. A containment check built on a bare startswith(root) rather than
// startswith(root + separator) would let this through, because
// "operator_libextra" has "operator_lib" as a string prefix.
func TestHelperRefusesTheSiblingDirectoryPrefixTrap(t *testing.T) {
	python := requirePython3(t)
	root, _ := buildPackageFixture(t)
	work := t.TempDir()

	sibling := filepath.Join(root, "operator_libextra")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "secret.txt"),
		[]byte("not operator_lib\n"), 0o644); err != nil {
		t.Fatalf("write sibling file: %v", err)
	}

	env := runHelper(t, python, root, work, "read", "../operator_libextra/secret.txt", "1000")
	if env.Error != "outside_package" {
		t.Fatalf("error = %q, want outside_package — a prefix check without the "+
			"trailing separator would let \"operator_libextra\" pass as a prefix of "+
			"\"operator_lib\"", env.Error)
	}
}

// _tree's own exclusions: __pycache__ is not entered at all, and a .pyc is
// skipped wherever it sits.
func TestHelperTreeExcludesPycacheAndCompiledFiles(t *testing.T) {
	python := requirePython3(t)
	root, pkgDir := buildPackageFixture(t)
	work := t.TempDir()

	cache := filepath.Join(pkgDir, "__pycache__")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("mkdir __pycache__: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cache, "core.cpython-312.pyc"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write cached bytecode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "core.pyc"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write top-level .pyc: %v", err)
	}

	env := runHelper(t, python, root, work, "tree", "1000")
	if env.Error != "" {
		t.Fatalf("tree: error = %q (%s)", env.Error, env.Message)
	}
	for _, entry := range env.Files {
		if strings.Contains(entry.Path, "__pycache__") || strings.HasSuffix(entry.Path, ".pyc") {
			t.Errorf("tree lists %q, which should have been excluded", entry.Path)
		}
	}
}

// Finding 3. operator_lib installed as a single module file — operator_lib.py
// directly on the path, no package directory — has no submodule_search_locations
// for the helper to bound a read to, and is refused as its own condition rather
// than silently falling back to the whole of the containing directory.
func TestHelperRefusesASingleModuleInstall(t *testing.T) {
	python := requirePython3(t)
	root := t.TempDir()
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "operator_lib.py"), []byte("VALUE = 1\n"), 0o644); err != nil {
		t.Fatalf("write single module: %v", err)
	}

	if env := runHelper(t, python, root, work, "tree", "1000"); env.Error != "single_module" {
		t.Fatalf("tree: error = %q, want single_module for an install with no package directory",
			env.Error)
	}
	if env := runHelper(t, python, root, work, "read", "operator_lib.py", "1000"); env.Error != "single_module" {
		t.Fatalf("read: error = %q, want single_module regardless of the subcommand", env.Error)
	}
}

// A path shaped like an attempt to break out of the program text and run code,
// the same string TestReadFileArgvCarriesThePathNotTheProgramText in
// library_test.go uses to prove the same thing one layer up against a fake.
// Here it runs through the real interpreter: refused as outside_package, and
// nothing it names ever executes.
func TestHelperRefusesAPathThatLooksLikeAnInjectionAttempt(t *testing.T) {
	python := requirePython3(t)
	root, _ := buildPackageFixture(t)
	work := t.TempDir()

	const suspicious = `../../etc/passwd"; import os; os.system("echo pwned`

	env := runHelper(t, python, root, work, "read", suspicious, "1000")
	if env.Error != "outside_package" {
		t.Fatalf("error = %q, want outside_package", env.Error)
	}
	if env.Path != suspicious {
		t.Errorf("path = %q, want the argument echoed back unchanged", env.Path)
	}
	if _, err := os.Stat(filepath.Join(work, "pwned")); !os.IsNotExist(err) {
		t.Error("a file named \"pwned\" exists in the working directory: the " +
			"injection-shaped path ran as code")
	}
}
