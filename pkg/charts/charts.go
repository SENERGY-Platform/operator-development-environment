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

// Package charts is the backend half of the exploration pane (SPEC §5.9, §5.10,
// M5).
//
// Three things it is built around, and each is a decision rather than a detail.
//
// **The specification is declarative and the frontend renders it.** §5.9 is
// explicit that the LLM emits a spec and never an image. So nothing here draws
// anything: a chart is a document naming series, transforms, annotations and an
// axis, and this package validates it, resolves what it names against the
// ontology, and reads the values it needs.
//
// **The values never pass through an LLM context.** render_chart sits at tier L1
// (§5.8) although a chart plainly shows values, and that is consistent rather
// than sloppy: the model emits the spec, the *developer's browser* asks for the
// data with the developer's own token, and the model is told the chart id and the
// resolved axis and nothing else. A model at L1 can therefore demonstrate a
// selection visually without seeing a single value — which is the property M5's
// acceptance criterion is really about.
//
// **Transforms are server-side.** §5.3.1 removes four things ODE would otherwise
// compute client-side, and a chart is where the temptation is strongest. Every
// transform maps onto POST /queries/v2: resample onto groupTime and groupType,
// diff and rate onto the difference aggregates and the math field, convert onto
// sourceCharacteristicId → targetCharacteristicId evaluated against the
// ontology's conversion graph. This package selects a target; timescale-wrapper
// evaluates the formula (§5.4.11).
package charts

import (
	"fmt"
	"strings"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// Author says who put a chart element on screen. It exists because a chart mixes
// three sources of claim — the developer, the model, and the profiler's own
// detectors — and a reader has to be able to tell them apart. An LLM can name any
// `source` it likes in a spec it authors; it cannot claim authorship it does not
// have, because the backend stamps this field.
type Author string

const (
	AuthorDeveloper Author = "developer"
	AuthorLLM       Author = "llm"
	AuthorProfiler  Author = "profiler"
)

// Spec is the chart specification of §5.9, as stored.
//
// Three fields go beyond the shape printed there, and all three are what a
// renderable, reproducible chart needs. Window, because a chart with no time
// range is not renderable and a *relative* range would make the same chart mean
// something different tomorrow — so it is resolved once at creation and stored
// absolute, the same immutability the profiles have (D21). Series.ProfileID,
// because the profiler-derived annotations of §5.10 have to come from a named
// profile rather than from a guess. And Annotation.FieldPath, because a
// confirmable annotation is worth nothing if nobody can say which field
// confirming it writes to.
type Spec struct {
	ChartID string `json:"chart_id"`
	Title   string `json:"title"`
	Caption string `json:"caption,omitempty"`

	Series      []SeriesSpec `json:"series"`
	Annotations []Annotation `json:"annotations"`
	Markers     []Marker     `json:"markers"`
	YAxis       YAxis        `json:"y_axis"`

	// Window is the range the chart covers. Absolute, and resolved at creation.
	Window profiler.Window `json:"window"`
	// GroupTime is the chart-wide aggregation bucket. Empty means "derive one that
	// fits the point cap", which is the normal case: a spec author knows the shape
	// they want to see, not how many points a browser should draw.
	//
	// One bucket for the whole chart rather than one per series is §5.3.1 item 4:
	// alignment across series is a property of the request. A series may still
	// override it with resample:, and the response says when that has cost
	// alignment.
	GroupTime string `json:"group_time,omitempty"`

	Author    Author    `json:"author"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// SessionID ties a chart the model proposed to the conversation it was
	// proposed in, so the pane can list "the charts of this session". Empty for a
	// chart the developer built themselves.
	SessionID string `json:"session_id,omitempty"`
}

// SeriesSpec is one line on the chart.
type SeriesSpec struct {
	Ref       profiler.SeriesRef `json:"ref"`
	Transform string             `json:"transform,omitempty"`
	Label     string             `json:"label,omitempty"`
	// ProfileID names the profile the profiler-derived annotations come from
	// (§5.10). Optional: a chart of a series nobody has profiled still draws, it
	// just carries no detected sessions or gaps.
	ProfileID string `json:"profile_id,omitempty"`
}

// AnnotationSpan is the one annotation type §5.9 defines: a labelled range. An
// instant is a marker, which the specification keeps as a separate list and which
// stays a separate type here.
const AnnotationSpan = "span"

const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Annotation is a labelled range, as §5.9 defines it.
type Annotation struct {
	// AnnotationID is minted by ODE, not by the spec author: it is what a
	// confirmation refers back to, so it has to be unforgeable within a chart.
	AnnotationID string    `json:"annotation_id"`
	Type         string    `json:"type"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Label        string    `json:"label"`
	Severity     string    `json:"severity"`
	// Source is the free-text provenance of §5.9 ("profiler.sessions").
	Source string `json:"source,omitempty"`
	// Confirmable marks an annotation the developer may confirm, correct or reject
	// into the override overlay (§5.10). It requires FieldPath, because a
	// confirmation with nowhere to go is theatre.
	Confirmable bool   `json:"confirmable"`
	FieldPath   string `json:"field_path,omitempty"`
	// SeriesIndex is which series the annotation belongs to, or nil for the whole
	// chart. A confirmation needs it: the overlay is keyed by series reference.
	SeriesIndex *int `json:"series_index,omitempty"`
	// Author is stamped by ODE. A derived annotation is profiler-authored and is
	// added at read time, so it cannot be forged in a spec.
	Author Author `json:"author"`
}

// Marker is a labelled instant — §5.9's counter-reset case.
type Marker struct {
	MarkerID    string    `json:"marker_id"`
	At          time.Time `json:"at"`
	Label       string    `json:"label"`
	Source      string    `json:"source,omitempty"`
	SeriesIndex *int      `json:"series_index,omitempty"`
	Author      Author    `json:"author"`
}

// YAxis is what §5.9 asks for, and only what the spec author may say: a unit and
// where they got it. The resolved axis a chart is actually drawn against is
// computed from the ontology and the override overlay, and lives on Data.
type YAxis struct {
	Unit       string `json:"unit,omitempty"`
	UnitSource string `json:"unit_source,omitempty"`
}

// Transform kinds. The strings are §5.9's own.
const (
	TransformNone     = "none"
	TransformDiff     = "diff"
	TransformRate     = "rate"
	prefixResample    = "resample:"
	prefixConvert     = "convert:"
	transformResample = "resample"
	transformConvert  = "convert"
)

// transform is a parsed §5.9 transform string.
type transform struct {
	Kind string
	// Bucket is the resample target, already in the interval form the server
	// accepts.
	Bucket string
	// Target is the characteristic a convert: aims at. The source characteristic
	// is the variable's own and is never taken from the spec: §5.4.11 is explicit
	// that a fabricated characteristic id silently authorises a wrong conversion.
	Target string
}

func (t transform) String() string {
	switch t.Kind {
	case transformResample:
		return prefixResample + t.Bucket
	case transformConvert:
		return prefixConvert + t.Target
	case "":
		return TransformNone
	default:
		return t.Kind
	}
}

// parseTransform reads one transform string.
//
// The characteristic id in convert: carries colons of its own
// (urn:infai:ses:characteristic:…), so the split is on the first colon only.
func parseTransform(raw string) (transform, error) {
	value := strings.TrimSpace(raw)
	switch {
	case value == "", value == TransformNone:
		return transform{Kind: TransformNone}, nil
	case value == TransformDiff:
		return transform{Kind: TransformDiff}, nil
	case value == TransformRate:
		return transform{Kind: TransformRate}, nil
	case strings.HasPrefix(value, prefixResample):
		bucket, err := parseBucket(strings.TrimPrefix(value, prefixResample))
		if err != nil {
			return transform{}, err
		}
		return transform{Kind: transformResample, Bucket: bucket}, nil
	case strings.HasPrefix(value, prefixConvert):
		target := strings.TrimSpace(strings.TrimPrefix(value, prefixConvert))
		if target == "" {
			return transform{}, fmt.Errorf(
				"%w: convert: needs a target characteristic id — a unit string cannot be converted (SPEC D29)",
				ErrInvalidSpec)
		}
		return transform{Kind: transformConvert, Target: target}, nil
	default:
		return transform{}, fmt.Errorf(
			"%w: transform %q is not one of none, diff, rate, resample:<interval>, convert:<characteristic_id>",
			ErrInvalidSpec, raw)
	}
}

// parseBucket normalises a resample target to an interval the server accepts.
//
// "900s" and "15m" are the same bucket and both appear in §5.9; a spec author
// should not have to know which form timescale-wrapper's regex takes.
func parseBucket(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("%w: resample: needs an interval, e.g. resample:900s", ErrInvalidSpec)
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		if parsed <= 0 {
			return "", fmt.Errorf("%w: resample interval %q is not positive", ErrInvalidSpec, raw)
		}
		// Whole seconds, and at least one. The server's interval is expressed in
		// whole seconds, so anything finer has to be rounded to be sent at all —
		// and rounding here would be silent: "500ms" would become a bucket of no
		// length and "90500ms" a bucket half a second short of what was asked for,
		// with the rate divisor computed from the rounded figure either way. A
		// refusal that says which interval to ask for instead is the smaller cost.
		if parsed%time.Second != 0 {
			return "", fmt.Errorf(
				"%w: resample interval %q is finer than whole seconds, which the platform's "+
					"bucket cannot express; ask for whole seconds, e.g. resample:1s",
				ErrInvalidSpec, raw)
		}
		return timeseries.FormatBucket(parsed), nil
	}
	// Forms the server accepts and Go's parser does not — a day, a week, a month.
	// Passed through unchanged rather than rejected, and the server validates it.
	return value, nil
}
