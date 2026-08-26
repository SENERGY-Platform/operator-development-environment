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

/**
 * Monaco, assembled deliberately rather than imported whole.
 *
 * `monaco-editor` is MIT. Importing its `editor.main` entry point would bring
 * every language it supports, including the TypeScript language service — several
 * megabytes of JavaScript for languages an operator repository does not contain.
 * `edcore.main` is the editor and its features with no languages at all, and the
 * six below are what the scaffold of §5.11 item 3 actually consists of: Python,
 * YAML, a Dockerfile, Markdown, shell and — for pyproject.toml, which Monaco has
 * no TOML mode for — ini, whose `[section]` and `key = value` shape is the same.
 *
 * The workers are imported through Vite's `?worker` suffix, so they are bundled
 * rather than fetched from a CDN. That is not a preference: the cluster this runs
 * in restricts egress (M10), and a CDN-loaded editor would be a blank pane there.
 */

// The typed API and the untyped feature bundle are the same module instance, so
// the two imports together give a Monaco that is both complete and type-checked:
// `editor.api` ships the declarations, `edcore.main` registers the editor's
// contributions (find, folding, the command palette) and has none.
import * as monaco from "monaco-editor/esm/vs/editor/editor.api";
import "monaco-editor/esm/vs/editor/edcore.main";

import "monaco-editor/esm/vs/basic-languages/python/python.contribution";
import "monaco-editor/esm/vs/basic-languages/yaml/yaml.contribution";
import "monaco-editor/esm/vs/basic-languages/dockerfile/dockerfile.contribution";
import "monaco-editor/esm/vs/basic-languages/markdown/markdown.contribution";
import "monaco-editor/esm/vs/basic-languages/shell/shell.contribution";
import "monaco-editor/esm/vs/basic-languages/ini/ini.contribution";
import "monaco-editor/esm/vs/language/json/monaco.contribution";

import EditorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import JsonWorker from "monaco-editor/esm/vs/language/json/json.worker?worker";

// monaco-editor already declares MonacoEnvironment globally, as its own
// Environment type — so this assigns to that declaration rather than making a
// second one, which TypeScript would reject as a conflicting global.
self.MonacoEnvironment = {
  getWorker(_workerId: string, label: string): Worker {
    if (label === "json") return new JsonWorker();
    return new EditorWorker();
  },
};

/**
 * The editor language for a path. The backend sends one with every file it reads,
 * so this only translates it to what Monaco calls the same thing — `toml` has no
 * mode of its own, and anything unrecognised is left as plain text rather than
 * highlighted wrongly.
 */
export function monacoLanguage(language: string | undefined, path: string): string {
  const named = language ?? "";
  switch (named) {
    case "python":
    case "yaml":
    case "dockerfile":
    case "markdown":
    case "shell":
    case "json":
      return named;
    case "toml":
      return "ini";
    default:
      break;
  }
  // A file the backend had no opinion about, judged by its name. Kept as a
  // fallback rather than the primary path so the two sides cannot disagree.
  if (path.endsWith(".py")) return "python";
  if (path.endsWith(".yml") || path.endsWith(".yaml")) return "yaml";
  return "plaintext";
}

export { monaco };
