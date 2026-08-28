# In flight: workbenches — several operators developed at once

**Status**: implemented. Written 2026-08-27, checked against the code 2026-08-28.
Landed in `59f7479`. One documentation item and two deployment prerequisites are
still open — see *What is left* at the end. Everything above that line is done and
is kept here only until those two are closed and the handover below is complete.

The problem it solved: a developer had exactly one repository checkout and one
kernel. The link was keyed by `user_sub` alone (`ode_repo_links`, `pkg/repo/store.go`)
and the kernel session by the Hub username, so two chat sessions shared one working
copy — selecting a repository in one changed where the other's `write_file` landed.
The working context is now plural.

## Goal

A developer holds several **workbenches** at once. One workbench is one repository
checkout plus one kernel plus its own busy state. Creating a chat session either
joins an existing workbench or opens a new one.

## Decisions taken, with the alternatives that were rejected

### One pod, several kernels — not a named server per workbench

`serverAPI.createKernel` already starts kernels through `POST /api/kernels` with a
`path`, and `jupyter_server` derives each kernel's cwd from it. A singleuser server
runs any number of kernels. So N workbenches are N kernel processes in the pod the
developer already has: same spawn, same per-user token, same activity keep-alive,
and `kernel.RequiredScopes` is unchanged.

**No JupyterHub configuration change is required.** Two deployment-side
prerequisites are, neither of them ODE configuration:

1. The ODE KubeSpawner profile's `mem_limit`/`mem_guarantee` has to carry more than
   one Python process with a loaded dataframe. Sized for one kernel, a second one
   OOM-kills the pod.
2. `MappingKernelManager.cull_idle_timeout` in the singleuser server has to be off
   or long. ODE's keep-alive addresses the Hub's culler, not this one, so a
   server-side kernel culler would silently kill whichever workbench the developer
   is not currently typing in.

*Rejected*: a pod per workbench through `c.JupyterHub.allow_named_servers`. It buys
real resource isolation and costs Hub configuration, a rewrite of every
`/users/{name}/server` path in `pkg/kernel/hub.go`, a cold start per workbench, and
it does not work at all where the per-user PVC is `ReadWriteOnce` — two pods cannot
both mount the volume the checkouts live on.

### The name is "workbench"

*Rejected*: `workspace`, which is already the PVC root in `kernel.Options` and
`kernel.Status`; and `code session`, which would be the fourth meaning of "session"
here, one of them in the same package.

### A kernel's cwd is its workbench's checkout

`open("notes.txt")` from a cell lands next to `op.py`. Behaviour change: today it
lands in the workspace root. Files written from a cell before this change stay
where they are; new ones move.

*Rejected*: keeping the workspace root, which would leave both kernels sharing a
cwd — a cell writing a relative path could not say which operator it belonged to.

### One repository per workbench, and per developer

A repository is in at most one of a developer's workbenches. The checkout path is
`{owner}/{name}` (`pkg/repo/service.go:285`), so two workbenches on one repository
would put two kernels and two git command streams in one working tree: `index.lock`
corruption and a lost diff. Selecting an already-linked repository answers 409 and
names the workbench holding it.

### The schema change is additive, and adoption is lazy

`database.Migrate` is idempotent DDL with no version table, and says in its own
comment that a destructive change has no home in it. So `ode_repo_links` is left in
place and read once: a developer with a link row and no workbench has it adopted
into a freshly minted workbench on first load. The table can be dropped in a later
release.

*Rejected*: repurposing `ode_repo_links` by changing its primary key, and a
SQL-side backfill — the latter would mint ids outside `pkg/identifiers`, and ids
that appear in URLs must not be guessable.

## The work, as six commits — all landed

Checked by reading the code rather than by trusting the plan. Where a claim is
marked with a file, that is where it is.

1. **Schema and repo store — done.** `ode_workbenches` and the additive
   `ode_chat_sessions.workbench_id` are in `pkg/database/database.go`; adoption of
   the old link row is `repo.Service.adopt` (`pkg/repo/workbench.go:160`), called
   from the list path at `:125` and taking
   the unique index as the arbiter when two requests race.
   *Planned as:* `ode_workbenches` (id, user_sub, title, plus the link
   columns). `ode_chat_sessions.workbench_id`, nullable. `repo.Store` keys on
   workbench id with an ownership check; `repo.Request` gains `WorkbenchID`. Lazy
   adoption of the old link row. The GitHub identity stays per user.
2. **Kernel: per-pod and per-kernel state split — done.** `pod` and `bench` are
   `pkg/kernel/session.go:121` and `:148`; `RefreshPlatformToken` reaches every live
   bench (`session.go:663`).
   *Planned as:* `userSession` becomes
   `pod` (serverURL, token and expiry, keep-alive, live-bench count) and `bench`
   (kernelID, conn, pushedToken, running/idle/runs, lastUsed). `claim` then contends
   per bench, which is the point: a training cell in one workbench no longer makes
   another's file read answer 409. The keep-alive starts with the first bench and
   stops with the last. `reap` releases benches; the last release releases the pod.
   `RefreshPlatformToken` pushes into every live bench. Each kernel is created with
   `path` = its checkout. The five callers outside the package (`pkg/repo/git.go`
   x2, `pkg/experiments/archive.go` x2, `pkg/experiments/criteria.go`) take a
   `{Bearer, WorkbenchID}` ref instead of a bare bearer.
3. **API — done.** The `/workbenches` group is `pkg/api/api.go:318`; the
   `workbench` query parameter is read in `repo.go`, `kernel.go` and
   `experiments.go`. A foreign id answers 404 (`ErrNoWorkbench`,
   `pkg/repo/workbench.go:48`).
   *Planned as:* A `/workbenches` group (list, create, rename, delete) plus a `workbench`
   query parameter on the existing `/repo/*` and `/kernel/*` routes. Omitted means
   the developer's only workbench, which keeps the existing tests, the committed
   contract fixtures and the swagger surface valid. A foreign workbench id is a 404,
   as a foreign chat session id already is.
4. **Chat, tools, experiments — done.** `chat.Session.WorkbenchID`
   (`pkg/chat/session.go:69`), `tools.Request.WorkbenchID`
   (`pkg/tools/registry.go:131`), `admin.Limits.MaxWorkbenches`
   (`pkg/admin/limits.go:92`) with the default of three in
   `configuration.RepoMaxWorkbenches`.
   *Planned as:* `chat.Session.WorkbenchID`, validated on create.
   `tools.Request` gains it beside the `SessionID` it already carries, so
   `write_file` and `run_code` route to the session's own workbench. Experiments
   launch from that workbench's commit. `admin.Limits.MaxWorkbenches`, default 3,
   beside `MaxConcurrentSessions`.
5. **Frontend — done.** One repository queue per workbench, `frontend/src/api.ts:115`.
   *Planned as:* `?workbench=` beside `?session=`. The repo queue in `api.ts`
   becomes one queue per workbench — otherwise one workbench's file read waits
   behind another's clone and most of the benefit is gone. Code and Kernel panes
   scope to the current workbench; opening a chat session switches them to its
   workbench. `NewSession` gains the workbench select plus "new workbench", where
   new is a `POST /workbenches` followed by the session create.
6. **Docs — mostly done.** `docs/kernel-and-repository.md` is rewritten and carries
   the two deployment prerequisites; decision **D32** is in `docs/decisions.md`; the
   OpenAPI document is regenerated and `go generate ./...` leaves it unchanged.
   `docs/component-design.md` is the one that was missed — see below.
   *Planned as:* `docs/kernel-and-repository.md` goes stale hardest: its
   one-cell-at-a-time consequence and its "a second ODE-owned kernel has not been
   built" note both stop being true. Also `docs/component-design.md`,
   `docs/decisions.md`, and regenerated swagger.

## Assumptions

- Workbench ids come from `pkg/identifiers`, like session ids.
- Deleting a workbench unlinks and leaves the checkout on the PVC, as switching
  repositories already does.
- Existing chat sessions adopt the adopted workbench, so none loses its context.

## Not doing

Exposure tiers, the profiler and the interpretation poller are untouched.
Workbenches are not shared between developers. `ode_repo_links` is not dropped.

## Risk

Two, and both get a test rather than a note: the keep-alive refcount, where an
off-by-one culls a pod mid-run; and pod memory, which is the deployment
prerequisite above. Everything else is additive and reverts by ignoring the new
column.

## Verification — run 2026-08-28

`go build ./...`, `go test ./...` and `go vet ./...` all pass; `go generate ./...`
leaves `docs/` unchanged, which is the drift check CI applies. The frontend suite is
214 tests, with `tsc --noEmit` and `vite build` clean. This repository has no
`.claude/gates.env`, so `/gates:run` does not apply to it.

Every test the plan asked for exists:

- concurrent benches without `ErrBusy` — `pkg/kernel/workspace_test.go:410`
- the keep-alive outliving one bench and stopping with the last —
  `pkg/kernel/workbench_test.go:182`
- a refreshed token reaching every workbench — `workbench_test.go:284`
- a second workbench on the same repository refused — `pkg/repo/workbench_test.go:100`
  (`ErrRepositoryInUse`, which names the workbench holding it)
- a foreign workbench id answering 404 — `pkg/api/workbench_test.go:97`
- one queue per workbench — `frontend/src/api.workspace.test.ts`

Not verified by running the application: nothing here was exercised against a live
JupyterHub, so the two prerequisites below are unproven in a real deployment.

## What is left

**One documentation item.** `docs/component-design.md` was named in commit 6 and not
touched. It is not wrong — it makes no claim that plurality contradicts — but §5.6
still describes the kernel surface as though a developer had one, and its
idle-culling item talks about the Hub's culler without distinguishing the
server-side `MappingKernelManager.cull_idle_timeout`, which is a different culler
and the one that can now kill the workbench nobody is typing in.

**Two deployment prerequisites, neither of them ODE configuration.** Both are stated
in `docs/kernel-and-repository.md` and neither is code, so nothing in this repository
can confirm them:

1. the KubeSpawner profile's `mem_limit`/`mem_guarantee` must carry more than one
   Python process with a loaded dataframe;
2. `MappingKernelManager.cull_idle_timeout` in the singleuser server must be off or
   long.

Until the first is applied, a second workbench OOM-kills the pod — which is a
production failure, not a degradation, and it is the reason this section outlives
the code.

## When this is done

The decisions above went to `docs/decisions.md` (D32) and
`docs/kernel-and-repository.md`, which is the condition this file set for its own
removal — the rejected alternatives are most of what it is worth, and they are now
somewhere permanent. So once `component-design.md` is caught up and the two
prerequisites are confirmed in the deployment, delete this file rather than leaving
an implemented plan to read as pending work.
