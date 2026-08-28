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
	"io"
	"net/http"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// The routes, from the outside: whose workbenches these are, what happens to a
// request that names none, and what a foreign id gets.

// openWorkbench creates one through the route and returns it.
func (h *repoHarness) openWorkbench(t *testing.T, title string) repo.Workbench {
	t.Helper()
	response := h.call(t, http.MethodPost, "/workbenches",
		map[string]string{"title": title}, "developer")
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create workbench = %d: %s", response.StatusCode, body)
	}
	var bench repo.Workbench
	h.decode(t, response, &bench)
	return bench
}

func TestTheWorkbenchRoutesAreBehindTheDeveloperRole(t *testing.T) {
	h := newRepoHarness(t)

	if response := h.call(t, http.MethodGet, "/workbenches", nil); response.StatusCode !=
		http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", response.StatusCode)
	}
	if response := h.call(t, http.MethodGet, "/workbenches", nil, "analyst"); response.StatusCode !=
		http.StatusForbidden {
		t.Errorf("without the developer role = %d, want 403", response.StatusCode)
	}
}

func TestOpeningAndClosingAWorkbench(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)

	bench := h.openWorkbench(t, "anomalies")
	if bench.ID == "" || bench.Title != "anomalies" {
		t.Fatalf("created workbench = %+v", bench)
	}

	var listed struct {
		Workbenches []repo.Workbench `json:"workbenches"`
		Max         int              `json:"max"`
	}
	h.decode(t, h.call(t, http.MethodGet, "/workbenches", nil, "developer"), &listed)
	if len(listed.Workbenches) != 1 || listed.Workbenches[0].ID != bench.ID {
		t.Fatalf("listing = %+v", listed.Workbenches)
	}
	if listed.Max < 1 {
		t.Errorf("max = %d, want the ceiling the SPA disables its button on", listed.Max)
	}

	if response := h.call(t, http.MethodDelete, "/workbenches/"+bench.ID, nil,
		"developer"); response.StatusCode != http.StatusOK {
		t.Fatalf("close = %d", response.StatusCode)
	}
	h.decode(t, h.call(t, http.MethodGet, "/workbenches", nil, "developer"), &listed)
	if len(listed.Workbenches) != 0 {
		t.Errorf("the closed workbench is still listed: %+v", listed.Workbenches)
	}
}

func TestAWorkbenchIdThatIsNotTheCallersAnswers404(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)

	// Nothing was ever created with this id, and the answer for "belongs to
	// somebody else" is the same one — which is the point: an id in a URL must not
	// be enough to learn that another developer's workbench exists.
	response := h.call(t, http.MethodDelete, "/workbenches/not-a-real-id", nil, "developer")
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("delete of a foreign id = %d, want 404", response.StatusCode)
	}
	response = h.call(t, http.MethodPut, "/workbenches/not-a-real-id",
		map[string]string{"title": "mine now"}, "developer")
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("rename of a foreign id = %d, want 404", response.StatusCode)
	}
}

func TestTheRepoRoutesActInTheWorkbenchTheyName(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)
	first := h.create(t) // Creates a repository, and the workbench holding it.

	second := h.openWorkbench(t, "anomalies")
	response := h.call(t, http.MethodPost, "/repo/link?workbench="+second.ID,
		map[string]any{"full_name": "devuser/anomaly-detect"}, "developer")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("select into the second workbench = %d: %s", response.StatusCode, body)
	}

	// Naming the first one still answers with what is in it, which is the whole
	// point of the parameter.
	var status repo.Status
	h.decode(t, h.call(t, http.MethodGet,
		"/repo?workbench="+first.Link.WorkbenchID, nil, "developer"), &status)
	if status.Link.FullName != first.Link.FullName {
		t.Errorf("the first workbench now holds %q, want %q",
			status.Link.FullName, first.Link.FullName)
	}

	// And naming none, with two open, is refused rather than guessed: choosing
	// between two working copies on the developer's behalf is the failure this
	// whole thing exists to prevent.
	if response := h.call(t, http.MethodGet, "/repo", nil, "developer"); response.StatusCode !=
		http.StatusBadRequest {
		t.Errorf("an ambiguous status read = %d, want 400", response.StatusCode)
	}
}

func TestARepositoryOpenInOneWorkbenchIsRefusedInAnother(t *testing.T) {
	h := newRepoHarness(t)
	h.connect(t)
	first := h.create(t)

	second := h.openWorkbench(t, "")
	response := h.call(t, http.MethodPost, "/repo/link?workbench="+second.ID,
		map[string]any{"full_name": first.Link.FullName}, "developer")
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("selecting the same repository twice = %d, want 409", response.StatusCode)
	}

	var problem map[string]string
	h.decode(t, response, &problem)
	if problem["needs"] != "another_repository" {
		t.Errorf("needs = %q, want the repair the SPA can act on", problem["needs"])
	}
}
