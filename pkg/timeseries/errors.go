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

package timeseries

import (
	"errors"
	"fmt"
)

// ErrInvalidRequest is a request ODE refused to send, as opposed to one the
// platform refused to answer.
var ErrInvalidRequest = errors.New("invalid timeseries request")

// UpstreamError carries the platform's own verdict so the API layer can
// forward it rather than flattening everything to 500. Code 0 means the request
// never got an answer.
type UpstreamError struct {
	Resource string
	Code     int
	Err      error
}

func (e *UpstreamError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("timeseries: %s: request failed: %v", e.Resource, e.Err)
	}
	return fmt.Sprintf("timeseries: %s: timescale-wrapper returned %d: %v", e.Resource, e.Code, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }
