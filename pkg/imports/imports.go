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

// Package imports reads import types and import instances on behalf of the
// calling user; the reasoning is in docs/imports-as-operator-inputs.md.
//
// An import is the platform's second kind of operator input: a containerised
// adapter that pulls data from outside and publishes it to one Kafka topic. It
// is described by an import type the way a device is described by a device type,
// and both use the same metadata model — content variables carrying function,
// aspect and characteristic ids. Semantic selection therefore applies unchanged,
// and an intent resolves to imports and devices through the same ontology.
//
// Discovery goes through **device-selection** rather than import-repository
// directly. That service already does the four things a direct caller would have
// to do itself, and getting any of them wrong produces a silently short answer
// rather than an error:
//
//   - import-repository does not expand an aspect criterion over its subtree, unlike
//     the device repository; the caller must send the node plus every descendant id
//   - the criteria index is flattened per import type, so a *type* matches and the
//     matching *paths* have to be found by walking the type's output tree
//   - import-deploy has no filter by import_type_id, so type-to-instance is a
//     client-side join over a full listing
//   - an import type's output describes the whole Kafka message rather than only its
//     payload, so every path needs its first element trimmed to be addressable
//
// One asymmetry does not go away and is the caller's to report: an import type
// carries no device class and an import path is always EVENT, so a resolution
// that narrows by device class cannot include imports at all.
//
// Nothing here is cached. Import visibility is per user, so a cache shared across
// users would be an authorisation bug rather than an optimisation — the same
// reason pkg/devices holds none.
package imports

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
)

// ErrNoCriteria refuses a selectables query with nothing to filter on, for the
// reason ontology.ErrNoCriteria does: an empty criteria list is not an empty
// filter upstream, it is a match on everything.
var ErrNoCriteria = errors.New("imports: a selectables query needs at least one criterion")

// ErrInvalidRequest is a caller's mistake rather than a platform failure, so the
// API can answer 400 instead of reporting an outage.
var ErrInvalidRequest = errors.New("imports: invalid request")

const (
	// DefaultLimit and MaxLimit mirror pkg/devices, so the two listings behave the
	// same way for a caller that does not care which kind of input it is browsing.
	DefaultLimit = 100
	MaxLimit     = 1000

	// instanceListLimit is what a type-to-instance join reads. import-deploy has no
	// filter by import type and its free-text search matches the instance name
	// only, so the join is client-side over a full listing. device-selection uses
	// 10000 for the same reason; matching it keeps the two answers consistent
	// rather than ODE quietly seeing fewer instances than the platform's own UI.
	instanceListLimit = 10000
)

// Selectables is the discovery half: device-selection's selectables query,
// restricted to imports.
type Selectables interface {
	QueryImports(ctx context.Context, token string, criteria []drmodel.FilterCriteria) ([]dsmodel.Selectable, error)
}

// Instances is the deploy half: the listing and the single read, both of which
// carry the container status that discovery does not.
type Instances interface {
	ListInstances(ctx context.Context, token string, opts InstanceListOptions) ([]idmodel.Instance, int64, error)
	ReadInstance(ctx context.Context, token string, id string) (idmodel.Instance, error)
}

// Types is import-repository: one type in full, and the type catalogue.
//
// The catalogue is the half discovery cannot stand in for. device-selection
// joins every matching type to its instances and reports one selectable per
// instance, so a type with no instance is invisible there — which is precisely
// the type somebody is about to deploy the first instance of.
type Types interface {
	ReadImportType(ctx context.Context, token string, id string) (dsmodel.ImportType, error)
	ListImportTypes(ctx context.Context, token string, opts TypeListOptions) ([]dsmodel.ImportType, int64, error)
}

// Deployer is the write half of import-deploy.
//
// Separate from Instances rather than folded into it, because the read path is
// the one every other package here depends on and it has no business carrying a
// method that deploys a container. A deployment can wire reads without writes;
// the reverse is refused, since nothing can be created without first reading the
// type it is created from.
type Deployer interface {
	CreateInstance(ctx context.Context, token string, instance idmodel.Instance) (idmodel.Instance, error)
	DeleteInstance(ctx context.Context, token string, id string) error
}

// ExportWriter is the write half of analytics-serving, plus the export database
// listing that a creation needs and a history lookup does not.
type ExportWriter interface {
	CreateExport(ctx context.Context, token string, request ServingRequest) (Export, error)
	DeleteExport(ctx context.Context, token string, id string) error
	ListDatabases(ctx context.Context, token string) ([]ExportDatabase, error)
}

type InstanceListOptions struct {
	Search string
	Limit  int64
	Offset int64
	// SortBy is import-deploy's `sort` parameter. Empty means name ascending,
	// which is what the platform's own UI shows.
	SortBy string
	// IDs restricts the listing. Nil means no restriction; an empty non-nil slice
	// is refused rather than sent, because upstream reads it as match-nothing and
	// an accidental `ids=` would look like an empty platform.
	IDs []string
	// ExcludeGenerated drops instances a smart service created. Off by default:
	// a generated import is still a usable operator input.
	ExcludeGenerated bool
}

// TypeCriterion is import-repository's filter shape, which is narrower than the
// device repository's in two ways that both matter.
//
// There is no device class and no interaction: an import type has neither, so a
// resolution narrowing by device class cannot include imports at all, and an
// interaction filter is either trivially satisfied or unsatisfiable, since every
// import path is an event.
//
// AspectIDs is a list rather than an id because upstream expands nothing. The
// device repository covers an aspect's whole subtree from the node alone; here
// the node and its descendants are the caller's to send, and sending only the
// node quietly excludes every type described against a child aspect.
type TypeCriterion struct {
	FunctionID string   `json:"function_id"`
	AspectIDs  []string `json:"aspect_ids,omitempty"`
}

type TypeListOptions struct {
	// Search matches the type name only, upstream, as the instance search matches
	// the instance name only.
	Search string
	// IDs restricts the listing, with the same rule InstanceListOptions.IDs has:
	// nil means no restriction, and an empty non-nil slice is refused rather than
	// sent, because upstream reads `ids=` as match-nothing.
	IDs []string
	// Criteria are ANDed by upstream. One criterion per request is the caller's
	// job; see Service.ListTypes.
	Criteria []TypeCriterion
	Limit    int64
	Offset   int64
	// SortBy is import-repository's `sort`. Empty means name ascending.
	SortBy string
}

// TypeListResult mirrors ListResult, so the two listings project the same way.
type TypeListResult struct {
	Types []dsmodel.ImportType `json:"types"`
	// Total is -1 when X-Total-Count did not arrive or did not parse. Unknown is
	// its own answer: a caller cannot otherwise tell a short page from an
	// exhausted one.
	Total  int64 `json:"total"`
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
}

type Service struct {
	selectables Selectables
	instances   Instances
	types       Types
	exports     Exports

	deployer       Deployer
	exportWriter   ExportWriter
	exportDefaults ExportDefaults
}

// Deps are the services this package reads through. selectables and instances
// are required; types and exports are not.
//
// Requiring instances is deliberate. A deployment that could discover imports but
// not say whether one is running would rank a stopped import beside a live one,
// which is the failure this package exists to avoid — discovery carries no status
// field at all.
type Deps struct {
	Selectables Selectables
	Instances   Instances
	// Types answers a direct lookup of one import type by id. Discovery returns the
	// type alongside every instance, so without this only that one tool is lost.
	Types Types
	// Exports is analytics-serving. Without it every history lookup answers
	// HistoryUnknown, which is the honest state rather than a degraded one.
	Exports Exports

	// Deployer and ExportWriter are the write half (see create.go). Both are
	// optional: without them ODE reads the platform and changes nothing, which is
	// what it did before M12 and what a deployment that wants it to keep doing
	// gets by leaving these nil.
	//
	// Neither is usable without Types. Creating an instance validates its configs
	// against the import type and creating an export types its columns from it, and
	// both refuse rather than send an unchecked request — so a deployment with a
	// writer and no import-repository has the tools declared and refusing, not
	// silently guessing.
	Deployer     Deployer
	ExportWriter ExportWriter

	// ExportDefaults are the deployment-specific fields of an export. See the type.
	ExportDefaults ExportDefaults
}

func New(deps Deps) (*Service, error) {
	if deps.Selectables == nil || deps.Instances == nil {
		return nil, errors.New("imports: a selectables client and an instance client are required")
	}
	if deps.ExportWriter != nil && deps.Exports == nil {
		// CreateExport reads the existing exports to copy a timestamp format and to
		// report what it created; a writer without the reader would be a service that
		// can create an export and cannot tell whether one exists.
		return nil, errors.New("imports: an export writer requires the export listing")
	}
	if deps.ExportDefaults.Offset != "" && !validOffset(deps.ExportDefaults.Offset) {
		return nil, fmt.Errorf("imports: export offset %q is not one of %v",
			deps.ExportDefaults.Offset, OffsetValues())
	}
	return &Service{
		selectables:    deps.Selectables,
		instances:      deps.Instances,
		types:          deps.Types,
		exports:        deps.Exports,
		deployer:       deps.Deployer,
		exportWriter:   deps.ExportWriter,
		exportDefaults: deps.ExportDefaults,
	}, nil
}

// Selectable is one addressable variable of one import instance — the
// import-side equivalent of a device-type selectable, except that there is no
// type/instance split to bridge: device-selection resolves both halves in one
// answer, so a selectable here is already tied to a running container.
type Selectable struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	// KafkaTopic is what an operator input's topicName has to be. It is always the
	// instance id with the colons replaced by underscores, but it is read from the
	// instance rather than derived, because deriving it would be ODE asserting an
	// upstream implementation detail.
	KafkaTopic     string `json:"kafka_topic"`
	ImportTypeID   string `json:"import_type_id"`
	ImportTypeName string `json:"import_type_name"`

	// Path is message-relative: `value.temperature`, not `root.value.temperature`.
	// That is the form an operator mapping needs and the form the platform's own
	// flow deployment uses; see the package comment.
	Path string `json:"path"`

	// CharacteristicID is canonical and never fabricated: null is a legitimate
	// answer and a made-up id would authorise a wrong server-side conversion.
	CharacteristicID *string `json:"characteristic_id"`
	Type             string  `json:"type,omitempty"`

	FunctionID string `json:"function_id,omitempty"`
	AspectID   string `json:"aspect_id,omitempty"`
}

// QueryImports resolves criteria to import selectables.
//
// One criterion per call is the caller's job, not this function's: upstream ANDs
// a criteria list for devices but ORs it for imports, and the only request shape
// under which the two halves of a resolution mean the same thing is a single
// criterion. pkg/selection already sends one per combination for the device
// half; this rides on that.
func (s *Service) QueryImports(ctx context.Context, token string, criteria []drmodel.FilterCriteria) ([]Selectable, error) {
	if len(criteria) == 0 {
		return nil, ErrNoCriteria
	}
	found, err := s.selectables.QueryImports(ctx, token, criteria)
	if err != nil {
		return nil, err
	}

	out := []Selectable{}
	for _, selectable := range found {
		if selectable.Import == nil || selectable.ImportType == nil {
			// A device or a device group. The query asks for imports only, so this
			// should not arrive — skipping it beats emitting a selectable with an
			// empty instance id, which a caller would try to wire up.
			continue
		}
		for _, options := range selectable.ServicePathOptions {
			for _, option := range options {
				out = append(out, Selectable{
					InstanceID:       selectable.Import.Id,
					InstanceName:     selectable.Import.Name,
					KafkaTopic:       selectable.Import.KafkaTopic,
					ImportTypeID:     selectable.ImportType.Id,
					ImportTypeName:   selectable.ImportType.Name,
					Path:             option.Path,
					CharacteristicID: characteristic(option.CharacteristicId),
					Type:             string(option.Type),
					FunctionID:       option.FunctionId,
					AspectID:         option.AspectNode.Id,
				})
			}
		}
	}

	// Sorted so a resolution is reproducible. ServicePathOptions is a map and its
	// iteration order is not, which would otherwise reorder the answer between two
	// identical requests.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].InstanceName != out[j].InstanceName {
			return out[i].InstanceName < out[j].InstanceName
		}
		if out[i].InstanceID != out[j].InstanceID {
			return out[i].InstanceID < out[j].InstanceID
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// ListResult mirrors devices.ListResult so the two listings project the same way.
type ListResult struct {
	Instances []idmodel.Instance `json:"instances"`
	Total     int64              `json:"total"`
	Limit     int64              `json:"limit"`
	Offset    int64              `json:"offset"`
}

// List returns the import instances the token's owner may read, with their
// container status.
func (s *Service) List(ctx context.Context, token string, opts InstanceListOptions) (ListResult, error) {
	if opts.IDs != nil && len(opts.IDs) == 0 {
		return ListResult{}, fmt.Errorf("%w: an empty id list matches nothing upstream", ErrInvalidRequest)
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	if opts.Limit > MaxLimit {
		return ListResult{}, fmt.Errorf("%w: limit must not exceed %d, got %d", ErrInvalidRequest, MaxLimit, opts.Limit)
	}
	found, total, err := s.instances.ListInstances(ctx, token, opts)
	if err != nil {
		return ListResult{}, err
	}
	if found == nil {
		found = []idmodel.Instance{}
	}
	return ListResult{Instances: found, Total: total, Limit: opts.Limit, Offset: opts.Offset}, nil
}

// Get reads one instance, including its status.
func (s *Service) Get(ctx context.Context, token string, id string) (idmodel.Instance, error) {
	if strings.TrimSpace(id) == "" {
		return idmodel.Instance{}, fmt.Errorf("%w: an instance id is required", ErrInvalidRequest)
	}
	return s.instances.ReadInstance(ctx, token, id)
}

// GetType reads one import type in full.
func (s *Service) GetType(ctx context.Context, token string, id string) (dsmodel.ImportType, error) {
	if s.types == nil {
		return dsmodel.ImportType{}, fmt.Errorf(
			"%w: no import-repository is configured, so an import type cannot be read by id; "+
				"semantic selection returns the type alongside every instance", ErrInvalidRequest)
	}
	if strings.TrimSpace(id) == "" {
		return dsmodel.ImportType{}, fmt.Errorf("%w: an import type id is required", ErrInvalidRequest)
	}
	return s.types.ReadImportType(ctx, token, id)
}

// ListTypes reads the import type catalogue: what could be deployed, as opposed
// to what is deployed.
//
// One criterion per call, for the reason QueryImports takes one: upstream ANDs
// the criteria list, so [{function A}, {function B}] asks for a type carrying
// both. Alternatives are separate calls, unioned by the caller.
//
// Nothing here filters by what is already running. That join is the caller's,
// because the two questions have different answers: a type with a running
// instance this account cannot read is deployable to this account and invisible
// to its discovery.
func (s *Service) ListTypes(ctx context.Context, token string, opts TypeListOptions) (TypeListResult, error) {
	if s.types == nil {
		return TypeListResult{}, fmt.Errorf(
			"%w: no import-repository is configured, so the import type catalogue cannot be "+
				"read; semantic selection returns the type alongside every instance, but only "+
				"for a type that already has one", ErrInvalidRequest)
	}
	if opts.IDs != nil && len(opts.IDs) == 0 {
		return TypeListResult{}, fmt.Errorf("%w: an empty id list matches nothing upstream", ErrInvalidRequest)
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	if opts.Limit > MaxLimit {
		return TypeListResult{}, fmt.Errorf("%w: limit must not exceed %d, got %d", ErrInvalidRequest, MaxLimit, opts.Limit)
	}
	found, total, err := s.types.ListImportTypes(ctx, token, opts)
	if err != nil {
		return TypeListResult{}, err
	}
	if found == nil {
		found = []dsmodel.ImportType{}
	}
	return TypeListResult{Types: found, Total: total, Limit: opts.Limit, Offset: opts.Offset}, nil
}

// InstancesOfTypes is the type-to-instance join, done client-side because
// import-deploy offers no filter for it (see instanceListLimit).
//
// It exists for the direct-lookup path. Semantic selection does not need it:
// device-selection has already joined by the time a selectable arrives.
func (s *Service) InstancesOfTypes(ctx context.Context, token string, typeIDs []string) ([]idmodel.Instance, error) {
	if len(typeIDs) == 0 {
		return []idmodel.Instance{}, nil
	}
	wanted := make(map[string]bool, len(typeIDs))
	for _, id := range typeIDs {
		wanted[id] = true
	}

	all, _, err := s.instances.ListInstances(ctx, token, InstanceListOptions{Limit: instanceListLimit})
	if err != nil {
		return nil, err
	}
	out := []idmodel.Instance{}
	for _, instance := range all {
		if wanted[instance.ImportTypeId] {
			out = append(out, instance)
		}
	}
	return out, nil
}

// Running reports whether an instance is up, and is deliberately three-valued.
//
// Discovery cannot answer this at all — device-selection's import model omits the
// status field — and a stopped import looks exactly like a live one in a
// selectables answer. Reporting "not running" for an instance whose status simply
// did not arrive would be a wrong claim rather than a missing one, so the absent
// case is its own.
func Running(instance idmodel.Instance) (running bool, known bool) {
	if instance.Status == nil {
		return false, false
	}
	return instance.Status.Running, true
}

// TransitionMessage is what a caller should show beside a stopped instance.
// Empty when the status did not arrive or when there is nothing to say.
func TransitionMessage(instance idmodel.Instance) string {
	if instance.Status == nil {
		return ""
	}
	if instance.Status.Transitioning {
		if instance.Status.Message != "" {
			return "starting or stopping: " + instance.Status.Message
		}
		return "starting or stopping"
	}
	return instance.Status.Message
}

// characteristic keeps an undeclared characteristic distinguishable from a
// declared empty one, for the reason selection.Selectable does: the
// characteristic decides the unit and the declared range, and a fabricated id
// would authorise a wrong conversion.
func characteristic(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}
