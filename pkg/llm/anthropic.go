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
	// AdaptiveThinking sends thinking: {type: "adaptive"}.
	//
	// Worth setting deliberately rather than defaulting on: on the current models
	// it is either the default already or the recommended mode, but a request that
	// names an older model and sends it is refused. Off means "send no thinking
	// configuration at all", which every model accepts.
	AdaptiveThinking bool
}

const (
	defaultMaxTokens = 8192
	// defaultAnthropicModel is the current Opus. Named as a plain string because
	// anthropic.Model is a string alias and the SDK carries no constant for it.
	defaultAnthropicModel = "claude-opus-5"
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

		stream := p.client.Messages.NewStreaming(ctx, params)
		// Accumulate rebuilds the complete message from the deltas, which is what
		// makes tool arguments usable: both this API and OpenAI's stream a tool's
		// JSON in fragments, and a half-decoded object cannot be dispatched.
		message := anthropic.Message{}

		for stream.Next() {
			event := stream.Current()
			if err := message.Accumulate(event); err != nil {
				send(ctx, events, ErrorEvent(fmt.Errorf("llm: %s: accumulate: %w", p.name, err)))
				return
			}

			// Only text is forwarded live. A tool call is forwarded once whole,
			// below, because a partial one is not actionable.
			if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
				if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok && text.Text != "" {
					if !send(ctx, events, TextEvent(text.Text)) {
						return
					}
				}
			}
		}

		if err := stream.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				// A cancelled turn is the developer stopping it, not a failure.
				slog.DebugContext(ctx, "anthropic stream cancelled", "provider", p.name)
				return
			}
			send(ctx, events, ErrorEvent(fmt.Errorf("llm: %s: %w", p.name, err)))
			return
		}

		for _, block := range message.Content {
			if toolUse, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
				input := json.RawMessage(toolUse.Input)
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				if !send(ctx, events, ToolCallEvent(ToolCall{
					ID: toolUse.ID, Name: toolUse.Name, Input: input,
				})) {
					return
				}
			}
		}

		usage := Usage{
			InputTokens:       int(message.Usage.InputTokens),
			OutputTokens:      int(message.Usage.OutputTokens),
			CachedInputTokens: int(message.Usage.CacheReadInputTokens),
			Provider:          p.name,
			Model:             string(message.Model),
		}
		p.pricing.Apply(&usage)
		send(ctx, events, DoneEvent(string(message.StopReason), usage))
	}()

	return events, nil
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

	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
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
		adaptive := anthropic.ThinkingConfigAdaptiveParam{}
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

	return params, nil
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
				var input any = map[string]any{}
				if len(content.ToolInput) > 0 {
					_ = json.Unmarshal(content.ToolInput, &input)
				}
				blocks = append(blocks,
					anthropic.NewToolUseBlock(content.ToolUseID, input, content.ToolName))
			case ContentToolResult:
				blocks = append(blocks,
					anthropic.NewToolResultBlock(content.ToolUseID, content.ToolResult, content.IsError))
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
