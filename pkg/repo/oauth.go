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

package repo

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The GitHub OAuth web flow of §5.11 item 1.
//
// The redirect lands on the SPA, not here, and the SPA posts the code back with
// its own platform token. That is one route fewer and one property more: every
// route in this package is behind the same realm-role gate as the rest of ODE, so
// there is no unauthenticated endpoint that takes a code and writes a credential.
//
// `state` is issued here, bound to the developer's subject, single-use and
// short-lived. All three matter. A state not bound to a subject lets one
// developer's browser complete another's flow; a state that is reusable turns a
// leaked URL into a replay; and a state without an expiry accumulates.

// oauthState is one pending flow.
type oauthState struct {
	userSub string
	created time.Time
}

// stateStore holds pending flows. In memory on purpose: a state lives for the
// seconds between two redirects, and a developer whose flow is interrupted by an
// ODE restart clicks connect again. Nothing is lost that a database would save.
type stateStore struct {
	mux    sync.Mutex
	states map[string]oauthState
	ttl    time.Duration
}

func newStateStore(ttl time.Duration) *stateStore {
	return &stateStore{states: map[string]oauthState{}, ttl: ttl}
}

func (s *stateStore) issue(userSub string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("issuing an oauth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)

	s.mux.Lock()
	defer s.mux.Unlock()
	s.expireLocked()
	s.states[state] = oauthState{userSub: userSub, created: time.Now()}
	return state, nil
}

// consume takes the state away whether or not it matches, so a wrong guess
// cannot be retried against the same value.
func (s *stateStore) consume(state, userSub string) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.expireLocked()

	pending, found := s.states[state]
	delete(s.states, state)
	if !found {
		return fmt.Errorf("%w: this authorisation is unknown or has expired; start the "+
			"connection again", ErrInvalidRequest)
	}
	if pending.userSub != userSub {
		return fmt.Errorf("%w: this authorisation was started by a different user", ErrInvalidRequest)
	}
	return nil
}

func (s *stateStore) expireLocked() {
	deadline := time.Now().Add(-s.ttl)
	for state, pending := range s.states {
		if pending.created.Before(deadline) {
			delete(s.states, state)
		}
	}
}

// AuthorizeRequest is what the SPA needs to start the flow.
type AuthorizeRequest struct {
	URL   string `json:"url"`
	State string `json:"state"`
	// Scopes is what ODE is asking for, reported so the SPA can say what the
	// consent screen will show.
	Scopes []string `json:"scopes"`
	// RedirectURI is where GitHub will send the browser. Reported because it has to
	// match the OAuth app's registered callback exactly, and a mismatch is the most
	// common setup failure — naming it makes the error message actionable.
	RedirectURI string `json:"redirect_uri"`
}

// Authorize begins the flow for one developer.
func (s *Service) Authorize(userSub string) (AuthorizeRequest, error) {
	if s.opts.ClientID == "" {
		return AuthorizeRequest{}, fmt.Errorf("%w: no GitHub OAuth app is configured", ErrInvalidRequest)
	}
	state, err := s.states.issue(userSub)
	if err != nil {
		return AuthorizeRequest{}, err
	}
	query := url.Values{
		"client_id":    {s.opts.ClientID},
		"redirect_uri": {s.opts.RedirectURI},
		"scope":        {strings.Join(s.opts.Scopes, " ")},
		"state":        {state},
		// The developer may have several GitHub accounts; asking every time is
		// friendlier than silently reusing whichever is logged in.
		"allow_signup": {"false"},
	}
	return AuthorizeRequest{
		URL:         strings.TrimSuffix(s.opts.WebURL, "/") + "/login/oauth/authorize?" + query.Encode(),
		State:       state,
		Scopes:      s.opts.Scopes,
		RedirectURI: s.opts.RedirectURI,
	}, nil
}

// Connect completes the flow: exchange the code, read who it belongs to, store it
// sealed.
//
// The identity read comes before the store on purpose. A token GitHub will not
// answer `GET /user` with is a token ODE cannot use for anything, and storing it
// would move the failure to the developer's first push.
func (s *Service) Connect(ctx context.Context, userSub, code, state string) (Identity, error) {
	if strings.TrimSpace(code) == "" {
		return Identity{}, fmt.Errorf("%w: no authorisation code", ErrInvalidRequest)
	}
	if err := s.states.consume(state, userSub); err != nil {
		return Identity{}, err
	}

	token, grantedScopes, err := s.exchange(ctx, code)
	if err != nil {
		return Identity{}, err
	}

	client := s.githubClient(token)
	identity, headerScopes, err := client.Viewer(ctx)
	if err != nil {
		return Identity{}, err
	}
	// The header is authoritative where it is present: it is what the token can
	// actually do now, while the exchange response is what the grant said at the
	// moment it was made.
	scopes := headerScopes
	if len(scopes) == 0 {
		scopes = grantedScopes
	}
	identity.Scopes = scopes
	identity.MissingScopes = missingScopes(s.opts.Scopes, scopes)
	identity.ConnectedAt = time.Now().UTC()

	sealed, err := s.sealer.Seal(token)
	if err != nil {
		return Identity{}, err
	}
	if err := s.store.PutIdentity(ctx, StoredIdentity{
		UserSub:       userSub,
		Login:         identity.Login,
		Name:          identity.Name,
		AvatarURL:     identity.AvatarURL,
		Scopes:        scopes,
		SealedToken:   sealed,
		ConnectedAt:   identity.ConnectedAt,
		MissingScopes: identity.MissingScopes,
	}); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

// Disconnect forgets the credential. It does not delete the working copy: the
// files are the developer's, they are on their PVC, and §5.11 item 6 is emphatic
// that ODE does not remove work on its own initiative.
func (s *Service) Disconnect(ctx context.Context, userSub string) error {
	return s.store.DeleteIdentity(ctx, userSub)
}

// Connection reports the stored identity, if any.
func (s *Service) Connection(ctx context.Context, userSub string) (Identity, bool, error) {
	stored, found, err := s.store.GetIdentity(ctx, userSub)
	if err != nil || !found {
		return Identity{}, found, err
	}
	return Identity{
		Login:         stored.Login,
		Name:          stored.Name,
		AvatarURL:     stored.AvatarURL,
		Scopes:        stored.Scopes,
		ConnectedAt:   stored.ConnectedAt,
		MissingScopes: stored.MissingScopes,
	}, true, nil
}

// exchange turns the authorisation code into an access token.
type exchangeResponse struct {
	AccessToken      string `json:"access_token"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (s *Service) exchange(ctx context.Context, code string) (string, []string, error) {
	form := url.Values{
		"client_id":     {s.opts.ClientID},
		"client_secret": {s.opts.ClientSecret},
		"code":          {code},
		"redirect_uri":  {s.opts.RedirectURI},
	}
	endpoint := strings.TrimSuffix(s.opts.WebURL, "/") + "/login/oauth/access_token"

	ctx, cancel := context.WithTimeout(ctx, s.opts.RequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", nil, &UpstreamError{Resource: "oauth/access_token", Err: err}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := s.http.Do(request)
	if err != nil {
		return "", nil, &UpstreamError{Resource: "oauth/access_token", Err: err}
	}
	defer response.Body.Close()

	var decoded exchangeResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return "", nil, &UpstreamError{
			Resource: "oauth/access_token", Code: response.StatusCode, Err: err,
		}
	}
	// GitHub answers a refused exchange with 200 and an error field, so the status
	// code alone would read this as a success with an empty token.
	if decoded.Error != "" {
		return "", nil, &UpstreamError{
			Resource: "oauth/access_token",
			Code:     response.StatusCode,
			Message:  firstNonEmpty(decoded.ErrorDescription, decoded.Error),
		}
	}
	if decoded.AccessToken == "" {
		return "", nil, &UpstreamError{
			Resource: "oauth/access_token",
			Code:     response.StatusCode,
			Message:  "the exchange returned no access token",
		}
	}
	return decoded.AccessToken, splitScopes(decoded.Scope), nil
}

func splitScopes(scope string) []string {
	fields := strings.FieldsFunc(scope, func(r rune) bool { return r == ',' || r == ' ' })
	scopes := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			scopes = append(scopes, trimmed)
		}
	}
	return scopes
}

// missingScopes is what the grant lacks.
//
// `repo` implies its narrower children, which is why this compares prefixes
// rather than strings: a token with `repo` holds `repo:status` too, and reporting
// that as missing would be wrong.
//
// The result is empty rather than nil when nothing is missing. pgx encodes a nil
// slice as SQL NULL, and missing_scopes is TEXT[] NOT NULL, so a nil here made the
// insert fail — on exactly the grants that were complete. The JSON tag carries
// omitempty, so an empty slice is still absent from the wire.
func missingScopes(wanted, granted []string) []string {
	held := map[string]bool{}
	for _, scope := range granted {
		held[scope] = true
	}
	missing := []string{}
	for _, scope := range wanted {
		if held[scope] {
			continue
		}
		if root := strings.SplitN(scope, ":", 2)[0]; held[root] {
			continue
		}
		missing = append(missing, scope)
	}
	return missing
}
