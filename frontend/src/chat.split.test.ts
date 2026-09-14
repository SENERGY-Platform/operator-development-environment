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
//
// jsdom because importing the chat module reaches the router, which reads
// window.location at import time.

// The zone is set before anything imports a module that builds a Date, because
// this file exists to prove that the split's bounds do not depend on it. A
// runner that happens to sit in UTC would pass either implementation.
process.env.TZ = "America/New_York";

import { expect, it } from "vitest";
import { localInputFromUTC, utcFromLocalInput } from "./chat";

/**
 * The bound a developer types is the bound that is sent.
 *
 * `new Date("2026-06-01T00:00")` reads a zoneless value as *local* time, so in
 * New York the same keystrokes used to become 04:00 UTC and in Berlin 22:00 the
 * day before — four hours of test data trained on, or two hours of training data
 * withheld, with nothing to notice it by: the shifted pair still validates, and
 * both ends move together so the window keeps its length.
 */
it("reads a datetime-local value as UTC rather than as the browser's zone", () => {
  expect(utcFromLocalInput("2026-06-01T00:00")).toBe("2026-06-01T00:00:00.000Z");
  expect(utcFromLocalInput("2026-06-08T13:45")).toBe("2026-06-08T13:45:00.000Z");
  // What the old implementation produced here, kept as the thing this must not be.
  expect(utcFromLocalInput("2026-06-01T00:00")).not.toBe(
    new Date("2026-06-01T00:00").toISOString(),
  );
});

it("accepts the seconds some browsers add and defaults them to zero", () => {
  expect(utcFromLocalInput("2026-06-01T00:00:30")).toBe("2026-06-01T00:00:30.000Z");
});

it("refuses a value that is not a full date and time", () => {
  for (const value of ["", "2026-06-01", "not a time", "2026-06-01T00"]) {
    expect(utcFromLocalInput(value)).toBeNull();
  }
});

/**
 * The inverse, which is what pre-fills the inputs from the current split: a
 * developer changing one bound must see the other one unchanged, in the same
 * zone the control reads and writes.
 */
it("round-trips an instant back into the input it came from", () => {
  const iso = "2026-06-01T00:00:00.000Z";
  expect(localInputFromUTC(iso)).toBe("2026-06-01T00:00");
  expect(utcFromLocalInput(localInputFromUTC(iso))).toBe(iso);
});
