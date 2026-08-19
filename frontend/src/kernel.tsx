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

import { useCallback, useEffect, useRef, useState } from "react";
import { api, type KernelEvent, type KernelFile, type KernelStatus } from "./api";
import { Muted, Pane, bytes, dateTime, describe, shortId } from "./ui";
import { odeSocket } from "./ws";

/**
 * The Kernel view (SPEC §5.6, M4).
 *
 * A console rather than a notebook. The Code pane of §5.11 — a full file tree
 * with Monaco and write access on every file — is M7; what this needs to show is
 * narrower and specific: that a developer runs their own Python in their own pod,
 * and that a file written in one session is there in the next.
 *
 * Two things on screen are load-bearing rather than decorative. The workspace
 * path is shown beside the file list because only that directory is on the
 * per-user PVC — anything written elsewhere is gone when the pod is culled, and a
 * developer has no other way to tell. And the pod's state is shown while it
 * spawns, because a cold start is up to a minute (§5.6) and an unexplained
 * minute of nothing reads as a broken tool.
 */
export function KernelView() {
  const [status, setStatus] = useState<KernelStatus | null>(null);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [starting, setStarting] = useState(true);

  // Pre-warm on open, as §5.6 asks: the cold start happens while the developer is
  // reading rather than after they press run.
  useEffect(() => {
    let live = true;
    api
      .kernelEnsure()
      .then((next) => {
        if (live) setStatus(next);
      })
      .catch((e: unknown) => {
        if (live) setStatusError(describe(e));
      })
      .finally(() => {
        if (live) setStarting(false);
      });
    return () => {
      live = false;
    };
  }, []);

  const refresh = useCallback(async () => {
    try {
      setStatus(await api.kernelStatus());
    } catch (e: unknown) {
      setStatusError(describe(e));
    }
  }, []);

  return (
    <main className="panes kernel-layout">
      <KernelPane
        status={status}
        starting={starting}
        error={statusError}
        onStatus={setStatus}
        onError={setStatusError}
      />
      <WorkspacePane status={status} onRefreshStatus={refresh} />
    </main>
  );
}

interface Cell {
  code: string;
  events: KernelEvent[];
  finished: boolean;
  status?: string;
}

function KernelPane({
  status,
  starting,
  error,
  onStatus,
  onError,
}: {
  status: KernelStatus | null;
  starting: boolean;
  error: string | null;
  onStatus: (status: KernelStatus) => void;
  onError: (error: string | null) => void;
}) {
  const [code, setCode] = useState(
    'import os, platform\nprint(platform.python_version(), os.getcwd())\n',
  );
  const [cells, setCells] = useState<Cell[]>([]);
  const [running, setRunning] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const transcript = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    transcript.current?.scrollTo({ top: transcript.current.scrollHeight });
  }, [cells]);

  const run = useCallback(async () => {
    if (running || !code.trim()) return;
    const index = cells.length;
    setCells((previous) => [...previous, { code, events: [], finished: false }]);
    setRunning(true);
    onError(null);

    const current = new AbortController();
    controller.current = current;
    try {
      await odeSocket.execute(code, {
        signal: current.signal,
        onEvent: (event) =>
          setCells((previous) =>
            previous.map((cell, at) =>
              at === index
                ? {
                    ...cell,
                    events: [...cell.events, event],
                    finished: cell.finished || event.kind === "done",
                    status: event.kind === "done" ? event.status : cell.status,
                  }
                : cell,
            ),
          ),
      });
    } catch (e: unknown) {
      onError(describe(e));
    } finally {
      controller.current = null;
      setRunning(false);
      setCells((previous) =>
        previous.map((cell, at) => (at === index ? { ...cell, finished: true } : cell)),
      );
      try {
        onStatus(await api.kernelStatus());
      } catch {
        // The status is a nicety here; the transcript already says what happened.
      }
    }
  }, [cells.length, code, onError, onStatus, running]);

  const act = useCallback(
    async (action: () => Promise<unknown>) => {
      onError(null);
      try {
        await action();
        onStatus(await api.kernelStatus());
      } catch (e: unknown) {
        onError(describe(e));
      }
    },
    [onError, onStatus],
  );

  return (
    <Pane
      title="Kernel"
      subtitle="Python in this developer's own pod, with this developer's own platform access"
      actions={
        <>
          <button onClick={() => void act(api.kernelRestart)} disabled={running}>
            Restart
          </button>
          <button onClick={() => void act(api.kernelShutdown)} disabled={running}>
            Shut down
          </button>
        </>
      }
    >
      <KernelStatusLine status={status} starting={starting} />
      {error && <p className="notice notice-error">{error}</p>}

      <div className="cells" ref={transcript}>
        {cells.length === 0 && (
          <Muted>
            Nothing has run yet. The kernel keeps its state between cells, and its working
            directory is the workspace shown beside this pane.
          </Muted>
        )}
        {cells.map((cell, index) => (
          <CellView key={index} cell={cell} />
        ))}
      </div>

      <div className="composer">
        <textarea
          value={code}
          spellCheck={false}
          rows={6}
          onChange={(e) => setCode(e.target.value)}
          onKeyDown={(e) => {
            // Ctrl+Enter runs, as in every notebook. Enter stays a newline: this is
            // a code editor, not a chat box.
            if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
              e.preventDefault();
              void run();
            }
          }}
          placeholder="Python. Ctrl+Enter to run."
          aria-label="Python to run in the kernel"
        />
        <div className="composer-actions">
          <span className="muted-inline">
            Ctrl+Enter to run. Output is capped; write large results to a file in the workspace.
          </span>
          {running ? (
            <button onClick={() => controller.current?.abort()}>Interrupt</button>
          ) : (
            <button onClick={() => void run()} disabled={!code.trim() || starting}>
              Run
            </button>
          )}
        </div>
      </div>
    </Pane>
  );
}

function KernelStatusLine({ status, starting }: { status: KernelStatus | null; starting: boolean }) {
  if (starting && !status) {
    return (
      <p className="notice notice-info">
        Starting the pod. A cold start takes up to a minute; nothing is lost while it does.
      </p>
    );
  }
  if (!status) return null;

  const state = status.server_pending
    ? status.server_pending
    : status.server_ready
      ? "ready"
      : "stopped";

  return (
    <dl className="kv kernel-status">
      <dt>Pod</dt>
      <dd>
        <span className={`state ${status.server_ready ? "online" : "unknown"}`}>{state}</span>
        {status.profile && <span className="device-type">profile {status.profile}</span>}
      </dd>
      <dt>Kernel</dt>
      <dd>
        {status.kernel_id ? (
          <>
            <code>{shortId(status.kernel_id)}</code>
            {status.kernel_name && <span className="muted-inline"> {status.kernel_name}</span>}
            {status.busy && <span className="tag warn">busy</span>}
          </>
        ) : (
          <span className="muted-inline">none</span>
        )}
      </dd>
      {status.last_activity && (
        <>
          <dt>Last activity</dt>
          <dd>{dateTime(status.last_activity)}</dd>
        </>
      )}
    </dl>
  );
}

function CellView({ cell }: { cell: Cell }) {
  const truncated = cell.events.some((event) => event.truncated);

  return (
    <article className={`cell ${cell.status === "error" ? "cell-error" : ""}`}>
      <pre className="cell-code">{cell.code}</pre>
      <div className="cell-output">
        {cell.events.map((event, index) => (
          <EventView key={index} event={event} />
        ))}
        {!cell.finished && <span className="cell-running">running…</span>}
      </div>
      {truncated && (
        <p className="notice notice-warn">
          Output was truncated at the byte cap. What is missing is not shown anywhere — write it
          to a file in the workspace instead.
        </p>
      )}
      {cell.finished && cell.status && cell.status !== "ok" && (
        <p className="cell-status">finished: {cell.status}</p>
      )}
    </article>
  );
}

function EventView({ event }: { event: KernelEvent }) {
  switch (event.kind) {
    case "stream":
      return <pre className={`out ${event.stream === "stderr" ? "stderr" : "stdout"}`}>{event.text}</pre>;

    case "execute_result":
    case "display_data":
      return (
        <div className="out result">
          {event.text && <pre>{event.text}</pre>}
          {/*
            Images are rendered here and nowhere else. §5.9 makes an LLM's chart a
            declarative spec rather than an image, and run_code never returns one to
            a model — but a developer's own matplotlib figure is the point of having
            run the cell, so their console keeps it.
          */}
          {event.mime &&
            Object.entries(event.mime)
              .filter(([media]) => media.startsWith("image/"))
              .map(([media, payload]) => (
                <img key={media} src={`data:${media};base64,${payload}`} alt="cell output" />
              ))}
        </div>
      );

    case "error":
      return (
        <pre className="out cell-traceback">
          {event.text || `${event.error_name ?? "error"}: ${event.error_value ?? ""}`}
        </pre>
      );

    case "done":
      return event.error ? <p className="notice notice-warn">{event.error}</p> : null;

    // status and execute_input are how the pane knows a cell started; they carry
    // nothing a developer needs to read.
    default:
      return null;
  }
}

function WorkspacePane({
  status,
  onRefreshStatus,
}: {
  status: KernelStatus | null;
  onRefreshStatus: () => Promise<void>;
}) {
  const [entries, setEntries] = useState<KernelFile[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const listing = await api.kernelFiles();
      setEntries(listing.entries);
    } catch (e: unknown) {
      setError(describe(e));
    } finally {
      setLoading(false);
    }
  }, []);

  // Reloaded whenever a kernel appears or is replaced, which is when a file is
  // most likely to have just been written.
  useEffect(() => {
    if (status?.server_ready) void load();
  }, [load, status?.kernel_id, status?.server_ready]);

  return (
    <Pane
      title="Workspace"
      subtitle="Persistent storage. A file written here is present in the next session"
      actions={
        <button
          onClick={() => {
            void load();
            void onRefreshStatus();
          }}
        >
          Refresh
        </button>
      }
    >
      {status && (
        <p className="workspace-path">
          <code>{status.workspace}</code>
          <span className="muted-inline">
            {" "}
            — the kernel's working directory, on the per-user volume. Anything written outside it
            is lost when the pod is culled.
          </span>
        </p>
      )}
      {loading && <Muted>Loading…</Muted>}
      {error && <Muted>{error}</Muted>}
      {entries && entries.length === 0 && <Muted>The workspace is empty.</Muted>}
      {entries && entries.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th>Name</th>
              <th className="numeric">Size</th>
              <th>Modified</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={entry.path}>
                <td>
                  {entry.type === "directory" ? "▸ " : ""}
                  {entry.name}
                </td>
                <td className="numeric">{entry.type === "directory" ? "" : bytes(entry.size)}</td>
                <td>{entry.last_modified ? dateTime(entry.last_modified) : ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Pane>
  );
}
