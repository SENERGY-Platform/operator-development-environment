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
	"slices"
	"strings"
	"testing"
)

type inputSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

func noop(context.Context, Request) (any, error) { return nil, nil }

func parseSchema(t *testing.T, definition Definition) inputSchema {
	t.Helper()
	var schema inputSchema
	if err := json.Unmarshal(definition.Schema, &schema); err != nil {
		t.Fatalf("%s has an unparseable schema: %v", definition.Name, err)
	}
	return schema
}

// D39: every call the model makes carries its reason, so the developer reads why
// beside what. Checked over the whole declared surface, implemented or not, because
// a tool that gains its executor later is advertised with the schema it has now.
func TestEveryToolAsksForARationale(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	for _, definition := range registry.Definitions() {
		schema := parseSchema(t, definition)
		if _, found := schema.Properties["rationale"]; !found {
			t.Errorf("%s does not ask for a rationale", definition.Name)
		}
		if !slices.Contains(schema.Required, "rationale") {
			t.Errorf("%s does not require its rationale: required = %v", definition.Name, schema.Required)
		}
		if count := strings.Count(strings.Join(schema.Required, ","), "rationale"); count > 1 {
			t.Errorf("%s requires its rationale %d times", definition.Name, count)
		}
	}
}

// A tool that already asks for a rationale in its own terms keeps them: the
// confirmed tools describe what the developer is being asked to agree to, and
// the generic sentence would replace that with something vaguer.
func TestAToolsOwnRationaleIsKept(t *testing.T) {
	registry, err := NewSurface(Deps{})
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	definition, found := registry.Lookup("propose_data_selection")
	if !found {
		t.Fatal("propose_data_selection is not declared")
	}
	schema := parseSchema(t, definition)
	if !strings.Contains(string(schema.Properties["rationale"]), "Why this set, as a whole.") {
		t.Errorf("rationale = %s, want the tool's own description", schema.Properties["rationale"])
	}
}

// The rationale is required in the schema and nowhere else: a call without one —
// from a provider that ignores `required`, or an older MCP client — still runs.
// Refusing a read for a missing sentence would cost a round trip and tell the
// developer nothing they could act on.
func TestACallWithoutARationaleStillRuns(t *testing.T) {
	tracker := &ran{}
	_, dispatcher, _ := testSurface(t, tracker)

	result := dispatcher.Dispatch(context.Background(), request(L0),
		Call{ID: "call-1", Name: "l0_tool", Input: json.RawMessage(`{"device_id":"d1"}`)})

	if result.Outcome != OutcomeOK {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeOK)
	}
	if !tracker.was("l0_tool") {
		t.Error("a call without a rationale never reached its executor")
	}
}

func TestARationaleIsAddedToASchemaWithoutProperties(t *testing.T) {
	registry, err := NewRegistry(NewDefinition(Definition{
		Name:    "bare",
		MinTier: L0,
		Schema:  json.RawMessage(`{"type":"object"}`),
	}, noop))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	definition, _ := registry.Lookup("bare")
	schema := parseSchema(t, definition)
	if schema.Type != "object" {
		t.Errorf("type = %q, want the schema's own", schema.Type)
	}
	if _, found := schema.Properties["rationale"]; !found || !slices.Equal(schema.Required, []string{"rationale"}) {
		t.Errorf("schema = %s, want a required rationale", definition.Schema)
	}
}

func TestASchemaThatIsNotAnObjectIsRefused(t *testing.T) {
	_, err := NewRegistry(NewDefinition(Definition{
		Name:    "broken",
		MinTier: L0,
		Schema:  json.RawMessage(`null`),
	}, noop))
	if err == nil {
		t.Fatal("a null schema was accepted")
	}
}
