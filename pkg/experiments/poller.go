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
	"context"
	"errors"
	"log/slog"
	"time"
)

// Noticing that a run finished, without a developer watching (M9).
//
// M8's Get and List refresh a status when somebody asks, and said so in as many
// words: "the refresh is here rather than on a background poller because a status
// is only wanted when somebody is looking, and nothing in ODE acts on a status
// change on its own." M9 is the milestone where something does — §5.13 injects a
// summary into the conversation the run came from — so the poller earns its place.
//
// It has two phases, and the second is why it is not simply a loop over unfinished
// runs.
//
//   - **Settle.** Ask Ray about the runs the store still calls unfinished, and
//     write back what it says. This is what turns a job that ended at three in the
//     morning into a terminal record, and it is bounded in every direction: a
//     capped batch, concurrent within the batch, and a deadline for the tick.
//
//   - **Deliver.** Hand every *recently* terminal run that belongs to a chat
//     session to the sink. Recently, not ever: a run that has been finished for a
//     week is not news, and a poller that re-offered every historical run on every
//     tick would grow with the table.
//
// Delivering from a query rather than from the transition is deliberate. A
// developer opening the Experiments pane refreshes their own runs through List, so
// a run can become terminal on an HTTP request and never pass through this loop at
// all; a poller that fired on its own transitions would then silently skip the
// interpretation for exactly the developers who were paying attention. Asking "what
// finished lately" catches all of them, including runs that finished while ODE was
// down. What makes that safe to repeat is that the sink is idempotent — see
// TerminalSink.

// TerminalSink is told about runs that have finished.
//
// Two requirements, both load-bearing and neither expressible in the type:
//
//   - **It must not block.** It is called from the poller's own goroutine inside a
//     tick that has a deadline, and the work it starts — building a summary,
//     running an assistant turn — takes far longer than a tick. Enqueue and return.
//
//   - **It must be idempotent per run.** The same record is offered on every tick
//     for as long as it is inside the delivery window, and after a restart it is
//     offered again. The sink is what decides a run has already been dealt with.
type TerminalSink interface {
	RunFinished(ctx context.Context, record Experiment)
}

// PollerOptions is how a deployment tunes the loop.
type PollerOptions struct {
	// Interval is how often a tick runs.
	Interval time.Duration
	// Window is how far back a terminal run is still offered to the sink. It bounds
	// the second phase against the size of the table rather than against the number
	// of runs still going, and it is what stops a restart re-offering a year of
	// history.
	Window time.Duration
	// Batch caps how many records one tick may touch in each phase.
	Batch int
	// Timeout bounds one whole tick, so a cluster that has stopped answering costs
	// one tick rather than the loop.
	Timeout time.Duration
}

const (
	defaultPollInterval = 30 * time.Second
	// defaultPollWindow is generous next to the interval on purpose: it is what
	// covers an ODE restart, and a deployment that was down for an hour should still
	// interpret the runs that finished during it.
	defaultPollWindow  = 6 * time.Hour
	defaultPollBatch   = 200
	defaultPollTimeout = 2 * time.Minute
)

// Poller is the loop.
type Poller struct {
	service *Service
	sink    TerminalSink
	opts    PollerOptions

	// stopped closes when the goroutine has returned, which is what lets shutdown
	// and a test wait for it rather than sleep.
	stopped chan struct{}
}

// NewPoller builds it. It refuses rather than degrades on a missing dependency:
// a poller with no sink is a loop that costs cluster calls and does nothing with
// them, which is worse than no poller.
func NewPoller(service *Service, sink TerminalSink, opts PollerOptions) (*Poller, error) {
	if service == nil {
		return nil, errors.New("experiments: a poller needs an experiment service")
	}
	if sink == nil {
		return nil, errors.New(
			"experiments: a poller needs somewhere to deliver a finished run; without one " +
				"it would poll a cluster and discard the answer")
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultPollInterval
	}
	if opts.Window <= 0 {
		opts.Window = defaultPollWindow
	}
	if opts.Batch <= 0 {
		opts.Batch = defaultPollBatch
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultPollTimeout
	}
	return &Poller{
		service: service, sink: sink, opts: opts, stopped: make(chan struct{}),
	}, nil
}

// Start runs the loop until ctx ends.
//
// The same lifecycle idiom kernel.Service.Start uses, and for the same reason:
// ctx is the process's, so shutdown stops the goroutine, and there is exactly one
// of them for the whole deployment rather than one per developer or one per run.
func (p *Poller) Start(ctx context.Context) {
	go func() {
		defer close(p.stopped)
		ticker := time.NewTicker(p.opts.Interval)
		defer ticker.Stop()
		slog.InfoContext(ctx, "experiment poller started",
			"interval", p.opts.Interval, "window", p.opts.Window, "batch", p.opts.Batch)
		for {
			select {
			case <-ticker.C:
				p.Tick(ctx)
			case <-ctx.Done():
				slog.InfoContext(ctx, "experiment poller stopped")
				return
			}
		}
	}()
}

// Stopped closes when the loop has returned. Shutdown waits on it, and so does a
// test that asserts the goroutine is gone rather than merely asked to go.
func (p *Poller) Stopped() <-chan struct{} { return p.stopped }

// Tick is one pass. Exported so a test can drive it without waiting for a timer,
// which is what keeps the poller's tests deterministic rather than timing-based.
func (p *Poller) Tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	p.settle(ctx)
	p.deliver(ctx)
}

// settle asks Ray about the runs the store still calls unfinished.
func (p *Poller) settle(ctx context.Context) {
	running, err := p.service.store.Running(ctx, p.opts.Batch)
	if err != nil {
		slog.ErrorContext(ctx, "the experiment poller could not read the running experiments",
			"error", err)
		return
	}
	for _, record := range running {
		if ctx.Err() != nil {
			// The tick's budget is spent. What is left is picked up next time, which is
			// the whole point of the record being in the store rather than in a queue.
			return
		}
		// refresh is the same code path Get and List use, including settleForgotten —
		// so a submission the cluster has forgotten becomes terminal here rather than
		// being polled forever, which is the defect M8 left and the one that would
		// have bitten this loop hardest.
		if _, err := p.service.refresh(ctx, record); err != nil {
			slog.WarnContext(ctx, "an experiment's status could not be refreshed by the poller",
				"experiment", record.ID, "error", err)
		}
	}
}

// deliver hands the recently finished runs to the sink.
func (p *Poller) deliver(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	since := time.Now().UTC().Add(-p.opts.Window)
	finished, err := p.service.store.RecentlyTerminal(ctx, since, p.opts.Batch)
	if err != nil {
		slog.ErrorContext(ctx, "the experiment poller could not read the finished experiments",
			"error", err)
		return
	}
	if len(finished) >= p.opts.Batch {
		// The batch is full, and this set — unlike the one settle works on — does not
		// shrink as runs are delivered: a finished run stays finished and stays inside
		// the window. So a saturated batch means the newest runs in the window are not
		// being reached at all, and the only repair is a larger batch or a shorter
		// window. Said out loud rather than left to be inferred from a summary that
		// never arrived.
		slog.WarnContext(ctx, "the interpretation batch is full, so the most recently "+
			"finished runs in the window are not being offered; raise "+
			"experiment_poll_batch or shorten experiment_poll_window",
			"batch", p.opts.Batch, "window", p.opts.Window)
	}
	for _, record := range finished {
		if ctx.Err() != nil {
			return
		}
		if record.SessionID == "" || record.RunID == "" {
			// Nowhere to deliver it, and nothing to summarise. A run launched from the
			// Experiments pane rather than from a conversation is read through
			// /experiments/{id}/results when the developer looks, which is what §5.13
			// asks for — the injection is into "the chat context", and this one has none.
			continue
		}
		p.sink.RunFinished(ctx, record)
	}
}
