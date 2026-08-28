# A tour of the panes

What each surface does and what to look at first, in the order a developer meets
them. This was originally written as a per-milestone acceptance walkthrough; the
milestones are done, so it is organised by pane instead. The acceptance criteria
themselves are gone: see [build-order-and-risks.md](build-order-and-risks.md).

## Applies when

Trying the application, or checking by hand that a surface still behaves as
designed. Start the backend and the SPA as [README.md](../README.md) describes.

**Not this if**: the question is *why* a surface behaves as it does. Each pane's
design notes have their own document — the pointers are inline below, and the
index is in the README.

`geltung`: `einzelfall` — one build of one application.

## Profiler

*where the app's instrumentation starts, and the one pane that reads values*

Start the backend and the SPA as above and open **Under the hood → Profiler**.
The app itself lands on the workspace — the conversation beside the Code pane —
and every view below is one entry of that menu. In order:

1. **Candidates.** The list is assembled from availability, usage, liveness and
   the ontology, and the counter above it says how many *values* were read to
   build it. It has to read zero — that is the property the tier rests on, and the
   counter turns red rather than green if it ever does not. Devices the account
   cannot read data from are listed as skipped, with the reason.

   It expands **ten devices** by default. `/data-availability` is one call per
   device and cannot be batched, so that number is what decides how long a listing
   takes; the Devices box raises it when you want to search wider.
2. **Pick a row, then Full profile → Compute.** This is the first thing that
   reads values: one bounded raw pass and one aggregated pass, and the header
   reports both along with the windows and the bucket. Every variable of the
   service is profiled from those same two reads, so the siblings switch without
   another one.

   It runs over the WebSocket, so it is not bound by an HTTP timeout, and
   **Cancel** stops the platform reads rather than only the waiting.
3. **Read the non-results.** A field that could not be computed says so, with its
   reason and detail — an empty cell would be the one misreading the whole design
   is built to prevent. `not computed · insufficient coverage` on a sparse series
   is the profiler working, not failing.
4. **Confirm or correct something.** The unit is the interesting one. The profile
   body does not change; the correction lands in an append-only overlay and the
   field is marked confirmed on the next read.
5. **LLM view.** Exactly what a model would be handed, with what was collapsed
   recorded in the elided block. Put a small number in the token budget to watch
   detail get dropped in a fixed order, each drop recorded.

The exposure tier in the header gates LLM tools, not this UI — which is why the
profiler is reachable at L0. What enforces the tier is the tool dispatcher — see
[authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md).

## Selection

*a text intent resolved through the ontology, with every step auditable*

Open **Under the hood → Selection** and type what you want to model — `forecast PV
generation for this site` is the example §5.2 is written around. There is no
model involved: the words are matched against the ontology's own function, aspect
and device-class names.

What to look at, in the order the resolution happens:

1. **The terms.** Your intent as the matcher read it, green where a match used the
   word and amber where nothing did. There is no synonym table on purpose — an
   amber `pv` means the platform ontology has no such wording, which is a fact
   about the ontology rather than a matcher that needs another attempt. Deciding
   that "generation" means "production" would be ODE asserting domain knowledge it
   does not have, invisibly.
2. **The matches, with their evidence.** Each carries a score — the share of that
   ontology term's own words your intent mentioned — the words it used, and which
   label it matched on. Nothing is narrowed to one: `Power Generation` and `Power
   Consumption` both surface for "power", ranked, because only you can tell which
   you meant.
3. **The criteria sent.** One row per request, with how many device types each
   found. A row finding zero is the difference between *the ontology has no such
   thing* and *this platform has none of them* — the two failures that otherwise
   look identical from an empty list.
4. **The read counter.** Same as the Profiler's and for the same reason: selection and
   triage both complete at tier L0, so the value count has to be zero. It counts
   the selectables and device calls too, which is what a resolution actually costs.
5. **Ontology gaps.** What each device type fails to declare, discovered at
   runtime (D16), grouped by consequence — "no server-side unit conversion" and
   "cannot be found by semantic selection" are different problems with different
   fixes. This is the same judgement each candidate reports about itself, not a
   second opinion.

Untick **rank by QuickProfile** for the cheap form: the ontology resolution with no
per-device availability call at all. Series that exist in the ontology but cannot be
read — a service input, a JSONB list column — are shown and marked rather than
dropped, because the developer who searched for one needs to know it was seen.

There is no button from a resolved series into the profiler. Promoting a selection
is `propose_data_selection`, a tool on the assistant's surface that requires
developer confirmation; the Profiler view also starts from its own list.

## Chat

*the conversation, its tools and its exposure tier*

The chat surface is the LLM half: a provider abstraction, the tool allow-list of §5.8, the
exposure tiers of §3.2 enforced before any tool runs, and the admin limits of §3.3.

### Without an API key

The local `claude` CLI is the development path, and it needs no key. It reaches
ODE's tools over MCP, so `public_url` has to be something the CLI can resolve:

```bash
CLAUDE_CLI_ENABLED=true PUBLIC_URL=http://localhost:8080 ./ode -config config.json
```

Startup logs what came up:

```json
{"level":"INFO","msg":"llm surface ready","providers":["claude-cli"],
 "tools_declared":27,"tools_available_at_l0":10,"persistent":false}
```

`tools_declared` is the whole published table — §5.8's eighteen, the eight the
import surface adds, and `probe_export_data`; `tools_available_at_l0` is what a
session can actually reach at the default tier. The gap between the two is the tier doing its work.

The four tools that need your confirmation — `run_code`, `propose_data_selection`,
`propose_operator_input`, `launch_experiment` — work here as they do on an API key.
The CLI runs its own tool loop, so the call is held open on ODE's side while the
card appears in the chat pane; approving it is what runs the tool, and the answer
goes back to the model as that call's result. `chat_confirmation_timeout` bounds
how long it waits.

With an API key instead, set `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`. Several
providers may be configured at once; the first registered is the default, and a
session records which one it uses.

## Workspace

*the developer's own JupyterHub pod*

This pane needs a JupyterHub that shares this Keycloak instance and mounts a per-user
PVC. The Hub side is cluster configuration and is not described here; what
matters from ODE's side is that ODE needs a service token
holding `servers`, `tokens`, `access:servers` and `users:activity`, and refuses
to start without all four.

```json
"jupyterhub_url": "http://proxy-public.<hub-namespace>.svc.cluster.local",
"jupyterhub_token": "<the service token>",
"jupyterhub_workspace_path": "data/ode"
```

Startup says what it got, including when the credential is narrower than it
should be:

```text
WARN  jupyterhub credential: scope servers is restricted to user=devuser,
      so ODE can only serve that user
INFO  kernel surface ready  hub=… credential=ode kind=service
      kernel=python3 workspace=data/ode
```

That warning is the honest report of the development shortcut: a developer's own
Hub token holds exactly the four scopes, restricted to themselves, which is
enough to try everything below without touching the Hub's configuration.

Open **Under the hood → Kernel**. The pod is spawned on open rather than on the first
run — a cold start is up to a minute (§5.6) — and the pane says so while it
happens. Then:

```python
import os
open("marker.txt", "w").write("written in this session")
print(os.getcwd())
print("platform token bytes:", len(os.environ["SENERGY_TOKEN"]))
```

Three things in that output are the point.

`os.getcwd()` is inside the **mounted PVC**, not the pod's home. Only that
directory survives the pod being culled and respawned, so it is where the kernel
runs and it is what the Workspace pane lists. Press **Restart**, run
`print(open("marker.txt").read())`, and the file is there — the kernel that wrote
it is gone and only the volume could be carrying it.

`SENERGY_TOKEN` is the developer's own access token, pushed into the kernel by a
hidden cell at session start and again on every refresh (§5.6 item 4, because
spawn-time environment variables cannot be refreshed). Code in the pod therefore
reaches exactly the data the developer may read — not more, and not less.

And **Interrupt** stops the cell in the pod rather than only stopping the pane
watching it. A cell left running would hold the kernel against the next one.

## Exploration

*charts the assistant proposes and the developer confirms*

This pane needs nothing beyond a `timescale_wrapper_url`, and it is the first surface that
puts values in front of a human rather than in front of a detector.

Open **Under the hood → Profiler**, compute a profile, and press **Chart it**. The
exploration pane opens with that series drawn over the profile's own analysis
window, and the profile's detections on top of it: sessions as bands, gaps in
amber, advised exclusions in red, counter resets as marks. Each band says who
claimed it — `profiler`, `llm` or `developer` — because those are three different
kinds of claim and a chart that blurred them would be arguing on borrowed
authority.

Then the two things this pane rests on.

**The assistant proposes a selection and shows it.** In chat, at tier L1 or above,
ask for a chart of what it has profiled. `render_chart` answers with a chart id, the
resolved axis and nothing else:

```json
{"chart_id":"…","title":"PV generation","y_axis":{"unit":"W","unit_source":"characteristic"},
 "values_read":0,
 "note":"the specification is stored and the developer's pane draws it from their own read…"}
```

`values_read` is zero and is meant to be checked. The model wrote the
specification; the browser fetched the data with the developer's token. That is why
a tool which plainly produces a picture of data sits at **L1** rather than L2 — and
why the tool result carries no `points` array for a model to read statistics out
of, which §4 forbids.

**The developer confirms an inferred unit.** Where the ontology answered the unit
outright the pane says so and offers nothing: §5.10's point is that the ontology
reduces how often a human is asked. Where it did not — no characteristic, a unit
that travels in the message, or a characteristic whose concept contradicts the
function's — the series shows *why* and offers confirm, correct or reject. A
correction is appended to the profiler's overlay, and the axis relabels with the
resolver's own value kept beside it:

```json
{"override":{"field_path":"value_semantics.unit","computed_value":"W",
             "confirmed_value":"kW","action":"correct","created_by":"…"},
 "series":{"unit":{"unit":"kW","computed_unit":"W","confirmed":true,"confirmed_by":"…"}}}
```

It is the same overlay `POST /profiles/{id}/overrides` writes to, keyed by series
rather than by profile — so the confirmation survives a recomputation, reaches the
next profile of that series, and can be made *before* anything has been profiled at
all.

Correcting the **characteristic** rather than the unit string is the stronger move,
and the pane says so. A unit string cannot be converted; a characteristic can
(D29). Once one resolves, the series offers the conversions the ontology can reach
from it, and choosing one derives a new chart with a `convert:` transform — new,
because a specification is an immutable artifact and the original stays as it was
proposed.

## Relations

*multi-device conditional patterns from the aspect tree*

This pane needs nothing beyond a `timescale_wrapper_url` either, and it is the surface
where the ontology earns its keep twice over: once to decide *which devices* a
question is about, and once so the answer needs no second detector of its own.

Open **Under the hood → Relations** and pick an aspect — a room, a subsystem. What comes back
is device sets, and the order is a claim about how much each grouping is worth
trusting:

```json
{"aspect_name":"Kitchen","sets":[
  {"origin":"graph_siblings","name":"Kitchen circuit","devices":2,
   "graph_name":"Kitchen sub-metering","graph_node":"n-circuit",
   "rationale":"all 2 devices feed Kitchen circuit in the graph Kitchen sub-metering, so they are
                metered together rather than merely sharing a label — the topology says where they meet"},
  {"origin":"graph_flow","name":"Kitchen circuit (flow)","devices":3,
   "rationale":"the graph Kitchen sub-metering has 2 device(s) feeding Kitchen circuit and 1 measuring
                it, so this is containment rather than a shared location: one side is a sub-meter of the other"},
  {"origin":"device_group","name":"Kitchen appliances","devices":2,
   "rationale":"an existing device group on the platform, which is a stronger grouping than a shared aspect"},
  {"origin":"aspect_node","name":"Kitchen","devices":2,
   "rationale":"both devices report under the aspect Kitchen"}],
 "reads":{"aligned":0,"profiles":0,"values":0}}
```

`values` is zero and is meant to be checked, exactly as it is for a `QuickProfile`.
Nothing has been read: this is selectables, a device list, a device-group list, a
graph list and one device read per graph neighbour — all metadata. The neighbour reads
are counted under `reads.devices` rather than left uncounted, because a proposal that
quietly read a dozen devices should not look free even when none of them cost a value.

## Code

*the repository working copy, on the developer's PVC*

This pane needs two things beyond the Hub: a GitHub OAuth app, and a key to encrypt the
token it issues. Create the app under **Settings → Developer settings → OAuth
Apps** with the SPA's callback as its *Authorization callback URL* — for local
development that is `http://localhost:5173/github/callback`.

```json
"github_client_id": "<the app's client id>",
"github_client_secret": "<the app's client secret>",
"github_token_key": "<openssl rand -base64 32>",
"github_redirect_uri": "http://localhost:5173/github/callback"
```

The key is required rather than optional. §5.11 item 1 says the GitHub token is
stored encrypted, and a deployment that stored it in the clear would be a
different design rather than a convenience — so ODE refuses to start with a client
id and no key, and serves no repo routes at all when neither is set. It also needs
a `jupyterhub_url`: the working copy lives on the developer's pod, and without one
there is nowhere for it to be.

The callback is a path in the **SPA**, not a backend route, and that is deliberate:
the SPA holds a platform token and every repo route requires one, so completing the
flow from there means ODE has no unauthenticated endpoint that takes an
authorisation code and writes a credential. The Vite dev server serves `index.html`
for that path already; a deployment serving the built bundle needs the usual
single-page fallback, or GitHub's redirect lands on a 404.

```text
INFO  repo surface ready  github=https://api.github.com scopes=[repo workflow]
      redirect=http://localhost:5173/github/callback
      operator_lib=SENERGY-Platform/analytics-operator-lib-python
      operator_lib_ref=(resolved at scaffold time) persistent=true
```

Open the workspace at `/`, connect GitHub in the Code pane, and create a
repository. The order matters:

1. ODE creates an **empty** repository — `auto_init` is false — so the first commit
   in its history is the developer's.
2. It clones into the workspace on the per-user PVC, at `data/ode/{repo-name}`.
   The pane shows that path, because it is the same directory the kernel runs in:
   code in the Kernel view can `open("op.py")` and read what the editor is showing.
3. It writes the template of §5.11 item 3 into the working copy — eleven files,
   none of them committed. The pane lists them as uncommitted changes with
   **commit**, **stash** and **discard** beside them.
4. The developer commits and pushes. That is what makes the repository non-empty
   on GitHub, and what starts the build workflow.

`git` runs **in the developer's pod**, not on ODE. That is not an implementation
detail: the working copy has to be on the PVC to survive a pod being culled (§5.6),
ODE has no filesystem that could hold it, and the credential therefore has to
travel to where git is. It travels in the environment of one command —
`GIT_CONFIG_COUNT` with an `http.extraheader`, the same mechanism `actions/checkout`
uses — so it is never written into `.git/config` on the volume and never appears in
the pod's own `ps` output. `GIT_TERMINAL_PROMPT=0` goes with it, because a git that
wants a password in a pod with no terminal hangs until the command timeout instead
of failing with "authentication failed".

## Experiments

*a run submitted from a commit*

This pane needs a Ray cluster and an MLflow tracking server on top of the Hub and
GitHub app. Both are plain HTTP APIs and ODE speaks them directly, so there is
nothing to install on this side.

```json
"ray_url": "http://ray-head:8265",
"mlflow_url": "http://mlflow:5000",
"ray_dashboard_url": "https://ray.example.org",
"mlflow_ui_url": "https://mlflow.example.org"
```

The first two are the **API bases ODE calls**; the second two are what a
*browser* should open, which in a cluster is routinely a different host. Empty
falls back to the API base, which is right when the two are the same origin and
wrong in a way the embed probe will not catch, because a URL ODE can reach and a
URL the developer's browser can reach are different questions.

Setting one of `ray_url` and `mlflow_url` without the other **fails startup**
rather than half-serving. ODE creates the MLflow run before it submits the job —
that is what puts `mlflow_run_id` in Ray's metadata — so a cluster without a
tracking server cannot launch anything at all. Without both, the routes are not
served and the two experiment tools stay declared-but-unavailable, the same
degradation the profiler and the kernel do.

The surface also needs the Hub and the GitHub app. Not for tidiness: the
job package is `git archive` of the developer's working copy, and that working
copy lives on their pod.

```text
INFO  experiment surface ready  ray=http://ray-head:8265 mlflow=http://mlflow:5000
      entrypoint="python training.py" max_package_bytes=16777216
      scoped_job_token=false persistent=true
WARN  no keycloak token exchange is configured: a Ray job carries the developer's
      interactive session token, so a run that outlives the session loses its
      platform access partway through (§3.1 item 6). …
```

That warning is discussed below.

## Interpretation

*what happens when a run finishes unwatched*

Interpretation needs nothing new configured. It runs on the same Ray, MLflow and LLM
provider, and it closes a gap that was real for a while: the summary of §5.13
could be built when somebody asked for it, and this is what makes a finished run reach the
conversation it came from with nobody asking.

```text
INFO  experiment poller started        interval=30s window=6h batch=200
INFO  result interpretation started    retry_interval=30s turn_timeout=10m
INFO  result interpretation ready      poll_interval=30s poll_window=6h
      retry_interval=30s persistent_decisions=true
```

Without an LLM provider the poller is not started at all and the line says so —
experiments still launch and `/experiments/{id}/results` still answers, but nothing
interprets anything, because there is nowhere to interpret it into.
