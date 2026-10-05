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
	"reflect"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
)

func TestAspectIDsPrefersTheListOverTheAlias(t *testing.T) {
	got := AspectIDs([]string{"b", "a"}, "c")
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v: the list wins and is sorted", got, want)
	}
}

func TestAspectIDsFallsBackToTheAliasWhenTheListIsEmpty(t *testing.T) {
	got := AspectIDs(nil, "a")
	want := []string{"a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v: the alias stands in for an empty list", got, want)
	}
}

func TestAspectIDsIsEmptyWhenBothAreEmpty(t *testing.T) {
	if got := AspectIDs(nil, ""); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestAspectIDsSortsAndDeduplicates(t *testing.T) {
	got := AspectIDs([]string{"b", "a", "b", "a"}, "")
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v: sorted and deduplicated so order never affects a set comparison", got, want)
	}

	// The same set, listed in the other order, must render the same way — this is
	// what makes EqualAspectSets a plain positional comparison.
	reordered := AspectIDs([]string{"a", "b"}, "")
	if !EqualAspectSets(got, reordered) {
		t.Errorf("got %v and %v, want the same set regardless of input order", got, reordered)
	}
}

func TestEqualAspectSets(t *testing.T) {
	if !EqualAspectSets(nil, nil) {
		t.Error("two empty sets should be equal")
	}
	if EqualAspectSets([]string{"a"}, nil) {
		t.Error("a non-empty set must not equal an empty one")
	}
	if !EqualAspectSets([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("identical sorted sets should be equal")
	}
	if EqualAspectSets([]string{"a", "b"}, []string{"a", "c"}) {
		t.Error("different sets must not be equal")
	}
}

func TestAspectRefsByIDNamesFromTheLookupAndSortsByID(t *testing.T) {
	names := map[string]string{"b": "Bathroom", "a": "Attic"}
	refs := AspectRefsByID([]string{"b", "a"}, names)
	if len(refs) != 2 || refs[0].ID != "a" || refs[0].Name != "Attic" || refs[1].ID != "b" {
		t.Fatalf("refs = %+v, want sorted by id with names filled in", refs)
	}
}

func TestAspectRefsByIDLeavesAnUnknownIDUnnamed(t *testing.T) {
	refs := AspectRefsByID([]string{"unknown"}, map[string]string{})
	if len(refs) != 1 || refs[0].ID != "unknown" || refs[0].Name != "" {
		t.Errorf("refs = %+v, want the id kept with an empty name", refs)
	}
}

func TestAspectRefsFromNodesPrefersThePluralList(t *testing.T) {
	refs := AspectRefsFromNodes(
		[]models.AspectNode{{Id: "b", Name: "Bathroom"}, {Id: "a", Name: "Attic"}},
		models.AspectNode{Id: "c", Name: "Cellar"},
	)
	if len(refs) != 2 || refs[0].ID != "a" || refs[1].ID != "b" {
		t.Fatalf("refs = %+v, want the plural list, sorted, alias ignored", refs)
	}
}

func TestAspectRefsFromNodesFallsBackToTheAlias(t *testing.T) {
	refs := AspectRefsFromNodes(nil, models.AspectNode{Id: "c", Name: "Cellar"})
	if len(refs) != 1 || refs[0].ID != "c" || refs[0].Name != "Cellar" {
		t.Fatalf("refs = %+v, want the alias as a single-element list", refs)
	}
}

func TestAspectRefsFromNodesIsEmptyWhenNeitherIsSet(t *testing.T) {
	if refs := AspectRefsFromNodes(nil, models.AspectNode{}); len(refs) != 0 {
		t.Errorf("refs = %+v, want none", refs)
	}
}

func TestAspectNodeIDsExtractsIDsInOrder(t *testing.T) {
	ids := AspectNodeIDs([]models.AspectNode{{Id: "a"}, {Id: "b"}})
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Errorf("ids = %v, want [a b]", ids)
	}
}
