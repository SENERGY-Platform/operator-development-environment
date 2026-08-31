# Chat, streaming and the detached exchange

A chat turn does not belong to the connection that started it. That one decision
explains the cancellation semantics, the reconnect behaviour, why everything
streams over `/ws` against what §5.7 says, and why a token is read per tool
call rather than per request.

## Applies when

Working on `pkg/chat`, on `pkg/api/ws.go`, or on the SPA's socket handling.
Also when a bug report says an exchange "died on reload" or "kept running after I
closed the tab" — those are the two halves of the same design and only one of
them is a fault.

**Not this if**: the question is whether a tool may run at all, which is the tier
gate — [authorisation-and-exposure-tiers.md](authorisation-and-exposure-tiers.md).

The SSE comparison near the end is why WebSocket was chosen for *this* surface,
not a claim that SSE could not carry it — it says in as many words that a
heartbeat would also have worked.

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
  whole thing, not just the remainder. The SPA attaches on every socket open, and on
  every mount of a conversation: opening a session again after switching away is a
  reconnect as far as this is concerned, since the switch detached the view.
- **A slow subscriber is dropped, not waited for.** If a client stops draining, the
  exchange closes that subscriber rather than stalling the work; the client re-reads
  the persisted messages, which are the source of truth in any case.

### `chat_watch` is the one subscription that is not about a conversation

The detached exchange has a cost the three consequences above do not name: the SPA
shows one conversation at a time, so switching sessions leaves a running turn with
nothing in the tab watching it. The developer who starts a five-minute profile and
carries on in a second session is the case the whole design is for, and it was the
one case where the answer arrived unannounced.

`chat_watch` ([ws_chat.go](../pkg/api/ws_chat.go)) answers it, and it is
deliberately not a variant of `chat_attach`. An attach is a *view onto one turn*
and carries its events; a watch is one line per state change, for every session the
caller owns, carrying no content at all — `running`, `waiting`, `idle`. Reading the
reply is still the attach's job. The distinction matters at the size a panel runs
at: watching six conversations through six attaches means six turns' worth of text
deltas decoded and thrown away.

Three properties are load-bearing:

- **Per developer, not per session.** "Which of my conversations wants me" is one
  question. Subscribing per session would also mean re-subscribing whenever a
  session is created, which is exactly when the answer matters.
- **It opens with a snapshot.** `Engine.Activities` reads the live exchanges out of
  memory, so a reload during a long turn finds the conversation marked busy rather
  than silent until it ends. The subscription is registered *before* the snapshot is
  read: a turn starting between the two then arrives as an event rather than falling
  into the gap. States are absolute, so a repeat says the same thing twice.
- **`waiting` is separate from both.** A held call
  ([hold.go](../pkg/chat/hold.go)) keeps its exchange *running* while it waits for
  the developer, and the native confirmation path *ends* the turn with the
  confirmation stored — so without this state the conversation that most needs the
  developer would be the one reporting that it wants nothing, in two different ways.
  The store is asked once, when a turn ends, rather than per session in a listing.

What is not here is unread state. "You have not looked at it since it finished" is
the client's knowledge — the engine has no idea which pane is on screen — so the
engine reports transitions and the SPA keeps the mark until the conversation is
opened. See [frontend.md](frontend.md).

A watcher that stops draining is dropped and its channel closed, for the reason an
exchange subscriber is: an engine that blocked there would let a wedged browser tab
stall a turn. The client re-subscribes and takes a fresh snapshot, which is the
whole recovery — this state is in memory, not a log.

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

### A confirmed tool is *held* when the provider runs its own tool loop

The CLI provider (§5.7) does not hand its tool calls to the engine; it runs its own
loop against ODE's MCP endpoint. D11's pause lives in the engine's loop, so a
confirmed tool arriving over MCP has no pause to sit in — and it used to be refused
outright with `confirmation_unavailable`, which made `run_code`,
`propose_operator_input`, `propose_data_selection` and `launch_experiment`
permanently unreachable on that provider, while telling the developer to run them
"from the ODE interface", which dispatches no tool by hand.

The call is held open instead ([hold.go](../pkg/chat/hold.go)). What makes that
correct rather than a workaround is **where the waiting happens**: the CLI is never
asked to surface a decision, it just sees one slow tool call. ODE waits, because ODE
is what has the developer's session in front of them. The three alternatives are all
worse — auto-approving defeats the point of a confirmed tool, refusing removes four
tools, and asking the client to render a decision is a protocol ODE does not have.

A hold is a wait *inside a running exchange*, which is what makes it a different
shape from `Engine.Confirm`:

|  | `Confirm` | `Hold` / `Decide` |
| --- | --- | --- |
| Precondition | no exchange running | an exchange **is** running |
| The turn | paused, and resumes from the decision | never stopped |
| Where the result goes | a new provider turn, as a user message | back to the MCP call, as its tool result |
| Answered by | `chat_confirm`, which streams | `chat_decide`, which answers once |

Two consequences worth knowing. The decision is sent on a different WebSocket
message *because* the turn is still streaming: routing it through `chat_confirm`
would have the SPA subscribe a second time to the exchange it is already watching,
and `Exchange.Subscribe` replays history into a view that already has it. And the
pending confirmation carries `out_of_band`, which is computed from the live hold
registry rather than stored — being held is a fact about now, and nothing is holding
anything after a restart.

The bound is `chat_confirmation_timeout` (default `5m`), and it is not
`chat_exchange_timeout`: it has to fit *inside* the provider's own turn, or the turn
ends underneath the card the developer is reading and their approval runs a tool
whose caller has gone. ODE also writes a per-server tool timeout into the CLI's
generated MCP config, so `MCP_TOOL_TIMEOUT` in ODE's environment cannot cut a
legitimate hold short.

### A conversation can change workbench, and the history has to be told

Which workbench a conversation acts in — whose checkout `write_file` writes into,
whose kernel `run_code` runs in — is chosen when the session is opened, which is
before the developer necessarily knows what the conversation will turn out to be
about. `PUT /chat/sessions/{id}/workbench` is the repair. An empty `workbench_id`
clears the assignment, which is the state every session written before workbenches
is already in and reads as *my only one*.

Two things about it are not obvious.

**It is refused while a turn is running.** A turn reads the session once, at its
start, and carries that workbench through every tool call it makes — so a move
underneath it would have one turn writing into two checkouts while reasoning about
the first. The refusal is `chat.ErrInvalidRequest`, so 400, and the SPA disables the
control rather than letting the developer find out that way. One narrow window
stays: `start()` reads the session before it registers its exchange and writes it
back when it titles an untitled one, so a move landing between those two is lost and
costs a second move on the very first message of a fresh conversation. Closing it
needs a session-level write lock the engine does not have.

**The move appends a message to the conversation.** Everything above it — every file
read, every path written, every cell run — happened in another checkout, and a model
handed that history with no marker goes on believing the files it wrote are still
there. The note is `OriginODE` with subject `workbench:{id}`, the same mechanism
§5.13's run summary uses, and it is *appended* rather than sent through
`SendInjected`: a move is not a question, and starting a turn to have it answered
would spend the developer's §3.3 budget on the word "understood".

That appending is the one shape change in the history. Nothing else puts a message
in without a turn following it, so nothing else could leave two user messages in a
row — and consecutive same-role messages are rejected by both native protocols, the
same constraint `repairUnansweredToolCalls` merges into the following turn for. So
`conversation()` coalesces consecutive user turns on the way out, in chronological
order, which is what keeps a `tool_result` first in the turn that answers its call.
On the way out and not in the store, for the reason the tool-call repair gives: the
stored history is the record of what happened, and what the provider sees is a
reading of it that the protocol accepts.

### `time.Duration` marshals as nanoseconds

A field named `duration_ms` carrying a
`time.Duration` is wrong by a factor of a million and nothing about the JSON says
so — both are plausible integers. `tools.Millis` exists to make the name true. The
frontend's contract check is what caught it; that is the second time that check has
paid for itself.