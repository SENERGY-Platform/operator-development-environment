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
	"math"
	"testing"
	"time"
)

// The raw point limit is what these are sized against. Two detectors were
// superlinear in it before the limit was raised: the KDE evaluated the kernel at
// every grid point for every value, and gap classification scanned every sibling
// timestamp for every gap.
func benchSeries(points int) ([]time.Time, []float64) {
	times := make([]time.Time, 0, points)
	values := make([]float64, 0, points)
	at := fixtureStart
	for i := 0; i < points; i++ {
		// A washing-machine shape, so the session detector has real work: long
		// idle stretches broken by cycles.
		value := 2.0
		if (i/1200)%5 == 0 {
			value = 1800 + 40*math.Sin(float64(i))
		}
		times = append(times, at)
		values = append(values, value)
		at = at.Add(5 * time.Second)
	}
	return times, values
}

func BenchmarkActivityAtTheRawPointLimit(b *testing.B) {
	times, values := benchSeries(defaultRawWindowPoints)
	params := DefaultSessionParams(5)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detectActivity(activityInput{
			Times: times, Values: values, Interval: 5,
			Kind: KindInstantaneous, Regularity: Regular,
			Params: params, ProfileID: "bench",
		})
	}
}

func BenchmarkGapClassificationAtTheRawPointLimit(b *testing.B) {
	// A gap every two hundred points, and a sibling reporting throughout: the
	// worst case for the search, since every gap has to be looked up.
	times, _ := benchSeries(defaultRawWindowPoints)
	gaps := make([]Gap, 0, maxGapsRecorded)
	for i := 0; i+1 < len(times) && len(gaps) < maxGapsRecorded; i += 200 {
		gaps = append(gaps, Gap{From: times[i], To: times[i+1], DurationS: 5})
	}
	siblings := make([]time.Time, len(times))
	copy(siblings, times)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		classifyGaps(gaps, siblings, "online", times[len(times)-1])
	}
}

func BenchmarkSamplingAtTheRawPointLimit(b *testing.B) {
	times, _ := benchSeries(defaultRawWindowPoints)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detectSampling(times)
	}
}
