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

package experiments_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// The scoped job credential (§3.1 item 6, and the risk register's "token expiry
// vs. long Ray jobs" row).

// keycloak is a token endpoint that records what it was asked for.
type keycloak struct {
	server *httptest.Server

	mux sync.Mutex
	// forms is every exchange request, so a test can assert the RFC 8693 grant was
	// used rather than something that happened to work.
	forms []url.Values
	// token and expiresIn are what it answers with.
	token     string
	expiresIn int64
	// status non-zero refuses with that code.
	status int
	cause  string
}

func newKeycloak(t *testing.T) *keycloak {
	t.Helper()
	fake := &keycloak{token: "a-token-minted-for-the-job", expiresIn: 43200}
	fake.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if err := request.ParseForm(); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			fake.mux.Lock()
			fake.forms = append(fake.forms, request.PostForm)
			status, cause, token, expires := fake.status, fake.cause, fake.token, fake.expiresIn
			fake.mux.Unlock()

			writer.Header().Set("Content-Type", "application/json")
			if status != 0 {
				writer.WriteHeader(status)
				_ = json.NewEncoder(writer).Encode(map[string]string{
					"error": cause,
					// Deliberately present: error_description echoes request parameters,
					// and a test asserts ODE never carries it into an error.
					"error_description": "subject_token=" + request.PostFormValue("subject_token"),
				})
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"access_token": token,
				"expires_in":   expires,
				"token_type":   "Bearer",
			})
		}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (k *keycloak) lastForm(t *testing.T) url.Values {
	t.Helper()
	k.mux.Lock()
	defer k.mux.Unlock()
	if len(k.forms) == 0 {
		t.Fatal("the token endpoint was never called")
	}
	return k.forms[len(k.forms)-1]
}

func withKeycloak(fake *keycloak) options {
	return func(deps *experiments.Deps) {
		deps.KeycloakURL = fake.server.URL
		deps.KeycloakRealm = "senergy"
		deps.KeycloakClientID = "ode"
		deps.KeycloakClientSecret = "a-client-secret"
		deps.JobTokenAudience = "timescale-wrapper"
	}
}

// Without a configured exchange, ODE degrades the way the rest of ODE degrades:
// it passes the caller's token and *says so in the answer*. The saying-so is the
// part under test — the alternative is not "no limitation", it is an undocumented
// one discovered from a Ray log at hour two.
func TestWithoutATokenExchangeTheJobCarriesTheSessionTokenAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.ready()

	result := h.launch()

	if result.Credential.Source != "session" {
		t.Errorf("source = %q, want the session token", result.Credential.Source)
	}
	if !result.Credential.ExpiresWithSession {
		t.Error("the result does not say the credential expires with the session")
	}
	if !strings.Contains(result.Credential.Note, "keycloak_url") {
		t.Errorf("note = %q, want it to name what a deployment would configure",
			result.Credential.Note)
	}
	if result.ScopedCredential {
		t.Error("the record claims a scoped credential where none was minted")
	}
	// The job still gets a working credential — the developer's own, which is what
	// §3.1 step 3 requires either way.
	if got := h.ray.LastJob(t).RuntimeEnv.EnvVars["SENERGY_TOKEN"]; got == "" {
		t.Error("the job was given no platform token at all")
	}
}

// The bearer ODE receives still carries its "Bearer " prefix: service-commons'
// jwt.Parse keeps the caller's original string and strips the prefix only to
// parse it. Operator Lib's wrapper client builds its own Authorization header
// from SENERGY_TOKEN, so a prefix passed through arrives as "Bearer Bearer ey..."
// and the read fails with a 401 that surfaces as TokenExpiredError — an hour of
// looking at token lifetimes for a defect in the first eight characters.
func TestTheSessionTokenReachesTheJobWithoutItsBearerPrefix(t *testing.T) {
	h := newHarness(t)
	h.ready()

	bare := unsignedToken(testUsername)
	h.launch(func(req *experiments.LaunchRequest) {
		req.Bearer = "Bearer " + bare
	})

	if got := h.ray.LastJob(t).RuntimeEnv.EnvVars["SENERGY_TOKEN"]; got != bare {
		t.Errorf("SENERGY_TOKEN = %q, want the bare token %q", got, bare)
	}
}

func TestAConfiguredExchangeMintsATokenForTheJob(t *testing.T) {
	fake := newKeycloak(t)
	h := newHarness(t, withKeycloak(fake))
	h.ready()

	result := h.launch()

	form := fake.lastForm(t)
	if got := form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:token-exchange" {
		t.Errorf("grant_type = %q, want RFC 8693's", got)
	}
	if got := form.Get("subject_token_type"); got != "urn:ietf:params:oauth:token-type:access_token" {
		t.Errorf("subject_token_type = %q", got)
	}
	if form.Get("client_id") != "ode" || form.Get("client_secret") != "a-client-secret" {
		t.Error("the exchange did not authenticate as the configured client")
	}
	if form.Get("audience") != "timescale-wrapper" {
		t.Errorf("audience = %q, want the client a job actually reads", form.Get("audience"))
	}
	// On behalf of the developer, not instead of them (§3.1 step 3).
	if !strings.HasPrefix(form.Get("subject_token"), "eyJ") {
		t.Errorf("subject_token = %q, want the developer's own token exchanged",
			form.Get("subject_token"))
	}

	if result.Credential.Source != "exchanged" {
		t.Errorf("source = %q, want the minted token", result.Credential.Source)
	}
	if result.Credential.ExpiresWithSession {
		t.Error("a minted token was reported as expiring with the session")
	}
	if result.Credential.ExpiresIn != 43200 {
		t.Errorf("expires_in = %d, want the issuer's own figure", result.Credential.ExpiresIn)
	}
	if !result.ScopedCredential {
		t.Error("the record does not record that the job has its own credential")
	}
	if got := h.ray.LastJob(t).RuntimeEnv.EnvVars["SENERGY_TOKEN"]; got != "a-token-minted-for-the-job" {
		t.Errorf("the job carries %q, want the minted token", got)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for a healthy exchange", result.Warnings)
	}
}

// A configured exchange that refuses must not fail the launch. The developer
// could have had the run; what they get is the run and a warning.
func TestARefusedExchangeDegradesToTheSessionTokenWithAWarning(t *testing.T) {
	fake := newKeycloak(t)
	fake.status, fake.cause = http.StatusBadRequest, "invalid_grant"
	h := newHarness(t, withKeycloak(fake))
	h.ready()

	result := h.launch()

	if result.Credential.Source != "session" {
		t.Errorf("source = %q, want the fallback", result.Credential.Source)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("a refused exchange produced no warning")
	}
	warning := strings.Join(result.Warnings, " ")
	if !strings.Contains(warning, "invalid_grant") {
		t.Errorf("warning = %q, want the OAuth error code", warning)
	}
	// error_description echoes the subject token back. It must never travel.
	if strings.Contains(warning, "subject_token") || strings.Contains(warning, "eyJ") {
		t.Errorf("the warning carries the request's own token: %q", warning)
	}
	if len(h.ray.Jobs()) != 1 {
		t.Error("the launch was abandoned over a credential it had a fallback for")
	}
}

// The configured lifetime is an expectation, not a request: neither RFC 8693 nor
// Keycloak takes one. What ODE can do is notice the gap and say so.
func TestAShorterLifetimeThanConfiguredIsReportedRatherThanSilentlyAccepted(t *testing.T) {
	fake := newKeycloak(t)
	fake.expiresIn = 300
	h := newHarness(t, withKeycloak(fake), func(deps *experiments.Deps) {
		deps.JobTokenLifetime = 12 * time.Hour
	})
	h.ready()

	result := h.launch()

	if result.Credential.Source != "exchanged" {
		t.Fatalf("source = %q, want the minted token", result.Credential.Source)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("a five-minute token in a deployment expecting twelve hours passed silently")
	}
	warning := strings.Join(result.Warnings, " ")
	if !strings.Contains(warning, "300") || !strings.Contains(warning, "43200") {
		t.Errorf("warning = %q, want both figures so the gap is actionable", warning)
	}
	// The lifetime is not something ODE can ask for, so it must not have tried.
	form := fake.lastForm(t)
	for _, unsupported := range []string{"requested_lifetime", "expires_in", "lifetime"} {
		if form.Get(unsupported) != "" {
			t.Errorf("the exchange sent %q, which neither RFC 8693 nor Keycloak accepts",
				unsupported)
		}
	}
}

// Nothing on this path may write a token anywhere a reader could reach it.
func TestNoTokenReachesAResponseBody(t *testing.T) {
	fake := newKeycloak(t)
	h := newHarness(t, withKeycloak(fake))
	h.ready()

	result := h.launch()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"a-token-minted-for-the-job", "a-client-secret", "eyJ"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("the launch result carries %q: %s", secret, encoded)
		}
	}

	listed, err := h.service.List(t.Context(), h.request(), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	encoded, _ = json.Marshal(listed)
	for _, secret := range []string{"a-token-minted-for-the-job", "a-client-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("a stored experiment carries %q: %s", secret, encoded)
		}
	}
}
