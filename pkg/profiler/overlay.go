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

package profiler

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

// Overrides is a persistent home for the override overlay.
//
// It is separate from Store because the two halves of that interface have
// genuinely different durability requirements. A computed profile is a
// reproducible artifact — losing it costs a recomputation. An override is a
// developer's confirmation of derived semantics, which §5.4.3 calls an empirical
// record and D11 makes a first-class part of the design; losing it destroys evidence that
// cannot be regenerated.
type Overrides interface {
	Append(override ProfileOverride) (ProfileOverride, error)
	ForSeries(ref SeriesRef) []ProfileOverride
}

// OverlayStore is a Store whose profiles live in one place and whose overrides
// live in another.
type OverlayStore struct {
	profiles  Store
	overrides Overrides
}

// NewOverlayStore composes the two. profiles handles everything except the
// overlay, which overrides takes over.
func NewOverlayStore(profiles Store, overrides Overrides) *OverlayStore {
	return &OverlayStore{profiles: profiles, overrides: overrides}
}

func (s *OverlayStore) Put(profile SeriesProfile, sessions []Session) (SeriesProfile, bool, error) {
	return s.profiles.Put(profile, sessions)
}

func (s *OverlayStore) ByID(profileID string) (SeriesProfile, bool) {
	return s.profiles.ByID(profileID)
}

func (s *OverlayStore) ByCacheKey(cacheKey string) (SeriesProfile, bool) {
	return s.profiles.ByCacheKey(cacheKey)
}

func (s *OverlayStore) Sessions(profileID string, query SessionQuery) (SessionPage, error) {
	return s.profiles.Sessions(profileID, query)
}

func (s *OverlayStore) AppendOverride(override ProfileOverride) (ProfileOverride, error) {
	if err := override.Validate(); err != nil {
		return ProfileOverride{}, err
	}
	return s.overrides.Append(override)
}

func (s *OverlayStore) Overrides(ref SeriesRef) []ProfileOverride {
	return s.overrides.ForSeries(ref)
}

// PostgresOverrides persists the overlay.
//
// The Store interface takes no context, which is right for an in-memory
// implementation and awkward here: the queries need one. Rather than change an
// interface the whole profiler is written against, each call gets its own bounded
// context. The bound matters — a wedged database must not hang a profile read
// forever — and it is generous, because these are two-column indexed queries.
type PostgresOverrides struct {
	pool *pgxpool.Pool
	ids  interface{ NewID() string }
}

const overrideQueryTimeout = 10 * time.Second

func NewPostgresOverrides(db *database.DB) *PostgresOverrides {
	return &PostgresOverrides{pool: db.Pool(), ids: &overrideIDs{}}
}

func (s *PostgresOverrides) Append(override ProfileOverride) (ProfileOverride, error) {
	if override.OverrideID == "" {
		override.OverrideID = s.ids.NewID()
	}
	if override.CreatedAt.IsZero() {
		override.CreatedAt = time.Now().UTC()
	}

	encoded, err := json.Marshal(override)
	if err != nil {
		return ProfileOverride{}, fmt.Errorf("profiler: encoding an override: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), overrideQueryTimeout)
	defer cancel()

	// The whole override is stored as one document *and* its series reference is
	// stored in columns. The columns are what the lookup uses; the document is what
	// survives a change to the override schema, so an old confirmation stays
	// readable rather than needing a migration to keep its meaning.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO ode_profile_overrides (id, device_id, service_id, variable_path, override, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		override.OverrideID, override.SeriesRef.DeviceID, override.SeriesRef.ServiceID,
		override.SeriesRef.VariablePath, encoded, override.CreatedAt); err != nil {
		return ProfileOverride{}, fmt.Errorf("profiler: storing an override: %w", err)
	}
	return override, nil
}

// ForSeries returns the overlay for one series, oldest first.
//
// A read failure returns no overrides rather than an error, because the interface
// has nowhere to put one. That is the wrong direction to fail in and it is worth
// being explicit about: the profile would then be served without a confirmation
// the developer had made. It is logged so it is visible rather than silent, and
// the alternative — failing every profile read when the database blips — would be
// worse for the developer in front of it.
//
// The levels below follow the log guidelines rather than the severity of the
// consequence: ERROR pages someone over Slack, so it is reserved for the one case
// here that does not heal on its own — a stored override that cannot be decoded,
// which stays broken until a human looks at the row. A query or a scan that fails
// because the database is briefly unreachable is WARN: the next read recovers.
func (s *PostgresOverrides) ForSeries(ref SeriesRef) []ProfileOverride {
	ctx, cancel := context.WithTimeout(context.Background(), overrideQueryTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT override FROM ode_profile_overrides
		WHERE device_id = $1 AND service_id = $2 AND variable_path = $3
		ORDER BY created_at`,
		ref.DeviceID, ref.ServiceID, ref.VariablePath)
	if err != nil {
		slog.Warn("could not read the profile override overlay; the profile will be served "+
			"without the developer's confirmations", "series", ref.String(), "error", err)
		return []ProfileOverride{}
	}
	defer rows.Close()

	out := []ProfileOverride{}
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			slog.Warn("could not scan a profile override", "series", ref.String(), "error", err)
			return out
		}
		var override ProfileOverride
		if err := json.Unmarshal(encoded, &override); err != nil {
			slog.Error("a stored profile override could not be decoded", "error", err)
			continue
		}
		out = append(out, override)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("reading the profile override overlay failed part-way",
			"series", ref.String(), "error", err)
	}
	return out
}

// overrideIDs mints override ids.
//
// Random rather than time-derived: two confirmations submitted in the same
// instant would collide on the primary key, and the clock is not a source of
// uniqueness under concurrency.
type overrideIDs struct{}

func (o *overrideIDs) NewID() string {
	buffer := make([]byte, 12)
	_, _ = rand.Read(buffer)
	return "ovr-" + hex.EncodeToString(buffer)
}
