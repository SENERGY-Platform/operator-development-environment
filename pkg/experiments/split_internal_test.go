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

package experiments

import (
	"database/sql"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
)

// --- the Postgres pair (split_training_end, split_test_end) ---

func TestSplitColumnsRoundTripsThroughTheNullablePair(t *testing.T) {
	split := &exposure.Split{
		TrainingEnd: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TestEnd:     time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	trainingEnd, testEnd := splitColumns(split)
	if !trainingEnd.Valid || !testEnd.Valid {
		t.Fatalf("columns = %+v, %+v, want both valid for a set split", trainingEnd, testEnd)
	}
	back := splitFromColumns(trainingEnd, testEnd)
	if back == nil || !back.Equal(*split) {
		t.Errorf("round trip = %+v, want %+v back", back, split)
	}
}

func TestSplitColumnsForNoSplitAreBothUnset(t *testing.T) {
	trainingEnd, testEnd := splitColumns(nil)
	if trainingEnd.Valid || testEnd.Valid {
		t.Errorf("columns = %+v, %+v, want neither valid for no split", trainingEnd, testEnd)
	}
	if back := splitFromColumns(trainingEnd, testEnd); back != nil {
		t.Errorf("split = %+v, want nil when neither column is set", back)
	}
}

// A partially-written pair cannot happen from splitColumns, but scanExperiment
// reads whatever two columns a row actually has — so the inverse has to treat a
// half-set pair as no split rather than as a split with a zero bound.
func TestSplitFromColumnsRefusesAHalfSetPair(t *testing.T) {
	only := sql.NullTime{Time: time.Now(), Valid: true}
	if back := splitFromColumns(only, sql.NullTime{}); back != nil {
		t.Errorf("split = %+v, want nil when only training_end is set", back)
	}
	if back := splitFromColumns(sql.NullTime{}, only); back != nil {
		t.Errorf("split = %+v, want nil when only test_end is set", back)
	}
}

// --- the run's own confirmation (splitReport) ---

func TestSplitReportIsNilWithoutASplit(t *testing.T) {
	if report := splitReport(nil, nil, nil, true); report != nil {
		t.Errorf("report = %+v, want nil for a run with no data split", report)
	}
}

func TestSplitReportConfirmsWhenTheRunsTagsMatch(t *testing.T) {
	split := &exposure.Split{
		TrainingEnd: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TestEnd:     time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	tags := map[string]string{
		operatorHistoryEndTag: "2026-06-01T00:00:00+00:00",
		operatorTestEndTag:    "2026-06-08T00:00:00.123456+00:00",
	}
	params := map[string]string{
		paramEvaluationMessages:    "1200",
		paramEvaluationResults:     "1150",
		paramEvaluationWindowStart: "2026-06-01T00:00:00+00:00",
		paramEvaluationWindowEnd:   "2026-06-08T00:00:00+00:00",
	}
	report := splitReport(split, tags, params, true)
	if report == nil {
		t.Fatal("report = nil, want one for a set split")
	}
	if report.Confirmed != splitConfirmed {
		t.Errorf("confirmed = %q, want %q: the tags equal the split to the second",
			report.Confirmed, splitConfirmed)
	}
	if report.Note != "" {
		t.Errorf("note = %q, want none for a confirmed run", report.Note)
	}
	if report.Messages == nil || *report.Messages != 1200 {
		t.Errorf("messages = %v, want 1200", report.Messages)
	}
	if report.Results == nil || *report.Results != 1150 {
		t.Errorf("results = %v, want 1150", report.Results)
	}
	// Re-rendered from the parsed instant rather than echoed as written: a tag is
	// writable by whoever holds the run id, so nothing of the original string's
	// own shape reaches a model. The instant it named is unchanged.
	if report.RunHistoryEnd != "2026-06-01T00:00:00Z" {
		t.Errorf("run_history_end = %q, want the instant the tag named, normalised",
			report.RunHistoryEnd)
	}
	if report.RunTestEnd != "2026-06-08T00:00:00Z" {
		t.Errorf("run_test_end = %q, want the instant the tag named, normalised",
			report.RunTestEnd)
	}
}

// A tag that is not a timestamp does not travel: the field is read by a model, and
// a run that wrote prose there is a run whose operator chose that prose.
func TestSplitReportDropsATagThatIsNotATimestamp(t *testing.T) {
	split := &exposure.Split{
		TrainingEnd: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TestEnd:     time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	tags := map[string]string{
		operatorHistoryEndTag: "forged: leaked test-window value=123.456",
		operatorTestEndTag:    "2026-06-08T00:00:00+00:00",
	}
	params := map[string]string{
		paramEvaluationWindowStart: "also not a time",
		paramEvaluationWindowEnd:   "2026-06-08T00:00:00+00:00",
	}
	report := splitReport(split, tags, params, true)
	if report.RunHistoryEnd != "" {
		t.Errorf("run_history_end = %q, want it dropped", report.RunHistoryEnd)
	}
	if report.WindowStart != "" {
		t.Errorf("window_start = %q, want it dropped", report.WindowStart)
	}
	if report.Confirmed != splitNotConfirmed {
		t.Errorf("confirmed = %q, want %q", report.Confirmed, splitNotConfirmed)
	}
}

func TestSplitReportIsNotConfirmedWhenAFinishedRunsTagsAreAbsent(t *testing.T) {
	split := &exposure.Split{
		TrainingEnd: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TestEnd:     time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	report := splitReport(split, map[string]string{}, map[string]string{}, true)
	if report.Confirmed != splitNotConfirmed {
		t.Errorf("confirmed = %q, want %q for a finished run with no split tags",
			report.Confirmed, splitNotConfirmed)
	}
	if report.Note == "" {
		t.Error("note is empty, want it to explain the older-library symptom")
	}
}

func TestSplitReportIsNotConfirmedWhenAFinishedRunsTagsDisagree(t *testing.T) {
	split := &exposure.Split{
		TrainingEnd: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TestEnd:     time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	tags := map[string]string{
		operatorHistoryEndTag: "2026-06-02T00:00:00+00:00", // a day off
		operatorTestEndTag:    "2026-06-08T00:00:00+00:00",
	}
	report := splitReport(split, tags, map[string]string{}, true)
	if report.Confirmed != splitNotConfirmed {
		t.Errorf("confirmed = %q, want %q when the run's own tag disagrees",
			report.Confirmed, splitNotConfirmed)
	}
}

func TestSplitReportIsPendingWhileTheRunHasNotFinished(t *testing.T) {
	split := &exposure.Split{
		TrainingEnd: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		TestEnd:     time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
	}
	// No tags yet — the run has not reached the finally block that writes them —
	// and finished is false, which is the only thing that should read as pending
	// rather than as a silently-ignored split.
	report := splitReport(split, map[string]string{}, map[string]string{}, false)
	if report.Confirmed != splitPending {
		t.Errorf("confirmed = %q, want %q for a run still going", report.Confirmed, splitPending)
	}
	if report.Note != "" {
		t.Errorf("note = %q, want none while pending: nothing is wrong yet", report.Note)
	}
}

func TestParseTaggedSplitAcceptsWhatOperatorLibWrites(t *testing.T) {
	cases := []struct {
		name             string
		historyEnd, tEnd string
		wantOK           bool
	}{
		{"both isoformat with offset", "2026-06-01T00:00:00+00:00", "2026-06-08T00:00:00+00:00", true},
		{"fractional seconds", "2026-06-01T00:00:00.500000+00:00", "2026-06-08T00:00:00+00:00", true},
		{"history end missing", "", "2026-06-08T00:00:00+00:00", false},
		{"test end missing", "2026-06-01T00:00:00+00:00", "", false},
		{"unparseable", "not-a-time", "2026-06-08T00:00:00+00:00", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, ok := parseTaggedSplit(test.historyEnd, test.tEnd)
			if ok != test.wantOK {
				t.Errorf("ok = %v, want %v", ok, test.wantOK)
			}
		})
	}
}
