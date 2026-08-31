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
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

/*
 * What a developer is told when git cannot authenticate.
 *
 * There are two of these and they look identical from git's side, which is the
 * problem being fixed. GitHub answers a push it will not authenticate with
 *
 *	remote: Invalid username or token. Password authentication is not supported…
 *	fatal: Authentication failed for 'https://github.com/owner/name.git/'
 *
 * whether the credential it received was a revoked token or nothing at all. The
 * repairs are opposites — reconnect the account, or look at the pod — so the answer
 * cannot be the same sentence twice. ODE asks GitHub's API the question git cannot,
 * and the two tests below are the two answers.
 */

// rejectsAuth is the pod with git's authentication failure staged for one
// subcommand. The remote in these tests is a file:// path that would happily accept
// a push, so the failure has to be injected: what is under test is what ODE does
// with git's report, not git.
type rejectsAuth struct {
	pod        repo.Workspace
	subcommand string
}

// githubRefusal is GitHub's own wording, verbatim from the report this fixes.
const githubRefusal = "remote: Invalid username or token. Password authentication is not " +
	"supported for Git operations.\n" +
	"fatal: Authentication failed for 'https://github.com/franzmueller/operator-test.git/'"

func (w *rejectsAuth) refuses(argv []string) bool {
	for _, argument := range argv {
		if argument == w.subcommand {
			return true
		}
	}
	return false
}

func (w *rejectsAuth) refusal() kernel.CommandResult {
	return kernel.CommandResult{ExitCode: 128, Stderr: githubRefusal}
}

func (w *rejectsAuth) Command(
	ctx context.Context, ref kernel.Ref, cmd kernel.Command,
) (kernel.CommandResult, error) {
	if w.refuses(cmd.Argv) {
		return w.refusal(), nil
	}
	return w.pod.Command(ctx, ref, cmd)
}

func (w *rejectsAuth) CommandBatch(
	ctx context.Context, ref kernel.Ref, cmds []kernel.Command,
) ([]kernel.CommandResult, error) {
	results := make([]kernel.CommandResult, 0, len(cmds))
	for _, cmd := range cmds {
		if w.refuses(cmd.Argv) {
			return append(results, w.refusal()), nil
		}
		result, err := w.pod.Command(ctx, ref, cmd)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (w *rejectsAuth) Tree(
	ctx context.Context, ref kernel.Ref, req kernel.TreeRequest,
) (kernel.Node, error) {
	return w.pod.Tree(ctx, ref, req)
}

func (w *rejectsAuth) ReadFile(
	ctx context.Context, ref kernel.Ref, path string, maxBytes int,
) (kernel.FileContent, error) {
	return w.pod.ReadFile(ctx, ref, path, maxBytes)
}

func (w *rejectsAuth) WriteFile(
	ctx context.Context, ref kernel.Ref, path string, content []byte,
) (kernel.Node, error) {
	return w.pod.WriteFile(ctx, ref, path, content)
}

func (w *rejectsAuth) MakeDir(
	ctx context.Context, ref kernel.Ref, path string,
) (kernel.Node, error) {
	return w.pod.MakeDir(ctx, ref, path)
}

func (w *rejectsAuth) Remove(
	ctx context.Context, ref kernel.Ref, path string, recursive bool,
) error {
	return w.pod.Remove(ctx, ref, path, recursive)
}

func (w *rejectsAuth) Workspace() string { return w.pod.Workspace() }

// The credential has gone: the developer revoked the grant on GitHub, or it
// expired. ODE's own store still says "connected", because that row records what
// was granted and not whether it still is — so the only way to know is to ask, and
// the answer has to be the repair rather than git's exit code.
func TestAPushRefusedByAStaleCredentialSaysToReconnect(t *testing.T) {
	h := newHarnessWith(t, func(pod repo.Workspace) repo.Workspace {
		return &rejectsAuth{pod: pod, subcommand: "push"}
	})
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")

	// Revoked after the connection was made, which is the real sequence.
	h.github.SetRevoked(true)

	_, err := h.service.Push(context.Background(), repo.PushRequest{Request: h.request()})
	if !errors.Is(err, repo.ErrCredentialRejected) {
		t.Fatalf("Push error = %v, want ErrCredentialRejected", err)
	}
	// git's own text survives inside it: the developer asked ODE to push and is
	// entitled to see what the remote said.
	if !strings.Contains(err.Error(), "Invalid username or token") {
		t.Errorf("the answer dropped git's report: %v", err)
	}
	// And it is not a GitError any more, because the API layer must not answer 502
	// for something nothing upstream broke on.
	var gitErr *repo.GitError
	if errors.As(err, &gitErr) {
		t.Error("a stale credential is still reported as a git failure")
	}
}

// The other one, and the reason the check is worth making at all. The credential
// works — the API accepts it — so git in the pod could not use what it was given,
// and "reconnect your account" would send the developer round a loop that cannot
// help. The answer stays git's, with the one thing ODE knows added to it.
func TestAPushRefusedWithAWorkingCredentialSaysWhereToLook(t *testing.T) {
	h := newHarnessWith(t, func(pod repo.Workspace) repo.Workspace {
		return &rejectsAuth{pod: pod, subcommand: "push"}
	})
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")

	_, err := h.service.Push(context.Background(), repo.PushRequest{Request: h.request()})
	if errors.Is(err, repo.ErrCredentialRejected) {
		t.Fatalf("Push error = %v, want git's own failure while the credential works", err)
	}
	var gitErr *repo.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("Push error = %v, want a GitError", err)
	}
	if gitErr.Hint == "" {
		t.Fatal("nothing distinguishes this from a revoked token, which is the whole bug")
	}
	// The hint has to name the two facts the developer cannot see: the credential is
	// fine, and git is where to look.
	for _, want := range []string{"still works", "git in the pod"} {
		if !strings.Contains(gitErr.Hint, want) {
			t.Errorf("the hint does not say %q: %s", want, gitErr.Hint)
		}
	}
}

// 403 is not a credential to replace. GitHub answers it for a rate limit and for a
// grant too narrow for the resource, and sending the developer through a consent
// screen for either is the loop this whole check exists to prevent — so it keeps
// git's own failure and says what GitHub actually said.
func TestAPushRefusedWhileGithubAnswers403IsNotAReconnect(t *testing.T) {
	h := newHarnessWith(t, func(pod repo.Workspace) repo.Workspace {
		return &rejectsAuth{pod: pod, subcommand: "push"}
	})
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")
	h.github.SetRateLimited(true)

	_, err := h.service.Push(context.Background(), repo.PushRequest{Request: h.request()})
	if errors.Is(err, repo.ErrCredentialRejected) {
		t.Fatal("a 403 was reported as a credential to replace")
	}
	var gitErr *repo.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("Push error = %v, want a GitError", err)
	}
	if !strings.Contains(gitErr.Hint, "rate limit") {
		t.Errorf("the hint does not carry GitHub's own message: %q", gitErr.Hint)
	}
}

// What GitHub says about the credential, asked out loud.
//
// The token's kind is the field worth having: a deployment that registered a GitHub
// App where it meant an OAuth app gets user tokens that expire within hours, which
// looks exactly like a developer revoking the grant every afternoon and is a
// deployment setting rather than anything ODE can repair.
func TestVerifyReportsWhatGithubSaysAboutTheCredential(t *testing.T) {
	h := newHarness(t)
	h.connect()

	report, err := h.service.Verify(context.Background(), testUserSub)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Valid || report.Login != "jonah" {
		t.Errorf("verification = %+v, want the credential accepted", report)
	}
	if !report.ScopesReported || len(report.Scopes) != 2 {
		t.Errorf("scopes = %v (reported %v), want the two GitHub sent",
			report.Scopes, report.ScopesReported)
	}
	// The kind, and nothing of the value: the double issues a gho_ token.
	if !strings.HasPrefix(report.Kind, "gho_") {
		t.Errorf("kind = %q, want the token's prefix and what it means", report.Kind)
	}
	if strings.Contains(report.Kind, "testtoken") || report.Length == 0 {
		t.Errorf("verification leaks the credential or lost its length: %+v", report)
	}

	// How long ODE has held it, which is the fact that says whether a reconnection
	// happened: a refused credential stored yesterday was never replaced.
	if report.Age == "" || report.StoredAt == "" || report.StoredLogin != "jonah" {
		t.Errorf("verification = %+v, want when the credential was stored and whose it is",
			report)
	}

	// And once revoked, the same call says so — which is the answer a developer who
	// has just reconnected needs, because it is the only one that distinguishes
	// "still broken" from "broken again".
	h.github.SetRevoked(true)
	report, err = h.service.Verify(context.Background(), testUserSub)
	if err != nil {
		t.Fatalf("Verify after revocation: %v", err)
	}
	if report.Valid || report.Code != 401 || !strings.Contains(report.Message, "Bad credentials") {
		t.Errorf("verification = %+v, want GitHub's refusal", report)
	}
}

// Everything else git refuses must come through untouched. A rejected non-fast
// forward, a bad refspec and a missing branch all exit 128 as well, and an
// explanation about credentials on top of one of those is worse than no explanation.
func TestAGitFailureThatIsNotAboutCredentialsIsLeftAlone(t *testing.T) {
	h := newHarnessWith(t, func(pod repo.Workspace) repo.Workspace {
		return &rejectsOther{pod: pod}
	})
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")

	_, err := h.service.Push(context.Background(), repo.PushRequest{Request: h.request()})
	var gitErr *repo.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("Push error = %v, want a GitError", err)
	}
	if errors.Is(err, repo.ErrCredentialRejected) || gitErr.Hint != "" {
		t.Errorf("a rejected push was explained as a credential problem: %+v", gitErr)
	}
	// And ODE did not spend a GitHub call on a failure that says nothing about the
	// credential. One /user call belongs to the connection itself; a second would be
	// this push asking a question it has no reason to ask.
	if viewers := countCalls(h.github.Calls(), "GET /user"); viewers != 1 {
		t.Errorf("/user was called %d times, want only the one the connection made", viewers)
	}
}

// rejectsOther stages a push failure that has nothing to do with authentication.
type rejectsOther struct{ pod repo.Workspace }

const rejectedPush = "! [rejected]        main -> main (non-fast-forward)\n" +
	"error: failed to push some refs to 'https://github.com/franzmueller/operator-test.git'"

func (w *rejectsOther) Command(
	ctx context.Context, ref kernel.Ref, cmd kernel.Command,
) (kernel.CommandResult, error) {
	for _, argument := range cmd.Argv {
		if argument == "push" {
			return kernel.CommandResult{ExitCode: 1, Stderr: rejectedPush}, nil
		}
	}
	return w.pod.Command(ctx, ref, cmd)
}

func (w *rejectsOther) CommandBatch(
	ctx context.Context, ref kernel.Ref, cmds []kernel.Command,
) ([]kernel.CommandResult, error) {
	return w.pod.CommandBatch(ctx, ref, cmds)
}

func (w *rejectsOther) Tree(
	ctx context.Context, ref kernel.Ref, req kernel.TreeRequest,
) (kernel.Node, error) {
	return w.pod.Tree(ctx, ref, req)
}

func (w *rejectsOther) ReadFile(
	ctx context.Context, ref kernel.Ref, path string, maxBytes int,
) (kernel.FileContent, error) {
	return w.pod.ReadFile(ctx, ref, path, maxBytes)
}

func (w *rejectsOther) WriteFile(
	ctx context.Context, ref kernel.Ref, path string, content []byte,
) (kernel.Node, error) {
	return w.pod.WriteFile(ctx, ref, path, content)
}

func (w *rejectsOther) MakeDir(
	ctx context.Context, ref kernel.Ref, path string,
) (kernel.Node, error) {
	return w.pod.MakeDir(ctx, ref, path)
}

func (w *rejectsOther) Remove(
	ctx context.Context, ref kernel.Ref, path string, recursive bool,
) error {
	return w.pod.Remove(ctx, ref, path, recursive)
}

func (w *rejectsOther) Workspace() string { return w.pod.Workspace() }

func countCalls(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}
