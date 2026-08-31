# MCP for clients that are not ODE

ODE publishes its tools over MCP, and the transport is real: same registry, same
dispatcher, same tier gate. What it is *not* is a standalone product surface. Four
things every call depends on — that a chat session exists, its exposure tier, its
workbench, and a developer available to answer a confirmation — are written by the
ODE frontend and only ever read here. So a third-party client (Claude Desktop, a
hand-rolled agent, another Claude Code) reaches the read surface and stops there,
and it stops there permanently rather than intermittently.

## Applies when

Answering whether ODE can be used "as just an MCP server", pointing a client other
than ODE's own `claude` subprocess at `/mcp`, or diagnosing a client that connects,
lists tools and then gets `confirmation_unavailable` on everything worth calling.

**Not this if**: the question is *how* a held confirmation works — the hold, its
timeout and why `Decide` is not `Confirm` are in
[chat-and-streaming.md](chat-and-streaming.md). Nor this if the question is whether
a tool may run at all, which is the tier gate in
[authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md).

`geltung`: `allgemein` — each item below follows from the code, not from one
observed client.

### The transport is mounted for the CLI provider, and only then

`deps.MCP` is built only when `claude_cli_enabled` is set
([pkg.go:540](../pkg/pkg.go#L540)), because mounting it unconditionally would
publish ODE's tools to any MCP client for no configured reason. An external client
against a deployment that has no CLI provider configured therefore gets a plain 404
from the router, with nothing in the logs to explain it.

The deployment precondition is unchanged and is the sharper one: ODE does not
validate tokens, so `/mcp` has to sit behind the platform gateway exactly as every
REST route does. Exposing it to a laptop by any route that goes around the gateway
authenticates the caller with nothing.

### An MCP request is scoped to a chat session it has no way to create

Every call must carry `X-ODE-Session`, and tier, workbench and the standing answer
are read from that session per request — never from a header
([mcp.go:157-175](../pkg/mcp/mcp.go#L157-L175)). That is the property the gate rests
on, and it is also what makes the transport dependent: a session is created by
`POST /chat/sessions`, starts at L0, and is raised by `PUT /chat/sessions/{id}/tier`.
Neither is an MCP tool, and `set_exposure_tier` is in `tools.Denied()` deliberately —
a model that can raise its own data exposure is not subject to the tier at all.

So an external client either has a developer driving ODE's REST API alongside it, or
it sits at L0 with no workbench: `list_devices` and the ontology, and `write_file`
resolving to no checkout.

### A confirmed tool needs an exchange in flight, which an external client never has

`Hold` starts with `Attach(sessionID)` and returns `held=false` when no exchange is
running ([hold.go:99-102](../pkg/chat/hold.go#L99-L102)); the MCP layer then answers
`confirmation_unavailable`. An exchange is running only while ODE is streaming a
turn from one of *its own* providers. A client holding the conversation itself
creates no exchange, so it reaches no confirmed tool — ever, rather than
intermittently.

This is the single biggest gap and it is a consequence of the design rather than an
oversight: the hold exists because ODE has the developer's session in front of it,
and a client that is not ODE has nobody to ask. Which tools that removes:

```bash
grep -c 'Confirm:\s*true' pkg/tools/surface.go   # confirmed
grep -c '^\s*Definition{' pkg/tools/surface.go   # declared
```

At the time of writing, fifteen of forty-three — `run_code`, `launch_experiment`,
both proposals, the import and export writes, and every simulation write. What
survives is the read surface — including `list_files` and `read_file`, which read
the working copy and ask nobody — plus `write_file` and `render_chart`.

### Git is not on the tool surface at all

`write_file` writes into the working copy and stages nothing. Commit, push, stash,
fetch, scaffold, the commit-message draft and workbench management are REST routes
under `/repo` and `/workbenches` ([api.go:320-345](../pkg/api/api.go#L320-L345)),
because §5.11 makes them the developer's explicit actions. An external client can
therefore edit an operator and has no way to commit it.

GitHub itself is not a per-call concern — the OAuth code flow runs in the browser
against the SPA's callback and the token is sealed per developer — but the flow
needs that browser. Connecting, and reconnecting when a grant goes stale, is
frontend work with no tool behind it and correctly so.

### What a standalone client would need, stated as preconditions rather than a plan

Three, and the middle one is a design question rather than plumbing:

1. A way for a client to obtain a session and a tier without the SPA.
2. A route for a confirmation to reach a developer who has no ODE window open.
   MCP elicitation is the obvious candidate and moves the decision into the model's
   own client — which is the party the confirmation exists to check.
3. OAuth protected-resource metadata (RFC 9728), if the client is to authenticate
   itself rather than be handed a bearer by a shim. Nothing serves it today.

None of these is decided. Whether they should be is tracked outside this repository.
