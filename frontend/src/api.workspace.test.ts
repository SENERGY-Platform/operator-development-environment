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

// jsdom because `api.ts` pulls in the keycloak module, which reads `window`.
// @vitest-environment jsdom

import { afterEach, expect, it, vi } from "vitest";
import { api, setActiveWorkbench } from "./api";

/**
 * The repository requests run one at a time, and that is a property rather than a
 * habit.
 *
 * Each of them executes a cell in the developer's pod and a kernel runs one cell at
 * a time, so the three the Code pane asks for on a reload — the status, the tree and
 * the open file — used to race each other for it and two of the three came back 409.
 * The backend now waits for an idle kernel instead of refusing the second caller;
 * this is the other half, which keeps the SPA from sending requests that would spend
 * that wait on ODE's own doing.
 *
 * Asserted at the fetch boundary, because the queue is in `api.ts` and invisible
 * above it: what the test watches is whether a second request is sent before the
 * first has answered.
 */

vi.mock("./keycloak", () => ({ token: async () => "test-token" }));

afterEach(() => {
  vi.unstubAllGlobals();
});

/** A fetch that never overlaps in the answer, and records whether it was asked to. */
function serialWatcher() {
  let inFlight = 0;
  const overlaps: string[] = [];
  const order: string[] = [];
  const release: Array<() => void> = [];

  const fetch = vi.fn((input: string) => {
    order.push(input);
    inFlight++;
    if (inFlight > 1) overlaps.push(input);
    return new Promise((resolve) => {
      release.push(() => {
        inFlight--;
        resolve({
          ok: true,
          status: 200,
          json: async () => ({}),
        } as Response);
      });
    });
  });

  return { fetch, overlaps, order, release };
}

it("sends the repository requests one after another rather than all at once", async () => {
  const watcher = serialWatcher();
  vi.stubGlobal("fetch", watcher.fetch);

  // Fired the way the Code pane fires them: together, without awaiting each other.
  const calls = [api.repoStatus(), api.repoFiles(), api.repoFile("op.py")];

  // One request is in flight and the other two are behind it. Released in turn,
  // which is also what proves the queue moves rather than merely holds.
  for (let released = 0; released < calls.length; released++) {
    await vi.waitFor(() => expect(watcher.release.length).toBe(released + 1));
    watcher.release[released]();
  }
  await Promise.all(calls);

  expect(watcher.overlaps).toEqual([]);
  expect(watcher.order).toHaveLength(3);
  // In the order they were asked for: the pane reads the tree it just wrote to, so
  // a queue that reordered would answer with the state before the write.
  expect(watcher.order[0]).toBe("/api/repo");
  expect(watcher.order[1]).toBe("/api/repo/files");
  expect(watcher.order[2]).toContain("/api/repo/files/content");
});

it("carries on after a request that failed", async () => {
  // The queue is a chain of attempts, not of successes. A 409 or a dropped
  // connection on one repository request must not leave every later one unsent —
  // which, on a chain that only continued on fulfilment, is exactly what would
  // happen, and the pane would sit on "Reading the working copy…" forever.
  const answers: Array<() => Promise<Response>> = [
    () => Promise.reject(new Error("the connection dropped")),
    () =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ tree: { name: "repo", path: "repo", type: "directory" } }),
      } as Response),
  ];
  vi.stubGlobal(
    "fetch",
    vi.fn(() => (answers.shift() ?? (() => Promise.reject(new Error("unexpected"))))()),
  );

  const failed = api.repoStatus();
  const behind = api.repoFiles();

  await expect(failed).rejects.toThrow("the connection dropped");
  await expect(behind).resolves.toMatchObject({ tree: { name: "repo" } });
});

/**
 * Two workbenches are two kernels, so their requests are two queues.
 *
 * One queue for all of them would put a file read in one operator behind a clone in
 * another, which is precisely what having several workbenches is meant to avoid —
 * and it would do it invisibly, because the backend would see a request that never
 * arrived rather than one it refused.
 */
it("queues each workbench separately", async () => {
  const watcher = serialWatcher();
  vi.stubGlobal("fetch", watcher.fetch);

  setActiveWorkbench("wb-forecast");
  const training = api.repoStatus();
  setActiveWorkbench("wb-anomalies");
  const other = api.repoFiles();

  // Both in flight at once: the second did not wait for the first. The watcher
  // records an overlap rather than refusing one, so this is asserted on what was
  // actually sent.
  await vi.waitFor(() => expect(watcher.order).toHaveLength(2));
  watcher.release[0]();
  watcher.release[1]();
  await Promise.all([training, other]);

  // And each names its own workbench, which is what makes them land in the right
  // checkout at the other end.
  expect(watcher.order[0]).toContain("workbench=wb-forecast");
  expect(watcher.order[1]).toContain("workbench=wb-anomalies");
  setActiveWorkbench(null);
});

it("names no workbench when the developer has only one", async () => {
  // The compatibility case, and the ordinary one: a developer with a single
  // workbench never sees the concept, and the backend reads an unnamed request as
  // "my only one".
  const watcher = serialWatcher();
  vi.stubGlobal("fetch", watcher.fetch);

  setActiveWorkbench(null);
  const call = api.repoStatus();
  await vi.waitFor(() => expect(watcher.release.length).toBe(1));
  watcher.release[0]();
  await call;

  expect(watcher.order[0]).toBe("/api/repo");
});
