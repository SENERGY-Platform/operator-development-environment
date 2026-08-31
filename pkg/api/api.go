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

// Package api exposes ODE's REST surface. Every route except /health and /doc
// sits behind auth.Middleware, so a handler can assume a validated token carrying
// the required realm role (§3.1).
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	ginmw "github.com/SENERGY-Platform/gin-middleware"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/interpret"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/mcp"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
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

	// Imports is the second kind of operator input
	// (docs/imports-as-operator-inputs.md). Absent when no
	// device_selection_url is configured, in which case semantic selection reports
	// devices only and says so, and the three import tools stay
	// declared-but-unavailable.
	Imports *imports.Service

	// M3. Chat and Admin arrive together: a chat engine without an admin service
	// cannot enforce §3.3, and chat.New refuses to be built without one.
	Chat  *chat.Engine
	Admin *admin.Service
	// MCP is the tool surface over its second transport, mounted only when a
	// provider that needs it is configured.
	MCP http.Handler

	// M4. Absent when no jupyterhub_url is configured, in which case the kernel
	// routes are not served and run_code has no executor.
	Kernel *kernel.Service

	// M5. The exploration pane (§5.9). Present whenever a timescale-wrapper is,
	// because a chart is a read of series values plus the profiler's annotations
	// over them.
	Charts *charts.Service

	// M6. Multi-device conditional patterns (§5.5). Needs the profiler for the
	// activity_pattern every state series comes from and the resolver for the
	// aspect-scoped proposals, so it is present whenever both are.
	Relations *relations.Service

	// M7. The Code pane and GitHub (§5.11). Needs a Hub — the working copy lives in
	// the developer's pod — a GitHub OAuth app and an encryption key for the token,
	// so it is absent unless all three are configured.
	Repo *repo.Service

	// M8. Ray and MLflow (§5.12). Needs a Ray cluster, a tracking server, a Hub and
	// a repo surface: the job package is the committed state of a working copy that
	// lives in the developer's pod, so it is absent unless all four are configured.
	Experiments *experiments.Service
	// Interpretations is M9's result interpretation (§5.13). Optional in the shape
	// everything else here is: without an LLM provider there is no chat engine, and
	// without one there is nowhere to interpret a run into.
	Interpretations *interpret.Service
}

// NewRouter wires the ODE HTTP surface.
//
//	@title			Operator Development Environment
//	@description	ODE helps a developer build a SENERGY analytics operator: it profiles
//	@description	the series behind a device, resolves a semantic intent to concrete
//	@description	series, and runs code in the developer's own kernel — with an LLM
//	@description	assistant whose reach over platform data is bounded by an exposure
//	@description	tier (§3.2).
//	@description
//	@description	Every route except /health and /doc requires a bearer token carrying
//	@description	the configured realm role. The platform API gateway validates the
//	@description	token; ODE authorises on it (§3.1, D5). Routes appear only when
//	@description	the capability behind them is configured, so a deployment without a
//	@description	timescale-wrapper answers 404 on the profiler rather than 500.
//	@version		1.0
//	@license.name	Apache 2.0
//	@license.url	http://www.apache.org/licenses/LICENSE-2.0.html
//	@basePath		/
//
//	@securityDefinitions.apikey	Bearer
//	@in							header
//	@name						Authorization
//	@description				A Keycloak bearer token, as `Bearer <jwt>`.
func NewRouter(cfg Config, deps Deps) *gin.Engine {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	// Access logging and panic recovery come from gin-middleware, so both land in
	// the structured log with the same attribute keys every other SENERGY service
	// uses. Unlike the gin.Logger it replaces, this runs in every mode: a
	// production deployment without a request log cannot answer "who called what,
	// and did it 500".
	//
	// /health is skipped. It is the kubelet probe, it answers from memory, and
	// logging it buries the traffic that carries information.
	r := gin.New()
	logger := slog.Default()
	r.Use(
		ginmw.StructRecoveryHandler(logger, ginmw.DefaultRecoveryFunc),
		ginmw.StructLoggerHandler(logger, attributes.Provider, []string{"/health"}, nil),
	)
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
	r.GET("/health", handleHealth())

	// Unauthenticated for the reason given in doc.go.
	r.GET("/doc", handleDoc())

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
		if deps.Profiler != nil {
			// The export half sits here rather than under /profiles because it answers
			// the same question /availability does — where is there data — and it needs
			// the profiler rather than the bare client: the count has to be addressed
			// by column name, and those come from the export definition.
			ts.GET("/export-data", handleExportData(deps.Profiler))
		}
	}
	// The WebSocket is ODE's streaming surface: the profiler operations below, the
	// relational pass of §5.5, the chat exchange of §5.7, and kernel execution (§5.6). Registered whenever any of
	// them is configured, because a deployment may have one without the others.
	//
	// It is not behind auth.Middleware: a browser cannot set an Authorization header
	// on a WebSocket handshake, so the handler reads the token from the subprotocol
	// or the query and enforces the realm role itself. Everything else about §3.1 is
	// unchanged — the gateway validates, ODE authorises.
	if deps.Profiler != nil || deps.Chat != nil || deps.Kernel != nil || deps.Relations != nil {
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

	// M5. The exploration pane (§5.9, §5.10). On HTTP rather than the WebSocket:
	// one chart is one batched, point-capped query, so it answers in seconds and
	// needs neither cancellation nor a second code path.
	if deps.Charts != nil {
		chartRoutes := secured.Group("/charts")
		chartRoutes.POST("", handleCreateChart(deps.Charts))
		chartRoutes.GET("", handleListCharts(deps.Charts))
		chartRoutes.GET("/:id", handleGetChart(deps.Charts))
		chartRoutes.DELETE("/:id", handleDeleteChart(deps.Charts))
		// The only route that hands series values to a client, and it hands them to
		// the developer under their own token. The exposure tier bounds an LLM
		// context, not a developer's view of their own data (§3.2).
		chartRoutes.GET("/:id/data", handleChartData(deps.Charts))
		// Developer action only, never an LLM tool (§5.8, D21) — the same overlay
		// the profiler route above writes to.
		chartRoutes.POST("/:id/confirmations", handleConfirmChart(deps.Charts))
	}

	// M6. Multi-device conditional patterns (§5.5). Proposing sets is metadata and
	// answers in seconds, so it is HTTP; the pass itself is also on the WebSocket as
	// `relate`, because it profiles every participating service before it reads.
	if deps.Relations != nil {
		relationRoutes := secured.Group("/relations")
		// Static segments before the :id wildcard, so gin does not have to choose
		// between them.
		relationRoutes.GET("/candidate-sets", handleCandidateSets(deps.Relations))
		relationRoutes.GET("/rule-decisions", handleRuleDecisions(deps.Relations))
		relationRoutes.POST("", handleCreateRelation(deps.Relations))
		relationRoutes.GET("/:id", handleGetRelation(deps.Relations))
		// Developer action only, never an LLM tool (§5.8, D21) — the same boundary the
		// profiler's override route and the chart's confirmation route sit behind.
		relationRoutes.POST("/:id/rule-decisions", handleDecideRule(deps.Relations))
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
		// The developer's own name for a conversation. Its own sub-resource for the
		// same reason the tier is: a PUT of the whole session would be a second way
		// to move a tier, and that one would not audit it.
		sessions.PUT("/:id/title", handleRenameChatSession(deps.Chat))
		// Which working context the conversation acts in, changed after the fact. Only
		// with a repository surface: without one there are no workbenches to move
		// between, and the route would answer 404 to every id there is.
		if deps.Repo != nil {
			sessions.PUT("/:id/workbench", handleMoveChatSession(deps.Chat, deps.Repo))
		}
		// Sending a message and resolving a confirmation are on the WebSocket, not
		// here: both stream, and an exchange outlives any one request (see ws_chat.go).
		//
		// The developer's tier control (§3.2). No LLM tool exists for this.
		sessions.PUT("/:id/tier", handleSetTier(deps.Chat))
		sessions.PUT("/:id/auto-run", handleSetAutoRun(deps.Chat))
		sessions.GET("/:id/tier-changes", handleTierAudit(deps.Chat))
	}

	// M4. The developer's own pod (§5.6). Executing is on the WebSocket, because a
	// cell streams and outlives a request; everything that answers once is here.
	if deps.Kernel != nil {
		kern := secured.Group("/kernel")
		kern.GET("", handleKernelStatus(deps.Kernel))
		kern.POST("", handleKernelEnsure(deps.Kernel))
		kern.DELETE("", handleKernelShutdown(deps.Kernel))
		kern.POST("/restart", handleKernelRestart(deps.Kernel))
		kern.POST("/interrupt", handleKernelInterrupt(deps.Kernel))
		kern.GET("/files", handleKernelFiles(deps.Kernel))
	}

	// M7. The repository surface (§5.11). On HTTP throughout: a git command answers
	// in seconds and a file edit in milliseconds, so none of it needs the streaming
	// surface — a clone is the one slow operation and it is still one request.
	//
	// There is no unauthenticated OAuth callback. GitHub returns the browser to the
	// SPA, which posts the code here with its own platform token, so every route in
	// this group sits behind the same realm-role gate as the rest of ODE.
	if deps.Repo != nil {
		// The working contexts themselves. Everything that acts *in* one names it
		// with a `workbench` query parameter on the routes below, which is what keeps
		// those routes static and lets a request that names none mean "my only one".
		benches := secured.Group("/workbenches")
		benches.GET("", handleListWorkbenches(deps.Repo))
		benches.POST("", handleCreateWorkbench(deps.Repo))
		benches.PUT("/:id", handleRenameWorkbench(deps.Repo))
		benches.DELETE("/:id", handleDeleteWorkbench(deps.Repo))

		repoRoutes := secured.Group("/repo")
		repoRoutes.GET("", handleRepoStatus(deps.Repo))
		repoRoutes.GET("/connection", handleRepoConnection(deps.Repo))
		repoRoutes.POST("/connection/authorize", handleRepoAuthorize(deps.Repo))
		repoRoutes.POST("/connection", handleRepoConnect(deps.Repo))
		repoRoutes.DELETE("/connection", handleRepoDisconnect(deps.Repo))
		repoRoutes.GET("/repositories", handleRepoRepositories(deps.Repo))
		repoRoutes.POST("/repositories", handleRepoCreate(deps.Repo))
		repoRoutes.POST("/link", handleRepoSelect(deps.Repo))
		repoRoutes.DELETE("/link", handleRepoUnlink(deps.Repo))
		repoRoutes.POST("/fetch", handleRepoFetch(deps.Repo))
		repoRoutes.POST("/scaffold", handleRepoScaffold(deps.Repo))
		// The three explicit actions of §5.11 item 5, and the two of item 6. None of
		// them happens as a side effect of anything else.
		repoRoutes.POST("/commit", handleRepoCommit(deps.Repo))
		// A draft is not one of them: it reads the diff and answers with text, and
		// the commit stays the developer's own next action.
		repoRoutes.POST("/commit/message", handleRepoCommitMessage(deps.Repo))
		repoRoutes.POST("/push", handleRepoPush(deps.Repo))
		repoRoutes.POST("/stash", handleRepoStash(deps.Repo))
		repoRoutes.POST("/discard", handleRepoDiscard(deps.Repo))
		// The Code pane (D14). Static segments only, so no wildcard has to compete
		// with them: the path of a file is a query parameter rather than a path
		// segment, which also spares every caller two rounds of escaping.
		repoRoutes.GET("/files", handleRepoFiles(deps.Repo))
		repoRoutes.GET("/files/content", handleRepoReadFile(deps.Repo))
		repoRoutes.PUT("/files/content", handleRepoWriteFile(deps.Repo))
		repoRoutes.DELETE("/files/content", handleRepoDeleteFile(deps.Repo))
		repoRoutes.POST("/files/directory", handleRepoMakeDir(deps.Repo))
	}

	// M8. Ray and MLflow (§5.12, D6). On HTTP throughout: a launch is bounded in
	// seconds and answers once, and the thing that takes hours is the job, which is
	// polled rather than streamed. As with the kernel and repo routes, none of them
	// takes a user parameter.
	if deps.Experiments != nil {
		experimentRoutes := secured.Group("/experiments")
		experimentRoutes.POST("", handleLaunchExperiment(deps.Experiments))
		experimentRoutes.GET("", handleListExperiments(deps.Experiments))
		// The static segment is registered before the :id wildcard, so gin does not
		// have to choose between them — the same order the relation routes use.
		experimentRoutes.GET("/embed", handleExperimentEmbed(deps.Experiments))
		experimentRoutes.GET("/:id", handleGetExperiment(deps.Experiments))
		experimentRoutes.GET("/:id/results", handleExperimentResults(deps.Experiments))
		// Logs have a route and no LLM tool, which is §5.13's "never raw logs" made
		// structural: the developer reads them here, and the tool surface has no
		// method that could reach them.
		experimentRoutes.GET("/:id/logs", handleExperimentLogs(deps.Experiments))
		experimentRoutes.POST("/:id/stop", handleStopExperiment(deps.Experiments))

		// M9 (§5.13). Under the experiment group because an interpretation is a
		// property of a run rather than a resource of its own, and because the
		// ownership check that guards it is the experiment store's.
		if deps.Interpretations != nil {
			experimentRoutes.GET("/:id/interpretation",
				handleGetInterpretation(deps.Interpretations))
			// A developer action with no LLM tool behind it (§5.8, D28).
			experimentRoutes.POST("/:id/interpretation/decision",
				handleDecideProposal(deps.Interpretations))
		}
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

// handleHealth answers liveness.
//
//	@Summary		Liveness
//	@Description	Answers from memory and never touches the platform, so that a
//	@Description	device-repository outage does not take ODE's pods down with it.
//	@Tags			meta
//	@Produce		json
//	@Success		200	{object}	map[string]string	"always {\"status\":\"ok\"}"
//	@Router			/health [get]
func handleHealth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// handleSession lets the SPA confirm who the backend thinks it is talking to,
// and learn what this deployment can do.
//
// The exposure tier reported here is the *default* a new session starts at, plus
// the ceiling this user may raise one to (§3.3). It is not a live tier: a tier is
// session-scoped (§3.2), so the SPA reads the real one from the session itself.
//
//	@Summary		Who am I, and what can this deployment do
//	@Description	Identity from the token, the realm roles behind it, which capabilities
//	@Description	this deployment serves, and — when an admin service is configured —
//	@Description	the caller's limits and spend so far (§3.3).
//	@Tags			meta
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	map[string]interface{}
//	@Failure		401	{object}	map[string]string	"no token, or the required realm role is missing"
//	@Router			/session [get]
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
				"profiler":    deps.Profiler != nil,
				"selection":   deps.Selection != nil,
				"chat":        deps.Chat != nil,
				"mcp":         deps.MCP != nil,
				"kernel":      deps.Kernel != nil,
				"charts":      deps.Charts != nil,
				"relations":   deps.Relations != nil,
				"repo":        deps.Repo != nil,
				"experiments": deps.Experiments != nil,
				// §5.13's automated interpretation, which needs both an experiment
				// surface and a chat engine.
				"result_interpretation": deps.Interpretations != nil,
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
		if deps.Repo != nil {
			// What the GitHub consent screen will ask for, so the SPA can say it before
			// the developer is looking at GitHub's own wording (§5.11 item 1).
			body["repo"] = gin.H{
				"scopes": deps.Repo.Scopes(),
				// Whether the commit box may offer to draft a message. A deployment
				// with no LLM provider serves the repo routes anyway, and a button
				// that could only ever answer 503 is worse than no button.
				"commit_message_draft": deps.Repo.DraftsAvailable(),
			}
		}
		if deps.Experiments != nil {
			// Where a browser should open the two services, and whether ODE can mint a
			// credential that outlives the session (§3.1 item 6) — so the pane can say
			// what a long run will survive before the developer starts one.
			body["experiments"] = gin.H{
				"ray_url":          deps.Experiments.DashboardURL(),
				"mlflow_url":       deps.Experiments.TrackingUIURL(),
				"scoped_job_token": deps.Experiments.ExchangeConfigured(),
			}
		}
		if deps.Kernel != nil {
			// The workspace path is reported so the SPA can say where a file it lists
			// actually lives, which is the difference between "somewhere in the pod"
			// and "on storage that survives the pod".
			body["kernel"] = gin.H{
				"workspace": deps.Kernel.Workspace(),
				"kernel":    deps.Kernel.KernelName(),
			}
		}

		c.JSON(http.StatusOK, body)
	}
}

// @Summary		The aspect hierarchy as a tree
// @Tags			ontology
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"the device repository could not be read"
// @Router			/ontology/aspect-tree [get]
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

// @Summary		Aspect nodes, flat
// @Tags			ontology
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string][]models.AspectNode
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string
// @Router			/ontology/aspect-nodes [get]
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
//
//	@Summary		Functions, measuring by default
//	@Description	Semantic selection resolves an intent to a measuring function (§5.2), so that is the default list.
//	@Tags			ontology
//	@Produce		json
//	@Security		Bearer
//	@Param			rdf_type	query		string	false	"which functions to return"	Enums(measuring, controlling, all)	default(measuring)
//	@Success		200			{object}	map[string]interface{}
//	@Failure		400			{object}	map[string]string	"unknown rdf_type"
//	@Failure		401			{object}	map[string]string
//	@Failure		502			{object}	map[string]string
//	@Router			/ontology/functions [get]
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

// @Summary		Characteristics
// @Tags			ontology
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string
// @Router			/ontology/characteristics [get]
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

// @Summary		Concepts
// @Tags			ontology
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string][]models.Concept
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string
// @Router			/ontology/concepts [get]
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

// @Summary		Device classes
// @Tags			ontology
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string][]models.DeviceClass
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string
// @Router			/ontology/device-classes [get]
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

// @Summary		List the caller's devices
// @Description	Read on behalf of the caller, never as a service account (D5), so
// @Description	this returns exactly what that user may see.
// @Tags			devices
// @Produce		json
// @Security		Bearer
// @Param			search	query		string	false	"free-text filter"
// @Param			limit	query		int		false	"page size"
// @Param			offset	query		int		false	"page offset"
// @Success		200		{object}	map[string]interface{}
// @Failure		400		{object}	map[string]string	"unparseable list options"
// @Failure		401		{object}	map[string]string
// @Failure		502		{object}	map[string]string
// @Router			/devices [get]
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

// @Summary		One device
// @Tags			devices
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"device id"
// @Success		200	{object}	models.Device
// @Failure		401	{object}	map[string]string
// @Failure		403	{object}	map[string]string	"the platform refused this user the device"
// @Failure		404	{object}	map[string]string
// @Failure		502	{object}	map[string]string
// @Router			/devices/{id} [get]
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
