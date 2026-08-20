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

// DeviceGroupOptions are the knobs of GET /device-groups that ODE has a reason to
// set.
type DeviceGroupOptions struct {
	// DeviceIDs narrows to groups containing any of the listed devices. This is how
	// §5.5's "check /device-groups for existing groupings" is asked without listing
	// every group on the platform: the devices are already known from the aspect
	// resolution, and a group sharing none of them is not a grouping of them.
	DeviceIDs []string
	Limit     int64
	// IgnoreGenerated drops the groups the platform generates itself. On by default
	// at the call site: a generated group is a by-product of some other feature and
	// proposing it as "an existing grouping worth preferring" would be misleading.
	IgnoreGenerated bool
}

// ListDeviceGroups reads the caller's device groups.
//
// Uncached, like DeviceTypeSelectables and unlike the snapshot, and for a stronger
// reason: a device group is permission-filtered per user, so a process-wide cache
// would be a cache of one developer's groups served to the next. The client method
// takes the token as an argument and sets the header itself, so this one does not
// need the per-call client the tokenless ontology reads do.
func (r *Repository) ListDeviceGroups(
	ctx context.Context, token string, opts DeviceGroupOptions,
) ([]models.DeviceGroup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultDeviceGroupLimit
	}
	options := model.DeviceGroupListOptions{
		Limit:           limit,
		IgnoreGenerated: opts.IgnoreGenerated,
		// Read, not Execute. A device group is metadata about which devices belong
		// together; whether the caller may read those devices' *data* is decided per
		// device where the series are resolved (§5.1).
		Permission: models.Read,
	}
	// Nil rather than an empty slice matters upstream: DeviceIds == nil means "do not
	// filter", DeviceIds == []string{} would filter on nothing and return nothing.
	if len(opts.DeviceIDs) > 0 {
		options.DeviceIds = opts.DeviceIDs
	}

	found, _, err, code := r.newClient(token).ListDeviceGroups(token, options)
	if err != nil {
		return nil, upstream("device-groups", err, code)
	}
	if found == nil {
		found = []models.DeviceGroup{}
	}
	return found, nil
}

const defaultDeviceGroupLimit = 100
