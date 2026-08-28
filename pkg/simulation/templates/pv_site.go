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

package templates

import (
	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
)

// PVSite is a photovoltaic site: one inverter on a daylight profile, an
// irradiance context source following the same sun, and an optional ambient
// temperature.
//
// It is the scenario a generation forecast is developed against, which is why the
// energy channel is here beside the power one rather than left out as redundant.
// A forecast is trained on power and evaluated against energy, and a meter
// reading is not a series a consumer can derive from an instantaneous value
// without knowing what happened between two samples.
type PVSite struct{}

func (PVSite) Name() string { return "pv_site" }

func (PVSite) Summary() string {
	return "A PV site: one inverter publishing power on a daylight curve and, optionally, a " +
		"cumulative energy meter and an ambient temperature, under an irradiance context that " +
		"follows the same sun."
}

func (PVSite) Notes() []string {
	return []string{
		"The day curve is a half sine between sunrise and sunset with a random spread, not a " +
			"weather model: it has no cloud cover, no seasons and no horizon. What it is good " +
			"for is a periodic generation series with a known shape — a forecast trained on it " +
			"is being tested against the shape, not against the weather.",
		"The power channel and the energy channel are two profiles over the same day, not one " +
			"integrated into the other. The energy meter counts up from zero at simulation " +
			"start; it is a plausible meter reading, not the exact integral of the power series.",
		"Both channels backfill, because a profile is a pure function of the clock. Whether the " +
			"rows land at the timestamps they are given is decided by the device type, not here " +
			"— see the backfill note on the tool.",
	}
}

func (PVSite) Assets() []AssetRole {
	return []AssetRole{
		{
			Name:     "inverter",
			Kind:     simulation.AssetInverter,
			Purpose:  "The inverter of the site, which is what publishes the generation.",
			Required: true,
			Channels: []ChannelRole{
				{
					Name:      "power",
					Purpose:   "Instantaneous generation. Bind a service whose value is a power — the characteristic decides the unit and is copied from the service.",
					Required:  true,
					Direction: simulation.Sensor,
				},
				{
					Name:       "energy",
					Purpose:    "The generation meter reading, counting up. Bind a service whose value is an energy.",
					Required:   false,
					Direction:  simulation.Sensor,
					Cumulative: true,
				},
			},
		},
		{
			Name:     "weather",
			Kind:     simulation.AssetSensor,
			Purpose:  "An ambient sensor at the site. Optional, and worth binding when the operator is meant to see generation and temperature together.",
			Required: false,
			Channels: []ChannelRole{
				{
					Name:      "temperature",
					Purpose:   "Outside air temperature at the site.",
					Required:  true,
					Direction: simulation.Sensor,
				},
			},
		},
	}
}

func (PVSite) Params() []Param {
	return []Param{
		{Name: "peak_power", Description: "The generation at midday, in the unit of the bound power service.", Default: 10000, Min: 1, Max: 100000000},
		{Name: "daily_energy", Description: "How much the energy meter gains on a full day, in the unit of the bound energy service.", Default: 60, Min: 0, Max: 100000000},
		{Name: "sunrise_hour", Description: "Local hour the generation starts.", Default: 6, Min: 0, Max: 23},
		{Name: "sunset_hour", Description: "Local hour it ends. Must be after sunrise_hour: a PV day does not wrap past midnight.", Default: 20, Min: 1, Max: 24},
		{Name: "spread_percent", Description: "Random variation around the curve, which is what stands in for weather.", Default: 12, Min: 0, Max: 90},
		{Name: "interval_seconds", Description: "How often each channel publishes. This is what decides how many rows a backfill writes.", Default: 60, Min: 1, Max: 3600, Integral: true},
		{Name: "ambient_temperature", Description: "Midday air temperature, in the unit of the bound temperature service.", Default: 18, Min: -60, Max: 60},
		{Name: "night_temperature", Description: "Air temperature before sunrise and after sunset.", Default: 8, Min: -60, Max: 60},
	}
}

func (t PVSite) Render(in Input) (simulation.Environment, error) {
	name, err := checkName(in.Name)
	if err != nil {
		return simulation.Environment{}, err
	}
	values, err := params(in, t.Params())
	if err != nil {
		return simulation.Environment{}, err
	}
	sunrise, sunset := values["sunrise_hour"], values["sunset_hour"]
	if sunset <= sunrise {
		return simulation.Environment{}, errInvalid(
			"sunset_hour is %g and sunrise_hour is %g; a PV day does not wrap past midnight",
			sunset, sunrise)
	}
	interval := int64(values["interval_seconds"])
	spread := values["spread_percent"]
	shape := dayShape(sunrise, sunset)

	inverterBinding, inverterChannels, err := bind(in, t.Assets()[0])
	if err != nil {
		return simulation.Environment{}, err
	}
	weatherBinding, weatherChannels, err := bind(in, t.Assets()[1])
	if err != nil {
		return simulation.Environment{}, err
	}

	assets := []simulation.Asset{}

	channels := []simulation.Channel{}
	for _, channel := range inverterChannels {
		base := values["peak_power"]
		if channel.role.Cumulative {
			// A cumulative profile is a rate that is integrated, so the base is what the
			// meter gains per day spread over the hours the sun is up rather than the
			// day's total: MOSES accumulates base × hour factor × weekday factor at each
			// tick. Dividing by the area under the curve is what makes a full day come to
			// daily_energy rather than to some multiple of it.
			base = perDayRate(values["daily_energy"], shape)
		}
		channels = append(channels, simulation.Channel{
			ID:               "channel-inverter-" + channel.role.Name,
			Name:             titleOf(channel.role.Name),
			Direction:        simulation.Sensor,
			ExternalRef:      channel.serviceID,
			CharacteristicId: channel.characteristicID,
			Unit:             channel.unit,
			IntervalSeconds:  interval,
			Source: simulation.Source{
				Kind: simulation.SourceProfile,
				Profile: &simulation.ProfileSource{
					Base:           base,
					HourFactors:    shape,
					WeekdayFactors: flatWeek(),
					SpreadPercent:  spread,
					Cumulative:     channel.role.Cumulative,
				},
			},
		})
	}
	assets = append(assets, simulation.Asset{
		ID:             "asset-inverter",
		Name:           name + " inverter",
		Kind:           simulation.AssetInverter,
		ExternalTypeId: inverterBinding.DeviceTypeID,
		InitialStates:  map[string]any{},
		Channels:       channels,
	})

	if len(weatherChannels) > 0 {
		night := values["night_temperature"]
		day := values["ambient_temperature"]
		assets = append(assets, simulation.Asset{
			ID:             "asset-weather",
			Name:           name + " ambient",
			Kind:           simulation.AssetSensor,
			ExternalTypeId: weatherBinding.DeviceTypeID,
			InitialStates:  map[string]any{},
			Channels: []simulation.Channel{{
				ID:               "channel-weather-temperature",
				Name:             "Temperature",
				Direction:        simulation.Sensor,
				ExternalRef:      weatherChannels[0].serviceID,
				CharacteristicId: weatherChannels[0].characteristicID,
				Unit:             weatherChannels[0].unit,
				IntervalSeconds:  interval,
				Source: simulation.Source{
					Kind: simulation.SourceProfile,
					Profile: &simulation.ProfileSource{
						// Base one with the factors carrying the degrees. A temperature range
						// touches and crosses zero — a night at exactly 0 °C is an ordinary
						// configuration — and the multiplicative encoding cannot express that
						// without making every value zero along with the base. See absoluteShape.
						Base:           1,
						HourFactors:    absoluteShape(shape, night, day),
						WeekdayFactors: flatWeek(),
						SpreadPercent:  spread / 4,
					},
				},
			}},
		})
	}

	return simulation.Environment{
		Name: name,
		Type: simulation.IndustrialSite,
		Seed: in.Seed,
		Context: map[string]any{
			"irradiance": 0.0,
		},
		ContextSources: map[string]simulation.Source{
			// The context every zone below reads, on the same day shape as the inverter.
			// Without a source the key keeps its initial value and the context looks inert,
			// which is the failure MOSES's own documentation warns about.
			"irradiance": {
				Kind:            simulation.SourceProfile,
				IntervalSeconds: interval,
				Profile: &simulation.ProfileSource{
					Base:           1000,
					HourFactors:    shape,
					WeekdayFactors: flatWeek(),
					SpreadPercent:  spread,
				},
			},
		},
		Zones: []simulation.Zone{{
			ID:            "zone-site",
			Name:          name,
			Type:          simulation.ZoneSite,
			Tags:          []string{"pv"},
			InitialStates: map[string]any{},
			Zones:         []simulation.Zone{},
			Assets:        assets,
		}},
	}, nil
}
