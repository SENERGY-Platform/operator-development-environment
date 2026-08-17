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
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// TimeseriesReader is the part of the timeseries client the thin passthrough
// routes need. An interface rather than the concrete client so the suite can
// serve fixtures without a platform.
type TimeseriesReader interface {
	DataAvailability(ctx context.Context, token string, deviceID string) ([]timeseries.Availability, error)
	DeviceUsage(ctx context.Context, token string, deviceIDs []string) ([]timeseries.Usage, error)
}

const (
	// defaultDeviceLimit is how many devices a candidate listing expands by
	// default.
	//
	// Ten, because this is the knob that decides how long the listing takes:
	// /data-availability is per device and cannot be batched, so the wall clock is
	// devices divided by the concurrency limit. Ten devices is two rounds and a
	// list a developer can actually read; a hundred is fifty seconds of waiting to
	// scroll past ninety of them. Raise it per request with ?limit=.
	defaultDeviceLimit = 10
	// maxDeviceLimit is the ceiling on that override.
	maxDeviceLimit = 200
)

func handleAvailability(reader TimeseriesReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := strings.TrimSpace(c.Query("device_id"))
		if deviceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
			return
		}
		windows, err := reader.DataAvailability(c.Request.Context(), auth.Bearer(c), deviceID)
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"device_id": deviceID, "availability": windows})
	}
}

func handleUsage(reader TimeseriesReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceIDs := splitCSV(c.Query("device_ids"))
		if len(deviceIDs) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "device_ids is required"})
			return
		}
		usage, err := reader.DeviceUsage(c.Request.Context(), auth.Bearer(c), deviceIDs)
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"usage": usage})
	}
}

// handleQuickProfiles is the M1a surface: candidate series ranked from metadata
// alone (§5.2, §5.4.2). The response carries the read counts, so "no value was
// read" is verifiable from the answer rather than a claim about the code.
func handleQuickProfiles(deviceService *devices.Service, prof *profiler.Profiler) gin.HandlerFunc {
	return func(c *gin.Context) {
		window, err := parseWindowQuery(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		includeUnqueryable, err := parseBool(c.Query("include_unqueryable"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "include_unqueryable must be a boolean"})
			return
		}
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		output, err := runQuickProfiles(c.Request.Context(), auth.Bearer(c), deviceService, prof,
			QuickProfileInput{
				Search:             strings.TrimSpace(c.Query("search")),
				Limit:              limit,
				Window:             window,
				IncludeUnqueryable: includeUnqueryable,
			})
		if err != nil {
			respondProfilerError(c, err)
			return
		}
		c.JSON(http.StatusOK, output)
	}
}

func parseLimit(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("limit must be a non-negative integer")
	}
	return parsed, nil
}

type profileRequestBody struct {
	DeviceID       string       `json:"device_id"`
	ServiceID      string       `json:"service_id"`
	AnalysisWindow windowBody   `json:"analysis_window"`
	RawWindow      windowBody   `json:"raw_window"`
	GroupTime      string       `json:"group_time"`
	SessionParams  *sessionBody `json:"session_params"`
}

func (b profileRequestBody) toInput() (ProfileInput, error) {
	analysis, err := b.AnalysisWindow.parse("analysis_window")
	if err != nil {
		return ProfileInput{}, err
	}
	raw, err := b.RawWindow.parse("raw_window")
	if err != nil {
		return ProfileInput{}, err
	}
	input := ProfileInput{
		DeviceID:       strings.TrimSpace(b.DeviceID),
		ServiceID:      strings.TrimSpace(b.ServiceID),
		AnalysisWindow: analysis,
		RawWindow:      raw,
		GroupTime:      strings.TrimSpace(b.GroupTime),
	}
	if b.SessionParams != nil {
		input.SessionParams = &profiler.SessionParams{
			MinDurationS:   b.SessionParams.MinDurationS,
			MergeGapS:      b.SessionParams.MergeGapS,
			HysteresisFrac: b.SessionParams.HysteresisFrac,
		}
	}
	return input, nil
}

// quickProfileBody is the candidate listing over the WebSocket. The HTTP route
// takes the same fields as query parameters.
type quickProfileBody struct {
	Search             string     `json:"search"`
	Limit              int64      `json:"limit"`
	Window             windowBody `json:"window"`
	IncludeUnqueryable bool       `json:"include_unqueryable"`
}

func (b quickProfileBody) toInput() (QuickProfileInput, error) {
	window, err := b.Window.parse("window")
	if err != nil {
		return QuickProfileInput{}, err
	}
	return QuickProfileInput{
		Search:             strings.TrimSpace(b.Search),
		Limit:              b.Limit,
		Window:             window,
		IncludeUnqueryable: b.IncludeUnqueryable,
	}, nil
}

type windowBody struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type sessionBody struct {
	MinDurationS   float64 `json:"min_duration_s"`
	MergeGapS      float64 `json:"merge_gap_s"`
	HysteresisFrac float64 `json:"hysteresis_frac"`
}

// handleCreateProfiles computes the full profile of every variable of one
// service — the batched unit of work of D19, not one variable at a time.
func handleCreateProfiles(deviceService *devices.Service, prof *profiler.Profiler) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body profileRequestBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		input, err := body.toInput()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		result, err := runProfile(c.Request.Context(), auth.Bearer(c), deviceService, prof, input)
		if err != nil {
			respondProfilerError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func handleGetProfile(prof *profiler.Profiler) gin.HandlerFunc {
	return func(c *gin.Context) {
		profile, found := prof.Profile(c.Param("id"))
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "no such profile"})
			return
		}
		c.JSON(http.StatusOK, profile)
	}
}

// handleProjection serves the one model-facing view of a profile (D26). It is
// exposed over HTTP because the SPA needs to show the developer exactly what the
// LLM will be given, before M3 wires a model up to it.
func handleProjection(prof *profiler.Profiler) gin.HandlerFunc {
	return func(c *gin.Context) {
		profile, found := prof.Profile(c.Param("id"))
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "no such profile"})
			return
		}
		budget := 0
		if raw := strings.TrimSpace(c.Query("token_budget")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "token_budget must be a non-negative integer"})
				return
			}
			budget = parsed
		}
		c.JSON(http.StatusOK, profiler.Project(profile, budget))
	}
}

// handleSessions is the paginated session resource of D27. The profile carries
// statistics and a handful of exemplars; the full list lives here, because two
// years of washing-machine cycles is thousands of entries.
func handleSessions(prof *profiler.Profiler) gin.HandlerFunc {
	return func(c *gin.Context) {
		query := profiler.SessionQuery{Cursor: strings.TrimSpace(c.Query("cursor"))}

		if raw := strings.TrimSpace(c.Query("from")); raw != "" {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "from must be an RFC3339 timestamp"})
				return
			}
			query.From = at.UTC()
		}
		if raw := strings.TrimSpace(c.Query("to")); raw != "" {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "to must be an RFC3339 timestamp"})
				return
			}
			query.To = at.UTC()
		}
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a non-negative integer"})
				return
			}
			query.Limit = parsed
		}

		page, err := prof.Store().Sessions(c.Param("id"), query)
		if err != nil {
			respondProfilerError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

type overrideBody struct {
	FieldPath      string `json:"field_path"`
	Action         string `json:"action"`
	ComputedValue  any    `json:"computed_value"`
	ConfirmedValue any    `json:"confirmed_value"`
	Note           string `json:"note"`
}

// handleCreateOverride appends a developer confirmation to the overlay (D21).
//
// This route exists for the developer only. §5.8 lists writing a ProfileOverride
// among the operations with no tool at all, so when the LLM surface lands in M3
// it must not gain one: a model that can confirm its own inferred unit has
// confirmed nothing.
func handleCreateOverride(prof *profiler.Profiler) gin.HandlerFunc {
	return func(c *gin.Context) {
		profile, found := prof.Profile(c.Param("id"))
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "no such profile"})
			return
		}

		var body overrideBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}

		token := auth.MustFromContext(c)
		stored, err := prof.Store().AppendOverride(profiler.ProfileOverride{
			SeriesRef:       profile.SeriesRef,
			ProfileID:       profile.ProfileID,
			DetectorVersion: profile.DetectorVersion,
			CreatedBy:       token.Sub,
			CreatedAt:       time.Now().UTC(),
			FieldPath:       strings.TrimSpace(body.FieldPath),
			Action:          profiler.OverrideAction(strings.TrimSpace(body.Action)),
			ComputedValue:   body.ComputedValue,
			ConfirmedValue:  body.ConfirmedValue,
			Note:            body.Note,
		})
		if err != nil {
			respondProfilerError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"override":    stored,
			"confirmable": profiler.ConfirmablePaths,
		})
	}
}

func (w windowBody) parse(name string) (profiler.Window, error) {
	out := profiler.Window{}
	from := strings.TrimSpace(w.From)
	to := strings.TrimSpace(w.To)
	if from == "" && to == "" {
		return out, nil
	}
	if from == "" || to == "" {
		return out, errors.New(name + " needs both from and to")
	}
	parsedFrom, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return out, errors.New(name + ".from must be an RFC3339 timestamp")
	}
	parsedTo, err := time.Parse(time.RFC3339, to)
	if err != nil {
		return out, errors.New(name + ".to must be an RFC3339 timestamp")
	}
	if !parsedTo.After(parsedFrom) {
		return out, errors.New(name + ".to must be after " + name + ".from")
	}
	return profiler.Window{From: parsedFrom.UTC(), To: parsedTo.UTC()}, nil
}

func parseWindowQuery(c *gin.Context) (profiler.Window, error) {
	return windowBody{From: c.Query("from"), To: c.Query("to")}.parse("window")
}

func parseBool(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}

func splitCSV(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// respondProfilerError separates what the caller asked for wrongly from what the
// platform refused, so a bad window is not reported as an outage.
func respondProfilerError(c *gin.Context, err error) {
	switch status := statusForError(err); status {
	case 0:
		respondUpstream(c, err)
	default:
		c.JSON(status, gin.H{"error": err.Error()})
	}
}

// statusForError classifies an error once, for both the HTTP routes and the
// WebSocket. Zero means "ask respondUpstream", which knows how to report a
// platform failure.
func statusForError(err error) int {
	switch {
	case errors.Is(err, profiler.ErrInvalidRequest),
		errors.Is(err, profiler.ErrNoVariables),
		errors.Is(err, profiler.ErrInvalidOverride),
		errors.Is(err, profiler.ErrInvalidCursor),
		errors.Is(err, devices.ErrInvalidOption),
		errors.Is(err, timeseries.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, profiler.ErrNoPermission):
		return http.StatusForbidden
	case errors.Is(err, profiler.ErrProfileNotFound):
		return http.StatusNotFound
	default:
		return 0
	}
}
