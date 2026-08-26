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
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidRequest is a request ODE refused to make. The same name pkg/kernel,
// pkg/repo and pkg/timeseries use, so the API layer has one shape to switch on.
var ErrInvalidRequest = errors.New("invalid experiment request")

// ErrNotFound is an experiment id this developer does not have. Ownership is
// checked before existence, so it is also the answer for another developer's
// experiment — there is no route that reveals one exists.
var ErrNotFound = errors.New("no such experiment")

// ErrNoRepository means no repository is linked, so there is no committed state
// to submit. Distinct from ErrDirty because the repair is different: one needs a
// repository selected, the other needs a commit.
var ErrNoRepository = errors.New("no repository is linked, so there is no code to submit")

// DirtyError is the guard of §5.11 item 7, made concrete.
//
// A run is reproducible from a commit or it is not reproducible at all. Launching
// from a working copy that has uncommitted changes would record a SHA whose tree
// is not what ran, and no reader of the MLflow run could ever tell — the tag would
// look exactly like a correct one. So the launch is refused, and the refusal names
// the paths, because "commit your work" without saying which files is a worse
// answer than the pane already gives.
type DirtyError struct {
	Repository string
	// Paths are the uncommitted paths, capped: a developer who has not committed a
	// generated directory does not need four hundred of them to get the point.
	Paths []string
	// Elided is how many more there were.
	Elided int
	// Unborn is the special case of a repository with no commit at all, where the
	// answer is "make the first commit" rather than "commit these changes".
	Unborn bool
}

func (e *DirtyError) Error() string {
	if e.Unborn {
		return fmt.Sprintf(
			"the working copy of %s has no commit yet, and an experiment is submitted "+
				"from a commit so that its MLflow run is reproducible from one (SPEC §5.11 item 7)",
			e.Repository)
	}
	listed := strings.Join(e.Paths, ", ")
	if e.Elided > 0 {
		listed = fmt.Sprintf("%s and %d more", listed, e.Elided)
	}
	return fmt.Sprintf(
		"the working copy of %s has uncommitted changes (%s); an experiment is submitted "+
			"from the committed state so that its recorded commit SHA is the code that ran "+
			"(SPEC §5.11 item 7)",
		e.Repository, listed)
}

// PackageTooLargeError is the archive exceeding the configured bound.
//
// Reported rather than truncated, for D26's reason applied to a different object:
// a job that ran against a silently shortened copy of the repository would fail in
// a way nobody could diagnose from the run. The numbers are both here because the
// developer's next step depends on the gap — a .gitignore fix or a raised cap.
type PackageTooLargeError struct {
	Repository string
	CommitSHA  string
	Bytes      int64
	Limit      int64
}

func (e *PackageTooLargeError) Error() string {
	return fmt.Sprintf(
		"the job package for %s at %s is %d bytes, over the configured limit of %d; "+
			"exclude what the job does not need from the repository, or raise "+
			"experiment_max_package_bytes",
		e.Repository, shortSHA(e.CommitSHA), e.Bytes, e.Limit)
}

// UpstreamError is Ray's or MLflow's own verdict, kept whole for the reason
// kernel.UpstreamError and repo.UpstreamError are: the codes mean different
// things and flattening them to 500 loses every one. A 404 from MLflow's
// get-by-name is a normal step in creating an experiment; a 404 from Ray on a
// submission id means the cluster has forgotten the job.
type UpstreamError struct {
	// Service is "ray" or "mlflow", so a failure says which half is down.
	Service  string
	Resource string
	Code     int
	Message  string
	Err      error
}

func (e *UpstreamError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("%s: %s: request failed: %v", e.Service, e.Resource, e.Err)
	}
	return fmt.Sprintf("%s: %s: returned %d: %s", e.Service, e.Resource, e.Code, e.Message)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// TokenExchangeError is the identity provider refusing to mint a job credential.
//
// It never carries the response body. A token endpoint's error body is small and
// usually harmless, but it is the one body in this package that can contain a
// token, and a rule with an exception is not a rule (see §3.1 item 6 and the
// redaction practice in pkg/repo/git.go).
type TokenExchangeError struct {
	Code int
	// Cause is the OAuth `error` field only — a short machine code such as
	// invalid_grant. Never `error_description`, which echoes request parameters.
	Cause string
	Err   error
}

func (e *TokenExchangeError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("token exchange: the identity provider could not be reached: %v", e.Err)
	}
	if e.Cause == "" {
		return fmt.Sprintf("token exchange: the identity provider returned %d", e.Code)
	}
	return fmt.Sprintf("token exchange: the identity provider returned %d (%s)", e.Code, e.Cause)
}

func (e *TokenExchangeError) Unwrap() error { return e.Err }

// shortSHA is the seven characters a human reads a commit by.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
