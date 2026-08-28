# Relations: candidate sets and rule confirmation

Multi-device conditional patterns. The part that is not obvious is where a
candidate *set* comes from: a wiring graph, a device group and an aspect subtree
are three different kinds of evidence about which devices belong together, and
they are not equally trustworthy.

## Applies when

Working on `pkg/relations`, or reading a proposed rule and wanting to know how
much its grouping is worth.

**Not this if**: the question is how a single series behaves rather than how two
relate — that is the profiler, see [profiler-reads.md](profiler-reads.md).

`geltung`: `allgemein` for the origin ranking and the confirmation model, which
follow from §5.5 and D11; the detector thresholds are `einzelfall`.

## Why a graph outranks a device group

§5.5 asks for existing groupings to be preferred over constructed ones, and there are
two of those. `/device-groups` says which devices somebody grouped. `/graphs` says how
they are **wired**, which is a stronger statement, and `models.Graph.Valid` is what
makes it precise: a directed acyclic graph whose outgoing edge weights sum to 100 per
node, with exactly one node that has no outputs and may not be a device. That is a
flow topology — sub-metering or aggregation — and the intermediate nodes are whatever
the site is structured by: a busbar, a location, a business unit.

So one graph answers two different questions, and conflating them would produce
confident nonsense in one of the two cases:

| Origin | What the topology says | What a rule over it means |
| --- | --- | --- |
| `graph_siblings` | these devices converge on one node | a genuine co-occurrence question — they are metered together, and `graph.via_name` says where |
| `graph_flow` | one device measures what the others feed | **containment.** "The sub-meter runs whenever the main meter runs" is arithmetic, not a finding |

The flow set is still proposed, for the case that is not arithmetic: a device drawing
while what it feeds reads idle is a metering or wiring fault, and that is exactly the
anomaly a rule in that direction defines. The set says so in its own notes rather
than leaving a developer to work it out, and the lift filter rejects the degenerate
direction on its own — a main meter is almost always active, so a rule with it as
consequent scores a lift near 1.

**A graph legitimately reaches outside the aspect, and that is the point.** A site
meter is not "in the kitchen", so intersecting a graph with the aspect's own devices —
the way a device group is intersected — would drop precisely the cross-level pair a
sub-metering question is about. Those neighbours are resolved on their own instead:
one device read each under `Execute`, their series enumerated from the device type and
their units from the ontology index, bounded by `relation_max_graph_neighbours`
because a graph of a whole site names hundreds of devices. A member reached that way
carries `from_aspect: false`, and the pane marks it, because a device from outside the
room you asked about is otherwise a puzzle.

One field is easy to misread. `graph.weight` is an **output split**, not a
contribution share: the validator requires each node's outgoing weights to sum to 100,
so a device feeding exactly one node always reads 100 and says nothing. Below 100
means its flow is allocated across several downstream nodes — one meter split 60/40
across two business units — and only then is the figure a fact about that edge.

Then take a set and press **Relate**. Four things happen in an order that matters:

1. **Every participating service is profiled.** Not a second threshold invented
   here — `activity_pattern.active_threshold` is a confirmable field (§5.10), so a
   developer who already corrected the idle/active split for a series has corrected
   it for every rule that series takes part in. The member row says `detector` or
   `confirmed`, because a rule computed against a corrected threshold is a different
   claim.
2. **The members are aligned by one batched query at one bucket**, and the bucket
   comes from the *coarsest* member. That direction is the whole point and is easy to
   get backwards: a grid finer than the slowest series leaves it with empty buckets
   between its arrivals, and an empty bucket is not an idle device.
3. **Each series becomes idle and active**, with the profiler's hysteresis band. A
   bucket with no reading is `unknown` rather than idle — the one mistake that would
   make every rule here wrong in the same direction.
4. **Every pair is tabulated and conditioned** on hour of day and weekday/weekend.

What comes out is the sentence §5.5 opens with:

```json
{"statement":"the oven active → the kitchen lights active",
 "anomaly":"the oven active while the kitchen lights idle",
 "confidence":0.8571,"lift":6.8571,"support":0.125,
 "samples":420,"violations":60,"strength":"likely",
 "exceptions":[{"dimension":"hour_of_day","bucket":"06:00-12:00",
                "from_hour":6,"to_hour":12,"confidence":0,"drop":0.8571,"samples":60}],
 "advisory":"candidate only: not a configured rule, not an anomaly definition, and never
             read by an operator or a training job until a developer confirms it (§5.5, D28)"}
```

Read the three figures together, because each answers a question the others cannot.
`confidence` is P(consequent | antecedent). `lift` is how far that exceeds the
consequent's own base rate — and it is what rejects the most common false finding in
association mining, a consequent that is simply usually true: a light left on all
year scores a confidence near 1.0 against every antecedent and a lift of 1.0 against
all of them. `samples` and `violations` are the evidence, because a confidence of 1.0
over three buckets and one over three thousand are the same number and not the same
finding.

`strength` is separate from `confidence` and ordinal, and the distinction is
deliberate rather than pedantic. `confidence` is a ratio with a definition;
`strength` is the detector's own certainty, which D23 says must not be dressed as a
number. It never reads `certain` — that level is reserved for ontology-derived and
developer-confirmed values, and a detected co-occurrence is neither until somebody
confirms it.

## Both directions, and why an idle antecedent is not examined

Each pair yields up to four directed rules, because "the oven runs, so the lights
are on" and "the lights are on, so the oven runs" are different claims and only one
of them may be a finding. In the fixture above the reverse holds at confidence 1.0
with no exceptions: lights on with the oven off is an ordinary evening, lights on
*only* while the oven runs is a pattern.

An **idle** antecedent is deliberately not examined at all. "While the oven is idle
the lights are idle" is true most of the night in any kitchen, it holds at high
confidence for reasons that have nothing to do with the pair, and lift alone is not
enough of a filter to keep a rule list a human will read to the end.

## Confirming a rule

Every candidate ends beside a confirm, a correct and a reject, and the decision goes
into an append-only log:

```json
{"action":"confirm","created_by":"…","rule_id":"rule-0c29b1557e8c1ecf9405a12f",
 "computed":{"statement":"the oven active → the kitchen lights active","confidence":0.8571,
             "exceptions":[{"bucket":"06:00-12:00","confidence":0,"drop":0.8571}]},
 "note":"matches how the kitchen is used"}
```

`rule_id` is a fingerprint of what the rule **says** — the two series, their states
and the direction — and deliberately not of the window, the grid or the detector
version. Relate the same pair next month over a longer window with a sharper
detector and the relation id changes while the rule id does not, so the decision is
still attached to it. That is the same reasoning that keys a `ProfileOverride` by
series rather than by profile (D21), and the acceptance test for it recomputes over a
different window and asserts the verdict comes back.

Correcting rather than confirming records **both** forms, which is the point: "the
detector said 0.86 and the developer narrowed it to evenings" is a finding, and a
mutable document would destroy it (§5.4.3).

### Two routes to the same decision

The pane offers the choice twice, and the difference is worth knowing before
changing either.

The **rule cards** are the reference: every candidate on screen at once, with its
evidence, its exceptions and its three buttons. That is the right shape for
checking one rule against another, and it writes on every click.

The **review** — shadcn's
[`Questionnaire`](https://ui.shadcn.com/docs/components/base/questionnaire), shown
only when two or more rules are still undecided — walks them one at a time, with
progress, a note per decision and an explicit skip. A stack of eleven identical
three-button cards gives no sense of how many are left, no way back to the one just
answered, and no way to record "not yet" as distinct from silence.

The review defers every write to its Submit, which is the one place the two routes
differ in behaviour, and it is deliberate. A developer working through eleven rules
should be able to change their mind about the third before anything is recorded —
and because the log is append-only, a decision written early cannot be withdrawn,
only added to. Skipping writes nothing at all.

**No LLM tool writes one of these.** `decide_relation_rule` is in the denied set
beside `write_profile_override`, so `NewRegistry` refuses to register it and the
surface cannot gain one by accident — a model that could confirm the rules it
proposed would be grading its own work.

## What a missing rule means

The pane keeps a member on screen even when it yielded no state series, with the
reason, because that is the first thing to check when an expected rule is absent:

- `wrong_kind` on a **continuous** series — active more than half the time, so it is
  one population with variation and an idle state would be a property of the
  threshold rather than of the device.
- `wrong_kind` on a **status or categorical** series — it has states, not sessions,
  and inventing an ordering over category labels would be the wrong answer
  confidently.
- `insufficient_coverage` — fewer than twenty observed buckets, so there is nothing
  to conclude from.
- `read_failed` — the service could not be profiled at all. One service failing does
  not end the pass: the oven-and-lights finding does not depend on the third device
  having usable data.

An empty rule list with two usable members is a result. An empty rule list with one
usable member is a different thing entirely, and reading the second as the first is
the D24 mistake one level up from a field.

## The relation routes

| Route | Effect |
| --- | --- |
| `GET /relations/candidate-sets?aspect_id=&include_descendants=&limit=&max_members=` | Sets proposed from an aspect, its device groups and its graphs. Reads no values |
| `POST /relations` | Run a pass. Body: `members[]`, `window`, `params`, `conditioning`, `candidate_set_id` |
| `GET /relations/{id}` | The stored document, with the decision log as it stands now |
| `POST /relations/{id}/rule-decisions` | Confirm, correct or reject one rule. Body: `rule_id`, `action`, `confirmed`, `note` |
| `GET /relations/rule-decisions?rule_id=` | Every verdict recorded against a fingerprint, oldest first |

A pass is also `relate` on the WebSocket, and that is the route the pane uses. It
profiles every participating service before it aligns them, which makes it the
longest read ODE makes, and the socket both reports its phases and cancels it —
closing the tab stops paying for it.

Relation profiles live in memory, bounded; the decision log goes to Postgres when
there is one. The same split the profiler makes and for the same reason: a relation
profile is a reproducible artifact whose loss costs a recomputation, and a
developer's judgement on a rule is an empirical record whose loss destroys evidence
that cannot be regenerated.