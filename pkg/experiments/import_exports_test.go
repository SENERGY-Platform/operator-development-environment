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

package experiments_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/analytics-flow-engine/lib/access"
	"github.com/SENERGY-Platform/analytics-flow-engine/lib/exports"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

const (
	importID    = "urn:infai:ses:import:a50aa583-282e-56c2-b101-7aaf68ebd2b9"
	importTopic = "urn_infai_ses_import_a50aa583-282e-56c2-b101-7aaf68ebd2b9"
)

func importInputTopics() []experiments.InputTopic {
	return []experiments.InputTopic{{
		Name: importTopic, FilterType: "ImportId", FilterValue: importID,
		Mappings: []experiments.TopicMapping{
			{Dest: "temp", Source: "value.instant_air_temperature"},
		},
	}}
}

func exportOf(id string) exports.Export {
	return exports.Export{
		ID: id, Topic: importTopic, Filter: importID, FilterType: "import_id",
		Database:       "ca4d1149-e3ed-4e0b-9e49-3bda908de436",
		ExportDatabase: exports.ExportDatabase{Type: "timescaledb", EwFilterTopic: "other"},
		Values: []exports.ExportValue{
			{Name: "instant_air_temperature", Path: "value.instant_air_temperature"},
		},
	}
}

type fakeLister struct {
	found []exports.Export
	err   error
	calls int
	auth  string
}

func (f *fakeLister) ListExports(_ context.Context, authorization string, _, _ int64) ([]exports.Export, int64, error) {
	f.calls++
	f.auth = authorization
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.found, int64(len(f.found)), nil
}

// executeOnly allows every resource except the export ids it was not given.
type executeOnly struct{ denied map[string]bool }

func (e executeOnly) UserHasExecuteAccess(resource string, ids []string, _ string) (bool, error) {
	if resource != exports.ResourceExports {
		return true, nil
	}
	for _, id := range ids {
		if e.denied[id] {
			return false, nil
		}
	}
	return true, nil
}

func withExports(l experiments.ExportLister) options {
	return func(d *experiments.Deps) { d.Exports = l }
}

func launchImport(h *harness) (experiments.LaunchResult, error) {
	return h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: importInputTopics()})
}

type importExportsConfig struct {
	Config map[string]json.RawMessage `json:"config"`
}

func configOf(t *testing.T, h *harness) importExportsConfig {
	t.Helper()
	var c importExportsConfig
	if err := json.Unmarshal([]byte(h.ray.LastJob(t).RuntimeEnv.EnvVars["CONFIG"]), &c); err != nil {
		t.Fatalf("CONFIG: %v", err)
	}
	return c
}

func hasWarning(warnings []string, parts ...string) bool {
	for _, w := range warnings {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(w, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// One export the developer may execute: it is named in the config, with the
// table and the column of the mapped source, and the launch says so.
func TestALaunchNamesTheExportOfAnImportInput(t *testing.T) {
	lister := &fakeLister{found: []exports.Export{exportOf("e31775ba-4bf5-46a5-ba8f-81adb3977104")}}
	h := newHarness(t, withExports(lister))
	h.ready()

	result, err := launchImport(h)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	raw, ok := configOf(t, h).Config["import_exports"]
	if !ok {
		t.Fatal("CONFIG has no import_exports although the developer may execute an export")
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatalf("import_exports is not a JSON string: %v (%s)", err, raw)
	}
	var entries []exports.ImportExport
	if err := json.Unmarshal([]byte(encoded), &entries); err != nil {
		t.Fatalf("import_exports does not decode: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	e := entries[0]
	if e.Topic != importTopic || e.ImportID != importID ||
		e.ExportID != "e31775ba-4bf5-46a5-ba8f-81adb3977104" ||
		e.Table != "userid:yk0RSePtTgueSTvakI3kNg_export:4xd1ukv1RqW6j4Gts5dxBA" ||
		e.Columns["value.instant_air_temperature"] != "instant_air_temperature" {
		t.Errorf("entry = %+v", e)
	}
	if !hasWarning(result.Warnings, importTopic, "history from export e31775ba") {
		t.Errorf("warnings = %v, want the export named", result.Warnings)
	}
	if lister.auth == "" {
		t.Error("the listing was read without the developer's authorization")
	}
}

// No export: Kafka as before, the key absent, and the launch says why.
func TestALaunchWithoutAnExportReadsKafkaAndSaysSo(t *testing.T) {
	h := newHarness(t, withExports(&fakeLister{}))
	h.ready()

	result, err := launchImport(h)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, ok := configOf(t, h).Config["import_exports"]; ok {
		t.Error("CONFIG carries import_exports although no export exists")
	}
	if !hasWarning(result.Warnings, importTopic, "Kafka only") {
		t.Errorf("warnings = %v, want a Kafka-only warning", result.Warnings)
	}
}

// An export the developer may read but not execute is no export: timescale-wrapper
// would refuse it, so the run falls back to Kafka instead of failing inside train().
func TestAnExportTheDeveloperCannotExecuteIsNotUsed(t *testing.T) {
	lister := &fakeLister{found: []exports.Export{exportOf("e31775ba-4bf5-46a5-ba8f-81adb3977104")}}
	h := newHarness(t, withExports(lister), withPermissions(executeOnly{
		denied: map[string]bool{"e31775ba-4bf5-46a5-ba8f-81adb3977104": true},
	}))
	h.ready()

	result, err := launchImport(h)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, ok := configOf(t, h).Config["import_exports"]; ok {
		t.Error("CONFIG names an export the developer may not execute")
	}
	if !hasWarning(result.Warnings, "Kafka only") {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

// An export that lacks a mapped path cannot serve the operator. Guards the
// mapping hand-over to lib/exports: without it this case would match vacuously.
func TestAnExportMissingAMappedPathIsNotUsed(t *testing.T) {
	e := exportOf("e31775ba-4bf5-46a5-ba8f-81adb3977104")
	e.Values = []exports.ExportValue{{Name: "other", Path: "value.other"}}
	h := newHarness(t, withExports(&fakeLister{found: []exports.Export{e}}))
	h.ready()

	if _, err := launchImport(h); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, ok := configOf(t, h).Config["import_exports"]; ok {
		t.Error("CONFIG names an export that does not cover the mapping")
	}
}

// Two exports the developer may execute: the developer decides, the launch does
// not guess and does not spend anything.
func TestAmbiguousExportsRefuseTheLaunch(t *testing.T) {
	lister := &fakeLister{found: []exports.Export{
		exportOf("e31775ba-4bf5-46a5-ba8f-81adb3977104"),
		exportOf("11111111-4bf5-46a5-ba8f-81adb3977104"),
	}}
	h := newHarness(t, withExports(lister))
	h.ready()

	_, err := launchImport(h)
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	for _, want := range []string{"e31775ba", "11111111", importTopic, "choose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(h.ray.Jobs()) != 0 {
		t.Errorf("%d jobs submitted, want none", len(h.ray.Jobs()))
	}
}

// A listing that fails is not "no export".
func TestAFailingExportListingRefusesTheLaunch(t *testing.T) {
	h := newHarness(t, withExports(&fakeLister{err: errors.New("connection refused")}))
	h.ready()

	_, err := launchImport(h)
	var upstream *experiments.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %v, want an UpstreamError so the route answers 502", err)
	}
	if len(h.ray.Jobs()) != 0 {
		t.Errorf("%d jobs submitted, want none", len(h.ray.Jobs()))
	}
}

// A permissions failure while checking a candidate is not "not permitted" either.
func TestAFailingExecuteCheckOnAnExportRefusesTheLaunch(t *testing.T) {
	h := newHarness(t,
		withExports(&fakeLister{found: []exports.Export{exportOf("e31775ba-4bf5-46a5-ba8f-81adb3977104")}}),
		withPermissions(failingOnExports{}))
	h.ready()

	_, err := launchImport(h)
	var upstream *experiments.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %v, want an UpstreamError", err)
	}
}

type failingOnExports struct{}

func (failingOnExports) UserHasExecuteAccess(resource string, _ []string, _ string) (bool, error) {
	if resource == exports.ResourceExports {
		return false, errors.New("permissions unreachable")
	}
	return true, nil
}

// No analytics-serving configured: nothing is looked up, the key is absent, and
// every import topic is named as Kafka only.
func TestALaunchWithoutAnExportListerSaysKafkaOnly(t *testing.T) {
	h := newHarness(t)
	h.ready()

	result, err := launchImport(h)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, ok := configOf(t, h).Config["import_exports"]; ok {
		t.Error("CONFIG carries import_exports with no lister configured")
	}
	if !hasWarning(result.Warnings, importTopic, "Kafka topic only") {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

// A device-only launch is untouched: no listing, no key, no warning.
func TestADeviceLaunchDoesNotLookUpExports(t *testing.T) {
	lister := &fakeLister{}
	h := newHarness(t, withExports(lister))
	h.ready()

	result := h.launch()
	if lister.calls != 0 {
		t.Errorf("%d listings for a launch with no import input", lister.calls)
	}
	if _, ok := configOf(t, h).Config["import_exports"]; ok {
		t.Error("CONFIG carries import_exports for a device-only launch")
	}
	if hasWarning(result.Warnings, "import input") {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

var _ access.Checker = executeOnly{}
