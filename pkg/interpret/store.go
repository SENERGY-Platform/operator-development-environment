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

package interpret

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
)

// What is persisted here, and what is not.
//
// The split is the one pkg/profiler and pkg/relations make and pkg/pkg.go wires:
// a recomputable artifact stays in memory and a record of a human judgement goes
// to Postgres (§5.4.3). Applied here:
//
//   - The **summary** is recomputable — it is MLflow's params and metrics reduced
//     by a pure function — so losing one costs a read of the tracking server.
//   - The **interpretation** is not recomputable, and it is already durable: it is
//     stored as chat messages in ode_chat_messages, which is where a developer
//     reads it. Keeping a second copy in a table of its own would be two records of
//     one conversation, and they would diverge.
//   - The **decision** is a human judgement and nothing can regenerate it. It goes
//     to Postgres, append-only, keyed by the proposal's fingerprint so it survives
//     the interpretation being recomputed.
//
// So this store holds decisions and nothing else.

// Store is the append-only decision log.
type Store interface {
	// Append records one decision. Implementations validate before writing, so
	// there is no path into the log that skips it.
	Append(ctx context.Context, decision ProposalDecision) (ProposalDecision, error)
	// ForExperiment is every decision about one experiment's proposals, oldest
	// first.
	ForExperiment(ctx context.Context, userSub, experimentID string) ([]ProposalDecision, error)
}

// MemoryStore is what a deployment without Postgres gets.
//
// A real degradation and not a neutral alternative: a restart loses every
// developer's answers, so a proposal they rejected comes back as though they had
// never been asked. pkg/pkg.go's validate() already says what a database-less
// deployment costs; this is one more line of it.
type MemoryStore struct {
	mux       sync.Mutex
	sequence  int
	decisions map[string][]ProposalDecision // by experiment id
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{decisions: map[string][]ProposalDecision{}}
}

func (s *MemoryStore) Append(
	_ context.Context, decision ProposalDecision,
) (ProposalDecision, error) {
	if err := decision.Validate(); err != nil {
		return ProposalDecision{}, err
	}
	s.mux.Lock()
	defer s.mux.Unlock()
	s.sequence++
	if decision.DecisionID == "" {
		decision.DecisionID = "decision-" + strconv.Itoa(s.sequence)
	}
	s.decisions[decision.ExperimentID] = append(s.decisions[decision.ExperimentID], decision)
	return decision, nil
}

func (s *MemoryStore) ForExperiment(
	_ context.Context, userSub, experimentID string,
) ([]ProposalDecision, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	out := make([]ProposalDecision, 0, len(s.decisions[experimentID]))
	for _, decision := range s.decisions[experimentID] {
		// The subject is part of the lookup rather than checked afterwards, for the
		// reason the experiment store puts it in the WHERE clause: a caller cannot
		// forget it, and another developer's decision is never in memory here.
		if decision.CreatedBy == userSub {
			out = append(out, decision)
		}
	}
	sortDecisions(out)
	return out, nil
}

// PostgresStore is the same log, in the database.
type PostgresStore struct{ db *database.DB }

func NewPostgresStore(db *database.DB) *PostgresStore { return &PostgresStore{db: db} }

func (s *PostgresStore) Append(
	ctx context.Context, decision ProposalDecision,
) (ProposalDecision, error) {
	if err := decision.Validate(); err != nil {
		return ProposalDecision{}, err
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return ProposalDecision{}, err
	}
	// The whole record as JSONB beside the four columns that are queried, the same
	// shape ode_relation_rule_decisions uses: the columns are what a lookup needs
	// and the document is what a reader needs, and neither has to be kept in step
	// with a schema change.
	_, err = s.db.Pool().Exec(ctx, `
INSERT INTO ode_proposal_decisions
    (id, experiment_id, proposal_id, created_by, decision, record, created_at)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()))`,
		decision.DecisionID, decision.ExperimentID, decision.ProposalID,
		decision.CreatedBy, decision.Decision, encoded, nullTime(decision.CreatedAt))
	if err != nil {
		return ProposalDecision{}, err
	}
	return decision, nil
}

func (s *PostgresStore) ForExperiment(
	ctx context.Context, userSub, experimentID string,
) ([]ProposalDecision, error) {
	rows, err := s.db.Pool().Query(ctx, `
SELECT record FROM ode_proposal_decisions
WHERE experiment_id = $1 AND created_by = $2
ORDER BY created_at, id`, experimentID, userSub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProposalDecision{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var decision ProposalDecision
		if err := json.Unmarshal(raw, &decision); err != nil {
			return nil, err
		}
		out = append(out, decision)
	}
	return out, rows.Err()
}

// nullTime lets the column's own default stand for a decision with no timestamp,
// rather than writing a zero time that would sort before every real one.
func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

// sortDecisions orders a log oldest first, breaking a tie on the id so a page is
// stable. The same ordering pkg/relations gives its own log, and for the same
// reason: the newest is the one in force, so the order has to be total.
func sortDecisions(decisions []ProposalDecision) {
	sort.Slice(decisions, func(i, j int) bool {
		if decisions[i].CreatedAt.Equal(decisions[j].CreatedAt) {
			return decisions[i].DecisionID < decisions[j].DecisionID
		}
		return decisions[i].CreatedAt.Before(decisions[j].CreatedAt)
	})
}

// latestDecision is the one that currently stands for a proposal.
//
// The log is append-only, so a developer who changed their mind added a record
// rather than replacing one, and the newest is in force. The history stays
// readable beside it — which is what stops "they rejected it, then accepted it"
// from looking like "they accepted it".
func latestDecision(decisions []ProposalDecision, proposalID string) *ProposalDecision {
	if proposalID == "" {
		return nil
	}
	var latest *ProposalDecision
	for index := range decisions {
		if decisions[index].ProposalID != proposalID {
			continue
		}
		latest = &decisions[index]
	}
	if latest == nil {
		return nil
	}
	out := *latest
	return &out
}
