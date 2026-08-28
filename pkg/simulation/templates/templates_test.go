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

package templates_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation/templates"
)

// The catalogue a template is bound against. Two device types, each with a
// service whose characteristic is named — because the characteristic is the one
// thing a template copies and never invents.
func catalogue() []simulation.DeviceType {
	return []simulation.DeviceType{
		{
			ID:   "type-inverter",
			Name: "Simulated inverter",
			Services: []simulation.DeviceTypeService{
				{ID: "service-power", Name: "Power", Direction: simulation.Sensor, CharacteristicId: "characteristic-watt", ValuePath: "value"},
				{ID: "service-energy", Name: "Energy", Direction: simulation.Sensor, CharacteristicId: "characteristic-kwh", ValuePath: "value"},
				{ID: "service-setpoint", Name: "Set limit", Direction: simulation.Actuator, CharacteristicId: "characteristic-watt", ValuePath: "value"},
				{ID: "service-nameless", Name: "Raw", Direction: simulation.Sensor, ValuePath: "value"},
			},
		},
		{
			ID:   "type-sensor",
			Name: "Simulated sensor",
			Services: []simulation.DeviceTypeService{
				{ID: "service-temperature", Name: "Temperature", Direction: simulation.Sensor, CharacteristicId: "characteristic-celsius", ValuePath: "value"},
				{ID: "service-other-power", Name: "Power", Direction: simulation.Sensor, CharacteristicId: "characteristic-kw", ValuePath: "value"},
			},
		},
	}
}

func pvInput() templates.Input {
	return templates.Input{
		Name:      "Roof array",
		Seed:      42,
		Catalogue: catalogue(),
		Bindings: map[string]templates.Binding{
			"inverter": {DeviceTypeID: "type-inverter", Channels: map[string]string{
				"power":  "service-power",
				"energy": "service-energy",
			}},
		},
	}
}

func hallInput() templates.Input {
	return templates.Input{
		Name:      "Werk 2",
		Seed:      7,
		Catalogue: catalogue(),
		Bindings: map[string]templates.Binding{
			"machine":    {DeviceTypeID: "type-inverter", Channels: map[string]string{"power": "service-power"}},
			"hall_meter": {DeviceTypeID: "type-inverter", Channels: map[string]string{"power": "service-power"}},
		},
	}
}

// The one property every rendered document has to have, because it is the one
// MOSES refuses at exactly the wrong length and the one a hand-written document
// loses in a rebase.
func TestEveryProfileCarriesTwentyFourHoursAndSevenWeekdays(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		input templates.Input
	}{
		{"pv_site", pvInput()},
		{"machine_hall", hallInput()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			template, found := templates.Lookup(testCase.name)
			if !found {
				t.Fatalf("no template %q", testCase.name)
			}
			environment, err := template.Render(testCase.input)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			checked := 0
			environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
				for _, channel := range asset.Channels {
					if channel.Source.Profile == nil {
						continue
					}
					checked++
					if got := len(channel.Source.Profile.HourFactors); got != 24 {
						t.Errorf("%s/%s has %d hour factors, want 24", asset.Name, channel.Name, got)
					}
					if got := len(channel.Source.Profile.WeekdayFactors); got != 7 {
						t.Errorf("%s/%s has %d weekday factors, want 7", asset.Name, channel.Name, got)
					}
				}
			})
			for key, source := range environment.ContextSources {
				if source.Profile == nil {
					continue
				}
				checked++
				if got := len(source.Profile.HourFactors); got != 24 {
					t.Errorf("context source %q has %d hour factors, want 24", key, got)
				}
			}
			if checked == 0 {
				t.Fatal("no profile was checked, so this test would pass on an empty render")
			}
		})
	}
}

// The characteristic decides the unit for everything downstream, so it is copied
// from the bound service and never invented.
func TestTheCharacteristicComesFromTheBoundService(t *testing.T) {
	environment, err := templates.PVSite{}.Render(pvInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	found := map[string]string{}
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		for _, channel := range asset.Channels {
			found[channel.ExternalRef] = channel.CharacteristicId
		}
	})
	if found["service-power"] != "characteristic-watt" {
		t.Errorf("the power channel carries %q, want the service's own characteristic",
			found["service-power"])
	}
	if found["service-energy"] != "characteristic-kwh" {
		t.Errorf("the energy channel carries %q, want the service's own characteristic",
			found["service-energy"])
	}
}

// The failure this template exists to prevent: a meter that deploys, publishes,
// and reports zero forever because it aggregates by a characteristic none of the
// machines below it carries.
func TestAHallMeterOnADifferentCharacteristicIsRefused(t *testing.T) {
	input := hallInput()
	input.Bindings["hall_meter"] = templates.Binding{
		DeviceTypeID: "type-sensor",
		Channels:     map[string]string{"power": "service-other-power"},
	}
	_, err := templates.MachineHall{}.Render(input)
	if err == nil {
		t.Fatal("a hall meter on a different characteristic rendered; it would report zero forever")
	}
	if !strings.Contains(err.Error(), "characteristic") {
		t.Errorf("the refusal is %q and does not say what is wrong", err)
	}
}

// The meter tree is the whole configuration of an aggregate, so every machine has
// to point at the meter and the meter has to carry no variant at all.
func TestTheHallMeterAggregatesEveryMachineUnderIt(t *testing.T) {
	input := hallInput()
	input.Params = map[string]float64{"machines": 3}
	environment, err := templates.MachineHall{}.Render(input)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var meterID string
	submetered := 0
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.Kind == simulation.AssetMeter {
			meterID = asset.ID
			for _, channel := range asset.Channels {
				if channel.Source.Kind != simulation.SourceAggregate {
					t.Errorf("the hall meter's channel is a %q, want an aggregate", channel.Source.Kind)
				}
				if channel.Source.Profile != nil || channel.Source.Dataset != nil ||
					channel.Source.Formula != nil || channel.Source.Script != nil {
					t.Error("the aggregate carries a variant; it is configurationless by design")
				}
			}
		}
	})
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.Kind != simulation.AssetMachine {
			return
		}
		if asset.SubmeteredBy != meterID {
			t.Errorf("%s is sub-metered by %q, want the hall meter %q — without it the meter "+
				"aggregates nothing", asset.Name, asset.SubmeteredBy, meterID)
		}
		submetered++
	})
	if submetered != 3 {
		t.Errorf("%d machines are sub-metered, want the 3 the parameter asked for", submetered)
	}
}

// Every id a template cross-references has to exist before the write: MOSES
// assigns ids for a document that omits them, but validation checks
// submetered_by against the ids in the document it was given.
func TestEveryCrossReferencedIdIsSetByTheTemplate(t *testing.T) {
	environment, err := templates.MachineHall{}.Render(hallInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	known := map[string]bool{}
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.ID == "" {
			t.Errorf("asset %q has no id", asset.Name)
		}
		known[asset.ID] = true
	})
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.SubmeteredBy != "" && !known[asset.SubmeteredBy] {
			t.Errorf("%s is sub-metered by %q, which is not an asset of this document",
				asset.Name, asset.SubmeteredBy)
		}
	})
}

// A cumulative profile accumulates base × factor per tick, so handing the day's
// total in as the base makes a meter count up by roughly the area under the
// curve times what was asked for. It looks like a working meter, which is what
// makes it worth pinning.
func TestTheEnergyMeterGainsWhatTheParameterAsksForOverADay(t *testing.T) {
	input := pvInput()
	input.Params = map[string]float64{"daily_energy": 60}
	environment, err := templates.PVSite{}.Render(input)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var energy *simulation.ProfileSource
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		for i, channel := range asset.Channels {
			if channel.ExternalRef == "service-energy" {
				energy = asset.Channels[i].Source.Profile
			}
		}
	})
	if energy == nil {
		t.Fatal("the energy channel did not render")
	}
	if !energy.Cumulative {
		t.Fatal("the energy channel is not cumulative, so it is not a meter reading")
	}
	gained := 0.0
	for _, factor := range energy.HourFactors {
		gained += energy.Base * factor
	}
	if gained < 59.9 || gained > 60.1 {
		t.Errorf("a full day gains %.2f, want the 60 the parameter asked for", gained)
	}
}

// A machine that is off outside its shift still draws standby, and that standby
// is the thing a submetering operator is meant to find. A scaled shape would put
// it at exactly zero.
func TestAMachineOutsideItsShiftStillDrawsItsIdlePower(t *testing.T) {
	input := hallInput()
	input.Params = map[string]float64{
		"machine_power": 4000, "idle_power": 150,
		"shift_start_hour": 6, "shift_end_hour": 22,
	}
	environment, err := templates.MachineHall{}.Render(input)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var profile *simulation.ProfileSource
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.Kind == simulation.AssetMachine && profile == nil {
			profile = asset.Channels[0].Source.Profile
		}
	})
	if profile == nil {
		t.Fatal("no machine rendered")
	}
	night := profile.Base * profile.HourFactors[3]
	day := profile.Base * profile.HourFactors[12]
	if night < 149 || night > 151 {
		t.Errorf("a machine at 03:00 draws %.1f, want the idle 150", night)
	}
	if day < 3999 || day > 4001 {
		t.Errorf("a machine at 12:00 draws %.1f, want the running 4000", day)
	}
}

// A night shift wraps past midnight, which is the ordinary case for an
// industrial site and would otherwise cost the caller two environments for one
// hall.
func TestANightShiftWrapsPastMidnight(t *testing.T) {
	input := hallInput()
	input.Params = map[string]float64{"shift_start_hour": 22, "shift_end_hour": 6, "idle_power": 100, "machine_power": 1000}
	environment, err := templates.MachineHall{}.Render(input)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var profile *simulation.ProfileSource
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.Kind == simulation.AssetMachine && profile == nil {
			profile = asset.Channels[0].Source.Profile
		}
	})
	if got := profile.Base * profile.HourFactors[23]; got < 999 {
		t.Errorf("at 23:00 the machine draws %.0f, want the running load", got)
	}
	if got := profile.Base * profile.HourFactors[12]; got > 101 {
		t.Errorf("at midday the machine draws %.0f, want the idle load", got)
	}
}

func TestABindingIsCheckedAgainstTheCatalogue(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		mutate  func(in *templates.Input)
		wantsay string
	}{
		{
			name: "a device type this MOSES cannot simulate",
			mutate: func(in *templates.Input) {
				in.Bindings["inverter"] = templates.Binding{DeviceTypeID: "type-invented", Channels: map[string]string{"power": "service-power"}}
			},
			wantsay: "list_simulation_device_types",
		},
		{
			name: "a service of another device type",
			mutate: func(in *templates.Input) {
				in.Bindings["inverter"] = templates.Binding{DeviceTypeID: "type-inverter", Channels: map[string]string{"power": "service-temperature"}}
			},
			wantsay: "is not part of device type",
		},
		{
			name: "an actuator where a measurement is needed",
			mutate: func(in *templates.Input) {
				in.Bindings["inverter"] = templates.Binding{DeviceTypeID: "type-inverter", Channels: map[string]string{"power": "service-setpoint"}}
			},
			wantsay: "has to be a sensor",
		},
		{
			name: "a service with no characteristic",
			mutate: func(in *templates.Input) {
				in.Bindings["inverter"] = templates.Binding{DeviceTypeID: "type-inverter", Channels: map[string]string{"power": "service-nameless"}}
			},
			wantsay: "no characteristic",
		},
		{
			name:    "no binding at all",
			mutate:  func(in *templates.Input) { in.Bindings = map[string]templates.Binding{} },
			wantsay: "needs a device type",
		},
		{
			name:    "no name",
			mutate:  func(in *templates.Input) { in.Name = "  " },
			wantsay: "needs a name",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := pvInput()
			testCase.mutate(&input)
			_, err := templates.PVSite{}.Render(input)
			if !errors.Is(err, templates.ErrInvalidInput) {
				t.Fatalf("Render = %v, want ErrInvalidInput", err)
			}
			if !strings.Contains(err.Error(), testCase.wantsay) {
				t.Errorf("the refusal is %q and does not say %q", err, testCase.wantsay)
			}
		})
	}
}

// A parameter the template does not have is a caller that believes it configured
// something. Rendering the default silently is how a scenario comes out wrong
// with nothing to point at.
func TestAnUnknownParameterIsNamedRatherThanIgnored(t *testing.T) {
	input := pvInput()
	input.Params = map[string]float64{"panel_tilt": 30}
	_, err := templates.PVSite{}.Render(input)
	if !errors.Is(err, templates.ErrInvalidInput) {
		t.Fatalf("Render = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "panel_tilt") {
		t.Errorf("the refusal is %q and does not name the parameter", err)
	}
}

func TestAParameterOutsideItsRangeIsRefused(t *testing.T) {
	input := hallInput()
	input.Params = map[string]float64{"machines": 500}
	_, err := templates.MachineHall{}.Render(input)
	if !errors.Is(err, templates.ErrInvalidInput) {
		t.Fatalf("Render = %v, want ErrInvalidInput", err)
	}
	input.Params = map[string]float64{"machines": 2.5}
	_, err = templates.MachineHall{}.Render(input)
	if !errors.Is(err, templates.ErrInvalidInput) {
		t.Fatalf("Render with a fractional count = %v, want ErrInvalidInput", err)
	}
}

// An optional role left unbound produces no asset, rather than an asset bound to
// nothing.
func TestAnOptionalRoleLeftUnboundProducesNothing(t *testing.T) {
	environment, err := templates.PVSite{}.Render(pvInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.Kind == simulation.AssetSensor {
			t.Errorf("the weather asset rendered although nothing was bound to it")
		}
		if asset.ExternalTypeId == "" {
			t.Errorf("%s names no device type, so MOSES would provision nothing for it", asset.Name)
		}
	})
}

func TestEveryTemplateIsFoundByNameAndSaysWhatItDoesNotSimulate(t *testing.T) {
	for _, name := range templates.Names() {
		template, found := templates.Lookup(name)
		if !found {
			t.Fatalf("Names() reports %q and Lookup does not find it", name)
		}
		if strings.TrimSpace(template.Summary()) == "" {
			t.Errorf("%s has no summary", name)
		}
		if len(template.Notes()) == 0 {
			t.Errorf("%s carries no notes; what a scenario does *not* simulate is what a "+
				"developer has to know before trusting data from it", name)
		}
	}
}

// The configuration a multiplicative profile cannot express, and the reason the
// factors carry the values rather than multipliers. Both of these are ordinary
// things to ask for, and both used to come out as a flat zero series.
func TestARangeThatTouchesZeroSurvivesTheEncoding(t *testing.T) {
	t.Run("a hall that draws nothing outside its shift", func(t *testing.T) {
		input := hallInput()
		input.Params = map[string]float64{
			"idle_power": 0, "machine_power": 4000,
			"shift_start_hour": 6, "shift_end_hour": 22,
		}
		environment, err := templates.MachineHall{}.Render(input)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		var profile *simulation.ProfileSource
		environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
			if asset.Kind == simulation.AssetMachine && profile == nil {
				profile = asset.Channels[0].Source.Profile
			}
		})
		if got := profile.Base * profile.HourFactors[12]; got < 3999 {
			t.Errorf("a machine at midday draws %.1f, want 4000 — an idle of zero must not "+
				"take the whole series down with it", got)
		}
		if got := profile.Base * profile.HourFactors[3]; got != 0 {
			t.Errorf("a machine at 03:00 draws %.1f, want the zero that was asked for", got)
		}
	})

	t.Run("a PV site whose nights are at freezing", func(t *testing.T) {
		input := pvInput()
		input.Bindings["weather"] = templates.Binding{
			DeviceTypeID: "type-sensor",
			Channels:     map[string]string{"temperature": "service-temperature"},
		}
		input.Params = map[string]float64{"night_temperature": 0, "ambient_temperature": 14}
		environment, err := templates.PVSite{}.Render(input)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		var profile *simulation.ProfileSource
		environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
			if asset.Kind == simulation.AssetSensor {
				profile = asset.Channels[0].Source.Profile
			}
		})
		if profile == nil {
			t.Fatal("the weather asset did not render")
		}
		peak := 0.0
		for _, factor := range profile.HourFactors {
			if value := profile.Base * factor; value > peak {
				peak = value
			}
		}
		if peak < 13 || peak > 15 {
			t.Errorf("the warmest hour is %.1f °C, want about 14 — a night at exactly 0 °C "+
				"must not flatten the whole day", peak)
		}
	})

	t.Run("a site whose nights are below freezing", func(t *testing.T) {
		input := pvInput()
		input.Bindings["weather"] = templates.Binding{
			DeviceTypeID: "type-sensor",
			Channels:     map[string]string{"temperature": "service-temperature"},
		}
		input.Params = map[string]float64{"night_temperature": -7, "ambient_temperature": 2}
		environment, err := templates.PVSite{}.Render(input)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		var profile *simulation.ProfileSource
		environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
			if asset.Kind == simulation.AssetSensor {
				profile = asset.Channels[0].Source.Profile
			}
		})
		if got := profile.Base * profile.HourFactors[2]; got < -7.5 || got > -6.5 {
			t.Errorf("the night is %.1f °C, want -7", got)
		}
	})
}
