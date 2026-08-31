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
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/identifiers"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// Options is how a deployment configures this package.
type Options struct {
	// ClientID and ClientSecret are the GitHub OAuth app's. Without a client id
	// this package is not served at all.
	ClientID     string
	ClientSecret string
	// APIURL and WebURL are api.github.com and github.com by default, and are
	// separate so a GitHub Enterprise deployment can point at its own.
	APIURL string
	WebURL string
	// Scopes is what the consent screen asks for: `repo` to read and write the
	// repository, `workflow` to write .github/workflows (§5.11 item 1). Without
	// `workflow`, GitHub rejects a push that touches a workflow file — which the
	// scaffold always does.
	Scopes []string
	// RedirectURI is where GitHub returns the browser. It is the SPA's own URL, and
	// it has to match the OAuth app's registered callback exactly.
	RedirectURI string
	// StateTTL bounds a pending authorisation.
	StateTTL time.Duration
	// RequestTimeout bounds one GitHub API call.
	RequestTimeout time.Duration
	// CommandTimeout bounds one git command. A clone of a repository with history
	// is the slow one, so this is minutes rather than seconds.
	CommandTimeout time.Duration
	// LockTimeout bounds `uv lock` at the end of a scaffold. Its own figure rather
	// than CommandTimeout's, because it is not doing git's kind of work: resolving
	// the Operator Lib pin means cloning that repository and building its metadata
	// on a cold uv cache, and a bound sized for a clone would report a timeout on
	// the first scaffold a pod ever runs.
	LockTimeout time.Duration
	// MaxFileBytes bounds a file the Code pane reads or writes.
	MaxFileBytes int
	// MaxTreeEntries bounds one file tree.
	MaxTreeEntries int
	// MaxCommandOutputBytes bounds what one git command may return.
	MaxCommandOutputBytes int
	// OperatorLib is the library the scaffold pins, as owner/name.
	OperatorLib string
	// OperatorLibRef pins it to something fixed. Empty resolves the newest tag at
	// scaffold time, which is what D15 asks for.
	OperatorLibRef string
	// MaxWorkbenches caps how many working contexts one developer may hold open.
	// The cost being capped is a kernel process each in their pod, not a row here.
	// Zero takes DefaultMaxWorkbenches.
	MaxWorkbenches int
}

// Deps is what the service is built from.
type Deps struct {
	// Workspace is the developer's pod. Required: without it there is nowhere for a
	// working copy to be.
	Workspace Workspace
	Store     Store
	Sealer    *Sealer
	// IDs mints workbench ids. Nil takes the real source; a test supplies a
	// deterministic one, as chat and tools do.
	IDs IDs
	// Limits is the per-user administration of §3.3, when a deployment has one.
	// Nil leaves only this deployment's own ceiling, which is the configuration a
	// database-less ODE runs with.
	Limits Limits
	// Now is the clock, replaced by tests. Nil takes time.Now.
	Now func() time.Time
	// HTTPClient is replaced by tests.
	HTTPClient *http.Client
	Options
}

// IDs is the identifier source. Declared here rather than taken from
// pkg/identifiers directly so a test can make the ids in an assertion readable.
type IDs interface {
	NewID() string
}

// Limits is the per-user ceiling on open workbenches. *admin.Service implements
// it; declared here so the dependency points one way, as pkg/kernel's Workbenches
// does.
type Limits interface {
	// CheckWorkbenchCount refuses when current is already at this user's ceiling.
	// A nil error means the deployment's own cap is the only one that applies.
	CheckWorkbenchCount(ctx context.Context, userSub string, current int) error
}

// Service is ODE's repository surface.
type Service struct {
	workspace Workspace
	store     Store
	sealer    *Sealer
	ids       IDs
	limits    Limits
	// drafts is the LLM side of a commit message draft (§5.11 item 5), installed by
	// UseDrafts. A zero value means this deployment cannot draft one.
	drafts DraftDeps
	now    func() time.Time
	http   *http.Client
	states *stateStore
	opts   Options
}

const (
	defaultStateTTL       = 10 * time.Minute
	defaultRequestTimeout = 30 * time.Second
	defaultCommandTimeout = 5 * time.Minute
	defaultLockTimeout    = 10 * time.Minute
	defaultMaxFileBytes   = 1 << 20
	defaultMaxTreeEntries = 4000
	defaultMaxCommandOut  = 1 << 20
	defaultOperatorLib    = "SENERGY-Platform/analytics-operator-lib-python"
	defaultAPIURL         = "https://api.github.com"
	defaultWebURL         = "https://github.com"
)

// New builds the service. It refuses rather than degrades on the two things that
// cannot be defaulted: somewhere to put a working copy, and a key to seal a
// credential with. Both are deployment facts, and a repo surface that answers
// requests without either would fail on the developer's first click instead.
func New(deps Deps) (*Service, error) {
	if deps.Workspace == nil {
		return nil, errors.New("repo: a workspace is required (§5.11 item 5)")
	}
	if deps.Store == nil {
		return nil, errors.New("repo: a store is required")
	}
	if deps.Sealer == nil {
		return nil, errors.New("repo: a sealer is required to store a GitHub token (§5.11 item 1)")
	}
	if deps.ClientID == "" || deps.ClientSecret == "" {
		return nil, errors.New("repo: a GitHub OAuth app client id and secret are required")
	}
	if deps.RedirectURI == "" {
		return nil, errors.New("repo: a redirect uri is required for the OAuth web flow")
	}

	opts := deps.Options
	if opts.APIURL == "" {
		opts.APIURL = defaultAPIURL
	}
	if opts.WebURL == "" {
		opts.WebURL = defaultWebURL
	}
	if len(opts.Scopes) == 0 {
		opts.Scopes = []string{"repo", "workflow"}
	}
	if opts.StateTTL <= 0 {
		opts.StateTTL = defaultStateTTL
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = defaultCommandTimeout
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = defaultLockTimeout
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = defaultMaxFileBytes
	}
	if opts.MaxTreeEntries <= 0 {
		opts.MaxTreeEntries = defaultMaxTreeEntries
	}
	if opts.MaxCommandOutputBytes <= 0 {
		opts.MaxCommandOutputBytes = defaultMaxCommandOut
	}
	if opts.OperatorLib == "" {
		opts.OperatorLib = defaultOperatorLib
	}
	if opts.MaxWorkbenches <= 0 {
		opts.MaxWorkbenches = DefaultMaxWorkbenches
	}

	client := deps.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: opts.RequestTimeout + 10*time.Second}
	}
	ids := deps.IDs
	if ids == nil {
		ids = identifiers.New()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	return &Service{
		workspace: deps.Workspace,
		store:     deps.Store,
		sealer:    deps.Sealer,
		ids:       ids,
		limits:    deps.Limits,
		now:       now,
		http:      client,
		states:    newStateStore(opts.StateTTL),
		opts:      opts,
	}, nil
}

// UseLimits installs the per-user administration of §3.3.
//
// Set after construction rather than in Deps for the reason
// kernel.UseWorkbenches gives: the admin service is built inside the LLM surface's
// wiring, which runs after this one because write_file has to be registrable by
// then. Nil-safe both ways — a deployment without an admin surface keeps only its
// own ceiling.
func (s *Service) UseLimits(limits Limits) { s.limits = limits }

// Scopes is what this deployment asks GitHub for, reported by /session so the SPA
// can explain the consent screen before the developer sees it.
func (s *Service) Scopes() []string { return s.opts.Scopes }

// Request is what every operation needs: whose pod, whose GitHub account, and who
// a commit would be by.
//
// Bearer is the developer's platform token, forwarded to the kernel; UserSub keys
// ODE's own two rows. They come from the same validated token and are carried
// separately for the reason kernel.User does: one addresses a pod, the other keys
// a record.
type Request struct {
	Bearer  string
	UserSub string
	Author  Author
	// WorkbenchID names the working context this acts in: which checkout, and which
	// kernel runs the git commands. Empty means the developer's only one, which is
	// what keeps a caller that has never heard of workbenches correct — and is
	// refused once there are two, because guessing between two working copies is
	// the failure workbenches exist to prevent.
	WorkbenchID string
}

// Repositories lists what the developer could work on.
func (s *Service) Repositories(ctx context.Context, req Request) ([]Repository, error) {
	client, err := s.clientFor(ctx, req.UserSub)
	if err != nil {
		return nil, err
	}
	return client.Repositories(ctx)
}

// SelectRequest names an existing repository to work on.
type SelectRequest struct {
	Request
	FullName string
	// Scaffold writes the template into the working copy after the clone. False by
	// default for an existing repository: a developer selecting their own code did
	// not ask for eleven new files.
	Scaffold bool
}

// Select links an existing repository and makes sure its working copy is present.
func (s *Service) Select(ctx context.Context, req SelectRequest) (Status, error) {
	owner, name, err := splitFullName(req.FullName)
	if err != nil {
		return Status{}, err
	}
	client, err := s.clientFor(ctx, req.UserSub)
	if err != nil {
		return Status{}, err
	}
	repository, err := client.Repository(ctx, owner, name)
	if err != nil {
		return Status{}, err
	}
	return s.link(ctx, req.Request, repository, req.Scaffold)
}

// CreateRequest describes a repository to create.
type CreateRequest struct {
	Request
	Name        string
	Description string
	Private     bool
	// Organisation creates the repository under an organisation rather than under
	// the developer's own account. An institute's operators usually live there.
	Organisation string
	// Scaffold defaults to true for a created repository: an empty repository with
	// no scaffold is not something anyone asked for.
	Scaffold bool
}

// Create makes a repository, links it, clones it and scaffolds the working copy.
//
// What it does not do is commit. The repository is empty on GitHub until the
// developer commits and pushes, which is §5.11 item 5 taken literally — and it is
// also why this path and Select converge after one branch: both end with a
// checkout on the PVC and a developer deciding what to do with it.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Status, error) {
	if !validRepositoryName(req.Name) {
		return Status{}, fmt.Errorf(
			"%w: %q is not a usable repository name; letters, digits, dot, dash and "+
				"underscore only", ErrInvalidRequest, req.Name)
	}
	client, err := s.clientFor(ctx, req.UserSub)
	if err != nil {
		return Status{}, err
	}
	repository, err := client.CreateRepository(
		ctx, req.Name, req.Description, req.Private, req.Organisation)
	if err != nil {
		return Status{}, err
	}
	return s.link(ctx, req.Request, repository, req.Scaffold)
}

// link is the shared tail of Select and Create: record the link, make sure the
// checkout is there, optionally scaffold, and report the status.
func (s *Service) link(
	ctx context.Context, req Request, repository Repository, scaffold bool,
) (Status, error) {
	branch := repository.DefaultBranch
	if branch == "" {
		// A repository with no commits reports no default branch, so the name comes
		// from GitHub's account-level default. `main` rather than `master` because
		// that is what GitHub creates today, and a mismatch here would push the
		// scaffold to a branch the repository's own settings do not point at.
		branch = "main"
	}
	// The workbench this lands in, resolved before anything touches the PVC: a
	// repository already open in another one is refused here rather than after a
	// clone has already happened.
	bench, err := s.forSelection(ctx, req, repository.FullName)
	if err != nil {
		return Status{}, err
	}
	// Every operation below runs in that workbench, including the ones Scaffold
	// makes on its own.
	req.WorkbenchID = bench.ID

	link := Link{
		UserSub:       req.UserSub,
		WorkbenchID:   bench.ID,
		FullName:      repository.FullName,
		Name:          repository.Name,
		Owner:         repository.Owner,
		DefaultBranch: branch,
		Private:       repository.Private,
		CloneURL:      repository.CloneURL,
		HTMLURL:       repository.HTMLURL,
		Path:          checkoutPath(repository.Owner, repository.Name),
		SelectedAt:    time.Now().UTC(),
	}
	// Carried over so that re-selecting a repository ODE scaffolded earlier does not
	// lose which Operator Lib it was pinned to, nor which directory its working copy
	// is in: the path a previous session used is where the developer's uncommitted
	// work is, and deriving a different one here would abandon it.
	if previous := bench.Link; previous.FullName == link.FullName {
		link.OperatorLibRef = previous.OperatorLibRef
		link.ScaffoldedAt = previous.ScaffoldedAt
		if previous.Path != "" {
			link.Path = previous.Path
		}
	}

	token, err := s.tokenFor(ctx, req.UserSub)
	if err != nil {
		return Status{}, err
	}
	path, err := s.resolveCheckoutPath(ctx, req, link, token)
	if err != nil {
		return Status{}, err
	}
	link.Path = path

	if err := s.putLink(ctx, link); err != nil {
		return Status{}, err
	}
	if err := s.ensureCheckout(ctx, req, link, token); err != nil {
		return Status{}, err
	}

	if scaffold {
		if _, err := s.Scaffold(ctx, ScaffoldRequest{Request: req}); err != nil {
			return Status{}, err
		}
		// Re-read: Scaffold records the pin, and the status should report the link as
		// it now is rather than as it was a moment ago.
		if updated, err := s.Workbench(ctx, req.UserSub, bench.ID); err == nil {
			link = updated.Link
		}
	}

	return s.statusOf(ctx, req, link, token, false)
}

// resolveCheckoutPath decides which directory this link's working copy is in.
//
// Normally that is `{owner}/{name}` and there is nothing to decide. The case that
// needs one is a developer who has a checkout from before the owner was part of
// the path: it is named by the repository alone, and it holds whatever they had
// not committed when they last worked on it. Cloning afresh under the new name
// would leave that work in a directory nothing points at any more, which is the
// silent loss §5.11 item 6 forbids as squarely as a reset would be — so the old
// directory is adopted and the link records it.
//
// The adoption is conditional on the origin, and that condition is the whole
// point: a directory of that name whose origin is *another* owner's repository is
// exactly what the owner in the path exists to separate, and reusing it would
// reintroduce the bug this path shape fixes.
func (s *Service) resolveCheckoutPath(
	ctx context.Context, req Request, link Link, token string,
) (string, error) {
	preferred := link.Path
	legacy := sanitiseSegment(link.Name)
	if preferred == legacy {
		return preferred, nil
	}
	if present, err := s.git(req, link.WorkbenchID, preferred, token).isRepository(ctx); err != nil {
		return "", err
	} else if present {
		return preferred, nil
	}

	previous := s.git(req, link.WorkbenchID, legacy, token)
	present, err := previous.isRepository(ctx)
	if err != nil {
		return "", err
	}
	if !present {
		return preferred, nil
	}
	origin, err := previous.originURL(ctx)
	if err != nil {
		return "", err
	}
	if !sameRemote(origin, link.CloneURL) {
		return preferred, nil
	}
	return legacy, nil
}

// ensureCheckout clones the repository if the PVC does not already have it.
//
// §5.11 item 5: reuse the existing checkout rather than re-cloning. A directory
// that is already a working copy is left exactly as it is — including its
// uncommitted changes, which item 6 is about — and its origin is compared against
// the link, so a directory pointing somewhere else is refused rather than
// committed into.
func (s *Service) ensureCheckout(
	ctx context.Context, req Request, link Link, token string,
) error {
	checkout := s.git(req, link.WorkbenchID, link.Path, token)
	present, err := checkout.isRepository(ctx)
	if err != nil {
		return err
	}
	if present {
		return checkout.verifyRemote(ctx, link)
	}

	root := s.git(req, link.WorkbenchID, "", token)
	if err := root.clone(ctx, link.CloneURL, link.Path); err != nil {
		// The first operation that authenticates, and the one a developer meets on
		// their first click: a clone refused for a stale credential has to say so
		// rather than read as a broken repository.
		return s.explainAuth(ctx, req, err)
	}
	// A clone of an empty repository leaves HEAD on the local default branch name,
	// which is often not the one GitHub will serve. Fixed here, once, so the first
	// push lands where the repository's settings point.
	status, err := s.rawStatus(ctx, checkout)
	if err != nil {
		return err
	}
	if status.Unborn && status.Branch != link.DefaultBranch {
		return checkout.setBranch(ctx, link.DefaultBranch)
	}
	return nil
}

// StatusRequest asks for the working copy's state.
type StatusRequest struct {
	Request
	// Fetch contacts the remote first, so the ahead/behind counts are current.
	// Off by default because the pane polls this and a fetch is a network round
	// trip per poll; the SPA asks for one when the developer opens the pane or
	// presses refresh, which is where §5.11 item 5's "report divergence" belongs.
	Fetch bool
}

// Status reports the working copy.
func (s *Service) Status(ctx context.Context, req StatusRequest) (Status, error) {
	link, err := s.linkFor(ctx, req.Request)
	if err != nil {
		return Status{}, err
	}
	token, err := s.tokenFor(ctx, req.UserSub)
	if err != nil {
		return Status{}, err
	}
	return s.statusOf(ctx, req.Request, link, token, req.Fetch)
}

// Unlink forgets which repository the developer was working on. The checkout stays
// on the PVC: it is their work, and §5.11 item 6 is explicit that ODE does not
// discard it on its own initiative.
func (s *Service) Unlink(ctx context.Context, req Request) error {
	bench, err := s.resolve(ctx, req)
	if err != nil {
		return err
	}
	bench.Link = Link{}
	bench.LastUsedAt = s.now()
	return s.store.PutWorkbench(ctx, bench)
}

func (s *Service) statusOf(
	ctx context.Context, req Request, link Link, token string, fetch bool,
) (Status, error) {
	status := Status{Link: link, Workspace: s.workspace.Workspace()}
	checkout := s.git(req, link.WorkbenchID, link.Path, token)

	present, err := checkout.isRepository(ctx)
	if err != nil {
		return status, err
	}
	if !present {
		return status, nil
	}
	status.Cloned = true

	if fetch {
		if _, err := checkout.run(ctx, "fetch", "--prune", "origin"); err != nil {
			return status, s.explainAuth(ctx, req, err)
		}
		status.Fetched = true
	}

	raw, err := s.rawStatus(ctx, checkout)
	if err != nil {
		return status, err
	}
	status.Branch, status.Upstream = raw.Branch, raw.Upstream
	status.Ahead, status.Behind = raw.Ahead, raw.Behind
	status.Diverged = raw.Ahead > 0 && raw.Behind > 0
	status.Detached, status.Unborn = raw.Detached, raw.Unborn
	status.Changes = raw.Changes
	if status.Changes == nil {
		status.Changes = []Change{}
	}
	status.Dirty = len(status.Changes) > 0

	if !raw.Unborn {
		if result, err := checkout.attempt(ctx, "log", "-1", "--pretty=format:"+headFormat); err == nil &&
			result.ExitCode == 0 {
			status.Head, status.HeadSubject, status.HeadDate = parseHead(result.Stdout)
		}
	}
	if result, err := checkout.attempt(ctx, "remote", "get-url", "origin"); err == nil &&
		result.ExitCode == 0 {
		status.Remote = strings.TrimSpace(result.Stdout)
		status.RemoteMismatch = !sameRemote(status.Remote, link.CloneURL)
	}

	state, err := s.scaffoldState(ctx, req, link)
	if err != nil {
		return status, err
	}
	status.Scaffold = state
	return status, nil
}

func (s *Service) rawStatus(ctx context.Context, checkout gitContext) (gitStatus, error) {
	result, err := checkout.run(ctx,
		"status", "--porcelain=v2", "--branch", "--untracked-files=all")
	if err != nil {
		return gitStatus{}, err
	}
	return parseStatus(result.Stdout), nil
}

// scaffoldState compares the working copy against the compliance set. It reads the
// tree rather than stat-ing eleven paths, which is one pod round trip instead of
// eleven.
func (s *Service) scaffoldState(
	ctx context.Context, req Request, link Link,
) (ScaffoldState, error) {
	tree, err := s.workspace.Tree(ctx, s.ref(req, link), kernel.TreeRequest{
		Path:       link.Path,
		Recursive:  true,
		MaxEntries: s.opts.MaxTreeEntries,
		Exclude:    []string{".git"},
	})
	if err != nil {
		return ScaffoldState{}, err
	}
	present := map[string]bool{}
	collectPaths(tree, link.Path, present)

	state := ScaffoldState{Present: []string{}, Missing: []string{}}
	for _, wanted := range scaffoldPaths {
		if present[wanted] {
			state.Present = append(state.Present, wanted)
		} else {
			state.Missing = append(state.Missing, wanted)
		}
	}
	state.Complete = len(state.Missing) == 0
	return state, nil
}

// collectPaths flattens a tree into checkout-relative file paths.
func collectPaths(node kernel.Node, root string, into map[string]bool) {
	if node.Type == "file" {
		if relative := strings.TrimPrefix(strings.TrimPrefix(node.Path, root), "/"); relative != "" {
			into[relative] = true
		}
	}
	for _, child := range node.Children {
		collectPaths(child, root, into)
	}
}

// ScaffoldRequest writes the template into the working copy.
//
// There is no overwrite option, and its absence is the decision: a file that
// exists belongs to the developer, and a scaffold that replaced one would be
// exactly the silent write §5.11 forbids.
type ScaffoldRequest struct {
	Request
}

// Scaffold writes the missing template files into the working copy.
func (s *Service) Scaffold(ctx context.Context, req ScaffoldRequest) (ScaffoldResult, error) {
	link, err := s.linkFor(ctx, req.Request)
	if err != nil {
		return ScaffoldResult{}, err
	}
	token, err := s.tokenFor(ctx, req.UserSub)
	if err != nil {
		return ScaffoldResult{}, err
	}
	checkout := s.git(req.Request, link.WorkbenchID, link.Path, token)
	present, err := checkout.isRepository(ctx)
	if err != nil {
		return ScaffoldResult{}, err
	}
	if !present {
		return ScaffoldResult{}, ErrNotCloned
	}

	// The pin of D15. Resolved once and remembered: a second scaffold of the same
	// repository must not quietly move the developer to a newer library.
	ref := link.OperatorLibRef
	if ref == "" {
		ref = s.opts.OperatorLibRef
	}
	if ref == "" {
		client, err := s.clientFor(ctx, req.UserSub)
		if err != nil {
			return ScaffoldResult{}, err
		}
		if ref, err = client.LatestRef(ctx, s.opts.OperatorLib); err != nil {
			return ScaffoldResult{}, err
		}
	}

	files, err := RenderScaffold(ScaffoldValues{
		Repository:     link.FullName,
		Name:           link.Name,
		ClassName:      className(link.Name),
		OperatorLib:    s.opts.OperatorLib,
		OperatorLibRef: ref,
		Image:          imageReference(link.FullName),
		Branch:         link.DefaultBranch,
	})
	if err != nil {
		return ScaffoldResult{}, err
	}

	state, err := s.scaffoldState(ctx, req.Request, link)
	if err != nil {
		return ScaffoldResult{}, err
	}
	existing := map[string]bool{}
	for _, path := range state.Present {
		existing[path] = true
	}

	result := ScaffoldResult{Written: []string{}, Skipped: []string{}, OperatorLibRef: ref}
	for _, file := range files {
		if existing[file.Path] {
			result.Skipped = append(result.Skipped, file.Path)
			continue
		}
		if _, err := s.workspace.WriteFile(ctx, s.ref(req.Request, link),
			path.Join(link.Path, file.Path), []byte(file.Content)); err != nil {
			return result, err
		}
		result.Written = append(result.Written, file.Path)
	}
	// The lock, which is generated rather than rendered — and the reason it is ODE's
	// job rather than the developer's. The README used to ask them to run `uv lock`
	// before their first experiment; the file it produces is what makes a run's
	// recorded commit SHA describe the whole run rather than only its source, and a
	// step that has to be remembered to keep that true is a step that will be
	// forgotten. Here it lands as one more untracked file in the diff they are
	// already told to read, so it joins the first commit without anyone being asked.
	//
	// Not overwritten when it is already there, for the same reason nothing else is.
	if existing[LockFile] {
		result.Skipped = append(result.Skipped, LockFile)
	} else if reason := s.lock(ctx, req.Request, link); reason != "" {
		result.LockError = reason
	} else {
		result.Written = append(result.Written, LockFile)
	}

	result.Hint = "nothing is committed yet: read the diff, then commit and push"

	link.OperatorLibRef = ref
	now := time.Now().UTC()
	link.ScaffoldedAt = &now
	if err := s.putLink(ctx, link); err != nil {
		return result, err
	}
	return result, nil
}

// lock runs `uv lock` in the checkout and answers with why it did not, or empty.
//
// It is a plain kernel command rather than anything git-shaped: same pod, same
// workbench, same mechanism the working copy is driven with. No credential goes
// with it. The scaffold's pin is a public repository, and a resolver that clones
// git sources and runs their build backends is not somewhere to hand a developer's
// GitHub token without deciding to — a private pin fails here and is locked by
// hand, which is a worse outcome than a leak.
func (s *Service) lock(ctx context.Context, req Request, link Link) string {
	result, err := s.workspace.Command(ctx, s.ref(req, link), kernel.Command{
		Argv: []string{"uv", "lock"},
		Dir:  link.Path,
		Env: map[string]string{
			// uv shells out to git for the Operator Lib source. Without this a git
			// that wants credentials waits for a terminal that is not there, and the
			// scaffold reports a timeout minutes later instead of the refusal.
			"GIT_TERMINAL_PROMPT": "0",
			// The reason text is read by a person in the SPA and by a model in the
			// chat. Escape sequences help neither.
			"NO_COLOR": "1",
		},
		Timeout:        s.opts.LockTimeout,
		MaxOutputBytes: s.opts.MaxCommandOutputBytes,
	})
	if err != nil {
		return err.Error()
	}
	if result.TimedOut {
		return fmt.Sprintf("`uv lock` did not finish within %s", s.opts.LockTimeout)
	}
	if result.ExitCode != 0 {
		return lockReason(result)
	}
	return ""
}

// lockReason is uv's own complaint, cut to something an error field can carry.
//
// The last lines rather than the first: uv reports what it was doing and then why
// it stopped, and the second is the part that names the repair.
func lockReason(result kernel.CommandResult) string {
	const maxReason = 500
	text := strings.TrimSpace(result.Stderr)
	if text == "" {
		text = strings.TrimSpace(result.Stdout)
	}
	if text == "" {
		return fmt.Sprintf("`uv lock` exited %d without saying why", result.ExitCode)
	}
	// Never below one line: dropping the last one would leave an empty reason, and
	// an empty reason is read by the caller as a lock that succeeded.
	lines := strings.Split(text, "\n")
	for len(lines) > 1 && len(text) > maxReason {
		lines = lines[1:]
		text = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	if len(text) > maxReason {
		// One line longer than the bound on its own. Keep its end, and drop the
		// half rune the cut can land in the middle of.
		text = strings.TrimSpace(strings.ToValidUTF8(text[len(text)-maxReason:], ""))
	}
	if text == "" {
		return fmt.Sprintf("`uv lock` exited %d without saying why", result.ExitCode)
	}
	return text
}

// CommitRequest is one commit, explicitly asked for.
type CommitRequest struct {
	Request
	Message string
	// Paths stages only these, relative to the checkout. Empty stages everything,
	// which is what the pane's "commit all" does.
	Paths []string
}

// Commit stages and commits. There is no path anywhere in ODE that commits without
// a developer asking (§5.11 item 5).
func (s *Service) Commit(ctx context.Context, req CommitRequest) (CommitResult, error) {
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return CommitResult{}, fmt.Errorf("%w: a commit needs a message", ErrInvalidRequest)
	}
	link, err := s.linkFor(ctx, req.Request)
	if err != nil {
		return CommitResult{}, err
	}
	token, err := s.tokenFor(ctx, req.UserSub)
	if err != nil {
		return CommitResult{}, err
	}
	checkout := s.git(req.Request, link.WorkbenchID, link.Path, token)
	if present, err := checkout.isRepository(ctx); err != nil {
		return CommitResult{}, err
	} else if !present {
		return CommitResult{}, ErrNotCloned
	}
	// Before anything is staged: a commit into a checkout that points at another
	// repository is a commit the developer would push there next.
	if err := checkout.verifyRemote(ctx, link); err != nil {
		return CommitResult{}, err
	}

	stage := []string{"add", "--all", "--"}
	if len(req.Paths) == 0 {
		stage = append(stage, ".")
	} else {
		for _, requested := range req.Paths {
			clean, err := relativePath(requested)
			if err != nil {
				return CommitResult{}, err
			}
			stage = append(stage, clean)
		}
	}
	// `--` before the message would be wrong here: commit takes it as an option
	// terminator for paths, and the message is an option value.
	commit := append(authorArgs(req.Author), "commit", "--message", message)

	// Staging and committing under one claim. As two, the stage could land and the
	// commit be refused as busy — leaving the developer told that nothing happened
	// and their whole working copy staged. Nothing is lost by that, unlike Discard,
	// but it is still not what the answer said.
	sequence := [][]string{stage, commit}
	results, err := checkout.runAll(ctx, sequence...)
	if err != nil {
		return CommitResult{}, err
	}
	// Anything but a clean stage followed by a commit that ran is reported as the
	// command that refused. A commit that ran and exited non-zero is the one case
	// with a reading of its own, below.
	if len(results) < 2 || results[0].ExitCode != 0 || results[0].TimedOut {
		return CommitResult{}, checkout.batchFailure(sequence, results)
	}
	result := results[1]
	if result.ExitCode != 0 {
		// git exits 1 with "nothing to commit" on a clean tree, which is not a
		// failure of anything and reads terribly as one.
		if strings.Contains(result.Stdout, "nothing to commit") ||
			strings.Contains(result.Stdout, "nothing added to commit") ||
			strings.Contains(result.Stderr, "nothing to commit") {
			return CommitResult{}, ErrNothingToCommit
		}
		return CommitResult{}, checkout.failure([]string{"commit"}, result)
	}

	committed := CommitResult{Subject: message, Files: commitSummary(result.Stdout)}
	if head, err := checkout.attempt(ctx, "log", "-1", "--pretty=format:"+headFormat); err == nil &&
		head.ExitCode == 0 {
		committed.SHA, committed.Subject, _ = parseHead(head.Stdout)
	}
	if branch, err := checkout.attempt(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil &&
		branch.ExitCode == 0 {
		committed.Branch = strings.TrimSpace(branch.Stdout)
	}
	return committed, nil
}

// PushRequest is one push.
//
// There is no force option, deliberately: a force push destroys history on a
// shared remote, no ODE workflow needs one, and a developer who wants it has a
// terminal in the same pod.
type PushRequest struct {
	Request
	// Branch defaults to the checkout's current branch.
	Branch string
}

// Push sends the current branch to GitHub.
func (s *Service) Push(ctx context.Context, req PushRequest) (PushResult, error) {
	link, err := s.linkFor(ctx, req.Request)
	if err != nil {
		return PushResult{}, err
	}
	token, err := s.tokenFor(ctx, req.UserSub)
	if err != nil {
		return PushResult{}, err
	}
	checkout := s.git(req.Request, link.WorkbenchID, link.Path, token)
	if present, err := checkout.isRepository(ctx); err != nil {
		return PushResult{}, err
	} else if !present {
		return PushResult{}, ErrNotCloned
	}
	// The one that matters most: git pushes to the origin it has, not to the
	// repository ODE recorded, and a push cannot be taken back from the remote.
	if err := checkout.verifyRemote(ctx, link); err != nil {
		return PushResult{}, err
	}

	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		result, err := checkout.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return PushResult{}, err
		}
		branch = strings.TrimSpace(result.Stdout)
	}
	if branch == "HEAD" {
		return PushResult{}, fmt.Errorf(
			"%w: the working copy is not on a branch, so there is nothing to push",
			ErrInvalidRequest)
	}
	refspec, err := pushRefspec(branch)
	if err != nil {
		return PushResult{}, err
	}

	result, err := checkout.run(ctx, "push", "--set-upstream", "origin", refspec)
	if err != nil {
		return PushResult{}, s.explainAuth(ctx, req.Request, err)
	}
	pushed := PushResult{
		Branch: branch,
		Remote: "origin",
		// git reports a push on stderr, which is not an error but its progress
		// channel — the useful part, including a pull request URL, is in there.
		Output: strings.TrimSpace(firstNonEmpty(result.Stderr, result.Stdout)),
	}
	if head, err := checkout.attempt(ctx, "rev-parse", "HEAD"); err == nil && head.ExitCode == 0 {
		pushed.HeadSHA = strings.TrimSpace(head.Stdout)
	}
	return pushed, nil
}

// FetchRequest asks the remote what it has.
type FetchRequest struct{ Request }

// Fetch updates the remote-tracking branches and reports the divergence. It is the
// "report divergence rather than re-cloning" of §5.11 item 5 as an action a
// developer can take at any time, not only on reopen.
func (s *Service) Fetch(ctx context.Context, req FetchRequest) (Status, error) {
	return s.Status(ctx, StatusRequest{Request: req.Request, Fetch: true})
}

// StashRequest puts uncommitted work aside.
type StashRequest struct {
	Request
	Message string
}

// Stash is one of the three answers §5.11 item 6 requires for uncommitted changes
// found on reopen — the reversible one, and therefore the one to reach for first.
func (s *Service) Stash(ctx context.Context, req StashRequest) (Status, error) {
	link, token, checkout, err := s.checkoutFor(ctx, req.Request)
	if err != nil {
		return Status{}, err
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		message = "ODE: work in progress"
	}
	args := []string{"stash", "push", "--include-untracked", "--message", message}
	result, err := checkout.attempt(ctx, args...)
	if err != nil {
		return Status{}, err
	}
	if result.ExitCode != 0 {
		return Status{}, checkout.failure(args, result)
	}
	return s.statusOf(ctx, req.Request, link, token, false)
}

// DiscardRequest throws uncommitted work away.
type DiscardRequest struct {
	Request
	// Confirm has to be true. The one destructive operation in this package asks
	// twice on purpose: the API layer requires the flag and this refuses without
	// it, so neither an over-eager client nor a mistaken curl can lose a
	// developer's afternoon.
	Confirm bool
}

// Discard resets the working copy to HEAD and removes untracked files.
func (s *Service) Discard(ctx context.Context, req DiscardRequest) (Status, error) {
	if !req.Confirm {
		return Status{}, fmt.Errorf(
			"%w: discarding uncommitted changes has to be confirmed", ErrInvalidRequest)
	}
	link, token, checkout, err := s.checkoutFor(ctx, req.Request)
	if err != nil {
		return Status{}, err
	}
	// On an unborn branch there is no HEAD to reset to, and `git reset --hard`
	// fails; the clean is the whole of the operation there.
	raw, err := s.rawStatus(ctx, checkout)
	if err != nil {
		return Status{}, err
	}
	// Both under one claim. As two, the reset could win its claim and the clean
	// lose the next one — the API answering 409 "the kernel is running a cell"
	// with the tracked changes already gone and only the untracked files left.
	// §5.11 item 6 requires this operation in particular not to take partial
	// effect silently, and a destructive half is the worst way to break that.
	sequence := [][]string{}
	if !raw.Unborn {
		sequence = append(sequence, []string{"reset", "--hard", "HEAD"})
	}
	sequence = append(sequence, []string{"clean", "--force", "-d"})

	results, err := checkout.runAll(ctx, sequence...)
	if err != nil {
		return Status{}, err
	}
	if err := checkout.batchFailure(sequence, results); err != nil {
		return Status{}, err
	}
	return s.statusOf(ctx, req.Request, link, token, false)
}

// Files is the Code pane's tree (D14): every file of the working copy, with
// nothing hidden and nothing reserved.
func (s *Service) Files(ctx context.Context, req Request) (FileTree, error) {
	link, err := s.linkFor(ctx, req)
	if err != nil {
		return FileTree{}, err
	}
	tree, err := s.workspace.Tree(ctx, s.ref(req, link), kernel.TreeRequest{
		Path:       link.Path,
		Recursive:  true,
		MaxEntries: s.opts.MaxTreeEntries,
		Exclude:    []string{".git"},
	})
	if err != nil {
		return FileTree{}, err
	}
	return FileTree{Root: link.Path, Tree: tree, Excluded: []string{".git"}}, nil
}

// ReadFile reads one file of the working copy.
func (s *Service) ReadFile(ctx context.Context, req Request, requested string) (File, error) {
	link, err := s.linkFor(ctx, req)
	if err != nil {
		return File{}, err
	}
	clean, err := relativePath(requested)
	if err != nil {
		return File{}, err
	}
	content, err := s.workspace.ReadFile(ctx, s.ref(req, link),
		path.Join(link.Path, clean), s.opts.MaxFileBytes)
	if err != nil {
		return File{}, err
	}
	file := File{
		Path:      clean,
		Size:      content.Size,
		Text:      content.Text,
		Binary:    content.Binary,
		Truncated: content.Truncated,
		Language:  languageOf(clean),
	}
	if content.Modified != nil {
		file.Modified = content.Modified.UTC().Format(time.RFC3339)
	}
	return file, nil
}

// WriteResult is one file, written.
type WriteResult struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Repository is which working copy it landed in. Carried because a caller that
	// is not looking at the pane — the `write_file` tool — may have been told about
	// a different repository earlier in the same session.
	Repository string `json:"repository"`
	// Committed is always false and is reported anyway, because it is the property
	// that matters: writing a file changes the working copy and nothing else
	// (§5.11 item 5). The `write_file` tool returns this to the model for the same
	// reason — so it does not tell the developer their change is pushed.
	Committed bool `json:"committed"`
}

// WriteFile writes one file of the working copy. Used by the Code pane's save and
// by the `write_file` tool of §5.8, which is the same operation with a different
// caller and must not be a second, laxer path.
func (s *Service) WriteFile(
	ctx context.Context, req Request, requested string, content []byte,
) (WriteResult, error) {
	link, err := s.linkFor(ctx, req)
	if err != nil {
		return WriteResult{}, err
	}
	clean, err := relativePath(requested)
	if err != nil {
		return WriteResult{}, err
	}
	if len(content) > s.opts.MaxFileBytes {
		return WriteResult{}, fmt.Errorf("%w: %s is %d bytes, and the limit is %d",
			ErrInvalidRequest, clean, len(content), s.opts.MaxFileBytes)
	}
	node, err := s.workspace.WriteFile(ctx, s.ref(req, link), path.Join(link.Path, clean), content)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Path: clean, Size: node.Size, Repository: link.FullName}, nil
}

// MakeDir creates a directory in the working copy. git does not track an empty
// one, which is why this exists at all: a developer laying out a package needs the
// directory before the file.
func (s *Service) MakeDir(ctx context.Context, req Request, requested string) error {
	link, err := s.linkFor(ctx, req)
	if err != nil {
		return err
	}
	clean, err := relativePath(requested)
	if err != nil {
		return err
	}
	_, err = s.workspace.MakeDir(ctx, s.ref(req, link), path.Join(link.Path, clean))
	return err
}

// Delete removes a file or a directory from the working copy.
//
// From the working copy, not from history: the deletion is a change like any
// other, and it reaches GitHub when the developer commits it.
func (s *Service) Delete(
	ctx context.Context, req Request, requested string, recursive bool,
) error {
	link, err := s.linkFor(ctx, req)
	if err != nil {
		return err
	}
	clean, err := relativePath(requested)
	if err != nil {
		return err
	}
	return s.workspace.Remove(ctx, s.ref(req, link), path.Join(link.Path, clean), recursive)
}

// checkoutFor is the preamble the mutating operations share.
func (s *Service) checkoutFor(
	ctx context.Context, req Request,
) (Link, string, gitContext, error) {
	link, err := s.linkFor(ctx, req)
	if err != nil {
		return Link{}, "", gitContext{}, err
	}
	token, err := s.tokenFor(ctx, req.UserSub)
	if err != nil {
		return Link{}, "", gitContext{}, err
	}
	checkout := s.git(req, link.WorkbenchID, link.Path, token)
	present, err := checkout.isRepository(ctx)
	if err != nil {
		return Link{}, "", gitContext{}, err
	}
	if !present {
		return Link{}, "", gitContext{}, ErrNotCloned
	}
	return link, token, checkout, nil
}

// git builds the context one git command runs in: whose pod, which workbench's
// kernel, which directory, and the credential.
//
// The workbench is passed rather than read from the request because the request
// may name none — the developer has one and did not have to say so — while the
// link that was resolved from it always names the concrete one. A command sent to
// the default kernel while the checkout belongs to a workbench would run in the
// right directory but the wrong process, and would queue behind whatever that
// process is doing.
// ref addresses the kernel a link's operations run in.
func (s *Service) ref(req Request, link Link) kernel.Ref {
	return kernel.Ref{Bearer: req.Bearer, Workbench: link.WorkbenchID}
}

func (s *Service) git(req Request, workbench, dir, token string) gitContext {
	return gitContext{
		workspace: s.workspace,
		ref:       kernel.Ref{Bearer: req.Bearer, Workbench: workbench},
		dir:       dir,
		token:     token,
		webURL:    s.opts.WebURL,
		template: kernel.Command{
			Timeout:        s.opts.CommandTimeout,
			MaxOutputBytes: s.opts.MaxCommandOutputBytes,
		},
	}
}

// linkFor is the repository a request acts on: the one selected in the workbench
// it names, or in the developer's only workbench when it names none.
func (s *Service) linkFor(ctx context.Context, req Request) (Link, error) {
	bench, err := s.resolve(ctx, req)
	if err != nil {
		return Link{}, err
	}
	if !bench.Selected() {
		return Link{}, ErrNoRepository
	}
	return bench.Link, nil
}

// putLink writes a changed link back into the workbench it belongs to.
func (s *Service) putLink(ctx context.Context, link Link) error {
	bench, err := s.Workbench(ctx, link.UserSub, link.WorkbenchID)
	if err != nil {
		return err
	}
	bench.Link = link
	bench.LastUsedAt = s.now()
	return s.store.PutWorkbench(ctx, bench)
}

// explainAuth turns "git could not authenticate" into an answer the developer can
// act on, by asking GitHub the one question git cannot: is the credential ODE holds
// still good?
//
// The two outcomes are different repairs, and telling them apart is the whole
// reason this exists. A revoked or expired token is ErrCredentialRejected, which
// the API answers as "reconnect your GitHub account" — a step the developer takes
// themselves. A credential the API still accepts means git in the pod could not use
// the one it was given, which is nothing the developer can fix by reconnecting, and
// saying so would send them round a loop that cannot help. That case keeps git's
// own text and gains a hint naming where to look.
//
// Anything that is not an authentication failure comes back untouched, and so does
// an authentication failure ODE cannot get a second opinion on — a check that fails
// must not turn a diagnosable error into a guess.
func (s *Service) explainAuth(ctx context.Context, req Request, err error) error {
	var gitErr *GitError
	if !errors.As(err, &gitErr) || !gitErr.authenticationFailed() {
		return err
	}
	client, clientErr := s.clientFor(ctx, req.UserSub)
	if clientErr != nil {
		// No stored identity at all is a better answer than anything about git.
		if errors.Is(clientErr, ErrNotConnected) {
			return clientErr
		}
		return err
	}
	if _, _, viewerErr := client.Viewer(ctx); viewerErr != nil {
		var upstream *UpstreamError
		if errors.As(viewerErr, &upstream) {
			switch upstream.Code {
			case http.StatusUnauthorized:
				// The only status that means the credential itself. GitHub answers 401
				// "Bad credentials" to a token that is revoked, expired or malformed.
				return fmt.Errorf("%w: GitHub answered %q; git said: %s",
					ErrCredentialRejected, upstream.Message, gitErr.Error())
			case http.StatusForbidden:
				// Not the credential. GitHub uses 403 for a rate limit and for a grant
				// too narrow for the resource, and both would send a developer through a
				// consent screen that cannot help — which is what this case existed to
				// stop happening. GitHub's own message is the useful part.
				explained := *gitErr
				explained.Hint = "GitHub answered 403 to ODE's own API call with this " +
					"credential (" + upstream.Message + "), which is a rate limit or a grant " +
					"too narrow rather than a credential to replace. GET /repo/connection?verify=true " +
					"reports what GitHub says about it."
				return &explained
			}
		}
		// GitHub unreachable, or anything else: ODE does not know, and git's own report
		// is the honest answer.
		return err
	}
	explained := *gitErr
	explained.Hint = credentialAliveHint
	return &explained
}

// credentialAliveHint is the case that used to be indistinguishable from a revoked
// token, named.
//
// git only asks the askpass helper when the Authorization header it was configured
// with was absent or refused, so a credential the API accepts and git could not use
// means the header did not reach git: an image whose git predates the
// GIT_CONFIG_COUNT environment configuration (2.31, March 2021), or a pod that
// strips the environment of a command.
const credentialAliveHint = "the GitHub credential ODE holds still works, so this is not a " +
	"reconnect: git in the pod could not use the credential it was given. Check that the " +
	"singleuser image has git 2.31 or newer, which is what reads GIT_CONFIG_COUNT, and see " +
	"GET /repo/connection?verify=true for what GitHub says about the credential."

func (s *Service) tokenFor(ctx context.Context, userSub string) (string, error) {
	stored, found, err := s.store.GetIdentity(ctx, userSub)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotConnected
	}
	return s.sealer.Open(stored.SealedToken)
}

func (s *Service) clientFor(ctx context.Context, userSub string) (*githubClient, error) {
	token, err := s.tokenFor(ctx, userSub)
	if err != nil {
		return nil, err
	}
	return s.githubClient(token), nil
}

// checkoutPath is where a repository's working copy goes: owner and name, both
// sanitised, under the workspace. §5.6 calls it a stable path, and stable is the
// operative word — the same repository has to land in the same place next session
// for the reuse to work.
//
// The owner is in the path because the name alone does not identify a repository.
// `institut/pump-detector` and `jonah/pump-detector` are two repositories, and a
// directory serving both would carry one of them a checkout whose origin is the
// other — which is a commit and a push into a repository the developer did not
// choose, rather than an inconvenience.
func checkoutPath(owner, name string) string {
	if strings.TrimSpace(owner) == "" {
		return sanitiseSegment(name)
	}
	return sanitiseSegment(owner) + "/" + sanitiseSegment(name)
}

var unsafeSegment = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitiseSegment(name string) string {
	cleaned := unsafeSegment.ReplaceAllString(strings.TrimSpace(name), "-")
	cleaned = strings.Trim(cleaned, "-.")
	if cleaned == "" {
		return "repository"
	}
	return cleaned
}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func validRepositoryName(name string) bool {
	return repositoryName.MatchString(name) && name != "." && name != ".."
}

// relativePath validates a path inside the working copy.
//
// The kernel already refuses to leave the *workspace*, which is a wider boundary
// than this one: `../notebooks/scratch.ipynb` stays inside the workspace while
// leaving the repository, and a write there would land outside the working copy
// the developer thinks they are editing.
//
// `.git` is the one exception to D14's "every file". A developer has read and
// write on every file of their repository; the object database is not one of those
// files, it is git's own bookkeeping, and a Code pane save into it corrupts the
// checkout in a way nothing in ODE could explain afterwards.
func relativePath(requested string) (string, error) {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return "", fmt.Errorf("%w: no path was given", ErrInvalidRequest)
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("%w: %q is absolute; paths are relative to the repository",
			ErrInvalidRequest, requested)
	}
	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q leaves the repository", ErrInvalidRequest, requested)
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == ".git" {
			return "", fmt.Errorf(
				"%w: .git is git's own storage rather than a file of the repository, and "+
					"editing it would corrupt the checkout", ErrInvalidRequest)
		}
	}
	return clean, nil
}

// imageReference is where the scaffolded workflow pushes. Lower-cased because a
// container reference may not carry upper case and GitHub owners often do.
func imageReference(fullName string) string {
	return "ghcr.io/" + strings.ToLower(fullName)
}

// sameRemote compares two clone URLs loosely: the same repository reached over
// https with or without a .git suffix is the same remote, and reporting a mismatch
// there would be noise.
func sameRemote(actual, expected string) bool {
	normalise := func(url string) string {
		url = strings.TrimSuffix(strings.TrimSpace(url), "/")
		url = strings.TrimSuffix(url, ".git")
		url = strings.TrimPrefix(url, "https://")
		url = strings.TrimPrefix(url, "http://")
		url = strings.TrimPrefix(url, "git@")
		return strings.ToLower(strings.ReplaceAll(url, ":", "/"))
	}
	return normalise(actual) == normalise(expected)
}

// languageOf is the editor's syntax hint. Derived here rather than in the SPA so
// that the pane and anything else reading a file agree on what it is.
func languageOf(name string) string {
	switch strings.ToLower(path.Base(name)) {
	case "dockerfile":
		return "dockerfile"
	case "makefile":
		return "makefile"
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".py":
		return "python"
	case ".yml", ".yaml":
		return "yaml"
	case ".json":
		return "json"
	case ".toml":
		return "toml"
	case ".md":
		return "markdown"
	case ".sh":
		return "shell"
	case ".txt", "":
		return "plaintext"
	default:
		return "plaintext"
	}
}
