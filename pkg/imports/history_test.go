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

package imports

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func importExport() Export {
	return Export{
		ID:         "export-1",
		FilterType: FilterTypeImportExport,
		Filter:     testInstanceID,
		Name:       "Leipzig weather",
		Topic:      testTopic,
		Values: []ExportValue{
			// Note the Path: relative to the payload, one level below the
			// message-relative path a Selectable carries.
			{Name: "temp_c", Path: "temperature_2m", Type: "float"},
			{Name: "station", Path: "station_id", Type: "string", Tag: true},
		},
	}
}

func TestAnExportedImportReportsItsColumns(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{},
		&fakeExports{serve: []Export{importExport()}, total: 1})

	history := service.History(context.Background(), testToken, testInstanceID)

	if history.State != HistoryExported {
		t.Fatalf("state = %q, want exported", history.State)
	}
	if history.ExportID != "export-1" {
		t.Errorf("export_id = %q, want the id timescale-wrapper takes as exportId", history.ExportID)
	}
	column, found := history.ExportColumn("value.temperature_2m")
	if !found {
		t.Fatal("the temperature column was not resolved; the mapping from variable path to " +
			"timescale column is the whole point of this lookup")
	}
	if column != "temp_c" {
		t.Errorf("column = %q, want temp_c: the column is named by whoever created the export, "+
			"not by the content variable, so assuming the path would query a column that does "+
			"not exist", column)
	}
	if history.Reason == "" {
		t.Error("a reason is expected for every state, including the good one")
	}
}

func TestAnUnexportedImportIsLiveOnly(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{},
		&fakeExports{serve: []Export{{
			ID: "export-9", FilterType: "deviceId", Filter: "urn:infai:ses:device:x",
		}}, total: 1})

	history := service.History(context.Background(), testToken, testInstanceID)

	if history.State != HistoryLiveOnly {
		t.Fatalf("state = %q, want live_only when no export names this import", history.State)
	}
	if history.ExportID != "" {
		t.Errorf("export_id = %q, want it empty", history.ExportID)
	}
	if !strings.Contains(history.Reason, "Kafka") {
		t.Errorf("the reason should say what an operator can still do: %q", history.Reason)
	}
}

// The distinction the plan insisted on: not found within the bound is not the
// same claim as not exported, and only the second is actionable.
func TestABoundedScanThatRanOutAnswersUnknown(t *testing.T) {
	// More exports exist than were read, and none of the read ones matched.
	service := newService(t, &fakeSelectables{}, &fakeInstances{},
		&fakeExports{serve: []Export{{ID: "e1", FilterType: "deviceId"}}, total: 5000})

	history := service.History(context.Background(), testToken, testInstanceID)

	if history.State != HistoryUnknown {
		t.Errorf("state = %q, want unknown: reporting live_only here would send a developer to "+
			"design for a cold start they may not have", history.State)
	}
}

func TestAnUnreachableServingAnswersUnknown(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{},
		&fakeExports{err: errors.New("analytics-serving is down")})

	history := service.History(context.Background(), testToken, testInstanceID)

	if history.State != HistoryUnknown {
		t.Errorf("state = %q, want unknown", history.State)
	}
	if !strings.Contains(history.Reason, "analytics-serving is down") {
		t.Errorf("the upstream error should survive into the reason: %q", history.Reason)
	}
}

func TestNoServingConfiguredAnswersUnknownNotLiveOnly(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{}, nil)

	history := service.History(context.Background(), testToken, testInstanceID)

	if history.State != HistoryUnknown {
		t.Errorf("state = %q, want unknown: an unconfigured lookup has not established that "+
			"there is no history", history.State)
	}
	if !strings.Contains(history.Reason, "configured") {
		t.Errorf("the reason should name the missing configuration: %q", history.Reason)
	}
}

// A column the export did not include is an ordinary answer, not an error: the
// export dialog offers a selection of the import type's variables.
func TestAnUnexportedVariableIsNotFound(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{},
		&fakeExports{serve: []Export{importExport()}, total: 1})

	history := service.History(context.Background(), testToken, testInstanceID)
	if _, found := history.ExportColumn("value.pressure_msl"); found {
		t.Error("a variable the export does not carry must not resolve to a column")
	}
}

// The same variable addressed with the output root still on the front has to
// resolve, because that is the form a model repeats back from an import type.
func TestExportColumnNormalisesThePath(t *testing.T) {
	service := newService(t, &fakeSelectables{}, &fakeInstances{},
		&fakeExports{serve: []Export{importExport()}, total: 1})

	history := service.History(context.Background(), testToken, testInstanceID)
	column, found := history.ExportColumn("root.value.temperature_2m")
	if !found || column != "temp_c" {
		t.Errorf("column = %q found = %v, want temp_c", column, found)
	}
}
