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

package profiler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// QuickProfile is the read-free profile tier (D20, §5.4.2). Everything in it
// comes from /data-availability, /usage/devices, the ontology and the device's
// connection state, so it sits at exposure tier L0 and a developer can go from
// an intent to a shortlist of three series before any value is read.
type QuickProfile struct {
	SeriesRef SeriesRef  `json:"series_ref"`
	Device    DeviceInfo `json:"device"`
	Tier      string     `json:"tier"`

	Availability Value[AvailabilityWindow] `json:"availability"`
	Volume       Value[Volume]             `json:"volume"`
	Declared     Declared                  `json:"declared"`
	Interaction  models.Interaction        `json:"interaction"`
	Liveness     Liveness                  `json:"liveness"`

	OntologyCompleteness Completeness `json:"ontology_completeness"`
	RankHints            RankHints    `json:"rank_hints"`

	// Queryable mirrors Variable.Queryable: a variable ODE cannot read is still
	// reported, because a developer hunting for it needs to know it was seen.
	Queryable bool   `json:"queryable"`
	Reason    string `json:"reason,omitempty"`

	Provenance Provenance `json:"provenance"`
}

// DeviceInfo is the candidate's device as a human reads it, beside the SeriesRef
// that machines key on.
//
// It exists because a ranked list of forty candidates addressed only by URN is a
// list nobody can choose from: the ids differ in their last few characters, and the
// device type is what separates two meters in the same room. The ids are not
// repeated here — SeriesRef carries them.
type DeviceInfo struct {
	// Name is the platform's display name where it has one (see devices.DisplayName).
	Name           string `json:"name"`
	DeviceTypeID   string `json:"device_type_id"`
	DeviceTypeName string `json:"device_type_name"`
}

type Aggregate struct {
	GroupTime string `json:"group_time"`
	GroupType string `json:"group_type"`
}

type AvailabilityWindow struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	SpanDays float64   `json:"span_days"`
	// Aggregates are the pre-aggregated variants that exist, which is what makes
	// the aggregated pass of §5.3.2 cheap when it matches one of them.
	Aggregates []Aggregate `json:"aggregates"`
	// RawAvailable is false when the platform reports only aggregated windows,
	// which retention policy can cause. The structural detectors need raw data,
	// so this decides whether a full profile is possible at all.
	RawAvailable bool `json:"raw_available"`
}

type Volume struct {
	Bytes       uint64  `json:"bytes"`
	BytesPerDay float64 `json:"bytes_per_day"`
	// EstimatedIntervalS is order of magnitude only and is never used for a
	// resampling decision (§5.4.2). It is derived from a device-wide byte rate
	// divided by a modelled row width, so it describes the device's message
	// rate rather than this variable's sampling interval.
	EstimatedIntervalS Value[float64] `json:"estimated_interval_s"`
	EstimateBasis      string         `json:"estimate_basis"`
	Confidence         Confidence     `json:"confidence"`
}

// Declared is what the ontology says the value should be, before any of it is
// checked against data.
type Declared struct {
	CharacteristicID *string        `json:"characteristic_id"`
	Unit             string         `json:"unit"`
	UnitSource       UnitSource     `json:"unit_source"`
	MinValue         Value[float64] `json:"min_value"`
	MaxValue         Value[float64] `json:"max_value"`
	Type             models.Type    `json:"type"`
	FunctionID       string         `json:"function_id,omitempty"`
	AspectID         string         `json:"aspect_id,omitempty"`
}

type Liveness struct {
	ConnectionState models.ConnectionState `json:"connection_state"`
	// LastValueAgeS is the age of the newest data point.
	//
	// It comes from the availability window's end, not from /last-values.
	// §5.4.2 lists the four sources a QuickProfile may use and /last-values is
	// not among them: it returns actual values, and reading one to compute an
	// age would put the tier's read-free property at the mercy of a discarded
	// field.
	LastValueAgeS Value[float64] `json:"last_value_age_s"`
	Basis         string         `json:"basis"`
}

const livenessBasis = "availability_window_end"

type Completeness struct {
	Status      string   `json:"status"`
	Missing     []string `json:"missing"`
	Consequence string   `json:"consequence,omitempty"`
}

const (
	CompletenessComplete = "complete"
	CompletenessPartial  = "partial"
)

// RankHints are the inputs to the ordering, exposed rather than hidden so a
// developer can see why one candidate outranks another (§5.2).
type RankHints struct {
	SpanDays      float64 `json:"span_days"`
	CoverageProxy float64 `json:"coverage_proxy"`
	IsLive        bool    `json:"is_live"`
	Score         float64 `json:"score"`
}

// Ranking weights. Deliberately explicit and deliberately blunt: this orders a
// shortlist for a human, and a developer who disagrees re-sorts in the UI.
const (
	weightSpan     = 0.3
	weightCoverage = 0.4
	weightLiveness = 0.3
	// spanSaturationDays is where more history stops improving the score. A
	// year covers the seasonal cycle that matters in this domain.
	spanSaturationDays = 365
	// liveWithinS is how recent the newest point must be for a series to count
	// as live. One day tolerates a daily-batch import without calling a dead
	// series live.
	liveWithinS = 24 * 3600
)

// QuickRequest describes the candidate set to profile.
type QuickRequest struct {
	// Devices must carry their DeviceType, which means they were read with
	// fulldt: the variable enumeration is a walk over the service outputs.
	Devices []models.ExtendedDevice
	// Window is the range the developer cares about, used for the coverage
	// proxy. Zero means the default lookback.
	Window Window
	// IncludeUnqueryable keeps variables that cannot be read as a series in the
	// result, ranked last. Default false, since the common caller wants
	// candidates rather than an inventory.
	IncludeUnqueryable bool
}

type SkippedDevice struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
}

// ReadCounts is what the operation actually asked of the platform.
//
// Values is the number that matters: M1a's acceptance is that ranking 40
// candidate series involves no value read at all, and a counter in the response
// is a stronger statement of that than a line in a log.
type ReadCounts struct {
	Availability int `json:"availability"`
	Usage        int `json:"usage"`
	Values       int `json:"values"`
}

type QuickResult struct {
	Candidates []QuickProfile  `json:"candidates"`
	Skipped    []SkippedDevice `json:"skipped"`
	Reads      ReadCounts      `json:"reads"`
	Window     Window          `json:"coverage_window"`
}

// QuickProfiles assembles and ranks a QuickProfile per addressable variable of
// every device given.
//
// Availability is per device and cannot be batched, so those calls run
// concurrently under a bounded limit; usage is one batched call for the whole
// set. Neither reads a value.
func (p *Profiler) QuickProfiles(ctx context.Context, token string, req QuickRequest) (QuickResult, error) {
	index, err := p.ontology.Ontology(ctx, token)
	if err != nil {
		return QuickResult{}, err
	}

	window := req.Window
	if !window.Valid() {
		now := p.now()
		window = Window{From: now.Add(-time.Duration(p.opts.CoverageWindowDays) * 24 * time.Hour), To: now}
	}

	result := QuickResult{Candidates: []QuickProfile{}, Skipped: []SkippedDevice{}, Window: window}

	readable := make([]models.ExtendedDevice, 0, len(req.Devices))
	for _, device := range req.Devices {
		switch {
		case device.DeviceType == nil:
			result.Skipped = append(result.Skipped, SkippedDevice{
				DeviceID: device.Id, Name: devices.DisplayName(device),
				Reason: "device type not resolved: read the device with full_device_type to enumerate its variables",
			})
		case !device.Permissions.Execute:
			// models.Read governs metadata; models.Execute governs reading the
			// device's data (§5.1). Offering a series ODE cannot read wastes the
			// developer's decision and fails at query time instead of here.
			result.Skipped = append(result.Skipped, SkippedDevice{
				DeviceID: device.Id, Name: devices.DisplayName(device),
				Reason: "no execute permission: the caller may see this device but not read its data",
			})
		default:
			readable = append(readable, device)
		}
	}
	if len(readable) == 0 {
		return result, nil
	}

	deviceIDs := make([]string, 0, len(readable))
	for _, device := range readable {
		deviceIDs = append(deviceIDs, device.Id)
	}

	usage := p.deviceUsage(ctx, token, deviceIDs, &result.Reads)
	availability := p.availability(ctx, token, deviceIDs, &result.Reads)

	for _, device := range readable {
		for _, variable := range DeviceTypeVariables(*device.DeviceType) {
			if !variable.Queryable && !req.IncludeUnqueryable {
				continue
			}
			result.Candidates = append(result.Candidates,
				p.quickProfile(device, variable, availability[device.Id], usage[device.Id], window, index))
		}
	}

	Rank(result.Candidates)

	slog.DebugContext(ctx, "quick profiles assembled",
		"devices", len(readable), "skipped", len(result.Skipped), "candidates", len(result.Candidates),
		"availability_reads", result.Reads.Availability, "usage_reads", result.Reads.Usage,
		"value_reads", result.Reads.Values)

	return result, nil
}

// deviceUsage batches the usage read. A failure is not fatal: volume becomes
// not_computed and the candidate is still rankable on span and liveness, which
// beats failing a whole candidate list over a cost estimate.
func (p *Profiler) deviceUsage(ctx context.Context, token string, deviceIDs []string, reads *ReadCounts) map[string]timeseries.Usage {
	out := map[string]timeseries.Usage{}
	usages, err := p.ts.DeviceUsage(ctx, token, deviceIDs)
	reads.Usage++
	if err != nil {
		slog.WarnContext(ctx, "usage read failed; volume will be reported as not computed", "error", err)
		return out
	}
	for _, usage := range usages {
		if usage.DeviceId != "" {
			out[usage.DeviceId] = usage
		}
	}
	return out
}

func (p *Profiler) availability(ctx context.Context, token string, deviceIDs []string, reads *ReadCounts) map[string][]timeseries.Availability {
	type entry struct {
		deviceID string
		windows  []timeseries.Availability
	}

	limit := p.opts.Concurrency
	if limit <= 0 {
		limit = defaultConcurrency
	}
	gate := make(chan struct{}, limit)
	results := make(chan entry, len(deviceIDs))

	wg := sync.WaitGroup{}
	for _, deviceID := range deviceIDs {
		wg.Add(1)
		go func(deviceID string) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()

			windows, err := p.ts.DataAvailability(ctx, token, deviceID)
			if err != nil {
				slog.WarnContext(ctx, "availability read failed",
					"device_id", deviceID, "error", err)
				results <- entry{deviceID: deviceID}
				return
			}
			results <- entry{deviceID: deviceID, windows: windows}
		}(deviceID)
	}
	wg.Wait()
	close(results)

	out := map[string][]timeseries.Availability{}
	for r := range results {
		reads.Availability++
		out[r.deviceID] = r.windows
	}
	return out
}

func (p *Profiler) quickProfile(
	device models.ExtendedDevice,
	variable Variable,
	availability []timeseries.Availability,
	usage timeseries.Usage,
	window Window,
	index *OntologyIndex,
) QuickProfile {
	prov := Provenance{}
	semantics := ResolveUnits(variable, index, prov)

	profile := QuickProfile{
		SeriesRef: SeriesRef{
			DeviceID:     device.Id,
			ServiceID:    variable.ServiceID,
			VariablePath: variable.Path,
		},
		Device: DeviceInfo{
			Name:           devices.DisplayName(device),
			DeviceTypeID:   device.DeviceTypeId,
			DeviceTypeName: devices.TypeName(device),
		},
		Tier:        TierQuick,
		Interaction: variable.Interaction,
		Queryable:   variable.Queryable,
		Reason:      variable.Reason,
		Declared: Declared{
			CharacteristicID: semantics.CharacteristicID,
			Unit:             semantics.Unit,
			UnitSource:       semantics.UnitSource,
			MinValue:         semantics.DeclaredRange.Min,
			MaxValue:         semantics.DeclaredRange.Max,
			Type:             variable.Type,
			FunctionID:       variable.FunctionID,
			AspectID:         variable.AspectID,
		},
		Provenance: prov,
	}

	profile.Availability = serviceAvailability(availability, variable.ServiceID, prov)
	profile.Volume = estimateVolume(usage, *device.DeviceType, prov)
	profile.Liveness = liveness(device.ConnectionState, profile.Availability, p.now(), prov)
	profile.OntologyCompleteness = completeness(variable, semantics)
	profile.RankHints = rankHints(profile, window)

	return profile
}

// serviceAvailability picks this service's window out of the device's
// availability response.
//
// The endpoint returns one element per service *and per materialised aggregate*,
// so the entry without a groupTime is the raw window and the rest describe
// pre-aggregated variants. A service with only aggregated entries has had its
// raw data aged out, which the structural detectors need to know about.
func serviceAvailability(entries []timeseries.Availability, serviceID string, prov Provenance) Value[AvailabilityWindow] {
	window := AvailabilityWindow{Aggregates: []Aggregate{}}
	found := false

	for _, entry := range entries {
		if entry.ServiceId != serviceID {
			continue
		}
		found = true
		groupTime := ""
		if entry.GroupTime != nil {
			groupTime = *entry.GroupTime
		}
		if groupTime == "" {
			window.RawAvailable = true
		} else {
			groupType := ""
			if entry.GroupType != nil {
				groupType = *entry.GroupType
			}
			window.Aggregates = append(window.Aggregates, Aggregate{GroupTime: groupTime, GroupType: groupType})
		}
		if entry.From != nil && (window.From.IsZero() || entry.From.Before(window.From)) {
			window.From = entry.From.UTC()
		}
		if entry.To != nil && entry.To.After(window.To) {
			window.To = entry.To.UTC()
		}
	}

	if !found {
		return Uncomputablef[AvailabilityWindow](ReasonReadFailed,
			"the platform reports no availability for service %s", serviceID)
	}
	if window.From.IsZero() || window.To.IsZero() {
		return Uncomputablef[AvailabilityWindow](ReasonInsufficientSpan,
			"service %s is known but has no data window yet", serviceID)
	}

	window.SpanDays = window.To.Sub(window.From).Hours() / 24
	sort.SliceStable(window.Aggregates, func(i, j int) bool {
		return window.Aggregates[i].GroupTime < window.Aggregates[j].GroupTime
	})
	prov.FromAPI("availability", "timescale-wrapper:/data-availability")
	return Computed(window)
}

// Row width model for the interval estimate. A timescale row costs a timestamp
// plus per-column storage plus tuple overhead; these two numbers are a crude
// stand-in for that, which is why the result is always uncertain.
const (
	bytesPerRowOverhead = 40
	bytesPerColumn      = 8
)

func estimateVolume(usage timeseries.Usage, deviceType models.DeviceType, prov Provenance) Value[Volume] {
	if usage.DeviceId == "" {
		return Uncomputable[Volume](ReasonReadFailed, "no usage figure for this device")
	}

	volume := Volume{
		Bytes:         usage.Bytes,
		BytesPerDay:   usage.BytesPerDay,
		EstimateBasis: "bytes_per_day",
		Confidence:    Uncertain,
	}

	columns := 0
	for _, service := range deviceType.Services {
		columns += len(ServiceVariables(service))
	}
	rowBytes := float64(bytesPerRowOverhead + bytesPerColumn*columns)

	switch {
	case usage.BytesPerDay <= 0:
		volume.EstimatedIntervalS = Uncomputable[float64](ReasonInsufficientSpan,
			"the platform reports no ingest rate for this device yet")
	case rowBytes <= 0:
		volume.EstimatedIntervalS = Uncomputable[float64](ReasonOutOfScope,
			"the device type declares no columns to model a row width from")
	default:
		rowsPerDay := usage.BytesPerDay / rowBytes
		if rowsPerDay <= 0 {
			volume.EstimatedIntervalS = Uncomputable[float64](ReasonInsufficientSpan,
				"the modelled row width exceeds a day's ingest")
		} else {
			volume.EstimatedIntervalS = Computed(86400 / rowsPerDay)
		}
	}

	prov.FromAPI("volume", "timescale-wrapper:/usage/devices")
	prov.FromInference("volume.estimated_interval_s", "volume_estimate_v1",
		fmt.Sprintf("device-wide byte rate over a modelled %.0f byte row across %d columns; "+
			"describes the device's message rate, not this variable's sampling interval", rowBytes, columns))
	return Computed(volume)
}

func liveness(state models.ConnectionState, availability Value[AvailabilityWindow], now time.Time, prov Provenance) Liveness {
	out := Liveness{ConnectionState: state, Basis: livenessBasis}
	window, ok := availability.Get()
	if !ok {
		out.LastValueAgeS = Uncomputable[float64](ReasonReadFailed,
			"no availability window to take the newest timestamp from")
		return out
	}
	age := now.Sub(window.To).Seconds()
	if age < 0 {
		// A window ending in the future means clock skew between ODE and the
		// database. Reporting zero is honest about "as recent as it gets"
		// without inventing a negative age.
		age = 0
	}
	out.LastValueAgeS = Computed(age)
	prov.FromAPI("liveness.last_value_age_s", "timescale-wrapper:/data-availability")
	return out
}

// VariableCompleteness reports what the ontology fails to declare about one
// variable, reading nothing.
//
// It exists so that semantic selection's ontology_gaps (§5.2) is the *same*
// judgement a QuickProfile reports in ontology_completeness, rather than a
// second gap detector with its own vocabulary. Two of those would eventually
// disagree about one variable in two places in the same UI, and neither would
// look wrong.
func VariableCompleteness(variable Variable, index *OntologyIndex) Completeness {
	return completeness(variable, ResolveUnits(variable, index, Provenance{}))
}

// completeness implements D16: what the device type fails to declare is
// discovered per variable at runtime and reported, never assumed.
func completeness(variable Variable, semantics ValueSemantics) Completeness {
	missing := []string{}
	if semantics.CharacteristicID == nil {
		missing = append(missing, "characteristic_id")
	}
	if semantics.Unit == "" {
		missing = append(missing, "unit")
	}
	if variable.FunctionID == "" {
		missing = append(missing, "function_id")
	}
	if variable.AspectID == "" {
		missing = append(missing, "aspect_id")
	}

	out := Completeness{Status: CompletenessComplete, Missing: missing}
	if len(missing) > 0 {
		out.Status = CompletenessPartial
	}
	switch {
	case semantics.UnitSource == UnitConflict:
		out.Consequence = "the declared characteristic and the function's concept disagree; the unit needs confirming"
	case semantics.CharacteristicID == nil && semantics.UnitSource == UnitFromUnitReference:
		out.Consequence = "the unit arrives in the message, so no conversion target can be selected without reading one"
	case semantics.CharacteristicID == nil:
		out.Consequence = "no characteristic, so no server-side unit conversion and no declared range check"
	case variable.FunctionID == "" || variable.AspectID == "":
		out.Consequence = "the variable cannot be found by semantic selection on function or aspect"
	}
	return out
}

func rankHints(profile QuickProfile, window Window) RankHints {
	hints := RankHints{}

	if availability, ok := profile.Availability.Get(); ok {
		hints.SpanDays = availability.SpanDays
		hints.CoverageProxy = coverageProxy(availability, window)
	}
	if age, ok := profile.Liveness.LastValueAgeS.Get(); ok {
		hints.IsLive = age <= liveWithinS && profile.Liveness.ConnectionState != models.ConnectionStateOffline
	}

	if !profile.Queryable {
		return hints
	}

	span := hints.SpanDays / spanSaturationDays
	if span > 1 {
		span = 1
	}
	live := 0.0
	if hints.IsLive {
		live = 1
	}
	hints.Score = weightSpan*span + weightCoverage*hints.CoverageProxy + weightLiveness*live
	return hints
}

// coverageProxy is the share of the window the developer cares about that the
// data actually spans.
//
// It is a proxy and named as one: real coverage needs a point count, which needs
// a read. What this answers without one is "does data exist across the range I
// intend to train on", which is what separates a usable candidate from a series
// that started last week.
func coverageProxy(availability AvailabilityWindow, window Window) float64 {
	if !window.Valid() {
		return 0
	}
	from := availability.From
	if from.Before(window.From) {
		from = window.From
	}
	to := availability.To
	if to.After(window.To) {
		to = window.To
	}
	overlap := to.Sub(from).Seconds()
	if overlap <= 0 {
		return 0
	}
	return overlap / window.Duration().Seconds()
}

// Rank orders candidates in place, best first (§5.2: a query may yield 40
// series, and the developer narrows to a handful before anything is read).
//
// Unqueryable variables sort last whatever their metadata says, and ties break
// on the series reference so the order is stable across calls.
func Rank(candidates []QuickProfile) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Queryable != b.Queryable {
			return a.Queryable
		}
		if a.RankHints.Score != b.RankHints.Score {
			return a.RankHints.Score > b.RankHints.Score
		}
		return a.SeriesRef.String() < b.SeriesRef.String()
	})
}
