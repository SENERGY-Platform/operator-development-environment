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

// maxDeviceTypeIDs bounds one lookup. The endpoint ignores limit and offset when
// ids are given, so the bound has to be applied on the way in; a caller with more
// than this many types is asking a different question and should be filtering
// first.
const maxDeviceTypeIDs = 200

// DeviceTypesByID reads whole device types, with the services and — the reason
// this exists — the **attributes** on them.
//
// Every other route ODE has to a device type gives a projection that drops them.
// A selectable carries what a semantic query matched; MOSES's own `/device-types`
// carries id, name, direction, characteristic and value path. Neither carries
// `senergy/time_path`, which is what decides whether a channel published through
// that service can ever hold a historical timestamp — so the question "can this
// be backfilled" has no answer anywhere but here.
//
// One call for the whole set rather than one per type: the endpoint ignores
// limit and offset when ids are given, so a caller that already knows which
// types it means pays for one request.
//
// Uncached, like ListGraphs and ListDeviceGroups, though for the weaker of their
// two reasons. A device type is not permission-filtered the way a graph is, but
// its attributes are exactly the kind of thing an administrator changes to make
// a type backfillable — and an answer served from an hour-old snapshot would tell
// a developer their change did not take.
func (r *Repository) DeviceTypesByID(
	ctx context.Context, token string, ids []string,
) (map[string]models.DeviceType, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wanted := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		wanted = append(wanted, id)
		if len(wanted) >= maxDeviceTypeIDs {
			break
		}
	}
	if len(wanted) == 0 {
		return map[string]models.DeviceType{}, nil
	}

	found, _, err, code := r.newClient(token).ListDeviceTypesV3(token, model.DeviceTypeListOptions{
		Ids: wanted,
	})
	if err != nil {
		return nil, upstream("device-types", err, code)
	}
	out := make(map[string]models.DeviceType, len(found))
	for _, deviceType := range found {
		out[deviceType.Id] = deviceType
	}
	return out, nil
}
