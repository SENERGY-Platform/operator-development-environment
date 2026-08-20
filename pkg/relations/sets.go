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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
)

// The origins a candidate set can have, most trustworthy first.
const (
	// OriginDeviceGroup is a grouping the platform already holds. §5.5 asks for
	// these to be checked before constructing new ones: somebody has already said
	// these devices belong together, which is a stronger statement than sharing an
	// aspect.
	OriginDeviceGroup = "device_group"
	// OriginAspectNode is the devices reporting under one aspect node.
	OriginAspectNode = "aspect_node"
	// OriginAspectSubtree is the devices reporting anywhere under the requested
	// node. It is what catches the case where the oven declares "Kitchen" and the
	// lights declare "Kitchen Ceiling".
	OriginAspectSubtree = "aspect_subtree"
)

// SetMember is one addressable series a candidate set offers.
type SetMember struct {
	Ref profiler.SeriesRef `json:"ref"`
	// Label is what a rule statement will call this member. Built from the device
	// name and the variable's last path segment, because "the oven's power" reads and
	// "urn:infai:ses:device:4711|…|value.power" does not.
	Label string `json:"label"`

	DeviceName      string                 `json:"device_name"`
	DeviceTypeName  string                 `json:"device_type_name,omitempty"`
	ServiceName     string                 `json:"service_name,omitempty"`
	FunctionID      string                 `json:"function_id,omitempty"`
	AspectID        string                 `json:"aspect_id,omitempty"`
	AspectName      string                 `json:"aspect_name,omitempty"`
	ConnectionState models.ConnectionState `json:"connection_state,omitempty"`

	// CharacteristicID is canonical and never fabricated (§5.4.11).
	CharacteristicID *string             `json:"characteristic_id"`
	Unit             string              `json:"unit"`
	UnitSource       profiler.UnitSource `json:"unit_source"`

	// Graph is where this member sits in the topology that proposed it, and is nil for
	// a member the aspect hierarchy proposed. It changes how a rule about the member
	// should be read: a sibling and a sub-meter are different relationships, and the
	// weight says what share of the flow this device accounts for.
	Graph *GraphPlacement `json:"graph,omitempty"`
	// FromAspect says the member came from the requested aspect rather than being
	// reached through a graph. A graph legitimately crosses out of the aspect — a site
	// meter is not in the kitchen — so a reader comparing a set against the aspect they
	// asked about needs to know which members are which.
	FromAspect bool `json:"from_aspect"`
}

// CandidateSet is a set of series proposed for a relational pass (§5.5).
type CandidateSet struct {
	// SetID is a fingerprint of the origin and the members, so the same proposal
	// made twice carries the same id and a relation can name where its members came
	// from.
	SetID  string `json:"set_id"`
	Origin string `json:"origin"`
	Name   string `json:"name"`
	// Rationale says why these devices are together, in the terms the developer can
	// judge — "both report under the aspect Kitchen" rather than a score.
	Rationale string `json:"rationale"`

	AspectID   string `json:"aspect_id,omitempty"`
	AspectName string `json:"aspect_name,omitempty"`
	// AspectPath is root-first, so a reader can see where "Kitchen" sits without
	// fetching the tree.
	AspectPath    []string `json:"aspect_path,omitempty"`
	DeviceGroupID string   `json:"device_group_id,omitempty"`

	// The graph a set came from, and the node its members meet at. Empty for a set the
	// aspect hierarchy or a device group proposed.
	GraphID   string `json:"graph_id,omitempty"`
	GraphName string `json:"graph_name,omitempty"`
	GraphNode string `json:"graph_node,omitempty"`

	Members []SetMember `json:"members"`
	// Devices is how many distinct devices the members span. A set of one device is
	// not a multi-device pattern, which is why it is never proposed.
	Devices int `json:"devices"`
	// Truncated says the member cap applied, and Notes says what was left out. A
	// silent truncation reads as completeness.
	Truncated bool     `json:"truncated"`
	Notes     []string `json:"notes"`
}

// Proposal is the answer to one candidate-set request.
//
// §5.5 gives the signature as returning the sets alone. It returns the audit around
// them as well, for the reason every other operation in ODE does: an empty list has
// several causes — the aspect has no devices, the caller may read none of them, the
// device types carry no aspect at all — and they are different problems with
// different fixes.
type Proposal struct {
	AspectID   string `json:"aspect_id"`
	AspectName string `json:"aspect_name"`
	// IncludeDescendants is echoed because it changes the answer materially and the
	// caller may have relied on a default.
	IncludeDescendants bool `json:"include_descendants"`
	// Subtree is the aspect nodes that were considered, root-first.
	Subtree []AspectRef `json:"subtree"`

	Sets []CandidateSet `json:"sets"`
	// CandidateDevices is every readable device the aspect resolved to, whether or
	// not it made it into a set. It is what makes "only one device here" visible.
	CandidateDevices []selection.CandidateDevice `json:"candidate_devices"`
	OntologyGaps     []selection.OntologyGap     `json:"ontology_gaps"`

	// Reads is structurally value-free: this operation is tier L0 (§5.8) and reads
	// the ontology, the device list and the device groups. A non-zero Values here
	// would mean the tier claim had been broken.
	Reads Reads    `json:"reads"`
	Notes []string `json:"notes"`
}

// AspectRef is one node of the considered subtree.
type AspectRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id,omitempty"`
	// Depth is 0 for the requested node.
	Depth int `json:"depth"`
}

// ProposalRequest is one candidate-set request.
type ProposalRequest struct {
	AspectID string
	// IncludeDescendants keeps the series declared against nodes below the requested
	// one. Default false is deliberate at the API boundary rather than here: the
	// device repository expands an aspect criterion to the whole subtree upstream, so
	// the descendants arrive whether or not they were asked for, and this flag decides
	// whether they are kept. A caller that wants only the node itself would otherwise
	// silently get its children too.
	IncludeDescendants bool
	// DeviceLimit bounds how many devices the resolution expands, with the same
	// meaning as everywhere else in ODE. Zero means the service default.
	DeviceLimit int64
	// MaxMembers bounds one set. Zero means the service default.
	MaxMembers int
}

// ProposeRelatedSets turns an aspect node into candidate device sets (§5.5).
//
// **The aspect hierarchy is what solves candidate selection**, and the two existing
// grouping sources refine it. Rather than making a developer pick devices by name,
// the devices under "Kitchen" yield the oven and the lights on their own; a device
// group says which of them somebody has already grouped; and a graph says how they
// are *wired*, which is the strongest statement of the three because it carries
// direction and share as well as membership.
//
// The order the sets come back in is a claim about how much each grouping is worth
// trusting:
//
//  1. **graph siblings** — devices converging on one node. Somebody drew that
//     topology deliberately, and it says where they meet.
//  2. **graph flow** — a sub-metering pair. A containment rather than a
//     co-occurrence, which the set says in as many words.
//  3. **device group** — an asserted grouping, without the topology.
//  4. **aspect node** — a shared semantic label.
//  5. **aspect subtree** — the same, one level looser, for two devices on sibling
//     nodes.
//
// A graph legitimately reaches outside the requested aspect: a site meter is not in
// the kitchen, and dropping it would lose the very pair a sub-metering question is
// about. Those neighbours are resolved on their own, bounded by MaxGraphNeighbours.
//
// Tier L0 throughout. Selectables, a device list, a device-group list, a graph list
// and a device read per graph neighbour are all metadata, and the Reads block reports
// zero values so the property is checkable from the answer rather than taken on trust.
func (s *Service) ProposeRelatedSets(ctx context.Context, token string, req ProposalRequest) (Proposal, error) {
	if strings.TrimSpace(req.AspectID) == "" {
		return Proposal{}, fmt.Errorf("%w: an aspect id is required", ErrInvalidRequest)
	}
	maxMembers := req.MaxMembers
	if maxMembers <= 0 {
		maxMembers = s.deps.MaxMembers
	}

	snapshot, err := s.deps.Ontology.Snapshot(ctx, token)
	if err != nil {
		return Proposal{}, err
	}
	subtree, found := aspectSubtree(snapshot.AspectNodes, req.AspectID)
	if !found {
		return Proposal{}, fmt.Errorf("%w: no aspect node %q", ErrInvalidRequest, req.AspectID)
	}

	proposal := Proposal{
		AspectID:           req.AspectID,
		AspectName:         subtree[0].Name,
		IncludeDescendants: req.IncludeDescendants,
		Subtree:            subtree,
		Sets:               []CandidateSet{},
		CandidateDevices:   []selection.CandidateDevice{},
		OntologyGaps:       []selection.OntologyGap{},
		Notes:              []string{},
	}

	// One aspect criterion, no function: every measured variable under the node,
	// whatever it measures. Ranking is skipped because it costs one availability
	// call per device and a proposal is a list of what exists, not an order over it —
	// the relational pass is where reading starts.
	resolved, err := s.deps.Selection.Resolve(ctx, token, selection.Request{
		AspectIDs:   []string{req.AspectID},
		DeviceLimit: req.DeviceLimit,
		SkipRanking: true,
	})
	if err != nil {
		return Proposal{}, err
	}
	proposal.CandidateDevices = resolved.CandidateDevices
	proposal.OntologyGaps = resolved.OntologyGaps
	proposal.Notes = append(proposal.Notes, resolved.Notes...)

	allowed := map[string]bool{}
	for _, node := range subtree {
		if node.Depth == 0 || req.IncludeDescendants {
			allowed[node.ID] = true
		}
	}

	devicesByID := map[string]selection.CandidateDevice{}
	for _, device := range resolved.CandidateDevices {
		devicesByID[device.DeviceID] = device
	}

	members, dropped := setMembers(resolved, devicesByID, allowed)
	if dropped > 0 {
		proposal.Notes = append(proposal.Notes, fmt.Sprintf(
			"%d resolved variable(s) are not readable as a scalar series and were left out; "+
				"the selection route reports each with its reason", dropped))
	}
	if len(members) == 0 {
		proposal.Notes = append(proposal.Notes, aspectEmptyNote(req, resolved))
		return proposal, nil
	}

	// Existing groupings before constructed ones (§5.5), and the topology before the
	// membership. A failure in either is a note rather than an error: the
	// aspect-derived sets are still an answer, and losing them because a secondary
	// listing blipped would be the wrong trade.
	graphSets, graphReads, graphNotes := s.graphSets(ctx, token, members, maxMembers)
	proposal.Sets = append(proposal.Sets, graphSets...)
	proposal.Reads.Devices += graphReads
	proposal.Notes = append(proposal.Notes, graphNotes...)

	groups, err := s.deps.Ontology.ListDeviceGroups(ctx, token, ontology.DeviceGroupOptions{
		DeviceIDs:       deviceIDs(members),
		IgnoreGenerated: true,
	})
	if err != nil {
		proposal.Notes = append(proposal.Notes,
			"existing device groups could not be listed, so only the graph- and aspect-derived "+
				"sets are proposed: "+err.Error())
	} else {
		for _, group := range groups {
			if set, ok := groupSet(group, members, maxMembers); ok {
				proposal.Sets = append(proposal.Sets, set)
			}
		}
	}

	// One set per aspect node that has two devices of its own.
	for _, node := range subtree {
		if !allowed[node.ID] {
			continue
		}
		nodeMembers := filterByAspect(members, node.ID)
		if set, ok := buildSet(OriginAspectNode, node, subtree, nodeMembers, maxMembers,
			fmt.Sprintf("both devices report under the aspect %s", displayName(node)),
		); ok {
			proposal.Sets = append(proposal.Sets, set)
		}
	}

	// The whole subtree, when it says something the per-node sets do not.
	if req.IncludeDescendants && len(subtree) > 1 {
		if set, ok := buildSet(OriginAspectSubtree, subtree[0], subtree, members, maxMembers,
			fmt.Sprintf("the devices report under %s or a node below it, which is how two devices "+
				"on sibling nodes are still proposed together", displayName(subtree[0])),
		); ok && !duplicate(proposal.Sets, set) {
			proposal.Sets = append(proposal.Sets, set)
		}
	}

	sortSets(proposal.Sets)
	if len(proposal.Sets) == 0 {
		proposal.Notes = append(proposal.Notes, fmt.Sprintf(
			"%d readable series were found under this aspect but they span %d device(s); "+
				"a conditional pattern needs at least two devices, so nothing is proposed",
			len(members), distinctDevices(members)))
	}
	return proposal, nil
}

// setMembers joins the resolved selectables to the devices the caller may read.
//
// A selectable is a device *type* level answer (§5.2), so one selectable becomes one
// member per device of that type. Unqueryable paths are counted and dropped: a
// service input or a JSONB column has no series to relate, and offering one would
// fail later at the read.
func setMembers(
	resolved selection.Result, devicesByID map[string]selection.CandidateDevice, allowed map[string]bool,
) ([]SetMember, int) {
	byType := map[string][]selection.Selectable{}
	for _, selectable := range resolved.Selectables {
		byType[selectable.DeviceTypeID] = append(byType[selectable.DeviceTypeID], selectable)
	}

	out := []SetMember{}
	dropped := 0
	seen := map[string]bool{}
	for _, device := range resolved.CandidateDevices {
		for _, selectable := range byType[device.DeviceTypeID] {
			if !selectable.Queryable {
				dropped++
				continue
			}
			if selectable.AspectID != "" && !allowed[selectable.AspectID] {
				continue
			}
			ref := profiler.SeriesRef{
				DeviceID:     device.DeviceID,
				ServiceID:    selectable.ServiceID,
				VariablePath: selectable.Path,
			}
			if seen[ref.String()] {
				continue
			}
			seen[ref.String()] = true
			out = append(out, SetMember{
				Ref:              ref,
				FromAspect:       true,
				Label:            memberLabel(device.Name, selectable.Path),
				DeviceName:       device.Name,
				DeviceTypeName:   device.DeviceTypeName,
				ServiceName:      selectable.ServiceName,
				FunctionID:       selectable.FunctionID,
				AspectID:         selectable.AspectID,
				AspectName:       selectable.AspectName,
				ConnectionState:  device.ConnectionState,
				CharacteristicID: selectable.CharacteristicID,
				Unit:             selectable.Unit,
				UnitSource:       selectable.UnitSource,
			})
		}
	}
	sortMembers(out)
	return out, dropped
}

// buildSet assembles one set, or reports that there is no set to make.
func buildSet(
	origin string, node AspectRef, subtree []AspectRef, members []SetMember, maxMembers int, rationale string,
) (CandidateSet, bool) {
	chosen, truncated, notes := pickMembers(members, maxMembers)
	if distinctDevices(chosen) < 2 {
		return CandidateSet{}, false
	}
	set := CandidateSet{
		Origin:     origin,
		Name:       displayName(node),
		Rationale:  rationale,
		AspectID:   node.ID,
		AspectName: node.Name,
		AspectPath: aspectPath(subtree, node.ID),
		Members:    chosen,
		Devices:    distinctDevices(chosen),
		Truncated:  truncated,
		Notes:      notes,
	}
	set.SetID = setFingerprint(set)
	return set, true
}

// groupSet builds a set from a platform device group.
//
// The group's own device list is intersected with what the aspect resolved and the
// caller may read, rather than being taken whole: a group may name devices in
// another room, or devices this developer has no permission for, and a set carrying
// either would fail at the first read.
func groupSet(group models.DeviceGroup, members []SetMember, maxMembers int) (CandidateSet, bool) {
	inGroup := map[string]bool{}
	for _, id := range group.DeviceIds {
		inGroup[id] = true
	}
	matched := []SetMember{}
	for _, member := range members {
		if inGroup[member.Ref.DeviceID] {
			matched = append(matched, member)
		}
	}
	chosen, truncated, notes := pickMembers(matched, maxMembers)
	if distinctDevices(chosen) < 2 {
		return CandidateSet{}, false
	}
	if len(group.DeviceIds) > distinctDevices(chosen) {
		notes = append(notes, fmt.Sprintf(
			"the group names %d devices; %d of them are readable by this developer and report under "+
				"the requested aspect", len(group.DeviceIds), distinctDevices(chosen)))
	}
	set := CandidateSet{
		Origin:        OriginDeviceGroup,
		Name:          group.Name,
		Rationale:     "an existing device group on the platform, which is a stronger grouping than a shared aspect",
		DeviceGroupID: group.Id,
		Members:       chosen,
		Devices:       distinctDevices(chosen),
		Truncated:     truncated,
		Notes:         notes,
	}
	set.SetID = setFingerprint(set)
	return set, true
}

// disambiguateLabels makes every label in a set distinct.
//
// memberLabel builds a label from the device name and the variable's last path
// segment, which collides constantly on real device types: "value" is the commonest
// leaf name on the platform, so one device with three services yields three members
// all called "Licht EG value". That is unreadable in a picker and worse in a rule —
// "Licht EG value active → Licht EG value active" says nothing about which of the
// three, and a rule nobody can read is a rule nobody can confirm (§5.10).
//
// The suffix is whatever actually separates them, preferring what reads best: the
// service name, then the device, then the variable path, and the whole reference as
// the guaranteed-unique last resort. Members whose label was already unique are left
// alone — suffixing every member would cost the common case to fix the uncommon one.
func disambiguateLabels(members []SetMember) []SetMember {
	byLabel := map[string][]int{}
	for i, member := range members {
		byLabel[member.Label] = append(byLabel[member.Label], i)
	}

	out := append([]SetMember{}, members...)
	for _, indices := range byLabel {
		if len(indices) < 2 {
			continue
		}
		options := make([][]string, 0, len(indices))
		for _, i := range indices {
			options = append(options, []string{
				out[i].ServiceName,
				out[i].DeviceName,
				out[i].Ref.VariablePath,
				out[i].Ref.String(),
			})
		}
		for position, suffix := range labelSuffixes(options) {
			out[indices[position]].Label += " (" + suffix + ")"
		}
	}
	return out
}

// pickMembers applies the member cap, one series per device first.
//
// Breadth before depth is the right bias for this operation: the oven-and-lights
// case is one series each, and a cap spent on eight channels of the same meter
// would propose a set with nothing to relate. Extra series of a device are added
// only once every device has one.
func pickMembers(members []SetMember, maxMembers int) (chosen []SetMember, truncated bool, notes []string) {
	notes = []string{}
	if maxMembers <= 0 || len(members) <= maxMembers {
		return disambiguateLabels(members), false, notes
	}

	taken := map[string]bool{}
	chosen = []SetMember{}
	for _, member := range members {
		if len(chosen) == maxMembers {
			break
		}
		if taken[member.Ref.DeviceID] {
			continue
		}
		taken[member.Ref.DeviceID] = true
		chosen = append(chosen, member)
	}
	for _, member := range members {
		if len(chosen) == maxMembers {
			break
		}
		if containsRef(chosen, member.Ref) {
			continue
		}
		chosen = append(chosen, member)
	}

	notes = append(notes, fmt.Sprintf(
		"%d of %d readable series are offered, one per device before a second series of any device",
		len(chosen), len(members)))
	// After the cap, so a suffix is only added where the *offered* labels collide.
	return disambiguateLabels(chosen), true, notes
}

// aspectSubtree returns the requested node and its descendants, root-first.
//
// Derived from ParentId rather than ChildIds, and cycle-guarded, for the reasons
// AspectTree gives: a node whose parent is missing still surfaces, and a cycle in
// the links costs a bounded walk rather than the stack.
func aspectSubtree(nodes []models.AspectNode, rootID string) ([]AspectRef, bool) {
	byID := map[string]models.AspectNode{}
	children := map[string][]string{}
	for _, node := range nodes {
		byID[node.Id] = node
	}
	for _, node := range nodes {
		if node.ParentId == "" || node.ParentId == node.Id {
			continue
		}
		children[node.ParentId] = append(children[node.ParentId], node.Id)
	}
	root, found := byID[rootID]
	if !found {
		return nil, false
	}

	out := []AspectRef{{ID: root.Id, Name: root.Name, ParentID: root.ParentId, Depth: 0}}
	visited := map[string]bool{root.Id: true}
	frontier := []string{root.Id}
	for depth := 1; len(frontier) > 0; depth++ {
		next := []string{}
		ids := []string{}
		for _, parent := range frontier {
			ids = append(ids, children[parent]...)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if visited[id] {
				continue
			}
			visited[id] = true
			node := byID[id]
			out = append(out, AspectRef{ID: node.Id, Name: node.Name, ParentID: node.ParentId, Depth: depth})
			next = append(next, id)
		}
		frontier = next
	}
	return out, true
}

// aspectPath is the chain from the subtree's root to one node, by name.
func aspectPath(subtree []AspectRef, nodeID string) []string {
	byID := map[string]AspectRef{}
	for _, node := range subtree {
		byID[node.ID] = node
	}
	path := []string{}
	for id := nodeID; id != ""; {
		node, found := byID[id]
		if !found {
			break
		}
		path = append([]string{displayName(node)}, path...)
		if node.Depth == 0 {
			break
		}
		id = node.ParentID
	}
	return path
}

func filterByAspect(members []SetMember, aspectID string) []SetMember {
	out := []SetMember{}
	for _, member := range members {
		if member.AspectID == aspectID {
			out = append(out, member)
		}
	}
	return out
}

// aspectEmptyNote names the reason nothing was found, rather than leaving an empty
// list to be read as "no such pattern exists".
func aspectEmptyNote(req ProposalRequest, resolved selection.Result) string {
	switch {
	case len(resolved.Selectables) == 0:
		return fmt.Sprintf("no device type on the platform declares a variable under aspect %s, "+
			"so there is nothing to group: this is an ontology gap rather than an absence of devices",
			req.AspectID)
	case len(resolved.CandidateDevices) == 0:
		return "device types under this aspect exist, but this developer may read the data of none of " +
			"them; permission is the platform's decision and not ODE's"
	case !req.IncludeDescendants:
		return "every readable series under this aspect is declared against a node below the one " +
			"requested; ask again with include_descendants to see them"
	default:
		return "the readable series under this aspect are not addressable as scalar series; the " +
			"selection route reports each with its reason"
	}
}

func memberLabel(deviceName, path string) string {
	segment := path
	if index := strings.LastIndex(path, "."); index >= 0 && index+1 < len(path) {
		segment = path[index+1:]
	}
	segment = strings.ReplaceAll(segment, "_", " ")
	if deviceName == "" {
		return segment
	}
	return deviceName + " " + segment
}

func displayName(node AspectRef) string {
	if node.Name != "" {
		return node.Name
	}
	return node.ID
}

func distinctDevices(members []SetMember) int {
	seen := map[string]bool{}
	for _, member := range members {
		seen[member.Ref.DeviceID] = true
	}
	return len(seen)
}

func deviceIDs(members []SetMember) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, member := range members {
		if seen[member.Ref.DeviceID] {
			continue
		}
		seen[member.Ref.DeviceID] = true
		out = append(out, member.Ref.DeviceID)
	}
	return out
}

func containsRef(members []SetMember, ref profiler.SeriesRef) bool {
	for _, member := range members {
		if member.Ref == ref {
			return true
		}
	}
	return false
}

// duplicate reports whether a set with the same members already exists, which is
// how the subtree set is suppressed when one aspect node already covers everything.
func duplicate(sets []CandidateSet, candidate CandidateSet) bool {
	for _, set := range sets {
		if set.SetID == candidate.SetID {
			return true
		}
		if len(set.Members) != len(candidate.Members) {
			continue
		}
		same := true
		for i := range set.Members {
			if set.Members[i].Ref != candidate.Members[i].Ref {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// setFingerprint identifies a set by its origin and its members, so the same
// proposal made twice carries the same id and a stored relation can name the set it
// came from.
func setFingerprint(set CandidateSet) string {
	refs := make([]string, 0, len(set.Members))
	for _, member := range set.Members {
		refs = append(refs, member.Ref.String())
	}
	sort.Strings(refs)
	sum := sha256.Sum256([]byte(strings.Join(append(
		[]string{set.Origin, set.AspectID, set.DeviceGroupID, set.GraphID, set.GraphNode},
		refs...), "\x00")))
	return "set-" + hex.EncodeToString(sum[:])[:20]
}

// sortSets puts the most trustworthy origin first, then the widest set.
//
// The ranking is the argument of §5.5 made concrete: a graph edge is the most
// specific statement available because it carries direction and share as well as
// membership; a device group asserts membership alone; an aspect asserts only a
// shared label. Siblings rank above a flow pair because a peer group is what the
// pairwise detector is actually for — a containment is proposed for the fault case,
// not for the co-occurrence.
func sortSets(sets []CandidateSet) {
	rank := map[string]int{
		OriginGraphSiblings: 0,
		OriginGraphFlow:     1,
		OriginDeviceGroup:   2,
		OriginAspectNode:    3,
		OriginAspectSubtree: 4,
	}
	sort.SliceStable(sets, func(i, j int) bool {
		if rank[sets[i].Origin] != rank[sets[j].Origin] {
			return rank[sets[i].Origin] < rank[sets[j].Origin]
		}
		if sets[i].Devices != sets[j].Devices {
			return sets[i].Devices > sets[j].Devices
		}
		return sets[i].SetID < sets[j].SetID
	})
}

// sortMembers keeps a proposal reproducible: device name, then path.
func sortMembers(members []SetMember) {
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].DeviceName != members[j].DeviceName {
			return members[i].DeviceName < members[j].DeviceName
		}
		if members[i].Ref.DeviceID != members[j].Ref.DeviceID {
			return members[i].Ref.DeviceID < members[j].Ref.DeviceID
		}
		return members[i].Ref.String() < members[j].Ref.String()
	})
}
