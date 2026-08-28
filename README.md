# ODE — Operator Development Environment

Human-in-the-loop development environment for machine learning operators on the
SENERGY platform. A developer goes from a problem statement to a
deployable operator, assisted by an LLM that queries the platform ontology,
interprets computed data profiles and proposes modelling approaches. The
developer defines the evaluation criteria and makes every promotion decision.

The design and the reasons for it are in [docs/decisions.md](docs/decisions.md)
and [docs/component-design.md](docs/component-design.md). This file says what
exists, how to run it, and where everything else is written down.

## Architecture

A Go backend (gin) and a standalone React SPA — with Monaco (MIT) in the Code
pane, loaded as its own chunk so a developer who never opens it does not pay for
the editor — deployed into the same Kubernetes cluster as JupyterHub, Ray and
MLflow. The backend reuses `device-repository/lib/client` and `models/go` rather
than reimplementing a platform client. The SPA holds a Keycloak token and
presents it; every authorisation decision belongs to the backend and, beyond it,
to the platform's own per-user permissions.

```text
pkg/auth/          claim reading, developer-role gate, on-behalf-of token
pkg/ontology/      cached snapshot facade over the device repository, the aspect
                   tree, the selectables query, the per-user device groups and
                   wiring graphs, and the lexical intent matcher
pkg/devices/       per-user device reads, never cached across users
pkg/imports/       imports as the second kind of operator input: discovery through
                   device-selection, instance status, the export-or-Kafka history
                   verdict, the flow-engine input that wires one up, and the only
                   writes ODE makes to the platform — deploying an import and
                   creating the export that gives it a history
pkg/timeseries/    timescale-wrapper client: availability, device and export
                   usage, batched reads addressed at a device service or an export
pkg/profiler/      QuickProfile, SeriesProfile, detectors, store, projection —
                   over a device's service, or over an export, where the row count
                   stands in for the availability endpoint devices have
pkg/selection/     semantic selection: intent → criteria → selectables → devices
                   → ranked series, plus ontology_gaps and the import half
pkg/llm/           provider abstraction: one interface, one event stream, four
                   transports, plus the model price table for cost estimation
pkg/tools/         the §5.8 tool surface and the one Dispatcher that enforces
                   the exposure tier before any tool runs
pkg/chat/          sessions, the tool loop, tier changes with their audit trail,
                   held confirmations
pkg/admin/         §3.3: effective limits, the pre-request check, accounting
pkg/mcp/           the same tool registry over MCP, for the CLI provider
pkg/kernel/        JupyterHub: service registration, spawn, per-user token, the
                   kernel WebSocket protocol, workspace and keep-alive — one pod
                   per developer, one kernel per workbench inside it
pkg/repo/          the developer's git working copies, run in their own pod: a
                   workbench is one checkout and one kernel, and they hold several
pkg/charts/        §5.9's chart specification: validation, the transform-to-query
                   mapping, the profiler-derived annotations, and the
                   confirmations §5.10 takes from a chart
pkg/relations/     §5.5: candidate sets, one-query alignment, the idle/active
                   state series, pairwise contingency, the rule decision log
pkg/experiments/   Ray submission and MLflow, from a commit
pkg/interpret/     §5.13: the unwatched-run poller and the interpretation turn
pkg/database/      pgx pool and the schema the above persist into
pkg/identifiers/   unguessable ids for anything that appears in a URL
pkg/api/           gin routes, plus the cancellable WebSocket in ws.go and the
                   operations both surfaces share in operations.go
pkg/configuration/ config.json plus environment overrides
singleuser-image/  the JupyterHub notebook image the kernel runs in: Operator Lib
                   and the platform helper, built and published by CI. Source for
                   an artifact, not deployment configuration
docs/              the generated OpenAPI document, plus the hand-written knowledge
                   documents indexed below
```

## Running it locally

```bash
go build -o ode .
./ode -config config.json
```

The OpenAPI specification in `docs/` is generated from the annotations on the
handlers and **is committed**, so a fresh clone builds without generating first.
Run `go generate ./...` after changing a route or an annotation and commit the
result; CI regenerates and fails if it differs, which is what keeps a committed
spec from claiming an outdated state. The Dockerfile regenerates too. The result
is served at `/doc`, unauthenticated, which is how the platform's
developer-swagger-api collects it.

Configuration is read from `config.json` and overridden from the environment.
Copy `.env.example` to `.env` and fill in what your machine needs. Every setting,
what it decides, and which surface goes unserved when it is empty:
[docs/configuration.md](docs/configuration.md).

The SPA is a separate `npm run dev` in `frontend/` — see
[docs/frontend.md](docs/frontend.md). To run the tests, see
[docs/testing.md](docs/testing.md).

## Documentation

`docs/` is a knowledge location, not only a Swagger drop. Everything in it is
committed — the generated `swagger.json`, `swagger.yaml` and `docs.go` beside the
hand-written markdown — so the whole folder is readable without a build. Look
there before deriving a service's behaviour from its code again.

### Trying it

- [docs/walkthrough.md](docs/walkthrough.md) — a tour of the panes: what each
  surface does and what to look at first

### The design, and why

- [docs/decisions.md](docs/decisions.md) — purpose, non-goals, the thirty locked
  decisions D1 to D30, the architecture, the exposure-tier model (§0 to §4)
- [docs/component-design.md](docs/component-design.md) — what each component is
  for and must guarantee (§5)
- [docs/build-order-and-risks.md](docs/build-order-and-risks.md) — what was built
  in which order, the live risks, and what is still worth finding out (§6 to §8)

### Reference

- [docs/configuration.md](docs/configuration.md) — every setting, and what an
  empty one costs
- [docs/testing.md](docs/testing.md) — the two suites, and the checks that exist
  to catch one class of mistake
- [docs/frontend.md](docs/frontend.md) — the SPA's routes, its hand-rolled
  router, and the one thing a production host has to be told
- [docs/profiler-api.md](docs/profiler-api.md) — the profiler over `/ws` and over
  HTTP, and why there are two

### How it is meant to work, and how it breaks

- [docs/authorisation-and-exposure-tiers.md](docs/authorisation-and-exposure-tiers.md)
  — who ODE trusts, the single gate every tool call passes, and why a denied
  capability has no tool at all
- [docs/profiler-detectors.md](docs/profiler-detectors.md) — the `SeriesProfile`
  schema field by field, the detectors, and which numerics Go does not have
- [docs/profiler-contracts.md](docs/profiler-contracts.md) — never null and never
  absent, the store's two durabilities, the token budgets, the two timeouts
- [docs/profiler-reads.md](docs/profiler-reads.md) — the two-pass read, what
  bounds the raw pass, and telling an oversized response from a sick upstream
- [docs/chat-and-streaming.md](docs/chat-and-streaming.md) — a turn outlives its
  connection, and everything that follows from it
- [docs/kernel-and-repository.md](docs/kernel-and-repository.md) — why git runs in
  the developer's pod, and why ODE never stops one
- [docs/charts.md](docs/charts.md) — where a transform runs, and why a model may
  not compute a unit conversion
- [docs/relations.md](docs/relations.md) — where a candidate set comes from, and
  how much its grouping is worth
- [docs/experiments.md](docs/experiments.md) — a run is submitted from a commit or
  it is not submitted, and why a cell that logs to MLflow is still not a run
- [docs/result-interpretation.md](docs/result-interpretation.md) — an unwatched
  run, and why the interpretation waits for the developer
- [docs/imports-as-operator-inputs.md](docs/imports-as-operator-inputs.md) — how
  an import is found and wired, the four ways it differs from a device, and the
  first two things ODE creates on the platform: an import and its export
- [docs/simulation.md](docs/simulation.md) — what to do when the platform has no
  data for the case: a simulated site through MOSES, why the model picks a
  template rather than writing the document, and what a backfill does not produce

## Licence

Apache-2.0. See [LICENSE](LICENSE).
