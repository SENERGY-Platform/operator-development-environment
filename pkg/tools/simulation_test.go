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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
)

// The same session id the import tests use, because newFakeCreations keys its
// pre-seeded entries by it: a delete gate that was tested against a different
// session would be testing the mismatch rather than the gate.
const testSimulationSession = testSessionID

// fakeSimulation is MOSES as these tests need it: a store that counts versions
// and refuses a stale write, because that is the contract the executors are
// written against.
//
// It is not simulationtest. That double is an HTTP MOSES and belongs to the
// client's tests; here what is under test is the executor, and a fake at the
// interface says which method was called with what — which is the assertion
// nearly every test below makes.
type fakeSimulation struct {
	environments map[string]simulation.Environment
	deviceTypes  []simulation.DeviceType
	datasets     []simulation.Dataset
	state        simulation.EnvironmentState
	status       simulation.BackfillStatus

	// The errors a test installs to reach a refusal path.
	getErr      error
	createErr   error
	replaceErr  error
	patchErr    error
	backfillErr error
	statusErr   error
	uploadErr   error

	// What the executors sent, so a test can assert the document rather than the
	// call count.
	created  []simulation.Environment
	replaced []simulation.Environment
	deleted  []string
	patched  []simulation.StateChange
	windows  [][2]time.Time
	uploads  []fakeUpload
	nextID   int
}

type fakeUpload struct {
	Name     string
	Timezone string
	Content  string
}

func (f *fakeSimulation) List(context.Context, string) ([]simulation.Environment, error) {
	out := []simulation.Environment{}
	for _, environment := range f.environments {
		out = append(out, environment)
	}
	return out, nil
}

func (f *fakeSimulation) Get(_ context.Context, _ string, id string) (simulation.Environment, error) {
	if f.getErr != nil {
		return simulation.Environment{}, f.getErr
	}
	environment, found := f.environments[id]
	if !found {
		return simulation.Environment{}, simulation.ErrNotFound
	}
	return deepCopy(environment), nil
}

func (f *fakeSimulation) Create(_ context.Context, _ string, env simulation.Environment) (simulation.Environment, error) {
	// Recorded as a copy before anything below touches it. The zones are a slice,
	// so provisioning into the returned document would otherwise rewrite the record
	// of what was sent — and the assertion that ODE named no device would pass on
	// an ODE that named one.
	f.created = append(f.created, deepCopy(env))
	if f.createErr != nil {
		return simulation.Environment{}, f.createErr
	}
	f.nextID++
	env.ID = "env-created"
	env.Version = 1
	env.ExternalGraphRef = "graph-1"
	// What MOSES does and what a test of a delete has to see: a device per asset,
	// managed because MOSES created it.
	env.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		f.nextID++
		asset.ExternalRef = "device-" + asset.ID
		asset.ExternalManaged = true
	})
	if f.environments == nil {
		f.environments = map[string]simulation.Environment{}
	}
	f.environments[env.ID] = env
	return env, nil
}

func (f *fakeSimulation) Replace(_ context.Context, _ string, env simulation.Environment) (simulation.Environment, error) {
	f.replaced = append(f.replaced, deepCopy(env))
	if f.replaceErr != nil {
		return simulation.Environment{}, f.replaceErr
	}
	stored := f.environments[env.ID]
	if env.Version != stored.Version {
		return simulation.Environment{}, &simulation.VersionConflict{
			ID: env.ID, Carried: env.Version,
			Detail: fmt.Sprintf("expected version %d, stored version is %d", env.Version, stored.Version),
		}
	}
	env.Version = stored.Version + 1
	env.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.ExternalRef == "" && asset.ExternalTypeId != "" {
			asset.ExternalRef = "device-" + asset.ID
			asset.ExternalManaged = true
		}
	})
	f.environments[env.ID] = env
	return env, nil
}

func (f *fakeSimulation) Delete(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	delete(f.environments, id)
	return nil
}

func (f *fakeSimulation) State(context.Context, string, string) (simulation.EnvironmentState, error) {
	return f.state, nil
}

func (f *fakeSimulation) Patch(_ context.Context, _ string, _ string, change simulation.StateChange) error {
	f.patched = append(f.patched, change)
	return f.patchErr
}

func (f *fakeSimulation) Backfill(_ context.Context, _ string, id string, from, to time.Time) (simulation.BackfillStatus, error) {
	f.windows = append(f.windows, [2]time.Time{from, to})
	if f.backfillErr != nil {
		return simulation.BackfillStatus{}, f.backfillErr
	}
	return simulation.BackfillStatus{
		EnvironmentID: id, State: simulation.BackfillRunning, From: from, To: to,
	}, nil
}

func (f *fakeSimulation) BackfillStatusOf(context.Context, string, string) (simulation.BackfillStatus, error) {
	if f.statusErr != nil {
		return simulation.BackfillStatus{}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeSimulation) DeviceTypes(context.Context, string) ([]simulation.DeviceType, error) {
	return f.deviceTypes, nil
}

func (f *fakeSimulation) Datasets(context.Context, string) ([]simulation.Dataset, error) {
	return f.datasets, nil
}

func (f *fakeSimulation) UploadDataset(_ context.Context, _ string, name, timezone string, content []byte) (simulation.Dataset, error) {
	f.uploads = append(f.uploads, fakeUpload{Name: name, Timezone: timezone, Content: string(content)})
	if f.uploadErr != nil {
		return simulation.Dataset{}, f.uploadErr
	}
	return simulation.Dataset{
		ID: "dataset-1", Name: name, Timezone: orDefault(timezone, "Europe/Berlin"),
		SizeBytes: int64(len(content)),
		Columns: []simulation.DatasetColumn{
			{Name: "power", Points: 720, FromUnix: 1750000000, ToUnix: 1752592000},
		},
	}, nil
}

func (f *fakeSimulation) MaxDatasetBytes() int { return 1 << 20 }

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// deepCopy is what a real Get gives back: a document the caller can edit without
// the store seeing the edit. Without it the read-modify-write tests would pass
// on a shared pointer and prove nothing about the write.
func deepCopy(environment simulation.Environment) simulation.Environment {
	encoded, _ := json.Marshal(environment)
	var out simulation.Environment
	_ = json.Unmarshal(encoded, &out)
	out.UnknownField = environment.UnknownField
	return out
}

func simulationCatalogue() []simulation.DeviceType {
	return []simulation.DeviceType{{
		ID:   "type-inverter",
		Name: "Simulated inverter",
		Services: []simulation.DeviceTypeService{
			{ID: "service-power", Name: "Power", Direction: simulation.Sensor, CharacteristicId: "characteristic-watt", ValuePath: "value"},
			{ID: "service-energy", Name: "Energy", Direction: simulation.Sensor, CharacteristicId: "characteristic-kwh", ValuePath: "value"},
			{ID: "service-limit", Name: "Set limit", Direction: simulation.Actuator, CharacteristicId: "characteristic-watt", ValuePath: "value"},
		},
	}}
}

func storedSimulation() simulation.Environment {
	return simulation.Environment{
		ID:      "env-1",
		Name:    "Werk 2",
		Type:    simulation.IndustrialSite,
		Version: 3,
		Context: map[string]any{"outdoor_temperature": 12.0},
		Zones: []simulation.Zone{{
			ID: "zone-site", Name: "Werk 2", Type: simulation.ZoneSite,
			InitialStates: map[string]any{},
			Assets: []simulation.Asset{{
				ID: "asset-meter", Name: "Hall meter", Kind: simulation.AssetMeter,
				ExternalRef: "device-meter", ExternalTypeId: "type-inverter", ExternalManaged: true,
				InitialStates: map[string]any{},
				Channels: []simulation.Channel{{
					ID: "channel-meter-power", Name: "Hall power", Direction: simulation.Sensor,
					ExternalRef: "service-power", CharacteristicId: "characteristic-watt",
					IntervalSeconds: 60,
					Source:          simulation.Source{Kind: simulation.SourceAggregate},
				}},
			}},
			Zones: []simulation.Zone{{
				ID: "zone-hall", Name: "Hall", Type: simulation.ZoneHall,
				InitialStates: map[string]any{},
				Assets: []simulation.Asset{{
					ID: "asset-machine-1", Name: "Machine 1", Kind: simulation.AssetMachine,
					ExternalRef: "device-machine-1", ExternalTypeId: "type-inverter",
					ExternalManaged: true, SubmeteredBy: "asset-meter",
					InitialStates: map[string]any{},
					Channels: []simulation.Channel{{
						ID: "channel-machine-1-power", Name: "Power", Direction: simulation.Sensor,
						ExternalRef: "service-power", CharacteristicId: "characteristic-watt",
						IntervalSeconds: 60,
						Source: simulation.Source{
							Kind:    simulation.SourceProfile,
							Profile: &simulation.ProfileSource{Base: 4000, HourFactors: make([]float64, 24)},
						},
					}},
				}},
			}},
		}},
	}
}

func simulationDeps(sim *fakeSimulation, extra ...func(*Deps)) Deps {
	deps := Deps{Simulation: sim, Creations: newFakeCreations(), DeviceLimit: 10}
	for _, apply := range extra {
		apply(&deps)
	}
	return deps
}

func dispatchSimulationTool(t *testing.T, deps Deps, name string, input any) Result {
	t.Helper()
	definition, dispatcher := executorFor(t, deps, name)
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := Request{
		Token: "Bearer t", UserSub: "sub-1",
		SessionID: testSimulationSession, WorkbenchID: "wb-1", Tier: L0,
	}
	result := dispatcher.Dispatch(context.Background(), request, Call{ID: "c1", Name: name, Input: raw})
	if definition.Confirm && result.Outcome == OutcomeAwaitingConfirmation {
		// Nothing is applied on the model's word alone, so the test agrees the way a
		// developer would rather than reaching past the gate.
		return dispatcher.Confirm(context.Background(), request, *result.Confirmation)
	}
	return result
}

func callSimulationTool(t *testing.T, deps Deps, name string, input any) map[string]any {
	t.Helper()
	result := dispatchSimulationTool(t, deps, name, input)
	if result.Outcome != OutcomeOK {
		t.Fatalf("%s: outcome %s: %+v", name, result.Outcome, result.Content)
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var answer map[string]any
	if err := json.Unmarshal(encoded, &answer); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	return answer
}

func refuseSimulationTool(t *testing.T, deps Deps, name string, input any, wantSay string) Result {
	t.Helper()
	result := dispatchSimulationTool(t, deps, name, input)
	if result.Outcome != OutcomeInvalidInput {
		t.Fatalf("%s: outcome %s, want invalid_input: %+v", name, result.Outcome, result.Content)
	}
	if wantSay != "" && !strings.Contains(refusalText(result), wantSay) {
		t.Errorf("%s refused with %q, which does not say %q", name, refusalText(result), wantSay)
	}
	return result
}

// refusalText is what the model actually reads back. The refusal is the whole
// product of a bad call, so a test that only checked the outcome would pass on a
// tool that refused with nothing a model could act on.
func refusalText(result Result) string {
	encoded, _ := json.Marshal(result.Content)
	return string(encoded)
}

// ---- reads ----

func TestGetSimulationReportsTheIdsEveryOtherToolTakes(t *testing.T) {
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": storedSimulation()}}
	answer := callSimulationTool(t, simulationDeps(sim), "get_simulation",
		map[string]any{"simulation_id": "env-1"})

	if answer["version"] == nil {
		t.Error("the answer carries no version; the next write has to send it back")
	}
	zones, _ := answer["zones"].([]any)
	if len(zones) != 1 {
		t.Fatalf("zones = %v", answer["zones"])
	}
	site := zones[0].(map[string]any)
	assets := site["assets"].([]any)
	meter := assets[0].(map[string]any)
	if meter["device_id"] != "device-meter" {
		t.Errorf("the meter's device_id is %v, want the platform device — it is what makes a "+
			"simulated asset findable by every other tool", meter["device_id"])
	}
	channel := meter["channels"].([]any)[0].(map[string]any)
	if channel["service_id"] != "service-power" {
		t.Errorf("the channel's service_id is %v", channel["service_id"])
	}
	if channel["characteristic_id"] != "characteristic-watt" {
		t.Errorf("the channel carries no characteristic, so nothing downstream knows its unit")
	}
}

// A document a newer MOSES stored reads, and says it cannot be edited from here.
// Reading it is the point; the refusal is what stops a write that would delete
// the field.
func TestASimulationWithAnUnknownFieldReadsAsReadOnly(t *testing.T) {
	environment := storedSimulation()
	environment.UnknownField = "climate_control"
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": environment}}

	answer := callSimulationTool(t, simulationDeps(sim), "get_simulation",
		map[string]any{"simulation_id": "env-1"})
	if answer["read_only"] != true {
		t.Fatal("a simulation carrying an unknown field is not marked read-only")
	}
	if reason, _ := answer["read_only_reason"].(string); !strings.Contains(reason, "climate_control") {
		t.Errorf("the reason is %q and does not name the field", reason)
	}
}

func TestAPlatformWithNoSimulatableDeviceTypeSaysSoRatherThanAnsweringEmpty(t *testing.T) {
	answer := callSimulationTool(t, simulationDeps(&fakeSimulation{}),
		"list_simulation_device_types", map[string]any{})
	note, _ := answer["note"].(string)
	if !strings.Contains(note, "modelling gap") {
		t.Errorf("note = %q; an empty catalogue is a gap in the device repository and the "+
			"answer has to say so rather than reading as \"nothing matched\"", note)
	}
}

// The tier is the point of this one. Everything else about a simulation is
// structure; the live state is values.
func TestTheLiveStateIsTheOneSimulationReadAboveL0(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		state: simulation.EnvironmentState{
			Running: true, AsOf: time.Now(),
			StateChange: simulation.StateChange{Context: map[string]any{"outdoor_temperature": 21.5}},
		},
	}
	definition, dispatcher := executorFor(t, simulationDeps(sim), "get_simulation_state")
	if definition.MinTier != L1 {
		t.Fatalf("get_simulation_state sits at %v, want L1", definition.MinTier)
	}
	call := Call{ID: "c1", Name: "get_simulation_state",
		Input: json.RawMessage(`{"simulation_id":"env-1"}`)}

	atL0 := dispatcher.Dispatch(context.Background(), Request{Token: "t", Tier: L0}, call)
	if atL0.Outcome != OutcomeBlockedByTier {
		t.Errorf("at L0 the outcome is %s, want blocked_by_tier", atL0.Outcome)
	}
	atL1 := dispatcher.Dispatch(context.Background(), Request{Token: "t", Tier: L1}, call)
	if atL1.Outcome != OutcomeOK {
		t.Errorf("at L1 the outcome is %s: %+v", atL1.Outcome, atL1.Content)
	}
}

func TestAStoredButNotRunningSimulationIsReportedAsSuchRatherThanAsEmpty(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		state:        simulation.EnvironmentState{Running: false},
	}
	_, dispatcher := executorFor(t, simulationDeps(sim), "get_simulation_state")
	result := dispatcher.Dispatch(context.Background(), Request{Token: "t", Tier: L1},
		Call{ID: "c1", Name: "get_simulation_state", Input: json.RawMessage(`{"simulation_id":"env-1"}`)})
	if result.Outcome != OutcomeOK {
		t.Fatalf("outcome %s: %+v", result.Outcome, result.Content)
	}
	encoded, _ := json.Marshal(result.Content)
	if !strings.Contains(string(encoded), "not a failure") {
		t.Errorf("the answer is %s and reads like an empty simulation rather than one that "+
			"is simply not running here", encoded)
	}
}

// ---- authoring ----

func TestCreateSimulationRendersATemplateAndRecordsWhatItMade(t *testing.T) {
	sim := &fakeSimulation{deviceTypes: simulationCatalogue()}
	log := newFakeCreations()
	deps := simulationDeps(sim, func(d *Deps) { d.Creations = log })

	answer := callSimulationTool(t, deps, "create_simulation", map[string]any{
		"template":  "pv_site",
		"name":      "Roof array",
		"seed":      11,
		"rationale": "this platform has no PV generation series at all, and the forecast needs one",
		"params":    map[string]any{"peak_power": 12000},
		"bindings": map[string]any{
			"inverter": map[string]any{
				"device_type_id": "type-inverter",
				"channels":       map[string]any{"power": "service-power", "energy": "service-energy"},
			},
		},
	})

	if answer["simulation_id"] != "env-created" {
		t.Fatalf("simulation_id = %v", answer["simulation_id"])
	}
	if len(sim.created) != 1 {
		t.Fatalf("MOSES saw %d creates", len(sim.created))
	}
	sent := sim.created[0]
	if sent.ID != "" {
		t.Error("the create carried an id; MOSES assigns one and a client-chosen id is a collision waiting")
	}
	if sent.Seed != 11 {
		t.Errorf("seed = %d, want the one that was asked for — it is what makes the scenario "+
			"reproducible", sent.Seed)
	}
	sentChannels := 0
	sent.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.ExternalRef != "" {
			t.Error("the create named a platform device; MOSES provisions one per asset")
		}
		sentChannels += len(asset.Channels)
	})
	if sentChannels != 2 {
		t.Errorf("the rendered document has %d channels, want the power and energy that were bound",
			sentChannels)
	}

	recorded, _ := log.Creations(context.Background(), testSimulationSession)
	if len(recorded) != 1 || recorded[0].Kind != CreatedSimulation {
		t.Fatalf("recorded %v, want the simulation so this session can delete it again", recorded)
	}
	warnings, _ := answer["warnings"].([]any)
	if len(warnings) < 2 {
		t.Errorf("warnings = %v; a create has to say that the devices are now inventory and "+
			"that the simulation has no past", warnings)
	}
}

func TestCreateSimulationRefusesWhatTheTemplateCannotBuild(t *testing.T) {
	sim := &fakeSimulation{deviceTypes: simulationCatalogue()}
	deps := simulationDeps(sim)

	refuseSimulationTool(t, deps, "create_simulation", map[string]any{
		"template": "solar_farm", "name": "x", "rationale": "y",
		"bindings": map[string]any{},
	}, "is not a template")

	refuseSimulationTool(t, deps, "create_simulation", map[string]any{
		"template": "pv_site", "name": "x", "rationale": "y",
		"bindings": map[string]any{},
	}, "needs a device type")

	// The confirmation cannot be argued for without a reason, and a developer
	// cannot confirm what is not argued.
	refuseSimulationTool(t, deps, "create_simulation", map[string]any{
		"template": "pv_site", "name": "x",
		"bindings": map[string]any{"inverter": map[string]any{
			"device_type_id": "type-inverter", "channels": map[string]any{"power": "service-power"}}},
	}, "rationale is required")

	if len(sim.created) != 0 {
		t.Errorf("a refused create reached MOSES anyway: %d calls", len(sim.created))
	}
}

// The write carries the version it was read at, which is the whole of the
// concurrency story. A test that did not assert it would pass on an executor
// that dropped it and then silently overwrote a concurrent edit.
func TestAWriteCarriesTheVersionItWasReadAt(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	callSimulationTool(t, simulationDeps(sim), "add_simulated_asset", map[string]any{
		"simulation_id": "env-1", "zone_id": "zone-hall", "name": "Machine 2",
		"kind": "machine", "device_type_id": "type-inverter", "submetered_by": "asset-meter",
		"rationale": "the hall runs four machines and the scenario has one",
		"channels": []any{map[string]any{
			"service_id": "service-power",
			"source": map[string]any{
				"kind": "profile",
				"profile": map[string]any{
					"base":       4000,
					"day_window": map[string]any{"from_hour": 6, "to_hour": 22},
				},
			},
		}},
	})
	if len(sim.replaced) != 1 {
		t.Fatalf("MOSES saw %d writes", len(sim.replaced))
	}
	if sim.replaced[0].Version != 3 {
		t.Errorf("the write carried version %d, want the 3 it was read at", sim.replaced[0].Version)
	}
}

// A stale write is a race, not a bad argument and not a platform failure. The
// only correct reaction is to read again — so the refusal has to say that, or a
// model will retry the same document forever.
func TestAStaleWriteTellsTheModelToReadAgainRatherThanRetry(t *testing.T) {
	stale := storedSimulation()
	stale.Version = 2
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": stale},
		deviceTypes:  simulationCatalogue(),
	}
	// The store answers a Get at version 2 and then reports 3 as stored, which is
	// the race: somebody wrote in between.
	sim.environments["env-1"] = stale
	sim.replaceErr = &simulation.VersionConflict{
		ID: "env-1", Carried: 2, Detail: "expected version 2, stored version is 3",
	}

	result := refuseSimulationTool(t, simulationDeps(sim), "set_channel_source", map[string]any{
		"simulation_id": "env-1", "channel_id": "channel-machine-1-power",
		"rationale": "replay a real machine instead of a profile",
		"source": map[string]any{
			"kind": "dataset",
			"dataset": map[string]any{
				"origin": "file", "ref": "dataset-1", "column": "power", "resample": "hold",
			},
		},
	}, "Read the simulation again")
	if !strings.Contains(refusalText(result), "Nothing was written") {
		t.Errorf("the refusal is %q and does not say that nothing was written, which is what "+
			"decides whether the model tries to undo something", refusalText(result))
	}
}

func TestSetChannelSourceCanReplayARealDevicesHistory(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	answer := callSimulationTool(t, simulationDeps(sim), "set_channel_source", map[string]any{
		"simulation_id": "env-1", "channel_id": "channel-machine-1-power",
		"rationale": "there is a real machine on this platform whose cycle is what the operator has to find",
		"source": map[string]any{
			"kind": "dataset",
			"dataset": map[string]any{
				"origin": "platform", "ref": "urn:infai:ses:device:real-1",
				"service_ref": "urn:infai:ses:service:power", "column": "value",
				"window": "4w", "resample": "linear", "anchor": "loop",
			},
		},
	})
	if answer["was"] != "profile" || answer["now"] != "dataset" {
		t.Errorf("the answer says %v -> %v", answer["was"], answer["now"])
	}
	if replay, _ := answer["replay"].(string); !strings.Contains(replay, "recording") {
		t.Errorf("replay note = %q; a platform replay is a recording of a window rather than a "+
			"mirror of the device, and a model has to know which", replay)
	}

	written := sim.replaced[0]
	_, channel, found := written.FindChannel("channel-machine-1-power")
	if !found {
		t.Fatal("the channel is gone from the written document")
	}
	if channel.Source.Dataset == nil || channel.Source.Dataset.Window != "4w" {
		t.Errorf("the written source is %+v", channel.Source)
	}
}

func TestASourceIsRefusedForTheReasonItWouldFail(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	deps := simulationDeps(sim)
	base := func(source map[string]any) map[string]any {
		return map[string]any{
			"simulation_id": "env-1", "channel_id": "channel-machine-1-power",
			"rationale": "testing", "source": source,
		}
	}

	for _, testCase := range []struct {
		name    string
		source  map[string]any
		wantSay string
	}{
		{
			name:    "a profile with the wrong number of hour factors",
			source:  map[string]any{"kind": "profile", "profile": map[string]any{"base": 1, "hour_factors": []float64{1, 2, 3}}},
			wantSay: "exactly 24",
		},
		{
			name:    "a replay with no resample mode",
			source:  map[string]any{"kind": "dataset", "dataset": map[string]any{"origin": "file", "ref": "d1"}},
			wantSay: "resample mode",
		},
		{
			name:    "a platform replay with no service",
			source:  map[string]any{"kind": "dataset", "dataset": map[string]any{"origin": "platform", "ref": "d1", "resample": "hold"}},
			wantSay: "needs the service",
		},
		{
			name:    "an unreadable replay window",
			source:  map[string]any{"kind": "dataset", "dataset": map[string]any{"origin": "platform", "ref": "d1", "service_ref": "s1", "column": "value", "resample": "hold", "window": "last month"}},
			wantSay: "unreadable replay window",
		},
		{
			name:    "a script",
			source:  map[string]any{"kind": "script"},
			wantSay: "MOSES UI",
		},
		{
			name:    "a formula with no inputs",
			source:  map[string]any{"kind": "formula", "formula": map[string]any{"expression": "a + b"}},
			wantSay: "needs inputs",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			refuseSimulationTool(t, deps, "set_channel_source", base(testCase.source), testCase.wantSay)
		})
	}
	if len(sim.replaced) != 0 {
		t.Errorf("a refused source reached MOSES anyway: %d writes", len(sim.replaced))
	}
}

// day_window is the shorthand that exists so a caller never has to count to
// twenty-four. If it produced the wrong twenty-four it would be worse than not
// existing, because nothing would report it.
func TestADayWindowBuildsTheTwentyFourFactorsItSaysItDoes(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	callSimulationTool(t, simulationDeps(sim), "set_channel_source", map[string]any{
		"simulation_id": "env-1", "channel_id": "channel-machine-1-power",
		"rationale": "a night shift",
		"source": map[string]any{
			"kind": "profile",
			"profile": map[string]any{
				"base":       1000,
				"day_window": map[string]any{"from_hour": 22, "to_hour": 6, "shape": "block"},
			},
		},
	})
	_, channel, _ := sim.replaced[0].FindChannel("channel-machine-1-power")
	factors := channel.Source.Profile.HourFactors
	if len(factors) != 24 {
		t.Fatalf("%d hour factors, want 24", len(factors))
	}
	for _, hour := range []int{22, 23, 0, 5} {
		if factors[hour] != 1 {
			t.Errorf("hour %d is %g, want the shift running — a window from 22 to 6 wraps past "+
				"midnight", hour, factors[hour])
		}
	}
	for _, hour := range []int{6, 12, 21} {
		if factors[hour] != 0 {
			t.Errorf("hour %d is %g, want the shift over", hour, factors[hour])
		}
	}
}

func TestAnAggregateOnASubMeteredAssetIsRefused(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	refuseSimulationTool(t, simulationDeps(sim), "set_channel_source", map[string]any{
		"simulation_id": "env-1", "channel_id": "channel-machine-1-power",
		"rationale": "testing",
		"source":    map[string]any{"kind": "aggregate"},
	}, "sum a tree it is itself part of")
}

func TestAnActuatorMustNotCarryAnInterval(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	refuseSimulationTool(t, simulationDeps(sim), "add_simulated_asset", map[string]any{
		"simulation_id": "env-1", "zone_id": "zone-hall", "name": "Limiter",
		"kind": "actuator", "device_type_id": "type-inverter", "rationale": "testing",
		"channels": []any{map[string]any{
			"service_id": "service-limit", "interval_seconds": 30,
			"source": map[string]any{"kind": "profile", "profile": map[string]any{"base": 1}},
		}},
	}, "must not carry an interval")
}

func TestAddingAnAssetChecksTheZoneAndTheMeterItPointsAt(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	deps := simulationDeps(sim)
	asset := func(zone, submetered string) map[string]any {
		return map[string]any{
			"simulation_id": "env-1", "zone_id": zone, "name": "Machine 2",
			"kind": "machine", "device_type_id": "type-inverter", "submetered_by": submetered,
			"rationale": "testing",
			"channels": []any{map[string]any{
				"service_id": "service-power",
				"source":     map[string]any{"kind": "profile", "profile": map[string]any{"base": 1}},
			}},
		}
	}
	refuseSimulationTool(t, deps, "add_simulated_asset", asset("zone-invented", ""), "no zone")
	refuseSimulationTool(t, deps, "add_simulated_asset", asset("zone-hall", "asset-invented"),
		"not an asset of this simulation")
}

// ---- driving ----

func TestSettingContextPatchesTheLiveStateAndSaysThatItIsNotTheDefinition(t *testing.T) {
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": storedSimulation()}}
	answer := callSimulationTool(t, simulationDeps(sim), "set_simulation_context", map[string]any{
		"simulation_id": "env-1",
		"rationale":     "see what the operator does when the hall runs hot",
		"context":       map[string]any{"outdoor_temperature": 34.0},
	})
	if len(sim.patched) != 1 {
		t.Fatalf("MOSES saw %d patches", len(sim.patched))
	}
	if sim.patched[0].Context["outdoor_temperature"] != 34.0 {
		t.Errorf("the patch carried %v", sim.patched[0].Context)
	}
	note, _ := answer["note"].(string)
	if !strings.Contains(note, "not the definition") {
		t.Errorf("note = %q; a live patch is gone on restart and a model that thought it had "+
			"changed the scenario would be wrong about everything after it", note)
	}
	if answer["one_step_only"] == nil {
		t.Error("the answer does not say there is no scheduling here")
	}
}

func TestAPatchThatSetsNothingIsRefusedBeforeItIsSent(t *testing.T) {
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": storedSimulation()}}
	refuseSimulationTool(t, simulationDeps(sim), "set_simulation_context", map[string]any{
		"simulation_id": "env-1", "rationale": "testing",
	}, "sets nothing")
	if len(sim.patched) != 0 {
		t.Error("an empty patch reached MOSES")
	}
}

// ---- deleting ----

func TestDeleteSimulationReachesOnlyWhatThisSessionCreated(t *testing.T) {
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": storedSimulation()}}
	log := newFakeCreations()
	deps := simulationDeps(sim, func(d *Deps) { d.Creations = log })

	refuseSimulationTool(t, deps, "delete_simulation", map[string]any{
		"simulation_id": "env-1", "rationale": "cleaning up",
	}, "this session created no simulation")
	if len(sim.deleted) != 0 {
		t.Fatalf("a simulation this session did not create was deleted: %v", sim.deleted)
	}

	log = newFakeCreations(Creation{Kind: CreatedSimulation, ID: "env-1", Name: "Werk 2"})
	deps = simulationDeps(sim, func(d *Deps) { d.Creations = log })
	answer := callSimulationTool(t, deps, "delete_simulation", map[string]any{
		"simulation_id": "env-1", "rationale": "the scenario was wrong and a new one replaces it",
	})
	if len(sim.deleted) != 1 || sim.deleted[0] != "env-1" {
		t.Fatalf("deleted %v", sim.deleted)
	}
	devices := stringsOf(answer["devices_deleted"])
	if len(devices) != 2 {
		t.Errorf("devices_deleted = %v, want both platform devices — what they published is "+
			"gone with them, and the developer has to be told", devices)
	}
	if answer["undoable"] != false {
		t.Error("the answer does not say this cannot be undone")
	}
}

// A device the developer attached themselves outlives the simulation, and saying
// which is which is the difference between "your data is gone" and "your data is
// fine".
func TestDeleteSaysWhichDevicesSurviveIt(t *testing.T) {
	environment := storedSimulation()
	environment.Zones[0].Assets[0].ExternalManaged = false
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": environment}}
	log := newFakeCreations(Creation{Kind: CreatedSimulation, ID: "env-1", Name: "Werk 2"})

	answer := callSimulationTool(t, simulationDeps(sim, func(d *Deps) { d.Creations = log }),
		"delete_simulation", map[string]any{"simulation_id": "env-1", "rationale": "done with it"})

	kept := stringsOf(answer["devices_kept"])
	if len(kept) != 1 || kept[0] != "device-meter" {
		t.Errorf("devices_kept = %v, want the developer's own device", kept)
	}
	if len(stringsOf(answer["devices_deleted"])) != 1 {
		t.Errorf("devices_deleted = %v", answer["devices_deleted"])
	}
}

// ---- backfill ----

func TestABackfillNamesItsWindowAndWarnsThatItIsNotIdempotent(t *testing.T) {
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": storedSimulation()}}
	from := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	to := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	answer := callSimulationTool(t, simulationDeps(sim), "backfill_simulation", map[string]any{
		"simulation_id": "env-1",
		"from":          from.Format(time.RFC3339),
		"to":            to.Format(time.RFC3339),
		"rationale":     "the forecast needs four weeks of history and the scenario is a day old",
	})
	if len(sim.windows) != 1 || !sim.windows[0][0].Equal(from) {
		t.Fatalf("MOSES saw the window %v, want %v..%v", sim.windows, from, to)
	}
	warnings := strings.Join(stringsOf(answer["warnings"]), " ")
	if !strings.Contains(warnings, "not idempotent") {
		t.Errorf("warnings = %q; running the same window twice writes every row twice and "+
			"nothing downstream de-duplicates", warnings)
	}
	// The catalogue here is empty, so nothing was checked — and silence where a
	// check was expected reads as a pass, which is the one outcome to avoid.
	if !strings.Contains(warnings, "not established") {
		t.Errorf("warnings = %q; with nothing checked the answer must say so rather than "+
			"letting the absence of a warning read as a working backfill", warnings)
	}
}

func TestABackfillWithNoWindowIsRefusedRatherThanDefaulted(t *testing.T) {
	sim := &fakeSimulation{environments: map[string]simulation.Environment{"env-1": storedSimulation()}}
	refuseSimulationTool(t, simulationDeps(sim), "backfill_simulation", map[string]any{
		"simulation_id": "env-1", "rationale": "history",
	}, "both ends of the window")
	if len(sim.windows) != 0 {
		t.Error("a backfill with no window reached MOSES")
	}
}

// The failure this tool exists to make visible: a job that reports itself done
// having published nothing at all.
func TestAJobThatSkippedEveryChannelSaysSoRatherThanReportingSuccess(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		status: simulation.BackfillStatus{
			EnvironmentID: "env-1", State: simulation.BackfillDone,
			ChannelsTotal: 2, ChannelsDone: 2,
			Channels: []simulation.BackfillChannelStatus{
				{ChannelID: "c1", Name: "Hall power", Backfillable: false,
					SkipReason: "an aggregate is derived from the channels below it"},
				{ChannelID: "c2", Name: "Power", Backfillable: false,
					SkipReason: "the service does not declare senergy/time_path"},
			},
		},
	}
	answer := callSimulationTool(t, simulationDeps(sim), "get_backfill_status",
		map[string]any{"simulation_id": "env-1"})

	skipped, _ := answer["skipped_channels"].([]any)
	if len(skipped) != 2 {
		t.Fatalf("skipped_channels = %v", answer["skipped_channels"])
	}
	note, _ := answer["note"].(string)
	if !strings.Contains(note, "Every channel was skipped") {
		t.Errorf("note = %q; a done job with nothing published reads as success and is not one", note)
	}
}

func TestAnUnknownJobIsNotReportedAsAFailure(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		statusErr:    simulation.ErrNotFound,
	}
	answer := callSimulationTool(t, simulationDeps(sim), "get_backfill_status",
		map[string]any{"simulation_id": "env-1"})
	if answer["state"] != "unknown" {
		t.Errorf("state = %v, want unknown", answer["state"])
	}
	note, _ := answer["note"].(string)
	if !strings.Contains(note, "restarted") {
		t.Errorf("note = %q; jobs live in memory and a restart forgets them, which is a "+
			"different thing from a job that failed", note)
	}
}

// ---- example data ----

func TestUploadingADatasetReadsItFromTheDevelopersOwnWorkspace(t *testing.T) {
	csv := "time,power\n2026-07-01T00:00:00Z,120\n2026-07-01T01:00:00Z,340\n"
	kernelFake := &fakeKernel{files: map[string]kernel.FileContent{
		"data/hall-july.csv": {Path: "data/hall-july.csv", Text: csv, Size: int64(len(csv))},
	}}
	sim := &fakeSimulation{}
	log := newFakeCreations()
	deps := simulationDeps(sim, func(d *Deps) { d.Kernel = kernelFake; d.Creations = log })

	answer := callSimulationTool(t, deps, "upload_simulation_dataset", map[string]any{
		"workspace_path": "data/hall-july.csv",
		"name":           "A real hall, july",
		"timezone":       "Europe/Berlin",
		"rationale":      "open data from the utility's own portal, one minute values for a comparable hall",
	})

	if len(sim.uploads) != 1 {
		t.Fatalf("MOSES saw %d uploads", len(sim.uploads))
	}
	if sim.uploads[0].Content != csv {
		t.Errorf("MOSES received %q, want the file as it is in the workspace", sim.uploads[0].Content)
	}
	if sim.uploads[0].Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %q; a file of offsetless local timestamps read in the wrong zone "+
			"shifts every value", sim.uploads[0].Timezone)
	}
	if answer["dataset_id"] != "dataset-1" {
		t.Errorf("dataset_id = %v", answer["dataset_id"])
	}
	if next, _ := answer["next"].(string); !strings.Contains(next, "set_channel_source") {
		t.Errorf("next = %q; an uploaded dataset that nothing points at is not example data yet", next)
	}
	recorded, _ := log.Creations(context.Background(), testSimulationSession)
	if len(recorded) != 1 || recorded[0].Kind != CreatedSimulationDataset {
		t.Errorf("recorded %v, want the dataset", recorded)
	}
}

// A truncated CSV parses — the last line is simply gone — so MOSES would accept
// it and the replay would end early with nothing anywhere saying why.
func TestATruncatedFileIsRefusedRatherThanUploadedShort(t *testing.T) {
	kernelFake := &fakeKernel{files: map[string]kernel.FileContent{
		"data/big.csv": {Path: "data/big.csv", Text: "time,power\n2026-07-01T00:00:00Z,120\n", Truncated: true},
	}}
	sim := &fakeSimulation{}
	refuseSimulationTool(t, simulationDeps(sim, func(d *Deps) { d.Kernel = kernelFake }),
		"upload_simulation_dataset", map[string]any{
			"workspace_path": "data/big.csv", "name": "big", "rationale": "testing",
		}, "cut-off file")
	if len(sim.uploads) != 0 {
		t.Error("a truncated file was uploaded anyway")
	}
}

func TestABinaryFileIsRefusedWithWhatADatasetActuallyIs(t *testing.T) {
	kernelFake := &fakeKernel{files: map[string]kernel.FileContent{
		"data/hall.parquet": {Path: "data/hall.parquet", Binary: true},
	}}
	refuseSimulationTool(t, simulationDeps(&fakeSimulation{}, func(d *Deps) { d.Kernel = kernelFake }),
		"upload_simulation_dataset", map[string]any{
			"workspace_path": "data/hall.parquet", "name": "hall", "rationale": "testing",
		}, "a header line")
}

// ---- degradation ----

func TestTheSimulationToolsStayUnavailableWithoutAMosesUrl(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, name := range []string{
		"list_simulations", "get_simulation", "list_simulation_templates",
		"list_simulation_device_types", "get_simulation_state", "create_simulation",
		"add_simulated_asset", "set_channel_source", "set_simulation_context",
		"delete_simulation", "backfill_simulation", "get_backfill_status",
		"list_simulation_datasets", "upload_simulation_dataset",
	} {
		definition, found := registry.Lookup(name)
		if !found {
			t.Fatalf("%q left the documented surface", name)
		}
		if definition.Implemented() {
			t.Errorf("%q has an executor with no simulator behind it", name)
		}
		if !strings.Contains(definition.Unavailable, "moses_url") {
			t.Errorf("%q says it needs %q, which does not name the configuration a deployment "+
				"would have to add", name, definition.Unavailable)
		}
	}
}

// upload_simulation_dataset needs the pod as well as the simulator, because the
// file it uploads is one the developer's own workspace holds.
func TestUploadingADatasetIsUnavailableWithoutTheDevelopersPod(t *testing.T) {
	registry, err := NewSurface(Deps{Simulation: &fakeSimulation{}})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, _ := registry.Lookup("upload_simulation_dataset")
	if definition.Implemented() {
		t.Error("upload_simulation_dataset has an executor with no workspace to read from")
	}
	if !strings.Contains(definition.Unavailable, "jupyterhub_url") {
		t.Errorf("it says it needs %q", definition.Unavailable)
	}
}

// ---- the backfill precondition ----

// backfillableType is a device type whose power service declares where its
// message carries the event time, and whose energy service does not. Two
// services on one type, because that is the shape the choice actually has: the
// developer picks a service, not a device type.
func backfillableType() models.DeviceType {
	timeVariable := models.ContentVariable{Name: "time", Type: models.String}
	valueVariable := models.ContentVariable{Name: "value", Type: models.Float}
	root := models.ContentVariable{
		Name: "metrics", Type: models.Structure,
		SubContentVariables: []models.ContentVariable{valueVariable, timeVariable},
	}
	output := models.Content{
		Serialization: models.JSON, ContentVariable: root,
	}
	return models.DeviceType{
		Id: "type-inverter", Name: "Simulated inverter",
		Services: []models.Service{
			{
				Id: "service-power", Name: "Power",
				Attributes: []models.Attribute{{Key: simulation.TimePathAttribute, Value: "metrics.time"}},
				Outputs:    []models.Content{output},
			},
			{
				// No attribute at all, which is the ordinary state of a device type: it is
				// optional, and nobody sets it for a device that reports the present.
				Id: "service-energy", Name: "Energy",
				Outputs: []models.Content{output},
			},
		},
	}
}

func ontologyWith(types ...models.DeviceType) *fakeOntology {
	byID := map[string]models.DeviceType{}
	for _, deviceType := range types {
		byID[deviceType.Id] = deviceType
	}
	return &fakeOntology{deviceTypes: byID}
}

// The whole point of checking early: the model is told while it is choosing,
// which is the only moment the answer costs nothing.
func TestTheCatalogueSaysWhichServicesCanCarryAHistoricalTimestamp(t *testing.T) {
	sim := &fakeSimulation{deviceTypes: simulationCatalogue()}
	repo := ontologyWith(backfillableType())
	answer := callSimulationTool(t,
		simulationDeps(sim, func(d *Deps) { d.Ontology = repo }),
		"list_simulation_device_types", map[string]any{})

	services := answer["device_types"].([]any)[0].(map[string]any)["services"].([]any)
	byName := map[string]map[string]any{}
	for _, entry := range services {
		service := entry.(map[string]any)
		byName[service["name"].(string)] = service
	}

	power := byName["Power"]
	if power["backfillable"] != string(simulation.BackfillPossible) {
		t.Errorf("Power reads %v, want possible — its service declares a time path",
			power["backfillable"])
	}
	if power["time_path"] != "metrics.time" {
		t.Errorf("Power's time_path is %v", power["time_path"])
	}

	energy := byName["Energy"]
	if energy["backfillable"] != string(simulation.BackfillImpossible) {
		t.Fatalf("Energy reads %v, want impossible — it declares no time path",
			energy["backfillable"])
	}
	reason, _ := energy["backfill_reason"].(string)
	if !strings.Contains(reason, simulation.TimePathAttribute) {
		t.Errorf("the reason is %q and does not name the attribute that is missing", reason)
	}
	if !strings.Contains(reason, "identical timestamps") {
		t.Errorf("the reason is %q and does not say what would actually happen — a block of "+
			"identical timestamps is worse than no data, and that is the part that decides",
			reason)
	}

	if note, _ := answer["backfill_note"].(string); !strings.Contains(note, "prefer a service") {
		t.Errorf("backfill_note = %q; the catalogue is where the choice is made", note)
	}
	// One request for the whole catalogue, not one per type.
	if len(repo.deviceTypeCalls) != 1 {
		t.Errorf("the device repository was asked %d times, want once for the whole catalogue",
			len(repo.deviceTypeCalls))
	}
}

// A platform where nothing can be backfilled is a fact worth stating plainly:
// the answer is a modelling change in the device repository, not another attempt.
func TestACatalogueWithNoBackfillableServiceSaysSo(t *testing.T) {
	noTimePath := backfillableType()
	noTimePath.Services[0].Attributes = nil
	answer := callSimulationTool(t,
		simulationDeps(&fakeSimulation{deviceTypes: simulationCatalogue()},
			func(d *Deps) { d.Ontology = ontologyWith(noTimePath) }),
		"list_simulation_device_types", map[string]any{})

	summary, _ := answer["backfill_summary"].(string)
	if !strings.Contains(summary, "cannot be backfilled at all") {
		t.Errorf("backfill_summary = %q; a platform where no simulated channel can hold history "+
			"is the case a developer needs told before building a scenario", summary)
	}
	if !strings.Contains(summary, simulation.TimePathAttribute) {
		t.Errorf("summary = %q and does not say what would have to change", summary)
	}
}

// A device repository that did not answer says nothing about a device type. The
// difference between "no" and "not established" is what stops a warning being
// wrong in the direction that gets acted on.
func TestAFailedDeviceTypeReadIsUnknownRatherThanNo(t *testing.T) {
	repo := ontologyWith()
	repo.deviceTypeErr = errors.New("device-repository: 503")
	answer := callSimulationTool(t,
		simulationDeps(&fakeSimulation{deviceTypes: simulationCatalogue()},
			func(d *Deps) { d.Ontology = repo }),
		"list_simulation_device_types", map[string]any{})

	services := answer["device_types"].([]any)[0].(map[string]any)["services"].([]any)
	for _, entry := range services {
		service := entry.(map[string]any)
		if service["backfillable"] != string(simulation.BackfillUnknown) {
			t.Errorf("%v reads %v, want unknown", service["name"], service["backfillable"])
		}
	}
	if answer["backfill_summary"] != nil {
		t.Errorf("backfill_summary = %v; nothing was established, so there is nothing to sum up",
			answer["backfill_summary"])
	}
}

// The warning has to reach the developer at the moment the devices come into
// existence, because after that it costs a device in the repository and two
// confirmations to have found out.
func TestCreatingASimulationWarnsAboutChannelsThatCannotBeBackfilled(t *testing.T) {
	sim := &fakeSimulation{deviceTypes: simulationCatalogue()}
	answer := callSimulationTool(t,
		simulationDeps(sim, func(d *Deps) { d.Ontology = ontologyWith(backfillableType()) }),
		"create_simulation", map[string]any{
			"template":  "pv_site",
			"name":      "Roof array",
			"rationale": "no PV generation series on this platform",
			"bindings": map[string]any{
				"inverter": map[string]any{
					"device_type_id": "type-inverter",
					"channels":       map[string]any{"power": "service-power", "energy": "service-energy"},
				},
			},
		})

	warnings := strings.Join(stringsOf(answer["warnings"]), " ")
	if !strings.Contains(warnings, "cannot be backfilled") {
		t.Fatalf("warnings = %q; the energy channel is on a service with no time path", warnings)
	}
	if !strings.Contains(warnings, "Energy") {
		t.Errorf("warnings = %q and does not name the channel", warnings)
	}
	if strings.Contains(warnings, "Power:") {
		t.Errorf("warnings = %q; the power channel is fine and naming it would bury the one "+
			"that is not", warnings)
	}
	if !strings.Contains(warnings, "device repository") {
		t.Errorf("warnings = %q; the fix is a change to the device type by whoever owns it, "+
			"and a warning that does not say where to go is a dead end", warnings)
	}
}

// The check must not stop the create. A simulation nobody will backfill is a
// perfectly good scenario, and refusing to build it would be ODE deciding what
// the developer's case is.
func TestAnUnbackfillableBindingStillCreatesTheSimulation(t *testing.T) {
	noTimePath := backfillableType()
	noTimePath.Services[0].Attributes = nil
	sim := &fakeSimulation{deviceTypes: simulationCatalogue()}
	answer := callSimulationTool(t,
		simulationDeps(sim, func(d *Deps) { d.Ontology = ontologyWith(noTimePath) }),
		"create_simulation", map[string]any{
			"template":  "pv_site",
			"name":      "Live only",
			"rationale": "the operator is watched as it runs, so it needs no history",
			"bindings": map[string]any{
				"inverter": map[string]any{
					"device_type_id": "type-inverter",
					"channels":       map[string]any{"power": "service-power"},
				},
			},
		})
	if answer["simulation_id"] == nil {
		t.Fatal("the simulation was not created")
	}
	if len(sim.created) != 1 {
		t.Errorf("MOSES saw %d creates", len(sim.created))
	}
	if !strings.Contains(strings.Join(stringsOf(answer["warnings"]), " "), "cannot be backfilled") {
		t.Error("it was created without the warning, which is the half that had to survive")
	}
}

func TestAddingAnAssetWarnsAboutTheSameThing(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	noTimePath := backfillableType()
	noTimePath.Services[0].Attributes = nil
	answer := callSimulationTool(t,
		simulationDeps(sim, func(d *Deps) { d.Ontology = ontologyWith(noTimePath) }),
		"add_simulated_asset", map[string]any{
			"simulation_id": "env-1", "zone_id": "zone-hall", "name": "Machine 2",
			"kind": "machine", "device_type_id": "type-inverter", "rationale": "a second machine",
			"channels": []any{map[string]any{
				"service_id": "service-power",
				"source":     map[string]any{"kind": "profile", "profile": map[string]any{"base": 1}},
			}},
		})
	if !strings.Contains(strings.Join(stringsOf(answer["warnings"]), " "), "cannot be backfilled") {
		t.Errorf("warnings = %v; the new asset publishes through a service with no time path",
			answer["warnings"])
	}
}

// Before the job rather than out of its status: a simulation can be built by one
// session and backfilled by another, and a job that skips everything still runs,
// reports itself done and publishes nothing.
func TestABackfillWarnsBeforeItRunsRatherThanAfter(t *testing.T) {
	stored := storedSimulation()
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": stored},
		deviceTypes:  simulationCatalogue(),
	}
	noTimePath := backfillableType()
	noTimePath.Services[0].Attributes = nil

	answer := callSimulationTool(t,
		simulationDeps(sim, func(d *Deps) { d.Ontology = ontologyWith(noTimePath) }),
		"backfill_simulation", map[string]any{
			"simulation_id": "env-1",
			"from":          time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
			"to":            time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"rationale":     "the operator needs two days of history",
		})

	warnings := strings.Join(stringsOf(answer["warnings"]), " ")
	if !strings.Contains(warnings, "cannot be backfilled") {
		t.Errorf("warnings = %q; every channel of this simulation is on a service with no "+
			"time path, and finding that out from the status afterwards is the failure this "+
			"check exists to prevent", warnings)
	}
	// The job still starts: the developer confirmed a window, and the status it
	// leaves behind is itself the record.
	if answer["state"] != string(simulation.BackfillRunning) {
		t.Errorf("state = %v; the warning must not cancel a confirmed job", answer["state"])
	}
}

func TestABackfillOnGoodServicesSaysWhatWasAndWasNotChecked(t *testing.T) {
	sim := &fakeSimulation{
		environments: map[string]simulation.Environment{"env-1": storedSimulation()},
		deviceTypes:  simulationCatalogue(),
	}
	answer := callSimulationTool(t,
		simulationDeps(sim, func(d *Deps) { d.Ontology = ontologyWith(backfillableType()) }),
		"backfill_simulation", map[string]any{
			"simulation_id": "env-1",
			"from":          time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
			"to":            time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"rationale":     "history",
		})
	warnings := strings.Join(stringsOf(answer["warnings"]), " ")
	if !strings.Contains(warnings, "MOSES decides finally") {
		t.Errorf("warnings = %q; ODE's verdict is a necessary condition and not a sufficient "+
			"one, and an answer that omitted that would be a promise it cannot keep", warnings)
	}
}

// The verdict is a fact about the device type, so it is worth pinning against
// the shapes the platform's own ingestion cannot survive rather than only the
// happy path.
func TestTheTimePathVerdictMirrorsWhatTheIngestionCanRead(t *testing.T) {
	usable := backfillableType().Services[0]

	for _, testCase := range []struct {
		name    string
		mutate  func(service *models.Service)
		want    simulation.Backfillable
		wantSay string
	}{
		{
			name:   "a declared and resolvable path",
			mutate: func(*models.Service) {},
			want:   simulation.BackfillPossible,
		},
		{
			name:    "no attribute at all",
			mutate:  func(s *models.Service) { s.Attributes = nil },
			want:    simulation.BackfillImpossible,
			wantSay: "moment it arrived",
		},
		{
			name: "a path naming only the root",
			mutate: func(s *models.Service) {
				s.Attributes = []models.Attribute{{Key: simulation.TimePathAttribute, Value: "metrics"}}
			},
			want:    simulation.BackfillImpossible,
			wantSay: "names a whole output",
		},
		{
			name: "a path that starts somewhere else",
			mutate: func(s *models.Service) {
				s.Attributes = []models.Attribute{{Key: simulation.TimePathAttribute, Value: "other.time"}}
			},
			want:    simulation.BackfillImpossible,
			wantSay: "root variable is",
		},
		{
			name: "a member the output does not have",
			mutate: func(s *models.Service) {
				s.Attributes = []models.Attribute{{Key: simulation.TimePathAttribute, Value: "metrics.stamp"}}
			},
			want:    simulation.BackfillImpossible,
			wantSay: "has no member",
		},
		{
			name: "two outputs",
			mutate: func(s *models.Service) {
				s.Outputs = append(s.Outputs, s.Outputs[0])
			},
			want:    simulation.BackfillImpossible,
			wantSay: "single protocol segment",
		},
		{
			name: "an output that is not json",
			mutate: func(s *models.Service) {
				s.Outputs[0].Serialization = models.XML
			},
			want:    simulation.BackfillImpossible,
			wantSay: "publishes json",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := usable
			service.Attributes = append([]models.Attribute{}, usable.Attributes...)
			service.Outputs = append([]models.Content{}, usable.Outputs...)
			testCase.mutate(&service)

			verdict := simulation.CheckTimePath(service)
			if verdict.Backfillable != testCase.want {
				t.Fatalf("verdict = %q (%s), want %q",
					verdict.Backfillable, verdict.Reason, testCase.want)
			}
			if testCase.wantSay != "" && !strings.Contains(verdict.Reason, testCase.wantSay) {
				t.Errorf("the reason is %q and does not say %q", verdict.Reason, testCase.wantSay)
			}
		})
	}
}
