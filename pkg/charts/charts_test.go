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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

const (
	deviceID   = "urn:infai:ses:device:chart-1"
	serviceID  = "urn:infai:ses:service:11111111-2222-3333-4444-555555555555"
	powerPath  = "value.power"
	energyPath = "value.energy"
	statusPath = "value.status"
	wattID     = "urn:infai:ses:characteristic:watt"
	kilowattID = "urn:infai:ses:characteristic:kilowatt"
	conceptID  = "urn:infai:ses:concept:power"
	developer  = "sub-developer"
)

var (
	chartFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	chartTo   = time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
)

// --- fakes ---

type fakeTimeseries struct {
	elements []timeseries.QueryElement
	token    string
	calls    int
	rows     [][]any
	err      error
}

func (f *fakeTimeseries) Query(
	_ context.Context, token string, elements []timeseries.QueryElement, _ timeseries.QueryOptions,
) ([]timeseries.QueryResult, error) {
	f.token = token
	f.calls++
	f.elements = elements
	if f.err != nil {
		return nil, f.err
	}
	device, service := deviceID, serviceID
	out := make([]timeseries.QueryResult, 0, len(elements))
	for i, element := range elements {
		names := []string{"time"}
		for _, column := range element.Columns {
			names = append(names, column.Name)
		}
		out = append(out, timeseries.QueryResult{
			RequestIndex: i, DeviceId: &device, ServiceId: &service,
			ColumnNames: names, Data: [][][]any{f.rows},
		})
	}
	return out, nil
}

type fakeDevices struct {
	device models.ExtendedDevice
	reads  int
	err    error
}

func (f *fakeDevices) Get(_ string, id string, action drmodel.AuthAction) (models.ExtendedDevice, error) {
	f.reads++
	if f.err != nil {
		return models.ExtendedDevice{}, f.err
	}
	if action != models.Execute {
		return models.ExtendedDevice{}, fmt.Errorf("a chart read must be checked under execute, got %v", action)
	}
	if id != f.device.Id {
		return models.ExtendedDevice{}, errors.New("no such device")
	}
	return f.device, nil
}

type staticOntology struct{ index *profiler.OntologyIndex }

func (s staticOntology) Ontology(context.Context, string) (*profiler.OntologyIndex, error) {
	return s.index, nil
}

// sequentialIDs makes the annotation and chart ids assertable.
type sequentialIDs struct{ n int }

func (s *sequentialIDs) NewID() string {
	s.n++
	return "id-" + strconv.Itoa(s.n)
}

// --- fixtures ---

func ontologyIndex() *profiler.OntologyIndex {
	watt := models.Characteristic{Id: wattID, Name: "Watt", DisplayUnit: "W"}
	kilowatt := models.Characteristic{Id: kilowattID, Name: "Kilowatt", DisplayUnit: "kW"}
	return profiler.NewOntologyIndex(
		[]models.Characteristic{watt, kilowatt},
		[]models.ConceptWithCharacteristics{{
			Id: conceptID, BaseCharacteristicId: wattID,
			Characteristics: []models.Characteristic{watt, kilowatt},
			Conversions: []models.ConverterExtension{
				{From: wattID, To: kilowattID, Distance: 1},
				{From: kilowattID, To: wattID, Distance: 1},
			},
		}},
		[]models.Function{{Id: "fn-power", ConceptId: conceptID}},
	)
}

func chartDevice() models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: "PV meter", DeviceTypeId: "dt-meter"},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: "dt-meter", Name: "Meter",
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: "value", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{
							{
								Id: "cv-power", Name: "power", Type: models.Float,
								CharacteristicId: wattID, FunctionId: "fn-power", AspectId: "site",
							},
							{
								// No characteristic and no unit reference: the case where a
								// unit cannot be resolved and D29 says never to invent one.
								Id: "cv-energy", Name: "energy", Type: models.Float,
								FunctionId: "fn-power", AspectId: "site",
							},
							{
								Id: "cv-status", Name: "status", Type: models.String,
								FunctionId: "fn-power", AspectId: "site",
							},
						},
					},
				}},
			}},
		},
	}
}

func hourlyRows(from, to time.Time) [][]any {
	rows := [][]any{}
	for at := from; at.Before(to); at = at.Add(time.Hour) {
		rows = append(rows, []any{
			at.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			json.Number("42.5"),
		})
	}
	return rows
}

type harness struct {
	service    *Service
	timeseries *fakeTimeseries
	devices    *fakeDevices
	profiles   *profiler.MemoryStore
	ids        *sequentialIDs
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		timeseries: &fakeTimeseries{rows: hourlyRows(chartFrom, chartTo)},
		devices:    &fakeDevices{device: chartDevice()},
		profiles:   profiler.NewMemoryStore(),
		ids:        &sequentialIDs{},
	}
	service, err := New(Deps{
		Timeseries: h.timeseries,
		Devices:    h.devices,
		Ontology:   staticOntology{index: ontologyIndex()},
		Profiles:   h.profiles,
		Store:      NewMemoryStore(0),
		IDs:        h.ids,
		MaxPoints:  2000,
		Now:        func() time.Time { return chartTo },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.service = service
	return h
}

func (h *harness) create(t *testing.T, req CreateRequest) Created {
	t.Helper()
	if req.UserSub == "" {
		req.UserSub = developer
	}
	created, err := h.service.Create(context.Background(), "Bearer t", req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func powerSeries(transform string) SeriesSpec {
	return SeriesSpec{
		Ref:       profiler.SeriesRef{DeviceID: deviceID, ServiceID: serviceID, VariablePath: powerPath},
		Transform: transform,
	}
}

func window() profiler.Window { return profiler.Window{From: chartFrom, To: chartTo} }

// --- the specification (§5.9) ---

func TestATransformIsOneOfTheFormsTheSpecificationNames(t *testing.T) {
	valid := map[string]transform{
		"":                  {Kind: TransformNone},
		"none":              {Kind: TransformNone},
		"diff":              {Kind: TransformDiff},
		"rate":              {Kind: TransformRate},
		"resample:900s":     {Kind: transformResample, Bucket: "15m"},
		"resample:15m":      {Kind: transformResample, Bucket: "15m"},
		"resample:90s":      {Kind: transformResample, Bucket: "90s"},
		"resample:1day":     {Kind: transformResample, Bucket: "1day"},
		"convert:" + wattID: {Kind: transformConvert, Target: wattID},
	}
	for input, want := range valid {
		got, err := parseTransform(input)
		if err != nil {
			t.Errorf("parseTransform(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseTransform(%q) = %+v, want %+v", input, got, want)
		}
	}

	for _, input := range []string{"smooth", "resample:", "convert:", "resample:0s", "diff:1"} {
		if _, err := parseTransform(input); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("parseTransform(%q) accepted or misclassified: %v", input, err)
		}
	}
}

// A resample of 90 seconds must not become a bucket of 60: the label would say one
// thing and the read would do another.
func TestASubMinuteResampleKeepsItsLength(t *testing.T) {
	parsed, err := parseTransform("resample:90s")
	if err != nil {
		t.Fatalf("parseTransform: %v", err)
	}
	if seconds := timeseries.BucketSeconds(parsed.Bucket); seconds != 90 {
		t.Errorf("resample:90s resolved to %s (%g s), want 90 s", parsed.Bucket, seconds)
	}
}

func TestCreateRefusesASpecificationItCannotDraw(t *testing.T) {
	h := newHarness(t)
	index := 0
	outside := 7

	cases := map[string]CreateRequest{
		"no series": {UserSub: developer},
		"a series that is not fully addressed": {UserSub: developer, Series: []SeriesSpec{{
			Ref: profiler.SeriesRef{DeviceID: deviceID, ServiceID: serviceID},
		}}},
		"an unknown variable": {UserSub: developer, Series: []SeriesSpec{{
			Ref: profiler.SeriesRef{DeviceID: deviceID, ServiceID: serviceID, VariablePath: "value.nothing"},
		}}},
		"an unparseable transform": {UserSub: developer, Series: []SeriesSpec{powerSeries("smooth")}},
		"a span that ends before it starts": {UserSub: developer, Series: []SeriesSpec{powerSeries("")},
			Annotations: []Annotation{{From: chartTo, To: chartFrom, Label: "backwards"}}},
		"an unknown severity": {UserSub: developer, Series: []SeriesSpec{powerSeries("")},
			Annotations: []Annotation{{From: chartFrom, To: chartTo, Label: "x", Severity: "critical"}}},
		"a confirmable annotation with no field path": {UserSub: developer, Series: []SeriesSpec{powerSeries("")},
			Annotations: []Annotation{{From: chartFrom, To: chartTo, Label: "x",
				Confirmable: true, SeriesIndex: &index}}},
		"a confirmable annotation on no series": {UserSub: developer, Series: []SeriesSpec{powerSeries("")},
			Annotations: []Annotation{{From: chartFrom, To: chartTo, Label: "x",
				Confirmable: true, FieldPath: profiler.FieldSessions}}},
		"an annotation on a series that does not exist": {UserSub: developer, Series: []SeriesSpec{powerSeries("")},
			Annotations: []Annotation{{From: chartFrom, To: chartTo, Label: "x", SeriesIndex: &outside}}},
		"a window with only one end": {UserSub: developer, Series: []SeriesSpec{powerSeries("")},
			Window: profiler.Window{From: chartFrom}},
	}

	for name, request := range cases {
		if _, err := h.service.Create(context.Background(), "Bearer t", request); err == nil {
			t.Errorf("Create accepted %s", name)
		}
	}
}

// The point of the closed set is that a confirmable band leads somewhere. A path
// the profiler does not consider confirmable is refused rather than accepted and
// silently ignored for the rest of the project.
func TestAConfirmableAnnotationMustNameAConfirmableField(t *testing.T) {
	h := newHarness(t)
	index := 0
	_, err := h.service.Create(context.Background(), "Bearer t", CreateRequest{
		UserSub: developer,
		Series:  []SeriesSpec{powerSeries("")},
		Annotations: []Annotation{{
			From: chartFrom, To: chartTo, Label: "look here",
			Confirmable: true, FieldPath: "distribution.mean", SeriesIndex: &index,
		}},
	})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Create accepted a confirmation of distribution.mean: %v", err)
	}
}

func TestTheUnitComesFromTheOntologyAndTheConversionsWithIt(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{Series: []SeriesSpec{powerSeries("")}, Window: window()})

	unit := created.Series[0].Unit
	if unit.Unit != "W" || unit.UnitSource != profiler.UnitFromCharacteristic {
		t.Errorf("unit = %q from %q, want W from the characteristic", unit.Unit, unit.UnitSource)
	}
	if unit.CharacteristicID == nil || *unit.CharacteristicID != wattID {
		t.Errorf("characteristic = %v, want %s", unit.CharacteristicID, wattID)
	}
	if unit.Confirmable {
		t.Error("a unit the ontology answered is offered for confirmation; §5.10 says the ontology reduces that need")
	}
	if len(unit.AvailableConversions) != 1 || unit.AvailableConversions[0].ToUnit != "kW" {
		t.Errorf("conversions = %+v, want kW reachable", unit.AvailableConversions)
	}
	if created.Axis.Unit.Unit != "W" || created.Axis.From != "series" {
		t.Errorf("axis = %+v, want W resolved from the series", created.Axis)
	}
}

func TestAnUnresolvableUnitIsOfferedForConfirmationRatherThanInvented(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{
		Series: []SeriesSpec{{Ref: profiler.SeriesRef{
			DeviceID: deviceID, ServiceID: serviceID, VariablePath: energyPath,
		}}},
		Window: window(),
	})

	unit := created.Series[0].Unit
	if unit.CharacteristicID != nil {
		t.Errorf("characteristic = %v, want none: D29 forbids fabricating one", *unit.CharacteristicID)
	}
	if unit.UnitSource != profiler.UnitUnknown {
		t.Errorf("unit source = %q, want unknown", unit.UnitSource)
	}
	if !unit.Confirmable {
		t.Error("an unknown unit is not offered for confirmation, so §5.10's control has nothing to act on")
	}
}

func TestAChartBelongsToTheDeveloperWhoCreatedIt(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{Series: []SeriesSpec{powerSeries("")}, Window: window()})

	if _, err := h.service.Get(created.Spec.ChartID, "someone-else"); !errors.Is(err, ErrChartNotFound) {
		t.Errorf("another developer read the chart: %v", err)
	}
	_, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
		ChartID: created.Spec.ChartID, UserSub: "someone-else",
	})
	if !errors.Is(err, ErrChartNotFound) {
		t.Errorf("another developer read the chart's data: %v", err)
	}
}

// The window is resolved once and stored absolute, so the same chart still means
// the same thing tomorrow.
func TestTheWindowFallsBackToTheProfileThatCarriesTheAnnotations(t *testing.T) {
	h := newHarness(t)
	profile := storedProfile(t, h, powerPath)

	created := h.create(t, CreateRequest{Series: []SeriesSpec{{
		Ref:       profiler.SeriesRef{DeviceID: deviceID, ServiceID: serviceID, VariablePath: powerPath},
		ProfileID: profile.ProfileID,
	}}})
	if !created.Spec.Window.From.Equal(chartFrom) || !created.Spec.Window.To.Equal(chartTo) {
		t.Errorf("window = %s, want the profile's analysis window %s",
			created.Spec.Window.String(), profile.AnalysisWindow.String())
	}
}

func TestTheDefaultWindowIsTheLookback(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{Series: []SeriesSpec{powerSeries("")}})
	if got := created.Spec.Window.Duration(); got != defaultLookback {
		t.Errorf("window = %s, want the %s default lookback", got, defaultLookback)
	}
	if !created.Spec.Window.To.Equal(chartTo) {
		t.Errorf("window ends at %s, want now", created.Spec.Window.To)
	}
}

// --- transforms map onto the platform, never onto arithmetic here (§5.3.1) ---

func TestEveryTransformIsResolvedIntoAQueryField(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{
		Series: []SeriesSpec{
			powerSeries(""),
			powerSeries("resample:15m"),
			powerSeries("diff"),
			powerSeries("rate"),
			powerSeries("convert:" + kilowattID),
		},
		Window: window(),
		// An explicit bucket, so the assertions below are about the mapping rather
		// than about what the point cap would have derived.
		GroupTime: "1h",
	})

	if _, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
		ChartID: created.Spec.ChartID, UserSub: developer,
	}); err != nil {
		t.Fatalf("Data: %v", err)
	}
	if h.timeseries.calls != 1 {
		t.Errorf("%d queries for one chart; §5.3.1 item 4 wants one batched request", h.timeseries.calls)
	}
	elements := h.timeseries.elements
	if len(elements) != 5 {
		t.Fatalf("%d elements, want one per series", len(elements))
	}

	// none: the chart-wide bucket and a plain mean.
	if got := deref(elements[0].GroupTime); got != "1h" {
		t.Errorf("series 0 bucket = %q, want the chart's 1h", got)
	}
	if got := deref(elements[0].Columns[0].GroupType); got != timeseries.GroupMean {
		t.Errorf("series 0 aggregate = %q, want mean", got)
	}

	// resample: the bucket the author asked for, not the chart's.
	if got := deref(elements[1].GroupTime); got != "15m" {
		t.Errorf("series 1 bucket = %q, want 15m", got)
	}

	// diff: differenced by the platform.
	if got := deref(elements[2].Columns[0].GroupType); got != timeseries.GroupDiffLast {
		t.Errorf("series 2 aggregate = %q, want %s", got, timeseries.GroupDiffLast)
	}
	if elements[2].Columns[0].Math != nil {
		t.Errorf("series 2 carries math %q; a diff needs none", *elements[2].Columns[0].Math)
	}

	// rate: the same difference divided by the bucket length, by the platform.
	if got := deref(elements[3].Columns[0].Math); got != "/3600" {
		t.Errorf("series 3 math = %q, want /3600 for an hourly bucket", got)
	}

	// convert: source, target and concept, all from the ontology.
	column := elements[4].Columns[0]
	if got := deref(column.SourceCharacteristicId); got != wattID {
		t.Errorf("convert source = %q, want the variable's own characteristic", got)
	}
	if got := deref(column.TargetCharacteristicId); got != kilowattID {
		t.Errorf("convert target = %q, want %q", got, kilowattID)
	}
	if got := deref(column.ConceptId); got != conceptID {
		t.Errorf("convert concept = %q; the platform rejects a target without one", got)
	}

	for i, element := range elements {
		if !element.Valid() {
			t.Errorf("element %d is rejected by the shared schema: %+v", i, element)
		}
	}
}

func TestAConvertedSeriesIsLabelledInTheTargetUnit(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{
		Series: []SeriesSpec{powerSeries("convert:" + kilowattID)},
		Window: window(),
	})
	if got := created.Series[0].Unit.Unit; got != "kW" {
		t.Errorf("converted unit = %q, want kW", got)
	}
	if created.Axis.Unit.Unit != "kW" {
		t.Errorf("axis = %q, want the converted unit", created.Axis.Unit.Unit)
	}
}

func TestAConversionOutsideTheGraphIsRefusedRatherThanSentToThePlatform(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Create(context.Background(), "Bearer t", CreateRequest{
		UserSub: developer,
		Series:  []SeriesSpec{powerSeries("convert:urn:infai:ses:characteristic:celsius")},
		Window:  window(),
	})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("an unreachable conversion was accepted: %v", err)
	}
	if h.timeseries.calls != 0 {
		t.Error("the platform was queried for a conversion ODE knows cannot be evaluated")
	}
}

// A variable with no characteristic cannot be converted, and inventing one is what
// D29 forbids in as many words.
func TestAConversionNeedsASourceCharacteristic(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Create(context.Background(), "Bearer t", CreateRequest{
		UserSub: developer,
		Series: []SeriesSpec{{
			Ref:       profiler.SeriesRef{DeviceID: deviceID, ServiceID: serviceID, VariablePath: energyPath},
			Transform: "convert:" + kilowattID,
		}},
		Window: window(),
	})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("a conversion without a source characteristic was accepted: %v", err)
	}
}

func TestABucketIsWidenedRatherThanTheWindowTruncated(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{
		Series:    []SeriesSpec{powerSeries("")},
		Window:    profiler.Window{From: chartTo.AddDate(0, 0, -365), To: chartTo},
		GroupTime: "1m",
	})
	data, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
		ChartID: created.Spec.ChartID, UserSub: developer,
	})
	if err != nil {
		t.Fatalf("Data: %v", err)
	}
	if data.GroupTime == "1m" {
		t.Error("a year at one-minute buckets was read as asked; the point cap did not bite")
	}
	if !data.Window.From.Equal(chartTo.AddDate(0, 0, -365)) {
		t.Error("the window was truncated instead of the bucket widened")
	}
	if len(data.Notes) == 0 {
		t.Error("the widening is not reported, so the chart claims a resolution it does not have")
	}
}

func TestANonNumericVariableSaysSoRatherThanDrawingNothing(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{
		Series: []SeriesSpec{{Ref: profiler.SeriesRef{
			DeviceID: deviceID, ServiceID: serviceID, VariablePath: statusPath,
		}}},
		Window: window(),
	})
	if len(created.Series[0].Notes) == 0 {
		t.Error("a string variable is charted silently; the empty line has no explanation")
	}
}

// --- annotations (§5.10) ---

func TestAProfileContributesItsDetectionsAsAnnotations(t *testing.T) {
	h := newHarness(t)
	profile := storedProfile(t, h, powerPath)

	created := h.create(t, CreateRequest{Series: []SeriesSpec{{
		Ref:       profiler.SeriesRef{DeviceID: deviceID, ServiceID: serviceID, VariablePath: powerPath},
		ProfileID: profile.ProfileID,
	}}, Window: window()})

	data, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
		ChartID: created.Spec.ChartID, UserSub: developer,
	})
	if err != nil {
		t.Fatalf("Data: %v", err)
	}

	bySource := map[string]Annotation{}
	for _, annotation := range data.Annotations {
		bySource[annotation.Source] = annotation
	}
	for _, source := range []string{SourceSessions, SourceGaps, SourceExclusions, SourceUsableRange} {
		annotation, found := bySource[source]
		if !found {
			t.Errorf("no annotation from %s", source)
			continue
		}
		if annotation.Author != AuthorProfiler {
			t.Errorf("%s is authored by %q, want the profiler", source, annotation.Author)
		}
		if !annotation.Confirmable || annotation.FieldPath == "" {
			t.Errorf("%s is not confirmable, so §5.10's control has nothing to act on", source)
		}
		if _, confirmable := profiler.ConfirmablePaths[annotation.FieldPath]; !confirmable {
			t.Errorf("%s names field %q, which the profiler will refuse", source, annotation.FieldPath)
		}
	}
	if len(data.Markers) == 0 || data.Markers[0].Source != SourceResets {
		t.Errorf("markers = %+v, want the counter reset §5.9's own example shows", data.Markers)
	}
}

// An annotation the profiler produced and an annotation a model wrote are different
// kinds of claim, and a model must not be able to dress one as the other.
func TestASpecificationCannotClaimProfilerAuthorship(t *testing.T) {
	h := newHarness(t)
	index := 0
	created := h.create(t, CreateRequest{
		Author: AuthorLLM,
		Series: []SeriesSpec{powerSeries("")},
		Annotations: []Annotation{{
			From: chartFrom.Add(time.Hour), To: chartFrom.Add(2 * time.Hour),
			Label: "detected session", Source: SourceSessions,
			Author: AuthorProfiler, Confirmable: true,
			FieldPath: profiler.FieldSessions, SeriesIndex: &index,
		}},
		Window: window(),
	})
	if got := created.Spec.Annotations[0].Author; got != AuthorLLM {
		t.Errorf("annotation author = %q, want llm — the backend stamps it", got)
	}
}

func TestAConfirmedFieldStopsAskingAgain(t *testing.T) {
	h := newHarness(t)
	profile := storedProfile(t, h, powerPath)
	if _, err := h.profiles.AppendOverride(profiler.ProfileOverride{
		SeriesRef: profile.SeriesRef, ProfileID: profile.ProfileID, CreatedBy: developer,
		CreatedAt: chartTo, FieldPath: profiler.FieldSessions, Action: profiler.ActionConfirm,
	}); err != nil {
		t.Fatalf("AppendOverride: %v", err)
	}

	created := h.create(t, CreateRequest{Series: []SeriesSpec{{
		Ref:       profile.SeriesRef,
		ProfileID: profile.ProfileID,
	}}, Window: window()})
	data, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
		ChartID: created.Spec.ChartID, UserSub: developer,
	})
	if err != nil {
		t.Fatalf("Data: %v", err)
	}
	for _, annotation := range data.Annotations {
		if annotation.Source == SourceSessions && annotation.Confirmable {
			t.Error("a session band is still offered for confirmation after the developer confirmed it")
		}
	}
}

func TestCappingAnnotationsIsReportedAndKeepsTheSevereOnes(t *testing.T) {
	annotations := []Annotation{}
	for i := 0; i < 10; i++ {
		annotations = append(annotations, Annotation{
			From: chartFrom.Add(time.Duration(i) * time.Hour), Severity: SeverityInfo,
		})
	}
	annotations = append(annotations, Annotation{
		From: chartFrom.Add(20 * time.Hour), Severity: SeverityError, Label: "excluded",
	})

	kept, dropped := capAnnotations(annotations, 3)
	if dropped != 8 {
		t.Errorf("dropped = %d, want 8", dropped)
	}
	found := false
	for _, annotation := range kept {
		if annotation.Severity == SeverityError {
			found = true
		}
	}
	if !found {
		t.Error("the error band was capped away behind informational ones")
	}
	for i := 1; i < len(kept); i++ {
		if kept[i].From.Before(kept[i-1].From) {
			t.Error("what survived is not in chronological order")
		}
	}
}

// --- confirmation (§5.10, D21) ---

func TestConfirmingAUnitCorrectsTheAxisAndRecordsWhatTheResolverSaid(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{Series: []SeriesSpec{powerSeries("")}, Window: window()})

	confirmed, err := h.service.Confirm(context.Background(), "Bearer t", ConfirmRequest{
		ChartID: created.Spec.ChartID, UserSub: developer, SeriesIndex: 0,
		FieldPath: profiler.FieldUnit, Action: profiler.ActionCorrect, ConfirmedValue: "kW",
		Note: "the meter reports kilowatts",
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if got := confirmed.Override.ComputedValue; got != "W" {
		t.Errorf("computed_value = %v, want W — the record has to stay diffable (§5.4.3)", got)
	}
	if confirmed.Series.Unit.Unit != "kW" || !confirmed.Series.Unit.Confirmed {
		t.Errorf("unit after confirmation = %+v, want a confirmed kW", confirmed.Series.Unit)
	}
	if confirmed.Series.Unit.ComputedUnit != "W" {
		t.Errorf("computed unit = %q, want W kept beside the confirmation", confirmed.Series.Unit.ComputedUnit)
	}

	// And it is in the overlay, keyed by series, which is what makes it survive a
	// recomputation and reach the next profile of the same series.
	overrides := h.profiles.Overrides(profiler.SeriesRef{
		DeviceID: deviceID, ServiceID: serviceID, VariablePath: powerPath,
	})
	if len(overrides) != 1 || overrides[0].FieldPath != profiler.FieldUnit {
		t.Fatalf("overlay = %+v, want one unit override", overrides)
	}

	data, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
		ChartID: created.Spec.ChartID, UserSub: developer,
	})
	if err != nil {
		t.Fatalf("Data: %v", err)
	}
	if data.Axis.Unit.Unit != "kW" {
		t.Errorf("axis = %q after the correction, want kW", data.Axis.Unit.Unit)
	}
	if len(data.Series[0].Confirmations) != 1 {
		t.Error("the chart does not carry the confirmation beside the series it applies to")
	}
}

// A series nobody has profiled can still have its unit confirmed: the overlay is
// keyed by series reference, so the decision waits for the profile rather than the
// other way round.
func TestAUnitCanBeConfirmedBeforeAnythingIsProfiled(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{
		Series: []SeriesSpec{{Ref: profiler.SeriesRef{
			DeviceID: deviceID, ServiceID: serviceID, VariablePath: energyPath,
		}}},
		Window: window(),
	})
	confirmed, err := h.service.Confirm(context.Background(), "Bearer t", ConfirmRequest{
		ChartID: created.Spec.ChartID, UserSub: developer, SeriesIndex: 0,
		FieldPath: profiler.FieldCharacteristic, Action: profiler.ActionCorrect, ConfirmedValue: wattID,
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if confirmed.Override.ProfileID != "" {
		t.Errorf("profile_id = %q, want empty: there is no profile", confirmed.Override.ProfileID)
	}
	// Correcting the canonical key is what makes conversion possible at all (D29).
	if confirmed.Series.Unit.Unit != "W" {
		t.Errorf("unit = %q, want W to follow from the corrected characteristic", confirmed.Series.Unit.Unit)
	}
	if len(confirmed.Series.Unit.AvailableConversions) == 0 {
		t.Error("no conversions after the characteristic was corrected, so the correction bought nothing")
	}
}

func TestConfirmationIsRefusedOutsideTheClosedSet(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{Series: []SeriesSpec{powerSeries("")}, Window: window()})

	_, err := h.service.Confirm(context.Background(), "Bearer t", ConfirmRequest{
		ChartID: created.Spec.ChartID, UserSub: developer, SeriesIndex: 0,
		FieldPath: "distribution.mean", Action: profiler.ActionConfirm,
	})
	if !errors.Is(err, ErrNotConfirmable) {
		t.Fatalf("a confirmation of a computed statistic was accepted: %v", err)
	}

	if _, err := h.service.Confirm(context.Background(), "Bearer t", ConfirmRequest{
		ChartID: created.Spec.ChartID, UserSub: developer, SeriesIndex: 4,
		FieldPath: profiler.FieldUnit, Action: profiler.ActionConfirm,
	}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("a confirmation of a series outside the chart was accepted: %v", err)
	}
}

func TestConfirmationsSupersedeRatherThanReplace(t *testing.T) {
	h := newHarness(t)
	created := h.create(t, CreateRequest{Series: []SeriesSpec{powerSeries("")}, Window: window()})

	for _, value := range []string{"kW", "MW"} {
		if _, err := h.service.Confirm(context.Background(), "Bearer t", ConfirmRequest{
			ChartID: created.Spec.ChartID, UserSub: developer, SeriesIndex: 0,
			FieldPath: profiler.FieldUnit, Action: profiler.ActionCorrect, ConfirmedValue: value,
		}); err != nil {
			t.Fatalf("Confirm %s: %v", value, err)
		}
	}

	overrides := h.profiles.Overrides(profiler.SeriesRef{
		DeviceID: deviceID, ServiceID: serviceID, VariablePath: powerPath,
	})
	if len(overrides) != 2 {
		t.Errorf("%d overrides, want both kept: the overlay is append-only (§5.4.3)", len(overrides))
	}
	data, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
		ChartID: created.Spec.ChartID, UserSub: developer,
	})
	if err != nil {
		t.Fatalf("Data: %v", err)
	}
	if data.Axis.Unit.Unit != "MW" {
		t.Errorf("axis = %q, want the later correction MW", data.Axis.Unit.Unit)
	}
}

// --- the store ---

func TestTheStoreBoundsOneDeveloperWithoutTouchingAnother(t *testing.T) {
	store := NewMemoryStore(2)
	for i := 0; i < 3; i++ {
		store.Put(Spec{
			ChartID:   "chart-" + strconv.Itoa(i),
			CreatedBy: developer,
			CreatedAt: chartFrom.Add(time.Duration(i) * time.Minute),
		})
	}
	store.Put(Spec{ChartID: "other", CreatedBy: "someone-else", CreatedAt: chartFrom})

	if _, found := store.ByID("chart-0"); found {
		t.Error("the oldest chart survived the bound")
	}
	if got := len(store.ForUser(developer, "", 0)); got != 2 {
		t.Errorf("%d charts for the developer, want the bound of 2", got)
	}
	if _, found := store.ByID("other"); !found {
		t.Error("another developer's chart was evicted by this one's bound")
	}
}

func TestChartsAreListedNewestFirstAndPerSession(t *testing.T) {
	h := newHarness(t)
	first := h.create(t, CreateRequest{Series: []SeriesSpec{powerSeries("")}, Window: window()})
	h.service.deps.Now = func() time.Time { return chartTo.Add(time.Minute) }
	second := h.create(t, CreateRequest{
		SessionID: "sess-1", Series: []SeriesSpec{powerSeries("")}, Window: window(),
	})

	listed := h.service.List(developer, "", 0)
	if len(listed) != 2 || listed[0].ChartID != second.Spec.ChartID {
		t.Errorf("listing = %+v, want the newest first", listed)
	}
	scoped := h.service.List(developer, "sess-1", 0)
	if len(scoped) != 1 || scoped[0].ChartID != second.Spec.ChartID {
		t.Errorf("session listing = %+v, want only the chart of that session", scoped)
	}
	if err := h.service.Delete(first.Spec.ChartID, developer); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(h.service.List(developer, "", 0)) != 1 {
		t.Error("the discarded chart is still listed")
	}
}

// --- helpers ---

// storedProfile puts a profile in the store carrying one of each detection a chart
// annotates, so the derivation is tested against the profile shape rather than
// against a real detector run.
func storedProfile(t *testing.T, h *harness, path string) profiler.SeriesProfile {
	t.Helper()
	ref := profiler.SeriesRef{DeviceID: deviceID, ServiceID: serviceID, VariablePath: path}
	profile := profiler.SeriesProfile{
		ProfileID:       "profile-1",
		Tier:            profiler.TierFull,
		SeriesRef:       ref,
		DetectorVersion: profiler.DetectorVersion,
		AnalysisWindow:  profiler.Window{From: chartFrom, To: chartTo},
		Sampling: profiler.Computed(profiler.Sampling{
			DetectedIntervalS: 900,
			Gaps: []profiler.Gap{{
				From: chartFrom.Add(6 * time.Hour), To: chartFrom.Add(9 * time.Hour),
				DurationS: 10800, Classification: profiler.GapDeviceOffline,
			}},
		}),
		ValueSemantics: profiler.ValueSemantics{
			Kind:          profiler.Computed(profiler.KindCumulativeCounter),
			CounterResets: profiler.Computed([]time.Time{chartFrom.Add(30 * time.Hour)}),
		},
		Recommendations: profiler.Recommendations{
			Advisory: true,
			UsableRange: profiler.Computed(profiler.Window{
				From: chartFrom.Add(12 * time.Hour), To: chartTo,
			}),
			Exclusions: []profiler.Exclusion{{
				From: chartFrom, To: chartFrom.Add(3 * time.Hour), Reason: "frozen sensor",
			}},
		},
	}
	sessions := []profiler.Session{
		{From: chartFrom.Add(time.Hour), To: chartFrom.Add(2 * time.Hour), DurationS: 3600, Peak: 1200},
		{From: chartFrom.Add(25 * time.Hour), To: chartFrom.Add(26 * time.Hour), DurationS: 3600, Peak: 900},
	}
	stored, _, err := h.profiles.Put(profile, sessions)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return stored
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// A rate over a long bucket was rejected by the platform, and took the whole
// chart with it.
//
// The divisor is written into the server's `math` field, which it validates
// against `^([+\-*/])\d+(\.\d+)?$` — a pattern with no exponent. Rendering the
// bucket length with %g switched to exponential notation at 1e6, so every bucket
// longer than about eleven and a half days produced a request the platform
// refused. Because §5.3.1 sends every series of a chart in one batch, one rate
// series failing meant no series was drawn at all.
//
// TestEveryTransformIsResolvedIntoAQueryField already asserted element.Valid(),
// which is the check that catches this — but only at an hourly bucket, where the
// rendering happens to be safe. The bucket, not the transform, is the variable
// that mattered, so this case names one long enough to show it.
func TestARateOverALongBucketIsStillAcceptedByTheSharedSchema(t *testing.T) {
	for _, bucket := range []string{"1h", "14d", "30d"} {
		t.Run(bucket, func(t *testing.T) {
			h := newHarness(t)
			created := h.create(t, CreateRequest{
				Series:    []SeriesSpec{powerSeries("rate")},
				Window:    window(),
				GroupTime: bucket,
			})
			// The elements are built for the read, so the chart has to be drawn
			// before there is anything to assert against.
			if _, err := h.service.Data(context.Background(), "Bearer t", DataRequest{
				ChartID: created.Spec.ChartID, UserSub: developer,
			}); err != nil {
				t.Fatalf("Data: %v", err)
			}

			elements := h.timeseries.elements
			if len(elements) != 1 {
				t.Fatalf("query elements = %d, want 1", len(elements))
			}
			math := deref(elements[0].Columns[0].Math)
			if strings.ContainsAny(math, "eE") {
				t.Errorf("math = %q: the platform's own pattern has no exponent", math)
			}
			if !elements[0].Valid() {
				t.Errorf("a rate at %s is rejected by the shared schema: math = %q", bucket, math)
			}
		})
	}
}

// A bucket finer than a whole second used to become "0s".
//
// FormatBucket rendered sub-second durations as whole seconds, so 500ms
// truncated to zero — and "0s" is a form the server's own regex accepts, which
// meant nothing between here and Postgres would refuse it. It reached the store
// as time_bucket('0 seconds', ...). The point cap did not save it either: Bucket
// treated a parsed-but-zero request the same as an unparseable one and passed it
// straight through.
func TestASubSecondResampleIsRefusedRatherThanRoundedToNothing(t *testing.T) {
	for _, interval := range []string{"500ms", "1ms", "90500ms"} {
		t.Run(interval, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.service.Create(context.Background(), "Bearer t", CreateRequest{
				UserSub: developer,
				Series:  []SeriesSpec{powerSeries("resample:" + interval)},
				Window:  window(),
			})
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("resample:%s was accepted: %v", interval, err)
			}
			if h.timeseries.calls != 0 {
				t.Error("the platform was queried with a bucket ODE cannot express")
			}
		})
	}
}
