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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// AnthropicCLIProvider is §5.7's fourth row: the local `claude` CLI, wrapped with
// os/exec, reaching ODE's tools over MCP.
//
// The reason for MCP rather than tool-use plumbing is in the spec: the CLI runs
// its own agent loop, and fighting it to get tool calls back out would mean
// re-implementing that loop. Pointing it at ODE's MCP server instead means the
// same tool definitions and the same Dispatcher serve both transports, so the
// tier gate holds without a second enforcement path.
//
// The consequence is declared in Capabilities as ToolsOutOfBand: the engine must
// not run its own tool loop here. Tool calls still appear on the event stream,
// because the developer needs to see what the assistant did, but they are a
// report of what the CLI already ran rather than a request to run something.
//
// §5.7 calls the CLI's tool-calling parity unverified and says it must not block
// anything: it is a development convenience for working without an API key, not a
// production path. Probe() therefore never fails a startup — it degrades.
type AnthropicCLIProvider struct {
	name    string
	options CLIOptions
	pricing *Pricing

	mux          sync.RWMutex
	capabilities Capabilities
}

type CLIOptions struct {
	// Binary is the executable to run. Empty means "claude" on PATH.
	Binary string
	Models []string
	// Timeout bounds one turn. A CLI turn runs a whole agent loop, so this is
	// generous by nature; zero means defaultCLITimeout.
	Timeout time.Duration
	// ProbeTimeout bounds the startup capability probe.
	ProbeTimeout time.Duration
}

const (
	defaultCLITimeout      = 10 * time.Minute
	defaultCLIProbeTimeout = 15 * time.Second
)

func NewAnthropicCLIProvider(name string, opts CLIOptions, pricing *Pricing) *AnthropicCLIProvider {
	if name == "" {
		name = "claude-cli"
	}
	if opts.Binary == "" {
		opts.Binary = "claude"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultCLITimeout
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = defaultCLIProbeTimeout
	}
	return &AnthropicCLIProvider{
		name:    name,
		options: opts,
		pricing: pricing,
		// Until Probe runs, the provider declares itself unusable rather than
		// capable. An optimistic default would show a developer a provider that
		// fails on first use.
		capabilities: Capabilities{
			Degraded:       true,
			DegradedReason: "not yet probed",
			Models:         append([]string{}, opts.Models...),
		},
	}
}

func (p *AnthropicCLIProvider) Name() string { return p.name }

func (p *AnthropicCLIProvider) Capabilities() Capabilities {
	p.mux.RLock()
	defer p.mux.RUnlock()
	return p.capabilities
}

// Probe establishes what the local CLI can do, at startup (§5.7: "Probe
// capabilities at startup; degrade to text-only advisory mode if MCP invocation
// fails").
//
// It never returns an error. A missing or broken CLI is a configuration fact to
// report in the UI, not a reason to refuse to start ODE — the other providers are
// unaffected, and this one is explicitly not a production path.
func (p *AnthropicCLIProvider) Probe(ctx context.Context) Capabilities {
	capabilities := Capabilities{
		Streaming: true,
		System:    true,
		Models:    append([]string{}, p.options.Models...),
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.options.ProbeTimeout)
	defer cancel()

	version, err := exec.CommandContext(probeCtx, p.options.Binary, "--version").Output()
	if err != nil {
		capabilities.Degraded = true
		capabilities.DegradedReason = fmt.Sprintf(
			"the %s binary could not be run (%s); this provider is unavailable",
			p.options.Binary, err.Error())
		p.set(capabilities)
		slog.WarnContext(ctx, "claude CLI provider unavailable",
			"provider", p.name, "binary", p.options.Binary, "error", err)
		return capabilities
	}

	// The CLI is present. Whether it will actually invoke MCP tools is what §5.7
	// calls unverified, and it cannot be established without a live session, so it
	// is asserted here and corrected by the first turn that proves otherwise.
	capabilities.Tools = true
	capabilities.ToolsOutOfBand = true
	p.set(capabilities)

	slog.InfoContext(ctx, "claude CLI provider probed",
		"provider", p.name, "version", strings.TrimSpace(string(version)))
	return capabilities
}

// Degrade records that MCP invocation did not work, dropping the provider to
// text-only advisory mode. Called by the engine when a turn shows the CLI is not
// reaching ODE's tools.
func (p *AnthropicCLIProvider) Degrade(reason string) {
	p.mux.Lock()
	defer p.mux.Unlock()
	p.capabilities.Tools = false
	p.capabilities.ToolsOutOfBand = false
	p.capabilities.Degraded = true
	p.capabilities.DegradedReason = reason
}

func (p *AnthropicCLIProvider) set(capabilities Capabilities) {
	p.mux.Lock()
	defer p.mux.Unlock()
	p.capabilities = capabilities
}

func (p *AnthropicCLIProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if p.Capabilities().Degraded && !p.Capabilities().Streaming {
		return nil, fmt.Errorf("llm: %s is unavailable: %s",
			p.name, p.Capabilities().DegradedReason)
	}

	prompt := cliPrompt(req)
	if prompt == "" {
		return nil, fmt.Errorf("llm: %s: nothing to send", p.name)
	}

	args := []string{"--print", "--output-format", "stream-json", "--verbose"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	} else if len(p.options.Models) > 0 {
		args = append(args, "--model", p.options.Models[0])
	}
	if req.System != "" {
		args = append(args, "--append-system-prompt", req.System)
	}

	// The MCP wiring. Without an endpoint the CLI still answers, with no access to
	// the platform — which is the text-only advisory mode §5.7 describes.
	//
	// The configuration goes to a file and the file's path onto the command line.
	// The inline form the flag also accepts would put the developer's access token
	// in argv — see writeMCPConfig.
	cleanup := func() {}
	if endpoint := req.ToolEndpoint; endpoint != nil && endpoint.URL != "" && p.Capabilities().Tools {
		path, remove, err := writeMCPConfig(endpoint)
		if err != nil {
			return nil, err
		}
		cleanup = remove
		args = append(args, "--mcp-config", path, "--strict-mcp-config")
		if len(endpoint.AllowedTools) > 0 {
			args = append(args, "--allowedTools", strings.Join(prefixed(endpoint.AllowedTools), ","))
		}
	}

	turnCtx, cancel := context.WithTimeout(ctx, p.options.Timeout)

	command := exec.CommandContext(turnCtx, p.options.Binary, args...)
	command.Stdin = strings.NewReader(prompt)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		cleanup()
		return nil, fmt.Errorf("llm: %s: stdout: %w", p.name, err)
	}
	stderr := &strings.Builder{}
	command.Stderr = stderr
	// Bounds Wait. Stderr is not an *os.File, so os/exec pipes it and copies it on a
	// goroutine, and Wait waits for that copy to finish. A grandchild that inherited
	// the write end keeps it open after the CLI itself is killed — and the CLI runs
	// exactly such things, a bash tool and stdio MCP servers among them. Without a
	// delay Wait would then block for ever, and since the event channel is closed
	// after it, the whole exchange would hang on a session that could never be used
	// again. Five seconds is long enough for an ordinary exit and short enough that
	// a stuck turn ends.
	command.WaitDelay = 5 * time.Second

	if err := command.Start(); err != nil {
		cancel()
		cleanup()
		return nil, fmt.Errorf("llm: %s: could not start %s: %w", p.name, p.options.Binary, err)
	}

	events := make(chan Event, 16)
	go func() {
		// The process is waited for exactly once, on whichever path leaves this
		// goroutine. cancel() signals it; only Wait reaps it, closes the stdout pipe
		// and releases the two goroutines os/exec keeps per running command — so a
		// turn that ended before the CLI did used to leave a zombie, a file
		// descriptor and those goroutines behind, once per turn and with no recovery
		// short of restarting ODE.
		//
		// A plain flag rather than a sync.Once: the body and its deferred calls all
		// run on this one goroutine.
		waited := false
		reaped := error(nil)
		wait := func() error {
			if !waited {
				waited = true
				reaped = command.Wait()
			}
			return reaped
		}

		// Deferred calls run last-registered-first, so these run as cancel, wait,
		// cleanup, close: signal the process, reap it, then remove its configuration
		// file and end the stream.
		defer close(events)
		defer cleanup()
		defer wait()
		defer cancel()

		scanner := bufio.NewScanner(stdout)
		// The CLI emits one JSON object per line, and an assistant message carrying
		// a long tool result is far larger than the default 64KiB token.
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

		usage := Usage{Provider: p.name}
		stopReason := ""
		sawTool := false

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			emitted, tooled, failure := p.handleLine(ctx, events, line, &usage, &stopReason)
			sawTool = sawTool || tooled
			if emitted {
				continue
			}
			// The turn is over before the CLI is, and it has still been billed for
			// the assistant messages it produced. It reports them: a turn's usage is
			// filled from the done event alone, so leaving without one accounted a
			// stopped turn as nothing at all and made §3.3's caps avoidable by
			// stopping every turn.
			p.pricing.Apply(&usage)
			if failure == nil {
				// No failure to report means the send failed, which happens when the
				// caller has gone away.
				deliverDone(ctx, events, DoneEvent(StopReasonCancelled, usage))
				return
			}
			if stopReason == "" {
				stopReason = StopReasonError
			}
			// Before the error, because a consumer stops reading at one and would
			// otherwise never learn what the turn cost.
			deliverDone(ctx, events, DoneEvent(stopReason, usage))
			send(ctx, events, ErrorEvent(failure))
			return
		}

		waitErr := wait()
		if scanErr := scanner.Err(); scanErr != nil {
			p.pricing.Apply(&usage)
			deliverDone(ctx, events, DoneEvent(StopReasonError, usage))
			send(ctx, events, ErrorEvent(fmt.Errorf("llm: %s: reading output: %w", p.name, scanErr)))
			return
		}
		if waitErr != nil {
			p.pricing.Apply(&usage)
			if errors.Is(ctx.Err(), context.Canceled) {
				deliverDone(ctx, events, DoneEvent(StopReasonCancelled, usage))
				return
			}
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = waitErr.Error()
			}
			deliverDone(ctx, events, DoneEvent(StopReasonError, usage))
			send(ctx, events, ErrorEvent(fmt.Errorf("llm: %s: %s", p.name, detail)))
			return
		}

		// The unverified part of §5.7, made visible: tools were offered and the CLI
		// used none. That is not proof of failure — a question may need no tool — so
		// it is logged rather than used to degrade the provider automatically.
		if !sawTool && req.ToolEndpoint != nil && len(req.Tools) > 0 {
			slog.DebugContext(ctx, "claude CLI turn used no ODE tool",
				"provider", p.name, "offered", len(req.Tools))
		}

		p.pricing.Apply(&usage)
		send(ctx, events, DoneEvent(stopReason, usage))
	}()

	return events, nil
}

// cliStreamLine is the subset of the CLI's stream-json output ODE reads. Fields
// outside it are ignored on purpose: the format carries session bookkeeping that
// is the CLI's business, and parsing it strictly would break on the next release.
type cliStreamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Result  string `json:"result"`
	Message struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	} `json:"message"`
	Usage struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	IsError bool `json:"is_error"`
}

// handleLine maps one output line onto the normalised stream. It reports whether
// to keep going, whether the line involved an ODE tool, and what went wrong when
// the turn has to stop.
//
// The failure is returned rather than emitted here so the caller can report the
// turn's usage first. A consumer stops reading at an error event, so an error sent
// from inside this function would bury the done event behind it.
func (p *AnthropicCLIProvider) handleLine(
	ctx context.Context, events chan<- Event, line string, usage *Usage, stopReason *string,
) (keepGoing bool, sawTool bool, failure error) {
	var parsed cliStreamLine
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		// Not fatal: the CLI writes the occasional non-JSON line, and killing a
		// turn over one would make this provider useless.
		slog.DebugContext(ctx, "claude CLI line was not JSON", "provider", p.name)
		return true, false, nil
	}

	switch parsed.Type {
	case "assistant":
		if parsed.Message.Model != "" {
			usage.Model = parsed.Message.Model
		}
		if parsed.Message.StopReason != "" {
			*stopReason = parsed.Message.StopReason
		}
		accumulateCLIUsage(usage, parsed.Message.Usage.InputTokens,
			parsed.Message.Usage.OutputTokens, parsed.Message.Usage.CacheReadInputTokens)

		for _, content := range parsed.Message.Content {
			switch content.Type {
			case "text":
				if content.Text == "" {
					continue
				}
				if !send(ctx, events, TextEvent(content.Text)) {
					return false, sawTool, nil
				}
			case "tool_use":
				name, isODE := unprefix(content.Name)
				sawTool = sawTool || isODE
				input := content.Input
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				// Reported, not dispatched: the CLI has already run this over MCP.
				if !send(ctx, events, ToolCallEvent(ToolCall{
					ID: content.ID, Name: name, Input: input,
				})) {
					return false, sawTool, nil
				}
			}
		}

	case "user":
		// The CLI echoes tool results back on the stream. Forwarded so the SPA can
		// show what each tool returned, exactly as it would for a dispatched call.
		for _, content := range parsed.Message.Content {
			if content.Type != "tool_result" {
				continue
			}
			if !send(ctx, events, ToolResultEvent(ToolResult{
				CallID:  content.ToolUseID,
				Content: json.RawMessage(content.Content),
				IsError: content.IsError,
			})) {
				return false, sawTool, nil
			}
		}

	case "result":
		accumulateCLIUsage(usage, parsed.Usage.InputTokens,
			parsed.Usage.OutputTokens, parsed.Usage.CacheReadInputTokens)
		if parsed.Subtype != "" && parsed.Subtype != "success" {
			*stopReason = parsed.Subtype
		}
		if parsed.IsError {
			message := parsed.Result
			if message == "" {
				message = parsed.Subtype
			}
			return false, sawTool, fmt.Errorf("llm: %s: %s", p.name, message)
		}
	}

	return true, sawTool, nil
}

// accumulateCLIUsage adds a report's tokens.
//
// The CLI reports usage per assistant message and again in the final result, and
// the totals are what the developer is billed for, so they are summed rather than
// replaced — an inner loop of ten tool calls costs ten messages' tokens, and
// taking only the last would under-report spend against a §3.3 cap.
func accumulateCLIUsage(usage *Usage, input, output, cached int) {
	usage.InputTokens += input
	usage.OutputTokens += output
	usage.CachedInputTokens += cached
}

// writeMCPConfig puts the CLI's MCP configuration in a private file and returns
// its path together with a function that removes it.
//
// A file, not the --mcp-config flag's inline JSON form, because that JSON carries
// the developer's Keycloak access token in an Authorization header and a command
// line is not a private channel: /proc/<pid>/cmdline is world-readable and ps
// prints it verbatim, to every process in the container for as long as the CLI
// runs. The CLI itself is the nearest reader — it runs its own agent loop here,
// with a bash tool and stdio MCP servers as further children of ODE, any of which
// could have read the token out of the parent's argv. A token read out of there is
// a full on-behalf-of credential for device and timeseries reads until it expires,
// which is exactly what §3.1 item 3 and D5 reserve to the developer.
//
// The file lives in the system temporary directory with mode 0600, and is removed
// when the turn ends. That is the smallest exposure reachable without introducing
// new configuration: only ODE's own uid can read it, and it exists for one turn
// rather than for the token's lifetime. It is not nothing — a process killed
// between the write and the cleanup leaves the file behind until the container's
// filesystem goes — but an orphan readable by one uid is a different order of
// exposure from an argument every process could read.
func writeMCPConfig(endpoint *ToolEndpoint) (path string, cleanup func(), err error) {
	noop := func() {}

	config, err := mcpConfig(endpoint)
	if err != nil {
		return "", noop, err
	}

	file, err := os.CreateTemp("", "ode-mcp-*.json")
	if err != nil {
		return "", noop, fmt.Errorf("llm: mcp config: %w", err)
	}
	name := file.Name()
	remove := func() {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("a temporary MCP configuration file could not be removed",
				"path", name, "error", err)
		}
	}

	// CreateTemp already opens with 0600. Set explicitly because the mode is the
	// entire point of writing a file at all, and a documented default is a poor
	// place for that to rest.
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		remove()
		return "", noop, fmt.Errorf("llm: mcp config: %w", err)
	}
	if _, err := file.WriteString(config); err != nil {
		_ = file.Close()
		remove()
		return "", noop, fmt.Errorf("llm: mcp config: %w", err)
	}
	if err := file.Close(); err != nil {
		remove()
		return "", noop, fmt.Errorf("llm: mcp config: %w", err)
	}
	return name, remove, nil
}

// mcpConfig renders the CLI's MCP server configuration, pointing at ODE itself.
func mcpConfig(endpoint *ToolEndpoint) (string, error) {
	type server struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	headers := map[string]string{"Authorization": "Bearer " + strings.TrimPrefix(endpoint.Token, "Bearer ")}
	if endpoint.SessionID != "" {
		headers[SessionHeader] = endpoint.SessionID
	}
	config := map[string]any{
		"mcpServers": map[string]server{
			MCPServerName: {Type: "http", URL: endpoint.URL, Headers: headers},
		},
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("llm: mcp config: %w", err)
	}
	return string(encoded), nil
}

const (
	// MCPServerName is what ODE's tool surface is called inside the CLI. The CLI
	// namespaces MCP tools as mcp__<server>__<tool>, so this prefix is what makes
	// an ODE tool distinguishable from the CLI's own.
	MCPServerName = "ode"
	// SessionHeader carries the chat session id to the MCP server, which is how the
	// dispatcher knows which exposure tier to enforce.
	SessionHeader = "X-ODE-Session"
)

func prefixed(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "mcp__"+MCPServerName+"__"+name)
	}
	return out
}

// unprefix strips the CLI's MCP namespace, and says whether the tool was ODE's.
// A tool that was not ODE's is the CLI's own — a file read, a bash command — and
// is shown to the developer under its own name rather than pretending it came
// from the platform.
func unprefix(name string) (string, bool) {
	prefix := "mcp__" + MCPServerName + "__"
	if trimmed, found := strings.CutPrefix(name, prefix); found {
		return trimmed, true
	}
	return name, false
}

// cliPrompt flattens the conversation into one prompt.
//
// The CLI takes a single prompt, not a message list, so history has to be
// rendered as text. This is the provider's real limitation and the reason it is a
// development convenience: an exchange replayed as prose is not the same input as
// a structured conversation, and no wrapper can make it so.
func cliPrompt(req Request) string {
	if len(req.Messages) == 0 {
		return ""
	}
	// The common case, and the faithful one: a fresh turn with no history.
	if len(req.Messages) == 1 && req.Messages[0].Role == RoleUser {
		return messageText(req.Messages[0])
	}

	builder := &strings.Builder{}
	builder.WriteString("Continue this conversation. Earlier turns are transcribed below.\n\n")
	for _, message := range req.Messages[:len(req.Messages)-1] {
		text := messageText(message)
		if text == "" {
			continue
		}
		label := "Developer"
		if message.Role == RoleAssistant {
			label = "Assistant"
		}
		fmt.Fprintf(builder, "%s: %s\n\n", label, text)
	}
	last := req.Messages[len(req.Messages)-1]
	fmt.Fprintf(builder, "Developer (current message): %s", messageText(last))
	return builder.String()
}

func messageText(message Message) string {
	parts := make([]string, 0, len(message.Content))
	for _, content := range message.Content {
		switch content.Type {
		case ContentText:
			if content.Text != "" {
				parts = append(parts, content.Text)
			}
		case ContentToolResult:
			if content.ToolResult != "" {
				parts = append(parts, "[tool result] "+content.ToolResult)
			}
		}
	}
	return strings.Join(parts, "\n")
}
