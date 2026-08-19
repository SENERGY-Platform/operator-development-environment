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
	"net/http"
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

// Gateway reports a failure that happened between ODE and the service rather than
// inside it: the gateway could not get a usable answer, or gave up waiting, or
// refused the size.
//
// It matters because the two classes call for opposite responses. A 400 or a 500
// from timescale-wrapper is about the request or about the service, and the
// caller's own error text is the useful one. A 502, 503, 504 or 413 is about the
// *response* — too large, too slow, or the upstream dropped — and the useful thing
// to say is what to make smaller.
func (e *UpstreamError) Gateway() bool {
	switch e.Code {
	case http.StatusRequestEntityTooLarge,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
