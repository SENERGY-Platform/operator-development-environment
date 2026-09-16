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

package experiments

import (
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// Resolved is one input topic described in the names a person reads: the
// launch_experiment card shows this rather than the raw InputTopic, and moving a
// topic to another device is a Retarget call that returns the same shape.
type Resolved struct {
	Topic    InputTopic        `json:"topic"`
	Device   ResolvedDevice    `json:"device"`
	Service  ResolvedService   `json:"service"`
	Mappings []ResolvedMapping `json:"mappings"`
	// Alternatives are the matched service's other queryable variables, written
	// in the same source convention as the topic's own mappings (mapping 0's,
	// since every mapping resolves to one service — see Retarget). A developer
	// uses this to correct a derivation that picked a plausible but wrong
	// variable.
	Alternatives []Alternative `json:"alternatives,omitempty"`
	Warnings     []string      `json:"warnings,omitempty"`
}

type ResolvedDevice struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DeviceTypeID   string `json:"device_type_id"`
	DeviceTypeName string `json:"device_type_name"`
}

type ResolvedService struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ResolvedMapping is one mapping with the variable it reads, named.
type ResolvedMapping struct {
	Dest             string `json:"dest"`
	Source           string `json:"source"`
	VariableName     string `json:"variable_name"`
	VariablePath     string `json:"variable_path"`
	Unit             string `json:"unit,omitempty"`
	CharacteristicID string `json:"characteristic_id,omitempty"`
	FunctionID       string `json:"function_id,omitempty"`
	AspectID         string `json:"aspect_id,omitempty"`
}

// Alternative is one other variable of the matched service, offered so a
// developer can correct a derivation that picked a plausible but wrong one.
type Alternative struct {
	Source       string `json:"source"`
	VariableName string `json:"variable_name"`
	VariablePath string `json:"variable_path"`
	Unit         string `json:"unit,omitempty"`
}

// ValidateResolvableTopic is the shape check Describe and Retarget both start
// with. Exported, unlike validateTopics in deployment.go, because the API
// handler needs it too: it names the device to read (topic.filterValue) before
// either function runs, so refusing here means a malformed topic never spends a
// device read on the way to being rejected. One helper rather than two copies,
// for the reason the confirmation-input change gives its own duplicated
// check: two copies drift.
func ValidateResolvableTopic(topic InputTopic) error {
	if strings.TrimSpace(topic.FilterValue) == "" {
		return fmt.Errorf("%w: an input topic needs a filterValue naming the device", ErrInvalidRequest)
	}
	if len(topic.Mappings) == 0 {
		return fmt.Errorf("%w: an input topic needs at least one mapping", ErrInvalidRequest)
	}
	if topic.FilterType != "DeviceId" {
		return fmt.Errorf(
			"%w: only a topic filtered by DeviceId names a device to resolve or retarget, not %q",
			ErrInvalidRequest, topic.FilterType)
	}
	return nil
}

// Describe renders a topic as proposed, against the device type it names.
func Describe(topic InputTopic, device models.ExtendedDevice) (Resolved, error) {
	if err := ValidateResolvableTopic(topic); err != nil {
		return Resolved{}, err
	}
	if device.DeviceType == nil {
		return Resolved{}, fmt.Errorf("%w: device %s carries no device type", ErrInvalidRequest, device.Id)
	}

	service, ok := findService(*device.DeviceType, topic.Name)
	if !ok {
		return Resolved{}, fmt.Errorf(
			"%w: no service on %s (device type %s) is named %s",
			ErrInvalidRequest, device.Id, device.DeviceTypeId, topic.Name)
	}

	resolvedMappings := make([]ResolvedMapping, 0, len(topic.Mappings))
	var warnings []string
	var firstTransform sourceTransform

	for i, mapping := range topic.Mappings {
		v, transform, ok := resolveOriginVariable(*device.DeviceType, service.Id, mapping.Source)
		if !ok {
			return Resolved{}, fmt.Errorf(
				"%w: mapping %s (%s) names no variable on %s",
				ErrInvalidRequest, mapping.Dest, mapping.Source, service.Name)
		}
		if i == 0 {
			firstTransform = transform
			if w := envelopeWarning(mapping.Dest, transform, mapping.Source, v.Path); w != "" {
				warnings = append(warnings, w)
			}
		} else if !transform.equals(firstTransform) {
			return Resolved{}, fmt.Errorf(
				"%w: mapping %s (%s) sits differently in the message than mapping %s (%s) does, "+
					"so at least one of the two resolved by accident; a topic is one message shape",
				ErrInvalidRequest, mapping.Dest, mapping.Source,
				topic.Mappings[0].Dest, topic.Mappings[0].Source)
		}
		resolvedMappings = append(resolvedMappings, resolvedMappingOf(mapping.Dest, mapping.Source, v))
		if w := queryableWarning(mapping.Dest, v); w != "" {
			warnings = append(warnings, w)
		}
	}

	return Resolved{
		Topic:        topic,
		Device:       resolvedDeviceOf(device),
		Service:      ResolvedService{ID: service.Id, Name: service.Name},
		Mappings:     resolvedMappings,
		Alternatives: alternativesOf(service, firstTransform, resolvedMappings),
		Warnings:     warnings,
	}, nil
}

// Retarget rewrites a topic for another device, deriving its name, filterValue
// and every mapping source from the new device's type.
//
// Every mapping has to land on the same service the first one does: a topic is
// one Kafka topic and cannot read two. So mapping 0 alone picks the target
// service (by path, else by function and aspect — see findCounterpart), and
// every later mapping is resolved against that one service only; a mapping with
// no counterpart there refuses the whole topic rather than silently reading a
// second service.
func Retarget(topic InputTopic, from, to models.ExtendedDevice) (Resolved, error) {
	if err := ValidateResolvableTopic(topic); err != nil {
		return Resolved{}, err
	}
	if from.DeviceType == nil {
		return Resolved{}, fmt.Errorf("%w: device %s carries no device type", ErrInvalidRequest, from.Id)
	}
	if to.DeviceType == nil {
		return Resolved{}, fmt.Errorf("%w: device %s carries no device type", ErrInvalidRequest, to.Id)
	}

	originService, ok := findService(*from.DeviceType, topic.Name)
	if !ok {
		return Resolved{}, fmt.Errorf(
			"%w: no service on %s (device type %s) is named %s",
			ErrInvalidRequest, from.Id, from.DeviceTypeId, topic.Name)
	}

	first := topic.Mappings[0]
	firstOrigin, firstTransform, ok := resolveOriginVariable(*from.DeviceType, originService.Id, first.Source)
	if !ok {
		return Resolved{}, fmt.Errorf(
			"%w: mapping %s (%s) names no variable on %s",
			ErrInvalidRequest, first.Dest, first.Source, originService.Name)
	}
	firstTarget, bySemantics, ok := findCounterpart(*to.DeviceType, firstOrigin)
	if !ok {
		return Resolved{}, fmt.Errorf(
			"%w: mapping %s (%s) has no counterpart on device type %s",
			ErrInvalidRequest, first.Dest, first.Source, to.DeviceTypeId)
	}
	targetService, ok := serviceByID(*to.DeviceType, firstTarget.ServiceID)
	if !ok {
		// Unreachable in practice: findCounterpart only ever returns a variable
		// profiler.DeviceTypeVariables built from to.DeviceType.Services, so its
		// ServiceID always names one of them.
		return Resolved{}, fmt.Errorf(
			"%w: matched service %s not found on device type %s",
			ErrInvalidRequest, firstTarget.ServiceID, to.DeviceTypeId)
	}

	resolvedMappings := make([]ResolvedMapping, 0, len(topic.Mappings))
	newMappings := make([]TopicMapping, 0, len(topic.Mappings))
	var warnings []string

	appendMapping := func(dest string, origin, target profiler.Variable, transform sourceTransform, bySemantics bool) {
		newSource := transform.apply(target.Path)
		newMappings = append(newMappings, TopicMapping{Dest: dest, Source: newSource})
		resolvedMappings = append(resolvedMappings, resolvedMappingOf(dest, newSource, target))
		warnings = append(warnings, counterpartWarnings(dest, origin, target, bySemantics)...)
	}

	// Raised before any counterpart warning, because it is about the source the
	// derivation started from rather than about the variable it landed on: if this
	// reading of the original source is wrong, every path below it is wrong too.
	if w := envelopeWarning(first.Dest, firstTransform, first.Source, firstOrigin.Path); w != "" {
		warnings = append(warnings, w)
	}
	appendMapping(first.Dest, firstOrigin, firstTarget, firstTransform, bySemantics)

	for _, mapping := range topic.Mappings[1:] {
		origin, transform, ok := resolveOriginVariable(*from.DeviceType, originService.Id, mapping.Source)
		if !ok {
			return Resolved{}, fmt.Errorf(
				"%w: mapping %s (%s) names no variable on %s",
				ErrInvalidRequest, mapping.Dest, mapping.Source, originService.Name)
		}
		if !transform.equals(firstTransform) {
			return Resolved{}, fmt.Errorf(
				"%w: mapping %s (%s) sits differently in the message than mapping %s (%s) does, "+
					"so at least one of the two resolved by accident; a topic is one message shape",
				ErrInvalidRequest, mapping.Dest, mapping.Source, first.Dest, first.Source)
		}
		target, sem, ok := findCounterpartInService(targetService, origin)
		if !ok {
			return Resolved{}, fmt.Errorf(
				"%w: mapping %s (%s) has no counterpart on %s, so the topic would have to read a second service",
				ErrInvalidRequest, mapping.Dest, mapping.Source, targetService.Name)
		}
		appendMapping(mapping.Dest, origin, target, transform, sem)
	}

	newTopic := InputTopic{
		Name:        serviceTopicName(targetService.Id),
		FilterType:  "DeviceId",
		FilterValue: to.Id,
		Mappings:    newMappings,
	}

	return Resolved{
		Topic:        newTopic,
		Device:       resolvedDeviceOf(to),
		Service:      ResolvedService{ID: targetService.Id, Name: targetService.Name},
		Mappings:     resolvedMappings,
		Alternatives: alternativesOf(targetService, firstTransform, resolvedMappings),
		Warnings:     warnings,
	}, nil
}

// serviceTopicName is the Kafka topic name Operator Lib expects for a device
// service.
//
// Assumption, and the only statement of this convention
// anywhere in the repository is the tool schema's example,
// "urn_infai_ses_service_..." (pkg/tools/surface.go), and no Go code performs
// the conversion. If a service id ever legitimately contains an underscore, this
// is no longer invertible — which is exactly why findService below matches
// forward (computing this for each candidate service) instead of trying to
// reverse it.
func serviceTopicName(serviceID string) string {
	return strings.ReplaceAll(serviceID, ":", "_")
}

// findService locates the service on a device type whose Kafka topic name
// matches a topic's Name.
func findService(dt models.DeviceType, topicName string) (models.Service, bool) {
	for _, service := range dt.Services {
		if serviceTopicName(service.Id) == topicName {
			return service, true
		}
	}
	return models.Service{}, false
}

// serviceByID looks a service up by id on a device type already in hand, so a
// variable's ServiceID (from profiler.DeviceTypeVariables) can be turned back
// into the models.Service that carries its Name.
func serviceByID(dt models.DeviceType, id string) (models.Service, bool) {
	for _, service := range dt.Services {
		if service.Id == id {
			return service, true
		}
	}
	return models.Service{}, false
}

// transformKind is which of the three shapes A3 tries fixed a mapping's source
// to its profiler.Variable.Path.
type transformKind int

const (
	transformIdentity transformKind = iota
	transformSuffix
	transformPrefix
)

// sourceTransform is how a mapping's source differs from the profiler's own
// Variable.Path — a relation nothing in the repository states: quick.json's variable_path and the launch fixtures' source
// disagree, e.g. "value.power" versus "value.power.value"). It is derived per
// mapping from what actually resolves, never assumed, so it can be inverted to
// write a new source in the same convention once a counterpart path is found.
type sourceTransform struct {
	kind transformKind
	// literal is the dot-segment the mapping's own convention adds beyond the
	// variable's path — the "value" in "value.power.value" over "value.power".
	literal string
}

func (t sourceTransform) apply(path string) string {
	switch t.kind {
	case transformSuffix:
		return path + "." + t.literal
	case transformPrefix:
		return t.literal + "." + path
	default:
		return path
	}
}

// equals compares two derived conventions. Every mapping of one topic has to
// produce the same one: a topic is one Kafka message shape, so two mappings
// disagreeing about where the variable path sits inside it is not two
// conventions, it is a sign that at least one of the two resolved by accident.
func (t sourceTransform) equals(other sourceTransform) bool {
	return t.kind == other.kind && t.literal == other.literal
}

// envelopeWarning names the convention a non-identity transform assumed.
//
// It exists because resolveOriginVariable's fallbacks can hit by accident. A
// source of "value.power.total", on a service carrying both value.power and
// value.total, resolves to the variable value.power with "total" left over —
// and the same leftover is then appended to whatever path the counterpart has,
// producing a source that is well-formed, plausible on the card, and addresses
// nothing. Nothing in this repository states the convention (see
// sourceTransform), so the derivation cannot tell that case from a deployment
// whose sources genuinely carry an extra segment. What it can do is refuse to
// make the assumption quietly: the developer reads this before approving, and
// the accidental case is the one where the leftover segment reads as nonsense.
func envelopeWarning(dest string, t sourceTransform, source, path string) string {
	switch t.kind {
	case transformSuffix:
		return fmt.Sprintf(
			"mapping %s: read %q as the variable %q followed by %q; check that %q is part of this "+
				"deployment's message shape and not a path that simply does not exist",
			dest, source, path, t.literal, t.literal)
	case transformPrefix:
		return fmt.Sprintf(
			"mapping %s: read %q as %q followed by the variable %q; check that %q is part of this "+
				"deployment's message shape and not a path that simply does not exist",
			dest, source, t.literal, path, t.literal)
	default:
		return ""
	}
}

// resolveOriginVariable finds the profiler.Variable a mapping's source names on
// one service, trying the source itself, then the source with its last
// dot-segment removed, then with its first removed — in that order. The transform that hits is returned so Retarget can
// invert it when writing the new source from a counterpart's Path.
func resolveOriginVariable(dt models.DeviceType, serviceID, source string) (profiler.Variable, sourceTransform, bool) {
	if v, ok := profiler.FindVariable(dt, serviceID, source); ok {
		return v, sourceTransform{kind: transformIdentity}, true
	}
	if idx := strings.LastIndex(source, "."); idx >= 0 {
		head, tail := source[:idx], source[idx+1:]
		if v, ok := profiler.FindVariable(dt, serviceID, head); ok {
			return v, sourceTransform{kind: transformSuffix, literal: tail}, true
		}
	}
	if idx := strings.Index(source, "."); idx >= 0 {
		head, tail := source[:idx], source[idx+1:]
		if v, ok := profiler.FindVariable(dt, serviceID, tail); ok {
			return v, sourceTransform{kind: transformPrefix, literal: head}, true
		}
	}
	return profiler.Variable{}, sourceTransform{}, false
}

// findCounterpart locates the variable on a target device type that reads the
// same thing an origin variable did: first by the same path, else by the same
// function and aspect, both of which the origin variable has to declare. The
// second return says whether the match came from the semantic branch, for the
// warning that names what it replaced.
//
// profiler.DeviceTypeVariables already walks services in Service.Id order, so
// the first hit is deterministic without this function sorting anything itself.
func findCounterpart(toType models.DeviceType, origin profiler.Variable) (profiler.Variable, bool, bool) {
	for _, v := range profiler.DeviceTypeVariables(toType) {
		if v.Path == origin.Path {
			return v, false, true
		}
	}
	if origin.FunctionID == "" || origin.AspectID == "" {
		return profiler.Variable{}, false, false
	}
	for _, v := range profiler.DeviceTypeVariables(toType) {
		if v.FunctionID == origin.FunctionID && v.AspectID == origin.AspectID {
			return v, true, true
		}
	}
	return profiler.Variable{}, false, false
}

// findCounterpartInService is findCounterpart narrowed to the one service
// mapping 0 already chose. Every later mapping of the same topic has to resolve
// there too, or the topic would have to read a second Kafka topic to satisfy it.
func findCounterpartInService(service models.Service, origin profiler.Variable) (profiler.Variable, bool, bool) {
	for _, v := range profiler.ServiceVariables(service) {
		if v.Path == origin.Path {
			return v, false, true
		}
	}
	if origin.FunctionID == "" || origin.AspectID == "" {
		return profiler.Variable{}, false, false
	}
	for _, v := range profiler.ServiceVariables(service) {
		if v.FunctionID == origin.FunctionID && v.AspectID == origin.AspectID {
			return v, true, true
		}
	}
	return profiler.Variable{}, false, false
}

func resolvedMappingOf(dest, source string, v profiler.Variable) ResolvedMapping {
	return ResolvedMapping{
		Dest: dest, Source: source,
		VariableName: v.Name, VariablePath: v.Path,
		Unit:             v.UnitReference,
		CharacteristicID: v.CharacteristicID,
		FunctionID:       v.FunctionID,
		AspectID:         v.AspectID,
	}
}

func resolvedDeviceOf(device models.ExtendedDevice) ResolvedDevice {
	return ResolvedDevice{
		ID:             device.Id,
		Name:           devices.DisplayName(device),
		DeviceTypeID:   device.DeviceTypeId,
		DeviceTypeName: devices.TypeName(device),
	}
}

// alternativesOf lists a service's other queryable variables, written in the
// mapping-0 convention so a source offered here is one a developer can drop
// straight into a mapping. Variables
// already used by a resolved mapping are left out — they are not "other".
func alternativesOf(service models.Service, transform sourceTransform, used []ResolvedMapping) []Alternative {
	usedPaths := make(map[string]bool, len(used))
	for _, m := range used {
		usedPaths[m.VariablePath] = true
	}
	var out []Alternative
	for _, v := range profiler.ServiceVariables(service) {
		if !v.Queryable || usedPaths[v.Path] {
			continue
		}
		out = append(out, Alternative{
			Source: transform.apply(v.Path), VariableName: v.Name, VariablePath: v.Path, Unit: v.UnitReference,
		})
	}
	return out
}

// queryableWarning is the one warning Describe can raise on its own account,
// without a counterpart search: the variable a mapping already names might not
// be queryable (profiler.Variable.Reason says why), and a developer previewing
// the topic should see that before approving a launch that reads it.
func queryableWarning(dest string, v profiler.Variable) string {
	if v.Queryable {
		return ""
	}
	return fmt.Sprintf("mapping %s: %s is not queryable: %s", dest, v.Path, v.Reason)
}

// counterpartWarnings are Retarget's three warnings: a semantic rather than a path match, a characteristic that changed,
// and a counterpart that is not queryable.
func counterpartWarnings(dest string, origin, target profiler.Variable, bySemantics bool) []string {
	var out []string
	if bySemantics {
		out = append(out, fmt.Sprintf(
			"mapping %s: matched by function and aspect rather than by path; replaced %s with %s",
			dest, origin.Path, target.Path))
	}
	if origin.CharacteristicID != target.CharacteristicID {
		out = append(out, fmt.Sprintf(
			"mapping %s: characteristic changed from %q to %q; Operator Lib converts nothing, so the operator will read %s in a different unit",
			dest, origin.CharacteristicID, target.CharacteristicID, dest))
	}
	if w := queryableWarning(dest, target); w != "" {
		out = append(out, w)
	}
	return out
}
