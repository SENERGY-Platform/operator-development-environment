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
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var (
	trainingEnd = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	testEnd     = time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	split       = &Split{TrainingEnd: trainingEnd, TestEnd: testEnd}
)

func TestANilSplitLeavesAWindowAlone(t *testing.T) {
	var none *Split
	from, to := trainingEnd.AddDate(0, 0, -30), testEnd.AddDate(0, 1, 0)
	gotFrom, gotTo, err := none.ClampWindow(from, to)
	if err != nil {
		t.Fatalf("a session without a split refused a read: %v", err)
	}
	if !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Fatalf("window moved without a split: %s..%s", gotFrom, gotTo)
	}
	if none.Horizon() != nil {
		t.Fatal("a nil split has a horizon")
	}
	if none.Exposes() == "" {
		t.Fatal("a nil split has nothing to say about itself")
	}
}

func TestAWindowCrossingTheTrainingEndIsLoweredToIt(t *testing.T) {
	from := trainingEnd.AddDate(0, 0, -7)
	gotFrom, gotTo, err := split.ClampWindow(from, testEnd)
	if err != nil {
		t.Fatalf("a window that starts before the training end was refused: %v", err)
	}
	if !gotFrom.Equal(from) {
		t.Fatalf("the start moved: %s", gotFrom)
	}
	if !gotTo.Equal(trainingEnd) {
		t.Fatalf("the end is %s, want the training end %s", gotTo, trainingEnd)
	}
}

func TestAWindowWithNoUpperBoundEndsAtTheTrainingEnd(t *testing.T) {
	_, gotTo, err := split.ClampWindow(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !gotTo.Equal(trainingEnd) {
		t.Fatalf("an unbounded read ends at %s, want %s", gotTo, trainingEnd)
	}
}

func TestAWindowBelowTheTrainingEndIsKept(t *testing.T) {
	from, to := trainingEnd.AddDate(0, 0, -14), trainingEnd.AddDate(0, 0, -7)
	_, gotTo, err := split.ClampWindow(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTo.Equal(to) {
		t.Fatalf("an end below the training end was moved to %s", gotTo)
	}
}

func TestAWindowStartingAtOrAfterTheTrainingEndIsRefused(t *testing.T) {
	for _, from := range []time.Time{trainingEnd, trainingEnd.Add(time.Second), testEnd} {
		_, _, err := split.ClampWindow(from, testEnd)
		var beyond *BeyondTrainingEndError
		if !errors.As(err, &beyond) {
			t.Fatalf("a read from %s was not refused as beyond the training end: %v", from, err)
		}
		if !beyond.From.Equal(from) || !beyond.TrainingEnd.Equal(trainingEnd) {
			t.Fatalf("the refusal names %s and %s", beyond.From, beyond.TrainingEnd)
		}
		if beyond.Error() == "" {
			t.Fatal("the refusal has no message for the model")
		}
	}
	// A window ending exactly at the training end starts before it and is fine.
	if _, _, err := split.ClampWindow(trainingEnd.Add(-time.Second), trainingEnd); err != nil {
		t.Fatalf("a window ending exactly at the training end was refused: %v", err)
	}
}

func TestValidateRefusesMissingOrReversedBounds(t *testing.T) {
	cases := map[string]Split{
		"no training end": {TestEnd: testEnd},
		"no test end":     {TrainingEnd: trainingEnd},
		"reversed":        {TrainingEnd: testEnd, TestEnd: trainingEnd},
		"equal":           {TrainingEnd: trainingEnd, TestEnd: trainingEnd},
	}
	for name, s := range cases {
		if err := s.Validate(); !errors.Is(err, ErrInvalidSplit) {
			t.Errorf("%s: want ErrInvalidSplit, got %v", name, err)
		}
	}
	if err := split.Validate(); err != nil {
		t.Fatalf("a well-formed split was refused: %v", err)
	}
}

func TestEqualComparesToTheSecond(t *testing.T) {
	other := Split{
		TrainingEnd: trainingEnd.Add(300 * time.Millisecond),
		TestEnd:     testEnd.In(time.FixedZone("CEST", 2*3600)),
	}
	if !split.Equal(other) {
		t.Fatal("a split that differs below the second, or only in zone, is not equal")
	}
	if split.Equal(Split{TrainingEnd: trainingEnd.Add(time.Second), TestEnd: testEnd}) {
		t.Fatal("a split one second apart is equal")
	}
}

func TestASplitRoundTripsThroughJSONInUTC(t *testing.T) {
	local := Split{
		TrainingEnd: trainingEnd.In(time.FixedZone("CEST", 2*3600)),
		TestEnd:     testEnd.In(time.FixedZone("CEST", 2*3600)),
	}.Normalised()
	encoded, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"training_end":"2026-06-01T00:00:00Z","test_end":"2026-06-08T00:00:00Z"}` {
		t.Fatalf("encoded as %s", encoded)
	}
	var decoded Split
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.TrainingEnd.Equal(trainingEnd) || !decoded.TestEnd.Equal(testEnd) {
		t.Fatalf("decoded as %+v", decoded)
	}
}

func TestExposesNamesBothBounds(t *testing.T) {
	sentence := split.Exposes()
	for _, want := range []string{"2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z", "refused"} {
		if !contains(sentence, want) {
			t.Errorf("the sentence does not carry %q: %s", want, sentence)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
