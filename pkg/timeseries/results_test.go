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

package timeseries_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// request is the element the decoder maps positions against. Column identity
// comes from here rather than from the response, because /queries/v2 names only
// the first column of a multi-column request.
func request(columns ...string) []timeseries.QueryElement {
	device, service := "urn:infai:ses:device:1", "urn:infai:ses:service:1"
	element := timeseries.QueryElement{DeviceId: &device, ServiceId: &service}
	for _, name := range columns {
		element.Columns = append(element.Columns, timeseries.QueryColumn{Name: name})
	}
	return []timeseries.QueryElement{element}
}

// result is one wide table, [time, c1, c2, …]. This is the shape a single-column
// read returns, and the shape the decoder falls back to when the sub-series count
// does not match the column count.
func result(rows [][]any, columns ...string) timeseries.QueryResult {
	device, service := "urn:infai:ses:device:1", "urn:infai:ses:service:1"
	return timeseries.QueryResult{
		RequestIndex: 0,
		DeviceId:     &device,
		ServiceId:    &service,
		ColumnNames:  columns,
		Data:         [][][]any{rows},
	}
}

// perColumn is what /queries/v2 actually returns for a device and service: one
// [time, value] series per requested column, in request order.
func perColumn(columns []string, series ...[][]any) timeseries.QueryResult {
	device, service := "urn:infai:ses:device:1", "urn:infai:ses:service:1"
	out := timeseries.QueryResult{
		RequestIndex: 0,
		DeviceId:     &device,
		ServiceId:    &service,
		// Deliberately only the first name, as the server sends it.
		ColumnNames: columns[:1],
	}
	out.Data = append(out.Data, series...)
	return out
}

func TestARowSetDecodesIntoTimestampsAndColumnMajorValues(t *testing.T) {
	sets, err := timeseries.DecodeResults(request("value.power", "value.total"), []timeseries.QueryResult{result([][]any{
		{"2026-06-01T00:00:00.000Z", json.Number("10"), json.Number("100")},
		{"2026-06-01T00:15:00.000Z", json.Number("11"), json.Number("110")},
	}, "value.power", "value.total")}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("sets = %d, want one", len(sets))
	}
	set := sets[0]

	if set.Rows() != 2 {
		t.Fatalf("rows = %d, want 2", set.Rows())
	}
	if set.DeviceID != "urn:infai:ses:device:1" || set.ServiceID != "urn:infai:ses:service:1" {
		t.Errorf("set identifies %s/%s, want the device and service from the response",
			set.DeviceID, set.ServiceID)
	}
	if !set.Times[0].Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("first timestamp = %s, want the first row's", set.Times[0])
	}

	power, ok := set.Column("value.power")
	if !ok || power.Len() != 2 {
		t.Fatalf("power column = %+v, want two values", power)
	}
	if got, _ := timeseries.ToFloat(power.Values[1]); got != 11 {
		t.Errorf("second power value = %v, want 11", got)
	}
}

// The raw pass orders descending so the point limit takes the newest points.
// Decoding sorts back, so the detectors always see a forward-running series.
func TestRowsAreSortedAscendingWhateverOrderTheyArriveIn(t *testing.T) {
	sets, err := timeseries.DecodeResults(request("value.power"), []timeseries.QueryResult{result([][]any{
		{"2026-06-01T02:00:00.000Z", json.Number("3")},
		{"2026-06-01T00:00:00.000Z", json.Number("1")},
		{"2026-06-01T01:00:00.000Z", json.Number("2")},
	}, "value.power")}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}

	column, _ := sets[0].Column("value.power")
	for i := 1; i < column.Len(); i++ {
		if !column.Times[i].After(column.Times[i-1]) {
			t.Fatalf("timestamps are not ascending: %v", column.Times)
		}
	}
	if got, _ := timeseries.ToFloat(column.Values[0]); got != 1 {
		t.Errorf("first value = %v, want the earliest row's 1", got)
	}
}

// A service message need not carry every variable it declares, so a NULL is
// normal. Per-column timestamps are the honest consequence: two variables of one
// service can have different point counts, and averaging that away would hide it.
func TestNullsAreDroppedPerColumnAndCounted(t *testing.T) {
	sets, err := timeseries.DecodeResults(request("value.power", "value.total"), []timeseries.QueryResult{result([][]any{
		{"2026-06-01T00:00:00.000Z", json.Number("10"), json.Number("100")},
		{"2026-06-01T00:15:00.000Z", nil, json.Number("110")},
		{"2026-06-01T00:30:00.000Z", json.Number("12"), nil},
	}, "value.power", "value.total")}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}

	power, _ := sets[0].Column("value.power")
	total, _ := sets[0].Column("value.total")
	if power.Len() != 2 || power.NullRows != 1 {
		t.Errorf("power = %d values with %d nulls, want 2 and 1", power.Len(), power.NullRows)
	}
	if total.Len() != 2 || total.NullRows != 1 {
		t.Errorf("total = %d values with %d nulls, want 2 and 1", total.Len(), total.NullRows)
	}
	// The row set keeps every row, so the shared row index still aligns the two
	// variables for the cross-variable checks.
	if sets[0].Rows() != 3 {
		t.Errorf("rows = %d, want all three kept", sets[0].Rows())
	}
}

// A row shorter than the named columns reads as NULL rather than as a decode
// failure: the alternative is refusing a whole profile over a truncated row.
func TestAShortRowReadsAsNull(t *testing.T) {
	sets, err := timeseries.DecodeResults(request("value.power", "value.total"), []timeseries.QueryResult{result([][]any{
		{"2026-06-01T00:00:00.000Z", json.Number("10")},
	}, "value.power", "value.total")}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	total, ok := sets[0].Column("value.total")
	if !ok {
		t.Fatal("the missing column is not reported at all")
	}
	if total.Len() != 0 || total.NullRows != 1 {
		t.Errorf("total = %d values with %d nulls, want 0 and 1", total.Len(), total.NullRows)
	}
}

func TestAnUnknownColumnIsReportedAsMissing(t *testing.T) {
	sets, err := timeseries.DecodeResults(request("value.power"), []timeseries.QueryResult{result([][]any{
		{"2026-06-01T00:00:00.000Z", json.Number("10")},
	}, "value.power")}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	if _, ok := sets[0].Column("value.nonexistent"); ok {
		t.Error("a column that was never requested was reported as present")
	}
}

// A categorical or status variable is not a broken one, so the caller is told how
// many values it could not read rather than being handed silence.
func TestNumericDropsNonNumericValuesAndCountsThem(t *testing.T) {
	sets, err := timeseries.DecodeResults(request("value.power"), []timeseries.QueryResult{result([][]any{
		{"2026-06-01T00:00:00.000Z", json.Number("10")},
		{"2026-06-01T00:15:00.000Z", map[string]any{"nested": true}},
		{"2026-06-01T00:30:00.000Z", json.Number("12")},
	}, "value.power")}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}

	column, _ := sets[0].Column("value.power")
	times, values, dropped := column.Numeric()
	if len(values) != 2 || dropped != 1 {
		t.Errorf("numeric = %d values with %d dropped, want 2 and 1", len(values), dropped)
	}
	if len(times) != len(values) {
		t.Errorf("times = %d, values = %d; they must stay paired", len(times), len(values))
	}
}

// A binary sensor has a duty cycle and a distribution, so booleans convert; the
// value-semantics detector decides whether reading it that way is meaningful.
func TestBooleansConvertToZeroAndOne(t *testing.T) {
	for value, want := range map[any]float64{true: 1, false: 0} {
		got, ok := timeseries.ToFloat(value)
		if !ok || got != want {
			t.Errorf("ToFloat(%v) = %v, %v; want %v", value, got, ok, want)
		}
	}
}

func TestToFloatRefusesWhatIsNotANumber(t *testing.T) {
	for _, value := range []any{nil, map[string]any{}, []any{1}, "not a number"} {
		if _, ok := timeseries.ToFloat(value); ok {
			t.Errorf("ToFloat(%v) reported success", value)
		}
	}
}

// Counting distinct values keeps the wire literal, so 1 and 1.0 count as the
// different literals they are.
func TestDistinctKeySeparatesTypesAndLiterals(t *testing.T) {
	keys := map[string]bool{}
	for _, value := range []any{json.Number("1"), json.Number("1.0"), "1", true, nil} {
		keys[timeseries.DistinctKey(value)] = true
	}
	if len(keys) != 5 {
		t.Errorf("distinct keys = %d, want 5 distinct identities: %v", len(keys), keys)
	}
}

// A silently misread timestamp corrupts every temporal detector at once, so an
// unreadable one is reported rather than guessed at.
func TestAnUnreadableTimestampIsReportedWithItsValue(t *testing.T) {
	_, err := timeseries.DecodeResults(request("value.power"), []timeseries.QueryResult{result([][]any{
		{"the day before yesterday", json.Number("10")},
	}, "value.power")}, "")
	if err == nil {
		t.Fatal("an unreadable timestamp was accepted")
	}
	if !strings.Contains(err.Error(), "the day before yesterday") {
		t.Errorf("error = %v, want the offending value named", err)
	}
}

// The fallback covers a server rendering a whole second without the millisecond
// field.
func TestATimestampWithoutMillisecondsStillParses(t *testing.T) {
	sets, err := timeseries.DecodeResults(request("value.power"), []timeseries.QueryResult{result([][]any{
		{"2026-06-01T00:00:00Z", json.Number("10")},
	}, "value.power")}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	if !sets[0].Times[0].Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamp = %s, want it parsed", sets[0].Times[0])
	}
}

// The sub-series of one element are its columns and belong in one result set.
// This used to assert the opposite — a set per sub-series — which is the bug that
// made a four-column read find one column: the extra sets were built with the
// wrong names and then discarded by the caller.
//
// It also checks that the set keeps the request index, which is what maps a
// batched read back onto the element that asked for it.
func TestTheSubSeriesOfOneElementCombineIntoOneResultSet(t *testing.T) {
	device, service := "urn:infai:ses:device:1", "urn:infai:ses:service:1"
	element := func(columns ...string) timeseries.QueryElement {
		out := timeseries.QueryElement{DeviceId: &device, ServiceId: &service}
		for _, name := range columns {
			out.Columns = append(out.Columns, timeseries.QueryColumn{Name: name})
		}
		return out
	}
	batch := []timeseries.QueryElement{
		element("value.a"),
		element("value.b"),
		element("value.power", "value.total"),
	}

	response := timeseries.QueryResult{
		RequestIndex: 2,
		DeviceId:     &device,
		ServiceId:    &service,
		ColumnNames:  []string{"value.power"},
		Data: [][][]any{
			{{"2026-06-01T00:00:00.000Z", json.Number("1")}},
			{{"2026-06-01T00:00:00.000Z", json.Number("2")}},
		},
	}

	sets, err := timeseries.DecodeResults(batch, []timeseries.QueryResult{response}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("sets = %d, want one per request element", len(sets))
	}
	if sets[0].RequestIndex != 2 {
		t.Errorf("request index = %d, want 2 so the batch maps back", sets[0].RequestIndex)
	}

	power, ok := sets[0].Column("value.power")
	if !ok {
		t.Fatal("value.power is missing")
	}
	if got, _ := timeseries.ToFloat(power.Values[0]); got != 1 {
		t.Errorf("value.power = %v, want the first sub-series", got)
	}
	total, ok := sets[0].Column("value.total")
	if !ok {
		t.Fatal("value.total is missing; the second sub-series was dropped")
	}
	if got, _ := timeseries.ToFloat(total.Values[0]); got != 2 {
		t.Errorf("value.total = %v, want the second sub-series", got)
	}
}

// Per-column series are separate queries server-side and the server trims
// trailing empty rows from each, so they can end at different points. They
// recombine on their timestamps, and the short one reads as null where it stops
// rather than shifting the other column's values onto the wrong rows.
func TestPerColumnSeriesOfDifferentLengthsAlignOnTime(t *testing.T) {
	device, service := "urn:infai:ses:device:1", "urn:infai:ses:service:1"
	batch := []timeseries.QueryElement{{
		DeviceId: &device, ServiceId: &service,
		Columns: []timeseries.QueryColumn{{Name: "value.power"}, {Name: "value.total"}},
	}}
	response := timeseries.QueryResult{
		RequestIndex: 0, DeviceId: &device, ServiceId: &service,
		ColumnNames: []string{"value.power"},
		Data: [][][]any{
			{
				{"2026-06-01T00:00:00.000Z", json.Number("10")},
				{"2026-06-01T00:15:00.000Z", json.Number("11")},
			},
			{
				{"2026-06-01T00:00:00.000Z", json.Number("100")},
			},
		},
	}

	sets, err := timeseries.DecodeResults(batch, []timeseries.QueryResult{response}, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	set := sets[0]
	if set.Rows() != 2 {
		t.Fatalf("rows = %d, want the union of both series", set.Rows())
	}
	power, _ := set.Column("value.power")
	total, _ := set.Column("value.total")
	if power.Len() != 2 {
		t.Errorf("value.power has %d values, want 2", power.Len())
	}
	if total.Len() != 1 || total.NullRows != 1 {
		t.Errorf("value.total has %d values and %d nulls, want 1 and 1", total.Len(), total.NullRows)
	}
	if got, _ := timeseries.ToFloat(total.Values[0]); got != 100 {
		t.Errorf("value.total = %v, want 100 on the row it does cover", got)
	}
}

func TestAnEmptyResponseDecodesToNothingRatherThanAnError(t *testing.T) {
	sets, err := timeseries.DecodeResults(nil, nil, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("sets = %d, want none", len(sets))
	}
}
