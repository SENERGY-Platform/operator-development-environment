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

// Package selection implements semantic data selection (§5.2): the
// operation the spec calls resolve_semantic_selection.
//
// A text intent is resolved through the ontology to concrete addressable series,
// in five steps and without reading a single value:
//
//	intent            → matched functions, aspects (pkg/ontology, lexical)
//	matched entities  → filter criteria             (one request per combination)
//	criteria          → device types, services, variable paths
//	device types      → devices the caller may read data from
//	devices × paths   → QuickProfiles, ranked       (pkg/profiler, tier L0)
//
// The whole operation sits at exposure tier L0, which the response states rather
// than claims: reads.values is part of the document and is zero. That is what
// makes the Datensparsamkeit argument of §3.2 concrete — a developer goes from a
// problem statement to a ranked shortlist before any value is exposed.
//
// Nothing here is cached. The ontology snapshot underneath it is; the selectables
// query is not (see ontology.DeviceTypeSelectables), and devices never are,
// because device visibility is per user.
package selection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// ErrInvalidRequest is a caller's mistake rather than a platform failure, so the
// API can answer 400 instead of reporting an outage.
var ErrInvalidRequest = errors.New("selection: invalid request")

// Ontology is the slice of pkg/ontology this package needs. Declared here rather
// than imported wholesale so the tests can answer with three functions instead
// of a platform snapshot.
type Ontology interface {
	Snapshot(ctx context.Context, token string) (*ontology.Snapshot, error)
	DeviceTypeSelectables(ctx context.Context, token string, criteria []drmodel.FilterCriteria, opts ontology.SelectableOptions) ([]drmodel.DeviceTypeSelectable, error)
}

// Devices lists devices as the calling user (D5). The token is a parameter for
// the same reason it is everywhere else in ODE: the platform's per-user
// permissions are the only authority on what a developer may select.
type Devices interface {
	List(token string, options drmodel.ExtendedDeviceListOptions) (devices.ListResult, error)
}

// Ranker orders the resolved series by QuickProfile (§5.2: "candidates are
// ranked by QuickProfile, not returned unordered").
//
// It is optional. A deployment without a timescale-wrapper URL runs no profiler,
// and the ontology half of this answer is still worth serving — with a note
// saying the ranking is missing, rather than a 404 on the whole operation.
type Ranker interface {
	QuickProfiles(ctx context.Context, token string, req profiler.QuickRequest) (profiler.QuickResult, error)
}

type Options struct {
	// Concurrency bounds the selectables requests. There is one per criteria
	// combination and they are independent, so this is what decides how long a
	// resolution takes.
	Concurrency int
	// MaxCriteria caps the criteria cross product. Each combination is one
	// platform request, and matched functions times matched aspects grows fast.
	MaxCriteria int
	// DeviceLimit is how many devices a resolution expands when the request names
	// no limit of its own.
	DeviceLimit int64
}

const (
	defaultConcurrency = 4
	defaultMaxCriteria = 12
	defaultDeviceLimit = 10
)

type Resolver struct {
	ontology Ontology
	index    profiler.OntologySource
	devices  Devices
	ranker   Ranker
	imports  Imports
	opts     Options
}

// New wires a resolver. ranker and imp may be nil; index may not, because the
// unit and completeness of every resolved variable come from it.
//
// imp being optional is not the same kind of optional as ranker. Without a ranker
// the answer is the same set of series in a worse order; without imports it is a
// smaller set — a whole class of operator input goes unmentioned. Both degrade
// rather than fail, but only the second one changes what the developer is told
// exists, which is why Notes says so explicitly (see importNotes).
func New(ont Ontology, index profiler.OntologySource, dev Devices, ranker Ranker, imp Imports, opts Options) (*Resolver, error) {
	if ont == nil || index == nil || dev == nil {
		return nil, errors.New("selection: an ontology, an ontology index and a device lister are required")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.MaxCriteria <= 0 {
		opts.MaxCriteria = defaultMaxCriteria
	}
	if opts.DeviceLimit <= 0 {
		opts.DeviceLimit = defaultDeviceLimit
	}
	return &Resolver{ontology: ont, index: index, devices: dev, ranker: ranker, imports: imp, opts: opts}, nil
}

// Request is one semantic selection.
//
// Intent and the explicit id lists are additive: an intent alone is the
// developer's route, explicit ids alone are how an LLM that has already read the
// ontology asks (§5.8's resolve_semantic_selection), and both together let a
// caller pin one half of the query and search the other.
type Request struct {
	Intent string

	FunctionIDs    []string
	AspectIDs      []string
	DeviceClassIDs []string

	// Interaction filters services by how they reach the platform. Empty means
	// the default, models.EVENT: a request-only service is polled on demand and
	// streams nothing, so no series exists for it (§5.4.13 item 5). Pass
	// InteractionAny to see those too.
	Interaction models.Interaction
	// IncludeControlling searches controlling functions as well as measuring
	// ones. Off by default — a series is something measured.
	IncludeControlling bool

	// MatchLimit and MinScore tune the lexical matcher; zero means its defaults.
	MatchLimit int
	MinScore   float64

	// DeviceLimit is how many devices to expand. Availability is one call per
	// device and cannot be batched, so this decides the wall clock. Zero means
	// the resolver's default; callers are expected to clamp their own ceiling.
	DeviceLimit int64
	// Window is the range the developer cares about, used for the coverage proxy
	// of the ranking. Zero means the profiler's default lookback.
	Window profiler.Window
	// SkipRanking returns the ontology resolution alone. It is the cheap form of
	// this operation: no availability calls, so no per-device round trips.
	SkipRanking bool

	// SkipImports leaves the import half unresolved.
	//
	// Off by default, deliberately: an intent should find every kind of input the
	// platform can satisfy it with, and a caller who has to remember to ask for
	// imports will forget. This exists for the caller that has already resolved
	// them, or that is answering a question about devices specifically.
	SkipImports bool
}

// InteractionAny asks for no interaction filter at all.
const InteractionAny models.Interaction = "any"

// Result is §5.2's document: the four resolution layers, the gaps found on the
// way, and what it cost.
//
// Every list is empty rather than absent when there is nothing in it. D24's
// reasoning about never-null applies to a shape a caller iterates as much as to a
// field they read.
type Result struct {
	Intent         string   `json:"intent"`
	Terms          []string `json:"terms"`
	UnmatchedTerms []string `json:"unmatched_terms"`

	MatchedFunctions     []ontology.FunctionMatch    `json:"matched_functions"`
	MatchedAspects       []ontology.AspectMatch      `json:"matched_aspects"`
	MatchedDeviceClasses []ontology.DeviceClassMatch `json:"matched_device_classes"`

	// MatchElided is what the lexical matcher's per-list limit cut from the three
	// lists above, one entry per list that lost something. It is not folded into the
	// LLMResult's own Elided because the two answer different questions — this one
	// is about the ontology resolution and survives into every form of this document,
	// projected or not.
	MatchElided []ontology.Elision `json:"match_elided"`

	// Criteria is what was actually asked of the platform, with the number of
	// device types each combination returned. It is the difference between "the
	// ontology has nothing for this" and "the platform has no such device".
	Criteria []Criterion `json:"criteria"`

	Selectables      []Selectable      `json:"selectables"`
	CandidateDevices []CandidateDevice `json:"candidate_devices"`
	OntologyGaps     []OntologyGap     `json:"ontology_gaps"`

	// The import half of the answer. Reported beside the device half rather than
	// merged into it: the two are found by the same criteria and mean the same
	// thing semantically, but they are read completely differently — an import has
	// no stored series unless it was exported — and a single merged list would
	// invite a caller to treat them as interchangeable.
	ImportSelectables []ImportSelectable `json:"import_selectables"`
	ImportCandidates  []ImportCandidate  `json:"import_candidates"`

	// DeployableImportTypes are the import types that match and have no instance
	// in this answer. It is the difference between "this platform carries nothing
	// of that kind" and "nobody has deployed one yet", which an empty
	// ImportCandidates alone cannot express — and the second is a state a
	// developer can act on, through create_import_instance.
	DeployableImportTypes []DeployableImportType `json:"deployable_import_types"`

	// Candidates are the concrete series, ranked. Empty when ranking was skipped
	// or is unavailable, which Notes then says.
	Candidates []profiler.QuickProfile  `json:"candidates"`
	Skipped    []profiler.SkippedDevice `json:"skipped"`

	Reads Reads `json:"reads"`
	// CoverageWindow is the range the ranking measured coverage against. It is the
	// zero window when nothing was ranked, because there was no coverage to
	// measure — read it together with Candidates rather than on its own.
	CoverageWindow profiler.Window `json:"coverage_window"`
	DeviceLimit    int64           `json:"device_limit"`
	TotalDevices   int64           `json:"total_devices"`

	// Notes carries what a caller would otherwise have to infer from an empty
	// list: nothing matched, a cap was applied, ranking did not run. Silence
	// about a truncation reads as completeness.
	Notes []string `json:"notes"`
}

// Criterion is one FilterCriteria as sent, plus what it found.
type Criterion struct {
	FunctionID    string             `json:"function_id,omitempty"`
	AspectID      string             `json:"aspect_id,omitempty"`
	DeviceClassID string             `json:"device_class_id,omitempty"`
	Interaction   models.Interaction `json:"interaction,omitempty"`
	DeviceTypes   int                `json:"device_types"`

	// score orders the cross product so the strongest combinations survive the
	// cap. Not part of the document: it is the sum of two match scores and means
	// nothing on its own.
	score float64
}

func (c Criterion) filter() drmodel.FilterCriteria {
	return drmodel.FilterCriteria{
		FunctionId:    c.FunctionID,
		AspectId:      c.AspectID,
		DeviceClassId: c.DeviceClassID,
		Interaction:   c.Interaction,
	}
}

// Selectable is one addressable variable of one device type — the device-type
// level answer, before any device instance is involved.
type Selectable struct {
	DeviceTypeID string `json:"device_type_id"`
	// DeviceTypeName is filled in from the devices that were listed, which is the
	// only place it is available: a selectables answer carries ids and services, not
	// the type's own name. Empty for a device type this account can reach no device
	// of, and the reader falls back to the id.
	DeviceTypeName string `json:"device_type_name"`
	ServiceID      string `json:"service_id"`
	ServiceName    string `json:"service_name,omitempty"`
	Path           string `json:"path"`

	// CharacteristicID is canonical and never fabricated: null is a legitimate
	// answer and a made-up id would authorise a wrong server-side conversion
	// (§5.4.11).
	CharacteristicID *string             `json:"characteristic_id"`
	Unit             string              `json:"unit"`
	UnitSource       profiler.UnitSource `json:"unit_source"`
	Interaction      models.Interaction  `json:"interaction"`
	Type             models.Type         `json:"type,omitempty"`

	FunctionID string `json:"function_id,omitempty"`
	AspectID   string `json:"aspect_id,omitempty"`
	AspectName string `json:"aspect_name,omitempty"`

	// Queryable is false when the path exists in the ontology but is not a
	// readable scalar series — a service input, a JSONB list column, a
	// request-only service. Reporting it beats dropping it: a developer hunting
	// for that variable needs to know it was seen and why it is not on offer.
	Queryable bool   `json:"queryable"`
	Reason    string `json:"reason,omitempty"`

	OntologyCompleteness profiler.Completeness `json:"ontology_completeness"`
}

// CandidateDevice is one device instance carrying at least one matched device
// type. Permissions travel with it because the platform's verdict, not ODE's, is
// what decides whether the series can be read.
type CandidateDevice struct {
	DeviceID string `json:"device_id"`
	// Name is the platform's display name where it has one (devices.DisplayName),
	// because this list exists to be read rather than joined on.
	Name            string                 `json:"name"`
	ConnectionState models.ConnectionState `json:"connection_state"`
	DeviceTypeID    string                 `json:"device_type_id"`
	DeviceTypeName  string                 `json:"device_type_name"`
	Permissions     models.Permissions     `json:"permissions"`
	// Series is how many resolved series this device contributes, which is what
	// makes a device with forty matching paths distinguishable from one with a
	// single relevant column.
	Series int `json:"series"`
}

// OntologyGap implements D16: completeness is discovered at runtime, per device
// type, and reported rather than assumed.
//
// One entry per device type per distinct consequence, because that is what a
// caller acts on — "unit must be inferred" and "cannot be found by semantic
// selection" are different problems with different fixes, and merging them into
// one row would leave the reader unable to tell which paths had which.
type OntologyGap struct {
	DeviceTypeID string `json:"device_type_id"`
	// DeviceTypeName is empty when no listed device carries this type; see
	// Selectable.DeviceTypeName.
	DeviceTypeName string   `json:"device_type_name"`
	Missing        []string `json:"missing"`
	Consequence    string   `json:"consequence"`
	Paths          []string `json:"paths"`
}

// Reads is what the resolution asked of the platform.
//
// Values is the number that matters, and it is structurally zero: nothing in this
// package reads a value, and the ranking it delegates to is the read-free profile
// tier. A non-zero figure here would mean tier L0 had been broken (§3.2).
type Reads struct {
	Selectables  int `json:"selectables"`
	DeviceLists  int `json:"device_lists"`
	Availability int `json:"availability"`
	Usage        int `json:"usage"`
	Values       int `json:"values"`

	// ImportSelectables is one device-selection request per criteria combination
	// that could be applied to imports, which is not every combination — see
	// importFilter.
	ImportSelectables int `json:"import_selectables"`
	// ImportInstances is the import-deploy listing that answers whether each
	// shortlisted import is running, and ImportExports the analytics-serving listing
	// behind their history. Both are one read for the whole shortlist, because
	// neither service can filter by what is being asked. Neither reads a value; they
	// are counted for the reason the others are, so the cost of an answer is visible
	// in it.
	ImportInstances int `json:"import_instances"`
	ImportExports   int `json:"import_exports"`
	// ImportTypes is one import-repository request per criteria combination that
	// could be applied to imports. It is not folded into ImportSelectables: that
	// count is device-selection's, this one is a second service's, and a
	// deployment without an import-repository URL reads 0 here while the other
	// stays as it was.
	ImportTypes int `json:"import_types"`
}

// Resolve runs one semantic selection.
func (r *Resolver) Resolve(ctx context.Context, token string, req Request) (Result, error) {
	snap, err := r.ontology.Snapshot(ctx, token)
	if err != nil {
		return Result{}, err
	}
	index, err := r.index.Ontology(ctx, token)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Intent:               req.Intent,
		Terms:                []string{},
		UnmatchedTerms:       []string{},
		MatchedFunctions:     []ontology.FunctionMatch{},
		MatchedAspects:       []ontology.AspectMatch{},
		MatchedDeviceClasses: []ontology.DeviceClassMatch{},
		MatchElided:          []ontology.Elision{},
		Criteria:             []Criterion{},
		Selectables:          []Selectable{},
		CandidateDevices:     []CandidateDevice{},
		OntologyGaps:         []OntologyGap{},
		ImportSelectables:    []ImportSelectable{},
		ImportCandidates:     []ImportCandidate{},

		DeployableImportTypes: []DeployableImportType{},
		Candidates:            []profiler.QuickProfile{},
		Skipped:               []profiler.SkippedDevice{},
		Notes:                 []string{},
	}

	match := ontology.MatchIntent(snap, ontology.Intent{
		Text:               req.Intent,
		Limit:              req.MatchLimit,
		MinScore:           req.MinScore,
		IncludeControlling: req.IncludeControlling,
	})
	result.Terms = match.Terms
	result.UnmatchedTerms = match.UnmatchedTerms
	result.MatchedFunctions = match.Functions
	result.MatchedAspects = match.Aspects
	result.MatchedDeviceClasses = match.DeviceClasses
	result.MatchElided = match.Elided
	for _, elision := range match.Elided {
		// Carried into Notes as well as into the field. Notes is what this document
		// already uses to say that a cap was applied, and it is the part a model reads
		// as prose — a count in a field it does not look at is a truncation nobody sees,
		// which was the original defect rather than the truncation itself.
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%d %s matched the intent and the strongest %d are carried; raise match_limit or "+
				"pass explicit ids to see the rest",
			elision.Total, matchListName(elision.Field), elision.Shown))
	}

	// Explicit ids come first in each list: a caller who named an id asked for it,
	// and it should not be pushed out of the match limit by a lexical guess.
	if len(req.FunctionIDs) > 0 {
		explicit, unknown := ontology.ExplicitFunctions(snap, req.FunctionIDs)
		result.MatchedFunctions = mergeMatches(explicit, result.MatchedFunctions, functionID)
		result.Notes = appendUnknown(result.Notes, "function", unknown)
	}
	if len(req.AspectIDs) > 0 {
		explicit, unknown := ontology.ExplicitAspects(snap, req.AspectIDs)
		result.MatchedAspects = mergeMatches(explicit, result.MatchedAspects, aspectID)
		result.Notes = appendUnknown(result.Notes, "aspect", unknown)
	}
	if len(req.DeviceClassIDs) > 0 {
		explicit, unknown := ontology.ExplicitDeviceClasses(snap, req.DeviceClassIDs)
		result.MatchedDeviceClasses = mergeMatches(explicit, result.MatchedDeviceClasses, deviceClassID)
		result.Notes = appendUnknown(result.Notes, "device class", unknown)
	}

	interaction := req.Interaction
	switch interaction {
	case "":
		interaction = models.EVENT
	case InteractionAny:
		interaction = ""
	}

	criteria, dropped := buildCriteria(
		result.MatchedFunctions, result.MatchedAspects, req.DeviceClassIDs,
		interaction, r.opts.MaxCriteria)
	// The same slice, deliberately: querySelectables fills in each criterion's
	// device type count, and the report is meant to carry it.
	result.Criteria = criteria

	if len(result.MatchedDeviceClasses) > 0 && len(req.DeviceClassIDs) == 0 {
		// A device class match narrows by ANDing, and a wrong lexical one empties
		// the result with no error anywhere. §5.2 pairs function with aspect; a
		// device class is a narrowing the caller has to mean.
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%d device class(es) matched the intent but were not used as criteria; "+
				"pass device_class_ids to narrow by one deliberately",
			len(result.MatchedDeviceClasses)))
	}
	if dropped > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%d criteria combination(s) were dropped at the cap of %d, weakest match first; "+
				"narrow the intent or pass explicit ids to see them",
			dropped, r.opts.MaxCriteria))
	}
	if len(criteria) == 0 {
		result.Notes = append(result.Notes,
			"no function, aspect or device class resolved from this request, so nothing was queried: "+
				"an empty criteria list matches every device type on the platform and is refused")
		return result, nil
	}

	// The import half runs here, before the device half's early returns.
	//
	// That placement is the point rather than an accident: an intent the platform
	// can only satisfy from an import must still be answered when no device type
	// matches, and every `return result, nil` below is a case where the device
	// side found nothing. Resolving imports afterwards would silently drop them in
	// exactly the situation where they are the whole answer.
	if err := r.addImports(ctx, token, &result, criteria, req, index, snap); err != nil {
		return Result{}, err
	}

	matched, err := r.querySelectables(ctx, token, criteria)
	if err != nil {
		return Result{}, err
	}
	result.Reads.Selectables = len(criteria)

	for _, deviceType := range sortedDeviceTypes(matched) {
		result.Selectables = append(result.Selectables, deviceType.selectables(index)...)
	}
	result.OntologyGaps = ontologyGaps(result.Selectables)

	if len(result.Selectables) == 0 {
		// "no device type", not "nothing": the import half above may well have
		// matched, and this used to be the last word on the resolution.
		result.Notes = append(result.Notes,
			"the criteria matched no device type, so no device on this platform is described as carrying this")
		return result, nil
	}

	deviceTypeIDs := make([]string, 0, len(matched))
	for id := range matched {
		deviceTypeIDs = append(deviceTypeIDs, id)
	}
	sort.Strings(deviceTypeIDs)

	limit := req.DeviceLimit
	if limit <= 0 {
		limit = r.opts.DeviceLimit
	}
	result.DeviceLimit = limit

	listed, err := r.devices.List(token, drmodel.ExtendedDeviceListOptions{
		DeviceTypeIds: deviceTypeIDs,
		Limit:         limit,
		// models.Execute, not models.Read: this listing offers series to read, and
		// Execute is what governs reading a device's data (§5.1). Listing under
		// Read would offer series the caller cannot read and fail at query time.
		Permission: models.Execute,
		// The variable enumeration walks the service outputs, so the device type
		// has to arrive with the device.
		FullDt: true,
	})
	if err != nil {
		return Result{}, err
	}
	result.Reads.DeviceLists = 1
	result.TotalDevices = listed.Total

	// The device type names arrive with the devices, so this is the first point at
	// which the ontology half of the answer can be labelled. It runs before the
	// early return below because a resolution with no reachable device still reports
	// its selectables and gaps — they just stay addressed by id.
	nameDeviceTypes(&result, listed.Devices)

	if len(listed.Devices) == 0 {
		result.Notes = append(result.Notes,
			"the ontology describes these series, but this account has execute permission on no device of a matching type")
		return result, nil
	}

	selected := selectedPaths(result.Selectables)
	result.CandidateDevices = candidateDevices(listed.Devices, selected)

	switch {
	case req.SkipRanking:
		result.Notes = append(result.Notes,
			"ranking was skipped on request: the candidates are the ontology's, unordered by availability")
	case r.ranker == nil:
		result.Notes = append(result.Notes,
			"ranking is unavailable because no timescale-wrapper is configured; "+
				"the ontology resolution stands, the availability-based order does not")
	default:
		quick, err := r.ranker.QuickProfiles(ctx, token, profiler.QuickRequest{
			Devices: listed.Devices,
			Window:  req.Window,
			// Selected variables that cannot be read as a series are kept and ranked
			// last rather than hidden: the developer asked for that variable, and
			// "it exists but is a JSONB list column" is the answer.
			IncludeUnqueryable: true,
		})
		if err != nil {
			return Result{}, err
		}
		result.Candidates = keepSelected(quick.Candidates, listed.Devices, selected)
		result.Skipped = quick.Skipped
		result.CoverageWindow = quick.Window
		result.Reads.Availability = quick.Reads.Availability
		result.Reads.Usage = quick.Reads.Usage
		result.Reads.Values = quick.Reads.Values
		result.CandidateDevices = seriesCounts(result.CandidateDevices, result.Candidates)
	}

	slog.DebugContext(ctx, "semantic selection resolved",
		"functions", len(result.MatchedFunctions), "aspects", len(result.MatchedAspects),
		"criteria", len(criteria), "device_types", len(matched),
		"selectables", len(result.Selectables), "devices", len(listed.Devices),
		"import_selectables", len(result.ImportSelectables),
		"import_candidates", len(result.ImportCandidates),
		"candidates", len(result.Candidates), "gaps", len(result.OntologyGaps),
		"value_reads", result.Reads.Values)

	return result, nil
}

// querySelectables runs one request per criteria combination and merges the
// answers, recording per criterion how many device types it found.
//
// Concurrent because the requests are independent and the platform is the slow
// part; bounded because a wide intent produces a dozen of them. A single failure
// fails the whole resolution: a result silently missing one function's device
// types is worse than an error, and there is no field on this document that could
// honestly say "partially answered".
func (r *Resolver) querySelectables(ctx context.Context, token string, criteria []Criterion) (map[string]*deviceTypeMatch, error) {
	type outcome struct {
		index int
		found []drmodel.DeviceTypeSelectable
		err   error
	}

	gate := make(chan struct{}, r.opts.Concurrency)
	results := make(chan outcome, len(criteria))
	wg := sync.WaitGroup{}

	for i, criterion := range criteria {
		wg.Add(1)
		go func(i int, criterion Criterion) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()

			found, err := r.ontology.DeviceTypeSelectables(ctx, token,
				[]drmodel.FilterCriteria{criterion.filter()}, ontology.SelectableOptions{})
			results <- outcome{index: i, found: found, err: err}
		}(i, criterion)
	}
	wg.Wait()
	close(results)

	merged := map[string]*deviceTypeMatch{}
	var firstErr error
	for out := range results {
		if out.err != nil {
			if firstErr == nil {
				firstErr = out.err
			}
			continue
		}
		criteria[out.index].DeviceTypes = len(out.found)
		for _, selectable := range out.found {
			entry, known := merged[selectable.DeviceTypeId]
			if !known {
				entry = newDeviceTypeMatch(selectable.DeviceTypeId)
				merged[selectable.DeviceTypeId] = entry
			}
			entry.merge(selectable)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return merged, nil
}

// selectedPaths is the set of variables the resolution actually selected, keyed
// by device type. It is what narrows a device's whole variable list down to the
// ones the intent asked about.
func selectedPaths(selectables []Selectable) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, s := range selectables {
		paths, known := out[s.DeviceTypeID]
		if !known {
			paths = map[string]bool{}
			out[s.DeviceTypeID] = paths
		}
		paths[s.ServiceID+"|"+s.Path] = true
	}
	return out
}

// keepSelected drops the candidates that belong to a device's other variables.
//
// QuickProfiles enumerates every variable of every device it is given, which is
// right for browsing and wrong here: this operation answers an intent, and a
// device's unrelated forty columns are not part of the answer. Filtering after
// the fact costs nothing on the wire — availability and usage are per device, not
// per variable — and it keeps the ranking identical to the one the candidate list
// shows, rather than a second ordering computed elsewhere.
func keepSelected(candidates []profiler.QuickProfile, listed []models.ExtendedDevice, selected map[string]map[string]bool) []profiler.QuickProfile {
	deviceTypeOf := map[string]string{}
	for _, device := range listed {
		deviceTypeOf[device.Id] = device.DeviceTypeId
	}

	out := []profiler.QuickProfile{}
	for _, candidate := range candidates {
		paths := selected[deviceTypeOf[candidate.SeriesRef.DeviceID]]
		if paths[candidate.SeriesRef.ServiceID+"|"+candidate.SeriesRef.VariablePath] {
			out = append(out, candidate)
		}
	}
	return out
}

// nameDeviceTypes labels the device-type-level half of the answer from the devices
// that were listed.
//
// Doing it here rather than asking the repository for the device types is a
// deliberate trade: naming N types would be another platform call returning every
// service and content variable of each, to display one string. The types a
// developer can actually use are exactly the ones with a reachable device, and
// those are named.
func nameDeviceTypes(result *Result, listed []models.ExtendedDevice) {
	names := make(map[string]string, len(listed))
	for _, device := range listed {
		if name := devices.TypeName(device); name != "" {
			names[device.DeviceTypeId] = name
		}
	}
	for i := range result.Selectables {
		result.Selectables[i].DeviceTypeName = names[result.Selectables[i].DeviceTypeID]
	}
	for i := range result.OntologyGaps {
		result.OntologyGaps[i].DeviceTypeName = names[result.OntologyGaps[i].DeviceTypeID]
	}
}

func candidateDevices(listed []models.ExtendedDevice, selected map[string]map[string]bool) []CandidateDevice {
	out := make([]CandidateDevice, 0, len(listed))
	for _, device := range listed {
		paths, matching := selected[device.DeviceTypeId]
		if !matching {
			// The device repository matched this device to one of the requested
			// device type ids, so this only happens if the id was modified upstream.
			// Reporting a device none of whose variables were selected would be
			// noise.
			continue
		}
		out = append(out, CandidateDevice{
			DeviceID:        device.Id,
			Name:            devices.DisplayName(device),
			ConnectionState: device.ConnectionState,
			DeviceTypeID:    device.DeviceTypeId,
			DeviceTypeName:  devices.TypeName(device),
			Permissions:     device.Permissions,
			// From the selection, so the count is right whether or not the ranking
			// runs. Zero here would say this device contributes nothing, which is a
			// different claim from "nothing was ranked".
			Series: len(paths),
		})
	}
	return out
}

// seriesCounts replaces the per-device count with what the ranking actually
// produced.
//
// The two differ when the profiler skipped a device — an unresolved device type,
// no execute permission — and then the ranked count is the honest one: the series
// exist in the ontology but this device yielded none.
func seriesCounts(candidateDevices []CandidateDevice, candidates []profiler.QuickProfile) []CandidateDevice {
	counts := map[string]int{}
	for _, candidate := range candidates {
		counts[candidate.SeriesRef.DeviceID]++
	}
	for i := range candidateDevices {
		candidateDevices[i].Series = counts[candidateDevices[i].DeviceID]
	}
	return candidateDevices
}

// matchListName is an ontology elision's field as a sentence wants it:
// "matched_functions" is a JSON key, "functions" is what a note reads with.
func matchListName(field string) string {
	switch field {
	case ontology.FieldMatchedFunctions:
		return "functions"
	case ontology.FieldMatchedAspects:
		return "aspects"
	case ontology.FieldMatchedDeviceClasses:
		return "device classes"
	default:
		return field
	}
}

func appendUnknown(notes []string, kind string, unknown []string) []string {
	if len(unknown) == 0 {
		return notes
	}
	return append(notes, fmt.Sprintf(
		"%s id(s) %v are not in the ontology snapshot and were queried anyway: "+
			"the snapshot can be older than the platform, so an unknown id is reported rather than refused",
		kind, unknown))
}

// mergeMatches concatenates two match lists without the duplicate a lexical match
// may have produced for an entity the caller also named explicitly, keeping the
// first list's order.
//
// It builds a new slice rather than appending onto `first`: appending would write
// into the caller's backing array whenever it has spare capacity, which is the kind
// of aliasing that stays invisible until one of these lists is reused.
func mergeMatches[T any](first, second []T, id func(T) string) []T {
	seen := make(map[string]bool, len(first)+len(second))
	out := make([]T, 0, len(first)+len(second))
	for _, list := range [][]T{first, second} {
		for _, match := range list {
			if seen[id(match)] {
				continue
			}
			seen[id(match)] = true
			out = append(out, match)
		}
	}
	return out
}

func functionID(m ontology.FunctionMatch) string       { return m.Id }
func aspectID(m ontology.AspectMatch) string           { return m.Id }
func deviceClassID(m ontology.DeviceClassMatch) string { return m.Id }
