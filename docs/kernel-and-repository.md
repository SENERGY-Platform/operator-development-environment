# The kernel and the repository working copy

Everything a developer writes lives on their own JupyterHub pod, not on ODE.
That is why git runs in the pod, why a file read can answer 409 during a training
run, and why ODE starts pods and never stops them.

## Applies when

Working on `pkg/kernel` or `pkg/repo`, or diagnosing why a file a developer
wrote is gone after a cull.

**Not this if**: the question is what the *deployed* Hub does differently from
what §5.6 assumes. The username claim and the PVC mount point are properties of
that Hub rather than of ODE and are documented on the platform side;
`jupyterhub_username_claim` and `jupyterhub_workspace_path` are the two settings
that exist because of them, and are all ODE needs to know.

`geltung`: `allgemein` for the one-cell-at-a-time consequence and the pod
lifecycle; the rest follows from SPEC §5.6 and §5.11.

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
409 rather than queueing — a second ODE-owned kernel would fix that and has not
been built.

### ODE spawns pods and never stops them

Shutting down a kernel leaves the pod
running, and the reaper that drops an idle session closes ODE's socket and stops
its keep-alives rather than deleting anything. Both are the same judgement: the
pod is the developer's, their files and their running processes are on it, and a
respawn costs them a cold start. Reclaiming it is the cluster's idle culling —
which ODE stops holding off precisely so that it can.

### The kernel connection is persistent per developer, and that is not only an optimisation

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
read while a training cell is running answers **409** with `needs: idle_kernel`
rather than queueing behind it. A second, ODE-owned kernel per developer would
decouple the two; that is a real cost per developer and the case for it should come
from use.

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
