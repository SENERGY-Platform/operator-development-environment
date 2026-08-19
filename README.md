# ODE — Operator Development Environment

Human-in-the-loop development environment for machine learning operators on the
KIEEZ / SENERGY platform. A developer goes from a problem statement to a
deployable operator, assisted by an LLM that queries the platform ontology,
interprets computed data profiles and proposes modelling approaches. The
developer defines the evaluation criteria and makes every promotion decision.

The build specification is [SPEC.md](SPEC.md). It is the source of truth; this
file only says how to run what exists.

## Status

**M0, M1a, M1b, M2, M3 and M4 of the build order in SPEC.md §6.** What works today:

- The `developer` realm role gate on every route. Token signature, expiry and
  audience are validated by the platform API gateway, not here.
- Ontology client and snapshot cache over the platform device repository:
  aspect tree, measuring and controlling functions, characteristics, concepts,
  device classes.
- Device listing and reading **on behalf of the calling user**. ODE never
  substitutes a service account for user data (SPEC D5).
- A React SPA that logs in against Keycloak and shows all of the above.
- **Timeseries client** over timescale-wrapper: availability windows, usage
  figures, and batched `POST /queries/v2` reads.
- **`QuickProfile`** — candidate series ranked from availability, usage,
  connection state and the ontology, with **no series read at all**. This is
  exposure tier L0, and the response reports its own read counts so the
  property is checkable from the answer.
- **`SeriesProfile`** — detectors 1 to 9 of §5.4.13 over a two-pass read
  strategy, provenance per field, explicit `not_computed`, an immutable profile
  store with an append-only override overlay, the LLM projection, and sessions
  as a paginated resource.

- **Semantic selection** — a text intent resolved through the ontology to
  concrete series: matched functions and aspects with the evidence behind each
  match, `device-type-selectables` per criteria combination, the devices the
  caller may read data from, `ontology_gaps` per device type, and the resulting
  series ranked by `QuickProfile`. Still no values read, so still tier L0.

- A **Selection view and a Profiler view in the SPA**: the intent resolution with
  every step auditable, then the ranked candidate list with its read counter, the
  full profile with every non-result shown as one, the paginated sessions, the LLM
  projection, and the confirmation form.

- **LLM provider abstraction** — one `Provider` interface over the Anthropic API,
  the OpenAI API, any OpenAI-compatible server, and the local `claude` CLI, all
  normalised to one event stream. A session names a provider; nothing above
  `pkg/llm` mentions a concrete one.
- **The tool surface of §5.8** — all eighteen tools declared, thirteen implemented,
  every call dispatched through one gate that enforces the **exposure tier** before
  anything executes. The four capabilities §5.8 denies have no tool at all, and a
  test asserts they never gain one.
- **An MCP server** carrying the same registry over a second transport, for the CLI
  provider. Same dispatcher, same tier gate — not a second, weaker door.
- **Chat with tool dispatch**: sessions, the agentic loop, developer-controlled
  tiers with an append-only audit trail, and held confirmations for the tools that
  need one (D11). A turn is a **detached exchange** — it outlives the connection
  that started it, so closing the tab during a five-minute profile leaves the answer
  waiting rather than losing it. Streamed over the same WebSocket as the profiler.
- **Admin limits (§3.3)** behind the `admin` realm role: per-user and global token
  and cost caps, provider and model allow-lists, a maximum exposure tier, and
  accounting recorded per provider request.
- **PostgreSQL** for the things whose loss would matter: spend accounting, the
  tier audit trail, chat history, and the profiler's override overlay.
- A **Chat view and a Settings view in the SPA**: the conversation with every tool
  call and refusal visible, the tier control beside it, and the admin surface that
  says which limits it actually enforces.

- **Execution in the developer's own JupyterHub pod** (§5.6): ODE registers as a
  Hub service, spawns the pod, mints a per-user token narrowed to that one server,
  starts a kernel in a workspace on the per-user PVC, and speaks the kernel
  WebSocket protocol directly. The developer's platform token is pushed into the
  kernel at session start and on every refresh, so code in the pod has exactly the
  developer's own authorisation and nothing more. Keep-alives hold the idle culler
  off while a session is live and stop when it is not.
- **`run_code`**, the thirteenth tool of §5.8, behind the confirmation D11
  requires. What comes back to the model is stdout, the result and the exception —
  never a figure's bytes, and never the platform token, which is redacted from
  what is persisted.
- A **Kernel view in the SPA**: the pod's state while it spawns, a console with
  streamed output and an interrupt, and the workspace listing beside it, which is
  what makes "a file written in one session is present in the next" visible rather
  than asserted.

Not built yet: M5 onward — the exploration pane, relational profiling,
repositories and experiments. There are no charts: the pane is M5, and it needs
the chart-spec surface of §5.9 to draw anything. The NetworkPolicy on singleuser
pods is M10 and is still the one hard security prerequisite before external
users — M4 makes it concrete by running developer- and LLM-authored code in
those pods, and does not close it.

## Architecture in one paragraph

A Go backend (gin) and a standalone React SPA, deployed into the same
Kubernetes cluster as JupyterHub, Ray and MLflow. The backend reuses
`device-repository/lib/client` and `models/go` rather than reimplementing a
platform client. The SPA holds a Keycloak token and presents it; every
authorisation decision belongs to the backend and, beyond it, to the platform's
own per-user permissions.

```text
pkg/auth/          claim reading, developer-role gate, on-behalf-of token
pkg/ontology/      cached snapshot facade over the device repository, the aspect
                   tree, the selectables query and the lexical intent matcher
pkg/devices/       per-user device reads, never cached across users
pkg/timeseries/    timescale-wrapper client: availability, usage, batched reads
pkg/profiler/      QuickProfile, SeriesProfile, detectors, store, projection
pkg/selection/     semantic selection: intent → criteria → selectables → devices
                   → ranked series, plus ontology_gaps
pkg/llm/           provider abstraction: one interface, one event stream, four
                   transports, plus the model price table for cost estimation
pkg/tools/         the §5.8 tool surface and the one Dispatcher that enforces
                   the exposure tier before any tool runs
pkg/chat/          sessions, the tool loop, tier changes with their audit trail,
                   held confirmations
pkg/admin/         §3.3: effective limits, the pre-request check, accounting
pkg/mcp/           the same tool registry over MCP, for the CLI provider
pkg/kernel/        JupyterHub: service registration, spawn, per-user token, the
                   kernel WebSocket protocol, workspace and keep-alive
pkg/database/      pgx pool and the schema the above persist into
pkg/identifiers/   unguessable ids for anything that appears in a URL
pkg/api/           gin routes, plus the cancellable WebSocket in ws.go and the
                   operations both surfaces share in operations.go
pkg/configuration/ config.json plus environment overrides
deploy/            the JupyterHub values and singleuser image M4 needs, applied
                   outside this repository — see deploy/README.md
```

## Running it locally

### Backend

```bash
go generate ./...          # writes docs/, which pkg/api imports; only needed once
go build -o ode .
./ode -config config.json
```

The generate step produces the OpenAPI specification from the annotations on the
handlers. It is not committed — generated artifacts do not belong in the
repository — so a fresh clone has to run it once before the first build, and
again after changing a route or an annotation. The Dockerfile and the test
workflow both run it themselves. The result is served at `/doc`, unauthenticated,
which is how the platform's developer-swagger-api collects it.

Configuration is read from `config.json` and overridden by environment
variables, using the platform's usual camel-case-to-`UPPER_SNAKE` mapping
(`device_repo_url` → `DEVICE_REPO_URL`). The VS Code **Backend** launch
configuration reads those overrides from an untracked `.env` rather than
carrying them itself, so nothing machine-specific and no credential sits in a
committed file; copy `.env.example` and fill in what your machine needs. The
settings that matter:

| Key | Meaning |
| --- | --- |
| `required_realm_role` | Defaults to `developer` |
| `device_repo_url` | Device repository base URL |
| `timescale_wrapper_url` | Timeseries base URL. Empty means the timeseries and profiler routes are not served at all |
| `ontology_cache_ttl` | How long a snapshot is served without a freshness check |
| `profiler_raw_window_days` / `profiler_raw_window_points` | Bounds on the raw pass, the smaller of the two wins (SPEC D25). 14 days or 100 000 points |
| `profiler_coverage_window_days` | Lookback the QuickProfile coverage proxy uses when a request names no window |
| `profiler_local_timezone` | Used only to flag DST transitions; computation is UTC throughout |
| `selection_max_criteria` | How many criteria combinations one resolution may send, default 12. One request each, because the platform ANDs a criteria list — see the last section |
| `selection_device_limit` / `selection_concurrency` | Devices a resolution expands (default 10) and how many selectables requests run at once (default 4) |
| `postgres_url` | Empty runs the in-memory stores. **A per-user spend cap is then only as old as the process** — see the last section |
| `anthropic_api_key` / `openai_api_key` | The central key of D8. Any subset of the four providers may be configured; with none, the chat routes are not served |
| `compatible_base_url` / `compatible_tools` | An OpenAI-compatible server (vLLM, Ollama, Azure). `compatible_tools` declares whether it implements function calling — ODE cannot find out without trying |
| `claude_cli_enabled` / `public_url` | The local `claude` CLI, for working without an API key. It reaches ODE's tools over MCP, so it needs `public_url` |
| `llm_pricing` / `llm_currency` | Per million tokens, for the estimated cost §3.3 accounts against. Not baked into the binary, because a stale price makes a cost cap quietly wrong |
| `llm_effort` / `llm_adaptive_thinking` | Anthropic's `output_config.effort`, and whether to send `thinking: {type: "adaptive"}` |
| `llm_max_tool_iterations` | How many times one exchange may loop through tools, default 12. A model that never concludes is stopped by control flow rather than by the spend cap |
| `tool_preview_max_points` | Caps a tier-L2 preview, default 500. This is what keeps "downsampled preview" from becoming a raw series read |
| `profiler_read_timeout` | Bounds one *value-reading* request, default 300s, separately from `timeseries_request_timeout` above, which bounds a metadata probe and should fail fast |
| `chat_exchange_timeout` | Ceiling on one detached turn, default 30m. It exists because an exchange no longer ends with its connection |
| `jupyterhub_url` / `jupyterhub_token` | The Hub, and ODE's **service** credential for it. Empty leaves the kernel routes unserved and `run_code` declared but not callable. A token missing any of `servers`, `tokens`, `access:servers`, `users:activity` fails startup — see `deploy/jupyterhub/README.md` |
| `jupyterhub_workspace_path` | The kernel's working directory, default `data/ode`. **It has to be inside the mounted PVC** or nothing a developer writes survives the pod |
| `jupyterhub_profile` | The KubeSpawner profile to spawn with. Empty takes the deployment default, which is the plain notebook image — set it once the ODE image of §5.6 item 1 exists |
| `jupyterhub_keepalive_interval` / `jupyterhub_idle_timeout` | How often an active session reports activity (default 5m, must stay below the cluster's cull timeout) and when ODE lets go of a session it has not heard from (default 2h) |
| `jupyterhub_execute_timeout` / `jupyterhub_max_output_bytes` | Ceiling on one cell (default 10m, then it is interrupted rather than abandoned) and on what one cell may stream (default 1 MiB) |
| `tool_run_code_max_output_bytes` | Far smaller, default 8000: what a cell's output costs in *model context*, as opposed to memory |
| `cors_origins` | Only needed if the SPA is not served through the Vite proxy |

## Trying M1

Start the backend and the SPA as above and open the **Profiler** tab, which is
where the app lands. In order:

1. **Candidates.** The list is assembled from availability, usage, liveness and
   the ontology, and the counter above it says how many *values* were read to
   build it. It has to read zero — that is M1a's acceptance criterion, and the
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
profiler is reachable at L0. Enforcing it is M3.

## Trying M2

Open the **Selection** tab and type what you want to model — `forecast PV
generation for this site` is the example SPEC §5.2 is written around. There is no
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
4. **The read counter.** Same as M1a's and for the same reason: selection and
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
is `propose_data_selection`, which requires developer confirmation and arrives with
the tool surface in M3; until then, the Profiler tab starts from its own list.

### Semantic selection over HTTP

```text
POST /selection
{
  "intent": "forecast PV generation for this site",
  "function_ids": [], "aspect_ids": [], "device_class_ids": [],
  "interaction": "event",          // or request, event+request, any
  "include_controlling": false,
  "match_limit": 5, "min_score": 0.5,
  "limit": 10,                     // devices to expand
  "window": {"from": "…", "to": "…"},
  "rank": true
}
```

`intent` or one of the three id lists is required — with nothing to resolve there
are no criteria, and an empty criteria list matches *every* device type on the
platform, so it is refused rather than sent. The id lists bypass the matcher
entirely, which is how the LLM will call this in M3 once it has read the ontology
itself; an id the ontology snapshot does not know is reported and queried anyway,
because the snapshot can be older than the platform.

The route is served even without a `timescale_wrapper_url`: the resolution is
ontology work. Only the ranking needs the profiler, and the response says so in
`notes` instead of failing. The SPA's Selection tab does need the WebSocket, which
is registered with the profiler.

## Trying M3

M3 is the LLM surface: a provider abstraction, the tool allow-list of §5.8, the
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
 "tools_declared":18,"tools_available_at_l0":8,"persistent":false}
```

`tools_declared` is §5.8's whole table; `tools_available_at_l0` is what a session
can actually reach at the default tier. The gap between the two is the point of the
milestone.

With an API key instead, set `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`. Several
providers may be configured at once; the first registered is the default, and a
session records which one it uses.

### The tool table, and what has no tool

```bash
curl -s -H "Authorization: Bearer $TOKEN" $BASE/llm/tools | jq '{
  at_l0: (.tiers[] | select(.tier=="L0") | .available),
  denied: (.denied | keys)
}'
```

`denied` is §5.8's "no tool exists" list — changing the exposure tier, changing
admin limits, writing a `ProfileOverride`, promoting a recommendation. They are
absent from the registry rather than refused at dispatch, and `NewRegistry` refuses
to register one. A refusal would still advertise the capability and invite the model
to argue with it.

### Watching the tier block a tool

Create a session — it starts at L0, which §3.2 makes the default rather than a
choice a caller has to remember:

```bash
SID=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{}' $BASE/chat/sessions | jq -r .id)
```

Sending a message is a WebSocket operation, so this one needs a WebSocket client
rather than curl — `websocat` will do:

```bash
websocat -H="Authorization: Bearer $TOKEN" ws://localhost:8080/ws <<EOF
{"type":"chat_send","id":"r1","payload":{"session_id":"$SID",
 "message":"show me the actual values for any power series you can find"}}
EOF
```

Frames come back as `accepted`, then one `event` per item of the stream, then `done`.
At L0 the assistant can resolve the intent and rank candidates,
and `preview_series` is not offered to it at all; if it asks anyway, the dispatcher
answers with §3.2's refusal verbatim:

```json
{"blocked_by_tier":"L0","required":"L2","tool":"preview_series",
 "hint":"the developer controls this. Ask them to raise the exposure tier to L2 …"}
```

Two more messages are worth knowing by hand. `chat_attach` subscribes to a turn
already in flight and replays it from the start, which is what a reconnect does:

```json
{"type":"chat_attach","id":"r2","payload":{"session_id":"<SID>"}}
```

`chat_cancel` abandons the turn. Closing the socket does **not** — that only detaches
your view, and the exchange runs on:

```json
{"type":"chat_cancel","id":"r3","payload":{"session_id":"<SID>"}}
```

Worth trying, because it is the behaviour the design turns on: start a message, close
the socket while a tool is running, reconnect, `chat_attach`, and the turn is still
there. `GET /chat/sessions/$SID` shows the messages persisted either way.

Raise the tier, and the same request works:

```bash
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"exposure_tier":"L2"}' $BASE/chat/sessions/$SID/tier

# Every change is logged with its time and its user (§3.2).
curl -s -H "Authorization: Bearer $TOKEN" $BASE/chat/sessions/$SID/tier-changes | jq .
```

### The same gate over MCP

The MCP transport is the CLI provider's route to the tools, and it enforces the
same gate through the same dispatcher. Worth checking by hand, because a second
transport is exactly where a bypass would hide:

```bash
# The session id is a header, because the tier lives on the session.
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-ODE-Session: $SID" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",
       "capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}' $BASE/mcp

curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-ODE-Session: $SID" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' $BASE/mcp
```

The advertised list is the session's tier, read from the session on every request —
never from a header a client could set for itself.

### Admin limits

Behind the `admin` realm role. A cap is checked before each provider request, and
answered with a structured refusal rather than a generic error:

```bash
curl -s -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"period":"24h","token_cap":50}' $BASE/admin/limits/$SUB

# Then, once 50 tokens are spent, the next message is refused:
# 429 {"error":"limit_exceeded","scope":"user","kind":"tokens","cap":50,
#      "spent":43306,"period":"24h0m0s","resets_at":"…"}
```

Two things the settings surface tells you that a plain form would hide. Which
fields this build actually enforces — the Ray cap of §3.3 is stored now and
enforced from M7, and the kernel resource caps are stored and **never** enforced
by ODE, because a pod's resources are KubeSpawner's and the Hub's spawn API
selects a profile rather than carrying overrides. They belong in
`values-ode-singleuser.yaml` in `rancher-2-defs`, and an administrator setting
one here should know it does nothing.
And whether a cost cap can bind at all: cost is estimated from `llm_pricing`, so a
model with no configured price accrues zero and the cap silently does not apply to
it. `GET /admin/usage` marks those requests `unpriced` rather than showing them as
free.

## Trying M4

M4 needs a JupyterHub that shares this Keycloak instance and mounts a per-user
PVC. [deploy/jupyterhub/README.md](deploy/jupyterhub/README.md) says what has to
be configured there and why the service token cannot come from the chart's own
`hub.services` mechanism; the short version is that ODE needs a service token
holding `servers`, `tokens`, `access:servers` and `users:activity`, and refuses
to start without all four.

```json
"jupyterhub_url": "http://proxy-public.jupyterhub.svc.cluster.local",
"jupyterhub_token": "<the service token>",
"jupyterhub_workspace_path": "data/ode"
```

Startup says what it got, including when the credential is narrower than it
should be:

```text
WARN  jupyterhub credential: scope servers is restricted to user=jonah,
      so ODE can only serve that user
INFO  kernel surface ready  hub=… credential=ode kind=service
      kernel=python3 workspace=data/ode
```

That warning is the honest report of the development shortcut: a developer's own
Hub token holds exactly the four scopes, restricted to themselves, which is
enough to try everything below without touching the Hub's configuration.

Open the **Kernel** tab. The pod is spawned on open rather than on the first
run — a cold start is up to a minute (§5.6) — and the pane says so while it
happens. Then:

```python
import os
open("marker.txt", "w").write("written in this session")
print(os.getcwd())
print("platform token bytes:", len(os.environ["SENERGY_TOKEN"]))
```

Three things in that output are the milestone.

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

### The kernel over HTTP and the WebSocket

Executing streams, so it lives on `GET /ws` beside the profiler and chat
operations. Everything that answers once is a route. None of them takes a user
parameter: the pod is resolved from the caller's own token, which is the only
thing stopping ODE's Hub credential from reaching anyone else's.

| Route | Effect |
| --- | --- |
| `GET /kernel` | What is running. Starts nothing, so it is safe to poll |
| `POST /kernel` | Spawn, start a kernel in the workspace, install the platform token. Idempotent |
| `POST /kernel/restart` | A fresh kernel in the same pod. The workspace is untouched |
| `POST /kernel/interrupt` | Stop the running cell, keeping the kernel's state |
| `DELETE /kernel` | End the kernel. **Not** the pod: that is the developer's, and their files are on it |
| `GET /kernel/files?path=` | The workspace listing. Read-only — the full tree with write access is §5.11, in M7 |

Over the socket, `{"type":"kernel_execute","id":"…","payload":{"code":"…"}}`
streams one `event` per thing the cell produced and ends with `done`. Sending
`{"type":"cancel","id":"…"}` interrupts it — and the events keep coming until the
final one says `interrupted`, because a pane that simply goes quiet reads like a
lost connection rather than a stopped cell.

That is the opposite of the chat rule two sections down, deliberately. Detaching
from a chat turn leaves it running, because an answer nobody is watching is still
worth having; detaching from a cell stops it, because a training loop nobody is
watching is only costing the developer their own pod.

### run_code, and what the tier does not cover

`run_code` is tier **L0** with a confirmation, which is what §5.8's table says
and is worth understanding rather than reading past. Confirmed code runs with the
developer's own token, so it can reach values `preview_series` would refuse at
L0. The control is the developer's confirmation, not the tier — the same control
D11 puts on every other consequential action — and it is why every execution is
a decision rather than a default.

What ODE does do is keep the accidents out of the record: the platform token is
redacted from what `run_code` returns, so a `print(os.environ)` while debugging
does not put a live credential into a conversation that is persisted to Postgres.
That is hygiene, not a boundary. Code that deliberately encodes the token defeats
it, and nothing here pretends otherwise.

## The profiler over a WebSocket

The Profiler and Selection views run their slow operations over `GET /ws` rather
than the HTTP routes, because a read outlives an HTTP request: a raw pass bounded at
100 000 points is megabytes of JSON per column, and an ingress idle timeout turns
that into a 504 — with the backend still reading for a client that has gone. A
resolution is on the socket for the milder version of the same reason: it expands
devices, availability is one call per device, and a developer changing their mind
should stop those reads rather than leave them running.

Every request carries an `id`. The client can cancel one, and closing the socket
cancels everything that connection was doing, which is what stops a closed browser
tab from costing platform reads.

```text
→ {"type":"quick_profiles","id":"q1","payload":{"limit":10,"search":"","window":{…}}}
→ {"type":"profile","id":"p1","payload":{"device_id":"…","service_id":"…"}}
→ {"type":"resolve_selection","id":"s1","payload":{"intent":"…","limit":10}}
→ {"type":"cancel","id":"p1"}
→ {"type":"ping","id":"…"}
→ {"type":"auth","id":"a1","payload":{"token":"<refreshed access token>"}}

← {"type":"accepted","id":"p1"}
← {"type":"result","id":"p1","payload":{…}}
← {"type":"error","id":"p1","error":"…","status":400}
← {"type":"cancelled","id":"p1"}
← {"type":"pong","id":"…"}
```

The payloads and results are the same documents the HTTP routes below return —
both surfaces call the same functions in `pkg/api/operations.go`, because two code
paths would drift and the one nobody demos is the one that rots.

**Authentication.** A browser cannot set an `Authorization` header on a WebSocket
handshake, so the token travels as the subprotocol `ode.bearer.token.<token>`.
An `Authorization` header and an `?access_token=` parameter also work, for clients
that are not browsers; the query form is supported but avoided by the SPA because
it ends up in access logs. The realm role is enforced on the upgrade, so §3.1 is
unchanged: the gateway validates, ODE authorises.

**And it has to be re-presented.** The handshake happens once; the access token
expires. `auth` replaces the credential this connection presents to the platform,
and the SPA sends it whenever a refresh produced a new token — which is what keeps
a tab open for an hour from reading with an expired one. The subject must be
unchanged and the realm role must still be present, or the frame is refused with
403 and the connection keeps the credential it had: `sub` is the only thing tying a
connection's chat sessions and its spend against the §3.3 cap to the token its
reads are made with. Expiry is not checked here, exactly as it is not checked on
the upgrade — that is the gateway's, and `servicejwt.Token` does not carry `exp`.

A cancelled operation answers `cancelled`, never `error`. An aborted read fails on
the way out, and reporting that as a platform fault would be a lie.

## The profiler over HTTP

The same operations as request/response, for scripting and for the contract
fixtures. All of them sit behind the `developer` role gate.

| Route | Effect |
| --- | --- |
| `GET /timeseries/availability?device_id=` | Per-service availability window and materialised aggregates |
| `GET /timeseries/usage?device_ids=a,b` | Bytes and bytes per day, for cost estimation at tier L0 |
| `GET /quick-profiles` | Candidate series ranked from metadata alone. `limit` is **devices**, default 10; plus `search`, `from`, `to`, `include_unqueryable`. Reports its own read counts, and `reads.values` is always 0 |
| `POST /selection` | Semantic selection (§5.2), documented above. Also `reads.values` 0 |
| `POST /profiles` | Computes a full profile per variable of one service. Body: `device_id`, `service_id`, optional `analysis_window`, `raw_window`, `group_time`, `session_params` |
| `GET /profiles/{id}` | The stored profile with its override overlay resolved |
| `GET /profiles/{id}/projection?token_budget=` | The one model-facing view: arrays collapsed, provenance dropped, elisions recorded |
| `GET /profiles/{id}/sessions?from&to&limit&cursor` | The paginated session resource |
| `POST /profiles/{id}/overrides` | Appends a developer confirmation. Body: `field_path`, `action`, `computed_value`, `confirmed_value`, `note` |

The override route is a **developer** action. SPEC §5.8 lists writing a
`ProfileOverride` among the operations with no LLM tool at all, and that has to
stay true when the tool surface lands in M3: a model that can confirm its own
inferred unit has confirmed nothing.

### Frontend

```bash
cd frontend
npm install
npm run dev
```

Vite serves on `:5173` and proxies `/api` to the backend on `:8080`. Copy
`frontend/.env.example` to `.env.local` and adjust.

Two Keycloak details cost time if you get them wrong, so they are worth stating
plainly:

- **The base URL needs the `/auth` suffix** — `https://auth.senergy.infai.org/auth`.
  This deployment serves the legacy base path, while keycloak-js 17 and later
  default to the path without it. Omit it and the login redirect 404s, which
  looks like a missing realm rather than a missing prefix. Confirm with
  `curl -s <url>/realms/master/.well-known/openid-configuration | jq .issuer`.
- **The client must be public, with PKCE, and must list the dev origin** under
  Valid Redirect URIs (`http://localhost:5173/*`). Otherwise Keycloak answers
  `Invalid parameter: redirect_uri`. That message means the client exists but
  the origin is not registered; an unknown client id says `Client not found`
  instead, which is how to tell the two apart.

## Tests

```bash
go generate ./...            # once, if docs/ is absent; pkg/api imports it
go test -race ./...          # backend
cd frontend && npm run build # frontend: type-check and bundle
```

The frontend build is also a **contract test**. `frontend/src/__contract__` holds
JSON captured from a running backend, assigned to the types the SPA declares for
those endpoints, so a renamed or dropped field fails the build instead of
becoming `undefined` in front of a developer. It earned its place on the first
run by catching four defects, and caught a fifth on the M3 pass — a `duration_ms`
field carrying nanoseconds. See the README in that directory, including how to
recapture the fixtures when the shape changes on purpose.

The M3 and M4 fixtures are regenerated from the API test harness rather than a
platform:

```bash
ODE_WRITE_CONTRACT=$PWD/frontend/src/__contract__ \
  go test ./pkg/api/ -run ContractFixtures
```

The backend tests use no containers and no network. The device repository is a
fake and test tokens are minted unsigned — deliberately, since signatures are
the gateway's concern — so the suite runs in a few seconds without the platform.
The M3 packages hold to the same rule: the LLM providers are exercised through a
scripted fake that returns a fixed event script, so a whole tool loop runs with no
API key, and the Postgres stores are only wired in `pkg.Start` — the tests run the
memory implementations of the same interfaces.

The MCP tests are the exception worth knowing about: they run a real MCP client
against a real server over `httptest`, because the property being checked is that a
second transport enforces the same gate, and a fake client would only prove the
server agrees with itself.

M4 has the same shape and one addition. `pkg/kernel/kerneltest` is a JupyterHub
and one singleuser server in memory, speaking the real protocol rather than a
simplification of it — a spawn that is pending before it is ready, a scoped
token, an execute that produces busy / input / stream / reply / idle in that
order — because everything worth testing in `pkg/kernel` depends on that
ordering. It is a package rather than a test file because `pkg/api` needs it too,
and two copies of a protocol double would drift.

The addition is a handful of tests that do need a cluster, skipped unless it is
there:

```bash
ODE_JUPYTERHUB_URL=http://proxy-public.jupyterhub.svc.cluster.local \
ODE_JUPYTERHUB_TOKEN=... ODE_JUPYTERHUB_USER=... \
  go test ./pkg/kernel/ -run Live -v
```

They exist because the parts of §5.6 most likely to be wrong are the ones a fake
cannot check: whether the credential really holds the scopes, whether the
WebSocket handshake races the kernel's channel bridge, and above all whether a
file written in one session is present in the next. The last one restarts the
kernel and reads the file back, which is the acceptance criterion itself. A
mistyped Hub field — `progress_url` as an integer — was caught on their first
run and would have passed every unit test in the package.

Detector correctness is checked against fixtures with known answers rather than
against the platform (SPEC §5.4.14): a synthesised 15-minute series with an
injected gap, a monotonic counter with two resets, a bimodal washing-machine
load, white noise against a random walk. That is what makes the profiler testable
without an LLM and without the cluster.

## Things worth knowing before you extend this

**ODE does not validate tokens, and must therefore sit behind the gateway.**
Signature, expiry and audience are checked centrally by the platform API
gateway; `pkg/auth` parses claims unverified to read `sub` and `realm_access`.
That is correct for gateway traffic and unsound for anything else — a
cluster-internal caller reaching ODE's service DNS directly is authenticated by
nothing. Since JupyterHub singleuser pods run developer and LLM-authored Python
in the same cluster, the M10 NetworkPolicy is what makes the assumption hold.
Do not expose ODE before it.

**Read permission is not data permission.** `models.Read` governs device
metadata; `models.Execute` governs reading a device's *data*. `/devices` still
lists under `Read`, because it serves metadata. Everything that reads or offers
to read a series — `/quick-profiles`, `POST /profiles` — is scoped to `Execute`,
because timescale-wrapper checks `Execute` itself and would otherwise refuse the
read after the developer had already chosen the series. `POST /selection` is scoped
to `Execute` for the same reason: it offers series to read.

**Never null, never absent.** Every computable profile field is either a value or
an explicit `{"status": "not_computed", "reason": ..., "detail": ...}` (SPEC
D24). `profiler.Value[T]` makes that structural rather than a convention each
detector has to remember, and its zero value marshals as `not_computed` — so a
detector that fails to run cannot report a silent zero. Absence and negation must
stay distinguishable: an LLM that reads a missing `dominant_periods_s` as "no
periodicity" will propose a model on that basis, and nothing downstream can
recover the difference.

**A token budget per item is not a bound on a response.** `tool_profile_token_budget`
bounds one `LLMProfileView`, and for a while it was the only bound the tool surface
had. Breadth then multiplied it: `quick_profile` over three inverters assembled
eighty candidates and about 48k tokens, two thirds of that the provenance sidecar
and the same `not_computed` sentence repeated once per candidate, and
`profile_series` returns one profile per variable of a service. A tool result is
resent on every iteration of the tool loop, so the cost lands on the whole turn
rather than on the call. `profiler.ProjectQuick` is the L0 counterpart of
`profiler.Project`: it drops what only ODE itself needs, spends
`tool_quick_token_budget` a device at a time — a fleet of one device type ties on
every ranking input, so a ranked prefix would answer about three inverters with one
inverter's variables — and records what it cut in `elided` and `elided_devices`,
devices by name. `tool_profile_max_profiles` bounds the other list, and
`variable_paths` is how a caller asks for a profile the cap left out. The HTTP and
WebSocket surfaces are unprojected on purpose: the frontend renders every field.
Add a tool that returns a list, and bound the list.

**A connection outlives its token, and a chat turn can too.** The WebSocket
handshake authenticates once and the socket then lives as long as the tab, while
the SPA's access token is refreshed on a thirty-second horizon. The connection
therefore holds a `sessionToken` that an `auth` frame replaces, and every operation
reads it at the moment it runs rather than copying it per connection. The chat
engine goes one step further: `chat.TokenSource` is read per *tool call*, because an
exchange is detached from the request that started it — a turn running twelve tool
iterations, or a confirmation a developer approves ten minutes later, would
otherwise dispatch with a credential that expired while it waited. Both failures
looked like platform faults and both disappeared on reload, which is the worst kind
of bug report to receive.

**A selectables criteria list is an AND, and an empty one matches everything.**
`POST /v2/query/device-type-selectables` narrows the device type set with each
criterion in turn, so `[{function: power}, {function: energy}]` asks for a device
type carrying *both* — for two unrelated functions, none at all. An intent means
the opposite: any matched function, in any matched aspect. `pkg/selection` therefore
sends **one criterion per request** and unions the answers, which is why a
resolution's request count is functions × aspects and why `selection_max_criteria`
exists. Two corollaries are just as easy to get wrong: an aspect criterion already
covers the node's whole subtree, so passing descendants as extra criteria ANDs a
parent with its child and matches nothing; and an *empty* criteria list is
substituted upstream with one empty criterion that matches every device type on the
platform, which is why `ontology.DeviceTypeSelectables` refuses it outright. None of
this is a compile error and all of it looks like an empty platform.

**Computed profiles are in memory; the override overlay is not.** The two halves of
`profiler.Store` have different durability requirements and now have different
homes. A computed profile is reproducible — losing it costs a recomputation — so it
stays in `MemoryStore`. An override is a developer's confirmation of derived
semantics, which §5.4.3 calls an empirical record, so with a `postgres_url` it goes
to a table (`profiler.NewOverlayStore` composes the two). Without one, both are in
memory and the warning at startup says so.

**Every tool call goes through one `Dispatch`, and that is the whole tier
argument.** `pkg/tools` is written so there is no path to an executor that skips a
check: the executor lives in an unexported field, `Dispatch` is the only caller, and
the order — exists, implemented, tier, confirmation, run — is the milestone's exit
criterion rather than an implementation detail. The MCP transport shares the same
dispatcher for the same reason; a second tool list would be a second, weaker gate.
If you add a tool, add it to the registry, not beside it.

**A denied capability has no tool, and that is different from refusing one.** The
four operations §5.8 forbids are absent from the registry, `NewRegistry` refuses to
register one, and a call to a denied name is answered "unknown tool" rather than
"forbidden". Naming it forbidden would describe a capability boundary the model will
then try to talk its way around; "no such tool" ends the line of enquiry. There is a
test asserting all of this, because absence is the kind of property that quietly
stops holding.

**The admin tier ceiling binds continuously, not only when raising.** An earlier
version checked `max_tier` only on the way up, so a session already at L2 kept its
L2 tools after an administrator lowered the maximum — a policy that applied to
future sessions only. `chat.Engine.effectiveTier` now clamps on every read of a
session's tier, including the MCP path, and fails closed to L0 if the policy cannot
be read. The stored tier and the effective tier can therefore differ, which is why
`SetTier` compares against the stored one.

**`time.Duration` marshals as nanoseconds.** A field named `duration_ms` carrying a
`time.Duration` is wrong by a factor of a million and nothing about the JSON says
so — both are plausible integers. `tools.Millis` exists to make the name true. The
frontend's contract check is what caught it; that is the second time that check has
paid for itself.

**A chat exchange is detached from the connection that started it, and this is the
load-bearing decision in the whole surface.** `chat.Exchange` is a turn running on
the process's own context with its own ceiling (`chat_exchange_timeout`), publishing
to zero or more subscribers. A connection is a *view*, not the owner.

It is worth knowing why it is at the level of the whole exchange rather than of
individual slow tools. A tool result is only useful inside a conversation: if
`profile_series` ran as a background job but the exchange died with the socket, the
profile would complete into a cache nobody was reading and the conversation would
have lost the turn. Detaching the exchange makes every tool inside it survive for
free, and leaves nothing for a per-tool job registry to add.

Three consequences:

- **Closing a tab is not cancelling.** Detaching a view leaves the turn running;
  `chat_cancel` abandons it. The two are separate messages because they are separate
  intentions, and the Stop button sends both.
- **Reconnecting resumes.** `chat_attach` subscribes to whatever is still in flight,
  and `Subscribe` replays from the start of the turn — so a reattached view sees the
  whole thing, not just the remainder. The SPA attaches on every socket open.
- **A slow subscriber is dropped, not waited for.** If a client stops draining, the
  exchange closes that subscriber rather than stalling the work; the client re-reads
  the persisted messages, which are the source of truth in any case.

**Streaming is all on `/ws`, and this departs from §5.7 deliberately.** The spec says
the provider stream is "Streamed to the SPA over SSE", and the first implementation
did that. It was wrong in practice for the reason `ws.go` was written: between the
`tool_call` event and its `tool_result` nothing is written, and a 3-second tool
produced 3.001s of measured silence — so any proxy idle timeout closed a healthy
connection mid-exchange.

SSE can be kept alive with a comment heartbeat, and that is a real fix; the reason it
is not the one here is that it would leave ODE maintaining two streaming paths with
two sets of liveness and cancellation semantics, when the WebSocket already had the
harder half working. Note the mechanism either way: **the WebSocket survives idle
because it pings every 30s ([ws.go:242](pkg/api/ws.go#L242)), not because it is a
WebSocket.**

Request/response stays REST — sessions, the tier, the audit, admin, MCP — because a
status code means something there and those routes are worth being able to curl.

**Two platform timeouts, because the two kinds of request are not comparable.**
`timeseries_request_timeout` (60s) bounds a metadata probe: availability, usage. It
should fail fast. `profiler_read_timeout` (300s) bounds a value read, where the
server assembles megabytes of JSON for a raw pass of up to a hundred thousand
points. One shared timeout means either the probe waits far too long to fail or the
read is cut off mid-assembly. The client applies whichever it is given as a context
deadline and carries no `http.Client.Timeout` of its own — that field is an absolute
cap and would silently win over the longer one.

**Spend accounting is per provider request, not per exchange.** §3.3 says "recorded
per request", and it has to be: the tool loop makes several provider calls, and
recording only the aggregate at the end would let one exchange overrun a cap by its
whole length — bounded by nothing but `llm_max_tool_iterations`. The cap is checked
before each call and against spend already recorded, so it can be overshot by at
most one request. Refusing on a prediction instead would refuse requests that would
have fit.

**Two details of §5.6 are not what the spec assumes, because the deployment is
not.** Both were found by reading the running Hub rather than the chart, and both
are configuration rather than code.

- **The Hub username is `preferred_username`, not `sub`.** §5.6 says the Hub
  username "derives from the same Keycloak subject", which is true of the identity
  and not of the string: the deployed `GenericOAuthenticator` produces `jonah`,
  and a spawn addressed to a UUID would 404 for every developer.
  `jupyterhub_username_claim` picks between the two. No mapping table either way,
  which was the actual point being made.
- **The PVC is mounted at `~/data`, not over the home directory.** §5.11 suggests
  `~/ode/{repo}`; here that path is on the pod's ephemeral filesystem and is gone
  the first time the culler runs. `jupyterhub_workspace_path` defaults to
  `data/ode` for that reason, and the Workspace pane names the path on screen
  because a developer has no other way to tell which of their files will survive.

**ODE spawns pods and never stops them.** Shutting down a kernel leaves the pod
running, and the reaper that drops an idle session closes ODE's socket and stops
its keep-alives rather than deleting anything. Both are the same judgement: the
pod is the developer's, their files and their running processes are on it, and a
respawn costs them a cold start. Reclaiming it is the cluster's idle culling —
which ODE stops holding off precisely so that it can.

**The kernel connection is persistent per developer, and that is not only an
optimisation.** `jupyter_server` bridges the kernel's ZeroMQ sockets onto the
WebSocket when the connection opens, and a request sent before that bridge exists
loses its early `iopub` messages — the busy status, sometimes the first lines of
output. Paying a `kernel_info` handshake once on connect closes the race for
every cell after it. Reconnecting per cell would reopen it every time, which is
the kind of bug that reproduces on a loaded cluster and nowhere else.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
