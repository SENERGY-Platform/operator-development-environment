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

package simulation

import (
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/models/go/models"
)

// TimePathAttribute is the service attribute the platform's timescale ingestion
// reads to find the event time inside the payload. Without it a row is stamped
// with the moment it arrived.
//
// The value is a dotted path starting at the name of the output's root content
// variable, because the ingestion looks the value up in the flattened message
// where every column carries that root name as a prefix. Verbatim from
// platform-connector-lib, `psql/publisher.go`: `var timeAttributeKey =
// "senergy/time_path"`.
const TimePathAttribute = "senergy/time_path"

// Backfillable is what ODE could establish about a service's ability to carry a
// historical timestamp.
//
// Three states rather than a boolean, and the third is the point. ODE checks
// what is decidable from the device type alone; MOSES checks more when the job
// runs, and the difference must not read as a promise. See CheckTimePath.
type Backfillable string

const (
	// BackfillPossible means nothing ODE can see stands in the way. It is a
	// necessary condition, not a sufficient one.
	BackfillPossible Backfillable = "possible"
	// BackfillImpossible means this service cannot carry a historical timestamp
	// at all, and the reason is a fact about the device type.
	BackfillImpossible Backfillable = "impossible"
	// BackfillUnknown means ODE could not read the device type to find out —
	// the device repository refused or did not answer. It is not "no".
	BackfillUnknown Backfillable = "unknown"
)

// TimePathVerdict is one service's answer.
type TimePathVerdict struct {
	Backfillable Backfillable `json:"backfillable"`
	// TimePath is the declared attribute value, when there is one. Reported even
	// when the verdict is impossible, because a path that is declared and wrong is
	// a different fix from one that is absent.
	TimePath string `json:"time_path,omitempty"`
	// Reason is why, in the words a developer would need to act on it. Present
	// whenever the verdict is not possible.
	Reason string `json:"reason,omitempty"`
}

// CheckTimePath reports whether a platform service can carry an event time of
// the publisher's choosing, which is what decides whether a simulated channel on
// it can ever be backfilled.
//
// **Why this is checked before a device is created rather than after a job
// runs.** `senergy/time_path` is optional on the platform and unset on most
// device types — it only matters for a publisher that wants to write history, so
// nobody sets it for a device that reports the present. A simulation, though, is
// usually built *because* an operator needs weeks of history on the day the work
// starts, and a channel on a service without the attribute cannot produce one
// row of it. Discovering that from a backfill status is discovering it after the
// devices exist, the scenario is authored and the developer has confirmed twice.
//
// **The verdict is necessary, not sufficient**, and the type says so with three
// states instead of two. MOSES's own `ResolveTimeShape` checks two further
// things that this deliberately does not mirror:
//
//   - that the time variable's characteristic and its declared type agree — the
//     ingestion reads a unix time out of a number and an iso timestamp out of a
//     string. That needs the converter's characteristic ids, and mirroring a
//     table of platform constants here is how a warning starts lying.
//   - that a variable beside the time carries a characteristic and is numeric,
//     so there is a value to publish at all.
//
// So `BackfillPossible` means "nothing ODE can see stands in the way", and every
// answer built on it says that MOSES decides finally when the job runs. Claiming
// more would be worse than checking nothing: a false reassurance is acted on.
//
// The checks below are MOSES's own, in MOSES's order, down to that line. Read
// from `SENERGY-Platform/moses`, `lib/devices/timepath.go`, at
// `docs/backfill.md`'s state of 2026-08-27.
func CheckTimePath(service models.Service) TimePathVerdict {
	// First non-empty attribute wins, which is what the ingestion does.
	path := ""
	for _, attribute := range service.Attributes {
		if attribute.Key == TimePathAttribute && attribute.Value != "" {
			path = attribute.Value
			break
		}
	}
	if path == "" {
		return TimePathVerdict{
			Backfillable: BackfillImpossible,
			Reason: "the service declares no " + TimePathAttribute + " attribute, so the " +
				"platform stamps every event with the moment it arrived. A channel on it " +
				"publishes live data and cannot be backfilled at all — the rows would be a " +
				"block of identical timestamps, which is worse than none",
		}
	}

	verdict := TimePathVerdict{Backfillable: BackfillPossible, TimePath: path}
	impossible := func(format string, args ...any) TimePathVerdict {
		verdict.Backfillable = BackfillImpossible
		verdict.Reason = fmt.Sprintf(format, args...)
		return verdict
	}

	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return impossible("the time path %q names a whole output; the value has to sit beside "+
			"the time in the same output", path)
	}
	// MOSES publishes exactly one protocol segment, so it can fill exactly one
	// output. With a second, the ingestion would read a column MOSES never wrote.
	if len(service.Outputs) != 1 {
		return impossible("the service has %d outputs; the simulator publishes a single "+
			"protocol segment and can fill only one", len(service.Outputs))
	}
	output := service.Outputs[0]
	if output.Serialization != models.JSON {
		return impossible("the output is serialised as %q; the simulator publishes json",
			string(output.Serialization))
	}
	root := output.ContentVariable
	if root.Name != parts[0] {
		// The ingestion resolves the characteristic by exactly this comparison and
		// finds nothing, which leaves it casting from an empty characteristic.
		return impossible("the time path starts at %q, but the output's root variable is %q",
			parts[0], root.Name)
	}
	if _, err := resolveVariable(root, parts[1:]); err != nil {
		return impossible("the time path %q does not resolve: %s", path, err.Error())
	}
	return verdict
}

// resolveVariable walks a dotted path below the root, refusing the shapes the
// platform's own message cleaning cannot survive.
func resolveVariable(root models.ContentVariable, path []string) (models.ContentVariable, error) {
	current := root
	for _, name := range path {
		if current.Type != models.Structure {
			return current, fmt.Errorf("%q is declared as %q and has no member %q",
				current.Name, string(current.Type), name)
		}
		if len(current.SubContentVariables) == 0 {
			// The platform's message cleaning indexes SubContentVariables[0] without
			// checking, so this shape takes the connector down rather than failing.
			return current, fmt.Errorf("%q is a structure without members", current.Name)
		}
		if current.SubContentVariables[0].Name == "*" {
			// A map, not a record: a named key would be read as an entry.
			return current, fmt.Errorf("%q is a map, not a record with a member %q",
				current.Name, name)
		}
		next, found := memberOf(current, name)
		if !found {
			return current, fmt.Errorf("%q has no member %q", current.Name, name)
		}
		current = next
	}
	return current, nil
}

func memberOf(variable models.ContentVariable, name string) (models.ContentVariable, bool) {
	for _, sub := range variable.SubContentVariables {
		if sub.Name == name {
			return sub, true
		}
	}
	return models.ContentVariable{}, false
}

// BackfillCaveat is what every answer built on a CheckTimePath verdict has to
// carry, so that "possible" is never read as "will work".
const BackfillCaveat = "This is what could be established from the device type. MOSES decides " +
	"finally when a job runs, and checks two more things ODE does not: that the time " +
	"variable's characteristic and declared type agree, and that a variable beside it " +
	"carries a numeric value to publish. Read get_backfill_status rather than assuming."
