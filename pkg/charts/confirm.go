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

package charts

import (
	"context"
	"fmt"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// ConfirmRequest is one developer decision taken from a chart (§5.10).
//
// There is deliberately no annotation id here. The bands a profile contributes are
// derived at read time, so their ids exist only for the render they were minted
// for; referring to one afterwards would be referring to nothing. What is durable
// is the field path plus the computed value that was on screen — which is exactly
// the pair the override overlay records, and exactly what makes "the detector said
// X, the developer said Y" readable a year later (§5.4.3).
type ConfirmRequest struct {
	ChartID     string
	UserSub     string
	SeriesIndex int

	FieldPath      string
	Action         profiler.OverrideAction
	ComputedValue  any
	ConfirmedValue any
	Note           string
}

// Confirmed is the record that was appended, plus the series as it now resolves —
// so a pane that has just confirmed a unit can relabel its axis without guessing
// what the confirmation did.
type Confirmed struct {
	Override profiler.ProfileOverride `json:"override"`
	Series   SeriesResolution         `json:"series"`
	// Confirmable is the closed set of paths, echoed for the same reason the
	// profiler route echoes it: the backend is authoritative and a client that has
	// drifted should be able to see it.
	Confirmable map[string]string `json:"confirmable"`
}

// Confirm appends a developer confirmation to the profiler's override overlay.
//
// It writes to that overlay rather than to a store of its own, and that is the
// substance of M5's confirmation controls rather than an implementation detail.
// §5.10 says confirmations persist as overrides, are re-injected into subsequent
// profiles, and are recorded in the artifact — so a unit confirmed while looking at
// a chart has to be the same record as one confirmed while reading a profile. The
// overlay is keyed by series reference (§5.4.3), which is also why a chart can
// confirm a series that has never been profiled: the decision waits for the profile
// rather than the other way round.
//
// There is no LLM tool for this and there must never be. §5.8 lists writing a
// ProfileOverride among the operations with no tool at all: a model that can
// confirm its own inferred unit has confirmed nothing.
func (s *Service) Confirm(ctx context.Context, token string, req ConfirmRequest) (Confirmed, error) {
	spec, err := s.Get(req.ChartID, req.UserSub)
	if err != nil {
		return Confirmed{}, err
	}
	if req.SeriesIndex < 0 || req.SeriesIndex >= len(spec.Series) {
		return Confirmed{}, fmt.Errorf("%w: series_index %d is outside the chart's %d series",
			ErrInvalidSpec, req.SeriesIndex, len(spec.Series))
	}
	if _, confirmable := profiler.ConfirmablePaths[req.FieldPath]; !confirmable {
		return Confirmed{}, fmt.Errorf("%w: %q. Confirmable fields are the derived semantics the profiler infers",
			ErrNotConfirmable, req.FieldPath)
	}

	series := spec.Series[req.SeriesIndex]

	index, err := s.deps.Ontology.Ontology(ctx, token)
	if err != nil {
		return Confirmed{}, err
	}
	device, err := s.deps.Devices.Get(token, series.Ref.DeviceID, models.Execute)
	if err != nil {
		return Confirmed{}, err
	}
	resolved, err := s.resolveSeries(req.SeriesIndex, series, device, index, spec, spec.GroupTime)
	if err != nil {
		return Confirmed{}, err
	}

	computed := req.ComputedValue
	if computed == nil {
		// Filled here rather than trusted from the client. The left-hand side of the
		// record is what ODE computed, and a client that omits it — or misreports it —
		// would leave the overlay undiffable, which is the one thing §5.4.3 says a
		// mutable document would destroy.
		computed = s.computedValue(series, resolveUnit(resolved.variable, index), req.FieldPath)
	}

	detectorVersion := ""
	if series.ProfileID != "" {
		if profile, found := s.deps.Profiles.ByID(series.ProfileID); found {
			detectorVersion = profile.DetectorVersion
		}
	}

	stored, err := s.deps.Profiles.AppendOverride(profiler.ProfileOverride{
		SeriesRef:       series.Ref,
		ProfileID:       series.ProfileID,
		DetectorVersion: detectorVersion,
		CreatedBy:       req.UserSub,
		CreatedAt:       s.deps.Now(),
		FieldPath:       req.FieldPath,
		Action:          req.Action,
		ComputedValue:   computed,
		ConfirmedValue:  req.ConfirmedValue,
		Note:            req.Note,
	})
	if err != nil {
		return Confirmed{}, err
	}

	// Re-resolved after the append, so the answer carries the unit that now stands
	// rather than the one that did a moment ago.
	after, err := s.resolveSeries(req.SeriesIndex, series, device, index, spec, spec.GroupTime)
	if err != nil {
		return Confirmed{}, err
	}
	return Confirmed{
		Override:    stored,
		Series:      after.SeriesResolution,
		Confirmable: profiler.ConfirmablePaths,
	}, nil
}

// computedValue is what the detectors and the ontology said about a field, for the
// left-hand side of the override record.
//
// The structured fields carry a summary rather than the whole array, mirroring what
// the profiler view sends: confirming a session boundary records which boundaries
// were on screen, not a second copy of them.
func (s *Service) computedValue(series SeriesSpec, unit Unit, fieldPath string) any {
	switch fieldPath {
	case profiler.FieldUnit:
		return unit.Unit
	case profiler.FieldCharacteristic:
		if unit.CharacteristicID == nil {
			return nil
		}
		return *unit.CharacteristicID
	}

	if series.ProfileID == "" {
		return nil
	}
	profile, found := s.deps.Profiles.ByID(series.ProfileID)
	if !found {
		return nil
	}

	switch fieldPath {
	case profiler.FieldValueKind:
		if kind, ok := profile.ValueSemantics.Kind.Get(); ok {
			return kind
		}
	case profiler.FieldActivityClass:
		if activity, ok := profile.ActivityPattern.Get(); ok {
			return activity.Classification
		}
	case profiler.FieldActiveThreshold:
		if activity, ok := profile.ActivityPattern.Get(); ok {
			return activity.ActiveThreshold
		}
	case profiler.FieldSessions:
		if activity, ok := profile.ActivityPattern.Get(); ok {
			if stats, ok := activity.SessionStats.Get(); ok {
				return map[string]any{"count": stats.Count, "median_duration_s": stats.MedianDurationS}
			}
		}
	case profiler.FieldSamplingGaps:
		if sampling, ok := profile.Sampling.Get(); ok {
			return map[string]any{"count": len(sampling.Gaps)}
		}
	case profiler.FieldUsableRange:
		if usable, ok := profile.Recommendations.UsableRange.Get(); ok {
			return usable
		}
	case profiler.FieldExclusions:
		return map[string]any{"count": len(profile.Recommendations.Exclusions)}
	}
	return nil
}
