# Design decisions

The purpose, the non-goals, the thirty locked decisions D1 to D30, the
architecture, the identity and exposure model, and the one design rule everything
else follows from. This is the source of truth for *why*.

## Applies when

Deciding anything a locked decision already settles, or wanting to know why a
component is shaped the way it is. Decisions are referenced by their D-number
from the code and from the rest of `docs/`.

The section numbers are load-bearing. Over a thousand references in the Go
comments and a hundred and fifty in the rest of `docs/` address this material as
§5.4, §3.2, D24 — so the numbering is kept exactly as it was when it lived in a
root SPEC.md, and a section is never renumbered. Add, do not shuffle.

**Not this if**: the question is what a component *does* rather than why — see
[component-design.md](component-design.md), or the subject documents indexed in
[README.md](../README.md). And not this if the answer is a property of a platform
service rather than of ODE. Those are documented on the platform side, not here.

`geltung`: `allgemein` — these are decisions, not observations. A decision changes
by being replaced, not by being disproved.

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

This is a design principle, not a performance note. Do not violate it for
convenience.

---
