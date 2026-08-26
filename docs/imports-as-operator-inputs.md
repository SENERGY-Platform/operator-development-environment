# Imports as operator inputs

How ODE finds import types and import instances, and how it turns one into a
deployable operator input. Implemented in [`pkg/imports`](../pkg/imports),
[`pkg/selection/imports.go`](../pkg/selection/imports.go) and
[`pkg/tools/imports.go`](../pkg/tools/imports.go); specified in
[component-design.md](component-design.md) §5.2.1 and §5.8.

## Applies when

ODE resolves a data intent and the platform can satisfy it from an import rather
than a device, or a developer names an import directly. Requires
`device_selection_url` and `import_deploy_url`; `import_repo_url` and
`analytics_serving_url` each remove one capability rather than the surface. The
behaviour depends on the versions pinned in `go.mod` —
`device-selection v0.0.27`, `import-deploy v0.1.0`,
`analytics-flow-engine/lib v0.0.0-20251112135741` — because three of the four
asymmetries below are properties of those services rather than of ODE.

**Not this if**: the question is about the metadata model of imports, the wiring
contract the operator libraries expect, or why an import has no stored history.
Those are platform facts that hold for anything touching imports rather than ODE
decisions, and are documented on the platform side. This document only covers
what ODE does with them.

`geltung`: `einzelfall` — one implementation, verified against the pinned
versions above and against the platform's own callers (web-ui, device-selection),
not against a running platform.

## Which service answers what

| Question | Service | Why not somewhere else |
|---|---|---|
| Which import variables match these criteria, on which instances | device-selection `POST /v2/query/selectables` | It already expands the aspect subtree, walks the output tree, joins type to instance and trims the path. Doing that against import-repository directly means reimplementing four things whose failure mode is a silently short answer. |
| Is this instance running | import-deploy `GET /instances` | A selectables answer carries no container status at all. |
| One import type by id | import-repository `GET /import-types/{id}` | Only for a lookup with no criteria to send; discovery returns the type alongside every instance. |
| Is any of this import's past stored | analytics-serving `GET /instance` | timescale-wrapper has no `importId`, so the export is the only route. |

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
   device-selection.*
2. **No device class, and every import path is an event.**
   `ImportTypeFilterCriteria` is `{function_id, aspect_ids}` and nothing else.
   *Not absorbed, and ODE's to report*: a resolution that narrows by
   `device_class_id` cannot include imports at all, and says so in `notes` rather
   than returning a device-only answer that looks complete. See
   `selection.importNotes`.
3. **Type-level match, variable-level paths.** The criteria index is flattened
   per import type, so a *type* matches and the matching *paths* must be found by
   walking its output. *Absorbed by device-selection.*
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

## Degradation ladder

| Missing configuration | What stops | What still works |
|---|---|---|
| `device_selection_url` | the whole import half; `notes` says imports were not searched | every device answer, unchanged |
| `import_deploy_url` while device-selection is set | **startup**, deliberately — see `startImports` | — |
| `import_repo_url` | `get_import_type_metadata` by type id | discovery, which returns the type anyway |
| `analytics_serving_url` | the history verdict, which becomes `unknown` | discovery, status, wiring |

`import_deploy_url` is the one that refuses rather than degrades. Discovery
carries no container status, so a deployment that could find imports but never
ask whether one is running would rank a stopped import as though it were live —
and that is the failure `pkg/imports` exists to prevent.

## The tool surface

Three tools, and the shape of the set is the decision:

- `list_import_instances` and `get_import_type_metadata` — the direct lookup and
  the type read, neither of which a criteria query can express.
- `propose_operator_input` — confirmed (D11), because it produces something the
  developer deploys.
- **no discovery tool.** An import is found through
  `resolve_semantic_selection`, like a device. A separate "find imports" tool
  would let a model search one kind, get a plausible answer and never learn the
  other existed: a coverage gap that never announces itself, which is worse than
  an error.

`propose_operator_input` reads the instance rather than trusting the model for the
topic. The topic is derivable, but deriving it would make ODE assert an upstream
implementation detail — and the read also answers the question the developer asks
next. Its warnings cover the three ways a correct input still produces nothing: a
stopped instance, a live-only import, and a bound variable the export does not
carry.

## Where this came up

Adding imports to ODE after the device half was finished, which is why the
asymmetries are recorded as differences from a working device path rather than as
properties of imports on their own.
