# Imports as operator inputs

How ODE finds import types and import instances, how it turns one into a
deployable operator input, and how it creates an import or an export in the first
place. Implemented in [`pkg/imports`](../pkg/imports),
[`pkg/selection/imports.go`](../pkg/selection/imports.go) and
[`pkg/tools/imports.go`](../pkg/tools/imports.go); specified in
[component-design.md](component-design.md) §5.2.1 and §5.8.

**This is also where ODE stops being a reader.** Everything else it does to the
platform is a read; deploying an import and creating an export are the only
writes, and the outgoing dependency they create is recorded under
[what a one-sided change upstream costs](#what-a-one-sided-change-upstream-costs).

## Applies when

ODE resolves a data intent and the platform can satisfy it from an import rather
than a device, a developer names an import directly, or the platform does not
carry the wanted signal yet and one is created for it.

Requires `device_selection_url` and `import_deploy_url`. Beyond those,
`import_repo_url` and `analytics_serving_url` each remove a capability rather
than the surface — and both become load-bearing for the write half, which refuses
rather than sending a request it could not check first. `import_repo_url` is the
more load-bearing of the two: without it there is no route at all to an import
type that has no instance yet, which is the only kind the write half is for. See
[finding a type that has no instance](#finding-a-type-that-has-no-instance). Creating an export needs
four further settings that are a deployment's rather than an import's; see
[configuration.md](configuration.md#creating-an-export).

The behaviour depends on the versions pinned in `go.mod` —
`device-selection v0.0.27`, `import-deploy v0.1.0`,
`analytics-flow-engine/lib v0.0.0-20251112135741` — because three of the four
asymmetries below are properties of those services rather than of ODE.
**analytics-serving is deliberately not among them**, which is the single most
important fact about the export half: its models live in an `internal` package
with no JSON tags, so there is nothing importable to couple to and nothing the
compiler checks. Its wire shape here was read from the source of
`analytics-serving v0.0.15`.

**Not this if**: the question is about the metadata model of imports, the wiring
contract the operator libraries expect, or why an import has no stored history.
Those are platform facts that hold for anything touching imports rather than ODE
decisions, and are documented on the platform side. This document only covers
what ODE does with them.

`geltung`: `einzelfall` — one implementation, verified against the pinned
versions above and against the platform's own callers (web-ui, device-selection),
not against a running platform. That caveat is sharper for the write half than
for the read half: a read that got a contract wrong returns a short or empty
answer, and a write that gets one wrong leaves a container or an export table
behind on a real platform. Nothing in this document has been executed against a
running import-deploy or analytics-serving.

## Which service answers what

| Question | Service | Why not somewhere else |
|---|---|---|
| Which import variables match these criteria, on which instances | device-selection `POST /v2/query/selectables` | It already expands the aspect subtree, walks the output tree, joins type to instance and trims the path. Doing that against import-repository directly means reimplementing four things whose failure mode is a silently short answer. |
| Is this instance running | import-deploy `GET /instances` | A selectables answer carries no container status at all. |
| One import type by id | import-repository `GET /import-types/{id}` | Only for a lookup with no criteria to send; discovery returns the type alongside every instance. |
| Which import types exist, deployed or not | import-repository `GET /import-types` | The only route to a type with **no** instance. Selectables are per instance, so a type nobody has deployed is absent from discovery entirely — and that is the only kind there is anything to create. |
| Is any of this import's past stored | analytics-serving `GET /instance` | timescale-wrapper has no `importId`, so the export is the only route. |
| Deploy this import type | import-deploy `POST /instances` | The only route. It mints the id, the Kafka topic and takes the image from the type, so a caller that derived any of them is refused rather than obeyed. |
| Remove an import this session deployed | import-deploy `DELETE /instances/{id}` | Deletes the container **and the Kafka topic**. |
| Store this import in timescale | analytics-serving `POST /instance` | There is no table worker for imports, so an export is the only thing that puts one in timescale at all. |
| Remove an export this session created | analytics-serving `DELETE /instance/{id}` | Tells the export worker to drop the timescale table with it. |
| Which export database may this write to | analytics-serving `GET /databases` | Only when `export_database_id` is unset; the id is minted by that service's own migration and no code here can produce it. |

**Devices are deliberately not read through device-selection**, although the same
endpoint offers them. Its device answer embeds a bare `models.Device`: no
`connection_state` (a `QuickProfile` ranking input), no `device_type_name` (what
`devices.TypeName` uses to tell two devices with the same display name apart), no
full `DeviceType`, and `shared` hard-coded to `false` — a wrong value rather than
a missing one. ODE would have to call `ListExtendedDevices` anyway, so the switch
would cost a third hop for strictly less information. The device half keeps
talking to device-repository.

## The four asymmetries, and who absorbs them

Recorded because they come back the moment anyone considers calling
import-repository directly.

1. **No server-side aspect subtree expansion.** Unlike the device repository,
   import-repository does not expand an aspect criterion over its descendants —
   the caller sends the node plus every descendant id. *Absorbed by
   device-selection* for discovery, and **by ODE for the type catalogue**, which
   talks to import-repository directly: `ontology.AspectSubtreeIDs` expands the
   node from the snapshot ODE already holds, so it costs no request. Sending the
   bare node there is the failure this whole list is about — every import type
   described against a child aspect is missing, and nothing says so.
2. **No device class, and every import path is an event.**
   `ImportTypeFilterCriteria` is `{function_id, aspect_ids}` and nothing else.
   *Not absorbed, and ODE's to report*: a resolution that narrows by
   `device_class_id` cannot include imports at all, and says so in `notes` rather
   than returning a device-only answer that looks complete. See
   `selection.importNotes`.
3. **Type-level match, variable-level paths.** The criteria index is flattened
   per import type, so a *type* matches and the matching *paths* must be found by
   walking its output. *Absorbed by device-selection* for discovery, and **by ODE
   for the type catalogue**, in `imports.MatchingVariables`. It is also the one
   asymmetry that cannot be fully absorbed: a type can match because two of its
   variables carry one criterion each, and then no single variable carries both.
   That type is reported with an empty `matching_variables` and a note saying so,
   rather than dropped — it matched, and reading it is the next step.
4. **No filter by import type on instances.** `GET /instances` has no
   `import_type_id` parameter and its `search` matches the instance name only, so
   type-to-instance is a client-side join over a full listing. *Absorbed by
   device-selection for discovery*; ODE does it itself in
   `imports.InstancesOfTypes` for the direct-lookup path, and in
   `list_import_instances`, which says in its own note that the filter was local
   so `total` counts something other than the page.

## Two implementation decisions worth keeping

**One criterion per request, for imports as for devices.** device-selection ANDs
a criteria list for devices — it comes from the device repository — and ORs it for
imports, in `contentVariableContainsAnyCriteria`. A multi-criterion request would
therefore mean different things to the two halves of one answer. The
cross-product-then-union in `selection/criteria.go` was written for the device
repository's AND; it is now also what makes the two halves comparable, and is a
requirement rather than a style.

The type catalogue lands on the same shape from a third direction:
import-repository ANDs its criteria — one `$elemMatch` per entry over the type's
flattened criteria index — so `[{function A}, {function B}]` asks for a type
carrying both. One per request, unioned by id, is the only form under which the
catalogue and the selectables halves of one answer mean the same thing.

**A local HTTP client rather than `device-selection/pkg/client`.** That client's
`GetSelectablesOptions` has no field for `import_path_trim_first_element`,
although the endpoint accepts the parameter — and without it every path arrives
with the import type's output root still on the front, which addresses one level
too deep and yields nothing. Its methods also take no context, so a resolution
could not be cancelled when the caller disconnects. Its criteria parameter is
`[]models.DeviceGroupFilterCriteria` where the endpoint documents
`[]devicemodel.FilterCriteria`; the two are field-compatible, so the wire form is
identical and ODE keeps one criteria type across both halves.

Adding the option upstream is the better fix. When it lands,
`imports.SelectionClient` goes away. The wire types are shared either way — a
rename in `dsmodel.Selectable` or `idmodel.Instance` breaks this build rather
than a response at runtime.

## The import half runs before the device half's early returns

`Resolver.Resolve` has three `return result, nil` paths where the device side
found nothing. `addImports` is called ahead of all of them, so an intent this
platform can only satisfy from an import is still answered. The device-side note
was reworded for the same reason: "no device on this platform is described as
carrying this", not "nothing of this kind on this platform".

## Imports are reported, never ranked beside devices

`ImportSelectables` and `ImportCandidates` sit beside `Selectables` and
`CandidateDevices`, and no import enters the ranked `Candidates`. A device
candidate is ranked on availability and volume; an import has neither unless
somebody exported it, so a merged ranking would compare a measured span against
nothing at all.

Each import candidate carries three things a selectable cannot answer:

- `running` / `running_known` — three-valued, because discovery sees no status
  and "stopped" is a claim ODE would not have established. Reporting it as such
  sends a developer to restart something that may be running fine.
- `history.state` — `exported`, `live_only` or `unknown`. Collapsing the last two
  is the defect to avoid: "no history" is actionable and "could not find out" is
  not the same claim.
- `history.columns` — the map from variable path to timescale column, which is
  not derivable. An export's column is named by whoever created the export.

Both the status listing and the export listing are **one wide read for the whole
shortlist**, not one per candidate: neither upstream can filter by what is being
asked, so a per-candidate lookup re-reads the same listing every time. That is
what `imports.Histories` exists for beside `imports.History`, and
`Reads.ImportInstances` / `Reads.ImportExports` are both 1 in an answer.

## Reading an export back, which is a different question from having one

`history.state: exported` says an export *exists*. It says nothing about whether
anything is in it, and until the read half below there was no way to ask — which
left the export worker's most common misconfiguration undetectable from ODE. It is
described in [what the two upstreams accept
silently](#what-the-two-upstreams-accept-and-what-they-accept-silently): value
paths that resolve against the message root rather than its payload still find the
envelope's `time`, so **the export deploys, rows land, and every column is null**.
An export in that state is indistinguishable from a healthy one in the export
listing, in `history`, and in stored bytes.

`probe_export_data` (L0) and `profile_series` with an `export_id` (L1) are the
answer, and both start from the same place: `imports.ExportDefinition`, because a
query over an export takes **column names** and they exist only in the export.
That resolution is a bounded scan of the export listing for the reason `History`'s
is — analytics-serving cannot filter by id — and it says when it hit the bound
rather than reporting the export as absent.

The verdict has four states, kept apart on the same principle `HistoryState`'s
three are:

| State | What it means | What to do |
|---|---|---|
| `filled` | rows exist, every readable column carries values | profile it, train on it |
| `partly_filled` | rows exist, and a **named** column is null in all of them — up to and including every column | fix those columns' value paths; the topic and the time path are demonstrably fine, because the rows arrived |
| `empty` | not one row was written in the counted window | check the time path and the topic, or wait if the export is young. The value paths are not the suspect here: a row lands for every message whose *timestamp* resolves, whatever happens to the values |
| `unknown` | the question could not be answered | not `empty`. Read the reason; the two call for opposite advice |

The row that made this worth counting per column is the second one at its extreme:
**every** column null. Rows exist, so the export is consuming its topic and finding
the timestamp, and the count per column is the only thing in the platform that says
the values are missing — stored bytes, the export listing and `history` all report
that export as healthy. It is `partly_filled` rather than `empty` for exactly that
reason, and `buckets_with_rows` in the answer is the evidence: the server groups by
the bucket, so a bucket comes back only where a row falls in it.

Two properties of the answer are worth knowing before relying on it. It reads no
value — the query asks `count` per column per bucket, which is what keeps it at L0
beside `probe_availability` — and `/usage/exports` is carried beside the counts as
what it is: a size, from a usage table a collector fills per timescale table, so an
absent row means a young export rather than an empty one.

Profiling an export is then the device pass over the export's columns, with two
differences that are reported rather than smoothed over: the analysis window comes
from the row count because the platform has no availability endpoint for an export,
and a column carries units only where the import type behind it still declares
them. The detail is in
[profiler-contracts.md](profiler-contracts.md#a-series-is-addressed-two-ways-and-one-of-them-has-no-availability-endpoint).

## Finding a type that has no instance

The gap this closes, stated plainly: **an import type was reachable only through
an instance of itself.** Discovery lists the matching types upstream and then
joins each to its instances, emitting one `Selectable` per *instance*, so a type
nobody has deployed produces nothing at all. `get_import_type_metadata` takes an
id. `create_import_instance` takes an id "as `get_import_type_metadata` reports
it" — which it reports only for an id it was given. The one thing the write half
exists for, deploying an import for a signal the platform does not carry yet, had
no route to the id it needed except a developer pasting one from the platform UI.

`GET /import-types` is the only endpoint that answers independently of instances.
It takes the same `{function_id, aspect_ids}` criteria as the filter behind
discovery, a free-text `search` over the type name, an `ids` list, and reports its
total in `X-Total-Count` rather than in the body — which is why this one listing
does not go through the shared decode helper in `pkg/imports/client.go`.

**Two answers, on purpose.** A resolution reports `deployable_import_types`
itself, and `list_import_types` is the same read asked directly:

| | `deployable_import_types` | `list_import_types` |
|---|---|---|
| Asked | by every resolution, from the criteria it already built | by the model, by name, id or criteria |
| Answers | what matches this intent and has no instance in this answer | what is in the catalogue |
| Exists because | an empty import half must say which of its two causes it is | a developer names an adapter, or wants to see what there is |

The first is the load-bearing one, and it is why adding a tool does not reopen
§5.8's coverage-gap argument: a model that only ever calls
`resolve_semantic_selection` still learns that an undeployed type exists. The
tool is for naming one directly.

**A type already deployed is not offered for deployment.** Types with an instance
in `import_candidates` are dropped from `deployable_import_types` and kept in the
count. They are already reported above, with a topic and a status this list has
nothing to say about, and repeating them invites a second container for data the
platform already pulls. `list_import_types` cannot do the same — it is asked
without a resolution around it — so its note sends the reader to
`list_import_instances` with `import_type_ids` before deploying anything.

**What a catalogue row says, and what it cannot.** There is no instance, so there
is no topic, no container status and no history — every question a `Selectable`
answers is a question about an instance. What is left is what the type declares:
the variables that matched, `required_configs`, and `blocking_credentials` with
`deployable: false` where a credential config has no default. That last one is
reported rather than discovered on refusal, so a model is told before the
developer is asked to confirm a creation `CreateInstance` would reject. Both read
`secretShaped`, so the description and the refusal cannot disagree.

**A failed catalogue read degrades; a failed selectables read does not.** The
selectables half fails the whole resolution, because no field on the answer could
honestly say "the device half is complete and the import half is not". The
catalogue is additive: an answer without it is still complete about what exists,
as long as it says so — which the notes do, distinguishing "could not be read"
from "there is nothing to deploy".

**One upstream caveat, unresolved.** In `import-repository` v0.0.14 and v0.0.15,
`Controller.ListImportTypes` computes the caller's accessible id set and then
never passes it to the query, while `ReadImportType` does check access. Read from
those two versions' source; which version a given platform runs is not something
this repository knows. If it holds there, a catalogue listing can name a type
whose metadata read then answers 403 — an inconsistency between two tools that
looks like a bug here and is not one. Worth fixing upstream rather than
compensating for: filtering client-side would need the same permissions call this
service is already making and discarding.

## Degradation ladder

| Missing configuration | What stops | What still works |
|---|---|---|
| `device_selection_url` | the whole import half; `notes` says imports were not searched | every device answer, unchanged |
| `import_deploy_url` while device-selection is set | **startup**, deliberately — see `startImports` | — |
| `import_repo_url` | `get_import_type_metadata` by type id, `list_import_types`, `deployable_import_types` on every resolution, and **both creations**, which refuse rather than send a request they could not check | discovery, which returns the type of anything that already has an instance |
| `analytics_serving_url` | the history verdict, which becomes `unknown`; `create_export` / `delete_export`; and **reading an export at all** — `probe_export_data` stays declared-but-unavailable and `profile_series` refuses an `export_id`, because a query over an export needs column names that exist only in the export definition | discovery, status, wiring, creating an import |
| a chat store | `delete_import_instance` and `delete_export`, which have no record to check and are not advertised | creating, which reports the id to remove by hand |

`import_deploy_url` is the one that refuses rather than degrades. Discovery
carries no container status, so a deployment that could find imports but never
ask whether one is running would rank a stopped import as though it were live —
and that is the failure `pkg/imports` exists to prevent.

## The tool surface

Nine tools. Five read or emit, four change the platform, and the shape of the set
is the decision:

- `list_import_instances` and `get_import_type_metadata` — the direct lookup and
  the type read, neither of which a criteria query can express.
- `list_import_types` — the catalogue, which nothing else can reach. See
  [finding a type that has no instance](#finding-a-type-that-has-no-instance).
- `probe_export_data` — whether the export an import's history names actually holds
  rows, and which of its columns are null throughout. It belongs to this half
  because an export is how an import has any history at all, and it is at L0 so
  that the session that created one can verify it. See [reading an export
  back](#reading-an-export-back-which-is-a-different-question-from-having-one).
- `propose_operator_input` — confirmed (D11), because it produces something the
  developer deploys.
- **no signal discovery tool.** An import is found through
  `resolve_semantic_selection`, like a device. A separate "find imports" tool
  would let a model search one kind, get a plausible answer and never learn the
  other existed: a coverage gap that never announces itself, which is worse than
  an error. `list_import_types` is not that tool and cannot become it: a type has
  no instance, no topic and no data, so it can never answer what the platform
  carries.

- `create_import_instance`, `create_export` and their two deletions — the write
  half, all confirmed. See below.

`propose_operator_input` reads the instance rather than trusting the model for the
topic. The topic is derivable, but deriving it would make ODE assert an upstream
implementation detail — and the read also answers the question the developer asks
next. Its warnings cover the three ways a correct input still produces nothing: a
stopped instance, a live-only import, and a bound variable the export does not
carry.

## Creating an import, and creating its export

Implemented in [`pkg/imports/create.go`](../pkg/imports/create.go) and the second
half of [`pkg/tools/imports.go`](../pkg/tools/imports.go). The rest of this
document is about reading the platform; this section is the only part of ODE that
changes it.

### What the two upstreams accept, and what they accept silently

Both creations validate against the import type before sending, and the reason is
the same in both cases: the failure that gets through otherwise is not an error.

| Sent | What upstream does | What ODE does instead |
|---|---|---|
| A config name the import type never declared | Accepts it, marshals it into the container's `CONFIG` environment, never reads it back | Refuses, naming the config and listing what the type declares |
| A config value of the wrong type | 400 with `config value of wrong type` and no field name | Refuses, naming the config and its declared type — the same test, mirrored from `validateConfig` |
| An instance `id`, `kafka_topic` or `image` | 400: `explicit setting of id not allowed`; the image must equal the type's | Clears all three in the client; they are import-deploy's to mint |
| An export for an import the caller may not access | **201, with an empty instance body** — `userHasSourceAccess` returns `false` with a nil error and the handler encodes it anyway | Treats an empty export id as the refusal it is |
| An export value path that is not a payload leaf | Creates a column that is never written | Refuses: the path is checked against the type's output tree |
| Two export values in one column | Creates the export; the table carries one column | Refuses, naming the column and both variables |

The export's value paths stay **message-relative**, the form a `Selectable`
carries. analytics-serving puts them and the `TimePath` into one mappings map
(`addMappings` in `internal/ew-api/util.go`, `addTimescaleDBTimeMapping` in
`internal/ew-api/timescaledb.go`) and the export worker resolves every entry of it
against the same message document. Trimming the `value` envelope off a value path
therefore addresses the message root, where the payload's leaves are not — and
because the envelope's `time` does sit there, the timestamp still resolves. The
export deploys, rows land, and every column is null. `History` reports an export's
paths back unchanged for the same reason.

### The four fields that are the deployment's, not the import's

An export request carries `Offset`, `TimePath`, `TimestampFormat` and
`ExportDatabaseID`, and none of them is derivable from an import type. The export
database is created per deployment by analytics-serving's own migration and
carries whatever id that migration was given; the timestamp format is whatever
this platform's export worker parses. Guessing either produces an export that is
accepted, deploys, and stores nothing — a failure with no error anywhere.

So they are configuration (`export_offset`, `export_time_path`,
`export_timestamp_format`, `export_database_id`), with two of them able to answer
from evidence when unset:

- **`export_timestamp_format` empty** — copied from the newest export this
  platform already has, and reported in `derived` so a developer knows to check it
  first if nothing lands. With no export to copy from, creating one is refused.
- **`export_database_id` empty** — resolved from `GET /databases` when there is
  exactly one. Two is a refusal that names both: a platform with a timescale
  database and an influx one has them for a reason, and picking the first puts the
  export somewhere nobody asked for, where it is found only by the history lookup
  coming back empty.

`export_time_path` has a real default (`time`) because every import message
carries its timestamp beside its `value` payload. `export_offset` defaults to
`smallest`.

### Three of the four may be overridden per export

`offset`, `time_path` and `timestamp_format` are on `CreateExportRequest` and on
the `create_export` schema; `export_database_id` is not, because which database an
export lands in is not a modelling decision. `Derived` reports of each whether it
came from the request, from configuration, or was copied.

The offset is a modelling decision outright: "replay what the topic retains"
against "start from now" is not a deployment fact.

The time path is one because it is a property of the import type, and one platform
carries import types of both kinds. The envelope's `time` is the moment the import
wrote the message, which is what an import polling a live source wants — but an
import that backfills the past or forecasts the future carries the time its values
*describe* somewhere in the payload, and the envelope time then says only when the
replay ran. A backfill of several years finishes in minutes; with the envelope time
the whole of it lands inside those minutes, the export deploys, the rows are there,
and every reader that goes through the time axis sees a few minutes of data. The
export table's time column is `time` whatever the path was — analytics-serving
hardcodes `args.TimeColumn = "time"` in `genTimescaleDBExportArgs` — so this is not
recoverable by querying differently.

`messageTimePath` checks a requested path against the import type rather than
passing it through. It cannot go through `MessagePath`, which refuses everything
outside the `value` payload: that is right for a column and wrong for a timestamp,
because the envelope's own `time` is the default and has to stay sayable. The check
exists for the reason the value paths are checked — a path that names nothing is
not an error upstream, it is an export that consumes the topic and writes no row.

The timestamp format is overridable only because the time path is. The format is
what the export worker parses the field the time path names with, so a different
field is usually a different format, and the two are set together or the export
stores nothing. Set alone, the time path produces a note saying exactly that. The
tool schema tells the model to take the format from the developer and never to
invent one, which is the same rule that keeps `export_timestamp_format`
configuration rather than something ODE derives.

### Deleting is bounded by what the session created

Both deletions destroy data. import-deploy deletes the instance's Kafka topic with
it, so every retained message is gone and anything consuming the topic stops
receiving; analytics-serving tells the export worker to drop the timescale table,
so the stored history goes with the export. §5.8 denies `delete_platform_data`,
and a general delete tool would be that capability under another name.

What exists instead is narrower: `pkg/chat` records what each session created
(`ode_platform_creations`), and the two delete tools resolve the id against that
record before anything is called. An id the session did not create is refused, and
so is a read of the record that failed — "could not check" must not become "went
ahead". The refusal names what the session *did* create, so the model corrects
itself instead of retrying the same id.

A deployment without a chat store has no such record, so the two delete tools have
no executor at all and are never advertised. The two create tools still work: only
the undo needs the log, and a creation that could not be recorded says so in its
own answer, with the id to remove it by hand.

### What a one-sided change upstream costs

The outgoing dependency, as a fact. ODE writes to two services it does not own,
and the two halves fail in opposite ways — which is the whole of what is worth
knowing before either is upgraded.

| Shape | Where it comes from | What a rename upstream does |
|---|---|---|
| `idmodel.Instance` — the create and read body of import-deploy | `import-deploy v0.1.0`, pinned in `go.mod` | **Breaks this build.** A field that moved is a compile error here, before anything is deployed. |
| `flowengine.NodeInput` — the operator input `propose_operator_input` emits | `analytics-flow-engine/lib`, pinned | **Breaks this build**, same reason. |
| `dsmodel.ImportType`, `dsmodel.Selectable` — discovery and the type read | `device-selection v0.0.27`, pinned | **Breaks this build**, same reason. |
| `imports.ServingRequest`, `imports.Export`, `imports.ExportDatabase` — everything analytics-serving | **declared in this repository**, because upstream's are gorm entities in an `internal` package with no JSON tags | **Breaks at runtime, on a real platform.** A renamed request field is a 400 whose body is a map of field names; a renamed response field silently reads as empty, and an empty export id is how ODE is told the caller had no access — so a rename there arrives looking like a permission refusal. |

Two consequences follow, and both are the reason this table exists rather than a
sentence saying the services are coupled:

- **An analytics-serving upgrade is not a no-op for ODE**, even though ODE does
  not depend on it. Nothing in CI notices. What notices is
  `imports.Service.CreateExport` returning "analytics-serving answered without an
  export id" for every attempt, or a history lookup reporting `live_only` for
  imports that are in fact exported — the failure `pkg/imports/history.go` already
  warns about, now with a second cause.
- **Upstream is worth changing instead.** Two of ODE's local clients exist only
  because the shipped ones cannot express what is needed:
  `device-selection/pkg/client` has no field for
  `import_path_trim_first_element`, and analytics-serving re-exports its
  `internal` gorm model. Both are worth proposing upstream, and both would delete
  code here rather than add it — `imports.SelectionClient` goes away with the
  first, and the four declared wire shapes above with the second.

Deployment-side coordination — which ODE version ran against which platform
version, and who has to be told before an upgrade — is not something this
repository can keep true about itself and is not recorded here.

### Credentials

`create_import_instance` refuses any config whose name reads as a credential —
`api_key`, `password`, `token`, a bare `key` segment — either when the model
supplies a value for it, or when the import type declares no default for it. That
import is created in the platform's own dialog.

The test is a heuristic on the name and cannot be anything else: an import type
declares a name, a type and a description, and nothing marks a config sensitive.
It errs toward refusing, which costs a developer one step in the platform UI, and
the opposite error costs a model-invented credential in a running container. The
confirmation card is not the control here — it shows arguments and cannot edit
them, so a wrong value can only be declined, not corrected.

## Where this came up

Adding imports to ODE after the device half was finished, which is why the
asymmetries are recorded as differences from a working device path rather than as
properties of imports on their own.

The catalogue came later, from a gap the write half had all along and nobody had
walked into: `create_import_instance` needs an import type id, and every route to
one ran through an instance of that type. Three alternatives were considered:

- **Fold a search into `get_import_type_metadata`.** No new tool, no §5.8 row.
  Rejected: the name says "read one", and a tool that answers with an object or a
  list depending on its arguments is harder for a model to use correctly than two
  tools.
- **Only `deployable_import_types`, no tool.** The answer appears exactly where
  the failure appears, and nothing widens the surface. Rejected as incomplete
  rather than wrong: it cannot answer "is there a DWD weather import", which is
  how a developer who knows the platform asks.
- **Only the tool.** Smallest change, and the model has to think to call it after
  an empty resolution — at the moment it is least likely to, because nothing in
  the empty answer says a type exists. Both were built for that reason.

The write half (D31) came later still, from the case the read half exposes and
cannot answer: a resolution that finds nothing, or finds an import whose history
reads `live_only`, tells a developer exactly what is missing and leaves them to
go and create it elsewhere. Four alternatives were considered and rejected, and
they are recorded because each is the obvious next suggestion:

- **Create only, no deletions.** The narrowest surface, and it keeps §5.8's
  `delete_platform_data` denial literally true. Rejected because a creation the
  developer immediately regrets then has no undo anywhere near where it was made,
  and the assistant is the one that proposed it.
- **Deletions bounded only by the developer's own permissions.** The most useful,
  and rejected: one approval click on a mistyped id destroys a timescale table
  that may hold a year of history, and the denial would have to be rewritten to
  say deletion is permitted rather than bounded. The session-scoped record costs
  one table and makes the widest possible deletion an undo of something made
  minutes earlier.
- **Exports for devices and pipeline operators too.** `FilterType` also accepts
  `deviceId` and `operatorId`. Rejected as out of scope rather than as wrong: a
  device's values are already in timescale through the table worker, so a device
  export shapes a narrower table rather than making history exist — which is the
  problem the import half actually has.
- **Deriving the export's deployment fields instead of configuring them.** The
  timestamp format and the export database id were the temptation, because
  configuring them makes an otherwise self-contained feature need setup. Rejected
  because a wrong value there is accepted, deploys, and stores nothing. Copying
  them from an export that already exists is the middle path that was kept, and
  refusing outright is what happens when there is nothing to copy.

One thing was reconsidered mid-implementation and is worth naming: the
credential rule started as "flag it in the confirmation card and let the
developer see it". That is not a control, because the card renders arguments and
cannot edit them — a developer who spots a wrong secret can only decline. So a
credential-shaped config is refused outright instead, and the import is created
in the platform's own dialog.
