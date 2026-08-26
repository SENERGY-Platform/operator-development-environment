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

import { useCallback, useState } from "react";
import type {
  AspectMatch,
  CandidateDevice,
  Criterion,
  DeviceClassMatch,
  FunctionMatch,
  Matched,
  OntologyGap,
  Selectable,
  SelectionRequest,
  SelectionResult,
} from "./api";
// The candidate table, its score bar and the read counter come from the profiler
// view rather than being written again here: the columns mean the same thing in
// both places, and a second table would be the one that drifts.
import { CandidateRow, DEFAULT_DEVICE_LIMIT, ReadCounter, ScoreBar, seriesKey } from "./profiler";
import { setParam, useLocation } from "./router";
import { profilerSocket } from "./ws";
import { Muted, Pane, Section, date, shortId, useAction } from "./ui";

/**
 * The M2 surface: semantic data selection (SPEC §5.2).
 *
 * An intent in words becomes concrete series through the ontology, and the view
 * is built to make each step auditable rather than to hide them. The matcher is
 * lexical, so every match carries its score and the words it used; the criteria
 * that were actually sent are shown with what each found; and the read counter
 * says the whole thing cost no value read, which is the tier-L0 claim of §3.2.
 *
 * There is no path from here into the profiler. Promoting a resolved series to a
 * selection is `propose_data_selection`, which needs developer confirmation and
 * arrives with the tool surface in M3.
 */
/** The interactions the form offers, and the guard for one arriving from a URL. */
const INTERACTIONS = ["event", "event+request", "request", "any"] as const;

export function SelectionView() {
  const { params } = useLocation();

  /*
   * The URL restores the inputs, not the result.
   *
   * A resolution cannot be re-fetched by id — it is computed per request, and
   * ranking it reads availability once per device, which is the slow part. So
   * reloading this view puts the intent and its options back into the form and
   * leaves the Resolve button to the developer, rather than spending their quota
   * on a read they did not ask for a second time. Read once, at mount: after that
   * the form is the developer's and the URL follows it on submit.
   */
  const [form, setForm] = useState(() => {
    const interaction = params.get("interaction");
    return {
      intent: params.get("intent") ?? "",
      limit: params.get("limit") ?? String(DEFAULT_DEVICE_LIMIT),
      interaction: (INTERACTIONS as readonly string[]).includes(interaction ?? "")
        ? (interaction as NonNullable<SelectionRequest["interaction"]>)
        : ("event" as NonNullable<SelectionRequest["interaction"]>),
      includeControlling: params.get("controlling") === "1",
      // Ranking is on by default, so only its absence is written.
      rank: params.get("rank") !== "0",
    };
  });

  // Over the socket, like the candidate listing, and for the same reason: a
  // resolution expands devices and availability is one call per device, so
  // changing your mind should stop those reads rather than leave them running.
  const resolve = useAction((signal: AbortSignal, request: SelectionRequest) =>
    profilerSocket.request<SelectionResult>("resolve_selection", request, signal),
  );

  const submit = useCallback(
    (event: React.FormEvent) => {
      event.preventDefault();
      const intent = form.intent.trim();
      if (!intent) return;
      // Written on submit rather than on every keystroke: the parameters should
      // describe the resolution on screen, not whatever is half-typed above it.
      setParam("intent", intent);
      setParam("limit", form.limit === String(DEFAULT_DEVICE_LIMIT) ? null : form.limit);
      setParam("interaction", form.interaction === "event" ? null : form.interaction);
      setParam("controlling", form.includeControlling ? "1" : null);
      setParam("rank", form.rank ? null : "0");
      void resolve.invoke({
        intent,
        limit: deviceLimit(form.limit),
        interaction: form.interaction,
        include_controlling: form.includeControlling,
        rank: form.rank,
      });
    },
    [form, resolve],
  );

  const result = resolve.data;

  return (
    <main className="panes selection">
      <Pane
        title="Intent"
        subtitle="Resolved against the platform ontology — no model, no values, tier L0"
      >
        <form className="filters" onSubmit={submit}>
          <input
            value={form.intent}
            onChange={(e) => setForm({ ...form, intent: e.target.value })}
            placeholder="forecast PV generation for this site"
            aria-label="Intent"
          />
          <label
            className="filter-check"
            title="Availability is one call per device and cannot be batched, so this is what decides how long a resolution takes"
          >
            <span>Devices</span>
            <input
              value={form.limit}
              onChange={(e) => setForm({ ...form, limit: e.target.value })}
              inputMode="numeric"
              aria-label="Device limit"
            />
          </label>
          <label
            className="filter-check"
            title="A request-only service is polled on demand and streams nothing, so no series exists for it"
          >
            <span>Interaction</span>
            <select
              value={form.interaction}
              onChange={(e) =>
                setForm({
                  ...form,
                  interaction: e.target.value as NonNullable<SelectionRequest["interaction"]>,
                })
              }
              aria-label="Interaction"
            >
              <option value="event">event</option>
              <option value="event+request">event+request</option>
              <option value="request">request</option>
              <option value="any">any</option>
            </select>
          </label>
          <label className="filter-check">
            <input
              type="checkbox"
              checked={form.includeControlling}
              onChange={(e) => setForm({ ...form, includeControlling: e.target.checked })}
            />
            <span title="A series is something measured; controlling functions actuate">
              controlling too
            </span>
          </label>
          <label className="filter-check">
            <input
              type="checkbox"
              checked={form.rank}
              onChange={(e) => setForm({ ...form, rank: e.target.checked })}
            />
            <span title="Ranking reads availability per device. Unticked, the resolution is ontology only and costs no per-device call">
              rank by QuickProfile
            </span>
          </label>
          <button type="submit" disabled={resolve.pending || form.intent.trim() === ""}>
            Resolve
          </button>
          {resolve.pending && (
            <button type="button" onClick={resolve.abort}>
              Cancel
            </button>
          )}
        </form>

        {resolve.pending && <Muted>Resolving through the ontology…</Muted>}
        {resolve.error && <Muted>{resolve.error}</Muted>}
        {!result && !resolve.pending && !resolve.error && (
          <Muted>
            Describe what you want to model. The words are matched against function,
            aspect and device-class names — the ontology's vocabulary, not a synonym
            list, so anything it could not place is reported back to you.
          </Muted>
        )}

        {result && <Resolution result={result} />}
      </Pane>

      <Pane
        title="Resolved series"
        subtitle="Concrete addressable series, ranked from metadata alone"
      >
        {!result && <Muted>Nothing resolved yet.</Muted>}
        {result && <Resolved result={result} />}
      </Pane>
    </main>
  );
}

/** The backend clamps its own ceiling, so this only has to reject nonsense. */
function deviceLimit(raw: string): number {
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isFinite(parsed) || parsed <= 0) return DEFAULT_DEVICE_LIMIT;
  return parsed;
}

/** What the intent resolved to, and what it did not. */
function Resolution({ result }: { result: SelectionResult }) {
  return (
    <>
      <div className="terms">
        {result.terms.map((term) => (
          <span
            key={term}
            className={`tag ${result.unmatched_terms.includes(term) ? "warn" : "ok-tag"}`}
            title={
              result.unmatched_terms.includes(term)
                ? "No ontology entity used this word"
                : "Used by a match below"
            }
          >
            {term}
          </span>
        ))}
      </div>
      {result.unmatched_terms.length > 0 && (
        <Muted>
          {result.unmatched_terms.length} word
          {result.unmatched_terms.length === 1 ? "" : "s"} matched nothing. The ontology
          has its own wording for things; this is which of yours it does not share.
        </Muted>
      )}

      {result.notes.length > 0 && (
        <ul className="list notes">
          {result.notes.map((note) => (
            <li key={note}>{note}</li>
          ))}
        </ul>
      )}

      <Section title={`Functions (${result.matched_functions.length})`}>
        {result.matched_functions.length === 0 && <Muted>No function matched.</Muted>}
        {result.matched_functions.map((match: FunctionMatch) => (
          <MatchRow
            key={match.id}
            name={match.name || shortId(match.id)}
            id={match.id}
            matched={match.matched}
          />
        ))}
      </Section>

      <Section title={`Aspects (${result.matched_aspects.length})`}>
        {result.matched_aspects.length === 0 && (
          <Muted>No aspect matched, so the query was not scoped to a subsystem.</Muted>
        )}
        {result.matched_aspects.map((match: AspectMatch) => (
          <MatchRow
            key={match.id}
            name={`${match.name || shortId(match.id)}${match.descendants_included ? " + descendants" : ""}`}
            id={match.id}
            matched={match.matched}
          />
        ))}
      </Section>

      {result.matched_device_classes.length > 0 && (
        <Section
          title={`Device classes (${result.matched_device_classes.length})`}
          note="reported, not applied"
          defaultOpen={false}
        >
          {result.matched_device_classes.map((match: DeviceClassMatch) => (
            <MatchRow
              key={match.id}
              name={match.name || shortId(match.id)}
              id={match.id}
              matched={match.matched}
            />
          ))}
        </Section>
      )}

      <Section
        title={`Criteria sent (${result.criteria.length})`}
        note="one request each"
        defaultOpen={false}
      >
        {result.criteria.length === 0 && <Muted>Nothing was queried.</Muted>}
        {result.criteria.length > 0 && (
          <table className="grid">
            <thead>
              <tr>
                <th>Function</th>
                <th>Aspect</th>
                <th title="How many device types this combination returned">Types</th>
              </tr>
            </thead>
            <tbody>
              {result.criteria.map((criterion: Criterion) => (
                <tr key={criterionKey(criterion)}>
                  <td>{criterion.function_id ? shortId(criterion.function_id) : <Any />}</td>
                  <td>{criterion.aspect_id ? shortId(criterion.aspect_id) : <Any />}</td>
                  <td className="numeric">{criterion.device_types}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <Muted>
          The platform ANDs a criteria list, so alternatives are separate requests and
          the answers are merged here. A row finding no device type means the ontology
          describes it and this platform has none.
        </Muted>
      </Section>
    </>
  );
}

function criterionKey(criterion: Criterion): string {
  return `${criterion.function_id ?? ""}|${criterion.aspect_id ?? ""}|${criterion.device_class_id ?? ""}`;
}

function Any() {
  return <span className="muted-inline">any</span>;
}

function MatchRow({ name, id, matched }: { name: string; id: string; matched: Matched }) {
  return (
    <div className="match">
      <span className="match-name" title={id}>
        {name}
      </span>
      <span className="match-score">
        <ScoreBar score={matched.score} />
      </span>
      <span className="match-basis">
        <span className="tag" title="Which label the match rests on">
          {matched.basis.replace(/_/g, " ")}
        </span>
        {matched.matched_terms.map((term) => (
          <span key={term} className="tag ok-tag">
            {term}
          </span>
        ))}
      </span>
    </div>
  );
}

/** The series the resolution arrived at, plus what the ontology failed to declare. */
function Resolved({ result }: { result: SelectionResult }) {
  const ranked = result.candidates.length > 0;
  const deviceTypes = new Set(result.selectables.map((s) => s.device_type_id));

  return (
    <>
      <ReadCounter
        reads={result.reads}
        detail={`${result.reads.selectables} selectables · ${result.reads.device_lists} device list · ${result.reads.availability} availability · ${result.reads.usage} usage`}
      />

      <Muted>
        {result.selectables.length} variable{result.selectables.length === 1 ? "" : "s"} across{" "}
        {deviceTypes.size} device type{deviceTypes.size === 1 ? "" : "s"},{" "}
        {result.candidate_devices.length} readable device
        {result.candidate_devices.length === 1 ? "" : "s"}
        {result.total_devices > result.candidate_devices.length &&
          ` of ${result.total_devices}`}
        {ranked && (
          <>
            , coverage over {date(result.coverage_window.from)} to{" "}
            {date(result.coverage_window.to)}
          </>
        )}
      </Muted>

      {ranked && (
        <table className="grid candidates">
          <thead>
            <tr>
              <th>#</th>
              <th>Device</th>
              <th>Variable</th>
              <th>Unit</th>
              <th title="Days between the first and last available point">Span</th>
              <th title="Share of the requested window the data actually spans">Cover</th>
              <th title="Newest point within a day, and the device is not offline">Live</th>
              <th title="0.3 span + 0.4 coverage + 0.3 liveness">Score</th>
            </tr>
          </thead>
          <tbody>
            {result.candidates.map((candidate, index) => (
              <CandidateRow key={seriesKey(candidate)} candidate={candidate} rank={index + 1} />
            ))}
          </tbody>
        </table>
      )}

      {!ranked && result.selectables.length > 0 && (
        <Muted>
          Not ranked, so these are the ontology's variables in path order rather than a
          shortlist. Tick “rank by QuickProfile” to order them by span, coverage and
          liveness.
        </Muted>
      )}

      <Section
        title={`Selectables (${result.selectables.length})`}
        note="device-type level"
        defaultOpen={!ranked}
      >
        {result.selectables.length === 0 && (
          <Muted>The criteria matched no device type on this platform.</Muted>
        )}
        {result.selectables.length > 0 && (
          <table className="grid">
            <thead>
              <tr>
                <th>Device type</th>
                <th>Variable</th>
                <th>Unit</th>
                <th>Aspect</th>
              </tr>
            </thead>
            <tbody>
              {result.selectables.map((selectable: Selectable) => (
                <tr
                  key={`${selectable.device_type_id}|${selectable.service_id}|${selectable.path}`}
                  className={selectable.queryable ? "" : "unreadable"}
                  title={selectable.reason}
                >
                  <td title={selectable.device_type_id}>
                    {selectable.device_type_name || shortId(selectable.device_type_id)}
                  </td>
                  <td>
                    <code>{selectable.path}</code>
                    {!selectable.queryable && <span className="tag warn">unreadable</span>}
                    {selectable.ontology_completeness.status === "partial" && (
                      <span
                        className="tag"
                        title={selectable.ontology_completeness.consequence}
                      >
                        partial
                      </span>
                    )}
                  </td>
                  <td>
                    {selectable.unit || <span className="muted-inline">unknown</span>}
                  </td>
                  <td>{selectable.aspect_name || shortId(selectable.aspect_id ?? "")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Section>

      <Section
        title={`Ontology gaps (${result.ontology_gaps.length})`}
        note="discovered at runtime"
        defaultOpen={result.ontology_gaps.length > 0}
      >
        {result.ontology_gaps.length === 0 && (
          <Muted>Every resolved variable declares its characteristic, function and aspect.</Muted>
        )}
        {result.ontology_gaps.map((gap: OntologyGap) => (
          <div className="gap" key={`${gap.device_type_id}|${gap.consequence}`}>
            <div className="gap-head">
              <span title={gap.device_type_id}>
                {gap.device_type_name || shortId(gap.device_type_id)}
              </span>
              {gap.missing.map((missing) => (
                <span key={missing} className="tag warn">
                  no {missing.replace(/_/g, " ")}
                </span>
              ))}
            </div>
            <div className="gap-consequence">{gap.consequence}</div>
            <div className="gap-paths">
              {gap.paths.map((path) => (
                <code key={path}>{path}</code>
              ))}
            </div>
          </div>
        ))}
      </Section>

      {result.candidate_devices.length > 0 && (
        <Section title={`Devices (${result.candidate_devices.length})`} defaultOpen={false}>
          <table className="grid">
            <thead>
              <tr>
                <th>Device</th>
                <th>State</th>
                <th title="How many resolved series this device contributes">Series</th>
              </tr>
            </thead>
            <tbody>
              {result.candidate_devices.map((device: CandidateDevice) => (
                <tr key={device.device_id}>
                  <td title={device.device_id}>
                    {device.name || shortId(device.device_id)}
                    <span className="device-type" title={device.device_type_id}>
                      {device.device_type_name || shortId(device.device_type_id)}
                    </span>
                  </td>
                  <td>
                    <span className={`state ${device.connection_state || "unknown"}`}>
                      {device.connection_state || "unknown"}
                    </span>
                  </td>
                  <td className="numeric">{device.series}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Section>
      )}

      {result.skipped.length > 0 && (
        <Section title={`Skipped devices (${result.skipped.length})`} defaultOpen={false}>
          <ul className="list">
            {result.skipped.map((skip) => (
              <li key={skip.device_id}>
                <strong>{skip.name || shortId(skip.device_id)}</strong>
                <span className="muted-inline"> — {skip.reason}</span>
              </li>
            ))}
          </ul>
        </Section>
      )}
    </>
  );
}
