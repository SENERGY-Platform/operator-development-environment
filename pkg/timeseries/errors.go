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
	"time"
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
	// Elapsed is how long the request took before it failed.
	//
	// It is here because the status code alone cannot separate the two things a 502
	// means, and the difference decides what to do next. A gateway that refused a
	// response for its size or its duration had to wait for the upstream to produce
	// it, so the failure arrives late. A gateway reporting an upstream that errored
	// or dropped the connection answers in milliseconds. Same code, opposite
	// remedies — see Immediate.
	Elapsed time.Duration
}

func (e *UpstreamError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("timeseries: %s: request failed after %s: %v",
			e.Resource, e.Elapsed.Round(time.Millisecond), e.Err)
	}
	return fmt.Sprintf("timeseries: %s: timescale-wrapper returned %d after %s: %v",
		e.Resource, e.Code, e.Elapsed.Round(time.Millisecond), e.Err)
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

// immediateFailure is how fast a failure has to come back before the size or the
// duration of a response can be ruled out as its cause.
//
// A second is generous on purpose. The read this matters for is bounded at tens of
// thousands of rows across several variables — megabytes of JSON the upstream has to
// query and serialise — so a refusal that is genuinely about that volume cannot
// arrive in under a second. Anything faster never produced the volume at all.
const immediateFailure = time.Second

// Immediate reports a failure that came back too fast to have been about the size or
// the duration of the response.
//
// This is what keeps ODE from giving the wrong advice on a 502. The status class says
// "the gateway could not get a usable answer", which covers both "the answer was too
// big" and "the service is unwell" — and the remedies are opposite: the first is
// answered by asking for less, the second by asking again later, or by someone
// looking at the service. Telling a developer to narrow their window while
// timescale-wrapper is down sends them turning a knob that cannot help.
//
// A 413 is exempt: the gateway said outright that the entity was too large, so its
// speed says nothing. A 504 is exempt for the mirror reason — a gateway timeout is by
// definition not immediate, and a fast one would be a misconfiguration rather than a
// signal about the response.
func (e *UpstreamError) Immediate() bool {
	if e.Code == http.StatusRequestEntityTooLarge || e.Code == http.StatusGatewayTimeout {
		return false
	}
	return e.Elapsed > 0 && e.Elapsed < immediateFailure
}
