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

package simulation

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrInvalidRequest marks a caller's own mistake rather than the simulator's, so
// the tool surface can report it as invalid input instead of as a platform
// failure. Same role imports.ErrInvalidRequest plays for the import half.
var ErrInvalidRequest = errors.New("simulation: invalid request")

// ErrNotFound is an environment, dataset or job MOSES does not have — or does not
// let this developer see, which it answers the same way and for the same reason
// every other permissioned service does.
var ErrNotFound = errors.New("simulation: not found")

// ErrNotRunning is an environment that is stored but not simulated on the MOSES
// instance that was asked.
//
// It is not an error state and it is not "no data": an environment that was just
// written is not running yet, and another instance may be running it. It is kept
// apart from ErrNotFound because reporting one as the other is exactly the
// confusion the MOSES api documents itself against.
var ErrNotRunning = errors.New("simulation: the environment is not running on this instance")

// ErrUnknownField is a stored environment carrying a member this ODE's mirror
// does not know.
//
// It refuses a *write*, never a read. Every write to MOSES is a whole document,
// so writing back what this ODE understood would delete the field that a newer
// MOSES stored — silently, and from the developer's own environment. Refusing
// says what happened and leaves the MOSES UI as the way to edit that document.
var ErrUnknownField = errors.New("simulation: the environment carries a field this ODE does not know")

// VersionConflict is a write refused because the stored document moved on since
// it was read.
//
// The answer to it is never a retry: the document has to be read again and the
// change re-applied to what is there now. Retrying blind is the mirror image of
// the ODE-403 rule — there the write may already have landed, here it certainly
// did not, because MOSES refuses the write whole and deletes nothing on a refusal.
//
// Detail is MOSES's own message, which names both versions in prose. It is
// carried verbatim rather than parsed into a number: the number would be stale by
// the time anybody acted on it anyway, and the caller re-reads the document.
type VersionConflict struct {
	ID      string
	Carried int64
	Detail  string
}

func (e *VersionConflict) Error() string {
	message := fmt.Sprintf(
		"simulation: environment %s was changed since it was read at version %d",
		e.ID, e.Carried)
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

// UpstreamError is what MOSES answered when it was not a success.
type UpstreamError struct {
	Resource string
	Code     int
	Err      error
}

func (e *UpstreamError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("simulation: %s unreachable: %v", e.Resource, e.Err)
	}
	return fmt.Sprintf("simulation: %s answered %d: %v", e.Resource, e.Code, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// Is folds the status codes that mean something specific onto the sentinels
// above, so a caller writes errors.Is rather than reaching for the code.
//
// 400 and 409 are deliberately absent here. A 400 from MOSES is a validation
// error whose body names the offending fields, and it is wrapped as
// ErrInvalidRequest where it is produced so that the body travels with it; a 409
// on an environment write is a VersionConflict, which carries both versions and
// cannot be reconstructed from a code.
func (e *UpstreamError) Is(target error) bool {
	switch e.Code {
	case http.StatusNotFound:
		return target == ErrNotFound
	}
	return false
}
