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

package tools

import (
	"context"
	"encoding/json"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/plaincode"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// The dependencies the tool surface reads through, each narrowed to what the
// tools actually use — the same reason pkg/selection declares its own
// interfaces: a test answers with three functions rather than a platform.
type (
	Ontology interface {
		Snapshot(ctx context.Context, token string) (*ontology.Snapshot, error)
	}

	Devices interface {
		List(token string, options drmodel.ExtendedDeviceListOptions) (devices.ListResult, error)
		Get(token string, id string, action drmodel.AuthAction) (models.ExtendedDevice, error)
	}

	Timeseries interface {
		DataAvailability(ctx context.Context, token string, deviceID string) ([]timeseries.Availability, error)
		DeviceUsage(ctx context.Context, token string, deviceIDs []string) ([]timeseries.Usage, error)
		// ExportUsage is the export half of the cost estimate. There is no
		// availability endpoint to go with it — that one is device-scoped — which is
		// why probe_export_data exists beside probe_availability rather than as a
		// parameter on it.
		ExportUsage(ctx context.Context, token string, exportIDs []string) ([]timeseries.Usage, error)
		Query(ctx context.Context, token string, elements []timeseries.QueryElement, opts timeseries.QueryOptions) ([]timeseries.QueryResult, error)
	}

	Profiler interface {
		QuickProfiles(ctx context.Context, token string, req profiler.QuickRequest) (profiler.QuickResult, error)
		ProfileService(ctx context.Context, token string, req profiler.ProfileRequest) (profiler.ProfileResult, error)
		// The export half. ExportFill is the L0 question — is anything in there —
		// and ProfileExport is the same profile pass a device service gets, over an
		// export's columns.
		ExportFill(ctx context.Context, token string, req profiler.ExportFillRequest) (profiler.ExportFill, error)
		ProfileExport(ctx context.Context, token string, req profiler.ExportProfileRequest) (profiler.ProfileResult, error)
		Profile(profileID string) (profiler.ResolvedProfile, bool)
		Store() profiler.Store
	}

	Selection interface {
		Resolve(ctx context.Context, token string, req selection.Request) (selection.Result, error)
	}

	// Imports is the second kind of operator input (PLAN). Discovery is not here:
	// an import is found through resolve_semantic_selection like a device, because
	// a developer asking for a signal has no business knowing which of the two the
	// platform happens to deliver it from. What is here is what discovery cannot
	// answer — the direct lookup by id, and whether an instance is running.
	Imports interface {
		List(ctx context.Context, token string, opts imports.InstanceListOptions) (imports.ListResult, error)
		Get(ctx context.Context, token string, id string) (idmodel.Instance, error)
		GetType(ctx context.Context, token string, id string) (dsmodel.ImportType, error)
		// ListTypes is the type catalogue: what could be deployed, as opposed to what
		// is. Discovery cannot stand in for it — it reports one row per instance, so a
		// type nobody has deployed is absent from it entirely.
		ListTypes(ctx context.Context, token string, opts imports.TypeListOptions) (imports.TypeListResult, error)
		History(ctx context.Context, token string, instanceID string) imports.History
		// Histories is the batch form, for a listing. analytics-serving cannot filter
		// by import, so asking per instance would re-read the whole export listing once
		// per row.
		Histories(ctx context.Context, token string, instanceIDs []string) map[string]imports.History

		// The write half. These four are the only methods on this interface that
		// change the platform, and every tool reaching them is a confirmed one.
		CreateInstance(ctx context.Context, token string, req imports.CreateInstanceRequest) (imports.CreatedInstance, error)
		DeleteInstance(ctx context.Context, token string, id string) error
		CreateExport(ctx context.Context, token string, req imports.CreateExportRequest) (imports.CreatedExport, error)
		DeleteExport(ctx context.Context, token string, id string) error
		ExportDefaults() imports.ExportDefaults
	}

	// Creations records what a session created on the platform, and is what makes
	// the two delete tools safe enough to exist (§5.8).
	//
	// §5.8 denies delete_platform_data outright, and deleting an export drops its
	// timescale table while deleting an import deletes its Kafka topic — so a
	// general delete tool would be that denied capability under another name. What
	// is permitted instead is strictly narrower: a session may remove what that same
	// session created, minutes earlier, with the developer confirming again. Every
	// other id is refused, and the refusal is not a policy check the model can argue
	// with — the id is simply not in the session's list.
	//
	// Implemented by the chat store, for the reason SelectionSink is: this is
	// session state, and it has to survive the process that wrote it.
	Creations interface {
		RecordCreation(ctx context.Context, sessionID string, created Creation) error
		Creations(ctx context.Context, sessionID string) ([]Creation, error)
	}

	// SelectionSink is where propose_data_selection writes. Implemented by the
	// chat store, because a proposed selection is session state (§5.10:
	// "confirmations persist as session overrides").
	SelectionSink interface {
		PutProposedSelection(ctx context.Context, sessionID string, proposal ProposedSelection) error
	}

	// Charts is the exploration pane's backend (§5.9). render_chart writes a
	// specification here and reads back how it resolved; it never reads values,
	// which is why a tool that plainly produces a picture of data sits at L1.
	Charts interface {
		Create(ctx context.Context, token string, req charts.CreateRequest) (charts.Created, error)
	}

	// Relations is the multi-device conditional pattern surface (§5.5). Narrowed to
	// the two operations §5.8 gives a tool: proposing sets from an aspect, which
	// reads only the ontology, and computing a relation, which reads values. Deciding
	// a rule is absent on purpose — it is a developer action and there is no tool for
	// it, the same boundary that keeps ProfileOverride out of this interface.
	Relations interface {
		ProposeRelatedSets(ctx context.Context, token string, req relations.ProposalRequest) (relations.Proposal, error)
		Relate(ctx context.Context, token string, req relations.Request) (relations.RelationProfile, error)
		MaxMembers() int
	}

	// Kernel runs code in the developer's own pod (§5.6). Narrowed to the two
	// methods run_code needs: the token identifies the developer, so nothing here
	// takes a user.
	Kernel interface {
		Run(ctx context.Context, ref kernel.Ref, code string) (<-chan kernel.ExecutionEvent, error)
		Workspace() string
	}

	// Repo is the working copy (§5.11). Exactly one method, which is the point:
	// §5.8 gives the model a tool that writes a file and no tool that commits,
	// stages, pushes, selects a repository or discards a change. A wider interface
	// here would be the first step towards one.
	Repo interface {
		WriteFile(ctx context.Context, req repo.Request, path string, content []byte) (repo.WriteResult, error)
	}

	// Experiments is the Ray and MLflow surface (§5.12). Three methods, and the
	// omission is the interesting part: there is no Logs here, because §5.13 says a
	// model's context never carries raw logs and the cheapest way to guarantee that
	// is for the tool surface to have no way of asking. Stopping a job is absent for
	// the reason a commit is absent from Repo — it is a developer's decision about
	// their own cluster time.
	Experiments interface {
		Launch(ctx context.Context, req experiments.LaunchRequest) (experiments.LaunchResult, error)
		Results(ctx context.Context, req experiments.Request, id string) (experiments.Summary, error)
		List(ctx context.Context, req experiments.Request, limit int) ([]experiments.Experiment, error)
	}
)

// Deps carries the services the executors need. Every field is optional: a
// deployment without a timescale-wrapper has no profiler, and the tools that
// need one are then declared without an executor rather than registered and
// broken. That is the same degradation the M1 routes already do.
// CreationKind is what a session created. Two kinds, because two things can be
// created and each is deleted through a different service.
type CreationKind string

const (
	CreatedImportInstance CreationKind = "import_instance"
	CreatedExport         CreationKind = "export"
)

// Creation is one platform object a session created.
//
// Name and Tool are recorded beside the id so that a refusal can say what the
// session did create — "you created the export weather-history, not the one you
// just named" is an answer a model can act on, where a bare refusal invites it to
// try the id again.
type Creation struct {
	Kind CreationKind `json:"kind"`
	ID   string       `json:"id"`
	Name string       `json:"name"`
	Tool string       `json:"tool"`
	At   time.Time    `json:"at"`
}

type Deps struct {
	Ontology Ontology
	Devices  Devices
	Imports  Imports
	// Creations is where the two create tools record what they made and the two
	// delete tools look before removing anything. Absent, the deletes are not
	// advertised at all — there is no memory for them to check.
	Creations     Creations
	Timeseries    Timeseries
	Profiler      Profiler
	Selection     Selection
	SelectionSink SelectionSink
	Kernel        Kernel
	Charts        Charts
	Relations     Relations
	Repo          Repo
	Experiments   Experiments

	// ProfileTokenBudget bounds the projection handed to the model (D26). The
	// stored profile is unbounded; what an LLM reads never is.
	ProfileTokenBudget int
	// ProfileMaxProfiles bounds how many of those projections one response
	// carries. ProfileTokenBudget is per profile, and profile_series profiles
	// every variable of a service, so without this a twenty-variable service
	// answers with twenty times the budget.
	ProfileMaxProfiles int
	// QuickTokenBudget bounds the projected candidate list of the L0 tools. Same
	// rule as ProfileTokenBudget, applied to breadth rather than depth: ranking a
	// shortlist must not cost more context than reading the profiles would.
	QuickTokenBudget int
	// RelationTokenBudget bounds what one relate_series response costs in context.
	// Same rule as ProfileTokenBudget: the stored relation profile is unbounded — a
	// pair count grows with the square of the members and each pair carries a table
	// per conditioning bucket — and what a model reads never is.
	RelationTokenBudget int
	// RelationMaxRules caps how many candidate rules one response carries, strongest
	// first. Breadth rather than depth, and the same reason ProfileMaxProfiles exists:
	// a per-item budget cannot bound a list.
	RelationMaxRules int
	// PreviewMaxPoints caps a tier-L2 preview. A "downsampled preview" that
	// returned fifty thousand points would be a raw series read with a friendlier
	// name, and would put the core design rule of §4 in question.
	PreviewMaxPoints int
	// DeviceLimit bounds how many devices a tool expands, matching the ceiling the
	// HTTP surface already applies.
	DeviceLimit int64
	// RunCodeMaxOutputBytes bounds what one execution returns to the model. Far
	// smaller than the cap on the developer's own console: the two answer to
	// different costs, memory there and context here.
	RunCodeMaxOutputBytes int
}

const (
	defaultProfileTokenBudget = 4000
	defaultProfileMaxProfiles = 4
	defaultQuickTokenBudget   = 4000
	defaultPreviewMaxPoints   = 500
	defaultDeviceLimit        = 10
	defaultRunCodeMaxOutput   = 8000
	defaultRelationBudget     = 4000
	defaultRelationMaxRules   = 12
)

// NewSurface builds the registry of §5.8 against the services that are present.
//
// Every tool of §5.8 is declared, and so are the nine beyond it (see the package
// comment). A tool whose dependency is absent,
// or whose backend belongs to a later milestone, is declared without an executor
// and names the milestone: it appears in the published table, is never advertised
// to a model, and answers a direct call with a structured refusal rather than a
// panic.
func NewSurface(deps Deps) (*Registry, error) {
	if deps.ProfileTokenBudget <= 0 {
		deps.ProfileTokenBudget = defaultProfileTokenBudget
	}
	if deps.ProfileMaxProfiles <= 0 {
		deps.ProfileMaxProfiles = defaultProfileMaxProfiles
	}
	if deps.QuickTokenBudget <= 0 {
		deps.QuickTokenBudget = defaultQuickTokenBudget
	}
	if deps.PreviewMaxPoints <= 0 {
		deps.PreviewMaxPoints = defaultPreviewMaxPoints
	}
	if deps.DeviceLimit <= 0 {
		deps.DeviceLimit = defaultDeviceLimit
	}
	if deps.RunCodeMaxOutputBytes <= 0 {
		deps.RunCodeMaxOutputBytes = defaultRunCodeMaxOutput
	}
	if deps.RelationTokenBudget <= 0 {
		deps.RelationTokenBudget = defaultRelationBudget
	}
	if deps.RelationMaxRules <= 0 {
		deps.RelationMaxRules = defaultRelationMaxRules
	}

	s := &surface{deps: deps}

	return NewRegistry(
		// ---- L0: the ontology and metadata half. No values, by construction. ----
		Definition{
			Name: "search_ontology",
			Description: "Search the platform ontology for measuring functions, aspects (the " +
				"hierarchical subsystem tree) and device classes matching a phrase. Returns the " +
				"evidence behind each match. Start here: the ontology is how data is addressed on " +
				"this platform, and names of devices are not.",
			Effect:  "read aspects/functions/characteristics",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "query": {"type": "string", "description": "Words to match, e.g. \"power generation PV\"."},
			    "include_controlling": {"type": "boolean", "description": "Also search controlling functions. Default false: a series is something measured."},
			    "limit": {"type": "integer", "description": "Matches to keep per entity type."}
			  },
			  "required": ["query"]
			}`),
			Unavailable: "requires device_repo_url",
			executor:    ifPresent(s.searchOntology, deps.Ontology),
		},
		Definition{
			Name: "resolve_semantic_selection",
			Description: "Resolve a natural-language data intent to concrete addressable series " +
				"through the ontology, and rank the candidates by QuickProfile. Reads no values. " +
				"This is the primary way to find data: it reports what matched, what the platform " +
				"had, and where the ontology is incomplete.",
			Effect:  "read, semantic query",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "intent": {"type": "string", "description": "What the data is needed for, e.g. \"forecast PV generation for this site\"."},
			    "function_ids": {"type": "array", "items": {"type": "string"}, "description": "Pin specific functions instead of matching lexically."},
			    "aspect_ids": {"type": "array", "items": {"type": "string"}, "description": "Pin specific aspects. An aspect already covers its whole subtree."},
			    "device_class_ids": {"type": "array", "items": {"type": "string"}, "description": "Narrow by device class. Deliberate only: this ANDs with the rest."},
			    "include_controlling": {"type": "boolean"},
			    "device_limit": {"type": "integer", "description": "How many devices to expand. Each costs one availability call."},
			    "skip_ranking": {"type": "boolean", "description": "Ontology resolution only, with no per-device availability calls. Much cheaper."}
			  },
			  "required": ["intent"]
			}`),
			Unavailable: "requires device_repo_url",
			executor:    ifPresent(s.resolveSemanticSelection, deps.Selection),
		},
		Definition{
			Name: "list_devices",
			Description: "List the devices this developer may read, optionally filtered by a search " +
				"term or device type. Metadata only: names, types, connection state, permissions.",
			Effect:  "read",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "search": {"type": "string"},
			    "device_type_ids": {"type": "array", "items": {"type": "string"}},
			    "limit": {"type": "integer"}
			  }
			}`),
			Unavailable: "requires device_repo_url",
			executor:    ifPresent(s.listDevices, deps.Devices),
		},
		Definition{
			Name: "get_device_metadata",
			Description: "Read one device: its type, services, addressable variable paths, " +
				"characteristics and connection state. No values.",
			Effect:  "read",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {"device_id": {"type": "string"}},
			  "required": ["device_id"]
			}`),
			Unavailable: "requires device_repo_url",
			executor:    ifPresent(s.getDeviceMetadata, deps.Devices),
		},
		Definition{
			Name: "list_import_instances",
			Description: "List the import instances this developer may read. An import is the " +
				"platform's other kind of operator input: an adapter that pulls data from outside — " +
				"a weather service, a price feed, a public sensor network — and publishes it to one " +
				"Kafka topic. Metadata only: names, import type, the Kafka topic, whether the " +
				"container is running, and whether any of its past is stored.\n\n" +
				"Prefer resolve_semantic_selection: it finds imports and devices together, by " +
				"meaning. Use this when the developer names an import, or to see what exists at all.",
			Effect:  "read",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "search": {"type": "string", "description": "Matches the instance name only, upstream."},
			    "import_type_ids": {"type": "array", "items": {"type": "string"}, "description": "Keep only instances of these import types. Filtered here, not upstream, so it costs a full listing."},
			    "limit": {"type": "integer"},
			    "include_history": {"type": "boolean", "description": "Also report per instance whether its data is stored in timescale. One extra call each; off by default."}
			  }
			}`),
			Unavailable: "requires device_selection_url and import_deploy_url",
			executor:    ifPresent(s.listImportInstances, deps.Imports),
		},
		Definition{
			Name: "get_import_type_metadata",
			Description: "Read one import type: its configs and its output tree, with the " +
				"addressable variable paths, characteristics and semantics of each variable.\n\n" +
				"Read the paths carefully. An import type's output describes the *whole* Kafka " +
				"message, so it carries an `import_id` and a `time` variable that are not signals, " +
				"and every real variable sits under a payload node. The variable_path this returns " +
				"is already the addressable form.",
			Effect:  "read",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "import_type_id": {"type": "string"},
			    "instance_id": {"type": "string", "description": "Alternative to import_type_id: read the type of this instance."}
			  }
			}`),
			Unavailable: "requires device_selection_url, import_deploy_url and import_repo_url",
			executor:    ifPresent(s.getImportTypeMetadata, deps.Imports),
		},
		Definition{
			Name: "list_import_types",
			Description: "Search the import type catalogue: the adapters this platform could " +
				"deploy an import from. A type is a blueprint — it is not running, carries no " +
				"data and has no Kafka topic; an instance created from it does.\n\n" +
				"This is the only way to reach a type that has no instance yet, which is exactly " +
				"the type create_import_instance is for. resolve_semantic_selection reports " +
				"imports that exist, so a type nobody has deployed never appears there — it " +
				"names the matching ones in deployable_import_types instead.\n\n" +
				"Use it when a resolution found nothing and the developer needs the signal " +
				"anyway, or when they name an adapter directly. It is not a way to find data: " +
				"for that, resolve_semantic_selection finds devices and imports together.\n\n" +
				"Before deploying, check list_import_instances with import_type_ids — an " +
				"instance of the type may already exist, and a second one costs a container and " +
				"a topic for data the platform already pulls.",
			Effect:  "read",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "search": {"type": "string", "description": "Matches the import type name only, upstream."},
			    "function_id": {"type": "string", "description": "Keep only types carrying this measuring function. From search_ontology."},
			    "aspect_id": {"type": "string", "description": "Keep only types carrying this aspect. Its descendants are included."},
			    "import_type_ids": {"type": "array", "items": {"type": "string"}, "description": "Read these types by id. Ignores search and the criteria upstream."},
			    "limit": {"type": "integer"}
			  }
			}`),
			Unavailable: "requires device_selection_url, import_deploy_url and import_repo_url",
			executor:    ifPresent(s.listImportTypes, deps.Imports),
		},
		Definition{
			Name: "probe_availability",
			Description: "Report the time range for which a device has stored data, per service, " +
				"with the pre-computed aggregate tables available. Reads no values.",
			Effect:  "read /data-availability",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {"device_id": {"type": "string"}},
			  "required": ["device_id"]
			}`),
			Unavailable: "requires timescale_wrapper_url",
			executor:    ifPresent(s.probeAvailability, deps.Timeseries),
		},
		Definition{
			Name: "probe_export_data",
			Description: "Report whether an export's timescale table actually holds rows, per column, " +
				"and over what span. Use it before trusting an export as a data source, and " +
				"immediately after creating one: an export exists in the platform whether or not a " +
				"single row was ever written to it, and the export listing cannot tell the two apart. " +
				"Reads no values — it asks for row counts.\n\n" +
				"The states are not interchangeable. `filled` means rows exist and every readable " +
				"column carries values. `partly_filled` means rows exist and a named column is null in " +
				"every one of them, which is the export worker finding the timestamp and not the " +
				"values — report the column rather than reporting the export as working. `empty` means " +
				"nothing was written in the window. `unknown` means the question could not be answered " +
				"and must never be reported as `empty`: one sends a developer to fix their export, the " +
				"other does not.",
			Effect:  "read /usage/exports and one bucketed row count",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "export_id": {"type": "string", "description": "The export's id, as list_import_instances reports it under history.export_id and as create_export returns it."},
			    "from": {"type": "string", "description": "RFC3339. Empty means a multi-year lookback, which is deliberate: an export that stopped receiving rows a year ago must not be reported as empty."},
			    "to": {"type": "string", "description": "RFC3339."}
			  },
			  "required": ["export_id"]
			}`),
			Unavailable: "requires timescale_wrapper_url and analytics_serving_url",
			executor:    ifPresent(s.probeExportData, deps.Profiler),
		},
		Definition{
			Name: "estimate_read_cost",
			Description: "Estimate what reading a series would cost before reading it: stored bytes, " +
				"bytes per day, an order-of-magnitude sampling interval and an estimated point " +
				"count for a window. Takes devices, exports, or both. Use this to warn about an " +
				"expensive selection while still at L0.\n\n" +
				"For an export, an absent entry is not an empty export — the accounting is filled by a " +
				"collector and a young export is not in it yet. probe_export_data is what answers " +
				"whether anything is stored.",
			Effect:  "read /usage/devices and /usage/exports",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "device_ids": {"type": "array", "items": {"type": "string"}},
			    "export_ids": {"type": "array", "items": {"type": "string"}, "description": "Export ids, for the import half of the platform. Give these, device_ids, or both."},
			    "from": {"type": "string", "description": "RFC3339. The window to estimate for, together with \"to\"."},
			    "to": {"type": "string", "description": "RFC3339."}
			  }
			}`),
			Unavailable: "requires timescale_wrapper_url",
			executor:    ifPresent(s.estimateReadCost, deps.Timeseries),
		},
		Definition{
			Name: "quick_profile",
			Description: "Rank candidate series for a device set from availability, volume, " +
				"connection state and the ontology, reading no values at all. This is how a " +
				"shortlist is produced before any value is exposed. The list is ranked " +
				"strongest first and truncated to a token budget; what was cut is reported " +
				"per device, so narrow the search rather than assuming you saw everything.",
			Effect:  "assemble QuickProfile, no series read",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "search": {"type": "string", "description": "Device search term."},
			    "device_limit": {"type": "integer"},
			    "from": {"type": "string", "description": "RFC3339 start of the window coverage is judged against."},
			    "to": {"type": "string", "description": "RFC3339 end."},
			    "include_unqueryable": {"type": "boolean", "description": "Keep variables that exist but cannot be read as a series, ranked last."}
			  }
			}`),
			Unavailable: "requires timescale_wrapper_url",
			executor:    ifPresent(s.quickProfile, deps.Profiler, deps.Devices),
		},

		Definition{
			Name: "propose_related_sets",
			Description: "Propose multi-device sets related through the aspect hierarchy and the " +
				"platform's device relationship graphs. Give it an aspect node — a room, a " +
				"subsystem — and it answers with sets of series from the devices reporting under " +
				"it, so you never have to guess which devices belong together. Reads no values. " +
				"Use it before relate_series: the set it returns is what you pass there, and its " +
				"candidate_set_id records where the members came from.\n\n" +
				"The `origin` of a set says how much the grouping is worth trusting, strongest " +
				"first. `graph_siblings` means the devices converge on one node of a wiring or " +
				"aggregation graph — they are metered together, and `graph.via_name` says where " +
				"they meet. `graph_flow` is a sub-metering pair: one side measures what the other " +
				"feeds, which is **containment rather than co-occurrence** — do not report " +
				"\"the sub-meter runs whenever the main meter runs\" as a finding, because that is " +
				"arithmetic; the fault case is the reverse. `device_group` is an asserted grouping " +
				"without the topology, and `aspect_node` or `aspect_subtree` only a shared label. " +
				"A member with `from_aspect: false` was reached through a graph and sits outside " +
				"the aspect you asked about, which is normal for a meter one level up.",
			Effect:  "read ontology",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "aspect_id": {
			      "type": "string",
			      "description": "The aspect node to propose from, as search_ontology returns it."
			    },
			    "include_descendants": {
			      "type": "boolean",
			      "description": "Keep series declared against nodes below the requested one. Use it when the devices you want sit on sibling nodes — an oven on \"Kitchen\" and lights on \"Kitchen Ceiling\"."
			    },
			    "limit": {"type": "integer", "description": "How many devices to expand."}
			  },
			  "required": ["aspect_id"]
			}`),
			Unavailable: "requires a timescale-wrapper URL (the relational profiler needs the profiler)",
			executor:    ifPresent(s.proposeRelatedSets, deps.Relations),
		},

		// ---- L1: computed statistics. Aggregates are still data (§3.2). ----
		Definition{
			Name: "profile_series",
			Description: "Compute the full deterministic SeriesProfile for every variable of one " +
				"service, or every column of one export: coverage and gaps, sampling regularity, " +
				"counter-versus-instantaneous classification, units, distribution, periodicity, " +
				"trend, activity sessions and quality flags. You read the profile; you never compute " +
				"statistics yourself. Fields that could not be computed say so explicitly — an absent " +
				"field means \"could not determine\", never \"zero\" or \"none\".\n\n" +
				"For an export, two things differ and both are reported rather than smoothed over: " +
				"the window comes from counting rows, because the platform has no availability " +
				"endpoint for an export, and a column carries units only where the import type " +
				"behind the export still declares them. An export with no rows is refused rather " +
				"than profiled into a body of \"not_computed\" — call probe_export_data first if you " +
				"want that answer without the refusal.",
			Effect:  "compute SeriesProfile (source-scoped batch read)",
			MinTier: L1,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "device_id": {"type": "string"},
			    "service_id": {"type": "string"},
			    "export_id": {"type": "string", "description": "Profile an export's own table instead. Exclusive with device_id and service_id, and the variable paths are then the export's column names."},
			    "from": {"type": "string", "description": "RFC3339 start of the analysis window."},
			    "to": {"type": "string", "description": "RFC3339 end of the analysis window."},
			    "group_time": {"type": "string", "description": "Aggregation bucket, e.g. \"15m\". Empty means derived from the detected sampling interval, which is the better answer unless you mean to override it. At least 1s, and coarse enough that the analysis window divides into at most 500000 buckets; a bucket below that is refused and the refusal names the finest one the window allows."},
			    "variable_paths": {
			      "type": "array", "items": {"type": "string"},
			      "description": "Restrict the response to these variable paths — for an export, its column names. The source is read once for all of its variables either way, so this narrows what you read back, not what it costs. Omit it and the response carries the first few profiles and names the variables it left out."
			    }
			  }
			}`),
			Unavailable: "requires timescale_wrapper_url",
			executor:    ifPresent(s.profileSeries, deps.Profiler, deps.Devices),
		},
		Definition{
			Name: "get_sessions",
			Description: "Page through the detected activity sessions of a profile. The profile " +
				"carries only summary statistics and a few exemplars; this is the full list.",
			Effect:  "read paginated session resource",
			MinTier: L1,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "profile_id": {"type": "string"},
			    "from": {"type": "string"}, "to": {"type": "string"},
			    "limit": {"type": "integer"}, "cursor": {"type": "string"}
			  },
			  "required": ["profile_id"]
			}`),
			Unavailable: "requires timescale_wrapper_url",
			executor:    ifPresent(s.getSessions, deps.Profiler),
		},

		Definition{
			Name: "relate_series",
			Description: "Compute a RelationProfile over several series: which of them are active at " +
				"the same time, how reliably, and under which conditions that stops holding. Each " +
				"series is turned into idle/active using the profiler's own detected threshold, then " +
				"all of them are read onto one aligned grid, then every pair is tabulated and " +
				"conditioned on hour of day and weekday/weekend. What comes back are candidate " +
				"rules with support, confidence, lift and explicit exception windows — for example " +
				"\"while the oven is active the kitchen lights are active, except 06:00-12:00\". " +
				"Returns no series values. **The rules are candidates.** They are not anomaly " +
				"definitions and nothing downstream reads them: only the developer can confirm one, " +
				"and you have no tool for that. Propose them, explain the numbers, and ask.",
			Effect:  "compute + read",
			MinTier: L1,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "series": {
			      "type": "array",
			      "description": "Two or more series, from at most a handful of devices. A pattern across two devices is the point; a set of eight channels of one meter is not.",
			      "items": {
			        "type": "object",
			        "properties": {
			          "device_id": {"type": "string"},
			          "service_id": {"type": "string"},
			          "variable_path": {"type": "string"},
			          "label": {"type": "string", "description": "What to call this series in a rule statement, e.g. \"the oven\". Omit and a device name is used."}
			        },
			        "required": ["device_id", "service_id", "variable_path"]
			      }
			    },
			    "from": {"type": "string", "description": "RFC3339. Omit for the default lookback, which is a month — an exception at certain times of day needs more than a week of samples."},
			    "to": {"type": "string", "description": "RFC3339."},
			    "candidate_set_id": {"type": "string", "description": "The set_id propose_related_sets gave you, so a confirmed rule can be traced back to the aspect that suggested the devices."},
			    "min_confidence": {"type": "number", "description": "How reliably a pattern must hold to be proposed, 0 to 1. Default 0.7. Raise it to narrow a long rule list rather than reading a truncated one."},
			    "min_lift": {"type": "number", "description": "How much more often the pair must co-occur than independent base rates predict. Default 1.2. This is what separates a finding from a device that is simply always on."},
			    "hour_buckets": {"type": "integer", "description": "How many equal parts the day is split into for conditioning. Default 4."}
			  },
			  "required": ["series"]
			}`),
			Unavailable: "requires a timescale-wrapper URL (the relational profiler needs the profiler)",
			executor:    ifPresent(s.relateSeries, deps.Relations),
		},

		// ---- L2: actual values. ----
		Definition{
			Name: "preview_series",
			Description: "Read a downsampled preview of actual values for one series — a device's " +
				"variable or an export's column — to reason about its shape. Heavily aggregated and " +
				"point-capped: this is for seeing the form of a signal, not for computing statistics " +
				"from it.",
			Effect:  "read values",
			MinTier: L2,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "device_id": {"type": "string"},
			    "service_id": {"type": "string"},
			    "export_id": {"type": "string", "description": "Preview an export's column instead. Exclusive with device_id and service_id."},
			    "variable_path": {"type": "string", "description": "The variable path, or for an export the column name."},
			    "from": {"type": "string", "description": "RFC3339."},
			    "to": {"type": "string", "description": "RFC3339."},
			    "group_time": {"type": "string", "description": "Aggregation bucket, e.g. \"1h\"."},
			    "group_type": {"type": "string", "description": "mean, min, max, sum, first, last, median, difference-mean, difference-sum, difference-last, time-weighted-mean-linear."},
			    "max_points": {"type": "integer"}
			  },
			  "required": ["variable_path"]
			}`),
			Unavailable: "requires timescale_wrapper_url",
			executor:    ifPresent(s.previewSeries, deps.Timeseries),
		},

		// ---- Confirmed developer actions (D11). ----
		Definition{
			Name: "propose_data_selection",
			Description: "Propose a concrete set of series as the data selection for this project, " +
				"with the reason for each. The developer must confirm; nothing is applied on your " +
				"word alone. Propose only series you have grounds for from the ontology or a profile.",
			Effect:  "write session state",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "rationale": {"type": "string", "description": "Why this set, as a whole."},
			    "series": {
			      "type": "array",
			      "items": {
			        "type": "object",
			        "properties": {
			          "device_id": {"type": "string"},
			          "service_id": {"type": "string"},
			          "variable_path": {"type": "string"},
			          "role": {"type": "string", "description": "e.g. \"target\", \"feature\", \"exogenous\"."},
			          "reason": {"type": "string"}
			        },
			        "required": ["device_id", "service_id", "variable_path"]
			      }
			    }
			  },
			  "required": ["series", "rationale"]
			}`),
			Unavailable: "requires a chat store",
			executor:    ifPresent(s.proposeDataSelection, deps.SelectionSink),
		},

		Definition{
			Name: "propose_operator_input",
			Description: "Propose the concrete pipeline input that wires one import instance into " +
				"an operator, ready for the developer to deploy. You name the instance and which " +
				"of its variables feeds which operator input; this returns the exact node input " +
				"the analytics flow engine takes, with the topic and the filter resolved.\n\n" +
				"The developer must confirm; nothing is deployed on your word. Propose only " +
				"variables you found through resolve_semantic_selection or " +
				"get_import_type_metadata — a path you guessed produces an operator that " +
				"subscribes successfully and never receives a value.\n\n" +
				"One proposal is one import instance. Every instance has its own topic, so two " +
				"imports are two inputs.",
			Effect:  "emit a pipeline input, no deployment",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "instance_id": {"type": "string", "description": "The import instance to consume."},
			    "rationale": {"type": "string", "description": "Why this import, and why these variables."},
			    "bindings": {
			      "type": "array",
			      "items": {
			        "type": "object",
			        "properties": {
			          "input_name": {"type": "string", "description": "The operator's own name for the input, which its code reads."},
			          "variable_path": {"type": "string", "description": "The import variable to feed it, as resolve_semantic_selection reported it."},
			          "reason": {"type": "string"}
			        },
			        "required": ["input_name", "variable_path"]
			      }
			    }
			  },
			  "required": ["instance_id", "bindings", "rationale"]
			}`),
			Unavailable: "requires device_selection_url and import_deploy_url",
			executor:    ifPresent(s.proposeOperatorInput, deps.Imports),
		},

		Definition{
			Name: "create_import_instance",
			Description: "Deploy an import instance from an import type, so the platform starts " +
				"pulling that data in. This is the only way to obtain a signal the platform does " +
				"not carry yet — where resolve_semantic_selection found nothing and an import type " +
				"describes what is wanted.\n\n" +
				"The developer must confirm, and they see the exact configuration first. Read the " +
				"import type with get_import_type_metadata before proposing: every config you set " +
				"must be one the type declares, and a config you leave out takes the type's own " +
				"default rather than nothing.\n\n" +
				"You cannot set a config that is a credential — an api key, a password, a token. " +
				"Those are entered by the developer in the platform's import dialog, and this tool " +
				"refuses an import type that needs one. Say so rather than inventing a value.\n\n" +
				"A new instance starts empty: it has no past, and no history until an export exists " +
				"for it (see create_export).",
			Effect:  "deploy an import container on the platform",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "import_type_id": {"type": "string", "description": "The import type to deploy, as get_import_type_metadata reports it."},
			    "name": {"type": "string", "description": "What the developer will find this import under. Say what it imports, not that it is an import."},
			    "rationale": {"type": "string", "description": "Why this import type, and why deploying a new instance rather than using one that exists."},
			    "configs": {
			      "type": "array",
			      "description": "Configuration values. Omit a config to take the import type's declared default.",
			      "items": {
			        "type": "object",
			        "properties": {
			          "name": {"type": "string", "description": "Exactly as the import type declares it."},
			          "value": {"description": "A string, number, boolean, list or object, matching the type the config declares."}
			        },
			        "required": ["name", "value"]
			      }
			    },
			    "restart": {"type": "boolean", "description": "Whether the container restarts on failure. Omit to take the import type's default."}
			  },
			  "required": ["import_type_id", "name", "rationale"]
			}`),
			Unavailable: "requires device_selection_url, import_deploy_url and import_repo_url",
			executor:    ifPresent(s.createImportInstance, deps.Imports),
		},

		Definition{
			Name: "create_export",
			Description: "Create an export of an import instance, so its values are stored in " +
				"timescale and can be profiled, charted and trained on. Use it when an import's " +
				"history reads live_only and the developer needs its past rather than only what " +
				"arrives from now on.\n\n" +
				"The developer must confirm. Name only variables you found through " +
				"resolve_semantic_selection or get_import_type_metadata, and only the ones that are " +
				"actually needed: an export stores the columns it was created with, and adding one " +
				"later means creating a second export.\n\n" +
				"An export is not retroactive. It stores what the topic still retains from the " +
				"offset you choose onward, which is Kafka's retention window and not the import's " +
				"whole past — say that to the developer rather than promising history that does not " +
				"exist.\n\n" +
				"An export is timestamped with the moment the import wrote the message, which is " +
				"what an import that polls a live source wants. An import that carries the time its " +
				"values describe — a backfill of past weather, a forecast — needs `time_path` set " +
				"to that variable instead, or a replay of years lands inside the minutes the replay " +
				"took and reads as no history at all. Ask the developer for the format that " +
				"variable is written in; do not guess one.",
			Effect:  "create an export and the timescale table behind it",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "instance_id": {"type": "string", "description": "The import instance to export."},
			    "name": {"type": "string", "description": "What the developer will find this export under."},
			    "description": {"type": "string"},
			    "rationale": {"type": "string", "description": "Why this import needs storing, and why these variables."},
			    "offset": {
			      "type": "string",
			      "enum": ["smallest", "largest"],
			      "description": "Where the export starts reading. \"smallest\" takes what the topic still retains, \"largest\" only what arrives from now on. Omit for the deployment's default."
			    },
			    "time_path": {
			      "type": "string",
			      "description": "The variable the export takes its timestamps from. Omit for the message's own arrival time, which is right for an import polling a live source. Set it to the variable that carries the described time — as get_import_type_metadata reports it — when the import backfills the past or forecasts the future."
			    },
			    "timestamp_format": {
			      "type": "string",
			      "description": "How the export worker parses the timestamp, in this platform's own format vocabulary. Omit unless the developer gave you one: it belongs with time_path, because a different variable is usually written differently, and a format that does not parse stores no rows at all. Never invent one."
			    },
			    "values": {
			      "type": "array",
			      "description": "The variables to store, each becoming one column.",
			      "items": {
			        "type": "object",
			        "properties": {
			          "variable_path": {"type": "string", "description": "The import variable, as resolve_semantic_selection reported it."},
			          "column": {"type": "string", "description": "The column name. Omit to take the variable's own last path element."},
			          "tag": {"type": "boolean", "description": "True for a label to group by rather than a measured value."}
			        },
			        "required": ["variable_path"]
			      }
			    }
			  },
			  "required": ["instance_id", "name", "values", "rationale"]
			}`),
			Unavailable: "requires analytics_serving_url and import_repo_url",
			executor:    ifPresent(s.createExport, deps.Imports),
		},

		Definition{
			Name: "delete_import_instance",
			Description: "Remove an import instance **that this session created**. Any other id is " +
				"refused: deleting an import is not yours to propose for something that was already " +
				"there.\n\n" +
				"The developer must confirm. This deletes the instance's Kafka topic with it, so " +
				"every message it still held is gone and anything consuming that topic stops " +
				"receiving — it is an undo of a deployment you just made, not a cleanup tool.",
			Effect:  "remove an import instance this session created, and its kafka topic",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "instance_id": {"type": "string", "description": "The instance, which must be one this session created."},
			    "rationale": {"type": "string", "description": "Why it should be removed."}
			  },
			  "required": ["instance_id", "rationale"]
			}`),
			Unavailable: "requires import_deploy_url and a chat store",
			executor:    ifPresent(s.deleteImportInstance, deps.Imports, deps.Creations),
		},

		Definition{
			Name: "delete_export",
			Description: "Remove an export **that this session created**. Any other id is refused.\n\n" +
				"The developer must confirm. This drops the export's timescale table, so everything " +
				"it had stored is gone and the import goes back to having no history at all.",
			Effect:  "remove an export this session created, and its stored table",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "export_id": {"type": "string", "description": "The export, which must be one this session created."},
			    "rationale": {"type": "string", "description": "Why it should be removed."}
			  },
			  "required": ["export_id", "rationale"]
			}`),
			Unavailable: "requires analytics_serving_url and a chat store",
			executor:    ifPresent(s.deleteExport, deps.Imports, deps.Creations),
		},

		Definition{
			Name: "render_chart",
			Description: "Emit a declarative chart specification for the developer's exploration pane " +
				"to draw. This is how you demonstrate a claim about data visually: you name the series, " +
				"the transform and the annotations, and the pane renders them from the developer's own " +
				"read. You never receive the values — this returns the resolved axis, the units and the " +
				"chart id only. Transforms are evaluated by the platform, including unit conversion, so " +
				"never compute one yourself. Annotate what you want the developer to check, and mark an " +
				"annotation confirmable when it is a claim they should confirm or correct.",
			Effect:  "emit chart spec",
			MinTier: L1,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "title": {"type": "string"},
			    "caption": {"type": "string", "description": "What the developer should take from the chart."},
			    "series": {
			      "type": "array",
			      "description": "Up to eight series, drawn on one axis.",
			      "items": {
			        "type": "object",
			        "properties": {
			          "device_id": {"type": "string"},
			          "service_id": {"type": "string"},
			          "variable_path": {"type": "string"},
			          "label": {"type": "string"},
			          "transform": {
			            "type": "string",
			            "description": "one of: none | diff | rate | resample:<interval, e.g. 900s or 15m> | convert:<target characteristic id>. diff and rate are for cumulative counters; convert needs a target reachable through the ontology conversion graph, which profile_series reports as available_conversions."
			          },
			          "profile_id": {
			            "type": "string",
			            "description": "The profile whose detected sessions, gaps, exclusions and counter resets should be drawn as annotations. Omit and the chart carries only the annotations you write yourself."
			          }
			        },
			        "required": ["device_id", "service_id", "variable_path"]
			      }
			    },
			    "annotations": {
			      "type": "array",
			      "description": "Labelled time ranges. Use them to point at what you want looked at.",
			      "items": {
			        "type": "object",
			        "properties": {
			          "from": {"type": "string", "description": "RFC3339."},
			          "to": {"type": "string", "description": "RFC3339."},
			          "label": {"type": "string"},
			          "severity": {"type": "string", "description": "info | warn | error"},
			          "source": {"type": "string", "description": "Where the claim comes from, e.g. \"profile.quality_flags\"."},
			          "series_index": {"type": "integer", "description": "Which series it applies to; required when confirmable."},
			          "confirmable": {"type": "boolean", "description": "Ask the developer to confirm, correct or reject this. Requires field_path."},
			          "field_path": {"type": "string", "description": "The profile field a confirmation writes to, e.g. activity_pattern.sessions or sampling.gaps."}
			        },
			        "required": ["from", "to", "label"]
			      }
			    },
			    "markers": {
			      "type": "array",
			      "description": "Labelled instants.",
			      "items": {
			        "type": "object",
			        "properties": {
			          "at": {"type": "string", "description": "RFC3339."},
			          "label": {"type": "string"},
			          "source": {"type": "string"},
			          "series_index": {"type": "integer"}
			        },
			        "required": ["at", "label"]
			      }
			    },
			    "y_axis": {
			      "type": "object",
			      "description": "Optional. Omit it and the axis is resolved from the ontology, which is the better answer — state one only when you mean to override that.",
			      "properties": {
			        "unit": {"type": "string"},
			        "unit_source": {"type": "string"}
			      }
			    },
			    "window": {
			      "type": "object",
			      "description": "The charted range. Omit it and the analysis window of a named profile is used, or the last seven days.",
			      "properties": {"from": {"type": "string"}, "to": {"type": "string"}}
			    },
			    "group_time": {
			      "type": "string",
			      "description": "Aggregation bucket for every series, e.g. \"15m\". Omit it and one is derived that fits the point cap."
			    }
			  },
			  "required": ["series"]
			}`),
			Unavailable: "requires timescale_wrapper_url",
			executor:    ifPresent(s.renderChart, deps.Charts),
		},
		Definition{
			Name: "write_file",
			Description: "Write a file into the developer's working copy of the operator " +
				"repository, on their own persistent storage. The path is relative to the " +
				"repository root and every file of it is writable, including the Dockerfile " +
				"and .github/workflows/build.yml. The whole file is replaced, so send its " +
				"complete new content rather than a fragment. Nothing is staged, committed " +
				"or pushed: the developer reviews the change and commits it. Do not write " +
				"evaluation.yaml — the criteria are the developer's.",
			Effect:  "write repo working copy",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "path": {"type": "string", "description": "Path relative to the repository root, e.g. op.py or tests/test_op.py."},
			    "content": {"type": "string", "description": "The file's complete new content."}
			  },
			  "required": ["path", "content"]
			}`),
			Unavailable: "requires github_client_id and a Hub",
			executor:    ifPresent(s.writeFile, deps.Repo),
		},
		Definition{
			Name: "run_code",
			Description: "Execute Python in the developer's own JupyterHub pod, with their own " +
				"platform authorisation and nothing more. The developer must confirm each run. " +
				"The kernel keeps its state between calls, so variables defined in one call are " +
				"there in the next, and its working directory is a workspace on persistent " +
				"storage — a file written there is still there in a later session. The " +
				"developer's platform access token is in the SENERGY_TOKEN environment variable. " +
				"Output is capped: print what you need rather than everything, and write large " +
				"results to a file in the workspace instead of to stdout.\n\n" +
				"The kernel is not wired to MLflow or to the Ray cluster: it carries " +
				"SENERGY_TOKEN, ODE_WORKSPACE and the two platform URLs, and nothing else. " +
				"Operator code that logs through Operator Lib therefore records nothing anyone " +
				"can find again, and `ray.init()` here starts a Ray inside the pod rather than " +
				"reaching the cluster. So this is the right loop for finding out whether " +
				"something works and the wrong one for a result meant to be kept — never " +
				"describe a cell as a tracked run.",
			Effect:  "execute in kernel",
			MinTier: L0,
			Confirm: true,
			// The one tool with a standing answer, and the reason auto mode exists: a
			// developer reading a dataframe meets this confirmation dozens of times an
			// afternoon, and `df.head()` is the same question every time.
			//
			// What is recognised — and why recognising is not the same as judging safe
			// — is in pkg/plaincode. Inert unless the session asked for it, and the
			// only predicate on the whole surface, so no configuration can turn auto
			// mode into a waiver for the tools that create or delete platform objects.
			AutoApprove: func(input json.RawMessage) (bool, string) {
				var in runCodeInput
				if err := json.Unmarshal(input, &in); err != nil {
					return false, "the arguments did not parse"
				}
				return plaincode.Recognised(in.Code)
			},
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "code": {"type": "string", "description": "Python source. Runs as one cell, so the value of the last expression comes back as the result."}
			  },
			  "required": ["code"]
			}`),
			Unavailable: "requires jupyterhub_url",
			executor:    ifPresent(s.runCode, deps.Kernel),
		},
		Definition{
			Name: "launch_experiment",
			Description: "Submit a training run to the Ray cluster from the developer's " +
				"**committed** repository state, and create the MLflow run it will log to. " +
				"The developer must confirm each launch: it spends cluster time.\n\n" +
				"The job is built with `git archive` of HEAD, so **a working copy with " +
				"uncommitted changes is refused** and the refusal names the files. That is " +
				"not a nuisance check — the run is tagged with the commit SHA and is " +
				"supposed to be reproducible from it, which is only true if the code that " +
				"ran is that commit. When it refuses, ask the developer to commit; you have " +
				"no tool that commits for them.\n\n" +
				"The job reads its training data from the platform directly with its own " +
				"credential, so it does not stream through this conversation and you will " +
				"not see its output. Read the credential block in the answer: when it says " +
				"the token expires with the session, say so before proposing a long run.\n\n" +
				"Before proposing a launch, ask whether a recorded run is what the developer " +
				"needs. Trying an idea out is cheaper in run_code: the same code, in their pod, " +
				"against the same data, with no commit and no cluster time. A launch is for a " +
				"result that will be held against another result later, which is what the commit " +
				"SHA and the MLflow run buy. When the question is only whether the fit does what " +
				"they think, say that a cell answers it sooner.",
			Effect:  "submit Ray job",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "entrypoint": {
			      "type": "string",
			      "description": "The command Ray runs in the unpacked repository, e.g. \"python training.py --folds 5\". Omit it for the deployment's default, which points at the scaffold's training.py."
			    },
			    "env_vars": {
			      "type": "object",
			      "description": "Extra environment variables for the job, as a flat string-to-string map. This is how a hyperparameter reaches a run that reads one from the environment. The MLflow and platform variables are set by ODE and cannot be overridden here.",
			      "additionalProperties": {"type": "string"}
			    },
			    "run_name": {
			      "type": "string",
			      "description": "What to call the run in MLflow. Omit and it is named after the commit. Use it to say what the run is trying, e.g. \"wider lookback, 5 folds\"."
			    }
			  }
			}`),
			Unavailable: "requires ray_url and mlflow_url",
			executor:    ifPresent(s.launchExperiment, deps.Experiments),
		},
		Definition{
			Name: "get_experiment_results",
			Description: "Read one experiment as a compact structured summary: status, " +
				"params, the latest value of each metric, the run's tags, the resource usage, " +
				"and **the comparison against the previous run of the same experiment** — " +
				"which is usually the number that answers whether a change helped. Call it " +
				"without an experiment_id to list the developer's recent runs and choose one.\n\n" +
				"It never returns logs, stdout or an artifact, by design: you interpret " +
				"recorded metrics, you do not read a training process's output. A run that " +
				"has not finished answers with a snapshot and says so — do not report a " +
				"mid-run metric as a result.\n\n" +
				"`comparison_to_previous` carries `lower_is_better` beside each direction, " +
				"and it is inferred from the metric's *name*. Say which way you read a metric " +
				"when it matters rather than asserting an improvement the naming happened to " +
				"produce.",
			Effect:  "read MLflow",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "experiment_id": {
			      "type": "string",
			      "description": "The experiment_id launch_experiment returned. Omit it to get the developer's recent runs instead, newest first, so you can name one."
			    }
			  }
			}`),
			Unavailable: "requires ray_url and mlflow_url",
			executor:    ifPresent(s.getExperimentResults, deps.Experiments),
		},
	)
}

// surface holds the deps for the executor methods.
type surface struct{ deps Deps }

// ifPresent returns the executor only when every dependency it needs is there.
//
// When one is absent the tool keeps its declaration and its Unavailable reason,
// so it stays in the published table, is never advertised, and answers a direct
// call by saying which service this deployment is missing.
//
// The parameters are interfaces, so the nil check has to look through the
// interface value rather than compare it — a Deps field filled from a typed nil
// pointer is non-nil as an interface. That is the footgun pkg.rankerOrNil
// documents, and isNil is why it cannot bite here.
func ifPresent(executor Executor, dependencies ...any) Executor {
	for _, dependency := range dependencies {
		if isNil(dependency) {
			return nil
		}
	}
	return executor
}
