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
)

// Completion statuses reported on the final KindDone event.
const (
	StatusOK          = "ok"
	StatusError       = "error"
	StatusAbort       = "abort"
	StatusInterrupted = "interrupted"
	StatusFailed      = "failed"
)

type executeOptions struct {
	// Silent hides the execution from the kernel's history and from the developer.
	// Used for the token push of §5.6 item 4, which is ODE's business and not
	// something the developer asked to run.
	Silent bool
	// Quiet keeps the execution out of the kernel's history like Silent does, but
	// still delivers its output. It is what the workspace operations of
	// workspace.go run under, because they read the answer off stdout.
	//
	// The distinction is not pedantry. The protocol says a silent execution should
	// be "as quiet as possible", so an implementation is free to suppress its
	// output entirely — fine for a token push that only needs to have succeeded,
	// useless for a git command whose stdout is the result.
	Quiet bool
	// MaxOutputBytes bounds the text one execution may emit. Zero means unbounded,
	// which is never what a caller wants and is only reachable from a test.
	MaxOutputBytes int
	// OnCancel runs when the caller's context ends before the cell does. It is how
	// the interrupt reaches the kernel: abandoning the channel alone would leave
	// the cell running and the next one queued behind it.
	OnCancel func()
}

// execute runs one cell and streams what it produces.
//
// The stream ends on a KindDone event, always — including on error, timeout and
// interrupt — so a consumer never has to distinguish "finished" from "the channel
// closed for some other reason".
//
// Completion is exact rather than a heuristic: a cell is over when both its
// execute_reply has arrived on the shell channel and the kernel has posted the
// matching idle status on iopub. Waiting for only the reply truncates trailing
// output, and waiting for only the idle status misses how the cell finished.
func (c *connection) execute(
	ctx context.Context, code string, opts executeOptions,
) (<-chan ExecutionEvent, error) {
	msgID := ids.NewID()
	messages, release := c.subscribe(msgID)

	request, err := newMessage(c.session, c.username, msgExecuteRequest, channelShell, msgID, executeRequest{
		Code:            code,
		Silent:          opts.Silent,
		StoreHistory:    !opts.Silent && !opts.Quiet,
		UserExpressions: map[string]any{},
		AllowStdin:      false,
		StopOnError:     true,
	})
	if err != nil {
		release()
		return nil, err
	}
	if err := c.send(request); err != nil {
		release()
		return nil, err
	}

	events := make(chan ExecutionEvent, 32)
	go c.relay(ctx, msgID, messages, release, events, opts)
	return events, nil
}

// relay forwards one execution's messages as events until the cell is over.
func (c *connection) relay(
	ctx context.Context,
	msgID string,
	messages <-chan message,
	release func(),
	events chan<- ExecutionEvent,
	opts executeOptions,
) {
	defer close(events)
	defer release()

	var (
		written     int
		truncated   bool
		replySeen   bool
		idleSeen    bool
		replyStatus = StatusOK
	)

	// emit drops the event rather than blocking when the consumer has stopped
	// reading, which happens whenever a developer closes the tab mid-cell. The
	// execution itself is unaffected; only this view of it ends.
	emit := func(event ExecutionEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}

	finish := func(status, failure string) {
		emit(ExecutionEvent{
			Kind:      KindDone,
			Status:    status,
			Truncated: truncated || c.dropped(msgID) > 0,
			Error:     failure,
		})
	}

	for {
		select {
		case m, open := <-messages:
			if !open {
				// The connection went away mid-cell. Whether the code finished is
				// genuinely unknown, and saying so is better than reporting either.
				finish(StatusFailed, errorText(c.err()))
				return
			}

			if m.Header.MsgType == msgExecuteReply {
				replySeen = true
				if content, ok := decodeReply(m); ok && content.Status != "" {
					replyStatus = content.Status
				}
				if replySeen && idleSeen {
					finish(replyStatus, "")
					return
				}
				continue
			}

			event, surfaced := toEvent(m)
			if !surfaced {
				continue
			}

			if event.Kind == KindStatus {
				if event.State == "idle" {
					idleSeen = true
					if replySeen {
						finish(replyStatus, "")
						return
					}
				}
			}

			// A silent execution is ODE's own, so nothing of it reaches the
			// developer; only the completion does, which is what the caller waits on.
			if opts.Silent && event.Kind != KindDone {
				continue
			}

			// The budget bounds what the cell *emitted*, so the status transitions and
			// the echo of the code that was sent are not charged against it. The echo
			// matters: a workspace operation sends a cell of several kilobytes and would
			// otherwise spend its whole allowance on hearing its own request back.
			if opts.MaxOutputBytes > 0 && event.Kind != KindStatus && event.Kind != KindInput {
				remaining := opts.MaxOutputBytes - written
				if remaining <= 0 {
					truncated = true
					continue
				}
				event, written, truncated = capEvent(event, remaining, written, truncated)
			}

			if !emit(event) {
				return
			}

		case <-ctx.Done():
			// Stop the cell rather than only stopping the watching. A cell left
			// running holds the kernel, and the next execution would queue behind
			// something nobody is waiting for.
			if opts.OnCancel != nil {
				opts.OnCancel()
			}
			status := StatusInterrupted
			failure := ""
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				failure = "the execution exceeded its time limit and the kernel was interrupted"
			}
			// Emitted on a context that is already done, so it cannot use emit.
			select {
			case events <- ExecutionEvent{
				Kind: KindDone, Status: status, Error: failure,
				Truncated: truncated || c.dropped(msgID) > 0,
			}:
			default:
			}
			return

		case <-c.done:
			finish(StatusFailed, errorText(c.err()))
			return
		}
	}
}

// capEvent trims one event to the remaining budget.
func capEvent(event ExecutionEvent, remaining, written int, truncated bool) (ExecutionEvent, int, bool) {
	size := len(event.Text)
	for _, value := range event.MIME {
		size += len(value)
	}
	if size <= remaining {
		return event, written + size, truncated
	}

	// Over budget. Keep as much of the text as fits and drop the other renderings
	// entirely: half a base64 image is worse than none, while half a traceback
	// still says what went wrong.
	if len(event.Text) > remaining {
		event.Text = event.Text[:remaining]
	}
	event.MIME = nil
	return event, written + len(event.Text), true
}

func decodeReply(m message) (replyContent, bool) {
	var content replyContent
	if err := unmarshal(m.Content, &content); err != nil {
		return replyContent{}, false
	}
	return content, true
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
