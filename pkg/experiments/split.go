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
	"encoding/json"
	"fmt"
	"strings"
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
// in it is not an evaluation), and a test window that holds more input rows than
// the configured cap (the risk register's sequential-replay risk).

// SeriesReader is the one method of *timeseries.Client that sizing the test
// window needs: a batched POST /queries/v2. An interface rather than
// *timeseries.Client directly, and defined here rather than imported from
// pkg/tools, because pkg/tools imports pkg/experiments (the tool surface calls
// Launch) and the reverse import would cycle.
type SeriesReader interface {
	Query(
		ctx context.Context, token string, elements []timeseries.QueryElement,
		opts timeseries.QueryOptions,
	) ([]timeseries.QueryResult, error)
}

// countBucket is the GroupTime of the sizing query. Counts are summed over the
// buckets, so the width only bounds the response size: one row per day and column
// instead of one per message. Valid for timescale-wrapper's interval pattern.
const countBucket = "1d"

// serviceTopicPrefix is how operator-lib's replay tells a topic it reads through
// timescale-wrapper from one it reads from Kafka (operator_lib/util/helpers/data.py).
// A topic without it would also fail timescale-wrapper's service id check, and in
// one batched query that would refuse the whole launch.
const serviceTopicPrefix = "urn_infai_ses_service"

// resolveSplit is Launch's split handling in one place: normalise, validate,
// refuse a training end still in the future, and size the test window against
// the configured cap. Returns the normalised split (nil when the request carried
// none) and any warnings that do not themselves refuse the launch — an unsized
// window, or an input topic the count could not cover.
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

// sizeEvaluationWindow counts the input rows the test window replays and refuses a
// launch whose count exceeds MaxEvaluationRows.
//
// The count mirrors the replay (operator-lib, ts_wrapper.py and op_ml.py): every
// topic whose filter is a device is read through timescale-wrapper as one service
// of one device with one column per mapping, and the frames of all topics are
// concatenated, so infer() runs once per row of every topic. A topic's rows are
// counted, per bucket, as the largest count over its mapped columns, and the
// window's size is the sum over the topics. The replay's rows are the union of the
// columns' timestamps, so the largest column is exact when every message carries
// every mapped field, which one service's messages normally do, and a lower bound
// when the columns are filled in different messages; the sum of the columns would
// bound it from above, but would refuse the common case at k times its size for k
// mapped columns. Only a topic the replay reads through timescale-wrapper is
// counted, decided as the replay decides it (data.py: the topic name's service
// prefix) plus a device filter to count on; any other topic is named in a warning
// and is not part of the counted size. A nil Series (no timescale-wrapper configured) skips the count
// with a warning rather than refusing every split launch outright. A negative
// MaxEvaluationRows disables the cap, and with it the query.
func (s *Service) sizeEvaluationWindow(
	ctx context.Context, bearer string, topics []InputTopic, split exposure.Split,
) ([]string, error) {
	if s.opts.MaxEvaluationRows < 0 {
		return nil, nil
	}
	if s.opts.Series == nil {
		return []string{"the test window was not sized: no timeseries reader is configured"}, nil
	}

	var warnings []string
	var elements []timeseries.QueryElement
	start := split.TrainingEnd.UTC().Format(time.RFC3339)
	end := split.TestEnd.UTC().Format(time.RFC3339)
	count := "count"
	bucket := countBucket
	for _, topic := range topics {
		if !strings.HasPrefix(topic.Name, serviceTopicPrefix) {
			warnings = append(warnings, fmt.Sprintf(
				"the input topic %s (%s) is read from Kafka by the replay, which "+
					"timescale-wrapper cannot count, so it is not part of the counted window size",
				topic.Name, topic.FilterType))
			continue
		}
		if topic.FilterType != "DeviceId" {
			warnings = append(warnings, fmt.Sprintf(
				"the input topic %s is filtered by %s rather than by a device, so there is "+
					"no device to count it for and it is not part of the counted window size",
				topic.Name, topic.FilterType))
			continue
		}
		if len(topic.Mappings) == 0 {
			warnings = append(warnings, fmt.Sprintf(
				"the input topic %s has no mappings, so it is not part of the counted window size",
				topic.Name))
			continue
		}
		deviceID := topic.FilterValue
		serviceID := strings.ReplaceAll(topic.Name, "_", ":")
		columns := make([]timeseries.QueryColumn, 0, len(topic.Mappings))
		for _, mapping := range topic.Mappings {
			columns = append(columns, timeseries.QueryColumn{
				Name: sourcePath(mapping.Source), GroupType: &count,
			})
		}
		elements = append(elements, timeseries.QueryElement{
			DeviceId:  &deviceID,
			ServiceId: &serviceID,
			Columns:   columns,
			GroupTime: &bucket,
			Time:      &timeseries.QueryTime{Start: &start, End: &end},
		})
	}

	var rows int64
	if len(elements) > 0 {
		// No Split in the options, on purpose. The split's clamp lowers every
		// element's end to the training end, and the whole test window lies at or
		// after it, so the clamp would refuse or empty exactly the read this makes.
		// The read returns counts and never a value, so it does not leak the test
		// window's content to anything that sees ODE's output.
		results, err := s.opts.Series.Query(ctx, bearer, elements, timeseries.QueryOptions{})
		if err != nil {
			return nil, fmt.Errorf("sizing the evaluation window: %w", err)
		}
		sets, err := timeseries.DecodeResults(elements, results, "")
		if err != nil {
			return nil, fmt.Errorf("sizing the evaluation window: %w", err)
		}
		for _, set := range sets {
			rows += rowsOf(set)
		}
	}

	if rows > s.opts.MaxEvaluationRows {
		return nil, fmt.Errorf(
			"%w: the test window [%s, %s) holds %d input rows, over the "+
				"configured cap of %d; an evaluation replays infer() once per input row "+
				"in the driver, so narrow the window or the input topics, or raise "+
				"experiment_max_evaluation_rows",
			ErrInvalidRequest, split.TrainingEnd.Format(time.RFC3339),
			split.TestEnd.Format(time.RFC3339), rows, s.opts.MaxEvaluationRows)
	}
	return warnings, nil
}

// sourcePath is the column name the replay asks for: the mapping's source without
// its first path element, as operator-lib's _source_path computes it.
func sourcePath(source string) string {
	parts := strings.Split(source, ".")
	return strings.Join(parts[1:], ".")
}

// rowsOf sums, over a topic's buckets, the largest count among its columns. A
// null or unreadable count is 0.
func rowsOf(set timeseries.ResultSet) int64 {
	var rows int64
	for bucket := range set.Times {
		var widest int64
		for column := range set.Values {
			if n := countOf(set.Values[column][bucket]); n > widest {
				widest = n
			}
		}
		rows += widest
	}
	return rows
}

func countOf(value any) int64 {
	switch n := value.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
