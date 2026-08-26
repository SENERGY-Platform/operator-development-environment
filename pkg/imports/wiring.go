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
	"fmt"
	"sort"
	"strings"

	flowengine "github.com/SENERGY-Platform/analytics-flow-engine/lib"
)

// FilterTypeImport is the string the flow engine matches on.
//
// It is compared exactly, and anything else falls through to the default of
// "DeviceId" rather than being rejected — so a typo here does not fail, it wires
// the operator to a device filter that will never match an import message and
// produces an operator that simply never fires.
const FilterTypeImport = "ImportId"

// pathPrefix is the envelope every import message carries.
//
// An import type's output describes the whole message — import_id, time, value —
// so the payload sits one level down. Both operator libraries then strip the
// first segment again before matching: the Java lib in Message.parseMessageForInputs,
// and the Python lib by using the mapping source unprefixed against the raw
// message. A path with the prefix missing therefore addresses one level too
// shallow and silently yields nothing.
const pathPrefix = "value"

// Binding is one operator input bound to one variable of one import instance.
type Binding struct {
	// InputName is the operator's own name for the input — the `dest` of the
	// mapping, and what the operator code reads.
	InputName string
	// Path is the message-relative path, as a Selectable carries it.
	Path string
}

// NodeInput builds the pipeline node input that wires an import instance into an
// operator.
//
// The type comes from the flow engine rather than being restated, so a rename
// upstream breaks this build instead of a deployment at runtime. ODE emits the
// value and never decodes one: the developer takes it to the flow engine.
//
// One input is one instance, deliberately. FilterIds is comma-splittable and the
// device path uses that for device groups, but every import instance has its own
// Kafka topic — it is the instance id with the colons replaced by underscores —
// and a node input carries a single topic. Two instances are two inputs.
func NodeInput(instanceID, kafkaTopic string, bindings []Binding) (flowengine.NodeInput, error) {
	if strings.TrimSpace(instanceID) == "" {
		return flowengine.NodeInput{}, fmt.Errorf("%w: an instance id is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(kafkaTopic) == "" {
		return flowengine.NodeInput{}, fmt.Errorf(
			"%w: instance %s carries no kafka topic, so there is nothing to consume from; "+
				"read the instance from import-deploy rather than constructing the topic",
			ErrInvalidRequest, instanceID)
	}
	if len(bindings) == 0 {
		return flowengine.NodeInput{}, fmt.Errorf(
			"%w: an input with no values would subscribe to the topic and read nothing from it",
			ErrInvalidRequest)
	}

	values := make([]flowengine.NodeValue, 0, len(bindings))
	seen := map[string]string{}
	for _, binding := range bindings {
		name := strings.TrimSpace(binding.InputName)
		if name == "" {
			return flowengine.NodeInput{}, fmt.Errorf(
				"%w: the binding for %q has no input name, and the mapping is keyed on it",
				ErrInvalidRequest, binding.Path)
		}
		path, err := MessagePath(binding.Path)
		if err != nil {
			return flowengine.NodeInput{}, err
		}
		if previous, duplicate := seen[name]; duplicate {
			// Upstream keeps both and the last one wins, which reads as the operator
			// ignoring a variable the developer selected.
			return flowengine.NodeInput{}, fmt.Errorf(
				"%w: input %q is bound twice, to %s and to %s; one input takes one path",
				ErrInvalidRequest, name, previous, path)
		}
		seen[name] = path
		values = append(values, flowengine.NodeValue{Name: name, Path: path})
	}

	// Sorted by input name so the same selection always produces the same
	// document. A caller diffing two proposals should see what changed, not a
	// reordering.
	sort.SliceStable(values, func(i, j int) bool { return values[i].Name < values[j].Name })

	return flowengine.NodeInput{
		FilterType: FilterTypeImport,
		FilterIds:  instanceID,
		TopicName:  kafkaTopic,
		Values:     values,
	}, nil
}

// MessagePath normalises a path to the form a mapping source takes.
//
// A Selectable already carries the message-relative path, because the selectables
// query is asked with import_path_trim_first_element. This exists for the paths
// that do not come from there — an id a model repeated back, a path read from an
// import type directly — where the root element is still on the front and the
// resulting mapping would address one level too deep.
//
// It cannot be a blind trim. `value.temperature` is already correct and trimming
// it would leave `temperature`, which is one level too shallow; the two cases are
// told apart by the prefix, which is the same test the platform's own export
// dialog applies to find the payload node.
func MessagePath(path string) (string, error) {
	path = strings.Trim(strings.TrimSpace(path), ".")
	if path == "" {
		return "", fmt.Errorf("%w: an empty path addresses the whole message envelope", ErrInvalidRequest)
	}

	segments := strings.Split(path, ".")
	if segments[0] == pathPrefix {
		if len(segments) == 1 {
			return "", fmt.Errorf(
				"%w: %q is the payload envelope itself, not a variable in it", ErrInvalidRequest, path)
		}
		return path, nil
	}

	// Not prefixed, so the first element is the import type's output root — whose
	// name is not guaranteed to be "root", which is why this matches on the
	// payload node instead.
	if len(segments) < 2 {
		return "", fmt.Errorf(
			"%w: %q names one element and no payload; a variable path reads %s.<field>",
			ErrInvalidRequest, path, pathPrefix)
	}
	if segments[1] != pathPrefix {
		return "", fmt.Errorf(
			"%w: %q has no %q element, so it addresses the message envelope rather than the "+
				"payload — import_id and time are content variables but not series",
			ErrInvalidRequest, path, pathPrefix)
	}
	return strings.Join(segments[1:], "."), nil
}
