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
	"fmt"
	"reflect"
	"sort"
)

// ChangedPaths reports the leaf paths on which two decoded JSON documents differ,
// plus the paths present on one side only.
//
// This is how a developer's edit is told to the model: a confirmation may be
// approved with an input that differs from what the model proposed (see
// docs/chat-and-streaming.md), and both the decision turn and the record need to
// say *which* fields moved, not just that something did. Exported and tested on its own, because a wrong diff would misreport an
// edit to the one place — the model's own context — that has no way to check it
// against the record.
func ChangedPaths(before, after json.RawMessage) []string {
	changes := diffJSON(before, after)
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.path)
	}
	return paths
}

// jsonDiff is one leaf where two decoded JSON documents differ: its path, and the
// compact JSON of each side. beforeOK/afterOK is false where that side has
// nothing at this path at all, which is a different fact from a leaf whose value
// is JSON null.
type jsonDiff struct {
	path              string
	before, after     string
	beforeOK, afterOK bool
}

// diffJSON is ChangedPaths' own computation, kept separate so changeNote can
// render the values beside each path without re-walking both documents to find
// them again.
func diffJSON(before, after json.RawMessage) []jsonDiff {
	bv, bPresent, bErr := decodeLoose(before)
	av, aPresent, aErr := decodeLoose(after)
	if bErr != nil || aErr != nil {
		// The decision has already run by the time this renders a sentence, so an
		// unparseable side is reported whole rather than turning a best-effort
		// message into a second error.
		return []jsonDiff{{
			path:   "(document)",
			before: string(before), beforeOK: len(before) > 0,
			after: string(after), afterOK: len(after) > 0,
		}}
	}

	var out []jsonDiff
	walkDiff("", bv, bPresent, av, aPresent, &out)
	return out
}

// decodeLoose decodes a possibly-absent JSON document. Absent (len 0) and
// unparseable are different outcomes: the first is silently "nothing here", which
// diffJSON treats as ordinary absence; the second is the one diffJSON gives up on.
func decodeLoose(raw json.RawMessage) (value any, present bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, err
	}
	return value, true, nil
}

// walkDiff recurses into two decoded values in lock step, appending one jsonDiff
// per leaf that differs. Objects recurse by key (sorted, so the result is stable
// across runs on the same input); arrays recurse by index; anything else is a
// leaf, compared as a whole.
func walkDiff(path string, before any, beforeOK bool, after any, afterOK bool, out *[]jsonDiff) {
	// Absent on both sides is not a change — it is what diffJSON's own top-level
	// call passes for two omitted documents, and every recursive call below only
	// ever visits a key or index present in at least one of them.
	if !beforeOK && !afterOK {
		return
	}

	bm, bIsMap := before.(map[string]any)
	am, aIsMap := after.(map[string]any)
	if beforeOK && afterOK && bIsMap && aIsMap {
		keys := make(map[string]struct{}, len(bm)+len(am))
		for k := range bm {
			keys[k] = struct{}{}
		}
		for k := range am {
			keys[k] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			bv, bok := bm[k]
			av, aok := am[k]
			walkDiff(joinField(path, k), bv, bok, av, aok, out)
		}
		return
	}

	bs, bIsSlice := before.([]any)
	as, aIsSlice := after.([]any)
	if beforeOK && afterOK && bIsSlice && aIsSlice {
		n := len(bs)
		if len(as) > n {
			n = len(as)
		}
		for i := 0; i < n; i++ {
			var bv, av any
			bok := i < len(bs)
			aok := i < len(as)
			if bok {
				bv = bs[i]
			}
			if aok {
				av = as[i]
			}
			walkDiff(fmt.Sprintf("%s[%d]", path, i), bv, bok, av, aok, out)
		}
		return
	}

	// A leaf: both sides present with the same container shape falls through to
	// here only when they are not both maps and not both slices, so a value that
	// changed from an object to an array is compared here too, whole.
	if beforeOK && afterOK && reflect.DeepEqual(before, after) {
		return
	}
	*out = append(*out, jsonDiff{
		path:     path,
		before:   compactJSON(before, beforeOK),
		beforeOK: beforeOK,
		after:    compactJSON(after, afterOK),
		afterOK:  afterOK,
	})
}

// joinField extends a path with a map key, without the leading dot a root-level
// field would otherwise carry.
func joinField(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// compactJSON renders one side of a leaf for changeNote. Absent sides are not
// this function's concern — the caller checks presence before trusting the text.
func compactJSON(v any, present bool) string {
	if !present {
		return ""
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(encoded)
}
