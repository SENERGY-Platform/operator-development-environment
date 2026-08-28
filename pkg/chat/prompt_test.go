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

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// promptSimulation is a MOSES that answers nothing. What is under test is
// whether the prompt mentions the simulator at all, which turns on the
// dependency being present rather than on anything it says.
type promptSimulation struct{}

func (promptSimulation) List(context.Context, string) ([]simulation.Environment, error) {
	return nil, nil
}
func (promptSimulation) Get(context.Context, string, string) (simulation.Environment, error) {
	return simulation.Environment{}, nil
}
func (promptSimulation) Create(context.Context, string, simulation.Environment) (simulation.Environment, error) {
	return simulation.Environment{}, nil
}
func (promptSimulation) Replace(context.Context, string, simulation.Environment) (simulation.Environment, error) {
	return simulation.Environment{}, nil
}
func (promptSimulation) Delete(context.Context, string, string) error { return nil }
func (promptSimulation) State(context.Context, string, string) (simulation.EnvironmentState, error) {
	return simulation.EnvironmentState{}, nil
}
func (promptSimulation) Patch(context.Context, string, string, simulation.StateChange) error {
	return nil
}
func (promptSimulation) Backfill(context.Context, string, string, time.Time, time.Time) (simulation.BackfillStatus, error) {
	return simulation.BackfillStatus{}, nil
}
func (promptSimulation) BackfillStatusOf(context.Context, string, string) (simulation.BackfillStatus, error) {
	return simulation.BackfillStatus{}, nil
}
func (promptSimulation) DeviceTypes(context.Context, string) ([]simulation.DeviceType, error) {
	return nil, nil
}
func (promptSimulation) Datasets(context.Context, string) ([]simulation.Dataset, error) {
	return nil, nil
}
func (promptSimulation) UploadDataset(context.Context, string, string, string, []byte) (simulation.Dataset, error) {
	return simulation.Dataset{}, nil
}
func (promptSimulation) MaxDatasetBytes() int { return 1 << 20 }

func promptRegistry(t *testing.T, deps tools.Deps) *tools.Registry {
	t.Helper()
	registry, err := tools.NewSurface(deps)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return registry
}

// The decision this paragraph is about — "the platform has no data for this
// case, so build the scenario" — happens before any simulation tool is a
// candidate. A model that only meets MOSES in a tool list meets it while
// browsing tools, which is the wrong moment.
func TestThePromptTellsTheAssistantAboutTheSimulatorWhenThereIsOne(t *testing.T) {
	prompt := systemPrompt(
		promptRegistry(t, tools.Deps{Simulation: promptSimulation{}}), Session{Tier: tools.L0}, true)

	for _, expected := range []string{
		"MOSES",
		"backfill_simulation",
		"Look for real data first",
		"upload_simulation_dataset",
		"where the data came from",
		"does not simulate",
		// The backfill precondition. It is decided by the device type, before the
		// devices exist, and it is what usually makes a simulation worth building at
		// all — a model that meets it only in a tool description meets it too late.
		"backfillable",
		"list_simulation_device_types",
	} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("the prompt does not mention %q, so the assistant would meet the "+
				"simulator only as a tool name", expected)
		}
	}
}

// A deployment without a simulator must not be told about one: the paragraph
// would be an invitation to propose something that cannot happen.
func TestThePromptSaysNothingAboutTheSimulatorWithoutOne(t *testing.T) {
	prompt := systemPrompt(promptRegistry(t, tools.Deps{}), Session{Tier: tools.L0}, true)
	if strings.Contains(prompt, "MOSES") {
		t.Error("a deployment with no moses_url is told about a simulator it does not have")
	}
}

// The paragraph has to hold the order, not only the names: a real series beats a
// simulated one, and looking for example data comes before asserting a shape.
func TestThePromptPutsRealDataAheadOfSimulatedData(t *testing.T) {
	prompt := systemPrompt(
		promptRegistry(t, tools.Deps{Simulation: promptSimulation{}}), Session{Tier: tools.L0}, true)
	real := strings.Index(prompt, "Look for real data first")
	example := strings.Index(prompt, "go and find example data")
	asserted := strings.Index(prompt, "become the right source")
	if real < 0 || example < 0 || asserted < 0 {
		t.Fatalf("the ordering paragraph is not in the prompt: %d %d %d", real, example, asserted)
	}
	if !(real < example && example < asserted) {
		t.Error("the prompt does not put real data before example data before an asserted shape")
	}
}

// Nothing in the simulation surface may be advertised at a tier that does not
// reach it, and get_simulation_state is the one that sits above L0.
func TestTheLiveStateToolIsNotAdvertisedAtL0(t *testing.T) {
	registry := promptRegistry(t, tools.Deps{Simulation: promptSimulation{}})
	for _, definition := range registry.Available(tools.L0) {
		if definition.Name == "get_simulation_state" {
			t.Error("get_simulation_state is advertised at L0, where it would be refused")
		}
	}
	beyond := map[string]bool{}
	for _, definition := range registry.Beyond(tools.L0) {
		beyond[definition.Name] = true
	}
	if !beyond["get_simulation_state"] {
		t.Error("get_simulation_state is not reported as reachable at a higher tier, so the " +
			"assistant cannot ask for the raise")
	}
}

// Every simulation tool's schema has to be something a provider will accept, and
// the source shape is the one that is inlined into two of them by hand.
func TestEverySimulationToolCarriesAUsableSchema(t *testing.T) {
	registry := promptRegistry(t, tools.Deps{Simulation: promptSimulation{}})
	for _, definition := range registry.Definitions() {
		if !strings.Contains(definition.Name, "simulation") &&
			!strings.Contains(definition.Name, "backfill") &&
			definition.Name != "set_channel_source" &&
			definition.Name != "add_simulated_asset" {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Errorf("%s has an unparseable schema: %v", definition.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s has a schema of type %v, want object", definition.Name, schema["type"])
		}
		// A local $ref is legal JSON Schema and is not reliably resolved by every
		// provider's tool-input validator, so the source shape is inlined. A $ref
		// creeping back in is a tool the model cannot call for a reason nothing reports.
		if strings.Contains(string(definition.Schema), `"$ref"`) {
			t.Errorf("%s carries a $ref; the shared source shape is inlined on purpose",
				definition.Name)
		}
	}
}
