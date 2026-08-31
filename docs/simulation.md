# Simulated environments as a source of test scenarios

How ODE creates a simulated site in MOSES, what a template is and why the model
does not write the environment document, how example data gets into a scenario,
and what a backfill does and does not produce. Implemented in
[`pkg/simulation`](../pkg/simulation), [`pkg/simulation/templates`](../pkg/simulation/templates)
and [`pkg/tools/simulation.go`](../pkg/tools/simulation.go).

**This is the second place ODE stops being a reader**, after
[imports-as-operator-inputs.md](imports-as-operator-inputs.md). Creating a
simulation registers a platform device per asset, and a backfill puts rows in
timescale that are indistinguishable from live ones. Both are confirmed by the
developer, and the delete reaches only what the session created.

## Applies when

An operator needs data before it can be developed and the platform does not carry
it: a forecast that wants weeks of history on the day the work starts, a cycle
detector with no machine that cycles, a submetering operator with no meter tree.

**Not this if** the platform has the signal somewhere. A real series beats a
simulated one and it is not close — a simulation is a stand-in, and a result
measured against a stand-in is a result about the assumptions that went into it.
[resolve_semantic_selection](authorisation-and-exposure-tiers.md) and the import
catalogue come first, every time.

Requires `moses_url`. Without it the fourteen simulation tools are declared and
not callable, the same degradation an absent `ray_url` gives the two experiment
tools. `upload_simulation_dataset` additionally needs `jupyterhub_url`, because
the file it uploads is read out of the developer's own pod.

Everything below is one build of ODE against the service versions in the table.
**Re-check this document when one of these moves**, because behaviour here
depends on each:

| Dependency | State this was written against | What depends on it |
| --- | --- | --- |
| `SENERGY-Platform/moses`, `master` | `70c2272` | the environment document, `provisionZone`'s condition for registering a device, the version conflict, the validation rules the templates render around |
| `device-repository` | `v0.2.53` (`go.mod`) | `ListDeviceTypesV3`, and the service attribute shape the backfill precondition reads |
| `platform-connector-lib`, as **MOSES** pins it | `v0.0.0-20260827082232-c8133d0f997d` | what the timescale ingestion does with `senergy/time_path`. ODE does not depend on it directly and does not mirror the parts that turn on it — see [three states, not a boolean](#three-states-not-a-boolean) |

## What MOSES is, in the terms ODE uses

An **environment** is one document: a site, building or apartment. It carries

- **zones**, nestable and typed (`site`, `building`, `floor`, `unit`, `hall`,
  `room`), so depth is data rather than schema;
- **assets** inside zones (`meter`, `inverter`, `machine`, `sensor`, `actuator`),
  each bound one-to-one to a platform device;
- **channels** inside assets, each publishing to one platform service, carrying a
  `characteristic_id` and a unit;
- **context** and **context sources** — outdoor temperature on a day cycle,
  irradiance following the sun — shared by every zone below;
- a **seed**, from which every stochastic source derives, so the same document
  and clock produce the same values.

A channel is driven by one **source**: `script` (JavaScript), `profile`
(declarative per-hour and per-weekday factors, optionally cumulative), `dataset`
(replay of a real timeseries), `formula` (derived from other channels and the
context) or `aggregate` (the sum over everything sub-metered by this asset —
configurationless, because the meter tree *is* the configuration).

**A simulated asset is an ordinary platform device.** That is the whole reason
this integration is small: `pkg/ontology`, `pkg/devices`, the profiler and the
exploration pane already work on one, unchanged, and nothing in ODE has a
concept of "simulated data" to special-case.

### The MOSES endpoints ODE uses

| Method | Path | What |
| --- | --- | --- |
| GET | `/environments` | the developer's environments |
| POST | `/environments` | create one |
| GET | `/environments/:id` | read the document |
| PUT | `/environments/:id` | replace it, with the version |
| DELETE | `/environments/:id` | delete it, and its managed devices |
| GET | `/environments/:id/state` | live state of a running environment |
| PATCH | `/environments/:id/state` | set boundary conditions on it |
| POST | `/environments/:id/backfill` | compute and publish a past window |
| GET | `/environments/:id/backfill` | follow that job |
| GET | `/device-types` | the device types an asset can be built from |
| GET, POST, DELETE | `/datasets`, `/datasets/:id` | uploaded timeseries for replay |

`POST /devices` and `DELETE /devices/:id` are deliberately absent although MOSES
offers them: a device belongs to an asset, MOSES provisions and removes it as
part of storing the document, and a client that created one separately would be
creating a device with nothing to justify it.

## Four properties that decide the design

### The owner comes from the caller's token

MOSES never serialises `Environment.Owner` and takes it from the token the
request carried. ODE passes the developer's own token, which is §3.1 item 3
without asking for anything new.

**There is no service account here and there will not be one.** One would create
simulations belonging to ODE: the developer could not find them in the MOSES UI,
and nothing could delete them. This is the same reasoning that keeps every other
platform read on the developer's token, arriving at a stronger conclusion,
because a simulation is a thing that exists afterwards rather than a read that is
over.

### Two fields are the server's, and ODE strips exactly those two

MOSES reconciles `external_managed` (whether it may delete an asset's device
again) and `external_graph_ref` (the mirrored device-repository graph) on every
write, because the whole document is sent each time and an echoed flag would
decide whether somebody's real device is destroyed.
`Environment.forWrite` clears both.

**It does not strip `external_ref`, although the plan this was built from said
every `external_*` field would be.** That would be the opposite of safe.
`provisionZone` in MOSES registers a device for every asset that names a device
type and carries no reference:

```go
if asset.ExternalRef != "" || asset.ExternalTypeId == "" {
    continue
}
```

So a write with the reference removed does not fail. It provisions a *second*
device for an asset that already had one, `reconcileManagedFlags` marks it
managed because the reference changed, and the timeseries of the first device is
orphaned — attached to a device nothing publishes through any more.
`TestAnEditKeepsTheDeviceTheAssetAlreadyPublishesThrough` pins it, against the
fake in [`pkg/simulation/simulationtest`](../pkg/simulation/simulationtest),
which provisions the way MOSES does for exactly this reason.

### Writes are whole-document, with a version

`Environment.Version` is counted by MOSES; a write carrying a stale version is
refused with `409`, and a refused write deletes nothing. Every ODE tool that
changes a simulation is therefore Get, edit, Replace, and a conflict is reported
to the model as:

> Nothing was written and nothing was deleted: MOSES refuses the whole document.
> Read the simulation again with `get_simulation` and apply the change to what is
> there now — do not send this one again, it will be refused the same way.

That wording is doing work. It is the mirror image of
the ODE-403 rule — there a write may already have landed and the fix is to
re-list before retrying; here the write certainly did not land, and a blind retry
fails identically forever.

### The mirror is complete, and refuses when it is not

ODE mirrors MOSES's domain types rather than importing
`moses/lib/domain`: that package is documented as changing with its owner's
migrations, and ODE would take a build dependency on it for a handful of structs.

The cost of mirroring is drift, and drift here is not cosmetic. A whole-document
PUT means a field this build does not know would be **deleted from the
developer's environment by the act of editing it**. So the cost is paid down
rather than accepted:

- `Service.Get` decodes a second time with `DisallowUnknownFields` and records
  the first member it did not recognise.
- `Service.Replace` and `Service.Create` refuse a document carrying one, with
  `ErrUnknownField`.
- `get_simulation` reports it as `read_only` with the field named, so the
  assistant says "edit this one in the MOSES UI" rather than discovering the
  refusal on a write.

**Reading is never refused for it.** An ODE that could not show a developer their
own environment because MOSES gained a field would be the worse failure; the
refusal belongs on the write, which is the only place the loss happens.

## The tool surface

Fourteen tools, in the order a developer meets them.

| Tool | Tier | Confirm | What |
| --- | --- | --- | --- |
| `list_simulations` | L0 | no | names, types, ids, counts |
| `get_simulation` | L0 | no | the document: zones, assets, channels, sources |
| `list_simulation_templates` | L0 | no | the scenarios, their roles and parameters |
| `list_simulation_device_types` | L0 | no | what an asset can be built from |
| `list_simulation_datasets` | L0 | no | uploaded timeseries available for replay |
| `get_backfill_status` | L0 | no | follows a job |
| `get_simulation_state` | **L1** | no | live values are values |
| `create_simulation` | L0 | **yes** | renders a template; creates platform devices |
| `add_simulated_asset` | L0 | **yes** | same, one asset at a time |
| `set_channel_source` | L0 | **yes** | changes what a device publishes |
| `set_simulation_context` | L0 | **yes** | patches the live state |
| `delete_simulation` | L0 | **yes** | destroys devices and their timeseries |
| `backfill_simulation` | L0 | **yes** | publishes a past window into timescale |
| `upload_simulation_dataset` | L0 | **yes** | stores example data for replay |

`get_simulation_state` is at L1 for the reason every read in the surface sits
where it does: it is actual data. Everything else about a simulation is
structure and belongs at L0.

The confirmations are not ceremony. A simulated device appears in the
device-repository and is inventory other people's applications see until somebody
removes it, which is the same argument the four import writes make. Backfill's
confirmation is its own: it writes weeks of rows through the ordinary connector,
and once they are in timescale a backfilled row is indistinguishable from a live
one, so the developer should see the window before it happens.

`list_simulation_templates` is not in the plan's own table. It is here because
the plan's central decision needs it — see below.

## The model does not write the environment document

An environment is three hundred lines of nested JSON, validated by a service the
model cannot see, with two fields it must not set and several refused at exactly
the wrong length: a profile needs 24 hour factors or none, an actuator must not
carry an interval, an asset may not be sub-metered by itself or across a top
level zone. Letting a model author that makes every one of those a `400` it
discovers by retrying.

So it picks a **template** and fills in what the template asks for. A template
is a Go struct in [`pkg/simulation/templates`](../pkg/simulation/templates) that
renders to an `Environment`, which makes a validation error ODE's to prevent
rather than the model's to discover. The failure mode becomes "the template lacks
a knob", which is a bug report.

Templates live in ODE rather than being read from MOSES because MOSES's own
template endpoints belong to the legacy `world`/`room`/`device` model the
environment API replaced.

### The catalogue is not part of the template

Which device types exist, and which characteristic a service carries, is a
property of the platform this ODE talks to. A template therefore declares the
**roles** it needs filled, and the caller binds each to a device type and a
service it actually found:

```
list_simulation_templates  →  what has to be bound and what may be tuned
list_simulation_device_types  →  what this platform has
create_simulation  →  the binding, the parameters and a name
```

The characteristic and the unit are copied from the bound service and never
invented, which is the same rule the rest of ODE follows for every characteristic
it reports. A service that carries no characteristic is refused rather than
rendered: a channel on one publishes a bare number, and everything downstream
that reads it — the profiler's unit, a chart's axis, a conversion — has nothing
to go on.

Channels are grouped under an asset role rather than listed flat, because MOSES
binds an asset to exactly one platform device. A flat list would let a caller
bind two channels to services of two different device types and find out from a
`400`.

### The two templates

**`pv_site`** — an inverter with a `power` channel on a daylight curve and an
optional cumulative `energy` meter, an irradiance context source on the same sun,
and an optional ambient temperature. The scenario a generation forecast is
developed against.

**`machine_hall`** — N machines on a shift pattern, each sub-metered by one hall
meter whose channel is an `aggregate`. The sub-metering case with something
actually under it, which is the part that is hard to find in real inventory: a
platform whose devices were commissioned one at a time rarely records which meter
sits above which machine.

Both are guesses from the two operators in flight when this was built. One real
scenario a developer wanted last week beats both, and adding a third is a struct
and a table row.

### Three template details that are wrong if done the obvious way

**A cumulative profile's base is a rate, not a total.** MOSES accumulates
`base × hour factor` at each tick, so handing the day's energy in as the base
makes a meter count up by the area under the curve times what was asked for. It
looks like a working meter. `perDayRate` divides by that area;
`TestTheEnergyMeterGainsWhatTheParameterAsksForOverADay` pins it.

**A temperature is an offset day, not a scaled one.** A scaled shape puts every
night at exactly zero degrees whatever the scenario said, and zero degrees is a
temperature rather than an absence of one, so nothing downstream could tell.
`offsetShape` maps the 0..1 shape onto `base`..`base+span`.

**A machine outside its shift still draws standby**, for the same reason — and
that standby is the thing a submetering operator is meant to find. A hall
configured to stand at exactly zero gets the smallest base that keeps the shape
multiplicative and has its shift level scaled against it, which is reported as
"a thousandth of the running draw" rather than as zero. The alternative is a
machine whose shift pattern is not in the data at all.

**The hall meter has to carry the machines' characteristic.** An `aggregate` sums
the sub-metered channels carrying *its own* characteristic, so a meter bound to a
different one is a valid document whose meter reads zero forever — it deploys, it
publishes, and the number is a lie about the hall. `MachineHall.Render` refuses
that binding by name.

## Example data: finding it, and getting it in

A template gives a *shape*, asserted by whoever wrote the template. That is the
weakest kind of test data and the assistant is told to say so. The system prompt
gives the order to work in, and it is the part of this integration that is about
judgement rather than plumbing:

1. **Look for real data first, every time.** A real series beats a simulated one.
2. **If nothing fits, go and find example data rather than inventing a shape.**
   In order of worth:
   - **a real device on this platform** whose history resembles the case, which
     `set_channel_source` replays straight into a simulated channel with a
     `dataset` source of origin `platform`. Nothing moves anywhere and no file is
     uploaded; MOSES fetches the window once at start and replays it.
   - **an open dataset**, fetched with `run_code` into the developer's own
     workspace as CSV and uploaded with `upload_simulation_dataset`.
   - **a file the developer already has** in that workspace.
3. **Only then a profile**, said plainly to be an assertion.
4. **Say where the data came from.** A scenario built on data of unknown
   provenance is not evidence of anything, and it is indistinguishable from real
   measurement to everybody downstream once it is in timescale.

### Why the upload goes through the developer's pod

`upload_simulation_dataset` takes a `workspace_path`, not file content. Three
things follow from that and each is the reason:

- **ODE reaches no network on its own.** The fetch is `run_code` — the
  developer's own confirmed cell, under the developer's own identity, subject to
  whatever egress policy their pod has. ODE's backend never becomes an
  unbounded HTTP client on a model's word.
- **The bytes travel as bytes.** A year of minute values is megabytes; through a
  model's context it is impossible, and through a tool argument it is a token
  bill for data nobody reads.
- **The pod is where the search already happened.** The assistant that fetched
  the file is the one that names its path.

`tools.Kernel` gained `ReadFile` for this. It is not new authority — `run_code`
can already read any file on that pod — and the interface comment says so.

Two refusals in that executor are worth knowing. A **truncated** read is refused
rather than uploaded short: a cut-off CSV parses, so MOSES would accept it and
the replay would end early with nothing anywhere saying why. A **binary** file is
refused with what a dataset actually is, because "did not decode" on its own
sends the developer looking for an encoding problem.

`timezone` is not cosmetic. A file of local timestamps without an offset read in
the wrong zone shifts every value by an hour or two, which is invisible in the
data and wrong in everything trained on it. MOSES records the zone it parsed
with, so a later re-parse cannot silently read them differently.

## Backfill

`POST /environments/{id}/backfill {"from":…,"to":…}` computes the environment
over a window that has already passed and publishes every reading with the
timestamp it would have had. Seed plus window determine the result, so **the same
document and window produce the same dataset** — which is what makes a model
retrainable on it, and is the reason the whole integration is worth building.

Six things about it belong in every answer the assistant gives, and are in the
tool's own warnings:

- **It is not idempotent.** Running the same window twice writes every row twice
  and nothing downstream de-duplicates. MOSES's `409` prevents it concurrently,
  not sequentially.
- **Only `profile` and `dataset` channels are reconstructed.** A `script` channel
  is stateful, and a `formula` or `aggregate` channel is derived — the honest
  place to derive it is downstream, from the backfilled inputs.
- **A channel whose service does not declare `senergy/time_path` cannot be
  backfilled at all.** This is the one worth acting on before anything is built;
  it has [its own section](#the-backfill-precondition-checked-before-a-device-exists).
- **MOSES decides finally when the job runs**, not when the window is validated.
  A request is accepted before it is known that every channel will be skipped,
  which is why ODE checks what it can much earlier.
- **A job can report `done` with everything skipped.** `get_backfill_status`
  therefore reports `skipped_channels` with a reason each, and says so in its
  note rather than letting `state: done` read as success.
- **A `404` from the status endpoint is not a failure.** Jobs live in memory and
  a restart forgets them, which MOSES reports rather than claiming a state it
  cannot know. ODE answers `state: "unknown"` and says the way to tell the two
  apart is to profile the data.

The window is checked in ODE before it is sent — not out of distrust, but because
a refusal that names the number is better than a `400` after a round trip, and
one of the rules ("may not end in the future") is checked against a clock that is
not MOSES's.

## The backfill precondition, checked before a device exists

`senergy/time_path` is a service attribute in the device repository. The
platform's timescale ingestion stamps a row with `time.Now()` unless it is set,
and its value is a dotted path to the content variable carrying the event time.
A channel whose service does not declare it cannot be backfilled at all: the rows
would be a block of identical timestamps, which is worse than no data.

**The attribute is optional and is unset on most device types.** That is not an
oversight. It only matters for a publisher that wants to write the past, and
nobody sets it for a device that reports the present. So the one property that
decides whether a simulated scenario can carry history is also the one nobody
thought about when the device type was modelled — and a simulation is usually
built *because* an operator needs weeks of history on the day the work starts.

Discovering that from a backfill status means discovering it after the devices
exist in the device repository, the scenario is authored, and the developer has
confirmed twice. So ODE checks earlier, in three places:

| Where | Why there |
| --- | --- |
| `list_simulation_device_types` | While the model is *choosing* the service. This is the only point at which the answer is free. |
| `create_simulation` and `add_simulated_asset` | Against the rendered document, before the write — the last moment before a device exists in somebody's repository. |
| `backfill_simulation` | Before the job, not out of its status. A simulation can be built by one session and backfilled by another, and a job that skips every channel still runs, reports itself `done` and publishes nothing. |

### It warns; it does not refuse

A simulation nobody will backfill is a perfectly good scenario — a live one, for
an operator being watched as it runs. Refusing to build it would be ODE deciding
what the developer's case is. What ODE can do is make sure nobody finds out
afterwards, and that is what the warning is:

> **These channels cannot be backfilled**, so this simulation will only ever have
> the history it accumulates from now on. Making one backfillable means adding
> the `senergy/time_path` attribute to the *device type* in the device
> repository, which is shared inventory and a modelling decision for whoever owns
> that type — neither ODE nor the simulator changes it.

That last clause is the operative one. MOSES stays passive about the attribute
deliberately: a device type is shared, real devices use the same one, and
changing it from a simulator would change how their data is ingested. ODE
inherits that position, so the honest answer is a modelling change by somebody
else rather than another attempt from here.

### Why the attribute has to be fetched separately

**MOSES's own `/device-types` drops it.** That catalogue is a projection —
`lib/devices/catalog.go` defines a five-field `Service` (id, name, direction,
characteristic, value path) described as "reduced to what building an asset from
it needs". Attributes are not part of building an asset, so they are not in it.
The projection is deliberate and, for its purpose, right; the sting is that the
one attribute it drops is the one that decides whether the asset can do the thing
MOSES's own backfill exists for.

**The device repository does carry it, but not through an endpoint keyed by id.**
Both selectable types — `model.DeviceTypeSelectable` from the device repository
and `pkg/model.Selectable` from device-selection — hold `[]models.Service`, the
full type, attributes included. What they are not is a lookup: both answer a
*semantic* query and return the services that matched given function and aspect
criteria. MOSES hands over device type *ids*, which is the wrong shape for either.

So `ontology.DeviceTypesByID` exists to close that gap: whole device types, in one
request, keyed by the ids MOSES said are simulatable.

### Three states, not a boolean

`simulation.CheckTimePath` answers `possible`, `impossible` or `unknown`, and the
third is the point.

**`impossible` is decisive.** No attribute means no backfill, and nothing further
up can overturn that.

**`possible` is a necessary condition, not a sufficient one.** MOSES's own
`ResolveTimeShape` checks two further things that ODE deliberately does *not*
mirror:

- that the time variable's characteristic and its declared type agree — the
  ingestion reads a unix time out of a number and an iso timestamp out of a
  string. That needs the converter's characteristic ids, and mirroring a table of
  platform constants here is how a warning starts lying.
- that a variable beside the time carries a characteristic and is numeric, so
  there is a value to publish at all.

So every answer built on a `possible` verdict carries `simulation.BackfillCaveat`
saying what was not checked and pointing at `get_backfill_status`. Claiming more
would be worse than checking nothing: a false reassurance is acted on.

**`unknown` is not `no`.** A device repository that did not answer says nothing
about a device type. The same applies when the repository and MOSES disagree
about which services a type has — that is a disagreement between two services
rather than a fact about the service.

What ODE *does* mirror is MOSES's structural checks, in MOSES's order: the path
has at least two segments, the service has exactly one output, that output is
JSON, the path starts at the root variable's own name, and the named variable
resolves — refusing a structure without members and a map along the way, because
the platform's message cleaning indexes `SubContentVariables[0]` without checking
and that shape takes the connector down rather than failing.

## What the model may not do

**Write a script source.** A `script` source is JavaScript executed inside MOSES,
and admitting one is a decision about a second execution surface rather than a
feature. `profile`, `dataset`, `formula` and `aggregate` cover these scenarios
declaratively. The refusal says what to reach for instead, per kind, and points
at the MOSES UI for a developer who genuinely wants a script.

**Delete anything this session did not create.** `delete_simulation` goes through
the same `tools.Creations` gate as `delete_export` and `delete_import_instance`,
and the refusal names what the session *did* create rather than being a flat no.
What a delete destroys is stated before it happens: the devices MOSES created and
everything they published, but not a device the developer attached themselves,
which outlives the simulation with its own timeseries.

**Schedule anything.** `set_simulation_context` sets one state per call. ODE has
no scheduler and this does not add one — "warm the hall to 30 °C over the next
hour" is the developer's own `run_code` against the same endpoint, or a
follow-up tool once somebody has wanted it twice.

## Rejected alternatives

These were decided before the code existed and are kept because a rejected
alternative is the part nobody can reconstruct afterwards.

**The legacy `world` / `room` / `device` / `service` endpoints**, with
`changeroutine`, `routinetemplate` and `usetemplate`. They are the model MOSES
migrated away from and are undocumented in its own API description. Building
against them would have been building this twice, and the modern model is the one
that carries sub-metering, the backfill and the device-repository graph mirror —
all three of which are the reasons to integrate at all.

**Letting the model write the environment document.** Three hundred lines of
nested JSON, validated by a service the model cannot see, with two fields it must
not set and several refused at exactly the wrong length. Templates make the
failure mode "the template lacks a knob", which is a bug report rather than a
`400` the model retries. Written up under
[the model does not write the environment document](#the-model-does-not-write-the-environment-document).

**A service account for MOSES.** The owner comes from the caller's token, so a
service account would create simulations belonging to ODE: the developer could
not find them in the MOSES UI and nothing could delete them. This is the only
place in ODE where a service account would be technically easy, and it is refused
on that ground alone.

**Importing `moses/lib/domain`.** A build dependency on a package documented as
changing with its owner's migrations, for a handful of structs. The mirror plus
the unknown-field guard costs less and fails better — see
[the mirror is complete, and refuses when it is not](#the-mirror-is-complete-and-refuses-when-it-is-not).

**Mirroring MOSES's full `ResolveTimeShape`.** The characteristic-and-declared-type
check needs the converter's characteristic ids, and a table of platform constants
copied into ODE is how a warning starts lying. Three verdict states carry the gap
instead.

**A scheduler for scenarios.** Out of scope. `set_simulation_context` sets one
state per call; ramping a value over an hour is the developer's own `run_code`
against the same endpoint, until somebody has wanted it twice.

**Refusing to create a simulation whose channels cannot be backfilled.** A live
scenario is a legitimate one, and refusing it would be ODE deciding what the
developer's case is. It warns instead.

## What is not built

**The SPA pane.** The plan's M4, deliberately last: the assistant reaching these
tools is the point, and a pane that only mirrored them is work a developer can do
in the MOSES UI. There is no `pkg/api/simulation.go` and no route, which is why
the client is constructed inside `startM3` — where the LLM surface is wired —
rather than in `New` beside the other platform clients: nothing but the tool
surface reads it.

**The backfill precondition on `get_simulation`.** It would be the natural fourth
place — it is the read a model makes before proposing a backfill — and it is left
out on cost. `get_simulation` is the cheap structure read of the surface, called
whenever a channel id is needed, and putting the check there would add two
upstream calls to every one of them. The three places it *is* checked cover the
moments that matter: choosing the service, creating the device, and starting the
job.

**A delete for an uploaded dataset.** Uploads are recorded as creations so a
session can say what it put there, but there is no tool to remove one. A dataset
is small, immutable and referenced by whatever replays it; the gate exists and
adding the tool is fifteen lines when somebody wants it.

**A provenance mark on a simulated series.** A developer reading a profile of
synthetic data should know it is synthetic. That belongs in the provenance
sidecar (D22), and `external_managed` or the mirrored graph ref is how it would
be known. Until then the honest place it is said is the assistant's own answer,
which the system prompt requires.

## What a one-sided change upstream costs

MOSES's environment API is the contract, and ODE mirrors its types rather than
importing them. Three changes upstream are worth naming:

- **A new field on any mirrored struct.** ODE reads the document, marks it
  read-only and refuses to write it. Nothing is lost and the developer is told;
  the fix is to add the field to `pkg/simulation/environment.go`.
- **A change to `provisionZone`'s condition for registering a device.** The
  round-trip test would still pass while the property it protects had moved. The
  test's own comment names the condition it stands on.
- **A new source kind.** ODE reads it (`Source.Kind` is a string and the
  variants are pointers), reports it by name in `get_simulation`, and cannot
  author one until `buildSource` learns it. That degradation is the right one.
- **A change to what MOSES skips in a backfill, or to `ResolveTimeShape`.** ODE's
  own precondition check would keep answering, and would be answering the old
  question. A `possible` verdict is already documented as necessary rather than
  sufficient, so the failure is a warning that under-reports rather than one that
  lies — but the table above is the trigger to re-read it.

The reverse direction is thinner than it looks: ODE writes only documents it
rendered from its own templates or edited one channel of, so a MOSES that
tightened validation would refuse those with a field path, which
`ErrInvalidRequest` carries through to the model verbatim.
