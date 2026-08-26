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

// @vitest-environment jsdom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, expect, it, vi } from "vitest";

/**
 * The router, exercised against a real history and a real address bar.
 *
 * jsdom rather than a fake `window`: `navigate` and `setParam` are a thin layer
 * over `history.pushState`/`replaceState`, and the properties worth asserting —
 * that a filter does not cost a back-button press, that re-navigating to the open
 * view stacks nothing — are properties of the history, not of the module's own
 * bookkeeping. A hand-written double would be asserting that the double agrees
 * with itself.
 *
 * Every test loads its own copy of the module. `BASE` is read from
 * `import.meta.env.BASE_URL` once, when the module is evaluated, so a deployment
 * under `/ode/` cannot be reached by setting a variable afterwards.
 */

type Router = typeof import("./router");

/**
 * load evaluates a fresh router at `address`, served under `base`.
 *
 * `vi.resetModules` clears the source graph but not the externalised
 * node_modules, so the fresh router shares one React instance with the harness
 * below — which is what keeps `useSyncExternalStore` from seeing two copies of
 * React and refusing to run.
 */
async function load(base: string, address: string): Promise<Router> {
  vi.stubEnv("BASE_URL", base);
  window.history.replaceState({}, "", address);
  vi.resetModules();
  return import("./router");
}

const mounted: Root[] = [];

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
  vi.unstubAllEnvs();
});

/**
 * observe mounts the one consumer the location snapshot has: a component.
 *
 * `useLocation` is a `useSyncExternalStore` call, so the parsed pathname is not
 * reachable except through a render. That is deliberate in the router and it is
 * why the base-path tests go through React rather than calling `strip` directly:
 * a private function renamed is not a regression, a path that stops resolving is.
 */
async function observe(router: Router): Promise<() => { pathname: string; search: string }> {
  let latest = { pathname: "", search: "" };

  function Probe() {
    const location = router.useLocation();
    latest = { pathname: location.pathname, search: location.params.toString() };
    return null;
  }

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<Probe />));
  return () => latest;
}

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

/** The full browser address, which is what history assertions are about. */
function address(): string {
  return window.location.pathname + window.location.search;
}

// --- href: the sticky conversation ---

it("an href inherits the open conversation from the address it is written on", async () => {
  const router = await load("/", "/tools/profiler?session=S1");

  expect(router.href("/tools/exploration")).toBe("/tools/exploration?session=S1");
});

// The inherited value is a default, not a rule. `openChart` and the chat pane both
// build links that name a session on purpose; if inheritance won, a link into
// another conversation would silently reopen the current one.
it("a session named by the target wins over the one it would inherit", async () => {
  const router = await load("/", "/chat?session=S1");

  expect(router.href("/chat?session=S2")).toBe("/chat?session=S2");
});

// A `chart` means nothing to the profiler and a `file` means nothing to the
// relational profiler. Carrying view-local state across a view change would leave
// a pane reading a parameter that belongs to the view the developer just left.
it("view-local query state is dropped when a link points at another view", async () => {
  const router = await load("/", "/tools/exploration?session=S1&chart=c-1&file=main.py");

  expect(router.href("/tools/relations")).toBe("/tools/relations?session=S1");
});

it("a link written on an address with nothing sticky carries no query at all", async () => {
  const router = await load("/", "/tools");

  expect(router.href("/tools/ontology")).toBe("/tools/ontology");
});

it("an empty session in the address is not inherited as an empty parameter", async () => {
  const router = await load("/", "/tools?session=");

  expect(router.href("/tools/ontology")).toBe("/tools/ontology");
});

// --- href and strip: the base path ---

it("every href is written under the base the SPA is served from", async () => {
  const router = await load("/ode/", "/ode/chat?session=S1");

  expect(router.href("/tools/profiler")).toBe("/ode/tools/profiler?session=S1");
  expect(router.href("/")).toBe("/ode/?session=S1");
});

// `/ode` and `/ode/` are the same page — a link a colleague retypes without the
// trailing slash must not land on the unknown-path card.
it("the base without its trailing slash resolves to the application root", async () => {
  const router = await load("/ode/", "/ode");
  const location = await observe(router);

  expect(location().pathname).toBe("/");
});

it("the base with its trailing slash resolves to the application root", async () => {
  const router = await load("/ode/", "/ode/");
  const location = await observe(router);

  expect(location().pathname).toBe("/");
});

it("a view under the base resolves to the path the route table is written in", async () => {
  const router = await load("/ode/", "/ode/tools/profiler");
  const location = await observe(router);

  expect(location().pathname).toBe("/tools/profiler");
});

// A trailing slash is the same view. Without this the route table — which spells
// every path without one — would answer `/tools/profiler/` with "no such view".
it("a trailing slash resolves to the same view as the path without one", async () => {
  const router = await load("/ode/", "/ode/tools/profiler/");
  const location = await observe(router);

  expect(location().pathname).toBe("/tools/profiler");
});

it("a trailing slash resolves to the same view when the base is the root", async () => {
  const router = await load("/", "/tools/");
  const location = await observe(router);

  expect(location().pathname).toBe("/tools");
});

// Reporting it as it is, rather than stripping something that is not there, is
// what lets the unknown-path card say which address failed. Rewriting it would
// resolve someone else's path to one of ours and render a view under it.
it("a path outside the base is reported as it is rather than silently rewritten", async () => {
  const router = await load("/ode/", "/somewhere/else");
  const location = await observe(router);

  expect(location().pathname).toBe("/somewhere/else");
});

// --- navigate ---

it("a view change pushes exactly one history entry", async () => {
  const router = await load("/", "/tools");
  const before = window.history.length;

  router.navigate("/tools/ontology");

  expect(address()).toBe("/tools/ontology");
  expect(window.history.length).toBe(before + 1);
});

it("navigating carries the open conversation to the new view", async () => {
  const router = await load("/", "/tools/profiler?session=S1&chart=c-1");

  router.navigate("/tools/exploration");

  expect(address()).toBe("/tools/exploration?session=S1");
});

it("re-navigating to the address already open pushes nothing", async () => {
  const router = await load("/", "/tools/ontology?session=S1");
  const before = window.history.length;

  router.navigate("/tools/ontology");

  expect(window.history.length).toBe(before);
});

/*
 * The property, not the spelling.
 *
 * `href` appends the sticky parameters after whatever the caller named, so a
 * target reached from an address that already holds them comes back with the same
 * parameters in a different order. Compared as strings that is a navigation, and a
 * navigation pushes — the developer collects a back-button step that undoes
 * nothing visible. `openChart` reaches this on the shortest path: opening from
 * chat the chart the address already names.
 *
 * This test asserts that two addresses with the same parameters are the same
 * address whichever order they are written in. It fails against a raw string
 * comparison, which is what stood here.
 */
it("the address already open is recognised whatever order its query is written in", async () => {
  const router = await load("/", "/tools/exploration?session=S1&chart=c-1");
  const before = window.history.length;

  // href would spell this "?chart=c-1&session=S1" — same view, same parameters.
  router.navigate("/tools/exploration?chart=c-1");

  expect(address()).toBe("/tools/exploration?session=S1&chart=c-1");
  expect(
    window.history.length,
    "re-opening the address already on screen must not cost a back-button step",
  ).toBe(before);
});

it("a different value for the same parameter is a navigation rather than a no-op", async () => {
  const router = await load("/", "/tools/exploration?session=S1&chart=c-1");
  const before = window.history.length;

  router.navigate("/tools/exploration?chart=c-2");

  expect(address()).toBe("/tools/exploration?chart=c-2&session=S1");
  expect(window.history.length).toBe(before + 1);
});

// The OAuth callback replaces rather than pushes: the code in its query is
// single-use, and an entry the back button can return to is an entry that replays
// a spent code.
it("a replacing navigation moves without adding a history entry", async () => {
  const router = await load("/", "/github/callback?code=abc&state=xyz");
  const before = window.history.length;

  router.navigate("/", { replace: true });

  expect(address()).toBe("/");
  expect(window.history.length).toBe(before);
});

// --- setParam ---

// Twenty filter changes must not cost twenty presses of the back button to escape
// the view. This is the whole reason query state replaces and view changes push.
it("setting a view-local parameter replaces the entry rather than pushing one", async () => {
  const router = await load("/", "/tools/profiler?session=S1");
  const before = window.history.length;

  router.setParam("device", "d-1");

  expect(address()).toBe("/tools/profiler?session=S1&device=d-1");
  expect(window.history.length).toBe(before);
});

// Reading from the live URL rather than from the React snapshot is what makes this
// hold: the snapshot the second call would see is the one taken before the first,
// so a handler that sets two parameters would keep only the last.
it("two parameters set in one handler both survive", async () => {
  const router = await load("/", "/tools/profiler");

  router.setParam("device", "d-1");
  router.setParam("service", "s-1");

  const params = new URLSearchParams(window.location.search);
  expect(params.get("device")).toBe("d-1");
  expect(params.get("service")).toBe("s-1");
});

it("setting a parameter to null removes it and leaves the rest standing", async () => {
  const router = await load("/", "/tools/profiler?session=S1&device=d-1");

  router.setParam("device", null);

  expect(address()).toBe("/tools/profiler?session=S1");
});

it("setting a parameter to the empty string removes it rather than storing a blank", async () => {
  const router = await load("/", "/tools/profiler?device=d-1");

  router.setParam("device", "");

  expect(address()).toBe("/tools/profiler");
});

it("setting a parameter never moves the view", async () => {
  const router = await load("/ode/", "/ode/tools/profiler?session=S1");

  router.setParam("device", "d-1");

  expect(window.location.pathname).toBe("/ode/tools/profiler");
});

// A no-op write must not replace the entry either: `setParam` is called from
// render-adjacent handlers, and an unconditional replaceState there is a write per
// keystroke against the address bar.
it("writing the value a parameter already has changes nothing", async () => {
  const router = await load("/", "/tools/profiler?device=d-1");
  const before = address();

  router.setParam("device", "d-1");

  expect(address()).toBe(before);
});

// --- what the location reports back ---

/**
 * observeParam mounts a consumer of `useParam`, the way a view reads its own
 * query state.
 */
async function observeParam(router: Router, name: string): Promise<() => string | null> {
  let latest: string | null = null;

  function Probe() {
    latest = router.useParam(name);
    return null;
  }

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<Probe />));
  return () => latest;
}

// A parameter written as `?device=` is the developer having cleared a filter, not
// having selected a device called "". Every view branches on null, so an empty
// string reaching one is a filter that matches nothing rather than everything.
it("a parameter that is present but empty reads as absent", async () => {
  const router = await load("/", "/tools/profiler?device=&session=S1");
  const device = await observeParam(router, "device");

  expect(device()).toBeNull();
});

it("a parameter that is set reads back as its value", async () => {
  const router = await load("/", "/tools/profiler?device=d-1");
  const device = await observeParam(router, "device");

  expect(device()).toBe("d-1");
});

// The rendered href has to follow the address, not the render that produced it:
// a link copied after the conversation was opened must carry it.
it("a mounted consumer sees the address a navigation moved to", async () => {
  const router = await load("/", "/tools");
  const location = await observe(router);

  await act(async () => router.navigate("/tools/exploration?chart=c-1"));

  expect(location()).toEqual({ pathname: "/tools/exploration", search: "chart=c-1" });
});
