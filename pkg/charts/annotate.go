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
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// Annotation sources, as §5.9's `source` field. Dotted paths into the profile
// rather than free text, so a reader can go from a band on the chart to the field
// that produced it.
const (
	SourceSessions    = "profiler.sessions"
	SourceGaps        = "profiler.sampling.gaps"
	SourceExclusions  = "profiler.recommendations.exclusions"
	SourceUsableRange = "profiler.recommendations.usable_range"
	SourceResets      = "profiler.value_semantics.counter_resets"
)

// derived is what a profile contributes to a chart, before clipping and capping.
type derived struct {
	annotations []Annotation
	markers     []Marker
	notes       []string
}

// deriveAnnotations turns one series' profile into the bands and marks §5.10 makes
// confirmable.
//
// What is included is decided by that list, not by what would look busy: inferred
// units, activity classification, session boundaries and thresholds, recommended
// usable range and exclusions, gap classifications. Counter resets are included as
// markers although they are not confirmable, because §5.9's own example is a
// counter-reset marker and reading a counter chart without them invites exactly
// the wrong conclusion about a drop to zero.
//
// Constant runs are deliberately left out even though the profile carries them: a
// frozen sensor is already reported as a quality flag, and a chart covered in
// unconfirmable bands is one nobody reads.
//
// Everything here is stamped AuthorProfiler and produced at read time from the
// stored profile. A specification cannot forge one — which is the point of the
// author field.
func deriveAnnotations(
	profile profiler.SeriesProfile,
	sessions []profiler.Session,
	overrides []profiler.ProfileOverride,
	window profiler.Window,
	seriesIndex int,
	ids IDs,
) derived {
	out := derived{annotations: []Annotation{}, markers: []Marker{}, notes: []string{}}
	index := seriesIndex
	confirmed := confirmedPaths(overrides)

	for _, session := range sessions {
		if !overlaps(session.From, session.To, window) {
			continue
		}
		out.annotations = append(out.annotations, Annotation{
			AnnotationID: ids.NewID(),
			Type:         AnnotationSpan,
			From:         session.From,
			To:           session.To,
			Label: fmt.Sprintf("session · %s · peak %s",
				profiler.FormatSeconds(session.DurationS), formatValue(session.Peak)),
			Severity:    SeverityInfo,
			Source:      SourceSessions,
			Confirmable: !confirmed[profiler.FieldSessions],
			FieldPath:   profiler.FieldSessions,
			SeriesIndex: &index,
			Author:      AuthorProfiler,
		})
	}

	if sampling, ok := profile.Sampling.Get(); ok {
		for _, gap := range sampling.Gaps {
			if !overlaps(gap.From, gap.To, window) {
				continue
			}
			out.annotations = append(out.annotations, Annotation{
				AnnotationID: ids.NewID(),
				Type:         AnnotationSpan,
				From:         gap.From,
				To:           gap.To,
				Label: fmt.Sprintf("gap · %s · %s",
					profiler.FormatSeconds(gap.DurationS), gap.Classification),
				Severity:    SeverityWarn,
				Source:      SourceGaps,
				Confirmable: !confirmed[profiler.FieldSamplingGaps],
				FieldPath:   profiler.FieldSamplingGaps,
				SeriesIndex: &index,
				Author:      AuthorProfiler,
			})
		}
	}

	for _, exclusion := range profile.Recommendations.Exclusions {
		if !overlaps(exclusion.From, exclusion.To, window) {
			continue
		}
		out.annotations = append(out.annotations, Annotation{
			AnnotationID: ids.NewID(),
			Type:         AnnotationSpan,
			From:         exclusion.From,
			To:           exclusion.To,
			Label:        "advised exclusion · " + exclusion.Reason,
			Severity:     SeverityError,
			Source:       SourceExclusions,
			Confirmable:  !confirmed[profiler.FieldExclusions],
			FieldPath:    profiler.FieldExclusions,
			SeriesIndex:  &index,
			Author:       AuthorProfiler,
		})
	}

	if usable, ok := profile.Recommendations.UsableRange.Get(); ok && overlaps(usable.From, usable.To, window) {
		out.annotations = append(out.annotations, Annotation{
			AnnotationID: ids.NewID(),
			Type:         AnnotationSpan,
			From:         usable.From,
			To:           usable.To,
			Label:        "advised usable range",
			Severity:     SeverityInfo,
			Source:       SourceUsableRange,
			Confirmable:  !confirmed[profiler.FieldUsableRange],
			FieldPath:    profiler.FieldUsableRange,
			SeriesIndex:  &index,
			Author:       AuthorProfiler,
		})
	}

	if resets, ok := profile.ValueSemantics.CounterResets.Get(); ok {
		for _, at := range resets {
			if at.Before(window.From) || at.After(window.To) {
				continue
			}
			out.markers = append(out.markers, Marker{
				MarkerID:    ids.NewID(),
				At:          at,
				Label:       "counter reset",
				Source:      SourceResets,
				SeriesIndex: &index,
				Author:      AuthorProfiler,
			})
		}
	}

	// A profile computed over a window that does not reach this chart's is worth
	// saying out loud: the absence of bands would otherwise read as "no sessions
	// here" rather than "nothing was analysed here" — D24's distinction, applied to
	// a chart.
	if !covers(profile.AnalysisWindow, window) {
		out.notes = append(out.notes, fmt.Sprintf(
			"the profile behind series %d was computed over %s, which does not cover the charted window; "+
				"annotations exist only inside the analysed range",
			seriesIndex, profile.AnalysisWindow.String()))
	}
	if profile.DetectorVersion != profiler.DetectorVersion {
		out.notes = append(out.notes, fmt.Sprintf(
			"the profile behind series %d was computed by detector version %s, and this build runs %s; "+
				"recompute it if a boundary looks wrong",
			seriesIndex, profile.DetectorVersion, profiler.DetectorVersion))
	}
	return out
}

// confirmedPaths is the set of field paths this series already carries a decision
// on. The overlay's granularity is the field path, not the individual boundary, so
// one confirmation of activity_pattern.sessions settles the band set that was on
// screen — which is what the override's computed_value records.
func confirmedPaths(overrides []profiler.ProfileOverride) map[string]bool {
	out := map[string]bool{}
	for _, override := range overrides {
		out[override.FieldPath] = true
	}
	return out
}

// capAnnotations bounds what one chart carries and reports what it dropped.
//
// Truncating silently would be the same mistake as a truncated series read: a
// chart showing the first fifty of four hundred sessions looks like a chart of
// four hundred. So the count is reported, and the developer narrows the window.
func capAnnotations(annotations []Annotation, limit int) ([]Annotation, int) {
	sort.SliceStable(annotations, func(i, j int) bool {
		if annotations[i].From.Equal(annotations[j].From) {
			return severityRank(annotations[i].Severity) > severityRank(annotations[j].Severity)
		}
		return annotations[i].From.Before(annotations[j].From)
	})
	if limit <= 0 || len(annotations) <= limit {
		return annotations, 0
	}
	// Severity first, so capping never hides an error band behind a hundred
	// informational ones, then chronological within what survives.
	kept := make([]Annotation, len(annotations))
	copy(kept, annotations)
	sort.SliceStable(kept, func(i, j int) bool {
		return severityRank(kept[i].Severity) > severityRank(kept[j].Severity)
	})
	dropped := len(kept) - limit
	kept = kept[:limit]
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].From.Before(kept[j].From) })
	return kept, dropped
}

func severityRank(severity string) int {
	switch severity {
	case SeverityError:
		return 2
	case SeverityWarn:
		return 1
	default:
		return 0
	}
}

func overlaps(from, to time.Time, window profiler.Window) bool {
	if to.IsZero() || to.Before(from) {
		to = from
	}
	return !to.Before(window.From) && !from.After(window.To)
}

func covers(outer, inner profiler.Window) bool {
	if !outer.Valid() {
		return false
	}
	return !outer.From.After(inner.From) && !outer.To.Before(inner.To)
}

// formatValue renders a peak or an energy for a label. Deliberately short: a band
// label is read at a glance and the profile carries the exact figure.
func formatValue(value float64) string {
	switch abs := math.Abs(value); {
	case abs == 0:
		return "0"
	case abs >= 1000:
		return fmt.Sprintf("%.0f", value)
	case abs >= 1:
		return fmt.Sprintf("%.2f", value)
	default:
		return fmt.Sprintf("%.4g", value)
	}
}
