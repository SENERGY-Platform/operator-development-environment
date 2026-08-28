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

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/interpret"
)

// The result interpretation surface (§5.13, M9).
//
// Two routes and no more. The interpretation itself is delivered into the
// conversation — that is what §5.13 asks for and where the developer reads it — so
// these exist for the pane beside it: one to read the run's interpretation as a
// document, and one for the decision §5.13's last sentence requires.
//
// The decision route is a developer action with no LLM tool behind it, in the
// shape §5.8 requires of every human judgement: a model that could accept its own
// proposal would be grading its own work, exactly as one that could write a
// ProfileOverride would (D21, D28).

// @Summary		A finished run's interpretation
// @Description	§5.13's summary, the assistant's reading of it, the concrete next
// @Description	adjustment it proposed, and the developer's decision on that proposal
// @Description	if they have given one.
// @Description
// @Description	Recomputed rather than stored: the summary comes from MLflow, the
// @Description	interpretation from the conversation the assistant wrote it in, and only
// @Description	the decisions come from a table — the split §5.4.3 makes between a
// @Description	recomputable artifact and a record of human judgement.
// @Description
// @Description	`proposal` carries an explicit `status: not_computed` where there is no
// @Description	proposal: a run whose developer has not been connected since it finished
// @Description	has not been interpreted yet, and that is a different fact from an
// @Description	assistant that proposed nothing.
// @Tags			experiments
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"the experiment id"
// @Success		200	{object}	interpret.Interpretation
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"MLflow could not be reached"
// @Router			/experiments/{id}/interpretation [get]
func handleGetInterpretation(svc *interpret.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := svc.Interpretation(c.Request.Context(), experimentRequest(c), c.Param("id"))
		if err != nil {
			respondInterpret(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

type proposalDecisionBody struct {
	// ProposalID is what the developer was looking at. Required, and checked against
	// the proposal that currently stands: deciding on a stale one is answered with a
	// 409 rather than recorded as agreement with something else.
	ProposalID string `json:"proposal_id"`
	// Decision is "accepted", "edited" or "rejected" — §5.13's three answers.
	Decision string `json:"decision"`
	// Edited is the developer's own form of the adjustment, required for an edit.
	Edited string `json:"edited"`
	Note   string `json:"note"`
}

// @Summary		Accept, edit or reject a proposed next experiment
// @Description	§5.13's last sentence, recorded. Append-only and keyed by the
// @Description	proposal's own fingerprint, so a rejected proposal stays rejected when the
// @Description	same run is interpreted again, and a developer who changes their mind adds
// @Description	a record rather than replacing one.
// @Description
// @Description	**Nothing here is binding (D28).** Accepting records agreement and
// @Description	launches nothing; promoting a value into `evaluation.yaml` or the operator
// @Description	config is a separate action the developer takes themselves, and §5.8 has no
// @Description	tool for either. There is no LLM tool for this route: a model that could
// @Description	accept its own proposal would be grading its own work.
// @Tags			experiments
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string			true	"the experiment id"
// @Param			body	body		proposalDecisionBody	true	"the decision"
// @Success		201		{object}	interpret.Interpretation
// @Failure		400		{object}	map[string]string	"no proposal to decide on, or an edit with no edited form"
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string
// @Failure		409		{object}	map[string]string	"the proposal has changed since it was read"
// @Router			/experiments/{id}/interpretation/decision [post]
func handleDecideProposal(svc *interpret.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body proposalDecisionBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		result, err := svc.Decide(c.Request.Context(), experimentRequest(c), c.Param("id"),
			interpret.DecisionRequest{
				ProposalID: body.ProposalID,
				Decision:   body.Decision,
				Edited:     body.Edited,
				Note:       body.Note,
			})
		if err != nil {
			respondInterpret(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	}
}

// respondInterpret maps this package's errors onto status codes.
func respondInterpret(c *gin.Context, err error) {
	switch {
	case errors.Is(err, interpret.ErrNotFound), errors.Is(err, experiments.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, interpret.ErrInvalidRequest):
		// A stale proposal id is the one case that is a conflict rather than a bad
		// request: the request was well formed and the world moved under it, and the
		// pane's answer is to re-read rather than to fix the body.
		if isStaleProposal(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": err.Error(),
				"needs": "reread",
				"hint": "the run was interpreted again and proposed something else; read " +
					"the interpretation and decide on the proposal that stands now",
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		respondExperiments(c, err)
	}
}

// isStaleProposal tells the conflict apart from the other refusals, which all
// share ErrInvalidRequest because they are all "this request cannot be recorded".
func isStaleProposal(err error) bool {
	var stale *interpret.StaleProposalError
	return errors.As(err, &stale)
}
