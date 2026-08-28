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

package charts

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// resolvedSeries is one series with everything the read needs worked out.
type resolvedSeries struct {
	SeriesResolution

	variable  profiler.Variable
	parsed    transform
	element   timeseries.QueryElement
	groupTime string
	groupType string
	math      string
}

// resolve turns a specification into query elements.
//
// Every transform of §5.9 is mapped onto a field of POST /queries/v2 here, and
// none of them is computed in ODE. That is §5.3.1's whole point: downsampling is
// groupTime, arithmetic is math, unit conversion is
// sourceCharacteristicId → targetCharacteristicId against the ontology's
// conversion graph. A chart is the place where reimplementing any of the three
// would be easiest and least noticeable.
func (s *Service) resolve(ctx context.Context, token string, spec Spec) ([]resolvedSeries, []string, error) {
	index, err := s.deps.Ontology.Ontology(ctx, token)
	if err != nil {
		return nil, nil, err
	}

	span := spec.Window.Duration()
	chartBucket, chartWidened := timeseries.Bucket(spec.GroupTime, span, s.deps.MaxPoints)

	notes := []string{}
	if chartWidened {
		notes = append(notes, fmt.Sprintf(
			"group_time was widened to %s so the charted window fits %d points per series",
			chartBucket, s.deps.MaxPoints))
	}

	// One device read per distinct device rather than per series: a chart of three
	// variables of the same meter is one read, and it is the read that checks the
	// developer's own permission to execute against it.
	devices := map[string]models.ExtendedDevice{}

	out := make([]resolvedSeries, 0, len(spec.Series))
	for i, series := range spec.Series {
		device, cached := devices[series.Ref.DeviceID]
		if !cached {
			device, err = s.deps.Devices.Get(token, series.Ref.DeviceID, models.Execute)
			if err != nil {
				return nil, nil, err
			}
			devices[series.Ref.DeviceID] = device
		}
		resolved, err := s.resolveSeries(i, series, device, index, spec, chartBucket)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, resolved)
		if resolved.groupTime != chartBucket {
			notes = append(notes, fmt.Sprintf(
				"series %d is bucketed at %s rather than the chart's %s, so it is not aligned with the others "+
					"(alignment is a property of one shared groupTime)",
				i, resolved.groupTime, chartBucket))
		}
	}
	return out, notes, nil
}

// resolveSeries resolves one series: the variable it addresses, the unit its
// values are in once the overlay has been applied, and the query element.
func (s *Service) resolveSeries(
	position int,
	series SeriesSpec,
	device models.ExtendedDevice,
	index *profiler.OntologyIndex,
	spec Spec,
	chartBucket string,
) (resolvedSeries, error) {
	if device.DeviceType == nil {
		return resolvedSeries{}, fmt.Errorf("%w: device %s was read without its device type",
			profiler.ErrInvalidRequest, device.Id)
	}
	if !device.Permissions.Execute {
		return resolvedSeries{}, fmt.Errorf("%w: device %s", profiler.ErrNoPermission, device.Id)
	}

	variable, found := profiler.FindVariable(*device.DeviceType, series.Ref.ServiceID, series.Ref.VariablePath)
	if !found {
		return resolvedSeries{}, fmt.Errorf(
			"%w: device type %s has no variable %q on service %s",
			ErrInvalidSpec, device.DeviceType.Id, series.Ref.VariablePath, series.Ref.ServiceID)
	}
	if !variable.Queryable {
		return resolvedSeries{}, fmt.Errorf("%w: %q cannot be read as a scalar series: %s",
			ErrInvalidSpec, series.Ref.VariablePath, variable.Reason)
	}

	parsed, err := parseTransform(series.Transform)
	if err != nil {
		return resolvedSeries{}, err
	}

	unit := applyConfirmations(resolveUnit(variable, index), s.deps.Profiles.Overrides(series.Ref), index)

	resolved := resolvedSeries{
		SeriesResolution: SeriesResolution{
			Index:     position,
			Ref:       series.Ref,
			Label:     series.Label,
			Transform: parsed.String(),
			ProfileID: series.ProfileID,
			Unit:      unit,
			Notes:     []string{},
		},
		variable:  variable,
		parsed:    parsed,
		groupTime: chartBucket,
		groupType: timeseries.GroupMean,
	}

	column := timeseries.QueryColumn{Name: variable.Path}

	switch parsed.Kind {
	case transformResample:
		bucket, widened := timeseries.Bucket(parsed.Bucket, spec.Window.Duration(), s.deps.MaxPoints)
		resolved.groupTime = bucket
		if widened {
			resolved.Notes = append(resolved.Notes, fmt.Sprintf(
				"resample:%s was widened to %s so the window fits %d points",
				parsed.Bucket, bucket, s.deps.MaxPoints))
		}

	case TransformDiff, TransformRate:
		// difference-last is the per-bucket increase of a cumulative counter: the
		// platform differences server-side (§5.3.5 confirmed it exists), so a
		// counter is charted as what it delivered rather than as an ever-rising
		// line — and ODE does no arithmetic on values.
		resolved.groupType = timeseries.GroupDiffLast
		if parsed.Kind == TransformRate {
			seconds := timeseries.BucketSeconds(resolved.groupTime)
			if seconds <= 0 {
				return resolvedSeries{}, fmt.Errorf(
					"%w: rate needs a bucket of a fixed length and %q is calendar-dependent; resample: to a fixed interval first",
					ErrInvalidSpec, resolved.groupTime)
			}
			// Division by the bucket length, which the server applies to the column.
			// Scaling commutes with differencing and with every aggregate used here,
			// so the result is the increase per second whichever order the server
			// evaluates in.
			divisor := formatDivisor(seconds)
			resolved.math = "/" + divisor
			resolved.Notes = append(resolved.Notes, fmt.Sprintf(
				"rate is the per-bucket difference divided by %s s, both evaluated by the platform", divisor))
		}
		if kind, ok := unitKind(s.deps.Profiles, series.ProfileID); ok && kind != profiler.KindCumulativeCounter {
			resolved.Notes = append(resolved.Notes, fmt.Sprintf(
				"the profile classifies this series as %s rather than a cumulative counter, so %s may not mean what it looks like",
				kind, parsed.Kind))
		}

	case transformConvert:
		source, concept, err := convertTarget(unit, parsed.Target, index)
		if err != nil {
			return resolvedSeries{}, fmt.Errorf("series %d: %w", position, err)
		}
		sourceID, targetID, conceptID := source, parsed.Target, concept
		column.SourceCharacteristicId = &sourceID
		column.TargetCharacteristicId = &targetID
		column.ConceptId = &conceptID
		resolved.Unit = targetUnit(unit, parsed.Target)
		resolved.Notes = append(resolved.Notes, fmt.Sprintf(
			"converted from %s to %s by the platform, along the ontology's conversion graph",
			unit.Unit, resolved.Unit.Unit))
	}

	aggregate := resolved.groupType
	column.GroupType = &aggregate
	if resolved.math != "" {
		math := resolved.math
		column.Math = &math
	}

	deviceID, serviceID, bucket := series.Ref.DeviceID, series.Ref.ServiceID, resolved.groupTime
	resolved.element = timeseries.QueryElement{
		DeviceId:  &deviceID,
		ServiceId: &serviceID,
		Columns:   []timeseries.QueryColumn{column},
		GroupTime: &bucket,
		Time: &timeseries.QueryTime{
			Start: stringPtr(spec.Window.From.UTC().Format(time.RFC3339)),
			End:   stringPtr(spec.Window.To.UTC().Format(time.RFC3339)),
		},
	}

	if !variable.Numeric() {
		resolved.Notes = append(resolved.Notes, fmt.Sprintf(
			"%q is declared as %s, so it has no numeric aggregate and the line will be empty; "+
				"a status series is read as values rather than charted",
			variable.Path, variable.Type))
	}
	if unit.Confirmable {
		resolved.Notes = append(resolved.Notes, "the unit is not settled: "+unit.Note+
			" — confirming it is a developer action")
	}
	return resolved, nil
}

// unitKind reads the value kind a profile detected, when one is named. It is
// advisory: charting a diff of something that is not a counter is allowed, it is
// just worth saying.
func unitKind(profiles Profiles, profileID string) (profiler.ValueKind, bool) {
	if profileID == "" {
		return "", false
	}
	profile, found := profiles.ByID(profileID)
	if !found {
		return "", false
	}
	return profile.ValueSemantics.Kind.Get()
}

// Axis is the resolved y-axis: what the chart is actually drawn against, as
// opposed to what the specification claimed.
type Axis struct {
	Unit
	// Mixed says the series do not share a unit. The chart still draws — comparing
	// a power series with a temperature is a legitimate thing to want to look at —
	// but the axis label would be a lie, so the pane says so instead.
	Mixed bool `json:"mixed"`
	// From records where the label came from: the specification's own y_axis, the
	// resolved series, or nothing that agreed.
	From string `json:"from"`
}

// axis decides the label. The specification's y_axis wins where it is set: an
// author who says "kW" has said something deliberate. Otherwise the series agree
// or they do not, and both outcomes are reported rather than averaged away.
func (s *Service) axis(spec Spec, resolved []resolvedSeries) (Axis, []string) {
	notes := []string{}

	if spec.YAxis.Unit != "" {
		axis := Axis{
			Unit: Unit{
				Unit:                 spec.YAxis.Unit,
				UnitSource:           profiler.UnitSource(spec.YAxis.UnitSource),
				AvailableConversions: []profiler.Conversion{},
				ComputedUnit:         spec.YAxis.Unit,
			},
			From: "spec",
		}
		if axis.UnitSource == "" {
			axis.UnitSource = profiler.UnitUnknown
		}
		if axis.UnitSource != profiler.UnitFromCharacteristic {
			axis.Confirmable = true
			axis.Note = "the axis unit was stated in the specification rather than resolved from the ontology"
		}
		return axis, notes
	}

	units := map[string]bool{}
	for _, series := range resolved {
		units[series.Unit.Unit] = true
	}
	switch {
	case len(resolved) == 0:
		return Axis{Unit: Unit{UnitSource: profiler.UnitUnknown, AvailableConversions: []profiler.Conversion{}},
			From: "none"}, notes
	case len(units) == 1:
		return Axis{Unit: resolved[0].Unit, From: "series"}, notes
	default:
		notes = append(notes, "the series carry different units, so the axis is unlabelled; "+
			"convert: one of them to compare them on one scale")
		return Axis{
			Unit:  Unit{UnitSource: profiler.UnitUnknown, AvailableConversions: []profiler.Conversion{}},
			Mixed: true, From: "mixed",
		}, notes
	}
}

// Point is one charted value. Short field names because a chart of eight series
// at two thousand points repeats them sixteen thousand times.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// SeriesData is one series with its values.
type SeriesData struct {
	SeriesResolution
	GroupTime string  `json:"group_time"`
	GroupType string  `json:"group_type"`
	Math      string  `json:"math,omitempty"`
	Points    []Point `json:"points"`
	// NonNumeric is how many returned values were not numbers, and NullRows how
	// many rows carried nothing for this variable. Both are reported rather than
	// dropped quietly: a line with half its points missing looks like a sparse
	// series, and the cause matters (D24's distinction again).
	NonNumeric int `json:"non_numeric_dropped"`
	NullRows   int `json:"null_rows"`
	// Confirmations is the override overlay for this series, so the pane can show
	// what the developer has already decided next to what the detectors said.
	Confirmations []profiler.ProfileOverride `json:"confirmations"`
}

// Reads reports what drawing this chart cost the platform, in the same spirit as
// the QuickProfile read counters: the claim that the LLM path reads no values is
// checkable from the response rather than from a promise.
type Reads struct {
	Devices int `json:"devices"`
	Queries int `json:"queries"`
	Points  int `json:"points"`
}

// Data is a rendered chart's payload: everything the pane needs and nothing an
// LLM ever sees.
type Data struct {
	ChartID string          `json:"chart_id"`
	Title   string          `json:"title"`
	Caption string          `json:"caption,omitempty"`
	Window  profiler.Window `json:"window"`
	// GroupTime is the chart-wide bucket; Aligned says whether every series shares
	// it.
	GroupTime string `json:"group_time"`
	Aligned   bool   `json:"aligned"`

	Series      []SeriesData `json:"series"`
	Annotations []Annotation `json:"annotations"`
	Markers     []Marker     `json:"markers"`
	Axis        Axis         `json:"y_axis"`

	// AnnotationsDropped is how many bands the cap removed. Never silent: a chart
	// showing fifty of four hundred sessions looks like a chart of fifty.
	AnnotationsDropped int      `json:"annotations_dropped"`
	Reads              Reads    `json:"reads"`
	Notes              []string `json:"notes"`
}

// DataRequest asks for a chart's values. The window and bucket overrides are how
// the pane zooms without minting a second chart: the specification stays as it
// was authored, and the read follows the developer's eye.
type DataRequest struct {
	ChartID   string
	UserSub   string
	Window    profiler.Window
	GroupTime string
}

// Data reads the values a chart draws, with the caller's own token.
//
// This is the read the tier does not gate, and that is the design rather than a
// hole in it. §3.2's tiers bound what reaches an *LLM context*; a developer
// looking at their own permitted series is the baseline the whole product rests
// on. render_chart therefore sits at L1 and returns no values (§5.8): the model
// says what to draw, and this — behind the developer's token, on their request —
// is what draws it.
func (s *Service) Data(ctx context.Context, token string, req DataRequest) (Data, error) {
	spec, err := s.Get(req.ChartID, req.UserSub)
	if err != nil {
		return Data{}, err
	}

	if req.Window.From.IsZero() != req.Window.To.IsZero() {
		return Data{}, fmt.Errorf("%w: a window override needs both from and to", ErrInvalidSpec)
	}
	if req.Window.Valid() {
		spec.Window = profiler.Window{From: req.Window.From.UTC(), To: req.Window.To.UTC()}
	} else if !req.Window.From.IsZero() {
		return Data{}, fmt.Errorf("%w: the window override ends before it starts", ErrInvalidSpec)
	}
	if req.GroupTime != "" {
		bucket, err := parseBucket(req.GroupTime)
		if err != nil {
			return Data{}, err
		}
		spec.GroupTime = bucket
	}

	resolved, notes, err := s.resolve(ctx, token, spec)
	if err != nil {
		return Data{}, err
	}
	axis, axisNotes := s.axis(spec, resolved)
	notes = append(notes, axisNotes...)

	elements := make([]timeseries.QueryElement, 0, len(resolved))
	for _, series := range resolved {
		elements = append(elements, series.element)
	}

	// One batched request for the whole chart (§5.3.1 item 4). Not an optimisation:
	// the series are drawn on one axis and alignment is a property of the request.
	results, err := s.deps.Timeseries.Query(ctx, token, elements, timeseries.QueryOptions{})
	if err != nil {
		return Data{}, err
	}
	sets, err := timeseries.DecodeResults(elements, results, "")
	if err != nil {
		return Data{}, err
	}
	byElement := map[int]timeseries.ResultSet{}
	for _, set := range sets {
		byElement[set.RequestIndex] = set
	}

	data := Data{
		ChartID:     spec.ChartID,
		Title:       spec.Title,
		Caption:     spec.Caption,
		Window:      spec.Window,
		Aligned:     true,
		Series:      make([]SeriesData, 0, len(resolved)),
		Annotations: []Annotation{},
		Markers:     []Marker{},
		Axis:        axis,
		Notes:       notes,
		Reads: Reads{
			Devices: distinctDevices(spec.Series),
			Queries: 1,
		},
	}

	chartBucket, _ := timeseries.Bucket(spec.GroupTime, spec.Window.Duration(), s.deps.MaxPoints)
	data.GroupTime = chartBucket

	for _, series := range resolved {
		entry := SeriesData{
			SeriesResolution: series.SeriesResolution,
			GroupTime:        series.groupTime,
			GroupType:        series.groupType,
			Math:             series.math,
			Points:           []Point{},
			Confirmations:    s.deps.Profiles.Overrides(series.Ref),
		}
		if entry.Confirmations == nil {
			entry.Confirmations = []profiler.ProfileOverride{}
		}
		if series.groupTime != chartBucket {
			data.Aligned = false
		}

		if set, found := byElement[series.Index]; found {
			if column, ok := set.Column(series.variable.Path); ok {
				times, values, dropped := column.Numeric()
				entry.NonNumeric = dropped
				entry.NullRows = column.NullRows
				for i := range times {
					if len(entry.Points) >= s.deps.MaxPoints {
						break
					}
					entry.Points = append(entry.Points, Point{T: times[i].UTC(), V: values[i]})
				}
				sort.SliceStable(entry.Points, func(a, b int) bool {
					return entry.Points[a].T.Before(entry.Points[b].T)
				})
			}
		}
		data.Reads.Points += len(entry.Points)
		data.Series = append(data.Series, entry)

		// The author's own annotations, clipped to the window that is on screen.
		for _, annotation := range spec.Annotations {
			if annotation.SeriesIndex != nil && *annotation.SeriesIndex == series.Index &&
				overlaps(annotation.From, annotation.To, spec.Window) {
				data.Annotations = append(data.Annotations, annotation)
			}
		}
		for _, marker := range spec.Markers {
			if marker.SeriesIndex != nil && *marker.SeriesIndex == series.Index &&
				!marker.At.Before(spec.Window.From) && !marker.At.After(spec.Window.To) {
				data.Markers = append(data.Markers, marker)
			}
		}

		derivedForSeries := s.derive(series, spec.Window)
		data.Annotations = append(data.Annotations, derivedForSeries.annotations...)
		data.Markers = append(data.Markers, derivedForSeries.markers...)
		data.Notes = append(data.Notes, derivedForSeries.notes...)
	}

	// Chart-wide annotations, which belong to no single series.
	for _, annotation := range spec.Annotations {
		if annotation.SeriesIndex == nil && overlaps(annotation.From, annotation.To, spec.Window) {
			data.Annotations = append(data.Annotations, annotation)
		}
	}
	for _, marker := range spec.Markers {
		if marker.SeriesIndex == nil &&
			!marker.At.Before(spec.Window.From) && !marker.At.After(spec.Window.To) {
			data.Markers = append(data.Markers, marker)
		}
	}

	data.Annotations, data.AnnotationsDropped = capAnnotations(data.Annotations, s.deps.MaxAnnotations)
	if data.AnnotationsDropped > 0 {
		data.Notes = append(data.Notes, fmt.Sprintf(
			"%d annotations were left out of the %d the window contains; narrow the window, "+
				"or page the full list through the sessions resource",
			data.AnnotationsDropped, data.AnnotationsDropped+len(data.Annotations)))
	}
	if !data.Aligned {
		data.Notes = append(data.Notes,
			"the series are not on one bucket, so points at the same x are not the same interval")
	}
	sort.SliceStable(data.Markers, func(a, b int) bool { return data.Markers[a].At.Before(data.Markers[b].At) })
	return data, nil
}

// derive collects the profiler-authored annotations for one series.
//
// The sessions come from the paginated resource rather than from the profile body,
// because the body carries statistics and a few exemplars by design (D27) — and a
// chart wants the boundaries that fall inside the window, which is exactly what
// that resource answers.
func (s *Service) derive(series resolvedSeries, window profiler.Window) derived {
	if series.ProfileID == "" {
		return derived{annotations: []Annotation{}, markers: []Marker{}, notes: []string{}}
	}
	profile, found := s.deps.Profiles.ByID(series.ProfileID)
	if !found {
		return derived{
			annotations: []Annotation{}, markers: []Marker{},
			notes: []string{fmt.Sprintf(
				"series %d names profile %s, which this deployment no longer holds — computed profiles are "+
					"in memory, so a restart loses them and they are recomputed rather than recovered",
				series.Index, series.ProfileID)},
		}
	}

	sessions := []profiler.Session{}
	page, err := s.deps.Profiles.Sessions(series.ProfileID, profiler.SessionQuery{
		From:  window.From,
		To:    window.To,
		Limit: sessionAnnotationLimit,
	})
	if err == nil {
		sessions = page.Sessions
	}

	out := deriveAnnotations(profile, sessions, s.deps.Profiles.Overrides(series.Ref),
		window, series.Index, s.deps.IDs)
	if err == nil && page.Total > len(page.Sessions) {
		out.notes = append(out.notes, fmt.Sprintf(
			"series %d has %d detected sessions in this window and the first %d are shown",
			series.Index, page.Total, len(page.Sessions)))
	}
	return out
}

func distinctDevices(series []SeriesSpec) int {
	seen := map[string]bool{}
	for _, entry := range series {
		seen[entry.Ref.DeviceID] = true
	}
	return len(seen)
}

func stringPtr(s string) *string { return &s }

// formatDivisor renders a bucket length for the server's `math` field.
//
// Not %g, which is the obvious choice and the wrong one: it switches to
// exponential notation at 1e6, and the server validates math against
// `^([+\-*/])\d+(\.\d+)?$`, which has no exponent. A rate over a bucket of
// fourteen days is 1209600 s, so `/%g` produced `/1.2096e+06` and the request was
// rejected — and because §5.3.1 sends every series of a chart in one batch, one
// rate series took the whole chart with it. The threshold is a bucket of about
// eleven and a half days, which is why nothing shorter ever showed it.
func formatDivisor(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}
