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

package experiments

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// Building the job package, in the developer's pod.
//
// The working copy is on the developer's PVC and ODE's backend cannot see that
// filesystem — the same constraint pkg/repo is built around — so the archive is
// produced *there*, by git, and carried back through the kernel's workspace
// surface.
//
// Three decisions, each of which has a wrong-looking alternative.
//
//   - **`git archive HEAD`, not a copy of the directory.** It produces the
//     *committed* tree, with the files at the archive root, which is both the
//     shape Ray's package format wants and the only shape under which the commit
//     SHA recorded on the MLflow run describes what actually ran (§5.11 item 7).
//     A zip of the working directory would also carry `.git`, every editor's
//     scratch file and any large local artefact — and would silently make the
//     recorded SHA a lie whenever the tree was dirty.
//
//   - **The archive comes back base64 through stdout, not through ReadFile.**
//     kernel.Service.ReadFile decodes as UTF-8 and reports anything that does not
//     as `binary: true` with no content at all, which is correct for its purpose
//     — the Code pane must not hand a JPEG to a text editor — and useless for a
//     zip. So the return trip is a tiny reader program whose stdout is ASCII by
//     construction. That program also enforces the size cap and unlinks the
//     temporary file, so a refused launch leaves nothing behind.
//
//   - **The temporary file is in the pod's `/tmp`, not in the workspace.** It is
//     not the developer's work, it must not appear in the Code pane's tree, and
//     above all it must not be inside the checkout, where it would make the
//     working copy dirty and so make the next launch refuse itself. `/tmp` in the
//     pod is ephemeral, which is the correct lifetime for it.

// archive is the built job package.
type archive struct {
	// Bytes is the zip. Held whole because Ray's upload is a single PUT and
	// because the hash that names it is over the whole content.
	Bytes []byte
	// CommitSHA is the state it was built from.
	CommitSHA string
}

// archiveReader is the program that carries the zip back.
//
// The path and the limit arrive as argv rather than being interpolated into the
// source, for the reason kernel.workspaceCode gives: a value ODE pastes into
// Python is a bug waiting for the first path with a quote in it. The unlink is in
// a finally, so the temporary file goes away whether the read succeeded, exceeded
// the cap, or raised.
const archiveReader = `
import base64, os, sys

path, limit = sys.argv[1], int(sys.argv[2])
try:
    size = os.path.getsize(path)
    if size > limit:
        sys.stderr.write("ODE_TOO_LARGE %d" % size)
        sys.exit(2)
    with open(path, "rb") as handle:
        sys.stdout.write(base64.b64encode(handle.read()).decode("ascii"))
finally:
    try:
        os.unlink(path)
    except OSError:
        pass
`

// tooLargeMarker is what archiveReader writes when the cap bites. A marker rather
// than a parsed message, so the two cases — over the cap, and any other failure —
// are told apart by something that cannot be produced by accident.
const tooLargeMarker = "ODE_TOO_LARGE"

// buildArchive produces the job package for one checkout.
//
// checkout is workspace-relative; tempPath is an absolute path in the pod, unique
// per submission so two launches cannot collide over it.
func (s *Service) buildArchive(
	ctx context.Context, ref kernel.Ref, repository, checkout, commitSHA, tempPath string,
) (archive, error) {
	// git archive writes the zip itself. -o rather than stdout because the workspace
	// surface decodes a command's stdout as text with replacement characters, which
	// would corrupt every byte of a zip that is not valid UTF-8.
	written, err := s.workspace.Command(ctx, ref, kernel.Command{
		Argv:           []string{"git", "archive", "--format=zip", "-o", tempPath, commitSHA},
		Dir:            checkout,
		Timeout:        s.opts.CommandTimeout,
		MaxOutputBytes: 64 << 10,
	})
	if err != nil {
		return archive{}, err
	}
	if written.ExitCode != 0 || written.TimedOut {
		return archive{}, fmt.Errorf(
			"experiments: git archive of %s failed (exit %d): %s",
			shortSHA(commitSHA), written.ExitCode, firstLine(written.Stderr))
	}

	// base64 costs four bytes per three, and the cell's own framing needs a little
	// more, so the output bound is derived from the package bound rather than
	// configured separately — two figures that must stay in step are one figure.
	limit := s.opts.MaxPackageBytes
	read, err := s.workspace.Command(ctx, ref, kernel.Command{
		Argv:           []string{"python3", "-c", archiveReader, tempPath, strconv.FormatInt(limit, 10)},
		Dir:            checkout,
		Timeout:        s.opts.CommandTimeout,
		MaxOutputBytes: int(limit/3*4) + 8192,
	})
	if err != nil {
		return archive{}, err
	}
	if read.ExitCode != 0 || read.TimedOut {
		if size, over := parseTooLarge(read.Stderr); over {
			return archive{}, &PackageTooLargeError{
				Repository: repository, CommitSHA: commitSHA, Bytes: size, Limit: limit,
			}
		}
		return archive{}, fmt.Errorf(
			"experiments: reading the job package back from the pod failed (exit %d): %s",
			read.ExitCode, lastLine(read.Stderr))
	}
	if read.Truncated {
		// Belt and braces behind the cap the reader already enforces: a truncated
		// archive is a zip that unpacks to a partial repository, and a job that ran
		// against one would fail in a way nobody could diagnose from the run.
		return archive{}, &PackageTooLargeError{
			Repository: repository, CommitSHA: commitSHA, Bytes: limit, Limit: limit,
		}
	}

	// Whitespace, because the helper's stdout may have been split across several
	// stream messages on its way through the kernel protocol.
	payload := strings.Join(strings.Fields(read.Stdout), "")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return archive{}, fmt.Errorf(
			"experiments: the job package came back unreadable from the pod: %w", err)
	}
	if len(decoded) == 0 {
		return archive{}, fmt.Errorf(
			"experiments: the job package for %s is empty", shortSHA(commitSHA))
	}
	if int64(len(decoded)) > limit {
		return archive{}, &PackageTooLargeError{
			Repository: repository, CommitSHA: commitSHA,
			Bytes: int64(len(decoded)), Limit: limit,
		}
	}
	return archive{Bytes: decoded, CommitSHA: commitSHA}, nil
}

// parseTooLarge reads the reader's marker and the size beside it.
func parseTooLarge(stderr string) (int64, bool) {
	index := strings.Index(stderr, tooLargeMarker)
	if index < 0 {
		return 0, false
	}
	fields := strings.Fields(stderr[index+len(tooLargeMarker):])
	if len(fields) == 0 {
		return 0, true
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, true
	}
	return size, true
}

// firstLine keeps a diagnostic to one line. git writes its refusals on the first
// one, unlike a Python traceback, which is why lastLines is not the rule here.
func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if index := strings.IndexByte(trimmed, '\n'); index > 0 {
		trimmed = trimmed[:index]
	}
	if len(trimmed) > 400 {
		trimmed = trimmed[:400] + "…"
	}
	if trimmed == "" {
		return "no diagnostic"
	}
	return trimmed
}

// packagePath is where in the pod the archive is staged.
//
// Outside the workspace on purpose: inside the checkout it would make the working
// copy dirty and so make the next launch refuse itself, and elsewhere in the
// workspace it would show up in the Code pane's tree as a file the developer did
// not create (D14 is about every file being theirs, which cuts both ways).
func packagePath(submissionID string) string {
	return "/tmp/ode-experiment-" + submissionID + ".zip"
}

// lastLine is the last non-empty line of some output.
//
// The counterpart of firstLine, and the two exist because git and Python put their
// answer at opposite ends. git writes its refusal on the first line; a Python
// traceback *ends* with the exception and begins with the word "Traceback", so
// firstLine on one produces "Traceback (most recent call last):" and nothing else —
// a message that says only that something went wrong, which is what every failure
// message already says.
func lastLine(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if trimmed := strings.TrimSpace(lines[index]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
