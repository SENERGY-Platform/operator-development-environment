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

// handleUsage is §3.3's accounting report.
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
