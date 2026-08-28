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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
)

// What ODE stores about a repository, which is deliberately almost nothing.
//
// Two kinds of row: the GitHub credential, one per developer, and a workbench —
// a working context, of which a developer has as many as they have operators in
// flight. The working copy, its history and its contents are on the PVC and in
// GitHub, which is the whole point of §5.6 — a store on this side would be a third
// copy of state that is already authoritative somewhere else.
//
// Both have different durability requirements and the same home, unlike the
// profiler's split (§5.4.3): losing either is not recoverable by recomputation.
// A lost credential means every developer reconnects; a lost workbench means every
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

	// PutWorkbench writes one, whether it is new or a link that changed. It is the
	// place the one-repository-per-workbench rule is enforced for good: a store
	// backed by Postgres has a unique index behind it, so two requests racing to
	// select the same repository cannot both win.
	PutWorkbench(ctx context.Context, bench Workbench) error
	Workbench(ctx context.Context, id string) (Workbench, bool, error)
	// Workbenches lists a developer's, oldest first.
	Workbenches(ctx context.Context, userSub string) ([]Workbench, error)
	DeleteWorkbench(ctx context.Context, id string) error

	// GetLegacyLink reads the pre-workbench row of a developer who has not had one
	// adopted yet: it is read to take a checkout that is already on their PVC into
	// their first workbench, and answers not-found both for everyone who arrived
	// after workbenches existed and for everyone whose row is already spent.
	GetLegacyLink(ctx context.Context, userSub string) (Link, bool, error)
	// MarkLegacyAdopted spends that row, which is what makes the adoption happen
	// once per developer rather than once per empty workbench list. Without it, a
	// developer who closes their only workbench is handed the old link back as a
	// new one on their next request.
	MarkLegacyAdopted(ctx context.Context, userSub string, at time.Time) error
}

// MemoryStore is the store a deployment without a database gets.
type MemoryStore struct {
	mux        sync.RWMutex
	identities map[string]StoredIdentity
	benches    map[string]Workbench
	// legacy stands in for ode_repo_links so the adoption path can be exercised
	// without a database. Nothing but a test writes it.
	legacy map[string]Link
	// adopted is that row's adopted_at column: the developers whose legacy link has
	// already become a workbench and must not become a second one.
	adopted map[string]bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		identities: map[string]StoredIdentity{},
		benches:    map[string]Workbench{},
		legacy:     map[string]Link{},
		adopted:    map[string]bool{},
	}
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

func (s *MemoryStore) PutWorkbench(_ context.Context, bench Workbench) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	// The unique index the Postgres store has, by hand, so a test finds the same
	// refusal a deployment would.
	for id, existing := range s.benches {
		if id == bench.ID || existing.UserSub != bench.UserSub || existing.Link.FullName == "" {
			continue
		}
		if strings.EqualFold(existing.Link.FullName, bench.Link.FullName) {
			return fmt.Errorf("%w: %s is open in %q",
				ErrRepositoryInUse, bench.Link.FullName, existing.Label())
		}
	}
	s.benches[bench.ID] = bench
	return nil
}

func (s *MemoryStore) Workbench(_ context.Context, id string) (Workbench, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	bench, found := s.benches[id]
	return bench, found, nil
}

func (s *MemoryStore) Workbenches(_ context.Context, userSub string) ([]Workbench, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	var benches []Workbench
	for _, bench := range s.benches {
		if bench.UserSub == userSub {
			benches = append(benches, bench)
		}
	}
	sort.Slice(benches, func(i, j int) bool {
		if benches[i].CreatedAt.Equal(benches[j].CreatedAt) {
			return benches[i].ID < benches[j].ID
		}
		return benches[i].CreatedAt.Before(benches[j].CreatedAt)
	})
	return benches, nil
}

func (s *MemoryStore) DeleteWorkbench(_ context.Context, id string) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	delete(s.benches, id)
	return nil
}

func (s *MemoryStore) GetLegacyLink(_ context.Context, userSub string) (Link, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	if s.adopted[userSub] {
		return Link{}, false, nil
	}
	link, found := s.legacy[userSub]
	return link, found, nil
}

// MarkLegacyAdopted spends the row, the way the Postgres store writes adopted_at.
// The time is not kept: nothing reads it here, and the column exists in the
// database for someone reading the table by hand.
func (s *MemoryStore) MarkLegacyAdopted(_ context.Context, userSub string, _ time.Time) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.adopted[userSub] = true
	return nil
}

// PutLegacyLink stages a pre-workbench link. Only a test calls it: it is how the
// adoption path is exercised without a database holding a row from an older
// release.
func (s *MemoryStore) PutLegacyLink(_ context.Context, link Link) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.legacy[link.UserSub] = link
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

// workbenchColumns is the read list, in the order scanWorkbench takes them.
const workbenchColumns = `id, user_sub, title, full_name, name, owner, default_branch,
       private, clone_url, html_url, path, operator_lib_ref, scaffolded_at,
       selected_at, created_at, last_used_at`

func (s *PostgresStore) PutWorkbench(ctx context.Context, bench Workbench) error {
	link := bench.Link
	_, err := s.db.Pool().Exec(ctx, `
INSERT INTO ode_workbenches
    (id, user_sub, title, full_name, name, owner, default_branch, private, clone_url,
     html_url, path, operator_lib_ref, scaffolded_at, selected_at, created_at, last_used_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (id) DO UPDATE SET
    title = EXCLUDED.title, full_name = EXCLUDED.full_name, name = EXCLUDED.name,
    owner = EXCLUDED.owner, default_branch = EXCLUDED.default_branch,
    private = EXCLUDED.private, clone_url = EXCLUDED.clone_url,
    html_url = EXCLUDED.html_url, path = EXCLUDED.path,
    operator_lib_ref = EXCLUDED.operator_lib_ref, scaffolded_at = EXCLUDED.scaffolded_at,
    selected_at = EXCLUDED.selected_at, last_used_at = EXCLUDED.last_used_at`,
		bench.ID, bench.UserSub, bench.Title, link.FullName, link.Name, link.Owner,
		link.DefaultBranch, link.Private, link.CloneURL, link.HTMLURL, link.Path,
		link.OperatorLibRef, link.ScaffoldedAt, nullTime(link.SelectedAt),
		bench.CreatedAt, bench.LastUsedAt)

	// The unique index doing its job: two requests raced to select one repository
	// into two workbenches, or a second ODE did. Reported as the rule refusing
	// rather than as a database error, because that is what happened.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %s is open in another workbench",
			ErrRepositoryInUse, link.FullName)
	}
	return err
}

// uniqueViolation is Postgres' SQLSTATE for a broken unique constraint.
const uniqueViolation = "23505"

func (s *PostgresStore) Workbench(ctx context.Context, id string) (Workbench, bool, error) {
	row := s.db.Pool().QueryRow(ctx,
		`SELECT `+workbenchColumns+` FROM ode_workbenches WHERE id = $1`, id)
	bench, err := scanWorkbench(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workbench{}, false, nil
	}
	if err != nil {
		return Workbench{}, false, err
	}
	return bench, true, nil
}

func (s *PostgresStore) Workbenches(ctx context.Context, userSub string) ([]Workbench, error) {
	rows, err := s.db.Pool().Query(ctx,
		`SELECT `+workbenchColumns+` FROM ode_workbenches
         WHERE user_sub = $1 ORDER BY created_at, id`, userSub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var benches []Workbench
	for rows.Next() {
		bench, err := scanWorkbench(rows)
		if err != nil {
			return nil, err
		}
		benches = append(benches, bench)
	}
	return benches, rows.Err()
}

func (s *PostgresStore) DeleteWorkbench(ctx context.Context, id string) error {
	_, err := s.db.Pool().Exec(ctx, `DELETE FROM ode_workbenches WHERE id = $1`, id)
	return err
}

// GetLegacyLink reads the row a pre-workbench release wrote, until it is spent.
//
// adopted_at IS NULL is the whole of the once-per-developer rule as far as reading
// goes: a row that has already become a workbench is invisible here, so an empty
// workbench list later — the developer closed the only one — is an empty list and
// not an invitation to hand the old link back.
func (s *PostgresStore) GetLegacyLink(ctx context.Context, userSub string) (Link, bool, error) {
	link := Link{UserSub: userSub}
	err := s.db.Pool().QueryRow(ctx, `
SELECT full_name, name, owner, default_branch, private, clone_url, html_url,
       path, operator_lib_ref, scaffolded_at, selected_at
FROM ode_repo_links WHERE user_sub = $1 AND adopted_at IS NULL`, userSub).Scan(
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

// MarkLegacyAdopted records that the row has become a workbench.
//
// The row itself stays. Dropping it would be the tidier write, but a deployment
// that rolls back to a release without workbenches reads ode_repo_links as its
// only record of what the developer was working on, and a column an older release
// does not select costs that release nothing.
func (s *PostgresStore) MarkLegacyAdopted(
	ctx context.Context, userSub string, at time.Time,
) error {
	_, err := s.db.Pool().Exec(ctx,
		`UPDATE ode_repo_links SET adopted_at = $2 WHERE user_sub = $1`, userSub, at)
	return err
}

// scannable is what pgx's Row and Rows have in common, so one scan serves the
// single read and the listing.
type scannable interface {
	Scan(dest ...any) error
}

func scanWorkbench(row scannable) (Workbench, error) {
	var bench Workbench
	var selected *time.Time
	err := row.Scan(
		&bench.ID, &bench.UserSub, &bench.Title, &bench.Link.FullName, &bench.Link.Name,
		&bench.Link.Owner, &bench.Link.DefaultBranch, &bench.Link.Private,
		&bench.Link.CloneURL, &bench.Link.HTMLURL, &bench.Link.Path,
		&bench.Link.OperatorLibRef, &bench.Link.ScaffoldedAt, &selected,
		&bench.CreatedAt, &bench.LastUsedAt)
	if err != nil {
		return Workbench{}, err
	}
	if selected != nil {
		bench.Link.SelectedAt = *selected
	}
	bench.Link.UserSub = bench.UserSub
	bench.Link.WorkbenchID = bench.ID
	return bench, nil
}

// nullTime keeps an unselected workbench's selected_at NULL rather than year zero,
// which Postgres cannot store in a timestamptz anyway.
func nullTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}
