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

import { expect, it } from "vitest";
import { syncSpin } from "./components/ui/spinner";

/*
 * Phase, not motion.
 *
 * Whether the thing turns is CSS's business and jsdom has no opinion on it — it
 * has no getAnimations at all. What is testable is the part that was wrong: every
 * animation the node reports is pinned to the same origin, so two spinners that
 * mounted seconds apart are at the same angle, and a host without the API is left
 * alone rather than taking the pane down with it.
 */

it("pins every animation on the node to the timeline origin", () => {
  const animations = [{ startTime: 4321 }, { startTime: null }];
  const node = { getAnimations: () => animations } as unknown as Element;

  syncSpin(node);

  expect(animations.map((entry) => entry.startTime)).toEqual([0, 0]);
});

it("leaves a host without the animations API alone", () => {
  expect(() => syncSpin({} as Element)).not.toThrow();
  expect(() => syncSpin(null)).not.toThrow();
});
