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

import { useCallback, useMemo, useState } from "react";
import {
  api,
  deviceLabel,
  type AspectRef,
  type AspectTreeNode,
  type Device,
  type OntologyFunction,
} from "./api";
import { setParam, useParam } from "./router";
import { Busy, Muted, Pane, useLoad } from "./ui";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

/**
 * The ontology view: the semantic model the rest of ODE selects against (§5.1).
 *
 * It was the first surface built and is still the one to check when a selection
 * comes back empty — an aspect that is not in the tree cannot be resolved to, and
 * a device this account cannot read is not a device ODE can profile.
 *
 * Both filters live in the URL. Neither costs a series read, both re-run on their
 * own when the page loads, so restoring them is free: a developer who reloads
 * while looking at controlling functions gets controlling functions back.
 */
export function OntologyView() {
  return (
    <main className="panes">
      <AspectTreePane />
      <FunctionsPane />
      <DevicesPane />
    </main>
  );
}

/** One class's row in the tree: its root aspects, or none for a class the
 * ontology declares but nothing is classified under yet. */
interface AspectClassGroup {
  id: string;
  name: string;
  roots: AspectTreeNode[];
}

/** The id groupByClass gives the roots with no `aspect_class_id` — never a real
 * aspect class, so a class actually using it could not be confused with it. */
const UNCLASSIFIED = "";

/**
 * Buckets the tree's root nodes under the class each was classified into, root
 * rather than node because `aspect_class_id` is only ever set at the root and
 * shared down the whole hierarchy. Every class is given a row even
 * with no roots, so an ontology class with nothing classified under it yet is
 * still visible rather than silently absent. Unclassified roots are grouped last.
 */
function groupByClass(tree: AspectTreeNode[], classes: AspectRef[]): AspectClassGroup[] {
  const byId = new Map<string, AspectClassGroup>();
  for (const cls of classes) {
    byId.set(cls.id, { id: cls.id, name: cls.name, roots: [] });
  }
  const unclassified: AspectTreeNode[] = [];
  for (const root of tree) {
    if (!root.aspect_class_id) {
      unclassified.push(root);
      continue;
    }
    let group = byId.get(root.aspect_class_id);
    if (!group) {
      // A root names a class the class listing did not — shown by its id rather
      // than dropped, since that would silently lose roots from the tree.
      group = { id: root.aspect_class_id, name: root.aspect_class_id, roots: [] };
      byId.set(root.aspect_class_id, group);
    }
    group.roots.push(root);
  }
  const groups = [...byId.values()];
  if (unclassified.length > 0) {
    groups.push({ id: UNCLASSIFIED, name: "Unclassified", roots: unclassified });
  }
  return groups;
}

function AspectTreePane() {
  const load = useCallback(() => api.aspectTree(), []);
  const { data, error, loading } = useLoad(load);
  const groups = useMemo(
    () => (data ? groupByClass(data.tree, data.classes) : []),
    [data],
  );

  return (
    <Pane title="Aspects" subtitle="Hierarchical subsystems from the platform ontology">
      {loading && <Busy>Loading…</Busy>}
      {error && <Muted>{error}</Muted>}
      {data && groups.length === 0 && <Muted>The ontology contains no aspects.</Muted>}
      {data && groups.length > 0 && (
        <ul className="tree">
          {groups.map((group) => (
            <li key={group.id || "unclassified"}>
              <div className="tree-row">
                <span className="tree-name font-medium">{group.name}</span>
              </div>
              {group.roots.length > 0 && (
                <ul>
                  {group.roots.map((node) => (
                    <TreeNode key={node.id} node={node} />
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
      )}
    </Pane>
  );
}

function TreeNode({ node }: { node: AspectTreeNode }) {
  const children = node.children ?? [];
  const [open, setOpen] = useState(true);
  const hasChildren = children.length > 0;

  return (
    <li>
      {/*
        The same row the code pane's file tree uses, and for the same reasons: the
        whole row is the control, so the hit target is a row rather than a glyph, and
        the arrow is a mark inside it rather than a bordered button of its own. It was
        an outline `Button` around the arrow, which drew a box at every branch and
        pushed the name of a branch out of line with the name of a leaf.
      */}
      <div className="tree-row">
        {hasChildren ? (
          <Button
            variant="ghost"
            size="sm"
            // `aria-expanded:bg-transparent`: the ghost variant fills an expanded
            // button with `--muted`, and every branch of this tree starts expanded —
            // which shaded the whole hierarchy and read as a selection.
            className="tree-dir h-auto flex-1 justify-start py-1 font-normal aria-expanded:bg-transparent"
            onClick={() => setOpen(!open)}
            aria-expanded={open}
          >
            <span
              className="twisty inline-block w-3 shrink-0 text-center text-xs text-muted-foreground"
              aria-hidden="true"
            >
              {open ? "▾" : "▸"}
            </span>
            <span className="tree-name">{node.name || node.id}</span>
          </Button>
        ) : (
          // A leaf is not a control — nothing in this pane opens an aspect — so it is
          // a row of the same shape rather than a button that does nothing.
          <span className="tree-file">
            <span
              className="twisty leaf inline-block w-3 shrink-0 text-center text-xs text-muted-foreground"
              aria-hidden="true"
            >
              ·
            </span>
            <span className="tree-name">{node.name || node.id}</span>
          </span>
        )}
      </div>
      {hasChildren && open && (
        <ul>
          {children.map((child) => (
            <TreeNode key={child.id} node={child} />
          ))}
        </ul>
      )}
    </li>
  );
}

function FunctionsPane() {
  const rdfType = useParam("functions") === "controlling" ? "controlling" : "measuring";
  const load = useCallback(() => api.functions(rdfType).then((r) => r.functions), [rdfType]);
  const { data, error, loading } = useLoad(load);

  return (
    <Pane title="Functions" subtitle="What the platform can measure and control">
      <div className="toggle">
        {(["measuring", "controlling"] as const).map((t) => (
          <Button variant="outline"
            key={t}
            className={t === rdfType ? "active" : ""}
            aria-pressed={t === rdfType}
            // Measuring is the default the pane opens at, so it is written as an
            // absent parameter rather than as ?functions=measuring — a URL should
            // name what was changed, not restate every default.
            onClick={() => setParam("functions", t === "measuring" ? null : t)}
          >
            {t}
          </Button>
        ))}
      </div>
      {loading && <Busy>Loading…</Busy>}
      {error && <Muted>{error}</Muted>}
      {data && (
        <ul className="list flex flex-col gap-1">
          {data.map((fn: OntologyFunction) => (
            <li key={fn.id}>
              {fn.display_name || fn.name || fn.id}
              {fn.deprecated && (
                <span className="muted-inline text-xs text-muted-foreground"> deprecated</span>
              )}
            </li>
          ))}
        </ul>
      )}
    </Pane>
  );
}

function DevicesPane() {
  // The applied query is the URL; the input is local until it is submitted, so
  // typing does not fire a listing per keystroke.
  const query = useParam("devices") ?? "";
  const [search, setSearch] = useState(query);
  const load = useCallback(() => api.devices(query), [query]);
  const { data, error, loading } = useLoad(load);

  return (
    <Pane
      title="Devices"
      subtitle="Only devices this account may read — the platform decides, not ODE"
    >
      <form
        className="search"
        onSubmit={(e) => {
          e.preventDefault();
          setParam("devices", search || null);
        }}
      >
        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search devices"
          aria-label="Search devices"
        />
        <Button variant="outline" type="submit">Search</Button>
      </form>
      {loading && <Busy>Loading…</Busy>}
      {error && <Muted>{error}</Muted>}
      {data && data.devices.length === 0 && <Muted>No devices match.</Muted>}
      {data && data.devices.length > 0 && (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>State</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.devices.map((device: Device) => (
                <TableRow key={device.id}>
                  <TableCell title={device.id}>
                    {deviceLabel(device)}
                    {device.device_type_name && (
                      <span className="device-type" title={device.device_type_id}>
                        {device.device_type_name}
                      </span>
                    )}
                  </TableCell>
                  <TableCell>
                    <span className={`state ${device.connection_state || "unknown"}`}>
                      {device.connection_state || "unknown"}
                    </span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <Muted>
            {data.devices.length} shown{data.total > 0 && ` of ${data.total}`}
          </Muted>
        </>
      )}
    </Pane>
  );
}
