# Chat, streaming and the detached exchange

A chat turn does not belong to the connection that started it. That one decision
explains the cancellation semantics, the reconnect behaviour, why everything
streams over `/ws` against what SPEC §5.7 says, and why a token is read per tool
call rather than per request.

## Applies when

Working on `pkg/chat`, on `pkg/api/ws.go`, or on the SPA's socket handling.
Also when a bug report says an exchange "died on reload" or "kept running after I
closed the tab" — those are the two halves of the same design and only one of
them is a fault.

**Not this if**: the question is whether a tool may run at all, which is the tier
gate — [authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md).

`geltung`: `allgemein` for the exchange model; the SSE comparison is a recorded
decision rather than a general claim.

### A chat exchange is detached from the connection that started it, and this is the load-bearing decision in the whole surface

`chat.Exchange` is a turn running on
the process's own context with its own ceiling (`chat_exchange_timeout`), publishing
to zero or more subscribers. A connection is a *view*, not the owner.

It is worth knowing why it is at the level of the whole exchange rather than of
individual slow tools. A tool result is only useful inside a conversation: if
`profile_series` ran as a background job but the exchange died with the socket, the
profile would complete into a cache nobody was reading and the conversation would
have lost the turn. Detaching the exchange makes every tool inside it survive for
free, and leaves nothing for a per-tool job registry to add.

Three consequences:

- **Closing a tab is not cancelling.** Detaching a view leaves the turn running;
  `chat_cancel` abandons it. The two are separate messages because they are separate
  intentions, and the Stop button sends both.
- **Reconnecting resumes.** `chat_attach` subscribes to whatever is still in flight,
  and `Subscribe` replays from the start of the turn — so a reattached view sees the
  whole thing, not just the remainder. The SPA attaches on every socket open.
- **A slow subscriber is dropped, not waited for.** If a client stops draining, the
  exchange closes that subscriber rather than stalling the work; the client re-reads
  the persisted messages, which are the source of truth in any case.

### Streaming is all on `/ws`, and this departs from §5.7 deliberately

The spec says
the provider stream is "Streamed to the SPA over SSE", and the first implementation
did that. It was wrong in practice for the reason `ws.go` was written: between the
`tool_call` event and its `tool_result` nothing is written, and a 3-second tool
produced 3.001s of measured silence — so any proxy idle timeout closed a healthy
connection mid-exchange.

SSE can be kept alive with a comment heartbeat, and that is a real fix; the reason it
is not the one here is that it would leave ODE maintaining two streaming paths with
two sets of liveness and cancellation semantics, when the WebSocket already had the
harder half working. Note the mechanism either way: **the WebSocket survives idle
because it pings every 30s ([ws.go:242](../pkg/api/ws.go#L242)), not because it is a
WebSocket.**

Request/response stays REST — sessions, the tier, the audit, admin, MCP — because a
status code means something there and those routes are worth being able to curl.

### A connection outlives its token, and a chat turn can too

The WebSocket
handshake authenticates once and the socket then lives as long as the tab, while
the SPA's access token is refreshed on a thirty-second horizon. The connection
therefore holds a `sessionToken` that an `auth` frame replaces, and every operation
reads it at the moment it runs rather than copying it per connection. The chat
engine goes one step further: `chat.TokenSource` is read per *tool call*, because an
exchange is detached from the request that started it — a turn running twelve tool
iterations, or a confirmation a developer approves ten minutes later, would
otherwise dispatch with a credential that expired while it waited. Both failures
looked like platform faults and both disappeared on reload, which is the worst kind
of bug report to receive.

### `time.Duration` marshals as nanoseconds

A field named `duration_ms` carrying a
`time.Duration` is wrong by a factor of a million and nothing about the JSON says
so — both are plausible integers. `tools.Millis` exists to make the name true. The
frontend's contract check is what caught it; that is the second time that check has
paid for itself.