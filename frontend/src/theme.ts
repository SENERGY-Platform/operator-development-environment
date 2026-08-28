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

import { useEffect, useState } from "react";

/**
 * The light/dark choice.
 *
 * Three states, not two, and the third one is the default. `null` means "no
 * choice made", which is not the same as "light": it means the operating
 * system's setting decides, and keeps deciding — if the developer's desktop
 * switches at sunset, so does this. An explicit "light" is a different thing,
 * and has to beat a dark desktop.
 *
 * That distinction is why the attribute is *removed* rather than set to "light"
 * when the choice is cleared. `index.css` reads it in three scopes: the bare
 * `:root` is light, a `prefers-color-scheme: dark` block guarded by
 * `:not([data-theme="light"])` is the operating system's say, and
 * `[data-theme="dark"]` is this choice. Writing `data-theme="light"` for the
 * default would silence the media query for everyone and turn "follow the
 * system" into "always light".
 */
export type Choice = "light" | "dark" | null;

/** Where the choice is kept. Namespaced, because the origin is shared with the SPA's other keys. */
const KEY = "ode.theme";

const QUERY = "(prefers-color-scheme: dark)";

/**
 * The stored choice.
 *
 * localStorage throws rather than returning null in a few real cases — Safari's
 * private mode historically, and any browser configured to block site data — and
 * a theme preference is not worth a blank screen, so a failure reads as "no
 * choice".
 */
export function stored(): Choice {
  try {
    const value = localStorage.getItem(KEY);
    return value === "light" || value === "dark" ? value : null;
  } catch {
    return null;
  }
}

/**
 * What the operating system asks for.
 *
 * `matchMedia` is guarded rather than assumed. Every browser has it, but this
 * module is also loaded under jsdom, where it does not exist — and a theme
 * preference is not a good reason for the header to throw and take the whole
 * application down with it. Absent, the answer is the stylesheet's own default.
 */
function system(): "light" | "dark" {
  if (typeof window.matchMedia !== "function") return "light";
  return window.matchMedia(QUERY).matches ? "dark" : "light";
}

/** The theme actually on screen: the choice if there is one, the system's otherwise. */
export function resolved(choice: Choice = stored()): "light" | "dark" {
  return choice ?? system();
}

/**
 * Writes the choice to the document and to storage.
 *
 * The attribute goes on the root element, which is what the stylesheet's third
 * scope selects. Removing it — rather than setting it to "light" — is what hands
 * the decision back to the media query.
 */
export function apply(choice: Choice): void {
  const root = document.documentElement;
  if (choice === null) root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);
  try {
    if (choice === null) localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, choice);
  } catch {
    // The theme still applied; it just will not survive a reload. Not worth
    // reporting to a developer who came here to read a profile.
  }
}

/**
 * Applies the stored choice before the first render.
 *
 * Called from `main.tsx` at module scope rather than from an effect: an effect
 * runs after the first paint, so a developer who chose dark would see one frame
 * of light on every load.
 */
export function initTheme(): void {
  const choice = stored();
  if (choice !== null) apply(choice);
}

/**
 * useTheme is the control's state.
 *
 * It tracks the system preference as well as the choice, because with no choice
 * stored the button has to say which theme is actually showing — and has to
 * change when the desktop does. The listener is only interesting in that case,
 * but it is cheap and unconditional, which is simpler than adding and removing
 * it as the choice changes.
 */
export function useTheme(): {
  choice: Choice;
  active: "light" | "dark";
  /** Switches to the other theme, and records that as a deliberate choice. */
  toggle: () => void;
  /** Hands the decision back to the operating system. */
  clear: () => void;
  /** Records a choice, or `null` to follow the operating system. */
  set: (choice: Choice) => void;
} {
  const [choice, setChoice] = useState<Choice>(stored);
  const [preference, setPreference] = useState<"light" | "dark">(system);

  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const media = window.matchMedia(QUERY);
    const onChange = (event: MediaQueryListEvent) => {
      setPreference(event.matches ? "dark" : "light");
    };
    media.addEventListener("change", onChange);
    return () => media.removeEventListener("change", onChange);
  }, []);

  const active = choice ?? preference;

  return {
    choice,
    active,
    toggle: () => {
      const next: Choice = active === "dark" ? "light" : "dark";
      apply(next);
      setChoice(next);
    },
    clear: () => {
      apply(null);
      setChoice(null);
    },
    set: (next: Choice) => {
      apply(next);
      setChoice(next);
    },
  };
}
