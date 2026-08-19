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

package configuration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadReadsJson(t *testing.T) {
	path := writeConfig(t, `{"api_port":"9090","debug":true,"device_repo_url":"http://dr:8080"}`)

	config, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.ApiPort != "9090" {
		t.Errorf("ApiPort = %q, want 9090", config.ApiPort)
	}
	if !config.Debug {
		t.Error("Debug = false, want true")
	}
	if config.DeviceRepoUrl != "http://dr:8080" {
		t.Errorf("DeviceRepoUrl = %q", config.DeviceRepoUrl)
	}
}

func TestLoadFailsOnAMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestLoadFailsOnInvalidJson(t *testing.T) {
	if _, err := Load(writeConfig(t, `{"api_port":`)); err == nil {
		t.Fatal("expected an error for malformed json")
	}
}

func TestDefaultsFillTheUnsetOperationalValues(t *testing.T) {
	config, err := Load(writeConfig(t, `{"api_port":"8080"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.RequiredRealmRole != "developer" {
		t.Errorf("RequiredRealmRole = %q, want developer", config.RequiredRealmRole)
	}
	if config.OntologyCacheTtl != "1h" {
		t.Errorf("OntologyCacheTtl = %q, want 1h", config.OntologyCacheTtl)
	}
	if config.OntologyInvalidateInt != "5m" {
		t.Errorf("OntologyInvalidateInt = %q, want 5m", config.OntologyInvalidateInt)
	}
	if config.TimeseriesRequestTimeout != "60s" {
		t.Errorf("TimeseriesRequestTimeout = %q, want 60s", config.TimeseriesRequestTimeout)
	}
	// SPEC D25: the raw pass reads the smaller of fourteen days or a hundred
	// thousand points.
	if config.ProfilerRawWindowDays != 14 {
		t.Errorf("ProfilerRawWindowDays = %d, want 14", config.ProfilerRawWindowDays)
	}
	if config.ProfilerRawWindowPoints != 100000 {
		t.Errorf("ProfilerRawWindowPoints = %d, want 100000", config.ProfilerRawWindowPoints)
	}
	if config.ProfilerCoverageWindowDays != 90 {
		t.Errorf("ProfilerCoverageWindowDays = %d, want 90", config.ProfilerCoverageWindowDays)
	}
	if config.ProfilerConcurrency != 4 {
		t.Errorf("ProfilerConcurrency = %d, want 4", config.ProfilerConcurrency)
	}
	if config.ProfilerLocalTimezone != "Europe/Berlin" {
		t.Errorf("ProfilerLocalTimezone = %q, want Europe/Berlin", config.ProfilerLocalTimezone)
	}
	// SPEC §5.6. The username claim and the workspace path are the two that would
	// be wrong quietly: a spawn addressed to the subject 404s for every developer,
	// and a workspace outside the mounted PVC loses every file the first time the
	// pod is culled.
	if config.JupyterhubUsernameClaim != "preferred_username" {
		t.Errorf("JupyterhubUsernameClaim = %q, want preferred_username",
			config.JupyterhubUsernameClaim)
	}
	if config.JupyterhubWorkspacePath != "data/ode" {
		t.Errorf("JupyterhubWorkspacePath = %q, want data/ode", config.JupyterhubWorkspacePath)
	}
	if config.JupyterhubKernel != "python3" {
		t.Errorf("JupyterhubKernel = %q, want python3", config.JupyterhubKernel)
	}
	// The keep-alive has to stay well below the cluster's cull timeout, and ODE
	// has to let go long before a pod is worth keeping for someone who left.
	if config.JupyterhubKeepaliveInterval != "5m" || config.JupyterhubIdleTimeout != "2h" {
		t.Errorf("keepalive = %q, idle timeout = %q, want 5m and 2h",
			config.JupyterhubKeepaliveInterval, config.JupyterhubIdleTimeout)
	}
	if config.ToolRunCodeMaxOutputBytes != 8000 {
		t.Errorf("ToolRunCodeMaxOutputBytes = %d, want 8000", config.ToolRunCodeMaxOutputBytes)
	}
}

// The profiler's numeric settings are int64 rather than int because
// HandleEnvironmentVars only knows how to set Int64 among the integer kinds: an
// int field would silently ignore its environment variable.
func TestTheProfilerWindowsCanBeSetFromTheEnvironment(t *testing.T) {
	t.Setenv("PROFILER_RAW_WINDOW_DAYS", "7")
	t.Setenv("PROFILER_RAW_WINDOW_POINTS", "5000")

	config, err := Load(writeConfig(t, `{"api_port":"8080"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.ProfilerRawWindowDays != 7 {
		t.Errorf("ProfilerRawWindowDays = %d, want the environment value 7", config.ProfilerRawWindowDays)
	}
	if config.ProfilerRawWindowPoints != 5000 {
		t.Errorf("ProfilerRawWindowPoints = %d, want the environment value 5000", config.ProfilerRawWindowPoints)
	}
}

func TestAnExplicitRealmRoleSurvivesTheDefaults(t *testing.T) {
	config, err := Load(writeConfig(t, `{"required_realm_role":"ode-user"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.RequiredRealmRole != "ode-user" {
		t.Errorf("RequiredRealmRole = %q, want ode-user", config.RequiredRealmRole)
	}
}

func TestEnvironmentVariablesOverrideTheFile(t *testing.T) {
	t.Setenv("API_PORT", "7777")
	t.Setenv("DEVICE_REPO_URL", "http://from-env:8080")
	t.Setenv("DEBUG", "false")

	config, err := Load(writeConfig(t, `{"api_port":"8080","debug":true,"device_repo_url":"http://from-file:8080"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.ApiPort != "7777" {
		t.Errorf("ApiPort = %q, want the environment value", config.ApiPort)
	}
	if config.DeviceRepoUrl != "http://from-env:8080" {
		t.Errorf("DeviceRepoUrl = %q, want the environment value", config.DeviceRepoUrl)
	}
	if config.Debug {
		t.Error("Debug = true, want the environment value false")
	}
}

func TestEnvironmentVariablesFillSlices(t *testing.T) {
	t.Setenv("CORS_ORIGINS", "http://a.example.org, http://b.example.org")

	config, err := Load(writeConfig(t, `{"api_port":"8080"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"http://a.example.org", "http://b.example.org"}
	if len(config.CorsOrigins) != len(want) {
		t.Fatalf("CorsOrigins = %v, want %v", config.CorsOrigins, want)
	}
	for i := range want {
		if config.CorsOrigins[i] != want[i] {
			t.Fatalf("CorsOrigins = %v, want %v (whitespace must be trimmed)", config.CorsOrigins, want)
		}
	}
}

// The environment is applied before the defaults, so a value supplied only via
// the environment must still suppress its default.
func TestAnEnvironmentSuppliedRoleSuppressesTheDefault(t *testing.T) {
	t.Setenv("REQUIRED_REALM_ROLE", "ode-user")

	config, err := Load(writeConfig(t, `{"api_port":"8080"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.RequiredRealmRole != "ode-user" {
		t.Errorf("RequiredRealmRole = %q, want ode-user", config.RequiredRealmRole)
	}
}

func TestFieldNameToEnvName(t *testing.T) {
	cases := map[string]string{
		"ApiPort":               "API_PORT",
		"DeviceRepoUrl":         "DEVICE_REPO_URL",
		"OntologyInvalidateInt": "ONTOLOGY_INVALIDATE_INT",
		"Debug":                 "DEBUG",
	}
	for field, want := range cases {
		if got := fieldNameToEnvName(field); got != want {
			t.Errorf("fieldNameToEnvName(%q) = %q, want %q", field, got, want)
		}
	}
}

// The point of types.Secret is that a careless print cannot leak the value. This
// pins the property rather than the type: a future refactor that swaps the field
// back to a plain string would compile and would fail here.
func TestSecretsDoNotSurviveBeingPrintedOrMarshalled(t *testing.T) {
	const key = "sk-ant-do-not-log-me"

	config, err := Load(writeConfig(t, `{"api_port":"8080","anthropic_api_key":"`+key+`",
		"postgres_url":"postgres://user:hunter2@localhost/ode",
		"jupyterhub_token":"`+key+`","compatible_api_key":"`+key+`","openai_api_key":"`+key+`"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The real value is still reachable where it is needed.
	if config.AnthropicApiKey.Value() != key {
		t.Fatalf("AnthropicApiKey.Value() = %q, want %q", config.AnthropicApiKey.Value(), key)
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, rendering := range []string{
		fmt.Sprintf("%v", config),
		fmt.Sprintf("%+v", *config),
		fmt.Sprint(config.AnthropicApiKey),
		string(encoded),
	} {
		if strings.Contains(rendering, key) {
			t.Errorf("an api key reached a rendering of the config: %s", rendering)
		}
		if strings.Contains(rendering, "hunter2") {
			t.Errorf("the postgres password reached a rendering of the config: %s", rendering)
		}
	}
}

// An environment override of a secret must not put the value in the log either,
// which is the one thing the old config:"secret" tag did and the type now does.
func TestAnEnvironmentSuppliedSecretIsNotLogged(t *testing.T) {
	const token = "hub-token-do-not-log-me"
	t.Setenv("JUPYTERHUB_TOKEN", token)
	t.Setenv("JUPYTERHUB_URL", "http://hub.example.org")

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	defer slog.SetDefault(previous)

	config, err := Load(writeConfig(t, `{"api_port":"8080"}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.JupyterhubToken.Value() != token {
		t.Fatalf("JupyterhubToken.Value() = %q, want %q", config.JupyterhubToken.Value(), token)
	}

	if strings.Contains(logged.String(), token) {
		t.Errorf("the token reached the log: %s", logged.String())
	}
	// The non-secret override beside it is still reported in full, so the trace
	// stays useful.
	if !strings.Contains(logged.String(), "http://hub.example.org") {
		t.Errorf("a non-secret override was not logged: %s", logged.String())
	}
	if !strings.Contains(logged.String(), "JUPYTERHUB_TOKEN") {
		t.Errorf("the secret override was not reported at all: %s", logged.String())
	}
}
