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

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	servicejwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/relations"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/selection"
)

// The WebSocket surface exists for one reason: a profile read outlives an HTTP
// request.
//
// A raw pass bounded at a hundred thousand points is several megabytes of JSON
// per column from the platform, and the aggregated pass runs over the whole
// analysis window. Behind an ingress with a sixty-second idle timeout that is a
// 504 for the developer and, worse, a read that the backend keeps paying for
// after nobody is listening. Here the work is tied to a connection: the client
// can cancel it, and dropping the connection cancels it too.

const (
	// Message types, client to server.
	msgQuickProfiles = "quick_profiles"
	msgProfile       = "profile"
	msgSelection     = "resolve_selection"
	// msgRelate is M6's relational pass (§5.5). Here for the same reason a profile
	// is: it profiles every participating service before it aligns them, so it is the
	// longest read ODE makes, and cancelling it has to stop those reads rather than
	// only the waiting.
	msgRelate = "relate"
	msgCancel = "cancel"
	msgPing   = "ping"
	// msgAuth replaces the connection's token. A handshake happens once and the
	// connection then lives as long as the tab; the access token does not.
	msgAuth = "auth"

	// Chat (§5.7). These differ in kind from the three above: a profiler operation
	// answers once with a result, whereas an exchange emits a stream of events, so
	// they are served by streamExchange rather than start.
	msgChatSend    = "chat_send"
	msgChatConfirm = "chat_confirm"
	msgChatAttach  = "chat_attach"
	msgChatCancel  = "chat_cancel"

	// The kernel (§5.6). Bound to the connection rather than detached: cancelling
	// one interrupts the cell, because a developer who closed the tab is not
	// waiting for it.
	msgKernelExecute = "kernel_execute"

	// Message types, server to client.
	msgAccepted  = "accepted"
	msgResult    = "result"
	msgEvent     = "event"
	msgError     = "error"
	msgCancelled = "cancelled"
	msgDone      = "done"
	msgPong      = "pong"
)

const (
	// writeWait bounds a single frame write, so one wedged client cannot hold the
	// writer goroutine forever.
	writeWait = 20 * time.Second
	// pongWait is how long a peer may stay silent before the connection is
	// considered dead; pingPeriod must be comfortably below it.
	pongWait   = 90 * time.Second
	pingPeriod = 30 * time.Second
	// maxMessageSize bounds an inbound frame. Requests here are small objects;
	// anything larger is a mistake or an attack.
	maxMessageSize = 64 * 1024
	// outboundBuffer is how many results may queue for a slow client before the
	// connection is dropped. Results are large, so this is deliberately shallow.
	outboundBuffer = 8
	// kernelRefreshTimeout bounds installing a refreshed platform token in a live
	// kernel. Short: it is one hidden cell, and if the kernel is too busy to take
	// it the next execution will push it anyway.
	kernelRefreshTimeout = 30 * time.Second
	// concurrentPerConnection caps the operations one connection may run at once.
	// Each one reads from the platform, and a client that fires twenty profiles
	// would otherwise turn into twenty concurrent platform reads.
	concurrentPerConnection = 4
)

type wsInbound struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

type wsOutbound struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Payload any    `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Status  int    `json:"status,omitempty"`
}

// handleWebSocket upgrades and serves one connection.
//
// @Summary		The streaming surface
// @Description	One WebSocket carries everything that streams: profiler operations, the
// @Description	chat exchange of §5.7, and kernel execution (§5.6). There is one
// @Description	streaming mechanism rather than two, because an exchange can run for
// @Description	minutes inside a single tool call and the connection has to survive it.
// @Description
// @Description	Not behind the usual middleware: a browser cannot set an Authorization
// @Description	header on a handshake, so the token arrives in the Sec-WebSocket-Protocol
// @Description	subprotocol or the query, and this handler enforces the realm role
// @Description	itself. Everything else about §3.1 is unchanged.
// @Tags			streaming
// @Param			token	query	string	false	"bearer token, when the subprotocol cannot carry it"
// @Success		101		"switching protocols"
// @Failure		401		{object}	map[string]string	"no token, or the required realm role is missing"
// @Router			/ws [get]
func handleWebSocket(cfg Config, deps Deps) gin.HandlerFunc {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   4 * 1024,
		WriteBufferSize:  32 * 1024,
		// Gorilla rejects cross-origin by default, which is right, but the SPA is
		// served from another origin in development. The allow-list is the same one
		// CORS uses, so there is one answer to "who may talk to this backend".
		CheckOrigin:  originChecker(cfg.CorsOrigins),
		Subprotocols: []string{bearerSubprotocol},
	}

	return func(c *gin.Context) {
		token, err := websocketToken(c.Request)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		parsed, err := servicejwt.Parse(token)
		if err != nil || parsed.Valid() != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid auth token"})
			return
		}
		// The role check is ODE's own authorisation decision and applies here
		// exactly as it does to every HTTP route (SPEC D5, §3.1).
		if cfg.RequiredRealmRole != "" && !parsed.HasRole(cfg.RequiredRealmRole) {
			c.JSON(http.StatusForbidden, gin.H{
				"error":         "missing required realm role",
				"required_role": cfg.RequiredRealmRole,
			})
			return
		}

		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			// Upgrade has already written a response by this point.
			slog.DebugContext(c.Request.Context(), "websocket upgrade failed", "error", err)
			return
		}

		session := &wsSession{
			conn:          conn,
			token:         newSessionToken(token),
			user:          parsed.Sub,
			requiredRole:  cfg.RequiredRealmRole,
			deviceService: deps.Devices,
			profiler:      deps.Profiler,
			selection:     deps.Selection,
			relations:     deps.Relations,
			chat:          deps.Chat,
			kernel:        deps.Kernel,
			outbound:      make(chan wsOutbound, outboundBuffer),
			running:       map[string]context.CancelFunc{},
			slots:         make(chan struct{}, concurrentPerConnection),
		}
		session.serve(c.Request.Context())
	}
}

// bearerSubprotocol carries the token when a browser cannot set a header.
//
// A browser cannot add Authorization to a WebSocket handshake, so the token
// travels either as this subprotocol or as a query parameter. The subprotocol is
// preferred because a query string ends up in access logs and proxy telemetry;
// the parameter stays supported because not every gateway forwards subprotocols.
// Either way the gateway is what validates the token, and ODE only reads claims —
// the trust boundary of §3.1 is unchanged.
const bearerSubprotocol = "ode.bearer.token"

func websocketToken(r *http.Request) (string, error) {
	// A header, when something other than a browser is connecting.
	if header := r.Header.Get("Authorization"); header != "" {
		return strings.TrimPrefix(strings.TrimPrefix(header, "Bearer "), "bearer "), nil
	}
	for _, protocol := range websocket.Subprotocols(r) {
		if strings.HasPrefix(protocol, bearerSubprotocol+".") {
			return strings.TrimPrefix(protocol, bearerSubprotocol+"."), nil
		}
	}
	if token := r.URL.Query().Get("access_token"); token != "" {
		return token, nil
	}
	return "", errors.New("missing auth token: send it as the " + bearerSubprotocol +
		".<token> subprotocol, an Authorization header, or an access_token parameter")
}

// originChecker allows same-origin plus the configured list. An empty list means
// same-origin only, which is what a deployment behind the gateway wants.
func originChecker(allowed []string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// Not a browser; the gateway is what authenticated this.
			return true
		}
		if strings.EqualFold(origin, "http://"+r.Host) || strings.EqualFold(origin, "https://"+r.Host) {
			return true
		}
		for _, candidate := range allowed {
			if strings.EqualFold(candidate, origin) {
				return true
			}
		}
		return false
	}
}

// sessionToken is the connection's credential, replaceable while the connection
// stays up.
//
// The handshake authenticates once, and every platform read afterwards presents
// whatever this holds. Keeping the handshake's copy for the life of the socket was
// wrong in a way that is hard to report: the SPA refreshes its access token on a
// thirty-second horizon, so a tab left open past the token's lifetime would send
// an expired one on its next profile, the platform would answer 401, and the
// failure would look like a platform fault that goes away on reload.
//
// Read under RLock on every operation rather than captured per connection, so a
// refresh reaches work that is already running.
type sessionToken struct {
	mux    sync.RWMutex
	bearer string
}

func newSessionToken(token string) *sessionToken {
	return &sessionToken{bearer: "Bearer " + token}
}

// Bearer is a chat.TokenSource as a method value, which is how the detached chat
// exchange reads the current token per tool call.
func (t *sessionToken) Bearer() string {
	t.mux.RLock()
	defer t.mux.RUnlock()
	return t.bearer
}

func (t *sessionToken) replace(token string) {
	t.mux.Lock()
	defer t.mux.Unlock()
	t.bearer = "Bearer " + token
}

type wsSession struct {
	conn          *websocket.Conn
	token         *sessionToken
	user          string
	requiredRole  string
	deviceService *devices.Service
	profiler      *profiler.Profiler
	selection     *selection.Resolver
	relations     *relations.Service
	chat          *chat.Engine
	kernel        *kernel.Service

	outbound chan wsOutbound

	mux     sync.Mutex
	running map[string]context.CancelFunc

	slots chan struct{}
	work  sync.WaitGroup
}

func (s *wsSession) serve(parent context.Context) {
	// Cancelling this cancels every operation on the connection, which is what
	// makes a closed browser tab stop costing platform reads.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()

	writerDone := make(chan struct{})
	go s.writeLoop(ctx, writerDone)

	s.readLoop(ctx)

	// Stop the work, wait for it to notice, then let the writer drain and close.
	cancel()
	s.work.Wait()
	close(s.outbound)
	<-writerDone
	_ = s.conn.Close()
}

// writeLoop is the only goroutine that writes: gorilla permits one concurrent
// writer, and results arrive from as many goroutines as there are operations.
func (s *wsSession) writeLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case message, ok := <-s.outbound:
			if !ok {
				_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
				_ = s.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := s.conn.WriteJSON(message); err != nil {
				slog.DebugContext(ctx, "websocket write failed", "user", s.user, "error", err)
				return
			}
		case <-ticker.C:
			// The ping is what detects a peer that vanished without closing —
			// otherwise a long profile would run to completion for nobody.
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *wsSession) readLoop(ctx context.Context) {
	s.conn.SetReadLimit(maxMessageSize)
	_ = s.conn.SetReadDeadline(time.Now().Add(pongWait))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		var message wsInbound
		if err := s.conn.ReadJSON(&message); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.DebugContext(ctx, "websocket closed", "user", s.user, "error", err)
			}
			return
		}
		// Any traffic counts as liveness, not just pongs.
		_ = s.conn.SetReadDeadline(time.Now().Add(pongWait))

		switch message.Type {
		case msgPing:
			s.send(wsOutbound{Type: msgPong, ID: message.ID})
		case msgCancel:
			s.cancelOperation(message.ID)
		case msgAuth:
			s.replaceToken(message)
		case msgQuickProfiles, msgProfile, msgSelection, msgRelate:
			s.start(ctx, message)
		case msgKernelExecute:
			s.startExecution(ctx, message)
		case msgChatSend, msgChatConfirm, msgChatAttach:
			s.startExchange(ctx, message)
		case msgChatCancel:
			s.cancelExchange(ctx, message)
		default:
			s.send(wsOutbound{
				Type: msgError, ID: message.ID,
				Error: "unknown message type " + message.Type, Status: http.StatusBadRequest,
			})
		}
	}
}

// start runs one operation, in its own goroutine under its own cancellable
// context so a later cancel message can reach it.
func (s *wsSession) start(ctx context.Context, message wsInbound) {
	if message.ID == "" {
		s.send(wsOutbound{
			Type: msgError, Error: "every request needs an id, so it can be cancelled",
			Status: http.StatusBadRequest,
		})
		return
	}

	operationCtx, cancel := context.WithCancel(ctx)

	s.mux.Lock()
	if _, exists := s.running[message.ID]; exists {
		s.mux.Unlock()
		cancel()
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: "a request with this id is already running", Status: http.StatusConflict,
		})
		return
	}
	s.running[message.ID] = cancel
	s.mux.Unlock()

	s.work.Add(1)
	go func() {
		defer s.work.Done()
		defer func() {
			cancel()
			s.mux.Lock()
			delete(s.running, message.ID)
			s.mux.Unlock()
		}()

		s.send(wsOutbound{Type: msgAccepted, ID: message.ID})

		// The slot is taken after accepting, so a client sees its request
		// acknowledged even while it queues behind others.
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-operationCtx.Done():
			s.send(wsOutbound{Type: msgCancelled, ID: message.ID})
			return
		}

		payload, err := s.run(operationCtx, message)
		switch {
		case operationCtx.Err() != nil:
			// A cancelled operation reports cancellation, not the error the
			// cancellation caused — an aborted read fails on the way out, and
			// reporting that as a platform fault would be a lie.
			s.send(wsOutbound{Type: msgCancelled, ID: message.ID})
		case err != nil:
			s.send(wsOutbound{
				Type: msgError, ID: message.ID,
				Error: err.Error(), Status: statusForError(err),
			})
		default:
			s.send(wsOutbound{Type: msgResult, ID: message.ID, Payload: payload})
		}
	}()
}

func (s *wsSession) run(ctx context.Context, message wsInbound) (any, error) {
	switch message.Type {
	case msgQuickProfiles:
		var body quickProfileBody
		if err := decodePayload(message.Payload, &body); err != nil {
			return nil, err
		}
		input, err := body.toInput()
		if err != nil {
			return nil, err
		}
		return runQuickProfiles(ctx, s.token.Bearer(), s.deviceService, s.profiler, input)

	case msgProfile:
		var body profileRequestBody
		if err := decodePayload(message.Payload, &body); err != nil {
			return nil, err
		}
		input, err := body.toInput()
		if err != nil {
			return nil, err
		}
		return runProfile(ctx, s.token.Bearer(), s.deviceService, s.profiler, input)

	case msgSelection:
		// Here for the same reason the candidate listing is: a resolution expands
		// devices, and availability is one call per device. Cancelling it stops
		// those reads rather than only the waiting.
		var body selectionBody
		if err := decodePayload(message.Payload, &body); err != nil {
			return nil, err
		}
		input, err := body.toInput()
		if err != nil {
			return nil, err
		}
		return runSelection(ctx, s.token.Bearer(), s.selection, input)

	case msgRelate:
		var body relationBody
		if err := decodePayload(message.Payload, &body); err != nil {
			return nil, err
		}
		input, err := body.toInput()
		if err != nil {
			return nil, err
		}
		// The phases are relayed with send rather than sendStream: a dropped phase
		// costs a progress line and the result still arrives, whereas waiting for room
		// inside the pass would let a slow client stall a platform read.
		input.Progress = func(phase relations.Phase) {
			s.send(wsOutbound{Type: msgEvent, ID: message.ID, Payload: phase})
		}
		return runRelation(ctx, s.token.Bearer(), s.relations, input)

	default:
		return nil, errors.New("unknown message type " + message.Type)
	}
}

// replaceToken adopts a token the client has just refreshed.
//
// Three checks, and each of them is the same check the handshake makes, for the
// same reason. It must parse, or the connection would keep working until the next
// read failed upstream with an error that says nothing about its cause. The
// subject must be unchanged: ODE reads claims without verifying them (§3.1 — the
// gateway verifies), so `sub` is the only thing tying this connection's identity —
// its chat sessions, its spend against the §3.3 cap, its audit rows — to the
// credential the reads are made with, and a token for someone else belongs on a
// new connection. And the realm role is re-checked, because a role revoked while
// the tab was open should end the connection's authority rather than survive in a
// socket nobody re-authorised.
//
// Expiry is deliberately not checked, matching the handshake: the gateway is what
// validates it (§3.1), and servicejwt.Token does not even carry `exp`. A client
// that installs an already-expired token gets 401s from the platform, which is its
// own answer.
func (s *wsSession) replaceToken(message wsInbound) {
	var body struct {
		Token string `json:"token"`
	}
	if err := decodePayload(message.Payload, &body); err != nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: err.Error(), Status: http.StatusBadRequest,
		})
		return
	}

	token := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(body.Token), "Bearer "), "bearer ")
	if token == "" {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: "a token is required", Status: http.StatusBadRequest,
		})
		return
	}

	parsed, err := servicejwt.Parse(token)
	if err != nil || parsed.Valid() != nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: "invalid auth token", Status: http.StatusUnauthorized,
		})
		return
	}
	if parsed.Sub != s.user {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error:  "the token belongs to another user; open a new connection for it",
			Status: http.StatusForbidden,
		})
		return
	}
	if s.requiredRole != "" && !parsed.HasRole(s.requiredRole) {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error:  "the refreshed token is missing the required realm role",
			Status: http.StatusForbidden,
		})
		return
	}

	s.token.replace(token)
	// §5.6 item 4: spawn-time environment variables cannot be refreshed, so a
	// live kernel is told about the new token by executing into it.
	s.refreshKernelToken(token)
	slog.Debug("websocket token replaced", "user", s.user)
	s.send(wsOutbound{Type: msgResult, ID: message.ID, Payload: map[string]any{"authenticated": true}})
}

func (s *wsSession) cancelOperation(id string) {
	s.mux.Lock()
	cancel, running := s.running[id]
	s.mux.Unlock()

	if !running {
		// Cancelling something already finished is normal — the client cannot know
		// the result was in flight — so it is not an error.
		s.send(wsOutbound{Type: msgCancelled, ID: id})
		return
	}
	cancel()
}

// send drops the message rather than blocking when a client cannot keep up.
// Blocking here would stall every other operation on the connection, and the
// ping/pong deadline is what eventually closes a client that has stopped reading.
// send queues a message, dropping it if the client cannot keep up, and reports
// whether it was queued.
//
// Dropping is right for the profiler surface: those messages are large, infrequent,
// and each operation's result is self-contained, so losing one costs a retry rather
// than corrupting anything.
func (s *wsSession) send(message wsOutbound) bool {
	select {
	case s.outbound <- message:
		return true
	default:
		slog.Warn("websocket outbound buffer full; dropping a message",
			"user", s.user, "type", message.Type, "id", message.ID)
		return false
	}
}

// sendStream queues a message, waiting for room, and reports whether it got there.
//
// The chat relay uses this because dropping an event would silently lose part of the
// assistant's answer — a text delta or a tool result — leaving the developer reading
// a mangled reply with no indication anything was missing.
//
// Waiting is safe rather than a stall risk: the exchange publishes without blocking,
// and its own subscriber buffer is what bounds how far behind this relay may fall.
// Once that buffer overflows the exchange drops the subscriber, the events channel
// closes, and the relay reports done — so a wedged client ends its own view instead
// of holding up the work.
func (s *wsSession) sendStream(ctx context.Context, message wsOutbound) bool {
	select {
	case s.outbound <- message:
		return true
	case <-ctx.Done():
		return false
	}
}

func decodePayload(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("invalid payload: " + err.Error())
	}
	return nil
}
