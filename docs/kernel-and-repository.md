# The kernel and the repository working copy

Everything a developer writes lives on their own JupyterHub pod, not on ODE.
That is why git runs in the pod, why a file read can answer 409 during a training
run in the same workbench, and why ODE starts pods and never stops them.

## Applies when

Working on `pkg/kernel` or `pkg/repo`, or diagnosing why a file a developer
wrote is gone after a cull, or why a request was refused for naming no workbench.

**Not this if**: the question is what the *deployed* Hub does differently from
what §5.6 assumes. The username claim and the PVC mount point are properties of
that Hub rather than of ODE and are documented on the platform side;
`jupyterhub_username_claim` and `jupyterhub_workspace_path` are the two settings
that exist because of them, and are all ODE needs to know. Nor this if the
question is why running the operator's code in a cell is not an experiment — the
environment a kernel does *not* get, and the three loops that follow from it, are
in [experiments.md](experiments.md).

`geltung`: `allgemein` for the one-cell-at-a-time consequence and the pod
lifecycle; the rest follows from §5.6 and §5.11.

### A workbench is one checkout and one kernel, and a developer has several

The unit everything below hangs off is the **workbench**: one repository checkout
on the PVC, and one kernel running in it. It exists because a developer works on
more than one operator. Before it, the selected repository was keyed by the
developer alone, so two chat sessions shared one working copy — selecting a
repository in one changed where the other's `write_file` landed, and a training run
in either made a file read in both answer 409.

Two rules hold it together, and both are enforced rather than assumed. A workbench
holds at most one repository, and a repository is open in at most one of a
developer's workbenches — the checkout is at `{owner}/{name}` under the workspace,
so a second workbench on the same repository would put two kernels and two streams
of git commands in one working tree, which is a corrupted index and a lost diff.
The unique index on `ode_workbenches (user_sub, full_name)` is what makes that true
even against a second ODE talking to the same database.

A developer who selected a repository before workbenches existed has a row in
`ode_repo_links` and no workbench. That row is adopted into their first workbench
lazily, on the next request that reads the list, so the checkout already on their
PVC stays the one they are working in. Two things make the adoption survive
reality. It is stamped `adopted_at` once it has happened, because the trigger is an
empty workbench list and closing the last workbench would otherwise hand the old
link straight back as a new one. And a request that loses the race to adopt — one
page load sends several that name no workbench, they all find the list empty, and
the unique index lets exactly one write — reads the list again instead of reporting
the conflict. Reporting it is what produced a 409 saying a repository was open in
another workbench, naming the workbench the developer's own page had made a
millisecond earlier.

A request that names no workbench means *the developer's only one*. That is what
keeps every client written before workbenches existed correct, and it is refused
once there are two rather than guessed: choosing between two working copies on the
developer's behalf is the failure the whole arrangement exists to prevent.

A chat session names the workbench it acts in, and that naming can be changed after
the fact — the conversation is often what tells the developer which operator it is
really about. `PUT /chat/sessions/{id}/workbench` does it, checking the workbench
against the caller before the session is written, and leaving a note in the
conversation because the file and cell results above it describe the checkout it has
left. `docs/chat-and-streaming.md` has the rest, including why the move is refused
while a turn is running.

### The repository working copy is on the developer's PVC, so git runs in their pod

ODE's backend has no filesystem a checkout could live on that survives a
pod restart, and §5.6 puts state that must survive on the volume — so every git
command and every file edit in `pkg/repo` is a `kernel.Command` executed in the
developer's own singleuser pod, under their own GitHub credential, passed in the
environment of one command rather than written into `.git/config`. Two consequences
follow that are easy to miss. The file operations deliberately do *not* use
jupyter_server's contents API, because `allow_hidden` is false by default and D14
requires `.github/workflows/build.yml` to be as editable as any other file. And a
kernel runs one cell at a time, so a file read during a long training run answers
409 once its bounded wait for an idle kernel runs out — bounded by the *workbench*
now rather than by the developer, because every other workbench has its own kernel.

### One pod, several kernels — and no JupyterHub configuration for it

A singleuser server runs any number of kernels, each with its own working
directory: `POST /api/kernels` takes a `path` and `jupyter_server` derives the
cwd from it. So N workbenches are N kernel processes in the pod the developer
already has. Nothing about the Hub changes — the same spawn, the same per-user
token, the same activity keep-alive, and `kernel.RequiredScopes` is untouched.

What that costs is on the deployment side rather than in ODE's configuration, and
it is worth stating because both failures are silent:

  - **Pod memory.** Each kernel is a full Python process. The ODE KubeSpawner
    profile has to carry as many as `repo_max_workbenches` allows, or a developer's
    second workbench OOM-kills their first one's training run.
  - **The singleuser server's own kernel culler.**
    `MappingKernelManager.cull_idle_timeout` has to be off or long. ODE's keep-alive
    addresses the *Hub's* culler and says nothing to this one, which would kill
    whichever workbench the developer is not currently typing in.

*Rejected*: a pod per workbench through `c.JupyterHub.allow_named_servers`. It buys
real resource isolation, and costs Hub configuration, a rewrite of every
`/users/{name}/server` path in `hub.go`, a cold start per workbench — and it does
not work at all where the per-user PVC is `ReadWriteOnce`, because two pods cannot
both mount the volume the checkouts live on.

### A kernel's working directory is its workbench's checkout

`open("notes.txt")` from a cell lands next to `op.py`, because each kernel is created
with a `path` of its own checkout and `jupyter_server` derives the cwd from it.

This changed behaviour once, and the change is worth knowing when reading an old
conversation: before workbenches, a cell's relative path landed in the workspace
root. Files written there before the change are still there; new ones are not.

*Rejected*: keeping the workspace root for every kernel. It would leave two kernels
sharing one cwd, and a cell writing a relative path could not say which operator it
belonged to — the first thing a second workbench would have broken.

### Why the name is "workbench"

*Rejected*: `workspace`, which is already the PVC root in `kernel.Options` and
`kernel.Status`, so the same word would have meant two nested things one type apart;
and `code session`, which would have been the fourth meaning of "session" in this
codebase, two of them in the same package.

### The schema change was additive, and adoption is lazy

`database.Migrate` is idempotent DDL with no version table, and says in its own
comment that a destructive change has no home in it. So `ode_repo_links` is left
where it is and read once, per the adoption above. The table can be dropped in a
later release; nothing reads it after a developer's first load.

*Rejected*: repurposing `ode_repo_links` by changing its primary key, which is the
destructive shape that migration style cannot carry. *Also rejected*: a SQL-side
backfill to mint the first workbench, which would have created ids outside
`pkg/identifiers` — and these ids appear in URLs, so they must not be guessable.

### The pod state and the kernel state are split, and the lock order is one-way

`pkg/kernel` holds a `pod` per developer and a `bench` per workbench. Anything the
Hub knows about — server URL, minted token, the keep-alive — is on the pod and
shared, so a second workbench costs no second spawn and no second keep-alive loop.
Anything a kernel has — its id, its socket, the pushed platform token, the
one-cell-at-a-time hold — is on the bench.

A bench's mutex may be held while taking its pod's, and never the other way round.
Everything that walks the benches of a pod snapshots under the service mutex and
lets it go before touching one. The two places this bites are worth knowing: a pod
that turns out to be gone bumps a **generation** counter rather than reaching into
its sibling benches, so each one drops its dead kernel on its own next use under
its own lock; and `kernelCount` is passed the bench its caller already holds, so
reporting a status does not deadlock on the bench being reported.

The keep-alive is refcounted — it starts with the first bench to hold the pod and
stops with the last to let go. An off-by-one in either direction is a pod culled
with a training run in it, or one held alive after the developer went home, which
is why the count only ever moves through `holdLocked` and `releaseHoldLocked` and
has its own test.

### ODE spawns pods and never stops them

Shutting down a kernel leaves the pod
running, and the reaper that drops an idle session closes ODE's socket and stops
its keep-alives rather than deleting anything. Both are the same judgement: the
pod is the developer's, their files and their running processes are on it, and a
respawn costs them a cold start. Reclaiming it is the cluster's idle culling —
which ODE stops holding off precisely so that it can.

### The kernel connection is persistent per workbench, and that is not only an optimisation

`jupyter_server` bridges the kernel's ZeroMQ sockets onto the
WebSocket when the connection opens, and a request sent before that bridge exists
loses its early `iopub` messages — the busy status, sometimes the first lines of
output. Paying a `kernel_info` handshake once on connect closes the race for
every cell after it. Reconnecting per cell would reopen it every time, which is
the kind of bug that reproduces on a loaded cluster and nowhere else.

## Every file, including the ones a notebook server hides

The Code pane's tree is the whole working copy and every file in it is writable,
which is D14 taken literally. `.github/workflows/build.yml` is the case that makes
the point: it is where the registry lives (§5.11 item 4 — ODE does not hold it as
configuration), so changing where the image goes means editing that file, and a
pane that could not open it would make the decision unchangeable.

It is also why the file operations run **through the kernel** rather than over
jupyter_server's contents API. That API can read and write files, but
`ContentsManager.allow_hidden` is false by default, so it refuses exactly the paths
a compliant operator repository needs. Running a small Python helper in the pod has
no opinion about dotfiles, and it means one mechanism serves both the files and
git.

The cost, stated rather than hidden: a kernel runs one cell at a time, so a file
read while a training cell is running in **the same workbench** answers **409**
with `needs: idle_kernel` rather than waiting it out. Another workbench is
unaffected, which is what makes two operators at once workable. Within one
workbench the trade stands, because the alternative is a second kernel per
workbench — doubling the pod's memory for a case the developer can already answer
by opening another workbench.

What that cost is *not* is these operations refusing each other. They are cells of
a few milliseconds and a page load asks for three of them at once — the repository
status, the file tree and the open file — so refusing the second and third made the
Code pane come up with an error where the tree and the editor should be. Two
changes, at the two places that know something the other cannot. A workspace
operation now claims the kernel with a bounded wait
(`kernel.Options.WorkspaceWait`, 10s) and queues among its own kind; the 409 is
what a wait that expires reports, because past that bound the kernel is held by
something that is not a repository operation. And the SPA sends its repository
requests through one queue in `api.ts`, since it is the only place that knows the
requests it issued itself — they wait in the browser rather than each holding a
request open on the way to the same kernel. A developer's own cell still waits for
nothing: `Run` claims with no wait at all, because "busy" is the answer the pane
can act on, and the action is the interrupt. The SPA's queue is one *per
workbench*, for the same reason the kernels are: a single queue would put a file
read in one operator behind a clone in another, invisibly, because the backend
would see a request that never arrived rather than one it refused.

`.git` is the one exception to "every file", and deliberately: it is git's own
storage rather than source, the tree excludes it, and a write into it is refused.
A Code pane save that corrupted the object database would be unexplainable
afterwards.

## The scaffold is not invented

The template is the shape Operator Lib requires and the layout the platform's own
ML operator uses:

| File | What it is |
| --- | --- |
| `main.py` | Hands the process to `OperatorLib(Operator(), name=…)` |
| `op.py` | An `MLOperator` with `infer`, `train`, `need_retraining`, a `Selector` and a typed `Config` |
| `training.py` | The Ray task and the `PythonModel` MLflow registers |
| `pyproject.toml` | Dependencies, with Operator Lib pinned at the ref resolved when the repository was scaffolded |
| `Dockerfile` | `uv run`, because Ray workers inherit the launching interpreter; writes `git_commit` from the build's SHA |
| `.github/workflows/build.yml` | Builds and pushes `ghcr.io/{owner}/{name}` on `GITHUB_TOKEN`, no secret to set up |
| `operator.yaml` | The payload the analytics operator repository accepts on registration (§5.14) |
| `evaluation.yaml` | The developer's criteria. No tool writes it (§5.8) and the file says so |
| `tests/test_op.py` | Tests for the three methods that are the developer's, without Kafka, Ray or MLflow |

The Operator Lib pin is D15 made concrete: the newest tag is resolved **once**, at
scaffold time, recorded against the repository, and a second scaffold of the same
repository reuses it. So a repository does not move to a newer library because
someone re-ran the scaffold to recover a deleted file.

## write_file, and what it cannot do

`write_file` is §5.8's sixteenth implemented tool, at tier **L0** with no
confirmation — and that combination is only defensible because of what the tool
cannot do. It writes into the working copy on the developer's own storage. It
cannot stage, commit, push, select a repository, discard a change or leave the
repository, because the interface it is given has exactly one method. The worst
outcome is a file the developer reads in the pane and reverts, which is a diff
rather than an incident.

Its result says `committed: false` explicitly. A model that assumed otherwise
would tell the developer their change was live.

One file it refuses outright: `evaluation.yaml`. §5.8 lists "modifying evaluation
criteria" among the capabilities that are *denied* server-side rather than
permitted at some tier, and a tool that can write every file of a repository would
be a way around that — so the tool refuses that name and says why. The developer's
own routes write it like any other file, because it is theirs.

## The two authentication failures, and why they read the same

git reports "the credential was rejected" and "there was no credential" with the
same sentence. Both arrive as

```
remote: Invalid username or token. Password authentication is not supported for Git operations.
fatal: Authentication failed for 'https://github.com/owner/name.git/'
```

and the repairs are opposites: reconnect the GitHub account, or look at the pod.
Two things in `pkg/repo` exist because of that.

**`GIT_ASKPASS=/bin/false`, not `/bin/true`.** git only consults the askpass helper
when the `http.<url>.extraheader` credential was absent or refused. A helper that
exits 0 with no output answers *successfully with nothing*, and git sends that empty
answer as a credential — so a header that never reached git comes back as GitHub
rejecting a token. Measured against a server that logs the header:

| askpass | extraheader | what git sent | what git said |
|---|---|---|---|
| `/bin/true` | none | nothing | `fatal: Authentication failed for <url>` |
| `/bin/false` | none | nothing | `could not read Username for <url>: terminal prompts disabled` |
| either | set | `basic <base64 x-access-token:…>` | the remote's own answer |

The credential ships either way — the extraheader is configuration and has nothing
to do with askpass — so the only thing the change costs is the graphical-prompt
belt-and-braces, which `GIT_TERMINAL_PROMPT=0` already covers.

**`explainAuth`, which asks GitHub the question git cannot.** On an authentication
failure, ODE calls `GET /user` with the credential it holds:

- **401 or 403** — the token is revoked, expired, or the authorisation was
  withdrawn. `ErrCredentialRejected`, answered as `409` with
  `needs: "github_connection"`: the same repair as no connection at all, and a step
  the developer takes themselves. It is not a `502`, because nothing upstream broke.
- **200** — the credential works, so git in the pod could not use what it was given.
  git's own text stands and gains `GitError.Hint`, which says the credential is fine
  and names the likely cause: an image whose git predates the `GIT_CONFIG_COUNT`
  environment configuration (2.31, March 2021), or a pod that strips a command's
  environment.
- **anything else** — GitHub unreachable or rate limited. ODE does not know, so
  git's report is returned untouched. A check that fails must not turn a
  diagnosable error into a guess.

A permission refusal is deliberately *not* classified as an authentication failure.
"Permission to X denied", "Write access to repository not granted" mean the
credential worked and the grant is too narrow, which is a third repair —
re-consenting to `repo` and `workflow`, which the connection surface already
reports as `missing_scopes`.

**`GET /repo/connection?verify=true`** is the same question asked deliberately, and
the pane fetches it whenever a refusal blames the credential. It reports GitHub's
status and message, the scopes GitHub returns for the token — and whether it sent the
scopes header at all, which is not the same as an empty list — plus three things the
API cannot supply:

| Field | Why it is there |
|---|---|
| `kind` | The token's public prefix and what it means. `gho_` is an OAuth app's token: scopes, no expiry. `ghu_` is a GitHub App's user token: no scopes, expires in hours unless the app disables expiry, and reaches only repositories the app is installed on. A deployment that registered the wrong kind of app works for one afternoon and then refuses every push, and nothing else in ODE says so. |
| `length` | A whole token from a truncated one. |
| `age` / `stored_at` | Whether a reconnection actually happened. A credential GitHub refuses that ODE stored yesterday was never replaced — the flow was abandoned or it failed — and the repair is to finish it. One GitHub refuses that ODE stored a minute ago is a different search entirely. |

`stored_login` beside GitHub's `login` catches the last one: a developer who
reconnects in a browser signed in to a second GitHub account gets a credential that
works and cannot see their repositories. No part of the token's value appears in any
of it.

`ErrNotConnected` still means what it did: there is no stored row at all.
`Connection` reads that row and never asks GitHub, which is why the pane can say
"connected" about a credential that has since been revoked — and why the answer to
a failed push has to come from the failed push.
