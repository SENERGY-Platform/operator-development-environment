# The profiler over a WebSocket and over HTTP

The same two operations — a QuickProfile listing and a SeriesProfile computation
— are reachable twice, and the difference is not stylistic. Which one to use, and
what the WebSocket does about authentication and cancellation that HTTP cannot.

## Applies when

Calling the profiler or the selection resolver from something other than the SPA,
or changing either surface. Both are unprojected: the frontend renders every
field, and the token budgets of the tool surface do not apply here.

**Not this if**: the caller is an LLM. Everything a model reaches goes through
`pkg/tools` and is projected to a budget — see
[authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md), and
[profiler-contracts.md](profiler-contracts.md) for why a per-item budget is not a
bound on a response.

`geltung`: `allgemein` for the protocol, which follows from the code; the
deployment note at the end is `einzelfall`.

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
| `GET /timeseries/export-data?export_id=` | Whether an export's table holds rows, per column, and over what span. The export-side counterpart of `/availability`, and a different kind of answer because the platform's availability endpoint is device-scoped — the rows are counted. Optional `from`, `to`; empty means a multi-year lookback, so an export that stopped a year ago is not reported empty. Reads no value |
| `GET /quick-profiles` | Candidate series ranked from metadata alone. `limit` is **devices**, default 10; plus `search`, `from`, `to`, `include_unqueryable`. Reports its own read counts, and `reads.values` is always 0 |
| `POST /selection` | Semantic selection (§5.2), documented above. Also `reads.values` 0 |
| `POST /profiles` | Computes a full profile per variable of one service, or per column of one export. Body: `device_id` with `service_id`, **or** `export_id` — not both; optional `analysis_window`, `raw_window`, `group_time`, `session_params`. An export's window comes from counting rows, and an export with none is refused rather than profiled into a body of `not_computed` |
| `GET /profiles/{id}` | The stored profile with its override overlay resolved |
| `GET /profiles/{id}/projection?token_budget=` | The one model-facing view: arrays collapsed, provenance dropped, elisions recorded |
| `GET /profiles/{id}/sessions?from&to&limit&cursor` | The paginated session resource |
| `POST /profiles/{id}/overrides` | Appends a developer confirmation. Body: `field_path`, `action`, `computed_value`, `confirmed_value`, `note` |

The override route is a **developer** action. §5.8 lists writing a
`ProfileOverride` among the operations with no LLM tool at all, and the tool
surface has none: a model that can confirm its own inferred unit has confirmed
nothing.
