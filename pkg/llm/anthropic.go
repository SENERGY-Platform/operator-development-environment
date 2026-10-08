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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider is §5.7's first row: the Anthropic API over
// anthropics/anthropic-sdk-go, with native tool use.
type AnthropicProvider struct {
	name    string
	client  anthropic.Client
	options AnthropicOptions
	pricing *Pricing
}

type AnthropicOptions struct {
	APIKey  string
	BaseURL string
	// Models is the permitted list, first entry the default.
	Models []string
	// MaxTokens is the default response bound. The API requires the field, so a
	// zero here becomes defaultMaxTokens rather than a 400 at request time.
	MaxTokens int
	// Effort maps to output_config.effort. Empty sends none and takes the API
	// default, which is "high".
	Effort string
	// AdaptiveThinking sends thinking: {type: "adaptive", display: "summarized"}.
	//
	// The display is what makes the thinking visible. The current models default
	// to "omitted", which streams thinking blocks with empty text: the thinking is
	// done and billed the same, and the developer sees a long pause. "summarized"
	// streams the provider's summary of it instead, forwarded as thinking_delta.
	// The raw reasoning is not available under any setting.
	//
	// It also sends block_binding.prefix_mismatch_behavior "drop_block". ODE sends
	// thinking blocks back (see toAnthropicMessages), and the API checks that the
	// conversation before each one is byte for byte the one that produced it. An
	// account created on or after 2026-08-31 gets a 400 when it is not, and keeps
	// getting it on every later turn, since the history is stored; dropping the
	// block instead costs that turn the reasoning and nothing else. A drop is
	// reported in input_transformations and logged, because it means ODE edited a
	// history it promised not to. Only alongside the thinking configuration, because
	// the field lives inside it; the API accepts it on every model that accepts
	// adaptive thinking, enforcing or not, and a deployment whose models predate
	// that leaves this off and sends neither.
	//
	// Worth setting deliberately rather than defaulting on: on the current models
	// it is either the default already or the recommended mode, but a request that
	// names an older model and sends it is refused. Off means "send no thinking
	// configuration at all", which every model accepts.
	AdaptiveThinking bool
}

const (
	// defaultMaxTokens bounds thinking and answer together. At 8192 a turn at
	// effort xhigh could spend the whole bound thinking and end with nothing to
	// show; the request streams, so a larger bound costs no HTTP timeout.
	defaultMaxTokens = 64000
	// defaultAnthropicModel is the current Opus. Named as a plain string because
	// anthropic.Model is a string alias and the SDK carries no constant for it.
	defaultAnthropicModel = "claude-opus-5-5"

	// thinkingBindingBeta lets a request say what happens to a replayed thinking
	// block whose conversation no longer matches the one that produced it.
	thinkingBindingBeta = "thinking-binding-controls-2026-08-01"
)

// NewAnthropicProvider wires the provider. An empty API key is an error rather
// than a provider that fails on first use: §3.3 has a central key, and a
// deployment that forgot it should hear so at startup.
func NewAnthropicProvider(name string, opts AnthropicOptions, pricing *Pricing) (*AnthropicProvider, error) {
	if name == "" {
		name = "anthropic"
	}
	if opts.APIKey == "" {
		return nil, fmt.Errorf("llm: provider %q needs an API key", name)
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = defaultMaxTokens
	}
	if len(opts.Models) == 0 {
		opts.Models = []string{defaultAnthropicModel}
	}

	requestOptions := []option.RequestOption{option.WithAPIKey(opts.APIKey)}
	if opts.BaseURL != "" {
		requestOptions = append(requestOptions, option.WithBaseURL(opts.BaseURL))
	}

	return &AnthropicProvider{
		name:    name,
		client:  anthropic.NewClient(requestOptions...),
		options: opts,
		pricing: pricing,
	}, nil
}

func (p *AnthropicProvider) Name() string { return p.name }

func (p *AnthropicProvider) Capabilities() Capabilities {
	return Capabilities{
		Tools:     true,
		Streaming: true,
		System:    true,
		MaxTokens: p.options.MaxTokens,
		Models:    append([]string{}, p.options.Models...),
		// The API rejects a request with no model, so ODE has to have one.
		ModelRequired: true,
	}
}

func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	params, err := p.params(req)
	if err != nil {
		return nil, err
	}

	events := make(chan Event, 16)
	go func() {
		defer close(events)

		stream := p.client.Messages.NewStreaming(ctx, params, p.requestOptions()...)
		// Accumulate rebuilds the complete message from the deltas, which is what
		// makes tool arguments usable: both this API and OpenAI's stream a tool's
		// JSON in fragments, and a half-decoded object cannot be dispatched.
		message := anthropic.Message{}

		for stream.Next() {
			event := stream.Current()
			if err := message.Accumulate(event); err != nil {
				deliverDone(ctx, events, DoneEvent(StopReasonError, p.usage(&message)))
				send(ctx, events, ErrorEvent(fmt.Errorf("llm: %s: accumulate: %w", p.name, err)))
				return
			}

			if start, ok := event.AsAny().(anthropic.MessageStartEvent); ok {
				p.reportTransformations(ctx, start)
			}

			// Text and the thinking summary are forwarded live. A tool call is
			// forwarded once whole, below, because a partial one is not actionable.
			if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				var live Event
				switch piece := delta.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					live = TextEvent(piece.Text)
				case anthropic.ThinkingDelta:
					live = ThinkingEvent(piece.Thinking)
				}
				if live.Text != "" && !send(ctx, events, live) {
					// send only fails when the caller has gone, which here means the
					// developer stopped the turn. It was still billed.
					deliverDone(ctx, events, DoneEvent(StopReasonCancelled, p.usage(&message)))
					return
				}
			}
		}

		if err := stream.Err(); err != nil {
			// The usage accumulated so far is reported rather than discarded. It is
			// partial: message_start carries the input tokens, and the output count is
			// only completed by the message_delta that arrives at the end, so a turn
			// stopped in the middle knows what it read but not exactly what it wrote.
			// Input dominates the case this matters for — a large context, streamed and
			// then stopped — and the alternative was a zero usage and no accounting row
			// at all, which made cancelling every turn a free way past §3.3's caps.
			usage := p.usage(&message)
			if errors.Is(err, context.Canceled) {
				// A cancelled turn is the developer stopping it, not a failure.
				slog.DebugContext(ctx, "anthropic stream cancelled", "provider", p.name)
				deliverDone(ctx, events, DoneEvent(StopReasonCancelled, usage))
				return
			}
			// Before the error event, because a consumer stops reading at one and
			// would otherwise never see what the turn cost.
			deliverDone(ctx, events, DoneEvent(StopReasonError, usage))
			send(ctx, events, ErrorEvent(fmt.Errorf("llm: %s: %w", p.name, err)))
			return
		}

		// The turn as the API returned it, in its order, for the engine to store and
		// this adapter to send back. Thinking blocks are why it is needed: they have
		// to return where they were, and the engine's own text-then-calls rebuild
		// would move them. Block types ODE never asks for — server tools — are left
		// out, which only an integration that used them could notice.
		content := make([]Content, 0, len(message.Content))
		for _, block := range message.Content {
			switch block := block.AsAny().(type) {
			case anthropic.TextBlock:
				content = append(content, Content{Type: ContentText, Text: block.Text})
			case anthropic.ThinkingBlock:
				content = append(content, Content{
					Type: ContentThinking, Thinking: block.Thinking, Signature: block.Signature,
				})
			case anthropic.RedactedThinkingBlock:
				content = append(content, Content{Type: ContentRedactedThinking, Data: block.Data})
			case anthropic.ToolUseBlock:
				input := json.RawMessage(block.Input)
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				content = append(content, Content{
					Type: ContentToolUse, ToolUseID: block.ID, ToolName: block.Name, ToolInput: input,
				})
				if !send(ctx, events, ToolCallEvent(ToolCall{
					ID: block.ID, Name: block.Name, Input: input,
				})) {
					deliverDone(ctx, events, DoneEvent(StopReasonCancelled, p.usage(&message)))
					return
				}
			}
		}

		done := DoneEvent(string(message.StopReason), p.usage(&message))
		done.Content = content
		if message.StopReason == anthropic.StopReasonRefusal {
			done.StopDetail = string(message.StopDetails.Category)
		}
		send(ctx, events, done)
	}()

	return events, nil
}

// requestOptions carries what the typed params of this SDK version do not: the
// thinking-binding beta and its field. See AnthropicOptions.AdaptiveThinking.
func (p *AnthropicProvider) requestOptions() []option.RequestOption {
	if !p.options.AdaptiveThinking {
		return nil
	}
	return []option.RequestOption{
		option.WithHeaderAdd("anthropic-beta", thinkingBindingBeta),
		option.WithJSONSet("thinking.block_binding.prefix_mismatch_behavior", "drop_block"),
	}
}

// reportTransformations logs what the API changed in the request before the
// model read it. Under the thinking-binding beta every response carries the list,
// empty when nothing was dropped; an entry means a thinking block did not reach
// the model — ODE's own history edit (prefix_binding_mismatch) or a model switch
// (model_binding_mismatch), which is expected.
func (p *AnthropicProvider) reportTransformations(ctx context.Context, start anthropic.MessageStartEvent) {
	raw := start.Message.JSON.ExtraFields["input_transformations"].Raw()
	if raw == "" || raw == "null" || raw == "[]" {
		return
	}
	slog.WarnContext(ctx, "the Anthropic API dropped thinking blocks before the model read them",
		"provider", p.name, "model", string(start.Message.Model), "input_transformations", raw)
}

// usage is what the accumulated message says the turn cost, priced.
//
// Read through one function because three exit paths need it now: the turn that
// finished, the turn that was cancelled and the turn that failed. All three were
// billed.
func (p *AnthropicProvider) usage(message *anthropic.Message) Usage {
	usage := Usage{
		InputTokens:       int(message.Usage.InputTokens),
		OutputTokens:      int(message.Usage.OutputTokens),
		CachedInputTokens: int(message.Usage.CacheReadInputTokens),
		CacheWriteTokens:  int(message.Usage.CacheCreationInputTokens),
		Provider:          p.name,
		Model:             string(message.Model),
	}
	p.pricing.Apply(&usage)
	return usage
}

func (p *AnthropicProvider) params(req Request) (anthropic.MessageNewParams, error) {
	model, err := ResolveModel(p, req.Model)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.options.MaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		Messages:  toAnthropicMessages(req.Messages),
	}
	markCachedPrefix(params.Messages)

	if req.System != "" {
		// The first of the three cache breakpoints, and the one that pays for itself
		// fastest. Tools render before system, so a mark here caches both — and tools
		// are the larger half: ODE offers the whole permitted surface with its
		// descriptions and JSON schemas on every call of the tool loop, which is up to
		// MaxIterations calls for one message.
		params.System = []anthropic.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}

	effort := req.Effort
	if effort == "" {
		effort = p.options.Effort
	}
	if effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(effort),
		}
	}

	if p.options.AdaptiveThinking {
		adaptive := anthropic.ThinkingConfigAdaptiveParam{
			Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}

	// req.Temperature is deliberately dropped.
	//
	// The current Anthropic models reject temperature, top_p and top_k with a 400.
	// Forwarding the field would turn "a caller set a sampling knob" into "this
	// provider is broken", and the interface exists so a caller need not know
	// which provider it is talking to. Steering here is by prompt and by effort.
	if req.Temperature != nil {
		slog.Debug("anthropic provider ignoring temperature: the current models reject it",
			"provider", p.name, "model", model)
	}

	for _, definition := range req.Tools {
		tool, err := toAnthropicTool(definition)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	markCachedTools(params.Tools)

	return params, nil
}

// markCachedTools puts a breakpoint on the last tool definition.
//
// The system mark below already covers tools, because tools render first — but it
// covers them only as far as the system text is unchanged. Within a session it no
// longer changes: the tier, split and selection it used to carry are notes in the
// conversation now (see chat.sessionStateNotice). Between providers it still does
// — a provider without tools gets other instructions — and tools are the larger
// half of the prefix: 43 definitions are about 16k tokens against roughly 4k of
// system text.
//
// A mark of their own splits the two, so whatever changes the system block leaves
// the tool block cached, across sessions and developers too, since the schemas
// depend on neither.
//
// This is the fourth breakpoint of the API's four, together with the system mark
// and the two in markCachedPrefix. There is no room for a fifth.
func markCachedTools(tools []anthropic.ToolUnionParam) {
	if len(tools) == 0 {
		return
	}
	last := tools[len(tools)-1].OfTool
	if last == nil {
		return
	}
	last.CacheControl = anthropic.NewCacheControlEphemeralParam()
}

// toAnthropicTool converts a definition, moving the JSON Schema across as the
// input schema.
//
// The schema arrives as raw JSON because that is what the tool registry holds and
// what the wire wants, so it is decoded only far enough to fill the SDK's typed
// properties field.
func toAnthropicTool(definition ToolDefinition) (anthropic.ToolParam, error) {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if len(definition.Schema) > 0 {
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			return anthropic.ToolParam{}, fmt.Errorf(
				"llm: tool %q has an unparseable schema: %w", definition.Name, err)
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}

	tool := anthropic.ToolParam{
		Name:        definition.Name,
		Description: anthropic.String(definition.Description),
		InputSchema: anthropic.ToolInputSchemaParam{Properties: schema.Properties},
	}
	if len(schema.Required) > 0 {
		tool.InputSchema.Required = schema.Required
	}
	return tool, nil
}

// messagesPerIteration is how many messages one pass of the chat engine's tool
// loop appends: the assistant's turn and the user turn carrying the tool results.
//
// Used only to guess where the previous request's breakpoint sat. A wrong guess
// wastes one of the four breakpoints and nothing else, so the constant does not
// have to be right for every shape of turn — a message that produced no tool call
// appends one message rather than two, and the mark then lands a turn further back
// than intended, which still reads.
const messagesPerIteration = 2

// markCachedPrefix places the conversation's cache breakpoints.
//
// Two of them, on the last block of the last message and on the last block of
// where the previous request ended. The first is what makes the next request
// cheap: the whole history behind it — every answer, every tool result, every
// document a tool read — is cached, and over a long session that is by far the
// largest thing ODE re-sends. The second is the read point for *this* request,
// written by the last one; the API also looks back on its own from an explicit
// breakpoint, so the second mark is belt and braces rather than load-bearing, and
// it costs nothing but one of four slots.
//
// The system block takes a third, leaving one spare.
//
// Deliberately five-minute entries, which is what an unqualified ephemeral mark
// means. A one-hour entry would survive the gaps this conversation actually has —
// a developer reading a confirmation card holds a tool call for up to
// chat_confirmation_timeout, and thinking time between messages is longer still —
// but it is charged at twice fresh input to write rather than a quarter more, so
// it only pays where the reads really do arrive late. That is a question for the
// run log to answer, not for this line to assume.
func markCachedPrefix(messages []anthropic.MessageParam) {
	mark := func(index int) {
		if index < 0 || index >= len(messages) {
			return
		}
		blocks := messages[index].Content
		if len(blocks) == 0 {
			return
		}
		// Nil for a block variant that carries no cache control. None of the three
		// ODE produces is one, and checking is cheaper than depending on that.
		if control := blocks[len(blocks)-1].GetCacheControl(); control != nil {
			*control = anthropic.NewCacheControlEphemeralParam()
		}
	}
	mark(len(messages) - 1)
	mark(len(messages) - 1 - messagesPerIteration)
}

func toAnthropicMessages(messages []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(messages))
	for _, message := range messages {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(message.Content))
		for _, content := range message.Content {
			switch content.Type {
			case ContentText:
				if content.Text == "" {
					continue
				}
				blocks = append(blocks, anthropic.NewTextBlock(content.Text))
			case ContentToolUse:
				// The bytes the model produced, not a decoded copy: a round trip through
				// any sorted the keys and ran every number through float64, so the
				// history showed the model arguments it never wrote, an integer above
				// 2^53 among them.
				input := json.RawMessage(`{}`)
				if len(content.ToolInput) > 0 && json.Valid(content.ToolInput) {
					input = content.ToolInput
				}
				blocks = append(blocks,
					anthropic.NewToolUseBlock(content.ToolUseID, input, content.ToolName))
			case ContentToolResult:
				blocks = append(blocks,
					anthropic.NewToolResultBlock(content.ToolUseID, content.ToolResult, content.IsError))
			case ContentThinking:
				// Sent back as it came, empty summary included: the signature is what
				// the API reads. An unsigned block is one a stream was cut off in, which
				// the API would refuse; the engine does not store those.
				if content.Signature == "" {
					continue
				}
				blocks = append(blocks, anthropic.NewThinkingBlock(content.Signature, content.Thinking))
			case ContentRedactedThinking:
				if content.Data == "" {
					continue
				}
				blocks = append(blocks, anthropic.NewRedactedThinkingBlock(content.Data))
			}
		}
		if len(blocks) == 0 {
			// A message with nothing in it is refused by the API, and it can arise
			// from a turn that produced only a tool call whose result was dropped.
			continue
		}
		if message.Role == RoleAssistant {
			out = append(out, anthropic.NewAssistantMessage(blocks...))
			continue
		}
		out = append(out, anthropic.NewUserMessage(blocks...))
	}
	return out
}

// send delivers an event unless the caller has gone away, and reports whether
// the stream should continue. Every provider uses it, so no provider can block
// forever on a channel nobody is reading.
func send(ctx context.Context, events chan<- Event, event Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// deliverDone hands over the closing done event of a turn that ended early.
//
// send would be wrong here. Its select has two ready cases the moment ctx is
// cancelled, and Go picks between ready cases at random — so the one event that
// must arrive on a cancelled turn would arrive about half the time. The tokens
// have been spent and §3.3's accounting reads them from this event, so it is
// offered to the channel first and only given up on if the buffer is full *and*
// the caller has gone, which is a caller that has stopped reading anyway. That
// keeps it from blocking a provider goroutine for ever.
func deliverDone(ctx context.Context, events chan<- Event, event Event) {
	select {
	case events <- event:
		return
	default:
	}
	select {
	case events <- event:
	case <-ctx.Done():
	}
}
