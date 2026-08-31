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

import { ChevronRightIcon } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Card, CardAction, CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Spinner } from "@/components/ui/spinner";
import { cn } from "@/lib/utils";
import { ApiError, isNotComputed, type Confidence, type NotComputed, type Value } from "./api";
import { Cancelled, WsError } from "./ws";

// --- loading ---

/**
 * useLoad runs a loader whenever its identity changes, and aborts the previous
 * run when it does.
 *
 * The AbortSignal is passed through rather than only used locally: over the
 * WebSocket it becomes a cancel message, so changing a filter mid-read stops the
 * backend's platform reads instead of leaving them to finish for a result nobody
 * will see.
 */
export function useLoad<T>(load: (signal: AbortSignal) => Promise<T>) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    setLoading(true);
    setError(null);
    // The previous result is dropped before the new one arrives. Keeping it would
    // leave the old table on screen under a new filter or beside a fresh error,
    // reading as though it answered the query that just failed — and it would
    // leave a stale pager offering a cursor that no longer belongs to what is
    // shown.
    setData(null);
    load(controller.signal)
      .then((result) => {
        if (!cancelled) setData(result);
      })
      .catch((e: unknown) => {
        // An abort is this effect being superseded, not a failure to report.
        if (!cancelled && !(e instanceof Cancelled)) setError(describe(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [load]);

  return { data, error, loading };
}

/**
 * useAction is useLoad's counterpart for something the developer sets off
 * deliberately — computing a profile, writing a confirmation. Nothing here runs
 * on mount, because every one of these actions costs a platform read or writes
 * to the override log.
 */
export function useAction<Args extends unknown[], T>(
  run: (signal: AbortSignal, ...args: Args) => Promise<T>,
) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const controller = useRef<AbortController | null>(null);

  const invoke = useCallback(
    async (...args: Args) => {
      controller.current?.abort();
      const current = new AbortController();
      controller.current = current;

      setPending(true);
      setError(null);
      try {
        const result = await run(current.signal, ...args);
        setData(result);
        return result;
      } catch (e: unknown) {
        // A cancellation is the developer's own decision, not something to report
        // back to them as a failure.
        if (!(e instanceof Cancelled)) setError(describe(e));
        return null;
      } finally {
        if (controller.current === current) {
          controller.current = null;
          setPending(false);
        }
      }
    },
    [run],
  );

  /** abort stops the operation, and the backend with it. */
  const abort = useCallback(() => {
    controller.current?.abort();
    controller.current = null;
    setPending(false);
  }, []);

  const reset = useCallback(() => {
    setData(null);
    setError(null);
  }, []);

  // Leaving the screen mid-read cancels it rather than letting it finish unseen.
  useEffect(() => () => controller.current?.abort(), []);

  return { data, error, pending, invoke, abort, reset };
}

export function describe(e: unknown): string {
  if (e instanceof Cancelled) return "cancelled";
  if (e instanceof WsError) {
    if (e.isForbidden) {
      return "Forbidden. This account is missing the `developer` realm role, or may not read this resource.";
    }
    return e.status ? `${e.status}: ${e.message}` : e.message;
  }
  if (e instanceof ApiError) {
    if (e.isForbidden) {
      return "Forbidden. This account is missing the `developer` realm role, or may not read this resource.";
    }
    return `${e.status}: ${e.message}`;
  }
  return e instanceof Error ? e.message : String(e);
}

// --- layout ---

/**
 * Pane is the panel every view is built out of: one shadcn `Card` with a titled
 * head, an optional action strip and a scrolling body.
 *
 * The `pane` class stays on the element. It carries no colour or border any more
 * — the Card does that — but the grid rules that place panes beside one another
 * still select on it, and so do the render tests. Treat it as a layout hook, not
 * as styling.
 */
export function Pane({
  title,
  subtitle,
  actions,
  className,
  children,
}: {
  title: string;
  subtitle: string;
  actions?: React.ReactNode;
  /**
   * An extra class on the card, for a pane whose *contents* need laying out
   * differently from the default column — the GitHub account card, which is a
   * strip rather than a panel. Layout of the panes themselves stays in the grid
   * rules; this is for what is inside one.
   */
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <Card
      // `Card` renders a div, and this is a landmark: the panes are the page's
      // top-level regions and a screen reader should be able to jump between
      // them. `role="region"` with a name is what `<section aria-label>` means,
      // and it avoids wrapping the card in a second box just to get the tag.
      role="region"
      aria-label={title}
      className={cn("pane min-h-0 gap-0 overflow-hidden py-0", className)}
    >
      <CardHeader className="grid-cols-[1fr_auto] gap-2 border-b px-4 py-3">
        <div className="min-w-0">
          <h2 className="truncate text-sm leading-none font-semibold">{title}</h2>
          <CardDescription className="pane-subtitle mt-1 text-xs">{subtitle}</CardDescription>
        </div>
        {actions && <CardAction className="pane-actions flex items-center gap-2">{actions}</CardAction>}
      </CardHeader>
      <CardContent className="pane-body min-h-0 flex-1 overflow-auto px-4 py-3">{children}</CardContent>
    </Card>
  );
}

export function Muted({ children }: { children: React.ReactNode }) {
  return <p className="muted text-sm text-muted-foreground">{children}</p>;
}

/**
 * Busy is Muted for the line that is only on screen because something is running.
 *
 * Two things separate it from a muted line that happens to end in an ellipsis. It
 * carries a spinner — a clone or a kernel spawn takes tens of seconds, and a
 * caption that has not moved in that time is indistinguishable from one left
 * behind by a request that died. And it is a live region, because the same wait is
 * invisible to a screen reader otherwise: the text appears with nothing to
 * announce it and disappears the same way.
 *
 * `polite`, never `assertive`. "Cloning…" is not worth cutting into what the
 * developer is already being read.
 *
 * The spinner is `aria-hidden` even though shadcn gives it `role="status"`: this
 * paragraph is already the live region, and two announcements for one wait is one
 * too many.
 */
export function Busy({ children }: { children: React.ReactNode }) {
  return (
    <p className="muted busy flex items-center gap-2 text-sm text-muted-foreground animate-pulse" aria-live="polite">
      <Spinner aria-hidden className="size-3.5" aria-label={undefined} role={undefined} />
      {children}
    </p>
  );
}

export function Centered({ children }: { children: React.ReactNode }) {
  // `min-h-svh` and not `min-h-full`: every use of this is a whole-page state —
  // the session loading, a fatal error, the editor chunk arriving — and those
  // render into a parent with no height of its own, where `min-height: 100%`
  // resolves to nothing and the content sits at the top of the window.
  return (
    <div className="centered flex min-h-svh items-center justify-center p-6">{children}</div>
  );
}

/** Section is a collapsible block, so a long profile can be read in parts. */
export function Section({
  title,
  note,
  defaultOpen = true,
  children,
}: {
  title: string;
  note?: string;
  defaultOpen?: boolean;
  children: React.ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <Collapsible
      render={<section className="section border-b last:border-b-0" />}
      open={open}
      onOpenChange={setOpen}
    >
      <CollapsibleTrigger
        className={cn(
          "section-head flex w-full items-center gap-2 py-2 text-left text-sm font-medium",
          "hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none",
        )}
      >
        <ChevronRightIcon
          aria-hidden
          className="twisty size-3.5 shrink-0 text-muted-foreground transition-transform data-[open]:rotate-90 inline-block w-3 text-center text-xs"
          data-open={open ? "" : undefined}
        />
        <span className="section-title">{title}</span>
        {note && <span className="section-note ml-auto text-xs font-normal text-muted-foreground">{note}</span>}
      </CollapsibleTrigger>
      <CollapsibleContent className="section-body pb-3">{children}</CollapsibleContent>
    </Collapsible>
  );
}

/**
 * KV lays out label/value rows.
 *
 * A two-column grid rather than a table, because a `dd` here is often a whole
 * block — a tag with a disclosure, a nested list — and a table cell would make
 * every row in the list as tall as the tallest one.
 */
export function KV({ children }: { children: React.ReactNode }) {
  return (
    <dl className="kv grid grid-cols-[minmax(8rem,auto)_1fr] items-baseline gap-x-4 gap-y-1.5 text-sm">
      {children}
    </dl>
  );
}

export function Row({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground" title={hint}>
        {label}
      </dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

// --- the not_computed contract, made visible ---

/**
 * Any of D24's non-results, for the tag that draws them.
 *
 * Deliberately wider than `NotComputed`. The reason *sets* are per feature and
 * closed — the profiler's five are about reading a series, `CriterionNotComputed`'s
 * seven are about reading a file in a repository, and a proposal's two are about a
 * turn that did or did not run — but each names a different repair, and telling
 * them apart is the point of having separate sets. What they share is the shape and
 * the rendering, which is what api.ts means by "rendering is shared; the repairs are
 * not". Narrowing this back to one union would force a caller to either mislabel a
 * reason or duplicate the tag, and both are worse than a `string` here: the closed
 * set still holds where the value is produced, and this end only formats it.
 */
export interface NotComputedLike {
  status: "not_computed";
  reason: string;
  detail: string;
}

/**
 * NotComputedTag is why this file exists.
 *
 * D24 makes absence and negation distinguishable in the data; rendering a
 * non-result as an empty cell would throw that away again at the last step, and
 * the reader would draw exactly the conclusion the decision exists to prevent —
 * that a missing periodicity means there is none. So a non-result is shown
 * explicitly, with its reason, and the detail is one click away.
 *
 * The detail is a `Popover` rather than the `title` attribute it used to be. A
 * `title` is unreachable by keyboard and invisible on a touch device, and the
 * detail is the half that says what to do about the non-result — which makes it
 * the half worth reaching.
 */
export function NotComputedTag({ status }: { status: NotComputedLike }) {
  return (
    <Popover>
      <PopoverTrigger
        render={
          <Badge
            render={<button type="button" />}
            variant="outline"
            className="nc nc-tag cursor-default font-normal text-muted-foreground hover:bg-muted"
          />
        }
      >
        not computed · {status.reason.replace(/_/g, " ")}
      </PopoverTrigger>
      <PopoverContent className="nc-detail w-80 text-sm">{status.detail}</PopoverContent>
    </Popover>
  );
}

/**
 * Field renders a Value<T>: the value through `render`, or the non-result. The
 * union in the API types means a caller cannot forget the second case.
 */
export function Field<T>({
  label,
  hint,
  value,
  render,
}: {
  label: string;
  hint?: string;
  value: Value<T>;
  render: (value: T) => React.ReactNode;
}) {
  return (
    <Row label={label} hint={hint}>
      <Val value={value} render={render} />
    </Row>
  );
}

/** Inline variant, for a Value inside a rendered block rather than its own row. */
export function Val<T>({ value, render }: { value: Value<T>; render: (value: T) => React.ReactNode }) {
  // The backend never emits null for a Value — its marshaller cannot, and a Go
  // test walks a whole profile to keep it that way. Treating one as a non-result
  // anyway costs a line and means a future null arrives as an honest "not
  // computed" rather than as `undefined.length` in the middle of a table.
  if (value === null || value === undefined) {
    return (
      <NotComputedTag
        status={
          {
            status: "not_computed",
            reason: "out_of_scope",
            detail: "the field arrived as null",
          } satisfies NotComputed
        }
      />
    );
  }
  return isNotComputed(value) ? <NotComputedTag status={value} /> : <>{render(value)}</>;
}

/**
 * The three confidence levels, as a badge each.
 *
 * Colour is not the only carrier — the word is written out — but the ordering is
 * worth reading straight off, so `high` gets the solid badge, `medium` the muted
 * one and `low` the outline. That is a ramp in weight rather than in hue, which
 * survives both themes and a monochrome screen.
 */
const CONFIDENCE_VARIANT = {
  certain: "default",
  likely: "secondary",
  uncertain: "outline",
} as const satisfies Record<Confidence, "default" | "secondary" | "outline">;

export function ConfidenceTag({ confidence }: { confidence: Confidence }) {
  return (
    <Badge variant={CONFIDENCE_VARIANT[confidence]} className={`confidence ${confidence} font-normal`}>
      {confidence}
    </Badge>
  );
}

// --- formatting ---

export function seconds(value: number): string {
  const abs = Math.abs(value);
  if (abs === 0) return "0 s";
  if (abs < 1) return `${round(value * 1000, 0)} ms`;
  if (abs < 90) return `${round(value, value < 10 ? 2 : 0)} s`;
  if (abs < 5400) return `${round(value / 60, 1)} min`;
  if (abs < 172800) return `${round(value / 3600, 1)} h`;
  return `${round(value / 86400, 1)} d`;
}

/** Named cycles are reported by name, which is what §5.4.13 item 6 asks for. */
export function period(value: number): string {
  const named: [number, string][] = [
    [86400, "daily"],
    [604800, "weekly"],
    [3600, "hourly"],
    [43200, "half-daily"],
  ];
  for (const [length, name] of named) {
    if (value > length * 0.9 && value < length * 1.1) return `${name} (${seconds(value)})`;
  }
  return seconds(value);
}

export function round(value: number, digits = 2): number {
  const factor = 10 ** digits;
  return Math.round(value * factor) / factor;
}

/** Thousands separators, so that a long figure stays countable at a glance. */
const grouped = new Intl.NumberFormat("en-GB", { maximumFractionDigits: 4 });

/** Three significant figures, for a value that has already been scaled. */
function threeFigures(value: number): number {
  const abs = Math.abs(value);
  return round(value, abs < 10 ? 2 : abs < 100 ? 1 : 0);
}

/**
 * Significant figures, for numbers whose magnitude is not known in advance.
 * Never exponential: "6.95e+3" is a form the reader has to decode first. A
 * large number is grouped and, past a million, carries an SI suffix; a small
 * one is written out with its leading zeros.
 */
export function num(value: number): string {
  if (!Number.isFinite(value)) return "—";
  if (value === 0) return "0";
  const abs = Math.abs(value);
  if (abs >= 1e6) {
    const units = ["M", "G", "T", "P", "E"];
    let scaled = value / 1e6;
    let unit = 0;
    while (Math.abs(scaled) >= 1000 && unit < units.length - 1) {
      scaled /= 1000;
      unit += 1;
    }
    return `${grouped.format(threeFigures(scaled))}${units[unit]}`;
  }
  if (abs >= 0.01) return grouped.format(round(value, abs < 1 ? 4 : 2));
  // Three significant figures written out, however far below a hundredth the
  // value sits. Twelve decimals is where a reading stops being one, and what
  // rounds away there was never a measurement.
  const written = value
    .toFixed(Math.min(12, 2 - Math.floor(Math.log10(abs))))
    .replace(/0+$/, "")
    .replace(/\.$/, "");
  return written === "0" || written === "-0" ? "0" : written;
}

export function percent(ratio: number): string {
  return `${round(ratio * 100, 1)}%`;
}

export function bytes(value: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let scaled = value;
  let unit = 0;
  while (scaled >= 1024 && unit < units.length - 1) {
    scaled /= 1024;
    unit += 1;
  }
  return `${round(scaled, 1)} ${units[unit]}`;
}

export function dateTime(value: string): string {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function date(value: string): string {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "2-digit" });
}

export function ago(secondsElapsed: number): string {
  return `${seconds(secondsElapsed)} ago`;
}

/** Shortens a platform URN to its last segment, for a table cell. */
export function shortId(id: string): string {
  const parts = id.split(":");
  return parts.length > 1 ? parts[parts.length - 1] : id;
}

/**
 * A commit as the seven characters a developer reads, quotes and pastes.
 *
 * Separate from shortId, which cuts a URN at its last colon: a sha has no colon
 * in it, so shortId hands all forty characters straight back — which is how the
 * repository panel came to print a full sha where it meant to print a short one.
 */
export function shortSHA(sha: string): string {
  return sha.slice(0, 7);
}

/**
 * A moment as a clock time, for a fact whose worth is how long ago it was.
 *
 * The date is left off deliberately: this labels something that happened in the
 * browser session being looked at, and a session that has run past midnight is
 * rare enough that carrying a date on every reading of it is the worse trade.
 */
export function clock(at: number): string {
  return new Date(at).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}
