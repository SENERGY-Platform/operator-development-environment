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

// Package llm is the provider abstraction of SPEC §5.7 (D7).
//
// One interface, one event stream, four transports. The point of the
// indirection is stated as an exit criterion for M3 — "provider swap requires no
// call-site change" — so the interface is deliberately narrow: Stream and
// Capabilities, nothing else. Everything a caller might otherwise reach for on a
// concrete provider (a model list, a token counter, a provider-specific option)
// either belongs in Capabilities or does not belong in the abstraction.
//
//	AnthropicProvider         anthropics/anthropic-sdk-go   native tool use
//	OpenAIProvider            openai/openai-go              native function calling
//	OpenAICompatibleProvider  openai/openai-go, other base  native, capability-gated
//	AnthropicCLIProvider      os/exec over the claude CLI   MCP
//
// The one asymmetry worth knowing about is sampling. Current Anthropic models
// reject temperature, top_p and top_k with a 400, while OpenAI-compatible servers
// expect them. Request therefore carries Temperature as an optional pointer, and
// the Anthropic provider drops it rather than passing it through — see
// anthropic.go. A caller that sets it is not punished for talking to the wrong
// provider, which is the whole promise of a swappable interface.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Role is who produced a message. There is deliberately no system role: the
// system prompt is a field on Request, because two of the four transports treat
// it as one and a role would have to be flattened per provider anyway.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ContentType is one block of a message.
type ContentType string

const (
	ContentText       ContentType = "text"
	ContentToolUse    ContentType = "tool_use"
	ContentToolResult ContentType = "tool_result"
)

// Content is one block. A message is a list of these because a single assistant
// turn routinely contains text and several tool calls, and both native protocols
// represent that as a list.
type Content struct {
	Type ContentType `json:"type"`

	Text string `json:"text,omitempty"`

	// Tool use, on an assistant message.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`

	// Tool result, on a user message. Both protocols carry a tool result as a
	// user-role message, which reads oddly and is correct.
	ToolResult string `json:"tool_result,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
}

type Message struct {
	Role    Role      `json:"role"`
	Content []Content `json:"content"`
}

// UserText and AssistantText build the common single-block cases.
func UserText(text string) Message {
	return Message{Role: RoleUser, Content: []Content{{Type: ContentText, Text: text}}}
}

func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Type: ContentText, Text: text}}}
}

// ToolDefinition is a tool as offered to the model. It is the provider-facing
// projection of tools.Definition: name, description and schema, with none of
// ODE's tier or confirmation metadata, because those are enforcement concerns
// and the model has no business reading them.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Request is one provider call.
type Request struct {
	Model  string
	System string
	// Messages is the conversation so far, oldest first.
	Messages []Message
	Tools    []ToolDefinition
	// MaxTokens bounds the response. Required by the Anthropic API and defaulted
	// by the providers when zero, because a forgotten bound is a 400 rather than
	// an unbounded answer.
	MaxTokens int

	// Effort maps to Anthropic's output_config.effort (low, medium, high, xhigh,
	// max). Ignored by providers that have no equivalent rather than emulated,
	// since a fabricated mapping onto temperature would be a different knob
	// wearing this one's name.
	Effort string

	// Temperature is for OpenAI-compatible servers. It is a pointer because
	// "unset" and "zero" are different requests, and because current Anthropic
	// models reject the field outright — see the package comment.
	Temperature *float64

	// ToolEndpoint is set only for a provider whose Capabilities report
	// ToolsOutOfBand: the CLI provider, which reaches ODE's tools over MCP rather
	// than handing calls back through the engine. Every other provider ignores it.
	//
	// It is on Request rather than on the provider because the endpoint carries the
	// caller's token, which is per request and must never be captured at
	// construction (§3.1 step 3).
	ToolEndpoint *ToolEndpoint
}

// ToolEndpoint is how an out-of-band provider reaches ODE's own tool surface.
type ToolEndpoint struct {
	// URL is ODE's MCP endpoint.
	URL string
	// Token is the developer's access token, forwarded so the MCP server
	// authorises exactly as the REST surface does.
	Token string
	// SessionID scopes the MCP calls to one chat session, which is what carries
	// the exposure tier the dispatcher enforces against.
	SessionID string
	// AllowedTools is the tool surface the session's tier permits. The CLI is told
	// explicitly, because it decides what to call and a name it never sees is a
	// refusal that never happens.
	AllowedTools []string
}

// Capabilities is what a provider can actually do, probed or declared. §5.7
// requires probing at startup and degrading to text-only advisory mode when tool
// invocation fails, which is only expressible if this is data rather than an
// assumption.
type Capabilities struct {
	// Tools is false for a provider that cannot invoke tools. The chat engine
	// then offers none and tells the model so, rather than offering tools that
	// silently never fire.
	Tools bool `json:"tools"`
	// Streaming is false for a provider that can only answer in one piece. The
	// turn still produces the same event stream; it just arrives at once.
	Streaming bool `json:"streaming"`
	// System is false where a provider has no system prompt and it must be
	// prepended to the first user message instead.
	System bool `json:"system"`
	// MaxTokens is the provider's ceiling, or zero when it declares none.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Models is what the deployment permits, first entry being the default.
	// Empty means ODE holds no allow-list, which is normal for an OpenAI-compatible
	// server serving whatever it was started with.
	Models []string `json:"models,omitempty"`

	// ModelRequired says the provider cannot answer without a named model.
	//
	// Separate from an empty Models list, because the two are different facts and
	// conflating them refuses a legitimate request: "ODE has no allow-list" is not
	// "the provider has no default". The HTTP APIs require a model in the request
	// body; the CLI has its own default and is handed no --model flag at all.
	ModelRequired bool `json:"model_required,omitempty"`

	// ToolsOutOfBand marks a provider that invokes ODE's tools itself rather than
	// returning tool calls for the engine to dispatch. The CLI provider does this
	// over MCP (§5.7), and the difference is not cosmetic: the engine must not run
	// its own tool loop for such a provider, or every call happens twice.
	//
	// The tier gate is unaffected — the MCP server shares the one Dispatcher — but
	// which process runs the loop changes, so it has to be declared.
	ToolsOutOfBand bool `json:"tools_out_of_band,omitempty"`

	// Degraded and DegradedReason record a provider that came up in a reduced
	// mode. §5.7's CLI risk is exactly this case, and it must be visible in the
	// UI rather than inferred from tools never being called.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// Provider is §5.7's interface.
//
// Stream returns a channel the caller reads to completion. The provider closes
// it, and sends an EventError before closing when a turn fails. Cancelling ctx
// stops the underlying request and closes the channel.
type Provider interface {
	// Name is the configured provider name, which is what a session stores and
	// what the registry resolves. Not a display name.
	Name() string
	Capabilities() Capabilities
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}

var (
	ErrNoSuchProvider = errors.New("llm: no such provider")
	ErrNoSuchModel    = errors.New("llm: model not permitted for this provider")
	ErrNotConfigured  = errors.New("llm: no provider is configured")
)

// Registry resolves a provider by name.
//
// This is the indirection the exit criterion names. The chat engine holds a
// registry and a session holds a provider name; nothing above this package
// mentions Anthropic or OpenAI, so adding a fifth transport is a registration
// rather than an edit at the call sites.
type Registry struct {
	providers map[string]Provider
	order     []string
	// defaultName is the first provider registered, which is the one a session
	// gets when it names none.
	defaultName string
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{providers: map[string]Provider{}}
	for _, provider := range providers {
		if err := r.Register(provider); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) Register(provider Provider) error {
	if provider == nil {
		return errors.New("llm: cannot register a nil provider")
	}
	name := provider.Name()
	if name == "" {
		return errors.New("llm: cannot register a provider with no name")
	}
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("llm: provider %q is already registered", name)
	}
	r.providers[name] = provider
	r.order = append(r.order, name)
	if r.defaultName == "" {
		r.defaultName = name
	}
	return nil
}

// Get resolves a provider. An empty name means the default.
func (r *Registry) Get(name string) (Provider, error) {
	if len(r.providers) == 0 {
		return nil, ErrNotConfigured
	}
	if name == "" {
		name = r.defaultName
	}
	provider, found := r.providers[name]
	if !found {
		return nil, fmt.Errorf("%w: %q; configured: %s",
			ErrNoSuchProvider, name, strings.Join(r.Names(), ", "))
	}
	return provider, nil
}

// Names lists the registered providers in registration order, which is also
// preference order: the first is the default.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

func (r *Registry) Default() string { return r.defaultName }

func (r *Registry) Len() int { return len(r.providers) }

// Describe is the settings and session surface: every provider with its
// capabilities, so the SPA can show which are usable and which came up degraded.
func (r *Registry) Describe() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(r.order))
	for _, name := range r.order {
		provider := r.providers[name]
		out = append(out, ProviderInfo{
			Name:         name,
			Capabilities: provider.Capabilities(),
			Default:      name == r.defaultName,
		})
	}
	return out
}

type ProviderInfo struct {
	Name         string       `json:"name"`
	Capabilities Capabilities `json:"capabilities"`
	Default      bool         `json:"default"`
}

// ResolveModel checks a model against a provider's declared list and returns the
// default when none was asked for.
//
// An empty Models list means ODE holds no allow-list, which is right for a local
// OpenAI-compatible server: it serves whatever it was started with, and ODE has no
// list to check against. Admin limits narrow this further, per user (§3.3).
//
// An empty result is legitimate for a provider that does not require a model — the
// CLI — and means "let the provider choose".
func ResolveModel(provider Provider, model string) (string, error) {
	capabilities := provider.Capabilities()
	models := capabilities.Models
	if model == "" {
		if len(models) == 0 {
			if capabilities.ModelRequired {
				return "", fmt.Errorf(
					"%w: provider %q needs a model to be named, and none is configured",
					ErrNoSuchModel, provider.Name())
			}
			// The provider has its own default; nothing to resolve.
			return "", nil
		}
		return models[0], nil
	}
	if len(models) == 0 {
		return model, nil
	}
	for _, permitted := range models {
		if permitted == model {
			return model, nil
		}
	}
	sorted := append([]string{}, models...)
	sort.Strings(sorted)
	return "", fmt.Errorf("%w: %q on provider %q; permitted: %s",
		ErrNoSuchModel, model, provider.Name(), strings.Join(sorted, ", "))
}
