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
	aspectSlots := []slot{{}}
	if len(aspects) > 0 {
		aspectSlots = aspectSlots[:0]
		for _, a := range aspects {
			aspectSlots = append(aspectSlots, slot{id: a.Id, score: a.Matched.Score})
		}
	}
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
		for _, aspect := range aspectSlots {
			for _, class := range classSlots {
				if function.id == "" && aspect.id == "" && class.id == "" {
					// Nothing resolved. Falling through would send one empty criterion,
					// which upstream matches every device type on the platform.
					continue
				}
				out = append(out, Criterion{
					FunctionID:    function.id,
					AspectID:      aspect.id,
					DeviceClassID: class.id,
					Interaction:   interaction,
					score:         function.score + aspect.score + class.score,
				})
			}
		}
	}

	// Strongest first, so the cap drops the weakest combinations rather than
	// whichever the map iteration happened to reach last. Ties break on the ids to
	// keep a resolution reproducible.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		if out[i].FunctionID != out[j].FunctionID {
			return out[i].FunctionID < out[j].FunctionID
		}
		if out[i].AspectID != out[j].AspectID {
			return out[i].AspectID < out[j].AspectID
		}
		return out[i].DeviceClassID < out[j].DeviceClassID
	})

	if max > 0 && len(out) > max {
		dropped = len(out) - max
		out = out[:max]
	}
	return out, dropped
}
