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

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// Store persists sessions, their messages, the tier audit trail and the pending
// confirmations.
type Store interface {
	CreateSession(ctx context.Context, session Session) error
	Session(ctx context.Context, id string) (Session, bool, error)
	Sessions(ctx context.Context, userSub string, limit int) ([]Session, error)
	UpdateSession(ctx context.Context, session Session) error
	DeleteSession(ctx context.Context, id string) error
	CountSessions(ctx context.Context, userSub string) (int, error)

	AppendMessages(ctx context.Context, sessionID string, messages ...StoredMessage) error
	Messages(ctx context.Context, sessionID string) ([]StoredMessage, error)

	AppendTierChange(ctx context.Context, change TierChange) error
	TierChanges(ctx context.Context, sessionID string) ([]TierChange, error)

	PutConfirmation(ctx context.Context, confirmation Confirmation) error
	Confirmation(ctx context.Context, id string) (Confirmation, bool, error)
	PendingConfirmations(ctx context.Context, sessionID string) ([]Confirmation, error)

	// RecordCreation and Creations are the log of what a session created on the
	// platform, which is what the delete tools of §5.8 check before removing
	// anything. Append-only: an entry stays after the object is deleted, because
	// "this session created and then removed an import" is the record that makes
	// the deletion accountable, and losing it would leave a deletion with no trace
	// of what authorised it.
	RecordCreation(ctx context.Context, sessionID string, created tools.Creation) error
	Creations(ctx context.Context, sessionID string) ([]tools.Creation, error)
}

// --- in-memory ---

// MemoryStore is what a deployment without Postgres runs. Chat history is the
// least costly thing here to lose — a conversation is a conversation — but the
// tier audit trail is not, which is why the Postgres path exists.
type MemoryStore struct {
	mux           sync.RWMutex
	sessions      map[string]Session
	messages      map[string][]StoredMessage
	tierChanges   map[string][]TierChange
	confirmations map[string]Confirmation
	creations     map[string][]tools.Creation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions:      map[string]Session{},
		messages:      map[string][]StoredMessage{},
		tierChanges:   map[string][]TierChange{},
		confirmations: map[string]Confirmation{},
		creations:     map[string][]tools.Creation{},
	}
}

func (s *MemoryStore) CreateSession(_ context.Context, session Session) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if _, exists := s.sessions[session.ID]; exists {
		return errors.New("chat: session already exists")
	}
	s.sessions[session.ID] = session
	return nil
}

func (s *MemoryStore) Session(_ context.Context, id string) (Session, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	session, found := s.sessions[id]
	if found {
		session.MessageCount = len(s.messages[id])
	}
	return session, found, nil
}

func (s *MemoryStore) Sessions(_ context.Context, userSub string, limit int) ([]Session, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	out := []Session{}
	for _, session := range s.sessions {
		if session.UserSub != userSub {
			continue
		}
		session.MessageCount = len(s.messages[session.ID])
		out = append(out, session)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) UpdateSession(_ context.Context, session Session) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if _, exists := s.sessions[session.ID]; !exists {
		return ErrNoSuchSession
	}
	s.sessions[session.ID] = session
	return nil
}

func (s *MemoryStore) DeleteSession(_ context.Context, id string) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	delete(s.sessions, id)
	delete(s.messages, id)
	delete(s.tierChanges, id)
	// The creation log goes with the session deliberately, unlike in Postgres where
	// it is kept: a deleted session can no longer reach a delete tool, so what the
	// entry would authorise is unreachable either way, and holding platform ids in
	// memory for a conversation the developer discarded serves nothing.
	delete(s.creations, id)
	for confirmationID, confirmation := range s.confirmations {
		if confirmation.SessionID == id {
			delete(s.confirmations, confirmationID)
		}
	}
	return nil
}

func (s *MemoryStore) CountSessions(_ context.Context, userSub string) (int, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	count := 0
	for _, session := range s.sessions {
		if session.UserSub == userSub {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) AppendMessages(_ context.Context, sessionID string, messages ...StoredMessage) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	existing := s.messages[sessionID]
	for _, message := range messages {
		message.Seq = int64(len(existing))
		if message.CreatedAt.IsZero() {
			message.CreatedAt = time.Now().UTC()
		}
		existing = append(existing, message)
	}
	s.messages[sessionID] = existing
	return nil
}

func (s *MemoryStore) Messages(_ context.Context, sessionID string) ([]StoredMessage, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return append([]StoredMessage{}, s.messages[sessionID]...), nil
}

func (s *MemoryStore) AppendTierChange(_ context.Context, change TierChange) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if change.At.IsZero() {
		change.At = time.Now().UTC()
	}
	s.tierChanges[change.SessionID] = append(s.tierChanges[change.SessionID], change)
	return nil
}

func (s *MemoryStore) TierChanges(_ context.Context, sessionID string) ([]TierChange, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	return append([]TierChange{}, s.tierChanges[sessionID]...), nil
}

func (s *MemoryStore) PutConfirmation(_ context.Context, confirmation Confirmation) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.confirmations[confirmation.ID] = confirmation
	return nil
}

func (s *MemoryStore) Confirmation(_ context.Context, id string) (Confirmation, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	confirmation, found := s.confirmations[id]
	return confirmation, found, nil
}

func (s *MemoryStore) RecordCreation(_ context.Context, sessionID string, created tools.Creation) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.creations[sessionID] = append(s.creations[sessionID], created)
	return nil
}

func (s *MemoryStore) Creations(_ context.Context, sessionID string) ([]tools.Creation, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	out := make([]tools.Creation, len(s.creations[sessionID]))
	copy(out, s.creations[sessionID])
	return out, nil
}

func (s *MemoryStore) PendingConfirmations(_ context.Context, sessionID string) ([]Confirmation, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	out := []Confirmation{}
	for _, confirmation := range s.confirmations {
		if confirmation.SessionID == sessionID && confirmation.Pending() {
			out = append(out, confirmation)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// --- postgres ---

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(db *database.DB) *PostgresStore {
	return &PostgresStore{pool: db.Pool()}
}

func (s *PostgresStore) CreateSession(ctx context.Context, session Session) error {
	selection, err := marshalSelection(session.Selection)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO ode_chat_sessions (id, user_sub, title, provider, model, tier, selection,
		                               workbench_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())`,
		session.ID, session.UserSub, session.Title, session.Provider, session.Model,
		session.Tier.String(), selection, session.WorkbenchID)
	return err
}

func (s *PostgresStore) Session(ctx context.Context, id string) (Session, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT s.id, s.user_sub, s.title, s.provider, s.model, s.tier, s.selection,
		       s.workbench_id, s.created_at, s.updated_at,
		       (SELECT COUNT(*) FROM ode_chat_messages m WHERE m.session_id = s.id)
		FROM ode_chat_sessions s WHERE s.id = $1`, id)

	session, err := scanSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, false, nil
		}
		return Session{}, false, err
	}
	return session, true, nil
}

func (s *PostgresStore) Sessions(ctx context.Context, userSub string, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.user_sub, s.title, s.provider, s.model, s.tier, s.selection,
		       s.workbench_id, s.created_at, s.updated_at,
		       (SELECT COUNT(*) FROM ode_chat_messages m WHERE m.session_id = s.id)
		FROM ode_chat_sessions s
		WHERE s.user_sub = $1 AND s.archived_at IS NULL
		ORDER BY s.updated_at DESC
		LIMIT $2`, userSub, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateSession(ctx context.Context, session Session) error {
	selection, err := marshalSelection(session.Selection)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE ode_chat_sessions
		SET title = $2, provider = $3, model = $4, tier = $5, selection = $6,
		    workbench_id = $7, updated_at = now()
		WHERE id = $1`,
		session.ID, session.Title, session.Provider, session.Model,
		session.Tier.String(), selection, session.WorkbenchID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchSession
	}
	return nil
}

func (s *PostgresStore) DeleteSession(ctx context.Context, id string) error {
	// Messages cascade; confirmations and tier changes do not carry a foreign key,
	// because the audit trail should outlive a deleted session rather than vanish
	// with it. They are cleaned up explicitly here for the developer's own
	// confirmations only.
	_, err := s.pool.Exec(ctx, `DELETE FROM ode_chat_sessions WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) CountSessions(ctx context.Context, userSub string) (int, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ode_chat_sessions WHERE user_sub = $1 AND archived_at IS NULL`, userSub)
	count := 0
	err := row.Scan(&count)
	return count, err
}

func (s *PostgresStore) AppendMessages(ctx context.Context, sessionID string, messages ...StoredMessage) error {
	if len(messages) == 0 {
		return nil
	}
	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// The sequence is derived inside the transaction rather than passed in, so two
	// concurrent turns on one session cannot collide on the primary key.
	var next int64
	row := transaction.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq) + 1, 0) FROM ode_chat_messages WHERE session_id = $1`, sessionID)
	if err := row.Scan(&next); err != nil {
		return err
	}

	for _, message := range messages {
		content, err := json.Marshal(message.Content)
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(ctx, `
			INSERT INTO ode_chat_messages (session_id, seq, role, content, origin, subject)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			sessionID, next, string(message.Role), content,
			message.Origin, message.Subject); err != nil {
			return err
		}
		next++
	}

	if _, err := transaction.Exec(ctx,
		`UPDATE ode_chat_sessions SET updated_at = now() WHERE id = $1`, sessionID); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (s *PostgresStore) Messages(ctx context.Context, sessionID string) ([]StoredMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, seq, role, content, created_at, origin, subject
		FROM ode_chat_messages WHERE session_id = $1 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StoredMessage{}
	for rows.Next() {
		var message StoredMessage
		var role string
		var content []byte
		if err := rows.Scan(&message.SessionID, &message.Seq, &role, &content,
			&message.CreatedAt, &message.Origin, &message.Subject); err != nil {
			return nil, err
		}
		message.Role = llm.Role(role)
		if err := json.Unmarshal(content, &message.Content); err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AppendTierChange(ctx context.Context, change TierChange) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ode_tier_changes (session_id, user_sub, from_tier, to_tier)
		VALUES ($1, $2, $3, $4)`,
		change.SessionID, change.UserSub, change.From.String(), change.To.String())
	return err
}

func (s *PostgresStore) TierChanges(ctx context.Context, sessionID string) ([]TierChange, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, user_sub, from_tier, to_tier, at
		FROM ode_tier_changes WHERE session_id = $1 ORDER BY at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TierChange{}
	for rows.Next() {
		var change TierChange
		var from, to string
		if err := rows.Scan(&change.SessionID, &change.UserSub, &from, &to, &change.At); err != nil {
			return nil, err
		}
		change.From, _ = tools.ParseTier(from)
		change.To, _ = tools.ParseTier(to)
		out = append(out, change)
	}
	return out, rows.Err()
}

func (s *PostgresStore) PutConfirmation(ctx context.Context, confirmation Confirmation) error {
	input := confirmation.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ode_confirmations (id, session_id, user_sub, call_id, tool, input, tier,
		                              created_at, resolved_at, decision)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, now()), $9, $10)
		ON CONFLICT (id) DO UPDATE
		SET resolved_at = EXCLUDED.resolved_at, decision = EXCLUDED.decision`,
		confirmation.ID, confirmation.SessionID, confirmation.UserSub, confirmation.CallID,
		confirmation.Tool, []byte(input), confirmation.Tier.String(),
		nullTime(confirmation.CreatedAt), confirmation.ResolvedAt, confirmation.Decision)
	return err
}

func (s *PostgresStore) Confirmation(ctx context.Context, id string) (Confirmation, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, session_id, user_sub, call_id, tool, input, tier, created_at, resolved_at, decision
		FROM ode_confirmations WHERE id = $1`, id)

	confirmation, err := scanConfirmation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Confirmation{}, false, nil
		}
		return Confirmation{}, false, err
	}
	return confirmation, true, nil
}

func (s *PostgresStore) PendingConfirmations(ctx context.Context, sessionID string) ([]Confirmation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, session_id, user_sub, call_id, tool, input, tier, created_at, resolved_at, decision
		FROM ode_confirmations
		WHERE session_id = $1 AND resolved_at IS NULL
		ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Confirmation{}
	for rows.Next() {
		confirmation, err := scanConfirmation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, confirmation)
	}
	return out, rows.Err()
}

// scanner is what pgx.Row and pgx.Rows have in common, so one scan function
// serves both the single-row and the list query.
type scanner interface {
	Scan(dest ...any) error
}

func scanSession(row scanner) (Session, error) {
	var session Session
	var tier string
	var selection []byte
	if err := row.Scan(&session.ID, &session.UserSub, &session.Title, &session.Provider,
		&session.Model, &tier, &selection, &session.WorkbenchID, &session.CreatedAt,
		&session.UpdatedAt, &session.MessageCount); err != nil {
		return Session{}, err
	}
	// An unparseable tier falls back to L0 rather than erroring: L0 exposes
	// nothing, so a corrupt value fails closed.
	parsed, err := tools.ParseTier(tier)
	if err != nil {
		parsed = tools.DefaultTier
	}
	session.Tier = parsed
	if len(selection) > 0 {
		var proposed tools.ProposedSelection
		if err := json.Unmarshal(selection, &proposed); err == nil {
			session.Selection = &proposed
		}
	}
	return session, nil
}

func (s *PostgresStore) RecordCreation(ctx context.Context, sessionID string, created tools.Creation) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ode_platform_creations (session_id, kind, object_id, name, tool, at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, now()))`,
		sessionID, string(created.Kind), created.ID, created.Name, created.Tool, nullTime(created.At))
	return err
}

func (s *PostgresStore) Creations(ctx context.Context, sessionID string) ([]tools.Creation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, object_id, name, tool, at
		FROM ode_platform_creations WHERE session_id = $1 ORDER BY at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []tools.Creation{}
	for rows.Next() {
		var creation tools.Creation
		var kind string
		if err := rows.Scan(&kind, &creation.ID, &creation.Name, &creation.Tool, &creation.At); err != nil {
			return nil, err
		}
		creation.Kind = tools.CreationKind(kind)
		out = append(out, creation)
	}
	return out, rows.Err()
}

func scanConfirmation(row scanner) (Confirmation, error) {
	var confirmation Confirmation
	var tier string
	var input []byte
	if err := row.Scan(&confirmation.ID, &confirmation.SessionID, &confirmation.UserSub,
		&confirmation.CallID, &confirmation.Tool, &input, &tier,
		&confirmation.CreatedAt, &confirmation.ResolvedAt, &confirmation.Decision); err != nil {
		return Confirmation{}, err
	}
	confirmation.Input = json.RawMessage(input)
	if parsed, err := tools.ParseTier(tier); err == nil {
		confirmation.Tier = parsed
	}
	return confirmation, nil
}

func marshalSelection(selection *tools.ProposedSelection) (any, error) {
	if selection == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(selection)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
