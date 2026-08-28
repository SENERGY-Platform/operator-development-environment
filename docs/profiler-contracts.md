# Profiler contracts

Three rules the profiler and everything reading it depend on: a non-result is
never a blank, a per-item budget is not a bound on a response, and the two halves
of the store have different durability because they are different kinds of fact.
Plus what a series is addressed by — a device's service or an export, and only one
of those has an availability endpoint — and the two platform timeouts, which exist
for the same reason as the two-pass read.

## Applies when

Adding a detector, a profile field, or a tool that returns a list of profiles;
changing what `profiler.Store` persists; profiling an export rather than a device
service; or tuning either timescale-wrapper timeout.

**Not this if**: the question is how to *call* the profiler rather than what it
guarantees — see [profiler-api.md](profiler-api.md). The detector specifications
themselves are in [profiler-detectors.md](profiler-detectors.md).

`geltung`: `allgemein` for the never-null rule and the store split, which follow
from D24 and §5.4.3; `mehrfach` for the budget figures, which came from
measured responses.

### Never null, never absent

Every computable profile field is either a value or
an explicit `{"status": "not_computed", "reason": ..., "detail": ...}` (D24).
`profiler.Value[T]` makes that structural rather than a convention each
detector has to remember, and its zero value marshals as `not_computed` — so a
detector that fails to run cannot report a silent zero. Absence and negation must
stay distinguishable: an LLM that reads a missing `dominant_periods_s` as "no
periodicity" will propose a model on that basis, and nothing downstream can
recover the difference.

### A token budget per item is not a bound on a response

`tool_profile_token_budget`
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

### Computed profiles are in memory; the override overlay is not

The two halves of
`profiler.Store` have different durability requirements and now have different
homes. A computed profile is reproducible — losing it costs a recomputation — so it
stays in `MemoryStore`. An override is a developer's confirmation of derived
semantics, which §5.4.3 calls an empirical record, so with a `postgres_url` it goes
to a table (`profiler.NewOverlayStore` composes the two). Without one, both are in
memory and the warning at startup says so.

### A series is addressed two ways, and one of them has no availability endpoint

`profiler.SeriesRef` addresses either a device's service or an export, never both,
and everything after the prologue is the same pass — the same two reads, the same
retry, the same detectors, the same store. What differs is what bounds it.

There is no `/data-availability` for an export: the endpoint takes a `device_id`.
So the export prologue counts rows instead (`profiler.ExportFill`, one bucketed
`count` per column) and uses the span it finds where the device prologue uses the
availability window. That has three consequences worth knowing before reading an
export profile:

- **The span is bucket-resolution.** `ExportFill.Bucket` is reported beside it for
  exactly that reason; on the default multi-year probe a bucket is a fortnight
  wide.
- **An empty export is refused rather than profiled.** A device whose availability
  probe fails can still be profiled over the developer's own window, because the
  platform has separately said the device holds data. Nothing says that for an
  export, so `ProfileExport` returns the probe's own reason — which is the one
  sentence that says what to check.
- **Units reach a column only through the import type.** An export declares its
  column type and nothing else; the characteristic, function and aspect come from
  the import type behind an import export, resolved through the export's own
  filter. A device export has no such route and its columns carry no semantics —
  reported in `notes`, so an empty unit is legible as absent rather than as
  unresolved.

The variable path of an export profile is the export's **column name**, not the
variable path it was fed from. The two are not derivable from one another: the
column is named by whoever created the export. `ExportColumn` carries both, and the
query uses the column.

The override overlay keys on all four fields, which is why
`ode_profile_overrides` gained an `export_id` column: two exports with a like-named
column would otherwise share one developer confirmation. The device form of
`SeriesRef.String()` is byte-identical to what it was, so no stored profile changed
its id and no existing override stopped applying.

### Two platform timeouts, because the two kinds of request are not comparable

`timeseries_request_timeout` (60s) bounds a metadata probe: availability, usage,
and the export definition read behind a counting probe. It should fail fast. The
count itself is a `POST /queries/v2` and takes the read timeout, because it is one
join per column server-side even though what comes back is a handful of integers. `profiler_read_timeout` (300s) bounds a value read, where the
server assembles megabytes of JSON for a raw pass of up to a hundred thousand
points. One shared timeout means either the probe waits far too long to fail or the
read is cut off mid-assembly. The client applies whichever it is given as a context
deadline and carries no `http.Client.Timeout` of its own — that field is an absolute
cap and would silently win over the longer one.