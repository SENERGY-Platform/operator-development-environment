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

package kernel

// One developer's hold on their pod, and one kernel per workbench inside it.
//
// The split is the point. A developer has one pod — one spawn, one minted token,
// one keep-alive — and as many kernels in it as they have workbenches, because a
// singleuser server runs any number of them and each gets its own working
// directory. So a training run in one operator's workbench no longer makes a file
// read in another one wait, or answer 409.
//
// What state belongs where follows from that. Anything the Hub knows about — the
// server URL, the per-user token, the activity that stops the idle culler — is on
// the pod and shared. Anything a kernel has — its id, its socket, the platform
// token pushed into it, and the one-cell-at-a-time hold — is on the bench.
//
// Two locks, and the order between them is never the other way round: a bench's
// mutex may be held while taking its pod's, because bringing a kernel up needs the
// server and the token. Never pod-then-bench. Everything that walks the benches of
// a pod — the reaper, a shutdown — takes its snapshot under the service mutex and
// lets it go before touching a bench.
//
// The operations in this file are what the API routes and the run_code tool call.
// The six methods of §5.6 in kernel.go are what they are assembled from; the
// difference is that these bring the session up first, so no caller has to
// remember the token push, the workspace or the keep-alive.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ref addresses one kernel: whose pod, and which of their workbenches.
//
// An empty Workbench is a caller that has no opinion, which is every caller that
// predates workbenches and every deployment serving one working copy per
// developer. It names the pod's default bench, so the behaviour is exactly what it
// was before this existed.
type Ref struct {
	// Bearer is the developer's platform token. It says who they are, is forwarded
	// into the kernel (§5.6 item 4), and is never logged.
	Bearer string
	// Workbench is the working context, as pkg/repo mints it.
	Workbench string
	// WithPlatformToken asks for the developer's token to be installed in the
	// kernel for this execution. Only consulted under Options.ContainCells, which
	// is what makes withholding it possible at all; without that option every
	// execution receives the token as it always has.
	//
	// It says what the execution needs, not who is asking. Run sets it for the
	// developer's own pane; a tool call sets it when the model has been told by a
	// failed contained run that the cell cannot do without it, and that is the call
	// the developer confirms.
	WithPlatformToken bool
}

// Workbenches says which directory a workbench's kernel runs in.
//
// Declared here and implemented by pkg/repo so the dependency points one way: the
// repository surface is built on the kernel, and a kernel package that imported it
// back would be a cycle. Empty is a legitimate answer — a workbench with no
// repository selected yet — and means the workspace root.
type Workbenches interface {
	Checkout(ctx context.Context, userSub, workbench string) (string, error)
}

// Status is what the SPA and the tool report about one kernel.
type Status struct {
	User          string     `json:"user"`
	ServerReady   bool       `json:"server_ready"`
	ServerPending string     `json:"server_pending,omitempty"`
	ServerURL     string     `json:"server_url,omitempty"`
	Started       *time.Time `json:"started,omitempty"`
	LastActivity  *time.Time `json:"last_activity,omitempty"`

	// Workbench is which working context this kernel belongs to, empty for the
	// default one.
	Workbench string `json:"workbench,omitempty"`
	KernelID  string `json:"kernel_id,omitempty"`
	// KernelCount is how many kernels ODE is holding in this pod. Reported because
	// each one is a Python process in a pod with one memory limit, and a developer
	// wondering why their run was killed should be able to see it.
	KernelCount int    `json:"kernel_count"`
	KernelName  string `json:"kernel_name,omitempty"`
	// Profile is the KubeSpawner profile ODE spawns with, empty for the default.
	Profile string `json:"profile,omitempty"`
	// Busy is true while an execution ODE started is still running in this kernel.
	// One workbench's own kernel only: a busy workbench says nothing about another.
	Busy bool `json:"busy"`
	// Workspace is the persistent working directory tree, the root of everything on
	// the PVC.
	Workspace string `json:"workspace"`
	// Directory is where this kernel actually runs, workspace-relative and empty
	// for the root. It is the workbench's checkout, so `open("notes.txt")` in a
	// cell lands next to the operator's code.
	Directory string `json:"directory,omitempty"`
	// WorkspaceReady says the directory exists on the PVC.
	WorkspaceReady bool `json:"workspace_ready"`
}

// runToken identifies one execution's hold on a kernel. Zero is "no hold": it is
// never handed out, so a token compared against it is always a live one.
type runToken uint64

// pod is ODE's hold on one developer's singleuser server.
//
// Everything here is shared by every kernel in it, and that sharing is what keeps
// a second workbench cheap: no second spawn, no second token, no second keep-alive
// loop reporting the same activity twice.
type pod struct {
	user User

	mux         sync.Mutex
	serverURL   string
	token       HubToken
	tokenExpiry time.Time
	// generation counts how many times the pod ODE was addressing turned out to be
	// gone. A bench holding an older generation is holding a kernel id and a socket
	// that belonged to a pod which no longer exists, and drops both on its next
	// use. That is how a sibling workbench finds out, without any bench ever having
	// to reach into another one's state under a lock it does not hold.
	generation uint64
	// live counts the benches keeping this pod up. The keep-alive runs while it is
	// above zero and stops when the last bench lets go — an off-by-one either way
	// is a pod culled with a training run in it, or one held alive after everyone
	// went home, so it is only ever changed through hold and release below.
	live      int
	keepalive context.CancelFunc
}

// bench is one workbench's kernel.
//
// Its mutex serialises execution: one workbench, one cell at a time. ipykernel
// would queue a second request on the same kernel anyway, and holding that here
// means the second caller is answered rather than left waiting on a kernel that is
// busy with something they cannot see.
type bench struct {
	pod       *pod
	workbench string

	mux sync.Mutex
	// dir is where this kernel was started, workspace-relative. Compared against
	// the workbench's current checkout on every bring-up: a kernel left running in
	// a directory that is no longer the workbench's would write the developer's
	// files into the previous operator's checkout.
	dir string
	// generation is the pod generation this bench's kernel belongs to.
	generation uint64
	kernelID   string
	conn       *connection
	// pushedToken is the platform token currently installed in this kernel, and the
	// empty string when none is — which under Options.ContainCells is the ordinary
	// state rather than an error. Held to notice a refresh and to notice that the
	// token has to come out again before the next contained cell, and never logged.
	pushedToken string
	// environmentReady says the hidden environment cell landed. Separate from
	// pushedToken because the interesting state is now reachable with no token in
	// it: "" alone cannot distinguish a kernel deliberately left contained from one
	// whose environment was never installed, and the second must not be run in.
	environmentReady bool
	workspaceReady   bool
	// held says this bench is counted in pod.live.
	held bool
	// running names the execution that currently holds the kernel, zero when none
	// does. A token rather than a flag because the execution that took the kernel
	// is not always the one that finishes last: a Restart ends a cell and brings a
	// new kernel up while the ended cell is still forwarding into a slow browser,
	// and that cell must not release a kernel a later execution has since claimed.
	running runToken
	// runs counts the executions this bench has claimed. Only ever incremented, so
	// a token is never reused and a stale finish can never match a live run.
	runs runToken
	// idle is closed when the run named by running lets the kernel go, and nil
	// whenever running is zero. It is how a caller that is willing to wait hears
	// that the kernel is free — closed rather than sent on, because every waiter
	// has to be woken and only one of them will win the retry.
	idle chan struct{}
	// waiting counts the callers sitting on idle right now, so a queue too deep to
	// serve inside anyone's bound is answered instead of joined. See maxQueuedClaims.
	waiting  int
	lastUsed time.Time
}

// benchKey addresses one kernel in the service's map.
//
// The separator is a NUL because a Hub username cannot contain one — see
// validateUsername — so no pair of (user, workbench) can collide with another.
func benchKey(user, workbench string) string { return user + "\x00" + workbench }

// UseWorkbenches tells the service where to find a workbench's checkout.
//
// Set after construction rather than in Options because the repository surface is
// built on top of this one: it takes the kernel service as its workspace, so it
// cannot exist yet when the kernel service is made. Without it every kernel runs
// in the workspace root, which is a supported deployment — an ODE with no GitHub
// app has no checkouts to run in.
func (s *Service) UseWorkbenches(workbenches Workbenches) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.workbenches = workbenches
}

// pod returns the developer's pod record, creating it if this is the first time.
func (s *Service) pod(user User) *pod {
	s.mux.Lock()
	defer s.mux.Unlock()
	existing, found := s.pods[user.Name]
	if found {
		return existing
	}
	created := &pod{user: user}
	s.pods[user.Name] = created
	return created
}

// bench returns one workbench's kernel record, creating it if this is the first
// time. Creating one starts nothing: the pod, the kernel and the socket come up on
// first use.
func (s *Service) bench(user User, workbench string) *bench {
	container := s.pod(user)

	s.mux.Lock()
	defer s.mux.Unlock()
	key := benchKey(user.Name, workbench)
	if existing, found := s.benches[key]; found {
		return existing
	}
	created := &bench{pod: container, workbench: workbench, lastUsed: time.Now()}
	s.benches[key] = created
	return created
}

// lookupBench finds an existing one without creating it.
func (s *Service) lookupBench(user User, workbench string) (*bench, bool) {
	s.mux.Lock()
	defer s.mux.Unlock()
	existing, found := s.benches[benchKey(user.Name, workbench)]
	return existing, found
}

// benchesOf snapshots every bench of one developer.
//
// A snapshot rather than a walk under the lock, because every caller then has to
// take a bench mutex, and taking one while holding the service mutex is the
// deadlock this file's ordering rule exists to prevent.
func (s *Service) benchesOf(user User) []*bench {
	s.mux.Lock()
	defer s.mux.Unlock()
	var found []*bench
	prefix := user.Name + "\x00"
	for key, existing := range s.benches {
		if strings.HasPrefix(key, prefix) {
			found = append(found, existing)
		}
	}
	return found
}

// resolver returns the checkout source, if a deployment configured one.
func (s *Service) resolver() Workbenches {
	s.mux.Lock()
	defer s.mux.Unlock()
	return s.workbenches
}

// checkoutFor asks where this workbench's kernel should run.
//
// Asked on every bring-up rather than remembered, which is deliberate: a developer
// who selects a different repository into a workbench moves its checkout, and a
// kernel left running in the old directory would put their next `open(...)` in the
// previous operator's working copy. The cost is one indexed row read per kernel
// operation, against a round trip to the pod and a Python cell — and the
// alternative is the repository surface having to remember to tell this one, which
// is a second mechanism to keep in step with the first.
//
// A failure to answer is not a failure to run: the workspace root is always a
// usable directory, and refusing a developer their kernel because a database read
// failed would turn a degraded store into no kernel at all. It is logged, because
// a kernel silently running one directory up from the operator's code is the kind
// of thing that gets diagnosed as "my files went missing".
func (s *Service) checkoutFor(ctx context.Context, user User, workbench string) string {
	resolver := s.resolver()
	if resolver == nil || workbench == "" {
		return ""
	}
	dir, err := resolver.Checkout(ctx, user.Sub, workbench)
	if err != nil {
		slog.WarnContext(ctx, "could not resolve a workbench's checkout; "+
			"its kernel runs in the workspace root",
			"user", user.Name, "workbench", workbench, "error", err)
		return ""
	}
	clean, err := cleanWorkspacePath(dir)
	if err != nil {
		slog.WarnContext(ctx, "a workbench's checkout is not a usable workspace path",
			"user", user.Name, "workbench", workbench, "error", err)
		return ""
	}
	return clean
}

// ---- The session-level operations everything above ODE actually calls ----

// Ensure brings one workbench's kernel up: server, token, kernel, workspace and
// the platform token inside it. Safe to call on every session open.
func (s *Service) Ensure(ctx context.Context, ref Ref) (Status, error) {
	user, err := s.UserFor(ref.Bearer)
	if err != nil {
		return Status{}, err
	}
	target := s.bench(user, ref.Workbench)
	target.mux.Lock()
	defer target.mux.Unlock()

	if _, err := s.ensureLocked(ctx, target, ref); err != nil {
		return Status{}, err
	}
	return s.statusLocked(ctx, target)
}

// Status reports what is running, without starting anything.
func (s *Service) Status(ctx context.Context, ref Ref) (Status, error) {
	user, err := s.UserFor(ref.Bearer)
	if err != nil {
		return Status{}, err
	}
	target, found := s.lookupBench(user, ref.Workbench)
	if !found {
		state, err := s.hub.ServerState(ctx, user.Name)
		if err != nil {
			return Status{}, err
		}
		return Status{
			User:          user.Name,
			ServerReady:   state.Ready,
			ServerPending: state.Pending,
			ServerURL:     state.URL,
			Started:       state.Started,
			LastActivity:  state.LastActivity,
			Workbench:     ref.Workbench,
			KernelCount:   s.kernelCount(user, nil),
			Profile:       s.opts.Profile,
			Workspace:     s.opts.WorkspacePath,
			Directory:     s.checkoutFor(ctx, user, ref.Workbench),
		}, nil
	}
	target.mux.Lock()
	defer target.mux.Unlock()
	return s.statusLocked(ctx, target)
}

// Run executes a developer's own cell in one workbench's kernel.
//
// It is the only entry point that both brings the session up and runs something,
// which is deliberate: every caller then gets the token push, the keep-alive and
// the workspace without having to remember them.
//
// No wait: a developer's cell that finds their own workbench busy is exactly the
// case ErrBusy reports, and the pane's answer is the interrupt rather than a
// queue — they are sitting in front of it and can decide. Another workbench being
// busy does not reach here at all.
// The developer's own cell always carries the platform token. Options.ContainCells
// withholds it from the assistant, whose cells are the ones a confirmation exists
// to check; a console that cannot reach the platform is not a console, and the
// developer typing into it is the party whose authority the token already is.
func (s *Service) Run(
	ctx context.Context, ref Ref, code string,
) (<-chan ExecutionEvent, error) {
	ref.WithPlatformToken = true
	return s.run(ctx, ref, code, 0)
}

// RunQueued executes the assistant's code, queueing behind a busy kernel.
//
// The difference from Run is the answer to "who is waiting". A tool call is not a
// person in front of a pane: refusing it produces
// `{"error":"this kernel is already running code"}` in the model's context, from
// which the best it can do is guess how long to leave it and ask again — and the
// two callers it collides with are a cell the developer started seconds ago and
// ODE's own workspace operations, both of which are over in seconds. Waiting is
// what the developer would have done.
//
// The bound is Options.AssistantWait, and its expiry still answers ErrBusy: past
// that, the kernel is held by something long enough that the model should be told
// rather than left hanging on a tool call the provider will eventually give up on.
func (s *Service) RunQueued(
	ctx context.Context, ref Ref, code string,
) (<-chan ExecutionEvent, error) {
	return s.run(ctx, ref, code, s.opts.AssistantWait)
}

func (s *Service) run(
	ctx context.Context, ref Ref, code string, wait time.Duration,
) (<-chan ExecutionEvent, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("%w: there is no code to run", ErrInvalidRequest)
	}
	target, conn, handle, user, run, err := s.claim(ctx, ref, wait)
	if err != nil {
		return nil, err
	}

	// The cell's own deadline, descending from the caller so that a disconnected
	// developer still stops their cell, and bounded so a runaway one does not hold
	// the kernel forever.
	executeCtx, cancel := context.WithTimeout(ctx, s.opts.ExecuteTimeout)

	raw, err := conn.execute(executeCtx, code, executeOptions{
		MaxOutputBytes: s.opts.MaxOutputBytes,
		OnCancel: func() {
			interruptCtx, interruptCancel := context.WithTimeout(
				context.WithoutCancel(ctx), s.opts.RequestTimeout)
			defer interruptCancel()
			if err := s.Interrupt(interruptCtx, handle); err != nil {
				slog.Warn("interrupting a cancelled execution failed", "user", user.Name, "error", err)
			}
		},
	})
	if err != nil {
		cancel()
		s.finishRun(target, run)
		return nil, err
	}

	// Wrapped so that the busy flag and the execution deadline are released when
	// the cell ends, whatever ends it.
	out := make(chan ExecutionEvent, 32)
	go func() {
		defer close(out)
		defer cancel()
		defer s.finishRun(target, run)
		for event := range raw {
			out <- event
		}
	}()
	return out, nil
}

// claim brings a workbench's kernel up and marks it busy for one execution.
//
// Shared by Run and by the workspace operations of workspace.go, which need the
// same four things in the same order: the developer resolved from their own
// token, a bench that is not already running a cell, a live connection, and the
// handle an interrupt would need. Extracted rather than repeated so that the busy
// check cannot be forgotten on one path — a kernel executes one cell at a time,
// and a second request that skipped it would silently queue behind the first.
//
// wait is how long the caller is prepared to sit behind whatever holds this
// kernel, and the two callers answer it differently on purpose. A developer's own
// cell waits for nothing: it is the thing the 409 exists to report, and a browser
// told "busy" can offer the interrupt. ODE's own workspace operations wait,
// because the collisions they lose are with each other — a page reload issues a
// status read, a tree read and a file read at once, each a cell of a few
// milliseconds, and refusing two of the three is ODE getting in its own way. What
// the bound preserves is the case the 409 was written for: a wait that expires is
// a kernel busy with something longer than any repository operation, and that is
// still reported rather than waited out.
func (s *Service) claim(
	ctx context.Context, ref Ref, wait time.Duration,
) (*bench, *connection, KernelHandle, User, runToken, error) {
	user, err := s.UserFor(ref.Bearer)
	if err != nil {
		return nil, nil, KernelHandle{}, User{}, 0, err
	}
	target := s.bench(user, ref.Workbench)
	deadline := time.Now().Add(wait)

	for {
		target.mux.Lock()
		if target.running == 0 {
			handle, err := s.ensureLocked(ctx, target, ref)
			if err != nil {
				target.mux.Unlock()
				return nil, nil, KernelHandle{}, user, 0, err
			}
			conn := target.conn
			run := target.takeLocked()
			target.lastUsed = time.Now()
			target.mux.Unlock()
			return target, conn, handle, user, run, nil
		}
		// Read under the lock, waited on outside it: the run holding the kernel
		// releases it through the same mutex, so holding it here would be the
		// deadlock.
		idle := target.idle
		queued := target.waiting
		target.mux.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 || idle == nil {
			return nil, nil, KernelHandle{}, user, 0, ErrBusy
		}
		// A queue this deep is not a collision any more.
		//
		// The cap is not about memory — a waiter is a goroutine on a timer. It is
		// that a caller behind eight others will not be served inside any bound
		// worth reporting, and answering it now is more use than answering it in
		// four minutes. It also keeps a caller that retries on refusal from turning
		// one busy kernel into an unbounded backlog.
		if queued >= maxQueuedClaims {
			return nil, nil, KernelHandle{}, user, 0, fmt.Errorf(
				"%w: %d callers are already waiting for it", ErrBusy, queued)
		}

		target.mux.Lock()
		target.waiting++
		target.mux.Unlock()
		timer := time.NewTimer(remaining)
		select {
		case <-idle:
			timer.Stop()
			// Free, and the retry may still lose it to another waiter — every one of
			// them was woken. The loop is what makes that safe rather than a race.
			//
			// Deliberately not a queue with an order. Waiters here are independent
			// callers — a tool call in one conversation, a workspace read in another,
			// a cell the developer started — and there is no order between them to
			// preserve; the calls that do have one, an assistant's own successive
			// tool calls, are issued one after the other and never race. What the
			// cap above bounds is the starvation this leaves possible.
		case <-timer.C:
			timer.Stop()
			target.mux.Lock()
			target.waiting--
			target.mux.Unlock()
			return nil, nil, KernelHandle{}, user, 0, fmt.Errorf(
				"%w: it was still busy after %s", ErrBusy, wait)
		case <-ctx.Done():
			timer.Stop()
			target.mux.Lock()
			target.waiting--
			target.mux.Unlock()
			return nil, nil, KernelHandle{}, user, 0, ctx.Err()
		}
		target.mux.Lock()
		target.waiting--
		target.mux.Unlock()
	}
}

// takeLocked marks the kernel taken by a new run and returns its token.
//
// Paired with freeLocked so that "running is set" and "there is a channel to wake
// waiters with" cannot come apart: a run recorded without one would leave every
// waiting caller on the timer instead of on the release.
func (b *bench) takeLocked() runToken {
	b.runs++
	b.running = b.runs
	b.idle = make(chan struct{})
	return b.running
}

// freeLocked frees the kernel and wakes whoever is waiting for it.
func (b *bench) freeLocked() {
	b.running = 0
	if b.idle != nil {
		close(b.idle)
		b.idle = nil
	}
}

// finishRun releases the kernel, if the run that is finishing is the one holding
// it.
//
// The check is not defensive: a Restart ends the running cell and brings a new
// kernel up while that cell's forwarding goroutine may still be draining into a
// browser that is not reading. When it does finish, the kernel it ran on is gone
// and the bench may already be running a later cell — and clearing the flag then
// would put two ODE executions on one kernel, which is what ErrBusy exists to
// prevent.
func (s *Service) finishRun(target *bench, run runToken) {
	target.mux.Lock()
	if target.running == run {
		target.freeLocked()
	}
	target.lastUsed = time.Now()
	target.mux.Unlock()
}

// InterruptUser stops the cell running in one workbench.
func (s *Service) InterruptUser(ctx context.Context, ref Ref) error {
	handle, err := s.handleFor(ref)
	if err != nil {
		return err
	}
	return s.Interrupt(ctx, handle)
}

// Restart ends one workbench's kernel and starts a fresh one, keeping the pod and
// therefore the workspace, and leaving every other workbench alone. This is the
// "my session is wedged" action.
func (s *Service) Restart(ctx context.Context, ref Ref) (Status, error) {
	user, err := s.UserFor(ref.Bearer)
	if err != nil {
		return Status{}, err
	}
	target := s.bench(user, ref.Workbench)

	target.mux.Lock()
	defer target.mux.Unlock()

	s.shutdownKernelLocked(ctx, target)
	if _, err := s.ensureLocked(ctx, target, ref); err != nil {
		return Status{}, err
	}
	return s.statusLocked(ctx, target)
}

// ShutdownUser ends one workbench's kernel and releases ODE's hold on it.
//
// The pod stays up, and so does every other workbench's kernel. Ending the last
// one stops the keep-alive, which is ODE letting go of the pod rather than
// stopping it: the cluster's idle culling applies again, which is §5.6's
// arrangement and the reason ODE never deletes a pod itself.
func (s *Service) ShutdownUser(ctx context.Context, ref Ref) error {
	user, err := s.UserFor(ref.Bearer)
	if err != nil {
		return err
	}
	target, found := s.lookupBench(user, ref.Workbench)
	if !found {
		return ErrNoKernel
	}

	target.mux.Lock()
	defer target.mux.Unlock()
	if target.kernelID == "" {
		return ErrNoKernel
	}
	err = s.shutdownKernelLocked(ctx, target)
	s.releaseHoldLocked(target)
	return err
}

// shutdownKernelLocked ends the kernel in the pod and forgets it here.
func (s *Service) shutdownKernelLocked(ctx context.Context, target *bench) error {
	if target.kernelID == "" {
		return nil
	}
	container := target.pod
	container.mux.Lock()
	handle := KernelHandle{
		User: container.user, ServerURL: ServerURL(container.serverURL),
		Token: container.token, KernelID: target.kernelID,
	}
	container.mux.Unlock()

	err := s.Shutdown(ctx, handle)
	if err != nil {
		slog.WarnContext(ctx, "shutting a kernel down failed",
			"user", container.user.Name, "workbench", target.workbench, "error", err)
	}
	s.dropKernelLocked(target)
	return err
}

// Files lists one directory of the developer's workspace.
//
// It reads through the server's contents API rather than through a kernel, so it
// needs the pod and not a bench: a developer with no kernel running can still see
// what is on their PVC.
func (s *Service) Files(ctx context.Context, ref Ref, path string) ([]FileEntry, error) {
	user, err := s.UserFor(ref.Bearer)
	if err != nil {
		return nil, err
	}
	container := s.pod(user)

	container.mux.Lock()
	defer container.mux.Unlock()

	server, err := s.ensureServerLocked(ctx, container)
	if err != nil {
		return nil, err
	}
	token, err := s.ensureTokenLocked(ctx, container)
	if err != nil {
		return nil, err
	}

	target := s.opts.WorkspacePath
	if trimmed := strings.Trim(strings.TrimSpace(path), "/"); trimmed != "" {
		clean, err := cleanWorkspacePath(trimmed)
		if err != nil {
			return nil, err
		}
		target = s.opts.WorkspacePath + "/" + clean
	}
	entries, err := s.serverAPIFor(server, token).listDirectory(ctx, target)
	if err == nil || !s.serverGoneLocked(ctx, container, err) {
		return entries, err
	}
	// The same stale route as in ensureLocked, reached without a kernel: the pane
	// that lists the workspace is often the first thing to notice the pod is gone.
	slog.InfoContext(ctx, "the remembered singleuser server is gone; spawning a new one",
		"user", container.user.Name, "error", err)
	s.dropServerLocked(container)
	if server, err = s.ensureServerLocked(ctx, container); err != nil {
		return nil, err
	}
	return s.serverAPIFor(server, token).listDirectory(ctx, target)
}

// RefreshPlatformToken pushes a renewed platform token into every live kernel of
// this developer.
//
// §5.6 item 4: spawn-time environment variables cannot be refreshed, so the
// current token is installed by executing into the kernel instead. Called
// whenever the connection ODE is serving has adopted a new token. Every kernel,
// because the token is the developer's rather than any one workbench's, and a
// workbench left holding an expired one would fail on its next platform call for
// reasons that have nothing to do with what the developer was doing in it.
//
// Not while a cell is running, and that exception is the point. The push is an
// execution like any other, and ipykernel runs one at a time — so it would queue
// behind a training run of unknown length while this call holds the bench for
// however long that takes. Everything else on that workbench goes through the
// same mutex: its status reads, its repository operations, the running cell's own
// finish, and the reaper. A refresh that arrives on the SPA's poll timer must not
// be able to stop all of them.
//
// Skipping loses nothing. The token is not dropped: pushedToken still holds the
// previous one, so the next claim — which is what any use of the kernel goes
// through — finds them different and installs the current token before the
// developer's code runs. The window where the kernel holds a token older than
// ODE's is a window in which it is executing something that was started with the
// older one anyway.
func (s *Service) RefreshPlatformToken(ctx context.Context, bearer string) error {
	user, err := s.UserFor(bearer)
	if err != nil {
		return err
	}

	var failure error
	for _, target := range s.benchesOf(user) {
		target.mux.Lock()
		switch {
		case target.conn == nil:
			// Nothing is running there; the next Ensure pushes the current token.
		case target.running != 0:
			slog.DebugContext(ctx,
				"a cell is running, so the refreshed platform token is left to the next execution",
				"user", user.Name, "workbench", target.workbench)
		default:
			if err := s.pushEnvironmentLocked(
				ctx, target, bearer, target.pushedToken != ""); err != nil && failure == nil {
				failure = err
			}
		}
		target.mux.Unlock()
	}
	return failure
}

// ---- The locked helpers the operations above are assembled from ----

// ensureLocked is the whole bring-up for one workbench, idempotent and cheap when
// everything is already there — and willing to do it twice.
//
// Everything below the server is addressed through the route ODE remembers, and
// that route is only good for as long as the pod behind it lives. Once the pod is
// gone — culled, or stopped from JupyterLab while ODE still held the session —
// /user/{name}/ falls back to the Hub itself, which answers an API POST with its
// own 403 page: the Hub applies XSRF protection to its handlers, and a token
// authenticated client sends no _xsrf. Reporting that is useless, so a failure
// the Hub confirms is a gone server is retried once from a fresh spawn.
func (s *Service) ensureLocked(
	ctx context.Context, target *bench, ref Ref,
) (KernelHandle, error) {
	handle, err := s.bringUpLocked(ctx, target, ref)
	if err == nil || !s.serverGone(ctx, target.pod, err) {
		return handle, err
	}
	slog.InfoContext(ctx, "the remembered singleuser server is gone; spawning a new one",
		"user", target.pod.user.Name, "error", err)
	s.dropServer(target.pod)
	return s.bringUpLocked(ctx, target, ref)
}

// bringUpLocked is one attempt at the bring-up, in the order the pieces depend on
// each other.
func (s *Service) bringUpLocked(
	ctx context.Context, target *bench, ref Ref,
) (KernelHandle, error) {
	container := target.pod

	container.mux.Lock()
	server, err := s.ensureServerLocked(ctx, container)
	if err != nil {
		container.mux.Unlock()
		return KernelHandle{}, err
	}
	token, err := s.ensureTokenLocked(ctx, container)
	if err != nil {
		container.mux.Unlock()
		return KernelHandle{}, err
	}
	generation := container.generation
	container.mux.Unlock()

	// A pod that has been replaced since this kernel started took the kernel and the
	// socket with it. Noticing here, on this bench's own next use and under its own
	// lock, is what lets one workbench discover a respawn without any other having
	// to reach into its state.
	if target.generation != generation {
		s.dropKernelLocked(target)
		target.workspaceReady = false
		target.generation = generation
	}

	if err := s.ensureKernelLocked(ctx, target, ref, server, token); err != nil {
		return KernelHandle{}, err
	}
	if err := s.ensureConnectionLocked(ctx, target, server, token); err != nil {
		return KernelHandle{}, err
	}
	if err := s.pushEnvironmentLocked(ctx, target, ref.Bearer, ref.WithPlatformToken); err != nil {
		return KernelHandle{}, err
	}
	s.holdLocked(target)
	target.lastUsed = time.Now()

	return KernelHandle{
		User:      container.user,
		ServerURL: ServerURL(server),
		Token:     token,
		KernelID:  target.kernelID,
		Name:      s.opts.KernelName,
	}, nil
}

// serverGone asks the Hub whether a failed step failed because the pod ODE was
// addressing is no longer there.
//
// The Hub is asked rather than the response inspected, because the failure looks
// different at every step — a 404 on the remembered kernel, a 403 page on the
// next create, a refused WebSocket — while the state behind all of them is the
// one thing worth knowing. Only a remembered route qualifies: a bring-up that
// spawned in this very call has already had the Hub's answer, and asking again
// would turn one genuine failure into two.
func (s *Service) serverGone(ctx context.Context, container *pod, err error) bool {
	container.mux.Lock()
	remembered := container.serverURL
	container.mux.Unlock()
	return s.serverGoneWith(ctx, container, remembered, err)
}

// serverGoneLocked is the same question from a caller that already holds the pod.
func (s *Service) serverGoneLocked(ctx context.Context, container *pod, err error) bool {
	return s.serverGoneWith(ctx, container, container.serverURL, err)
}

func (s *Service) serverGoneWith(
	ctx context.Context, container *pod, remembered string, err error,
) bool {
	if remembered == "" || ctx.Err() != nil {
		return false
	}
	// Refusals of ODE's own making, which a new pod would refuse just as well.
	if errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrBusy) ||
		errors.Is(err, ErrSpawnTimeout) {
		return false
	}
	state, stateErr := s.hub.ServerState(ctx, container.user.Name)
	if stateErr != nil {
		slog.WarnContext(ctx, "could not ask the hub whether the server is still up",
			"user", container.user.Name, "error", stateErr)
		return false
	}
	return !state.Ready
}

// dropServer forgets the pod without ending any developer's session.
//
// The kernels and the sockets go with it; they lived in that pod. The minted token
// does not, because it is scoped to the user rather than to one server, so the pod
// that replaces this one accepts it. Every bench is invalidated by the generation
// bump rather than by being reached into here, which is what keeps the lock order
// one-way — each one drops its dead kernel on its own next use.
func (s *Service) dropServer(container *pod) {
	container.mux.Lock()
	defer container.mux.Unlock()
	s.dropServerLocked(container)
}

func (s *Service) dropServerLocked(container *pod) {
	container.serverURL = ""
	container.generation++
}

func (s *Service) ensureServerLocked(ctx context.Context, container *pod) (string, error) {
	if container.serverURL != "" {
		return container.serverURL, nil
	}
	server, err := s.EnsureServer(ctx, container.user)
	if err != nil {
		return "", err
	}
	container.serverURL = string(server)
	return container.serverURL, nil
}

func (s *Service) ensureTokenLocked(ctx context.Context, container *pod) (HubToken, error) {
	if container.token != "" && time.Until(container.tokenExpiry) > tokenRenewBefore {
		return container.token, nil
	}
	token, expiry, err := s.hub.MintToken(ctx, container.user.Name, s.opts.TokenTTL)
	if err != nil {
		return "", err
	}
	container.token, container.tokenExpiry = token, expiry
	// The open sockets are left alone. They were authorised at connect time and
	// stay valid; the renewed token is what the next reconnect will use.
	return container.token, nil
}

// ensureKernelLocked makes sure this workbench has a kernel, in the directory the
// workbench actually points at.
func (s *Service) ensureKernelLocked(
	ctx context.Context, target *bench, ref Ref, server string, token HubToken,
) error {
	api := s.serverAPIFor(server, token)
	user := target.pod.user
	dir := s.checkoutFor(ctx, user, ref.Workbench)

	if target.kernelID != "" {
		if _, err := api.getKernel(ctx, target.kernelID); err != nil {
			// The kernel ODE remembers is gone — the pod was culled and respawned, or
			// someone shut it down in JupyterLab. Starting a new one is the useful
			// answer; the workspace is what carried anything worth keeping.
			slog.InfoContext(ctx, "the remembered kernel is gone; starting a new one",
				"user", user.Name, "workbench", target.workbench, "kernel", target.kernelID)
			s.dropKernelLocked(target)
		} else if target.dir == dir {
			return nil
		} else {
			// The workbench points somewhere else than when this kernel started, which
			// happens when a developer selects a different repository into it. Left
			// alone, the next `open("notes.txt")` in a cell would write into the
			// previous operator's checkout.
			slog.InfoContext(ctx, "this workbench's checkout moved; restarting its kernel",
				"user", user.Name, "workbench", target.workbench,
				"from", directoryOrRoot(target.dir), "to", directoryOrRoot(dir))
			if err := s.Shutdown(ctx, KernelHandle{
				User: user, ServerURL: ServerURL(server), Token: token, KernelID: target.kernelID,
			}); err != nil {
				slog.WarnContext(ctx, "shutting down a relocated kernel failed",
					"user", user.Name, "error", err)
			}
			s.dropKernelLocked(target)
		}
	}

	path := s.opts.WorkspacePath
	if dir != "" {
		path += "/" + dir
	}
	if !target.workspaceReady || target.dir != dir {
		// The whole path, not only the workspace root: a workbench whose repository
		// has not been cloned yet still needs somewhere for its kernel to start, and
		// git clones happily into an existing empty directory.
		if err := api.ensureDirectory(ctx, path); err != nil {
			return err
		}
		target.workspaceReady = true
	}

	created, err := api.createKernel(ctx, s.opts.KernelName, path)
	if err != nil {
		return err
	}
	target.kernelID = created.ID
	target.dir = dir
	slog.InfoContext(ctx, "started a kernel",
		"user", user.Name, "workbench", target.workbench,
		"kernel", created.ID, "directory", path)
	return nil
}

func (s *Service) ensureConnectionLocked(
	ctx context.Context, target *bench, server string, token HubToken,
) error {
	if target.conn != nil && !target.conn.isClosed() {
		return nil
	}
	if target.conn != nil {
		logConnectionClose(target.pod.user.Name, target.conn.err())
		target.conn = nil
		// A dropped socket says nothing about the kernel, but the pushed token was
		// installed in a kernel that may itself be gone, so it is re-pushed.
		target.pushedToken = ""
		target.environmentReady = false
	}

	endpoint := channelsEndpoint(server, target.kernelID)
	conn, err := dial(ctx, s.opts.Dialer, endpoint, token, target.pod.user.Name)
	if err != nil {
		return err
	}
	target.conn = conn
	return nil
}

// pushEnvironmentLocked installs the developer's platform token and ODE's
// configuration in the kernel (§5.6 item 4).
//
// The values are base64-encoded rather than interpolated into the source. A JWT
// happens to be safe inside a Python string literal and a URL from configuration
// need not be, and the difference is not something a reader of this code should
// have to reason about. The execution is silent, so it leaves no history and
// nothing reaches the developer's console.
// Under Options.ContainCells the token is installed only for an execution that
// asked for it, and removed again before one that did not. Both directions happen
// here, under the bench lock, in the bring-up every execution goes through — which
// is what makes the removal worth anything: a cell that did not ask for the token
// cannot observe the window in which the previous one had it, because closing that
// window is a step on its own path to the kernel.
func (s *Service) pushEnvironmentLocked(
	ctx context.Context, target *bench, bearer string, withToken bool,
) error {
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(bearer, "Bearer "), "bearer "))
	if token == "" {
		return fmt.Errorf("%w: no platform token to install in the kernel", ErrInvalidRequest)
	}
	// Without containment every execution carries the token, which is the behaviour
	// every deployment had before the option existed.
	wanted := token
	if s.opts.ContainCells && !withToken {
		wanted = ""
	}
	if target.pushedToken == wanted && target.environmentReady {
		return nil
	}
	if target.conn == nil {
		return ErrNoKernel
	}

	environment := map[string]string{WorkspaceEnv: s.opts.WorkspacePath}
	for name, value := range s.opts.Environment {
		environment[name] = value
	}
	if wanted != "" {
		environment[PlatformTokenEnv] = wanted
	}

	// The removal is unconditional rather than conditional on pushedToken. A kernel
	// ODE did not start, one the developer pushed a token into from JupyterLab, and
	// one whose bench record was rebuilt all look the same from here, and in each of
	// them a `del` that finds nothing is free.
	code := environmentCode(environment)
	if wanted == "" {
		code += fmt.Sprintf("_ = _os_env.pop(%q, None)\n", PlatformTokenEnv)
	}

	events, err := target.conn.execute(ctx, environmentPrelude+code+environmentEpilogue,
		executeOptions{Silent: true, MaxOutputBytes: 4096})
	if err != nil {
		return err
	}
	for event := range events {
		if event.Kind == KindDone && event.Status != StatusOK {
			// Deliberately does not name which direction failed. A kernel whose
			// environment is not what ODE believes it to be must not be run in either
			// way, and environmentReady staying false is what stops the next claim
			// from skipping this.
			target.environmentReady = false
			return fmt.Errorf("kernel: installing the environment failed: %s %s",
				event.Status, event.Error)
		}
	}
	target.pushedToken = wanted
	target.environmentReady = true
	return nil
}

const (
	environmentPrelude  = "import base64 as _b64\nfrom os import environ as _os_env\n"
	environmentEpilogue = "del _b64, _os_env\n"
)

// environmentCode renders the hidden cell that installs the environment.
func environmentCode(environment map[string]string) string {
	var builder strings.Builder
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	// Sorted so the cell is the same text for the same environment. Map order would
	// make pushedToken's short-circuit the only thing standing between a reconnect
	// and a different hidden cell every time.
	sort.Strings(names)
	for _, name := range names {
		builder.WriteString(fmt.Sprintf("_os_env[%q] = _b64.b64decode(%q).decode(\"utf-8\")\n",
			name, base64.StdEncoding.EncodeToString([]byte(environment[name]))))
	}
	return builder.String()
}

// holdLocked counts this bench as keeping the pod up, and starts the keep-alive if
// it is the first one to do so.
//
// Called with the bench locked, which is the only place pod.live grows. The
// pairing with releaseHoldLocked is what the whole arrangement rests on: a count
// that drifts up leaves a pod alive after the developer has gone home, and one
// that drifts down stops the keep-alive under a workbench that is still training.
func (s *Service) holdLocked(target *bench) {
	if target.held {
		return
	}
	target.held = true

	container := target.pod
	container.mux.Lock()
	defer container.mux.Unlock()
	container.live++
	if container.live == 1 {
		s.startKeepaliveLocked(container)
	}
}

// releaseHoldLocked gives up this bench's share of the pod, stopping the
// keep-alive when it was the last one.
func (s *Service) releaseHoldLocked(target *bench) {
	if !target.held {
		return
	}
	target.held = false

	container := target.pod
	container.mux.Lock()
	defer container.mux.Unlock()
	if container.live > 0 {
		container.live--
	}
	if container.live == 0 {
		s.stopKeepaliveLocked(container)
	}
}

// startKeepaliveLocked reports activity for as long as ODE holds this pod.
//
// The idle culler kills a server whose last activity is older than its timeout,
// and it counts activity rather than liveness: a developer thinking for twenty
// minutes between cells looks exactly like an abandoned pod. One loop per pod, not
// per kernel — the Hub's activity is a property of the server, and reporting it
// twice as often because someone opened a second workbench would say nothing new.
func (s *Service) startKeepaliveLocked(container *pod) {
	if container.keepalive != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.root)
	container.keepalive = cancel

	user := container.user.Name
	go func() {
		ticker := time.NewTicker(s.opts.KeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reportCtx, reportCancel := context.WithTimeout(ctx, s.opts.RequestTimeout)
				err := s.hub.ReportActivity(reportCtx, user, time.Now())
				reportCancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("reporting kernel activity to the hub failed", "user", user, "error", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Service) stopKeepaliveLocked(container *pod) {
	if container.keepalive != nil {
		container.keepalive()
		container.keepalive = nil
	}
}

func (s *Service) dropKernelLocked(target *bench) {
	if target.conn != nil {
		target.conn.close(nil)
		target.conn = nil
	}
	target.kernelID = ""
	target.pushedToken = ""
	target.environmentReady = false
	// The kernel is gone, so no execution holds it any more. The token of the run
	// that did is not reused, so its own finish will find nothing to release.
	target.freeLocked()
}

func (s *Service) statusLocked(ctx context.Context, target *bench) (Status, error) {
	container := target.pod
	state, err := s.hub.ServerState(ctx, container.user.Name)
	if err != nil {
		return Status{}, err
	}
	status := Status{
		User:           container.user.Name,
		ServerReady:    state.Ready,
		ServerPending:  state.Pending,
		ServerURL:      state.URL,
		Started:        state.Started,
		LastActivity:   state.LastActivity,
		Workbench:      target.workbench,
		KernelID:       target.kernelID,
		KernelCount:    s.kernelCount(container.user, target),
		Busy:           target.running != 0,
		Profile:        s.opts.Profile,
		Workspace:      s.opts.WorkspacePath,
		Directory:      target.dir,
		WorkspaceReady: target.workspaceReady,
	}
	if target.kernelID != "" {
		status.KernelName = s.opts.KernelName
	}
	return status, nil
}

// kernelCount is how many kernels ODE is holding in this developer's pod.
//
// Counted from the map rather than from pod.live, because live counts holds and
// this is meant to answer "how many Python processes did ODE start in there" —
// which is what a developer looking at a pod's memory wants to know.
//
// held is the bench whose lock the caller already has, and it is excluded rather
// than locked again: statusLocked reports the count for the very bench it is
// describing, and a mutex that is not reentrant would deadlock on it. Its own
// kernel is counted from the state the caller can already see.
func (s *Service) kernelCount(user User, held *bench) int {
	count := 0
	if held != nil && held.kernelID != "" {
		count++
	}
	for _, target := range s.benchesOf(user) {
		if target == held {
			continue
		}
		target.mux.Lock()
		if target.kernelID != "" {
			count++
		}
		target.mux.Unlock()
	}
	return count
}

// handleFor rebuilds a handle for a kernel that is already up.
func (s *Service) handleFor(ref Ref) (KernelHandle, error) {
	user, err := s.UserFor(ref.Bearer)
	if err != nil {
		return KernelHandle{}, err
	}
	target, found := s.lookupBench(user, ref.Workbench)
	if !found {
		return KernelHandle{}, ErrNoKernel
	}

	target.mux.Lock()
	defer target.mux.Unlock()
	if target.kernelID == "" {
		return KernelHandle{}, ErrNoKernel
	}

	container := target.pod
	container.mux.Lock()
	defer container.mux.Unlock()
	return KernelHandle{
		User: container.user, ServerURL: ServerURL(container.serverURL),
		Token: container.token, KernelID: target.kernelID, Name: s.opts.KernelName,
	}, nil
}

// reap releases benches nobody has used, which is what stops ODE keeping a pod
// alive past the developer's interest in it.
//
// Per bench rather than per developer: one workbench going quiet while another
// trains is the ordinary case now, and letting go of the quiet one costs nothing
// as long as the pod stays held by the busy one — which is exactly what the hold
// count does.
func (s *Service) reap() {
	cutoff := time.Now().Add(-s.opts.IdleTimeout)

	s.mux.Lock()
	candidates := make(map[string]*bench, len(s.benches))
	for key, target := range s.benches {
		candidates[key] = target
	}
	s.mux.Unlock()

	for key, target := range candidates {
		target.mux.Lock()
		if target.running != 0 || target.lastUsed.After(cutoff) {
			target.mux.Unlock()
			continue
		}
		name := target.pod.user.Name
		workbench := target.workbench
		s.releaseLocked(target)
		target.mux.Unlock()

		s.mux.Lock()
		delete(s.benches, key)
		s.mux.Unlock()
		slog.Info("released an idle kernel; the pod is now the cluster's to cull",
			"user", name, "workbench", workbench)
	}
}

func (s *Service) releaseAll() {
	s.mux.Lock()
	benches := s.benches
	s.benches = map[string]*bench{}
	s.pods = map[string]*pod{}
	s.mux.Unlock()

	for _, target := range benches {
		target.mux.Lock()
		s.releaseLocked(target)
		target.mux.Unlock()
	}
}

// releaseLocked drops ODE's hold without touching the pod: the kernel keeps
// running, the files stay, and the cluster's idle culling applies again.
func (s *Service) releaseLocked(target *bench) {
	if target.conn != nil {
		target.conn.close(nil)
		target.conn = nil
	}
	target.pushedToken = ""
	target.environmentReady = false
	s.releaseHoldLocked(target)
}

// directoryOrRoot names a workspace-relative directory for a log line.
func directoryOrRoot(dir string) string {
	if dir == "" {
		return "the workspace root"
	}
	return dir
}
