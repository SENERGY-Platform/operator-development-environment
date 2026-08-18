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

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/configuration"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
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

	// The ontology index is built once and memoised per snapshot. It is hoisted
	// out of the profiler's block below because semantic selection needs it too —
	// the unit and the completeness of a resolved variable come from it — and
	// selection is served whether or not a timescale-wrapper is configured.
	ontologyIndex := profiler.NewSnapshotOntology(ontologyRepo)

	deps := api.Deps{
		Ontology: ontologyRepo,
		Devices:  deviceService,
	}

	// The timeseries client and the profiler are optional so that a deployment
	// without a timescale-wrapper URL still serves the M0 surface instead of
	// failing to start. validate() warns about it.
	if config.TimescaleWrapperUrl != "" {
		timeout, err := time.ParseDuration(config.TimeseriesRequestTimeout)
		if err != nil {
			return nil, fmt.Errorf("config: timeseries_request_timeout: %w", err)
		}
		timeseriesClient := timeseries.New(config.TimescaleWrapperUrl, timeseries.Options{Timeout: timeout})

		// The profile store is in-memory (see profiler.MemoryStore): computed
		// profiles are recomputable, but the override overlay is developer input
		// and does not survive a restart. Persisting it needs a database, which
		// is a deployment decision this milestone does not make.
		profilerService, err := profiler.New(
			timeseriesClient,
			ontologyIndex,
			profiler.NewMemoryStore(),
			profiler.Options{
				RawWindowMaxDays:   int(config.ProfilerRawWindowDays),
				RawWindowMaxPoints: int(config.ProfilerRawWindowPoints),
				CoverageWindowDays: int(config.ProfilerCoverageWindowDays),
				Concurrency:        int(config.ProfilerConcurrency),
				LocalTimezone:      config.ProfilerLocalTimezone,
			},
		)
		if err != nil {
			return nil, err
		}
		deps.Timeseries = timeseriesClient
		deps.Profiler = profilerService
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
	}()

	return wg, nil
}

// rankerOrNil hands the resolver an interface that is actually nil when there is
// no profiler.
//
// Passing the typed nil pointer straight in would produce a non-nil interface
// holding a nil pointer, and the resolver's "is there a ranker" check would pass
// before dereferencing it. This is the one Go footgun in the wiring, so it is a
// named function rather than an inline conditional.
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
	return nil
}
