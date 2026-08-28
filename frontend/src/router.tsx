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

import { useSyncExternalStore } from "react";

/**
 * A router, hand-rolled, because the alternative is a fifth runtime dependency.
 *
 * The SPA carries four — react, react-dom, keycloak-js, monaco-editor — and what
 * it needs from a router is a path, a query, a link that behaves like a link, and
 * the back button. react-router would bring loaders, actions, nested outlets and
 * a data layer this application already has in `useLoad`. The whole of what is
 * needed is below.
 *
 * Three things here are decisions rather than mechanics:
 *
 *   - **A `<Link>` renders a real `<a href>`.** Middle-click, ctrl-click and "copy
 *     link address" are how developers actually use an instrumentation UI, and a
 *     `<button>` styled as a link breaks all three silently.
 *   - **Query state replaces, view changes push.** Tweaking a filter twenty times
 *     should not cost twenty presses of the back button to escape; moving from the
 *     profiler to the chart should cost exactly one.
 *   - **Some query state is sticky.** The open conversation and the workbench it
 *     is about follow the developer across every view, so they are carried by
 *     `navigate` and written into every link's href — including the href a
 *     middle-click opens in a new tab.
 */

/**
 * Query parameters carried across a view change.
 *
 * `session` is the open conversation. Chat sits beside every instrumentation view
 * (§2's pane layout), so moving from the profiler to the exploration pane must not
 * close the conversation that sent you there.
 *
 * `workbench` is the working context those views act in: which checkout a file read
 * is answered from, which kernel `run_code` runs in, which commit an experiment is
 * launched from. It travels for the same reason, and it has to: a developer with
 * two open who arrives at a view without it does not get an error, they get the
 * first workbench — the SPA picks one so that the backend is never asked to guess
 * between two working copies. Dropped on the way to the kernel pane, that ran their
 * code in the other operator's checkout; dropped on the way back to the workspace,
 * it put the other operator's repository beside the conversation they were reading.
 *
 * Everything else is view-local and is deliberately dropped on navigation: a `file`
 * is a path inside one checkout and means nothing to the relational profiler.
 */
const STICKY = ["session", "workbench"] as const;

/**
 * The base the SPA is served under, always with a trailing slash.
 *
 * Vite substitutes this at build time, so a deployment under `/ode/` gets working
 * links without a runtime setting. Internally every path is base-relative and
 * starts with `/`; the base is added on the way out and stripped on the way in, in
 * exactly these two functions.
 */
const BASE = normaliseBase(import.meta.env.BASE_URL);

function normaliseBase(raw: string | undefined): string {
  if (!raw || raw === "/") return "/";
  return raw.endsWith("/") ? raw : `${raw}/`;
}

/** Strips the base from a browser pathname. Always returns a path starting with `/`. */
function strip(pathname: string): string {
  if (BASE === "/") return pathname || "/";
  // The base without its trailing slash is a valid location for the app root:
  // `/ode` and `/ode/` are the same page.
  const trimmed = BASE.slice(0, -1);
  if (pathname === trimmed) return "/";
  if (pathname.startsWith(BASE)) return `/${pathname.slice(BASE.length)}`;
  // Outside the base entirely. Nothing sensible to resolve, so report it as it is
  // and let the unknown-path card say so rather than silently rewriting it.
  return pathname;
}

export interface Location {
  /** Base-relative, leading slash, no trailing slash except for the root. */
  pathname: string;
  params: URLSearchParams;
}

function read(): Location {
  const pathname = strip(window.location.pathname);
  return {
    // A trailing slash is the same view, so `/tools/` resolves like `/tools`
    // rather than falling through to the unknown-path card.
    pathname: pathname.length > 1 ? pathname.replace(/\/+$/, "") : "/",
    params: new URLSearchParams(window.location.search),
  };
}

// One snapshot object shared by every subscriber. useSyncExternalStore compares
// snapshots by identity, so it is rebuilt only when the URL actually changes —
// rebuilding it per read would re-render every consumer on every render.
let snapshot: Location = read();
const listeners = new Set<() => void>();

function emit(): void {
  snapshot = read();
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

// popstate covers back, forward and the history entries the browser restores on
// reload. pushState and replaceState do not fire it, so `navigate` emits itself.
window.addEventListener("popstate", emit);

/**
 * refresh re-reads the URL.
 *
 * The first snapshot is taken when this module loads, which is before
 * keycloak-js has finished with the address. It cleans its own response
 * parameters out with replaceState, and although it puts them in the fragment by
 * default — which nothing here reads — a deployment that configured the query
 * response mode would leave this module holding a search string that no longer
 * exists. One call after authentication settles costs nothing and removes the
 * question.
 */
export function refresh(): void {
  emit();
}

export function useLocation(): Location {
  return useSyncExternalStore(subscribe, () => snapshot);
}

/**
 * href turns a base-relative target into the URL a browser can follow.
 *
 * The sticky parameters are merged in here rather than in `navigate` so that the
 * `<a href>` a developer copies, middle-clicks or opens in a new window already
 * carries them. A target that names a sticky parameter itself wins — it is being
 * explicit about something the caller only inherits by default.
 */
export function href(to: string): string {
  const cut = to.indexOf("?");
  const path = cut === -1 ? to : to.slice(0, cut);
  const params = new URLSearchParams(cut === -1 ? "" : to.slice(cut + 1));

  const current = new URLSearchParams(window.location.search);
  for (const name of STICKY) {
    if (params.has(name)) continue;
    const inherited = current.get(name);
    if (inherited) params.set(name, inherited);
  }

  const query = params.toString();
  return `${BASE}${path.replace(/^\//, "")}${query ? `?${query}` : ""}`;
}

/**
 * sameAddress asks whether two URLs name the same view, rather than whether they
 * are spelled the same way.
 *
 * They routinely are not. `href` appends the sticky parameters after whatever the
 * caller named, so `navigate("/tools/profiler?q=x")` from an address that already
 * reads `?session=S&q=x` produces `?q=x&session=S` — the same view, the same
 * parameters, a different string. Compared raw, that is a navigation, and a
 * navigation pushes: the developer collects a back-button step that undoes
 * nothing visible. `openChart` reaches it by the shortest path — opening the chart
 * the address already names, from a chat result rather than from the list.
 *
 * Sorting is enough because a query is a set here: no view reads a repeated
 * parameter, and `URLSearchParams.sort` is stable, so a duplicate that did appear
 * keeps its relative order and still compares correctly.
 */
function sameAddress(a: string, b: string): boolean {
  return canonical(a) === canonical(b);
}

function canonical(url: string): string {
  const cut = url.indexOf("?");
  if (cut === -1) return url;
  const params = new URLSearchParams(url.slice(cut + 1));
  params.sort();
  const query = params.toString();
  return `${url.slice(0, cut)}${query ? `?${query}` : ""}`;
}

/**
 * navigate moves to another view.
 *
 * It pushes by default: a view change is a step a developer expects the back
 * button to undo. `replace` is for the cases where the previous URL should not be
 * returnable — the OAuth callback, whose query holds a code that must not be
 * replayable from history.
 */
export function navigate(to: string, options: { replace?: boolean } = {}): void {
  const url = href(to);
  // Re-navigating to where we already are would stack identical history entries:
  // a nav entry clicked twice should not cost two presses of the back button.
  if (sameAddress(url, window.location.pathname + window.location.search)) return;
  if (options.replace) window.history.replaceState({}, "", url);
  else window.history.pushState({}, "", url);
  emit();
}

/** useParam reads one query parameter. Null when it is absent or empty. */
export function useParam(name: string): string | null {
  const { params } = useLocation();
  return params.get(name) || null;
}

/**
 * getParam reads one query parameter outside a render.
 *
 * A component reads the URL with `useParam`, which re-renders it when the URL
 * moves. This is for the handlers that have to compare against the *live* address
 * before writing it — the same reason `setParam` below reads
 * `window.location.search` rather than the snapshot.
 */
export function getParam(name: string): string | null {
  return new URLSearchParams(window.location.search).get(name) || null;
}

/**
 * setParam writes one query parameter, replacing the current history entry.
 *
 * Replacing rather than pushing is the point: a filter, an open file, a selected
 * chart are things a developer changes repeatedly while looking at one view, and
 * pushing each one would bury the view they arrived from under a stack of
 * near-identical URLs. The cost is that the back button does not undo a filter —
 * which is the trade this codebase wants, because the filter is visible on screen
 * and the previous view is not.
 *
 * Passing null removes the parameter.
 */
export function setParam(name: string, value: string | null): void {
  // Read from the live URL rather than from the snapshot: two setParam calls in
  // one event handler must both survive, and replaceState is synchronous, so the
  // second call sees the first.
  const params = new URLSearchParams(window.location.search);
  if (value === null || value === "") params.delete(name);
  else params.set(name, value);
  const query = params.toString();
  const url = `${window.location.pathname}${query ? `?${query}` : ""}`;
  if (url === window.location.pathname + window.location.search) return;
  window.history.replaceState({}, "", url);
  emit();
}

/**
 * Link is an anchor that navigates without a page load.
 *
 * Only a plain left click is intercepted. A modified click means the developer
 * asked the browser for something — a new tab, a new window, a saved target — and
 * the browser does that better than any handler could.
 */
export function Link({
  to,
  replace,
  children,
  onNavigate,
  ...rest
}: {
  to: string;
  replace?: boolean;
  children: React.ReactNode;
  /** Runs after a same-document navigation only, e.g. to close a menu. */
  onNavigate?: () => void;
} & Omit<React.AnchorHTMLAttributes<HTMLAnchorElement>, "href">) {
  // Subscribing to the location keeps the rendered href current when a sticky
  // parameter changes, so a link copied after opening a conversation carries it.
  useLocation();

  return (
    <a
      {...rest}
      // After the spread, so a caller cannot displace the destination.
      href={href(to)}
      onClick={(event) => {
        rest.onClick?.(event);
        if (event.defaultPrevented) return;
        // button 0 is the plain left click. Middle-click does not reach onClick in
        // React, but a pointer device that reports otherwise should still be let
        // through rather than swallowed.
        if (event.button !== 0) return;
        if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
        if (rest.target && rest.target !== "_self") return;
        event.preventDefault();
        navigate(to, { replace });
        onNavigate?.();
      }}
    >
      {children}
    </a>
  );
}
