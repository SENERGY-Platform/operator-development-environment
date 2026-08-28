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

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
)

// fakeRelations records what the two M6 tools asked of §5.5's service.
type fakeRelations struct {
	token     string
	proposals []relations.ProposalRequest
	requests  []relations.Request

	proposal relations.Proposal
	profile  relations.RelationProfile
}

func (f *fakeRelations) ProposeRelatedSets(
	_ context.Context, token string, req relations.ProposalRequest,
) (relations.Proposal, error) {
	f.token = token
	f.proposals = append(f.proposals, req)
	return f.proposal, nil
}

func (f *fakeRelations) Relate(
	_ context.Context, token string, req relations.Request,
) (relations.RelationProfile, error) {
	f.token = token
	f.requests = append(f.requests, req)
	if req.Progress != nil {
		req.Progress(relations.Phase{Stage: relations.PhaseAlign, Detail: "one batched read"})
	}
	return f.profile, nil
}

func (f *fakeRelations) MaxMembers() int { return 6 }

// fakeSelectionSink stands in for the chat store, so the L0 count below covers
// propose_data_selection too.
type fakeSelectionSink struct{}

func (fakeSelectionSink) PutProposedSelection(context.Context, string, ProposedSelection) error {
	return nil
}

func kitchenProposal() relations.Proposal {
	return relations.Proposal{
		AspectID:   "kitchen",
		AspectName: "Kitchen",
		Sets: []relations.CandidateSet{{
			SetID:     "set-1",
			Origin:    relations.OriginGraphSiblings,
			Name:      "Kitchen circuit",
			Rationale: "all 2 devices feed Kitchen circuit in the graph Kitchen sub-metering",
			Devices:   2,
			Members: []relations.SetMember{
				{Ref: profiler.SeriesRef{DeviceID: "d1", ServiceID: "s1", VariablePath: "value.power"},
					Label: "Oven power", DeviceName: "Oven", Unit: "W"},
				{Ref: profiler.SeriesRef{DeviceID: "d2", ServiceID: "s1", VariablePath: "value.power"},
					Label: "Kitchen lights power", DeviceName: "Kitchen lights", Unit: "W"},
			},
			Notes: []string{},
		}},
		Reads: relations.Reads{},
		Notes: []string{},
	}
}

func kitchenRelation(rules int) relations.RelationProfile {
	profile := relations.RelationProfile{
		RelationID:      "rel-1",
		Tier:            relations.TierRelation,
		DetectorVersion: relations.DetectorVersion,
		GroupTime:       "15m",
		GridSeconds:     900,
		Buckets:         2880,
		Observed:        2880,
		Params:          relations.DefaultRuleParams(),
		Conditioning:    relations.DefaultConditioning(),
		CandidateSetID:  "set-1",
		Members: []relations.Member{
			{Ref: profiler.SeriesRef{DeviceID: "d1", ServiceID: "s1", VariablePath: "value.power"},
				Label: "the oven", State: relations.StateSummary{Usable: true, Threshold: 100}},
			{Ref: profiler.SeriesRef{DeviceID: "d2", ServiceID: "s1", VariablePath: "value.power"},
				Label: "the kitchen lights", State: relations.StateSummary{Usable: true, Threshold: 30}},
		},
		Pairs: []relations.PairRelation{{A: 0, B: 1, Overall: relations.Contingency{
			ActiveActive: 360, ActiveIdle: 60, IdleIdle: 2460, Observed: 2880,
		}}},
		Reads: relations.Reads{Aligned: 1, Profiles: 4, Values: 5},
		Notes: []string{},
	}
	for i := 0; i < rules; i++ {
		profile.CandidateRules = append(profile.CandidateRules, relations.CandidateRule{
			RuleID:     "rule-" + string(rune('a'+i)),
			Antecedent: relations.RuleTerm{Member: 0, Label: "the oven", State: relations.StateActive},
			Consequent: relations.RuleTerm{Member: 1, Label: "the kitchen lights", State: relations.StateActive},
			Statement:  "the oven active → the kitchen lights active",
			Anomaly:    "the oven active while the kitchen lights idle",
			Support:    0.125, Confidence: 0.8571, Lift: 6.86, Samples: 420, Violations: 60,
			Strength: profiler.Likely,
			Exceptions: []relations.Exception{{
				Dimension: relations.DimensionHourOfDay, Bucket: "06:00-12:00",
				FromHour: 6, ToHour: 12, Samples: 60, Confidence: 0,
			}},
			Advisory: "candidate only",
		})
	}
	return profile
}

// propose_related_sets is L0 and that is the substantive claim of M6's tool half:
// the hardest part of a multi-device pattern is knowing which devices to look at, and
// the ontology answers it without a value being read.
func TestProposeRelatedSetsSitsAtL0AndReadsNoValues(t *testing.T) {
	fake := &fakeRelations{proposal: kitchenProposal()}
	definition, dispatcher := executorFor(t, Deps{Relations: fake}, "propose_related_sets")

	if definition.MinTier != L0 {
		t.Errorf("min tier = %v, §5.8 says L0", definition.MinTier)
	}
	if definition.Confirm {
		t.Error("propose_related_sets requires confirmation; §5.8 does not")
	}

	decoded := dispatchJSON(t, dispatcher, L0, "propose_related_sets",
		`{"aspect_id": "kitchen", "include_descendants": true}`)

	if len(fake.proposals) != 1 {
		t.Fatalf("proposals = %d, want 1", len(fake.proposals))
	}
	if fake.proposals[0].AspectID != "kitchen" {
		t.Errorf("aspect id = %q, want kitchen", fake.proposals[0].AspectID)
	}
	if !fake.proposals[0].IncludeDescendants {
		t.Error("include_descendants was dropped between the model and the service")
	}
	if fake.token != "Bearer t" {
		t.Errorf("token = %q, want the caller's", fake.token)
	}

	reads, ok := decoded["reads"].(map[string]any)
	if !ok {
		t.Fatalf("no reads block in %v", decoded)
	}
	if reads["values"] != float64(0) {
		t.Errorf("values read = %v, want 0 — the tier claim has to be checkable from the answer",
			reads["values"])
	}
	if sets, ok := decoded["sets"].([]any); !ok || len(sets) != 1 {
		t.Errorf("sets = %v, want one", decoded["sets"])
	}
	note, _ := decoded["note"].(string)
	if !strings.Contains(note, "relate_series") {
		t.Errorf("note = %q, want it to point at the next step", note)
	}
}

// An empty answer must not read as "these devices are unrelated": the cause is more
// often an ontology gap or a permission.
func TestAnEmptyProposalSaysWhatAnEmptyListDoesNotMean(t *testing.T) {
	fake := &fakeRelations{proposal: relations.Proposal{
		AspectID: "kitchen", Sets: []relations.CandidateSet{},
		Notes: []string{"device types under this aspect exist, but this developer may read none"},
	}}
	_, dispatcher := executorFor(t, Deps{Relations: fake}, "propose_related_sets")

	decoded := dispatchJSON(t, dispatcher, L0, "propose_related_sets", `{"aspect_id": "kitchen"}`)
	note, _ := decoded["note"].(string)
	if !strings.Contains(note, "Do not conclude") {
		t.Errorf("note = %q, want it to warn against reading absence as negation (D24)", note)
	}
}

func TestProposeRelatedSetsNeedsAnAspect(t *testing.T) {
	fake := &fakeRelations{proposal: kitchenProposal()}
	_, dispatcher := executorFor(t, Deps{Relations: fake}, "propose_related_sets")

	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", Tier: L0},
		Call{ID: "c1", Name: "propose_related_sets", Input: json.RawMessage(`{}`)})
	if !result.IsError {
		t.Error("a call with no aspect id succeeded")
	}
	if len(fake.proposals) != 0 {
		t.Error("the service was called for a request that could not be answered")
	}
}

// relate_series reads values and is therefore L1, not L0 — and returns none of them,
// which is why it is not L2.
func TestRelateSeriesSitsAtL1AndReturnsNoValues(t *testing.T) {
	fake := &fakeRelations{profile: kitchenRelation(1)}
	definition, dispatcher := executorFor(t, Deps{Relations: fake}, "relate_series")

	if definition.MinTier != L1 {
		t.Errorf("min tier = %v, §5.8 says L1", definition.MinTier)
	}

	decoded := dispatchJSON(t, dispatcher, L1, "relate_series", `{
	  "series": [
	    {"device_id": "d1", "service_id": "s1", "variable_path": "value.power", "label": "the oven"},
	    {"device_id": "d2", "service_id": "s1", "variable_path": "value.power", "label": "the kitchen lights"}
	  ],
	  "candidate_set_id": "set-1",
	  "min_confidence": 0.8,
	  "hour_buckets": 6
	}`)

	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}
	request := fake.requests[0]
	if len(request.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(request.Members))
	}
	if request.Members[0].Label != "the oven" {
		t.Errorf("label = %q, want the model's own: a rule nobody can read is a rule nobody "+
			"can confirm", request.Members[0].Label)
	}
	if request.Params.MinConfidence != 0.8 || request.Params.HourBuckets != 6 {
		t.Errorf("params = %+v, want the model's thresholds carried through", request.Params)
	}
	if request.CandidateSetID != "set-1" {
		t.Errorf("candidate_set_id = %q, want it carried so a confirmed rule is traceable",
			request.CandidateSetID)
	}

	relation, ok := decoded["relation"].(map[string]any)
	if !ok {
		t.Fatalf("no relation in %v", decoded)
	}
	if relation["tier"] != "L1" {
		t.Errorf("tier = %v, want L1", relation["tier"])
	}
	// The pairwise tables never reach the model — the projection elides them whole
	// rather than sampling, because a model cannot tell which pairs it is missing.
	if _, present := relation["pairs"]; present {
		t.Error("the projection carries the pairwise tables; they belong behind the relation route")
	}
	elided, _ := relation["elided"].([]any)
	if len(elided) == 0 {
		t.Error("the tables were dropped with nothing recording it (D26)")
	}

	// No series value appears anywhere in what the model receives.
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"\"values\":[", "\"points\":["} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the response carries %s; relate_series returns no values", forbidden)
		}
	}
}

func TestRelateSeriesIsBlockedAtTheDefaultTier(t *testing.T) {
	fake := &fakeRelations{profile: kitchenRelation(1)}
	_, dispatcher := executorFor(t, Deps{Relations: fake}, "relate_series")

	result := dispatcher.Dispatch(context.Background(),
		Request{Token: "Bearer t", Tier: L0},
		Call{ID: "c1", Name: "relate_series", Input: json.RawMessage(`{
		  "series": [
		    {"device_id": "d1", "service_id": "s1", "variable_path": "value.power"},
		    {"device_id": "d2", "service_id": "s1", "variable_path": "value.power"}
		  ]}`)})

	if !result.IsError {
		t.Fatal("relate_series ran at L0")
	}
	if len(fake.requests) != 0 {
		t.Error("the platform was read before the tier was checked")
	}
	encoded, _ := json.Marshal(result.Content)
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["blocked_by_tier"] != "L0" || decoded["required"] != "L1" {
		t.Errorf("refusal = %v, want the §3.2 shape the model relays", decoded)
	}
}

func TestRelateSeriesNeedsTwoWellFormedSeries(t *testing.T) {
	fake := &fakeRelations{profile: kitchenRelation(1)}
	_, dispatcher := executorFor(t, Deps{Relations: fake}, "relate_series")

	for name, input := range map[string]string{
		"one series": `{"series": [{"device_id": "d1", "service_id": "s1", "variable_path": "value.power"}]}`,
		"no path": `{"series": [
		  {"device_id": "d1", "service_id": "s1"},
		  {"device_id": "d2", "service_id": "s1", "variable_path": "value.power"}]}`,
		"bad window": `{"series": [
		  {"device_id": "d1", "service_id": "s1", "variable_path": "value.power"},
		  {"device_id": "d2", "service_id": "s1", "variable_path": "value.power"}],
		  "from": "yesterday"}`,
	} {
		result := dispatcher.Dispatch(context.Background(),
			Request{Token: "Bearer t", Tier: L1},
			Call{ID: "c1", Name: "relate_series", Input: json.RawMessage(input)})
		if !result.IsError {
			t.Errorf("%s: the call succeeded", name)
		}
	}
	if len(fake.requests) != 0 {
		t.Errorf("the service was called %d times for requests that could not be answered",
			len(fake.requests))
	}
}

// The projection has to bound breadth as well as depth: a twenty-rule relation would
// otherwise answer with twenty times a budget written to bound what a model reads.
func TestRelateSeriesBoundsTheRuleList(t *testing.T) {
	fake := &fakeRelations{profile: kitchenRelation(20)}
	_, dispatcher := executorFor(t, Deps{Relations: fake, RelationMaxRules: 3}, "relate_series")

	decoded := dispatchJSON(t, dispatcher, L1, "relate_series", `{"series": [
	  {"device_id": "d1", "service_id": "s1", "variable_path": "value.power"},
	  {"device_id": "d2", "service_id": "s1", "variable_path": "value.power"}]}`)

	relation, _ := decoded["relation"].(map[string]any)
	rules, _ := relation["candidate_rules"].([]any)
	if len(rules) != 3 {
		t.Errorf("rules = %d, want the cap of 3", len(rules))
	}
	notes, _ := relation["notes"].([]any)
	truncation := false
	for _, note := range notes {
		if text, ok := note.(string); ok && strings.Contains(text, "strongest") {
			truncation = true
		}
	}
	if !truncation {
		t.Errorf("notes = %v, want one saying the list was truncated", notes)
	}
}

// The note is the boundary D28 draws, restated where a model will read it.
func TestRelateSeriesTellsTheModelTheRulesAreCandidates(t *testing.T) {
	fake := &fakeRelations{profile: kitchenRelation(1)}
	_, dispatcher := executorFor(t, Deps{Relations: fake}, "relate_series")

	decoded := dispatchJSON(t, dispatcher, L1, "relate_series", `{"series": [
	  {"device_id": "d1", "service_id": "s1", "variable_path": "value.power"},
	  {"device_id": "d2", "service_id": "s1", "variable_path": "value.power"}]}`)

	note, _ := decoded["note"].(string)
	for _, want := range []string{"candidate", "developer", "confirm"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to mention %q", note, want)
		}
	}
}

// A rule the developer already rejected is the most important thing for a model to
// know before proposing it again.
func TestAnAlreadyDecidedRuleIsCalledOutInTheNote(t *testing.T) {
	profile := kitchenRelation(1)
	profile.CandidateRules[0].Decision = &relations.RuleDecision{
		RuleID: profile.CandidateRules[0].RuleID,
		Action: relations.ActionReject,
		Note:   "the morning run is intentional",
	}
	fake := &fakeRelations{profile: profile}
	_, dispatcher := executorFor(t, Deps{Relations: fake}, "relate_series")

	decoded := dispatchJSON(t, dispatcher, L1, "relate_series", `{"series": [
	  {"device_id": "d1", "service_id": "s1", "variable_path": "value.power"},
	  {"device_id": "d2", "service_id": "s1", "variable_path": "value.power"}]}`)

	note, _ := decoded["note"].(string)
	if !strings.Contains(note, "developer decision") {
		t.Errorf("note = %q, want it to say a rule already carries a decision", note)
	}

	relation, _ := decoded["relation"].(map[string]any)
	rules, _ := relation["candidate_rules"].([]any)
	rule, _ := rules[0].(map[string]any)
	if _, present := rule["decision"]; !present {
		t.Error("the decision was dropped from the projection")
	}
}

// A relational pass is the slowest tool on the surface, so its phases are relayed.
func TestRelateSeriesForwardsItsPhases(t *testing.T) {
	fake := &fakeRelations{profile: kitchenRelation(1)}
	registry, err := NewSurface(Deps{Relations: fake})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	dispatcher, err := NewDispatcher(registry, nil, &sequentialIDs{})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	seen := []Progress{}
	result := dispatcher.Dispatch(context.Background(),
		Request{
			Token: "Bearer t", Tier: L1,
			Report: func(progress Progress) { seen = append(seen, progress) },
		},
		Call{ID: "c1", Name: "relate_series", Input: json.RawMessage(`{"series": [
		  {"device_id": "d1", "service_id": "s1", "variable_path": "value.power"},
		  {"device_id": "d2", "service_id": "s1", "variable_path": "value.power"}]}`)})
	if result.IsError {
		t.Fatalf("relate_series failed: %v", result.Content)
	}

	found := false
	for _, progress := range seen {
		if progress.Stage == relations.PhaseAlign {
			found = true
		}
	}
	if !found {
		t.Errorf("the align phase was not relayed; got %+v", seen)
	}
}

// Without a relational profiler both tools stay declared and unavailable rather than
// registered and broken — the degradation the rest of the surface already does.
func TestBothRelationalToolsAreUnavailableWithoutTheService(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, name := range []string{"propose_related_sets", "relate_series"} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%q is not declared", name)
		}
		if definition.Implemented() {
			t.Errorf("%q has an executor with no relational profiler configured", name)
		}
		if definition.Unavailable == "" {
			t.Errorf("%q is unavailable without saying why", name)
		}
	}
}

// No tool decides a rule, for the reason §5.8 gives about writing a ProfileOverride:
// a model that could confirm its own findings would be grading its own work.
//
// The guarantee is structural rather than a naming convention — the capability is in
// the denied set, so NewRegistry refuses to register it and the surface cannot gain
// one by accident. This checks both halves.
func TestNoToolConfirmsARule(t *testing.T) {
	registry, err := NewSurface(Deps{Relations: &fakeRelations{}})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, definition := range registry.Definitions() {
		name := strings.ToLower(definition.Name)
		if strings.Contains(name, "decide") || strings.Contains(name, "confirm_rule") ||
			strings.Contains(name, "rule_decision") {
			t.Errorf("%q exists; deciding a candidate rule is a developer action only",
				definition.Name)
		}
	}

	if _, denied := Denied()["decide_relation_rule"]; !denied {
		t.Error("deciding a rule is not in the denied set, so nothing stops a later " +
			"milestone from registering one")
	}
}

// The README quotes what a deployment reports at startup, and a hand-counted figure
// in prose is a figure that rots. This pins the one M6 moved: propose_related_sets is
// the ninth tool a session can reach at the default tier, which is what makes the
// milestone's tier claim concrete — a developer can get from "which devices are
// related" to a shortlist without raising the tier at all.
func TestProposeRelatedSetsIsReachableAtTheDefaultTier(t *testing.T) {
	// Everything a deployment with a timescale-wrapper has, and no JupyterHub — which
	// is the deployment the README's startup log was taken from. run_code is L0 too and
	// would make it ten with a Hub configured.
	registry, err := NewSurface(Deps{
		Ontology:      &fakeOntology{snapshot: nil},
		Devices:       &fakeDevices{},
		Timeseries:    &fakeTimeseries{},
		Profiler:      newTestProfiler(t),
		Selection:     &fakeSelection{},
		SelectionSink: &fakeSelectionSink{},
		Relations:     &fakeRelations{},
	})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}

	available := registry.Available(L0)
	names := make([]string, 0, len(available))
	for _, definition := range available {
		names = append(names, definition.Name)
	}
	// Ten with probe_export_data, which the README quotes alongside the rest.
	if len(available) != 10 {
		t.Errorf("L0 tools = %d, want 10 — the figure the README quotes: %v", len(available), names)
	}
	found := false
	for _, name := range names {
		if name == "propose_related_sets" {
			found = true
		}
	}
	if !found {
		t.Errorf("propose_related_sets is not reachable at L0; got %v", names)
	}
}
