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

import { afterEach, expect, it, vi } from "vitest";
import { api } from "./api";

vi.mock("./keycloak", () => ({ token: async () => "test-token" }));

afterEach(() => {
  vi.unstubAllGlobals();
});

it("sends null as literal JSON null when clearing a split", async () => {
  let sentBody: string | undefined;
  const fetch = vi.fn((_input: string, init?: RequestInit) => {
    sentBody = init?.body as string | undefined;
    return Promise.resolve({
      ok: true,
      status: 200,
      json: async () => ({
        id: "session-1",
        data_split: null,
      }),
    } as Response);
  });

  vi.stubGlobal("fetch", fetch);

  await api.setSplit("session-1", null);

  expect(sentBody).toBe("null");
  expect(fetch).toHaveBeenCalledWith(
    expect.stringContaining("/chat/sessions/session-1/split"),
    expect.objectContaining({ method: "PUT" })
  );
});

it("sends a DataSplit with training_end and test_end", async () => {
  let sentBody: string | undefined;
  const fetch = vi.fn((_input: string, init?: RequestInit) => {
    sentBody = init?.body as string | undefined;
    return Promise.resolve({
      ok: true,
      status: 200,
      json: async () => ({
        id: "session-1",
        data_split: {
          training_end: "2026-06-01T00:00:00Z",
          test_end: "2026-06-02T00:00:00Z",
        },
      }),
    } as Response);
  });

  vi.stubGlobal("fetch", fetch);

  const split = {
    training_end: "2026-06-01T00:00:00Z",
    test_end: "2026-06-02T00:00:00Z",
  };
  await api.setSplit("session-1", split);

  expect(sentBody).toContain("training_end");
  expect(sentBody).toContain("test_end");
  expect(JSON.parse(sentBody!)).toEqual(split);
});
