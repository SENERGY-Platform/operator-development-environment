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

// A workbench is one working context: a repository checkout on the developer's
// PVC and the kernel that runs in it.
//
// It exists because a developer works on more than one operator. Before it, the
// selected repository was keyed by the developer alone, so two chat sessions
// shared one working copy and selecting a repository in one changed where the
// other's write_file landed. A workbench makes that context plural and gives each
// chat session one to act in.
//
// Two rules run through everything below. A workbench holds at most one
// repository, and a repository is in at most one of a developer's workbenches —
// the checkout is at {owner}/{name} under the workspace, so a second workbench on
// the same repository would put two kernels and two streams of git commands in one
// working tree. And a workbench is cheap: creating one costs a row, and the
// expensive things — the clone, the pod, the kernel — happen when it is used.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoWorkbench means the id names no workbench of this developer's.
//
// The same answer for "does not exist" and "belongs to someone else", for the
// reason chat sessions give: an id in a URL must not be enough to learn that
// somebody else's exists.
var ErrNoWorkbench = errors.New("no such workbench")

// ErrRepositoryInUse is the one-repository-per-workbench rule refusing.
//
// Its own error rather than ErrInvalidRequest because the repair is a choice
// between two reasonable actions — work in the workbench that already has it, or
// pick a different repository — and the answer names the workbench holding it so
// the developer can take either.
var ErrRepositoryInUse = errors.New("that repository is open in another workbench")

// ErrTooManyWorkbenches is the per-developer ceiling. Each workbench is a kernel
// process in the developer's pod, so the limit is about the pod's memory rather
// than about ODE's storage.
var ErrTooManyWorkbenches = errors.New("this developer has as many workbenches as are allowed")

// DefaultMaxWorkbenches is the ceiling when a deployment configures none. Three
// is not a technical bound: it is what a pod sized for the ODE profile carries
// without the kernels competing for memory, and a developer who needs more is
// better served by a deployment that says so than by an OOM kill mid-run.
const DefaultMaxWorkbenches = 3

// Workbench is one working context.
//
// Link is embedded rather than referenced: a workbench without a repository and a
// repository without a workbench are both states nothing can act on, and the two
// are created and forgotten together. Link.FullName is empty between creating a
// workbench and selecting what to work on in it.
type Workbench struct {
	ID      string `json:"id"`
	UserSub string `json:"-"`
	// Title is the developer's own name for it. Empty falls back to the repository
	// and, before there is one, to the id — see Label.
	Title string `json:"title,omitempty"`
	// Link is the repository this workbench works on. Zero until one is selected.
	Link      Link      `json:"link"`
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt orders the list and is what a picker shows first. Not a liveness
	// signal: whether a kernel is actually running is the kernel surface's answer.
	LastUsedAt time.Time `json:"last_used_at"`
}

// Selected says whether a repository has been chosen in this workbench.
func (w Workbench) Selected() bool { return w.Link.FullName != "" }

// Label is what a surface shows when it has room for one string.
func (w Workbench) Label() string {
	if w.Title != "" {
		return w.Title
	}
	if w.Link.FullName != "" {
		return w.Link.FullName
	}
	return w.ID
}

// MaxWorkbenches is the ceiling this deployment allows, reported so the SPA can
// disable the button rather than let a click fail.
func (s *Service) MaxWorkbenches() int { return s.opts.MaxWorkbenches }

// Workbenches lists what the developer has open, oldest first.
//
// Oldest first rather than most-recently-used, deliberately: this is the list a
// picker renders, and a list that reorders itself as the developer works moves the
// entry they are aiming at out from under the cursor.
//
// It is also the adoption point. A developer who selected a repository before
// workbenches existed has a row in ode_repo_links and no workbench; that row
// becomes their first workbench here, once, so the checkout already on their PVC
// stays the one they are working in.
func (s *Service) Workbenches(ctx context.Context, userSub string) ([]Workbench, error) {
	benches, err := s.store.Workbenches(ctx, userSub)
	if err != nil {
		return nil, err
	}
	if len(benches) > 0 {
		return benches, nil
	}
	adopted, err := s.adopt(ctx, userSub)
	if errors.Is(err, ErrRepositoryInUse) {
		// A concurrent request adopted the same row a moment earlier.
		//
		// Every request that names no workbench comes through here, so one page load
		// sends several at once, they all find the list empty, and they all try to
		// adopt. The unique index lets one write and refuses the rest — which is the
		// index doing its job, but it is not this caller's answer: the workbench it
		// was about to create exists now. So it reads the list again instead of
		// reporting a conflict between the developer and themselves.
		return s.store.Workbenches(ctx, userSub)
	}
	if err != nil {
		return nil, err
	}
	if adopted == nil {
		return nil, nil
	}
	return []Workbench{*adopted}, nil
}

// adopt turns a pre-workbench link into this developer's first workbench.
//
// Lazy rather than a migration pass at startup, for two reasons. The ids are
// minted here rather than by the database, because they appear in URLs and a
// database-assigned one would be guessable. And a startup pass would have to run
// on every boot to catch a deployment that rolled back and forward again, while
// this runs exactly once per developer, the first time they need it.
//
// Once means once: the row is stamped adopted afterwards, so a developer who
// later closes their only workbench gets an empty list rather than the link they
// closed, handed back with a new id.
//
// Returns nil when there is nothing to adopt, which is the ordinary case for
// every developer who arrives after this change.
func (s *Service) adopt(ctx context.Context, userSub string) (*Workbench, error) {
	link, found, err := s.store.GetLegacyLink(ctx, userSub)
	if err != nil || !found {
		return nil, err
	}
	now := s.now()
	bench := Workbench{
		ID:         s.ids.NewID(),
		UserSub:    userSub,
		Link:       link,
		CreatedAt:  now,
		LastUsedAt: now,
	}
	bench.Link.WorkbenchID = bench.ID
	bench.Link.UserSub = userSub
	if err := s.store.PutWorkbench(ctx, bench); err != nil {
		return nil, err
	}
	// Stamped after the write and not before, so a row whose workbench failed to
	// appear stays adoptable rather than pointing at a checkout nothing holds.
	//
	// Best effort, like touch below: the workbench exists either way, and failing an
	// adoption that succeeded would be the wrong trade. A stamp that did not land
	// costs a second attempt later, which the caller above already handles.
	_ = s.store.MarkLegacyAdopted(ctx, userSub, now)
	return &bench, nil
}

// Workbench reads one, checking that it is this developer's.
func (s *Service) Workbench(ctx context.Context, userSub, id string) (Workbench, error) {
	if strings.TrimSpace(id) == "" {
		return Workbench{}, ErrNoWorkbench
	}
	bench, found, err := s.store.Workbench(ctx, id)
	if err != nil {
		return Workbench{}, err
	}
	if !found || bench.UserSub != userSub {
		return Workbench{}, ErrNoWorkbench
	}
	return bench, nil
}

// CreateWorkbench opens an empty one. Nothing is cloned and no pod is touched:
// the repository is chosen afterwards, by Select or Create.
func (s *Service) CreateWorkbench(ctx context.Context, userSub, title string) (Workbench, error) {
	existing, err := s.Workbenches(ctx, userSub)
	if err != nil {
		return Workbench{}, err
	}
	if len(existing) >= s.opts.MaxWorkbenches {
		return Workbench{}, fmt.Errorf("%w: %d of %d are open",
			ErrTooManyWorkbenches, len(existing), s.opts.MaxWorkbenches)
	}
	// The per-user ceiling, when a deployment sets one. It narrows rather than
	// widens: the deployment cap above is about how many kernels a pod's memory
	// carries, and no admin setting makes a pod bigger.
	if s.limits != nil {
		if err := s.limits.CheckWorkbenchCount(ctx, userSub, len(existing)); err != nil {
			return Workbench{}, fmt.Errorf("%w: %v", ErrTooManyWorkbenches, err)
		}
	}
	now := s.now()
	bench := Workbench{
		ID:         s.ids.NewID(),
		UserSub:    userSub,
		Title:      strings.TrimSpace(title),
		CreatedAt:  now,
		LastUsedAt: now,
	}
	if err := s.store.PutWorkbench(ctx, bench); err != nil {
		return Workbench{}, err
	}
	return bench, nil
}

// RenameWorkbench sets the developer's own name for it. An empty title clears it,
// which puts the label back to the repository.
func (s *Service) RenameWorkbench(
	ctx context.Context, userSub, id, title string,
) (Workbench, error) {
	bench, err := s.Workbench(ctx, userSub, id)
	if err != nil {
		return Workbench{}, err
	}
	bench.Title = strings.TrimSpace(title)
	if err := s.store.PutWorkbench(ctx, bench); err != nil {
		return Workbench{}, err
	}
	return bench, nil
}

// DeleteWorkbench forgets one.
//
// The checkout stays on the PVC. That is the same judgement Unlink already makes
// and §5.11 item 6 requires: the working copy is the developer's, it may hold
// uncommitted work, and ODE does not discard it on its own initiative. Opening a
// workbench on the same repository again finds the directory and reuses it.
//
// The kernel is not shut down here either. It belongs to the pod, ODE's hold on it
// is released by the reaper, and shutting it down would end whatever is running in
// it — which is exactly the thing a developer might have deleted the workbench
// while forgetting about.
func (s *Service) DeleteWorkbench(ctx context.Context, userSub, id string) error {
	if _, err := s.Workbench(ctx, userSub, id); err != nil {
		return err
	}
	return s.store.DeleteWorkbench(ctx, id)
}

// Checkout implements kernel.Workbenches: it says which directory this
// workbench's kernel runs in, relative to the workspace root.
//
// Empty is a legitimate answer and means the workspace root — a workbench with no
// repository selected yet, or a request naming no workbench at all. The kernel
// surface treats it as "no opinion" rather than an error, because a developer
// opening ODE before choosing anything should still get a kernel.
func (s *Service) Checkout(ctx context.Context, userSub, workbenchID string) (string, error) {
	if strings.TrimSpace(workbenchID) == "" {
		return "", nil
	}
	bench, err := s.Workbench(ctx, userSub, workbenchID)
	if errors.Is(err, ErrNoWorkbench) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return bench.Link.Path, nil
}

// resolve turns a request into the workbench it acts in.
//
// A request that names none is answered with the developer's only workbench, and
// that default is what keeps every existing caller working: a deployment with one
// workbench per developer behaves exactly as it did before this existed. With
// several open, a request that names none is a caller that has not been taught
// about them, and guessing between two working copies is the failure this whole
// change is about — so it is refused rather than guessed.
func (s *Service) resolve(ctx context.Context, req Request) (Workbench, error) {
	if id := strings.TrimSpace(req.WorkbenchID); id != "" {
		return s.Workbench(ctx, req.UserSub, id)
	}
	benches, err := s.Workbenches(ctx, req.UserSub)
	if err != nil {
		return Workbench{}, err
	}
	switch len(benches) {
	case 0:
		return Workbench{}, ErrNoRepository
	case 1:
		return benches[0], nil
	default:
		return Workbench{}, fmt.Errorf(
			"%w: %d workbenches are open, so the request has to name the one it means",
			ErrInvalidRequest, len(benches))
	}
}

// forSelection resolves the workbench a repository is about to be selected into,
// creating one when the developer has none.
//
// Creating on demand is what makes selecting a repository a single action for a
// developer who has never seen a workbench: they pick a repository, and the
// workbench that holds it comes into being underneath. It also refuses here rather
// than after the clone if the repository is already open elsewhere, so the
// developer is told before anything touches their PVC.
func (s *Service) forSelection(
	ctx context.Context, req Request, fullName string,
) (Workbench, error) {
	benches, err := s.Workbenches(ctx, req.UserSub)
	if err != nil {
		return Workbench{}, err
	}

	for _, bench := range benches {
		if !strings.EqualFold(bench.Link.FullName, fullName) {
			continue
		}
		if bench.ID == strings.TrimSpace(req.WorkbenchID) || req.WorkbenchID == "" {
			// Re-selecting what is already there: the ordinary way a developer returns
			// to a workbench, and the path that reuses the checkout.
			return bench, nil
		}
		return Workbench{}, fmt.Errorf("%w: %s is open in %q",
			ErrRepositoryInUse, fullName, bench.Label())
	}

	if id := strings.TrimSpace(req.WorkbenchID); id != "" {
		return s.Workbench(ctx, req.UserSub, id)
	}
	if len(benches) == 1 {
		return benches[0], nil
	}
	if len(benches) > 1 {
		return Workbench{}, fmt.Errorf(
			"%w: %d workbenches are open, so the request has to name the one to select into",
			ErrInvalidRequest, len(benches))
	}
	return s.CreateWorkbench(ctx, req.UserSub, "")
}

// touch records that a workbench was used, so a picker can order by it.
//
// Best effort: it is ordering information, and failing an operation the developer
// asked for because a timestamp could not be written would be the wrong trade.
func (s *Service) touch(ctx context.Context, bench Workbench) {
	bench.LastUsedAt = s.now()
	_ = s.store.PutWorkbench(ctx, bench)
}
