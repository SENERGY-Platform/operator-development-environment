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

package profiler

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Confidence is ordinal and never numeric (SPEC D23). A threshold heuristic
// that reports 0.87 invites an LLM to treat it as a probability it is not;
// three levels plus the raw evidence say the same thing without the false
// precision. `certain` is reserved for ontology-derived and developer-confirmed
// values.
type Confidence string

const (
	Certain   Confidence = "certain"
	Likely    Confidence = "likely"
	Uncertain Confidence = "uncertain"
)

// NotComputedReason is the closed set of reasons a field has no value
// (SPEC §5.4.6).
type NotComputedReason string

const (
	ReasonInsufficientCoverage NotComputedReason = "insufficient_coverage"
	ReasonInsufficientSpan     NotComputedReason = "insufficient_span"
	ReasonWrongKind            NotComputedReason = "wrong_kind"
	ReasonReadFailed           NotComputedReason = "read_failed"
	ReasonOutOfScope           NotComputedReason = "out_of_scope"
)

const notComputedStatus = "not_computed"

// NotComputed is the explicit non-result. It is what a field carries instead of
// null or absence, so that a reader can tell "could not determine" from "no".
type NotComputed struct {
	Status string            `json:"status"`
	Reason NotComputedReason `json:"reason"`
	Detail string            `json:"detail"`
}

// Value holds either a computed value or an explicit NotComputed (SPEC D24).
//
// The type exists to make the rule structural rather than a convention every
// detector has to remember. Absence and negation must be distinguishable: an
// LLM that reads a missing dominant_periods_s as "no periodicity" will go on to
// propose a model on that basis, and nothing downstream can recover the
// difference. So there is no way to express "no value" here without also saying
// why — the zero value marshals as not_computed rather than as null.
type Value[T any] struct {
	value  T
	ok     bool
	reason NotComputedReason
	detail string
}

// Computed wraps a value a detector produced.
func Computed[T any](v T) Value[T] {
	return Value[T]{value: v, ok: true}
}

// Uncomputable records why there is no value.
func Uncomputable[T any](reason NotComputedReason, detail string) Value[T] {
	return Value[T]{reason: reason, detail: detail}
}

// Uncomputablef is Uncomputable with a formatted detail, which is the common
// case: the detail is expected to carry the numbers behind the decision, as in
// "completeness_ratio 0.61 < 0.80".
func Uncomputablef[T any](reason NotComputedReason, format string, args ...any) Value[T] {
	return Value[T]{reason: reason, detail: fmt.Sprintf(format, args...)}
}

func (v Value[T]) IsComputed() bool { return v.ok }

// Get returns the value and whether there is one.
func (v Value[T]) Get() (T, bool) { return v.value, v.ok }

// Or returns the value, or the fallback when there is none. For arithmetic on
// optional fields; never for serialisation, which must keep the distinction.
func (v Value[T]) Or(fallback T) T {
	if v.ok {
		return v.value
	}
	return fallback
}

// Status describes the non-result. An unset Value reports out_of_scope rather
// than an empty reason, because a field no detector touched is exactly that.
func (v Value[T]) Status() NotComputed {
	if v.ok {
		return NotComputed{}
	}
	reason, detail := v.reason, v.detail
	if reason == "" {
		reason = ReasonOutOfScope
		detail = "no detector populated this field"
	}
	return NotComputed{Status: notComputedStatus, Reason: reason, Detail: detail}
}

func (v Value[T]) MarshalJSON() ([]byte, error) {
	if v.ok {
		return json.Marshal(v.value)
	}
	return json.Marshal(v.Status())
}

func (v *Value[T]) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		// Nothing in ODE writes null, but a hand-edited fixture might, and
		// silently reading it as a computed zero would be the exact confusion
		// this type exists to prevent.
		*v = Uncomputable[T](ReasonOutOfScope, "field was null on read")
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("{")) {
		var probe struct {
			Status string            `json:"status"`
			Reason NotComputedReason `json:"reason"`
			Detail string            `json:"detail"`
		}
		if err := json.Unmarshal(trimmed, &probe); err == nil && probe.Status == notComputedStatus {
			*v = Uncomputable[T](probe.Reason, probe.Detail)
			return nil
		}
	}
	var value T
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return err
	}
	*v = Computed(value)
	return nil
}
