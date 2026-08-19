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
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
)

// The admin surface of §3.3, behind the `admin` realm role.

// requireAdmin gates the settings routes.
//
// A separate middleware rather than a check inside each handler, so that adding a
// route cannot accidentally omit the gate — the group carries it.
func requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		if !token.IsAdmin() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":         "missing required realm role",
				"required_role": "admin",
			})
			return
		}
		c.Next()
	}
}

// handleGetLimits serves the whole policy set, plus what this build actually
// enforces and the prices a cost cap can bind on.
//
// @Summary		Every limits record
// @Description	Returns the stored policies, the built-in defaults, which fields
// @Description	this build enforces versus merely stores for a later milestone, and
// @Description	the model pricing a cost cap is computed from (SPEC §3.3).
// @Tags			admin
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Failure		403	{object}	map[string]string	"the admin realm role is missing"
// @Failure		502	{object}	map[string]string	"the limits store could not be read"
// @Router			/admin/limits [get]
func handleGetLimits(service *admin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		records, err := service.AllLimits(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"limits":   records,
			"defaults": admin.Defaults(),
			// Which fields this build acts on, and which are stored for a later
			// milestone. An admin setting a kernel cap should know it is not yet
			// enforced rather than assume it is.
			"enforced": admin.EnforcedFields(),
			"declared": admin.DeclaredFields(),
			// A cost cap can only bind on a model ODE has a price for.
			"pricing":  service.Pricing().Prices(),
			"currency": service.Pricing().Currency(),
		})
	}
}

// @Summary		One subject's effective policy and spend
// @Description	The effective policy is the per-user record layered over the global
// @Description	one; the spend is what that subject has used in the current period.
// @Tags			admin
// @Produce		json
// @Security		Bearer
// @Param			sub	path		string	true	"platform user id"
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Failure		403	{object}	map[string]string
// @Failure		502	{object}	map[string]string
// @Router			/admin/limits/{sub} [get]
func handleGetSubjectLimits(service *admin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		subject := c.Param("sub")
		effective, err := service.Effective(c.Request.Context(), subject)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		spend, err := service.Spend(c.Request.Context(), subject, effective.PeriodDuration())
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"subject":   subject,
			"effective": effective,
			"spend":     spend,
		})
	}
}

// handlePutLimits writes the global or a per-user policy.
//
// @Summary		Write a limits policy
// @Description	Serves both /admin/limits, which writes the global record, and
// @Description	/admin/limits/{sub}, which writes one user's. The two paths exist so
// @Description	that "global" is explicit rather than an omitted path segment. The
// @Description	response carries the resulting effective policy, which for a subject
// @Description	is the per-user record layered over the global one.
// @Tags			admin
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			sub		path		string			false	"platform user id; absent writes the global record"
// @Param			limits	body		admin.Limits	true	"the policy to store"
// @Success		200		{object}	map[string]interface{}
// @Failure		400		{object}	map[string]string	"the body is not a valid policy"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string
// @Failure		502		{object}	map[string]string
// @Router			/admin/limits [put]
// @Router			/admin/limits/{sub} [put]
func handlePutLimits(service *admin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var limits admin.Limits
		if err := c.ShouldBindJSON(&limits); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// An absent :sub is the global record. The route pair makes that explicit
		// rather than overloading one path with an optional segment.
		subject := c.Param("sub")

		token := auth.MustFromContext(c)
		if err := service.SetLimits(c.Request.Context(), subject, limits, token.Sub); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		effective, err := service.Effective(c.Request.Context(), subject)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"subject": subject, "effective": effective})
	}
}

// handleAdminUsage is §3.3's accounting report.
//
// @Summary		LLM usage and spend
// @Tags			admin
// @Produce		json
// @Security		Bearer
// @Param			sub		query		string	false	"restrict to one platform user; absent reports across all"
// @Param			period	query		string	false	"Go duration, e.g. 720h; an unparseable value falls back to the default"
// @Param			limit	query		int		false	"maximum records"	default(200)
// @Success		200		{object}	map[string]interface{}
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string
// @Failure		502		{object}	map[string]string
// @Router			/admin/usage [get]
func handleAdminUsage(service *admin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		subject := c.Query("sub")
		period := parsePeriod(c.Query("period"))
		limit := 200
		if raw := c.Query("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = parsed
			}
		}

		records, err := service.Usage(c.Request.Context(), subject, period, limit)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		spend, err := service.Spend(c.Request.Context(), subject, period)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"usage":    records,
			"spend":    spend,
			"period":   period.String(),
			"currency": service.Pricing().Currency(),
		})
	}
}

// handleToolAudit is the record of what the LLM actually reached for, refusals
// included. It is what makes the tier argument checkable after the fact.
//
// @Summary		The tool-call audit trail
// @Description	Every tool the assistant reached for, refusals included. This is what
// @Description	makes the exposure-tier argument of §3.2 checkable after the fact.
// @Tags			admin
// @Produce		json
// @Security		Bearer
// @Param			sub		query		string	false	"restrict to one platform user"
// @Param			period	query		string	false	"Go duration, e.g. 720h"
// @Param			limit	query		int		false	"maximum records"	default(200)
// @Success		200		{object}	map[string]interface{}
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string
// @Failure		502		{object}	map[string]string
// @Router			/admin/tool-calls [get]
func handleToolAudit(service *admin.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		subject := c.Query("sub")
		period := parsePeriod(c.Query("period"))
		limit := 200
		if raw := c.Query("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = parsed
			}
		}

		calls, err := service.ToolCalls(c.Request.Context(), subject, period, limit)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"tool_calls": calls, "period": period.String()})
	}
}

func parsePeriod(raw string) time.Duration {
	if raw == "" {
		return admin.DefaultPeriod
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return admin.DefaultPeriod
	}
	return parsed
}
