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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

func irradianceColumn() []ExportColumn {
	return []ExportColumn{{Column: "irradiance", Type: "float", VariablePath: "value.ghi"}}
}

func TestExportExtentCountsRowsAndFindsTheFirstRowWithoutAValue(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	buckets := []time.Time{now.AddDate(-2, 0, 0), now.AddDate(-1, 0, 0)}
	first := buckets[0].Add(3*time.Hour + 17*time.Second)
	fake := &fakeTimeseries{
		results: [][]timeseries.QueryResult{
			{countResult([]string{"irradiance"}, buckets, map[string][]float64{"irradiance": {8000, 8760}})},
			{countResult([]string{"irradiance"}, []time.Time{first}, map[string][]float64{"irradiance": {1}})},
		},
	}
	prof := exportProfiler(t, fake, nil, now)

	extent := prof.ExportExtent(context.Background(), "Bearer caller", ExportExtentRequest{
		ExportID: testExportID,
		Columns:  irradianceColumn(),
	})

	if rows := mustGet(t, extent.Rows, "rows"); rows != 16760 {
		t.Errorf("rows = %d, want the summed 16760", rows)
	}
	if got := mustGet(t, extent.FirstRow, "first_row"); !got.Equal(first) {
		t.Errorf("first_row = %s, want %s", got, first)
	}
	if extent.Reads.Values != 2 {
		t.Errorf("reads = %d, want a count and a first-row lookup", extent.Reads.Values)
	}

	if len(fake.queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(fake.queries))
	}
	for _, query := range fake.queries {
		for _, column := range query[0].Columns {
			if column.GroupType == nil || *column.GroupType != timeseries.GroupCount {
				t.Errorf("column %s asks for %v, want count: anything else returns a value", column.Name, column.GroupType)
			}
		}
	}
	lookup := fake.queries[1][0]
	if lookup.GroupTime == nil || *lookup.GroupTime != firstRowBucket {
		t.Errorf("groupTime = %v, want %s", lookup.GroupTime, firstRowBucket)
	}
	if lookup.Limit == nil || *lookup.Limit != 1 ||
		lookup.OrderColumnIndex == nil || *lookup.OrderColumnIndex != 0 ||
		lookup.OrderDirection == nil || *lookup.OrderDirection != timeseries.OrderAscending {
		t.Errorf("the lookup is not ordered by time ascending and limited to one: %+v", lookup)
	}
	if lookup.Filters == nil || len(*lookup.Filters) != 1 ||
		(*lookup.Filters)[0].Column != "irradiance" || (*lookup.Filters)[0].Type != "!=" || (*lookup.Filters)[0].Value != nil {
		t.Errorf("filters = %+v, want irradiance IS NOT NULL", lookup.Filters)
	}
}

// Rows and FirstRow describe one series: the first row is looked up for the
// column the rows were taken from, not for whichever column was named first.
func TestTheFirstRowIsLookedUpForTheColumnWithTheMostRows(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	buckets := []time.Time{now.AddDate(-1, 0, 0)}
	fake := &fakeTimeseries{
		results: [][]timeseries.QueryResult{
			{countResult([]string{"irradiance", "temperature"}, buckets,
				map[string][]float64{"irradiance": {10}, "temperature": {900}})},
			{countResult([]string{"temperature"}, buckets, map[string][]float64{"temperature": {1}})},
		},
	}
	prof := exportProfiler(t, fake, nil, now)

	extent := prof.ExportExtent(context.Background(), "Bearer caller", ExportExtentRequest{
		ExportID: testExportID,
		Columns: []ExportColumn{
			{Column: "irradiance", Type: "float"},
			{Column: "temperature", Type: "float"},
		},
	})

	if rows := mustGet(t, extent.Rows, "rows"); rows != 900 {
		t.Errorf("rows = %d, want 900 from temperature", rows)
	}
	if len(fake.queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(fake.queries))
	}
	if column := fake.queries[1][0].Columns[0].Name; column != "temperature" {
		t.Errorf("the first row was looked up for %s, want temperature", column)
	}
}

// Also the case where rows exist and the requested column is null in all of
// them: the count is 0 either way, and there is no value to find the first of.
func TestAnExportWithNoRowHasNoFirstRowAndIsNotAskedForOne(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeTimeseries{results: [][]timeseries.QueryResult{
		{countResult([]string{"irradiance"}, nil, nil)},
	}}
	prof := exportProfiler(t, fake, nil, now)

	extent := prof.ExportExtent(context.Background(), "Bearer caller", ExportExtentRequest{
		ExportID: testExportID,
		Columns:  irradianceColumn(),
	})

	if rows := mustGet(t, extent.Rows, "rows"); rows != 0 {
		t.Errorf("rows = %d, want 0", rows)
	}
	if extent.FirstRow.IsComputed() {
		t.Errorf("first_row = %v, want not_computed", extent.FirstRow)
	}
	if status := extent.FirstRow.Status(); status.Reason != ReasonInsufficientCoverage {
		t.Errorf("first_row status = %+v, want insufficient_coverage", status)
	}
	if len(fake.queries) != 1 {
		t.Errorf("queries = %d, want only the count", len(fake.queries))
	}
}

func TestAFailedRowCountLeavesTheFirstRowStanding(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	first := now.AddDate(-1, 0, 0)
	fake := &fakeTimeseries{
		queryErr:     errors.New("statement timeout"),
		queryErrCall: 1,
		results: [][]timeseries.QueryResult{
			{countResult([]string{"irradiance"}, []time.Time{first}, map[string][]float64{"irradiance": {1}})},
		},
	}
	prof := exportProfiler(t, fake, nil, now)

	extent := prof.ExportExtent(context.Background(), "Bearer caller", ExportExtentRequest{
		ExportID: testExportID,
		Columns:  irradianceColumn(),
	})

	if extent.Rows.IsComputed() {
		t.Fatalf("rows = %v, want not_computed", extent.Rows)
	}
	if status := extent.Rows.Status(); status.Reason != ReasonReadFailed || !strings.Contains(status.Detail, "statement timeout") {
		t.Errorf("rows status = %+v, want read_failed with the platform's error", status)
	}
	if got := mustGet(t, extent.FirstRow, "first_row"); !got.Equal(first) {
		t.Errorf("first_row = %s, want %s", got, first)
	}
}

func TestTheExtentWindowStopsAtTheTrainingEnd(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	trainingEnd := now.AddDate(0, -2, 0)
	fake := &fakeTimeseries{}
	prof := exportProfiler(t, fake, nil, now)

	extent := prof.ExportExtent(context.Background(), "Bearer caller", ExportExtentRequest{
		ExportID: testExportID,
		Columns:  irradianceColumn(),
		Split:    &exposure.Split{TrainingEnd: trainingEnd, TestEnd: now},
	})

	if !extent.Window.To.Equal(trainingEnd) {
		t.Errorf("window ends %s, want the training end %s", extent.Window.To, trainingEnd)
	}
	if len(fake.queries) == 0 {
		t.Fatal("nothing was counted")
	}
	if end := *fake.queries[0][0].Time.End; end != trainingEnd.Format(time.RFC3339) {
		t.Errorf("the count runs to %s, want %s", end, trainingEnd.Format(time.RFC3339))
	}
}

func TestAnExportWithNoCountableColumnIsNotQueried(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeTimeseries{}
	prof := exportProfiler(t, fake, nil, now)

	extent := prof.ExportExtent(context.Background(), "Bearer caller", ExportExtentRequest{
		ExportID: testExportID,
		Columns:  []ExportColumn{{Column: "irradiance", Type: "geometry"}},
	})

	if status := extent.Rows.Status(); status.Reason != ReasonWrongKind {
		t.Errorf("rows status = %+v, want wrong_kind", status)
	}
	if len(fake.queries) != 0 {
		t.Errorf("queries = %d, want none", len(fake.queries))
	}
}
