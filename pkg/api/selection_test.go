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

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The intent the fake ontology can answer: its measuring function is named
// "power generation" and its aspect tree has a Kitchen.
const testIntent = "forecast power generation in the kitchen"

func decodeSelection(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %q: %v", string(body), err)
	}
	return out
}

func list(t *testing.T, body map[string]any, key string) []any {
	t.Helper()
	value, present := body[key]
	if !present {
		t.Fatalf("%s is absent from %v", key, body)
	}
	if value == nil {
		t.Fatalf("%s arrived as null, want a list", key)
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s = %v, want a list", key, value)
	}
	return items
}

func TestSelectionRejectsAnAnonymousCaller(t *testing.T) {
	h := newProfileHarness(t)
	if w := h.do(t, http.MethodPost, "/selection", map[string]any{"intent": testIntent}); w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestSelectionRejectsATokenWithoutTheDeveloperRole(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection", map[string]any{"intent": testIntent}, "offline_access")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// M2's acceptance criterion over HTTP: a natural-language intent resolves to
// concrete series through the ontology, and the answer says it cost no value read.
func TestSelectionResolvesAnIntentToRankedSeries(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection", map[string]any{"intent": testIntent}, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	body := decodeSelection(t, w.Body.Bytes())

	if len(list(t, body, "matched_functions")) != 1 {
		t.Errorf("matched_functions = %v, want the power function", body["matched_functions"])
	}
	if len(list(t, body, "matched_aspects")) != 1 {
		t.Errorf("matched_aspects = %v, want the kitchen aspect", body["matched_aspects"])
	}
	if len(list(t, body, "selectables")) != 1 {
		t.Errorf("selectables = %v, want the one variable path", body["selectables"])
	}
	if len(list(t, body, "candidate_devices")) != 1 {
		t.Errorf("candidate_devices = %v, want the one readable device", body["candidate_devices"])
	}

	candidates := list(t, body, "candidates")
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want one ranked series", candidates)
	}
	ref := candidates[0].(map[string]any)["series_ref"].(map[string]any)
	if ref["variable_path"] != powerPath {
		t.Errorf("variable_path = %v, want %s", ref["variable_path"], powerPath)
	}

	reads := body["reads"].(map[string]any)
	if reads["values"].(float64) != 0 {
		t.Errorf("reads.values = %v, want 0 — selection completes at tier L0", reads["values"])
	}
	if reads["selectables"].(float64) != 1 {
		t.Errorf("reads.selectables = %v, want one per criterion", reads["selectables"])
	}
}

// The selectable is the device-type level answer and carries what the ontology
// declares about the variable, so a developer can see the unit before choosing.
func TestSelectionReportsResolvedUnitsAndCompleteness(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection", map[string]any{"intent": testIntent}, "developer")
	body := decodeSelection(t, w.Body.Bytes())

	selectable := list(t, body, "selectables")[0].(map[string]any)
	if selectable["unit"] != "W" {
		t.Errorf("unit = %v, want W", selectable["unit"])
	}
	if selectable["characteristic_id"] != "ch-watt" {
		t.Errorf("characteristic_id = %v, want ch-watt", selectable["characteristic_id"])
	}
	if selectable["queryable"] != true {
		t.Errorf("queryable = %v, want true", selectable["queryable"])
	}
	completeness := selectable["ontology_completeness"].(map[string]any)
	if completeness["status"] != "complete" {
		t.Errorf("completeness = %v, want complete", completeness)
	}
	if _, present := body["ontology_gaps"]; !present {
		t.Error("ontology_gaps is absent; it must be an empty list rather than missing")
	}
}

// An intent with nothing to resolve is refused rather than answered: with no
// criteria there is no query to send, and an empty criteria list matches every
// device type on the platform.
func TestSelectionRefusesARequestWithNothingToResolve(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection", map[string]any{}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	if body := decodeSelection(t, w.Body.Bytes()); body["error"] == nil {
		t.Error("no error message explaining what the request needs")
	}
}

// An intent the ontology has no words for is a 200 with an empty resolution: the
// request was well formed, and the answer is that this platform describes nothing
// of the kind.
func TestAnUnresolvableIntentAnswersWithAnExplanation(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection",
		map[string]any{"intent": "Photovoltaik Erzeugung"}, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	body := decodeSelection(t, w.Body.Bytes())

	if len(list(t, body, "candidates")) != 0 || len(list(t, body, "criteria")) != 0 {
		t.Error("an unresolvable intent produced a resolution")
	}
	if len(list(t, body, "notes")) == 0 {
		t.Error("notes are empty; an empty answer with no explanation is unreadable")
	}
	if len(list(t, body, "unmatched_terms")) != 2 {
		t.Errorf("unmatched_terms = %v, want the words the ontology does not have",
			body["unmatched_terms"])
	}
}

func TestSelectionRejectsAnUnknownInteraction(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection",
		map[string]any{"intent": testIntent, "interaction": "carrier pigeon"}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestSelectionRejectsAHalfWindow(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection", map[string]any{
		"intent": testIntent,
		"window": map[string]any{"from": "2026-01-01T00:00:00Z"},
	}, "developer")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

// rank:false is the cheap form of the operation — the ontology resolution without
// the per-device availability calls.
func TestSelectionWithoutRankingSkipsThePerDeviceReads(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection",
		map[string]any{"intent": testIntent, "rank": false}, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	body := decodeSelection(t, w.Body.Bytes())

	if len(list(t, body, "candidates")) != 0 {
		t.Error("candidates were ranked although ranking was declined")
	}
	if len(list(t, body, "selectables")) == 0 {
		t.Error("declining the ranking dropped the ontology resolution too")
	}
	reads := body["reads"].(map[string]any)
	if reads["availability"].(float64) != 0 {
		t.Errorf("reads.availability = %v, want 0", reads["availability"])
	}
}

// The route is served without a profiler, because it needs only the ontology and
// the device repository. §5.2's answer stands; the availability-based order does
// not, and the response says so.
func TestSelectionIsServedWithoutAProfiler(t *testing.T) {
	h := newHarness(t)
	req := map[string]any{"intent": testIntent}

	w := h.post(t, "/selection", req, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	body := decodeSelection(t, w.Body.Bytes())

	if len(list(t, body, "selectables")) == 0 {
		t.Error("no selectables without a profiler")
	}
	if len(list(t, body, "candidates")) != 0 {
		t.Error("candidates appeared without a profiler")
	}
	found := false
	for _, note := range list(t, body, "notes") {
		if text, ok := note.(string); ok && strings.Contains(text, "ranking is unavailable") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the missing ranking reported", body["notes"])
	}
}

// Explicit ids are how the LLM calls this once it has read the ontology itself, so
// they have to work with no intent at all.
func TestSelectionAcceptsExplicitIdsWithoutAnIntent(t *testing.T) {
	h := newProfileHarness(t)
	w := h.do(t, http.MethodPost, "/selection",
		map[string]any{"function_ids": []string{"fn-power"}}, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	body := decodeSelection(t, w.Body.Bytes())

	matched := list(t, body, "matched_functions")
	if len(matched) != 1 {
		t.Fatalf("matched_functions = %v, want the explicit id", matched)
	}
	evidence := matched[0].(map[string]any)["matched"].(map[string]any)
	if evidence["basis"] != "explicit_id" {
		t.Errorf("basis = %v, want explicit_id", evidence["basis"])
	}
	if len(list(t, body, "candidates")) != 1 {
		t.Errorf("candidates = %v, want the resolved series", body["candidates"])
	}
}
