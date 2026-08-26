# Frontend

The SPA: how it is built and served, what its routes carry, why the router is
hand-rolled, and the one thing a production host has to be told.

## Applies when

Working on the SPA, or deploying it behind something other than the Vite dev
proxy. The deployment note at the end is the one that bites first.

**Not this if**: the question is what the backend answers rather than how the SPA
asks — see [profiler-api.md](profiler-api.md) for the two profiler surfaces and
the generated OpenAPI document at `/doc` for the routes themselves.

`geltung`: `einzelfall` — one SPA, one deployment shape.

```bash
cd frontend
npm install
npm run dev
```

Vite serves on `:5173` and proxies `/api` to the backend on `:8080`. Copy
`frontend/.env.example` to `.env.local` and adjust.

Two Keycloak details cost time if you get them wrong, so they are worth stating
plainly:

- **The base URL needs the `/auth` suffix** — `https://<keycloak-host>/auth`.
  This deployment serves the legacy base path, while keycloak-js 17 and later
  default to the path without it. Omit it and the login redirect 404s, which
  looks like a missing realm rather than a missing prefix. Confirm with
  `curl -s <url>/realms/master/.well-known/openid-configuration | jq .issuer`.
- **The client must be public, with PKCE, and must list the dev origin** under
  Valid Redirect URIs (`http://localhost:5173/*`). Otherwise Keycloak answers
  `Invalid parameter: redirect_uri`. That message means the client exists but
  the origin is not registered; an unknown client id says `Client not found`
  instead, which is how to tell the two apart.

### Routes

The SPA is chat and code, with everything else behind a submenu. The pair is the
loop ODE exists for — the assistant writes files, the developer reads and
corrects them — and the rest is instrumentation the assistant drives and a
developer opens to audit or to take over by hand.

| Path | Shows |
|---|---|
| `/` | The workspace: the conversation on the left, the Code pane on the right |
| `/chat` | The conversation alone, full width |
| `/tools` | An index of the instrumentation views, each with what it is for and the LLM tools that reach the same backend |
| `/tools/ontology` | Aspects, functions and readable devices (§5.1) |
| `/tools/selection` | Semantic selection (§5.2) |
| `/tools/profiler` | Candidates and profiles (§5.4) |
| `/tools/exploration` | Chart specifications (§5.9) |
| `/tools/relations` | The relational profiler (§5.5) |
| `/tools/kernel` | The execution console (§5.6) |
| `/tools/experiments` | Reserved. M8's backend is served; the pane is not built yet, and the route says so |
| `/settings` | Admin limits, pricing, accounting, tool audit (§3.3), `admin` realm role only |
| `/github/callback` | The GitHub OAuth landing (§5.11 item 1) |

Every `/tools/…` route puts the conversation beside the view, with a divider the
developer can drag, collapse or nudge with the arrow keys. The divider position
is remembered in `localStorage` rather than in the URL: a link is something you
send to someone, and it should say *what* to look at, not impose the width of
your screen on theirs.

A route whose backend this deployment does not serve renders a card naming the
missing configuration — never a blank screen, never a silent redirect to the
start page. `session.features` is the source of truth, the same flags that gate
the header's navigation. An unknown path renders a "no such view" card for the
same reason: a wrong address deserves an answer.

### What the query carries

A reload used to land on the start page. What is being looked at now lives in
the address, so a reload, a bookmark and a link to a colleague all mean the same
thing.

| Parameter | Where | Restores |
|---|---|---|
| `session` | everywhere | The open conversation. Carried across every navigation, so moving to the profiler and back does not close it |
| `file` | `/` | The file open in Monaco, re-read from the working copy. Only the workspace has a code pane, so it is dropped when you move to a `/tools/…` view and comes back with the back button |
| `chart` | `/tools/exploration` | The chart on screen, fetched by id |
| `q`, `from`, `to`, `limit`, `unreadable` | `/tools/profiler` | The candidate query. Re-run on load, which costs nothing extra: the listing loads on mount anyway and reads no values (tier L0) |
| `series` | `/tools/profiler` | The selected candidate, matched out of that listing |
| `profile`, `tab` | `/tools/profiler` | The computed profile, **fetched by id**. Profiles are immutable and stored (D21), so a reload costs no value reads. A profile whose series or window does not match the selection is refused and the parameter dropped, rather than shown under the wrong label |
| `aspect`, `descendants` | `/tools/relations` | The proposal form. The proposal itself stays behind the button |
| `relation` | `/tools/relations` | The relation document, **fetched by id**. A pass profiles every participating service before it reads, so re-running one on a reload would be indefensible |
| `intent`, `limit`, `interaction`, `controlling`, `rank` | `/tools/selection` | The form. A resolution is not addressable by id, so the inputs come back and the run stays the developer's |
| `devices`, `functions` | `/tools/ontology` | The device search and the function type |

The rule behind the split: where a result is stored and addressable, the URL
names it and it is fetched back. Where it is not, the URL restores the *inputs*
and the developer presses the button. Nothing expensive re-runs because a page
was reloaded. The kernel console and the settings surface carry nothing — a code
draft does not belong in an address bar, and the settings page loads everything
it has on mount.

Query parameters are written with `replaceState`, so twenty filter tweaks cost
one press of the back button to escape rather than twenty; moving between views
pushes, so the back button walks the views.

### The router is hand-rolled

`frontend/src/router.tsx`, under 250 lines and a good half of them comment. The
SPA carries four runtime dependencies — react, react-dom, keycloak-js, monaco-editor — and what it needs
from a router is a path, a query, a link that behaves like a link, and the back
button. react-router would bring loaders, actions, nested outlets and a data
layer this application already has in `useLoad`.

A `<Link>` renders a real `<a href>` and intercepts only a plain left click, so
middle-click, ctrl-click and "copy link address" all do what they do everywhere
else. Base paths come from `import.meta.env.BASE_URL`, so the SPA works served
under a sub-path without a runtime setting.

**One deployment consequence, and it bites the first time someone reloads.** A
static host serving this SPA must rewrite unknown paths to `index.html`.
Without that, a reload of `/tools/profiler` asks the host for a file that does
not exist and gets a 404 — the app never loads, so it never gets the chance to
route. Vite's dev server already does this, which is why it does not show up
until the build is deployed. On nginx that is `try_files $uri $uri/ /index.html;`;
on an ingress or an object-store host it is the same idea under a different
name. ODE's Go backend does not serve the bundle today — if it is ever made to,
its static handler needs the same fallback for anything that is not an API
route, and `github_redirect_uri` in `config.json` already assumes that
arrangement is possible.

