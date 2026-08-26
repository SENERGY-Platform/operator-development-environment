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

package repo_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
)

func TestAuthorizeAsksForWhatItNeedsAndSaysWhereItReturns(t *testing.T) {
	h := newHarness(t)

	authorize, err := h.service.Authorize(testUserSub)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	parsed, err := url.Parse(authorize.URL)
	if err != nil {
		t.Fatalf("the authorize url does not parse: %v", err)
	}
	query := parsed.Query()
	if query.Get("client_id") != "client" || query.Get("state") != authorize.State {
		t.Errorf("query = %v", query)
	}
	if query.Get("scope") != "repo workflow" {
		t.Errorf("scope = %q, want the two scopes §5.11 item 1 names", query.Get("scope"))
	}
	// The redirect is reported because it has to match the OAuth app's registered
	// callback exactly, and a mismatch is the failure a developer cannot diagnose
	// from GitHub's error page.
	if authorize.RedirectURI != "http://localhost:5173/github/callback" ||
		query.Get("redirect_uri") != authorize.RedirectURI {
		t.Errorf("redirect = %q", authorize.RedirectURI)
	}
}

func TestConnectRefusesACodeWithoutAMatchingState(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.Connect(context.Background(), testUserSub, "code-1", "invented"); !errors.Is(
		err, repo.ErrInvalidRequest) {
		t.Errorf("error = %v, want a refusal", err)
	}
	// A state issued for one developer cannot be completed by another.
	authorize, _ := h.service.Authorize(testUserSub)
	if _, err := h.service.Connect(context.Background(), "someone-else", "code-1",
		authorize.State); !errors.Is(err, repo.ErrInvalidRequest) {
		t.Errorf("error = %v, want a refusal", err)
	}
}

func TestConnectStoresTheCredentialSealed(t *testing.T) {
	h := newHarness(t)
	identity := h.connect()

	if identity.Login != "jonah" || len(identity.MissingScopes) != 0 {
		t.Fatalf("identity = %+v", identity)
	}

	stored, found, err := h.store.GetIdentity(context.Background(), testUserSub)
	if err != nil || !found {
		t.Fatalf("GetIdentity: %v, found %v", err, found)
	}
	if strings.Contains(stored.SealedToken, repotest.Token) {
		t.Fatal("the stored row contains the token in plain text")
	}
	if opened, err := h.sealer.Open(stored.SealedToken); err != nil || opened != repotest.Token {
		t.Fatalf("the stored token does not open back to the credential: %q, %v", opened, err)
	}

	// And it is reported without the credential.
	reported, connected, err := h.service.Connection(context.Background(), testUserSub)
	if err != nil || !connected || reported.Login != "jonah" {
		t.Fatalf("Connection = %+v, %v, %v", reported, connected, err)
	}
}

// A developer can narrow the grant on GitHub's consent screen. Pushing the
// scaffold would then fail on the workflow file, so the shortfall is reported at
// connection time rather than discovered later.
func TestConnectReportsAGrantThatIsNarrowerThanAsked(t *testing.T) {
	h := newHarness(t)
	h.github.SetTokenScopes("repo")

	identity := h.connect()
	if len(identity.MissingScopes) != 1 || identity.MissingScopes[0] != "workflow" {
		t.Errorf("missing scopes = %v, want workflow", identity.MissingScopes)
	}
}

func TestDisconnectForgetsTheCredentialAndKeepsTheFiles(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "Scaffold the operator")

	if err := h.service.Disconnect(context.Background(), testUserSub); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if _, connected, _ := h.service.Connection(context.Background(), testUserSub); connected {
		t.Error("the credential is still there")
	}
	// The working copy is the developer's work on their own PVC, and §5.11 item 6 is
	// explicit that ODE does not remove it.
	if content := h.read(t, "jonah/pv-forecast/op.py"); content == "" {
		t.Error("disconnecting removed the working copy")
	}
}
