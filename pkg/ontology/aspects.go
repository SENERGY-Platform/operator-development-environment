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

// This file is the one reading point for an aspect list that still carries its
// deprecated singular alias (SNRGY-4648). ContentVariable,
// FilterCriteria, ImportTypeFilterCriteria and PathOption each moved from one
// aspect id to a list, keeping the old field as an alias that holds only the
// alphabetically first entry. Reading `cv.AspectId` anywhere downstream would
// silently lose every aspect past the first, and reading `cv.AspectIds` alone
// would silently drop every document the platform has not migrated yet. Every
// call site in this package reads through AspectIDs (or, for a path option's
// richer AspectNode, AspectRefsFromNodes) instead of either field directly.

// AspectRef names one aspect node without its hierarchy or classification — the
// shape used wherever a variable's aspect needs a label rather than a tree
// position: a device-type-level selectable, an import selectable, a relation
// member.
type AspectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AspectIDs resolves an aspect-list field: list wins when it is not empty, alias
// is the fallback used as a single-element list, and both empty is no aspects at
// all. The result is sorted and deduplicated, which is what makes a set
// comparison (sameQuantity, findCounterpart) independent of the order either
// side was built in.
func AspectIDs(list []string, alias string) []string {
	ids := list
	if len(ids) == 0 && alias != "" {
		ids = []string{alias}
	}
	return sortedUniqueStrings(ids)
}

// AspectNodeIDs extracts the ids from a path option's plural aspect nodes, for
// passing to AspectIDs alongside the option's singular alias.
func AspectNodeIDs(nodes []models.AspectNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Id)
	}
	return out
}

func sortedUniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, id := range in {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// EqualAspectSets reports whether two aspect id lists are the same set. Both
// sides are expected to already be sorted and deduplicated (see AspectIDs),
// which is what lets this be a positional comparison rather than a second sort —
// callers that build a list any other way must not use this without running it
// through AspectIDs first.
func EqualAspectSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AspectNamesByID indexes a snapshot's aspect nodes by id, once, so naming a
// list of ids is a map lookup rather than a rescan of the snapshot per id.
func AspectNamesByID(nodes []models.AspectNode) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		out[n.Id] = n.Name
	}
	return out
}

// AspectRefsByID resolves a sorted id list into {id, name} pairs, naming each
// from the lookup and leaving the name empty for an id the lookup does not know
// — the snapshot can be older than the platform. Used wherever a variable's own
// declared aspect ids need their names from the snapshot rather than from
// whatever upstream answer happened to carry them.
func AspectRefsByID(ids []string, names map[string]string) []AspectRef {
	out := make([]AspectRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, AspectRef{ID: id, Name: names[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AspectRefsFromNodes converts the aspect nodes a path option carries directly —
// the plural list if present, the deprecated single node otherwise — into
// sorted, deduplicated {id, name} pairs.
//
// It exists for the one case where there is no declared variable to read an id
// list from at all (see selection/series.go): there, the option is the only
// source of the aspect, and it already carries the name, so looking it up in the
// snapshot a second time would be redundant work for data already in hand.
// Everywhere else, AspectRefsByID names a variable's own ids from the snapshot.
func AspectRefsFromNodes(nodes []models.AspectNode, alias models.AspectNode) []AspectRef {
	byID := make(map[string]string, len(nodes)+1)
	for _, n := range nodes {
		if n.Id == "" {
			continue
		}
		if _, known := byID[n.Id]; !known {
			byID[n.Id] = n.Name
		}
	}
	if len(byID) == 0 && alias.Id != "" {
		byID[alias.Id] = alias.Name
	}
	out := make([]AspectRef, 0, len(byID))
	for id, name := range byID {
		out = append(out, AspectRef{ID: id, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
