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

// In-package tests for the parts a caller cannot reach: the porcelain parser, the
// path rules, the credential envelope and the OAuth state. The end-to-end
// behaviour is in the _test package beside this file.
package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

func TestParseStatusReadsBranchDivergenceAndChanges(t *testing.T) {
	// Real porcelain v2 output: a branch with an upstream, two commits ahead and one
	// behind, a staged modification, an unstaged deletion, a rename and an untracked
	// file — including a path with a space in it, which is what breaks a parser that
	// splits on whitespace.
	output := strings.Join([]string{
		"# branch.oid 6f1c0e1f0f4b4c5d6e7f8a9b0c1d2e3f40516273",
		"# branch.head main",
		"# branch.upstream origin/main",
		"# branch.ab +2 -1",
		"1 M. N... 100644 100644 100644 aaaa bbbb op.py",
		"1 .D N... 100644 100644 000000 cccc dddd notes/old notes.md",
		"2 R. N... 100644 100644 100644 eeee ffff R100 training.py\ttrain.py",
		"? scratch.py",
		"! ignored.pyc",
	}, "\n")

	status := parseStatus(output)
	if status.Branch != "main" || status.Upstream != "origin/main" {
		t.Errorf("branch = %q upstream = %q", status.Branch, status.Upstream)
	}
	if status.Ahead != 2 || status.Behind != 1 {
		t.Errorf("ahead = %d behind = %d, want 2 and 1", status.Ahead, status.Behind)
	}
	if status.Unborn || status.Detached {
		t.Errorf("unborn = %v detached = %v, want neither", status.Unborn, status.Detached)
	}
	if status.Head != "6f1c0e1f0f4b4c5d6e7f8a9b0c1d2e3f40516273" {
		t.Errorf("head = %q", status.Head)
	}

	// The ignored file is deliberately absent: it is not a change a developer has to
	// decide about, and listing it would bury the ones that are.
	if len(status.Changes) != 4 {
		t.Fatalf("changes = %+v, want four", status.Changes)
	}
	byPath := map[string]Change{}
	for _, change := range status.Changes {
		byPath[change.Path] = change
	}
	if change := byPath["op.py"]; change.Kind != "modified" || !change.Staged || change.Unstaged {
		t.Errorf("op.py = %+v, want a staged modification", change)
	}
	if change := byPath["notes/old notes.md"]; change.Kind != "deleted" || change.Staged ||
		!change.Unstaged {
		t.Errorf("the path with a space = %+v, want an unstaged deletion", change)
	}
	if change := byPath["training.py"]; change.Kind != "renamed" || change.RenamedFrom != "train.py" {
		t.Errorf("the rename = %+v, want training.py from train.py", change)
	}
	if change := byPath["scratch.py"]; change.Kind != "untracked" || change.Staged {
		t.Errorf("scratch.py = %+v, want an untracked file", change)
	}
}

func TestParseStatusReadsAnUnbornBranchAndADetachedHead(t *testing.T) {
	unborn := parseStatus("# branch.oid (initial)\n# branch.head main\n")
	if !unborn.Unborn || unborn.Branch != "main" || unborn.Head != "" {
		t.Errorf("unborn status = %+v", unborn)
	}
	detached := parseStatus("# branch.oid abc123\n# branch.head (detached)\n")
	if !detached.Detached || detached.Branch != "" {
		t.Errorf("detached status = %+v", detached)
	}
}

func TestCommitSummaryReadsTheFileCount(t *testing.T) {
	cases := map[string]int{
		"[main (root-commit) 9f8e7d6] Scaffold\n 11 files changed, 402 insertions(+)": 11,
		"[main 1a2b3c4] One thing\n 1 file changed, 2 insertions(+), 1 deletion(-)":   1,
		"nothing recognisable": 0,
	}
	for output, want := range cases {
		if got := commitSummary(output); got != want {
			t.Errorf("commitSummary(%q) = %d, want %d", output, got, want)
		}
	}
}

func TestParseHeadSplitsTheRecordOnAUnitSeparator(t *testing.T) {
	// A subject with a colon and a dash in it, because those are what a naive
	// separator would have used.
	sha, subject, date := parseHead("abc123\x1ffix(profiler): bound the raw pass\x1f2026-08-20T10:00:00+02:00")
	if sha != "abc123" || subject != "fix(profiler): bound the raw pass" ||
		date != "2026-08-20T10:00:00+02:00" {
		t.Errorf("parseHead = %q %q %q", sha, subject, date)
	}
}

func TestAuthEnvKeepsTheCredentialOutOfTheCommandLine(t *testing.T) {
	git := gitContext{token: "gho_secret", webURL: "https://github.com"}
	environment := git.authEnv()

	if environment["GIT_TERMINAL_PROMPT"] != "0" {
		t.Error("a git that wants a password would hang rather than fail")
	}
	if environment["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraheader" {
		t.Errorf("config key = %q", environment["GIT_CONFIG_KEY_0"])
	}
	// The header is the credential, base64 of x-access-token:<token>. What matters
	// is that the token appears nowhere in plain form and nowhere in argv, which is
	// what the value being in the environment buys.
	if strings.Contains(environment["GIT_CONFIG_VALUE_0"], "gho_secret") {
		t.Error("the token is in the environment in plain text")
	}
	if !strings.HasPrefix(environment["GIT_CONFIG_VALUE_0"], "AUTHORIZATION: basic ") {
		t.Errorf("header = %q", environment["GIT_CONFIG_VALUE_0"])
	}

	// Without a token there is no configuration override at all, so a public clone
	// does not carry an empty credential.
	anonymous := gitContext{webURL: "https://github.com"}.authEnv()
	if _, present := anonymous["GIT_CONFIG_COUNT"]; present {
		t.Error("an anonymous command carries a credential override")
	}
}

func TestRelativePathRefusesWhatWouldLeaveTheRepository(t *testing.T) {
	for _, path := range []string{
		"", "   ", "/etc/passwd", "..", "../op.py", "a/../../op.py",
		".git/config", "src/.git/hooks/pre-commit",
	} {
		if clean, err := relativePath(path); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("relativePath(%q) = %q, %v; want a refusal", path, clean, err)
		}
	}
	for path, want := range map[string]string{
		"op.py":                       "op.py",
		"./op.py":                     "op.py",
		"tests/test_op.py":            "tests/test_op.py",
		".github/workflows/build.yml": ".github/workflows/build.yml",
		"a/b/../c.py":                 "a/c.py",
	} {
		clean, err := relativePath(path)
		if err != nil || clean != want {
			t.Errorf("relativePath(%q) = %q, %v; want %q", path, clean, err, want)
		}
	}
}

func TestValidBranchRefusesWhatWouldHaveToBeEscaped(t *testing.T) {
	for _, branch := range []string{"", "-force", "a b", "feature/../x", "with;semicolon", "a/"} {
		if validBranch(branch) {
			t.Errorf("validBranch(%q) = true", branch)
		}
	}
	for _, branch := range []string{"main", "feature/pv-forecast", "release-1.2", "v2_x"} {
		if !validBranch(branch) {
			t.Errorf("validBranch(%q) = false", branch)
		}
	}
}

func TestCheckoutPathIsStableAndSafe(t *testing.T) {
	// Owner and name are one segment each, and neither may contribute a separator
	// or a traversal: the path is joined onto the workspace root in the pod.
	cases := []struct{ owner, name, want string }{
		{"jonah", "pv-forecast", "jonah/pv-forecast"},
		{"institut", "pv-forecast", "institut/pv-forecast"},
		{"SENERGY Platform", "PV Forecast", "SENERGY-Platform/PV-Forecast"},
		{"..", "../escape", "repository/escape"},
		{"a/b", "my/operator", "a-b/my-operator"},
		{"jonah", "operator.git", "jonah/operator.git"},
		// No owner is the shape a checkout had before the owner was part of the
		// path, and it stays reachable so an adopted directory can be named.
		{"", "pv-forecast", "pv-forecast"},
	}
	for _, one := range cases {
		if got := checkoutPath(one.owner, one.name); got != one.want {
			t.Errorf("checkoutPath(%q, %q) = %q, want %q", one.owner, one.name, got, one.want)
		}
	}
}

func TestImageReferenceIsLowerCased(t *testing.T) {
	if got := imageReference("SENERGY-Platform/PV-Forecast"); got !=
		"ghcr.io/senergy-platform/pv-forecast" {
		t.Errorf("imageReference = %q", got)
	}
}

func TestSameRemoteIgnoresTheDifferencesThatAreNotOne(t *testing.T) {
	if !sameRemote("https://github.com/jonah/op.git", "https://github.com/jonah/op") {
		t.Error("the .git suffix was read as a different repository")
	}
	if sameRemote("https://github.com/jonah/other", "https://github.com/jonah/op") {
		t.Error("two different repositories compared equal")
	}
}

func TestMissingScopesUnderstandsThatRepoImpliesItsChildren(t *testing.T) {
	if missing := missingScopes([]string{"repo", "workflow"},
		[]string{"repo", "repo:status", "workflow"}); len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if missing := missingScopes([]string{"repo", "workflow"}, []string{"repo"}); len(missing) != 1 ||
		missing[0] != "workflow" {
		t.Errorf("missing = %v, want workflow", missing)
	}
	// The narrower grant a developer can pick on the consent screen: public_repo
	// does not imply repo, and reporting it as sufficient would move the failure to
	// the first push against a private repository.
	if missing := missingScopes([]string{"repo"}, []string{"public_repo"}); len(missing) != 1 {
		t.Errorf("missing = %v, want repo", missing)
	}
}

// A complete grant has to yield an empty slice, not nil. pgx writes a nil slice as
// SQL NULL and ode_github_identities.missing_scopes is TEXT[] NOT NULL, so a nil
// here failed the insert for every developer who granted everything ODE asked for.
func TestMissingScopesIsEmptyRatherThanNilWhenNothingIsMissing(t *testing.T) {
	missing := missingScopes([]string{"repo", "workflow"}, []string{"repo", "workflow"})
	if missing == nil {
		t.Fatal("missing = nil, want an empty slice: a nil reaches postgres as NULL")
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

func TestSealedTokensRoundTripAndRefuseTampering(t *testing.T) {
	sealer, err := NewSealer("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	sealed, err := sealer.Seal("gho_token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "gho_token") {
		t.Fatal("the sealed value contains the token")
	}
	opened, err := sealer.Open(sealed)
	if err != nil || opened != "gho_token" {
		t.Fatalf("Open = %q, %v", opened, err)
	}

	// Sealing twice must not produce the same row, or the store would leak which
	// developers hold the same token.
	again, _ := sealer.Seal("gho_token")
	if again == sealed {
		t.Error("two sealings produced the same ciphertext")
	}

	// A tampered ciphertext and a wrong key both have to fail rather than return
	// something plausible.
	if _, err := sealer.Open(sealed[:len(sealed)-4] + "AAAA"); err == nil {
		t.Error("a tampered token was accepted")
	}
	other, err := NewSealer("ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Error("a token sealed under another key was accepted")
	}
}

func TestNewSealerRefusesAKeyThatIsNotOne(t *testing.T) {
	for _, key := range []string{"", "not base64!", "c2hvcnQ="} {
		if _, err := NewSealer(key); err == nil {
			t.Errorf("NewSealer(%q) was accepted", key)
		}
	}
	// URL-safe base64 without padding is the other spelling an operator will paste.
	if _, err := NewSealer("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"); err != nil {
		t.Errorf("a raw URL-safe key was refused: %v", err)
	}
}

func TestOAuthStateIsSingleUseBoundToTheUserAndExpires(t *testing.T) {
	states := newStateStore(time.Minute)

	state, err := states.issue("user-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Another developer's browser cannot complete this flow.
	if err := states.consume(state, "user-2"); err == nil {
		t.Fatal("a state was accepted for the wrong user")
	}
	// And that attempt consumed it, so a wrong guess cannot be retried.
	if err := states.consume(state, "user-1"); err == nil {
		t.Fatal("a state survived a failed attempt")
	}

	fresh, _ := states.issue("user-1")
	if err := states.consume(fresh, "user-1"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := states.consume(fresh, "user-1"); err == nil {
		t.Fatal("a state was accepted twice")
	}

	expiring := newStateStore(-time.Second)
	stale, _ := expiring.issue("user-1")
	if err := expiring.consume(stale, "user-1"); err == nil {
		t.Fatal("an expired state was accepted")
	}
}

func TestLanguageOfNamesWhatTheEditorNeeds(t *testing.T) {
	cases := map[string]string{
		"op.py":                       "python",
		"operator.yaml":               "yaml",
		".github/workflows/build.yml": "yaml",
		"pyproject.toml":              "toml",
		"Dockerfile":                  "dockerfile",
		"README.md":                   "markdown",
		"LICENSE":                     "plaintext",
	}
	for name, want := range cases {
		if got := languageOf(name); got != want {
			t.Errorf("languageOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// stubWorkspace is a pod that answers one canned command result.
//
// Only Command is implemented: the credential tests drive gitContext, and every
// other method of the interface would be a way for a future test to reach the
// pod without saying so.
type stubWorkspace struct {
	result kernel.CommandResult
}

func (s *stubWorkspace) Command(context.Context, kernel.Ref, kernel.Command) (kernel.CommandResult, error) {
	return s.result, nil
}

func (s *stubWorkspace) CommandBatch(
	context.Context, kernel.Ref, []kernel.Command,
) ([]kernel.CommandResult, error) {
	return []kernel.CommandResult{s.result}, nil
}

func (s *stubWorkspace) Tree(context.Context, kernel.Ref, kernel.TreeRequest) (kernel.Node, error) {
	panic("the credential tests do not read a tree")
}

func (s *stubWorkspace) ReadFile(context.Context, kernel.Ref, string, int) (kernel.FileContent, error) {
	panic("the credential tests do not read a file")
}

func (s *stubWorkspace) WriteFile(context.Context, kernel.Ref, string, []byte) (kernel.Node, error) {
	panic("the credential tests do not write a file")
}

func (s *stubWorkspace) MakeDir(context.Context, kernel.Ref, string) (kernel.Node, error) {
	panic("the credential tests do not create a directory")
}

func (s *stubWorkspace) Remove(context.Context, kernel.Ref, string, bool) error {
	panic("the credential tests do not remove anything")
}

func (s *stubWorkspace) Workspace() string { return "data/ode" }

// A GitError travels: it is the text of the 502 the API layer answers with, and
// it outlives the request in whatever collects that. So no form of the credential
// may be in it — and the form that can actually appear is the encoded one, since
// that is what ODE hands git in GIT_CONFIG_VALUE_0. Stdout as well as stderr,
// because Error() falls back to Stdout when stderr is empty.
func TestAGitFailureCarriesNoFormOfTheCredential(t *testing.T) {
	const token = "gho_testtoken0123456789"
	encoded := encodedCredential(token)

	workspace := &stubWorkspace{result: kernel.CommandResult{
		ExitCode: 128,
		Stdout:   "fatal: could not read Username for 'https://github.com': " + encoded,
		Stderr:   "fatal: unable to access 'https://github.com/o/n.git/': " + encoded + " " + token,
	}}
	git := gitContext{workspace: workspace, token: token, webURL: "https://github.com"}

	_, err := git.run(context.Background(), "push", "--set-upstream", "origin", "HEAD:refs/heads/main")
	var failure *GitError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want a GitError", err)
	}
	for label, text := range map[string]string{
		"stderr":  failure.Stderr,
		"stdout":  failure.Stdout,
		"message": failure.Error(),
	} {
		if strings.Contains(text, encoded) {
			t.Errorf("the encoded credential survived into the %s: %s", label, text)
		}
		if strings.Contains(text, token) {
			t.Errorf("the token survived into the %s: %s", label, text)
		}
	}
}
