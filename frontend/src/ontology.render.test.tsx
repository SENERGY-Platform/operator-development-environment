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
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { AspectRef, AspectTreeNode } from "./api";

/**
 * The aspect tree pane: roots grouped under the ontology class each was
 * classified into (SNRGY-4648 decision 4), only the API answered by `api.aspectTree`
 * faked.
 *
 * `api.functions` and `api.devices` are stubbed empty rather than left unmocked:
 * `OntologyView` mounts all three panes, and the two below the one under test
 * would otherwise reach the real HTTP client with no backend behind it.
 */
let tree: AspectTreeNode[] = [];
let classes: AspectRef[] = [];

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      aspectTree: async () => ({ tree, classes }),
      functions: async () => ({ functions: [], rdf_type: "measuring" }),
      devices: async () => ({ devices: [], total: 0 }),
    },
  };
});

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: Root[] = [];

beforeEach(() => {
  tree = [];
  classes = [];
});

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
});

/** Mounts the ontology view and lets its three panes' mount-time reads settle. */
async function open(): Promise<HTMLElement> {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
  const { OntologyView } = await import("./ontology");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<OntologyView />));
  await act(async () => {
    await Promise.resolve();
  });
  return host;
}

/** A root or non-root aspect node, classified or not. */
function node(
  id: string,
  name: string,
  aspectClassId: string,
  children: AspectTreeNode[] = [],
): AspectTreeNode {
  return {
    id,
    name,
    root_id: id,
    parent_id: "",
    aspect_class_id: aspectClassId,
    children: children.length > 0 ? children : null,
  };
}

/** The top-level rows of the aspect tree, in order: one per class (occupied or
 * not) plus "Unclassified" where any root lacks a class — never an individual
 * aspect node, which only ever nests one level deeper. */
function topLevelRows(host: HTMLElement): string[] {
  const pane = host.querySelector('[aria-label="Aspects"]');
  return [...(pane?.querySelectorAll("ul.tree > li > div.tree-row > span.tree-name") ?? [])].map(
    (el) => el.textContent ?? "",
  );
}

it("groups root aspects under their class, keeps an empty class's row, and puts unclassified roots last", async () => {
  classes = [
    { id: "cls-energy", name: "Energy" },
    { id: "cls-empty", name: "Empty class" },
  ];
  tree = [node("kitchen", "Kitchen", "cls-energy"), node("site", "Site", "")];

  const host = await open();

  expect(topLevelRows(host)).toEqual(["Energy", "Empty class", "Unclassified"]);

  // The classified root renders nested under its class's row rather than beside
  // it — the existing per-node rendering, now one level deeper.
  const energyRow = [...host.querySelectorAll(".tree-row")].find(
    (row) => row.textContent === "Energy",
  );
  const energyRoots = energyRow?.closest("li")?.querySelector(":scope > ul .tree-name");
  expect(energyRoots?.textContent).toBe("Kitchen");

  // Same for the unclassified bucket.
  const unclassifiedRow = [...host.querySelectorAll(".tree-row")].find(
    (row) => row.textContent === "Unclassified",
  );
  const unclassifiedRoots = unclassifiedRow?.closest("li")?.querySelector(":scope > ul .tree-name");
  expect(unclassifiedRoots?.textContent).toBe("Site");

  // The empty class carries no nested list at all, not an empty one standing in
  // for "no aspects".
  const emptyRow = [...host.querySelectorAll(".tree-row")].find(
    (row) => row.textContent === "Empty class",
  );
  expect(emptyRow?.closest("li")?.querySelector("ul")).toBeNull();
});

it("still says the ontology has no aspects when there is neither a tree nor a class", async () => {
  tree = [];
  classes = [];

  const host = await open();

  const pane = host.querySelector('[aria-label="Aspects"]');
  expect(pane?.textContent).toContain("The ontology contains no aspects.");
  expect(pane?.querySelector("ul.tree")).toBeNull();
});
