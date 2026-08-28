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
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	flowengine "github.com/SENERGY-Platform/analytics-flow-engine/lib"
	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
)

// The import tools that are not discovery.
//
// Signal discovery is deliberately absent from this file: an import is found
// through resolve_semantic_selection, alongside devices, because both are
// described by the same content variables and a developer asking for a signal
// should not have to know which kind the platform delivers it from. What is here
// is what that resolution cannot do — look one up by id, turn a chosen variable
// into something deployable, and, below, create the import or the export that a
// resolution found the platform does not have yet.
//
// list_import_types is not an exception to that rule. It searches the *catalogue*
// — what could be deployed — and structurally cannot answer what data the
// platform carries, because a type has no instance, no topic and no history. The
// resolution names the matching types itself in deployable_import_types, so a
// model that only ever calls resolve_semantic_selection still learns they exist;
// this tool is for naming one directly and for browsing.

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

// ---- list_import_types (L0) ----

type listImportTypesInput struct {
	Search        string   `json:"search"`
	FunctionID    string   `json:"function_id"`
	AspectID      string   `json:"aspect_id"`
	ImportTypeIDs []string `json:"import_type_ids"`
	Limit         int64    `json:"limit"`
}

// listImportTypes reads the import type catalogue.
//
// It exists because every other route to an import type id runs through an
// instance: discovery joins each matching type to its instances and reports one
// row per instance, and get_import_type_metadata takes an id. So the type nobody
// has deployed yet — the only kind create_import_instance is for — was
// unreachable except by pasting an id from the platform UI.
func (s *surface) listImportTypes(ctx context.Context, req Request) (any, error) {
	var in listImportTypesInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 || limit > s.deps.DeviceLimit {
		limit = s.deps.DeviceLimit
	}

	notes := []string{}
	criteria := []imports.TypeCriterion{}
	if in.FunctionID != "" || in.AspectID != "" {
		criterion := imports.TypeCriterion{FunctionID: in.FunctionID}
		if in.AspectID != "" {
			// Expanded here because import-repository matches aspect ids literally,
			// unlike the device repository. Without the subtree, an import type
			// described against a child aspect is missing from the answer and nothing
			// says so.
			criterion.AspectIDs = s.aspectSubtree(ctx, req.Token, in.AspectID)
			if len(criterion.AspectIDs) == 0 {
				criterion.AspectIDs = []string{in.AspectID}
				notes = append(notes, "the aspect subtree could not be read, so only the aspect "+
					"itself was matched; an import type described against a narrower aspect below it "+
					"is missing from this answer")
			}
		}
		criteria = append(criteria, criterion)
	}

	result, err := s.deps.Imports.ListTypes(ctx, req.Token, imports.TypeListOptions{
		Search:   in.Search,
		IDs:      in.ImportTypeIDs,
		Criteria: criteria,
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}

	listed := make([]map[string]any, 0, len(result.Types))
	for _, importType := range result.Types {
		listed = append(listed, importTypeView(importType, criteria))
	}

	note := "import types, not imports: nothing here is running and none of it carries data " +
		"yet. A type is a blueprint — create_import_instance deploys one, and the instance " +
		"is what has a Kafka topic. Check list_import_instances with import_type_ids before " +
		"deploying: an instance of this type may already exist."
	if len(criteria) == 0 {
		note += " No criteria were given, so the variables of each type are counted rather " +
			"than listed; read one with get_import_type_metadata."
	}
	answer := map[string]any{
		"import_types": listed,
		"total":        result.Total,
		"limit":        limit,
		"note":         note,
	}
	if result.Total < 0 {
		notes = append(notes, "upstream reported no total, so whether this page is the whole "+
			"answer is unknown")
	} else if result.Total > int64(len(listed)) {
		answer["truncated"] = true
	}
	if len(notes) > 0 {
		answer["notes"] = notes
	}
	return answer, nil
}

// aspectSubtree resolves an aspect id to itself plus its descendants, or nil if
// the ontology could not be read. Nil is a degraded answer the caller reports
// rather than an error: a narrower match still answers the question asked, and
// failing the whole listing over it would be worse.
func (s *surface) aspectSubtree(ctx context.Context, token, aspectID string) []string {
	if s.deps.Ontology == nil {
		return nil
	}
	snap, err := s.deps.Ontology.Snapshot(ctx, token)
	if err != nil || snap == nil {
		return nil
	}
	return ontology.AspectSubtreeIDs(snap.AspectNodes, aspectID)
}

// importTypeView is one catalogue row.
//
// The variables are listed only when criteria narrowed the query, and counted
// otherwise. A browse over the whole catalogue would otherwise pay for every leaf
// of every type — and the answer to "which variable do I want" is
// get_import_type_metadata, one type at a time.
func importTypeView(importType dsmodel.ImportType, criteria []imports.TypeCriterion) map[string]any {
	blocking := imports.BlockingCredentials(importType)
	row := map[string]any{
		"import_type_id":   importType.Id,
		"name":             importType.Name,
		"description":      importType.Description,
		"required_configs": imports.RequiredConfigs(importType),
		// Stated rather than left to be inferred from an empty credential list: a
		// reader who does not know the credential rule would infer it the wrong way.
		"deployable": len(blocking) == 0,
	}
	if len(blocking) > 0 {
		row["blocking_credentials"] = blocking
		row["reason"] = "this import type cannot be deployed from a chat: " +
			strings.Join(blocking, ", ") + " reads as a credential and the type declares no " +
			"default. The developer creates this import in the platform's own import dialog."
	}

	if len(criteria) == 0 {
		row["variables"] = len(imports.TypeVariables(importType))
		return row
	}

	matching := imports.MatchingVariables(importType, criteria)
	rows := make([]map[string]any, 0, len(matching))
	for _, variable := range matching {
		entry := map[string]any{"variable_path": variable.Path, "type": variable.Type}
		if variable.CharacteristicID != nil {
			entry["characteristic_id"] = *variable.CharacteristicID
		}
		if variable.FunctionID != "" {
			entry["function_id"] = variable.FunctionID
		}
		if variable.AspectID != "" {
			entry["aspect_id"] = variable.AspectID
		}
		rows = append(rows, entry)
	}
	row["matching_variables"] = rows
	if len(rows) == 0 {
		// Upstream's criteria index is flattened per import type, so a type matches
		// when its variables carry the criteria between them rather than one of them
		// carrying both.
		row["reason"] = "this type matches, but no single variable of it carries both the " +
			"function and the aspect; read it with get_import_type_metadata"
	}
	return row
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

// ---- create_import_instance, create_export and their two undos ----
//
// The four tools that change the platform. Everything above this line reads.
//
// Each is Confirm, so Dispatch holds it and the developer sees the arguments
// before anything runs (D11, §5.10). Two properties beyond that are this file's
// and cannot be left to the confirmation:
//
//   - **A delete reaches only what this session created.** §5.8 denies
//     delete_platform_data, and both deletions here destroy stored data —
//     an export's timescale table, an import's Kafka topic. What makes them
//     permissible is that the id has to be in the session's own creation log, so
//     the widest thing either can do is undo a deployment made minutes earlier
//     in the same conversation. An id the model read somewhere is not deletable,
//     and no argument from the model changes that.
//
//   - **A creation is recorded even when the tool answer is not read.** The
//     record is what makes the undo possible, so it is written before the answer
//     is assembled, and a failure to write it is reported to the developer rather
//     than swallowed: the import exists either way, and the difference is whether
//     chat can remove it again.

type createImportInstanceInput struct {
	ImportTypeID string `json:"import_type_id"`
	Name         string `json:"name"`
	Rationale    string `json:"rationale"`
	Configs      []struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	} `json:"configs"`
	Restart *bool `json:"restart"`
}

func (s *surface) createImportInstance(ctx context.Context, req Request) (any, error) {
	var in createImportInstanceInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.ImportTypeID) == "" {
		return nil, fmt.Errorf("%w: import_type_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}

	configs := make([]imports.ConfigValue, 0, len(in.Configs))
	for _, config := range in.Configs {
		configs = append(configs, imports.ConfigValue{Name: config.Name, Value: config.Value})
	}

	req.Progress("deploying", "asking import-deploy to start the container")
	created, err := s.deps.Imports.CreateInstance(ctx, req.Token, imports.CreateInstanceRequest{
		ImportTypeID: in.ImportTypeID,
		Name:         in.Name,
		Configs:      configs,
		Restart:      in.Restart,
	})
	if err != nil {
		return nil, invalidIfRequest(err)
	}

	answer := map[string]any{
		"instance":  importInstanceView(created.Instance, nil),
		"rationale": in.Rationale,
		"warnings": append(created.Notes,
			"the container has just been asked to start, so its status will read unknown or "+
				"transitioning for a while; list_import_instances answers whether it came up",
			"a new import has no stored past at all: it has no export, so nothing of it is in "+
				"timescale until one is created"),
		"note": "This import now exists on the platform and is the developer's to keep. It was " +
			"created with the developer's own permissions, and only this chat session can " +
			"remove it again from here.",
	}
	if len(created.Defaulted) > 0 {
		answer["defaulted_configs"] = created.Defaulted
	}
	if note := s.recordCreation(ctx, req, Creation{
		Kind: CreatedImportInstance,
		ID:   created.Instance.Id,
		Name: created.Instance.Name,
		Tool: "create_import_instance",
	}); note != "" {
		answer["warnings"] = append(answer["warnings"].([]string), note)
	}
	return answer, nil
}

type deleteImportInstanceInput struct {
	InstanceID string `json:"instance_id"`
	Rationale  string `json:"rationale"`
}

func (s *surface) deleteImportInstance(ctx context.Context, req Request) (any, error) {
	var in deleteImportInstanceInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	created, err := s.creationOf(ctx, req, CreatedImportInstance, in.InstanceID, in.Rationale)
	if err != nil {
		return nil, err
	}

	if err := s.deps.Imports.DeleteInstance(ctx, req.Token, created.ID); err != nil {
		return nil, invalidIfRequest(err)
	}
	return map[string]any{
		"deleted":     created,
		"rationale":   in.Rationale,
		"note":        "the instance, its container and its kafka topic are gone; anything that was consuming that topic now receives nothing",
		"undoable":    false,
		"next_action": "creating it again produces a different instance with a different id and a different topic",
	}, nil
}

type createExportInput struct {
	InstanceID      string `json:"instance_id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Rationale       string `json:"rationale"`
	Offset          string `json:"offset"`
	TimePath        string `json:"time_path"`
	TimestampFormat string `json:"timestamp_format"`
	Values          []struct {
		VariablePath string `json:"variable_path"`
		Column       string `json:"column"`
		Tag          bool   `json:"tag"`
	} `json:"values"`
}

func (s *surface) createExport(ctx context.Context, req Request) (any, error) {
	var in createExportInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.InstanceID) == "" {
		return nil, fmt.Errorf("%w: instance_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}

	values := make([]imports.ExportValueRequest, 0, len(in.Values))
	for _, value := range in.Values {
		values = append(values, imports.ExportValueRequest{
			VariablePath: value.VariablePath,
			Column:       value.Column,
			Tag:          value.Tag,
		})
	}

	req.Progress("creating", "asking analytics-serving for the export")
	created, err := s.deps.Imports.CreateExport(ctx, req.Token, imports.CreateExportRequest{
		InstanceID:      in.InstanceID,
		Name:            in.Name,
		Description:     in.Description,
		Values:          values,
		Offset:          in.Offset,
		TimePath:        in.TimePath,
		TimestampFormat: in.TimestampFormat,
	})
	if err != nil {
		return nil, invalidIfRequest(err)
	}

	columns := make([]map[string]any, 0, len(created.Export.Values))
	for _, value := range created.Export.Values {
		columns = append(columns, map[string]any{
			// The export stores the path message-relative, which is how everything else
			// a model has seen about this import is keyed.
			"variable_path": value.Path,
			"column":        value.Name,
			"type":          value.Type,
			"tag":           value.Tag,
		})
	}

	answer := map[string]any{
		"export_id":   created.Export.ID,
		"export_name": created.Export.Name,
		"instance_id": in.InstanceID,
		"rationale":   in.Rationale,
		"columns":     columns,
		"derived":     created.Derived,
		"warnings": append(created.Notes,
			"nothing that arrived before this export existed is stored beyond what the kafka "+
				"topic still retains; an export is not retroactive",
			"the column names are this export's own, not the variable paths — a timescale query "+
				"takes export_id and the columns above"),
		"note": "This export writes to timescale from now on. It will take a moment before the " +
			"first rows land, so a profile taken immediately reads as empty rather than as broken. " +
			"Verify it with probe_export_data once rows have had time to arrive: an export is " +
			"accepted, deploys and stores nothing when a value path names something the message " +
			"does not carry, and the failure is silent everywhere else — the export listing, this " +
			"answer and the stored byte count all look healthy. That check reports a column that is " +
			"null in every row by name.",
	}
	if note := s.recordCreation(ctx, req, Creation{
		Kind: CreatedExport,
		ID:   created.Export.ID,
		Name: created.Export.Name,
		Tool: "create_export",
	}); note != "" {
		answer["warnings"] = append(answer["warnings"].([]string), note)
	}
	return answer, nil
}

type deleteExportInput struct {
	ExportID  string `json:"export_id"`
	Rationale string `json:"rationale"`
}

func (s *surface) deleteExport(ctx context.Context, req Request) (any, error) {
	var in deleteExportInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	created, err := s.creationOf(ctx, req, CreatedExport, in.ExportID, in.Rationale)
	if err != nil {
		return nil, err
	}

	if err := s.deps.Imports.DeleteExport(ctx, req.Token, created.ID); err != nil {
		return nil, invalidIfRequest(err)
	}
	return map[string]any{
		"deleted":   created,
		"rationale": in.Rationale,
		"note": "the export and its timescale table are gone, so the import has no stored history " +
			"again; what the table held is not recoverable",
		"undoable": false,
	}, nil
}

// creationOf is the gate both delete tools pass through.
//
// It answers with the recorded creation rather than a boolean, so the caller
// deletes the id ODE recorded rather than the one the model sent — the two are
// the same string here, and making the answer carry it means a future caller
// cannot accidentally pass the unchecked one.
func (s *surface) creationOf(ctx context.Context, req Request, kind CreationKind, id, rationale string) (Creation, error) {
	if strings.TrimSpace(id) == "" {
		return Creation{}, fmt.Errorf("%w: an id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(rationale) == "" {
		return Creation{}, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return Creation{}, fmt.Errorf(
			"%w: this call carries no chat session, so there is no record of what was created here "+
				"and nothing may be deleted", ErrInvalidInput)
	}

	recorded, err := s.deps.Creations.Creations(ctx, req.SessionID)
	if err != nil {
		// Refusing on a failed read rather than falling back to deleting is the whole
		// point of the gate: "could not check" must not become "went ahead".
		return Creation{}, fmt.Errorf("what this session created could not be read, so nothing "+
			"will be deleted: %w", err)
	}

	mine := []string{}
	for _, creation := range recorded {
		if creation.Kind != kind {
			continue
		}
		if creation.ID == id {
			return creation, nil
		}
		mine = append(mine, fmt.Sprintf("%s (%s)", creation.ID, creation.Name))
	}

	if len(mine) == 0 {
		return Creation{}, fmt.Errorf(
			"%w: this session created no %s, so there is nothing here to delete. ODE removes only "+
				"what it created in this conversation; anything else is deleted by the developer "+
				"in the platform's own interface, and telling them so is the answer",
			ErrInvalidInput, kind)
	}
	return Creation{}, fmt.Errorf(
		"%w: %s was not created in this session, so it will not be deleted from here. What this "+
			"session created: %v. Anything else is the developer's to remove in the platform's "+
			"own interface",
		ErrInvalidInput, id, mine)
}

// recordCreation writes the creation log entry, and returns the warning to hand
// the developer when it could not be written.
//
// It never fails the tool. The object exists on the platform by the time this
// runs, and answering with an error would tell the model the creation did not
// happen — which is the one thing that is certainly untrue.
func (s *surface) recordCreation(ctx context.Context, req Request, creation Creation) string {
	if s.deps.Creations == nil {
		return "this deployment keeps no record of what a session created, so " + creation.Tool +
			" cannot be undone from chat; removing it again is done in the platform's own interface"
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return "this call carries no chat session, so what was just created was not recorded and " +
			"cannot be removed from chat"
	}
	creation.At = time.Now()
	if err := s.deps.Creations.RecordCreation(ctx, req.SessionID, creation); err != nil {
		slog.ErrorContext(ctx, "could not record what a session created",
			"tool", creation.Tool, "kind", string(creation.Kind), "session", req.SessionID,
			"error", err)
		return "this was created on the platform but recording it here failed, so it cannot be " +
			"removed from chat: " + creation.ID + " is the id to remove it by in the platform's " +
			"own interface"
	}
	return ""
}

// invalidIfRequest maps the imports package's own request errors onto
// ErrInvalidInput, so that a model's bad argument is recorded as invalid_input
// rather than as a platform failure. Anything else is the platform's verdict and
// is passed through unchanged.
func invalidIfRequest(err error) error {
	if errors.Is(err, imports.ErrInvalidRequest) {
		return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	return err
}
