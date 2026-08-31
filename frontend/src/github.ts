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

import { api } from "./api";

/**
 * Reconnecting GitHub without losing the pane.
 *
 * A stored credential goes stale while a developer is working — revoked on GitHub,
 * or an authorisation that expired — and the first they hear of it is a push that
 * refuses. The repair is another trip through GitHub's OAuth flow, and the way the
 * connect card does it, `window.location.assign`, takes the whole tab: the commit
 * message they had just written, the panel that was open, the pane's state. For the
 * *first* connection that costs nothing, because there is no working copy yet. For a
 * reconnection in the middle of a push it costs exactly the thing they were doing.
 *
 * So this runs the same flow in a popup, and the tab stays where it is.
 *
 * What it cannot do is run without being seen, and that is GitHub's decision rather
 * than a shortcut not taken:
 *
 *   - **A hidden iframe is refused.** github.com/login/oauth/authorize answers with
 *     `X-Frame-Options: deny`, so there is no frame to hide.
 *   - **A revoked grant means a consent screen.** GitHub asks again when the
 *     authorisation it had is gone, and asking is the whole point of it.
 *
 * What it does do is make the common case invisible anyway: when the authorisation
 * still exists and only the token has expired, GitHub redirects straight back and
 * the popup opens and closes faster than it can be read.
 *
 * The popup needs a user gesture. Browsers grant one to a click and take it away
 * again across an `await`, so this must be called from a handler and not from the
 * catch block of a request that has already failed — which is why the pane offers a
 * button rather than reconnecting by itself.
 */

/** The message a callback popup sends its opener. */
export const CALLBACK_MESSAGE = "ode.github.callback";

/** Where the state is kept between the authorize call and the answer coming back. */
const STATE_KEY = "ode.github.state";

interface CallbackMessage {
  ode: typeof CALLBACK_MESSAGE;
  code: string;
  state: string;
  error: string;
}

/**
 * relayAuthorisation is the popup's whole job: hand the answer to the tab that
 * opened it, and close.
 *
 * Called from `main.tsx` before anything else, and that placement is the point. The
 * popup does not need the application, a theme or a platform session — it needs to
 * pass on two query parameters — and booting the shell to do it would put a Keycloak
 * round trip inside a window that exists for a quarter of a second. It also keeps
 * the authorisation code in one place: a code is single-use, and the tab holding the
 * developer's work is the one that has to know whether spending it worked.
 *
 * Returns whether it handled the load, which is when the caller must not start the
 * application.
 */
export function relayAuthorisation(): boolean {
  const opener = window.opener as Window | null;
  if (!opener || opener === window) return false;
  if (!window.location.pathname.endsWith("/github/callback")) return false;

  const query = new URLSearchParams(window.location.search);
  const code = query.get("code") ?? "";
  const state = query.get("state") ?? "";
  const error = query.get("error_description") ?? query.get("error") ?? "";
  if (!code && !error) return false;

  const message: CallbackMessage = { ode: CALLBACK_MESSAGE, code, state, error };
  try {
    opener.postMessage(message, window.location.origin);
  } catch {
    // An opener that has since navigated away or closed. Nothing to relay to, and
    // the developer is looking at this window: let the application load and finish
    // the flow here, which is the old behaviour.
    return false;
  }
  // Some browsers refuse to close a window script did not open in that same tab's
  // history. Saying so beats a blank page.
  document.body.textContent = "GitHub is connected. You can close this window.";
  window.close();
  return true;
}

/** A developer who closed the popup, or GitHub refusing. Not a fault to report as one. */
export class Abandoned extends Error {
  constructor(message: string) {
    super(message);
    this.name = "Abandoned";
  }
}

/** How long the popup may stay open before this stops waiting for it. */
const PATIENCE_MS = 5 * 60 * 1000;

/**
 * reconnect runs the OAuth flow in a popup and stores the credential it comes back
 * with. Resolves when ODE holds a working GitHub token again.
 *
 * Falls back to taking the tab when the popup is blocked: a blocked popup is a
 * browser setting, and refusing to reconnect over it would leave the developer with
 * no way through at all. The flow then completes the way the connect card's does,
 * and the pane state is the price.
 */
export async function reconnect(): Promise<void> {
  const authorize = await api.repoAuthorize();
  try {
    sessionStorage.setItem(STATE_KEY, authorize.state);
  } catch {
    // Private mode with storage off. The backend checks the state as well, which is
    // the check that matters; this copy only lets the answer be refused earlier.
  }

  const popup = window.open(
    authorize.url,
    "ode-github-authorisation",
    "popup,width=680,height=820",
  );
  if (!popup) {
    window.location.assign(authorize.url);
    // The page is leaving. Never resolving is correct: nothing after this call gets
    // to run, and a resolution would let a caller report success at a moment when
    // nothing has happened yet.
    return new Promise<never>(() => {});
  }

  const answer = await waitFor(popup);
  if (answer.error) {
    throw new Abandoned(`GitHub refused the authorisation: ${answer.error}`);
  }
  const expected = readState();
  if (expected !== null && answer.state !== expected) {
    // Refused here rather than sent on: the backend refuses a mismatched state too,
    // and spending a round trip to be told so is a round trip.
    throw new Error("the authorisation came back with a state ODE did not ask for");
  }
  await api.repoConnect(answer.code, answer.state);
}

function readState(): string | null {
  try {
    const state = sessionStorage.getItem(STATE_KEY);
    if (state !== null) sessionStorage.removeItem(STATE_KEY);
    return state;
  } catch {
    return null;
  }
}

/**
 * waitFor resolves with what the popup relayed, or rejects when the developer closed
 * it.
 *
 * The closed check is polled and given a moment's grace, because the popup posts its
 * message and closes itself immediately afterwards — a closed window is the normal
 * end of a *successful* flow, and reading it as abandonment would fail every
 * reconnection that worked.
 */
function waitFor(popup: Window): Promise<CallbackMessage> {
  return new Promise<CallbackMessage>((resolve, reject) => {
    let settled = false;
    let closedAt: number | null = null;

    const finish = (run: () => void) => {
      if (settled) return;
      settled = true;
      window.removeEventListener("message", onMessage);
      window.clearInterval(watch);
      window.clearTimeout(patience);
      run();
    };

    const onMessage = (event: MessageEvent) => {
      if (event.origin !== window.location.origin) return;
      const data = event.data as Partial<CallbackMessage> | null;
      if (!data || data.ode !== CALLBACK_MESSAGE) return;
      finish(() =>
        resolve({
          ode: CALLBACK_MESSAGE,
          code: data.code ?? "",
          state: data.state ?? "",
          error: data.error ?? "",
        }),
      );
    };
    window.addEventListener("message", onMessage);

    const watch = window.setInterval(() => {
      if (!popup.closed) return;
      if (closedAt === null) {
        closedAt = Date.now();
        return;
      }
      if (Date.now() - closedAt < 400) return;
      finish(() => reject(new Abandoned("the GitHub window was closed before it answered")));
    }, 200);

    const patience = window.setTimeout(() => {
      finish(() => {
        popup.close();
        reject(new Abandoned("the GitHub authorisation was not completed"));
      });
    }, PATIENCE_MS);
  });
}
