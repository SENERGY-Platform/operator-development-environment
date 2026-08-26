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
   * An extra class on the section, for a pane whose *contents* need laying out
   * differently from the default column — the GitHub account card, which is a
   * strip rather than a panel. Layout of the panes themselves stays in the grid
   * rules; this is for what is inside one.
   */
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <section className={className === undefined ? "pane" : `pane ${className}`}>
      <div className="pane-head">
        <div>
          <h2>{title}</h2>
          <p className="pane-subtitle">{subtitle}</p>
        </div>
        {actions && <div className="pane-actions">{actions}</div>}
      </div>
      <div className="pane-body">{children}</div>
    </section>
  );
}

export function Muted({ children }: { children: React.ReactNode }) {
  return <p className="muted">{children}</p>;
}

export function Centered({ children }: { children: React.ReactNode }) {
  return <div className="centered">{children}</div>;
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
    <section className="section">
      <button className="section-head" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="twisty">{open ? "▾" : "▸"}</span>
        <span className="section-title">{title}</span>
        {note && <span className="section-note">{note}</span>}
      </button>
      {open && <div className="section-body">{children}</div>}
    </section>
  );
}

/** KV lays out label/value rows. */
export function KV({ children }: { children: React.ReactNode }) {
  return <dl className="kv">{children}</dl>;
}

export function Row({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <>
      <dt title={hint}>{label}</dt>
      <dd>{children}</dd>
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
 * SPEC D24 makes absence and negation distinguishable in the data; rendering a
 * non-result as an empty cell would throw that away again at the last step, and
 * the reader would draw exactly the conclusion the decision exists to prevent —
 * that a missing periodicity means there is none. So a non-result is shown
 * explicitly, with its reason, and the detail is one click away.
 */
export function NotComputedTag({ status }: { status: NotComputedLike }) {
  const [open, setOpen] = useState(false);
  return (
    <span className="nc">
      <button className="nc-tag" onClick={() => setOpen(!open)} title={status.detail}>
        not computed · {status.reason.replace(/_/g, " ")}
      </button>
      {open && <span className="nc-detail">{status.detail}</span>}
    </span>
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

export function ConfidenceTag({ confidence }: { confidence: Confidence }) {
  return <span className={`confidence ${confidence}`}>{confidence}</span>;
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

/** Significant figures, for numbers whose magnitude is not known in advance. */
export function num(value: number): string {
  if (!Number.isFinite(value)) return "—";
  if (value === 0) return "0";
  const abs = Math.abs(value);
  if (abs >= 1000 || abs < 0.01) return value.toExponential(2);
  return String(round(value, abs < 1 ? 4 : 2));
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
