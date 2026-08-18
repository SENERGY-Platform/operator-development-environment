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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Millis is a duration on the wire, in whole milliseconds.
//
// It exists because time.Duration marshals as an integer count of *nanoseconds*,
// so a JSON field named duration_ms carrying a time.Duration is off by a factor
// of a million and says so in its own name. A reader would have no way to notice:
// both are plausible integers.
type Millis time.Duration

func (m Millis) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(time.Duration(m).Milliseconds(), 10)), nil
}

func (m *Millis) UnmarshalJSON(data []byte) error {
	milliseconds, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return fmt.Errorf("tools: duration_ms must be an integer: %w", err)
	}
	*m = Millis(time.Duration(milliseconds) * time.Millisecond)
	return nil
}

// Duration is the value as a Go duration, for anything that computes with it.
func (m Millis) Duration() time.Duration { return time.Duration(m) }

// Outcome classifies what happened to a tool call, for the UI, the audit log and
// the tests. It is separate from the content handed to the model, because the
// model needs prose it can act on and the operator needs a value they can count.
type Outcome string

const (
	OutcomeOK                   Outcome = "ok"
	OutcomeUnknownTool          Outcome = "unknown_tool"
	OutcomeBlockedByTier        Outcome = "blocked_by_tier"
	OutcomeNotImplemented       Outcome = "not_implemented"
	OutcomeAwaitingConfirmation Outcome = "awaiting_confirmation"
	OutcomeInvalidInput         Outcome = "invalid_input"
	OutcomeFailed               Outcome = "failed"
)

// TierRefusal is §3.2's structured refusal, verbatim.
//
// The two field names are fixed by the spec because the LLM is expected to relay
// them: "so the assistant asks the developer to raise it rather than failing
// opaquely". Hint exists to make that relay likely rather than hoped for — a bare
// pair of tier names is a fact, not an instruction, and models routinely respond
// to a fact by retrying it.
type TierRefusal struct {
	BlockedByTier Tier   `json:"blocked_by_tier"`
	Required      Tier   `json:"required"`
	Tool          string `json:"tool,omitempty"`
	Hint          string `json:"hint,omitempty"`
}

// NotImplemented is the refusal for a tool that §5.8 declares and this build does
// not yet provide. It cannot normally be reached, because an unimplemented tool
// is never advertised; it exists for the MCP surface, where a client may call a
// name it read from the published table rather than from tools/list.
type NotImplemented struct {
	Error  string `json:"error"`
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
	Hint   string `json:"hint,omitempty"`
}

// UnknownTool is the refusal for a name that is not in the registry at all —
// including, deliberately, every name on the Denied list.
//
// A denied capability is indistinguishable from a typo here, and that is the
// correct answer rather than a lazy one: telling the model "that tool is
// forbidden" describes a capability boundary it will then try to talk its way
// around. "No such tool" is the truth and ends the line of enquiry.
type UnknownTool struct {
	Error string   `json:"error"`
	Tool  string   `json:"tool"`
	Known []string `json:"known_tools,omitempty"`
}

// ConfirmationRequired is what the model is told when a tool needs the developer
// to agree first (D11, §5.10).
type ConfirmationRequired struct {
	RequiresConfirmation bool   `json:"requires_confirmation"`
	ConfirmationID       string `json:"confirmation_id"`
	Tool                 string `json:"tool"`
	Hint                 string `json:"hint,omitempty"`
}

// Failure is a tool that ran and did not work. The message is the executor's,
// which for a platform read means the platform's own verdict.
type Failure struct {
	Error string `json:"error"`
	Tool  string `json:"tool"`
}

// PendingConfirmation is a call held back for developer agreement.
type PendingConfirmation struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	// Tier is the tier the session was at when the model asked. Recorded rather
	// than trusted: Confirm re-checks against the tier at confirmation time, and
	// the difference between the two is worth being able to see afterwards.
	Tier      Tier      `json:"tier"`
	CreatedAt time.Time `json:"created_at"`
}

// Result is one dispatched call.
type Result struct {
	CallID  string  `json:"call_id"`
	Tool    string  `json:"tool"`
	Outcome Outcome `json:"outcome"`
	// Content is what goes back to the model as the tool result.
	Content any `json:"content"`
	// IsError marks the result as an error in the provider's own tool-result
	// shape, which both native protocols carry and which materially changes how a
	// model reads it.
	IsError bool `json:"is_error"`
	// Confirmation is set only for OutcomeAwaitingConfirmation.
	Confirmation *PendingConfirmation `json:"confirmation,omitempty"`
	Duration     Millis               `json:"duration_ms"`
}

// AuditSink receives every dispatched call. Implemented by the admin service, so
// that "who ran which tool at which tier" is answerable without reading logs.
type AuditSink interface {
	RecordToolCall(ctx context.Context, entry ToolCallRecord)
}

// ToolCallRecord is one audited dispatch.
type ToolCallRecord struct {
	UserSub   string    `json:"user_sub"`
	SessionID string    `json:"session_id"`
	Tool      string    `json:"tool"`
	Tier      Tier      `json:"tier"`
	Outcome   Outcome   `json:"outcome"`
	Duration  Millis    `json:"duration_ms"`
	At        time.Time `json:"at"`
}

// IDs mints confirmation ids. An interface so tests get deterministic ones.
type IDs interface{ NewID() string }

// Dispatcher is the enforcement point of §3.2: "Enforced in ToolDispatcher
// before any tool executes. Never client-side."
type Dispatcher struct {
	registry *Registry
	audit    AuditSink
	ids      IDs
	now      func() time.Time
}

func NewDispatcher(registry *Registry, audit AuditSink, ids IDs) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("tools: a registry is required")
	}
	if ids == nil {
		return nil, errors.New("tools: an id source is required")
	}
	return &Dispatcher{registry: registry, audit: audit, ids: ids, now: time.Now}, nil
}

// Registry exposes the surface for the routes that publish it and for the MCP
// server, which advertises the same definitions over a different transport.
func (d *Dispatcher) Registry() *Registry { return d.registry }

// Dispatch runs one tool call, in the order that is the milestone's exit
// criterion. Every gate precedes execution, and there is no path to an executor
// that skips one:
//
//  1. the tool exists at all — a denied name never gets past here;
//  2. it is implemented in this build;
//  3. the session's tier permits it;
//  4. it does not need developer confirmation, or it does and is held;
//  5. its input parses;
//  6. only then does it run.
//
// Spend limits are deliberately *not* checked here. They bound LLM token cost
// (§3.3), which is incurred by the provider call and not by a platform read, so
// pkg/chat checks them before each provider request. Checking them again here
// would suggest a tool call costs provider tokens, which it does not.
func (d *Dispatcher) Dispatch(ctx context.Context, req Request, call Call) Result {
	started := d.now()

	result := d.dispatch(ctx, req, call)
	result.CallID = call.ID
	result.Tool = call.Name
	result.Duration = Millis(d.now().Sub(started))

	if d.audit != nil {
		d.audit.RecordToolCall(ctx, ToolCallRecord{
			UserSub:   req.UserSub,
			SessionID: req.SessionID,
			Tool:      call.Name,
			Tier:      req.Tier,
			Outcome:   result.Outcome,
			Duration:  result.Duration,
			At:        started,
		})
	}

	// Logged at info rather than debug: which tools an LLM reached for, and which
	// were refused, is the record that makes the tier argument auditable after the
	// fact. It carries no values and no intent text.
	slog.InfoContext(ctx, "tool dispatched",
		"tool", call.Name, "outcome", result.Outcome, "tier", req.Tier.String(),
		"session", req.SessionID, "duration_ms", result.Duration.Duration().Milliseconds())

	return result
}

func (d *Dispatcher) dispatch(ctx context.Context, req Request, call Call) Result {
	definition, found := d.registry.Lookup(call.Name)
	if !found {
		return Result{
			Outcome: OutcomeUnknownTool,
			IsError: true,
			Content: UnknownTool{
				Error: "unknown_tool",
				Tool:  call.Name,
				Known: names(d.registry.Available(req.Tier)),
			},
		}
	}

	if !definition.Implemented() {
		return Result{
			Outcome: OutcomeNotImplemented,
			IsError: true,
			Content: NotImplemented{
				Error:  "not_implemented",
				Tool:   call.Name,
				Reason: definition.Unavailable,
				Hint: fmt.Sprintf(
					"this tool is part of the documented surface but is not callable here: %s. Do not retry it.",
					definition.Unavailable),
			},
		}
	}

	if !req.Tier.Permits(definition.MinTier) {
		return Result{
			Outcome: OutcomeBlockedByTier,
			IsError: true,
			Content: TierRefusal{
				BlockedByTier: req.Tier,
				Required:      definition.MinTier,
				Tool:          call.Name,
				Hint: fmt.Sprintf(
					"the developer controls this. Ask them to raise the exposure tier to %s and explain "+
						"what %s would tell you; do not retry at %s.",
					definition.MinTier, call.Name, req.Tier),
			},
		}
	}

	if definition.Confirm {
		pending := &PendingConfirmation{
			ID:        d.ids.NewID(),
			SessionID: req.SessionID,
			CallID:    call.ID,
			Tool:      call.Name,
			Input:     call.Input,
			Tier:      req.Tier,
			CreatedAt: d.now(),
		}
		return Result{
			Outcome:      OutcomeAwaitingConfirmation,
			Confirmation: pending,
			// Not an error: the model did nothing wrong and the call may yet
			// succeed. Marking it an error teaches the model to avoid a tool whose
			// whole purpose is to ask the developer something.
			IsError: false,
			Content: ConfirmationRequired{
				RequiresConfirmation: true,
				ConfirmationID:       pending.ID,
				Tool:                 call.Name,
				Hint: "the developer has been asked to confirm. Wait for their decision; " +
					"do not call this again and do not assume it was accepted.",
			},
		}
	}

	return d.execute(ctx, req, call, definition)
}

// Confirm runs a call the developer has agreed to.
//
// It re-checks the tier against the session's tier *now* rather than the tier
// recorded on the pending confirmation. The two can differ: a developer may
// propose something at L2, lower the tier, and only then confirm. Trusting the
// recorded tier would turn a pending confirmation into a way to run an
// L2 tool at L0, which is exactly the hole §3.2 exists to close.
func (d *Dispatcher) Confirm(ctx context.Context, req Request, pending PendingConfirmation) Result {
	started := d.now()
	call := Call{ID: pending.CallID, Name: pending.Tool, Input: pending.Input}

	result := func() Result {
		definition, found := d.registry.Lookup(pending.Tool)
		if !found || !definition.Implemented() {
			return Result{
				Outcome: OutcomeUnknownTool,
				IsError: true,
				Content: UnknownTool{Error: "unknown_tool", Tool: pending.Tool},
			}
		}
		if !req.Tier.Permits(definition.MinTier) {
			return Result{
				Outcome: OutcomeBlockedByTier,
				IsError: true,
				Content: TierRefusal{
					BlockedByTier: req.Tier,
					Required:      definition.MinTier,
					Tool:          pending.Tool,
					Hint: "the exposure tier was lowered after this was proposed, so the " +
						"confirmation no longer authorises it.",
				},
			}
		}
		return d.execute(ctx, req, call, definition)
	}()

	result.CallID = pending.CallID
	result.Tool = pending.Tool
	result.Duration = Millis(d.now().Sub(started))

	if d.audit != nil {
		d.audit.RecordToolCall(ctx, ToolCallRecord{
			UserSub:   req.UserSub,
			SessionID: req.SessionID,
			Tool:      pending.Tool,
			Tier:      req.Tier,
			Outcome:   result.Outcome,
			Duration:  result.Duration,
			At:        started,
		})
	}
	slog.InfoContext(ctx, "confirmed tool dispatched",
		"tool", pending.Tool, "outcome", result.Outcome, "tier", req.Tier.String(),
		"session", req.SessionID)

	return result
}

// execute is past every gate. It parses the input and runs the executor.
func (d *Dispatcher) execute(ctx context.Context, req Request, call Call, definition Definition) Result {
	// An absent input object is normal for a no-argument tool, and `null` arrives
	// from at least one provider for the same case. Both become an empty object so
	// executors can unmarshal unconditionally.
	req.Input = call.Input
	if len(req.Input) == 0 || string(req.Input) == "null" {
		req.Input = json.RawMessage(`{}`)
	}

	// The tool name is stamped here rather than trusted from the executor, so a
	// progress line cannot be attributed to the wrong tool.
	if req.Report != nil {
		report := req.Report
		req.Report = func(progress Progress) {
			progress.Tool = call.Name
			report(progress)
		}
	}

	output, err := definition.executor(ctx, req)
	if err != nil {
		outcome := OutcomeFailed
		if errors.Is(err, ErrInvalidInput) {
			outcome = OutcomeInvalidInput
		}
		return Result{
			Outcome: outcome,
			IsError: true,
			Content: Failure{Error: err.Error(), Tool: call.Name},
		}
	}
	return Result{Outcome: OutcomeOK, Content: output}
}

// ErrInvalidInput marks an executor failure caused by the model's arguments
// rather than by the platform, so the outcome distinguishes "the model asked
// wrongly" from "the read failed".
var ErrInvalidInput = errors.New("invalid tool input")

func names(definitions []Definition) []string {
	out := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, definition.Name)
	}
	return out
}
