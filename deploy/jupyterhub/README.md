# What JupyterHub has to be told

## The role

Four scopes, and ODE refuses to start without all four:

| Scope | Why |
|---|---|
| `servers` | `POST /users/{name}/server` — the spawn |
| `tokens` | `POST /users/{name}/tokens` — the per-user credential the kernel API is called with. ODE narrows each minted token to `access:servers!user={name}` and gives it an expiry, so what reaches a pod cannot spawn or stop anything |
| `access:servers` | Reaching `/user/{name}/api/*` at all |
| `users:activity` | `POST /users/{name}/activity` — the keep-alive of §5.6 item 3 |

Plus `read:users`, to tell "starting" from "ready" while a pod spawns. User
models only: not their tokens, and not their auth state.

## The one thing that is not obvious

**The token must not come from `hub.services.ode.apiToken`,** which is the
chart's own mechanism and the natural thing to reach for.

When `apiToken` is unset, z2jh's helper looks the current value up from the live
`hub` Secret with Helm's `lookup`, and generates a fresh random one when it
cannot find it. Argo CD renders with `helm template`, where `lookup` returns
nothing — so every sync would mint a different token and ODE would answer 403
until its pod was restarted, with nothing saying why.

`values-ode.yaml` therefore appends the service in `hub.extraConfig`, reading the
token from an environment variable fed by the SealedSecret. That is the pattern
`values-keycloak.yaml` already uses for the OAuth client secret.

The same rotation applies today to `cookie_secret`, `CryptKeeper.keys` and the
proxy auth token, which is why the hub pod restarts on every Argo CD sync and
every user is logged out. The evidence is in the hub ReplicaSets: their
`checksum/config-map` annotation is identical across revisions while
`checksum/secret` differs every time. That is a pre-existing problem, it is not
ODE's to fix, and `values-ode.yaml` says so where someone changing this would
look.

## Idle culling

No cull exception is needed while `jupyterhub_keepalive_interval` stays well
below `cull.timeout`: ODE reports activity for as long as it holds a session, and
stops once it has heard nothing for `jupyterhub_idle_timeout`, which hands the
pod back to the cluster. The deployment culls at 3600s and ODE keeps alive every
300s.
