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
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// The exploration pane's routes (SPEC §5.9, §5.10, M5).
//
// Two things about this surface are decisions rather than defaults.
//
// It is HTTP rather than the WebSocket, unlike the profiler's operations. A chart
// read is one batched POST /queries/v2 bounded by a point cap — seconds, not
// minutes — so it does not need the cancellable socket that a raw profiler pass
// does. Adding it there would mean a second code path for something that answers
// once.
//
// And the *data* route is where a chart's values come from, never a tool result.
// §3.2's tiers bound what reaches an LLM context; they do not stand between a
// developer and their own permitted series. So render_chart returns a
// specification (§5.8, tier L1) and this reads the values behind it under the
// caller's own token.

// chartBody is §5.9's own document shape, so the HTTP surface takes the
// specification as the specification prints it. The tool schema in pkg/tools is
// flattened instead, because a model writes {device_id, service_id,
// variable_path} far more reliably than a nested ref.
type chartBody struct {
	SessionID   string              `json:"session_id"`
	Title       string              `json:"title"`
	Caption     string              `json:"caption"`
	Series      []charts.SeriesSpec `json:"series"`
	Annotations []charts.Annotation `json:"annotations"`
	Markers     []charts.Marker     `json:"markers"`
	YAxis       charts.YAxis        `json:"y_axis"`
	Window      windowBody          `json:"window"`
	GroupTime   string              `json:"group_time"`
}

// @Summary		Create a chart specification
// @Description	Validates a declarative chart specification (SPEC §5.9), resolves every
// @Description	series against the device type and the ontology, and stores it. No values
// @Description	are read: this answers with the resolved units, the axis and the chart id,
// @Description	and GET /charts/{id}/data is what reads the data behind it.
// @Description
// @Description	The resolution happens now rather than at first render so that a
// @Description	`convert:` naming an unreachable characteristic is refused while the author
// @Description	can still fix it — and because the device read it takes is the same
// @Description	permission check the value read will need.
// @Tags			charts
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		chartBody	true	"the specification"
// @Success		201		{object}	charts.Created
// @Failure		400		{object}	map[string]string	"a malformed specification, or a transform that cannot be resolved"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string	"the caller may not read one of the devices"
// @Failure		502		{object}	map[string]string	"the platform could not be read"
// @Router			/charts [post]
func handleCreateChart(service *charts.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body chartBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		window, err := body.Window.parse("window")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		token := auth.MustFromContext(c)
		created, err := service.Create(c.Request.Context(), auth.Bearer(c), charts.CreateRequest{
			UserSub:   token.Sub,
			SessionID: strings.TrimSpace(body.SessionID),
			// The route is the developer's, so the author is the developer. An author
			// is never read from a request body: the pane shows who put an element on
			// screen, and that has to mean something.
			Author:      charts.AuthorDeveloper,
			Title:       body.Title,
			Caption:     body.Caption,
			Series:      body.Series,
			Annotations: body.Annotations,
			Markers:     body.Markers,
			YAxis:       body.YAxis,
			Window:      window,
			GroupTime:   body.GroupTime,
		})
		if err != nil {
			respondChartError(c, err)
			return
		}
		c.JSON(http.StatusCreated, created)
	}
}

// @Summary		The caller's charts
// @Description	Newest first, optionally narrowed to one chat session — which is how the
// @Description	pane lists what the assistant proposed in the conversation being read.
// @Tags			charts
// @Produce		json
// @Security		Bearer
// @Param			session_id	query		string	false	"only charts proposed in this chat session"
// @Param			limit		query		int		false	"how many to return"
// @Success		200			{object}	map[string]interface{}
// @Failure		401			{object}	map[string]string
// @Router			/charts [get]
func handleListCharts(service *charts.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		token := auth.MustFromContext(c)
		specs := service.List(token.Sub, strings.TrimSpace(c.Query("session_id")), int(limit))
		c.JSON(http.StatusOK, gin.H{"charts": specs, "count": len(specs)})
	}
}

// @Summary		One chart specification
// @Tags			charts
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"chart id"
// @Success		200	{object}	charts.Spec
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string	"no such chart, or not this developer's"
// @Router			/charts/{id} [get]
func handleGetChart(service *charts.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		spec, err := service.Get(c.Param("id"), token.Sub)
		if err != nil {
			respondChartError(c, err)
			return
		}
		c.JSON(http.StatusOK, spec)
	}
}

// @Summary		Discard a chart
// @Tags			charts
// @Security		Bearer
// @Param			id	path	string	true	"chart id"
// @Success		204	"discarded"
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Router			/charts/{id} [delete]
func handleDeleteChart(service *charts.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		if err := service.Delete(c.Param("id"), token.Sub); err != nil {
			respondChartError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// @Summary		The values a chart draws
// @Description	Reads the series a specification names, on behalf of the caller, and
// @Description	returns them with the profiler-derived annotations of §5.10: detected
// @Description	sessions, gaps, advised exclusions and usable range, and counter-reset
// @Description	markers. Every transform is evaluated by the platform (§5.3.1) — the
// @Description	bucket, the counter differencing and the unit conversion are all fields of
// @Description	POST /queries/v2, not arithmetic done here.
// @Description
// @Description	This is the only route in ODE that hands series values to a client, and it
// @Description	hands them to the developer. The exposure tier bounds what an LLM sees; it
// @Description	does not stand between a developer and their own data.
// @Tags			charts
// @Produce		json
// @Security		Bearer
// @Param			id			path		string	true	"chart id"
// @Param			from		query		string	false	"RFC3339. Overrides the specification's window, for zooming"
// @Param			to			query		string	false	"RFC3339"
// @Param			group_time	query		string	false	"aggregation bucket, e.g. 15m. Widened if the window would exceed the point cap"
// @Success		200			{object}	charts.Data
// @Failure		400			{object}	map[string]string	"a malformed window or bucket"
// @Failure		401			{object}	map[string]string
// @Failure		403			{object}	map[string]string	"the caller may not read one of the devices"
// @Failure		404			{object}	map[string]string	"no such chart, or not this developer's"
// @Failure		502			{object}	map[string]string	"the platform could not be read"
// @Router			/charts/{id}/data [get]
func handleChartData(service *charts.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		window, err := parseWindowQuery(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		token := auth.MustFromContext(c)
		data, err := service.Data(c.Request.Context(), auth.Bearer(c), charts.DataRequest{
			ChartID:   c.Param("id"),
			UserSub:   token.Sub,
			Window:    window,
			GroupTime: strings.TrimSpace(c.Query("group_time")),
		})
		if err != nil {
			respondChartError(c, err)
			return
		}
		c.JSON(http.StatusOK, data)
	}
}

type chartConfirmBody struct {
	SeriesIndex    int    `json:"series_index"`
	FieldPath      string `json:"field_path"`
	Action         string `json:"action"`
	ComputedValue  any    `json:"computed_value"`
	ConfirmedValue any    `json:"confirmed_value"`
	Note           string `json:"note"`
}

// @Summary		Confirm what a chart shows
// @Description	Appends a developer confirmation, correction or rejection to the profiler's
// @Description	override overlay (D21) from the exploration pane — an inferred unit, a
// @Description	session boundary, a gap classification, an advised range (§5.10). It is the
// @Description	same overlay the profiler view writes to and the same one every later
// @Description	profile is resolved against, keyed by series rather than by profile, so a
// @Description	confirmation survives recomputation and applies to a series that has never
// @Description	been profiled at all.
// @Description
// @Description	A developer action only. §5.8 lists writing a ProfileOverride among the
// @Description	operations with no LLM tool: a model that can confirm its own inferred unit
// @Description	has confirmed nothing.
// @Tags			charts
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string				true	"chart id"
// @Param			request	body		chartConfirmBody	true	"which series, which field, and what the developer says"
// @Success		201		{object}	charts.Confirmed
// @Failure		400		{object}	map[string]string	"a malformed body, or a field that cannot be confirmed"
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string
// @Router			/charts/{id}/confirmations [post]
func handleConfirmChart(service *charts.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body chartConfirmBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		token := auth.MustFromContext(c)
		confirmed, err := service.Confirm(c.Request.Context(), auth.Bearer(c), charts.ConfirmRequest{
			ChartID:        c.Param("id"),
			UserSub:        token.Sub,
			SeriesIndex:    body.SeriesIndex,
			FieldPath:      strings.TrimSpace(body.FieldPath),
			Action:         profiler.OverrideAction(strings.TrimSpace(body.Action)),
			ComputedValue:  body.ComputedValue,
			ConfirmedValue: body.ConfirmedValue,
			Note:           body.Note,
		})
		if err != nil {
			respondChartError(c, err)
			return
		}
		c.JSON(http.StatusCreated, confirmed)
	}
}

// respondChartError separates a bad specification from a platform failure, the
// same split respondProfilerError makes and for the same reason: a refused
// transform is the author's to fix, and reporting it as an outage sends them
// looking in the wrong place.
func respondChartError(c *gin.Context, err error) {
	respondProfilerError(c, err)
}
