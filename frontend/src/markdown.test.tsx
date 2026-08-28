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
import { afterEach, expect, it } from "vitest";
import { Markdown } from "./markdown";

/**
 * What the markdown renderer has to get right, in two groups.
 *
 * The first is structure: the assistant's prose has to become the tags it asked
 * for, mid-stream as well as when it is finished. Half of a `**` and an unclosed
 * fence are the normal case in a streaming turn, not an edge case, so they are
 * asserted rather than assumed.
 *
 * The second is the allowlist, and it is the reason this file is long. The model's
 * output is untrusted input — a prompt, an ontology label or a tool result can
 * reach it — and markdown passes raw HTML through by design. Every one of these
 * cases is a thing `marked` will happily emit and the walk has to refuse: a script,
 * an event handler, an image that would fetch, a `javascript:` link. None of it can
 * be caught by a type, and all of it is silent when it is wrong.
 *
 * Asserted against a mounted component rather than against a string, because the
 * output is React elements: there is no HTML in between to inspect, which is the
 * point of building it that way.
 */

/** The flag React wants before `act`, as the other render tests set it. */
(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: Root[] = [];

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
});

/** show renders `text` and returns the element the renderer produced. */
async function show(text: string): Promise<HTMLElement> {
  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<Markdown text={text} />));
  return host.firstElementChild as HTMLElement;
}

// --- structure ---

it("renders headings, emphasis and lists as the tags they ask for", async () => {
  const md = await show(
    "## Two candidates\n\nThe **shorter** window wins, see `window_s`.\n\n- 30s: 0.81\n- 60s: 0.74\n",
  );

  expect(md.querySelector("h2")?.textContent).toBe("Two candidates");
  expect(md.querySelector("strong")?.textContent).toBe("shorter");
  expect(md.querySelector("p code")?.textContent).toBe("window_s");
  expect([...md.querySelectorAll("ul > li")].map((item) => item.textContent)).toEqual([
    "30s: 0.81",
    "60s: 0.74",
  ]);
  // No literal syntax left anywhere.
  expect(md.textContent).not.toContain("**");
  expect(md.textContent).not.toContain("##");
});

it("keeps the start of a list that does not start at one", async () => {
  const md = await show("7. adjust the window\n8. rerun\n");

  expect(md.querySelector("ol")?.getAttribute("start")).toBe("7");
});

it("renders a table into its own scroll container, with the column alignment", async () => {
  const md = await show(
    ["| metric | previous | current |", "| --- | ---: | ---: |", "| f1 | 0.74 | 0.81 |"].join("\n"),
  );

  const wrapper = md.querySelector(".md-table");
  expect(wrapper?.firstElementChild?.tagName).toBe("TABLE");
  expect([...md.querySelectorAll("th")].map((cell) => cell.textContent)).toEqual([
    "metric",
    "previous",
    "current",
  ]);
  const cells = [...md.querySelectorAll("tbody td")] as HTMLElement[];
  expect(cells.map((cell) => cell.textContent)).toEqual(["f1", "0.74", "0.81"]);
  expect(cells[1].style.textAlign).toBe("right");
  expect(cells[0].style.textAlign).toBe("");
});

it("names the language of a fenced block and leaves its contents alone", async () => {
  const md = await show("```python\nwindow = 30  # **not** bold\n```\n");

  expect(md.querySelector(".md-code-language")?.textContent).toBe("python");
  expect(md.querySelector(".md-code pre code")?.textContent?.trim()).toBe(
    "window = 30  # **not** bold",
  );
});

it("keeps the mark of a task list item", async () => {
  const md = await show("- [x] window adjusted\n- [ ] rerun\n");

  const items = [...md.querySelectorAll("li")].map((item) => item.textContent);
  expect(items[0]).toContain("☑");
  expect(items[1]).toContain("☐");
  expect(items[0]).toContain("window adjusted");
  // The input itself is dropped: nothing here is operable.
  expect(md.querySelector("input")).toBeNull();
});

// --- mid-stream, which is most of the time a turn is on screen ---

it("renders an unclosed fence as the code block it is becoming", async () => {
  const md = await show("Here it is:\n\n```python\nwindow = 3");

  expect(md.querySelector(".md-code pre code")?.textContent?.trim()).toBe("window = 3");
});

it("leaves an emphasis marker that has not closed yet as text", async () => {
  const md = await show("the **shorter wind");

  expect(md.querySelector("strong")).toBeNull();
  expect(md.textContent?.trim()).toBe("the **shorter wind");
});

it("renders nothing for the empty turn a stream starts as", async () => {
  const md = await show("");

  expect(md.className).toContain("md");
  expect(md.childElementCount).toBe(0);
});

// --- the allowlist ---

it("drops a script with its contents rather than unwrapping it", async () => {
  const md = await show("before\n\n<script>window.token = 1</script>\n\nafter");

  expect(md.querySelector("script")).toBeNull();
  expect(md.textContent).not.toContain("window.token");
  expect(md.textContent).toContain("before");
  expect(md.textContent).toContain("after");
});

it("drops an image, which would be an outbound request the developer did not make", async () => {
  const md = await show('![pixel](http://example.invalid/p.gif)\n\n<img src="x" onerror="alert(1)">');

  expect(md.querySelector("img")).toBeNull();
  expect(md.innerHTML).not.toContain("onerror");
});

it("drops an iframe and an event handler on an allowed tag", async () => {
  const md = await show('<iframe src="http://example.invalid"></iframe>\n\n<p onclick="alert(1)">text</p>');

  expect(md.querySelector("iframe")).toBeNull();
  expect(md.querySelector("p")?.textContent).toBe("text");
  expect(md.innerHTML).not.toContain("onclick");
});

it("keeps the text of an unknown tag and discards the tag", async () => {
  const md = await show("<div><section>a section</section></div>");

  expect(md.querySelector("div")).toBeNull();
  expect(md.querySelector("section")).toBeNull();
  expect(md.textContent).toContain("a section");
});

it("opens an http link away from the session and refuses any other scheme", async () => {
  const md = await show(
    "[the run](https://mlflow.example.invalid/runs/7) and [not this](javascript:alert(1))",
  );

  const links = [...md.querySelectorAll("a")] as HTMLAnchorElement[];
  expect(links).toHaveLength(1);
  expect(links[0].getAttribute("href")).toBe("https://mlflow.example.invalid/runs/7");
  expect(links[0].getAttribute("target")).toBe("_blank");
  expect(links[0].getAttribute("rel")).toBe("noopener noreferrer nofollow");
  // The words of the refused link survive; only the navigation goes.
  expect(md.textContent).toContain("not this");
});

/**
 * The developer's words come back the way they went out; the assistant's do not.
 *
 * This used to be a cascade assertion against the stylesheet — `.md` and
 * `.turn-body` were both single classes, so which of them decided `white-space`
 * rested on source order and nothing in the build would have noticed it flipping.
 * The redesign removed the collision rather than guarding it: `whitespace-pre-wrap`
 * is written on the developer's turn where it applies, and the assistant's turn
 * never carries it because markdown has already made the paragraphs.
 *
 * The property is still worth a test, because it is a property of the *product* and
 * not of the stylesheet. An ontology path with underscores in it, or a snippet with
 * two spaces of indent, has to survive the round trip verbatim — and would not if
 * the developer's turn were ever routed through the markdown renderer.
 */
it("renders the developer's turn verbatim and the assistant's as markdown", async () => {
  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);

  // Markdown makes a paragraph out of this; verbatim it stays one text node with
  // the underscores and the double space intact.
  const text = "device_state  is *on*";
  await act(async () => root.render(<Markdown className="turn-body" text={text} />));

  const rendered = host.firstElementChild as HTMLElement;
  expect(rendered.classList.contains("md")).toBe(true);
  expect(rendered.querySelector("p")).not.toBeNull();
  expect(rendered.querySelector("em")?.textContent).toBe("on");
  // And the class the assistant's turn carries is not one that would re-wrap it.
  expect(rendered.className).not.toContain("whitespace-pre-wrap");
});

it("adds the class of the block it replaces, so it keeps that block's box", async () => {
  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<Markdown className="turn-body" text="text" />));

  const md = host.firstElementChild as HTMLElement;
  expect(md.classList.contains("md")).toBe(true);
  expect(md.classList.contains("turn-body")).toBe(true);
});
