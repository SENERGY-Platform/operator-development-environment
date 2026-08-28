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

package selection

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/imports"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// The import half of a resolution.
//
// It is part of resolve_semantic_selection rather than a tool of its own, and
// that is the design decision worth defending. The developer who asks for "PV
// generation" has no business knowing whether this platform happens to deliver
// that signal from a device or from an import; both are described by the same
// content variables and found by the same criteria. Two separate tools would let
// a model search one kind, get a plausible answer, and never learn the other kind
// existed — a coverage gap that never announces itself, which is worse than an
// error.
//
// What the two halves cannot share is the read side. A device candidate is ranked
// on availability and volume; an import has neither unless somebody exported it
// (see imports.History). So imports are resolved and reported, never folded into
// the ranked Candidates list — a ranking that mixed the two would be comparing a
// measured span against nothing at all.

// Imports is the slice of pkg/imports this package needs.
//
// Optional in the way Ranker is: a deployment with no device-selection URL
// resolves devices exactly as before and says in Notes that the import half was
// not searched, rather than failing a resolution that is three quarters useful.
type Imports interface {
	QueryImports(ctx context.Context, token string, criteria []drmodel.FilterCriteria) ([]imports.Selectable, error)
	List(ctx context.Context, token string, opts imports.InstanceListOptions) (imports.ListResult, error)
	// ListTypes is the catalogue half, and it answers a question QueryImports
	// structurally cannot: discovery joins every matching type to its instances, so
	// a type with no instance is absent from a selectables answer — which is the
	// state of every type somebody is about to deploy the first instance of.
	ListTypes(ctx context.Context, token string, opts imports.TypeListOptions) (imports.TypeListResult, error)
	// Histories rather than History: analytics-serving cannot filter by import, so
	// one answer per candidate would re-read the same export listing once per
	// candidate. A resolution asks once for the whole shortlist.
	Histories(ctx context.Context, token string, instanceIDs []string) map[string]imports.History
}

// ImportSelectable is one addressable variable of one import instance.
//
// Deliberately shaped like Selectable, field for field where a field means the
// same thing, so a caller reading both lists is reading one vocabulary. The
// differences are the ones that are real: there is no service, because an import
// publishes one topic rather than several services; there is no interaction,
// because an import path is always an event; and Queryable is not here, because
// whether values can be read is a property of the instance's export rather than
// of the path — ImportCandidate.History carries it.
type ImportSelectable struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	// KafkaTopic is what an operator input's topicName must be. Carried at the
	// selectable level because it is what makes this row actionable: with the path
	// beside it, these two fields are the whole wiring.
	KafkaTopic     string `json:"kafka_topic"`
	ImportTypeID   string `json:"import_type_id"`
	ImportTypeName string `json:"import_type_name"`

	// Path is message-relative — `value.temperature` — which is the form an
	// operator mapping takes. See imports.MessagePath.
	Path string `json:"path"`

	// CharacteristicID is canonical and never fabricated, for the reason
	// Selectable.CharacteristicID is not: it decides the unit and the declared
	// range, and inventing one would authorise a wrong conversion.
	CharacteristicID *string             `json:"characteristic_id"`
	Unit             string              `json:"unit"`
	UnitSource       profiler.UnitSource `json:"unit_source"`
	Type             models.Type         `json:"type,omitempty"`

	FunctionID string `json:"function_id,omitempty"`
	AspectID   string `json:"aspect_id,omitempty"`
	AspectName string `json:"aspect_name,omitempty"`

	OntologyCompleteness profiler.Completeness `json:"ontology_completeness"`
}

// ImportCandidate is one import instance that contributes at least one selected
// variable — the import-side counterpart of CandidateDevice.
type ImportCandidate struct {
	InstanceID     string `json:"instance_id"`
	Name           string `json:"name"`
	KafkaTopic     string `json:"kafka_topic"`
	ImportTypeID   string `json:"import_type_id"`
	ImportTypeName string `json:"import_type_name"`

	// Running is three-valued for the reason imports.Running is: discovery carries
	// no status at all, so "not running" and "status did not arrive" are different
	// claims and only one of them is actionable.
	Running      bool   `json:"running"`
	RunningKnown bool   `json:"running_known"`
	StatusNote   string `json:"status_note,omitempty"`

	// History says whether any of this import's past exists in timescale. This is
	// the field that stops an import being read as a device: a live_only import can
	// feed a running operator and cannot be profiled or backtested at all.
	History imports.History `json:"history"`

	// Series is how many resolved variables this instance contributes.
	Series int `json:"series"`
}

// DeployableImportType is an import type that matches the criteria and has no
// instance in this answer.
//
// It exists because an empty import half has two causes that look identical, and
// only one of them is a dead end: the platform describes nothing of this kind at
// all, or it describes it and nobody has deployed an import for it yet. The
// second is actionable — create_import_instance turns this row into an import —
// and reporting it is the difference between "this platform does not have that"
// and "this platform does not have that *running*".
//
// Nothing here is a property of an instance, because there is none: no Kafka
// topic, no container status, no history. What a developer gets instead is what
// deploying one would cost them, which is the configuration.
type DeployableImportType struct {
	ImportTypeID string `json:"import_type_id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`

	// MatchingVariables are the payload leaves that carry the wanted function and
	// aspect. Empty is a real answer rather than a defect, and Note says so: the
	// criteria index upstream is flattened per import type, so a type matches when
	// its variables carry the criteria *between* them, and no single variable need
	// carry both.
	MatchingVariables []ImportTypeVariable `json:"matching_variables"`

	// RequiredConfigs are declared with no usable default, so deploying means
	// deciding them.
	RequiredConfigs []string `json:"required_configs"`
	// BlockingCredentials are the configs that make this type undeployable from a
	// chat at all — credential-shaped, with no default. The import is created in
	// the platform's own dialog instead.
	BlockingCredentials []string `json:"blocking_credentials,omitempty"`
	// Deployable is false exactly when BlockingCredentials is not empty. Stated
	// rather than left to be inferred from an empty list, because the inference
	// runs the wrong way for a reader who does not know the credential rule.
	Deployable bool   `json:"deployable"`
	Note       string `json:"note,omitempty"`
}

// ImportTypeVariable is one variable an import type would publish, described the
// way a resolved one is.
//
// The unit and the completeness come from the same two profiler functions the
// device and import selectables use, so "unit_source: characteristic" means one
// thing across all three lists rather than three.
type ImportTypeVariable struct {
	Path             string              `json:"path"`
	CharacteristicID *string             `json:"characteristic_id"`
	Unit             string              `json:"unit"`
	UnitSource       profiler.UnitSource `json:"unit_source"`
	Type             models.Type         `json:"type,omitempty"`

	FunctionID string `json:"function_id,omitempty"`
	AspectID   string `json:"aspect_id,omitempty"`
	AspectName string `json:"aspect_name,omitempty"`

	OntologyCompleteness profiler.Completeness `json:"ontology_completeness"`
}

// resolveImports runs the import half against the criteria the device half
// already built.
//
// The same criteria, one request per combination, for a reason that is not just
// symmetry: device-selection ANDs a criteria list for devices but ORs it for
// imports, so a multi-criterion request would mean different things to the two
// halves of one answer. Sending one at a time is the only shape under which the
// device and import lists of a single resolution are comparable — which turns the
// cross product in buildCriteria from a workaround for the device repository into
// a requirement of the operation.
func (r *Resolver) resolveImports(ctx context.Context, token string, criteria []Criterion) ([]imports.Selectable, error) {
	type outcome struct {
		found []imports.Selectable
		err   error
	}

	gate := make(chan struct{}, r.opts.Concurrency)
	results := make(chan outcome, len(criteria))
	wg := sync.WaitGroup{}

	for _, criterion := range criteria {
		if criterion.DeviceClassID != "" {
			// An import type carries no device class, so this criterion cannot be
			// expressed at all. Sending it with the field dropped would silently widen
			// the query — returning imports the caller's narrowing was meant to exclude —
			// so it is skipped here and reported in Notes instead.
			continue
		}
		wg.Add(1)
		go func(criterion Criterion) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()

			found, err := r.imports.QueryImports(ctx, token, []drmodel.FilterCriteria{criterion.importFilter()})
			results <- outcome{found: found, err: err}
		}(criterion)
	}
	wg.Wait()
	close(results)

	// Merged by instance and path: the same variable comes back from every
	// criterion that matched it, exactly as a device type does.
	seen := map[string]bool{}
	merged := []imports.Selectable{}
	var firstErr error
	for out := range results {
		if out.err != nil {
			if firstErr == nil {
				firstErr = out.err
			}
			continue
		}
		for _, selectable := range out.found {
			key := selectable.InstanceID + "|" + selectable.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, selectable)
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].InstanceName != merged[j].InstanceName {
			return merged[i].InstanceName < merged[j].InstanceName
		}
		if merged[i].InstanceID != merged[j].InstanceID {
			return merged[i].InstanceID < merged[j].InstanceID
		}
		return merged[i].Path < merged[j].Path
	})
	return merged, nil
}

// importFilter is the criterion as the import half sends it.
//
// The interaction is dropped, not translated. Every import path is an event by
// construction, so an EVENT filter is satisfied trivially and a REQUEST filter
// would be asking for something that cannot exist — and device-selection passes
// the field to import-repository, which has no interaction dimension to match it
// against. Dropping it here keeps the request honest; the caller that asked for
// request-only interaction is told in Notes why imports are absent.
func (c Criterion) importFilter() drmodel.FilterCriteria {
	return drmodel.FilterCriteria{
		FunctionId: c.FunctionID,
		AspectId:   c.AspectID,
	}
}

// importSelectables resolves the raw platform view into the reported one, adding
// the unit and the completeness from the same ontology index the device half
// uses.
//
// Building a profiler.Variable to do it is not a detour. That is the type
// ResolveUnits and VariableCompleteness are written against, and running an
// import variable through the same two functions is what makes "unit_source:
// characteristic" mean the same thing on both lists. A parallel implementation
// would drift, and the first symptom would be an import reporting a unit a device
// with the same characteristic did not.
func importSelectables(found []imports.Selectable, index *profiler.OntologyIndex, aspectNames map[string]string) []ImportSelectable {
	out := make([]ImportSelectable, 0, len(found))
	for _, selectable := range found {
		variable := profiler.Variable{
			Path: selectable.Path,
			Name: lastSegment(selectable.Path),
			Type: models.Type(selectable.Type),
			// EVENT rather than empty: an import publishes to Kafka, which is exactly
			// what the interaction distinction is about, and leaving it empty would make
			// Streamed() false and mark every import variable unreadable.
			Interaction: models.EVENT,
			FunctionID:  selectable.FunctionID,
			AspectID:    selectable.AspectID,
			Queryable:   true,
		}
		if selectable.CharacteristicID != nil {
			variable.CharacteristicID = *selectable.CharacteristicID
		}

		semantics := profiler.ResolveUnits(variable, index, profiler.Provenance{})
		out = append(out, ImportSelectable{
			InstanceID:     selectable.InstanceID,
			InstanceName:   selectable.InstanceName,
			KafkaTopic:     selectable.KafkaTopic,
			ImportTypeID:   selectable.ImportTypeID,
			ImportTypeName: selectable.ImportTypeName,
			Path:           selectable.Path,
			// From the resolution rather than from the raw option, so that a
			// characteristic the ontology index knows about and the platform answer left
			// blank is reported the same way it is for a device.
			CharacteristicID:     semantics.CharacteristicID,
			Unit:                 semantics.Unit,
			UnitSource:           semantics.UnitSource,
			Type:                 variable.Type,
			FunctionID:           selectable.FunctionID,
			AspectID:             selectable.AspectID,
			AspectName:           aspectNames[selectable.AspectID],
			OntologyCompleteness: profiler.VariableCompleteness(variable, index),
		})
	}
	return out
}

// importCandidates groups the selected variables by instance and answers the two
// questions a selectable cannot: is this import running, and is any of its past
// stored.
//
// Both need calls discovery does not make. The status comes from import-deploy,
// which is why one listing by id is issued here; the history comes from
// analytics-serving. Neither failure is fatal — an import whose status did not
// arrive is still a wireable input, and saying so beats refusing the whole
// resolution.
func (r *Resolver) importCandidates(ctx context.Context, token string, selectables []ImportSelectable) []ImportCandidate {
	order := []string{}
	byInstance := map[string]*ImportCandidate{}
	for _, selectable := range selectables {
		candidate, known := byInstance[selectable.InstanceID]
		if !known {
			candidate = &ImportCandidate{
				InstanceID:     selectable.InstanceID,
				Name:           selectable.InstanceName,
				KafkaTopic:     selectable.KafkaTopic,
				ImportTypeID:   selectable.ImportTypeID,
				ImportTypeName: selectable.ImportTypeName,
			}
			byInstance[selectable.InstanceID] = candidate
			order = append(order, selectable.InstanceID)
		}
		candidate.Series++
	}
	if len(order) == 0 {
		return []ImportCandidate{}
	}

	// One listing for every instance in the shortlist. import-deploy accepts an id
	// list, so this is one call rather than one per instance — and a listing
	// restricted to ids is the only place ODE asks import-deploy for anything during
	// a resolution.
	statuses := map[string]idmodel.Instance{}
	listed, err := r.imports.List(ctx, token, imports.InstanceListOptions{
		IDs:   order,
		Limit: int64(len(order)),
	})
	if err == nil {
		for _, instance := range listed.Instances {
			statuses[instance.Id] = instance
		}
	}

	histories := r.imports.Histories(ctx, token, order)

	out := make([]ImportCandidate, 0, len(order))
	for _, id := range order {
		candidate := *byInstance[id]
		if instance, known := statuses[id]; known {
			candidate.Running, candidate.RunningKnown = imports.Running(instance)
			candidate.StatusNote = imports.TransitionMessage(instance)
		} else if err != nil {
			candidate.StatusNote = "import-deploy could not be read, so whether this import is running is unknown"
		} else {
			// Listed by id and absent from the answer. The selectable came from
			// device-selection, which reads the same service, so this is a race with a
			// deletion rather than a permission difference.
			candidate.StatusNote = "this import was not returned by import-deploy; it may have just been deleted"
		}
		candidate.History = histories[id]
		out = append(out, candidate)
	}
	return out
}

// typeListLimit is what one catalogue query reads. device-selection sends 1000
// for the same query against the same endpoint; matching it means ODE does not
// quietly see fewer import types than the platform's own selectables answer was
// built from.
const typeListLimit = 1000

// catalogue is what the type half of this answer managed to establish, so the
// notes can tell "there is no such import type" apart from "there is none
// running" and from "this was not looked up".
type catalogue struct {
	// read is true only when every criterion was answered. A partial catalogue is
	// treated as unread: reporting the types one criterion found while another
	// failed would understate the platform in a way nothing on the document says.
	read bool
	// matched counts the types matching the criteria, deployed or not.
	matched int
	// deployable counts those with no instance in this answer.
	deployable int
	// failure is why the catalogue was not read, empty when it was.
	failure string
}

// deployableImportTypes asks import-repository which import types match, which
// is the one question device-selection cannot be asked.
//
// One request per criterion and a union of the answers, for the reason
// resolveImports sends one at a time: upstream ANDs a criteria list, so a
// multi-criterion request asks for a type carrying all of it. The aspect subtree
// is expanded here rather than upstream, because import-repository matches
// aspect ids literally — the asymmetry device-selection absorbs for the
// selectables half and nobody absorbs for this one.
//
// deployed is the set of import type ids this resolution already reported as
// running instances. Those are dropped from the answer and kept in the count:
// they are already in import_candidates, where they carry a topic and a status
// that this list has nothing to say about.
func (r *Resolver) deployableImportTypes(
	ctx context.Context,
	token string,
	criteria []Criterion,
	snap *ontology.Snapshot,
	deployed map[string]bool,
	index *profiler.OntologyIndex,
) ([]DeployableImportType, catalogue, int) {
	filters := []imports.TypeCriterion{}
	for _, criterion := range criteria {
		if criterion.DeviceClassID != "" {
			// Same reason resolveImports skips it: an import type carries no device
			// class, and sending the criterion with the field dropped would widen the
			// query rather than express it.
			continue
		}
		filter := imports.TypeCriterion{FunctionID: criterion.FunctionID}
		if criterion.AspectID != "" {
			filter.AspectIDs = ontology.AspectSubtreeIDs(snap.AspectNodes, criterion.AspectID)
			if len(filter.AspectIDs) == 0 {
				// An aspect the snapshot does not carry. Sending the bare id asks a
				// narrower question than the caller meant, which is still the question
				// they asked; dropping it would ask a wider one they did not.
				filter.AspectIDs = []string{criterion.AspectID}
			}
		}
		if filter.FunctionID == "" && len(filter.AspectIDs) == 0 {
			// Nothing to narrow on. Upstream reads an empty criterion as "any type with
			// any criteria at all", which is every import type on the platform.
			continue
		}
		filters = append(filters, filter)
	}
	if len(filters) == 0 {
		return []DeployableImportType{}, catalogue{read: true}, 0
	}

	type outcome struct {
		found []dsmodel.ImportType
		err   error
	}
	gate := make(chan struct{}, r.opts.Concurrency)
	results := make(chan outcome, len(filters))
	wg := sync.WaitGroup{}
	for _, filter := range filters {
		wg.Add(1)
		go func(filter imports.TypeCriterion) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()

			listed, err := r.imports.ListTypes(ctx, token, imports.TypeListOptions{
				Criteria: []imports.TypeCriterion{filter},
				Limit:    typeListLimit,
			})
			results <- outcome{found: listed.Types, err: err}
		}(filter)
	}
	wg.Wait()
	close(results)

	seen := map[string]bool{}
	matched := []dsmodel.ImportType{}
	var firstErr error
	for out := range results {
		if out.err != nil {
			if firstErr == nil {
				firstErr = out.err
			}
			continue
		}
		for _, importType := range out.found {
			if seen[importType.Id] {
				continue
			}
			seen[importType.Id] = true
			matched = append(matched, importType)
		}
	}
	if firstErr != nil {
		// Degraded rather than fatal, unlike resolveImports. That one is the answer
		// itself — a resolution missing every import of a matching type has no field
		// that could honestly say so — while this one says what could additionally be
		// deployed, and an answer without it is complete about what exists.
		//
		// An invalid request here means one thing: no import-repository is
		// configured, so there is no catalogue to read at all. That is permanent for
		// this deployment rather than a service that did not answer, and the two get
		// different notes — quoting a configuration error as an outage would send
		// somebody to check a service that was never called.
		if errors.Is(firstErr, imports.ErrInvalidRequest) {
			return []DeployableImportType{}, catalogue{}, len(filters)
		}
		return []DeployableImportType{}, catalogue{failure: firstErr.Error()}, len(filters)
	}

	out := make([]DeployableImportType, 0, len(matched))
	names := aspectNames(snap)
	for _, importType := range matched {
		if deployed[importType.Id] {
			continue
		}
		out = append(out, deployableImportType(importType, filters, index, names))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ImportTypeID < out[j].ImportTypeID
	})
	return out, catalogue{read: true, matched: len(matched), deployable: len(out)}, len(filters)
}

func deployableImportType(
	importType dsmodel.ImportType,
	filters []imports.TypeCriterion,
	index *profiler.OntologyIndex,
	aspects map[string]string,
) DeployableImportType {
	blocking := imports.BlockingCredentials(importType)
	out := DeployableImportType{
		ImportTypeID:        importType.Id,
		Name:                importType.Name,
		Description:         importType.Description,
		MatchingVariables:   importTypeVariables(imports.MatchingVariables(importType, filters), index, aspects),
		RequiredConfigs:     imports.RequiredConfigs(importType),
		BlockingCredentials: blocking,
		Deployable:          len(blocking) == 0,
	}
	switch {
	case len(blocking) > 0:
		out.Note = "this import type cannot be deployed from a chat: " +
			strings.Join(blocking, ", ") + " reads as a credential and the type declares no default, " +
			"so the developer creates this import in the platform's own import dialog"
	case len(out.MatchingVariables) == 0:
		// The type matched and none of its variables did. Upstream's criteria index
		// is flattened per import type, so the function and the aspect can be carried
		// by two different variables and the type still matches — see asymmetry 3 in
		// imports-as-operator-inputs.md.
		out.Note = "this import type matches, but no single variable of it carries both the " +
			"function and the aspect: read it with get_import_type_metadata before deploying it"
	case len(out.RequiredConfigs) > 0:
		out.Note = "deploying this needs " + strings.Join(out.RequiredConfigs, ", ") +
			"; the import type declares no usable default for them"
	}
	return out
}

func importTypeVariables(found []imports.TypeVariable, index *profiler.OntologyIndex, aspects map[string]string) []ImportTypeVariable {
	out := make([]ImportTypeVariable, 0, len(found))
	for _, variable := range found {
		resolved := profiler.Variable{
			Path: variable.Path,
			Name: lastSegment(variable.Path),
			Type: models.Type(variable.Type),
			// EVENT for the reason importSelectables sets it: an import publishes to a
			// topic, and an empty interaction would mark every variable unreadable.
			Interaction: models.EVENT,
			FunctionID:  variable.FunctionID,
			AspectID:    variable.AspectID,
			Queryable:   true,
		}
		if variable.CharacteristicID != nil {
			resolved.CharacteristicID = *variable.CharacteristicID
		}
		semantics := profiler.ResolveUnits(resolved, index, profiler.Provenance{})
		out = append(out, ImportTypeVariable{
			Path:                 variable.Path,
			CharacteristicID:     semantics.CharacteristicID,
			Unit:                 semantics.Unit,
			UnitSource:           semantics.UnitSource,
			Type:                 resolved.Type,
			FunctionID:           variable.FunctionID,
			AspectID:             variable.AspectID,
			AspectName:           aspects[variable.AspectID],
			OntologyCompleteness: profiler.VariableCompleteness(resolved, index),
		})
	}
	return out
}

// importNotes says what the import half of this answer does and does not cover.
//
// Every branch here exists because silence would read as completeness. A
// resolution that searched no imports and one that searched and found none look
// identical in an empty list, and only the second means "the platform has none".
func importNotes(notes []string, configured bool, criteria []Criterion, selectables []ImportSelectable, interaction models.Interaction, cat catalogue) []string {
	if !configured {
		return append(notes,
			"imports were not searched because no device-selection URL is configured: "+
				"an operator can take an import as an input just as it can a device, so this "+
				"answer may be missing a whole class of candidate")
	}

	classConstrained := 0
	for _, criterion := range criteria {
		if criterion.DeviceClassID != "" {
			classConstrained++
		}
	}
	if classConstrained == len(criteria) && len(criteria) > 0 {
		return append(notes,
			"no import was searched: every criterion narrows by device class, and an import "+
				"type has no device class to narrow on. Drop device_class_ids to include imports")
	}
	if classConstrained > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d of %d criteria combination(s) narrow by device class and were not applied to imports, "+
				"which carry none; the import half of this answer comes from the remaining %d",
			classConstrained, len(criteria), len(criteria)-classConstrained))
	}
	if interaction == models.REQUEST {
		notes = append(notes,
			"imports were searched but cannot match a request-only interaction: an import "+
				"publishes to a topic, so every import variable is an event")
	}

	switch {
	case cat.failure != "":
		notes = append(notes,
			"the import type catalogue could not be read ("+cat.failure+"), so whether an import "+
				"type exists that could be deployed for this is unknown; only imports that are "+
				"already deployed are in this answer")
	case !cat.read:
		// No import-repository is configured, so the type catalogue was never
		// readable. The two causes of an empty import half stay indistinguishable,
		// and saying so is the honest answer.
		if len(selectables) == 0 {
			notes = append(notes,
				"no import type on this platform declares these functions and aspects, "+
					"or none has a running instance this account may read; without an "+
					"import_repo_url ODE cannot tell the two apart, and cannot name an "+
					"import type to deploy")
		}
	case len(selectables) == 0 && cat.matched == 0:
		notes = append(notes,
			"no import type on this platform declares these functions and aspects: there is "+
				"nothing deployed for this and nothing to deploy")
	case len(selectables) == 0:
		notes = append(notes, fmt.Sprintf(
			"no import instance this account may read carries this, but %d import type(s) do "+
				"and have none deployed: see deployable_import_types, which create_import_instance "+
				"takes an id from", cat.deployable))
	case cat.deployable > 0:
		notes = append(notes, fmt.Sprintf(
			"%d further import type(s) match and have no instance this account may read; "+
				"see deployable_import_types", cat.deployable))
	}
	return notes
}

// addImports resolves the import half onto the result, or explains its absence.
//
// A failure here fails the whole resolution, matching querySelectables: a result
// silently missing every import of a matching type is worse than an error,
// because there is no field on this document that could honestly say "the device
// half is complete and the import half is not". The two per-candidate lookups
// underneath — status and history — are the exception and degrade in place, since
// each has a field that says it did not arrive.
func (r *Resolver) addImports(
	ctx context.Context,
	token string,
	result *Result,
	criteria []Criterion,
	req Request,
	index *profiler.OntologyIndex,
	snap *ontology.Snapshot,
) error {
	if req.SkipImports {
		result.Notes = append(result.Notes,
			"the import half was skipped on request: only devices were searched")
		return nil
	}
	if r.imports == nil {
		result.Notes = importNotes(result.Notes, false, criteria, nil, req.Interaction, catalogue{})
		return nil
	}

	found, err := r.resolveImports(ctx, token, criteria)
	if err != nil {
		return err
	}
	for _, criterion := range criteria {
		if criterion.DeviceClassID == "" {
			result.Reads.ImportSelectables++
		}
	}

	result.ImportSelectables = importSelectables(found, index, aspectNames(snap))
	result.ImportCandidates = r.importCandidates(ctx, token, result.ImportSelectables)
	if len(result.ImportCandidates) > 0 {
		// One of each, for the whole shortlist: neither upstream can filter by the
		// thing being asked about, so both are single wide reads rather than a call
		// per candidate.
		result.Reads.ImportInstances = 1
		result.Reads.ImportExports = 1
	}

	// The catalogue half. Deliberately after the instance half, and given what it
	// found: a type that is already deployed belongs in import_candidates, where it
	// carries a topic and a status, not in a list of things to create.
	deployed := make(map[string]bool, len(result.ImportCandidates))
	for _, candidate := range result.ImportCandidates {
		deployed[candidate.ImportTypeID] = true
	}
	types, cat, reads := r.deployableImportTypes(ctx, token, criteria, snap, deployed, index)
	result.DeployableImportTypes = types
	result.Reads.ImportTypes = reads

	result.Notes = importNotes(result.Notes, true, criteria, result.ImportSelectables, req.Interaction, cat)
	return nil
}

// aspectNames indexes the snapshot's aspect nodes by id.
//
// device-selection resolves the whole aspect node for a device path option but
// only the id for an import one, so an import selectable would otherwise carry an
// aspect the reader cannot name. ODE already holds the full node list, so the
// name is a map lookup rather than a request.
func aspectNames(snap *ontology.Snapshot) map[string]string {
	if snap == nil {
		return nil
	}
	names := make(map[string]string, len(snap.AspectNodes))
	for _, node := range snap.AspectNodes {
		names[node.Id] = node.Name
	}
	return names
}
