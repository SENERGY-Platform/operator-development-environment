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

import type { Session } from "./api";
import { ExperimentsView } from "./experiments";
import { ExplorationView } from "./exploration";
import { KernelView } from "./kernel";
import { OntologyView } from "./ontology";
import { ProfilerView } from "./profiler";
import { RelationsView } from "./relations";
import { Link } from "./router";
import { SelectionView } from "./selection";
import { Muted, Pane } from "./ui";

/**
 * The instrumentation views, as data.
 *
 * One table, read by three things that would otherwise drift apart: the
 * "Under the hood" menu, the `/tools` index, and the resolver that turns a path
 * into a pane. Adding a view here adds it to all three.
 *
 * Every entry names the LLM tools it corresponds to, because that is what these
 * views are for. The assistant drives the platform through the tool surface of
 * §5.8; these panes are where a developer watches it work, or does the same thing
 * by hand when the assistant gets it wrong.
 */

type Feature = keyof Session["features"];

export interface ToolRoute {
  /** The path segment under /tools. */
  slug: string;
  label: string;
  /** One line: what this view is for. */
  summary: string;
  /** The tools of §5.8 that reach the same backend. */
  tools: string[];
  /**
   * The session feature that must be true for the view to work, or null when the
   * view needs nothing beyond the device repository, which is required config.
   */
  feature: Feature | null;
  /** What is absent from the deployment when `feature` is false. */
  missing: string;
}

/**
 * TIMESERIES is the same absence three times over. The profiler, the exploration
 * pane and the relational profiler all read series, so a deployment without a
 * timescale-wrapper serves none of them — and the backend says so at startup in
 * these words.
 */
const TIMESERIES = "`timescale_wrapper_url` is not configured, so this deployment serves no series reads.";

export const TOOL_ROUTES: ToolRoute[] = [
  {
    slug: "ontology",
    label: "Ontology",
    summary:
      "The platform's semantic model — aspects, functions, and the devices this account may read.",
    tools: ["search_ontology", "list_devices", "get_device_metadata"],
    feature: null,
    missing: "",
  },
  {
    slug: "selection",
    label: "Selection",
    summary:
      "An intent in words resolved to concrete series through the ontology, with the evidence behind every match.",
    tools: ["resolve_semantic_selection", "propose_data_selection"],
    feature: "selection",
    missing: "The semantic resolver is not served by this deployment.",
  },
  {
    slug: "profiler",
    label: "Profiler",
    summary:
      "Candidates ranked without reading a value, then the full per-series profile with every non-result shown as one.",
    tools: [
      "quick_profile",
      "profile_series",
      "get_sessions",
      "probe_availability",
      "probe_export_data",
      "estimate_read_cost",
    ],
    feature: "profiler",
    missing: TIMESERIES,
  },
  {
    slug: "exploration",
    label: "Exploration",
    summary:
      "Chart specifications drawn with your token — the assistant writes the spec, never the picture.",
    tools: ["render_chart", "preview_series"],
    feature: "charts",
    missing: TIMESERIES,
  },
  {
    slug: "relations",
    label: "Relations",
    summary:
      "Conditional patterns across devices, scoped by the aspect hierarchy, with each candidate rule left for you to confirm.",
    tools: ["propose_related_sets", "relate_series"],
    feature: "relations",
    missing: TIMESERIES,
  },
  {
    slug: "kernel",
    label: "Kernel",
    summary: "Your own Python in your own JupyterHub pod, against the workspace the repo is cloned into.",
    tools: ["run_code"],
    feature: "kernel",
    missing:
      "`jupyterhub_url` is not configured, so a developer cannot run code and `run_code` is declared but not callable.",
  },
  {
    slug: "experiments",
    label: "Experiments",
    summary: "Ray jobs and their MLflow runs, each tagged with the commit it was built from.",
    tools: ["launch_experiment", "get_experiment_results"],
    feature: "experiments",
    missing:
      "Neither `ray_url` nor `mlflow_url` is configured, so the experiment routes are not served " +
      "and both experiment tools stay declared but not callable. The surface needs both.",
  },
];

export function findToolRoute(slug: string): ToolRoute | undefined {
  return TOOL_ROUTES.find((route) => route.slug === slug);
}

/** available says whether the deployment actually serves a route's backend. */
export function available(route: ToolRoute, session: Session): boolean {
  return route.feature === null || session.features[route.feature];
}

/** The context a view needs from the shell. Kept small on purpose. */
export interface ViewContext {
  session: Session;
  /**
   * Opens a chart in the exploration pane. Absent when no exploration backend is
   * configured, in which case a render_chart result is shown as what it is.
   */
  openChart?: (chartId: string) => void;
}

/**
 * renderTool maps a route to its pane.
 *
 * Kept beside the table rather than in the shell so that a route and the thing it
 * renders cannot disagree about which view is which.
 */
export function renderTool(route: ToolRoute, context: ViewContext): React.ReactNode {
  switch (route.slug) {
    case "ontology":
      return <OntologyView />;
    case "selection":
      return <SelectionView />;
    case "profiler":
      return <ProfilerView onOpenChart={context.openChart} />;
    case "exploration":
      return <ExplorationView />;
    case "relations":
      return <RelationsView />;
    case "kernel":
      return <KernelView />;
    case "experiments":
      return <ExperimentsView session={context.session} />;
    default:
      return <NoSuchView />;
  }
}

/**
 * NotConfigured is what a route renders when its backend is absent.
 *
 * Not a blank pane and not a redirect. A developer who followed a link, or
 * reloaded a bookmark, is owed the reason — and the reason is a configuration key,
 * which is something they or their operator can act on. A silent redirect to the
 * start page teaches them the link was wrong when the deployment was.
 */
export function NotConfigured({ title, missing }: { title: string; missing: string }) {
  return (
    <main className="panes single">
      <Pane title={title} subtitle="Not served by this deployment">
        <p>{missing}</p>
        <Muted>
          ODE degrades by feature rather than by failing: the views whose backends are configured
          work, and the ones that are not say so. The corresponding LLM tools stay declared but
          are not callable, so the assistant is told the same thing you are.
        </Muted>
        <p>
          <Link to="/tools">Back to the tools index</Link>
        </p>
      </Pane>
    </main>
  );
}

/** The unknown-path card. A wrong URL is answered, not redirected away from. */
export function NoSuchView() {
  return (
    <main className="panes single">
      <Pane title="No such view" subtitle="Nothing is served at this path">
        <Muted>
          The address does not match any view. If it came from a bookmark, the route may have been
          renamed; if it came from a link, the link is wrong.
        </Muted>
        <p>
          <Link to="/">Back to the workspace</Link> · <Link to="/tools">the tools index</Link>
        </p>
      </Pane>
    </main>
  );
}

/**
 * The /tools index: one line per instrumentation view, and the tools that reach
 * the same backend.
 *
 * It exists so that "everything else is under the hood" does not mean "everything
 * else is hidden". A developer who wants to check what the assistant did needs to
 * know which pane shows it.
 */
export function ToolsIndex({ session }: { session: Session }) {
  return (
    <main className="panes single tools-index">
      <Pane
        title="Under the hood"
        subtitle="What the assistant drives, and where to watch it or take over"
      >
        <ul className="list tool-index-list flex flex-col gap-1">
          {TOOL_ROUTES.map((route) => {
            const served = available(route, session);
            return (
              <li key={route.slug} className={served ? undefined : "unserved"}>
                <Link className="tool-index-entry" to={`/tools/${route.slug}`}>
                  <span className="tool-index-label">{route.label}</span>
                  <span className="tool-index-summary">{route.summary}</span>
                  <span className="tool-index-tools">
                    {route.tools.map((tool) => (
                      <code key={tool}>{tool}</code>
                    ))}
                  </span>
                  {!served && <span className="badge inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs">not configured</span>}
                </Link>
              </li>
            );
          })}
        </ul>
        {session.is_admin && session.features.chat && (
          <p>
            <Link to="/settings">Settings</Link> — per-user and global LLM limits, the price table,
            accounting and the tool audit.
          </p>
        )}
      </Pane>
    </main>
  );
}
