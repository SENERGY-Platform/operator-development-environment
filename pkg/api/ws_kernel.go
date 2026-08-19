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
	"log/slog"
	"net/http"
)

// Execution over the WebSocket.
//
// It is a third shape beside the two already here, and the difference is worth
// stating. A profiler operation answers once with a result. A chat exchange is
// detached, so its stream survives the connection that started it. An execution
// is neither: it streams, but it is *bound* to the connection, because a cell is
// something a developer is watching. Dropping the connection cancels it, which
// interrupts the kernel — the opposite of the chat rule, and right for the same
// reason chat's rule is right. Nobody wants a five-minute training loop to keep
// running in their pod because they closed a tab, and nobody wants an assistant's
// answer thrown away because they did.
//
// Cancelling is therefore plain msgCancel, which start() already wires to the
// operation's context: kernel.Service turns that cancellation into an interrupt.

type kernelExecuteBody struct {
	Code string `json:"code"`
}

// startExecution runs one cell and relays its events.
func (s *wsSession) startExecution(ctx context.Context, message wsInbound) {
	if message.ID == "" {
		s.send(wsOutbound{
			Type: msgError, Error: "every request needs an id, so it can be cancelled",
			Status: http.StatusBadRequest,
		})
		return
	}
	if s.kernel == nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID,
			Error: "the kernel surface is not configured on this deployment", Status: http.StatusNotFound,
		})
		return
	}

	var body kernelExecuteBody
	if err := decodePayload(message.Payload, &body); err != nil {
		s.send(wsOutbound{
			Type: msgError, ID: message.ID, Error: err.Error(), Status: http.StatusBadRequest,
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

		// Deliberately not gated on s.slots. That gate bounds concurrent platform
		// reads; an execution runs in the developer's own pod, and one developer can
		// only have one cell running anyway — the kernel service refuses a second.
		events, err := s.kernel.Run(operationCtx, s.token.Bearer(), body.Code)
		if err != nil {
			s.send(wsOutbound{
				Type: msgError, ID: message.ID,
				Error: err.Error(), Status: statusForKernelError(err),
			})
			return
		}
		s.send(wsOutbound{Type: msgAccepted, ID: message.ID})

		for event := range events {
			// The connection's context, not the operation's, and that is the point of
			// this loop. Cancelling an execution interrupts the cell; it does not stop
			// the developer being told how it ended. Relaying on the cancelled context
			// would drop the final `interrupted` event and leave the pane simply going
			// quiet, which reads like a lost connection rather than a stopped cell.
			//
			// sendStream rather than send: losing a line of a developer's own output
			// would be silent corruption of what they are reading.
			if !s.sendStream(ctx, wsOutbound{Type: msgEvent, ID: message.ID, Payload: event}) {
				slog.DebugContext(ctx, "kernel relay stopped", "user", s.user)
				// The events channel is drained so the execution finishes and its
				// interrupt fires, rather than blocking on a reader that has gone.
				for range events {
				}
				return
			}
		}
		s.send(wsOutbound{Type: msgDone, ID: message.ID})
	}()
}

// refreshKernelToken installs a renewed platform token in a live kernel
// (§5.6 item 4).
//
// Called from replaceToken, in a goroutine, because it executes a hidden cell:
// the read loop must not wait on a kernel that may be busy with the developer's
// own code.
func (s *wsSession) refreshKernelToken(bearer string) {
	if s.kernel == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), kernelRefreshTimeout)
		defer cancel()
		if err := s.kernel.RefreshPlatformToken(ctx, bearer); err != nil {
			slog.Debug("pushing the refreshed platform token into the kernel failed",
				"user", s.user, "error", err)
		}
	}()
}
