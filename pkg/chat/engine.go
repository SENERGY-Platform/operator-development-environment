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
// stream, plus the ODE-specific ones below, which have no provider equivalent.
type EventType string

const (
	EventTextDelta  EventType = "text_delta"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventDone       EventType = "done"
	EventError      EventType = "error"

	// EventConfirmation asks the developer to decide on a held tool call (D11).
	EventConfirmation EventType = "confirmation_required"
	// EventConfirmationResolved says a held call has been decided, so a view
	// drawing its card can take it down.
	//
	// It exists because a confirmation has more than one audience. A developer with
	// the same conversation open in two windows must see the card in both — that is
	// what a per-exchange event stream gives — and must see it go from both when
	// either one answers, which nothing used to say. The other reader is a window
	// reattaching to a still-running exchange: the replay hands it every card the
	// exchange ever asked for, and without the resolutions among them a reload
	// during a held turn drew a column of cards, most of them already answered.
	EventConfirmationResolved EventType = "confirmation_resolved"
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

// The stop reasons ODE produces itself, as opposed to the ones a provider
// reports. Named because the SPA and any other API consumer read them.
const (
	// StopAwaitingConfirmation is a turn paused on a held tool call (D11). The
	// exchange resumes from Confirm.
	StopAwaitingConfirmation = "awaiting_confirmation"
	// StopConfirmationUnavailable is a turn stopped because a call needed the
	// developer's decision and the request for it could not be recorded, so they
	// will never be asked. Distinct from the above, which a caller may wait on.
	StopConfirmationUnavailable = "confirmation_unavailable"
	// StopMaxIterations is the tool-loop bound.
	StopMaxIterations = "max_iterations"
)

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
	// ConfirmationTimeout bounds how long a tool call held open on an out-of-band
	// transport waits for the developer's decision (see hold.go). It is not the
	// same ceiling as ExchangeTimeout and must be well under the provider's own
	// turn timeout, or the turn dies underneath the card the developer is reading.
	ConfirmationTimeout time.Duration
}

const (
	defaultMaxIterations = 12
	defaultTitleWords    = 8
	// maxTitleRunes bounds a title the developer sets themselves. The column is
	// TEXT and takes anything; a session list is what has a width. Generous enough
	// that no sentence a developer would name a conversation with reaches it, and
	// small enough that a script cannot put a megabyte in a row of the panel.
	maxTitleRunes = 200
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

	// watchers is who is being told what these conversations are doing, keyed by
	// subscription rather than by developer: one developer may have the panel open
	// in two tabs. See activity.go for why the signal is per developer.
	activityMux sync.Mutex
	watchers    map[int]*watcher
	nextWatcher int

	// holds is the confirmed tool calls a transport is keeping open right now,
	// keyed by confirmation id — see hold.go. Not one per session: an out-of-band
	// provider runs its own loop and may have several calls in flight at once.
	//
	// In memory on purpose. It records that something is waiting for an answer,
	// which nothing outlives a restart to keep waiting for.
	holdMux sync.Mutex
	holds   map[string]*hold
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
	if opts.ConfirmationTimeout <= 0 {
		opts.ConfirmationTimeout = defaultConfirmationTimeout
	}
	return &Engine{
		providers: providers, dispatcher: dispatcher, store: store,
		limits: limits, ids: ids, opts: opts,
		now:      func() time.Time { return time.Now().UTC() },
		root:     root,
		live:     map[string]*Exchange{},
		holds:    map[string]*hold{},
		watchers: map[int]*watcher{},
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
	// WorkbenchID is the working context the conversation acts in. Empty means the
	// developer's only one — which is what a client that has not been taught about
	// workbenches sends, and the right answer for a developer who has one.
	WorkbenchID string
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
		ID:          e.ids.NewID(),
		UserSub:     sub,
		Title:       req.Title,
		Provider:    provider.Name(),
		Model:       model,
		Tier:        req.Tier,
		WorkbenchID: strings.TrimSpace(req.WorkbenchID),
		CreatedAt:   e.now(),
		UpdatedAt:   e.now(),
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

// RenameSession sets the developer's own name for a conversation.
//
// Until they do, the title is ODE's guess: the first few words of the opening
// message, which labels a question well and a conversation that has since moved on
// badly. An empty title clears the name and hands the guess back — the next message
// titles the session again, because start only derives one when there is nothing
// there. That is deliberate: clearing is how a developer says "call it whatever it
// turns out to be about", and the alternative — an empty row in the list until they
// think of something — helps nobody.
//
// Unlike SetTier there is no audit record and no ceiling. A title is a label in the
// developer's own list; it decides nothing about what the assistant may see.
func (e *Engine) RenameSession(ctx context.Context, sub, id, name string) (Session, error) {
	trimmed := strings.TrimSpace(name)
	if len([]rune(trimmed)) > maxTitleRunes {
		return Session{}, fmt.Errorf("%w: a title of %d characters is longer than the %d a session keeps",
			ErrInvalidRequest, len([]rune(trimmed)), maxTitleRunes)
	}

	// From the store rather than through Session, for the reason SetTier reads
	// unclamped: Session reports the *effective* tier, and writing that back would
	// persist the admin clamp — silently lowering the developer's stored tier
	// because they renamed a conversation. Only the title is theirs to change here.
	stored, found, err := e.store.Session(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if !found || stored.UserSub != sub {
		// Not-found rather than forbidden, as everywhere else: whether a session id
		// exists is itself information about another user.
		return Session{}, ErrNoSuchSession
	}

	if stored.Title != trimmed {
		stored.Title = trimmed
		stored.UpdatedAt = e.now()
		if err := e.store.UpdateSession(ctx, stored); err != nil {
			return Session{}, err
		}
	}

	// Answered the way every other read of a session answers, so the SPA can store
	// this in place of the one it listed.
	stored.Tier = e.effectiveTier(ctx, sub, stored.Tier)
	return stored, nil
}

// MoveSession changes the workbench a conversation acts in.
//
// The working context is chosen when a session is created, and choosing it wrong is
// ordinary: a developer opens a conversation, works out what it is actually about,
// and finds it is about the other operator. Before this, that conversation was
// stuck there — write_file landed in the checkout it named at creation and nothing
// could re-point it — so the way out was a new session, which threw away the
// history that had got them that far.
//
// An empty id clears the assignment rather than being refused. That is a state
// sessions genuinely have: one written before workbenches existed, and one whose
// workbench has since been closed. Both read as "my only workbench" everywhere
// else, so clearing is how a developer says "wherever I am working".
//
// Refused while an exchange is running. A turn reads the session once, at its
// start, and carries that workbench through every tool call it makes — so a move
// underneath it would have one turn writing into two checkouts while reasoning
// about the first. A turn is seconds, and the refusal says to wait for it.
//
// One narrow window stays open: start() reads the session before it registers its
// exchange, and writes it back when it titles an untitled one, so a move landing
// between those two costs the developer a second move on the very first message of
// a fresh conversation. Closing it properly needs a session-level write lock the
// rest of the engine does not have, which is a larger change than this.
//
// Like a rename and unlike a tier change, there is no audit trail. Which checkout a
// conversation acts in decides nothing about what the assistant may see, and §3.2's
// trail is about exposure.
//
// Whether the workbench exists and is this developer's is checked before this is
// reached, by the route: workbenches live in pkg/repo, and a chat engine that had to
// know about them would couple the conversation surface to the repository surface
// for one lookup. label is that workbench's own name and is used only in the note
// the move leaves behind.
func (e *Engine) MoveSession(
	ctx context.Context, sub, id, workbenchID, label string,
) (Session, error) {
	target := strings.TrimSpace(workbenchID)

	// From the store rather than through Session, for the reason RenameSession reads
	// unclamped: Session reports the *effective* tier, and writing that back would
	// persist the admin clamp — silently lowering the developer's stored tier because
	// they moved a conversation to another operator.
	stored, found, err := e.store.Session(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if !found || stored.UserSub != sub {
		// Not-found rather than forbidden, as everywhere else: whether a session id
		// exists is itself information about another user.
		return Session{}, ErrNoSuchSession
	}

	if stored.WorkbenchID == target {
		// Nothing moved, so nothing is written and nothing is said in the conversation:
		// a note about a move that did not happen would be a false entry in the
		// history, and this is the shape a double-click sends.
		stored.Tier = e.effectiveTier(ctx, sub, stored.Tier)
		return stored, nil
	}

	if _, running := e.Attach(id); running {
		return Session{}, fmt.Errorf("%w: an exchange is already running on this session, "+
			"and it is acting in the workbench this would move away from", ErrInvalidRequest)
	}

	previous := stored.WorkbenchID
	stored.WorkbenchID = target
	stored.UpdatedAt = e.now()
	if err := e.store.UpdateSession(ctx, stored); err != nil {
		return Session{}, err
	}

	// The note goes *into* the conversation rather than beside it, because the history
	// is what the next turn reads. Everything above it — every file read, every path
	// written, every cell run — happened in another checkout, and a model handed that
	// history with no marker goes on believing the files it wrote are still there.
	//
	// Appended rather than sent through SendInjected: a move is not a question, and
	// starting a turn to have it answered would spend the developer's §3.3 budget on
	// the word "understood". The next turn reads it as its first input instead.
	notice := moveNotice(target, label)
	notice.SessionID = id
	if err := e.store.AppendMessages(ctx, id, notice); err != nil {
		// The move is applied and is what the developer asked for, so this does not
		// fail the request. At error rather than warn: the conversation now has a code
		// context its own history contradicts, and nothing downstream repairs that.
		slog.ErrorContext(ctx, "a session changed workbench without a note in its history",
			"session", id, "from", previous, "to", target, "error", err)
	}

	slog.InfoContext(ctx, "session moved to another workbench",
		"session", id, "user", sub, "from", previous, "to", target)

	// Answered the way every other read of a session answers, so the SPA can store
	// this in place of the one it listed.
	stored.Tier = e.effectiveTier(ctx, sub, stored.Tier)
	return stored, nil
}

// moveSubjectPrefix marks a move note's subject.
//
// Prefixed rather than the bare workbench id, because a bare id in this field is
// read as an experiment's elsewhere — pkg/interpret finds the summary belonging to
// one run by comparing Subject with an experiment id — and both ids are minted from
// the same space.
const moveSubjectPrefix = "workbench:"

// moveNotice is what a move leaves in the conversation.
//
// Prose rather than the JSON block §5.13's summary carries: there is no data in it,
// only a fact about the history above it, and the developer reads the same words the
// model does.
func moveNotice(workbenchID, label string) StoredMessage {
	named := strings.TrimSpace(label)
	if named == "" {
		named = workbenchID
	}
	text := "ODE moved this conversation to another code workspace: " + named + "."
	if workbenchID == "" {
		text = "ODE cleared this conversation's code workspace, so it now acts in " +
			"whichever single workbench the developer has open."
	}
	text += " Every file read, file write and cell run earlier in this conversation" +
		" happened in the previous one. Nothing above describes the checkout you are in" +
		" now, so re-read whatever you are about to rely on rather than assuming a path" +
		" or a result is still there."

	return StoredMessage{
		Role:    llm.RoleUser,
		Content: []llm.Content{{Type: llm.ContentText, Text: text}},
		Origin:  OriginODE,
		Subject: moveSubjectPrefix + workbenchID,
	}
}

/*
SetAutoRun sets a session's standing answer to a recognised `run_code`.

Deliberately thinner than SetTier, and the difference says what the setting is. A
tier bounds what the assistant may *see*, so raising one is checked against the
admin ceiling and written to an audit trail. This changes only who is asked before
something runs, inside a session that could already run that code the moment the
developer clicked approve — so there is no ceiling to check and no separate log to
keep.

What it is not is a widening of authority, and that is why no tool can call it:
`set_auto_run` is in the denied set. The developer turns it on; the model lives
with the answer.
*/
func (e *Engine) SetAutoRun(ctx context.Context, sub, id string, on bool) (Session, error) {
	session, err := e.Session(ctx, sub, id)
	if err != nil {
		return Session{}, err
	}
	if session.AutoRun == on {
		return session, nil
	}
	session.AutoRun = on
	if err := e.store.UpdateSession(ctx, session); err != nil {
		return Session{}, err
	}
	// At info, because "it ran without being asked" is answered by two things: the
	// per-call line in Dispatch, and this — when the standing answer was given.
	slog.InfoContext(ctx, "auto mode changed for a session",
		"session", id, "user", sub, "auto_run", on)
	return session, nil
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
	pending, err := e.store.PendingConfirmations(ctx, id)
	if err != nil {
		return nil, err
	}
	// Marked from the live registry rather than read from the row, because being
	// held is a fact about now — see Confirmation.OutOfBand. This is what lets a
	// developer who reloaded the page mid-turn still answer a held call in place.
	for i := range pending {
		pending[i].OutOfBand = e.heldOutOfBand(pending[i].ID)
	}
	return pending, nil
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
func (e *Engine) Send(
	ctx context.Context, token TokenSource, sub, sessionID, text string,
) (*Exchange, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: an empty message", ErrInvalidRequest)
	}
	return e.start(ctx, token, sub, sessionID, StoredMessage{
		SessionID: sessionID, Role: llm.RoleUser,
		Content: []llm.Content{{Type: llm.ContentText, Text: text}},
	}, text)
}

// SendInjected starts a turn from a message ODE composed rather than the developer
// (§5.13, M9).
//
// It is the same turn in every respect that matters, and that is the design rather
// than an economy. An automated turn dispatches tools, spends tokens against the
// developer's §3.3 cap and reads the platform on their behalf, so it goes through
// the same guards as a typed one: the session's ownership check, the one-exchange-
// at-a-time rule, limits.Check before anything is stored, and the session's own
// exposure tier re-read on every iteration of the loop. There is deliberately no
// second path into run() that skips any of them.
//
// Three things differ, and each is visible in the arguments:
//
//   - The stored message is marked OriginODE with a subject, so the SPA's replay
//     and any later reader can tell it from something the developer typed. A block
//     of JSON rendered in the developer's own voice would be a lie about who said it.
//   - It never titles the session. A conversation named after a machine-generated
//     summary would lose the developer's own opening line from every listing.
//   - The token is still a developer's. It has to be: a background poller has no
//     credential and §3.1 item 3 does not let it acquire one, so the caller only
//     reaches here when a live token exists.
func (e *Engine) SendInjected(
	ctx context.Context, token TokenSource, sub, sessionID string, message InjectedMessage,
) (*Exchange, error) {
	if strings.TrimSpace(message.Text) == "" {
		return nil, fmt.Errorf("%w: an empty injected message", ErrInvalidRequest)
	}
	return e.start(ctx, token, sub, sessionID, StoredMessage{
		SessionID: sessionID, Role: llm.RoleUser,
		Content: []llm.Content{{Type: llm.ContentText, Text: message.Text}},
		Origin:  OriginODE,
		Subject: message.Subject,
	}, "")
}

// Continue starts a turn on a session without adding anything to it.
//
// One caller, one reason: ODE injected §5.13's summary and then stopped — a
// restart, a crash — before the assistant answered it. Re-injecting would put a
// second copy of the same summary in the conversation, which is ODE talking over
// itself; leaving it would be a summary nobody ever read. So the turn is run over
// the history as it stands.
//
// It goes through the same guards as any other turn, which is the point of it
// sharing start: the cap is checked, the tier is re-read, and a session already
// running an exchange refuses it.
func (e *Engine) Continue(
	ctx context.Context, token TokenSource, sub, sessionID string,
) (*Exchange, error) {
	return e.start(ctx, token, sub, sessionID, StoredMessage{}, "")
}

// InjectedMessage is what ODE puts into a conversation on the developer's behalf.
type InjectedMessage struct {
	// Text is what the model reads. It is stored, so it is also what the developer
	// sees — there is no hidden half of a conversation.
	Text string
	// Subject names what it is about, which for §5.13 is the experiment id. It is
	// what makes the delivery idempotent: the stored message is itself the record
	// that this run was already injected, so a poller offering the same run again
	// finds it rather than injecting a second copy.
	Subject string
}

// start is the body Send and SendInjected share: every check, in the one order.
//
// titleFrom is the text a session with no title takes one from, and is empty for
// an injected message.
func (e *Engine) start(
	ctx context.Context, token TokenSource, sub, sessionID string,
	message StoredMessage, titleFrom string,
) (*Exchange, error) {
	session, err := e.Session(ctx, sub, sessionID)
	if err != nil {
		return nil, err
	}
	// One turn at a time per conversation. Two concurrent exchanges would interleave
	// their assistant messages into one history and leave it unreadable by either.
	//
	// Checked before the message is stored, which is what makes a refused automated
	// turn harmless: nothing is appended, so the conversation is not left with a
	// summary wedged between an assistant's tool call and its result — a shape both
	// native protocols reject outright. The caller retries later.
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

	if session.Title == "" && titleFrom != "" {
		session.Title = title(titleFrom, e.opts.TitleWords)
		session.UpdatedAt = e.now()
		if err := e.store.UpdateSession(ctx, session); err != nil {
			return nil, err
		}
	}

	// A message with no content is Continue's: the turn runs over the history as it
	// already stands, and appending an empty message would put a blank turn in the
	// conversation that both native protocols would then have to be given a role for.
	if len(message.Content) > 0 {
		message.SessionID = sessionID
		if message.CreatedAt.IsZero() {
			message.CreatedAt = e.now()
		}
		if err := e.store.AppendMessages(ctx, sessionID, message); err != nil {
			return nil, err
		}
	}

	exchange := e.begin(sub, sessionID)
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
//
// The owner is carried on the exchange rather than looked up later, because the
// two readers of it — the activity watchers, and anything asking what this
// developer has running — run after the request that knew who asked has gone.
func (e *Engine) begin(sub, sessionID string) *Exchange {
	// WithoutCancel so the request's cancellation does not reach the work, plus a
	// ceiling so nothing runs forever. Rooted at the process, so shutdown stops it.
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(e.root), e.opts.ExchangeTimeout)

	exchange := newExchange(sessionID, cancel)
	exchange.ctx = ctx
	exchange.UserSub = sub

	e.exchangeMux.Lock()
	e.live[sessionID] = exchange
	e.exchangeMux.Unlock()

	e.publishActivity(sub, sessionID, ActivityRunning)
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

	// After close, not before: the panel's mark means "there is something here for
	// you", and the last events of the turn are what put it there.
	e.publishActivity(exchange.UserSub, exchange.SessionID, e.endedState(exchange.SessionID))
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
	// A held call is answered where it waits. Running it from here would dispatch
	// the tool a second time and then start a turn beside the one still streaming.
	if e.heldOutOfBand(confirmationID) {
		return nil, ErrHeldOutOfBand
	}
	if _, running := e.Attach(sessionID); running {
		return nil, fmt.Errorf("%w: an exchange is already running on this session",
			ErrInvalidRequest)
	}

	exchange := e.begin(sub, sessionID)
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
				AutoRun:     session.AutoRun,
				WorkbenchID: session.WorkbenchID,
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
		// Published even though this exchange is a new one that no other window is
		// watching yet: publish appends to the exchange's history, so a window that
		// reattaches — which is what the second window does when the panel tells it
		// this session started running again — reads the decision with the rest.
		publishResolution(exchange, resolved)

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
				// Longer than the hold, so the wait ends here rather than at the
				// provider: ODE knows why a confirmation went unanswered and can say so,
				// whereas a client-side timeout is an opaque failed tool call.
				CallTimeout: e.opts.ConfirmationTimeout + confirmationCallMargin,
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
		} else if turn.stopReason == llm.StopReasonCancelled || turn.stopReason == llm.StopReasonError {
			// The turn was billed and could not say by how much. Not every protocol
			// reports usage as it streams — the chat-completions one sends it only in
			// the final chunk — so a turn stopped before the end has nothing to
			// account. §3.3's cap then under-counts, and that is worth seeing rather
			// than inferring from a gap in the records.
			slog.WarnContext(ctx, "a turn ended early without reporting its usage; "+
				"the tokens it spent are not accounted",
				"session", session.ID, "provider", session.Provider,
				"model", session.Model, "stop_reason", turn.stopReason)
		}
		if !ok {
			// Abandoned — the provider reported an error, or the exchange's own
			// context ended under it — and what it had already produced is stored
			// anyway.
			//
			// It has to be. The developer watched this arrive, and the SPA replaces
			// the streamed view with the stored history the moment the turn ends: a
			// turn that stores nothing is therefore not merely unrecorded but wiped
			// off the screen, with the alert announcing a reply that is not there.
			// The CLI's own ten-minute turn timeout reaches exactly this line after a
			// long turn, and the text and the tool activity of those ten minutes went
			// with it.
			//
			// The same argument the usage record above already makes: it happened,
			// whether or not the turn got to finish.
			e.persistAbandoned(ctx, session.ID, turn)
			return
		}

		// An out-of-band provider ran its own loop over MCP, so there is nothing
		// here to dispatch and the exchange is over. Its text and tool activity have
		// already been forwarded.
		if capabilities.ToolsOutOfBand {
			if err := e.persistAssistant(ctx, session.ID, turn); err != nil {
				exchange.publish(Event{Type: EventError, Error: err.Error()})
			}
			// The results are stored next to the calls even though nothing here
			// dispatched them. persistAssistant has just written the provider's
			// tool_use blocks, and a tool_use that nothing answers is a conversation
			// both native protocols refuse — so leaving them out would strand the
			// session the moment it was moved to another provider, and would have
			// conversation() report a result ODE is in fact holding as lost.
			if len(turn.results) > 0 {
				if err := e.appendToolResults(ctx, session.ID, outOfBandResultMessage(turn.results)); err != nil {
					exchange.publish(Event{Type: EventError, Error: err.Error()})
				}
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

		results, stop := e.dispatch(ctx, exchange, token, session, turn.calls)

		if len(results) > 0 {
			if err := e.appendToolResults(ctx, session.ID, toolResultMessage(results)); err != nil {
				exchange.publish(Event{Type: EventError, Error: err.Error()})
				return
			}
		}

		// A held confirmation ends the exchange. The loop resumes from Confirm when
		// the developer decides, which is the whole point of D11: nothing proceeds
		// on the model's word.
		if stop != "" {
			exchange.publish(Event{Type: EventDone, StopReason: stop})
			return
		}
	}

	exchange.publish(Event{
		Type:       EventDone,
		StopReason: StopMaxIterations,
		Error: fmt.Sprintf(
			"the assistant used tools %d times without concluding, and the exchange was stopped",
			e.opts.MaxIterations),
	})
}

// turnResult is what one provider call produced.
type turnResult struct {
	text  string
	calls []llm.ToolCall
	// results is what an out-of-band provider reported for the calls it ran itself.
	// Empty for every other provider, whose results come from the dispatcher.
	results    []llm.ToolResult
	usage      llm.Usage
	stopReason string
}

// consume forwards a provider's stream and collects the turn. It reports false
// when the exchange should stop — an error, or the caller going away.
func (e *Engine) consume(ctx context.Context, exchange *Exchange, stream <-chan llm.Event) (turnResult, bool) {
	turn := turnResult{}

	// abandoned marks a turn that will not be continued — the developer stopped it,
	// or the provider reported an error.
	//
	// The loop keeps reading to the end of the stream either way rather than
	// returning at once. A provider reports what a turn cost in its closing done
	// event, which is therefore the last thing to arrive; returning at the first
	// event seen after a cancellation dropped it, turn.usage stayed zero and
	// RecordUsage was skipped entirely. §3.3's caps are computed from recorded
	// usage, so that made stopping a turn a free and repeatable way past them.
	//
	// Nothing further is published, so the developer still sees the turn stop where
	// they stopped it. Termination rests on the provider closing its channel, which
	// the Provider contract requires and every adapter does with a deferred close.
	abandoned := false
	for event := range stream {
		if event.Type == llm.EventDone {
			turn.stopReason = event.StopReason
			if event.Usage != nil {
				turn.usage = *event.Usage
			}
			continue
		}
		if abandoned {
			continue
		}
		switch event.Type {
		case llm.EventTextDelta:
			turn.text += event.Text
			exchange.publish(Event{Type: EventTextDelta, Text: event.Text})
			abandoned = ctx.Err() != nil
		case llm.EventToolCall:
			if event.ToolCall == nil {
				continue
			}
			turn.calls = append(turn.calls, *event.ToolCall)
			exchange.publish(Event{Type: EventToolCall, ToolCall: event.ToolCall})
			abandoned = ctx.Err() != nil
		case llm.EventToolResult:
			// Only an out-of-band provider produces these: the CLI reporting what it
			// already ran over MCP.
			if event.ToolResult == nil {
				continue
			}
			turn.results = append(turn.results, *event.ToolResult)
			exchange.publish(Event{Type: EventToolResult, ToolResult: &tools.Result{
				CallID:  event.ToolResult.CallID,
				Tool:    event.ToolResult.Name,
				Outcome: tools.OutcomeOK,
				Content: event.ToolResult.Content,
				IsError: event.ToolResult.IsError,
			}})
			abandoned = ctx.Err() != nil
		case llm.EventError:
			exchange.publish(Event{Type: EventError, Error: event.Error})
			abandoned = true
		}
	}
	return turn, !abandoned
}

// dispatch runs the turn's tool calls and reports the stop reason when the
// exchange must not continue past them. An empty stop reason means carry on.
//
// Two reasons to stop, and they are not the same thing: a call is waiting for the
// developer (D11), or a call needed their decision and ODE could not ask for it.
// The first resumes from Confirm; the second never will, and saying so is what
// keeps an API consumer from waiting on a confirmation that was never recorded.
func (e *Engine) dispatch(
	ctx context.Context, exchange *Exchange, token TokenSource, session Session, calls []llm.ToolCall,
) (results []tools.Result, stop string) {
	awaiting, unavailable := false, false
	for _, call := range calls {
		// Every call goes through the one Dispatcher, which is where the tier gate
		// lives. Nothing in this loop decides what a tool may do.
		result := e.dispatcher.Dispatch(ctx, tools.Request{
			// Read per call, not per turn: this is the point of TokenSource.
			Token:     token.bearer(),
			UserSub:   session.UserSub,
			SessionID: session.ID,
			// The session's own workbench, so a model working on one operator writes
			// into that operator's checkout and runs in that operator's kernel.
			WorkbenchID: session.WorkbenchID,
			Tier:        session.Tier,
			AutoRun:     session.AutoRun,
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
				// Two things follow from a confirmation that was not recorded, and
				// neither used to happen.
				//
				// The call still needs an answer. Its tool_use block is already in the
				// assistant message this loop is answering, and a tool_use with no
				// tool_result is a conversation both native protocols refuse with a 400
				// — for the rest of the session's life, since the history is stored.
				//
				// And the exchange has to stop. The developer will never be asked,
				// because nothing recorded that there was anything to ask about, so
				// carrying on would call the provider again on the model's word for a
				// tool D11 says nothing may proceed on.
				result = tools.Result{
					CallID:  call.ID,
					Tool:    call.Name,
					Outcome: tools.OutcomeFailed,
					IsError: true,
					Content: map[string]any{
						"error": "this call needs the developer's confirmation and ODE could not " +
							"record the request, so they were never asked. It did not run.",
						"hint": "tell the developer that the confirmation could not be stored, " +
							"and do not retry it.",
					},
				}
				results = append(results, result)
				unavailable = true
				continue
			}
			exchange.publish(Event{Type: EventConfirmation, Confirmation: confirmation.Describe()})
			awaiting = true
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
	switch {
	case awaiting:
		// One recorded confirmation is enough to make waiting the right description,
		// even if another in the same turn could not be recorded: the developer has
		// something to answer, and answering it resumes the exchange.
		return results, StopAwaitingConfirmation
	case unavailable:
		return results, StopConfirmationUnavailable
	}
	return results, ""
}

// appendToolResults stores the answers to a turn's tool calls, retrying once on a
// context detached from the exchange.
//
// The assistant's tool_use blocks are already in the history by the time this
// runs. If their results never land, the session keeps a tool_use that nothing
// answers, which both native protocols refuse with a 400 — so the session becomes
// unusable, not merely incomplete. Two of the three ways this write fails are the
// exchange's own deadline and CancelExchange, and neither says anything about
// whether the store would accept the write; the retry is therefore made without
// that deadline, exactly as the usage record is (see run). A store that is
// genuinely down still loses it, and conversation() repairs that on the way back
// out.
func (e *Engine) appendToolResults(ctx context.Context, sessionID string, message StoredMessage) error {
	err := e.store.AppendMessages(ctx, sessionID, message)
	if err == nil {
		return nil
	}
	// Warn, not error: the retry below usually settles it, and this is the expected
	// shape of a cancelled turn rather than something anyone has to act on.
	slog.WarnContext(ctx, "a turn's tool results could not be stored; retrying detached",
		"session", sessionID, "error", err)

	retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	// The first attempt may have landed despite reporting an error — a transaction
	// that committed and an acknowledgement that never came back. Writing the same
	// results again would answer one tool call twice, and an unmatched tool_result
	// is refused just as an unanswered tool_use is, so the history is re-read
	// first. A read that fails leaves the retry to go ahead: an unanswered call is
	// the more likely and the more damaging of the two.
	if stored, err := e.store.Messages(retryCtx, sessionID); err == nil && alreadyAnswered(stored, message) {
		return nil
	}

	if err := e.store.AppendMessages(retryCtx, sessionID, message); err != nil {
		slog.ErrorContext(ctx, "a turn's tool results were lost; the session's history now "+
			"has an unanswered tool call and is repaired on read",
			"session", sessionID, "error", err)
		return err
	}
	return nil
}

// alreadyAnswered reports whether every tool call the message answers is already
// answered in the stored history.
func alreadyAnswered(stored []StoredMessage, message StoredMessage) bool {
	answered := map[string]bool{}
	for _, existing := range stored {
		for _, content := range existing.Content {
			if content.Type == llm.ContentToolResult {
				answered[content.ToolUseID] = true
			}
		}
	}
	found := false
	for _, content := range message.Content {
		if content.Type != llm.ContentToolResult {
			continue
		}
		if !answered[content.ToolUseID] {
			return false
		}
		found = true
	}
	return found
}

// outOfBandResultMessage packs the results an out-of-band provider reported into
// the user-role message both protocols expect for tool output.
func outOfBandResultMessage(results []llm.ToolResult) StoredMessage {
	content := make([]llm.Content, 0, len(results))
	for _, result := range results {
		encoded, err := json.Marshal(result.Content)
		if err != nil {
			encoded = []byte(`{"error":"the tool result could not be encoded"}`)
		}
		content = append(content, llm.Content{
			Type:       llm.ContentToolResult,
			ToolUseID:  result.CallID,
			ToolName:   result.Name,
			ToolResult: string(encoded),
			IsError:    result.IsError,
		})
	}
	return StoredMessage{Role: llm.RoleUser, Content: content}
}

// persistAbandoned stores what a turn produced before it was abandoned.
//
// Detached from the exchange's context, because the usual reason a turn is
// abandoned is that very context ending, and a write on it would fail for the
// same reason — see appendToolResults, which retries detached for this reason.
//
// An assistant tool_use stored here may end up with no result, since nothing will
// dispatch the calls now. That is the one shape both native protocols refuse, and
// it is repaired on the way out by repairUnansweredToolCalls rather than by
// dropping the record of what the model asked for.
func (e *Engine) persistAbandoned(ctx context.Context, sessionID string, turn turnResult) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := e.persistAssistant(writeCtx, sessionID, turn); err != nil {
		slog.ErrorContext(ctx, "an abandoned turn's answer could not be stored, so the "+
			"developer loses what they watched arrive",
			"session", sessionID, "error", err)
		return
	}
	if len(turn.results) > 0 {
		if err := e.appendToolResults(writeCtx, sessionID,
			outOfBandResultMessage(turn.results)); err != nil {
			slog.ErrorContext(ctx, "an abandoned turn's tool results could not be stored",
				"session", sessionID, "error", err)
		}
	}
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

// conversation renders the stored history as a provider request, answering any
// tool call left unanswered on the way.
func conversation(messages []StoredMessage) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Message())
	}
	return coalesceUserTurns(repairUnansweredToolCalls(out))
}

// orphanedToolResult is what an unanswered tool call is answered with. It says
// the call did not complete rather than inventing an outcome, because the one
// thing that must not happen is a model concluding that a tool ran.
const orphanedToolResult = `{"error":"ODE lost the result of this call, so it is not known ` +
	`whether the tool ran","hint":"do not assume any effect; say so and ask the developer ` +
	`whether to try again"}`

// repairUnansweredToolCalls answers every tool_use that nothing answers.
//
// Anthropic and OpenAI both reject a conversation containing an assistant
// tool_use with no matching tool_result — a 400, every time it is sent. Since the
// history is stored and replayed on every turn, a session that acquires one is not
// merely odd but permanently unusable, with no repair short of deleting it. The
// paths that can produce one are guarded upstream, but not all of them can be:
// a store that will not accept the results leaves an orphan whatever the engine
// does, and an out-of-band provider (§5.7's CLI) stores tool_use blocks for calls
// it ran over MCP that ODE never has results for at all.
//
// The repair happens on the way out rather than in the store, for two reasons.
// The stored history is the record of what actually happened and is not rewritten
// to make it look tidier; what the provider sees is a reading of that record that
// the protocol accepts. And it is retrospective — a session already broken in the
// wild recovers on its next turn, which no write-side fix can do.
func repairUnansweredToolCalls(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		message := messages[i]
		out = append(out, message)
		if message.Role != llm.RoleAssistant {
			continue
		}

		calls := []llm.Content{}
		for _, content := range message.Content {
			if content.Type == llm.ContentToolUse && content.ToolUseID != "" {
				calls = append(calls, content)
			}
		}
		if len(calls) == 0 {
			continue
		}

		// The answers, if there are any, are in the turn that follows.
		var next *llm.Message
		answered := map[string]bool{}
		if i+1 < len(messages) && messages[i+1].Role == llm.RoleUser {
			next = &messages[i+1]
			for _, content := range next.Content {
				if content.Type == llm.ContentToolResult {
					answered[content.ToolUseID] = true
				}
			}
		}

		missing := make([]llm.Content, 0, len(calls))
		for _, call := range calls {
			if answered[call.ToolUseID] {
				continue
			}
			missing = append(missing, llm.Content{
				Type:       llm.ContentToolResult,
				ToolUseID:  call.ToolUseID,
				ToolName:   call.ToolName,
				ToolResult: orphanedToolResult,
				IsError:    true,
			})
		}
		if len(missing) == 0 {
			continue
		}

		if next == nil {
			out = append(out, llm.Message{Role: llm.RoleUser, Content: missing})
			continue
		}
		// Merged into the following user turn rather than inserted before it as a
		// second user message: a tool result has to come first in the turn that
		// answers the call, and consecutive same-role messages are a shape not every
		// provider accepts.
		repaired := *next
		repaired.Content = append(missing, next.Content...)
		out = append(out, repaired)
		i++
	}
	return out
}

// coalesceUserTurns merges consecutive user messages into one.
//
// The history acquires that shape when ODE appends something without starting a turn
// — a move note (MoveSession) sitting in front of whatever the developer types next
// — and consecutive same-role messages are a shape not every provider accepts, which
// is the same reason repairUnansweredToolCalls merges into the following turn rather
// than inserting a second user message before it.
//
// So the merge happens on the way out, over the stored record rather than in it: the
// two messages are two things that happened, one of them not the developer's, and the
// history keeps saying so.
//
// Chronological order is kept, which is what keeps a tool_result first in the turn
// that answers a call: a results turn followed by a note merges to [results…, note]
// and never the other way round.
func coalesceUserTurns(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		last := len(out) - 1
		if last >= 0 && message.Role == llm.RoleUser && out[last].Role == llm.RoleUser {
			// Into a fresh slice: the content underneath belongs to the caller's stored
			// messages, and appending in place would grow into whatever shares its array.
			merged := out[last]
			content := make([]llm.Content, 0, len(merged.Content)+len(message.Content))
			content = append(content, merged.Content...)
			content = append(content, message.Content...)
			merged.Content = content
			out[last] = merged
			continue
		}
		out = append(out, message)
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

// RecordCreation and Creations implement tools.Creations, so the confirmed create
// tools can record what they made and the confirmed delete tools can check it.
//
// The session is not re-authorised here. Both are reached only from an executor,
// which runs behind Dispatch, which is reached only from a turn this engine
// started for an authenticated user in a session it already resolved — so a check
// here would be re-deriving something two layers up already established. What it
// does refuse is an unknown session, because writing a creation into one that does
// not exist means the object could never be deleted again.
func (e *Engine) RecordCreation(ctx context.Context, sessionID string, created tools.Creation) error {
	if _, found, err := e.store.Session(ctx, sessionID); err != nil {
		return err
	} else if !found {
		return ErrNoSuchSession
	}
	return e.store.RecordCreation(ctx, sessionID, created)
}

func (e *Engine) Creations(ctx context.Context, sessionID string) ([]tools.Creation, error) {
	return e.store.Creations(ctx, sessionID)
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
