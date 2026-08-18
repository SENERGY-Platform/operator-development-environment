# ODE — Operator Development Environment

**Build specification v1.5**
Human-in-the-loop development environment for machine learning operators on the KIEEZ / SENERGY streaming energy analytics platform.

> **Changes in v1.5:** backend language changed from Python/FastAPI to **Go** (D1, D30, §2). Token validation clarified as **centralised at the platform API gateway** (§3.1): ODE parses claims and enforces the realm role, and does not re-verify signatures per service. Rationale: the team's depth is in Go and the entire platform — device-repository, timescale-wrapper, permissions-v2 — is Go. The change is a net simplification for most of the backend, because `device-repository/lib/client` and `models/go` already provide the §5.1 ontology client and its types. It costs effort in exactly one place, the profiler's numerics (§5.4.14), which is specified rather than hand-waved.
>
> **Changes in v1.4:** `SeriesProfile` design decided and specified in full (§5.4, decisions D19–D29). Notably: two profile tiers, with the read-free `QuickProfile` at exposure tier **L0** (§3.2); immutable profiles with an append-only override overlay; explicit `not_computed` status instead of null; advisory-only recommendations.
>
> **Changes in v1.3:** JupyterHub confirmed as already integrated with the same Keycloak instance, with per-user PVCs auto-mounted. Identity alignment and workspace persistence are therefore resolved rather than open (§5.6, §7, §8). Remaining kernel work reduces to the singleuser image, egress policy, cull exceptions and token refresh.
>
> **Changes in v1.2:** timescale-wrapper OpenAPI spec analysed and integrated (§5.3). Aggregation, resampling, unit conversion and arithmetic are **server-side**; client-side downsampling and conversion are largely removed. `probe_availability` maps directly onto `GET /data-availability`. Batched multi-series queries simplify the relational profiler. No blocking specification gaps remain.
>
> **Changes in v1.1:** device-repository OpenAPI spec analysed and integrated — the platform exposes a full semantic ontology, which replaces name-based data selection with semantic query (§5.1, §5.2). Kernel execution delegates to the existing Kubernetes JupyterHub instead of a bespoke pod spawner (§5.6).

---

## 0. Purpose and non-goals

**Purpose.** Enable a developer with data-science experience to go from a problem statement to a deployable ML operator, assisted by an LLM that queries the platform ontology, interprets computed data profiles, proposes modelling approaches, and interprets experiment results. The developer defines evaluation criteria and makes all promotion decisions.

**Output artifact.** A GitHub repository containing an operator conforming to the Operator Lib interface, built into a container image by GitHub Actions and pushed to `ghcr.io`.

**Non-goals — do not build these:**

- Not a notebook environment. Cells are not the organising metaphor.
- Not an AutoML system. The LLM suggests; it does not select autonomously.
- Does not build or push container images. GitHub Actions does that.
- Does not deploy to production pipelines.
- Does not modify evaluation criteria autonomously.
- No synthetic/public-data reproduction mode required.

---

## 1. Locked decisions

| # | Decision |
|---|---|
| D1 | **Standalone React SPA + Go backend.** Not a JupyterLab extension (§1.1). Language rationale in D30. |
| D2 | Arbitrary Python execution required → **delegate to the existing k8s JupyterHub** (§5.5). Do not build a bespoke pod spawner. |
| D3 | Profiling is per-device/per-series. A separate **relational profiler** handles multi-device conditional patterns, scoped by the aspect hierarchy. |
| D4 | Data exposure to the LLM is a **developer-controlled three-tier toggle**, enforced server-side (§3.2). |
| D5 | Public hosting via proxy. **Keycloak** auth, `developer` realm role required. All platform reads use the **user's token** (on-behalf-of), never a service account. |
| D6 | Ray/MLflow UI embedding is **probed at runtime**; falls back to link-only on framing failure. |
| D7 | Providers: Anthropic API, OpenAI, local OpenAI-compatible, plus a **local `claude` CLI wrapper** for development without an API key. |
| D8 | **Central** LLM API key. Per-user and global limits configurable by the `admin` realm role. |
| D9 | Repo-based only. GitHub OAuth; select or create repo; CI workflow ensured on creation. Registry `ghcr.io`, changeable by editing repo files. |
| D10 | Analytics Stack registration is **offered**, developer decides. |
| D11 | Derived semantics (units, sessions, rules) are **confirmed interactively**. No labelled ground truth assumed. |
| D12 | Open source. Working UI beats polished UI. |
| D13 | **Data selection is semantic, not name-based.** Driven by the device-repository ontology (aspects, functions, characteristics), not string matching on device names. |
| D14 | The developer has **full read/write access to every file in the repo** — no hidden or ODE-managed files. |
| D15 | Operator Lib: track **latest**; no stability guarantees assumed. Pin per-repo at scaffold time and allow upgrade. |
| D16 | Device-type completeness is **probed at runtime** against the ontology, not assumed. |
| D17 | MLflow per-user experiment namespacing is sufficient. No further isolation work. |
| D18 | No per-user Ray namespace or quota work required. |
| D19 | Addressable unit is `{device_id, service_id, variable_path}`. Profiles are per-variable, computed in **service-scoped batches** with a `service_context` block for cross-variable checks. |
| D20 | **Two profile tiers.** `QuickProfile` requires no series reads and sits at exposure tier L0; `SeriesProfile` requires reads and sits at L1. |
| D21 | Profiles are **immutable**; developer confirmations live in an **append-only override overlay**, merged at read time only. |
| D22 | Provenance is a **sidecar map keyed by dotted field path**, not a per-field envelope. |
| D23 | Confidence is **ordinal** (`certain \| likely \| uncertain`) and always carries raw evidence. No numeric probabilities on heuristics. |
| D24 | Un-computable fields carry an **explicit `not_computed` object**. Never null, never absent. |
| D25 | Two explicit windows (`analysis_window`, `raw_window`). Raw default is min(14 days, ~10k points), **developer-overridable**. `detector_version` is part of the cache key. |
| D26 | One stored form, one **projection function** for the LLM that collapses unbounded arrays and **records elisions**. |
| D27 | Sessions are a **separate paginated resource**, referenced from the profile. |
| D28 | `recommendations` are **strictly advisory** — binding only on explicit developer promotion. |
| D29 | **`characteristic_id` is canonical**; the unit string is derived. Never fabricate a characteristic ID. |
| D30 | **Backend is Go**, matching the rest of the platform. Ontology and timeseries clients reuse `device-repository/lib/client` and `models/go`. The profiler's statistical detectors are implemented in Go against `gonum`; the one detector without a library implementation (ADF) is specified in §5.4.14. |

### 1.1 Why not a JupyterLab extension — and why JupyterHub is still used

These are separate questions. **The UI decision stands:** ODE is a standalone SPA because the deliverable is a container image rather than a notebook, the workflow is staged rather than free-form, the exploration component is bespoke either way, and iframe hide/pop-out is awkward inside Lumino and JupyterLab's CSP.

**The execution backend decision is different.** Reusing the existing JupyterHub as kernel infrastructure is orthogonal to where the UI lives, and it is clearly correct — see §5.6.

---

## 2. Architecture

```
                          Browser (React SPA)
  ChatPane │ DataPane │ ExplorationPane │ CodePane │ ExperimentPane
                    LayoutManager (dock / hide / pop-out)
                              │
                    REST + SSE, Bearer: Keycloak token
                              │
                        Backend (Go, gin)
  pkg/auth/         Keycloak validation, role check, on-behalf-of propagation
  pkg/llm/          provider abstraction, tool dispatch, exposure-tier gate
  pkg/mcp/          ODE tool surface exposed as MCP server (for CLI provider)
  pkg/ontology/     device-repository client, semantic cache, selectable query
  pkg/timeseries/   timescale-wrapper client: availability, batched queries, download
  pkg/profiler/     deterministic series profiling + segmentation
  pkg/relations/    aspect-scoped alignment, contingency, rule candidates
  pkg/kernel/       JupyterHub client: spawn, kernel lifecycle, execution
  pkg/repo/         GitHub OAuth, clone, scaffold, commit, push
  pkg/experiments/  Ray job submission, MLflow query, embed probe
  pkg/session/      project state, confirmation overrides, quota accounting
                              │
   ┌────────────┬─────────────┼───────────┬──────────┬────────────┐
device-repo   timeseries   JupyterHub    Ray      MLflow      GitHub
(user token) (user token)  (svc + user  (svc)     (svc)     (user token)
                            token)
```

**Deployment.** Container in the same Kubernetes cluster as JupyterHub, Ray and MLflow; exposed publicly through the existing ingress. In-cluster placement resolves service DNS and keeps series data off the public path.

---

## 3. Identity, authorisation, data governance

| Identity | Source | Governs |
|---|---|---|
| Platform user | Keycloak access token | Which devices and series are readable |
| Notebook user | JupyterHub, **same Keycloak instance** | Which singleuser pod executes code; identity is already aligned |
| Repo owner | GitHub OAuth token | Which repositories are writable |
| LLM caller | Central API key | Provider access; accounted and limited per platform user |

### 3.1 Authentication and authorisation

1. Frontend obtains a Keycloak access token; sends it as `Authorization: Bearer` on every backend request.
2. **Signature, expiry and audience are validated centrally by the platform API gateway**, not per service. ODE parses the token unverified — via the shared `service-commons/pkg/jwt.Token` — to read `sub` and `realm_access`, and enforces the **`developer` realm role** itself. A missing token is 401; a valid token without the role is 403.

   The role check stays in ODE because it is an authorisation decision specific to this service, not authentication. **The trust boundary this creates is that ODE must be reachable only through the gateway.** In-cluster callers bypass the gateway and are authenticated by nothing — and §5.6 puts arbitrary developer and LLM-authored Python in JupyterHub singleuser pods in the same cluster. The M10 NetworkPolicy is therefore not only about egress *from* those pods; it is what makes step 2 sound.
3. **All device-repository and timeseries reads use the caller's token.** The platform's per-user authorisation is therefore the single source of truth. Never substitute a service account for user data.
4. The device-repository exposes `/permissions/accessible/devices` and `/permissions/check/devices` — use these rather than reimplementing authorisation logic. `ExtendedDevice` also returns a computed `permissions` object and a `shared` flag per device.
5. Service accounts are used **only** for Ray, MLflow, and JupyterHub admin operations.
6. **Token lifetime vs. long Ray jobs:** mint a short-lived scoped token at job submission. Do not rely on the interactive session token outliving the run.

### 3.2 Data exposure tiers (D4)

Enforced in `ToolDispatcher` **before** any tool executes. Never client-side. Session-scoped, developer-settable, default **L0**. Every change logged with timestamp and user.

| Tier | Exposed to the LLM | Notes |
|---|---|---|
| **L0** (default) | Ontology (aspects, functions, characteristics, device classes), device names and types, availability windows, volume and rate estimates, connection state, **`QuickProfile`** (§5.4.2) | No values whatsoever. Semantic selection *and* candidate triage complete entirely here |
| **L1** | L0 + `SeriesProfile` and `RelationProfile` (statistics, detected periods, session stats, quality flags) | Aggregates are still data — a deliberate step |
| **L2** | L1 + downsampled series previews (actual values) | Required for the LLM to reason about shape |

Surface the current tier persistently in the UI. When a tool is blocked, return a structured refusal the LLM can relay: `{"blocked_by_tier": "L0", "required": "L1"}` — so the assistant asks the developer to raise it rather than failing opaquely.

Rationale for three tiers rather than a binary toggle: the VHB commits to *Datensparsamkeit*. A single on/off switch would force full value exposure to gain any statistical reasoning. **Two things make L0 substantive rather than nominal:** the ontology is rich enough for semantic selection (§5.2), and `QuickProfile` (§5.4.2) ranks candidates from availability, volume and liveness metadata without reading a single value. A developer can therefore go from problem statement to a shortlist of three series before raising the tier at all.

### 3.3 Admin controls (D8)

Realm role `admin` gates a settings surface for:

- Per-user LLM token/cost limits (period, hard cap, soft warning)
- Global LLM spend cap
- Allowed providers and models
- Maximum exposure tier permitted per user or globally
- Default and maximum kernel resource requests
- Concurrent session and Ray job caps

Accounting is per Keycloak `sub`, recorded per request: provider, model, input/output tokens, estimated cost, session, timestamp. Enforce before dispatch; return a structured error on cap breach.

---

## 4. Core design rule

> **The LLM never computes statistics from raw data. The Profiler computes; the LLM reads the Profile and interprets.**

Consequences: raw series never enter the LLM context; profiling is reproducible and testable without an LLM; context cost stays bounded; the profiler is unit-testable in isolation.

Stated as a design principle in the paper. Do not violate it for convenience.

---

## 5. Components

### 5.1 `ontology/` — semantic model client

**This is the most consequential finding of the spec analysis.** The device-repository is not a flat device list with field descriptions. It exposes a full ontology, and ODE should be built around it.

**Model (from `devicerepository_swagger.yaml`):**

| Entity | Key fields | Role in ODE |
|---|---|---|
| `AspectNode` | `id`, `parent_id`, `child_ids`, `ancestor_ids`, `descendent_ids`, `root_id` | **Hierarchical** location/subsystem. Scopes multi-device analysis (§5.4) |
| `Function` | `id`, `concept_id`, `rdf_type` (`MEASURING` \| `CONTROLLING`), `display_name` | What is measured or controlled |
| `Concept` | `base_characteristic_id`, `characteristic_ids`, **`conversions`** | Groups characteristics; carries the unit conversion graph |
| `ConverterExtension` | `from`, `to`, `formula`, `distance`, `placeholder_name` | **Executable unit conversion** between characteristics |
| `Characteristic` | `display_unit`, `min_value`, `max_value`, `allowed_values`, `type`, `sub_characteristics` | **Authoritative unit and range source** |
| `ContentVariable` | `aspect_id`, `function_id`, `characteristic_id`, `unit_reference`, `path`, `type`, `sub_content_variables` | The leaf: an addressable series |
| `Service` | `interaction` (`event` \| `request` \| `event+request`), `inputs`, `outputs` | `event` ⇒ streamed to Kafka; determines whether a series exists at all |
| `DeviceType` | `services`, `service_groups`, `device_class_id`, `attributes` | Device template |
| `DeviceClass` | `id`, `name` | Kind of device (lamp, thermostat, meter) |
| `DeviceGroup` | `criteria` (`aspect_id`, `device_class_id`, `function_id`, `interaction`) | Pre-existing semantic groupings — reuse before inventing |
| `Location` | `device_ids`, `device_group_ids` | Physical grouping |
| `Graph`/`Node`/`Edge` | `resource_type: device`, weighted edges | **Probe at runtime** — a device relationship graph may already exist and would seed §5.4 |
| `Attribute` | `key`, `value`, `origin` | Extension point; may carry PV capacity, orientation, etc. |

**Endpoints — prefer versioned:**

```
GET  /v2/aspects, /v2/aspect-nodes, /v2/characteristics
GET  /v2/concepts-with-characteristics, /v2/device-classes
GET  /v3/device-types            ?ids,search,criteria(JSON FilterCriteria),attr-keys,attr-values
GET  /functions, /measuring-functions, /controlling-functions
GET  /aspect-nodes/{id}/measuring-functions   ?ancestors,descendants
POST /v2/query/device-type-selectables        ← primary semantic selection endpoint
GET  /extended-devices          ?ids,search,device-type-ids,connection-state,fulldt,attr-keys,p
GET  /devices/{id}/connection-state
GET  /device-groups, /locations, /graphs
GET  /permissions/accessible/devices
GET  /last-update-timestamps    ← ontology cache invalidation
```

**Most of this client already exists (D30).** `github.com/SENERGY-Platform/device-repository/lib/client` implements every endpoint above — including `GetDeviceTypeSelectablesV2`, `GetAspectNodes*`, `ListConceptsWithCharacteristics` and `ListExtendedDevices` — and `github.com/SENERGY-Platform/models/go` carries the entity types. **Do not reimplement it.** `pkg/ontology` is a caching, ODE-shaped facade over that client, not a new HTTP client.

Two properties of that client shape ODE's design:

- **User-scoped reads take an explicit `token string`** (`ListExtendedDevices`, `ListDeviceTypesV3`, `ListDeviceGroups`, `ListGraphs`). On-behalf-of (D5, §3.1) is therefore a parameter, not a client-construction concern.
- **Ontology reads take no token argument** (`GetAspectNodes`, `GetFunctionsByType`, `ListCharacteristics`, `GetDeviceClasses`, `GetDeviceTypeSelectablesV2`). This confirms the caching split below: the ontology is platform-global and cacheable process-wide; devices are per-user and are not.

> **They still need a token on the wire.** Those methods set no `Authorization` header of their own and defer entirely to the auth closure passed to `client.NewClient`. ODE reaches the device repository **through the Kong API gateway**, which rejects an unauthenticated request with 401 before it reaches the repository at all. Construct the ontology client **per load, bound to the caller's token**; a client built once at startup with a `nil` closure makes every ontology read fail with 401.
>
> The snapshot stays shared process-wide, which remains correct — the ontology is identical for every user, and only the transport needed authenticating. Two tests in `pkg/pkg_test.go` pin this against the real client, because a fake cannot catch it and the compiler will not either.

**Caching.** The ontology is small and slow-changing. Cache aspects, functions, concepts, characteristics and device classes at startup; invalidate via `/last-update-timestamps`. Devices are per-user and must not be cached across users.

**Permission nuance for later milestones.** `models.Read` ('r') governs device *metadata*; `models.Execute` ('x') governs *reading device data*. M0 lists devices under `Read`; every timeseries read from M1a onward must be scoped to `Execute`, or ODE will offer series it cannot actually read.

### 5.2 Semantic data selection (replaces name-based browsing)

The original design had the LLM read device names as strings. With this ontology that is unnecessary and much weaker. **`POST /v2/query/device-type-selectables` takes a `FilterCriteria` list (`function_id`, `aspect_id`, `device_class_id`, `interaction`) and returns matching services with resolved variable paths.**

The LLM therefore selects data by *meaning*:

```
"forecast PV generation for this site"
  → resolve function: measuring-function ~ "power generation"
  → resolve aspect subtree: aspect "PV System" + descendants
  → query device-type-selectables(criteria=[{function_id, aspect_id, interaction: "event"}])
  → resolve characteristic → display_unit, min/max
  → GET /extended-devices filtered by resulting device-type-ids
  → probe_availability on each candidate series
  → propose_data_selection (requires developer confirmation)
```

Implement as `resolve_semantic_selection(intent)`:

```json
{
  "matched_functions": [{"id": "...", "name": "...", "rdf_type": "...", "concept_id": "..."}],
  "matched_aspects": [{"id": "...", "name": "...", "descendants_included": true}],
  "selectables": [{"device_type_id": "...", "service_id": "...", "path": "...",
                   "characteristic_id": "...", "unit": "W", "interaction": "event"}],
  "candidate_devices": [{"device_id": "...", "name": "...", "connection_state": "online",
                         "device_type_id": "...", "permissions": {...}}],
  "ontology_gaps": [{"device_type_id": "...", "missing": ["characteristic_id"],
                     "consequence": "unit must be inferred"}]
}
```

`ontology_gaps` implements D16: completeness is discovered at runtime, per device type, and reported rather than assumed.

**Candidates are ranked by `QuickProfile`** (§5.4.2), not returned unordered. A query may yield 40 series; rank by span, coverage proxy and liveness so the developer narrows to a handful before any `SeriesProfile` is computed. All of this is tier L0.

**Paper note.** This is a concrete differentiator from IBM Castor. Castor's semantic layer is an entity/signal pair. This is a hierarchical aspect tree plus a function ontology plus a characteristic layer carrying executable unit conversions. Say so explicitly in related work.

### 5.3 `timeseries/` — data retrieval (timescale-wrapper)

Source of truth: `timescale-wrapper/docs/swagger.yaml`. Same `Bearer` JWT scheme as device-repository, so the on-behalf-of chain (§3.1) holds end to end with no additional identity work.

| Endpoint | Use in ODE |
|---|---|
| `GET /data-availability?device_id=` | **Implements `probe_availability`.** Returns per-service `{serviceId, from, to, groupTime, groupType}` — availability window *and* which pre-aggregated variants exist |
| `POST /queries/v2` | Primary read. Accepts an **array** of query elements — many series in one call |
| `POST /last-values` | Batched current values; cheap liveness check |
| `GET /last-message`, `GET /raw-value` | Single-value probes |
| `GET /usage/devices` | `{bytes, bytesPerDay, updatedAt}` per device — **volume and rate estimate without reading any values** |
| `GET /prepare-download` → `GET /download/{secret}` | Bulk export path for training data |
| `GET /usage/exports` | Export volume accounting |

**`QueriesRequestElement`:**

```
source     : deviceId + serviceId | deviceGroupId | exportId | locationId
columns[]  : { name, groupType, math, conceptId,
               sourceCharacteristicId, targetCharacteristicId, criteria }
time       : { start, end, last, ahead }     # 'last' is relative; 'ahead' allows future
groupTime  : bucket width, e.g. "15m"        # server-side resampling
filters[]  : { column, type, math, value }
limit, orderColumnIndex, orderDirection (asc|desc)
```

Query params on `/queries/v2`: `format` (`per_query` default, 3D array), `time_format` (Go layout), `order_column_index`, `order_direction`, `locate_lat`/`locate_lon` for multivalued location series.

#### 5.3.1 Consequences — remove client-side work

Four things the v1.1 spec planned to build client-side are server-side here:

1. **Downsampling** → `groupTime` + `groupType`. **Drop client-side LTTB** except as a fallback for genuinely raw reads where no bucketing is acceptable.
2. **Unit conversion** → `sourceCharacteristicId` → `targetCharacteristicId`, executed against the ontology conversion graph (§5.1). The `convert:` chart transform (§5.9) maps directly onto this; do not reimplement formula evaluation.
3. **Arithmetic** → the `math` field, e.g. scaling W→kW.
4. **Alignment** → issue one batched `POST /queries/v2` with the **same `groupTime` across all elements**. Multi-series alignment for the relational profiler (§5.5) is then a property of the request rather than client-side resampling code.

#### 5.3.2 Profiler read strategy — important subtlety

**Aggregated reads hide exactly what the profiler needs to detect.** With `groupTime` set, gaps are filled or smoothed and sampling irregularity disappears. Therefore:

| Detector | Read mode |
|---|---|
| Sampling interval, regularity, gaps, counter resets, frozen sensor | **Raw** (no `groupTime`), bounded window |
| Distribution, periodicity, trend, stationarity | Aggregated (`groupTime`) over the full range — cheap |
| Session detection | **Raw** or fine bucket; coarse buckets destroy short sessions |
| Range violation, quality flags | Either, depending on flag |

Implement as a two-pass strategy: a bounded raw window for structural detection, a full-range aggregated pass for statistical structure. Record which mode produced each profile field.

#### 5.3.3 Cost estimation at tier L0

`GET /usage/devices` returns `bytesPerDay`, which yields an approximate ingest rate and total volume **before any value is read**. Combined with `/data-availability` windows, ODE can estimate read cost and warn on expensive selections while still at exposure tier **L0** (§3.2). This materially strengthens the case that meaningful work completes before any data reaches the LLM.

#### 5.3.4 Training data path

Ray jobs must **not** stream training data through the ODE backend. Two acceptable paths: the job queries `timescale-wrapper` directly with its scoped token, or ODE calls `/prepare-download` and passes the resulting secret. Prefer direct query for repeatable training; use download for large one-off extracts.

#### 5.3.5 Open detail

`groupType` and `groupTime` accepted values are not enumerated in the spec. **Probe at runtime** against a known device and cache the working set. Confirm in particular whether a `difference` aggregate exists — if so, cumulative counters (§5.4) can be differenced server-side rather than in the profiler.

### 5.4 `profiler/` — deterministic profiling

**Highest-risk component. Build first, before any LLM integration.** Eleven schema decisions are locked as D19–D29 below.

#### 5.4.1 Addressable unit and atomicity (D19)

The addressable series is **`{device_id, service_id, variable_path}`** — not `{device_id, service_id}`. A `Service` output is a `ContentVariable` *tree*, and `timescale-wrapper` addresses leaves via `columns[].name`.

Profiles are computed **per variable, in service-scoped batches**: one `POST /queries/v2` per service covering all its variables, then one profile emitted per variable. Each profile carries a `service_context` block enabling cross-variable checks that no single variable reveals.

```json
"service_context": {
  "service_id": "...", "interaction": "event",
  "sibling_variables": [{"path": "...", "characteristic_id": "...", "kind": "..."}],
  "relationships": [{
    "type": "integral_of | derivative_of | redundant_with | inconsistent_with",
    "other_path": "...", "evidence": {"correlation": 0.0, "residual_ratio": 0.0},
    "confidence": "likely"
  }]
}
```

**The check that motivates this:** energy meters routinely emit instantaneous power *and* a cumulative energy counter on the same service. Comparing `diff(energy_total)` against integrated `power` catches unit errors, dead channels and unflagged counter resets. The batched read makes it free.

#### 5.4.2 Two profile tiers (D20)

**`QuickProfile` — zero series reads, exposure tier L0.** Assembled from `/data-availability`, `/usage/devices`, the ontology, and `connection-state`.

```json
{
  "series_ref": {"device_id": "...", "service_id": "...", "variable_path": "..."},
  "tier": "quick",
  "availability": {"from": "...", "to": "...", "span_days": 0,
                   "aggregates": [{"group_time": "15m", "group_type": "mean"}]},
  "volume": {"bytes": 0, "bytes_per_day": 0.0,
             "estimated_interval_s": 900, "estimate_basis": "bytes_per_day",
             "confidence": "uncertain"},
  "declared": {"characteristic_id": "...", "unit": "W",
               "min_value": 0, "max_value": 0, "type": "https://schema.org/Float"},
  "interaction": "event",
  "liveness": {"connection_state": "online", "last_value_age_s": 0},
  "ontology_completeness": {"status": "complete | partial", "missing": []},
  "rank_hints": {"span_days": 0, "coverage_proxy": 0.0, "is_live": true}
}
```

`estimated_interval_s` derived from `bytes_per_day` is order-of-magnitude only — always `confidence: "uncertain"`, never used for resampling decisions.

**`SeriesProfile` — full, exposure tier L1, requires reads.** Schema in §5.4.12.

Semantic selection (§5.2) may return 40 candidates; QuickProfile ranks all of them and the developer narrows to a handful before any value is read. **Selection and triage both complete at L0** — this is what makes the *Datensparsamkeit* argument concrete rather than nominal.

#### 5.4.3 Immutability and the override overlay (D21)

Profiles are **immutable computed artifacts**. Developer confirmations (D11) live in a **separate append-only overlay**, merged only at read time. Never write the merged form to storage.

```json
// ProfileOverride
{
  "override_id": "...", "profile_id": "...",
  "created_at": "...", "created_by": "...",
  "field_path": "value_semantics.unit",
  "computed_value": "W", "confirmed_value": "kW",
  "action": "confirm | correct | reject",
  "note": "..."
}
```

```go
// Resolve adds a `resolution` map of overridden paths. Pure function, no I/O.
func Resolve(p SeriesProfile, overrides []ProfileOverride) ResolvedProfile
```

Three reasons this matters: recomputation is non-destructive, so improving a detector or widening a range preserves confirmations; computed-versus-confirmed stays diffable; and **the override log is an empirical record** — "detector said X, developer corrected to Y" is a paper finding, and a mutable document destroys it.

#### 5.4.4 Provenance sidecar (D22)

Keyed by dotted field path, kept out of the profile body so the body stays flat and readable.

```json
"provenance": {
  "sampling.gaps": {"read_mode": "raw", "source": "detector",
                    "detector": "gap_v1", "window": {"from": "...", "to": "..."}},
  "value_semantics.unit": {"source": "ontology", "ref": "characteristic:abc"},
  "temporal_structure.dominant_periods_s": {"read_mode": "aggregated",
                                            "group_time": "15m", "source": "detector"}
}
```

`source` ∈ `ontology | detector | inference | developer | api`. `read_mode` ∈ `raw | aggregated | none`, recording which pass of §5.3.2 produced the field. Dropped by the LLM projection.

#### 5.4.5 Confidence is ordinal only (D23)

`certain | likely | uncertain`, **always accompanied by the raw evidence**. Counter detection reports `monotonic_ratio: 0.98` *and* `likely`.

`certain` is reserved for ontology-derived and developer-confirmed values. Never attach a numeric probability to a threshold heuristic — it is fake precision, and an LLM will over-trust `0.87`.

#### 5.4.6 Explicit field status — never null (D24)

Every computable field is either a value or an explicit non-result:

```json
{"status": "not_computed",
 "reason": "insufficient_coverage | insufficient_span | wrong_kind | read_failed | out_of_scope",
 "detail": "completeness_ratio 0.61 < 0.80"}
```

**Never `null`, never absent.** An LLM reading a missing `dominant_periods_s` concludes *"no periodicity"* rather than *"could not determine"*, then proposes a model on that basis. Absence and negation must be distinguishable. Treat this as non-negotiable.

#### 5.4.7 Windows, detector version, cache key (D25)

Two windows, both explicit:

- `analysis_window` — the full requested range, read aggregated.
- `raw_window` — the bounded subset read raw for structural detection.

**Default raw window:** the smaller of 14 days or ~10 000 points, anchored at the most recent data. Recent data matters most for drift, and bounded raw reads keep cost predictable.

**The developer can override the raw window** per profile request and as a session default. Overrides are recorded in `provenance` and surfaced in the UI — a profile computed over an unusual window must not be mistaken for a default one.

```
detector_version   in the profile body AND in the cache key
cache_key = hash(series_ref, analysis_window, raw_window, detector_version)
```

Without `detector_version` in the key, improving the session detector silently leaves stale profiles in the LLM's context.

#### 5.4.8 LLM projection (D26)

```go
func Project(p ResolvedProfile, tokenBudget int) LLMProfileView
```

Collapses unbounded arrays: sessions → `session_stats` + 3–5 exemplars; gaps → count + largest 3 + total duration; `constant_runs` → count + longest. Drops `provenance`, merges overrides.

**Records what it elided**, so the LLM knows it is seeing a summary:

```json
"elided": [{"field": "activity_pattern.sessions", "total": 1847, "shown": 5,
            "fetch": "/profiles/{id}/sessions"}]
```

A washing machine over two years produces thousands of sessions; unelided, that alone exceeds any sane context budget.

**The same rule at L0, and against breadth rather than depth.**

```go
func ProjectQuick(r QuickResult, tokenBudget int) QuickView
```

Drops `provenance` and the materialised-aggregate list, states the fields that are the same for every candidate once, and fits the ranked list to the budget one device at a time — a fleet of one device type ties on every ranking input, and a ranked prefix would answer a question about three inverters with one inverter's variables. What was cut is recorded per device, named as well as identified.

A budget *per item* is not a bound on a response: eighty candidates unprojected are around 48k tokens, and `profile_series` yields one projection per variable of a service. The tool surface therefore bounds both, `tool_quick_token_budget` for the candidate list and `tool_profile_max_profiles` for the profile list, with `variable_paths` as the way to ask for a profile the cap left out. A tool result is resent on every iteration of the tool loop, so an unbounded one is charged for the whole turn, not once.

#### 5.4.9 Sessions as a separate resource (D27)

```
GET /profiles/{profile_id}/sessions?from&to&limit&cursor
```

The profile holds `session_stats`, exemplars and a reference. Chart annotations (§5.9) fetch on demand — which is also what the exploration pane needs for zoom-dependent rendering.

#### 5.4.10 Recommendations are strictly advisory (D28)

`recommendations` is **never read directly** by the operator scaffold, the training code or a Ray job. Values become binding only when explicitly promoted by developer action into `evaluation.yaml` or the operator config, and the promotion is recorded.

If profiler recommendations silently drove training behaviour, a threshold heuristic would be setting the resampling policy with nobody deciding — precisely the autonomous system this design rejects. The profiler suggests; the developer decides.

#### 5.4.11 Unit canonicalisation (D29)

**`characteristic_id` is canonical and authoritative. The unit string is derived and advisory.** Store both.

A unit string cannot be converted; a characteristic can — it is the key that makes `sourceCharacteristicId` → `targetCharacteristicId` conversion possible (§5.3.1). Where `unit_source` is `inferred`, `characteristic_id` may legitimately be `null`. **Represent that honestly; never fabricate one**, because a fabricated characteristic ID silently enables a wrong server-side conversion.

Resolution order: `Characteristic.display_unit` → `ContentVariable.unit_reference` → inference from magnitude → `unknown`. Inference is the fallback for incomplete device types only. The profiler *selects* a target characteristic; `timescale-wrapper` evaluates the formula.

Two capabilities the ontology unlocks: **automatic conversion** via `Concept.conversions` (prefer the lowest `distance` path), and **range validation** against `Characteristic.min_value`/`max_value` — a quality flag with a defensible basis rather than a heuristic.

#### 5.4.12 `SeriesProfile` schema

```json
{
  "profile_id": "...", "tier": "full",
  "series_ref": {"device_id": "...", "service_id": "...", "variable_path": "..."},
  "detector_version": "1.0.0",
  "analysis_window": {"from": "...", "to": "..."},
  "raw_window": {"from": "...", "to": "...", "source": "default | developer_override"},
  "computed_at": "...",

  "service_context": { "...": "see §5.4.1" },

  "coverage": {"n_points": 0, "expected_points": 0, "completeness_ratio": 0.0},

  "sampling": {
    "detected_interval_s": 900,
    "regularity": "regular | irregular | mixed",
    "confidence": "certain | likely | uncertain",
    "gaps": [{"from": "...", "to": "...", "duration_s": 0,
              "classification": "device_offline | sensor_fault | ingestion_gap | unknown"}]
  },

  "value_semantics": {
    "kind": "instantaneous | cumulative_counter | binary | categorical | status",
    "kind_confidence": "likely",
    "kind_evidence": {"monotonic_ratio": 0.0, "distinct_values": 0},
    "characteristic_id": "... | null",
    "unit": "W",
    "unit_source": "characteristic | unit_reference | inferred | unknown | conflict",
    "declared_range": {"min": 0, "max": 0},
    "range_violation_ratio": 0.0,
    "counter_resets": ["..."],
    "available_conversions": [{"to_characteristic_id": "...", "to_unit": "kW", "distance": 1}]
  },

  "distribution": {
    "min": 0, "max": 0, "mean": 0, "median": 0, "p01": 0, "p99": 0,
    "zero_ratio": 0.0,
    "constant_runs": [{"from": "...", "to": "...", "value": 0, "duration_s": 0}]
  },

  "temporal_structure": {
    "dominant_periods_s": [86400, 604800],
    "trend": {"slope": 0.0, "significant": false},
    "stationarity": {"adf_p": 0.0}
  },

  "activity_pattern": {
    "classification": "continuous | session_based | intermittent | status",
    "classification_confidence": "likely",
    "idle_level": 0.0, "active_threshold": 0.0,
    "session_stats": {"count": 0, "median_duration_s": 0,
                      "inter_arrival_median_s": 0, "median_energy": 0.0},
    "session_exemplars": [{"from": "...", "to": "...", "duration_s": 0,
                           "energy": 0.0, "peak": 0.0}],
    "sessions_ref": "/profiles/{profile_id}/sessions"
  },

  "quality_flags": [{"flag": "frozen_sensor", "confidence": "certain",
                     "evidence": {"longest_constant_run_s": 0}}],

  "recommendations": {
    "advisory": true,
    "resample_to_s": 900,
    "interpolation_strategy": "none | linear | ffill",
    "usable_range": {"from": "...", "to": "..."},
    "exclusions": [{"from": "...", "to": "...", "reason": "..."}]
  },

  "provenance": { "...": "see §5.4.4" }
}
```

Any field above may instead carry the `not_computed` object of §5.4.6.

#### 5.4.13 Detector build order

1. **Sampling interval** — modal inter-arrival delta, irregularity ratio, gap list. **Raw pass.**
2. **Value semantics** — highest-impact detector. `cumulative_counter` when monotonic ratio > 0.95; detect resets via large negative deltas. Misreading a cumulative kWh counter as instantaneous power produces silent garbage. **Raw pass.**
3. **Unit resolution** — ontology first, inference as fallback (§5.4.11). **No read.**
4. **Gap classification** — correlate gaps with `GET /devices/{id}/connection-state` history. A gap while the device was offline is *expected*, not a sensor fault. Materially reduces false quality flags.
5. **Interaction check** — a `Service` with `interaction: "request"` is polled, not streamed. Confirm `event` or `event+request` before treating the variable as a time series.
6. **Periodicity** — ACF peaks plus FFT; report daily and weekly explicitly. **Aggregated pass.**
7. **Session detection** — bimodal KDE or Otsu for the idle/active split, hysteresis, minimum duration, sub-threshold gap merging. All parameters developer-adjustable. **Raw or fine bucket** — coarse buckets destroy short sessions.
8. **Cross-variable relationships** — §5.4.1, using the service-scoped batch.
9. **Quality flags** — frozen sensor, negative values on unsigned quantities, DST ambiguity, range violation.

**Time handling is not optional.** Store and compute in UTC, display in local time, flag DST transition windows. Silent DST bugs in 15-minute meter data are a recurring failure mode in this domain.

#### 5.4.14 Numerics in Go (D30)

Go has no SciPy. This is the one place where D30 costs effort rather than saving it, so it is specified rather than discovered during M1b.

| Detector | Implementation |
|---|---|
| Modal inter-arrival, gaps, monotonic ratio, counter resets, zero ratio, constant runs | Plain Go. No library needed |
| Percentiles, mean, median, variance | `gonum.org/v1/gonum/stat` (BSD-3-Clause) |
| Periodicity — FFT | `gonum.org/v1/gonum/dsp/fourier` |
| Periodicity — ACF | `stat.AutoCorrelation`, peak-picking in plain Go |
| Idle/active split — Otsu | ~40 lines of plain Go; the histogram formulation is elementary |
| Idle/active split — KDE | Gaussian kernel over a fixed grid, plain Go. Silverman bandwidth |
| **Stationarity — ADF** | **No Go implementation exists.** See below |

**ADF is the only genuine gap.** The augmented Dickey-Fuller test needs an OLS regression on lagged differences plus MacKinnon critical values. `gonum/stat/regression` covers the OLS part; the MacKinnon surface has to be tabulated.

Do not fake it. `temporal_structure.stationarity` carries the `not_computed` object of §5.4.6 with `reason: "out_of_scope"` until it is implemented deliberately, and D24 exists precisely so that an absent field is read as *"could not determine"* rather than *"stationary"*. Implementing ADF is a discrete, testable task with published reference values — schedule it inside M1b, do not let it block the other eight detectors.

**Verification.** Detector correctness is checked against fixtures with known answers, not against the platform: a synthesised 15-minute series with an injected gap, a monotonic counter with two resets, a bimodal washing-machine load. This is what makes the profiler testable without an LLM (§4) and without the cluster.

### 5.5 `relations/` — multi-device conditional patterns

Motivating case: *"the oven being on while the kitchen lights are off is an anomaly, except at certain times of day."*

**The aspect hierarchy solves candidate selection.** Rather than making the developer pick devices manually, propose device sets from a shared `AspectNode` subtree — devices under aspect "Kitchen" yield oven and lights automatically. Also check `/device-groups` and `/graphs` for existing groupings before constructing new ones.

```go
func ProposeRelatedSets(ctx context.Context, aspectID string, includeDescendants bool) ([]CandidateSet, error)
// Align issues ONE batched POST /queries/v2 with identical groupTime across elements.
func Align(ctx context.Context, refs []SeriesRef, gridSeconds int) (AlignedFrame, error)
func DeriveState(s Series, p ResolvedProfile) StateSeries // from activity_pattern
func Relate(states []StateSeries, conditioning Conditioning) RelationProfile
```

**Alignment is server-side** (§5.3.1). Issue a single batched query with the same `groupTime` for every member rather than resampling client-side. Choose the bucket from the coarsest member's detected sampling interval; a bucket finer than the slowest series produces spurious idle states.

`RelationProfile` as in v1.0: pairwise contingency with lift/confidence/support, conditioned on `hour_of_day_bucket` and `weekday_weekend`, plus `candidate_rules` carrying explicit `exceptions` windows.

Rules are **candidates**. The developer confirms, rejects, or edits. Confirmed rules become features or anomaly definitions in the operator.

### 5.6 `kernel/` — execution via existing JupyterHub (D2, revised)

**Do not build a bespoke pod spawner or a workspace persistence layer.** The existing Kubernetes JupyterHub already provides everything the v1.0 `PodKernel` was going to build, and two prerequisites are **already satisfied in the current deployment**:

| Prerequisite | Status |
|---|---|
| Authentication against the same Keycloak instance | **Confirmed present.** No authenticator work; identity is aligned by construction |
| Per-user PVC auto-mounted | **Confirmed present.** Workspace and repo clone persist across sessions with no ODE-side storage |

**What comes free:** per-user isolated pods (KubeSpawner), resource limits, lifecycle and idle culling, image control, aligned identity, and persistent per-user storage.

**Consequences for ODE:**

- No `SubprocessKernel` fallback is needed. Build directly against the Hub from M4.
- The repo working copy (§5.11) lives on the user's PVC at a stable path, e.g. `~/ode/{repo-name}`. ODE clones once and reuses it; a returning developer resumes an existing checkout rather than re-cloning.
- Session state that must survive a pod restart belongs on the PVC, not in ODE's session store. Keep ODE's `session/` for conversation, confirmations and quota only.
- Because the Hub username derives from the same Keycloak subject as the ODE token, no username mapping table is required — resolve the Hub user directly from the validated token claim.

**Integration:**

```go
type KernelBackend interface {
    EnsureServer(ctx context.Context, user User) (ServerURL, error)   // POST /hub/api/users/{name}/server
    MintToken(ctx context.Context, user User) (HubToken, error)       // POST /hub/api/users/{name}/tokens
    StartKernel(ctx context.Context, s ServerURL, t HubToken) (KernelHandle, error) // /user/{name}/api/kernels
    Execute(ctx context.Context, h KernelHandle, code string) (<-chan ExecutionEvent, error) // WS stream
    Interrupt(ctx context.Context, h KernelHandle) error
    Shutdown(ctx context.Context, h KernelHandle) error
}
```

ODE registers as a **JupyterHub service** with `admin:users`, `admin:servers`, `access:servers` scopes.

**Kernel protocol in Go.** There is no `jupyter_client` equivalent, but none is needed: the Hub proxies kernels over a WebSocket at `/user/{name}/api/kernels/{id}/channels`, and the wire format is documented JSON messages (`execute_request` → `stream` / `execute_result` / `display_data` / `error` / `status`). Implement against `gorilla/websocket` as a typed message struct plus a dispatch loop — a few hundred lines, no ZeroMQ, because the Hub's WebSocket layer already bridges it. Signing (HMAC over message parts) is not required on the WebSocket path; the Hub token authorises the connection.

**What still needs doing — four items, none blocking:**

1. **Custom singleuser image** containing Operator Lib (latest, per D15), `ray[client]`, `mlflow`, pandas, and the ontology/timeseries clients. Version alongside Operator Lib; rebuild on release. Decide whether this replaces the current image for all users or is offered as an additional profile — **an additional KubeSpawner profile is preferable**, so existing notebook users are unaffected.
2. **NetworkPolicy on singleuser pods.** The one genuine gap: JupyterHub isolates users from one another but does not by itself restrict egress. Restrict to device-repository, timescale-wrapper, MLflow, Ray, `ghcr.io` and PyPI. **This is the residual former-M9 and it does not go away** — it is now the only hard security prerequisite before external users.
3. **Idle culling exceptions.** Send keep-alives during an active ODE session, or configure a cull exception for ODE-spawned servers. The PVC preserves files, but a culled pod loses in-memory kernel state mid-task.
4. **Platform token refresh.** Spawn-time env vars cannot be refreshed, so push the current Keycloak token into the kernel via a hidden `execute` at session start and on each refresh.

**Spawn latency** remains a UX consideration rather than a work item: cold start is 10–60s, so pre-warm on session open, show progress, and never block the chat pane on spawn.

The kernel inherits exactly the user's data authorisation — developer code and LLM-authored code have identical, non-escalating access.

### 5.7 `llm/` — provider abstraction (D7)

A `Provider` interface with `Stream()` and `Capabilities()`; normalise all providers to one internal event stream (`text_delta`, `tool_call`, `tool_result`, `done`, `error`); provider-specific shapes must not leak upward. Streamed to the SPA over SSE.

| Implementation | Transport | Tools via |
|---|---|---|
| `AnthropicProvider` | `anthropics/anthropic-sdk-go` (MIT) | native tool use |
| `OpenAIProvider` | `openai/openai-go` (Apache-2.0) | native function calling |
| `OpenAICompatibleProvider` | plain HTTP (vLLM, Ollama, Azure) | native, capability-gated |
| `AnthropicCLIProvider` | `os/exec` wrapping local `claude` | **MCP** |

**CLI provider.** Expose ODE's tool surface as an MCP server (`mcp/`) and point the CLI at it, rather than fighting the CLI's own tool loop. Same tool definitions serve both transports. Invoke in streaming JSON output mode; map events onto the internal stream.

**Known risk.** CLI tool-calling parity is unverified. Probe capabilities at startup; degrade to text-only advisory mode if MCP invocation fails. Must not block M2 — this is a development convenience, not a production path.

### 5.8 LLM tool surface — allow-list

Publish as a paper table.

| Tool | Effect | Min tier | Confirm |
|---|---|---|---|
| `search_ontology` | read aspects/functions/characteristics | L0 | no |
| `resolve_semantic_selection` | read, semantic query | L0 | no |
| `list_devices` | read | L0 | no |
| `get_device_metadata` | read | L0 | no |
| `probe_availability` | read `/data-availability` | L0 | no |
| `estimate_read_cost` | read `/usage/devices` | L0 | no |
| `quick_profile` | assemble `QuickProfile`, no series read | L0 | no |
| `profile_series` | compute `SeriesProfile` (service-scoped batch read) | L1 | no |
| `get_sessions` | read paginated session resource | L1 | no |
| `propose_related_sets` | read ontology | L0 | no |
| `relate_series` | compute + read | L1 | no |
| `preview_series` | read values | L2 | no |
| `render_chart` | emit chart spec | L1 | no |
| `propose_data_selection` | write session state | L0 | **yes** |
| `write_file` | write repo working copy | L0 | no |
| `run_code` | execute in kernel | L0 | **yes** |
| `launch_experiment` | submit Ray job | L0 | **yes** |
| `get_experiment_results` | read MLflow | L0 | no |

**Denied, enforced server-side — no tool exists:** modifying evaluation criteria, modifying the Operator Lib, deploying to a production pipeline, deleting platform data, writing to the timeseries store, changing the exposure tier, changing admin limits, **writing a `ProfileOverride`**, and **promoting a `recommendations` value to binding config** — the last two are developer actions only (D21, D28).

### 5.9 Chart specification

Unchanged from v1.0. The LLM emits declarative specs; the frontend renders. **Never images.**

```json
{
  "chart_id": "...", "title": "...",
  "series": [{"ref": {...}, "transform": "none | diff | rate | resample:900s | convert:{characteristic_id}", "label": "..."}],
  "annotations": [{"type": "span", "from": "...", "to": "...", "label": "detected session",
                   "severity": "info | warn | error", "source": "profiler.sessions", "confirmable": true}],
  "markers": [{"at": "...", "label": "counter reset"}],
  "y_axis": {"unit": "W", "unit_source": "characteristic"},
  "caption": "..."
}
```

All transforms map onto `POST /queries/v2` fields rather than client-side computation: `resample:` → `groupTime` + `groupType`, `convert:` → `sourceCharacteristicId`/`targetCharacteristicId`, `diff`/`rate` → `groupType` or `math` (§5.3.1).

### 5.10 Interactive confirmation (D11)

Confirmable items: inferred units (only where `unit_source` is `inferred` or `conflict`), activity classification, session boundaries and thresholds, candidate relational rules, recommended usable range and exclusions, gap classifications.

Confirmations persist as session overrides, are re-injected into subsequent profiles, and are recorded in the artifact. **A paper contribution — "human confirmation of derived semantics" — not merely a UI affordance.** Note that the ontology reduces how often confirmation is needed, which strengthens rather than weakens the design argument.

### 5.11 `repo/` — GitHub integration (D9, D14)

1. GitHub OAuth (web flow), scopes `repo` and `workflow`. Token stored per user, encrypted, separate from the Keycloak session.
2. Developer selects an existing repository or creates one from template.
3. **Template contents:** operator skeleton conforming to Operator Lib (pinned to latest at scaffold time, D15); `Dockerfile`; `.github/workflows/build.yml` building and pushing to **`ghcr.io`**; `operator.yaml`; test scaffold; `evaluation.yaml` (developer-owned criteria).
4. **Registry is `ghcr.io` by default and is changed by editing the workflow file** — ODE does not manage it as configuration state.
5. Clone into the **auto-mounted JupyterHub per-user PVC** at a stable path (e.g. `~/ode/{repo-name}`). On return, reuse the existing checkout — fetch and report divergence rather than re-cloning. The Code pane must expose a **full file tree with read/write on every file** (D14) — no hidden or ODE-reserved files. Explicit commit and push actions; never silent commits.
6. Because the workspace is persistent, handle the case of **uncommitted local changes from a previous session**: surface them on reopen and let the developer commit, stash or discard. Do not silently reset.
7. **Every experiment records the commit SHA as an MLflow tag.** Each run is reproducible from a specific code state — a design principle worth stating in the paper.

### 5.12 `experiments/` — Ray and MLflow

- **Ray:** submit via the Job Submission API with the repo working directory. Metadata carries `session_id`, `user_sub`, `commit_sha`, `mlflow_run_id`. No per-user namespace work (D18). Jobs read training data **directly from timescale-wrapper** with their scoped token, or via a `/prepare-download` secret — never streamed through the ODE backend (§5.3.4).
- **MLflow:** one experiment per ODE project, namespaced per user (D17); one run per job.
- **Embed probe (D6):** on pane open, attempt to load the target UI in a hidden iframe. On framing failure (CSP / `X-Frame-Options`), record per service and render a link-only card with "Open in new tab". Cache; re-probe on config change. Panes support **embed / hide / pop-out** regardless of outcome.

### 5.13 Result interpretation

On completion, the backend pulls metrics and params from MLflow, builds a **compact structured summary** (never raw logs), injects it into chat context; the LLM interprets and proposes the next adjustment. The developer accepts, edits, or rejects.

```json
{
  "run_id": "...", "commit_sha": "...", "status": "finished",
  "params": {}, "metrics": {},
  "comparison_to_previous": [{"metric": "...", "delta": 0.0, "direction": "better|worse"}],
  "evaluation_criteria": {"metric": "...", "threshold": 0.0, "met": false},
  "resource_usage": {"duration_s": 0, "peak_memory_mb": 0}
}
```

### 5.14 Registration (D10)

After a successful CI build, offer "Register with Analytics Stack" as an explicit developer action, called with the user's token. Never automatic.

---

## 6. Build order

| M | Deliverable | Accept when |
|---|---|---|
| **M0** | Backend skeleton; Keycloak validation + `developer` gate; proxy; ontology client + cache; on-behalf-of device read; one chart | A logged-in developer sees only permitted devices; aspect tree and functions load |
| **M1a** | timescale-wrapper client (availability, batched v2 queries, usage); **`QuickProfile`**. **No series reads, no LLM.** | 40 candidate series ranked from metadata alone; zero value reads confirmed in logs |
| **M1b** | `SeriesProfile`: detectors 1–9 (§5.4.13), two-pass read strategy, provenance, `not_computed` handling, immutable store + override overlay, projection, sessions resource. **No LLM.** | Profiles for 10 real series correct under inspection; counter-vs-instantaneous never misclassified; units resolve from characteristics; a low-coverage series yields `not_computed` rather than a number; an override survives recomputation |
| **M2** | Semantic selection (`resolve_semantic_selection`), `ontology_gaps` reporting | A natural-language intent resolves to concrete series through the ontology, entirely at tier L0 |
| **M3** | Provider abstraction; MCP tool server; chat; tool dispatch; **exposure tiers**; admin limits | L0 provably blocks value-bearing tools; provider swap requires no call-site change; a per-user cap is enforced |
| **M4** | JupyterHub integration: ODE service registration, spawn, kernel, execute, token refresh, keep-alive; singleuser image profile | Developer runs arbitrary Python against permitted series in their own pod; a file written in one session is present in the next |
| **M5** | Exploration pane: chart specs, annotations, confirmation controls, unit conversion | LLM proposes a selection and demonstrates it visually; developer confirms an inferred unit |
| **M6** | Session detection + relational profiler + aspect-scoped candidate sets | Oven/lights-style rule surfaces from the aspect tree, with exceptions, developer-confirmable |
| **M7** | Code pane (Monaco), full file tree, GitHub OAuth, repo select/create, scaffold, commit/push | A compliant operator repo is created and pushed; Actions builds and pushes to ghcr.io |
| **M8** | Ray submission, MLflow, embed probe with fallback | Experiment launches; run tagged with commit SHA; framing failure degrades to a link |
| **M9** | Automated result interpretation in chat | Completed run produces an interpretation and a concrete next proposal |
| **M10** | NetworkPolicy on singleuser pods; egress allow-list | **Required before external users — the only hard security prerequisite.** Session code cannot reach unlisted egress targets |

**M1 before everything LLM-related is deliberate.** The profiler is deterministic, independently testable, and most likely to overrun. LLM work can proceed against fixture profiles in parallel.

**M2 before M3 is new in v1.1.** Semantic selection is pure ontology work and needs no LLM; proving it standalone de-risks the most novel component.

---

## 7. Risk register

| Risk | Severity | Mitigation |
|---|---|---|
| Egress control on singleuser pods | **High** | M10 mandatory before external users; NetworkPolicy is not provided by JupyterHub. Now the only hard security prerequisite |
| Singleuser image change disrupts existing notebook users | Medium | Ship as an additional KubeSpawner profile rather than replacing the default image |
| Stale uncommitted work on the persistent PVC | Low | Surface divergence on reopen; never silently reset (§5.11) |
| Session/relational detection quality without labels | **High** | Interactive confirmation reframes as design, not accuracy; do not claim detector accuracy in the paper |
| Aggregated reads mask gaps and irregularity | **High** | Two-pass raw/aggregated read strategy (§5.3.2); record read mode in `provenance` per field |
| LLM reads a missing field as a negative finding | **High** | Explicit `not_computed` with reason (D24); never null, never absent |
| Stale profiles in context after a detector change | Medium | `detector_version` in the cache key (D25) |
| Projection hides material detail from the LLM | Medium | `elided` block records totals and a fetch reference (D26) |
| `groupType`/`groupTime` accepted values undocumented | Medium | Runtime probe and cache (§5.3.5) |
| No SciPy in Go for profiler numerics (D30) | Medium | `gonum` covers stats, FFT and OLS; Otsu and KDE are short and testable; ADF is `not_computed` until deliberately implemented (§5.4.14). Fixture-based verification, not platform-based |
| ODE reachable in-cluster, bypassing the gateway that authenticates tokens | **High** | Token validation is centralised at the API gateway (§3.1), so a direct in-cluster call is unauthenticated. M10's NetworkPolicy is what closes this, and singleuser pods running developer code make it concrete. Raise M10 if ODE is exposed before it |
| Incomplete device types | Medium | Downgraded by ontology use; `ontology_gaps` surfaces per device type at runtime |
| `claude` CLI tool-calling parity | Medium | Capability probe; degrade to advisory mode; dev-only path |
| Ray/MLflow refuse framing | Medium | Runtime probe with link fallback — test in week 1 |
| Idle culling kills kernel state mid-task | Medium | Keep-alive during active session or cull exception. Files are safe on the PVC; only in-memory state is lost |
| Spawn latency degrades first impression | Low | Pre-warm on session open; never block chat on spawn |
| Token expiry vs. long Ray jobs | Medium | Short-lived scoped token minted at submission |

---

## 8. Open items

### 8.1 Blocking

**None.** Both prior blockers are closed: the timeseries specification is integrated (§5.3), and JupyterHub is confirmed to share the Keycloak instance and auto-mount per-user PVCs (§5.6). **M0 is buildable.**

Two configuration details to obtain during M0, neither gating design work:

- Whether an ODE service can be registered with `admin:users`, `admin:servers`, `access:servers` scopes, or whether per-user token minting must go through an existing admin token.
- Current KubeSpawner profile list and resource defaults, to size the ODE profile (§5.6, item 1).

### 8.2 Non-blocking

- Whether `/graphs` already contains a device relationship graph worth seeding §5.4 from.
- Which `Attribute` keys are in practical use on devices and device types (possible carriers for PV capacity, orientation, nominal power).
- Operator Lib interface surface at current HEAD, for scaffold generation.
- Whether existing `DeviceGroup` definitions cover common analysis groupings.
- Accepted `groupType` / `groupTime` values, and whether a `difference` aggregate exists for server-side counter differencing (§5.3.5).
- Whether `deviceGroupId` and `locationId` query modes are populated in practice — if so, they shortcut parts of §5.5.
- PVC size quota per user, and whether cloned repos plus cached extracts risk exhausting it.
- Whether the existing cull timeout is short enough to require an explicit exception or merely keep-alives.
- Retention and continuous-aggregate policy per device type, to predict when `/data-availability` reports coarse-only windows.