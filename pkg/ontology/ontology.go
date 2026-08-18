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

// Package ontology is a caching, ODE-shaped facade over
// device-repository/lib/client (SPEC §5.1). It does not implement an HTTP
// client: the platform already ships one, and reimplementing it would be a
// second thing to keep in step with the device repository.
//
// The ontology is cached as a whole snapshot rather than as independent keys.
// That matches how it is read — the aspect tree and the function list are
// served entire — and it means a refresh can never leave callers looking at
// aspects from one generation and functions from another.
package ontology

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

// Client is the slice of the device-repository client this package needs.
// Declaring it here rather than importing client.Interface keeps the fake in
// the tests small and documents exactly which endpoints ODE depends on.
//
// Note that none of the ontology reads take a token argument: the ontology is
// platform-global, which is what makes a process-wide cache correct. They
// still need one on the wire, though — see ClientFactory.
type Client interface {
	GetAspectNodes() ([]models.AspectNode, error, int)
	GetFunctionsByType(rdfType string) ([]models.Function, error, int)
	ListCharacteristics(options model.CharacteristicListOptions) ([]models.Characteristic, int64, error, int)
	ListConceptsWithCharacteristics(options model.ConceptListOptions) ([]models.ConceptWithCharacteristics, int64, error, int)
	GetDeviceClasses() ([]models.DeviceClass, error, int)
	GetLastUpdateTimestamps(token string, userId string) ([]model.LastUpdateTimestamp, error, int)
	// GetDeviceTypeSelectablesV2 is the semantic selection query of §5.2. It is
	// listed among the tokenless reads for the same reason as the rest: it
	// authenticates through the client's auth closure, not through an argument.
	GetDeviceTypeSelectablesV2(query []model.FilterCriteria, pathPrefix string, includeModified bool, servicesMustMatchAllCriteria bool) ([]model.DeviceTypeSelectable, error, int)
}

// Snapshot is one consistent read of the ontology.
type Snapshot struct {
	AspectNodes          []models.AspectNode                 `json:"aspect_nodes"`
	MeasuringFunctions   []models.Function                   `json:"measuring_functions"`
	ControllingFunctions []models.Function                   `json:"controlling_functions"`
	Characteristics      []models.Characteristic             `json:"characteristics"`
	Concepts             []models.ConceptWithCharacteristics `json:"concepts"`
	DeviceClasses        []models.DeviceClass                `json:"device_classes"`

	LoadedAt time.Time `json:"loaded_at"`
	// Generation is the newest platform update timestamp this snapshot was
	// built from. Zero means it could not be established, which disables
	// generation-based invalidation for this snapshot rather than causing a
	// reload on every probe.
	Generation int64 `json:"generation"`
}

// ClientFactory builds a client bound to one caller's token.
//
// This indirection exists because the ontology methods on
// device-repository/lib/client take no token argument and set no
// Authorization header of their own: they rely on the client-level auth
// closure supplied at construction. When ODE reaches the device repository
// through the Kong API gateway — which it does, by URL — a request without
// that header is rejected with 401 before it ever reaches the repository.
//
// So the client cannot be built once at startup with a nil closure. It is
// built per load, bound to the token of whichever request triggered it. The
// resulting snapshot is still shared process-wide, which is correct: the
// ontology is identical for every user, and only the transport needed
// authenticating.
type ClientFactory func(token string) Client

type Options struct {
	// TTL is the longest a snapshot is served without any freshness check.
	TTL time.Duration
	// InvalidateInterval is how often the cheap /last-update-timestamps probe
	// runs. It is much shorter than TTL: the probe is one small request,
	// whereas a reload is six.
	InvalidateInterval time.Duration
}

const (
	defaultTTL                = time.Hour
	defaultInvalidateInterval = 5 * time.Minute
	// listPageSize is large enough to hold the whole ontology; the device
	// repository defaults to 100 per page, which would silently truncate it.
	listPageSize = 10000
)

type Repository struct {
	newClient ClientFactory
	opts      Options

	mux  sync.RWMutex
	snap *Snapshot

	// loadMux serialises reloads so a burst of concurrent requests on a cold
	// or expired cache produces one set of upstream calls, not one per caller.
	loadMux sync.Mutex

	probeMux     sync.Mutex
	lastProbedAt time.Time
}

func New(newClient ClientFactory, opts Options) *Repository {
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.InvalidateInterval <= 0 {
		opts.InvalidateInterval = defaultInvalidateInterval
	}
	return &Repository{newClient: newClient, opts: opts}
}

// Snapshot returns the cached ontology, loading or refreshing it when needed.
//
// token is the caller's access token. It is used only for the
// /last-update-timestamps probe: ODE holds no service account for platform
// reads (SPEC D5), so invalidation rides along on user requests rather than on
// a background loop.
func (r *Repository) Snapshot(ctx context.Context, token string) (*Snapshot, error) {
	current := r.cached()
	if current == nil {
		return r.reload(ctx, token, nil)
	}
	if time.Since(current.LoadedAt) >= r.opts.TTL {
		return r.reload(ctx, token, current)
	}
	if newest, probed := r.probeGeneration(token); probed &&
		current.Generation != 0 && newest > current.Generation {
		return r.reload(ctx, token, current)
	}
	return current, nil
}

func (r *Repository) cached() *Snapshot {
	r.mux.RLock()
	defer r.mux.RUnlock()
	return r.snap
}

// probeGeneration asks the platform for its newest update timestamp, at most
// once per InvalidateInterval. A probe failure reports "not probed" rather
// than an error: serving a slightly stale ontology beats failing a request
// because one auxiliary call did not answer.
func (r *Repository) probeGeneration(token string) (newest int64, probed bool) {
	if token == "" {
		return 0, false
	}
	r.probeMux.Lock()
	if time.Since(r.lastProbedAt) < r.opts.InvalidateInterval {
		r.probeMux.Unlock()
		return 0, false
	}
	r.lastProbedAt = time.Now()
	r.probeMux.Unlock()

	stamps, err, _ := r.newClient(token).GetLastUpdateTimestamps(token, "")
	if err != nil {
		return 0, false
	}
	return newestTimestamp(stamps), true
}

func newestTimestamp(stamps []model.LastUpdateTimestamp) int64 {
	var newest int64
	for _, s := range stamps {
		if s.UnixTimestamp > newest {
			newest = s.UnixTimestamp
		}
	}
	return newest
}

// Reload fetches a fresh snapshot, unless a concurrent caller already did.
func (r *Repository) Reload(ctx context.Context, token string) (*Snapshot, error) {
	return r.reload(ctx, token, r.cached())
}

// reload replaces the snapshot that `seen` refers to. If another goroutine
// swapped in a different one while this call waited for loadMux, that result
// is returned instead of fetching again — which is what keeps a thundering
// herd down to a single set of upstream calls.
func (r *Repository) reload(ctx context.Context, token string, seen *Snapshot) (*Snapshot, error) {
	r.loadMux.Lock()
	defer r.loadMux.Unlock()

	if current := r.cached(); current != nil && current != seen {
		return current, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snap := &Snapshot{LoadedAt: time.Now()}
	client := r.newClient(token)

	aspects, err, code := client.GetAspectNodes()
	if err != nil {
		return nil, upstream("aspect-nodes", err, code)
	}
	snap.AspectNodes = aspects

	measuring, err, code := client.GetFunctionsByType(models.SES_ONTOLOGY_MEASURING_FUNCTION)
	if err != nil {
		return nil, upstream("measuring-functions", err, code)
	}
	snap.MeasuringFunctions = measuring

	controlling, err, code := client.GetFunctionsByType(models.SES_ONTOLOGY_CONTROLLING_FUNCTION)
	if err != nil {
		return nil, upstream("controlling-functions", err, code)
	}
	snap.ControllingFunctions = controlling

	characteristics, _, err, code := client.ListCharacteristics(model.CharacteristicListOptions{Limit: listPageSize})
	if err != nil {
		return nil, upstream("characteristics", err, code)
	}
	snap.Characteristics = characteristics

	concepts, _, err, code := client.ListConceptsWithCharacteristics(model.ConceptListOptions{Limit: listPageSize})
	if err != nil {
		return nil, upstream("concepts-with-characteristics", err, code)
	}
	snap.Concepts = concepts

	deviceClasses, err, code := client.GetDeviceClasses()
	if err != nil {
		return nil, upstream("device-classes", err, code)
	}
	snap.DeviceClasses = deviceClasses

	// Stamp the generation now, so the first probe after this load has a real
	// value to compare against instead of forcing an immediate second reload.
	// This counts as a probe: without resetting the clock, the very next
	// request would probe again to learn what was just established.
	if token != "" {
		if stamps, err, _ := client.GetLastUpdateTimestamps(token, ""); err == nil {
			snap.Generation = newestTimestamp(stamps)
			r.probeMux.Lock()
			r.lastProbedAt = time.Now()
			r.probeMux.Unlock()
		}
	}

	r.mux.Lock()
	r.snap = snap
	r.mux.Unlock()
	return snap, nil
}

type UpstreamError struct {
	Resource string
	Code     int
	Err      error
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("ontology: reading %s from device-repository failed with %d: %v", e.Resource, e.Code, e.Err)
}
func (e *UpstreamError) Unwrap() error { return e.Err }

func upstream(resource string, err error, code int) error {
	return &UpstreamError{Resource: resource, Code: code, Err: err}
}
