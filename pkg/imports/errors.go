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

package imports

import (
	"fmt"
	"net/http"
)

// UpstreamError carries the platform's own verdict so the API layer can forward
// it rather than flattening everything to 500, as pkg/devices and pkg/timeseries
// do. Code 0 means the request never got an answer.
//
// Resource is the endpoint rather than a service name, because three services
// answer through this package — device-selection, import-deploy and
// import-repository — and "which one refused" is the first thing a reader needs.
type UpstreamError struct {
	Resource string
	Code     int
	Err      error
}

func (e *UpstreamError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("imports: %s: request failed: %v", e.Resource, e.Err)
	}
	return fmt.Sprintf("imports: %s: returned %d: %v", e.Resource, e.Code, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// NotFound separates "this import does not exist, or you may not see it" from a
// service failure. The two look the same to a caller that only reads the error
// text, and only the first is worth reporting to a developer as an answer.
func (e *UpstreamError) NotFound() bool {
	return e.Code == http.StatusNotFound
}

// Forbidden reports the platform refusing on permissions. Worth its own question
// because it is not a failure at all: it is the authorisation model working, and
// the honest answer is that this account cannot see that import.
func (e *UpstreamError) Forbidden() bool {
	return e.Code == http.StatusForbidden || e.Code == http.StatusUnauthorized
}
