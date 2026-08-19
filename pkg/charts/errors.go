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

package charts

import "errors"

var (
	// ErrInvalidSpec is a specification ODE refuses. Classified as 400 by the API,
	// and relayed to a model as a tool error it can correct — which is the point of
	// naming what is wrong rather than answering an empty chart.
	ErrInvalidSpec = errors.New("invalid chart specification")
	// ErrChartNotFound covers both "no such chart" and "not this developer's
	// chart". One error for the two on purpose: an owner-scoped id must not
	// distinguish "exists but is someone else's" from "does not exist".
	ErrChartNotFound = errors.New("chart not found")
	// ErrNotConfirmable is a confirmation aimed at a field outside
	// profiler.ConfirmablePaths (§5.10).
	ErrNotConfirmable = errors.New("field is not confirmable")
)
