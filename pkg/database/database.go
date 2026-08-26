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

// Package database is ODE's PostgreSQL connection and schema.
//
// It exists because of one requirement in §3.3: a per-user LLM spend cap over a
// period. A cap is only as trustworthy as the record it is computed from, and an
// in-process record resets on every deployment — so "you have spent 40 of your
// 50 euros this month" would silently become "you have spent nothing" after a
// pod restart. That is not a limitation worth documenting; it is a cap that does
// not hold.
//
// The same store therefore also keeps the things whose loss M1b documented and
// accepted: the exposure-tier audit trail (§3.2 requires every change logged),
// and the profiler's override overlay, which §5.4.3 calls an empirical record of
// human confirmation.
//
// Postgres is optional. A deployment without a URL runs the in-memory stores and
// says so loudly at startup, matching how a missing timescale-wrapper already
// degrades. That keeps local development and the test suite free of containers,
// which the README promises.
package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is a connection pool plus the schema it has been migrated to.
type DB struct {
	pool *pgxpool.Pool
}

type Options struct {
	// MaxConns bounds the pool. ODE's database traffic is small — session reads,
	// usage inserts — so this is deliberately modest and exists mainly to stop a
	// runaway from exhausting the server's connection slots.
	MaxConns int32
	// ConnectTimeout bounds the initial connection, so a wrong URL fails at
	// startup with a legible error rather than hanging.
	ConnectTimeout time.Duration
}

const (
	defaultMaxConns       = 8
	defaultConnectTimeout = 10 * time.Second
)

// Connect opens the pool and verifies it with a ping.
//
// The ping is not ceremony: pgxpool connects lazily, so without it a
// misconfigured URL surfaces on the first developer's first message rather than
// in the startup logs.
func Connect(ctx context.Context, url string, opts Options) (*DB, error) {
	if url == "" {
		return nil, errors.New("database: no connection URL")
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = defaultMaxConns
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = defaultConnectTimeout
	}

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The error from ParseConfig can quote the DSN, which carries the password.
		return nil, errors.New("database: the connection URL could not be parsed")
	}
	config.MaxConns = opts.MaxConns

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("database: pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: could not reach the server: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Pool exposes the pool to the store implementations in other packages.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

func (db *DB) Close() {
	if db != nil && db.pool != nil {
		db.pool.Close()
	}
}

// Migrate applies the schema.
//
// Plain idempotent DDL run at startup rather than a migration tool with a version
// table. That is a deliberate trade for this stage: the schema is small, every
// statement is CREATE ... IF NOT EXISTS, and running it twice is a no-op. The cost
// is that a *destructive* change later — renaming a column, narrowing a type —
// has no place to live here and will need a real migration tool. Say so now rather
// than discovering it during one.
func Migrate(ctx context.Context, db *DB) error {
	if db == nil || db.pool == nil {
		return errors.New("database: cannot migrate without a connection")
	}

	for _, statement := range schema {
		if _, err := db.pool.Exec(ctx, statement.sql); err != nil {
			return fmt.Errorf("database: migrating %s: %w", statement.name, err)
		}
	}
	slog.InfoContext(ctx, "database schema applied", "statements", len(schema))
	return nil
}

type migration struct {
	name string
	sql  string
}

// schema is the whole of ODE's persistent state.
//
// Two conventions run through it. Every table is prefixed ode_, because ODE may
// share a database with another service and an unprefixed "sessions" table is an
// invitation to collide. And every id is text rather than a serial, because the
// ids are minted by ODE and appear in URLs — a database-assigned integer would
// make a session id guessable and enumerable.
var schema = []migration{
	{
		name: "ode_chat_sessions",
		sql: `
CREATE TABLE IF NOT EXISTS ode_chat_sessions (
    id          TEXT PRIMARY KEY,
    user_sub    TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    provider    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    -- The exposure tier as its wire name ("L0"), not an integer. A tier is a named
    -- level (§3.2) and storing 0/1/2 invites an off-by-one on the next reader.
    tier        TEXT NOT NULL DEFAULT 'L0',
    selection   JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ
)`,
	},
	{
		name: "ode_chat_sessions_by_user",
		sql: `CREATE INDEX IF NOT EXISTS ode_chat_sessions_user_idx
              ON ode_chat_sessions (user_sub, updated_at DESC)`,
	},
	{
		name: "ode_chat_messages",
		sql: `
CREATE TABLE IF NOT EXISTS ode_chat_messages (
    session_id TEXT NOT NULL REFERENCES ode_chat_sessions (id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL,
    role       TEXT NOT NULL,
    -- The content blocks as stored, so a conversation replays to any provider
    -- with the tool calls and results intact. Flattening to text here would make
    -- a resumed session lose every tool call it ever made.
    content    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq)
)`,
	},
	{
		// The two columns of M9's injected messages, added rather than declared in the
		// CREATE above so a deployment that already has the table gets them too. Both
		// are additive with a default, which is the only shape of change this
		// migration style can carry — see Migrate.
		name: "ode_chat_messages_origin",
		sql: `
ALTER TABLE ode_chat_messages
    -- Who put the message there: '' is the developer, 'ode' is a message ODE
    -- composed and injected, which today means §5.13's result summary. The default
    -- is the developer's because every row written before M9 was one.
    ADD COLUMN IF NOT EXISTS origin  TEXT NOT NULL DEFAULT '',
    -- What an injected message is about — an experiment id, for a run summary — so
    -- a reader can find the message belonging to one run without parsing it. It is
    -- also what makes the delivery idempotent: the message *is* the record that the
    -- run was interpreted, so a poller re-offering the same run finds it here.
    ADD COLUMN IF NOT EXISTS subject TEXT NOT NULL DEFAULT ''`,
	},
	{
		name: "ode_chat_messages_by_subject",
		sql: `CREATE INDEX IF NOT EXISTS ode_chat_messages_subject_idx
              ON ode_chat_messages (session_id, subject)
              WHERE subject <> ''`,
	},
	{
		name: "ode_tier_changes",
		sql: `
CREATE TABLE IF NOT EXISTS ode_tier_changes (
    id         BIGSERIAL PRIMARY KEY,
    session_id TEXT NOT NULL,
    user_sub   TEXT NOT NULL,
    from_tier  TEXT NOT NULL,
    to_tier    TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_tier_changes_by_session",
		sql: `CREATE INDEX IF NOT EXISTS ode_tier_changes_session_idx
              ON ode_tier_changes (session_id, at DESC)`,
	},
	{
		name: "ode_usage",
		sql: `
CREATE TABLE IF NOT EXISTS ode_usage (
    id                  BIGSERIAL PRIMARY KEY,
    user_sub            TEXT NOT NULL,
    session_id          TEXT NOT NULL DEFAULT '',
    provider            TEXT NOT NULL,
    model               TEXT NOT NULL,
    input_tokens        BIGINT NOT NULL DEFAULT 0,
    output_tokens       BIGINT NOT NULL DEFAULT 0,
    cached_input_tokens BIGINT NOT NULL DEFAULT 0,
    -- NUMERIC, not double precision: this column is summed against a spend cap,
    -- and binary floating point accumulates error over thousands of rows in the
    -- one place where the total has to be defensible.
    cost                NUMERIC(14, 6) NOT NULL DEFAULT 0,
    cost_estimated      BOOLEAN NOT NULL DEFAULT TRUE,
    at                  TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_usage_by_user_time",
		sql:  `CREATE INDEX IF NOT EXISTS ode_usage_user_at_idx ON ode_usage (user_sub, at DESC)`,
	},
	{
		name: "ode_usage_by_time",
		sql:  `CREATE INDEX IF NOT EXISTS ode_usage_at_idx ON ode_usage (at DESC)`,
	},
	{
		name: "ode_tool_calls",
		sql: `
CREATE TABLE IF NOT EXISTS ode_tool_calls (
    id          BIGSERIAL PRIMARY KEY,
    user_sub    TEXT NOT NULL,
    session_id  TEXT NOT NULL DEFAULT '',
    tool        TEXT NOT NULL,
    tier        TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    at          TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_tool_calls_by_user_time",
		sql:  `CREATE INDEX IF NOT EXISTS ode_tool_calls_user_at_idx ON ode_tool_calls (user_sub, at DESC)`,
	},
	{
		name: "ode_limits",
		sql: `
CREATE TABLE IF NOT EXISTS ode_limits (
    -- The empty string is the global row. A sentinel rather than a nullable
    -- column so the primary key still forbids two global rows.
    subject    TEXT PRIMARY KEY,
    limits     JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL DEFAULT ''
)`,
	},
	{
		name: "ode_confirmations",
		sql: `
CREATE TABLE IF NOT EXISTS ode_confirmations (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL,
    user_sub    TEXT NOT NULL,
    call_id     TEXT NOT NULL,
    tool        TEXT NOT NULL,
    input       JSONB NOT NULL,
    tier        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    decision    TEXT NOT NULL DEFAULT ''
)`,
	},
	{
		name: "ode_confirmations_pending",
		sql: `CREATE INDEX IF NOT EXISTS ode_confirmations_session_idx
              ON ode_confirmations (session_id, created_at DESC)`,
	},
	{
		name: "ode_profile_overrides",
		sql: `
-- The override overlay of §5.4.3: append-only, keyed by series reference rather
-- than by profile id, which is what lets a confirmation survive a recomputation
-- under a new detector version.
CREATE TABLE IF NOT EXISTS ode_profile_overrides (
    id            TEXT PRIMARY KEY,
    device_id     TEXT NOT NULL,
    service_id    TEXT NOT NULL,
    variable_path TEXT NOT NULL,
    override      JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_profile_overrides_by_series",
		sql: `CREATE INDEX IF NOT EXISTS ode_profile_overrides_series_idx
              ON ode_profile_overrides (device_id, service_id, variable_path, created_at)`,
	},
	{
		name: "ode_relation_rule_decisions",
		sql: `
-- The rule decision log of §5.5 and §5.10: append-only, keyed by a fingerprint of
-- what the rule *says* rather than by the relation profile it was seen in, which is
-- what lets a verdict survive the same rule being recomputed over a different window
-- by a later detector. Relation profiles themselves stay in memory — losing one costs
-- a recomputation, losing a developer's judgement destroys evidence.
CREATE TABLE IF NOT EXISTS ode_relation_rule_decisions (
    id         TEXT PRIMARY KEY,
    rule_id    TEXT NOT NULL,
    created_by TEXT NOT NULL,
    decision   JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_relation_rule_decisions_by_rule",
		sql: `CREATE INDEX IF NOT EXISTS ode_relation_rule_decisions_rule_idx
              ON ode_relation_rule_decisions (rule_id, created_at)`,
	},
	{
		name: "ode_github_identities",
		sql: `
-- The GitHub credential of §5.11 item 1: per user, encrypted, and separate from the
-- Keycloak session. sealed_token is AES-256-GCM under github_token_key and is the
-- one column in this schema whose loss is a security event rather than an
-- inconvenience — a row read out of a backup is a set of repositories.
--
-- One row per developer. A second GitHub account would be a second row and there is
-- no use for one: the account is how ODE pushes, not what it pushes to.
CREATE TABLE IF NOT EXISTS ode_github_identities (
    user_sub       TEXT PRIMARY KEY,
    login          TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    avatar_url     TEXT NOT NULL DEFAULT '',
    scopes         TEXT[] NOT NULL DEFAULT '{}',
    missing_scopes TEXT[] NOT NULL DEFAULT '{}',
    sealed_token   TEXT NOT NULL,
    connected_at   TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_repo_links",
		sql: `
-- Which repository a developer is working on, and where its working copy sits on
-- their PVC (§5.11 item 5). Everything else about the repository — its contents, its
-- history, its divergence — is read from the checkout or from GitHub, because both
-- are authoritative and a cached copy here could only be wrong.
--
-- One active repository per developer. Switching replaces the row and leaves the old
-- checkout in place, which is what makes switching back a reuse rather than a clone.
CREATE TABLE IF NOT EXISTS ode_repo_links (
    user_sub         TEXT PRIMARY KEY,
    full_name        TEXT NOT NULL,
    name             TEXT NOT NULL,
    owner            TEXT NOT NULL,
    default_branch   TEXT NOT NULL DEFAULT '',
    private          BOOLEAN NOT NULL DEFAULT FALSE,
    clone_url        TEXT NOT NULL,
    html_url         TEXT NOT NULL DEFAULT '',
    path             TEXT NOT NULL,
    -- The Operator Lib the scaffold pinned (D15), so an upgrade later is a visible
    -- decision rather than a silent drift.
    operator_lib_ref TEXT NOT NULL DEFAULT '',
    scaffolded_at    TIMESTAMPTZ,
    selected_at      TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_experiments",
		sql: `
-- One submitted Ray job (§5.12), and the one table in this schema whose contents
-- are recomputable from nowhere else.
--
-- The profiler and the relational profiler split their state — the artifact stays
-- in memory because losing it costs a recomputation, and only a developer's own
-- judgement is worth a table (§5.4.3). An experiment record makes no such split.
-- Ray forgets a submission when the cluster restarts and keeps a finished job only
-- as long as its own retention allows; MLflow knows the run but not which ODE
-- session, which working copy or which Ray submission produced it. The join
-- between the three exists here, so losing this row loses the trail from a run
-- back to the commit it came from — which is the whole of §5.11 item 7.
CREATE TABLE IF NOT EXISTS ode_experiments (
    id                     TEXT PRIMARY KEY,
    user_sub               TEXT NOT NULL,
    -- Ray's own id for the job. Unique per cluster, and ODE mints it rather than
    -- letting Ray, which is what makes a resubmitted request a refusal rather than
    -- a second job.
    submission_id          TEXT NOT NULL,
    mlflow_run_id          TEXT NOT NULL DEFAULT '',
    mlflow_experiment_id   TEXT NOT NULL DEFAULT '',
    mlflow_experiment_name TEXT NOT NULL DEFAULT '',
    session_id             TEXT NOT NULL DEFAULT '',
    repository             TEXT NOT NULL DEFAULT '',
    -- The state the job package was built from. The reason this table exists.
    commit_sha             TEXT NOT NULL DEFAULT '',
    branch                 TEXT NOT NULL DEFAULT '',
    entrypoint             TEXT NOT NULL DEFAULT '',
    package_uri            TEXT NOT NULL DEFAULT '',
    package_bytes          BIGINT NOT NULL DEFAULT 0,
    package_reused         BOOLEAN NOT NULL DEFAULT FALSE,
    -- Ray's own status vocabulary, not a translation of it: these strings are what
    -- the dashboard beside the pane shows, and a second vocabulary would give a
    -- developer two answers to reconcile.
    status                 TEXT NOT NULL DEFAULT 'PENDING',
    message                TEXT NOT NULL DEFAULT '',
    -- Whether the job carried a token minted for it (§3.1 item 6) or the caller's
    -- session token. Recorded because it decides what a long run survives, and a
    -- run that died at hour two should be explicable afterwards.
    scoped_credential      BOOLEAN NOT NULL DEFAULT FALSE,
    submitted_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at             TIMESTAMPTZ,
    ended_at               TIMESTAMPTZ
)`,
	},
	{
		name: "ode_proposal_decisions",
		sql: `
-- The developer's answer to a proposed next experiment (§5.13's last sentence,
-- D28): append-only, keyed by a fingerprint of *what was proposed* rather than by
-- the interpretation it appeared in — the same choice ode_relation_rule_decisions
-- makes, and for the same reason. A run interpreted again produces a fresh
-- summary and a fresh reading of it, and a decision tied to that reading would
-- silently stop applying; tied to the proposal's text, a rejection stays a
-- rejection.
--
-- This is the only table M9 adds, and the split behind that is §5.4.3's. The
-- summary is recomputable from MLflow, the assistant's interpretation is already
-- durable as chat messages in ode_chat_messages, and only the human judgement
-- here can be regenerated by nothing.
--
-- Nothing in it is binding. Accepting a proposal records agreement; promoting a
-- value into evaluation.yaml or the operator config is a separate developer
-- action, and §5.8 has no tool for either.
CREATE TABLE IF NOT EXISTS ode_proposal_decisions (
    id            TEXT PRIMARY KEY,
    experiment_id TEXT NOT NULL,
    proposal_id   TEXT NOT NULL,
    created_by    TEXT NOT NULL,
    decision      TEXT NOT NULL,
    -- The whole record, so a reader gets what was proposed and what the developer
    -- put in its place without this schema having to grow a column per field.
    record        JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_proposal_decisions_by_experiment",
		sql: `CREATE INDEX IF NOT EXISTS ode_proposal_decisions_experiment_idx
              ON ode_proposal_decisions (experiment_id, created_by, created_at)`,
	},
	{
		name: "ode_proposal_decisions_by_proposal",
		sql: `CREATE INDEX IF NOT EXISTS ode_proposal_decisions_proposal_idx
              ON ode_proposal_decisions (proposal_id, created_at)`,
	},
	{
		name: "ode_experiments_by_user_time",
		sql: `CREATE INDEX IF NOT EXISTS ode_experiments_user_at_idx
              ON ode_experiments (user_sub, submitted_at DESC)`,
	},
	{
		name: "ode_experiments_by_submission",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS ode_experiments_submission_idx
              ON ode_experiments (submission_id)`,
	},
	{
		name: "ode_experiments_unfinished",
		sql: `-- The poller of §5.13 asks this every interval: which runs does the store
              -- still call unfinished. Partial, because that set is small and stays small
              -- however large the table grows — which is the whole point of indexing it
              -- rather than letting a sequence scan get slower every week.
              CREATE INDEX IF NOT EXISTS ode_experiments_unfinished_idx
              ON ode_experiments (updated_at)
              WHERE status NOT IN ('SUCCEEDED', 'FAILED', 'STOPPED')`,
	},
	{
		name: "ode_experiments_recently_terminal",
		sql: `-- The poller's second query: which session-bound runs finished lately. The
              -- expression matches the one in the query, COALESCE and all, or the planner
              -- would not use it.
              CREATE INDEX IF NOT EXISTS ode_experiments_recently_terminal_idx
              ON ode_experiments (COALESCE(ended_at, updated_at))
              WHERE session_id <> '' AND mlflow_run_id <> ''`,
	},
	{
		name: "ode_experiments_previous",
		sql: `-- The lookup §5.13's comparison_to_previous makes: the most recent finished run
              -- of the same developer's same MLflow experiment.
              CREATE INDEX IF NOT EXISTS ode_experiments_previous_idx
              ON ode_experiments (user_sub, mlflow_experiment_id, submitted_at DESC)`,
	},
}
