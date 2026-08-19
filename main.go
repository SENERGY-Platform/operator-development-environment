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

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	structlogger "github.com/SENERGY-Platform/go-service-base/struct-logger"

	"github.com/SENERGY-Platform/operator-development-environment/pkg"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/configuration"
)

// The two attributes the log guidelines require on every record, alongside the
// level slog writes itself. struct-logger attaches them to the handler, so they
// cannot be forgotten at a call site.
const (
	logOrganization = "github.com/SENERGY-Platform"
	logProject      = "operator-development-environment"
)

// newLogger builds ODE's logger. JSON to stdout, timestamps in UTC — a cluster
// collects stdout and correlates across pods in different zones.
func newLogger(level string) *slog.Logger {
	return structlogger.New(structlogger.Config{
		Handler: structlogger.JsonHandlerSelector,
		Level:   level,
		TimeUtc: true,
		AddMeta: true,
	}, os.Stdout, logOrganization, logProject)
}

func main() {
	configLocation := flag.String("config", "config.json", "configuration file")
	flag.Parse()

	// Installed before the configuration is read, so that loading it — which reports
	// every environment override — logs like everything else. The level is not known
	// until the file has been read, so debug is enabled in a second step below.
	slog.SetDefault(newLogger(structlogger.LevelInfo))

	config, err := configuration.Load(*configLocation)
	if err != nil {
		slog.Error("the configuration could not be loaded", "error", err)
		os.Exit(1)
	}

	if config.Debug {
		slog.SetDefault(newLogger(structlogger.LevelDebug))
	}

	ctx, cancel := context.WithCancel(context.Background())

	wg, err := pkg.Start(ctx, config)
	if err != nil {
		cancel()
		slog.Error("ODE could not start", "error", err)
		os.Exit(1)
	}

	go func() {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
		sig := <-shutdown
		slog.Info("received shutdown signal", "signal", sig.String())
		cancel()
	}()

	wg.Wait()
}
