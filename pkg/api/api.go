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

// Package api exposes ODE's REST surface. Every route except /health sits
// behind auth.Middleware, so a handler can assume a validated token carrying
// the required realm role (SPEC §3.1).
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/mcp"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

type Config struct {
	RequiredRealmRole string
	CorsOrigins       []string
	Debug             bool
}

type Deps struct {
	Ontology   *ontology.Repository
	Devices    *devices.Service
	Timeseries TimeseriesReader
	Profiler   *profiler.Profiler
	Selection  *selection.Resolver

	// M3. Chat and Admin arrive together: a chat engine without an admin service
	// cannot enforce §3.3, and chat.New refuses to be built without one.
	Chat  *chat.Engine
	Admin *admin.Service
	// MCP is the tool surface over its second transport, mounted only when a
	// provider that needs it is configured.
	MCP http.Handler
}

// NewRouter wires the ODE HTTP surface.
func NewRouter(cfg Config, deps Deps) *gin.Engine {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	if cfg.Debug {
		r.Use(gin.Logger())
	}
	if len(cfg.CorsOrigins) > 0 {
		r.Use(cors.New(cors.Config{
			AllowOrigins:     cfg.CorsOrigins,
			AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
			AllowHeaders:     []string{"Authorization", "Content-Type"},
			AllowCredentials: true,
			MaxAge:           12 * time.Hour,
		}))
	}

	// Unauthenticated: liveness only. It must not touch the platform, or a
	// device-repository outage would take ODE's pods down with it.
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	secured := r.Group("/", auth.Middleware(cfg.RequiredRealmRole))

	secured.GET("/session", handleSession(deps))

	ont := secured.Group("/ontology")
	ont.GET("/aspect-tree", handleAspectTree(deps.Ontology))
	ont.GET("/aspect-nodes", handleAspectNodes(deps.Ontology))
	ont.GET("/functions", handleFunctions(deps.Ontology))
	ont.GET("/characteristics", handleCharacteristics(deps.Ontology))
	ont.GET("/concepts", handleConcepts(deps.Ontology))
	ont.GET("/device-classes", handleDeviceClasses(deps.Ontology))

	dev := secured.Group("/devices")
	dev.GET("", handleListDevices(deps.Devices))
	dev.GET("/:id", handleGetDevice(deps.Devices))

	// M2. Served whenever a resolver is configured, which needs only the ontology
	// and the device repository: a deployment without a timescale-wrapper URL still
	// resolves an intent to series, it just cannot rank them by availability.
	if deps.Selection != nil {
		secured.POST("/selection", handleSelection(deps.Selection))
	}

	// M1a and M1b. The routes stay off the router entirely when the profiler is
	// not configured, so a deployment without a timeseries URL answers 404
	// rather than panicking on the first request.
	if deps.Timeseries != nil {
		ts := secured.Group("/timeseries")
		ts.GET("/availability", handleAvailability(deps.Timeseries))
		ts.GET("/usage", handleUsage(deps.Timeseries))
	}
	// The WebSocket is ODE's streaming surface: the profiler operations below, and
	// the chat exchange of §5.7. Registered whenever either is configured, because a
	// deployment may have one without the other.
	//
	// It is not behind auth.Middleware: a browser cannot set an Authorization header
	// on a WebSocket handshake, so the handler reads the token from the subprotocol
	// or the query and enforces the realm role itself. Everything else about §3.1 is
	// unchanged — the gateway validates, ODE authorises.
	if deps.Profiler != nil || deps.Chat != nil {
		r.GET("/ws", handleWebSocket(cfg, deps))
	}

	if deps.Profiler != nil {
		// Kept off /profiles to avoid a static segment beside the :id wildcard.
		secured.GET("/quick-profiles", handleQuickProfiles(deps.Devices, deps.Profiler))

		profiles := secured.Group("/profiles")
		profiles.POST("", handleCreateProfiles(deps.Devices, deps.Profiler))
		profiles.GET("/:id", handleGetProfile(deps.Profiler))
		profiles.GET("/:id/projection", handleProjection(deps.Profiler))
		profiles.GET("/:id/sessions", handleSessions(deps.Profiler))
		// Developer action only, never an LLM tool (§5.8, D21).
		profiles.POST("/:id/overrides", handleCreateOverride(deps.Profiler))
	}

	// M3. Chat, the tool surface and the admin controls (§3.2, §3.3, §5.7, §5.8).
	if deps.Chat != nil {
		secured.GET("/llm/providers", handleListProviders(deps.Chat))
		// The §5.8 table, including the tools this build does not implement and the
		// capabilities that deliberately have none. Readable by any developer:
		// knowing what the assistant may do is not privileged information.
		secured.GET("/llm/tools", handleListTools(deps.Chat))

		sessions := secured.Group("/chat/sessions")
		sessions.POST("", handleCreateChatSession(deps.Chat))
		sessions.GET("", handleListChatSessions(deps.Chat))
		sessions.GET("/:id", handleGetChatSession(deps.Chat))
		sessions.DELETE("/:id", handleDeleteChatSession(deps.Chat))
		// Sending a message and resolving a confirmation are on the WebSocket, not
		// here: both stream, and an exchange outlives any one request (see ws_chat.go).
		//
		// The developer's tier control (§3.2). No LLM tool exists for this.
		sessions.PUT("/:id/tier", handleSetTier(deps.Chat))
		sessions.GET("/:id/tier-changes", handleTierAudit(deps.Chat))
	}

	if deps.Admin != nil {
		// The realm role gate is on the group, so a route added here later cannot
		// forget it.
		adminGroup := secured.Group("/admin", requireAdmin())
		adminGroup.GET("/limits", handleGetLimits(deps.Admin))
		adminGroup.PUT("/limits", handlePutLimits(deps.Admin))
		adminGroup.GET("/limits/:sub", handleGetSubjectLimits(deps.Admin))
		adminGroup.PUT("/limits/:sub", handlePutLimits(deps.Admin))
		adminGroup.GET("/usage", handleAdminUsage(deps.Admin))
		adminGroup.GET("/tool-calls", handleToolAudit(deps.Admin))
	}

	if deps.MCP != nil {
		// Not under `secured`: the MCP handler authenticates itself, because it has
		// to read the session header and resolve a tier before any tool is offered.
		// It uses the same token parsing and the same required realm role.
		r.Any(mcp.Path, gin.WrapH(deps.MCP))
	}

	return r
}

// handleSession lets the SPA confirm who the backend thinks it is talking to,
// and learn what this deployment can do.
//
// The exposure tier reported here is the *default* a new session starts at, plus
// the ceiling this user may raise one to (§3.3). It is not a live tier: a tier is
// session-scoped (§3.2), so the SPA reads the real one from the session itself.
func handleSession(deps Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		body := gin.H{
			"user_id":  token.Sub,
			"username": token.Username,
			"email":    token.Email,
			"roles":    token.GetRoles(),
			"is_admin": token.IsAdmin(),
			// The default a new session starts at (§3.2).
			"exposure_tier": tools.DefaultTier,
			"features": gin.H{
				"profiler":  deps.Profiler != nil,
				"selection": deps.Selection != nil,
				"chat":      deps.Chat != nil,
				"mcp":       deps.MCP != nil,
			},
		}

		if deps.Admin != nil {
			effective, err := deps.Admin.Effective(c.Request.Context(), token.Sub)
			if err == nil {
				body["max_exposure_tier"] = effective.MaxTierOr()
				body["limits"] = effective
				if spend, err := deps.Admin.Spend(c.Request.Context(), token.Sub,
					effective.PeriodDuration()); err == nil {
					body["spend"] = spend
				}
			}
		}
		if deps.Chat != nil {
			body["providers"] = deps.Chat.Providers().Describe()
		}

		c.JSON(http.StatusOK, body)
	}
}

func handleAspectTree(repo *ontology.Repository) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap, err := repo.Snapshot(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"tree": ontology.AspectTree(snap.AspectNodes)})
	}
}

func handleAspectNodes(repo *ontology.Repository) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap, err := repo.Snapshot(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"aspect_nodes": snap.AspectNodes})
	}
}

// handleFunctions serves measuring functions by default. Semantic selection
// (§5.2) resolves an intent to a measuring function, so that is the list the
// SPA needs first; ?rdf_type=controlling asks for the other.
func handleFunctions(repo *ontology.Repository) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap, err := repo.Snapshot(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondUpstream(c, err)
			return
		}
		switch c.Query("rdf_type") {
		case "", "measuring":
			c.JSON(http.StatusOK, gin.H{"functions": snap.MeasuringFunctions, "rdf_type": "measuring"})
		case "controlling":
			c.JSON(http.StatusOK, gin.H{"functions": snap.ControllingFunctions, "rdf_type": "controlling"})
		case "all":
			all := append(append([]models.Function{}, snap.MeasuringFunctions...), snap.ControllingFunctions...)
			c.JSON(http.StatusOK, gin.H{"functions": all, "rdf_type": "all"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "rdf_type must be one of measuring, controlling, all",
			})
		}
	}
}

func handleCharacteristics(repo *ontology.Repository) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap, err := repo.Snapshot(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"characteristics": snap.Characteristics})
	}
}

func handleConcepts(repo *ontology.Repository) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap, err := repo.Snapshot(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"concepts": snap.Concepts})
	}
}

func handleDeviceClasses(repo *ontology.Repository) gin.HandlerFunc {
	return func(c *gin.Context) {
		snap, err := repo.Snapshot(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"device_classes": snap.DeviceClasses})
	}
}

func handleListDevices(svc *devices.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		opts, err := devices.ParseListOptions(c.Request.URL.Query())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// D5: read as the caller, never as a service account.
		result, err := svc.List(auth.Bearer(c), opts)
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func handleGetDevice(svc *devices.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		device, err := svc.Get(auth.Bearer(c), c.Param("id"), models.Read)
		if err != nil {
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, device)
	}
}

// respondUpstream forwards the platform's own authorisation verdict rather
// than flattening everything to 500. A 403 from the device repository means
// the user may not see that device, and the SPA has to be able to tell that
// apart from ODE being broken.
func respondUpstream(c *gin.Context, err error) {
	var devErr *devices.UpstreamError
	var ontErr *ontology.UpstreamError
	var tsErr *timeseries.UpstreamError

	code := 0
	switch {
	case errors.As(err, &devErr):
		code = devErr.Code
	case errors.As(err, &ontErr):
		code = ontErr.Code
	case errors.As(err, &tsErr):
		code = tsErr.Code
	}

	switch code {
	case http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest:
		c.JSON(code, gin.H{"error": http.StatusText(code)})
	case http.StatusUnauthorized:
		// Deliberately not forwarded as 401. The caller already authenticated
		// with ODE, so a 401 from the platform means ODE failed to
		// authenticate itself upstream — a configuration or integration fault,
		// not an expired session. Forwarding it sends the SPA into a pointless
		// re-login loop and hides the actual cause.
		c.JSON(http.StatusBadGateway, gin.H{
			"error": "ODE could not authenticate against the platform",
			"hint":  "check device_repo_url and that the caller's token is forwarded upstream",
		})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream platform request failed"})
	}
}
