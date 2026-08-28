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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
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

// @Summary		Where a device has data
// @Description	A thin passthrough to the timescale-wrapper, read as the caller.
// @Tags			timeseries
// @Produce		json
// @Security		Bearer
// @Param			device_id	query		string	true	"device id"
// @Success		200			{object}	map[string]interface{}
// @Failure		400			{object}	map[string]string	"device_id is missing"
// @Failure		401			{object}	map[string]string
// @Failure		502			{object}	map[string]string	"the timescale-wrapper could not be read"
// @Router			/timeseries/availability [get]
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

// @Summary		How much data devices hold
// @Tags			timeseries
// @Produce		json
// @Security		Bearer
// @Param			device_ids	query		string	true	"comma-separated device ids"
// @Success			200			{object}	map[string][]timeseries.Usage
// @Failure		400			{object}	map[string]string	"device_ids is missing"
// @Failure		401			{object}	map[string]string
// @Failure		502			{object}	map[string]string
// @Router			/timeseries/usage [get]
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

// @Summary		Whether an export holds any rows
// @Description	The export-side counterpart of /timeseries/availability, and a
// @Description	different kind of answer because the platform has no availability
// @Description	endpoint for an export: the rows are counted per column, so the reply
// @Description	says whether anything is stored and which columns are null throughout.
// @Description	No value is read.
// @Tags			timeseries
// @Produce		json
// @Security		Bearer
// @Param			export_id	query		string	true	"export id"
// @Param			from		query		string	false	"window start, RFC3339; empty means a multi-year lookback"
// @Param			to			query		string	false	"window end, RFC3339"
// @Success		200			{object}	profiler.ExportFill
// @Failure		400			{object}	map[string]string	"export_id is missing, or an unparseable window"
// @Failure		401			{object}	map[string]string
// @Failure		404			{object}	map[string]string	"no such export is visible to this account"
// @Failure		502			{object}	map[string]string	"the platform could not be read"
// @Router			/timeseries/export-data [get]
func handleExportData(prof *profiler.Profiler) gin.HandlerFunc {
	return func(c *gin.Context) {
		exportID := strings.TrimSpace(c.Query("export_id"))
		if exportID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "export_id is required"})
			return
		}
		window, err := parseWindowQuery(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		fill, err := prof.ExportFill(c.Request.Context(), auth.Bearer(c),
			profiler.ExportFillRequest{ExportID: exportID, Window: window})
		if err != nil {
			respondProfilerError(c, err)
			return
		}
		c.JSON(http.StatusOK, fill)
	}
}

// handleQuickProfiles is the M1a surface: candidate series ranked from metadata
// alone (§5.2, §5.4.2). The response carries the read counts, so "no value was
// read" is verifiable from the answer rather than a claim about the code.
//
// @Summary		Candidate series, ranked from metadata alone
// @Description	The M1a surface (§5.2, §5.4.2): every candidate series behind the
// @Description	matching devices, ranked without reading a single value. The response
// @Description	carries the read counts, so "no value was read" is verifiable from the
// @Description	answer rather than a claim about the code.
// @Tags			profiler
// @Produce		json
// @Security		Bearer
// @Param			search					query		string	false	"free-text device filter"
// @Param			limit					query		int		false	"how many devices to expand; the ceiling is 200"	default(10)
// @Param			from					query		string	false	"window start, RFC3339"
// @Param			to						query		string	false	"window end, RFC3339"
// @Param			include_unqueryable		query		bool	false	"keep series the platform cannot serve"
// @Success		200						{object}	map[string]interface{}
// @Failure		400						{object}	map[string]string	"an unparseable window, limit or flag"
// @Failure		401						{object}	map[string]string
// @Failure		502						{object}	map[string]string
// @Router			/quick-profiles [get]
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
	DeviceID  string `json:"device_id"`
	ServiceID string `json:"service_id"`
	// ExportID profiles an export instead of a device's service. Exclusive with
	// the two above.
	ExportID       string       `json:"export_id"`
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
		ExportID:       strings.TrimSpace(b.ExportID),
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
//
// An export is profiled through the same route, by export_id instead of the
// device pair. The unit of work is then the export and the variable paths are its
// column names; everything else about the answer is the same shape.
//
// @Summary		Profile every variable of one service, or every column of one export
// @Description	The batched unit of work of D19: one request profiles all variables of
// @Description	a service rather than one variable at a time. The raw pass reads the
// @Description	smaller of the configured window bounds, anchored at the most recent
// @Description	data (D25).
// @Description
// @Description	Give export_id instead of device_id and service_id to profile an
// @Description	export. Its window comes from counting rows rather than from
// @Description	/data-availability, which the platform offers for devices only.
// @Tags			profiler
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		profileRequestBody	true	"which service to profile, and over which windows"
// @Success		200		{object}	map[string]interface{}
// @Failure		400		{object}	map[string]string	"a malformed body or window"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string	"the platform refused this user the device"
// @Failure		404		{object}	map[string]string	"no such device or service"
// @Failure		502		{object}	map[string]string
// @Router			/profiles [post]
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

// @Summary		One computed profile
// @Tags			profiler
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"profile id"
// @Success		200	{object}	profiler.SeriesProfile
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Router			/profiles/{id} [get]
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
// LLM is given, whether or not a model is reading it.
//
// @Summary		The model-facing view of a profile
// @Description	The one projection an LLM is ever given (D26), exposed over HTTP so the
// @Description	SPA can show the developer exactly what the model will see.
// @Tags			profiler
// @Produce		json
// @Security		Bearer
// @Param			id				path		string	true	"profile id"
// @Param			token_budget	query		int		false	"cap the projection; 0 or absent takes the configured budget"
// @Success		200				{object}	map[string]interface{}
// @Failure		400				{object}	map[string]string	"token_budget is not a non-negative integer"
// @Failure		401				{object}	map[string]string
// @Failure		404				{object}	map[string]string
// @Router			/profiles/{id}/projection [get]
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
//
// @Summary		A profile's detected sessions, paginated
// @Description	The paginated resource of D27. A profile carries session statistics and
// @Description	a handful of exemplars; the full list is here, because two years of
// @Description	washing-machine cycles is thousands of entries.
// @Tags			profiler
// @Produce		json
// @Security		Bearer
// @Param			id		path		string	true	"profile id"
// @Param			cursor	query		string	false	"continuation cursor from a previous page"
// @Param			from	query		string	false	"only sessions at or after this RFC3339 timestamp"
// @Param			to		query		string	false	"only sessions before this RFC3339 timestamp"
// @Param			limit	query		int		false	"page size"
// @Success		200		{object}	profiler.SessionPage
// @Failure		400		{object}	map[string]string	"an unparseable timestamp or limit"
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string
// @Router			/profiles/{id}/sessions [get]
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
// among the operations with no tool at all, and the LLM surface has none: a model
// that can confirm its own inferred unit has confirmed nothing.
//
// @Summary		Confirm or correct a derived field
// @Description	Appends a developer confirmation to the override overlay (D21), which
// @Description	§5.4.3 calls an empirical record. A developer action only: §5.8 lists
// @Description	writing an override among the operations with no LLM tool at all,
// @Description	because a model that can confirm its own inferred unit has confirmed
// @Description	nothing.
// @Tags			profiler
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string			true	"profile id"
// @Param			request	body		overrideBody	true	"which field, and what the developer says it is"
// @Success		201		{object}	profiler.ProfileOverride
// @Failure		400		{object}	map[string]string	"a malformed body, or a field that cannot be overridden"
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string
// @Router			/profiles/{id}/overrides [post]
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

// parse reads a window from a request body.
//
// The errors wrap profiler.ErrInvalidRequest so that statusForError classifies
// them as 400 rather than falling through to "the platform failed". The HTTP
// routes answer 400 for a malformed body themselves, but the WebSocket classifies
// through that one function — and a badly typed window arriving as a platform
// outage is precisely the guess the socket's status field exists to prevent.
func (w windowBody) parse(name string) (profiler.Window, error) {
	out := profiler.Window{}
	from := strings.TrimSpace(w.From)
	to := strings.TrimSpace(w.To)
	if from == "" && to == "" {
		return out, nil
	}
	if from == "" || to == "" {
		return out, fmt.Errorf("%w: %s needs both from and to", profiler.ErrInvalidRequest, name)
	}
	parsedFrom, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return out, fmt.Errorf("%w: %s.from must be an RFC3339 timestamp", profiler.ErrInvalidRequest, name)
	}
	parsedTo, err := time.Parse(time.RFC3339, to)
	if err != nil {
		return out, fmt.Errorf("%w: %s.to must be an RFC3339 timestamp", profiler.ErrInvalidRequest, name)
	}
	if !parsedTo.After(parsedFrom) {
		return out, fmt.Errorf("%w: %s.to must be after %s.from", profiler.ErrInvalidRequest, name, name)
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
		errors.Is(err, charts.ErrInvalidSpec),
		errors.Is(err, charts.ErrNotConfirmable),
		errors.Is(err, profiler.ErrInvalidOverride),
		errors.Is(err, profiler.ErrInvalidCursor),
		errors.Is(err, devices.ErrInvalidOption),
		errors.Is(err, selection.ErrInvalidRequest),
		errors.Is(err, ontology.ErrNoCriteria),
		errors.Is(err, timeseries.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, profiler.ErrNoPermission):
		return http.StatusForbidden
	case errors.Is(err, profiler.ErrProfileNotFound),
		errors.Is(err, charts.ErrChartNotFound):
		return http.StatusNotFound
	default:
		return 0
	}
}
