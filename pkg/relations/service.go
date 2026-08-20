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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/ontology"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// The dependencies, each narrowed to what a relational pass actually uses — the
// same reason pkg/charts and pkg/selection declare their own: a test answers with
// a handful of functions rather than a platform.
type (
	Timeseries interface {
		Query(ctx context.Context, token string, elements []timeseries.QueryElement,
			opts timeseries.QueryOptions) ([]timeseries.QueryResult, error)
	}

	Devices interface {
		Get(token string, id string, action drmodel.AuthAction) (models.ExtendedDevice, error)
	}

	// Ontology supplies the aspect hierarchy that solves candidate selection, and the
	// two existing-grouping sources §5.5 asks to be checked before constructing new
	// sets: device groups, which say which devices belong together, and graphs, which
	// say how they are wired.
	Ontology interface {
		Snapshot(ctx context.Context, token string) (*ontology.Snapshot, error)
		ListDeviceGroups(ctx context.Context, token string, opts ontology.DeviceGroupOptions) ([]models.DeviceGroup, error)
		ListGraphs(ctx context.Context, token string, opts ontology.GraphOptions) ([]models.Graph, error)
	}

	// Selection resolves an aspect to concrete series. Reused rather than
	// reimplemented: §5.2 already answers "which series exist under this aspect,
	// and may this developer read them", and a second implementation would be a
	// second answer to drift from.
	Selection interface {
		Resolve(ctx context.Context, token string, req selection.Request) (selection.Result, error)
	}

	// Profiler supplies the activity_pattern every state series is derived from,
	// with the developer's overlay already applied.
	Profiler interface {
		ProfileService(ctx context.Context, token string, req profiler.ProfileRequest) (profiler.ProfileResult, error)
	}

	IDs interface{ NewID() string }
)

type Deps struct {
	Timeseries Timeseries
	Devices    Devices
	Ontology   Ontology
	Selection  Selection
	Profiler   Profiler
	Store      Store
	IDs        IDs
	// OntologyIndex resolves the unit and characteristic of a variable. Needed
	// because a graph reaches devices the aspect resolution never saw — a site meter
	// is not in the kitchen — and those have to be enumerated from their device type
	// rather than from a selectables answer.
	OntologyIndex profiler.OntologySource

	// MaxMembers bounds one relational pass. The pair count grows with the square of
	// the members and the rule count with four times that, so this is what keeps a
	// rule list readable rather than what keeps the read cheap — the read is one
	// batched query either way.
	MaxMembers int
	// MaxBuckets bounds the aligned grid. Two years at fifteen minutes is seventy
	// thousand buckets per member; the grid is widened to fit rather than the window
	// truncated, so the pass still covers what it says it covers.
	MaxBuckets int
	// MaxRules bounds the candidate list a pass emits. The strongest survive, and a
	// truncation is stated in Notes rather than being silent.
	MaxRules int
	// DefaultLookback is the window a pass covers when nothing names one.
	DefaultLookback time.Duration
	// ReadTimeout bounds the aligned read, overriding the timeseries client default
	// for the same reason the profiler has its own: a month of buckets across six
	// members is not comparable to a metadata probe.
	ReadTimeout time.Duration
	// DeviceLimit bounds how many devices a proposal expands, matching the ceiling
	// the rest of ODE applies.
	DeviceLimit int64
	// MaxGraphNeighbours bounds how many devices outside the requested aspect a
	// proposal will resolve through a graph. Each one costs a device read, and a graph
	// of a whole site legitimately names hundreds — so this is what keeps an
	// aspect-scoped question from expanding into a site-wide one. What it drops is
	// stated in the notes rather than being silent.
	MaxGraphNeighbours int

	// Now is the clock, injectable so a test can assert on a stored timestamp.
	Now func() time.Time
}

const (
	defaultMaxMembers = 6
	defaultMaxBuckets = 20000
	defaultMaxRules   = 100
	defaultLookback   = 30 * 24 * time.Hour
	maxAllowedMembers = 12
	// A site meter, a floor meter and a handful of circuits: enough to reach the levels
	// above an aspect without turning one room's question into a site survey.
	defaultMaxGraphNeighbours = 12
)

type Service struct {
	deps Deps
}

func New(deps Deps) (*Service, error) {
	if deps.Timeseries == nil || deps.Devices == nil || deps.Ontology == nil {
		return nil, errors.New("relations: a timeseries client, a device reader and an ontology source are required")
	}
	if deps.Selection == nil {
		return nil, errors.New("relations: a selection resolver is required — the aspect hierarchy is what proposes the sets (§5.5)")
	}
	if deps.Profiler == nil {
		return nil, errors.New("relations: a profiler is required — every state series comes from an activity_pattern")
	}
	if deps.OntologyIndex == nil {
		return nil, errors.New("relations: an ontology index is required — a graph reaches devices " +
			"outside the requested aspect, and their units come from it")
	}
	if deps.IDs == nil {
		return nil, errors.New("relations: an id source is required")
	}
	if deps.Store == nil {
		deps.Store = NewMemoryStore(0)
	}
	if deps.MaxMembers <= 0 {
		deps.MaxMembers = defaultMaxMembers
	}
	if deps.MaxMembers > maxAllowedMembers {
		deps.MaxMembers = maxAllowedMembers
	}
	if deps.MaxBuckets <= 0 {
		deps.MaxBuckets = defaultMaxBuckets
	}
	if deps.MaxRules <= 0 {
		deps.MaxRules = defaultMaxRules
	}
	if deps.MaxGraphNeighbours <= 0 {
		deps.MaxGraphNeighbours = defaultMaxGraphNeighbours
	}
	if deps.DefaultLookback <= 0 {
		deps.DefaultLookback = defaultLookback
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{deps: deps}, nil
}

// MaxMembers is the ceiling a caller can report before submitting a pass.
func (s *Service) MaxMembers() int { return s.deps.MaxMembers }

// Request is one relational pass (§5.5).
type Request struct {
	Members []SeriesMember
	Window  profiler.Window
	// GridSeconds overrides the derived alignment bucket. Recorded in the profile so
	// a pass run on an unusual grid is not mistaken for a default one, in the same
	// spirit as D25's window source.
	GridSeconds  float64
	Params       RuleParams
	Conditioning *Conditioning
	// CandidateSetID names the proposal these members came from, so a confirmed rule
	// can be traced back to the aspect that suggested the devices.
	CandidateSetID string

	// Progress is called as the pass moves between phases, so a caller streaming to
	// a developer can show that a multi-minute operation is alive. Optional; called
	// from the goroutine running the pass and must not block.
	Progress func(Phase)
}

// Phase is one step of a relational pass, for Progress. Same shape as the
// profiler's, because the same WebSocket carries both and a client should not need
// two readers.
type Phase struct {
	Stage  string `json:"stage"`
	Detail string `json:"detail"`
}

const (
	PhaseProfiles = "profiles"
	PhaseAlign    = "align"
	PhaseStates   = "states"
	PhaseRelate   = "relate"
	PhaseStore    = "store"
)

func (r Request) report(stage, detail string) {
	if r.Progress == nil {
		return
	}
	r.Progress(Phase{Stage: stage, Detail: detail})
}

// Relate runs one relational pass end to end.
//
// The order is §5.5's, and each step depends on the previous one in a way worth
// stating: the profiles decide the grid, because the grid comes from the coarsest
// member's detected sampling interval; the grid decides the alignment, because a
// shared groupTime is what makes the buckets comparable; and the alignment decides
// the states, because a state is a property of a bucket rather than of a reading.
//
// Tier L1. Values are read — a state series cannot be derived without them — and
// nothing that comes back contains one.
func (s *Service) Relate(ctx context.Context, token string, req Request) (RelationProfile, error) {
	members, err := s.validate(req)
	if err != nil {
		return RelationProfile{}, err
	}

	window := req.Window
	if !window.Valid() {
		if !window.From.IsZero() || !window.To.IsZero() {
			return RelationProfile{}, fmt.Errorf(
				"%w: a window needs both a from and a to, and the from must be earlier", ErrInvalidRequest)
		}
		now := s.deps.Now().UTC()
		window = profiler.Window{From: now.Add(-s.deps.DefaultLookback), To: now}
	}

	params := req.Params.withDefaults()
	conditioning := DefaultConditioning()
	if req.Conditioning != nil {
		conditioning = *req.Conditioning
	}

	profile := RelationProfile{
		DetectorVersion: DetectorVersion,
		ComputedAt:      s.deps.Now().UTC(),
		Tier:            TierRelation,
		Window:          window,
		Params:          params,
		Conditioning:    conditioning,
		CandidateSetID:  req.CandidateSetID,
		Members:         []Member{},
		Pairs:           []PairRelation{},
		CandidateRules:  []CandidateRule{},
		Notes:           []string{},
	}

	req.report(PhaseProfiles, fmt.Sprintf("profiling %d series across %d service(s)",
		len(members), distinctServices(members)))
	resolved, reads, deviceReads, notes, err := s.profileMembers(ctx, token, members, window)
	if err != nil {
		return RelationProfile{}, err
	}
	profile.Reads.Profiles = reads
	profile.Reads.Devices = deviceReads
	profile.Notes = append(profile.Notes, notes...)

	intervals := make([]float64, 0, len(resolved))
	requests := make([]alignRequest, 0, len(resolved))
	for _, member := range resolved {
		requests = append(requests, alignRequest{Ref: member.ref, Kind: member.kind})
		if member.interval > 0 {
			intervals = append(intervals, member.interval)
		}
	}

	gridSeconds := req.GridSeconds
	if gridSeconds > 0 {
		profile.Notes = append(profile.Notes, fmt.Sprintf(
			"the alignment grid was set to %gs by the caller rather than derived from the coarsest "+
				"member; a grid finer than the slowest series produces idle states that are gaps", gridSeconds))
	} else {
		widened := false
		gridSeconds, widened = chooseGrid(window, intervals, s.deps.MaxBuckets)
		if widened {
			profile.Notes = append(profile.Notes, fmt.Sprintf(
				"the grid was widened to %gs so the window fits %d buckets; the window was not shortened",
				gridSeconds, s.deps.MaxBuckets))
		}
		if len(intervals) > 0 {
			sorted := sortedIntervals(intervals)
			profile.Notes = append(profile.Notes, fmt.Sprintf(
				"the grid follows the coarsest member's sampling interval (%gs of %v)",
				sorted[len(sorted)-1], formatIntervals(sorted)))
		} else {
			profile.Notes = append(profile.Notes,
				"no member reported a sampling interval, so the grid fell back to the platform's "+
					"15-minute meter cadence")
		}
	}

	req.report(PhaseAlign, fmt.Sprintf("one batched read at a %gs bucket", gridSeconds))
	frame, err := s.Align(ctx, token, requests, window, gridSeconds)
	if err != nil {
		return RelationProfile{}, err
	}
	profile.Reads.Aligned = frame.Reads
	profile.Reads.Values = profile.Reads.Aligned + profile.Reads.Profiles
	profile.GroupTime = frame.GroupTime
	profile.GridSeconds = frame.GridSeconds
	profile.Buckets = len(frame.Times)
	profile.Notes = append(profile.Notes, frame.Notes...)

	req.report(PhaseStates, "deriving idle and active from each activity_pattern")
	series := make([]StateSeries, 0, len(resolved))
	for i, member := range resolved {
		state := StateSeries{}
		if member.failure != nil {
			// No profile, so no threshold and no state series. The recorded cause is kept
			// rather than DeriveState's generic one: "the service could not be profiled"
			// is actionable and "no detector populated this field" is not.
			state = unusableSeries(member.member, *member.failure, len(frame.Times),
				frame.Columns[i].Points)
		} else {
			state = DeriveState(frame.Columns[i], member.member, member.profile)
		}
		series = append(series, state)
		profile.Members = append(profile.Members, state.Member)
	}

	req.report(PhaseRelate, "tabulating pairs and proposing rules")
	pairs, rules, observed, relateNotes := Relate(frame.Times, series, params, conditioning)
	profile.Pairs = pairs
	profile.Observed = observed
	profile.Notes = append(profile.Notes, relateNotes...)

	if len(rules) > s.deps.MaxRules {
		profile.Notes = append(profile.Notes, fmt.Sprintf(
			"%d candidate rules cleared the thresholds and the strongest %d are carried; "+
				"raise min_confidence or min_lift to narrow the list rather than reading a truncated one",
			len(rules), s.deps.MaxRules))
		rules = rules[:s.deps.MaxRules]
	}
	profile.CandidateRules = s.attachDecisions(rules)

	profile.CacheKey = s.cacheKey(members, window, gridSeconds, params, conditioning)
	profile.RelationID = profile.CacheKey

	req.report(PhaseStore, "storing the relation profile")
	stored, created, err := s.deps.Store.Put(profile)
	if err != nil {
		return RelationProfile{}, err
	}
	if !created {
		// The same members over the same window with the same detectors: the stored
		// document is returned rather than the freshly computed one, so a decision made
		// against what a developer read still applies to what they read (D21). The
		// decisions are re-attached because the log may have moved on since.
		stored.CandidateRules = s.attachDecisions(stored.CandidateRules)
	}
	return stored, nil
}

// TierRelation is the exposure tier a RelationProfile may be shown to a model at
// (§3.2). L1: the document is aggregates — contingency counts, ratios, bucket
// durations — and carries no value of any series.
const TierRelation = "L1"

// resolvedMember is one member with everything the pass learned about it.
type resolvedMember struct {
	ref      profiler.SeriesRef
	member   Member
	profile  profiler.ResolvedProfile
	kind     profiler.ValueKind
	interval float64
	// failure is set when no profile could be computed for this member. It is kept
	// separate from the profile because DeriveState would otherwise report the
	// generic out_of_scope of an empty activity_pattern and lose the actual cause —
	// which is the one thing a developer can act on.
	failure *profiler.NotComputed
}

// validate checks the member list before anything is read.
func (s *Service) validate(req Request) ([]SeriesMember, error) {
	seen := map[string]bool{}
	out := make([]SeriesMember, 0, len(req.Members))
	for _, member := range req.Members {
		if !member.Ref.Valid() {
			return nil, fmt.Errorf("%w: every member needs a device id, a service id and a variable path",
				ErrInvalidRequest)
		}
		// A duplicate is dropped rather than refused: a member appearing twice would
		// relate a series to itself at confidence 1.0, and a caller assembling a set
		// from two overlapping proposals has made an ordinary mistake.
		if seen[member.Ref.String()] {
			continue
		}
		seen[member.Ref.String()] = true
		out = append(out, member)
	}
	if len(out) < 2 {
		return nil, fmt.Errorf("%w: got %d distinct series", ErrTooFewMembers, len(out))
	}
	if len(out) > s.deps.MaxMembers {
		return nil, fmt.Errorf("%w: %d members exceeds the %d this deployment allows, and the pair "+
			"count grows with the square of them", ErrInvalidRequest, len(out), s.deps.MaxMembers)
	}
	return out, nil
}

// profileMembers computes or reuses a SeriesProfile for every member.
//
// One profile pass per (device, service), not per member: the profiler's unit of
// work is the service (§5.4.1, D19), so two members of the same meter cost one pass
// between them. A pass already stored under the same cache key is returned from the
// store rather than recomputed, which is what makes a second relational pass over
// the same window cheap.
//
// A member whose profile cannot be computed is kept, with the reason, and takes part
// in no rule. Failing the whole pass would be the wrong trade: the oven-and-lights
// finding does not depend on the third device having usable data.
func (s *Service) profileMembers(
	ctx context.Context, token string, members []SeriesMember, window profiler.Window,
) ([]resolvedMember, int, int, []string, error) {
	type serviceKey struct{ device, service string }

	notes := []string{}
	reads, deviceReads := 0, 0
	profiles := map[string]profiler.ResolvedProfile{}
	deviceNames := map[string]string{}
	serviceNames := map[string]string{}
	failed := map[serviceKey]string{}

	order := []serviceKey{}
	for _, member := range members {
		key := serviceKey{member.Ref.DeviceID, member.Ref.ServiceID}
		if !containsKey(order, key) {
			order = append(order, key)
		}
	}

	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return nil, reads, deviceReads, notes, err
		}
		// Execute, not Read: this is about to read the device's data (§5.1).
		device, err := s.deps.Devices.Get(token, key.device, models.Execute)
		deviceReads++
		if err != nil {
			return nil, reads, deviceReads, notes, err
		}
		deviceNames[key.device] = displayDeviceName(device)
		if device.DeviceType != nil {
			for _, service := range device.DeviceType.Services {
				if service.Id == key.service {
					serviceNames[key.service] = service.Name
				}
			}
		}

		result, err := s.deps.Profiler.ProfileService(ctx, token, profiler.ProfileRequest{
			Device:         device,
			ServiceID:      key.service,
			AnalysisWindow: window,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, reads, deviceReads, notes, err
			}
			// One service's profile failing is recorded against its members rather than
			// ending the pass, for the reason above.
			failed[key] = err.Error()
			notes = append(notes, fmt.Sprintf(
				"%s could not be profiled, so its series take part in no rule: %s", key.service, err.Error()))
			continue
		}
		reads += result.Reads.Values
		for _, profile := range result.Profiles {
			profiles[profile.SeriesRef.String()] = profile
		}
	}

	out := make([]resolvedMember, 0, len(members))
	for _, member := range members {
		resolved := resolvedMember{ref: member.Ref}
		profile, found := profiles[member.Ref.String()]
		resolved.profile = profile

		label := member.Label
		if label == "" {
			label = memberLabel(deviceNames[member.Ref.DeviceID], member.Ref.VariablePath)
		}
		if strings.TrimSpace(label) == "" {
			label = member.Ref.String()
		}

		resolved.member = Member{
			Ref:         member.Ref,
			Label:       label,
			DeviceName:  deviceNames[member.Ref.DeviceID],
			ServiceName: serviceNames[member.Ref.ServiceID],
			ProfileID:   profile.ProfileID,
			Unit:        profile.ValueSemantics.Unit,
		}
		if kind, known := profile.ValueSemantics.Kind.Get(); known {
			resolved.kind = kind
			resolved.member.Kind = kind
		}
		if sampling, known := profile.Sampling.Get(); known {
			resolved.interval = sampling.DetectedIntervalS
		}
		if !found {
			reason := "no profile was computed for this series"
			if detail, refused := failed[serviceKey{member.Ref.DeviceID, member.Ref.ServiceID}]; refused {
				reason = "the service could not be profiled: " + detail
			}
			resolved.failure = &profiler.NotComputed{
				Status: notComputedStatus, Reason: profiler.ReasonReadFailed, Detail: reason,
			}
		}
		out = append(out, resolved)
	}

	// Labels decide what every rule statement says, so a collision here produces
	// "Licht EG value active → Licht EG value active" — true, unreadable, and
	// unconfirmable. Settled after the members are resolved rather than trusted from
	// the request, because a caller may well send colliding labels: the proposal's own
	// labels come from a device name and a leaf path segment, and "value" is the
	// commonest leaf on the platform.
	out = labelMembers(out)
	return out, reads, deviceReads, notes, nil
}

// labelMembers makes the members' labels distinct, on the same terms
// disambiguateLabels uses for a proposal: whatever separates them, preferring what
// reads best.
func labelMembers(members []resolvedMember) []resolvedMember {
	byLabel := map[string][]int{}
	for i, member := range members {
		byLabel[member.member.Label] = append(byLabel[member.member.Label], i)
	}

	for _, indices := range byLabel {
		if len(indices) < 2 {
			continue
		}
		options := make([][]string, 0, len(indices))
		for _, i := range indices {
			options = append(options, []string{
				members[i].member.ServiceName,
				members[i].member.DeviceName,
				members[i].ref.VariablePath,
				members[i].ref.String(),
			})
		}
		for position, suffix := range labelSuffixes(options) {
			members[indices[position]].member.Label += " (" + suffix + ")"
		}
	}
	return members
}

// attachDecisions re-injects the developer's verdicts (§5.10).
//
// Re-injection rather than storage inside the rule: the decision log is the record,
// the rule is a computation over data, and copying the verdict in at read time is
// what lets a recomputed rule arrive already carrying what somebody decided about it
// last month.
func (s *Service) attachDecisions(rules []CandidateRule) []CandidateRule {
	if len(rules) == 0 {
		return []CandidateRule{}
	}
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		ids = append(ids, rule.RuleID)
	}
	log := s.deps.Store.Decisions(ids)

	out := make([]CandidateRule, len(rules))
	copy(out, rules)
	for i := range out {
		out[i].Decision = latest(log[out[i].RuleID])
	}
	return out
}

// Get serves a stored relation profile with the decision log as it stands now.
func (s *Service) Get(relationID string) (RelationProfile, error) {
	profile, found := s.deps.Store.ByID(relationID)
	if !found {
		return RelationProfile{}, fmt.Errorf("%w: %s", ErrRelationNotFound, relationID)
	}
	profile.CandidateRules = s.attachDecisions(profile.CandidateRules)
	return profile, nil
}

// DecisionRequest is one developer verdict on one candidate rule.
type DecisionRequest struct {
	RelationID string
	RuleID     string
	Action     DecisionAction
	// Confirmed is the developer's own form of the rule, required for a correction.
	Confirmed *DecidedRule
	Note      string
	// UserSub is stamped by the caller from the authenticated token, never taken from
	// a request body.
	UserSub string
}

// Decide records a decision on a candidate rule (§5.10, D21).
//
// The rule has to exist in the named relation profile. That check is the reason this
// takes a relation id at all: the fingerprint is what the decision is keyed by and
// would be enough to store one, but a decision on a mistyped fingerprint would be a
// record nothing ever reads back — and it is a developer's judgement, which is the
// one thing in this package that cannot be recomputed.
func (s *Service) Decide(req DecisionRequest) (RuleDecision, error) {
	profile, found := s.deps.Store.ByID(req.RelationID)
	if !found {
		return RuleDecision{}, fmt.Errorf("%w: %s", ErrRelationNotFound, req.RelationID)
	}

	var rule *CandidateRule
	for i := range profile.CandidateRules {
		if profile.CandidateRules[i].RuleID == req.RuleID {
			rule = &profile.CandidateRules[i]
			break
		}
	}
	if rule == nil {
		return RuleDecision{}, fmt.Errorf("%w: %s carries no rule %s",
			ErrUnknownRule, req.RelationID, req.RuleID)
	}

	decision := RuleDecision{
		DecisionID:      s.deps.IDs.NewID(),
		CreatedAt:       s.deps.Now().UTC(),
		CreatedBy:       req.UserSub,
		RuleID:          rule.RuleID,
		RelationID:      profile.RelationID,
		DetectorVersion: profile.DetectorVersion,
		Action:          req.Action,
		Computed: DecidedRule{
			Statement:  rule.Statement,
			Anomaly:    rule.Anomaly,
			Support:    rule.Support,
			Confidence: rule.Confidence,
			Lift:       rule.Lift,
			Exceptions: rule.Exceptions,
		},
		Confirmed: req.Confirmed,
		Note:      req.Note,
	}
	if decision.Confirmed != nil && decision.Confirmed.Exceptions == nil {
		decision.Confirmed.Exceptions = []Exception{}
	}
	return s.deps.Store.AppendDecision(decision)
}

// Decisions is the log for one rule, oldest first — the history behind whatever
// currently stands.
func (s *Service) Decisions(ruleID string) []RuleDecision {
	log := s.deps.Store.Decisions([]string{ruleID})
	if found, ok := log[ruleID]; ok {
		return found
	}
	return []RuleDecision{}
}

// cacheKey identifies a relation profile by everything that can change its content
// (D25), including both detector versions: the rules are this package's, and the
// state series they rest on are the profiler's.
func (s *Service) cacheKey(
	members []SeriesMember, window profiler.Window, grid float64,
	params RuleParams, conditioning Conditioning,
) string {
	refs := make([]string, 0, len(members))
	for _, member := range members {
		refs = append(refs, member.Ref.String())
	}
	sort.Strings(refs)

	parts := append([]string{}, refs...)
	parts = append(parts,
		window.String(),
		strconv.FormatFloat(grid, 'f', -1, 64),
		fmt.Sprintf("%v", params),
		fmt.Sprintf("%v", conditioning),
		DetectorVersion,
		profiler.DetectorVersion,
	)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func containsKey[T comparable](keys []T, key T) bool {
	for _, existing := range keys {
		if existing == key {
			return true
		}
	}
	return false
}

func distinctServices(members []SeriesMember) int {
	seen := map[string]bool{}
	for _, member := range members {
		seen[member.Ref.DeviceID+"|"+member.Ref.ServiceID] = true
	}
	return len(seen)
}

func displayDeviceName(device models.ExtendedDevice) string {
	if device.DisplayName != "" {
		return device.DisplayName
	}
	return device.Name
}

func formatIntervals(intervals []float64) []string {
	out := make([]string, 0, len(intervals))
	for _, interval := range intervals {
		out = append(out, strconv.FormatFloat(interval, 'g', 4, 64)+"s")
	}
	return out
}
