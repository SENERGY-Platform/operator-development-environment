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
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Reading an export's own definition, which is what anything that reads its
// stored data needs first.
//
// timescale-wrapper addresses an export's table by export id and takes column
// **names**. Those names exist in exactly one place — the export in
// analytics-serving — and they are not derivable from the import type: the column
// is named by whoever created the export. So a query over an export starts here,
// and an export id alone is not enough to read one.
//
// What this adds beyond History is the direction. History answers "does this
// import have stored data", from an instance id. This answers "what is in this
// export", from an export id, and it is the form a profiler needs.

// ExportDefinition is one export described the way a reader of its timescale
// table needs it: the columns, and what can be said about what they mean.
type ExportDefinition struct {
	ExportID string `json:"export_id"`
	Name     string `json:"name,omitempty"`
	// Source is analytics-serving's FilterType and SourceID its Filter.
	// "import_id" with an instance id is an import export, which is the only kind
	// whose columns can be given semantics — see ExportColumnInfo.
	Source   string             `json:"source,omitempty"`
	SourceID string             `json:"source_id,omitempty"`
	Topic    string             `json:"topic,omitempty"`
	Columns  []ExportColumnInfo `json:"columns"`
	// Notes carry what could not be established. An export whose semantics are
	// missing because the import type could not be read must not be
	// indistinguishable from one whose import type declares none.
	Notes []string `json:"notes,omitempty"`
}

// ExportColumnInfo is one column of an export's table.
//
// Column and VariablePath are both carried because neither substitutes for the
// other: the query needs the column, and a developer who knows the variable
// knows the path. Everything below them is the import type's, and is absent
// wherever the export is not an import export or the type could not be read.
type ExportColumnInfo struct {
	Column string `json:"column"`
	// Type is the export worker's own column vocabulary — float, int, bool,
	// string — which is what an export declares. It is not a platform content
	// variable type, and the two are mapped where they meet (see
	// exportTypeOf in create.go for the other direction).
	//
	// It is read from the same unchecked shape Export is, with the consequence
	// worth knowing: a rename of `Values[].Type` upstream would not break the
	// build, it would make every column of every export report as having no
	// readable type — so a profile of an export refusing with "no column that can
	// be read as a series" is the symptom to look for.
	Type             string  `json:"type,omitempty"`
	VariablePath     string  `json:"variable_path,omitempty"`
	CharacteristicID *string `json:"characteristic_id"`
	FunctionID       string  `json:"function_id,omitempty"`
	AspectID         string  `json:"aspect_id,omitempty"`
	Tag              bool    `json:"tag,omitempty"`
}

// ExportDefinition resolves one export by id.
//
// The lookup is a bounded scan of the export listing, not a read by id, for the
// reason History's is: analytics-serving cannot filter its listing, and the
// listing is the route ODE already depends on. A scan that hits its bound says so
// rather than reporting the export as absent — an export that exists and was not
// reached is a different answer from one that does not exist, and only the second
// means "check the id".
//
// Semantics are best-effort by design. An export's columns are readable without
// them: the type is declared on the export itself, and a profile over a column
// with no characteristic reports no unit rather than failing. So a note is added
// and the definition is returned.
func (s *Service) ExportDefinition(ctx context.Context, token string, exportID string) (ExportDefinition, error) {
	if s.exports == nil {
		return ExportDefinition{}, fmt.Errorf(
			"%w: no analytics-serving is configured, so an export's column names cannot be read — and "+
				"a query over an export needs them: timescale-wrapper takes column names, and they exist "+
				"only in the export", ErrInvalidRequest)
	}
	exportID = strings.TrimSpace(exportID)
	if exportID == "" {
		return ExportDefinition{}, fmt.Errorf("%w: an export id is required", ErrInvalidRequest)
	}

	found, total, err := s.exports.ListExports(ctx, token, exportListLimit, 0)
	if err != nil {
		return ExportDefinition{}, err
	}

	var export Export
	matched := false
	for _, candidate := range found {
		if candidate.ID == exportID {
			export, matched = candidate, true
			break
		}
	}
	if !matched {
		if total > int64(len(found)) || len(found) >= exportListLimit {
			return ExportDefinition{}, fmt.Errorf(
				"%w: export %s was not among the first %d of %s exports, and analytics-serving cannot "+
					"filter its listing by id — the export may exist and not have been reached",
				ErrInvalidRequest, exportID, len(found), strconv.FormatInt(total, 10))
		}
		return ExportDefinition{}, fmt.Errorf(
			"%w: no export %s is visible to this account", ErrInvalidRequest, exportID)
	}

	definition := ExportDefinition{
		ExportID: export.ID,
		Name:     export.Name,
		Source:   export.FilterType,
		SourceID: export.Filter,
		Topic:    export.Topic,
		Columns:  make([]ExportColumnInfo, 0, len(export.Values)),
	}
	for _, value := range export.Values {
		definition.Columns = append(definition.Columns, ExportColumnInfo{
			Column: value.Name,
			Type:   value.Type,
			// Reported as the export stores it, unchanged. An export whose paths lost
			// the `value` envelope resolves against the message root and writes null
			// columns; putting the prefix back here would report it as if it had not.
			VariablePath: value.Path,
			Tag:          value.Tag,
		})
	}

	if definition.Source != FilterTypeImportExport {
		if definition.Source != "" {
			definition.Notes = append(definition.Notes, fmt.Sprintf(
				"this export is filtered by %s rather than by an import, so its columns carry no "+
					"characteristic, function or aspect: the semantics would have to come from whatever "+
					"feeds it, and ODE resolves that only for imports", definition.Source))
		}
		return definition, nil
	}

	semantics, note := s.importSemantics(ctx, token, definition.SourceID)
	if note != "" {
		definition.Notes = append(definition.Notes, note)
	}
	for i := range definition.Columns {
		path, err := MessagePath(definition.Columns[i].VariablePath)
		if err != nil {
			continue
		}
		if variable, known := semantics[path]; known {
			definition.Columns[i].CharacteristicID = variable.CharacteristicID
			definition.Columns[i].FunctionID = variable.FunctionID
			definition.Columns[i].AspectID = variable.AspectID
		}
	}
	return definition, nil
}

// importSemantics maps the import type's payload leaves by message-relative path.
//
// Two reads, and both are allowed to fail: the instance says which type it is and
// the type declares the semantics. The note is what a caller reports instead, so
// an export profiled without units says why rather than looking like an export
// whose import type declares none.
func (s *Service) importSemantics(ctx context.Context, token, instanceID string) (map[string]TypeVariable, string) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, "this export names no import instance, so its columns carry no semantics"
	}
	if s.types == nil {
		return nil, "no import-repository is configured, so the import type behind this export cannot " +
			"be read and its columns carry no characteristic, function or aspect"
	}

	instance, err := s.instances.ReadInstance(ctx, token, instanceID)
	if err != nil {
		return nil, fmt.Sprintf("the import instance %s behind this export could not be read, so its "+
			"columns carry no semantics: %v", instanceID, err)
	}
	importType, err := s.types.ReadImportType(ctx, token, instance.ImportTypeId)
	if err != nil {
		return nil, fmt.Sprintf("the import type %s behind this export could not be read, so its "+
			"columns carry no semantics: %v", instance.ImportTypeId, err)
	}

	out := map[string]TypeVariable{}
	for _, variable := range TypeVariables(importType) {
		out[variable.Path] = variable
	}
	return out, ""
}
