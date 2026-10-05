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

// Package library reads operator_lib's own source out of the developer's pod, so
// a model working on an operator can look at the library it is calling without
// spending a run_code confirmation on it.
//
// It runs one fixed Python program through kernel.Service.Command — the same
// argv-list, no-shell route pkg/repo drives git through — rather than reading
// through kernel.Service.ReadFile. ReadFile refuses on purpose: cleanWorkspacePath
// rejects anything outside the developer's workspace, and operator_lib lives in
// site-packages, which is not under it. A short Python program run in the pod can
// reach it, and containment there is enforced the same way this package enforces
// its own: by resolving the real path and checking it, not by trusting the
// directory a name was joined onto.
//
// This is a read-only surface. It runs no `uv sync`, builds no venv, and writes
// nothing — the library it reads is whatever image already gave the pod.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// Workspace is the developer's pod, as this package needs it. *kernel.Service
// implements it.
//
// An interface rather than the concrete service, for the same reason pkg/repo
// declares its own: a test can drive the whole flow against a fake that never
// touches a pod, and the dependency points one way — this package knows about the
// workspace, the workspace knows nothing about operator_lib.
type Workspace interface {
	Command(ctx context.Context, ref kernel.Ref, cmd kernel.Command) (kernel.CommandResult, error)
}

// Options is how a deployment configures this package.
type Options struct {
	// Timeout bounds the helper program itself. Reading a library's source is a
	// handful of stat calls and a file read — seconds, not minutes — so the
	// default is far tighter than kernel.Command's own 5-minute default, which is
	// sized for git.
	Timeout time.Duration
	// MaxOutputBytes bounds the helper's own stdout — the JSON line it prints —
	// for Files, and is the default read size for ReadFile when its caller passes
	// none. It is not a limit on the library's total size; only on one answer.
	MaxOutputBytes int
	// MaxEntries bounds how many files Files reports. operator_lib is a normal
	// Python package, not a repository — a few hundred files at most — so this is
	// generous headroom rather than a working limit in practice.
	MaxEntries int
}

const (
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = 1 << 20
	defaultMaxEntries     = 400
)

// Service reads operator_lib out of one developer's pod.
//
// What it reads is the singleuser image's own install (singleuser-image/
// Dockerfile:93, pinned there as of this writing to v1.7.0), not the uv
// environment a scaffolded checkout resolves from its pyproject.toml pin. The two
// can diverge: a cell the developer runs through the kernel already imports
// against the image's install and never consults the repository's pin
// (docs/operator-lib-versions.md), so what this package shows a model is exactly
// what the developer's own cells see — but it is not necessarily what `uv run`
// resolves for a training run, which is a separate, per-repository pin. That is
// why every Listing and File carries the Version this call actually read rather
// than a version assumed from configuration: a stale answer that named the wrong
// library is worse than one that says plainly which one it read.
type Service struct {
	workspace Workspace
	opts      Options
}

// New builds the service, filling in defaults for whatever Options leaves zero.
func New(workspace Workspace, opts Options) *Service {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = defaultMaxOutputBytes
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = defaultMaxEntries
	}
	return &Service{workspace: workspace, opts: opts}
}

// ErrNotInstalled means operator_lib does not import in this pod at all — no
// site-packages entry importlib can find. There is nothing this package can read;
// the repair is outside it (installing the library, or answering without its
// source).
var ErrNotInstalled = errors.New(
	"operator_lib is not installed in this pod; there is no source to read")

// ErrNotFound is a path Files would not have listed: it does not exist under
// operator_lib's own directory, or exists but is not a regular file. Call Files
// again and read one of the paths it returns.
var ErrNotFound = errors.New(
	"no such file in operator_lib; call Files to see what is actually there")

// ErrOutsidePackage is the containment refusal: the requested path, once
// resolved, leaves operator_lib's own directory — by "..", by being absolute, or
// by a symlink inside the package that points outside it. The repair is the same
// as ErrNotFound: read a path Files actually listed.
var ErrOutsidePackage = errors.New(
	"the path resolves outside operator_lib; only a path Files listed can be read")

// ErrSingleModuleInstall means operator_lib is installed as one module file —
// operator_lib.py sitting directly in site-packages — rather than as a package
// directory. The containment root the helper resolves (see the comment on
// submodule_search_locations in operatorLibProgram) only exists for a package;
// a single-module install gives it nothing to bound a read to, and the helper
// refuses rather than falling back to the whole of site-packages, which would
// make every other library installed there readable through this one. The
// repair is not a retry: how the image installs the library is a deployment
// fact, and the model has to ask the developer about it rather than the pod.
var ErrSingleModuleInstall = errors.New(
	"operator_lib is installed as a single module rather than a package, so " +
		"there is no package directory this helper can safely read from; ask " +
		"the developer about this deployment")

// ErrInvalidRequest is a request this package refuses before ever running
// anything in the pod.
var ErrInvalidRequest = errors.New("invalid library request")

// Entry is one file of operator_lib's tree.
type Entry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Listing is operator_lib's file tree, as installed in this pod.
type Listing struct {
	Package string  `json:"package"`
	Version string  `json:"version"`
	Root    string  `json:"root"`
	Files   []Entry `json:"files"`
	// Count is how many files the walk actually found, which can be larger than
	// len(Files): Files is cut at Options.MaxEntries, Count is not. A model
	// reading Truncated=true alongside Count can tell how much of the package it
	// is not seeing, the same thing kernel.Node's Elided reports for a directory
	// walk.
	Count     int  `json:"count"`
	Truncated bool `json:"truncated"`
}

// File is one file of operator_lib, read whole up to the caller's bound. There is
// no line window here — pkg/tools applies from_line/max_lines on top of this with
// its own existing helpers, the same way it does for repo.File.
type File struct {
	Package   string `json:"package"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Text      string `json:"text"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
}

// Files lists operator_lib's tree as installed in this pod.
func (s *Service) Files(ctx context.Context, ref kernel.Ref) (Listing, error) {
	argv := []string{"python3", "-c", operatorLibProgram, "tree", strconv.Itoa(s.opts.MaxEntries)}
	env, err := s.run(ctx, ref, argv, s.opts.MaxOutputBytes)
	if err != nil {
		return Listing{}, err
	}
	if env.Error != "" {
		return Listing{}, mapError(env)
	}
	files := make([]Entry, 0, len(env.Files))
	for _, entry := range env.Files {
		files = append(files, Entry{Path: entry.Path, Size: entry.Size})
	}
	return Listing{
		Package:   env.Package,
		Version:   env.Version,
		Root:      env.Root,
		Files:     files,
		Count:     env.Count,
		Truncated: env.Truncated,
	}, nil
}

// ReadFile reads one file of operator_lib. path is relative to the package root,
// as Files reports it. maxBytes bounds the read; zero or less takes
// Options.MaxOutputBytes. There is no default for path — a caller names one of
// the paths Files returned.
func (s *Service) ReadFile(ctx context.Context, ref kernel.Ref, path string, maxBytes int) (File, error) {
	if path == "" {
		return File{}, fmt.Errorf("%w: no file was named", ErrInvalidRequest)
	}
	if maxBytes <= 0 {
		maxBytes = s.opts.MaxOutputBytes
	}
	argv := []string{"python3", "-c", operatorLibProgram, "read", path, strconv.Itoa(maxBytes)}
	// The helper writes plain JSON, not base64 like kernel's own workspace helper
	// does — see the comment on operatorLibProgram for why. json.dumps escapes a
	// quote or a backslash to two bytes and, with ensure_ascii left off, otherwise
	// passes UTF-8 through unchanged, so a file of maxBytes can, in the realistic
	// worst case of a file that is nothing but quotes and backslashes, still
	// double in transit. The 4x here is headroom past that, not a tight bound:
	// undersizing it does not lose data, it only turns a legitimate read into a
	// truncated-envelope error that asks for a smaller maxBytes.
	env, err := s.run(ctx, ref, argv, 4*maxBytes+8192)
	if err != nil {
		return File{}, err
	}
	if env.Error != "" {
		return File{}, mapError(env)
	}
	return File{
		Package:   env.Package,
		Version:   env.Version,
		Path:      env.Path,
		Size:      env.Size,
		Text:      env.Text,
		Binary:    env.Binary,
		Truncated: env.Truncated,
	}, nil
}

// envelope is the helper's one JSON line, in its raw shape for either
// subcommand — decoded once and then split into the exported Listing or File so
// neither carries fields that belong to the other.
type envelope struct {
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`

	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`

	Root  string          `json:"root,omitempty"`
	Files []envelopeEntry `json:"files,omitempty"`
	Count int             `json:"count,omitempty"`

	Path      string `json:"path,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Text      string `json:"text,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type envelopeEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// run executes the fixed helper program for one subcommand and decodes its one
// JSON line.
//
// A non-zero exit, a timeout or unparsable stdout is this function's own error,
// none of them the sentinels above: those are only for the conditions the helper
// itself recognises and reports through its "error" field. An exit code that is
// not zero means the helper itself failed to run to completion — a Python that
// crashed on something none of the named conditions cover — and stderr, not the
// exit code, is what says why (mirrors gitContext.failure in pkg/repo/git.go).
func (s *Service) run(
	ctx context.Context, ref kernel.Ref, argv []string, maxOutput int,
) (envelope, error) {
	result, err := s.workspace.Command(ctx, ref, kernel.Command{
		Argv:           argv,
		Timeout:        s.opts.Timeout,
		MaxOutputBytes: maxOutput,
		// Dir and Env are left at their zero value on purpose: reading
		// operator_lib's source needs no working directory inside the checkout
		// and no credential. It is not platform data (see kernel.Command.Env).
	})
	if err != nil {
		// Not wrapped further: a busy kernel surfaces as kernel.ErrBusy exactly as
		// it does for read_file, and a caller doing errors.Is on it must still see
		// it after this call.
		return envelope{}, err
	}
	if result.TimedOut {
		return envelope{}, fmt.Errorf("library: reading operator_lib timed out after %s", s.opts.Timeout)
	}
	if result.ExitCode != 0 {
		return envelope{}, fmt.Errorf("library: the operator_lib helper failed (exit %d): %s",
			result.ExitCode, lastLines(result.Stderr, 3))
	}
	if result.Truncated {
		return envelope{}, fmt.Errorf(
			"library: the operator_lib helper's answer exceeded %d bytes; ask for fewer bytes or fewer entries",
			maxOutput)
	}

	var env envelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &env); err != nil {
		return envelope{}, fmt.Errorf("library: the operator_lib helper answered unusably: %w (stderr: %s)",
			err, lastLines(result.Stderr, 3))
	}
	return env, nil
}

// mapError maps the helper's machine-readable cause onto this package's
// sentinels.
func mapError(env envelope) error {
	switch env.Error {
	case "not_installed":
		return ErrNotInstalled
	case "not_found":
		return fmt.Errorf("%w: %s", ErrNotFound, firstNonEmpty(env.Path, env.Message))
	case "outside_package":
		return fmt.Errorf("%w: %s", ErrOutsidePackage, firstNonEmpty(env.Path, env.Message))
	case "single_module":
		return ErrSingleModuleInstall
	default:
		return fmt.Errorf("library: the operator_lib helper reported %q: %s", env.Error, env.Message)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// lastLines keeps the tail of a diagnostic. The tail rather than the head, for
// the reason kernel.lastLines gives: a Python traceback puts the exception on the
// last line.
func lastLines(text string, count int) string {
	text = strings.TrimRight(strings.TrimSpace(text), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " | ")
}

// operatorLibProgram is the whole helper, a fixed Go string constant — never
// built per call, never touched by fmt.Sprintf. That is what makes the one rule
// enforceable by inspection: the path a caller asks for travels only as a
// sys.argv element (argv[2] below), and nothing this package holds is ever
// formatted into this text. Argv stays a list the whole way down to
// subprocess.run in kernel's own helper, so there is no shell to escape into
// even if that rule were broken.
//
// It answers with one plain JSON line on stdout and always exits 0 — a failure
// this program recognises is the "error" field of that line, not a non-zero
// exit, so that a condition it names can never be confused with the helper
// itself having crashed (see run). ensure_ascii is left off so a
// docstring or a comment in the library's own language round-trips as UTF-8
// bytes instead of a \uXXXX escape per character, which is also why ReadFile
// sizes its own read past a 1x multiplier: see the comment there.
const operatorLibProgram = `
import importlib.metadata
import importlib.util
import json
import os
import sys


def _reply(**payload):
    sys.stdout.write(json.dumps(payload, ensure_ascii=False))
    sys.stdout.write("\n")


def _version():
    # operator-lib is the distribution name; operator_lib is the import name, and
    # they occasionally get asked for interchangeably depending on how a package
    # was built. Neither found is not a failure: singleuser-image installs the
    # library from a git ref (see the comment on Service), and a ref carries no
    # version unless it happens to be a tag.
    for name in ("operator-lib", "operator_lib"):
        try:
            return importlib.metadata.version(name)
        except importlib.metadata.PackageNotFoundError:
            continue
    return ""


def _tree(root, max_entries):
    files = []
    for directory, dirnames, filenames in os.walk(root):
        dirnames[:] = [name for name in dirnames if name != "__pycache__"]
        for name in filenames:
            if name.endswith(".pyc"):
                continue
            full = os.path.join(directory, name)
            if os.path.islink(full):
                # A symlink pointing back inside the package is already listed
                # through its target; one pointing outside it is not something
                # read_lib_file can serve (_read's containment check refuses it
                # correctly). Listing it here anyway would let os.stat follow the
                # link and report the size of whatever it points at — outside
                # root included — which is a size for a read that is refused,
                # and a boundary this helper otherwise holds everywhere else.
                continue
            try:
                size = os.stat(full).st_size
            except OSError:
                # Gone between listing and stat-ing it, which is the developer's
                # own doing (nothing here writes) and not worth failing the whole
                # tree over.
                continue
            relative = os.path.relpath(full, root).replace(os.sep, "/")
            files.append({"path": relative, "size": size})
    # Sorted before the cut, not after: a walk's own order depends on directory
    # names, and truncating that order would drop an arbitrary tail rather than
    # the alphabetically last files.
    files.sort(key=lambda entry: entry["path"])
    total = len(files)
    return files[:max_entries], total


def _read(root, argument, max_bytes):
    root_real = os.path.realpath(root)
    # The containment check: realpath resolves ".." and a symlink alike, and the
    # comparison is against the *resolved* target regardless of how it was built.
    # os.path.join drops "root" entirely when argument is an absolute path, but
    # that does not weaken this: the resulting realpath still will not start with
    # root_real + os.sep, so an absolute escape is refused here exactly like a
    # relative one.
    target_real = os.path.realpath(os.path.join(root, argument))
    if not target_real.startswith(root_real + os.sep):
        return None, ("outside_package", argument)
    if not os.path.isfile(target_real):
        return None, ("not_found", argument)

    size = os.stat(target_real).st_size
    with open(target_real, "rb") as handle:
        raw = handle.read(max_bytes + 1)
    truncated = len(raw) > max_bytes
    raw = raw[:max_bytes]

    binary = b"\x00" in raw
    text = ""
    if not binary:
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError:
            binary = True

    relative = os.path.relpath(target_real, root_real).replace(os.sep, "/")
    return {
        "path": relative,
        "size": size,
        "text": text,
        "binary": binary,
        "truncated": truncated,
    }, None


def main():
    # sys.argv[0] is "-c" itself here, not a program name — this runs as
    # "python3 -c <this text> <op> ...".
    op = sys.argv[1]

    spec = importlib.util.find_spec("operator_lib")
    if spec is None or spec.origin is None:
        _reply(error="not_installed")
        return

    # A regular package's containment root is its own directory, and
    # submodule_search_locations names it directly — it is exactly what makes
    # "import operator_lib.core" resolvable at all, so it exists whenever there
    # is a directory to bound a read to. dirname(spec.origin) used to stand in
    # for this and is wrong for a single-module install (operator_lib.py sitting
    # directly in site-packages, no __init__.py): there is no package
    # directory, submodule_search_locations is empty, and dirname(spec.origin)
    # would instead name the whole site-packages directory — every other
    # library installed next to it would then be reachable through
    # read_lib_file's containment check, because that check would have nothing
    # narrower to bound itself to. That layout is refused instead of silently
    # widening the root.
    locations = list(spec.submodule_search_locations or [])
    if not locations:
        _reply(error="single_module")
        return
    root = locations[0]
    version = _version()

    if op == "tree":
        max_entries = int(sys.argv[2])
        files, total = _tree(root, max_entries)
        _reply(package="operator_lib", version=version, root=root,
               files=files, count=total, truncated=total > len(files))
        return

    if op == "read":
        argument = sys.argv[2]
        max_bytes = int(sys.argv[3])
        result, failure = _read(root, argument, max_bytes)
        if failure is not None:
            kind, path = failure
            _reply(error=kind, path=path)
            return
        result["package"] = "operator_lib"
        result["version"] = version
        _reply(**result)
        return

    _reply(error="failed", message="unknown library operation " + repr(op))


main()
`
