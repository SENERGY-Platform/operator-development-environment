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

// Package chat is the conversation surface: sessions, the tool loop, the
// exposure tier a session sits at, and the confirmations a developer owes.
//
// The session is where §3.2's tier lives — "session-scoped, developer-settable,
// default L0" — and every change to it is written to an append-only audit trail
// with the timestamp and the user, because the spec asks for that and because a
// tier the developer cannot see the history of is not really under their control.
//
// The tool loop is here rather than in pkg/llm because it is not a provider
// concern: it is ODE deciding, per iteration, what the tier permits, what the
// developer must confirm, and when to stop. pkg/llm hands up tool calls; this
// package decides what happens to them, by asking the one Dispatcher.
package chat

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

var (
	ErrNoSuchSession      = errors.New("chat: no such session")
	ErrNotOwner           = errors.New("chat: this session belongs to another user")
	ErrInvalidRequest     = errors.New("chat: invalid request")
	ErrNoSuchConfirmation = errors.New("chat: no such confirmation")
	ErrAlreadyResolved    = errors.New("chat: this confirmation was already resolved")
)

// Session is one conversation.
type Session struct {
	ID string `json:"id"`
	// UserSub owns the session. Every read checks it: a session id in a URL must
	// not be enough to read someone else's conversation.
	UserSub  string `json:"user_sub"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// Tier is the exposure tier (§3.2). Default L0.
	Tier tools.Tier `json:"exposure_tier"`

	// WorkbenchID is the working context this conversation acts in: which checkout
	// write_file writes into, and which kernel run_code runs in. Two sessions may
	// name the same one — talking about one operator from two angles — or different
	// ones, which is a developer working on two operators at once.
	//
	// Empty is a session written before workbenches existed, or one whose workbench
	// has since been closed. Both resolve to the developer's only workbench when
	// they have one, so no conversation loses its code context.
	WorkbenchID string `json:"workbench_id,omitempty"`
	// AutoRun is the developer's standing answer to a `run_code` confirmation whose
	// code pkg/plaincode recognises: run it rather than asking again.
	//
	// Per session, not per developer, because the exposure a conversation is
	// working at is already a per-session decision and this belongs beside it. Off
	// unless turned on, and turning it on is a developer action with no tool behind
	// it — `set_auto_run` is in the denied set for the same reason
	// `set_exposure_tier` is, so a model cannot widen what it is allowed to do
	// without being asked.
	//
	// It is a convenience and not a boundary. See pkg/plaincode.
	AutoRun bool `json:"auto_run"`

	// Selection is the data selection the developer has confirmed, if any (§5.2's
	// last step). Stored on the session because §5.10 says confirmations persist as
	// session overrides.
	Selection *tools.ProposedSelection `json:"selection,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// MessageCount lets a listing show session size without loading every message.
	MessageCount int `json:"message_count"`
}

// Origin says who put a message in the conversation.
//
// Two values, and the distinction is the point rather than bookkeeping. Most of
// what ODE stores with the user role is the developer's own typing; §5.13's
// summary is not — it is a structured document ODE composed and injected when a
// run finished, and the assistant answers it as though it had been asked. A reader
// that could not tell the two apart would show the developer a message they never
// wrote, in their own voice, containing a block of JSON.
//
// The empty string is the developer, so every message written before this existed
// reads as theirs, which is what they were.
const (
	OriginDeveloper = ""
	// OriginODE is a message ODE composed and injected on the developer's behalf.
	OriginODE = "ode"
)

// StoredMessage is one turn as persisted. The content blocks are kept structurally
// rather than flattened to text, so a conversation resumed after a restart still
// carries its tool calls and their results — a flattened history would replay as
// prose and the model would lose every result it had already read.
type StoredMessage struct {
	SessionID string        `json:"session_id"`
	Seq       int64         `json:"seq"`
	Role      llm.Role      `json:"role"`
	Content   []llm.Content `json:"content"`
	CreatedAt time.Time     `json:"created_at"`
	// Origin distinguishes what the developer typed from what ODE injected. Absent
	// on the wire for a developer's own message, so the common case costs nothing and
	// a reader that ignores the field behaves exactly as it did before.
	Origin string `json:"origin,omitempty"`
	// Subject names what an injected message is about — the experiment id, for a
	// §5.13 summary — so a pane can render it as a result card rather than as prose,
	// and so a later reader can find the message belonging to one run without
	// parsing it. Empty for anything a developer typed.
	Subject string `json:"subject,omitempty"`
}

// Injected reports whether ODE composed this message rather than the developer.
func (m StoredMessage) Injected() bool { return m.Origin == OriginODE }

// Message is the SPA's view of a turn: the same content, plus the tool outcomes
// that belong beside it.
func (m StoredMessage) Message() llm.Message {
	return llm.Message{Role: m.Role, Content: m.Content}
}

// TierChange is one entry of §3.2's audit trail ("Every change logged with
// timestamp and user").
type TierChange struct {
	SessionID string     `json:"session_id"`
	UserSub   string     `json:"user_sub"`
	From      tools.Tier `json:"from"`
	To        tools.Tier `json:"to"`
	At        time.Time  `json:"at"`
}

// Confirmation is a held tool call awaiting the developer's decision (D11).
type Confirmation struct {
	tools.PendingConfirmation
	UserSub    string     `json:"user_sub"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// Decision is empty while pending, then "approved" or "rejected".
	Decision string `json:"decision,omitempty"`
	// OutOfBand marks a call a provider's own tool loop is holding open right now,
	// waiting for this decision (see hold.go). It changes what answering it means:
	// a held call is answered in place and the running turn carries the result,
	// whereas an ordinary confirmation resumes a turn that stopped.
	//
	// Deliberately not persisted. It is not a property of the confirmation but of
	// whether something is waiting on it at this moment, and nothing waits across a
	// restart — a stored flag would outlive the caller and describe a hold that no
	// longer exists. The engine sets it from its own registry of live holds.
	OutOfBand bool `json:"out_of_band,omitempty"`
}

const (
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
)

func (c Confirmation) Pending() bool { return c.Decision == "" }

// Describe renders the held call for the developer. A confirmation prompt that
// shows only a tool name asks them to approve something they cannot see, so the
// arguments travel with it.
func (c Confirmation) Describe() map[string]any {
	var input any
	if len(c.Input) > 0 {
		if err := json.Unmarshal(c.Input, &input); err != nil {
			input = string(c.Input)
		}
	}
	described := map[string]any{
		"id":         c.ID,
		"tool":       c.Tool,
		"input":      input,
		"tier":       c.Tier,
		"created_at": c.CreatedAt,
	}
	if c.OutOfBand {
		described["out_of_band"] = true
	}
	return described
}
