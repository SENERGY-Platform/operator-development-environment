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
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
)

// What ODE stores about a repository, which is deliberately almost nothing.
//
// Two rows per developer: the GitHub credential, and which repository they are
// working on. The working copy, its history and its contents are on the PVC and in
// GitHub, which is the whole point of §5.6 — a store on this side would be a third
// copy of state that is already authoritative somewhere else.
//
// The two have different durability requirements and the same home, unlike the
// profiler's split (§5.4.3): losing either is not recoverable by recomputation.
// A lost credential means every developer reconnects; a lost link means every
// developer re-selects a repository whose checkout is still sitting on their PVC.
// Neither is data loss, both are avoidable, so both go to Postgres when there is
// one.

// StoredIdentity is the credential row. SealedToken is the only sensitive field
// and it never leaves this package unsealed.
type StoredIdentity struct {
	UserSub       string
	Login         string
	Name          string
	AvatarURL     string
	Scopes        []string
	MissingScopes []string
	SealedToken   string
	ConnectedAt   time.Time
}

// Store is what the service needs to remember.
type Store interface {
	PutIdentity(ctx context.Context, identity StoredIdentity) error
	GetIdentity(ctx context.Context, userSub string) (StoredIdentity, bool, error)
	DeleteIdentity(ctx context.Context, userSub string) error

	PutLink(ctx context.Context, link Link) error
	GetLink(ctx context.Context, userSub string) (Link, bool, error)
	DeleteLink(ctx context.Context, userSub string) error
}

// MemoryStore is the store a deployment without a database gets.
type MemoryStore struct {
	mux        sync.RWMutex
	identities map[string]StoredIdentity
	links      map[string]Link
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{identities: map[string]StoredIdentity{}, links: map[string]Link{}}
}

func (s *MemoryStore) PutIdentity(_ context.Context, identity StoredIdentity) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.identities[identity.UserSub] = identity
	return nil
}

func (s *MemoryStore) GetIdentity(_ context.Context, userSub string) (StoredIdentity, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	identity, found := s.identities[userSub]
	return identity, found, nil
}

func (s *MemoryStore) DeleteIdentity(_ context.Context, userSub string) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	delete(s.identities, userSub)
	return nil
}

func (s *MemoryStore) PutLink(_ context.Context, link Link) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.links[link.UserSub] = link
	return nil
}

func (s *MemoryStore) GetLink(_ context.Context, userSub string) (Link, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	link, found := s.links[userSub]
	return link, found, nil
}

func (s *MemoryStore) DeleteLink(_ context.Context, userSub string) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	delete(s.links, userSub)
	return nil
}

// PostgresStore is the same two rows, in the database.
type PostgresStore struct {
	db *database.DB
}

func NewPostgresStore(db *database.DB) *PostgresStore { return &PostgresStore{db: db} }

func (s *PostgresStore) PutIdentity(ctx context.Context, identity StoredIdentity) error {
	_, err := s.db.Pool().Exec(ctx, `
INSERT INTO ode_github_identities
    (user_sub, login, name, avatar_url, scopes, missing_scopes, sealed_token, connected_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_sub) DO UPDATE SET
    login = EXCLUDED.login, name = EXCLUDED.name, avatar_url = EXCLUDED.avatar_url,
    scopes = EXCLUDED.scopes, missing_scopes = EXCLUDED.missing_scopes,
    sealed_token = EXCLUDED.sealed_token, connected_at = EXCLUDED.connected_at`,
		identity.UserSub, identity.Login, identity.Name, identity.AvatarURL,
		identity.Scopes, identity.MissingScopes, identity.SealedToken, identity.ConnectedAt)
	return err
}

func (s *PostgresStore) GetIdentity(
	ctx context.Context, userSub string,
) (StoredIdentity, bool, error) {
	identity := StoredIdentity{UserSub: userSub}
	err := s.db.Pool().QueryRow(ctx, `
SELECT login, name, avatar_url, scopes, missing_scopes, sealed_token, connected_at
FROM ode_github_identities WHERE user_sub = $1`, userSub).Scan(
		&identity.Login, &identity.Name, &identity.AvatarURL, &identity.Scopes,
		&identity.MissingScopes, &identity.SealedToken, &identity.ConnectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredIdentity{}, false, nil
	}
	if err != nil {
		return StoredIdentity{}, false, err
	}
	return identity, true, nil
}

func (s *PostgresStore) DeleteIdentity(ctx context.Context, userSub string) error {
	_, err := s.db.Pool().Exec(ctx,
		`DELETE FROM ode_github_identities WHERE user_sub = $1`, userSub)
	return err
}

func (s *PostgresStore) PutLink(ctx context.Context, link Link) error {
	_, err := s.db.Pool().Exec(ctx, `
INSERT INTO ode_repo_links
    (user_sub, full_name, name, owner, default_branch, private, clone_url, html_url,
     path, operator_lib_ref, scaffolded_at, selected_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (user_sub) DO UPDATE SET
    full_name = EXCLUDED.full_name, name = EXCLUDED.name, owner = EXCLUDED.owner,
    default_branch = EXCLUDED.default_branch, private = EXCLUDED.private,
    clone_url = EXCLUDED.clone_url, html_url = EXCLUDED.html_url, path = EXCLUDED.path,
    operator_lib_ref = EXCLUDED.operator_lib_ref, scaffolded_at = EXCLUDED.scaffolded_at,
    selected_at = EXCLUDED.selected_at`,
		link.UserSub, link.FullName, link.Name, link.Owner, link.DefaultBranch, link.Private,
		link.CloneURL, link.HTMLURL, link.Path, link.OperatorLibRef, link.ScaffoldedAt,
		link.SelectedAt)
	return err
}

func (s *PostgresStore) GetLink(ctx context.Context, userSub string) (Link, bool, error) {
	link := Link{UserSub: userSub}
	err := s.db.Pool().QueryRow(ctx, `
SELECT full_name, name, owner, default_branch, private, clone_url, html_url,
       path, operator_lib_ref, scaffolded_at, selected_at
FROM ode_repo_links WHERE user_sub = $1`, userSub).Scan(
		&link.FullName, &link.Name, &link.Owner, &link.DefaultBranch, &link.Private,
		&link.CloneURL, &link.HTMLURL, &link.Path, &link.OperatorLibRef,
		&link.ScaffoldedAt, &link.SelectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	return link, true, nil
}

func (s *PostgresStore) DeleteLink(ctx context.Context, userSub string) error {
	_, err := s.db.Pool().Exec(ctx, `DELETE FROM ode_repo_links WHERE user_sub = $1`, userSub)
	return err
}
