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
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
)

// A conversation with every block type in it: text, a tool call, and its result.
// This is the shape a tool loop produces, and it is what both conversions have to
// carry without loss.
func exchange() []Message {
	return []Message{
		UserText("which devices are there?"),
		{Role: RoleAssistant, Content: []Content{
			{Type: ContentText, Text: "Let me look."},
			{Type: ContentToolUse, ToolUseID: "call-1", ToolName: "list_devices",
				ToolInput: json.RawMessage(`{"limit":5}`)},
		}},
		{Role: RoleUser, Content: []Content{
			{Type: ContentToolResult, ToolUseID: "call-1", ToolName: "list_devices",
				ToolResult: `{"devices":[]}`},
		}},
		UserText("and now?"),
	}
}

// --- Anthropic conversion ---

func TestAnthropicConversionKeepsEveryBlock(t *testing.T) {
	converted := toAnthropicMessages(exchange())

	if len(converted) != 4 {
		t.Fatalf("messages = %d, want 4", len(converted))
	}
	if converted[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("message 0 role = %v, want user", converted[0].Role)
	}
	if converted[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("message 1 role = %v, want assistant", converted[1].Role)
	}
	// The assistant turn keeps its text and its tool call together, which is how
	// the protocol represents one turn that said something and then acted.
	if len(converted[1].Content) != 2 {
		t.Errorf("assistant blocks = %d, want text plus tool_use", len(converted[1].Content))
	}
	// A tool result is a user-role message in this protocol, which reads oddly and
	// is correct.
	if converted[2].Role != anthropic.MessageParamRoleUser {
		t.Errorf("the tool result is role %v, want user", converted[2].Role)
	}
}

// TestAnthropicConversionDropsEmptyMessages guards a 400: the API refuses a
// message with no content, and one can arise from a turn whose only tool result
// was dropped.
func TestAnthropicConversionDropsEmptyMessages(t *testing.T) {
	converted := toAnthropicMessages([]Message{
		UserText("real"),
		{Role: RoleAssistant, Content: []Content{{Type: ContentText, Text: ""}}},
		{Role: RoleUser, Content: []Content{}},
	})
	if len(converted) != 1 {
		t.Errorf("messages = %d, want 1: empty messages must be dropped, not sent", len(converted))
	}
}

// TestAnthropicDropsTemperature is the asymmetry the package comment describes.
// Current Anthropic models reject the field with a 400, so a caller that sets it
// must not be punished for talking to this provider.
func TestAnthropicDropsTemperature(t *testing.T) {
	provider, err := NewAnthropicProvider("anthropic", AnthropicOptions{
		APIKey: "test-key", Models: []string{"claude-opus-5"},
	}, nil)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}

	temperature := 0.7
	params, err := provider.params(Request{
		Messages: []Message{UserText("hi")}, Temperature: &temperature,
	})
	if err != nil {
		t.Fatalf("params: %v", err)
	}

	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"temperature", "top_p", "top_k"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the request carries %q, which current models reject with a 400: %s",
				forbidden, encoded)
		}
	}
}

func TestAnthropicEffortAndThinking(t *testing.T) {
	provider, err := NewAnthropicProvider("anthropic", AnthropicOptions{
		APIKey: "k", Models: []string{"claude-opus-5"},
		Effort: "high", AdaptiveThinking: true,
	}, nil)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}

	params, err := provider.params(Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	encoded, _ := json.Marshal(params)
	if !strings.Contains(string(encoded), `"effort":"high"`) {
		t.Errorf("effort not sent: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"adaptive"`) {
		t.Errorf("adaptive thinking not sent: %s", encoded)
	}

	// Off means no thinking configuration at all, which every model accepts.
	plain, err := NewAnthropicProvider("a2", AnthropicOptions{
		APIKey: "k", Models: []string{"claude-opus-5"},
	}, nil)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	params, _ = plain.params(Request{Messages: []Message{UserText("hi")}})
	encoded, _ = json.Marshal(params)
	if strings.Contains(string(encoded), "thinking") {
		t.Errorf("thinking configuration was sent when it was not asked for: %s", encoded)
	}
}

func TestAnthropicToolSchemaCarriesRequired(t *testing.T) {
	tool, err := toAnthropicTool(ToolDefinition{
		Name:        "profile_series",
		Description: "compute a profile",
		Schema: json.RawMessage(`{
			"type":"object",
			"properties":{"device_id":{"type":"string"},"service_id":{"type":"string"}},
			"required":["device_id","service_id"]
		}`),
	})
	if err != nil {
		t.Fatalf("toAnthropicTool: %v", err)
	}
	if len(tool.InputSchema.Properties.(map[string]any)) != 2 {
		t.Errorf("properties = %v, want two", tool.InputSchema.Properties)
	}
	if strings.Join(tool.InputSchema.Required, ",") != "device_id,service_id" {
		t.Errorf("required = %v, want both fields; a dropped required list lets the model omit them",
			tool.InputSchema.Required)
	}
}

func TestAnthropicToolRejectsBadSchema(t *testing.T) {
	if _, err := toAnthropicTool(ToolDefinition{
		Name: "t", Schema: json.RawMessage(`{"type":`),
	}); err == nil {
		t.Error("an unparseable schema was accepted")
	}
}

// --- OpenAI conversion ---

// TestOpenAIConversionReshapesTheConversation covers the three places the two
// protocols genuinely disagree, all of which have to be absorbed in one function.
func TestOpenAIConversionReshapesTheConversation(t *testing.T) {
	converted := toOpenAIMessages("you are helpful", exchange())

	encoded, err := json.Marshal(converted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 1. The system prompt is a message, not a field, and it comes first.
	if len(decoded) == 0 || decoded[0]["role"] != "system" {
		t.Fatalf("first message = %v, want the system prompt", decoded[0])
	}

	roles := []string{}
	for _, message := range decoded {
		roles = append(roles, message["role"].(string))
	}
	want := "system,user,assistant,tool,user"
	if strings.Join(roles, ",") != want {
		t.Errorf("roles = %v, want %s", roles, want)
	}

	// 2. The assistant's tool calls live on the message, not among its content.
	assistant := decoded[2]
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v, want one", assistant["tool_calls"])
	}
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["name"] != "list_devices" {
		t.Errorf("tool call name = %v", function["name"])
	}
	if function["arguments"] != `{"limit":5}` {
		t.Errorf("arguments = %v, want the raw JSON string this protocol expects",
			function["arguments"])
	}

	// 3. A tool result is its own role, carrying the call id.
	toolMessage := decoded[3]
	if toolMessage["tool_call_id"] != "call-1" {
		t.Errorf("tool_call_id = %v, want call-1", toolMessage["tool_call_id"])
	}
}

// TestOpenAIToolResultsPrecedeText is a protocol requirement: every tool call must
// be answered before anything else follows, and a user turn can carry both a
// result and new text.
func TestOpenAIToolResultsPrecedeText(t *testing.T) {
	converted := toOpenAIMessages("", []Message{
		{Role: RoleUser, Content: []Content{
			{Type: ContentText, Text: "and also this"},
			{Type: ContentToolResult, ToolUseID: "c1", ToolResult: `{}`},
		}},
	})

	encoded, _ := json.Marshal(converted)
	var decoded []map[string]any
	_ = json.Unmarshal(encoded, &decoded)

	if len(decoded) != 2 {
		t.Fatalf("messages = %d, want the tool result and the text separately", len(decoded))
	}
	if decoded[0]["role"] != "tool" {
		t.Errorf("first = %v, want the tool result first even though the text came first in the block list",
			decoded[0]["role"])
	}
}

func TestOpenAIOmitsEmptyAssistantTurns(t *testing.T) {
	converted := toOpenAIMessages("", []Message{
		{Role: RoleAssistant, Content: []Content{{Type: ContentText, Text: ""}}},
		UserText("hello"),
	})
	if len(converted) != 1 {
		t.Errorf("messages = %d, want the empty assistant turn dropped", len(converted))
	}
}

func TestOpenAICompatibleRequiresABaseURL(t *testing.T) {
	// Without one it would silently talk to the OpenAI API, which is the worst
	// possible outcome for a config meant to point at a local server.
	if _, err := NewOpenAICompatibleProvider("local", OpenAIOptions{}, nil); err == nil {
		t.Error("an OpenAI-compatible provider was built with no base URL")
	}
}

func TestOpenAICompatibleWithoutToolsIsDegraded(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider("local", OpenAIOptions{
		BaseURL: "http://localhost:8000/v1", Models: []string{"local-model"},
	}, nil)
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider: %v", err)
	}

	capabilities := provider.Capabilities()
	if capabilities.Tools {
		t.Error("tool support was assumed for a server that did not declare it")
	}
	if !capabilities.Degraded || capabilities.DegradedReason == "" {
		t.Error("a text-only provider must declare itself degraded, or its silence looks like success")
	}
}

func TestOpenAIProviderAlwaysDeclaresTools(t *testing.T) {
	provider, err := NewOpenAIProvider("openai", OpenAIOptions{
		APIKey: "k", Models: []string{"gpt-4o"},
	}, nil)
	if err != nil {
		t.Fatalf("NewOpenAIProvider: %v", err)
	}
	if !provider.Capabilities().Tools {
		t.Error("the OpenAI API supports function calling and should declare it")
	}
}

func TestOpenAIParamsIncludeUsage(t *testing.T) {
	provider, _ := NewOpenAIProvider("openai", OpenAIOptions{
		APIKey: "k", Models: []string{"gpt-4o"}, MaxTokens: 100,
	}, nil)
	params, err := provider.params(Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	encoded, _ := json.Marshal(params)
	// Without include_usage a streamed turn reports no tokens and §3.3's accounting
	// has nothing to record.
	if !strings.Contains(string(encoded), "include_usage") {
		t.Errorf("stream options do not request usage: %s", encoded)
	}
	if !strings.Contains(string(encoded), "max_completion_tokens") {
		t.Errorf("max tokens not sent: %s", encoded)
	}
}

func TestOpenAIToolsAreOmittedWhenUnsupported(t *testing.T) {
	provider, _ := NewOpenAICompatibleProvider("local", OpenAIOptions{
		BaseURL: "http://localhost:8000/v1", Models: []string{"m"}, Tools: false,
	}, nil)
	params, err := provider.params(Request{
		Messages: []Message{UserText("hi")},
		Tools: []ToolDefinition{{
			Name: "t", Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		}},
	})
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	if len(params.Tools) != 0 {
		t.Error("tools were sent to a server that does not implement function calling")
	}
}

var _ = openai.ChatCompletionNewParams{}

// --- the registry (the exit criterion's mechanism) ---

func TestRegistryResolvesByNameWithAFirstDefault(t *testing.T) {
	first := &stubProvider{name: "first", models: []string{"m1"}}
	second := &stubProvider{name: "second", models: []string{"m2"}}

	registry, err := NewRegistry(first, second)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if registry.Default() != "first" {
		t.Errorf("default = %q, want the first registered", registry.Default())
	}
	if provider, err := registry.Get(""); err != nil || provider.Name() != "first" {
		t.Errorf("empty name gave %v/%v, want the default", provider, err)
	}
	if provider, err := registry.Get("second"); err != nil || provider.Name() != "second" {
		t.Errorf("Get(second) gave %v/%v", provider, err)
	}

	// An unknown name is an error, not a silent fallback: a typo in a deployment's
	// configuration should be visible.
	if _, err := registry.Get("third"); !errors.Is(err, ErrNoSuchProvider) {
		t.Errorf("Get(third) gave %v, want ErrNoSuchProvider", err)
	}
	if got := strings.Join(registry.Names(), ","); got != "first,second" {
		t.Errorf("Names = %q, want registration order", got)
	}
}

func TestEmptyRegistryIsNotConfigured(t *testing.T) {
	registry, _ := NewRegistry()
	if _, err := registry.Get("anything"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("an empty registry gave %v, want ErrNotConfigured", err)
	}
}

func TestRegistryRejectsDuplicatesAndNils(t *testing.T) {
	registry, _ := NewRegistry()
	if err := registry.Register(nil); err == nil {
		t.Error("a nil provider was registered")
	}
	if err := registry.Register(&stubProvider{name: ""}); err == nil {
		t.Error("a provider with no name was registered")
	}
	if err := registry.Register(&stubProvider{name: "p"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(&stubProvider{name: "p"}); err == nil {
		t.Error("a duplicate name was registered")
	}
}

func TestResolveModel(t *testing.T) {
	provider := &stubProvider{name: "p", models: []string{"good", "better"}}

	if model, err := ResolveModel(provider, ""); err != nil || model != "good" {
		t.Errorf("default model = %q/%v, want the first declared", model, err)
	}
	if model, err := ResolveModel(provider, "better"); err != nil || model != "better" {
		t.Errorf("named model = %q/%v", model, err)
	}
	if _, err := ResolveModel(provider, "nope"); !errors.Is(err, ErrNoSuchModel) {
		t.Errorf("an undeclared model gave %v, want ErrNoSuchModel", err)
	}

	// An empty list means ODE holds no allow-list, which is normal for a local
	// server serving whatever it was started with.
	open := &stubProvider{name: "open"}
	if model, err := ResolveModel(open, "whatever"); err != nil || model != "whatever" {
		t.Errorf("open provider gave %q/%v, want the model through", model, err)
	}

	// Two different facts, and conflating them refused a legitimate request: a
	// provider that requires a model and has none configured is an error, whereas
	// one with its own default (the CLI) resolves to the empty string and chooses
	// for itself.
	needsModel := &stubProvider{name: "api", required: true}
	if _, err := ResolveModel(needsModel, ""); !errors.Is(err, ErrNoSuchModel) {
		t.Errorf("a provider needing a model gave %v, want ErrNoSuchModel", err)
	}
	if model, err := ResolveModel(open, ""); err != nil || model != "" {
		t.Errorf("a provider with its own default gave %q/%v, want the empty string", model, err)
	}
}

func TestDescribeReportsDegradation(t *testing.T) {
	degraded := &stubProvider{name: "cli", degraded: true}
	registry, _ := NewRegistry(&stubProvider{name: "api"}, degraded)

	described := registry.Describe()
	if len(described) != 2 {
		t.Fatalf("described %d providers, want 2", len(described))
	}
	if !described[0].Default {
		t.Error("the first provider is not marked as the default")
	}
	if !described[1].Capabilities.Degraded {
		t.Error("a degraded provider is not reported as such, so the UI cannot show it")
	}
}

type stubProvider struct {
	name     string
	models   []string
	degraded bool
	required bool
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Capabilities() Capabilities {
	return Capabilities{
		Tools: true, Streaming: true, Models: s.models,
		Degraded: s.degraded, ModelRequired: s.required,
	}
}
func (s *stubProvider) Stream(context.Context, Request) (<-chan Event, error) {
	out := make(chan Event)
	close(out)
	return out, nil
}

// --- usage accumulation ---

func TestUsageAddsAcrossTurns(t *testing.T) {
	total := Usage{}
	total.Add(Usage{InputTokens: 10, OutputTokens: 5, CostEUR: 0.1,
		Provider: "p", Model: "m", CostEstimated: true})
	total.Add(Usage{InputTokens: 3, OutputTokens: 2, CachedInputTokens: 7, CostEUR: 0.05,
		Provider: "p", Model: "m"})

	if total.InputTokens != 13 || total.OutputTokens != 7 || total.CachedInputTokens != 7 {
		t.Errorf("tokens = %+v, want 13/7/7", total)
	}
	if total.CostEUR != 0.15000000000000002 && total.CostEUR != 0.15 {
		t.Errorf("cost = %v, want 0.15", total.CostEUR)
	}
	if !total.CostEstimated {
		t.Error("an exchange containing an estimated turn must stay marked estimated")
	}
}

// --- CLI provider ---

func TestCLIProviderStartsUnusableUntilProbed(t *testing.T) {
	provider := NewAnthropicCLIProvider("cli", CLIOptions{Binary: "definitely-not-a-real-binary"}, nil)

	// Before probing it must not claim to work: an optimistic default would show a
	// developer a provider that fails on first use.
	if !provider.Capabilities().Degraded {
		t.Error("an unprobed CLI provider claims to be usable")
	}

	capabilities := provider.Probe(context.Background())
	if !capabilities.Degraded || capabilities.Tools {
		t.Errorf("probing a missing binary gave %+v, want degraded with no tools", capabilities)
	}
	if capabilities.DegradedReason == "" {
		t.Error("the degradation gives no reason, so an operator cannot fix it")
	}
}

func TestCLIDegradeDropsToTextOnly(t *testing.T) {
	provider := NewAnthropicCLIProvider("cli", CLIOptions{}, nil)
	provider.set(Capabilities{Tools: true, ToolsOutOfBand: true, Streaming: true})

	provider.Degrade("mcp invocation failed")

	capabilities := provider.Capabilities()
	if capabilities.Tools || capabilities.ToolsOutOfBand {
		t.Error("degrading did not remove tool support")
	}
	if capabilities.DegradedReason != "mcp invocation failed" {
		t.Errorf("reason = %q", capabilities.DegradedReason)
	}
}

func TestCLIPromptRendersASingleTurnFaithfully(t *testing.T) {
	// One user message is the faithful case and must not be wrapped in transcript
	// scaffolding.
	prompt := cliPrompt(Request{Messages: []Message{UserText("what is the power?")}})
	if prompt != "what is the power?" {
		t.Errorf("prompt = %q, want the message verbatim", prompt)
	}
}

func TestCLIPromptTranscribesHistory(t *testing.T) {
	prompt := cliPrompt(Request{Messages: []Message{
		UserText("first"),
		AssistantText("answer"),
		UserText("second"),
	}})
	for _, want := range []string{"first", "answer", "second", "current message"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestCLIMCPConfigCarriesAuthAndSession(t *testing.T) {
	config, err := mcpConfig(&ToolEndpoint{
		URL: "https://ode.example.org/mcp", Token: "Bearer abc", SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("mcpConfig: %v", err)
	}

	var decoded struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(config), &decoded); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}

	server, found := decoded.MCPServers[MCPServerName]
	if !found {
		t.Fatalf("no %q server in the config: %s", MCPServerName, config)
	}
	if server.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("Authorization = %q; the token must reach the MCP server or every tool is unauthorised",
			server.Headers["Authorization"])
	}
	if server.Headers[SessionHeader] != "sess-1" {
		t.Errorf("%s = %q; without it there is no tier to enforce against",
			SessionHeader, server.Headers[SessionHeader])
	}
	// A doubled prefix would produce "Bearer Bearer abc".
	if strings.Contains(server.Headers["Authorization"], "Bearer Bearer") {
		t.Error("the bearer prefix was doubled")
	}
}

func TestCLIToolPrefixRoundTrips(t *testing.T) {
	prefixedNames := prefixed([]string{"list_devices", "quick_profile"})
	for _, name := range prefixedNames {
		if !strings.HasPrefix(name, "mcp__"+MCPServerName+"__") {
			t.Errorf("%q is not namespaced for the CLI", name)
		}
	}

	if got, isODE := unprefix(prefixedNames[0]); got != "list_devices" || !isODE {
		t.Errorf("unprefix gave %q/%v, want list_devices/true", got, isODE)
	}
	// The CLI's own tools are shown under their own names rather than pretending to
	// be ODE's.
	if got, isODE := unprefix("Bash"); got != "Bash" || isODE {
		t.Errorf("unprefix(Bash) gave %q/%v, want Bash/false", got, isODE)
	}
}

// --- events ---

func TestEventConstructorsSetTheirFields(t *testing.T) {
	if event := TextEvent("hi"); event.Type != EventTextDelta || event.Text != "hi" {
		t.Errorf("TextEvent = %+v", event)
	}
	if event := ToolCallEvent(ToolCall{ID: "1"}); event.Type != EventToolCall || event.ToolCall == nil {
		t.Errorf("ToolCallEvent = %+v", event)
	}
	if event := DoneEvent("end_turn", Usage{}); event.Type != EventDone || event.Usage == nil {
		t.Errorf("DoneEvent = %+v", event)
	}
	if event := ErrorEvent(errors.New("boom")); event.Type != EventError || event.Error != "boom" {
		t.Errorf("ErrorEvent = %+v", event)
	}
	// A nil error still has to produce something the SPA can display.
	if event := ErrorEvent(nil); event.Error == "" {
		t.Error("ErrorEvent(nil) produced an empty message")
	}
}
