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
	"errors"
	"fmt"
	"strings"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// The dependencies, each narrowed to what a chart actually needs — the same
// reason pkg/tools and pkg/selection declare their own: a test answers with three
// functions rather than a platform.
type (
	Timeseries interface {
		Query(ctx context.Context, token string, elements []timeseries.QueryElement,
			opts timeseries.QueryOptions) ([]timeseries.QueryResult, error)
	}

	Devices interface {
		Get(token string, id string, action drmodel.AuthAction) (models.ExtendedDevice, error)
	}

	// Profiles is the profiler's store, narrowed. Charts read profiles for their
	// annotations and write confirmations into the same append-only overlay the
	// profiler view writes to (§5.4.3) — there is one overlay, not one per surface,
	// which is what lets a confirmation made on a chart apply to the next profile.
	Profiles interface {
		ByID(profileID string) (profiler.SeriesProfile, bool)
		Sessions(profileID string, query profiler.SessionQuery) (profiler.SessionPage, error)
		Overrides(ref profiler.SeriesRef) []profiler.ProfileOverride
		AppendOverride(override profiler.ProfileOverride) (profiler.ProfileOverride, error)
	}

	IDs interface{ NewID() string }
)

type Deps struct {
	Timeseries Timeseries
	Devices    Devices
	Ontology   profiler.OntologySource
	Profiles   Profiles
	Store      Store
	IDs        IDs

	// MaxPoints bounds one series of one chart. A browser draws a few thousand
	// points legibly and nothing beyond that; the bucket is widened to fit rather
	// than the window truncated, so the chart still covers what it says it covers.
	MaxPoints int
	// MaxSeries bounds one chart. Eight, by default, because that is roughly where
	// distinguishable colours run out — and because every series is one more
	// element in the same batched read.
	MaxSeries int
	// MaxAnnotations bounds the bands one chart carries. A washing machine over two
	// years has thousands of sessions (D27); a chart of them is unreadable and the
	// paginated session resource is where they belong.
	MaxAnnotations int
	// DefaultLookback is the window a chart covers when nothing names one and no
	// profile suggests one.
	DefaultLookback time.Duration

	// Now is the clock, injectable so a test can assert on a stored timestamp.
	Now func() time.Time
}

const (
	defaultMaxPoints       = 2000
	defaultMaxSeries       = 8
	defaultMaxAnnotations  = 200
	defaultLookback        = 7 * 24 * time.Hour
	sessionAnnotationLimit = 500
)

type Service struct {
	deps Deps
}

func New(deps Deps) (*Service, error) {
	if deps.Timeseries == nil || deps.Devices == nil || deps.Ontology == nil {
		return nil, errors.New("charts: a timeseries client, a device reader and an ontology source are required")
	}
	if deps.Profiles == nil {
		return nil, errors.New("charts: a profile store is required — annotations and confirmations live in it")
	}
	if deps.IDs == nil {
		return nil, errors.New("charts: an id source is required")
	}
	if deps.Store == nil {
		deps.Store = NewMemoryStore(0)
	}
	if deps.MaxPoints <= 0 {
		deps.MaxPoints = defaultMaxPoints
	}
	if deps.MaxSeries <= 0 {
		deps.MaxSeries = defaultMaxSeries
	}
	if deps.MaxAnnotations <= 0 {
		deps.MaxAnnotations = defaultMaxAnnotations
	}
	if deps.DefaultLookback <= 0 {
		deps.DefaultLookback = defaultLookback
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{deps: deps}, nil
}

// CreateRequest is a chart as its author asked for it.
type CreateRequest struct {
	UserSub   string
	SessionID string
	// Author is stamped by the caller — the HTTP route says developer, the tool
	// dispatcher says llm — and never read from the request body.
	Author Author

	Title       string
	Caption     string
	Series      []SeriesSpec
	Annotations []Annotation
	Markers     []Marker
	YAxis       YAxis
	Window      profiler.Window
	GroupTime   string
}

// SeriesResolution is one series' spec resolved against the platform: which
// variable it addresses, and what unit its values are in.
type SeriesResolution struct {
	Index     int                `json:"index"`
	Ref       profiler.SeriesRef `json:"ref"`
	Label     string             `json:"label"`
	Transform string             `json:"transform"`
	ProfileID string             `json:"profile_id,omitempty"`
	Unit      Unit               `json:"unit"`
	// Notes carries anything the resolution had to say about this series — a
	// widened bucket, a refused conversion, a unit that needs confirming.
	Notes []string `json:"notes"`
}

// Created is what a caller gets back from Create: the stored specification plus
// the resolution, so an author learns what their transforms actually mean without
// asking for the data.
type Created struct {
	Spec   Spec               `json:"spec"`
	Series []SeriesResolution `json:"series"`
	Axis   Axis               `json:"y_axis"`
	Notes  []string           `json:"notes"`
}

// Create validates a specification, resolves what it names, and stores it.
//
// The resolution happens now rather than at first render for two reasons. An
// author — and especially a model — needs to be told that convert: names an
// unreachable characteristic while it can still fix it, not silently later. And
// the device read it takes is the same permission check the value read will need,
// so a chart of a series the developer may not read fails here, under their own
// token, rather than as an empty picture.
func (s *Service) Create(ctx context.Context, token string, req CreateRequest) (Created, error) {
	spec, err := s.normalise(req)
	if err != nil {
		return Created{}, err
	}

	resolved, notes, err := s.resolve(ctx, token, spec)
	if err != nil {
		return Created{}, err
	}

	spec.ChartID = s.deps.IDs.NewID()
	spec.CreatedAt = s.deps.Now()
	s.deps.Store.Put(spec)

	summary := make([]SeriesResolution, 0, len(resolved))
	for _, series := range resolved {
		summary = append(summary, series.SeriesResolution)
	}
	axis, axisNotes := s.axis(spec, resolved)
	return Created{Spec: spec, Series: summary, Axis: axis, Notes: append(notes, axisNotes...)}, nil
}

// Get returns a chart the caller owns.
//
// Ownership rather than a permission check on the series: the series permissions
// are the platform's and are re-checked on every read. What this guards is the
// chart itself, which carries a developer's own framing of their problem.
func (s *Service) Get(chartID, userSub string) (Spec, error) {
	spec, found := s.deps.Store.ByID(chartID)
	if !found || spec.CreatedBy != userSub {
		return Spec{}, fmt.Errorf("%w: %s", ErrChartNotFound, chartID)
	}
	return spec, nil
}

// List is a developer's charts, newest first, optionally narrowed to one chat
// session.
func (s *Service) List(userSub, sessionID string, limit int) []Spec {
	return s.deps.Store.ForUser(userSub, sessionID, limit)
}

func (s *Service) Delete(chartID, userSub string) error {
	if _, err := s.Get(chartID, userSub); err != nil {
		return err
	}
	s.deps.Store.Remove(chartID)
	return nil
}

// normalise validates a request and fills what the author left out. No I/O: this
// is the half of validation that a fixture can check.
func (s *Service) normalise(req CreateRequest) (Spec, error) {
	if req.UserSub == "" {
		return Spec{}, fmt.Errorf("%w: a chart belongs to a developer", ErrInvalidSpec)
	}
	if len(req.Series) == 0 {
		return Spec{}, fmt.Errorf("%w: a chart needs at least one series", ErrInvalidSpec)
	}
	if len(req.Series) > s.deps.MaxSeries {
		return Spec{}, fmt.Errorf("%w: %d series exceeds the limit of %d on one chart",
			ErrInvalidSpec, len(req.Series), s.deps.MaxSeries)
	}

	spec := Spec{
		Title:       strings.TrimSpace(req.Title),
		Caption:     strings.TrimSpace(req.Caption),
		Series:      make([]SeriesSpec, 0, len(req.Series)),
		Annotations: []Annotation{},
		Markers:     []Marker{},
		YAxis:       req.YAxis,
		GroupTime:   strings.TrimSpace(req.GroupTime),
		Author:      req.Author,
		CreatedBy:   req.UserSub,
		SessionID:   req.SessionID,
	}
	if spec.Author == "" {
		spec.Author = AuthorDeveloper
	}

	for i, series := range req.Series {
		if !series.Ref.Valid() {
			return Spec{}, fmt.Errorf(
				"%w: series %d is not fully addressed; a series is {device_id, service_id, variable_path}",
				ErrInvalidSpec, i)
		}
		parsed, err := parseTransform(series.Transform)
		if err != nil {
			return Spec{}, fmt.Errorf("series %d: %w", i, err)
		}
		series.Transform = parsed.String()
		if strings.TrimSpace(series.Label) == "" {
			series.Label = series.Ref.VariablePath
		}
		spec.Series = append(spec.Series, series)
	}

	if spec.GroupTime != "" {
		bucket, err := parseBucket(spec.GroupTime)
		if err != nil {
			return Spec{}, err
		}
		spec.GroupTime = bucket
	}

	if spec.Title == "" {
		spec.Title = spec.Series[0].Label
	}

	for i, annotation := range req.Annotations {
		normalised, err := s.normaliseAnnotation(annotation, len(spec.Series), spec.Author)
		if err != nil {
			return Spec{}, fmt.Errorf("annotation %d: %w", i, err)
		}
		spec.Annotations = append(spec.Annotations, normalised)
	}
	for i, marker := range req.Markers {
		if marker.At.IsZero() {
			return Spec{}, fmt.Errorf("%w: marker %d has no timestamp", ErrInvalidSpec, i)
		}
		if err := checkSeriesIndex(marker.SeriesIndex, len(spec.Series)); err != nil {
			return Spec{}, fmt.Errorf("marker %d: %w", i, err)
		}
		marker.MarkerID = s.deps.IDs.NewID()
		marker.Author = spec.Author
		spec.Markers = append(spec.Markers, marker)
	}

	window, err := s.window(req.Window, spec.Series)
	if err != nil {
		return Spec{}, err
	}
	spec.Window = window
	return spec, nil
}

// normaliseAnnotation checks one author-supplied band.
//
// The confirmable check is the load-bearing one. §5.10 makes confirmation a
// contribution rather than a UI affordance, and a band marked confirmable that
// names no field, or names a field outside profiler.ConfirmablePaths, would offer
// the developer a decision that goes nowhere. It is refused instead — and the
// author is told what the confirmable paths are.
func (s *Service) normaliseAnnotation(annotation Annotation, series int, author Author) (Annotation, error) {
	if annotation.Type == "" {
		annotation.Type = AnnotationSpan
	}
	if annotation.Type != AnnotationSpan {
		return annotation, fmt.Errorf("%w: annotation type %q is not a span; an instant is a marker",
			ErrInvalidSpec, annotation.Type)
	}
	if annotation.From.IsZero() || annotation.To.IsZero() {
		return annotation, fmt.Errorf("%w: a span needs both from and to", ErrInvalidSpec)
	}
	if annotation.To.Before(annotation.From) {
		return annotation, fmt.Errorf("%w: a span ends before it starts", ErrInvalidSpec)
	}
	switch annotation.Severity {
	case "":
		annotation.Severity = SeverityInfo
	case SeverityInfo, SeverityWarn, SeverityError:
	default:
		return annotation, fmt.Errorf("%w: severity %q is not info, warn or error",
			ErrInvalidSpec, annotation.Severity)
	}
	if err := checkSeriesIndex(annotation.SeriesIndex, series); err != nil {
		return annotation, err
	}
	if annotation.Confirmable {
		if _, confirmable := profiler.ConfirmablePaths[annotation.FieldPath]; !confirmable {
			return annotation, fmt.Errorf(
				"%w: a confirmable annotation must name a confirmable field_path; %q is not one",
				ErrInvalidSpec, annotation.FieldPath)
		}
		if annotation.SeriesIndex == nil {
			return annotation, fmt.Errorf(
				"%w: a confirmable annotation must name the series it applies to — the override overlay is keyed by series",
				ErrInvalidSpec)
		}
	}
	annotation.AnnotationID = s.deps.IDs.NewID()
	annotation.Author = author
	return annotation, nil
}

func checkSeriesIndex(index *int, series int) error {
	if index == nil {
		return nil
	}
	if *index < 0 || *index >= series {
		return fmt.Errorf("%w: series_index %d is outside the chart's %d series", ErrInvalidSpec, *index, series)
	}
	return nil
}

// window resolves the charted range, absolute and once.
//
// A profile's own analysis window is preferred over a default lookback where the
// spec names a profile: a chart of a series that was just profiled should show the
// range the annotations were computed over, or every band would sit outside it.
func (s *Service) window(requested profiler.Window, series []SeriesSpec) (profiler.Window, error) {
	if !requested.From.IsZero() || !requested.To.IsZero() {
		if !requested.Valid() {
			return profiler.Window{}, fmt.Errorf(
				"%w: the window needs both from and to, with to after from", ErrInvalidSpec)
		}
		return profiler.Window{From: requested.From.UTC(), To: requested.To.UTC()}, nil
	}

	for _, candidate := range series {
		if candidate.ProfileID == "" {
			continue
		}
		if profile, found := s.deps.Profiles.ByID(candidate.ProfileID); found && profile.AnalysisWindow.Valid() {
			return profile.AnalysisWindow, nil
		}
	}

	now := s.deps.Now().UTC()
	return profiler.Window{From: now.Add(-s.deps.DefaultLookback), To: now}, nil
}
