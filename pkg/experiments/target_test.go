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
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// The launch's check of evaluation.yaml's target series against the topics it
// sends (target.go): Operator Lib's rule — a mapping's source without its first
// path element, exactly one match across input and training topics — read before
// the run rather than after it, as a warning and never a refusal, and never naming
// the series itself (D38).

// targetWarnings are the launch warnings about the target series, out of every
// warning the launch returned: under a split without a timeseries reader the
// harness also warns that the window was not sized, which is not this check's.
func targetWarnings(warnings []string) []string {
	var out []string
	for _, w := range warnings {
		if strings.Contains(w, "target series") {
			out = append(out, w)
		}
	}
	return out
}

// launchScored commits an evaluation.yaml naming targetSeries and launches the
// given topics, under a split when split is set.
func launchScored(t *testing.T, targetSeries string, topics []experiments.InputTopic, split bool) experiments.LaunchResult {
	t.Helper()
	h := newHarness(t)
	h.ready()
	file := "metric: mae\ngoal: minimise\nthreshold: 5.0\nprediction_field: prediction\nresolution: 1h\n"
	if targetSeries != "" {
		file += "target_series: " + targetSeries + "\n"
	}
	h.write("evaluation.yaml", file)
	h.commit("State the criterion and its target series")

	return h.launch(func(req *experiments.LaunchRequest) {
		req.InputTopics = topics
		if split {
			trainingEnd := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
			req.Split = testSplit(trainingEnd, 6*time.Hour)
		}
	})
}

// labelOn is testTrainingTopic reading source instead of its own path.
func labelOn(source string) experiments.InputTopic {
	topic := testTrainingTopic()
	topic.Mappings = []experiments.TopicMapping{{Dest: "label", Source: source}}
	return topic
}

func TestALaunchWarnsWhenTheTargetSeriesMatchesNoMapping(t *testing.T) {
	// testInputTopics reads value.power.value, whose path is power.value; the
	// series names a path no topic maps.
	const target = "ENERGY.HeatPump"
	result := launchScored(t, target, testInputTopics(), true)

	found := targetWarnings(result.Warnings)
	if len(found) != 1 {
		t.Fatalf("target warnings = %v (all: %v), want one", found, result.Warnings)
	}
	if !strings.Contains(found[0], "matches no input topic") {
		t.Errorf("warning = %q, want the library's own words for no match", found[0])
	}
	assertSeriesWithheld(t, result.Warnings, target)
}

func TestALaunchWarnsWhenTheTargetSeriesMatchesTwoMappings(t *testing.T) {
	// The input and the label both map a power.value path: once each, on two topics.
	const target = "power.value"
	result := launchScored(t, target,
		append(testInputTopics(), labelOn("value.power.value")), true)

	found := targetWarnings(result.Warnings)
	if len(found) != 1 {
		t.Fatalf("target warnings = %v (all: %v), want one", found, result.Warnings)
	}
	if !strings.Contains(found[0], "matches 2 mappings, expected exactly one") {
		t.Errorf("warning = %q, want the count in the library's own words", found[0])
	}
	assertSeriesWithheld(t, result.Warnings, target)
}

// Exactly one match is the case that scores, whichever list the mapping is in:
// a target series mapped only in a training topic is the case training topics
// exist for, and resolves exactly as one mapped in an input does.
func TestALaunchDoesNotWarnWhenTheTargetSeriesMatchesOneMapping(t *testing.T) {
	cases := []struct {
		name   string
		target string
		topics []experiments.InputTopic
	}{
		{"in an input topic", "power.value", testInputTopics()},
		{"in a training topic", "energy.value", append(testInputTopics(), testTrainingTopic())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := launchScored(t, tc.target, tc.topics, true)
			if found := targetWarnings(result.Warnings); len(found) != 0 {
				t.Errorf("target warnings = %v, want none for exactly one match", found)
			}
		})
	}
}

// Without a split there is no replay and nothing is scored, so the series is not
// checked: the check would be about a metric no run computes.
func TestALaunchWithoutASplitDoesNotCheckTheTargetSeries(t *testing.T) {
	result := launchScored(t, "ENERGY.HeatPump", testInputTopics(), false)
	if found := targetWarnings(result.Warnings); len(found) != 0 {
		t.Errorf("target warnings = %v, want none for a launch without a split", found)
	}
}

// A file that names no target series has nothing to check; the library says in
// evaluation.metric_status that the setting is missing.
func TestALaunchWithoutATargetSeriesDoesNotWarnAboutOne(t *testing.T) {
	result := launchScored(t, "", testInputTopics(), true)
	if found := targetWarnings(result.Warnings); len(found) != 0 {
		t.Errorf("target warnings = %v, want none when the file names no target series", found)
	}
}

// assertSeriesWithheld fails when any warning carries the target series in any
// case: the launch result is read by the model, and D38 withholds the series
// from it.
func assertSeriesWithheld(t *testing.T, warnings []string, target string) {
	t.Helper()
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), strings.ToLower(target)) {
			t.Errorf("warning = %q names the target series %q, which the model may not read", w, target)
		}
	}
}
