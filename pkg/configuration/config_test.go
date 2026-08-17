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
	"os"
	"path/filepath"
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
