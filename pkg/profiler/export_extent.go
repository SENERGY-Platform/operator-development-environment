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
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// The extent of an export is what a resolution orders import candidates by.
//
// Two imports can carry the same signal and differ only in what is stored of it:
// one has years of rows in its export, the other has an export nothing was written
// to, or none at all. A launch trains on the export, so that difference decides
// what an operator learns from, and the export listing cannot show it. ExportFill
// answers the same question in full for one export the developer names; this is
// the part of it a shortlist needs — how many rows, and since when — over the
// selected columns only.

// firstRowBucket is the resolution of FirstRow. A count per second ordered by time
// and limited to one bucket is the earliest row's timestamp to the second, and it
// keeps the answer a count rather than a value.
const firstRowBucket = "1s"

// ExportExtentRequest asks how much one export holds of some of its columns.
type ExportExtentRequest struct {
	ExportID string
	// Columns are the export columns of the variables the caller selected, not
	// every column the export declares: an import is chosen for a signal, and an
	// export whose other columns are full says nothing about that one.
	Columns []ExportColumn
	// Window bounds both reads. Zero means DefaultExportProbeDays back from now,
	// the window probe_export_data counts over, so the two answers agree.
	Window Window
	// Split is the session's data split (D36), or nil. The count names how much
	// of a window is stored, so it is clamped to the training end like
	// ExportFill's.
	Split *exposure.Split
}

// ExportExtent is how much of the requested columns an export holds.
type ExportExtent struct {
	// Window is the range both reads covered, after the split clamped it.
	Window Window `json:"window"`
	// Rows is how many rows of Window carry a value in at least one requested
	// column, taken from the column that carries the most — ExportFill.Rows over
	// fewer columns.
	Rows Value[int] `json:"rows"`
	// FirstRow is the earliest row in Window, to the second, in which the column
	// Rows was taken from carries a value. Not the table's first row: an export
	// can write rows for years before the requested column is filled, and those
	// years hold nothing to train on.
	FirstRow Value[time.Time] `json:"first_row"`
	// Reads is what the two reads cost. Not serialised: the caller adds it to its
	// own counters.
	Reads ReadCounts `json:"-"`
}

// ExportExtent counts the rows an export holds of the requested columns and finds
// its first row.
//
// Two POST /queries/v2, neither of which returns a value: the count is ExportFill's
// bucketed `count`, and the first row is a `count` per second over the rows where
// the counted column is not null, ordered by time and limited to one bucket. A
// failed read degrades its own field to not_computed and leaves the other
// standing, because a resolution that ranks on this must not fail over it.
func (p *Profiler) ExportExtent(ctx context.Context, token string, req ExportExtentRequest) ExportExtent {
	window := req.Window
	if !window.Valid() {
		to := p.now()
		window = Window{From: to.AddDate(0, 0, -DefaultExportProbeDays), To: to}
	}
	out := ExportExtent{Window: window}

	from, to, err := req.Split.ClampWindow(window.From, window.To)
	if err != nil {
		out.Rows = Uncomputablef[int](ReasonOutOfScope, "the window is past the training end: %v", err)
		out.FirstRow = Uncomputablef[time.Time](ReasonOutOfScope, "the window is past the training end: %v", err)
		return out
	}
	window.From, window.To = from, to
	out.Window = window

	countable := []Variable{}
	for _, variable := range ExportVariables(req.Columns) {
		if variable.Queryable {
			countable = append(countable, variable)
		}
	}
	if len(countable) == 0 {
		const detail = "none of the requested export columns can be read as a series, so their rows cannot be counted"
		out.Rows = Uncomputable[int](ReasonWrongKind, detail)
		out.FirstRow = Uncomputable[time.Time](ReasonWrongKind, detail)
		return out
	}

	// The first row is looked up for the column Rows is taken from, so the two
	// describe the same series. When the count fails that column is unknown, and
	// the first countable one stands in.
	fullest := countable[0]
	counted, err := p.countRows(ctx, token, req.ExportID, countable, window, exportProbeBucket(window), &out.Reads, req.Split)
	if err != nil {
		out.Rows = Uncomputablef[int](ReasonReadFailed, "the row count could not be read: %v", err)
	} else {
		rows := -1
		for _, variable := range countable {
			if n := counted.perColumn[variable.Path]; n > rows {
				rows, fullest = n, variable
			}
		}
		out.Rows = Computed(rows)
		if rows == 0 {
			// Counted and found nothing: there is no row with a value to look up.
			out.FirstRow = Uncomputablef[time.Time](ReasonInsufficientCoverage,
				"no row over %s carries a value in the requested columns", window.String())
			return out
		}
	}

	first, found, err := p.firstRow(ctx, token, req.ExportID, fullest, window, &out.Reads, req.Split)
	switch {
	case err != nil:
		out.FirstRow = Uncomputablef[time.Time](ReasonReadFailed, "the first row could not be read: %v", err)
	case !found:
		out.FirstRow = Uncomputablef[time.Time](ReasonInsufficientCoverage,
			"no row over %s carries a value in column %s", window.String(), fullest.Path)
	default:
		out.FirstRow = Computed(first)
	}
	return out
}

// firstRow asks for the earliest row of an export in window that carries a value
// in variable's column.
//
// timescale-wrapper applies an element's order and limit inside the per-column
// sub-query as well as outside it, so the server buckets the window, sorts the
// buckets by time and stops at the first one rather than returning them all. The
// not-null filter is what makes it the column's first value rather than the
// table's first row: a bucket otherwise appears wherever a row falls, null columns
// included.
func (p *Profiler) firstRow(
	ctx context.Context, token, exportID string,
	variable Variable, window Window, reads *ReadCounts, split *exposure.Split,
) (time.Time, bool, error) {
	count := timeseries.GroupCount
	bucket := firstRowBucket
	limit, orderIndex := 1, 0
	ascending := timeseries.Direction(timeseries.OrderAscending)

	element := exportSource(exportID).element()
	element.Columns = []timeseries.QueryColumn{{Name: variable.Path, GroupType: &count}}
	// A nil value with "!=" is rendered as IS NOT NULL.
	element.Filters = &[]timeseries.QueryFilter{{Column: variable.Path, Type: "!="}}
	element.GroupTime = &bucket
	element.Limit = &limit
	element.OrderColumnIndex = &orderIndex
	element.OrderDirection = &ascending
	element.Time = &timeseries.QueryTime{
		Start: stringPtr(window.From.UTC().Format(time.RFC3339)),
		End:   stringPtr(window.To.UTC().Format(time.RFC3339)),
	}

	results, err := p.ts.Query(ctx, token, []timeseries.QueryElement{element},
		timeseries.QueryOptions{Timeout: p.opts.ReadTimeout, Split: split})
	// Under Values for the reason countRows counts there: it is a POST /queries/v2,
	// and what it returns is a count and a timestamp.
	reads.Values++
	if err != nil {
		return time.Time{}, false, err
	}
	sets, err := timeseries.DecodeResults([]timeseries.QueryElement{element}, results, "")
	if err != nil {
		return time.Time{}, false, err
	}
	var first time.Time
	for _, set := range sets {
		if set.Rows() > 0 && (first.IsZero() || set.Times[0].Before(first)) {
			first = set.Times[0]
		}
	}
	return first, !first.IsZero(), nil
}
