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
	"errors"
	"log/slog"
	"net/http"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
)

// The chat surface over the WebSocket.
//
// It sits beside the profiler operations rather than replacing them, and the two
// have genuinely different shapes: a profiler operation answers once with a result,
// whereas an exchange emits a stream of events until the turn ends. So this does not
// reuse start() — it subscribes to a chat.Exchange and relays.
//
// Two properties follow from the exchange being detached (see chat.Exchange), and
// both are the point of the change:
//
//   - Dropping the connection does not stop the turn. The messages are persisted as
//     they are produced, so the developer can close the tab during a five-minute
//     profile and find the answer waiting.
//   - A connection is a view. chat_attach subscribes to whatever is already running
//     on a session, so a reconnect resumes mid-turn instead of showing a
//     conversation that appears to have stalled.
//
// Stopping the work is therefore a separate, explicit act: chat_cancel.

type chatSendBody struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

type chatConfirmBody struct {
	SessionID      string `json:"session_id"`
	ConfirmationID string `json:"confirmation_id"`
	Approve        *bool  `json:"approve"`
}

type chatAttachBody struct {
	SessionID string `json:"session_id"`
}

// startExchange begins or attaches to an exchange and relays its events.
func (s *wsSession) startExchange(ctx context.Context, message wsInbound) {
	if message.ID == "" {
		s.send(wsOutbound{
			Type: msgError, Error: "every request needs an id, so it can be cancelled",
			Status: http.StatusBadRequest,
		})
		return
	}
	if s.chat == nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: "chat is not configured on this deployment", Status: http.StatusNotFound,
		})
		return
	}

	// The relay's own context, so a cancel message detaches this view. It does not
	// stop the exchange — chat_cancel does that — because closing a view and
	// abandoning a turn are different intentions.
	relayCtx, cancel := context.WithCancel(ctx)

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

		exchange, err := s.exchangeFor(relayCtx, message)
		if err != nil {
			s.send(wsOutbound{
				Type: msgError, ID: message.ID,
				Error: err.Error(), Status: statusForChatError(err),
			})
			return
		}
		if exchange == nil {
			// chat_attach with nothing running. Not an error: the SPA asks on every
			// reconnect and usually there is no turn in flight.
			s.send(wsOutbound{Type: msgDone, ID: message.ID,
				Payload: map[string]any{"attached": false}})
			return
		}

		s.send(wsOutbound{Type: msgAccepted, ID: message.ID})

		// Deliberately not gated on s.slots. That gate bounds concurrent *platform
		// reads*, and an exchange relay is a subscriber doing no platform work of its
		// own — the reads happen inside the detached exchange. Taking a slot here
		// would let one open conversation block the profiler operations beside it.
		s.relay(relayCtx, message.ID, exchange)
	}()
}

// exchangeFor resolves the message to an exchange, starting one if asked.
func (s *wsSession) exchangeFor(ctx context.Context, message wsInbound) (*chat.Exchange, error) {
	switch message.Type {
	case msgChatSend:
		var body chatSendBody
		if err := decodePayload(message.Payload, &body); err != nil {
			return nil, err
		}
		return s.chat.Send(ctx, s.token.Bearer, s.user, body.SessionID, body.Message)

	case msgChatConfirm:
		var body chatConfirmBody
		if err := decodePayload(message.Payload, &body); err != nil {
			return nil, err
		}
		if body.Approve == nil {
			return nil, errors.New("approve must be true or false; " +
				"there is no default for a confirmation")
		}
		// s.token.Bearer, not a copy of it: the exchange outlives this call, and a
		// turn that runs for minutes needs the token the client refreshes meanwhile.
		return s.chat.Confirm(ctx, s.token.Bearer, s.user,
			body.SessionID, body.ConfirmationID, *body.Approve)

	case msgChatAttach:
		var body chatAttachBody
		if err := decodePayload(message.Payload, &body); err != nil {
			return nil, err
		}
		// Ownership is checked before attaching, or a session id would be enough to
		// watch someone else's conversation.
		if _, err := s.chat.Session(ctx, s.user, body.SessionID); err != nil {
			return nil, err
		}
		exchange, running := s.chat.Attach(body.SessionID)
		if !running {
			return nil, nil
		}
		return exchange, nil

	default:
		return nil, errors.New("unknown message type " + message.Type)
	}
}

// relay forwards an exchange's events to the client until the turn ends or this
// view is detached.
func (s *wsSession) relay(ctx context.Context, id string, exchange *chat.Exchange) {
	events, detach := exchange.Subscribe()
	defer detach()

	for {
		select {
		case event, open := <-events:
			if !open {
				// Either the turn ended or this subscriber fell behind and was dropped.
				// Both are reported the same way, because the client's response is the
				// same: re-read the persisted messages, which are the source of truth.
				s.send(wsOutbound{Type: msgDone, ID: id,
					Payload: map[string]any{"attached": true}})
				return
			}
			if !s.sendStream(ctx, wsOutbound{Type: msgEvent, ID: id, Payload: event}) {
				// The connection is going away. The exchange keeps running; that is the
				// whole point of detaching it.
				slog.DebugContext(ctx, "chat relay stopped, exchange continues",
					"user", s.user, "session", exchange.SessionID)
				return
			}

		case <-ctx.Done():
			// The view is detached. Says nothing about the exchange.
			s.send(wsOutbound{Type: msgCancelled, ID: id})
			return
		}
	}
}

// cancelExchange abandons the turn running on a session.
//
// Distinct from msgCancel, which detaches a view: this stops the work. The
// distinction only exists because the exchange is detached, and it is the one the
// developer means when they press stop.
func (s *wsSession) cancelExchange(ctx context.Context, message wsInbound) {
	if s.chat == nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: "chat is not configured on this deployment", Status: http.StatusNotFound,
		})
		return
	}

	var body chatAttachBody
	if err := decodePayload(message.Payload, &body); err != nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID, Error: err.Error(), Status: http.StatusBadRequest,
		})
		return
	}

	if err := s.chat.CancelExchange(ctx, s.user, body.SessionID); err != nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: err.Error(), Status: statusForChatError(err),
		})
		return
	}
	s.send(wsOutbound{Type: msgCancelled, ID: message.ID})
}

// statusForChatError maps the chat domain errors onto the status codes the SPA
// already switches on, so the WebSocket and the REST routes agree.
func statusForChatError(err error) int {
	var limitErr *admin.LimitError
	if errors.As(err, &limitErr) {
		return http.StatusTooManyRequests
	}
	switch {
	case errors.Is(err, chat.ErrNoSuchSession), errors.Is(err, chat.ErrNoSuchConfirmation):
		return http.StatusNotFound
	case errors.Is(err, chat.ErrAlreadyResolved):
		return http.StatusConflict
	case errors.Is(err, chat.ErrInvalidRequest):
		return http.StatusBadRequest
	default:
		return http.StatusForbidden
	}
}
