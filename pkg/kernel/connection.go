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

package kernel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/identifiers"
)

// ids mints Jupyter message and session ids. Random for the same reason
// pkg/identifiers gives: nothing here should be guessable or countable.
var ids = identifiers.New()

const (
	// connectionWriteWait bounds one frame write.
	connectionWriteWait = 20 * time.Second
	// connectionPongWait is how long the kernel may stay silent before the socket
	// is treated as dead. Generous, because a kernel executing a long cell sends
	// nothing at all in the meantime and is not idle.
	connectionPongWait  = 120 * time.Second
	connectionPingEvery = 30 * time.Second
	// subscriberBuffer is how many messages may queue for one execution before
	// output is dropped. A cell printing in a tight loop produces thousands of
	// stream messages, and blocking the read loop on a slow consumer would stall
	// every other execution on the connection — so the drop is bounded and
	// reported, rather than turning into backpressure on the kernel.
	subscriberBuffer = 1024
)

// connection is one WebSocket to one kernel.
//
// It is persistent for the life of the developer's kernel session, rather than
// opened per cell, and that is not only an optimisation. jupyter_server bridges
// the kernel's ZeroMQ sockets onto this WebSocket when the connection opens, and
// a request sent before that bridge is established loses its early iopub
// messages — the busy status, sometimes the first lines of output. Paying a
// kernel_info handshake once on connect closes that race for every cell after
// it; reconnecting per cell would reopen it every time.
type connection struct {
	ws       *websocket.Conn
	session  string
	username string

	writeMux sync.Mutex

	mux         sync.Mutex
	subscribers map[string]*subscriber
	closed      bool
	closeErr    error

	done chan struct{}
}

// subscriber is one execution's view of the connection.
type subscriber struct {
	messages chan message
	// dropped counts messages discarded because the buffer was full. Reported to
	// the caller as truncation rather than lost quietly.
	dropped int
}

// dial opens the kernel channel socket and completes the readiness handshake.
func dial(
	ctx context.Context, dialer *websocket.Dialer, endpoint string, token HubToken, username string,
) (*connection, error) {
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	header := http.Header{}
	header.Set("Authorization", "token "+string(token))

	ws, response, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		code := 0
		if response != nil {
			code = response.StatusCode
			_ = response.Body.Close()
		}
		return nil, &UpstreamError{Resource: endpoint, Code: code, Err: err}
	}

	c := &connection{
		ws:          ws,
		session:     ids.NewID(),
		username:    username,
		subscribers: map[string]*subscriber{},
		done:        make(chan struct{}),
	}

	ws.SetReadLimit(0) // A cell's output is legitimately large; the byte cap is applied above.
	_ = ws.SetReadDeadline(time.Now().Add(connectionPongWait))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(connectionPongWait))
	})

	go c.readLoop()
	go c.pingLoop()

	if err := c.handshake(ctx); err != nil {
		c.close(err)
		return nil, err
	}
	return c, nil
}

// handshake sends kernel_info_request and waits for its reply.
//
// This is the readiness check described on the type: once a reply has come back
// over this socket, the kernel's channels are bridged and no later execute_request
// can race the bridge.
func (c *connection) handshake(ctx context.Context) error {
	msgID := ids.NewID()
	messages, release := c.subscribe(msgID)
	defer release()

	request, err := newMessage(c.session, c.username, msgKernelInfoRequest, channelShell, msgID, struct{}{})
	if err != nil {
		return err
	}
	if err := c.send(request); err != nil {
		return err
	}

	for {
		select {
		case m, open := <-messages:
			if !open {
				return c.err()
			}
			if m.Header.MsgType == msgKernelInfoReply {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.err()
		}
	}
}

func (c *connection) subscribe(msgID string) (<-chan message, func()) {
	sub := &subscriber{messages: make(chan message, subscriberBuffer)}

	c.mux.Lock()
	if c.closed {
		c.mux.Unlock()
		close(sub.messages)
		return sub.messages, func() {}
	}
	c.subscribers[msgID] = sub
	c.mux.Unlock()

	return sub.messages, func() {
		c.mux.Lock()
		defer c.mux.Unlock()
		if existing, found := c.subscribers[msgID]; found && existing == sub {
			delete(c.subscribers, msgID)
			close(sub.messages)
		}
	}
}

// dropped reports how many messages this execution lost to a full buffer.
func (c *connection) dropped(msgID string) int {
	c.mux.Lock()
	defer c.mux.Unlock()
	if sub, found := c.subscribers[msgID]; found {
		return sub.dropped
	}
	return 0
}

func (c *connection) send(m message) error {
	c.writeMux.Lock()
	defer c.writeMux.Unlock()
	if c.isClosed() {
		return c.err()
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(connectionWriteWait))
	if err := c.ws.WriteJSON(m); err != nil {
		return &UpstreamError{Resource: "kernel channels", Err: err}
	}
	return nil
}

// readLoop routes every inbound message to the execution that caused it.
//
// A message with no matching parent is dropped. That is the correct behaviour
// rather than a gap: the kernel emits status transitions and display output
// caused by things ODE did not ask for — a background thread, another client
// attached to the same kernel — and forwarding those into whichever execution
// happens to be running would attribute someone else's output to this cell.
func (c *connection) readLoop() {
	for {
		var m message
		if err := c.ws.ReadJSON(&m); err != nil {
			c.close(readError(err))
			return
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(connectionPongWait))

		parent := m.ParentHeader.MsgID
		if parent == "" {
			continue
		}

		c.mux.Lock()
		sub, found := c.subscribers[parent]
		if found {
			select {
			case sub.messages <- m:
			default:
				sub.dropped++
			}
		}
		c.mux.Unlock()
	}
}

func (c *connection) pingLoop() {
	ticker := time.NewTicker(connectionPingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.writeMux.Lock()
			_ = c.ws.SetWriteDeadline(time.Now().Add(connectionWriteWait))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMux.Unlock()
			if err != nil {
				c.close(&UpstreamError{Resource: "kernel channels", Err: err})
				return
			}
		case <-c.done:
			return
		}
	}
}

// close tears the connection down once, waking every subscriber.
func (c *connection) close(cause error) {
	c.mux.Lock()
	if c.closed {
		c.mux.Unlock()
		return
	}
	c.closed = true
	c.closeErr = cause
	subscribers := c.subscribers
	c.subscribers = map[string]*subscriber{}
	c.mux.Unlock()

	for _, sub := range subscribers {
		close(sub.messages)
	}
	close(c.done)
	_ = c.ws.Close()
}

func (c *connection) isClosed() bool {
	c.mux.Lock()
	defer c.mux.Unlock()
	return c.closed
}

func (c *connection) err() error {
	c.mux.Lock()
	defer c.mux.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	if c.closed {
		return errors.New("kernel: the connection to the kernel closed")
	}
	return nil
}

// readError turns a normal close into something readable. A kernel that was shut
// down or restarted closes the socket, and reporting that as a protocol fault
// would send a developer looking for a bug that is not there.
func readError(err error) error {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return errors.New("kernel: the kernel closed the connection")
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		return errors.New("kernel: the connection to the kernel was closed")
	}
	return &UpstreamError{Resource: "kernel channels", Err: err}
}

func logConnectionClose(user string, err error) {
	if err == nil {
		return
	}
	slog.Debug("kernel connection closed", "user", user, "error", err)
}
