# ODE — Operator Development Environment

Human-in-the-loop development environment for machine learning operators on the
KIEEZ / SENERGY platform. A developer goes from a problem statement to a
deployable operator, assisted by an LLM that queries the platform ontology,
interprets computed data profiles and proposes modelling approaches. The
developer defines the evaluation criteria and makes every promotion decision.

The build specification is [SPEC.md](SPEC.md). It is the source of truth; this
file only says how to run what exists.

## Status

**M0, M1a and M1b of the build order in SPEC.md §6.** What works today:

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

- A **Profiler view in the SPA** over all of the above: the ranked candidate
  list with its read counter, the full profile with every non-result shown as
  one, the paginated sessions, the LLM projection, and the confirmation form.

Not built yet: everything from M2 onward — semantic selection, the LLM tool
surface and exposure-tier enforcement, kernels, repositories and experiments.
There are no charts: the exploration pane is M5, and it needs the chart-spec
surface of §5.9 to draw anything.

## Architecture in one paragraph

A Go backend (gin) and a standalone React SPA, deployed into the same
Kubernetes cluster as JupyterHub, Ray and MLflow. The backend reuses
`device-repository/lib/client` and `models/go` rather than reimplementing a
platform client. The SPA holds a Keycloak token and presents it; every
authorisation decision belongs to the backend and, beyond it, to the platform's
own per-user permissions.

```text
pkg/auth/          claim reading, developer-role gate, on-behalf-of token
pkg/ontology/      cached snapshot facade over the device repository
pkg/devices/       per-user device reads, never cached across users
pkg/timeseries/    timescale-wrapper client: availability, usage, batched reads
pkg/profiler/      QuickProfile, SeriesProfile, detectors, store, projection
pkg/api/           gin routes, plus the cancellable WebSocket in ws.go and the
                   operations both surfaces share in operations.go
pkg/configuration/ config.json plus environment overrides
```

## Running it locally

### Backend

```bash
go build -o ode .
./ode -config config.json
```

Configuration is read from `config.json` and overridden by environment
variables, using the platform's usual camel-case-to-`UPPER_SNAKE` mapping
(`device_repo_url` → `DEVICE_REPO_URL`). The settings that matter:

| Key | Meaning |
| --- | --- |
| `required_realm_role` | Defaults to `developer` |
| `device_repo_url` | Device repository base URL |
| `timescale_wrapper_url` | Timeseries base URL. Empty means the timeseries and profiler routes are not served at all |
| `ontology_cache_ttl` | How long a snapshot is served without a freshness check |
| `profiler_raw_window_days` / `profiler_raw_window_points` | Bounds on the raw pass, the smaller of the two wins (SPEC D25). 14 days or 100 000 points |
| `profiler_coverage_window_days` | Lookback the QuickProfile coverage proxy uses when a request names no window |
| `profiler_local_timezone` | Used only to flag DST transitions; computation is UTC throughout |
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

## The profiler over a WebSocket

The Profiler view runs its two slow operations over `GET /ws` rather than the HTTP
routes, because a read outlives an HTTP request: a raw pass bounded at 100 000
points is megabytes of JSON per column, and an ingress idle timeout turns that
into a 504 — with the backend still reading for a client that has gone.

Every request carries an `id`. The client can cancel one, and closing the socket
cancels everything that connection was doing, which is what stops a closed browser
tab from costing platform reads.

```text
→ {"type":"quick_profiles","id":"q1","payload":{"limit":10,"search":"","window":{…}}}
→ {"type":"profile","id":"p1","payload":{"device_id":"…","service_id":"…"}}
→ {"type":"cancel","id":"p1"}
→ {"type":"ping","id":"…"}

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
go test -race ./...          # backend
cd frontend && npm run build # frontend: type-check and bundle
```

The frontend build is also a **contract test**. `frontend/src/__contract__` holds
JSON captured from a running backend, assigned to the types the SPA declares for
those endpoints, so a renamed or dropped field fails the build instead of
becoming `undefined` in front of a developer. It earned its place on the first
run by catching four defects; see the README in that directory, including how to
recapture the fixtures when the shape changes on purpose.

The backend tests use no containers and no network. The device repository is a
fake and test tokens are minted unsigned — deliberately, since signatures are
the gateway's concern — so the suite runs in a few seconds without the platform.

Detector correctness is checked against fixtures with known answers rather than
against the platform (SPEC §5.4.14): a synthesised 15-minute series with an
injected gap, a monotonic counter with two resets, a bimodal washing-machine
load, white noise against a random walk. That is what makes the profiler testable
without an LLM and without the cluster.

## Four things worth knowing before you extend this

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
read after the developer had already chosen the series.

**Never null, never absent.** Every computable profile field is either a value or
an explicit `{"status": "not_computed", "reason": ..., "detail": ...}` (SPEC
D24). `profiler.Value[T]` makes that structural rather than a convention each
detector has to remember, and its zero value marshals as `not_computed` — so a
detector that fails to run cannot report a silent zero. Absence and negation must
stay distinguishable: an LLM that reads a missing `dominant_periods_s` as "no
periodicity" will propose a model on that basis, and nothing downstream can
recover the difference.

**The profile store is in-memory.** `profiler.MemoryStore` is the only
implementation, behind an interface. Losing computed profiles across a restart
only costs a recomputation, but the **override overlay is developer input and an
empirical record** (§5.4.3), and it does not survive either. Persisting it means
choosing and deploying a database, which is a decision M1b did not need to make —
so make it deliberately, rather than discovering the gap in a demo.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
