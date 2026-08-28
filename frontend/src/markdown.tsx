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

import { Fragment, createElement, useMemo, type ReactNode } from "react";
import { marked } from "marked";

/**
 * The assistant writes markdown. This is where it stops being characters and
 * becomes structure.
 *
 * Everything the model says arrived on screen inside `white-space: pre-wrap`, so a
 * table was a row of pipes, a heading was a hash and a metric list was one long
 * paragraph with asterisks in it. The prose that matters most here — §5.13's
 * interpretation of a run — is exactly the kind that carries a table.
 *
 * Two decisions are worth the space:
 *
 * **marked parses, we decide what survives.** `marked` (MIT, already on disk as
 * monaco's dependency, now declared) turns the text into HTML. That HTML is *not*
 * handed to `dangerouslySetInnerHTML`. It is parsed into an inert document and
 * walked against the allowlist below, which emits React elements. So the sanitiser
 * is ours, and there is no code path on which an unsanitised string reaches the
 * DOM — the failure mode of a sanitiser that returns a string is that someone
 * later forgets to call it, and there is nothing to forget here.
 *
 * **The model's output is untrusted input.** It is not the developer's own text: it
 * is a document that a prompt, an ontology, or a tool result can influence. Raw
 * HTML passes straight through markdown by design, so `<script>`, `<iframe>` and
 * `onerror=` are all things the assistant can emit. They are dropped here rather
 * than escaped, and so are images: an `<img src="http://…">` in a chat turn is an
 * outbound request the developer never asked for, made from their session.
 */

/**
 * Elements that survive the walk. A tag outside this map is *unwrapped* — its
 * children are kept and the element itself is discarded — because the common case
 * is a `<div>` or a `<span>` around text that is worth reading.
 *
 * `code` is here for inline spans; a fenced block is handled by its `pre` parent,
 * which reads the text directly.
 */
const ALLOWED = new Set([
  "p",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "ul",
  "ol",
  "li",
  "blockquote",
  "pre",
  "code",
  "hr",
  "br",
  "strong",
  "em",
  "del",
  "a",
  "table",
  "thead",
  "tbody",
  "tr",
  "th",
  "td",
]);

/**
 * Elements dropped with their subtree, rather than unwrapped.
 *
 * Unwrapping is the safe default for an unknown tag, but it is the wrong answer
 * for these: the text inside a `<style>` is a stylesheet, the text inside a
 * `<script>` is a program, and neither is prose the developer wants to read. The
 * media and form tags are here because they fetch, submit or play rather than
 * describe.
 */
const DROPPED = new Set([
  "script",
  "style",
  "noscript",
  "template",
  "iframe",
  "frame",
  "frameset",
  "object",
  "embed",
  "applet",
  "link",
  "meta",
  "base",
  "title",
  "svg",
  "math",
  "img",
  "picture",
  "source",
  "audio",
  "video",
  "track",
  "canvas",
  "map",
  "form",
  "input",
  "button",
  "select",
  "option",
  "textarea",
  "label",
  "fieldset",
]);

/** The schemes a link may use. Everything else — `javascript:`, `data:`, `file:` — is
 *  not a link the assistant has any business creating. */
const SCHEMES = new Set(["http:", "https:", "mailto:"]);

/**
 * Markdown renders `text` as a block of prose.
 *
 * `className` is added beside `md` so a call site can keep its own spacing rule;
 * the elements inside are styled by `.md <tag>` in `index.css` rather than by a
 * class per node, which keeps this file free of presentation.
 */
export function Markdown({ text, className }: { text: string; className?: string }) {
  // Memoised on the text because the chat re-renders on every streamed delta, and
  // a delta re-parses everything received so far.
  const content = useMemo(() => render(text), [text]);
  return <div className={className ? `md ${className}` : "md"}>{content}</div>;
}

/**
 * render parses markdown and returns what survived as React nodes.
 *
 * Partial input is expected rather than tolerated: this is called on every delta of
 * a streaming turn, so half of a `**` and a fence that has not been closed yet are
 * the normal case. `marked` resolves both the way a reader would — the unclosed
 * fence becomes a code block that is still growing — which is why the streaming
 * turn can render through this instead of waiting for the final message.
 */
function render(text: string): ReactNode[] {
  if (!text) return [];
  const html = marked.parse(text, { async: false, gfm: true, breaks: true });
  // A document from DOMParser is inert: scripts do not run, `src` and `href` are
  // not fetched. It is a parser, not a page.
  const parsed = new DOMParser().parseFromString(html, "text/html");
  return children(parsed.body);
}

/** children converts every child node of `parent`, dropping what does not survive. */
function children(parent: Node): ReactNode[] {
  const nodes: ReactNode[] = [];
  parent.childNodes.forEach((child, index) => {
    const converted = convert(child, index);
    if (converted !== null) nodes.push(converted);
  });
  return nodes;
}

/** convert turns one parsed node into a React node, or into null if it is dropped. */
function convert(node: Node, key: number): ReactNode {
  if (node.nodeType === 3 /* text */) return node.nodeValue;
  if (node.nodeType !== 1 /* element */) return null;

  const element = node as Element;
  const tag = element.tagName.toLowerCase();
  if (DROPPED.has(tag)) return null;
  // The unknown tag: keep the words, discard the wrapper.
  if (!ALLOWED.has(tag)) {
    const kept = children(element);
    return kept.length ? <Fragment key={key}>{kept}</Fragment> : null;
  }

  switch (tag) {
    case "pre":
      return <CodeBlock key={key} element={element} />;
    case "a":
      return anchor(element, key);
    case "hr":
      return <hr key={key} />;
    case "br":
      return <br key={key} />;
    case "ol": {
      // A list that starts at 7 says so, and losing that renumbers the assistant's
      // own reference to "step 7".
      const start = Number(element.getAttribute("start"));
      return (
        <ol key={key} start={Number.isFinite(start) && start > 0 ? start : undefined}>
          {children(element)}
        </ol>
      );
    }
    case "li":
      return <li key={key}>{listItem(element)}</li>;
    case "table":
      // Wrapped, because a comparison table is as wide as the run has metrics and
      // the pane it sits in is half a window. The scroll belongs to the table; a
      // chat that scrolls sideways as a whole is a broken chat.
      return (
        <div className="md-table" key={key}>
          <table>{children(element)}</table>
        </div>
      );
    case "th":
    case "td": {
      // GFM's column alignment arrives as an `align` attribute, which is where a
      // table of metrics gets its numbers under each other.
      const align = element.getAttribute("align");
      const aligned = align === "left" || align === "center" || align === "right";
      return createElement(
        tag,
        { key, style: aligned ? { textAlign: align } : undefined },
        ...children(element),
      );
    }
    default:
      // The remaining tags — paragraphs, headings, lists, quotes, emphasis, inline
      // code, the table frame — carry no attribute worth keeping, so the tag name
      // is the whole decision.
      return createElement(tag, { key }, ...children(element));
  }
}

/**
 * listItem keeps the mark of a task list item.
 *
 * The checkbox `marked` emits for `- [x]` is an `<input>`, and an input is dropped
 * here, which would turn a plan into a list of things with no indication of which
 * are done. Task lists are not otherwise implemented — there is no interaction and
 * no styling, and the box is advisory like everything else the assistant writes —
 * this only refuses to silently lose the one bit of state in the line.
 */
function listItem(element: Element): ReactNode[] {
  const box = element.querySelector(":scope > input[type=checkbox]");
  if (!box) return children(element);
  const mark = box.hasAttribute("checked") ? "☑ " : "☐ ";
  return [mark, ...children(element)];
}

/**
 * anchor renders a link the developer can trust as far as its scheme.
 *
 * A rejected scheme is not an error: the link text is still prose worth reading, so
 * it stays and only the navigation goes. `noreferrer` is deliberate alongside
 * `noopener` — ODE's own paths carry a session in them, and a link the *model*
 * wrote should not hand the referrer to whatever it points at.
 */
function anchor(element: Element, key: number): ReactNode {
  const href = element.getAttribute("href") ?? "";
  const text = <Fragment key={key}>{children(element)}</Fragment>;
  if (!href) return text;
  let url: URL;
  try {
    url = new URL(href, window.location.origin);
  } catch {
    return text;
  }
  if (!SCHEMES.has(url.protocol)) return text;
  return (
    <a key={key} href={url.href} target="_blank" rel="noopener noreferrer nofollow">
      {children(element)}
    </a>
  );
}

/**
 * CodeBlock renders a fenced block, with its language named rather than applied.
 *
 * The text is read with `textContent` instead of walked: whatever is inside a fence
 * is characters, and a `<b>` in there came from the model's own text rather than
 * from its intent to emphasise. Nothing is highlighted — the editor pane has Monaco
 * for code that is being worked on, and this is code that is being quoted.
 */
function CodeBlock({ element }: { element: Element }) {
  const code = element.querySelector(":scope > code");
  const language = code
    ? (Array.from(code.classList)
        .find((name) => name.startsWith("language-"))
        ?.slice("language-".length) ?? "")
    : "";
  return (
    <div className="md-code">
      {language && <span className="md-code-language">{language}</span>}
      <pre>
        <code>{(code ?? element).textContent ?? ""}</code>
      </pre>
    </div>
  );
}
