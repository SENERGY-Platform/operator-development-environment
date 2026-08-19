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

package kernel

import (
	"errors"
	"fmt"
)

// ErrInvalidRequest is a request ODE refused to send, as opposed to one the Hub
// refused to answer. Mirrors timeseries.ErrInvalidRequest so the API layer has
// one shape to switch on.
var ErrInvalidRequest = errors.New("invalid kernel request")

// ErrNoKernel means this developer has no kernel running. Distinguished from a
// failure because the answer is "start one", not "something is broken".
var ErrNoKernel = errors.New("no kernel is running for this developer")

// ErrSpawnTimeout is a server that did not become ready in time. A cold start is
// 10-60s (§5.6), so this is a real outcome rather than an exceptional one, and
// the SPA reports it as "still starting" rather than as a fault.
var ErrSpawnTimeout = errors.New("the singleuser server did not become ready in time")

// ErrBusy is a second execution on a kernel that is already running one. ODE
// serialises per developer (see Service.Run), and this is what the second caller
// is told rather than being silently queued behind an unbounded wait.
var ErrBusy = errors.New("this kernel is already running code")

// UpstreamError carries JupyterHub's own verdict so the API layer can forward it
// rather than flattening everything to 500. Code 0 means the request never got
// an answer.
//
// Resource is the Hub path rather than a category, because the three failures
// that matter in practice — spawn refused, token minting refused, kernel API
// refused — are told apart by which path answered.
type UpstreamError struct {
	Resource string
	Code     int
	Err      error
}

func (e *UpstreamError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("kernel: %s: request failed: %v", e.Resource, e.Err)
	}
	return fmt.Sprintf("kernel: %s: jupyterhub returned %d: %v", e.Resource, e.Code, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// ScopeError is a Hub credential that cannot do the job. It fails startup rather
// than surfacing later as a 403 on someone's first spawn: a missing scope is a
// deployment fault, and the deployment is the only place it can be fixed.
type ScopeError struct {
	Missing []string
	Held    []string
	Kind    string
}

func (e *ScopeError) Error() string {
	return fmt.Sprintf(
		"kernel: the jupyterhub credential (kind %q) is missing the scopes %v; it holds %v. "+
			"Register ODE as a JupyterHub service with these scopes — see deploy/jupyterhub/README.md",
		e.Kind, e.Missing, e.Held)
}
