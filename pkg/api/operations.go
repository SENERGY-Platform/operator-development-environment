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
)

// The two operations the profiler surface exposes, as functions rather than
// handlers.
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

// ProfileInput is one service-scoped profile computation.
type ProfileInput struct {
	DeviceID       string
	ServiceID      string
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
	if input.DeviceID == "" || input.ServiceID == "" {
		return profiler.ProfileResult{}, fmt.Errorf("%w: device_id and service_id are required",
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
