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
import type { Session } from "./api";
import { logout } from "./keycloak";
import { Link, useLocation } from "./router";
import { TOOL_ROUTES, available } from "./routes";
import { useTheme } from "./theme";

/**
 * The shell: the header, the "Under the hood" menu, and the split.
 *
 * The information architecture it expresses is the brief of §2 read literally.
 * The developer's loop is chat and code — the assistant writes files, the
 * developer reads and corrects them — so that pair is the screen. Everything else
 * is instrumentation the assistant drives through the tool surface, and a
 * developer opens to audit it or to do the same thing by hand. Instrumentation
 * behind a menu is not instrumentation hidden; it is instrumentation that is not
 * competing with the work for the same row of pixels.
 */

// --- the header ---

export function Header({ session }: { session: Session }) {
  const { pathname } = useLocation();
  const underTheHood = pathname === "/tools" || pathname.startsWith("/tools/") || pathname === "/settings";

  return (
    <header className="header">
      <div className="header-left">
        <span className="brand">ODE</span>
        <nav className="nav" aria-label="Primary">
          {/*
            "Workspace" rather than "Code": the code pane has no route of its own —
            it is the right half of the workspace, beside the conversation that
            writes into it. A nav entry named after the pane would name a
            destination that does not exist.
          */}
          <Link className="nav-entry" to="/" aria-current={pathname === "/" ? "page" : undefined}>
            Workspace
          </Link>
          {session.features.chat && (
            <Link
              className="nav-entry"
              to="/chat"
              aria-current={pathname === "/chat" ? "page" : undefined}
            >
              Chat
            </Link>
          )}
          <UnderTheHood session={session} current={underTheHood} />
        </nav>
      </div>
      <div className="header-right">
        {/*
          SPEC §3.2: the exposure tier is surfaced persistently. What appears here is
          the default a *new* chat session starts at, plus the ceiling this developer
          may raise one to — a live tier is session-scoped and belongs beside the
          conversation it governs, which is where the chat view puts it.
        */}
        <span
          className="tier"
          title={
            "Data exposure tier for the LLM (SPEC §3.2). It gates LLM tools, not this UI. " +
            "New sessions start here; the live tier is shown in the chat pane."
          }
        >
          New sessions: {session.exposure_tier}
          {session.max_exposure_tier && session.max_exposure_tier !== "L2" && (
            <span className="tier-cap"> (max {session.max_exposure_tier})</span>
          )}
        </span>
        {session.spend && (
          <span className="spend" title="Estimated LLM spend this period (SPEC §3.3)">
            {session.spend.tokens.toLocaleString("en-GB")} tokens
          </span>
        )}
        <span className="user">
          {session.username}
          {session.is_admin && <span className="badge">admin</span>}
        </span>
        <ThemeToggle />
        <button onClick={logout}>Sign out</button>
      </div>
    </header>
  );
}

/**
 * The light/dark control.
 *
 * One button that switches, plus a way back to "follow the system" — which is the
 * state a developer starts in and cannot otherwise return to once they have
 * pressed anything. The second affordance is the title and a shift-click rather
 * than a third button: reverting to the system setting is a thing people do once,
 * and a three-state segmented control in the header would cost more room than the
 * case deserves.
 */
function ThemeToggle() {
  const { choice, active, toggle, clear } = useTheme();
  const next = active === "dark" ? "light" : "dark";

  return (
    <button
      className="theme-toggle"
      onClick={(event) => {
        if (event.shiftKey && choice !== null) clear();
        else toggle();
      }}
      aria-label={`Switch to the ${next} theme`}
      title={
        choice === null
          ? `Following your system setting (${active}). Click for ${next}.`
          : `Theme set to ${choice}. Click for ${next}; shift-click to follow your system setting.`
      }
    >
      <span className="theme-toggle-icon" aria-hidden="true">
        {active === "dark" ? "\u25D1" : "\u25D0"}
      </span>
      <span className="theme-toggle-label">{active === "dark" ? "Dark" : "Light"}</span>
      {/* A dot marks "this was chosen", so the button can be told apart from one
          that is merely reflecting the desktop. */}
      {choice !== null && <span className="theme-toggle-pinned" aria-hidden="true" />}
    </button>
  );
}

/**
 * The instrumentation menu.
 *
 * Keyboard-operable because it is a menu and a menu that only works with a mouse
 * is a menu half the entries are unreachable in. Every entry is a real link, so
 * the keyboard support is in addition to middle-click and "copy link address"
 * rather than instead of them.
 */
function UnderTheHood({ session, current }: { session: Session; current: boolean }) {
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const container = useRef<HTMLDivElement | null>(null);
  const { pathname } = useLocation();

  const entries = TOOL_ROUTES;
  const settings = session.is_admin && session.features.chat;

  const close = useCallback((restoreFocus: boolean) => {
    setOpen(false);
    if (restoreFocus) trigger.current?.focus();
  }, []);

  // A menu that survives the navigation it caused would sit over the view it just
  // opened. Closing on the path change covers every route into the menu — a click,
  // Enter, and the back button landing somewhere else.
  useEffect(() => {
    setOpen(false);
  }, [pathname]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      if (container.current && !container.current.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  /** Moves focus within the open menu. Wraps, because a menu is a ring. */
  const move = (from: HTMLElement, delta: number | "first" | "last") => {
    const items = Array.from(
      container.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [],
    );
    if (items.length === 0) return;
    const index =
      delta === "first"
        ? 0
        : delta === "last"
          ? items.length - 1
          : (items.indexOf(from) + delta + items.length) % items.length;
    items[index]?.focus();
  };

  const openWith = (which: "first" | "last") => {
    setOpen(true);
    // After the menu has rendered. requestAnimationFrame rather than a timeout
    // because the only thing being waited for is the paint that creates the items.
    requestAnimationFrame(() => {
      const items = container.current?.querySelectorAll<HTMLElement>('[role="menuitem"]');
      if (!items || items.length === 0) return;
      (which === "first" ? items[0] : items[items.length - 1]).focus();
    });
  };

  return (
    <div
      className="menu-host"
      ref={container}
      onKeyDown={(event) => {
        if (event.key === "Escape" && open) {
          event.preventDefault();
          close(true);
        }
      }}
    >
      <button
        ref={trigger}
        className={current ? "nav-entry menu-trigger active" : "nav-entry menu-trigger"}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls="under-the-hood-menu"
        onClick={() => setOpen(!open)}
        onKeyDown={(event) => {
          if (event.key === "ArrowDown") {
            event.preventDefault();
            openWith("first");
          } else if (event.key === "ArrowUp") {
            event.preventDefault();
            openWith("last");
          }
        }}
      >
        Under the hood
        <span className="menu-caret" aria-hidden="true">
          ▾
        </span>
      </button>

      {open && (
        <div className="menu" id="under-the-hood-menu" role="menu" aria-label="Under the hood">
          <Link
            className="menu-item"
            role="menuitem"
            to="/tools"
            aria-current={pathname === "/tools" ? "page" : undefined}
            onKeyDown={(event) => onMenuKey(event, move)}
            onNavigate={() => close(false)}
          >
            <span className="menu-item-label">All views</span>
            <span className="menu-item-note">the index, with what each one is for</span>
          </Link>
          <hr className="menu-rule" />
          {entries.map((route) => {
            const served = available(route, session) && !route.unbuilt;
            const path = `/tools/${route.slug}`;
            return (
              <Link
                key={route.slug}
                className={served ? "menu-item" : "menu-item unserved"}
                role="menuitem"
                to={path}
                aria-current={pathname === path ? "page" : undefined}
                onKeyDown={(event) => onMenuKey(event, move)}
                onNavigate={() => close(false)}
              >
                <span className="menu-item-label">{route.label}</span>
                <span className="menu-item-note">
                  {route.unbuilt
                    ? "reserved — not built yet"
                    : served
                      ? route.tools.join(", ")
                      : "not configured in this deployment"}
                </span>
              </Link>
            );
          })}
          {settings && (
            <>
              <hr className="menu-rule" />
              <Link
                className="menu-item"
                role="menuitem"
                to="/settings"
                aria-current={pathname === "/settings" ? "page" : undefined}
                onKeyDown={(event) => onMenuKey(event, move)}
                onNavigate={() => close(false)}
              >
                <span className="menu-item-label">Settings</span>
                <span className="menu-item-note">LLM limits, pricing, accounting, tool audit</span>
              </Link>
            </>
          )}
        </div>
      )}
    </div>
  );
}

function onMenuKey(
  event: React.KeyboardEvent<HTMLElement>,
  move: (from: HTMLElement, delta: number | "first" | "last") => void,
) {
  const from = event.currentTarget;
  if (event.key === "ArrowDown") {
    event.preventDefault();
    move(from, 1);
  } else if (event.key === "ArrowUp") {
    event.preventDefault();
    move(from, -1);
  } else if (event.key === "Home") {
    event.preventDefault();
    move(from, "first");
  } else if (event.key === "End") {
    event.preventDefault();
    move(from, "last");
  }
}

// --- the split ---

/** Neither side is useful below this, so the drag stops here rather than at zero. */
const MIN_SIDE_PX = 320;
/** Below this the two columns stop being two columns and become a choice of one. */
const NARROW = "(max-width: 900px)";
const STORAGE_KEY = "ode.split";

type Mode = "split" | "left" | "right";

interface Stored {
  /** The left side's share of the split, in percent. */
  width: number;
  mode: Mode;
}

/**
 * The width and which side is showing are remembered locally rather than put in
 * the URL.
 *
 * A URL is a thing you send to someone. Carrying the divider position in it would
 * mean that "look at this profile" also said "and read it in a column two thirds
 * of the way across, with the conversation collapsed" — a preference of the
 * sender's screen, imposed on the recipient's. The state the URL carries is what
 * is being looked at; how it is arranged is per-developer and belongs here.
 *
 * localStorage throws in some contexts — a browser configured to block site data,
 * a sandboxed frame — and a layout preference is not worth a blank screen.
 */
function readStored(): Stored {
  const fallback: Stored = { width: 42, mode: "split" };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return fallback;
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return fallback;
    const record = parsed as Partial<Stored>;
    const width = typeof record.width === "number" && Number.isFinite(record.width) ? record.width : fallback.width;
    const mode: Mode =
      record.mode === "left" || record.mode === "right" || record.mode === "split"
        ? record.mode
        : fallback.mode;
    return { width: Math.min(90, Math.max(10, width)), mode };
  } catch {
    return fallback;
  }
}

function writeStored(value: Stored): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(value));
  } catch {
    // A preference that cannot be saved is still a preference for this tab.
  }
}

/**
 * Split puts the conversation beside a view, with a divider the developer owns.
 *
 * Both sides stay mounted when one is hidden. A conversation is a live stream and
 * a Monaco model carries the undo history of the file in it; unmounting either to
 * save a few nodes would throw away the thing the developer was in the middle of.
 */
export function Split({
  left,
  right,
  leftLabel,
  rightLabel,
}: {
  left: React.ReactNode;
  right: React.ReactNode;
  leftLabel: string;
  rightLabel: string;
}) {
  const [stored, setStored] = useState<Stored>(readStored);
  const [narrow, setNarrow] = useState(() => window.matchMedia?.(NARROW).matches ?? false);
  const frame = useRef<HTMLDivElement | null>(null);
  const separator = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const query = window.matchMedia?.(NARROW);
    if (!query) return;
    const update = () => setNarrow(query.matches);
    query.addEventListener("change", update);
    return () => query.removeEventListener("change", update);
  }, []);

  const set = useCallback((next: Stored) => {
    setStored(next);
    writeStored(next);
  }, []);

  /*
   * Narrow means one at a time, and which one is deliberately not persisted.
   *
   * "split" is kept in storage rather than rewritten, so widening the window gives
   * the split back instead of leaving the developer on whichever side the
   * phone-sized layout happened to pick. Chat is the one it picks to begin with: it
   * is where the loop starts, and the other side is one button away.
   *
   * That only holds while pressing the button does not write. Below the breakpoint
   * a toggle is the *only* way to see the other pane, so the first press is not a
   * preference — it is the developer asking to look at the thing the window is too
   * small to show beside the conversation. Storing it turned "show me the code for
   * a moment on a laptop" into "open with the conversation collapsed on the 1600px
   * monitor, from now on". So the narrow choice lives in state that dies with the
   * tab, and `stored.mode` keeps saying what was chosen at a width where both panes
   * fit.
   */
  const [narrowSide, setNarrowSide] = useState<Mode | null>(null);
  const mode: Mode = narrow
    ? (narrowSide ?? (stored.mode === "split" ? "left" : stored.mode))
    : stored.mode;

  const clamp = useCallback((percent: number): number => {
    const width = frame.current?.getBoundingClientRect().width ?? 0;
    if (width < MIN_SIDE_PX * 2) return percent;
    const min = (MIN_SIDE_PX / width) * 100;
    return Math.min(100 - min, Math.max(min, percent));
  }, []);

  /*
   * The stored share is a percentage and the minimum is a width, so the two can
   * only be reconciled against a real layout.
   *
   * Without this the minimum would hold only while the divider is being dragged: a
   * window narrower than the one the preference was saved on would open — or be
   * resized into — a pane too thin to read. Not written back to storage, because
   * this is the window being small rather than the developer changing their mind.
   * The observer fires once on observe, which covers the first layout too.
   */
  useEffect(() => {
    const element = frame.current;
    if (!element || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(() => {
      setStored((current) => {
        const fitted = clamp(current.width);
        return fitted === current.width ? current : { ...current, width: fitted };
      });
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, [clamp]);

  const drag = (event: React.PointerEvent<HTMLDivElement>) => {
    if (mode !== "split") return;
    const frameRect = frame.current?.getBoundingClientRect();
    if (!frameRect) return;
    // React nulls `currentTarget` once the handler returns, so the element and the
    // rectangle are both taken now rather than read from the event later.
    const handle = event.currentTarget;
    handle.setPointerCapture(event.pointerId);

    let latest = stored.width;
    const move = (moved: PointerEvent) => {
      latest = clamp(((moved.clientX - frameRect.left) / frameRect.width) * 100);
      setStored((current) => ({ ...current, width: latest }));
    };
    const up = () => {
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", up);
      handle.removeEventListener("pointercancel", up);
      // Written once at the end rather than on every move: a drag is one decision,
      // not two hundred writes to local storage.
      writeStored({ ...stored, width: latest });
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", up);
    handle.addEventListener("pointercancel", up);
  };

  const nudge = (event: React.KeyboardEvent<HTMLDivElement>) => {
    // Shift for a coarse step, which is what makes the keyboard a usable way to
    // cross the pane rather than only to fine-tune it.
    const step = event.shiftKey ? 10 : 2;
    if (event.key === "ArrowLeft") set({ ...stored, width: clamp(stored.width - step) });
    else if (event.key === "ArrowRight") set({ ...stored, width: clamp(stored.width + step) });
    else if (event.key === "Home") set({ ...stored, width: clamp(0) });
    else if (event.key === "End") set({ ...stored, width: clamp(100) });
    else return;
    event.preventDefault();
  };

  const show = (next: Mode) => {
    if (narrow) {
      // Held, not saved — see the note on `narrowSide`. The buttons below never
      // pass "split" while narrow, and the separator is not rendered there either,
      // so there is nothing to move focus to.
      setNarrowSide(next);
      return;
    }
    // A choice made where both panes fit supersedes any narrow one, so the next
    // trip below the breakpoint starts from the stored preference again rather than
    // from a side picked on a phone-sized window three screens ago.
    setNarrowSide(null);
    set({ ...stored, mode: next });
    // Focus would otherwise stay on a button that has just changed meaning.
    if (next === "split") requestAnimationFrame(() => separator.current?.focus());
  };

  return (
    <div
      className="split"
      data-mode={mode}
      ref={frame}
      style={{ "--split-left": `${stored.width}%` } as React.CSSProperties}
    >
      <section className="split-side split-left" id="split-left" aria-label={leftLabel}>
        {left}
      </section>

      <div className="split-gutter">
        {mode === "split" && !narrow && (
          <div
            className="split-separator"
            ref={separator}
            role="separator"
            tabIndex={0}
            aria-orientation="vertical"
            aria-label={`Width of the ${leftLabel} pane`}
            aria-controls="split-left"
            aria-valuenow={Math.round(stored.width)}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuetext={`${Math.round(stored.width)} percent`}
            onPointerDown={drag}
            onKeyDown={nudge}
            onDoubleClick={() => set({ ...stored, width: 42 })}
            title="Drag, or use the arrow keys. Double-click to reset."
          />
        )}
        {/*
          Two toggles rather than one. Each says whether its own pane is showing,
          which is what aria-pressed can express; a single "swap" button could not,
          and would have no answer for the state where both are up. The arrow points
          the way the pane goes, and the label is spelled out below the breakpoint
          where the strip becomes a bar and there is room for words.
        */}
        <div className="split-buttons">
          <button
            className={mode === "right" ? "split-toggle" : "split-toggle on"}
            aria-pressed={mode !== "right"}
            aria-label={`${mode === "right" ? "Show" : "Hide"} ${leftLabel}`}
            title={`${mode === "right" ? "Show" : "Hide"} ${leftLabel}`}
            onClick={() => show(mode === "right" ? (narrow ? "left" : "split") : "right")}
          >
            <span className="split-toggle-glyph" aria-hidden="true">
              {mode === "right" ? "▶" : "◀"}
            </span>
            <span className="split-toggle-label">{leftLabel}</span>
          </button>
          <button
            className={mode === "left" ? "split-toggle" : "split-toggle on"}
            aria-pressed={mode !== "left"}
            aria-label={`${mode === "left" ? "Show" : "Hide"} ${rightLabel}`}
            title={`${mode === "left" ? "Show" : "Hide"} ${rightLabel}`}
            onClick={() => show(mode === "left" ? (narrow ? "right" : "split") : "left")}
          >
            <span className="split-toggle-glyph" aria-hidden="true">
              {mode === "left" ? "◀" : "▶"}
            </span>
            <span className="split-toggle-label">{rightLabel}</span>
          </button>
        </div>
      </div>

      <section className="split-side split-right" id="split-right" aria-label={rightLabel}>
        {right}
      </section>
    </div>
  );
}
