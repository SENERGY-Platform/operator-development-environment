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

import { beforeEach, describe, expect, it, vi } from "vitest";

/**
 * The handoff that lets the GitHub callback survive a Keycloak login.
 *
 * Keycloak refuses a `redirect_uri` whose query already carries a parameter it is
 * itself about to append. Confirmed against the deployment rather than assumed —
 * `?code=`, `?state=`, `?session_state=`, `?iss=` and `?error=` are each answered
 * with 400, while `?mycode=` and `?statement=` are answered with the login page,
 * so the check is on exact parameter names.
 *
 * That makes §5.11 item 1 impossible without moving them: GitHub's callback is a
 * fresh page load carrying `code` and `state`, `login-required` hands the current
 * address to Keycloak, and Keycloak rejects it. What is tested here is that the
 * parameters come off before the login and go back on after it, because the two
 * halves are separated by a full page redirect and only sessionStorage crosses it.
 */

// keycloak-js is not exercised here; only the address bookkeeping around it is.
vi.mock("keycloak-js", () => ({
  default: class {
    init = vi.fn(async () => true);
    updateToken = vi.fn(async () => true);
    login = vi.fn();
    logout = vi.fn();
    token = "test-token";
  },
}));

const { __internals } = await import("./keycloak");
const { setAside, restore, CARRIED, RESERVED } = __internals;

/** Points jsdom's address at `url` without a navigation. */
function at(url: string) {
  window.history.replaceState(null, "", url);
}

beforeEach(() => {
  sessionStorage.clear();
  at("/");
});

describe("setting the reserved parameters aside", () => {
  it("leaves an ordinary address alone", () => {
    at("/tools/profiler");
    expect(setAside()).toBeUndefined();
    expect(sessionStorage.getItem(CARRIED)).toBeNull();
  });

  /*
   * The one that matters for every bookmark in the README's table. These names are
   * not reserved, Keycloak accepts them, and stripping them would send a developer
   * who reloaded a profile back to an empty form.
   */
  it("leaves the view's own query alone", () => {
    at("/tools/profiler?series=abc&profile=p1&tab=periodicity");
    expect(setAside()).toBeUndefined();
    expect(window.location.search).toBe("?series=abc&profile=p1&tab=periodicity");
  });

  it("moves GitHub's callback out of the redirect and remembers it", () => {
    at("/github/callback?code=gh_code_1&state=gh_state_1");
    const redirect = setAside();
    expect(redirect).toBe("http://localhost:3000/github/callback");
    expect(sessionStorage.getItem(CARRIED)).toBe("?code=gh_code_1&state=gh_state_1");
  });

  it("keeps the path, so the callback route is still the one returned to", () => {
    at("/github/callback?code=x&state=y");
    expect(setAside()).toContain("/github/callback");
  });

  /* A refusal comes back as `?error=access_denied`, which is reserved too — so the
     "GitHub refused the authorisation" path needs the same handling as the happy one. */
  it("moves a refusal as well as a grant", () => {
    at("/github/callback?error=access_denied&error_description=denied");
    expect(setAside()).toBe("http://localhost:3000/github/callback");
    expect(sessionStorage.getItem(CARRIED)).toContain("error=access_denied");
  });

  it("keeps a non-reserved parameter while moving a reserved one", () => {
    at("/github/callback?session=s1&code=c1");
    expect(setAside()).toBe("http://localhost:3000/github/callback?session=s1");
    expect(sessionStorage.getItem(CARRIED)).toBe("?session=s1&code=c1");
  });

  it("does not carry a fragment into the redirect, which is Keycloak's to use", () => {
    at("/github/callback?code=c#somewhere");
    expect(setAside()).not.toContain("#");
  });

  it("only reacts to exact names, the way the server does", () => {
    // Verified against the deployment: ?mycode= and ?statement= are accepted there.
    at("/github/callback?mycode=abc&statement=x");
    expect(setAside()).toBeUndefined();
  });

  it("covers every name the deployment was observed to refuse", () => {
    for (const name of ["code", "state", "session_state", "iss", "error"]) {
      expect(RESERVED, `${name} is refused by Keycloak but not handled here`).toContain(name);
    }
  });
});

describe("restoring them afterwards", () => {
  it("puts the callback back on the address", () => {
    at("/github/callback?code=c1&state=s1");
    setAside();
    // What the browser looks like coming back from Keycloak: the clean path, with
    // Keycloak's own response already consumed out of the fragment by keycloak-js.
    at("/github/callback");
    restore();
    expect(window.location.search).toBe("?code=c1&state=s1");
    expect(window.location.pathname).toBe("/github/callback");
  });

  it("clears the handoff, so a reload does not replay a spent authorisation", () => {
    at("/github/callback?code=c1&state=s1");
    setAside();
    at("/github/callback");
    restore();
    expect(sessionStorage.getItem(CARRIED)).toBeNull();
    // A second pass finds nothing and leaves the address as it is.
    at("/");
    restore();
    expect(window.location.search).toBe("");
  });

  it("does nothing when there was no callback to carry", () => {
    at("/tools/profiler?series=abc");
    restore();
    expect(window.location.search).toBe("?series=abc");
  });

  it("survives storage being unavailable", () => {
    const get = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    at("/github/callback");
    expect(() => restore()).not.toThrow();
    get.mockRestore();
  });

  it("survives storage being unavailable while setting aside", () => {
    const set = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    at("/github/callback?code=c1");
    // The parameters still come off: a refused login is worse than a lost callback.
    expect(setAside()).toBe("http://localhost:3000/github/callback");
    set.mockRestore();
  });
});
