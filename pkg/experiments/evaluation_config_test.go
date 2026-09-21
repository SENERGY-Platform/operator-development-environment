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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// The D37 addendum's four evaluation_* deployment config keys (deployment.go):
// read from the developer's own evaluation.yaml at the launch's commit, set only
// alongside the data split that makes them meaningful, and never allowed to fail
// a launch — the same "read, never a reason to refuse" shape criteria.go already
// keeps for grading a finished run.

type evaluationConfig struct {
	Config struct {
		EvaluationMetric          string `json:"evaluation_metric"`
		EvaluationTargetSeries    string `json:"evaluation_target_series"`
		EvaluationPredictionField string `json:"evaluation_prediction_field"`
		EvaluationResolution      string `json:"evaluation_resolution"`
	} `json:"config"`
}

func lastJobEvaluationConfig(t *testing.T, h *harness) (evaluationConfig, string) {
	t.Helper()
	job := h.ray.LastJob(t)
	raw, ok := job.RuntimeEnv.EnvVars["CONFIG"]
	if !ok {
		t.Fatal("the job carries no CONFIG, so Operator Lib has no deployment config to read")
	}
	var config evaluationConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("CONFIG is not the JSON Operator Lib parses: %v", err)
	}
	return config, raw
}

// A split, a criteria file naming all three keys: the deployment config carries
// all four evaluation_* settings, the criterion's own metric name among them.
func TestALaunchWithASplitAndCriteriaWritesTheEvaluationKeysIntoTheDeploymentConfig(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\ngoal: minimise\nthreshold: 5.0\n"+
		"target_series: sensor.ENERGY.Power\nprediction_field: prediction\nresolution: 1h\n")
	h.commit("State the real criterion and the library's own settings")

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	config, raw := lastJobEvaluationConfig(t, h)
	if config.Config.EvaluationMetric != "mae" {
		t.Errorf("evaluation_metric = %q, want the criterion's own metric name (CONFIG=%s)",
			config.Config.EvaluationMetric, raw)
	}
	if config.Config.EvaluationTargetSeries != "sensor.ENERGY.Power" {
		t.Errorf("evaluation_target_series = %q", config.Config.EvaluationTargetSeries)
	}
	if config.Config.EvaluationPredictionField != "prediction" {
		t.Errorf("evaluation_prediction_field = %q", config.Config.EvaluationPredictionField)
	}
	if config.Config.EvaluationResolution != "1h" {
		t.Errorf("evaluation_resolution = %q", config.Config.EvaluationResolution)
	}
}

// Without a split there is no replay to hand these four to, so none of them is
// set — even though the criteria file names all three keys. Operator Lib's own
// Config has no attribute for any of the four until a split switches init() into
// its evaluation mode, so a job that read them anyway would have nothing to do
// with them.
func TestALaunchWithoutASplitOmitsTheEvaluationKeysEvenWithCriteriaSet(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\ngoal: minimise\nthreshold: 5.0\n"+
		"target_series: sensor.ENERGY.Power\nprediction_field: prediction\nresolution: 1h\n")
	h.commit("State the real criterion and the library's own settings")

	h.launch()

	_, raw := lastJobEvaluationConfig(t, h)
	for _, key := range []string{
		"evaluation_metric", "evaluation_target_series",
		"evaluation_prediction_field", "evaluation_resolution",
	} {
		if strings.Contains(raw, key) {
			t.Errorf("CONFIG = %s, want no %q for an ordinary launch with no split", raw, key)
		}
	}
}

// A split with no evaluation.yaml at all — the commit ODE reads has none, which
// criteriaFor answers with ReasonNoCriteriaFile rather than an error. The launch
// still proceeds and the four keys are simply absent, exactly as if the developer
// had not written them.
func TestALaunchWithASplitAndNoCriteriaFileOmitsTheEvaluationKeys(t *testing.T) {
	h := newHarness(t)
	h.createRepository()
	h.removeFile("evaluation.yaml")
	h.commit("Start without criteria")

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	_, raw := lastJobEvaluationConfig(t, h)
	for _, key := range []string{
		"evaluation_metric", "evaluation_target_series",
		"evaluation_prediction_field", "evaluation_resolution",
	} {
		if strings.Contains(raw, key) {
			t.Errorf("CONFIG = %s, want no %q: the commit has no evaluation.yaml", raw, key)
		}
	}
}

// A criteria file ODE cannot parse must not fail the launch. A launch that died
// on an unreadable evaluation.yaml would be a new way for an existing repository
// to fail to start, over a file nothing before this feature ever depended on at
// launch time.
func TestALaunchSurvivesAnUnparseableCriteriaFileAndOmitsTheEvaluationKeys(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\nnested:\n\tthreshold: 0.3\n")
	h.commit("Reformat the criteria with a tab")

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	// h.launch itself calls t.Fatalf on any error from Launch, so reaching the
	// assertions below already proves the launch was accepted.
	h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	_, raw := lastJobEvaluationConfig(t, h)
	for _, key := range []string{
		"evaluation_metric", "evaluation_target_series",
		"evaluation_prediction_field", "evaluation_resolution",
	} {
		if strings.Contains(raw, key) {
			t.Errorf("CONFIG = %s, want no %q: the criteria file did not parse", raw, key)
		}
	}
}

// A criteria file naming only the metric, none of the other three — the three are
// independent and unset stays unset rather than half the group vanishing or the
// missing ones being invented.
func TestALaunchOmitsOnlyTheEvaluationKeysTheCriteriaFileDidNotName(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("evaluation.yaml", "metric: mae\ngoal: minimise\nthreshold: 5.0\n")
	h.commit("Name only the metric")

	trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	split := testSplit(trainingEnd, 6*time.Hour)
	h.launch(func(req *experiments.LaunchRequest) { req.Split = split })

	config, raw := lastJobEvaluationConfig(t, h)
	if config.Config.EvaluationMetric != "mae" {
		t.Errorf("evaluation_metric = %q, want the criterion's own name", config.Config.EvaluationMetric)
	}
	for _, key := range []string{
		"evaluation_target_series", "evaluation_prediction_field", "evaluation_resolution",
	} {
		if strings.Contains(raw, key) {
			t.Errorf("CONFIG = %s, want no %q: the file never named it", raw, key)
		}
	}
}
