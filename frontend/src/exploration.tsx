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

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  type ChartAnnotation,
  type ChartData,
  type ChartMarker,
  type ChartSeriesData,
  type ProfileOverrideRecord,
} from "./api";
import { setParam, useParam } from "./router";
import { Busy, Muted, Pane, Section, dateTime, num, useAction, useLoad } from "./ui";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

/**
 * The exploration pane (§5.9, §5.10).
 *
 * Three things about it are load-bearing rather than decoration.
 *
 * It renders a *specification*, never an image. The assistant emits the document
 * §5.9 defines — series, transforms, annotations, an axis — and this draws it from
 * the developer's own read of the platform. That is why an LLM at tier L1 can
 * demonstrate a data selection visually while never seeing a value itself.
 *
 * The bands and marks are attributed. A span the profiler detected, a span the
 * assistant wrote and a span the developer added are three different kinds of
 * claim, and the backend stamps which is which — so nothing on screen can borrow
 * authority it does not have.
 *
 * And a confirmable claim is confirmable *here*, where it is visible. §5.10 calls
 * human confirmation of derived semantics a contribution rather than an
 * affordance; the profiler view can already confirm a unit from a table, but a
 * session boundary is a thing you have to see to judge.
 */
export function ExplorationView() {
  const [reloadKey, setReloadKey] = useState(0);

  // reloadKey is in the dependency list without being read in the body, which is
  // the one legitimate use of that: bumping it is how a chart derived here gets
  // into a listing that was loaded before it existed.
  const charts = useLoad(useCallback(() => api.charts({ limit: 100 }), [reloadKey]));

  // Which chart is open is the URL's. A chart proposed in chat or built from a
  // profile arrives as a navigation to ?chart=<id> rather than as state handed
  // across panes by the shell, so it survives a reload and can be sent to someone
  // — which for a claim a developer is being asked to confirm (§5.10) is the
  // difference between "look at this" and "open the pane and find it".
  const selected = useParam("chart");

  const specs = charts.data?.charts ?? [];

  /*
   * A chart can arrive while this view is already mounted.
   *
   * The developer is standing on /tools/exploration, the assistant in the chat pane
   * beside it returns a render_chart result, and "Open in exploration" navigates to
   * ?chart=X. Nothing remounts: the pane on the right is keyed on the id and loads
   * X quite happily, but the listing on the left was fetched before X existed. So
   * the chart being shown is missing from the column that names every chart, no
   * entry is marked active, and only "Refresh" repairs it. Bumping the key is what
   * the prop-driven version did before the id moved into the URL, and it is still
   * the whole of the fix.
   *
   * Guarded on the id in a ref rather than on "is it in the list", because an empty
   * list means two different things — not loaded yet, and failed to load — and a
   * membership test cannot tell them apart. It would refetch on every render after
   * a failed listing, which is a retry loop against the platform wearing a fix's
   * clothes. One refresh per id this has not already seen; the value at mount
   * counts as seen, because the first load is issued after the navigation that
   * carried X and already returns it.
   */
  const refreshedFor = useRef(selected);
  useEffect(() => {
    if (selected === null || selected === refreshedFor.current) return;
    refreshedFor.current = selected;
    if (specs.some((spec) => spec.chart_id === selected)) return;
    setReloadKey((key) => key + 1);
  }, [selected, specs]);

  // Falling back to the newest keeps a bare /tools/exploration useful. It is
  // deliberately not written back: the parameter should say what the developer
  // chose, so a link to the pane keeps meaning "the newest chart" rather than
  // freezing whichever one happened to be newest when the link was made.
  const current = selected ?? specs[0]?.chart_id ?? null;

  return (
    <main className="panes exploration">
      <Pane
        title="Charts"
        subtitle="declarative specifications, newest first"
        actions={
          <Button variant="outline"
            className={charts.loading ? "busy animate-pulse" : undefined}
            onClick={() => setReloadKey((key) => key + 1)}
            disabled={charts.loading}
          >
            {charts.loading ? "Loading…" : "Refresh"}
          </Button>
        }
      >
        {charts.error && <Muted>{charts.error}</Muted>}
        {!charts.error && specs.length === 0 && (
          <Muted>
            No charts yet. The assistant proposes one with <code>render_chart</code>, and the
            profiler view charts a series it has just profiled. A chart is a specification, not a
            picture: the values are read here, with your token.
          </Muted>
        )}
        <ul className="list chart-list flex flex-col gap-1">
          {specs.map((spec) => (
            <li key={spec.chart_id}>
              <Button variant="ghost" size="sm"
                className={spec.chart_id === current ? "chart-entry active" : "chart-entry"}
                onClick={() => setParam("chart", spec.chart_id)}
              >
                <span className="chart-entry-title">{spec.title}</span>
                <span className="chart-entry-meta">
                  <span className={`tag author-${spec.author}`}>{spec.author}</span>
                  {spec.series.length} series · {dateTime(spec.created_at)}
                </span>
              </Button>
            </li>
          ))}
        </ul>
      </Pane>

      {current ? (
        <ChartPane
          key={current}
          chartId={current}
          onChanged={() => setReloadKey((key) => key + 1)}
          onDerived={(chartId) => {
            setParam("chart", chartId);
            setReloadKey((key) => key + 1);
          }}
        />
      ) : (
        <Pane title="Exploration" subtitle="nothing selected">
          <Muted>Select a chart.</Muted>
        </Pane>
      )}
    </main>
  );
}

/** The window presets, in the terms a developer thinks in rather than in seconds. */
const RANGES: [string, number][] = [
  ["24 h", 24 * 3600],
  ["7 d", 7 * 24 * 3600],
  ["30 d", 30 * 24 * 3600],
  ["90 d", 90 * 24 * 3600],
];

const BUCKETS = ["", "1m", "5m", "15m", "1h", "6h", "1d"];

function ChartPane({
  chartId,
  onChanged,
  onDerived,
}: {
  chartId: string;
  onChanged: () => void;
  onDerived: (chartId: string) => void;
}) {
  const [range, setRange] = useState<number | null>(null);
  const [bucket, setBucket] = useState("");
  const [version, setVersion] = useState(0);

  // version is a dependency the body does not read, for the same reason as above: a
  // confirmation changes what the next read resolves, so recording one has to
  // re-run this.
  const load = useCallback(async () => {
    const params: { from?: string; to?: string; groupTime?: string } = {};
    if (range) {
      // A preset window is resolved against the specification's own end rather than
      // against now: a chart of last March must not silently become a chart of this
      // week because a button says "7 d".
      const spec = await api.chart(chartId);
      const end = new Date(spec.window.to);
      params.to = end.toISOString();
      params.from = new Date(end.getTime() - range * 1000).toISOString();
    }
    if (bucket) params.groupTime = bucket;
    return api.chartData(chartId, params);
  }, [chartId, range, bucket, version]);

  const { data, error, loading } = useLoad(load);
  const reload = useCallback(() => setVersion((v) => v + 1), []);

  const confirm = useAction(
    useCallback(
      async (_signal: AbortSignal, body: Parameters<typeof api.confirmChart>[1]) => {
        const response = await api.confirmChart(chartId, body);
        reload();
        return response;
      },
      [chartId, reload],
    ),
  );

  // A transform is part of the specification, and a specification is an immutable
  // artifact (the same rule the profiles follow, D21). So converting an axis
  // derives a *new* chart from this one rather than editing it — the original
  // stays exactly as it was proposed, which is what makes the record readable
  // later.
  const derive = useAction(
    useCallback(
      async (_signal: AbortSignal, seriesIndex: number, transform: string, suffix: string) => {
        const spec = await api.chart(chartId);
        const created = await api.createChart({
          title: `${spec.title} ${suffix}`,
          caption: spec.caption,
          session_id: spec.session_id,
          series: spec.series.map((series, index) =>
            index === seriesIndex ? { ...series, transform } : series,
          ),
          annotations: spec.annotations,
          markers: spec.markers,
          window: { from: spec.window.from, to: spec.window.to },
          group_time: spec.group_time,
        });
        onDerived(created.spec.chart_id);
        return created;
      },
      [chartId, onDerived],
    ),
  );

  const discard = useAction(
    useCallback(
      async () => {
        await api.deleteChart(chartId);
        onChanged();
      },
      [chartId, onChanged],
    ),
  );

  return (
    <Pane
      title={data?.title ?? "Chart"}
      subtitle={
        data
          ? `${data.series.length} series · ${data.group_time} buckets · ${data.reads.points} points read`
          : "loading"
      }
      actions={
        <>
          <Select value={bucket} onValueChange={(value) => setBucket(value ?? "")}>
            <SelectTrigger size="sm" title="aggregation bucket" aria-label="Aggregation bucket" className="w-auto">
              <SelectValue>{(value: string | null) => value || "bucket: auto"}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {BUCKETS.map((option) => (
                <SelectItem key={option || "auto"} value={option}>
                  {option || "bucket: auto"}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <div className="tabs small inline-flex items-center gap-1 text-xs">
            {RANGES.map(([label, span]) => (
              <Button variant="outline"
                key={label}
                className={range === span ? "active" : ""}
                onClick={() => setRange(range === span ? null : span)}
              >
                {label}
              </Button>
            ))}
            <Button variant="outline" className={range === null ? "active" : ""} onClick={() => setRange(null)}>
              full
            </Button>
          </div>
          <Button variant="outline" className={loading ? "busy animate-pulse" : undefined} onClick={reload} disabled={loading}>
            {loading ? "Reading…" : "Reload"}
          </Button>
          <Button variant="outline" onClick={() => void discard.invoke()}>Discard</Button>
        </>
      }
    >
      {error && <Muted>{error}</Muted>}
      {derive.error && <Muted>{derive.error}</Muted>}
      {discard.error && <Muted>{discard.error}</Muted>}
      {!data && !error && <Busy>Reading the series…</Busy>}

      {data && (
        <>
          <ChartCanvas data={data} />
          {data.caption && <p className="chart-caption">{data.caption}</p>}
          <ChartNotes data={data} />

          <Section title="Series and units" note="the axis is resolved from the ontology">
            {data.series.map((series) => (
              <SeriesRow
                key={series.index}
                series={series}
                pending={confirm.pending || derive.pending}
                onConfirm={(body) => void confirm.invoke(body)}
                onConvert={(target, unit) =>
                  void derive.invoke(series.index, `convert:${target}`, `(${unit})`)
                }
              />
            ))}
            {confirm.error && <Muted>{confirm.error}</Muted>}
          </Section>

          <AnnotationSection
            annotations={data.annotations}
            markers={data.markers}
            dropped={data.annotations_dropped}
            pending={confirm.pending}
            onConfirm={(body) => void confirm.invoke(body)}
          />

          <ConfirmationLog series={data.series} />
        </>
      )}
    </Pane>
  );
}

// --- the canvas ---

/**
 * Eight slots, which is also why a chart is capped at eight series: beyond that
 * the lines stop being distinguishable and the chart stops being an argument.
 *
 * The slot is a class name and the colour behind it lives in `index.css`, as
 * `--series-1` through `--series-8`. Two reasons for the indirection. The palette
 * has to differ between the light and the dark theme — the same eight hues stepped
 * for the surface they are drawn on, because a colour chosen against #161a22 is
 * not readable on white — and a stylesheet can do that in a media query where an
 * array of hex strings in here would need to re-render the chart to follow the
 * theme. The set they replaced also failed on its own terms: its olive and its
 * orange were ΔE 3.9 apart for a deuteranope and ΔE 12.4 apart for everyone else,
 * so two adjacent series were one colour in practice.
 *
 * Keyed on `series.index` rather than on the position in the array being mapped.
 * Those are usually the same number and must not be relied on to be: the index is
 * assigned by the backend and belongs to the series, so a resolution that omits
 * one would otherwise shift every colour after it — and shift the line without
 * shifting the swatch in the list, which reads as two different series.
 */
const SLOTS = 8;

function slot(index: number): string {
  return `s${(index % SLOTS) + 1}`;
}

const VIEW = { width: 1000, height: 340, left: 68, right: 14, top: 14, bottom: 44 };

/**
 * ChartCanvas draws the specification as inline SVG.
 *
 * Hand-drawn rather than through a charting library, and deliberately: the whole
 * frontend depends on React and keycloak-js and nothing else, and what §5.9 needs
 * — lines, bands, marks, one axis — is a page of geometry. A library would arrive
 * with a licence to clear and a rendering model to fight over annotations.
 */
function ChartCanvas({ data }: { data: ChartData }) {
  const geometry = useMemo(() => plotGeometry(data), [data]);

  if (!geometry) {
    return (
      <div className="chart-empty">
        <Muted>
          No numeric values came back for this window. That is not the same as a flat series:
          check the notes below, and the profile's read summary, before reading it as one.
        </Muted>
      </div>
    );
  }

  const { x, y, ticksY, ticksX, plot } = geometry;

  return (
    <svg className="chart" viewBox={`0 0 ${VIEW.width} ${VIEW.height}`} role="img"
      aria-label={`${data.title}, ${data.series.length} series`}>
      {/* Annotation bands first, so the lines are read over them rather than under. */}
      {data.annotations.map((annotation) => {
        const from = Math.max(x(new Date(annotation.from).getTime()), plot.x0);
        const to = Math.min(x(new Date(annotation.to).getTime()), plot.x1);
        if (!(to > from)) return null;
        return (
          <rect
            key={annotation.annotation_id}
            className={`band band-${annotation.severity}`}
            x={from}
            y={plot.y0}
            width={Math.max(to - from, 1)}
            height={plot.y1 - plot.y0}
          >
            <title>{`${annotation.label} · ${annotation.source ?? annotation.author}`}</title>
          </rect>
        );
      })}

      {ticksY.map((tick) => (
        <g key={`y-${tick.value}`}>
          <line className="grid-line" x1={plot.x0} x2={plot.x1} y1={y(tick.value)} y2={y(tick.value)} />
          <text className="axis-label" x={plot.x0 - 8} y={y(tick.value) + 4} textAnchor="end">
            {tick.label}
          </text>
        </g>
      ))}
      {ticksX.map((tick) => (
        <g key={`x-${tick.value}`}>
          <line className="grid-line faint" x1={x(tick.value)} x2={x(tick.value)} y1={plot.y0} y2={plot.y1} />
          <text className="axis-label" x={x(tick.value)} y={plot.y1 + 18} textAnchor="middle">
            {tick.label}
          </text>
        </g>
      ))}

      {data.series.map((series) => (
        <g key={series.index}>
          <polyline
            className={`series-line ${slot(series.index)}`}
            points={series.points
              .map((point) => `${x(new Date(point.t).getTime())},${y(point.v)}`)
              .join(" ")}
          />
          {/*
            A sparse series is drawn as points as well as a line. A single value is
            otherwise a line of zero length — invisible, and indistinguishable from
            a series that returned nothing at all.
          */}
          {series.points.length <= 40 &&
            series.points.map((point) => (
              <circle
                key={point.t}
                className={`series-point ${slot(series.index)}`}
                cx={x(new Date(point.t).getTime())}
                cy={y(point.v)}
                r={2}
              />
            ))}
        </g>
      ))}

      {data.markers.map((marker) => {
        const at = x(new Date(marker.at).getTime());
        if (at < plot.x0 || at > plot.x1) return null;
        return (
          <g key={marker.marker_id} className="marker">
            <line x1={at} x2={at} y1={plot.y0} y2={plot.y1} />
            <polygon points={`${at - 4},${plot.y0} ${at + 4},${plot.y0} ${at},${plot.y0 + 7}`} />
            <title>{`${marker.label} · ${marker.source ?? marker.author}`}</title>
          </g>
        );
      })}

      <line className="axis" x1={plot.x0} x2={plot.x1} y1={plot.y1} y2={plot.y1} />
      <line className="axis" x1={plot.x0} x2={plot.x0} y1={plot.y0} y2={plot.y1} />
      <text className="axis-unit" x={4} y={plot.y0 - 2}>
        {axisLabel(data)}
      </text>
    </svg>
  );
}

/** axisLabel says what the axis is, including when it cannot say (D24, D29). */
function axisLabel(data: ChartData): string {
  if (data.y_axis.mixed) return "mixed units";
  if (!data.y_axis.unit) return "unit unknown";
  return data.y_axis.confirmed ? `${data.y_axis.unit} (confirmed)` : data.y_axis.unit;
}

interface Tick {
  value: number;
  label: string;
}

function plotGeometry(data: ChartData) {
  // A running minimum and maximum rather than Math.min(...values): the point cap is
  // configurable, and a spread over tens of thousands of arguments is how that turns
  // into a blown call stack.
  let low = Number.POSITIVE_INFINITY;
  let high = Number.NEGATIVE_INFINITY;
  let count = 0;
  for (const series of data.series) {
    for (const point of series.points) {
      if (!Number.isFinite(point.v)) continue;
      if (point.v < low) low = point.v;
      if (point.v > high) high = point.v;
      count += 1;
    }
  }
  if (count === 0) return null;

  const from = new Date(data.window.from).getTime();
  const to = new Date(data.window.to).getTime();
  if (low === high) {
    // A constant series still deserves an axis it can be read against, and a
    // zero-height plot would draw the line on the frame.
    const padding = Math.abs(low) > 0 ? Math.abs(low) * 0.1 : 1;
    low -= padding;
    high += padding;
  } else {
    const padding = (high - low) * 0.06;
    low -= padding;
    high += padding;
  }

  const plot = {
    x0: VIEW.left,
    x1: VIEW.width - VIEW.right,
    y0: VIEW.top,
    y1: VIEW.height - VIEW.bottom,
  };
  const x = (at: number) =>
    plot.x0 + ((at - from) / Math.max(to - from, 1)) * (plot.x1 - plot.x0);
  const y = (value: number) =>
    plot.y1 - ((value - low) / (high - low)) * (plot.y1 - plot.y0);

  const ticksY: Tick[] = [];
  for (let i = 0; i <= 4; i++) {
    const value = low + ((high - low) * i) / 4;
    ticksY.push({ value, label: num(value) });
  }

  const span = to - from;
  const perDay = span <= 3 * 24 * 3600 * 1000;
  const ticksX: Tick[] = [];
  for (let i = 0; i <= 4; i++) {
    const value = from + (span * i) / 4;
    const at = new Date(value);
    ticksX.push({
      value,
      label: perDay
        ? at.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })
        : at.toLocaleDateString(undefined, { month: "short", day: "2-digit" }),
    });
  }

  return { x, y, ticksY, ticksX, plot };
}

/**
 * ChartNotes surfaces what the read had to say about itself: a widened bucket,
 * series that are not on one bucket, annotations the cap dropped. Hiding these
 * would leave the chart claiming a resolution or a completeness it does not have —
 * the same reason the profiler never renders a non-result as an empty cell (D24).
 */
function ChartNotes({ data }: { data: ChartData }) {
  const notes = [...data.notes];
  for (const series of data.series) notes.push(...series.notes);
  const dropped = data.series.reduce((total, series) => total + series.non_numeric_dropped, 0);
  if (dropped > 0) notes.push(`${dropped} returned values were not numbers and are not drawn`);
  if (notes.length === 0) return null;

  return (
    <ul className="list tight chart-notes flex flex-col gap-1 leading-tight">
      {notes.map((note, index) => (
        <li key={index} className="muted-inline text-xs text-muted-foreground">
          {note}
        </li>
      ))}
    </ul>
  );
}

// --- units and their confirmation (§5.10) ---

function SeriesRow({
  series,
  pending,
  onConfirm,
  onConvert,
}: {
  series: ChartSeriesData;
  pending: boolean;
  onConfirm: (body: Parameters<typeof api.confirmChart>[1]) => void;
  onConvert: (targetCharacteristicId: string, unit: string) => void;
}) {
  const [correcting, setCorrecting] = useState(false);
  const [value, setValue] = useState("");
  const unit = series.unit;
  // Correcting the characteristic is the stronger of the two, and the one that
  // makes a conversion possible at all: a unit string cannot be converted (D29).
  const field = unit.characteristic_id ? "value_semantics.unit" : "value_semantics.characteristic_id";

  return (
    <div className="series-row">
      <div className="series-head">
        <span className={`series-swatch ${slot(series.index)}`} />
        <strong>{series.label}</strong>
        <code>{series.ref.variable_path}</code>
        <span className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">{series.transform}</span>
        <span className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">{series.group_type}</span>
        <span className="muted-inline text-xs text-muted-foreground">
          {series.points.length} points · {series.group_time}
        </span>
      </div>

      <div className="series-unit">
        <span className={unit.confirmed ? "tag ok-tag inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs" : "tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs"}>
          {unit.unit || "unit unknown"}
        </span>
        <span className="muted-inline text-xs text-muted-foreground">
          from {unit.unit_source}
          {unit.confirmed && unit.computed_unit && unit.computed_unit !== unit.unit
            ? ` · the resolver said ${unit.computed_unit}`
            : ""}
          {unit.confirmed_by ? ` · confirmed by ${unit.confirmed_by}` : ""}
        </span>
        {unit.note && <span className="muted-inline text-xs text-muted-foreground">{unit.note}</span>}
      </div>

      {unit.confirmable && (
        <div className="unit-confirm">
          <span className="muted-inline text-xs text-muted-foreground">
            {field === "value_semantics.unit"
              ? "This unit is not settled. Confirming it is yours to do, not the assistant's."
              : "No canonical characteristic resolved. Naming one is what makes a server-side conversion possible."}
          </span>
          <div className="unit-actions">
            {field === "value_semantics.unit" && (
              <Button variant="outline"
                disabled={pending}
                onClick={() =>
                  onConfirm({
                    series_index: series.index,
                    field_path: field,
                    action: "confirm",
                    note: "confirmed from the exploration pane",
                  })
                }
              >
                Confirm {unit.unit || "as unknown"}
              </Button>
            )}
            <Button variant="outline" disabled={pending} onClick={() => setCorrecting(!correcting)}>
              {correcting ? "Cancel" : "Correct"}
            </Button>
            <Button variant="outline"
              disabled={pending}
              onClick={() =>
                onConfirm({
                  series_index: series.index,
                  field_path: field,
                  action: "reject",
                  note: "rejected from the exploration pane",
                })
              }
            >
              Reject
            </Button>
          </div>
          {correcting && (
            <form
              className="override-form"
              onSubmit={(event) => {
                event.preventDefault();
                onConfirm({
                  series_index: series.index,
                  field_path: field,
                  action: "correct",
                  confirmed_value: value,
                  note: "corrected from the exploration pane",
                });
                setCorrecting(false);
                setValue("");
              }}
            >
              <label className="grow">
                <span>{field === "value_semantics.unit" ? "The unit is" : "The characteristic is"}</span>
                <Input
                  value={value}
                  onChange={(event) => setValue(event.target.value)}
                  placeholder={
                    field === "value_semantics.unit" ? "kW" : "urn:infai:ses:characteristic:…"
                  }
                  required
                />
              </label>
              <Button variant="outline" type="submit" disabled={pending}>
                Record
              </Button>
            </form>
          )}
        </div>
      )}

      {unit.available_conversions.length > 0 && (
        <div className="conversions">
          <span className="muted-inline text-xs text-muted-foreground">Convert, server-side, along the ontology's graph:</span>
          {unit.available_conversions.map((conversion) => (
            <Button variant="outline"
              key={conversion.to_characteristic_id}
              disabled={pending}
              title={`${conversion.to_characteristic_id} · distance ${conversion.distance}`}
              onClick={() => onConvert(conversion.to_characteristic_id, conversion.to_unit || "converted")}
            >
              → {conversion.to_unit || conversion.to_characteristic_id}
            </Button>
          ))}
        </div>
      )}
    </div>
  );
}

// --- annotations and their confirmation (§5.10) ---

function AnnotationSection({
  annotations,
  markers,
  dropped,
  pending,
  onConfirm,
}: {
  annotations: ChartAnnotation[];
  markers: ChartMarker[];
  dropped: number;
  pending: boolean;
  onConfirm: (body: Parameters<typeof api.confirmChart>[1]) => void;
}) {
  const confirmable = annotations.filter((annotation) => annotation.confirmable).length;

  return (
    <Section
      title={`Annotations (${annotations.length})`}
      note={
        confirmable > 0
          ? `${confirmable} awaiting a decision${dropped > 0 ? `, ${dropped} not shown` : ""}`
          : dropped > 0
            ? `${dropped} not shown`
            : "nothing to decide"
      }
      defaultOpen={confirmable > 0}
    >
      {annotations.length === 0 && markers.length === 0 && (
        <Muted>
          Nothing is annotated. A chart of a series with a profile behind it carries its detected
          sessions, gaps and advised ranges; without one it carries only what the author wrote.
        </Muted>
      )}

      {annotations.length > 0 && (
        <Table className="annotations">
          <TableHeader>
            <TableRow>
              <TableHead>From</TableHead>
              <TableHead>To</TableHead>
              <TableHead>What</TableHead>
              <TableHead>By</TableHead>
              <TableHead>Decision</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {annotations.map((annotation) => (
              <TableRow key={annotation.annotation_id} className={`severity-${annotation.severity}`}>
                <TableCell>{dateTime(annotation.from)}</TableCell>
                <TableCell>{dateTime(annotation.to)}</TableCell>
                <TableCell>
                  {annotation.label}
                  {annotation.source && <span className="muted-inline text-xs text-muted-foreground"> {annotation.source}</span>}
                </TableCell>
                <TableCell>
                  <span className={`tag author-${annotation.author}`}>{annotation.author}</span>
                </TableCell>
                <TableCell>
                  {annotation.confirmable && annotation.field_path !== undefined ? (
                    <AnnotationDecision
                      annotation={annotation}
                      pending={pending}
                      onConfirm={onConfirm}
                    />
                  ) : (
                    <span className="muted-inline text-xs text-muted-foreground">—</span>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      {markers.length > 0 && (
        <ul className="list tight flex flex-col gap-1 leading-tight">
          {markers.map((marker) => (
            <li key={marker.marker_id}>
              <code>{dateTime(marker.at)}</code> {marker.label}
              <span className="muted-inline text-xs text-muted-foreground"> {marker.source ?? marker.author}</span>
            </li>
          ))}
        </ul>
      )}
    </Section>
  );
}

/**
 * AnnotationDecision records a decision about one band.
 *
 * The band itself travels as the computed value, which is the point of the
 * overlay: "the detector put a session here and the developer rejected it" is the
 * empirical record §5.4.3 asks for, and it is worth nothing if the left-hand side
 * is empty.
 */
function AnnotationDecision({
  annotation,
  pending,
  onConfirm,
}: {
  annotation: ChartAnnotation;
  pending: boolean;
  onConfirm: (body: Parameters<typeof api.confirmChart>[1]) => void;
}) {
  const decide = (action: "confirm" | "reject") =>
    onConfirm({
      series_index: annotation.series_index ?? 0,
      field_path: annotation.field_path ?? "",
      action,
      computed_value: {
        from: annotation.from,
        to: annotation.to,
        label: annotation.label,
        source: annotation.source,
      },
      note: `${action === "confirm" ? "confirmed" : "rejected"} on the chart: ${annotation.label}`,
    });

  return (
    <span className="decision">
      <Button variant="outline" disabled={pending} onClick={() => decide("confirm")}>
        Confirm
      </Button>
      <Button variant="outline" disabled={pending} onClick={() => decide("reject")}>
        Reject
      </Button>
      <code className="muted-inline text-xs text-muted-foreground">{annotation.field_path}</code>
    </span>
  );
}

/** ConfirmationLog is the overlay as it stands, so a decision is visible after it. */
function ConfirmationLog({ series }: { series: ChartSeriesData[] }) {
  const records: [number, ProfileOverrideRecord][] = [];
  for (const entry of series) {
    for (const override of entry.confirmations) records.push([entry.index, override]);
  }
  if (records.length === 0) return null;

  return (
    <Section
      title={`Developer confirmations (${records.length})`}
      note="append-only, merged at read time only"
      defaultOpen={false}
    >
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Series</TableHead>
            <TableHead>Field</TableHead>
            <TableHead>Computed</TableHead>
            <TableHead>Confirmed</TableHead>
            <TableHead>Action</TableHead>
            <TableHead>By</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {records.map(([index, override]) => (
            <TableRow key={override.override_id}>
              <TableCell>{index}</TableCell>
              <TableCell>
                <code>{override.field_path}</code>
              </TableCell>
              <TableCell>{renderOverrideValue(override.computed_value)}</TableCell>
              <TableCell>{renderOverrideValue(override.confirmed_value)}</TableCell>
              <TableCell>
                <span className="tag inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs">{override.action}</span>
              </TableCell>
              <TableCell className="muted-inline text-xs text-muted-foreground">
                {override.created_by} · {dateTime(override.created_at)}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Section>
  );
}

function renderOverrideValue(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "string") return value;
  if (typeof value === "number") return num(value);
  if (typeof value === "boolean") return String(value);
  return JSON.stringify(value);
}

/**
 * chartFromProfile is the developer's own way into this pane: a series that has
 * just been profiled, charted with the detections that profile carries.
 *
 * It lives here rather than in the profiler view because the shape of the
 * specification is this pane's business, and because the caption is what tells the
 * developer why they are looking at it.
 */
export async function chartFromProfile(input: {
  deviceID: string;
  serviceID: string;
  variablePath: string;
  profileID: string;
  label: string;
  window: { from: string; to: string };
}): Promise<string> {
  const created = await api.createChart({
    title: input.label,
    caption:
      "charted from the profile, so the bands are that profile's detections. " +
      "Confirming one records a decision, not a note.",
    series: [
      {
        ref: {
          device_id: input.deviceID,
          service_id: input.serviceID,
          variable_path: input.variablePath,
        },
        label: input.label,
        profile_id: input.profileID,
      },
    ],
    window: input.window,
  });
  return created.spec.chart_id;
}
