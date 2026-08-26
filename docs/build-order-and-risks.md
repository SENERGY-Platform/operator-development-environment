# Build order, risks and open items

What was built in which order and why that order mattered, the risks still live,
and what is worth finding out. Kept together because they answer one question:
what state is this in.

## Applies when

Planning work, or judging whether a risk this project accepted still stands.

**Not this if**: the question is what a surface *does* — that is
[walkthrough.md](walkthrough.md), one section per pane. The acceptance criteria
that used to sit beside each finished milestone are gone: they are met, and a
criterion kept after it is met stops being read.

`geltung`: `einzelfall` — the state of one project at one time. The risk
severities are judgements, not measurements.

## 6. Build order

**Done: M0 to M9, and M11.** The per-milestone acceptance criteria are met and are
no longer carried here — what each surface does and how to check it by hand is
[docs/walkthrough.md](walkthrough.md), one section per pane.

The order these were built in is still worth stating, because two of the
dependencies were deliberate and would be wrong to reverse:

| | Deliverable |
|---|---|
| **M0** | Backend skeleton; Keycloak validation and the `developer` gate; ontology client and cache; on-behalf-of device read |
| **M1a** | timescale-wrapper client and `QuickProfile`. No series reads, no LLM |
| **M1b** | `SeriesProfile`: the detectors, the two-pass read, provenance, `not_computed`, the immutable store with its override overlay, the projection |
| **M2** | Semantic selection and `ontology_gaps`. Still no LLM |
| **M3** | Provider abstraction; MCP; chat; tool dispatch; **exposure tiers**; admin limits |
| **M4** | JupyterHub: registration, spawn, kernel, execute, token refresh, keep-alive |
| **M5** | Exploration pane: chart specs, annotations, confirmation controls, unit conversion |
| **M6** | Session detection, the relational profiler, aspect-scoped candidate sets |
| **M7** | Code pane, GitHub OAuth, repo select/create, scaffold, commit and push |
| **M8** | Ray submission, MLflow, the embed probe with its fallback |
| **M9** | Automated result interpretation in chat |
| **M11** | Imports as operator inputs (§5.2.1) |

**M1 before everything LLM-related was deliberate.** The profiler is
deterministic, independently testable, and was the most likely to overrun. LLM
work proceeded against fixture profiles in parallel.

**M2 before M3 for the same reason.** Semantic selection is pure ontology work
and needs no LLM; proving it standalone de-risked the most novel component. M11's
import half was built the same way and for the same reason.

### Outstanding

| | Deliverable | Accept when |
|---|---|---|
| **M10** | NetworkPolicy on singleuser pods; egress allow-list | **Required before external users — the only hard security prerequisite.** Session code cannot reach unlisted egress targets. Written, not applied; the control and its verification are not part of this repository |

## 7. Risk register

Live risks only. Entries the build settled are gone rather than kept with a tick:
a register that lists closed risks stops being read.

| Risk | Severity | Mitigation |
|---|---|---|
| Egress control on singleuser pods | **High** | M10, mandatory before external users. NetworkPolicy is not provided by JupyterHub, and z2jh's own default permits the whole public internet — so the gap was hiding behind a policy that looked closed |
| ODE reachable in-cluster, bypassing the gateway that authenticates tokens | **High** | Token validation is centralised at the API gateway (§3.1), so a direct in-cluster call is unauthenticated. M10's refusal of private IP space is what closes it, and singleuser pods running developer code make it concrete. Raise the severity if ODE is exposed before M10 is applied |
| Session and relational detection quality without labels | **High** | Interactive confirmation reframes the output as design rather than accuracy. No accuracy claim can be made without labelled ground truth, and none is assumed (D11) |
| Aggregated reads mask gaps and irregularity | **High** | Two-pass raw/aggregated read strategy (§5.3.2), with the read mode recorded in `provenance` per field |
| An LLM reads a missing field as a negative finding | **High** | Explicit `not_computed` with a reason (D24) — never null, never absent |
| Incomplete device types | Medium | Downgraded by ontology use; `ontology_gaps` surfaces the gap per device type at runtime |
| Projection hides material detail from the LLM | Medium | The `elided` block records the totals and a fetch reference (D26). Note this needed a second bound for breadth, not only the per-item one |
| Stale profiles in context after a detector change | Medium | `detector_version` in the cache key (D25) |
| Stale uncommitted work on the persistent PVC | Low | Surface divergence on reopen; never silently reset (§5.11) |
| Singleuser image change disrupts existing notebook users | Low | Ship as an additional KubeSpawner profile rather than replacing the default image |

## 8. Open items

Nothing blocks. The two original blockers closed during M0 — the timeseries
specification is integrated (§5.3) and JupyterHub is confirmed to share the
Keycloak instance and auto-mount per-user PVCs (§5.6) — and the two configuration
questions M0 carried were answered by the deployment rather than by design work —
the running Hub differs from its chart in two places, both documented on the
platform side.

The list below is what is still worth finding out. None of it gates work.

- Whether `/graphs` already contains a device relationship graph worth seeding §5.4 from.
- Which `Attribute` keys are in practical use on devices and device types (possible carriers for PV capacity, orientation, nominal power).
- Operator Lib interface surface at current HEAD, for scaffold generation.
- Whether existing `DeviceGroup` definitions cover common analysis groupings.
- Accepted `groupType` / `groupTime` values, and whether a `difference` aggregate exists for server-side counter differencing (§5.3.5).
- Whether `deviceGroupId` and `locationId` query modes are populated in practice — if so, they shortcut parts of §5.5.
- PVC size quota per user, and whether cloned repos plus cached extracts risk exhausting it.
- Whether the existing cull timeout is short enough to require an explicit exception or merely keep-alives.
- Retention and continuous-aggregate policy per device type, to predict when `/data-availability` reports coarse-only windows.
- Whether ODE should offer to **create** the missing export for a `live_only` import (§5.2.1). It is a write against analytics-serving and it would turn an import with no stored past into one that can be profiled and backtested — so it is the obvious follow-up to M11 and deliberately not in it. Two things to settle first: whether an export belongs to the developer or to the operator, and who pays for the storage it starts consuming.
- Whether `device-selection`'s selectables cache, which keys on the criteria hash without the token, is safe for the *import* branch — the instances come from the same call path, and whether their visibility is checked only afterwards decides whether it caches across users. Flagged as open on the platform side, where the import stack is documented.
- The SPA does not render the import half of a resolution: `SelectionResult` in `frontend/src/api.ts` and `frontend/src/__contract__/selection.json` do not carry `import_selectables` or `import_candidates`. Nothing breaks — the fixture still satisfies its type — but the fixture is no longer a complete record of the backend's answer.
