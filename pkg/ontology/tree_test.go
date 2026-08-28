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
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
)

func node(id, name, parent string) models.AspectNode {
	root := parent
	if root == "" {
		root = id
	}
	return models.AspectNode{Id: id, Name: name, ParentId: parent, RootId: root}
}

func TestAspectTreeNestsChildrenUnderTheirParent(t *testing.T) {
	tree := AspectTree([]models.AspectNode{
		node("building", "Building", ""),
		node("kitchen", "Kitchen", "building"),
		node("oven", "Oven", "kitchen"),
	})

	if len(tree) != 1 {
		t.Fatalf("got %d roots, want 1: %+v", len(tree), tree)
	}
	if tree[0].Id != "building" {
		t.Fatalf("root = %q, want %q", tree[0].Id, "building")
	}
	if len(tree[0].Children) != 1 || tree[0].Children[0].Id != "kitchen" {
		t.Fatalf("expected Kitchen under Building, got %+v", tree[0].Children)
	}
	grandchildren := tree[0].Children[0].Children
	if len(grandchildren) != 1 || grandchildren[0].Id != "oven" {
		t.Fatalf("expected Oven under Kitchen, got %+v", grandchildren)
	}
}

func TestAspectTreeSortsSiblingsByName(t *testing.T) {
	tree := AspectTree([]models.AspectNode{
		node("b", "Bathroom", ""),
		node("a", "Attic", ""),
		node("c", "Cellar", ""),
	})

	got := []string{tree[0].Name, tree[1].Name, tree[2].Name}
	want := []string{"Attic", "Bathroom", "Cellar"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Losing an aspect would remove a whole subsystem from semantic selection with
// no error anywhere, so an unresolvable parent must promote rather than drop.
func TestAspectTreeKeepsNodesWhoseParentIsAbsent(t *testing.T) {
	tree := AspectTree([]models.AspectNode{
		node("orphan", "Orphan", "nowhere"),
	})

	if len(tree) != 1 || tree[0].Id != "orphan" {
		t.Fatalf("expected the orphan to surface as a root, got %+v", tree)
	}
}

func TestAspectTreeTreatsASelfParentAsARoot(t *testing.T) {
	tree := AspectTree([]models.AspectNode{
		{Id: "self", Name: "Self", ParentId: "self"},
	})

	if len(tree) != 1 || len(tree[0].Children) != 0 {
		t.Fatalf("expected one childless root, got %+v", tree)
	}
}

func TestAspectTreeTerminatesOnACycle(t *testing.T) {
	// a -> b -> a. Neither has an absent parent, so both would otherwise
	// recurse into each other forever.
	tree := AspectTree([]models.AspectNode{
		node("a", "A", "b"),
		node("b", "B", "a"),
	})

	// The pair is unreachable from any root, so the forest is empty rather
	// than infinite. What matters is that the call returns at all.
	if len(tree) != 0 {
		t.Logf("cycle produced %d roots: %+v", len(tree), tree)
	}
}

func TestAspectTreeHandlesAnEmptyOntology(t *testing.T) {
	tree := AspectTree(nil)
	if len(tree) != 0 {
		t.Fatalf("got %d roots, want 0", len(tree))
	}
}

func TestAspectTreeKeepsSiblingSubtreesSeparate(t *testing.T) {
	tree := AspectTree([]models.AspectNode{
		node("house", "House", ""),
		node("kitchen", "Kitchen", "house"),
		node("bath", "Bath", "house"),
		node("sink-k", "Sink", "kitchen"),
	})

	if len(tree[0].Children) != 2 {
		t.Fatalf("expected two rooms, got %+v", tree[0].Children)
	}
	bath, kitchen := tree[0].Children[0], tree[0].Children[1]
	if bath.Name != "Bath" || kitchen.Name != "Kitchen" {
		t.Fatalf("unexpected sibling order: %q, %q", bath.Name, kitchen.Name)
	}
	if len(bath.Children) != 0 {
		t.Errorf("Bath should have no children, got %+v", bath.Children)
	}
	if len(kitchen.Children) != 1 {
		t.Errorf("Kitchen should have one child, got %+v", kitchen.Children)
	}
}

func TestAspectSubtreeIDsCarriesEveryLevelBelowTheNode(t *testing.T) {
	// import-repository matches aspect ids literally, so what this returns is
	// exactly what a criterion covers. A missing grandchild is an import type that
	// silently does not match.
	ids := AspectSubtreeIDs([]models.AspectNode{
		node("house", "House", ""),
		node("kitchen", "Kitchen", "house"),
		node("sink", "Sink", "kitchen"),
		node("bath", "Bath", "house"),
	}, "kitchen")

	want := []string{"kitchen", "sink"}
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("got %v, want %v", ids, want)
		}
	}
}

func TestAspectSubtreeIDsExcludesSiblingsAndAncestors(t *testing.T) {
	ids := AspectSubtreeIDs([]models.AspectNode{
		node("house", "House", ""),
		node("kitchen", "Kitchen", "house"),
		node("bath", "Bath", "house"),
	}, "kitchen")

	for _, id := range ids {
		if id == "house" || id == "bath" {
			t.Fatalf("subtree of kitchen contains %q: %v", id, ids)
		}
	}
}

func TestAspectSubtreeIDsOfAnUnknownNodeIsNil(t *testing.T) {
	// Distinguishable from a leaf, which answers with itself. The caller decides
	// what to do about an aspect the snapshot does not carry; it must not silently
	// become "no aspect filter at all", which matches every import type.
	if ids := AspectSubtreeIDs([]models.AspectNode{node("house", "House", "")}, "kitchen"); ids != nil {
		t.Fatalf("got %v, want nil", ids)
	}
	if ids := AspectSubtreeIDs(nil, ""); ids != nil {
		t.Fatalf("got %v, want nil", ids)
	}
}

func TestAspectSubtreeIDsOfALeafIsTheLeaf(t *testing.T) {
	ids := AspectSubtreeIDs([]models.AspectNode{
		node("house", "House", ""),
		node("kitchen", "Kitchen", "house"),
	}, "kitchen")

	if len(ids) != 1 || ids[0] != "kitchen" {
		t.Fatalf("got %v, want [kitchen]", ids)
	}
}

func TestAspectSubtreeIDsTerminatesOnACycle(t *testing.T) {
	// a -> b -> a. Each is the other's parent, so an unguarded walk would not
	// return. What matters is that it does, with each id once.
	ids := AspectSubtreeIDs([]models.AspectNode{
		node("a", "A", "b"),
		node("b", "B", "a"),
	}, "a")

	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("id %q repeated: %v", id, ids)
		}
		seen[id] = true
	}
}
