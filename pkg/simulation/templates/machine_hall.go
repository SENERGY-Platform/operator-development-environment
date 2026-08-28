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
	"fmt"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
)

// MachineHall is a production hall: several machines drawing power on a shift
// pattern, and a hall meter that sums exactly them.
//
// It is the sub-metering case with something under it, which is the part that is
// hard to find in real inventory. A meter tree needs a meter and the things it
// meters to be modelled as such, and a platform whose devices were commissioned
// one at a time rarely records which meter sits above which machine — so an
// operator that divides a total between its contributors has nothing to be
// developed against until somebody builds this.
//
// The hall meter's channel is an aggregate, which is configurationless on
// purpose: it sums every asset whose submetered_by names this one, over the
// channel carrying the same characteristic. Adding a machine later, with
// add_simulated_asset, is therefore a change to the tree and not to the meter.
type MachineHall struct{}

func (MachineHall) Name() string { return "machine_hall" }

func (MachineHall) Summary() string {
	return "A production hall: N identical machines on a shift pattern, each sub-metered by one " +
		"hall meter whose channel is the sum over them. The sub-metering case with something " +
		"actually under it."
}

func (MachineHall) Notes() []string {
	return []string{
		"The shift pattern is per hour, so a machine is on through the shift and off outside " +
			"it. That is a cycle at the scale of a working day, not a duty cycle at the scale " +
			"of a part: nothing here switches on and off within an hour. For a minute-scale " +
			"cycle, point a machine's channel at a dataset with set_channel_source — a real " +
			"machine's own history replayed is a truer cycle than any profile.",
		"The hall meter does not backfill. An aggregate is derived — it follows from the " +
			"channels below it rather than being a series of its own — so a backfill writes " +
			"the machines and skips the meter, and it says so in its status. The total over a " +
			"backfilled window is a sum a consumer can take at any moment it likes.",
		"Every machine is the same device type and the same profile, differing only in the " +
			"seed's spread. A hall of machines that behave differently is several add_simulated_asset " +
			"calls or several set_channel_source calls on this one.",
	}
}

func (MachineHall) Assets() []AssetRole {
	return []AssetRole{
		{
			Name:     "machine",
			Kind:     simulation.AssetMachine,
			Purpose:  "One machine on the hall floor. The template builds `machines` of them from this one binding.",
			Required: true,
			Repeated: true,
			Channels: []ChannelRole{{
				Name:      "power",
				Purpose:   "What the machine draws. Bind a service whose value is a power.",
				Required:  true,
				Direction: simulation.Sensor,
			}},
		},
		{
			Name:     "hall_meter",
			Kind:     simulation.AssetMeter,
			Purpose:  "The meter above the machines. Its channel is the sum over them and carries no configuration of its own.",
			Required: true,
			Channels: []ChannelRole{{
				Name:      "power",
				Purpose:   "The hall total. It has to carry the same characteristic as the machines' power channel, because that is what an aggregate sums by.",
				Required:  true,
				Direction: simulation.Sensor,
			}},
		},
	}
}

func (MachineHall) Params() []Param {
	return []Param{
		{Name: "machines", Description: "How many machines the hall holds.", Default: 4, Min: 1, Max: 24, Integral: true},
		{Name: "machine_power", Description: "What one machine draws while it runs, in the unit of the bound power service.", Default: 4000, Min: 1, Max: 100000000},
		{Name: "idle_power", Description: "What one machine draws outside its shift. Zero is a machine that is switched off.", Default: 150, Min: 0, Max: 100000000},
		{Name: "shift_start_hour", Description: "Local hour the shift starts.", Default: 6, Min: 0, Max: 23, Integral: true},
		{Name: "shift_end_hour", Description: "Local hour it ends. A value below shift_start_hour is a night shift and wraps past midnight.", Default: 22, Min: 0, Max: 24, Integral: true},
		{Name: "weekend_factor", Description: "What fraction of the weekday load the hall carries on saturday and sunday. Zero is a hall that stands still.", Default: 0, Min: 0, Max: 1},
		{Name: "spread_percent", Description: "Random variation around the load, which is what keeps two machines from being the same series.", Default: 20, Min: 0, Max: 90},
		{Name: "interval_seconds", Description: "How often each channel publishes. This is what decides how many rows a backfill writes.", Default: 60, Min: 1, Max: 3600, Integral: true},
	}
}

func (t MachineHall) Render(in Input) (simulation.Environment, error) {
	name, err := checkName(in.Name)
	if err != nil {
		return simulation.Environment{}, err
	}
	values, err := params(in, t.Params())
	if err != nil {
		return simulation.Environment{}, err
	}

	machineBinding, machineChannels, err := bind(in, t.Assets()[0])
	if err != nil {
		return simulation.Environment{}, err
	}
	meterBinding, meterChannels, err := bind(in, t.Assets()[1])
	if err != nil {
		return simulation.Environment{}, err
	}

	machinePower, meterPower := machineChannels[0], meterChannels[0]
	if machinePower.characteristicID != meterPower.characteristicID {
		// Refused here rather than left to MOSES, which would accept it: an aggregate
		// sums the sub-metered channels carrying *its own* characteristic, so a meter
		// on a different one is a valid document whose meter reads zero forever. That
		// is the worst kind of wrong — it deploys, it publishes, and the number is a
		// lie about the hall.
		return simulation.Environment{}, errInvalid(
			"the hall meter's channel carries characteristic %s and the machines' carries %s. "+
				"An aggregate sums the sub-metered channels that carry its own characteristic, "+
				"so this meter would report zero for a hall that is running. Bind both to "+
				"services describing the same quantity",
			meterPower.characteristicID, machinePower.characteristicID)
	}

	count := int(values["machines"])
	interval := int64(values["interval_seconds"])
	shape := blockShape(int(values["shift_start_hour"]), int(values["shift_end_hour"]))
	week := weekdayShape(values["weekend_factor"])
	idle := values["idle_power"]
	running := values["machine_power"]

	const meterAssetID = "asset-hall-meter"

	assets := []simulation.Asset{{
		ID:             meterAssetID,
		Name:           name + " meter",
		Kind:           simulation.AssetMeter,
		ExternalTypeId: meterBinding.DeviceTypeID,
		InitialStates:  map[string]any{},
		Channels: []simulation.Channel{{
			ID:               "channel-hall-meter-power",
			Name:             "Hall power",
			Direction:        simulation.Sensor,
			ExternalRef:      meterPower.serviceID,
			CharacteristicId: meterPower.characteristicID,
			Unit:             meterPower.unit,
			IntervalSeconds:  interval,
			// No variant at all, which is not an omission: the meter tree below is the
			// configuration, so a machine added later is summed without touching this.
			Source: simulation.Source{Kind: simulation.SourceAggregate},
		}},
	}}

	for i := 1; i <= count; i++ {
		assets = append(assets, simulation.Asset{
			ID:             fmt.Sprintf("asset-machine-%d", i),
			Name:           fmt.Sprintf("%s machine %d", name, i),
			Kind:           simulation.AssetMachine,
			ExternalTypeId: machineBinding.DeviceTypeID,
			// What makes the hall meter mean anything. Without it every machine attaches
			// to its zone in the mirrored graph and the meter aggregates nothing.
			SubmeteredBy:  meterAssetID,
			InitialStates: map[string]any{},
			Channels: []simulation.Channel{{
				ID:               fmt.Sprintf("channel-machine-%d-power", i),
				Name:             "Power",
				Direction:        simulation.Sensor,
				ExternalRef:      machinePower.serviceID,
				CharacteristicId: machinePower.characteristicID,
				Unit:             machinePower.unit,
				IntervalSeconds:  interval,
				Source: simulation.Source{
					Kind: simulation.SourceProfile,
					Profile: &simulation.ProfileSource{
						// Base one with the factors carrying the load, for the reason the PV
						// temperature does it: a hall configured to draw nothing outside its
						// shift is an ordinary configuration, and the multiplicative encoding
						// would answer it with a base of zero and a machine that draws nothing
						// ever. The standby draw is exactly what a submetering operator is
						// meant to find, so it must not be the thing the encoding loses.
						Base:           1,
						HourFactors:    absoluteShape(shape, idle, running),
						WeekdayFactors: week,
						SpreadPercent:  values["spread_percent"],
					},
				},
			}},
		})
	}

	return simulation.Environment{
		Name:    name,
		Type:    simulation.IndustrialSite,
		Seed:    in.Seed,
		Context: map[string]any{},
		Zones: []simulation.Zone{{
			ID:            "zone-site",
			Name:          name,
			Type:          simulation.ZoneSite,
			Tags:          []string{"production"},
			InitialStates: map[string]any{},
			Assets:        []simulation.Asset{},
			Zones: []simulation.Zone{{
				ID:            "zone-hall",
				Name:          name + " hall",
				Type:          simulation.ZoneHall,
				Tags:          []string{"production"},
				InitialStates: map[string]any{},
				Zones:         []simulation.Zone{},
				Assets:        assets,
			}},
		}},
	}, nil
}
