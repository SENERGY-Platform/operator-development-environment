# The profiler's two reads, and how they fail

A `SeriesProfile` is computed from exactly two platform reads: one bounded raw
pass and one aggregated pass. What bounds the raw pass is the part worth
understanding, because a gateway refusing an oversized response and a gateway
reporting a sick upstream arrive as the same status code and call for opposite
responses.

## Applies when

Changing the read strategy, the raw-pass bounds, or the retry; or diagnosing a
502 from timescale-wrapper during a profile computation.

**Not this if**: the failure is a *metadata* probe rather than a value read.
Availability and usage are bounded by a different timeout on purpose — see
[profiler-contracts.md](profiler-contracts.md). And not this if nothing was read
at all: a QuickProfile reads zero values by construction, which is the property
its own response reports.

`geltung`: `mehrfach` — the bounds and the retry shape came from repeated real
responses, not from one.

## What bounds the raw pass

`profiler_raw_window_points` bounds the **response**, not the rows, and the
difference matters on every real service. A raw read is one wide
`SELECT "time", col1 … colN … LIMIT n` — one row per message, one value per
variable — so the body costs rows times variables. An eleven-variable energy meter
read at a hundred thousand rows is over a million values in one response, which the
API gateway refuses with a 502 (`An invalid response was received from the upstream
server`) rather than relays. So the configured figure is divided by the variables
being read, floored at 2 000 rows, and the applied number is recorded in
`raw_window.row_limit` and shown in the profile header — a raw window shorter than
the one configured should be explicable without reading the source. A
single-variable service is unaffected, which is why the arithmetic went unnoticed:
it is exactly right for one column.

A gateway refusal is retried **once**, with half the rows, and the retry is recorded
as `raw_window.limit_reduced`. Once rather than twice because a second refusal is
the platform saying no rather than a size to negotiate down, and a rejected
*request* — a 400, or a 500 from the wrapper — is not retried at all, since halving
the rows cannot make a bad column name good.

When a read does fail, the error names the pass, the variables, the window and the
bound it was made with, and for the status codes that mean the *response* was the
problem it names that pass's own levers: fewer rows for the raw pass, a wider bucket
for the aggregated one. The aggregated pass stays non-fatal either way — its fields
report `read_failed` and the structural detectors still have the raw pass — so an
error that reaches the caller is always the raw one.

**A 502 has two meanings, and the levers only fix one of them.** The status class says
the gateway could not get a usable answer, which covers both "the response was too
large to relay" and "the upstream errored or dropped the connection". The remedies are
opposite — ask for less, versus ask again later — so advising the first while the
second is true sends a developer turning a knob that cannot help. Two pieces of
evidence separate them, and ODE has both to hand:

- **How fast it failed.** A gateway refusing a response for its size had to wait for
  the upstream to produce it; one reporting a broken upstream answers in
  milliseconds. Every `UpstreamError` therefore records its elapsed time, and it is
  in the log line.
- **Whether the availability probe failed the same way.** That endpoint answers from
  metadata, so *its* response cannot be too large for anything. A gateway 5xx on it
  and on the value read in the same pass is a statement about the service.

This came out of a real incident, and the log is the clearest way to show it:

```json
{"level":"WARN","msg":"availability probe failed; profiling over the requested window instead",
 "error":"timeseries: /data-availability: timescale-wrapper returned 502: An invalid response…"}
{"level":"WARN","msg":"the gateway refused the raw read; retrying with fewer rows",
 "variables":4,"from_rows":25000,"to_rows":12500,
 "error":"timeseries: /queries/v2: timescale-wrapper returned 502: An invalid response…"}
```

Both 502s, 34 milliseconds apart, one of them from an endpoint that returns a handful
of metadata rows. No volume of rows explains that, so the size hypothesis was wrong —
and the second line, as written then, blamed the size. It now says which hypothesis it
is acting on, and on final failure it says *"asking for less will not help"* with the
evidence rather than offering `profiler_raw_window_points`. The retry still happens
either way: it is one read, and a transient fault may well have passed.

An error with no recorded elapsed time is treated as the size case, which is the
conservative default — the diagnosis only changes when there is evidence for it.

**"Unwell" is not the same as "transient", and the next run showed the difference.**
The same 502s came back a quarter of an hour later against the *same service id* on two
different devices — while the availability probe for one of those devices succeeded, so
the wrapper was plainly answering other requests. A fault that reproduces on one
service while its neighbours are read fine is not an outage to wait out; it is
something about that service. So the failure names the **columns** it asked for, with
their declared types:

```text
row limit 25000, so up to 50000 values in one response;
columns value.power (Float), value.total (Float)
```

That is the list the upstream choked on, and it is what turns "that service is broken"
into a column somebody can go and look at. The list is capped at eight with the
remainder counted, so a forty-variable inverter does not fill the log line.

## The bug behind those 502s, and why the retry is shaped as it is

Those 502s were not about size at all: `timescale-wrapper` was **crashing**. Its own
pod logs said so, and the number is the giveaway.

```text
panic: runtime error: slice bounds out of range [:25000] with capacity 1706
  timescale-wrapper/pkg/api.formatResponse … queries_postprocessing.go:101
```

`25000` was ODE's row limit and `1706` the rows that service had. The slice to the
requested limit was unguarded, so **any** read whose limit exceeded the rows available
killed the process — and the process is shared: in the thirty seconds before that
crash it had served 192 device-command lookups and 99 connection-log checks, all of
which died with it.

**Fixed upstream in `timescale-wrapper v0.1.2`**, which guards the slice (and rejects a
negative limit while it is there):

```go
if limit := request[seriesIndex].Limit; limit != nil && *limit >= 0 && len(results[seriesIndex]) > *limit {
```

ODE briefly carried a workaround — dropping the row limit on the retry rather than
halving it — and it was removed once v0.1.2 was deployed. What stayed is the part that
was never a workaround: the **classification**, because a 502 genuinely has two
meanings and this incident is why ODE now tells them apart rather than assuming.

## When the availability probe fails

`GET /data-availability` derives its answer by regexing the `view_definition` of
each of a device's continuous aggregates
(`timescale-wrapper/pkg/timescale/data-availabilty.go`), and one view it cannot
parse fails the request for the **whole device** with a 500 — for example
`unexpected type matches from view description`, which is its aggregate-name regex
finding no match.

ODE treats that as what it is: metadata it only reads. The probe is bounded
metadata, while the reads a profile is made of are `POST /queries/v2` and are
unaffected, so a profile is computed anyway — over the analysis window the
developer set, since there is no available range to intersect with. What is lost is
recorded rather than papered over: `read_summary.raw_available` comes back as an
explicit **non-result** carrying the platform's own error, not as `false`, because
read as a false it would send a reader looking for retention that is not the cause
(D24).

Two consequences worth knowing. **A window is required** in that state — the
default lookback is anchored on the end of the *available* data, and anchoring it
on nothing would invent a range and then report profiles computed over it as though
it had been chosen (D25) — so a profile with no window asks for one and names the
upstream error. And the **candidate list is unaffected**, because `QuickProfile`
has always tolerated a failed probe per device; coverage and liveness simply arrive
as non-results there.

Charts do not use the endpoint at all: `charts.Data` needs device metadata, the
ontology and one query, so a series on a device in this state can still be drawn
and its unit confirmed.

The fix belongs upstream — read the bucket and the aggregate from the
`timescaledb_information.continuous_aggregates` columns instead of parsing
generated SQL, and skip an unparsable view rather than failing the device.