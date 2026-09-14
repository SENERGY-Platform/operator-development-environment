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
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// D36: a data split on the session that launches a run.

// fakeUsage is the double for experiments.UsageReader — the same figure
// estimate_read_cost reads in pkg/tools/executors.go, stood in here so Launch's
// window-size check can be tested without a timescale-wrapper.
type fakeUsage struct {
	bytesPerDay map[string]float64
	err         error
	// asked records every call's device ids, so a test can check what was asked.
	asked [][]string
}

func (f *fakeUsage) DeviceUsage(
	_ context.Context, _ string, deviceIDs []string,
) ([]timeseries.Usage, error) {
	f.asked = append(f.asked, deviceIDs)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]timeseries.Usage, 0, len(deviceIDs))
	for _, id := range deviceIDs {
		out = append(out, timeseries.Usage{DeviceId: id, BytesPerDay: f.bytesPerDay[id]})
	}
	return out, nil
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
	usage := &fakeUsage{bytesPerDay: map[string]float64{
		// 32 bytes/point (approxBytesPerPoint) * 1000 points/day.
		testDeviceID: 32 * 1000,
	}}
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Usage = usage
		deps.MaxEvaluationRows = 100
	})
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	// A week at 1000 points/day estimates 7000 rows, well over the cap of 100.
	split := testSplit(trainingEnd, 7*24*time.Hour)

	_, err := h.service.Launch(context.Background(), experiments.LaunchRequest{
		Request: h.request(), InputTopics: testInputTopics(), Split: split,
	})
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	for _, want := range []string{"7000", "100"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name both the estimate and the cap (%s)",
				err, want)
		}
	}
	if len(h.ray.Jobs()) != 0 {
		t.Error("a job was submitted over the configured evaluation row cap")
	}
}

// --- the accepted case: the deployment config and the stored record ---

func TestALaunchWithASplitWritesTheBoundsIntoTheDeploymentConfigAndTheRecord(t *testing.T) {
	usage := &fakeUsage{bytesPerDay: map[string]float64{testDeviceID: 320}} // 10 pts/day
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Usage = usage
	})
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)

	result := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	if result.Split == nil || !result.Split.Equal(*split) {
		t.Fatalf("result.Split = %+v, want the normalised split back", result.Split)
	}
	// The usage reader was asked about the device the one input topic names.
	if len(usage.asked) != 1 || len(usage.asked[0]) != 1 || usage.asked[0][0] != testDeviceID {
		t.Errorf("asked = %v, want one call naming %q", usage.asked, testDeviceID)
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

// No usage reader configured (the harness default, matching a deployment with no
// timescale-wrapper) skips the size check rather than refusing every split
// launch, and says so.
func TestALaunchWithASplitButNoUsageReaderIsAcceptedWithAWarning(t *testing.T) {
	h := newHarness(t)
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 6*time.Hour)

	result := h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "no usage reader is configured") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one saying the window was not sized", result.Warnings)
	}
}

// An OperatorId topic cannot be sized by DeviceUsage — it is not a device — so it
// is named in a warning rather than silently left out of the estimate or refused.
func TestATopicThatIsNotADeviceIsNamedInAWarningRatherThanRefused(t *testing.T) {
	usage := &fakeUsage{bytesPerDay: map[string]float64{testDeviceID: 320}}
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.Usage = usage
	})
	h.ready()

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour)
	split := testSplit(trainingEnd, 6*time.Hour)
	topics := append(testInputTopics(), experiments.InputTopic{
		Name:       "urn_infai_ses_operator_import",
		FilterType: "OperatorId",
		// operatorId:pipelineId, the form access.CheckTopics resolves for an
		// operator input that is not part of this deployment.
		FilterValue: "other-operator:other-pipeline",
		Mappings:    []experiments.TopicMapping{{Dest: "value", Source: "value"}},
	})

	result := h.launch(func(req *experiments.LaunchRequest) {
		req.Split = split
		req.InputTopics = topics
	})

	found := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "urn_infai_ses_operator_import") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want the OperatorId topic named", result.Warnings)
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

// The guard against a cluster image whose Operator Lib is older than v1.7.0: it
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
