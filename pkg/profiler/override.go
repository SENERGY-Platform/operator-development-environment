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
	"fmt"
	"sort"
	"time"
)

type OverrideAction string

const (
	ActionConfirm OverrideAction = "confirm"
	ActionCorrect OverrideAction = "correct"
	ActionReject  OverrideAction = "reject"
)

// ProfileOverride is one developer confirmation, correction or rejection
// (D21, §5.4.3). It never modifies a profile: the overlay is append-only and is
// merged at read time only.
//
// Three things rest on that. Recomputation is non-destructive, so improving a
// detector preserves confirmations. Computed versus confirmed stays diffable.
// And the log is an empirical record — "the detector said W, the developer
// corrected it to kW" is a finding, and a mutable document would destroy it.
type ProfileOverride struct {
	OverrideID string    `json:"override_id"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by"`

	// SeriesRef is what an override is keyed by. ProfileID records which profile
	// the developer was looking at, and DetectorVersion which detectors produced
	// it, so a confirmation made against an old detector can be reviewed rather
	// than silently carried forward — but neither is used for lookup, because a
	// recomputation changes both and the confirmation has to survive it.
	SeriesRef       SeriesRef `json:"series_ref"`
	ProfileID       string    `json:"profile_id"`
	DetectorVersion string    `json:"detector_version,omitempty"`

	FieldPath      string         `json:"field_path"`
	ComputedValue  any            `json:"computed_value"`
	ConfirmedValue any            `json:"confirmed_value"`
	Action         OverrideAction `json:"action"`
	Note           string         `json:"note,omitempty"`
}

// ConfirmablePaths is the closed set of fields a developer may override
// (§5.10), so a typo in a field path is refused rather than accepted and
// silently ignored for the rest of the project.
//
// value_semantics.kind is included beyond the list in §5.10. It is derived
// semantics of exactly the kind D11 covers, and it is the one detector whose
// misreading produces silent garbage — a cumulative kWh counter treated as
// instantaneous power. Leaving it uncorrectable would be the wrong trade.
var ConfirmablePaths = map[string]string{
	FieldUnit:            "the resolved unit, where it was inferred, unknown or in conflict",
	FieldCharacteristic:  "the canonical characteristic id",
	FieldValueKind:       "instantaneous, cumulative counter, binary, categorical or status",
	FieldActivityClass:   "continuous, session based, intermittent or status",
	FieldActiveThreshold: "the idle/active split the session detector used",
	FieldSessions:        "detected session boundaries",
	FieldSamplingGaps:    "the classification of a detected gap",
	FieldUsableRange:     "the range recommended as usable",
	FieldExclusions:      "the ranges recommended for exclusion",
}

func (o ProfileOverride) Validate() error {
	if !o.SeriesRef.Valid() {
		return fmt.Errorf("%w: series_ref must name a device, service and variable path", ErrInvalidOverride)
	}
	if _, confirmable := ConfirmablePaths[o.FieldPath]; !confirmable {
		return fmt.Errorf("%w: field_path %q is not confirmable", ErrInvalidOverride, o.FieldPath)
	}
	switch o.Action {
	case ActionConfirm, ActionCorrect, ActionReject:
	default:
		return fmt.Errorf("%w: action must be confirm, correct or reject, got %q", ErrInvalidOverride, o.Action)
	}
	if o.Action == ActionCorrect && o.ConfirmedValue == nil {
		return fmt.Errorf("%w: a correction must carry a confirmed_value", ErrInvalidOverride)
	}
	if o.CreatedBy == "" {
		return fmt.Errorf("%w: created_by must identify the developer", ErrInvalidOverride)
	}
	return nil
}

// Resolution is one applied override, as it appears in a resolved profile.
type Resolution struct {
	FieldPath      string         `json:"field_path"`
	ComputedValue  any            `json:"computed_value"`
	ConfirmedValue any            `json:"confirmed_value"`
	Action         OverrideAction `json:"action"`
	OverrideID     string         `json:"override_id"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	Note           string         `json:"note,omitempty"`
	// Supersedes names earlier overrides on the same path. The overlay is
	// append-only, so a developer who changes their mind adds a record rather
	// than replacing one, and the history stays visible.
	Supersedes []string `json:"supersedes,omitempty"`
}

// ResolvedProfile is a profile plus the overlay that applies to it. The body is
// untouched: the merged form is never stored, and a reader can always see what
// the detector said next to what the developer said.
type ResolvedProfile struct {
	SeriesProfile
	Resolution map[string]Resolution `json:"resolution"`
}

// Resolve merges the overlay onto a profile. Pure function, no I/O (§5.4.3).
//
// Overrides for other series are ignored rather than rejected, so a caller may
// pass a project's whole overlay without filtering it first.
func Resolve(profile SeriesProfile, overrides []ProfileOverride) ResolvedProfile {
	resolved := ResolvedProfile{SeriesProfile: profile, Resolution: map[string]Resolution{}}

	applicable := make([]ProfileOverride, 0, len(overrides))
	for _, override := range overrides {
		if override.SeriesRef == profile.SeriesRef {
			applicable = append(applicable, override)
		}
	}
	sort.SliceStable(applicable, func(i, j int) bool {
		if applicable[i].CreatedAt.Equal(applicable[j].CreatedAt) {
			return applicable[i].OverrideID < applicable[j].OverrideID
		}
		return applicable[i].CreatedAt.Before(applicable[j].CreatedAt)
	})

	for _, override := range applicable {
		previous, existed := resolved.Resolution[override.FieldPath]
		resolution := Resolution{
			FieldPath:      override.FieldPath,
			ComputedValue:  override.ComputedValue,
			ConfirmedValue: override.ConfirmedValue,
			Action:         override.Action,
			OverrideID:     override.OverrideID,
			CreatedBy:      override.CreatedBy,
			CreatedAt:      override.CreatedAt,
			Note:           override.Note,
		}
		if existed {
			resolution.Supersedes = append(append([]string{}, previous.Supersedes...), previous.OverrideID)
			// The computed value stays that of the first override on the path:
			// it is what the detector produced, and later overrides correct each
			// other rather than the detector.
			resolution.ComputedValue = previous.ComputedValue
		}
		resolved.Resolution[override.FieldPath] = resolution
	}
	return resolved
}

// Effective returns the value that stands for a field: the developer's, where
// one was recorded and not a rejection, and otherwise the computed one.
func (r ResolvedProfile) Effective(fieldPath string, computed any) (any, bool) {
	resolution, overridden := r.Resolution[fieldPath]
	if !overridden {
		return computed, false
	}
	switch resolution.Action {
	case ActionCorrect:
		return resolution.ConfirmedValue, true
	case ActionReject:
		return nil, true
	default:
		// A confirmation agrees with the computed value; the value does not
		// change, but its confidence does (D23: developer-confirmed is certain).
		return computed, true
	}
}

// Confirmed says whether a field carries a developer decision at all, which is
// what raises a field's confidence to certain.
func (r ResolvedProfile) Confirmed(fieldPath string) bool {
	_, overridden := r.Resolution[fieldPath]
	return overridden
}
