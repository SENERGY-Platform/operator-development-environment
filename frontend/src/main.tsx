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
import { initKeycloak } from "./keycloak";
import "./index.css";

const root = createRoot(document.getElementById("root")!);

// Nothing renders before authentication: every backend route except /health
// requires a token, so an unauthenticated shell would only show errors.
initKeycloak()
  .then((authenticated) => {
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
