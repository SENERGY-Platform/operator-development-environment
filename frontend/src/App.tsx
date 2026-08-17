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
  ApiError,
  type AspectTreeNode,
  type Device,
  type OntologyFunction,
  type Session,
} from "./api";
import { logout } from "./keycloak";

/**
 * M0 shell. The pane layout of SPEC §2 (Chat / Data / Exploration / Code /
 * Experiment) arrives with the milestones that fill those panes; showing five
 * empty docks now would be scaffolding, not progress.
 */
export default function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);

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
      <Header session={session} />
      <main className="panes">
        <AspectTreePane />
        <FunctionsPane />
        <DevicesPane />
      </main>
    </div>
  );
}

function Header({ session }: { session: Session }) {
  return (
    <header className="header">
      <div>
        <span className="brand">ODE</span>
        <span className="subtitle">Operator Development Environment</span>
      </div>
      <div className="header-right">
        {/* SPEC §3.2: the current exposure tier is surfaced persistently. */}
        <span className="tier" title="Data exposure tier for the LLM (SPEC §3.2)">
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
          <table className="devices">
            <thead>
              <tr>
                <th>Name</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {data.devices.map((device: Device) => (
                <tr key={device.id}>
                  <td>{device.name || device.id}</td>
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

// --- shared bits ---

function useLoad<T>(load: () => Promise<T>) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    load()
      .then((result) => {
        if (!cancelled) setData(result);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(describe(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [load]);

  return { data, error, loading };
}

function describe(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.isForbidden) {
      return "Forbidden. This account is missing the `developer` realm role, or may not read this resource.";
    }
    return `${e.status}: ${e.message}`;
  }
  return e instanceof Error ? e.message : String(e);
}

function Pane({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle: string;
  children: React.ReactNode;
}) {
  return (
    <section className="pane">
      <h2>{title}</h2>
      <p className="pane-subtitle">{subtitle}</p>
      <div className="pane-body">{children}</div>
    </section>
  );
}

function Muted({ children }: { children: React.ReactNode }) {
  return <p className="muted">{children}</p>;
}

function Centered({ children }: { children: React.ReactNode }) {
  return <div className="centered">{children}</div>;
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
