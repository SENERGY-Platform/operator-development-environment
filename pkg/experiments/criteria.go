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

package experiments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// The developer's evaluation criteria, read and applied (SPEC §5.13, M9).
//
// M8 left this deliberately undone and said why: the criteria are the developer's,
// §5.8 denies every tool that could modify them, and turning the file into a
// verdict was M9's work. This is that work, and two properties bound it.
//
//   - **Read, never written.** §5.8 lists "modifying evaluation criteria" among the
//     capabilities with no tool at all, and pkg/tools/repo.go already refuses
//     `write_file` on this path. Nothing here is a way around that: this package
//     has a read-only view of the repository (see the Repository interface), the
//     file is fetched with `git show`, and no code path in ODE writes it. D28 is the
//     same rule from the other side — a recommendation becomes binding only when a
//     developer promotes it into this file themselves.
//
//   - **A criterion that could not be evaluated is not a criterion that failed.**
//     This is D24 applied outside the profiler, and it is the whole reason `Met` is
//     a Verdict rather than a bool. A missing file, a file outside the subset ODE
//     reads, a metric the run never logged and a criterion with no threshold are
//     four different facts, and every one of them would have been flattened to
//     `met: false` by a bool. An assistant reading `met: false` says the run missed
//     the developer's target; an assistant reading `not_computed` with a reason asks
//     for the thing that is missing. The first is a fabricated finding.

// EvaluationCriteriaPath is the file, relative to the repository root. The same
// constant pkg/tools refuses to write, named once here so the two cannot drift.
const EvaluationCriteriaPath = "evaluation.yaml"

// maxCriteriaBytes bounds the read. Two orders of magnitude above the scaffold's
// own file, and small enough that a repository with a large file at this path is
// refused rather than pulled through the kernel.
const maxCriteriaBytes = 64 << 10

// NotComputedStatus is the marker of an explicit non-result.
//
// The same word pkg/profiler writes, on purpose: an assistant reading an ODE
// document should meet one vocabulary for "this could not be determined", not one
// per package. The *reasons* are this domain's own, because the profiler's closed
// set is about series and these are about a file in a repository.
const NotComputedStatus = "not_computed"

// CriterionReason is the closed set of reasons a criterion has no verdict.
// Each names a different repair, which is what makes them worth telling apart.
type CriterionReason string

const (
	// ReasonNoCriteriaFile is a commit with no evaluation.yaml in it. The repair is
	// to scaffold one, and the run is not thereby a failure.
	ReasonNoCriteriaFile CriterionReason = "no_criteria_file"
	// ReasonCriteriaUnreadable is a file ODE could not fetch: no checkout, a
	// checkout of another repository, a commit the working copy no longer has, or
	// git refusing. Distinct from a missing file because nothing here knows whether
	// there is one.
	ReasonCriteriaUnreadable CriterionReason = "criteria_unreadable"
	// ReasonCriteriaUnparseable is a file read whole and outside the subset ODE
	// reads. The detail names the line, so the repair is a specific edit.
	ReasonCriteriaUnparseable CriterionReason = "criteria_unparseable"
	// ReasonNoCriterionStated is a file that parses and names no metric.
	ReasonNoCriterionStated CriterionReason = "no_criterion_stated"
	// ReasonNoThreshold is a criterion naming a metric with nothing to compare it
	// against. The value is still reported; only the verdict is withheld.
	ReasonNoThreshold CriterionReason = "no_threshold"
	// ReasonMetricNotReported is a criterion whose metric the run never logged.
	// The scaffold's own comment warns about exactly this, and it is the case a
	// bool would have turned into "the run missed the target".
	ReasonMetricNotReported CriterionReason = "metric_not_reported"
	// ReasonNoDeveloperCredential is a summary built with no developer token —
	// which is every summary the poller builds, because a background poller has no
	// token and §3.1 item 3 does not let it acquire one. The criteria are read when
	// the developer returns.
	ReasonNoDeveloperCredential CriterionReason = "no_developer_credential"
)

// NotComputed is the explicit non-result, in the shape pkg/profiler writes it.
type NotComputed struct {
	Status string          `json:"status"`
	Reason CriterionReason `json:"reason"`
	Detail string          `json:"detail"`
}

func notComputed(reason CriterionReason, format string, args ...any) NotComputed {
	return NotComputed{
		Status: NotComputedStatus, Reason: reason, Detail: fmt.Sprintf(format, args...),
	}
}

// Verdict is §5.13's `met`: true, false, or an explicit non-result.
//
// It marshals as a bare `true` or `false` when there is a verdict, so §5.13's
// documented shape is what a reader sees in the ordinary case, and as a
// `not_computed` object when there is not. There is deliberately no way to build
// one that says "no verdict" without also saying why — the zero value marshals as
// not_computed rather than as false, so a criterion nobody graded cannot be read
// as a criterion that failed.
type Verdict struct {
	met    bool
	known  bool
	status NotComputed
}

// Met and Unmet are the two verdicts.
func Met() Verdict   { return Verdict{met: true, known: true} }
func Unmet() Verdict { return Verdict{known: true} }

// NotEvaluated is the third answer, which a bool could not carry.
func NotEvaluated(reason CriterionReason, format string, args ...any) Verdict {
	return Verdict{status: notComputed(reason, format, args...)}
}

// Known reports whether there is a verdict at all.
func (v Verdict) Known() bool { return v.known }

// IsMet is the verdict, and is only meaningful when Known.
func (v Verdict) IsMet() bool { return v.known && v.met }

// Status describes the non-result. An unset Verdict reports out of scope rather
// than an empty reason, for the reason profiler.Value does: a field nobody
// populated is exactly that, and saying so beats saying nothing.
func (v Verdict) Status() NotComputed {
	if v.known {
		return NotComputed{}
	}
	if v.status.Status == "" {
		return notComputed(ReasonNoCriterionStated, "nothing evaluated this criterion")
	}
	return v.status
}

func (v Verdict) MarshalJSON() ([]byte, error) {
	if v.known {
		return json.Marshal(v.met)
	}
	return json.Marshal(v.Status())
}

func (v *Verdict) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case bytes.Equal(trimmed, []byte("true")):
		*v = Met()
		return nil
	case bytes.Equal(trimmed, []byte("false")):
		*v = Unmet()
		return nil
	case bytes.Equal(trimmed, []byte("null")):
		// Nothing in ODE writes null. A hand-edited fixture might, and reading it as
		// a computed false is the exact confusion this type exists to prevent.
		*v = NotEvaluated(ReasonNoCriterionStated, "the verdict was null on read")
		return nil
	}
	var status NotComputed
	if err := json.Unmarshal(trimmed, &status); err != nil {
		return err
	}
	if status.Status != NotComputedStatus {
		// Only a hand-written fixture can get here, and normalising it is still worth
		// doing: a status this package does not write is a document nobody can rely on,
		// and carrying it through unchanged would let a reader switch on a word ODE
		// never produces.
		status = notComputed(ReasonNoCriterionStated,
			"the verdict was an object with no recognised status (%q)", status.Status)
	}
	*v = Verdict{status: status}
	return nil
}

// Goal is which direction counts as better, as the criteria file states it.
const (
	GoalMinimise = "minimise"
	GoalMaximise = "maximise"
)

// CriteriaDocument is `evaluation.yaml` as ODE reads it.
type CriteriaDocument struct {
	// Primary is the metric a run is judged on — §5.13's `evaluation_criteria`.
	Primary *CriterionSpec
	// Secondary are the metrics the file asks to watch beside it. The scaffold's own
	// file has a `secondary_metrics` key, and dropping it silently would lose part of
	// what the developer wrote.
	Secondary []CriterionSpec
	// Rationale is the developer's own words about why the numbers are what they
	// are. Carried because an assistant proposing a change to a run should be able to
	// read what the criterion is *for* before proposing to miss it.
	Rationale string
}

// CriterionSpec is one criterion as the file states it, before any run is graded.
type CriterionSpec struct {
	Metric string
	// Threshold is only meaningful when HasThreshold. A criterion naming a metric
	// with no threshold is a legitimate thing to write — "watch this" — and grading
	// it against a defaulted zero would be a verdict nobody asked for.
	Threshold    float64
	HasThreshold bool
	// LowerIsBetter is the direction, and GoalStated says whether the file said so
	// or whether it was inferred from the metric's name. The pair travels into the
	// Criterion, so a reader can see which rule produced the verdict.
	LowerIsBetter bool
	GoalStated    bool
}

// Goal renders the direction for a reader.
func (s CriterionSpec) Goal() string {
	if s.LowerIsBetter {
		return GoalMinimise
	}
	return GoalMaximise
}

// criteriaKeys are the spellings each field is accepted under.
//
// Several rather than one, because §5.11 item 3 scaffolds this file and then it is
// the developer's: they rename, they restructure, and a reader that only knew the
// scaffold's exact words would report a perfectly clear file as having no
// criterion. What is *not* guessed is a threshold or a metric that is not there.
var (
	metricKeys    = []string{"metric", "primary_metric", "target_metric"}
	thresholdKeys = []string{"threshold", "target", "limit"}
	goalKeys      = []string{"goal", "direction", "objective", "optimise", "optimize"}
	criteriaKeys  = []string{"criteria", "evaluation_criteria", "criterion"}
	secondaryKeys = []string{"secondary_metrics", "secondary", "watch", "also_report"}
)

// itemMetricKeys are the spellings a metric may have inside a *list* of criteria,
// where `- name: rmse` is the ordinary way to write one.
//
// `name` is here and deliberately not in metricKeys, because the two positions mean
// different things. At the top level of the document `name:` is the operator's own
// name — the scaffold's operator.yaml has one — and reading it as a metric invented
// a criterion out of a metadata field, which then *displaced* the run's own
// evaluation tags. Inside a list item there is nothing else it could be.
//
// `value` is nowhere. It is the most generic key in YAML and reading it as a
// threshold turned any `value: 12` into a target the developer never set.
var itemMetricKeys = append(append([]string{}, metricKeys...), "name")

// minimiseWords and maximiseWords are how a developer writes a direction.
var (
	minimiseWords = []string{"min", "minimise", "minimize", "lower", "lower_is_better",
		"decrease", "down", "less"}
	maximiseWords = []string{"max", "maximise", "maximize", "higher", "higher_is_better",
		"increase", "up", "more", "greater"}
)

// ParseCriteria reads the document. Exported because the parse is the interesting
// half and is worth testing without a pod, a repository or a cluster behind it.
func ParseCriteria(source string) (CriteriaDocument, error) {
	root, err := parseYAML(source)
	if err != nil {
		return CriteriaDocument{}, err
	}
	if root.kind != yamlMapping {
		return CriteriaDocument{}, fmt.Errorf(
			"the file is not a mapping of keys to values, so there is nothing to read a " +
				"metric and a threshold out of")
	}

	document := CriteriaDocument{Rationale: firstText(root, "rationale", "note", "why")}

	// A list form first, because a developer who restructured into one meant it to
	// be the whole answer, and the flat keys beside it would then be leftovers.
	for _, key := range criteriaKeys {
		items := root.items(key)
		if len(items) == 0 {
			continue
		}
		for _, item := range items {
			spec, ok := specOf(item, itemMetricKeys)
			if !ok {
				continue
			}
			if document.Primary == nil {
				primary := spec
				document.Primary = &primary
				continue
			}
			document.Secondary = append(document.Secondary, spec)
		}
		break
	}

	if document.Primary == nil {
		// The flat form. metricKeys rather than itemMetricKeys: at the top level a
		// `name:` belongs to the operator, not to a metric.
		if spec, ok := specOf(root, metricKeys); ok {
			primary := spec
			document.Primary = &primary
		}
	}

	for _, key := range secondaryKeys {
		items := root.items(key)
		if len(items) == 0 {
			continue
		}
		for _, item := range items {
			spec, ok := specOf(item, itemMetricKeys)
			if !ok {
				continue
			}
			if document.Primary != nil && spec.Metric == document.Primary.Metric {
				continue
			}
			document.Secondary = append(document.Secondary, spec)
		}
		break
	}

	return document, nil
}

// specOf reads one criterion out of a node, which may be a mapping or a bare
// metric name.
func specOf(node *yamlNode, metricNames []string) (CriterionSpec, bool) {
	if node == nil {
		return CriterionSpec{}, false
	}
	if node.kind == yamlScalar {
		// A bare name in `secondary_metrics`. A metric with no threshold, which is a
		// real thing to write and is graded as such: reported, never judged.
		metric := strings.TrimSpace(node.scalar)
		if metric == "" {
			return CriterionSpec{}, false
		}
		return CriterionSpec{Metric: metric, LowerIsBetter: lowerIsBetter(metric)}, true
	}
	if node.kind != yamlMapping {
		return CriterionSpec{}, false
	}

	metric := firstText(node, metricNames...)
	if metric == "" {
		return CriterionSpec{}, false
	}
	spec := CriterionSpec{Metric: metric}

	for _, key := range thresholdKeys {
		raw := node.text(key)
		if raw == "" {
			continue
		}
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			// A threshold that is not a number is not a threshold. Left unset rather
			// than defaulted, so the criterion reports no_threshold instead of being
			// graded against zero.
			continue
		}
		spec.Threshold, spec.HasThreshold = parsed, true
		break
	}

	stated := firstText(node, goalKeys...)
	if lower, ok := directionOf(stated); ok {
		spec.LowerIsBetter, spec.GoalStated = lower, true
	} else {
		spec.LowerIsBetter = lowerIsBetter(metric)
	}
	return spec, true
}

// directionOf reads a stated goal.
func directionOf(stated string) (lowerIsBetter, ok bool) {
	value := strings.ToLower(strings.TrimSpace(stated))
	if value == "" {
		return false, false
	}
	for _, word := range minimiseWords {
		if value == word {
			return true, true
		}
	}
	for _, word := range maximiseWords {
		if value == word {
			return false, true
		}
	}
	return false, false
}

func firstText(node *yamlNode, keys ...string) string {
	for _, key := range keys {
		if value := node.text(key); value != "" {
			return value
		}
	}
	return ""
}

// grade turns one criterion and one run's metrics into §5.13's block.
//
// The metric's value travels with the verdict whether or not there is one, because
// a reader told "the criterion could not be evaluated" and shown the number can
// often see why; and the direction rule travels with it for the reason MetricDelta
// carries LowerIsBetter — a verdict whose rule is invisible reads as a judgement.
func grade(spec CriterionSpec, metrics map[string]float64, source string) Criterion {
	criterion := Criterion{
		Metric:        spec.Metric,
		Goal:          spec.Goal(),
		GoalStated:    spec.GoalStated,
		LowerIsBetter: spec.LowerIsBetter,
		Source:        source,
	}
	if spec.HasThreshold {
		threshold := spec.Threshold
		criterion.Threshold = &threshold
	}

	value, reported := metrics[spec.Metric]
	if reported {
		criterion.Value = &value
	}

	switch {
	case !reported:
		criterion.Met = NotEvaluated(ReasonMetricNotReported,
			"the run logged no %s; it logged %s. A criterion whose metric was never "+
				"recorded is not a criterion the run missed",
			spec.Metric, reportedMetrics(metrics))
	case !spec.HasThreshold:
		criterion.Met = NotEvaluated(ReasonNoThreshold,
			"%s names no threshold for %s, so there is a value (%g) and nothing to "+
				"compare it against", EvaluationCriteriaPath, spec.Metric, value)
	case spec.LowerIsBetter && value <= spec.Threshold,
		!spec.LowerIsBetter && value >= spec.Threshold:
		criterion.Met = Met()
	default:
		criterion.Met = Unmet()
	}
	return criterion
}

// reportedMetrics names what the run did log, in a bounded, ordered list. The
// point is a repair: a criterion on `rmse` against a run logging `val_rmse` is a
// typo the developer can see the moment the names are side by side.
func reportedMetrics(metrics map[string]float64) string {
	if len(metrics) == 0 {
		return "no metrics at all"
	}
	names := make([]string, 0, len(metrics))
	for name := range metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	const listed = 12
	elided := 0
	if len(names) > listed {
		elided = len(names) - listed
		names = names[:listed]
	}
	joined := strings.Join(names, ", ")
	if elided > 0 {
		joined = fmt.Sprintf("%s and %d more", joined, elided)
	}
	return joined
}

// criteriaFor fetches and parses the developer's criteria for one run's commit.
//
// It needs the developer's own token, and that is the crux of M9's design rather
// than an inconvenience. The file lives in a working copy on the developer's PVC,
// reachable only through their Hub pod, and §3.1 item 3 says every read on their
// behalf uses their credential — a background poller has none and must never mint
// one. So a summary built without a token carries `no_developer_credential`, which
// is a fact about the summary rather than a fact about the criterion, and the read
// happens when the developer is back.
//
// Read at the run's commit, not at HEAD. A criterion is part of the code state a
// run came from (§5.11 item 7): grading a six-hour run against a threshold the
// developer edited while it ran would be judging it by a rule it never had.
func (s *Service) criteriaFor(
	ctx context.Context, req Request, record Experiment,
) (CriteriaDocument, *NotComputed) {
	if cached, found := s.criteria.get(req.UserSub, record); found {
		return cached.document, cached.problem
	}
	document, problem := s.readCriteria(ctx, req, record)
	s.criteria.put(req.UserSub, record, document, problem)
	return document, problem
}

// readCriteria is the uncached read: a repository status and a `git show` in the
// developer's pod, then a parse.
func (s *Service) readCriteria(
	ctx context.Context, req Request, record Experiment,
) (CriteriaDocument, *NotComputed) {
	if strings.TrimSpace(req.Bearer) == "" {
		problem := notComputed(ReasonNoDeveloperCredential,
			"this summary was built without a developer credential, so %s was not read; "+
				"every repository read is on behalf of the developer (SPEC §3.1 item 3) "+
				"and it is read when they are next connected",
			EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}
	if record.CommitSHA == "" {
		problem := notComputed(ReasonCriteriaUnreadable,
			"the run records no commit, so there is no code state to read %s from",
			EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}

	status, err := s.repo.Status(ctx, repo.StatusRequest{
		Request: repo.Request{
			Bearer: req.Bearer, UserSub: req.UserSub, Author: req.Author,
		},
	})
	if err != nil {
		problem := notComputed(ReasonCriteriaUnreadable,
			"the working copy could not be read: %v", err)
		return CriteriaDocument{}, &problem
	}
	if !status.Cloned {
		problem := notComputed(ReasonCriteriaUnreadable,
			"there is no working copy on this developer's workspace to read %s from",
			EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}
	if record.Repository != "" && status.Link.FullName != record.Repository {
		// The developer has moved on to another repository. Reported rather than read
		// from whatever is checked out now: an evaluation.yaml from a different project
		// is not this run's criterion, and grading against one would be worse than not
		// grading at all.
		problem := notComputed(ReasonCriteriaUnreadable,
			"this run is from %s and the workspace now holds %s, so its %s is not here",
			record.Repository, status.Link.FullName, EvaluationCriteriaPath)
		return CriteriaDocument{}, &problem
	}

	// `git show <commit>:<path>`, which reads the committed state directly and needs
	// no checkout of it — so a developer who has moved to another branch since the
	// launch still gets the criterion the run was submitted with.
	result, err := s.workspace.Command(ctx, req.Bearer, kernel.Command{
		Argv:           []string{"git", "show", record.CommitSHA + ":" + EvaluationCriteriaPath},
		Dir:            status.Link.Path,
		Timeout:        s.opts.CommandTimeout,
		MaxOutputBytes: maxCriteriaBytes,
	})
	if err != nil {
		problem := notComputed(ReasonCriteriaUnreadable,
			"%s could not be read from the workspace: %v", EvaluationCriteriaPath, err)
		return CriteriaDocument{}, &problem
	}
	if result.ExitCode != 0 || result.TimedOut {
		problem := criteriaGitFailure(record.CommitSHA, result)
		return CriteriaDocument{}, &problem
	}
	if result.Truncated {
		problem := notComputed(ReasonCriteriaUnreadable,
			"%s at %s is larger than the %d bytes ODE reads, so it was not parsed rather "+
				"than parsed in part",
			EvaluationCriteriaPath, shortSHA(record.CommitSHA), maxCriteriaBytes)
		return CriteriaDocument{}, &problem
	}

	document, err := ParseCriteria(result.Stdout)
	if err != nil {
		problem := notComputed(ReasonCriteriaUnparseable,
			"%s at %s is outside the YAML subset ODE reads (%v). It is your file and ODE "+
				"does not write it; simplifying the shape is what makes it readable",
			EvaluationCriteriaPath, shortSHA(record.CommitSHA), err)
		return CriteriaDocument{}, &problem
	}
	return document, nil
}

// criteriaGitFailure tells a missing file apart from a commit that is not here.
//
// The two look the same from an exit code and are different facts with different
// repairs, which is the distinction D24 asks to keep. git's own wording is what
// separates them, so it is matched rather than guessed at — and where it matches
// neither, the answer is "unreadable" with git's message rather than a guess at
// "missing".
func criteriaGitFailure(commitSHA string, result kernel.CommandResult) NotComputed {
	stderr := strings.ToLower(firstLine(result.Stderr))
	switch {
	case result.TimedOut:
		return notComputed(ReasonCriteriaUnreadable,
			"reading %s at %s from the workspace timed out",
			EvaluationCriteriaPath, shortSHA(commitSHA))
	case strings.Contains(stderr, "does not exist"),
		strings.Contains(stderr, "exists on disk, but not in"),
		strings.Contains(stderr, "path '"+EvaluationCriteriaPath+"' does not exist"):
		return notComputed(ReasonNoCriteriaFile,
			"the commit %s has no %s. §5.11 item 3 scaffolds one; until there is one, "+
				"ODE has no criterion to grade this run against and does not invent one",
			shortSHA(commitSHA), EvaluationCriteriaPath)
	case strings.Contains(stderr, "unknown revision"),
		strings.Contains(stderr, "bad object"),
		strings.Contains(stderr, "invalid object name"),
		strings.Contains(stderr, "not a valid object name"):
		return notComputed(ReasonCriteriaUnreadable,
			"the working copy does not have commit %s, so its %s cannot be read; the "+
				"branch it was on may have been deleted or rewritten",
			shortSHA(commitSHA), EvaluationCriteriaPath)
	default:
		return notComputed(ReasonCriteriaUnreadable,
			"git could not read %s at %s: %s",
			EvaluationCriteriaPath, shortSHA(commitSHA), firstLine(result.Stderr))
	}
}

// criteriaCache memoises what a commit's evaluation.yaml says.
//
// It exists because reading one is not cheap: a repository status and a `git show`,
// both of them commands executed in the developer's Hub pod, and §5.13's summary is
// now read by a pane, by a tool call and by every interpretation. Four pod commands
// per read of a document that cannot have changed is a cost with nothing to show
// for it.
//
// What makes a cache correct here rather than a source of stale answers is that the
// key is a **commit**. The tree at a commit is immutable by construction, so the
// file at that path either was there or was not, and it either parsed or did not.
// The developer editing evaluation.yaml produces a new commit and a new key; a
// force-push that removes the commit produces a read failure, which is not cached.
//
// So only the immutable outcomes are kept: the parsed document, a commit that has
// no such file, and a file outside the subset ODE reads. Everything under
// `criteria_unreadable` and `no_developer_credential` is a fact about *this
// moment* — no checkout yet, another repository selected, the pod not up, no token
// on the request — and caching one would turn a transient condition into a
// permanent answer.
type criteriaCache struct {
	mux     sync.Mutex
	entries map[string]criteriaEntry
}

type criteriaEntry struct {
	document CriteriaDocument
	problem  *NotComputed
}

// maxCachedCriteria bounds it. One entry per developer per commit they have
// launched from, which in practice is a handful; the cap is what stops a long-lived
// process accumulating one per commit in a repository's history.
const maxCachedCriteria = 256

func (c *criteriaCache) key(userSub string, record Experiment) string {
	// The subject is in the key, not merely checked: two developers on the same
	// commit of the same repository legitimately have different working copies, and
	// a key without it would serve one of them the other's file.
	return userSub + "\x00" + record.Repository + "\x00" + record.CommitSHA
}

func (c *criteriaCache) get(userSub string, record Experiment) (criteriaEntry, bool) {
	if record.CommitSHA == "" {
		return criteriaEntry{}, false
	}
	c.mux.Lock()
	defer c.mux.Unlock()
	entry, found := c.entries[c.key(userSub, record)]
	return entry, found
}

func (c *criteriaCache) put(
	userSub string, record Experiment, document CriteriaDocument, problem *NotComputed,
) {
	if record.CommitSHA == "" {
		return
	}
	if problem != nil && !cacheableProblem(problem.Reason) {
		return
	}

	c.mux.Lock()
	defer c.mux.Unlock()
	if c.entries == nil {
		c.entries = map[string]criteriaEntry{}
	}
	if len(c.entries) >= maxCachedCriteria {
		// Cleared rather than evicted one by one. A miss costs a re-read of a file
		// that is a few hundred bytes, and an LRU here would be machinery in aid of
		// nothing — the working set is a developer's recent commits.
		c.entries = map[string]criteriaEntry{}
	}
	c.entries[c.key(userSub, record)] = criteriaEntry{document: document, problem: problem}
}

// cacheableProblem is the distinction the cache turns on: a property of the
// commit, or a property of this moment.
func cacheableProblem(reason CriterionReason) bool {
	switch reason {
	case ReasonNoCriteriaFile, ReasonCriteriaUnparseable, ReasonNoCriterionStated:
		return true
	default:
		return false
	}
}
