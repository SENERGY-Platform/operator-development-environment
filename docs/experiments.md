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

`geltung`: `allgemein` for the commit rule and the credential model, which follow
from §5.12 and §3.1 item 6; the embed probe's behaviour is `einzelfall`.

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
`kernelEnvironment` in `pkg/pkg.go`, and it is the whole list: no
`MLFLOW_TRACKING_URI`, no `RAY_ADDRESS`. The MLflow trio is set at submission
time only, and it could not be earlier — ODE creates the run before the job
exists, so `MLFLOW_RUN_ID` has no value to carry until then. Those same three
names sit in `reservedEnv`, so a caller's `env_vars` cannot supply them either.

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

The second gap is where the two MLflow uses part company, and they are genuinely
two. Operator Lib's is the **registry** path in production: `MLOperator` loads the
model registered under this pipeline and operator, and `train()` — handed a
`TrainMlflowLogger` by the library itself — registers the next one. It is keyed by
the deployment. ODE's is **tracking for comparison**, keyed by the commit. Both
are MLflow; they answer different questions, and neither substitutes for the
other.

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
   comparison rests on knowing which code produced which number.

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

## ODE creates the MLflow run, not the job

The order matters and it is the reason "run tagged with commit SHA" holds:

1. the working copy is checked and the launch refused if it is dirty;
2. the package is built from that commit and uploaded;
3. the per-user experiment is found or created (D17);
4. **the run is created, with its tags in the same request**;
5. only then is the job submitted, with the run id already in its metadata.

Step 4 is a `runs/create` carrying `commit_sha`, `session_id`, `user_sub`,
`ode_experiment_id`, the repository, the branch, the entrypoint and the Ray
submission id — not a create followed by five `set-tag` calls. The difference is
that there is no window in which the run exists without the tag the whole claim
rests on; a crash between two round trips would otherwise leave a run permanently
unreproducible and looking fine.

It is also what lets `mlflow_run_id` be one of the four metadata keys §5.12 names
at submission time. A job that minted its own run could not be, and the tag would
then depend on the developer's own code remembering to write it.

The experiment name is deterministic per developer and repository (D17):
`ode/{hub username}/{owner}-{repo}`. The username is used because that string is
what a human reads in MLflow's own UI, and the Keycloak subject — which is what
actually identifies the developer and cannot change — is an experiment tag, so the
mapping survives a rename.

Steps 2 to 5 also mean a failed submission leaves a created run behind. It is
marked failed rather than deleted: a run that existed and failed to launch is a
fact about the developer's day, and deleting a run is theirs to do.

## The job's own credential, and what it costs when there is none

A Ray job reads its training data from timescale-wrapper **directly** with its own
token (§5.3.4) — never streamed through ODE. An interactive access token is
minutes to an hour; a training run is hours. Handing the job the session token
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