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

package ontology

import (
	"sort"

	"github.com/SENERGY-Platform/models/go/models"
)

// TreeNode is an AspectNode with its children resolved. The device repository
// returns aspect nodes as a flat list carrying parent and child ids; the SPA
// needs the hierarchy (SPEC §5.1, "Hierarchical location/subsystem").
type TreeNode struct {
	Id       string     `json:"id"`
	Name     string     `json:"name"`
	RootId   string     `json:"root_id"`
	ParentId string     `json:"parent_id"`
	Children []TreeNode `json:"children"`
}

func (n TreeNode) sortKey() string {
	if n.Name != "" {
		return n.Name
	}
	return n.Id
}

// AspectTree builds the aspect forest from a snapshot's flat node list,
// sorted by name so the SPA renders a stable order.
//
// The hierarchy is derived from ParentId rather than ChildIds so that a node
// whose parent is absent from the list still surfaces, as a root. Dropping it
// instead would remove a whole subsystem from semantic selection (§5.2) with
// no error raised anywhere.
func AspectTree(nodes []models.AspectNode) []TreeNode {
	byId := make(map[string]*TreeNode, len(nodes))
	for _, n := range nodes {
		byId[n.Id] = &TreeNode{
			Id:       n.Id,
			Name:     n.Name,
			RootId:   n.RootId,
			ParentId: n.ParentId,
		}
	}

	childIds := make(map[string][]string, len(nodes))
	rootIds := make([]string, 0, len(nodes))
	for _, n := range nodes {
		_, parentPresent := byId[n.ParentId]
		if n.ParentId == "" || n.ParentId == n.Id || !parentPresent {
			rootIds = append(rootIds, n.Id)
			continue
		}
		childIds[n.ParentId] = append(childIds[n.ParentId], n.Id)
	}

	out := make([]TreeNode, 0, len(rootIds))
	for _, id := range rootIds {
		out = append(out, materialise(byId[id], byId, childIds, map[string]bool{}))
	}
	sortNodes(out)
	return out
}

// materialise resolves a node's children recursively. visited guards against a
// cycle in the parent links: without it the recursion would run until the
// stack is exhausted. A cycle costs the repeated subtree, not the process.
func materialise(node *TreeNode, byId map[string]*TreeNode, childIds map[string][]string, visited map[string]bool) TreeNode {
	out := *node
	out.Children = nil
	if visited[node.Id] {
		return out
	}
	visited[node.Id] = true
	defer delete(visited, node.Id)

	ids := childIds[node.Id]
	if len(ids) == 0 {
		return out
	}
	children := make([]TreeNode, 0, len(ids))
	for _, id := range ids {
		if child, ok := byId[id]; ok {
			children = append(children, materialise(child, byId, childIds, visited))
		}
	}
	sortNodes(children)
	out.Children = children
	return out
}

func sortNodes(nodes []TreeNode) {
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i].sortKey(), nodes[j].sortKey()
		if a == b {
			return nodes[i].Id < nodes[j].Id
		}
		return a < b
	})
}
