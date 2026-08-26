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
import { expect, it } from "vitest";

/**
 * The folds at the end of `index.css`, kept winnable.
 *
 * `@container` contributes nothing to specificity — it is a condition on a rule,
 * not part of its selector — so `.panes.profiler` inside a fold block and
 * `.panes.profiler` in the body of the file are a tie, and a tie is settled by
 * source order. The fold blocks work *only* because they are last. That is a
 * property of the file's layout rather than of any rule in it, which is why it
 * broke once already without a single check going red: every container query in
 * the file was inert while `tsc --noEmit` and `vite build` stayed green.
 *
 * Two properties are asserted here, and a third test asserts that the assertions
 * themselves still bite:
 *
 *   - nothing below the folds overrides a folded declaration, judged by matching
 *     real selectors against real elements rather than by comparing strings;
 *   - no per-view layout rule is written below the folds at all, which is the near
 *     miss — a rule that is not yet an override and becomes one the moment its
 *     view joins the fold list.
 */

/**
 * The stylesheet is read as text rather than imported.
 *
 * `?raw` returns an empty string here — Vite hands a `.css` import to its CSS
 * pipeline before the suffix is honoured — and an empty stylesheet would make
 * every assertion below pass without looking at anything. Hence the read, and
 * hence the length check in the last test.
 */
const CSS_PATH = ["src/index.css", "frontend/src/index.css"]
  .map((candidate) => resolve(process.cwd(), candidate))
  .find((candidate) => existsSync(candidate));
if (CSS_PATH === undefined) throw new Error("index.css not found from " + process.cwd());
const CSS = readFileSync(CSS_PATH, "utf8");

/** Where the "belongs above this" instruction is written, for failure messages. */
const MARKER = "--- the folds, last on purpose ---";

/** The classes the folds lay out. A layout rule on one of these belongs above them. */
const FOLD_FAMILY = ["panes", "pane", "chat-layout", "code-layout", "file-tree"];

interface Rule {
  selectors: string[];
  properties: string[];
  /** Byte offset of the selector, so a failure can name a line. */
  offset: number;
  /** The at-rule this sits in, or null at the top level. */
  at: string | null;
}

/** Replaces comments with spaces, which keeps every later offset truthful. */
function blankComments(css: string): string {
  return css.replace(/\/\*[\s\S]*?\*\//g, (comment) => " ".repeat(comment.length));
}

/**
 * A stylesheet parser sized for this stylesheet: comments, one level of at-rule
 * nesting, no CSS nesting, no `@supports`. The last test asserts that shape rather
 * than assuming it, because a parser that quietly skips a construct it does not
 * know reports "no conflicts" for a stylesheet full of them.
 */
function parse(css: string): Rule[] {
  const source = blankComments(css);
  const rules: Rule[] = [];
  const open: string[] = [];
  let position = 0;

  while (position < source.length) {
    const brace = source.slice(position).search(/[{}]/);
    if (brace === -1) break;
    const at = position + brace;

    if (source[at] === "}") {
      open.pop();
      position = at + 1;
      continue;
    }

    const raw = source.slice(position, at);
    const prelude = raw.trim();
    const offset = position + Math.max(raw.indexOf(prelude), 0);

    if (prelude.startsWith("@")) {
      open.push(prelude);
      position = at + 1;
      continue;
    }

    const close = source.indexOf("}", at + 1);
    const end = close === -1 ? source.length : close;
    rules.push({
      selectors: split(prelude, ","),
      properties: split(source.slice(at + 1, end), ";")
        .map((declaration) => declaration.slice(0, declaration.indexOf(":")).trim().toLowerCase())
        .filter((property) => property !== ""),
      offset,
      at: open.length > 0 ? open[open.length - 1] : null,
    });
    position = end + 1;
  }

  return rules;
}

/** Splits on a separator that is not inside brackets. */
function split(text: string, separator: string): string[] {
  const parts: string[] = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < text.length; i++) {
    const character = text[i];
    if (character === "(" || character === "[") depth += 1;
    else if (character === ")" || character === "]") depth -= 1;
    else if (character === separator && depth === 0) {
      parts.push(text.slice(start, i).trim());
      start = i + 1;
    }
  }
  parts.push(text.slice(start).trim());
  return parts.filter((part) => part !== "");
}

function lineOf(css: string, offset: number): number {
  return css.slice(0, offset).split("\n").length;
}

/**
 * Specificity as the three counts the cascade compares. `:is()` and `:not()` take
 * the specificity of their argument and are not used in this stylesheet; if one
 * appears, this counts low rather than high, which makes the check report a
 * conflict the browser might resolve — a false alarm, not a silent pass.
 */
function specificity(selector: string): [number, number, number] {
  const stripped = selector.replace(/::[\w-]+/g, " ");
  const ids = stripped.match(/#[\w-]+/g)?.length ?? 0;
  const classes =
    (stripped.match(/\.[\w-]+/g)?.length ?? 0) +
    (stripped.match(/\[[^\]]*\]/g)?.length ?? 0) +
    (stripped.match(/(?<!:):[\w-]+/g)?.length ?? 0);
  const types = stripped.match(/(^|[\s>+~])[a-zA-Z][\w-]*/g)?.length ?? 0;
  return [ids, classes, types];
}

/** True when `a` wins against `b`, source order deciding a tie in `a`'s favour. */
function beats(a: [number, number, number], b: [number, number, number]): boolean {
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] > b[i];
  }
  return true;
}

/**
 * Builds an element the given selector matches, so a candidate selector can be
 * judged with the browser's own matching rather than with a string comparison.
 * Sibling combinators would need a sibling and are not used in this stylesheet.
 */
function elementFor(selector: string): Element | null {
  if (/[+~]/.test(selector)) return null;
  let parent: Element = document.createElement("div");
  let leaf: Element | null = null;

  for (const compound of selector.split(/\s*>\s*|\s+/).filter((part) => part !== "")) {
    const tag = compound.match(/^[a-zA-Z][\w-]*/)?.[0] ?? "div";
    const element = document.createElement(tag);
    for (const [, name] of compound.matchAll(/\.([\w-]+)/g)) element.classList.add(name);
    const id = compound.match(/#([\w-]+)/);
    if (id) element.id = id[1];
    parent.append(element);
    parent = element;
    leaf = element;
  }

  return leaf;
}

/**
 * Drops type selectors, keeping the combinators.
 *
 * A folded selector that names no tag — `.panes.exploration` — matches every
 * element carrying those classes, whatever its tag, so a later `main.panes.
 * exploration` overrides it for the elements the application actually renders.
 * Matching the candidate as written against a synthesised `div` would miss that
 * whole class of override, so the tag constraint is dropped for the matching step
 * and kept for the specificity comparison. Where the folded selector *does* name a
 * tag this is conservative: a candidate naming a different one is reported rather
 * than dismissed, which is the direction a check like this should err in.
 */
function withoutTypes(selector: string): string {
  return selector
    // Split on the combinators, keeping them, so the structure survives.
    .split(/(\s*[>+~]\s*|\s+)/)
    .map((part) => {
      if (/^\s*[>+~]?\s*$/.test(part)) return part;
      // A compound that was only a tag becomes the universal selector; one that
      // also carries classes simply loses its tag.
      const stripped = part.replace(/^[a-zA-Z][\w-]*/, "");
      return stripped === "" ? "*" : stripped;
    })
    .join("");
}

function matches(element: Element, selector: string): boolean {
  try {
    return element.matches(selector);
  } catch {
    // A selector jsdom cannot parse is one this check cannot reason about. Treated
    // as a match, so it surfaces as a reported conflict rather than as silence.
    return true;
  }
}

interface Folds {
  start: number;
  folded: Rule[];
  below: Rule[];
}

function foldsOf(css: string): Folds {
  const rules = parse(css);
  const start = blankComments(css).indexOf("@container");
  return {
    start,
    folded: rules.filter((rule) => rule.at?.startsWith("@container") === true && rule.offset > start),
    // Everything after the folds that is not itself a fold. A later `@container`
    // refining an earlier one is the folds talking among themselves.
    below: rules.filter((rule) => rule.offset > start && rule.at?.startsWith("@container") !== true),
  };
}

/** Every way a rule below the folds beats one of them. Empty is the invariant. */
function overrides(css: string): string[] {
  const { folded, below } = foldsOf(css);
  const found: string[] = [];

  for (const fold of folded) {
    for (const selector of fold.selectors) {
      const element = elementFor(selector);
      if (!element) continue;

      for (const rule of below) {
        if (rule.offset < fold.offset) continue;
        const shared = rule.properties.filter((property) => fold.properties.includes(property));
        if (shared.length === 0) continue;

        for (const candidate of rule.selectors) {
          if (!matches(element, withoutTypes(candidate))) continue;
          if (!beats(specificity(candidate), specificity(selector))) continue;
          found.push(
            `\`${candidate}\` (line ${lineOf(css, rule.offset)}) sets ${shared.join(", ")} on the same ` +
              `elements as the fold's \`${selector}\` (line ${lineOf(css, fold.offset)}), and wins on ` +
              `source order because @container adds no specificity`,
          );
        }
      }
    }
  }

  return found;
}

/** Layout rules for a pane written below the folds, override or not yet. */
function misplaced(css: string): string[] {
  const { folded, below } = foldsOf(css);
  const foldProperties = new Set(folded.flatMap((rule) => rule.properties));
  const found: string[] = [];

  for (const rule of below) {
    const shared = rule.properties.filter((property) => foldProperties.has(property));
    if (shared.length === 0) continue;
    for (const selector of rule.selectors) {
      const classes = [...selector.matchAll(/\.([\w-]+)/g)].map(([, name]) => name);
      if (!classes.some((name) => FOLD_FAMILY.includes(name))) continue;
      found.push(`\`${selector}\` (line ${lineOf(css, rule.offset)}) sets ${shared.join(", ")}`);
    }
  }

  return found;
}

function moveItUp(what: string, findings: string[]): string {
  return (
    `${what}:\n` +
    findings.map((line) => `  - ${line}`).join("\n") +
    `\n\nMove the rule above the "${MARKER}" comment in src/index.css. The folds must stay ` +
    `last: @container is a condition on a rule, not part of its selector, so identical ` +
    `selectors are a tie and the later one wins — and a view that renders two columns where ` +
    `it meant one does not look like a fault.`
  );
}

// --- the invariant ---

it("the fold blocks are where the cascade needs them, at the end of the stylesheet", () => {
  const { start, folded } = foldsOf(CSS);
  expect(start, "index.css has no @container block at all").toBeGreaterThan(0);
  expect(CSS.indexOf(MARKER), "the marker the failure messages point at has moved").toBeGreaterThan(0);
  expect(folded.flatMap((rule) => rule.selectors)).toContain(".panes.profiler");
  expect(folded.some((rule) => rule.properties.includes("grid-template-columns"))).toBe(true);
});

/*
 * The property itself: for every declaration a fold makes, no rule further down
 * the file both matches the same elements and wins the tie.
 *
 * Judged over the cascade rather than over the text, so it also holds for a rule
 * written with its classes in another order, with a tag in front, or reached
 * through a descendant — none of which a string comparison would catch.
 */
it("no rule below the folds can override a folded declaration", () => {
  const findings = overrides(CSS);
  expect(
    findings,
    moveItUp("A rule below the folds beats them, so the view will not fold inside a split pane", findings),
  ).toEqual([]);
});

/*
 * The near miss, which is how this comes back.
 *
 * A developer adding the next pane writes its layout at the end of the file and
 * adds the view to the fold list in the same change. The first half is harmless
 * until the second half lands, and then the fold silently loses. Flagging the
 * layout rule while it is still only misplaced catches it a step earlier.
 */
it("no per-view layout rule is written below the folds", () => {
  const findings = misplaced(CSS);
  expect(findings, moveItUp("A layout rule for a pane sits below the folds", findings)).toEqual([]);
});

// --- the checks above, checked ---

/*
 * A stylesheet that satisfies these two tests trivially would satisfy them
 * silently, and this file exists because a silent pass is exactly what happened
 * last time. So the defects are reintroduced here, against a copy of the real
 * stylesheet, and the checks are required to find them.
 */
it("the override check finds a per-view rule appended below the folds", () => {
  const broken = `${CSS}\n.panes.profiler {\n  grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);\n}\n`;

  const findings = overrides(broken);
  expect(findings.join("\n")).toContain(".panes.profiler");
  expect(findings.join("\n")).toContain("grid-template-columns");
});

// The same rule written differently: another class order, and reached through a
// tag. A text comparison against the fold's own selectors would miss both.
it("the override check is about the elements a selector matches, not how it is spelled", () => {
  const broken = `${CSS}\nmain.exploration.panes {\n  grid-template-columns: repeat(2, minmax(0, 1fr));\n}\n`;

  expect(overrides(broken).join("\n")).toContain("main.exploration.panes");
});

it("the override check ignores a rule below the folds that cannot match a folded element", () => {
  const harmless = `${CSS}\n.file-head code {\n  grid-template-columns: minmax(0, 1fr);\n}\n`;

  expect(overrides(harmless)).toEqual([]);
});

it("the misplacement check finds a new view's layout written below the folds", () => {
  const early = `${CSS}\n.panes.registration {\n  grid-template-columns: minmax(0, 2fr) minmax(0, 1fr);\n}\n`;

  /*
   * The class has to name a view the fold list does not carry yet, because that is
   * the state being tested: a layout rule that is not an override *yet* and becomes
   * one the moment its view joins the folds. This stood as `.panes.experiments`
   * until the experiments pane was built and joined them, at which point the
   * fixture started asserting the opposite of what it meant — the first
   * expectation below found the very override the second half is about. §5.14's
   * registration pane is the next one that does not exist, which is exactly the
   * property the fixture needs; when it is built, move this to the one after it.
   */
  expect(overrides(early)).toEqual([]);
  expect(misplaced(early).join("\n")).toContain(".panes.registration");
});

it("the misplacement check leaves an unrelated rule below the folds alone", () => {
  const unrelated = `${CSS}\n.badge.warn {\n  color: red;\n}\n`;

  expect(misplaced(unrelated)).toEqual([]);
});

/*
 * The parser's own footing. Every test above is only as true as the parse under
 * it, so the constructs the parser does not understand must not be in the file.
 */
it("the stylesheet stays within the shape this parser understands", () => {
  const source = blankComments(CSS);
  expect(source.length, "the stylesheet was read as empty").toBeGreaterThan(1000);
  expect(source, "CSS nesting is not parsed here").not.toMatch(/^\s*&/m);
  expect(source.split("{").length, "braces do not balance").toBe(source.split("}").length);
  for (const [word] of source.matchAll(/@[\w-]+/g)) {
    expect(["@media", "@container"], `${word} is an at-rule this parser has not been taught`).toContain(word);
  }
});
