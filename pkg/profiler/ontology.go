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

package profiler

import (
	"context"
	"sort"
	"sync"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
)

// OntologySource hands the profiler an index of the ontology. It is an
// interface so detector tests can supply three characteristics instead of a
// platform snapshot.
type OntologySource interface {
	Ontology(ctx context.Context, token string) (*OntologyIndex, error)
}

// OntologyIndex answers the lookups unit resolution needs (§5.4.11), by id
// rather than by scanning the snapshot's slices per variable.
type OntologyIndex struct {
	characteristics map[string]models.Characteristic
	// conceptOfCharacteristic maps a characteristic to the concept that groups
	// it, which is where the conversion graph lives.
	conceptOfCharacteristic map[string]models.ConceptWithCharacteristics
	functions               map[string]models.Function
}

func NewOntologyIndex(
	characteristics []models.Characteristic,
	concepts []models.ConceptWithCharacteristics,
	functions []models.Function,
) *OntologyIndex {
	index := &OntologyIndex{
		characteristics:         map[string]models.Characteristic{},
		conceptOfCharacteristic: map[string]models.ConceptWithCharacteristics{},
		functions:               map[string]models.Function{},
	}
	for _, c := range characteristics {
		index.addCharacteristic(c)
	}
	for _, concept := range concepts {
		for _, c := range concept.Characteristics {
			index.addCharacteristic(c)
			index.conceptOfCharacteristic[c.Id] = concept
		}
		if concept.BaseCharacteristicId != "" {
			if _, known := index.conceptOfCharacteristic[concept.BaseCharacteristicId]; !known {
				index.conceptOfCharacteristic[concept.BaseCharacteristicId] = concept
			}
		}
	}
	for _, f := range functions {
		index.functions[f.Id] = f
	}
	return index
}

// addCharacteristic indexes sub-characteristics too: a structured
// characteristic's leaves are what a ContentVariable actually references.
func (i *OntologyIndex) addCharacteristic(c models.Characteristic) {
	if c.Id == "" {
		return
	}
	if _, exists := i.characteristics[c.Id]; !exists {
		i.characteristics[c.Id] = c
	}
	for _, sub := range c.SubCharacteristics {
		i.addCharacteristic(sub)
	}
}

func (i *OntologyIndex) Characteristic(id string) (models.Characteristic, bool) {
	if i == nil || id == "" {
		return models.Characteristic{}, false
	}
	c, ok := i.characteristics[id]
	return c, ok
}

func (i *OntologyIndex) Function(id string) (models.Function, bool) {
	if i == nil || id == "" {
		return models.Function{}, false
	}
	f, ok := i.functions[id]
	return f, ok
}

func (i *OntologyIndex) Concept(characteristicID string) (models.ConceptWithCharacteristics, bool) {
	if i == nil || characteristicID == "" {
		return models.ConceptWithCharacteristics{}, false
	}
	c, ok := i.conceptOfCharacteristic[characteristicID]
	return c, ok
}

// Conversions lists the characteristics reachable from this one through the
// concept's conversion graph, cheapest path first (§5.4.11).
//
// It is a shortest-path walk rather than a scan of direct edges because the
// graph is not required to be complete: W→kW and kW→MW can exist without a
// W→MW edge, and the useful answer is that MW is reachable at distance 2. ODE
// only ever selects a target; timescale-wrapper evaluates the formula.
// The empty result is an empty slice rather than nil: a nil slice marshals as
// JSON null, and a consumer that reads a list has to special-case that. D24's
// reasoning applies to the shape as well as to the semantics — "no conversions"
// is an empty list, not an absent field.
func (i *OntologyIndex) Conversions(characteristicID string) []Conversion {
	concept, ok := i.Concept(characteristicID)
	if !ok || len(concept.Conversions) == 0 {
		return []Conversion{}
	}

	edges := map[string][]models.ConverterExtension{}
	for _, edge := range concept.Conversions {
		edges[edge.From] = append(edges[edge.From], edge)
	}

	best := map[string]int64{characteristicID: 0}
	queue := []string{characteristicID}
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		for _, edge := range edges[from] {
			distance := best[from] + edge.Distance
			if edge.Distance <= 0 {
				// A zero or negative distance would make the walk cheaper the
				// longer it gets. Treat it as one hop.
				distance = best[from] + 1
			}
			if known, seen := best[edge.To]; seen && known <= distance {
				continue
			}
			best[edge.To] = distance
			queue = append(queue, edge.To)
		}
	}

	out := make([]Conversion, 0, len(best))
	for id, distance := range best {
		if id == characteristicID {
			continue
		}
		conversion := Conversion{ToCharacteristicID: id, Distance: distance}
		if target, ok := i.Characteristic(id); ok {
			conversion.ToUnit = target.DisplayUnit
		}
		out = append(out, conversion)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Distance == out[b].Distance {
			return out[a].ToCharacteristicID < out[b].ToCharacteristicID
		}
		return out[a].Distance < out[b].Distance
	})
	return out
}

// SnapshotOntology builds indexes from the ontology snapshot cache, memoised
// per snapshot.
//
// Building an index is cheap but not free, and every profile request needs one;
// keying the memo on the snapshot pointer means a refresh invalidates it
// automatically, because the repository swaps in a new snapshot rather than
// mutating the old one.
type SnapshotOntology struct {
	repo *ontology.Repository

	mux   sync.Mutex
	from  *ontology.Snapshot
	index *OntologyIndex
}

func NewSnapshotOntology(repo *ontology.Repository) *SnapshotOntology {
	return &SnapshotOntology{repo: repo}
}

func (s *SnapshotOntology) Ontology(ctx context.Context, token string) (*OntologyIndex, error) {
	snap, err := s.repo.Snapshot(ctx, token)
	if err != nil {
		return nil, err
	}

	s.mux.Lock()
	defer s.mux.Unlock()
	if s.from == snap && s.index != nil {
		return s.index, nil
	}
	functions := make([]models.Function, 0, len(snap.MeasuringFunctions)+len(snap.ControllingFunctions))
	functions = append(functions, snap.MeasuringFunctions...)
	functions = append(functions, snap.ControllingFunctions...)
	s.index = NewOntologyIndex(snap.Characteristics, snap.Concepts, functions)
	s.from = snap
	return s.index, nil
}
