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

package imports

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNodeInputIsWhatTheFlowEngineExpects(t *testing.T) {
	input, err := NodeInput(testInstanceID, testTopic, []Binding{
		{InputName: "temperature", Path: "value.temperature_2m"},
	})
	if err != nil {
		t.Fatalf("NodeInput: %v", err)
	}
	if input.FilterType != "ImportId" {
		t.Errorf("filterType = %q, want ImportId exactly: the flow engine compares the string "+
			"and falls back to DeviceId, so a near miss wires an operator that never fires",
			input.FilterType)
	}
	if input.FilterIds != testInstanceID {
		t.Errorf("filterIds = %q, want the instance id", input.FilterIds)
	}
	if input.TopicName != testTopic {
		t.Errorf("topicName = %q, want the instance's kafka topic", input.TopicName)
	}
	if len(input.Values) != 1 || input.Values[0].Path != "value.temperature_2m" {
		t.Fatalf("values = %+v, want the message-relative path", input.Values)
	}

	// The wire form is what a developer pastes into the flow engine, so the JSON
	// keys matter as much as the values.
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"filterType"`, `"filterIds"`, `"topicName"`, `"values"`, `"name"`, `"path"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("encoded input has no %s: %s", key, encoded)
		}
	}
}

func TestNodeInputSortsValuesForAStableDocument(t *testing.T) {
	input, err := NodeInput(testInstanceID, testTopic, []Binding{
		{InputName: "pressure", Path: "value.pressure_msl"},
		{InputName: "cloud", Path: "value.cloudcover"},
	})
	if err != nil {
		t.Fatalf("NodeInput: %v", err)
	}
	if input.Values[0].Name != "cloud" || input.Values[1].Name != "pressure" {
		t.Errorf("values = %+v, want them sorted by input name so two proposals diff cleanly",
			input.Values)
	}
}

func TestNodeInputRefusesWhatWouldFailSilently(t *testing.T) {
	cases := []struct {
		name     string
		instance string
		topic    string
		bindings []Binding
	}{
		{"no instance", "", testTopic, []Binding{{InputName: "t", Path: "value.t"}}},
		{"no topic", testInstanceID, "", []Binding{{InputName: "t", Path: "value.t"}}},
		{"no bindings", testInstanceID, testTopic, nil},
		{"no input name", testInstanceID, testTopic, []Binding{{Path: "value.t"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NodeInput(testCase.instance, testCase.topic, testCase.bindings); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// Upstream keeps both mappings and the last one wins, which reads as the operator
// ignoring a variable the developer picked.
func TestNodeInputRefusesADoubleBoundInput(t *testing.T) {
	_, err := NodeInput(testInstanceID, testTopic, []Binding{
		{InputName: "temperature", Path: "value.temperature_2m"},
		{InputName: "temperature", Path: "value.apparent_temperature"},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want a refusal for one input bound twice", err)
	}
}

func TestMessagePathKeepsAnAlreadyRelativePath(t *testing.T) {
	got, err := MessagePath("value.temperature_2m")
	if err != nil {
		t.Fatalf("MessagePath: %v", err)
	}
	if got != "value.temperature_2m" {
		t.Errorf("got %q, want it unchanged: a blind trim would leave temperature_2m, which is "+
			"one level too shallow and yields nothing", got)
	}
}

func TestMessagePathTrimsTheOutputRoot(t *testing.T) {
	// The root name is not guaranteed to be "root", which is why the payload node
	// is what the check keys on.
	for _, path := range []string{"root.value.temperature_2m", "envelope.value.temperature_2m"} {
		got, err := MessagePath(path)
		if err != nil {
			t.Fatalf("MessagePath(%q): %v", path, err)
		}
		if got != "value.temperature_2m" {
			t.Errorf("MessagePath(%q) = %q, want value.temperature_2m", path, got)
		}
	}
}

func TestMessagePathKeepsNestedStructures(t *testing.T) {
	got, err := MessagePath("root.value.units.temperature_2m")
	if err != nil {
		t.Fatalf("MessagePath: %v", err)
	}
	if got != "value.units.temperature_2m" {
		t.Errorf("got %q, want the whole subtree below the payload preserved", got)
	}
}

// import_id and time are content variables of an import type, and neither is a
// series. Accepting them would produce an operator input bound to the envelope.
func TestMessagePathRefusesAnEnvelopeField(t *testing.T) {
	for _, path := range []string{"root.import_id", "root.time", "value", "", "temperature_2m"} {
		if _, err := MessagePath(path); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("MessagePath(%q) err = %v, want a refusal", path, err)
		}
	}
}
