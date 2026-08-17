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
	"math"
	"time"

	// The zone database is embedded rather than read from the host. ODE ships on
	// alpine with only ca-certificates, where LoadLocation finds nothing and the
	// profiler would refuse to start over a timezone it only needs to flag DST
	// with. Half a megabyte in the binary is the cheaper failure mode.
	_ "time/tzdata"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

const (
	defaultConcurrency   = 4
	defaultRawWindowDays = 14
	// The raw pass is bounded by days and by points, and the smaller wins (D25).
	// A hundred thousand rather than ten: at ten, a five-second series had a raw
	// window of fourteen *hours* against the fourteen days the profile claimed, so
	// the structural detectors were describing half a day. The cost is a slower
	// read, which is what the WebSocket surface exists to absorb.
	defaultRawWindowPoints    = 100000
	defaultCoverageWindowDays = 90
	// targetAggregatedBuckets is what the aggregated pass aims for: enough
	// resolution for a weekly cycle over a year, few enough that the FFT and the
	// ADF regression stay cheap.
	targetAggregatedBuckets = 4000
	// minExclusionBuckets is how many consecutive empty buckets make a hole
	// worth excluding rather than a bucket that happened to be missed.
	minExclusionBuckets = 3
)

// TimeseriesClient is the slice of pkg/timeseries the profiler uses. Declared
// here so a test can supply a client that fails if a value read is attempted,
// which is how M1a's zero-read property is enforced rather than asserted.
type TimeseriesClient interface {
	DataAvailability(ctx context.Context, token string, deviceID string) ([]timeseries.Availability, error)
	DeviceUsage(ctx context.Context, token string, deviceIDs []string) ([]timeseries.Usage, error)
	Query(ctx context.Context, token string, elements []timeseries.QueryElement, opts timeseries.QueryOptions) ([]timeseries.QueryResult, error)
}

type Options struct {
	// RawWindowMaxDays and RawWindowMaxPoints bound the raw pass (D25): the
	// smaller of 14 days or about 10 000 points, anchored at the most recent
	// data. Recent data matters most for drift, and a bounded raw read keeps the
	// cost predictable.
	RawWindowMaxDays   int
	RawWindowMaxPoints int
	// CoverageWindowDays is the lookback the QuickProfile coverage proxy uses
	// when the caller names no window.
	CoverageWindowDays int
	Concurrency        int
	// LocalTimezone is used only to flag DST transitions and never for
	// computation, which stays in UTC throughout.
	LocalTimezone string
	Now           func() time.Time
}

type Profiler struct {
	ts       TimeseriesClient
	ontology OntologySource
	store    Store
	opts     Options

	localZone *time.Location
	now       func() time.Time
}

// DefaultLocalTimezone is the zone DST transitions are flagged against. The
// platform's meter data is German, and 15-minute data across a DST boundary is
// the recurring failure mode §5.4.13 names.
const DefaultLocalTimezone = "Europe/Berlin"

func New(ts TimeseriesClient, ontologySource OntologySource, store Store, opts Options) (*Profiler, error) {
	if opts.RawWindowMaxDays <= 0 {
		opts.RawWindowMaxDays = defaultRawWindowDays
	}
	if opts.RawWindowMaxPoints <= 0 {
		opts.RawWindowMaxPoints = defaultRawWindowPoints
	}
	if opts.CoverageWindowDays <= 0 {
		opts.CoverageWindowDays = defaultCoverageWindowDays
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.LocalTimezone == "" {
		opts.LocalTimezone = DefaultLocalTimezone
	}
	zone, err := time.LoadLocation(opts.LocalTimezone)
	if err != nil {
		return nil, fmt.Errorf("profiler: local timezone %q: %w", opts.LocalTimezone, err)
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Profiler{
		ts: ts, ontology: ontologySource, store: store, opts: opts,
		localZone: zone, now: now,
	}, nil
}

func (p *Profiler) Store() Store { return p.store }

// ProfileRequest asks for the full profile of every variable of one service.
//
// The unit of work is the service rather than the variable, because the read is
// (D19, §5.4.1): one POST /queries/v2 per service covering all its variables,
// then one profile emitted per variable, each carrying the service context that
// makes the cross-variable checks possible.
type ProfileRequest struct {
	Device         models.ExtendedDevice
	ServiceID      string
	AnalysisWindow Window
	// RawWindow overrides the default bounded raw read. Recorded in the profile
	// as a developer override, so a profile computed over an unusual window is
	// not mistaken for a default one (D25).
	RawWindow     Window
	SessionParams *SessionParams
	// GroupTime overrides the aggregation bucket. Empty means derived from the
	// detected sampling interval and the window length.
	GroupTime string
}

type ProfileResult struct {
	Profiles []ResolvedProfile `json:"profiles"`
	Reads    ReadCounts        `json:"reads"`
	// FromCache lists the profiles that were already stored under the same cache
	// key and were therefore not recomputed.
	FromCache      []string  `json:"from_cache"`
	AnalysisWindow Window    `json:"analysis_window"`
	RawWindow      RawWindow `json:"raw_window"`
	GroupTime      string    `json:"group_time"`
}

// ProfileService computes a full SeriesProfile per variable of one service.
func (p *Profiler) ProfileService(ctx context.Context, token string, req ProfileRequest) (ProfileResult, error) {
	device := req.Device
	if device.DeviceType == nil {
		return ProfileResult{}, fmt.Errorf("%w: device %s was read without its device type",
			ErrInvalidRequest, device.Id)
	}
	if !device.Permissions.Execute {
		return ProfileResult{}, fmt.Errorf("%w: device %s", ErrNoPermission, device.Id)
	}

	var service models.Service
	found := false
	for _, candidate := range device.DeviceType.Services {
		if candidate.Id == req.ServiceID {
			service, found = candidate, true
			break
		}
	}
	if !found {
		return ProfileResult{}, fmt.Errorf("%w: device type %s has no service %s",
			ErrInvalidRequest, device.DeviceType.Id, req.ServiceID)
	}

	variables := []Variable{}
	for _, variable := range ServiceVariables(service) {
		if variable.Queryable {
			variables = append(variables, variable)
		}
	}
	if len(variables) == 0 {
		return ProfileResult{}, fmt.Errorf("%w: %s", ErrNoVariables, req.ServiceID)
	}

	index, err := p.ontology.Ontology(ctx, token)
	if err != nil {
		return ProfileResult{}, err
	}

	result := ProfileResult{Profiles: []ResolvedProfile{}, FromCache: []string{}}

	availability, err := p.ts.DataAvailability(ctx, token, device.Id)
	result.Reads.Availability++
	if err != nil {
		return ProfileResult{}, err
	}
	dataWindow, rawAvailable, ok := serviceWindow(availability, req.ServiceID)
	if !ok {
		return ProfileResult{}, fmt.Errorf("%w: the platform reports no data window for service %s",
			ErrInvalidRequest, req.ServiceID)
	}
	if !rawAvailable {
		// Not an error: the aggregated pass still produces a useful profile, and
		// refusing would be worse than a partial one. But it is worth a log line,
		// because every structural field is about to report not_computed and the
		// reason is upstream retention rather than anything in the data.
		slog.WarnContext(ctx, "no raw window for this service; the structural detectors cannot run",
			"device_id", device.Id, "service_id", req.ServiceID,
			"hint", "retention has left only aggregated buckets")
	}

	analysis := intersect(req.AnalysisWindow, dataWindow)
	if !analysis.Valid() {
		return ProfileResult{}, fmt.Errorf("%w: the requested window does not overlap the available data (%s)",
			ErrInvalidRequest, dataWindow.String())
	}
	raw := p.rawWindow(analysis, req.RawWindow)
	result.AnalysisWindow = analysis
	result.RawWindow = raw

	// Every variable of the service shares the analysis and raw windows, so the
	// cache either has all of them or the batch read has to happen anyway.
	cached, allCached := p.cachedProfiles(variables, device.Id, req.ServiceID, analysis, raw)
	if allCached && req.GroupTime == "" && req.SessionParams == nil {
		for _, profile := range cached {
			result.Profiles = append(result.Profiles, p.resolve(profile))
			result.FromCache = append(result.FromCache, profile.ProfileID)
		}
		result.GroupTime = ""
		return result, nil
	}

	rawSet, rawTruncated, err := p.readRaw(ctx, token, device.Id, req.ServiceID, variables, raw, &result.Reads)
	if err != nil {
		return ProfileResult{}, err
	}
	if rawTruncated && rawSet.Rows() > 0 {
		// The point limit cut the window short, so the recorded window has to be
		// the one actually read. Leaving the requested start in place would make
		// the missing head look like a gap.
		raw.From = rawSet.Times[0]
		raw.Truncated = true
		result.RawWindow = raw
	}

	// The sampling interval of the first numeric variable sizes the aggregation
	// bucket. Variables of one service share a message and therefore an arrival
	// schedule, so any of them answers the question.
	interval := representativeInterval(rawSet, variables)
	groupTime := req.GroupTime
	if groupTime == "" {
		groupTime = chooseGroupTime(analysis, interval)
	}
	result.GroupTime = groupTime
	bucket := bucketSecondsOf(groupTime)

	aggregated, err := p.readAggregated(ctx, token, device.Id, req.ServiceID, variables, analysis, groupTime, &result.Reads)
	if err != nil {
		// The aggregated pass is not fatal: every field it feeds carries
		// not_computed with read_failed, and the structural detectors still have
		// the raw pass.
		slog.WarnContext(ctx, "aggregated pass failed; statistical fields will report read_failed",
			"device_id", device.Id, "service_id", req.ServiceID, "error", err)
		aggregated = map[string]aggregatedSeries{}
	}

	computed := p.detect(detectionInput{
		device:       device,
		service:      service,
		variables:    variables,
		rawSet:       rawSet,
		aggregate:    aggregated,
		analysis:     analysis,
		raw:          raw,
		groupTime:    groupTime,
		bucket:       bucket,
		index:        index,
		params:       req.SessionParams,
		rawAvailable: rawAvailable,
	})

	for _, item := range computed {
		stored, created, err := p.store.Put(item.profile, item.sessions)
		if err != nil {
			return ProfileResult{}, err
		}
		if !created {
			result.FromCache = append(result.FromCache, stored.ProfileID)
		}
		result.Profiles = append(result.Profiles, p.resolve(stored))
	}

	slog.DebugContext(ctx, "service profiled",
		"device_id", device.Id, "service_id", req.ServiceID,
		"variables", len(variables), "raw_points", rawSet.Rows(),
		"group_time", groupTime, "value_reads", result.Reads.Values)

	return result, nil
}

// Profile returns one stored profile with its overlay applied.
func (p *Profiler) Profile(profileID string) (ResolvedProfile, bool) {
	profile, found := p.store.ByID(profileID)
	if !found {
		return ResolvedProfile{}, false
	}
	return p.resolve(profile), true
}

func (p *Profiler) resolve(profile SeriesProfile) ResolvedProfile {
	return Resolve(profile, p.store.Overrides(profile.SeriesRef))
}

func (p *Profiler) cachedProfiles(variables []Variable, deviceID, serviceID string, analysis Window, raw RawWindow) ([]SeriesProfile, bool) {
	out := make([]SeriesProfile, 0, len(variables))
	for _, variable := range variables {
		ref := SeriesRef{DeviceID: deviceID, ServiceID: serviceID, VariablePath: variable.Path}
		key := CacheKey(ref, analysis, raw.Window, DetectorVersion)
		profile, found := p.store.ByCacheKey(key)
		if !found {
			return nil, false
		}
		out = append(out, profile)
	}
	return out, true
}

// rawWindow anchors the bounded raw read at the most recent data (D25).
func (p *Profiler) rawWindow(analysis Window, override Window) RawWindow {
	if override.Valid() {
		bounded := intersect(override, analysis)
		if bounded.Valid() {
			return RawWindow{Window: bounded, Source: WindowDeveloperOverride}
		}
	}
	from := analysis.To.Add(-time.Duration(p.opts.RawWindowMaxDays) * 24 * time.Hour)
	if from.Before(analysis.From) {
		from = analysis.From
	}
	return RawWindow{Window: Window{From: from, To: analysis.To}, Source: WindowDefault}
}

// readRaw is the structural pass: no groupTime, so gaps, irregular sampling and
// counter steps survive to be detected (§5.3.2).
//
// It orders descending with a limit, which takes the newest points rather than
// the oldest when the limit bites — the window is anchored at recent data and
// truncating from the far end would hand the detectors the wrong fortnight.
// DecodeResults sorts back into ascending order.
func (p *Profiler) readRaw(
	ctx context.Context, token, deviceID, serviceID string,
	variables []Variable, raw RawWindow, reads *ReadCounts,
) (timeseries.ResultSet, bool, error) {
	columns := make([]timeseries.QueryColumn, 0, len(variables))
	for _, variable := range variables {
		columns = append(columns, timeseries.QueryColumn{Name: variable.Path})
	}

	limit := p.opts.RawWindowMaxPoints
	descending := timeseries.Direction(timeseries.OrderDescending)
	orderIndex := 0
	element := timeseries.QueryElement{
		DeviceId:         &deviceID,
		ServiceId:        &serviceID,
		Columns:          columns,
		Limit:            &limit,
		OrderColumnIndex: &orderIndex,
		OrderDirection:   &descending,
		Time: &timeseries.QueryTime{
			Start: stringPtr(raw.From.UTC().Format(time.RFC3339)),
			End:   stringPtr(raw.To.UTC().Format(time.RFC3339)),
		},
	}

	results, err := p.ts.Query(ctx, token, []timeseries.QueryElement{element}, timeseries.QueryOptions{})
	reads.Values++
	if err != nil {
		return timeseries.ResultSet{}, false, err
	}
	sets, err := timeseries.DecodeResults([]timeseries.QueryElement{element}, results, "")
	if err != nil {
		return timeseries.ResultSet{}, false, err
	}
	if len(sets) == 0 {
		return timeseries.ResultSet{ColumnNames: columnNames(variables)}, false, nil
	}
	// One element in, so one set out. Taking the first used to silently discard
	// any others, which mattered when a set was a sub-series rather than an
	// element.
	if len(sets) > 1 {
		slog.WarnContext(ctx, "the raw pass returned more sets than elements sent; using the first",
			"sets", len(sets), "device_id", deviceID, "service_id", serviceID)
	}
	return sets[0], sets[0].Rows() >= limit, nil
}

// readAggregated is the statistical pass: the full analysis window at a bucket
// width, in three elements so that mean, minimum and maximum each arrive under
// an unambiguous column name.
//
// Three elements rather than three columns in one element because two columns of
// the same name in one element would come back indistinguishable in
// ColumnNames.
func (p *Profiler) readAggregated(
	ctx context.Context, token, deviceID, serviceID string,
	variables []Variable, analysis Window, groupTime string, reads *ReadCounts,
) (map[string]aggregatedSeries, error) {
	numeric := make([]Variable, 0, len(variables))
	for _, variable := range variables {
		if variable.Numeric() {
			numeric = append(numeric, variable)
		}
	}
	if len(numeric) == 0 {
		return map[string]aggregatedSeries{}, nil
	}

	groupTypes := []string{timeseries.GroupMean, timeseries.GroupMin, timeseries.GroupMax}
	elements := make([]timeseries.QueryElement, 0, len(groupTypes))
	for _, groupType := range groupTypes {
		columns := make([]timeseries.QueryColumn, 0, len(numeric))
		for _, variable := range numeric {
			aggregate := groupType
			columns = append(columns, timeseries.QueryColumn{Name: variable.Path, GroupType: &aggregate})
		}
		bucket := groupTime
		elements = append(elements, timeseries.QueryElement{
			DeviceId:  &deviceID,
			ServiceId: &serviceID,
			Columns:   columns,
			GroupTime: &bucket,
			Time: &timeseries.QueryTime{
				Start: stringPtr(analysis.From.UTC().Format(time.RFC3339)),
				End:   stringPtr(analysis.To.UTC().Format(time.RFC3339)),
			},
		})
	}

	results, err := p.ts.Query(ctx, token, elements, timeseries.QueryOptions{})
	reads.Values++
	if err != nil {
		return nil, err
	}
	sets, err := timeseries.DecodeResults(elements, results, "")
	if err != nil {
		return nil, err
	}

	out := map[string]aggregatedSeries{}
	for _, variable := range numeric {
		out[variable.Path] = aggregatedSeries{}
	}
	for _, set := range sets {
		if set.RequestIndex < 0 || set.RequestIndex >= len(groupTypes) {
			continue
		}
		groupType := groupTypes[set.RequestIndex]
		for _, variable := range numeric {
			column, ok := set.Column(variable.Path)
			if !ok {
				continue
			}
			times, values, _ := column.Numeric()
			series := out[variable.Path]
			switch groupType {
			case timeseries.GroupMean:
				series.Times, series.Mean = times, values
			case timeseries.GroupMin:
				series.Min = values
			case timeseries.GroupMax:
				series.Max = values
			}
			out[variable.Path] = series
		}
	}
	return out, nil
}

// chooseGroupTime picks the aggregation bucket: never finer than the sampling
// interval, never so coarse that the window has fewer buckets than the
// statistical detectors need, and always a value the server's interval grammar
// accepts.
func chooseGroupTime(analysis Window, interval float64) string {
	desired := analysis.Duration().Seconds() / targetAggregatedBuckets
	if interval > desired {
		desired = interval
	}
	if desired <= 0 {
		desired = 900
	}

	// A ladder of round buckets, in seconds. It stops at 24 hours because the
	// server's interval grammar takes days as "d" while everything ODE formats is
	// hours or finer, and a day is the coarsest bucket that still resolves a
	// weekly cycle.
	ladder := []float64{1, 5, 10, 15, 30, 60, 300, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400}
	chosen := ladder[len(ladder)-1]
	for _, step := range ladder {
		if step >= desired {
			chosen = step
			break
		}
	}
	return formatGroupTime(chosen)
}

func formatGroupTime(seconds float64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", int(seconds))
	case seconds < 3600 && math.Mod(seconds, 60) == 0:
		return fmt.Sprintf("%dm", int(seconds/60))
	default:
		return fmt.Sprintf("%dh", int(math.Round(seconds/3600)))
	}
}

// representativeInterval takes the sampling interval from the first variable
// that yields one. Variables of one service arrive in the same message, so their
// arrival schedule is shared.
func representativeInterval(set timeseries.ResultSet, variables []Variable) float64 {
	for _, variable := range variables {
		column, ok := set.Column(variable.Path)
		if !ok {
			continue
		}
		if _, interval := detectSampling(column.Times); interval > 0 {
			return interval
		}
	}
	if set.Rows() > 1 {
		if _, interval := detectSampling(set.Times); interval > 0 {
			return interval
		}
	}
	return 0
}

// serviceWindow is the range the platform holds for a service, together with
// whether any of it is unbucketed.
//
// The endpoint returns one entry per service *and per materialised aggregate*, so
// an entry without a groupTime is the raw window and the rest describe
// pre-aggregated variants. A service with only aggregated entries has had its raw
// data aged out by retention — which the quick profile has always reported, and
// which the profile path used to discard, leaving a profile whose every
// structural field said "insufficient coverage" for a reason nobody could see.
func serviceWindow(entries []timeseries.Availability, serviceID string) (window Window, rawAvailable bool, found bool) {
	for _, entry := range entries {
		if entry.ServiceId != serviceID {
			continue
		}
		if entry.GroupTime == nil || *entry.GroupTime == "" {
			rawAvailable = true
		}
		if entry.From != nil && (window.From.IsZero() || entry.From.Before(window.From)) {
			window.From = entry.From.UTC()
			found = true
		}
		if entry.To != nil && entry.To.After(window.To) {
			window.To = entry.To.UTC()
			found = true
		}
	}
	if !found || !window.Valid() {
		return Window{}, rawAvailable, false
	}
	return window, rawAvailable, true
}

func intersect(requested, available Window) Window {
	if !requested.Valid() {
		return available
	}
	out := Window{From: requested.From, To: requested.To}
	if out.From.Before(available.From) {
		out.From = available.From
	}
	if out.To.After(available.To) {
		out.To = available.To
	}
	return out
}

func columnNames(variables []Variable) []string {
	out := make([]string, 0, len(variables))
	for _, variable := range variables {
		out = append(out, variable.Path)
	}
	return out
}

func stringPtr(s string) *string { return &s }
