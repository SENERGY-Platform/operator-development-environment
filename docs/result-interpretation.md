# Automated result interpretation

A run finishes with nobody watching. ODE notices, builds the summary with its own
credential, and then **waits for the developer** before interpreting it — because
the interpretation turn dispatches tools on their behalf and cannot run without
their token. That distinction is the whole design.

## Applies when

Working on `pkg/interpret`, or on the grading of a run against the developer's
`evaluation.yaml`.

**Not this if**: the question is how the run was launched or what the job could
reach — see [experiments.md](experiments.md).

`geltung`: `allgemein` — follows from §5.13, D28 and §3.1 item 5.

## The token, which is the whole problem

An interpretation turn dispatches tools. Every platform read is on behalf of the
developer (§3.1 item 3), so the turn needs *their* token — and a run finishes when
it finishes, which is routinely at three in the morning with nobody connected.
Those two facts do not reconcile, so M9 does not try to reconcile them. It splits
the work at the line §3.1 already draws:

- **The summary is built with ODE's service credential and stored the moment the
  run is terminal**, connected or not. §3.1 item 5 permits a service account for
  exactly Ray and MLflow, and a summary reads nothing else.
- **The turn runs only while a developer's own credential is live.** The SPA holds
  one on its WebSocket and refreshes it there; the connection registers that
  *source* — not a copy of the token — for as long as it is up. Connecting nudges
  the delivery loop, so the wait is seconds rather than a tick.

The two halves meet at the moment the turn starts: **the summary is built again
with the developer's own token before it is injected.** The poller's copy has an
evaluation criterion reading `no_developer_credential`, because `evaluation.yaml`
is in their working copy — injecting that would hand the assistant a permanently
unevaluated criterion while the pane beside it showed the real verdict, which is
two answers to "did this run meet the target" with the model reasoning from the
wrong one. If the rebuild fails, the held summary is used and the criterion says
why it has none; the interpretation still happens.

What that buys, and it is the property worth checking: a developer who was away
comes back to the interpretation. What it refuses to buy: an assistant that read
their devices while they were asleep.

You can watch both halves. Launch from a chat session, let the job finish, and ask
for the summary with no browser open:

```text
GET /experiments/{id}/results
{"status":"SUCCEEDED","finished":true,
 "metrics":{"rmse":0.31,"r2":0.78},
 "evaluation_criteria":{
   "metric":"rmse","threshold":0.35,"value":0.31,"met":true,
   "goal":"minimise","goal_stated":true,"lower_is_better":true,
   "source":"evaluation.yaml at 1dba90e, which is the developer's own (§5.8: no tool may modify it)"}}
```

The conversation is still untouched at this point. Open the SPA — that is the whole
step — and the summary appears in it as a message marked as ODE's, followed by the
assistant's reading of it.

## The message is marked, because it is not the developer's

The injected message is stored with the user role, because that is what a model
reads as input, and it carries a block of JSON. Rendering that in the developer's
own voice would be a lie about who said it, so every stored message now has an
`origin`:

```json
{"seq": 4, "role": "user", "origin": "ode", "subject": "kxq3…",
 "content": [{"type": "text", "text": "A training run you launched from this conversation has finished.…"}]}
```

`origin` is absent or empty for everything a developer typed, including every
message written before M9 — which is what they were. `subject` is the experiment
the summary is about, and it is doing more than labelling: **it is the record that
this run was already delivered.** There is no table saying so. The poller re-offers
a finished run on every tick and after every restart, and the delivery is idempotent
because the conversation itself is the evidence — which also means the two can never
disagree, the way a message and a separate "delivered" flag eventually would.

## An automated turn is still a turn

It goes through the same call as a typed one, with two differences and no
exemptions: it marks the message as injected, and it never takes the session's
title from it. Everything else is the production path — the §3.3 spend cap checked
before anything is written, the session's exposure tier re-read on every iteration,
and the one-exchange-at-a-time rule.

That last one matters more than it looks. If the developer is mid-turn, the
automated turn is **refused and nothing is stored**. Storing the summary anyway
would wedge a user message between an assistant's `tool_use` block and its
`tool_result`, which both native tool protocols reject outright — a conversation
that could never be continued. So the run stays pending and is tried again; the
same is true of a spend cap that has not reset yet.

## The criteria are the developer's, read and never written

`Summary.evaluation_criteria` was M8's one stub: it carried a criterion only when
the run had tagged itself with one, and the comment said reading `evaluation.yaml`
was M9's work. It is done, and two constraints shaped it.

**Read at the run's commit, not at HEAD.** A criterion is part of the code state a
run came from (§5.11 item 7). Grading a six-hour run against a threshold the
developer tightened while it ran would report a run they watched succeed as a
failure, so ODE runs `git show <commit>:evaluation.yaml` and grades against what was
committed with the job.

**Read, never written.** §5.8 lists modifying evaluation criteria among the
capabilities with no tool at all; `write_file` already refuses the path, and nothing
in M9 is a way around it. The parser is a reader; this package has a read-only view
of the repository; and D28 is the same rule from the other side — a recommendation
becomes binding only when the developer promotes it into that file themselves.

The file is YAML and ODE reads a **subset** of it rather than taking a dependency.
That is a real limitation and it fails in the honest direction: anything outside the
subset is refused with the line that stopped it, and the run reports
`criteria_unparseable` rather than being graded against a half-read document.

## A criterion that could not be evaluated is not a criterion that failed

This is D24 outside the profiler, and it is the part most worth reading. `met` is
`true`, `false`, or an object:

```json
"evaluation_criteria": {
  "metric": "mape", "threshold": 5,
  "met": {"status": "not_computed", "reason": "metric_not_reported",
          "detail": "the run logged no mape; it logged r2, rmse. A criterion whose metric was never recorded is not a criterion the run missed"},
  "goal": "minimise", "goal_stated": true, "lower_is_better": true
}
```

Seven reasons, each with a different repair: `no_criteria_file`,
`criteria_unreadable`, `criteria_unparseable`, `no_criterion_stated`,
`no_threshold`, `metric_not_reported`, `no_developer_credential`. A boolean would
have flattened all seven into "the run missed the developer's target", and an
assistant reading that would tell them so — which is a fabricated finding, not a
harsh one.

The last reason is the one to expect. A summary built by the poller has no
developer token, so it carries `no_developer_credential` with the sentence
explaining that `evaluation.yaml` is read on the developer's behalf. It is a fact
about the summary rather than about the criterion, and the criteria are graded the
moment a real token exists.

## The proposal, and the three answers

The injected message asks for a reading of the numbers and **one concrete
adjustment**, on a last line beginning `NEXT STEP:`. A marker rather than a
nineteenth tool: §5.8's table is an allow-list published in full, and
adding a row so the assistant could hand back a structured object would be a change
to the specification rather than an implementation of it. A reply without one
produces `{"status": "not_computed", "reason": "no_proposal_stated"}` — never an
empty string, which would read as "nothing to change".

```text
GET /experiments/{id}/interpretation
{"interpretation":"rmse came in at 0.31, under the 0.35 you asked for…",
 "proposal":{"proposal_id":"7f38e5bee363e3c8",
             "text":"hold folds at 5 and raise lookback_days from 180 to 365, so the next run isolates the window."},
 "decisions":[]}
```

Then §5.13's last sentence:

```text
POST /experiments/{id}/interpretation/decision
{"proposal_id":"7f38e5bee363e3c8","decision":"edited",
 "edited":"raise lookback_days to 270 rather than 365; the series does not go back further",
 "note":"the window is bounded by what the device has recorded"}

201 {"decision":{"decision":"edited","binding":false,
                 "proposed":"hold folds at 5 and raise lookback_days from 180 to 365…",
                 "edited":"raise lookback_days to 270 rather than 365…"}}
```

Three things about that record. It is **append-only** — a developer who changes
their mind adds a row, and "rejected, then accepted" must not read as "accepted".
It carries **what was actually proposed**, copied in rather than referenced, so it
stays readable after the interpretation has been recomputed. And `binding` is
always `false` and is serialised anyway, so a reader meets D28 rather than having
to know it: accepting records agreement and launches nothing.

It is keyed on a fingerprint of **the proposal's own text**, not on the
interpretation it appeared in — the same choice `RuleDecision` makes in M6. That is
what makes a rejection survive: re-read the run, the summary is rebuilt from MLflow
and the proposal re-derived from the conversation, the fingerprint comes out the
same, and the rejection still stands. A materially different proposal is a
different fingerprint and is asked about again, which is the behaviour you want.
Deciding on a fingerprint that no longer stands is a **409** with the repair rather
than a silently recorded agreement with something the developer never read.

There is no LLM tool for any of this. A model that could accept its own proposal
would be grading its own work, exactly as one that could write a `ProfileOverride`
would (D21, D28).

## What is stored, and what is not

The one table M9 adds is `ode_proposal_decisions`. Everything else is recomputed:
the summary from MLflow, the interpretation from the conversation the assistant
wrote it in, the proposal from that text. It is the split §5.4.3 makes — a
recomputable artifact stays out of the database, and a record of human judgement
goes in — and it is why a second copy of the interpretation was not kept: two
records of one exchange diverge, and the conversation is the one the developer
actually reads.

Without Postgres the decisions are in memory, and the startup warning now says what
that costs: a proposal the developer rejected comes back after a restart as though
they had never been asked.