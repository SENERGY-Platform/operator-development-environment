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

// Package devices reads devices on behalf of the calling user (SPEC D5, §3.1
// step 3). Nothing here is cached: device visibility is per-user, so a cache
// shared across users would be an authorisation bug rather than an
// optimisation.
package devices

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

// Client is the slice of device-repository this package needs. Both methods
// take the caller's token: the platform's own per-user authorisation is the
// single source of truth, and ODE never substitutes a service account.
type Client interface {
	ListExtendedDevices(token string, options model.ExtendedDeviceListOptions) ([]models.ExtendedDevice, int64, error, int)
	ReadExtendedDevice(id string, token string, action model.AuthAction, fullDt bool) (models.ExtendedDevice, error, int)
}

const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

var ErrInvalidOption = errors.New("invalid list option")

type Service struct {
	client Client
}

func New(c Client) *Service { return &Service{client: c} }

type ListResult struct {
	Devices []models.ExtendedDevice `json:"devices"`
	Total   int64                   `json:"total"`
	Limit   int64                   `json:"limit"`
	Offset  int64                   `json:"offset"`
}

// List returns the devices the token's owner may read.
func (s *Service) List(token string, options model.ExtendedDeviceListOptions) (ListResult, error) {
	found, total, err, code := s.client.ListExtendedDevices(token, options)
	if err != nil {
		return ListResult{}, &UpstreamError{Code: code, Err: err}
	}
	if found == nil {
		found = []models.ExtendedDevice{}
	}
	return ListResult{Devices: found, Total: total, Limit: options.Limit, Offset: options.Offset}, nil
}

// Get reads one device. action is the permission being claimed; M0 reads
// metadata, so callers pass model.READ. Reading a device's *data* requires
// Execute rather than Read (§5.1), which matters from M1a onward.
func (s *Service) Get(token string, id string, action model.AuthAction) (models.ExtendedDevice, error) {
	device, err, code := s.client.ReadExtendedDevice(id, token, action, true)
	if err != nil {
		return models.ExtendedDevice{}, &UpstreamError{Code: code, Err: err}
	}
	return device, nil
}

// DisplayName is what a human should be shown for a device.
//
// The platform keeps two names: `name` is the device's own, and `display_name` is
// computed per request and is what the platform's own UIs show — an
// attribute-driven label a user chose, where there is one. Preferring it keeps ODE
// naming a device the way the rest of the platform does.
//
// It never falls back to the id. An id is what a series is keyed on, not what a
// developer picking between forty candidates should have to read; a caller that
// wants one takes it from the SeriesRef.
func DisplayName(device models.ExtendedDevice) string {
	if device.DisplayName != "" {
		return device.DisplayName
	}
	return device.Name
}

// TypeName is the device type's name, which is what makes a device
// distinguishable when several share a display name — "Meter 3" says much less
// than "Meter 3 (SmartMeter Modbus)".
//
// device_type_name is computed by the repository on request and is not always
// populated; the device type itself arrives whenever ODE reads with fulldt, which
// every candidate listing does. Empty means neither was available, and the caller
// shows the id instead rather than an empty gap.
func TypeName(device models.ExtendedDevice) string {
	if device.DeviceTypeName != "" {
		return device.DeviceTypeName
	}
	if device.DeviceType != nil {
		return device.DeviceType.Name
	}
	return ""
}

// ParseListOptions maps SPA query parameters onto the device-repository's
// option struct. It is a pure function so the mapping can be tested without a
// server or a platform.
//
// Unset filters must stay nil rather than becoming empty slices: the device
// repository treats `Ids == []string{}` as "match nothing", so an empty filter
// and an absent one mean opposite things.
func ParseListOptions(q url.Values) (model.ExtendedDeviceListOptions, error) {
	opts := model.ExtendedDeviceListOptions{
		Limit:      DefaultLimit,
		Permission: models.Read,
		FullDt:     false,
	}

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		limit, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || limit < 0 {
			return opts, fmt.Errorf("%w: limit must be a non-negative integer, got %q", ErrInvalidOption, raw)
		}
		if limit > MaxLimit {
			return opts, fmt.Errorf("%w: limit must not exceed %d, got %d", ErrInvalidOption, MaxLimit, limit)
		}
		opts.Limit = limit
	}

	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		offset, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			return opts, fmt.Errorf("%w: offset must be a non-negative integer, got %q", ErrInvalidOption, raw)
		}
		opts.Offset = offset
	}

	if search := strings.TrimSpace(q.Get("search")); search != "" {
		opts.Search = search
	}
	if sortBy := strings.TrimSpace(q.Get("sort")); sortBy != "" {
		opts.SortBy = sortBy
	}
	if ids := splitCSV(q.Get("ids")); ids != nil {
		opts.Ids = ids
	}
	if dtIds := splitCSV(q.Get("device_type_ids")); dtIds != nil {
		opts.DeviceTypeIds = dtIds
	}
	if raw := strings.TrimSpace(q.Get("connection_state")); raw != "" {
		state, err := parseConnectionState(raw)
		if err != nil {
			return opts, err
		}
		opts.ConnectionState = &state
	}
	if raw := strings.TrimSpace(q.Get("full_device_type")); raw != "" {
		full, err := strconv.ParseBool(raw)
		if err != nil {
			return opts, fmt.Errorf("%w: full_device_type must be a boolean, got %q", ErrInvalidOption, raw)
		}
		opts.FullDt = full
	}

	return opts, nil
}

// parseConnectionState accepts only online and offline. models.ConnectionStateUnknown
// is the empty string, which the device repository reads as "no filter", so
// there is no way to ask for unknown-state devices specifically — offering the
// value would silently return everything instead.
func parseConnectionState(raw string) (models.ConnectionState, error) {
	switch raw {
	case models.ConnectionStateOnline, models.ConnectionStateOffline:
		return raw, nil
	default:
		return "", fmt.Errorf("%w: connection_state must be %q or %q, got %q",
			ErrInvalidOption, models.ConnectionStateOnline, models.ConnectionStateOffline, raw)
	}
}

// splitCSV returns nil for an absent parameter and drops empty members, so
// `ids=` cannot accidentally become the "match nothing" filter.
func splitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type UpstreamError struct {
	Code int
	Err  error
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("devices: device-repository returned %d: %v", e.Code, e.Err)
}
func (e *UpstreamError) Unwrap() error { return e.Err }
