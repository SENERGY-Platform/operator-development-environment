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

// Package interpret is §5.13's last two sentences: the backend injects a finished
// run's summary into the conversation it came from, the assistant interprets it
// and proposes the next adjustment, and **the developer accepts, edits or
// rejects** (M9).
//
// It is its own package because of what it has to touch. Building the summary is
// pkg/experiments; running a turn is pkg/chat; and pkg/chat already depends on
// pkg/tools, which depends on pkg/experiments. Putting the delivery in either of
// them would close that cycle, so it lives above both and neither knows about it.
//
// # The token, which is the whole design
//
// An interpretation turn dispatches tools, and every platform read is on behalf of
// the developer (D5, §3.1 item 3). A background poller has no token and must never
// acquire one of its own — a service account reading a developer's devices is the
// one thing §3.1 rules out by name. But a run finishes when it finishes, which is
// routinely while nobody is connected. Those two facts pull in opposite directions
// and the split between them is this package's shape:
//
//   - **The summary is produced with the service credential and stored the moment
//     the run is terminal**, connected or not. §3.1 item 5 permits a service account
//     for exactly Ray and MLflow, which is all a summary reads. Nothing about
//     producing it waits for anybody.
//
//   - **The interpretation turn runs only with a real developer token.** The SPA
//     holds one and refreshes it over the WebSocket, and pkg/api registers that
//     living credential here for as long as the connection is up. When a developer
//     is connected the turn runs immediately; when they are not, the summary waits
//     and the turn runs the moment they come back.
//
// The consequence worth stating: a developer who was away comes back to the
// interpretation, not to a conversation that quietly skipped it. What they never
// get is an assistant that read their devices while they were asleep.
//
// # An automated turn is still a turn
//
// It goes through chat.Engine.SendInjected, which is chat.Engine.Send with two
// differences and no exemptions. The §3.3 spend cap is checked before anything is
// stored; the one-exchange-at-a-time rule refuses a second turn on a busy session;
// the session's exposure tier gates every tool the same way. A refusal leaves the
// run pending and it is tried again — nothing is dropped and nothing is forced.
//
// # The proposal is a proposal (D28)
//
// What the assistant names as the next step is recorded, shown, and binding on
// nothing. The developer's answer — accept, edit, reject — is an append-only
// record in the shape pkg/relations writes a rule decision and pkg/profiler writes
// an override: keyed by what was proposed rather than by the interpretation it
// appeared in, so a rejected proposal stays rejected when the same run is
// interpreted again. Nothing here writes evaluation.yaml, nothing here launches
// anything, and §5.8 has no tool that could.
package interpret

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

var (
	// ErrNotFound is an interpretation this developer does not have. Ownership is
	// checked before existence, so it is also the answer for another developer's.
	ErrNotFound = errors.New("no interpretation for this experiment")
	// ErrInvalidRequest is a decision ODE refused to record.
	ErrInvalidRequest = errors.New("invalid interpretation request")
)

// Interpretation is one finished run, as read back.
//
// The summary and the assistant's words are both here because they are only
// meaningful together: a proposal without the numbers it came from is advice with
// no basis, and §5.13's summary without the reading of it is what M8 already had.
type Interpretation struct {
	ExperimentID string `json:"experiment_id"`
	RunID        string `json:"run_id"`
	SessionID    string `json:"session_id"`
	UserSub      string `json:"-"`

	// Summary is §5.13's structured document, as it was injected.
	Summary experiments.Summary `json:"summary"`
	// SummaryAt is when it was built, which is when the run became terminal — not
	// when the developer came back to it.
	SummaryAt time.Time `json:"summary_at"`

	// Interpretation is what the assistant said, in full. Empty until the turn has
	// run, which is what InterpretedAt being zero means.
	Interpretation string     `json:"interpretation"`
	InterpretedAt  *time.Time `json:"interpreted_at,omitempty"`

	// Proposal is the concrete next adjustment, or an explicit non-result.
	Proposal Proposal `json:"proposal"`

	// Decision is the developer's answer, if they have given one. The latest record
	// in the log, merged at read time — never written into this document, for the
	// reason §5.4.3 gives about the override overlay: the computed artifact and the
	// human judgement stay separable, and "the assistant proposed X and the developer
	// rejected it" is the finding.
	Decision *ProposalDecision `json:"decision,omitempty"`
	// Decisions is the whole log for this proposal, oldest first. A developer who
	// changed their mind added a record rather than replacing one.
	Decisions []ProposalDecision `json:"decisions"`
}

// Pending reports whether the assistant has yet to read the summary.
func (i Interpretation) Pending() bool { return i.InterpretedAt == nil }

// Proposal is the next adjustment the assistant named, or why there is none.
//
// A struct rather than a string because "the assistant proposed nothing" and "the
// assistant proposed the empty string" are different facts, and D24's rule applies
// to an absent proposal exactly as it applies to an absent metric: an empty string
// read as "no change needed" would be a finding nobody made.
type Proposal struct {
	// ID is a fingerprint of what was proposed, for this experiment.
	//
	// Keyed on the text rather than on the interpretation it appeared in, which is
	// what makes a decision survive a recomputation — the same choice
	// relations.RuleDecision makes for the same reason. Re-interpreting a run that
	// produces the same proposal produces the same id, so a rejection still stands;
	// a materially different proposal is a different id and is asked about again.
	ID string `json:"proposal_id,omitempty"`
	// Text is the adjustment, in the assistant's own words.
	Text string `json:"text,omitempty"`
	// Status is empty when there is a proposal, and "not_computed" with a reason
	// when the assistant named none.
	Status string `json:"status,omitempty"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Stated reports whether there is a proposal at all.
func (p Proposal) Stated() bool { return p.Text != "" }

// Reasons a turn produced no concrete next step.
const (
	// ReasonNoProposal is a reply that read the numbers and named no adjustment.
	ReasonNoProposal = "no_proposal_stated"
	// ReasonNotInterpreted is a summary the assistant has not answered yet, because
	// the developer has not been connected since the run finished.
	ReasonNotInterpreted = "not_interpreted_yet"
)

func unstatedProposal(reason, detail string) Proposal {
	return Proposal{Status: experiments.NotComputedStatus, Reason: reason, Detail: detail}
}

// proposalID fingerprints a proposal for one experiment.
//
// Normalised on whitespace and case so that a re-interpretation wording the same
// adjustment with different spacing is recognised as the same one; not normalised
// any further than that, because two proposals that differ in a number are two
// proposals and the developer should be asked about both.
func proposalID(experimentID, text string) string {
	normalised := strings.ToLower(strings.Join(strings.Fields(text), " "))
	if normalised == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(experimentID + "\n" + normalised))
	return hex.EncodeToString(sum[:])[:16]
}

// The three answers of §5.13's last sentence.
const (
	// DecisionAccepted is the developer taking the proposal as it stands. It records
	// their agreement and changes nothing else — launching the next run is still an
	// act they perform.
	DecisionAccepted = "accepted"
	// DecisionEdited is the developer taking a different form of it, which is the
	// answer that carries the most information and the one a bare yes/no would lose.
	DecisionEdited = "edited"
	// DecisionRejected is the developer declining it. Recorded so the same proposal
	// does not come back as though it had never been answered.
	DecisionRejected = "rejected"
)

// ProposalDecision is one developer's verdict on one proposal (§5.13, D28).
//
// Append-only, in the shape pkg/relations writes a rule decision and pkg/profiler
// an override, and for the same three reasons those give: recomputation is
// non-destructive, proposed-versus-decided stays diffable, and the log is an
// empirical record — "the assistant proposed X, the developer edited it to Y" is a
// paper finding that a mutable document destroys.
//
// It has no tool. §5.8 denies every capability that would let a model record a
// developer's judgement, and this is one: a model that could accept its own
// proposal would be grading its own work.
type ProposalDecision struct {
	DecisionID string    `json:"decision_id"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by"`

	ExperimentID string `json:"experiment_id"`
	// RunID records which MLflow run the proposal was about, so the log is readable
	// after ODE's own record of the experiment is gone.
	RunID string `json:"run_id,omitempty"`
	// ProposalID is what the decision is keyed by — see Proposal.ID.
	ProposalID string `json:"proposal_id"`

	Decision string `json:"decision"`
	// Proposed is the assistant's own wording, copied in rather than referenced.
	// The record has to stay readable after the interpretation it came from has been
	// evicted from memory, and "what was actually proposed" is half the finding.
	Proposed string `json:"proposed"`
	// Edited is the developer's own form of the adjustment. Required for an edit and
	// empty otherwise, because an edit whose content was not recorded is a rejection
	// with extra steps.
	Edited string `json:"edited,omitempty"`
	Note   string `json:"note,omitempty"`

	// Binding is always false, and is serialised rather than omitted so that a
	// reader meets D28 rather than having to know it: accepting a proposal records
	// agreement and changes nothing. Promoting a value into evaluation.yaml or the
	// operator config is a separate act the developer performs themselves, and §5.8
	// has no tool for it either.
	Binding bool `json:"binding"`
}

// StaleProposalError is a decision on a proposal that no longer stands.
//
// Its own type because the answer differs from every other refusal: the request
// was well formed and the world moved under it, so the pane re-reads rather than
// fixes the body. Recording it anyway would be worse than refusing — the developer
// would have accepted something they never saw.
type StaleProposalError struct {
	Decided string
	Current string
}

func (e *StaleProposalError) Error() string {
	if e.Current == "" {
		return fmt.Sprintf(
			"the proposal %s was decided on, and there is no proposal on this run now",
			e.Decided)
	}
	return fmt.Sprintf(
		"the proposal has changed since it was read: %s stands now and the decision "+
			"named %s. Read the interpretation again before deciding",
		e.Current, e.Decided)
}

// Unwrap makes it an ErrInvalidRequest for a caller that only needs the class,
// while errors.As still separates it for the one that needs the status code.
func (e *StaleProposalError) Unwrap() error { return ErrInvalidRequest }

// Validate is the guard every path into the log goes through.
func (d ProposalDecision) Validate() error {
	if strings.TrimSpace(d.ExperimentID) == "" {
		return fmt.Errorf("%w: experiment_id must name the run the proposal was about",
			ErrInvalidRequest)
	}
	if strings.TrimSpace(d.ProposalID) == "" {
		return fmt.Errorf(
			"%w: proposal_id must name the proposal being decided; a decision on "+
				"\"whatever was last proposed\" could not survive a recomputation",
			ErrInvalidRequest)
	}
	switch d.Decision {
	case DecisionAccepted, DecisionEdited, DecisionRejected:
	default:
		return fmt.Errorf("%w: decision must be %s, %s or %s, got %q",
			ErrInvalidRequest, DecisionAccepted, DecisionEdited, DecisionRejected, d.Decision)
	}
	if d.Decision == DecisionEdited && strings.TrimSpace(d.Edited) == "" {
		return fmt.Errorf(
			"%w: an edited proposal must carry the developer's own form of it; without "+
				"one there is nothing recorded but a disagreement", ErrInvalidRequest)
	}
	if d.Decision != DecisionEdited && strings.TrimSpace(d.Edited) != "" {
		return fmt.Errorf("%w: only an edit carries an edited form", ErrInvalidRequest)
	}
	if strings.TrimSpace(d.CreatedBy) == "" {
		return fmt.Errorf("%w: created_by must identify the developer", ErrInvalidRequest)
	}
	if d.Binding {
		// Unreachable from any route, and refused here rather than merely documented:
		// D28 is a property of the system, and a field nothing sets is a field
		// something will set eventually.
		return fmt.Errorf(
			"%w: a decision is never binding; promoting a value into %s or the operator "+
				"config is a separate developer action (SPEC D28, §5.8)",
			ErrInvalidRequest, experiments.EvaluationCriteriaPath)
	}
	return nil
}
