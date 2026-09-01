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

package experiments

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
)

// Why a failed run says anything at all about itself (D34).
//
// §5.13 builds its summary from params and metrics, and for a run that finished
// that is the whole of what anyone needs. For a run that *failed* it is close to
// empty: no metrics, criteria that could not be evaluated, an empty comparison. The
// model was left with "it failed" and nothing to say about it, which is where the
// old prompt's "there are no logs" did real damage — it told the developer their
// run had produced no output while they were reading it in the pane beside.
//
// So a failed run carries a bounded extract of its own last exception: the class,
// the last few frames, and the message. Three properties make it not a log:
//
//   - **It is extracted, not excerpted.** A traceback has a shape, and only that
//     shape is read. Log lines around it — what the job printed, what a library
//     warned about, what a dataset looked like — have no field to travel in.
//   - **It is bounded before it is built**, by frame count and message length,
//     rather than by a byte budget that would make the size of the extract depend
//     on what the job happened to print.
//   - **A model reads it masked** (MaskedFor). An exception message is the one place
//     in a traceback where a value from the developer's own series appears — pandas
//     and numpy put the offending value in the text — and §3.2 says a raw value is
//     an L2 exposure. The developer's own route serves the extract unmasked,
//     because it is their data on their own token and the whole log is one click
//     away on the route beside it.
//
// What is *not* here: a tool. §5.8's table is unchanged, and there is still no way
// for a model to ask for a run's output. It gets what the summary carries or
// nothing.

// Bounds of the extract. Constants rather than configuration: they are properties
// of what a traceback is, not of a deployment. Three frames are the ones a Python
// developer reads — their own call, the library's entry, where it raised — and four
// hundred characters hold every stdlib and sklearn message worth reading.
const (
	maxFailureFrames  = 3
	maxFailureMessage = 400
)

// tracebackMarker is Python's own, and the last one in a log is the one that
// matters: a chained traceback ("During handling of the above exception…") ends
// with the exception that actually stopped the job.
const tracebackMarker = "Traceback (most recent call last):"

// FailureReason says why a failed run could not be diagnosed. Same shape as
// CriterionReason for the same purpose (D24): a non-result names itself.
type FailureReason string

const (
	// ReasonNoTraceback is a job that failed without a Python traceback — killed
	// for memory, a non-zero exit from something that is not Python, a driver that
	// died before the interpreter started.
	ReasonNoTraceback FailureReason = "no_traceback"
	// ReasonNoOutput is a failed job whose log is empty.
	ReasonNoOutput FailureReason = "no_output"
	// ReasonLogsUnavailable is Ray not answering for the output. The summary is
	// still built: a run's metrics do not depend on its log being readable.
	ReasonLogsUnavailable FailureReason = "logs_unavailable"
)

// NotDiagnosedStatus is the status string of a failure nobody could read, and is
// NotComputedStatus's counterpart in this file.
const NotDiagnosedStatus = "not_diagnosed"

// NotDiagnosed is why a failed run carries no exception, in the shape §5.4.6 and
// D24 use everywhere else: an explicit object with a reason, never an absent field.
// Absence would read as "the run failed for no reason", which is not a thing that
// happens.
type NotDiagnosed struct {
	Status string        `json:"status"`
	Reason FailureReason `json:"reason"`
	Detail string        `json:"detail"`
}

// Frame is one line of the traceback: where the failure was, not what was in it.
//
// The file is a base name rather than the path Ray unpacked the job to. That path
// is a session directory and a runtime-resources hash — noise for every reader, and
// the developer's own file name is what they need to find the line.
type Frame struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function,omitempty"`
}

func (f Frame) String() string {
	if f.Function == "" {
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return fmt.Sprintf("%s:%d in %s", f.File, f.Line, f.Function)
}

// Failure is a failed run's last exception, or why there is none.
//
// Exactly one half is set: an exception with its frames, or NotDiagnosed. It is
// present only on a run whose status is a failure — a summary with no Failure at
// all is a run that did not fail, and reading it as "failed for reasons unknown"
// would be wrong.
type Failure struct {
	// Exception is the class, as Python named it: "ValueError",
	// "ray.exceptions.RayTaskError". Empty when the last line of the traceback did
	// not look like one, in which case the whole line is the Message.
	Exception string `json:"exception,omitempty"`
	// Message is the exception's own text, masked for a tier when this Failure is
	// bound for a model's context (MaskedFor).
	Message string `json:"message,omitempty"`
	// Frames are the last maxFailureFrames of the traceback, innermost last, the
	// way Python prints them.
	Frames []Frame `json:"frames,omitempty"`
	// MaskedFor names the exposure tier this extract has been masked for, and is
	// empty on the extract as it came out of the log. Empty therefore means "raw",
	// which is what the developer's own route serves — so a reader can always tell
	// which of the two they are holding.
	MaskedFor string `json:"masked_for_tier,omitempty"`
	// MaskedLiterals counts what masking replaced, so a model reading `[value]`
	// three times can tell that three values were withheld rather than that the
	// message was written that way.
	MaskedLiterals int `json:"masked_literals,omitempty"`
	// Truncated says the message was longer than maxFailureMessage.
	Truncated bool `json:"truncated,omitempty"`
	// NotDiagnosed is set exactly when Exception and Message are both empty.
	NotDiagnosed *NotDiagnosed `json:"not_diagnosed,omitempty"`
}

// Diagnosed reports whether an exception was read out of the log at all.
func (f Failure) Diagnosed() bool { return f.Exception != "" || f.Message != "" }

func notDiagnosed(reason FailureReason, detail string) *Failure {
	return &Failure{NotDiagnosed: &NotDiagnosed{
		Status: NotDiagnosedStatus, Reason: reason, Detail: detail,
	}}
}

// frameLine matches Python's own frame line. The trailing `, in <function>` is
// optional because a frame from an exec'd string or a C extension has none.
var frameLine = regexp.MustCompile(`^\s+File "([^"]+)", line (\d+)(?:, in (.+))?\s*$`)

// exceptionLine is the line that ends a traceback: a dotted class name, a colon,
// and the message. The class is matched strictly so that a printed log line
// containing a colon cannot be read as an exception class.
var exceptionLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.]*)\s*:\s*(.*)$`)

// extractFailure reads the last exception out of a job's output.
//
// Tail-first, like the developer's own log route: a failure is at the end of a log,
// and the lines before the last traceback belong to a run that was still working.
func extractFailure(logs string) *Failure {
	if strings.TrimSpace(logs) == "" {
		return notDiagnosed(ReasonNoOutput,
			"the job's output is empty, so there is nothing in it that says why it failed")
	}

	start := strings.LastIndex(logs, tracebackMarker)
	if start < 0 {
		return notDiagnosed(ReasonNoTraceback,
			"the job's output holds no Python traceback — a job killed for memory or "+
				"stopped before the interpreter started leaves none. The whole output is "+
				"on the developer's own log route")
	}

	lines := strings.Split(logs[start+len(tracebackMarker):], "\n")
	failure := &Failure{}
	for _, line := range lines {
		if match := frameLine.FindStringSubmatch(line); match != nil {
			number, err := strconv.Atoi(match[2])
			if err != nil {
				continue
			}
			failure.Frames = append(failure.Frames, Frame{
				File: path.Base(forwardSlashes(match[1])), Line: number,
				Function: strings.TrimSpace(match[3]),
			})
			continue
		}
		trimmed := strings.TrimRight(line, " \t\r")
		// Indented and not a frame line: the source line Python echoes under a frame,
		// or the caret it underlines with. Neither is the exception, and the source
		// line is the developer's own code rather than anything about the failure.
		if trimmed == "" || trimmed != strings.TrimLeft(trimmed, " \t") {
			continue
		}
		// The first line back at column 0 ends the traceback, and it is the exception.
		if match := exceptionLine.FindStringSubmatch(trimmed); match != nil {
			failure.Exception = match[1]
			failure.Message = strings.TrimSpace(match[2])
		} else {
			failure.Message = trimmed
		}
		break
	}

	if len(failure.Frames) > maxFailureFrames {
		// The innermost frames: the outermost is `<module>` in the entrypoint, which
		// every traceback in a Ray job shares and none of them is about.
		failure.Frames = failure.Frames[len(failure.Frames)-maxFailureFrames:]
	}
	if len(failure.Message) > maxFailureMessage {
		// Cut here and not only in MaskedFor: the bound is a property of the extract
		// rather than of who is reading it, and a message left whole until masking
		// would put a dumped data frame on the developer's own route.
		failure.Message = strings.TrimSpace(failure.Message[:maxFailureMessage])
		failure.Truncated = true
	}
	if !failure.Diagnosed() {
		return notDiagnosed(ReasonNoTraceback,
			"the job's output holds a traceback whose last line is not an exception, "+
				"which is what a log cut off mid-write looks like")
	}
	return failure
}

// forwardSlashes keeps path.Base honest about Windows-style separators, which a
// traceback from a job packaged on a developer's own machine can carry.
func forwardSlashes(name string) string { return strings.ReplaceAll(name, `\`, "/") }

// --- masking ---

// credentialLike is what a token looks like in a log, and it is masked at every
// tier. A job that echoes its environment prints the credential §5.12 minted for
// it, and neither the tier nor the developer's own consent has anything to do with
// whether that belongs in a conversation stored in a database (the same rule
// pkg/repo/git.go and pkg/tools/kernel.go apply to their own outputs).
var credentialLike = regexp.MustCompile(
	`eyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}(?:\.[A-Za-z0-9_\-]+)?` +
		`|(?i:bearer)\s+[A-Za-z0-9._\-]{16,}`)

// quotedLiteral and numberLiteral are the two places a value from the developer's
// series reaches an exception message: quoted, because pandas prints the offending
// cell, and bare, because numpy prints the offending number.
var (
	quotedLiteral = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	numberLiteral = regexp.MustCompile(`-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)
)

// maskPlaceholder is what a withheld literal reads as. Bracketed and spelled out,
// so nobody reads it as part of the message the exception carried.
const maskPlaceholder = "[value]"

// MaskedFor returns the summary as a model at this tier may read it.
//
// **Every path that hands a Summary to a model calls this.** There are two —
// `get_experiment_results` in pkg/tools and the injected message in pkg/interpret —
// and both have a test that fails if the call goes missing. The developer's own
// HTTP route deliberately does not call it: the extract is their data, on their own
// token, and the log it came from is on the route beside it.
//
// The ladder is §3.2's, applied to the one field that can carry a value:
//
//   - **L0 and L1 mask.** A value in a traceback is a value, not an aggregate, and
//     §3.2 puts values at L2 — "aggregates are still data" is the step L1 is, and a
//     raw cell out of the developer's series is past it. So both tiers read the
//     exception class, the frames and the words of the message, and no literals.
//   - **L2 reads the message as it was raised**, because L2 is the tier at which
//     actual values are already exposed and a masked message would be withholding
//     something the model is looking at downsampled series with.
//
// The class and the frames are not masked at any tier: they are the identity of
// code, not of data, and a failure whose location was withheld would be worth
// nothing at all.
func (s Summary) MaskedFor(tier exposure.Tier) Summary {
	if s.Failure == nil {
		return s
	}
	masked := *s.Failure
	masked.MaskedFor = tier.String()
	masked.Message = credentialLike.ReplaceAllString(masked.Message, "[credential]")

	if tier < exposure.L2 {
		count := 0
		replace := func(string) string { count++; return maskPlaceholder }
		masked.Message = quotedLiteral.ReplaceAllStringFunc(masked.Message, replace)
		masked.Message = numberLiteral.ReplaceAllStringFunc(masked.Message, replace)
		masked.MaskedLiterals = count
	}

	// The second cut, because masking can lengthen: `'x'` is shorter than the
	// placeholder that replaces it. extractFailure applies the first.
	if len(masked.Message) > maxFailureMessage {
		masked.Message = strings.TrimSpace(masked.Message[:maxFailureMessage])
		masked.Truncated = true
	}
	s.Failure = &masked
	return s
}
