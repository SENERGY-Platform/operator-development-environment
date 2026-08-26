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
import { bytes, num, percent, period, seconds, shortId } from "./ui";

/**
 * The formatters, at the boundaries they branch on.
 *
 * These render the numbers a developer decides from — whether a series is worth
 * reading, whether a period is the daily cycle they expected — so the unit is part
 * of the answer, not decoration. Each test below sits on a branch of the
 * implementation rather than on a round number in the middle of one.
 */

// --- seconds ---

it("a duration is written in the largest unit that keeps it readable", () => {
  expect(seconds(0)).toBe("0 s");
  expect(seconds(0.5)).toBe("500 ms");
  expect(seconds(1)).toBe("1 s");
  expect(seconds(120)).toBe("2 min");
  expect(seconds(7200)).toBe("2 h");
  expect(seconds(259200)).toBe("3 d");
});

// Each of these is one step below the threshold and one step on it. A shifted
// comparison shows up as "1440 s" or "0.02 min" rather than as a wrong number,
// which is the kind of thing that reads as fine until someone compares two rows.
it("each unit changes exactly at its threshold", () => {
  expect(seconds(0.999)).toBe("999 ms");
  expect(seconds(1)).toBe("1 s");
  expect(seconds(89)).toBe("89 s");
  expect(seconds(90)).toBe("1.5 min");
  expect(seconds(5399)).toBe("90 min");
  expect(seconds(5400)).toBe("1.5 h");
  expect(seconds(172799)).toBe("48 h");
  expect(seconds(172800)).toBe("2 d");
});

// Two digits below ten and none above: a sub-second sampling interval needs the
// precision, a gap of twelve seconds does not.
it("precision follows magnitude within the seconds range", () => {
  expect(seconds(9.876)).toBe("9.88 s");
  expect(seconds(12.4)).toBe("12 s");
});

// A clock offset is the case that goes negative. The unit must follow the
// magnitude while the sign survives, or a lag of two days reads as "-172800 s".
it("a negative duration keeps its sign and picks its unit by magnitude", () => {
  expect(seconds(-0.25)).toBe("-250 ms");
  expect(seconds(-3600)).toBe("-60 min");
});

// --- period ---

it("a detected cycle within a tenth of a named one is reported by name", () => {
  expect(period(86400)).toBe("daily (24 h)");
  expect(period(604800)).toBe("weekly (7 d)");
  expect(period(3600)).toBe("hourly (60 min)");
  expect(period(43200)).toBe("half-daily (12 h)");
});

// A cycle detected off the named one is still that cycle, and the measured length
// is kept in the bracket rather than rounded away to the name.
it("a named cycle keeps the length that was actually measured", () => {
  expect(period(90000)).toBe("daily (25 h)");
  expect(period(3400)).toBe("hourly (56.7 min)");
});

// Just outside the tolerance. Naming it "daily" here would assert a cycle the
// profiler did not detect — SPEC §5.4.13 item 6 is about reporting the name when
// it holds, not about rounding towards one.
it("a cycle outside the tolerance is reported as a duration rather than named", () => {
  expect(period(77760)).toBe("21.6 h");
  expect(period(7200)).toBe("2 h");
});

// --- num ---

it("a number of unknown magnitude keeps three significant figures", () => {
  expect(num(0)).toBe("0");
  expect(num(1)).toBe("1");
  expect(num(0.5)).toBe("0.5");
  expect(num(999.5)).toBe("999.5");
});

// The two ends where the fixed notation stops carrying information.
it("a number leaves fixed notation at a thousand and at a hundredth", () => {
  expect(num(1000)).toBe("1.00e+3");
  expect(num(0.01)).toBe("0.01");
  expect(num(0.009)).toBe("9.00e-3");
  expect(num(-1234)).toBe("-1.23e+3");
});

// A non-finite value is a non-result reaching a formatter. An em dash says so;
// "NaN" in a table cell reads as a broken pane.
it("a value that is not a number is written as an absence rather than as NaN", () => {
  expect(num(Number.NaN)).toBe("—");
  expect(num(Number.POSITIVE_INFINITY)).toBe("—");
});

// --- percent ---

it("a ratio is written as a percentage with one decimal", () => {
  expect(percent(0)).toBe("0%");
  expect(percent(1)).toBe("100%");
  expect(percent(0.1234)).toBe("12.3%");
  expect(percent(0.005)).toBe("0.5%");
});

// --- bytes ---

it("a size is scaled to the largest binary unit below it", () => {
  expect(bytes(0)).toBe("0 B");
  expect(bytes(1023)).toBe("1023 B");
  expect(bytes(1024)).toBe("1 KiB");
  expect(bytes(1536)).toBe("1.5 KiB");
  expect(bytes(1024 ** 3)).toBe("1 GiB");
});

// The scale stops at TiB, so a larger value has to keep counting in TiB rather
// than run off the end of the unit list into `undefined`.
it("a size beyond the largest unit stays in that unit", () => {
  expect(bytes(1024 ** 4)).toBe("1 TiB");
  expect(bytes(1024 ** 5)).toBe("1024 TiB");
});

// --- shortId ---

it("a platform URN is shortened to its last segment", () => {
  expect(shortId("urn:infai:ses:device:1a2b3c")).toBe("1a2b3c");
});

// Not everything that reaches a table cell is a URN — a kernel id is a bare
// string, and cutting it to nothing would leave the cell empty.
it("an identifier that is not a URN is left as it is", () => {
  expect(shortId("kernel-1")).toBe("kernel-1");
  expect(shortId("")).toBe("");
});
