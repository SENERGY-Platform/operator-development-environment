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

// Package templates renders a simulated environment from a named scenario and a
// handful of decisions, instead of from a document somebody wrote by hand.
//
// It exists because of what the alternative costs. An environment is three
// hundred lines of nested JSON, validated by a service the author cannot see,
// with three fields the author must not set and several that are refused at
// exactly the wrong length — a profile needs 24 hour factors or none, an actuator
// must not carry an interval, an asset may not be sub-metered by itself. Letting
// a model write that document makes every one of those a 400 it discovers by
// retrying. A template makes the failure mode "the template lacks a knob", which
// is a bug report rather than a guessing game.
//
// **The catalogue is not part of the template.** Which device types exist, and
// which characteristic a service carries, is a property of the platform this ODE
// talks to — so a template declares the *roles* it needs filled and the caller
// binds each to a device type and a service it actually found. The characteristic
// and the unit are then copied from that service and never invented, which is the
// same rule the rest of ODE follows for every characteristic it reports.
//
// Templates live here rather than being read from MOSES because MOSES's own
// template endpoints belong to the legacy world/room/device model that the
// environment api replaced.
package templates

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
)

// ChannelRole is one channel a template needs bound to a platform service.
type ChannelRole struct {
	Name string `json:"name"`
	// Purpose is what this channel is for, written for whoever has to pick the
	// service that fills it.
	Purpose string `json:"purpose"`
	// Required says the template cannot render without it. An optional role left
	// unbound simply produces no channel.
	Required bool `json:"required"`
	// Direction is what kind of service fills it. Every role of both templates here
	// is a sensor: a simulation of a site produces measurements.
	Direction simulation.Direction `json:"direction"`
	// Cumulative marks a role that is a meter reading rather than an instantaneous
	// value, because the two need different sources from the same profile.
	Cumulative bool `json:"cumulative,omitempty"`
}

// AssetRole is one simulated device a template needs bound to a device type.
//
// Channels are grouped under an asset rather than listed flat because MOSES binds
// an asset to exactly one platform device: every channel of an asset has to be a
// service of the *same* device type, and a flat list would let a caller bind two
// roles to services of two different types and find out from a 400.
type AssetRole struct {
	Name string               `json:"name"`
	Kind simulation.AssetKind `json:"kind"`
	// Purpose is what this asset stands for in the scenario.
	Purpose string `json:"purpose"`
	// Required says the template cannot render without it.
	Required bool `json:"required"`
	// Repeated says the scenario builds several of these — how many is a parameter
	// — from one binding. All of them are the same device type, which is what
	// "twelve identical machines" means.
	Repeated bool          `json:"repeated,omitempty"`
	Channels []ChannelRole `json:"channels"`
}

// Param is one number a caller decides.
//
// Every parameter of every template here is numeric, and deliberately: a string
// parameter would end up naming something on the platform, and anything named on
// the platform belongs in a binding where it can be checked against the catalogue.
type Param struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Default     float64 `json:"default"`
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	Unit        string  `json:"unit,omitempty"`
	// Integral says a fractional value is a mistake rather than a rounding.
	Integral bool `json:"integral,omitempty"`
}

// Binding is what a caller found in the catalogue for one asset role.
type Binding struct {
	// DeviceTypeID is the device type the asset is built from. Every channel below
	// has to be a service of it.
	DeviceTypeID string `json:"device_type_id"`
	// Channels maps a channel role's name to the id of the service that fills it.
	Channels map[string]string `json:"channels"`
}

// Input is one render.
type Input struct {
	// Name is what the developer will find the environment under.
	Name string
	// Seed makes the environment reproducible. Zero is not "random": it is what
	// MOSES stores, and a caller that wants a different scenario from the same
	// template changes it deliberately.
	Seed int64
	// Bindings is one per asset role, keyed by the role's name.
	Bindings map[string]Binding
	// Params overrides the defaults, keyed by the parameter's name.
	Params map[string]float64
	// Catalogue is the device types the platform has, as
	// list_simulation_device_types reported them. The characteristic and the unit
	// of every channel are copied out of it, so a render against an empty
	// catalogue fails rather than inventing one.
	Catalogue []simulation.DeviceType
}

// Template is a scenario that renders to an environment.
type Template interface {
	Name() string
	// Summary is one sentence: what this scenario is for.
	Summary() string
	// Notes are what a caller should know before rendering — what the scenario
	// does not simulate, and what will not backfill.
	Notes() []string
	Assets() []AssetRole
	Params() []Param
	Render(in Input) (simulation.Environment, error)
}

// All is every template, in a stable order.
func All() []Template { return []Template{PVSite{}, MachineHall{}} }

// Lookup finds a template by name.
func Lookup(name string) (Template, bool) {
	for _, template := range All() {
		if template.Name() == name {
			return template, true
		}
	}
	return nil, false
}

// Names is every template name, for a refusal that lists the alternatives.
func Names() []string {
	out := []string{}
	for _, template := range All() {
		out = append(out, template.Name())
	}
	sort.Strings(out)
	return out
}

// ErrInvalidInput marks a render refused because of what the caller asked for.
// It is wrapped so the tool surface reports it as the model's mistake rather than
// as a failure of the simulator, which has not been called yet at that point.
var ErrInvalidInput = simulation.ErrInvalidRequest

// ---- the shared render machinery ----

// resolved is one bound channel: the service it publishes to, with the
// characteristic and unit copied out of the catalogue.
type resolved struct {
	role      ChannelRole
	serviceID string
	// characteristicID and unit come from the device type's service, never from the
	// caller and never from this package. An invented characteristic authorises a
	// wrong unit conversion everywhere downstream.
	characteristicID string
	unit             string
}

// bind checks one asset role against the catalogue and resolves its channels.
func bind(in Input, role AssetRole) (Binding, []resolved, error) {
	binding, given := in.Bindings[role.Name]
	if !given || strings.TrimSpace(binding.DeviceTypeID) == "" {
		if role.Required {
			return Binding{}, nil, fmt.Errorf(
				"%w: the asset role %q needs a device type; list_simulation_device_types reports what this platform has",
				ErrInvalidInput, role.Name)
		}
		return Binding{}, nil, nil
	}

	deviceType, found := deviceTypeOf(in.Catalogue, binding.DeviceTypeID)
	if !found {
		return Binding{}, nil, fmt.Errorf(
			"%w: device type %q is not one this MOSES can simulate. Only the types publishing "+
				"through its own protocol can be, which is what list_simulation_device_types lists",
			ErrInvalidInput, binding.DeviceTypeID)
	}

	out := []resolved{}
	for _, channelRole := range role.Channels {
		serviceID, bound := binding.Channels[channelRole.Name]
		if !bound || strings.TrimSpace(serviceID) == "" {
			if channelRole.Required {
				return Binding{}, nil, fmt.Errorf(
					"%w: %s.%s needs a service of device type %q. What it is for: %s",
					ErrInvalidInput, role.Name, channelRole.Name, deviceType.Name, channelRole.Purpose)
			}
			continue
		}
		service, serviceFound := serviceOf(deviceType, serviceID)
		if !serviceFound {
			return Binding{}, nil, fmt.Errorf(
				"%w: service %q is not part of device type %q (%s)",
				ErrInvalidInput, serviceID, deviceType.Name, deviceType.ID)
		}
		if service.Direction != channelRole.Direction {
			return Binding{}, nil, fmt.Errorf(
				"%w: %s.%s has to be a %s and %q is a %s",
				ErrInvalidInput, role.Name, channelRole.Name,
				channelRole.Direction, service.Name, service.Direction)
		}
		if strings.TrimSpace(service.CharacteristicId) == "" {
			// Refused rather than rendered with an empty characteristic. A channel without
			// one publishes a bare number, and everything downstream that reads it — the
			// profiler's unit, a chart's axis, a conversion — has nothing to go on.
			return Binding{}, nil, fmt.Errorf(
				"%w: service %q of device type %q carries no characteristic, so a channel on "+
					"it would publish a value with no unit. Pick a service whose value is described",
				ErrInvalidInput, service.Name, deviceType.Name)
		}
		out = append(out, resolved{
			role:             channelRole,
			serviceID:        service.ID,
			characteristicID: service.CharacteristicId,
			unit:             unitOf(service),
		})
	}
	return binding, out, nil
}

func deviceTypeOf(catalogue []simulation.DeviceType, id string) (simulation.DeviceType, bool) {
	for _, deviceType := range catalogue {
		if deviceType.ID == id {
			return deviceType, true
		}
	}
	return simulation.DeviceType{}, false
}

func serviceOf(deviceType simulation.DeviceType, id string) (simulation.DeviceTypeService, bool) {
	for _, service := range deviceType.Services {
		if service.ID == id {
			return service, true
		}
	}
	return simulation.DeviceTypeService{}, false
}

// unitOf is the denormalised unit MOSES stores beside the characteristic so a
// document reads without resolving it. MOSES's catalogue does not carry one, so
// it is left empty rather than guessed from the service name: a wrong unit beside
// a right characteristic is worse than no unit at all, because it is the one a
// human reads.
func unitOf(_ simulation.DeviceTypeService) string { return "" }

// param reads one parameter, defaulted and bounded.
func param(in Input, spec Param) (float64, error) {
	value, given := in.Params[spec.Name]
	if !given {
		return spec.Default, nil
	}
	if value < spec.Min || value > spec.Max {
		return 0, fmt.Errorf("%w: %s is %g and has to be between %g and %g",
			ErrInvalidInput, spec.Name, value, spec.Min, spec.Max)
	}
	if spec.Integral && value != float64(int64(value)) {
		return 0, fmt.Errorf("%w: %s counts things and cannot be %g", ErrInvalidInput, spec.Name, value)
	}
	return value, nil
}

// params reads every parameter of a template at once, so a render fails on the
// first bad one with the same message wherever it is called from.
func params(in Input, specs []Param) (map[string]float64, error) {
	out := map[string]float64{}
	known := map[string]bool{}
	for _, spec := range specs {
		value, err := param(in, spec)
		if err != nil {
			return nil, err
		}
		out[spec.Name] = value
		known[spec.Name] = true
	}
	for name := range in.Params {
		if !known[name] {
			// Named rather than ignored. A parameter the template does not have is a
			// caller that believes it configured something, and silently rendering the
			// default is how a scenario comes out wrong with nothing to point at.
			return nil, fmt.Errorf("%w: %q is not a parameter of this template; it takes %v",
				ErrInvalidInput, name, paramNames(specs))
		}
	}
	return out, nil
}

func paramNames(specs []Param) []string {
	out := []string{}
	for _, spec := range specs {
		out = append(out, spec.Name)
	}
	return out
}

// checkName refuses an unnamed environment here rather than at MOSES, because the
// name is the only thing the developer will find it by.
func checkName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: the environment needs a name, which is what the developer "+
			"will find it under in MOSES and in the device list", ErrInvalidInput)
	}
	return trimmed, nil
}

// dayShape is a 24-entry hour factor list rising and falling between two hours,
// peaking in the middle — a bell over the daylight window rather than a step.
//
// It is built rather than written out because MOSES refuses a list that is not
// exactly 24 long, and a hand-written literal is the kind of thing that loses an
// entry in a rebase and then produces a plausible-looking wrong day.
func dayShape(fromHour, toHour float64) []float64 {
	factors := make([]float64, 24)
	span := toHour - fromHour
	if span <= 0 {
		return factors
	}
	for hour := 0; hour < 24; hour++ {
		// The middle of the hour, so a symmetric window comes out symmetric rather
		// than leaning on whichever end the sampling happened to land in.
		at := float64(hour) + 0.5
		if at <= fromHour || at >= toHour {
			continue
		}
		position := (at - fromHour) / span
		// A half sine over the window: zero at both ends, one in the middle.
		factors[hour] = sinePeak(position)
	}
	return factors
}

// blockShape is a 24-entry hour factor list that is one inside a window and zero
// outside it: a shift rather than a daylight curve.
//
// A window that wraps past midnight is supported, because a night shift is the
// ordinary case for an industrial site and refusing it would push the caller into
// two environments for one hall.
func blockShape(fromHour, toHour int) []float64 {
	factors := make([]float64, 24)
	if fromHour == toHour {
		return factors
	}
	for hour := 0; hour < 24; hour++ {
		if fromHour < toHour {
			if hour >= fromHour && hour < toHour {
				factors[hour] = 1
			}
			continue
		}
		if hour >= fromHour || hour < toHour {
			factors[hour] = 1
		}
	}
	return factors
}

// weekdayShape is 7 entries starting at monday: working days at one, the weekend
// at whatever the scenario says.
func weekdayShape(weekend float64) []float64 {
	return []float64{1, 1, 1, 1, 1, weekend, weekend}
}

// flatWeek is 7 entries of one, for a scenario the calendar does not touch. The
// sun does not keep office hours.
func flatWeek() []float64 { return []float64{1, 1, 1, 1, 1, 1, 1} }

// errInvalid is a render refusal in the caller's own terms.
func errInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

// titleOf turns a role name into the channel name a developer reads in MOSES.
func titleOf(role string) string {
	if role == "" {
		return role
	}
	return strings.ToUpper(role[:1]) + strings.ReplaceAll(role[1:], "_", " ")
}

// perDayRate converts "this much per full day" into the base a cumulative
// profile needs.
//
// MOSES accumulates base × hour factor at each tick of an hour, so a full day
// gains base × (the sum of the hour factors). Handing the day's total in as the
// base would therefore gain that total once per unit of shape area — a meter that
// counts up roughly ten times too fast on a daylight curve, which looks like a
// working meter and is not.
func perDayRate(perDay float64, hourFactors []float64) float64 {
	area := 0.0
	for _, factor := range hourFactors {
		area += factor
	}
	if area <= 0 {
		return 0
	}
	return perDay / area
}

// absoluteShape turns a 0..1 shape into hour factors that *are* the values, to be
// used with a base of one.
//
// A profile is base × hour factor × weekday factor, so the obvious encoding —
// base at the floor and a factor above one at the peak — cannot express a range
// that touches or crosses zero. A night temperature of exactly 0 °C would make
// the base zero and every value with it, silently, and 0 °C is a temperature
// rather than an absence of one; a hall configured to draw nothing outside its
// shift has the same problem with the same silence. Both are ordinary
// configurations, so the encoding is the thing that has to give.
//
// Base one, factors carrying the real numbers, handles zero and negative values
// alike. The cost is that a factor list read in the MOSES UI is degrees or watts
// rather than multipliers, which is a legibility cost and not a correctness one.
func absoluteShape(shape []float64, low, high float64) []float64 {
	out := make([]float64, len(shape))
	for i, factor := range shape {
		out[i] = low + (high-low)*factor
	}
	return out
}
