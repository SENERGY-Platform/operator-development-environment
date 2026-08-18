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

import { useCallback, useEffect, useState } from "react";
import {
  api,
  deviceLabel,
  type AspectTreeNode,
  type Device,
  type OntologyFunction,
  type Session,
} from "./api";
import { logout } from "./keycloak";
import { ProfilerView } from "./profiler";
import { SelectionView } from "./selection";
import { Centered, Muted, Pane, describe, useLoad } from "./ui";

type View = "ontology" | "selection" | "profiler";

/**
 * M0 and M1 shell. The pane layout of SPEC §2 (Chat / Data / Exploration / Code
 * / Experiment) arrives with the milestones that fill those panes; showing five
 * empty docks now would be scaffolding, not progress. What exists is switched
 * between rather than crammed into one grid.
 */
export default function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [view, setView] = useState<View>("profiler");

  useEffect(() => {
    api
      .session()
      .then(setSession)
      .catch((e: unknown) => setError(describe(e)));
  }, []);

  if (error) return <FatalError message={error} />;
  if (!session) return <Centered>Loading session…</Centered>;

  return (
    <div className="app">
      <Header session={session} view={view} onView={setView} />
      {view === "ontology" && (
        <main className="panes">
          <AspectTreePane />
          <FunctionsPane />
          <DevicesPane />
        </main>
      )}
      {view === "selection" && <SelectionView />}
      {view === "profiler" && <ProfilerView />}
    </div>
  );
}

function Header({
  session,
  view,
  onView,
}: {
  session: Session;
  view: View;
  onView: (view: View) => void;
}) {
  return (
    <header className="header">
      <div className="header-left">
        <span className="brand">ODE</span>
        <nav className="tabs">
          {(
            [
              ["ontology", "Ontology"],
              ["selection", "Selection"],
              ["profiler", "Profiler"],
            ] as [View, string][]
          ).map(([id, label]) => (
            <button key={id} className={view === id ? "active" : ""} onClick={() => onView(id)}>
              {label}
            </button>
          ))}
        </nav>
      </div>
      <div className="header-right">
        {/*
          SPEC §3.2: the current exposure tier is surfaced persistently. It gates
          what the LLM may be given, not what the developer may see, which is why
          the profiler below is reachable at L0.
        */}
        <span
          className="tier"
          title="Data exposure tier for the LLM (SPEC §3.2). It gates LLM tools, not this UI."
        >
          Tier {session.exposure_tier}
        </span>
        <span className="user">
          {session.username}
          {session.is_admin && <span className="badge">admin</span>}
        </span>
        <button onClick={logout}>Sign out</button>
      </div>
    </header>
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
  const [rdfType, setRdfType] = useState<"measuring" | "controlling">("measuring");
  const load = useCallback(() => api.functions(rdfType).then((r) => r.functions), [rdfType]);
  const { data, error, loading } = useLoad(load);

  return (
    <Pane title="Functions" subtitle="What the platform can measure and control">
      <div className="toggle">
        {(["measuring", "controlling"] as const).map((t) => (
          <button key={t} className={t === rdfType ? "active" : ""} onClick={() => setRdfType(t)}>
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
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
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
          setQuery(search);
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

function FatalError({ message }: { message: string }) {
  return (
    <Centered>
      <div className="fatal">
        <h1>ODE could not start</h1>
        <p>{message}</p>
        <button onClick={logout}>Sign out</button>
      </div>
    </Centered>
  );
}
