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
	"errors"

	"github.com/SENERGY-Platform/device-repository/lib/model"
)

// ErrNoCriteria refuses a query with nothing to filter on.
//
// The device repository substitutes an empty criteria list with one empty
// criterion, which matches every device type on the platform. That is never what
// a caller means, and discovering it as a listing of the entire platform is an
// expensive way to find out.
var ErrNoCriteria = errors.New("ontology: a selectables query needs at least one criterion")

// SelectableOptions are the two knobs of POST /v2/query/device-type-selectables
// that ODE has a reason to set.
type SelectableOptions struct {
	// PathPrefix is prepended to every returned variable path. ODE leaves it
	// empty: the paths have to be the ones timescale-wrapper accepts as column
	// names, and those start at the service output's root variable.
	PathPrefix string
	// ServicesMustMatchAllCriteria demands that a single service satisfy every
	// criterion, rather than the device type satisfying them across several of
	// its services.
	ServicesMustMatchAllCriteria bool
}

// DeviceTypeSelectables is the primary semantic selection endpoint (SPEC §5.2):
// filter criteria in, matching services with resolved variable paths out.
//
// It is deliberately not cached, unlike the snapshot. This is a query over
// caller-supplied criteria rather than a fixed document, and the device
// repository answers it from an index built for exactly this purpose — so a
// cache here would key on criteria, hold arbitrarily many entries, and save a
// request the platform is already fast at.
//
// Two properties of the upstream implementation shape every caller:
//
//   - The criteria list is an AND. Each criterion narrows the device type set the
//     next one is applied to, so [{function A}, {function B}] asks for a device
//     type carrying *both*, not either. Alternatives are separate requests,
//     unioned by the caller.
//   - An aspect criterion already covers the aspect's whole subtree: the
//     repository expands it to the node plus its descendants. Passing descendants
//     as additional criteria would AND them and match nothing.
func (r *Repository) DeviceTypeSelectables(
	ctx context.Context,
	token string,
	criteria []model.FilterCriteria,
	opts SelectableOptions,
) ([]model.DeviceTypeSelectable, error) {
	if len(criteria) == 0 {
		return nil, ErrNoCriteria
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The client is built per call and bound to the caller's token for the same
	// reason the snapshot loader does it: this method takes no token argument and
	// sets no Authorization header of its own (see ClientFactory).
	//
	// include_id_modified is false. A modified device type id carries a
	// service-group suffix, and no device's device_type_id ever equals it — so a
	// modified selectable could not be joined back to a device, which is the next
	// step of §5.2's flow.
	found, err, code := r.newClient(token).GetDeviceTypeSelectablesV2(
		criteria, opts.PathPrefix, false, opts.ServicesMustMatchAllCriteria)
	if err != nil {
		return nil, upstream("device-type-selectables", err, code)
	}
	if found == nil {
		found = []model.DeviceTypeSelectable{}
	}
	return found, nil
}
