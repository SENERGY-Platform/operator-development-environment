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
	"encoding/json"
	"testing"
	"time"
)

// TestRelayInterruptsWhenTheCancellationLandsOnAnEventInFlight drives relay
// directly, because the interleaving it covers is not one a test can time
// through the hub: the cancellation has to arrive while relay is handing an
// event to its consumer rather than while it is waiting for the kernel.
//
// Cancelling there used to end the relay on the spot, which left the cell
// running on the kernel and the stream closed without its KindDone.
func TestRelayInterruptsWhenTheCancellationLandsOnAnEventInFlight(t *testing.T) {
	c := &connection{}
	ctx, cancel := context.WithCancel(context.Background())
	messages := make(chan message)
	// One slot, so that the second event leaves relay parked in emit.
	events := make(chan ExecutionEvent, 1)
	interrupted := make(chan struct{}, 1)
	// Held until the consumer has made room, so the completion has somewhere to go.
	resume := make(chan struct{})

	go c.relay(ctx, "msg-1", messages, func() {}, events, executeOptions{
		OnCancel: func() {
			interrupted <- struct{}{}
			<-resume
		},
	})

	messages <- statusMessage(t, "busy") // fills the one slot
	messages <- statusMessage(t, "busy") // received, and now stuck in emit
	cancel()

	select {
	case <-interrupted:
	case <-time.After(5 * time.Second):
		t.Fatal("the kernel was never interrupted, so the cell would keep running")
	}
	<-events
	close(resume)

	var done *ExecutionEvent
	for event := range events {
		if event.Kind == KindDone {
			done = &event
		}
	}
	if done == nil {
		t.Fatal("the stream closed without a done event, so a consumer waits forever")
	}
	if done.Status != StatusInterrupted {
		t.Errorf("status = %q, want interrupted", done.Status)
	}
}

// TestRelayEndsOnDoneWhateverTheSelectPicks covers the same cancellation with
// room left in the consumer's buffer, where whether relay notices the
// cancellation on the message or on the event is down to select picking between
// two ready cases. Repeated, because a single run only exercises one of them.
func TestRelayEndsOnDoneWhateverTheSelectPicks(t *testing.T) {
	for attempt := range 40 {
		c := &connection{}
		ctx, cancel := context.WithCancel(context.Background())
		messages := make(chan message, 8)
		for range cap(messages) {
			messages <- statusMessage(t, "busy")
		}
		events := make(chan ExecutionEvent, 32)
		interrupts := 0
		cancel()

		c.relay(ctx, "msg-1", messages, func() {}, events, executeOptions{
			OnCancel: func() { interrupts++ },
		})

		var done *ExecutionEvent
		for event := range events {
			if event.Kind == KindDone {
				done = &event
			}
		}
		if done == nil {
			t.Fatalf("attempt %d: the stream closed without a done event", attempt)
		}
		if done.Status != StatusInterrupted {
			t.Fatalf("attempt %d: status = %q, want interrupted", attempt, done.Status)
		}
		if interrupts != 1 {
			t.Fatalf("attempt %d: %d interrupts, want exactly one", attempt, interrupts)
		}
	}
}

func statusMessage(t *testing.T, state string) message {
	t.Helper()
	content, err := json.Marshal(map[string]any{"execution_state": state})
	if err != nil {
		t.Fatalf("marshalling the status content: %v", err)
	}
	return message{
		Header:  messageHeader{MsgID: "reply-msg-1", MsgType: msgStatus},
		Channel: "iopub",
		Content: content,
	}
}
