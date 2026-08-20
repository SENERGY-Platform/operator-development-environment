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

// Package relations finds conditional patterns across several devices
// (SPEC §5.5).
//
// The motivating case is one sentence: *the oven being on while the kitchen
// lights are off is an anomaly, except at certain times of day*. Everything here
// exists to get from an aspect node to that sentence, with the numbers behind it,
// without asking the developer to pick devices by name.
//
// Four steps, in the order §5.5 names them:
//
//   - **ProposeRelatedSets** turns an aspect node into candidate device sets. The
//     aspect hierarchy is what solves candidate selection: devices under "Kitchen"
//     yield the oven and the lights without anyone naming either. Existing
//     groupings are preferred over constructed ones, so platform device groups are
//     consulted first. Ontology only — no value is read, so this is tier L0.
//
//   - **Align** issues ONE batched POST /queries/v2 with an identical groupTime
//     for every member. Alignment is a property of the request (§5.3.1), not
//     something to resample client-side, and the bucket comes from the *coarsest*
//     member: a bucket finer than the slowest series manufactures idle states in
//     the gaps between its arrivals.
//
//   - **DeriveState** turns each aligned series into idle/active using the
//     profiler's own activity_pattern — the same threshold, hysteresis and
//     classification a developer already saw and could already confirm (§5.10).
//     A confirmed threshold wins over the detected one, which is what makes
//     confirmation feed forward rather than merely being recorded.
//
//   - **Relate** computes pairwise contingency with lift, confidence and support,
//     conditioned on hour of day and weekday/weekend, and emits candidate rules
//     carrying explicit exception windows.
//
// **Rules are candidates and nothing else.** They are not thresholds anything
// downstream reads, in the same sense and for the same reason as D28's
// recommendations: the developer confirms, corrects or rejects, and only a
// confirmed rule is a rule. The decision log is append-only and keyed by a
// fingerprint that survives recomputation, so improving a detector preserves what
// a developer decided (D21).
package relations

import (
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
)

// DetectorVersion is the version of everything in this package that can change a
// RelationProfile's content. It is part of the cache key for the same reason
// profiler.DetectorVersion is (D25): without it, sharpening the rule thresholds
// leaves stale relation profiles in an LLM's context with nothing to notice them
// by.
const DetectorVersion = "1.0.0"

// State is one member's condition in one aligned bucket.
//
// StateUnknown is not a third condition of the device; it is the absence of an
// observation, and it is kept distinct for the reason D24 gives about absence
// versus negation. A bucket with no reading is not a bucket where the oven was
// off, and counting it as one would inflate every co-occurrence in this file.
type State string

const (
	StateActive  State = "active"
	StateIdle    State = "idle"
	StateUnknown State = "unknown"
)

// SeriesMember is one participant, as a caller names it.
type SeriesMember struct {
	Ref profiler.SeriesRef `json:"ref"`
	// Label is what the member is called in a rule statement. Empty falls back to
	// the device name, then to the reference itself — a rule nobody can read is a
	// rule nobody can confirm.
	Label string `json:"label,omitempty"`
}

// RuleParams are the thresholds a candidate rule has to clear.
//
// They travel in the RelationProfile rather than staying in the code for the same
// reason SessionParams do (§5.4.13 item 7): a developer looking at a rule needs to
// see what admitted it before deciding whether to confirm it, and a developer who
// disagrees needs somewhere to say so.
type RuleParams struct {
	// MinSupport is the share of observed buckets a pattern must cover. It is what
	// stops a rule resting on four coincidences in a year.
	MinSupport float64 `json:"min_support"`
	// MinConfidence is P(consequent | antecedent): how reliably the pattern holds
	// where the antecedent is true.
	MinConfidence float64 `json:"min_confidence"`
	// MinLift is how much more often the pair co-occurs than independent base rates
	// predict. Confidence alone promotes a consequent that is simply always true —
	// a light left on all year confirms every rule about it at confidence 1.0.
	MinLift float64 `json:"min_lift"`
	// MinSamples is the fewest antecedent-true buckets a rule may be built from.
	MinSamples int `json:"min_samples"`
	// HourBuckets is how many equal parts the day is split into for conditioning.
	// Four is deliberately coarse: twenty-four buckets over a month leave too few
	// samples each to distinguish an exception from noise.
	HourBuckets int `json:"hour_buckets"`
	// ExceptionDrop is how far a conditioned confidence must fall below the rule's
	// overall confidence before that condition is reported as an exception.
	ExceptionDrop float64 `json:"exception_drop"`
}

// DefaultRuleParams are the thresholds a request that names none is run with.
func DefaultRuleParams() RuleParams {
	return RuleParams{
		MinSupport:    0.01,
		MinConfidence: 0.7,
		MinLift:       1.2,
		MinSamples:    20,
		HourBuckets:   4,
		ExceptionDrop: 0.25,
	}
}

func (p RuleParams) withDefaults() RuleParams {
	defaults := DefaultRuleParams()
	if p.MinSupport <= 0 {
		p.MinSupport = defaults.MinSupport
	}
	if p.MinConfidence <= 0 {
		p.MinConfidence = defaults.MinConfidence
	}
	if p.MinLift <= 0 {
		p.MinLift = defaults.MinLift
	}
	if p.MinSamples <= 0 {
		p.MinSamples = defaults.MinSamples
	}
	if p.HourBuckets <= 0 {
		p.HourBuckets = defaults.HourBuckets
	}
	if p.HourBuckets > 24 {
		p.HourBuckets = 24
	}
	if p.ExceptionDrop <= 0 {
		p.ExceptionDrop = defaults.ExceptionDrop
	}
	return p
}

// Conditioning says which dimensions a relation is conditioned on (§5.5).
//
// Both are on by default. The exception window in the motivating sentence — "except
// at certain times of day" — is exactly a conditioned contingency, so a relation
// computed without conditioning cannot express the case it exists for.
type Conditioning struct {
	HourOfDay      bool `json:"hour_of_day"`
	WeekdayWeekend bool `json:"weekday_weekend"`
}

// DefaultConditioning conditions on both dimensions.
func DefaultConditioning() Conditioning {
	return Conditioning{HourOfDay: true, WeekdayWeekend: true}
}

// The conditioning dimensions, as they appear in an exception.
const (
	DimensionHourOfDay      = "hour_of_day"
	DimensionWeekdayWeekend = "weekday_weekend"
)

// Member is one participant of a computed relation, with what was learned about
// it on the way.
type Member struct {
	Ref   profiler.SeriesRef `json:"ref"`
	Label string             `json:"label"`

	DeviceName  string `json:"device_name,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
	AspectID    string `json:"aspect_id,omitempty"`
	AspectName  string `json:"aspect_name,omitempty"`

	// ProfileID is the SeriesProfile the state series was derived from, so a
	// reader can go and look at the threshold rather than take it on trust.
	ProfileID string `json:"profile_id"`
	// Unit and Kind are carried because a rule about a cumulative counter is a rule
	// about its rate, and that distinction has to be visible where the rule is read.
	Unit string             `json:"unit"`
	Kind profiler.ValueKind `json:"kind"`

	// State is how idle and active were decided for this member, including whether
	// the threshold was the detector's or the developer's.
	State StateSummary `json:"state"`
}

// StateSummary is the derivation of one member's state series, as a document.
type StateSummary struct {
	// Usable is false when no state series could be derived. The reason says why,
	// and the member then takes part in no rule rather than contributing a series
	// of silently-idle buckets.
	Usable bool `json:"usable"`
	// Reason is a pointer because omitempty does not apply to a struct: as a value it
	// would put an empty reason object beside every *usable* member, which is exactly
	// the "there is a reason and it is blank" reading D24 exists to prevent. Absent
	// means there is nothing to explain, and Usable is what says so.
	Reason *profiler.NotComputed `json:"reason,omitempty"`
	Method string                `json:"method"`
	// Threshold is the value that separates idle from active, in the member's own
	// unit. Meaningless for a categorical member, which reports method "distinct".
	Threshold float64 `json:"threshold"`
	// ThresholdSource is `detector` or `confirmed`. §5.10's whole argument rests on
	// the second being visible: a rule computed against a threshold the developer
	// corrected is a different claim from one computed against a guess.
	ThresholdSource string                          `json:"threshold_source"`
	Classification  profiler.ActivityClassification `json:"classification"`
	// ActiveBuckets, IdleBuckets and UnknownBuckets are the state series as three
	// numbers, which is what makes a lift figure checkable by hand.
	ActiveBuckets  int     `json:"active_buckets"`
	IdleBuckets    int     `json:"idle_buckets"`
	UnknownBuckets int     `json:"unknown_buckets"`
	DutyCycle      float64 `json:"duty_cycle"`
	// ObservedBuckets is how many aligned buckets carried a value for this member, and
	// it is populated whether or not a state series could be derived.
	//
	// It is the one number that separates the two ways a member fails, and without it
	// they look identical: an unusable member reports 0 active, 0 idle and every bucket
	// unknown either because the read came back empty or because it came back full and
	// no idle/active split could be found in it. Those have different causes and
	// different fixes, and "1 aligned read" says the read happened, not what it
	// returned.
	ObservedBuckets int `json:"observed_buckets"`
}

// StateSeries is one member's states over the aligned grid.
//
// States is parallel to AlignedFrame.Times: index i of every state series and of
// Times describe the same bucket. That is the property Relate depends on and the
// reason alignment is a request-level concern rather than a post-processing step.
type StateSeries struct {
	Member  Member  `json:"member"`
	States  []State `json:"states"`
	Summary StateSummary
}

// RelationProfile is what one relational pass produced (§5.5).
type RelationProfile struct {
	RelationID      string `json:"relation_id"`
	DetectorVersion string `json:"detector_version"`
	// CacheKey is the relation id, for the same reason the profiler's is: a caller
	// holding a relation can check it for staleness without a second index.
	CacheKey   string    `json:"cache_key"`
	ComputedAt time.Time `json:"computed_at"`
	// Tier is the lowest exposure tier at which this document may be shown to a
	// model. It is L1 throughout — contingency counts and durations are aggregates,
	// and no value of any series appears here.
	Tier string `json:"tier"`

	Window      profiler.Window `json:"window"`
	GroupTime   string          `json:"group_time"`
	GridSeconds float64         `json:"grid_seconds"`
	// Buckets is the length of the aligned grid, and Observed how many buckets had
	// a reading from every usable member. A rule's support is a share of Observed,
	// not of Buckets, and stating both is what keeps that legible.
	Buckets  int `json:"buckets"`
	Observed int `json:"observed"`

	Members      []Member     `json:"members"`
	Params       RuleParams   `json:"params"`
	Conditioning Conditioning `json:"conditioning"`

	Pairs          []PairRelation  `json:"pairs"`
	CandidateRules []CandidateRule `json:"candidate_rules"`

	// CandidateSetID names the proposal this pass came from, when it came from one.
	// It is how a confirmed rule can be traced back to the aspect that suggested
	// the devices.
	CandidateSetID string `json:"candidate_set_id,omitempty"`

	Reads Reads    `json:"reads"`
	Notes []string `json:"notes"`
}

// Reads is what one relational pass asked of the platform.
//
// Aligned is the number of batched queries this package issued itself — one, by
// construction (§5.5), and a figure above one means the batching property has
// been lost. Profiles counts the profile passes it delegated, which read values
// too; they are separated because only the first is this package's own claim.
type Reads struct {
	Aligned  int `json:"aligned"`
	Profiles int `json:"profiles"`
	// Devices is the metadata reads: one per service a pass profiles, and one per
	// graph neighbour a proposal resolves from outside the requested aspect. Counted
	// separately from Values because a device read is metadata and does not touch the
	// tier — but counted, because a proposal that quietly made twelve of them would
	// otherwise look free.
	Devices int `json:"devices"`
	// Values totals the value-reading passes, so a caller comparing this against a
	// QuickProfile's zero has one number to compare.
	Values int `json:"values"`
}

// PairRelation is the contingency between two members, overall and per condition.
type PairRelation struct {
	// A and B index RelationProfile.Members. Indices rather than references because
	// the same series may legitimately appear twice under different labels, and a
	// reference would then be ambiguous.
	A int `json:"a"`
	B int `json:"b"`
	// Overall is the joint distribution over every bucket both members were
	// observed in.
	Overall Contingency `json:"overall"`
	// Conditions is Overall split by the conditioning dimensions.
	Conditions []ConditionedContingency `json:"conditions"`
}

// Contingency is the 2×2 table for a pair of members, plus what it implies.
//
// The four counts are carried rather than only the derived figures because they
// are the evidence: a confidence of 1.0 over three samples and one over three
// thousand are the same number and not the same finding, and D23's argument about
// false precision applies to a ratio just as much as to a self-assessed score.
type Contingency struct {
	// The counts, named by the state of A then of B.
	ActiveActive int `json:"active_active"`
	ActiveIdle   int `json:"active_idle"`
	IdleActive   int `json:"idle_active"`
	IdleIdle     int `json:"idle_idle"`
	// Observed is the sum of the four, which is the number of buckets in which both
	// members had a reading.
	Observed int `json:"observed"`
	// ActiveRateA and ActiveRateB are the marginals, and they are what a lift is
	// measured against.
	ActiveRateA float64 `json:"active_rate_a"`
	ActiveRateB float64 `json:"active_rate_b"`
}

// ConditionedContingency is one slice of a pair's joint distribution.
type ConditionedContingency struct {
	Dimension string `json:"dimension"`
	// Bucket is the condition in a form a person reads: "06:00-12:00", "weekend".
	Bucket      string      `json:"bucket"`
	Contingency Contingency `json:"contingency"`
}

// RuleTerm is one side of a candidate rule.
type RuleTerm struct {
	// Member indexes RelationProfile.Members.
	Member int    `json:"member"`
	Label  string `json:"label"`
	State  State  `json:"state"`
}

// CandidateRule is a pattern worth a developer's attention (§5.5).
//
// It is not an anomaly definition and not a feature. It becomes either of those
// only when the developer confirms it, which is the same boundary D28 draws around
// recommendations: the detector proposes, the developer decides, and the promotion
// is recorded.
type CandidateRule struct {
	// RuleID is a fingerprint of what the rule *says* — the two members, their
	// states, and the direction. It deliberately excludes the window, the grid and
	// the detector version, so a decision made today still applies to the same rule
	// recomputed next month over a different window (D21).
	RuleID string `json:"rule_id"`

	Antecedent RuleTerm `json:"antecedent"`
	Consequent RuleTerm `json:"consequent"`

	// Statement is the rule in words, and Anomaly is its violation in words. Both
	// are generated rather than templated at the call site, because they are what
	// appears beside the confirm button and the two have to agree.
	Statement string `json:"statement"`
	Anomaly   string `json:"anomaly"`

	// Support, Confidence and Lift are the association statistics §5.5 names. They
	// are ratios with definitions, unlike Strength below.
	Support    float64 `json:"support"`
	Confidence float64 `json:"confidence"`
	Lift       float64 `json:"lift"`
	// Samples is how many buckets the antecedent held in, and Violations how many
	// of those the consequent failed in. Violations is the count of the anomaly the
	// rule defines, which is the figure a developer judges it by.
	Samples    int `json:"samples"`
	Violations int `json:"violations"`

	// Strength is this detector's own certainty, and it is ordinal because D23
	// forbids inventing a probability for it. It is not the confidence above: that
	// is P(consequent | antecedent), a statistic; this is how much weight the
	// detector puts on the finding given how much evidence there is.
	Strength profiler.Confidence `json:"strength"`

	// Exceptions are the conditions under which the rule does not hold — the
	// "except at certain times of day" of the motivating case. Empty means it held
	// in every condition examined, which is a finding rather than a missing field.
	Exceptions []Exception `json:"exceptions"`

	// Decision is the developer's, re-injected from the append-only log when the
	// same rule is computed again. Nil means nobody has decided yet.
	Decision *RuleDecision `json:"decision,omitempty"`

	// Advisory states the boundary in the document itself, so a model reading a
	// projection of this cannot mistake a candidate for a configured rule.
	Advisory string `json:"advisory"`
}

// Exception is a condition in which a rule demonstrably does not hold.
type Exception struct {
	Dimension string `json:"dimension"`
	Bucket    string `json:"bucket"`
	// FromHour and ToHour bound an hour-of-day exception, half-open, in UTC — the
	// machine-readable form of Bucket. Both are zero for a weekday/weekend
	// exception, which Dimension already identifies.
	FromHour int `json:"from_hour,omitempty"`
	ToHour   int `json:"to_hour,omitempty"`

	Samples    int     `json:"samples"`
	Confidence float64 `json:"confidence"`
	// Drop is how far below the rule's overall confidence this condition fell. It
	// is the reason the exception was raised and belongs beside it.
	Drop float64 `json:"drop"`
}

// advisoryNote is stamped on every candidate rule. Wording matters here: it is
// read by a model that would otherwise be inclined to act on a rule it found.
const advisoryNote = "candidate only: not a configured rule, not an anomaly definition, " +
	"and never read by an operator or a training job until a developer confirms it (§5.5, D28)"

// notComputedStatus is the status string profiler.NotComputed carries. Restated
// rather than imported because the profiler keeps it unexported, and a hand-built
// non-result has to marshal identically to a detector's own (D24).
const notComputedStatus = "not_computed"

// labelSuffixes chooses how to tell apart a group of members whose labels collide.
//
// options[i] holds member i's discriminators, most readable first, and every member
// supplies the same number of them. The first position whose values are distinct
// across the whole group and non-empty everywhere wins. If none qualifies the last
// position is used regardless, because it is the one guaranteed unique — a label that
// still collides is worse than an ugly one, since a rule naming neither of two
// members cannot be confirmed (§5.10).
func labelSuffixes(options [][]string) []string {
	if len(options) == 0 {
		return nil
	}
	depth := len(options[0])
	for position := 0; position < depth; position++ {
		seen := map[string]bool{}
		usable := true
		for _, candidate := range options {
			value := candidate[position]
			if value == "" || seen[value] {
				usable = false
				break
			}
			seen[value] = true
		}
		if !usable {
			continue
		}
		out := make([]string, 0, len(options))
		for _, candidate := range options {
			out = append(out, candidate[position])
		}
		return out
	}

	out := make([]string, 0, len(options))
	for _, candidate := range options {
		out = append(out, candidate[depth-1])
	}
	return out
}
