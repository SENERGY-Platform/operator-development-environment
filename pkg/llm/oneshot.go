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

package llm

import (
	"context"
	"errors"
	"strings"
)

// Completion is one answer, whole: the text of a turn, what it cost, and why it
// ended.
type Completion struct {
	Text       string
	Usage      Usage
	StopReason string
}

// Text runs one turn and returns it in one piece.
//
// The chat engine consumes Stream incrementally, because a developer watches the
// answer arrive. Everything else that needs the model wants the finished text and
// nothing else — the commit message draft of §5.11 item 5 is the first such
// caller. Each of them writing its own loop over the channel would be several
// slightly different treatments of the two events that are easy to get wrong: an
// error that arrives *after* text has already been streamed, and the done event
// that carries the usage §3.3's caps are computed from. So the loop lives here
// once, and the usage comes back even when the turn failed.
//
// Tools are refused rather than dropped. A one-shot call has no dispatcher, so a
// request that offered one would either hang waiting for a result nobody is going
// to produce, or silently return the model's preamble as if it were the answer.
func Text(ctx context.Context, provider Provider, req Request) (Completion, error) {
	if provider == nil {
		return Completion{}, ErrNotConfigured
	}
	if len(req.Tools) > 0 {
		return Completion{}, errors.New(
			"llm: a one-shot completion cannot offer tools; there is nothing to dispatch them")
	}
	stream, err := provider.Stream(ctx, req)
	if err != nil {
		return Completion{}, err
	}

	var (
		text      strings.Builder
		answer    Completion
		streamErr error
	)
	// Drained to the end in every case, including a failure: the done event sits
	// behind the error event, and it carries what the provider already billed.
	for event := range stream {
		switch event.Type {
		case EventTextDelta:
			text.WriteString(event.Text)
		case EventDone:
			if event.Usage != nil {
				answer.Usage = *event.Usage
			}
			answer.StopReason = event.StopReason
		case EventError:
			if streamErr == nil {
				streamErr = event.Err
				if streamErr == nil {
					streamErr = errors.New(event.Error)
				}
			}
		}
	}
	answer.Text = text.String()
	if streamErr != nil {
		return answer, streamErr
	}
	return answer, nil
}
