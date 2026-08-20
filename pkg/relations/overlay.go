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

package relations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
)

// PostgresDecisions persists the rule decision log.
//
// The Store interface takes no context, which is right for the in-memory
// implementation and awkward here — so each call gets its own bounded context,
// exactly as PostgresOverrides does. The bound matters: a wedged database must not
// hang a relational pass, and these are single-column indexed queries.
type PostgresDecisions struct {
	pool *pgxpool.Pool
	ids  interface{ NewID() string }
}

const decisionQueryTimeout = 10 * time.Second

func NewPostgresDecisions(db *database.DB) *PostgresDecisions {
	return &PostgresDecisions{pool: db.Pool(), ids: &decisionIDs{}}
}

func (s *PostgresDecisions) Append(decision RuleDecision) (RuleDecision, error) {
	if decision.DecisionID == "" {
		decision.DecisionID = s.ids.NewID()
	}
	if decision.CreatedAt.IsZero() {
		decision.CreatedAt = time.Now().UTC()
	}

	encoded, err := json.Marshal(decision)
	if err != nil {
		return RuleDecision{}, fmt.Errorf("relations: encoding a rule decision: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), decisionQueryTimeout)
	defer cancel()

	// The whole decision is one document *and* its lookup key is a column, for the
	// reason the override table gives: the column is what the query uses, the
	// document is what survives a change to the schema, so an old confirmation stays
	// readable without a migration to keep its meaning.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO ode_relation_rule_decisions (id, rule_id, created_by, decision, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		decision.DecisionID, decision.RuleID, decision.CreatedBy, encoded, decision.CreatedAt); err != nil {
		return RuleDecision{}, fmt.Errorf("relations: storing a rule decision: %w", err)
	}
	return decision, nil
}

// ForRules reads the log for a set of rules, oldest first.
//
// A read failure returns nothing rather than an error, because the interface has
// nowhere to put one, and that is the wrong direction to fail in — the relation
// profile is then served as though nobody had ever decided anything. It is logged
// so it is visible rather than silent, and the alternative, failing the whole pass
// when the database blips, is worse for the developer in front of it. The log levels
// follow the same rule as the profiler's overlay: WARN for what the next read
// recovers from, ERROR only for a stored row that stays broken until a human looks.
func (s *PostgresDecisions) ForRules(ruleIDs []string) map[string][]RuleDecision {
	out := map[string][]RuleDecision{}
	if len(ruleIDs) == 0 {
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), decisionQueryTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT decision FROM ode_relation_rule_decisions
		WHERE rule_id = ANY($1)
		ORDER BY created_at, id`, ruleIDs)
	if err != nil {
		slog.Warn("could not read the relation rule decision log; the relation profile will be "+
			"served without the developer's decisions", "rules", len(ruleIDs), "error", err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			slog.Warn("could not scan a rule decision", "error", err)
			return out
		}
		var decision RuleDecision
		if err := json.Unmarshal(encoded, &decision); err != nil {
			slog.Error("a stored rule decision could not be decoded", "error", err)
			continue
		}
		out[decision.RuleID] = append(out[decision.RuleID], decision)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("reading the relation rule decision log failed part-way", "error", err)
	}
	return out
}

// decisionIDs mints decision ids. Random rather than time-derived, for the reason
// overrideIDs gives: two decisions submitted in the same instant would collide on
// the primary key, and the clock is not a source of uniqueness under concurrency.
type decisionIDs struct{}

func (d *decisionIDs) NewID() string {
	buffer := make([]byte, 12)
	_, _ = rand.Read(buffer)
	return "dec-" + hex.EncodeToString(buffer)
}
