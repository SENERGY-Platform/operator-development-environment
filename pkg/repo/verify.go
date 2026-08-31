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

// Asking GitHub about the credential ODE holds, and reporting the answer.
//
// Connection reports the stored row, which records what was granted and not whether
// it still is. That is the right default — the pane polls it, and a GitHub round trip
// per poll is not free — and it leaves one question unanswerable from inside ODE:
// when a push is refused, is the credential dead, too narrow, or fine?
//
// This is that question asked out loud. It exists because the alternative was a
// developer and an assistant guessing at it in turns: git reports a rejected
// credential and a missing one in the same sentence, and ODE's own error text can
// only ever be as specific as what it checked.
//
// What it reports about the token itself is its *kind* and its length, never any part
// of its value. The kind is the prefix GitHub puts on every token it issues, and it
// answers the question that changes the diagnosis most: `gho_` is an OAuth app's
// token, which carries scopes and does not expire, while `ghu_` is a GitHub App's
// user token, which carries no scopes, expires in hours unless the app is configured
// not to, and only reaches repositories the app is installed on. A deployment that
// registered a GitHub App where it meant to register an OAuth app works for one
// afternoon and then refuses every push, and nothing else in ODE says so.

package repo

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Verification is what GitHub says about the stored credential right now.
type Verification struct {
	// Valid is whether GitHub answered ODE's own API call with it.
	Valid bool `json:"valid"`
	// Code and Message are GitHub's, when it refused.
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Login is who GitHub says the credential belongs to, which is worth comparing
	// against the stored row: a developer who reconnected a different account gets a
	// working credential for the wrong repositories.
	Login string `json:"login,omitempty"`
	// Scopes is what GitHub reports for the token, and ScopesReported whether it sent
	// the header at all. An empty list with the header present is a grant that was
	// narrowed; no header is a credential that has no scopes as a concept.
	Scopes         []string `json:"scopes"`
	ScopesReported bool     `json:"scopes_reported"`
	// Kind is the token's prefix and what that prefix means. Never any part of the
	// token itself.
	Kind string `json:"kind"`
	// Length is the token's length in characters, which distinguishes a truncated
	// credential from a whole one.
	Length int `json:"length"`

	// StoredAt is when ODE wrote this credential, and Age how long ago that was.
	//
	// The field that ends an argument about whether a reconnection happened. A
	// credential GitHub refuses that ODE stored yesterday means the new one was never
	// written — the flow was abandoned, or it failed — and the repair is to finish it.
	// A credential GitHub refuses that ODE stored a minute ago means something is
	// wrong with the exchange itself, which is a completely different search. Without
	// this the two are the same sentence, and the developer is left pressing the same
	// button.
	StoredAt string `json:"stored_at,omitempty"`
	Age      string `json:"age,omitempty"`
	// StoredLogin is the account the row says this credential belongs to. Worth
	// comparing against Login: a developer who reconnected while signed in to a second
	// GitHub account gets a credential that works and cannot see their repositories,
	// and the two names differing is the only visible sign of it.
	StoredLogin string `json:"stored_login,omitempty"`
}

// tokenKinds is GitHub's prefix vocabulary. The prefixes are public and fixed; a
// token's first four characters are what GitHub itself uses to route secret
// scanning, and they are the only part of it that appears anywhere in ODE.
var tokenKinds = []struct {
	prefix  string
	meaning string
}{
	{"gho_", "an OAuth app's user token: carries scopes, does not expire"},
	{"ghu_", "a GitHub App's user access token: no scopes, expires in hours unless the " +
		"app disables expiry, and reaches only repositories the app is installed on"},
	{"ghs_", "a GitHub App's installation token: not what a user authorisation produces"},
	{"ghp_", "a classic personal access token"},
	{"github_pat_", "a fine-grained personal access token"},
	{"ghr_", "a GitHub App's refresh token: not usable as a credential"},
}

func tokenKind(token string) string {
	for _, kind := range tokenKinds {
		if strings.HasPrefix(token, kind.prefix) {
			return kind.prefix + " — " + kind.meaning
		}
	}
	if strings.TrimSpace(token) == "" {
		return "empty"
	}
	return "unrecognised prefix"
}

// Verify asks GitHub whether the stored credential still works, and reports what it
// said. ErrNotConnected when there is no stored credential to ask about.
func (s *Service) Verify(ctx context.Context, userSub string) (Verification, error) {
	token, err := s.tokenFor(ctx, userSub)
	if err != nil {
		return Verification{}, err
	}
	report := Verification{Kind: tokenKind(token), Length: len(token), Scopes: []string{}}

	// The row, for the two facts GitHub cannot supply: when this credential was
	// stored, and whose it is said to be.
	if stored, found, err := s.store.GetIdentity(ctx, userSub); err == nil && found {
		report.StoredLogin = stored.Login
		if !stored.ConnectedAt.IsZero() {
			report.StoredAt = stored.ConnectedAt.UTC().Format(time.RFC3339)
			report.Age = s.now().Sub(stored.ConnectedAt).Round(time.Second).String()
		}
	}

	identity, scopes, reported, err := s.githubClient(token).viewer(ctx)
	if err != nil {
		var upstream *UpstreamError
		if errors.As(err, &upstream) {
			report.Code, report.Message = upstream.Code, upstream.Message
			if upstream.Code == 0 {
				// A transport failure says nothing about the credential, and reporting
				// it as invalid would be a guess dressed as a check.
				report.Message = "ODE could not reach GitHub: " + upstream.Err.Error()
			}
			return report, nil
		}
		return report, err
	}
	report.Valid = true
	report.Code = http.StatusOK
	report.Login = identity.Login
	report.ScopesReported = reported
	if scopes != nil {
		report.Scopes = scopes
	}
	return report, nil
}
