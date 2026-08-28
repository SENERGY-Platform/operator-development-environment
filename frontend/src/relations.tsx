/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  api,
  type AspectTreeNode,
  type CandidateRule,
  type CandidateSet,
  type CandidateSetMember,
  type Contingency,
  type GraphPlacement,
  type PairRelation,
  type RelationMember,
  type RelationProfile,
  type RelationProposal,
  type RelationRequest,
  type RuleDecisionRequest,
  type RuleException,
  type SeriesRef,
} from "./api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import {
  Questionnaire,
  QuestionnaireActions,
  QuestionnaireChoice,
  QuestionnaireChoiceDescription,
  QuestionnaireChoices,
  QuestionnaireDescription,
  QuestionnaireItem,
  QuestionnaireNext,
  QuestionnairePrevious,
  QuestionnaireProgress,
  QuestionnaireSkip,
  QuestionnaireSubmit,
  QuestionnaireTitle,
} from "@/components/ui/questionnaire";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cn } from "@/lib/utils";
import { setParam, useLocation, useParam } from "./router";
import { profilerSocket, type OperationPhase } from "./ws";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import {
  ConfidenceTag,
  KV,
  Muted,
  NotComputedTag,
  Pane,
  Row,
  Section,
  dateTime,
  describe,
  num,
  percent,
  round,
  seconds,
  shortId,
  useAction,
  useLoad,
} from "./ui";

/**
 * The relational surface: multi-device conditional patterns (§5.5, §5.10).
 *
 * The pane follows the order the design argues for. A developer picks an aspect —
 * a room, a subsystem — rather than devices, because the aspect hierarchy is what
 * solves candidate selection; the sets that come back are proposals with the reason
 * for each; the pass over one of them is on the socket, because it profiles every
 * participating service before it reads; and every candidate rule ends beside a
 * confirm, correct and reject, because a rule is a candidate until a developer says
 * otherwise (D28).
 *
 * Two things are deliberately visible rather than tidied away. Each member says how
 * its idle/active split was decided and whether the threshold was the detector's or
 * a developer's, because a rule computed against a corrected threshold is a
 * different claim. And a member that yielded no state series stays on screen with
 * the reason, because that is the first thing to check when an expected rule is
 * missing — an absent rule read as "no such pattern" is the D24 mistake one level up.
 */
export function RelationsView() {
  const { params } = useLocation();
  /*
   * The URL restores two different kinds of thing here, and it restores them two
   * different ways.
   *
   * The aspect and its descendants switch are *inputs*. Proposing sets from them
   * is an ontology walk over every device under the node, so it stays behind the
   * button: reloading puts the choice back into the form, not the proposal back on
   * screen. Read once at mount; after that the form is the developer's and the URL
   * follows it on submit.
   *
   * A relation is a *result*, and results here are stored. A pass profiles every
   * participating service and then issues one aligned read — minutes, and the most
   * expensive thing ODE asks of the platform — so re-running it on a reload would
   * be indefensible. It is fetched back by id instead.
   */
  const [aspect, setAspect] = useState<string>(() => params.get("aspect") ?? "");
  const [includeDescendants, setIncludeDescendants] = useState(
    () => params.get("descendants") !== "0",
  );
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [setId, setSetId] = useState<string | null>(null);
  const [phase, setPhase] = useState<OperationPhase | null>(null);
  const [restored, setRestored] = useState<RelationProfile | null>(null);
  const [restoreError, setRestoreError] = useState<string | null>(null);

  const loadTree = useCallback(() => api.aspectTree().then((r) => r.tree), []);
  const tree = useLoad(loadTree);

  const propose = useAction((_signal: AbortSignal, request: { aspectId: string; includeDescendants: boolean }) =>
    api.candidateSets(request),
  );

  // Over the socket, unlike the proposal above. A pass profiles every participating
  // service and then issues the aligned read, which is the longest thing ODE asks of
  // the platform — so changing your mind has to stop those reads rather than leave
  // them running for nobody (§5.5).
  const relate = useAction((signal: AbortSignal, request: RelationRequest) =>
    profilerSocket.request<RelationProfile>("relate", request, signal, setPhase),
  );

  const proposal = propose.data;
  const computed = relate.data;
  const relationId = useParam("relation");

  useEffect(() => {
    // Nothing to fetch when the parameter names the pass that has just finished
    // here — that would be a second read of a document already in hand.
    if (!relationId || computed?.relation_id === relationId) {
      setRestored(null);
      setRestoreError(null);
      return;
    }
    let cancelled = false;
    setRestoreError(null);
    api
      .relation(relationId)
      .then((document) => {
        if (!cancelled) setRestored(document);
      })
      .catch((e: unknown) => {
        if (!cancelled) setRestoreError(describe(e));
      });
    return () => {
      cancelled = true;
    };
  }, [relationId, computed]);

  const relation = computed ?? restored;

  const chosen = useMemo(() => {
    if (!proposal) return [];
    const members: CandidateSetMember[] = [];
    for (const set of proposal.sets) {
      for (const member of set.members) {
        if (selected[refKey(member.ref)] && !members.some((m) => refKey(m.ref) === refKey(member.ref))) {
          members.push(member);
        }
      }
    }
    return members;
  }, [proposal, selected]);

  const submitAspect = useCallback(
    (event: React.FormEvent) => {
      event.preventDefault();
      if (!aspect) return;
      setSelected({});
      setSetId(null);
      relate.reset();
      // A new proposal supersedes whatever relation was on screen, so the
      // parameter naming it goes with it — the alternative is an address that
      // still points at a document the pane no longer shows.
      setParam("relation", null);
      setParam("aspect", aspect);
      setParam("descendants", includeDescendants ? null : "0");
      void propose.invoke({ aspectId: aspect, includeDescendants });
    },
    [aspect, includeDescendants, propose, relate],
  );

  const takeSet = useCallback((set: CandidateSet) => {
    const next: Record<string, boolean> = {};
    for (const member of set.members) next[refKey(member.ref)] = true;
    setSelected(next);
    setSetId(set.set_id);
  }, []);

  const toggle = useCallback((ref: SeriesRef) => {
    setSelected((current) => ({ ...current, [refKey(ref)]: !current[refKey(ref)] }));
    // A hand-edited set is no longer the proposal it started from, and a confirmed
    // rule that claimed otherwise would be traceable to the wrong origin.
    setSetId(null);
  }, []);

  const run = useCallback(() => {
    if (chosen.length < 2) return;
    setPhase(null);
    void relate
      .invoke({
        members: chosen.map((member) => ({
          device_id: member.ref.device_id,
          service_id: member.ref.service_id,
          variable_path: member.ref.variable_path,
          label: member.label,
        })),
        candidate_set_id: setId ?? undefined,
      })
      .then((document) => {
        // Recorded only once it exists. A pass that was cancelled or failed has no
        // document to point at, and a parameter naming one would 404 on reload.
        if (document) setParam("relation", document.relation_id);
      });
  }, [chosen, relate, setId]);

  return (
    <main className="panes relations">
      <Pane
        title="Aspect"
        subtitle="Device sets proposed from the hierarchy — ontology only, no values read, tier L0"
      >
        <form className="filters flex flex-wrap items-center gap-2" onSubmit={submitAspect}>
          <Select value={aspect} onValueChange={(value) => setAspect(value ?? "")}>
            <SelectTrigger size="sm" aria-label="Aspect" className="w-auto min-w-52">
              <SelectValue placeholder="Choose an aspect…" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="">Choose an aspect…</SelectItem>
              {tree.data?.flatMap((node) => aspectOptions(node, 0))}
            </SelectContent>
          </Select>
          <label
            className="filter-check flex items-center gap-2 text-sm"
            title="Keeps series declared against nodes below the one chosen — an oven on “Kitchen” and lights on “Kitchen Ceiling” are still proposed together"
          >
            <Checkbox
              checked={includeDescendants}
              onCheckedChange={(checked) => setIncludeDescendants(checked)}
            />
            <span>include descendants</span>
          </label>
          <Button variant="outline"
            className={propose.pending ? "busy animate-pulse" : undefined}
            type="submit"
            disabled={propose.pending || aspect === ""}
          >
            {propose.pending ? "Proposing…" : "Propose sets"}
          </Button>
        </form>

        {tree.error && <Muted>{tree.error}</Muted>}
        {propose.error && <Muted>{propose.error}</Muted>}
        {!proposal && !propose.pending && (
          <Muted>
            Pick an aspect. The devices under it are what a conditional pattern is looked for
            across, which is why nothing here asks you to name a device.
          </Muted>
        )}

        {proposal && (
          <>
            <ProposalSummary proposal={proposal} />
            {proposal.sets.length === 0 && (
              <Muted>
                No set could be proposed. The notes above say why — an empty list is not evidence
                that these devices are unrelated.
              </Muted>
            )}
            {proposal.sets.map((set) => (
              <CandidateSetCard
                key={set.set_id}
                set={set}
                selected={selected}
                active={setId === set.set_id}
                onTake={takeSet}
                onToggle={toggle}
              />
            ))}
          </>
        )}
      </Pane>

      <Pane
        title="Relation"
        subtitle="Idle/active from each profile, aligned on one grid by one batched read — tier L1"
        actions={
          relate.pending ? (
            <Button variant="outline" onClick={relate.abort}>Cancel</Button>
          ) : (
            <Button variant="outline" onClick={run} disabled={chosen.length < 2}>
              Relate {chosen.length > 0 ? `${chosen.length} series` : ""}
            </Button>
          )
        }
      >
        {chosen.length > 0 && (
          <ul className="chosen">
            {chosen.map((member) => (
              <li key={refKey(member.ref)}>
                <span className="chosen-label">{member.label}</span>
                <Button variant="link" className="link" onClick={() => toggle(member.ref)} aria-label="Remove">
                  ×
                </Button>
              </li>
            ))}
          </ul>
        )}
        {chosen.length === 1 && (
          <Muted>
            One series selected. A conditional pattern is a statement about a pair, so at least two
            are needed.
          </Muted>
        )}
        {chosen.length === 0 && !relation && (
          <Muted>Take a proposed set, or tick the series you want related.</Muted>
        )}

        {relate.pending && (
          <p className="phase">
            <span className="busy animate-pulse">
              {phase ? `${phase.stage}: ${phase.detail}` : "starting…"}
            </span>
            <br />
            <span className="muted text-muted-foreground">
              Every participating service is profiled first, so a wide window takes minutes.
              Cancelling stops the platform reads, not just the waiting.
            </span>
          </p>
        )}
        {relate.error && <Muted>{relate.error}</Muted>}
        {restoreError && (
          <Muted>
            The relation named in the address could not be read: {restoreError}. The store is
            bounded, so an old relation may have been evicted; the pass can be run again from the
            set on the left.
          </Muted>
        )}
        {relation && <RelationDocument relation={relation} />}
      </Pane>
    </main>
  );
}

function ProposalSummary({ proposal }: { proposal: RelationProposal }) {
  return (
    <Section
      title={`${proposal.aspect_name || proposal.aspect_id} — ${proposal.sets.length} set(s)`}
      note={`${proposal.candidate_devices.length} readable device(s), ${proposal.reads.values} values read`}
      defaultOpen={proposal.sets.length === 0}
    >
      <KV>
        <Row label="Subtree" hint="The aspect nodes that were considered, root first">
          {proposal.subtree.map((node) => node.name || node.id).join(" › ")}
        </Row>
        <Row
          label="Values read"
          hint="Structurally zero: proposing a set is ontology, a device list and a device-group list (tier L0)"
        >
          {proposal.reads.values}
        </Row>
      </KV>
      {proposal.ontology_gaps.length > 0 && (
        <ul className="notes list-disc pl-4 text-xs text-muted-foreground">
          {proposal.ontology_gaps.map((gap, index) => (
            <li key={index}>
              <strong>{gap.device_type_name || gap.device_type_id}</strong>: {gap.consequence}
            </li>
          ))}
        </ul>
      )}
      {proposal.notes.length > 0 && (
        <ul className="notes list-disc pl-4 text-xs text-muted-foreground">
          {proposal.notes.map((note, index) => (
            <li key={index}>{note}</li>
          ))}
        </ul>
      )}
    </Section>
  );
}

/**
 * The origin is a claim about how much a grouping is worth trusting, so it is shown
 * rather than left to the rationale alone. A graph carries direction and share as
 * well as membership; a device group asserts membership; an aspect only a label.
 */
const ORIGIN_LABEL: Record<string, string> = {
  graph_siblings: "metered together",
  graph_flow: "sub-metering",
  device_group: "existing device group",
  aspect_node: "aspect node",
  aspect_subtree: "aspect subtree",
};

const ROLE_LABEL: Record<string, string> = {
  sibling: "peer",
  upstream: "feeds",
  downstream: "measures",
};

function CandidateSetCard({
  set,
  selected,
  active,
  onTake,
  onToggle,
}: {
  set: CandidateSet;
  selected: Record<string, boolean>;
  active: boolean;
  onTake: (set: CandidateSet) => void;
  onToggle: (ref: SeriesRef) => void;
}) {
  return (
    <Card className={cn("set-card mb-3 gap-3 py-4", active && "active border-primary/50")}>
      <CardHeader className="grid-cols-[1fr_auto] gap-3 px-4">
        <div className="min-w-0">
          <h3 className="text-sm font-semibold">{set.name}</h3>
          <p className="set-rationale mt-0.5 text-xs text-muted-foreground">{set.rationale}</p>
        </div>
        <div className="set-meta flex items-center gap-2">
          <Badge variant="outline" className={`origin origin-${set.origin} font-normal`}>
            {ORIGIN_LABEL[set.origin] ?? set.origin}
          </Badge>
          <span className="muted text-xs text-muted-foreground">{set.devices} devices</span>
          <Button size="sm" onClick={() => onTake(set)}>
            Take set
          </Button>
        </div>
      </CardHeader>
      <CardContent className="px-4">
      {set.graph_name && (
        <p className="set-graph text-xs text-muted-foreground">
          from the graph <strong className="font-medium text-foreground">{set.graph_name}</strong>
        </p>
      )}
      <ul className="set-members mt-2 flex flex-col gap-1.5">
        {set.members.map((member) => (
          <li key={refKey(member.ref)} className="text-sm">
            <label className="flex flex-wrap items-center gap-2">
              <Checkbox
                checked={Boolean(selected[refKey(member.ref)])}
                onCheckedChange={() => onToggle(member.ref)}
              />
              <span className="member-label">{member.label}</span>
              {member.graph && (
                <Badge
                  variant="secondary"
                  className={`role role-${member.graph.role} font-normal`}
                  title={roleHint(member.graph)}
                >
                  {ROLE_LABEL[member.graph.role] ?? member.graph.role}
                  {member.graph.via_name ? ` ${member.graph.via_name}` : ""}
                  {/*
                    A weight below 100 is the only case where the figure says something:
                    the platform requires each node's outgoing weights to sum to 100, so a
                    device with one parent always reads 100.
                  */}
                  {member.graph.weight !== undefined &&
                    member.graph.weight > 0 &&
                    member.graph.weight < 100 &&
                    ` · ${member.graph.weight}%`}
                </Badge>
              )}
              {/*
                A member the graph reached from outside the requested aspect. Marked
                because it is not an error — a meter one level up is not in the room —
                and a developer comparing the set against the aspect they asked for
                would otherwise wonder where it came from.
              */}
              {!member.from_aspect && (
                <Badge
                  variant="outline"
                  className="outside font-normal"
                  title="Reached through the graph rather than the aspect you asked about — normal for a meter one level up"
                >
                  outside aspect
                </Badge>
              )}
            </label>
            <span className="muted ml-6 block text-xs text-muted-foreground">
              {member.device_name}
              {member.unit ? ` · ${member.unit}` : ""}
              {member.aspect_name ? ` · ${member.aspect_name}` : ""}
            </span>
          </li>
        ))}
      </ul>
      {set.notes.length > 0 && (
        <ul className="notes mt-2 list-disc pl-4 text-xs text-muted-foreground">
          {set.notes.map((note, index) => (
            <li key={index}>{note}</li>
          ))}
        </ul>
      )}
      </CardContent>
    </Card>
  );
}

function RelationDocument({ relation }: { relation: RelationProfile }) {
  // The decision log is held here rather than re-fetching the relation: a confirm
  // answers with the record it wrote, and re-reading the whole document to see one
  // field change would cost the pairwise tables again.
  const [decided, setDecided] = useState<Record<string, RuleDecisionRequest["action"]>>({});

  const usable = relation.members.filter((member) => member.state.usable).length;

  return (
    <>
      <Section
        title="What was read"
        note={`${relation.candidate_rules.length} candidate rule(s) from ${usable} of ${relation.members.length} members`}
      >
        <KV>
          <Row label="Window">
            {dateTime(relation.window.from)} → {dateTime(relation.window.to)}
          </Row>
          <Row
            label="Grid"
            hint="Taken from the coarsest member's sampling interval: a finer bucket leaves the slowest series with empty buckets, and an empty bucket is not an idle device"
          >
            {relation.group_time} ({seconds(relation.grid_seconds)}) · {num(relation.buckets)} buckets,{" "}
            {num(relation.observed)} observed by every member
          </Row>
          <Row
            label="Reads"
            hint="One batched query aligns every member, however many there are. The profile passes are counted separately because only the first figure is this package's own"
          >
            {relation.reads.aligned} aligned + {relation.reads.profiles} profile ={" "}
            {relation.reads.values}
          </Row>
          <Row label="Thresholds" hint="What a candidate rule had to clear to be proposed">
            confidence ≥ {relation.params.min_confidence} · lift ≥ {relation.params.min_lift} ·
            support ≥ {relation.params.min_support} · ≥ {relation.params.min_samples} samples
          </Row>
          <Row label="Relation" hint="The id this document is stored under">
            {shortId(relation.relation_id)} · detector {relation.detector_version} ·{" "}
            {dateTime(relation.computed_at)}
          </Row>
        </KV>
        {relation.notes.length > 0 && (
          <ul className="notes list-disc pl-4 text-xs text-muted-foreground">
            {relation.notes.map((note, index) => (
              <li key={index}>{note}</li>
            ))}
          </ul>
        )}
      </Section>

      {usable === 0 && relation.members.length > 0 && (
        <div className="no-members">
          <p>
            <strong>No member yielded a state series</strong>, so there was nothing to relate.
            Every rule below would be missing for that reason alone, not because the devices
            are unrelated.
          </p>
          <ul>
            {relation.members.map((member) => (
              <li key={refKey(member.ref)}>
                <span className="member-label">{member.label}</span>{" "}
                <span className="muted text-muted-foreground">
                  {/*
                    The observed count is the first thing to read: it separates a read that
                    came back empty from one that came back full and could not be split.
                  */}
                  {member.state.observed_buckets} of {relation.buckets} buckets carried a value
                  {member.state.reason?.reason ? ` · ${member.state.reason.reason}` : ""}
                </span>
                {member.state.reason?.detail && (
                  <div className="muted text-muted-foreground">{member.state.reason.detail}</div>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}

      <Section title="Members" note="how idle and active were decided">
        <Table className="members">
          <TableHeader>
            <TableRow>
              <TableHead>Series</TableHead>
              <TableHead>Split</TableHead>
              <TableHead>Duty cycle</TableHead>
              <TableHead>Buckets</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {relation.members.map((member) => (
              <MemberRow key={refKey(member.ref)} member={member} />
            ))}
          </TableBody>
        </Table>
      </Section>

      <Section
        title="Candidate rules"
        note="candidates only — nothing downstream reads one until you confirm it"
      >
        {relation.candidate_rules.length === 0 && (
          <Muted>
            No pattern cleared the thresholds. That is a result rather than a failure — but check
            the members above first, because a member that yielded no state series could not
            contribute to any rule.
          </Muted>
        )}
        {/*
          Two ways through the same decision, and which one is offered depends on how
          many decisions there are.

          The cards below are the reference: every rule, its evidence, its exceptions
          and its three buttons, all on screen at once. That is the right shape for
          checking one rule against another, and the wrong shape for working through
          eleven of them — a stack of eleven identical three-button cards gives no
          sense of how many are left, no way back to the one just answered, and no
          record of which have been dealt with.

          So where more than one rule is still undecided, the review comes first: one
          rule at a time, with progress, a note per decision and a skip for the ones
          the developer wants to leave. It is an alternative route through the cards,
          not a replacement — D28 says the developer decides, and part of deciding is
          being able to see the evidence, which is what the cards are for.
        */}
        <RuleReview
          relationId={relation.relation_id}
          rules={relation.candidate_rules}
          decided={decided}
          onDecided={(ruleId, action) =>
            setDecided((current) => ({ ...current, [ruleId]: action }))
          }
        />
        {relation.candidate_rules.map((rule) => (
          <RuleCard
            key={rule.rule_id}
            relationId={relation.relation_id}
            rule={rule}
            decided={decided[rule.rule_id]}
            onDecided={(action) => setDecided((current) => ({ ...current, [rule.rule_id]: action }))}
          />
        ))}
      </Section>

      {relation.pairs.length > 0 && (
        <Section title="Pairwise tables" note="the counts every ratio above rests on" defaultOpen={false}>
          {relation.pairs.map((pair, index) => (
            <PairTable key={index} pair={pair} members={relation.members} />
          ))}
        </Section>
      )}
    </>
  );
}

function MemberRow({ member }: { member: RelationMember }) {
  return (
    <TableRow className={member.state.usable ? "" : "unusable"}>
      <TableCell>
        <div className="member-label">{member.label}</div>
        <div className="muted text-muted-foreground">
          {member.kind || "kind unknown"}
          {member.unit ? ` · ${member.unit}` : ""}
        </div>
      </TableCell>
      <TableCell>
        {member.state.usable ? (
          <>
            <div>
              ≥ {round(member.state.threshold, 4)}
              {member.unit ? ` ${member.unit}` : ""}
            </div>
            <div className="muted text-muted-foreground">
              {member.state.method} ·{" "}
              <span
                className={
                  member.state.threshold_source === "confirmed" ? "source confirmed" : "source"
                }
                title={
                  member.state.threshold_source === "confirmed"
                    ? "You corrected this threshold in the profiler or on a chart, and the rules below were computed against your value rather than the detector's"
                    : "The detector's own idle/active split"
                }
              >
                {member.state.threshold_source}
              </span>{" "}
              · {member.state.classification}
            </div>
          </>
        ) : (
          // Kept on screen rather than dropped: this is the first thing to check when
          // an expected rule is missing.
          member.state.reason && <NotComputedTag status={member.state.reason} />
        )}
      </TableCell>
      <TableCell>{member.state.usable ? percent(member.state.duty_cycle) : "—"}</TableCell>
      <TableCell className="muted text-muted-foreground">
        {member.state.active_buckets} active · {member.state.idle_buckets} idle ·{" "}
        {member.state.unknown_buckets} unknown
        <div title="Aligned buckets that carried a value, whether or not a split was found">
          {member.state.observed_buckets} read
        </div>
      </TableCell>
    </TableRow>
  );
}

/**
 * The three things a developer can say about a candidate rule, in the order the
 * decision is usually reached: it holds, it nearly holds, it does not.
 */
const RULE_CHOICES = [
  {
    value: "confirm",
    label: "Confirm",
    description: "The rule holds as stated. Downstream may read it.",
  },
  {
    value: "correct",
    label: "Correct",
    description: "The pattern is real but the statement is wrong. Say how below.",
  },
  {
    value: "reject",
    label: "Reject",
    description: "Not a rule. The candidate is recorded as refused, with your reason.",
  },
] as const satisfies readonly {
  value: RuleDecisionRequest["action"];
  label: string;
  description: string;
}[];

/**
 * RuleReview walks the undecided candidate rules, one at a time.
 *
 * Why a questionnaire and not a longer list of cards: this is a sequence of
 * independent single-choice decisions with an optional note each, which is exactly
 * the shape the control is for. What it adds over the cards is the three things a
 * stack of cards cannot give — how many are left, a way back to the one just
 * answered, and a skip that is recorded as "not yet decided" rather than as silence.
 *
 * Nothing is written until Submit. That is deliberate and it is the one place this
 * differs from the cards, which write on every click: a developer working through
 * eleven rules should be able to change their mind about the third without the log
 * carrying both answers. The log is append-only (D28), so a decision written early
 * cannot be taken back — only added to.
 *
 * Absent where it would add nothing: with one rule left there is no sequence to walk,
 * and the card below already asks the question with the evidence beside it.
 */
function RuleReview({
  relationId,
  rules,
  decided,
  onDecided,
}: {
  relationId: string;
  rules: CandidateRule[];
  decided: Record<string, RuleDecisionRequest["action"]>;
  onDecided: (ruleId: string, action: RuleDecisionRequest["action"]) => void;
}) {
  const outstanding = useMemo(
    () => rules.filter((rule) => decided[rule.rule_id] === undefined && !rule.decision),
    [decided, rules],
  );
  const [open, setOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  // Kept out of the questionnaire's own state because the corrected statement is a
  // second field on one branch of one answer, not an answer in its own right.
  const [corrections, setCorrections] = useState<Record<string, string>>({});

  const items = useMemo(
    () =>
      outstanding.map((rule) => ({
        name: rule.rule_id,
        // Every rule is skippable: leaving one undecided is a legitimate outcome, and
        // forcing an answer is how a developer ends up confirming something to get
        // past it.
        required: false,
        choices: RULE_CHOICES.map((choice) => ({ value: choice.value })),
      })),
    [outstanding],
  );

  if (outstanding.length < 2) return null;

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    setSaving(true);
    setError(null);
    try {
      for (const rule of outstanding) {
        const action = form.get(rule.rule_id);
        // A skipped rule has no answer, and skipping is a decision to decide later.
        if (typeof action !== "string" || action === "") continue;
        const body: RuleDecisionRequest = {
          rule_id: rule.rule_id,
          action: action as RuleDecisionRequest["action"],
          note: String(form.get(`${rule.rule_id}-note`) ?? "").trim(),
        };
        if (body.action === "correct") {
          const statement = (corrections[rule.rule_id] ?? rule.statement).trim();
          // A correction with nothing corrected is the developer having picked the
          // branch and not filled it in. Confirming their words as the rule's own
          // would put a statement in the log they never wrote.
          if (statement === "") {
            setError(`“${rule.statement}” is marked as needing correction but has no new wording.`);
            setSaving(false);
            return;
          }
          body.confirmed = {
            statement,
            anomaly: rule.anomaly,
            support: rule.support,
            confidence: rule.confidence,
            lift: rule.lift,
            exceptions: rule.exceptions,
          };
        }
        await api.decideRule(relationId, body);
        onDecided(rule.rule_id, body.action);
      }
      setOpen(false);
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setSaving(false);
    }
  };

  if (!open) {
    return (
      <div className="rule-review-open mb-3 flex flex-wrap items-center gap-2">
        <Button size="sm" onClick={() => setOpen(true)}>
          Review {outstanding.length} rules
        </Button>
        <span className="text-xs text-muted-foreground">
          one at a time, with a note each — or decide them on the cards below
        </span>
      </div>
    );
  }

  return (
    <Card className="rule-review mb-4">
      <CardHeader className="grid-cols-[1fr_auto] gap-2">
        <QuestionnaireProgress className="self-center" />
        <Button variant="ghost" size="sm" onClick={() => setOpen(false)}>
          Close
        </Button>
      </CardHeader>
      <CardContent>
        <Questionnaire items={items} shortcuts="numbers" onSubmit={(event) => void submit(event)}>
          {outstanding.map((rule) => (
            <QuestionnaireItem key={rule.rule_id} name={rule.rule_id}>
              <QuestionnaireTitle>{rule.statement}</QuestionnaireTitle>
              <QuestionnaireDescription>
                Confidence {percent(rule.confidence)} · lift ×{round(rule.lift, 2)} · support{" "}
                {percent(rule.support)} · {num(rule.samples)} samples,{" "}
                {num(rule.violations)} violations.
                {rule.exceptions.length > 0
                  ? ` Does not hold in ${num(rule.exceptions.length)} examined condition(s).`
                  : " Held in every condition examined."}
              </QuestionnaireDescription>
              <QuestionnaireChoices>
                {RULE_CHOICES.map((choice) => (
                  <QuestionnaireChoice key={choice.value} value={choice.value}>
                    {choice.label}
                    <QuestionnaireChoiceDescription>
                      {choice.description}
                    </QuestionnaireChoiceDescription>
                  </QuestionnaireChoice>
                ))}
              </QuestionnaireChoices>
              {/*
                Always rendered rather than revealed by the "Correct" choice: a field
                that appears under the cursor moves the buttons the developer was
                about to press. It is only read when that branch is the answer.
              */}
              <Input
                name={`${rule.rule_id}-statement`}
                value={corrections[rule.rule_id] ?? rule.statement}
                onChange={(event) =>
                  setCorrections((current) => ({
                    ...current,
                    [rule.rule_id]: event.target.value,
                  }))
                }
                aria-label="The rule as you would state it"
                placeholder="the rule as you would state it"
              />
              {/*
                A plain input, not `QuestionnaireInput`: that one *is* the item's
                freeform answer and carries the item's own name, which is already
                taken by the three choices. The note is a second field about the
                answer rather than a second answer, so it rides along on the form.
              */}
              <Input
                name={`${rule.rule_id}-note`}
                placeholder="why (recorded with your decision)"
                aria-label="Note"
              />
            </QuestionnaireItem>
          ))}
          <QuestionnaireActions>
            <QuestionnairePrevious />
            <QuestionnaireSkip>Decide later</QuestionnaireSkip>
            <QuestionnaireNext />
            <QuestionnaireSubmit disabled={saving}>
              {saving ? "Recording…" : "Record decisions"}
            </QuestionnaireSubmit>
          </QuestionnaireActions>
        </Questionnaire>
        {error && <Muted>{error}</Muted>}
      </CardContent>
    </Card>
  );
}

function RuleCard({
  relationId,
  rule,
  decided,
  onDecided,
}: {
  relationId: string;
  rule: CandidateRule;
  decided?: RuleDecisionRequest["action"];
  onDecided: (action: RuleDecisionRequest["action"]) => void;
}) {
  const [note, setNote] = useState("");
  const [editing, setEditing] = useState(false);
  const [statement, setStatement] = useState(rule.statement);

  const decide = useAction((_signal: AbortSignal, body: RuleDecisionRequest) =>
    api.decideRule(relationId, body),
  );

  const submit = useCallback(
    async (action: RuleDecisionRequest["action"]) => {
      const body: RuleDecisionRequest = { rule_id: rule.rule_id, action, note: note.trim() };
      if (action === "correct") {
        body.confirmed = {
          statement: statement.trim(),
          anomaly: rule.anomaly,
          support: rule.support,
          confidence: rule.confidence,
          lift: rule.lift,
          exceptions: rule.exceptions,
        };
      }
      const result = await decide.invoke(body);
      if (result) {
        onDecided(action);
        setEditing(false);
      }
    },
    [decide, note, onDecided, rule, statement],
  );

  // Whatever currently stands: the record the backend re-injected, or the one this
  // pane just wrote.
  const standing = decided ?? rule.decision?.action;

  return (
    <div className={`rule-card${standing ? ` decided-${standing}` : ""}`}>
      <div className="rule-head">
        <p className="rule-statement">{rule.statement}</p>
        <ConfidenceTag confidence={rule.strength} />
      </div>

      <p className="rule-anomaly" title="The violation this rule defines, if you confirm it">
        Anomaly: {rule.anomaly}
      </p>

      <dl className="rule-stats">
        <div title="P(consequent | antecedent): how reliably the pattern holds where the antecedent is true">
          <dt>confidence</dt>
          <dd>{percent(rule.confidence)}</dd>
        </div>
        <div title="How much more often the pair co-occurs than independent base rates predict. A high confidence at a lift near 1 means the consequent is simply usually true">
          <dt>lift</dt>
          <dd>×{round(rule.lift, 2)}</dd>
        </div>
        <div title="The share of observed buckets this pattern covers">
          <dt>support</dt>
          <dd>{percent(rule.support)}</dd>
        </div>
        <div title="Buckets in which the antecedent held, and how many of those the consequent failed in">
          <dt>evidence</dt>
          <dd>
            {num(rule.samples)} samples · {num(rule.violations)} violations
          </dd>
        </div>
      </dl>

      {rule.exceptions.length > 0 ? (
        <div className="exceptions">
          <span className="exceptions-title" title="Conditions in which this rule demonstrably does not hold">
            Except
          </span>
          <ul>
            {rule.exceptions.map((exception, index) => (
              <ExceptionItem key={index} exception={exception} />
            ))}
          </ul>
        </div>
      ) : (
        <p className="muted text-muted-foreground">
          Held in every condition examined — hour of day and weekday/weekend. That is a finding
          rather than a missing field.
        </p>
      )}

      {rule.decision && (
        <p className="decision-record">
          {rule.decision.action} by {rule.decision.created_by} on {dateTime(rule.decision.created_at)}
          {rule.decision.note ? ` — “${rule.decision.note}”` : ""}
          {rule.decision.confirmed && (
            <>
              <br />
              <span className="muted text-muted-foreground">their form: {rule.decision.confirmed.statement}</span>
            </>
          )}
        </p>
      )}

      <p className="advisory text-xs text-muted-foreground" title="Advisory: a candidate rule binds nothing until you confirm it">
        {rule.advisory}
      </p>

      {editing && (
        <Input
          className="rule-edit"
          value={statement}
          onChange={(e) => setStatement(e.target.value)}
          aria-label="Corrected rule"
          placeholder="the rule as you would state it"
        />
      )}
      <div className="rule-actions">
        <Input
          value={note}
          onChange={(e) => setNote(e.target.value)}
          placeholder="why (recorded with your decision)"
          aria-label="Note"
        />
        <Button variant="outline" onClick={() => void submit("confirm")} disabled={decide.pending}>
          Confirm
        </Button>
        {editing ? (
          <Button variant="outline"
            onClick={() => void submit("correct")}
            disabled={decide.pending || statement.trim() === ""}
          >
            Save correction
          </Button>
        ) : (
          <Button variant="outline" onClick={() => setEditing(true)} disabled={decide.pending}>
            Correct…
          </Button>
        )}
        <Button variant="outline" onClick={() => void submit("reject")} disabled={decide.pending}>
          Reject
        </Button>
      </div>
      {decide.error && <Muted>{decide.error}</Muted>}
      {decided && (
        <p className="muted text-muted-foreground">
          Recorded as {decided}. The log is append-only, so changing your mind adds a record rather
          than replacing this one.
        </p>
      )}
    </div>
  );
}

function ExceptionItem({ exception }: { exception: RuleException }) {
  const where =
    exception.dimension === "hour_of_day"
      ? `${exception.bucket} UTC`
      : `${exception.bucket}s`;
  return (
    <li>
      <strong>{where}</strong>: confidence {percent(exception.confidence)} — {percent(exception.drop)}{" "}
      below the rule, over {num(exception.samples)} samples
    </li>
  );
}

function PairTable({ pair, members }: { pair: PairRelation; members: RelationMember[] }) {
  const a = members[pair.a]?.label ?? `member ${pair.a}`;
  const b = members[pair.b]?.label ?? `member ${pair.b}`;
  return (
    <div className="pair">
      <h4>
        {a} × {b}
      </h4>
      <ContingencyTable a={a} b={b} table={pair.overall} />
      {pair.conditions.length > 0 && (
        <Table className="conditions">
          <TableHeader>
            <TableRow>
              <TableHead>Condition</TableHead>
              <TableHead>both active</TableHead>
              <TableHead>a only</TableHead>
              <TableHead>b only</TableHead>
              <TableHead>neither</TableHead>
              <TableHead>observed</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pair.conditions.map((condition, index) => (
              <TableRow key={index}>
                <TableCell>{condition.bucket}</TableCell>
                <TableCell>{condition.contingency.active_active}</TableCell>
                <TableCell>{condition.contingency.active_idle}</TableCell>
                <TableCell>{condition.contingency.idle_active}</TableCell>
                <TableCell>{condition.contingency.idle_idle}</TableCell>
                <TableCell className="muted text-muted-foreground">{condition.contingency.observed}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}

function ContingencyTable({ a, b, table }: { a: string; b: string; table: Contingency }) {
  return (
    <Table className="contingency">
      <TableHeader>
        <TableRow>
          <TableHead />
          <TableHead>{b} active</TableHead>
          <TableHead>{b} idle</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        <TableRow>
          <TableHead>{a} active</TableHead>
          <TableCell>{num(table.active_active)}</TableCell>
          <TableCell>{num(table.active_idle)}</TableCell>
        </TableRow>
        <TableRow>
          <TableHead>{a} idle</TableHead>
          <TableCell>{num(table.idle_active)}</TableCell>
          <TableCell>{num(table.idle_idle)}</TableCell>
        </TableRow>
      </TableBody>
    </Table>
  );
}

/** aspectOptions flattens the tree into indented options, so the picker shows depth. */
function aspectOptions(node: AspectTreeNode, depth: number): React.ReactNode[] {
  const label = `${"  ".repeat(depth)}${node.name || node.id}`;
  return [
    <option key={node.id} value={node.id}>
      {label}
    </option>,
    ...(node.children ?? []).flatMap((child) => aspectOptions(child, depth + 1)),
  ];
}

function refKey(ref: SeriesRef): string {
  return `${ref.device_id}|${ref.service_id}|${ref.variable_path}`;
}

/**
 * roleHint spells out what a placement means, because "measures" beside a device is
 * ambiguous on its own and the containment case is the one worth being explicit
 * about: a sub-meter running whenever its parent runs is arithmetic, not a finding.
 */
function roleHint(graph: GraphPlacement): string {
  const via = graph.via_name || "the node it connects to";
  switch (graph.role) {
    case "downstream":
      return `This device measures ${via}, so the others in this set are sub-meters of it. A rule saying they run together is arithmetic; the finding is one of them drawing while this reads idle.`;
    case "upstream":
      return `This device feeds ${via}, which another member of this set measures.`;
    default:
      return `This device feeds ${via}, and so do the other members — they are metered together.`;
  }
}
