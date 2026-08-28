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

// Package simulation is ODE's view of MOSES, the platform's environment
// simulator; the reasoning is in docs/simulation.md.
//
// It exists because an operator needs data before it can be developed, and the
// data an operator needs is rarely there on the day the work starts. MOSES
// simulates the site instead: a simulated asset is an ordinary platform device,
// so pkg/ontology, pkg/devices and the profiler already find one. What ODE could
// not do is bring one into existence, and that round trip out of ODE and back is
// what this package closes.
//
// **The types here are a mirror, not an import.** Depending on
// moses/lib/domain would tie ODE's build to a package documented as changing
// with its owner's migrations, for a handful of structs. The cost of mirroring
// is drift, and drift here is not cosmetic: every write is a whole-document PUT,
// so a field this mirror does not know would be deleted from somebody's
// environment by the act of editing it. That cost is paid down rather than
// accepted — Get decodes strictly and records what it did not recognise, and
// Replace refuses a document carrying an unknown field instead of writing it
// back short. See Environment.UnknownField and ErrUnknownField.
package simulation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// EnvironmentType is what kind of place is simulated. Mirrors
// moses/lib/domain.EnvironmentType.
type EnvironmentType string

const (
	IndustrialSite    EnvironmentType = "industrial_site"
	OfficeBuilding    EnvironmentType = "office_building"
	ApartmentBuilding EnvironmentType = "apartment_building"
	SingleFamilyHome  EnvironmentType = "single_family_home"
	Apartment         EnvironmentType = "apartment"
)

// EnvironmentTypes is every type MOSES accepts, for a schema and for a refusal
// that names the alternatives.
func EnvironmentTypes() []EnvironmentType {
	return []EnvironmentType{IndustrialSite, OfficeBuilding, ApartmentBuilding, SingleFamilyHome, Apartment}
}

// ZoneType is a level of the nesting. Site, building, floor, unit, hall and room
// are the same entity with a different type, which is why depth is data here
// rather than schema.
type ZoneType string

const (
	ZoneSite     ZoneType = "site"
	ZoneBuilding ZoneType = "building"
	ZoneFloor    ZoneType = "floor"
	ZoneUnit     ZoneType = "unit"
	ZoneHall     ZoneType = "hall"
	ZoneRoom     ZoneType = "room"
)

// AssetKind is what a simulated device is. Each asset binds one-to-one to a
// platform device.
type AssetKind string

const (
	AssetMeter    AssetKind = "meter"
	AssetInverter AssetKind = "inverter"
	AssetMachine  AssetKind = "machine"
	AssetSensor   AssetKind = "sensor"
	AssetActuator AssetKind = "actuator"
)

// Direction is whether a channel is measured or driven.
type Direction string

const (
	Sensor   Direction = "sensor"
	Actuator Direction = "actuator"
)

// Environment is one simulated site, building or apartment: the whole document,
// which is also the whole unit of writing.
//
// Owner is absent deliberately and not by omission: MOSES never serialises it and
// takes it from the caller's token, which is what makes every environment ODE
// creates belong to the developer who asked for it rather than to a service
// account nobody can find.
type Environment struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Type EnvironmentType `json:"type"`

	// Version is counted by MOSES. A write carries the version of the document it
	// was read from and is refused with 409 if the stored one has moved on, which
	// is what keeps two editors from overwriting each other — and, with them, from
	// deleting a device the winning document still publishes through.
	Version int64 `json:"version"`

	// ExternalGraphRef is the device-repository graph this environment is mirrored
	// as. MOSES decides it and overwrites whatever a client sends; ODE clears it on
	// the way out rather than echoing a value that, if it were ever honoured, would
	// let one environment overwrite another's graph.
	ExternalGraphRef string `json:"external_graph_ref"`

	// Seed is what every stochastic source derives from, so the same document and
	// the same clock produce the same values. It is why a backfill is reproducible
	// and therefore why a model can be retrained on one.
	Seed int64 `json:"seed"`

	// ContextSources drive context keys over time — outdoor temperature on a day
	// cycle, irradiance following the sun. Keyed by the context key written.
	// Without one a context key keeps its initial value and the context looks inert.
	ContextSources map[string]Source `json:"context_sources,omitempty"`

	// Context is the shared surroundings every zone below reads. Initial values only.
	Context map[string]any `json:"context"`

	Zones []Zone `json:"zones"`

	// UnknownField names a member MOSES sent that this mirror does not know.
	// Never serialised: it is ODE's own record of the drift, not part of the
	// document, and it is what makes Replace refuse rather than write the document
	// back with that field deleted. See Service.Get and ErrUnknownField.
	UnknownField string `json:"-"`
}

// Zone is a recursive node of the environment.
type Zone struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Type ZoneType `json:"type"`

	// Tags carry what the fixed type list does not, so a new kind of space needs no
	// new enum value.
	Tags []string `json:"tags"`

	// TimeConstants makes a state value follow a set point rather than jump to it,
	// in seconds per state key: the thermal inertia of a space. A key without one is
	// set at once.
	TimeConstants map[string]int64 `json:"time_constants,omitempty"`

	// InitialStates seeds the runtime state at start. Live values are not here —
	// they are read through GET .../state.
	InitialStates map[string]any `json:"initial_states"`

	Zones  []Zone  `json:"zones"`
	Assets []Asset `json:"assets"`
}

// Asset is a simulated device, bound one-to-one to a platform device.
type Asset struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Kind AssetKind `json:"kind"`

	// ExternalRef is the platform device this asset publishes through, and
	// ExternalTypeId the device type it was built from.
	//
	// Both are carried verbatim through a read-modify-write and are *not* stripped,
	// although the two external_* fields beside them are. Stripping ExternalRef
	// would be the opposite of safe: MOSES provisions a device for every asset that
	// names a type and carries no device, so a write with the reference removed
	// creates a second device for an asset that already had one, and the timeseries
	// of the first is orphaned. What is stripped is the pair MOSES reconciles from
	// the stored document and a client cannot influence — ExternalManaged and
	// ExternalGraphRef. See Environment.forWrite, and docs/simulation.md.
	ExternalRef    string `json:"external_ref"`
	ExternalTypeId string `json:"external_type_id"`

	// ExternalManaged tells a device MOSES created for this asset from one the user
	// picked and attached. Only a managed device is deleted again with the asset.
	// MOSES decides it on every write by comparing against the stored document, so
	// what ODE sends is discarded — which is exactly why ODE sends nothing.
	ExternalManaged bool `json:"external_managed"`

	// SubmeteredBy names, by asset id, the asset whose device also meters this one.
	// It is the whole configuration of an aggregate channel: the meter tree is the
	// configuration, so an asset added below is summed without editing the meter.
	SubmeteredBy string `json:"submetered_by,omitempty"`

	InitialStates map[string]any `json:"initial_states"`

	Channels []Channel `json:"channels"`
}

// Channel is one measuring point or manipulated variable of an asset, publishing
// to one platform service.
type Channel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Direction Direction `json:"direction"`

	// ExternalRef is the platform service id this channel publishes to.
	ExternalRef string `json:"external_ref"`

	// CharacteristicId decides the unit and is never invented: it comes from the
	// device type's content variable, which is what list_simulation_device_types
	// reports. Unit is denormalised beside it so a document reads without resolving
	// the characteristic.
	CharacteristicId string `json:"characteristic_id"`
	Unit             string `json:"unit"`

	// IntervalSeconds is how often a sensor channel emits. Zero means the channel is
	// only driven from outside, the normal case for an actuator — and MOSES refuses
	// an actuator that carries one.
	IntervalSeconds int64 `json:"interval_seconds"`

	Source Source `json:"source"`
}

// SourceKind is what drives a channel.
type SourceKind string

const (
	// SourceScript is user-supplied JavaScript, executed inside MOSES.
	SourceScript SourceKind = "script"
	// SourceProfile is a declarative load profile over day and week.
	SourceProfile SourceKind = "profile"
	// SourceDataset replays a real timeseries.
	SourceDataset SourceKind = "dataset"
	// SourceFormula derives a value from other channels and the context.
	SourceFormula SourceKind = "formula"
	// SourceAggregate sums everything sub-metered by this channel's asset. It has no
	// variant at all: the meter tree is the configuration.
	SourceAggregate SourceKind = "aggregate"
)

// Source is what drives a channel; exactly one variant matches Kind, with
// SourceAggregate the one kind that has no variant. MOSES refuses a kind whose
// variant is missing rather than storing a channel that produces nothing.
type Source struct {
	Kind SourceKind `json:"kind"`

	// IntervalSeconds is how often the source computes, which is not how often the
	// channel publishes. Zero computes when the channel publishes.
	IntervalSeconds int64 `json:"interval_seconds,omitempty"`

	Script  *ScriptSource  `json:"script,omitempty"`
	Profile *ProfileSource `json:"profile,omitempty"`
	Dataset *DatasetSource `json:"dataset,omitempty"`
	Formula *FormulaSource `json:"formula,omitempty"`
}

type ScriptSource struct {
	Code string `json:"code"`
}

// ProfileSource is a base value with per-hour and per-weekday factors, plus a
// spread drawn from the environment's seed so repeated runs match.
type ProfileSource struct {
	Base float64 `json:"base"`
	// HourFactors has 24 entries or none, WeekdayFactors 7 starting at monday or
	// none. MOSES refuses any other length, which is why the templates build them
	// rather than letting a caller pass a list of the wrong size.
	HourFactors    []float64 `json:"hour_factors"`
	WeekdayFactors []float64 `json:"weekday_factors"`
	// SpreadPercent is the random variation around the resulting value.
	SpreadPercent float64 `json:"spread_percent"`
	// Cumulative turns the profile into a meter reading that keeps counting up.
	Cumulative bool `json:"cumulative"`
}

// DatasetOrigin is where a replayed timeseries comes from.
type DatasetOrigin string

const (
	// OriginPlatform replays the timeseries of a real platform device. This is the
	// cheapest kind of "example data": the platform already has it, and no file
	// moves anywhere.
	OriginPlatform DatasetOrigin = "platform"
	// OriginFile replays an uploaded dataset by id.
	OriginFile DatasetOrigin = "file"
	// OriginEndpoint polls an allow-listed HTTP endpoint. The allow-list is MOSES's,
	// which is why ODE proposes one and does not decide it.
	OriginEndpoint DatasetOrigin = "endpoint"
)

// ResampleMode is how a replay fills the gaps between samples.
type ResampleMode string

const (
	// ResampleHold keeps the last value until the next sample. Correct for states.
	ResampleHold ResampleMode = "hold"
	// ResampleLinear interpolates. Correct for temperatures.
	ResampleLinear ResampleMode = "linear"
	// ResampleDistribute spreads a sample across the interval. Correct for energy.
	ResampleDistribute ResampleMode = "distribute"
)

// AnchorMode is where a replay puts the data on the clock.
type AnchorMode string

const (
	// AnchorLoop replays relative to simulation start and repeats forever. The only
	// usable mode for a site that runs permanently.
	AnchorLoop AnchorMode = "loop"
	// AnchorOriginal replays at the timestamps the data actually carries.
	AnchorOriginal AnchorMode = "original"
)

// DatasetSource replays a real timeseries into a simulated channel.
//
// The mapping fields exist because a German energy export imported naively
// produces wrong values instead of failing: semicolon separated, comma decimal
// mark, local time without an offset.
type DatasetSource struct {
	Origin DatasetOrigin `json:"origin"`

	// Ref is a platform device id, an uploaded dataset id or a URL, per Origin.
	Ref string `json:"ref"`
	// ServiceRef selects the service when Origin is OriginPlatform.
	ServiceRef string `json:"service_ref,omitempty"`
	// Column selects the value: for an upload the column name (empty is the first
	// one), for a platform timeseries the output variable path, e.g. "value".
	Column string `json:"column,omitempty"`

	// Window is how much of a platform timeseries is fetched, backwards from the
	// moment the environment starts: "36h", "7d", "4w".
	Window string `json:"window,omitempty"`

	Resample ResampleMode `json:"resample"`
	Anchor   AnchorMode   `json:"anchor"`
	// Scale multiplies every value, for adapting a foreign profile in size. Zero is
	// unscaled, so a document that omits it plays the data as it is.
	Scale float64 `json:"scale,omitempty"`
	// Cumulative keeps a meter reading counting across a loop boundary instead of
	// jumping back to the first value.
	Cumulative bool `json:"cumulative"`
}

// FormulaSource derives a value from other channels and the context.
type FormulaSource struct {
	Expression string `json:"expression"`
	// Inputs maps a name usable in Expression to a channel id or a context key.
	Inputs map[string]string `json:"inputs"`
}

// ---- the catalogue an asset is built from ----

// DeviceType is a platform device type a simulated asset can be built from,
// reduced by MOSES to what building one needs.
type DeviceType struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Services []DeviceTypeService `json:"services"`
}

// DeviceTypeService is one measuring point of a device type. CharacteristicId is
// what a channel takes verbatim; ValuePath names the variable inside the message.
type DeviceTypeService struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Direction        Direction `json:"direction"`
	CharacteristicId string    `json:"characteristic_id"`
	ValuePath        string    `json:"value_path"`
}

// ---- uploaded datasets ----

// DatasetColumn is one value column of an upload, as MOSES parsed it at upload
// time. The span is what tells a caller whether the file covers the window it
// wanted before anything replays it.
type DatasetColumn struct {
	Name     string `json:"name"`
	Points   int    `json:"points"`
	FromUnix int64  `json:"from_unix"`
	ToUnix   int64  `json:"to_unix"`
}

// From and To are the column's span as instants, which is the form everything
// else in ODE speaks.
func (c DatasetColumn) From() time.Time { return time.Unix(c.FromUnix, 0).UTC() }
func (c DatasetColumn) To() time.Time   { return time.Unix(c.ToUnix, 0).UTC() }

// Dataset is an uploaded timeseries. Datasets are immutable in MOSES: a replay
// stays reproducible because the data under an id can never change, so a
// corrected file is a new dataset rather than an update.
type Dataset struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Timezone    string          `json:"timezone"`
	Columns     []DatasetColumn `json:"columns"`
	SizeBytes   int64           `json:"size_bytes"`
	CreatedUnix int64           `json:"created_unix"`
}

// ---- live state ----

// StateChange is the body PATCH .../state accepts and the shape GET .../state
// answers in, so a value can be read, changed and sent straight back.
type StateChange struct {
	Context map[string]any            `json:"context"`
	Zones   map[string]map[string]any `json:"zones"`
	Assets  map[string]map[string]any `json:"assets"`
}

// Empty reports whether the change would do nothing. A patch that does nothing is
// refused before it is sent rather than after: MOSES answers it with a 400, and
// the round trip buys nothing.
func (c StateChange) Empty() bool {
	return len(c.Context) == 0 && len(c.Zones) == 0 && len(c.Assets) == 0
}

// EnvironmentState is the live state of one environment.
type EnvironmentState struct {
	StateChange

	// Running says whether the MOSES instance simulates this environment at all. A
	// stored but not running environment is the normal case for one just written,
	// and is not an error — it simply carries no state.
	Running bool `json:"running"`

	// AsOf is when the values were read. It is not decoration: a zone value with a
	// time constant is on its way to a set point and is resolved to exactly this
	// instant, so the number means nothing without it.
	AsOf time.Time `json:"as_of"`
}

// ---- backfill ----

// BackfillState is where a job stands.
type BackfillState string

const (
	BackfillRunning   BackfillState = "running"
	BackfillDone      BackfillState = "done"
	BackfillFailed    BackfillState = "failed"
	BackfillCancelled BackfillState = "cancelled"
)

// BackfillChannelStatus is what became of one channel. A channel that was not
// backfilled says why, because "no data appeared" is otherwise indistinguishable
// from a channel that published nothing.
type BackfillChannelStatus struct {
	ChannelID string `json:"channel_id"`
	AssetID   string `json:"asset_id"`
	Name      string `json:"name"`

	Backfillable bool   `json:"backfillable"`
	SkipReason   string `json:"skip_reason,omitempty"`

	Published int64 `json:"published"`
	Silent    int64 `json:"silent,omitempty"`
	Failed    int64 `json:"failed,omitempty"`

	LastError string `json:"last_error,omitempty"`
}

// BackfillStatus is one job as it stands.
type BackfillStatus struct {
	EnvironmentID string        `json:"environment_id"`
	State         BackfillState `json:"state"`

	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// ChannelsTotal counts every channel, backfillable or not; ChannelsDone counts
	// the ones that are finished with.
	ChannelsTotal int `json:"channels_total"`
	ChannelsDone  int `json:"channels_done"`

	CurrentChannel string     `json:"current_channel,omitempty"`
	Position       *time.Time `json:"position,omitempty"`

	Published int64  `json:"published"`
	Error     string `json:"error,omitempty"`

	Channels []BackfillChannelStatus `json:"channels"`
}

// Skipped is the channels that produced nothing and the reason each gave. It is
// the part of a finished job worth reading first: a job can report "done" with
// every channel skipped, and the honest answer to "where is my data" is this list.
func (s BackfillStatus) Skipped() []BackfillChannelStatus {
	out := []BackfillChannelStatus{}
	for _, channel := range s.Channels {
		if !channel.Backfillable {
			out = append(out, channel)
		}
	}
	return out
}

// forWrite is the document as it is sent back to MOSES.
//
// Two fields are cleared and no more. ExternalManaged and ExternalGraphRef are
// the ones MOSES reconciles from the stored document on every write, and an
// echoed value is what would decide that somebody's real device or another
// environment's graph is destroyed. Everything else — ExternalRef above all — is
// sent exactly as it was read, because MOSES provisions a device for any asset
// that names a type and carries no reference.
func (e Environment) forWrite() Environment {
	out := e
	out.ExternalGraphRef = ""
	out.UnknownField = ""
	out.Zones = zonesForWrite(e.Zones)
	return out
}

func zonesForWrite(zones []Zone) []Zone {
	if zones == nil {
		return nil
	}
	out := make([]Zone, len(zones))
	for i, zone := range zones {
		out[i] = zone
		out[i].Zones = zonesForWrite(zone.Zones)
		if zone.Assets != nil {
			assets := make([]Asset, len(zone.Assets))
			for j, asset := range zone.Assets {
				assets[j] = asset
				assets[j].ExternalManaged = false
			}
			out[i].Assets = assets
		}
	}
	return out
}

// ForEachAsset visits every asset, in nested zones as well. An asset in a
// sub-zone is not a special case anywhere else and must not become one here.
func (e *Environment) ForEachAsset(visit func(zone *Zone, asset *Asset)) {
	if e == nil {
		return
	}
	var walk func(zone *Zone)
	walk = func(zone *Zone) {
		for i := range zone.Zones {
			walk(&zone.Zones[i])
		}
		for i := range zone.Assets {
			visit(zone, &zone.Assets[i])
		}
	}
	for i := range e.Zones {
		walk(&e.Zones[i])
	}
}

// FindChannel locates one channel by id, with the asset that carries it.
func (e *Environment) FindChannel(channelID string) (*Asset, *Channel, bool) {
	var foundAsset *Asset
	var foundChannel *Channel
	e.ForEachAsset(func(_ *Zone, asset *Asset) {
		for i := range asset.Channels {
			if asset.Channels[i].ID == channelID {
				foundAsset, foundChannel = asset, &asset.Channels[i]
			}
		}
	})
	return foundAsset, foundChannel, foundChannel != nil
}

// FindZone locates one zone by id, anywhere in the nesting.
func (e *Environment) FindZone(zoneID string) (*Zone, bool) {
	var found *Zone
	var walk func(zone *Zone)
	walk = func(zone *Zone) {
		if zone.ID == zoneID {
			found = zone
		}
		for i := range zone.Zones {
			walk(&zone.Zones[i])
		}
	}
	for i := range e.Zones {
		walk(&e.Zones[i])
	}
	return found, found != nil
}

// unknownField decodes the document a second time, strictly, and reports the
// first member this mirror does not know.
//
// A second decode rather than a permanently strict type: strictness is only
// wanted where a document might be written back. A type that refused an unknown
// field on every decode would leave ODE unable to *read* an environment a newer
// MOSES stored, which is the wrong failure — reading stays lenient, and writing
// is what refuses (see ErrUnknownField).
//
// One name rather than all of them, because encoding/json stops at the first and
// one is enough to say what happened: this ODE is behind the MOSES it is talking
// to, and editing that environment from here would delete the field.
func unknownField(raw []byte) (string, bool) {
	var probe Environment
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&probe)
	if err == nil {
		return "", false
	}
	// The message is `json: unknown field "x"`, which is the only place the name
	// appears — encoding/json reports it as a plain error rather than a typed one.
	// Anything else here is the strict pass disagreeing about something the lenient
	// pass already accepted, and there is no field to name.
	match := unknownFieldPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return "", false
	}
	return match[1], true
}

var unknownFieldPattern = regexp.MustCompile(`unknown field "([^"]*)"`)

// ParseWindow reads a replay window the way MOSES does. time.ParseDuration covers
// h/m/s; days and weeks are there because "7d" is how people think about load
// data, and a caller that had to write "168h" would get it wrong.
//
// Mirrored rather than left to MOSES for one reason: the alternative is a channel
// stored with an unreadable window, which does not fail — it plays nothing.
func ParseWindow(window string) (time.Duration, error) {
	trimmed := strings.TrimSpace(window)
	if trimmed == "" {
		return 0, fmt.Errorf("the replay window must not be empty")
	}
	multiplier := time.Duration(0)
	switch trimmed[len(trimmed)-1] {
	case 'd':
		multiplier = 24 * time.Hour
	case 'w':
		multiplier = 7 * 24 * time.Hour
	}
	if multiplier > 0 {
		count, err := strconv.ParseFloat(trimmed[:len(trimmed)-1], 64)
		if err != nil || count <= 0 {
			return 0, fmt.Errorf("unreadable replay window %q", window)
		}
		return time.Duration(count * float64(multiplier)), nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("unreadable replay window %q, use a duration like \"36h\", \"7d\" or \"4w\"", window)
	}
	return duration, nil
}
