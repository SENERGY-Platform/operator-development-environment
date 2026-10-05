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
	Topic   InputTopic      `json:"topic"`
	Device  ResolvedDevice  `json:"device"`
	Service ResolvedService `json:"service"`
	// Services are every service of Device's own device type, in the order the
	// device type itself declares them (see servicesOf). A developer uses this to
	// move the topic onto another service of the same device by hand — the
	// launch card's service Select — without the picker having to keep a whole
	// device type in memory just to offer the list.
	Services []ResolvedService `json:"services,omitempty"`
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
	// Guessed is true when RetargetToService could not find a counterpart for
	// this mapping on the chosen service and picked a queryable variable by
	// position instead (see RetargetToService and defaultVariable). It is the
	// one place ODE stops deriving and starts guessing, so a developer reading
	// the launch card needs to see it before approving.
	Guessed bool `json:"guessed,omitempty"`
}

// Alternative is one other variable of the matched service, offered so a
// developer can correct a derivation that picked a plausible but wrong one.
type Alternative struct {
	Source       string `json:"source"`
	VariableName string `json:"variable_name"`
	VariablePath string `json:"variable_path"`
	Unit         string `json:"unit,omitempty"`
}

// FitMatch is how closely a candidate device answers the same thing an input
// topic's first mapping already reads. The same three outcomes findCounterpart
// has always distinguished internally — FitMatch and Fit just give them names
// an API response can carry.
type FitMatch string

const (
	// FitPath is a variable on the candidate device with the same Path as the
	// topic's own mapping-0 variable.
	FitPath FitMatch = "path"
	// FitSemantics is a variable with no path match but the same FunctionID and
	// AspectID.
	FitSemantics FitMatch = "semantics"
	// FitNone is neither: the topic cannot be moved to this device without a
	// developer choosing a service and variable by hand.
	FitNone FitMatch = "none"
)

// Fit is one device's answer to "could this topic move here", computed without
// retargeting anything.
type Fit struct {
	Match FitMatch `json:"match"`
	// Service is where a move to this device would land, and it is set whatever
	// Match says — the two answer different questions. Match is how the topic's
	// first mapping found its counterpart, or that it found none; Service is the
	// service the move will read, derived where there was a counterpart and the
	// first one that can carry the topic where there was not. Naming it in both
	// cases is what makes a FitNone row pickable rather than a warning:
	// RetargetToService refuses a device it can derive nothing for unless it is
	// told which service to use, so the caller hands this back and the move
	// resolves.
	//
	// A pointer, and nil where no service resolves at all — the origin-side
	// failures, where nothing on this device type is the problem, and a device type
	// that can carry the topic on none of its services. Absent rather than an empty
	// ResolvedService{}, which a truthiness check would read as a landing service.
	Service *ResolvedService `json:"service,omitempty"`
	// Reason says why nothing could be derived. Set for FitNone only.
	Reason string `json:"reason,omitempty"`
}

// FitOf answers whether moving this topic to this device would resolve, and how
// its first mapping found its counterpart, so a candidate listing can rank every
// device a developer may execute without retargeting each one to find out. It
// reads only the two device types topic and from/to already carry: no series,
// no platform call.
//
// It ends by running the move it is about to promise. Deriving mapping 0 is not
// enough to know the answer: mapping 0 alone picks the service, and every later
// mapping then has to come out of that one service, because a topic is one Kafka
// topic. A device type that carries the first reading on a service too narrow for
// the rest refuses the whole topic — and a row that had reported FitPath on
// mapping 0 would have promised a move that fails on the click. The dry run costs
// nothing a caller is not already paying: it walks the same two device types, and
// the listing does one platform call for all of them together.
func FitOf(topic InputTopic, from, to models.ExtendedDevice) Fit {
	if from.DeviceType == nil || to.DeviceType == nil {
		return Fit{Match: FitNone, Reason: "one of the two devices carries no device type"}
	}
	if len(topic.Mappings) == 0 {
		return Fit{Match: FitNone, Reason: "the topic has no mapping to match"}
	}

	originService, ok := findService(*from.DeviceType, topic.Name)
	if !ok {
		return Fit{Match: FitNone, Reason: fmt.Sprintf("no service on device type %s is named %s", from.DeviceTypeId, topic.Name)}
	}

	first := topic.Mappings[0]
	origin, _, ok := resolveOriginVariable(*from.DeviceType, originService.Id, first.Source)
	if !ok {
		return Fit{Match: FitNone, Reason: fmt.Sprintf("mapping %s (%s) names no variable on %s", first.Dest, first.Source, originService.Name)}
	}

	target, bySemantics, ok := findCounterpart(*to.DeviceType, origin)
	if !ok {
		return Fit{
			Match:   FitNone,
			Reason:  "no variable with the same path or the same function and aspect",
			Service: firstLandableService(topic, from, to),
		}
	}
	targetService, ok := serviceByID(*to.DeviceType, target.ServiceID)
	if !ok {
		// Unreachable in practice for the reason findCounterpart's own comment
		// gives: it only ever returns a variable profiler.DeviceTypeVariables
		// built from to.DeviceType.Services, so its ServiceID always names one.
		return Fit{Match: FitNone, Reason: fmt.Sprintf("matched service %s not found on device type %s", target.ServiceID, to.DeviceTypeId)}
	}

	// The service the click will send, so the dry run is the call the click makes
	// and not a stricter cousin of it: with a serviceID present a later mapping
	// that finds no counterpart is guessed rather than refused.
	if _, err := RetargetToService(topic, from, to, targetService.Id); err != nil {
		return Fit{
			Match: FitNone,
			// The error already names the mapping and the service it could not be
			// placed on, which is what the row's tooltip has to say. Only the
			// sentinel wrapper is dropped — "invalid experiment request" tells a
			// developer reading a device list nothing.
			Reason: strings.TrimPrefix(err.Error(), ErrInvalidRequest.Error()+": "),
			// Not targetService: that is the one that just refused. Offering it back
			// would fail on the click for the second time, having said it was
			// pickable.
			Service: firstLandableService(topic, from, to),
		}
	}

	match := FitPath
	if bySemantics {
		match = FitSemantics
	}
	return Fit{Match: match, Service: &ResolvedService{ID: targetService.Id, Name: targetService.Name}}
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
		Services:     servicesOf(*device.DeviceType),
		Mappings:     resolvedMappings,
		Alternatives: alternativesOf(service, firstTransform, resolvedMappings),
		Warnings:     warnings,
	}, nil
}

// Retarget rewrites a topic for another device, deriving its name, filterValue
// and every mapping source from the new device's type. It is RetargetToService
// with no serviceID, so mapping 0 alone still picks the target service — by
// path, else by function and aspect, see findCounterpart — the way it always
// has; existing callers see no change in behaviour.
func Retarget(topic InputTopic, from, to models.ExtendedDevice) (Resolved, error) {
	return RetargetToService(topic, from, to, "")
}

// RetargetToService is Retarget with the target service fixed by the caller
// instead of derived from mapping 0's own match. A developer reaches this from
// the launch card's service Select once they already know which service they
// want; deriving it through mapping 0 first would refuse a service that fits
// their intent just because mapping 0's own variable happens to have no
// counterpart there.
//
// Every mapping still has to land on serviceID — a topic is one Kafka topic and
// cannot read two — but with serviceID set, a mapping with no counterpart no
// longer refuses the whole topic the way Retarget's own derivation does.
// Instead it falls back to a guess: the first queryable variable of serviceID
// that no other mapping of this topic already reads, preferring one whose Type
// matches the origin variable's (see defaultVariable). This is a guess, not a
// derivation — the mapping comes back with Guessed set and the response carries
// a warning naming it, because nothing here confirms the chosen variable holds
// what the mapping's dest expects. A service with no queryable variable left to
// guess from still refuses with ErrInvalidRequest: there is nothing to choose.
func RetargetToService(topic InputTopic, from, to models.ExtendedDevice, serviceID string) (Resolved, error) {
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

	var targetService models.Service
	var firstTarget profiler.Variable
	var firstBySemantics, firstGuessed bool

	if serviceID == "" {
		firstTarget, firstBySemantics, ok = findCounterpart(*to.DeviceType, firstOrigin)
		if !ok {
			return Resolved{}, fmt.Errorf(
				"%w: mapping %s (%s) has no counterpart on device type %s",
				ErrInvalidRequest, first.Dest, first.Source, to.DeviceTypeId)
		}
		targetService, ok = serviceByID(*to.DeviceType, firstTarget.ServiceID)
		if !ok {
			// Unreachable in practice: findCounterpart only ever returns a variable
			// profiler.DeviceTypeVariables built from to.DeviceType.Services, so its
			// ServiceID always names one of them.
			return Resolved{}, fmt.Errorf(
				"%w: matched service %s not found on device type %s",
				ErrInvalidRequest, firstTarget.ServiceID, to.DeviceTypeId)
		}
	} else {
		targetService, ok = serviceByID(*to.DeviceType, serviceID)
		if !ok {
			return Resolved{}, fmt.Errorf(
				"%w: service %s is not on device type %s", ErrInvalidRequest, serviceID, to.DeviceTypeId)
		}
		firstTarget, firstBySemantics, ok = findCounterpartInService(targetService, firstOrigin)
		if !ok {
			def, defOk := defaultVariable(targetService, firstOrigin.Type, nil)
			if !defOk {
				return Resolved{}, fmt.Errorf(
					"%w: mapping %s (%s) has no counterpart on %s, and %s has no queryable variable left to guess one from",
					ErrInvalidRequest, first.Dest, first.Source, targetService.Name, targetService.Name)
			}
			firstTarget = def
			firstGuessed = true
		}
	}

	resolvedMappings := make([]ResolvedMapping, 0, len(topic.Mappings))
	newMappings := make([]TopicMapping, 0, len(topic.Mappings))
	var warnings []string
	usedPaths := make(map[string]bool, len(topic.Mappings))

	appendMapping := func(dest string, origin, target profiler.Variable, transform sourceTransform, bySemantics, guessed bool) {
		newSource := transform.apply(target.Path)
		newMappings = append(newMappings, TopicMapping{Dest: dest, Source: newSource})
		resolved := resolvedMappingOf(dest, newSource, target)
		resolved.Guessed = guessed
		resolvedMappings = append(resolvedMappings, resolved)
		usedPaths[target.Path] = true
		if guessed {
			warnings = append(warnings, guessedWarning(dest, targetService.Name, target.Path))
			return
		}
		warnings = append(warnings, counterpartWarnings(dest, origin, target, bySemantics)...)
	}

	// Raised before any counterpart warning, because it is about the source the
	// derivation started from rather than about the variable it landed on: if this
	// reading of the original source is wrong, every path below it is wrong too.
	if w := envelopeWarning(first.Dest, firstTransform, first.Source, firstOrigin.Path); w != "" {
		warnings = append(warnings, w)
	}
	appendMapping(first.Dest, firstOrigin, firstTarget, firstTransform, firstBySemantics, firstGuessed)

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
		guessed := false
		if !ok {
			if serviceID == "" {
				return Resolved{}, fmt.Errorf(
					"%w: mapping %s (%s) has no counterpart on %s, so the topic would have to read a second service",
					ErrInvalidRequest, mapping.Dest, mapping.Source, targetService.Name)
			}
			def, defOk := defaultVariable(targetService, origin.Type, usedPaths)
			if !defOk {
				return Resolved{}, fmt.Errorf(
					"%w: mapping %s (%s) has no counterpart on %s, and %s has no queryable variable left to guess one from",
					ErrInvalidRequest, mapping.Dest, mapping.Source, targetService.Name, targetService.Name)
			}
			target = def
			guessed = true
		}
		appendMapping(mapping.Dest, origin, target, transform, sem, guessed)
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
		Services:     servicesOf(*to.DeviceType),
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

// defaultVariable is RetargetToService's fallback for a mapping that has no
// counterpart on the service the caller fixed: the first queryable variable
// that no other mapping of this same resolution has already claimed (used),
// preferring one whose Type matches the origin variable's. used may be nil,
// which reads as "nothing claimed yet" — the state mapping 0 starts from.
//
// This is a guess, not a derivation, so the caller marks whatever it returns
// Guessed and warns about it; defaultVariable itself only picks, it does not
// judge whether the pick is any good.
func defaultVariable(service models.Service, preferType models.Type, used map[string]bool) (profiler.Variable, bool) {
	var fallback profiler.Variable
	haveFallback := false
	for _, v := range profiler.ServiceVariables(service) {
		if !v.Queryable || used[v.Path] {
			continue
		}
		if v.Type == preferType {
			return v, true
		}
		if !haveFallback {
			fallback = v
			haveFallback = true
		}
	}
	return fallback, haveFallback
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

// servicesOf lists every service of a device type, in the order the device type
// itself declares them. Unsorted on purpose, unlike profiler.DeviceTypeVariables:
// this is what a developer picks a service from directly, not a derived
// ranking, so reordering it would just make the Select jump around from one
// resolve to the next for no reason the developer could see.
func servicesOf(dt models.DeviceType) []ResolvedService {
	out := make([]ResolvedService, 0, len(dt.Services))
	for _, s := range dt.Services {
		out = append(out, ResolvedService{ID: s.Id, Name: s.Name})
	}
	return out
}

// firstLandableService is the service a move to this device can actually use when
// nothing was derived, proven by running the move rather than by a property that
// stands in for it. Nil means no service on this device type can carry the topic,
// which is the honest answer and the one that keeps a row from promising a move it
// cannot make.
//
// The device type's own order decides, and the walk stops at the first service
// that works: the caller needs one service to hand back, not a ranking, and the
// developer's recourse if it is the wrong one is the card's service select. Each
// attempt is the same walk over the same two device types the fit already made —
// no series, no platform call — but it is a walk per service, so stopping early is
// worth the line it costs.
func firstLandableService(topic InputTopic, from, to models.ExtendedDevice) *ResolvedService {
	if to.DeviceType == nil {
		return nil
	}
	for _, s := range to.DeviceType.Services {
		if _, err := RetargetToService(topic, from, to, s.Id); err == nil {
			return &ResolvedService{ID: s.Id, Name: s.Name}
		}
	}
	return nil
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

// guessedWarning is RetargetToService's warning for a mapping defaultVariable
// had to pick for: it names the mapping, says nothing could be derived on the
// service, and says the variable was chosen by position rather than by reading
// anything about it — the three things counterpartWarnings would otherwise say
// do not apply, because this branch never found a match to compare against.
func guessedWarning(dest, serviceName, path string) string {
	return fmt.Sprintf(
		"mapping %s: nothing could be derived on %s, so %s was guessed by position among its queryable variables",
		dest, serviceName, path)
}
