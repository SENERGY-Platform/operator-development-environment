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
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The Jupyter message protocol, as much of it as running a cell needs.
//
// §5.6 is right that no jupyter_client equivalent is needed: the Hub's WebSocket
// layer bridges ZeroMQ, so this is documented JSON over one socket with a
// `channel` field naming which of the kernel's sockets a message came from or is
// going to. HMAC signing does not apply on this path either — the Hub token
// authorised the connection, and signature fields exist in the envelope but are
// the Hub's business, not ODE's.
//
// One thing worth knowing before changing anything here: correlation is by
// `parent_header.msg_id`. Everything a cell produces carries the msg_id of the
// request that caused it, which is what lets one connection carry several
// executions and what makes the completion rule in execute.go exact rather than
// a timeout.

const protocolVersion = "5.3"

type messageHeader struct {
	MsgID    string `json:"msg_id"`
	Session  string `json:"session"`
	Username string `json:"username"`
	Date     string `json:"date"`
	MsgType  string `json:"msg_type"`
	Version  string `json:"version"`
}

// message is one frame in either direction.
//
// Content is a json.RawMessage rather than a typed union because the message
// types ODE does not handle vastly outnumber the ones it does, and decoding a
// kernel's whole vocabulary to ignore most of it would be work with no reader.
type message struct {
	Header       messageHeader   `json:"header"`
	ParentHeader messageHeader   `json:"parent_header"`
	Metadata     map[string]any  `json:"metadata"`
	Content      json.RawMessage `json:"content"`
	Channel      string          `json:"channel"`
	Buffers      []any           `json:"buffers,omitempty"`
}

// channelShell is the only channel ODE writes to. Everything it reads arrives
// on iopub or shell and is told apart by message type, not by channel, so the
// other channel names are not restated here.
const channelShell = "shell"

const (
	msgExecuteRequest    = "execute_request"
	msgExecuteReply      = "execute_reply"
	msgExecuteInput      = "execute_input"
	msgExecuteResult     = "execute_result"
	msgStream            = "stream"
	msgDisplayData       = "display_data"
	msgUpdateDisplayData = "update_display_data"
	msgError             = "error"
	msgStatus            = "status"
	msgKernelInfoRequest = "kernel_info_request"
	msgKernelInfoReply   = "kernel_info_reply"
)

// newMessage builds a request envelope.
func newMessage(session, username, msgType, channel, msgID string, content any) (message, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return message{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return message{
		Header: messageHeader{
			MsgID:    msgID,
			Session:  session,
			Username: username,
			Date:     time.Now().UTC().Format(time.RFC3339Nano),
			MsgType:  msgType,
			Version:  protocolVersion,
		},
		ParentHeader: messageHeader{},
		Metadata:     map[string]any{},
		Content:      encoded,
		Channel:      channel,
	}, nil
}

// executeRequest is the content of an execute_request.
//
// AllowStdin is false throughout: nothing is watching a blocking input() prompt,
// and a kernel that waits for one is indistinguishable from a hung one. With it
// false the kernel raises StdinNotImplementedError, which the developer sees as
// an error rather than as a cell that never finishes.
type executeRequest struct {
	Code            string         `json:"code"`
	Silent          bool           `json:"silent"`
	StoreHistory    bool           `json:"store_history"`
	UserExpressions map[string]any `json:"user_expressions"`
	AllowStdin      bool           `json:"allow_stdin"`
	StopOnError     bool           `json:"stop_on_error"`
}

// EventKind is what a running cell produced. The set is the subset of the
// protocol a developer or an LLM can act on; anything else is dropped rather
// than surfaced as an unnamed event.
type EventKind string

const (
	// KindStream is stdout or stderr, as written.
	KindStream EventKind = "stream"
	// KindResult is the value of the last expression, the Out[n] of a notebook.
	KindResult EventKind = "execute_result"
	// KindDisplay is anything the code displayed explicitly — a plot, a rich repr.
	KindDisplay EventKind = "display_data"
	// KindError is an exception, with the traceback the kernel formatted.
	KindError EventKind = "error"
	// KindStatus is the kernel's busy/idle transition. Surfaced because a cell that
	// has been accepted but not started looks identical to a lost one otherwise.
	KindStatus EventKind = "status"
	// KindInput echoes the code the kernel accepted, with its execution count.
	KindInput EventKind = "execute_input"
	// KindDone ends the stream, carrying how the cell finished.
	KindDone EventKind = "done"
)

// ExecutionEvent is one thing that happened while a cell ran.
type ExecutionEvent struct {
	Kind EventKind `json:"kind"`

	// Stream is "stdout" or "stderr" for KindStream.
	Stream string `json:"stream,omitempty"`
	// Text is the payload a human reads: stream text, the text/plain rendering of
	// a result, or the joined traceback of an error.
	Text string `json:"text,omitempty"`

	// MIME carries the other renderings of a result or a display, keyed by media
	// type. Kept for the developer's own console — a matplotlib figure is the
	// point of running the cell — and deliberately dropped before anything reaches
	// a model: §5.9 makes charts declarative specs rather than images, and a
	// base64 PNG in a tool result would be tokens spent on something no model can
	// read back.
	MIME map[string]string `json:"mime,omitempty"`

	ExecutionCount int `json:"execution_count,omitempty"`

	// ErrorName and ErrorValue are the exception's class and message.
	ErrorName  string   `json:"error_name,omitempty"`
	ErrorValue string   `json:"error_value,omitempty"`
	Traceback  []string `json:"traceback,omitempty"`

	// State is "busy" or "idle" for KindStatus.
	State string `json:"state,omitempty"`

	// Status is how the cell finished, on KindDone: "ok", "error", "abort",
	// "interrupted" or "timeout".
	Status string `json:"status,omitempty"`
	// Truncated says output was dropped because the cell exceeded the byte cap.
	// Reported rather than silently cut: a developer reading a truncated answer as
	// a complete one is the failure this prevents.
	Truncated bool `json:"truncated,omitempty"`
	// Error is set on KindDone when the execution failed for a reason outside the
	// code — a dropped connection, a timeout.
	Error string `json:"error,omitempty"`
}

// streamContent, resultContent and the rest are the message contents ODE reads.
type streamContent struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type displayContent struct {
	Data           map[string]json.RawMessage `json:"data"`
	Metadata       map[string]any             `json:"metadata"`
	ExecutionCount int                        `json:"execution_count"`
}

type errorContent struct {
	Name      string   `json:"ename"`
	Value     string   `json:"evalue"`
	Traceback []string `json:"traceback"`
}

type statusContent struct {
	ExecutionState string `json:"execution_state"`
}

type executeInputContent struct {
	Code           string `json:"code"`
	ExecutionCount int    `json:"execution_count"`
}

type replyContent struct {
	Status         string `json:"status"`
	ExecutionCount int    `json:"execution_count"`
	Name           string `json:"ename"`
	Value          string `json:"evalue"`
}

// toEvent maps one inbound message onto an ExecutionEvent, or reports that this
// message type is not one ODE surfaces.
func toEvent(m message) (ExecutionEvent, bool) {
	switch m.Header.MsgType {
	case msgStream:
		var content streamContent
		if json.Unmarshal(m.Content, &content) != nil {
			return ExecutionEvent{}, false
		}
		return ExecutionEvent{Kind: KindStream, Stream: content.Name, Text: content.Text}, true

	case msgExecuteResult, msgDisplayData, msgUpdateDisplayData:
		var content displayContent
		if json.Unmarshal(m.Content, &content) != nil {
			return ExecutionEvent{}, false
		}
		kind := KindDisplay
		if m.Header.MsgType == msgExecuteResult {
			kind = KindResult
		}
		text, mime := splitBundle(content.Data)
		return ExecutionEvent{
			Kind:           kind,
			Text:           text,
			MIME:           mime,
			ExecutionCount: content.ExecutionCount,
		}, true

	case msgError:
		var content errorContent
		if json.Unmarshal(m.Content, &content) != nil {
			return ExecutionEvent{}, false
		}
		return ExecutionEvent{
			Kind:       KindError,
			ErrorName:  content.Name,
			ErrorValue: content.Value,
			Traceback:  content.Traceback,
			Text:       stripANSI(strings.Join(content.Traceback, "\n")),
		}, true

	case msgStatus:
		var content statusContent
		if json.Unmarshal(m.Content, &content) != nil {
			return ExecutionEvent{}, false
		}
		return ExecutionEvent{Kind: KindStatus, State: content.ExecutionState}, true

	case msgExecuteInput:
		var content executeInputContent
		if json.Unmarshal(m.Content, &content) != nil {
			return ExecutionEvent{}, false
		}
		return ExecutionEvent{
			Kind: KindInput, Text: content.Code, ExecutionCount: content.ExecutionCount,
		}, true

	default:
		return ExecutionEvent{}, false
	}
}

// splitBundle separates the text/plain rendering from the rest of a MIME bundle.
//
// text/plain is what a human and a model both read, so it becomes Text. The rest
// stays keyed by media type, with each value rendered as a string: a bundle's
// values are JSON, and an image arrives as a base64 string while a JSON mime type
// arrives as an object.
func splitBundle(data map[string]json.RawMessage) (string, map[string]string) {
	if len(data) == 0 {
		return "", nil
	}
	var text string
	mime := make(map[string]string, len(data))
	for mediaType, raw := range data {
		value := renderMIME(raw)
		if mediaType == "text/plain" {
			text = value
			continue
		}
		mime[mediaType] = value
	}
	if len(mime) == 0 {
		mime = nil
	}
	return text, mime
}

func renderMIME(raw json.RawMessage) string {
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	return string(raw)
}

// stripANSI removes the escape sequences IPython colours a traceback with.
//
// The traceback is kept verbatim in Traceback for a UI that can render colour;
// Text is the plain reading, because a model handed raw escape codes spends
// tokens on them and occasionally quotes them back.
func stripANSI(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for index := 0; index < len(s); index++ {
		if s[index] != 0x1b {
			out.WriteByte(s[index])
			continue
		}
		// Skip to the end of the escape sequence. The introducer itself is inside
		// the terminator range, so it has to be consumed before the scan starts, or
		// the sequence ends on its own first byte; anything unterminated is dropped
		// to the end of the string.
		index++
		if index < len(s) && s[index] == '[' {
			index++
			for index < len(s) && !(s[index] >= '@' && s[index] <= '~') {
				index++
			}
		}
	}
	return out.String()
}

// unmarshal is json.Unmarshal, named so execute.go reads without importing
// encoding/json for one call.
func unmarshal(raw json.RawMessage, target any) error { return json.Unmarshal(raw, target) }
