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

package configuration

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/SENERGY-Platform/go-service-base/config-hdl/types"
)

type ConfigStruct struct {
	ApiPort string `json:"api_port"`
	Debug   bool   `json:"debug"`

	// RequiredRealmRole gates every authenticated route (D5, §3.1).
	// Token signature, expiry and audience are validated by the platform API
	// gateway, not here.
	RequiredRealmRole string `json:"required_realm_role"`

	// Platform services. Read on behalf of the calling user (§3.1 step 3).
	DeviceRepoUrl       string `json:"device_repo_url"`
	TimescaleWrapperUrl string `json:"timescale_wrapper_url"`
	// PermissionsUrl is permissions-v2, which answers whether the developer may
	// execute a device, an import instance or a pipeline. It is what a launch
	// authorizes its input topics against; without it there is no experiment
	// surface, because a launch decides what data a run reads (SNRGY-4637).
	PermissionsUrl        string `json:"permissions_url"`
	OntologyCacheTtl      string `json:"ontology_cache_ttl"`
	OntologyInvalidateInt string `json:"ontology_invalidate_interval"`

	// Imports as operator inputs (docs/imports-as-operator-inputs.md). An operator
	// can take an import as an
	// input the way it takes a device, and an import type carries the same content
	// variables a device type does — so semantic selection finds both, or the
	// answer is quietly missing a class of candidate.
	//
	// DeviceSelectionUrl is the discovery half. It is deliberately not used for
	// devices: device-selection's device answer drops the connection state, the
	// device type name and the full device type, and hard-codes `shared` to false,
	// so ODE would have to read the devices again anyway. Its *import* answer is
	// the whole reason to have it — aspect subtree expansion, the type-to-instance
	// join and the message-relative paths are all things a direct caller would have
	// to reimplement, and get silently wrong.
	//
	// ImportDeployUrl answers whether an instance is actually running, which
	// discovery cannot: a stopped import is indistinguishable from a live one in a
	// selectables answer.
	//
	// ImportRepoUrl is needed only for a direct lookup of one import type by id;
	// discovery returns the type alongside every instance.
	//
	// AnalyticsServingUrl is how ODE tells a live-only import from an exported one.
	// timescale-wrapper has no importId, and there is no table worker for imports,
	// so an import's history exists if and only if an export was created for it.
	// Without this URL that question answers "unknown" — which is honest, and
	// different from "no history".
	DeviceSelectionUrl  string `json:"device_selection_url"`
	ImportDeployUrl     string `json:"import_deploy_url"`
	ImportRepoUrl       string `json:"import_repo_url"`
	AnalyticsServingUrl string `json:"analytics_serving_url"`

	// The four fields of an export that belong to a deployment rather than to the
	// import being exported (§5.8's create_export). None is derivable: the export
	// database is created per deployment by analytics-serving's own migration and
	// carries whatever id that migration was given, and the timestamp format is
	// whatever this platform's export worker parses. ODE guessing either produces an
	// export that is accepted, deploys, and stores nothing.
	//
	// ExportOffset is where the export worker starts reading — "smallest" replays
	// what the Kafka topic still retains, "largest" starts at the next message. The
	// model may choose between them per export; this is what it gets when it does
	// not.
	//
	// ExportTimePath is where the timestamp sits in an import message. This one has
	// a real default, because every import message carries `time` beside its `value`
	// payload.
	//
	// ExportTimestampFormat and ExportDatabaseID empty are not defaults: they mean
	// "copy it from an export this platform already has", and creating an export
	// refuses rather than inventing one when there is nothing to copy.
	ExportOffset          string `json:"export_offset"`
	ExportTimePath        string `json:"export_time_path"`
	ExportTimestampFormat string `json:"export_timestamp_format"`
	ExportDatabaseID      string `json:"export_database_id"`

	// MOSES, the platform's environment simulator (docs/simulation.md). Empty
	// moses_url leaves the fourteen simulation tools
	// declared-but-unavailable, exactly as an empty ray_url does for the two
	// experiment tools — nothing in ODE requires a simulator.
	//
	// There is no token here and there deliberately never will be. MOSES takes an
	// environment's owner from the caller's token, so a service account would
	// create simulations belonging to ODE: nobody could find them, and nobody could
	// delete them.
	//
	// MosesMaxDatasetBytes bounds one uploaded timeseries. It is a bound on ODE's
	// own memory rather than a policy about file sizes — the file travels out of
	// the developer's pod base64-encoded and then through ODE whole — and a file
	// over it is refused rather than uploaded truncated, because a cut-off CSV
	// parses and then plays silence from wherever it was cut.
	MosesUrl             string `json:"moses_url"`
	MosesRequestTimeout  string `json:"moses_request_timeout"`
	MosesMaxDatasetBytes int64  `json:"moses_max_dataset_bytes"`

	// ImportRequestTimeout bounds a single request to any of the four services
	// above. Held separately from TimeseriesRequestTimeout because these are
	// metadata reads: generous by timescale standards would be a hang here.
	ImportRequestTimeout string `json:"import_request_timeout"`

	// TimeseriesRequestTimeout bounds a single timescale-wrapper request. A
	// profiler pass over a long window is the slowest thing ODE asks of the
	// platform, so this is generous by design.
	TimeseriesRequestTimeout string `json:"timeseries_request_timeout"`

	// Profiler windows (D25). The raw pass reads the smaller of these two
	// bounds, anchored at the most recent data.
	//
	// int64 rather than int deliberately: HandleEnvironmentVars only knows how to
	// set Int64 among the integer kinds, so an int field would silently ignore
	// its environment variable.
	ProfilerRawWindowDays      int64 `json:"profiler_raw_window_days"`
	ProfilerRawWindowPoints    int64 `json:"profiler_raw_window_points"`
	ProfilerCoverageWindowDays int64 `json:"profiler_coverage_window_days"`
	ProfilerConcurrency        int64 `json:"profiler_concurrency"`

	// ProfilerLocalTimezone is used only to flag DST transition windows.
	// Computation stays in UTC throughout (§5.4.13).
	ProfilerLocalTimezone string `json:"profiler_local_timezone"`

	// Semantic selection (§5.2). SelectionMaxCriteria is the one worth
	// understanding: a resolution sends one device-type-selectables request per
	// combination of matched function and matched aspect, because the platform ANDs
	// a criteria list, and this caps how many.
	SelectionConcurrency int64 `json:"selection_concurrency"`
	SelectionMaxCriteria int64 `json:"selection_max_criteria"`
	SelectionDeviceLimit int64 `json:"selection_device_limit"`

	// PostgresUrl persists what §3.3 makes load-bearing: LLM spend accounting, the
	// exposure-tier audit trail, chat sessions, and the profiler's override overlay.
	// Empty runs the in-memory stores instead, which validate() warns about — a
	// spend cap computed in memory resets on every restart.
	//
	// types.Secret so the DSN, which carries a password, cannot reach a log or a
	// JSON body by accident: its String and MarshalJSON return a random
	// placeholder, and the real value is only available through Value().
	PostgresUrl      types.Secret `json:"postgres_url"`
	PostgresMaxConns int64        `json:"postgres_max_conns"`

	// LLM providers (§5.7, D7). Each is configured independently and any subset may
	// be present; a deployment with none serves M0–M2 and no chat.
	//
	// The API keys are the central key of D8, and are accounted per platform user
	// rather than issued per user.
	AnthropicApiKey  types.Secret `json:"anthropic_api_key"`
	AnthropicBaseUrl string       `json:"anthropic_base_url"`
	AnthropicModels  []string     `json:"anthropic_models"`

	OpenaiApiKey  types.Secret `json:"openai_api_key"`
	OpenaiBaseUrl string       `json:"openai_base_url"`
	OpenaiModels  []string     `json:"openai_models"`

	// The OpenAI-compatible row of §5.7: vLLM, Ollama, Azure. CompatibleTools says
	// whether that server implements function calling, because ODE cannot find out
	// without trying and a wrong assumption means tools that silently never fire.
	CompatibleName    string       `json:"compatible_name"`
	CompatibleBaseUrl string       `json:"compatible_base_url"`
	CompatibleApiKey  types.Secret `json:"compatible_api_key"`
	CompatibleModels  []string     `json:"compatible_models"`
	CompatibleTools   bool         `json:"compatible_tools"`

	// The local `claude` CLI, for development without an API key. Reaches ODE's
	// tools over MCP, so PublicUrl has to be set for its tools to work.
	ClaudeCliEnabled bool     `json:"claude_cli_enabled"`
	ClaudeCliBinary  string   `json:"claude_cli_binary"`
	ClaudeCliModels  []string `json:"claude_cli_models"`
	// ClaudeCliTimeout bounds one CLI turn: the whole agent loop the CLI runs for
	// itself, not one model call. Empty takes llm.DefaultCLITimeout. It is the
	// ceiling a held confirmation has to fit inside, so raising it gives a developer
	// more room to answer a card and lowering it takes room away — startup warns
	// when chat_confirmation_timeout no longer fits.
	ClaudeCliTimeout string `json:"claude_cli_timeout"`

	// PublicUrl is how a subprocess reaches this ODE. Needed only by the CLI
	// provider, which points the CLI's MCP client back at /mcp.
	PublicUrl string `json:"public_url"`

	// LlmMaxTokens bounds one response; LlmEffort maps to Anthropic's
	// output_config.effort; LlmAdaptiveThinking sends thinking: {type: "adaptive"}.
	LlmMaxTokens        int64  `json:"llm_max_tokens"`
	LlmEffort           string `json:"llm_effort"`
	LlmAdaptiveThinking bool   `json:"llm_adaptive_thinking"`
	// LlmMaxToolIterations bounds the tool loop, so a model that never concludes is
	// stopped by control flow rather than by the spend cap.
	LlmMaxToolIterations int64 `json:"llm_max_tool_iterations"`

	// Pricing is what a million tokens costs, per model, used for §3.3's estimated
	// cost. Not baked into the binary: published prices change, and a stale
	// hard-coded figure would make a spend cap quietly wrong.
	LlmCurrency string       `json:"llm_currency"`
	LlmPricing  []ModelPrice `json:"llm_pricing"`

	// ProfilerReadTimeout bounds one value-reading request to the platform,
	// separately from TimeseriesRequestTimeout.
	//
	// The two are not comparable. A raw pass bounded at a hundred thousand points is
	// megabytes of JSON the server has to assemble; an availability probe answers
	// from metadata. One shared timeout means either the probe waits far too long to
	// fail or the read is cut off mid-assembly.
	ProfilerReadTimeout string `json:"profiler_read_timeout"`

	// ChatExchangeTimeout bounds one detached exchange.
	//
	// An exchange no longer ends with the connection that started it, so it needs a
	// ceiling of its own: without one, a wedged provider or a hung platform read
	// would hold a session's turn slot and a goroutine indefinitely.
	ChatExchangeTimeout string `json:"chat_exchange_timeout"`

	// ChatConfirmationTimeout bounds how long a tool call held open on an
	// out-of-band transport waits for the developer's decision (D11).
	//
	// A separate ceiling from the one above, because it bounds something different:
	// not the turn, but one call inside it that has stopped to ask a question. It
	// has to stay well under the provider's own turn timeout — a turn that dies
	// underneath the card leaves the developer approving a call that has gone.
	ChatConfirmationTimeout string `json:"chat_confirmation_timeout"`

	// JupyterHub (§5.6, M4). Empty jupyterhub_url leaves the kernel routes
	// unserved, in the same way an absent timescale-wrapper leaves the profiler
	// unserved: a deployment without an execution backend still serves everything
	// below it.
	//
	// JupyterhubToken is ODE's service credential. It has to hold the scopes
	// kernel.RequiredScopes lists, and startup fails if it does not - a partial
	// grant is a deployment fault, and discovering it on a developer's first
	// spawn would be worse than not starting.
	JupyterhubUrl           string       `json:"jupyterhub_url"`
	JupyterhubToken         types.Secret `json:"jupyterhub_token"`
	JupyterhubUsernameClaim string       `json:"jupyterhub_username_claim"`
	JupyterhubKernel        string       `json:"jupyterhub_kernel"`

	// JupyterhubProfile is the KubeSpawner profile slug ODE spawns with. Empty
	// takes the deployment's default. §5.6 item 1 ships the ODE image as an
	// additional profile rather than replacing the default one, so a deployment
	// that has built it has to name it here or developers get the plain image.
	JupyterhubProfile string `json:"jupyterhub_profile"`

	// JupyterhubWorkspacePath is the kernel's working directory, relative to the
	// singleuser server's root. It must be inside the mounted PVC: only what is
	// written there survives the pod being culled and respawned, which is what
	// M4's "a file written in one session is present in the next" rests on.
	//
	// §5.11 suggests ~/ode/{repo}. The deployed chart mounts the PVC at
	// ~/data rather than over the whole home, so the default is under that.
	JupyterhubWorkspacePath string `json:"jupyterhub_workspace_path"`

	// JupyterhubSpawnTimeout bounds a cold start, which §5.6 puts at 10-60s.
	// JupyterhubExecuteTimeout bounds one cell; a cell that exceeds it is
	// interrupted rather than abandoned, so the kernel stays usable.
	JupyterhubSpawnTimeout   string `json:"jupyterhub_spawn_timeout"`
	JupyterhubRequestTimeout string `json:"jupyterhub_request_timeout"`
	JupyterhubExecuteTimeout string `json:"jupyterhub_execute_timeout"`

	// JupyterhubKeepaliveInterval must stay comfortably below the cluster's cull
	// timeout, or a developer thinking between cells loses their kernel state
	// (§5.6 item 3). JupyterhubIdleTimeout is the other side of the same control:
	// ODE stops keeping a pod alive once it has heard nothing for this long.
	JupyterhubKeepaliveInterval string `json:"jupyterhub_keepalive_interval"`
	JupyterhubIdleTimeout       string `json:"jupyterhub_idle_timeout"`
	// JupyterhubTokenTtl is how long the per-user token ODE mints for a pod lives.
	JupyterhubTokenTtl string `json:"jupyterhub_token_ttl"`
	// JupyterhubMaxOutputBytes bounds what one execution streams to the developer.
	JupyterhubMaxOutputBytes int64 `json:"jupyterhub_max_output_bytes"`

	// ToolRunCodeMaxOutputBytes is the far smaller bound on what run_code returns
	// to a model. Separate from the figure above because they answer to different
	// costs: a developer's console is bounded by memory, a tool result by context.
	ToolRunCodeMaxOutputBytes int64 `json:"tool_run_code_max_output_bytes"`

	// KernelContainCells withholds the platform token from an assistant cell that
	// did not ask for it, which is what lets such a cell run without a confirmation.
	// It moves what is confirmed from the code to the credential: measured over 261
	// real confirmations, recognising the code tops out near 29% while the token is
	// needed by 16%.
	//
	// Off by default, and the default is not timidity. The containment is the
	// absence of the credential and nothing else — the pod keeps whatever network
	// its NetworkPolicy leaves it, so a contained cell can still put data on the
	// wire. A deployment that has not restricted egress from the singleuser pod
	// should leave this off and keep the confirmation on every cell.
	KernelContainCells bool `json:"kernel_contain_cells"`

	// ToolRepoMaxReadBytes bounds one read_file answer, and it is the same kind of
	// bound as the one above rather than a limit on what may be read: pkg/repo's
	// MaxFileBytes is a megabyte because that is a sensible size for an editor, and
	// a model handed a megabyte of one file has spent the session on it. Over this,
	// the answer becomes a window that names the line to continue at.
	ToolRepoMaxReadBytes int64 `json:"tool_repo_max_read_bytes"`

	// Tool surface bounds (§5.8). ToolProfileTokenBudget caps the projection handed
	// to the model (D26); ToolPreviewMaxPoints caps a tier-L2 preview, which is what
	// keeps "downsampled preview" from becoming a raw series read (§4).
	//
	// The other two bound breadth rather than depth, which a per-item budget cannot:
	// ToolProfileMaxProfiles caps how many profiles one profile_series response
	// carries, and ToolQuickTokenBudget caps the ranked candidate list of the L0
	// tools. Without them a wide device set or a twenty-variable service answers
	// with a payload many times the size of the budget above it.
	// Chart bounds (§5.9, M5). ChartMaxPoints is the one that shapes what a
	// developer sees: the bucket is widened until the charted window fits it, so a
	// larger figure means a finer picture and a heavier read, never a truncated
	// window. ChartMaxAnnotations bounds the bands one chart carries, because a
	// session-based series over two years has thousands (D27) and what the cap drops
	// is reported rather than hidden.
	ChartMaxPoints       int64  `json:"chart_max_points"`
	ChartMaxAnnotations  int64  `json:"chart_max_annotations"`
	ChartMaxPerUser      int64  `json:"chart_max_per_user"`
	ChartDefaultLookback string `json:"chart_default_lookback"`

	// Relational bounds (§5.5, M6). RelationMaxMembers is the one that shapes the
	// answer: the pair count grows with the square of the members and the rule count
	// with four times that, so a generous figure buys breadth at the cost of a rule
	// list nobody reads to the end. RelationMaxBuckets bounds the aligned grid, which
	// is widened rather than the window truncated. RelationMaxRules bounds the
	// candidate list and what it drops is stated in the profile's notes.
	RelationMaxMembers         int64  `json:"relation_max_members"`
	RelationMaxGraphNeighbours int64  `json:"relation_max_graph_neighbours"`
	RelationMaxBuckets         int64  `json:"relation_max_buckets"`
	RelationMaxRules           int64  `json:"relation_max_rules"`
	RelationMaxStored          int64  `json:"relation_max_stored"`
	RelationDefaultLookback    string `json:"relation_default_lookback"`

	ToolProfileTokenBudget int64 `json:"tool_profile_token_budget"`
	ToolProfileMaxProfiles int64 `json:"tool_profile_max_profiles"`
	ToolQuickTokenBudget   int64 `json:"tool_quick_token_budget"`
	ToolPreviewMaxPoints   int64 `json:"tool_preview_max_points"`
	// The relational tool bounds (§5.5, M6). The same two-part shape as the profile
	// bounds above: a token budget per document, and a cap on how many candidate rules
	// one response carries, because a per-item budget cannot bound a list.
	ToolRelationTokenBudget int64 `json:"tool_relation_token_budget"`
	ToolRelationMaxRules    int64 `json:"tool_relation_max_rules"`

	// GitHub integration (§5.11, M7). Without GithubClientId the repo routes are not
	// served and `write_file` stays declared-but-unavailable — the same degradation
	// the profiler and the kernel do.
	//
	// GithubTokenKey is the key the developer's GitHub token is encrypted with, as
	// base64 of 32 bytes: `openssl rand -base64 32`. It is required rather than
	// optional, because §5.11 item 1 says the token is stored encrypted and a
	// deployment that stored it in the clear would be a different design, not a
	// convenience.
	//
	// GithubRedirectUri is the SPA's own callback URL, which has to match the OAuth
	// app's registered one exactly. Empty derives it from PublicUrl, which is right
	// only where ODE serves the SPA from the same origin.
	GithubClientId     string       `json:"github_client_id"`
	GithubClientSecret types.Secret `json:"github_client_secret"`
	GithubTokenKey     types.Secret `json:"github_token_key"`
	GithubApiUrl       string       `json:"github_api_url"`
	GithubWebUrl       string       `json:"github_web_url"`
	GithubScopes       []string     `json:"github_scopes"`
	GithubRedirectUri  string       `json:"github_redirect_uri"`

	// Repository bounds (§5.11, M7). RepoCommandTimeout bounds one git command, and
	// is minutes rather than seconds because a clone of a repository with history is
	// the slow one. RepoMaxFileBytes bounds a file the Code pane reads or writes; a
	// larger figure makes both the editor and the `write_file` tool able to move more
	// in one request, and neither wants a repository of model binaries.
	// RepoMaxWorkbenches caps how many working contexts one developer may hold open
	// at once. Each is a kernel process in their pod, so this figure and the memory
	// on the KubeSpawner profile belong together: raising it without raising the
	// profile's limit is how a developer's training run is OOM-killed by their own
	// second workbench.
	//
	// RepoLockTimeout bounds the `uv lock` a scaffold ends with, and is longer than
	// RepoCommandTimeout because it is different work: the Operator Lib pin is a git
	// source, so a first scaffold on a pod with a cold uv cache clones that
	// repository and builds its metadata before it can write a line.
	RepoCommandTimeout        string `json:"repo_command_timeout"`
	RepoLockTimeout           string `json:"repo_lock_timeout"`
	RepoMaxFileBytes          int64  `json:"repo_max_file_bytes"`
	RepoMaxTreeEntries        int64  `json:"repo_max_tree_entries"`
	RepoMaxCommandOutputBytes int64  `json:"repo_max_command_output_bytes"`
	RepoMaxWorkbenches        int64  `json:"repo_max_workbenches"`

	// The library the scaffold pins (D15). v1.5.0 is the floor: it is where
	// Config.ts_conn lost its compiled-in default and ts_wrapper_url appeared, and a
	// run reads history through the wrapper under the developer's own permission
	// (SNRGY-4637). An older pin ignores the field and falls back to the built-in
	// DSN, silently. v1.4.0 is separately required for train_once(), which fails
	// loudly instead.
	// OperatorLibRef empty resolves the newest
	// tag at scaffold time, which is what "track latest, pin per repo" means; setting
	// it fixes every new repository to one ref, which is what a deployment reproducing
	// an evaluation write-up wants.
	OperatorLibRepo string `json:"operator_lib_repo"`
	OperatorLibRef  string `json:"operator_lib_ref"`

	// Experiments (§5.12, M8). Without ray_url or mlflow_url the experiment routes
	// are not served and both experiment tools stay declared-but-unavailable — the
	// same degradation the profiler, the kernel and the repo surface do. The surface
	// also needs a Hub and a GitHub app, because the job package is the committed
	// state of a working copy that lives on the developer's pod.
	//
	// RayUrl and MlflowUrl are the *API* bases ODE calls. RayDashboardUrl and
	// MlflowUiUrl are what a browser should open, which is routinely a different
	// host: a cluster-internal service and an ingress-exposed UI. They are what the
	// panes link to.
	//
	// The two tokens are the service accounts §3.1 item 5 permits — the only place
	// in ODE where one is legitimate, because a Ray cluster and a tracking server
	// have no per-user identity to act as and D18 rules out building one. Both may
	// be empty: an in-cluster dashboard is commonly unauthenticated, and M10's
	// NetworkPolicy is what bounds who reaches it.
	RayUrl                      string       `json:"ray_url"`
	RayToken                    types.Secret `json:"ray_token"`
	RayDashboardUrl             string       `json:"ray_dashboard_url"`
	MlflowUrl                   string       `json:"mlflow_url"`
	MlflowToken                 types.Secret `json:"mlflow_token"`
	MlflowUiUrl                 string       `json:"mlflow_ui_url"`
	MlflowExperimentPrefix      string       `json:"mlflow_experiment_prefix"`
	ExperimentDefaultEntrypoint string       `json:"experiment_default_entrypoint"`

	// A run reads history through timescale_wrapper_url above, not through a DSN of
	// its own. There is deliberately no experiment_ts_conn: Operator Lib supports
	// both and prefers a DSN where it has one, and a run executes the developer's
	// own Python, so a DSN in its environment is a credential handed to code ODE did
	// not write — os.environ["CONFIG"] is all it takes to read it back out. The
	// wrapper needs no credential in the job and checks the developer's own Execute
	// permission on each device, which is the authority this path had before
	// experiments moved onto the operator path and lost when they did (SNRGY-4637).
	//
	// A deployed operator is the other way round: the flow engine sets it a DSN and
	// gives it no token, and its code is a reviewed image rather than a working copy.
	// ExperimentKafkaBootstrap is the broker list a training run's deployment config
	// carries. A run reads history from timescale for a device topic and replays kafka
	// for everything else (an import's topic, §5.3.4), which is the case that needs it.
	// Empty leaves the run able to train from timescale-backed topics only.
	ExperimentKafkaBootstrap string `json:"experiment_kafka_bootstrap"`
	// ExperimentRayClientUrl is what a run's deployment config names as ray_url.
	//
	// Not the same string as ray_url above: that one is the dashboard ODE submits
	// jobs to over HTTP, while this is what Operator Lib passes to ray.init(). A
	// deployed operator names the cluster's client endpoint (ray://host:10001)
	// because it connects from outside; a run's driver is already on the cluster,
	// so "auto" attaches to the cluster it is running in rather than opening a
	// client connection back into it.
	ExperimentRayClientUrl string `json:"experiment_ray_client_url"`
	// ExperimentPyExecutable is what Ray starts worker processes with. It has to
	// match what the entrypoint launches the driver with, or a Ray task starts on the
	// cluster image's interpreter and fails on the first import the lock file
	// provides. Empty omits the field and leaves Ray's own uv detection to it.
	ExperimentPyExecutable string `json:"experiment_py_executable"`

	// ExperimentMaxPackageBytes bounds the job archive. Exceeding it is reported
	// rather than truncated: a job that ran against a partial repository fails in a
	// way nobody could diagnose from the run. It also bounds ODE's own memory — the
	// archive is held whole, and it travels back from the pod base64-encoded, which
	// costs a third more again.
	ExperimentMaxPackageBytes int64 `json:"experiment_max_package_bytes"`
	// ExperimentMaxEnvVars and ExperimentMaxEnvValueBytes bound what one launch may
	// push into a cluster's job spec. They matter because a launch arrives from an
	// HTTP body or from an LLM tool call, and neither is trusted input.
	ExperimentMaxEnvVars       int64 `json:"experiment_max_env_vars"`
	ExperimentMaxEnvValueBytes int64 `json:"experiment_max_env_value_bytes"`
	// ExperimentMaxLogBytes bounds a log read. Logs go to the developer's own route
	// and never to a model (§5.13).
	ExperimentMaxLogBytes int64 `json:"experiment_max_log_bytes"`

	// ExperimentRequestTimeout bounds one Ray or MLflow API call;
	// ExperimentUploadTimeout bounds the one request that moves the whole archive,
	// separately, for the reason the profiler's two read timeouts are separate: a
	// bound that fits a status probe cannot fit a multi-megabyte upload.
	ExperimentRequestTimeout string `json:"experiment_request_timeout"`
	ExperimentUploadTimeout  string `json:"experiment_upload_timeout"`

	// The Keycloak token exchange of §3.1 item 6, and the risk register's "token
	// expiry vs. long Ray jobs" row.
	//
	// A Ray job reads training data directly from timescale-wrapper with its own
	// token (§5.3.4), and a training run outlives an interactive session — so where
	// these are configured ODE mints one token per submission through RFC 8693, on
	// behalf of the developer. Where they are not, the caller's session token is
	// passed, a warning names what is missing at startup, and the launch result says
	// the credential expires with the session. That is the degradation the rest of
	// ODE does, and the limitation is visible in the answer rather than discovered
	// from a Ray log at hour two.
	//
	// JobTokenAudience is not optional in practice: Keycloak returns a token for the
	// *requesting* client unless an audience names another, and a job reads
	// timescale-wrapper. JobTokenLifetime is an expectation rather than a request —
	// neither RFC 8693 nor Keycloak accepts a requested lifetime, so ODE compares it
	// against the issuer's own expires_in and warns on a shortfall.
	KeycloakUrl          string       `json:"keycloak_url"`
	KeycloakRealm        string       `json:"keycloak_realm"`
	KeycloakClientId     string       `json:"keycloak_client_id"`
	KeycloakClientSecret types.Secret `json:"keycloak_client_secret"`
	JobTokenAudience     string       `json:"job_token_audience"`
	JobTokenLifetime     string       `json:"job_token_lifetime"`

	// Result interpretation (§5.13, M9). ExperimentPollInterval is how often ODE
	// asks Ray about the runs it still calls unfinished; without the poller nothing
	// would notice a run ended unless a developer opened the pane.
	//
	// ExperimentPollWindow is how far back a finished run is still offered for
	// interpretation, and it is the figure that covers an ODE restart rather than a
	// tuning knob: a deployment that was down for an hour should still interpret the
	// runs that finished during it, and one that has been down for a week should
	// not replay the week.
	//
	// ExperimentPollBatch caps how many records one tick touches, and
	// ExperimentPollTimeout bounds the whole tick so a cluster that stopped
	// answering costs one tick rather than the loop.
	ExperimentPollInterval string `json:"experiment_poll_interval"`
	ExperimentPollWindow   string `json:"experiment_poll_window"`
	ExperimentPollBatch    int64  `json:"experiment_poll_batch"`
	ExperimentPollTimeout  string `json:"experiment_poll_timeout"`

	// InterpretationRetryInterval is how often the runs waiting for a developer are
	// tried again. Every reason a turn is refused is transient — a session already
	// running an exchange, a spend cap that resets, a developer who has not come
	// back — so this is the interval at which those resolve rather than a backoff.
	//
	// InterpretationTurnTimeout bounds how long one interpretation turn is waited
	// for before the run is left pending; the turn itself is not stopped, because
	// chat_exchange_timeout is the real ceiling and abandoning a turn the developer
	// can see would be worse than recording its proposal a minute later.
	//
	// InterpretationMaxPending bounds the queue of summaries held for developers who
	// are away.
	InterpretationRetryInterval string `json:"interpretation_retry_interval"`
	InterpretationTurnTimeout   string `json:"interpretation_turn_timeout"`
	InterpretationMaxPending    int64  `json:"interpretation_max_pending"`

	CorsOrigins []string `json:"cors_origins"`
}

// ModelPrice mirrors llm.ModelPrice. Duplicated rather than imported so that
// pkg/configuration keeps depending on nothing inside ODE — every other package
// depends on it, and an import the other way would be a cycle waiting to happen.
type ModelPrice struct {
	Model              string  `json:"model"`
	InputPerMTok       float64 `json:"input_per_mtok"`
	OutputPerMTok      float64 `json:"output_per_mtok"`
	CachedInputPerMTok float64 `json:"cached_input_per_mtok"`
}

type Config = *ConfigStruct

func Load(location string) (config Config, err error) {
	file, err := os.Open(location)
	if err != nil {
		// Wrapped and returned rather than logged here: Load runs before main has
		// installed the structured logger, and a package that both logs and returns
		// an error gets the failure reported twice.
		return config, fmt.Errorf("opening the configuration file %q: %w", location, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		return config, fmt.Errorf("decoding the configuration file %q: %w", location, err)
	}
	HandleEnvironmentVars(config)
	applyDefaults(config)
	return config, nil
}

// applyDefaults fills the operational values a deployment need not restate.
func applyDefaults(config Config) {
	if config.RequiredRealmRole == "" {
		config.RequiredRealmRole = "developer"
	}
	if config.OntologyCacheTtl == "" {
		config.OntologyCacheTtl = "1h"
	}
	if config.OntologyInvalidateInt == "" {
		config.OntologyInvalidateInt = "5m"
	}
	if config.ImportRequestTimeout == "" {
		config.ImportRequestTimeout = "30s"
	}
	if config.TimeseriesRequestTimeout == "" {
		config.TimeseriesRequestTimeout = "60s"
	}
	if config.ProfilerRawWindowDays <= 0 {
		config.ProfilerRawWindowDays = 14
	}
	if config.ProfilerRawWindowPoints <= 0 {
		config.ProfilerRawWindowPoints = 100000
	}
	if config.ProfilerCoverageWindowDays <= 0 {
		config.ProfilerCoverageWindowDays = 90
	}
	if config.ProfilerConcurrency <= 0 {
		config.ProfilerConcurrency = 4
	}
	if config.ProfilerLocalTimezone == "" {
		config.ProfilerLocalTimezone = "Europe/Berlin"
	}
	if config.SelectionConcurrency <= 0 {
		config.SelectionConcurrency = 4
	}
	if config.SelectionMaxCriteria <= 0 {
		config.SelectionMaxCriteria = 12
	}
	if config.SelectionDeviceLimit <= 0 {
		config.SelectionDeviceLimit = 10
	}
	if config.PostgresMaxConns <= 0 {
		config.PostgresMaxConns = 8
	}
	if config.LlmMaxTokens <= 0 {
		config.LlmMaxTokens = 8192
	}
	if config.LlmMaxToolIterations <= 0 {
		config.LlmMaxToolIterations = 12
	}
	if config.LlmCurrency == "" {
		config.LlmCurrency = "EUR"
	}
	if config.ToolProfileTokenBudget <= 0 {
		config.ToolProfileTokenBudget = 4000
	}
	if config.ToolProfileMaxProfiles <= 0 {
		config.ToolProfileMaxProfiles = 4
	}
	if config.ToolQuickTokenBudget <= 0 {
		config.ToolQuickTokenBudget = 4000
	}
	if config.ToolPreviewMaxPoints <= 0 {
		config.ToolPreviewMaxPoints = 500
	}
	if config.ProfilerReadTimeout == "" {
		config.ProfilerReadTimeout = "300s"
	}
	if config.ChatExchangeTimeout == "" {
		config.ChatExchangeTimeout = "30m"
	}
	if config.ChatConfirmationTimeout == "" {
		config.ChatConfirmationTimeout = "5m"
	}
	if config.CompatibleName == "" {
		config.CompatibleName = "openai-compatible"
	}
	if config.ClaudeCliBinary == "" {
		config.ClaudeCliBinary = "claude"
	}
	if config.JupyterhubUsernameClaim == "" {
		config.JupyterhubUsernameClaim = "preferred_username"
	}
	if config.JupyterhubKernel == "" {
		config.JupyterhubKernel = "python3"
	}
	if config.JupyterhubWorkspacePath == "" {
		config.JupyterhubWorkspacePath = "data/ode"
	}
	if config.JupyterhubSpawnTimeout == "" {
		config.JupyterhubSpawnTimeout = "180s"
	}
	if config.JupyterhubRequestTimeout == "" {
		config.JupyterhubRequestTimeout = "30s"
	}
	if config.JupyterhubExecuteTimeout == "" {
		config.JupyterhubExecuteTimeout = "10m"
	}
	if config.JupyterhubKeepaliveInterval == "" {
		config.JupyterhubKeepaliveInterval = "5m"
	}
	if config.JupyterhubIdleTimeout == "" {
		config.JupyterhubIdleTimeout = "2h"
	}
	if config.JupyterhubTokenTtl == "" {
		config.JupyterhubTokenTtl = "12h"
	}
	if config.JupyterhubMaxOutputBytes <= 0 {
		config.JupyterhubMaxOutputBytes = 1048576
	}
	if config.ToolRunCodeMaxOutputBytes <= 0 {
		config.ToolRunCodeMaxOutputBytes = 8000
	}
	if config.ToolRepoMaxReadBytes <= 0 {
		config.ToolRepoMaxReadBytes = 8000
	}
	if config.ChartMaxPoints <= 0 {
		config.ChartMaxPoints = 2000
	}
	if config.ChartMaxAnnotations <= 0 {
		config.ChartMaxAnnotations = 200
	}
	if config.ChartMaxPerUser <= 0 {
		config.ChartMaxPerUser = 100
	}
	if config.ChartDefaultLookback == "" {
		config.ChartDefaultLookback = "168h"
	}
	if config.ToolRelationTokenBudget <= 0 {
		config.ToolRelationTokenBudget = 4000
	}
	if config.ToolRelationMaxRules <= 0 {
		config.ToolRelationMaxRules = 12
	}
	if config.RelationMaxMembers <= 0 {
		config.RelationMaxMembers = 6
	}
	if config.RelationMaxGraphNeighbours <= 0 {
		config.RelationMaxGraphNeighbours = 12
	}
	if config.RelationMaxBuckets <= 0 {
		config.RelationMaxBuckets = 20000
	}
	if config.RelationMaxRules <= 0 {
		config.RelationMaxRules = 100
	}
	if config.RelationMaxStored <= 0 {
		config.RelationMaxStored = 200
	}
	if config.GithubApiUrl == "" {
		config.GithubApiUrl = "https://api.github.com"
	}
	if config.GithubWebUrl == "" {
		config.GithubWebUrl = "https://github.com"
	}
	if len(config.GithubScopes) == 0 {
		// The two §5.11 item 1 names. `workflow` is not optional in practice: the
		// scaffold writes .github/workflows/build.yml, and GitHub rejects a push that
		// touches a workflow file from a token without it.
		config.GithubScopes = []string{"repo", "workflow"}
	}
	if config.RepoCommandTimeout == "" {
		config.RepoCommandTimeout = "300s"
	}
	if config.RepoLockTimeout == "" {
		config.RepoLockTimeout = "600s"
	}
	if config.RepoMaxFileBytes <= 0 {
		config.RepoMaxFileBytes = 1048576
	}
	if config.RepoMaxTreeEntries <= 0 {
		config.RepoMaxTreeEntries = 4000
	}
	if config.RepoMaxCommandOutputBytes <= 0 {
		config.RepoMaxCommandOutputBytes = 1048576
	}
	if config.RepoMaxWorkbenches <= 0 {
		config.RepoMaxWorkbenches = 3
	}
	if config.OperatorLibRepo == "" {
		config.OperatorLibRepo = "SENERGY-Platform/analytics-operator-lib-python"
	}
	if config.MlflowExperimentPrefix == "" {
		config.MlflowExperimentPrefix = "ode"
	}
	if config.ExperimentDefaultEntrypoint == "" {
		// train.py is the scaffold's training-only entrypoint: Operator Lib's own init
		// sequence, stopped before the kafka loop that main.py would enter and never
		// leave. It is a committed file rather than something ODE injects, so the
		// package stays exactly the commit it claims to be (§5.11 item 7).
		//
		// `uv run` rather than `python` is what makes the cluster image irrelevant to
		// the operator's dependencies: uv builds the environment from the repository's
		// own pyproject.toml and uv.lock, both of which travel in the package, and
		// caches it per node so the second run on a node pays almost nothing. The
		// alternative was baking Operator Lib into the Ray image, which cannot work —
		// an operator's own dependencies are its own, and torch is not predictable
		// from a shared image.
		config.ExperimentDefaultEntrypoint = "uv run python train.py"
	}
	if config.ExperimentRayClientUrl == "" {
		config.ExperimentRayClientUrl = "auto"
	}
	if config.ExperimentPyExecutable == "" {
		config.ExperimentPyExecutable = "uv run"
	}
	if config.ExperimentPollInterval == "" {
		config.ExperimentPollInterval = "30s"
	}
	if config.ExperimentPollWindow == "" {
		config.ExperimentPollWindow = "6h"
	}
	if config.ExperimentPollBatch <= 0 {
		config.ExperimentPollBatch = 200
	}
	if config.ExperimentPollTimeout == "" {
		config.ExperimentPollTimeout = "120s"
	}
	if config.InterpretationRetryInterval == "" {
		config.InterpretationRetryInterval = "30s"
	}
	if config.InterpretationTurnTimeout == "" {
		config.InterpretationTurnTimeout = "10m"
	}
	if config.InterpretationMaxPending <= 0 {
		config.InterpretationMaxPending = 200
	}
	if config.ExperimentMaxPackageBytes <= 0 {
		config.ExperimentMaxPackageBytes = 16777216
	}
	if config.ExperimentMaxEnvVars <= 0 {
		config.ExperimentMaxEnvVars = 32
	}
	if config.ExperimentMaxEnvValueBytes <= 0 {
		config.ExperimentMaxEnvValueBytes = 4096
	}
	if config.ExperimentMaxLogBytes <= 0 {
		config.ExperimentMaxLogBytes = 1048576
	}
	if config.ExperimentRequestTimeout == "" {
		config.ExperimentRequestTimeout = "30s"
	}
	if config.ExperimentUploadTimeout == "" {
		config.ExperimentUploadTimeout = "300s"
	}
	if config.KeycloakRealm == "" {
		config.KeycloakRealm = "master"
	}
	if config.JobTokenLifetime == "" {
		// Matches jupyterhub_token_ttl, because the two bound the same thing from
		// different directions: how long a developer's work can run unattended.
		config.JobTokenLifetime = "12h"
	}
	if config.RelationDefaultLookback == "" {
		// A month, not a week. An exception "at certain times of day" needs several
		// samples in each hour bucket of each weekday, and a week does not hold them.
		config.RelationDefaultLookback = "720h"
	}
}

var camel = regexp.MustCompile("(^[^A-Z]*|[A-Z]*)([A-Z][^A-Z]+|$)")

func fieldNameToEnvName(s string) string {
	var a []string
	for _, sub := range camel.FindAllStringSubmatch(s, -1) {
		if sub[1] != "" {
			a = append(a, sub[1])
		}
		if sub[2] != "" {
			a = append(a, sub[2])
		}
	}
	return strings.ToUpper(strings.Join(a, "_"))
}

// HandleEnvironmentVars overrides config fields from the environment, for docker.
func HandleEnvironmentVars(config Config) {
	configValue := reflect.Indirect(reflect.ValueOf(config))
	configType := configValue.Type()
	for index := 0; index < configType.NumField(); index++ {
		fieldName := configType.Field(index).Name
		envName := fieldNameToEnvName(fieldName)
		envValue := os.Getenv(envName)
		if envValue == "" {
			continue
		}
		isSecret := configType.Field(index).Type == reflect.TypeOf(types.Secret(""))
		// The value is withheld for a secret field. The type already masks it on the
		// way out, but there is no reason to hand it to the log in the first place.
		if isSecret {
			slog.Info("configuration overridden from the environment", "variable", envName)
		} else {
			slog.Info("configuration overridden from the environment", "variable", envName,
				"value", envValue)
		}
		field := configValue.FieldByName(fieldName)
		switch field.Kind() {
		case reflect.Int64:
			i, _ := strconv.ParseInt(envValue, 10, 64)
			field.SetInt(i)
		case reflect.Uint16:
			i, _ := strconv.ParseUint(envValue, 10, 16)
			field.SetUint(i)
		case reflect.Float64:
			f, _ := strconv.ParseFloat(envValue, 64)
			field.SetFloat(f)
		case reflect.String:
			field.SetString(envValue)
		case reflect.Bool:
			b, _ := strconv.ParseBool(envValue)
			field.SetBool(b)
		case reflect.Slice:
			val := []string{}
			for _, element := range strings.Split(envValue, ",") {
				val = append(val, strings.TrimSpace(element))
			}
			field.Set(reflect.ValueOf(val))
		case reflect.Map:
			value := map[string]string{}
			for _, element := range strings.Split(envValue, ",") {
				keyVal := strings.SplitN(element, ":", 2)
				if len(keyVal) != 2 {
					continue
				}
				value[strings.TrimSpace(keyVal[0])] = strings.TrimSpace(keyVal[1])
			}
			field.Set(reflect.ValueOf(value))
		}
	}
}
