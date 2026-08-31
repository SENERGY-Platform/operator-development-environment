# Frontend

The SPA: how it is built and served, what it is built *out of*, what its routes
carry, why the router is hand-rolled, how it asks for attention when a turn ends,
and the one thing a production host has to be told.

## Applies when

Working on the SPA, or deploying it behind something other than the Vite dev
proxy. The deployment note at the end is the one that bites first.

**Not this if**: the question is what the backend answers rather than how the SPA
asks — see [profiler-api.md](profiler-api.md) for the two profiler surfaces and
the generated OpenAPI document at `/doc` for the routes themselves.

This is one SPA and one deployment shape. Two facts below are the deployment's
rather than the SPA's — that Keycloak serves the legacy `/auth` base path, and
that the Go backend does not serve the bundle — and both are called out where
they appear.

```bash
cd frontend
npm install
npm run dev
```

Vite serves on `:5173` and proxies `/api` to the backend on `:8080`. Copy
`frontend/.env.example` to `.env.local` and adjust.

### What the interface is built out of

Tailwind CSS v4 and [shadcn/ui](https://ui.shadcn.com) on Base UI, with the
`base-vega` style and the `mist` palette (preset `b7BYR8oLo`, Source Sans 3, small
radius). `components.json` pins that choice; `npx shadcn@latest add <name>` writes a
new component into `src/components/ui/`, which is **vendored MIT source and not ODE
code** — see the README in that directory for the attribution and for the one local
edit a regeneration would overwrite.

**`shadcn apply <preset>` needs a manual step afterwards**, and skipping it is
silent. It writes the new light palette into `:root` and appends the dark half as a
`.dark {}` block — a selector this application never matches, because it themes on
`data-theme`. Left as generated, the result is a light theme on the new preset and a
dark theme still on the old one, with every check green. So after an apply: lift the
values out of the appended `.dark`, restate both dark scopes below from them, and
delete it. The comment at the top of `index.css` says the same thing where it will
be read.

Three things about the setup are ours rather than the generator's, and each is
commented where it lives:

- **The theme has three states, not shadcn's two.** shadcn switches on a `.dark`
  class being present or absent, which is also why the apply step above exists. This application distinguishes "no choice made,
  follow the operating system" from an explicit light, because `theme.ts` *removes*
  the attribute to mean the former. `src/index.css` therefore declares the dark
  palette in two scopes — a guarded `prefers-color-scheme` block and
  `:root[data-theme="dark"]` — and defines `@custom-variant dark` over the same
  three states so a `dark:` utility in a component agrees with the tokens.
  `theme.test.ts` asserts all of it.
- **The chart palette is not the preset's.** `exploration.tsx` draws up to eight
  series and the preset ships five neutral greys, so `--series-1`..`--series-8`
  are carried over from the stylesheet this replaced. They are a property of the
  data, not of the theme.
- **Page geometry stays in CSS.** shadcn has no layout primitives, so the split,
  the per-view pane grids, the file tree and the container-query folds are written
  as rules at the end of `src/index.css` rather than as arbitrary-value utilities.
  The folds are last on purpose: `@container` adds no specificity, so a rule
  written below them that names the same selector silently wins.

Four components carry surfaces that had no equivalent before:
[`Message`](https://ui.shadcn.com/docs/components/base/message) and
[`MessageScroller`](https://ui.shadcn.com/docs/components/base/message-scroller)
are the conversation — the scroller is what replaced an effect that called
`scrollIntoView` on every token and yanked the reader back to the bottom whenever
they scrolled up mid-answer;
[`Marker`](https://ui.shadcn.com/docs/components/base/marker) is the transcript's
system rows, the notices and the tool calls, which are things that *happened* in
the conversation rather than things either party said; and
[`Questionnaire`](https://ui.shadcn.com/docs/components/base/questionnaire) is the
rule review in `relations.tsx`, described under [relations.md](relations.md).

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
| `/tools/experiments` | Ray jobs, their MLflow runs and the interpretation of the finished ones (§5.12, §5.13) |
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
| `session` | everywhere | The open conversation. Carried across every navigation, so moving to the profiler and back does not close it, and paired with the `workbench` below |
| `workbench` | everywhere | Which working context the panes act in: which checkout a file read is answered from, which kernel `run_code` runs in, which commit an experiment is launched from. Absent means *my only one*, which is what the backend reads a request naming none as; with two open the SPA writes the choice here rather than guessing per request. Carried across every navigation for that reason — dropped, a step into the kernel pane and back gave the developer the first workbench, silently, which ran their code in the other operator's checkout |
| `file` | `/` | The file open in Monaco, re-read from the working copy. Only the workspace has a code pane, so it is dropped when you move to a `/tools/…` view and comes back with the back button. Dropped by a workbench switch as well: it is a path inside one checkout, and no two workbenches hold the same repository, so carrying it would ask the workbench being opened for a file it does not have |
| `chart` | `/tools/exploration` | The chart on screen, fetched by id |
| `q`, `from`, `to`, `limit`, `unreadable` | `/tools/profiler` | The candidate query. Re-run on load, which costs nothing extra: the listing loads on mount anyway and reads no values (tier L0) |
| `series` | `/tools/profiler` | The selected candidate, matched out of that listing |
| `profile`, `tab` | `/tools/profiler` | The computed profile, **fetched by id**. Profiles are immutable and stored (D21), so a reload costs no value reads. A profile whose series or window does not match the selection is refused and the parameter dropped, rather than shown under the wrong label |
| `aspect`, `descendants` | `/tools/relations` | The proposal form. The proposal itself stays behind the button |
| `relation` | `/tools/relations` | The relation document, **fetched by id**. A pass profiles every participating service before it reads, so re-running one on a reload would be indefensible |
| `intent`, `limit`, `interaction`, `controlling`, `rank` | `/tools/selection` | The form. A resolution is not addressable by id, so the inputs come back and the run stays the developer's |
| `devices`, `functions` | `/tools/ontology` | The device search and the function type |

**The conversation and the workbench travel together.** A workbench is one
operator — one checkout, one kernel — and a conversation is about one operator, so
these two parameters are reconciled rather than left to drift apart. Opening a
conversation puts the code pane on the workbench that conversation names, whichever
way it was opened: the session list, the link from an experiment back to the
conversation that launched it, a reload of a colleague's link, the back button.
Switching workbenches in the tab bar moves the other way, to that workbench's most
recent conversation, and closes the conversation when it has none — leaving one
about another operator beside this code is how the assistant ends up asked to write
files it was never told about.

Two cases move nothing. A conversation that names no workbench claims nothing: it
was written before workbenches existed, and the backend reads it as *my only one*.
And a conversation naming a workbench that has since been closed is not followed —
the working copy it was about is not open, and there is nothing honest to put on
screen for it. `frontend/src/workbench.tsx` holds both directions in one place; the
chat pane only publishes its session list.

A conversation's workbench is not fixed once it is chosen. The picker in the
conversation's own header moves it, which is where the developer is standing when
they notice the conversation is about the other operator — a session row would have
them fix it from a list instead of from the thing they are reading. Moving publishes
the updated session through the same list, so the pairing above then carries the code
pane to the new checkout with no extra wiring. The picker is absent where there is
nothing to choose — no repository surface, or one workbench, which is what an
unassigned conversation already acts in — with one exception: a conversation naming a
workbench that has since been closed always offers it, because that is the case a
developer needs it for most, and the option shows *closed workbench* rather than
displaying the first open one as though it were the one.

The move leaves a note in the conversation, and the pane renders it as ODE's rather
than the developer's. It is stored with the user role, because that is what a model
reads as input, so a pane that went by role alone would show the developer a message
they never wrote in their own voice — the same rule §5.13's run summary needs, and
the reason `origin` is on the wire at all.

The rule behind the split: where a result is stored and addressable, the URL
names it and it is fetched back. Where it is not, the URL restores the *inputs*
and the developer presses the button. Nothing expensive re-runs because a page
was reloaded. The kernel console and the settings surface carry nothing — a code
draft does not belong in an address bar, and the settings page loads everything
it has on mount.

A half-typed chat message is neither addressable nor an input to re-run, so it is
held in memory — one draft per session in `ChatView`, deliberately above the key
that remounts a conversation. The remount is what replaces the turns and the held
confirmations when you switch sessions; the sentence you were writing is not part
of what is being replaced. It does not survive a reload, and does not need to: it
was never sent, so there is nothing on the backend to restore it from.

Query parameters are written with `replaceState`, so twenty filter tweaks cost
one press of the back button to escape rather than twenty; moving between views
pushes, so the back button walks the views.

### A finished turn asks for attention, and only from the background

A chat turn is detached and can run for minutes
([chat-and-streaming.md](chat-and-streaming.md)), so the developer is expected to
go and do something else. `frontend/src/attention.ts` is what tells them it is
over. Three signals, and they are not equal:

- **The title blinks** (`● Reply ready` alternating with the page title). Free,
  needs no permission, works everywhere, and it is what changes the label on the
  Windows taskbar button. Always on.
- **A desktop notification.** The only one that makes a taskbar button *flash*:
  the browser raises a real OS notification and the window manager takes it from
  there. Needs `Notification.requestPermission()`, hence the header toggle —
  Chrome and Firefox both hold a permission request without user activation
  against the origin, and an unannounced prompt on first load is the interruption
  this feature exists to replace.
- **A short synthesised tone.** No asset, and no autoplay problem: it only plays
  after the developer has clicked the toggle on this page.

Two properties are load-bearing. Nothing fires while the window is in front of
the developer, judged by `visibilityState` **and** `document.hasFocus()` — an ODE
window fully visible on a second monitor while the developer types in their
editor is `visible` and unfocused, and that is the case the feature is for.
And Stop never alerts: abandoning a turn is not waiting for one.

**The notification needs a secure context.** Served over plain HTTP anywhere but
localhost, `Notification` is undefined, the module reports `unsupported`, and the
toggle marks itself — the blink and the tone still work, the taskbar flash does
not. That is the same constraint as Keycloak's, so a deployment that has one has
the other.

### The sessions panel marks the conversation that has come back

The attention signals above are about the *window*. They say nothing when the
developer is sitting in front of ODE with three conversations open, working in one
while another finishes — and that is the ordinary way to use a surface where a turn
takes minutes.

So `useSessionMarks` in `frontend/src/chat.tsx` subscribes to `chat_watch`
([chat-and-streaming.md](chat-and-streaming.md)) and marks the rows of the sessions
panel. Three marks, because there are three questions about a conversation you are
not looking at:

| Mark | Dot | Means |
|---|---|---|
| `working` | hollow, breathing | a turn is in flight; nothing is expected of you |
| `reply ready` | filled, `--online` | it finished and you have not been back since |
| `needs you` | filled, `--warn` | it stopped on a confirmation |

Filled against hollow carries the distinction, colour separates the two filled
states, and the word settles it — so the row still reads on a monochrome screen and
with a colour deficiency. Motion is the fourth carrier and it is on the `working`
mark alone: the row breathes and a ring leaves the dot, on the shared busy idiom
below. It answers the question the other three cannot — whether the turn beside it
is still alive, or the socket went away and left a static outline behind. The two
settled marks stay still, because a finished conversation is not more urgent for
blinking, and `attention.ts` is where the interrupting version of *that* lives,
careful to fire only from the background.

Three properties, and each of them is a decision rather than an implementation
detail:

- **Half of it is the tab's, half the engine's, and the hook holds the two apart.**
  "You have not looked at it since it finished" is a fact about this screen;
  "a turn is in flight" is a fact about the conversation, true whether or not
  anyone is looking. So `idle` only becomes unread for a conversation this tab
  watched start — otherwise every conversation the developer has ever had would be
  marked "reply ready" — and opening a conversation settles the unread half only,
  from the effect on `?session=` rather than from the click handler, because the
  parameter is also set by links elsewhere in the shell. `working` and `needs you`
  are not the reader's to settle: reading a conversation neither finishes its turn
  nor answers its confirmation, so both are merely *hidden* on the open row and are
  back on it the moment the developer switches away. Held as one value per row they
  could not be, because the only way to drop the unread mark was to drop the row's
  entry — which is how a running turn used to be forgotten by being looked at.
- **It does not survive a reload, and does not need to.** A conversation that
  finished before the page loaded is a conversation with a reply in it, which the
  list already shows. What does survive is anything still *running*, because the
  watch opens with a snapshot of it.
- **A dropped connection settles every mark to "reply ready".** The tab no longer
  knows what is in flight; a turn that ended during the gap is exactly what the
  developer needs telling about, and one that did not is put straight back by the
  snapshot the next subscription opens with.

The subscription is registered once, off `odeSocket.onState`, for the reason the
conversation's own reattach is: `onState` replays the current state to every new
listener, so a subscription that depended on anything per-render would re-subscribe
in a loop. `chat.render.test.tsx` asserts the count.

### One thing moves, and only while something is running

A clone takes tens of seconds, a kernel cold start up to a minute, a chat turn
minutes. For that whole time the only evidence the developer has is a caption that
says `Cloning…` — and a caption that has not moved in forty seconds is
indistinguishable from one left behind by a request that died. So waiting is the
one thing this interface animates.

It is one idiom, shared, rather than a spinner per pane:

- **`.busy`** — Tailwind's `animate-pulse`, on a button label whose text has
  switched to the running form (`Cloning…`, `Committing…`, `Submitting…`), and on
  any inline span that is only on screen because something is in flight
  (`running…` under a kernel cell, the socket's `connecting…`, the phase line of a
  relation run).
- **`Busy` in `ui.tsx`** — `Muted` for the same case: the caption that stands in
  for a pane while it loads. It carries shadcn's `Spinner` beside the text, and it
  is an `aria-live="polite"` region, because otherwise the whole wait is silent to
  a screen reader — the text appears with nothing to announce it and vanishes the
  same way. The spinner is `aria-hidden` even though shadcn gives it
  `role="status"`: the paragraph is already the live region, and two announcements
  for one wait is one too many.
- **The conversation's own wait** is a `Marker` with a spinner rather than either
  of these, because it is a row in the transcript.

Two consequences worth keeping. A muted line that merely *ends in an ellipsis* is
not a `Busy` — `Take a proposed set…` is an instruction, and animating it would
say something false. And nothing here needs a reduced-motion guard of its own:
Tailwind's `animate-*` utilities and the `tw-animate-css` keyframes both respect
`prefers-reduced-motion`.

### What a session row says, and renaming it

Two lines. The title, and under it the tier badge, the provider and the workbench
this conversation acts in — `L2 · claude-cli · franzmueller/pv-forecast`.

The workbench is there in place of a message count, which is what the row carried
first. A count grows by two every turn and answers no question a developer has in
front of the list; which operator a conversation is about is the question, because
that is also the workbench the code pane follows when the row is opened
([kernel-and-repository.md](kernel-and-repository.md) has the pairing). A session
that names no workbench falls back to the developer's only one, which is how the
backend already reads an unnamed workbench — with several open there is nothing
honest to name, and one whose workbench has been closed says so, which is also why
opening it leaves the code pane where it is.

Both lines are cut with an ellipsis and carry the full text in a `title`. That is
not cosmetic: ODE titles a new conversation with the first eight words of its
opening message, so a row reads `Ich möchte einen neuen operator bauen, der …`
before the developer has renamed anything, and it used to run off the panel and
under the delete button.

Renaming is the other half of that. `PUT /chat/sessions/{id}/title` sets the
developer's own name, the pencil opens the box in the row rather than in a dialog —
the reason to rename this conversation is what can be read on it, and a modal over
the list takes that away — and an empty name hands the guess back, since the engine
titles a session from the next message whenever there is no title. Enter commits,
Escape abandons, losing focus commits: clicking away from a name you have just typed
reads as being done with it. All three overlap, because Enter and Escape both take
the input off the screen and a removed element delivers its `focusout` in some
browsers and not others — so an edit is marked settled when it ends, and the second
delivery finds nothing left to send. `chat.render.test.tsx` dispatches the key and
the `focusout` in one batch, which is the order where that matters.

### Two runs on one conversation: the later one owns the view

`run` in `frontend/src/chat.tsx` is what watches a turn — a send, a confirmation or
an attach — and only one of them at a time is *the view*. It marks that with an
`AbortController` in a ref: starting a run aborts whatever was there, and a run that
finds the ref pointing at something newer than itself must land silently, touching
neither the busy flag nor the reload nor the alert. They belong to whoever is
watching now.

Two things made that rule necessary rather than tidy, and both are in
`chat.render.test.tsx`, which mounts the pane under `StrictMode` the way `main.tsx`
does:

- **StrictMode mounts, tears down and mounts again.** The teardown aborts the
  controller the first attach installed. So the reattach's guard has to ask whether a
  *live* view exists — an aborted controller is not one — or the second attempt skips
  itself and the only attach left is the cancelled one.
- **The aborted run lands after the one that replaced it.** Its `finally` runs a
  microtask later, so without the rule it reached `setBusy(false)` *after* the fresh
  attach had set the flag. A developer coming back to a conversation mid-turn saw
  nothing happening, on a turn that was still running — the one thing a detached
  exchange must never look like.

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


### Reconnecting GitHub without losing the pane

The connect card starts the OAuth flow with `window.location.assign`, which is
right for a first connection: there is no working copy yet, so the tab has nothing
worth keeping. A credential that goes stale *mid-session* is the other case, and
there the same navigation costs the developer the commit message they had just
written, the panel that was open, and the memory of which action they were taking.

So `github.ts` runs the same flow in a popup, and the tab stays where it is.

```
tab                                popup (…/github/callback?code=…&state=…)
 POST /repo/connection/authorize
 window.open(url) ───────────────►  main.tsx: relayAuthorisation() runs FIRST
 …waits for a message              postMessage({code, state}, origin) → window.close()
 POST /repo/connection ◄────────────┘
 retries the action that failed
```

Four decisions in that picture:

- **The popup does not boot the application.** `relayAuthorisation()` is called in
  `main.tsx` before the theme and before Keycloak, and when it returns true nothing
  else runs. The window exists to pass on two query parameters; loading the shell
  would put a Keycloak round trip — possibly a visible login — inside a window that
  lives for a quarter of a second.
- **The tab spends the code, not the popup.** An authorisation code is single-use,
  and the window holding the developer's work is the one that has to know whether
  spending it worked.
- **The check is the opener, not the path.** `…/github/callback` means two different
  things depending on which window it loaded in: with an opener it is a relay, and
  without one it is the full-tab flow `App` has always completed.
- **A blocked popup takes the tab.** That is a browser setting, and refusing to
  reconnect over it would leave no way through at all.

It cannot be silent, and that is GitHub's decision. `login/oauth/authorize` answers
with `X-Frame-Options: deny`, so there is no hidden frame to run it in, and a grant
that was revoked means a consent screen — which is the point of one. What the popup
does buy is that the *common* case looks silent anyway: when the authorisation still
exists and only the token expired, GitHub redirects straight back and the window
opens and closes faster than it can be read.

**The bar keeps the action that failed.** A 409 with `needs: "github_connection"`
stores the closure, and the notice offers "Reconnect GitHub and push" — one click for
the reconnection *and* the thing the developer actually asked for. It is a button
rather than something automatic for two reasons: a browser grants a popup to a
gesture and refuses one to the catch block of a request that has already failed, and
a consent screen nobody asked for should not open behind anyone's back.
