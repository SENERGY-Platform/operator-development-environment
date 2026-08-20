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
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
)

// The relational surface (SPEC §5.5, M6).
//
// The split across the two transports is the same one the profiler makes, for the
// same reason. Proposing candidate sets reads the ontology, a device list and a
// device-group list — metadata, seconds, tier L0 — so it is an ordinary HTTP route.
// Running a relational pass profiles every participating service and then issues an
// aligned read across all of them, which is minutes on a wide window, so it is on
// the cancellable WebSocket as well, and closing the tab stops paying for it.
//
// Deciding a rule is HTTP and developer-only. §5.8 gives no tool for writing a
// ProfileOverride and this is the same act on a different object: a model that could
// confirm the rules it proposed would be grading its own work.

// @Summary		Propose related device sets from an aspect
// @Description	Turns an aspect node into candidate device sets (SPEC §5.5). The aspect
// @Description	hierarchy is what solves candidate selection: the devices under "Kitchen"
// @Description	yield the oven and the lights without the developer naming either.
// @Description
// @Description	Three kinds of set come back, in order of how much the grouping is worth
// @Description	trusting: an existing platform device group, then one set per aspect node,
// @Description	then the whole subtree. Every set names at least two devices, because a
// @Description	single-device set has no conditional pattern in it.
// @Description
// @Description	Reads no values. The reads block says so, which is what makes the tier-L0
// @Description	claim checkable from the answer.
// @Tags			relations
// @Produce		json
// @Security		Bearer
// @Param			aspect_id			query		string	true	"the aspect node to propose from"
// @Param			include_descendants	query		bool	false	"keep series declared against nodes below the requested one"
// @Param			limit				query		int		false	"how many devices to expand"
// @Param			max_members			query		int		false	"how many series one set may offer"
// @Success		200					{object}	relations.Proposal
// @Failure		400					{object}	map[string]string	"no aspect id, or no such aspect node"
// @Failure		401					{object}	map[string]string
// @Failure		502					{object}	map[string]string	"the platform could not be read"
// @Router			/relations/candidate-sets [get]
func handleCandidateSets(service *relations.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		includeDescendants, err := parseBool(c.Query("include_descendants"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "include_descendants must be true or false"})
			return
		}
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		maxMembers, err := parseCount(c.Query("max_members"), "max_members")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		proposal, err := service.ProposeRelatedSets(c.Request.Context(), auth.Bearer(c),
			relations.ProposalRequest{
				AspectID:           strings.TrimSpace(c.Query("aspect_id")),
				IncludeDescendants: includeDescendants,
				DeviceLimit:        clampDeviceLimit(limit),
				MaxMembers:         maxMembers,
			})
		if err != nil {
			respondRelationError(c, err)
			return
		}
		c.JSON(http.StatusOK, proposal)
	}
}

// relationBody is one relational pass as a caller asks for it.
//
// Members are flat {device_id, service_id, variable_path} rather than a nested ref,
// matching the tool schema: the shape a model writes reliably is also the shape a
// hand-written curl gets right first time.
type relationBody struct {
	Members        []relationMemberBody    `json:"members"`
	Window         windowBody              `json:"window"`
	GridSeconds    float64                 `json:"grid_seconds"`
	Params         relations.RuleParams    `json:"params"`
	Conditioning   *relations.Conditioning `json:"conditioning"`
	CandidateSetID string                  `json:"candidate_set_id"`
}

type relationMemberBody struct {
	DeviceID     string `json:"device_id"`
	ServiceID    string `json:"service_id"`
	VariablePath string `json:"variable_path"`
	Label        string `json:"label"`
}

func (b relationBody) toInput() (RelationInput, error) {
	window, err := b.Window.parse("window")
	if err != nil {
		return RelationInput{}, err
	}
	members := make([]relations.SeriesMember, 0, len(b.Members))
	for _, member := range b.Members {
		members = append(members, relations.SeriesMember{
			Ref: profiler.SeriesRef{
				DeviceID:     strings.TrimSpace(member.DeviceID),
				ServiceID:    strings.TrimSpace(member.ServiceID),
				VariablePath: strings.TrimSpace(member.VariablePath),
			},
			Label: strings.TrimSpace(member.Label),
		})
	}
	return RelationInput{
		Members:        members,
		Window:         window,
		GridSeconds:    b.GridSeconds,
		Params:         b.Params,
		Conditioning:   b.Conditioning,
		CandidateSetID: strings.TrimSpace(b.CandidateSetID),
	}, nil
}

// @Summary		Compute a relational profile
// @Description	Profiles every participating service, aligns the members onto one grid with
// @Description	a single batched query, derives idle and active from each activity_pattern,
// @Description	and proposes candidate rules with their exception windows (SPEC §5.5).
// @Description
// @Description	Values are read, so this is tier L1 for an LLM — but nothing that comes
// @Description	back carries one: the document is contingency counts, ratios and bucket
// @Description	durations.
// @Description
// @Description	Also available over the WebSocket as `relate`, which is the better route for
// @Description	a wide window: the pass reads two passes per service plus the aligned read,
// @Description	and the socket can cancel it.
// @Tags			relations
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		relationBody	true	"the members and the window"
// @Success		201		{object}	relations.RelationProfile
// @Failure		400		{object}	map[string]string	"fewer than two members, too many, or a malformed window"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string	"the caller may not read one of the devices"
// @Failure		502		{object}	map[string]string	"the platform could not be read"
// @Router			/relations [post]
func handleCreateRelation(service *relations.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body relationBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		input, err := body.toInput()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		profile, err := runRelation(c.Request.Context(), auth.Bearer(c), service, input)
		if err != nil {
			respondRelationError(c, err)
			return
		}
		c.JSON(http.StatusCreated, profile)
	}
}

// @Summary		Read a stored relational profile
// @Description	Serves a computed relation with the decision log as it stands now, so a rule
// @Description	confirmed after the pass ran arrives carrying that verdict.
// @Tags			relations
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"relation id"
// @Success		200	{object}	relations.RelationProfile
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Router			/relations/{id} [get]
func handleGetRelation(service *relations.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		profile, err := service.Get(c.Param("id"))
		if err != nil {
			respondRelationError(c, err)
			return
		}
		c.JSON(http.StatusOK, profile)
	}
}

// decisionBody is one developer verdict on one candidate rule.
type decisionBody struct {
	RuleID string `json:"rule_id"`
	Action string `json:"action"`
	// Confirmed is the developer's own form of the rule, required for a correction —
	// a narrowed statement, exceptions they added, exceptions they struck out.
	Confirmed *relations.DecidedRule `json:"confirmed"`
	Note      string                 `json:"note"`
}

// @Summary		Confirm, correct or reject a candidate rule
// @Description	Records a developer's verdict on one candidate rule (SPEC §5.10, D21). The
// @Description	log is append-only and keyed by a fingerprint of what the rule *says*, so
// @Description	a verdict survives the rule being recomputed over a different window by a
// @Description	later detector — and a developer who changes their mind adds a record
// @Description	rather than replacing one.
// @Description
// @Description	Developer action only. No LLM tool exists for it, for the reason §5.8 gives
// @Description	about writing a ProfileOverride: a model confirming its own findings is
// @Description	grading its own work.
// @Tags			relations
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string			true	"relation id"
// @Param			request	body		decisionBody	true	"the verdict"
// @Success		201		{object}	relations.RuleDecision
// @Failure		400		{object}	map[string]string	"an unknown action, or a correction with no rule"
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string	"no such relation, or it carries no such rule"
// @Router			/relations/{id}/rule-decisions [post]
func handleDecideRule(service *relations.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body decisionBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}

		token := auth.MustFromContext(c)
		decision, err := service.Decide(relations.DecisionRequest{
			RelationID: c.Param("id"),
			RuleID:     strings.TrimSpace(body.RuleID),
			Action:     relations.DecisionAction(strings.TrimSpace(body.Action)),
			Confirmed:  body.Confirmed,
			Note:       strings.TrimSpace(body.Note),
			// From the authenticated token, never from the body: this is the record of
			// who decided, and it is the whole evidentiary value of the log (§5.4.3).
			UserSub: token.Sub,
		})
		if err != nil {
			respondRelationError(c, err)
			return
		}
		c.JSON(http.StatusCreated, decision)
	}
}

// @Summary		Read the decision history of one rule
// @Description	Every verdict recorded against a rule fingerprint, oldest first. The log is
// @Description	append-only, so this is where "the detector said 0.91, the developer
// @Description	confirmed it, then narrowed it a week later" is readable.
// @Tags			relations
// @Produce		json
// @Security		Bearer
// @Param			rule_id	query		string	true	"the rule fingerprint"
// @Success		200		{object}	map[string]any
// @Failure		400		{object}	map[string]string
// @Failure		401		{object}	map[string]string
// @Router			/relations/rule-decisions [get]
func handleRuleDecisions(service *relations.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ruleID := strings.TrimSpace(c.Query("rule_id"))
		if ruleID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "rule_id is required"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"rule_id":   ruleID,
			"decisions": service.Decisions(ruleID),
		})
	}
}

// clampDeviceLimit applies ODE's one answer to "how many devices may a request
// expand". The clamp lives beside the other expanding operations for the reason
// runSelection gives: a proposal that quietly listed two hundred devices would take
// twenty times as long as the candidate listing next to it, with nothing saying why.
func clampDeviceLimit(limit int64) int64 {
	if limit <= 0 {
		return defaultDeviceLimit
	}
	if limit > maxDeviceLimit {
		return maxDeviceLimit
	}
	return limit
}

// parseCount reads a non-negative bound from a query string. Zero means "the
// service default", which every bound in this package treats the same way.
func parseCount(raw string, name string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return parsed, nil
}

// respondRelationError separates what the caller asked for wrongly from what the
// platform refused, so two members named badly is not reported as an outage.
func respondRelationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, relations.ErrInvalidRequest),
		errors.Is(err, relations.ErrTooFewMembers),
		errors.Is(err, relations.ErrInvalidDecision):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, relations.ErrRelationNotFound),
		errors.Is(err, relations.ErrUnknownRule):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		respondProfilerError(c, err)
	}
}
