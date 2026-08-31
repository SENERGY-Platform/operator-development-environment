# Authorisation and the exposure tiers

Who ODE trusts, what it checks itself, and the single gate every LLM tool call
passes through. Four of the five things below are properties that stop holding
quietly rather than failing a build, which is why each says what it would look
like when broken.

## Applies when

Changing anything on the path from a request to a platform read: the auth
middleware, the tool registry, the dispatcher, a session's tier, or the admin
limits. Also before exposing ODE anywhere new — the first item is a deployment
precondition, not a code note.

**Not this if**: the question is what a *tool response* may contain rather than
whether it may run — the budgets and the never-null rule are in
[profiler-contracts.md](profiler-contracts.md).

### ODE does not validate tokens, and must therefore sit behind the gateway

Signature, expiry and audience are checked centrally by the platform API
gateway; `pkg/auth` parses claims unverified to read `sub` and `realm_access`.
That is correct for gateway traffic and unsound for anything else — a
cluster-internal caller reaching ODE's service DNS directly is authenticated by
nothing. Since JupyterHub singleuser pods run developer and LLM-authored Python
in the same cluster, the M10 NetworkPolicy is what makes the assumption hold —
specifically its refusal of private IP space, which is the half of that control
that has nothing to do with the internet. The policy itself is deployment
configuration and is not part of this repository. Do not expose ODE before it
is applied.

### Read permission is not data permission

`models.Read` governs device
metadata; `models.Execute` governs reading a device's *data*. `/devices` still
lists under `Read`, because it serves metadata. Everything that reads or offers
to read a series — `/quick-profiles`, `POST /profiles` — is scoped to `Execute`,
because timescale-wrapper checks `Execute` itself and would otherwise refuse the
read after the developer had already chosen the series. `POST /selection` is scoped
to `Execute` for the same reason: it offers series to read.

### A launch authorizes its input topics, because Operator Lib authorizes nothing

The tier gate decides which *tools* a session may call. It says nothing about
which *series* a run reads, and those are different questions: a developer at any
tier may launch an experiment, and what that experiment reads is decided by the
input topics in its deployment config.

Operator Lib performs no check of its own. `provide_historic_data` reads whatever
its `inputTopics` name, so whichever service wrote those topics is the only party
able to refuse. There are two such services and they now apply the same rule from
the same code: the flow engine when it deploys a pipeline, and `experiments.Launch`
here. The rule lives in `lib/access` of `analytics-flow-engine` — id extraction
per filter type, `Execute` on `devices`, `import-instances` or
`analytics-pipelines`, and a refusal for anything it cannot resolve.

Three properties are deliberate:

- **It fails closed.** An unknown filter type or an operator reference that names
  no pipeline is refused rather than skipped. A topic that skips the check is a
  topic that is read unauthorized.
- **It runs before the package is built**, for the same reason the dirty
  working-copy refusal does: everything past that point spends something.
- **It authorizes the topic, not the storage.** A topic backed by timescale and
  one replayed from Kafka are authorized identically, which is what makes this
  cover the Kafka path — where there is no per-read check at all, the broker
  taking no credentials.

The checker is a required dependency: `experiments.New` refuses without one
rather than launching unauthorized while looking like it works.

### A run carries no database credential

Before this, a run's deployment config carried `ts_conn`, a shared DSN reaching
every series. A run executes the developer's own Python, so that was a credential
handed to code ODE did not write — `os.environ["CONFIG"]` is all it takes to read
it back out, and no gate in this document stands between a developer and that
string.

A run is now given `ts_wrapper_url` instead. Operator Lib supports both and
prefers a DSN where it has one; ODE gives it none, so it reads through
timescale-wrapper with the `SENERGY_TOKEN` the job already carries, and
timescale-wrapper checks `Execute` on the device itself. That restores the
authority the pre-operator-path read had — the reason `POST /selection` and the
profile reads are scoped to `Execute` in the first place — and it is enforced at
read time rather than only at launch.

A deployed operator is the other way round: the flow engine sets it a DSN and
gives it no token, and its code is a reviewed image rather than a working copy.

### Every tool call goes through one `Dispatch`, and that is the whole tier argument

`pkg/tools` is written so there is no path to an executor that skips a
check: the executor lives in an unexported field, `Dispatch` is the only caller, and
the order — exists, implemented, tier, confirmation, run — is the guarantee the
surface is built on rather than an implementation detail. The MCP transport shares the same
dispatcher for the same reason; a second tool list would be a second, weaker gate.
If you add a tool, add it to the registry, not beside it.

### A denied capability has no tool, and that is different from refusing one

The
four operations §5.8 forbids are absent from the registry, `NewRegistry` refuses to
register one, and a call to a denied name is answered "unknown tool" rather than
"forbidden". Naming it forbidden would describe a capability boundary the model will
then try to talk its way around; "no such tool" ends the line of enquiry. There is a
test asserting all of this, because absence is the kind of property that quietly
stops holding.

### The admin tier ceiling binds continuously, not only when raising

An earlier
version checked `max_tier` only on the way up, so a session already at L2 kept its
L2 tools after an administrator lowered the maximum — a policy that applied to
future sessions only. `chat.Engine.effectiveTier` now clamps on every read of a
session's tier, including the MCP path, and fails closed to L0 if the policy cannot
be read. The stored tier and the effective tier can therefore differ, which is why
`SetTier` compares against the stored one.

### Spend accounting is per provider request, not per exchange

§3.3 says "recorded
per request", and it has to be: the tool loop makes several provider calls, and
recording only the aggregate at the end would let one exchange overrun a cap by its
whole length — bounded by nothing but `llm_max_tool_iterations`. The cap is checked
before each call and against spend already recorded, so it can be overshot by at
most one request. Refusing on a prediction instead would refuse requests that would
have fit.

## The tool table, and what has no tool

```bash
curl -s -H "Authorization: Bearer $TOKEN" $BASE/llm/tools | jq '{
  at_l0: (.tiers[] | select(.tier=="L0") | .available),
  denied: (.denied | keys)
}'
```

`denied` is §5.8's "no tool exists" list — changing the exposure tier, changing
admin limits, writing a `ProfileOverride`, promoting a recommendation, deciding a
relational rule (M6's addition, by the same reasoning as the override). They are
absent from the registry rather than refused at dispatch, and `NewRegistry` refuses
to register one. A refusal would still advertise the capability and invite the model
to argue with it.

## Watching the tier block a tool

Create a session — it starts at L0, which §3.2 makes the default rather than a
choice a caller has to remember:

```bash
SID=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{}' $BASE/chat/sessions | jq -r .id)
```

Sending a message is a WebSocket operation, so this one needs a WebSocket client
rather than curl — `websocat` will do:

```bash
websocat -H="Authorization: Bearer $TOKEN" ws://localhost:8080/ws <<EOF
{"type":"chat_send","id":"r1","payload":{"session_id":"$SID",
 "message":"show me the actual values for any power series you can find"}}
EOF
```

Frames come back as `accepted`, then one `event` per item of the stream, then `done`.
At L0 the assistant can resolve the intent and rank candidates,
and `preview_series` is not offered to it at all; if it asks anyway, the dispatcher
answers with §3.2's refusal verbatim:

```json
{"blocked_by_tier":"L0","required":"L2","tool":"preview_series",
 "hint":"the developer controls this. Ask them to raise the exposure tier to L2 …"}
```

Two more messages are worth knowing by hand. `chat_attach` subscribes to a turn
already in flight and replays it from the start, which is what a reconnect does:

```json
{"type":"chat_attach","id":"r2","payload":{"session_id":"<SID>"}}
```

`chat_cancel` abandons the turn. Closing the socket does **not** — that only detaches
your view, and the exchange runs on:

```json
{"type":"chat_cancel","id":"r3","payload":{"session_id":"<SID>"}}
```

Worth trying, because it is the behaviour the design turns on: start a message, close
the socket while a tool is running, reconnect, `chat_attach`, and the turn is still
there. `GET /chat/sessions/$SID` shows the messages persisted either way.

Raise the tier, and the same request works:

```bash
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"exposure_tier":"L2"}' $BASE/chat/sessions/$SID/tier

# Every change is logged with its time and its user (§3.2).
curl -s -H "Authorization: Bearer $TOKEN" $BASE/chat/sessions/$SID/tier-changes | jq .
```

## Auto mode: a standing answer, not a weaker gate

A developer reading a dataframe meets `run_code`'s confirmation dozens of times an
afternoon, and most of those are `df.head()`. Auto mode is a per-session switch —
`PUT /chat/sessions/{id}/auto-run`, off by default — that answers that question in
advance for code the backend *recognises*.

**It is not a security control, and the code says so in every place it appears.**
It cannot be one. Python has no sound static answer to "is this safe": a denylist
is walked around with `getattr(__builtins__, "ev" + "al")` or `__import__`, and the
party writing the code is the model — the same party the confirmation exists to
check. What is actually protecting anything is unchanged and is stated in
[kernel-and-repository.md](kernel-and-repository.md): the code runs in the
developer's own pod under the developer's own platform token, and it can reach
exactly what they can reach. Auto mode changes who is asked, never what is
possible.

What `pkg/plaincode` does is *recognition*, not detection. It knows a fixed
vocabulary — reading a dataframe's shape, its columns, a column's mean, printing,
comprehensions over those, reading a file out of the checkout and matching over
what was read — and anything it does not positively know is unrecognised. The
failure it produces is therefore an unnecessary prompt, which is the end it is
deliberately wrong at. A recogniser that erred the other way would be the boundary
it refuses to be.

What the vocabulary holds is decided against real confirmations rather than
guessed. Measured over 241 `run_code` cells from four days of one developer's
sessions, of which they approved 232: nine were recognised. Adding the members
those cells were refused for — `re.search` and `.group` over source the cell had
just read, `joinpath` where `/` would have passed, `np.array`, `.tolist`,
`json.dumps` — takes it to 33. The remaining 208 are not a gap to be closed. They
contain `subprocess`, `sys`, `importlib`, `inspect`, `urllib`, a `!` shell escape,
an `open` in write mode, or a `def`: the subset excludes those on purpose, and a
vocabulary widened until they passed would be the rubber stamp this is not. Two
things follow that are worth stating plainly. Enumerating a tree stays out
(`glob`, `rglob`, `iterdir`, `walk`) even though reading a named file is in — a
cell told which file to read has been told what to look at, and one that walks the
pod has not. And auto mode is a standing answer about a *class of act*, not about
whether a cell was a good idea: four of the 33 are cells the developer rejected on
the merits, all four read-only file dumps, and under a standing answer they would
have run.

Three properties bound it, and each has a test in `pkg/tools/dispatch_test.go`:

- a session that did not turn it on is unaffected;
- unrecognised input is still confirmed;
- **a confirmed tool with no recogniser of its own can never be waived.** The
  predicate lives on the tool (`Definition.AutoApprove`) and exactly one tool has
  one. No session setting, and no configuration, gives `create_export` or
  `delete_import_instance` a way past their confirmation — they carry no predicate
  and nothing can attach one at runtime.

There is no LLM tool for the switch. `set_auto_run` is in `tools.Denied()` for the
same reason `set_exposure_tier` is: a model able to stop itself being asked is not
subject to the confirmation at all.

## The same gate over MCP

The MCP transport is the CLI provider's route to the tools, and it enforces the
same gate through the same dispatcher. Worth checking by hand, because a second
transport is exactly where a bypass would hide:

```bash
# The session id is a header, because the tier lives on the session.
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-ODE-Session: $SID" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",
       "capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}' $BASE/mcp

curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-ODE-Session: $SID" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' $BASE/mcp
```

The advertised list is the session's tier, read from the session on every request —
never from a header a client could set for itself.

### And the same confirmation

A confirmed tool reached over MCP is **held**, not auto-approved and not refused.
The call blocks while the request appears on the developer's screen as the same
card the native providers produce, and their decision becomes its result — so D11
binds identically whichever transport the model came in on. Details in
[chat-and-streaming.md](chat-and-streaming.md).

Two properties are worth checking rather than assuming, because a hold is a place a
bypass could hide:

- The tier gate comes **before** the hold. A tool the tier forbids is refused by the
  dispatcher, not put to the developer as something an approval could get past.
- The tier is re-read **at the decision**, not taken from the call that proposed it.
  A developer may propose at L2, lower the session while the card is on screen, and
  approve — and the approval then runs at L0, which refuses. Trusting the recorded
  tier would make a pending confirmation a way around the ceiling.

## Admin limits

Behind the `admin` realm role. A cap is checked before each provider request, and
answered with a structured refusal rather than a generic error:

```bash
curl -s -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"period":"24h","token_cap":50}' $BASE/admin/limits/$SUB

# Then, once 50 tokens are spent, the next message is refused:
# 429 {"error":"limit_exceeded","scope":"user","kind":"tokens","cap":50,
#      "spent":43306,"period":"24h0m0s","resets_at":"…"}
```

Two things the settings surface tells you that a plain form would hide. Which
fields this build actually enforces — the Ray cap of §3.3 is stored now and
enforced from M7, and the kernel resource caps are stored and **never** enforced
by ODE, because a pod's resources are KubeSpawner's and the Hub's spawn API
selects a profile rather than carrying overrides. They belong in
the KubeSpawner profile itself, and an administrator setting
one here should know it does nothing.
And whether a cost cap can bind at all: cost is estimated from `llm_pricing`, so a
model with no configured price accrues zero and the cap silently does not apply to
it. `GET /admin/usage` marks those requests `unpriced` rather than showing them as
free.

## probe_export_data at L0, and where the line actually is

`probe_export_data` counts rows and sits at **L0**, which looks like a value read
sitting below the tier that governs value reads. It is worth stating why it is
not, because the argument is the line itself.

The tier model separates *what the data is* from *facts about the store holding
it*. `probe_availability` is already at L0 while reporting `from` and `to` —
timestamps derived from the rows themselves — because a window says nothing about
any value in it. A row count is the same kind of fact: `count("power")` answers how
many rows carry something in that column, and no answer it can give distinguishes
a kilowatt from a megawatt. The request carries `groupType: count` on every column,
so there is no shape of response in which a value could arrive.

What made this worth building at L0 rather than L1 is the sequence it belongs to.
`create_export` is L0 with a confirmation, so a session at L0 can create an export
— and the export worker's most common misconfiguration stores rows in which every
column is null, which the export listing and stored bytes both report as healthy.
A verification the creating session cannot perform is not a verification; it is a
note in a log for somebody else. So the tool that checks the export is at the tier
that can create one.

The boundary is unchanged where it matters: reading what the rows *contain* is
still `preview_series` at L2, and every statistic over them is still
`profile_series` at L1. `probe_export_data` reports `values: 0` in its own answer,
as `probe_availability` does, so the claim is checkable from the response rather
than being a property of the code.

## run_code, and what the tier does not cover

`run_code` is tier **L0** with a confirmation, which is what §5.8's table says
and is worth understanding rather than reading past. Confirmed code runs with the
developer's own token, so it can reach values `preview_series` would refuse at
L0. The control is the developer's confirmation, not the tier — the same control
D11 puts on every other consequential action — and it is why every execution is
a decision rather than a default.

What ODE does do is keep the accidents out of the record: the platform token is
redacted from what `run_code` returns, so a `print(os.environ)` while debugging
does not put a live credential into a conversation that is persisted to Postgres.
That is hygiene, not a boundary. Code that deliberately encodes the token defeats
it, and nothing here pretends otherwise.
