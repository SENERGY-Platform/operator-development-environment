# Configuration

Every setting `config.json` accepts, what it decides, and which surface goes
unserved when it is empty. ODE is built to come up one dependency at a time: an
absent URL leaves a surface undeclared and says so at startup rather than failing.

## Applies when

Bringing ODE up, or working out why a route answers 404 or a tool reports itself
unavailable. Values are read from `config.json` and overridden from the
environment with the platform's usual camel-case-to-`UPPER_SNAKE` mapping
(`device_repo_url` → `DEVICE_REPO_URL`).

**Not this if**: the question is what the *import* services do or why
`device_selection_url` is used for imports and not for devices — see
[imports-as-operator-inputs.md](imports-as-operator-inputs.md), which owns that
decision and its degradation ladder.

`geltung`: `einzelfall` — the defaults and the warnings are this codebase's.

## Where the values come from

Editor run configurations are not committed — `.vscode/` is ignored, because a
launch configuration carries paths, hosts and env choices belonging to one
machine. Point yours at an untracked `.env` rather than carrying the values
itself, so nothing machine-specific and no credential sits in a committed file.
Copy `.env.example` and fill in what your machine needs; an empty value there is
deliberate rather than a placeholder to be guessed at.

Three things about the tables below.

**A key absent from `config.json` still has a value.** `applyDefaults` in
`pkg/configuration/config.go` fills it, so the **Default** column is what ODE
uses when nothing says otherwise — not what the checked-in `config.json` happens
to contain. Where the two differ, the row says so.

**`config.json` carries its own reasoning** in `//`-prefixed keys, per group.
Those explain the trade a figure makes; this file is the index that says a key
exists at all, which a comment attached to a group cannot do.

**A URL is a placeholder here and stays one.** The platform base URLs in
`config.json` are written `https://<platform-gateway>/…`: a reachable endpoint
is deployment data, so the host comes from the environment while the path — the
platform's own API shape — is kept.

## Process and identity

| Key | Default | What it decides |
| --- | --- | --- |
| `api_port` | `8080` | The port. Required: startup fails without it |
| `debug` | `false` | Raises the structured logger to debug level. Nothing else |
| `required_realm_role` | `developer` | The realm role a token must carry. Empty warns and lets **every** authenticated platform user in — see [authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md) |
| `cors_origins` | none | Only needed if the SPA is not served through the Vite dev proxy |

## Platform reads

| Key | Default | What it decides |
| --- | --- | --- |
| `device_repo_url` | none | Device repository base URL. Required: startup fails without it |
| `timescale_wrapper_url` | none | Timeseries base URL. Empty means the timeseries and profiler routes are not served at all |
| `ontology_cache_ttl` | `1h` | How long a snapshot is served without a freshness check |
| `ontology_invalidate_interval` | `5m` | How often the cached snapshot is checked for invalidation |
| `timeseries_request_timeout` | `60s` (this repo's `config.json` ships `180s`) | Bounds a *metadata* probe — availability, usage. Short on purpose; the value read has its own timeout below |

## Imports

The four import services and their timeout are documented with the decision that
put them there, in
[imports-as-operator-inputs.md](imports-as-operator-inputs.md#degradation-ladder).

| Key | Default | What it decides |
| --- | --- | --- |
| `device_selection_url` | none | The switch for the whole import half. Empty means semantic selection reports devices only, and says so in its notes |
| `import_deploy_url` | none | Instance status. **Required once `device_selection_url` is set** — the import service refuses to be built without it, because discovery carries no container status and a stopped import would be indistinguishable from a live one |
| `import_repo_url` | none | `get_import_type_metadata` by type id, `list_import_types`, the `deployable_import_types` half of a resolution, and both creations — `create_import_instance` and `create_export` check their request against the import type and refuse rather than send one they could not check. Discovery does not need it for a type that already has an instance: it returns the type alongside every one of them. It is the only route to a type that has none, which is the only kind `create_import_instance` is for |
| `analytics_serving_url` | none | Tells a live-only import from an exported one, and carries `create_export` / `delete_export`. It also gates **reading** an export: a query over one takes column names, and they exist only in the export definition — so without it `probe_export_data` is declared-but-unavailable and `profile_series` refuses an `export_id`. Without it the history question answers `unknown`, which differs from `no history` |
| `import_request_timeout` | `30s` | Bounds one call to any of the four |

### Creating an export

Four fields of an export belong to the deployment rather than to the import being
exported, and none is derivable: the export database is created per deployment by
analytics-serving's own migration, and the timestamp format is whatever this
platform's export worker parses. An export created with the wrong one is accepted,
deploys, and stores nothing — so ODE refuses rather than guessing. The reasoning
is in
[imports-as-operator-inputs.md](imports-as-operator-inputs.md#the-four-fields-that-are-the-deployments-not-the-imports).

| Key | Default | What it decides |
| --- | --- | --- |
| `export_offset` | `smallest` | Where the export worker starts reading. `smallest` takes what the Kafka topic still retains, `largest` only what arrives from now on. The one field the assistant may choose per export, because it is a modelling decision rather than a deployment fact |
| `export_time_path` | `time` | Where an import message carries its timestamp. A real default: every import message carries `time` beside its `value` payload. Overridable per export, for an import whose values describe a time other than their arrival |
| `export_timestamp_format` | none | What the export worker parses the timestamp with. Empty means "copy it from the newest export this platform already has", reported in the answer's `derived`; with nothing to copy, creating an export is refused. Overridable per export, and belongs with a per-export `export_time_path` |
| `export_database_id` | none | Where the export writes. Empty means "the one this platform offers", resolved from `GET /databases`; two is a refusal naming both, because putting an export in the wrong database is found only by the history lookup coming back empty |

## Simulation

MOSES, the platform's environment simulator, is what ODE creates test scenarios
with when the platform has no data for the case. The reasoning is in
[simulation.md](simulation.md).

| Key | Default | What it decides |
| --- | --- | --- |
| `moses_url` | none | The switch for the whole simulation half. Empty leaves the fourteen simulation tools declared and not callable, the same degradation an empty `ray_url` gives the two experiment tools. Nothing in ODE requires a simulator |
| `moses_request_timeout` | `60s` | Bounds one call. Generous by metadata standards, because storing an environment registers a platform device per new asset — one call each — before MOSES answers at all |
| `moses_max_dataset_bytes` | `10485760` | Bounds one uploaded timeseries. A bound on ODE's own memory rather than a policy about file sizes: the file travels out of the developer's pod base64-encoded and then through ODE whole. A file over it is **refused rather than uploaded truncated**, because a cut-off CSV parses and then plays silence from wherever it was cut |

**There is no token here, and there will not be one.** MOSES takes an
environment's owner from the caller's token, so a service account would create
simulations belonging to ODE: nobody could find them in the MOSES UI, and nothing
could delete them. This is the only place in ODE where a service account would be
technically easy and is refused on that ground alone — contrast the Ray and
MLflow tokens, which are permitted precisely because neither service has a
per-user identity to act as.

`upload_simulation_dataset` additionally needs `jupyterhub_url`. The file it
uploads is read out of the developer's own workspace, so without a pod there is
nothing to read; the tool is then declared and not callable, and startup says so.
A simulated channel can still replay a real platform device's history without it,
which is the better source of example data anyway.

## Profiler

The bounds and the two-pass read they describe are in
[profiler-reads.md](profiler-reads.md); the guarantees callers depend on are in
[profiler-contracts.md](profiler-contracts.md).

| Key | Default | What it decides |
| --- | --- | --- |
| `profiler_raw_window_days` | `14` | Lookback of the raw pass. The smaller of this and the points bound wins (D25) |
| `profiler_raw_window_points` | `100000` | Bounds the raw pass's **response, not its rows**: the read is one wide row per message carrying one value per variable, so the figure is divided by the variables read (floored at 2000 rows) and the applied number is recorded as `raw_window.row_limit` |
| `profiler_coverage_window_days` | `90` | Lookback the QuickProfile coverage proxy uses when a request names no window |
| `profiler_concurrency` | `4` | How many series are profiled at once |
| `profiler_local_timezone` | `Europe/Berlin` | Used **only** to flag DST transitions; computation is UTC throughout |
| `profiler_read_timeout` | `300s` | Bounds one *value-reading* request, separately from `timeseries_request_timeout`. One shared figure would either cut the read off mid-assembly or let the probe hang |

## Semantic selection

| Key | Default | What it decides |
| --- | --- | --- |
| `selection_max_criteria` | `12` | How many criteria combinations one resolution may send. One request each, because the platform ANDs a criteria list |
| `selection_device_limit` | `10` | How many devices a resolution expands |
| `selection_concurrency` | `4` | How many selectables requests run at once |

## Persistence

| Key | Default | What it decides |
| --- | --- | --- |
| `postgres_url` | empty | Empty runs the in-memory stores. **A per-user spend cap is then only as old as the process**, the tier audit trail and the profiler override overlay do not survive a restart, and the link from an MLflow run back to its commit is lost |
| `postgres_max_conns` | `8` | Pool size |

## LLM providers

Any subset of the four providers of §5.7 may be configured. With none, the
chat, tool and admin routes are not served. Keys belong in the environment.

| Key | Default | What it decides |
| --- | --- | --- |
| `anthropic_api_key` / `anthropic_base_url` / `anthropic_models` | key empty, base URL the vendor's, models `["claude-opus-5"]` | The central key of D8, accounted per platform user rather than issued per user |
| `openai_api_key` / `openai_base_url` / `openai_models` | empty | The same, for OpenAI |
| `compatible_name` | `openai-compatible` (this repo's `config.json` ships `local`) | Display name of the OpenAI-compatible provider |
| `compatible_base_url` / `compatible_api_key` / `compatible_models` | empty | An OpenAI-compatible server: vLLM, Ollama, Azure |
| `compatible_tools` | `false` | Declares whether that server implements function calling. ODE cannot find out without trying, and a wrong assumption means tools that silently never fire |
| `claude_cli_enabled` / `claude_cli_binary` / `claude_cli_models` | `false`, `claude`, none | The local `claude` CLI, for working without an API key. It reaches ODE's tools over MCP, so it needs `public_url` |
| `public_url` | none | ODE's own externally reachable base URL. Used by the CLI provider's MCP endpoint and to derive the GitHub callback |

## LLM behaviour and cost

| Key | Default | What it decides |
| --- | --- | --- |
| `llm_max_tokens` | `8192` | Bounds one response |
| `llm_effort` | unset | Anthropic's `output_config.effort` |
| `llm_adaptive_thinking` | unset | Whether to send `thinking: {type: "adaptive"}` |
| `llm_max_tool_iterations` | `12` | How many times one exchange may loop through tools. A model that never concludes is stopped by control flow rather than by the spend cap |
| `llm_currency` | `EUR` | The currency the figures below are in |
| `llm_pricing` | the three models in `config.json` | Per million tokens, for the estimated cost §3.3 accounts against. Entries are `{model, input_per_mtok, output_per_mtok, cached_input_per_mtok}`, matched exactly and otherwise by longest prefix. Not baked into the binary, because a stale price makes a cost cap quietly wrong — **verify before relying on one** |
| `chat_exchange_timeout` | `30m` | Ceiling on one detached turn. It exists because an exchange no longer ends with its connection — see [chat-and-streaming.md](chat-and-streaming.md) |
| `chat_confirmation_timeout` | `5m` | How long a confirmed tool call is held open waiting for the developer, where the provider runs its own tool loop (the CLI). Not the turn's ceiling above: it has to fit *inside* one, or the turn ends underneath the card. Startup warns when it does not |

## JupyterHub and the kernel

Why any of this runs in the developer's pod is in
[kernel-and-repository.md](kernel-and-repository.md).

| Key | Default | What it decides |
| --- | --- | --- |
| `jupyterhub_url` | none | The Hub. Empty leaves the kernel routes unserved and `run_code` declared but not callable |
| `jupyterhub_token` | none | ODE's JupyterHub **service** credential. Required once `jupyterhub_url` is set, and a token missing any of `servers`, `tokens`, `access:servers`, `users:activity` fails startup. A non-service token is accepted with a warning, because every spawn is then attributed to that account |
| `jupyterhub_username_claim` | `preferred_username` | Which token claim names the Hub user. A property of the deployed Hub, not of ODE |
| `jupyterhub_kernel` | `python3` | The kernelspec to start. An image whose kernel is named otherwise fails every spawn, at the point where it looks like ODE's fault |
| `jupyterhub_profile` | none (this repo's `config.json` ships `ode`) | The KubeSpawner profile to spawn with. Empty takes the deployment's own default, which is the plain notebook image — set it once the ODE image of §5.6 item 1 exists |
| `jupyterhub_workspace_path` | `data/ode` | The kernel's working directory, relative to the singleuser server root. **It has to be inside the mounted PVC** or nothing a developer writes survives the pod |
| `jupyterhub_spawn_timeout` | `180s` | How long a spawn may take |
| `jupyterhub_request_timeout` | `30s` | Bounds one Hub API call, the startup scope check included |
| `jupyterhub_execute_timeout` | `10m` | Ceiling on one cell, after which it is interrupted rather than abandoned |
| `jupyterhub_keepalive_interval` | `5m` | How often an active session reports activity. Must stay below the cluster's cull timeout |
| `jupyterhub_idle_timeout` | `2h` | When ODE lets go of a session it has not heard from |
| `jupyterhub_token_ttl` | `12h` | Lifetime of the per-user token ODE mints, scoped to `access:servers!user={name}` |
| `jupyterhub_max_output_bytes` | `1048576` | What one cell may stream to the developer's console |

## Tool bounds

The first four bound what a model is handed; they are the budgets of
[profiler-contracts.md](profiler-contracts.md). Raise one and every tool call
carries the difference for the rest of the turn.

| Key | Default | What it decides |
| --- | --- | --- |
| `tool_profile_token_budget` | `4000` | What one projected profile may cost the model |
| `tool_profile_max_profiles` | `4` | How many full profiles one `profile_series` answer carries |
| `tool_quick_token_budget` | `4000` | What a ranked candidate list may cost |
| `tool_relation_token_budget` / `tool_relation_max_rules` | `4000`, `12` | The same, for a relational answer |
| `tool_preview_max_points` | `500` | Caps a tier-L2 preview. This is what keeps "downsampled preview" from becoming a raw series read |
| `tool_run_code_max_output_bytes` | `8000` | Far smaller than `jupyterhub_max_output_bytes`, and for a different cost: that one bounds a developer's console, this one bounds what a cell's output costs in *model context* |
| `tool_repo_max_read_bytes` | `8000` | What one `read_file` answer may cost. The same relationship to the Code pane's megabyte that the row above has to the console: over this the answer becomes a window of whole lines that names the `from_line` to continue at, never a silent cut |

## Charts

Where a transform runs, and why, is in [charts.md](charts.md).

| Key | Default | What it decides |
| --- | --- | --- |
| `chart_max_points` | `2000` | Bounds one series of one chart. The aggregation bucket is widened until the window fits, so this trades resolution against the size of the read and **never truncates the window** |
| `chart_max_annotations` | `200` | Annotations one chart may carry |
| `chart_max_per_user` | `100` | Charts one developer may keep |
| `chart_default_lookback` | `168h` | What a chart covers when neither the author nor a named profile says |

## Relations

Where a candidate set comes from is in [relations.md](relations.md).

| Key | Default | What it decides |
| --- | --- | --- |
| `relation_max_members` | `6` | Members of one relational pass. The pair count grows with the square of it and the rule count with four times that, so this bounds how readable the rule list is rather than how expensive the read is — the aligned read is one batched query either way |
| `relation_max_graph_neighbours` | `12` | How far out of the requested aspect a device relationship graph may pull. This is what keeps one room's question from becoming a site survey |
| `relation_max_buckets` | `20000` | Bounds the aligned grid, which is widened to fit rather than shortening the window |
| `relation_max_rules` | `100` | Rules one pass may produce |
| `relation_max_stored` | `200` | Rule decisions kept per developer |
| `relation_default_lookback` | `720h` | A month, not a week: an exception at certain times of day needs several samples in each hour bucket of each weekday |

## GitHub and the working copy

| Key | Default | What it decides |
| --- | --- | --- |
| `github_client_id` | none | Empty leaves the repo surface unserved and `write_file` declared-but-unavailable. The surface also needs a `jupyterhub_url`, because the working copy lives on the developer's pod |
| `github_client_secret` | none | Required once `github_client_id` is set; startup fails without it. Belongs in the environment |
| `github_token_key` | none | Required once `github_client_id` is set — the developer's GitHub token is stored encrypted (§5.11 item 1). Base64 of 32 bytes: `openssl rand -base64 32`. An absent or malformed key fails startup. Belongs in the environment |
| `github_api_url` / `github_web_url` | `https://api.github.com`, `https://github.com` | For a GitHub Enterprise host |
| `github_scopes` | `["repo", "workflow"]` | `workflow` is not optional in practice: the scaffold writes `.github/workflows/build.yml`, and GitHub rejects a push touching a workflow file from a token without it |
| `github_redirect_uri` | derived from `public_url` | The **SPA's** callback, which has to match the OAuth app's registered one exactly. Deriving it is right only when ODE serves the SPA itself; with neither set, startup fails |
| `repo_command_timeout` | `300s` | Bounds one git command. Minutes, because a clone with history is the slow one |
| `repo_max_file_bytes` | `1048576` | A file the Code pane or `write_file` may move in one request |
| `repo_max_tree_entries` | `4000` | Entries one tree listing returns |
| `repo_max_command_output_bytes` | `1048576` | What one git command may return |
| `repo_max_workbenches` | `3` | How many working contexts one developer may hold open. Each is a kernel process in their pod, so this and the KubeSpawner profile's memory limit are raised together — otherwise a second workbench OOM-kills the first one's training run. A per-user override is `max_workbenches` in the admin limits |
| `operator_lib_repo` | `SENERGY-Platform/analytics-operator-lib-python` | Where the scaffold takes Operator Lib from |
| `operator_lib_ref` | empty | Empty resolves the newest tag at scaffold time and records it per repository, which is D15's *track latest, pin per repo*. Setting it fixes every new repository to one ref |

## Experiments

A run is submitted from a commit or it is not submitted — see
[experiments.md](experiments.md).

| Key | Default | What it decides |
| --- | --- | --- |
| `ray_url` / `mlflow_url` | none | The API bases ODE calls. Empty leaves the experiment routes unserved and `launch_experiment` and `get_experiment_results` declared-but-unavailable; **setting one without the other fails startup**, because ODE creates the MLflow run before submitting the job. The surface also needs a `jupyterhub_url` and a `github_client_id` |
| `ray_token` / `mlflow_token` | empty | The service accounts §3.1 item 5 permits — the only place in ODE where one is legitimate. Both may be empty, because an in-cluster dashboard is commonly unauthenticated and M10's NetworkPolicy is what bounds who reaches it. They belong in the environment |
| `ray_dashboard_url` / `mlflow_ui_url` | the API base | What a browser should open, which in a cluster is routinely a different host. This is what the embed probe of D6 asks and what the pane links to |
| `mlflow_experiment_prefix` | `ode` | Prefix on the MLflow experiment names ODE creates |
| `experiment_default_entrypoint` | `python train.py` | The scaffold's training-only entrypoint: Operator Lib's own init sequence, stopped before the Kafka loop `main.py` would enter and never leave. A committed file rather than one ODE injects, so the package stays exactly the commit it claims to be |
| `experiment_ray_client_url` | `auto` | What Operator Lib passes to `ray.init()`. **Not** `ray_url`, which is the dashboard ODE submits jobs to over HTTP. `auto` attaches to the cluster the driver already runs in; a deployed operator names the cluster's client endpoint (`ray://host:10001`) because it connects from outside |
| `experiment_ts_conn` | empty | The timescale DSN `provide_historic_data` reads history through. A shared database credential rather than the caller's token: it reaches every series, and which series a run reads is decided by its input topics rather than by who launched it. Named here rather than left to Operator Lib's compiled-in default so it is visible where it is handed out. Acceptable while every developer is team-internal with platform administration rights; see [SNRGY-4637](https://bitnify.atlassian.net/browse/SNRGY-4637) |
| `experiment_kafka_bootstrap` | empty | The brokers a run's deployment config carries, for an input topic replayed from Kafka rather than read from timescale. Empty leaves a run able to train from timescale-backed topics only |
| `experiment_max_package_bytes` | `16777216` | Bounds the job archive, and exceeding it is **reported rather than truncated**: a job that ran against a partial repository fails in a way nobody could diagnose. It also bounds ODE's own memory, since the archive is held whole and travels back base64-encoded. What it catches in practice is a checked-in model file or a data directory |
| `experiment_max_env_vars` / `experiment_max_env_value_bytes` | `32`, `4096` | A launch arrives from an HTTP body or from an LLM tool call, and neither is trusted input |
| `experiment_max_log_bytes` | `1048576` | Bounds the developer's own log route. Logs never reach a model (§5.13) |
| `experiment_request_timeout` | `30s` | Bounds one Ray or MLflow API call |
| `experiment_upload_timeout` | `300s` | Bounds the one request that moves the whole archive. Separate for the reason the profiler's two read timeouts are separate: one figure for both means either the probe hangs or the upload is cut off mid-body |
| `experiment_embed_ttl` / `experiment_embed_timeout` | `10m`, `5s` | D6's pair: how long a framing verdict is cached, and how long one probe may take. The probe timeout is short because an unreachable service is a normal answer there and the pane is waiting for it |

## Result interpretation

Why the interpretation waits for the developer is in
[result-interpretation.md](result-interpretation.md).

| Key | Default | What it decides |
| --- | --- | --- |
| `experiment_poll_interval` | `30s` | How often ODE asks Ray about the runs it still calls unfinished |
| `experiment_poll_window` | `6h` | How far back a finished run is still offered for interpretation. Not a tuning knob: it is the figure that covers an ODE restart. A deployment down for an hour should still interpret what finished during it; one down for a week should not replay the week |
| `experiment_poll_batch` / `experiment_poll_timeout` | `200`, `120s` | How many records one tick touches and how long the whole tick may take, so a Ray dashboard that has stopped answering costs one tick rather than the loop |
| `interpretation_retry_interval` | `30s` | How often the runs waiting for a developer are tried again. Every reason an automated turn is refused is transient, so this is the interval at which those resolve rather than a backoff |
| `interpretation_turn_timeout` | `10m` | How long one turn is *waited for*, not how long it may run — `chat_exchange_timeout` is the real ceiling |
| `interpretation_max_pending` | `200` | Summaries held for developers who are away, so a cluster that finished a thousand jobs overnight does not become a thousand held documents |

## The job token

§3.1 item 6. A Ray job reads its training data from timescale-wrapper with
its own token, and a training run outlives an interactive session. Where these
are set, ODE mints one token per submission through an RFC 8693 token exchange on
behalf of the developer, so the job's authorisation stays theirs — which a
service account would have violated. Where they are not set, the caller's session
token is passed, a warning names what is missing, and every launch result says
the credential expires with the session.

| Key | Default | What it decides |
| --- | --- | --- |
| `keycloak_url` | none | The token exchange endpoint's host |
| `keycloak_realm` | `master` | The realm to exchange in |
| `keycloak_client_id` / `keycloak_client_secret` | none | The exchanging client. The secret belongs in the environment |
| `job_token_audience` | none | Not optional in practice: Keycloak returns a token for the *requesting* client unless an audience names another, and a job reads timescale-wrapper |
| `job_token_lifetime` | `12h` | An expectation rather than a request — neither RFC 8693 nor Keycloak accepts a requested lifetime, so ODE compares this against the issuer's own `expires_in` and warns on a shortfall |

## Startup fails on a precondition, not on an absence

The rule is that a missing dependency costs a surface and says so. Startup fails
only where continuing would produce a confusing runtime failure instead:

- `validate()` in `pkg/pkg.go` fails on a missing `api_port` or `device_repo_url`.
  Everything else it checks warns and names the surface that will not be served.
- Each surface then refuses to be built on its **own** precondition — a setting
  that only makes sense alongside another: `device_selection_url` without
  `import_deploy_url`; `jupyterhub_url` without `jupyterhub_token`, or with a
  token missing a required scope; `github_client_id` without
  `github_client_secret`, without a usable `github_token_key`, or with no
  callback to derive; `ray_url` without `mlflow_url` or the reverse.
- Any duration or size that will not parse fails at the point it is read, naming
  the key.

That is the behaviour to preserve when adding a dependency: a deployment that can
do less should say so, not fall over — but a half-configured surface should not
pretend to work.
