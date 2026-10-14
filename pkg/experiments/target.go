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

import "fmt"

// The launch's own reading of evaluation.yaml's target series against the topics
// it is about to send, so that a target the replay cannot resolve is said before
// the run spends its training rather than in a param read after it.
//
// Operator Lib resolves evaluation_target_series once the replay is over
// (_resolve_target_series in op_ml.py): over the mappings of every input and
// training topic, a mapping matches when its source without the first path
// element equals the series, and exactly one mapping must match or no metric is
// computed. The comparison here is that rule, with sourcePath standing for the
// library's _source_path, so the two cannot disagree about a launch both read
// the same way.

// targetSeriesMatches counts the mappings, across every topic whatever its role,
// whose source the library would resolve targetSeries to.
func targetSeriesMatches(targetSeries string, topics []InputTopic) int {
	matches := 0
	for _, topic := range topics {
		for _, mapping := range topic.Mappings {
			if mapping.Source != "" && sourcePath(mapping.Source) == targetSeries {
				matches++
			}
		}
	}
	return matches
}

// targetSeriesWarnings is the launch warning for a target series that would not
// resolve to exactly one mapping, or nothing when it would or when there is none.
//
// A warning, never a refusal: the library itself computes no metric in this case
// and records the reason in evaluation.metric_status, and the run's training is
// still worth having. Refusing would make a launch depend on a file read that
// exists only to grade the run afterwards, which Launch already declines to do for
// an unreadable evaluation.yaml.
//
// The words are the library's ("matches no input topic", "matches N mappings,
// expected exactly one"), so that the warning and the status that follows the run
// read as the same finding. What they leave out is the series itself and the
// mappings that matched it. The launch result is read by the model, and D38
// withholds the developer's target series from the model on every route, so the
// warning carries a count and nothing a model could read the series back out of.
//
// An empty targetSeries is no warning, compared exactly as the library compares
// it: the library treats only an empty value as unset, so a value of spaces is a
// series that matches nothing, there as here.
func targetSeriesWarnings(targetSeries string, topics []InputTopic) []string {
	if targetSeries == "" {
		return nil
	}
	switch matches := targetSeriesMatches(targetSeries, topics); {
	case matches == 0:
		return []string{fmt.Sprintf(
			"the target series in %s matches no input topic: no mapping of an input or "+
				"training topic reads that path, so the run will compute no metric over "+
				"the test window", EvaluationCriteriaPath)}
	case matches > 1:
		return []string{fmt.Sprintf(
			"the target series in %s matches %d mappings, expected exactly one, so the "+
				"run will compute no metric over the test window; keep the ground truth in "+
				"one mapping of one topic", EvaluationCriteriaPath, matches)}
	}
	return nil
}
