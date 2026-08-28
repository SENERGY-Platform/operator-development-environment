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

import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Workbench } from "./api";
import type { PairedConversation } from "./workbench";

/**
 * Switching workbenches, and the state that moves with it.
 *
 * A workbench is one checkout, and no two of a developer's workbenches hold the
 * same repository. `?file=` is a path inside one of those checkouts, so carrying it
 * across a switch asks the new workbench for a file it does not have — which is
 * what the Files pane then reports, as a 404 for a file the developer never opened.
 *
 * `?session=` moves the other way: a conversation is about one operator, so the
 * pair has to stay coherent in both directions — the code pane follows the
 * conversation the address names, and a tab that changes the code moves to that
 * workbench's own conversation rather than leaving another operator's beside it.
 *
 * These tests pin the address after each way of switching, because the address is
 * the only place this state lives.
 */

/** The list the mocked route answers with, and the workbench a create returns. */
let listed: Workbench[] = [bench("wb-1", "franzmueller/operator-test")];
let created: Workbench = bench("wb-3", "");
/** Ids the mocked delete was asked for, so the close test can prove it ran. */
let closed: string[] = [];
/**
 * The conversations the chat pane has published, newest first as it lists them.
 *
 * Null is that pane's own request still being in flight, which is the state a
 * reload starts in — the list below is published from a fetch, not from a literal.
 */
let conversations: PairedConversation[] | null = [];

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      workbenches: async () => ({ workbenches: listed, max: 3 }),
      createWorkbench: async () => created,
      deleteWorkbench: async (id: string) => {
        closed.push(id);
        listed = listed.filter((entry) => entry.id !== id);
      },
    },
  };
});

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function bench(id: string, fullName: string): Workbench {
  const [owner = "", name = ""] = fullName.split("/");
  return {
    id,
    link: {
      workbench_id: id,
      full_name: fullName,
      name,
      owner,
      default_branch: "main",
      private: false,
      clone_url: fullName ? `https://github.com/${fullName}.git` : "",
      html_url: fullName ? `https://github.com/${fullName}` : "",
      path: fullName,
      selected_at: "2026-08-20T09:00:00Z",
    },
    created_at: "2026-08-20T09:00:00Z",
    last_used_at: "2026-08-20T09:00:00Z",
  };
}

const mounted: Root[] = [];

beforeEach(() => {
  listed = [
    bench("wb-1", "franzmueller/operator-test"),
    bench("wb-2", "franzmueller/operator-test-2"),
  ];
  created = bench("wb-3", "");
  closed = [];
  conversations = [];
  routed = null;
  publish = null;
});

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
});

/** The router of the module graph the current test mounted, for writing the URL. */
let routed: typeof import("./router") | null = null;
/** The mounted stand-in's setter, for publishing a list after the mount. */
let publish: ((listed: PairedConversation[] | null) => void) | null = null;

/**
 * Mounts the bar at `address` and returns its host, once the list has loaded.
 *
 * Beside the bar sits a stand-in for the chat pane: the one thing that pane does
 * for the workbenches is publish its conversation list and the id of the open one,
 * which is `useConversationPairing`. Mounting the real pane would drag its socket
 * and its whole HTTP surface in for two fields.
 */
async function open(address: string): Promise<HTMLElement> {
  window.history.replaceState({}, "", address);
  vi.resetModules();
  const { WorkbenchProvider, WorkbenchBar, useConversationPairing } = await import("./workbench");
  const router = await import("./router");
  routed = router;

  function Chat() {
    // Held as state rather than read straight from the module, so that a test can
    // answer with the list *after* the mount — see `publishConversations`.
    const [held, setHeld] = useState<PairedConversation[] | null>(conversations);
    publish = setHeld;
    useConversationPairing(held, router.useParam("session"));
    return null;
  }

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () =>
    root.render(
      <WorkbenchProvider>
        <Chat />
        <WorkbenchBar />
      </WorkbenchProvider>,
    ),
  );
  // One more turn, for the list the provider reads on mount and the effect that
  // resolves the address against it.
  await act(async () => {
    await Promise.resolve();
  });
  return host;
}

/** Opens a conversation, the way every path in the SPA does: by writing `?session=`. */
async function openConversation(id: string | null): Promise<void> {
  if (!routed) throw new Error("nothing is mounted");
  await act(async () => routed?.setParam("session", id));
  await act(async () => {
    await Promise.resolve();
  });
}

/**
 * The chat pane's own request answering, after the mount rather than before it.
 *
 * `open` takes whatever `conversations` holds as the pane's opening state, which is
 * every test where the list is simply there. This is for the ones about the order
 * the two requests answer in.
 */
async function publishConversations(listed: PairedConversation[]): Promise<void> {
  if (!publish) throw new Error("nothing is mounted");
  await act(async () => publish?.(listed));
  await act(async () => {
    await Promise.resolve();
  });
}

/** A conversation about a workbench, as the chat pane publishes it. */
function conversation(id: string, workbench?: string): PairedConversation {
  return { id, workbench_id: workbench };
}

/** The tab whose label contains `label`. */
function tab(host: HTMLElement, label: string): HTMLElement {
  const found = [...host.querySelectorAll<HTMLElement>("[data-slot='tabs-trigger']")].find(
    (candidate) => (candidate.textContent ?? "").includes(label),
  );
  if (!found) throw new Error(`no workbench tab contains "${label}"`);
  return found;
}

function params(): URLSearchParams {
  return new URLSearchParams(window.location.search);
}

it("drops the open file when a tab switches workbench", async () => {
  const host = await open("/?workbench=wb-1&file=solar.py");

  await act(async () => tab(host, "operator-test-2").click());

  expect(params().get("workbench")).toBe("wb-2");
  // The reported failure: solar.py exists in the checkout that was left, so asking
  // the new one for it answers 404 for a file nobody opened.
  expect(params().get("file")).toBeNull();
});

it("leaves the editor alone when the tab already on screen is clicked", async () => {
  const host = await open("/?workbench=wb-1&file=op.py");

  await act(async () => tab(host, "operator-test").click());

  expect(params().get("workbench")).toBe("wb-1");
  expect(params().get("file")).toBe("op.py");
});

it("drops the open file when a new workbench is opened", async () => {
  const host = await open("/?workbench=wb-1&file=op.py");

  const button = host.querySelector<HTMLButtonElement>("button.workbench-new");
  await act(async () => button?.click());

  expect(params().get("workbench")).toBe("wb-3");
  expect(params().get("file")).toBeNull();
});

it("drops the open file when the workbench on screen is closed", async () => {
  const host = await open("/?workbench=wb-2&file=solar.py");

  const button = host.querySelector<HTMLButtonElement>("button.workbench-close");
  await act(async () => button?.click());
  // The list is re-read after the delete, and the provider's effect then resolves
  // the emptied parameter against what is left.
  await act(async () => {
    await Promise.resolve();
  });

  expect(closed).toEqual(["wb-2"]);
  // Nothing names wb-1: one workbench left means an unnamed request is unambiguous
  // again, which is the state a developer who never opened a second one is in.
  expect(params().get("workbench")).toBeNull();
  expect(params().get("file")).toBeNull();
});

it("drops the open file when the address names a workbench that is gone", async () => {
  await open("/?workbench=wb-gone&file=solar.py");

  expect(params().get("workbench")).toBe("wb-1");
  expect(params().get("file")).toBeNull();
});

/*
 * The one case that keeps the file: an address that named no workbench never
 * claimed one, so `/?file=op.py` — a link written before the recipient had a second
 * workbench, or by someone who has one — still opens that file in the workbench
 * chosen for them. It may not be there, and then the pane says so; dropping it
 * would break the link even when it is.
 */
it("keeps a file named by an address that named no workbench", async () => {
  await open("/?file=op.py");

  expect(params().get("workbench")).toBe("wb-1");
  expect(params().get("file")).toBe("op.py");
});

it("renders nothing while a single workbench has no repository", async () => {
  listed = [bench("wb-1", "")];

  const host = await open("/");
  expect(host.querySelector(".workbench-bar")).toBeNull();
});

/*
 * --- the conversation and the code, in both directions ---
 *
 * One workbench is one operator, and a conversation is about one operator: the
 * assistant writes into that checkout and runs code in that kernel. So the pane
 * beside the conversation has to be its own, and the two parameters that say so
 * are reconciled rather than allowed to drift.
 */

it("puts the code pane on the workbench the open conversation is about", async () => {
  conversations = [conversation("s-2", "wb-2"), conversation("s-1", "wb-1")];

  // The address a reload, a bookmark or a colleague's link restores: the sticky
  // conversation, and no workbench of its own. Before the pairing, the first
  // workbench was chosen here — so a conversation about the second operator opened
  // beside the first one's repository.
  await open("/?session=s-2");

  expect(params().get("workbench")).toBe("wb-2");
});

it("moves the code pane when another conversation is opened", async () => {
  conversations = [conversation("s-2", "wb-2"), conversation("s-1", "wb-1")];
  await open("/?workbench=wb-1&session=s-1&file=op.py");

  // Written straight into the address, the way the session list, the link from an
  // experiment and the back button all reach it.
  await openConversation("s-2");

  expect(params().get("workbench")).toBe("wb-2");
  // The file was a path in the other checkout.
  expect(params().get("file")).toBeNull();
});

it("leaves the code pane alone for a conversation that names no workbench", async () => {
  conversations = [conversation("s-0"), conversation("s-2", "wb-2")];
  await open("/?workbench=wb-2&session=s-2");

  await openConversation("s-0");

  // A session written before workbenches existed names none, and the backend reads
  // that as "my only workbench". It claims nothing, so it moves nothing.
  expect(params().get("workbench")).toBe("wb-2");
});

/*
 * A conversation may name a workbench that has since been closed: the checkout
 * outlives the workbench and nothing clears the column. Following it would mean
 * writing an id the provider then repairs, which repairs an id this follows again —
 * so the address is left as the provider resolved it. `settle` is the assertion
 * that matters here: a single read cannot tell a resolved address from one still
 * being written back and forth.
 */
it("does not chase a conversation whose workbench has been closed", async () => {
  conversations = [conversation("s-gone", "wb-closed")];
  await open("/?session=s-gone");

  for (let turn = 0; turn < 10; turn++) {
    await act(async () => {
      await Promise.resolve();
    });
  }

  expect(params().get("workbench")).toBe("wb-1");
  expect(params().get("session")).toBe("s-gone");
});

it("opens that workbench's newest conversation when a tab switches the code", async () => {
  conversations = [
    conversation("s-3", "wb-2"),
    conversation("s-2", "wb-2"),
    conversation("s-1", "wb-1"),
  ];
  const host = await open("/?workbench=wb-1&session=s-1");

  await act(async () => tab(host, "operator-test-2").click());

  expect(params().get("workbench")).toBe("wb-2");
  // Newest first, as the chat pane lists them: the conversation the developer was
  // last in on that operator, not the oldest one they ever started.
  expect(params().get("session")).toBe("s-3");
});

it("closes the conversation when the workbench switched to has none", async () => {
  conversations = [conversation("s-1", "wb-1")];
  const host = await open("/?workbench=wb-1&session=s-1");

  await act(async () => tab(host, "operator-test-2").click());

  expect(params().get("workbench")).toBe("wb-2");
  // Leaving s-1 open would put a conversation about the other operator beside this
  // code, and the assistant would be asked to write files it was never told about.
  expect(params().get("session")).toBeNull();
});

it("keeps a conversation that names no workbench across a tab switch", async () => {
  conversations = [conversation("s-0")];
  const host = await open("/?workbench=wb-1&session=s-0");

  await act(async () => tab(host, "operator-test-2").click());

  expect(params().get("workbench")).toBe("wb-2");
  expect(params().get("session")).toBe("s-0");
});

/*
 * A `?session=` the list does not have is a stale link, and the chat pane says so
 * in those words. Replacing it on a workbench switch would answer a broken link
 * with a conversation the developer did not ask for — and the same guard is what
 * keeps a session created a moment ago, and not yet published, from being replaced
 * by an older one about the same workbench.
 */
it("keeps a conversation the published list does not have", async () => {
  conversations = [conversation("s-1", "wb-1"), conversation("s-2", "wb-2")];
  const host = await open("/?workbench=wb-1&session=s-unknown");

  await act(async () => tab(host, "operator-test-2").click());

  expect(params().get("workbench")).toBe("wb-2");
  expect(params().get("session")).toBe("s-unknown");
});

/*
 * The pairing writes as little as it can. `?workbench=` absent means "my only
 * workbench" to the backend and to the provider, so a developer who has one never
 * has to have it resolved — and every address they copy stays as short as it was
 * before workbenches existed.
 */
it("names no workbench in the address when the developer has only one", async () => {
  listed = [bench("wb-1", "franzmueller/operator-test")];
  conversations = [conversation("s-1", "wb-1")];

  await open("/?session=s-1&file=op.py");

  expect(params().get("workbench")).toBeNull();
  // And the file the link named survives, which it would not if following the
  // conversation went through a workbench switch.
  expect(params().get("file")).toBe("op.py");
});

it("keeps the file a link named when it named no workbench", async () => {
  conversations = [conversation("s-2", "wb-2")];

  // Two open, so the choice does have to be written — but the address never claimed
  // a workbench, so the path it names is not one this switch is leaving behind.
  await open("/?session=s-2&file=op.py");

  expect(params().get("workbench")).toBe("wb-2");
  expect(params().get("file")).toBe("op.py");
});


/*
 * --- the reload, and which of its two requests answers first ---
 *
 * The address a reload restores names a conversation and a file but no workbench,
 * and resolving it needs both lists: the workbenches, which the provider reads, and
 * the conversations, which belong to the chat pane. Two requests, no ordering
 * between them — so the provider routinely has to choose a workbench before the
 * conversation that would have chosen for it has arrived.
 *
 * What it chose is then corrected. The correction must not read as a switch: the
 * file in the address was never the guessed workbench's to lose, and before this it
 * came down to which request was quicker whether a reload kept the editor open.
 */

it("keeps the open file when the conversation list answers after the workbench list", async () => {
  conversations = null;
  await open("/?session=s-2&file=op.py");

  // Two are open and the address named none, so a workbench had to be written —
  // and the conversation that could have said which had not been published yet.
  expect(params().get("workbench")).toBe("wb-1");
  expect(params().get("file")).toBe("op.py");

  await publishConversations([conversation("s-2", "wb-2")]);

  expect(params().get("workbench")).toBe("wb-2");
  expect(params().get("file")).toBe("op.py");
});

it("drops the file for a conversation opened after that correction", async () => {
  conversations = null;
  await open("/?session=s-2&file=op.py");
  await publishConversations([conversation("s-2", "wb-2"), conversation("s-1", "wb-1")]);

  // The workbench in the address is now the conversation's, which is a claim. So the
  // next conversation about another operator is an ordinary switch again.
  await openConversation("s-1");

  expect(params().get("workbench")).toBe("wb-1");
  expect(params().get("file")).toBeNull();
});

it("drops the file when a tab switches away from the workbench that was assumed", async () => {
  conversations = null;
  const host = await open("/?session=s-2&file=op.py");

  // No correction: the published list says the conversation is about the workbench
  // already on screen. The tab is then a switch like any other.
  await publishConversations([conversation("s-2", "wb-1")]);
  await act(async () => tab(host, "operator-test-2").click());

  expect(params().get("workbench")).toBe("wb-2");
  expect(params().get("file")).toBeNull();
});
