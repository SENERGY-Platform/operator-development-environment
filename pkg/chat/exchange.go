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

package chat

import (
	"context"
	"sync"
)

// Exchange is one running turn, detached from whatever connection asked for it.
//
// This is the async mechanism, and it is deliberately at the level of the whole
// exchange rather than of individual slow tools. The reason is that a tool result
// is only useful inside a conversation: if `profile_series` ran as a background job
// but the exchange died with the socket, the profile would complete into a cache
// nobody was reading and the conversation would have lost the turn. Detaching the
// exchange instead makes every tool inside it survive for free, and there is
// nothing left for a per-tool job registry to add.
//
// Two consequences worth stating:
//
//   - The developer can close the tab mid-profile and come back to a finished
//     answer, because messages are persisted as the exchange produces them.
//   - A connection is a *view*, not the owner. Subscribe replays what has happened
//     so far and then follows along, so attaching late shows the whole turn.
type Exchange struct {
	SessionID string
	// UserSub owns the conversation this turn belongs to. Recorded here because an
	// exchange outlives the request that started it, and the activity watchers of
	// activity.go need to know whose panel this turn belongs on.
	UserSub string

	mux sync.Mutex
	// history is every event so far, replayed to a late subscriber. Bounded by the
	// turn itself, which the iteration cap already limits, so it needs no trimming.
	history []Event
	subs    map[int]*subscriber
	nextID  int
	closed  bool

	// ctx is the exchange's own lifetime, descended from the engine's root rather
	// than from any request. The work runs under it.
	ctx context.Context
	// cancel stops the work. Held here because the caller that started the exchange
	// has usually gone by the time anyone wants to stop it.
	cancel context.CancelFunc
	// done closes when the exchange finishes, so a waiter needs no polling.
	done chan struct{}
}

type subscriber struct {
	events chan Event
	// lagged marks a subscriber that could not keep up and was dropped. Its channel
	// is closed; the client is expected to re-read the persisted messages, which are
	// the source of truth in any case.
	lagged bool
}

// subscriberBuffer is generous because the alternative to buffering is either
// blocking the exchange on a slow socket or silently dropping events. A whole
// exchange is well under this, so in practice nothing lags.
const subscriberBuffer = 256

func newExchange(sessionID string, cancel context.CancelFunc) *Exchange {
	return &Exchange{
		SessionID: sessionID,
		subs:      map[int]*subscriber{},
		cancel:    cancel,
		done:      make(chan struct{}),
	}
}

// Subscribe returns a channel carrying everything that has happened on this
// exchange followed by everything that happens next, and a function to detach.
//
// The channel closes when the exchange ends or the subscriber is detached. A
// subscriber that stops reading is dropped rather than allowed to stall the
// exchange — see subscriber.lagged.
func (x *Exchange) Subscribe() (<-chan Event, func()) {
	x.mux.Lock()
	defer x.mux.Unlock()

	sub := &subscriber{events: make(chan Event, subscriberBuffer+len(x.history))}
	// Replay under the same lock that guards publish, so no event can slip between
	// the replay and the subscription and be lost or duplicated.
	for _, event := range x.history {
		sub.events <- event
	}
	if x.closed {
		close(sub.events)
		return sub.events, func() {}
	}

	id := x.nextID
	x.nextID++
	x.subs[id] = sub

	return sub.events, func() { x.detach(id) }
}

// detach removes one subscriber without disturbing the others or the work.
func (x *Exchange) detach(id int) {
	x.mux.Lock()
	defer x.mux.Unlock()
	sub, found := x.subs[id]
	if !found {
		return
	}
	delete(x.subs, id)
	if !sub.lagged {
		close(sub.events)
	}
}

// publish records an event and fans it out.
func (x *Exchange) publish(event Event) {
	x.mux.Lock()
	defer x.mux.Unlock()
	if x.closed {
		return
	}
	x.history = append(x.history, event)

	for id, sub := range x.subs {
		select {
		case sub.events <- event:
		default:
			// Dropped rather than blocked. The exchange is doing real work for the
			// developer and must not be held up by a socket that has stopped draining.
			sub.lagged = true
			close(sub.events)
			delete(x.subs, id)
		}
	}
}

// close ends the exchange and closes every subscriber.
func (x *Exchange) close() {
	x.mux.Lock()
	defer x.mux.Unlock()
	if x.closed {
		return
	}
	x.closed = true
	for id, sub := range x.subs {
		close(sub.events)
		delete(x.subs, id)
	}
	close(x.done)
}

// Cancel stops the work. Used when the developer presses stop, which is a
// different act from closing a tab: closing a tab detaches a view, whereas this
// abandons the turn.
func (x *Exchange) Cancel() {
	if x.cancel != nil {
		x.cancel()
	}
}

// Done closes when the exchange has finished.
func (x *Exchange) Done() <-chan struct{} { return x.done }

// Running reports whether the exchange is still working.
func (x *Exchange) Running() bool {
	x.mux.Lock()
	defer x.mux.Unlock()
	return !x.closed
}

// History is what has happened so far, for a caller that wants a snapshot rather
// than a subscription.
func (x *Exchange) History() []Event {
	x.mux.Lock()
	defer x.mux.Unlock()
	return append([]Event{}, x.history...)
}
