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
import type { RepoStatus, Session, Workbench } from "./api";

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
/** Scopes the connected GitHub grant is missing, which is a warning of its own. */
let missingScopes: string[] = [];
/** The workbenches the list route answers with. Empty for the tests without a bar. */
let benches: Workbench[] = [];
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
      repoConnection: async () => ({
        connected: true,
        scopes_requested: ["repo", "workflow"],
        identity: {
          login: "franzmueller",
          connected_at: "2026-08-20T09:00:00Z",
          scopes: ["repo"],
          missing_scopes: missingScopes,
        },
      }),
      repoStatus: async () => current,
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
  missingScopes = [];
  benches = [];
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

/** open mounts the code view against `current` and returns its root element. */
async function open(): Promise<HTMLElement> {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
  const { CodeView } = await import("./code");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<CodeView session={SESSION} />));
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
  expect(panel?.textContent).toContain("op.py");
  expect(panel?.textContent).toContain("tests/test_op.py");
  // The three answers, beside the work rather than somewhere else.
  for (const answer of ["Commit", "Stash", "Discard"]) {
    expect(panel?.textContent).toContain(answer);
  }
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
