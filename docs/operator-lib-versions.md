# Operator Lib versions: one Ray cluster, one supported version

Only the **latest** Operator Lib is supported. Not as a policy preference — as a
consequence of one fact: Operator Lib pins the Ray *client* libraries, ODE runs
against exactly one Ray cluster, and Ray does not talk to a client of a different
version. Every other version rule here follows from that, including the awkward
one: an operator on an older pin updates that pin before implementation on it can
continue.

## Applies when

A new Operator Lib is released and ODE has to follow it; a developer asks why they
cannot stay on the version their repository was scaffolded with; a training run
fails at `ray init` rather than in `train()`; or someone proposes making the
version selectable.

**Not this if**: the question is what a run packages and who creates the MLflow
run — that is [experiments.md](experiments.md). The scaffold's file list and the
per-repository pin as a *mechanism* are
[kernel-and-repository.md](kernel-and-repository.md); what this file adds is why
the pin is not a choice.

The Ray argument and the fact that an experiment does not use the repository's
pin both follow from the code and from Operator Lib's `setup.py`. The version
numbers and the runbook's cluster steps are this deployment's at this time —
read them off the cluster before acting on them, because a release moves them
and nothing here fails when they go stale.

## Four places a version lives, and which one decides what

| Place | Scope | Set by | Decides |
| --- | --- | --- | --- |
| `pyproject.toml` in the developer's repository | per repository | [service.go:665-676](../pkg/repo/service.go#L665), rendered at [scaffold.go:472](../pkg/repo/scaffold.go#L472), stored in `operator_lib_ref` ([database.go:466](../pkg/database/database.go#L466), [:517](../pkg/database/database.go#L517)) | The **deployed** operator's own container image, and nothing else |
| The singleuser image | deployment-wide | `ARG OPERATOR_LIB_REF` at [singleuser-image/Dockerfile:60](../singleuser-image/Dockerfile#L60); the profile comes from [`jupyterhub_profile`](../pkg/configuration/config.go#L261), passed at [kernel.go:396](../pkg/kernel/kernel.go#L396) | Every cell the developer runs |
| The Ray cluster's own image | deployment-wide, **outside this repository** | The cluster's configuration | Every experiment ODE launches |
| `operator_lib_ref` in ODE's configuration | deployment-wide | [config.json:140](../config.json#L140) | What new repositories are pinned to; empty resolves the newest tag |

The distribution matters. The pin the developer can see in the Code pane
([code.tsx:1355](../frontend/src/code.tsx#L1355)) governs the *production* image
only. The two environments they actually work in — the kernel and the Ray
cluster — are both deployment-wide and neither consults it.

As of 2026-08-31 the library is at `v1.3.6` and pins:

```text
ray[data]==2.55.0
ray[client]==2.55.0
mlflow==3.8.1
confluent_kafka==2.4.0
psycopg2-binary==2.9.11
```

## Why only the latest is supported

`MLOperator` connects to Ray itself, from the deployment config:

```python
ray.init(address=require_config(self.config.ray_url, "ray_url", "reach the ray cluster"))
```

— `operator_lib/util/op_ml.py`, in `__wrap_training`. Everything a training run
does with data goes through `ray.data` and `ray.train`:
`provide_historic_data` returns `List[ray.ObjectRef[ray.data.Dataset]]`, the
timescale and Kafka readers are `@ray.remote` tasks returning `ray.data.Dataset`,
and `TrainMlflowLogger` subclasses `ray.train.UserCallback`.

So the Ray version is not a transitive detail of Operator Lib. It is the wire
protocol between an operator and the cluster. Ray requires a client and its
cluster to be the same version — a `ray://` client connection is version-checked
and refused on a mismatch, and `ray.data`/`ray.train` objects are not compatible
across versions even where a connection is established.

There is one Ray cluster. Therefore there is one usable Ray version, therefore
one usable Operator Lib version, and it is whichever one the cluster was built
for. A repository pinned to an older Operator Lib is a repository whose operator
image cannot train: it fails at `ray.init`, before any of the developer's code
runs.

**The consequence, stated plainly.** An operator whose repository carries an older
pin has to move to the current one *first*. Not as cleanup afterwards — the
per-repository pin is what its build installs, so until it moves, every image
built from that repository is unable to reach the cluster. Continuing to implement
against it produces code that cannot be trained.

Two of the other pins carry the same shape of constraint, more weakly:

- **`mlflow==3.8.1`.** The tracking protocol tolerates more version skew than Ray
  does, so a mismatch here is a risk rather than a refusal — but the model
  registry is what a promotion decision reads, and it is not worth being casual
  about.
- **`confluent_kafka==2.4.0`.** This is why the singleuser base image is pinned to
  Python 3.12 rather than 3.13: 2.4.0 publishes no cp313 wheel, so a 3.13 base
  falls back to building from source against librdkafka headers that are not
  there. The reasoning is at
  [singleuser-image/Dockerfile:24-36](../singleuser-image/Dockerfile#L24), and the
  base moves forward when this pin does.

## An experiment does not use the repository's pin

This is the part most likely to mislead someone, because everything about the
launch path suggests otherwise: ODE packages the developer's committed working
copy, uploads it, and runs `python train.py` inside it.

But the runtime environment ODE sends carries two keys and no third:

```go
RuntimeEnv: jobRuntimeEnv{
    WorkingDir: uri,
    EnvVars:    environment,
},
```

— [service.go:512](../pkg/experiments/service.go#L512), over the struct at
[ray.go:75](../pkg/experiments/ray.go#L75), whose comment says outright that
`pip` and `conda` belong to the repository's own code. Nothing installs anything.
So when the scaffold's `train.py` runs

```python
import operator_lib.util as util
from op import Operator
```

`op` comes from the uploaded working directory and **`operator_lib` comes from the
Ray cluster's own Python environment**. Same for `mlflow`, `confluent_kafka` and
`ray` itself. `experiment_ray_client_url` is `"auto"`
([config.json:153](../config.json#L153)), which attaches to the cluster the driver
is already running in, so the client/cluster versions match trivially — because
they are the same installation.

Three things follow:

1. **The Ray cluster's image is an ODE deployment prerequisite that ODE never
   checks.** If Operator Lib is not installed there, every launch fails with an
   `ImportError` in the job log — a Ray-side failure, at the point where it looks
   like the developer's code. ODE reports no version, compares no version, and
   has nothing to say about it. The empty-`ray_url` degradation
   ([config.go:382](../pkg/configuration/config.go#L382)) covers a *missing*
   cluster, not a wrongly provisioned one.
2. **An experiment tests the developer's source against the cluster's library, not
   against their pin.** A run can pass on a repository whose `pyproject.toml`
   would build an operator that cannot start. The reverse also holds: an
   experiment fails on a library the repository does not name. Neither is visible
   from the run.
3. **Keeping the cluster and the singleuser image on the same Operator Lib is not
   tidiness.** It is what makes "it worked in a cell" and "it worked as an
   experiment" mean the same thing. They are two separately built images with no
   mechanism keeping them in step, only the runbook below.

## When a new Operator Lib is released

In this order. The order is the content: step 2 gates everything after it, and
steps 6 and 8 are the two with no artefact in this repository — nothing here will
remind anybody about them.

1. **Read the diff of `setup.py`.** The pinned versions are the whole reason this
   is not automatic. `ray[client]`/`ray[data]`, `mlflow`, `confluent_kafka`,
   `psycopg2-binary` and `python_requires` are the five lines that decide how much
   work follows. A release that moves none of them is a small job; one that moves
   Ray is a cluster operation.
2. **If Ray moved: the cluster goes first.** The Ray cluster is shared, so this is
   not an ODE-local decision and it is not reversible per developer. Nothing else
   in this list can be rolled out ahead of it — a new singleuser image against a
   not-yet-upgraded cluster breaks every launch for everyone.
3. **If `confluent_kafka` moved: check for a cp313 wheel** and move the singleuser
   base image forward if there is one. Leaving it behind is safe; moving it
   without checking is what puts a source build into the image.
4. **Rebuild the singleuser image.** Run
   [.github/workflows/singleuser.yml](../.github/workflows/singleuser.yml) with
   `operator_lib_ref` set to the new tag. It resolves the tag to a commit SHA,
   builds, runs the import and kernelspec checks, and publishes a `date-sha` tag.
   There is deliberately no `latest` — a moving tag on a KubeSpawner profile
   changes a developer's environment mid-project.
   Note the drift the workflow already documents: the library's git tag and
   `operator_lib.__version__` do not always agree, which is why the image is
   traced by commit SHA and not by version string.
5. **Point the KubeSpawner profile at the new tag**, in the cluster's own
   configuration, and confirm [`jupyterhub_profile`](../pkg/configuration/config.go#L261)
   still names that profile. A spawn that names nothing gets the plain notebook
   image, without Operator Lib at all.
6. **Install the same Operator Lib on the Ray cluster's head and worker images**,
   for the reason the previous section gives: the experiment driver imports it
   from there. This is the step with no artefact in this repository and therefore
   the one nothing will remind anybody about.
7. **Check the scaffold against the new library.** The template is the shape
   Operator Lib actually calls, so a rename upstream makes it a file that looks
   right and never runs. The surfaces it depends on:
   `MLOperator`, `Config`, `Selector` and `TrainMlflowLogger`
   ([scaffold.go:299-300](../pkg/repo/scaffold.go#L299)), `provide_historic_data`
   ([scaffold.go:400](../pkg/repo/scaffold.go#L400)), `OperatorLib(...)`
   ([scaffold.go:190-194](../pkg/repo/scaffold.go#L190)), and in `train.py`
   `util.DeploymentConfig`, `util.OperatorConfig`, `util.create_filter_handler`
   and the keyword arguments of `operator.init(...)`.
   [`TestTheOperatorSkeletonImplementsWhatOperatorLibCalls`](../pkg/repo/scaffold_test.go#L118)
   does **not** catch this: it asserts that certain strings are present in the
   rendered template, never that the library exports them. Read the upstream diff.
8. **Existing repositories keep their old pin, and nothing moves it.** The
   `operator_lib_ref` column is written once, at the first scaffold, and reused on
   every later scaffold of the same repository
   ([service.go:665-668](../pkg/repo/service.go#L665)) — deliberately, so
   re-running a scaffold to recover a deleted file cannot move a developer to a
   newer library. There is no upgrade route, no tool and no UI action. So each
   repository's `pyproject.toml` has to be edited by whoever owns it, and until it
   is, its build produces an operator that cannot reach the cluster. This is the
   step that makes "an older operator updates first" an action rather than a
   sentence.
9. **Update what names a version in this repository.** Test fixtures at
   [scaffold_test.go:33](../pkg/repo/scaffold_test.go#L33) and
   [repotest/github.go:80](../pkg/repo/repotest/github.go#L80) hard-code `v1.3.1`;
   they are fixtures and will keep passing, but they are also what a reader takes
   for current. The frontend contract fixtures
   ([repo_scaffold.json](../frontend/src/__contract__/repo_scaffold.json),
   [repo_status.json](../frontend/src/__contract__/repo_status.json),
   [workbenches.json](../frontend/src/__contract__/workbenches.json)) carry it as a
   captured value; `contract.ts` checks types rather than values, so these do not
   break either. Update this file's version block while you are here.
10. **Leave `operator_lib_ref` in the configuration empty.** Empty means "newest
    at scaffold time", which is the behaviour that keeps new repositories correct
    without anybody remembering. A deployment that has set it — to reproduce an
    evaluation write-up — has to move it too, or every new repository is scaffolded
    onto a library the cluster cannot serve.

## Making the version selectable, and why it is not built

Recorded so it is not re-derived. D15 reads *"track latest; pin per-repo at
scaffold time and allow upgrade"* ([decisions.md:61](decisions.md#L61)); the
"allow upgrade" half was never built, and the Ray argument above is why building
it is worth less than it looks.

**Choosing and changing the pin** is the cheap half and the only one that would
pay for itself, mostly as an *upgrade* path rather than a choice of versions:

- A ref list to choose from: a `Refs` method beside
  [`LatestRef`](../pkg/repo/github.go#L216) over `/repos/{owner}/{name}/tags`, plus
  a route. It goes through the per-user GitHub client
  ([`clientFor`](../pkg/repo/service.go#L670)), so the result wants caching — the
  list is identical for every developer.
- An `operator_lib_ref` field on the create and scaffold requests
  ([repo.go:257](../pkg/api/repo.go#L257)). **It must be validated**: the value is
  interpolated into a `git+https://…@<ref>` URL and a `pyproject.toml` line, and
  no ref validator exists today — only
  [`repositoryName`](../pkg/repo/service.go#L1284) and
  [`unsafeSegment`](../pkg/repo/service.go#L1273).
- An upgrade route rewrites one dependency line in a file the developer owns,
  which collides with the scaffold's never-overwrite rule
  ([scaffold.go:38](../pkg/repo/scaffold.go#L38)) and with D14. The consistent
  resolution is the scaffold's own: write it, do not commit, let it be read as a
  diff.
- No migration — the column exists on both tables already.
- It stays off the tool surface. [`modify_operator_lib`](../pkg/tools/registry.go#L314)
  is denied for a different reason (editing the library's code), but a version
  change is a developer decision either way.

**Making the kernel run the chosen version** is where it stops being worth it:

- One singleuser image per supported version, with a version-derived tag a profile
  can name, and the Python base as a second `ARG` — the supported set is
  `(operator_lib_ref, python_base)` pairs, not a list of refs.
- One KubeSpawner profile per version, declared outside this repository.
  `Options.Profile` ([kernel.go:135](../pkg/kernel/kernel.go#L135)) becomes a
  lookup rather than a constant.
- **The blocker:** JupyterHub gives a user one server, and
  [`StartServer`](../pkg/kernel/hub.go#L217) spawns the default one, choosing the
  profile at spawn time. A developer holding several workbenches on different pins
  would need named servers, which `pkg/kernel` is not built for. So this is
  realistically a per-*developer* setting with a respawn on change — losing running
  kernels, keeping the PVC — not a per-workbench one.
- And it still would not help, because of the Ray cluster. A kernel on an older
  Operator Lib can import and can compute in-pod, but the run that matters happens
  on the one cluster at the one version.

**Making experiments match** means adding `pip` to
[`jobRuntimeEnv`](../pkg/experiments/ray.go#L75) from the repository's pin, paying
an install per job — and it cannot fix the Ray version, which is the cluster's.
That is the point where the whole idea collapses: the constraint is not a missing
feature in ODE, it is that there is one cluster.

So the decision stands: track latest, and treat an old pin as work to be done in
the repository rather than a configuration ODE should support.

## What is not verified

- The exact failure a Ray version mismatch produces against *this* cluster has not
  been reproduced. The argument rests on Operator Lib's pins, its `ray.init` call
  site, and Ray's documented client/cluster version requirement.
- Which Operator Lib the Ray cluster's images currently carry is not recorded
  anywhere in this repository, and ODE cannot report it. Confirming it is a
  cluster-side check.
