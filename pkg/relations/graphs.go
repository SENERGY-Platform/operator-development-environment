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
	"fmt"
	"sort"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// Candidate sets derived from a device relationship graph (§5.5, §5.1's Graph row).
//
// A graph is not a bag of devices. models.Graph.Valid pins it down: directed,
// acyclic, outgoing edge weights summing to 100 per node, and exactly one node with
// no outputs which may not itself be a device. That is a flow topology — a
// sub-metering or aggregation tree — and the intermediate nodes are whatever the
// operator structures by: a busbar, a location, a business unit.
//
// So a graph answers two different questions, and conflating them would produce
// confident nonsense in one of the two cases.
const (
	// OriginGraphSiblings is devices whose flow converges on the same immediate
	// downstream node. They are peers behind one aggregation point — metered
	// together, on the same circuit, in the same business unit — and this is the
	// strongest candidate set ODE can offer: somebody drew that topology on purpose,
	// which is more than a shared aspect asserts and more specific than a device
	// group, because it also says *where* they meet.
	OriginGraphSiblings = "graph_siblings"
	// OriginGraphFlow is an upstream/downstream pair: a device that measures a node
	// other devices feed. This is **containment, not co-occurrence**, and the
	// distinction matters — a sub-meter is active while its parent is active as a
	// matter of arithmetic, not as a finding, and the association filter rejects it
	// on lift because the parent is almost always active.
	//
	// It is still worth proposing, for the case that is not arithmetic: a sub-meter
	// drawing while its parent reads idle is a metering or wiring fault, and that is
	// exactly the anomaly a candidate rule in this direction defines.
	OriginGraphFlow = "graph_flow"
)

// The role a member plays in the graph the set came from.
const (
	RoleSibling    = "sibling"
	RoleUpstream   = "upstream"
	RoleDownstream = "downstream"
)

// GraphPlacement is where a member sits in the graph that proposed it.
//
// Carried onto the member because it changes how a rule about it should be read, and
// Via names the node the members meet at, which is what makes the set's rationale
// legible.
type GraphPlacement struct {
	GraphID   string `json:"graph_id"`
	GraphName string `json:"graph_name,omitempty"`
	// Role is sibling, upstream or downstream.
	Role string `json:"role"`
	// Via is the node this member's flow reaches, and ViaName its label. For a
	// downstream member of a flow set it is the node they all feed, which that member
	// measures.
	Via     string `json:"via,omitempty"`
	ViaName string `json:"via_name,omitempty"`
	// Weight is the edge weight out of this device toward Via.
	//
	// It is an *output split*, not a contribution share, and the difference matters
	// when reading one: models.Graph.Valid requires the outgoing weights of each node
	// to sum to 100, so a device feeding exactly one node always carries 100 and says
	// nothing. Below that means the device's flow is allocated across several
	// downstream nodes — one meter split across two business units, say — and only then
	// is the figure a fact about this edge rather than about the node having one.
	//
	// Zero where the member has no outgoing edge at all: the sink end of a flow set.
	Weight int `json:"weight,omitempty"`
	// Depth is how many edges separate this member from the graph's sink. It is what
	// makes "these two are at the same level" and "one contains the other" visible
	// without walking the topology again.
	Depth int `json:"depth"`
}

// graphIndex is one graph in the shape the set derivation needs.
type graphIndex struct {
	graph models.Graph
	name  string
	// byNode and deviceNodes index the nodes; outgoing and incoming the edges.
	byNode      map[string]models.Node
	deviceNodes map[string]models.Node // device id → node
	outgoing    map[string][]models.Edge
	incoming    map[string][]models.Edge
	depth       map[string]int
}

func indexGraph(graph models.Graph) graphIndex {
	index := graphIndex{
		graph:       graph,
		name:        ontology.GraphName(graph),
		byNode:      map[string]models.Node{},
		deviceNodes: map[string]models.Node{},
		outgoing:    map[string][]models.Edge{},
		incoming:    map[string][]models.Edge{},
		depth:       map[string]int{},
	}
	for _, node := range graph.Nodes {
		index.byNode[node.Id] = node
		if node.ResourceType == models.GraphResourceTypeDevice && node.ResourceId != "" {
			index.deviceNodes[node.ResourceId] = node
		}
	}
	for _, edge := range graph.Edges {
		index.outgoing[edge.FromNodeId] = append(index.outgoing[edge.FromNodeId], edge)
		index.incoming[edge.ToNodeId] = append(index.incoming[edge.ToNodeId], edge)
	}
	index.computeDepths()
	return index
}

// computeDepths measures every node's distance from the sink.
//
// The graph is guaranteed acyclic and single-sinked by models.Graph.Valid, so this
// is a walk from the sink backwards. The guard is kept anyway: this reads a document
// the platform stored, ODE did not validate it, and a cycle here would otherwise
// cost the stack rather than a wrong number.
func (g *graphIndex) computeDepths() {
	sinks := []string{}
	for _, node := range g.graph.Nodes {
		if len(g.outgoing[node.Id]) == 0 {
			sinks = append(sinks, node.Id)
		}
	}
	sort.Strings(sinks)

	frontier := sinks
	for _, id := range sinks {
		g.depth[id] = 0
	}
	for depth := 1; len(frontier) > 0 && depth <= len(g.graph.Nodes); depth++ {
		next := []string{}
		for _, id := range frontier {
			for _, edge := range g.incoming[id] {
				if _, seen := g.depth[edge.FromNodeId]; seen {
					continue
				}
				g.depth[edge.FromNodeId] = depth
				next = append(next, edge.FromNodeId)
			}
		}
		sort.Strings(next)
		frontier = next
	}
}

// deviceIDs is every device the graph names, in a stable order.
func (g graphIndex) deviceIDs() []string {
	out := make([]string, 0, len(g.deviceNodes))
	for id := range g.deviceNodes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// graphSets turns the caller's graphs into candidate sets (§5.5).
//
// The devices a graph names are frequently *not* all under the requested aspect,
// and that is the point rather than an inconvenience: a site meter is not "in the
// kitchen", so intersecting a graph with the aspect's own devices — the way a device
// group is intersected — would drop exactly the cross-level pair a sub-metering
// question is about. Neighbours outside the aspect are therefore resolved on their
// own, which costs one device read each and still reads no value.
func (s *Service) graphSets(
	ctx context.Context, token string, seed []SetMember, maxMembers int,
) (sets []CandidateSet, deviceReads int, notes []string) {
	sets, notes = []CandidateSet{}, []string{}
	if len(seed) == 0 {
		return sets, 0, notes
	}

	graphs, err := s.deps.Ontology.ListGraphs(ctx, token, ontology.GraphOptions{
		DeviceIDs: deviceIDs(seed),
	})
	if err != nil {
		// A note rather than an error, for the reason the device-group listing gives:
		// the aspect-derived sets are still the answer, and losing them because a
		// secondary listing blipped would be the wrong trade.
		return sets, 0, append(notes, "the device relationship graphs could not be listed, so no "+
			"graph-derived set is proposed: "+err.Error())
	}
	if len(graphs) == 0 {
		return sets, 0, append(notes, fmt.Sprintf(
			"no device relationship graph names any of these %d devices, so the sets below come "+
				"from the aspect hierarchy alone", len(deviceIDs(seed))))
	}

	// The resolver caches per device across graphs: two graphs commonly share a site
	// meter, and resolving it twice would cost a device read for nothing.
	resolver := &neighbourResolver{
		service: s,
		known:   map[string][]SetMember{},
		budget:  s.deps.MaxGraphNeighbours,
	}
	for _, member := range seed {
		resolver.known[member.Ref.DeviceID] = append(resolver.known[member.Ref.DeviceID], member)
	}

	for _, graph := range graphs {
		index := indexGraph(graph)
		siblings, siblingNotes := s.siblingSets(ctx, token, index, resolver, maxMembers)
		flows, flowNotes := s.flowSets(ctx, token, index, resolver, maxMembers)
		sets = append(sets, siblings...)
		sets = append(sets, flows...)
		notes = append(notes, siblingNotes...)
		notes = append(notes, flowNotes...)
	}
	if resolver.dropped > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d graph neighbour(s) outside this aspect were not resolved: the per-request budget of "+
				"%d was reached, so a set may name fewer devices than its graph does",
			resolver.dropped, resolver.budget))
	}
	return sets, resolver.reads, notes
}

// siblingSets proposes the devices that meet at one node.
//
// Only nodes with two or more device children produce a set. A node fed by one
// device and one structural branch is a pass-through, and calling that a peer group
// would be asserting a relationship the topology does not contain.
func (s *Service) siblingSets(
	ctx context.Context, token string, index graphIndex,
	resolver *neighbourResolver, maxMembers int,
) ([]CandidateSet, []string) {
	sets, notes := []CandidateSet{}, []string{}

	junctions := make([]string, 0, len(index.byNode))
	for id := range index.byNode {
		junctions = append(junctions, id)
	}
	sort.Strings(junctions)

	for _, junction := range junctions {
		type contributor struct {
			deviceID string
			weight   int
		}
		contributors := []contributor{}
		for _, edge := range index.incoming[junction] {
			node, found := index.byNode[edge.FromNodeId]
			if !found || node.ResourceType != models.GraphResourceTypeDevice || node.ResourceId == "" {
				continue
			}
			contributors = append(contributors, contributor{deviceID: node.ResourceId, weight: edge.Weight})
		}
		if len(contributors) < 2 {
			continue
		}
		sort.SliceStable(contributors, func(i, j int) bool { return contributors[i].deviceID < contributors[j].deviceID })

		via := index.byNode[junction]
		viaName := ontology.GraphNodeName(via)

		members := []SetMember{}
		for _, c := range contributors {
			resolved, note := resolver.members(ctx, token, c.deviceID)
			if note != "" {
				notes = append(notes, note)
			}
			for _, member := range resolved {
				member.Graph = &GraphPlacement{
					GraphID: index.graph.Id, GraphName: index.name,
					Role: RoleSibling, Via: junction, ViaName: viaName,
					Weight: c.weight, Depth: index.depth[index.deviceNodes[c.deviceID].Id],
				}
				members = append(members, member)
			}
		}
		sortMembers(members)

		chosen, truncated, setNotes := pickMembers(members, maxMembers)
		if distinctDevices(chosen) < 2 {
			continue
		}
		set := CandidateSet{
			Origin: OriginGraphSiblings,
			Name:   viaName,
			Rationale: fmt.Sprintf(
				"all %d devices feed %s in the graph %s, so they are metered together rather than "+
					"merely sharing a label — the topology says where they meet",
				distinctDevices(chosen), viaName, index.name),
			GraphID:   index.graph.Id,
			GraphName: index.name,
			GraphNode: junction,
			Members:   chosen,
			Devices:   distinctDevices(chosen),
			Truncated: truncated,
			Notes:     setNotes,
		}
		set.SetID = setFingerprint(set)
		sets = append(sets, set)
	}
	return sets, notes
}

// flowSets proposes an upstream/downstream pair where a device measures a node that
// other devices feed.
//
// This is the sub-metering shape, and the rationale says plainly what it is. A
// containment is not a co-occurrence: the parent is active whenever any child is, as
// arithmetic rather than as a finding, and the lift filter rejects that rule for
// exactly the right reason. What survives is the case worth a developer's attention —
// a child drawing while the parent reads idle, which is a metering or wiring fault
// and not a habit of the household.
func (s *Service) flowSets(
	ctx context.Context, token string, index graphIndex,
	resolver *neighbourResolver, maxMembers int,
) ([]CandidateSet, []string) {
	sets, notes := []CandidateSet{}, []string{}

	// A "meter of a node" is a device node immediately downstream of it. Walking
	// device→node→device rather than device→device because the platform's topology puts
	// a structural node between the two in every case the validator allows.
	measured := map[string][]string{} // junction → measuring device ids
	junctions := make([]string, 0, len(index.byNode))
	for id := range index.byNode {
		junctions = append(junctions, id)
	}
	sort.Strings(junctions)

	for _, junction := range junctions {
		for _, edge := range index.outgoing[junction] {
			node, found := index.byNode[edge.ToNodeId]
			if found && node.ResourceType == models.GraphResourceTypeDevice && node.ResourceId != "" {
				measured[junction] = append(measured[junction], node.ResourceId)
			}
		}
	}

	for _, junction := range junctions {
		parents := measured[junction]
		if len(parents) == 0 {
			continue
		}
		children := []struct {
			deviceID string
			weight   int
		}{}
		for _, edge := range index.incoming[junction] {
			node, found := index.byNode[edge.FromNodeId]
			if !found || node.ResourceType != models.GraphResourceTypeDevice || node.ResourceId == "" {
				continue
			}
			children = append(children, struct {
				deviceID string
				weight   int
			}{node.ResourceId, edge.Weight})
		}
		if len(children) == 0 {
			continue
		}
		sort.SliceStable(children, func(i, j int) bool { return children[i].deviceID < children[j].deviceID })
		sort.Strings(parents)

		via := index.byNode[junction]
		viaName := ontology.GraphNodeName(via)

		members := []SetMember{}
		// The measuring device first: it is the antecedent a developer reads the set
		// against, and a member order that buried it would make the rules harder to read.
		for _, parent := range parents {
			resolved, note := resolver.members(ctx, token, parent)
			if note != "" {
				notes = append(notes, note)
			}
			for _, member := range resolved {
				member.Graph = &GraphPlacement{
					GraphID: index.graph.Id, GraphName: index.name,
					Role: RoleDownstream, Via: junction, ViaName: viaName,
					Depth: index.depth[index.deviceNodes[parent].Id],
				}
				members = append(members, member)
			}
		}
		for _, child := range children {
			resolved, note := resolver.members(ctx, token, child.deviceID)
			if note != "" {
				notes = append(notes, note)
			}
			for _, member := range resolved {
				member.Graph = &GraphPlacement{
					GraphID: index.graph.Id, GraphName: index.name,
					Role: RoleUpstream, Via: junction, ViaName: viaName,
					Weight: child.weight, Depth: index.depth[index.deviceNodes[child.deviceID].Id],
				}
				members = append(members, member)
			}
		}

		chosen, truncated, setNotes := pickMembers(members, maxMembers)
		if distinctDevices(chosen) < 2 || !hasBothRoles(chosen) {
			continue
		}
		setNotes = append(setNotes,
			"a sub-meter is active whenever what it feeds is active, which is arithmetic rather "+
				"than a finding — the lift filter rejects that rule. What this set is for is the "+
				"reverse: a device drawing while what it feeds reads idle is a metering or wiring "+
				"fault, and that is the anomaly a rule here defines.")

		set := CandidateSet{
			Origin: OriginGraphFlow,
			Name:   viaName + " (flow)",
			Rationale: fmt.Sprintf(
				"the graph %s has %d device(s) feeding %s and %d measuring it, so this is "+
					"containment rather than a shared location: one side is a sub-meter of the other",
				index.name, len(children), viaName, len(parents)),
			GraphID:   index.graph.Id,
			GraphName: index.name,
			GraphNode: junction,
			Members:   chosen,
			Devices:   distinctDevices(chosen),
			Truncated: truncated,
			Notes:     setNotes,
		}
		set.SetID = setFingerprint(set)
		sets = append(sets, set)
	}
	return sets, notes
}

// hasBothRoles guards against a flow set the member cap has reduced to one side of
// the containment, which would be a peer group mislabelled as a sub-metering pair.
func hasBothRoles(members []SetMember) bool {
	upstream, downstream := false, false
	for _, member := range members {
		if member.Graph == nil {
			continue
		}
		switch member.Graph.Role {
		case RoleUpstream:
			upstream = true
		case RoleDownstream:
			downstream = true
		}
	}
	return upstream && downstream
}

// neighbourResolver turns a device id into set members, reading the device when it
// was not part of the aspect resolution.
//
// This is what makes a cross-level graph set possible at all. It stays at tier L0:
// a device read is metadata, the variables come from the device type, and the units
// from the ontology index — no value is read, and the Reads block still says zero.
type neighbourResolver struct {
	service *Service
	known   map[string][]SetMember
	budget  int
	spent   int
	dropped int
	// reads is the device reads made, reported so a proposal's metadata cost is
	// visible rather than free-looking.
	reads int
}

func (r *neighbourResolver) members(ctx context.Context, token, deviceID string) ([]SetMember, string) {
	if members, found := r.known[deviceID]; found {
		return members, ""
	}
	if r.budget > 0 && r.spent >= r.budget {
		r.dropped++
		r.known[deviceID] = nil
		return nil, ""
	}
	r.spent++

	// Execute rather than Read, and deliberately: the whole point of resolving this
	// device is to offer its series for a relational pass, and offering one the caller
	// cannot read would fail later at the query instead of here (§5.1).
	device, err := r.service.deps.Devices.Get(token, deviceID, models.Execute)
	r.reads++
	if err != nil {
		// A neighbour this developer may not read is the ordinary case, not a fault: a
		// graph is a topology, and permission is per device.
		r.known[deviceID] = nil
		return nil, fmt.Sprintf(
			"a graph names device %s, which this developer's permissions do not cover, so it is "+
				"absent from the sets below: %s", deviceID, err.Error())
	}
	if device.DeviceType == nil {
		r.known[deviceID] = nil
		return nil, fmt.Sprintf("device %s came back without its device type, so its series "+
			"could not be enumerated", deviceID)
	}

	index, err := r.service.deps.OntologyIndex.Ontology(ctx, token)
	if err != nil {
		r.known[deviceID] = nil
		return nil, "the ontology index could not be read, so a graph neighbour's units are unknown: " +
			err.Error()
	}

	members := []SetMember{}
	for _, variable := range profiler.DeviceTypeVariables(*device.DeviceType) {
		if !variable.Queryable || !variable.Streamed() || !variable.Numeric() {
			continue
		}
		semantics := profiler.ResolveUnits(variable, index, profiler.Provenance{})
		ref := profiler.SeriesRef{
			DeviceID: deviceID, ServiceID: variable.ServiceID, VariablePath: variable.Path,
		}
		members = append(members, SetMember{
			Ref:              ref,
			Label:            memberLabel(displayDeviceName(device), variable.Path),
			DeviceName:       displayDeviceName(device),
			DeviceTypeName:   device.DeviceType.Name,
			ServiceName:      variable.ServiceName,
			FunctionID:       variable.FunctionID,
			AspectID:         variable.AspectID,
			ConnectionState:  device.ConnectionState,
			CharacteristicID: semantics.CharacteristicID,
			Unit:             semantics.Unit,
			UnitSource:       semantics.UnitSource,
			// FromAspect is false: this device was reached through the graph rather than
			// through the aspect, and a reader comparing a set against the aspect they asked
			// about needs to know which members came from where.
		})
	}
	if len(members) == 0 {
		r.known[deviceID] = nil
		return nil, fmt.Sprintf("device %s has no readable scalar series, so it takes part in no "+
			"graph-derived set", deviceID)
	}
	r.known[deviceID] = members
	return members, ""
}
