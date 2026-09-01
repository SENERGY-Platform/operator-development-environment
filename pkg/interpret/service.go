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

package interpret

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
)

// Experiments is what this package needs of pkg/experiments. Narrow, because an
// interface is a statement about what this package may do: it summarises and it
// reads, and there is deliberately no method here that could launch anything.
type Experiments interface {
	// Summarise builds §5.13's summary with ODE's own Ray and MLflow credential and
	// nobody connected (§3.1 item 5).
	Summarise(ctx context.Context, record experiments.Experiment) (experiments.Summary, error)
	// Results builds the same summary on behalf of a developer, which is what adds
	// the evaluation criteria — those live in their working copy and are read with
	// their token (§3.1 item 3).
	Results(ctx context.Context, req experiments.Request, id string) (experiments.Summary, error)
	// Record is the experiment itself, under the caller's own ownership check. Named
	// Record rather than Get because this package reads one thing from it — which
	// session and which run — and the name should say so.
	Record(ctx context.Context, req experiments.Request, id string) (experiments.Experiment, error)
}

// Conversations is what this package needs of pkg/chat.
type Conversations interface {
	Session(ctx context.Context, sub, id string) (chat.Session, error)
	Messages(ctx context.Context, sub, id string) ([]chat.StoredMessage, error)
	SendInjected(ctx context.Context, token chat.TokenSource, sub, sessionID string,
		message chat.InjectedMessage) (*chat.Exchange, error)
	Continue(ctx context.Context, token chat.TokenSource, sub, sessionID string) (*chat.Exchange, error)
}

// IDs mints decision identifiers, the same shape every other package uses.
type IDs interface{ NewID() string }

// Options is how a deployment tunes the delivery.
type Options struct {
	// RetryInterval is how often the pending runs of connected developers are tried
	// again. It exists because the reasons a turn is refused are transient: a session
	// already running an exchange, a spend cap that resets, a developer who has not
	// come back yet.
	RetryInterval time.Duration
	// TurnTimeout bounds how long one interpretation turn is waited for before the
	// run is left pending. Generous: the turn may run tool calls, and chat's own
	// ExchangeTimeout is the real ceiling.
	TurnTimeout time.Duration
	// MaxPending bounds the queue. A cluster that finished a thousand jobs while
	// every developer was away must not turn into a thousand held summaries.
	MaxPending int
	// MaxAttempts bounds how many passes may fail on one run before it is let go of
	// *in this process*. Zero takes the default.
	//
	// Letting go is not giving up: the run is dropped from the queue without being
	// marked interpreted, so the poller offers it again for as long as it is inside
	// its window, and the durable evidence that it was already delivered is the
	// injected message rather than anything held here. What the bound actually
	// prevents is a run whose session is permanently wedged sitting in the queue
	// forever and costing a conversation read on every tick.
	MaxAttempts int
}

const (
	defaultRetryInterval = 30 * time.Second
	defaultTurnTimeout   = 10 * time.Minute
	defaultMaxPending    = 200
	defaultMaxAttempts   = 10
)

// Deps is what the service is built from.
type Deps struct {
	Experiments Experiments
	Chat        Conversations
	Store       Store
	IDs         IDs
	Options
}

// Service delivers a finished run into the conversation it came from.
type Service struct {
	experiments Experiments
	chat        Conversations
	store       Store
	ids         IDs
	opts        Options
	now         func() time.Time

	presence *presence

	mux sync.Mutex
	// pending is the runs whose summary is built and whose turn has not run. In
	// memory on purpose: it is recomputable from the experiment store, which is what
	// the poller's second phase re-reads on every tick and after every restart.
	pending map[string]*held
	// interpreted is what this process has already delivered, so the poller offering
	// the same run every tick costs a map lookup rather than a read of the
	// conversation.
	//
	// A cache and not a record. The durable answer is the injected message in the
	// conversation, which deliveryState reads, so losing this map costs one extra
	// read per run and nothing else — which is what lets it be cleared when it grows
	// rather than kept forever.
	interpreted map[string]bool

	// work carries a nudge to the delivery loop. Buffered and best-effort: a missed
	// nudge costs one retry interval, and blocking a caller — the poller, or a
	// developer's WebSocket handshake — would be worse.
	work chan struct{}
	// stopped closes when the loop has returned.
	stopped chan struct{}

	// passMux serialises Deliver with itself. The loop calls it, and so may a
	// caller — a test, or a future route — and two passes overlapping would let one
	// inject a summary the other was about to inject. Separate from mux, which
	// guards the queue for the moment it takes to read or write it and is never held
	// across a turn.
	passMux sync.Mutex
}

// held is one run waiting for a developer.
type held struct {
	record  experiments.Experiment
	summary experiments.Summary
	// summaryAt is when the summary was built, which is when the run finished rather
	// than when anybody looked.
	summaryAt time.Time
	// attempts counts refused deliveries, for the log line that says why a run is
	// still waiting.
	attempts int
	// silent counts turns that ran and produced no reply, which is a different
	// failure from a refusal and is bounded separately — see errSilentTurn.
	silent int
}

// maxSilentTurns is how many times a turn may run and say nothing before the run
// is let go of. Small, because each one is a provider call charged to the
// developer and a model that answered nothing twice will answer nothing again.
const maxSilentTurns = 3

// New builds the service.
func New(deps Deps) (*Service, error) {
	if deps.Experiments == nil {
		return nil, errors.New("interpret: an experiment service is required")
	}
	if deps.Chat == nil {
		return nil, errors.New(
			"interpret: a chat engine is required: §5.13 injects the summary into a " +
				"conversation, and without one there is nowhere for it to go")
	}
	if deps.Store == nil {
		return nil, errors.New(
			"interpret: a decision store is required, because a proposal the developer " +
				"answered has to stay answered (§5.13, D28)")
	}
	if deps.IDs == nil {
		return nil, errors.New("interpret: an id source is required")
	}
	opts := deps.Options
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = defaultRetryInterval
	}
	if opts.TurnTimeout <= 0 {
		opts.TurnTimeout = defaultTurnTimeout
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = defaultMaxPending
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultMaxAttempts
	}
	return &Service{
		experiments: deps.Experiments,
		chat:        deps.Chat,
		store:       deps.Store,
		ids:         deps.IDs,
		opts:        opts,
		now:         func() time.Time { return time.Now().UTC() },
		presence:    newPresence(),
		pending:     map[string]*held{},
		interpreted: map[string]bool{},
		work:        make(chan struct{}, 1),
		stopped:     make(chan struct{}),
	}, nil
}

// Start runs the delivery loop until ctx ends.
//
// One goroutine for the whole deployment, in the lifecycle idiom kernel.Service
// and experiments.Poller use: ctx is the process's, so shutdown stops it, and
// Stopped closes when it has actually returned rather than when it was asked to.
func (s *Service) Start(ctx context.Context) {
	go func() {
		defer close(s.stopped)
		ticker := time.NewTicker(s.opts.RetryInterval)
		defer ticker.Stop()
		slog.InfoContext(ctx, "result interpretation started",
			"retry_interval", s.opts.RetryInterval, "turn_timeout", s.opts.TurnTimeout)
		for {
			select {
			case <-ticker.C:
				s.Deliver(ctx)
			case <-s.work:
				s.Deliver(ctx)
			case <-ctx.Done():
				slog.InfoContext(ctx, "result interpretation stopped")
				return
			}
		}
	}()
}

// Stopped closes when the loop has returned.
func (s *Service) Stopped() <-chan struct{} { return s.stopped }

// Connected registers a developer's live credential for as long as their
// connection is up, and returns the function that withdraws it.
//
// Called by the WebSocket handler. Registering here is what turns "a developer is
// back" into "the runs that finished while they were away can be interpreted now",
// and the nudge is why that happens in seconds rather than at the next tick.
func (s *Service) Connected(userSub string, token chat.TokenSource) func() {
	remove := s.presence.add(userSub, token)
	s.nudge()
	return remove
}

// nudge asks the loop to run a pass, without blocking if one is already queued.
func (s *Service) nudge() {
	select {
	case s.work <- struct{}{}:
	default:
	}
}

// RunFinished implements experiments.TerminalSink.
//
// It returns immediately, as the interface requires: the summary is built and the
// turn is run on the delivery loop, and both are far longer than a poller tick.
func (s *Service) RunFinished(_ context.Context, record experiments.Experiment) {
	if record.SessionID == "" || record.RunID == "" {
		return
	}

	s.mux.Lock()
	if s.interpreted[record.ID] {
		s.mux.Unlock()
		return
	}
	if _, waiting := s.pending[record.ID]; waiting {
		s.mux.Unlock()
		return
	}
	if len(s.pending) >= s.opts.MaxPending {
		s.mux.Unlock()
		slog.WarnContext(context.Background(),
			"the interpretation queue is full; a finished run is not held",
			"experiment", record.ID, "max_pending", s.opts.MaxPending)
		return
	}
	// Held without a summary. The summary is built by the loop, with ODE's own
	// credential, before anything waits for a developer — see summarise.
	s.pending[record.ID] = &held{record: record}
	s.mux.Unlock()

	s.nudge()
}

// PendingExperiments is the ids this process is holding, for a test and for a
// future operator surface. It says nothing about whether a run was interpreted —
// that lives in the conversation, not here.
func (s *Service) PendingExperiments() []string {
	s.mux.Lock()
	defer s.mux.Unlock()
	out := make([]string, 0, len(s.pending))
	for id := range s.pending {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Deliver is one pass: build the summaries that are missing, then run the turns
// that a live credential allows. Exported so a test can drive it without a timer.
func (s *Service) Deliver(ctx context.Context) {
	s.passMux.Lock()
	defer s.passMux.Unlock()

	for _, item := range s.snapshot() {
		if ctx.Err() != nil {
			return
		}
		s.summarise(ctx, item)
	}

	for _, userSub := range s.presence.connected() {
		if ctx.Err() != nil {
			return
		}
		s.deliverFor(ctx, userSub)
	}
}

// snapshot copies the queue so the pass holds no lock while it works.
func (s *Service) snapshot() []*held {
	s.mux.Lock()
	defer s.mux.Unlock()
	out := make([]*held, 0, len(s.pending))
	for _, item := range s.pending {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].record.SubmittedAt.Before(out[j].record.SubmittedAt)
	})
	return out
}

// summarise builds §5.13's summary with the service credential, once.
//
// This is the half that must not wait for anybody. §3.1 item 5 permits a service
// account for exactly Ray and MLflow, which is all a summary reads, so a run that
// finishes at three in the morning is summarised at three in the morning and the
// developer finds a result rather than the beginning of one. The evaluation
// criteria are the exception and say so themselves: they are in the developer's
// working copy, they are read with the developer's token, and until then the
// criterion carries `no_developer_credential` rather than a verdict.
func (s *Service) summarise(ctx context.Context, item *held) {
	s.mux.Lock()
	done := item.summaryAt
	s.mux.Unlock()
	if !done.IsZero() {
		return
	}

	summary, err := s.experiments.Summarise(ctx, item.record)
	if err != nil {
		slog.WarnContext(ctx, "a finished run could not be summarised",
			"experiment", item.record.ID, "error", err)
		return
	}

	s.mux.Lock()
	item.summary = summary
	item.summaryAt = s.now()
	s.mux.Unlock()

	slog.InfoContext(ctx, "a finished run was summarised and is waiting for its developer",
		"experiment", item.record.ID, "session", item.record.SessionID,
		"status", summary.Status)
}

// deliverFor runs the turns for one developer's pending runs.
//
// Serially, on the delivery loop's own goroutine, which is a deliberate ceiling
// rather than an oversight: one turn at a time across the whole deployment means a
// burst of finished runs cannot become a burst of provider calls charged to
// several developers at once, and TurnTimeout bounds how long any one of them
// holds the loop. The cost, stated: a turn that runs its full budget delays every
// other developer's delivery by that much. Their runs are not lost — they are
// still pending, and the next pass takes them — so the failure mode is lateness
// rather than a missed interpretation.
func (s *Service) deliverFor(ctx context.Context, userSub string) {
	for _, item := range s.snapshot() {
		if ctx.Err() != nil {
			return
		}
		if item.record.UserSub != userSub {
			continue
		}
		// Read again for every run, not once for the pass. A turn can take minutes,
		// and a developer with five waiting runs may close the tab during the first
		// one — after which there is no live credential and the rest must wait rather
		// than being interpreted with a token nobody is behind any more (§3.1 item 3,
		// and the promise in pkg/api/ws.go).
		token, live := s.presence.token(userSub)
		if !live {
			return
		}
		s.mux.Lock()
		ready := !item.summaryAt.IsZero()
		s.mux.Unlock()
		if !ready {
			continue
		}
		if err := s.runTurn(ctx, item, token); err != nil {
			s.mux.Lock()
			item.attempts++
			attempts := item.attempts
			s.mux.Unlock()
			// Not an error line: every reason this fails is a condition that resolves —
			// a session already running a turn, a spend cap that resets, a repository
			// briefly unreachable. It is retried, and an ERROR here would page somebody
			// for a developer who was mid-conversation.
			slog.InfoContext(ctx, "a finished run is still waiting to be interpreted",
				"experiment", item.record.ID, "attempts", attempts, "reason", err)
			if errors.Is(err, errSilentTurn) {
				s.mux.Lock()
				item.silent++
				silent := item.silent
				s.mux.Unlock()
				if silent >= maxSilentTurns {
					// Retired rather than dropped: the turns did run, they were charged to
					// this developer, and offering the run again would buy more of the same.
					// The summary stays in the conversation, which is where they will see
					// that it was never read.
					s.retire(item.record.ID)
					slog.WarnContext(ctx, "a finished run's interpretation turn produced no "+
						"reply and is not being retried further; its summary is in the "+
						"conversation with no reading of it",
						"experiment", item.record.ID, "session", item.record.SessionID,
						"turns", silent)
				}
				continue
			}
			if attempts >= s.opts.MaxAttempts {
				// Let go of it here without marking it interpreted, so the poller may
				// still offer it while it is recent. See Options.MaxAttempts.
				s.drop(item.record.ID)
				slog.WarnContext(ctx, "a finished run has been dropped from the "+
					"interpretation queue after repeated refusals; it is offered again while "+
					"it is inside the poller's window",
					"experiment", item.record.ID, "session", item.record.SessionID,
					"attempts", attempts, "reason", err)
			}
			continue
		}
	}
}

// runTurn is the delivery of one run: inject, interpret, record.
func (s *Service) runTurn(ctx context.Context, item *held, token chat.TokenSource) error {
	record := item.record
	// Copied under the lock and used without it. The turn below blocks for as long
	// as the assistant takes, and holding the queue's lock across that would stop
	// the poller handing over anything else that finished meanwhile.
	s.mux.Lock()
	summary := item.summary
	s.mux.Unlock()

	// Rebuilt with the developer's own credential, which is the whole reason this
	// runs here rather than when the run finished.
	//
	// The held summary was built by the poller with ODE's Ray and MLflow service
	// account, and its evaluation criteria therefore say `no_developer_credential`
	// — evaluation.yaml is in the developer's working copy and is read on their
	// behalf (§3.1 item 3). Injecting *that* document would hand the model a summary
	// whose criterion is permanently un-evaluated while the pane beside it showed the
	// real verdict: two answers to "did this run meet the target", and the one the
	// assistant reasons from would be the wrong one. So the moment there is a token,
	// the summary is built again with it.
	//
	// A failure here falls back to the held summary rather than delaying the turn.
	// That is the honest degradation: the criterion then says why it has no verdict,
	// which is exactly what it is for, and the developer still gets an interpretation
	// of the metrics.
	// Ownership, and that the session still exists. A conversation the developer
	// deleted is not an error worth retrying, so it retires the run.
	//
	// Read before the summary rather than after it, because the session carries the
	// exposure tier D34's extract is masked at, and a summary built for one tier and
	// injected at another would be the one bug this whole path is about.
	session, err := s.chat.Session(ctx, record.UserSub, record.SessionID)
	if err != nil {
		if errors.Is(err, chat.ErrNoSuchSession) {
			s.retire(record.ID)
			return nil
		}
		return err
	}

	if graded, err := s.experiments.Results(ctx, experiments.Request{
		Bearer:    token(),
		UserSub:   record.UserSub,
		SessionID: record.SessionID,
	}, record.ID); err == nil {
		summary = graded
	} else {
		slog.WarnContext(ctx, "the developer's evaluation criteria could not be read for "+
			"an interpretation; the summary is injected with the criterion left unevaluated",
			"experiment", record.ID, "error", err)
	}

	messages, err := s.chat.Messages(ctx, record.UserSub, record.SessionID)
	if err != nil {
		return err
	}
	injectedAt, answered := deliveryState(messages, record.ID)

	switch {
	case injectedAt >= 0 && answered:
		// Already done, in a previous process or by a route. Recorded from what is in
		// the conversation, so a restart does not lose the proposal the developer is
		// about to decide on.
		s.record(ctx, item, messages, injectedAt)
		return nil

	case injectedAt >= 0:
		// The summary is in the conversation and nothing answered it — ODE stopped
		// between the two. Continued rather than re-injected: a second copy of the
		// same summary would be ODE talking over itself.
		exchange, err := s.chat.Continue(ctx, token, record.UserSub, record.SessionID)
		if err != nil {
			return err
		}
		if err := s.wait(ctx, exchange); err != nil {
			return err
		}

	default:
		// The §3.3 cap, the tier gate and the one-exchange-at-a-time rule are all
		// inside this call. A refusal here means nothing was stored — see
		// chat.Engine.start — so the conversation is untouched and the run is retried.
		exchange, err := s.chat.SendInjected(ctx, token, record.UserSub, record.SessionID,
			chat.InjectedMessage{
				// Masked at the session's own tier (D34). The extract as built is what a
				// failed run raised, values and all, and this turn is the other of the two
				// paths a summary takes into a model's context — pkg/tools is the first.
				// Both mask at the boundary, and both have a test that fails if the call
				// goes missing.
				Text:    injectedMessage(summary.MaskedFor(session.Tier), record),
				Subject: record.ID,
			})
		if err != nil {
			return err
		}
		if err := s.wait(ctx, exchange); err != nil {
			return err
		}
	}

	// Re-read rather than taken from the exchange's event stream: the store is what
	// the developer sees, and reading the same thing they do means the recorded
	// interpretation cannot disagree with the conversation.
	messages, err = s.chat.Messages(ctx, record.UserSub, record.SessionID)
	if err != nil {
		return err
	}
	injectedAt, answered = deliveryState(messages, record.ID)
	if injectedAt < 0 {
		return fmt.Errorf("the injected summary for %s is not in the conversation", record.ID)
	}
	if !answered {
		// The turn ran and the assistant said nothing — a provider error, an empty
		// completion, a loop that only dispatched tools. Retiring here would mark the
		// run interpreted on the strength of a turn that interpreted nothing, and the
		// developer would be left with a summary in their conversation and no reading
		// of it, permanently. Reported so the retry path takes it instead.
		return errSilentTurn
	}
	s.record(ctx, item, messages, injectedAt)
	return nil
}

// errSilentTurn is a turn that ran and produced no interpretation.
//
// Its own value because the retry it deserves is different from a refusal's. A
// refusal — a busy session, a spent cap — costs nothing and resolves on its own, so
// it is worth retrying many times. A turn that ran and said nothing has already
// been paid for, and re-prompting a model that keeps saying nothing is not going to
// improve, so it is retried a few times and then let go of with a warning.
var errSilentTurn = errors.New("the interpretation turn produced no reply")

// wait blocks until the turn ends or the budget is spent.
func (s *Service) wait(ctx context.Context, exchange *chat.Exchange) error {
	if exchange == nil {
		return errors.New("the chat engine started no exchange")
	}
	timer := time.NewTimer(s.opts.TurnTimeout)
	defer timer.Stop()
	select {
	case <-exchange.Done():
		return nil
	case <-timer.C:
		// The exchange keeps running — it is detached, and stopping a turn the
		// developer can see would be worse than not recording its proposal yet. The
		// next pass reads the conversation and finds the answer.
		return errors.New("the interpretation turn is still running")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// record retires the run once the conversation carries an interpretation of it.
//
// It stores nothing of the interpretation itself, on purpose. The assistant's
// words are already durable — they are chat messages, in the conversation the
// developer reads — and a second copy in a table of this package's own would be
// two records of one exchange that could disagree. Interpretation() reads them
// back and re-derives the proposal, which is the same split §5.4.3 makes between a
// computed artifact and the log of human judgement beside it.
func (s *Service) record(
	ctx context.Context, item *held, messages []chat.StoredMessage, injectedAt int,
) {
	proposal := extractProposal(item.record.ID, assistantReply(messages, injectedAt))
	s.retire(item.record.ID)
	slog.InfoContext(ctx, "a finished run was interpreted",
		"experiment", item.record.ID, "session", item.record.SessionID,
		"proposed", proposal.Stated())
}

// Interpretation reads one run's interpretation back, on behalf of a developer.
//
// Recomputed rather than stored: §5.13's summary from MLflow, the assistant's own
// words from the conversation, the proposal re-derived from those words, and the
// developer's decisions merged from the append-only log. Every part of it is
// either recoverable from a source of truth or is a human judgement in Postgres,
// which is the split the rest of ODE makes (§5.4.3).
//
// Read with the caller's token, so the criteria in the summary are graded here even
// when the run finished while nobody was connected.
func (s *Service) Interpretation(
	ctx context.Context, req experiments.Request, experimentID string,
) (Interpretation, error) {
	record, err := s.experiments.Record(ctx, req, experimentID)
	if err != nil {
		return Interpretation{}, err
	}
	summary, err := s.experiments.Results(ctx, req, experimentID)
	if err != nil {
		return Interpretation{}, err
	}

	out := Interpretation{
		ExperimentID: record.ID,
		RunID:        record.RunID,
		SessionID:    record.SessionID,
		UserSub:      record.UserSub,
		Summary:      summary,
		SummaryAt:    s.summaryTime(record),
		Decisions:    []ProposalDecision{},
	}

	if record.SessionID == "" {
		out.Proposal = unstatedProposal(ReasonNotInterpreted,
			"this run was launched outside a conversation, so there is no chat context "+
				"for ODE to inject its summary into")
		return out, nil
	}

	messages, err := s.chat.Messages(ctx, record.UserSub, record.SessionID)
	if err != nil {
		return Interpretation{}, err
	}
	injectedAt, answered := deliveryState(messages, record.ID)
	switch {
	case injectedAt < 0:
		out.Proposal = unstatedProposal(ReasonNotInterpreted,
			"the summary has not been put into the conversation yet; an interpretation "+
				"turn runs on the developer's own credential and this developer has not "+
				"been connected since the run finished")
		return out, nil
	case !answered:
		out.Proposal = unstatedProposal(ReasonNotInterpreted,
			"the summary is in the conversation and the assistant has not answered it yet")
		return out, nil
	}

	out.Interpretation = assistantReply(messages, injectedAt)
	at := messages[injectedAt].CreatedAt
	if last := lastAssistantAt(messages, injectedAt); !last.IsZero() {
		at = last
	}
	out.InterpretedAt = &at
	out.Proposal = extractProposal(record.ID, out.Interpretation)

	decisions, err := s.store.ForExperiment(ctx, record.UserSub, record.ID)
	if err != nil {
		return Interpretation{}, err
	}
	if decisions != nil {
		out.Decisions = decisions
	}
	// Merged at read time, never written into the document. A rejected proposal that
	// is interpreted again produces the same fingerprint and so is still rejected —
	// which is the property relations.RuleDecision keys on a rule's fingerprint for,
	// and the reason this one is keyed on the proposal's.
	out.Decision = latestDecision(out.Decisions, out.Proposal.ID)
	return out, nil
}

// summaryTime is when the run's result was known to ODE.
func (s *Service) summaryTime(record experiments.Experiment) time.Time {
	if record.EndedAt != nil && !record.EndedAt.IsZero() {
		return *record.EndedAt
	}
	return record.UpdatedAt
}

// DecisionRequest is a developer answering a proposal.
type DecisionRequest struct {
	// ProposalID is what they were looking at. Required, and checked against the
	// proposal that currently stands: a developer deciding on a proposal that has
	// since been re-interpreted into a different one should be told, not silently
	// recorded as having accepted the new one.
	ProposalID string
	Decision   string
	// Edited is their own form of the adjustment, for an edit.
	Edited string
	Note   string
}

// Decide records the developer's answer (§5.13's last sentence).
//
// The three answers are first-class and all three are recorded, including the
// rejection — a proposal that was rejected must not come back as though nobody had
// been asked. None of them is binding: accepting records agreement and launches
// nothing, and promoting a value into evaluation.yaml is a separate act with no
// tool behind it (D28, §5.8).
func (s *Service) Decide(
	ctx context.Context, req experiments.Request, experimentID string, decision DecisionRequest,
) (Interpretation, error) {
	current, err := s.Interpretation(ctx, req, experimentID)
	if err != nil {
		return Interpretation{}, err
	}
	if !current.Proposal.Stated() {
		return Interpretation{}, fmt.Errorf(
			"%w: there is no proposal on %s to decide on (%s)",
			ErrInvalidRequest, experimentID, current.Proposal.Detail)
	}
	if strings.TrimSpace(decision.ProposalID) == "" {
		return Interpretation{}, fmt.Errorf(
			"%w: proposal_id is required, so a decision names what it decided on",
			ErrInvalidRequest)
	}
	if decision.ProposalID != current.Proposal.ID {
		return Interpretation{}, &StaleProposalError{
			Decided: decision.ProposalID, Current: current.Proposal.ID,
		}
	}

	stored, err := s.store.Append(ctx, ProposalDecision{
		DecisionID:   s.ids.NewID(),
		CreatedAt:    s.now(),
		CreatedBy:    req.UserSub,
		ExperimentID: experimentID,
		RunID:        current.RunID,
		ProposalID:   current.Proposal.ID,
		Decision:     decision.Decision,
		Proposed:     current.Proposal.Text,
		Edited:       strings.TrimSpace(decision.Edited),
		Note:         strings.TrimSpace(decision.Note),
	})
	if err != nil {
		return Interpretation{}, err
	}

	slog.InfoContext(ctx, "a developer decided on a proposed next experiment",
		"experiment", experimentID, "decision", stored.Decision,
		"proposal", stored.ProposalID)

	current.Decisions = append(current.Decisions, stored)
	current.Decision = latestDecision(current.Decisions, current.Proposal.ID)
	return current, nil
}

// lastAssistantAt is when the assistant last spoke after the summary.
func lastAssistantAt(messages []chat.StoredMessage, injectedAt int) time.Time {
	var at time.Time
	for index := injectedAt + 1; index < len(messages); index++ {
		if messages[index].Injected() {
			break
		}
		if messages[index].Role == llm.RoleAssistant && hasText(messages[index]) {
			at = messages[index].CreatedAt
		}
	}
	return at
}

// retire moves a run out of the queue and marks it done for this process.
func (s *Service) retire(experimentID string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	delete(s.pending, experimentID)
	if len(s.interpreted) >= maxInterpretedCache {
		// Cleared wholesale rather than evicted one at a time. It is a cache over a
		// durable fact, so the cost of forgetting is one conversation read per run the
		// poller offers again — and an unbounded map in a process that runs for months
		// is the one growth this service would otherwise have.
		s.interpreted = map[string]bool{}
	}
	s.interpreted[experimentID] = true
}

// maxInterpretedCache bounds that cache. Generous next to the number of runs a
// deployment finishes inside one poll window, which is what it needs to cover.
const maxInterpretedCache = 4096

// drop lets go of a run without claiming it was interpreted.
//
// The difference from retire is the whole point: retire says "this is done", and
// drop says "this process is not holding it any more". A dropped run is offered
// again by the poller while it is still recent, and if it was in fact already
// delivered, deliveryState sees the injected message and retires it properly.
func (s *Service) drop(experimentID string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	delete(s.pending, experimentID)
}

// deliveryState reads the conversation for what has already happened to one run.
//
// The injected message *is* the record that the summary was delivered — there is
// no second table saying so, because the two would diverge and the conversation is
// what the developer actually reads. An assistant turn after it is the record that
// it was interpreted. Both survive a restart, which is what makes the poller
// re-offering a run harmless.
func deliveryState(messages []chat.StoredMessage, experimentID string) (at int, answered bool) {
	at = -1
	for index, message := range messages {
		if message.Injected() {
			if message.Subject == experimentID {
				at, answered = index, false
				continue
			}
			if at >= 0 {
				// A *different* run's summary, after this one's. Everything from here on
				// answers that run, not this one — so the scan stops rather than reading
				// the next run's interpretation as this one's answer. Without this, a run
				// whose turn produced no text would be reported as answered by whatever
				// the assistant said about the run after it.
				return at, answered
			}
			continue
		}
		if at >= 0 && message.Role == llm.RoleAssistant && hasText(message) {
			answered = true
		}
	}
	return at, answered
}

// hasText reports whether an assistant message said anything, as opposed to
// carrying only tool calls. A turn that only dispatched a tool has not interpreted
// anything yet.
func hasText(message chat.StoredMessage) bool {
	for _, block := range message.Content {
		if block.Type == llm.ContentText && strings.TrimSpace(block.Text) != "" {
			return true
		}
	}
	return false
}

// assistantReply is everything the assistant said after the summary was injected,
// joined. Several turns where the loop used tools between them, and the proposal
// is read from all of them.
func assistantReply(messages []chat.StoredMessage, injectedAt int) string {
	parts := []string{}
	for index := injectedAt + 1; index < len(messages); index++ {
		message := messages[index]
		if message.Injected() {
			// A later run's summary. Everything after it belongs to that one.
			break
		}
		if message.Role != llm.RoleAssistant {
			continue
		}
		for _, block := range message.Content {
			if block.Type == llm.ContentText && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
