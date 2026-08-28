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
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

const testExportID = "urn:infai:ses:export:1"

type fakeExports struct {
	definition ExportDefinition
	err        error
	asked      []string
}

func (f *fakeExports) ExportDefinition(_ context.Context, _ string, exportID string) (ExportDefinition, error) {
	f.asked = append(f.asked, exportID)
	if f.err != nil {
		return ExportDefinition{}, f.err
	}
	definition := f.definition
	if definition.ExportID == "" {
		definition.ExportID = exportID
	}
	return definition, nil
}

// weatherExport is an import export: two readable columns whose paths and
// characteristics come from the import type, one tag, and — the interesting part —
// a column name that is not its variable path.
func weatherExport() ExportDefinition {
	watt := "ch-watt"
	return ExportDefinition{
		ExportID: testExportID,
		Name:     "weather history",
		Source:   "import_id",
		SourceID: "urn:infai:ses:import:9",
		Columns: []ExportColumn{
			{
				Column: "irradiance", Type: "float", VariablePath: "value.ghi",
				CharacteristicID: &watt, FunctionID: "fn-power", AspectID: "aspect-pv",
			},
			{Column: "station", Type: "string", VariablePath: "value.station", Tag: true},
		},
	}
}

func exportProfiler(t *testing.T, ts TimeseriesClient, exports ExportSource, now time.Time) *Profiler {
	t.Helper()
	prof, err := New(ts, fakeOntology{index: powerOntology()}, NewMemoryStore(), Options{
		Now:     func() time.Time { return now },
		Exports: exports,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return prof
}

// countResult renders the wire shape a bucketed count comes back in: one wide
// table, [time, c1, c2, …], which is what the server produces for a grouped
// element — the columns are joined server-side rather than split per column.
func countResult(columns []string, times []time.Time, counts map[string][]float64) timeseries.QueryResult {
	export := testExportID
	rows := make([][]any, 0, len(times))
	for i, at := range times {
		row := []any{at.UTC().Format("2006-01-02T15:04:05.000Z07:00")}
		for _, name := range columns {
			series := counts[name]
			var value any
			if i < len(series) {
				value = json.Number(strconv.FormatFloat(series[i], 'f', -1, 64))
			}
			row = append(row, value)
		}
		rows = append(rows, row)
	}
	return timeseries.QueryResult{
		RequestIndex: 0,
		ExportId:     &export,
		ColumnNames:  columns,
		Data:         [][][]any{rows},
	}
}

// --- ExportVariables ---

func TestExportVariablesAddressTheColumnRatherThanTheVariablePath(t *testing.T) {
	variables := ExportVariables(weatherExport().Columns)
	if len(variables) != 2 {
		t.Fatalf("variables = %d, want one per column", len(variables))
	}

	var irradiance Variable
	for _, variable := range variables {
		if variable.Path == "irradiance" {
			irradiance = variable
		}
	}
	if irradiance.Path != "irradiance" {
		t.Fatalf("no variable addresses the column: %+v", variables)
	}
	// The path is the column, because that is what timescale-wrapper takes as
	// columns[].name for an export. Addressing "value.ghi" would name a column the
	// table does not have.
	if irradiance.Type != models.Float || !irradiance.Numeric() {
		t.Errorf("type = %q, want the float the export declares", irradiance.Type)
	}
	if irradiance.CharacteristicID != "ch-watt" || irradiance.FunctionID != "fn-power" {
		t.Errorf("semantics = %q/%q, want the import type's", irradiance.CharacteristicID, irradiance.FunctionID)
	}
	if !irradiance.Queryable {
		t.Errorf("the column is not queryable: %s", irradiance.Reason)
	}
	if !irradiance.Streamed() {
		t.Error("an export column must count as streamed: it is fed from a Kafka topic")
	}
	if irradiance.ServiceID != "" {
		t.Errorf("service id = %q, want none — an export has no service", irradiance.ServiceID)
	}
}

// A column ODE cannot read is reported as unqueryable with a reason rather than
// dropped: it exists in the table, and a developer looking for it needs to know
// it was seen.
func TestAnUnreadableExportColumnIsReportedRatherThanDropped(t *testing.T) {
	variables := ExportVariables([]ExportColumn{
		{Column: "fine", Type: "float"},
		{Column: "shape", Type: "structure"},
		{Column: "not a column", Type: "float"},
		{Column: "  ", Type: "float"},
	})
	if len(variables) != 4 {
		t.Fatalf("variables = %d, want every column reported", len(variables))
	}
	byPath := map[string]Variable{}
	for _, variable := range variables {
		byPath[variable.Path] = variable
	}
	if !byPath["fine"].Queryable {
		t.Error("a float column must be queryable")
	}
	if v := byPath["shape"]; v.Queryable || v.Reason != reasonUnknownExportType {
		t.Errorf("unknown type = %+v, want unqueryable with the type reason", v)
	}
	if v := byPath["not a column"]; v.Queryable || v.Reason != reasonBadPath {
		t.Errorf("bad column name = %+v, want unqueryable with the path reason", v)
	}
	if v := byPath[""]; v.Queryable || v.Reason == "" {
		t.Errorf("nameless column = %+v, want unqueryable with a reason", v)
	}
}

// The device form of a series reference has to stay byte-identical: it is the
// cache key and the override lookup key, so a change to it would silently
// invalidate every stored profile and orphan every confirmation a developer has
// made.
func TestTheDeviceSeriesReferenceIsUnchangedByTheExportForm(t *testing.T) {
	device := SeriesRef{DeviceID: "d1", ServiceID: "s1", VariablePath: "value.power"}
	if device.String() != "d1|s1|value.power" {
		t.Errorf("device key = %q, want the unchanged three-part form", device.String())
	}
	if device.IsExport() {
		t.Error("a device reference must not read as an export")
	}

	export := SeriesRef{ExportID: "e1", VariablePath: "value.power"}
	if export.String() == device.String() {
		t.Error("an export and a device with the same path share a key, so they would share a profile")
	}
	if !export.Valid() || !export.IsExport() {
		t.Errorf("%+v does not read as a valid export reference", export)
	}

	// Both set is an ambiguous reference rather than a richer one, and it is the
	// shape timescale-wrapper's own schema refuses.
	mixed := SeriesRef{DeviceID: "d1", ServiceID: "s1", ExportID: "e1", VariablePath: "value.power"}
	if mixed.Valid() {
		t.Error("a reference carrying both a device and an export must not be valid")
	}
	if (SeriesRef{ExportID: "e1"}).Valid() {
		t.Error("an export reference with no variable path must not be valid")
	}
}

// --- ExportFill ---

func TestExportFillCountsRowsWithoutReadingAValue(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	times := []time.Time{now.AddDate(0, 0, -60), now.AddDate(0, 0, -30)}
	fake := &fakeTimeseries{
		exportUsage: map[string]timeseries.Usage{
			testExportID: {ExportId: testExportID, Bytes: 4096, BytesPerDay: 64},
		},
		results: [][]timeseries.QueryResult{{
			countResult([]string{"irradiance", "station"}, times, map[string][]float64{
				"irradiance": {100, 120},
				"station":    {100, 120},
			}),
		}},
	}
	prof := exportProfiler(t, fake, &fakeExports{definition: weatherExport()}, now)

	fill, err := prof.ExportFill(context.Background(), "Bearer caller", ExportFillRequest{ExportID: testExportID})
	if err != nil {
		t.Fatalf("ExportFill: %v", err)
	}
	if fill.State != ExportFilled {
		t.Fatalf("state = %q (%s), want filled", fill.State, fill.Reason)
	}
	if fill.Rows != 220 {
		t.Errorf("rows = %d, want the counted 220", fill.Rows)
	}
	if fill.Reason == "" {
		t.Error("the reason is empty; every state has to say why")
	}

	// The one property that decides the exposure tier: the request asked for
	// counts, so nothing it can answer with is a value.
	if len(fake.queries) != 1 {
		t.Fatalf("queries = %d, want exactly one", len(fake.queries))
	}
	element := fake.queries[0][0]
	if element.ExportId == nil || *element.ExportId != testExportID {
		t.Errorf("element = %+v, want it addressed at the export", element)
	}
	if element.DeviceId != nil || element.ServiceId != nil {
		t.Error("the element carries a device or service beside the export, which the platform refuses")
	}
	if element.GroupTime == nil || *element.GroupTime == "" {
		t.Error("no groupTime: count is only accepted as an aggregate")
	}
	for _, column := range element.Columns {
		if column.GroupType == nil || *column.GroupType != timeseries.GroupCount {
			t.Errorf("column %s asks for %v, want count", column.Name, column.GroupType)
		}
	}

	usage := mustGet(t, fill.Usage, "usage")
	if usage.Bytes != 4096 {
		t.Errorf("bytes = %d, want what /usage/exports reported", usage.Bytes)
	}
	span := mustGet(t, fill.Span, "span")
	if !span.From.Equal(times[0]) {
		t.Errorf("span starts %s, want the first counted bucket %s", span.From, times[0])
	}
	// The last bucket is named by its start and runs to its own end, so the span
	// reaches past it — but never past now.
	if !span.To.After(times[1]) || span.To.After(now) {
		t.Errorf("span ends %s, want it past %s and no later than %s", span.To, times[1], now)
	}
}

// The failure this whole probe exists for: rows arrive, the timestamp resolves,
// and a value path names nothing the message carries — so the column is null in
// every row. Bytes are stored and the export listing looks healthy.
func TestAColumnNullInEveryRowIsReportedAsPartlyFilled(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	times := []time.Time{now.AddDate(0, 0, -10)}
	fake := &fakeTimeseries{
		results: [][]timeseries.QueryResult{{
			countResult([]string{"irradiance", "station"}, times, map[string][]float64{
				"irradiance": {0},
				"station":    {500},
			}),
		}},
	}
	prof := exportProfiler(t, fake, &fakeExports{definition: weatherExport()}, now)

	fill, err := prof.ExportFill(context.Background(), "Bearer caller", ExportFillRequest{ExportID: testExportID})
	if err != nil {
		t.Fatalf("ExportFill: %v", err)
	}
	if fill.State != ExportPartlyFilled {
		t.Fatalf("state = %q (%s), want partly_filled", fill.State, fill.Reason)
	}
	if fill.Rows != 500 {
		t.Errorf("rows = %d, want the fullest column's 500", fill.Rows)
	}

	byColumn := map[string]ExportColumnFill{}
	for _, column := range fill.Columns {
		byColumn[column.Column] = column
	}
	if column := byColumn["irradiance"]; !column.Counted || !column.Empty {
		t.Errorf("irradiance = %+v, want counted and empty", column)
	}
	if column := byColumn["station"]; column.Empty {
		t.Errorf("station = %+v, want it not empty", column)
	}
	// The reason has to name the columns, because "partly filled" alone does not
	// tell a developer which value path to go and fix.
	if !strings.Contains(fill.Reason, "irradiance") {
		t.Errorf("reason = %q, want it to name the null column", fill.Reason)
	}
}

// The export worker's classic misconfiguration, and the case a count per column is
// the only way to see: every value path resolves against the message root, so the
// timestamp is found and nothing else. Rows land, bytes are stored, the export
// listing is healthy, and every column is null.
//
// It must not be reported as empty. "Nothing was written" sends a developer to the
// topic; this sends them to the value paths.
func TestAnExportWhoseEveryColumnIsNullIsNotReportedAsEmpty(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	times := []time.Time{now.AddDate(0, 0, -20), now.AddDate(0, 0, -10)}
	// Buckets came back — the server groups by the bucket, so a bucket exists only
	// where a row does — and every count in them is zero.
	fake := &fakeTimeseries{
		results: [][]timeseries.QueryResult{{
			countResult([]string{"irradiance", "station"}, times, map[string][]float64{
				"irradiance": {0, 0},
				"station":    {0, 0},
			}),
		}},
	}
	prof := exportProfiler(t, fake, &fakeExports{definition: weatherExport()}, now)

	fill, err := prof.ExportFill(context.Background(), "Bearer caller", ExportFillRequest{ExportID: testExportID})
	if err != nil {
		t.Fatalf("ExportFill: %v", err)
	}
	if fill.State != ExportPartlyFilled {
		t.Fatalf("state = %q (%s), want partly_filled, not empty", fill.State, fill.Reason)
	}
	if fill.Rows != 0 {
		t.Errorf("rows = %d, want none: no column carries a value", fill.Rows)
	}
	if fill.BucketsWithRows != 2 {
		t.Errorf("buckets_with_rows = %d, want the two that came back", fill.BucketsWithRows)
	}
	if !fill.Span.IsComputed() {
		t.Error("rows exist, so the span is known even though no column carries a value")
	}
	// The reason has to point at the paths rather than at the topic.
	if !strings.Contains(fill.Reason, "message root") {
		t.Errorf("reason = %q, want it to name the path failure", fill.Reason)
	}
}

func TestAnExportWithNoRowsIsEmptyAndSaysWhatToCheck(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeTimeseries{results: [][]timeseries.QueryResult{{}}}
	prof := exportProfiler(t, fake, &fakeExports{definition: weatherExport()}, now)

	fill, err := prof.ExportFill(context.Background(), "Bearer caller", ExportFillRequest{ExportID: testExportID})
	if err != nil {
		t.Fatalf("ExportFill: %v", err)
	}
	if fill.State != ExportEmpty {
		t.Fatalf("state = %q (%s), want empty", fill.State, fill.Reason)
	}
	if fill.Rows != 0 {
		t.Errorf("rows = %d, want none", fill.Rows)
	}
	if fill.Span.IsComputed() {
		t.Error("an empty export has no data span, and reporting one would invent it")
	}
	// The default probe window is deliberately long: an export that stopped
	// receiving rows a year ago must not come back empty.
	if fill.Window.SpanDays() < 365 {
		t.Errorf("window = %s, want the multi-year default", fill.Window)
	}
}

// A missing usage row and a failed count are the two ways of not knowing, and
// neither may be reported as an empty export.
func TestAFailedCountIsUnknownRatherThanEmpty(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeTimeseries{queryErr: errors.New("upstream fell over")}
	prof := exportProfiler(t, fake, &fakeExports{definition: weatherExport()}, now)

	fill, err := prof.ExportFill(context.Background(), "Bearer caller", ExportFillRequest{ExportID: testExportID})
	if err != nil {
		t.Fatalf("ExportFill: %v", err)
	}
	if fill.State != ExportFillUnknown {
		t.Fatalf("state = %q, want unknown", fill.State)
	}
	if !strings.Contains(fill.Reason, "upstream fell over") {
		t.Errorf("reason = %q, want the platform's own error in it", fill.Reason)
	}
	if fill.Usage.IsComputed() {
		t.Error("usage was not stubbed, so it must report not_computed rather than zero bytes")
	}
	status := fill.Usage.Status()
	if status.Reason != ReasonInsufficientCoverage {
		t.Errorf("usage status = %+v, want the absent-row reason", status)
	}
	if !strings.Contains(status.Detail, "not evidence") {
		t.Errorf("usage detail = %q, want it to say an absent row proves nothing", status.Detail)
	}
}

func TestExportFillRefusesWithoutAnExportSource(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	prof := newTestProfiler(t, &fakeTimeseries{}, powerOntology(), now)
	if _, err := prof.ExportFill(context.Background(), "Bearer caller",
		ExportFillRequest{ExportID: testExportID}); !errors.Is(err, ErrNoExportSource) {
		t.Errorf("error = %v, want ErrNoExportSource", err)
	}
	if _, err := prof.ProfileExport(context.Background(), "Bearer caller",
		ExportProfileRequest{ExportID: testExportID}); !errors.Is(err, ErrNoExportSource) {
		t.Errorf("error = %v, want ErrNoExportSource", err)
	}
}

// --- ProfileExport ---

func TestProfileExportProfilesEveryColumnUnderAnExportSeriesRef(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	start := now.Add(-100 * quarterHour)
	times, values := regularSeries(start, quarterHour, 100, func(i int) float64 { return 100 + float64(i) })

	countTimes := []time.Time{start}
	fake := &fakeTimeseries{
		results: [][]timeseries.QueryResult{
			// The counting probe, which stands in for /data-availability.
			{countResult([]string{"irradiance"}, countTimes, map[string][]float64{"irradiance": {100}})},
			// The raw pass.
			{exportQueryResult(0, []string{"irradiance"}, times, map[string][]float64{"irradiance": values})},
			// The aggregated pass: mean, min and max, one element each.
			{
				exportQueryResult(0, []string{"irradiance"}, times, map[string][]float64{"irradiance": values}),
				exportQueryResult(1, []string{"irradiance"}, times, map[string][]float64{"irradiance": values}),
				exportQueryResult(2, []string{"irradiance"}, times, map[string][]float64{"irradiance": values}),
			},
		},
	}
	definition := weatherExport()
	// One readable column keeps the fixture legible; the tag column would be
	// profiled the same way.
	definition.Columns = definition.Columns[:1]
	prof := exportProfiler(t, fake, &fakeExports{definition: definition}, now)

	result, err := prof.ProfileExport(context.Background(), "Bearer caller", ExportProfileRequest{
		ExportID: testExportID,
	})
	if err != nil {
		t.Fatalf("ProfileExport: %v", err)
	}
	if len(result.Profiles) != 1 {
		t.Fatalf("profiles = %d, want one per readable column", len(result.Profiles))
	}

	ref := result.Profiles[0].SeriesRef
	if ref.ExportID != testExportID || ref.VariablePath != "irradiance" {
		t.Errorf("series ref = %+v, want the export and the column", ref)
	}
	if ref.DeviceID != "" || ref.ServiceID != "" {
		t.Errorf("series ref = %+v, want no device or service on an export profile", ref)
	}
	if !ref.Valid() || !ref.IsExport() {
		t.Errorf("series ref %+v does not read as a valid export reference", ref)
	}

	// The unit comes from the characteristic the import type declared, which is the
	// whole reason the column carries one.
	profile := result.Profiles[0].SeriesProfile
	if profile.ValueSemantics.Unit != "W" {
		t.Errorf("unit = %q, want the watt the characteristic declares", profile.ValueSemantics.Unit)
	}
	if profile.ReadSummary.RawRows != len(times) {
		t.Errorf("raw rows = %d, want the %d read", profile.ReadSummary.RawRows, len(times))
	}
	if raw, ok := profile.ReadSummary.RawAvailable.Get(); !ok || !raw {
		t.Errorf("raw_available = %+v, want true: an export's table is unbucketed by construction",
			profile.ReadSummary.RawAvailable.Status())
	}

	// Every read is addressed at the export and none at a device.
	for i, batch := range fake.queries {
		for _, element := range batch {
			if element.ExportId == nil {
				t.Errorf("query %d has an element with no export id: %+v", i, element)
			}
			if element.DeviceId != nil || element.ServiceId != nil {
				t.Errorf("query %d addressed a device: %+v", i, element)
			}
		}
	}
}

// An export with no rows is refused rather than profiled into a body of
// not_computed, and the refusal is the probe's own reason — which is the one
// place that says what to check.
func TestProfileExportRefusesAnEmptyExport(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeTimeseries{results: [][]timeseries.QueryResult{{}}}
	prof := exportProfiler(t, fake, &fakeExports{definition: weatherExport()}, now)

	_, err := prof.ProfileExport(context.Background(), "Bearer caller",
		ExportProfileRequest{ExportID: testExportID})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "no row was counted") {
		t.Errorf("error = %v, want the probe's own reason", err)
	}
}

func TestProfileExportRefusesAnExportWithNoReadableColumn(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	definition := ExportDefinition{
		ExportID: testExportID,
		Columns:  []ExportColumn{{Column: "shape", Type: "structure"}},
	}
	prof := exportProfiler(t, &fakeTimeseries{}, &fakeExports{definition: definition}, now)

	if _, err := prof.ProfileExport(context.Background(), "Bearer caller",
		ExportProfileRequest{ExportID: testExportID}); !errors.Is(err, ErrNoVariables) {
		t.Errorf("error = %v, want ErrNoVariables", err)
	}
}

// exportQueryResult is queryResult for an export: the same per-column wire shape,
// addressed by export id.
func exportQueryResult(requestIndex int, columns []string, times []time.Time, values map[string][]float64) timeseries.QueryResult {
	out := queryResult(requestIndex, "", "", columns, times, values)
	out.DeviceId, out.ServiceId = nil, nil
	export := testExportID
	out.ExportId = &export
	return out
}
