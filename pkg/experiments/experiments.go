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

// Package experiments submits training runs to Ray and records them in MLflow
// (§5.12, D6, D17, D18, M8).
//
// Four properties shape everything in here, and each of them is a decision
// rather than an implementation detail.
//
//   - **A run is submitted from a commit, never from a working copy.** §5.11
//     item 7 says every experiment records the commit SHA as an MLflow tag so the
//     run is reproducible from a specific code state. That is only true if the
//     code Ray receives *is* that state, so the job package is built with
//     `git archive HEAD` — the committed tree — and a dirty working copy is
//     refused with the paths that made it dirty. A run whose code is not the
//     recorded SHA is worse than no run: it is a reproducibility claim that does
//     not hold, and nothing downstream could tell.
//
//   - **ODE creates the MLflow run, not the job.** The run is created here,
//     tagged here, and its id is passed into the job's environment and into
//     Ray's metadata. So "run tagged with commit SHA" holds whether or not the
//     developer's own training code remembers to tag, and `mlflow_run_id` can be
//     one of the four metadata keys §5.12 names at submission time — which it
//     could not be if the job minted its own run.
//
//   - **The job gets its own credential where one can be minted.** §3.1 item 6
//     and the risk register's "token expiry vs. long Ray jobs" row: a training
//     run outlives an interactive session, and a job reading training data
//     directly from timescale-wrapper (§5.3.4) with the caller's session token
//     would fail partway through. Where a Keycloak token exchange is configured,
//     one is minted per submission. Where it is not, the caller's token is passed
//     and the launch result *says so* — the limitation is in the answer rather
//     than discovered when a six-hour run dies at hour two.
//
//   - **Logs never reach a model.** §5.13 builds a compact structured summary
//     from params and metrics and says "never raw logs". Logs are read — a
//     developer needs them — but they are a route of their own and no part of
//     any tool result.
//
// Everything here speaks plain JSON over net/http, like pkg/timeseries and
// pkg/kernel, so neither Ray nor MLflow adds a dependency to this repository.
package experiments

import "time"

// Status is a Ray job's state, in Ray's own vocabulary.
//
// Not translated into ODE words: these strings appear in the Ray dashboard the
// developer can open beside the pane, and a second vocabulary would mean two
// answers to "what is it doing" that a developer has to reconcile themselves.
const (
	StatusPending   = "PENDING"
	StatusRunning   = "RUNNING"
	StatusStopped   = "STOPPED"
	StatusSucceeded = "SUCCEEDED"
	StatusFailed    = "FAILED"
)

// Terminal reports whether a status is one a job does not leave. A caller that
// polls uses it to stop polling; the summary uses it to decide whether the
// metrics it read are final or a snapshot of a run still in flight.
func Terminal(status string) bool {
	switch status {
	case StatusStopped, StatusSucceeded, StatusFailed:
		return true
	default:
		return false
	}
}

// Experiment is one submitted run, as ODE records it.
//
// This is the record the store keeps, and it is the one thing in M8 that is not
// recomputable from anywhere else: Ray forgets a submission when the cluster
// restarts, MLflow knows the run but not which ODE session or which working copy
// produced it, and the link between the two lives only here.
type Experiment struct {
	// ID is ODE's own identifier, the one that appears in URLs. Separate from
	// SubmissionID because the two have different owners: a Ray cluster may be
	// replaced, and a developer's own list of what they ran should survive that.
	ID      string `json:"experiment_id"`
	UserSub string `json:"-"`
	// SubmissionID is what Ray was asked to call the job. ODE mints it rather than
	// letting Ray, so a resubmission of the same request cannot silently become two
	// jobs: Ray rejects a duplicate submission id.
	SubmissionID string `json:"submission_id"`
	// RunID and MLflowExperimentID are the run ODE created before submitting.
	RunID                string `json:"mlflow_run_id"`
	MLflowExperimentID   string `json:"mlflow_experiment_id"`
	MLflowExperimentName string `json:"mlflow_experiment_name"`
	// SessionID is the chat session this was launched from, when it was. One of the
	// four metadata keys §5.12 names.
	SessionID string `json:"session_id,omitempty"`
	// WorkbenchID is the working context the package came from: which checkout, and
	// which kernel packaged it. Kept so an interpretation months later reads this
	// run's own evaluation.yaml rather than whichever workbench happens to be the
	// developer's only one by then.
	WorkbenchID string `json:"workbench_id,omitempty"`
	// Repository is owner/name, and CommitSHA the state the package was built from.
	Repository string `json:"repository"`
	CommitSHA  string `json:"commit_sha"`
	Branch     string `json:"branch,omitempty"`
	Entrypoint string `json:"entrypoint"`
	// PackageURI is the gcs:// URI of the working directory Ray unpacks, and
	// PackageBytes its size — reported because it is the one number a developer can
	// act on when a launch is refused for exceeding the cap.
	PackageURI   string `json:"package_uri"`
	PackageBytes int64  `json:"package_bytes"`
	// PackageReused says the archive was already on the cluster and was not
	// uploaded again. Two launches from the same commit produce the same bytes.
	PackageReused bool   `json:"package_reused"`
	Status        string `json:"status"`
	// Message is Ray's own, for a job that failed. Never a log.
	Message string `json:"message,omitempty"`
	// ScopedCredential says whether the job carries a token of its own (§3.1 item
	// 6) or the caller's session token. False is a supported deployment and a
	// stated limitation, not a fault — see Credential.
	ScopedCredential bool       `json:"scoped_credential"`
	SubmittedAt      time.Time  `json:"submitted_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
}

// Credential describes what the job will authenticate to the platform with, and
// how long that lasts.
//
// It is part of the launch result rather than a log line because the difference
// decides whether a long run is viable. A developer told "this credential expires
// with your session" can shorten the run or ask an operator to configure the
// exchange; a developer told nothing discovers it from a 401 in a Ray log.
type Credential struct {
	// Source is "exchanged" — a token minted for this job through the Keycloak
	// token exchange — or "session", the caller's own.
	Source string `json:"source"`
	// ExpiresIn is how long the job's token is good for, as the issuer reported it.
	// Zero when unknown, which is the honest answer for a session token: ODE does
	// not validate tokens (§3.1 step 2) and does not read their expiry.
	ExpiresIn int64 `json:"expires_in_seconds,omitempty"`
	// ExpiresWithSession is the limitation, stated. True means a run outliving the
	// developer's session will lose its platform access partway through.
	ExpiresWithSession bool `json:"expires_with_session"`
	// Note is the sentence a pane or a model shows. Written here rather than at
	// each call site so both say the same thing.
	Note string `json:"note,omitempty"`
}

// LaunchResult is one submission, made.
type LaunchResult struct {
	Experiment
	Credential Credential `json:"credential"`
	// TrackingURI is what the job was told MLflow is, so a developer can open the
	// run without knowing ODE's configuration.
	TrackingURI string `json:"mlflow_tracking_uri,omitempty"`
	// Warnings are things that did not stop the launch but that the developer
	// should read — a token exchange that answered with a shorter lifetime than the
	// deployment asked for, for instance.
	Warnings []string `json:"warnings,omitempty"`
}

// Summary is §5.13's compact structured summary: what the LLM is given about a
// finished run, and the only shape it is ever given.
//
// Params, metrics and tags — never an artifact, never stdout, and never a log.
// §5.13 is explicit about that, and the reason is the same as §4's: an LLM reading
// raw output is an LLM computing from raw data.
//
// The one thing that came out of a log is Failure, and it is not an exception to
// that rule but an application of it: a failed run's exception class, the last few
// frames and a message whose literals are masked below L2 (D34). No line of the log
// itself has a field to travel in, and there is still no tool that would fetch one.
type Summary struct {
	RunID        string `json:"run_id"`
	ExperimentID string `json:"experiment_id"`
	SubmissionID string `json:"submission_id"`
	CommitSHA    string `json:"commit_sha"`
	Repository   string `json:"repository,omitempty"`
	Entrypoint   string `json:"entrypoint,omitempty"`
	// Status is the run's, which is Ray's job status reconciled with MLflow's own.
	// Where they disagree Ray wins for a job still running and MLflow for a
	// finished one — see reconcile.
	Status string `json:"status"`
	// Finished says the run is in a state it will not leave, so a model can tell a
	// final metric from a snapshot of one still moving.
	Finished bool               `json:"finished"`
	Params   map[string]string  `json:"params"`
	Metrics  map[string]float64 `json:"metrics"`
	Tags     map[string]string  `json:"tags"`
	// ComparisonToPrevious is this run against the previous finished run of the
	// same MLflow experiment — which, per D17, is the same developer on the same
	// repository. Empty when there is no previous run, and that is worth reading as
	// "first run" rather than "no change".
	ComparisonToPrevious []MetricDelta `json:"comparison_to_previous"`
	// EvaluationCriteria is §5.13's block: the developer's own criterion, from their
	// `evaluation.yaml` at the run's commit, graded against what the run logged.
	//
	// **Always present, never absent.** A run with no criteria file, a file ODE
	// could not read, a summary built before the developer was back — each is a
	// criterion whose `met` is an explicit non-result with a reason, not a criterion
	// that is missing. Absence would be read as "there was nothing to meet"; D24 is
	// the same rule the profiler applies to a detector's field, and it applies here
	// for the same reason.
	EvaluationCriteria Criterion `json:"evaluation_criteria"`
	// SecondaryCriteria are the other metrics the file asks to watch. Beyond §5.13's
	// literal shape, and here because §5.11 item 3's scaffold has a
	// `secondary_metrics` key and silently dropping what a developer wrote there
	// would make the summary a partial reading of their own file.
	SecondaryCriteria []Criterion   `json:"secondary_criteria,omitempty"`
	ResourceUsage     ResourceUsage `json:"resource_usage"`
	// PreviousRunID names what the comparison is against, so a claim about an
	// improvement can be checked.
	PreviousRunID string     `json:"previous_run_id,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	// Note carries what a reader would otherwise have to infer: that a run is still
	// going, that there was nothing to compare against, that Ray and MLflow
	// disagreed.
	Note string `json:"note,omitempty"`
	// Failure is the last exception of a run that failed, or why there is none
	// (D34). Present exactly when the status is a failure, so a summary without one
	// is a run that did not fail rather than one that failed unaccountably.
	//
	// As built it is **raw**, and the developer's own route serves it that way. Every
	// path into a model's context goes through MaskedFor, which is where §3.2's
	// ladder is applied to it; failure.go says why the split is on that line.
	Failure *Failure `json:"failure,omitempty"`
}

// MetricDelta is one metric, this run against the previous.
type MetricDelta struct {
	Metric   string  `json:"metric"`
	Previous float64 `json:"previous"`
	Current  float64 `json:"current"`
	Delta    float64 `json:"delta"`
	// Direction is §5.13's "better|worse", and LowerIsBetter is how it was decided.
	//
	// The two are separate on purpose. Whether a smaller number is an improvement
	// is a property of the metric, and without the developer's evaluation criteria
	// ODE can only go by the name — loss, error, mae, rmse and mape count down,
	// everything else counts up. Carrying the rule beside the verdict means a model
	// reading this can say "assuming lower rmse is better" instead of asserting an
	// improvement it inferred from a naming convention.
	Direction     string `json:"direction"`
	LowerIsBetter bool   `json:"lower_is_better"`
}

// Criterion is one of the developer's evaluation criteria, applied to a run.
//
// Every field except Met is what the criteria file said; Met is the only judgement
// in here, and it is a Verdict rather than a bool so that "could not be evaluated"
// has somewhere to live other than "false" (§5.4.6, D24). Metric is empty exactly
// when there was no criterion to evaluate at all, and Met then says why.
type Criterion struct {
	Metric string `json:"metric,omitempty"`
	// Threshold is a pointer for the reason Value is: `omitempty` on a float64 would
	// hide a threshold of exactly zero, which is what §5.11 item 3's scaffold ships,
	// while a plain float64 put `"threshold": 0` into every criterion that has none —
	// a number the developer never wrote, in the document a model reads.
	Threshold *float64 `json:"threshold,omitempty"`
	// Value is what the run logged for the metric. A pointer because a metric of
	// exactly zero is a real reading and `omitempty` on a float64 would hide it,
	// while a metric the run never logged has to be visibly absent.
	Value *float64 `json:"value,omitempty"`
	// Met is true, false, or a not_computed object naming the reason.
	Met Verdict `json:"met"`
	// Goal is "minimise" or "maximise", and GoalStated says whether the file said so
	// or whether it was inferred from the metric's name. The pair is here for the
	// reason MetricDelta carries LowerIsBetter: a verdict whose rule is invisible
	// reads as a judgement, and "assuming lower rmse is better" is a sentence a
	// model should be able to say.
	Goal          string `json:"goal"`
	GoalStated    bool   `json:"goal_stated"`
	LowerIsBetter bool   `json:"lower_is_better"`
	// Source names where the criterion came from — the developer's file at a commit,
	// or the run's own tags — so nobody reads the verdict as ODE's own opinion.
	Source string `json:"source"`
}

// ResourceUsage is §5.13's block. Duration comes from the run's own timestamps;
// peak memory only if the job logged it as a metric, because nothing else in this
// path knows it.
type ResourceUsage struct {
	DurationSeconds float64 `json:"duration_s"`
	PeakMemoryMB    float64 `json:"peak_memory_mb,omitempty"`
	// PeakMemorySource is empty when the job reported no memory metric, which is
	// the normal case. Present rather than a silent zero, for D24's reason: a zero
	// read as "used no memory" would be a fabricated finding.
	PeakMemorySource string `json:"peak_memory_source,omitempty"`
}

// LogPage is what a developer's own pane reads. It exists so that the one thing
// §5.13 forbids in a model's context has a route that is plainly not a tool.
type LogPage struct {
	SubmissionID string `json:"submission_id"`
	Logs         string `json:"logs"`
	Truncated    bool   `json:"truncated"`
}

