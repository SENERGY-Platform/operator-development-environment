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

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// ---- search_ontology (L0) ----

type searchOntologyInput struct {
	Query              string `json:"query"`
	IncludeControlling bool   `json:"include_controlling"`
	Limit              int    `json:"limit"`
}

func (s *surface) searchOntology(ctx context.Context, req Request) (any, error) {
	var in searchOntologyInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if in.Query == "" {
		return nil, fmt.Errorf("%w: query is required", ErrInvalidInput)
	}

	snap, err := s.deps.Ontology.Snapshot(ctx, req.Token)
	if err != nil {
		return nil, err
	}
	match := ontology.MatchIntent(snap, ontology.Intent{
		Text:               in.Query,
		Limit:              in.Limit,
		IncludeControlling: in.IncludeControlling,
	})
	return map[string]any{
		"matched_functions":      match.Functions,
		"matched_aspects":        match.Aspects,
		"matched_device_classes": match.DeviceClasses,
		"terms":                  match.Terms,
		// The honest half: words the ontology had no wording for. An LLM that does
		// not see this will assume its vocabulary matched and keep using it.
		"unmatched_terms": match.UnmatchedTerms,
		// The other honest half: how many matches each list above was cut down from.
		// Five functions with no total read as the ontology's whole answer, and a
		// model that believes that stops looking — raise `limit` to see the rest.
		"elided": match.Elided,
		"ontology_size": map[string]int{
			"aspect_nodes":          len(snap.AspectNodes),
			"measuring_functions":   len(snap.MeasuringFunctions),
			"controlling_functions": len(snap.ControllingFunctions),
			"device_classes":        len(snap.DeviceClasses),
			"characteristics":       len(snap.Characteristics),
		},
	}, nil
}

// ---- resolve_semantic_selection (L0) ----

type resolveSelectionInput struct {
	Intent             string   `json:"intent"`
	FunctionIDs        []string `json:"function_ids"`
	AspectIDs          []string `json:"aspect_ids"`
	DeviceClassIDs     []string `json:"device_class_ids"`
	IncludeControlling bool     `json:"include_controlling"`
	DeviceLimit        int64    `json:"device_limit"`
	SkipRanking        bool     `json:"skip_ranking"`
}

func (s *surface) resolveSemanticSelection(ctx context.Context, req Request) (any, error) {
	var in resolveSelectionInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if in.Intent == "" && len(in.FunctionIDs) == 0 && len(in.AspectIDs) == 0 {
		return nil, fmt.Errorf(
			"%w: give an intent, or explicit function_ids or aspect_ids", ErrInvalidInput)
	}

	limit := in.DeviceLimit
	if limit <= 0 || limit > s.deps.DeviceLimit {
		limit = s.deps.DeviceLimit
	}

	req.Progress("resolving", "matching the intent against the ontology, then expanding devices")
	result, err := s.deps.Selection.Resolve(ctx, req.Token, selection.Request{
		Intent:             in.Intent,
		FunctionIDs:        in.FunctionIDs,
		AspectIDs:          in.AspectIDs,
		DeviceClassIDs:     in.DeviceClassIDs,
		IncludeControlling: in.IncludeControlling,
		DeviceLimit:        limit,
		SkipRanking:        in.SkipRanking,
	})
	if err != nil {
		return nil, err
	}

	// The ranked candidates here are the same QuickProfiles quick_profile
	// returns, so they get the same projection. The HTTP and websocket surfaces
	// keep the full result: the selection view renders every field of it.
	return result.ProjectForLLM(s.deps.QuickTokenBudget), nil
}

// ---- list_devices (L0) ----

type listDevicesInput struct {
	Search        string   `json:"search"`
	DeviceTypeIDs []string `json:"device_type_ids"`
	Limit         int64    `json:"limit"`
}

func (s *surface) listDevices(ctx context.Context, req Request) (any, error) {
	var in listDevicesInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 || limit > s.deps.DeviceLimit {
		limit = s.deps.DeviceLimit
	}

	// models.Read, not Execute: this lists device *metadata*, which is what Read
	// governs (§5.1). The tools that offer to read data use Execute.
	result, err := s.deps.Devices.List(req.Token, drmodel.ExtendedDeviceListOptions{
		Search:        in.Search,
		DeviceTypeIds: in.DeviceTypeIDs,
		Limit:         limit,
		Permission:    models.Read,
	})
	if err != nil {
		return nil, err
	}

	// Projected rather than passed through. An ExtendedDevice carries its whole
	// device type, which for a forty-variable meter is thousands of tokens per
	// device — and this tool answers "which devices are there", not "describe
	// each one". get_device_metadata is the tool for the latter.
	listed := make([]map[string]any, 0, len(result.Devices))
	for _, device := range result.Devices {
		listed = append(listed, map[string]any{
			"device_id":        device.Id,
			"name":             devices.DisplayName(device),
			"device_type_id":   device.DeviceTypeId,
			"device_type_name": devices.TypeName(device),
			"connection_state": device.ConnectionState,
			"permissions":      device.Permissions,
		})
	}
	return map[string]any{
		"devices":   listed,
		"total":     result.Total,
		"limit":     limit,
		"note":      "metadata only, no values. Permissions are the platform's, not ODE's.",
		"truncated": result.Total > int64(len(listed)),
	}, nil
}

// ---- get_device_metadata (L0) ----

type getDeviceInput struct {
	DeviceID string `json:"device_id"`
}

func (s *surface) getDeviceMetadata(ctx context.Context, req Request) (any, error) {
	var in getDeviceInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if in.DeviceID == "" {
		return nil, fmt.Errorf("%w: device_id is required", ErrInvalidInput)
	}

	device, err := s.deps.Devices.Get(req.Token, in.DeviceID, models.Read)
	if err != nil {
		return nil, err
	}

	// The addressable series of the device, which is what a model actually needs
	// from "metadata": {device_id, service_id, variable_path} is the addressable
	// unit (D19), and it is buried several levels down in a ContentVariable tree.
	services := []map[string]any{}
	if device.DeviceType != nil {
		for _, service := range device.DeviceType.Services {
			variables := []map[string]any{}
			for _, variable := range profiler.ServiceVariables(service) {
				variables = append(variables, map[string]any{
					"variable_path":     variable.Path,
					"type":              variable.Type,
					"characteristic_id": variable.CharacteristicID,
					"queryable":         variable.Queryable,
					"reason":            variable.Reason,
				})
			}
			services = append(services, map[string]any{
				"service_id":  service.Id,
				"name":        service.Name,
				"interaction": service.Interaction,
				"variables":   variables,
			})
		}
	}

	return map[string]any{
		"device_id":        device.Id,
		"name":             devices.DisplayName(device),
		"device_type_id":   device.DeviceTypeId,
		"device_type_name": devices.TypeName(device),
		"connection_state": device.ConnectionState,
		"permissions":      device.Permissions,
		"shared":           device.Shared,
		"services":         services,
	}, nil
}

// ---- probe_availability (L0) ----

func (s *surface) probeAvailability(ctx context.Context, req Request) (any, error) {
	var in getDeviceInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if in.DeviceID == "" {
		return nil, fmt.Errorf("%w: device_id is required", ErrInvalidInput)
	}

	available, err := s.deps.Timeseries.DataAvailability(ctx, req.Token, in.DeviceID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"device_id":    in.DeviceID,
		"availability": available,
		"reads": map[string]int{
			// Stated rather than implied: this is the property that makes L0
			// substantive, and it is checkable from the answer.
			"values": 0,
		},
	}, nil
}

// ---- probe_export_data (L0) ----

type probeExportInput struct {
	ExportID string `json:"export_id"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// probeExportData is probe_availability's export-side counterpart, and it is a
// separate tool rather than a parameter for a reason worth stating: there is no
// /data-availability for an export. The endpoint is device-scoped, so the window
// a device answers with has to be counted for an export — and counting is what
// also answers the question a developer actually has after creating one, which is
// whether anything is in it.
//
// It stays at L0 on the same footing probe_availability does: what it asks for is
// how many rows carry a value, never what the value is. Every figure it can return
// is metadata about the table.
func (s *surface) probeExportData(ctx context.Context, req Request) (any, error) {
	var in probeExportInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.ExportID) == "" {
		return nil, fmt.Errorf("%w: export_id is required", ErrInvalidInput)
	}
	window, err := parseWindow(in.From, in.To)
	if err != nil {
		return nil, err
	}

	req.Progress("export_definition", "reading the export's column list")
	fill, err := s.deps.Profiler.ExportFill(ctx, req.Token, profiler.ExportFillRequest{
		ExportID: in.ExportID,
		Window:   window,
		Progress: func(phase profiler.Phase) { req.Progress(phase.Stage, phase.Detail) },
	})
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"export": fill,
		"reads": map[string]int{
			// Stated rather than implied, as probe_availability does it. The query
			// this tool makes asks for counts per bucket, so there is no value in
			// anything it can answer with.
			"values": 0,
			"counts": fill.Reads.Values,
			"usage":  fill.Reads.Usage,
		},
		"note": "state \"filled\" means rows exist and every readable column carries values. " +
			"\"partly_filled\" means rows exist and a named column is null in all of them, which is " +
			"an export whose value path resolves against the message root rather than its payload — " +
			"report the column, do not report the export as working. \"empty\" means nothing was " +
			"written in the window. \"unknown\" is not \"empty\": it means the question could not be " +
			"answered, and the two call for opposite advice. Counts are row counts; no value was read.",
	}, nil
}

// ---- estimate_read_cost (L0) ----

type estimateCostInput struct {
	DeviceIDs []string `json:"device_ids"`
	ExportIDs []string `json:"export_ids"`
	From      string   `json:"from"`
	To        string   `json:"to"`
}

// estimateReadCost implements §5.3.3: bytesPerDay from /usage/devices plus the
// availability window yields an approximate read cost before any value is read.
//
// Every derived figure here is order-of-magnitude and says so. The spec is
// explicit that an interval derived from bytes per day must never drive a
// resampling decision, so the estimate is labelled at the point a model reads it
// rather than only in the documentation.
func (s *surface) estimateReadCost(ctx context.Context, req Request) (any, error) {
	var in estimateCostInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if len(in.DeviceIDs) == 0 && len(in.ExportIDs) == 0 {
		return nil, fmt.Errorf("%w: device_ids or export_ids is required", ErrInvalidInput)
	}
	if int64(len(in.DeviceIDs)) > s.deps.DeviceLimit {
		in.DeviceIDs = in.DeviceIDs[:s.deps.DeviceLimit]
	}
	if int64(len(in.ExportIDs)) > s.deps.DeviceLimit {
		in.ExportIDs = in.ExportIDs[:s.deps.DeviceLimit]
	}

	window, err := parseWindow(in.From, in.To)
	if err != nil {
		return nil, err
	}

	// Two endpoints, one answer. They are separate calls upstream because a device
	// and an export are separate tables, and merging them here is what keeps a
	// model from having to know that a series it was offered came from one or the
	// other before it can ask what reading it costs.
	usage := []timeseries.Usage{}
	if len(in.DeviceIDs) > 0 {
		devices, err := s.deps.Timeseries.DeviceUsage(ctx, req.Token, in.DeviceIDs)
		if err != nil {
			return nil, err
		}
		usage = append(usage, devices...)
	}
	missingExports := []string{}
	if len(in.ExportIDs) > 0 {
		exports, err := s.deps.Timeseries.ExportUsage(ctx, req.Token, in.ExportIDs)
		if err != nil {
			return nil, err
		}
		usage = append(usage, exports...)

		// An export with no usage row is the one absence that must not read as an
		// empty export: the usage table is filled per timescale table by a collector,
		// so a young export is simply not in it yet.
		accounted := map[string]bool{}
		for _, entry := range exports {
			accounted[entry.ExportId] = true
		}
		for _, id := range in.ExportIDs {
			if !accounted[id] {
				missingExports = append(missingExports, id)
			}
		}
	}

	estimates := make([]map[string]any, 0, len(usage))
	for _, entry := range usage {
		estimate := map[string]any{
			"bytes":         entry.Bytes,
			"bytes_per_day": entry.BytesPerDay,
			"updated_at":    entry.UpdatedAt,
		}
		if entry.ExportId != "" {
			estimate["export_id"] = entry.ExportId
		} else {
			estimate["device_id"] = entry.DeviceId
		}
		if window.Valid() && entry.BytesPerDay > 0 {
			days := window.SpanDays()
			estimate["window"] = window
			estimate["estimated_window_bytes"] = entry.BytesPerDay * days
		}
		// A row of a timeseries table is a timestamp plus its columns; the constant
		// is a rough per-point size and is what makes this order-of-magnitude only.
		if entry.BytesPerDay > 0 {
			estimate["estimated_points_per_day"] = entry.BytesPerDay / approxBytesPerPoint
			estimate["confidence"] = "uncertain"
			estimate["basis"] = "bytes_per_day"
		}
		estimates = append(estimates, estimate)
	}

	out := map[string]any{
		"estimates": estimates,
		"caveat": "order-of-magnitude only, derived from stored bytes per day across all of a " +
			"device's or export's columns. Never use it to choose a resampling interval; " +
			"profile_series detects the real sampling interval.",
		"reads": map[string]int{"values": 0},
	}
	if len(missingExports) > 0 {
		out["exports_not_accounted"] = missingExports
		out["exports_not_accounted_note"] = "the usage accounting has no row for these exports. It is " +
			"filled per timescale table by a collector, so a young export is simply not in it yet — " +
			"this is not evidence that nothing is stored. probe_export_data counts the rows and answers " +
			"that question."
	}
	return out, nil
}

// approxBytesPerPoint is a rough stored size for one timestamped numeric point,
// used only to turn bytes per day into an order-of-magnitude point count.
const approxBytesPerPoint = 32.0

// ---- quick_profile (L0) ----

type quickProfileInput struct {
	Search             string `json:"search"`
	DeviceLimit        int64  `json:"device_limit"`
	From               string `json:"from"`
	To                 string `json:"to"`
	IncludeUnqueryable bool   `json:"include_unqueryable"`
}

func (s *surface) quickProfile(ctx context.Context, req Request) (any, error) {
	var in quickProfileInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	window, err := parseWindow(in.From, in.To)
	if err != nil {
		return nil, err
	}
	limit := in.DeviceLimit
	if limit <= 0 || limit > s.deps.DeviceLimit {
		limit = s.deps.DeviceLimit
	}

	// Execute, not Read: this offers series to read, and timescale-wrapper checks
	// Execute itself. Listing under Read would offer series the caller cannot read
	// and defer the failure to query time (§5.1).
	req.Progress("devices", "listing devices this account may read data from")
	listed, err := s.deps.Devices.List(req.Token, drmodel.ExtendedDeviceListOptions{
		Search:     in.Search,
		Limit:      limit,
		Permission: models.Execute,
		FullDt:     true,
	})
	if err != nil {
		return nil, err
	}

	// One availability call per device, and they cannot be batched, so this is the
	// part that takes time.
	req.Progress("availability", fmt.Sprintf(
		"probing availability for %d device(s), no values read", len(listed.Devices)))
	result, err := s.deps.Profiler.QuickProfiles(ctx, req.Token, profiler.QuickRequest{
		Devices:            listed.Devices,
		Window:             window,
		IncludeUnqueryable: in.IncludeUnqueryable,
	})
	if err != nil {
		return nil, err
	}

	// D26 again, at L0: the model reads the projection, never the assembled
	// QuickProfile. Depth is not the problem this time, breadth is — eighty
	// candidates unprojected are around forty-eight thousand tokens, and the tool
	// exists to make a shortlist cheap. What was cut is recorded per device.
	view := profiler.ProjectQuick(result, s.deps.QuickTokenBudget)
	view.DevicesListed = len(listed.Devices)
	view.TotalDevices = listed.Total
	return view, nil
}

// ---- profile_series (L1) ----

type profileSeriesInput struct {
	DeviceID  string `json:"device_id"`
	ServiceID string `json:"service_id"`
	// ExportID addresses an export instead of a device's service. Exclusive with
	// the two above: a series lives in one table or the other, and an input naming
	// both is a mistake rather than a narrower request.
	ExportID      string   `json:"export_id"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	GroupTime     string   `json:"group_time"`
	VariablePaths []string `json:"variable_paths"`
}

func (s *surface) profileSeries(ctx context.Context, req Request) (any, error) {
	var in profileSeriesInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	export := strings.TrimSpace(in.ExportID)
	switch {
	case export != "" && (in.DeviceID != "" || in.ServiceID != ""):
		return nil, fmt.Errorf(
			"%w: an export_id addresses an export's own table and a device_id with a service_id "+
				"addresses a device's — give one or the other, not both", ErrInvalidInput)
	case export == "" && (in.DeviceID == "" || in.ServiceID == ""):
		return nil, fmt.Errorf(
			"%w: device_id and service_id are required, or export_id for an export", ErrInvalidInput)
	}
	window, err := parseWindow(in.From, in.To)
	if err != nil {
		return nil, err
	}

	// The profiler's own phases, forwarded verbatim. This is the tool that
	// actually needs it: the passes it runs are where the minutes go.
	progress := func(phase profiler.Phase) {
		req.Progress(phase.Stage, phase.Detail)
	}

	// The scope a variable_paths filter is reported against, and the scope an
	// unmatched filter names in its refusal.
	scope := in.ServiceID

	var result profiler.ProfileResult
	if export != "" {
		// No permission check of ODE's own here, unlike the device branch. An
		// export is not in the device repository, so there is no Execute to ask
		// about: timescale-wrapper verifies the caller's access to the export
		// itself, on the caller's own token, and refuses the read if they may not
		// have it (§5.1).
		scope = export
		result, err = s.deps.Profiler.ProfileExport(ctx, req.Token, profiler.ExportProfileRequest{
			ExportID:       export,
			AnalysisWindow: window,
			GroupTime:      in.GroupTime,
			Progress:       progress,
		})
	} else {
		// Execute: this is about to read the device's data.
		req.Progress("permission", "checking execute permission on the device")
		device, deviceErr := s.deps.Devices.Get(req.Token, in.DeviceID, models.Execute)
		if deviceErr != nil {
			return nil, deviceErr
		}
		result, err = s.deps.Profiler.ProfileService(ctx, req.Token, profiler.ProfileRequest{
			Device:         device,
			ServiceID:      in.ServiceID,
			AnalysisWindow: window,
			GroupTime:      in.GroupTime,
			Progress:       progress,
		})
	}
	if err != nil {
		return nil, err
	}

	// The whole service, or the whole export, was profiled: the read is
	// source-scoped and batched (§5.3.2). A variable_paths filter therefore
	// narrows the answer and not the cost, and it is how a caller asks for a
	// variable the cap below would otherwise have cut.
	matched, err := selectProfiles(result.Profiles, in.VariablePaths, scope)
	if err != nil {
		return nil, err
	}

	// ProfileTokenBudget bounds one projection; this bounds the response. A
	// twenty-variable service would otherwise answer with twenty full profiles,
	// which is twenty times a budget that was written to bound what a model reads.
	shown, notShown := matched, []string{}
	if len(shown) > s.deps.ProfileMaxProfiles {
		for _, resolved := range shown[s.deps.ProfileMaxProfiles:] {
			notShown = append(notShown, resolved.SeriesRef.VariablePath)
		}
		shown = shown[:s.deps.ProfileMaxProfiles]
	}

	// D26: the model reads the projection, never the stored profile. Unbounded
	// arrays are collapsed and the elisions recorded, so the model knows it is
	// looking at a summary and knows which resource holds the rest.
	views := make([]profiler.LLMProfileView, 0, len(shown))
	for _, resolved := range shown {
		views = append(views, profiler.Project(resolved, s.deps.ProfileTokenBudget))
	}

	note := "statistics are computed by ODE's deterministic profiler, not by you. " +
		"A field carrying status \"not_computed\" means it could not be determined — " +
		"it does not mean zero, absent or none."

	response := map[string]any{
		"profiles":        views,
		"analysis_window": result.AnalysisWindow,
		"raw_window":      result.RawWindow,
		"group_time":      result.GroupTime,
		"from_cache":      result.FromCache,
		"reads":           result.Reads,
	}
	if len(notShown) > 0 {
		response["elided"] = []profiler.Elision{{
			Field: "profiles", Total: len(matched), Shown: len(views),
		}}
		response["variables_not_shown"] = notShown
		note += fmt.Sprintf(" This service has %d profiled variables and the response carries %d; "+
			"the values were read once for all of them, so asking again with variable_paths "+
			"costs nothing beyond the profile you get back.", len(matched), len(views))
	}
	response["note"] = note
	return response, nil
}

// selectProfiles applies the variable_paths filter. An empty filter means every
// variable, which is the tool's original behaviour.
//
// A filter that matches nothing is refused rather than answered with an empty
// list: a mistyped path and a service that genuinely has no such variable look
// identical in an empty response, and the paths the service does have are the
// one thing that makes either fixable.
// scope is the service or the export the profiles came from, and appears only in
// the refusal: naming it is what makes a mistyped path fixable.
func selectProfiles(
	profiles []profiler.ResolvedProfile, paths []string, scope string,
) ([]profiler.ResolvedProfile, error) {
	if len(paths) == 0 {
		return profiles, nil
	}

	requested := make(map[string]bool, len(paths))
	for _, path := range paths {
		requested[path] = true
	}

	matched := make([]profiler.ResolvedProfile, 0, len(profiles))
	available := make([]string, 0, len(profiles))
	for _, resolved := range profiles {
		available = append(available, resolved.SeriesRef.VariablePath)
		if requested[resolved.SeriesRef.VariablePath] {
			matched = append(matched, resolved)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("%w: %s has none of the requested variable_paths; it has %v",
			ErrInvalidInput, scope, available)
	}
	return matched, nil
}

// ---- get_sessions (L1) ----

type getSessionsInput struct {
	ProfileID string `json:"profile_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Limit     int    `json:"limit"`
	Cursor    string `json:"cursor"`
}

func (s *surface) getSessions(ctx context.Context, req Request) (any, error) {
	var in getSessionsInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if in.ProfileID == "" {
		return nil, fmt.Errorf("%w: profile_id is required", ErrInvalidInput)
	}

	// The profile is looked up first so an unknown id is a clear refusal rather
	// than an empty page, which a model would read as "this series has no
	// sessions" — a different and wrong conclusion.
	if _, found := s.deps.Profiler.Profile(in.ProfileID); !found {
		return nil, fmt.Errorf("%w: no profile %q; compute one with profile_series first",
			ErrInvalidInput, in.ProfileID)
	}

	from, err := parseTime(in.From, "from")
	if err != nil {
		return nil, err
	}
	to, err := parseTime(in.To, "to")
	if err != nil {
		return nil, err
	}

	page, err := s.deps.Profiler.Store().Sessions(in.ProfileID, profiler.SessionQuery{
		From: from, To: to, Limit: in.Limit, Cursor: in.Cursor,
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// ---- propose_related_sets (L0) ----

type proposeRelatedSetsInput struct {
	AspectID           string `json:"aspect_id"`
	IncludeDescendants bool   `json:"include_descendants"`
	Limit              int64  `json:"limit"`
}

// proposeRelatedSets is the aspect-scoped half of §5.5, and it reads no values.
//
// It sits at L0 for a reason worth stating: the hardest part of a multi-device
// pattern is knowing which devices to look at, and the ontology answers that on its
// own. A model at the default tier can therefore get all the way to "the oven and
// the kitchen lights are the pair to examine" before any value is read at all — the
// same property that makes semantic selection substantive at L0 (§3.2).
func (s *surface) proposeRelatedSets(ctx context.Context, req Request) (any, error) {
	var in proposeRelatedSetsInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if in.AspectID == "" {
		return nil, fmt.Errorf("%w: aspect_id is required; search_ontology lists the aspect nodes",
			ErrInvalidInput)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = s.deps.DeviceLimit
	}
	req.Progress("propose", "resolving the aspect subtree to devices")
	proposal, err := s.deps.Relations.ProposeRelatedSets(ctx, req.Token, relations.ProposalRequest{
		AspectID:           in.AspectID,
		IncludeDescendants: in.IncludeDescendants,
		DeviceLimit:        limit,
		MaxMembers:         s.deps.Relations.MaxMembers(),
	})
	if err != nil {
		return nil, err
	}

	note := "these sets come from the ontology and no value was read, which the reads block " +
		"shows. Pass one set's members to relate_series to find the conditional patterns in it. " +
		"An empty sets list is not evidence that the devices are unrelated — read notes for why."
	if len(proposal.Sets) == 0 {
		note = "no set could be proposed, and notes says why. Do not conclude that these devices " +
			"have no relationship: the cause is more often an ontology gap or a permission than an " +
			"absence of pattern."
	}
	return map[string]any{
		"aspect_id":           proposal.AspectID,
		"aspect_name":         proposal.AspectName,
		"include_descendants": proposal.IncludeDescendants,
		"sets":                proposal.Sets,
		"candidate_devices":   proposal.CandidateDevices,
		"ontology_gaps":       proposal.OntologyGaps,
		"reads":               proposal.Reads,
		"notes":               proposal.Notes,
		"note":                note,
	}, nil
}

// ---- relate_series (L1) ----

type relateSeriesInput struct {
	Series         []relateSeriesMember `json:"series"`
	From           string               `json:"from"`
	To             string               `json:"to"`
	CandidateSetID string               `json:"candidate_set_id"`
	MinConfidence  float64              `json:"min_confidence"`
	MinLift        float64              `json:"min_lift"`
	HourBuckets    int                  `json:"hour_buckets"`
}

type relateSeriesMember struct {
	DeviceID     string `json:"device_id"`
	ServiceID    string `json:"service_id"`
	VariablePath string `json:"variable_path"`
	Label        string `json:"label"`
}

// relateSeries is the computed half of §5.5.
//
// L1 rather than L2, and the distinction is exact: the pass reads values, and what
// comes back is contingency counts, ratios and bucket durations. Not one value of
// any series appears in the projection — a state is a comparison against a threshold
// the profiler already published, so the model learns that the oven was on without
// learning what it drew.
//
// Only the three thresholds a model has a reason to move are exposed. min_support and
// the exception drop stay at their defaults deliberately: a model lowering the support
// floor to find something to say is the failure mode this whole surface is shaped
// against, and a developer who needs to move them has the HTTP route.
func (s *surface) relateSeries(ctx context.Context, req Request) (any, error) {
	var in relateSeriesInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if len(in.Series) < 2 {
		return nil, fmt.Errorf("%w: relate_series needs at least two series; a conditional pattern "+
			"is a statement about a pair", ErrInvalidInput)
	}
	window, err := parseWindow(in.From, in.To)
	if err != nil {
		return nil, err
	}

	members := make([]relations.SeriesMember, 0, len(in.Series))
	for i, member := range in.Series {
		if member.DeviceID == "" || member.ServiceID == "" || member.VariablePath == "" {
			return nil, fmt.Errorf("%w: series[%d] needs a device_id, a service_id and a variable_path",
				ErrInvalidInput, i)
		}
		members = append(members, relations.SeriesMember{
			Ref: profiler.SeriesRef{
				DeviceID:     member.DeviceID,
				ServiceID:    member.ServiceID,
				VariablePath: member.VariablePath,
			},
			Label: member.Label,
		})
	}

	profile, err := s.deps.Relations.Relate(ctx, req.Token, relations.Request{
		Members:        members,
		Window:         window,
		CandidateSetID: in.CandidateSetID,
		Params: relations.RuleParams{
			MinConfidence: in.MinConfidence,
			MinLift:       in.MinLift,
			HourBuckets:   in.HourBuckets,
		},
		// The pass profiles every participating service before it aligns them, so this
		// is the slowest tool on the surface. Forwarding its phases is what lets the
		// developer watching the chat see that it is alive.
		Progress: func(phase relations.Phase) {
			req.Progress(phase.Stage, phase.Detail)
		},
	})
	if err != nil {
		return nil, err
	}

	view := relations.Project(profile, s.deps.RelationMaxRules, s.deps.RelationTokenBudget)
	note := "every rule here is a candidate. It is not an anomaly definition, nothing downstream " +
		"reads it, and you have no tool to confirm one — only the developer can, and the numbers " +
		"are what you should put to them. `confidence` is P(consequent | antecedent) and `lift` is " +
		"how much that exceeds the consequent's own base rate; a high confidence with a lift near 1 " +
		"means the consequent is simply usually true. A member whose state block says usable false " +
		"took part in no rule, and the reason there is what to fix."
	if len(view.CandidateRules) == 0 {
		note = "no pattern cleared the thresholds. That is a result, not a failure, and it is not " +
			"evidence that the devices are unrelated: check each member's state block first, because " +
			"a member that yielded no state series could not contribute to any rule."
	}
	if decided := decidedRules(view.CandidateRules); decided > 0 {
		note += fmt.Sprintf(" %d of these rules already carry a developer decision — read it before "+
			"proposing the rule again.", decided)
	}

	return map[string]any{
		"relation":         view,
		"candidate_set_id": profile.CandidateSetID,
		"note":             note,
	}, nil
}

// decidedRules counts the rules a developer has already ruled on, so the note can
// say so rather than leaving a model to notice.
func decidedRules(rules []relations.CandidateRule) int {
	count := 0
	for _, rule := range rules {
		if rule.Decision != nil {
			count++
		}
	}
	return count
}

// ---- preview_series (L2) ----

type previewSeriesInput struct {
	DeviceID  string `json:"device_id"`
	ServiceID string `json:"service_id"`
	// ExportID addresses an export instead, and the variable path is then the
	// export's column name rather than a message path.
	ExportID     string `json:"export_id"`
	VariablePath string `json:"variable_path"`
	From         string `json:"from"`
	To           string `json:"to"`
	GroupTime    string `json:"group_time"`
	GroupType    string `json:"group_type"`
	MaxPoints    int    `json:"max_points"`
}

// previewSeries is the only tool that returns actual values, which is why it sits
// at L2 and why it is aggregated rather than raw.
//
// The point cap is not a performance guard. §4 states that the LLM never computes
// statistics from raw data, and a preview large enough to compute statistics from
// would let a model do exactly that while nominally respecting the tier. So the
// bucket is widened until the window fits the cap, and the response says what it
// did.
func (s *surface) previewSeries(ctx context.Context, req Request) (any, error) {
	var in previewSeriesInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	export := strings.TrimSpace(in.ExportID)
	switch {
	case in.VariablePath == "":
		return nil, fmt.Errorf("%w: variable_path is required — for an export it is the column name",
			ErrInvalidInput)
	case export != "" && (in.DeviceID != "" || in.ServiceID != ""):
		return nil, fmt.Errorf(
			"%w: give export_id, or device_id with service_id, not both", ErrInvalidInput)
	case export == "" && (in.DeviceID == "" || in.ServiceID == ""):
		return nil, fmt.Errorf(
			"%w: device_id, service_id and variable_path are required — a device series is addressed "+
				"by all three; an export series by export_id and the column name",
			ErrInvalidInput)
	}

	window, err := parseWindow(in.From, in.To)
	if err != nil {
		return nil, err
	}
	if !window.Valid() {
		window = profiler.Window{From: time.Now().UTC().AddDate(0, 0, -7), To: time.Now().UTC()}
	}

	maxPoints := in.MaxPoints
	if maxPoints <= 0 || maxPoints > s.deps.PreviewMaxPoints {
		maxPoints = s.deps.PreviewMaxPoints
	}

	groupType := in.GroupType
	if groupType == "" {
		groupType = timeseries.GroupMean
	}
	if !timeseries.ValidGroupType(groupType) {
		return nil, fmt.Errorf("%w: group_type %q is not accepted by the platform", ErrInvalidInput, groupType)
	}

	groupTime, widened := previewBucket(in.GroupTime, window, maxPoints)

	bucket := groupTime
	aggregate := groupType
	ref := profiler.SeriesRef{
		DeviceID: in.DeviceID, ServiceID: in.ServiceID, VariablePath: in.VariablePath,
	}
	element := timeseries.QueryElement{
		Columns:   []timeseries.QueryColumn{{Name: in.VariablePath, GroupType: &aggregate}},
		GroupTime: &bucket,
		Time: &timeseries.QueryTime{
			Start: stringPtr(window.From.UTC().Format(time.RFC3339)),
			End:   stringPtr(window.To.UTC().Format(time.RFC3339)),
		},
	}
	if export != "" {
		ref = profiler.SeriesRef{ExportID: export, VariablePath: in.VariablePath}
		element.ExportId = &export
	} else {
		deviceID, serviceID := in.DeviceID, in.ServiceID
		element.DeviceId, element.ServiceId = &deviceID, &serviceID
	}

	results, err := s.deps.Timeseries.Query(ctx, req.Token, []timeseries.QueryElement{element},
		timeseries.QueryOptions{})
	if err != nil {
		return nil, err
	}
	sets, err := timeseries.DecodeResults([]timeseries.QueryElement{element}, results, "")
	if err != nil {
		return nil, err
	}

	points := []map[string]any{}
	dropped := 0
	if len(sets) > 0 {
		column, found := sets[0].Column(in.VariablePath)
		if found {
			times, values, skipped := column.Numeric()
			dropped = skipped
			for i := range times {
				if len(points) >= maxPoints {
					break
				}
				points = append(points, map[string]any{
					"t": times[i].UTC().Format(time.RFC3339),
					"v": values[i],
				})
			}
		}
	}

	out := map[string]any{
		"series_ref":  ref,
		"window":      window,
		"group_time":  groupTime,
		"group_type":  groupType,
		"points":      points,
		"point_count": len(points),
		"max_points":  maxPoints,
		"note": "a downsampled preview for judging shape. Do not compute statistics from it; " +
			"profile_series computes them deterministically over the full window.",
	}
	if widened {
		out["bucket_widened"] = true
		out["bucket_note"] = fmt.Sprintf(
			"group_time was widened to %s so the window fits %d points", groupTime, maxPoints)
	}
	if dropped > 0 {
		out["non_numeric_dropped"] = dropped
	}
	return out, nil
}

// ---- propose_data_selection (L0, confirmed) ----

// ProposedSelection is a data selection awaiting developer confirmation (§5.2's
// last step, D11).
type ProposedSelection struct {
	Rationale string           `json:"rationale"`
	Series    []ProposedSeries `json:"series"`
	// ProposedAt is stamped by ODE rather than taken from the model.
	ProposedAt time.Time `json:"proposed_at"`
}

type ProposedSeries struct {
	DeviceID     string `json:"device_id"`
	ServiceID    string `json:"service_id"`
	VariablePath string `json:"variable_path"`
	Role         string `json:"role,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func (s *surface) proposeDataSelection(ctx context.Context, req Request) (any, error) {
	var in ProposedSelection
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if len(in.Series) == 0 {
		return nil, fmt.Errorf("%w: propose at least one series", ErrInvalidInput)
	}
	for i, series := range in.Series {
		if series.DeviceID == "" || series.ServiceID == "" || series.VariablePath == "" {
			return nil, fmt.Errorf(
				"%w: series %d is not fully addressed; a series is {device_id, service_id, variable_path}",
				ErrInvalidInput, i)
		}
	}
	in.ProposedAt = time.Now().UTC()

	if err := s.deps.SelectionSink.PutProposedSelection(ctx, req.SessionID, in); err != nil {
		return nil, err
	}
	return map[string]any{
		"accepted":     true,
		"series_count": len(in.Series),
		"note":         "the developer confirmed this selection; it is recorded on the session.",
	}, nil
}

// ---- render_chart (L1) ----

type renderChartInput struct {
	Title       string                  `json:"title"`
	Caption     string                  `json:"caption"`
	Series      []renderChartSeries     `json:"series"`
	Annotations []renderChartAnnotation `json:"annotations"`
	Markers     []renderChartMarker     `json:"markers"`
	YAxis       struct {
		Unit       string `json:"unit"`
		UnitSource string `json:"unit_source"`
	} `json:"y_axis"`
	Window struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"window"`
	GroupTime string `json:"group_time"`
}

type renderChartSeries struct {
	DeviceID     string `json:"device_id"`
	ServiceID    string `json:"service_id"`
	VariablePath string `json:"variable_path"`
	Label        string `json:"label"`
	Transform    string `json:"transform"`
	ProfileID    string `json:"profile_id"`
}

type renderChartAnnotation struct {
	Type        string `json:"type"`
	From        string `json:"from"`
	To          string `json:"to"`
	Label       string `json:"label"`
	Severity    string `json:"severity"`
	Source      string `json:"source"`
	Confirmable bool   `json:"confirmable"`
	FieldPath   string `json:"field_path"`
	SeriesIndex *int   `json:"series_index"`
}

type renderChartMarker struct {
	At          string `json:"at"`
	Label       string `json:"label"`
	Source      string `json:"source"`
	SeriesIndex *int   `json:"series_index"`
}

// renderChart emits a §5.9 chart specification for the exploration pane.
//
// The result deliberately carries no values, and that is what makes the tier
// assignment coherent rather than lax. render_chart is L1 because a chart shows
// values and the developer will see them — but the model gets the chart id, the
// resolved units and the notes, and the browser fetches the data itself with the
// developer's token. So a model at L1 can demonstrate a selection visually without
// a single value entering its context, which is the property M5 is accepted on.
//
// values_read is reported for the same reason QuickProfile reports its read counts:
// the claim is then checkable from the answer rather than from a promise.
func (s *surface) renderChart(ctx context.Context, req Request) (any, error) {
	var in renderChartInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if len(in.Series) == 0 {
		return nil, fmt.Errorf("%w: a chart needs at least one series", ErrInvalidInput)
	}

	window, err := parseWindow(in.Window.From, in.Window.To)
	if err != nil {
		return nil, err
	}

	series := make([]charts.SeriesSpec, 0, len(in.Series))
	for i, entry := range in.Series {
		if entry.DeviceID == "" || entry.ServiceID == "" || entry.VariablePath == "" {
			return nil, fmt.Errorf(
				"%w: series %d is not fully addressed; a series is {device_id, service_id, variable_path}",
				ErrInvalidInput, i)
		}
		series = append(series, charts.SeriesSpec{
			Ref: profiler.SeriesRef{
				DeviceID: entry.DeviceID, ServiceID: entry.ServiceID, VariablePath: entry.VariablePath,
			},
			Label:     entry.Label,
			Transform: entry.Transform,
			ProfileID: entry.ProfileID,
		})
	}

	annotations := make([]charts.Annotation, 0, len(in.Annotations))
	for i, entry := range in.Annotations {
		from, err := parseTime(entry.From, fmt.Sprintf("annotations[%d].from", i))
		if err != nil {
			return nil, err
		}
		to, err := parseTime(entry.To, fmt.Sprintf("annotations[%d].to", i))
		if err != nil {
			return nil, err
		}
		annotations = append(annotations, charts.Annotation{
			Type: entry.Type, From: from, To: to, Label: entry.Label, Severity: entry.Severity,
			Source: entry.Source, Confirmable: entry.Confirmable, FieldPath: entry.FieldPath,
			SeriesIndex: entry.SeriesIndex,
		})
	}

	markers := make([]charts.Marker, 0, len(in.Markers))
	for i, entry := range in.Markers {
		at, err := parseTime(entry.At, fmt.Sprintf("markers[%d].at", i))
		if err != nil {
			return nil, err
		}
		markers = append(markers, charts.Marker{
			At: at, Label: entry.Label, Source: entry.Source, SeriesIndex: entry.SeriesIndex,
		})
	}

	created, err := s.deps.Charts.Create(ctx, req.Token, charts.CreateRequest{
		UserSub:   req.UserSub,
		SessionID: req.SessionID,
		// Stamped here, not read from the input: an author cannot claim to be the
		// developer, and the pane shows who put each element on screen.
		Author:      charts.AuthorLLM,
		Title:       in.Title,
		Caption:     in.Caption,
		Series:      series,
		Annotations: annotations,
		Markers:     markers,
		YAxis:       charts.YAxis{Unit: in.YAxis.Unit, UnitSource: in.YAxis.UnitSource},
		Window:      window,
		GroupTime:   in.GroupTime,
	})
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"chart_id":    created.Spec.ChartID,
		"title":       created.Spec.Title,
		"window":      created.Spec.Window,
		"group_time":  created.Spec.GroupTime,
		"series":      created.Series,
		"y_axis":      created.Axis,
		"annotations": len(created.Spec.Annotations),
		"markers":     len(created.Spec.Markers),
		"values_read": 0,
		"note": "the specification is stored and the developer's pane draws it from their own read. " +
			"You do not receive the values. If a unit is reported as not settled, ask the developer to " +
			"confirm it rather than assuming one.",
	}
	if len(created.Notes) > 0 {
		out["notes"] = created.Notes
	}
	return out, nil
}

// ---- helpers ----

func decode(input json.RawMessage, into any) error {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(input, into); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	return nil
}

// parseWindow reads an optional RFC3339 range. Both empty is legal and means "the
// service's own default lookback", which every caller of this treats as such.
func parseWindow(from, to string) (profiler.Window, error) {
	start, err := parseTime(from, "from")
	if err != nil {
		return profiler.Window{}, err
	}
	end, err := parseTime(to, "to")
	if err != nil {
		return profiler.Window{}, err
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return profiler.Window{}, fmt.Errorf("%w: `to` must be after `from`", ErrInvalidInput)
	}
	return profiler.Window{From: start, To: end}, nil
}

func parseTime(value, field string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be RFC3339, got %q", ErrInvalidInput, field, value)
	}
	return parsed.UTC(), nil
}

// previewBucket picks the aggregation bucket for a preview.
//
// The widening itself is timeseries.Bucket, shared with the exploration pane's
// chart reads (§5.9): both have to answer "how coarse must this be to fit" the
// same way, or the same series would be shown at two different resolutions.
func previewBucket(requested string, window profiler.Window, maxPoints int) (bucket string, widened bool) {
	return timeseries.Bucket(requested, window.Duration(), maxPoints)
}

func stringPtr(s string) *string { return &s }

// isNil answers whether an interface value holds nothing, including the typed-nil
// case that an ordinary `== nil` misses. pkg.rankerOrNil documents the same
// footgun; here the check has to be reflective because Deps holds interfaces the
// caller may have filled from a typed nil pointer.
func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}
