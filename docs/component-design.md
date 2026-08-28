# Component design

What each package is for and what it must guarantee, component by component. The
decisions behind it are in [decisions.md](decisions.md) as D1 to D30; the
implementation detail that moves when the code does is in the subject documents
indexed in [README.md](../README.md), and this file points at them rather than
repeating them.

## Applies when

Adding to a package, or checking what a component was specified to do before
changing what it does. Sections are addressed as §5.x from the code — see the
note in [decisions.md](decisions.md) on why the numbering does not move.

**Not this if**: the question is how a component *fails*. The failure modes are
the subject documents' business: [profiler-reads.md](profiler-reads.md) for the
two-pass read, [authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md)
for the tier gate, [chat-and-streaming.md](chat-and-streaming.md) for a detached
turn.

`geltung`: `allgemein` for the specifications; where a section names a version or
a deployment property, that part is `einzelfall`.

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

**Permission nuance.** `models.Read` ('r') governs device *metadata*; `models.Execute` ('x') governs *reading device data*. Listing devices is a `Read`; every timeseries read must be scoped to `Execute`, or ODE will offer series it cannot actually read.

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

**Why this is more than a lookup.** The semantic layer here is a hierarchical aspect tree plus a function ontology plus a characteristic layer that carries executable unit conversions — three dimensions, not an entity/signal pair. That is what makes an intent resolvable to a series without reading a single device name.

#### 5.2.1 Imports as the second kind of operator input

An operator's input is not always a device. The platform also has **imports** —
containerised adapters that pull data from outside and publish it to one Kafka
topic — described by an **import type** the way a device is described by a device
type, and carrying the same content variables with the same `function_id`,
`aspect_id` and `characteristic_id`. Semantic selection therefore applies
unchanged, and one `resolve_semantic_selection` answers with both halves:
`import_selectables` beside `selectables`, `import_candidates` beside
`candidate_devices`.

Discovery goes through **device-selection**
(`POST /v2/query/selectables?include_imports=true&import_path_trim_first_element=true`),
not import-repository directly. That service already does four things a direct
caller would have to reimplement, each of which produces a silently short answer
when wrong: import-repository does not expand an aspect criterion over its
subtree the way the device repository does; the criteria index is flattened per
import type, so a *type* matches and the matching *paths* must be found by
walking its output; import-deploy has no filter by import type, so
type-to-instance is a client-side join; and an import type's output describes the
whole Kafka message rather than only its payload, so every path needs its first
element trimmed before it addresses anything.

Devices are **not** read through the same endpoint. Its device answer drops the
connection state — a `QuickProfile` input — along with the device type name and
the full device type, and hard-codes `shared` to false, so ODE would have to read
the devices again anyway.

Three consequences shape the answer:

1. **An import type has no device class, and every import path is an event.** A
   resolution that narrows by `device_class_id` therefore cannot include imports
   at all, and says so in `notes` rather than returning a device-only answer that
   looks complete.
2. **A path is one level deeper than a device's.** An import type's `output`
   covers the whole message — `import_id`, `time`, `value` — so its real
   variables sit under a payload node, and the addressable form is
   message-relative (`value.temperature`). `import_id` and `time` are content
   variables but not series.
3. **An import has no stored series unless somebody exported it.**
   timescale-wrapper addresses a series by device and service, by device group, by
   location or by export — there is no import id — and the table worker that
   materialises device tables handles devices only. So an import's history exists
   in timescale if and only if an analytics-serving export was created for it, and
   is addressed by that export's id, with **column names chosen at export time
   rather than derived from the content variable**. Each import candidate reports
   `exported`, `live_only` or `unknown`; collapsing the last two would be the real
   defect, because "no history" is actionable and "could not find out" is not the
   same claim.

Because of (3), imports are reported but never folded into the ranked
`candidates`: a ranking that mixed them would compare a measured span against
nothing at all.

**A fourth consequence is the one discovery cannot express at all.** device-selection
joins each matching type to its instances and emits one selectable per instance,
so an import type nobody has deployed yet produces nothing — and an empty
`import_candidates` therefore has two causes that look identical: the platform
describes nothing of this kind, or it describes it and nobody has deployed an
import for it. Only the second is actionable, and it is the case
`create_import_instance` exists for. So a resolution also asks
import-repository's own `GET /import-types` with the same criteria and reports
what matched with no instance in this answer, as `deployable_import_types`
beside the two lists above. Two things about that endpoint are the caller's to
absorb rather than device-selection's: it ANDs its criteria, so ODE sends one per
combination and unions the answers, and it matches aspect ids literally, so ODE
sends the aspect node together with its descendants. `list_import_types` is the
same read by name or by id (§5.8).

**Wiring.** A resolved import variable becomes an operator input through
`propose_operator_input` (§5.8), which emits the flow engine's node input:
`filterType: "ImportId"` — compared exactly upstream, with a silent fallback to a
device filter — the instance id as `filterIds`, the instance's `kafka_topic` as
`topicName`, and one `{name, path}` mapping per bound variable. Both operator
libraries strip the message envelope themselves, so the mapping path is the
message-relative one. The Python library ODE scaffolds routes historic data to
timescale only for a device topic and replays Kafka for everything else, which is
what makes a `live_only` import trainable at all.

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
| `POST /usage/exports` | The export half of `estimate_read_cost`. Same shape keyed on `exportId`; POST despite the annotation, as `/usage/devices` is |

**An export has no availability endpoint, and that shapes the export half.**
`/data-availability` takes a `device_id` and nothing else, so the window a device
profile is bounded by cannot be asked for an export. Two facts fill the gap, and
neither is a substitute on its own:

- `/usage/exports` says what is stored in bytes. It cannot say whether a row
  exists: the figures come from a usage table a collector fills per timescale
  table, so a young export has no row at all, and a table whose rows are all null
  has a size like any other.
- One bucketed `count` per column over `POST /queries/v2` says how many rows carry
  a value, per column, and which buckets they fall in. That answers the window
  *and* the question a developer actually has after creating an export — whether
  anything is in it — and it reads no value, which is why it sits at L0 beside
  `probe_availability` (see
  [authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md)).

`profiler.ExportFill` is that probe, `probe_export_data` publishes it, and
`ProfileExport` uses its span where the device path uses the availability window.
The four states it reports — `filled`, `partly_filled`, `empty`, `unknown` — are
kept apart for the reason `History`'s three are: `partly_filled` is an export whose
value paths resolve against the message root, which stores rows in which every
column is null, and `unknown` is not `empty`.

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

Three reasons this matters: recomputation is non-destructive, so improving a detector or widening a range preserves confirmations; computed-versus-confirmed stays diffable; and **the override log is an empirical record** — "detector said X, developer corrected to Y" is evidence about the detector, and a mutable document destroys it.

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

#### 5.4.12 Schema, detector order and numerics

Extracted to [docs/profiler-detectors.md](profiler-detectors.md): the
field-by-field `SeriesProfile` shape, the order the detectors were built in, and
which numerical methods `gonum` covers against which are deliberately
`not_computed`. Reference detail rather than decisions, and it changes when a
detector does — which is why it is not here.

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
- **A developer holds several of these at once (D32).** One *workbench* is one checkout plus one kernel plus its own busy state, and each chat session names the one it acts in. The kernels are several processes in the one singleuser pod — no JupyterHub configuration, but the spawner profile's memory has to carry them. See [kernel-and-repository.md](kernel-and-repository.md), which also has the rejected alternatives.
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

ODE registers as a **JupyterHub service** whose token holds `servers`, `tokens`, `access:servers` and `users:activity` — `pkg/kernel.RequiredScopes`, checked at startup. Deliberately not the `admin:*` scopes: `tokens` and `access:servers` are what minting a per-user token and reaching `/user/{name}/api/*` need, and admin over every Hub user is authority ODE has no use for.

**Kernel protocol in Go.** There is no `jupyter_client` equivalent, but none is needed: the Hub proxies kernels over a WebSocket at `/user/{name}/api/kernels/{id}/channels`, and the wire format is documented JSON messages (`execute_request` → `stream` / `execute_result` / `display_data` / `error` / `status`). Implement against `gorilla/websocket` as a typed message struct plus a dispatch loop — a few hundred lines, no ZeroMQ, because the Hub's WebSocket layer already bridges it. Signing (HMAC over message parts) is not required on the WebSocket path; the Hub token authorises the connection.

**What still needs doing — four items, none blocking:**

1. **Custom singleuser image** containing Operator Lib (latest, per D15), `ray[client]`, `mlflow`, pandas, and the ontology/timeseries clients. Version alongside Operator Lib; rebuild on release. Decide whether this replaces the current image for all users or is offered as an additional profile — **an additional KubeSpawner profile is preferable**, so existing notebook users are unaffected.
2. **NetworkPolicy on singleuser pods.** The one genuine gap: JupyterHub isolates users from one another but does not by itself restrict egress. Restrict to device-repository, timescale-wrapper, MLflow, Ray, `ghcr.io` and PyPI. **This is M10 and it does not go away** — it is now the only hard security prerequisite before external users.
3. **Idle culling exceptions — two different cullers.** The Hub's culler stops pods, and ODE's keep-alive addresses it: send keep-alives during an active session, or configure a cull exception for ODE-spawned servers. The PVC preserves files, but a culled pod loses in-memory kernel state mid-task. The singleuser server has a *second* culler of its own, `MappingKernelManager.cull_idle_timeout`, which stops kernels rather than pods and hears nothing from the keep-alive — with several workbenches in one pod it silently kills whichever one the developer is not currently typing in, so it has to be off or long.
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

**Confirmed tools over MCP.** The CLI's loop has no pause for D11 to use, so a confirmed call is *held open* on the MCP request while the developer decides, and their answer becomes the tool's result. The waiting is ODE's, not the client's — see [chat-and-streaming.md](chat-and-streaming.md). Bounded by `chat_confirmation_timeout`, which has to fit inside one CLI turn. Where there is no turn in flight to ask on, the call is refused rather than left waiting.

**Known risk.** CLI tool-calling parity is unverified. Probe capabilities at startup; degrade to text-only advisory mode if MCP invocation fails. It must not hold up the LLM surface — this is a development convenience, not a production path.

### 5.8 LLM tool surface — allow-list

The whole surface, in one table, because the omissions are the point.

| Tool | Effect | Min tier | Confirm |
|---|---|---|---|
| `search_ontology` | read aspects/functions/characteristics | L0 | no |
| `resolve_semantic_selection` | read, semantic query | L0 | no |
| `list_devices` | read | L0 | no |
| `get_device_metadata` | read | L0 | no |
| `list_import_instances` | read | L0 | no |
| `get_import_type_metadata` | read | L0 | no |
| `list_import_types` | read | L0 | no |
| `probe_availability` | read `/data-availability` | L0 | no |
| `probe_export_data` | read `/usage/exports` and one bucketed row count | L0 | no |
| `estimate_read_cost` | read `/usage/devices` and `/usage/exports` | L0 | no |
| `quick_profile` | assemble `QuickProfile`, no series read | L0 | no |
| `profile_series` | compute `SeriesProfile` (source-scoped batch read: a service, or an export) | L1 | no |
| `get_sessions` | read paginated session resource | L1 | no |
| `propose_related_sets` | read ontology | L0 | no |
| `relate_series` | compute + read | L1 | no |
| `preview_series` | read values (a device's variable or an export's column) | L2 | no |
| `render_chart` | emit chart spec | L1 | no |
| `propose_data_selection` | write session state | L0 | **yes** |
| `propose_operator_input` | emit a pipeline input, no deployment | L0 | **yes** |
| `create_import_instance` | deploy an import container on the platform | L0 | **yes** |
| `create_export` | create an export and the timescale table behind it | L0 | **yes** |
| `delete_import_instance` | remove an import instance this session created | L0 | **yes** |
| `delete_export` | remove an export this session created | L0 | **yes** |
| `write_file` | write repo working copy | L0 | no |
| `run_code` | execute in kernel | L0 | **yes** |
| `launch_experiment` | submit Ray job | L0 | **yes** |
| `get_experiment_results` | read MLflow | L0 | no |

**Imports have no *signal* discovery tool of their own, deliberately.** An
operator's input can be an import as well as a device (§5.2.1), and both are
described by the same content variables — so an import is found through
`resolve_semantic_selection` like anything else. A separate "find imports" tool
would let a model search one kind, get a plausible answer and never learn the
other existed: a coverage gap that never announces itself. `list_import_instances`
and `get_import_type_metadata` are the direct lookup and the type read, which a
criteria query cannot express; `propose_operator_input` is the confirmed action
that turns a resolved import variable into the pipeline input the analytics flow
engine takes.

**`list_import_types` is not an exception to that rule**, and the distinction is
worth stating because it looks like one. It searches the *catalogue* — the
adapters that could be deployed — and structurally cannot answer what data the
platform carries: a type has no instance, no topic, no status and no history.
It exists because every other route to a type id runs through an instance.
Discovery lists the matching types upstream and then joins each to its instances,
emitting one row per instance, so a type nobody has deployed produces nothing at
all — which is the state of every type `create_import_instance` is for. The
coverage gap the rule above guards against is closed from the other side too:
`resolve_semantic_selection` names the matching undeployed types itself, in
`deployable_import_types`, so a model that never calls this tool still learns
they exist. See [imports-as-operator-inputs.md](imports-as-operator-inputs.md).

**The four that change the platform.** Everything else in this table reads, or
emits a document the developer takes somewhere. These four do not, and three
properties make them acceptable rather than a widening of the surface:

- **Confirmed, like every other write.** Held in `ToolDispatcher` before the
  executor, with the exact request in the developer's view. Nothing is created or
  removed on the model's word.
- **Checked against the import type before the request leaves.** Both creations
  read the type first and refuse what upstream would accept and ignore — a config
  name the type never declared, an exported path that is not a leaf of the output.
  Upstream's own refusals name no field, and its worst answers are silent: a
  config it passes to a container that nothing reads, and a 201 with an empty body
  where the caller had no access to the import.
- **A deletion reaches only what this session created.** Both deletions destroy
  stored data — an export's timescale table, an import's Kafka topic — so they are
  bounded by a per-session record of what ODE itself created. Any other id is
  refused, and the refusal is not a judgement the model can argue with: the id is
  not in the session's list. This is what keeps the denial below true.

**A credential is never sent from a chat.** `create_import_instance` refuses an
import type whose configuration includes a credential without a default, and
refuses any credential-shaped value the model supplies. That import is created in
the platform's own dialog, where the developer types the value. The test is a
heuristic on the config name and errs toward refusing, because the confirmation
card shows arguments and cannot edit them — "the developer will notice" is not a
control here.

**An export is not retroactive**, and the tool says so rather than letting a
developer read "history" as the import's whole past: it stores what the Kafka
topic still retains from the chosen offset onward.

**Denied, enforced server-side — no tool exists:** modifying evaluation criteria, modifying the Operator Lib, deploying to a production pipeline, deleting platform data the developer did not just create through ODE, writing to the timeseries store, changing the exposure tier, changing admin limits, **writing a `ProfileOverride`**, and **promoting a `recommendations` value to binding config** — the last two are developer actions only (D21, D28).

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

Confirmations persist as session overrides, are re-injected into subsequent profiles, and are recorded in the artifact. **This is human confirmation of derived semantics, not a UI affordance** — the confirmation is what makes an inferred unit or a session boundary safe to build on. The ontology reduces how often it is needed, which strengthens the design rather than weakening it.

### 5.11 `repo/` — GitHub integration (D9, D14)

1. GitHub OAuth (web flow), scopes `repo` and `workflow`. Token stored per user, encrypted, separate from the Keycloak session.
2. Developer selects an existing repository or creates one from template.
3. **Template contents:** operator skeleton conforming to Operator Lib (pinned to latest at scaffold time, D15); `Dockerfile`; `.github/workflows/build.yml` building and pushing to **`ghcr.io`**; `operator.yaml`; test scaffold; `evaluation.yaml` (developer-owned criteria).
4. **Registry is `ghcr.io` by default and is changed by editing the workflow file** — ODE does not manage it as configuration state.
5. Clone into the **auto-mounted JupyterHub per-user PVC** at a stable path (e.g. `~/ode/{repo-name}`). On return, reuse the existing checkout — fetch and report divergence rather than re-cloning. The Code pane must expose a **full file tree with read/write on every file** (D14) — no hidden or ODE-reserved files. Explicit commit and push actions; never silent commits.
6. Because the workspace is persistent, handle the case of **uncommitted local changes from a previous session**: surface them on reopen and let the developer commit, stash or discard. Do not silently reset.
7. **Every experiment records the commit SHA as an MLflow tag.** Each run is reproducible from a specific code state, which is the whole reason a run is submitted from a commit rather than from a working copy.

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
