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
  isNotComputed,
  type DeviceInfo,
  type QuickProfileList,
  type LLMProfileView,
  type ProfileResult,
  type Provenance,
  type QualityFlag,
  type QuickProfile,
  type ReadCounts,
  type ReadSummary,
  type SeriesProfile,
  type SessionPage,
} from "./api";
import { chartFromProfile } from "./exploration";
import { setParam, useLocation, useParam } from "./router";
import { profilerSocket, type SocketState } from "./ws";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import {
  Busy,
  ConfidenceTag,
  Field,
  KV,
  Muted,
  Pane,
  Row,
  Section,
  Val,
  bytes,
  date,
  dateTime,
  describe,
  num,
  percent,
  period,
  round,
  seconds,
  shortId,
  useAction,
  useLoad,
} from "./ui";

/**
 * The profiler surface. Drawing a series is the exploration pane's work (§5.9),
 * so this is not that: it is the developer-facing view of what `QuickProfile` and
 * `SeriesProfile` compute, enough to check both by hand — candidates ranked with
 * no value read, and a profile whose non-results say why.
 */
export function ProfilerView({
  onOpenChart,
}: {
  /**
   * Charts the profile on screen in the exploration pane (§5.9). Absent when no
   * exploration backend is configured. It belongs here because a chart of a
   * profiled series is the one thing this view cannot show: a session boundary or
   * a gap is a claim a developer has to see before confirming it (§5.10).
   */
  onOpenChart?: (chartId: string) => void;
} = {}) {
  const { params } = useLocation();
  const from = params.get("from");
  const to = params.get("to");

  /*
   * The window is in the URL rather than in state, because both panes mean the
   * same thing by it: the range the developer cares about. It ranks candidates by
   * coverage at tier L0, and it is the analysis window a profile is computed over.
   * Keeping it in one place is what stops the second use from silently not
   * happening — and putting that place in the address means a reload does not
   * quietly widen it back to everything.
   *
   * Memoised on the two strings rather than on the parameter bag: an unrelated
   * parameter changing — the open conversation, say — must not look like a new
   * window and set a fresh listing off against the platform.
   */
  const analysisWindow = useMemo<AppliedWindow>(
    () => (from && to ? { from: `${from}T00:00:00Z`, to: `${to}T00:00:00Z` } : null),
    [from, to],
  );

  const [selected, setSelected] = useState<QuickProfile | null>(null);
  const seriesParam = params.get("series");

  /*
   * One entry point for both a click and a restore.
   *
   * When the listing hands back the candidate the address already names, the keys
   * are equal and nothing is written — which is what leaves `?profile=` intact for
   * the pane on the right to restore. When the developer picks a different series,
   * the profile parameter goes with the old selection, because a profile belongs
   * to the series it was computed for.
   */
  const select = useCallback(
    (candidate: QuickProfile) => {
      setSelected(candidate);
      const key = seriesKey(candidate);
      if (key === seriesParam) return;
      setParam("series", key);
      setParam("profile", null);
    },
    [seriesParam],
  );

  /*
   * An address that names a profile but not the series it belongs to.
   *
   * Every URL this view writes carries both, because a profile can only be
   * computed after a candidate has been picked. A link written by hand, or copied
   * from a tool result, may carry only the profile id — and the pane on the right
   * cannot render one without the candidate the pane on the left selects. The
   * series is recoverable from the profile itself, so it is recovered, and the
   * listing then selects the candidate the way it does for any other restore.
   *
   * The second read of the same document that this costs happens only on this
   * path. A profile is immutable and stored (D21), so it is a store read and not
   * a platform one.
   */
  const profileParam = params.get("profile");
  useEffect(() => {
    if (!profileParam || seriesParam) return;
    let cancelled = false;
    api
      .profile(profileParam)
      .then((stored) => {
        if (cancelled) return;
        const ref = stored.series_ref;
        setParam("series", `${ref.device_id}|${ref.service_id}|${ref.variable_path}`);
      })
      .catch(() => {
        // No such profile. The parameter is dropped rather than left pointing at
        // nothing: what stays on screen is the candidate list, which is where a
        // developer would have to start anyway.
        if (!cancelled) setParam("profile", null);
      });
    return () => {
      cancelled = true;
    };
  }, [profileParam, seriesParam]);

  // Keying the right-hand pane resets everything it holds when the selection
  // moves to another service — a profile computed for one says nothing about
  // another — and when the window changes, because a profile *is* its window and
  // showing one computed over a range the developer has since changed is the same
  // mislabelling in a different guise.
  const paneKey = selected
    ? `${selected.series_ref.device_id}|${selected.series_ref.service_id}|${windowKey(analysisWindow)}`
    : "none";

  return (
    <main className="panes profiler">
      <CandidatesPane selected={selected} onSelect={select} analysisWindow={analysisWindow} />
      <ProfilePane
        key={paneKey}
        candidate={selected}
        analysisWindow={analysisWindow}
        onOpenChart={onOpenChart}
      />
    </main>
  );
}

/**
 * AppliedWindow is a resolved range or nothing. Null means "whatever the
 * platform has", which for a profile is the whole availability window — not an
 * empty range.
 */
export type AppliedWindow = { from: string; to: string } | null;

/**
 * DEFAULT_DEVICE_LIMIT mirrors the backend's own default. Ten because the listing
 * costs one availability call per device and they cannot be batched, so this is
 * the number that decides how long it takes — and ten devices is a list a
 * developer can read rather than scroll.
 */
export const DEFAULT_DEVICE_LIMIT = 10;
const MAX_DEVICE_LIMIT = 200;

function deviceLimit(raw: string): number {
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isFinite(parsed) || parsed <= 0) return DEFAULT_DEVICE_LIMIT;
  return Math.min(parsed, MAX_DEVICE_LIMIT);
}

/** ConnectionState says when the socket is down, so a stalled screen has a cause. */
function ConnectionState() {
  const [state, setState] = useState<SocketState>("idle");
  useEffect(() => profilerSocket.onState(setState), []);

  if (state === "open" || state === "idle") return null;
  const message =
    state === "connecting"
      ? "connecting…"
      : state === "reconnecting"
        ? "reconnecting — work in flight was cancelled"
        : "disconnected";
  return (
    <span className={`socket ${state}${state === "connecting" ? " busy animate-pulse" : ""}`}>{message}</span>
  );
}

function spanDays(range: { from: string; to: string }): number {
  return (new Date(range.to).getTime() - new Date(range.from).getTime()) / (24 * 3600 * 1000);
}

function windowKey(range: AppliedWindow): string {
  return range ? `${range.from}|${range.to}` : "full";
}

/**
 * rawWindowFor turns "the last N days" into the explicit range the backend
 * wants, anchored at the end of the analysis window rather than at today —
 * because the raw pass is a subset of the analysis window, and anchoring it at
 * now would put it outside a historical window entirely.
 *
 * Empty means no override, and the backend applies the bounded default.
 */
function rawWindowFor(analysis: AppliedWindow, days: string): { from: string; to: string } | undefined {
  const parsed = Number.parseFloat(days);
  if (!Number.isFinite(parsed) || parsed <= 0) return undefined;

  const end = analysis ? new Date(analysis.to) : new Date();
  if (Number.isNaN(end.getTime())) return undefined;
  const start = new Date(end.getTime() - parsed * 24 * 3600 * 1000);
  return { from: start.toISOString(), to: end.toISOString() };
}

// --- M1a: candidates ---

function CandidatesPane({
  selected,
  onSelect,
  analysisWindow,
}: {
  selected: QuickProfile | null;
  onSelect: (candidate: QuickProfile) => void;
  analysisWindow: AppliedWindow;
}) {
  const { params } = useLocation();

  /*
   * The applied query is the URL; the form is the draft above it.
   *
   * Restoring it costs nothing that was not going to be spent anyway: this listing
   * loads on mount whatever the filters say, and at tier L0 it reads no values at
   * all. So the honest thing is to load what the developer was actually looking at
   * rather than the defaults. The form is seeded once, at mount, and follows the
   * URL from there only through Apply — typing must not re-run a listing per
   * keystroke.
   */
  const applied = {
    search: params.get("q") ?? "",
    includeUnqueryable: params.get("unreadable") === "1",
    limit: deviceLimit(params.get("limit") ?? ""),
  };
  const [form, setForm] = useState(() => ({
    search: params.get("q") ?? "",
    from: params.get("from") ?? "",
    to: params.get("to") ?? "",
    includeUnqueryable: params.get("unreadable") === "1",
    limit: params.get("limit") ?? String(DEFAULT_DEVICE_LIMIT),
  }));

  // A half-filled range is refused rather than sent: the backend needs both ends,
  // and a 400 is a worse answer than a disabled button that says why.
  const halfWindow = (form.from === "") !== (form.to === "");
  const invertedWindow = form.from !== "" && form.to !== "" && form.to <= form.from;

  // Over the socket rather than HTTP, and with the signal threaded through: an
  // availability call per device means a listing takes as long as the devices
  // divided by the concurrency limit, and changing a filter mid-listing should
  // stop those reads rather than leave them running for a discarded result.
  const load = useCallback(
    (signal: AbortSignal) =>
      profilerSocket.request<QuickProfileList>(
        "quick_profiles",
        {
          search: applied.search || undefined,
          limit: applied.limit,
          window: analysisWindow ? { from: analysisWindow.from, to: analysisWindow.to } : undefined,
          include_unqueryable: applied.includeUnqueryable,
        },
        signal,
      ),
    // The fields rather than the object: `applied` is derived from the URL and is a
    // new object on every render, so depending on it would re-list on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [applied.search, applied.limit, applied.includeUnqueryable, analysisWindow],
  );
  const { data, error, loading } = useLoad(load);

  /*
   * A selection restored from the address.
   *
   * The candidate itself is a row of this listing — it carries the device, the
   * score and whether the series is readable at all — so it can only be handed up
   * once the listing has arrived. Matched by the same key the rows are keyed on,
   * which is what makes a restored selection and a clicked one the same object.
   */
  const restore = params.get("series");
  const match = restore && data ? data.candidates.find((c) => seriesKey(c) === restore) : undefined;
  useEffect(() => {
    if (selected === null && match) onSelect(match);
  }, [selected, match, onSelect]);

  return (
    <Pane
      title="Candidates"
      subtitle="Ranked from availability, volume, liveness and the ontology — tier L0, no value read"
      actions={<ConnectionState />}
    >
      <form
        className="filters flex flex-wrap items-center gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (halfWindow || invertedWindow) return;
          // Applied by writing the address, which is also what re-runs the listing:
          // the query below reads these back. Written with replace rather than push,
          // so twenty filter tweaks cost one press of the back button to escape, not
          // twenty. The clamped limit is written rather than the raw text, and a
          // value that is already the default is left out — an address should name
          // what was changed, not restate every default.
          const limit = deviceLimit(form.limit);
          setParam("q", form.search || null);
          setParam("unreadable", form.includeUnqueryable ? "1" : null);
          setParam("limit", limit === DEFAULT_DEVICE_LIMIT ? null : String(limit));

          /*
           * A profile is its window, so a changed window takes the profile with it.
           *
           * The pane on the right is keyed on the window and remounts, which is how
           * a profile that no longer applies is meant to be discarded — but the
           * remounted pane restores `?profile=` from the address, and the guard it
           * restores through cannot refuse the one case that matters. Compare a
           * stored two-day profile against a two-day window and the days match;
           * against a *cleared* window there is nothing to compare it to, so it is
           * let through, and a profile computed over two days reappears beside a
           * candidate list ranked over the whole availability range. Nothing on
           * screen says which of the two it is.
           *
           * The guard cannot be taught the null case either: a profile computed with
           * no window stores the concrete range the platform resolved, so "was this
           * computed over no window" is not a question its record can answer. What
           * can be answered here is whether the developer just changed the window,
           * and that is the question — so the parameter is dropped at the point the
           * change is made rather than guessed at afterwards.
           */
          const from = form.from || null;
          const to = form.to || null;
          // The live address rather than the render's snapshot, for the reason
          // setParam gives for reading it that way: three parameters have already
          // been written by the time this runs.
          const address = new URLSearchParams(window.location.search);
          if (from !== (address.get("from") || null) || to !== (address.get("to") || null)) {
            setParam("profile", null);
          }
          setParam("from", from);
          setParam("to", to);
        }}
      >
        <Input
          value={form.search}
          onChange={(e) => setForm({ ...form, search: e.target.value })}
          placeholder="Search devices"
          aria-label="Search devices"
        />
        <label
          className="filter-window"
          title="Ranks candidates by how much of this range they cover, and becomes the analysis window a profile is computed over"
        >
          <span>Window</span>
          <Input
            type="date"
            value={form.from}
            onChange={(e) => setForm({ ...form, from: e.target.value })}
            aria-label="Window start"
          />
          <Input
            type="date"
            value={form.to}
            onChange={(e) => setForm({ ...form, to: e.target.value })}
            aria-label="Window end"
          />
        </label>
        <label
          className="filter-check flex items-center gap-2 text-sm"
          title="Availability is one call per device and cannot be batched, so this is what decides how long a listing takes"
        >
          <span>Devices</span>
          <Input
            value={form.limit}
            onChange={(e) => setForm({ ...form, limit: e.target.value })}
            inputMode="numeric"
            aria-label="Device limit"
          />
        </label>
        <label className="filter-check flex items-center gap-2 text-sm">
          <Checkbox
            checked={form.includeUnqueryable}
            onCheckedChange={(checked) => setForm({ ...form, includeUnqueryable: checked })}
          />
          <span title="Variables that exist but cannot be read as a scalar series">
            include unreadable
          </span>
        </label>
        <Button variant="outline" type="submit" disabled={halfWindow || invertedWindow}>
          Apply
        </Button>
      </form>
      {halfWindow && <Muted>A window needs both a start and an end, or neither.</Muted>}
      {invertedWindow && <Muted>The window ends before it starts.</Muted>}
      {restore && data && !match && selected === null && (
        <Muted>
          The series named in the address is not in this listing. It is ranked out by the current
          filters, or the account no longer reaches that device.
        </Muted>
      )}

      {loading && <Busy>Reading metadata for {applied.limit} devices…</Busy>}
      {error && <Muted>{error}</Muted>}

      {data && (
        <>
          <ReadCounter reads={data.reads} />
          <Muted>
            {data.candidates.length} candidate series from {data.devices_listed} device
            {data.devices_listed === 1 ? "" : "s"}
            {data.total_devices > data.devices_listed && ` of ${data.total_devices} readable`}, over{" "}
            {date(data.coverage_window.from)} to {date(data.coverage_window.to)}
          </Muted>

          {data.candidates.length === 0 && (
            <Muted>
              No candidate series. Either no device grants execute permission to this account, or the
              devices it can reach declare no streamed variables.
            </Muted>
          )}

          {data.candidates.length > 0 && (
            <Table className="candidates">
              <TableHeader>
                <TableRow>
                  <TableHead>#</TableHead>
                  <TableHead>Device</TableHead>
                  <TableHead>Variable</TableHead>
                  <TableHead>Unit</TableHead>
                  <TableHead title="Days between the first and last available point">Span</TableHead>
                  <TableHead title="Share of the requested window the data actually spans">Cover</TableHead>
                  <TableHead title="Newest point within a day, and the device is not offline">Live</TableHead>
                  <TableHead title="0.3 span + 0.4 coverage + 0.3 liveness">Score</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.candidates.map((candidate, index) => (
                  <CandidateRow
                    key={seriesKey(candidate)}
                    candidate={candidate}
                    rank={index + 1}
                    selected={selected !== null && seriesKey(candidate) === seriesKey(selected)}
                    onSelect={() => onSelect(candidate)}
                  />
                ))}
              </TableBody>
            </Table>
          )}

          {data.skipped.length > 0 && (
            <Section title={`Skipped devices (${data.skipped.length})`} defaultOpen={false}>
              <ul className="list flex flex-col gap-1">
                {data.skipped.map((skip) => (
                  <li key={skip.device_id}>
                    <strong>{skip.name || shortId(skip.device_id)}</strong>
                    <span className="muted-inline text-xs text-muted-foreground"> — {skip.reason}</span>
                  </li>
                ))}
              </ul>
            </Section>
          )}
        </>
      )}
    </Pane>
  );
}

export function seriesKey(candidate: QuickProfile): string {
  const ref = candidate.series_ref;
  return `${ref.device_id}|${ref.service_id}|${ref.variable_path}`;
}

/**
 * ReadCounter is the tier-L0 claim on screen: ranking a candidate list must cost
 * no value read at all. The backend counts them, so this is the answer's own
 * claim rather than a promise about the code — and it turns red if it is ever
 * not zero.
 *
 * Semantic selection makes the same claim about a longer chain of calls, so it
 * reuses this and passes its own breakdown as `detail`.
 */
export function ReadCounter({ reads, detail }: { reads: ReadCounts; detail?: string }) {
  const clean = reads.values === 0;
  return (
    <div className={`reads ${clean ? "clean" : "dirty"}`}>
      <span className="reads-headline">
        {reads.values} value read{reads.values === 1 ? "" : "s"}
      </span>
      <span className="reads-detail">
        {detail ?? `${reads.availability} availability · ${reads.usage} usage`}
      </span>
      <span className="reads-note">
        {clean
          ? "Selection and triage complete at tier L0"
          : "A value was read while ranking — that breaks the tier"}
      </span>
    </div>
  );
}

/**
 * CandidateRow is shared with the selection view, where a resolved series is
 * shown but not selectable — hence the optional onSelect. One row component
 * rather than two, because the columns mean the same thing in both places and two
 * would drift.
 */
export function CandidateRow({
  candidate,
  rank,
  selected = false,
  onSelect,
}: {
  candidate: QuickProfile;
  rank: number;
  selected?: boolean;
  onSelect?: () => void;
}) {
  const hints = candidate.rank_hints;
  return (
    <TableRow
      className={`${selected ? "selected" : ""} ${candidate.queryable ? "" : "unreadable"}`}
      onClick={onSelect}
      title={candidate.queryable ? undefined : candidate.reason}
    >
      <TableCell className="numeric text-right tabular-nums">{rank}</TableCell>
      <TableCell title={candidate.series_ref.device_id}>
        <DeviceLabel device={candidate.device} fallbackId={candidate.series_ref.device_id} />
      </TableCell>
      <TableCell>
        <code>{candidate.series_ref.variable_path}</code>
        {!candidate.queryable && <span className="tag warn inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs text-foreground">unreadable</span>}
        {candidate.ontology_completeness.status === "partial" && (
          <span className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs" title={candidate.ontology_completeness.missing.join(", ")}>
            partial
          </span>
        )}
      </TableCell>
      <TableCell>{candidate.declared.unit || <span className="muted-inline text-xs text-muted-foreground">unknown</span>}</TableCell>
      <TableCell className="numeric text-right tabular-nums">{round(hints.span_days, 0)} d</TableCell>
      <TableCell className="numeric text-right tabular-nums">{percent(hints.coverage_proxy)}</TableCell>
      <TableCell>
        <span className={`state ${hints.is_live ? "online" : "unknown"}`}>
          {hints.is_live ? "live" : "stale"}
        </span>
      </TableCell>
      <TableCell className="numeric text-right tabular-nums">
        <ScoreBar score={hints.score} />
      </TableCell>
    </TableRow>
  );
}

/**
 * deviceName is the one rule for naming a device on screen: the platform's display
 * name, and the shortened id only when there is nothing else.
 *
 * `device` is required in the API type and the contract check proves a current
 * backend sends it, so the undefined case is not supposed to happen. It is admitted
 * anyway because it did happen — a frontend hot-reloading against a backend older
 * than the field crashed the whole candidate table on `device.name`. Degrading to
 * the id is the same trade Val makes for a null Value in ui.tsx: one line, and a
 * shape surprise stays a cosmetic loss rather than a blank pane.
 */
export function deviceName(device: DeviceInfo | undefined, fallbackId: string): string {
  return device?.name || shortId(fallbackId);
}

/** The device type, named where the platform named it and shortened where not. */
export function deviceTypeLabel(device: DeviceInfo | undefined): string {
  if (!device) return "";
  return device.device_type_name || (device.device_type_id ? shortId(device.device_type_id) : "");
}

/**
 * DeviceLabel names a device with its device type beside it — "Kitchen Meter" tells
 * a developer nothing when three of them exist, and "SmartMeter Modbus" is what
 * separates them.
 *
 * The id stays in the row's tooltip either way, because that is what a bug report
 * needs to quote.
 */
export function DeviceLabel({
  device,
  fallbackId,
}: {
  device: DeviceInfo | undefined;
  fallbackId: string;
}) {
  const type = deviceTypeLabel(device);
  return (
    <>
      <span>{deviceName(device, fallbackId)}</span>
      {type && (
        <span className="device-type" title={device?.device_type_id}>
          {type}
        </span>
      )}
    </>
  );
}

export function ScoreBar({ score }: { score: number }) {
  const width = Math.min(100, Math.max(0, score * 100));
  return (
    <span className="score" title={String(round(score, 3))}>
      <span className="score-fill" style={{ width: `${width}%` }} />
      <span className="score-text">{round(score, 2)}</span>
    </span>
  );
}

// --- the right-hand pane ---

type Tab = "quick" | "full" | "llm" | "sessions";

const TABS: [Tab, string][] = [
  ["quick", "Quick"],
  ["full", "Full profile"],
  ["llm", "LLM view"],
  ["sessions", "Sessions"],
];

function ProfilePane({
  candidate,
  analysisWindow,
  onOpenChart,
}: {
  candidate: QuickProfile | null;
  analysisWindow: AppliedWindow;
  onOpenChart?: (chartId: string) => void;
}) {
  const restoreId = useParam("profile");
  const tabParam = useParam("tab");
  // A restored profile opens on the profile rather than on the quick view: landing
  // on the tab that does not show it would read as the restore having failed.
  const [tab, setTab] = useState<Tab>(() => {
    const named = TABS.find(([id]) => id === tabParam)?.[0];
    return named ?? (restoreId ? "full" : "quick");
  });
  const [computed, setComputed] = useState<ProfileResult | null>(null);
  const [viewing, setViewing] = useState<string | null>(null);
  const [rawDays, setRawDays] = useState("");
  const [restoreError, setRestoreError] = useState<string | null>(null);

  const showTab = useCallback((next: Tab) => {
    setTab(next);
    // Quick is where the pane opens, so it is written as an absent parameter.
    setParam("tab", next === "quick" ? null : next);
  }, []);

  // Selecting a sibling of the same service deliberately does not remount this
  // pane — the computed batch covers every variable of the service, so switching
  // between them should cost nothing. But the profile on show is chosen by
  // `viewing`, and without this the header would name the newly selected variable
  // while the body still rendered the previous one. Wrong data under the right
  // label is the one failure this whole screen exists to avoid.
  const selectedPath = candidate?.series_ref.variable_path ?? null;
  useEffect(() => {
    if (selectedPath !== null) setViewing(selectedPath);
  }, [selectedPath]);

  const compute = useAction(
    useCallback(
      (signal: AbortSignal, deviceId: string, serviceId: string, path: string) =>
        profilerSocket
          .request<ProfileResult>(
            "profile",
            {
            device_id: deviceId,
            service_id: serviceId,
            // The window the developer set is the analysis window. Omitting it —
            // which is what this did before — silently reads the whole
            // availability range instead of the range they asked for.
            analysis_window: analysisWindow ?? undefined,
            // The raw pass defaults to the smaller of fourteen days or the point
            // limit (D25). An explicit override is recorded in the profile as
            // developer_override, so a profile read over an unusual window is not
            // mistaken for a default one.
            raw_window: rawWindowFor(analysisWindow, rawDays),
            },
            signal,
          )
          .then((result) => {
            setComputed(result);
            setViewing(path);
            // Recorded so a reload comes back to this profile by id rather than by
            // reading the platform again. The batch covers every variable of the
            // service (D19); the one named is the one that was asked for.
            const asked = result.profiles.find((p) => p.series_ref.variable_path === path);
            if (asked) setParam("profile", asked.profile_id);
            return result;
          }),
      [analysisWindow, rawDays],
    ),
  );

  // Refetching one profile after a confirmation is what makes the overlay
  // visible: the stored body never changes, so only the resolution map does.
  const refresh = useCallback(async (profileId: string) => {
    const updated = await api.profile(profileId);
    setComputed((previous) =>
      previous
        ? {
            ...previous,
            profiles: previous.profiles.map((p) => (p.profile_id === profileId ? updated : p)),
          }
        : previous,
    );
  }, []);

  // The chart is built from the profile on screen, so its window is the window the
  // detections were computed over — a chart whose bands all fell outside it would
  // be worse than no chart.
  const chart = useAction(
    useCallback(
      async (_signal: AbortSignal, viewed: SeriesProfile) => {
        const chartId = await chartFromProfile({
          deviceID: viewed.series_ref.device_id,
          serviceID: viewed.series_ref.service_id,
          variablePath: viewed.series_ref.variable_path,
          profileID: viewed.profile_id,
          label: viewed.series_ref.variable_path,
          window: { from: viewed.analysis_window.from, to: viewed.analysis_window.to },
        });
        onOpenChart?.(chartId);
        return chartId;
      },
      [onOpenChart],
    ),
  );

  /*
   * A profile restored from the address.
   *
   * Profiles are immutable and stored (D21), so this is a fetch by id — not the two
   * reads that produced it. That is the whole reason the parameter is a profile id
   * rather than a set of compute inputs: a reload is not consent to spend a
   * developer's value reads a second time.
   *
   * What comes back is one profile rather than the service-scoped batch it was
   * computed in, so the header is assembled from it: no value reads, because none
   * were made, and one entry in from_cache, because that is exactly what happened.
   * The sibling variables of the batch are not offered, because they were not
   * fetched — and switching to one is a within-batch move the address does not
   * describe, so a reload comes back to the profile it names.
   */
  useEffect(() => {
    if (!restoreId || computed || !candidate) return;
    let cancelled = false;
    setRestoreError(null);
    api
      .profile(restoreId)
      .then((stored) => {
        if (cancelled) return;
        if (!profileMatches(stored, candidate, analysisWindow)) {
          // The address pairs a profile with a series or a window it was not
          // computed for — a hand-edited URL, or a parameter left behind by a
          // selection that has since moved. Wrong data under the right label is the
          // one failure this screen exists to avoid, so the parameter is dropped
          // rather than the profile shown.
          setParam("profile", null);
          return;
        }
        setComputed({
          profiles: [stored],
          reads: { availability: 0, usage: 0, values: 0 },
          from_cache: [stored.profile_id],
          analysis_window: stored.analysis_window,
          raw_window: stored.raw_window,
          // Nothing was grouped and nothing was read, so there is no group time to
          // report. Empty, which the header leaves out rather than inventing.
          group_time: "",
        });
      })
      .catch((e: unknown) => {
        if (!cancelled) setRestoreError(describe(e));
      });
    return () => {
      cancelled = true;
    };
  }, [restoreId, computed, candidate, analysisWindow]);

  if (!candidate) {
    return (
      <Pane title="Profile" subtitle="Pick a candidate on the left">
        <Muted>
          Nothing selected. The candidate list is assembled from metadata alone, so choosing from it
          costs nothing; computing a full profile is the first thing that reads values.
        </Muted>
      </Pane>
    );
  }

  const profile = computed?.profiles.find((p) => p.series_ref.variable_path === viewing) ?? null;

  // Through the same two helpers the candidate rows use, so the header and the row
  // a developer clicked cannot disagree about what the device is called.
  const named = deviceName(candidate.device, candidate.series_ref.device_id);
  const type = deviceTypeLabel(candidate.device);

  return (
    <Pane
      title={candidate.series_ref.variable_path}
      subtitle={`${named}${type ? ` (${type})` : ""} · service ${shortId(
        candidate.series_ref.service_id,
      )} · interaction ${candidate.interaction}`}
      actions={
        <>
          {profile && onOpenChart && (
            <Button variant="outline"
              className={chart.pending ? "busy animate-pulse" : undefined}
              disabled={chart.pending}
              title="Draws this profile in the exploration pane, with its detected sessions, gaps and advised ranges as confirmable annotations"
              onClick={() => void chart.invoke(profile)}
            >
              {chart.pending ? "Charting…" : "Chart it"}
            </Button>
          )}
          <div className="tabs inline-flex items-center gap-1">
            {TABS.map(([id, label]) => (
              <Button variant="outline" key={id} className={tab === id ? "active" : ""} onClick={() => showTab(id)}>
                {label}
              </Button>
            ))}
          </div>
        </>
      }
    >
      {tab === "quick" && <QuickProfileDetail candidate={candidate} />}

      {tab !== "quick" && !profile && (
        <div className="compute">
          <p>
            The full profile reads values: one bounded raw pass for the structural detectors and one
            aggregated pass over the analysis window for the statistical ones. Every variable of the
            service is profiled from those same two reads.
          </p>
          {/*
            What is about to be read, before it is read. The window came from the
            filter on the left, and seeing it here is what makes the difference
            between "the range I picked" and "everything" visible in advance
            rather than afterwards in the header.
          */}
          <KV>
            <Row label="Analysis window" hint="Read aggregated. The range the statistical detectors see">
              {analysisWindow ? (
                <>
                  {date(analysisWindow.from)} to {date(analysisWindow.to)}
                  <span className="muted-inline text-xs text-muted-foreground">
                    {" "}
                    · {round(spanDays(analysisWindow), 1)} days, from the filter on the left
                  </span>
                </>
              ) : (
                <span className="muted-inline text-xs text-muted-foreground">
                  everything the platform has — set a window on the left to narrow it
                </span>
              )}
            </Row>
            <Row
              label="Raw window"
              hint="Read unbucketed for the structural detectors, and bounded because it is the expensive one"
            >
              <span className="raw-override">
                <Input
                  value={rawDays}
                  onChange={(e) => setRawDays(e.target.value)}
                  placeholder="14"
                  inputMode="decimal"
                  aria-label="Raw window in days"
                />
                <span className="muted-inline text-xs text-muted-foreground">
                  days back from the window end
                  {rawDays.trim() === "" ? ", default" : ", recorded as a developer override"}
                </span>
              </span>
            </Row>
          </KV>
          <div className="compute-actions">
            <Button variant="default"
              className={compute.pending ? "primary busy animate-pulse" : "primary"}
              disabled={compute.pending || !candidate.queryable}
              onClick={() =>
                void compute.invoke(
                  candidate.series_ref.device_id,
                  candidate.series_ref.service_id,
                  candidate.series_ref.variable_path,
                )
              }
            >
              {compute.pending ? "Reading…" : "Compute profile"}
            </Button>
            {compute.pending && (
              <Button variant="outline"
                onClick={compute.abort}
                title="Stops the platform reads, not just the waiting"
              >
                Cancel
              </Button>
            )}
          </div>
          {!candidate.queryable && <Muted>{candidate.reason}</Muted>}
          {compute.error && <Muted>{compute.error}</Muted>}
          {restoreError && (
            <Muted>
              The profile named in the address could not be read: {restoreError}. Computing it
              again would cost the same two reads, so it is left to the button.
            </Muted>
          )}
        </div>
      )}

      {profile && computed && (
        <>
          <ComputedHeader
            result={computed}
            viewing={profile.series_ref.variable_path}
            onView={setViewing}
          />
          {tab === "full" && <FullProfile profile={profile} onChanged={refresh} />}
          {tab === "llm" && <ProjectionTab profileId={profile.profile_id} />}
          {tab === "sessions" && <SessionsTab profileId={profile.profile_id} />}
        </>
      )}
    </Pane>
  );
}

/**
 * profileMatches guards a restore against an address that names the wrong pair.
 *
 * The series is compared in full, down to the variable, because the pane's title
 * comes from the candidate and its body from the profile: a profile of a sibling
 * variable would be rendered under another variable's name. The window is compared
 * to the day, which is the resolution the filter above offers — a profile *is* its
 * window, and one computed over a range the developer has since changed is the
 * same mislabelling in a different guise.
 */
function profileMatches(
  profile: SeriesProfile,
  candidate: QuickProfile,
  window: AppliedWindow,
): boolean {
  const stored = profile.series_ref;
  const wanted = candidate.series_ref;
  if (
    stored.device_id !== wanted.device_id ||
    stored.service_id !== wanted.service_id ||
    stored.variable_path !== wanted.variable_path
  ) {
    return false;
  }
  if (!window) return true;
  return (
    profile.analysis_window.from.slice(0, 10) === window.from.slice(0, 10) &&
    profile.analysis_window.to.slice(0, 10) === window.to.slice(0, 10)
  );
}

/**
 * ComputedHeader shows what the read actually cost and which sibling variables
 * came out of it — the service-scoped batch of D19 is easier to believe when the
 * siblings are switchable without another read.
 */
function ComputedHeader({
  result,
  viewing,
  onView,
}: {
  result: ProfileResult;
  viewing: string;
  onView: (path: string) => void;
}) {
  return (
    <div className="computed-head">
      <div className="computed-reads">
        <span>
          {result.reads.values} value read{result.reads.values === 1 ? "" : "s"} for{" "}
          {result.profiles.length} profile{result.profiles.length === 1 ? "" : "s"}
        </span>
        <span className="muted-inline text-xs text-muted-foreground">
          {date(result.analysis_window.from)} to {date(result.analysis_window.to)}
          {result.group_time && ` at ${result.group_time}`} · raw {date(result.raw_window.from)} to{" "}
          {date(result.raw_window.to)}
          {result.raw_window.source === "developer_override" && " (override)"}
          {result.raw_window.truncated && " (truncated by the row limit)"}
          {result.raw_window.row_limit !== undefined && (
            <span className="muted-inline text-xs text-muted-foreground"> · {result.raw_window.row_limit.toLocaleString("en-GB")} rows
              {" "}max, the point bound over the variables read</span>
          )}
          {result.raw_window.limit_reduced && (
            <span className="warn text-foreground">
              {" "}· halved after the gateway refused the first read
            </span>
          )}
        </span>
        {result.from_cache.length > 0 && (
          <span className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs" title={result.from_cache.join(", ")}>
            {result.from_cache.length} from cache
          </span>
        )}
      </div>
      {result.profiles.length > 1 && (
        <div className="tabs small inline-flex items-center gap-1 text-xs">
          {result.profiles.map((p) => (
            <Button variant="outline"
              key={p.profile_id}
              className={p.series_ref.variable_path === viewing ? "active" : ""}
              onClick={() => onView(p.series_ref.variable_path)}
            >
              {p.series_ref.variable_path}
            </Button>
          ))}
        </div>
      )}
    </div>
  );
}

// --- M1a detail ---

function QuickProfileDetail({ candidate }: { candidate: QuickProfile }) {
  return (
    <>
      <Section title="Availability" note="from /data-availability">
        <KV>
          <Field
            label="Window"
            value={candidate.availability}
            render={(a) => (
              <>
                {date(a.from)} to {date(a.to)} · {round(a.span_days, 0)} days
                {!a.raw_available && (
                  <span className="tag warn inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs text-foreground" title="Retention has aged the raw data out">
                    aggregated only
                  </span>
                )}
              </>
            )}
          />
          <Field
            label="Materialised aggregates"
            hint="Pre-aggregated variants that already exist, which make the aggregated pass cheap"
            value={candidate.availability}
            render={(a) =>
              a.aggregates.length === 0 ? (
                <span className="muted-inline text-xs text-muted-foreground">none</span>
              ) : (
                a.aggregates.map((aggregate, index) => (
                  <span key={`${aggregate.group_time}-${aggregate.group_type}-${index}`} className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">
                    {aggregate.group_time} {aggregate.group_type}
                  </span>
                ))
              )
            }
          />
        </KV>
      </Section>

      <Section title="Volume" note="from /usage/devices — cost before any read">
        <KV>
          <Field
            label="Stored"
            value={candidate.volume}
            render={(v) => `${bytes(v.bytes)} · ${bytes(v.bytes_per_day)}/day`}
          />
          <Field
            label="Estimated interval"
            hint="Order of magnitude only, and never used for a resampling decision"
            value={candidate.volume}
            render={(v) => (
              <>
                <Val value={v.estimated_interval_s} render={(s) => seconds(s)} />{" "}
                <ConfidenceTag confidence={v.confidence} />
                <span className="muted-inline text-xs text-muted-foreground"> basis: {v.estimate_basis}</span>
              </>
            )}
          />
        </KV>
      </Section>

      <Section title="Declared" note="from the ontology, no read">
        <KV>
          <Row label="Unit" hint="Derived from the characteristic and advisory">
            {candidate.declared.unit || <span className="muted-inline text-xs text-muted-foreground">unknown</span>}
            <span className="muted-inline text-xs text-muted-foreground"> ({candidate.declared.unit_source})</span>
          </Row>
          <Row label="Characteristic" hint="Canonical and authoritative; never fabricated">
            {candidate.declared.characteristic_id ? (
              <code>{shortId(candidate.declared.characteristic_id)}</code>
            ) : (
              <span className="muted-inline text-xs text-muted-foreground">null — none declared</span>
            )}
          </Row>
          <Field label="Minimum" value={candidate.declared.min_value} render={(v) => num(v)} />
          <Field label="Maximum" value={candidate.declared.max_value} render={(v) => num(v)} />
          <Row label="Type">
            <code>{shortId(candidate.declared.type)}</code>
          </Row>
        </KV>
      </Section>

      <Section title="Liveness">
        <KV>
          <Row label="Connection">
            <span className={`state ${candidate.liveness.connection_state || "unknown"}`}>
              {candidate.liveness.connection_state || "unknown"}
            </span>
          </Row>
          <Field
            label="Newest point"
            hint="Taken from the availability window's end, not from /last-values, which returns actual values"
            value={candidate.liveness.last_value_age_s}
            render={(age) => `${seconds(age)} old`}
          />
        </KV>
      </Section>

      <Section
        title="Ontology completeness"
        note="discovered per variable at runtime, never assumed"
      >
        <KV>
          <Row label="Status">
            <span className={candidate.ontology_completeness.status === "complete" ? "ok text-foreground" : "warn text-foreground"}>
              {candidate.ontology_completeness.status}
            </span>
          </Row>
          <Row label="Missing">
            {candidate.ontology_completeness.missing.length === 0 ? (
              <span className="muted-inline text-xs text-muted-foreground">nothing</span>
            ) : (
              candidate.ontology_completeness.missing.map((field) => (
                <span key={field} className="tag warn inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs text-foreground">
                  {field}
                </span>
              ))
            )}
          </Row>
          {candidate.ontology_completeness.consequence && (
            <Row label="Consequence">{candidate.ontology_completeness.consequence}</Row>
          )}
        </KV>
      </Section>

      <ProvenanceSection provenance={candidate.provenance} />
    </>
  );
}

// --- M1b detail ---

function FullProfile({
  profile,
  onChanged,
}: {
  profile: SeriesProfile;
  onChanged: (profileId: string) => Promise<void>;
}) {
  const overridden = Object.keys(profile.resolution ?? {});

  return (
    <>
      <ReadDiagnosis summary={profile.read_summary} />
      {profile.quality_flags.length > 0 && <QualityFlags flags={profile.quality_flags} />}

      <Section title="Coverage and sampling" note="raw pass">
        <KV>
          <Field
            label="Coverage"
            hint="Points read against the points the detected interval implies, over the raw window"
            value={profile.coverage}
            render={(c) => (
              <>
                {c.n_points} of {c.expected_points} expected ·{" "}
                <strong>{percent(c.completeness_ratio)}</strong>
              </>
            )}
          />
          <Field
            label="Interval"
            value={profile.sampling}
            render={(s) => (
              <>
                {seconds(s.detected_interval_s)} · {s.regularity}{" "}
                <ConfidenceTag confidence={s.confidence} />
                <span className="muted-inline text-xs text-muted-foreground"> irregularity {percent(s.irregularity_ratio)}</span>
              </>
            )}
          />
          <Field
            label="Gaps"
            value={profile.sampling}
            render={(s) =>
              s.gaps.length === 0 ? (
                <span className="ok text-foreground">none</span>
              ) : (
                <>
                  {s.gaps.length} · longest {seconds(Math.max(...s.gaps.map((g) => g.duration_s)))}
                </>
              )
            }
          />
        </KV>
        <Val value={profile.sampling} render={(s) => <GapTable gaps={s.gaps} />} />
      </Section>

      <Section title="Value semantics" note="the detector whose misreading is silent">
        <KV>
          <Field
            label="Kind"
            value={profile.value_semantics.kind}
            render={(kind) => (
              <>
                <strong>{kind.replace(/_/g, " ")}</strong>{" "}
                <Val
                  value={profile.value_semantics.kind_confidence}
                  render={(c) => <ConfidenceTag confidence={c} />}
                />
                {overridden.includes("value_semantics.kind") && (
                  <span className="tag ok-tag inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">confirmed</span>
                )}
              </>
            )}
          />
          <Field
            label="Evidence"
            hint="A verdict without its evidence is a number to over-trust"
            value={profile.value_semantics.kind_evidence}
            render={(e) => (
              <span className="evidence text-xs text-muted-foreground">
                monotonic {percent(e.monotonic_ratio)} · {e.distinct_values} distinct ·{" "}
                {e.negative_deltas} negative steps · non-numeric {percent(e.non_numeric_ratio)}
              </span>
            )}
          />
          <Row label="Unit">
            {profile.value_semantics.unit || <span className="muted-inline text-xs text-muted-foreground">unknown</span>}
            <span className="muted-inline text-xs text-muted-foreground"> ({profile.value_semantics.unit_source})</span>
            {overridden.includes("value_semantics.unit") && (
              <span className="tag ok-tag inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">confirmed</span>
            )}
          </Row>
          <Row label="Characteristic">
            {profile.value_semantics.characteristic_id ? (
              <code>{shortId(profile.value_semantics.characteristic_id)}</code>
            ) : (
              <span className="muted-inline text-xs text-muted-foreground">null</span>
            )}
          </Row>
          <Field
            label="Range violations"
            value={profile.value_semantics.range_violation_ratio}
            render={(ratio) => <span className={ratio > 0 ? "warn text-foreground" : "ok text-foreground"}>{percent(ratio)}</span>}
          />
          <Field
            label="Counter resets"
            value={profile.value_semantics.counter_resets}
            render={(resets) =>
              resets.length === 0 ? (
                <span className="ok text-foreground">none</span>
              ) : (
                <>
                  {resets.length} ·{" "}
                  <span className="muted-inline text-xs text-muted-foreground">
                    {resets.slice(0, 3).map(dateTime).join(", ")}
                    {resets.length > 3 && " …"}
                  </span>
                </>
              )
            }
          />
          <Row label="Conversions" hint="Evaluated server-side; ODE only selects a target">
            {profile.value_semantics.available_conversions.length === 0 ? (
              <span className="muted-inline text-xs text-muted-foreground">none</span>
            ) : (
              profile.value_semantics.available_conversions.map((conversion) => (
                <span key={conversion.to_characteristic_id} className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">
                  {conversion.to_unit || shortId(conversion.to_characteristic_id)} (d
                  {conversion.distance})
                </span>
              ))
            )}
          </Row>
        </KV>
      </Section>

      <Section title="Distribution" note="aggregated pass; min and max from min/max buckets">
        <KV>
          <Field
            label="Statistics"
            hint="The percentiles are percentiles of bucket means, which pulls them towards the centre"
            value={profile.distribution}
            render={(d) => (
              <div className="stats">
                <Stat label="min" value={num(d.min)} />
                <Stat label="p01" value={num(d.p01)} />
                <Stat label="median" value={num(d.median)} />
                <Stat label="mean" value={num(d.mean)} />
                <Stat label="p99" value={num(d.p99)} />
                <Stat label="max" value={num(d.max)} />
                <Stat label="sd" value={num(d.std_dev)} />
                <Stat label="zero" value={percent(d.zero_ratio)} />
                <Stat label="flat runs" value={String(d.constant_runs.length)} />
              </div>
            )}
          />
        </KV>
      </Section>

      <Section title="Temporal structure" note="aggregated pass">
        <KV>
          <Field
            label="Dominant periods"
            hint="An empty list means the detector ran and found none — not that it could not look"
            value={profile.temporal_structure.dominant_periods_s}
            render={(periods) =>
              periods.length === 0 ? (
                <span className="muted-inline text-xs text-muted-foreground">none detected</span>
              ) : (
                periods.map((p) => (
                  <span key={p} className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">
                    {period(p)}
                  </span>
                ))
              )
            }
          />
          <Field
            label="Evidence"
            value={profile.temporal_structure.period_evidence}
            render={(entries) =>
              entries.length === 0 ? (
                <span className="muted-inline text-xs text-muted-foreground">none</span>
              ) : (
                <span className="evidence text-xs text-muted-foreground">
                  {entries
                    .map((e) => `${e.label || seconds(e.period_s)} ${e.method} ${round(e.strength, 2)}`)
                    .join(" · ")}
                </span>
              )
            }
          />
          <Field
            label="Trend"
            value={profile.temporal_structure.trend}
            render={(trend) => (
              <>
                {num(trend.slope_per_day)} per day ·{" "}
                <span className={trend.significant ? "warn text-foreground" : "muted-inline text-xs text-muted-foreground"}>
                  {trend.significant ? "significant" : "not significant"}
                </span>
                <span className="muted-inline text-xs text-muted-foreground">
                  {" "}
                  t {round(trend.t_stat, 2)} · r² {round(trend.r2, 2)}
                </span>
              </>
            )}
          />
          <Field
            label="Stationarity"
            hint="Augmented Dickey-Fuller with a constant, against asymptotic critical values"
            value={profile.temporal_structure.stationarity}
            render={(adf) => (
              <>
                <strong>{adf.stationary ? "stationary" : "unit root not rejected"}</strong>{" "}
                <ConfidenceTag confidence={adf.confidence} />
                <div className="evidence text-xs text-muted-foreground">
                  ADF {round(adf.adf_stat, 2)} · lag {adf.lag_order} · n {adf.n_obs} · p between{" "}
                  {adf.p_value_bracket.lower} and {adf.p_value_bracket.upper}
                  <div className="muted-inline text-xs text-muted-foreground">{adf.p_value_bracket.note}</div>
                  <div className="muted-inline text-xs text-muted-foreground">
                    critical{" "}
                    {Object.entries(adf.critical_values)
                      .sort(([a], [b]) => a.localeCompare(b))
                      .map(([level, value]) => `${level} ${round(value, 3)}`)
                      .join(" · ")}
                  </div>
                </div>
              </>
            )}
          />
        </KV>
      </Section>

      <Section title="Activity" note="raw pass; sessions live behind their own resource">
        <KV>
          <Field
            label="Classification"
            value={profile.activity_pattern}
            render={(activity) => (
              <>
                <strong>{activity.classification.replace(/_/g, " ")}</strong>{" "}
                <ConfidenceTag confidence={activity.classification_confidence} />
                {overridden.includes("activity_pattern.classification") && (
                  <span className="tag ok-tag inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">confirmed</span>
                )}
              </>
            )}
          />
          <Field
            label="Idle / threshold"
            hint="The split the session detector used, and which method found it"
            value={profile.activity_pattern}
            render={(activity) => (
              <>
                {num(activity.idle_level)} / {num(activity.active_threshold)}
                <span className="muted-inline text-xs text-muted-foreground"> by {activity.threshold_method}</span>
              </>
            )}
          />
          <Field
            label="Parameters"
            hint="Developer-adjustable, so they travel in the profile"
            value={profile.activity_pattern}
            render={(activity) => (
              <span className="evidence text-xs text-muted-foreground">
                min duration {seconds(activity.threshold_params.min_duration_s)} · merge gap{" "}
                {seconds(activity.threshold_params.merge_gap_s)} · hysteresis{" "}
                {percent(activity.threshold_params.hysteresis_frac)}
              </span>
            )}
          />
          <Field
            label="Sessions"
            value={profile.activity_pattern}
            render={(activity) => (
              <Val
                value={activity.session_stats}
                render={(stats) =>
                  stats.count === 0 ? (
                    <span className="muted-inline text-xs text-muted-foreground">none detected</span>
                  ) : (
                    <>
                      {stats.count} · median {seconds(stats.median_duration_s)} · every{" "}
                      {seconds(stats.inter_arrival_median_s)} · median energy{" "}
                      {num(stats.median_energy)}
                    </>
                  )
                }
              />
            )}
          />
        </KV>
      </Section>

      <Section
        title="Service context"
        note="what the batched read reveals that no single variable does"
      >
        <KV>
          <Row label="Siblings">
            {profile.service_context.sibling_variables.length === 0 ? (
              <span className="muted-inline text-xs text-muted-foreground">none</span>
            ) : (
              profile.service_context.sibling_variables.map((sibling) => (
                <span key={sibling.path} className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">
                  {sibling.path}
                  {sibling.kind && ` · ${sibling.kind.replace(/_/g, " ")}`}
                </span>
              ))
            )}
          </Row>
        </KV>
        {profile.service_context.relationships.length === 0 ? (
          <Muted>No cross-variable relationship was established.</Muted>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Relationship</TableHead>
                <TableHead>With</TableHead>
                <TableHead title="Correlation of the paired increments">r</TableHead>
                <TableHead title="Residual after the best-fit scale">Residual</TableHead>
                <TableHead title="The factor mapping the other series onto this one — a unit error shows up here">
                  Scale
                </TableHead>
                <TableHead>Confidence</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {profile.service_context.relationships.map((relationship) => (
                <TableRow key={`${relationship.type}-${relationship.other_path}`}>
                  <TableCell>
                    <span
                      className={`tag ${relationship.type === "inconsistent_with" ? "warn text-foreground" : ""}`}
                    >
                      {relationship.type.replace(/_/g, " ")}
                    </span>
                  </TableCell>
                  <TableCell>
                    <code>{relationship.other_path}</code>
                  </TableCell>
                  <TableCell className="numeric text-right tabular-nums">{round(relationship.evidence.correlation, 2)}</TableCell>
                  <TableCell className="numeric text-right tabular-nums">{round(relationship.evidence.residual_ratio, 2)}</TableCell>
                  <TableCell className="numeric text-right tabular-nums">{num(relationship.evidence.implied_scale)}</TableCell>
                  <TableCell>
                    <ConfidenceTag confidence={relationship.confidence} />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>

      <Section title="Recommendations" note="advisory only — binding on promotion, never before">
        <KV>
          <Field
            label="Resample to"
            value={profile.recommendations.resample_to_s}
            render={(s) => seconds(s)}
          />
          <Field
            label="Interpolation"
            value={profile.recommendations.interpolation_strategy}
            render={(strategy) => strategy}
          />
          <Field
            label="Usable range"
            value={profile.recommendations.usable_range}
            render={(window) => `${date(window.from)} to ${date(window.to)}`}
          />
          <Row label="Exclusions">
            {profile.recommendations.exclusions.length === 0 ? (
              <span className="muted-inline text-xs text-muted-foreground">none</span>
            ) : (
              <ul className="list tight flex flex-col gap-1 leading-tight">
                {profile.recommendations.exclusions.slice(0, 8).map((exclusion) => (
                  <li key={`${exclusion.from}-${exclusion.to}`}>
                    {date(exclusion.from)} to {date(exclusion.to)}
                    <span className="muted-inline text-xs text-muted-foreground"> — {exclusion.reason}</span>
                  </li>
                ))}
              </ul>
            )}
          </Row>
        </KV>
      </Section>

      <OverridesSection profile={profile} onChanged={onChanged} />
      <ProvenanceSection provenance={profile.provenance} />

      <Section title="Identity" defaultOpen={false}>
        <KV>
          <Row label="Profile id">
            <code className="wrap">{profile.profile_id}</code>
          </Row>
          <Row
            label="Detector version"
            hint="Part of the cache key, so improving a detector cannot leave a stale profile behind"
          >
            {profile.detector_version}
          </Row>
          <Row label="Computed at">{dateTime(profile.computed_at)}</Row>
        </KV>
      </Section>
    </>
  );
}

function GapTable({ gaps }: { gaps: SeriesProfileGap[] }) {
  if (gaps.length === 0) return null;
  const largest = [...gaps].sort((a, b) => b.duration_s - a.duration_s).slice(0, 10);
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>From</TableHead>
          <TableHead>To</TableHead>
          <TableHead>Duration</TableHead>
          <TableHead title="Unknown is the honest answer, and is not the same as 'fine'">Classification</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {largest.map((gap) => (
          <TableRow key={`${gap.from}-${gap.to}`}>
            <TableCell>{dateTime(gap.from)}</TableCell>
            <TableCell>{dateTime(gap.to)}</TableCell>
            <TableCell className="numeric text-right tabular-nums">{seconds(gap.duration_s)}</TableCell>
            <TableCell>
              <span className={`tag ${gap.classification === "unknown" ? "" : "warn text-foreground"}`}>
                {gap.classification.replace(/_/g, " ")}
              </span>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

type SeriesProfileGap = { from: string; to: string; duration_s: number; classification: string };

/**
 * ReadDiagnosis is what an empty profile needs most: what came back, and why
 * that leaves the fields below saying "not computed".
 *
 * Always shown, because the counts are worth seeing on a healthy profile too —
 * and because a block that only appears on failure teaches people to read its
 * absence as success rather than as a working read.
 */
function ReadDiagnosis({ summary }: { summary: ReadSummary }) {
  const healthy = summary.diagnosis === undefined || summary.diagnosis === "";
  return (
    <div className={`reads ${healthy ? "clean" : "dirty"}`}>
      <span className="reads-headline">
        {summary.raw_rows} raw row{summary.raw_rows === 1 ? "" : "s"}
      </span>
      <span className="reads-detail">
        {summary.values_present} with a value here · {summary.null_rows} null ·{" "}
        {summary.aggregated_buckets} aggregated bucket
        {summary.aggregated_buckets === 1 ? "" : "s"}
        {isNotComputed(summary.raw_available)
          ? " · whether a raw window exists is unknown: the availability probe failed"
          : summary.raw_available
            ? ""
            : " · no raw window on the platform"}
      </span>
      {!healthy && <span className="reads-diagnosis">{summary.diagnosis}</span>}
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <span className="stat">
      <span className="stat-label">{label}</span>
      <span className="stat-value">{value}</span>
    </span>
  );
}

function QualityFlags({ flags }: { flags: QualityFlag[] }) {
  return (
    <div className="flags">
      {/* The flag name is not unique: inconsistent_with_sibling is emitted once
          per disagreeing sibling, so the index is part of the identity. */}
      {flags.map((flag, index) => (
        <details key={`${flag.flag}-${index}`} className="flag">
          <summary>
            <span className="flag-name">{flag.flag.replace(/_/g, " ")}</span>
            <ConfidenceTag confidence={flag.confidence} />
          </summary>
          <pre>{JSON.stringify(flag.evidence, null, 2)}</pre>
        </details>
      ))}
    </div>
  );
}

/**
 * Confirmable paths mirror profiler.ConfirmablePaths. The backend is
 * authoritative and refuses anything outside its own set, so drift shows up as a
 * 400 rather than as a confirmation that silently does nothing.
 */
interface Confirmable {
  path: string;
  hint: string;
  /**
   * How a correction is entered, or "none" where the field is a structure that a
   * text box cannot express — a session boundary or an exclusion list. Those can
   * still be confirmed or rejected, which is the decision that matters; editing
   * them belongs to the exploration pane, where the boundary is visible.
   */
  correct: "text" | "number" | "none";
}

const CONFIRMABLE: Confirmable[] = [
  { path: "value_semantics.unit", hint: "the resolved unit", correct: "text" },
  { path: "value_semantics.characteristic_id", hint: "the canonical characteristic id", correct: "text" },
  {
    path: "value_semantics.kind",
    hint: "instantaneous, cumulative_counter, binary, categorical, status",
    correct: "text",
  },
  {
    path: "activity_pattern.classification",
    hint: "continuous, session_based, intermittent, status",
    correct: "text",
  },
  { path: "activity_pattern.active_threshold", hint: "the idle/active split", correct: "number" },
  { path: "activity_pattern.sessions", hint: "detected session boundaries", correct: "none" },
  { path: "sampling.gaps", hint: "the classification of a gap", correct: "none" },
  { path: "recommendations.usable_range", hint: "the range recommended as usable", correct: "none" },
  { path: "recommendations.exclusions", hint: "the ranges recommended for exclusion", correct: "none" },
];

function confirmableFor(path: string): Confirmable {
  return CONFIRMABLE.find((entry) => entry.path === path) ?? CONFIRMABLE[0];
}

function OverridesSection({
  profile,
  onChanged,
}: {
  profile: SeriesProfile;
  onChanged: (profileId: string) => Promise<void>;
}) {
  const resolution = Object.values(profile.resolution ?? {});
  const [fieldPath, setFieldPath] = useState(CONFIRMABLE[0].path);
  const [action, setAction] = useState<"confirm" | "correct" | "reject">("confirm");
  const [confirmedValue, setConfirmedValue] = useState("");
  const [note, setNote] = useState("");

  const confirmable = confirmableFor(fieldPath);
  const correcting = action === "correct" && confirmable.correct !== "none";

  const submit = useAction(
    useCallback(
      async (_signal: AbortSignal, profileId: string, body: Parameters<typeof api.createOverride>[1]) => {
        const response = await api.createOverride(profileId, body);
        setConfirmedValue("");
        setNote("");
        await onChanged(profileId);
        return response;
      },
      [onChanged],
    ),
  );

  return (
    <Section
      title={`Developer confirmations${resolution.length > 0 ? ` (${resolution.length})` : ""}`}
      note="append-only overlay, merged at read time only"
      defaultOpen={resolution.length > 0}
    >
      {resolution.length === 0 ? (
        <Muted>
          Nothing confirmed yet. A confirmation never edits the profile: it is appended to an overlay
          and merged on read, so recomputing with a better detector keeps it.
        </Muted>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Field</TableHead>
              <TableHead>Computed</TableHead>
              <TableHead>Confirmed</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>By</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {resolution.map((entry) => (
              <TableRow key={entry.override_id}>
                <TableCell>
                  <code>{entry.field_path}</code>
                </TableCell>
                <TableCell>{renderValue(entry.computed_value)}</TableCell>
                <TableCell>{renderValue(entry.confirmed_value)}</TableCell>
                <TableCell>
                  <span className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">{entry.action}</span>
                  {entry.supersedes && entry.supersedes.length > 0 && (
                    <span className="muted-inline text-xs text-muted-foreground"> supersedes {entry.supersedes.length}</span>
                  )}
                </TableCell>
                <TableCell className="muted-inline text-xs text-muted-foreground">{entry.created_by}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <form
        className="override-form"
        onSubmit={(e) => {
          e.preventDefault();
          void submit.invoke(profile.profile_id, {
            field_path: fieldPath,
            action,
            computed_value: computedFor(profile, fieldPath),
            // A number goes over the wire as a number: the backend stores the
            // value as given, so sending "3.5" would put a string where every
            // reader expects a threshold.
            confirmed_value: correcting
              ? confirmable.correct === "number"
                ? Number(confirmedValue)
                : confirmedValue
              : undefined,
            note: note || undefined,
          });
        }}
      >
        <label>
          <span>Field</span>
          <Select
            value={fieldPath}
            onValueChange={(value) => {
              if (value === null) return;
              setFieldPath(value);
              // A structured field cannot be corrected through a text box, so the
              // action falls back rather than submitting a correction with nothing
              // in it.
              if (confirmableFor(value).correct === "none" && action === "correct") {
                setAction("confirm");
              }
            }}
          >
            <SelectTrigger size="sm" aria-label="Field" className="w-auto min-w-52">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
            {CONFIRMABLE.map((entry) => (
              <SelectItem key={entry.path} value={entry.path} title={entry.hint}>
                {entry.path}
              </SelectItem>
            ))}
            </SelectContent>
          </Select>
        </label>
        <label>
          <span>Action</span>
          <Select
            value={action}
            onValueChange={(value) => {
              if (value === null) return;
              setAction(value as "confirm" | "correct" | "reject");
            }}
          >
            <SelectTrigger size="sm" aria-label="Action" className="w-auto min-w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="confirm">confirm</SelectItem>
              <SelectItem value="correct" disabled={confirmable.correct === "none"}>
                correct
              </SelectItem>
              <SelectItem value="reject">reject</SelectItem>
            </SelectContent>
          </Select>
        </label>
        {correcting && (
          <label>
            <span>Correct to</span>
            <Input
              value={confirmedValue}
              onChange={(e) => setConfirmedValue(e.target.value)}
              placeholder={confirmable.correct === "number" ? "3.5" : confirmable.hint}
              inputMode={confirmable.correct === "number" ? "decimal" : undefined}
              required
            />
          </label>
        )}
        <label className="grow">
          <span>Note</span>
          <Input value={note} onChange={(e) => setNote(e.target.value)} placeholder="Why" />
        </label>
        <Button variant="outline" className={submit.pending ? "busy animate-pulse" : undefined} type="submit" disabled={submit.pending}>
          {submit.pending ? "Recording…" : "Record"}
        </Button>
      </form>
      {submit.error && <Muted>{submit.error}</Muted>}
    </Section>
  );
}

/**
 * computedFor supplies what the detector said, so the overlay stays diffable —
 * "the detector said W, the developer corrected it to kW" is the record §5.4.3
 * wants, and it is worth nothing if the left-hand side is empty.
 *
 * The structured fields carry a summary rather than the whole array: a
 * confirmation of a session boundary needs to record which boundaries were on
 * screen, not to duplicate them.
 */
function computedFor(profile: SeriesProfile, fieldPath: string): unknown {
  const semantics = profile.value_semantics;
  const activity = profile.activity_pattern;
  const sampling = profile.sampling;

  switch (fieldPath) {
    case "value_semantics.unit":
      return semantics.unit;
    case "value_semantics.characteristic_id":
      return semantics.characteristic_id;
    case "value_semantics.kind":
      return isNotComputed(semantics.kind) ? null : semantics.kind;
    case "activity_pattern.classification":
      return isNotComputed(activity) ? null : activity.classification;
    case "activity_pattern.active_threshold":
      return isNotComputed(activity) ? null : activity.active_threshold;
    case "activity_pattern.sessions": {
      if (isNotComputed(activity) || isNotComputed(activity.session_stats)) return null;
      const stats = activity.session_stats;
      return { count: stats.count, median_duration_s: stats.median_duration_s };
    }
    case "sampling.gaps": {
      if (isNotComputed(sampling)) return null;
      const byClassification: Record<string, number> = {};
      for (const gap of sampling.gaps) {
        byClassification[gap.classification] = (byClassification[gap.classification] ?? 0) + 1;
      }
      return { count: sampling.gaps.length, by_classification: byClassification };
    }
    case "recommendations.usable_range": {
      const range = profile.recommendations.usable_range;
      return isNotComputed(range) ? null : range;
    }
    case "recommendations.exclusions":
      return { count: profile.recommendations.exclusions.length };
    default:
      return null;
  }
}

function renderValue(value: unknown): React.ReactNode {
  if (value === null || value === undefined) return <span className="muted-inline text-xs text-muted-foreground">—</span>;
  if (typeof value === "object") return <code>{JSON.stringify(value)}</code>;
  return <code>{String(value)}</code>;
}

function ProvenanceSection({ provenance }: { provenance: Provenance }) {
  const entries = Object.entries(provenance ?? {}).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return null;

  return (
    <Section
      title="Provenance"
      note="which pass produced each field — dropped from the LLM view"
      defaultOpen={false}
    >
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Field</TableHead>
            <TableHead title="Aggregated reads hide gaps and irregularity, so this matters per field">
              Read
            </TableHead>
            <TableHead>Source</TableHead>
            <TableHead>Detail</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {entries.map(([path, entry]) => (
            <TableRow key={path}>
              <TableCell>
                <code>{path}</code>
              </TableCell>
              <TableCell>
                <span className={`tag read-${entry?.read_mode ?? "none"}`}>
                  {entry?.read_mode ?? "—"}
                </span>
              </TableCell>
              <TableCell>{entry?.source ?? "—"}</TableCell>
              <TableCell className="muted-inline text-xs text-muted-foreground">
                {[entry?.detector, entry?.ref, entry?.group_time, entry?.note]
                  .filter(Boolean)
                  .join(" · ")}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Section>
  );
}

// --- the LLM view ---

function ProjectionTab({ profileId }: { profileId: string }) {
  const [budget, setBudget] = useState("");
  const [applied, setApplied] = useState<number | undefined>(undefined);
  const load = useCallback(() => api.projection(profileId, applied), [profileId, applied]);
  const { data, error, loading } = useLoad(load);

  return (
    <>
      <div className="llm-head">
        <p className="muted text-muted-foreground">
          Exactly what a model would be given. Unbounded arrays are collapsed, provenance is dropped,
          and every collapse is recorded — so the model knows it is reading a summary. The exposure
          tier governs which tools may reach this; a developer reading it here is not gated by it.
        </p>
        <form
          className="filters flex flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            const parsed = Number.parseInt(budget, 10);
            setApplied(Number.isFinite(parsed) && parsed > 0 ? parsed : undefined);
          }}
        >
          <label className="filter-check flex items-center gap-2 text-sm">
            <span>Token budget</span>
            <Input
              value={budget}
              onChange={(e) => setBudget(e.target.value)}
              placeholder="unbounded"
              inputMode="numeric"
              aria-label="Token budget"
            />
          </label>
          <Button variant="outline" type="submit">Apply</Button>
        </form>
      </div>

      {loading && <Busy>Projecting…</Busy>}
      {error && <Muted>{error}</Muted>}
      {data && <Projection view={data} />}
    </>
  );
}

function Projection({ view }: { view: LLMProfileView }) {
  const payload = useMemo(() => JSON.stringify(view, null, 2), [view]);
  const size = useMemo(() => JSON.stringify(view).length, [view]);

  return (
    <>
      <div className="reads clean">
        <span className="reads-headline">≈{Math.round(size / 4)} tokens</span>
        <span className="reads-detail">{size} bytes of JSON</span>
        <span className="reads-note">roughly four bytes to the token</span>
      </div>

      {view.elided.length > 0 && (
        <Section
          title={`Elided (${view.elided.length})`}
          note="what was summarised, and where the rest is"
        >
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Field</TableHead>
                <TableHead>Total</TableHead>
                <TableHead>Shown</TableHead>
                <TableHead>Fetch</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {view.elided.map((elision, index) => (
                // sampling.gaps can appear twice: once collapsed to the largest
                // few, once dropped entirely under a tight budget.
                <TableRow key={`${elision.field}-${index}`}>
                  <TableCell>
                    <code>{elision.field}</code>
                  </TableCell>
                  <TableCell className="numeric text-right tabular-nums">{elision.total}</TableCell>
                  <TableCell className="numeric text-right tabular-nums">{elision.shown}</TableCell>
                  <TableCell className="muted-inline text-xs text-muted-foreground">
                    {elision.fetch ? <code>{elision.fetch}</code> : "—"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Section>
      )}

      {view.overrides.length > 0 && (
        <Section title="Developer decisions the model sees">
          <ul className="list flex flex-col gap-1">
            {view.overrides.map((override) => (
              <li key={override.override_id}>
                <code>{override.field_path}</code> {override.action}{" "}
                {renderValue(override.computed_value)} → {renderValue(override.confirmed_value)}
              </li>
            ))}
          </ul>
        </Section>
      )}

      <Section title="Payload" note="the JSON itself" defaultOpen={false}>
        <pre className="json max-h-56 overflow-auto rounded-md bg-muted p-2 font-mono text-xs">{payload}</pre>
      </Section>
    </>
  );
}

// --- sessions ---

function SessionsTab({ profileId }: { profileId: string }) {
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [history, setHistory] = useState<(string | undefined)[]>([]);

  const load = useCallback(() => api.sessions(profileId, { limit: 50, cursor }), [profileId, cursor]);
  const { data, error, loading } = useLoad(load);

  const next = (nextCursor: string) => {
    setHistory([...history, cursor]);
    setCursor(nextCursor);
  };
  const back = () => {
    setCursor(history[history.length - 1]);
    setHistory(history.slice(0, -1));
  };

  return (
    <>
      <Muted>
        A separate paginated resource, referenced from the profile rather than embedded in it: two
        years of cycles is thousands of entries, and the profile carries statistics and a handful of
        exemplars instead.
      </Muted>
      {loading && <Busy>Loading sessions…</Busy>}
      {error && <Muted>{error}</Muted>}
      {data && (
        <SessionTable page={data} onNext={next} onBack={history.length > 0 ? back : undefined} />
      )}
    </>
  );
}

function SessionTable({
  page,
  onNext,
  onBack,
}: {
  page: SessionPage;
  onNext: (cursor: string) => void;
  onBack?: () => void;
}) {
  if (page.total === 0) {
    return <Muted>No sessions were detected for this series.</Muted>;
  }
  const nextCursor = page.next_cursor;
  return (
    <>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>From</TableHead>
            <TableHead>Duration</TableHead>
            <TableHead title="The value integrated over time, in the series' own unit multiplied by seconds">
              Energy
            </TableHead>
            <TableHead>Peak</TableHead>
            <TableHead>Points</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {page.sessions.map((session, index) => (
            <TableRow key={`${session.from}-${index}`}>
              <TableCell>{dateTime(session.from)}</TableCell>
              <TableCell className="numeric text-right tabular-nums">{seconds(session.duration_s)}</TableCell>
              <TableCell className="numeric text-right tabular-nums">{num(session.energy)}</TableCell>
              <TableCell className="numeric text-right tabular-nums">{num(session.peak)}</TableCell>
              <TableCell className="numeric text-right tabular-nums">{session.points}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <div className="pager">
        <span className="muted-inline text-xs text-muted-foreground">
          {page.sessions.length} shown of {page.total}
        </span>
        {onBack && <Button variant="outline" onClick={onBack}>Previous</Button>}
        {nextCursor && <Button variant="outline" onClick={() => onNext(nextCursor)}>Next</Button>}
      </div>
    </>
  );
}
