# Plan: MOSES as ODE's source of test scenarios

**Status:** proposed, nothing implemented. Written from the MOSES sources at
`SENERGY-Platform/moses@master` (paths below are that repository's).

## The problem this solves

An operator needs data before it can be developed, and the data an operator needs
is rarely there on the day the work starts. A forecasting operator wants weeks of
history; a cycle detector wants a machine that actually cycles; a submetering
operator wants a meter tree with something under it. Today the developer looks for
a real device that resembles the case, and if there is none, the work waits.

MOSES simulates the site instead. ODE already *finds* what it publishes — a
simulated asset is a platform device, so `pkg/ontology`, `pkg/devices` and the
profiler see it like any other. What ODE cannot do is bring one into existence, so
the developer leaves ODE to author a scenario and comes back to use it. That
round trip is the whole gap.

## What MOSES is, in the terms this plan uses

An **environment** is one document: a site, building or apartment
(`lib/domain/environment.go`). It carries

- **zones**, nestable and typed (`site`, `building`, `floor`, `unit`, `hall`,
  `room`), so depth is data rather than schema;
- **assets** inside zones (`meter`, `inverter`, `machine`, `sensor`, `actuator`),
  each bound one-to-one to a platform device;
- **channels** inside assets, each publishing to one platform service, carrying a
  `characteristic_id` and a unit;
- **context** and **context sources** — outdoor temperature on a day cycle,
  irradiance following the sun — shared by every zone below;
- a **seed**, from which every stochastic source derives, so the same document and
  clock produce the same values.

A channel is driven by one **source**: `script` (JavaScript), `profile`
(declarative per-hour and per-weekday factors, optionally cumulative), `dataset`
(replay of a real timeseries, an uploaded file or an allow-listed endpoint),
`formula` (derived from other channels and the context) or `aggregate` (the sum
over everything sub-metered by this asset — configurationless, the meter tree
*is* the configuration).

Four properties decide most of the design below:

1. **The owner comes from the caller's token** (`Environment.Owner`, never
   serialised). ODE passes the developer's own token and inherits §3.1 item 3
   without asking for anything new. No service account, and no simulation owned by
   nobody.
2. **Three fields are the server's to decide, and a client that sends them does
   damage:** `external_ref` (the platform device of an asset), `external_managed`
   (whether MOSES may delete that device again) and `external_graph_ref` (the
   mirrored graph). MOSES reconciles all three on every write
   (`lib/api/provision.go`), because the whole document is sent each time and an
   echoed flag would decide whether somebody's real device is deleted.
3. **Writes are whole-document with a version.** `Environment.Version` is counted
   by the server; a write carrying a stale version is refused with `409`, and a
   refused write deletes nothing. Read, change, write back — never edit blind.
4. **Backfill exists, and it is why this is worth building.**
   `POST /environments/{id}/backfill {"from":…,"to":…}` computes the environment
   over a window that has already passed and publishes every reading with the
   timestamp it would have had; `GET` on the same path follows the job. MOSES's own
   `docs/backfill.md` names operator development as the case. Seed plus window
   determine the result, so **the same document and window produce the same
   dataset** — a model can be retrained on it.

### The API surface worth using

Modern, from `lib/api`:

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
| POST, DELETE | `/devices`, `/devices/:id` | the catalog behind an asset |
| GET, POST | `/datasets`, `/datasets/:id` | uploaded timeseries for replay |

**Deprecated, and out of scope:** `world`, `room`, `device`, `service`,
`changeroutine`, `routinetemplate`, `usetemplate`. They are the legacy model MOSES
migrated away from; building against them would be building the plan twice.

## What ODE adds

Four groups, in the order a developer meets them.

### Read

| Tool | Tier | Confirm | Why |
| --- | --- | --- | --- |
| `list_simulations` | L0 | no | names, types and ids; no measurements |
| `get_simulation` | L0 | no | the document: zones, assets, channels, sources |
| `list_simulation_device_types` | L0 | no | what an asset can be built from |
| `get_simulation_state` | **L1** | no | live values are values (§3.2) |

`get_simulation_state` sits at L1 for the reason every read does: it is actual
data, not structure. Everything else here is structure and belongs at L0.

### Author

| Tool | Tier | Confirm | Why |
| --- | --- | --- | --- |
| `create_simulation` | L0 | **yes** | creates platform devices |
| `add_simulated_asset` | L0 | **yes** | same, one asset at a time |
| `set_channel_source` | L0 | **yes** | changes what a device publishes |
| `delete_simulation` | L0 | **yes** | destroys devices and their timeseries |

Confirmed for the reason D31's four tools are: this is platform state other
people's applications can see, and a simulated device that appears in the
device-repository is inventory until somebody removes it. `delete_simulation`
reaches **only what the session created**, recorded through the existing
`tools.Creations` mechanism and `ode_platform_creations` — the same rule and the
same table as imports and exports.

**The model does not author the document freehand.** It picks a *template* and
fills in what the template asks for: how many machines, which characteristic, what
load. Templates live in ODE (`pkg/simulation/templates`), because MOSES's own
template endpoints are part of the legacy surface. A template is a Go struct that
renders to an `Environment`, so validation errors are ODE's to prevent rather than
the model's to discover: `lib/domain/validate.go` refuses an environment with no
zone, a source whose variant is missing, a profile without 24 hour factors, an
actuator with an interval, an asset sub-metered by itself.

Two templates to start, matching the operators already being built here:

- **`pv_site`** — an inverter with `power` and `energy` channels on a profile with
  a day shape, plus an irradiance context source and a temperature channel.
- **`machine_hall`** — machines with a cycling `power` channel and a hall meter
  that aggregates them, which is the submetering case with something under it.

### Drive

| Tool | Tier | Confirm | Why |
| --- | --- | --- | --- |
| `set_simulation_context` | L0 | **yes** | patches the live state |

One patch per call, and no scheduling. ODE has no scheduler and this plan does not
add one: "warm the hall to 30 °C over the next hour" is the developer's own
`run_code` against the same endpoint, or a follow-up tool once somebody has wanted
it twice.

### Backfill

| Tool | Tier | Confirm | Why |
| --- | --- | --- | --- |
| `backfill_simulation` | L0 | **yes** | publishes into timescale |
| `get_backfill_status` | L0 | no | follows the job |

The confirmation is not ceremony: a backfill writes weeks of rows through the
ordinary connector, and once they are in timescale a backfilled row is
indistinguishable from a live one. The developer should see the window before it
happens.

## How it is built

```
pkg/simulation/          the client and the types (moses is the transport, not the concept)
  simulation.go          Service: List, Get, Create, Replace, Delete, State, Patch, Backfill
  environment.go         the domain types ODE needs, mirrored from lib/domain
  templates/             pv_site, machine_hall
pkg/tools/simulation.go  the executors, and the definitions in surface.go
pkg/api/simulation.go    REST for the SPA — M4, not before
docs/simulation.md       the reasoning, once it exists
```

- **Config:** `moses_url` and `moses_request_timeout`. Empty `moses_url` leaves the
  routes unserved and the tools declared-but-unavailable, exactly as
  `github_client_id` and `ray_url` do (§5.11, §5.12). Nothing in ODE requires a
  simulator.
- **The client mirrors the types it needs and no more.** Not an import of
  `moses/lib/domain`: ODE would take a dependency on a package documented as
  changing with its migrations, for a handful of structs.
- **Every write is read-modify-write with the version.** A `409` is reported to the
  model as a refusal with the reason, never retried blindly — the same lesson as
  the ODE-403 retry rule: re-read first, because the write may already have landed.
- **`external_*` fields are stripped on the way out.** ODE sends what it authored
  and nothing MOSES reconciles, so a stale ref from the model's copy of a document
  can never decide that a real device gets deleted.

## Milestones

**M1 — read.** Client, the four read tools, tier gating, an httptest fake in
`pkg/simulation/simulationtest`. *Exit:* a session at L0 lists and reads
environments; `get_simulation_state` is refused at L0 and served at L1; the
walkthrough has an entry.

**M2 — author.** `create_simulation` from both templates, `add_simulated_asset`,
`set_channel_source`, `delete_simulation` with creation recording. *Exit:* a
confirmed create produces devices the ontology tools then find, and a delete
removes exactly them and refuses anything this session did not create.

**M3 — backfill.** `backfill_simulation`, `get_backfill_status`. *Exit:* a
scenario created in M2 carries four weeks of history that `quick_profile` reports
as a real series, and the same window twice produces the same data.

**M4 — the pane, if it earns itself.** A simulation view in the SPA. Deliberately
last: the assistant reaching these tools is the point, and a pane that only
mirrors them is work the developer can do in the MOSES UI.

## Rejected

- **The legacy `world`/`room`/`device` endpoints**, with `changeroutine` and
  `usetemplate`. Deprecated in MOSES, and the modern model is the one that carries
  submetering, backfill and the graph mirror.
- **Letting the model write the environment document.** Three hundred lines of
  nested JSON, validated by a service the model cannot see, with three fields it
  must not set. Templates make the failure mode "the template lacks a knob", which
  is a bug report rather than a 400 the model retries.
- **A service account for MOSES.** The owner comes from the token; a service
  account would create simulations belonging to ODE, which nobody could find or
  delete.
- **Importing `moses/lib/domain`.** See above.
- **A scheduler for scenarios.** Out of scope, and `run_code` covers it until it
  does not.

## Open questions

- **Which templates first.** `pv_site` and `machine_hall` are guesses from the two
  operators in flight (PV forecast, cycle detection). One real scenario the
  developer wanted last week beats both.
- **Does the backfill condition hold in this deployment?**
  `docs/backfill.md` names `senergy/time_path` as the hard condition: the platform
  stamps a row with `time.Now()` unless the device type says where the message
  carries its own timestamp. That is the same field as `EXPORT_TIME_PATH` in this
  repository's `.env.example`, and it has to be checked against the device types
  the templates use before M3 is planned in detail.
- **Should a simulated series say so in its profile?** A developer reading a
  profile of synthetic data should know it is synthetic. The provenance sidecar
  (D22) is where that belongs, and the graph ref or `external_managed` is how it
  is known.
- **Whether `set_channel_source` should accept a script.** A `script` source is
  JavaScript the model would be writing, executed inside MOSES. `profile`,
  `formula` and `dataset` cover the scenarios above declaratively; admitting
  scripts is a decision about a second execution surface, not a feature.
