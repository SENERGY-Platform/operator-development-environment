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

package chat

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// sortedPaths is ChangedPaths with its own order canonicalised, so a test can
// assert on a set of paths without depending on the map-iteration order the
// implementation happens to produce beyond its own documented key sort.
func sortedPaths(before, after json.RawMessage) []string {
	got := ChangedPaths(before, after)
	sort.Strings(got)
	return got
}

func TestChangedPathsOnIdenticalDocumentsIsEmpty(t *testing.T) {
	doc := json.RawMessage(`{"a":1,"b":{"c":2},"d":[1,2,3]}`)
	got := ChangedPaths(doc, json.RawMessage(`{"a":1,"b":{"c":2},"d":[1,2,3]}`))
	if len(got) != 0 {
		t.Errorf("ChangedPaths on identical documents = %v, want none", got)
	}
}

func TestChangedPathsOverNestedObjects(t *testing.T) {
	before := json.RawMessage(`{"a":{"b":{"c":1}}}`)
	after := json.RawMessage(`{"a":{"b":{"c":2}}}`)
	want := []string{"a.b.c"}
	if got := sortedPaths(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedPaths = %v, want %v", got, want)
	}
}

func TestChangedPathsOverArrays(t *testing.T) {
	before := json.RawMessage(`{"topics":[{"filterValue":"urn:a"},{"filterValue":"urn:x"}]}`)
	after := json.RawMessage(`{"topics":[{"filterValue":"urn:b"},{"filterValue":"urn:x"}]}`)
	want := []string{"topics[0].filterValue"}
	if got := sortedPaths(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedPaths = %v, want %v", got, want)
	}
}

func TestChangedPathsOverOneSidedKeys(t *testing.T) {
	before := json.RawMessage(`{"a":1,"b":2}`)
	after := json.RawMessage(`{"a":1,"c":3}`)
	want := []string{"b", "c"}
	if got := sortedPaths(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedPaths = %v, want %v (b present only before, c present only after)", got, want)
	}
}

func TestChangedPathsOverATypeChange(t *testing.T) {
	before := json.RawMessage(`{"a":1}`)
	after := json.RawMessage(`{"a":"1"}`)
	want := []string{"a"}
	if got := sortedPaths(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedPaths = %v, want %v (a number becoming a same-looking string is still a change)", got, want)
	}
}

func TestChangedPathsOverAOneSidedArrayElement(t *testing.T) {
	before := json.RawMessage(`{"d":[1]}`)
	after := json.RawMessage(`{"d":[1,2]}`)
	want := []string{"d[1]"}
	if got := sortedPaths(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedPaths = %v, want %v", got, want)
	}
}

// An unparseable side is not this diff's job to explain — the decision has
// already run — so it is reported as one whole-document change rather than an
// error a caller would have to handle specially.
func TestChangedPathsOnUnparseableInputIsOneEntryNotAnError(t *testing.T) {
	got := ChangedPaths(json.RawMessage(`{"a":1}`), json.RawMessage(`{not valid json`))
	if len(got) != 1 {
		t.Fatalf("ChangedPaths on unparseable input = %v, want exactly one entry", got)
	}
}

func TestChangedPathsOnAbsentSidesIsEmpty(t *testing.T) {
	// Absent (len 0) is not "unparseable" — it is what an ordinary, unedited
	// decision passes as "applied", and must diff as no change at all rather than
	// as a document-wide one.
	got := ChangedPaths(nil, nil)
	if len(got) != 0 {
		t.Errorf("ChangedPaths(nil, nil) = %v, want none", got)
	}
}
