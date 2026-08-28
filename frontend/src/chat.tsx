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

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
  type SessionActivity,
  type Tier,
  type ToolResult,
  type ToolSurface,
  type Usage,
  type Workbench,
  workbenchLabel,
} from "./api";
import { AlertTriangleIcon, ChevronRightIcon, CircleAlertIcon, InfoIcon, PencilIcon, XIcon } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Bubble, BubbleContent } from "@/components/ui/bubble";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Marker, MarkerContent, MarkerIcon } from "@/components/ui/marker";
import { Message, MessageContent, MessageHeader } from "@/components/ui/message";
import {
  MessageScroller,
  MessageScrollerButton,
  MessageScrollerContent,
  MessageScrollerItem,
  MessageScrollerProvider,
  MessageScrollerViewport,
  useMessageScroller,
} from "@/components/ui/message-scroller";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import { announce } from "./attention";
import { Markdown } from "./markdown";
import { Cancelled, odeSocket, type SocketState } from "./ws";
import { setParam, useParam } from "./router";
import { Busy, Muted, Pane, Section, dateTime, describe, num, shortId } from "./ui";
import { useConversationPairing, useWorkbenches } from "./workbench";

/**
 * The sentinel the workbench select uses for "open a new one".
 *
 * A value that cannot be an id, because the option sits in the same list as the
 * workbenches themselves: choosing where a conversation happens and choosing to
 * make a new somewhere are the same decision for the developer, even though they
 * are two calls underneath.
 */
const NEW_WORKBENCH = "\u0000new";
// CLOSED_WORKBENCH is the option standing for a workbench this conversation names
// and the developer has since closed. Out of the id space for the reason above.
const CLOSED_WORKBENCH = "\u0000closed";

/**
 * The longest title the backend keeps, so the box stops where the refusal would.
 *
 * `maxTitleRunes` in pkg/chat/engine.go. Kept in step by hand — the alternative is
 * a round trip to learn a bound that has not moved since it was written — and the
 * only cost of the two drifting is a 400 the developer would see instead of the
 * input simply refusing the next character.
 */
const MAX_TITLE = 200;

/**
 * The chat pane of §2, and the surface where §3.2's exposure tier becomes
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

/**
 * What the sessions panel marks a conversation with.
 *
 * Three, because a developer holding several conversations has three different
 * questions about the ones they are not looking at: is it still thinking, is it
 * done, and is it stuck on me. The engine reports the first and the third
 * (`chat_watch`, see pkg/chat/activity.go); the second is this tab's own
 * knowledge, since only the tab knows the developer has not been back since.
 */
type SessionMark = "working" | "ready" | "decide";

const MARK_LABEL: Record<SessionMark, string> = {
  working: "Working",
  ready: "Reply ready",
  decide: "Needs you",
};

/**
 * The mark against a conversation the developer is not looking at.
 *
 * A dot and a word, not a word alone: the list is scanned rather than read, and
 * the dot is what carries at a glance. `working` is the only animated one — it is
 * the only mark that means "this is still changing", and a dot that pulses while
 * the other two sit still is the cheapest way to say so.
 *
 * These are not `Badge`s. A badge is a filled pill and three of them down a narrow
 * list would out-shout the conversation titles they annotate, which are the thing
 * the developer is actually looking for.
 */
function SessionMarkBadge({ mark }: { mark: SessionMark }) {
  return (
    <span
      className={cn(
        "session-mark ml-auto inline-flex shrink-0 items-center gap-1.5 text-xs",
        mark === "ready" && "text-foreground",
        mark === "decide" && "text-destructive",
        mark !== "ready" && mark !== "decide" && "text-muted-foreground",
      )}
    >
      {/*
        The same spinner the conversation puts beside "Working…", so a row in the
        list and the pane it opens say "still running" the same way. It was a
        hollow ring that pulsed, which had to be learned; this is the shape the
        developer has already seen doing exactly this job one pane over.

        `ready` and `decide` keep the dot: they are states the conversation is
        resting in, not activity, and a spinner would say the opposite.
      */}
      {mark === "working" ? (
        <Spinner className="size-3" aria-label={undefined} role={undefined} />
      ) : (
        <span aria-hidden className="size-2 rounded-full bg-current" />
      )}
      {MARK_LABEL[mark]}
    </span>
  );
}

/**
 * The exposure tier, as a badge.
 *
 * A ramp in weight rather than in hue — outline, muted, solid — because the thing
 * worth reading off is the *ordering*: L2 exposes more than L1 exposes more than
 * L0. Three unrelated colours would say "three categories", which is the wrong
 * shape for a scale, and would say nothing at all on a monochrome screen. The
 * label is the tier's own name, so nothing rests on the styling alone.
 */
const TIER_VARIANT = {
  L0: "outline",
  L1: "secondary",
  L2: "default",
} as const satisfies Record<Tier, "outline" | "secondary" | "default">;

function TierBadge({ tier, className }: { tier: Tier; className?: string }) {
  return (
    <Badge variant={TIER_VARIANT[tier]} className={cn(`tier tier-${tier} font-normal`, className)}>
      {tier}
    </Badge>
  );
}

/**
 * What this tab holds about the conversations in the list.
 *
 * Two maps rather than one, because they are two different kinds of fact and
 * opening a conversation does different things to them. `live` is the engine's:
 * what a conversation is doing right now, as true of one nobody is looking at as
 * of the one on screen. `unread` is this tab's: the developer has not been back
 * since it finished.
 *
 * Reading a conversation settles the second and changes nothing about the first.
 * Held as one mark it could not: the only way to drop the unread mark was to drop
 * the row's entry altogether, which threw away the fact that its turn was still
 * running — so a session opened while it worked came back from the switch with
 * nothing against it, and stayed that way until the socket next reconnected.
 */
type Watched = {
  live: Record<string, "running" | "waiting">;
  unread: Record<string, true>;
};

/**
 * useSessionMarks follows every conversation, not just the open one.
 *
 * This is the gap the mark closes. A turn is detached, so switching sessions
 * leaves it running with nothing in the tab watching it — the conversation's own
 * stream went with the component. attention.ts does not cover it either: it fires
 * from inside the open conversation, and only when the whole window is in the
 * background. So a developer working in a second session had no way of learning
 * that the first had answered.
 *
 * The unread half is this tab's, not the backend's, and that is deliberate: "you
 * have not looked at it since it finished" is a fact about this screen. It follows
 * that it does not survive a reload — a conversation that finished before the page
 * loaded is just a conversation with a reply in it, which is what the list already
 * shows. What does survive is anything still running, because the watch opens with
 * a snapshot of it.
 */
function useSessionMarks(openId: string | null): {
  marks: Record<string, SessionMark>;
  /** Drops everything held about a session, for when it is deleted. */
  forget: (sessionId: string) => void;
} {
  const [watched, setWatched] = useState<Watched>({ live: {}, unread: {} });

  // Read inside the subscription, which is registered once. Without the ref it
  // would close over the session that was open when the socket came up.
  const open = useRef(openId);
  open.current = openId;

  const apply = useCallback((activity: SessionActivity) => {
    const id = activity.session_id;
    setWatched((state) => {
      if (activity.state !== "idle") {
        // Running and waiting are both the engine's word on the conversation, and
        // both hold until it says otherwise. Waiting outlives the developer opening
        // the session on purpose: reading a confirmation is not answering it.
        if (state.live[id] === activity.state) return state;
        return { ...state, live: { ...state.live, [id]: activity.state } };
      }
      if (state.live[id] === undefined) {
        // Idle is the ordinary state of a conversation nobody asked anything, and
        // marking those would mark the whole list. Only one this tab watched doing
        // something has finished.
        return state;
      }
      const { [id]: _ended, ...live } = state.live;
      const unread =
        id === open.current || state.unread[id] !== undefined
          ? state.unread
          : { ...state.unread, [id]: true as const };
      return { live, unread };
    });
  }, []);

  const forget = useCallback((sessionId: string) => {
    setWatched((state) => {
      if (state.live[sessionId] === undefined && state.unread[sessionId] === undefined) {
        return state;
      }
      const { [sessionId]: _live, ...live } = state.live;
      const { [sessionId]: _seen, ...unread } = state.unread;
      return { live, unread };
    });
  }, []);

  // Opening a conversation is reading it. Only the unread half: whether a turn is
  // still in flight is not something looking at it changes, and the row has to say
  // so again the moment the developer moves on. Here rather than only in the click
  // handler, because ?session= is also set from links elsewhere in the shell.
  useEffect(() => {
    if (openId === null) return;
    setWatched((state) => {
      if (state.unread[openId] === undefined) return state;
      const { [openId]: _read, ...unread } = state.unread;
      return { ...state, unread };
    });
  }, [openId]);

  // Subscribed once. The same reasoning as the reattach in Conversation: a
  // subscription that depended on anything per-render would re-register on every
  // state change, and onState replays "open" to each new listener — so it would
  // re-subscribe in a loop.
  const watching = useRef(false);
  useEffect(() => {
    const controller = new AbortController();
    // Asked for rather than waited on. The subscription below wants a connection it
    // never sends anything over, and nothing else in this pane sends anything until
    // the developer does — so before this, a tab that only ever looked at chat sat
    // on an idle socket and learned nothing about any conversation. See
    // ensureConnected.
    odeSocket.ensureConnected();
    const stop = odeSocket.onState((state: SocketState) => {
      if (state !== "open" || watching.current) return;
      watching.current = true;
      // Whatever this tab believed was in flight is no longer known to be: the
      // connection that was reporting it has been away. Carried to unread rather
      // than dropped, because a turn that ended during the gap is exactly what the
      // developer needs telling about — and one that did not is put back by the
      // snapshot this subscription opens with. Not the one on screen: they are
      // looking at it, so there is nothing they have missed.
      setWatched((state) => {
        const ids = Object.keys(state.live);
        if (ids.length === 0) return state;
        const unread = { ...state.unread };
        for (const id of ids) if (id !== open.current) unread[id] = true;
        return { live: {}, unread };
      });
      void odeSocket
        .watchSessions({ onActivity: apply, signal: controller.signal })
        .catch(() => {
          // A dropped connection, or a watcher the backend gave up on. Both end
          // the same way: onState reports the next open and this re-subscribes.
        })
        .finally(() => {
          watching.current = false;
        });
    });
    return () => {
      stop();
      controller.abort();
    };
  }, [apply]);

  const marks = useMemo(() => {
    const shown: Record<string, SessionMark> = {};
    for (const [id, state] of Object.entries(watched.live)) {
      shown[id] = state === "running" ? "working" : "decide";
    }
    for (const id of Object.keys(watched.unread)) {
      if (shown[id] === undefined) shown[id] = "ready";
    }
    // Only "reply ready" is suppressed on the row being read, and only that one.
    //
    // It means "you have not been back since it finished", which is false of the
    // conversation on screen — the developer is back, that is what open means. The
    // other two are not about the reader at all: "working" and "needs you" are
    // facts about the conversation, and they stay true while it is being watched.
    // Dropping them cost the list its one continuous answer to "which of these is
    // busy", because the row went blank exactly when the developer selected it.
    //
    // Suppressed at the last moment rather than dropped from the state, so that
    // switching away puts back whatever is still true of it.
    if (openId !== null && shown[openId] === "ready") delete shown[openId];
    return shown;
  }, [watched, openId]);

  return { marks, forget };
}

/**
 * Which operator a conversation is about, for the line under its title.
 *
 * The workbench is what a developer chose when they started the session, and with
 * two of them open it is the one thing that tells two conversations apart before
 * either is opened. A count of messages is ODE's own bookkeeping — it grows by two
 * every turn and answers no question anybody has in front of this list.
 *
 * Null in every case where naming one would be a guess. A session that names no
 * workbench is one from before workbenches existed, and the backend reads it as
 * "my only one" — so with a single workbench open that is what it says, and with
 * several there is nothing honest to name. An empty list is either a deployment
 * with no repository surface or a list still being read; neither says anything
 * about this session.
 */
function workbenchName(session: ChatSession, benches: Workbench[]): string | null {
  if (benches.length === 0) return null;
  const named = benches.find((bench) => bench.id === session.workbench_id);
  if (named) return workbenchLabel(named);
  // Named one that is not in the list: the workbench has since been closed. Worth
  // saying, because it is also why opening this conversation does not move the code
  // pane — the checkout it was about is not on screen.
  if (session.workbench_id) return "closed workbench";
  return benches.length === 1 ? workbenchLabel(benches[0]) : null;
}

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
  // What the conversations the developer is *not* reading are doing.
  const { marks, forget: forgetSession } = useSessionMarks(openId);
  // A named session that is not in the list is a stale link or another account's,
  // and saying so beats silently opening nothing.
  const missingSession = sessions !== null && openId !== null && current === null;

  // The code beside this conversation is the code this conversation is about. The
  // pane on the right shows one workbench at a time and the assistant writes into
  // one, so whichever way `?session=` moved — the list below, a link from an
  // experiment, a reload, the back button — the workbench follows it here.
  useConversationPairing(sessions, openId);

  // The list, for the workbench each session names below. The same state the
  // conversation pairing acts on, read here only to put a name to an id.
  const { all: benches } = useWorkbenches();

  // Half-typed messages, one per session, kept here rather than in the composer.
  //
  // Conversation is keyed by session id, so switching conversations remounts it and
  // takes its state with it — which is what the turns and the held confirmations
  // want, since they belong to the conversation being replaced. A draft does not: it
  // belongs to the developer, who switched away to look something up and expects to
  // find the sentence they were writing still there. Above the key, it survives.
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  // Stable, for the same reason updateSession is: the composer's onChange lands in
  // the dependencies of everything the conversation memoises.
  const setDraft = useCallback((id: string, text: string) => {
    setDrafts((existing) => ({ ...existing, [id]: text }));
  }, []);

  // The row being renamed, and the name as it is being typed. One at a time, and
  // held here rather than in each row: a rename is a thing the panel is doing, and
  // a row that unmounts — a delete, a reordering list — takes its own state with it
  // while this survives.
  const [renaming, setRenaming] = useState<{ id: string; title: string } | null>(null);

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

  const create = useCallback(async (provider: string, workbench: string) => {
    setError(null);
    try {
      // Always L0. §3.2 makes it the default, and starting anywhere else would be
      // ODE choosing the developer's data exposure for them.
      const created = await api.createChatSession({
        provider,
        exposure_tier: "L0",
        // Which operator this conversation is about. A second session on the same
        // workbench is two conversations about one operator; a different one is a
        // developer working on two at once.
        workbench_id: workbench || undefined,
      });
      setSessions((existing) => [created, ...(existing ?? [])]);
      // Opening it is all that is needed: the pairing below then puts the code pane
      // on the operator this conversation was started about.
      setParam("session", created.id);
    } catch (e: unknown) {
      setError(describe(e));
    }
  }, []);

  // Stable, so that an effect in the conversation depending on it does not re-run on
  // every list update. Identity churn here is what fed the reattach loop described in
  // Conversation, since every turn ends by calling this.
  const updateSession = useCallback((updated: ChatSession) => {
    setSessions((existing) =>
      (existing ?? []).map((entry) => (entry.id === updated.id ? updated : entry)),
    );
  }, []);

  /*
   * Renaming, and the one awkward part of it: which of Enter, Escape and losing
   * focus has already settled the edit.
   *
   * All three end it, and they overlap. Escape and Enter both take the input off
   * the screen, and a focused element that is removed does not reliably deliver a
   * focusout — so the blur handler may run afterwards or not at all, depending on
   * the browser. Without a flag, the orders that do deliver it send a second
   * request, and the one after Escape sends the rename the developer just
   * abandoned. `settled` is what makes every order mean the same thing.
   */
  const settled = useRef(false);

  const startRename = useCallback((entry: ChatSession) => {
    settled.current = false;
    // The stored title rather than what the row displays: an untitled session shows
    // its short id, and starting the edit with that in the box would have the
    // developer delete an id before they can type a name.
    setRenaming({ id: entry.id, title: entry.title });
  }, []);

  const abandonRename = useCallback(() => {
    settled.current = true;
    setRenaming(null);
  }, []);

  const commitRename = useCallback(
    async (entry: ChatSession, title: string) => {
      if (settled.current) return;
      settled.current = true;
      setRenaming(null);
      // Nothing to send: the developer opened the box, thought better of it, and
      // pressed Enter. Trimmed on both sides of the wire, because the backend
      // trims what it stores and this is the comparison that has to match it.
      if (title.trim() === entry.title) return;
      setError(null);
      try {
        // Answered with the stored session, so the row shows what was kept rather
        // than what was typed — the trim included.
        updateSession(await api.renameChatSession(entry.id, title));
      } catch (e: unknown) {
        setError(describe(e));
      }
    },
    [updateSession],
  );

  const remove = useCallback(
    async (id: string) => {
      try {
        await api.deleteChatSession(id);
        setSessions((existing) => (existing ?? []).filter((entry) => entry.id !== id));
        // A mark on a session that no longer exists would be a mark nothing can
        // clear, since clearing one is opening the conversation it is on.
        forgetSession(id);
        // Nothing will ask for this one again, and keeping it would hold the text of
        // a conversation the developer has just thrown away.
        setDrafts((existing) => {
          const { [id]: _gone, ...rest } = existing;
          return rest;
        });
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
    [forgetSession, openId],
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
        {sessions === null && <Busy>Loading…</Busy>}
        {sessions?.length === 0 && <Muted>No sessions yet.</Muted>}
        {missingSession && (
          <Muted>The conversation named in the address is not in this account&apos;s list.</Muted>
        )}
        {sessions && sessions.length > 0 && (
          <ul className="list session-list -mx-1 flex flex-col gap-1">
            {sessions.map((entry) => {
              const about = workbenchName(entry, benches);
              // The workbench is named by its repository, so this line is cut with an
              // ellipsis more often than not — `franzmueller/operator-t…`. The full
              // text goes in the title, the way the file tree and the repository bar
              // carry theirs: unconditional, because whether a given row is actually
              // cut depends on the width of a pane the developer can drag, and a
              // measurement taken at render would be wrong by the next drag without
              // anything re-rendering to correct it.
              const line = about ? `${entry.provider} · ${about}` : entry.provider;
              const named = entry.title || shortId(entry.id);
              return (
                <li
                  key={entry.id}
                  className={cn(
                    // The row owns the highlight. It used to be on the button
                    // inside, inset by the list's own padding, so hovering drew a
                    // second smaller rectangle within the selected one and the
                    // sliver between the two was visible down both sides.
                    "group/session flex items-center gap-0.5 rounded-md pr-1 hover:bg-accent",
                    current?.id === entry.id && "active bg-accent",
                  )}
                >
                  {renaming?.id === entry.id ? (
                    <Input
                      className="session-rename my-1 h-8"
                      // The label says what is being renamed, because the box replaces
                      // the row it belongs to and a screen reader has nothing else left
                      // to say which conversation this is.
                      aria-label={`Rename ${named}`}
                      value={renaming.title}
                      maxLength={MAX_TITLE}
                      autoFocus
                      placeholder="A name for this conversation"
                      onChange={(event) =>
                        setRenaming({ id: entry.id, title: event.target.value })
                      }
                      onKeyDown={(event) => {
                        if (event.key === "Enter") void commitRename(entry, renaming.title);
                        if (event.key === "Escape") abandonRename();
                      }}
                      onBlur={() => void commitRename(entry, renaming.title)}
                    />
                  ) : (
                    <>
                      <button
                        // `items-stretch` is load-bearing: a <button> carries `align-items: center` in
                        // the UA stylesheet, so as a flex column it shrink-wraps its rows to their
                        // content. The title row was then only as wide as the title, which left the
                        // `ml-auto` on the mark nothing to push against and parked it beside the name
                        // instead of at the right edge.
                        className="session-open flex min-w-0 flex-1 flex-col items-stretch gap-1 rounded-md px-2 py-1.5 text-left focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                        onClick={() => setParam("session", entry.id)}
                      >
                        <span className="session-title flex min-w-0 items-center gap-2">
                          <span className="session-name truncate text-sm" title={named}>
                            {named}
                          </span>
                          {marks[entry.id] && (
                            <SessionMarkBadge mark={marks[entry.id]} />
                          )}
                        </span>
                        <span className="session-meta flex min-w-0 items-center gap-1.5">
                          <TierBadge tier={entry.exposure_tier} />
                          <span className="session-about truncate text-xs text-muted-foreground" title={line}>
                            {line}
                          </span>
                        </span>
                      </button>
                      {/*
                        Both row actions stay in the tab order and are only *visually*
                        revealed on hover. `opacity-0` rather than `hidden`, because a
                        keyboard user tabbing through the list has no hover to trigger
                        and would otherwise never reach them; `focus-visible:opacity-100`
                        brings them back into view when they do.
                      */}
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        className="session-rename-open shrink-0 opacity-0 hover:bg-background group-hover/session:opacity-100 focus-visible:opacity-100"
                        title="Rename this session"
                        aria-label={`Rename ${named}`}
                        onClick={() => startRename(entry)}
                      >
                        <PencilIcon />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        className="session-delete shrink-0 opacity-0 hover:bg-background hover:text-destructive group-hover/session:opacity-100 focus-visible:opacity-100"
                        title="Delete this session"
                        aria-label={`Delete ${named}`}
                        onClick={() => void remove(entry.id)}
                      >
                        <XIcon />
                      </Button>
                    </>
                  )}
                </li>
              );
            })}
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
          draft={drafts[current.id] ?? ""}
          onDraftChange={setDraft}
          onOpenChart={onOpenChart}
          onSessionChange={updateSession}
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

/**
 * Starting a conversation: which provider, and which operator it is about.
 *
 * The workbench choice is the question the developer actually has when they open a
 * second session — is this more of the same operator, or the start of another one?
 * So it is a select over what is open plus one entry that opens a new one, rather
 * than a setting somewhere else. With a single workbench and nothing to choose
 * between, the control is not rendered at all.
 */
function NewSession({
  providers,
  maxTier,
  onCreate,
}: {
  providers: ProviderInfo[];
  maxTier: Tier;
  onCreate: (provider: string, workbench: string) => void;
}) {
  const [provider, setProvider] = useState("");
  const chosen = provider || providers.find((entry) => entry.default)?.name || providers[0]?.name || "";

  const { all, current, max, create: openWorkbench } = useWorkbenches();
  // The one on screen, so a developer who has been working in an operator gets a
  // conversation about that operator without having to say so.
  const [workbench, setWorkbench] = useState("");
  const target = workbench || current?.id || all[0]?.id || "";
  const full = max > 0 && all.length >= max;

  return (
    <form
      className="new-session mb-3 flex flex-wrap items-center gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        void (async () => {
          // NEW opens the workbench first, then starts the conversation in it: two
          // calls, because they are two things, and the second one needs the id the
          // first mints.
          if (target === NEW_WORKBENCH) {
            const opened = await openWorkbench();
            if (!opened) return;
            onCreate(chosen, opened.id);
            return;
          }
          onCreate(chosen, target);
        })();
      }}
    >
      <Select value={chosen} onValueChange={(value) => setProvider(value ?? chosen)}>
        <SelectTrigger size="sm" aria-label="Provider" className="w-auto min-w-32">
          {/*
            The trigger formats the value itself rather than echoing the chosen
            item's markup. The options live in a popup that is not mounted until the
            listbox is opened, so a trigger that waits for one to hand it a label
            shows the raw value until the developer has opened the control once.
          */}
          <SelectValue>
            {(value: string | null) => {
              const entry = providers.find((candidate) => candidate.name === value);
              if (!entry) return value;
              return `${entry.name}${entry.capabilities.degraded ? " (degraded)" : ""}`;
            }}
          </SelectValue>
        </SelectTrigger>
        <SelectContent>
          {providers.map((entry) => (
            <SelectItem key={entry.name} value={entry.name}>
              {entry.name}
              {entry.capabilities.degraded ? " (degraded)" : ""}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {all.length > 0 && (
        <Select value={target} onValueChange={(value) => setWorkbench(value ?? target)}>
          <SelectTrigger size="sm" aria-label="Workbench" className="w-auto min-w-40">
            <SelectValue>
              {(value: string | null) => {
                if (value === NEW_WORKBENCH) return "New workbench…";
                const bench = all.find((candidate) => candidate.id === value);
                return bench ? workbenchLabel(bench) : value;
              }}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {all.map((bench) => (
              <SelectItem key={bench.id} value={bench.id}>
                {workbenchLabel(bench)}
              </SelectItem>
            ))}
            {!full && <SelectItem value={NEW_WORKBENCH}>New workbench…</SelectItem>}
          </SelectContent>
        </Select>
      )}
      <Button type="submit" size="sm" disabled={providers.length === 0}>
        New session
      </Button>
      {maxTier !== "L2" && (
        <span className="hint w-full text-xs text-muted-foreground">
          An administrator has capped your exposure tier at {maxTier}.
        </span>
      )}
      {providers.some((entry) => entry.capabilities.degraded) && (
        <ul className="degraded-list w-full list-disc pl-4 text-xs text-muted-foreground">
          {providers
            .filter((entry) => entry.capabilities.degraded)
            .map((entry) => (
              <li key={entry.name}>
                <strong className="font-medium text-foreground">{entry.name}</strong>:{" "}
                {entry.capabilities.degraded_reason}
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
      <Table className="grid tools">
        <TableHeader>
          <TableRow>
            <TableHead>Tool</TableHead>
            <TableHead>Tier</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {surface.tools.map((tool) => (
            <TableRow
              key={tool.name}
              // A tool the session cannot reach is dimmed rather than dropped: the
              // point of publishing the whole table is that the developer can see
              // what raising the tier would buy them.
              className={available.has(tool.name) ? "" : "unavailable text-muted-foreground"}
            >
              {/*
                There was a status column here, and two of its three values were
                already on the row. "available" repeated the row not being dimmed,
                and "needs L1" repeated the tier badge beside it — a column that is
                mostly restatement makes the table wider and the exceptions harder
                to pick out.

                The third value was not a restatement: a tool this build does not
                serve is declared but uncallable, which the tier says nothing about.
                So it stays, as a note under the name where it is the exception
                rather than a column of mostly-noise.
              */}
              <TableCell title={tool.description} className="font-mono">
                {tool.name}
                {!tool.implemented && (
                  <span
                    className="muted block font-sans text-xs text-muted-foreground"
                    title={tool.unavailable}
                  >
                    not in this build
                  </span>
                )}
                {tool.confirm && (
                  <Badge
                    variant="outline"
                    className="badge ml-1.5 font-normal inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs"
                    title="Needs your explicit confirmation"
                  >
                    confirm
                  </Badge>
                )}
              </TableCell>
              <TableCell>
                <TierBadge tier={tool.min_tier} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

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
  draft,
  onDraftChange,
  onSessionChange,
  onOpenChart,
}: {
  session: ChatSession;
  maxTier: Tier;
  surface: ToolSurface | null;
  /** What is in the composer, held above this component's key. */
  draft: string;
  onDraftChange: (sessionId: string, text: string) => void;
  onSessionChange: (session: ChatSession) => void;
  onOpenChart?: (chartId: string) => void;
}) {
  const [turns, setTurns] = useState<Turn[]>([]);
  const [pending, setPending] = useState<PendingConfirmation[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [usage, setUsage] = useState<Usage | null>(null);
  const controller = useRef<AbortController | null>(null);

  // Read here rather than handed down from the session list: this is the list the
  // control in the header offers, and the provider is the one source of it.
  const { all: benches } = useWorkbenches();

  const input = draft;
  const setInput = useCallback(
    (text: string) => onDraftChange(session.id, text),
    [onDraftChange, session.id],
  );

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
      // How the turn ended, for the alert in the finally. Null means there was
      // nothing to wait for — see the two cases below.
      let ending: "answered" | "failed" | null = null;
      // Whether the stream ran to its end, as opposed to being cut off.
      //
      // This is what decides whether the stored history may replace what is on
      // screen. A stream that threw says nothing about the turn: the socket drops,
      // the exchange keeps running server-side, and the store still holds only the
      // messages that were complete before it started.
      let completed = false;
      try {
        const outcome = await odeSocket.stream(kind, { session_id: session.id, ...payload }, {
          signal: current.signal,
          onEvent: consume,
        });
        // An attach that found nothing running is the common case on every socket
        // open. No turn ended, so there is nothing to announce.
        completed = true;
        ending = kind === "chat_attach" && !outcome.attached ? null : "answered";
      } catch (e: unknown) {
        if (e instanceof Cancelled) {
          // The view was detached, not the turn. The exchange is still running
          // server-side; the reattach effect below picks it up again. Also what
          // Stop produces, and nobody wants an alert about a turn they abandoned.
        } else {
          ending = "failed";
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
        // Whether this run is still the view, or has been replaced by a later one.
        //
        // A run that has been superseded must land silently: another is watching the
        // same exchange now, and it owns the busy flag, the reload and the alert.
        // Saying otherwise is how a fresh attach lost its "Working…" a moment after
        // making it — the aborted run it replaced reached this line second and
        // turned the flag back off, leaving a live turn looking like a dead one. A
        // null ref is nobody having taken over, which is the unmount case: the
        // updates below are harmless there and the session list still wants its
        // reload.
        const superseded = controller.current !== null && controller.current !== current;
        if (controller.current === current) controller.current = null;
        if (!superseded) {
          setBusy(false);
          // The stored history is the source of truth once a turn ends; the streamed
          // fragments existed only to show it arriving. This also picks up anything
          // that completed while this client was disconnected.
          let waiting = false;
          try {
            const detail = await api.chatSession(session.id);
            // The stored history replaces the streamed view only when the stream
            // reached its end. Cut off, it must not: the exchange is detached and
            // still running, so the store holds the developer's messages and none of
            // the answer — replacing with it wipes the turn off the screen and leaves
            // the conversation looking as though it had been cleared. It came back on
            // the next reattach, which replays the exchange from its own buffer, so
            // the loss was always temporary and always visible.
            //
            // Nothing is missed by waiting: a turn that ended while this client was
            // away is picked up by that same reattach, which finds no exchange
            // running and comes through here with `completed` set.
            if (completed) setTurns(replay(detail.messages));
            setPending(detail.pending_confirmations);
            onSessionChange(detail.session);
            waiting = detail.pending_confirmations.length > 0;
          } catch {
            // A reload failure leaves the streamed view in place, which is still
            // readable — better than clearing what the developer just watched arrive.
          }
          // Announced after the reload rather than on the last event, because what
          // the developer needs to know is which of the three endings it was, and a
          // held confirmation is only visible in the reloaded session. announce()
          // stays silent while this window is in front of them.
          if (ending === "failed") {
            announce("Turn failed", session.title || "The assistant stopped early.");
          } else if (ending === "answered" && waiting) {
            announce("Your decision is needed", session.title || "A tool is waiting to run.");
          } else if (ending === "answered") {
            announce("Reply ready", session.title || "The assistant has finished.");
          }
        }
      }
    },
    [consume, onSessionChange, session.id, session.title],
  );

  // Held in a ref so the subscription below, which is registered once, reattaches
  // with the current run rather than the one that existed when it subscribed.
  const attach = useRef(run);
  attach.current = run;

  // Reattach to a turn already in flight, whenever the socket comes up.
  //
  // This is what makes the detached exchange visible to the developer: reloading the
  // page or losing the connection during a five-minute profile now resumes the turn
  // instead of showing a conversation that appears to have stalled. chat_attach
  // answers attached=false when nothing is running, which is the common case.
  //
  // Subscribed once, and that is load-bearing rather than tidiness. onState replays
  // the current state to every new listener, so a resubscription while the socket is
  // open is itself an attach — and an attach ends by reloading the session, which
  // re-renders this component. Depending on anything that changes per render
  // therefore closed a loop: attach, reload, re-render, resubscribe, attach. It ran
  // as fast as the round trips came back, which is why it read as the backend being
  // flooded the moment a turn ended.
  //
  // The guard asks whether a *live* view exists, not whether the ref holds
  // something, and the difference is the whole of a bug. StrictMode mounts, tears
  // down and mounts again; the teardown runs the abort above, between the reattach's
  // two attempts. A ref still holding that aborted controller made the second
  // attempt skip itself — so the only attach was the cancelled one, and a developer
  // coming back to a conversation mid-turn found no sign that anything was
  // happening, which is exactly what a detached exchange must never look like.
  // Aborted from anywhere else — Stop, switching away — reads the same way here,
  // and correctly: what is aborted is not watching anything.
  useEffect(() => {
    // The conversation's own reason for wanting the connection up, and the one the
    // developer notices: the turn being reattached to here is one they started
    // before the reload. Idempotent, and the panel above asks for the same thing.
    odeSocket.ensureConnected();
    return odeSocket.onState((state: SocketState) => {
      if (state !== "open") return;
      // An in-flight send is already a view onto the same exchange, and taking it
      // over would be churn.
      const viewing = controller.current;
      if (viewing && !viewing.signal.aborted) return;
      void attach.current("chat_attach", {});
    });
  }, []);

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
    [run, session.id, setInput],
  );

  const decide = useCallback(
    (confirmation: PendingConfirmation, approve: boolean) => {
      setPending((existing) => existing.filter((entry) => entry.id !== confirmation.id));

      // A held call is answered in place. The provider's own tool loop is still
      // running and its relay is still open, so the result of this decision arrives
      // on the stream already being watched — starting a second one would detach
      // that view and replay the turn into it.
      if (confirmation.out_of_band) {
        void odeSocket.decide(session.id, confirmation.id, approve).catch((e: unknown) => {
          setError(describe(e));
        });
        return;
      }
      void run("chat_confirm", { confirmation_id: confirmation.id, approve });
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

  const move = useCallback(
    async (workbenchId: string) => {
      setError(null);
      try {
        onSessionChange(await api.moveChatSession(session.id, workbenchId));
        // Re-read rather than left as it was: the move puts a note in the
        // conversation, and a history on screen that does not show it would have the
        // developer reading file results from a checkout this session has left.
        const detail = await api.chatSession(session.id);
        setTurns(replay(detail.messages));
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
        <>
          <WorkbenchControl session={session} benches={benches} busy={busy} onMove={move} />
          <TierControl
            tier={session.exposure_tier}
            maxTier={maxTier}
            sessionId={session.id}
            surface={surface}
            onChange={setTier}
          />
        </>
      }
    >
      {error && <Muted>{error}</Muted>}

      {/*
        The transcript, and the one place where the scrolling is not ours.

        It used to be a plain div with a sentinel at the bottom and an effect that
        called `scrollIntoView` on every change to `turns`. That is wrong in the two
        cases that matter here. A streamed answer changes `turns` on nearly every
        frame, so the smooth scroll was permanently chasing itself; and scrolling up
        to re-read a tool result while the answer was still arriving yanked the
        developer back to the bottom on the next token.

        `MessageScrollerProvider` is anchoring that respects reader intent: it
        follows the tail while the reader is at the tail, and stops following the
        moment they scroll away — offering the jump-back button instead. Each turn is
        wrapped in a `MessageScrollerItem` so it can be measured and anchored to.
      */}
      {/*
        `autoScroll` and nothing else, which is the whole of the live-edge pattern:
        while the reader is at the tail the transcript follows a streamed reply, and
        the moment they scroll away it stops and leaves them where they are.

        Deliberately no `scrollAnchor` on the turns. Marking the developer's
        messages as turn boundaries reads well in a transcript being browsed, but it
        changes what *sending* does: the component scrolls the new question to the
        top of the viewport — padding with a spacer so it can get there even with
        nothing below it yet — and every previous answer goes above the fold. It
        looks exactly like the conversation was cleared, and it comes back only as
        the reply grows and the spacer collapses. Following the bottom is what was
        wanted; jumping to the top on send was not.
      */}
      <MessageScrollerProvider autoScroll defaultScrollPosition="end">
        <MessageScroller className="conversation relative min-h-0 flex-1">
          <MessageScrollerViewport
            /*
              The bottom fade off.

              `scroll-fade-b` is a `mask-image` on the scroll container, and a mask
              applies to everything the element paints — the scrollbar included. So
              the bottom of the scrollbar faded out with the text, which reads as a
              half-drawn control rather than as a soft edge. There is no way to fade
              one and not the other: it is one mask over the whole box.

              Turned off through the utility's own knob rather than by trying to
              drop the class, so a regenerated component keeps honouring it. The
              "Jump to the latest" button is the affordance for "there is more below"
              anyway, and it says so in words.
            */
            className="min-h-0 flex-1 overflow-y-auto [--scroll-fade-b-size:0px]"
          >
            <MessageScrollerContent
              /*
                The bottom padding is now just room to breathe above the composer.
                It was ten to clear the viewport's fade mask, which reached that far
                up and made the last line read as clipped; with the mask off it only
                has to stop the final turn touching the edge.

                Still worth stating rather than leaving at nothing: the scroller
                reads `padding-block-end` off this element in its height arithmetic,
                so whatever is here is space it anchors against rather than space it
                has to scroll past.
              */
              className="flex flex-col gap-4 pt-2 pb-4"
            >
              {turns.length === 0 && (
                <Muted>
                  Describe the problem you are trying to solve. The assistant starts from the
                  ontology, not from device names.
                </Muted>
              )}
              {turns.map((turn, index) => (
                <MessageScrollerItem
                  key={index}
                  messageId={String(index)}
                  /*
                    The item ships with `content-visibility: auto` and an intrinsic
                    size of 10rem, which is a bet that off-screen turns are all
                    about that tall. Here they are not: a one-line notice, a tool
                    call folded shut, and a thousand-word answer with a JSON blob
                    under it are all turns. Every one of them is laid out at 10rem
                    until it enters the viewport and then jumps to its real height,
                    which shifts everything below it and moves the scrollbar under
                    the reader's thumb — the scroll reads as jagged the whole way
                    through the history.

                    A conversation is bounded, so skipping layout for what is off
                    screen buys little; paying for it buys a scroll that tracks.
                  */
                  className="[content-visibility:visible] [contain-intrinsic-size:auto]"
                >
                  <TurnView turn={turn} onOpenChart={onOpenChart} />
                </MessageScrollerItem>
              ))}
              {/*
                `thinking busy` is a hook the render tests select on, not styling.
                The live region stays: a wait with nothing to announce is invisible
                to a screen reader, and this one lasts as long as a tool call does.

                Centred, because it belongs to the conversation rather than to
                either side of it — the same reason a notice is a separator rather
                than a message.

                No `animate-pulse` here, despite the `busy` class carrying it
                everywhere else: the spinner is already the motion, and a row that
                fades in and out around a thing that is spinning is two animations
                for one wait.
              */}
              {busy && (
                <Marker className="thinking busy justify-center" aria-live="polite">
                  <MarkerIcon>
                    <Spinner aria-label={undefined} role={undefined} />
                  </MarkerIcon>
                  <MarkerContent>Working…</MarkerContent>
                </Marker>
              )}
            </MessageScrollerContent>
          </MessageScrollerViewport>
          {/*
            No positioning here on purpose. The component already places itself:
            `inset-s-1/2` with `-translate-x-1/2`, plus its own `bottom-4`. Passing
            `inset-x-0 mx-auto` overrode the `left: 50%` while leaving the translate
            in place, so the button ended up half its own width left of centre.
          */}
          {/*
            `size="sm"` rather than the component's default `icon-sm`, which is a
            fixed `size-7` square meant for the bare arrow. With a label in it the
            text ran to both edges.
          */}
          <MessageScrollerButton direction="end" size="sm" className="shadow-sm">
            Jump to the latest
          </MessageScrollerButton>
        </MessageScroller>

      {pending.map((confirmation) => (
        <ConfirmationPrompt
          key={confirmation.id}
          confirmation={confirmation}
          onDecide={decide}
        />
      ))}

      <Composer
        input={input}
        busy={busy}
        usage={usage}
        onInput={setInput}
        onSend={submit}
        onStop={() => {
          // Both: stop the work server-side, and detach this view.
          odeSocket.cancelChat(session.id);
          controller.current?.abort();
        }}
      />
      </MessageScrollerProvider>
    </Pane>
  );
}

/**
 * The composer, and the one reason it is a component of its own.
 *
 * `useMessageScroller` has to be called *under* the provider, and `Conversation`
 * is the thing that renders the provider — so it cannot reach the hook itself.
 * Pulling the composer out puts it inside, which is what lets sending a message
 * take the reader to the bottom.
 *
 * That jump is the point. Auto-scroll follows the tail only while the reader is
 * already at the tail; someone who has scrolled up to re-read a tool result and
 * then types stays where they were, and their own message — plus the answer to it
 * — arrives somewhere off screen. Submitting is an unambiguous statement that the
 * end of the conversation is where they now want to be, so it is one of the few
 * places worth overriding their scroll position for.
 *
 * `smooth`, because this one is the developer's own action and the movement shows
 * them where they were taken. The auto-scroll during streaming is `auto` for the
 * opposite reason: it happens many times a second and animating it would smear.
 */
function Composer({
  input,
  busy,
  usage,
  onInput,
  onSend,
  onStop,
}: {
  input: string;
  busy: boolean;
  usage: Usage | null;
  onInput: (text: string) => void;
  onSend: (text: string) => void;
  onStop: () => void;
}) {
  const { scrollToEnd } = useMessageScroller();

  const send = useCallback(() => {
    const text = input.trim();
    if (text === "" || busy) return;
    onSend(text);
    scrollToEnd({ behavior: "smooth" });
  }, [busy, input, onSend, scrollToEnd]);

  return (
    <form
      className="composer mt-3 shrink-0 rounded-lg border bg-card focus-within:ring-[3px] focus-within:ring-ring/50"
      onSubmit={(event) => {
        event.preventDefault();
        send();
      }}
    >
      <Textarea
        value={input}
        onChange={(event) => onInput(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.shiftKey) {
            event.preventDefault();
            send();
          }
        }}
        placeholder="What are you trying to build?"
        rows={3}
        aria-label="Message"
        // The focus ring is drawn once, by the form, around the whole composer:
        // the textarea and the send button are one control as far as the eye is
        // concerned, and two nested rings read as a mistake.
        className="resize-none border-0 bg-transparent shadow-none focus-visible:ring-0"
      />
      <div className="composer-actions flex items-center gap-3 border-t px-3 py-2">
        {/*
          The running total first, the action last, and the action pushed to the
          far edge by `ml-auto` — which holds whether or not there is a total to
          put beside it, so the button does not move when the first reply lands.

          Reordering these costs nothing in the tab order: the usage is a `span`
          and never takes focus, so the submit is still the first thing reached
          from the textarea.
        */}
        {usage && (
          <span
            className="usage text-xs text-muted-foreground"
            title="Estimated from configured prices, not an invoice"
          >
            {num(usage.input_tokens + usage.output_tokens)} tokens
            {usage.cost_eur ? ` · ~${usage.cost_eur.toFixed(4)}` : ""}
          </span>
        )}
        {busy ? (
          <Button
            type="button"
            size="sm"
            variant="secondary"
            className="ml-auto"
            title="Abandon this turn. Closing the tab instead leaves it running."
            onClick={onStop}
          >
            Stop
          </Button>
        ) : (
          <Button type="submit" size="sm" className="ml-auto" disabled={!input.trim()}>
            Send
          </Button>
        )}
      </div>
    </form>
  );
}

/**
 * TierControl is §3.2's persistent surface, and the only way the tier changes:
 * there is no tool for it, so the assistant cannot raise its own exposure.
 */
/**
 * WorkbenchControl moves the conversation to another working context.
 *
 * The workbench is chosen when a conversation is opened, which is before the
 * developer necessarily knows what the conversation will turn out to be about. This
 * is the repair: it re-points every file tool and every cell in the session, and the
 * pairing in workbench.tsx then brings the code pane along.
 *
 * Absent where there is nothing to choose. A deployment with no repository surface
 * has no workbenches at all, and with one open there is nowhere else to go — an
 * unassigned conversation already acts in it. The exception is a conversation naming
 * a workbench that has since been closed, which is exactly the case a developer
 * needs this for.
 *
 * Disabled while a turn is running, because the backend refuses it then: that turn
 * read the session once and is acting in the workbench being moved away from. A
 * disabled control that says why is a better answer than a 400 in the error line.
 */
function WorkbenchControl({
  session,
  benches,
  busy,
  onMove,
}: {
  session: ChatSession;
  benches: Workbench[];
  busy: boolean;
  onMove: (workbenchId: string) => void;
}) {
  const assigned = session.workbench_id ?? "";
  // Named one that is not in the list: it has been closed. Its own option, so the
  // select shows the truth about where this conversation is rather than silently
  // displaying the first workbench as though it were the one.
  const closed = assigned !== "" && !benches.some((bench) => bench.id === assigned);
  if (benches.length === 0 || (benches.length < 2 && !closed)) return null;

  return (
    <div className="workbench-control flex items-center gap-2">
      <span className="workbench-control-label text-xs text-muted-foreground">Acts in</span>
      <Select
        value={closed ? CLOSED_WORKBENCH : assigned}
        disabled={busy}
        onValueChange={(value) => {
          // The closed-workbench entry is where the conversation *is*, not somewhere
          // it can be sent. Choosing it is a no-op rather than a move to nowhere.
          if (value === CLOSED_WORKBENCH || value === null) return;
          onMove(value);
        }}
      >
        <SelectTrigger
          size="sm"
          className="w-auto min-w-40"
          title={
            busy
              ? "A turn is running in this conversation. It is acting in the workbench " +
                "below, so the move waits for it to finish."
              : "Which checkout this conversation writes into, and which kernel its cells run in."
          }
          aria-label="The workbench this conversation acts in"
        >
          <SelectValue>
            {(value: string | null) => {
              if (value === CLOSED_WORKBENCH) return "closed workbench";
              if (value === "" || value === null) return "Wherever I am working";
              const bench = benches.find((candidate) => candidate.id === value);
              return bench ? workbenchLabel(bench) : value;
            }}
          </SelectValue>
        </SelectTrigger>
        <SelectContent>
          {closed && (
            <SelectItem value={CLOSED_WORKBENCH} disabled>
              closed workbench
            </SelectItem>
          )}
          <SelectItem value="">Wherever I am working</SelectItem>
          {benches.map((bench) => (
            <SelectItem key={bench.id} value={bench.id}>
              {workbenchLabel(bench)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

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
    <div className="tier-control flex max-w-72 flex-col items-end gap-1.5">
      {/*
        A toggle group rather than three buttons, because the three are one choice
        and only one of them can hold. `single` gives the group roving focus and
        arrow-key movement, which three separate buttons did not have.
      */}
      <ToggleGroup
        className="tier-buttons"
        variant="outline"
        size="sm"
        value={[tier]}
        // Single-select is the primitive's default, so the array holds at most one.
        // Clicking the pressed tier un-presses it and reports an empty array; a
        // session always has a tier, so that is read as "no change" rather than as
        // a move to nothing.
        onValueChange={(value) => {
          const picked = value[0];
          if (picked !== undefined && picked !== tier) onChange(picked as Tier);
        }}
        aria-label="Data exposure tier"
      >
        {TIERS.map((candidate, index) => (
          <ToggleGroupItem
            key={candidate}
            value={candidate}
            className={cn("tier-button", candidate === tier && `active tier-${candidate}`)}
            disabled={index > ceiling}
            title={
              index > ceiling
                ? `An administrator has capped your exposure tier at ${maxTier}.`
                : TIER_EXPOSES[candidate]
            }
          >
            {candidate}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
      <p className="tier-exposes text-right text-xs text-muted-foreground">{TIER_EXPOSES[tier]}</p>
      {surface && <RaiseHint tier={tier} surface={surface} />}
      <Button
        variant="link"
        size="xs"
        className="link h-auto p-0"
        onClick={() => setShowAudit(!showAudit)}
      >
        {showAudit ? "Hide" : "Show"} tier history
      </Button>
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
    <p className="tier-hint text-right text-xs text-muted-foreground">
      Raising to {next} would add:{" "}
      <strong className="font-medium text-foreground">{gained.join(", ")}</strong>.
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
  if (!changes) return <Busy>Loading…</Busy>;

  return (
    <ol className="tier-audit flex w-full flex-col gap-1">
      {changes.map((change, index) => (
        <li key={index} className="flex items-center justify-end gap-1.5 text-xs">
          <TierBadge tier={change.from} />
          <span aria-hidden>→</span>
          <TierBadge tier={change.to} />
          <span className="muted text-muted-foreground">{dateTime(change.at)}</span>
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
  onDecide: (confirmation: PendingConfirmation, approve: boolean) => void;
}) {
  return (
    <div className="confirmation mt-3 shrink-0 rounded-lg border border-primary/40 bg-card p-3">
      <div className="confirmation-head text-sm">
        <strong className="font-semibold">{confirmation.tool}</strong> needs your confirmation
      </div>
      {/* The arguments travel with the prompt: approving a tool name alone would be
          agreeing to something you cannot see. */}
      <pre className="json mt-2 max-h-56 overflow-auto rounded-md bg-muted p-2 font-mono text-xs">
        {JSON.stringify(confirmation.input, null, 2)}
      </pre>
      <div className="confirmation-actions mt-3 flex gap-2">
        <Button size="sm" onClick={() => onDecide(confirmation, true)}>
          Approve
        </Button>
        <Button size="sm" variant="outline" onClick={() => onDecide(confirmation, false)}>
          Decline
        </Button>
      </div>
    </div>
  );
}

/**
 * The icon and the tone each notice level is drawn with.
 *
 * Colour is never the only carrier — a notice always says what happened in words —
 * but the level is worth reading before the sentence is, and an icon does that
 * where a tint alone would not survive a monochrome screen.
 */
const NOTICE_TONE: Record<
  Extract<Turn, { kind: "notice" }>["level"],
  { icon: typeof InfoIcon; className: string }
> = {
  info: { icon: InfoIcon, className: "" },
  warn: { icon: AlertTriangleIcon, className: "text-foreground" },
  error: { icon: CircleAlertIcon, className: "text-destructive" },
};

function TurnView({
  turn,
  onOpenChart,
}: {
  turn: Turn;
  onOpenChart?: (chartId: string) => void;
}) {
  /*
   * A notice is not a message and is deliberately not drawn as one.
   *
   * `Marker` is the conversation's system-note row: full width, centred between
   * two rules, no avatar and no bubble. That is exactly the right shape for "a
   * limit was reached" or "something failed" — it belongs to the transcript
   * without claiming to have been said by either party.
   */
  if (turn.kind === "notice") {
    const tone = NOTICE_TONE[turn.level];
    return (
      <Marker variant="separator" className={cn(`notice notice-${turn.level}`, tone.className)}>
        <MarkerIcon>
          <tone.icon />
        </MarkerIcon>
        <MarkerContent>{turn.text}</MarkerContent>
      </Marker>
    );
  }
  if (turn.kind === "streaming") {
    return (
      <Message className="turn assistant">
        <MessageContent>
          {/* Rendered as markdown while it is still arriving: a heading that shows up as
              a hash and becomes a heading three tokens later is a worse read than one
              that was a heading from the start. */}
          <Markdown className="turn-body" text={turn.text} />
        </MessageContent>
      </Message>
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
  if (message.role === "assistant") {
    return (
      <Message className="turn assistant">
        <MessageContent>
          <Markdown className="turn-body" text={text} />
        </MessageContent>
      </Message>
    );
  }
  // A message ODE composed and injected is stored with the user role, because that
  // is what a model reads as input — but it is not the developer's, and rendering it
  // in their voice would put words in their mouth. §5.13's run summary and the note
  // a workbench move leaves are both this.
  //
  // So it takes the assistant's side of the conversation and says whose it is in the
  // header, rather than sitting in the developer's bubble on the right.
  if (message.origin === "ode") {
    return (
      <Message className="turn ode">
        <MessageContent>
          <MessageHeader className="turn-origin px-0">ODE</MessageHeader>
          <Bubble variant="outline" className="max-w-full">
            <BubbleContent className="turn-body border-dashed whitespace-pre-wrap">
              {text}
            </BubbleContent>
          </Bubble>
        </MessageContent>
      </Message>
    );
  }
  // The developer's own turn stays verbatim, and on the right. What they typed is
  // what they meant to send, and an ontology path with underscores in it should come
  // back looking the way it went out — hence `whitespace-pre-wrap` here and markdown
  // only on the assistant's side.
  return (
    <Message align="end" className={`turn ${message.role}`}>
      <MessageContent>
        {/*
          A `Bubble`, and not a div with the same padding on it.

          `MessageContent` right-aligns its children through
          `group-data-[align=end]/message:*:data-slot:self-end` — a selector that
          only matches children carrying a `data-slot`. A bare div has none, so
          `align="end"` reversed the row and then the content stretched full width
          again and the developer's turn sat on the left looking like everyone
          else's. `Bubble` carries the slot, and its own
          `group-data-[align=end]/message:self-end` besides.
        */}
        <Bubble variant="secondary">
          <BubbleContent className="turn-body whitespace-pre-wrap">{text}</BubbleContent>
        </Bubble>
      </MessageContent>
    </Message>
  );
}

/**
 * ToolTurn shows what the assistant did, and — when it was refused — what it would
 * have needed. A blocked call rendered as nothing would leave the developer
 * wondering why the answer was thin.
 *
 * Like a notice, this is a `Marker` rather than a `Message`: a tool call is
 * something that happened in the conversation, not something anybody said. The
 * difference is that this one opens.
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
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      render={
        <div
          className={cn(
            "tool-turn rounded-md border bg-card/50 px-3 py-2",
            result?.is_error && "tool-error border-destructive/40",
          )}
        />
      }
    >
      <CollapsibleTrigger
        render={<Marker render={<button type="button" />} className="tool-head cursor-default" />}
      >
        <MarkerIcon>
          <ChevronRightIcon className={cn("transition-transform", open && "rotate-90")} />
        </MarkerIcon>
        <MarkerContent className="tool-name font-mono text-foreground">{call.name}</MarkerContent>
        {result && (
          <Badge
            variant={result.is_error ? "destructive" : "secondary"}
            className={`tool-outcome outcome-${result.outcome} ml-auto font-normal`}
          >
            {result.outcome}
          </Badge>
        )}
        {!result && progress && <span className="tool-progress ml-auto text-xs">{progress}</span>}
        {refusal && (
          <span className="tool-refusal ml-auto text-xs text-destructive">
            needs {refusal.required}, session is at {refusal.blocked_by_tier}
          </span>
        )}
      </CollapsibleTrigger>
      {/*
        A chart specification is the one tool result that is worth nothing as JSON:
        §5.9 has the assistant emit a document and the pane draw it, and the values
        are read there under the developer's own token — the model never sees them.
        So the useful thing to offer here is the way in.

        Outside the collapsible on purpose: it is the action, and it should not need
        the disclosure opened to be found.
      */}
      {chartID && onOpenChart && (
        <div className="tool-chart mt-2 flex flex-wrap items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => onOpenChart(chartID)}>
            Open in exploration
          </Button>
          <span className="muted-inline text-xs text-muted-foreground">
            the assistant proposed a chart; the values behind it are read with your token
          </span>
        </div>
      )}
      <CollapsibleContent className="tool-body mt-2 flex flex-col gap-2">
        <div className="tool-part">
          <span className="tool-label text-xs font-medium text-muted-foreground">Arguments</span>
          <pre className="json mt-1 max-h-56 overflow-auto rounded-md bg-muted p-2 font-mono text-xs">
            {JSON.stringify(call.input ?? {}, null, 2)}
          </pre>
        </div>
        {result && (
          <div className="tool-part">
            <span className="tool-label text-xs font-medium text-muted-foreground">Result</span>
            <pre className="json mt-1 max-h-56 overflow-auto rounded-md bg-muted p-2 font-mono text-xs">
              {JSON.stringify(result.content, null, 2)}
            </pre>
          </div>
        )}
      </CollapsibleContent>
    </Collapsible>
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
