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

import { Suspense, lazy, useCallback, useEffect, useRef, useState } from "react";
import { api, type Session } from "./api";
import { AdminView } from "./admin";
import { ChatView } from "./chat";
import { logout } from "./keycloak";
import { navigate, useLocation } from "./router";
import {
  NoSuchView,
  NotConfigured,
  ToolsIndex,
  available,
  findToolRoute,
  renderTool,
} from "./routes";
import { Header, Split } from "./shell";
import { Centered, describe } from "./ui";
import { WorkbenchProvider } from "./workbench";
import { Button } from "@/components/ui/button";

/**
 * The Code view is loaded on demand, unlike every other pane.
 *
 * It contains Monaco, which is several megabytes of JavaScript — more than the
 * whole of the rest of the SPA. Bundling it into the initial chunk would make a
 * developer who never opens it pay for the editor on every load, so it is a
 * separate chunk fetched the first time the workspace is shown. The workspace is
 * now the start page, so most loads do fetch it — but behind the shell, which
 * paints and is usable while it arrives, and `/chat` and every `/tools/…` route
 * still never touch it.
 */
const CodeView = lazy(async () => ({ default: (await import("./code")).CodeView }));

/** What is missing from a deployment that serves no LLM. The backend says this at startup. */
const NO_PROVIDER =
  "No LLM provider is configured, so the chat, tool and admin routes are not served. " +
  "Set anthropic_api_key, openai_api_key, compatible_base_url or claude_cli_enabled.";

/** What is missing from a deployment that cannot hold a repository. */
const NO_REPO =
  "`github_client_id` is not configured, so a developer cannot connect a repository and " +
  "`write_file` is declared but not callable. The surface also needs a `jupyterhub_url`, " +
  "because the working copy lives on the developer's own pod.";

/**
 * The shell.
 *
 * Two things it does that the flat tab bar before it did not.
 *
 * It puts the workspace — the conversation and the repository, side by side — at
 * `/`, and everything else behind `/tools`. That is not a cosmetic reordering: the
 * assistant writing a file and the developer correcting it is the loop ODE exists
 * for, and every other pane is instrumentation for watching that loop or taking
 * it over by hand.
 *
 * And it puts state in the URL. A reload used to land on the start page, which
 * for a five-minute profile or a half-written commit message is the difference
 * between a tool and a toy. What is being looked at now lives in the path and the
 * query, so a reload, a bookmark and a link to a colleague all mean the same
 * thing.
 */
export default function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const { pathname } = useLocation();

  useEffect(() => {
    api
      .session()
      .then(setSession)
      .catch((e: unknown) => setError(describe(e)));
  }, []);

  // GitHub's OAuth redirect lands here rather than on the backend (§5.11 item 1):
  // the SPA holds a platform token and the backend's routes all require one, so
  // completing the flow from here means there is no unauthenticated endpoint that
  // takes a code and writes a credential.
  const [callback, setCallback] = useState<GitHubCallback | null>(() => readCallback());

  /*
   * A refused authorisation is not a failed start, so it does not share `error`.
   *
   * `error` above means the session could not be read: nothing is on screen, there
   * is nothing to try, and signing out is the only thing left — which is what
   * FatalError offers. Pressing Cancel on GitHub's consent screen is the opposite
   * of that. Nothing is broken; the developer decided not to connect a repository,
   * and the redirect says so with ?error=access_denied. Sharing one channel put
   * that decision behind a full-screen "ODE could not start", and took every
   * repoConnect failure — a state mismatch, a 502 from the gateway — with it. The
   * callback answers here instead, beside the workspace it sends the developer to.
   */
  const [connectError, setConnectError] = useState<string | null>(null);

  /*
   * The exchange is guarded on the code, not on the effect's cleanup.
   *
   * An authorisation code is single-use, and StrictMode runs an effect twice in the
   * dev build — mount, cleanup, mount. A `live` flag set by the cleanup does not
   * stop the second request; it only discards the result of the first. Which is the
   * wrong way round: the first run is the one that succeeds, and the second, posting
   * a code GitHub has by then spent, is the one that reports. Connecting a
   * repository therefore appeared to fail every time in local development while the
   * backend had in fact written the credential — and, before the previous note,
   * said so on a full-screen fatal page.
   *
   * A ref carries across the simulated remount, because the component instance
   * does; keying it on the code rather than on a bare boolean also makes a second,
   * genuinely different callback in the same tab work.
   */
  const exchanged = useRef<string | null>(null);

  useEffect(() => {
    if (!callback) return;

    // Cleared out of the address first — before the request rather than after it.
    // A spent code must not be replayable by a reload or by the back button, and
    // that has to hold while the exchange is in flight, which is exactly the window
    // a developer is most likely to reload in because the screen says "completing".
    // The workspace is where they wanted to end up: the repository they just
    // authorised is its right-hand pane.
    navigate("/", { replace: true });

    if (callback.error) {
      setCallback(null);
      setConnectError(`GitHub refused the authorisation: ${callback.error}`);
      return;
    }

    if (exchanged.current === callback.code) return;
    exchanged.current = callback.code;

    const done = (failure: string | null) => {
      setCallback(null);
      setConnectError(failure);
    };
    api
      .repoConnect(callback.code, callback.state)
      .then(() => done(null))
      .catch((e: unknown) => done(describe(e)));
    // No cleanup that cancels: the guard above is what makes this run once, and a
    // flag here would only throw away the answer to the request that worked.
  }, [callback]);

  if (error) return <FatalError message={error} />;
  if (!session) return <Centered><span className="busy animate-pulse">Loading session…</span></Centered>;
  if (callback)
    return (
      <Centered>
        <span className="busy animate-pulse">Completing the GitHub authorisation…</span>
      </Centered>
    );

  return (
    <div className="app">
      <Header session={session} />
      {connectError && (
        <p className="notice notice-error connect-notice">
          <span>{connectError}</span>
          {/*
            Dismissible rather than timed. The developer is the one who knows
            whether they meant to cancel or want to try again, and the button that
            starts the flow over is in the code pane below this line.
          */}
          <Button variant="outline" onClick={() => setConnectError(null)}>Dismiss</Button>
        </p>
      )}
      {/*
        Around the routed panes rather than inside one of them: the Code pane shows
        the workbench, and the chat pane needs to know which one a new conversation
        should act in. A deployment with no repository surface has no workbenches,
        and the provider answers with an empty state.
      */}
      <WorkbenchProvider>
        <Routed session={session} pathname={pathname} />
      </WorkbenchProvider>
    </div>
  );
}

/**
 * Routed turns a path into a pane.
 *
 * The workspace and every instrumentation view render through the same `Split` at
 * the same place in the tree, which is what keeps the conversation mounted while a
 * developer moves between them: React reconciles by position, so only the
 * right-hand pane is swapped. `?session=` would restore the conversation either
 * way; not remounting it means the scroll position and the half-typed message
 * survive too.
 */
function Routed({ session, pathname }: { session: Session; pathname: string }) {
  // A chart proposed in chat or built from a profile is opened by navigating,
  // rather than by handing a chart id across panes through the shell's state. The
  // navigation is the same thing the hand-off was, minus the state — and it leaves
  // a URL that can be reloaded, bookmarked and sent to someone else.
  const openChart = useCallback((chartId: string) => {
    navigate(`/tools/exploration?chart=${encodeURIComponent(chartId)}`);
  }, []);
  const chartOpener = session.features.charts ? openChart : undefined;

  const chat = session.features.chat ? (
    <ChatView session={session} onOpenChart={chartOpener} />
  ) : (
    <NotConfigured title="Chat" missing={NO_PROVIDER} />
  );

  if (pathname === "/") {
    const code = session.features.repo ? (
      <Suspense fallback={<Centered><span className="busy animate-pulse">Loading the editor…</span></Centered>}>
        <CodeView session={session} />
      </Suspense>
    ) : (
      <NotConfigured title="Code" missing={NO_REPO} />
    );
    // Without a provider there is no conversation to put beside the code, and half
    // an empty screen would be worse than none: the code pane takes the width.
    if (!session.features.chat) return code;
    return <Split left={chat} right={code} leftLabel="Chat" rightLabel="Code" />;
  }

  if (pathname === "/chat") {
    return session.features.chat ? chat : <NotConfigured title="Chat" missing={NO_PROVIDER} />;
  }

  if (pathname === "/tools") return <ToolsIndex session={session} />;

  if (pathname.startsWith("/tools/")) {
    const route = findToolRoute(pathname.slice("/tools/".length));
    if (!route) return <NoSuchView />;
    const view = available(route, session) ? (
      renderTool(route, { session, openChart: chartOpener })
    ) : (
      <NotConfigured title={route.label} missing={route.missing} />
    );
    if (!session.features.chat) return view;
    return <Split left={chat} right={view} leftLabel="Chat" rightLabel={route.label} />;
  }

  if (pathname === "/settings") {
    if (!session.features.chat) return <NotConfigured title="Settings" missing={NO_PROVIDER} />;
    // The backend enforces the realm role on the route; this only avoids sending a
    // developer to a screen that will answer 403 (§3.3).
    if (!session.is_admin) {
      return (
        <NotConfigured
          title="Settings"
          missing="This account does not hold the `admin` realm role, which is what gates the settings surface."
        />
      );
    }
    return <AdminView />;
  }

  return <NoSuchView />;
}

/** The code and state GitHub sent back, or the error it sent instead. */
interface GitHubCallback {
  code: string;
  state: string;
  error?: string;
}

/**
 * readCallback reads the OAuth redirect out of the URL.
 *
 * The path is matched rather than assumed so that a developer who bookmarked the
 * SPA with a stale query does not send a spent code on every load. Matched as a
 * suffix, which is what keeps it correct when the SPA is served under a sub-path.
 * Called once, during the first render, because the query is cleared as soon as it
 * is used.
 */
function readCallback(): GitHubCallback | null {
  if (!window.location.pathname.endsWith("/github/callback")) return null;
  const query = new URLSearchParams(window.location.search);
  const error = query.get("error_description") ?? query.get("error") ?? undefined;
  const code = query.get("code") ?? "";
  const state = query.get("state") ?? "";
  if (!error && (!code || !state)) return null;
  return { code, state, error };
}

function FatalError({ message }: { message: string }) {
  return (
    <Centered>
      <div className="fatal">
        <h1>ODE could not start</h1>
        <p>{message}</p>
        <Button variant="outline" onClick={logout}>Sign out</Button>
      </div>
    </Centered>
  );
}
