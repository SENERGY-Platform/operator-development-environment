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

package pkg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	devicerepo "github.com/SENERGY-Platform/device-repository/lib/client"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/configuration"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/database"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/identifiers"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/interpret"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/mcp"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// Start brings up the HTTP server and returns a WaitGroup that completes once
// it has shut down. Cancelling ctx triggers a graceful shutdown.
func Start(ctx context.Context, config configuration.Config) (*sync.WaitGroup, error) {
	if err := validate(config); err != nil {
		return nil, err
	}

	ttl, err := time.ParseDuration(config.OntologyCacheTtl)
	if err != nil {
		return nil, fmt.Errorf("config: ontology_cache_ttl: %w", err)
	}
	invalidateInterval, err := time.ParseDuration(config.OntologyInvalidateInt)
	if err != nil {
		return nil, fmt.Errorf("config: ontology_invalidate_interval: %w", err)
	}

	// Devices: one shared client is enough, because ListExtendedDevices and
	// ReadExtendedDevice set the Authorization header from their token
	// argument themselves (§5.1).
	deviceClient := devicerepo.NewClient(config.DeviceRepoUrl, nil)

	// Ontology: the ontology methods take no token and set no header, relying
	// on the client-level auth closure instead. Reaching the device repository
	// through the API gateway therefore needs a client bound to a real token,
	// or every ontology read is rejected with 401 before it arrives.
	newOntologyClient := func(token string) ontology.Client {
		return devicerepo.NewClient(config.DeviceRepoUrl, func() (string, error) {
			return token, nil
		})
	}

	ontologyRepo := ontology.New(newOntologyClient, ontology.Options{
		TTL:                ttl,
		InvalidateInterval: invalidateInterval,
	})

	deviceService := devices.New(deviceClient)

	// Postgres, if configured. Optional in the same way the timescale-wrapper is:
	// without it ODE runs the in-memory stores, and validate() has already said
	// what that costs.
	var db *database.DB
	if config.PostgresUrl != "" {
		db, err = database.Connect(ctx, config.PostgresUrl.Value(), database.Options{
			MaxConns: int32(config.PostgresMaxConns),
		})
		if err != nil {
			return nil, err
		}
		if err := database.Migrate(ctx, db); err != nil {
			db.Close()
			return nil, err
		}
	}

	// The ontology index is built once and memoised per snapshot. It is hoisted
	// out of the profiler's block below because semantic selection needs it too —
	// the unit and the completeness of a resolved variable come from it — and
	// selection is served whether or not a timescale-wrapper is configured.
	ontologyIndex := profiler.NewSnapshotOntology(ontologyRepo)

	deps := api.Deps{
		Ontology: ontologyRepo,
		Devices:  deviceService,
	}

	// Imports as the second kind of operator input (PLAN). Optional the way the
	// timescale-wrapper is: without a device_selection_url a resolution finds
	// devices exactly as before and says in its notes that the import half was not
	// searched, rather than pretending the platform has no imports.
	//
	// Built before the profiler because the profiler reads through it: an export is
	// addressed by id and queried by column name, and the column names live in
	// analytics-serving.
	importService, err := startImports(config)
	if err != nil {
		return nil, err
	}
	deps.Imports = importService

	// Held separately from deps.Timeseries, which is the narrow reader interface the
	// HTTP routes need. The tool surface additionally needs Query, for the tier-L2
	// preview, so it gets the concrete client.
	var timeseriesClient *timeseries.Client

	// The timeseries client and the profiler are optional so that a deployment
	// without a timescale-wrapper URL still serves the M0 surface instead of
	// failing to start. validate() warns about it.
	if config.TimescaleWrapperUrl != "" {
		timeout, err := time.ParseDuration(config.TimeseriesRequestTimeout)
		if err != nil {
			return nil, fmt.Errorf("config: timeseries_request_timeout: %w", err)
		}
		timeseriesClient = timeseries.New(config.TimescaleWrapperUrl, timeseries.Options{Timeout: timeout})

		// Separate from the client default above: value reads legitimately take far
		// longer than metadata probes, and one timeout cannot serve both.
		readTimeout, err := time.ParseDuration(config.ProfilerReadTimeout)
		if err != nil {
			return nil, fmt.Errorf("config: profiler_read_timeout: %w", err)
		}

		// The override overlay is an empirical record of human confirmation
		// (§5.4.3), so it goes to Postgres when there is one. Computed profiles stay
		// in memory either way: losing one costs a recomputation, and they are large.
		profileStore := profilerStore(db)

		profilerService, err := profiler.New(
			timeseriesClient,
			ontologyIndex,
			profileStore,
			profiler.Options{
				RawWindowMaxDays:   int(config.ProfilerRawWindowDays),
				RawWindowMaxPoints: int(config.ProfilerRawWindowPoints),
				CoverageWindowDays: int(config.ProfilerCoverageWindowDays),
				Concurrency:        int(config.ProfilerConcurrency),
				LocalTimezone:      config.ProfilerLocalTimezone,
				ReadTimeout:        readTimeout,
				// The export half (§5.3). Nil unless analytics-serving is configured, and
				// the export-addressed calls then refuse with ErrNoExportSource rather than
				// querying a table whose column names ODE would have had to invent.
				Exports: exportSourceOrNil(config, importService),
			},
		)
		if err != nil {
			return nil, err
		}
		deps.Timeseries = timeseriesClient
		deps.Profiler = profilerService

		// M5. The exploration pane (§5.9). It shares the profiler's store rather than
		// keeping one of its own: the annotations a chart draws are the profiler's
		// detections, and a confirmation taken from a chart has to land in the same
		// append-only overlay a confirmation taken from a profile does (§5.10). Two
		// stores would mean two records of the same human decision.
		lookback, err := time.ParseDuration(config.ChartDefaultLookback)
		if err != nil {
			return nil, fmt.Errorf("config: chart_default_lookback: %w", err)
		}
		chartService, err := charts.New(charts.Deps{
			Timeseries:      timeseriesClient,
			Devices:         deviceService,
			Ontology:        ontologyIndex,
			Profiles:        profileStore,
			Store:           charts.NewMemoryStore(int(config.ChartMaxPerUser)),
			IDs:             identifiers.New(),
			MaxPoints:       int(config.ChartMaxPoints),
			MaxAnnotations:  int(config.ChartMaxAnnotations),
			DefaultLookback: lookback,
		})
		if err != nil {
			return nil, err
		}
		deps.Charts = chartService
	}

	// Semantic selection (§5.2). The ranker is the profiler, which may be absent;
	// selection then resolves an intent to series without the availability-based
	// order, and says so in the response rather than failing.
	resolver, err := selection.New(ontologyRepo, ontologyIndex, deviceService, rankerOrNil(deps.Profiler),
		importsOrNil(importService),
		selection.Options{
			Concurrency: int(config.SelectionConcurrency),
			MaxCriteria: int(config.SelectionMaxCriteria),
			DeviceLimit: config.SelectionDeviceLimit,
		})
	if err != nil {
		return nil, err
	}
	deps.Selection = resolver

	// M6. The relational profiler (§5.5). It needs both halves: the profiler for the
	// activity_pattern every state series is derived from, and the resolver for the
	// aspect-scoped proposals. A deployment without a timescale-wrapper has no
	// profiler, so the routes stay off the router and both M6 tools stay
	// declared-but-unavailable — the same degradation the rest of ODE does.
	if deps.Profiler != nil {
		relationLookback, err := time.ParseDuration(config.RelationDefaultLookback)
		if err != nil {
			return nil, fmt.Errorf("config: relation_default_lookback: %w", err)
		}
		readTimeout, err := time.ParseDuration(config.ProfilerReadTimeout)
		if err != nil {
			return nil, fmt.Errorf("config: profiler_read_timeout: %w", err)
		}
		relationService, err := relations.New(relations.Deps{
			Timeseries: timeseriesClient,
			Devices:    deviceService,
			Ontology:   ontologyRepo,
			Selection:  resolver,
			Profiler:   deps.Profiler,
			// The same index selection and the profiler use. A graph reaches devices the
			// aspect resolution never saw, and their units have to come from somewhere.
			OntologyIndex: ontologyIndex,
			// The decision log goes to Postgres when there is one, for the reason the
			// override overlay does (§5.4.3): a relation profile is recomputable and a
			// developer's verdict on a rule is not.
			Store:              relationStore(db, int(config.RelationMaxStored)),
			IDs:                identifiers.New(),
			MaxMembers:         int(config.RelationMaxMembers),
			MaxBuckets:         int(config.RelationMaxBuckets),
			MaxRules:           int(config.RelationMaxRules),
			MaxGraphNeighbours: int(config.RelationMaxGraphNeighbours),
			DefaultLookback:    relationLookback,
			ReadTimeout:        readTimeout,
			DeviceLimit:        config.SelectionDeviceLimit,
		})
		if err != nil {
			return nil, err
		}
		deps.Relations = relationService
	}

	// M4 before M3, because the tool surface is built inside startM3 and run_code
	// needs the kernel service to exist by then. A deployment without a
	// jupyterhub_url gets a nil service and the tool stays declared-but-unavailable.
	kernelService, err := startM4(ctx, config)
	if err != nil {
		return nil, err
	}
	deps.Kernel = kernelService

	// M7 before M3 for the same reason: write_file is registered inside startM3 and
	// needs the repo service to exist by then.
	repoService, err := startM7(config, db, kernelService)
	if err != nil {
		return nil, err
	}
	deps.Repo = repoService

	// M8 before M3, for the reason M4 and M7 are: launch_experiment and
	// get_experiment_results are registered inside startM3 and need the service to
	// exist by then. It sits after M7 because it needs the repo surface — a run is
	// submitted from a commit, not from a working copy.
	experimentService, err := startM8(config, db, kernelService, repoService)
	if err != nil {
		return nil, err
	}
	deps.Experiments = experimentService

	// M3: providers, the tool surface, the dispatcher, chat and the admin controls.
	if err := startM3(ctx, config, &deps, db, ontologyRepo, deviceService, timeseriesClient, kernelService, repoService, experimentService); err != nil {
		return nil, err
	}

	// M9: result interpretation (§5.13). After M3 rather than inside startM8,
	// because it needs both halves — the experiment surface to summarise a run and
	// the chat engine to interpret it into — and the engine does not exist until
	// startM3 has run. It is the one piece of wiring that could not live in the
	// milestone's own start function.
	stopM9, err := startM9(ctx, config, &deps, db, experimentService)
	if err != nil {
		return nil, err
	}

	router := api.NewRouter(
		api.Config{
			RequiredRealmRole: config.RequiredRealmRole,
			CorsOrigins:       config.CorsOrigins,
			Debug:             config.Debug,
		},
		deps,
	)

	server := &http.Server{
		Addr:              ":" + config.ApiPort,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("api listening", "port", config.ApiPort)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("api server stopped", "error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("api shutdown", "error", err)
		}
		// The two M9 loops, before the database closes under them. Waited for rather
		// than merely cancelled: the delivery loop may be halfway through injecting a
		// summary and running a turn, and pulling the connection pool out from under
		// it would leave the summary in the conversation with nothing that answered
		// it. They stop at the next safe point because their context is ctx.
		stopM9(shutdownCtx)
		// After the server, so no in-flight request loses its connection mid-write.
		db.Close()
	}()

	return wg, nil
}

// profilerStore picks the profile store.
//
// MemoryStore when there is no database; otherwise the same in-memory store for
// computed profiles with the override overlay persisted. The split is deliberate:
// a profile is a recomputable artifact, an override is developer input that
// §5.4.3 calls an empirical record, and only one of the two is worth a table.
func profilerStore(db *database.DB) profiler.Store {
	if db == nil {
		return profiler.NewMemoryStore()
	}
	return profiler.NewOverlayStore(profiler.NewMemoryStore(), profiler.NewPostgresOverrides(db))
}

// relationStore picks the store for the relational profiler.
//
// Relation profiles stay in memory either way; only the rule decision log moves to
// Postgres. The same split the profiler makes, for the same reason: a profile is a
// reproducible artifact and a decision is an empirical record (§5.4.3, §5.10).
func relationStore(db *database.DB, maxStored int) relations.Store {
	memory := relations.NewMemoryStore(maxStored)
	if db == nil {
		return memory
	}
	return relations.NewOverlayStore(memory, relations.NewPostgresDecisions(db))
}

// startM3 wires the LLM surface. It is a function rather than inline because the
// wiring has real branching: any subset of four providers may be configured, and
// a deployment with none serves M0–M2 unchanged.
func startM3(
	ctx context.Context,
	config configuration.Config,
	deps *api.Deps,
	db *database.DB,
	ontologyRepo *ontology.Repository,
	deviceService *devices.Service,
	timeseriesClient *timeseries.Client,
	kernelService *kernel.Service,
	repoService *repo.Service,
	experimentService *experiments.Service,
) error {
	pricing := llm.NewPricing(config.LlmCurrency, modelPrices(config.LlmPricing)...)

	providers, err := buildProviders(ctx, config, pricing)
	if err != nil {
		return err
	}
	if providers.Len() == 0 {
		slog.Warn("no llm provider is configured: the chat, tool and admin routes are not served",
			"hint", "set anthropic_api_key, openai_api_key, compatible_base_url or claude_cli_enabled")
		return nil
	}

	// The admin service comes first: chat.New refuses to build without one, because
	// §3.3's caps are not an optional extra.
	adminStore := adminStore(db)
	adminService, err := admin.New(adminStore, pricing)
	if err != nil {
		return err
	}

	chatStore := chatStore(db)
	ids := identifiers.New()

	// The engine is needed by the tool surface — as the selection sink, and as the
	// log of what a session created — and the tool surface is needed by the engine,
	// so the registry is built against a holder the engine is written into once it
	// exists. A tool executor only runs during a dispatch, which is always after
	// Start has returned, so the indirection is never observed as a nil.
	sink := &sessionSink{}

	registry, err := tools.NewSurface(tools.Deps{
		Ontology:            ontologyRepo,
		Devices:             deviceService,
		Imports:             toolImportsOrNil(deps.Imports),
		Timeseries:          timeseriesOrNil(timeseriesClient),
		Profiler:            profilerOrNil(deps.Profiler),
		Selection:           selectionOrNil(deps.Selection),
		SelectionSink:       sink,
		Creations:           sink,
		Kernel:              kernelOrNil(kernelService),
		Charts:              chartsOrNil(deps.Charts),
		Relations:           relationsOrNil(deps.Relations),
		Repo:                repoOrNil(repoService),
		Experiments:         experimentsOrNil(experimentService),
		ProfileTokenBudget:  int(config.ToolProfileTokenBudget),
		ProfileMaxProfiles:  int(config.ToolProfileMaxProfiles),
		QuickTokenBudget:    int(config.ToolQuickTokenBudget),
		PreviewMaxPoints:    int(config.ToolPreviewMaxPoints),
		RelationTokenBudget: int(config.ToolRelationTokenBudget),
		RelationMaxRules:    int(config.ToolRelationMaxRules),
		DeviceLimit:         config.SelectionDeviceLimit,

		RunCodeMaxOutputBytes: int(config.ToolRunCodeMaxOutputBytes),
	})
	if err != nil {
		return err
	}

	// The per-user ceiling on open workbenches (§3.3). Installed here rather than
	// when the repo surface was built, because that happens first — write_file has
	// to be registrable by the time this function runs.
	if repoService != nil {
		repoService.UseLimits(adminService)
	}

	dispatcher, err := tools.NewDispatcher(registry, adminService, ids)
	if err != nil {
		return err
	}

	exchangeTimeout, err := time.ParseDuration(config.ChatExchangeTimeout)
	if err != nil {
		return fmt.Errorf("config: chat_exchange_timeout: %w", err)
	}

	confirmationTimeout, err := time.ParseDuration(config.ChatConfirmationTimeout)
	if err != nil {
		return fmt.Errorf("config: chat_confirmation_timeout: %w", err)
	}
	cliTimeout, err := cliTurnTimeout(config)
	if err != nil {
		return err
	}
	// A hold has to fit inside the turn that is waiting on it. The CLI provider is
	// the one that holds calls, and its turn ends on that ceiling — so a
	// confirmation window at or beyond it would time the turn out from under the
	// card the developer is reading, and their approval would run a tool whose
	// caller had gone.
	if config.ClaudeCliEnabled && confirmationTimeout+confirmationHeadroom >= cliTimeout {
		slog.Warn("chat_confirmation_timeout leaves no room inside a CLI turn: a developer who "+
			"takes that long to decide will find the turn already over",
			"chat_confirmation_timeout", confirmationTimeout, "cli_turn_timeout", cliTimeout)
	}
	// And the exchange is the outer bound of both: a turn cannot outlive the
	// exchange carrying it, so a CLI ceiling above chat_exchange_timeout is a number
	// that can never be reached.
	if config.ClaudeCliEnabled && cliTimeout >= exchangeTimeout {
		slog.Warn("claude_cli_timeout is at or above chat_exchange_timeout, so the exchange "+
			"ends first and the CLI ceiling is unreachable",
			"claude_cli_timeout", cliTimeout, "chat_exchange_timeout", exchangeTimeout)
	}

	// ctx, not a background context: an exchange is detached from the request that
	// started it but not from the process, so shutdown still stops one in flight.
	engine, err := chat.New(ctx, providers, dispatcher, chatStore, adminService, ids, chat.Options{
		MaxIterations:   int(config.LlmMaxToolIterations),
		MaxTokens:       int(config.LlmMaxTokens),
		Effort:          config.LlmEffort,
		MCPEndpoint:     mcpEndpoint(config),
		ExchangeTimeout: exchangeTimeout,

		ConfirmationTimeout: confirmationTimeout,
	})
	if err != nil {
		return err
	}
	sink.engine = engine

	deps.Chat = engine
	deps.Admin = adminService

	// The MCP transport is mounted only when something needs it, which today means
	// the CLI provider. Mounting it unconditionally would publish ODE's tools to
	// any MCP client for no configured reason.
	if config.ClaudeCliEnabled {
		server, err := mcp.New(dispatcher, engine, "0.3.0")
		if err != nil {
			return err
		}
		deps.MCP = server.Handler(api.AuthenticateMCP(config.RequiredRealmRole))
		if mcpEndpoint(config) == "" {
			slog.Warn("the claude CLI provider is enabled but public_url is not set: " +
				"the CLI will run in text-only advisory mode because it cannot reach ODE's MCP endpoint")
		}
	}

	slog.Info("llm surface ready",
		"providers", providers.Names(),
		"tools_declared", len(registry.Definitions()),
		"tools_available_at_l0", len(registry.Available(tools.L0)),
		"persistent", db != nil)
	return nil
}

// startM4 wires the execution backend (§5.6).
//
// It returns a nil service when no jupyterhub_url is configured, which is the
// same degradation the profiler and the LLM surface already do: the deployment
// serves everything below the missing dependency and says what is absent.
//
// What it does not do is degrade a *misconfigured* Hub. A credential that cannot
// spawn is a deployment fault, and the milestone was built on the decision that
// it fails startup rather than surfacing as a 403 on someone's first cell.
func startM4(ctx context.Context, config configuration.Config) (*kernel.Service, error) {
	if config.JupyterhubUrl == "" {
		slog.Warn("no jupyterhub_url configured: the kernel routes are not served and " +
			"run_code has no executor")
		return nil, nil
	}
	if config.JupyterhubToken == "" {
		return nil, errors.New("config: jupyterhub_token is required when jupyterhub_url is set")
	}

	parse := func(name, value string) (time.Duration, error) {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("config: %s: %w", name, err)
		}
		return parsed, nil
	}

	spawnTimeout, err := parse("jupyterhub_spawn_timeout", config.JupyterhubSpawnTimeout)
	if err != nil {
		return nil, err
	}
	requestTimeout, err := parse("jupyterhub_request_timeout", config.JupyterhubRequestTimeout)
	if err != nil {
		return nil, err
	}
	executeTimeout, err := parse("jupyterhub_execute_timeout", config.JupyterhubExecuteTimeout)
	if err != nil {
		return nil, err
	}
	keepalive, err := parse("jupyterhub_keepalive_interval", config.JupyterhubKeepaliveInterval)
	if err != nil {
		return nil, err
	}
	idleTimeout, err := parse("jupyterhub_idle_timeout", config.JupyterhubIdleTimeout)
	if err != nil {
		return nil, err
	}
	tokenTTL, err := parse("jupyterhub_token_ttl", config.JupyterhubTokenTtl)
	if err != nil {
		return nil, err
	}

	service, err := kernel.New(kernel.Options{
		BaseURL:           config.JupyterhubUrl,
		Token:             config.JupyterhubToken.Value(),
		UsernameClaim:     config.JupyterhubUsernameClaim,
		KernelName:        config.JupyterhubKernel,
		Profile:           config.JupyterhubProfile,
		WorkspacePath:     config.JupyterhubWorkspacePath,
		SpawnTimeout:      spawnTimeout,
		RequestTimeout:    requestTimeout,
		ExecuteTimeout:    executeTimeout,
		KeepaliveInterval: keepalive,
		IdleTimeout:       idleTimeout,
		TokenTTL:          tokenTTL,
		MaxOutputBytes:    int(config.JupyterhubMaxOutputBytes),
		// Handed to the pod so code there reaches the same platform ODE is
		// configured against, rather than the developer restating the URLs.
		Environment: kernelEnvironment(config),
	})
	if err != nil {
		return nil, err
	}

	// The scope check is a real request, so it is bounded: an unreachable Hub must
	// fail startup with its own error rather than hanging the process.
	checkCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	identity, warnings, err := service.CheckScopes(checkCtx)
	if err != nil {
		service.Close()
		return nil, err
	}
	for _, warning := range warnings {
		slog.Warn("jupyterhub credential: " + warning)
	}

	service.Start(ctx)
	slog.Info("kernel surface ready",
		"hub", config.JupyterhubUrl,
		"credential", identity.Name,
		"kind", identity.Kind,
		"kernel", service.KernelName(),
		"workspace", service.Workspace())
	return service, nil
}

// startM7 wires the repository surface (§5.11).
//
// It needs three things and degrades without any of them, in the same shape the
// rest of ODE degrades: no GitHub OAuth app, no repo routes and no `write_file`.
// The one thing it will not do is run without the encryption key — a token stored
// in the clear is a different design than the one §5.11 item 1 describes, not a
// development convenience — or without a Hub, because there would be nowhere for a
// working copy to live.
func startM7(
	config configuration.Config, db *database.DB, kernelService *kernel.Service,
) (*repo.Service, error) {
	if config.GithubClientId == "" {
		slog.Warn("no github_client_id configured: the repo routes are not served and " +
			"write_file is declared but not callable (§5.11)")
		return nil, nil
	}
	if kernelService == nil {
		// Not a refusal of the deployment, because a Hub-less ODE is a supported
		// configuration — but the repo surface cannot be part of one, since the
		// working copy lives in the developer's pod.
		slog.Warn("github_client_id is set but no jupyterhub_url is: the repo routes are " +
			"not served, because the working copy of §5.11 item 5 lives on the developer's pod")
		return nil, nil
	}
	if config.GithubClientSecret == "" {
		return nil, errors.New(
			"config: github_client_secret is required when github_client_id is set")
	}

	sealer, err := repo.NewSealer(config.GithubTokenKey.Value())
	if err != nil {
		return nil, err
	}

	commandTimeout, err := time.ParseDuration(config.RepoCommandTimeout)
	if err != nil {
		return nil, fmt.Errorf("config: repo_command_timeout: %w", err)
	}

	// The callback belongs to the SPA, which posts the code back to ODE with its own
	// platform token. Derived from public_url only as a convenience for a deployment
	// that serves both from one origin; anything else has to say it, because the
	// value has to match the OAuth app's registered callback exactly.
	redirect := config.GithubRedirectUri
	if redirect == "" && config.PublicUrl != "" {
		redirect = strings.TrimSuffix(config.PublicUrl, "/") + "/github/callback"
	}
	if redirect == "" {
		return nil, errors.New(
			"config: github_redirect_uri is required (or public_url, to derive it from)")
	}

	service, err := repo.New(repo.Deps{
		Workspace: kernelService,
		// The credential and the link go to Postgres when there is one. Neither is
		// recomputable: without them every developer reconnects GitHub and re-selects
		// a repository whose checkout is still on their PVC.
		Store:  repoStore(db),
		Sealer: sealer,
		Options: repo.Options{
			ClientID:              config.GithubClientId,
			ClientSecret:          config.GithubClientSecret.Value(),
			APIURL:                config.GithubApiUrl,
			WebURL:                config.GithubWebUrl,
			Scopes:                config.GithubScopes,
			RedirectURI:           redirect,
			CommandTimeout:        commandTimeout,
			MaxFileBytes:          int(config.RepoMaxFileBytes),
			MaxTreeEntries:        int(config.RepoMaxTreeEntries),
			MaxCommandOutputBytes: int(config.RepoMaxCommandOutputBytes),
			OperatorLib:           config.OperatorLibRepo,
			OperatorLibRef:        config.OperatorLibRef,
			MaxWorkbenches:        int(config.RepoMaxWorkbenches),
		},
	})
	if err != nil {
		return nil, err
	}

	// The kernel service can now resolve a workbench to the directory its kernel
	// runs in. Set here rather than in kernel.Options because this service is built
	// *on* that one — it takes it as its workspace — so it cannot exist yet when the
	// kernel service is made.
	kernelService.UseWorkbenches(service)

	slog.Info("repo surface ready",
		"github", config.GithubApiUrl,
		"scopes", config.GithubScopes,
		"redirect", redirect,
		"operator_lib", config.OperatorLibRepo,
		"operator_lib_ref", pinOrLatest(config.OperatorLibRef),
		"persistent", db != nil)
	return service, nil
}

// startM8 wires the experiment surface (§5.12).
//
// It degrades in the shape the rest of ODE degrades and refuses in the shape the
// rest of ODE refuses. No ray_url or mlflow_url means no experiment routes and two
// tools that stay declared-but-unavailable. A ray_url without an mlflow_url — or
// without the Hub and repo surfaces the job package is built from — is a
// deployment that cannot do the thing it was configured for, so it is named rather
// than half-served.
//
// The one thing it will not do is degrade a *misconfigured* one. An unparseable
// duration fails startup, for the reason startM4 gives: a deployment fault
// discovered on a developer's first launch is worse than one discovered at boot.
func startM8(
	config configuration.Config,
	db *database.DB,
	kernelService *kernel.Service,
	repoService *repo.Service,
) (*experiments.Service, error) {
	if config.RayUrl == "" && config.MlflowUrl == "" {
		slog.Warn("no ray_url or mlflow_url configured: the experiment routes are not " +
			"served and launch_experiment and get_experiment_results are declared but not " +
			"callable (§5.12)")
		return nil, nil
	}
	// Half a configuration is a deployment fault rather than a lesser capability:
	// ODE creates the MLflow run before submitting the job, so a Ray cluster without
	// a tracking server cannot launch anything at all.
	if config.RayUrl == "" {
		return nil, errors.New("config: ray_url is required when mlflow_url is set")
	}
	if config.MlflowUrl == "" {
		return nil, errors.New("config: mlflow_url is required when ray_url is set")
	}
	if kernelService == nil || repoService == nil {
		// Not a refusal of the deployment, for the reason startM7's Hub check is not:
		// a Hub-less or GitHub-less ODE is supported, and the experiment surface
		// simply cannot be part of one. The job package is `git archive` of a working
		// copy that lives on the developer's pod, so both are load-bearing.
		slog.Warn("ray_url and mlflow_url are set but the experiment surface needs both a "+
			"kernel and a repository service: the routes are not served and the two "+
			"experiment tools stay declared but not callable",
			"jupyterhub_url", config.JupyterhubUrl != "",
			"github_client_id", config.GithubClientId != "")
		return nil, nil
	}

	parse := func(name, value string) (time.Duration, error) {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("config: %s: %w", name, err)
		}
		return parsed, nil
	}

	requestTimeout, err := parse("experiment_request_timeout", config.ExperimentRequestTimeout)
	if err != nil {
		return nil, err
	}
	uploadTimeout, err := parse("experiment_upload_timeout", config.ExperimentUploadTimeout)
	if err != nil {
		return nil, err
	}
	commandTimeout, err := parse("repo_command_timeout", config.RepoCommandTimeout)
	if err != nil {
		return nil, err
	}
	embedTTL, err := parse("experiment_embed_ttl", config.ExperimentEmbedTtl)
	if err != nil {
		return nil, err
	}
	embedTimeout, err := parse("experiment_embed_timeout", config.ExperimentEmbedTimeout)
	if err != nil {
		return nil, err
	}
	jobTokenLifetime, err := parse("job_token_lifetime", config.JobTokenLifetime)
	if err != nil {
		return nil, err
	}

	service, err := experiments.New(experiments.Deps{
		Workspace: kernelService,
		Repo:      repoService,
		// An experiment record is the one thing in M8 that is recomputable from
		// nowhere else: Ray forgets a submission and MLflow does not know which
		// working copy produced a run. So it goes to Postgres whenever there is one,
		// unlike the profiles and relation profiles that stay in memory (§5.4.3).
		Store: experimentStore(db),
		IDs:   identifiers.New(),
		Options: experiments.Options{
			RayURL:            config.RayUrl,
			RayToken:          config.RayToken.Value(),
			RayDashboardURL:   config.RayDashboardUrl,
			MLflowURL:         config.MlflowUrl,
			MLflowToken:       config.MlflowToken.Value(),
			MLflowUIURL:       config.MlflowUiUrl,
			ExperimentPrefix:  config.MlflowExperimentPrefix,
			DefaultEntrypoint: config.ExperimentDefaultEntrypoint,
			MaxPackageBytes:   config.ExperimentMaxPackageBytes,
			MaxEnvVars:        int(config.ExperimentMaxEnvVars),
			MaxEnvValueBytes:  int(config.ExperimentMaxEnvValueBytes),
			MaxLogBytes:       int(config.ExperimentMaxLogBytes),
			RequestTimeout:    requestTimeout,
			UploadTimeout:     uploadTimeout,
			CommandTimeout:    commandTimeout,
			EmbedProbeTTL:     embedTTL,
			EmbedProbeTimeout: embedTimeout,

			KeycloakURL:          config.KeycloakUrl,
			KeycloakRealm:        config.KeycloakRealm,
			KeycloakClientID:     config.KeycloakClientId,
			KeycloakClientSecret: config.KeycloakClientSecret.Value(),
			JobTokenAudience:     config.JobTokenAudience,
			JobTokenLifetime:     jobTokenLifetime,

			// The same URLs a kernel is told about, for the same reason: a job reads
			// its training data from the platform directly (§5.3.4) rather than through
			// ODE, and should not need the developer to restate where.
			Environment: kernelEnvironment(config),
		},
	})
	if err != nil {
		return nil, err
	}

	if !service.ExchangeConfigured() {
		// The risk register's "token expiry vs. long Ray jobs" row, said once at
		// startup rather than only in each launch result. §3.1 item 6 asks for a
		// short-lived scoped token, and a deployment without one should know that a
		// long run will lose its platform access partway through.
		slog.Warn("no keycloak token exchange is configured: a Ray job carries the " +
			"developer's interactive session token, so a run that outlives the session " +
			"loses its platform access partway through (§3.1 item 6). Set " +
			"keycloak_url, keycloak_realm, keycloak_client_id and keycloak_client_secret")
	}

	slog.Info("experiment surface ready",
		"ray", config.RayUrl,
		"mlflow", config.MlflowUrl,
		"entrypoint", config.ExperimentDefaultEntrypoint,
		"max_package_bytes", config.ExperimentMaxPackageBytes,
		"scoped_job_token", service.ExchangeConfigured(),
		"persistent", db != nil)
	return service, nil
}

// experimentStore picks the store for submitted experiments.
//
// No split, unlike the profiler's and the relational profiler's: none of an
// experiment record is recomputable, so all of it goes to Postgres when there is
// one. Without a database the memory store keeps it for the life of the process,
// and validate() says what that costs.
func experimentStore(db *database.DB) experiments.Store {
	if db == nil {
		return experiments.NewMemoryStore()
	}
	return experiments.NewPostgresStore(db)
}

// startM9 wires result interpretation (§5.13).
//
// It degrades in the shape the rest of ODE degrades, and the two things it needs
// are the two halves of the sentence §5.13 is: an experiment surface to summarise
// a finished run, and a chat engine to interpret it into. A deployment missing
// either serves everything else and says which is absent — a Ray cluster with no
// LLM provider still launches experiments and still answers
// /experiments/{id}/results; nothing merely happens by itself.
//
// Two goroutines for the whole process, both rooted at ctx so shutdown stops them:
// the poller that notices a run finished, and the delivery loop that runs the turn
// when a developer's credential is available. Neither holds a credential of its
// own — that is the point of the design and the reason the loop exists at all.
// It returns the function shutdown calls to wait for both loops to have stopped,
// which is a no-op where neither was started.
func startM9(
	ctx context.Context,
	config configuration.Config,
	deps *api.Deps,
	db *database.DB,
	experimentService *experiments.Service,
) (func(context.Context), error) {
	if experimentService == nil {
		if deps.Chat != nil {
			slog.Warn("no experiment surface is configured: a finished run cannot be " +
				"interpreted into a conversation (§5.13)")
		}
		return noStop, nil
	}
	if deps.Chat == nil {
		slog.Warn("no llm provider is configured: experiments still launch and their " +
			"results are still read through /experiments/{id}/results, but nothing " +
			"interprets a finished run into a conversation (§5.13)")
		return noStop, nil
	}

	parse := func(name, value string) (time.Duration, error) {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("config: %s: %w", name, err)
		}
		return parsed, nil
	}

	pollInterval, err := parse("experiment_poll_interval", config.ExperimentPollInterval)
	if err != nil {
		return noStop, err
	}
	pollWindow, err := parse("experiment_poll_window", config.ExperimentPollWindow)
	if err != nil {
		return noStop, err
	}
	pollTimeout, err := parse("experiment_poll_timeout", config.ExperimentPollTimeout)
	if err != nil {
		return noStop, err
	}
	retryInterval, err := parse("interpretation_retry_interval", config.InterpretationRetryInterval)
	if err != nil {
		return noStop, err
	}
	turnTimeout, err := parse("interpretation_turn_timeout", config.InterpretationTurnTimeout)
	if err != nil {
		return noStop, err
	}

	service, err := interpret.New(interpret.Deps{
		Experiments: experimentService,
		Chat:        deps.Chat,
		// The decision log goes to Postgres when there is one, for the reason the
		// profiler's override overlay and the relational profiler's rule decisions do
		// (§5.4.3): the summary and the interpretation are recoverable — from MLflow
		// and from the conversation — and a developer's answer to a proposal is not.
		Store: interpretStore(db),
		IDs:   identifiers.New(),
		Options: interpret.Options{
			RetryInterval: retryInterval,
			TurnTimeout:   turnTimeout,
			MaxPending:    int(config.InterpretationMaxPending),
		},
	})
	if err != nil {
		return noStop, err
	}

	poller, err := experiments.NewPoller(experimentService, service, experiments.PollerOptions{
		Interval: pollInterval,
		Window:   pollWindow,
		Batch:    int(config.ExperimentPollBatch),
		Timeout:  pollTimeout,
	})
	if err != nil {
		return noStop, err
	}

	service.Start(ctx)
	poller.Start(ctx)
	deps.Interpretations = service

	slog.Info("result interpretation ready",
		"poll_interval", pollInterval,
		"poll_window", pollWindow,
		"retry_interval", retryInterval,
		"persistent_decisions", db != nil)

	return func(shutdownCtx context.Context) {
		// Both loops are rooted at the process context, so they are already stopping by
		// the time this runs; what this waits for is their goroutines actually having
		// returned. Bounded by the shutdown deadline, because a turn that will not end
		// must not hold the process open — the summary is durable either way.
		for _, done := range []<-chan struct{}{service.Stopped(), poller.Stopped()} {
			select {
			case <-done:
			case <-shutdownCtx.Done():
				slog.Warn("result interpretation did not stop within the shutdown deadline")
				return
			}
		}
	}, nil
}

// noStop is the shutdown hook of a deployment that started neither M9 loop.
func noStop(context.Context) {}

// interpretStore picks the store for the proposal decision log.
//
// The only persisted state M9 adds, and the split behind that is §5.4.3's: the
// summary is recomputable from MLflow, the interpretation is already durable as
// chat messages, and only the developer's answer can be regenerated by nothing.
// Without a database it is in memory, and a restart then loses every answer — so a
// proposal a developer rejected comes back as though they had never been asked.
func interpretStore(db *database.DB) interpret.Store {
	if db == nil {
		return interpret.NewMemoryStore()
	}
	return interpret.NewPostgresStore(db)
}

// experimentsOrNil keeps the two M8 tools declared-but-unavailable in a deployment
// without a Ray cluster, for the reason chartsOrNil documents.
func experimentsOrNil(service *experiments.Service) tools.Experiments {
	if service == nil {
		return nil
	}
	return service
}

func pinOrLatest(ref string) string {
	if ref == "" {
		return "(resolved at scaffold time)"
	}
	return ref
}

func repoStore(db *database.DB) repo.Store {
	if db == nil {
		return repo.NewMemoryStore()
	}
	return repo.NewPostgresStore(db)
}

// repoOrNil keeps write_file declared-but-unavailable in a deployment without a
// GitHub app, for the reason chartsOrNil documents.
func repoOrNil(service *repo.Service) tools.Repo {
	if service == nil {
		return nil
	}
	return service
}

// kernelEnvironment is what a pod is told about the platform, beside the
// developer's own token. Only URLs: no credential of ODE's own ever goes in.
func kernelEnvironment(config configuration.Config) map[string]string {
	environment := map[string]string{}
	if config.DeviceRepoUrl != "" {
		environment["SENERGY_DEVICE_REPO_URL"] = config.DeviceRepoUrl
	}
	if config.TimescaleWrapperUrl != "" {
		environment["SENERGY_TIMESCALE_URL"] = config.TimescaleWrapperUrl
	}
	return environment
}

// buildProviders registers every configured provider, in the order §5.7 lists
// them. The first registered is the default a session gets.
func buildProviders(
	ctx context.Context, config configuration.Config, pricing *llm.Pricing,
) (*llm.Registry, error) {
	registry, err := llm.NewRegistry()
	if err != nil {
		return nil, err
	}

	if config.AnthropicApiKey != "" {
		provider, err := llm.NewAnthropicProvider("anthropic", llm.AnthropicOptions{
			APIKey:           config.AnthropicApiKey.Value(),
			BaseURL:          config.AnthropicBaseUrl,
			Models:           config.AnthropicModels,
			MaxTokens:        int(config.LlmMaxTokens),
			Effort:           config.LlmEffort,
			AdaptiveThinking: config.LlmAdaptiveThinking,
		}, pricing)
		if err != nil {
			return nil, err
		}
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}

	if config.OpenaiApiKey != "" {
		provider, err := llm.NewOpenAIProvider("openai", llm.OpenAIOptions{
			APIKey:    config.OpenaiApiKey.Value(),
			BaseURL:   config.OpenaiBaseUrl,
			Models:    config.OpenaiModels,
			MaxTokens: int(config.LlmMaxTokens),
		}, pricing)
		if err != nil {
			return nil, err
		}
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}

	if config.CompatibleBaseUrl != "" {
		provider, err := llm.NewOpenAICompatibleProvider(config.CompatibleName, llm.OpenAIOptions{
			APIKey:    config.CompatibleApiKey.Value(),
			BaseURL:   config.CompatibleBaseUrl,
			Models:    config.CompatibleModels,
			MaxTokens: int(config.LlmMaxTokens),
			Tools:     config.CompatibleTools,
		}, pricing)
		if err != nil {
			return nil, err
		}
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}

	if config.ClaudeCliEnabled {
		timeout, err := cliTurnTimeout(config)
		if err != nil {
			return nil, err
		}
		provider := llm.NewAnthropicCLIProvider("claude-cli", llm.CLIOptions{
			Binary:  config.ClaudeCliBinary,
			Models:  config.ClaudeCliModels,
			Timeout: timeout,
		}, pricing)
		// Probed at startup, as §5.7 requires. It never fails startup: a missing CLI
		// degrades this one provider and leaves the others alone.
		provider.Probe(ctx)
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}

	return registry, nil
}

// cliTurnTimeout is the configured ceiling on one CLI turn, or llm's default.
//
// Read in two places — the provider that enforces it and the startup check that
// compares it against the confirmation window — so it is one function rather than
// two readings that could drift.
func cliTurnTimeout(config configuration.Config) (time.Duration, error) {
	if strings.TrimSpace(config.ClaudeCliTimeout) == "" {
		return llm.DefaultCLITimeout, nil
	}
	parsed, err := time.ParseDuration(config.ClaudeCliTimeout)
	if err != nil {
		return 0, fmt.Errorf("config: claude_cli_timeout: %w", err)
	}
	return parsed, nil
}

func mcpEndpoint(config configuration.Config) string {
	if config.PublicUrl == "" {
		return ""
	}
	return mcp.Endpoint(config.PublicUrl)
}

func modelPrices(configured []configuration.ModelPrice) []llm.ModelPrice {
	out := make([]llm.ModelPrice, 0, len(configured))
	for _, price := range configured {
		out = append(out, llm.ModelPrice{
			Model:              price.Model,
			InputPerMTok:       price.InputPerMTok,
			OutputPerMTok:      price.OutputPerMTok,
			CachedInputPerMTok: price.CachedInputPerMTok,
		})
	}
	return out
}

func adminStore(db *database.DB) admin.Store {
	if db == nil {
		return admin.NewMemoryStore()
	}
	return admin.NewPostgresStore(db)
}

func chatStore(db *database.DB) chat.Store {
	if db == nil {
		return chat.NewMemoryStore()
	}
	return chat.NewPostgresStore(db)
}

// sessionSink breaks the cycle between the tool surface and the chat engine.
//
// Two tool dependencies are session state and therefore the engine's:
// propose_data_selection writes the proposed selection, and the create tools
// write what they created so the delete tools can check it. Both need the engine;
// the engine needs the dispatcher, which needs the tools. The holder is written
// once during startup and read only inside a dispatch, which cannot happen before
// Start returns.
type sessionSink struct{ engine *chat.Engine }

func (s *sessionSink) PutProposedSelection(
	ctx context.Context, sessionID string, proposal tools.ProposedSelection,
) error {
	if s.engine == nil {
		return errors.New("chat is not configured")
	}
	return s.engine.PutProposedSelection(ctx, sessionID, proposal)
}

func (s *sessionSink) RecordCreation(
	ctx context.Context, sessionID string, created tools.Creation,
) error {
	if s.engine == nil {
		return errors.New("chat is not configured")
	}
	return s.engine.RecordCreation(ctx, sessionID, created)
}

func (s *sessionSink) Creations(ctx context.Context, sessionID string) ([]tools.Creation, error) {
	if s.engine == nil {
		// Refusing rather than answering with an empty list. An empty list reads as
		// "this session created nothing", which is the same answer a delete tool gives
		// for a session that created plenty and could not be read.
		return nil, errors.New("chat is not configured")
	}
	return s.engine.Creations(ctx, sessionID)
}

// profilerOrNil and selectionOrNil are rankerOrNil's siblings: the same typed-nil
// footgun, for the two optional services the tool surface reads through.
func timeseriesOrNil(client *timeseries.Client) tools.Timeseries {
	if client == nil {
		return nil
	}
	return client
}

func profilerOrNil(prof *profiler.Profiler) tools.Profiler {
	if prof == nil {
		return nil
	}
	return prof
}

// toolImportsOrNil is the tool surface's narrower view of the same service the
// resolver holds, and exists for the reason every other *OrNil here does: a typed
// nil in an interface is not nil, and the three import tools would then be
// registered with an executor that panics instead of declared-but-unavailable.
func toolImportsOrNil(svc *imports.Service) tools.Imports {
	if svc == nil {
		return nil
	}
	return svc
}

func selectionOrNil(resolver *selection.Resolver) tools.Selection {
	if resolver == nil {
		return nil
	}
	return resolver
}

func kernelOrNil(service *kernel.Service) tools.Kernel {
	if service == nil {
		return nil
	}
	return service
}

// rankerOrNil hands the resolver an interface that is actually nil when there is
// no profiler.
//
// Passing the typed nil pointer straight in would produce a non-nil interface
// holding a nil pointer, and the resolver's "is there a ranker" check would pass
// before dereferencing it. This is the one Go footgun in the wiring, so it is a
// named function rather than an inline conditional.
// chartsOrNil keeps render_chart declared-but-unavailable in a deployment without
// a timescale-wrapper, for the reason ifPresent documents: a typed nil pointer in
// an interface field is not nil as an interface, so the check has to happen here.
func chartsOrNil(service *charts.Service) tools.Charts {
	if service == nil {
		return nil
	}
	return service
}

// relationsOrNil does the same for the two M6 tools: a deployment without a
// timescale-wrapper has no profiler and therefore no relational profiler, and both
// tools stay declared-but-unavailable rather than registered and broken.
func relationsOrNil(service *relations.Service) tools.Relations {
	if service == nil {
		return nil
	}
	return service
}

func rankerOrNil(prof *profiler.Profiler) selection.Ranker {
	if prof == nil {
		return nil
	}
	return prof
}

// importsOrNil exists for the reason rankerOrNil does: a typed nil pointer in an
// interface is not nil, so returning the service unconditionally would make every
// `if r.imports == nil` guard downstream false and every call panic.
func importsOrNil(svc *imports.Service) selection.Imports {
	if svc == nil {
		return nil
	}
	return svc
}

// exportSource adapts the import service onto the profiler's ExportSource.
//
// The mapping is here rather than in either package because pkg/imports has no
// internal dependencies at all — it is the one package that talks only to the
// platform — and giving it one for the sake of a field-for-field copy would be
// the wrong trade. The profiler declares the shape it needs, as it does for the
// timeseries client and the ontology.
type exportSource struct{ svc *imports.Service }

func (s exportSource) ExportDefinition(
	ctx context.Context, token string, exportID string,
) (profiler.ExportDefinition, error) {
	definition, err := s.svc.ExportDefinition(ctx, token, exportID)
	if err != nil {
		return profiler.ExportDefinition{}, err
	}
	out := profiler.ExportDefinition{
		ExportID: definition.ExportID,
		Name:     definition.Name,
		Source:   definition.Source,
		SourceID: definition.SourceID,
		Notes:    definition.Notes,
		Columns:  make([]profiler.ExportColumn, 0, len(definition.Columns)),
	}
	for _, column := range definition.Columns {
		out.Columns = append(out.Columns, profiler.ExportColumn{
			Column:           column.Column,
			Type:             column.Type,
			VariablePath:     column.VariablePath,
			CharacteristicID: column.CharacteristicID,
			FunctionID:       column.FunctionID,
			AspectID:         column.AspectID,
			Tag:              column.Tag,
		})
	}
	return out, nil
}

// exportSourceOrNil withholds the source where it could not answer anyway.
//
// Two conditions, and the second is the one worth stating: the import service
// exists without analytics-serving — discovery, status and wiring all work
// without it — but an export's column names do not, and they are what a query
// over an export is made of. Handing the profiler a source that refuses every
// call would make the export tools advertise a capability this deployment does
// not have.
func exportSourceOrNil(config configuration.Config, svc *imports.Service) profiler.ExportSource {
	if svc == nil || config.AnalyticsServingUrl == "" {
		return nil
	}
	return exportSource{svc: svc}
}

// startImports wires the import surface, or returns nil when the platform
// services it needs are not configured.
//
// device_selection_url is the one that decides. The other three each remove a
// capability rather than the surface: without import-deploy an instance's status
// is unknown, without import-repository a type cannot be looked up by id alone,
// and without analytics-serving the history question answers "unknown". Each of
// those is reported in the answer, which is why none of them is fatal here.
//
// import-deploy is the exception that is required alongside device-selection: the
// import service refuses to be built without it, because discovery carries no
// container status at all and an import whose status is never asked for would be
// ranked as though it were running.
func startImports(config configuration.Config) (*imports.Service, error) {
	if config.DeviceSelectionUrl == "" {
		return nil, nil
	}
	if config.ImportDeployUrl == "" {
		return nil, errors.New("config: device_selection_url is set, so import_deploy_url is " +
			"required: without it ODE cannot tell a running import from a stopped one")
	}

	timeout, err := time.ParseDuration(config.ImportRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("config: import_request_timeout: %w", err)
	}
	opts := imports.ClientOptions{Timeout: timeout}

	deployClient := imports.NewDeployClient(config.ImportDeployUrl, opts)
	deps := imports.Deps{
		Selectables: imports.NewSelectionClient(config.DeviceSelectionUrl, opts),
		Instances:   deployClient,
		// The write half is the same client. It is a second field rather than one
		// interface so that the read path a dozen call sites depend on does not carry
		// a method that deploys a container; see imports.Deployer.
		Deployer: deployClient,
		ExportDefaults: imports.ExportDefaults{
			Offset:          config.ExportOffset,
			TimePath:        config.ExportTimePath,
			TimestampFormat: config.ExportTimestampFormat,
			DatabaseID:      config.ExportDatabaseID,
		},
	}
	if config.ImportRepoUrl != "" {
		deps.Types = imports.NewRepositoryClient(config.ImportRepoUrl, opts)
	}
	if config.AnalyticsServingUrl != "" {
		servingClient := imports.NewServingClient(config.AnalyticsServingUrl, opts)
		deps.Exports = servingClient
		deps.ExportWriter = servingClient
	}
	return imports.New(deps)
}

// confirmationHeadroom is the slack a held confirmation needs inside a turn: the
// margin the provider's own tool timeout is given, plus room for the tool to
// actually run once it is approved.
const confirmationHeadroom = 2 * time.Minute

// validate fails fast on configuration that would otherwise produce confusing
// runtime failures, and warns where a weak setting is legal but undesirable.
func validate(config configuration.Config) error {
	if config.ApiPort == "" {
		return errors.New("config: api_port is required")
	}
	if config.DeviceRepoUrl == "" {
		return errors.New("config: device_repo_url is required")
	}
	if config.RequiredRealmRole == "" {
		slog.Warn("no required_realm_role configured: every authenticated platform user may use ODE")
	}
	if config.TimescaleWrapperUrl == "" {
		slog.Warn("no timescale_wrapper_url configured: the timeseries and profiler routes are not served")
	}
	if config.DeviceSelectionUrl == "" {
		// Not a tidiness warning. An operator can take an import as an input, and
		// without this every semantic selection answers with devices only — so a
		// developer is told a signal does not exist when the platform imports it.
		slog.Warn("no device_selection_url configured: imports are not searched, so semantic " +
			"selection reports devices only and the seven import tools — list_import_instances, " +
			"get_import_type_metadata, list_import_types, propose_operator_input, " +
			"create_import_instance, create_export and their two deletions — are declared but " +
			"not callable")
	} else if config.ImportDeployUrl == "" {
		slog.Warn("device_selection_url is set but import_deploy_url is not: imports are found " +
			"but ODE cannot say whether one is running, and a stopped import looks exactly " +
			"like a live one in a selectables answer")
	} else if config.AnalyticsServingUrl == "" {
		slog.Warn("no analytics_serving_url configured: whether an import has stored history " +
			"answers 'unknown' rather than 'live only', because timescale-wrapper has no " +
			"importId and only an export puts an import in timescale; create_export and " +
			"delete_export are declared but not callable")
	}
	if config.ImportDeployUrl != "" && config.ImportRepoUrl == "" {
		// Not a degradation of the same kind as the ones above. Creating validates the
		// configs and the exported columns against the import type, and refuses without
		// it rather than sending an unchecked request — so the tools are advertised
		// and answer every call with the same refusal, which is worth saying at startup.
		slog.Warn("import_deploy_url is set but import_repo_url is not: get_import_type_metadata, " +
			"list_import_types, create_import_instance and create_export are advertised and will " +
			"refuse every call, and a resolution reports no deployable_import_types — so an " +
			"import type that has no instance yet cannot be found at all, which is the only " +
			"kind create_import_instance is for")
	}
	if config.JupyterhubUrl == "" {
		slog.Warn("no jupyterhub_url configured: a developer cannot run code, and run_code " +
			"is declared but not callable (§5.6)")
	}
	if config.GithubClientId == "" {
		slog.Warn("no github_client_id configured: a developer cannot connect a repository, " +
			"and write_file is declared but not callable (§5.11)")
	}
	if config.RayUrl == "" || config.MlflowUrl == "" {
		slog.Warn("no ray_url or mlflow_url configured: a developer cannot launch an " +
			"experiment, and launch_experiment and get_experiment_results are declared " +
			"but not callable (§5.12)")
	}
	if config.PostgresUrl == "" {
		// Not a warning about tidiness. §3.3's per-user spend cap is computed from
		// recorded usage, so without a database the cap is only as old as this
		// process: a restart hands every developer a fresh allowance.
		slog.Warn("no postgres_url configured: chat history, the exposure-tier audit trail, " +
			"the profiler override overlay, LLM spend accounting and the record of every " +
			"submitted experiment are in memory and will not survive a restart, so a " +
			"per-user spend cap does not hold across one and a restart loses the trail " +
			"from an MLflow run back to the commit it came from, and a next-experiment " +
			"proposal the developer rejected comes back as though they had never been asked")
	}
	return nil
}
