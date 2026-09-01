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

import { useCallback, useEffect, useState } from "react";
import {
  ApiError,
  api,
  type EvaluationCriterion,
  type Experiment,
  type ExperimentCredential,
  type ExperimentFailure,
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
import { ExternalLinkIcon } from "lucide-react";
import { Badge } from "@/components/ui/badge";
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
 *   - **Ray and MLflow are linked, never framed (D6).** Two links in the header and
 *     a pair on every run, rather than a dashboard embedded in a column that has no
 *     room for one. The module also holds the launch card the conversation renders,
 *     because the links and the statuses in it are these same two.
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
          <>
            <DashboardLinks session={session} />
            <Button variant="outline" onClick={() => void load()} disabled={loading}>
              Refresh
            </Button>
          </>
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
      <RunDocument
        key={selected?.experiment_id ?? "none"}
        experiment={selected}
        urls={session.experiments}
      />
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
    <Table>
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

function RunDocument({
  experiment,
  urls,
}: {
  experiment: Experiment | null;
  /** Where a browser should open Ray and MLflow, from `/session`. */
  urls: Session["experiments"];
}) {
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
      <Submission experiment={experiment} urls={urls} />
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
      {/*
        First, and only for a run that failed. A developer opening a failed run has
        one question, and the comparison against the previous run is not an answer to
        it — there are no metrics to compare.
      */}
      {summary.failure && <WhyItFailed failure={summary.failure} />}

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

/**
 * The exception a failed run raised, which is the one thing its metrics cannot say.
 *
 * The block is extracted from the job's output rather than excerpted from it (D34),
 * and this pane serves it as it was raised: it is the developer's own data on their
 * own token, and the whole log is under Logs beside it. What the assistant reads is
 * the same block with its literals masked below exposure tier L2 — which is said
 * here rather than left to be discovered, because a developer who cannot see the
 * difference cannot tell whether an assistant that failed to name a value was
 * withholding it or guessing.
 */
function WhyItFailed({ failure }: { failure: ExperimentFailure }) {
  if (failure.not_diagnosed) {
    return (
      <Section title="Why it failed">
        <p className="exp-failure-none">
          The job left no readable exception — {failure.not_diagnosed.reason.replace(/_/g, " ")}.{" "}
          {failure.not_diagnosed.detail}
        </p>
      </Section>
    );
  }

  return (
    <Section title="Why it failed" note={failure.exception}>
      <p className="exp-failure-message font-mono text-sm break-words">{failure.message}</p>
      {failure.frames && failure.frames.length > 0 && (
        <ol className="exp-frames mt-2 space-y-0.5 text-xs text-muted-foreground">
          {/* Python's own order: outermost call first, where it raised last. */}
          {failure.frames.map((frame) => (
            <li key={`${frame.file}:${frame.line}:${frame.function ?? ""}`}>
              <code>
                {frame.file}:{frame.line}
              </code>
              {frame.function ? ` in ${frame.function}` : ""}
            </li>
          ))}
        </ol>
      )}
      <p className="advisory mt-2 text-xs text-muted-foreground">
        The last exception out of the job's own output, as it was raised. The assistant reads the
        same block with every literal replaced by <code>[value]</code> below exposure tier L2 — a
        value in a traceback is a value. The whole output is under Logs, which has a route and no
        tool: only you can read it.
      </p>
    </Section>
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
    <Table className="exp-comparison">
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
function Submission({
  experiment,
  urls,
}: {
  experiment: Experiment;
  urls: Session["experiments"];
}) {
  return (
    <Section title="Submission" defaultOpen={false}>
      <KV>
        <Row label="Experiment">
          <code>{experiment.experiment_id}</code>
        </Row>
        {/*
          The ids stay, and the links sit beside them. An id is what a developer
          quotes in a ticket or greps a Ray log for; a link is what they press. D6
          replaced the framed dashboards with exactly this — the job and the run,
          opened where they belong.
        */}
        <Row label="Ray submission">
          <code>{experiment.submission_id}</code>
        </Row>
        <Row label="MLflow run">
          <code>{experiment.mlflow_run_id}</code>
        </Row>
        <Row label="Open">
          <RunLinks experiment={experiment} urls={urls} />
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

// --- the links into Ray and MLflow (D6) ---

/**
 * Ray and MLflow, as two links in the pane's header.
 *
 * D6 used to say the two UIs were probed at runtime and framed where the headers
 * allowed. They are not framed any more, and the probe is gone with them: a
 * dashboard that does frame is still a second application's chrome — its own
 * navigation, its own sidebar, its own idea of how wide it is — inside a pane with
 * room for none of it, and what a developer does with it is open it properly. Two
 * links do that and cost the pane no room at all, which is why they are beside
 * Refresh rather than in a card of their own.
 *
 * The URLs come from `/session` rather than from a probe, which is the same
 * configuration the probe reported: `ray_dashboard_url` and `mlflow_ui_url`, each
 * falling back to the API base ODE itself calls.
 */
function DashboardLinks({ session }: { session: Session }) {
  const urls = session.experiments;
  if (!urls?.ray_url && !urls?.mlflow_url) return null;
  return (
    <span className="exp-links">
      {urls.ray_url && <Popout href={urls.ray_url}>Ray dashboard</Popout>}
      {urls.mlflow_url && <Popout href={urls.mlflow_url}>MLflow</Popout>}
    </span>
  );
}

/**
 * Popout is every link that leaves ODE, drawn as one.
 *
 * It has to *look* like a link. These sat as bare anchors carrying a class with no
 * rule behind it, and the preset's reset had already taken the colour and the
 * underline off every `a` — so "Ray job" and "MLflow run" read as two words of
 * prose beside a status, and nobody would think to click them. The underline and
 * the colour say it is a link; the arrow says it opens elsewhere, and the screen
 * reader is told the same thing in words rather than left with an icon.
 */
function Popout({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      className="exp-popout inline-flex items-center gap-1 text-primary underline decoration-primary/40 underline-offset-2 hover:decoration-primary"
      href={href}
      target="_blank"
      rel="noreferrer noopener"
    >
      {children}
      <ExternalLinkIcon className="size-3" aria-hidden="true" />
      <span className="sr-only">(opens in a new tab)</span>
    </a>
  );
}

/**
 * rayJobUrl and mlflowRunUrl build the deep links D6 replaced the frames with.
 *
 * Built here rather than served, because the record already carries everything
 * they need and the two bases are in `/session`: a route that returned the strings
 * would be a second place for the same join to be wrong. Null when a base is
 * missing or the record has no id for that half — a submission that failed before
 * ODE created the MLflow run has no run to open, and a link to nothing is worse
 * than no link.
 */
export function rayJobUrl(base: string | undefined, submissionId: string): string | null {
  if (!base || !submissionId) return null;
  return `${trimSlash(base)}/#/jobs/${encodeURIComponent(submissionId)}`;
}

export function mlflowRunUrl(
  base: string | undefined,
  experimentId: string,
  runId: string,
): string | null {
  if (!base || !experimentId || !runId) return null;
  return `${trimSlash(base)}/#/experiments/${encodeURIComponent(experimentId)}/runs/${encodeURIComponent(runId)}`;
}

/** trimSlash keeps a configured base with a trailing slash from doubling it. */
function trimSlash(base: string): string {
  return base.replace(/\/+$/, "");
}

/**
 * RunLinks is the pair of pop-outs a run carries: the Ray job, and the MLflow run.
 *
 * One component because the two are always offered together and each can
 * legitimately be absent — an unconfigured base, or a launch that never got as far
 * as a run — and a card that renders one of them still reads correctly.
 */
export function RunLinks({
  experiment,
  urls,
}: {
  experiment: Pick<Experiment, "submission_id" | "mlflow_experiment_id" | "mlflow_run_id">;
  urls: { ray_url?: string; mlflow_url?: string } | undefined;
}) {
  const ray = rayJobUrl(urls?.ray_url, experiment.submission_id);
  const mlflow = mlflowRunUrl(
    urls?.mlflow_url,
    experiment.mlflow_experiment_id,
    experiment.mlflow_run_id,
  );
  if (!ray && !mlflow) return null;
  return (
    <span className="exp-links">
      {ray && <Popout href={ray}>Ray job</Popout>}
      {mlflow && <Popout href={mlflow}>MLflow run</Popout>}
    </span>
  );
}

// --- what a launch looks like in the conversation (§5.12, D6) ---

/**
 * The runs one conversation has launched, polled while any of them is unfinished.
 *
 * It exists because the assistant's answer to "launch it" is a tool result: an
 * experiment id, a submission id and the status Ray gave the job in the second it
 * was accepted, which is `PENDING` for every run that ever succeeded. The
 * developer's next question is whether it is still going, and re-reading a JSON
 * blob in a folded tool call cannot answer it.
 *
 * One hook per conversation rather than one per launch card. Three launches in a
 * conversation are three cards, and three pollers asking the same question of the
 * same route four times a minute is a cost with nothing to show for it.
 *
 * The listing route carries `ray_url` and `mlflow_url` beside the records, so the
 * links come from the same read as the statuses and the card needs no configuration
 * of its own.
 */
export interface LaunchedRuns {
  /** This conversation's runs, newest first. Empty until the first read lands. */
  runs: Experiment[];
  /** Where a browser should open Ray and MLflow, as the listing reported them. */
  urls: { ray_url: string; mlflow_url: string } | undefined;
  error: string | null;
  /** False only before the first read answers, so a card can say it is looking. */
  loaded: boolean;
}

/**
 * useLaunchedRuns reads them, and keeps reading while something is unfinished.
 *
 * Two reads rather than one, and the second is the one that makes the card honest.
 * The listing answers with the developer's runs and is filtered here to the ones
 * this conversation launched — that is what the card's "what else is still going"
 * half is made of. But a run that is missing from that filtered list is not
 * evidence that it does not exist: the record's `session_id` is written at launch
 * and a run reaching the developer by any other route has none, and the listing is
 * capped. So every id a launch actually returned that the filter did not produce is
 * read directly by id. One extra request per unaccounted launch, and the launch a
 * developer is looking at is never missing from the card that belongs to it.
 *
 * `launchedIds` is joined into a key because it is a fresh array on every render of
 * the transcript, and an effect keyed on the array itself would re-read on every
 * token that arrives. The ids are recovered from that key inside `load`, so the
 * dependency and the value used cannot drift apart.
 */
export function useLaunchedRuns(
  sessionId: string,
  launches: number,
  launchedIds: string[],
): LaunchedRuns {
  const [runs, setRuns] = useState<Experiment[]>([]);
  const [urls, setUrls] = useState<{ ray_url: string; mlflow_url: string } | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);

  // The count leads, because a launch whose result carried no readable id still has
  // to start the read: without it the card under that call would wait for ever.
  const key = `${launches}:${launchedIds.join(" ")}`;

  const load = useCallback(async () => {
    const ids = key.slice(key.indexOf(":") + 1).split(" ").filter(Boolean);
    try {
      const listing = await api.experiments();
      const mine = listing.experiments.filter((run) => run.session_id === sessionId);
      const seen = new Set(mine.map((run) => run.experiment_id));

      // Tolerated rather than awaited as a group: a run that cannot be read — it was
      // pruned, or the record is another developer's — must not take the runs that
      // can be read down with it, and the listing is already an answer.
      const missing = await Promise.all(
        ids
          .filter((id) => !seen.has(id))
          .map((id) => api.experiment(id).catch(() => null)),
      );

      const merged = [...mine, ...missing.filter((run): run is Experiment => run !== null)];
      merged.sort((left, right) => right.submitted_at.localeCompare(left.submitted_at));
      setRuns(merged);
      setUrls({ ray_url: listing.ray_url, mlflow_url: listing.mlflow_url });
      setError(null);
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setLoaded(true);
    }
  }, [key, sessionId]);

  useEffect(() => {
    if (launches === 0) return;
    void load();
  }, [launches, load]);

  // The same predicate the pane polls on, and for the same reason: a list of
  // finished runs cannot change, so re-reading it asks Ray nothing. This is what
  // makes the last poll after a job settles also the one that switches it off.
  const pending = launches > 0 && hasUnfinished(runs);
  useEffect(() => {
    if (!pending) return;
    const timer = window.setInterval(() => void load(), POLL_MS);
    return () => window.clearInterval(timer);
  }, [pending, load]);

  return { runs, urls, error, loaded };
}

/**
 * launchedExperimentId picks the run out of a `launch_experiment` result.
 *
 * Three shapes, because a tool result reaches the transcript by two routes. A
 * provider whose tool loop ODE runs stores what the executor returned, so the
 * result is the object itself. The CLI provider (§5.7) runs its own loop over MCP
 * and echoes the client's result back verbatim, which is MCP's envelope: a list of
 * content blocks whose text happens to be the JSON. And an unparseable result is
 * kept as the raw string it arrived as (see `replay`), which is worth one more
 * parse here rather than a card that silently has no run.
 *
 * Reading only the first shape is what put "No run of this conversation is on the
 * cluster yet" under a launch that had plainly succeeded.
 */
export function launchedExperimentId(content: unknown): string | null {
  if (typeof content === "string") {
    return launchedExperimentId(parseJSON(content));
  }
  if (Array.isArray(content)) {
    for (const block of content) {
      const text = (block as { type?: string; text?: string } | null)?.text;
      if (typeof text !== "string") continue;
      const found = launchedExperimentId(parseJSON(text));
      if (found) return found;
    }
    return null;
  }
  const result = content as { experiment_id?: string } | null;
  return typeof result?.experiment_id === "string" ? result.experiment_id : null;
}

/** parseJSON answers null rather than throwing, because a tool result may be prose. */
function parseJSON(raw: string): unknown {
  try {
    return JSON.parse(raw);
  } catch {
    return null;
  }
}

/**
 * liveLabel is the word the conversation uses for a job's state.
 *
 * The pane keeps Ray's own vocabulary deliberately — it sits beside the Ray
 * dashboard, and two names for one state would give a developer two answers to
 * reconcile. A chat card is read at a glance and in the middle of a sentence, so it
 * says the three things the reader is actually waiting to hear apart, and carries
 * Ray's own word in the title for whoever is about to go and look for it.
 */
export function liveLabel(status: ExperimentStatus): "running" | "finished" | "error" | "stopped" {
  switch (status) {
    case "PENDING":
    case "RUNNING":
      return "running";
    case "SUCCEEDED":
      return "finished";
    case "FAILED":
      return "error";
    case "STOPPED":
      return "stopped";
  }
}

/**
 * The card under a `launch_experiment` call: what this call started, and what else
 * is still going.
 *
 * Not the whole conversation's history. A finished run from an hour ago is answered
 * by the Experiments pane, and repeating it under every launch would make the rule
 * "everything, forever" — three launches in an afternoon and the transcript carries
 * the same six rows three times. The rule here is narrower and is the one a reader
 * of *this* call wants: the run it produced, whatever became of it, and anything
 * else of theirs that has not finished yet.
 *
 * Each row carries three links, and the third is the point of the other two being
 * only links: Ray has the job, MLflow has the run, and the thing a developer
 * actually came back for — the summary, the comparison and the interpretation of
 * §5.13 — is in ODE's own run document.
 */
/**
 * launchedRows is what one launch card shows: its own run, then what is still going.
 *
 * A function rather than an expression in the card, because the rule is the
 * decision — the card's own run stays whatever became of it, everything else earns
 * its place by being unfinished, and neither list may repeat the other. Newest
 * first is the listing's own order and is kept.
 */
export function launchedRows(runs: Experiment[], experimentId: string | null): Experiment[] {
  const own = runs.find((run) => run.experiment_id === experimentId) ?? null;
  return [...(own ? [own] : []), ...runs.filter((run) => run !== own && !isFinished(run.status))];
}

export function LaunchedRunsCard({
  experimentId,
  launched,
}: {
  /** The run this launch produced, from the tool result. */
  experimentId: string | null;
  launched: LaunchedRuns;
}) {
  const { runs, urls, error, loaded } = launched;
  const rows = launchedRows(runs, experimentId);

  return (
    <div className="exp-launched flex flex-col gap-1.5 rounded-md border bg-card/50 px-3 py-2">
      <span className="text-xs font-medium text-muted-foreground">Experiment runs</span>
      {error && (
        <p className="muted text-xs text-muted-foreground">The runs could not be read: {error}</p>
      )}
      {!error && !loaded && <Busy>Reading the runs this conversation launched…</Busy>}
      {/*
        An empty list is not an error and is not "nothing was launched": the record
        exists, and the listing has not caught up with a launch accepted a moment
        ago. The next poll fills it in.
      */}
      {!error && loaded && rows.length === 0 && (
        <Muted>No run of this conversation is on the cluster yet.</Muted>
      )}
      {rows.map((run) => (
        <LaunchedRunRow key={run.experiment_id} run={run} urls={urls} />
      ))}
    </div>
  );
}

function LaunchedRunRow({
  run,
  urls,
}: {
  run: Experiment;
  urls: LaunchedRuns["urls"];
}) {
  const label = liveLabel(run.status);
  const tone = statusTone(run.status);
  return (
    <div className="exp-launched-row flex flex-wrap items-center gap-2">
      <Badge
        variant={tone === "bad" ? "destructive" : tone === "running" ? "default" : "secondary"}
        className={tone === "running" ? "font-normal animate-pulse" : "font-normal"}
        // Ray's own word, for a developer about to look for this job in the
        // dashboard the link beside it opens.
        title={`Ray reports ${run.status}`}
      >
        {label}
      </Badge>
      <code className="text-xs">{run.entrypoint}</code>
      <span className="muted-inline text-xs text-muted-foreground" title={run.commit_sha}>
        {run.commit_sha.slice(0, 7)}
      </span>
      <RunLinks experiment={run} urls={urls} />
      {/*
        The third link is ODE's own, and it is deliberately not a pop-out: the run
        document is what §5.13 is delivered into — the summary, the comparison
        against the previous run, and the assistant's interpretation with the
        proposal there is something to decide about — and none of that is in Ray or
        in MLflow. Same tab, no arrow, because it is a move within this application
        rather than a departure from it.
      */}
      <Link
        className="exp-open-run text-primary underline decoration-primary/40 underline-offset-2 hover:decoration-primary"
        to={`/tools/experiments?run=${encodeURIComponent(run.experiment_id)}`}
        title="Opens this run in the Experiments pane: its summary, the comparison with the previous run, and the assistant's interpretation"
      >
        Results
      </Link>
    </div>
  );
}
