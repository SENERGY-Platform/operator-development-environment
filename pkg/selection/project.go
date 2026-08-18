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
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// LLMResult is the model-facing form of a Result.
//
// Selection is where the forty candidates of §5.2 come from, and they are
// QuickProfiles: unprojected, the ranked list alone is tens of thousands of
// tokens, which is the cost the ranking exists to avoid. Everything else in the
// document — matches, criteria, selectables, gaps, notes — is already written to
// be read, so only the candidate list is projected.
//
// The embedded Result is what keeps the two in step: a field added there reaches
// the model without an edit here. Candidates is declared again at this level so
// that encoding/json takes the projected list — a field at the shallower depth
// wins the tag — and the embedded copy is cleared so there is only ever one.
type LLMResult struct {
	Result

	Candidates    []profiler.QuickCandidateView `json:"candidates"`
	Elided        []profiler.Elision            `json:"elided"`
	ElidedDevices []profiler.DeviceElision      `json:"elided_devices,omitempty"`
	Caveat        string                        `json:"caveat"`
}

// ProjectForLLM projects the ranked candidates and fits them to the budget.
//
// tokenBudget of zero means no budget pressure, as with profiler.Project.
func (r Result) ProjectForLLM(tokenBudget int) LLMResult {
	view := LLMResult{
		Result:     r,
		Candidates: []profiler.QuickCandidateView{},
		Elided:     []profiler.Elision{},
		Caveat:     profiler.QuickCaveat,
	}
	view.Result.Candidates = nil

	projected := profiler.ProjectQuickCandidates(
		r.Candidates, tokenBudget, profiler.EnvelopeBytes(view))
	view.Candidates = projected.Shown
	view.Elided = projected.Elided
	view.ElidedDevices = projected.ElidedDevices

	if len(projected.ElidedDevices) > 0 || len(projected.Elided) > 0 {
		// Notes is where this document already says that a cap was applied, and
		// silence about a truncation reads as completeness. Appended to a copy:
		// the caller's Result is not this method's to modify.
		notes := make([]string, 0, len(r.Notes)+1)
		notes = append(notes, r.Notes...)
		view.Notes = append(notes,
			profiler.QuickTruncationNote(len(projected.Shown), len(r.Candidates)))
	}
	return view
}
