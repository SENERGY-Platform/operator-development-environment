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
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
)

// What ODE stores about an experiment, and why it is a table rather than a cache.
//
// The profiler and the relational profiler split their state: the computed
// artifact stays in memory because losing one costs a recomputation, and only the
// developer's own input goes to Postgres (§5.4.3). An experiment record makes no
// such split, because **none of it is recomputable.** Ray forgets a submission
// when the cluster restarts and keeps finished jobs only as long as its own
// retention allows; MLflow knows the run but not which ODE session, which working
// copy or which Ray submission produced it. The join between the three exists
// here and nowhere else, so losing it means a developer's list of what they ran
// is gone — and with it the trail from a run back to the commit it came from,
// which is the whole point of §5.11 item 7.

// Store is what the service needs to remember.
type Store interface {
	Put(ctx context.Context, record Experiment) error
	Get(ctx context.Context, userSub, id string) (Experiment, bool, error)
	// BySubmission finds the record for a Ray submission id, so a status poll that
	// only knows Ray's id can still be answered under the caller's own ownership check.
	BySubmission(ctx context.Context, userSub, submissionID string) (Experiment, bool, error)
	// List is the caller's own experiments, newest first, capped at limit.
	List(ctx context.Context, userSub string, limit int) ([]Experiment, error)
	// Previous is the most recent finished experiment of the same MLflow experiment
	// before this one, which is what §5.13's comparison_to_previous compares against.
	Previous(ctx context.Context, record Experiment) (Experiment, bool, error)

	// Running is every developer's unfinished experiments, oldest update first,
	// capped. Not scoped to a user, because the only caller is the poller of M9 and
	// a background loop has no user — it reads Ray and MLflow with the service
	// credential §3.1 item 5 permits and touches nothing of the platform's on
	// anyone's behalf. The subject travels on each record so that whatever acts on
	// one acts as the right developer.
	Running(ctx context.Context, limit int) ([]Experiment, error)

	// RecentlyTerminal is every developer's experiments that finished after `since`
	// and belong to a chat session, newest first, capped.
	//
	// The window is what bounds this against the size of the table rather than
	// against how busy the cluster is, and it is what makes a restart re-offer the
	// last few hours rather than the whole history. Session-bound only, because the
	// one caller delivers §5.13's summary into a conversation and a run launched from
	// the Experiments pane has none.
	RecentlyTerminal(ctx context.Context, since time.Time, limit int) ([]Experiment, error)
}

// MemoryStore is the store a deployment without a database gets.
//
// It is a real degradation rather than a neutral alternative, and validate() says
// so at startup: a restart loses every developer's experiment history, and the
// runs themselves carry on in Ray and MLflow with nothing left pointing at them.
type MemoryStore struct {
	mux     sync.RWMutex
	records map[string]Experiment
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]Experiment{}}
}

func (s *MemoryStore) Put(_ context.Context, record Experiment) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.records[record.ID] = record
	return nil
}

func (s *MemoryStore) Get(_ context.Context, userSub, id string) (Experiment, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	record, found := s.records[id]
	if !found || record.UserSub != userSub {
		return Experiment{}, false, nil
	}
	return record, true, nil
}

func (s *MemoryStore) BySubmission(
	_ context.Context, userSub, submissionID string,
) (Experiment, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	for _, record := range s.records {
		if record.UserSub == userSub && record.SubmissionID == submissionID {
			return record, true, nil
		}
	}
	return Experiment{}, false, nil
}

func (s *MemoryStore) List(_ context.Context, userSub string, limit int) ([]Experiment, error) {
	s.mux.RLock()
	out := make([]Experiment, 0, len(s.records))
	for _, record := range s.records {
		if record.UserSub == userSub {
			out = append(out, record)
		}
	}
	s.mux.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		return out[i].SubmittedAt.After(out[j].SubmittedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) Running(_ context.Context, limit int) ([]Experiment, error) {
	s.mux.RLock()
	out := make([]Experiment, 0, len(s.records))
	for _, record := range s.records {
		if !Terminal(record.Status) {
			out = append(out, record)
		}
	}
	s.mux.RUnlock()

	// Oldest update first, so a record the poller could not settle last tick is the
	// first one tried this tick rather than the last — a capped batch that always
	// started from the newest would starve the tail indefinitely.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) RecentlyTerminal(
	_ context.Context, since time.Time, limit int,
) ([]Experiment, error) {
	s.mux.RLock()
	out := make([]Experiment, 0, len(s.records))
	for _, record := range s.records {
		if recentlyTerminal(record, since) {
			out = append(out, record)
		}
	}
	s.mux.RUnlock()

	// Oldest first, for the reason Running sorts that way and one more besides.
	// Unlike Running, this set does not shrink as work is done — a delivered run is
	// still terminal and still inside the window — so a capped batch always returns
	// the same slice of it. Taking the newest would starve the runs closest to
	// falling out of the window entirely, which are the ones with the least time
	// left to be delivered.
	sort.Slice(out, func(i, j int) bool {
		return terminalAt(out[i]).Before(terminalAt(out[j]))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// recentlyTerminal is the rule both stores implement.
func recentlyTerminal(record Experiment, since time.Time) bool {
	return Terminal(record.Status) &&
		record.SessionID != "" &&
		record.RunID != "" &&
		terminalAt(record).After(since)
}

// terminalAt is when a run's end is known to ODE.
//
// EndedAt where the cluster reported one, and UpdatedAt otherwise. The fallback
// matters: a submission the cluster has forgotten has no end time at all (see
// settleForgotten — stamping one would be a duration nobody measured), and without
// the fallback such a run would sort as 1970 and fall out of every window.
func terminalAt(record Experiment) time.Time {
	if record.EndedAt != nil && !record.EndedAt.IsZero() {
		return *record.EndedAt
	}
	return record.UpdatedAt
}

func (s *MemoryStore) Previous(_ context.Context, record Experiment) (Experiment, bool, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	var best Experiment
	var found bool
	for _, candidate := range s.records {
		if !previousCandidate(candidate, record) {
			continue
		}
		if !found || candidate.SubmittedAt.After(best.SubmittedAt) {
			best, found = candidate, true
		}
	}
	return best, found, nil
}

// previousCandidate is the rule both stores implement: same developer, same
// MLflow experiment (which per D17 means the same repository), finished, and
// submitted before this one.
func previousCandidate(candidate, record Experiment) bool {
	return candidate.ID != record.ID &&
		candidate.UserSub == record.UserSub &&
		candidate.MLflowExperimentID == record.MLflowExperimentID &&
		candidate.RunID != "" &&
		Terminal(candidate.Status) &&
		candidate.SubmittedAt.Before(record.SubmittedAt)
}

// PostgresStore is the same records, in the database.
type PostgresStore struct {
	db *database.DB
}

func NewPostgresStore(db *database.DB) *PostgresStore { return &PostgresStore{db: db} }

const experimentColumns = `id, user_sub, submission_id, mlflow_run_id, mlflow_experiment_id,
       mlflow_experiment_name, session_id, repository, commit_sha, branch, entrypoint,
       package_uri, package_bytes, package_reused, status, message, scoped_credential,
       submitted_at, updated_at, started_at, ended_at`

func (s *PostgresStore) Put(ctx context.Context, record Experiment) error {
	_, err := s.db.Pool().Exec(ctx, `
INSERT INTO ode_experiments (`+experimentColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
ON CONFLICT (id) DO UPDATE SET
    status = EXCLUDED.status, message = EXCLUDED.message, updated_at = EXCLUDED.updated_at,
    started_at = EXCLUDED.started_at, ended_at = EXCLUDED.ended_at,
    mlflow_run_id = EXCLUDED.mlflow_run_id`,
		record.ID, record.UserSub, record.SubmissionID, record.RunID, record.MLflowExperimentID,
		record.MLflowExperimentName, record.SessionID, record.Repository, record.CommitSHA,
		record.Branch, record.Entrypoint, record.PackageURI, record.PackageBytes,
		record.PackageReused, record.Status, record.Message, record.ScopedCredential,
		record.SubmittedAt, record.UpdatedAt, record.StartedAt, record.EndedAt)
	return err
}

func (s *PostgresStore) Get(
	ctx context.Context, userSub, id string,
) (Experiment, bool, error) {
	// The subject is in the WHERE clause rather than checked after the read, so a
	// route cannot forget it and there is no moment where another developer's row
	// is in memory.
	row := s.db.Pool().QueryRow(ctx,
		`SELECT `+experimentColumns+` FROM ode_experiments WHERE id = $1 AND user_sub = $2`,
		id, userSub)
	return scanExperiment(row)
}

func (s *PostgresStore) BySubmission(
	ctx context.Context, userSub, submissionID string,
) (Experiment, bool, error) {
	row := s.db.Pool().QueryRow(ctx,
		`SELECT `+experimentColumns+`
FROM ode_experiments WHERE submission_id = $1 AND user_sub = $2`, submissionID, userSub)
	return scanExperiment(row)
}

func (s *PostgresStore) List(
	ctx context.Context, userSub string, limit int,
) ([]Experiment, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.Pool().Query(ctx,
		`SELECT `+experimentColumns+`
FROM ode_experiments WHERE user_sub = $1 ORDER BY submitted_at DESC LIMIT $2`, userSub, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExperiments(rows, limit)
}

func (s *PostgresStore) Previous(
	ctx context.Context, record Experiment,
) (Experiment, bool, error) {
	row := s.db.Pool().QueryRow(ctx,
		`SELECT `+experimentColumns+`
FROM ode_experiments
WHERE user_sub = $1 AND mlflow_experiment_id = $2 AND id <> $3
  AND mlflow_run_id <> '' AND status = ANY($4) AND submitted_at < $5
ORDER BY submitted_at DESC LIMIT 1`,
		record.UserSub, record.MLflowExperimentID, record.ID,
		[]string{StatusSucceeded, StatusFailed, StatusStopped}, record.SubmittedAt)
	return scanExperiment(row)
}

func (s *PostgresStore) Running(ctx context.Context, limit int) ([]Experiment, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.db.Pool().Query(ctx,
		`SELECT `+experimentColumns+`
FROM ode_experiments
WHERE status <> ALL($1)
ORDER BY updated_at ASC LIMIT $2`,
		[]string{StatusSucceeded, StatusFailed, StatusStopped}, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExperiments(rows, limit)
}

func (s *PostgresStore) RecentlyTerminal(
	ctx context.Context, since time.Time, limit int,
) ([]Experiment, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	// COALESCE for the reason terminalAt has its fallback: a submission the cluster
	// has forgotten carries no end time, and comparing a NULL would drop it from
	// every window rather than including it.
	rows, err := s.db.Pool().Query(ctx,
		`SELECT `+experimentColumns+`
FROM ode_experiments
WHERE status = ANY($1) AND session_id <> '' AND mlflow_run_id <> ''
  AND COALESCE(ended_at, updated_at) > $2
ORDER BY COALESCE(ended_at, updated_at) ASC LIMIT $3`,
		[]string{StatusSucceeded, StatusFailed, StatusStopped}, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExperiments(rows, limit)
}

// collectExperiments drains a result set.
func collectExperiments(rows pgx.Rows, capacity int) ([]Experiment, error) {
	out := make([]Experiment, 0, capacity)
	for rows.Next() {
		record, _, err := scanExperiment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// scanner is what both QueryRow and Rows satisfy, so one scan serves both.
type scanner interface {
	Scan(dest ...any) error
}

func scanExperiment(row scanner) (Experiment, bool, error) {
	var record Experiment
	err := row.Scan(
		&record.ID, &record.UserSub, &record.SubmissionID, &record.RunID,
		&record.MLflowExperimentID, &record.MLflowExperimentName, &record.SessionID,
		&record.Repository, &record.CommitSHA, &record.Branch, &record.Entrypoint,
		&record.PackageURI, &record.PackageBytes, &record.PackageReused, &record.Status,
		&record.Message, &record.ScopedCredential, &record.SubmittedAt, &record.UpdatedAt,
		&record.StartedAt, &record.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Experiment{}, false, nil
	}
	if err != nil {
		return Experiment{}, false, err
	}
	return record, true, nil
}

// defaultListLimit bounds a listing a caller did not bound. Generous, because
// this is a developer's own history rather than anything a model reads.
const defaultListLimit = 100

// touch stamps a record as updated. Here rather than at each call site so the two
// timestamps cannot drift apart.
func touch(record Experiment) Experiment {
	record.UpdatedAt = time.Now().UTC()
	return record
}
