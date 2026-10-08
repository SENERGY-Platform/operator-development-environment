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

package llm

import "encoding/json"

// EventType is the normalised event stream of §5.7. Every provider maps onto
// these six, and provider-specific shapes must not leak upward.
//
// The list is closed on purpose. A type added for one provider's convenience
// would be a shape the SPA has to know about per provider, which is the coupling
// this interface exists to prevent. thinking_delta is the one addition, and not
// such a type: reasoning models of both vendors think before they answer and can
// return a summary of it, and the event carries text and nothing else.
type EventType string

const (
	// EventTextDelta carries an incremental piece of assistant text.
	EventTextDelta EventType = "text_delta"
	// EventThinkingDelta carries an incremental piece of the provider's summary of
	// what the model is thinking. It is never part of the answer: it is not stored
	// as text, the model is never handed it back as text, and no view renders it
	// from the history — the thinking block it belongs to goes back to the API as
	// a block (Content). It exists so a turn that thinks for minutes before it says
	// anything is visibly working.
	EventThinkingDelta EventType = "thinking_delta"
	// EventToolCall is a complete tool invocation the model asked for. Emitted
	// once the arguments are whole: a partially decoded call is useless to a
	// dispatcher, and both native protocols stream arguments in fragments.
	EventToolCall EventType = "tool_call"
	// EventToolResult is what ODE gave back for a tool call. It comes from the
	// dispatcher rather than the provider, and is part of this stream because the
	// SPA renders one conversation, not two interleaved ones.
	EventToolResult EventType = "tool_result"
	// EventDone ends a turn and carries the usage that turn cost.
	EventDone EventType = "done"
	// EventError ends a turn abnormally. It is an event rather than a returned
	// error because a turn can fail after text has already been streamed, and the
	// SPA needs to keep what it has and say what went wrong.
	EventError EventType = "error"
)

// StopReasonCancelled and StopReasonError name the two ways a turn can end
// without the model having finished.
//
// They exist because a turn that ended early still has to report what it cost:
// the provider billed the input it read and the output it produced whether or not
// anyone waited for the answer, and §3.3's caps are computed from recorded usage.
// So such a turn emits a done event like any other — and the stop reason is what
// keeps anything downstream from reading it as a clean finish. Neither value can
// collide with a provider's own: Anthropic reports end_turn, max_tokens, tool_use
// or stop_sequence, OpenAI stop, length, tool_calls or content_filter.
const (
	StopReasonCancelled = "cancelled"
	StopReasonError     = "error"
)

// StopReasonMaxTokens is a turn cut off at its response bound before the model
// finished. Anthropic reports it under this name and the OpenAI adapter maps its
// `length` onto it, so a consumer looks for one value rather than one per
// protocol.
//
// The bound covers thinking and answer together. A turn that spent all of it
// thinking ends with no text and no tool call, which is a turn nobody can see
// unless something downstream reads this value.
const StopReasonMaxTokens = "max_tokens"

// StopReasonRefusal is a turn the provider declined: Anthropic's safety
// classifiers or the model itself, and OpenAI's content_filter, which the OpenAI
// adapter maps onto it. It arrives as a successful response, and like a turn cut
// off at the bound it can carry no text at all.
const StopReasonRefusal = "refusal"

// Event is one item of the normalised stream.
type Event struct {
	Type EventType `json:"type"`

	// Text is set on text_delta and thinking_delta.
	Text string `json:"text,omitempty"`

	// ToolCall is set on tool_call.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	// ToolResult is set on tool_result.
	ToolResult *ToolResult `json:"tool_result,omitempty"`

	// Usage and StopReason are set on done.
	Usage      *Usage `json:"usage,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	// StopDetail is set on done when the provider says why it stopped beyond the
	// reason itself: for a refusal, the policy category.
	StopDetail string `json:"stop_detail,omitempty"`
	// Content is set on done by a provider whose next request needs the turn back
	// exactly as it produced it, in its own order, thinking blocks included. The
	// text and tool calls in it are the ones already streamed. A consumer that
	// stores the turn stores this rather than rebuilding it; empty means rebuild.
	Content []Content `json:"content,omitempty"`

	// Err is set on error.
	Err error `json:"-"`
	// Error is the message the SPA sees. Kept separate from Err because an error
	// value does not marshal and the wire form has to say something.
	Error string `json:"error,omitempty"`
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the outcome of one tool call, as fed back to the model.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Content any    `json:"content"`
	IsError bool   `json:"is_error"`
}

// Usage is what one turn cost. Token counts come from the provider; the cost is
// ODE's own estimate from configured prices (§3.3).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CachedInputTokens is reported where the provider distinguishes it. Kept
	// separate rather than folded into InputTokens because it is priced
	// differently, and an accounting record that hid it would overstate spend.
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	// CacheWriteTokens is what it cost to *write* a cache entry, reported by
	// Anthropic as cache_creation_input_tokens and priced above plain input rather
	// than below it. Separate for the same reason CachedInputTokens is, and the
	// omission is the more expensive one: folded into InputTokens it would hide the
	// premium, and left out entirely it would let a session that rewrites its cache
	// every turn run past a cap that never saw the tokens.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`

	Provider string  `json:"provider,omitempty"`
	Model    string  `json:"model,omitempty"`
	CostEUR  float64 `json:"cost_eur,omitempty"`
	// CostEstimated is true when the cost came from a configured price rather
	// than from the provider. It always does today, and saying so keeps a
	// developer from reading the figure as an invoice (§3.3).
	CostEstimated bool `json:"cost_estimated,omitempty"`
	// CostBreakdown is CostEUR taken apart by kind of token, with the rate each was
	// priced at. Nil wherever CostEstimated is false: an unpriced model has no rate.
	CostBreakdown *CostBreakdown `json:"cost_breakdown,omitempty"`
}

// Add accumulates usage across the turns of one exchange, which is what a tool
// loop produces: one provider call per iteration, one bill for the exchange.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.CostEUR += other.CostEUR
	if other.Provider != "" {
		u.Provider = other.Provider
	}
	if other.Model != "" {
		u.Model = other.Model
	}
	u.CostEstimated = u.CostEstimated || other.CostEstimated
	if other.CostBreakdown != nil {
		// A copy, so the sum never writes into the breakdown of the turn it came from.
		merged := CostBreakdown{}
		if u.CostBreakdown != nil {
			merged = *u.CostBreakdown
		}
		merged.add(*other.CostBreakdown)
		u.CostBreakdown = &merged
	}
}

// TextEvent, ToolCallEvent, DoneEvent and ErrorEvent are constructors, so a
// provider cannot forget to set the field its type implies.
func TextEvent(text string) Event { return Event{Type: EventTextDelta, Text: text} }

func ThinkingEvent(text string) Event { return Event{Type: EventThinkingDelta, Text: text} }

func ToolCallEvent(call ToolCall) Event {
	return Event{Type: EventToolCall, ToolCall: &call}
}

func ToolResultEvent(result ToolResult) Event {
	return Event{Type: EventToolResult, ToolResult: &result}
}

func DoneEvent(stopReason string, usage Usage) Event {
	return Event{Type: EventDone, StopReason: stopReason, Usage: &usage}
}

func ErrorEvent(err error) Event {
	if err == nil {
		return Event{Type: EventError, Error: "unknown error"}
	}
	return Event{Type: EventError, Err: err, Error: err.Error()}
}
