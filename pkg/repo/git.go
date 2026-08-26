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

package repo

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// git, run in the developer's pod.
//
// Three details in here are the ones worth reviewing.
//
//   - **The credential is in the environment, per command.** Not in the remote
//     URL, which would write the token into .git/config on the PVC and leave it
//     there; not in `-c`, which would put it in the pod's own `ps` output.
//     GIT_CONFIG_COUNT is git's documented way to pass configuration through the
//     environment, and the http.extraheader it sets is what actions/checkout uses
//     for the same reason.
//
//   - **GIT_TERMINAL_PROMPT=0.** Without it, a git that wants credentials waits
//     for a terminal that does not exist, and the operation hangs until the
//     command timeout instead of failing with "authentication failed".
//
//   - **The author is passed with `-c`, not written to the repository's config.**
//     Nothing ODE does should leave configuration behind in a developer's working
//     copy that they did not put there (D14).

// gitContext is git in one place, for one developer.
type gitContext struct {
	workspace Workspace
	bearer    string
	// dir is the checkout, relative to the workspace. Empty runs in the workspace
	// root, which is what a clone needs.
	dir    string
	token  string
	webURL string
	// template carries the bounds every git command inherits — its timeout and its
	// output cap — so the two are configured once rather than per call site.
	template kernel.Command
}

// authEnv is the credential and the two settings that make a failure a failure
// rather than a hang.
func (g gitContext) authEnv() map[string]string {
	environment := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		// Belt and braces: an askpass helper that answers nothing is what stops a
		// build of git compiled with a graphical prompt from opening one.
		"GIT_ASKPASS": "/bin/true",
	}
	if g.token == "" {
		return environment
	}
	host := strings.TrimSuffix(g.webURL, "/") + "/"
	environment["GIT_CONFIG_COUNT"] = "1"
	environment["GIT_CONFIG_KEY_0"] = "http." + host + ".extraheader"
	environment["GIT_CONFIG_VALUE_0"] = "AUTHORIZATION: basic " + encodedCredential(g.token)
	return environment
}

// encodedCredential is the token in the form git is actually given: basic
// authentication over "x-access-token:<token>", which is what actions/checkout
// sends and what GitHub accepts in place of a password.
//
// Named rather than inlined because redact has to remove exactly this string, and
// two independent spellings of it would drift the day one of them changes.
func encodedCredential(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// run executes git and fails on a non-zero exit.
func (g gitContext) run(ctx context.Context, args ...string) (kernel.CommandResult, error) {
	result, err := g.attempt(ctx, args...)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 || result.TimedOut {
		return result, g.failure(args, result)
	}
	return result, nil
}

// failure is a command that refused, as an error with no credential in it.
//
// Every call site that builds a GitError goes through here: the redaction is easy
// to forget on one of them, and the one that forgets is the one that leaks.
func (g gitContext) failure(args []string, result kernel.CommandResult) *GitError {
	return &GitError{
		Args:     args,
		ExitCode: result.ExitCode,
		Stdout:   redact(result.Stdout, g.token),
		Stderr:   redact(result.Stderr, g.token),
		TimedOut: result.TimedOut,
	}
}

// runAll executes several git commands under one claim on the kernel, stopping
// at the first that refuses.
//
// The single claim is the reason it exists rather than a loop over run: between
// two claims the developer's own cell can take the kernel, and the second command
// then comes back busy with the first already done. For `git reset --hard`
// followed by `git clean` that is a working copy half destroyed under an answer
// that reads as "nothing happened".
//
// The commands stay argv lists the whole way — one kernel.Command each, no shell
// anywhere — so batching costs none of what the comment on kernel.Command
// promises about a commit message from an HTTP request.
func (g gitContext) runAll(ctx context.Context, argvs ...[]string) ([]kernel.CommandResult, error) {
	commands := make([]kernel.Command, 0, len(argvs))
	for _, argv := range argvs {
		command := g.template
		command.Argv = append([]string{"git"}, argv...)
		command.Dir = g.dir
		command.Env = g.authEnv()
		commands = append(commands, command)
	}
	return g.workspace.CommandBatch(ctx, g.bearer, commands)
}

// batchFailure is the first command of a batch that refused, as an error.
func (g gitContext) batchFailure(argvs [][]string, results []kernel.CommandResult) error {
	for index, result := range results {
		if result.ExitCode != 0 || result.TimedOut {
			return g.failure(argvs[index], result)
		}
	}
	if len(results) < len(argvs) {
		// The helper stops only on a failure, so a short answer with no failure in
		// it means the batch did not run as sent. Reported rather than read as
		// success, because the caller's next step assumes the sequence completed.
		return fmt.Errorf("git %s: %d of %d commands ran and none reported a failure",
			argvs[0][0], len(results), len(argvs))
	}
	return nil
}

// attempt executes git and reports a non-zero exit as a result rather than an
// error, for the commands where failing is an answer: `git status` in a directory
// that is not a repository, `git rev-parse HEAD` on an unborn branch.
func (g gitContext) attempt(ctx context.Context, args ...string) (kernel.CommandResult, error) {
	command := g.template
	command.Argv = append([]string{"git"}, args...)
	command.Dir = g.dir
	command.Env = g.authEnv()
	return g.workspace.Command(ctx, g.bearer, command)
}

// redact removes the credential from text, in both the forms it exists in.
//
// The encoded form is the one that can actually appear: what ODE hands git is
// base64 of "x-access-token:<token>" in GIT_CONFIG_VALUE_0, so that — and not the
// bare token — is what a git that quotes its own configuration back, or a helper
// that echoes its environment, would print. The plaintext form is redacted too
// because a remote's error text has been known to echo a URL, and neither costs
// anything to remove.
//
// A short token is left alone: below eight characters this would be replacing
// noise, and a credential that short is not one GitHub issued.
func redact(text, token string) string {
	if len(token) < 8 {
		return text
	}
	text = strings.ReplaceAll(text, encodedCredential(token), "[redacted]")
	return strings.ReplaceAll(text, token, "[redacted]")
}

// authorArgs is the `-c` pair that names the committer.
func authorArgs(author Author) []string {
	name := strings.TrimSpace(author.Name)
	if name == "" {
		name = "ODE developer"
	}
	email := strings.TrimSpace(author.Email)
	if email == "" {
		// A syntactically valid address that is obviously not deliverable, rather
		// than a plausible one that is somebody else's.
		email = "ode-developer@invalid"
	}
	return []string{"-c", "user.name=" + name, "-c", "user.email=" + email}
}

// clone puts the repository in the workspace. The remote URL carries no
// credential; authEnv does.
func (g gitContext) clone(ctx context.Context, cloneURL, path string) error {
	_, err := g.run(ctx, "clone", "--origin", "origin", cloneURL, path)
	return err
}

// setBranch points HEAD at a branch without touching the index.
//
// Used after cloning an empty repository, where git leaves HEAD on whatever the
// local init.defaultBranch says — often `master` while GitHub's default is `main`,
// and a first push to the wrong branch name leaves the repository with two.
// symbolic-ref rather than `checkout -b` because there is no commit to check out.
func (g gitContext) setBranch(ctx context.Context, branch string) error {
	if branch == "" {
		return nil
	}
	_, err := g.run(ctx, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return err
}

// originURL is the remote the working copy actually points at, empty when it has
// no origin at all. A checkout without one is not a failure of git — a developer
// with a terminal in the same pod can produce it — so it comes back as the empty
// string and the caller decides what it means.
func (g gitContext) originURL(ctx context.Context) (string, error) {
	result, err := g.attempt(ctx, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(result.Stdout), nil
}

// verifyRemote refuses a working copy whose origin is not the linked repository.
//
// Read from the checkout rather than trusted from the link, because the link is
// ODE's record of what the developer chose and the origin is where git would
// actually send the work. When those two disagree the one that decides is git's,
// which is why this is checked before a commit and before a push rather than only
// reported in the status.
func (g gitContext) verifyRemote(ctx context.Context, link Link) error {
	origin, err := g.originURL(ctx)
	if err != nil {
		return err
	}
	if sameRemote(origin, link.CloneURL) {
		return nil
	}
	if origin == "" {
		return fmt.Errorf("%w: the working copy at %s has no origin, while %s is selected",
			ErrRemoteMismatch, link.Path, link.FullName)
	}
	return fmt.Errorf("%w: the working copy at %s has origin %s, while %s is selected",
		ErrRemoteMismatch, link.Path, origin, link.FullName)
}

// isRepository reports whether dir is a git working copy.
func (g gitContext) isRepository(ctx context.Context) (bool, error) {
	result, err := g.attempt(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		// A missing directory is not a failure of git but a fact about the PVC, and
		// the caller's answer to it is to clone.
		if errors.Is(err, kernel.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == "true", nil
}

// gitStatus is the parsed `git status --porcelain=v2 --branch`.
type gitStatus struct {
	Branch   string
	Upstream string
	Ahead    int
	Behind   int
	Detached bool
	Unborn   bool
	Head     string
	Changes  []Change
}

// parseStatus reads porcelain v2.
//
// v2 rather than v1 because v1 does not carry the ahead/behind counts, and the
// divergence report of §5.11 item 5 is exactly those two numbers. The format is
// documented and stable, which is the reason to parse it rather than to parse
// human-readable output that changes with git's locale.
func parseStatus(output string) gitStatus {
	status := gitStatus{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			value := strings.TrimPrefix(line, "# branch.oid ")
			if value == "(initial)" {
				status.Unborn = true
			} else {
				status.Head = value
			}
		case strings.HasPrefix(line, "# branch.head "):
			value := strings.TrimPrefix(line, "# branch.head ")
			if value == "(detached)" {
				status.Detached = true
			} else {
				status.Branch = value
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			status.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			status.Ahead, status.Behind = parseAheadBehind(strings.TrimPrefix(line, "# branch.ab "))
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			if change, ok := parseTrackedEntry(line); ok {
				status.Changes = append(status.Changes, change)
			}
		case strings.HasPrefix(line, "u "):
			if path := lastField(line, 10); path != "" {
				status.Changes = append(status.Changes, Change{
					Path: path, Kind: "unmerged", Staged: true, Unstaged: true,
				})
			}
		case strings.HasPrefix(line, "? "):
			status.Changes = append(status.Changes, Change{
				Path: strings.TrimPrefix(line, "? "), Kind: "untracked", Unstaged: true,
			})
		}
	}
	return status
}

// parseAheadBehind reads "+2 -1".
func parseAheadBehind(value string) (int, int) {
	var ahead, behind int
	for _, field := range strings.Fields(value) {
		if len(field) < 2 {
			continue
		}
		number, err := strconv.Atoi(field[1:])
		if err != nil {
			continue
		}
		switch field[0] {
		case '+':
			ahead = number
		case '-':
			behind = number
		}
	}
	return ahead, behind
}

// parseTrackedEntry reads a "1" (ordinary) or "2" (renamed/copied) entry.
//
//	1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
//	2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\t<origPath>
//
// A path may contain spaces, which is why the fields are counted rather than
// split: everything after the eighth (or ninth) field is the path.
func parseTrackedEntry(line string) (Change, bool) {
	renamed := strings.HasPrefix(line, "2 ")
	fields := 8
	if renamed {
		fields = 9
	}
	parts := strings.SplitN(line, " ", fields+1)
	if len(parts) < fields+1 {
		return Change{}, false
	}
	codes := parts[1]
	if len(codes) < 2 {
		return Change{}, false
	}
	path := parts[fields]
	change := Change{
		Staged:   codes[0] != '.',
		Unstaged: codes[1] != '.',
		Kind:     changeKind(codes),
	}
	if renamed {
		// The tab separator is the documented one in the non-NUL format.
		if current, original, found := strings.Cut(path, "\t"); found {
			change.Path, change.RenamedFrom = current, original
		} else {
			change.Path = path
		}
	} else {
		change.Path = path
	}
	return change, change.Path != ""
}

// changeKind names git's XY codes. The staged side wins when both are set,
// because that is what a commit would record.
func changeKind(codes string) string {
	for _, code := range []byte{codes[0], codes[1]} {
		switch code {
		case 'A':
			return "added"
		case 'D':
			return "deleted"
		case 'R':
			return "renamed"
		case 'C':
			return "copied"
		case 'T':
			return "typechange"
		case 'M':
			return "modified"
		}
	}
	return "modified"
}

// lastField returns everything after the first count space-separated fields.
func lastField(line string, count int) string {
	parts := strings.SplitN(line, " ", count+1)
	if len(parts) < count+1 {
		return ""
	}
	return parts[count]
}

// commitSummary reads the file count out of `git commit`'s own summary line,
// which reads " 7 files changed, 214 insertions(+)".
func commitSummary(output string) int {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, marker := range []string{" files changed", " file changed"} {
			if index := strings.Index(trimmed, marker); index > 0 {
				if count, err := strconv.Atoi(strings.TrimSpace(trimmed[:index])); err == nil {
					return count
				}
			}
		}
	}
	return 0
}

// headDescription reads the current commit's sha, subject and date in one call.
// Separated by a unit that cannot occur in a subject.
const headFormat = "%H%x1f%s%x1f%cI"

func parseHead(output string) (sha, subject, date string) {
	parts := strings.Split(strings.TrimSpace(output), "\x1f")
	if len(parts) < 3 {
		return strings.TrimSpace(output), "", ""
	}
	return parts[0], parts[1], parts[2]
}

// pushRefspec is what a push should name.
//
// `HEAD:refs/heads/<branch>` rather than a bare branch name, because a first push
// from a clone of an empty repository has no upstream to infer one from, and
// naming the destination explicitly means the same command works before and after
// there is one.
func pushRefspec(branch string) (string, error) {
	if !validBranch(branch) {
		return "", fmt.Errorf("%w: %q is not a usable branch name", ErrInvalidRequest, branch)
	}
	return "HEAD:refs/heads/" + branch, nil
}

// validBranch is stricter than git's own rules on purpose: a name ODE would have
// to escape is better refused. It is the same reasoning as the Hub username
// pattern in pkg/kernel.
func validBranch(branch string) bool {
	if branch == "" || len(branch) > 200 {
		return false
	}
	if strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") || strings.Contains(branch, "..") {
		return false
	}
	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/':
		default:
			return false
		}
	}
	return true
}
