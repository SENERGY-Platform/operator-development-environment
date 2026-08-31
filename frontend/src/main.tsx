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

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { relayAuthorisation } from "./github";
import { initKeycloak } from "./keycloak";
import { refresh } from "./router";
import { initTheme } from "./theme";
import "./index.css";

/*
 * A popup carrying GitHub's answer home is not an application.
 *
 * It exists to hand two query parameters to the tab that opened it and close, and
 * booting the shell to do that would put a Keycloak round trip — possibly a visible
 * login — inside a window that lives for a quarter of a second. Checked before
 * anything else for that reason, including before the theme: there is nothing here
 * to paint.
 *
 * A full-tab callback, which is what the connect card's flow produces, has no
 * opener and falls through to the application as before.
 */
if (!relayAuthorisation()) boot();

function boot() {
  // Before the first render, not in an effect: an effect runs after the first paint,
  // so a developer who chose dark would get one frame of light on every load.
  initTheme();

  const root = createRoot(document.getElementById("root")!);

  // Nothing renders before authentication: every backend route except /health
  // requires a token, so an unauthenticated shell would only show errors.
  initKeycloak()
    .then((authenticated) => {
      // Keycloak rewrites the address to clear its own response parameters. The
      // router took its first reading before that happened, so it takes another.
      refresh();
      root.render(
        <StrictMode>{authenticated ? <App /> : <p>Authentication failed.</p>}</StrictMode>,
      );
    })
    .catch((err: unknown) => {
      root.render(
        <div className="centered">
          <div className="fatal">
            <h1>Could not reach Keycloak</h1>
            <p>{err instanceof Error ? err.message : String(err)}</p>
          </div>
        </div>,
      );
    });
}
