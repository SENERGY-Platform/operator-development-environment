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
import {
  ApiError,
  TIERS,
  TIER_EXPOSES,
  api,
  type ChatEvent,
  type ChatMessage,
  type ChatSession,
  type PendingConfirmation,
  type ProviderInfo,
  type Session,
  type Tier,
  type ToolResult,
  type ToolSurface,
  type Usage,
} from "./api";
import { Cancelled, odeSocket, type SocketState } from "./ws";
import { setParam, useParam } from "./router";
import { Muted, Pane, Section, dateTime, describe, num, shortId } from "./ui";

/**
 * The chat pane of SPEC §2, and the surface where §3.2's exposure tier becomes
 * something a developer operates rather than a value in a config file.
 *
 * Three things here are deliberate rather than incidental:
 *
 *   - the tier control sits beside the conversation, not in a settings dialog,
 *     because it is the developer's data-governance decision and it changes what
 *     the next message can do;
 *   - a tool call is shown with its arguments and its result, so "the assistant
 *     read the ontology" is visible rather than implied;
 *   - a refusal is rendered as a refusal. A tool blocked by the tier shows what it
 *     would have needed, which is the point of §3.2's structured refusal — hiding
 *     it would leave the developer wondering why the answer was thin.
 */

type Turn =
  | { kind: "message"; message: ChatMessage }
  | { kind: "streaming"; text: string }
  | {
      kind: "tool";
      call: { id: string; name: string; input: unknown };
      result?: ToolResult;
      /** The latest progress line, replaced rather than accumulated. */
      progress?: string;
    }
  | { kind: "notice"; level: "info" | "warn" | "error"; text: string };

export function ChatView({
  session,
  onOpenChart,
}: {
  session: Session;
  /**
   * Opens a chart the assistant proposed in the exploration pane. Absent when no
   * exploration backend is configured, in which case a render_chart result is
   * shown as what it is — a stored specification nothing can draw here.
   */
  onOpenChart?: (chartId: string) => void;
}) {
  const [sessions, setSessions] = useState<ChatSession[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [surface, setSurface] = useState<ToolSurface | null>(null);
  const [providers, setProviders] = useState<ProviderInfo[]>(session.providers ?? []);

  // Which conversation is open is the URL's, not this component's. It is the one
  // piece of state that has to survive moving to the profiler and back, and a
  // reload of any view — so it is a query parameter rather than a useState, and
  // the pane derives from it instead of mirroring it. `?session=` is sticky in the
  // router, so every link in the shell carries it forward.
  const openId = useParam("session");
  const current = sessions?.find((entry) => entry.id === openId) ?? null;
  // A named session that is not in the list is a stale link or another account's,
  // and saying so beats silently opening nothing.
  const missingSession = sessions !== null && openId !== null && current === null;

  useEffect(() => {
    let cancelled = false;
    Promise.all([api.chatSessions(), api.toolSurface(), api.providers()])
      .then(([listed, tools, described]) => {
        if (cancelled) return;
        setSessions(listed.sessions);
        setSurface(tools);
        setProviders(described.providers);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(describe(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const create = useCallback(async (provider: string) => {
    setError(null);
    try {
      // Always L0. §3.2 makes it the default, and starting anywhere else would be
      // ODE choosing the developer's data exposure for them.
      const created = await api.createChatSession({ provider, exposure_tier: "L0" });
      setSessions((existing) => [created, ...(existing ?? [])]);
      setParam("session", created.id);
    } catch (e: unknown) {
      setError(describe(e));
    }
  }, []);

  const remove = useCallback(
    async (id: string) => {
      try {
        await api.deleteChatSession(id);
        setSessions((existing) => (existing ?? []).filter((entry) => entry.id !== id));
        // Deleting the open one leaves the parameter naming something that no longer
        // exists, which would read as a broken link on the next reload. Only the open
        // one: `session` is sticky, so clearing it unconditionally would close the
        // conversation the developer is reading because they tidied up a different
        // one — and close it on every /tools/… route at the same time, since the
        // conversation sits in the left half of all of them.
        if (id === openId) setParam("session", null);
      } catch (e: unknown) {
        setError(describe(e));
      }
    },
    [openId],
  );

  return (
    <main className="panes chat-layout">
      <Pane title="Sessions" subtitle="One conversation per problem you are working on">
        {error && <Muted>{error}</Muted>}
        <NewSession
          providers={providers}
          maxTier={session.max_exposure_tier ?? "L2"}
          onCreate={create}
        />
        {sessions === null && <Muted>Loading…</Muted>}
        {sessions?.length === 0 && <Muted>No sessions yet.</Muted>}
        {missingSession && (
          <Muted>The conversation named in the address is not in this account&apos;s list.</Muted>
        )}
        {sessions && sessions.length > 0 && (
          <ul className="list session-list">
            {sessions.map((entry) => (
              <li key={entry.id} className={current?.id === entry.id ? "active" : ""}>
                <button className="session-open" onClick={() => setParam("session", entry.id)}>
                  <span className="session-title">{entry.title || shortId(entry.id)}</span>
                  <span className="session-meta">
                    <span className={`tier tier-${entry.exposure_tier}`}>
                      {entry.exposure_tier}
                    </span>
                    {entry.provider} · {entry.message_count} messages
                  </span>
                </button>
                <button
                  className="session-delete"
                  title="Delete this session"
                  onClick={() => void remove(entry.id)}
                >
                  ×
                </button>
              </li>
            ))}
          </ul>
        )}
        {surface && (
          <ToolSurfaceSummary surface={surface} tier={current?.exposure_tier ?? "L0"} />
        )}
      </Pane>

      {current ? (
        <Conversation
          key={current.id}
          session={current}
          maxTier={session.max_exposure_tier ?? "L2"}
          surface={surface}
          onOpenChart={onOpenChart}
          onSessionChange={(updated) => {
            setSessions((existing) =>
              (existing ?? []).map((entry) => (entry.id === updated.id ? updated : entry)),
            );
          }}
        />
      ) : (
        <Pane title="Assistant" subtitle="Pick a session, or start one">
          <Muted>
            The assistant reaches the platform only through ODE's tool surface, and only as far
            as the session's exposure tier allows.
          </Muted>
        </Pane>
      )}
    </main>
  );
}

function NewSession({
  providers,
  maxTier,
  onCreate,
}: {
  providers: ProviderInfo[];
  maxTier: Tier;
  onCreate: (provider: string) => void;
}) {
  const [provider, setProvider] = useState("");
  const chosen = provider || providers.find((entry) => entry.default)?.name || providers[0]?.name || "";

  return (
    <form
      className="new-session"
      onSubmit={(e) => {
        e.preventDefault();
        onCreate(chosen);
      }}
    >
      <select
        value={chosen}
        onChange={(e) => setProvider(e.target.value)}
        aria-label="Provider"
      >
        {providers.map((entry) => (
          <option key={entry.name} value={entry.name}>
            {entry.name}
            {entry.capabilities.degraded ? " (degraded)" : ""}
          </option>
        ))}
      </select>
      <button type="submit" disabled={providers.length === 0}>
        New session
      </button>
      {maxTier !== "L2" && (
        <span className="hint">An administrator has capped your exposure tier at {maxTier}.</span>
      )}
      {providers.some((entry) => entry.capabilities.degraded) && (
        <ul className="degraded-list">
          {providers
            .filter((entry) => entry.capabilities.degraded)
            .map((entry) => (
              <li key={entry.name}>
                <strong>{entry.name}</strong>: {entry.capabilities.degraded_reason}
              </li>
            ))}
        </ul>
      )}
    </form>
  );
}

/** ToolSurfaceSummary publishes §5.8's table, including what has no tool at all. */
function ToolSurfaceSummary({ surface, tier }: { surface: ToolSurface; tier: Tier }) {
  const available = new Set(surface.tiers.find((entry) => entry.tier === tier)?.available ?? []);

  return (
    <Section
      title="Tool surface"
      note={`${available.size} available at ${tier}`}
      defaultOpen={false}
    >
      <table className="grid tools">
        <thead>
          <tr>
            <th>Tool</th>
            <th>Tier</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {surface.tools.map((tool) => (
            <tr key={tool.name} className={available.has(tool.name) ? "" : "unavailable"}>
              <td title={tool.description}>
                {tool.name}
                {tool.confirm && (
                  <span className="badge" title="Needs your explicit confirmation">
                    confirm
                  </span>
                )}
              </td>
              <td>
                <span className={`tier tier-${tool.min_tier}`}>{tool.min_tier}</span>
              </td>
              <td>
                {!tool.implemented ? (
                  <span className="muted" title={tool.unavailable}>
                    not in this build
                  </span>
                ) : available.has(tool.name) ? (
                  "available"
                ) : (
                  <span className="muted">needs {tool.min_tier}</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <Section title="Deliberately has no tool" defaultOpen={false}>
        <Muted>
          These are developer actions. There is no tool for any of them, so the assistant cannot
          do them at all — it is not a matter of being refused.
        </Muted>
        <dl className="kv denied">
          {Object.entries(surface.denied).map(([name, reason]) => (
            <div key={name} className="denied-row">
              <dt>{name}</dt>
              <dd>{reason}</dd>
            </div>
          ))}
        </dl>
      </Section>
    </Section>
  );
}

function Conversation({
  session,
  maxTier,
  surface,
  onSessionChange,
  onOpenChart,
}: {
  session: ChatSession;
  maxTier: Tier;
  surface: ToolSurface | null;
  onSessionChange: (session: ChatSession) => void;
  onOpenChart?: (chartId: string) => void;
}) {
  const [turns, setTurns] = useState<Turn[]>([]);
  const [pending, setPending] = useState<PendingConfirmation[]>([]);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [usage, setUsage] = useState<Usage | null>(null);
  const controller = useRef<AbortController | null>(null);
  const bottom = useRef<HTMLDivElement | null>(null);

  // Load the stored conversation. A resumed session shows its tool calls, because
  // the backend stores them structurally rather than flattening them to text.
  useEffect(() => {
    let cancelled = false;
    api
      .chatSession(session.id)
      .then((detail) => {
        if (cancelled) return;
        setTurns(replay(detail.messages));
        setPending(detail.pending_confirmations);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(describe(e));
      });
    return () => {
      cancelled = true;
    };
  }, [session.id]);

  useEffect(() => {
    bottom.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [turns]);

  // Detaching this view on unmount. It does not stop the exchange — that is the
  // point of detaching it server-side — so navigating away mid-profile leaves the
  // turn to finish and persist.
  useEffect(() => () => controller.current?.abort(), []);


  const consume = useCallback((event: ChatEvent) => {
    setTurns((existing) => apply(existing, event));
    if (event.type === "confirmation_required" && event.confirmation) {
      const confirmation = event.confirmation;
      setPending((existing) => [...existing, confirmation]);
    }
    if (event.type === "usage" && event.usage) setUsage(event.usage);
  }, []);

  const run = useCallback(
    async (
      kind: "chat_send" | "chat_confirm" | "chat_attach",
      payload: Record<string, unknown>,
    ) => {
      controller.current?.abort();
      const current = new AbortController();
      controller.current = current;
      setBusy(true);
      setError(null);
      try {
        await odeSocket.stream(kind, { session_id: session.id, ...payload }, {
          signal: current.signal,
          onEvent: consume,
        });
      } catch (e: unknown) {
        if (e instanceof Cancelled) {
          // The view was detached, not the turn. The exchange is still running
          // server-side; the reattach effect below picks it up again.
        } else {
          setError(describe(e));
          // A spend cap arrives with status 429 and §3.3's message. Shown in the
          // conversation rather than as a toast, because it explains why the answer
          // stopped.
          const status = e instanceof ApiError ? e.status : (e as { status?: number })?.status;
          if (status === 429) {
            setTurns((existing) => [
              ...existing,
              { kind: "notice", level: "error", text: `Limit reached: ${describe(e)}` },
            ]);
          }
        }
      } finally {
        if (controller.current === current) controller.current = null;
        setBusy(false);
        // The stored history is the source of truth once a turn ends; the streamed
        // fragments existed only to show it arriving. This also picks up anything
        // that completed while this client was disconnected.
        try {
          const detail = await api.chatSession(session.id);
          setTurns(replay(detail.messages));
          setPending(detail.pending_confirmations);
          onSessionChange(detail.session);
        } catch {
          // A reload failure leaves the streamed view in place, which is still
          // readable — better than clearing what the developer just watched arrive.
        }
      }
    },
    [consume, onSessionChange, session.id],
  );

  // Reattach to a turn already in flight, whenever the socket comes up.
  //
  // This is what makes the detached exchange visible to the developer: reloading the
  // page or losing the connection during a five-minute profile now resumes the turn
  // instead of showing a conversation that appears to have stalled. chat_attach
  // answers attached=false when nothing is running, which is the common case.
  useEffect(
    () =>
      odeSocket.onState((state: SocketState) => {
        // Only when nothing is already being watched here: an in-flight send is
        // already a view onto the same exchange, and taking it over would be churn.
        if (state !== "open" || controller.current) return;
        void run("chat_attach", {});
      }),
    [run],
  );

  const submit = useCallback(
    (text: string) => {
      setTurns((existing) => [
        ...existing,
        {
          kind: "message",
          message: {
            session_id: session.id,
            seq: -1,
            role: "user",
            content: [{ type: "text", text }],
            created_at: new Date().toISOString(),
          },
        },
      ]);
      setInput("");
      void run("chat_send", { message: text });
    },
    [run, session.id],
  );

  const decide = useCallback(
    (confirmationId: string, approve: boolean) => {
      setPending((existing) => existing.filter((entry) => entry.id !== confirmationId));
      void run("chat_confirm", { confirmation_id: confirmationId, approve });
    },
    [run, session.id],
  );

  const setTier = useCallback(
    async (tier: Tier) => {
      setError(null);
      try {
        onSessionChange(await api.setTier(session.id, tier));
      } catch (e: unknown) {
        setError(describe(e));
      }
    },
    [onSessionChange, session.id],
  );

  return (
    <Pane
      title={session.title || "Assistant"}
      subtitle={`${session.provider} · ${session.model}`}
      actions={
        <TierControl
          tier={session.exposure_tier}
          maxTier={maxTier}
          sessionId={session.id}
          surface={surface}
          onChange={setTier}
        />
      }
    >
      {error && <Muted>{error}</Muted>}

      <div className="conversation">
        {turns.length === 0 && (
          <Muted>
            Describe the problem you are trying to solve. The assistant starts from the ontology,
            not from device names.
          </Muted>
        )}
        {turns.map((turn, index) => (
          <TurnView key={index} turn={turn} onOpenChart={onOpenChart} />
        ))}
        {busy && <div className="thinking">Working…</div>}
        <div ref={bottom} />
      </div>

      {pending.map((confirmation) => (
        <ConfirmationPrompt
          key={confirmation.id}
          confirmation={confirmation}
          onDecide={decide}
        />
      ))}

      <form
        className="composer"
        onSubmit={(e) => {
          e.preventDefault();
          if (input.trim() && !busy) submit(input.trim());
        }}
      >
        <textarea
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              if (input.trim() && !busy) submit(input.trim());
            }
          }}
          placeholder="What are you trying to build?"
          rows={3}
          aria-label="Message"
        />
        <div className="composer-actions">
          {busy ? (
            <button
              type="button"
              title="Abandon this turn. Closing the tab instead leaves it running."
              onClick={() => {
                // Both: stop the work server-side, and detach this view.
                odeSocket.cancelChat(session.id);
                controller.current?.abort();
              }}
            >
              Stop
            </button>
          ) : (
            <button type="submit" disabled={!input.trim()}>
              Send
            </button>
          )}
          {usage && (
            <span className="usage" title="Estimated from configured prices, not an invoice">
              {num(usage.input_tokens + usage.output_tokens)} tokens
              {usage.cost_eur ? ` · ~${usage.cost_eur.toFixed(4)}` : ""}
            </span>
          )}
        </div>
      </form>
    </Pane>
  );
}

/**
 * TierControl is §3.2's persistent surface, and the only way the tier changes:
 * there is no tool for it, so the assistant cannot raise its own exposure.
 */
function TierControl({
  tier,
  maxTier,
  sessionId,
  surface,
  onChange,
}: {
  tier: Tier;
  maxTier: Tier;
  sessionId: string;
  surface: ToolSurface | null;
  onChange: (tier: Tier) => void;
}) {
  const [showAudit, setShowAudit] = useState(false);
  const ceiling = TIERS.indexOf(maxTier);

  return (
    <div className="tier-control">
      <div className="tier-buttons" role="group" aria-label="Data exposure tier">
        {TIERS.map((candidate, index) => (
          <button
            key={candidate}
            className={candidate === tier ? `tier-button active tier-${candidate}` : "tier-button"}
            disabled={index > ceiling}
            title={
              index > ceiling
                ? `An administrator has capped your exposure tier at ${maxTier}.`
                : TIER_EXPOSES[candidate]
            }
            onClick={() => onChange(candidate)}
          >
            {candidate}
          </button>
        ))}
      </div>
      <p className="tier-exposes">{TIER_EXPOSES[tier]}</p>
      {surface && <RaiseHint tier={tier} surface={surface} />}
      <button className="link" onClick={() => setShowAudit(!showAudit)}>
        {showAudit ? "Hide" : "Show"} tier history
      </button>
      {showAudit && <TierAudit sessionId={sessionId} />}
    </div>
  );
}

/** RaiseHint says what the next tier would add, so the choice is informed. */
function RaiseHint({ tier, surface }: { tier: Tier; surface: ToolSurface }) {
  const next = TIERS[TIERS.indexOf(tier) + 1];
  if (!next) return null;

  const now = new Set(surface.tiers.find((entry) => entry.tier === tier)?.available ?? []);
  const then = surface.tiers.find((entry) => entry.tier === next)?.available ?? [];
  const gained = then.filter((name) => !now.has(name));
  if (gained.length === 0) return null;

  return (
    <p className="tier-hint">
      Raising to {next} would add: <strong>{gained.join(", ")}</strong>.
    </p>
  );
}

/** TierAudit is the log §3.2 requires: every change, with its time and its user. */
function TierAudit({ sessionId }: { sessionId: string }) {
  const [changes, setChanges] = useState<{ from: Tier; to: Tier; at: string }[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .tierChanges(sessionId)
      .then((result) => {
        if (!cancelled) setChanges(result.changes);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(describe(e));
      });
    return () => {
      cancelled = true;
    };
  }, [sessionId]);

  if (error) return <Muted>{error}</Muted>;
  if (!changes) return <Muted>Loading…</Muted>;

  return (
    <ol className="tier-audit">
      {changes.map((change, index) => (
        <li key={index}>
          <span className={`tier tier-${change.from}`}>{change.from}</span> →{" "}
          <span className={`tier tier-${change.to}`}>{change.to}</span>
          <span className="muted"> {dateTime(change.at)}</span>
        </li>
      ))}
    </ol>
  );
}

/** ConfirmationPrompt is D11: the developer decides, with the arguments in view. */
function ConfirmationPrompt({
  confirmation,
  onDecide,
}: {
  confirmation: PendingConfirmation;
  onDecide: (id: string, approve: boolean) => void;
}) {
  return (
    <div className="confirmation">
      <div className="confirmation-head">
        <strong>{confirmation.tool}</strong> needs your confirmation
      </div>
      {/* The arguments travel with the prompt: approving a tool name alone would be
          agreeing to something you cannot see. */}
      <pre className="json">{JSON.stringify(confirmation.input, null, 2)}</pre>
      <div className="confirmation-actions">
        <button onClick={() => onDecide(confirmation.id, true)}>Approve</button>
        <button onClick={() => onDecide(confirmation.id, false)}>Decline</button>
      </div>
    </div>
  );
}

function TurnView({
  turn,
  onOpenChart,
}: {
  turn: Turn;
  onOpenChart?: (chartId: string) => void;
}) {
  if (turn.kind === "notice") {
    return <div className={`notice notice-${turn.level}`}>{turn.text}</div>;
  }
  if (turn.kind === "streaming") {
    return (
      <div className="turn assistant">
        <div className="turn-body">{turn.text}</div>
      </div>
    );
  }
  if (turn.kind === "tool") {
    return (
      <ToolTurn
        call={turn.call}
        result={turn.result}
        progress={turn.progress}
        onOpenChart={onOpenChart}
      />
    );
  }

  const { message } = turn;
  const text = message.content
    .filter((content) => content.type === "text")
    .map((content) => content.text ?? "")
    .join("");

  if (!text) return null;
  return (
    <div className={`turn ${message.role}`}>
      <div className="turn-body">{text}</div>
    </div>
  );
}

/**
 * ToolTurn shows what the assistant did, and — when it was refused — what it would
 * have needed. A blocked call rendered as nothing would leave the developer
 * wondering why the answer was thin.
 */
function ToolTurn({
  call,
  result,
  progress,
  onOpenChart,
}: {
  call: { id: string; name: string; input: unknown };
  result?: ToolResult;
  progress?: string;
  onOpenChart?: (chartId: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const refusal = tierRefusal(result);
  const chartID = chartFromResult(call.name, result);

  return (
    <div className={`tool-turn ${result?.is_error ? "tool-error" : ""}`}>
      <button className="tool-head" onClick={() => setOpen(!open)}>
        <span className="twisty">{open ? "▾" : "▸"}</span>
        <span className="tool-name">{call.name}</span>
        {result && (
          <span className={`tool-outcome outcome-${result.outcome}`}>{result.outcome}</span>
        )}
        {!result && progress && <span className="tool-progress">{progress}</span>}
        {refusal && (
          <span className="tool-refusal">
            needs {refusal.required}, session is at {refusal.blocked_by_tier}
          </span>
        )}
      </button>
      {/*
        A chart specification is the one tool result that is worth nothing as JSON:
        §5.9 has the assistant emit a document and the pane draw it, and the values
        are read there under the developer's own token — the model never sees them.
        So the useful thing to offer here is the way in.
      */}
      {chartID && onOpenChart && (
        <div className="tool-chart">
          <button onClick={() => onOpenChart(chartID)}>Open in exploration</button>
          <span className="muted-inline">
            the assistant proposed a chart; the values behind it are read with your token
          </span>
        </div>
      )}
      {open && (
        <div className="tool-body">
          <div className="tool-part">
            <span className="tool-label">Arguments</span>
            <pre className="json">{JSON.stringify(call.input ?? {}, null, 2)}</pre>
          </div>
          {result && (
            <div className="tool-part">
              <span className="tool-label">Result</span>
              <pre className="json">{JSON.stringify(result.content, null, 2)}</pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/** chartFromResult picks the chart id out of a render_chart result, if there is one. */
function chartFromResult(tool: string, result?: ToolResult): string | null {
  if (tool !== "render_chart" || !result || result.is_error) return null;
  const content = result.content as { chart_id?: string } | null;
  return content?.chart_id ?? null;
}

/** tierRefusal recognises §3.2's structured refusal so it can be shown as one. */
function tierRefusal(result?: ToolResult): { blocked_by_tier: Tier; required: Tier } | null {
  if (!result || result.outcome !== "blocked_by_tier") return null;
  const content = result.content as { blocked_by_tier?: Tier; required?: Tier } | null;
  if (!content?.blocked_by_tier || !content.required) return null;
  return { blocked_by_tier: content.blocked_by_tier, required: content.required };
}

/** replay turns stored history into renderable turns, pairing calls with results. */
function replay(messages: ChatMessage[]): Turn[] {
  const results = new Map<string, ToolResult>();
  for (const message of messages) {
    for (const content of message.content) {
      if (content.type !== "tool_result" || !content.tool_use_id) continue;
      let parsed: unknown = content.tool_result;
      try {
        parsed = JSON.parse(content.tool_result ?? "null");
      } catch {
        // Kept as the raw string: an unparseable result is still worth showing.
      }
      results.set(content.tool_use_id, {
        call_id: content.tool_use_id,
        tool: content.tool_name ?? "",
        outcome: outcomeOf(parsed, content.is_error === true),
        content: parsed,
        is_error: content.is_error === true,
        duration_ms: 0,
      });
    }
  }

  const turns: Turn[] = [];
  for (const message of messages) {
    const text = message.content
      .filter((content) => content.type === "text")
      .map((content) => content.text ?? "")
      .join("");
    if (text) {
      turns.push({ kind: "message", message: { ...message, content: [{ type: "text", text }] } });
    }
    for (const content of message.content) {
      if (content.type !== "tool_use") continue;
      turns.push({
        kind: "tool",
        call: {
          id: content.tool_use_id ?? "",
          name: content.tool_name ?? "",
          input: content.tool_input,
        },
        result: results.get(content.tool_use_id ?? ""),
      });
    }
  }
  return turns;
}

/**
 * outcomeOf recovers the outcome from a stored result.
 *
 * The stored history keeps the content the model saw rather than ODE's own outcome
 * value, so a replayed refusal is recognised by its shape. That is one reason the
 * shape of §3.2's refusal is fixed rather than free-form.
 */
export function outcomeOf(content: unknown, isError: boolean): string {
  if (content && typeof content === "object") {
    const record = content as Record<string, unknown>;
    if (record.blocked_by_tier) return "blocked_by_tier";
    if (record.requires_confirmation) return "awaiting_confirmation";
    if (record.error === "not_implemented") return "not_implemented";
    if (record.error === "unknown_tool") return "unknown_tool";
  }
  return isError ? "failed" : "ok";
}

/** apply folds one stream event into the visible turns. */
export function apply(turns: Turn[], event: ChatEvent): Turn[] {
  switch (event.type) {
    case "text_delta": {
      const last = turns[turns.length - 1];
      if (last?.kind === "streaming") {
        return [
          ...turns.slice(0, -1),
          { kind: "streaming", text: last.text + (event.text ?? "") },
        ];
      }
      return [...turns, { kind: "streaming", text: event.text ?? "" }];
    }
    case "tool_call":
      return event.tool_call ? [...turns, { kind: "tool", call: event.tool_call }] : turns;
    case "progress": {
      // Attached to the running tool and replaced rather than appended: a profile
      // reports a handful of phases, and stacking them would push the conversation
      // off screen while saying nothing new.
      if (!event.progress) return turns;
      const line = event.progress.detail || event.progress.stage;
      for (let i = turns.length - 1; i >= 0; i -= 1) {
        const turn = turns[i];
        if (turn.kind === "tool" && !turn.result) {
          const copy = [...turns];
          copy[i] = { ...turn, progress: line };
          return copy;
        }
      }
      return turns;
    }
    case "tool_result": {
      if (!event.tool_result) return turns;
      const result = event.tool_result;
      // Attached to its call rather than appended, so arguments and result read as
      // one thing.
      let index = -1;
      for (let i = turns.length - 1; i >= 0; i -= 1) {
        const turn = turns[i];
        if (turn.kind === "tool" && turn.call.id === result.call_id && !turn.result) {
          index = i;
          break;
        }
      }
      if (index === -1) {
        return [
          ...turns,
          { kind: "tool", call: { id: result.call_id, name: result.tool, input: {} }, result },
        ];
      }
      const copy = [...turns];
      copy[index] = { ...(copy[index] as Extract<Turn, { kind: "tool" }>), result };
      return copy;
    }
    case "warning": {
      const warnings = event.warnings ?? [];
      if (warnings.length === 0) return turns;
      return [
        ...turns,
        {
          kind: "notice",
          level: "warn",
          text: warnings
            .map(
              (warning) =>
                `Approaching the ${warning.scope} ${warning.kind} limit: ${num(warning.spent)} of ${num(warning.cap)}.`,
            )
            .join(" "),
        },
      ];
    }
    case "limit_exceeded":
      return [
        ...turns,
        { kind: "notice", level: "error", text: event.error ?? "An LLM limit was reached." },
      ];
    case "error":
      return [
        ...turns,
        { kind: "notice", level: "error", text: event.error ?? "Something failed." },
      ];
    case "done":
      if (event.stop_reason === "max_iterations" && event.error) {
        return [...turns, { kind: "notice", level: "warn", text: event.error }];
      }
      return turns;
    default:
      return turns;
  }
}

export { replay };
export type { Turn };
