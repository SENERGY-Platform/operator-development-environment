# ODE — Operator Development Environment

Human-in-the-loop development environment for machine learning operators on the
KIEEZ / SENERGY platform. A developer goes from a problem statement to a
deployable operator, assisted by an LLM that queries the platform ontology,
interprets computed data profiles and proposes modelling approaches. The
developer defines the evaluation criteria and makes every promotion decision.

The build specification is [SPEC.md](SPEC.md). It is the source of truth; this
file only says how to run what exists.

## Status

**M0 of the build order in SPEC.md §6.** What works today:

- The `developer` realm role gate on every route. Token signature, expiry and
  audience are validated by the platform API gateway, not here.
- Ontology client and snapshot cache over the platform device repository:
  aspect tree, measuring and controlling functions, characteristics, concepts,
  device classes.
- Device listing and reading **on behalf of the calling user**. ODE never
  substitutes a service account for user data (SPEC D5).
- A React SPA that logs in against Keycloak and shows all of the above.

Not built yet: everything from M1a onward — the timeseries client, the
profiler, semantic selection, the LLM tool surface, kernels, repositories and
experiments.

## Architecture in one paragraph

A Go backend (gin) and a standalone React SPA, deployed into the same
Kubernetes cluster as JupyterHub, Ray and MLflow. The backend reuses
`device-repository/lib/client` and `models/go` rather than reimplementing a
platform client. The SPA holds a Keycloak token and presents it; every
authorisation decision belongs to the backend and, beyond it, to the platform's
own per-user permissions.

```
pkg/auth/          claim reading, developer-role gate, on-behalf-of token
pkg/ontology/      cached snapshot facade over the device repository
pkg/devices/       per-user device reads, never cached across users
pkg/api/           gin routes
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
|---|---|
| `required_realm_role` | Defaults to `developer` |
| `device_repo_url` | Device repository base URL |
| `ontology_cache_ttl` | How long a snapshot is served without a freshness check |
| `cors_origins` | Only needed if the SPA is not served through the Vite proxy |

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

The backend tests use no containers and no network. The device repository is a
fake and test tokens are minted unsigned — deliberately, since signatures are
the gateway's concern — so the suite runs in about a second without the
platform.

## Two things worth knowing before you extend this

**ODE does not validate tokens, and must therefore sit behind the gateway.**
Signature, expiry and audience are checked centrally by the platform API
gateway; `pkg/auth` parses claims unverified to read `sub` and `realm_access`.
That is correct for gateway traffic and unsound for anything else — a
cluster-internal caller reaching ODE's service DNS directly is authenticated by
nothing. Since JupyterHub singleuser pods run developer and LLM-authored Python
in the same cluster, the M10 NetworkPolicy is what makes the assumption hold.
Do not expose ODE before it.

**Read permission is not data permission.** `models.Read` governs device
metadata; `models.Execute` governs reading a device's *data*. M0 lists devices
under `Read`. Every timeseries read from M1a onward has to be scoped to
`Execute`, or ODE will offer series it cannot actually read.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
