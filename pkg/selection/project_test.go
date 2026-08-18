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

package selection

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// rankedResult is a resolved selection with the ranked candidate list the LLM
// projection exists to bound: two devices, several variables each, every
// candidate carrying the provenance sidecar a stored QuickProfile carries.
func rankedResult(devices, variables int) Result {
	result := Result{
		Intent:       "pv inverter power",
		Terms:        []string{"pv", "power"},
		Selectables:  []Selectable{{DeviceTypeID: "dt-1", ServiceID: "svc-1", Path: "root.acv", Unit: "V"}},
		Notes:        []string{"one criteria combination was dropped at the cap of 12"},
		Candidates:   []profiler.QuickProfile{},
		Skipped:      []profiler.SkippedDevice{},
		TotalDevices: int64(devices),
	}
	for device := 0; device < devices; device++ {
		deviceID := fmt.Sprintf("urn:infai:ses:device:%040d", device)
		for variable := 0; variable < variables; variable++ {
			result.Candidates = append(result.Candidates, profiler.QuickProfile{
				SeriesRef: profiler.SeriesRef{
					DeviceID:     deviceID,
					ServiceID:    fmt.Sprintf("urn:infai:ses:service:%040d", variable),
					VariablePath: fmt.Sprintf("root.value_%d", variable),
				},
				Device: profiler.DeviceInfo{
					Name:           fmt.Sprintf("Inverter %d", device),
					DeviceTypeID:   "urn:infai:ses:device-type:1",
					DeviceTypeName: "APSystems DS3-S",
				},
				Tier:        profiler.TierQuick,
				Interaction: models.EVENT,
				Queryable:   true,
				Provenance: profiler.Provenance{
					"availability": profiler.ProvenanceEntry{
						Source: profiler.SourceAPI, Ref: "timescale-wrapper:/data-availability",
					},
				},
			})
		}
	}
	return result
}

func TestTheSelectionProjectionBoundsTheCandidatesAndLeavesTheRestOfTheDocument(t *testing.T) {
	result := rankedResult(2, 6)

	view := result.ProjectForLLM(1200)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if count := strings.Count(string(encoded), `"candidates":`); count != 1 {
		t.Errorf("the document carries the candidate list %d times, want once", count)
	}
	// The candidates alone: the top-level caveat talks *about* provenance being
	// dropped, which is the point of saying it once instead of per candidate.
	candidates, err := json.Marshal(view.Candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}
	if strings.Contains(string(candidates), "provenance") {
		t.Error("the projected candidates still carry the provenance sidecar")
	}
	if len(view.Candidates) == 0 || len(view.Candidates) >= len(result.Candidates) {
		t.Fatalf("shown = %d of %d; the budget should have cut some and kept some",
			len(view.Candidates), len(result.Candidates))
	}

	// Everything that is not the candidate list is written to be read already.
	if view.Intent != result.Intent || len(view.Selectables) != 1 {
		t.Errorf("the resolution itself did not survive: %+v", view.Result)
	}
}

func TestATruncatedSelectionSaysSoInItsNotes(t *testing.T) {
	result := rankedResult(2, 6)

	view := result.ProjectForLLM(1200)

	if len(view.Notes) != len(result.Notes)+1 {
		t.Fatalf("notes = %v, want the original plus one about the truncation", view.Notes)
	}
	if view.Notes[0] != result.Notes[0] {
		t.Errorf("the original note was replaced rather than appended to: %v", view.Notes)
	}
	if !strings.Contains(view.Notes[len(view.Notes)-1], "ranked candidates are shown") {
		t.Errorf("the added note does not say what happened: %q", view.Notes[len(view.Notes)-1])
	}
	if len(view.Elided) != 1 || view.Elided[0].Total != len(result.Candidates) {
		t.Errorf("elided = %v, want one entry counting all %d candidates",
			view.Elided, len(result.Candidates))
	}
}

// Notes belongs to the caller's Result; a projection that appended to it in place
// would change the document the websocket surface serves.
func TestTheSelectionProjectionDoesNotTouchTheResultItWasGiven(t *testing.T) {
	result := rankedResult(2, 6)
	notes := len(result.Notes)
	candidates := len(result.Candidates)

	result.ProjectForLLM(1200)

	if len(result.Notes) != notes || len(result.Candidates) != candidates {
		t.Errorf("the source result changed: %d notes, %d candidates", len(result.Notes), len(result.Candidates))
	}
}

func TestAnUnbudgetedSelectionProjectionKeepsEveryCandidateAndAddsNoNote(t *testing.T) {
	result := rankedResult(2, 6)

	view := result.ProjectForLLM(0)

	if len(view.Candidates) != len(result.Candidates) {
		t.Errorf("shown = %d, want all %d", len(view.Candidates), len(result.Candidates))
	}
	if len(view.Notes) != len(result.Notes) {
		t.Errorf("notes = %v, want the original ones unchanged", view.Notes)
	}
}
