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

package relations

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// The methods a state series can be derived by, as they appear in a document.
const (
	// MethodActivityThreshold is the ordinary path: the profiler's idle/active
	// split with its hysteresis band.
	MethodActivityThreshold = "activity_threshold"
	// MethodBinary is a binary variable, where the split is not a detected
	// threshold but the meaning of the type.
	MethodBinary = "binary"
	// MethodNone is no derivation. It travels with a reason.
	MethodNone = "none"
)

// The origin of the threshold a state series was derived with (§5.10).
const (
	ThresholdDetector  = "detector"
	ThresholdConfirmed = "confirmed"
)

// DeriveState turns one aligned column into idle and active (§5.5).
//
// The threshold is the profiler's, not a second one invented here. That matters
// beyond avoiding duplication: activity_pattern.active_threshold is a confirmable
// field (§5.10), so a developer who has already corrected the split for this series
// has corrected it for every rule the series takes part in. A relational detector
// with its own threshold would quietly ignore that correction and produce rules
// contradicting the profile the developer was looking at.
//
// Three cases produce no state series, and each says so rather than defaulting:
//
//   - activity_pattern is not_computed, usually for insufficient coverage. Its
//     reason is carried through unchanged (D24).
//   - the classification is `continuous`. A series active more than half the time
//     is one population with variation, and thresholding it splits noise.
//   - the classification is `status` or the kind is categorical. There is an
//     argument for relating discrete states, and it is not the one §5.5 makes; a
//     fabricated ordering over category labels would be the wrong answer confidently.
func DeriveState(column AlignedColumn, member Member, profile profiler.ResolvedProfile) StateSeries {
	series := StateSeries{Member: member, States: make([]State, len(column.Values))}
	for i := range series.States {
		series.States[i] = StateUnknown
	}
	// Counted before anything can reject the member, so an unusable one still reports
	// what the read returned.
	observed := 0
	for _, present := range column.Present {
		if present {
			observed++
		}
	}

	activity, computed := profile.ActivityPattern.Get()
	if !computed {
		series.Summary = unusable(profile.ActivityPattern.Status(), len(column.Values)).withObserved(observed)
		series.Member.State = series.Summary
		return series
	}

	switch activity.Classification {
	case profiler.ActivityStatus:
		series.Summary = unusable(profiler.NotComputed{
			Status: notComputedStatus, Reason: profiler.ReasonWrongKind,
			Detail: "the series carries states rather than an idle/active split, and this " +
				"detector relates activity rather than category labels",
		}, len(column.Values)).withObserved(observed)
		series.Member.State = series.Summary
		return series
	case profiler.ActivityContinuous:
		series.Summary = unusable(profiler.NotComputed{
			Status: notComputedStatus, Reason: profiler.ReasonWrongKind,
			Detail: fmt.Sprintf("the series is continuous with variation rather than a sequence of "+
				"sessions (idle level %.4g, active threshold %.4g), so an idle state would be a "+
				"property of the threshold rather than of the device",
				activity.IdleLevel, activity.ActiveThreshold),
		}, len(column.Values)).withObserved(observed)
		series.Member.State = series.Summary
		return series
	}

	threshold := activity.ActiveThreshold
	source := ThresholdDetector
	if confirmed, found := confirmedThreshold(profile); found {
		threshold = confirmed
		source = ThresholdConfirmed
	}

	method := MethodActivityThreshold
	if kind, known := profile.ValueSemantics.Kind.Get(); known && kind == profiler.KindBinary {
		method = MethodBinary
	}

	// The same hysteresis the session detector uses, applied to the aligned grid:
	// once active, a member stays active until it falls below the lower band, so a
	// series hovering at the threshold does not alternate every bucket and inflate
	// both the support and the violation count of every rule it appears in.
	// The band is a fraction of the threshold's *magnitude*, not of its signed
	// value. Scaling the signed value works only while the threshold is positive:
	// for a negative one — a bidirectional meter idling near zero and charging at
	// -2000 W puts the KDE valley around -1000 — `threshold * 0.9` is the *less*
	// negative number, so the band came out above the threshold instead of below
	// it. The guard below then clamped it back, which meant no inversion but no
	// hysteresis either: the band collapsed to nothing and the chatter it exists to
	// suppress came straight back, silently, on exactly the series that need it.
	lower := threshold - math.Abs(threshold)*activity.ThresholdParams.HysteresisFrac
	if lower > threshold {
		// Unreachable with a non-negative fraction, and kept as the floor it always
		// was: a band that sits above its own threshold would make every bucket a
		// transition.
		lower = threshold
	}

	summary := StateSummary{
		Usable:          true,
		Method:          method,
		Threshold:       threshold,
		ThresholdSource: source,
		Classification:  activity.Classification,
		ObservedBuckets: observed,
	}

	active := false
	for i := range column.Values {
		if !column.Present[i] {
			// A gap does not carry the previous state forward. Hysteresis is about a
			// value hovering at the threshold, not about assuming a device kept doing
			// what it was doing while nobody was watching.
			active = false
			series.States[i] = StateUnknown
			summary.UnknownBuckets++
			continue
		}
		value := column.Values[i]
		if active {
			active = value >= lower
		} else {
			active = value >= threshold
		}
		if active {
			series.States[i] = StateActive
			summary.ActiveBuckets++
		} else {
			series.States[i] = StateIdle
			summary.IdleBuckets++
		}
	}

	classified := summary.ActiveBuckets + summary.IdleBuckets
	if classified > 0 {
		summary.DutyCycle = roundTo(float64(summary.ActiveBuckets)/float64(classified), 4)
	}
	// A member observed in too few buckets is reported as unusable rather than
	// contributing a state series nothing can be concluded from. The floor is the
	// rule sample floor, because a member below it cannot support a rule anyway.
	if classified < minStateBuckets {
		series.Summary = unusable(profiler.NotComputed{
			Status: notComputedStatus, Reason: profiler.ReasonInsufficientCoverage,
			Detail: fmt.Sprintf("%d of %d aligned buckets carried a value, need at least %d",
				classified, len(column.Values), minStateBuckets),
		}, len(column.Values)).withObserved(observed)
		series.Member.State = series.Summary
		for i := range series.States {
			series.States[i] = StateUnknown
		}
		return series
	}

	series.Summary = summary
	series.Member.State = summary
	return series
}

// minStateBuckets is the fewest observed buckets a member may take part with.
const minStateBuckets = 20

// unusableSeries is a member that yielded no state series at all, with the reason it
// did not. Every bucket is unknown, so it takes part in no pair and no rule.
func unusableSeries(member Member, reason profiler.NotComputed, buckets, observed int) StateSeries {
	summary := unusable(reason, buckets).withObserved(observed)
	member.State = summary
	states := make([]State, buckets)
	for i := range states {
		states[i] = StateUnknown
	}
	return StateSeries{Member: member, States: states, Summary: summary}
}

// withObserved stamps what the aligned read returned onto a summary, whichever way
// the member turned out.
func (s StateSummary) withObserved(observed int) StateSummary {
	s.ObservedBuckets = observed
	return s
}

func unusable(reason profiler.NotComputed, buckets int) StateSummary {
	return StateSummary{
		Usable:          false,
		Reason:          &reason,
		Method:          MethodNone,
		ThresholdSource: ThresholdDetector,
		UnknownBuckets:  buckets,
	}
}

// confirmedThreshold reads a developer's correction of the idle/active split out
// of the resolved profile's overlay.
//
// Only a correction carries a value: a `confirm` agrees with what the detector
// said and a `reject` says the field is wrong without saying what is right, so
// neither replaces the number. A rejected threshold deliberately still derives a
// state series from the detected value — the alternative is dropping the member
// entirely, and a developer who rejected a threshold has usually done so on the
// way to correcting it.
func confirmedThreshold(profile profiler.ResolvedProfile) (float64, bool) {
	resolution, found := profile.Resolution[profiler.FieldActiveThreshold]
	if !found || resolution.Action != profiler.ActionCorrect {
		return 0, false
	}
	switch value := resolution.ConfirmedValue.(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		if parsed, err := value.Float64(); err == nil {
			return parsed, true
		}
	}
	return 0, false
}
