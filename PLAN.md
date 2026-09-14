# Data split: training on training data, inference on test data, neither movable by the model

**Status**: agreed 2026-09-10, not yet built. Three points were decided at
agreement: `need_retraining()` is not consulted during the replay; the library
records artifacts and computes no metric; a launch refuses a test window whose
input row count exceeds a configured cap.

## Goal

A developer sets, per chat session, a **data split**: a training end and a test
end. From then on, for that session:

- the assistant observes no value after the training end, at any exposure tier;
- a launched experiment trains on history before the training end only, then
  runs the operator's inference over the test window and records what it
  produced;
- the assistant has no tool to set, change or skip any of this;
- every run records the bounds it actually applied, so a scoring script can
  verify the split held rather than trust that it did.

How long the test window is and how the recorded forecasts are scored stay with
the evaluation protocol. ODE and Operator Lib enforce the phases and the bounds;
they do not grade.

## Why this shape

Today `provide_historic_data(duration)` reads `[now - duration, now]` on all three
paths (`ts_wrapper.py:135`, `timescale.py:58`, `kafka.py:134`) and the end cannot
be set. An experiment is `train.py`: `init()`, `train_once()`, exit. There is no
inference phase at all — a run's MLflow record holds training metrics and nothing
about how the operator behaves on data it has not seen.

Two consequences for the tier ablation the paper runs. A pilot on a past window
is impossible, because training always reads up to launch. And arms launched on
different days train on different history, which confounds the tier effect with
data freshness.

Pinning only the training end would leave the split porous at L1 and L2: a
profile or a preview over the test window shows the assistant exactly what the
training may not see. So the training end is a session bound, the same way the
tier is. The tier bounds *what kind* of data the assistant observes; the split
bounds *up to when*. Both are session-scoped, developer-set, audited, denied as
tools, and enforced below the model.

The inference phase lives in Operator Lib rather than in an entrypoint the
repository carries, because the repository is what the assistant writes. A phase
sequence in `train.py` is a phase sequence the model can edit; one inside
`MLOperator.init()`, triggered by a config the launch owns, is not.

## Approach

### Operator Lib (`analytics-operator-lib-python`, v1.6.1 to v1.7.0)

1. **A clock the readers ask.** New `operator_lib.util.clock` with `now()`. In a
   deployment it is the wall clock. When the deployment config carries
   `training_end`, it starts there; the evaluation phase advances it per message.
   All three history readers take their `end` from `clock.now()` instead of
   computing `datetime.now()` themselves:
   - `ts_wrapper.read_history`: `end` replaces the `now()` at line 135, and
     `_await_full_duration` probes against it.
   - `timescale.__get_timescale_dataset_query`: `WHERE time >= %(start)s AND time
     < %(end)s`, parameterised, replacing the `NOW() - INTERVAL` literal.
   - `kafka.__get_kafka_dataset`: `start_offset = end - duration`, and rows whose
     `timestamp` (Kafka message time, int64 ms; Ray 2.55
     `kafka_datasource.py:75`) is at or after `end` are filtered out.
2. **Config fields**, both optional ISO 8601 UTC strings on `model.Config`:
   `training_end` and `test_end`. The flow engine sets neither, so deployed
   operators are unchanged. `simple_struct` reads declared keys only, so an older
   library ignores both silently (see Risk).
3. **`provide_historic_data` takes no `end` argument.** The bound comes from the
   clock, the clock from the config. Operator code — which the assistant writes —
   has nothing to pass. `require_full_duration=True` against a fixed end cannot
   wait its way to more data: if the series does not reach back `duration` before
   `end`, raise `ValueError` naming both rather than sleeping forever.
4. **Evaluation mode of `init()`.** When `test_end` is set, `MLOperator.init()`:
   - trains unconditionally through `__wrap_training()` with the clock at
     `training_end`, whatever the registry holds — a registered production model
     from an earlier run was trained on some other split;
   - then replays the test window: reads every input topic over
     `[training_end, test_end)` through the same bounded readers, merges the rows
     by time, and for each row sets the clock to the row's time and calls
     `infer(model, data, selector, device_id, timestamp)` directly. `data` is the
     row keyed by mapping `dest`, which is the frame `read_history` already
     returns; `selector` is resolved the way `gen_filter` resolves it
     (`__init__.py:116`); `device_id` is the topic's `filterValue` for a
     `DeviceId` filter.
   - `need_retraining()` is not consulted during the replay. Retraining inside the
     test window would train on test data, which is the thing the split forbids;
     retraining as lifecycle behaviour is a deployment matter and stays out of the
     experiment. `run()` is untouched: the replay calls `infer()`, not `run()`.
   - `init()` then returns as today. The scaffold's `train.py` stops before
     `start()`, so the process exits; a `main.py` deployment never carries
     `test_end`, so its `init()` is unchanged.
5. **What the evaluation records**, and the phase separation that decides where.
   The training run is ended before the first `infer()` and `MLFLOW_RUN_ID` is
   removed from the environment, so that no fluent write from the operator's own
   code can reach the run ODE reads: `mlflow.start_run()` without a run id reads
   that variable, and a `log_metric()` inside `infer()` would otherwise land in
   ODE's run and hand an L0 model a number computed from test-window values. What
   such a call now does is open a run of its own in the same experiment, which
   nothing in ODE reads. Unsetting the variable takes nothing from code that read
   it at import time, though — `op.py` is imported before `init()` — so the
   transition is also stamped on the run, and ODE withholds by that stamp rather
   than by trusting the boundary. The library records through `MlflowClient` with
   an explicit run id:
   - tags `operator_lib.history_end` (the bound the training read under),
     `operator_lib.test_end`, and `operator_lib.training_ended_at` — Unix
     milliseconds, MLflow's own time base for metric timestamps, set at the phase
     transition before the first `infer()` line runs;
   - params `evaluation.messages`, `evaluation.results`,
     `evaluation.window_start`, `evaluation.window_end`;
   - one artifact, `evaluation/predictions.csv` — a row per `infer()` call:
     message time, selector, device id, result time, result as JSON. There is no
     `inputs.csv`: it would carry platform measurements out of the test window,
     and the singleuser pod reaches MLflow without a token, so with
     `kernel_contain_cells` on a cell that asks for no token could download it
     unconfirmed. The scoring script reads the actuals itself through
     timescale-wrapper on its own credential. No metric is computed here either:
     what the target is and how far ahead the forecast looks are the protocol's to
     declare, and the library does not read `evaluation.yaml`.
6. **Scaffold `train.py`** (in ODE's `pkg/repo/scaffold.go`) skips its own
   `train_once()` when the config carries `test_end`, because `init()` has already
   trained. An older `train.py` trains twice — the second pass equally bounded,
   so wasteful rather than wrong — and the docs say so.
7. Tests under `tests/`: clock default and override; each reader receives the
   bound (mocked transport); `require_full_duration` raises with a fixed end;
   evaluation mode trains despite a registered model, calls `infer()` once per
   replayed row with the clock at that row, never calls `need_retraining()`,
   writes the tags, params and both artifacts; a config without `test_end` leaves
   `init()` on today's path.
8. `__version__ = 'v1.7.0'`. The Ray cluster image has to carry it before ODE
   relies on it (`docs/operator-lib-versions.md`, runbook step 6).

### ODE backend

9. `pkg/exposure`: `Split{TrainingEnd, TestEnd time.Time}` as a pointer on the
   session (nil = none), with `ClampWindow(from, to)` against `TrainingEnd` and
   an `Exposes()`-style sentence for the UI and the prompt. Beside `Tier`,
   because the same two packages enforce it: `pkg/tools` and `pkg/experiments`.
   `TestEnd` must be after `TrainingEnd`; `TrainingEnd` may be in the future,
   in which case the launch is refused until it has passed (a test window with
   no data in it is not an evaluation).
10. `chat.Session.Split *exposure.Split` (`json:"data_split,omitempty"`). Columns
    `split_training_end TIMESTAMPTZ`, `split_test_end TIMESTAMPTZ` via `ADD
    COLUMN IF NOT EXISTS` in `pkg/database/database.go`; `MemoryStore` mirrors
    them. `PUT /chat/sessions/{id}/split` with `{ "training_end": ..., "test_end":
    ... }` or `null` to clear; `Engine.SetSplit` mirroring `SetTier`
    (`engine.go:583`): owner check, store, audit row in `ode_split_changes`, `GET
    .../split-changes`. Clearing is permitted and audited like lowering the tier.
    No admin ceiling: there is nothing to bound.
11. `tools.Request.Split` set wherever `Tier` is set today: `engine.go:1046`,
    `engine.go:1382`, `hold.go:244`, `mcp/mcp.go:230`. Read once per call from
    the session, like `Tier`.
12. `deniedSet()` gains `set_data_split` with the same reasoning as
    `set_exposure_tier`. The existing test that every denied name is unregistered
    covers it.
13. Value reads clamp in one place: `timeseries.QueryOptions.Horizon`. `Query`
    lowers `Time.End` to the training end and refuses an element whose
    `Time.Start` is at or after it with a structured error the assistant can
    relay. Callers forward `req.Split`: `profiler/compute.go`,
    `profiler/export.go`, `charts/resolve.go`, `relations/overlay.go`,
    `tools/executors.go` (`preview_series`, which also clamps its default
    seven-day window).
14. Metadata that names a window clamps too, so the assistant reasons about one
    world: `QuickProfile.Availability.To`, `probe_availability`,
    `estimate_read_cost`. The profiler's `dataWindow` is clamped in the prologue
    (`compute.go`, before `runPass`), so analysis and raw windows inherit it.
15. `experiments.LaunchRequest.Split`; `deploymentEnvironment` writes
    `operatorSettings.TrainingEnd` and `.TestEnd` (`json:"training_end,omitempty"`,
    `json:"test_end,omitempty"`). `Experiment.Split` persisted (two `TIMESTAMPTZ`
    columns, same migration pattern) and shown in `get_experiment_results`.
16. When a run is terminal, the summary reads the tags and params from step 5.
    Tags absent while a split was set: the result carries `split: "not confirmed
    by the run"` and the interpretation says so. That is the guard against a
    cluster image still on an older library, where both fields would be dropped
    silently. The artifact is never read into a tool response and reaches the
    developer through the results route only, the way D34 serves the unmasked
    exception.

    And what a model reads of a run's metrics is cut twice in `MaskedFor`. By
    **phase**: a metric is withheld, whatever its name, when any point in its
    history is stamped at or after `operator_lib.training_ended_at`, which needs
    `latestMetrics` to keep the timestamps it already computes and discards. The
    whole history rather than the point the reduction selects, because
    `log_metric` takes `step` and `timestamp` from its caller and a backdated,
    high-step write is exactly what "the latest value" would point at. That is
    the cut that matches the claim — anything written after training may have
    come from the test window — and it holds against an operator that remembered
    `MLFLOW_RUN_ID` at import time. The cutoff lives on the run and is therefore
    writable too, so the filter is anchored on ODE's own record of whether a
    split ran: under one, a cutoff that is missing, unreadable or outside the
    run's own clock withholds every metric rather than falling back to the names.
    Params are cut to the replay's four under a split for the same reason, since
    a param carries no timestamp to test, and the `data_split` echo of the run's
    own tags is re-rendered from the parsed instant instead of passed through. It
    does not hold against an operator that forges a plausible timestamp on its
    only write, which is a named limit beside the exception text (D34) and egress
    from the cluster, not a hole to be closed here. Where the tag is missing while a
    split was set, the run is on a library older than v1.7.0 and the `data_split`
    block already says `"not confirmed by the run"`; it is not reported twice.
    And by **name**: what survives is `EvaluationCriteria.Metric`, every
    `SecondaryCriteria[].Metric` and the four `evaluation.*` params, with
    `comparison_to_previous` filtered against the same set or the value returns
    through the delta block. Everything withheld is one count,
    `withheld_metrics` — never a name, and with no distinction of reason. The
    developer's own route stays complete, as for D34. The name half is hygiene
    rather than a boundary and the code says so: a declared name can carry a
    test-window value. The phase boundary and the phase filter are what hold.
17. `launch_experiment`'s description says that a session with a split runs the
    evaluation and what the run will record; `get_experiment_results` shows the
    window, the message and result counts, and whether the bounds were
    confirmed. System prompt (`prompt.go:67`) gains one sentence naming the split
    and that reads past the training end are refused for this session, so the
    model asks the developer instead of retrying.
18. `docs/authorisation-and-exposure-tiers.md` gets a section, `docs/experiments.md`
    gets the evaluation phase, `D36` in `docs/decisions.md`, swagger regenerated.

### ODE frontend

19. Split control in the tier strip (`chat.tsx:2547`, `TierControl`): two
    date-time fields and a clear button, the current split always visible, the
    audit list behind the same "how it got here" toggle. `api.ts`: `setSplit`,
    `splitChanges`. Experiment cards show the bounds and whether the run confirmed
    them.

## Assumptions

- The split is set per session by the developer, changeable and audited, not
  locked after the first launch. The audit log is the pre-registration evidence,
  as it is for the tier.
- It binds the assistant's tools and the launches from the session. The
  developer's own UI reads (`POST /profiles`, charts) are not clamped, matching
  the tier, which binds the model and not the person.
- `run_code` in the developer's pod keeps the developer's credential and is
  outside this bound, as it is outside the tier. The protocol's confirmation
  policy covers it; ODE does not pretend otherwise. The same holds for operator
  code inside the Ray job that bypasses `provide_historic_data` and reads the
  wrapper directly with the job's token: the library bounds every read that goes
  through it, and a read that does not is visible in the committed code.
- Inference at time *t* may see history before *t*, including test-window values
  before *t*. That is how an online forecaster works and is not leakage; leakage
  is seeing values at or after *t*, which the clock prevents.
- Timestamps are UTC end to end; the API accepts RFC 3339 with offset and stores
  UTC.

## Not doing

- No `end` argument on `provide_historic_data`. Narrowing inside training
  (rolling-origin validation) is the operator's own slicing of the frame it
  received.
- No scoring in the library or in ODE. `evaluation.yaml` could later declare the
  target field and the forecast horizon, and the evaluation could then log MAE
  and RMSE beside the artifacts; that is a separate step once the protocol has
  fixed both.
- No replay through `run()` and no retraining inside the test window.
- No clamp in timescale-wrapper. A per-token time bound in the platform service
  would be the hard boundary; out of scope, stated as a limit.

## Risk

- Older Operator Lib on the Ray image drops both fields silently: the run trains
  unbounded and skips the evaluation. Caught by step 16 and reported on the run;
  the operator-lib-versions runbook is the fix.
- The replay is sequential `infer()` calls in the driver. A test window of weeks
  at one-second resolution is millions of calls; the evaluation logs progress and
  the launch refuses a window whose input row count, from `probe_availability`,
  exceeds a configured cap, naming the cap.
- A clamp that refuses a start past the training end can surface in the
  profiler where today a window is merely intersected. The refusal is the
  structured kind the assistant relays, and tests cover the boundary at exactly
  the training end.
- Undo: the split is nullable; clearing it restores today's behaviour without a
  schema change. Both library fields are optional; absence is the old path.

## Verification

- `go test ./...` in ODE, with new tests: session set/clear/audit; denied name;
  request carries the split on all four build sites; `Query` clamps and refuses
  at the boundary; `deploymentEnvironment` writes both fields; experiment record
  round-trips; summary flags missing tags; launch refuses a training end in the
  future and an oversized window.
- `pytest tests/` in Operator Lib with the tests from step 7.
- One end-to-end run against the dev cluster once the image carries v1.7.0:
  session with a split in the past, `profile_series` over a window crossing the
  training end (clamped in the response), `preview_series` starting after it
  (refused), a launch whose MLflow run carries both tags equal to the split,
  `evaluation/predictions.csv` with one row per input row in the window, and
  `get_experiment_results` showing the bounds confirmed.

## Handoff

Correctness-critical — a security boundary, a shared library without enforced
tests, a new phase in the operator lifecycle: `implementer-critical`, then
`verifier`. Two repositories, two commit streams; Operator Lib first, since ODE's
verification in step 16 reads what it writes.
