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

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAIProvider covers rows two and three of §5.7's table: the OpenAI API, and
// any OpenAI-compatible server (vLLM, Ollama, Azure).
//
// One implementation for both, because the difference between the rows is a base
// URL and a capability declaration, not a protocol. A second type would be the
// same four hundred lines with one constant changed, and the two would drift.
// NewOpenAIProvider and NewOpenAICompatibleProvider are the two entry points, and
// the compatible one is where tool support is gated: a local server may or may not
// implement function calling, and §5.7 requires that be declared rather than
// assumed.
type OpenAIProvider struct {
	name    string
	client  openai.Client
	options OpenAIOptions
	pricing *Pricing
}

type OpenAIOptions struct {
	APIKey  string
	BaseURL string
	Models  []string
	// MaxTokens becomes max_completion_tokens. Zero omits it and lets the server
	// decide, which is what a local server usually wants.
	MaxTokens int
	// Tools declares whether this endpoint implements function calling. True for
	// the OpenAI API; for a compatible server it is the deployment's claim about
	// what it is running.
	Tools bool
	// Temperature is the deployment's default when a request names none.
	Temperature *float64
}

// NewOpenAIProvider is the OpenAI API proper: an API key, native function
// calling, and the default base URL.
func NewOpenAIProvider(name string, opts OpenAIOptions, pricing *Pricing) (*OpenAIProvider, error) {
	if name == "" {
		name = "openai"
	}
	if opts.APIKey == "" {
		return nil, fmt.Errorf("llm: provider %q needs an API key", name)
	}
	opts.Tools = true
	return newOpenAI(name, opts, pricing)
}

// NewOpenAICompatibleProvider is a local or third-party server speaking the same
// protocol. The API key is optional, because a local vLLM or Ollama usually has
// none, and tool support is whatever the deployment declares.
func NewOpenAICompatibleProvider(name string, opts OpenAIOptions, pricing *Pricing) (*OpenAIProvider, error) {
	if name == "" {
		name = "openai-compatible"
	}
	if opts.BaseURL == "" {
		return nil, fmt.Errorf(
			"llm: provider %q needs a base URL; an OpenAI-compatible provider without one "+
				"would silently talk to the OpenAI API", name)
	}
	return newOpenAI(name, opts, pricing)
}

func newOpenAI(name string, opts OpenAIOptions, pricing *Pricing) (*OpenAIProvider, error) {
	requestOptions := []option.RequestOption{}
	if opts.APIKey != "" {
		requestOptions = append(requestOptions, option.WithAPIKey(opts.APIKey))
	}
	if opts.BaseURL != "" {
		requestOptions = append(requestOptions, option.WithBaseURL(opts.BaseURL))
	}
	return &OpenAIProvider{
		name:    name,
		client:  openai.NewClient(requestOptions...),
		options: opts,
		pricing: pricing,
	}, nil
}

func (p *OpenAIProvider) Name() string { return p.name }

func (p *OpenAIProvider) Capabilities() Capabilities {
	capabilities := Capabilities{
		Tools:     p.options.Tools,
		Streaming: true,
		System:    true,
		MaxTokens: p.options.MaxTokens,
		Models:    append([]string{}, p.options.Models...),
		// The chat-completions API rejects a request with no model.
		ModelRequired: true,
	}
	if !p.options.Tools {
		// §5.7's degraded mode, declared rather than discovered: the chat engine
		// offers no tools and says why, instead of offering tools that never fire.
		capabilities.Degraded = true
		capabilities.DegradedReason = "this endpoint is not configured as supporting function calling, " +
			"so the assistant runs in text-only advisory mode"
	}
	return capabilities
}

func (p *OpenAIProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	params, err := p.params(req)
	if err != nil {
		return nil, err
	}

	events := make(chan Event, 16)
	go func() {
		defer close(events)

		stream := p.client.Chat.Completions.NewStreaming(ctx, params)
		accumulator := openai.ChatCompletionAccumulator{}

		for stream.Next() {
			chunk := stream.Current()
			accumulator.AddChunk(chunk)

			// A finished tool call is emitted the moment the accumulator has one
			// whole, which is what JustFinishedToolCall exists for. Arguments arrive
			// as string fragments, so this is the earliest point the call is
			// dispatchable.
			if call, ok := accumulator.JustFinishedToolCall(); ok {
				arguments := json.RawMessage(call.Arguments)
				if len(arguments) == 0 || !json.Valid(arguments) {
					// A local server that streams malformed JSON would otherwise
					// produce an unmarshal failure inside the executor, attributed to
					// the tool rather than the provider.
					arguments = json.RawMessage(`{}`)
					slog.WarnContext(ctx, "openai provider: tool arguments were not valid JSON",
						"provider", p.name, "tool", call.Name, "raw", call.Arguments)
				}
				if !send(ctx, events, ToolCallEvent(ToolCall{
					ID: call.ID, Name: call.Name, Input: arguments,
				})) {
					return
				}
				continue
			}

			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				if !send(ctx, events, TextEvent(chunk.Choices[0].Delta.Content)) {
					return
				}
			}
		}

		if err := stream.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				slog.DebugContext(ctx, "openai stream cancelled", "provider", p.name)
				return
			}
			send(ctx, events, ErrorEvent(fmt.Errorf("llm: %s: %w", p.name, err)))
			return
		}

		stopReason := ""
		if len(accumulator.Choices) > 0 {
			stopReason = accumulator.Choices[0].FinishReason
		}
		usage := Usage{
			InputTokens:  int(accumulator.Usage.PromptTokens),
			OutputTokens: int(accumulator.Usage.CompletionTokens),
			CachedInputTokens: int(
				accumulator.Usage.PromptTokensDetails.CachedTokens),
			Provider: p.name,
			Model:    accumulator.Model,
		}
		if usage.Model == "" {
			usage.Model = string(params.Model)
		}
		// Cached tokens are reported inside the prompt total here, unlike the
		// Anthropic API where they are separate. Subtracting keeps Usage's meaning
		// the same across providers, which is the point of normalising at all.
		if usage.CachedInputTokens > 0 && usage.InputTokens >= usage.CachedInputTokens {
			usage.InputTokens -= usage.CachedInputTokens
		}
		p.pricing.Apply(&usage)
		send(ctx, events, DoneEvent(stopReason, usage))
	}()

	return events, nil
}

func (p *OpenAIProvider) params(req Request) (openai.ChatCompletionNewParams, error) {
	model, err := ResolveModel(p, req.Model)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}

	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(model),
		Messages: toOpenAIMessages(req.System, req.Messages),
		// Without this the final chunk carries no usage, and §3.3's accounting
		// would have nothing to record for a streamed turn.
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.options.MaxTokens
	}
	if maxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	temperature := req.Temperature
	if temperature == nil {
		temperature = p.options.Temperature
	}
	if temperature != nil {
		params.Temperature = openai.Float(*temperature)
	}

	// req.Effort is not mapped. OpenAI's reasoning-effort parameter is a different
	// control on a different set of models, and guessing an equivalence would put
	// one provider's knob behind another's name.
	if req.Effort != "" {
		slog.Debug("openai provider ignoring effort: no equivalent control",
			"provider", p.name, "effort", req.Effort)
	}

	if p.options.Tools {
		for _, definition := range req.Tools {
			var parameters map[string]any
			if len(definition.Schema) > 0 {
				if err := json.Unmarshal(definition.Schema, &parameters); err != nil {
					return openai.ChatCompletionNewParams{}, fmt.Errorf(
						"llm: tool %q has an unparseable schema: %w", definition.Name, err)
				}
			}
			params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(
				shared.FunctionDefinitionParam{
					Name:        definition.Name,
					Description: openai.String(definition.Description),
					Parameters:  parameters,
				}))
		}
	}

	return params, nil
}

// toOpenAIMessages flattens ODE's block-structured messages onto the chat
// protocol.
//
// Three shape differences have to be absorbed here, and each is a place the two
// protocols genuinely disagree rather than a translation nicety:
//
//   - the system prompt is a message, not a field;
//   - an assistant turn's tool calls live on the message, not among its content;
//   - a tool result is its own role, not a block inside a user message.
func toOpenAIMessages(system string, messages []Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	if system != "" {
		out = append(out, openai.SystemMessage(system))
	}

	for _, message := range messages {
		if message.Role == RoleAssistant {
			assistant := openai.ChatCompletionAssistantMessageParam{}
			text := ""
			for _, content := range message.Content {
				switch content.Type {
				case ContentText:
					text += content.Text
				case ContentToolUse:
					arguments := string(content.ToolInput)
					if arguments == "" {
						arguments = "{}"
					}
					assistant.ToolCalls = append(assistant.ToolCalls,
						openai.ChatCompletionMessageToolCallUnionParam{
							OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
								ID: content.ToolUseID,
								Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
									Name:      content.ToolName,
									Arguments: arguments,
								},
							},
						})
				}
			}
			if text != "" {
				assistant.Content.OfString = openai.String(text)
			}
			if text == "" && len(assistant.ToolCalls) == 0 {
				continue
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
			continue
		}

		// A user turn may carry tool results, plain text, or both. Tool results
		// become their own messages and must precede the text, because the protocol
		// requires every tool call to be answered before anything else follows.
		text := ""
		for _, content := range message.Content {
			switch content.Type {
			case ContentToolResult:
				out = append(out, openai.ToolMessage(content.ToolResult, content.ToolUseID))
			case ContentText:
				text += content.Text
			}
		}
		if text != "" {
			out = append(out, openai.UserMessage(text))
		}
	}

	return out
}
