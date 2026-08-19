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

// Package profiler computes deterministic profiles of platform series
// (SPEC §5.4). It is the component the core design rule of §4 rests on: the
// profiler computes statistics, the LLM reads a profile and interprets it, and
// raw series never enter an LLM context.
//
// Nothing here talks to an LLM, and nothing here needs one to be tested. Every
// detector is a function from a series to a profile field, verified against
// fixtures with known answers rather than against the platform (§5.4.14).
package profiler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DetectorVersion is part of both the profile body and the cache key (D25).
// Bump it whenever a detector's output can change for unchanged input;
// otherwise improving a detector leaves stale profiles in the LLM's context
// with no way to notice.
//
// It is deliberately a constant rather than configuration: a deployment that
// could set it would be able to claim a detector version it is not running.
const DetectorVersion = "1.0.0"

// SeriesRef is the addressable unit (D19). Not {device_id, service_id}: a
// service output is a ContentVariable tree and timescale-wrapper addresses its
// leaves individually, so a profile per service would mix unrelated variables.
type SeriesRef struct {
	DeviceID     string `json:"device_id"`
	ServiceID    string `json:"service_id"`
	VariablePath string `json:"variable_path"`
}

func (r SeriesRef) String() string {
	return r.DeviceID + "|" + r.ServiceID + "|" + r.VariablePath
}

func (r SeriesRef) Valid() bool {
	return r.DeviceID != "" && r.ServiceID != "" && r.VariablePath != ""
}

// Window is a half-open time range, always in UTC. Time handling is not
// optional in this domain (§5.4.13): store and compute in UTC, display local,
// and flag DST transitions explicitly.
type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func (w Window) Duration() time.Duration { return w.To.Sub(w.From) }

func (w Window) SpanDays() float64 { return w.Duration().Hours() / 24 }

func (w Window) Valid() bool { return !w.From.IsZero() && !w.To.IsZero() && w.To.After(w.From) }

func (w Window) String() string {
	return w.From.UTC().Format(time.RFC3339) + "/" + w.To.UTC().Format(time.RFC3339)
}

// WindowSource records whether a window is the default or something the
// developer asked for (D25). A profile computed over an unusual window must not
// be mistaken for a default one.
type WindowSource string

const (
	WindowDefault           WindowSource = "default"
	WindowDeveloperOverride WindowSource = "developer_override"
)

// RawWindow is the bounded subset read raw for structural detection.
type RawWindow struct {
	Window
	Source WindowSource `json:"source"`
	// Truncated says the point limit cut the window short, in which case From
	// is the oldest point actually read rather than the one requested. Without
	// this the gap detector would report a gap that is only a missing read.
	Truncated bool `json:"truncated"`
}

// --- profile body (§5.4.12) ---

type SeriesProfile struct {
	ProfileID       string    `json:"profile_id"`
	Tier            string    `json:"tier"`
	SeriesRef       SeriesRef `json:"series_ref"`
	DetectorVersion string    `json:"detector_version"`
	AnalysisWindow  Window    `json:"analysis_window"`
	RawWindow       RawWindow `json:"raw_window"`
	ComputedAt      time.Time `json:"computed_at"`
	// CacheKey is carried in the body as well as being the store's key, so a
	// profile handed around outside the store can still be checked for staleness.
	CacheKey string `json:"cache_key"`

	ServiceContext ServiceContext `json:"service_context"`

	// ReadSummary says what the two passes actually returned for this variable.
	//
	// It exists because a profile full of not_computed is otherwise a puzzle: the
	// fields report their own reason honestly, but "no sampling interval" is the
	// symptom, and the cause — nothing came back, or rows came back with no value
	// in this column, or there is no raw data to read at all — lives one level
	// down where nobody can see it. This is the one block that is always populated.
	ReadSummary ReadSummary `json:"read_summary"`

	Coverage          Value[Coverage]        `json:"coverage"`
	Sampling          Value[Sampling]        `json:"sampling"`
	ValueSemantics    ValueSemantics         `json:"value_semantics"`
	Distribution      Value[Distribution]    `json:"distribution"`
	TemporalStructure TemporalStructure      `json:"temporal_structure"`
	ActivityPattern   Value[ActivityPattern] `json:"activity_pattern"`
	QualityFlags      []QualityFlag          `json:"quality_flags"`
	Recommendations   Recommendations        `json:"recommendations"`

	Provenance Provenance `json:"provenance"`
}

const TierFull = "full"
const TierQuick = "quick"

// ReadSummary is what the platform returned, as opposed to what the detectors
// made of it.
type ReadSummary struct {
	// RawAvailable is what /data-availability says: false means the platform holds
	// only aggregated buckets for this service, so there is no unbucketed data to
	// read and every structural detector is dead on arrival. Retention causes this,
	// and it is the first thing to check on an empty profile.
	//
	// A Value rather than a bool because the endpoint can fail, and a bool cannot
	// tell "the platform says there is no raw window" from "the platform could not
	// be asked". That is precisely the distinction D24 exists to keep: read as a
	// false, an unanswered probe would send a reader looking for retention that is
	// not the cause. When it is not computed the reason is read_failed and the
	// detail is the platform's own error.
	RawAvailable Value[bool] `json:"raw_available"`
	// RawRows is how many rows the raw pass returned for the whole service.
	RawRows int `json:"raw_rows"`
	// ValuesPresent is how many of those rows carried a value for *this* variable,
	// and NullRows how many did not. A service can report while one of its channels
	// stays silent, which looks identical to a dead read from inside a single
	// variable's profile.
	ValuesPresent int `json:"values_present"`
	NullRows      int `json:"null_rows"`
	// AggregatedBuckets is how many buckets the aggregated pass returned.
	AggregatedBuckets int `json:"aggregated_buckets"`
	// Diagnosis names the cause when there is one to name, in the terms a reader
	// can act on. Empty when both passes returned data.
	Diagnosis string `json:"diagnosis,omitempty"`
}

// Diagnose explains an empty or thin read. The wording is deliberately concrete:
// the point is to answer "why is this profile empty" without a debugger.
//
// numeric matters because the aggregated pass deliberately skips non-numeric
// variables — there is no mean of a status string — and reporting that as a
// failure would send a developer looking for a fault that is a design decision.
func (r ReadSummary) Diagnose(queryable bool, numeric bool, reason string) string {
	rawAvailable, rawAvailabilityKnown := r.RawAvailable.Get()
	switch {
	case !queryable:
		return "this variable is not readable as a scalar series: " + reason
	case !rawAvailabilityKnown && r.RawRows == 0 && r.AggregatedBuckets == 0:
		// The one case that must not be reported as retention. Both passes came back
		// empty and the availability probe failed, so which of the two is the cause
		// is unknown — and naming retention here would be the negation-for-absence
		// mistake D24 is about.
		return "both passes returned no rows, and the platform could not say whether this " +
			"service has a raw window at all: " + r.RawAvailable.Status().Detail +
			". The requested window may not overlap the stored data, or the read may have failed"
	case !numeric && r.AggregatedBuckets == 0 && r.ValuesPresent > 0:
		return "the aggregated pass skips this variable because it is not numeric, so the " +
			"distribution and the temporal detectors do not apply. The structural detectors " +
			"read the raw pass and did run"
	case rawAvailabilityKnown && !rawAvailable && r.RawRows == 0:
		return "the platform reports no raw window for this service, only aggregated buckets. " +
			"Retention has aged the unbucketed data out, and the structural detectors need it — " +
			"sampling, gaps, value kind, counter resets and sessions cannot be computed from buckets"
	case r.RawRows == 0 && r.AggregatedBuckets == 0:
		return "both passes returned no rows for the requested windows, although the platform " +
			"reported a data window for this service. Check that the analysis window overlaps " +
			"the data and that the variable path matches a stored column"
	case r.RawRows == 0:
		return "the raw pass returned no rows while the aggregated pass returned " +
			itoa(r.AggregatedBuckets) + ". The structural detectors have nothing to work from; " +
			"the statistical ones do"
	case r.ValuesPresent == 0:
		return "the raw pass returned " + itoa(r.RawRows) +
			" rows for the service and none of them carried a value for this variable. " +
			"The service is reporting and this channel is not, which is a dead channel rather " +
			"than a failed read"
	case r.AggregatedBuckets == 0:
		return "the aggregated pass returned no buckets, so the statistical detectors could not run. " +
			"The structural ones read the raw pass and are unaffected"
	default:
		return ""
	}
}

// ServiceContext carries what the service-scoped batch reveals that no single
// variable does (§5.4.1). The motivating check: an energy meter that emits
// instantaneous power and a cumulative counter on the same service, where
// diff(counter) against integrated power catches unit errors and dead channels.
type ServiceContext struct {
	ServiceID        string            `json:"service_id"`
	Interaction      string            `json:"interaction"`
	SiblingVariables []SiblingVariable `json:"sibling_variables"`
	Relationships    []Relationship    `json:"relationships"`
}

type SiblingVariable struct {
	Path             string `json:"path"`
	CharacteristicID string `json:"characteristic_id"`
	Kind             string `json:"kind"`
}

type RelationshipType string

const (
	RelationIntegralOf       RelationshipType = "integral_of"
	RelationDerivativeOf     RelationshipType = "derivative_of"
	RelationRedundantWith    RelationshipType = "redundant_with"
	RelationInconsistentWith RelationshipType = "inconsistent_with"
)

type Relationship struct {
	Type       RelationshipType     `json:"type"`
	OtherPath  string               `json:"other_path"`
	Evidence   RelationshipEvidence `json:"evidence"`
	Confidence Confidence           `json:"confidence"`
}

type RelationshipEvidence struct {
	Correlation   float64 `json:"correlation"`
	ResidualRatio float64 `json:"residual_ratio"`
	OverlapPoints int     `json:"overlap_points"`
	// ImpliedScale is the factor that best maps the other series onto this one.
	// For a counter against a rate it is the unit ratio, so a value near 3600
	// says watt-hours against watts and a value near 1 says watt-seconds — which
	// is how a unit error shows up as a number rather than as a failed check.
	ImpliedScale float64 `json:"implied_scale"`
}

type Coverage struct {
	NPoints           int     `json:"n_points"`
	ExpectedPoints    int     `json:"expected_points"`
	CompletenessRatio float64 `json:"completeness_ratio"`
}

type Regularity string

const (
	Regular   Regularity = "regular"
	Irregular Regularity = "irregular"
	Mixed     Regularity = "mixed"
)

type GapClassification string

const (
	GapDeviceOffline GapClassification = "device_offline"
	GapSensorFault   GapClassification = "sensor_fault"
	GapIngestionGap  GapClassification = "ingestion_gap"
	GapUnknown       GapClassification = "unknown"
)

type Gap struct {
	From           time.Time         `json:"from"`
	To             time.Time         `json:"to"`
	DurationS      float64           `json:"duration_s"`
	Classification GapClassification `json:"classification"`
}

type Sampling struct {
	DetectedIntervalS float64    `json:"detected_interval_s"`
	Regularity        Regularity `json:"regularity"`
	Confidence        Confidence `json:"confidence"`
	// IrregularityRatio is the share of inter-arrival deltas that miss the
	// modal interval, and is the evidence behind Regularity (D23).
	IrregularityRatio float64 `json:"irregularity_ratio"`
	Gaps              []Gap   `json:"gaps"`
}

type ValueKind string

const (
	KindInstantaneous     ValueKind = "instantaneous"
	KindCumulativeCounter ValueKind = "cumulative_counter"
	KindBinary            ValueKind = "binary"
	KindCategorical       ValueKind = "categorical"
	KindStatus            ValueKind = "status"
)

type KindEvidence struct {
	MonotonicRatio  float64 `json:"monotonic_ratio"`
	DistinctValues  int     `json:"distinct_values"`
	NegativeDeltas  int     `json:"negative_deltas"`
	NonNumericRatio float64 `json:"non_numeric_ratio"`
}

type UnitSource string

const (
	UnitFromCharacteristic UnitSource = "characteristic"
	UnitFromUnitReference  UnitSource = "unit_reference"
	UnitInferred           UnitSource = "inferred"
	UnitUnknown            UnitSource = "unknown"
	UnitConflict           UnitSource = "conflict"
)

type DeclaredRange struct {
	Min Value[float64] `json:"min"`
	Max Value[float64] `json:"max"`
}

type Conversion struct {
	ToCharacteristicID string `json:"to_characteristic_id"`
	ToUnit             string `json:"to_unit"`
	Distance           int64  `json:"distance"`
}

// ValueSemantics mixes two sources deliberately. Unit, characteristic and range
// come from the ontology and need no read, so they are present even when the
// read failed; kind, resets and range violations come from the raw pass and
// carry not_computed when it did not happen.
type ValueSemantics struct {
	Kind           Value[ValueKind]    `json:"kind"`
	KindConfidence Value[Confidence]   `json:"kind_confidence"`
	KindEvidence   Value[KindEvidence] `json:"kind_evidence"`
	// CharacteristicID is canonical and authoritative; Unit is derived from it
	// and advisory (D29). It is a pointer because null is a legitimate value
	// where the unit was inferred, and fabricating an id here would silently
	// enable a wrong server-side conversion.
	CharacteristicID     *string            `json:"characteristic_id"`
	Unit                 string             `json:"unit"`
	UnitSource           UnitSource         `json:"unit_source"`
	DeclaredRange        DeclaredRange      `json:"declared_range"`
	RangeViolationRatio  Value[float64]     `json:"range_violation_ratio"`
	CounterResets        Value[[]time.Time] `json:"counter_resets"`
	AvailableConversions []Conversion       `json:"available_conversions"`
}

type ConstantRun struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Value     float64   `json:"value"`
	DurationS float64   `json:"duration_s"`
	Points    int       `json:"points"`
}

type Distribution struct {
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Mean      float64 `json:"mean"`
	Median    float64 `json:"median"`
	P01       float64 `json:"p01"`
	P99       float64 `json:"p99"`
	StdDev    float64 `json:"std_dev"`
	ZeroRatio float64 `json:"zero_ratio"`
	// ConstantRuns is unbounded and is what the projection collapses (D26).
	ConstantRuns []ConstantRun `json:"constant_runs"`
}

type PeriodEvidence struct {
	PeriodS  float64 `json:"period_s"`
	Method   string  `json:"method"`
	Strength float64 `json:"strength"`
	Label    string  `json:"label"`
}

type Trend struct {
	// Slope is per second, in the series' own unit. SlopePerDay is the same
	// number in the scale a reader can judge.
	Slope       float64 `json:"slope"`
	SlopePerDay float64 `json:"slope_per_day"`
	R2          float64 `json:"r2"`
	Significant bool    `json:"significant"`
	TStat       float64 `json:"t_stat"`
}

// PValueBracket brackets the ADF p-value between two published quantiles
// instead of interpolating a point estimate.
//
// SPEC §5.4.12 asks for `adf_p`. Producing one needs MacKinnon's p-value
// response surface, which is a table this implementation does not have; a
// bracket read off the critical values it does have is exact, and §5.4.14 is
// explicit that faking the number is not an option. Lower 0 means "below the
// tightest quantile tested", Upper 1 means "above the loosest".
type PValueBracket struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
	Note  string  `json:"note"`
}

type Stationarity struct {
	ADFStat        float64            `json:"adf_stat"`
	LagOrder       int                `json:"lag_order"`
	NObs           int                `json:"n_obs"`
	CriticalValues map[string]float64 `json:"critical_values"`
	PValueBracket  PValueBracket      `json:"p_value_bracket"`
	Stationary     bool               `json:"stationary"`
	Confidence     Confidence         `json:"confidence"`
	Regression     string             `json:"regression"`
}

type TemporalStructure struct {
	DominantPeriodsS Value[[]float64]        `json:"dominant_periods_s"`
	PeriodEvidence   Value[[]PeriodEvidence] `json:"period_evidence"`
	Trend            Value[Trend]            `json:"trend"`
	Stationarity     Value[Stationarity]     `json:"stationarity"`
}

type ActivityClassification string

const (
	ActivityContinuous   ActivityClassification = "continuous"
	ActivitySessionBased ActivityClassification = "session_based"
	ActivityIntermittent ActivityClassification = "intermittent"
	ActivityStatus       ActivityClassification = "status"
)

type SessionStats struct {
	Count               int     `json:"count"`
	MedianDurationS     float64 `json:"median_duration_s"`
	InterArrivalMedianS float64 `json:"inter_arrival_median_s"`
	MedianEnergy        float64 `json:"median_energy"`
}

type SessionExemplar struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	DurationS float64   `json:"duration_s"`
	Energy    float64   `json:"energy"`
	Peak      float64   `json:"peak"`
}

type ActivityPattern struct {
	Classification           ActivityClassification `json:"classification"`
	ClassificationConfidence Confidence             `json:"classification_confidence"`
	IdleLevel                float64                `json:"idle_level"`
	ActiveThreshold          float64                `json:"active_threshold"`
	// ThresholdMethod and ThresholdParams are what make the classification
	// adjustable rather than opaque (§5.4.13 item 7).
	ThresholdMethod  string              `json:"threshold_method"`
	ThresholdParams  SessionParams       `json:"threshold_params"`
	SessionStats     Value[SessionStats] `json:"session_stats"`
	SessionExemplars []SessionExemplar   `json:"session_exemplars"`
	SessionsRef      string              `json:"sessions_ref"`
}

// SessionParams are the session detector's inputs, recorded so a developer can
// see what produced a boundary before confirming it (§5.10).
type SessionParams struct {
	MinDurationS   float64 `json:"min_duration_s"`
	MergeGapS      float64 `json:"merge_gap_s"`
	HysteresisFrac float64 `json:"hysteresis_frac"`
}

// Session is a detected active period. Sessions are a separate paginated
// resource (D27) because a washing machine over two years produces thousands,
// and the profile carries only statistics, exemplars and a reference.
type Session struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	DurationS float64   `json:"duration_s"`
	Energy    float64   `json:"energy"`
	Peak      float64   `json:"peak"`
	Points    int       `json:"points"`
}

const (
	FlagFrozenSensor        = "frozen_sensor"
	FlagNegativeOnUnsigned  = "negative_on_unsigned"
	FlagRangeViolation      = "range_violation"
	FlagDSTAmbiguity        = "dst_ambiguity"
	FlagUnflaggedReset      = "unflagged_counter_reset"
	FlagNotStreamed         = "not_streamed"
	FlagSparseCoverage      = "sparse_coverage"
	FlagInconsistentSibling = "inconsistent_with_sibling"
)

type QualityFlag struct {
	Flag       string         `json:"flag"`
	Confidence Confidence     `json:"confidence"`
	Evidence   map[string]any `json:"evidence"`
}

type Exclusion struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

// Recommendations are strictly advisory (D28). Nothing downstream reads them:
// they become binding only when a developer promotes them explicitly, and the
// promotion is recorded. A threshold heuristic setting the resampling policy
// with nobody deciding is the autonomous behaviour this design rejects.
type Recommendations struct {
	Advisory              bool           `json:"advisory"`
	ResampleToS           Value[float64] `json:"resample_to_s"`
	InterpolationStrategy Value[string]  `json:"interpolation_strategy"`
	UsableRange           Value[Window]  `json:"usable_range"`
	Exclusions            []Exclusion    `json:"exclusions"`
}

const (
	InterpolationNone   = "none"
	InterpolationLinear = "linear"
	InterpolationFFill  = "ffill"
)

// SessionsPath is where the paginated session resource lives for a profile.
func SessionsPath(profileID string) string {
	return fmt.Sprintf("/profiles/%s/sessions", profileID)
}

// FormatSeconds renders a period in the way the LLM view and the UI both want
// it: the number plus a human-legible label for the well-known cycles.
func FormatSeconds(seconds float64) string {
	switch {
	case seconds == 86400:
		return "daily"
	case seconds == 604800:
		return "weekly"
	case seconds == 3600:
		return "hourly"
	case seconds >= 86400:
		return fmt.Sprintf("%.1fd", seconds/86400)
	case seconds >= 3600:
		return fmt.Sprintf("%.1fh", seconds/3600)
	case seconds >= 60:
		return fmt.Sprintf("%.1fm", seconds/60)
	default:
		return fmt.Sprintf("%.0fs", seconds)
	}
}

// labelPeriod names a detected period when it is within tolerance of a cycle
// worth reporting explicitly (§5.4.13 item 6 asks for daily and weekly by name).
func labelPeriod(seconds float64) string {
	for _, known := range []struct {
		label   string
		seconds float64
	}{
		{"daily", 86400},
		{"weekly", 604800},
		{"hourly", 3600},
		{"half-daily", 43200},
	} {
		if seconds > known.seconds*0.9 && seconds < known.seconds*1.1 {
			return known.label
		}
	}
	return ""
}

// pathKey builds a dotted provenance and override key, so the two agree on how
// a field is named.
func pathKey(parts ...string) string { return strings.Join(parts, ".") }

func itoa(value int) string { return strconv.Itoa(value) }
