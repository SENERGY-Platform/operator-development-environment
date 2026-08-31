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
import type {
  RepoCommitDraft,
  RepoStatus,
  RepoVerification,
  Session,
  Workbench,
} from "./api";

/**
 * The repository bar, and the promises it has to keep while holding the state in a
 * line of text.
 *
 * The bar replaced a pane, which means three things that used to be unconditionally
 * on screen are now a click away: the change list with commit, stash and discard,
 * the warning paragraphs, and the checkout's path. Two of those are load-bearing
 * rather than cosmetic — §5.11 item 6 requires the uncommitted work to be surfaced
 * with its three answers on reopen, and a refusal has to say why it refused — so
 * they are asserted here rather than left to a screenshot.
 *
 * Only process boundaries are faked: the platform API and Monaco. Monaco is mocked
 * because importing it drags several megabytes of editor into jsdom for a test that
 * never opens a file, not because anything here is about the editor.
 */

/**
 * The reconnection, stubbed at the module boundary.
 *
 * What it does — open a popup, wait for a message, spend the code — is tested in
 * github.test.ts against a fake window. What matters here is the bar's part of it:
 * that a refusal caused by a stale credential offers the repair, and that taking the
 * offer finishes the action the developer originally asked for.
 */
let reconnects = 0;
let reconnectFails: Error | null = null;

vi.mock("./github", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./github")>();
  return {
    ...actual,
    reconnect: async () => {
      reconnects++;
      if (reconnectFails) throw reconnectFails;
    },
  };
});

vi.mock("./monaco", () => ({
  monaco: {
    // Enough of an editor for the component's own lifecycle: it subscribes, binds
    // Ctrl-S and disposes. The tests below open a file but do not type in it.
    editor: {
      create: () => ({
        onDidChangeModelContent: () => ({ dispose: () => {} }),
        addCommand: () => {},
        getValue: () => "",
        getModel: () => null,
        dispose: () => {},
      }),
    },
    KeyMod: { CtrlCmd: 0 },
    KeyCode: { KeyS: 0 },
  },
  monacoLanguage: () => "plaintext",
}));

/** The status the mocked repo routes answer with. Set by each test before it renders. */
let current: RepoStatus = status();
/**
 * A gate the status route waits behind, for the one test about the wait itself.
 *
 * `/repo` is the slow route of the pane — it reads the checkout and, on the first
 * open of a browser session, fetches from GitHub — and every other test here wants
 * it instant. Null except where the wait is the subject.
 */
let holdStatus: Promise<void> | null = null;
/** Scopes the connected GitHub grant is missing, which is a warning of its own. */
let missingScopes: string[] = [];
/** The workbenches the list route answers with. Empty for the tests without a bar. */
let benches: Workbench[] = [];
/** Which mutating repo routes were called, in order. */
let calls: string[] = [];
/**
 * Every call to the status route, as whether it asked for a fetch.
 *
 * Kept apart from `calls`, which several tests assert whole: the status route is
 * read constantly and would drown the mutations those tests are about.
 */
let statusCalls: boolean[] = [];
/** How many more times the push route refuses for a stale credential. */
let pushRefusals = 1;
/** When set, the status route refuses with this — which is what puts the picker, or
 * the lapsed card, on screen instead of the working copy. */
let statusRejects: { status: number; message: string; body: Record<string, unknown> } | null = null;
/** Whether the repository list refuses for a lapsed credential. */
let repositoriesRefuse = false;
/** What GitHub is reported to say about the credential when asked. */
let verification: RepoVerification = {
  valid: false,
  code: 401,
  message: "Bad credentials",
  scopes: [] as string[],
  scopes_reported: false,
  kind: "ghu_ — a GitHub App's user access token: no scopes, expires in hours",
  length: 40,
  age: "19h4m2s",
};
/** A refusal for the push route that has nothing to do with the credential. */
let pushRejects: { status: number; message: string; body: Record<string, unknown> } | null = null;
/** What the draft route answers. */
let draftAnswer: RepoCommitDraft = {
  message: "feat(op): read the second input\n\nThe forecast needs it.",
  files: 1,
  committed: false,
  provider: "anthropic",
  model: "claude-opus-5",
};
/**
 * What each workbench's checkout holds. `/repo/files` is workbench-scoped, so the
 * tree on screen has to be the one belonging to the workbench on screen — which is
 * the whole point of the switch test at the bottom of this file.
 */
const FILES: Record<string, string[]> = {
  "": ["main.py"],
  "wb-1": ["main.py", "solar.py"],
  "wb-2": ["main.py", "wind.py"],
};

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      repoConnection: async (verify = false) => ({
        connected: true,
        scopes_requested: ["repo", "workflow"],
        // Only when asked for, exactly as the route behaves: the pane's poll must not
        // spend a GitHub round trip.
        verification: verify ? verification : undefined,
        identity: {
          login: "franzmueller",
          connected_at: "2026-08-20T09:00:00Z",
          scopes: ["repo"],
          missing_scopes: missingScopes,
        },
      }),
      // `fetched` is answered the way the route answers it: a fact about *this*
      // call and not about the checkout. A status asked without a fetch reports
      // false however recently the remote was last reached, which is the whole
      // reason the pane cannot read freshness off it.
      repoStatus: async (fetch = false) => {
        statusCalls.push(fetch);
        if (holdStatus) await holdStatus;
        if (statusRejects) {
          throw new actual.ApiError(statusRejects.status, statusRejects.message, statusRejects.body);
        }
        return { ...current, fetched: current.fetched && fetch };
      },
      // The repository list is a GitHub API call, and therefore one of the places a
      // lapsed credential surfaces — with no way out of it until now.
      repoRepositories: async () => {
        calls.push("repoRepositories");
        if (repositoriesRefuse) {
          throw new actual.ApiError(
            409,
            'GitHub rejected the stored credential; reconnect the GitHub account: GitHub answered "Bad credentials"',
            {
              needs: "github_connection",
              hint: "the stored GitHub credential is no longer accepted — connect the account again",
            },
          );
        }
        return { repositories: [] };
      },
      repoDisconnect: async () => {
        calls.push("repoDisconnect");
      },
      // A push that the backend refuses with the repair attached. 409 rather than
      // 502, because a credential GitHub no longer accepts is not something that
      // broke upstream — see repo.explainAuth.
      repoPush: async () => {
        calls.push("repoPush");
        if (pushRejects) {
          throw new actual.ApiError(pushRejects.status, pushRejects.message, pushRejects.body);
        }
        if (pushRefusals > 0) {
          pushRefusals--;
          throw new actual.ApiError(
            409,
            "GitHub rejected the stored credential; reconnect the GitHub account",
            {
              needs: "github_connection",
              hint: "the stored GitHub credential is no longer accepted — it was revoked, expired, or the authorisation was withdrawn; connect the account again",
            },
          );
        }
        return { branch: "main", remote: "origin", output: "main -> main", head_sha: "abc1234" };
      },
      // The draft route. Recorded rather than merely answered: what makes the draft
      // acceptable at all is that it commits nothing, and the test below checks that
      // by watching which routes were called.
      repoCommitMessage: async () => {
        calls.push("repoCommitMessage");
        return draftAnswer;
      },
      repoCommit: async (message: string) => {
        calls.push(`repoCommit:${message}`);
        return { sha: "abc1234", subject: message, files: 1, branch: "main" };
      },
      workbenches: async () => ({ workbenches: benches, max: 3 }),
      // The refusal the backend sends for a path that is not in the checkout, in its
      // own words: it is what a stale `?file=` used to produce after a switch.
      repoFile: async (path: string) => {
        const held = FILES[actual.getActiveWorkbench()] ?? [];
        if (!held.includes(path)) {
          throw new actual.ApiError(
            404,
            `no such path in the workspace: operator-test/${path} does not exist`,
          );
        }
        return { path, size: 1, text: `# ${path}`, binary: false, truncated: false };
      },
      // Answered through the module's active workbench, the same way the real route
      // is: a tree that ignored it could not show a switch going wrong.
      repoFiles: async () => ({
        tree: {
          name: "operator-test",
          path: "operator-test",
          type: "directory" as const,
          children: (FILES[actual.getActiveWorkbench()] ?? []).map((name) => ({
            name,
            path: `operator-test/${name}`,
            type: "file" as const,
            size: 1,
          })),
        },
      }),
    },
  };
});

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const SESSION = {
  user_id: "u-1",
  username: "dev",
  email: "dev@example.org",
  roles: ["developer"],
  is_admin: false,
  exposure_tier: "L0",
  features: {
    profiler: false,
    selection: false,
    chat: false,
    mcp: false,
    kernel: false,
    charts: false,
    relations: false,
    repo: true,
    experiments: false,
  },
} as Session;

/** A working copy in the state most of these tests start from: clean, in sync. */
function status(over: Partial<RepoStatus> = {}): RepoStatus {
  return {
    link: {
      workbench_id: "wb-1",
      full_name: "franzmueller/operator-test",
      name: "operator-test",
      owner: "franzmueller",
      default_branch: "main",
      private: false,
      clone_url: "https://github.com/franzmueller/operator-test.git",
      html_url: "https://github.com/franzmueller/operator-test",
      path: "operator-test",
      selected_at: "2026-08-20T09:00:00Z",
    },
    cloned: true,
    workspace: "data/ode/franzmueller",
    branch: "main",
    ahead: 0,
    behind: 0,
    diverged: false,
    detached: false,
    unborn: false,
    changes: [],
    dirty: false,
    fetched: true,
    scaffold: { present: ["main.py"], missing: [], complete: true },
    ...over,
  };
}

const mounted: Root[] = [];

beforeEach(() => {
  holdStatus = null;
  missingScopes = [];
  benches = [];
  calls = [];
  statusCalls = [];
  pushRefusals = 1;
  pushRejects = null;
  statusRejects = null;
  repositoriesRefuse = false;
  verification = {
    valid: false,
    code: 401,
    message: "Bad credentials",
    scopes: [],
    scopes_reported: false,
    kind: "ghu_ — a GitHub App's user access token: no scopes, expires in hours",
    length: 40,
    age: "19h4m2s",
  };
  reconnects = 0;
  reconnectFails = null;
  // The changes panel opens itself once per browser session, so the record of
  // having done so has to go before each test rather than leak into the next.
  window.sessionStorage.clear();
});

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
});

/**
 * benched mounts the same view inside the workbench provider, at `address`.
 *
 * Two workbenches are what puts the bar on screen at all — with one there is no
 * choice to present and it renders nothing — so the switch tests need their own
 * mount rather than `open()`.
 */
async function benched(address: string): Promise<HTMLElement> {
  benches = [workbench("wb-1", "franzmueller/operator-test"), workbench("wb-2", "franzmueller/operator-test-2")];
  window.history.replaceState({}, "", address);
  vi.resetModules();
  const { CodeView } = await import("./code");
  const { WorkbenchProvider } = await import("./workbench");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () =>
    root.render(
      <WorkbenchProvider>
        <CodeView session={SESSION} />
      </WorkbenchProvider>,
    ),
  );
  // Two turns: the workbench list and the connection, then the status, the tree and
  // the file the address names, each of which is read from the answer to the last.
  for (let turn = 0; turn < 3; turn++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
  return host;
}

/** A workbench as the list route reports it. */
function workbench(id: string, fullName: string): Workbench {
  const [owner = "", name = ""] = fullName.split("/");
  return {
    id,
    link: { ...status().link, workbench_id: id, full_name: fullName, name, owner, path: fullName },
    created_at: "2026-08-20T09:00:00Z",
    last_used_at: "2026-08-20T09:00:00Z",
  };
}

/** The file names the tree is listing, in the order it lists them. */
function listing(host: HTMLElement): string[] {
  return [...host.querySelectorAll(".file-tree button.tree-file .tree-name")].map((entry) =>
    (entry.textContent ?? "").trim(),
  );
}

/**
 * open mounts the code view against `current` and returns its root element.
 *
 * The session is a parameter because one thing on screen depends on the
 * deployment rather than on the working copy: whether the commit box offers to draft
 * a message, which needs an LLM provider the repo routes are served without.
 */
async function open(session: Session = SESSION): Promise<HTMLElement> {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
  const { CodeView } = await import("./code");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<CodeView session={session} />));
  // One more turn, for the connection and the status the view reads on mount.
  await act(async () => {
    await Promise.resolve();
  });
  return host;
}

/** The bar's button carrying `label`, whatever else is in it. */
function button(host: HTMLElement, label: string): HTMLButtonElement {
  const found = [...host.querySelectorAll("button")].find((candidate) =>
    (candidate.textContent ?? "").includes(label),
  );
  if (!found) throw new Error(`no button contains "${label}"`);
  return found;
}

// --- the wait for the status ---

/*
 * The gap between the connection and the checkout.
 *
 * The pane's own loading guard lets go as soon as the connection is in, and that
 * answer is free; the status behind it is the slow one. For as long as it took, the
 * pane was a bar over nothing — which reads as a workbench holding no repository,
 * not as one still being read.
 */
it("says the checkout is being read while the status route is still answering", async () => {
  let release = () => {};
  holdStatus = new Promise<void>((resolve) => {
    release = resolve;
  });

  window.history.replaceState({}, "", "/");
  vi.resetModules();
  const { CodeView } = await import("./code");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<CodeView session={SESSION} />));
  // The connection, which is not gated: enough to get past the guard and into the
  // state this test is about.
  await act(async () => {
    await Promise.resolve();
  });

  expect(host.querySelector(".busy")?.textContent).toContain("Reading the checkout…");

  release();
  for (let turn = 0; turn < 3; turn++) {
    await act(async () => {
      await Promise.resolve();
    });
  }

  expect(host.textContent).not.toContain("Reading the checkout…");
  expect(host.querySelector(".repo-bar")?.textContent).toContain("franzmueller/operator-test");
});

// --- what the bar says without being opened ---

it("carries the branch, the distance to the remote and the count of uncommitted files", async () => {
  current = status({
    ahead: 2,
    behind: 0,
    dirty: true,
    changes: [{ path: "op.py", kind: "modified", staged: false, unstaged: true }],
  });

  const bar = (await open()).querySelector(".repo-bar");
  expect(bar?.textContent).toContain("franzmueller/operator-test");
  expect(bar?.textContent).toContain("main");
  expect(bar?.textContent).toContain("2 ahead, 0 behind");
  expect(bar?.textContent).toContain("1 uncommitted");
});

it("says the remote was not fetched rather than showing a stale zero as agreement", async () => {
  current = status({ fetched: false });

  const bar = (await open()).querySelector(".repo-bar");
  expect(bar?.textContent).toContain("unfetched");
  expect(bar?.textContent).toContain("in sync");
});

/*
 * The other half of that: once the remote *has* been reached, the distance beside
 * it does not go back to being unfetched because the next status happened not to
 * fetch. It is the same measurement — the fetch moved the remote-tracking ref, and
 * every status after it reads the distance from that ref — so what changes is only
 * how old the reading is, and that is what the bar says.
 */
it("stops calling the distance unfetched once the remote has been reached", async () => {
  current = status();

  const bar = (await open()).querySelector(".repo-bar");
  expect(statusCalls).toContain(true);
  expect(bar?.textContent).not.toContain("unfetched");
  expect(bar?.textContent).toMatch(/fetched \d{1,2}:\d{2}/);
});

/*
 * The case this was reported from: push, whose own status read *does* fetch, and
 * which used to be followed by a second status read that did not — leaving the bar
 * saying "unfetched" about a remote it had contacted a moment earlier, under a
 * notice quoting what that remote had said.
 */
it("does not call the distance unfetched again after the push that just fetched it", async () => {
  pushRefusals = 0;
  current = status({ ahead: 1 });

  const host = await open();
  const before = statusCalls.length;
  await act(async () => button(host, "Push").click());

  expect(calls).toEqual(["repoPush"]);
  const bar = host.querySelector(".repo-bar");
  expect(bar?.textContent).toContain("main -> main");
  expect(bar?.textContent).not.toContain("unfetched");
  // One status read, and it fetched. The second, unfetched one is the bug.
  expect(statusCalls.slice(before)).toEqual([true]);
});

it("asks for a fetch when Fetch is pressed, and says it is working", async () => {
  current = status();

  const host = await open();
  const before = statusCalls.length;
  let held: (() => void) | null = null;
  holdStatus = new Promise<void>((resolve) => {
    held = resolve;
  });

  await act(async () => button(host, "Fetch").click());
  // The button is the only report a fetch has while it runs: nothing else on the
  // bar changes when the answer is that nothing changed.
  expect(button(host, "Fetching…")).toBeTruthy();

  holdStatus = null;
  await act(async () => {
    held?.();
    await Promise.resolve();
  });

  expect(statusCalls.slice(before)).toEqual([true]);
  expect(button(host, "Fetch")).toBeTruthy();
});

it("carries the commit the branch is on, with the subject on the hover", async () => {
  current = status({ head: "2057193e0dd8968f1981550931dc54fab66b74a3", head_subject: "first cut" });

  const sha = (await open()).querySelector(".repo-bar-sha");
  expect(sha?.textContent).toBe("2057193");
  expect(sha?.getAttribute("title")).toContain("2057193e0dd8968f1981550931dc54fab66b74a3");
  expect(sha?.getAttribute("title")).toContain("first cut");
});

/*
 * A clone of an empty repository has nothing to push, and the button says so rather
 * than letting git answer. The same button is the one the mismatch test below
 * checks, which is why both assert on `disabled` and not on the wording.
 */
it("disables push until the first commit", async () => {
  current = status({ unborn: true, branch: "main" });

  const host = await open();
  expect(button(host, "Push").disabled).toBe(true);
  expect(host.querySelector(".repo-bar")?.textContent).toContain("no commits yet");
});

// --- §5.11 item 6: uncommitted work, and the three answers to it ---

it("opens the changes panel by itself when the working copy is dirty on open", async () => {
  current = status({
    dirty: true,
    changes: [
      { path: "op.py", kind: "modified", staged: false, unstaged: true },
      { path: "tests/test_op.py", kind: "untracked", staged: false, unstaged: true },
    ],
  });

  const host = await open();
  const panel = host.querySelector("#repo-panel-changes");
  expect(panel).not.toBeNull();
  // The path split across two elements — the file, then the directory it sits in —
  // so the assertion is per part rather than on the joined text.
  const names = [...(panel?.querySelectorAll(".change-name") ?? [])].map((at) => at.textContent);
  expect(names).toEqual(["op.py", "test_op.py"]);
  expect(panel?.querySelectorAll(".change-dir")[1]?.textContent).toContain("tests");
  // The three answers, beside the work rather than somewhere else.
  for (const answer of ["Commit", "Stash", "Discard"]) {
    expect(panel?.textContent).toContain(answer);
  }
});

/*
 * What the row says about a change without being read.
 *
 * The list used to render the kind as a word directly in front of the path, which
 * ran the two together — `untrackedpvmodel.py` — so the one thing the list is for,
 * naming the files, was the thing it broke. The letter, its colour and the icon are
 * git's own vocabulary and VS Code's arrangement of it, which is what a developer
 * is already reading in the editor.
 */
it("marks each change with git's letter and a file icon rather than a word glued to the path", async () => {
  current = status({
    dirty: true,
    changes: [
      { path: "pkg/simulation/pvmodel.py", kind: "untracked", staged: false, unstaged: true },
      { path: "operator.yaml", kind: "modified", staged: true, unstaged: false },
      { path: "old.py", kind: "deleted", staged: true, unstaged: false },
      {
        path: "training.py",
        kind: "renamed",
        renamed_from: "train.py",
        staged: true,
        unstaged: false,
      },
    ],
  });

  const host = await open();
  const rows = [...host.querySelectorAll("#repo-panel-changes .change-row")];
  expect(rows).toHaveLength(4);

  // The kind is a letter and a class, and it is not in the name.
  expect(rows.map((row) => row.querySelector(".change-status")?.textContent)).toEqual([
    "U",
    "M",
    "D",
    "R",
  ]);
  expect(rows.map((row) => row.querySelector(".change-name")?.textContent)).toEqual([
    "pvmodel.py",
    "operator.yaml",
    "old.py",
    "training.py",
  ]);
  expect(rows[0]?.className).toContain("untracked");
  expect(rows[2]?.className).toContain("deleted");

  // Every row carries an icon, and the icon is hidden from the reading order: it is
  // a repetition of the letter beside it, not a second piece of information.
  for (const row of rows) {
    const icon = row.querySelector("svg.change-icon");
    expect(icon).not.toBeNull();
    expect(icon?.getAttribute("aria-hidden")).toBe("true");
  }
  // The colour families differ by file type, which is the property that makes the
  // list scannable at all.
  // classList rather than className: on an SVG element the latter is an
  // SVGAnimatedString, not a string.
  expect(rows[0]?.querySelector(".change-icon")?.classList.contains("icon-code")).toBe(true);
  expect(rows[1]?.querySelector(".change-icon")?.classList.contains("icon-config")).toBe(true);

  // And the word survives for anyone who does not read single letters.
  expect(rows[0]?.querySelector(".change-status")?.getAttribute("aria-label")).toBe("untracked");
  // A rename says what it was called.
  expect(rows[3]?.querySelector(".change-dir")?.textContent).toContain("train.py");
});

/*
 * A refusal whose answer is a step the developer takes.
 *
 * The repo routes have always sent a `hint` beside their `needs`, and the bar never
 * showed it — so "GitHub rejected the stored credential" arrived without the one
 * sentence that says what to do about it. The push below is the case that prompted
 * this: git reports a rejected credential and a missing one in the same words, and
 * only the backend knows which of the two happened.
 */
it("shows the repair the backend named, not only the refusal", async () => {
  current = status({ ahead: 1 });

  const host = await open();
  await act(async () => button(host, "Push").click());

  const notice = host.querySelector(".repo-bar-notice");
  expect(notice?.textContent).toContain("GitHub rejected the stored credential");
  expect(notice?.textContent).toContain("connect the account again");
  expect(notice?.className).toContain("error");
});

/*
 * The repair, in one click, without taking the tab.
 *
 * A credential that went stale mid-session is the case this exists for: the connect
 * card's flow is `window.location.assign`, which would take the pane and everything
 * typed into it, and the developer would then have to remember what they were doing
 * and press the button again. So the bar keeps the action and offers to finish it.
 */
/*
 * ODE's own error text can only ever be as specific as what ODE checked, and
 * "reconnect the account" is worth nothing to a developer who has just reconnected
 * it. So a refusal that blames the credential also carries what GitHub says about it,
 * including the token's kind — which is what tells a non-expiring OAuth token from a
 * GitHub App's user token that dies within hours.
 */
it("adds GitHub's own verdict to a refusal that blamed the credential", async () => {
  current = status({ ahead: 1 });

  const host = await open();
  await act(async () => button(host, "Push").click());
  // One more turn: the verdict is fetched beside the refusal rather than before it.
  await act(async () => {
    await Promise.resolve();
  });

  const verdictShown = host.querySelector(".repo-bar-verdict")?.textContent ?? "";
  expect(verdictShown).toContain("GitHub refuses this credential");
  expect(verdictShown).toContain("Bad credentials");
  expect(verdictShown).toContain("ghu_");
  // How long ODE has held it: the fact that says whether a reconnection happened.
  expect(verdictShown).toContain("19h4m2s");
});

it("says so when GitHub still accepts the credential, so nobody reconnects for nothing", async () => {
  current = status({ ahead: 1 });
  verification = {
    valid: true,
    code: 200,
    message: "",
    login: "franzmueller",
    scopes: ["repo", "workflow"],
    scopes_reported: true,
    kind: "gho_ — an OAuth app's user token: carries scopes, does not expire",
    length: 40,
    age: "42s",
  };

  const host = await open();
  await act(async () => button(host, "Push").click());
  await act(async () => {
    await Promise.resolve();
  });

  const verdictShown = host.querySelector(".repo-bar-verdict")?.textContent ?? "";
  expect(verdictShown).toContain("still accepts");
  expect(verdictShown).toContain("franzmueller");
  expect(verdictShown).toContain("repo, workflow");
});

it("reconnects and finishes the action that the stale credential refused", async () => {
  current = status({ ahead: 1 });

  const host = await open();
  await act(async () => button(host, "Push").click());

  // The offer names the action, because that is what pressing it will do.
  const offer = button(host, "Reconnect GitHub and push");
  expect(offer).toBeTruthy();

  await act(async () => offer.click());

  expect(reconnects).toBe(1);
  // Pushed again by itself: one click did the reconnection and the push.
  expect(calls).toEqual(["repoPush", "repoPush"]);
  expect(host.querySelector(".repo-bar-notice")?.textContent).toContain("main -> main");
  // And the offer is gone, so it cannot be pressed a second time for an action that
  // has already happened.
  expect(() => button(host, "Reconnect GitHub")).toThrow();
});

it("treats a closed GitHub window as a decision, and keeps the offer", async () => {
  current = status({ ahead: 1 });
  const { Abandoned } = await import("./github");
  reconnectFails = new Abandoned("the GitHub window was closed before it answered");

  const host = await open();
  await act(async () => button(host, "Push").click());
  await act(async () => button(host, "Reconnect GitHub and push").click());

  // Not an error: the developer closed it, and nothing is broken.
  const notice = host.querySelector(".repo-bar-notice");
  expect(notice?.textContent).toContain("closed before it answered");
  expect(notice?.className).not.toContain("error");
  // The push was not retried, and the offer stands.
  expect(calls).toEqual(["repoPush"]);
  expect(button(host, "Reconnect GitHub and push")).toBeTruthy();
});

it("offers nothing to reconnect for a refusal that is not about the credential", async () => {
  current = status({ ahead: 1 });
  // A rejected push: the remote has commits this branch does not. Reconnecting would
  // change nothing, and offering it would send the developer through a consent screen
  // for a problem it cannot touch.
  pushRejects = {
    status: 502,
    message: "git push failed (exit 1): ! [rejected] main -> main (non-fast-forward)",
    body: { git: "push", exit_code: 1 },
  };

  const host = await open();
  await act(async () => button(host, "Push").click());

  const notice = host.querySelector(".repo-bar-notice");
  expect(notice?.textContent).toContain("non-fast-forward");
  expect(notice?.className).toContain("error");
  expect(() => button(host, "Reconnect GitHub")).toThrow();
});

/*
 * The three places a lapsed credential shows up that are not the bar.
 *
 * The bar was fixed first, because that is where the report came from. That left the
 * pane in a state a screenshot made obvious: GitHub's raw 401 above a spinner that
 * never stopped, an account card offering only Disconnect, and nothing anywhere on
 * screen that could put a working credential back.
 */
const NO_REPOSITORY = {
  status: 409,
  message: "no repository is selected for this developer",
  body: { needs: "repository" },
};

it("offers the repair where the repository list refuses, and stops waiting", async () => {
  statusRejects = NO_REPOSITORY;
  repositoriesRefuse = true;

  const host = await open();

  // The refusal is on screen and the wait has ended. Both used to be true at once.
  expect(host.textContent).toContain("Bad credentials");
  expect(host.textContent).not.toContain("Reading your repositories…");

  const offer = button(host, "Reconnect GitHub");
  repositoriesRefuse = false;
  await act(async () => offer.click());

  expect(reconnects).toBe(1);
  // Asked for again by itself, rather than leaving the developer to reload the page.
  expect(calls.filter((call) => call === "repoRepositories")).toHaveLength(2);
});

it("offers the repair on the account card, beside disconnecting rather than instead of it", async () => {
  statusRejects = NO_REPOSITORY;

  const host = await open();
  const account = [...host.querySelectorAll(".pane")].find((pane) =>
    (pane.textContent ?? "").includes("GitHub account"),
  );
  expect(account).toBeTruthy();
  // Both. Replacing a credential is not a smaller version of throwing it away, and
  // Disconnect was the only thing on offer.
  expect(account?.textContent).toContain("Reconnect GitHub");
  expect(account?.textContent).toContain("Disconnect");
});

it("shows the lapsed card rather than a picker that cannot list anything", async () => {
  // Both of these are 409, and reading the status code before the `needs` handed the
  // developer a repository picker whose every call was going to be refused.
  statusRejects = {
    status: 409,
    message: "GitHub rejected the stored credential; reconnect the GitHub account",
    body: { needs: "github_connection", hint: "connect the account again" },
  };

  const host = await open();
  expect(host.textContent).toContain("stopped working");
  expect(host.textContent).toContain("franzmueller");
  expect(button(host, "Reconnect GitHub")).toBeTruthy();
  // And no picker: nothing on this screen can succeed until the credential is replaced.
  expect(calls).not.toContain("repoRepositories");
});

// --- the drafted commit message (§5.11 item 5) ---

/** A session that reports an LLM provider behind the repo surface. */
const DRAFTING_SESSION = {
  ...SESSION,
  repo: { scopes: ["repo", "workflow"], commit_message_draft: true },
} as Session;

it("fills the commit message from the draft, and commits nothing by doing so", async () => {
  current = status({
    dirty: true,
    changes: [{ path: "op.py", kind: "modified", staged: false, unstaged: true }],
  });

  const host = await open(DRAFTING_SESSION);
  await act(async () => button(host, "Draft").click());

  const field = host.querySelector<HTMLTextAreaElement>(".commit-message");
  expect(field?.value).toBe(draftAnswer.message);
  // The whole basis of §5.11 item 5: drafting asked for a message and nothing else
  // happened. The commit route was never called.
  expect(calls).toEqual(["repoCommitMessage"]);
  // And the bar says what it is, because a message the developer did not write is
  // one they have to read before it becomes history.
  expect(host.querySelector(".repo-bar-notice")?.textContent).toContain("suggestion");
});

it("says so when the diff was too large to send whole", async () => {
  current = status({
    dirty: true,
    changes: [{ path: "op.py", kind: "modified", staged: false, unstaged: true }],
  });
  draftAnswer = { ...draftAnswer, truncated: true, files: 40 };

  const host = await open(DRAFTING_SESSION);
  await act(async () => button(host, "Draft").click());

  // A body written from part of a diff is worth trusting less, and the developer is
  // the one who has to decide that.
  expect(host.querySelector(".repo-bar-notice")?.textContent).toContain("too large");
  draftAnswer = { ...draftAnswer, truncated: false, files: 1 };
});

it("leaves the button out where the deployment has no provider to draft with", async () => {
  current = status({
    dirty: true,
    changes: [{ path: "op.py", kind: "modified", staged: false, unstaged: true }],
  });

  // The default session reports no repo capability at all, which is the shape of a
  // deployment served without an LLM provider.
  const host = await open();
  expect(host.querySelector("#repo-panel-changes")).not.toBeNull();
  expect(() => button(host, "Draft")).toThrow();
});

it("leaves the panel closed on a clean working copy, and opens it on request", async () => {
  current = status();

  const host = await open();
  expect(host.querySelector("#repo-panel-changes")).toBeNull();

  await act(async () => button(host, "no changes").click());
  expect(host.querySelector("#repo-panel-changes")?.textContent).toContain(
    "matches the last commit",
  );
});

it("does not open the panel a second time in the same browser session", async () => {
  current = status({
    dirty: true,
    changes: [{ path: "op.py", kind: "modified", staged: false, unstaged: true }],
  });

  const first = await open();
  expect(first.querySelector("#repo-panel-changes")).not.toBeNull();

  const second = await open();
  expect(second.querySelector("#repo-panel-changes")).toBeNull();
  // Still surfaced, which is the part of item 6 that is unconditional.
  expect(second.querySelector(".repo-bar")?.textContent).toContain("1 uncommitted");
});

// --- a refusal has to say why ---

it("counts the warnings, refuses commit and push on a mismatched remote, and says so", async () => {
  current = status({
    remote_mismatch: true,
    remote: "git@github.com:someone-else/other.git",
    dirty: true,
    changes: [{ path: "op.py", kind: "modified", staged: false, unstaged: true }],
    scaffold: { present: [], missing: ["main.py", "op.py"], complete: false },
  });
  missingScopes = ["workflow"];

  const host = await open();
  expect(host.querySelector(".repo-bar")?.textContent).toContain("3 warnings");
  expect(button(host, "Push").disabled).toBe(true);
  // The changes panel opened itself, so commit is on screen and refused with it.
  expect(button(host, "Commit").disabled).toBe(true);

  // Stash and discard stay live: they do not leave the working copy. Asserted
  // before the warnings panel is opened, because one panel is open at a time and
  // opening that one closes this.
  expect(button(host, "Stash").disabled).toBe(false);
  expect(button(host, "Discard").disabled).toBe(false);

  await act(async () => button(host, "3 warnings").click());
  const warnings = host.querySelector("#repo-panel-warnings");
  expect(warnings?.textContent).toContain("someone-else/other.git");
  expect(warnings?.textContent).toContain("Missing from the operator template");
  expect(warnings?.textContent).toContain("workflow");
  expect(host.querySelector("#repo-panel-changes")).toBeNull();
});

// --- what the bar no longer shows, and where it went ---

it("keeps the checkout's path and the head reachable in the repository panel", async () => {
  current = status({ head: "2057193e0dd8968f1981550931dc54fab66b74a3", head_subject: "first cut" });

  const host = await open();
  await act(async () => button(host, "franzmueller/operator-test").click());
  const panel = host.querySelector("#repo-panel-repository");
  expect(panel?.textContent).toContain("data/ode/franzmueller/operator-test");
  expect(panel?.textContent).toContain("2057193");
  expect(panel?.textContent).toContain("first cut");
  expect(panel?.textContent).toContain("Switch repository");
});

it("closes the open panel on Escape", async () => {
  current = status();

  const host = await open();
  await act(async () => button(host, "franzmueller/operator-test").click());
  expect(host.querySelector("#repo-panel-repository")).not.toBeNull();

  const bar = host.querySelector(".repo-bar") as HTMLElement;
  await act(async () => {
    bar.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
  });
  expect(host.querySelector("#repo-panel-repository")).toBeNull();
});

// --- switching workbench takes the checkout with it ---

/*
 * The failure this covers, reported from the running SPA: the developer had solar.py
 * open in one workbench, clicked the tab of another, and the pane answered with
 * "404: no such path in the workspace" for a file they had not touched. Two things
 * were wrong at once — the address still named a file belonging to the checkout that
 * had been left, and the tree beside it still listed that checkout's files, so the
 * only way out was clicking a file that was not there either.
 */
it("closes the file and re-reads the tree when the workbench switches", async () => {
  current = status();

  const host = await benched("/?workbench=wb-1&file=solar.py");
  expect(listing(host)).toEqual(["main.py", "solar.py"]);
  expect(host.querySelector(".file-path")?.textContent).toBe("solar.py");

  const other = [...host.querySelectorAll<HTMLElement>("[data-slot='tabs-trigger']")].find(
    (candidate) => (candidate.textContent ?? "").includes("operator-test-2"),
  );
  // Not optional-chained: a selector that stops matching should fail here rather
  // than click nothing and leave the assertions below to report a stale tree.
  if (!other) throw new Error("no workbench tab for operator-test-2");
  await act(async () => other.click());
  for (let turn = 0; turn < 3; turn++) {
    await act(async () => {
      await Promise.resolve();
    });
  }

  // The other checkout's files, and only those.
  expect(listing(host)).toEqual(["main.py", "wind.py"]);
  // No open file, and therefore no refusal to explain: the editor invites a new one.
  expect(new URLSearchParams(window.location.search).get("file")).toBeNull();
  expect(host.textContent).not.toContain("does not exist");
  expect(host.querySelector(".file-editor")?.textContent).toContain("Pick a file");
});
