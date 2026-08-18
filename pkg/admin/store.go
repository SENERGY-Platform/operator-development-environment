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

package admin

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// Store keeps limits, accounting and the tool audit trail.
type Store interface {
	Limits(ctx context.Context, subject string) (LimitsRecord, bool, error)
	PutLimits(ctx context.Context, record LimitsRecord) error
	AllLimits(ctx context.Context) ([]LimitsRecord, error)

	AppendUsage(ctx context.Context, record Record) error
	// SpendSince is the accounting query the caps are enforced against. An empty
	// subject means every user, which is how the global cap is computed.
	SpendSince(ctx context.Context, subject string, since time.Time) (Spend, error)
	// UsageSince lists the individual records, for the admin surface.
	UsageSince(ctx context.Context, subject string, since time.Time, limit int) ([]Record, error)
	// UnpricedModelsSince names models used with no price, so a cost cap that
	// cannot bind says so instead of reading as zero spend.
	UnpricedModelsSince(ctx context.Context, subject string, since time.Time) ([]string, error)

	AppendToolCall(ctx context.Context, record tools.ToolCallRecord) error
	ToolCallsSince(ctx context.Context, subject string, since time.Time, limit int) ([]tools.ToolCallRecord, error)
}

// --- in-memory ---

// MemoryStore is the store a deployment without Postgres runs.
//
// It is correct and it is not durable, and the difference matters more here than
// it did for the profiler: a spend cap computed from this store is only as old as
// the process, so a restart hands every user a fresh allowance. pkg.Start warns
// about exactly that. It stays the test store, which is what keeps the suite free
// of containers.
type MemoryStore struct {
	mux    sync.RWMutex
	limits map[string]LimitsRecord
	usage  []Record
	calls  []tools.ToolCallRecord
	// priced records which models had a price at the time each record was written,
	// so UnpricedModelsSince can answer without a pricing table of its own.
	unpriced map[string]bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		limits:   map[string]LimitsRecord{},
		unpriced: map[string]bool{},
	}
}

func (s *MemoryStore) Limits(_ context.Context, subject string) (LimitsRecord, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	record, found := s.limits[subject]
	return record, found, nil
}

func (s *MemoryStore) PutLimits(_ context.Context, record LimitsRecord) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	s.limits[record.Subject] = record
	return nil
}

func (s *MemoryStore) AllLimits(_ context.Context) ([]LimitsRecord, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	out := make([]LimitsRecord, 0, len(s.limits))
	for _, record := range s.limits {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out, nil
}

func (s *MemoryStore) AppendUsage(_ context.Context, record Record) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	s.usage = append(s.usage, record)
	if !record.CostEstimated && record.Model != "" {
		s.unpriced[record.Model] = true
	}
	return nil
}

func (s *MemoryStore) SpendSince(_ context.Context, subject string, since time.Time) (Spend, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	spend := Spend{From: since, To: time.Now().UTC()}
	for _, record := range s.usage {
		if record.At.Before(since) {
			continue
		}
		if subject != "" && record.UserSub != subject {
			continue
		}
		spend.Tokens += record.Tokens()
		spend.Cost += record.Cost
		spend.Requests++
	}
	return spend, nil
}

func (s *MemoryStore) UsageSince(_ context.Context, subject string, since time.Time, limit int) ([]Record, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	out := []Record{}
	// Newest first, which is the order the admin surface reads.
	for i := len(s.usage) - 1; i >= 0; i-- {
		record := s.usage[i]
		if record.At.Before(since) {
			continue
		}
		if subject != "" && record.UserSub != subject {
			continue
		}
		out = append(out, record)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) UnpricedModelsSince(_ context.Context, subject string, since time.Time) ([]string, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	seen := map[string]bool{}
	for _, record := range s.usage {
		if record.At.Before(since) || record.CostEstimated || record.Model == "" {
			continue
		}
		if subject != "" && record.UserSub != subject {
			continue
		}
		seen[record.Model] = true
	}
	out := make([]string, 0, len(seen))
	for model := range seen {
		out = append(out, model)
	}
	sort.Strings(out)
	return out, nil
}

func (s *MemoryStore) AppendToolCall(_ context.Context, record tools.ToolCallRecord) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	s.calls = append(s.calls, record)
	return nil
}

func (s *MemoryStore) ToolCallsSince(_ context.Context, subject string, since time.Time, limit int) ([]tools.ToolCallRecord, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	out := []tools.ToolCallRecord{}
	for i := len(s.calls) - 1; i >= 0; i-- {
		record := s.calls[i]
		if record.At.Before(since) {
			continue
		}
		if subject != "" && record.UserSub != subject {
			continue
		}
		out = append(out, record)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// --- postgres ---

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(db *database.DB) *PostgresStore {
	return &PostgresStore{pool: db.Pool()}
}

func (s *PostgresStore) Limits(ctx context.Context, subject string) (LimitsRecord, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT subject, limits, updated_at, updated_by FROM ode_limits WHERE subject = $1`, subject)

	var record LimitsRecord
	var raw []byte
	if err := row.Scan(&record.Subject, &raw, &record.UpdatedAt, &record.UpdatedBy); err != nil {
		if isNoRows(err) {
			return LimitsRecord{}, false, nil
		}
		return LimitsRecord{}, false, err
	}
	limits, err := unmarshalLimits(raw)
	if err != nil {
		return LimitsRecord{}, false, err
	}
	record.Limits = limits
	return record, true, nil
}

func (s *PostgresStore) PutLimits(ctx context.Context, record LimitsRecord) error {
	encoded, err := record.Limits.marshal()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO ode_limits (subject, limits, updated_at, updated_by)
		VALUES ($1, $2, now(), $3)
		ON CONFLICT (subject) DO UPDATE
		SET limits = EXCLUDED.limits, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		record.Subject, encoded, record.UpdatedBy)
	return err
}

func (s *PostgresStore) AllLimits(ctx context.Context) ([]LimitsRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT subject, limits, updated_at, updated_by FROM ode_limits ORDER BY subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LimitsRecord{}
	for rows.Next() {
		var record LimitsRecord
		var raw []byte
		if err := rows.Scan(&record.Subject, &raw, &record.UpdatedAt, &record.UpdatedBy); err != nil {
			return nil, err
		}
		limits, err := unmarshalLimits(raw)
		if err != nil {
			return nil, err
		}
		record.Limits = limits
		out = append(out, record)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AppendUsage(ctx context.Context, record Record) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ode_usage (user_sub, session_id, provider, model,
		                       input_tokens, output_tokens, cached_input_tokens,
		                       cost, cost_estimated, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10, now()))`,
		record.UserSub, record.SessionID, record.Provider, record.Model,
		record.InputTokens, record.OutputTokens, record.CachedInputTokens,
		record.Cost, record.CostEstimated, nullTime(record.At))
	return err
}

func (s *PostgresStore) SpendSince(ctx context.Context, subject string, since time.Time) (Spend, error) {
	// COALESCE because SUM over no rows is NULL, and a fresh user has no rows.
	row := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(input_tokens + output_tokens + cached_input_tokens), 0),
		       COALESCE(SUM(cost), 0),
		       COUNT(*)
		FROM ode_usage
		WHERE at >= $1 AND ($2 = '' OR user_sub = $2)`, since, subject)

	spend := Spend{From: since, To: time.Now().UTC()}
	if err := row.Scan(&spend.Tokens, &spend.Cost, &spend.Requests); err != nil {
		return Spend{}, err
	}
	return spend, nil
}

func (s *PostgresStore) UsageSince(ctx context.Context, subject string, since time.Time, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT user_sub, session_id, provider, model, input_tokens, output_tokens,
		       cached_input_tokens, cost, cost_estimated, at
		FROM ode_usage
		WHERE at >= $1 AND ($2 = '' OR user_sub = $2)
		ORDER BY at DESC
		LIMIT $3`, since, subject, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var record Record
		if err := rows.Scan(&record.UserSub, &record.SessionID, &record.Provider, &record.Model,
			&record.InputTokens, &record.OutputTokens, &record.CachedInputTokens,
			&record.Cost, &record.CostEstimated, &record.At); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UnpricedModelsSince(ctx context.Context, subject string, since time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT model FROM ode_usage
		WHERE at >= $1 AND ($2 = '' OR user_sub = $2)
		  AND cost_estimated = FALSE AND model <> ''
		ORDER BY model`, since, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		out = append(out, model)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AppendToolCall(ctx context.Context, record tools.ToolCallRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ode_tool_calls (user_sub, session_id, tool, tier, outcome, duration_ms, at)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()))`,
		record.UserSub, record.SessionID, record.Tool, record.Tier.String(),
		string(record.Outcome), record.Duration.Duration().Milliseconds(), nullTime(record.At))
	return err
}

func (s *PostgresStore) ToolCallsSince(ctx context.Context, subject string, since time.Time, limit int) ([]tools.ToolCallRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT user_sub, session_id, tool, tier, outcome, duration_ms, at
		FROM ode_tool_calls
		WHERE at >= $1 AND ($2 = '' OR user_sub = $2)
		ORDER BY at DESC
		LIMIT $3`, since, subject, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []tools.ToolCallRecord{}
	for rows.Next() {
		var record tools.ToolCallRecord
		var tier, outcome string
		var durationMS int64
		if err := rows.Scan(&record.UserSub, &record.SessionID, &record.Tool,
			&tier, &outcome, &durationMS, &record.At); err != nil {
			return nil, err
		}
		// A tier that does not parse is kept rather than dropped: this is an audit
		// trail, and an unreadable row is still evidence the call happened.
		if parsed, err := tools.ParseTier(tier); err == nil {
			record.Tier = parsed
		}
		record.Outcome = tools.Outcome(outcome)
		record.Duration = tools.Millis(time.Duration(durationMS) * time.Millisecond)
		out = append(out, record)
	}
	return out, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
