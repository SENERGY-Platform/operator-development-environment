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

// Package repo is ODE's GitHub integration and the Code pane's backend
// (SPEC §5.11, D9, D14, D15).
//
// Four properties shape everything in here.
//
//   - **The working copy is on the developer's PVC, not on ODE.** ODE has no
//     filesystem the developer's repository could live on that would survive a
//     pod restart, and §5.6 says session state that must survive belongs on the
//     PVC. So every git command and every file edit runs *in the developer's own
//     pod*, through kernel.Service's workspace surface. ODE stores two small
//     facts — which GitHub account, which repository — and nothing else.
//
//   - **No hidden files and no ODE-managed files (D14).** The file tree reports
//     `.github/workflows/build.yml` and `.gitignore` like any other file, and
//     they are writable. That is why the workspace surface runs in the kernel
//     rather than over jupyter_server's contents API, which hides dotfiles by
//     default.
//
//   - **Never a silent commit (§5.11 item 5).** ODE writes files. Staging,
//     committing and pushing are explicit developer actions with their own
//     routes. The scaffold is no exception: a created repository is empty on
//     GitHub until the developer commits, which is also why one code path serves
//     both "create a repository" and "work on an existing one".
//
//   - **The GitHub token is a second credential (§5.11 item 1).** It is not the
//     Keycloak token, it is not derived from it, and it is stored encrypted with
//     a key ODE never writes down. A deployment without that key does not serve
//     this package at all.
package repo

import (
	"context"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// Workspace is the developer's pod, as this package needs it. *kernel.Service
// implements it.
//
// An interface rather than the concrete service so that a test can drive the
// whole flow against a directory, and so that the dependency reads one way: repo
// operations happen in a workspace, and the workspace knows nothing about repos.
type Workspace interface {
	Command(ctx context.Context, bearer string, cmd kernel.Command) (kernel.CommandResult, error)
	// CommandBatch runs a sequence under one claim on the kernel. Needed because a
	// sequence run as separate commands can be refused between two of them, and
	// `git reset --hard` followed by a refused `git clean` is a working copy half
	// destroyed under an answer that says nothing happened (§5.11 item 6).
	CommandBatch(ctx context.Context, bearer string, cmds []kernel.Command) ([]kernel.CommandResult, error)
	Tree(ctx context.Context, bearer string, req kernel.TreeRequest) (kernel.Node, error)
	ReadFile(ctx context.Context, bearer, path string, maxBytes int) (kernel.FileContent, error)
	WriteFile(ctx context.Context, bearer, path string, content []byte) (kernel.Node, error)
	MakeDir(ctx context.Context, bearer, path string) (kernel.Node, error)
	Remove(ctx context.Context, bearer, path string, recursive bool) error
	// Workspace is the configured workspace path, reported so a developer can see
	// where on the PVC their checkout actually is.
	Workspace() string
}

// Author is who a commit is by.
//
// It comes from the developer's own platform token rather than from the GitHub
// account, so the commit says who did the work in the platform's terms. GitHub
// matches the email to an account by itself if it can.
type Author struct {
	Name  string
	Email string
	Sub   string
}

// Identity is the GitHub account ODE holds a token for.
//
// Scopes is what the token actually carries, read from GitHub's own response
// header rather than from what ODE asked for: a developer can narrow the grant on
// the consent screen, and the difference only shows up on the first push.
type Identity struct {
	Login       string    `json:"login"`
	Name        string    `json:"name,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	Scopes      []string  `json:"scopes,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	// MissingScopes names what the grant lacks of what §5.11 item 1 asks for. Empty
	// is the normal case; a non-empty list is reported to the developer up front
	// rather than discovered when a push is rejected.
	MissingScopes []string `json:"missing_scopes,omitempty"`
}

// Repository is one GitHub repository as the selection list needs it.
type Repository struct {
	FullName      string     `json:"full_name"`
	Name          string     `json:"name"`
	Owner         string     `json:"owner"`
	Description   string     `json:"description,omitempty"`
	Private       bool       `json:"private"`
	DefaultBranch string     `json:"default_branch"`
	CloneURL      string     `json:"clone_url"`
	HTMLURL       string     `json:"html_url"`
	PushedAt      *time.Time `json:"pushed_at,omitempty"`
	// Permissions.Push is why this field exists: a repository the developer can
	// only read is offered, but selecting it will fail on push, and saying so in
	// the list is cheaper than saying so afterwards.
	CanPush bool `json:"can_push"`
	Empty   bool `json:"empty"`
}

// Link is the repository this developer is working on, and where its working
// copy lives.
//
// One per developer. Switching repositories replaces it and leaves the old
// checkout on the PVC, which is what makes switching back a reuse rather than a
// re-clone (§5.11 item 5).
type Link struct {
	UserSub       string `json:"-"`
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	// Path is where the checkout is, relative to the workspace root — so
	// `{workspace}/{path}` on the PVC. Derived from the repository name and stable,
	// which is the "stable path" of §5.6.
	Path string `json:"path"`
	// OperatorLibRef is the Operator Lib the scaffold pinned, per D15: latest at
	// scaffold time, recorded so an upgrade is a visible decision later. Empty for
	// a repository ODE did not scaffold.
	OperatorLibRef string     `json:"operator_lib_ref,omitempty"`
	ScaffoldedAt   *time.Time `json:"scaffolded_at,omitempty"`
	SelectedAt     time.Time  `json:"selected_at"`
}

// Change is one entry of `git status`.
type Change struct {
	Path string `json:"path"`
	// Kind is "modified", "added", "deleted", "renamed", "copied", "untracked",
	// "unmerged" or "typechange" — git's XY codes, named.
	Kind string `json:"kind"`
	// Staged and Unstaged are the two halves of git's XY, so the pane can show
	// what a commit would include. A file can be both.
	Staged   bool `json:"staged"`
	Unstaged bool `json:"unstaged"`
	// RenamedFrom is the old path of a rename or a copy.
	RenamedFrom string `json:"renamed_from,omitempty"`
}

// Status is the working copy as it stands.
//
// Everything §5.11 items 5 and 6 require a developer to see on reopen is in here:
// whether the checkout exists at all, which branch it is on, how far it has
// diverged from the remote in each direction, and what is uncommitted.
type Status struct {
	Link Link `json:"link"`
	// Cloned is false when the PVC has no checkout, which happens on a fresh PVC
	// and after a developer deletes the directory themselves.
	Cloned bool `json:"cloned"`
	// Workspace and AbsolutePath say where the checkout is, because "it is on the
	// PVC" is not something a developer can act on.
	Workspace string `json:"workspace"`
	Branch    string `json:"branch,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	// Ahead and Behind are commits, relative to the upstream branch. Both non-zero
	// is Diverged, which is the case that needs a human.
	Ahead    int  `json:"ahead"`
	Behind   int  `json:"behind"`
	Diverged bool `json:"diverged"`
	// Detached is a checkout not on a branch — nothing ODE does produces one, but a
	// developer with a terminal can, and committing there silently would lose work.
	Detached bool `json:"detached"`
	// Unborn is a clone of an empty repository: a branch with no commits yet. It is
	// the normal state of a repository ODE has just created.
	Unborn      bool   `json:"unborn"`
	Head        string `json:"head,omitempty"`
	HeadSubject string `json:"head_subject,omitempty"`
	HeadDate    string `json:"head_date,omitempty"`
	// Remote is the origin URL as the checkout has it, so a directory pointing at
	// a different repository than the link says is visible rather than silently
	// committed into.
	Remote string `json:"remote,omitempty"`
	// RemoteMismatch is that case, made explicit.
	RemoteMismatch bool `json:"remote_mismatch,omitempty"`

	Changes []Change `json:"changes"`
	Dirty   bool     `json:"dirty"`
	// Fetched says whether this status included a fetch. A status without one
	// reports the divergence ODE last knew about, and saying which it is prevents
	// reading a stale zero as "up to date".
	Fetched bool `json:"fetched"`
	// Scaffold reports which of the compliance files of §5.11 item 3 are present.
	// A repository the developer brought themselves is usually missing some, and
	// this is what the pane offers to scaffold from.
	Scaffold ScaffoldState `json:"scaffold"`
}

// ScaffoldState is which template files the working copy has.
type ScaffoldState struct {
	Present []string `json:"present"`
	Missing []string `json:"missing"`
	// Complete is Missing being empty — carried so the SPA does not have to know
	// that rule.
	Complete bool `json:"complete"`
}

// CommitResult is one commit, made.
type CommitResult struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	// Files is how many paths the commit touched, from git's own summary rather
	// than from what ODE staged — the two differ when a file was already staged.
	Files int `json:"files"`
	// Branch is where it landed, which for a first commit is the branch ODE put
	// HEAD on rather than one that existed before.
	Branch string `json:"branch"`
}

// PushResult is one push. Output is git's own reporting, kept because the useful
// part of a push is what the remote said — a protected branch, a rejected
// non-fast-forward, a new pull request URL.
type PushResult struct {
	Branch string `json:"branch"`
	Remote string `json:"remote"`
	Output string `json:"output,omitempty"`
	// HeadSHA is what the remote now has, so the experiment tag of §5.11 item 7
	// can be taken from a state that is actually pushed.
	HeadSHA string `json:"head_sha,omitempty"`
}

// FileTree is the Code pane's tree (D14).
type FileTree struct {
	// Root is the checkout, relative to the workspace.
	Root string      `json:"root"`
	Tree kernel.Node `json:"tree"`
	// Excluded names what the walk did not enter. `.git` and nothing else: a
	// developer may edit every file of their repository, but the object database is
	// not a file they edit — and listing it would bury the tree.
	Excluded []string `json:"excluded"`
}

// File is one file of the working copy, for the editor.
type File struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Text      string `json:"text"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	Modified  string `json:"modified,omitempty"`
	// Language is a hint for the editor, derived from the extension. The backend
	// does it so the pane and `write_file` agree on what a `.py` is.
	Language string `json:"language,omitempty"`
}
