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

import { useEffect, useMemo, useRef } from "react";

// The types only. Monaco itself is loaded when a card with code appears — see the
// effect below — and a type import is erased, so this costs the bundle nothing.
import type * as Monaco from "monaco-editor/esm/vs/editor/editor.api";

import { useTheme } from "./theme";

/**
 * CodeView is code the developer has to read and must not edit.
 *
 * It exists for `run_code`'s confirmation card. `JSON.stringify` is the right
 * rendering for a call whose arguments are a handful of ids, and the wrong one for
 * a cell of Python: it puts the whole program on one line behind `\n` escapes and
 * asks the developer to approve what they cannot read. The confirmation is the
 * one place in ODE where reading the argument *is* the feature — D11 exists so the
 * developer decides on what the model actually proposed.
 *
 * Monaco rather than a highlighter of its own: it is already assembled for the
 * code pane, including Python, so this is a second view of a dependency the bundle
 * carries either way. Loaded on demand all the same — the conversation is usable
 * without an editor, and a test that mounts a route should not be pulling megabytes
 * of one.
 *
 * Read-only in both senses Monaco distinguishes — `readOnly` refuses edits,
 * `domReadOnly` also keeps the textarea out of the way of a screen reader looking
 * for an editable field.
 */

/** Monaco needs an explicit height, so the box is sized from the text. */
const LINE_HEIGHT = 18;
/** Room for the top and bottom padding Monaco leaves inside the viewport. */
const PADDING = 12;

export function CodeView({
  code,
  language = "python",
  /** Beyond this the box stops growing and the code scrolls inside it. */
  maxLines = 18,
}: {
  code: string;
  language?: string;
  maxLines?: number;
}) {
  const host = useRef<HTMLDivElement | null>(null);
  const { active } = useTheme();

  // Three lines minimum: a one-line cell in a box its own height reads as an
  // input field rather than as a quotation of what is about to run.
  const height = useMemo(() => {
    const lines = code.split("\n").length;
    return Math.min(Math.max(lines, 3), maxLines) * LINE_HEIGHT + PADDING;
  }, [code, maxLines]);

  useEffect(() => {
    const container = host.current;
    if (container === null) return;

    let gone = false;
    let instance: Monaco.editor.IStandaloneCodeEditor | null = null;

    void import("./monaco").then(({ monaco }) => {
      // The card can be answered before the editor arrives, which is exactly what
      // a developer who recognises the tool name does.
      if (gone) return;
      instance = monaco.editor.create(container, {
        value: code,
        language,
        theme: active === "dark" ? "vs-dark" : "vs",
        readOnly: true,
        domReadOnly: true,
        automaticLayout: true,
        minimap: { enabled: false },
        scrollBeyondLastLine: false,
        lineHeight: LINE_HEIGHT,
        fontSize: 12,
        lineNumbersMinChars: 3,
        folding: false,
        contextmenu: false,
        renderLineHighlight: "none",
        overviewRulerLanes: 0,
        // The card sits inside a scrolling conversation. An editor that consumed
        // every wheel event would stop the pane scrolling whenever the pointer
        // happened to be over the code.
        scrollbar: { alwaysConsumeMouseWheel: false },
      });
    });

    return () => {
      gone = true;
      instance?.getModel()?.dispose();
      instance?.dispose();
    };
    // The text is a property of the call being confirmed and never changes under
    // this component, so it belongs in the dependency list rather than in an
    // update path: a different card is a different editor.
  }, [code, language, active]);

  return (
    <div
      className="monaco code-view mt-2 overflow-hidden rounded-md border"
      style={{ height }}
      ref={host}
    />
  );
}
