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
	"sort"
	"strings"

	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
)

// What a caller can say about an import type before anything has been deployed
// from it.
//
// Everything here is derived from the type alone, because that is all there is:
// a type nobody has instantiated has no topic, no container status and no
// history, and every question a Selectable answers is a question about an
// instance. What remains is what the type declares — which variables it would
// publish, and what it would have to be configured with.

// TypeVariable is one payload leaf of an import type's output.
//
// Path is message-relative — `value.temperature` — because that is the form an
// operator mapping and an export value take, and the form every other path in
// this package is in. The envelope leaves are not here at all: get_import_type_metadata
// reports them with a reason, which is the right answer when a developer is
// reading one type, and noise in a list of candidates.
type TypeVariable struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
	// CharacteristicID is canonical and never fabricated, as everywhere else here:
	// it decides the unit, and an invented one authorises a wrong conversion.
	CharacteristicID *string `json:"characteristic_id"`
	FunctionID       string  `json:"function_id,omitempty"`
	AspectID         string  `json:"aspect_id,omitempty"`
}

// TypeVariables walks an import type's output into its addressable payload
// leaves, sorted by path.
func TypeVariables(importType dsmodel.ImportType) []TypeVariable {
	out := []TypeVariable{}
	collectTypeVariables(&out, importType.Output, nil)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func collectTypeVariables(out *[]TypeVariable, variable dsmodel.ImportContentVariable, prefix []string) {
	path := append(append([]string{}, prefix...), variable.Name)
	if len(variable.SubContentVariables) == 0 {
		// A structure is a container rather than a value, and an envelope leaf —
		// import_id, time — is not a series. MessagePath decides both, so the rule
		// lives in one place for the whole package.
		if addressable, err := MessagePath(strings.Join(path, ".")); err == nil {
			*out = append(*out, TypeVariable{
				Path:             addressable,
				Type:             string(variable.Type),
				CharacteristicID: characteristic(variable.CharacteristicId),
				FunctionID:       variable.FunctionId,
				AspectID:         variable.AspectId,
			})
		}
		return
	}
	for _, sub := range variable.SubContentVariables {
		collectTypeVariables(out, sub, path)
	}
}

// MatchingVariables keeps the payload leaves that satisfy any of the criteria.
//
// ORed over the criteria, deliberately, although import-repository ANDs them to
// decide whether the *type* matches. The two are answering different questions:
// upstream asks whether this type carries all of what was wanted, possibly
// across different variables, and this asks which variables are the reason — so
// intersecting here would report nothing for a type that matched precisely
// because two of its variables each carried one half.
//
// A criterion with no function and no aspect matches everything, which is what
// an unnarrowed query means; callers that must not send one refuse it before it
// gets here.
func MatchingVariables(importType dsmodel.ImportType, criteria []TypeCriterion) []TypeVariable {
	all := TypeVariables(importType)
	if len(criteria) == 0 {
		return all
	}
	out := []TypeVariable{}
	for _, variable := range all {
		for _, criterion := range criteria {
			if criterion.matches(variable) {
				out = append(out, variable)
				break
			}
		}
	}
	return out
}

func (c TypeCriterion) matches(variable TypeVariable) bool {
	if c.FunctionID != "" && variable.FunctionID != c.FunctionID {
		return false
	}
	if len(c.AspectIDs) == 0 {
		return true
	}
	for _, id := range c.AspectIDs {
		if variable.AspectID == id {
			return true
		}
	}
	return false
}

// BlockingCredentials names the configs that make this import type impossible to
// deploy from a chat: credential-shaped, and with no default to fall back on.
//
// Reported rather than discovered on refusal, so that a model proposing a
// creation is told before the developer is asked to confirm one that
// CreateInstance would reject. The test is secretShaped's, so the two cannot
// disagree about what counts as a credential.
func BlockingCredentials(importType dsmodel.ImportType) []string {
	out := []string{}
	for _, config := range importType.Configs {
		if secretShaped(config.Name) && isEmptyValue(config.DefaultValue) {
			out = append(out, config.Name)
		}
	}
	sort.Strings(out)
	return out
}

// RequiredConfigs names the configs a caller has to decide: declared without a
// usable default, and not a credential — a credential is not a decision, it is a
// refusal (see BlockingCredentials).
func RequiredConfigs(importType dsmodel.ImportType) []string {
	out := []string{}
	for _, config := range importType.Configs {
		if secretShaped(config.Name) {
			continue
		}
		if isEmptyValue(config.DefaultValue) {
			out = append(out, config.Name)
		}
	}
	sort.Strings(out)
	return out
}
