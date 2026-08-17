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

	router := api.NewRouter(
		api.Config{
			RequiredRealmRole: config.RequiredRealmRole,
			CorsOrigins:       config.CorsOrigins,
			Debug:             config.Debug,
		},
		api.Deps{
			Ontology: ontology.New(newOntologyClient, ontology.Options{
				TTL:                ttl,
				InvalidateInterval: invalidateInterval,
			}),
			Devices: devices.New(deviceClient),
		},
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
	return nil
}
