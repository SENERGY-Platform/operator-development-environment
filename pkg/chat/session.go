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

	// Selection is the data selection the developer has confirmed, if any (§5.2's
	// last step). Stored on the session because §5.10 says confirmations persist as
	// session overrides.
	Selection *tools.ProposedSelection `json:"selection,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// MessageCount lets a listing show session size without loading every message.
	MessageCount int `json:"message_count"`
}

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
}

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
	return map[string]any{
		"id":         c.ID,
		"tool":       c.Tool,
		"input":      input,
		"tier":       c.Tier,
		"created_at": c.CreatedAt,
	}
}
