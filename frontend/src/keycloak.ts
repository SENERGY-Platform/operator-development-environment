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
// authorisation decision is the backend's (SPEC §3.1), so nothing here is a
// security control — hiding a button does not protect a route.
//
// The `/auth` suffix on the default URL is required, not cosmetic. This
// Keycloak serves the legacy base path — its discovery document reports
// `issuer: https://auth.senergy.infai.org/auth/realms/master` — while
// keycloak-js 17 and later default to the path without it. Dropping `/auth`
// produces a 404 on the authorization endpoint, which reads like a broken
// realm rather than a broken prefix.
const keycloak = new Keycloak({
  url: import.meta.env.VITE_KEYCLOAK_URL ?? "https://auth.senergy.infai.org/auth",
  realm: import.meta.env.VITE_KEYCLOAK_REALM ?? "master",
  clientId: import.meta.env.VITE_KEYCLOAK_CLIENT_ID ?? "ode",
});

export async function initKeycloak(): Promise<boolean> {
  return keycloak.init({
    onLoad: "login-required",
    pkceMethod: "S256",
    checkLoginIframe: false,
  });
}

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
