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
	"github.com/SENERGY-Platform/operator-development-environment/pkg/identifiers"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/mcp"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
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
	// argument themselves (SPEC §5.1).
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

	// M3: providers, the tool surface, the dispatcher, chat and the admin controls.
	if err := startM3(ctx, config, &deps, db, ontologyRepo, deviceService, timeseriesClient, kernelService); err != nil {
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

	// The engine is needed by the tool surface (as the selection sink) and the tool
	// surface is needed by the engine, so the registry is built against a holder
	// the engine is written into once it exists. A tool executor only runs during a
	// dispatch, which is always after Start has returned, so the indirection is
	// never observed as a nil.
	sink := &selectionSink{}

	registry, err := tools.NewSurface(tools.Deps{
		Ontology:            ontologyRepo,
		Devices:             deviceService,
		Timeseries:          timeseriesOrNil(timeseriesClient),
		Profiler:            profilerOrNil(deps.Profiler),
		Selection:           selectionOrNil(deps.Selection),
		SelectionSink:       sink,
		Kernel:              kernelOrNil(kernelService),
		Charts:              chartsOrNil(deps.Charts),
		Relations:           relationsOrNil(deps.Relations),
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

	dispatcher, err := tools.NewDispatcher(registry, adminService, ids)
	if err != nil {
		return err
	}

	exchangeTimeout, err := time.ParseDuration(config.ChatExchangeTimeout)
	if err != nil {
		return fmt.Errorf("config: chat_exchange_timeout: %w", err)
	}

	// ctx, not a background context: an exchange is detached from the request that
	// started it but not from the process, so shutdown still stops one in flight.
	engine, err := chat.New(ctx, providers, dispatcher, chatStore, adminService, ids, chat.Options{
		MaxIterations:   int(config.LlmMaxToolIterations),
		MaxTokens:       int(config.LlmMaxTokens),
		Effort:          config.LlmEffort,
		MCPEndpoint:     mcpEndpoint(config),
		ExchangeTimeout: exchangeTimeout,
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
		provider := llm.NewAnthropicCLIProvider("claude-cli", llm.CLIOptions{
			Binary: config.ClaudeCliBinary,
			Models: config.ClaudeCliModels,
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

// selectionSink breaks the cycle between the tool surface and the chat engine.
//
// propose_data_selection writes to the session it was called in, so the tool
// needs the engine; the engine needs the dispatcher, which needs the tools. The
// holder is written once during startup and read only inside a dispatch, which
// cannot happen before Start returns.
type selectionSink struct{ engine *chat.Engine }

func (s *selectionSink) PutProposedSelection(
	ctx context.Context, sessionID string, proposal tools.ProposedSelection,
) error {
	if s.engine == nil {
		return errors.New("chat is not configured")
	}
	return s.engine.PutProposedSelection(ctx, sessionID, proposal)
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
	if config.JupyterhubUrl == "" {
		slog.Warn("no jupyterhub_url configured: a developer cannot run code, and run_code " +
			"is declared but not callable (SPEC §5.6, M4)")
	}
	if config.PostgresUrl == "" {
		// Not a warning about tidiness. §3.3's per-user spend cap is computed from
		// recorded usage, so without a database the cap is only as old as this
		// process: a restart hands every developer a fresh allowance.
		slog.Warn("no postgres_url configured: chat history, the exposure-tier audit trail, " +
			"the profiler override overlay and LLM spend accounting are in memory and will not " +
			"survive a restart, so a per-user spend cap does not hold across one")
	}
	return nil
}
