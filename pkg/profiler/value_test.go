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
	"encoding/json"
	"strings"
	"testing"
)

// D24 is non-negotiable: never null, never absent. An LLM reading a missing
// dominant_periods_s concludes "no periodicity" rather than "could not
// determine", then proposes a model on that basis.
func TestAnUncomputedValueMarshalsAsAnExplicitNotComputedObject(t *testing.T) {
	value := Uncomputablef[float64](ReasonInsufficientCoverage, "completeness_ratio %.2f < %.2f", 0.61, 0.80)

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the encoding is not an object: %s", encoded)
	}
	if decoded["status"] != "not_computed" {
		t.Errorf("status = %v, want not_computed", decoded["status"])
	}
	if decoded["reason"] != "insufficient_coverage" {
		t.Errorf("reason = %v, want insufficient_coverage", decoded["reason"])
	}
	if !strings.Contains(decoded["detail"].(string), "0.61") {
		t.Errorf("detail = %v, want the numbers behind the decision", decoded["detail"])
	}
}

func TestNothingEverMarshalsAsNull(t *testing.T) {
	for name, encode := range map[string]func() ([]byte, error){
		"unset":        func() ([]byte, error) { return json.Marshal(Value[float64]{}) },
		"uncomputable": func() ([]byte, error) { return json.Marshal(Uncomputable[Coverage](ReasonReadFailed, "upstream 502")) },
		"slice":        func() ([]byte, error) { return json.Marshal(Uncomputable[[]float64](ReasonWrongKind, "not numeric")) },
	} {
		encoded, err := encode()
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if string(encoded) == "null" {
			t.Errorf("%s marshalled as null", name)
		}
	}
}

// A field no detector touched must still say so. The zero value cannot be a
// silent computed zero, or a detector that forgets to run reports success.
func TestTheZeroValueReportsOutOfScopeRatherThanZero(t *testing.T) {
	var unset Value[float64]

	if unset.IsComputed() {
		t.Fatal("the zero value claims to be computed")
	}
	status := unset.Status()
	if status.Reason != ReasonOutOfScope {
		t.Errorf("reason = %s, want out_of_scope", status.Reason)
	}
	if status.Detail == "" {
		t.Error("detail is empty, so nothing explains the absence")
	}
}

func TestAComputedValueMarshalsAsThePlainValue(t *testing.T) {
	encoded, err := json.Marshal(Computed(900.5))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "900.5" {
		t.Errorf("encoding = %s, want the bare number", encoded)
	}
}

func TestAValueRoundTripsThroughJSON(t *testing.T) {
	original := Computed(Coverage{NPoints: 96, ExpectedPoints: 96, CompletenessRatio: 1})
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Value[Coverage]
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	coverage := mustGet(t, decoded, "decoded coverage")
	if coverage.NPoints != 96 {
		t.Errorf("n_points = %d, want 96", coverage.NPoints)
	}
}

func TestANotComputedObjectRoundTripsAsANonResult(t *testing.T) {
	encoded, err := json.Marshal(Uncomputable[Coverage](ReasonReadFailed, "upstream 502"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Value[Coverage]
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.IsComputed() {
		t.Fatal("a not_computed object decoded as a computed value")
	}
	if decoded.Status().Reason != ReasonReadFailed {
		t.Errorf("reason = %s, want read_failed to survive the round trip", decoded.Status().Reason)
	}
}

// Nothing in ODE writes null, but a hand-edited fixture might, and reading it as
// a computed zero is the exact confusion the type exists to prevent.
func TestAnExplicitNullDecodesAsANonResult(t *testing.T) {
	var decoded Value[float64]
	if err := json.Unmarshal([]byte("null"), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.IsComputed() {
		t.Error("null decoded as a computed value")
	}
}

func TestOrFallsBackWithoutHidingTheDistinction(t *testing.T) {
	if got := Uncomputable[float64](ReasonWrongKind, "").Or(-1); got != -1 {
		t.Errorf("Or = %v, want the fallback", got)
	}
	if got := Computed(7.0).Or(-1); got != 7 {
		t.Errorf("Or = %v, want the value", got)
	}
}
