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
	"sort"
	"strings"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
)

// buildCriteria decomposes the resolved entities into the requests that will
// actually be sent.
//
// This is the one place where the shape of the platform's API dictates ODE's
// design, so it is worth stating plainly. The device repository treats a criteria
// list as an **AND**: each criterion narrows the device type set the next one is
// applied to. [{function: power}, {function: energy}] therefore asks for a device
// type that carries *both*, which is almost never what an intent means.
//
// What an intent means is "any matched function, in any matched aspect". That is
// the union over the cross product of the two sets — one request per combination,
// merged by the caller. Sending the matched sets as one list instead would return
// device types carrying every match at once, which for two unrelated functions is
// none at all: an empty answer that looks like an empty platform.
//
// Aspects are never expanded here. An aspect criterion already covers the node
// and all its descendants upstream, so passing descendants as extra criteria
// would AND a parent with its child and match nothing.
//
// Aspects are combined by class before that cross product (SNRGY-4648):
// a device-repository criterion ANDs its AspectIds on one variable,
// and a node classifies at most one aspect of a hierarchy per variable, so an
// intent naming "PV" and "kitchen" — two different classes — means a variable
// classified under both, not an unconstrained OR of the two. Two aspects of the
// *same* class are alternatives instead, because a variable carries at most one
// aspect per classified hierarchy: it cannot be both "upstairs" and "downstairs"
// in a location hierarchy, so those become separate criteria. An unclassified
// aspect is unrelated to any other match by definition and always forms its own
// criterion alone, exactly as every aspect did before classes existed.
func buildCriteria(
	functions []ontology.FunctionMatch,
	aspects []ontology.AspectMatch,
	deviceClassIDs []string,
	interaction models.Interaction,
	max int,
) (out []Criterion, dropped int) {
	// A slot list of one empty id means "do not constrain on this dimension": the
	// repository skips an empty field rather than matching on it.
	type slot struct {
		id    string
		score float64
	}

	functionSlots := []slot{{}}
	if len(functions) > 0 {
		functionSlots = functionSlots[:0]
		for _, f := range functions {
			functionSlots = append(functionSlots, slot{id: f.Id, score: f.Matched.Score})
		}
	}
	aspectCombos := aspectCombinations(aspects)
	// Device classes are only ever explicit (see the note the resolver adds), so
	// every one of them scores the same and contributes nothing to the ordering.
	classSlots := []slot{{}}
	if len(deviceClassIDs) > 0 {
		classSlots = classSlots[:0]
		for _, id := range deviceClassIDs {
			classSlots = append(classSlots, slot{id: id, score: 1})
		}
	}

	out = []Criterion{}
	for _, function := range functionSlots {
		for _, aspect := range aspectCombos {
			for _, class := range classSlots {
				if function.id == "" && len(aspect.ids) == 0 && class.id == "" {
					// Nothing resolved. Falling through would send one empty criterion,
					// which upstream matches every device type on the platform.
					continue
				}
				out = append(out, Criterion{
					FunctionID:    function.id,
					AspectIDs:     aspect.ids,
					DeviceClassID: class.id,
					Interaction:   interaction,
					score:         function.score + aspect.score + class.score,
				})
			}
		}
	}

	// Strongest first, so the cap drops the weakest combinations rather than
	// whichever the map iteration happened to reach last. Ties break on the joined
	// ids to keep a resolution reproducible.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if out[i].FunctionID != out[j].FunctionID {
			return out[i].FunctionID < out[j].FunctionID
		}
		if ai, aj := strings.Join(out[i].AspectIDs, ","), strings.Join(out[j].AspectIDs, ","); ai != aj {
			return ai < aj
		}
		return out[i].DeviceClassID < out[j].DeviceClassID
	})

	if max > 0 && len(out) > max {
		dropped = len(out) - max
		out = out[:max]
	}
	return out, dropped
}

// aspectCombo is one alternative way to narrow by aspect: the ids a single
// criterion ANDs, and the combined score of the matches it came from.
type aspectCombo struct {
	ids   []string
	score float64
}

// aspectCombinations groups matched or explicit aspects by aspect class and
// turns them into the alternatives buildCriteria crosses with functions and
// device classes. See buildCriteria's header for the reasoning.
func aspectCombinations(aspects []ontology.AspectMatch) []aspectCombo {
	if len(aspects) == 0 {
		return []aspectCombo{{}}
	}

	var classOrder []string
	byClass := map[string][]ontology.AspectMatch{}
	combos := []aspectCombo{}

	for _, a := range aspects {
		if a.AspectClassId == "" {
			// Unrelated to every other match by definition: always its own
			// criterion, never crossed with another class.
			combos = append(combos, aspectCombo{ids: []string{a.Id}, score: a.Matched.Score})
			continue
		}
		if _, seen := byClass[a.AspectClassId]; !seen {
			classOrder = append(classOrder, a.AspectClassId)
		}
		byClass[a.AspectClassId] = append(byClass[a.AspectClassId], a)
	}
	// Deterministic iteration order over the classes. It does not change which
	// combinations exist, only the order they are built in, which the caller
	// sorts afterwards anyway — but a map iterated directly would make that
	// sort's input order (and so its stable ties) flap between runs.
	sort.Strings(classOrder)

	product := []aspectCombo{{}}
	for _, class := range classOrder {
		next := make([]aspectCombo, 0, len(product)*len(byClass[class]))
		for _, existing := range product {
			for _, a := range byClass[class] {
				ids := append(append([]string{}, existing.ids...), a.Id)
				sort.Strings(ids)
				next = append(next, aspectCombo{ids: ids, score: existing.score + a.Matched.Score})
			}
		}
		product = next
	}
	if len(classOrder) > 0 {
		combos = append(combos, product...)
	}
	return combos
}
