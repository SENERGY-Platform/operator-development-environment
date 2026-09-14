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

package exposure

import (
	"errors"
	"fmt"
	"time"
)

// Split is a session's data split (D36): a training end and a test end.
//
// The tier bounds *what kind* of data the assistant observes; the split bounds
// *up to when*. From the moment a developer sets one, the assistant observes no
// value at or after the training end at any tier, and a run launched from the
// session trains on history before the training end only and then runs the
// operator's inference over the test window. It lives beside Tier because the
// same two packages enforce it: pkg/tools, which clamps every value read the
// model can ask for, and pkg/experiments, which writes both bounds into a
// launch's deployment config and later checks that the run confirmed them.
//
// Both bounds are UTC on the way in and on the way out; the API accepts an
// offset and Normalised discards it.
type Split struct {
	// TrainingEnd is the first instant the assistant may not observe and the
	// bound a launched run trains under. It may be in the future when set, in
	// which case a launch is refused until it has passed.
	TrainingEnd time.Time `json:"training_end"`
	// TestEnd closes the window a launched run runs inference over:
	// [TrainingEnd, TestEnd). Strictly after TrainingEnd.
	TestEnd time.Time `json:"test_end"`
}

// ErrInvalidSplit is the base of every refusal of a split's own shape, so a
// handler can answer 400 to all of them with one check.
var ErrInvalidSplit = errors.New("invalid data split")

// Validate refuses a split whose bounds are missing or out of order. It does not
// refuse a training end in the future — that is a launch-time condition, not a
// property of the split.
func (s Split) Validate() error {
	if s.TrainingEnd.IsZero() || s.TestEnd.IsZero() {
		return fmt.Errorf("%w: both training_end and test_end are required", ErrInvalidSplit)
	}
	if !s.TestEnd.After(s.TrainingEnd) {
		return fmt.Errorf("%w: test_end %s is not after training_end %s", ErrInvalidSplit,
			s.TestEnd.UTC().Format(time.RFC3339), s.TrainingEnd.UTC().Format(time.RFC3339))
	}
	return nil
}

// Normalised is the same split in UTC.
func (s Split) Normalised() Split {
	return Split{TrainingEnd: s.TrainingEnd.UTC(), TestEnd: s.TestEnd.UTC()}
}

// Equal compares two splits to the second, which is the resolution a run records
// its bounds at (an RFC 3339 tag with no fraction).
func (s Split) Equal(o Split) bool {
	return s.TrainingEnd.Truncate(time.Second).Equal(o.TrainingEnd.Truncate(time.Second)) &&
		s.TestEnd.Truncate(time.Second).Equal(o.TestEnd.Truncate(time.Second))
}

// Horizon is the training end as a pointer, or nil when there is no split.
//
// Nil-safe on the receiver so that a caller holding a *Split from a session that
// may have none asks once and passes the answer on without a branch.
func (s *Split) Horizon() *time.Time {
	if s == nil {
		return nil
	}
	end := s.TrainingEnd
	return &end
}

// BeyondTrainingEndError is a read the split refuses outright: one whose window
// starts at or after the training end, so that no clamp could leave anything of
// it. It is structured so the tool layer can relay it and the model can read
// what to do — ask the developer, not retry.
type BeyondTrainingEndError struct {
	TrainingEnd time.Time
	From        time.Time
}

func (e *BeyondTrainingEndError) Error() string {
	return fmt.Sprintf("this session's data split ends the observable history at %s; a read "+
		"starting at %s lies entirely in the test window and is refused. The bound is the "+
		"developer's, so ask them to change the split rather than retrying with another window",
		e.TrainingEnd.UTC().Format(time.RFC3339), e.From.UTC().Format(time.RFC3339))
}

// ClampWindow bounds a read window [from, to] to the training end.
//
// A nil split returns the inputs unchanged, which is what lets every reader call
// it unconditionally. A zero `to` means "no upper bound" and becomes the training
// end; a `to` past the training end is lowered to it; a `to` before it is kept.
// A `from` at or after the training end cannot be clamped into anything and is
// refused with *BeyondTrainingEndError. The training end itself is excluded: the
// bound is "no value at or after", so a window ending exactly there is fine and
// one starting exactly there is not.
func (s *Split) ClampWindow(from, to time.Time) (time.Time, time.Time, error) {
	if s == nil {
		return from, to, nil
	}
	end := s.TrainingEnd
	if !from.IsZero() && !from.Before(end) {
		return from, to, &BeyondTrainingEndError{TrainingEnd: end, From: from}
	}
	if to.IsZero() || to.After(end) {
		to = end
	}
	return from, to, nil
}

// Exposes describes the split for the UI and the system prompt, the way
// Tier.Exposes does for the tier. Nil-safe: a session without a split gets the
// sentence that says so.
func (s *Split) Exposes() string {
	if s == nil {
		return "No data split: reads run up to now, and a launch trains on history up to the launch."
	}
	return fmt.Sprintf("Data split: no value at or after %s is observable in this session at any "+
		"tier, and a read starting there is refused rather than moved. A launch trains on "+
		"history before that instant, then runs the operator's inference over [%s, %s) and "+
		"records what it produced.",
		s.TrainingEnd.UTC().Format(time.RFC3339),
		s.TrainingEnd.UTC().Format(time.RFC3339),
		s.TestEnd.UTC().Format(time.RFC3339))
}
