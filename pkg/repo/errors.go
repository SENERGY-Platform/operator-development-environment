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
	"errors"
	"fmt"
)

// ErrInvalidRequest is a request ODE refused to make. Mirrors the same name in
// pkg/timeseries and pkg/kernel, so the API layer has one shape to switch on.
var ErrInvalidRequest = errors.New("invalid repository request")

// ErrNotConnected means this developer has no GitHub token stored. The answer is
// "run the OAuth flow", not "something is broken", and the SPA shows the connect
// card rather than an error.
var ErrNotConnected = errors.New("this developer has not connected a GitHub account")

// ErrNoRepository means no repository is selected yet.
var ErrNoRepository = errors.New("no repository is selected for this developer")

// ErrNotCloned means the working copy is not on the PVC. Distinguished from
// ErrNoRepository because the repair differs: one needs a selection, the other a
// clone.
var ErrNotCloned = errors.New("the working copy is not present in the workspace")

// ErrDirty is a working copy with uncommitted changes where the operation would
// have destroyed them. §5.11 item 6: never silently reset.
var ErrDirty = errors.New("the working copy has uncommitted changes")

// ErrRemoteMismatch is a working copy whose origin is not the repository the
// developer has selected.
//
// Its own error rather than a GitError because nothing failed: git would happily
// commit and push, and the result would be the developer's work in a repository
// they did not choose. The repair is a decision only they can take — push to the
// remote the checkout actually has, or move the directory aside and clone the
// selected repository — so ODE stops and says which two repositories are involved.
var ErrRemoteMismatch = errors.New(
	"the working copy points at a different repository than the one selected")

// ErrNothingToCommit is git's "nothing to commit" as a value. Not a failure of
// ODE and not a failure of the developer, so the API layer answers 409 with the
// fact rather than 500 with git's wording.
var ErrNothingToCommit = errors.New("there is nothing to commit")

// ErrDraftsUnavailable means this deployment has no LLM provider, so a commit
// message cannot be drafted. Its own value rather than a generic failure because
// nothing is wrong: the repository surface is served without one, and the answer
// is "write the message yourself", which is what the SPA says.
var ErrDraftsUnavailable = errors.New("no LLM provider is configured to draft a commit message")

// ErrDraftFailed is a provider that answered with nothing usable. Distinguished
// from a transport failure because retrying is the sensible response to one and
// not to the other.
var ErrDraftFailed = errors.New("the commit message draft came back empty")

// ErrCredentialRejected means GitHub would not accept the credential ODE holds for
// this developer: git could not authenticate, and the API confirmed the token is no
// longer good.
//
// Separate from ErrNotConnected because the stored row is still there — nothing is
// missing, something has gone stale — and separate from GitError because the repair
// is neither "look at git" nor "retry": the developer reconnects their GitHub
// account, and only they can do that. It is the answer a token that was revoked,
// expired, or had its grant withdrawn produces, and until it existed all three
// arrived as a 502 quoting git.
var ErrCredentialRejected = errors.New(
	"GitHub rejected the stored credential; reconnect the GitHub account")

// GitError is a git command that ran and refused.
//
// It carries what git said because that is the only useful diagnosis: a rejected
// push, a merge conflict and an expired token all arrive this way and read
// completely differently. Stdout as well as stderr, because git splits its
// reporting across both depending on the subcommand.
type GitError struct {
	// Args is the command as it ran, with no credential in it — see authEnv.
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
	// Hint is what ODE knows about this failure that git does not, added by the
	// service where it has checked something git could not. Empty on almost every
	// GitError; set on the one that needs it, which is an authentication failure
	// with a credential the API says is still good — because then the fault is in
	// the pod and nothing about git's own text says so.
	Hint string
}

func (e *GitError) Error() string {
	message := firstNonEmpty(e.Stderr, e.Stdout)
	if e.TimedOut {
		return fmt.Sprintf("git %s timed out: %s", e.Args[0], message)
	}
	return fmt.Sprintf("git %s failed (exit %d): %s", e.Args[0], e.ExitCode, message)
}

// UpstreamError is GitHub's own verdict, kept whole for the reason
// kernel.UpstreamError is: a 401 from the API means the developer's token is
// gone, a 403 means a scope or a rate limit, and a 404 on a repository they can
// see means the token lost `repo`. Flattening them to 500 would lose all three.
type UpstreamError struct {
	Resource string
	Code     int
	Message  string
	Err      error
}

func (e *UpstreamError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("github: %s: request failed: %v", e.Resource, e.Err)
	}
	return fmt.Sprintf("github: %s: returned %d: %s", e.Resource, e.Code, e.Message)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
