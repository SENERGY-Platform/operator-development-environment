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

package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// ---- run_code (L0, confirmed) ----
//
// The tier on this tool is L0 and that is not an oversight in §5.8. Code the
// developer has confirmed runs in the developer's own pod under the developer's
// own platform token, so it can reach data no tool would return at L0. The
// control is the confirmation, not the tier, and Dispatch is what enforces it -
// this executor is only ever reached after the developer said yes.

type runCodeInput struct {
	Code string `json:"code"`
}

// RunCodeResult is what the model reads back.
//
// Deliberately not the raw event stream. A model needs to know what the code
// printed, what it returned and whether it raised; the busy/idle transitions and
// the PNG of a figure are for the developer's console and would be tokens spent
// on nothing here.
type RunCodeResult struct {
	Status string `json:"status"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Result string `json:"result,omitempty"`

	ErrorName  string `json:"error_name,omitempty"`
	ErrorValue string `json:"error_value,omitempty"`
	Traceback  string `json:"traceback,omitempty"`

	// Displays names the rich outputs that were produced without carrying them.
	// A model cannot read a base64 PNG, and §5.9 makes a chart a declarative spec
	// rather than an image, so what it gets is the fact that one exists.
	Displays []string `json:"displays,omitempty"`

	Truncated bool   `json:"truncated,omitempty"`
	Workspace string `json:"workspace"`
	Hint      string `json:"hint,omitempty"`
}

func (s *surface) runCode(ctx context.Context, req Request) (any, error) {
	var in runCodeInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Code) == "" {
		return nil, fmt.Errorf("%w: code is required", ErrInvalidInput)
	}

	req.Progress("kernel", "ensuring the developer's pod and kernel are up")
	events, err := s.deps.Kernel.Run(ctx, req.Token, in.Code)
	if err != nil {
		return nil, err
	}

	budget := s.deps.RunCodeMaxOutputBytes
	result := RunCodeResult{Workspace: s.deps.Kernel.Workspace()}

	var stdout, stderr, value, traceback strings.Builder
	displays := map[string]bool{}
	written := 0

	// append keeps the whole answer inside one budget rather than giving each
	// stream its own, because a cell that floods stderr would otherwise still cost
	// the model the full stdout budget on top.
	append_ := func(into *strings.Builder, text string) {
		if written >= budget {
			result.Truncated = true
			return
		}
		if len(text) > budget-written {
			text = text[:budget-written]
			result.Truncated = true
		}
		into.WriteString(text)
		written += len(text)
	}

	for event := range events {
		switch event.Kind {
		case kernel.KindStream:
			if event.Stream == "stderr" {
				append_(&stderr, event.Text)
			} else {
				append_(&stdout, event.Text)
			}
		case kernel.KindResult:
			append_(&value, event.Text)
			for mediaType := range event.MIME {
				displays[mediaType] = true
			}
		case kernel.KindDisplay:
			for mediaType := range event.MIME {
				displays[mediaType] = true
			}
			if event.Text != "" {
				append_(&value, event.Text)
			}
		case kernel.KindError:
			result.ErrorName, result.ErrorValue = event.ErrorName, event.ErrorValue
			// Inside the same budget as the streams: a traceback with its own
			// allowance would let a failing cell return twice what a working one may.
			append_(&traceback, event.Text)
		case kernel.KindDone:
			result.Status = event.Status
			if event.Truncated {
				result.Truncated = true
			}
			if event.Error != "" {
				result.ErrorValue = event.Error
			}
		}
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	result.Result = value.String()
	result.Traceback = traceback.String()
	for mediaType := range displays {
		result.Displays = append(result.Displays, mediaType)
	}
	if result.Truncated {
		result.Hint = "the output was truncated; print less, or write it to a file in the workspace " +
			"and read back the part that matters"
	}

	// Hygiene, not a boundary. The developer's platform token is installed in the
	// kernel (§5.6 item 4), and a cell that prints the environment while debugging
	// would otherwise put it in the conversation — which is persisted. Redacting
	// the literal string catches that case; it does not and cannot stop code that
	// deliberately encodes it, which is why the confirmation is the actual control.
	return redactToken(result, req.Token), nil
}

func redactToken(result RunCodeResult, bearer string) RunCodeResult {
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(bearer, "Bearer "), "bearer "))
	if len(token) < 16 {
		return result
	}
	const marker = "[redacted: platform token]"
	result.Stdout = strings.ReplaceAll(result.Stdout, token, marker)
	result.Stderr = strings.ReplaceAll(result.Stderr, token, marker)
	result.Result = strings.ReplaceAll(result.Result, token, marker)
	result.Traceback = strings.ReplaceAll(result.Traceback, token, marker)
	return result
}
