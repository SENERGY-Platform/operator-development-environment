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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SENERGY-Platform/analytics-flow-engine/lib/access"
	servicejwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// Workspace is the developer's pod, as this package needs it. *kernel.Service
// implements it.
//
// One method, because one is all a job package needs: the archive is built by git
// in the pod and read back through the same command surface. Narrow for the reason
// tools.Repo is narrow — an interface is a statement about what this package may
// do to a developer's pod, and a wider one here would be the first step to a
// second way of writing their files.
type Workspace interface {
	Command(ctx context.Context, ref kernel.Ref, cmd kernel.Command) (kernel.CommandResult, error)
}

// Repository is the working copy's state, as this package needs it. *repo.Service
// implements it.
//
// Also one method, and read-only. This package refuses a dirty working copy; it
// does not commit one, and it must not be able to. §5.11 item 5 makes committing a
// developer action, and a launch that quietly committed to make itself possible
// would be exactly the silent commit that rules out.
type Repository interface {
	Status(ctx context.Context, req repo.StatusRequest) (repo.Status, error)
}

// Options is how a deployment configures this package.
type Options struct {
	// RayURL is the Ray dashboard's API base, e.g. http://ray-head:8265. Without it
	// there is no experiment surface at all.
	RayURL string
	// RayToken is the service account of §3.1 item 5. Empty is legitimate: a Ray
	// dashboard inside the cluster is commonly unauthenticated, and M10's
	// NetworkPolicy is what bounds who can reach it.
	RayToken string
	// RayDashboardURL is where a *browser* should open the dashboard, when that is
	// not the API base — a cluster-internal API and an ingress-exposed UI are
	// routinely different hosts. Used for the links the panes offer, never for an
	// API call.
	RayDashboardURL string

	// MLflowURL is the tracking server's API base, and MLflowUIURL the browser's
	// view of it, for the reason above.
	MLflowURL   string
	MLflowToken string
	MLflowUIURL string

	// ExperimentPrefix is what a developer's synthesised pipeline id is prefixed
	// with (D17), so ODE's runs are distinguishable from a deployed operator's in a
	// tracking server shared with the rest of the platform. It reaches the
	// experiment name through that id, because the name is the model registry key.
	ExperimentPrefix string

	// PyExecutable is what Ray starts worker processes with, matching whatever the
	// entrypoint launches the driver with. "uv run" is the default and the reason
	// the cluster image needs no operator dependencies of its own: uv builds the
	// environment from the repository's own pyproject.toml and uv.lock. Empty omits
	// the field, leaving Ray's own detection to it.
	PyExecutable string

	// RayClientURL is what a run's deployment config names as ray_url — what
	// Operator Lib hands to ray.init(). Not RayURL, which is the dashboard's HTTP
	// API. "auto" attaches to the cluster the driver is already running in.
	RayClientURL string
	// TimescaleWrapperURL is what a run reads history through, as the ts_wrapper_url
	// of its operator config.
	//
	// Not a DSN. Operator Lib supports both and prefers a DSN where it has one, but
	// a run executes the developer's own Python, so a database credential in its
	// environment is a credential handed to code ODE did not write. The wrapper
	// checks the developer's Execute permission on each device itself, which is the
	// authority the pre-operator-path read had and the operator path lost
	// (SNRGY-4637). The token it uses is the one SENERGY_TOKEN already carries.
	TimescaleWrapperURL string
	// KafkaBootstrap is the broker list a run's deployment config carries, for an
	// input topic replayed from Kafka rather than read from timescale. Empty leaves
	// a run able to train from timescale-backed topics only.
	KafkaBootstrap string

	// DefaultEntrypoint is what a launch that names no command runs. The scaffold
	// of §5.11 item 3 puts the Ray task in training.py, so that is what it points at.
	DefaultEntrypoint string

	// MaxPackageBytes bounds the job archive. Exceeding it is reported rather than
	// truncated: a job that ran against a partial repository fails in a way nobody
	// could diagnose from the run.
	MaxPackageBytes int64
	// MaxEnvVars bounds how many environment variables one launch may set, and
	// MaxEnvValueBytes how long one may be. Both bound what an LLM-authored launch
	// can push into a cluster's job spec.
	MaxEnvVars       int
	MaxEnvValueBytes int
	// MaxLogBytes bounds a log read. Logs go to the developer's own route and never
	// to a model (§5.13).
	MaxLogBytes int

	// RequestTimeout bounds one Ray or MLflow API call, and UploadTimeout the one
	// request that moves the whole archive.
	RequestTimeout time.Duration
	UploadTimeout  time.Duration
	// CommandTimeout bounds `git archive` in the pod.
	CommandTimeout time.Duration

	// The Keycloak token exchange of §3.1 item 6. All four are needed; any missing
	// one degrades to the caller's session token, and the launch result says so.
	KeycloakURL          string
	KeycloakRealm        string
	KeycloakClientID     string
	KeycloakClientSecret string
	// JobTokenAudience names the client the minted token is for. Keycloak returns a
	// token for the *requesting* client unless an audience says otherwise, and a job
	// reads timescale-wrapper — so without this the token is usually for the wrong
	// audience and the gateway rejects it.
	JobTokenAudience string
	// JobTokenLifetime is what the deployment believes it has configured in
	// Keycloak. It is not sent: neither RFC 8693 nor Keycloak takes a requested
	// lifetime, so this is checked against the issuer's own expires_in and a
	// shortfall becomes a warning on the launch.
	JobTokenLifetime time.Duration

	// Environment is what a job is told about the platform besides its own
	// credential — the same URLs kernelEnvironment pushes into a pod, for the same
	// reason: a job reads timeseries directly (§5.3.4) and should not need the
	// developer to restate where.
	Environment map[string]string
}

// Deps is what the service is built from.
type Deps struct {
	// Workspace and Repo are both required. The job package is the committed state
	// of a working copy that lives in the developer's pod, so without either there
	// is nothing to submit.
	Workspace Workspace
	Repo      Repository
	Store     Store
	IDs       IDs
	// HTTPClient is replaced by tests. One client for Ray, MLflow and Keycloak,
	// because they differ only in the host.
	HTTPClient *http.Client
	// Access authorizes a launch's input topics. Required: without it a launch
	// would read whatever series its topics name, which is the one thing in a
	// deployment config that decides what data a run sees.
	Access access.Checker
	Options
}

// IDs mints the experiment and submission identifiers. An interface so tests get
// deterministic ones, the same shape tools.IDs and chat.IDs use.
type IDs interface {
	NewID() string
}

// Service is ODE's experiment surface.
type Service struct {
	workspace Workspace
	repo      Repository
	store     Store
	ids       IDs
	http      *http.Client
	ray       *rayClient
	mlflow    *mlflowClient
	// criteria memoises the developer's evaluation.yaml per commit. Safe because a
	// commit's tree is immutable — see criteriaCache.
	criteria criteriaCache
	access   access.Checker
	opts     Options
}

const (
	defaultExperimentPrefix = "ode"
	defaultEntrypoint       = "python training.py"
	// defaultMaxPackageBytes is 16 MiB. Source repositories are three orders of
	// magnitude smaller; what this actually bounds is a repository that has grown a
	// checked-in model file or a data directory, and 16 MiB is where that becomes
	// obvious without refusing anything reasonable. It also bounds memory here: the
	// archive is held whole, and base64 through the kernel costs a third more again.
	defaultMaxPackageBytes   = 16 << 20
	defaultMaxEnvVars        = 32
	defaultMaxEnvValueBytes  = 4096
	defaultMaxLogBytes       = 1 << 20
	defaultRequestTimeout    = 30 * time.Second
	defaultUploadTimeout     = 5 * time.Minute
	defaultCommandTimeout    = 5 * time.Minute
)

// New builds the service.
//
// It refuses rather than degrades on the four things that cannot be defaulted: a
// Ray cluster, a tracking server, somewhere to build a package, and a repository
// to build it from. A deployment missing any of them has no experiment surface at
// all, which startM8 turns into a warning and an absent route group — the same
// degradation the profiler, the kernel and the repo surface already do.
func New(deps Deps) (*Service, error) {
	if deps.RayURL == "" {
		return nil, errors.New("experiments: a ray_url is required (§5.12)")
	}
	if deps.MLflowURL == "" {
		return nil, errors.New("experiments: an mlflow_url is required (§5.12)")
	}
	if deps.Workspace == nil {
		return nil, errors.New(
			"experiments: a workspace is required: the job package is built by git in " +
				"the developer's pod (§5.11 item 5)")
	}
	if deps.Repo == nil {
		return nil, errors.New(
			"experiments: a repository service is required: a run is submitted from a " +
				"commit, not from a working copy (§5.11 item 7)")
	}
	if deps.Store == nil {
		return nil, errors.New("experiments: a store is required")
	}
	if deps.IDs == nil {
		return nil, errors.New("experiments: an id source is required")
	}
	if deps.Access == nil {
		// Refused rather than skipped. A launch decides what data a run reads, and a
		// deployment that forgot to wire this would authorize nothing while looking
		// like it worked -- the failure this check exists to prevent.
		return nil, errors.New(
			"experiments: a permission checker is required: a launch authorizes its " +
				"input topics as the developer (SNRGY-4637)")
	}

	opts := deps.Options
	opts.RayURL = strings.TrimRight(opts.RayURL, "/")
	opts.MLflowURL = strings.TrimRight(opts.MLflowURL, "/")
	if opts.ExperimentPrefix == "" {
		opts.ExperimentPrefix = defaultExperimentPrefix
	}
	if opts.DefaultEntrypoint == "" {
		opts.DefaultEntrypoint = defaultEntrypoint
	}
	if opts.MaxPackageBytes <= 0 {
		opts.MaxPackageBytes = defaultMaxPackageBytes
	}
	if opts.MaxEnvVars <= 0 {
		opts.MaxEnvVars = defaultMaxEnvVars
	}
	if opts.MaxEnvValueBytes <= 0 {
		opts.MaxEnvValueBytes = defaultMaxEnvValueBytes
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = defaultMaxLogBytes
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}
	if opts.UploadTimeout <= 0 {
		opts.UploadTimeout = defaultUploadTimeout
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = defaultCommandTimeout
	}
	if opts.JobTokenLifetime <= 0 {
		opts.JobTokenLifetime = defaultJobTokenLifetime
	}

	client := deps.HTTPClient
	if client == nil {
		// No client-level Timeout: every request carries its own deadline, and a
		// client Timeout would silently win over the longer one the archive upload
		// needs. The same reasoning pkg/timeseries records.
		client = &http.Client{}
	}

	return &Service{
		workspace: deps.Workspace,
		repo:      deps.Repo,
		store:     deps.Store,
		ids:       deps.IDs,
		access:    deps.Access,
		http:      client,
		ray: &rayClient{
			baseURL: opts.RayURL, token: opts.RayToken, http: client,
			timeout: opts.RequestTimeout, upload: opts.UploadTimeout,
		},
		mlflow: &mlflowClient{
			baseURL: opts.MLflowURL, token: opts.MLflowToken, http: client,
			timeout: opts.RequestTimeout,
		},
		opts: opts,
	}, nil
}

// TrackingURI is the MLflow URL jobs are pointed at, reported by /session so the
// SPA can link a run without knowing ODE's configuration.
func (s *Service) TrackingURI() string { return s.opts.MLflowURL }

// DashboardURL is where a browser should open Ray.
func (s *Service) DashboardURL() string {
	return firstNonEmpty(s.opts.RayDashboardURL, s.opts.RayURL)
}

// TrackingUIURL is where a browser should open MLflow.
func (s *Service) TrackingUIURL() string {
	return firstNonEmpty(s.opts.MLflowUIURL, s.opts.MLflowURL)
}

// firstNonEmpty is the fallback both URLs above use: a deployment that exposes one
// host to the browser and another to ODE sets both, and one that does not sets the
// API base alone.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Request is what every operation needs: whose pod, whose repository, and who is
// asking. The same shape repo.Request has, and for the same reason — the bearer
// addresses a pod and the subject keys a record, and both come from one validated
// token.
type Request struct {
	Bearer  string
	UserSub string
	// Username is the developer's Hub username, used to name their MLflow
	// experiment (D17). Empty falls back to the subject, which is stable but
	// unreadable in MLflow's own UI.
	Username string
	// WorkbenchID names the working context this is about: which checkout the
	// commit comes from, and which kernel packages it. Empty is the developer's
	// only workbench, as everywhere else.
	WorkbenchID string
	// SessionID is the chat session a launch came from, when it came from one. One
	// of the four metadata keys §5.12 names.
	SessionID string
	Author    repo.Author
}

// LaunchRequest is one experiment to submit.
type LaunchRequest struct {
	Request
	// Entrypoint is the command Ray runs in the unpacked working directory. Empty
	// takes the deployment's default.
	Entrypoint string
	// EnvVars are extra environment variables for the job. Bounded and validated:
	// they arrive from an HTTP body or from an LLM tool call, and the reserved names
	// ODE sets itself cannot be overridden from either.
	EnvVars map[string]string
	// RunName is what the MLflow run is called. Empty derives one from the commit.
	RunName string
	// InputTopics are the operator's inputs, which decide what history the run
	// reads. They travel in the deployment config rather than as loose variables,
	// because that is where Operator Lib looks for them.
	InputTopics []InputTopic
}

// envNamePattern is what a variable name may look like. Stricter than POSIX
// allows, for the reason repo.validBranch is stricter than git: a name ODE would
// have to think about escaping is better refused than escaped.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Launch submits one experiment.
//
// The order is the milestone. The working copy is checked first and the launch
// refused if it is dirty, because everything after that point records a commit SHA
// as the truth about what ran. Then the package is built from that commit, then
// the MLflow run is created and tagged — before Ray is asked for anything, so the
// run id can be in the submission's metadata as §5.12 requires — and only then is
// the job submitted.
//
// The consequence of that order, stated rather than hidden: a submission that
// fails leaves a created MLflow run behind. ODE records the *experiment* as FAILED
// with the cluster's refusal in its message and closes the run it opened, with
// FAILED and an end time, because nothing else will. Deleting it is not right
// either: a run that existed and failed to launch is a fact about the developer's
// day, and MLflow's own delete is a developer action.
func (s *Service) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	status, err := s.repo.Status(ctx, repo.StatusRequest{
		Request: repo.Request{
			Bearer: req.Bearer, UserSub: req.UserSub, Author: req.Author,
			WorkbenchID: req.WorkbenchID,
		},
	})
	if err != nil {
		return LaunchResult{}, err
	}
	commitSHA, err := requireCommittedState(status)
	if err != nil {
		return LaunchResult{}, err
	}

	entrypoint := strings.TrimSpace(req.Entrypoint)
	if entrypoint == "" {
		entrypoint = s.opts.DefaultEntrypoint
	}
	environment, err := s.jobEnvironment(req.EnvVars)
	if err != nil {
		return LaunchResult{}, err
	}
	// After the caller's own variables and before the package is built, for the
	// reason the dirty working copy check comes first: everything past this point
	// spends something.
	if err := validateTopics(req.InputTopics); err != nil {
		return LaunchResult{}, err
	}
	if err := requireInputTopics(req.InputTopics); err != nil {
		return LaunchResult{}, err
	}
	// Before the package is built, for the reason above: this is the check that
	// refuses, and everything past it spends something. Shared with the flow
	// engine, which applies the same rule when it deploys a pipeline -- Operator
	// Lib performs no check of its own, so whichever service wrote the topics is
	// the only one able to refuse.
	//
	// An experiment is a single operator, so it has no intra-deployment operator
	// wiring to exempt and passes no InternalOperatorIDs.
	if err := access.CheckTopics(s.access, req.Bearer, asPipeTopics(req.InputTopics),
		access.Options{}); err != nil {
		return LaunchResult{}, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}

	experimentID := s.ids.NewID()
	submissionID := s.ids.NewID()

	// The workbench comes from the link rather than from the request: the request
	// may name none because the developer has one, while the link always names the
	// concrete one whose kernel holds that checkout.
	built, err := s.buildArchive(ctx, kernel.Ref{
		Bearer: req.Bearer, Workbench: status.Link.WorkbenchID,
	}, status.Link.FullName, status.Link.Path, commitSHA, packagePath(submissionID))
	if err != nil {
		return LaunchResult{}, err
	}

	name := packageName(built.Bytes)
	uri := packageURI(name)
	present, err := s.ray.packageExists(ctx, name)
	if err != nil {
		return LaunchResult{}, err
	}
	if !present {
		if err := s.ray.uploadPackage(ctx, name, built.Bytes); err != nil {
			return LaunchResult{}, err
		}
	}

	// The experiment is D17's, one per developer per repository: Store.Previous
	// scopes the comparison to it, so a per-run experiment would make every run a
	// first run.
	//
	// It is named modelID rather than a namespace of ODE's own, and that is not a
	// preference. MLOperator calls set_experiment(model_id) before it resumes the
	// run MLFLOW_RUN_ID names, and mlflow's fluent start_run refuses a resume whose
	// run lives in a different experiment than the active one — so a separately
	// named experiment failed every launch inside operator.init(). It costs nothing
	// to agree: both ids are already stable per developer and per repository, which
	// is exactly the granularity D17 asks for, and the same string is the model
	// registry key — see deployment.go for why the stable pair needed Operator Lib
	// v1.4.0.
	pipelineID := s.pipelineID(req.Request)
	operatorIdentifier := operatorID(repositoryOf(status.Link))
	mlflowExperimentName := modelID(pipelineID, operatorIdentifier)
	mlflowExperimentID, err := s.mlflow.ensureExperiment(ctx, mlflowExperimentName, []mlflowTag{
		{Key: TagUserSub, Value: req.UserSub},
		{Key: TagRepository, Value: status.Link.FullName},
		{Key: TagSource, Value: "ode"},
	})
	if err != nil {
		return LaunchResult{}, err
	}

	submittedAt := time.Now().UTC()
	record := Experiment{
		ID:                   experimentID,
		UserSub:              req.UserSub,
		SubmissionID:         submissionID,
		MLflowExperimentID:   mlflowExperimentID,
		MLflowExperimentName: mlflowExperimentName,
		SessionID:            req.SessionID,
		WorkbenchID:          status.Link.WorkbenchID,
		Repository:           status.Link.FullName,
		CommitSHA:            commitSHA,
		Branch:               status.Branch,
		Entrypoint:           entrypoint,
		PackageURI:           uri,
		PackageBytes:         int64(len(built.Bytes)),
		PackageReused:        present,
		Status:               StatusPending,
		SubmittedAt:          submittedAt,
		UpdatedAt:            submittedAt,
	}

	// The run is ODE's, created before the job and tagged in the same request. That
	// is what makes "run tagged with commit SHA" hold whether or not the job's own
	// code remembers to tag, and what lets mlflow_run_id be in Ray's metadata at
	// submission time (§5.12).
	runID, err := s.mlflow.createRun(ctx, mlflowExperimentID, runName(req.RunName, commitSHA),
		submittedAt, runTags(record))
	if err != nil {
		return LaunchResult{}, err
	}
	record.RunID = runID

	credential, warnings := s.jobToken(ctx, req.Bearer)
	record.ScopedCredential = credential.Source == credentialExchanged

	deployment, err := s.deploymentEnvironment(
		record, pipelineID, operatorIdentifier, req.InputTopics, runID)
	if err != nil {
		return LaunchResult{}, err
	}
	for key, value := range deployment {
		environment[key] = value
	}
	// The developer's own authorisation, never ODE's (§3.1 step 3). Where a token
	// exchange is configured this is a token minted for the job on their behalf;
	// where it is not, it is their session token and Credential says so.
	environment["SENERGY_TOKEN"] = credential.Token

	accepted, err := s.ray.submit(ctx, jobSubmission{
		Entrypoint:   entrypoint,
		SubmissionID: submissionID,
		RuntimeEnv: jobRuntimeEnv{
			WorkingDir: uri,
			EnvVars:    environment,
			// The workers' interpreter, matching the driver's. See jobRuntimeEnv.
			PyExecutable: s.opts.PyExecutable,
		},
		// The four keys §5.12 names, and nothing else: Ray's metadata is visible to
		// anyone who can read the cluster's job list, so it carries identifiers rather
		// than anything about the developer.
		Metadata: map[string]string{
			"session_id":    req.SessionID,
			"user_sub":      req.UserSub,
			"commit_sha":    commitSHA,
			"mlflow_run_id": runID,
		},
	})
	if err != nil {
		record.Status = StatusFailed
		record.Message = "the job was not accepted by the cluster: " + err.Error()
		ended := time.Now().UTC()
		record.EndedAt = &ended
		record = touch(record)
		// The run ODE opened is closed here, because nothing else will.
		//
		// M8 left this: the run is created before the job is submitted, so a refused
		// submission leaves a run that MLflow's UI shows RUNNING forever beside an ODE
		// record that says the launch failed — two answers to "what happened", and the
		// one a developer sees first is wrong. Deleting it is not the fix either: a run
		// that existed and failed to launch is a fact, and MLflow's own delete is a
		// developer action. So it is closed with FAILED and an end time.
		//
		// Detached from ctx: the caller may already be gone, and a run left open
		// because a request was cancelled is the same wart in a different disguise.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.RequestTimeout)
		if closeErr := s.mlflow.updateRun(closeCtx, runID, mlflowFailed, ended); closeErr != nil {
			slog.WarnContext(ctx, "the mlflow run of a refused submission could not be closed; "+
				"it will read RUNNING in mlflow until someone closes it by hand",
				"experiment", record.ID, "mlflow_run", runID, "error", closeErr)
		}
		cancel()
		// Stored anyway. A run exists in MLflow with this record's id as a tag, and a
		// developer who sees it there should be able to find out from ODE what
		// happened rather than being told the experiment never existed.
		if storeErr := s.store.Put(ctx, record); storeErr != nil {
			slog.ErrorContext(ctx, "an experiment could not be recorded after a failed submission",
				"experiment", record.ID, "error", storeErr)
		}
		return LaunchResult{}, err
	}
	if accepted != submissionID {
		// Ray echoes the submission id it accepted. A different one means the cluster
		// renamed the job, and every subsequent poll of ODE's id would 404.
		record.SubmissionID = accepted
		// The run was tagged with the id ODE asked for, which is now wrong. This is the
		// one case setTag exists for: the run is ODE's, so its tags have to describe
		// what actually happened rather than what was requested. A failure here is a
		// stale tag rather than a failed launch, so it is logged and not returned.
		if err := s.mlflow.setTag(ctx, runID, TagSubmissionID, accepted); err != nil {
			slog.WarnContext(ctx, "the cluster renamed the submission and the run's tag "+
				"could not be corrected",
				"experiment", record.ID, "requested", submissionID, "accepted", accepted,
				"error", err)
		}
	}

	if err := s.store.Put(ctx, record); err != nil {
		// The job is already running and now has no record. Nothing here can undo the
		// submission — stopping a run the developer asked for because ODE could not
		// write a row would be worse — so the identifiers are logged at ERROR, which
		// is what makes the run recoverable by hand rather than orphaned silently.
		slog.ErrorContext(ctx, "an experiment was submitted but could not be recorded",
			"experiment", record.ID, "submission", record.SubmissionID,
			"mlflow_run", record.RunID, "commit", shortSHA(record.CommitSHA), "error", err)
		return LaunchResult{}, err
	}

	slog.InfoContext(ctx, "experiment submitted",
		"experiment", record.ID,
		"submission", record.SubmissionID,
		"repository", record.Repository,
		"commit", shortSHA(commitSHA),
		"mlflow_run", runID,
		"package_bytes", record.PackageBytes,
		"package_reused", record.PackageReused,
		// The credential's *source*, never its value.
		"credential", credential.Source)

	return LaunchResult{
		Experiment:  record,
		Credential:  credential.Credential,
		TrackingURI: s.TrackingUIURL(),
		Warnings:    warnings,
	}, nil
}

// requireCommittedState is the guard of §5.11 item 7.
//
// It is the reason this package refuses more launches than it accepts on a fresh
// checkout, and that is the intended behaviour: a run's commit SHA is either what
// ran or it is a claim nobody can check.
func requireCommittedState(status repo.Status) (string, error) {
	if !status.Cloned {
		return "", repo.ErrNotCloned
	}
	// A checkout whose origin is not the linked repository, refused before the
	// commit is read rather than after.
	//
	// It belongs in this guard and not beside it, because it is the same claim from
	// the other side. A run records `repository` and `commit_sha` as two tags that
	// are only meaningful together: the SHA resolves in the repository named beside
	// it or it resolves nowhere. `git archive` would happily build a package from
	// this checkout, the tags would look exactly like correct ones, and nothing
	// downstream — not MLflow, not the summary, not a developer six weeks later —
	// could tell that the pair names a commit in a repository nobody can fetch.
	//
	// Refused rather than corrected: which of the two repositories the developer
	// meant is a decision only they can take, which is why repo.ErrRemoteMismatch
	// exists as its own error rather than as a warning.
	if status.RemoteMismatch {
		return "", fmt.Errorf(
			"%w: the working copy at %s has origin %s while the selected repository is %s; "+
				"an experiment records the two as a pair, and a commit SHA tagged with the "+
				"wrong repository resolves nowhere",
			repo.ErrRemoteMismatch, status.Link.Path, remoteOrNone(status.Remote),
			status.Link.FullName)
	}
	if status.Unborn || status.Head == "" {
		return "", &DirtyError{Repository: status.Link.FullName, Unborn: true}
	}
	if !status.Dirty {
		return status.Head, nil
	}

	paths := make([]string, 0, len(status.Changes))
	for _, change := range status.Changes {
		paths = append(paths, change.Path)
	}
	sort.Strings(paths)
	const listed = 10
	elided := 0
	if len(paths) > listed {
		elided = len(paths) - listed
		paths = paths[:listed]
	}
	return "", &DirtyError{
		Repository: status.Link.FullName, Paths: paths, Elided: elided,
	}
}

// remoteOrNone names a checkout's origin, or says it has none. A checkout with no
// remote at all is a different repair from one pointing elsewhere, and "origin "
// followed by nothing would read as the first when it is the second.
func remoteOrNone(remote string) string {
	if strings.TrimSpace(remote) == "" {
		return "no origin at all"
	}
	return remote
}

// repositoryOf is the repository half of the operator id, and through it of the
// experiment name: GitHub's full name where the link carries one, the bare name
// otherwise.
func repositoryOf(link repo.Link) string {
	if link.FullName != "" {
		return link.FullName
	}
	return link.Name
}

// usernameOf is the Hub username this launch is namespaced under, from the request
// where the caller had one and from the caller's own token where it did not.
//
// The fallback is the fix for a split D17 did not survive. Two call sites reach
// Launch: the HTTP route, which reads the username off the validated token and
// passes it, and the `launch_experiment` tool, which cannot — a tools.Request
// carries a token, a subject, a session and a tier, and adding a username to it
// would mean threading one through the dispatcher, the chat session, the MCP
// transport and their stored state. So a chat launch arrived with an empty
// Username, fell back to the Keycloak subject, and produced a *second* MLflow
// experiment for the same developer on the same repository.
//
// That is not cosmetic. §5.13's comparison_to_previous searches within one MLflow
// experiment, so every chat-launched run reported itself as the first one — and an
// empty comparison reads as "nothing changed since the last run" to anything
// interpreting it. M9 builds an interpretation on that field, so a falsehood there
// is worse than an absence.
//
// Read from the bearer rather than passed, because the bearer is where the claim
// already is: `preferred_username` is a claim of the very token the caller
// presented, so there is one source for it instead of two call sites each
// remembering. Parsed unverified, exactly as pkg/auth and pkg/kernel parse it and
// for the same reason — the platform API gateway validates signature and expiry
// (§3.1 step 2) and this process only reads claims. A token that will not parse
// falls back to the subject, which is stable and unreadable rather than wrong.
func usernameOf(req Request) string {
	if trimmed := strings.TrimSpace(req.Username); trimmed != "" {
		return trimmed
	}
	if parsed, err := servicejwt.Parse(req.Bearer); err == nil {
		if name := strings.TrimSpace(parsed.Username); name != "" {
			return name
		}
	}
	return req.UserSub
}

// sanitiseSegment keeps a name segment to characters MLflow and a URL both accept
// without escaping.
func sanitiseSegment(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	cleaned := strings.Trim(builder.String(), "-")
	if cleaned == "" {
		return "unnamed"
	}
	return cleaned
}

// runTags are the tags every run carries, written at creation.
func runTags(record Experiment) []mlflowTag {
	tags := []mlflowTag{
		{Key: TagCommitSHA, Value: record.CommitSHA},
		{Key: TagUserSub, Value: record.UserSub},
		{Key: TagExperimentID, Value: record.ID},
		{Key: TagRepository, Value: record.Repository},
		{Key: TagSubmissionID, Value: record.SubmissionID},
		{Key: TagEntrypoint, Value: record.Entrypoint},
		{Key: TagSource, Value: "ode"},
	}
	if record.SessionID != "" {
		tags = append(tags, mlflowTag{Key: TagSessionID, Value: record.SessionID})
	}
	if record.Branch != "" {
		tags = append(tags, mlflowTag{Key: TagBranch, Value: record.Branch})
	}
	return tags
}

func runName(requested, commitSHA string) string {
	if trimmed := strings.TrimSpace(requested); trimmed != "" {
		return trimmed
	}
	return "ode-" + shortSHA(commitSHA)
}

// jobEnvironment validates the caller's variables and merges the deployment's.
//
// The caller's half arrives from an HTTP body or an LLM tool call, so it is
// validated at this boundary: bounded in count and size, names restricted to a
// pattern, and the names ODE sets itself refused rather than silently overwritten.
// Silently dropping an override would be worse than refusing it — a job told its
// MLFLOW_RUN_ID was honoured and finding it was not is a debugging session.
func (s *Service) jobEnvironment(requested map[string]string) (map[string]string, error) {
	if len(requested) > s.opts.MaxEnvVars {
		return nil, fmt.Errorf("%w: %d environment variables, at most %d are accepted",
			ErrInvalidRequest, len(requested), s.opts.MaxEnvVars)
	}

	environment := make(map[string]string, len(requested)+len(s.opts.Environment)+6)
	for key, value := range requested {
		if !envNamePattern.MatchString(key) {
			return nil, fmt.Errorf(
				"%w: %q is not a usable environment variable name", ErrInvalidRequest, key)
		}
		if reservedEnv[key] {
			return nil, fmt.Errorf(
				"%w: %s is set by ODE for every job and cannot be overridden",
				ErrInvalidRequest, key)
		}
		if len(value) > s.opts.MaxEnvValueBytes {
			return nil, fmt.Errorf("%w: the value of %s is %d bytes, at most %d are accepted",
				ErrInvalidRequest, key, len(value), s.opts.MaxEnvValueBytes)
		}
		environment[key] = value
	}
	// The deployment's own last, so it cannot be shadowed by a request: these are
	// the platform URLs a job reads timeseries from (§5.3.4).
	for key, value := range s.opts.Environment {
		environment[key] = value
	}
	return environment, nil
}

// Get reads one experiment, refreshing its status from Ray.
//
// The refresh is here rather than on a background poller because a status is only
// wanted when somebody is looking: a poller would hold a Ray connection per
// experiment forever, and nothing in ODE acts on a status change on its own.
func (s *Service) Get(ctx context.Context, req Request, id string) (Experiment, error) {
	record, found, err := s.store.Get(ctx, req.UserSub, id)
	if err != nil {
		return Experiment{}, err
	}
	if !found {
		return Experiment{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s.refresh(ctx, record)
}

// Record is Get under another name, for a caller that reads an experiment rather
// than watches one. Same ownership check, same refresh.
func (s *Service) Record(ctx context.Context, req Request, id string) (Experiment, error) {
	return s.Get(ctx, req, id)
}

// refresh reconciles a stored record with Ray's current view of the job.
func (s *Service) refresh(ctx context.Context, record Experiment) (Experiment, error) {
	if Terminal(record.Status) {
		// A finished job does not change, and Ray forgets old submissions — so
		// re-reading one would eventually turn a recorded success into a 404.
		return record, nil
	}

	details, err := s.ray.details(ctx, record.SubmissionID)
	if err != nil {
		var upstream *UpstreamError
		if errors.As(err, &upstream) && upstream.Code == http.StatusNotFound {
			return s.settleForgotten(ctx, record)
		}
		return record, err
	}

	record.Status = details.Status
	if details.Message != "" {
		record.Message = details.Message
	}
	record.StartedAt = rayTime(details.StartTime)
	record.EndedAt = rayTime(details.EndTime)
	record = touch(record)

	if err := s.store.Put(ctx, record); err != nil {
		return record, err
	}
	return record, nil
}

// settleForgotten resolves a submission the cluster no longer has.
//
// Ray keeps a finished job only as long as its own retention allows and forgets
// every submission when the head restarts, so this is a normal end state rather
// than a fault. M8 reported it as a message and returned the record untouched,
// which was wrong twice over. Untouched, the record stayed non-terminal, so
// anything reading the store for unfinished runs — M9's poller above all — would
// come back to it forever and get a 404 every time. Unpersisted, the message was
// recomputed on every read while the row in the database still said RUNNING.
//
// So it is settled, and settled from what ODE still has: MLflow. The job's own
// code writes the run's status, so a run that reached FINISHED or FAILED is the
// best evidence there is about how the job ended, and it outlives the cluster's
// memory. Where MLflow has nothing conclusive either — the run still open, no run
// at all, the tracking server unreachable — the answer is STOPPED, which is the
// only one of Ray's four terminal words that claims nothing about the outcome.
// Reporting SUCCEEDED or FAILED there would be a verdict with no basis, and the
// message says which of the two cases this is.
func (s *Service) settleForgotten(ctx context.Context, record Experiment) (Experiment, error) {
	const forgotten = "the Ray cluster no longer knows this submission; it may have been " +
		"restarted, or its retention window may have passed since the job ran"

	record.Status = StatusStopped
	record.Message = forgotten + ". Nothing ODE can still read says how the job ended, " +
		"so this is recorded as stopped rather than as a success or a failure"

	if record.RunID != "" {
		if run, err := s.mlflow.run(ctx, record.RunID); err == nil {
			switch strings.ToUpper(run.Info.Status) {
			case mlflowFinished:
				record.Status = StatusSucceeded
				record.Message = forgotten + ", but its MLflow run recorded FINISHED, " +
					"which is what this status comes from"
			case mlflowFailed, mlflowKilled:
				record.Status = StatusFailed
				record.Message = forgotten + ", but its MLflow run recorded " +
					strings.ToUpper(run.Info.Status) + ", which is what this status comes from"
			}
			if record.EndedAt == nil {
				record.EndedAt = mlflowTime(run.Info.EndTime)
			}
		} else {
			slog.WarnContext(ctx, "a forgotten submission's run could not be read from mlflow",
				"experiment", record.ID, "mlflow_run", record.RunID, "error", err)
		}
	}
	// EndedAt is deliberately left nil where MLflow had none. Stamping "now" would
	// put a duration into §5.13's resource_usage that nobody measured.

	record = touch(record)
	if err := s.store.Put(ctx, record); err != nil {
		// The reconciliation still stands for this answer; it will be redone on the
		// next read. Worth an error line because an unpersisted terminal status is
		// exactly what left the poller looping.
		slog.ErrorContext(ctx, "a forgotten submission could not be recorded as terminal",
			"experiment", record.ID, "status", record.Status, "error", err)
		return record, nil
	}
	return record, nil
}

// List is the caller's own experiments, newest first.
//
// Statuses are refreshed for the ones still running, and only those: a listing of
// fifty finished runs must not become fifty Ray calls.
func (s *Service) List(ctx context.Context, req Request, limit int) ([]Experiment, error) {
	// The caller's limit up to a ceiling, and zero takes the ceiling. Without the
	// cap a caller could ask for every row and turn one request into an unbounded
	// read followed by an unbounded number of cluster calls.
	if limit <= 0 || limit > defaultListLimit {
		limit = defaultListLimit
	}
	records, err := s.store.List(ctx, req.UserSub, limit)
	if err != nil {
		return nil, err
	}

	// The refresh is bounded in two directions, because it is the one place where a
	// developer's own listing depends on a third party answering.
	//
	// Concurrently, because it was serial: a developer with two dozen queued runs
	// against a cluster answering in 50ms waited over a second for a page of their
	// own history, and against a cluster that had stopped answering they waited the
	// request timeout *per run* — a listing that takes twelve minutes is a listing
	// that never arrives.
	//
	// And under one budget for the whole loop, not just per call. A record that could
	// not be refreshed in time is served as the store has it, which is the honest
	// answer: the stored record is still the truth about what was submitted, and
	// Ray's view of it is an enrichment. Failing the listing to get the enrichment
	// would be the wrong trade in the wrong direction.
	pending := make([]int, 0, len(records))
	for index, record := range records {
		if !Terminal(record.Status) {
			pending = append(pending, index)
		}
	}
	if len(pending) == 0 {
		return records, nil
	}

	refreshCtx, cancel := context.WithTimeout(ctx, s.opts.RequestTimeout)
	defer cancel()

	work := make(chan int)
	var mux sync.Mutex
	var wait sync.WaitGroup
	for range min(listRefreshParallelism, len(pending)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range work {
				mux.Lock()
				record := records[index]
				mux.Unlock()

				refreshed, err := s.refresh(refreshCtx, record)
				if err != nil {
					// One unreachable job must not empty the list.
					slog.WarnContext(ctx, "an experiment's status could not be refreshed",
						"experiment", record.ID, "error", err)
					continue
				}
				mux.Lock()
				records[index] = refreshed
				mux.Unlock()
			}
		}()
	}
	for _, index := range pending {
		select {
		case work <- index:
		case <-refreshCtx.Done():
			// The budget is spent. What is left keeps the status the store has, and the
			// listing is answered rather than failed.
		}
	}
	close(work)
	wait.Wait()

	return records, nil
}

// listRefreshParallelism bounds how many cluster calls one listing may have in
// flight. Small on purpose: this bounds ODE's load on a shared Ray dashboard, and
// the budget above is what bounds the developer's wait.
const listRefreshParallelism = 8

// Stop asks Ray to stop a job.
func (s *Service) Stop(ctx context.Context, req Request, id string) (Experiment, error) {
	record, found, err := s.store.Get(ctx, req.UserSub, id)
	if err != nil {
		return Experiment{}, err
	}
	if !found {
		return Experiment{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if Terminal(record.Status) {
		return record, nil
	}

	if _, err := s.ray.stop(ctx, record.SubmissionID); err != nil {
		return record, err
	}
	// Ray stops asynchronously, so the status is read back rather than assumed: a
	// record that said STOPPED while the job was still winding down would be a
	// second source of truth disagreeing with the dashboard beside it.
	return s.refresh(ctx, record)
}

// Results builds §5.13's summary for one experiment, on behalf of a developer.
//
// The criteria are read here because here is where a developer's token exists.
// Summarise is the same summary without one — see there for why the split is the
// shape of M9 rather than an optimisation.
func (s *Service) Results(ctx context.Context, req Request, id string) (Summary, error) {
	record, err := s.Get(ctx, req, id)
	if err != nil {
		return Summary{}, err
	}
	if record.RunID == "" {
		return Summary{}, fmt.Errorf(
			"%w: %s has no MLflow run, so there is nothing to summarise", ErrInvalidRequest, id)
	}

	run, err := s.mlflow.run(ctx, record.RunID)
	if err != nil {
		return Summary{}, err
	}

	previousRun := s.previousRun(ctx, record)

	criteria, problem := s.criteriaFor(ctx, req, record)
	return buildSummary(record, run, previousRun, criteria, problem), nil
}

// Summarise builds §5.13's summary with ODE's own service credential and no
// developer behind the request.
//
// This is the half of M9 that must not depend on anyone being connected. A run
// becomes terminal at three in the morning; the summary is built and stored then,
// so a developer who returns at nine finds a result rather than the beginning of
// one. §3.1 item 5 permits a service account for exactly Ray and MLflow, which is
// all this reads.
//
// What it does *not* do is read the developer's repository. `evaluation.yaml` is on
// their PVC and every read of it is on their behalf (§3.1 item 3), so the criterion
// in this summary is an explicit `no_developer_credential` non-result and the
// criteria are applied when a real token exists. That is a fact about the summary
// and not about the criterion, which is exactly why it is a reason rather than a
// `met: false`.
func (s *Service) Summarise(ctx context.Context, record Experiment) (Summary, error) {
	if record.RunID == "" {
		return Summary{}, fmt.Errorf(
			"%w: %s has no MLflow run, so there is nothing to summarise",
			ErrInvalidRequest, record.ID)
	}
	run, err := s.mlflow.run(ctx, record.RunID)
	if err != nil {
		return Summary{}, err
	}
	previousRun := s.previousRun(ctx, record)

	problem := notComputed(ReasonNoDeveloperCredential,
		"this summary was built when the run finished, with ODE's own Ray and MLflow "+
			"credential and nobody connected. %s is on the developer's workspace and is "+
			"read on their behalf, so the criteria are applied when they are next "+
			"connected",
		EvaluationCriteriaPath)
	return buildSummary(record, run, previousRun, CriteriaDocument{}, &problem), nil
}

// previousRun is what §5.13's comparison_to_previous compares against, or nil.
//
// A comparison is an enrichment rather than the answer, so every failure here is a
// warning and the summary is still built.
func (s *Service) previousRun(ctx context.Context, record Experiment) *mlflowRun {
	previous, found, err := s.store.Previous(ctx, record)
	if err != nil {
		slog.WarnContext(ctx, "the previous experiment could not be read",
			"experiment", record.ID, "error", err)
		return nil
	}
	if !found || previous.RunID == "" {
		return nil
	}
	fetched, err := s.mlflow.run(ctx, previous.RunID)
	if err != nil {
		slog.WarnContext(ctx, "the previous run could not be read from mlflow",
			"experiment", previous.ID, "error", err)
		return nil
	}
	return &fetched
}

// Logs reads a job's driver output for the developer's own pane.
//
// Never a tool result and never part of a summary (§5.13: "never raw logs"). It
// is a separate method with a separate route so that the boundary is visible in
// the code rather than maintained by discipline.
func (s *Service) Logs(ctx context.Context, req Request, id string) (LogPage, error) {
	record, found, err := s.store.Get(ctx, req.UserSub, id)
	if err != nil {
		return LogPage{}, err
	}
	if !found {
		return LogPage{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	logs, err := s.ray.logs(ctx, record.SubmissionID)
	if err != nil {
		return LogPage{}, err
	}
	page := LogPage{SubmissionID: record.SubmissionID, Logs: logs}
	if len(page.Logs) > s.opts.MaxLogBytes {
		// The tail rather than the head: a failure is at the end of a log, and a
		// developer reading one has come for the traceback.
		page.Logs = page.Logs[len(page.Logs)-s.opts.MaxLogBytes:]
		page.Truncated = true
	}
	return page, nil
}
