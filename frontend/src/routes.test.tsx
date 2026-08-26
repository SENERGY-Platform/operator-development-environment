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
import type { Session } from "./api";
import { TOOL_ROUTES, available, findToolRoute } from "./routes";

/**
 * What a path resolves to, and what happens when it resolves to something this
 * deployment does not serve.
 *
 * The route *table* is in `routes.tsx`; the resolution that reads it is `Routed`
 * in `App.tsx`, and the properties worth pinning are properties of the pair. So
 * these tests mount the real application at a real address and read what it put on
 * screen, rather than asserting that a lookup helper returns the record it was
 * given. Only two things are faked, both of them process boundaries: the platform
 * API and Keycloak.
 *
 * The sessions below all set `chat: false` unless the test is about chat, which
 * keeps the conversation pane — a live WebSocket — out of the tree. What is being
 * asserted is which pane the path resolved to, not what the pane then fetched.
 */

vi.mock("./keycloak", () => ({
  initKeycloak: vi.fn(async () => true),
  token: vi.fn(async () => "test-token"),
  logout: vi.fn(),
}));

/** The session the mocked API answers with. Set by each test before it renders. */
let current: Session = session();

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      session: async () => current,
      // Never settles, so the admin pane shows its loading state instead of
      // reaching for the network. The factory is hoisted above every top-level
      // binding in this file, so it has to be written out here.
      adminLimits: () => new Promise<never>(() => {}),
    },
  };
});

type Overrides = Omit<Partial<Session>, "features"> & {
  features?: Partial<Session["features"]>;
};

/**
 * A session, built here rather than taken from `__contract__`.
 *
 * The contract fixtures pin the wire shape against a running backend, which is a
 * different job from this one: these tests need a deployment with one feature off,
 * and mutating a fixture to get there would weaken the thing the fixture is for.
 */
function session(over: Overrides = {}): Session {
  const { features, ...rest } = over;
  return {
    user_id: "u-1",
    username: "dev",
    email: "dev@example.org",
    roles: ["developer"],
    is_admin: false,
    exposure_tier: "L0",
    ...rest,
    features: {
      profiler: false,
      selection: false,
      chat: false,
      mcp: false,
      kernel: false,
      charts: false,
      relations: false,
      repo: false,
      experiments: false,
      ...features,
    },
  };
}

const mounted: Root[] = [];

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
});

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

/** open mounts the application at `address` and returns the text it rendered. */
async function open(address: string, active: Session): Promise<string> {
  current = active;
  window.history.replaceState({}, "", address);
  vi.resetModules();
  const { default: App } = await import("./App");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<App />));
  return host.textContent ?? "";
}

// --- a route whose backend this deployment does not serve ---

/*
 * Degrading by feature is the deployment model, so an absent backend is a state
 * the UI renders rather than an error it hits. A route that threw here would take
 * the whole shell down — including the conversation in the pane beside it — for a
 * configuration key that is missing on purpose.
 */
it("a route whose feature is not configured renders the reason rather than throwing", async () => {
  const text = await open("/tools/profiler", session({ features: { profiler: false } }));

  expect(text).toContain("Not served by this deployment");
  expect(text).toContain("timescale_wrapper_url");
});

// The route says which key is missing because that is what the developer or their
// operator can act on. A blank pane would read as a fault in ODE.
it("every route that names a feature also names what is absent when it is off", () => {
  for (const route of TOOL_ROUTES) {
    if (route.feature === null) continue;
    expect(route.missing, `${route.slug} has no explanation for its absence`).not.toBe("");
  }
});

it("a route whose feature is configured renders its own view", async () => {
  const text = await open("/tools/ontology", session());

  expect(text).not.toContain("Not served by this deployment");
});

// --- an address that names no view ---

/*
 * Answered, not redirected. A wrong URL that lands on the start page teaches the
 * developer that their bookmark was fine and the application lost their place;
 * the card tells them the truth, and the address stays in the bar so they can see
 * what was wrong with it.
 */
it("an unknown path under /tools renders the unknown-path card without moving the address", async () => {
  const text = await open("/tools/no-such-pane", session());

  expect(text).toContain("No such view");
  expect(window.location.pathname).toBe("/tools/no-such-pane");
});

it("an unknown path outside /tools renders the unknown-path card without moving the address", async () => {
  const text = await open("/somewhere/else", session());

  expect(text).toContain("No such view");
  expect(window.location.pathname).toBe("/somewhere/else");
});

it("a slug that names no route resolves to nothing rather than to the first route", () => {
  expect(findToolRoute("no-such-pane")).toBeUndefined();
  expect(findToolRoute("")).toBeUndefined();
});

// --- the admin route ---

/*
 * The gate here is a courtesy, not a control: the backend enforces the realm role
 * on the route (§3.3). What this test protects is that the courtesy keeps saying
 * *which* role is missing — a developer sent to a screen that answers 403 learns
 * only that something is broken.
 */
it("the settings route is refused to an account without the admin realm role", async () => {
  const text = await open("/settings", session({ features: { chat: true }, is_admin: false }));

  expect(text).toContain("admin` realm role");
  expect(text).not.toContain("LLM limits");
});

it("the settings route opens for an account that holds the admin realm role", async () => {
  const text = await open(
    "/settings",
    session({ features: { chat: true }, is_admin: true, roles: ["developer", "admin"] }),
  );

  expect(text).not.toContain("realm role");
  // The admin pane mounted and is waiting on its first read, which is as far as
  // this test cares: the gate let it through.
  expect(text).toContain("Loading");
});

// Without an LLM there is no settings surface to gate, and the reason a developer
// is owed is the provider rather than their own role — telling an admin they are
// not an admin would send them to the wrong person.
it("the settings route names the absent provider rather than the role when no LLM is configured", async () => {
  const text = await open("/settings", session({ is_admin: true }));

  expect(text).toContain("No LLM provider is configured");
  expect(text).not.toContain("realm role");
});

// --- the table itself ---

it("no two routes claim the same slug", () => {
  const slugs = TOOL_ROUTES.map((route) => route.slug);
  expect(new Set(slugs).size).toBe(slugs.length);
});

// `available` is what the menu, the index and the resolver all ask, so a route
// without a feature must be available everywhere rather than nowhere: the ontology
// needs only the device repository, which is required configuration.
it("a route that names no feature is available in every deployment", () => {
  const nothing = session();
  for (const route of TOOL_ROUTES) {
    if (route.feature === null) expect(available(route, nothing)).toBe(true);
  }
});

it("a route that names a feature follows that feature in both directions", () => {
  for (const route of TOOL_ROUTES) {
    if (route.feature === null) continue;
    const off = session();
    const on = session({ features: { [route.feature]: true } });
    expect(available(route, off), `${route.slug} with its feature off`).toBe(false);
    expect(available(route, on), `${route.slug} with its feature on`).toBe(true);
  }
});
