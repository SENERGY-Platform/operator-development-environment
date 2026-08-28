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

import { useCallback, useEffect, useRef, useState } from "react";
import {
  ApiError,
  api,
  type EmbedProbe,
  type EmbedReport,
  type EvaluationCriterion,
  type Experiment,
  type ExperimentCredential,
  type ExperimentLaunch,
  type ExperimentLogs,
  type ExperimentStatus,
  type ExperimentSummary,
  type Interpretation,
  type MetricDelta,
  type Proposal,
  type ProposalDecisionKind,
  type Session,
} from "./api";
import { Markdown } from "./markdown";
import { Link, setParam, useParam } from "./router";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import {
  Busy,
  KV,
  Muted,
  NotComputedTag,
  Pane,
  Row,
  Section,
  bytes,
  dateTime,
  describe,
  num,
  seconds,
  shortId,
  useAction,
} from "./ui";

/**
 * Ray jobs, their MLflow runs, and what the assistant made of the ones that
 * finished (§5.12, §5.13).
 *
 * The pane is ordered by what a developer wants in the order they want it. The
 * list on the left is their own runs, newest first, each carrying the commit it was
 * built from — §5.11 item 7 is the whole reason this join is stored at all, so it
 * is a column rather than a detail. The document on the right opens with the
 * comparison against the previous run, because that is the number that answers
 * whether a change helped, and ends with the interpretation and the proposal,
 * because those are the things there is something to decide about.
 *
 * Four things here are decisions rather than mechanics:
 *
 *   - **A criterion has three states, not two.** `met` is `true`, `false`, or an
 *     object with a reason, and the third is rendered as an explicit non-result
 *     (D24). A run whose metric was never logged did not miss its target; nothing
 *     compared it, and a red cross would be the claim D24 exists to prevent.
 *   - **An empty comparison is "first run".** Not "no change" — the API's own
 *     comment draws that line and the pane has to keep it, because the two read
 *     identically as an empty table and mean opposite things.
 *   - **Nothing on this pane is binding (D28).** Accepting a proposal records
 *     agreement and launches nothing. Promoting a value into `evaluation.yaml` or
 *     the operator config is a separate act the developer performs by hand, and
 *     §5.8 denies every tool that could do it for them. So there is no "Apply".
 *   - **The dashboards are probed twice.** ODE reads the framing headers from
 *     inside the cluster; the browser is outside it and is the only thing that can
 *     find out whether the page actually renders in a frame. Both answers are used,
 *     and "unknown" from the backend is not treated as a refusal.
 */
export function ExperimentsView({ session }: { session: Session }) {
  /*
   * The list is held here rather than in `useLoad`, because it is polled.
   *
   * `useLoad` drops the previous result before the next arrives, which is right for
   * a filter change and wrong for a refresh on a timer: the list would blink out
   * every few seconds while a job runs. The poll writes over the rows in place
   * instead, and only the first read shows a loading state.
   */
  const [experiments, setExperiments] = useState<Experiment[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    try {
      const listing = await api.experiments();
      setExperiments(listing.experiments);
      setListError(null);
    } catch (e: unknown) {
      setListError(describe(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  /*
   * Polling stops when nothing is unfinished.
   *
   * The listing route refreshes a status from Ray only for the runs that have not
   * finished, so a list of finished runs costs the cluster nothing to re-read — and
   * costs it a round trip per developer per interval for an answer that cannot have
   * changed. The effect re-subscribes when that predicate flips, which is what makes
   * the last poll after a job settles also be the one that switches it off.
   */
  const pending = experiments !== null && hasUnfinished(experiments);
  useEffect(() => {
    if (!pending) return;
    const timer = window.setInterval(() => void load(), POLL_MS);
    return () => window.clearInterval(timer);
  }, [pending, load]);

  // The selected run lives in the address, so a reload comes back to it and a link
  // to a run means the same thing to a colleague. Replaced rather than pushed, like
  // every other selection in this application (see `setParam`).
  const runId = useParam("run");
  const selected = experiments?.find((run) => run.experiment_id === runId) ?? null;

  return (
    <main className="panes experiments">
      <Pane
        title="Experiments"
        subtitle="Ray jobs and their MLflow runs, each tagged with the commit it was built from"
        actions={
          <Button variant="outline" onClick={() => void load()} disabled={loading}>
            Refresh
          </Button>
        }
      >
        <LaunchCard session={session} onLaunched={load} />
        {listError && <p className="error text-destructive">{listError}</p>}
        {loading && !experiments && <Busy>Reading your experiments…</Busy>}
        {experiments && (
          <RunList
            experiments={experiments}
            selectedId={runId}
            onSelect={(id) => setParam("run", id)}
            onChanged={(updated) =>
              setExperiments((current) =>
                (current ?? []).map((run) =>
                  run.experiment_id === updated.experiment_id ? updated : run,
                ),
              )
            }
          />
        )}
      </Pane>

      {/*
        Keyed on the run, so moving between two runs remounts the document rather
        than leaving one run's logs, its open sections and its half-typed decision
        note attached to another's numbers. The same reason the profiler keys its
        right-hand pane on the candidate and the window.
      */}
      <RunDocument key={selected?.experiment_id ?? "none"} experiment={selected} />

      <div className="exp-dashboards">
        <Dashboards />
      </div>
    </main>
  );
}

/** How often an unfinished run is re-read. Long enough not to be a poll loop. */
const POLL_MS = 5000;

/** The states a Ray job does not leave. */
const TERMINAL: ExperimentStatus[] = ["STOPPED", "SUCCEEDED", "FAILED"];

/** isFinished says whether a status is one the job will not leave. */
export function isFinished(status: ExperimentStatus): boolean {
  return TERMINAL.includes(status);
}

/**
 * hasUnfinished is the polling predicate, and it is deliberately about the list
 * rather than about time. A page of finished runs is a page whose statuses cannot
 * change, so re-reading it asks Ray nothing and tells the developer nothing.
 */
export function hasUnfinished(experiments: Experiment[]): boolean {
  return experiments.some((run) => !isFinished(run.status));
}

/** A run that is still running is the only one there is anything to stop. */
export function canStop(status: ExperimentStatus): boolean {
  return status === "RUNNING" || status === "PENDING";
}

/** The tone a status is drawn in. Ray's vocabulary is kept; only the colour is ours. */
export function statusTone(status: ExperimentStatus): "running" | "good" | "bad" | "neutral" {
  switch (status) {
    case "PENDING":
    case "RUNNING":
      return "running";
    case "SUCCEEDED":
      return "good";
    case "FAILED":
      return "bad";
    case "STOPPED":
      return "neutral";
  }
}

// --- launching ---

/**
 * The launch card.
 *
 * A run is submitted from the repository's *committed* state, so the two refusals
 * it can come back with are steps the developer takes next rather than errors:
 * an uncommitted working copy and a checkout pointing at another remote. Both are
 * rendered as the action they name, with the way to the workspace beside them,
 * because "409: the working copy has uncommitted changes" is a sentence that tells
 * a developer nothing they can press.
 */
function LaunchCard({ session, onLaunched }: { session: Session; onLaunched: () => Promise<void> }) {
  const [entrypoint, setEntrypoint] = useState("");
  const [runName, setRunName] = useState("");
  const [refusal, setRefusal] = useState<LaunchRefusal | null>(null);
  const [launched, setLaunched] = useState<ExperimentLaunch | null>(null);

  // The open conversation, which is what ties a run to the exchange it came from.
  // M9 reads it back: the interpretation of a finished run is delivered into that
  // session rather than into whichever one happens to be open when it lands.
  const sessionId = useParam("session");

  const launch = useAction(async (_signal: AbortSignal) => {
    setRefusal(null);
    setLaunched(null);
    try {
      const result = await api.launchExperiment({
        entrypoint: entrypoint.trim() || undefined,
        run_name: runName.trim() || undefined,
        session_id: sessionId ?? undefined,
      });
      setLaunched(result);
      await onLaunched();
      return result;
    } catch (e: unknown) {
      const structured = launchRefusal(e);
      if (structured) {
        // Not rethrown: this is the pane's control flow, and `useAction` would put
        // it on screen a second time as a red line of prose.
        setRefusal(structured);
        return null;
      }
      throw e;
    }
  });

  // What the *next* run will carry, which is the thing worth knowing before a long
  // one is started rather than after it dies at hour two (§3.1 item 6). The
  // deployment answers this at session read; the launch answer below confirms it
  // for the run that was actually submitted.
  const scoped = session.experiments?.scoped_job_token ?? false;

  return (
    <div className="exp-launch">
      <form
        className="filters flex flex-wrap items-center gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          void launch.invoke();
        }}
      >
        <Input
          value={entrypoint}
          onChange={(event) => setEntrypoint(event.target.value)}
          placeholder="Entrypoint (blank: the repository's own)"
          aria-label="Entrypoint"
        />
        <Input
          value={runName}
          onChange={(event) => setRunName(event.target.value)}
          placeholder="Run name (optional)"
          aria-label="Run name"
        />
        <Button variant="default"
          className={launch.pending ? "primary busy animate-pulse" : "primary"}
          type="submit"
          disabled={launch.pending}
        >
          {launch.pending ? "Submitting…" : "Launch"}
        </Button>
      </form>

      <p className="muted text-muted-foreground">
        The job is built from the repository's current <strong>commit</strong>, not from the
        working copy: a run's commit SHA is only worth recording if it is what ran.
        {sessionId ? " This run will be tied to the open conversation." : ""}
      </p>

      {!scoped && (
        <p className="notice notice-warn exp-credential-warning">
          This deployment mints no scoped job token, so a job carries your interactive session
          token. A run that outlives your session loses its access to the platform
          partway through — worth knowing before a long one, not after a 401 in a Ray log.
        </p>
      )}

      {launch.error && <p className="error text-destructive">{launch.error}</p>}
      {refusal && <Refusal refusal={refusal} />}
      {launched && <Launched launch={launched} />}
    </div>
  );
}

/** What the backend refused a launch with, in the shape the card renders. */
export interface LaunchRefusal {
  needs: string;
  message: string;
  hint?: string;
  /** The uncommitted paths, for `needs: "commit"`. */
  paths?: string[];
  /** True when more paths were uncommitted than the backend listed. */
  elided?: boolean;
  /** The package size and the cap, for `needs: "smaller_package"`. */
  size?: { bytes: number; limit: number };
}

/**
 * launchRefusal recognises the 409s that are a next step rather than a fault.
 *
 * The launch reaches the repo and kernel surfaces on its way to the cluster, so it
 * can answer with any of their `needs` as well as its own two. Every one of them
 * names something the developer does, which is why they are pulled out of the error
 * here instead of being printed. Anything else — a 502 from Ray, a 500 — is left to
 * `describe`, because there is nothing to press.
 */
export function launchRefusal(e: unknown): LaunchRefusal | null {
  if (!(e instanceof ApiError) || e.status !== 409) return null;
  const needs = e.needs;
  if (!needs) return null;

  const body = e.body ?? {};
  const paths = Array.isArray(body.uncommitted)
    ? body.uncommitted.filter((path): path is string => typeof path === "string")
    : undefined;
  const size =
    typeof body.bytes === "number" && typeof body.limit === "number"
      ? { bytes: body.bytes, limit: body.limit }
      : undefined;

  return {
    needs,
    message: e.message,
    hint: typeof body.hint === "string" ? body.hint : undefined,
    paths,
    elided: body.uncommitted_elided === true,
    size,
  };
}

/** The headline for each refusal: what the developer does, in their own terms. */
const REFUSAL_TITLE: Record<string, string> = {
  commit: "Commit your work first",
  smaller_package: "The package is larger than this deployment allows",
  remote_match: "The checkout points at a different repository",
  clone: "The repository is not checked out yet",
  repository: "No repository is selected",
  github_connection: "GitHub is not connected",
  idle_kernel: "The kernel is busy",
};

function Refusal({ refusal }: { refusal: LaunchRefusal }) {
  return (
    <div className="notice notice-warn exp-refusal">
      <p className="exp-refusal-title">{REFUSAL_TITLE[refusal.needs] ?? "The launch was refused"}</p>
      <p>{refusal.message}</p>
      {refusal.paths && refusal.paths.length > 0 && (
        <ul className="list tight exp-uncommitted flex flex-col gap-1 leading-tight">
          {refusal.paths.map((path) => (
            <li key={path}>
              <code>{path}</code>
            </li>
          ))}
          {refusal.elided && <li className="muted text-muted-foreground">…and more</li>}
        </ul>
      )}
      {refusal.size && (
        <p>
          {bytes(refusal.size.bytes)} against a limit of {bytes(refusal.size.limit)}.
        </p>
      )}
      {refusal.hint && <p className="muted text-muted-foreground">{refusal.hint}</p>}
      {/*
        Every one of these refusals is answered in the workspace — the commit box,
        the repository picker and the connect card all live there — so the way out
        is a link to it rather than a second copy of those controls here.
      */}
      <p>
        <Link to="/">Open the workspace</Link>
      </p>
    </div>
  );
}

/** The launch answer: what was submitted, and what it will authenticate with. */
function Launched({ launch }: { launch: ExperimentLaunch }) {
  return (
    <div className="notice notice-info exp-launched">
      <p>
        Submitted as <code>{shortId(launch.experiment_id)}</code> from commit{" "}
        <code>{launch.commit_sha.slice(0, 7)}</code>
        {launch.branch ? ` on ${launch.branch}` : ""}.
      </p>
      <CredentialLine credential={launch.credential} />
      {launch.warnings && launch.warnings.length > 0 && (
        <ul className="list tight flex flex-col gap-1 leading-tight">
          {launch.warnings.map((warning, index) => (
            <li key={index} className="warn text-foreground">
              {warning}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * The credential the job carries, and how long for.
 *
 * `expires_with_session` is the sentence that decides whether a long run is viable,
 * so it is stated in the launch answer rather than left to a Ray log to reveal at
 * hour two. The backend writes the note so that every surface says the same thing.
 */
function CredentialLine({ credential }: { credential: ExperimentCredential }) {
  return (
    <p className={credential.expires_with_session ? "warn text-foreground" : "muted text-muted-foreground"}>
      {credential.source === "exchanged"
        ? "The job carries a token minted for it"
        : "The job carries your interactive session token"}
      {credential.expires_in_seconds !== undefined
        ? `, good for ${seconds(credential.expires_in_seconds)}`
        : ""}
      .{credential.note ? ` ${credential.note}` : ""}
    </p>
  );
}

// --- the list ---

function RunList({
  experiments,
  selectedId,
  onSelect,
  onChanged,
}: {
  experiments: Experiment[];
  selectedId: string | null;
  onSelect: (id: string) => void;
  onChanged: (experiment: Experiment) => void;
}) {
  if (experiments.length === 0) {
    return <Muted>No experiments yet. A launch appears here as soon as Ray accepts it.</Muted>;
  }

  return (
    <Table className="grid exp-list">
      <TableHeader>
        <TableRow>
          <TableHead>Status</TableHead>
          <TableHead>Commit</TableHead>
          <TableHead>Repository</TableHead>
          <TableHead>Entrypoint</TableHead>
          <TableHead>Submitted</TableHead>
          <TableHead />
        </TableRow>
      </TableHeader>
      <TableBody>
        {experiments.map((run) => (
          <RunRow
            key={run.experiment_id}
            run={run}
            selected={run.experiment_id === selectedId}
            onSelect={() => onSelect(run.experiment_id)}
            onChanged={onChanged}
          />
        ))}
      </TableBody>
    </Table>
  );
}

function RunRow({
  run,
  selected,
  onSelect,
  onChanged,
}: {
  run: Experiment;
  selected: boolean;
  onSelect: () => void;
  onChanged: (experiment: Experiment) => void;
}) {
  const stop = useAction(async (_signal: AbortSignal) => {
    const stopped = await api.stopExperiment(run.experiment_id);
    onChanged(stopped);
    return stopped;
  });

  return (
    <TableRow className={selected ? "exp-row selected" : "exp-row"}>
      <TableCell>
        <Button variant="ghost" size="sm" className="exp-open" onClick={onSelect}>
          <span className={`exp-status ${statusTone(run.status)}`}>{run.status}</span>
        </Button>
      </TableCell>
      {/*
        The commit, in the list rather than only in the document. §5.11 item 7 is
        about a run being reproducible from a code state, and two runs of the same
        entrypoint from two different commits are the case that makes the point.
      */}
      <TableCell>
        <code title={run.commit_sha}>{run.commit_sha.slice(0, 7)}</code>
        {run.branch ? <span className="muted-inline text-xs text-muted-foreground"> · {run.branch}</span> : null}
      </TableCell>
      <TableCell className="wrap">{run.repository}</TableCell>
      <TableCell className="wrap">
        <code>{run.entrypoint}</code>
      </TableCell>
      <TableCell>{dateTime(run.submitted_at)}</TableCell>
      <TableCell>
        {canStop(run.status) && (
          <Button variant="outline"
            className={stop.pending ? "busy animate-pulse" : undefined}
            onClick={() => void stop.invoke()}
            disabled={stop.pending}
          >
            {stop.pending ? "Stopping…" : "Stop"}
          </Button>
        )}
        {stop.error && <span className="error text-destructive">{stop.error}</span>}
      </TableCell>
    </TableRow>
  );
}

// --- the run document ---

function RunDocument({ experiment }: { experiment: Experiment | null }) {
  if (!experiment) {
    return (
      <Pane title="Run" subtitle="Pick an experiment on the left">
        <Muted>
          A run's document is its summary — params, metrics and the comparison against the
          previous run — with the assistant's reading of it and the adjustment it proposed.
        </Muted>
      </Pane>
    );
  }

  return (
    <Pane
      title="Run"
      subtitle={`${experiment.repository} at ${experiment.commit_sha.slice(0, 7)} — ${experiment.status}`}
    >
      {experiment.message && (
        <p className="error exp-ray-message text-destructive" title="Ray's own message for this job. Never a log.">
          {experiment.message}
        </p>
      )}
      <Results experiment={experiment} />
      <InterpretationSection experiment={experiment} />
      <Submission experiment={experiment} />
      <LogsSection experiment={experiment} />
    </Pane>
  );
}

/**
 * The §5.13 summary, opening with the comparison.
 *
 * The order is the developer's rather than the JSON's: whether the change helped is
 * the question they came with, and params and metrics are what they check once they
 * have the answer.
 */
function Results({ experiment }: { experiment: Experiment }) {
  const [summary, setSummary] = useState<ExperimentSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    // A submission that failed before ODE created the run has nothing on the other
    // end of this route, and asking anyway answers 400 — which would render as a
    // red error where the honest answer is that there is no run to read. The
    // record carries the run id, so the question can be answered without asking.
    if (!experiment.mlflow_run_id) {
      setSummary(null);
      setError(null);
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    api
      .experimentResults(experiment.experiment_id)
      .then((result) => {
        if (!cancelled) setSummary(result);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(describe(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // Re-read when the status settles: a snapshot taken mid-run is not the result.
  }, [experiment.experiment_id, experiment.status]);

  if (loading && !summary) {
    return (
      <Section title="Results">
        <Busy>Reading the run from MLflow…</Busy>
      </Section>
    );
  }
  if (error) {
    return (
      <Section title="Results">
        <p className="error text-destructive">{error}</p>
      </Section>
    );
  }
  if (!summary) {
    // Said rather than left blank, for the reason NotComputedTag exists: a section
    // that silently disappears reads as "no results", and the developer draws the
    // conclusion that the run produced nothing rather than that it never started.
    return (
      <Section title="Results">
        <Muted>
          This submission has no MLflow run, so there is nothing to summarise — it failed
          before one was created. The status and the message above are what there is.
        </Muted>
      </Section>
    );
  }

  return (
    <>
      <Section
        title="Against the previous run"
        note={summary.previous_run_id ? `vs ${shortId(summary.previous_run_id)}` : undefined}
      >
        <Comparison summary={summary} />
      </Section>

      <Section title="Evaluation criteria" note={criteriaNote(summary)}>
        <CriterionCard criterion={summary.evaluation_criteria} primary />
        {summary.secondary_criteria?.map((criterion, index) => (
          <CriterionCard key={`${criterion.metric ?? "criterion"}-${index}`} criterion={criterion} />
        ))}
        <p className="advisory text-xs text-muted-foreground">
          Read out of your <code>evaluation.yaml</code> at the run's own commit, so a threshold
          tightened while the job ran does not retroactively fail it. ODE reads that file and never
          writes it.
        </p>
      </Section>

      <Section title="Metrics and params" defaultOpen={false}>
        <KV>
          <Row label="Run">
            <code>{summary.run_id}</code>
          </Row>
          <Row label="Status">
            <span className={`exp-status ${statusTone(summary.status)}`}>{summary.status}</span>
            {!summary.finished && (
              <span className="muted-inline text-xs text-muted-foreground">
                {" "}
                — a snapshot rather than a result; the run has not settled
              </span>
            )}
          </Row>
          <Row label="Duration">{seconds(summary.resource_usage.duration_s)}</Row>
          {/*
            Absent rather than zero, and shown as absent. A peak memory of zero would
            read as "used no memory", which is a finding nobody measured.
          */}
          <Row label="Peak memory">
            {summary.resource_usage.peak_memory_mb !== undefined ? (
              <>
                {num(summary.resource_usage.peak_memory_mb)} MiB
                {summary.resource_usage.peak_memory_source
                  ? ` · ${summary.resource_usage.peak_memory_source}`
                  : ""}
              </>
            ) : (
              <NotComputedTag
                status={{
                  status: "not_computed",
                  reason: "out_of_scope",
                  detail: "the job logged no memory metric, so none was recorded",
                }}
              />
            )}
          </Row>
        </KV>
        <Pairs title="Metrics" entries={Object.entries(summary.metrics).map(([k, v]) => [k, num(v)])} />
        <Pairs title="Params" entries={Object.entries(summary.params)} />
        <Pairs title="Tags" entries={Object.entries(summary.tags)} />
      </Section>
    </>
  );
}

function criteriaNote(summary: ExperimentSummary): string {
  const count = 1 + (summary.secondary_criteria?.length ?? 0);
  return count === 1 ? "1 criterion" : `${count} criteria`;
}

/**
 * The comparison, or the reason there is none.
 *
 * An empty `comparison_to_previous` is the first run of an experiment, and it is
 * said in those words. Rendering it as an empty table would leave the developer to
 * read "no change" out of it, which is the opposite of what it means and exactly
 * the misreading D24 exists to prevent one level down.
 */
function Comparison({ summary }: { summary: ExperimentSummary }) {
  if (summary.comparison_to_previous.length === 0) {
    return (
      <p className="exp-first-run">
        First run of this experiment — there is nothing to compare against yet. That is not
        “no change”: no previous run exists.
      </p>
    );
  }

  return (
    <Table className="grid exp-comparison">
      <TableHeader>
        <TableRow>
          <TableHead>Metric</TableHead>
          <TableHead className="numeric text-right tabular-nums">Previous</TableHead>
          <TableHead className="numeric text-right tabular-nums">Current</TableHead>
          <TableHead className="numeric text-right tabular-nums">Delta</TableHead>
          <TableHead>Direction</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {summary.comparison_to_previous.map((delta) => (
          <ComparisonRow key={delta.metric} delta={delta} />
        ))}
      </TableBody>
    </Table>
  );
}

function ComparisonRow({ delta }: { delta: MetricDelta }) {
  return (
    <TableRow>
      <TableCell>
        <code>{delta.metric}</code>
      </TableCell>
      <TableCell className="numeric text-right tabular-nums">{num(delta.previous)}</TableCell>
      <TableCell className="numeric text-right tabular-nums">{num(delta.current)}</TableCell>
      <TableCell className="numeric text-right tabular-nums">
        {delta.delta > 0 ? "+" : ""}
        {num(delta.delta)}
      </TableCell>
      <TableCell>
        <span className={`exp-direction ${delta.direction}`}>{delta.direction}</span>
        {/*
          The rule beside the verdict, for the reason the API type puts them beside
          each other: without the developer's criteria the backend decides "better"
          from the metric's *name*, and a verdict whose rule is invisible reads as a
          judgement rather than as a convention.
        */}
        <span className="muted-inline text-xs text-muted-foreground"> · {delta.lower_is_better ? "lower" : "higher"} is better</span>
      </TableCell>
    </TableRow>
  );
}

/**
 * One evaluation criterion, in whichever of its three states it is in.
 *
 * The three are drawn differently on purpose. `true` and `false` are the developer's
 * threshold answered; the object arm is nobody having answered it, and it carries
 * its own reason — a missing `evaluation.yaml`, a metric the run never logged, a
 * summary built before the developer was back. Rendering that third arm as a failed
 * criterion would assert a result that was never computed (D24).
 */
function CriterionCard({
  criterion,
  primary = false,
}: {
  criterion: EvaluationCriterion;
  primary?: boolean;
}) {
  const verdict = criterionVerdict(criterion);
  return (
    <div className={`exp-criterion ${verdict === "met" ? "met" : verdict === "missed" ? "missed" : "unknown"}`}>
      <div className="exp-criterion-head">
        <span className="exp-criterion-metric">
          {criterion.metric ? <code>{criterion.metric}</code> : <em>no criterion stated</em>}
          {primary && <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">primary</span>}
        </span>
        {verdict === "met" && <span className="exp-verdict met">met</span>}
        {verdict === "missed" && <span className="exp-verdict missed">not met</span>}
        {typeof verdict !== "string" && <NotComputedTag status={verdict} />}
      </div>
      <KV>
        {/*
          `!== undefined` rather than a truthiness test on both of these: a threshold
          of zero is one the developer wrote, and a metric that read exactly zero is a
          real reading. Either shown as absent would be a fabricated non-result, and
          either absent shown as zero would be a target nobody set.
        */}
        <Row label="Threshold">
          {criterion.threshold !== undefined ? (
            num(criterion.threshold)
          ) : (
            <span className="muted-inline text-xs text-muted-foreground">none stated in the file</span>
          )}
        </Row>
        <Row label="Value">
          {criterion.value !== undefined ? (
            num(criterion.value)
          ) : (
            <span className="muted-inline text-xs text-muted-foreground">the run logged none</span>
          )}
        </Row>
        <Row
          label="Goal"
          hint="Whether the file named the direction, or whether it was inferred from the metric's name"
        >
          {criterion.goal}
          <span className="muted-inline text-xs text-muted-foreground">
            {" "}
            · {criterion.goal_stated ? "stated in the file" : "inferred from the metric name"}
          </span>
        </Row>
        <Row label="Source">
          <span className="muted-inline text-xs text-muted-foreground">{criterion.source}</span>
        </Row>
      </KV>
    </div>
  );
}

/**
 * criterionVerdict collapses `met` into the three things the pane draws.
 *
 * A function rather than a ternary at the call site so that the third arm cannot be
 * accidentally folded into the second by a later edit: `!met` is true for `false`
 * and false for the object, which is the bug this shape makes unwritable.
 */
export function criterionVerdict(
  criterion: EvaluationCriterion,
): "met" | "missed" | Exclude<EvaluationCriterion["met"], boolean> {
  if (criterion.met === true) return "met";
  if (criterion.met === false) return "missed";
  return criterion.met;
}

function Pairs({ title, entries }: { title: string; entries: [string, string][] }) {
  if (entries.length === 0) return null;
  return (
    <>
      <p className="exp-pairs-title">{title}</p>
      <KV>
        {entries.map(([key, value]) => (
          <Row key={key} label={key}>
            <span className="wrap">{value}</span>
          </Row>
        ))}
      </KV>
    </>
  );
}

/** The join ODE is the only record of: a Ray submission, an MLflow run, a commit. */
function Submission({ experiment }: { experiment: Experiment }) {
  return (
    <Section title="Submission" defaultOpen={false}>
      <KV>
        <Row label="Experiment">
          <code>{experiment.experiment_id}</code>
        </Row>
        <Row label="Ray submission">
          <code>{experiment.submission_id}</code>
        </Row>
        <Row label="MLflow run">
          <code>{experiment.mlflow_run_id}</code>
        </Row>
        <Row
          label="MLflow experiment"
          hint="Deterministic from the developer and the repository, so your runs land in one experiment across sessions"
        >
          <span className="wrap">{experiment.mlflow_experiment_name}</span>
        </Row>
        <Row label="Commit">
          <code title={experiment.commit_sha}>{experiment.commit_sha}</code>
        </Row>
        <Row label="Package">
          {bytes(experiment.package_bytes)}
          {experiment.package_reused && (
            <span className="muted-inline text-xs text-muted-foreground"> · already on the cluster, not uploaded again</span>
          )}
          <br />
          <code className="wrap">{experiment.package_uri}</code>
        </Row>
        {/*
          Only the boolean here: the full `ExperimentCredential` — its lifetime and
          the backend's own sentence about it — is part of the launch answer and is
          not stored on the record, so a run selected from the list can say which of
          the two kinds of token it carried and not how long that token was good for.
        */}
        <Row label="Credential" hint="The token the job runs with">
          {experiment.scoped_credential ? (
            "a token minted for the job"
          ) : (
            <span className="warn text-foreground">
              your interactive session token — a run outliving the session loses platform access
              partway through
            </span>
          )}
        </Row>
        {experiment.session_id && (
          <Row label="Conversation">
            <Link to={`/chat?session=${encodeURIComponent(experiment.session_id)}`}>
              the session it was launched from
            </Link>
          </Row>
        )}
        <Row label="Submitted">{dateTime(experiment.submitted_at)}</Row>
        {experiment.started_at && <Row label="Started">{dateTime(experiment.started_at)}</Row>}
        {experiment.ended_at && <Row label="Ended">{dateTime(experiment.ended_at)}</Row>}
      </KV>
    </Section>
  );
}

/**
 * The driver output, on demand.
 *
 * Fetched only when asked for: a tail-capped log is the largest thing this pane can
 * pull and nobody wants it by default. It is also the developer's own view and no
 * assistant's — §5.13 keeps a training process's raw output away from the model, and
 * that is enforced by there being a route here and no tool anywhere (§5.8). Saying
 * so beside the button is what keeps it a design rather than an omission a later
 * reader might "fix".
 */
function LogsSection({ experiment }: { experiment: Experiment }) {
  const [logs, setLogs] = useState<ExperimentLogs | null>(null);
  const fetchLogs = useAction(async (_signal: AbortSignal) => {
    const result = await api.experimentLogs(experiment.experiment_id);
    setLogs(result);
    return result;
  });

  return (
    <Section title="Logs" note="your view only — never the assistant's" defaultOpen={false}>
      <p className="muted text-muted-foreground">
        ODE builds the assistant a compact structured summary and never gives it raw output.
        There is a route for this and deliberately no tool, so what follows is yours alone.
      </p>
      <Button variant="outline"
        className={fetchLogs.pending ? "busy animate-pulse" : undefined}
        onClick={() => void fetchLogs.invoke()}
        disabled={fetchLogs.pending}
      >
        {fetchLogs.pending ? "Reading…" : logs ? "Read again" : "Show logs"}
      </Button>
      {fetchLogs.error && <p className="error text-destructive">{fetchLogs.error}</p>}
      {logs && (
        <>
          {logs.truncated && (
            <p className="muted text-muted-foreground">Tail only — the driver wrote more than the cap keeps.</p>
          )}
          <pre className="exp-logs">{logs.logs || "(the job wrote nothing to its driver)"}</pre>
        </>
      )}
    </Section>
  );
}

// --- the interpretation and the proposal (M9) ---

/**
 * The assistant's reading of a run, and the adjustment it proposed.
 *
 * This is a *second* view. The interpretation is delivered into the conversation
 * that launched the run, which is where it is written and where the developer can
 * argue with it; here it is the same thing laid out as a document, with the three
 * answers §5.13 ends on beside it. The link back to that session is not decoration:
 * a proposal read without the exchange around it is a sentence with its reasoning
 * removed.
 */
function InterpretationSection({ experiment }: { experiment: Experiment }) {
  const [interpretation, setInterpretation] = useState<Interpretation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const result = await api.interpretation(experiment.experiment_id);
      setInterpretation(result);
      setError(null);
      return result;
    } catch (e: unknown) {
      setError(describe(e));
      return null;
    } finally {
      setLoading(false);
    }
  }, [experiment.experiment_id]);

  useEffect(() => {
    // Only for a run that has settled. An unfinished job has no summary to interpret
    // and the route would answer for one that does not exist yet.
    if (!isFinished(experiment.status)) return;
    void load();
  }, [experiment.status, load]);

  if (!isFinished(experiment.status)) {
    return (
      <Section title="Interpretation">
        <Muted>
          The run has not settled. The summary is built when it does, and the assistant reads
          it then.
        </Muted>
      </Section>
    );
  }

  return (
    <Section title="Interpretation and proposal" note="advisory only">
      {loading && !interpretation && <Busy>Reading the interpretation…</Busy>}
      {error && !interpretation && <p className="error text-destructive">{error}</p>}
      {interpretation && (
        <>
          <p className="muted text-muted-foreground">
            Delivered into the conversation this run belongs to — that is where the assistant
            wrote it and where you can take it further.{" "}
            <Link to={`/chat?session=${encodeURIComponent(interpretation.session_id)}`}>
              Read it in context
            </Link>
            .
          </p>

          {interpretation.interpreted_at ? (
            <Markdown className="exp-interpretation" text={interpretation.interpretation} />
          ) : (
            /*
              Not a failure. The summary is built with ODE's own Ray and MLflow
              credential when the run finishes; the turn that interprets it needs the
              developer's own token, so a run that settled while they were away waits
              rather than fails (§3.1 items 3 and 5).
            */
            <p className="muted text-muted-foreground">
              Not interpreted yet. The summary was built when the run finished, with ODE's own
              credential; the assistant's turn waits for you to be connected.
            </p>
          )}

          <ProposalCard
            experimentId={experiment.experiment_id}
            interpretation={interpretation}
            onDecided={setInterpretation}
            onReread={load}
          />
        </>
      )}
    </Section>
  );
}

/**
 * The proposal, with accept, edit and reject — the three answers §5.13 ends on.
 *
 * The same interaction `relations.tsx` gives a candidate rule, and for the same
 * reason: the assistant proposed something, and what is recorded is what the
 * developer made of it. The decision log is append-only, so changing your mind adds
 * a record rather than rewriting one, and "the assistant proposed X and the
 * developer edited it to Y" survives as the finding it is.
 */
function ProposalCard({
  experimentId,
  interpretation,
  onDecided,
  onReread,
}: {
  experimentId: string;
  interpretation: Interpretation;
  onDecided: (next: Interpretation) => void;
  onReread: () => Promise<Interpretation | null>;
}) {
  const proposal = interpretation.proposal;
  const [note, setNote] = useState("");
  const [editing, setEditing] = useState(false);
  const [edited, setEdited] = useState(proposal.text ?? "");
  const [stale, setStale] = useState(false);

  const decide = useAction(async (_signal: AbortSignal, decision: ProposalDecisionKind) => {
    if (!proposal.proposal_id) return null;
    setStale(false);
    try {
      const next = await api.decideProposal(experimentId, {
        proposal_id: proposal.proposal_id,
        decision,
        edited: decision === "edited" ? edited.trim() : undefined,
        note: note.trim() || undefined,
      });
      onDecided(next);
      setEditing(false);
      setNote("");
      return next;
    } catch (e: unknown) {
      /*
       * 409 is the backend refusing to record agreement with something the developer
       * never read: the run was interpreted again and the proposal on screen is not
       * the one that stands. Re-reading and saying so is the whole point of the
       * status — surfacing it as "409: stale proposal" would leave them pressing
       * Accept again on the same stale text.
       */
      if (isStaleProposal(e)) {
        setStale(true);
        const fresh = await onReread();
        if (fresh) setEdited(fresh.proposal.text ?? "");
        return null;
      }
      throw e;
    }
  });

  return (
    <div className="exp-proposal">
      {stale && (
        <p className="notice notice-warn">
          The run was interpreted again while this was on screen and proposed something else. The
          proposal below is the one that stands now — nothing was recorded for the old one.
        </p>
      )}

      <ProposalText proposal={proposal} />

      {interpretation.decision && <DecisionRecord decision={interpretation.decision} />}

      {proposal.proposal_id && (
        <>
          {editing && (
            <Textarea
              className="exp-edit"
              value={edited}
              onChange={(event) => setEdited(event.target.value)}
              aria-label="The adjustment, as you would state it"
              placeholder="the adjustment as you would state it"
              rows={3}
            />
          )}
          <div className="rule-actions">
            <Input
              value={note}
              onChange={(event) => setNote(event.target.value)}
              placeholder="why (recorded with your decision)"
              aria-label="Note"
            />
            <Button variant="outline" onClick={() => void decide.invoke("accepted")} disabled={decide.pending}>
              Accept
            </Button>
            {editing ? (
              <Button variant="outline"
                onClick={() => void decide.invoke("edited")}
                disabled={decide.pending || edited.trim() === ""}
              >
                Save your version
              </Button>
            ) : (
              <Button variant="outline" onClick={() => setEditing(true)} disabled={decide.pending}>
                Edit…
              </Button>
            )}
            <Button variant="outline" onClick={() => void decide.invoke("rejected")} disabled={decide.pending}>
              Reject
            </Button>
          </div>
          {decide.error && <p className="error text-destructive">{decide.error}</p>}
        </>
      )}

      {/*
        D28, rendered rather than assumed. Accepting records agreement; it launches
        nothing and writes nothing into the repository. Promoting a value into
        `evaluation.yaml` or the operator config is a separate act the developer
        performs by hand — §5.8 denies every tool that could do it — which is why
        there is no button on this card that would.
      */}
      <p className="advisory text-xs text-muted-foreground">
        Advisory only. Accepting records that you agree; it launches no run and changes no file.
        Promoting a value into <code>evaluation.yaml</code> or the operator config is yours to do,
        and there is deliberately no tool for it.
      </p>

      {interpretation.decisions.length > 1 && (
        <Section title="Decision log" note={`${interpretation.decisions.length} records`} defaultOpen={false}>
          {interpretation.decisions.map((record) => (
            <DecisionRecord key={record.decision_id} decision={record} />
          ))}
        </Section>
      )}
    </div>
  );
}

/**
 * The proposal itself, or the explicit reason there is none.
 *
 * `no_proposal_stated` is an assistant that read the run and named no next step;
 * `not_interpreted_yet` is a turn that has not run. Two different facts, and neither
 * of them is "there is nothing to change" — which is what an empty box would say.
 */
function ProposalText({ proposal }: { proposal: Proposal }) {
  if (proposal.status === "not_computed") {
    return (
      <p className="exp-proposal-text">
        <NotComputedTag
          status={{
            status: "not_computed",
            reason: proposal.reason ?? "no_proposal_stated",
            detail: proposal.detail ?? "no proposal was recorded for this run",
          }}
        />
      </p>
    );
  }
  return <p className="exp-proposal-text">{proposal.text}</p>;
}

function DecisionRecord({
  decision,
}: {
  decision: Interpretation["decisions"][number];
}) {
  return (
    <p className="decision-record">
      <span className={`exp-decision ${decision.decision}`}>{decision.decision}</span> by{" "}
      {decision.created_by} on {dateTime(decision.created_at)}
      {decision.note ? ` — “${decision.note}”` : ""}
      {decision.edited && (
        <>
          <br />
          <span className="muted-inline text-xs text-muted-foreground">their version: {decision.edited}</span>
        </>
      )}
      {/*
        Serialised by the backend rather than omitted so that a reader meets D28
        instead of having to know it, which makes it worth rendering rather than
        skipping as a constant.
      */}
      {!decision.binding && <span className="muted-inline text-xs text-muted-foreground"> · not binding</span>}
    </p>
  );
}

/** isStaleProposal recognises the one 409 `decideProposal` answers with. */
export function isStaleProposal(e: unknown): boolean {
  return e instanceof ApiError && e.status === 409 && e.needs === "reread";
}

// --- the dashboards (D6) ---

/**
 * The Ray dashboard and the MLflow UI, framed if they can be framed.
 *
 * Two probes, because neither alone answers the question. ODE reads
 * `X-Frame-Options` and `frame-ancestors` from inside the cluster, which settles a
 * refusal but not a permission: it may not be able to reach a service the browser
 * reaches perfectly, and it answers `unknown` for that rather than `no`. The browser
 * settles the other half by trying, which is the only way to find out whether the
 * page actually renders in a frame.
 */
function Dashboards() {
  const [report, setReport] = useState<EmbedReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Bumped by a re-probe, so the iframes below remount and try again rather than
  // keeping the verdict the last attempt reached.
  const [attempt, setAttempt] = useState(0);

  const probe = useAction(async (_signal: AbortSignal, refresh: boolean) => {
    const result = await api.embedProbes(refresh);
    setReport(result);
    setError(null);
    setAttempt((n) => n + 1);
    return result;
  });

  useEffect(() => {
    // On pane open, as §5.12 asks. The backend caches with a TTL, so this is not a
    // probe per mount.
    api
      .embedProbes()
      .then((result) => {
        setReport(result);
        setAttempt((n) => n + 1);
      })
      .catch((e: unknown) => setError(describe(e)));
  }, []);

  return (
    <Pane
      title="Dashboards"
      subtitle="Ray and MLflow, embedded where framing allows and linked where it does not"
      actions={
        <Button variant="outline"
          className={probe.pending ? "busy animate-pulse" : undefined}
          onClick={() => void probe.invoke(true)}
          disabled={probe.pending}
        >
          {probe.pending ? "Probing…" : "Re-probe"}
        </Button>
      }
    >
      {error && <p className="error text-destructive">{error}</p>}
      {probe.error && <p className="error text-destructive">{probe.error}</p>}
      {!report && !error && <Busy>Probing…</Busy>}
      {report && (
        <>
          <div className="exp-embeds">
            {report.services.map((service) => (
              <EmbedCard key={service.service} probe={service} attempt={attempt} />
            ))}
          </div>
          <p className="muted text-muted-foreground">
            {report.cached ? "From the cached verdict" : "Freshly probed"} · TTL {report.ttl} · as of{" "}
            {dateTime(report.as_of)}
          </p>
        </>
      )}
    </Pane>
  );
}

/** How long the browser waits for a frame before calling it a refusal. */
const FRAME_TIMEOUT_MS = 6000;

/** What the browser made of its own attempt to frame a service. */
export type BrowserVerdict = "probing" | "loaded" | "timeout";

/**
 * framingVerdict combines the two probes into the one thing the card renders.
 *
 * A backend `no` wins outright: it read the header, and a browser reports an
 * `X-Frame-Options: DENY` refusal as a load of the error page, so the iframe cannot
 * contradict it. Everything else defers to the browser, which is why `unknown` does
 * not stop the attempt — ODE could not tell, not "framing fails" (D6).
 */
export function framingVerdict(
  embeddable: EmbedProbe["embeddable"],
  browser: BrowserVerdict,
): "ok" | "refused" | "probing" {
  if (embeddable === "no") return "refused";
  if (browser === "timeout") return "refused";
  if (browser === "loaded") return "ok";
  return "probing";
}

/**
 * One service's card: the frame, or the link that replaces it.
 *
 * Embed, hide and open-in-a-new-tab are offered whatever the verdict was, which is
 * §5.12's own wording — only the *content* of the card is conditional. A developer
 * who hid a working frame should be able to bring it back without re-probing, and
 * one whose frame was refused still gets the pop-out.
 */
function EmbedCard({ probe, attempt }: { probe: EmbedProbe; attempt: number }) {
  const [shown, setShown] = useState(true);
  const [browser, setBrowser] = useState<BrowserVerdict>("probing");
  const timer = useRef<number | null>(null);

  useEffect(() => {
    setBrowser("probing");
    timer.current = window.setTimeout(() => setBrowser("timeout"), FRAME_TIMEOUT_MS);
    return () => {
      if (timer.current !== null) window.clearTimeout(timer.current);
    };
  }, [probe.url, attempt]);

  const settle = useCallback(() => {
    if (timer.current !== null) window.clearTimeout(timer.current);
    setBrowser("loaded");
  }, []);

  const verdict = framingVerdict(probe.embeddable, browser);
  const label = probe.service === "ray" ? "Ray dashboard" : "MLflow";

  return (
    <div className="exp-embed">
      <div className="exp-embed-head">
        <span className="exp-embed-title">{label}</span>
        <div className="exp-embed-actions">
          <Button variant="outline" className={shown ? "active" : undefined} onClick={() => setShown(true)}>
            Embed
          </Button>
          <Button variant="outline" className={shown ? undefined : "active"} onClick={() => setShown(false)}>
            Hide
          </Button>
          {/*
            A real anchor rather than window.open: this is the "Open in new tab" of
            D6's link-only card and also the pop-out §5.12 asks every pane to have,
            and a developer should be able to middle-click or copy it like any link.
          */}
          <a className="exp-popout" href={probe.url} target="_blank" rel="noreferrer noopener">
            Open in new tab
          </a>
        </div>
      </div>

      {/*
        The hidden probe of §5.12, mounted outside the Hide switch: hiding the card
        is the developer saying they do not want the dashboard on screen, not that
        the question of whether it can be framed should go unanswered. It runs
        whenever the verdict is still open, which is every service the backend did
        not already rule out — "unknown" included, because that means ODE could not
        tell rather than that framing fails.
      */}
      {verdict === "probing" && (
        <iframe
          className="exp-frame-probe"
          src={probe.url}
          title={`${label} framing probe`}
          onLoad={settle}
          aria-hidden="true"
          tabIndex={-1}
        />
      )}

      {/*
        Hide works whatever the probe decided. §5.12 asks for embed, hide and
        pop-out regardless of the outcome, and a Hide button that does nothing on a
        link-only card would be a control that lies about what it does — the card
        still takes space on the pane whether or not it holds a frame.
      */}
      {!shown && <Muted>Hidden. Press Embed to bring it back.</Muted>}
      {shown && verdict === "probing" && (
        <Busy>Checking whether {label} can be framed…</Busy>
      )}
      {shown && verdict === "ok" && <iframe className="exp-frame" src={probe.url} title={label} />}
      {shown && verdict === "refused" && (
        <p className="exp-embed-refused">
          {label} cannot be shown in a frame here, so this is a link instead of an embed. Use
          “Open in new tab” above.
        </p>
      )}

      {/*
        ODE's own reading, always — a verdict without the header that produced it is
        not actionable by whoever would have to change it. The sentence after it is
        added rather than substituted, because "unknown" is a statement about ODE's
        vantage point and the reason is a statement about the service.
      */}
      <p className="muted exp-embed-reason text-muted-foreground">
        {probe.reason}
        {probe.status !== undefined ? ` · HTTP ${probe.status}` : ""}
        {probe.embeddable === "unknown"
          ? " — ODE probes from inside the cluster and your browser does not, so the browser was asked as well"
          : ""}
      </p>
    </div>
  );
}
