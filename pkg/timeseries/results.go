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

package timeseries

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// ResultSet is one decoded sub-series of a POST /queries/v2 response: the
// timestamps of a row set and, per requested column, the value in each row.
//
// Both views matter to the profiler. Per-column series are what the individual
// detectors read, and the shared row index is what makes the cross-variable
// checks of §5.4.1 possible without a second read.
type ResultSet struct {
	// RequestIndex is the index of the request element this answers, which is
	// how a service-scoped batch is mapped back onto its series.
	RequestIndex int
	DeviceID     string
	ServiceID    string
	// ExportID is set instead of DeviceID and ServiceID when the element
	// addressed an export. Which of the two a set came from is not derivable
	// otherwise, and a caller mixing device and export elements in one batch would
	// have no way to tell them apart.
	ExportID    string
	ColumnNames []string
	Times       []time.Time
	// Values is column-major: Values[column][row]. A nil entry is a NULL in
	// that row, which is normal — a service message need not carry every
	// variable it declares.
	Values [][]any
}

func (r ResultSet) Rows() int { return len(r.Times) }

// source names where a set came from, for an error that would otherwise report an
// export as a device with two empty ids.
func (r ResultSet) source() string {
	if r.ExportID != "" {
		return "export " + r.ExportID
	}
	return "device " + r.DeviceID + " service " + r.ServiceID
}

// Column is one variable's series, with the NULL rows dropped. Timestamps are
// therefore per column: two variables of the same service can legitimately have
// different point counts, and averaging that away would hide it.
type Column struct {
	Name   string
	Times  []time.Time
	Values []any
	// NullRows is how many rows carried no value for this column.
	NullRows int
}

func (r ResultSet) Column(name string) (Column, bool) {
	for i, n := range r.ColumnNames {
		if n != name || i >= len(r.Values) {
			continue
		}
		col := Column{Name: name}
		for row, v := range r.Values[i] {
			if v == nil {
				col.NullRows++
				continue
			}
			if row < len(r.Times) {
				col.Times = append(col.Times, r.Times[row])
				col.Values = append(col.Values, v)
			}
		}
		return col, true
	}
	return Column{}, false
}

func (c Column) Len() int { return len(c.Values) }

// Numeric returns the column as float64s, dropping values that are not
// numeric, and reports how many it dropped. A column that is mostly
// non-numeric is a categorical or status variable rather than a broken one, and
// the caller decides which detectors still apply.
func (c Column) Numeric() (times []time.Time, values []float64, dropped int) {
	times = make([]time.Time, 0, len(c.Values))
	values = make([]float64, 0, len(c.Values))
	for i, v := range c.Values {
		f, ok := ToFloat(v)
		if !ok {
			dropped++
			continue
		}
		times = append(times, c.Times[i])
		values = append(values, f)
	}
	return times, values, dropped
}

// ToFloat coerces a decoded JSON value to float64.
//
// json.Number is the common case, because the client decodes with UseNumber to
// keep large integers exact until here. Booleans convert to 0 and 1 so that a
// binary variable still has a distribution and a duty cycle; the value-semantics
// detector is what decides whether reading it that way is meaningful.
func ToFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// DistinctKey is a stable identity for a value, for counting distinct values
// without converting to float first. json.Number keeps its literal text, so
// 1 and 1.0 count as different literals — which is what they are on the wire.
func DistinctKey(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case string:
		return "s:" + n
	case bool:
		return "b:" + strconv.FormatBool(n)
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// DecodeResults turns the per_query response into typed result sets, one per
// request element.
//
// Column identity comes from the *request*, not from the response's
// ColumnNames, and that is not a stylistic choice. POST /queries/v2 splits every
// requested column into its own database query — see the loop over
// dbRequestElement.Columns in the server's queries_v2 handler — so `Data` holds
// one `[time, value]` series per requested column, in request order, while
// `ColumnNames` is built from one of those single-column elements and therefore
// names only the first. Trusting it means reading four columns and finding one.
//
// layout is the Go time layout the query asked for; empty means the package
// default. Rows are sorted ascending by time regardless of the order the query
// requested, so a raw read that takes the newest N points by ordering descending
// still hands the detectors a forward-running series.
func DecodeResults(request []QueryElement, results []QueryResult, layout string) ([]ResultSet, error) {
	if layout == "" {
		layout = timeFormat
	}
	out := make([]ResultSet, 0, len(results))
	for _, result := range results {
		if result.RequestIndex < 0 || result.RequestIndex >= len(request) {
			return nil, fmt.Errorf("timeseries: response names request element %d, but %d were sent",
				result.RequestIndex, len(request))
		}
		set, err := decodeElement(request[result.RequestIndex], result, layout)
		if err != nil {
			return nil, err
		}
		out = append(out, set)
	}
	return out, nil
}

func decodeElement(element QueryElement, result QueryResult, layout string) (ResultSet, error) {
	set := ResultSet{RequestIndex: result.RequestIndex}
	if result.DeviceId != nil {
		set.DeviceID = *result.DeviceId
	}
	if result.ServiceId != nil {
		set.ServiceID = *result.ServiceId
	}
	if result.ExportId != nil {
		set.ExportID = *result.ExportId
	}
	for _, column := range element.Columns {
		set.ColumnNames = append(set.ColumnNames, column.Name)
	}

	// A row is keyed by its timestamp so the per-column series recombine into
	// one aligned table. They are separate queries server-side and can end at
	// different points — the server trims trailing empty rows per series — so
	// they cannot be zipped by position.
	rows := map[int64][]any{}
	times := map[int64]time.Time{}

	for seriesIndex, series := range result.Data {
		for rowIndex, row := range series {
			if len(row) == 0 {
				continue
			}
			at, err := parseTime(row[0], layout)
			if err != nil {
				return ResultSet{}, fmt.Errorf("timeseries: decoding row %d of series %d for %s: %w",
					rowIndex, seriesIndex, set.source(), err)
			}
			key := at.UnixNano()
			if _, seen := rows[key]; !seen {
				rows[key] = make([]any, len(set.ColumnNames))
				times[key] = at
			}
			for _, target := range columnTargets(element, result, seriesIndex, len(row)) {
				if target.column < len(set.ColumnNames) && target.value < len(row) {
					rows[key][target.column] = row[target.value]
				}
			}
		}
	}

	keys := make([]int64, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	set.Times = make([]time.Time, 0, len(keys))
	set.Values = make([][]any, len(set.ColumnNames))
	for i := range set.Values {
		set.Values[i] = make([]any, 0, len(keys))
	}
	for _, key := range keys {
		set.Times = append(set.Times, times[key])
		for column := range set.ColumnNames {
			set.Values[column] = append(set.Values[column], rows[key][column])
		}
	}
	return set, nil
}

// target maps a position in a response row onto a requested column.
type target struct {
	column int // index into the request's columns
	value  int // index into the response row
}

// columnTargets works out which requested column a row position belongs to.
//
// Two shapes occur. The one /queries/v2 produces for a device and service is a
// series per column, where sub-series k carries column k and every row is
// [time, value]. The other is a single wide table, [time, c1, c2, …], which is
// what a request for one column also looks like. Deciding by width rather than
// guessing keeps both correct, and an unrecognised shape is reported rather than
// silently misread — reading the wrong column is exactly the failure this
// function exists to prevent.
func columnTargets(element QueryElement, result QueryResult, seriesIndex, rowWidth int) []target {
	columns := len(element.Columns)

	// A series per column: as many sub-series as columns, each two wide.
	if len(result.Data) == columns && rowWidth == 2 {
		return []target{{column: seriesIndex, value: 1}}
	}
	// One wide table carrying every column.
	if rowWidth == columns+1 {
		out := make([]target, 0, columns)
		for column := 0; column < columns; column++ {
			out = append(out, target{column: column, value: column + 1})
		}
		return out
	}
	// A short row within the per-column shape still belongs to its own series.
	if len(result.Data) == columns {
		return []target{{column: seriesIndex, value: 1}}
	}
	return nil
}

// parseTime accepts the requested layout first, then RFC3339 with nanoseconds.
// The fallback covers a server that renders a whole second without the
// millisecond field; anything else is reported rather than guessed at, because
// a silently misread timestamp corrupts every temporal detector at once.
func parseTime(v any, layout string) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("time column holds %T, want a formatted string", v)
	}
	if at, err := time.Parse(layout, s); err == nil {
		return at.UTC(), nil
	}
	if at, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return at.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("time %q parses as neither %q nor RFC3339", s, layout)
}
