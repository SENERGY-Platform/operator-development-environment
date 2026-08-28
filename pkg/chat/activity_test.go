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
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// The properties the sessions panel rests on: a turn says when it starts and when
// it ends, "ended" distinguishes a reply from a decision the developer owes, and
// none of it crosses between developers.

// awaitState reads states until the wanted session reaches a state, and fails if
// it does not. Reading rather than asserting on the next one, because a turn
// publishes several and a test that pinned the order would break on an unrelated
// state being added.
func awaitState(t *testing.T, states <-chan Activity, sessionID string, want ActivityState) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	seen := []Activity{}
	for {
		select {
		case activity, open := <-states:
			if !open {
				t.Fatalf("the watcher closed before %s reached %q; saw %+v", sessionID, want, seen)
			}
			seen = append(seen, activity)
			if activity.SessionID == sessionID && activity.State == want {
				return
			}
		case <-deadline:
			t.Fatalf("%s never reached %q within 20s; saw %+v", sessionID, want, seen)
		}
	}
}

func TestATurnReportsRunningThenIdle(t *testing.T) {
	h := newHarness(t, textTurn("here is your answer"))
	session := h.session(t, tools.L0)

	states, stop := h.engine.Watch(testUser)
	defer stop()

	exchange, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser,
		session.ID, "do the thing")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	awaitState(t, states, session.ID, ActivityRunning)

	drain(t, exchange)
	// Idle is the panel's "the reply is there". It has to arrive after the turn has
	// closed, or the developer would be sent to a conversation still being written.
	awaitState(t, states, session.ID, ActivityIdle)
}

func TestAnEndedTurnWaitingOnAConfirmationSaysSo(t *testing.T) {
	h := newHarness(t, toolTurn("call-1", "confirmed_tool"))
	session := h.session(t, tools.L0)

	states, stop := h.engine.Watch(testUser)
	defer stop()

	exchange, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser,
		session.ID, "do the thing")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, exchange)

	// The native confirmation path ends the turn and stores the confirmation, so
	// without the store being asked this conversation — the one that actually wants
	// the developer — would be the one reporting a finished reply.
	awaitState(t, states, session.ID, ActivityWaiting)
}

func TestActivityDoesNotCrossBetweenDevelopers(t *testing.T) {
	h := newHarness(t, textTurn("here is your answer"))
	session := h.session(t, tools.L0)

	other, stop := h.engine.Watch("sub-bob")
	defer stop()

	exchange, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser,
		session.ID, "do the thing")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, exchange)

	if got := h.engine.Activities("sub-bob"); len(got) != 0 {
		t.Errorf("another developer's snapshot = %+v, want empty", got)
	}
	select {
	case activity := <-other:
		t.Errorf("another developer's watcher received %+v", activity)
	default:
	}
}

func TestTheSnapshotListsWhatIsRunningNow(t *testing.T) {
	h := newHeldHarness(t, tools.L0)

	// The turn is blocked in the provider, so it is still live here.
	snapshot := h.engine.Activities(testUser)
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %+v, want one running session", snapshot)
	}
	if snapshot[0].SessionID != h.session.ID || snapshot[0].State != ActivityRunning {
		t.Errorf("snapshot[0] = %+v, want %s running", snapshot[0], h.session.ID)
	}

	h.stop()
	if got := h.engine.Activities(testUser); len(got) != 0 {
		t.Errorf("snapshot after the turn ended = %+v, want empty", got)
	}
}

func TestAHeldCallStopsReadingAsBusy(t *testing.T) {
	h := newHeldHarness(t, tools.L0)

	states, stop := h.engine.Watch(testUser)
	defer stop()

	results := h.hold(tools.L0, "confirmed_tool")
	confirmation := h.awaitConfirmation(t)
	// A held call keeps its exchange running. Reported as waiting anyway, because
	// the turn is not going anywhere until the developer answers.
	awaitState(t, states, h.session.ID, ActivityWaiting)

	if got := h.engine.Activities(testUser); len(got) != 1 || got[0].State != ActivityWaiting {
		t.Errorf("snapshot while held = %+v, want one waiting session", got)
	}

	if err := h.engine.Decide(context.Background(), testUser, h.session.ID, confirmation, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	awaitResult(t, results)
	// Back to running: the turn the hold was inside carries on.
	awaitState(t, states, h.session.ID, ActivityRunning)
}

func TestAWatcherThatStopsReadingIsDroppedNotWaitedFor(t *testing.T) {
	h := newHarness(t)
	states, stop := h.engine.Watch(testUser)
	defer stop()

	// One more than the buffer holds, from a watcher that never reads. The engine
	// must not block here: a wedged browser tab would otherwise stall a turn.
	for i := 0; i < watcherBuffer+1; i++ {
		h.engine.publishActivity(testUser, "session-1", ActivityRunning)
	}

	for {
		select {
		case _, open := <-states:
			if !open {
				return
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a watcher that stopped reading was neither dropped nor drained")
		}
	}
}
