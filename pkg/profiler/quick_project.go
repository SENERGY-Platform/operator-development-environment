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

package profiler

import (
	"encoding/json"
	"fmt"

	"github.com/SENERGY-Platform/models/go/models"
)

// QuickView is the model-facing form of a QuickResult — the L0 half of D26,
// beside LLMProfileView for the full tier.
//
// A QuickProfile is written for the UI and for the record, and eighty of them
// cost around forty-eight thousand tokens: per-field provenance alone is a third
// of that, and the same not_computed prose about a characteristic that declares
// no bound repeats once per candidate. A tool whose whole purpose is to rank a
// shortlist cheaply cannot hand the model a payload that large, so there is one
// stored form and one projection, and the projection records what it left out.
type QuickView struct {
	Tier       string               `json:"tier"`
	Candidates []QuickCandidateView `json:"candidates"`
	Skipped    []SkippedDevice      `json:"skipped"`
	Window     Window               `json:"coverage_window"`

	// DevicesListed and TotalDevices describe the listing the candidates came
	// from. The profiler does not list devices, so the caller that did fills
	// these in after projecting; the keys are counted in the budget either way.
	DevicesListed int   `json:"devices_listed"`
	TotalDevices  int64 `json:"total_devices"`

	// Reads is part of the answer on purpose (§3.2): a non-zero values figure
	// here would mean tier L0 had been broken.
	Reads ReadCounts `json:"reads"`

	Elided        []Elision       `json:"elided"`
	ElidedDevices []DeviceElision `json:"elided_devices,omitempty"`
	Caveat        string          `json:"caveat"`
}

// QuickCandidateView is one candidate as the model reads it.
//
// What survives is what a triage decision needs: what the series is, how much
// history it has, whether it is live, and what the ontology knows about it.
// Dropped, in the same spirit as LLMProfileView dropping provenance: the
// per-field provenance sidecar, the list of materialised aggregates (ODE's own
// read-strategy bookkeeping — profile_series picks the bucket, the model never
// passes one), the consequence prose of an incomplete ontology entry (derivable
// from missing), and the fields that are the same for every candidate, which the
// view states once in Caveat instead.
type QuickCandidateView struct {
	SeriesRef SeriesRef       `json:"series_ref"`
	Device    QuickDeviceView `json:"device"`

	Availability Value[QuickAvailabilityView] `json:"availability"`
	Volume       Value[QuickVolumeView]       `json:"volume"`
	Declared     QuickDeclaredView            `json:"declared"`
	Interaction  models.Interaction           `json:"interaction"`
	Liveness     QuickLivenessView            `json:"liveness"`

	OntologyCompleteness QuickCompletenessView `json:"ontology_completeness"`
	RankHints            QuickRankView         `json:"rank_hints"`

	// Queryable is stated rather than implied by absence: a variable ODE cannot
	// read as a series is still reported, and "absent means yes" is not a
	// reading anyone should have to guess at.
	Queryable bool   `json:"queryable"`
	Reason    string `json:"reason,omitempty"`
}

// QuickDeviceView keeps the device type's id as well as its name because
// list_devices takes device_type_ids: "show me the other meters of this type" is
// a move the model can actually make from it.
type QuickDeviceView struct {
	Name           string `json:"name"`
	DeviceTypeID   string `json:"device_type_id"`
	DeviceTypeName string `json:"device_type_name"`
}

type QuickAvailabilityView struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	SpanDays float64 `json:"span_days"`
	// RawAvailable false means retention has aged the raw data out, which decides
	// whether a full profile is possible at all.
	RawAvailable bool `json:"raw_available"`
	// AggregatesAvailable replaces the list of materialised aggregates: whether
	// pre-aggregated variants exist is the actionable half, which bucket widths
	// exist is not.
	AggregatesAvailable bool `json:"aggregates_available"`
}

type QuickVolumeView struct {
	Bytes       uint64  `json:"bytes"`
	BytesPerDay float64 `json:"bytes_per_day"`
	// EstimatedIntervalS keeps its not_computed status: an ingest rate the
	// platform does not report yet is a different thing from a fast series.
	EstimatedIntervalS Value[float64] `json:"estimated_interval_s"`
}

// QuickDeclaredView carries only what the ontology actually states.
//
// Min and Max are absent when the characteristic declares no bound. That is not
// the D24 hole it looks like: at L0 the ontology is the only source of a bound,
// so "the ontology declares none" is the determination and not a detector that
// failed. The alternative — a not_computed envelope per bound per candidate —
// was a fifth of the payload saying the same sentence eighty times.
type QuickDeclaredView struct {
	CharacteristicID *string     `json:"characteristic_id,omitempty"`
	Unit             string      `json:"unit,omitempty"`
	UnitSource       UnitSource  `json:"unit_source"`
	Type             models.Type `json:"type,omitempty"`
	FunctionID       string      `json:"function_id,omitempty"`
	AspectID         string      `json:"aspect_id,omitempty"`
	Min              *float64    `json:"min,omitempty"`
	Max              *float64    `json:"max,omitempty"`
}

type QuickLivenessView struct {
	ConnectionState models.ConnectionState `json:"connection_state"`
	LastValueAgeS   Value[float64]         `json:"last_value_age_s"`
}

type QuickCompletenessView struct {
	Status  string   `json:"status"`
	Missing []string `json:"missing,omitempty"`
}

// QuickRankView drops span_days, which availability already carries, and keeps
// the inputs that are not readable anywhere else in the candidate (§5.2).
type QuickRankView struct {
	CoverageProxy float64 `json:"coverage_proxy"`
	IsLive        bool    `json:"is_live"`
	Score         float64 `json:"score"`
}

// DeviceElision records a device whose candidates did not all fit.
//
// The name is here rather than only the id because a device that lost every one
// of its candidates would otherwise be a URN the model cannot talk about, and
// "the heat pump also has twenty-seven candidate series" is exactly what the
// developer needs to hear.
type DeviceElision struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Total    int    `json:"total"`
	Shown    int    `json:"shown"`
}

// QuickCaveat states once what the per-candidate view no longer repeats.
//
// estimate_read_cost already answers this way: the caveat that governs how a
// number may be used belongs in the response, not in a comment nobody reading
// the response can see.
const QuickCaveat = "these are ranked candidates, strongest first, assembled without reading a " +
	"single value. estimated_interval_s is order of magnitude only, derived from a device-wide " +
	"byte rate, and is never a resampling basis — profile_series detects the real interval. " +
	"liveness is judged from the end of the availability window, not from a value read. " +
	"This is a projection for you: per-field provenance, the list of materialised aggregate " +
	"buckets and the ontology's reasons for an absent bound are dropped, and the candidate " +
	"list may be truncated to fit a token budget — read elided and elided_devices, and narrow " +
	"the search or the device set rather than assuming you have seen everything."

const fieldCandidates = "candidates"

// ProjectQuick collapses a QuickResult into the model-facing view.
//
// tokenBudget of zero means no budget pressure, as with Project.
func ProjectQuick(result QuickResult, tokenBudget int) QuickView {
	view := QuickView{
		Tier:       TierQuick,
		Candidates: []QuickCandidateView{},
		Skipped:    result.Skipped,
		Window:     result.Window,
		Reads:      result.Reads,
		Elided:     []Elision{},
		Caveat:     QuickCaveat,
	}
	if view.Skipped == nil {
		view.Skipped = []SkippedDevice{}
	}

	projected := ProjectQuickCandidates(result.Candidates, tokenBudget, EnvelopeBytes(view))
	view.Candidates = projected.Shown
	view.Elided = projected.Elided
	view.ElidedDevices = projected.ElidedDevices
	return view
}

// QuickCandidates is a projected candidate list with the record of what fitting
// it to the budget cost.
type QuickCandidates struct {
	Shown         []QuickCandidateView
	Elided        []Elision
	ElidedDevices []DeviceElision
}

// ProjectQuickCandidates projects every candidate and then keeps as many of them
// as the budget allows.
//
// envelopeBytes is what the document around the list costs, so that the budget
// bounds the response a model receives rather than the list alone — the caller
// measures its own envelope with EnvelopeBytes.
//
// The budget is spent a device at a time, each device offering its next-best
// candidate, and the survivors are returned in rank order. Taking the ranked
// prefix instead would read as the more faithful thing to do, but the scores of a
// fleet of one device type tie: same span, same coverage, all online. Ranking is
// then stable rather than meaningful, and a prefix answers a question about three
// inverters with fourteen series from whichever was listed first. Spending it by
// device keeps every device the caller asked about in the answer, and within a
// device the order is still the ranking's.
func ProjectQuickCandidates(candidates []QuickProfile, tokenBudget int, envelopeBytes int) QuickCandidates {
	shown := make([]QuickCandidateView, 0, len(candidates))
	for _, candidate := range candidates {
		shown = append(shown, ProjectQuickCandidate(candidate))
	}
	out := QuickCandidates{Shown: shown, Elided: []Elision{}}
	if tokenBudget <= 0 || len(shown) == 0 {
		return out
	}

	// The reserve is the largest elision block this list could produce — one
	// entry per device, every candidate cut. Measuring the worst case keeps the
	// result inside the budget without having to know the answer first.
	limit := tokenBudget*bytesPerToken - envelopeBytes - elisionReserve(candidates)

	kept := fitByDevice(shown, limit)
	if len(kept) >= len(shown) {
		return out
	}

	out.Shown = kept
	out.Elided = append(out.Elided, Elision{
		Field: fieldCandidates, Total: len(shown), Shown: len(kept),
	})
	out.ElidedDevices = elidedDevices(candidates, kept)
	return out
}

// fitByDevice fills the byte limit round by round, one candidate per device per
// round, and returns what fits in rank order.
//
// The first candidate is taken whatever it costs: a budget too small for a single
// candidate is a misconfiguration, and an empty list plus a count of what is
// missing would hide the candidates rather than report them.
func fitByDevice(shown []QuickCandidateView, limit int) []QuickCandidateView {
	order := []string{}
	queue := map[string][]int{}
	for index, view := range shown {
		id := view.SeriesRef.DeviceID
		if _, seen := queue[id]; !seen {
			order = append(order, id)
		}
		queue[id] = append(queue[id], index)
	}

	sizes := make([]int, len(shown))
	for index, view := range shown {
		sizes[index] = len(marshalled(view)) + 1 // the comma that joins it to the list
	}

	taken := map[int]bool{}
	used := 0
	for advanced := true; advanced; {
		advanced = false
		for _, id := range order {
			pending := queue[id]
			if len(pending) == 0 {
				continue
			}
			index := pending[0]
			// A device whose next candidate does not fit keeps it: another device's
			// might, and the round after this one may find room for neither, which
			// is what ends the loop.
			if len(taken) > 0 && used+sizes[index] > limit {
				continue
			}
			queue[id] = pending[1:]
			taken[index] = true
			used += sizes[index]
			advanced = true
		}
	}

	kept := make([]QuickCandidateView, 0, len(taken))
	for index, view := range shown {
		if taken[index] {
			kept = append(kept, view)
		}
	}
	return kept
}

// ProjectQuickCandidate is the per-candidate half, exported for the callers that
// project a list they assembled themselves.
func ProjectQuickCandidate(profile QuickProfile) QuickCandidateView {
	view := QuickCandidateView{
		SeriesRef: profile.SeriesRef,
		Device: QuickDeviceView{
			Name:           profile.Device.Name,
			DeviceTypeID:   profile.Device.DeviceTypeID,
			DeviceTypeName: profile.Device.DeviceTypeName,
		},
		Interaction: profile.Interaction,
		Liveness: QuickLivenessView{
			ConnectionState: profile.Liveness.ConnectionState,
			LastValueAgeS:   roundedValue(profile.Liveness.LastValueAgeS, 1),
		},
		OntologyCompleteness: QuickCompletenessView{
			Status:  profile.OntologyCompleteness.Status,
			Missing: profile.OntologyCompleteness.Missing,
		},
		RankHints: QuickRankView{
			CoverageProxy: roundTo(profile.RankHints.CoverageProxy, 3),
			IsLive:        profile.RankHints.IsLive,
			Score:         roundTo(profile.RankHints.Score, 3),
		},
		Queryable: profile.Queryable,
		Reason:    profile.Reason,
	}

	if window, ok := profile.Availability.Get(); ok {
		view.Availability = Computed(QuickAvailabilityView{
			From:                window.From.UTC().Format(quickTimeFormat),
			To:                  window.To.UTC().Format(quickTimeFormat),
			SpanDays:            roundTo(window.SpanDays, 2),
			RawAvailable:        window.RawAvailable,
			AggregatesAvailable: len(window.Aggregates) > 0,
		})
	} else {
		status := profile.Availability.Status()
		view.Availability = Uncomputable[QuickAvailabilityView](status.Reason, status.Detail)
	}

	if volume, ok := profile.Volume.Get(); ok {
		view.Volume = Computed(QuickVolumeView{
			Bytes:              volume.Bytes,
			BytesPerDay:        roundTo(volume.BytesPerDay, 1),
			EstimatedIntervalS: roundedValue(volume.EstimatedIntervalS, 1),
		})
	} else {
		status := profile.Volume.Status()
		view.Volume = Uncomputable[QuickVolumeView](status.Reason, status.Detail)
	}

	view.Declared = QuickDeclaredView{
		CharacteristicID: profile.Declared.CharacteristicID,
		Unit:             profile.Declared.Unit,
		UnitSource:       profile.Declared.UnitSource,
		Type:             profile.Declared.Type,
		FunctionID:       profile.Declared.FunctionID,
		AspectID:         profile.Declared.AspectID,
	}
	if minimum, ok := profile.Declared.MinValue.Get(); ok {
		view.Declared.Min = &minimum
	}
	if maximum, ok := profile.Declared.MaxValue.Get(); ok {
		view.Declared.Max = &maximum
	}
	return view
}

// quickTimeFormat drops the sub-second precision of an availability window. A
// window end reported to the millisecond invites the model to read a coverage
// boundary as an exact measurement, and it is a byte rate away from being one.
const quickTimeFormat = "2006-01-02T15:04:05Z"

// EnvelopeBytes measures the document a projected candidate list sits in. Pass
// the view with an empty candidate list.
func EnvelopeBytes(view any) int { return len(marshalled(view)) }

func marshalled(v any) []byte {
	encoded, err := json.Marshal(v)
	if err != nil {
		// Nothing here can fail to marshal, and a size estimate is not worth an
		// error return that every caller would have to ignore. A zero-length
		// estimate errs towards a smaller answer, which is the safe direction.
		return nil
	}
	return encoded
}

// roundedValue rounds a computed number and leaves a not_computed one alone,
// which is what keeps the distinction D24 exists for through the projection.
func roundedValue(v Value[float64], places int) Value[float64] {
	number, ok := v.Get()
	if !ok {
		return v
	}
	return Computed(roundTo(number, places))
}

func elisionReserve(candidates []QuickProfile) int {
	return len(marshalled(struct {
		Elided        []Elision       `json:"elided"`
		ElidedDevices []DeviceElision `json:"elided_devices"`
	}{
		Elided:        []Elision{{Field: fieldCandidates, Total: len(candidates), Shown: len(candidates)}},
		ElidedDevices: elidedDevices(candidates, nil),
	}))
}

// elidedDevices reports, per device and in rank order, how many of its
// candidates survived.
func elidedDevices(candidates []QuickProfile, shown []QuickCandidateView) []DeviceElision {
	order := []string{}
	totals := map[string]int{}
	names := map[string]string{}

	for _, candidate := range candidates {
		id := candidate.SeriesRef.DeviceID
		if _, seen := totals[id]; !seen {
			order = append(order, id)
			names[id] = candidate.Device.Name
		}
		totals[id]++
	}

	kept := map[string]int{}
	for _, view := range shown {
		kept[view.SeriesRef.DeviceID]++
	}

	out := []DeviceElision{}
	for _, id := range order {
		if kept[id] >= totals[id] {
			continue
		}
		out = append(out, DeviceElision{
			DeviceID: id, Name: names[id], Total: totals[id], Shown: kept[id],
		})
	}
	return out
}

// QuickTruncationNote says in prose what Elided says in numbers, for the callers
// whose response already carries a notes list.
func QuickTruncationNote(shown, total int) string {
	return fmt.Sprintf("%d of %d ranked candidates are shown, strongest first, to stay inside "+
		"the token budget; elided_devices names every device whose candidates were cut", shown, total)
}
