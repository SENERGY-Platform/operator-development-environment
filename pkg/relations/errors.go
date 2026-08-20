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

package relations

import "errors"

var (
	// ErrInvalidRequest is a request ODE refuses rather than one the platform
	// refused. Classified as 400 by the API, and relayed to a model as a tool error
	// it can correct.
	ErrInvalidRequest = errors.New("invalid relation request")
	// ErrRelationNotFound is an unknown relation id.
	ErrRelationNotFound = errors.New("relation profile not found")
	// ErrUnknownRule is a decision aimed at a rule the named relation does not
	// carry. Refused rather than stored: a decision on a rule nobody can read back
	// is a record of nothing, and a mistyped fingerprint would become exactly that.
	ErrUnknownRule = errors.New("relation profile carries no such candidate rule")
	// ErrInvalidDecision is a developer decision missing what its action requires.
	ErrInvalidDecision = errors.New("invalid rule decision")
	// ErrTooFewMembers is a relational pass with nothing to relate. Two usable
	// members is the floor: the whole document is pairwise.
	ErrTooFewMembers = errors.New("a relation needs at least two members")
)
