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
	"time"
)

// Whether a conversation is working, waiting for its developer, or idle.
//
// This exists because an exchange is detached (see exchange.go) and a developer
// may hold several conversations at once. The SPA mounts one conversation at a
// time, so the moment they switch to another session nothing in the tab is
// watching the first — and the turn they left running finishes into a view that
// no longer exists. Attention (frontend/src/attention.ts) does not cover this
// either: it fires from inside the open conversation, and only when the whole
// window is in the background.
//
// So the engine says it, for every session at once. A watcher is per developer
// rather than per session, because "which of my conversations wants me" is one
// question and answering it with N subscriptions would mean the SPA subscribing
// to conversations it is not reading, each replaying a turn's worth of events it
// would then throw away.
//
// What is deliberately *not* here is unread state. "You have not looked at it
// since it finished" is the client's knowledge, not the engine's — the engine has
// no idea which pane is on screen — so it reports transitions and the panel keeps
// the mark until the developer opens that session.

// ActivityState is what a conversation is doing right now.
type ActivityState string

const (
	// ActivityRunning is a turn in flight. Nothing is expected of the developer.
	ActivityRunning ActivityState = "running"
	// ActivityWaiting is a turn that has stopped on a confirmation. It is reported
	// separately from idle because the developer has to do something about it, and
	// a turn holding a call open (hold.go) is *running* while it waits — without
	// this it would read as busy for as long as the developer ignored it.
	ActivityWaiting ActivityState = "waiting"
	// ActivityIdle is no turn in flight. After a running state it means the reply
	// is there; on its own it means nothing has been asked.
	ActivityIdle ActivityState = "idle"
)

// Activity is one conversation's state at one moment.
type Activity struct {
	SessionID string        `json:"session_id"`
	State     ActivityState `json:"state"`
	At        time.Time     `json:"at"`
}

// watcher is one subscriber to a developer's activity.
type watcher struct {
	sub    string
	states chan Activity
}

// watcherBuffer is deep enough for every conversation a developer has open to
// change state at once, several times over. A watcher that still falls behind is
// dropped rather than allowed to stall a turn — see publishActivity.
const watcherBuffer = 64

// Watch reports state changes for one developer's conversations, and returns a
// function to stop.
//
// The channel closes when the watcher is stopped, or when it fell behind and was
// dropped. Both mean the same thing to a client: re-subscribe and take a fresh
// snapshot, which is cheap because Activities is read out of memory.
func (e *Engine) Watch(sub string) (<-chan Activity, func()) {
	e.activityMux.Lock()
	defer e.activityMux.Unlock()

	id := e.nextWatcher
	e.nextWatcher++
	entry := &watcher{sub: sub, states: make(chan Activity, watcherBuffer)}
	e.watchers[id] = entry

	return entry.states, func() {
		e.activityMux.Lock()
		defer e.activityMux.Unlock()
		if _, found := e.watchers[id]; !found {
			return
		}
		delete(e.watchers, id)
		close(entry.states)
	}
}

// Activities is the state of every conversation of this developer's that is doing
// something right now.
//
// Idle sessions are left out rather than reported as idle: the answer is a
// snapshot for a panel that lists sessions from the store anyway, and "everything
// else is idle" is what an absent entry already says.
//
// Read out of memory, so it says nothing about a turn that ended before this
// process started. That is the right boundary: the exchange is process state, and
// a finished turn's result is in the conversation.
func (e *Engine) Activities(sub string) []Activity {
	e.exchangeMux.Lock()
	sessions := make([]string, 0, len(e.live))
	for id, exchange := range e.live {
		if exchange.UserSub == sub && exchange.Running() {
			sessions = append(sessions, id)
		}
	}
	e.exchangeMux.Unlock()

	// Held calls belong to a running exchange by construction (Hold attaches to one),
	// so this narrows the set above rather than extending it.
	held := e.heldSessions(sub)

	now := e.now()
	activities := make([]Activity, 0, len(sessions))
	for _, id := range sessions {
		state := ActivityRunning
		if held[id] {
			state = ActivityWaiting
		}
		activities = append(activities, Activity{SessionID: id, State: state, At: now})
	}
	return activities
}

// publishActivity tells every one of this developer's watchers what changed.
//
// A watcher that cannot keep up is dropped, for the reason a lagging exchange
// subscriber is: an engine that blocked here would let a wedged browser tab stall
// the turn itself. Dropping is safe because the state is recoverable — the client
// re-subscribes and reads Activities.
func (e *Engine) publishActivity(sub, sessionID string, state ActivityState) {
	if sub == "" || sessionID == "" {
		return
	}
	activity := Activity{SessionID: sessionID, State: state, At: e.now()}

	e.activityMux.Lock()
	defer e.activityMux.Unlock()
	for id, entry := range e.watchers {
		if entry.sub != sub {
			continue
		}
		select {
		case entry.states <- activity:
		default:
			delete(e.watchers, id)
			close(entry.states)
		}
	}
}

// stateOf is what a session is doing, read from memory.
//
// Used where a state has to be re-derived rather than known — when a held call
// ends, the turn it was inside may have continued or may itself be over, and
// which of the two decides whether the developer is still owed anything.
func (e *Engine) stateOf(sub, sessionID string) ActivityState {
	if e.heldSessions(sub)[sessionID] {
		return ActivityWaiting
	}
	if _, running := e.Attach(sessionID); running {
		return ActivityRunning
	}
	return ActivityIdle
}

// heldSessions is the set of this developer's sessions with a call held open.
func (e *Engine) heldSessions(sub string) map[string]bool {
	e.holdMux.Lock()
	defer e.holdMux.Unlock()
	sessions := map[string]bool{}
	for _, waiting := range e.holds {
		if waiting.userSub == sub {
			sessions[waiting.sessionID] = true
		}
	}
	return sessions
}

// endedState is what a session is in once its exchange has finished: waiting when
// it stopped on a confirmation the developer owes an answer to, idle otherwise.
//
// The store is asked once, at the end of a turn, rather than per session in a
// listing. It matters because the native confirmation path *ends* the turn and
// stores the confirmation — so without this the conversation that most needs the
// developer would be the one reporting that it wants nothing.
func (e *Engine) endedState(sessionID string) ActivityState {
	ctx, cancel := context.WithTimeout(e.root, endedStateTimeout)
	defer cancel()
	pending, err := e.store.PendingConfirmations(ctx, sessionID)
	if err != nil || len(pending) == 0 {
		// A failed read reports idle. The conversation itself still shows the
		// confirmation when it is opened; overstating "this wants you" on a store
		// error would put a mark on the panel that nothing can clear.
		return ActivityIdle
	}
	return ActivityWaiting
}

// endedStateTimeout bounds that one read. Short: it runs after the work is done
// and its only job is to label a dot in a list.
const endedStateTimeout = 5 * time.Second
