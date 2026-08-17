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

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// This is a verbatim response from the platform for a four-column request against
// one device and service. It is here as a literal because the shape is nothing
// like the one this package originally assumed, and an assumption is what made
// the first decoder wrong: POST /queries/v2 splits every requested column into
// its own query, so `data` holds one [time, value] series per column — while
// `columnNames` is built from a single-column element and names only the first.
//
// Reading `columnNames` positionally against wide rows found `root.lastUpdate`
// and nothing else, and every other variable of the service was reported as a
// dead channel.
const fourColumnResponse = `[
  {
    "requestIndex": 0,
    "data": [
      [
        ["2026-08-17T11:30:01.291Z", 1786966201290],
        ["2026-08-17T11:29:56.292Z", 1786966196290],
        ["2026-08-17T11:29:51.291Z", 1786966191290]
      ],
      [
        ["2026-08-17T11:30:01.291Z", "Timestamp (Unix Milliseconds)"],
        ["2026-08-17T11:29:56.292Z", "Timestamp (Unix Milliseconds)"],
        ["2026-08-17T11:29:51.291Z", "Timestamp (Unix Milliseconds)"]
      ],
      [
        ["2026-08-17T11:30:01.291Z", 1],
        ["2026-08-17T11:29:56.292Z", 1],
        ["2026-08-17T11:29:51.291Z", 1]
      ],
      [
        ["2026-08-17T11:30:01.291Z", "Watt"],
        ["2026-08-17T11:29:56.292Z", "Watt"],
        ["2026-08-17T11:29:51.291Z", "Watt"]
      ]
    ],
    "deviceId": "urn:infai:ses:device:f5543003-7811-44ab-8d13-bd592958ffa4",
    "serviceId": "urn:infai:ses:service:5cc05c60-5916-4b37-8991-53a93c354fe5",
    "columnNames": ["root.lastUpdate"]
  }
]`

func fourColumnRequest() []timeseries.QueryElement {
	device := "urn:infai:ses:device:f5543003-7811-44ab-8d13-bd592958ffa4"
	service := "urn:infai:ses:service:5cc05c60-5916-4b37-8991-53a93c354fe5"
	return []timeseries.QueryElement{{
		DeviceId:  &device,
		ServiceId: &service,
		Columns: []timeseries.QueryColumn{
			{Name: "root.lastUpdate"},
			{Name: "root.lastUpdate_unit"},
			{Name: "root.value"},
			{Name: "root.value_unit"},
		},
	}}
}

func decodeFourColumn(t *testing.T) timeseries.ResultSet {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(fourColumnResponse))
	decoder.UseNumber()
	var results []timeseries.QueryResult
	if err := decoder.Decode(&results); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	sets, err := timeseries.DecodeResults(fourColumnRequest(), results, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("sets = %d, want one per request element", len(sets))
	}
	return sets[0]
}

// The bug, stated as a test: the third requested column has to carry the third
// series, not nothing.
func TestEveryRequestedColumnIsFoundInAPerColumnResponse(t *testing.T) {
	set := decodeFourColumn(t)

	for _, name := range []string{"root.lastUpdate", "root.lastUpdate_unit", "root.value", "root.value_unit"} {
		column, ok := set.Column(name)
		if !ok {
			t.Errorf("%s is missing from the decoded set", name)
			continue
		}
		if column.Len() != 3 {
			t.Errorf("%s has %d values, want 3", name, column.Len())
		}
		if column.NullRows != 0 {
			t.Errorf("%s has %d null rows, want none", name, column.NullRows)
		}
	}
}

// And each column has to carry its *own* values, which is the failure a
// presence check alone would miss.
func TestEachColumnCarriesItsOwnValues(t *testing.T) {
	set := decodeFourColumn(t)

	power, _ := set.Column("root.value")
	_, values, dropped := power.Numeric()
	if dropped != 0 {
		t.Errorf("root.value dropped %d non-numeric values", dropped)
	}
	for _, value := range values {
		if value != 1 {
			t.Errorf("root.value = %v, want the watt reading of 1", value)
		}
	}

	unit, _ := set.Column("root.value_unit")
	for _, value := range unit.Values {
		if value != "Watt" {
			t.Errorf("root.value_unit = %v, want Watt", value)
		}
	}

	// The timestamp channel is the one the old decoder found, and it must not
	// have leaked into the others.
	updates, _ := set.Column("root.lastUpdate")
	if _, first := timeseries.ToFloat(updates.Values[0]); !first {
		t.Error("root.lastUpdate is not numeric")
	}
	if got, _ := timeseries.ToFloat(updates.Values[0]); got == 1 {
		t.Error("root.lastUpdate holds the power reading; the series are crossed")
	}
}

// The rows recombine on their shared timestamps, which is what the
// cross-variable checks of §5.4.1 need.
func TestPerColumnSeriesRecombineOnTheirTimestamps(t *testing.T) {
	set := decodeFourColumn(t)

	if set.Rows() != 3 {
		t.Fatalf("rows = %d, want the three shared timestamps", set.Rows())
	}
	for i := 1; i < set.Rows(); i++ {
		if !set.Times[i].After(set.Times[i-1]) {
			t.Errorf("timestamps are not ascending: %v", set.Times)
		}
	}
	if len(set.Values) != 4 {
		t.Fatalf("value columns = %d, want 4", len(set.Values))
	}
	for column, values := range set.Values {
		if len(values) != set.Rows() {
			t.Errorf("column %d has %d values against %d rows", column, len(values), set.Rows())
		}
	}
}

// A single-column request is the same wire shape read the other way, and it kept
// working throughout — which is why the bug hid: every test and every fake used
// one column or a shape nobody had checked against the platform.
func TestASingleColumnRequestStillDecodes(t *testing.T) {
	device, service := "urn:infai:ses:device:1", "urn:infai:ses:service:1"
	request := []timeseries.QueryElement{{
		DeviceId: &device, ServiceId: &service,
		Columns: []timeseries.QueryColumn{{Name: "root.value"}},
	}}
	results := []timeseries.QueryResult{{
		RequestIndex: 0, DeviceId: &device, ServiceId: &service,
		ColumnNames: []string{"root.value"},
		Data: [][][]any{{
			{"2026-08-17T11:30:01.291Z", json.Number("7")},
		}},
	}}

	sets, err := timeseries.DecodeResults(request, results, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	column, ok := sets[0].Column("root.value")
	if !ok || column.Len() != 1 {
		t.Fatalf("column = %+v, want one value", column)
	}
	if got, _ := timeseries.ToFloat(column.Values[0]); got != 7 {
		t.Errorf("value = %v, want 7", got)
	}
}

// A response naming an element that was never sent is a bug somewhere, and
// guessing which element it meant is how columns get crossed.
func TestAResponseForAnUnsentElementIsRefused(t *testing.T) {
	results := []timeseries.QueryResult{{RequestIndex: 4}}
	if _, err := timeseries.DecodeResults(fourColumnRequest(), results, ""); err == nil {
		t.Fatal("a response for element 4 was accepted against a one-element request")
	}
}
