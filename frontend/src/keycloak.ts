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

import Keycloak from "keycloak-js";

// The SPA is a public client: it obtains a token and presents it. Every
// authorisation decision is the backend's (§3.1), so nothing here is a
// security control — hiding a button does not protect a route.
//
// There is no default URL, deliberately: a deployment's Keycloak address is
// operational data and a committed fallback would send an unconfigured build at
// somebody's production realm. `VITE_KEYCLOAK_URL` is required.
//
// Whatever it is set to, the `/auth` suffix may be required and is not cosmetic.
// A Keycloak serving the legacy base path reports an issuer ending in
// `/auth/realms/<realm>`, while keycloak-js 17 and later default to the path
// without it. Dropping `/auth` against such a deployment produces a 404 on the
// authorization endpoint, which reads like a broken realm rather than a broken
// prefix. Confirm with the discovery document before guessing.
const keycloakUrl = import.meta.env.VITE_KEYCLOAK_URL;
if (!keycloakUrl) {
  throw new Error(
    "VITE_KEYCLOAK_URL is not set. Copy frontend/.env.example to .env.local and " +
      "set it — there is no default, because a deployment address does not belong " +
      "in a committed file.",
  );
}

const keycloak = new Keycloak({
  url: keycloakUrl,
  realm: import.meta.env.VITE_KEYCLOAK_REALM ?? "master",
  clientId: import.meta.env.VITE_KEYCLOAK_CLIENT_ID ?? "ode",
});

/**
 * The OAuth response parameters Keycloak refuses to see in a `redirect_uri`.
 *
 * Keycloak will not redirect to an address that already carries a parameter it is
 * itself about to append — allowing it would let a caller pre-seed the response
 * the client then reads. The rule is exact name matching, not a substring match:
 * `?mycode=` and `?statement=` are accepted, `?code=` and `?state=` are not.
 *
 * Reasonable of it, and directly in the way of §5.11 item 1. GitHub sends its
 * authorisation back as `…/github/callback?code=…&state=…`, which is a fresh page
 * load holding no token, so `login-required` sends the browser to Keycloak with
 * *that* address as the redirect — GitHub's `code` and `state` included. Keycloak
 * answers "Invalid parameter: redirect_uri" and the connection can never complete.
 * Nothing in the client's Valid Redirect URIs can allow it; the check is on the
 * parameter names, not on the pattern.
 */
const RESERVED = ["code", "state", "session_state", "iss", "error", "error_description", "error_uri"];

/** Where the callback's query waits while Keycloak is being talked to. */
const CARRIED = "ode.auth.carried";

/**
 * Takes the reserved parameters out of the address and remembers them, returning
 * the address Keycloak will accept — or undefined when there was nothing to move,
 * which is the ordinary case and leaves keycloak-js to work out its own redirect.
 *
 * Only the reserved names are removed. `?session=`, `?profile=`, `?series=` and the
 * rest of what the README calls the state in the URL are passed through untouched,
 * because Keycloak has no objection to them and dropping them would break every
 * bookmark and shared link to a view.
 */
function setAside(): string | undefined {
  const url = new URL(window.location.href);
  if (!RESERVED.some((name) => url.searchParams.has(name))) return undefined;

  try {
    sessionStorage.setItem(CARRIED, url.search);
  } catch {
    // Without storage the round trip cannot be completed, but handing Keycloak the
    // address unchanged only trades a lost callback for a refused login. The
    // parameters still come off.
  }
  for (const name of RESERVED) url.searchParams.delete(name);
  // No fragment: Keycloak puts its own response there.
  return url.origin + url.pathname + url.search;
}

/**
 * Puts the callback's query back, before anything renders.
 *
 * `App` reads the GitHub callback out of the address during its first render, so
 * this has to happen while `initKeycloak` is still being awaited rather than in an
 * effect. `replaceState` rather than a navigation, because the spent authorisation
 * must not come back with the back button.
 */
function restore(): void {
  let search: string | null = null;
  try {
    search = sessionStorage.getItem(CARRIED);
    if (search !== null) sessionStorage.removeItem(CARRIED);
  } catch {
    return;
  }
  if (search === null || search === "") return;
  window.history.replaceState(null, "", window.location.pathname + search + window.location.hash);
}

export async function initKeycloak(): Promise<boolean> {
  const redirectUri = setAside();
  const authenticated = await keycloak.init({
    onLoad: "login-required",
    pkceMethod: "S256",
    checkLoginIframe: false,
    // Only overridden when something had to be moved out of the way. Left alone,
    // keycloak-js uses the current address, which is the right answer.
    ...(redirectUri === undefined ? {} : { redirectUri }),
  });
  restore();
  return authenticated;
}

/** Exported for the test; not part of the module's job. */
export const __internals = { setAside, restore, RESERVED, CARRIED };

// token refreshes the access token when it is close to expiring. The backend
// rejects an expired token with 401, so this runs before every request rather
// than on a timer.
export async function token(): Promise<string | undefined> {
  try {
    await keycloak.updateToken(30);
  } catch {
    // Refresh failed — the session is gone. Send the user back to the login
    // page rather than letting every request fail with a 401.
    await keycloak.login();
    return undefined;
  }
  return keycloak.token;
}

export function logout(): void {
  void keycloak.logout();
}

export default keycloak;
