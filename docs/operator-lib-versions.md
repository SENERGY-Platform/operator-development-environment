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
| `pyproject.toml` in the developer's repository | per repository | [service.go:665-676](../pkg/repo/service.go#L665), rendered at [scaffold.go:546](../pkg/repo/scaffold.go#L546), stored in `operator_lib_ref` ([database.go:547](../pkg/database/database.go#L547), [:598](../pkg/database/database.go#L598)) | The **deployed** operator's own container image — and every experiment, which resolves it out of the package |
| The singleuser image | deployment-wide | `ARG OPERATOR_LIB_REF` at [singleuser-image/Dockerfile:93](../singleuser-image/Dockerfile#L93); the profile comes from [`jupyterhub_profile`](../pkg/configuration/config.go#L261), passed at [kernel.go:396](../pkg/kernel/kernel.go#L396) | Every cell the developer runs |
| The Ray cluster's own image | deployment-wide, **outside this repository** | The cluster's configuration | The Ray version every experiment has to match — not the operator's own dependencies |
| `operator_lib_ref` in ODE's configuration | deployment-wide | [config.json:140](../config.json#L140) | What new repositories are pinned to; empty resolves the newest tag |

The distribution matters. The pin the developer can see in the Code pane
([code.tsx:1355](../frontend/src/code.tsx#L1355)) governs the production image
*and* the experiment, because both build their environment from
`pyproject.toml`. The kernel is the one that does not consult it: a cell runs
against the singleuser image's library, which is deployment-wide.

As of 2026-10-07 the library is at `v1.8.0` and pins the same as `v1.7.0`:

```text
ray[data]==2.55.0
ray[client]==2.55.0
mlflow==3.8.1
confluent_kafka==2.4.0
psycopg2-binary==2.9.11
```

`v1.6.1` moved none of them. It changes one thing, and only on the experiment
path: `MLOperator` no longer renames a run it was handed in `MLFLOW_RUN_ID`, so an
ODE run keeps the name the launch gave it. A repository picks that up when its
own pin moves, which for an existing repository is an edit to its
`pyproject.toml`; the singleuser image carries the library for the kernel, not
because a cell reaches that code.

`v1.7.0` moves none of them either, and again only the experiment path changes:
`Config` gains the optional `training_end` and `test_end`, every history reader
takes its end from a clock the config sets, and `MLOperator.init()` runs the
evaluation phase of D36 when `test_end` is present
([experiments.md](experiments.md)). The same release scores that replay where it
can: `Config` also takes `evaluation_metric`, `evaluation_target_series`,
`evaluation_prediction_field` and `evaluation_resolution`, and where all four
resolve the replay reports one metric over the test window as the four
`evaluation.metric_*` params D37's addendum grades from. Once more it is the
repository's own pin that decides.
The failure of a pin left behind is silent in the job — `simple_struct` reads
declared keys only, so an older library ignores both fields, trains up to the
launch and skips the evaluation — and visible in one place: the run's summary says
`"not confirmed by the run"` under `data_split`, because the tags the library
would have written are missing. A summary saying that after a launch under a split
is the runbook telling you it was not run.

`v1.7.1` closes a second way to the same summary, one a correct pin does not
prevent. `simple_struct` reads only the attributes in a class's own `__dict__`,
so on `v1.7.0` an `op.py` whose `class CustomConfig(Config)` declares a field of
its own — the scaffold's declares `retrain_after_s` — left every inherited field
at its default: the split, the four `evaluation_*` fields and `ts_wrapper_url`.
`v1.7.1` copies `Config`'s fields into each subclass (`Config.__init_subclass__`).
Deployed operators with such a subclass receive every base field their config
sets — `mlflow_url`, `ray_url`, `ts_conn`, `logger_level` among them — for the
first time once their pin moves, so check which deployed configs carry those keys
before moving a pin. A repository on
`v1.7.0` that cannot move yet copies the fields itself, directly after the class:

```python
for _name, _value in vars(Config).items():
    if not _name.startswith("_") and _name not in vars(CustomConfig):
        setattr(CustomConfig, _name, _value)
```

`v1.8.0` moves none of the pins either. `Config` gains `import_exports`, which
the launch sets for an import input whose one executable export it resolved, and
that input's history is then read from the export instead of the Kafka topic
([imports-as-operator-inputs.md](imports-as-operator-inputs.md)). The replay's
metric scores each resolution bucket once, so `evaluation.metric_n` counts buckets
rather than predictions. A row on a timescale-wrapper chunk boundary is no longer
dropped. A repository left on `v1.7.x` ignores `import_exports` and keeps reading
imports from Kafka, which is the only symptom.

**A version this file names is not necessarily a version that exists.** The
library bumps `operator_lib/__init__.py`'s `__version__` inside the commit that
earns it rather than in a release step of its own, so a working copy can read
`v1.7.0` while the newest tag on the remote is still `v1.6.1` — and this file,
written alongside that work, describes the new version as though it were
installable. Step 6 below then pins a ref that nothing can resolve. Read the tags
rather than the version string before treating a release as available:

```text
git ls-remote --tags git@github.com:SENERGY-Platform/analytics-operator-lib-python.git
```

As of 2026-09-21, `v1.7.0` is tagged on `master`; the steps below it are not run.
As of 2026-10-06, `v1.7.1` is tagged on `master`.
As of 2026-10-07, `v1.8.0` is tagged on `master`.

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

## An experiment uses the repository's pin, and the cluster's Ray

The launch path behaves the way it looks, which is worth saying because this
section used to claim the opposite: ODE packages the developer's **committed**
tree with `git archive HEAD` ([archive.go:38](../pkg/experiments/archive.go#L38)),
so `pyproject.toml` and `uv.lock` travel with it, uploads it as the job's
`working_dir`, and runs the configured entrypoint inside it. That entrypoint is
`uv run python train.py` ([config.json:159](../config.json#L159)) and the workers
are started with `py_executable: "uv run"`
([config.json:164](../config.json#L164)), so uv builds the same environment from
those two files on the head and on every worker node, out of its own cache.

So when the scaffold's `train.py` runs

```python
import operator_lib.util as util
from op import Operator
```

`op` comes from the uploaded working directory and **`operator_lib` comes from
the repository's own pin** — `operator-lib @ git+…@<ref>` in the scaffolded
`pyproject.toml` ([scaffold.go:546](../pkg/repo/scaffold.go#L546)), resolved once
at scaffold time from `operator_lib_ref` and recorded per repository. Same for
`mlflow` and `confluent_kafka`, which Operator Lib pins. Nothing installs Operator
Lib on the Ray cluster for this, and nothing should: an operator's dependencies
are its own, and torch is not predictable from a shared image.

**All of that holds while the entrypoint starts the driver with `uv run`.** A
launch may name its own ([api/experiments.go:71](../pkg/api/experiments.go#L71),
and the `launch_experiment` tool takes one), it is not validated, and
`py_executable` is set to the deployment's value either way
([service.go:619-627](../pkg/experiments/service.go#L619)). So an entrypoint of
plain `python train.py` puts the *driver* on whatever interpreter the cluster
image carries while the workers stay in the uv environment — the one
configuration in which the cluster's own library matters, and a split
driver/worker environment on top of it. The tool's schema asks for the prefix
rather than the launch refusing its absence, which is a gap named here rather
than closed.

What the cluster's image does decide is **Ray**. `experiment_ray_client_url` is
`"auto"` ([config.json:162](../config.json#L162)), so the driver attaches to the
cluster it is already running in — but the `ray` it attaches with comes out of the
uv environment, resolved through Operator Lib's own pins, and Ray refuses a client
whose version differs from its cluster's. The version argument in this file
therefore reaches a repository *through* Operator Lib rather than around it.

Three things follow:

1. **The Ray cluster needs Ray, not Operator Lib** — for a launch that keeps the
   `uv run` prefix. A cluster image that carries no Operator Lib at all is then
   correct. A cluster on a different Ray version than the one Operator Lib pins
   is not, and it fails at `ray.init`, before a line of the developer's code
   runs. ODE reports no version and compares none: the
   empty-`ray_url` degradation
   ([config.go:382](../pkg/configuration/config.go#L382)) covers a *missing*
   cluster, not a mismatched one.
2. **An experiment tests the developer's source against the library they pinned.**
   That is what makes a passing run a statement about the operator being built
   rather than about the cluster it ran on — and it is also why a repository left
   on an older pin silently runs an older library, features included: a session's
   data split shows as `"not confirmed by the run"` rather than as anything about
   the pin.
3. **A cell and an experiment can still disagree.** The kernel imports Operator
   Lib from the singleuser image
   ([Dockerfile:92](../singleuser-image/Dockerfile#L92)), an experiment from the
   repository's pin. Those are two versions whenever a repository was scaffolded
   before the image last moved, and nothing compares them.

## When a new Operator Lib is released

In this order. The order is the content: step 2 gates everything after it, and
step 8 is the one with no artefact in this repository — nothing here will remind
anybody about it.

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
6. **Nothing is installed on the Ray cluster.** For the reason the previous
   section gives: an experiment resolves Operator Lib out of the package it was
   launched with, so the cluster only has to carry a Ray the new pin agrees with
   — which is step 2, or nothing at all when the release moves no Ray.
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
- Which Ray version the cluster's images currently carry is not recorded anywhere
  in this repository, and ODE cannot report it. Confirming it is a cluster-side
  check.
- That uv can fill its cache on a worker node has not been checked. A first run
  of a new commit resolves `operator-lib` from GitHub and the rest from PyPI, so
  a node without that egress fails whatever the cluster image holds. Nothing in
  this repository states the Ray nodes' egress rules; the scaffold pod's are
  stated ([scaffold.go:100-103](../pkg/repo/scaffold.go#L100)) and are a
  different pod.
