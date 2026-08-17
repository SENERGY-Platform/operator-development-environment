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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrProfileNotFound = errors.New("profile not found")
	ErrInvalidCursor   = errors.New("invalid session cursor")
)

// CacheKey identifies a profile by everything that can change its content
// (D25). detector_version belongs in the key: without it, improving the session
// detector leaves stale profiles in the LLM's context with nothing to notice
// them by.
func CacheKey(ref SeriesRef, analysis Window, raw Window, detectorVersion string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		ref.String(),
		analysis.String(),
		raw.String(),
		detectorVersion,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// SessionQuery is one page of the session resource (D27).
type SessionQuery struct {
	From   time.Time
	To     time.Time
	Limit  int
	Cursor string
}

type SessionPage struct {
	Sessions   []Session `json:"sessions"`
	Total      int       `json:"total"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

const (
	DefaultSessionPageSize = 100
	MaxSessionPageSize     = 1000
)

// Store keeps computed profiles and the override overlay.
//
// Profiles are immutable (D21): a profile already stored under a cache key is
// returned unchanged rather than replaced, so a recomputation cannot quietly
// alter something a developer has already read and confirmed against.
//
// Overrides are appended, never updated, and are looked up by series reference
// rather than by profile id. That is what makes a confirmation survive
// recomputation: a new detector version produces a new profile id, and an
// override tied to the old id would silently stop applying.
type Store interface {
	Put(profile SeriesProfile, sessions []Session) (stored SeriesProfile, created bool, err error)
	ByID(profileID string) (SeriesProfile, bool)
	ByCacheKey(cacheKey string) (SeriesProfile, bool)

	AppendOverride(override ProfileOverride) (ProfileOverride, error)
	Overrides(ref SeriesRef) []ProfileOverride

	Sessions(profileID string, query SessionQuery) (SessionPage, error)
}

// MemoryStore is an in-process Store.
//
// It is deliberately the only implementation for now. A persistent store means
// choosing and deploying a database, which is a decision this milestone does not
// need to make, and the interface is here so that choice can be made later
// without touching the profiler. The cost is real and worth stating: a restart
// loses computed profiles, which is merely a recomputation, and loses the
// override log, which is developer input and an empirical record (§5.4.3).
type MemoryStore struct {
	mux       sync.RWMutex
	profiles  map[string]SeriesProfile
	sessions  map[string][]Session
	overrides map[string][]ProfileOverride
	sequence  int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		profiles:  map[string]SeriesProfile{},
		sessions:  map[string][]Session{},
		overrides: map[string][]ProfileOverride{},
	}
}

func (s *MemoryStore) Put(profile SeriesProfile, sessions []Session) (SeriesProfile, bool, error) {
	if profile.ProfileID == "" {
		return SeriesProfile{}, false, errors.New("profiler: refusing to store a profile with no id")
	}
	s.mux.Lock()
	defer s.mux.Unlock()

	if existing, found := s.profiles[profile.ProfileID]; found {
		return existing, false, nil
	}
	s.profiles[profile.ProfileID] = profile

	stored := append([]Session{}, sessions...)
	sort.SliceStable(stored, func(i, j int) bool { return stored[i].From.Before(stored[j].From) })
	s.sessions[profile.ProfileID] = stored
	return profile, true, nil
}

func (s *MemoryStore) ByID(profileID string) (SeriesProfile, bool) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	profile, found := s.profiles[profileID]
	return profile, found
}

// ByCacheKey is ByID: the profile id *is* the cache key. Keeping them the same
// means a caller holding a profile can check it for staleness without a second
// index, and a recomputation over the same windows with the same detectors
// resolves to the same profile rather than a duplicate.
func (s *MemoryStore) ByCacheKey(cacheKey string) (SeriesProfile, bool) {
	return s.ByID(cacheKey)
}

func (s *MemoryStore) AppendOverride(override ProfileOverride) (ProfileOverride, error) {
	if err := override.Validate(); err != nil {
		return ProfileOverride{}, err
	}
	s.mux.Lock()
	defer s.mux.Unlock()

	s.sequence++
	override.OverrideID = fmt.Sprintf("ovr-%d", s.sequence)
	if override.CreatedAt.IsZero() {
		override.CreatedAt = time.Now().UTC()
	}
	key := override.SeriesRef.String()
	s.overrides[key] = append(s.overrides[key], override)
	return override, nil
}

func (s *MemoryStore) Overrides(ref SeriesRef) []ProfileOverride {
	s.mux.RLock()
	defer s.mux.RUnlock()
	stored := s.overrides[ref.String()]
	out := make([]ProfileOverride, len(stored))
	copy(out, stored)
	return out
}

func (s *MemoryStore) Sessions(profileID string, query SessionQuery) (SessionPage, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	if _, found := s.profiles[profileID]; !found {
		return SessionPage{}, fmt.Errorf("%w: %s", ErrProfileNotFound, profileID)
	}
	all := s.sessions[profileID]

	filtered := make([]Session, 0, len(all))
	for _, session := range all {
		if !query.From.IsZero() && session.To.Before(query.From) {
			continue
		}
		if !query.To.IsZero() && session.From.After(query.To) {
			continue
		}
		filtered = append(filtered, session)
	}

	offset := 0
	if query.Cursor != "" {
		parsed, err := strconv.Atoi(query.Cursor)
		if err != nil || parsed < 0 {
			return SessionPage{}, fmt.Errorf("%w: %q", ErrInvalidCursor, query.Cursor)
		}
		offset = parsed
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultSessionPageSize
	}
	if limit > MaxSessionPageSize {
		limit = MaxSessionPageSize
	}

	page := SessionPage{Total: len(filtered), Sessions: []Session{}}
	if offset >= len(filtered) {
		return page, nil
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page.Sessions = append(page.Sessions, filtered[offset:end]...)
	if end < len(filtered) {
		// The cursor is an opaque offset into the filtered set. Callers must not
		// interpret it, and it is only valid for the same from/to filter.
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}
