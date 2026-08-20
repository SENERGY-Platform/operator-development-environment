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

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/charts"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
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
		Query(ctx context.Context, token string, elements []timeseries.QueryElement, opts timeseries.QueryOptions) ([]timeseries.QueryResult, error)
	}

	Profiler interface {
		QuickProfiles(ctx context.Context, token string, req profiler.QuickRequest) (profiler.QuickResult, error)
		ProfileService(ctx context.Context, token string, req profiler.ProfileRequest) (profiler.ProfileResult, error)
		Profile(profileID string) (profiler.ResolvedProfile, bool)
		Store() profiler.Store
	}

	Selection interface {
		Resolve(ctx context.Context, token string, req selection.Request) (selection.Result, error)
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
		Run(ctx context.Context, bearer, code string) (<-chan kernel.ExecutionEvent, error)
		Workspace() string
	}
)

// Deps carries the services the executors need. Every field is optional: a
// deployment without a timescale-wrapper has no profiler, and the tools that
// need one are then declared without an executor rather than registered and
// broken. That is the same degradation the M1 routes already do.
type Deps struct {
	Ontology      Ontology
	Devices       Devices
	Timeseries    Timeseries
	Profiler      Profiler
	Selection     Selection
	SelectionSink SelectionSink
	Kernel        Kernel
	Charts        Charts
	Relations     Relations

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
// Every one of the eighteen tools is declared. A tool whose dependency is absent,
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
			Unavailable: "M0 — requires device_repo_url",
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
			Unavailable: "M2 — requires device_repo_url",
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
			Unavailable: "M0 — requires device_repo_url",
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
			Unavailable: "M0 — requires device_repo_url",
			executor:    ifPresent(s.getDeviceMetadata, deps.Devices),
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
			Unavailable: "M1a — requires timescale_wrapper_url",
			executor:    ifPresent(s.probeAvailability, deps.Timeseries),
		},
		Definition{
			Name: "estimate_read_cost",
			Description: "Estimate what reading a series would cost before reading it: stored bytes, " +
				"bytes per day, an order-of-magnitude sampling interval and an estimated point " +
				"count for a window. Use this to warn about an expensive selection while still at L0.",
			Effect:  "read /usage/devices",
			MinTier: L0,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "device_ids": {"type": "array", "items": {"type": "string"}},
			    "from": {"type": "string", "description": "RFC3339. The window to estimate for, together with \"to\"."},
			    "to": {"type": "string", "description": "RFC3339."}
			  },
			  "required": ["device_ids"]
			}`),
			Unavailable: "M1a — requires timescale_wrapper_url",
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
			Unavailable: "M1a — requires timescale_wrapper_url",
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
				"service: coverage and gaps, sampling regularity, counter-versus-instantaneous " +
				"classification, units, distribution, periodicity, trend, activity sessions and " +
				"quality flags. You read the profile; you never compute statistics yourself. " +
				"Fields that could not be computed say so explicitly — an absent field means " +
				"\"could not determine\", never \"zero\" or \"none\".",
			Effect:  "compute SeriesProfile (service-scoped batch read)",
			MinTier: L1,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "device_id": {"type": "string"},
			    "service_id": {"type": "string"},
			    "from": {"type": "string", "description": "RFC3339 start of the analysis window."},
			    "to": {"type": "string", "description": "RFC3339 end of the analysis window."},
			    "group_time": {"type": "string", "description": "Aggregation bucket, e.g. \"15m\". Empty means derived from the detected sampling interval."},
			    "variable_paths": {
			      "type": "array", "items": {"type": "string"},
			      "description": "Restrict the response to these variable paths. The service is read once for all of its variables either way, so this narrows what you read back, not what it costs. Omit it and the response carries the first few profiles and names the variables it left out."
			    }
			  },
			  "required": ["device_id", "service_id"]
			}`),
			Unavailable: "M1b — requires timescale_wrapper_url",
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
			Unavailable: "M1b — requires timescale_wrapper_url",
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
			Description: "Read a downsampled preview of actual values for one series, to reason " +
				"about its shape. Heavily aggregated and point-capped: this is for seeing the " +
				"form of a signal, not for computing statistics from it.",
			Effect:  "read values",
			MinTier: L2,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "device_id": {"type": "string"},
			    "service_id": {"type": "string"},
			    "variable_path": {"type": "string"},
			    "from": {"type": "string", "description": "RFC3339."},
			    "to": {"type": "string", "description": "RFC3339."},
			    "group_time": {"type": "string", "description": "Aggregation bucket, e.g. \"1h\"."},
			    "group_type": {"type": "string", "description": "mean, min, max, sum, first, last, median, difference-mean, difference-sum, difference-last, time-weighted-mean-linear."},
			    "max_points": {"type": "integer"}
			  },
			  "required": ["device_id", "service_id", "variable_path"]
			}`),
			Unavailable: "M1a — requires timescale_wrapper_url",
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
			Unavailable: "M3 — requires a chat store",
			executor:    ifPresent(s.proposeDataSelection, deps.SelectionSink),
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
			Unavailable: "M5 — requires timescale_wrapper_url",
			executor:    ifPresent(s.renderChart, deps.Charts),
		},
		Definition{
			Name:        "write_file",
			Description: "Write a file into the repository working copy.",
			Effect:      "write repo working copy",
			MinTier:     L0,
			Schema:      json.RawMessage(`{"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}}}`),
			Unavailable: "M7 (repo/, SPEC §5.11)",
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
				"results to a file in the workspace instead of to stdout.",
			Effect:  "execute in kernel",
			MinTier: L0,
			Confirm: true,
			Schema: json.RawMessage(`{
			  "type": "object",
			  "properties": {
			    "code": {"type": "string", "description": "Python source. Runs as one cell, so the value of the last expression comes back as the result."}
			  },
			  "required": ["code"]
			}`),
			Unavailable: "M4 — requires jupyterhub_url",
			executor:    ifPresent(s.runCode, deps.Kernel),
		},
		Definition{
			Name:        "launch_experiment",
			Description: "Submit a Ray job for the current repository state.",
			Effect:      "submit Ray job",
			MinTier:     L0,
			Confirm:     true,
			Schema:      json.RawMessage(`{"type": "object", "properties": {"entrypoint": {"type": "string"}}}`),
			Unavailable: "M8 (experiments/, SPEC §5.12)",
		},
		Definition{
			Name:        "get_experiment_results",
			Description: "Read metrics and parameters of a finished run from MLflow.",
			Effect:      "read MLflow",
			MinTier:     L0,
			Schema:      json.RawMessage(`{"type": "object", "properties": {"run_id": {"type": "string"}}}`),
			Unavailable: "M8 (experiments/, SPEC §5.12)",
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
