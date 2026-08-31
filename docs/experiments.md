# Experiments: Ray, MLflow and the commit a run comes from

A run is submitted from a commit or it is not submitted. Everything else here
follows from that: what gets packaged, who creates the MLflow run, which
credential the job carries, and what a completed run may and may not hand back to
a model.

## Applies when

Working on `pkg/experiments`, or diagnosing a launch that was refused, a run
whose commit cannot be identified, or an embed that will not frame.

**Not this if**: the question is what happens *after* a run finishes without
anyone watching — that is the interpretation turn, see
[result-interpretation.md](result-interpretation.md). The kernel's own
mechanics — the pod, the workspace, one cell at a time — are
[kernel-and-repository.md](kernel-and-repository.md); what this file adds about
them is only why a cell is not a run.

The commit rule and the credential model follow from §5.12 and §3.1 item 6. What
the embed probe *answers* does not: it reports the headers this deployment's Ray
and MLflow send, so an `embeddable` verdict from one cluster says nothing about
another.

## A run is submitted from a commit, or it is not submitted

Launch an experiment from a scaffolded repository that has not been committed and
the answer is a **409**, not a job:

```text
POST /experiments
409 {"error":"the working copy of devuser/pv-forecast has no commit yet, and an
      experiment is submitted from a commit so that its MLflow run is
      reproducible from one (§5.11 item 7)",
     "needs":"commit","unborn":true,"repository":"devuser/pv-forecast"}
```

With a commit and then a stray edit, the same refusal names the files:

```text
409 {"error":"the working copy of devuser/pv-forecast has uncommitted changes
      (notes.md, op.py); an experiment is submitted from the committed state so
      that its recorded commit SHA is the code that ran …",
     "needs":"commit","uncommitted":["notes.md","op.py"],"uncommitted_elided":0}
```

This is the guard the milestone is built around rather than a nuisance check.
§5.11 item 7 says every run is reproducible from a specific code state, and the
way that claim is made is an MLflow tag carrying the commit SHA. If the package
Ray receives is the *working directory*, the tag describes a tree that never ran
— and it looks exactly like a correct tag to everyone downstream, forever. So the
package is `git archive --format=zip` of the resolved HEAD commit: the committed
tree, files at the archive root, `.git` excluded, ignored artefacts excluded.
Both properties Ray's package format wants, and both properties the reproducibility
claim needs.

`needs: "commit"` is there so the pane offers a commit button rather than printing
a sentence, the same shape M7's two 409s have. ODE will not commit for the
developer: §5.11 item 5 makes that a human action, and a launch that quietly
committed to make itself possible would be exactly the silent commit that rules
out.

## The kernel loop is not a smaller experiment

The question that follows a refusal is the obvious one: the operator code carries
Operator Lib's own MLflow integration, so why not run it in a cell and skip the
commit? The cell is a good loop and nothing here discourages it. It simply
produces a different thing at the end.

What ODE installs in a kernel is four variables (§5.6 item 4) — `SENERGY_TOKEN`,
`ODE_WORKSPACE`, `SENERGY_DEVICE_REPO_URL`, `SENERGY_TIMESCALE_URL`. That is
`kernelEnvironment` in `pkg/pkg.go`, and it is the whole list: no `CONFIG`, no
`RAY_ADDRESS`. A kernel is not a deployment, so it has no deployment config, and
without one Operator Lib has nothing to connect with — `provide_historic_data`
reads `DeploymentConfig.config` and raises on its absence.

Both client libraries are in the pod regardless: the singleuser image installs
Operator Lib, which brings `ray` and `mlflow` with it. The imports work, which is
why the two gaps are worth naming rather than assuming.

- **`ray.init()` in a cell gives a Ray inside the pod.** `training.py`'s
  `_fit.remote()` runs, and it runs on the pod's CPUs and the pod's memory. The
  scaffold's ninety-day `TRAINING_WINDOW` is roughly where that stops being the
  same computation as the one the cluster would do.
- **A run logged from a cell carries no tags.** With no tracking URI the MLflow
  client writes to its local default store in the pod; with one set by hand the
  run does reach the server, but it holds none of `runTags` — `commit_sha`,
  `user_sub`, `ode_experiment_id`, `repository`, `submission_id`, `entrypoint`,
  `source=ode`. The Experiments pane lists by those tags and the interpretation
  turn reads them, so the run is invisible to both.

So there are three loops rather than two, and the middle one is not a lesser
version of the third:

1. **The scaffold's `tests/test_op.py`**, which runs without Kafka, without Ray
   and without MLflow — that is what it is for. It is the loop a change to
   `infer()` or `need_retraining()` wants.
2. **`run_code` on the operator's own code.** The fit is ordinary Python, the
   platform is reachable with the developer's token through the `ode_platform`
   helper the singleuser image ships, and nothing is recorded. This is the loop
   for "does this do what I think it does", and it needs no commit.
3. **`launch_experiment`**, when the result is one to hold against another result
   weeks later. That is the loop the commit rule belongs to, because the
   comparison rests on knowing which code produced which number. It is also the
   only one of the three that runs what the deployed operator runs.

The assistant is told the same thing, in the two tool descriptions rather than in
the system prompt: `run_code`'s says the kernel is wired to neither MLflow nor the
cluster, and `launch_experiment`'s says to ask whether a recorded run is what the
developer wants before proposing one. The descriptions are where the choice is
actually made, and a deployment without a Ray cluster does not pay for the
paragraph — only implemented tools are offered to a model.

Reaching for 3 where 2 would do is what makes the commit rule read as a toll
gate. It is not one — it is what a result costs if it has to still mean something
in a fortnight.

## The archive does not come back through ReadFile

`git archive` runs in the developer's pod, because that is where the working copy
is. Getting the zip back is the part worth knowing about, because the obvious
route does not work: `kernel.Service.ReadFile` decodes a file as UTF-8 and
reports anything that does not as `binary: true` with **no content at all**. That
is correct for the Code pane — it must not hand a JPEG to a text editor — and
useless for a zip.

So the archive is written to a temporary path in the pod's `/tmp`, and a small
Python helper carries it back base64-encoded on stdout, enforcing the size cap and
unlinking the file in a `finally` on the way. Three consequences, all deliberate:

- **The staging path is `/tmp`, not the workspace.** Inside the checkout it would
  make the working copy dirty and so make the next launch refuse itself; elsewhere
  in the workspace it would appear in the Code pane's tree as a file the developer
  did not create. `/tmp` in a pod is ephemeral, which is the right lifetime.
- **The cap is a refusal, not a truncation.** A job that ran against a silently
  shortened copy of the repository fails in a way nobody could diagnose from the
  run, so `experiment_max_package_bytes` answers 409 with the actual size and the
  limit — the gap is what tells a developer whether they need a `.gitignore` fix
  or a raised bound.
- **It costs a kernel cell.** A kernel runs one cell at a time, so a launch during
  a long-running cell answers **409** with `needs: idle_kernel`, exactly as the
  repo routes already do.

## One upload per commit

The package's name in Ray's object store is `_ray_pkg_<sha256 of the zip>.zip`, so
the name *is* the content. ODE asks the cluster whether it already holds that name
and uploads only when it does not. `git archive` of one commit is byte-identical
every time, so a second launch from the same commit finds it there:

```text
POST /experiments  → "package_uri":"gcs://_ray_pkg_9f3c….zip","package_reused":false
POST /experiments  → "package_uri":"gcs://_ray_pkg_9f3c….zip","package_reused":true
```

A new commit produces a new name and a new upload. Nothing caches on ODE's side;
the cluster's own package store is the cache, which is the only one that can be
right after a restart.

## The package is code, not an environment

The runtime environment ODE sends carries `working_dir` and `env_vars` and nothing
else (`jobRuntimeEnv` in `pkg/experiments/ray.go`) — no `pip`, no `conda`. So the
upload is the developer's *source*, and every import it makes resolves against the
**Ray cluster's own Python environment**: `operator_lib`, `mlflow`,
`confluent_kafka` and `ray` all come from there, not from the repository's
`pyproject.toml`.

Two consequences worth knowing before diagnosing a failed run. The cluster's
Operator Lib is an unchecked deployment prerequisite — if it is absent, every
launch fails with an `ImportError` in the job log, at the point where it looks
like the developer's code. And a run therefore tests the source against the
cluster's library rather than against the pin the repository was scaffolded with,
so a passing run does not prove the operator's own image would start.
[operator-lib-versions.md](operator-lib-versions.md) has the rest, including why
only the latest Operator Lib is supported at all.

## The run is Operator Lib's; the commit is ODE's

ODE used to do the machine-learning integration itself: submit the job, create the
MLflow run, and set `MLFLOW_TRACKING_URI`, `MLFLOW_EXPERIMENT_ID` and
`MLFLOW_RUN_ID` so the developer's code could log. That was a second
implementation of what Operator Lib already does, and the two never met. Operator
Lib reads `CONFIG`, `PIPELINE_ID` and `OPERATOR_ID`; it connects to Ray and MLflow
from `Config.ray_url` and `Config.mlflow_url`; and `provide_historic_data` reads
`DeploymentConfig.config`. None of those is a name ODE set, so a packaged operator
could not read a row of history — `json.loads(None)` raised before it tried. A
`TrainMlflowLogger` calls `set_tracking_uri` from the config as well, so even the
one variable that looked load-bearing was being overridden rather than honoured.

So the job is given what Operator Lib actually reads, and the ML integration is
Operator Lib's:

1. the working copy is checked and the launch refused if it is dirty;
2. the launch is refused if it names no input topics — see below;
3. the package is built from that commit and uploaded;
4. the per-user experiment is found or created (D17);
5. **the run is created, with its tags in the same request**;
6. the job is submitted with the deployment config, and `MLOperator` sets the
   tracking URI, opens the run, connects to Ray, calls `train()` and registers the
   model — the same sequence a deployed operator performs when it first comes up.

Step 5 is a `runs/create` carrying `commit_sha`, `session_id`, `user_sub`,
`ode_experiment_id`, the repository, the branch, the entrypoint and the Ray
submission id — not a create followed by five `set-tag` calls. The difference is
that there is no window in which the run exists without the tag the whole claim
rests on; a crash between two round trips would otherwise leave a run permanently
unreproducible and looking fine.

**The job adopts that run rather than opening its own.** `MLOperator` calls
`mlflow.start_run(run_name=...)` without a run id, and MLflow's fluent API resumes
the run `MLFLOW_RUN_ID` names when none is passed. Without that one variable there
would be two runs per experiment — ODE's, carrying the commit tag and no metrics,
and the job's, carrying the metrics and no commit — and `get_experiment_results`
reads ODE's.

The experiment stays D17's, one per developer and repository
(`ode/{hub username}/{owner}-{repo}`), because `Store.Previous` scopes §5.13's
comparison to it and a per-run experiment would make every run a first run.
`MLOperator` also calls `set_experiment(model_id)`, but that does not move the
run: `start_run` resumes by id whatever experiment is selected. What it leaves
behind is one empty MLflow experiment per launch, which is litter rather than a
problem.

## The pipeline and operator ids ODE invents

A deployed operator gets its pipeline and operator ids from the flow engine. A run
being developed has no deployment, so ODE synthesises the pair: the pipeline per
developer, the operator per repository. The pair is what Operator Lib builds
`model_id` from — `pipeline-{pipeline}_operator-{operator}` — and both halves are
stable, so a repository's model versions accumulate under one registry key and the
`production` alias moves between them. That is what a deployed operator does, and
running the real path is the point.

The pair still cannot collide with a deployed operator's, whose ids are a real
flow-engine pipeline id and a real operator id, so no run started here can move a
deployed operator's alias.

**It was per-launch until Operator Lib v1.4.0.** `MLOperator.init()` trains only
when no model is registered under the pair, so a stable pair would have trained on
the first launch and silently recorded nothing on the second. The pair was made
unique per launch to miss by construction. v1.4.0 made `train_once()` public — a
training pass a caller can ask for — and `train.py` asks, so the pair no longer
has to carry that job. The per-launch pair also left one empty MLflow experiment
behind per launch, from `init()`'s own `set_experiment(model_id)`; one per
repository is the remainder.

`train.py` asks the alias question itself before calling `init()`, because `init()`
does not report whether it trained. Getting that wrong costs a duplicate training
pass rather than a wrong result, which is why it is a cheap `get_model_version_by_alias`
rather than anything more careful.

## A run with no inputs is refused

`inputTopics` is what decides which history a run reads, so a launch that names
none is refused with 400 rather than submitted:

```text
POST /experiments
400 {"error":"this experiment has no input topics, so a run would read no history
      and fail inside train(); choose the operator's inputs first
      (propose_data_selection for a device, propose_operator_input for an import)"}
```

The alternative is not a smaller experiment. `provide_historic_data` returns an
empty list, the scaffold's `train()` returns `None` on it or raises, and what the
developer gets is cluster time spent on a failed run and a Python traceback in a
log they have to go and find. The refusal names the tool that fixes it, the way
the uncommitted-changes refusal names the commit.

## `train.py`, and why the entrypoint is not `main.py`

`main.py` is the deployed operator's entrypoint and is the wrong one here.
`OperatorLib.__init__` builds the Kafka clients, calls `operator.init(...)` — which
is where `MLOperator` trains — and then calls `operator.start()` and
`watchdog.join()`. An experiment would train correctly and then consume Kafka
forever.

So the scaffold carries `train.py`: Operator Lib's own init sequence, stopped
exactly where `main.py` would enter the loop. It is a committed file rather than
something ODE injects into the archive, because the package has to stay exactly
the commit it claims to be (§5.11 item 7). It is also the developer's to read and
change, which a file materialising inside a job would not be.

A repository scaffolded before `train.py` existed does not have it. `ScaffoldState`
reports it as missing, the way it reports any other absent file of the compliance
set; a launch against such a repository runs the deployment default and fails on
`python: can't open file 'train.py'`.

`train.py` needs Operator Lib **v1.5.0 or newer**, and that floor is the one for
`operator_lib_ref` and for the singleuser image's `OPERATOR_LIB_REF`. It is *not* a
requirement on the Ray image, for the reason the next section gives.

Two releases matter, and they fail differently. v1.4.0 made `train_once()` public;
an image below it raises `AttributeError` at the end of a run, which is late but
unmissable. v1.5.0 is where `Config.ts_conn` lost its compiled-in default and
`ts_wrapper_url` appeared — ODE sets the wrapper so a run reads history under the
developer's own permission (SNRGY-4637), and an image below it does not know the
field, ignores it, and falls back to the built-in DSN. Nothing in the run says so.
That is why the floor is stated as v1.5.0 rather than as the release that
introduced the call the code makes.

## `uv run`, and why the cluster image carries none of this

The entrypoint is `uv run python train.py`, not `python train.py`, and the job's
runtime environment sets `py_executable` to `uv run` to match.

The problem it solves is that a Ray cluster image cannot carry an operator's
dependencies. Operator Lib could be baked in; `torch` could not, and neither could
whatever the next operator needs. The package Ray receives already contains
`pyproject.toml` and `uv.lock`, so the environment is described in the repository
that is being trained — uv builds it there, on the head for the driver and on each
worker node for the tasks, out of a per-node cache that makes the second run cheap.
`rayproject/ray` has shipped uv since 2.45, so nothing has to be added to it.

`py_executable` is set explicitly rather than left to Ray's uv hook
(`RAY_RUNTIME_ENV_HOOK`), which detects a uv-launched driver and propagates the
executable itself. The hook works; it has also moved between releases, and the
failure when it does not fire is a worker silently starting on the cluster image's
interpreter and dying on the first import the lock file was supposed to provide.
Setting the field costs one line and removes the question.

Two consequences for a scaffolded repository, both in its README:

- **`uv.lock` is not scaffolded.** It is generated — `uv lock` — and it is the
  developer's to commit. Without it uv resolves at run time, which works, and
  weakens the claim the commit SHA makes: two runs of one commit could resolve
  different versions.
- **`requires-python` pins the minor series** (`==3.10.*`) rather than a floor.
  uv resolves driver and workers separately, and a floor lets them land on
  different minors — which Ray reports as a version mismatch between driver and
  worker, naming nothing about Python.

## The job's own credential, and what it costs when there is none

A Ray job reads its training data **directly**, never streamed through ODE
(§5.3.4). What it reads *with* changed when the run moved onto Operator Lib's
path, and the change is worth stating plainly rather than leaving to be
discovered.

`provide_historic_data` reads history one of two ways, and which one it takes is
decided by what its deployment config carries. Given a `ts_conn` it opens a direct
Postgres connection; given a `ts_wrapper_url` and a platform token it reads
through timescale-wrapper instead; given neither it refuses, naming both.

**ODE gives it a wrapper URL and no DSN.** A run executes the developer's own
Python, so a database credential in its environment is a credential handed to code
ODE did not write — `os.environ["CONFIG"]` is all it takes to read it back out,
and no gate in this repository stands between a developer and that string. The
wrapper needs no credential in the job at all: it reads with the `SENERGY_TOKEN`
described below, and timescale-wrapper checks `Execute` on the device itself. The
authorisation is the developer's own, which is what it was before the run moved
onto Operator Lib's path and what it briefly stopped being when it did
([SNRGY-4637](https://bitnify.atlassian.net/browse/SNRGY-4637)).

A deployed operator is the other way round: the flow engine sets it a DSN and
gives it no token, and its code is a reviewed image rather than a working copy.
That asymmetry is why Operator Lib keeps both paths rather than replacing one.

Two costs come with the wrapper path, both from going through the gateway rather
than the database. It reads in time-windowed pages, because the gateway answers an
oversized response with a 502 rather than relaying it — the same ceiling
[profiler-reads.md](profiler-reads.md) documents, met here at training volumes. And
it cannot shard the read across Ray workers the way `ray.data.read_sql` does with a
shard key, so it is the slower path. That is the price of the check.

A launch is separately refused if the developer may not read what its input topics
name; see
[authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md). The two
are worth having together — the launch gate refuses early and legibly, before a
build and a submit spend anything, and the wrapper is what enforces at read time.

The token below is a separate question and still worth its section: a job also
carries `SENERGY_TOKEN` for the developer's own code and for the platform helpers
in the singleuser image. An interactive access token is minutes to an hour; a
training run is hours. Handing the job the session token
means a run that dies partway through having already spent the cluster time, which
is the risk register's "token expiry vs. long Ray jobs" row.

Where a Keycloak token exchange is configured, ODE mints one token per submission
through RFC 8693, **on behalf of the developer** — the job's authorisation is
still theirs, which §3.1 step 3 requires and a service account would have violated:

```json
"keycloak_url": "https://auth.example.org",
"keycloak_realm": "senergy",
"keycloak_client_id": "ode",
"keycloak_client_secret": "…",
"job_token_audience": "timescale-wrapper"
```

`job_token_audience` is not decoration. Keycloak returns a token for the
*requesting* client unless an audience names another, and a job reads
timescale-wrapper, so without it the minted token is usually for the wrong
audience and the gateway rejects it.

Where it is not configured, ODE degrades the way the rest of ODE degrades. The
caller's token is passed, the startup warning above is logged once, and **every
launch result says so**:

```json
"credential": {
  "source": "session",
  "expires_with_session": true,
  "note": "this job carries the developer's interactive session token: a run that
           outlives the session will lose its access to the platform partway
           through. Configure keycloak_url, … (§3.1 item 6)"
}
```

That field is the point. The alternative to it is not "no limitation" — it is an
undocumented limitation discovered from a Ray log at hour two. `launch_experiment`
returns the same block to the assistant, so a model proposing an overnight run has
the sentence it needs to say first.

Two smaller decisions in the same area. A configured exchange that *refuses* does
not fail the launch — the developer could have had the run, so what they get is the
run plus a warning naming the OAuth error code. And `job_token_lifetime` is an
**expectation, not a request**: neither RFC 8693 nor Keycloak accepts a requested
lifetime, so ODE compares the configured figure against the issuer's own
`expires_in` and warns on a shortfall. A five-minute token in a deployment that
believes it has twelve hours is exactly the failure this path exists to prevent,
and it would otherwise pass silently.

No token is ever logged, stored or put in a response body. `TokenExchangeError`
carries the OAuth `error` code and never `error_description`, because that field
echoes the request's parameters back and the parameters include a token — the same
practice `pkg/repo/git.go` follows for a GitHub token in git's stderr.

## Reading a run back, and the one thing that never reaches the model

`GET /experiments/{id}/results` is §5.13's compact structured summary and nothing
else:

```json
{"run_id":"…","commit_sha":"686e01a…","status":"SUCCEEDED","finished":true,
 "params":{"folds":"5","lookback_days":"180"},
 "metrics":{"rmse":0.31,"r2":0.78},
 "comparison_to_previous":[
   {"metric":"r2","previous":0.71,"current":0.78,"delta":0.07,
    "direction":"better","lower_is_better":false},
   {"metric":"rmse","previous":0.42,"current":0.31,"delta":-0.11,
    "direction":"better","lower_is_better":true}],
 "resource_usage":{"duration_s":90},"previous_run_id":"run-1"}
```

Params, metrics and tags. **Never logs**, and that is structural rather than a
convention: logs have a route of their own, `GET /experiments/{id}/logs`, and the
interface the tool surface is given has no method that could reach them. An LLM
reading a training process's raw output is the same category of mistake as an LLM
reading a raw series (§4).

Three details in that payload are worth reading closely.

`comparison_to_previous` carries `lower_is_better` beside `direction`. Whether a
smaller number is an improvement is a property of the metric, and without the
developer's evaluation criteria ODE only has the metric's *name* to go on — loss,
error, mae, rmse, mape count down, everything else counts up. Carrying the rule
beside the verdict is what stops a naming convention from reading as a judgement.
An empty comparison means "first run", and the `note` says so in words, because an
empty list read as "no change" would be a fabricated finding (D24's rule, applied
one level up).

`evaluation_criteria` was M8's one stub: it appeared only when the run had tagged
itself with `evaluation_metric` and `evaluation_threshold`, because §5.8 denies
every tool that touches the criteria and turning the developer's own file into a
verdict was M9's work. **M9 does it** — the file is read at the run's commit and
the run is graded against it, with the run's own tags kept as the fallback for a
file that names no metric. See [result-interpretation.md](result-interpretation.md), including why `met` is
`true`, `false` *or* an object.

`status` reconciles two sources that routinely disagree. MLflow's run status is
written by the job's own code, so a job the cluster killed sits at RUNNING forever;
Ray's is the process's, so a job whose driver exited zero after failing every fold
reads SUCCEEDED. The rule: Ray decides whether the run is *over*, because only Ray
sees the process end — and MLflow's FAILED wins over Ray's SUCCEEDED, because a job
that recorded its own failure knew something the exit code did not.

## The embed probe, and why "unknown" is an answer

D6 says the Ray and MLflow UIs are probed at runtime and fall back to a link on
framing failure. `GET /experiments/embed` is the backend half:

```text
GET /experiments/embed
200 {"services":[
      {"service":"ray","url":"https://ray.example.org","embeddable":"no",
       "reason":"X-Frame-Options: DENY — the service refuses to be framed at all",
       "status":200,"probed_at":"…"},
      {"service":"mlflow","url":"https://mlflow.example.org","embeddable":"unknown",
       "reason":"ODE could not reach the service, which does not mean a browser
                 cannot: try the iframe and fall back to a link if it does not load"}],
     "cached":false,"ttl":"10m0s"}
```

It reads the two headers that decide framing: `X-Frame-Options`, and the
`frame-ancestors` directive of a `Content-Security-Policy`. CSP is checked even
when `X-Frame-Options` said nothing permissive, because `frame-ancestors` takes
precedence in the browsers that implement both. A concrete allow-list comes back
as **unknown** rather than yes or no — whether the SPA's origin is on it is a
question about the deployment, and ODE does not reliably know the origin it is
served from.

The division of labour is the design. Only a browser can find out whether a page
*actually* renders in an iframe: a service may permit framing by header and still
break inside one, or sit behind an SSO redirect that does not. So the pane still
loads a hidden iframe with a load timeout and falls back to a link-only card. What
the backend adds is the case the browser handles worst — a service answering
`X-Frame-Options: DENY` produces a blank frame and no event the SPA can catch, so
without this the pane would wait out its whole timeout on every open, and nobody
would learn which header caused it.

**"unknown" is a real answer and not a failure.** ODE is inside the cluster and the
developer's browser is not, so a service ODE cannot reach may frame perfectly. The
verdict is cached with a TTL, keyed on the configured URLs — which is D6's
"re-probe on config change" with one fewer moving part, since a changed URL simply
misses — and `?refresh=true` forces a fresh probe for the pane's re-probe control.

The SPA half is the Experiments pane at `/tools/experiments`, which renders the
verdict this probe returns: an embedded dashboard where framing allows, a link-only
card where it does not, and a re-probe control that passes `?refresh=true`.

## The last two tools

`launch_experiment` and `get_experiment_results` gain their executors here, so
**all eighteen** of §5.8's tools are implemented. The tiers are §5.8's own: both
L0, and only the launch confirmed.

L0 on both is not an oversight. Neither carries platform data — a launch ships the
developer's own committed repository to a cluster, and a result is params and
metrics their own code chose to record. The exposure tier bounds what a model
learns about the platform's *series*, and neither of these tells it anything about
one. The control on `launch_experiment` is the confirmation, exactly as it is for
`run_code`: it spends cluster time and publishes a run, and the dispatcher is what
makes sure a developer said yes before the executor is ever reached.

`get_experiment_results` called without an id answers with the developer's recent
runs rather than refusing, so "how did the last one go" is one call. Called with
one it returns the summary above — and there is no third tool, because the thing a
model might want next is the log, and §5.13 says it does not get one.