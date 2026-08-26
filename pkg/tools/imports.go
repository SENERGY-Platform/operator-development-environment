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
	"fmt"
	"sort"
	"strings"

	flowengine "github.com/SENERGY-Platform/analytics-flow-engine/lib"
	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
)

// The three import tools that are not discovery.
//
// Discovery is deliberately absent from this file: an import is found through
// resolve_semantic_selection, alongside devices, because both are described by
// the same content variables and a developer asking for a signal should not have
// to know which kind the platform delivers it from. What is here is what that
// resolution cannot do — look one up by id, and turn a chosen variable into
// something deployable.

// ---- list_import_instances (L0) ----

type listImportsInput struct {
	Search         string   `json:"search"`
	ImportTypeIDs  []string `json:"import_type_ids"`
	Limit          int64    `json:"limit"`
	IncludeHistory bool     `json:"include_history"`
}

func (s *surface) listImportInstances(ctx context.Context, req Request) (any, error) {
	var in listImportsInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 || limit > s.deps.DeviceLimit {
		limit = s.deps.DeviceLimit
	}

	// A type filter has to be applied here: import-deploy offers no filter by
	// import type and its search matches the instance name only. So the listing has
	// to be wide enough for the filter to have something to remove, and the note
	// below says the limit applied after it rather than before.
	listLimit := limit
	if len(in.ImportTypeIDs) > 0 {
		listLimit = imports.MaxLimit
	}

	result, err := s.deps.Imports.List(ctx, req.Token, imports.InstanceListOptions{
		Search: in.Search,
		Limit:  listLimit,
	})
	if err != nil {
		return nil, err
	}

	wanted := map[string]bool{}
	for _, id := range in.ImportTypeIDs {
		wanted[id] = true
	}

	kept := make([]idmodel.Instance, 0, len(result.Instances))
	cut := false
	for _, instance := range result.Instances {
		if len(wanted) > 0 && !wanted[instance.ImportTypeId] {
			continue
		}
		if int64(len(kept)) >= limit {
			// Recorded rather than inferred from the total. With a type filter the total
			// counts every visible instance, so comparing it against the page would
			// report a truncation that never happened.
			cut = true
			break
		}
		kept = append(kept, instance)
	}

	histories := s.importHistories(ctx, req.Token, kept, in.IncludeHistory)
	listed := make([]map[string]any, 0, len(kept))
	for _, instance := range kept {
		listed = append(listed, importInstanceView(instance, histories[instance.Id]))
	}

	answer := map[string]any{
		"instances": listed,
		"total":     result.Total,
		"limit":     limit,
		"note": "metadata only, no values. An import publishes to one Kafka topic; " +
			"kafka_topic and a variable path are what wire it into an operator " +
			"(see propose_operator_input).",
		"truncated": cut || (len(wanted) == 0 && result.Total > int64(len(listed))),
	}
	if len(wanted) > 0 {
		answer["note"] = answer["note"].(string) +
			" import_type_ids was filtered by ODE rather than upstream, so the limit applied " +
			"after the filter and `total` counts every visible instance, not the matching ones."
	}
	if !in.IncludeHistory {
		answer["note"] = answer["note"].(string) +
			" Whether an instance has stored history was not checked; pass include_history to ask."
	}
	return answer, nil
}

// importInstanceView is one instance as a model should read it.
//
// Running is reported as a string rather than a boolean because it has three
// states and a boolean cannot carry the third. Discovery never sees a status at
// all, and "stopped" would be a claim ODE has not established — it sends a model
// to tell the developer to restart something that may be running fine.
func importInstanceView(instance idmodel.Instance, history *imports.History) map[string]any {
	running, known := imports.Running(instance)
	state := "unknown"
	if known {
		state = "stopped"
		if running {
			state = "running"
		}
	}

	view := map[string]any{
		"instance_id":    instance.Id,
		"name":           instance.Name,
		"import_type_id": instance.ImportTypeId,
		"kafka_topic":    instance.KafkaTopic,
		"running":        state,
		"generated":      instance.Generated,
	}
	if note := imports.TransitionMessage(instance); note != "" {
		view["status_note"] = note
	}
	if history != nil {
		view["history"] = history
	}
	return view
}

// importHistories answers the history question only when it was asked, and then
// once for the whole page rather than once per row: analytics-serving cannot
// filter by import, so a per-instance lookup re-reads the same export listing
// every time.
func (s *surface) importHistories(ctx context.Context, token string, instances []idmodel.Instance, wanted bool) map[string]*imports.History {
	out := map[string]*imports.History{}
	if !wanted || len(instances) == 0 {
		return out
	}
	ids := make([]string, 0, len(instances))
	for _, instance := range instances {
		ids = append(ids, instance.Id)
	}
	for id, history := range s.deps.Imports.Histories(ctx, token, ids) {
		out[id] = &history
	}
	return out
}

// ---- get_import_type_metadata (L0) ----

type getImportTypeInput struct {
	ImportTypeID string `json:"import_type_id"`
	InstanceID   string `json:"instance_id"`
}

func (s *surface) getImportTypeMetadata(ctx context.Context, req Request) (any, error) {
	var in getImportTypeInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}

	typeID := strings.TrimSpace(in.ImportTypeID)
	var instance *idmodel.Instance
	if typeID == "" {
		if strings.TrimSpace(in.InstanceID) == "" {
			return nil, fmt.Errorf("%w: give an import_type_id or an instance_id", ErrInvalidInput)
		}
		found, err := s.deps.Imports.Get(ctx, req.Token, in.InstanceID)
		if err != nil {
			return nil, err
		}
		instance = &found
		typeID = found.ImportTypeId
	}

	importType, err := s.deps.Imports.GetType(ctx, req.Token, typeID)
	if err != nil {
		return nil, err
	}

	answer := map[string]any{
		"import_type_id": importType.Id,
		"name":           importType.Name,
		"description":    importType.Description,
		"configs":        importTypeConfigs(importType),
		"variables":      importTypeVariables(importType),
		"note": "An import type's output describes the whole Kafka message, so its " +
			"import_id and time variables are not series. Every variable_path below is " +
			"already in the addressable form an operator mapping takes.",
	}
	if instance != nil {
		answer["instance"] = importInstanceView(*instance, nil)
	}
	return answer, nil
}

func importTypeConfigs(importType dsmodel.ImportType) []map[string]any {
	out := make([]map[string]any, 0, len(importType.Configs))
	for _, config := range importType.Configs {
		out = append(out, map[string]any{
			"name":          config.Name,
			"description":   config.Description,
			"type":          config.Type,
			"default_value": config.DefaultValue,
		})
	}
	return out
}

// importTypeVariables walks the output tree into the flat, addressable list a
// model needs, and says of each row whether it is a signal.
//
// The envelope fields are reported rather than dropped, with a reason, for the
// same reason an unqueryable device path is: a developer or a model hunting for
// `time` needs to learn that it was seen and why it is not on offer, instead of
// concluding the import type does not declare it.
func importTypeVariables(importType dsmodel.ImportType) []map[string]any {
	out := []map[string]any{}
	walkImportVariable(&out, importType.Output, nil)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["path"].(string) < out[j]["path"].(string)
	})
	return out
}

func walkImportVariable(out *[]map[string]any, variable dsmodel.ImportContentVariable, prefix []string) {
	path := append(append([]string{}, prefix...), variable.Name)
	joined := strings.Join(path, ".")

	// A structure is a container rather than a value; only its leaves are
	// addressable. The payload node itself is the clearest example.
	if len(variable.SubContentVariables) == 0 {
		row := map[string]any{
			"path": joined,
			"type": variable.Type,
		}
		if variable.CharacteristicId != "" {
			row["characteristic_id"] = variable.CharacteristicId
		}
		if variable.FunctionId != "" {
			row["function_id"] = variable.FunctionId
		}
		if variable.AspectId != "" {
			row["aspect_id"] = variable.AspectId
		}
		if variable.UseAsTag {
			row["use_as_tag"] = true
		}

		if addressable, err := imports.MessagePath(joined); err == nil {
			row["variable_path"] = addressable
			row["is_series"] = true
		} else {
			row["is_series"] = false
			row["reason"] = "part of the message envelope rather than the payload, so it is not a " +
				"series: an import message carries its own id and timestamp beside the values"
		}
		*out = append(*out, row)
	}

	for _, sub := range variable.SubContentVariables {
		walkImportVariable(out, sub, path)
	}
}

// ---- propose_operator_input (confirmed developer action) ----

type proposeOperatorInputInput struct {
	InstanceID string `json:"instance_id"`
	Rationale  string `json:"rationale"`
	Bindings   []struct {
		InputName    string `json:"input_name"`
		VariablePath string `json:"variable_path"`
		Reason       string `json:"reason"`
	} `json:"bindings"`
}

// proposeOperatorInput turns a chosen import variable into the pipeline input the
// flow engine takes.
//
// It reads the instance rather than trusting the model for the topic. The topic
// is derivable — it is the instance id with the colons replaced — but deriving it
// would make ODE assert an upstream implementation detail, and reading it also
// answers the question the developer will ask next: is this import actually
// running. An input wired to a stopped import is syntactically perfect and
// produces nothing.
func (s *surface) proposeOperatorInput(ctx context.Context, req Request) (any, error) {
	var in proposeOperatorInputInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.InstanceID) == "" {
		return nil, fmt.Errorf("%w: instance_id is required", ErrInvalidInput)
	}
	if len(in.Bindings) == 0 {
		return nil, fmt.Errorf(
			"%w: at least one binding is required, or the operator would subscribe to the topic "+
				"and read nothing from it", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}

	instance, err := s.deps.Imports.Get(ctx, req.Token, in.InstanceID)
	if err != nil {
		return nil, err
	}

	bindings := make([]imports.Binding, 0, len(in.Bindings))
	reasons := make([]map[string]any, 0, len(in.Bindings))
	for _, binding := range in.Bindings {
		bindings = append(bindings, imports.Binding{
			InputName: binding.InputName,
			Path:      binding.VariablePath,
		})
		reasons = append(reasons, map[string]any{
			"input_name":    binding.InputName,
			"variable_path": binding.VariablePath,
			"reason":        binding.Reason,
		})
	}

	input, err := imports.NodeInput(instance.Id, instance.KafkaTopic, bindings)
	if err != nil {
		// A path that does not address the payload is the failure this tool exists to
		// prevent, and the error from imports says which one and why.
		return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}

	history := s.deps.Imports.History(ctx, req.Token, instance.Id)
	warnings := importWarnings(instance, history, input.Values)

	return map[string]any{
		"instance":   importInstanceView(instance, &history),
		"rationale":  in.Rationale,
		"bindings":   reasons,
		"node_input": input,
		"warnings":   warnings,
		"note": "This is the `inputs` entry of one node in an analytics flow-engine pipeline " +
			"request. It is not deployed: the developer takes it to the flow engine. " +
			"filterType is the literal string ImportId — the engine compares it exactly and " +
			"falls back to a device filter, which would never match an import message.",
	}, nil
}

// importWarnings says what would make this input deploy cleanly and still produce
// nothing useful. Each of these is silent at deployment time.
func importWarnings(instance idmodel.Instance, history imports.History, values []flowengine.NodeValue) []string {
	warnings := []string{}

	if running, known := imports.Running(instance); !known {
		warnings = append(warnings,
			"whether this import is running could not be established; an input wired to a "+
				"stopped import deploys cleanly and receives nothing")
	} else if !running {
		warnings = append(warnings,
			"this import is not running: the input is correct but no message will arrive until "+
				"the instance is started")
	}

	switch history.State {
	case imports.HistoryLiveOnly:
		warnings = append(warnings,
			"no export exists for this import, so none of its past is in timescale: the operator "+
				"can consume live values, and the Python operator library's provide_historic_data "+
				"replays the Kafka topic, but there is nothing to profile or backtest against first")
	case imports.HistoryUnknown:
		warnings = append(warnings,
			"whether this import has stored history is unknown: "+history.Reason)
	case imports.HistoryExported:
		missing := []string{}
		for _, value := range values {
			if _, found := history.ExportColumn(value.Path); !found {
				missing = append(missing, value.Path)
			}
		}
		if len(missing) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"the export for this import does not carry %v, so those variables have no stored "+
					"history even though others of this import do — an export includes only the "+
					"variables it was created with", missing))
		}
	}
	return warnings
}
