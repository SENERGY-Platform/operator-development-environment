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

import { useCallback, useState } from "react";
import { api, deviceLabel, type AspectTreeNode, type Device, type OntologyFunction } from "./api";
import { setParam, useParam } from "./router";
import { Muted, Pane, useLoad } from "./ui";

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

function AspectTreePane() {
  const load = useCallback(() => api.aspectTree().then((r) => r.tree), []);
  const { data, error, loading } = useLoad(load);

  return (
    <Pane title="Aspects" subtitle="Hierarchical subsystems from the platform ontology">
      {loading && <Muted>Loading…</Muted>}
      {error && <Muted>{error}</Muted>}
      {data && data.length === 0 && <Muted>The ontology contains no aspects.</Muted>}
      {data && data.length > 0 && (
        <ul className="tree">
          {data.map((node) => (
            <TreeNode key={node.id} node={node} />
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
      <div className="tree-row">
        {hasChildren ? (
          <button className="twisty" onClick={() => setOpen(!open)} aria-expanded={open}>
            {open ? "▾" : "▸"}
          </button>
        ) : (
          <span className="twisty leaf">·</span>
        )}
        <span>{node.name || node.id}</span>
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
          <button
            key={t}
            className={t === rdfType ? "active" : ""}
            aria-pressed={t === rdfType}
            // Measuring is the default the pane opens at, so it is written as an
            // absent parameter rather than as ?functions=measuring — a URL should
            // name what was changed, not restate every default.
            onClick={() => setParam("functions", t === "measuring" ? null : t)}
          >
            {t}
          </button>
        ))}
      </div>
      {loading && <Muted>Loading…</Muted>}
      {error && <Muted>{error}</Muted>}
      {data && (
        <ul className="list">
          {data.map((fn: OntologyFunction) => (
            <li key={fn.id}>{fn.display_name || fn.name || fn.id}</li>
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
        <input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search devices"
          aria-label="Search devices"
        />
        <button type="submit">Search</button>
      </form>
      {loading && <Muted>Loading…</Muted>}
      {error && <Muted>{error}</Muted>}
      {data && data.devices.length === 0 && <Muted>No devices match.</Muted>}
      {data && data.devices.length > 0 && (
        <>
          <table className="grid">
            <thead>
              <tr>
                <th>Name</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {data.devices.map((device: Device) => (
                <tr key={device.id}>
                  <td title={device.id}>
                    {deviceLabel(device)}
                    {device.device_type_name && (
                      <span className="device-type" title={device.device_type_id}>
                        {device.device_type_name}
                      </span>
                    )}
                  </td>
                  <td>
                    <span className={`state ${device.connection_state || "unknown"}`}>
                      {device.connection_state || "unknown"}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <Muted>
            {data.devices.length} shown{data.total > 0 && ` of ${data.total}`}
          </Muted>
        </>
      )}
    </Pane>
  );
}
