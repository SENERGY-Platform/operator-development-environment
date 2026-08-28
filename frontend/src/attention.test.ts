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

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { announce, notifications, stopFlash, store, stored } from "./attention";

/**
 * The alert, tested through the three things it is allowed to touch: the
 * document title, the Notification constructor, and localStorage.
 *
 * jsdom has neither `Notification` nor `AudioContext`, which is the interesting
 * half — the module has to stay silent rather than throw on a host that lacks
 * them, because it is called from a `finally` block that also puts the answer on
 * screen. A throw there would lose the reply to a missing sound.
 */

const TITLE = "ODE — Operator Development Environment";

/** Puts the tab in the background, which is the only state announce() acts in. */
function goAway(): void {
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  vi.spyOn(document, "hasFocus").mockReturnValue(false);
}

beforeEach(() => {
  document.title = TITLE;
  localStorage.clear();
});

afterEach(() => {
  stopFlash();
  vi.restoreAllMocks();
  vi.useRealTimers();
  localStorage.clear();
  delete (globalThis as { Notification?: unknown }).Notification;
});

describe("the preference", () => {
  it("is off until it is switched on", () => {
    expect(stored()).toBe(false);
    store(true);
    expect(stored()).toBe(true);
    store(false);
    expect(stored()).toBe(false);
  });

  /*
   * A browser configured to block site data throws on getItem. The header renders
   * this control on the first paint, so a throw here is a blank application.
   */
  it("reads as off when storage refuses to answer", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    expect(() => stored()).not.toThrow();
    expect(stored()).toBe(false);
  });

  it("survives storage refusing to be written", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    expect(() => store(true)).not.toThrow();
  });
});

describe("announcing a finished turn", () => {
  it("does nothing at all while the window is in front of the developer", () => {
    vi.spyOn(document, "hasFocus").mockReturnValue(true);
    store(true);
    announce("Reply ready", "session");
    expect(document.title).toBe(TITLE);
  });

  /*
   * The case the feature exists for: an ODE window fully visible on a second
   * monitor while the developer types in their editor. visibilityState says
   * "visible" there, so hasFocus is what has to carry the decision.
   */
  it("acts on a visible window that does not have focus", () => {
    vi.spyOn(document, "hasFocus").mockReturnValue(false);
    announce("Reply ready", "session");
    expect(document.title).toContain("Reply ready");
  });

  it("blinks the title between the mark and the original", () => {
    vi.useFakeTimers();
    goAway();

    announce("Reply ready", "session");
    expect(document.title).toBe("● Reply ready");

    vi.advanceTimersByTime(1200);
    expect(document.title).toBe(TITLE);

    vi.advanceTimersByTime(1200);
    expect(document.title).toBe("● Reply ready");
  });

  /*
   * The title is restored from what it was *before the first* announce, not from
   * whatever it happened to be when the second one arrived — otherwise two turns
   * ending in the same background stretch leave "● Reply ready" as the permanent
   * title of the tab.
   */
  it("keeps the original title across a second announcement", () => {
    vi.useFakeTimers();
    goAway();

    announce("Reply ready", "session");
    vi.advanceTimersByTime(1200);
    announce("Turn failed", "session");
    expect(document.title).toBe("● Turn failed");

    vi.advanceTimersByTime(1200);
    expect(document.title).toBe(TITLE);
  });

  it("stops the blink and restores the title when the developer comes back", () => {
    vi.useFakeTimers();
    goAway();
    announce("Reply ready", "session");
    expect(document.title).toBe("● Reply ready");

    vi.mocked(document.hasFocus).mockReturnValue(true);
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    window.dispatchEvent(new Event("focus"));

    expect(document.title).toBe(TITLE);
    vi.advanceTimersByTime(5000);
    expect(document.title).toBe(TITLE);
  });

  /*
   * visibilitychange also fires on the way *out*. Treating it as "the developer is
   * back" would cancel the blink the moment the tab is hidden, which is when it
   * has just started.
   */
  it("does not cancel the blink when the tab is hidden again", () => {
    vi.useFakeTimers();
    goAway();
    announce("Reply ready", "session");
    document.dispatchEvent(new Event("visibilitychange"));
    expect(document.title).toBe("● Reply ready");
  });
});

describe("the desktop notification", () => {
  /** A stand-in for the constructor jsdom does not have. */
  function install(permission: NotificationPermission) {
    const raised: { title: string; body?: string }[] = [];
    class Fake {
      static permission = permission;
      static requestPermission = async () => permission;
      onclick: (() => void) | null = null;
      close = vi.fn();
      constructor(title: string, options?: { body?: string }) {
        raised.push({ title, body: options?.body });
      }
    }
    (globalThis as { Notification?: unknown }).Notification = Fake;
    return raised;
  }

  it("reports an unsupported host rather than throwing", () => {
    expect(notifications()).toBe("unsupported");
  });

  it("raises nothing on a host without the API", () => {
    goAway();
    store(true);
    expect(() => announce("Reply ready", "session")).not.toThrow();
    expect(document.title).toBe("● Reply ready");
  });

  /*
   * The title names ODE and the blink does not: the notification is read away
   * from the tab, among notifications from everything else on the machine.
   */
  it("raises one naming ODE when the alert is on and the permission granted", () => {
    goAway();
    const raised = install("granted");
    store(true);
    announce("Reply ready", "the session");
    expect(raised).toEqual([{ title: "ODE — Reply ready", body: "the session" }]);
    expect(document.title).toBe("● Reply ready");
  });

  /*
   * The toggle gates the notification and the tone, not the blink: the title is
   * free and needs no permission, so it is the baseline rather than a feature.
   */
  it("blinks but raises nothing while the alert is off", () => {
    goAway();
    const raised = install("granted");
    announce("Reply ready", "the session");
    expect(raised).toEqual([]);
    expect(document.title).toBe("● Reply ready");
  });

  it("raises nothing when the browser has refused the permission", () => {
    goAway();
    const raised = install("denied");
    store(true);
    announce("Reply ready", "the session");
    expect(raised).toEqual([]);
    expect(document.title).toBe("● Reply ready");
  });

  /*
   * A second turn has to alert again rather than quietly update the first
   * notification, which is what a shared tag would do.
   */
  it("closes the previous notification and raises a fresh one", () => {
    goAway();
    const raised = install("granted");
    store(true);
    announce("Reply ready", "one");
    announce("Turn failed", "two");
    expect(raised).toHaveLength(2);
  });
});
