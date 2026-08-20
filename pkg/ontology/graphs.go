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

package ontology

import (
	"context"

	"github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

// GraphOptions are the knobs of GET /graphs that ODE has a reason to set.
type GraphOptions struct {
	// DeviceIDs narrows to graphs containing any of the listed devices. This is how
	// §5.5 asks the question without enumerating every graph on the platform: the
	// devices are already known from the aspect resolution, and a graph touching none
	// of them says nothing about them.
	DeviceIDs []string
	Limit     int64
}

// ListGraphs reads the caller's device relationship graphs.
//
// A graph is the platform's record of how devices are *wired*, not merely which
// belong together: SPEC §5.1's table calls it a device relationship graph with
// weighted edges, and models.Graph.Valid pins down what that means — a directed
// acyclic flow graph whose outgoing edge weights sum to 100 per node and which has
// exactly one node with no outputs, and that sink may not be a device. In practice
// that is a sub-metering or aggregation topology: devices feed structural nodes,
// the weights are the share attributed along each edge, and the intermediate nodes
// are whatever the operator structures by — a busbar, a location, a business unit.
//
// Uncached, like DeviceTypeSelectables and ListDeviceGroups, and for the same
// stronger reason as the latter: a graph is permission-filtered per user, so a
// process-wide cache would be one developer's topology served to the next. The
// client method takes the token as an argument and sets the header itself.
func (r *Repository) ListGraphs(
	ctx context.Context, token string, opts GraphOptions,
) ([]models.Graph, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultGraphLimit
	}
	options := model.GraphListOptions{
		Limit: limit,
		// Read, not Execute. A graph is metadata about how devices relate; whether the
		// caller may read a given device's *data* is decided per device where the series
		// are resolved (§5.1).
		Permission: models.Read,
	}
	// Nil rather than an empty slice matters upstream: DeviceIds == nil means "do not
	// filter", DeviceIds == []string{} would filter on nothing and return nothing.
	if len(opts.DeviceIDs) > 0 {
		options.DeviceIds = opts.DeviceIDs
	}

	found, _, err, code := r.newClient(token).ListGraphs(token, options)
	if err != nil {
		return nil, upstream("graphs", err, code)
	}
	if found == nil {
		found = []models.Graph{}
	}
	return found, nil
}

const defaultGraphLimit = 100

// GraphNodeName is a node's display name.
//
// models.Node carries no name field — only attributes — so a label has to be read
// out of them, and there is no constant upstream naming the key. The fallbacks are
// deliberate rather than cosmetic: a set proposed because two devices feed "the
// kitchen circuit" is one a developer can judge, and one proposed because they feed
// "node 7f3a" is not.
func GraphNodeName(node models.Node) string {
	for _, attribute := range node.Attributes {
		switch attribute.Key {
		case "name", "label", "display_name":
			if attribute.Value != "" {
				return attribute.Value
			}
		}
	}
	if node.ResourceType != "" {
		return node.ResourceType + " " + shortID(node.Id)
	}
	return "node " + shortID(node.Id)
}

// GraphName is a graph's display name, on the same terms.
func GraphName(graph models.Graph) string {
	for _, attribute := range graph.Attributes {
		switch attribute.Key {
		case "name", "label", "display_name":
			if attribute.Value != "" {
				return attribute.Value
			}
		}
	}
	return "graph " + shortID(graph.Id)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
