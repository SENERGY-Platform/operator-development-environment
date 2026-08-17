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

// Field paths, used as keys for both provenance (D22) and overrides (D21). They
// are constants so the two cannot drift: an override written against
// "value_semantics.unit" has to reach the same field the provenance entry
// describes.
const (
	FieldCoverage         = "coverage"
	FieldSamplingGaps     = "sampling.gaps"
	FieldSamplingInterval = "sampling.detected_interval_s"
	FieldValueKind        = "value_semantics.kind"
	FieldUnit             = "value_semantics.unit"
	FieldCharacteristic   = "value_semantics.characteristic_id"
	FieldDeclaredRange    = "value_semantics.declared_range"
	FieldCounterResets    = "value_semantics.counter_resets"
	FieldRangeViolation   = "value_semantics.range_violation_ratio"
	FieldDistribution     = "distribution"
	FieldConstantRuns     = "distribution.constant_runs"
	FieldPeriods          = "temporal_structure.dominant_periods_s"
	FieldTrend            = "temporal_structure.trend"
	FieldStationarity     = "temporal_structure.stationarity"
	FieldActivity         = "activity_pattern"
	FieldActivityClass    = "activity_pattern.classification"
	FieldActiveThreshold  = "activity_pattern.active_threshold"
	FieldSessions         = "activity_pattern.sessions"
	FieldSessionStats     = "activity_pattern.session_stats"
	FieldQualityFlags     = "quality_flags"
	FieldRelationships    = "service_context.relationships"
	FieldUsableRange      = "recommendations.usable_range"
	FieldExclusions       = "recommendations.exclusions"
	FieldResampleTo       = "recommendations.resample_to_s"
)

// ReadMode records which pass of the two-pass strategy (§5.3.2) produced a
// field. It matters because aggregated reads hide exactly what the structural
// detectors look for: with groupTime set, gaps are filled and sampling
// irregularity disappears. A field's read mode is how a reader knows whether to
// trust it on that point.
type ReadMode string

const (
	ReadRaw        ReadMode = "raw"
	ReadAggregated ReadMode = "aggregated"
	ReadNone       ReadMode = "none"
)

type Source string

const (
	SourceOntology  Source = "ontology"
	SourceDetector  Source = "detector"
	SourceInference Source = "inference"
	SourceDeveloper Source = "developer"
	SourceAPI       Source = "api"
)

type ProvenanceEntry struct {
	ReadMode  ReadMode `json:"read_mode"`
	Source    Source   `json:"source"`
	Detector  string   `json:"detector,omitempty"`
	Ref       string   `json:"ref,omitempty"`
	Window    *Window  `json:"window,omitempty"`
	GroupTime string   `json:"group_time,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// Provenance is a sidecar keyed by dotted field path (D22) rather than a
// per-field envelope, which keeps the profile body flat enough to read. The
// projection for the LLM drops it entirely.
type Provenance map[string]ProvenanceEntry

func (p Provenance) Set(path string, entry ProvenanceEntry) {
	if p == nil {
		return
	}
	p[path] = entry
}

// FromOntology records a field the ontology answered, with the entity it came
// from. No read was involved, so no window applies.
func (p Provenance) FromOntology(path string, ref string) {
	p.Set(path, ProvenanceEntry{ReadMode: ReadNone, Source: SourceOntology, Ref: ref})
}

// FromRaw records a field the bounded raw pass produced.
func (p Provenance) FromRaw(path string, detector string, window Window) {
	w := window
	p.Set(path, ProvenanceEntry{ReadMode: ReadRaw, Source: SourceDetector, Detector: detector, Window: &w})
}

// FromAggregated records a field the full-range aggregated pass produced,
// including the bucket width, because a period shorter than the bucket cannot
// have been detected from it.
func (p Provenance) FromAggregated(path string, detector string, window Window, groupTime string) {
	w := window
	p.Set(path, ProvenanceEntry{
		ReadMode: ReadAggregated, Source: SourceDetector, Detector: detector,
		Window: &w, GroupTime: groupTime,
	})
}

// FromInference records a field no source could answer and a heuristic guessed.
func (p Provenance) FromInference(path string, detector string, note string) {
	p.Set(path, ProvenanceEntry{ReadMode: ReadNone, Source: SourceInference, Detector: detector, Note: note})
}

// FromAPI records a field a platform endpoint answered directly, without a
// detector in between — the availability window, say, or a usage figure.
func (p Provenance) FromAPI(path string, ref string) {
	p.Set(path, ProvenanceEntry{ReadMode: ReadNone, Source: SourceAPI, Ref: ref})
}
