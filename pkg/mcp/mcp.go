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

// Package mcp exposes ODE's tool surface as an MCP server (SPEC §5.7, the
// `mcp/` component of §2).
//
// It exists for the CLI provider: rather than fighting the `claude` CLI's own
// tool loop, ODE publishes its tools over MCP and points the CLI at them. The
// important property is that this is a second *transport*, not a second
// implementation — the same tools.Registry and the same tools.Dispatcher serve
// both, so the tier gate of §3.2 is enforced identically whichever way a tool is
// reached. A separate MCP-side tool list would be a way around the gate, and the
// gate is the milestone's exit criterion.
//
// Transport is streamable HTTP mounted on ODE's own router, rather than stdio.
// The CLI supports both, and HTTP is what lets one ODE process serve the tools:
// a stdio server would have to be a subprocess, which would then need its own
// route back into this process's registry and session state.
//
// Statelessness is deliberate. Every request carries the developer's token and
// the chat session id, which is exactly what the dispatcher needs, so there is no
// server-side session to keep — and no way for a stale MCP session to hold a tier
// that the developer has since lowered.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// Sessions is the slice of the chat store this package needs: which tier a
// session is at, and who owns it.
type Sessions interface {
	TierFor(ctx context.Context, userSub, sessionID string) (tools.Tier, error)
}

// Server builds an MCP server per request.
type Server struct {
	dispatcher *tools.Dispatcher
	sessions   Sessions
	version    string
}

func New(dispatcher *tools.Dispatcher, sessions Sessions, version string) (*Server, error) {
	if dispatcher == nil {
		return nil, errors.New("mcp: a dispatcher is required")
	}
	if sessions == nil {
		return nil, errors.New("mcp: a session source is required")
	}
	if version == "" {
		version = "0.0.0"
	}
	return &Server{dispatcher: dispatcher, sessions: sessions, version: version}, nil
}

// caller is what one MCP request establishes about who is asking.
type caller struct {
	token     string
	userSub   string
	sessionID string
	tier      tools.Tier
}

type callerKey struct{}

// Handler returns the HTTP handler to mount.
//
// authenticate is supplied by the API layer so that this package does not parse
// tokens or know about realm roles: the MCP surface must authorise exactly as the
// REST surface does, and the way to guarantee that is to share the one
// implementation rather than to write a second.
func (s *Server) Handler(authenticate func(*http.Request) (userSub, token string, err error)) http.Handler {
	getServer := func(request *http.Request) *sdk.Server {
		// The caller was established by the middleware below and travels on the
		// request context, because getServer cannot report an error of its own.
		who, _ := request.Context().Value(callerKey{}).(caller)
		return s.serverFor(who)
	}

	streamable := sdk.NewStreamableHTTPHandler(getServer, &sdk.StreamableHTTPOptions{
		Stateless: true,
	})

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		userSub, token, err := authenticate(request)
		if err != nil {
			writeJSONError(writer, http.StatusUnauthorized, err.Error())
			return
		}

		sessionID := request.Header.Get(llm.SessionHeader)
		if sessionID == "" {
			writeJSONError(writer, http.StatusBadRequest, fmt.Sprintf(
				"the %s header is required: an MCP call is dispatched at a chat session's "+
					"exposure tier, and without a session there is no tier to enforce",
				llm.SessionHeader))
			return
		}

		// The tier comes from the session, on every request. Not from a header, and
		// not cached: a client that could name its own tier would be choosing its own
		// data exposure, which is precisely what §3.2 gives to the developer.
		tier, err := s.sessions.TierFor(request.Context(), userSub, sessionID)
		if err != nil {
			writeJSONError(writer, http.StatusNotFound, "no such chat session for this user")
			return
		}

		who := caller{token: token, userSub: userSub, sessionID: sessionID, tier: tier}
		streamable.ServeHTTP(writer, request.WithContext(
			context.WithValue(request.Context(), callerKey{}, who)))
	})
}

// serverFor builds the server one caller sees.
//
// The advertised tool list is the one their tier permits, which is the same list
// the chat engine offers a provider. Advertising more and refusing on call would
// also be safe — Dispatch is the gate — but it would spend the model's context on
// tools it cannot use and invite refusals.
func (s *Server) serverFor(who caller) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "ode",
		Title:   "ODE — Operator Development Environment",
		Version: s.version,
	}, &sdk.ServerOptions{
		Instructions: fmt.Sprintf(
			"ODE's tool surface for the SENERGY IoT platform. This session is at data "+
				"exposure tier %s: %s The developer controls the tier and no tool changes it. "+
				"Statistics come from ODE's profiler; never compute them yourself.",
			who.tier, who.tier.Exposes()),
	})

	for _, definition := range s.dispatcher.Registry().Available(who.tier) {
		s.addTool(server, definition, who)
	}
	return server
}

func (s *Server) addTool(server *sdk.Server, definition tools.Definition, who caller) {
	// The registry's raw JSON Schema is handed over as-is rather than reflected
	// from a Go type. It is the same schema the native providers receive, and one
	// source for both is what keeps the two transports honest about what a tool
	// accepts.
	schema := json.RawMessage(definition.Schema)

	tool := &sdk.Tool{
		Name:        definition.Name,
		Description: definition.Description,
		InputSchema: schema,
	}

	server.AddTool(tool, func(ctx context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		// A tool needing developer confirmation is published but refused here.
		//
		// Publishing it means the model can see it exists and suggest the developer
		// run it from the ODE interface. Dispatching it would produce a pending
		// confirmation that nobody on this transport can answer — the CLI has no way
		// to surface a decision and wait for it — so the call would hang or, worse,
		// look accepted. Auto-approving is the other alternative and would defeat
		// what a confirmed tool is for (D11).
		if definition.Confirm {
			return errorResult(confirmationUnavailable(definition.Name))
		}

		var input json.RawMessage
		if request.Params != nil && len(request.Params.Arguments) > 0 {
			input = json.RawMessage(request.Params.Arguments)
		}

		result := s.dispatcher.Dispatch(ctx, tools.Request{
			Token:     who.token,
			UserSub:   who.userSub,
			SessionID: who.sessionID,
			Tier:      who.tier,
		}, tools.Call{
			ID:    request.Params.Name + ":mcp",
			Name:  definition.Name,
			Input: input,
		})

		encoded, err := json.Marshal(result.Content)
		if err != nil {
			return nil, fmt.Errorf("mcp: encoding the result of %s: %w", definition.Name, err)
		}

		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: string(encoded)}},
			// IsError carries the tier refusal through as an error result, which is
			// what makes the CLI relay it rather than treat it as an answer.
			IsError: result.IsError,
		}, nil
	})

	slog.Debug("mcp tool published", "tool", definition.Name, "tier", who.tier.String())
}

// A confirmed tool over MCP is refused rather than held.
//
// Confirmation (D11) needs a developer sitting in front of the ODE UI to answer,
// and an MCP call has no way to reach them and wait — the CLI would block on a
// decision it cannot surface. So the tools that need confirmation are not
// published on this transport at all, and the refusal says why. The alternative
// would be to auto-approve, which would defeat the point of a confirmed tool.
func confirmationUnavailable(name string) map[string]any {
	return map[string]any{
		"error": "confirmation_unavailable",
		"tool":  name,
		"hint": "this tool needs the developer's explicit confirmation, which cannot be " +
			"collected over this transport. Ask them to run it from the ODE interface.",
	}
}

// errorResult renders a refusal in MCP's own shape.
func errorResult(content any) (*sdk.CallToolResult, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: string(encoded)}},
		IsError: true,
	}, nil
}

func writeJSONError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": message})
}

// Endpoint is the URL an MCP client should be pointed at, given ODE's own base
// URL. Kept here so the path is defined once and the CLI provider cannot be
// configured with a different one.
func Endpoint(baseURL string) string {
	return strings.TrimSuffix(baseURL, "/") + Path
}

// Path is where the handler mounts.
const Path = "/mcp"
