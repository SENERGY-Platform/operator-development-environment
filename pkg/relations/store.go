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
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// DecisionAction is what a developer did with a candidate rule.
//
// The three are profiler.OverrideAction's, deliberately: a developer confirming a
// unit and a developer confirming a rule are the same act of confirming derived
// semantics (§5.10), and giving the two different vocabularies would make the
// artifact harder to read for no gain.
type DecisionAction = profiler.OverrideAction

const (
	ActionConfirm = profiler.ActionConfirm
	ActionCorrect = profiler.ActionCorrect
	ActionReject  = profiler.ActionReject
)

// RuleDecision is one developer verdict on one candidate rule (§5.10, D21).
//
// Append-only, and keyed by the rule fingerprint rather than by the relation
// profile it was seen in. That is what makes it survive recomputation: a sharper
// detector produces a new relation id, and a decision tied to the old one would
// silently stop applying — the same failure ProfileOverride avoids by keying on the
// series rather than the profile.
//
// It is a developer action and has no tool. §5.8 lists writing a ProfileOverride
// among the capabilities that must not exist for an LLM, and for the same reason:
// a model that could confirm its own findings would be grading its own work.
type RuleDecision struct {
	DecisionID string    `json:"decision_id"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by"`

	RuleID string `json:"rule_id"`
	// RelationID and DetectorVersion record what the developer was looking at, so a
	// decision made against an older detector can be reviewed rather than carried
	// forward unexamined. Neither is used for lookup.
	RelationID      string `json:"relation_id"`
	DetectorVersion string `json:"detector_version,omitempty"`

	Action DecisionAction `json:"action"`
	// Computed is the rule as the detector stated it — the statement and the three
	// figures. Copied into the record rather than referenced, because the record has
	// to stay readable after the relation profile it came from has been evicted, and
	// because "the detector said 0.91 and the developer confirmed it" is the finding.
	Computed DecidedRule `json:"computed"`
	// Confirmed is the developer's own form of the rule, for a correction: a narrowed
	// statement, exceptions they added, exceptions they struck out. Nil for a plain
	// confirm or reject.
	Confirmed *DecidedRule `json:"confirmed,omitempty"`
	Note      string       `json:"note,omitempty"`
}

// DecidedRule is a rule as one side stated it.
type DecidedRule struct {
	Statement  string      `json:"statement"`
	Anomaly    string      `json:"anomaly,omitempty"`
	Support    float64     `json:"support,omitempty"`
	Confidence float64     `json:"confidence,omitempty"`
	Lift       float64     `json:"lift,omitempty"`
	Exceptions []Exception `json:"exceptions"`
}

func (d RuleDecision) Validate() error {
	if d.RuleID == "" {
		return fmt.Errorf("%w: rule_id must name the rule being decided", ErrInvalidDecision)
	}
	switch d.Action {
	case ActionConfirm, ActionCorrect, ActionReject:
	default:
		return fmt.Errorf("%w: action must be confirm, correct or reject, got %q",
			ErrInvalidDecision, d.Action)
	}
	if d.Action == ActionCorrect && d.Confirmed == nil {
		return fmt.Errorf("%w: a correction must carry the developer's own form of the rule",
			ErrInvalidDecision)
	}
	if d.Action == ActionCorrect && d.Confirmed.Statement == "" {
		return fmt.Errorf("%w: a corrected rule needs a statement", ErrInvalidDecision)
	}
	if d.CreatedBy == "" {
		return fmt.Errorf("%w: created_by must identify the developer", ErrInvalidDecision)
	}
	return nil
}

// Store keeps computed relation profiles and the decision log.
//
// The split in durability is the same one the profiler makes and for the same
// reason: a relation profile is a reproducible artifact whose loss costs a
// recomputation, and a decision is a developer's judgement whose loss destroys
// evidence that cannot be regenerated.
type Store interface {
	Put(profile RelationProfile) (stored RelationProfile, created bool, err error)
	ByID(relationID string) (RelationProfile, bool)

	AppendDecision(decision RuleDecision) (RuleDecision, error)
	// Decisions returns the log for each of the named rules, oldest first. A rule
	// with no decision is absent from the map rather than present and empty.
	Decisions(ruleIDs []string) map[string][]RuleDecision
}

// MemoryStore is an in-process Store. Relation profiles live here whether or not a
// database is configured; the decision log moves to Postgres when there is one (see
// NewOverlayStore).
type MemoryStore struct {
	mux       sync.RWMutex
	profiles  map[string]RelationProfile
	decisions map[string][]RuleDecision
	sequence  int64
	// maxProfiles bounds the map. Relation profiles are large — a pair count grows
	// with the square of the members — and an unbounded map in a long-lived process
	// is a leak with a delay on it. Zero means the default.
	maxProfiles int
	order       []string
}

func NewMemoryStore(maxProfiles int) *MemoryStore {
	if maxProfiles <= 0 {
		maxProfiles = defaultMaxStoredProfiles
	}
	return &MemoryStore{
		profiles:    map[string]RelationProfile{},
		decisions:   map[string][]RuleDecision{},
		maxProfiles: maxProfiles,
		order:       []string{},
	}
}

const defaultMaxStoredProfiles = 200

// Put stores a relation profile, or returns the one already stored under the same
// id.
//
// Immutable, like a SeriesProfile (D21): the id is the cache key, so a
// recomputation over the same window with the same detectors and the same members
// resolves to what is already there rather than replacing something a developer has
// read and decided against.
func (s *MemoryStore) Put(profile RelationProfile) (RelationProfile, bool, error) {
	if profile.RelationID == "" {
		return RelationProfile{}, false, errors.New("relations: refusing to store a profile with no id")
	}
	s.mux.Lock()
	defer s.mux.Unlock()

	if existing, found := s.profiles[profile.RelationID]; found {
		return existing, false, nil
	}
	s.profiles[profile.RelationID] = profile
	s.order = append(s.order, profile.RelationID)
	for len(s.order) > s.maxProfiles {
		delete(s.profiles, s.order[0])
		s.order = s.order[1:]
	}
	return profile, true, nil
}

func (s *MemoryStore) ByID(relationID string) (RelationProfile, bool) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	profile, found := s.profiles[relationID]
	return profile, found
}

func (s *MemoryStore) AppendDecision(decision RuleDecision) (RuleDecision, error) {
	if err := decision.Validate(); err != nil {
		return RuleDecision{}, err
	}
	s.mux.Lock()
	defer s.mux.Unlock()

	s.sequence++
	if decision.DecisionID == "" {
		decision.DecisionID = fmt.Sprintf("dec-%d", s.sequence)
	}
	if decision.CreatedAt.IsZero() {
		decision.CreatedAt = time.Now().UTC()
	}
	s.decisions[decision.RuleID] = append(s.decisions[decision.RuleID], decision)
	return decision, nil
}

func (s *MemoryStore) Decisions(ruleIDs []string) map[string][]RuleDecision {
	s.mux.RLock()
	defer s.mux.RUnlock()

	out := map[string][]RuleDecision{}
	for _, id := range ruleIDs {
		stored := s.decisions[id]
		if len(stored) == 0 {
			continue
		}
		copied := make([]RuleDecision, len(stored))
		copy(copied, stored)
		sortDecisions(copied)
		out[id] = copied
	}
	return out
}

// sortDecisions orders a log oldest first, breaking a tie on the id so a page is
// reproducible. Two decisions can share a timestamp: the clock is not a source of
// uniqueness under concurrency.
func sortDecisions(decisions []RuleDecision) {
	sort.SliceStable(decisions, func(i, j int) bool {
		if decisions[i].CreatedAt.Equal(decisions[j].CreatedAt) {
			return decisions[i].DecisionID < decisions[j].DecisionID
		}
		return decisions[i].CreatedAt.Before(decisions[j].CreatedAt)
	})
}

// Decisions is a persistent home for the decision log alone.
type Decisions interface {
	Append(decision RuleDecision) (RuleDecision, error)
	ForRules(ruleIDs []string) map[string][]RuleDecision
}

// OverlayStore is a Store whose relation profiles are in memory and whose decisions
// are somewhere durable.
type OverlayStore struct {
	profiles  Store
	decisions Decisions
}

func NewOverlayStore(profiles Store, decisions Decisions) *OverlayStore {
	return &OverlayStore{profiles: profiles, decisions: decisions}
}

func (s *OverlayStore) Put(profile RelationProfile) (RelationProfile, bool, error) {
	return s.profiles.Put(profile)
}

func (s *OverlayStore) ByID(relationID string) (RelationProfile, bool) {
	return s.profiles.ByID(relationID)
}

func (s *OverlayStore) AppendDecision(decision RuleDecision) (RuleDecision, error) {
	if err := decision.Validate(); err != nil {
		return RuleDecision{}, err
	}
	return s.decisions.Append(decision)
}

func (s *OverlayStore) Decisions(ruleIDs []string) map[string][]RuleDecision {
	return s.decisions.ForRules(ruleIDs)
}

// latest picks the decision that currently stands for a rule.
//
// The log is append-only, so a developer who changes their mind adds a record
// rather than replacing one, and the newest is the one in force. The history stays
// readable through the decision route.
func latest(decisions []RuleDecision) *RuleDecision {
	if len(decisions) == 0 {
		return nil
	}
	sortDecisions(decisions)
	out := decisions[len(decisions)-1]
	return &out
}
