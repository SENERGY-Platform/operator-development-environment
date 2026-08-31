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

import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Abandoned, CALLBACK_MESSAGE, reconnect, relayAuthorisation } from "./github";

/**
 * The reconnection flow, from both ends of the popup.
 *
 * Worth testing rather than eyeballing because the two halves run in two windows and
 * neither is on screen for long: the popup relays and closes, the tab spends the code
 * and retries what the developer asked for. The failure mode is silence — a popup
 * that hands its answer nowhere, or a tab that waits for a message that already
 * arrived — and neither shows up as an error anywhere.
 */

const authorize = vi.fn();
const connect = vi.fn();

vi.mock("./api", () => ({
  api: {
    repoAuthorize: () => authorize(),
    repoConnect: (code: string, state: string) => connect(code, state),
  },
}));

/** A popup, as much of one as this needs: it is opened, watched and closed. */
function fakePopup() {
  return { closed: false, close: vi.fn() } as unknown as Window & { closed: boolean };
}

beforeEach(() => {
  authorize.mockReset();
  connect.mockReset();
  authorize.mockResolvedValue({ url: "https://github.test/login/oauth/authorize", state: "st-1" });
  connect.mockResolvedValue(undefined);
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
});

afterEach(() => {
  vi.unstubAllGlobals();
  Object.defineProperty(window, "opener", { value: null, configurable: true, writable: true });
});

// --- the popup's half ---

it("relays the authorisation to the tab that opened it, and closes", () => {
  const post = vi.fn();
  Object.defineProperty(window, "opener", {
    value: { postMessage: post },
    configurable: true,
    writable: true,
  });
  const close = vi.fn();
  window.close = close;
  window.history.replaceState({}, "", "/github/callback?code=abc&state=st-1");

  expect(relayAuthorisation()).toBe(true);
  expect(post).toHaveBeenCalledWith(
    { ode: CALLBACK_MESSAGE, code: "abc", state: "st-1", error: "" },
    window.location.origin,
  );
  expect(close).toHaveBeenCalled();
  // The window says something, because a browser that refuses to close it would
  // otherwise leave a blank page on screen.
  expect(document.body.textContent).toContain("close this window");
});

it("relays GitHub's refusal too, rather than leaving the tab waiting for a code", () => {
  const post = vi.fn();
  Object.defineProperty(window, "opener", {
    value: { postMessage: post },
    configurable: true,
    writable: true,
  });
  window.close = vi.fn();
  window.history.replaceState({}, "", "/github/callback?error=access_denied");

  expect(relayAuthorisation()).toBe(true);
  expect(post.mock.calls[0]?.[0]).toMatchObject({ error: "access_denied", code: "" });
});

/*
 * A callback that took the whole tab — the connect card's flow, and the fallback when
 * a popup is blocked — must fall through to the application, which completes it. The
 * check is the opener, not the path: the same address means two different things
 * depending on which window it loaded in.
 */
it("leaves a full-tab callback to the application", () => {
  window.history.replaceState({}, "", "/github/callback?code=abc&state=st-1");
  expect(relayAuthorisation()).toBe(false);
});

it("ignores an ordinary load, opener or not", () => {
  Object.defineProperty(window, "opener", {
    value: { postMessage: vi.fn() },
    configurable: true,
    writable: true,
  });
  window.history.replaceState({}, "", "/?session=s-1");
  expect(relayAuthorisation()).toBe(false);
});

// --- the tab's half ---

it("stores the credential the popup came back with", async () => {
  const popup = fakePopup();
  vi.stubGlobal("open", vi.fn(() => popup));

  const flow = reconnect();
  // A turn, so the listener is attached before the answer arrives.
  await Promise.resolve();
  window.dispatchEvent(
    new MessageEvent("message", {
      origin: window.location.origin,
      data: { ode: CALLBACK_MESSAGE, code: "abc", state: "st-1", error: "" },
    }),
  );
  await flow;

  expect(connect).toHaveBeenCalledWith("abc", "st-1");
  // And the state is spent: a second answer carrying it must not be accepted.
  expect(sessionStorage.getItem("ode.github.state")).toBeNull();
});

it("refuses an answer whose state ODE did not ask for", async () => {
  const popup = fakePopup();
  vi.stubGlobal("open", vi.fn(() => popup));

  const flow = reconnect();
  await Promise.resolve();
  window.dispatchEvent(
    new MessageEvent("message", {
      origin: window.location.origin,
      data: { ode: CALLBACK_MESSAGE, code: "abc", state: "somebody-elses", error: "" },
    }),
  );

  await expect(flow).rejects.toThrow(/state ODE did not ask for/);
  expect(connect).not.toHaveBeenCalled();
});

it("ignores a message from another origin", async () => {
  const popup = fakePopup();
  vi.stubGlobal("open", vi.fn(() => popup));

  const flow = reconnect();
  await Promise.resolve();
  window.dispatchEvent(
    new MessageEvent("message", {
      origin: "https://not-ode.test",
      data: { ode: CALLBACK_MESSAGE, code: "abc", state: "st-1", error: "" },
    }),
  );
  // Nothing was accepted, so the flow is still waiting. Closing the popup is what
  // ends it, and it ends as abandonment rather than as a connection.
  popup.closed = true;
  await expect(flow).rejects.toBeInstanceOf(Abandoned);
  expect(connect).not.toHaveBeenCalled();
});

it("reports a closed window as a decision rather than a failure", async () => {
  const popup = fakePopup();
  vi.stubGlobal("open", vi.fn(() => popup));

  const flow = reconnect();
  await Promise.resolve();
  popup.closed = true;

  await expect(flow).rejects.toBeInstanceOf(Abandoned);
});

it("takes the tab when the popup is blocked, rather than refusing to reconnect", async () => {
  vi.stubGlobal("open", vi.fn(() => null));
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign, origin: window.location.origin });

  let settled = false;
  void reconnect().then(() => {
    settled = true;
  });
  await Promise.resolve();
  await Promise.resolve();

  expect(assign).toHaveBeenCalledWith("https://github.test/login/oauth/authorize");
  // Nothing after the navigation gets to run, so the promise must not resolve: a
  // caller that took resolution as success would report a connection that has not
  // happened.
  expect(settled).toBe(false);
});
