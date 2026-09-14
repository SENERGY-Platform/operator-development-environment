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

import "testing"

// D37's refinement over the value reduction below: the timestamp latestMetrics
// hands MaskedFor is the maximum over a key's whole history, not the timestamp of
// whichever point the step-first value reduction happens to select.
//
// The two can differ. MlflowClient.log_metric takes both step and timestamp from
// its caller, so a write with a higher step wins the value reduction regardless of
// what its own timestamp says — which means a check of only the selected point's
// timestamp can be fed a backdated one while a different, lower-step point for the
// same key carries the real, later time. Scanning the whole history instead means
// that later point still counts, even though it lost the value reduction.
func TestLatestMetricsTimestampIsTheMaximumOverHistoryNotOfTheSelectedPoint(t *testing.T) {
	const cutoff = int64(1_700_000_000_000) // an arbitrary Unix-millis instant

	var run mlflowRun
	run.Data.Metrics = []struct {
		Key       string  `json:"key"`
		Value     float64 `json:"value"`
		Timestamp int64   `json:"timestamp"`
		Step      int64   `json:"step"`
	}{
		// Wins the value reduction (the higher step) and, on its own, would read as
		// logged before the cutoff.
		{Key: "rmse", Value: 0.5, Step: 1, Timestamp: cutoff - 1000},
		// Loses the value reduction (the lower step) but carries the real later
		// timestamp — logged after the cutoff.
		{Key: "rmse", Value: 0.9, Step: 0, Timestamp: cutoff + 1000},
	}

	values, times := latestMetrics(run)
	if values["rmse"] != 0.5 {
		t.Fatalf("value = %v, want the step-first reduction's own selection unchanged",
			values["rmse"])
	}
	if times["rmse"] != cutoff+1000 {
		t.Errorf("timestamp = %d, want the maximum over the key's history (%d) rather "+
			"than the selected point's own (%d)", times["rmse"], cutoff+1000, cutoff-1000)
	}
}

// A key with one point in its history is the ordinary case, and the maximum
// reduction must not report a later time than the run actually recorded.
func TestLatestMetricsTimestampOfAnUnrepeatedMetricIsItsOwn(t *testing.T) {
	var run mlflowRun
	run.Data.Metrics = []struct {
		Key       string  `json:"key"`
		Value     float64 `json:"value"`
		Timestamp int64   `json:"timestamp"`
		Step      int64   `json:"step"`
	}{
		{Key: "rmse", Value: 0.31, Step: 0, Timestamp: 1_700_000_000_000},
	}

	values, times := latestMetrics(run)
	if values["rmse"] != 0.31 || times["rmse"] != 1_700_000_000_000 {
		t.Errorf("value, timestamp = %v, %d, want the single point's own",
			values["rmse"], times["rmse"])
	}
}
