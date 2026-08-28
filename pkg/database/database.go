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
		// Which workbench the session acts in — its checkout and its kernel. Added
		// rather than declared in the CREATE above so a deployment that already has
		// the table gets it too, and additive with a default, which is the only shape
		// of change this migration style can carry (see Migrate).
		//
		// Empty means a session written before workbenches existed, or one whose
		// workbench has since been deleted. Both resolve to the developer's first
		// workbench on read, so no conversation loses its code context.
		name: "ode_chat_sessions_workbench",
		sql: `
ALTER TABLE ode_chat_sessions
    ADD COLUMN IF NOT EXISTS workbench_id TEXT NOT NULL DEFAULT ''`,
	},
	{
		// The session's standing answer to a run_code confirmation it recognises.
		//
		// Additive with a default of false, so every existing session keeps being
		// asked — the setting is something a developer turns on deliberately and
		// never something a migration turns on for them.
		name: "ode_chat_sessions_auto_run",
		sql: `
ALTER TABLE ode_chat_sessions
    ADD COLUMN IF NOT EXISTS auto_run BOOLEAN NOT NULL DEFAULT FALSE`,
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
		name: "ode_platform_creations",
		sql: `
-- What a chat session created on the platform (§5.8's two create tools), and the
-- only thing the two delete tools may reach. Append-only, and deliberately not
-- cascaded from ode_sessions: a deleted session takes its ability to delete
-- anything with it, and the record of what was created outlives the conversation
-- that created it.
CREATE TABLE IF NOT EXISTS ode_platform_creations (
    id         BIGSERIAL PRIMARY KEY,
    session_id TEXT NOT NULL,
    -- "import_instance" or "export". Not an enum: a third kind would otherwise be
    -- a migration on a table whose rows are a log.
    kind       TEXT NOT NULL,
    object_id  TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    tool       TEXT NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_platform_creations_by_session",
		sql: `CREATE INDEX IF NOT EXISTS ode_platform_creations_session_idx
              ON ode_platform_creations (session_id, at)`,
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
    export_id     TEXT NOT NULL DEFAULT '',
    variable_path TEXT NOT NULL,
    override      JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		// A series is addressed by a device's service or by an export, and the
		// overlay is keyed by whichever it is. Added rather than folded into the
		// CREATE above because the CREATE is IF NOT EXISTS and would not touch a
		// table that already exists — the deployments that have one would keep a
		// four-column key and put every export's overrides in one bucket.
		//
		// The default is '' rather than NULL so the lookup stays one equality per
		// column: a NULL would need IS NULL for the device form and never match.
		name: "ode_profile_overrides_export_id",
		sql: `ALTER TABLE ode_profile_overrides
              ADD COLUMN IF NOT EXISTS export_id TEXT NOT NULL DEFAULT ''`,
	},
	{
		// Deliberately unchanged by the export_id column above. The name is what
		// CREATE INDEX IF NOT EXISTS keys on, so widening the index here would be a
		// no-op wherever the table already exists, and renaming it would leave every
		// existing deployment carrying two indexes for one lookup with nothing to
		// drop the old one. The columns below still narrow a lookup to its series —
		// an export's rows share two empty device columns and are separated by the
		// variable path — and export_id is filtered on top of that.
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
-- Superseded by ode_workbenches, and kept only to be read once: a developer who
-- has a row here and no workbench yet has it adopted into one on their next
-- request, so nobody re-selects a repository whose checkout is already on their
-- PVC. The only write left is the adopted_at stamp below, which says the reading
-- has happened. It can be dropped once every deployment has run a version that
-- adopts, which needs a real migration tool — see Migrate.
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
		// When this row became a workbench. Added rather than declared in the CREATE
		// above so a deployment that already has the table gets it too, and additive
		// and nullable, which is the only shape of change this migration style can
		// carry (see Migrate).
		//
		// NULL is a row that has not been adopted yet, which is every row a release
		// before workbenches wrote. It is what makes the adoption happen once for a
		// developer rather than every time their workbench list is empty: closing
		// the last workbench would otherwise bring the old link back as a new one on
		// the next request.
		name: "ode_repo_links_adopted_at",
		sql: `
ALTER TABLE ode_repo_links
    ADD COLUMN IF NOT EXISTS adopted_at TIMESTAMPTZ`,
	},
	{
		name: "ode_workbenches",
		sql: `
-- One working context: a repository checkout on the PVC and the kernel that runs
-- in it. It is what ode_repo_links was, made plural — a developer working on two
-- operators at once has two of these, and each chat session names the one it acts
-- in.
--
-- The link columns are the same ones ode_repo_links holds and mean the same
-- things. They live here rather than in a second table because a workbench
-- without a repository and a repository without a workbench are both states
-- nothing can do anything with: selecting is what a workbench is for, and the two
-- are created and forgotten together.
--
-- Empty full_name is a workbench whose repository has not been selected yet,
-- which is the state between creating one and choosing what to work on.
CREATE TABLE IF NOT EXISTS ode_workbenches (
    id               TEXT PRIMARY KEY,
    user_sub         TEXT NOT NULL,
    -- The developer's own name for it. Empty falls back to the repository, and
    -- before there is one, to the id.
    title            TEXT NOT NULL DEFAULT '',
    full_name        TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL DEFAULT '',
    owner            TEXT NOT NULL DEFAULT '',
    default_branch   TEXT NOT NULL DEFAULT '',
    private          BOOLEAN NOT NULL DEFAULT FALSE,
    clone_url        TEXT NOT NULL DEFAULT '',
    html_url         TEXT NOT NULL DEFAULT '',
    path             TEXT NOT NULL DEFAULT '',
    operator_lib_ref TEXT NOT NULL DEFAULT '',
    scaffolded_at    TIMESTAMPTZ,
    selected_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at     TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
	},
	{
		name: "ode_workbenches_by_user",
		sql: `CREATE INDEX IF NOT EXISTS ode_workbenches_user_idx
              ON ode_workbenches (user_sub, created_at)`,
	},
	{
		// The invariant the working copy depends on, held where it cannot be argued
		// with. A checkout is at {owner}/{name} under the workspace, so two
		// workbenches on one repository would be two kernels and two streams of git
		// commands in one working tree: a corrupted index and a lost diff. The
		// service refuses it with an explanation; this is what makes the refusal
		// true even if a second ODE is talking to the same database.
		name: "ode_workbenches_one_repository_per_user",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS ode_workbenches_repository_idx
              ON ode_workbenches (user_sub, full_name)
              WHERE full_name <> ''`,
	},
	{
		// The stamp for everyone who was adopted before there was one to write.
		//
		// A deployment that has already run an adopting release has developers with a
		// workbench and an unstamped row, and without this they keep the behaviour
		// the column exists to end: close the last workbench, and the old link comes
		// back as a new one. Having any workbench at all is what says the adoption
		// ran — it is the only way one gets created, since every path to a workbench
		// reads the list first, and the list is where adoption happens.
		//
		// Idempotent, like everything here: the second run finds nothing to stamp.
		// The cost if it ever stamped a row too early is one re-selection of a
		// repository whose checkout is still on the PVC, which is what selecting
		// already does.
		name: "ode_repo_links_adopted_backfill",
		sql: `
UPDATE ode_repo_links AS l
   SET adopted_at = now()
 WHERE l.adopted_at IS NULL
   AND EXISTS (SELECT 1 FROM ode_workbenches AS w WHERE w.user_sub = l.user_sub)`,
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
		// Which working context a run was launched from. Added rather than declared
		// in the CREATE above so a deployment that already has the table gets it too
		// (see Migrate), and it completes the trail §5.11 item 7 is about: a run
		// leads back to a commit, and now also to the checkout that commit was
		// packaged from — which is what the interpretation turn needs to read the
		// run's own evaluation.yaml once a developer has more than one open.
		//
		// Empty is a run from before workbenches existed, which resolves to the
		// developer's only one.
		name: "ode_experiments_workbench",
		sql: `
ALTER TABLE ode_experiments
    ADD COLUMN IF NOT EXISTS workbench_id TEXT NOT NULL DEFAULT ''`,
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
