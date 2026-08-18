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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type ConfigStruct struct {
	ApiPort string `json:"api_port"`
	Debug   bool   `json:"debug"`

	// RequiredRealmRole gates every authenticated route (SPEC D5, §3.1).
	// Token signature, expiry and audience are validated by the platform API
	// gateway, not here.
	RequiredRealmRole string `json:"required_realm_role"`

	// Platform services. Read on behalf of the calling user (SPEC §3.1 step 3).
	DeviceRepoUrl         string `json:"device_repo_url"`
	TimescaleWrapperUrl   string `json:"timescale_wrapper_url"`
	OntologyCacheTtl      string `json:"ontology_cache_ttl"`
	OntologyInvalidateInt string `json:"ontology_invalidate_interval"`

	// TimeseriesRequestTimeout bounds a single timescale-wrapper request. A
	// profiler pass over a long window is the slowest thing ODE asks of the
	// platform, so this is generous by design.
	TimeseriesRequestTimeout string `json:"timeseries_request_timeout"`

	// Profiler windows (SPEC D25). The raw pass reads the smaller of these two
	// bounds, anchored at the most recent data.
	//
	// int64 rather than int deliberately: HandleEnvironmentVars only knows how to
	// set Int64 among the integer kinds, so an int field would silently ignore
	// its environment variable.
	ProfilerRawWindowDays      int64 `json:"profiler_raw_window_days"`
	ProfilerRawWindowPoints    int64 `json:"profiler_raw_window_points"`
	ProfilerCoverageWindowDays int64 `json:"profiler_coverage_window_days"`
	ProfilerConcurrency        int64 `json:"profiler_concurrency"`

	// ProfilerLocalTimezone is used only to flag DST transition windows.
	// Computation stays in UTC throughout (§5.4.13).
	ProfilerLocalTimezone string `json:"profiler_local_timezone"`

	// Semantic selection (§5.2). SelectionMaxCriteria is the one worth
	// understanding: a resolution sends one device-type-selectables request per
	// combination of matched function and matched aspect, because the platform ANDs
	// a criteria list, and this caps how many.
	SelectionConcurrency int64 `json:"selection_concurrency"`
	SelectionMaxCriteria int64 `json:"selection_max_criteria"`
	SelectionDeviceLimit int64 `json:"selection_device_limit"`

	// PostgresUrl persists what §3.3 makes load-bearing: LLM spend accounting, the
	// exposure-tier audit trail, chat sessions, and the profiler's override overlay.
	// Empty runs the in-memory stores instead, which validate() warns about — a
	// spend cap computed in memory resets on every restart.
	//
	// Marked secret so HandleEnvironmentVars does not print the DSN, which carries
	// a password.
	PostgresUrl      string `json:"postgres_url" config:"secret"`
	PostgresMaxConns int64  `json:"postgres_max_conns"`

	// LLM providers (§5.7, D7). Each is configured independently and any subset may
	// be present; a deployment with none serves M0–M2 and no chat.
	//
	// The API keys are the central key of D8, and are accounted per platform user
	// rather than issued per user.
	AnthropicApiKey  string   `json:"anthropic_api_key" config:"secret"`
	AnthropicBaseUrl string   `json:"anthropic_base_url"`
	AnthropicModels  []string `json:"anthropic_models"`

	OpenaiApiKey  string   `json:"openai_api_key" config:"secret"`
	OpenaiBaseUrl string   `json:"openai_base_url"`
	OpenaiModels  []string `json:"openai_models"`

	// The OpenAI-compatible row of §5.7: vLLM, Ollama, Azure. CompatibleTools says
	// whether that server implements function calling, because ODE cannot find out
	// without trying and a wrong assumption means tools that silently never fire.
	CompatibleName    string   `json:"compatible_name"`
	CompatibleBaseUrl string   `json:"compatible_base_url"`
	CompatibleApiKey  string   `json:"compatible_api_key" config:"secret"`
	CompatibleModels  []string `json:"compatible_models"`
	CompatibleTools   bool     `json:"compatible_tools"`

	// The local `claude` CLI, for development without an API key. Reaches ODE's
	// tools over MCP, so PublicUrl has to be set for its tools to work.
	ClaudeCliEnabled bool     `json:"claude_cli_enabled"`
	ClaudeCliBinary  string   `json:"claude_cli_binary"`
	ClaudeCliModels  []string `json:"claude_cli_models"`

	// PublicUrl is how a subprocess reaches this ODE. Needed only by the CLI
	// provider, which points the CLI's MCP client back at /mcp.
	PublicUrl string `json:"public_url"`

	// LlmMaxTokens bounds one response; LlmEffort maps to Anthropic's
	// output_config.effort; LlmAdaptiveThinking sends thinking: {type: "adaptive"}.
	LlmMaxTokens        int64  `json:"llm_max_tokens"`
	LlmEffort           string `json:"llm_effort"`
	LlmAdaptiveThinking bool   `json:"llm_adaptive_thinking"`
	// LlmMaxToolIterations bounds the tool loop, so a model that never concludes is
	// stopped by control flow rather than by the spend cap.
	LlmMaxToolIterations int64 `json:"llm_max_tool_iterations"`

	// Pricing is what a million tokens costs, per model, used for §3.3's estimated
	// cost. Not baked into the binary: published prices change, and a stale
	// hard-coded figure would make a spend cap quietly wrong.
	LlmCurrency string       `json:"llm_currency"`
	LlmPricing  []ModelPrice `json:"llm_pricing"`

	// ProfilerReadTimeout bounds one value-reading request to the platform,
	// separately from TimeseriesRequestTimeout.
	//
	// The two are not comparable. A raw pass bounded at a hundred thousand points is
	// megabytes of JSON the server has to assemble; an availability probe answers
	// from metadata. One shared timeout means either the probe waits far too long to
	// fail or the read is cut off mid-assembly.
	ProfilerReadTimeout string `json:"profiler_read_timeout"`

	// ChatExchangeTimeout bounds one detached exchange.
	//
	// An exchange no longer ends with the connection that started it, so it needs a
	// ceiling of its own: without one, a wedged provider or a hung platform read
	// would hold a session's turn slot and a goroutine indefinitely.
	ChatExchangeTimeout string `json:"chat_exchange_timeout"`

	// Tool surface bounds (§5.8). ToolProfileTokenBudget caps the projection handed
	// to the model (D26); ToolPreviewMaxPoints caps a tier-L2 preview, which is what
	// keeps "downsampled preview" from becoming a raw series read (§4).
	//
	// The other two bound breadth rather than depth, which a per-item budget cannot:
	// ToolProfileMaxProfiles caps how many profiles one profile_series response
	// carries, and ToolQuickTokenBudget caps the ranked candidate list of the L0
	// tools. Without them a wide device set or a twenty-variable service answers
	// with a payload many times the size of the budget above it.
	ToolProfileTokenBudget int64 `json:"tool_profile_token_budget"`
	ToolProfileMaxProfiles int64 `json:"tool_profile_max_profiles"`
	ToolQuickTokenBudget   int64 `json:"tool_quick_token_budget"`
	ToolPreviewMaxPoints   int64 `json:"tool_preview_max_points"`

	CorsOrigins []string `json:"cors_origins"`
}

// ModelPrice mirrors llm.ModelPrice. Duplicated rather than imported so that
// pkg/configuration keeps depending on nothing inside ODE — every other package
// depends on it, and an import the other way would be a cycle waiting to happen.
type ModelPrice struct {
	Model              string  `json:"model"`
	InputPerMTok       float64 `json:"input_per_mtok"`
	OutputPerMTok      float64 `json:"output_per_mtok"`
	CachedInputPerMTok float64 `json:"cached_input_per_mtok"`
}

type Config = *ConfigStruct

func Load(location string) (config Config, err error) {
	file, err := os.Open(location)
	if err != nil {
		log.Println("error on config load: ", err)
		return config, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		log.Println("invalid config json: ", err)
		return config, err
	}
	HandleEnvironmentVars(config)
	applyDefaults(config)
	return config, nil
}

// applyDefaults fills the operational values a deployment need not restate.
func applyDefaults(config Config) {
	if config.RequiredRealmRole == "" {
		config.RequiredRealmRole = "developer"
	}
	if config.OntologyCacheTtl == "" {
		config.OntologyCacheTtl = "1h"
	}
	if config.OntologyInvalidateInt == "" {
		config.OntologyInvalidateInt = "5m"
	}
	if config.TimeseriesRequestTimeout == "" {
		config.TimeseriesRequestTimeout = "60s"
	}
	if config.ProfilerRawWindowDays <= 0 {
		config.ProfilerRawWindowDays = 14
	}
	if config.ProfilerRawWindowPoints <= 0 {
		config.ProfilerRawWindowPoints = 100000
	}
	if config.ProfilerCoverageWindowDays <= 0 {
		config.ProfilerCoverageWindowDays = 90
	}
	if config.ProfilerConcurrency <= 0 {
		config.ProfilerConcurrency = 4
	}
	if config.ProfilerLocalTimezone == "" {
		config.ProfilerLocalTimezone = "Europe/Berlin"
	}
	if config.SelectionConcurrency <= 0 {
		config.SelectionConcurrency = 4
	}
	if config.SelectionMaxCriteria <= 0 {
		config.SelectionMaxCriteria = 12
	}
	if config.SelectionDeviceLimit <= 0 {
		config.SelectionDeviceLimit = 10
	}
	if config.PostgresMaxConns <= 0 {
		config.PostgresMaxConns = 8
	}
	if config.LlmMaxTokens <= 0 {
		config.LlmMaxTokens = 8192
	}
	if config.LlmMaxToolIterations <= 0 {
		config.LlmMaxToolIterations = 12
	}
	if config.LlmCurrency == "" {
		config.LlmCurrency = "EUR"
	}
	if config.ToolProfileTokenBudget <= 0 {
		config.ToolProfileTokenBudget = 4000
	}
	if config.ToolProfileMaxProfiles <= 0 {
		config.ToolProfileMaxProfiles = 4
	}
	if config.ToolQuickTokenBudget <= 0 {
		config.ToolQuickTokenBudget = 4000
	}
	if config.ToolPreviewMaxPoints <= 0 {
		config.ToolPreviewMaxPoints = 500
	}
	if config.ProfilerReadTimeout == "" {
		config.ProfilerReadTimeout = "300s"
	}
	if config.ChatExchangeTimeout == "" {
		config.ChatExchangeTimeout = "30m"
	}
	if config.CompatibleName == "" {
		config.CompatibleName = "openai-compatible"
	}
	if config.ClaudeCliBinary == "" {
		config.ClaudeCliBinary = "claude"
	}
}

var camel = regexp.MustCompile("(^[^A-Z]*|[A-Z]*)([A-Z][^A-Z]+|$)")

func fieldNameToEnvName(s string) string {
	var a []string
	for _, sub := range camel.FindAllStringSubmatch(s, -1) {
		if sub[1] != "" {
			a = append(a, sub[1])
		}
		if sub[2] != "" {
			a = append(a, sub[2])
		}
	}
	return strings.ToUpper(strings.Join(a, "_"))
}

// HandleEnvironmentVars overrides config fields from the environment, for docker.
func HandleEnvironmentVars(config Config) {
	configValue := reflect.Indirect(reflect.ValueOf(config))
	configType := configValue.Type()
	for index := 0; index < configType.NumField(); index++ {
		fieldName := configType.Field(index).Name
		fieldConfig := configType.Field(index).Tag.Get("config")
		envName := fieldNameToEnvName(fieldName)
		envValue := os.Getenv(envName)
		if envValue == "" {
			continue
		}
		if !strings.Contains(fieldConfig, "secret") {
			fmt.Println("use environment variable: ", envName, " = ", envValue)
		}
		field := configValue.FieldByName(fieldName)
		switch field.Kind() {
		case reflect.Int64:
			i, _ := strconv.ParseInt(envValue, 10, 64)
			field.SetInt(i)
		case reflect.Uint16:
			i, _ := strconv.ParseUint(envValue, 10, 16)
			field.SetUint(i)
		case reflect.Float64:
			f, _ := strconv.ParseFloat(envValue, 64)
			field.SetFloat(f)
		case reflect.String:
			field.SetString(envValue)
		case reflect.Bool:
			b, _ := strconv.ParseBool(envValue)
			field.SetBool(b)
		case reflect.Slice:
			val := []string{}
			for _, element := range strings.Split(envValue, ",") {
				val = append(val, strings.TrimSpace(element))
			}
			field.Set(reflect.ValueOf(val))
		case reflect.Map:
			value := map[string]string{}
			for _, element := range strings.Split(envValue, ",") {
				keyVal := strings.SplitN(element, ":", 2)
				if len(keyVal) != 2 {
					continue
				}
				value[strings.TrimSpace(keyVal[0])] = strings.TrimSpace(keyVal[1])
			}
			field.Set(reflect.ValueOf(value))
		}
	}
}
