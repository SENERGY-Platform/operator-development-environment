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

	CorsOrigins []string `json:"cors_origins"`
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
