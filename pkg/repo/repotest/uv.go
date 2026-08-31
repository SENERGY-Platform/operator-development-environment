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

package repotest

// uv, as a test may have it.
//
// A scaffold ends by running `uv lock` in the pod, and every harness that runs
// commands through a real python3 would otherwise run the machine's real uv — which
// resolves the Operator Lib pin over the network, from a git source. That is a test
// that fails on a developer's train and passes at their desk, and it tests uv rather
// than ODE. So a harness that scaffolds installs one of these instead.
//
// It lives here rather than in pkg/repo's own harness because four packages
// scaffold: pkg/repo, pkg/api, pkg/experiments and pkg/interpret. The one that
// forgets is the one that goes to the network.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LockingUV writes a lock file, which is the only part of uv's behaviour a scaffold
// depends on.
const LockingUV = `#!/bin/sh
[ "$1" = "lock" ] || { echo "unexpected: $*" >&2; exit 2; }
printf 'version = 1\nrequires-python = "==3.10.*"\n' > uv.lock
`

// FailingUV refuses the way a pin that no longer resolves does: it says what it was
// doing and then why it stopped, which is the shape the reported reason is cut to.
const FailingUV = `#!/bin/sh
echo "  Resolved 0 packages in 12ms" >&2
echo "error: Git operation failed for the operator-lib source" >&2
exit 1
`

// StubUV puts a uv on PATH for the duration of the test.
func StubUV(t testing.TB, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uv"), []byte(script), 0o755); err != nil {
		t.Fatalf("write uv stub: %v", err)
	}
	// Prepended rather than replacing PATH: the cell is executed by a real python3
	// that has to stay findable, and so does git.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// WithoutUV takes uv off PATH and leaves everything else. It is the pod of an image
// built before uv was added to it, which is the case a scaffold has to survive.
func WithoutUV(t testing.TB) {
	t.Helper()
	entries := filepath.SplitList(os.Getenv("PATH"))
	kept := make([]string, 0, len(entries))
	for _, entry := range entries {
		if info, err := os.Stat(filepath.Join(entry, "uv")); err == nil && !info.IsDir() {
			continue
		}
		kept = append(kept, entry)
	}
	t.Setenv("PATH", strings.Join(kept, string(os.PathListSeparator)))
}
