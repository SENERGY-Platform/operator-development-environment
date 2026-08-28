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

// Package mcp exposes ODE's tool surface as an MCP server (§5.7, the
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
//
// One request does *not* answer promptly, and it is the exception worth stating
// next to the sentence above: a tool needing the developer's confirmation (D11)
// blocks while they decide. That is still no server state here — the hold lives on
// the chat engine, keyed by confirmation id, and this package only waits on it.
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

// Sessions is the slice of the chat engine this package needs: what a session is,
// and somewhere to park a call that needs the developer's agreement.
type Sessions interface {
	// State is every fact about the session that a gate downstream reads.
	//
	// One call rather than one per field, and deliberately not three: the tier, the
	// workbench and the standing answer are read together per request, from the same
	// session, and a transport that fetched only the one it remembered to ask for is
	// how two of them came to be missing here — a tool call arriving with no
	// workbench, and auto mode never applying to a provider that runs its own tool
	// loop.
	// Primitives rather than a struct of this package's: the engine implements
	// this, and a transport's type in the engine's signature is the wrong way round
	// for a dependency — which is why TierFor never took one either.
	State(ctx context.Context, userSub, sessionID string) (
		tier tools.Tier, workbench string, autoRun bool, err error)

	// Hold dispatches a call needing confirmation (D11) and waits for the
	// developer to decide, returning the outcome as an ordinary tool result.
	//
	// The bool reports whether it was held at all. False means nobody would ever
	// have been asked — no turn is in flight to show the request on, or it could
	// not be recorded — and this package refuses rather than waits.
	Hold(ctx context.Context, req tools.Request, call tools.Call) (tools.Result, bool, error)
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

// sessionState is what one chat session says about the calls made in its name.
type sessionState struct {
	// Tier is the exposure tier every dispatched call is gated at.
	Tier tools.Tier
	// WorkbenchID is the checkout and kernel the session acts in (D32). Without it
	// a developer with two workbenches open gets "the request has to name the one
	// it means" from the repository tools, and run_code lands in whichever kernel
	// an unnamed workbench resolves to — which is the worse of the two failures,
	// because it succeeds.
	WorkbenchID string
	// AutoRun is the session's standing answer to run_code's confirmation (D33).
	// It belongs on this transport for the same reason it belongs on the native
	// one: it is the developer's setting about their session, not a property of how
	// the model happens to reach the tools.
	AutoRun bool
}

// caller is what one MCP request establishes about who is asking.
type caller struct {
	token     string
	userSub   string
	sessionID string
	session   sessionState
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

		// Read from the session, on every request. Not from a header, and not cached:
		// a client that could name its own tier would be choosing its own data
		// exposure, which is precisely what §3.2 gives to the developer — and the same
		// argument covers the other two fields, since a client that could name its own
		// workbench could write into an operator the developer is not working on.
		tier, workbench, autoRun, err := s.sessions.State(request.Context(), userSub, sessionID)
		if err != nil {
			writeJSONError(writer, http.StatusNotFound, "no such chat session for this user")
			return
		}

		who := caller{
			token: token, userSub: userSub, sessionID: sessionID,
			session: sessionState{Tier: tier, WorkbenchID: workbench, AutoRun: autoRun},
		}
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
			who.session.Tier, who.session.Tier.Exposes()),
	})

	for _, definition := range s.dispatcher.Registry().Available(who.session.Tier) {
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
		var input json.RawMessage
		if request.Params != nil && len(request.Params.Arguments) > 0 {
			input = json.RawMessage(request.Params.Arguments)
		}

		toolRequest := tools.Request{
			Token:     who.token,
			UserSub:   who.userSub,
			SessionID: who.sessionID,
			Tier:      who.session.Tier,
			// Carried whole, so this transport gates exactly as the native tool loop
			// does. Every field left out here is a gate reading a zero value: an
			// unnamed workbench, and a standing answer the developer gave that never
			// reached the tool it was given for.
			WorkbenchID: who.session.WorkbenchID,
			AutoRun:     who.session.AutoRun,
		}
		call := tools.Call{
			ID:    request.Params.Name + ":mcp",
			Name:  definition.Name,
			Input: input,
		}

		// A tool needing the developer's confirmation is held open here rather than
		// dispatched, so this call is what waits for them (D11).
		//
		// The client is not asked to surface a decision; it sees one slow tool call.
		// ODE waits, because ODE has the developer's session in front of them. The
		// alternatives are both worse than the wait: auto-approving defeats what a
		// confirmed tool is for, and refusing — which this used to do — makes every
		// confirmed tool permanently unreachable on this transport.
		if definition.Confirm {
			result, held, err := s.sessions.Hold(ctx, toolRequest, call)
			if err != nil && !held {
				slog.WarnContext(ctx, "a confirmed tool could not be held",
					"tool", definition.Name, "session", who.sessionID, "error", err)
			}
			if !held {
				return errorResult(confirmationUnavailable(definition.Name))
			}
			return toolResult(definition.Name, result)
		}

		result := s.dispatcher.Dispatch(ctx, toolRequest, call)

		return toolResult(definition.Name, result)
	})

	slog.Debug("mcp tool published", "tool", definition.Name, "tier", who.session.Tier.String())
}

// toolResult renders a dispatched call in MCP's own shape.
func toolResult(name string, result tools.Result) (*sdk.CallToolResult, error) {
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		return nil, fmt.Errorf("mcp: encoding the result of %s: %w", name, err)
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: string(encoded)}},
		// IsError carries the tier refusal through as an error result, which is
		// what makes the CLI relay it rather than treat it as an answer.
		IsError: result.IsError,
	}, nil
}

// A confirmed tool is refused when there is nobody to ask.
//
// Confirmation (D11) needs the developer in front of the ODE UI, and the request
// is shown to them on the exchange their turn is streaming on. Without one — no
// turn in flight, or a confirmation the store would not take — the wait could
// never end, so the call is refused and says so rather than hanging. Ordinarily
// there is a turn: this transport exists for a provider whose tool loop runs
// inside one.
func confirmationUnavailable(name string) map[string]any {
	return map[string]any{
		"error": "confirmation_unavailable",
		"tool":  name,
		"hint": "this tool needs the developer's explicit confirmation, and there is no " +
			"exchange in flight to ask them on. Tell them what you were about to do and " +
			"let them ask again.",
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
