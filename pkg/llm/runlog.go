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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// RunEntry is one completed run, as the run log records it.
//
// It carries no session and no user, which is deliberate: that pairing is what
// ode_usage is for, and it is also the reason ode_usage cannot answer the
// question this log exists for. The accounting table is per provider call and is
// read to enforce a cap on a person; this is per run and is read to see whether
// caching is paying for itself. Keeping ODE's domain out of it also keeps this
// package from having to know what a session is.
//
// The four token counts are separate because they are priced apart — a cache read
// at a fraction of fresh input, a cache write above it. Summed, the one number the
// log exists to show is no longer recoverable from it.
type RunEntry struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DurationMS int64     `json:"duration_ms"`

	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Calls is how many provider requests the run actually made, which for a tool
	// loop is more than one and is not the iteration cap.
	Calls int `json:"calls"`

	InputTokens       int `json:"input_tokens"`
	CacheWriteTokens  int `json:"cache_write_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`

	Cost     float64 `json:"cost"`
	Currency string  `json:"currency,omitempty"`
	// CostEstimated repeats what Usage says: the figure is ODE's own, computed from
	// a configured price, and no provider returned it.
	CostEstimated bool `json:"cost_estimated"`
}

// RunLog appends one JSON object per line to a file.
//
// One line per run rather than a structured store, because the questions it
// answers are asked with jq over a few thousand lines and answered in a second.
// A table would need a schema, a migration and a query surface for a file that
// an operator reads by hand.
type RunLog struct {
	mux  sync.Mutex
	file *os.File
}

// NewRunLog opens the log. An empty path returns a nil *RunLog, which every
// method below accepts and ignores: the log is off by default, and "off" should
// not oblige every caller to hold a branch.
//
// A path that cannot be opened is an error rather than a warning. A log somebody
// asked for and that silently writes nowhere is worse than no log at all, because
// the absence of lines then reads as the absence of runs.
func NewRunLog(path string) (*RunLog, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("llm: cannot open the run log at %q: %w", path, err)
	}
	return &RunLog{file: file}, nil
}

// Append writes one line.
//
// A failure is logged and swallowed. The run it describes has already happened
// and has already been accounted in ode_usage; losing its line here is a gap in
// an observability file, and failing the developer's turn over one would be the
// larger harm.
func (l *RunLog) Append(ctx context.Context, entry RunEntry) {
	if l == nil {
		return
	}
	// Derived rather than trusted, so the field cannot disagree with the two
	// timestamps beside it in the same line.
	if !entry.Start.IsZero() && !entry.End.IsZero() {
		entry.DurationMS = entry.End.Sub(entry.Start).Milliseconds()
	}

	line, err := json.Marshal(entry)
	if err != nil {
		slog.ErrorContext(ctx, "could not encode a run log entry", "error", err)
		return
	}

	l.mux.Lock()
	defer l.mux.Unlock()
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		slog.ErrorContext(ctx, "could not write to the run log", "error", err)
	}
}

// Close releases the file. Safe on a nil log and safe to call twice.
func (l *RunLog) Close() error {
	if l == nil {
		return nil
	}
	l.mux.Lock()
	defer l.mux.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
