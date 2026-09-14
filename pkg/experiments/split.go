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
	"context"
	"fmt"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// A launch's data split (D36): what a session's Split becomes on a run.
//
// The split is refused early — before a package is built, before an MLflow run is
// opened — for the reason the dirty-working-copy check comes first: a launch that
// will not be allowed to train under its bounds should not spend anything getting
// there. Three refusals, in the order Launch applies them: the split's own shape
// (Validate), a training end that has not passed yet (a test window with no data
// in it is not an evaluation), and a test window estimated to exceed the
// configured row cap (the risk register's sequential-replay risk).

// UsageReader estimates a device's stored bytes per day, which is what
// pkg/tools/executors.go already reads for estimate_read_cost. An interface
// rather than *timeseries.Client directly, and defined here rather than imported
// from pkg/tools, because pkg/tools imports pkg/experiments (the tool surface
// calls Launch) and the reverse import would cycle.
type UsageReader interface {
	DeviceUsage(ctx context.Context, token string, deviceIDs []string) ([]timeseries.Usage, error)
}

// approxBytesPerPoint mirrors the constant of the same name in
// pkg/tools/executors.go — a rough stored size for one timestamped numeric
// point. Duplicated rather than imported for the reason UsageReader is: pkg/tools
// already imports this package, so the dependency cannot run the other way.
const approxBytesPerPoint = 32.0

// resolveSplit is Launch's split handling in one place: normalise, validate,
// refuse a training end still in the future, and size the test window against
// the configured cap. Returns the normalised split (nil when the request carried
// none) and any warnings that do not themselves refuse the launch — an unsized
// window, or an input topic the sizing could not account for.
func (s *Service) resolveSplit(
	ctx context.Context, req LaunchRequest,
) (*exposure.Split, []string, error) {
	if req.Split == nil {
		return nil, nil, nil
	}
	normalised := req.Split.Normalised()
	if err := normalised.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	if normalised.TrainingEnd.After(time.Now().UTC()) {
		return nil, nil, fmt.Errorf(
			"%w: the training end %s has not passed yet, so the test window holds no "+
				"data; launch after it or move the split",
			ErrInvalidRequest, normalised.TrainingEnd.Format(time.RFC3339))
	}
	warnings, err := s.sizeEvaluationWindow(ctx, req.Bearer, req.InputTopics, normalised)
	if err != nil {
		return nil, nil, err
	}
	return &normalised, warnings, nil
}

// sizeEvaluationWindow estimates how many input rows the test window replays and
// refuses a launch whose estimate exceeds MaxEvaluationRows.
//
// The estimate is the same shape estimate_read_cost already computes: bytes per
// day over an assumed bytes-per-point, times the number of days the window spans.
// Only a DeviceId topic can be sized this way — an OperatorId topic's usage is
// not something DeviceUsage answers — so an OperatorId topic is named in a
// warning rather than silently left out of the estimate, and a nil Usage (no
// timescale-wrapper configured) skips the estimate entirely, with a warning
// saying so rather than refusing every split launch outright.
func (s *Service) sizeEvaluationWindow(
	ctx context.Context, bearer string, topics []InputTopic, split exposure.Split,
) ([]string, error) {
	if s.opts.Usage == nil {
		return []string{"the test window was not sized: no usage reader is configured"}, nil
	}

	var warnings []string
	var deviceIDs []string
	for _, topic := range topics {
		if topic.FilterType == "DeviceId" {
			deviceIDs = append(deviceIDs, topic.FilterValue)
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"the input topic %s (%s) cannot be sized ahead of the run; only a device's "+
				"usage is known, so it is not part of the estimated window size",
			topic.Name, topic.FilterType))
	}

	var estimatedRows float64
	if len(deviceIDs) > 0 {
		usages, err := s.opts.Usage.DeviceUsage(ctx, bearer, deviceIDs)
		if err != nil {
			return nil, fmt.Errorf("sizing the evaluation window: %w", err)
		}
		bytesPerDay := make(map[string]float64, len(usages))
		for _, usage := range usages {
			bytesPerDay[usage.DeviceId] = usage.BytesPerDay
		}
		days := split.TestEnd.Sub(split.TrainingEnd).Hours() / 24
		for _, id := range deviceIDs {
			estimatedRows += bytesPerDay[id] / approxBytesPerPoint * days
		}
	}

	if int64(estimatedRows) > s.opts.MaxEvaluationRows {
		return nil, fmt.Errorf(
			"%w: the test window [%s, %s) is estimated at %.0f input rows, over the "+
				"configured cap of %d; an evaluation replays infer() once per input row "+
				"in the driver, so narrow the window or the input topics, or raise "+
				"experiment_max_evaluation_rows",
			ErrInvalidRequest, split.TrainingEnd.Format(time.RFC3339),
			split.TestEnd.Format(time.RFC3339), estimatedRows, s.opts.MaxEvaluationRows)
	}
	return warnings, nil
}
