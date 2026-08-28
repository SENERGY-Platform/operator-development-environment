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

import { useCallback, useEffect, useState } from "react";

/**
 * Asking for the developer's attention when a chat turn has ended.
 *
 * A turn runs detached and can take minutes, so the developer is expected to go
 * and do something else — that is the whole point of the design in
 * `docs/chat-and-streaming.md`. The cost of that is a tab nobody is watching when
 * the answer arrives.
 *
 * Three signals, deliberately unequal:
 *
 *   - **The title blinks.** Free, needs no permission, works everywhere, and it
 *     is what changes the label on the Windows taskbar button. Always on.
 *   - **A desktop notification.** This is the only one that makes the taskbar
 *     button *flash* — a browser raises a real OS notification and the window
 *     manager takes it from there. Needs a permission, hence the toggle.
 *   - **A short tone.** Two sine blips, no asset, no autoplay problem: it only
 *     ever plays after the developer has clicked something on this page.
 *
 * Nothing here ever runs while the window is in front of the developer. A dialog
 * would; that is why there is no dialog.
 */

/** Where the toggle is kept. Namespaced, because the origin is shared. */
const KEY = "ode.attention";

/** Prefixed to the flashing title, so the taskbar button reads as changed at a glance. */
const MARK = "●";

/** How long each half of the blink lasts. */
const FLASH_MS = 1200;

/** What the browser will say about desktop notifications, plus the case where it has none. */
export type Notifications = NotificationPermission | "unsupported";

/**
 * Whether the developer has switched the alert on.
 *
 * localStorage throws rather than returning null in real cases — a browser
 * configured to block site data, Safari's private mode historically — and being
 * unable to read a preference is not worth an exception on the first paint of the
 * header, so a failure reads as "off".
 */
export function stored(): boolean {
  try {
    return localStorage.getItem(KEY) === "on";
  } catch {
    return false;
  }
}

/** Records the choice. A storage failure leaves it applying to this tab only. */
export function store(on: boolean): void {
  try {
    localStorage.setItem(KEY, on ? "on" : "off");
  } catch {
    // The toggle still took effect; it just will not survive a reload.
  }
}

/**
 * What the browser currently allows.
 *
 * Guarded rather than assumed: `Notification` is missing under jsdom, and absent
 * on any page served over plain HTTP that is not localhost. The header renders
 * this control on the first paint, and an exception there takes the whole
 * application down rather than just one button.
 */
export function notifications(): Notifications {
  if (typeof Notification === "undefined") return "unsupported";
  return Notification.permission;
}

/**
 * Asks for the notification permission.
 *
 * Only ever called from a click. Chrome and Firefox both ignore — or hold against
 * the origin — a request that arrives without user activation, and a prompt the
 * developer did not ask for is the thing this design is trying to avoid in the
 * first place.
 */
export async function request(): Promise<Notifications> {
  if (typeof Notification === "undefined") return "unsupported";
  try {
    return await Notification.requestPermission();
  } catch {
    // Older signatures took a callback and returned undefined. Reading the
    // property afterwards is right in either case.
    return Notification.permission;
  }
}

/**
 * Whether the developer is somewhere else.
 *
 * Both questions, because they are different ones: a tab hidden behind another
 * tab is `hidden`, whereas an ODE window sitting fully visible on a second
 * monitor while the developer types in their editor is `visible` and unfocused.
 * The second case is the one this feature exists for.
 */
function away(): boolean {
  if (document.visibilityState === "hidden") return true;
  return typeof document.hasFocus === "function" ? !document.hasFocus() : false;
}

let timer: number | null = null;
/** The title to put back. Non-null exactly while the flash is running. */
let restore: string | null = null;

/** Puts the title back and unhooks. Idempotent, and safe to call when idle. */
export function stopFlash(): void {
  if (timer !== null) {
    clearInterval(timer);
    timer = null;
  }
  if (restore !== null) {
    document.title = restore;
    restore = null;
  }
  window.removeEventListener("focus", onReturn);
  document.removeEventListener("visibilitychange", onReturn);
}

function onReturn(): void {
  // A visibilitychange also fires on the way out. Only coming back ends the blink.
  if (!away()) stopFlash();
}

function startFlash(headline: string): void {
  const original = restore ?? document.title;
  stopFlash();
  restore = original;

  let marked = false;
  const tick = () => {
    marked = !marked;
    document.title = marked ? `${MARK} ${headline}` : original;
  };
  tick();
  timer = window.setInterval(tick, FLASH_MS);

  window.addEventListener("focus", onReturn);
  document.addEventListener("visibilitychange", onReturn);
}

/**
 * The last notification raised, so it can be closed before the next one.
 *
 * Reusing a `tag` would be the idiomatic way to keep them from stacking, but a
 * replacement is silent by default — the second turn of the evening would update
 * a notification nobody is looking at and flash nothing. Closing and raising
 * afresh alerts every time, which is the point.
 */
let last: Notification | null = null;

function toast(headline: string, body: string): void {
  if (notifications() !== "granted") return;
  try {
    last?.close();
    const note = new Notification(headline, { body });
    note.onclick = () => {
      // Bring ODE forward. Allowed here because the click is a user gesture, and
      // it is what the developer meant by clicking a notification about a tab.
      window.focus();
      note.close();
    };
    last = note;
  } catch {
    // Construction throws on Android Chrome, where notifications belong to a
    // service worker. The title is still blinking; that is enough.
  }
}

type AudioContextConstructor = typeof AudioContext;

let audio: AudioContext | null = null;

/**
 * Two short blips, quiet enough to sit next to somebody.
 *
 * Synthesised rather than shipped as a file: an asset would be a request, a
 * build-time decision about format, and a licence question, all for 200ms of
 * sine wave.
 */
function beep(): void {
  const Ctor: AudioContextConstructor | undefined =
    window.AudioContext ??
    (window as { webkitAudioContext?: AudioContextConstructor }).webkitAudioContext;
  if (Ctor === undefined) return;

  try {
    audio ??= new Ctor();
    // Suspended is the normal state for a context created before the first
    // gesture, and resuming is a no-op once it is running.
    void audio.resume();

    const at = audio.currentTime;
    for (const [index, hz] of [660, 880].entries()) {
      const start = at + index * 0.14;
      const oscillator = audio.createOscillator();
      const gain = audio.createGain();
      oscillator.type = "sine";
      oscillator.frequency.value = hz;
      // A ramp rather than a switch: an abrupt start and stop on a sine is a
      // click, which is louder than the note it is meant to bracket.
      gain.gain.setValueAtTime(0.0001, start);
      gain.gain.exponentialRampToValueAtTime(0.05, start + 0.015);
      gain.gain.exponentialRampToValueAtTime(0.0001, start + 0.12);
      oscillator.connect(gain).connect(audio.destination);
      oscillator.start(start);
      oscillator.stop(start + 0.13);
    }
  } catch {
    // No audio output, or a context the browser refused to create. Silent is an
    // acceptable outcome for a sound effect.
  }
}

/**
 * Announces that something the developer was waiting for has ended.
 *
 * Does nothing at all while the window is in front of them: they are already
 * looking at the answer, and a blinking title in a tab you are reading is the
 * ugly version of this feature.
 */
export function announce(headline: string, body: string): void {
  if (!away()) return;
  startFlash(headline);
  if (!stored()) return;
  toast(headline, body);
  beep();
}

/**
 * useAttention is the header control's state.
 *
 * The permission is held in state rather than read on each render, because
 * `Notification.permission` changing is not something React can observe — the
 * prompt resolves in a promise, and the answer has to be written back by hand.
 */
export function useAttention(): {
  enabled: boolean;
  permission: Notifications;
  /** Switches the alert on, asking for the permission the first time. */
  toggle: () => void;
} {
  const [enabled, setEnabled] = useState(stored);
  const [permission, setPermission] = useState<Notifications>(notifications);

  // Switching it off in one tab should not leave another tab beeping. `storage`
  // fires in the *other* tabs, which is exactly the set that needs telling.
  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key === KEY || event.key === null) setEnabled(stored());
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  const toggle = useCallback(() => {
    if (enabled) {
      store(false);
      setEnabled(false);
      stopFlash();
      return;
    }
    // Switched on either way. A blocked permission costs the desktop
    // notification — and with it the taskbar flash — but the title and the tone
    // still work, and refusing to switch on would leave the developer with a
    // dead button and no way to say what they wanted.
    store(true);
    setEnabled(true);
    if (notifications() === "default") void request().then(setPermission);
  }, [enabled]);

  return { enabled, permission, toggle };
}
