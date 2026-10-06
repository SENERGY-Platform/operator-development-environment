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

package experiments_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// D36: a data split on the session that launches a run.

// fakeSeries is the double for experiments.SeriesReader, so Launch's window
// count can be tested without a timescale-wrapper. It records what it was asked
// and answers with canned results.
type fakeSeries struct {
	results []timeseries.QueryResult
	err     error
	// calls records every Query's elements and options.
	calls []seriesCall
}

type seriesCall struct {
	elements []timeseries.QueryElement
	opts     timeseries.QueryOptions
}

func (f *fakeSeries) Query(
	_ context.Context, _ string, elements []timeseries.QueryElement,
	opts timeseries.QueryOptions,
) ([]timeseries.QueryResult, error) {
	f.calls = append(f.calls, seriesCall{elements: elements, opts: opts})
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

// countResult builds one response element for request element index, with one
// series per column, each a list of [bucket, count] rows. A nil count is a null.
func countResult(index int, columns ...[]any) timeseries.QueryResult {
	data := make([][][]any, 0, len(columns))
	for _, column := range columns {
		series := make([][]any, 0, len(column))
		for day, count := range column {
			bucket := time.Date(2026, 9, 1+day, 0, 0, 0, 0, time.UTC).
				Format("2006-01-02T15:04:05.000Z07:00")
			series = append(series, []any{bucket, count})
		}
		data = append(data, series)
	}
	return timeseries.QueryResult{RequestIndex: index, Data: data}
}

// singleColumn is a canned response for the one-topic, one-column harness topic
// holding total rows over the window.
func singleColumn(total int) *fakeSeries {
	return &fakeSeries{results: []timeseries.QueryResult{
		countResult(0, []any{json.Number(strconv.Itoa(total))}),
	}}
}

// testDeviceID is the one testInputTopics() names.
const testDeviceID = "urn:infai:ses:device:2ac5436e-5538-4eb3-a448-2d77de68e915"

func testSplit(trainingEnd time.Time, testWindow time.Duration) *exposure.Split {
	return &exposure.Split{TrainingEnd: trainingEnd, TestEnd: trainingEnd.Add(testWindow)}
}

// --- the two refusals (§Launch, D36) ---

func TestALaunchWithATrainingEndStillInTheFutureIsRefused(t *testing.T) {
	h := newHarness(t)
	h.ready()

	future := time.Now().UTC().Add(24 * time.Hour)
	split := testSplit(future, 48*time.Hour)

	_, err := h.service.Launch(context.Background(), experiments.LaunchRequest{
		Request: h.request(), InputTopics: testInputTopics(), Split: split,
	})
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "has not passed yet") {
		t.Errorf("error = %q, want it to say the training end has not passed", err)
	}
	if len(h.ray.Jobs()) != 0 {
		t.Error("a job was submitted for a training end still in the future")
	}
	if names := h.mlflow.Experiments(); len(names) != 0 {
		t.Errorf("an MLflow experiment was created for a refused launch: %v", names)
	}
}

// The replay is one infer() call per input row in the driver (risk register), so
// the cap is refused on before anything is built — not after a job discovers it
// cannot finish.
func TestALaunchOverTheConfiguredEvaluationRowCapIsRefused(t *testing.T) {
	series := singleColumn(7000)
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Series = series
		deps.MaxEvaluationRows = 100
	})
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 7*24*time.Hour)

	_, err := h.service.Launch(context.Background(), experiments.LaunchRequest{
		Request: h.request(), InputTopics: testInputTopics(), Split: split,
	})
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	for _, want := range []string{"holds 7000 input rows", "100"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name both the count and the cap (%s)",
				err, want)
		}
	}
	if len(h.ray.Jobs()) != 0 {
		t.Error("a job was submitted over the configured evaluation row cap")
	}
}

// A window holding no more rows than the cap launches, and the count is the
// number the reader returned.
func TestALaunchWithinTheEvaluationRowCapIsAccepted(t *testing.T) {
	series := singleColumn(100)
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Series = series
		deps.MaxEvaluationRows = 100
	})
	h.ready()

	split := testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	if len(h.ray.Jobs()) != 1 {
		t.Errorf("jobs = %d, want the launch submitted at exactly the cap", len(h.ray.Jobs()))
	}
}

// The count must ask for what the replay reads, and must not carry the split: the
// clamp would lower the end to the training end and leave the test window empty.
func TestTheWindowCountAsksForWhatTheReplayReads(t *testing.T) {
	series := singleColumn(10)
	h := newHarness(t, func(deps *experiments.Deps) { deps.Series = series })
	h.ready()

	trainingEnd := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 24*time.Hour)
	h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	if len(series.calls) != 1 {
		t.Fatalf("queries = %d, want one batched call", len(series.calls))
	}
	call := series.calls[0]
	if call.opts.Split != nil {
		t.Errorf("opts.Split = %+v, want nil: the clamp would empty the test window", call.opts.Split)
	}
	if len(call.elements) != 1 {
		t.Fatalf("elements = %d, want one for the one device topic", len(call.elements))
	}
	el := call.elements[0]
	if el.DeviceId == nil || *el.DeviceId != testDeviceID {
		t.Errorf("deviceId = %v, want %q", el.DeviceId, testDeviceID)
	}
	if want := "urn:infai:ses:service:9ba92218-37d8-4c80-ad3d-bb3eb5c8457d"; el.ServiceId == nil ||
		*el.ServiceId != want {
		t.Errorf("serviceId = %v, want %q", el.ServiceId, want)
	}
	if len(el.Columns) != 1 || el.Columns[0].Name != "power.value" {
		t.Errorf("columns = %+v, want the one mapping's source without its first element", el.Columns)
	}
	for _, column := range el.Columns {
		if column.GroupType == nil || *column.GroupType != "count" {
			t.Errorf("column %q groupType = %v, want count", column.Name, column.GroupType)
		}
	}
	if el.GroupTime == nil || *el.GroupTime != "1d" {
		t.Errorf("groupTime = %v, want 1d", el.GroupTime)
	}
	if el.Time == nil || el.Time.Start == nil || el.Time.End == nil ||
		*el.Time.Start != split.TrainingEnd.Format(time.RFC3339) ||
		*el.Time.End != split.TestEnd.Format(time.RFC3339) {
		t.Errorf("time = %+v, want [%s, %s)", el.Time,
			split.TrainingEnd.Format(time.RFC3339), split.TestEnd.Format(time.RFC3339))
	}
	if !el.Valid() {
		t.Error("the element is not valid under timescale-wrapper's own schema")
	}
}

// A row exists when any mapped column has a value, so a bucket counts the
// largest of its columns; a null bucket counts nothing; topics add up.
func TestTheWindowCountTakesTheLargestColumnPerBucketAndSumsTopics(t *testing.T) {
	second := experiments.InputTopic{
		Name:        "urn_infai_ses_service_aaaaaaaa-37d8-4c80-ad3d-bb3eb5c8457d",
		FilterType:  "DeviceId",
		FilterValue: "urn:infai:ses:device:other",
		Mappings:    []experiments.TopicMapping{{Dest: "v", Source: "value.x"}},
	}
	first := testInputTopics()[0]
	first.Mappings = []experiments.TopicMapping{
		{Dest: "a", Source: "value.power.value"}, {Dest: "b", Source: "value.power.unit"},
	}
	series := &fakeSeries{results: []timeseries.QueryResult{
		// Day 1: max(5, 3) = 5. Day 2: max(nil, 4) = 4. Day 3: max(nil, nil) = 0.
		countResult(0,
			[]any{json.Number("5"), nil, nil},
			[]any{json.Number("3"), json.Number("4"), nil}),
		countResult(1, []any{json.Number("10"), json.Number("2")}),
	}}
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Series = series
		deps.MaxEvaluationRows = 20
	})
	h.ready()

	split := testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	_, err := h.service.Launch(context.Background(), experiments.LaunchRequest{
		Request: h.request(), InputTopics: []experiments.InputTopic{first, second}, Split: split,
	})
	// 5 + 4 + 0 + 10 + 2 = 21, one over the cap of 20.
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "holds 21 input rows") {
		t.Errorf("error = %q, want the counted 21", err)
	}
	if len(series.calls) != 1 || len(series.calls[0].elements) != 2 {
		t.Fatalf("calls = %+v, want one call carrying both topics", series.calls)
	}
	if cols := series.calls[0].elements[0].Columns; len(cols) != 2 ||
		cols[0].Name != "power.value" || cols[1].Name != "power.unit" {
		t.Errorf("columns = %+v, want power.value and power.unit", cols)
	}
}

// A negative cap disables the check: a window that would be refused is launched,
// and the reader is not asked at all.
func TestALaunchWithTheEvaluationRowCapDisabledIsNotSized(t *testing.T) {
	series := singleColumn(7000)
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Series = series
		deps.MaxEvaluationRows = -1
	})
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 7*24*time.Hour)

	h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	if len(h.ray.Jobs()) != 1 {
		t.Errorf("jobs = %d, want the launch submitted with the cap disabled",
			len(h.ray.Jobs()))
	}
	if len(series.calls) != 0 {
		t.Errorf("calls = %v, want no query with the cap disabled", series.calls)
	}
}

// --- the accepted case: the deployment config and the stored record ---

func TestALaunchWithASplitWritesTheBoundsIntoTheDeploymentConfigAndTheRecord(t *testing.T) {
	series := singleColumn(10)
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Series = series
	})
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)

	result := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	if result.Split == nil || !result.Split.Equal(*split) {
		t.Fatalf("result.Split = %+v, want the normalised split back", result.Split)
	}
	// The reader was asked about the device the one input topic names.
	if len(series.calls) != 1 || len(series.calls[0].elements) != 1 ||
		series.calls[0].elements[0].DeviceId == nil ||
		*series.calls[0].elements[0].DeviceId != testDeviceID {
		t.Errorf("calls = %+v, want one call naming %q", series.calls, testDeviceID)
	}

	job := h.ray.LastJob(t)
	var config struct {
		Config struct {
			TrainingEnd string `json:"training_end"`
			TestEnd     string `json:"test_end"`
		} `json:"config"`
	}
	raw, ok := job.RuntimeEnv.EnvVars["CONFIG"]
	if !ok {
		t.Fatal("the job carries no CONFIG, so Operator Lib has no deployment config to read")
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("CONFIG is not the JSON Operator Lib parses: %v", err)
	}
	if want := split.TrainingEnd.Format(time.RFC3339); config.Config.TrainingEnd != want {
		t.Errorf("config.training_end = %q, want %q", config.Config.TrainingEnd, want)
	}
	if want := split.TestEnd.Format(time.RFC3339); config.Config.TestEnd != want {
		t.Errorf("config.test_end = %q, want %q", config.Config.TestEnd, want)
	}

	// The stored record — read back through the store rather than through the
	// launch result, so a bug that set the field only on the returned value and
	// not on what Put received would be caught.
	stored, found, err := h.store.Get(context.Background(), testUserSub, result.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !found {
		t.Fatal("the launched experiment was not found in the store")
	}
	if stored.Split == nil || !stored.Split.Equal(*split) {
		t.Errorf("stored.Split = %+v, want the launch's split", stored.Split)
	}
}

// An ordinary launch — no split on the session — carries neither field, and
// Operator Lib's Config has no training_end or test_end attribute set at all so
// an operator deployed the ordinary way is unaffected.
func TestALaunchWithoutASplitWritesNeitherBoundIntoTheDeploymentConfig(t *testing.T) {
	h := newHarness(t)
	h.ready()

	result := h.launch()
	if result.Split != nil {
		t.Errorf("result.Split = %+v, want nil for an ordinary launch", result.Split)
	}

	job := h.ray.LastJob(t)
	if strings.Contains(job.RuntimeEnv.EnvVars["CONFIG"], "training_end") ||
		strings.Contains(job.RuntimeEnv.EnvVars["CONFIG"], "test_end") {
		t.Errorf("CONFIG = %s, want neither key present for an ordinary launch",
			job.RuntimeEnv.EnvVars["CONFIG"])
	}
}

// No reader configured (the harness default, matching a deployment with no
// timescale-wrapper) skips the size check rather than refusing every split
// launch, and says so.
func TestALaunchWithASplitButNoSeriesReaderIsAcceptedWithAWarning(t *testing.T) {
	h := newHarness(t)
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 6*time.Hour)

	result := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "no timeseries reader is configured") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one saying the window was not sized", result.Warnings)
	}
}

// A topic the replay reads from Kafka cannot be counted by timescale-wrapper, so
// it is named in a warning and not queried, rather than refused.
func TestATopicThatIsNotADeviceIsNamedInAWarningAndNotQueried(t *testing.T) {
	series := singleColumn(10)
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Series = series
	})
	h.ready()

	split := testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	topics := append(testInputTopics(), experiments.InputTopic{
		Name:        "urn_infai_ses_operator_import",
		FilterType:  "ImportId",
		FilterValue: "import-1",
		Mappings:    []experiments.TopicMapping{{Dest: "value", Source: "value"}},
	})

	result := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = split
		req.InputTopics = topics
	})

	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "urn_infai_ses_operator_import") &&
			strings.Contains(warning, "Kafka") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want the topic named with its real reason", result.Warnings)
	}
	if len(series.calls) != 1 || len(series.calls[0].elements) != 1 {
		t.Errorf("calls = %+v, want one call with the device topic only", series.calls)
	}
}

// The replay decides by the topic name, not by the filter, which topic it reads
// through timescale-wrapper. A device-filtered topic without the service prefix
// is read from Kafka, and counting it would send a service id timescale-wrapper
// rejects, refusing the whole batched launch.
func TestADeviceTopicWithoutTheServicePrefixIsNotQueried(t *testing.T) {
	series := singleColumn(10)
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Series = series
	})
	h.ready()

	topics := append(testInputTopics(), experiments.InputTopic{
		Name:        "some_device_topic",
		FilterType:  "DeviceId",
		FilterValue: "urn:infai:ses:device:other",
		Mappings:    []experiments.TopicMapping{{Dest: "value", Source: "value.power"}},
	})
	result := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
		req.InputTopics = topics
	})

	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "some_device_topic") && strings.Contains(warning, "Kafka") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want the topic named as read from Kafka", result.Warnings)
	}
	if len(series.calls) != 1 || len(series.calls[0].elements) != 1 {
		t.Errorf("calls = %+v, want one call with the service topic only", series.calls)
	}
}

// A launch whose only topic is not a device has nothing to count: no query, a
// warning, accepted.
func TestALaunchWithOnlyKafkaTopicsIssuesNoQuery(t *testing.T) {
	series := singleColumn(10)
	h := newHarness(t, func(deps *experiments.Deps) { deps.Series = series })
	h.ready()

	split := testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	h.launch(func(req *experiments.LaunchRequest) {
		req.Split = split
		req.InputTopics = []experiments.InputTopic{{
			Name: "import_topic", FilterType: "ImportId", FilterValue: "import-1",
			Mappings: []experiments.TopicMapping{{Dest: "value", Source: "value"}},
		}}
	})
	if len(series.calls) != 0 {
		t.Errorf("calls = %+v, want no query", series.calls)
	}
}

// A failing count fails the launch rather than letting an unsized window through.
func TestALaunchWhoseWindowCountFailsIsRefused(t *testing.T) {
	series := &fakeSeries{err: errors.New("upstream down")}
	h := newHarness(t, func(deps *experiments.Deps) { deps.Series = series })
	h.ready()

	split := testSplit(time.Now().UTC().Add(-24*time.Hour), 6*time.Hour)
	_, err := h.service.Launch(context.Background(), experiments.LaunchRequest{
		Request: h.request(), InputTopics: testInputTopics(), Split: split,
	})
	if err == nil || !strings.Contains(err.Error(), "sizing the evaluation window") ||
		!strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("error = %v, want the sizing failure wrapping the cause", err)
	}
	if len(h.ray.Jobs()) != 0 {
		t.Error("a job was submitted although the window could not be counted")
	}
}

// --- the summary's confirmation (step 16) ---

func TestASummaryConfirmsASplitWhoseRunTagsMatch(t *testing.T) {
	h := newHarness(t)
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	launched := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	h.mlflow.SetTag(t, launched.RunID, "operator_lib.history_end",
		split.TrainingEnd.Format(time.RFC3339))
	h.mlflow.SetTag(t, launched.RunID, "operator_lib.test_end",
		split.TestEnd.Format(time.RFC3339))
	h.mlflow.SetParam(t, launched.RunID, "evaluation.messages", "42")
	h.mlflow.SetParam(t, launched.RunID, "evaluation.results", "40")
	h.mlflow.Finish(t, launched.RunID, "FINISHED", nil)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.Split == nil {
		t.Fatal("summary.Split = nil, want the confirmation block for a split launch")
	}
	if summary.Split.Confirmed != "confirmed" {
		t.Errorf("confirmed = %q, want %q", summary.Split.Confirmed, "confirmed")
	}
	if summary.Split.Messages == nil || *summary.Split.Messages != 42 {
		t.Errorf("messages = %v, want 42", summary.Split.Messages)
	}
	if summary.Split.Results == nil || *summary.Split.Results != 40 {
		t.Errorf("results = %v, want 40", summary.Split.Results)
	}
	if strings.Contains(summary.Note, "not confirmed") {
		t.Errorf("note = %q, want no warning for a confirmed split", summary.Note)
	}
}

// The guard against a repository whose Operator Lib pin is older than v1.7.0: it
// reads no training_end or test_end from the config at all (simple_struct reads
// declared keys only), trains unbounded, and never writes the two tags.
func TestASummaryReportsAFinishedRunThatNeverConfirmedItsSplit(t *testing.T) {
	h := newHarness(t)
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 6*time.Hour)
	launched := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	// No tags: the double stands in for an older Operator Lib that dropped both
	// config fields silently and ran train_once() as if no split had been set.
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.5})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.Split == nil {
		t.Fatal("summary.Split = nil, want the confirmation block for a split launch")
	}
	if summary.Split.Confirmed != "not confirmed by the run" {
		t.Errorf("confirmed = %q, want %q", summary.Split.Confirmed, "not confirmed by the run")
	}
	if summary.Split.Note == "" {
		t.Error("split note is empty, want it to explain the older-library symptom")
	}
	if !strings.Contains(summary.Note, "not confirmed") {
		t.Errorf("summary note = %q, want the top-level note to flag it too", summary.Note)
	}
}

// A run still going has not reached the point where Operator Lib writes the two
// tags either way, so the confirmation is "pending" rather than a false alarm.
func TestASummaryReportsAnUnfinishedSplitLaunchAsPending(t *testing.T) {
	h := newHarness(t)
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 6*time.Hour)
	launched := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if summary.Split == nil {
		t.Fatal("summary.Split = nil, want the confirmation block for a split launch")
	}
	if summary.Split.Confirmed != "pending" {
		t.Errorf("confirmed = %q, want %q for a run still going",
			summary.Split.Confirmed, "pending")
	}
}

// MaskedFor exists for a failed run's exception, and a data split's report
// carries no values at all — so it must survive masking unchanged, at every tier.
func TestMaskedForLeavesTheSplitReportUntouched(t *testing.T) {
	h := newHarness(t)
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 6*time.Hour)
	launched := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	summary, err := h.service.Results(context.Background(), h.request(), launched.ID)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	for _, tier := range []exposure.Tier{exposure.L0, exposure.L1, exposure.L2} {
		masked := summary.MaskedFor(tier)
		if masked.Split == nil || *masked.Split != *summary.Split {
			t.Errorf("tier %s: split = %+v, want it unchanged by masking", tier, masked.Split)
		}
	}
}
