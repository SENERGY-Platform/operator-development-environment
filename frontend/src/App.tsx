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
import { AdminView } from "./admin";
import { ChatView } from "./chat";
import { ExplorationView } from "./exploration";
import { KernelView } from "./kernel";
import { logout } from "./keycloak";
import { ProfilerView } from "./profiler";
import { SelectionView } from "./selection";
import { Centered, Muted, Pane, describe, useLoad } from "./ui";

type View = "chat" | "ontology" | "selection" | "profiler" | "exploration" | "kernel" | "admin";

/**
 * The shell through M3. The pane layout of SPEC §2 (Chat / Data / Exploration /
 * Code / Experiment) arrives with the milestones that fill those panes; showing
 * empty docks now would be scaffolding, not progress. What exists is switched
 * between rather than crammed into one grid.
 */
export default function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [view, setView] = useState<View | null>(null);
  // A chart named from another pane — a render_chart result in chat, a profile in
  // the profiler view. Held here because it is the one piece of state two panes
  // hand to a third; cleared once the exploration pane has taken it, so returning
  // to that tab later does not re-open the same chart over the developer's own
  // selection.
  const [chart, setChart] = useState<string | null>(null);

  const openChart = useCallback((chartId: string) => {
    setChart(chartId);
    setView("exploration");
  }, []);
  const clearChart = useCallback(() => setChart(null), []);

  useEffect(() => {
    api
      .session()
      .then(setSession)
      .catch((e: unknown) => setError(describe(e)));
  }, []);

  if (error) return <FatalError message={error} />;
  if (!session) return <Centered>Loading session…</Centered>;

  // Chat is the surface a developer starts from once a provider is configured;
  // without one, the profiler, as before. Resolved after the session loads rather
  // than defaulted in useState, because which tabs exist depends on the answer.
  const current: View = view ?? (session.features.chat ? "chat" : "profiler");

  return (
    <div className="app">
      <Header session={session} view={current} onView={setView} />
      {current === "chat" && (
        <ChatView session={session} onOpenChart={session.features.charts ? openChart : undefined} />
      )}
      {current === "ontology" && (
        <main className="panes">
          <AspectTreePane />
          <FunctionsPane />
          <DevicesPane />
        </main>
      )}
      {current === "selection" && <SelectionView />}
      {current === "profiler" && (
        <ProfilerView onOpenChart={session.features.charts ? openChart : undefined} />
      )}
      {current === "exploration" && <ExplorationView focus={chart} onFocusHandled={clearChart} />}
      {current === "kernel" && <KernelView />}
      {current === "admin" && <AdminView />}
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
  // A tab is offered only when its surface is actually served, so a deployment
  // without a provider or without a timescale-wrapper shows what it has rather
  // than tabs that answer 404.
  const tabs: [View, string][] = [];
  if (session.features.chat) tabs.push(["chat", "Chat"]);
  tabs.push(["ontology", "Ontology"]);
  if (session.features.selection) tabs.push(["selection", "Selection"]);
  if (session.features.profiler) tabs.push(["profiler", "Profiler"]);
  if (session.features.charts) tabs.push(["exploration", "Exploration"]);
  if (session.features.kernel) tabs.push(["kernel", "Kernel"]);
  // §3.3's settings surface, gated on the realm role at the router as well; hiding
  // the tab is a courtesy, not the enforcement.
  if (session.is_admin && session.features.chat) tabs.push(["admin", "Settings"]);

  return (
    <header className="header">
      <div className="header-left">
        <span className="brand">ODE</span>
        <nav className="tabs">
          {tabs.map(([id, label]) => (
            <button key={id} className={view === id ? "active" : ""} onClick={() => onView(id)}>
              {label}
            </button>
          ))}
        </nav>
      </div>
      <div className="header-right">
        {/*
          SPEC §3.2: the exposure tier is surfaced persistently. What appears here is
          the default a *new* chat session starts at, plus the ceiling this developer
          may raise one to — a live tier is session-scoped and belongs beside the
          conversation it governs, which is where the chat view puts it.
        */}
        <span
          className="tier"
          title={
            "Data exposure tier for the LLM (SPEC §3.2). It gates LLM tools, not this UI. " +
            "New sessions start here; the live tier is shown in the chat pane."
          }
        >
          New sessions: {session.exposure_tier}
          {session.max_exposure_tier && session.max_exposure_tier !== "L2" && (
            <span className="tier-cap"> (max {session.max_exposure_tier})</span>
          )}
        </span>
        {session.spend && (
          <span className="spend" title="Estimated LLM spend this period (SPEC §3.3)">
            {session.spend.tokens.toLocaleString("en-GB")} tokens
          </span>
        )}
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
