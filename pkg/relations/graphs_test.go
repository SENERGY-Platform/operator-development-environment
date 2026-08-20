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
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
)

// The graph fixture is a small sub-metering topology, of the shape
// models.Graph.Valid permits: directed, acyclic, outgoing weights summing to 100 per
// node, one non-device sink.
//
//	oven ──100──┐
//	            ├──▶ kitchen circuit ──100──▶ site meter ──100──▶ grid
//	lights ─100─┘
//
// So the oven and the lights are **siblings** — they meet at the kitchen circuit —
// and the site meter is **downstream** of that circuit, which makes it a sub-meter
// relationship rather than a shared location. The site meter is deliberately not
// under the Kitchen aspect: that is the case the whole cross-level resolution exists
// for, and a design that intersected the graph with the aspect's own devices would
// lose it.
//
// Every weight here is 100, and that is not laziness: the validator requires each
// node's *outgoing* weights to sum to 100, so a device feeding one node can carry
// nothing else. A split allocation needs a device with two parents, which
// TestASplitAllocationCarriesItsShares covers separately.
func kitchenGraph() models.Graph {
	return models.Graph{
		Id:         "graph-1",
		Owner:      "user-1",
		Attributes: []models.Attribute{{Key: "name", Value: "Kitchen sub-metering"}},
		Nodes: []models.Node{
			{Id: "n-oven", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-oven"},
			{Id: "n-lights", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-lights"},
			{Id: "n-circuit", Attributes: []models.Attribute{{Key: "name", Value: "Kitchen circuit"}}},
			{Id: "n-site", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-site"},
			{Id: "n-grid", Attributes: []models.Attribute{{Key: "name", Value: "Grid connection"}}},
		},
		Edges: []models.Edge{
			{Id: "e-1", FromNodeId: "n-oven", ToNodeId: "n-circuit", Weight: 100},
			{Id: "e-2", FromNodeId: "n-lights", ToNodeId: "n-circuit", Weight: 100},
			{Id: "e-3", FromNodeId: "n-circuit", ToNodeId: "n-site", Weight: 100},
			{Id: "e-4", FromNodeId: "n-site", ToNodeId: "n-grid", Weight: 100},
		},
	}
}

// The fixture is only worth anything if the platform would accept it, so the
// validator it has to satisfy is run against it here rather than assumed.
func TestTheGraphFixtureIsOneThePlatformWouldAccept(t *testing.T) {
	graph := kitchenGraph()
	if err := graph.Valid(); err != nil {
		t.Fatalf("the fixture graph is not valid upstream, so nothing built on it means anything: %v", err)
	}
}

func setByOrigin(sets []CandidateSet, origin string) (CandidateSet, bool) {
	for _, set := range sets {
		if set.Origin == origin {
			return set, true
		}
	}
	return CandidateSet{}, false
}

func memberFor(set CandidateSet, deviceID string) (SetMember, bool) {
	for _, member := range set.Members {
		if member.Ref.DeviceID == deviceID {
			return member, true
		}
	}
	return SetMember{}, false
}

func TestGraphSiblingsAreProposedWithTheNodeTheyMeetAt(t *testing.T) {
	h := newHarness(t)
	h.ontology.graphs = []models.Graph{kitchenGraph()}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}

	set, found := setByOrigin(proposal.Sets, OriginGraphSiblings)
	if !found {
		t.Fatalf("no sibling set proposed; got %v and notes %v",
			setSummaries(proposal.Sets), proposal.Notes)
	}
	if set.Devices != 2 {
		t.Errorf("the sibling set spans %d devices, want the oven and the lights", set.Devices)
	}
	if set.Name != "Kitchen circuit" {
		t.Errorf("name = %q, want the node they meet at", set.Name)
	}
	if set.GraphID != "graph-1" || set.GraphNode != "n-circuit" {
		t.Errorf("graph = %q node = %q, want graph-1 / n-circuit", set.GraphID, set.GraphNode)
	}
	if !strings.Contains(set.Rationale, "Kitchen circuit") {
		t.Errorf("rationale = %q, want it to name where they meet — that is what a graph adds "+
			"over a shared label", set.Rationale)
	}

	oven, ok := memberFor(set, "dev-oven")
	if !ok {
		t.Fatal("the oven is absent from its own sibling set")
	}
	if oven.Graph == nil {
		t.Fatal("the oven carries no graph placement")
	}
	if oven.Graph.Role != RoleSibling {
		t.Errorf("role = %q, want %q", oven.Graph.Role, RoleSibling)
	}
	// 100 because the oven feeds exactly one node, which is what the validator
	// requires — see the note on GraphPlacement.Weight.
	if oven.Graph.Weight != 100 {
		t.Errorf("weight = %d, want 100: a device feeding one node can carry nothing else",
			oven.Graph.Weight)
	}
	if oven.Graph.ViaName != "Kitchen circuit" {
		t.Errorf("via = %q, want the node's name", oven.Graph.ViaName)
	}

	// The graph is the most specific statement available, so it leads.
	if proposal.Sets[0].Origin != OriginGraphSiblings {
		t.Errorf("the first set's origin is %q, want %q: a graph carries direction and share "+
			"as well as membership", proposal.Sets[0].Origin, OriginGraphSiblings)
	}
}

// The sub-metering case, and the reason graph sets are not intersected with the
// aspect: the site meter is not in the kitchen.
func TestASubMeterOutsideTheAspectIsStillProposed(t *testing.T) {
	h := newHarness(t)
	h.ontology.graphs = []models.Graph{kitchenGraph()}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}

	// The aspect resolved two devices; the graph reaches a third.
	if len(proposal.CandidateDevices) != 2 {
		t.Fatalf("the aspect resolved %d devices, want 2 — the fixture depends on the site "+
			"meter being outside it", len(proposal.CandidateDevices))
	}

	set, found := setByOrigin(proposal.Sets, OriginGraphFlow)
	if !found {
		t.Fatalf("no flow set proposed; got %v and notes %v",
			setSummaries(proposal.Sets), proposal.Notes)
	}

	site, ok := memberFor(set, "dev-site")
	if !ok {
		t.Fatalf("the site meter is absent, so a cross-level pair cannot be related: %v",
			set.Members)
	}
	if site.FromAspect {
		t.Error("the site meter is marked as coming from the aspect, but it was reached " +
			"through the graph")
	}
	if site.Graph == nil || site.Graph.Role != RoleDownstream {
		t.Errorf("the site meter's placement is %+v, want the downstream role", site.Graph)
	}
	if site.Unit != "W" {
		t.Errorf("unit = %q, want it resolved from the ontology index like any other member",
			site.Unit)
	}
	if oven, ok := memberFor(set, "dev-oven"); !ok || oven.Graph.Role != RoleUpstream {
		t.Errorf("the oven's placement is %+v, want the upstream role", oven.Graph)
	}

	// A containment is not a co-occurrence, and the set has to say so — otherwise a
	// developer reads "sub-meter active while parent active, confidence 1.0" as a
	// finding rather than as arithmetic.
	if !strings.Contains(set.Rationale, "containment") {
		t.Errorf("rationale = %q, want it to name the relationship as containment", set.Rationale)
	}
	if !containsSubstring(set.Notes, "arithmetic") {
		t.Errorf("notes = %v, want one distinguishing the arithmetic case from the fault case",
			set.Notes)
	}
	if !containsSubstring(set.Notes, "fault") {
		t.Errorf("notes = %v, want one naming what the set is actually for", set.Notes)
	}

	// Depth is what makes "same level" and "one contains the other" readable without
	// walking the topology again: grid 0, site 1, circuit 2, devices 3.
	if site.Graph.Depth != 1 {
		t.Errorf("the site meter's depth is %d, want 1 (one edge from the sink)", site.Graph.Depth)
	}
	if oven, _ := memberFor(set, "dev-oven"); oven.Graph.Depth != 3 {
		t.Errorf("the oven's depth is %d, want 3", oven.Graph.Depth)
	}
}

// A graph read is metadata. Resolving a device outside the aspect costs a device
// read, and a device read is not a value read — the tier-L0 claim has to survive it.
func TestGraphNeighboursCostNoValueRead(t *testing.T) {
	h := newHarness(t)
	h.ontology.graphs = []models.Graph{kitchenGraph()}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if proposal.Reads.Values != 0 {
		t.Errorf("values read = %d, want 0 — a graph and a device are both metadata (§5.8)",
			proposal.Reads.Values)
	}
	// Counted, though: a proposal that quietly read twelve devices would otherwise look
	// free, and the one figure that must stay zero is Values.
	if proposal.Reads.Devices != 1 {
		t.Errorf("device reads = %d, want the 1 the graph neighbour cost", proposal.Reads.Devices)
	}
	if h.timeseries.calls != 0 {
		t.Errorf("%d timeseries queries were issued while proposing sets", h.timeseries.calls)
	}
	// Execute rather than Read on the neighbour: the point of resolving it is to offer
	// its series, and offering one the caller cannot read would fail at the query.
	for _, action := range h.devices.actions {
		if action != models.Execute {
			t.Errorf("a graph neighbour was read under %v, want Execute", action)
		}
	}
}

func TestAGraphNeighbourTheCallerCannotReadIsReportedRatherThanDropped(t *testing.T) {
	h := newHarness(t)
	h.ontology.graphs = []models.Graph{kitchenGraph()}
	h.devices.err = errors.New("access denied")

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if !containsSubstring(proposal.Notes, "permissions do not cover") {
		t.Errorf("notes = %v, want one naming the device the graph reaches and this developer "+
			"cannot read", proposal.Notes)
	}
	// The sibling set does not depend on the neighbour, so it survives.
	if _, found := setByOrigin(proposal.Sets, OriginGraphSiblings); !found {
		t.Errorf("the sibling set was lost because an unrelated neighbour was unreadable: %v",
			setSummaries(proposal.Sets))
	}
}

// Two meters downstream of one junction, both outside the aspect, so a budget of one
// has to bite — and say that it did.
func TestTheGraphNeighbourBudgetIsBoundedAndSaysWhatItDropped(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.MaxGraphNeighbours = 1 })

	graph := models.Graph{
		Id: "graph-3", Owner: "user-1",
		Attributes: []models.Attribute{{Key: "name", Value: "Two meters"}},
		Nodes: []models.Node{
			{Id: "n-oven", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-oven"},
			{Id: "n-lights", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-lights"},
			{Id: "n-circuit", Attributes: []models.Attribute{{Key: "name", Value: "Kitchen circuit"}}},
			{Id: "n-meter-a", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-meter-a"},
			{Id: "n-meter-b", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-meter-b"},
			{Id: "n-grid", Attributes: []models.Attribute{{Key: "name", Value: "Grid connection"}}},
		},
		Edges: []models.Edge{
			{Id: "e-1", FromNodeId: "n-oven", ToNodeId: "n-circuit", Weight: 100},
			{Id: "e-2", FromNodeId: "n-lights", ToNodeId: "n-circuit", Weight: 100},
			// The circuit's own output is split across two meters, which is what the
			// validator's per-node sum of 100 is actually about.
			{Id: "e-3", FromNodeId: "n-circuit", ToNodeId: "n-meter-a", Weight: 50},
			{Id: "e-4", FromNodeId: "n-circuit", ToNodeId: "n-meter-b", Weight: 50},
			{Id: "e-5", FromNodeId: "n-meter-a", ToNodeId: "n-grid", Weight: 100},
			{Id: "e-6", FromNodeId: "n-meter-b", ToNodeId: "n-grid", Weight: 100},
		},
	}
	if err := graph.Valid(); err != nil {
		t.Fatalf("the fixture is not valid upstream: %v", err)
	}
	h.ontology.graphs = []models.Graph{graph}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(h.devices.actions) != 1 {
		t.Errorf("device reads = %d, want the budget of 1", len(h.devices.actions))
	}
	if !containsSubstring(proposal.Notes, "budget") {
		t.Errorf("notes = %v, want one saying the neighbour budget was reached: a set naming "+
			"fewer devices than its graph does, silently, reads as the whole topology",
			proposal.Notes)
	}
}

// A device whose own output is split across two junctions is the case where the edge
// weight says something — one meter allocated across two business units, say. Below
// 100 it is a fact about the edge; at 100 it only says the device has one parent.
func TestASplitAllocationCarriesItsShares(t *testing.T) {
	h := newHarness(t)

	graph := models.Graph{
		Id: "graph-4", Owner: "user-1",
		Attributes: []models.Attribute{{Key: "name", Value: "Allocated across units"}},
		Nodes: []models.Node{
			{Id: "n-oven", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-oven"},
			{Id: "n-lights", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-lights"},
			{Id: "n-unit-a", Attributes: []models.Attribute{{Key: "name", Value: "Catering"}}},
			{Id: "n-unit-b", Attributes: []models.Attribute{{Key: "name", Value: "Front of house"}}},
			{Id: "n-grid", Attributes: []models.Attribute{{Key: "name", Value: "Grid connection"}}},
		},
		Edges: []models.Edge{
			// The oven's draw is attributed 60/40 across two business units.
			{Id: "e-1", FromNodeId: "n-oven", ToNodeId: "n-unit-a", Weight: 60},
			{Id: "e-2", FromNodeId: "n-oven", ToNodeId: "n-unit-b", Weight: 40},
			{Id: "e-3", FromNodeId: "n-lights", ToNodeId: "n-unit-a", Weight: 100},
			{Id: "e-4", FromNodeId: "n-unit-a", ToNodeId: "n-grid", Weight: 100},
			{Id: "e-5", FromNodeId: "n-unit-b", ToNodeId: "n-grid", Weight: 100},
		},
	}
	if err := graph.Valid(); err != nil {
		t.Fatalf("the fixture is not valid upstream: %v", err)
	}
	h.ontology.graphs = []models.Graph{graph}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}

	set, found := setByOrigin(proposal.Sets, OriginGraphSiblings)
	if !found {
		t.Fatalf("no sibling set; got %v and notes %v", setSummaries(proposal.Sets), proposal.Notes)
	}
	if set.Name != "Catering" {
		t.Errorf("name = %q, want the business unit they meet at", set.Name)
	}
	oven, ok := memberFor(set, "dev-oven")
	if !ok || oven.Graph == nil {
		t.Fatalf("the oven is absent or unplaced: %+v", set.Members)
	}
	if oven.Graph.Weight != 60 {
		t.Errorf("weight = %d, want the 60 attributed along this edge", oven.Graph.Weight)
	}
	if lights, ok := memberFor(set, "dev-lights"); !ok || lights.Graph.Weight != 100 {
		t.Errorf("the lights' weight is %+v, want 100 — their whole draw goes here", lights.Graph)
	}
}

// A node fed by one device is a pass-through, and calling that a peer group would
// assert a relationship the topology does not contain.
func TestANodeWithOneDeviceFeedingItIsNotASiblingSet(t *testing.T) {
	h := newHarness(t)
	h.ontology.graphs = []models.Graph{{
		Id: "graph-2", Owner: "user-1",
		Nodes: []models.Node{
			{Id: "n-oven", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-oven"},
			{Id: "n-circuit", Attributes: []models.Attribute{{Key: "name", Value: "Kitchen circuit"}}},
		},
		Edges: []models.Edge{{Id: "e-1", FromNodeId: "n-oven", ToNodeId: "n-circuit", Weight: 100}},
	}}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if set, found := setByOrigin(proposal.Sets, OriginGraphSiblings); found {
		t.Errorf("a single-contributor node produced a sibling set of %d devices", set.Devices)
	}
}

func TestAFailedGraphListingIsANoteRatherThanAnError(t *testing.T) {
	h := newHarness(t)
	h.ontology.graphErr = errors.New("device-repository unavailable")

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) == 0 {
		t.Error("the aspect-derived sets were lost when the graph listing failed")
	}
	if !containsSubstring(proposal.Notes, "could not be listed") {
		t.Errorf("notes = %v, want one saying the graph listing failed", proposal.Notes)
	}
}

// A graph names a device the aspect already resolved: it must not be read twice, and
// the member must keep the aspect provenance it already had.
func TestADeviceInBothTheAspectAndTheGraphIsResolvedOnce(t *testing.T) {
	h := newHarness(t)
	h.ontology.graphs = []models.Graph{kitchenGraph()}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}

	// Only the site meter is read: the oven and the lights came from the aspect.
	if len(h.devices.actions) != 1 {
		t.Errorf("device reads = %d, want 1 for the one device outside the aspect",
			len(h.devices.actions))
	}
	set, _ := setByOrigin(proposal.Sets, OriginGraphSiblings)
	if oven, ok := memberFor(set, "dev-oven"); !ok || !oven.FromAspect {
		t.Errorf("the oven's from_aspect is %v, want true: it was resolved through the aspect "+
			"and the graph only placed it", oven.FromAspect)
	}
}

// Two graphs over the same devices are two different claims, so the set ids have to
// differ — otherwise the second would look like the first already stored.
func TestSetsFromDifferentGraphsHaveDifferentIds(t *testing.T) {
	h := newHarness(t)
	second := kitchenGraph()
	second.Id = "graph-2"
	second.Attributes = []models.Attribute{{Key: "name", Value: "Alternative wiring"}}
	h.ontology.graphs = []models.Graph{kitchenGraph(), second}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}

	ids := map[string]bool{}
	siblings := 0
	for _, set := range proposal.Sets {
		if set.Origin == OriginGraphSiblings {
			siblings++
			if ids[set.SetID] {
				t.Errorf("two sibling sets share the id %s", set.SetID)
			}
			ids[set.SetID] = true
		}
	}
	if siblings != 2 {
		t.Errorf("sibling sets = %d, want one per graph", siblings)
	}
}

// The depths and the node index are the part most likely to be quietly wrong, so
// they are checked directly rather than only through a proposal.
func TestGraphDepthsAreMeasuredFromTheSink(t *testing.T) {
	index := indexGraph(kitchenGraph())

	for node, want := range map[string]int{
		"n-grid": 0, "n-site": 1, "n-circuit": 2, "n-oven": 3, "n-lights": 3,
	} {
		if got := index.depth[node]; got != want {
			t.Errorf("depth[%s] = %d, want %d", node, got, want)
		}
	}
	if index.name != "Kitchen sub-metering" {
		t.Errorf("graph name = %q, want the name attribute", index.name)
	}
	if len(index.deviceNodes) != 3 {
		t.Errorf("device nodes = %d, want 3", len(index.deviceNodes))
	}
	if got := index.deviceIDs(); strings.Join(got, ",") != "dev-lights,dev-oven,dev-site" {
		t.Errorf("device ids = %v, want them sorted", got)
	}
}

// A cycle cannot occur in a graph the platform validated, but ODE reads a stored
// document rather than validating it, and the cost of being wrong here should be a
// wrong number rather than the stack.
func TestACyclicGraphDoesNotHangTheWalk(t *testing.T) {
	index := indexGraph(models.Graph{
		Id: "graph-cycle",
		Nodes: []models.Node{
			{Id: "a", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-a"},
			{Id: "b", ResourceType: models.GraphResourceTypeDevice, ResourceId: "dev-b"},
		},
		Edges: []models.Edge{
			{Id: "e-1", FromNodeId: "a", ToNodeId: "b", Weight: 100},
			{Id: "e-2", FromNodeId: "b", ToNodeId: "a", Weight: 100},
		},
	})
	// No sink, so no depth is established. Reaching this line at all is the assertion.
	if len(index.depth) != 0 {
		t.Logf("depths from a cyclic graph: %v", index.depth)
	}
}

// A graph whose devices the aspect does not name at all should not produce a set:
// the operation is aspect-scoped, and the seed is what anchors it.
func TestAGraphTouchingNoneOfTheAspectsDevicesProposesNothing(t *testing.T) {
	h := newHarness(t)
	h.selection.result = selection.Result{
		Selectables:      []selection.Selectable{},
		CandidateDevices: []selection.CandidateDevice{},
		OntologyGaps:     []selection.OntologyGap{},
		Notes:            []string{},
	}
	h.ontology.graphs = []models.Graph{kitchenGraph()}

	proposal, err := h.service.ProposeRelatedSets(context.Background(), "token", ProposalRequest{
		AspectID: "kitchen",
	})
	if err != nil {
		t.Fatalf("ProposeRelatedSets: %v", err)
	}
	if len(proposal.Sets) != 0 {
		t.Errorf("sets = %v, want none: no device under this aspect anchors a graph",
			setSummaries(proposal.Sets))
	}
	if len(h.ontology.graphCalls) != 0 {
		t.Error("the graphs were listed with no device to filter on, which would have " +
			"enumerated the platform's whole topology")
	}
}
