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
	"fmt"
	"net/http"
	"strings"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
)

// selectionBody is the M2 surface: resolve_semantic_selection (SPEC §5.2).
//
// A POST rather than a GET, matching the platform's own
// POST /v2/query/device-type-selectables: the request carries three id lists and
// a window, and none of that belongs in a query string. It reads nothing and
// writes nothing.
type selectionBody struct {
	Intent string `json:"intent"`

	// The explicit id lists bypass the lexical matcher. They are how a caller that
	// has already read the ontology — an LLM in M3 — asks for exactly what it
	// means, and how a developer pins one half of a query.
	FunctionIDs    []string `json:"function_ids"`
	AspectIDs      []string `json:"aspect_ids"`
	DeviceClassIDs []string `json:"device_class_ids"`

	Interaction        string `json:"interaction"`
	IncludeControlling bool   `json:"include_controlling"`

	MatchLimit int      `json:"match_limit"`
	MinScore   *float64 `json:"min_score"`

	Limit  int64      `json:"limit"`
	Window windowBody `json:"window"`

	// Rank defaults to true when absent, which is why it is a pointer: §5.2 ranks
	// candidates by QuickProfile rather than returning them unordered. Sending
	// false asks for the ontology resolution alone, which costs no per-device
	// round trip.
	Rank *bool `json:"rank"`
}

func (b selectionBody) toInput() (SelectionInput, error) {
	window, err := b.Window.parse("window")
	if err != nil {
		return SelectionInput{}, err
	}
	interaction, err := parseInteraction(b.Interaction)
	if err != nil {
		return SelectionInput{}, err
	}

	input := SelectionInput{
		Intent:             strings.TrimSpace(b.Intent),
		FunctionIDs:        trimAll(b.FunctionIDs),
		AspectIDs:          trimAll(b.AspectIDs),
		DeviceClassIDs:     trimAll(b.DeviceClassIDs),
		Interaction:        interaction,
		IncludeControlling: b.IncludeControlling,
		MatchLimit:         b.MatchLimit,
		Limit:              b.Limit,
		Window:             window,
		SkipRanking:        b.Rank != nil && !*b.Rank,
	}
	if b.MinScore != nil {
		input.MinScore = *b.MinScore
	}

	if input.Intent == "" && len(input.FunctionIDs) == 0 &&
		len(input.AspectIDs) == 0 && len(input.DeviceClassIDs) == 0 {
		// Refused here rather than answered with an empty document: with nothing to
		// resolve there is no query to send, and an empty criteria list matches
		// every device type on the platform.
		return SelectionInput{}, fmt.Errorf(
			"%w: give an intent, or one of function_ids, aspect_ids and device_class_ids",
			selection.ErrInvalidRequest)
	}
	if input.Limit < 0 {
		return SelectionInput{}, fmt.Errorf("%w: limit must not be negative", selection.ErrInvalidRequest)
	}
	return input, nil
}

// parseInteraction maps the request's interaction onto the platform's own values.
//
// The default is event, because a request-only service is polled on demand and
// streams nothing to the database — there is no series to select (§5.4.13 item
// 5). "any" lifts the filter, which is how a developer sees a variable that
// exists in the ontology but was never going to be readable.
func parseInteraction(raw string) (models.Interaction, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return "", nil
	case string(models.EVENT):
		return models.EVENT, nil
	case string(models.REQUEST):
		return models.REQUEST, nil
	case string(models.EVENT_AND_REQUEST):
		return models.EVENT_AND_REQUEST, nil
	case string(selection.InteractionAny):
		return selection.InteractionAny, nil
	default:
		return "", fmt.Errorf("%w: interaction must be one of %q, %q, %q, %q",
			selection.ErrInvalidRequest,
			models.EVENT, models.REQUEST, models.EVENT_AND_REQUEST, selection.InteractionAny)
	}
}

func trimAll(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// handleSelection resolves one intent. The response carries its own read counts,
// so "this happened entirely at tier L0" is checkable from the answer rather than
// being a claim about the code (§3.2).
// @Summary		Resolve a semantic intent to concrete series
// @Description	The M2 surface (§5.2). A free-text intent is matched lexically against
// @Description	the ontology; explicit id lists bypass the matcher, which is how a
// @Description	caller that has already read the ontology asks for exactly what it
// @Description	means. Candidates are ranked by QuickProfile unless rank is false, and
// @Description	ranking needs a timescale-wrapper — without one the resolution still
// @Description	returns series, just unordered.
// @Tags			selection
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		selectionBody	true	"the intent, or the ids to resolve"
// @Success		200		{object}	map[string]interface{}
// @Failure		400		{object}	map[string]string	"a malformed body, or neither an intent nor any ids"
// @Failure		401		{object}	map[string]string
// @Failure		502		{object}	map[string]string	"the device repository could not be read"
// @Router			/selection [post]
func handleSelection(resolver *selection.Resolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body selectionBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		input, err := body.toInput()
		if err != nil {
			respondProfilerError(c, err)
			return
		}

		result, err := runSelection(c.Request.Context(), auth.Bearer(c), resolver, input)
		if err != nil {
			respondProfilerError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}
