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

package api

import (
	"context"
	"fmt"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
)

// The operations the profiler and selection surfaces expose, as functions rather
// than handlers.
//
// Both HTTP and the WebSocket call these, which is the point: the WebSocket
// exists because a profile read outlives an HTTP timeout, not because it should
// behave differently. Two code paths would drift, and the one nobody demos would
// be the one that rots.

// QuickProfileInput is a candidate listing.
type QuickProfileInput struct {
	Search string
	// Limit is how many *devices* to expand. Availability is one call per device
	// and cannot be batched, so this is what decides the wall clock. Zero means
	// the default.
	Limit              int64
	Window             profiler.Window
	IncludeUnqueryable bool
}

// QuickProfileOutput mirrors what the HTTP route returns, so the two surfaces
// hand back the same document.
type QuickProfileOutput struct {
	Candidates     []profiler.QuickProfile  `json:"candidates"`
	Skipped        []profiler.SkippedDevice `json:"skipped"`
	Reads          profiler.ReadCounts      `json:"reads"`
	CoverageWindow profiler.Window          `json:"coverage_window"`
	DevicesListed  int                      `json:"devices_listed"`
	TotalDevices   int64                    `json:"total_devices"`
	DeviceLimit    int64                    `json:"device_limit"`
}

func runQuickProfiles(
	ctx context.Context,
	token string,
	deviceService *devices.Service,
	prof *profiler.Profiler,
	input QuickProfileInput,
) (QuickProfileOutput, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = defaultDeviceLimit
	}
	if limit > maxDeviceLimit {
		limit = maxDeviceLimit
	}

	options := drmodel.ExtendedDeviceListOptions{
		Limit: limit,
		// models.Read governs device metadata; models.Execute governs reading a
		// device's data (§5.1). Listing candidates under Read would offer series
		// ODE cannot read, and the failure would surface at query time instead.
		Permission: models.Execute,
		// The variable enumeration is a walk over the service outputs, so the
		// device type has to come with the device.
		FullDt: true,
	}
	if input.Search != "" {
		options.Search = input.Search
	}

	listed, err := deviceService.List(token, options)
	if err != nil {
		return QuickProfileOutput{}, err
	}

	result, err := prof.QuickProfiles(ctx, token, profiler.QuickRequest{
		Devices:            listed.Devices,
		Window:             input.Window,
		IncludeUnqueryable: input.IncludeUnqueryable,
	})
	if err != nil {
		return QuickProfileOutput{}, err
	}

	return QuickProfileOutput{
		Candidates:     result.Candidates,
		Skipped:        result.Skipped,
		Reads:          result.Reads,
		CoverageWindow: result.Window,
		DevicesListed:  len(listed.Devices),
		TotalDevices:   listed.Total,
		DeviceLimit:    limit,
	}, nil
}

// SelectionInput is one semantic selection (§5.2).
type SelectionInput struct {
	Intent         string
	FunctionIDs    []string
	AspectIDs      []string
	DeviceClassIDs []string

	Interaction        models.Interaction
	IncludeControlling bool

	MatchLimit int
	MinScore   float64

	// Limit is how many *devices* the resolution expands, with the same meaning
	// and the same ceiling as a candidate listing: availability is one call per
	// device and cannot be batched. Zero means the resolver's default.
	Limit       int64
	Window      profiler.Window
	SkipRanking bool
}

// runSelection clamps the device limit and hands over to pkg/selection.
//
// The clamp lives here rather than in the resolver so that "how many devices an
// ODE request may expand" has one answer across both operations that expand
// devices — a resolution that quietly listed two hundred would take twenty times
// as long as the candidate listing beside it for no stated reason.
func runSelection(
	ctx context.Context,
	token string,
	resolver *selection.Resolver,
	input SelectionInput,
) (selection.Result, error) {
	if resolver == nil {
		return selection.Result{}, fmt.Errorf("%w: semantic selection is not configured",
			selection.ErrInvalidRequest)
	}

	limit := input.Limit
	if limit <= 0 {
		limit = defaultDeviceLimit
	}
	if limit > maxDeviceLimit {
		limit = maxDeviceLimit
	}

	return resolver.Resolve(ctx, token, selection.Request{
		Intent:             input.Intent,
		FunctionIDs:        input.FunctionIDs,
		AspectIDs:          input.AspectIDs,
		DeviceClassIDs:     input.DeviceClassIDs,
		Interaction:        input.Interaction,
		IncludeControlling: input.IncludeControlling,
		MatchLimit:         input.MatchLimit,
		MinScore:           input.MinScore,
		DeviceLimit:        limit,
		Window:             input.Window,
		SkipRanking:        input.SkipRanking,
	})
}

// ProfileInput is one source-scoped profile computation: a device's service, or
// an export.
type ProfileInput struct {
	DeviceID  string
	ServiceID string
	// ExportID profiles an export's own table instead. Exclusive with the two
	// above — a series lives in one table or the other — and the variable paths of
	// the resulting profiles are the export's column names.
	ExportID       string
	AnalysisWindow profiler.Window
	RawWindow      profiler.Window
	GroupTime      string
	SessionParams  *profiler.SessionParams
}

func runProfile(
	ctx context.Context,
	token string,
	deviceService *devices.Service,
	prof *profiler.Profiler,
	input ProfileInput,
) (profiler.ProfileResult, error) {
	if input.ExportID != "" {
		if input.DeviceID != "" || input.ServiceID != "" {
			return profiler.ProfileResult{}, fmt.Errorf(
				"%w: export_id addresses an export's own table and device_id with service_id "+
					"addresses a device's; give one or the other",
				profiler.ErrInvalidRequest)
		}
		// No device permission check, because there is no device: an export is not in
		// the device repository at all. timescale-wrapper verifies the caller's
		// access to the export on the caller's own token and refuses the read
		// otherwise, which is the same on-behalf-of chain the device path relies on
		// beyond the Execute check (§5.1).
		return prof.ProfileExport(ctx, token, profiler.ExportProfileRequest{
			ExportID:       input.ExportID,
			AnalysisWindow: input.AnalysisWindow,
			RawWindow:      input.RawWindow,
			GroupTime:      input.GroupTime,
			SessionParams:  input.SessionParams,
		})
	}
	if input.DeviceID == "" || input.ServiceID == "" {
		return profiler.ProfileResult{}, fmt.Errorf(
			"%w: device_id and service_id are required, or export_id for an export",
			profiler.ErrInvalidRequest)
	}

	// Execute, not Read: this request is about to read the device's data.
	device, err := deviceService.Get(token, input.DeviceID, models.Execute)
	if err != nil {
		return profiler.ProfileResult{}, err
	}

	return prof.ProfileService(ctx, token, profiler.ProfileRequest{
		Device:         device,
		ServiceID:      input.ServiceID,
		AnalysisWindow: input.AnalysisWindow,
		RawWindow:      input.RawWindow,
		GroupTime:      input.GroupTime,
		SessionParams:  input.SessionParams,
	})
}

// RelationInput is one relational pass (§5.5, M6).
type RelationInput struct {
	Members     []relations.SeriesMember
	Window      profiler.Window
	GridSeconds float64
	Params      relations.RuleParams
	// Conditioning is a pointer so that a caller who names nothing gets both
	// dimensions rather than neither: a zero Conditioning is "condition on nothing",
	// which is a legitimate request and never the default.
	Conditioning   *relations.Conditioning
	CandidateSetID string

	// Progress is forwarded to the service. The WebSocket sets it; the HTTP route
	// leaves it nil, because a single response has nowhere to put a phase.
	Progress func(relations.Phase)
}

// runRelation is the operation both surfaces call.
//
// Thin, unlike runQuickProfiles and runSelection: there is no device limit to clamp
// here, because the members are named explicitly and the service caps how many of
// them a pass may carry. It exists so the two transports cannot drift, which is the
// same reason the three above do.
func runRelation(
	ctx context.Context,
	token string,
	service *relations.Service,
	input RelationInput,
) (relations.RelationProfile, error) {
	if service == nil {
		return relations.RelationProfile{}, fmt.Errorf("%w: relational profiling is not configured",
			relations.ErrInvalidRequest)
	}
	return service.Relate(ctx, token, relations.Request{
		Members:        input.Members,
		Window:         input.Window,
		GridSeconds:    input.GridSeconds,
		Params:         input.Params,
		Conditioning:   input.Conditioning,
		CandidateSetID: input.CandidateSetID,
		Progress:       input.Progress,
	})
}
