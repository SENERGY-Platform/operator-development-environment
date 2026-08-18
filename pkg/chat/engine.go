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

package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// EventType is what the SPA receives over SSE. The five from §5.7's normalised
// stream, plus four ODE-specific ones that have no provider equivalent.
type EventType string

const (
	EventTextDelta  EventType = "text_delta"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventDone       EventType = "done"
	EventError      EventType = "error"

	// EventConfirmation asks the developer to decide on a held tool call (D11).
	EventConfirmation EventType = "confirmation_required"
	// EventLimit reports a §3.3 cap breach, carrying the structured refusal.
	EventLimit EventType = "limit_exceeded"
	// EventWarning is a soft limit crossed. The turn continues.
	EventWarning EventType = "warning"
	// EventUsage reports what the exchange cost, once, at the end.
	EventUsage EventType = "usage"
	// EventProgress is a step inside a running tool. Advisory: it carries no result
	// and the model never sees it. It exists so a developer watching a multi-minute
	// profile can tell it apart from a wedged one.
	EventProgress EventType = "progress"
)

// Event is one item of the chat stream.
type Event struct {
	Type EventType `json:"type"`

	Text string `json:"text,omitempty"`

	ToolCall     *llm.ToolCall   `json:"tool_call,omitempty"`
	ToolResult   *tools.Result   `json:"tool_result,omitempty"`
	Confirmation map[string]any  `json:"confirmation,omitempty"`
	Usage        *llm.Usage      `json:"usage,omitempty"`
	Warnings     []admin.Warning `json:"warnings,omitempty"`
	Progress     *tools.Progress `json:"progress,omitempty"`
	Limit        map[string]any  `json:"limit,omitempty"`
	Session      *Session        `json:"session,omitempty"`

	StopReason string `json:"stop_reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// IDs mints session, message and confirmation ids.
type IDs interface{ NewID() string }

type Options struct {
	// MaxIterations bounds the tool loop. A model that keeps calling tools without
	// concluding would otherwise run until the spend cap stopped it, which is a
	// governance control doing a control-flow job.
	MaxIterations int
	// MaxTokens is the default response bound handed to a provider.
	MaxTokens int
	// Effort is the default reasoning effort for providers that have one.
	Effort string
	// MCPEndpoint is ODE's own MCP URL, handed to out-of-band providers.
	MCPEndpoint string
	// TitleWords is how many words of the first message become a session title.
	TitleWords int
	// ExchangeTimeout bounds one detached exchange. It has to exist: an exchange no
	// longer dies with its connection, so without a ceiling a wedged provider or a
	// hung platform read would leave a goroutine and a session lock forever.
	ExchangeTimeout time.Duration
}

const (
	defaultMaxIterations = 12
	defaultTitleWords    = 8
	// defaultExchangeTimeout is generous because a single exchange may run several
	// profiler passes, each of which can take minutes on a real series.
	defaultExchangeTimeout = 30 * time.Minute
)

// Engine runs conversations.
type Engine struct {
	providers  *llm.Registry
	dispatcher *tools.Dispatcher
	store      Store
	limits     *admin.Service
	ids        IDs
	opts       Options
	now        func() time.Time

	// root is the process lifetime. Exchanges descend from it rather than from the
	// request that started them, which is what detaches them from the connection —
	// and it means shutdown still stops them.
	root context.Context

	// live is the running exchange per session. At most one: a second concurrent
	// turn on the same conversation would interleave two assistant replies into one
	// message history.
	exchangeMux sync.Mutex
	live        map[string]*Exchange
}

func New(
	root context.Context,
	providers *llm.Registry,
	dispatcher *tools.Dispatcher,
	store Store,
	limits *admin.Service,
	ids IDs,
	opts Options,
) (*Engine, error) {
	if root == nil {
		return nil, errors.New("chat: a root context is required, because an exchange " +
			"outlives the request that started it")
	}
	if providers == nil || providers.Len() == 0 {
		return nil, errors.New("chat: at least one llm provider is required")
	}
	if dispatcher == nil {
		return nil, errors.New("chat: a tool dispatcher is required")
	}
	if store == nil {
		return nil, errors.New("chat: a store is required")
	}
	if limits == nil {
		return nil, errors.New("chat: an admin service is required, because §3.3 caps are not optional")
	}
	if ids == nil {
		return nil, errors.New("chat: an id source is required")
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = defaultMaxIterations
	}
	if opts.TitleWords <= 0 {
		opts.TitleWords = defaultTitleWords
	}
	if opts.ExchangeTimeout <= 0 {
		opts.ExchangeTimeout = defaultExchangeTimeout
	}
	return &Engine{
		providers: providers, dispatcher: dispatcher, store: store,
		limits: limits, ids: ids, opts: opts,
		now:  func() time.Time { return time.Now().UTC() },
		root: root,
		live: map[string]*Exchange{},
	}, nil
}

func (e *Engine) Providers() *llm.Registry  { return e.providers }
func (e *Engine) Registry() *tools.Registry { return e.dispatcher.Registry() }

// CreateRequest is a new session.
type CreateRequest struct {
	Title    string
	Provider string
	Model    string
	// Tier is the starting tier. Zero means L0, which §3.2 makes the default.
	Tier tools.Tier
}

func (e *Engine) CreateSession(ctx context.Context, sub string, req CreateRequest) (Session, error) {
	provider, err := e.providers.Get(req.Provider)
	if err != nil {
		return Session{}, err
	}
	model, err := llm.ResolveModel(provider, req.Model)
	if err != nil {
		return Session{}, err
	}
	if err := e.limits.CheckProviderModel(ctx, sub, provider.Name(), model); err != nil {
		return Session{}, err
	}
	if err := e.limits.CheckTier(ctx, sub, req.Tier); err != nil {
		return Session{}, err
	}

	count, err := e.store.CountSessions(ctx, sub)
	if err != nil {
		return Session{}, err
	}
	if err := e.limits.CheckSessionCount(ctx, sub, count); err != nil {
		return Session{}, err
	}

	session := Session{
		ID:        e.ids.NewID(),
		UserSub:   sub,
		Title:     req.Title,
		Provider:  provider.Name(),
		Model:     model,
		Tier:      req.Tier,
		CreatedAt: e.now(),
		UpdatedAt: e.now(),
	}
	if err := e.store.CreateSession(ctx, session); err != nil {
		return Session{}, err
	}

	// The starting tier is audited like any other, so the trail is complete rather
	// than beginning at the first change.
	if err := e.store.AppendTierChange(ctx, TierChange{
		SessionID: session.ID, UserSub: sub,
		From: tools.DefaultTier, To: session.Tier, At: e.now(),
	}); err != nil {
		slog.ErrorContext(ctx, "could not record the initial exposure tier", "error", err)
	}

	return session, nil
}

// Session reads one session, checking ownership and clamping the tier by the
// admin ceiling.
func (e *Engine) Session(ctx context.Context, sub, id string) (Session, error) {
	session, found, err := e.store.Session(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrNoSuchSession
	}
	if session.UserSub != sub {
		// Reported as not-found rather than forbidden: whether a session id exists
		// is itself information about another user.
		return Session{}, ErrNoSuchSession
	}
	session.Tier = e.effectiveTier(ctx, sub, session.Tier)
	return session, nil
}

// effectiveTier clamps a session's tier by the admin ceiling of §3.3.
//
// The ceiling has to bind continuously, not only at the moment a developer raises
// the tier. Checking it only on the way up would mean an admin who lowers the
// maximum affects new sessions and leaves existing ones untouched — a developer
// with an L2 conversation open could keep reading values indefinitely after the
// policy changed. So every read of a session's tier goes through this.
//
// It fails closed. If the policy cannot be read, the tier is the default L0:
// exposing less than intended during a database blip is recoverable, exposing
// more is not.
func (e *Engine) effectiveTier(ctx context.Context, sub string, tier tools.Tier) tools.Tier {
	limits, err := e.limits.Effective(ctx, sub)
	if err != nil {
		slog.ErrorContext(ctx, "could not read the limits policy; clamping the exposure tier to L0",
			"user", sub, "error", err)
		return tools.DefaultTier
	}
	if maximum := limits.MaxTierOr(); tier > maximum {
		slog.InfoContext(ctx, "exposure tier clamped by the admin ceiling",
			"user", sub, "stored", tier.String(), "effective", maximum.String())
		return maximum
	}
	return tier
}

func (e *Engine) Sessions(ctx context.Context, sub string, limit int) ([]Session, error) {
	return e.store.Sessions(ctx, sub, limit)
}

func (e *Engine) Messages(ctx context.Context, sub, id string) ([]StoredMessage, error) {
	if _, err := e.Session(ctx, sub, id); err != nil {
		return nil, err
	}
	return e.store.Messages(ctx, id)
}

func (e *Engine) DeleteSession(ctx context.Context, sub, id string) error {
	if _, err := e.Session(ctx, sub, id); err != nil {
		return err
	}
	return e.store.DeleteSession(ctx, id)
}

// SetTier changes a session's exposure tier.
//
// The developer's control from §3.2, bounded by the admin ceiling from §3.3, and
// audited either way. Lowering is always permitted — a developer may always expose
// less — which is why only the raise is checked against the maximum.
func (e *Engine) SetTier(ctx context.Context, sub, id string, tier tools.Tier) (Session, error) {
	if !tier.Valid() {
		return Session{}, fmt.Errorf("%w: %v is not a valid exposure tier", ErrInvalidRequest, tier)
	}

	// Read unclamped here, deliberately. Session() reports the *effective* tier,
	// and comparing against that would make lowering a clamped session look like a
	// no-op — leaving the stored value stale above the ceiling. What is written has
	// to be compared with what is stored.
	stored, found, err := e.store.Session(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if !found || stored.UserSub != sub {
		return Session{}, ErrNoSuchSession
	}
	session := stored

	// Every raise is checked against the ceiling. Lowering never is: a developer may
	// always choose to expose less.
	if tier > session.Tier {
		if err := e.limits.CheckTier(ctx, sub, tier); err != nil {
			return Session{}, err
		}
	}
	if tier == session.Tier {
		session.Tier = e.effectiveTier(ctx, sub, session.Tier)
		return session, nil
	}

	previous := session.Tier
	session.Tier = tier
	session.UpdatedAt = e.now()
	if err := e.store.UpdateSession(ctx, session); err != nil {
		return Session{}, err
	}
	if err := e.store.AppendTierChange(ctx, TierChange{
		SessionID: id, UserSub: sub, From: previous, To: tier, At: e.now(),
	}); err != nil {
		// The change is already applied, so this cannot fail the request — but an
		// unaudited tier change is exactly what §3.2 asks not to happen, so it is
		// logged at error.
		slog.ErrorContext(ctx, "exposure tier changed without an audit record",
			"session", id, "from", previous.String(), "to", tier.String(), "error", err)
	}

	slog.InfoContext(ctx, "exposure tier changed",
		"session", id, "user", sub, "from", previous.String(), "to", tier.String())
	return session, nil
}

func (e *Engine) TierChanges(ctx context.Context, sub, id string) ([]TierChange, error) {
	if _, err := e.Session(ctx, sub, id); err != nil {
		return nil, err
	}
	return e.store.TierChanges(ctx, id)
}

func (e *Engine) PendingConfirmations(ctx context.Context, sub, id string) ([]Confirmation, error) {
	if _, err := e.Session(ctx, sub, id); err != nil {
		return nil, err
	}
	return e.store.PendingConfirmations(ctx, id)
}

// TokenSource yields the bearer token to present to the platform for this
// exchange's tool calls.
//
// A string would be a snapshot, and an exchange is deliberately detached from the
// request that started it (see Exchange): a turn that runs twelve tool iterations
// over several minutes outlives the access token it began with, and every platform
// read after that point fails with a 401 that the model then has to explain. The
// source is read once per tool call instead, so a token the client refreshed
// mid-turn reaches the work already in flight.
//
// It is called on the exchange's goroutine and may be called concurrently with the
// client replacing the token, so an implementation has to be safe for that.
type TokenSource func() string

// bearer reads the current token. A nil source yields none, which the platform
// then refuses — a caller that forgot to pass one should see an upstream 401
// rather than a nil dereference in the middle of a turn.
func (source TokenSource) bearer() string {
	if source == nil {
		return ""
	}
	return source()
}

// StaticToken is the TokenSource for a caller whose token cannot change: an HTTP
// request that ends before the exchange does, or a test.
func StaticToken(token string) TokenSource {
	return func() string { return token }
}

// Send starts one exchange: the developer's message, then as many provider turns as
// the tool loop needs.
//
// It returns as soon as the exchange has started. The work runs detached from ctx —
// see Exchange — so closing the connection stops nothing; subscribe to the returned
// Exchange to watch it, and call Cancel to abandon it.
//
// ctx is still used for the checks made before the exchange starts, so a caller
// that goes away during validation is not charged for a turn.
func (e *Engine) Send(ctx context.Context, token TokenSource, sub, sessionID, text string) (*Exchange, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: an empty message", ErrInvalidRequest)
	}
	session, err := e.Session(ctx, sub, sessionID)
	if err != nil {
		return nil, err
	}
	// One turn at a time per conversation. Two concurrent exchanges would interleave
	// their assistant messages into one history and leave it unreadable by either.
	if existing, running := e.Attach(sessionID); running {
		_ = existing
		return nil, fmt.Errorf("%w: an exchange is already running on this session",
			ErrInvalidRequest)
	}

	// Enforced before dispatch (§3.3). Done before the message is stored so a
	// capped user's conversation is not left with a question nobody answered.
	//
	// A LimitError is returned as-is; the API layer renders its structured payload.
	verdict, err := e.limits.Check(ctx, sub)
	if err != nil {
		return nil, err
	}

	if session.Title == "" {
		session.Title = title(text, e.opts.TitleWords)
		session.UpdatedAt = e.now()
		if err := e.store.UpdateSession(ctx, session); err != nil {
			return nil, err
		}
	}

	if err := e.store.AppendMessages(ctx, sessionID, StoredMessage{
		SessionID: sessionID, Role: llm.RoleUser,
		Content:   []llm.Content{{Type: llm.ContentText, Text: text}},
		CreatedAt: e.now(),
	}); err != nil {
		return nil, err
	}

	exchange := e.begin(sessionID)
	go func() {
		defer e.finish(exchange)
		if len(verdict.Warnings) > 0 {
			exchange.publish(Event{Type: EventWarning, Warnings: verdict.Warnings})
		}
		e.run(exchange.ctx, exchange, token, session)
	}()
	return exchange, nil
}

// begin registers a detached exchange for a session.
func (e *Engine) begin(sessionID string) *Exchange {
	// WithoutCancel so the request's cancellation does not reach the work, plus a
	// ceiling so nothing runs forever. Rooted at the process, so shutdown stops it.
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(e.root), e.opts.ExchangeTimeout)

	exchange := newExchange(sessionID, cancel)
	exchange.ctx = ctx

	e.exchangeMux.Lock()
	e.live[sessionID] = exchange
	e.exchangeMux.Unlock()
	return exchange
}

// finish closes an exchange and deregisters it.
func (e *Engine) finish(exchange *Exchange) {
	e.exchangeMux.Lock()
	if e.live[exchange.SessionID] == exchange {
		delete(e.live, exchange.SessionID)
	}
	e.exchangeMux.Unlock()

	exchange.Cancel()
	exchange.close()
}

// Attach returns the exchange running on a session, if there is one.
//
// This is what makes a reconnect useful: the SPA re-reads the persisted messages and
// then attaches to whatever is still in flight, rather than showing a conversation
// that appears to have stopped mid-turn.
func (e *Engine) Attach(sessionID string) (*Exchange, bool) {
	e.exchangeMux.Lock()
	defer e.exchangeMux.Unlock()
	exchange, running := e.live[sessionID]
	if !running || !exchange.Running() {
		return nil, false
	}
	return exchange, true
}

// CancelExchange abandons the turn running on a session, if any.
func (e *Engine) CancelExchange(ctx context.Context, sub, sessionID string) error {
	if _, err := e.Session(ctx, sub, sessionID); err != nil {
		return err
	}
	exchange, running := e.Attach(sessionID)
	if !running {
		return nil
	}
	exchange.Cancel()
	return nil
}

// Confirm resolves a held tool call and continues the exchange.
//
// Like Send, the continuation runs detached: the developer's decision starts work
// that no longer depends on their connection staying open.
func (e *Engine) Confirm(
	ctx context.Context, token TokenSource, sub, sessionID, confirmationID string, approve bool,
) (*Exchange, error) {
	session, err := e.Session(ctx, sub, sessionID)
	if err != nil {
		return nil, err
	}
	confirmation, found, err := e.store.Confirmation(ctx, confirmationID)
	if err != nil {
		return nil, err
	}
	if !found || confirmation.SessionID != sessionID {
		return nil, ErrNoSuchConfirmation
	}
	if !confirmation.Pending() {
		return nil, ErrAlreadyResolved
	}
	if _, running := e.Attach(sessionID); running {
		return nil, fmt.Errorf("%w: an exchange is already running on this session",
			ErrInvalidRequest)
	}

	exchange := e.begin(sessionID)
	go func() {
		defer e.finish(exchange)
		ctx := exchange.ctx

		resolved := confirmation
		now := e.now()
		resolved.ResolvedAt = &now
		resolved.Decision = DecisionRejected
		if approve {
			resolved.Decision = DecisionApproved
		}

		var result tools.Result
		if approve {
			// Re-dispatched through Confirm, which re-checks the tier against the
			// session's tier *now* rather than the one recorded when the model asked.
			result = e.dispatcher.Confirm(ctx, tools.Request{
				Token: token.bearer(), UserSub: sub, SessionID: sessionID, Tier: session.Tier,
				Report: func(progress tools.Progress) {
					exchange.publish(Event{Type: EventProgress, Progress: &progress})
				},
			}, confirmation.PendingConfirmation)
		} else {
			result = tools.Result{
				CallID: confirmation.CallID, Tool: confirmation.Tool,
				Outcome: tools.Outcome("rejected"), IsError: false,
				Content: map[string]any{
					"rejected": true,
					"hint": "the developer declined this. Do not retry it; ask what they would " +
						"prefer instead.",
				},
			}
		}

		if err := e.store.PutConfirmation(ctx, resolved); err != nil {
			slog.ErrorContext(ctx, "could not record a confirmation decision", "error", err)
		}

		// propose_data_selection is the one confirmed tool whose approval changes
		// session state the assistant then has to see, so the session is re-read.
		if approve && result.Outcome == tools.OutcomeOK {
			if refreshed, err := e.Session(ctx, sub, sessionID); err == nil {
				session = refreshed
			}
		}

		exchange.publish(Event{Type: EventToolResult, ToolResult: &result})

		// The outcome goes back as a user turn, not as a tool result.
		//
		// The held call's tool_use block was already answered when the exchange
		// paused — it had to be, because both native protocols require every
		// tool_use in an assistant turn to be answered before the conversation may
		// continue. A second tool_result for the same id is a protocol error, so the
		// developer's decision is reported as ordinary conversation instead.
		if err := e.store.AppendMessages(ctx, sessionID, confirmationOutcome(result, approve)); err != nil {
			exchange.publish(Event{Type: EventError, Error: err.Error()})
			return
		}

		e.run(ctx, exchange, token, session)
	}()

	return exchange, nil
}

// run is the tool loop.
func (e *Engine) run(ctx context.Context, exchange *Exchange, token TokenSource, session Session) {
	provider, err := e.providers.Get(session.Provider)
	if err != nil {
		exchange.publish(Event{Type: EventError, Error: err.Error()})
		return
	}
	capabilities := provider.Capabilities()

	// total is the exchange's aggregate, reported once at the end for the UI. The
	// individual turns are accounted as they complete — see below.
	total := llm.Usage{}
	defer func() {
		if total.InputTokens == 0 && total.OutputTokens == 0 {
			return
		}
		exchange.publish(Event{Type: EventUsage, Usage: &total})
	}()

	for iteration := 0; iteration < e.opts.MaxIterations; iteration++ {
		// The tier is re-read each iteration rather than captured once, and clamped
		// as it is read. A developer may lower the tier mid-exchange, and an admin may
		// lower the ceiling; the next tool call must respect either.
		stored, found, err := e.store.Session(ctx, session.ID)
		if err == nil && found {
			session.Tier = e.effectiveTier(ctx, session.UserSub, stored.Tier)
			session.Selection = stored.Selection
		}

		messages, err := e.store.Messages(ctx, session.ID)
		if err != nil {
			exchange.publish(Event{Type: EventError, Error: err.Error()})
			return
		}

		offered := []tools.Definition{}
		if capabilities.Tools {
			offered = e.dispatcher.Registry().Available(session.Tier)
		}

		request := llm.Request{
			Model:     session.Model,
			System:    systemPrompt(e.dispatcher.Registry(), session, capabilities.Tools),
			Messages:  conversation(messages),
			Tools:     toolDefinitions(offered),
			MaxTokens: e.opts.MaxTokens,
			Effort:    e.opts.Effort,
		}
		if capabilities.ToolsOutOfBand && e.opts.MCPEndpoint != "" {
			request.ToolEndpoint = &llm.ToolEndpoint{
				URL: e.opts.MCPEndpoint,
				// Read here rather than at the top of the turn: the provider calls
				// ODE's MCP endpoint with this, and a turn can outlive the token it
				// started with.
				Token:        token.bearer(),
				SessionID:    session.ID,
				AllowedTools: names(offered),
			}
		}

		// Checked before every provider call, not only the first: a tool loop makes
		// several, and a cap that only bound the first would be trivially exceeded.
		if iteration > 0 {
			if _, err := e.limits.Check(ctx, session.UserSub); err != nil {
				var limitErr *admin.LimitError
				if errors.As(err, &limitErr) {
					exchange.publish(Event{
						Type: EventLimit, Limit: limitErr.Payload(), Error: limitErr.Error(),
					})
					return
				}
				exchange.publish(Event{Type: EventError, Error: err.Error()})
				return
			}
		}

		stream, err := provider.Stream(ctx, request)
		if err != nil {
			exchange.publish(Event{Type: EventError, Error: err.Error()})
			return
		}

		turn, ok := e.consume(ctx, exchange, stream)
		total.Add(turn.usage)
		// Accounted per provider request, which is what §3.3 asks for — and what
		// makes the cap check at the top of the next iteration able to see this
		// exchange's own spend. Recording only the aggregate at the end would let a
		// single tool loop overrun the cap by its whole length, bounded by nothing
		// but MaxIterations.
		//
		// Detached from ctx so a cancelled turn still leaves its record: the tokens
		// were spent whether or not the developer waited for the answer.
		if turn.usage.InputTokens > 0 || turn.usage.OutputTokens > 0 {
			recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			e.limits.RecordUsage(recordCtx, session.UserSub, session.ID, turn.usage)
			cancel()
		}
		if !ok {
			return
		}

		// An out-of-band provider ran its own loop over MCP, so there is nothing
		// here to dispatch and the exchange is over. Its text and tool activity have
		// already been forwarded.
		if capabilities.ToolsOutOfBand {
			if err := e.persistAssistant(ctx, session.ID, turn); err != nil {
				exchange.publish(Event{Type: EventError, Error: err.Error()})
			}
			exchange.publish(Event{Type: EventDone, StopReason: turn.stopReason})
			return
		}

		if err := e.persistAssistant(ctx, session.ID, turn); err != nil {
			exchange.publish(Event{Type: EventError, Error: err.Error()})
			return
		}

		if len(turn.calls) == 0 {
			exchange.publish(Event{Type: EventDone, StopReason: turn.stopReason})
			return
		}

		results, held := e.dispatch(ctx, exchange, token, session, turn.calls)

		if len(results) > 0 {
			if err := e.store.AppendMessages(ctx, session.ID, toolResultMessage(results)); err != nil {
				exchange.publish(Event{Type: EventError, Error: err.Error()})
				return
			}
		}

		// A held confirmation ends the exchange. The loop resumes from Confirm when
		// the developer decides, which is the whole point of D11: nothing proceeds
		// on the model's word.
		if held {
			exchange.publish(Event{Type: EventDone, StopReason: "awaiting_confirmation"})
			return
		}
	}

	exchange.publish(Event{
		Type:       EventDone,
		StopReason: "max_iterations",
		Error: fmt.Sprintf(
			"the assistant used tools %d times without concluding, and the exchange was stopped",
			e.opts.MaxIterations),
	})
}

// turnResult is what one provider call produced.
type turnResult struct {
	text       string
	calls      []llm.ToolCall
	usage      llm.Usage
	stopReason string
}

// consume forwards a provider's stream and collects the turn. It reports false
// when the exchange should stop — an error, or the caller going away.
func (e *Engine) consume(ctx context.Context, exchange *Exchange, stream <-chan llm.Event) (turnResult, bool) {
	turn := turnResult{}
	for event := range stream {
		switch event.Type {
		case llm.EventTextDelta:
			turn.text += event.Text
			exchange.publish(Event{Type: EventTextDelta, Text: event.Text})
			if ctx.Err() != nil {
				return turn, false
			}
		case llm.EventToolCall:
			if event.ToolCall == nil {
				continue
			}
			turn.calls = append(turn.calls, *event.ToolCall)
			exchange.publish(Event{Type: EventToolCall, ToolCall: event.ToolCall})
			if ctx.Err() != nil {
				return turn, false
			}
		case llm.EventToolResult:
			// Only an out-of-band provider produces these: the CLI reporting what it
			// already ran over MCP.
			if event.ToolResult == nil {
				continue
			}
			exchange.publish(Event{Type: EventToolResult, ToolResult: &tools.Result{
				CallID:  event.ToolResult.CallID,
				Tool:    event.ToolResult.Name,
				Outcome: tools.OutcomeOK,
				Content: event.ToolResult.Content,
				IsError: event.ToolResult.IsError,
			}})
			if ctx.Err() != nil {
				return turn, false
			}
		case llm.EventDone:
			turn.stopReason = event.StopReason
			if event.Usage != nil {
				turn.usage = *event.Usage
			}
		case llm.EventError:
			exchange.publish(Event{Type: EventError, Error: event.Error})
			return turn, false
		}
	}
	return turn, true
}

// dispatch runs the turn's tool calls and reports whether any is held for
// confirmation.
func (e *Engine) dispatch(
	ctx context.Context, exchange *Exchange, token TokenSource, session Session, calls []llm.ToolCall,
) (results []tools.Result, held bool) {
	for _, call := range calls {
		// Every call goes through the one Dispatcher, which is where the tier gate
		// lives. Nothing in this loop decides what a tool may do.
		result := e.dispatcher.Dispatch(ctx, tools.Request{
			// Read per call, not per turn: this is the point of TokenSource.
			Token:     token.bearer(),
			UserSub:   session.UserSub,
			SessionID: session.ID,
			Tier:      session.Tier,
			// Published as it happens, so a long tool is visibly working. publish never
			// blocks, which is what makes this safe to call from inside a platform read.
			Report: func(progress tools.Progress) {
				exchange.publish(Event{Type: EventProgress, Progress: &progress})
			},
		}, tools.Call{ID: call.ID, Name: call.Name, Input: call.Input})

		if result.Outcome == tools.OutcomeAwaitingConfirmation && result.Confirmation != nil {
			confirmation := Confirmation{
				PendingConfirmation: *result.Confirmation,
				UserSub:             session.UserSub,
			}
			if err := e.store.PutConfirmation(ctx, confirmation); err != nil {
				exchange.publish(Event{Type: EventError, Error: err.Error()})
				continue
			}
			exchange.publish(Event{Type: EventConfirmation, Confirmation: confirmation.Describe()})
			held = true
			// Kept in results on purpose. The tool did not run, but its tool_use block
			// still needs an answer before the conversation can continue, and the
			// dispatcher's content says exactly what happened: confirmation required.
			// The real outcome arrives later as a user turn — see Confirm.
			results = append(results, result)
			continue
		}

		exchange.publish(Event{Type: EventToolResult, ToolResult: &result})
		results = append(results, result)
	}
	return results, held
}

func (e *Engine) persistAssistant(ctx context.Context, sessionID string, turn turnResult) error {
	content := []llm.Content{}
	if turn.text != "" {
		content = append(content, llm.Content{Type: llm.ContentText, Text: turn.text})
	}
	for _, call := range turn.calls {
		content = append(content, llm.Content{
			Type:      llm.ContentToolUse,
			ToolUseID: call.ID,
			ToolName:  call.Name,
			ToolInput: call.Input,
		})
	}
	if len(content) == 0 {
		return nil
	}
	return e.store.AppendMessages(ctx, sessionID, StoredMessage{
		SessionID: sessionID, Role: llm.RoleAssistant, Content: content, CreatedAt: e.now(),
	})
}

// confirmationOutcome reports a developer's decision as a user turn.
func confirmationOutcome(result tools.Result, approved bool) StoredMessage {
	builder := &strings.Builder{}
	if !approved {
		fmt.Fprintf(builder,
			"The developer declined the %s call. Do not retry it; ask what they would prefer.",
			result.Tool)
		return StoredMessage{
			Role:    llm.RoleUser,
			Content: []llm.Content{{Type: llm.ContentText, Text: builder.String()}},
		}
	}

	encoded, err := json.Marshal(result.Content)
	if err != nil {
		encoded = []byte(`{"error":"the result could not be encoded"}`)
	}
	if result.Outcome == tools.OutcomeOK {
		fmt.Fprintf(builder, "The developer approved the %s call. It ran and returned:\n%s",
			result.Tool, encoded)
	} else {
		fmt.Fprintf(builder,
			"The developer approved the %s call, but it did not run (%s):\n%s",
			result.Tool, result.Outcome, encoded)
	}
	return StoredMessage{
		Role:    llm.RoleUser,
		Content: []llm.Content{{Type: llm.ContentText, Text: builder.String()}},
	}
}

// toolResultMessage packs results into the user-role message both protocols
// expect for tool output.
func toolResultMessage(results []tools.Result) StoredMessage {
	content := make([]llm.Content, 0, len(results))
	for _, result := range results {
		encoded, err := json.Marshal(result.Content)
		if err != nil {
			encoded = []byte(`{"error":"the tool result could not be encoded"}`)
		}
		content = append(content, llm.Content{
			Type:       llm.ContentToolResult,
			ToolUseID:  result.CallID,
			ToolName:   result.Tool,
			ToolResult: string(encoded),
			IsError:    result.IsError,
		})
	}
	return StoredMessage{Role: llm.RoleUser, Content: content}
}

func conversation(messages []StoredMessage) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Message())
	}
	return out
}

func toolDefinitions(definitions []tools.Definition) []llm.ToolDefinition {
	out := make([]llm.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, llm.ToolDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			Schema:      definition.Schema,
		})
	}
	return out
}

func names(definitions []tools.Definition) []string {
	out := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, definition.Name)
	}
	return out
}

// title takes the first few words of the opening message.
func title(text string, words int) string {
	fields := strings.Fields(strings.ReplaceAll(text, "\n", " "))
	if len(fields) == 0 {
		return "New session"
	}
	if len(fields) > words {
		return strings.Join(fields[:words], " ") + "…"
	}
	return strings.Join(fields, " ")
}

// TierFor implements mcp.Sessions: the exposure tier of one session, checked
// against its owner.
//
// Two properties come from going through Session rather than the store: the
// ownership check, without which a developer could name someone else's session and
// inherit its tier, and the admin clamp, so the MCP transport cannot be used to
// reach a tier the ceiling has since forbidden.
func (e *Engine) TierFor(ctx context.Context, userSub, sessionID string) (tools.Tier, error) {
	session, err := e.Session(ctx, userSub, sessionID)
	if err != nil {
		return tools.DefaultTier, err
	}
	return session.Tier, nil
}

// PutProposedSelection implements tools.SelectionSink, so the confirmed
// propose_data_selection tool can write to the session it belongs to.
func (e *Engine) PutProposedSelection(
	ctx context.Context, sessionID string, proposal tools.ProposedSelection,
) error {
	session, found, err := e.store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNoSuchSession
	}
	session.Selection = &proposal
	session.UpdatedAt = e.now()
	return e.store.UpdateSession(ctx, session)
}
