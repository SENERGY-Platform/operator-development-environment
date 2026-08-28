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

import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { apply, initTheme, resolved, stored } from "./theme";

/**
 * The theme, from both ends: the module that writes the attribute, and the
 * stylesheet that reads it.
 *
 * The stylesheet half is the one worth having. The palette is two parallel lists
 * of custom properties, and the failure mode is not a crash — it is a token added
 * to the light theme and not to the dark one, which leaves that one colour at its
 * light value on a dark page. Nothing type-checks that, nothing renders wrong in
 * the theme the author was looking at, and the review that would catch it is a
 * diff of two forty-line blocks.
 */

const CSS_PATH = ["src/index.css", "frontend/src/index.css"]
  .map((candidate) => resolve(process.cwd(), candidate))
  .find((candidate) => existsSync(candidate));
if (CSS_PATH === undefined) throw new Error("index.css not found from " + process.cwd());
const CSS = readFileSync(CSS_PATH, "utf8");

/** The custom properties declared in the block opened by `selector`. */
function tokensOf(selector: string): Map<string, string> {
  const at = CSS.indexOf(selector);
  expect(at, `the stylesheet has no \`${selector}\` block`).toBeGreaterThan(-1);
  const open = CSS.indexOf("{", at);
  // Each of these blocks is closed by a `}` in the first column; the nested
  // scope inside the media query is closed by an indented one first.
  const close = CSS.indexOf("\n}", open);
  const body = CSS.slice(open, close);
  const found = new Map<string, string>();
  for (const [, name, value] of body.matchAll(/(--[\w-]+):\s*([^;]+);/g)) {
    found.set(name, value.trim());
  }
  return found;
}

const LIGHT = ":root {";
const OS_DARK = ':root:not([data-theme="light"]) {';
const CHOSEN_DARK = ':root[data-theme="dark"] {';

describe("the stylesheet's three theme scopes", () => {
  it("declares light on the bare :root, so the base case needs no override", () => {
    const light = tokensOf(LIGHT);
    expect(light.get("--background")).toBeDefined();
    expect(light.get("--card")).toBeDefined();
    expect(light.get("color-scheme" as string)).toBeUndefined();
    // A light theme whose text is lighter than its page is a dark theme with the
    // labels swapped, and every `bg-background text-foreground` would read as the
    // wrong plane. `--card` is only asserted to be no darker than the page,
    // because the preset gives both the same white and a raised card would fail a
    // strict comparison for the right reason.
    expect(luminance(light.get("--card")!)).toBeGreaterThanOrEqual(luminance(light.get("--background")!));
    expect(luminance(light.get("--foreground")!)).toBeLessThan(luminance(light.get("--background")!));
  });

  it("guards the operating system's preference so an explicit light choice still wins", () => {
    // Without the :not(...) the media query beats [data-theme="light"] on source
    // order, and the toggle works one way only: a developer on a dark desktop
    // could never get the light theme.
    expect(CSS).toContain("@media (prefers-color-scheme: dark)");
    expect(CSS).toContain(OS_DARK);
  });

  it("gives the toggle the last word, after the media query", () => {
    // Both dark scopes carry the same specificity class count, so the one written
    // later wins. The chosen-dark block must therefore come after the media block.
    expect(CSS.indexOf(CHOSEN_DARK)).toBeGreaterThan(CSS.indexOf(OS_DARK));
  });

  /*
   * The invariant this file exists for.
   *
   * Every colour the light theme names must be renamed by both dark scopes. A
   * token present in one and missing from the others is a colour that keeps its
   * light value on a dark page — legible in the theme its author had open, and
   * wrong in the other.
   */
  it("restates every colour token in both dark scopes", () => {
    const light = [...tokensOf(LIGHT)].filter(([, value]) => isColour(value)).map(([name]) => name);
    expect(light.length, "no colour tokens were parsed out of :root").toBeGreaterThan(10);

    for (const [name, scope] of [
      [OS_DARK, "the operating system's preference"],
      [CHOSEN_DARK, "the toggle"],
    ] as const) {
      const dark = tokensOf(name);
      const missing = light.filter((token) => !dark.has(token));
      expect(
        missing,
        `these colours keep their light value under ${scope}: ${missing.join(", ")}. ` +
          `Add them to \`${name}\` in src/index.css.`,
      ).toEqual([]);
    }
  });

  it("keeps the two dark scopes identical, since they mean the same thing", () => {
    // They are duplicated rather than shared because CSS cannot union two
    // conditions into one scope without `:where`-level trickery that reads worse
    // than the repetition. Duplication is fine; drift is not.
    const os = tokensOf(OS_DARK);
    const chosen = tokensOf(CHOSEN_DARK);
    const differing = [...os].filter(([name, value]) => chosen.get(name) !== value);
    expect(
      differing.map(([name]) => name),
      "the operating-system dark theme and the chosen dark theme disagree",
    ).toEqual([]);
  });

  it("names a series slot for every colour the chart can draw", () => {
    // `exploration.tsx` caps a chart at eight series and asks for `s1`..`s8`.
    for (const scope of [LIGHT, OS_DARK, CHOSEN_DARK]) {
      const tokens = tokensOf(scope);
      for (let slot = 1; slot <= 8; slot++) {
        expect(tokens.has(`--series-${slot}`), `${scope} is missing --series-${slot}`).toBe(true);
      }
    }
  });
});

/**
 * Whether a declaration's value is a colour.
 *
 * Two notations, because the file has two sources: the shadcn preset writes
 * `oklch(...)`, and the eight series slots carried over from the stylesheet this
 * file replaces are hex. `--radius: 0.625rem` is neither, and must not be counted
 * — the restated-in-both-dark-scopes rule is about colour, and a radius that the
 * dark scopes do not restate is correct rather than a drift.
 */
function isColour(value: string): boolean {
  return value.startsWith("#") || value.startsWith("oklch(");
}

/**
 * Relative luminance, enough to tell a light plane from a dark one.
 *
 * oklch's first component *is* perceptual lightness, so for those values there is
 * nothing to compute. Hex still goes the long way round, for the series slots.
 */
function luminance(colour: string): number {
  const oklch = /^oklch\(\s*([\d.]+)/.exec(colour);
  if (oklch !== null) return Number(oklch[1]);
  const channels = [1, 3, 5].map((at) => parseInt(colour.slice(at, at + 2), 16) / 255);
  const [r, g, b] = channels.map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

// --- the module ---

describe("the choice", () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  afterEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  it("has no choice stored to begin with", () => {
    expect(stored()).toBeNull();
  });

  it("writes an explicit choice to the document and to storage", () => {
    apply("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(stored()).toBe("dark");
  });

  /*
   * The asymmetry that matters. Clearing the choice *removes* the attribute
   * rather than setting it to "light": the stylesheet's light theme is the bare
   * `:root`, so `data-theme="light"` is not "the default" — it is a positive
   * instruction that silences the operating system's preference for good.
   */
  it("removes the attribute when the choice is cleared, handing the decision back", () => {
    apply("dark");
    apply(null);
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(stored()).toBeNull();
  });

  it("keeps an explicit light choice, which is not the same as no choice", () => {
    apply("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(stored()).toBe("light");
  });

  it("ignores a stored value it does not recognise", () => {
    localStorage.setItem("ode.theme", "solarized");
    expect(stored()).toBeNull();
  });

  it("applies the stored choice before the first render", () => {
    localStorage.setItem("ode.theme", "dark");
    initTheme();
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("stamps nothing when there is no choice, so the media query decides", () => {
    initTheme();
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  });

  /*
   * jsdom has no matchMedia. That is also the state of any non-browser host this
   * module is loaded in, and it must not throw — the header renders the control on
   * the first paint, and an exception there takes the whole application down
   * rather than just the theme button.
   */
  it("survives a host with no matchMedia", () => {
    expect(typeof window.matchMedia).not.toBe("function");
    expect(() => resolved()).not.toThrow();
    expect(resolved()).toBe("light");
    expect(resolved("dark")).toBe("dark");
  });
});
