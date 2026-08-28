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

// @vitest-environment jsdom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { workbenchLabel, type ChatSession, type Session, type Workbench } from "./api";
import sessionDetail from "./__contract__/chat_session.json";
import sessionList from "./__contract__/chat_sessions.json";
import toolSurface from "./__contract__/chat_tools.json";

/**
 * What a stopped turn must not do: keep talking to the backend.
 *
 * The reattach subscription and the history reload that ends every turn are two
 * mechanisms that each look right on their own. Together they closed a loop —
 * reattach, reload, re-render, resubscribe — because `onState` replays the current
 * state to a new listener, so resubscribing over an open socket *is* an attach.
 * Nothing crashed and nothing rendered wrongly; the pane sat there issuing round
 * trips as fast as they came back. So the assertion is about call counts over time
 * rather than about anything on screen, which no render test would have caught.
 *
 * Driven through the real component with only the two process boundaries faked, the
 * way `experiments.render.test.tsx` does it: the socket, because a loop in the
 * client is only visible in what it sends, and the HTTP surface, for the reload.
 */

vi.mock("./keycloak", () => ({
  initKeycloak: vi.fn(async () => true),
  token: vi.fn(async () => "test-token"),
  logout: vi.fn(),
}));

/** Every stream the pane opened, in order, with the type it opened. */
let streamed: string[] = [];
/** How many times the pane reloaded the stored conversation. */
let reloads = 0;
/** Session cancellations — the Stop button's server-side half. */
let cancelled: string[] = [];
/** Resolves the in-flight chat_send, standing in for the turn ending. */
let finishSend: (() => void) | null = null;
/** Auto-mode changes that reached the backend, as [session, on]. */
let autoRunSet: [string, boolean][] = [];
/** Rejects the live stream the way a dropped socket does. */
let dropStream: (() => void) | undefined;
/** The live stream's event sink, so a test can stream a partial answer. */
let emit: ((event: unknown) => void) | undefined;
/** Whether a turn is running server-side, which is what chat_attach answers with. */
let live = false;
/** What the developer has sent, which the backend stores before the turn starts. */
let sent: string[] = [];
/** Decisions sent through the non-streaming path, as [confirmation, approve]. */
let decided: [string, boolean][] = [];
/** What the stored conversation reports as awaiting the developer. */
let pending: Record<string, unknown>[] = [];
/** Every alert the pane asked for, as [headline, body]. */
let announced: [string, string][] = [];
/** The sessions the pane is told exist. The draft test needs two. */
let listed: ChatSession[] = [];
/** Every rename the pane sent, as [id, title]. */
let renamed: [string, string][] = [];
/** Every workbench move the pane sent, as [id, workbench]. */
let moved: [string, string][] = [];
/** Notes ODE has put in the conversation, which a move is one of. */
let notes: string[] = [];
/** The workbenches the developer has open, for the pairing test. */
let benches: Workbench[] = [];
/** How many session watches the panel has opened. Counted apart from `streamed`,
 *  because that list is about the conversation's own streams and one test asserts
 *  its exact contents. */
let watches = 0;
/** The live watch's handler, so a test can report what a conversation is doing. */
let report: ((activity: { session_id: string; state: string }) => void) | null = null;
/** Fails the open watch, which is what a dropped connection does to it. */
let dropWatch: (() => void) | null = null;

vi.mock("./attention", () => ({
  announce: (headline: string, body: string) => {
    announced.push([headline, body]);
  },
}));

/** The socket's state listeners, and the state a new one is replayed. */
let listeners: ((state: string) => void)[] = [];
let socketState = "idle";

class FakeCancelled extends Error {
  constructor() {
    super("cancelled");
    this.name = "Cancelled";
  }
}

/** open drives the socket to a state, replaying it to every listener as ws.ts does. */
function openSocket() {
  socketState = "open";
  for (const listener of [...listeners]) listener("open");
}

/** Whether the pane asked for the connection rather than waiting to be handed one. */
let connectRequests = 0;

vi.mock("./ws", () => ({
  Cancelled: FakeCancelled,
  odeSocket: {
    ensureConnected() {
      // The real one connects and the state follows. Counted as well as acted on,
      // because a pane that never asks is exactly the bug: `onState` alone leaves
      // the socket idle, and both of this pane's subscriptions wait on `open`.
      connectRequests += 1;
      if (socketState !== "open") openSocket();
    },
    onState(listener: (state: string) => void) {
      listeners.push(listener);
      // The replay that makes a resubscription indistinguishable from a reconnect.
      listener(socketState);
      return () => {
        listeners = listeners.filter((entry) => entry !== listener);
      };
    },
    stream(
      type: string,
      payload: unknown,
      handlers: { signal?: AbortSignal; onEvent?: (event: unknown) => void },
    ): Promise<{ attached: boolean }> {
      streamed.push(type);
      if (type === "chat_send") {
        // The engine stores the developer's message before it begins the exchange,
        // so a later read of the conversation has it whether or not the turn ended.
        sent.push(String((payload as { message?: string }).message ?? ""));
        live = true;
      }
      return new Promise((resolve, reject) => {
        handlers.signal?.addEventListener(
          "abort",
          () => reject(new FakeCancelled()),
          { once: true },
        );
        // An attach that finds nothing running answers at once; anything watching a
        // live turn is held until the test says that turn ended.
        if (type === "chat_attach" && !live) resolve({ attached: false });
        else {
          emit = handlers.onEvent;
          finishSend = () => {
            live = false;
            resolve({ attached: true });
          };
          // A transport failure, as distinct from finishSend: the socket goes and
          // the exchange keeps running server-side. Not a FakeCancelled, which is
          // what an abort raises and means the view was detached on purpose.
          dropStream = () => {
            emit = undefined;
            reject(new Error("the socket closed"));
          };
        }
      });
    },
    watchSessions(handlers: {
      onActivity: (activity: { session_id: string; state: string }) => void;
      signal?: AbortSignal;
    }): Promise<void> {
      watches += 1;
      report = handlers.onActivity;
      return new Promise((_resolve, reject) => {
        const fail = () => reject(new FakeCancelled());
        dropWatch = fail;
        handlers.signal?.addEventListener("abort", fail, { once: true });
      });
    },
    cancelChat(sessionId: string) {
      cancelled.push(sessionId);
    },
    async decide(_sessionId: string, confirmationId: string, approve: boolean) {
      decided.push([confirmationId, approve]);
    },
  },
}));

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      chatSessions: async () => ({ sessions: listed }),
      renameChatSession: async (id: string, title: string) => {
        renamed.push([id, title]);
        const entry = listed.find((session) => session.id === id);
        if (!entry) throw new Error(`renamed a session that is not listed: ${id}`);
        // Trimmed, as the backend trims what it stores: the row has to show what
        // was kept rather than what was typed.
        return { ...entry, title: title.trim() };
      },
      moveChatSession: async (id: string, workbenchId: string) => {
        moved.push([id, workbenchId]);
        const entry = listed.find((session) => session.id === id);
        if (!entry) throw new Error(`moved a session that is not listed: ${id}`);
        // The backend leaves a note in the conversation, so the next read has one.
        notes.push(`ODE moved this conversation to another code workspace: ${workbenchId}.`);
        const updated = { ...entry, workbench_id: workbenchId };
        listed = listed.map((session) => (session.id === id ? updated : session));
        return updated;
      },
      workbenches: async () => ({ workbenches: benches, max: 3 }),
      toolSurface: async () => toolSurface as unknown as ReturnType<typeof actual.api.toolSurface>,
      providers: async () => ({ providers: [], default: "stub" }),
      setAutoRun: async (id: string, on: boolean) => {
        autoRunSet.push([id, on]);
        const entry = listed.find((candidate) => candidate.id === id);
        if (!entry) throw new Error(`auto-run set on a session that is not listed: ${id}`);
        entry.auto_run = on;
        return JSON.parse(JSON.stringify(entry));
      },
      chatSession: async () => {
        reloads += 1;
        // A fresh object each time, as a real read gives: the loop fed on exactly
        // this, because storing it re-rendered the pane above.
        const detail = JSON.parse(JSON.stringify(sessionDetail));
        // One row in the store, so a read of the session answers with what the list
        // holds. Without this the fake contradicts itself the moment a test sets up
        // a list entry, and the pane believes the read — as it should.
        const stored = listed.find((entry) => entry.id === detail.session.id);
        if (stored) detail.session = JSON.parse(JSON.stringify(stored));
        detail.pending_confirmations = JSON.parse(JSON.stringify(pending));
        for (const text of sent) {
          detail.messages.push({
            session_id: detail.session.id,
            seq: detail.messages.length + 1,
            role: "user",
            content: [{ type: "text", text }],
            created_at: "2026-01-01T00:00:00Z",
          });
        }
        for (const text of notes) {
          // Stored with the user role, because that is what a model reads as input,
          // and marked as ODE's — which is the pair the pane has to render apart.
          detail.messages.push({
            session_id: detail.session.id,
            seq: detail.messages.length + 1,
            role: "user",
            origin: "ode",
            subject: "workbench:wb-2",
            content: [{ type: "text", text }],
            created_at: "2026-01-01T00:00:00Z",
          });
        }
        return detail;
      },
    },
  };
});

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// jsdom has no layout, so it has no scrollIntoView. The conversation scrolls itself
// to the newest turn on every change, which would throw before any assertion ran.
Element.prototype.scrollIntoView = () => {};

const SESSION = {
  user_id: "u-1",
  username: "dev",
  roles: ["developer"],
  is_admin: false,
  exposure_tier: "L1",
  max_exposure_tier: "L2",
  features: { chat: true },
} as unknown as Session;

const mounted: Root[] = [];

beforeEach(() => {
  autoRunSet = [];
  streamed = [];
  reloads = 0;
  cancelled = [];
  finishSend = null;
  decided = [];
  pending = [];
  announced = [];
  listeners = [];
  socketState = "idle";
  connectRequests = 0;
  watches = 0;
  report = null;
  dropWatch = null;
  live = false;
  sent = [];
  listed = JSON.parse(JSON.stringify(sessionList.sessions)) as ChatSession[];
  benches = [];
  renamed = [];
  moved = [];
  notes = [];
});

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
});

/** settle gives the pane many turns to do whatever it is going to keep doing. */
async function settle(turns = 30) {
  for (let i = 0; i < turns; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

/** open mounts the pane with the contract's conversation already selected. */
async function open(): Promise<HTMLElement> {
  window.history.replaceState({}, "", "/tools/chat?session=id-1");
  vi.resetModules();
  const { ChatView } = await import("./chat");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<ChatView session={SESSION} />));
  await settle(3);
  return host;
}

/**
 * openPaired mounts the pane the way App does: inside the workbench provider.
 *
 * `open` above leaves it out, and the provider answers a pane outside it with an
 * empty state — which is what a deployment with no repository surface has. The
 * pairing between a conversation and the code beside it needs the real thing.
 */
async function openPaired(): Promise<HTMLElement> {
  window.history.replaceState({}, "", "/?session=id-1");
  vi.resetModules();
  const { ChatView } = await import("./chat");
  const { WorkbenchProvider } = await import("./workbench");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () =>
    root.render(
      <WorkbenchProvider>
        <ChatView session={SESSION} />
      </WorkbenchProvider>,
    ),
  );
  await settle(3);
  return host;
}

/** A workbench, as the provider's list answers with one. Only its id is read here. */
function workbench(id: string): Workbench {
  return { id, link: { full_name: `dev/operator-${id}` } } as unknown as Workbench;
}

/** type puts text in the composer without sending it, as typing does. */
async function type(host: HTMLElement, text: string) {
  const box = host.querySelector("textarea") as HTMLTextAreaElement;
  await act(async () => {
    // Through the native setter, or React's controlled value never sees it and the
    // composer stays empty — which submits nothing and passes for the wrong reason.
    const value = Object.getOwnPropertyDescriptor(
      HTMLTextAreaElement.prototype,
      "value",
    )?.set;
    value?.call(box, text);
    box.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

/** ask types a message and submits it, as a developer does. */
async function ask(host: HTMLElement) {
  const form = host.querySelector("form.composer") as HTMLFormElement;
  await type(host, "which devices are there?");
  await act(async () => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
}

/**
 * openStrict mounts the pane the way main.tsx does.
 *
 * StrictMode is not decoration here. It mounts, tears down and mounts again, and
 * the conversation's effects have to survive that — the reattach is the one that
 * did not, because the teardown's abort ran between its two attempts.
 */
async function openStrict(): Promise<HTMLElement> {
  window.history.replaceState({}, "", "/tools/chat?session=id-1");
  vi.resetModules();
  const { ChatView } = await import("./chat");
  const { StrictMode } = await import("react");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () =>
    root.render(
      <StrictMode>
        <ChatView session={SESSION} />
      </StrictMode>,
    ),
  );
  await settle(3);
  return host;
}

/**
 * Whether the conversation on screen says a turn is in flight.
 *
 * `.busy` is part of the query rather than incidental to it: the caption is the
 * only thing on screen for the minutes a turn can take, and it is the animation
 * that separates "still running" from "left behind by a request that died". A
 * caption that lost the class would still read `Working…` and still pass a check
 * that only looked for `.thinking`.
 */
function working(host: HTMLElement): boolean {
  return host.querySelector(".thinking.busy") !== null;
}

/** composer reads back what is in the box on screen. */
function composer(host: HTMLElement): string {
  return (host.querySelector("textarea") as HTMLTextAreaElement).value;
}

/** switchTo opens another session from the list, the way the developer does. */
async function switchTo(host: HTMLElement, title: string) {
  const open = [...host.querySelectorAll("button.session-open")].find((entry) =>
    entry.textContent?.includes(title),
  );
  if (!open) throw new Error(`no session named ${title} in the list`);
  await act(async () => open.dispatchEvent(new MouseEvent("click", { bubbles: true })));
  await settle(3);
}

function press(host: HTMLElement, label: string) {
  const button = [...host.querySelectorAll("button")].find(
    (entry) => entry.textContent?.trim() === label,
  );
  if (!button) throw new Error(`no ${label} button on screen`);
  return act(async () => button.dispatchEvent(new MouseEvent("click", { bubbles: true })));
}

// --- the flood ---

/** How many times the pane has attached, for the counts that must not grow. */
function attaches(): number {
  return streamed.filter((type) => type === "chat_attach").length;
}

/**
 * The conversation is the direct child of the pane body, and that is load-bearing.
 *
 * `MessageScroller` does its own scrolling and anchoring, so the pane body around
 * it must not scroll too. The rule that stops it — `.pane-body:has(> .conversation)`
 * in `index.css` — is a *child* selector, so putting anything between the two, or
 * renaming either class, silently restores the nested scrollbar: the composer
 * scrolls off the bottom of the pane and the scroller never reaches the end it is
 * anchoring to. Nothing else in the build notices, which is why this is a test.
 */
it("hangs the conversation directly off the pane body, so only one of them scrolls", async () => {
  const host = await open();

  const conversation = host.querySelector(".conversation");
  expect(conversation, "the conversation is not on screen").not.toBeNull();
  expect(conversation?.parentElement?.classList.contains("pane-body")).toBe(true);

  // And the composer is a sibling of it rather than inside it, which is what lets
  // the conversation take the remaining height while the composer keeps its own.
  const composer = host.querySelector("form.composer");
  expect(composer?.parentElement).toBe(conversation?.parentElement);
});

/**
 * The developer's own turn sits on the right, and the assistant's does not.
 *
 * The right-alignment is not the `align="end"` prop by itself. `MessageContent`
 * moves its child over with `group-data-[align=end]/message:*:data-slot:self-end`,
 * which matches only children that carry a `data-slot` — so a turn rendered into a
 * bare `<div>` reversed the row, stretched back to full width and landed on the
 * left again, looking exactly like the assistant's. That is what this asserts: the
 * row is marked `end`, and the thing inside it is slot-bearing so the rule can
 * reach it.
 */
it("puts the developer's turn on the right and the assistant's on the left", async () => {
  const host = await open();
  await ask(host);

  const theirs = host.querySelector(".turn.user");
  expect(theirs, "the developer's turn is not on screen").not.toBeNull();
  expect(theirs?.getAttribute("data-align")).toBe("end");
  // The slot the alignment rule selects on. Without it the prop above is inert.
  expect(theirs?.querySelector("[data-slot='bubble']")).not.toBeNull();

  // The assistant's side is untouched: a long answer reads better full width than
  // in a bubble, so it stays at the start and carries no alignment override.
  const replay = host.querySelector(".turn.assistant");
  expect(replay?.getAttribute("data-align") ?? "start").toBe("start");
});

/**
 * A dropped socket must not take the answer off the screen.
 *
 * The turn is detached server-side, so losing the stream says nothing about it —
 * it carries on, and the exchange keeps every event it published so the reattach
 * can replay them. What the *store* holds meanwhile is only what was complete
 * before the turn began: the developer's messages and nothing else.
 *
 * So re-reading the store when the stream dies replaces a conversation in progress
 * with one that looks emptied — every answer gone, only the questions left, and no
 * indication why. It came back on the next reattach, which is what made it read as
 * a glitch rather than as a loss.
 */
it("keeps a streamed answer on screen when the socket drops mid-turn", async () => {
  const host = await open();
  await ask(host);
  await settle(3);

  // A partial answer, streamed. This exists nowhere but on screen: the store has
  // the question and will not have the reply until the turn ends.
  await act(async () => emit?.({ type: "text_delta", text: "the oven draws" }));
  await settle(2);
  expect(host.textContent).toContain("the oven draws");

  await act(async () => dropStream?.());
  await settle(5);

  // Still there. The turn is still running; nothing has happened that says the
  // answer is not coming.
  expect(host.textContent, "a dropped socket emptied the conversation").toContain(
    "the oven draws",
  );
  // And the developer's own message is not the only thing left, which is what the
  // failure looked like from the outside.
  expect(host.querySelectorAll(".turn.assistant").length).toBeGreaterThan(0);
});

it("stopping a turn leaves the backend alone", async () => {
  const host = await open();
  // Mounting brings the connection up, so the pane has already attached once and
  // found nothing running. What must not grow is the count from here.
  const attached = attaches();
  await ask(host);
  await settle(3);

  const before = reloads;
  await press(host, "Stop");
  await settle();

  expect(cancelled).toEqual(["id-1"]);
  // One reload, from the stopped turn's own finally. Not one per round trip.
  expect(reloads - before).toBe(1);
  expect(attaches()).toBe(attached);
});

it("a turn that ends on its own leaves the backend alone", async () => {
  const host = await open();
  const attached = attaches();
  await ask(host);
  await settle(3);

  const before = reloads;
  await act(async () => finishSend?.());
  await settle();

  expect(reloads - before).toBe(1);
  expect(attaches()).toBe(attached);
});

// --- the alert when a turn ends ---

/*
 * The three endings the pane distinguishes, asserted here rather than in
 * `attention.test.ts` because the module cannot see them: whether a stream ended,
 * failed, was abandoned, or attached to nothing is knowledge the pane holds. Which
 * of the alert's signals then fire — and whether any do, given where the window is
 * — is the module's business and tested there.
 */
it("alerts once when a turn ends with an answer", async () => {
  const host = await open();
  await ask(host);
  await act(async () => openSocket());
  await settle(3);

  await act(async () => finishSend?.());
  await settle();

  expect(announced).toEqual([["Reply ready", sessionDetail.session.title]]);
});

/*
 * The attach that finds nothing running happens on every socket open, including
 * the one that follows a page load. Alerting there would flash the taskbar of a
 * developer who has just arrived, for a turn that never existed.
 */
it("says nothing when an attach finds no turn in flight", async () => {
  await open();
  await act(async () => openSocket());
  await settle();

  expect(streamed).toContain("chat_attach");
  expect(announced).toEqual([]);
});

/*
 * Stop is the developer saying they are done with this turn. They are looking at
 * the window when they press it, and they will not be waiting for what it
 * produces.
 */
it("says nothing when the developer stops the turn", async () => {
  const host = await open();
  await ask(host);
  await act(async () => openSocket());
  await settle(3);

  await press(host, "Stop");
  await settle();

  expect(announced).toEqual([]);
});

/*
 * A held confirmation is the one ending that is worse to miss than a finished
 * answer: nothing is running, and nothing will until the developer comes back.
 */
it("alerts differently when the turn ended on a decision", async () => {
  const host = await open();
  await ask(host);
  await act(async () => openSocket());
  await settle(3);

  pending = [
    {
      id: "conf-1",
      tool: "run_code",
      input: {},
      tier: "L0",
      created_at: "2026-01-01T00:00:00Z",
      out_of_band: false,
    },
  ];
  await act(async () => finishSend?.());
  await settle();

  expect(announced).toEqual([["Your decision is needed", sessionDetail.session.title]]);
});

// --- what the subscription is for, still working ---

/*
 * The loop is not fixed by dropping the reattach: a five-minute profile that
 * outlives the connection is exactly what it exists for. A genuine transition to
 * open, with nothing being watched here, must still attach — once.
 */
// --- auto mode ---

/*
 * The developer's standing answer, and what the interface promises about it.
 *
 * The switch sits beside the tier because it is the same kind of decision: what
 * this conversation may do without interrupting. What it must not do is claim to
 * be a safety check — the backend recognises a small vocabulary and confirms
 * everything else, and code runs in the developer's own pod either way. So the
 * label says what happens ("without asking") rather than what it is not doing
 * ("safe"), and this asserts that, because the wording is the part a later edit
 * would soften without noticing.
 */
it("offers auto mode beside the tier, and sends the developer's answer", async () => {
  const host = await open();
  await settle(3);

  const control = host.querySelector(".auto-run");
  expect(control, "no auto-mode control in the conversation").not.toBeNull();
  expect(control?.textContent).toContain("without asking");
  // Never sold as a safety property. The title carries the caveat in full.
  expect(control?.textContent?.toLowerCase()).not.toContain("safe");

  const toggle = control?.querySelector("[role='switch']") as HTMLElement;
  expect(toggle, "the control is not a switch").not.toBeNull();
  await act(async () => toggle.click());
  await settle(3);

  expect(autoRunSet).toEqual([["id-1", true]]);
});

// --- answering a confirmation ---

/*
 * A confirmation the provider's own tool call is holding open is answered in place.
 * The distinction is not cosmetic: the turn holding it never stopped, so opening a
 * chat_confirm stream would abort the relay showing the answer arrive and then
 * replay the whole turn into a view that already has it.
 */
it("a held confirmation is decided without opening a second stream", async () => {
  pending = [
    {
      id: "conf-1",
      tool: "run_code",
      input: {},
      tier: "L0",
      created_at: "2026-08-27T00:00:00Z",
      out_of_band: true,
    },
  ];

  const host = await open();
  await ask(host);
  await act(async () => openSocket());
  await settle(3);

  await press(host, "Approve");
  await settle(3);

  expect(decided).toEqual([["conf-1", true]]);
  expect(streamed.filter((type) => type === "chat_confirm")).toHaveLength(0);
});

/*
 * A reload lands on the decision that is still owed.
 *
 * The cold case, and the one the other two do not cover: no socket, no turn
 * watched from its start, nothing in this tab's memory — just the pane mounting
 * against a session that already has a confirmation waiting. That is what a browser
 * reload is, and it is the moment a developer is most likely to meet a held call,
 * because the reason they still owe an answer is usually that they were away.
 *
 * The card has to come from the stored read alone. Nothing replays the event that
 * first announced it: `chat_attach` carries what happens next, not what already
 * happened, so a pane that waited for the socket to tell it would show nothing.
 */
it("shows a confirmation that was already waiting when the pane mounted", async () => {
  pending = [
    {
      id: "conf-3",
      tool: "run_code",
      input: { code: "print(1)" },
      tier: "L0",
      created_at: "2026-08-27T00:00:00Z",
      out_of_band: true,
    },
  ];

  // No ask(), no openSocket(): a mount and nothing else.
  const host = await open();
  await settle(3);

  expect(host.querySelector(".confirmation"), "no confirmation card after a reload").not.toBeNull();
  expect(host.textContent).toContain("run_code");
  // The arguments travel with it. Approving a tool name alone would be agreeing to
  // something the developer cannot see.
  expect(host.textContent).toContain("print(1)");

  // And it is answerable from here, on the held path rather than by opening a
  // second stream into a turn that never stopped.
  await press(host, "Approve");
  await settle(3);
  expect(decided).toEqual([["conf-3", true]]);
});

/* And the ordinary one still resumes the turn it paused, which streams. */
it("an ordinary confirmation is answered on a stream", async () => {
  pending = [
    {
      id: "conf-2",
      tool: "run_code",
      input: {},
      tier: "L0",
      created_at: "2026-08-27T00:00:00Z",
    },
  ];

  const host = await open();
  await settle(3);

  await press(host, "Approve");
  await settle(3);

  expect(decided).toEqual([]);
  expect(streamed).toContain("chat_confirm");
});

/*
 * --- the reload, and the turn that was running when it happened ---
 *
 * An exchange is detached, so F5 closes the socket and the backend goes on working.
 * The pane's job on the way back up is to attach to it, and two subscriptions do
 * that: this conversation's reattach, and the panel's watch over every other one.
 * Both begin on the socket's first `open` state — and nothing in a freshly loaded
 * tab sends anything, so nothing produced one. The turn ran to its end in the
 * backend while the conversation on screen sat there looking finished, until the
 * developer typed something else and the send brought the connection up.
 *
 * The socket is faked here, so the assertion has to be that the pane *asks* for the
 * connection rather than waiting to be handed one. `connectRequests` is that ask;
 * the mock then does what ws.ts does, which is open and replay the state.
 */
it("attaches to a turn still running when the pane is mounted", async () => {
  // What a reload lands in: the backend is mid-turn and this tab has just started.
  live = true;

  const host = await openStrict();

  expect(connectRequests).toBeGreaterThan(0);
  expect(streamed).toContain("chat_attach");
  expect(working(host)).toBe(true);
  // And the panel is following the conversations the developer is not reading, for
  // the same reason and over the same connection.
  expect(watches).toBe(1);
});

it("a reconnect while idle attaches once", async () => {
  await open();
  // Mounting asks for the connection, and the open that follows is itself an attach.
  expect(streamed).toEqual(["chat_attach"]);

  await act(async () => openSocket());
  await settle();

  // One more for the reconnect, and only one: an attach ends by reloading the
  // session, and a subscription that re-registered on that render would be read as
  // another reconnect and attach again, as fast as the round trips came back.
  expect(streamed).toEqual(["chat_attach", "chat_attach"]);
});

// --- the mark on a conversation nobody is reading ---

/*
 * The panel has to say which conversation has come back.
 *
 * A turn is detached, so a developer who starts one and switches to another session
 * leaves it running with nothing in the tab watching it: the conversation's stream
 * went with the component. attention.ts does not cover the case either — it fires
 * from inside the open conversation, and only when the whole window is in the
 * background. Without a mark on the row, the answer arrives and nothing says so.
 */

/** The words the panel is showing against each session, keyed by title. */
function marks(host: HTMLElement): Record<string, string> {
  const found: Record<string, string> = {};
  for (const row of host.querySelectorAll("button.session-open")) {
    const mark = row.querySelector(".session-mark");
    if (mark) found[row.querySelector(".session-title")?.firstChild?.textContent ?? ""] = mark.textContent ?? "";
  }
  return found;
}

/** says reports one state change, as the backend's chat_watch does. */
async function says(sessionId: string, state: string) {
  if (!report) throw new Error("the panel is not watching");
  await act(async () => report?.({ session_id: sessionId, state }));
  await settle(3);
}

it("marks a conversation the developer is not reading when its turn ends", async () => {
  listed = [...listed, { ...listed[0], id: "id-2", title: "the other conversation" }];
  const host = await open();
  await act(async () => openSocket());
  await settle(3);

  await says("id-2", "running");
  expect(marks(host)["the other conversation"]).toBe("Working");

  await says("id-2", "idle");
  expect(marks(host)["the other conversation"]).toBe("Reply ready");

  // Opening it is reading it, and a mark on the conversation on screen is noise.
  await switchTo(host, "the other conversation");
  expect(marks(host)["the other conversation"]).toBeUndefined();
});

it("marks a conversation that has stopped on a decision", async () => {
  listed = [...listed, { ...listed[0], id: "id-2", title: "the other conversation" }];
  const host = await open();
  await act(async () => openSocket());
  await settle(3);

  // Reported without this tab having seen the turn start, which is the case after
  // a reload: a decision is owed until somebody makes it.
  await says("id-2", "waiting");
  expect(marks(host)["the other conversation"]).toBe("Needs you");
});

it("keeps the live marks on the conversation on screen, and only drops 'reply ready'", async () => {
  const host = await open();
  await act(async () => openSocket());
  await settle(3);

  // "working" and "needs you" are facts about the conversation, not about the
  // reader, so being open changes neither. The row used to go blank exactly when
  // the developer selected it, which cost the list its one continuous answer to
  // "which of these is busy".
  await says("id-1", "running");
  expect(marks(host)[sessionList.sessions[0].title]).toBe("Working");
  await says("id-1", "waiting");
  expect(marks(host)[sessionList.sessions[0].title]).toBe("Needs you");

  // "reply ready" is the one that is about the reader — it means "you have not
  // been back since it finished", which is false of the conversation they are
  // looking at.
  await says("id-1", "idle");
  expect(marks(host)).toEqual({});
});

it("leaves an idle conversation nobody asked anything unmarked", async () => {
  listed = [...listed, { ...listed[0], id: "id-2", title: "the other conversation" }];
  const host = await open();
  await act(async () => openSocket());
  await settle(3);

  // Idle is the ordinary state of most of the list. Marking on it alone would put
  // "reply ready" against every conversation the developer has ever had.
  await says("id-2", "idle");
  expect(marks(host)).toEqual({});
});

/*
 * Reading a conversation is not the same event as its turn ending.
 *
 * The mark used to be one value per row, so the only way to take "reply ready" off
 * the row being read was to drop the row's entry — which threw away the engine's
 * word that its turn was still running. Opening a working conversation and moving
 * on therefore left it unmarked for as long as it kept working, and the developer
 * who had switched away to wait for exactly that answer had nothing to wait on
 * until the socket next reconnected.
 */
it("still marks a running turn after its conversation has been opened and left", async () => {
  listed = [...listed, { ...listed[0], id: "id-2", title: "the other conversation" }];
  const host = await open();
  await act(async () => openSocket());
  await settle(3);

  await says("id-2", "running");
  await switchTo(host, "the other conversation");
  // Still marked while it is the one on screen: the turn is running either way.
  expect(marks(host)["the other conversation"]).toBe("Working");

  // Nothing about the turn changed by being looked at, and the row still says so
  // once the developer is elsewhere.
  await switchTo(host, sessionList.sessions[0].title);
  expect(marks(host)["the other conversation"]).toBe("Working");

  await says("id-2", "idle");
  expect(marks(host)["the other conversation"]).toBe("Reply ready");
});

/* And a decision is still owed after the developer has read it and not made it. */
it("still marks a held decision after its conversation has been opened and left", async () => {
  listed = [...listed, { ...listed[0], id: "id-2", title: "the other conversation" }];
  const host = await open();
  await act(async () => openSocket());
  await settle(3);

  await says("id-2", "waiting");
  await switchTo(host, "the other conversation");
  await switchTo(host, sessionList.sessions[0].title);
  expect(marks(host)["the other conversation"]).toBe("Needs you");
});

/*
 * Coming back to a conversation mid-turn has to show the turn.
 *
 * Switching sessions unmounts the conversation and detaches its view of the
 * exchange — which keeps running, that being the point of detaching it. So the
 * remount has to put both halves back: the question the developer asked, out of the
 * stored conversation, and "Working…", out of a fresh attach. Without the attach the
 * pane reads as a conversation where nothing is happening, which is the one thing a
 * detached exchange must never look like.
 *
 * Under StrictMode it did not. React mounts, tears down and mounts again; the
 * teardown aborts the controller the first attach installed, and the guard on the
 * second attempt asked whether the ref held a controller rather than whether it held
 * a live one. So the only attach was the cancelled one. Mounted the way main.tsx
 * mounts it, or the case cannot appear at all.
 */
it("shows a turn still running when its conversation is opened again", async () => {
  listed = [...listed, { ...listed[0], id: "id-2", title: "the other conversation" }];
  const host = await openStrict();
  await act(async () => openSocket());
  await settle(3);

  await ask(host);
  expect(working(host)).toBe(true);

  await switchTo(host, "the other conversation");
  await switchTo(host, sessionList.sessions[0].title);

  expect(host.textContent).toContain("which devices are there?");
  expect(working(host)).toBe(true);
});

it("watches once per connection, and again after one drops", async () => {
  await open();
  // The panel asks for the connection itself, so it is watching before anything
  // has been sent — which is the whole point of a watch over conversations nobody
  // is typing in.
  expect(connectRequests).toBeGreaterThan(0);
  expect(watches).toBe(1);

  await act(async () => openSocket());
  await settle();
  // Still once, not once per render: onState replays "open" to every new listener,
  // which is what turned the conversation's own reattach into a loop.
  expect(watches).toBe(1);

  await act(async () => {
    dropWatch?.();
  });
  await settle(3);
  await act(async () => openSocket());
  await settle(3);
  expect(watches).toBe(2);
});

it("settles what it was watching when the connection drops", async () => {
  listed = [...listed, { ...listed[0], id: "id-2", title: "the other conversation" }];
  const host = await open();
  await act(async () => openSocket());
  await settle(3);
  await says("id-2", "running");

  // The connection that was reporting on that turn has been away, so the tab no
  // longer knows it is running. A turn that ended meanwhile is exactly what the
  // developer needs telling about; one that did not is put back by the snapshot the
  // next watch opens with.
  await act(async () => {
    dropWatch?.();
  });
  await settle(3);
  await act(async () => openSocket());
  await settle(3);

  expect(marks(host)["the other conversation"]).toBe("Reply ready");
});

// --- the half-typed message ---

/*
 * A draft belongs to the developer, not to the conversation it was typed into.
 *
 * Conversation is keyed by session id so that a switch replaces the turns and the
 * held confirmations wholesale — and that same remount used to take the composer's
 * contents with it. The developer switched away to check a device name in another
 * session, came back, and the sentence they were writing was gone. So the draft is
 * held above the key, one per session: each conversation shows its own, and neither
 * shows the other's.
 */
it("keeps each session's half-typed message across a switch", async () => {
  listed = [
    ...listed,
    { ...listed[0], id: "id-2", title: "the other conversation" },
  ];

  const host = await open();
  await type(host, "half a question about");

  await switchTo(host, "the other conversation");
  // Not the first session's text, which would put a message in front of the wrong
  // conversation — and could send it there.
  expect(composer(host)).toBe("");
  await type(host, "a note in the second");

  await switchTo(host, sessionList.sessions[0].title);
  expect(composer(host)).toBe("half a question about");

  await switchTo(host, "the other conversation");
  expect(composer(host)).toBe("a note in the second");
});

/* And sending still empties the box, rather than leaving the sent text behind. */
it("clears the draft when the message goes", async () => {
  const host = await open();
  await ask(host);
  await settle(3);

  expect(composer(host)).toBe("");
});

/**
 * The code beside the conversation is the conversation's own code.
 *
 * The rule itself, and both of its awkward directions, are pinned in
 * workbench.test.tsx, where they can be exercised without a socket. What this test
 * covers is the wiring: that this pane publishes its own session list and the id of
 * the open one, so that switching conversations moves `?workbench=` — the parameter
 * the Code pane, `run_code` and every repository read resolve through. Drop the
 * pairing call from the pane and the rule still holds everywhere except here, which
 * is the only place a developer meets it.
 */
it("switching conversations moves the code pane to that operator", async () => {
  benches = [workbench("wb-1"), workbench("wb-2")];
  listed = [
    { ...listed[0], workbench_id: "wb-1" },
    {
      ...listed[0],
      id: "id-2",
      title: "the other conversation",
      workbench_id: "wb-2",
    },
  ];

  const host = await openPaired();
  expect(new URLSearchParams(window.location.search).get("workbench")).toBe("wb-1");

  await switchTo(host, "the other conversation");

  expect(new URLSearchParams(window.location.search).get("workbench")).toBe("wb-2");
});

/*
 * What the line under a session title is for.
 *
 * It used to end in a message count, which is ODE's own bookkeeping: it grows by
 * two every turn and answers nothing a developer standing in front of this list
 * asks. With two operators open at once, the question is which one a conversation
 * is about — the same workbench the code pane follows when the session is opened —
 * so the row names it.
 */
function about(host: HTMLElement): (string | null)[] {
  return [...host.querySelectorAll(".session-about")].map((entry) => entry.textContent);
}

it("names the workbench each conversation is about", async () => {
  benches = [workbench("wb-1"), workbench("wb-2")];
  listed = [
    { ...listed[0], workbench_id: "wb-1" },
    {
      ...listed[0],
      id: "id-2",
      title: "the other conversation",
      workbench_id: "wb-2",
    },
  ];

  const host = await openPaired();

  expect(about(host)).toEqual([
    "stub · dev/operator-wb-1",
    "stub · dev/operator-wb-2",
  ]);
});

/*
 * A session that names no workbench is one from before workbenches existed, and
 * every read of it resolves to the developer's only workbench. The row says the
 * same thing, rather than leaving the one conversation on screen looking as though
 * it belonged to no code at all.
 */
it("names the only workbench for a conversation that names none", async () => {
  benches = [workbench("wb-1")];

  const host = await openPaired();

  expect(about(host)).toEqual(["stub · dev/operator-wb-1"]);
});

/*
 * And a workbench that has since been closed is said to be closed. That is also
 * the reason opening this conversation does not move the code pane: the checkout it
 * was about is not on screen, and a row that named it would point at nothing.
 */
it("says when the workbench a conversation was about has been closed", async () => {
  benches = [workbench("wb-1")];
  listed = [{ ...listed[0], workbench_id: "wb-gone" }];

  const host = await openPaired();

  expect(about(host)).toEqual(["stub · closed workbench"]);
});

/*
 * Renaming a conversation, and the three ways an edit ends.
 *
 * The title a session starts with is ODE's guess — the first eight words of the
 * opening message — which is why the row can be renamed at all. What needs pinning
 * is not the request but which gesture sends one: Enter does, Escape does not, and
 * neither may send a second one when the input then loses focus, which is exactly
 * what happened before `settled` existed. A removed element delivers a focusout in
 * some browsers and not in others, so the flag is the only thing making the two
 * orders agree.
 */

/** rename opens the box on the first row and types into it. */
async function rename(host: HTMLElement, text: string): Promise<HTMLInputElement> {
  const open = host.querySelector("button.session-rename-open") as HTMLButtonElement;
  await act(async () => open.dispatchEvent(new MouseEvent("click", { bubbles: true })));
  const box = host.querySelector("input.session-rename") as HTMLInputElement;
  await act(async () => {
    const value = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    value?.call(box, text);
    box.dispatchEvent(new Event("input", { bubbles: true }));
  });
  return box;
}

/** key sends one keystroke to the box, as a developer's finger does. */
async function key(box: HTMLInputElement, name: string) {
  await act(async () => {
    box.dispatchEvent(new KeyboardEvent("keydown", { key: name, bubbles: true }));
  });
  await settle(3);
}

function titles(host: HTMLElement): (string | null)[] {
  return [...host.querySelectorAll(".session-name")].map((entry) => entry.textContent);
}

it("renames a conversation from its row", async () => {
  const host = await open();
  const box = await rename(host, "  the pv forecast operator  ");
  await key(box, "Enter");

  expect(renamed).toEqual([["id-1", "  the pv forecast operator  "]]);
  // What the backend kept, not what was typed: the row shows the trimmed name.
  expect(titles(host)).toEqual(["the pv forecast operator"]);
  // And the box is gone, rather than left open over the row it renamed.
  expect(host.querySelector("input.session-rename")).toBeNull();
});

it("leaves the name alone when the edit is abandoned", async () => {
  const host = await open();
  const box = await rename(host, "half a name");
  await key(box, "Escape");

  expect(renamed).toEqual([]);
  expect(titles(host)).toEqual([sessionList.sessions[0].title]);
});

/*
 * Both keys are dispatched together with the focusout, in one act, which is the
 * order that matters: React has not re-rendered yet, so the input is still mounted
 * and its blur handler still reaches the pane. A browser that delivers the blur of
 * a removed element before the next paint does exactly this. Dispatched in separate
 * acts, the input is already gone and the handler cannot run at all — which is why
 * these two say nothing about the flag unless the events share a batch.
 */
it("sends nothing when the abandoned box loses focus in the same batch", async () => {
  const host = await open();
  const box = await rename(host, "half a name");
  await act(async () => {
    box.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    box.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
  });
  await settle(3);

  expect(renamed).toEqual([]);
  expect(titles(host)).toEqual([sessionList.sessions[0].title]);
});

/* And a committed rename is sent once rather than twice, for the same reason. */
it("sends one request when a committed box loses focus in the same batch", async () => {
  const host = await open();
  const box = await rename(host, "named once");
  await act(async () => {
    box.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    box.dispatchEvent(new FocusEvent("focusout", { bubbles: true }));
  });
  await settle(3);

  expect(renamed).toEqual([["id-1", "named once"]]);
});

/* Opening the box and pressing Enter is not a rename. */
it("sends nothing when the name has not changed", async () => {
  const host = await open();
  const open_ = host.querySelector("button.session-rename-open") as HTMLButtonElement;
  await act(async () => open_.dispatchEvent(new MouseEvent("click", { bubbles: true })));
  const box = host.querySelector("input.session-rename") as HTMLInputElement;
  await key(box, "Enter");

  expect(renamed).toEqual([]);
  expect(titles(host)).toEqual([sessionList.sessions[0].title]);
});

/* Losing focus commits, because clicking away from a name you just typed reads as
   being done with it rather than as discarding it. */
it("commits the typed name when the box loses focus", async () => {
  const host = await open();
  const box = await rename(host, "typed then clicked away");
  await act(async () => box.dispatchEvent(new FocusEvent("focusout", { bubbles: true })));
  await settle(3);

  expect(renamed).toEqual([["id-1", "typed then clicked away"]]);
  expect(titles(host)).toEqual(["typed then clicked away"]);
});

/*
 * Moving the open conversation to another workbench.
 *
 * The control is in the conversation's own header rather than on a session row,
 * because the moment a developer needs it is the moment they are reading the
 * conversation and notice it is about the other operator. What is asserted here is
 * the whole path a click takes: the call goes out, the pairing follows it so the code
 * pane is no longer showing the checkout the conversation has left, and the note the
 * backend leaves is read back into the history rather than staying invisible until
 * the next reload.
 */

/**
 * The header's workbench picker, absent where there is nothing to choose.
 *
 * The trigger, not a `<select>`: the control is a listbox that renders its options
 * into a popup only once opened, so the trigger is the only part of it that is on
 * the page at rest. Its text is the chosen option's, which is what the assertions
 * below want to read.
 */
function picker(host: HTMLElement): HTMLElement | null {
  return host.querySelector(".workbench-control [data-slot='select-trigger']");
}

/** choose picks a workbench in the header, as a developer does: open, then click. */
async function choose(host: HTMLElement, label: string) {
  const trigger = picker(host);
  if (!trigger) throw new Error("the conversation has no workbench picker");
  await act(async () => trigger.click());
  // The options are in a portal, so they are looked for on the document rather than
  // under the host the pane was rendered into.
  const option = [...document.querySelectorAll("[role='option']")].find(
    (entry) => entry.textContent === label,
  );
  if (!option) {
    const offered = [...document.querySelectorAll("[role='option']")].map((e) => e.textContent);
    throw new Error(`no workbench option named ${label}; the picker offers ${offered.join(", ")}`);
  }
  await act(async () => (option as HTMLElement).click());
  await settle(5);
}

it("moves the open conversation to another workbench, and the code pane follows", async () => {
  benches = [workbench("wb-1"), workbench("wb-2")];
  listed = [{ ...listed[0], workbench_id: "wb-1" }];

  const host = await openPaired();
  expect(new URLSearchParams(window.location.search).get("workbench")).toBe("wb-1");

  await choose(host, workbenchLabel(benches[1]));

  expect(moved).toEqual([["id-1", "wb-2"]]);
  // The pairing is what makes the move mean something: the file tools and the cells
  // now act in wb-2, and the code pane has to be showing that checkout.
  expect(new URLSearchParams(window.location.search).get("workbench")).toBe("wb-2");
});

it("shows the note the move leaves, in ODE's voice and not the developer's", async () => {
  benches = [workbench("wb-1"), workbench("wb-2")];
  listed = [{ ...listed[0], workbench_id: "wb-1" }];

  const host = await openPaired();
  await choose(host, workbenchLabel(benches[1]));

  const injected = host.querySelectorAll(".turn.ode");
  expect(injected).toHaveLength(1);
  expect(injected[0].textContent).toContain("moved this conversation");
  // Labelled, because the model answers it as though it had been asked and the
  // developer should not have to work out who said it.
  expect(injected[0].querySelector(".turn-origin")?.textContent).toBe("ODE");
  // And not rendered as the developer's own turn, which is the mistake worth a test:
  // it is stored with the user role, beside the messages they really did type.
  const theirs = [...host.querySelectorAll(".turn.user")].map((turn) => turn.textContent ?? "");
  expect(theirs.some((text) => text.includes("moved this conversation"))).toBe(false);
});

it("offers no move where there is nowhere to move to", async () => {
  // One workbench: an unassigned conversation already acts in it, and clearing the
  // assignment would change nothing.
  benches = [workbench("wb-1")];
  listed = [{ ...listed[0], workbench_id: "wb-1" }];

  expect(picker(await openPaired())).toBeNull();
});

it("offers the move when the conversation names a workbench that has been closed", async () => {
  // The case a developer needs this for most: the checkout the conversation was
  // about is not open, so nothing pairs with it and the code pane stays where it is.
  benches = [workbench("wb-1")];
  listed = [{ ...listed[0], workbench_id: "wb-closed" }];

  const trigger = picker(await openPaired());
  expect(trigger).not.toBeNull();
  // Showing the truth about where the conversation is, rather than the first
  // workbench as though it were the one.
  expect(trigger?.textContent).toContain("closed workbench");
});

it("does not offer the move while a turn is running", async () => {
  benches = [workbench("wb-1"), workbench("wb-2")];
  listed = [{ ...listed[0], workbench_id: "wb-1" }];

  const host = await openPaired();
  await ask(host);
  await act(async () => openSocket());
  await settle(3);

  // The backend refuses it: the running turn read the session once and is acting in
  // wb-1 for the rest of it. Disabled here, so the refusal is not something the
  // developer has to read out of an error line.
  // `disabled` on a listbox trigger is the attribute, not the DOM property a
  // `<select>` would have carried.
  expect(picker(host)?.hasAttribute("disabled")).toBe(true);

  await act(async () => finishSend?.());
  await settle();
  expect(picker(host)?.hasAttribute("disabled")).toBe(false);
});
