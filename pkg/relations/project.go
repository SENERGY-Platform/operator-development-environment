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
	"encoding/json"
	"fmt"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// LLMRelationView is the one projection of a relation profile that reaches a model
// (D26). There is one stored form and one projection function, as with a
// SeriesProfile.
//
// What gets dropped is chosen by what a model can act on. The pairwise tables are
// the biggest part of the document — one 2×2 table per pair per conditioning bucket,
// which for six members and six buckets is over two hundred tables — and a model
// reasoning about "is this rule worth showing the developer" needs the rule, not the
// tables it was derived from. So the pairs are summarised to their overall
// contingency and the rules keep their own numbers, and the elision says where the
// full document is.
type LLMRelationView struct {
	RelationID      string `json:"relation_id"`
	Tier            string `json:"tier"`
	DetectorVersion string `json:"detector_version"`

	Window      profiler.Window `json:"window"`
	GroupTime   string          `json:"group_time"`
	GridSeconds float64         `json:"grid_seconds"`
	Buckets     int             `json:"buckets"`
	Observed    int             `json:"observed"`

	Members        []MemberView    `json:"members"`
	Params         RuleParams      `json:"params"`
	Conditioning   Conditioning    `json:"conditioning"`
	CandidateRules []CandidateRule `json:"candidate_rules"`

	Reads  Reads              `json:"reads"`
	Notes  []string           `json:"notes"`
	Elided []profiler.Elision `json:"elided"`
}

// MemberView is a member without the bucket counts.
//
// The state derivation is kept in full for a usable member and in full for an
// unusable one — the reason a member could not be derived is the single most
// useful thing in this document when a rule the developer expected is absent, and
// eliding it would produce exactly the "absence read as negation" D24 exists to
// prevent.
type MemberView struct {
	Ref        profiler.SeriesRef `json:"ref"`
	Label      string             `json:"label"`
	DeviceName string             `json:"device_name,omitempty"`
	ProfileID  string             `json:"profile_id"`
	Unit       string             `json:"unit"`
	Kind       profiler.ValueKind `json:"kind"`
	State      StateSummary       `json:"state"`
}

// bytesPerToken is the same crude ratio the profiler's projection uses. Crude is
// fine: it is a bound, not an accounting.
const bytesPerToken = 4

// Project bounds a relation profile for a model (D26).
//
// maxRules caps the list, strongest first, because a per-item budget cannot bound a
// list. tokenBudget then reduces what remains. Both being zero means no pressure,
// which is what the HTTP surface wants and no tool wants.
func Project(profile RelationProfile, maxRules int, tokenBudget int) LLMRelationView {
	view := LLMRelationView{
		RelationID:      profile.RelationID,
		Tier:            profile.Tier,
		DetectorVersion: profile.DetectorVersion,
		Window:          profile.Window,
		GroupTime:       profile.GroupTime,
		GridSeconds:     profile.GridSeconds,
		Buckets:         profile.Buckets,
		Observed:        profile.Observed,
		Params:          profile.Params,
		Conditioning:    profile.Conditioning,
		Reads:           profile.Reads,
		Notes:           append([]string{}, profile.Notes...),
		Members:         make([]MemberView, 0, len(profile.Members)),
		Elided:          []profiler.Elision{},
	}
	for _, member := range profile.Members {
		view.Members = append(view.Members, MemberView{
			Ref:        member.Ref,
			Label:      member.Label,
			DeviceName: member.DeviceName,
			ProfileID:  member.ProfileID,
			Unit:       member.Unit,
			Kind:       member.Kind,
			State:      member.State,
		})
	}

	// The pairwise tables never reach a model. They are recorded as an elision with
	// the route that serves them, rather than being trimmed to a sample: a sample of
	// contingency tables is the one form of this data that invites a wrong conclusion,
	// because a model cannot tell which pairs it is missing.
	if len(profile.Pairs) > 0 {
		view.Elided = append(view.Elided, profiler.Elision{
			Field: "pairs", Total: len(profile.Pairs), Shown: 0,
			Fetch: RelationPath(profile.RelationID),
		})
	}

	rules := profile.CandidateRules
	if maxRules > 0 && len(rules) > maxRules {
		view.Elided = append(view.Elided, profiler.Elision{
			Field: "candidate_rules", Total: len(rules), Shown: maxRules,
			Fetch: RelationPath(profile.RelationID),
		})
		view.Notes = append(view.Notes, fmt.Sprintf(
			"%d candidate rules were proposed and the strongest %d are shown; raise min_confidence "+
				"or min_lift and ask again rather than treating this as the whole list",
			len(rules), maxRules))
		rules = rules[:maxRules]
	}
	view.CandidateRules = append([]CandidateRule{}, rules...)

	if tokenBudget > 0 {
		view = reduce(view, tokenBudget)
	}
	return view
}

// RelationPath is where the full document lives, for an elision's fetch reference.
func RelationPath(relationID string) string { return "/relations/" + relationID }

// reduce drops detail until the view fits the budget, in a fixed order, recording
// each drop.
//
// The order is by what a model loses least by losing. Exception detail goes before
// a whole rule does, because a rule without its exceptions is still a proposal worth
// putting to the developer while a dropped rule is invisible — but the *count* of
// exceptions is kept in the note, so "no exceptions" and "exceptions not shown"
// stay distinguishable.
func reduce(view LLMRelationView, tokenBudget int) LLMRelationView {
	fits := func(candidate LLMRelationView) bool {
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return true
		}
		return len(encoded)/bytesPerToken <= tokenBudget
	}
	if fits(view) {
		return view
	}

	// The developer's own decisions are dropped last of all and are not touched here:
	// a rule the developer already rejected is the single most important thing for a
	// model to know before proposing it again.
	trimmed := 0
	for i := range view.CandidateRules {
		if len(view.CandidateRules[i].Exceptions) > 1 {
			trimmed += len(view.CandidateRules[i].Exceptions) - 1
			view.CandidateRules[i].Exceptions = view.CandidateRules[i].Exceptions[:1]
		}
	}
	if trimmed > 0 {
		view.Elided = append(view.Elided, profiler.Elision{
			Field: "candidate_rules[].exceptions", Total: trimmed, Shown: 1,
			Fetch: RelationPath(view.RelationID),
		})
		if fits(view) {
			return view
		}
	}

	// Dropped one at a time so the strongest rule always survives, but recorded once:
	// an elision per iteration would be a dozen near-identical entries saying the same
	// thing, and the reader needs the total and what is left, not the path there.
	if total := len(view.CandidateRules); total > 1 {
		for len(view.CandidateRules) > 1 && !fits(view) {
			view.CandidateRules = view.CandidateRules[:len(view.CandidateRules)-1]
		}
		if len(view.CandidateRules) < total {
			view.Elided = append(view.Elided, profiler.Elision{
				Field: "candidate_rules", Total: total, Shown: len(view.CandidateRules),
				Fetch: RelationPath(view.RelationID),
			})
		}
		if fits(view) {
			return view
		}
	}

	// Notes are prose and the last thing worth paying for. Dropping them entirely
	// would hide a truncation, so the note that a truncation happened is what stays.
	if len(view.Notes) > 1 {
		total := len(view.Notes)
		view.Notes = view.Notes[total-1:]
		view.Elided = append(view.Elided, profiler.Elision{Field: "notes", Total: total, Shown: 1})
	}
	return view
}
