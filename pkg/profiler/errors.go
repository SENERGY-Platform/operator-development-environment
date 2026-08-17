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

package profiler

import "errors"

var (
	// ErrInvalidRequest is a profile request ODE will not act on, as opposed to
	// one the platform refused.
	ErrInvalidRequest = errors.New("invalid profile request")
	// ErrInvalidOverride is a developer confirmation that does not name a
	// confirmable field, or is missing what its action requires.
	ErrInvalidOverride = errors.New("invalid profile override")
	// ErrNoPermission is the caller lacking execute on the device. Read governs
	// metadata; execute governs reading data (§5.1).
	ErrNoPermission = errors.New("no permission to read this device's data")
	// ErrNoVariables is a service whose outputs contain nothing addressable as a
	// series.
	ErrNoVariables = errors.New("service has no queryable variables")
)
