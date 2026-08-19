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

package charts

import (
	"sort"
	"sync"
)

// Store keeps chart specifications.
//
// In memory, and unlike the override overlay that is not a gap to be filled
// later. The repository's rule is that what a restart may lose is what a restart
// can recompute: computed profiles stay in memory and the confirmations against
// them go to Postgres, because one is an artifact and the other is an empirical
// record (§5.4.3). A chart specification is on the artifact side — the model or
// the developer re-emits it in a sentence — while the confirmations a chart
// produces are written to the profiler's overlay and are therefore already as
// durable as the deployment allows.
type Store interface {
	Put(spec Spec)
	ByID(chartID string) (Spec, bool)
	// ForUser lists a developer's charts, newest first, optionally narrowed to one
	// chat session.
	ForUser(userSub, sessionID string, limit int) []Spec
	Remove(chartID string)
}

// MemoryStore is the in-process Store, bounded per developer.
//
// The bound is not a memory optimisation: a chat session can call render_chart on
// every turn, and an unbounded map of specifications held for the process lifetime
// is a slow leak that only shows up in a long-running deployment.
type MemoryStore struct {
	mux    sync.RWMutex
	charts map[string]Spec
	// order is per developer and in creation order, so eviction drops the oldest
	// chart of the developer who is over the bound rather than someone else's.
	order   map[string][]string
	perUser int
}

const defaultChartsPerUser = 100

func NewMemoryStore(perUser int) *MemoryStore {
	if perUser <= 0 {
		perUser = defaultChartsPerUser
	}
	return &MemoryStore{
		charts:  map[string]Spec{},
		order:   map[string][]string{},
		perUser: perUser,
	}
}

func (s *MemoryStore) Put(spec Spec) {
	s.mux.Lock()
	defer s.mux.Unlock()

	if _, exists := s.charts[spec.ChartID]; !exists {
		s.order[spec.CreatedBy] = append(s.order[spec.CreatedBy], spec.ChartID)
	}
	s.charts[spec.ChartID] = spec

	ids := s.order[spec.CreatedBy]
	for len(ids) > s.perUser {
		delete(s.charts, ids[0])
		ids = ids[1:]
	}
	s.order[spec.CreatedBy] = ids
}

func (s *MemoryStore) ByID(chartID string) (Spec, bool) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	spec, found := s.charts[chartID]
	return spec, found
}

func (s *MemoryStore) ForUser(userSub, sessionID string, limit int) []Spec {
	s.mux.RLock()
	defer s.mux.RUnlock()

	out := []Spec{}
	for _, id := range s.order[userSub] {
		spec, found := s.charts[id]
		if !found {
			continue
		}
		if sessionID != "" && spec.SessionID != sessionID {
			continue
		}
		out = append(out, spec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *MemoryStore) Remove(chartID string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	spec, found := s.charts[chartID]
	if !found {
		return
	}
	delete(s.charts, chartID)
	ids := s.order[spec.CreatedBy]
	for i, id := range ids {
		if id == chartID {
			s.order[spec.CreatedBy] = append(ids[:i:i], ids[i+1:]...)
			break
		}
	}
}
