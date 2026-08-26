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

package experiments

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The scoped job credential (SPEC §3.1 item 6, and the risk register's
// "token expiry vs. long Ray jobs" row).
//
// A Ray job reads its training data directly from timescale-wrapper with its own
// token (§5.3.4) — never streamed through ODE — so the job needs a credential
// that outlives the browser tab it was launched from. An interactive access token
// is minutes to an hour; a training run is hours. Handing the job the caller's
// session token means a run that dies partway through with a 401, having already
// spent the cluster time.
//
// So where the deployment configures a Keycloak token exchange, ODE mints one
// token per submission through RFC 8693, acting *on behalf of the developer* —
// the job's authorisation is still the developer's, which is what §3.1 step 3
// requires and what a service account would have violated.
//
// **Where it is not configured, ODE degrades the way the rest of ODE degrades.**
// The caller's token is passed, a warning names what is missing once at startup,
// and the launch result says the credential expires with the session. That last
// part is the one that matters: the alternative is not "no limitation", it is
// "an undocumented limitation discovered from a Ray log at hour two".
//
// Nothing here ever logs a token, and TokenExchangeError carries the OAuth error
// code but never error_description — which echoes request parameters back, and
// the request parameters include a token.

// jobCredential is what a submission will carry.
type jobCredential struct {
	// Token is the value that goes into the job's environment. Never logged, never
	// stored, never part of any response body.
	Token string
	Credential
}

// exchangeConfigured reports whether a token exchange can be attempted.
func (s *Service) exchangeConfigured() bool {
	return s.opts.KeycloakURL != "" && s.opts.KeycloakRealm != "" &&
		s.opts.KeycloakClientID != "" && s.opts.KeycloakClientSecret != ""
}

// ExchangeConfigured is exchangeConfigured for the wiring, so startup can say what
// a deployment is missing rather than restating the rule.
func (s *Service) ExchangeConfigured() bool { return s.exchangeConfigured() }

// tokenEndpoint is the realm's OpenID Connect token endpoint.
func (s *Service) tokenEndpoint() string {
	return strings.TrimSuffix(s.opts.KeycloakURL, "/") +
		"/realms/" + url.PathEscape(s.opts.KeycloakRealm) + "/protocol/openid-connect/token"
}

// The RFC 8693 vocabulary, spelled out rather than inlined: these are the exact
// URNs the grant is identified by, and a typo in one produces an
// unsupported_grant_type that reads like a server misconfiguration.
const (
	grantTokenExchange  = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccess     = "urn:ietf:params:oauth:token-type:access_token"
	credentialExchanged = "exchanged"
	credentialSession   = "session"
)

// jobToken produces the credential a submission will carry.
//
// It never fails a launch. An exchange that is configured but refuses is a
// deployment fault worth a warning, not a reason to refuse a run the developer
// could have had — so the caller's token is used instead and the result says both
// that it happened and why. The alternative, failing the launch, would make a
// misconfigured identity provider look like a broken Ray cluster.
func (s *Service) jobToken(ctx context.Context, bearer string) (jobCredential, []string) {
	session := jobCredential{
		Token: bearer,
		Credential: Credential{
			Source:             credentialSession,
			ExpiresWithSession: true,
			Note: "this job carries the developer's interactive session token: a run that " +
				"outlives the session will lose its access to the platform partway through. " +
				"Configure keycloak_url, keycloak_realm, keycloak_client_id and " +
				"keycloak_client_secret to mint a token scoped to the job instead (§3.1 item 6)",
		},
	}
	if !s.exchangeConfigured() {
		return session, nil
	}

	exchanged, expiresIn, err := s.exchange(ctx, bearer)
	if err != nil {
		// The error is logged by the caller with the launch it belongs to; here it
		// becomes a warning the developer reads, because it changes what their run
		// can survive.
		return session, []string{
			"the configured token exchange refused, so this job carries the session " +
				"token instead: " + err.Error(),
		}
	}

	credential := jobCredential{
		Token: exchanged,
		Credential: Credential{
			Source:             credentialExchanged,
			ExpiresIn:          expiresIn,
			ExpiresWithSession: false,
			Note: "this job carries a token minted for it, on behalf of the developer " +
				"(§3.1 item 6); it is not tied to the interactive session",
		},
	}

	// The configured lifetime is an *expectation*, not a request.
	//
	// Neither RFC 8693 nor Keycloak accepts a requested lifetime as a parameter —
	// the lifetime is the realm's and the client's configuration. So what ODE can do
	// is check what came back against what the deployment believes it configured,
	// and say when the two disagree. A silent 300-second token in a deployment that
	// thinks it has twelve hours is exactly the failure this whole path exists to
	// prevent.
	var warnings []string
	wanted := int64(s.opts.JobTokenLifetime.Seconds())
	if wanted > 0 && expiresIn > 0 && expiresIn < wanted {
		warnings = append(warnings, fmt.Sprintf(
			"the exchanged job token is valid for %ds, short of the %ds this deployment "+
				"expects; the lifetime is the Keycloak client's rather than something ODE "+
				"can ask for, so raise it there if a run needs longer",
			expiresIn, wanted))
	}
	return credential, warnings
}

// exchange performs the RFC 8693 request.
//
// Returns the token and its lifetime in seconds. The token never leaves this
// function except into a job's environment, and nothing on this path writes it
// anywhere — not to a log, not to the store, not into an error.
func (s *Service) exchange(ctx context.Context, bearer string) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.RequestTimeout)
	defer cancel()

	form := url.Values{
		"grant_type":           []string{grantTokenExchange},
		"client_id":            []string{s.opts.KeycloakClientID},
		"client_secret":        []string{s.opts.KeycloakClientSecret},
		"subject_token":        []string{strings.TrimPrefix(bearer, "Bearer ")},
		"subject_token_type":   []string{tokenTypeAccess},
		"requested_token_type": []string{tokenTypeAccess},
	}
	if s.opts.JobTokenAudience != "" {
		// Keycloak's exchange returns a token for the requesting client unless an
		// audience names another. A job reads timescale-wrapper, so without this the
		// minted token is usually for the wrong client and fails at the gateway.
		form.Set("audience", s.opts.JobTokenAudience)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenEndpoint(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, &TokenExchangeError{Err: err}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := s.http.Do(request)
	if err != nil {
		return "", 0, &TokenExchangeError{Err: err}
	}
	defer response.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	// Decoded before the status check so a refusal's `error` field is available;
	// a body that does not parse simply leaves Cause empty.
	_ = json.Unmarshal(body, &answer)

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, &TokenExchangeError{Code: response.StatusCode, Cause: answer.Error}
	}
	if answer.AccessToken == "" {
		return "", 0, &TokenExchangeError{
			Code: response.StatusCode, Cause: "the response carried no access_token",
		}
	}
	return answer.AccessToken, answer.ExpiresIn, nil
}

// defaultJobTokenLifetime is what a deployment is assumed to have configured when
// it says nothing. Twelve hours matches jupyterhub_token_ttl, because the two
// bound the same thing from different directions: how long a developer's work can
// run unattended.
const defaultJobTokenLifetime = 12 * time.Hour
