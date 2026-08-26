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

package relations

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/profiler"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// AlignedFrame is several series on one time grid.
//
// Times is the grid, and every column's Values and Present slices are the same
// length as it. That is the invariant the whole of relate.go depends on, and it is
// established by the *request* rather than by resampling afterwards (§5.3.1): one
// batched query, one groupTime for every element, one set of bucket boundaries.
type AlignedFrame struct {
	Window      profiler.Window `json:"window"`
	GroupTime   string          `json:"group_time"`
	GridSeconds float64         `json:"grid_seconds"`
	Times       []time.Time     `json:"times"`
	Columns     []AlignedColumn `json:"columns"`
	// Reads is one, by construction. It is reported rather than assumed because
	// "alignment costs one query" is a claim this package makes about itself, and a
	// figure in the answer is what makes it checkable (§5.5).
	Reads int      `json:"reads"`
	Notes []string `json:"notes"`
}

// AlignedColumn is one member's values on the shared grid.
type AlignedColumn struct {
	Ref    profiler.SeriesRef `json:"ref"`
	Values []float64          `json:"values"`
	// Present distinguishes a bucket with no reading from a bucket that read zero.
	// A power series is legitimately zero when the appliance is off, so folding the
	// two together would turn every gap into an idle period — the mistake that would
	// make every rule in this package wrong in the same direction.
	Present []bool `json:"present"`
	// GroupType is the aggregate the platform applied. A cumulative counter is
	// differenced server-side, so a rule about a meter is a rule about its rate;
	// recording which is which keeps that visible where the rule is read.
	GroupType string `json:"group_type"`
	// Points is how many buckets carried a reading.
	Points int `json:"points"`
}

// alignRequest is what Align needs about each member beyond its reference.
type alignRequest struct {
	Ref profiler.SeriesRef
	// Kind decides the aggregate: a cumulative counter is read as a per-bucket
	// difference, everything else as a mean.
	Kind profiler.ValueKind
}

// gridLadder is the set of bucket widths a relation may be computed on. It is the
// profiler's ladder, restricted to what a state series can be read on: a bucket
// under a minute over a month is millions of buckets, and a bucket over a day
// cannot express "certain times of day".
var gridLadder = []float64{60, 300, 900, 1800, 3600, 7200, 10800, 21600, 43200}

// chooseGrid picks the aligned bucket from the *coarsest* member (§5.5).
//
// The direction is the whole point and is easy to get backwards. A bucket finer
// than the slowest member's sampling interval leaves that member with empty
// buckets between its arrivals, and an empty bucket is not an idle device — so a
// fine grid manufactures exactly the idle states a co-occurrence rule is most
// sensitive to. Taking the maximum interval and rounding up the ladder means every
// member has something to say in every bucket.
//
// maxBuckets then widens further if the window would produce more buckets than the
// pass is allowed. Widening rather than truncating, for the reason
// timeseries.Bucket gives: a truncated read looks like the whole window.
func chooseGrid(window profiler.Window, intervals []float64, maxBuckets int) (seconds float64, widened bool) {
	coarsest := 0.0
	for _, interval := range intervals {
		if interval > coarsest {
			coarsest = interval
		}
	}
	if coarsest <= 0 {
		// No member reported a sampling interval. Fifteen minutes is the platform's
		// meter cadence and the least surprising floor to fall back to.
		coarsest = 900
	}

	chosen := gridLadder[len(gridLadder)-1]
	for _, step := range gridLadder {
		if step >= coarsest {
			chosen = step
			break
		}
	}

	if maxBuckets > 0 {
		span := window.Duration().Seconds()
		for span/chosen > float64(maxBuckets) {
			next, found := nextGrid(chosen)
			if !found {
				break
			}
			chosen = next
			widened = true
		}
	}
	return chosen, widened
}

// maxAlignableSeconds is the widest window a pass can cover: maxBuckets of the
// coarsest bucket on the ladder.
//
// Past it no grid choice can bound the read, because chooseGrid has nothing
// wider to widen to — and Align allocates the whole grid before the first query,
// so the overflow is an allocation rather than a slow answer. Kept in seconds
// rather than as a time.Duration: a deployment free to configure maxBuckets is
// free to configure one that overflows a Duration, and a bound that wraps is
// worse than none.
//
// Zero means unbounded, which is chooseGrid's convention for the same argument
// and reachable only past Service.New, which gives MaxBuckets a default.
func maxAlignableSeconds(maxBuckets int) float64 {
	if maxBuckets <= 0 {
		return 0
	}
	return float64(maxBuckets) * gridLadder[len(gridLadder)-1]
}

func nextGrid(current float64) (float64, bool) {
	for _, step := range gridLadder {
		if step > current {
			return step, true
		}
	}
	return current, false
}

// Align reads every member onto one grid with one batched query (§5.5).
//
// Members of the same device and service share an element, because /queries/v2
// takes several columns per element and the profiler's service-scoped batch
// (§5.4.1) is the same economy applied one level down. Elements for different
// devices carry an identical groupTime, which is what makes their buckets line up.
//
// Values are then indexed onto the grid by bucket floor rather than by row
// position. Two elements can legitimately return different row counts — a device
// that was offline for a week has no rows for it — and pairing them positionally
// would slide one member's week against another's.
func (s *Service) Align(
	ctx context.Context, token string, requests []alignRequest, window profiler.Window, gridSeconds float64,
) (AlignedFrame, error) {
	if len(requests) == 0 {
		return AlignedFrame{}, fmt.Errorf("%w: no series to align", ErrInvalidRequest)
	}
	if !window.Valid() {
		return AlignedFrame{}, fmt.Errorf("%w: the window must name a from before its to", ErrInvalidRequest)
	}
	if gridSeconds <= 0 {
		return AlignedFrame{}, fmt.Errorf("%w: the grid must be a positive number of seconds", ErrInvalidRequest)
	}

	groupTime := timeseries.FormatBucket(time.Duration(gridSeconds) * time.Second)
	frame := AlignedFrame{
		Window:      window,
		GroupTime:   groupTime,
		GridSeconds: gridSeconds,
		Times:       grid(window, gridSeconds),
		Columns:     make([]AlignedColumn, len(requests)),
		Notes:       []string{},
	}

	// One element per (device, service), with one column per member of it. The
	// aggregate is per column, so a counter and an instantaneous variable of the same
	// service still share the element.
	type elementKey struct{ device, service string }
	order := []elementKey{}
	byKey := map[elementKey][]int{}
	for i, request := range requests {
		key := elementKey{request.Ref.DeviceID, request.Ref.ServiceID}
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], i)
	}

	elements := make([]timeseries.QueryElement, 0, len(order))
	for _, key := range order {
		device, service := key.device, key.service
		bucket := groupTime
		columns := make([]timeseries.QueryColumn, 0, len(byKey[key]))
		for _, index := range byKey[key] {
			aggregate := groupTypeFor(requests[index].Kind)
			frame.Columns[index] = AlignedColumn{
				Ref:       requests[index].Ref,
				Values:    make([]float64, len(frame.Times)),
				Present:   make([]bool, len(frame.Times)),
				GroupType: aggregate,
			}
			columns = append(columns, timeseries.QueryColumn{
				Name:      requests[index].Ref.VariablePath,
				GroupType: &aggregate,
			})
		}
		elements = append(elements, timeseries.QueryElement{
			DeviceId:  &device,
			ServiceId: &service,
			Columns:   columns,
			GroupTime: &bucket,
			Time: &timeseries.QueryTime{
				Start: stringPtr(window.From.UTC().Format(time.RFC3339)),
				End:   stringPtr(window.To.UTC().Format(time.RFC3339)),
			},
		})
	}

	results, err := s.deps.Timeseries.Query(ctx, token, elements,
		timeseries.QueryOptions{Timeout: s.deps.ReadTimeout})
	frame.Reads = 1
	if err != nil {
		return AlignedFrame{}, err
	}
	sets, err := timeseries.DecodeResults(elements, results, "")
	if err != nil {
		return AlignedFrame{}, err
	}

	// A set carries the index of the element it answers, which is how a column is
	// found again. Two members of one service with the same path would be
	// indistinguishable here, which is why Service.validate drops a duplicate
	// reference before anything is read.
	for _, set := range sets {
		if set.RequestIndex < 0 || set.RequestIndex >= len(order) {
			continue
		}
		for _, index := range byKey[order[set.RequestIndex]] {
			column, found := set.Column(requests[index].Ref.VariablePath)
			if !found {
				continue
			}
			times, values, _ := column.Numeric()
			frame.Columns[index] = fill(frame.Columns[index], frame.Times, window, gridSeconds, times, values)
		}
	}

	for i := range frame.Columns {
		if frame.Columns[i].Points == 0 {
			frame.Notes = append(frame.Notes, fmt.Sprintf(
				"%s returned no bucket with a value over this window, so it can take part in no rule",
				requests[i].Ref.String()))
		}
	}
	return frame, nil
}

// groupTypeFor picks the server-side aggregate for a value kind.
//
// difference-mean for a cumulative counter is the point of §5.3.1's insistence
// that transforms are the platform's work: activity in a meter reading is in its
// rate of change, and thresholding the reading itself would call the series active
// from the first bucket the counter passes the threshold in and never idle again.
func groupTypeFor(kind profiler.ValueKind) string {
	if kind == profiler.KindCumulativeCounter {
		return timeseries.GroupDiffMean
	}
	return timeseries.GroupMean
}

// grid builds the bucket start times, half-open on the window's end.
func grid(window profiler.Window, seconds float64) []time.Time {
	step := time.Duration(seconds) * time.Second
	if step <= 0 {
		return []time.Time{}
	}
	// Anchored on the window rather than on the epoch, so that two passes over the
	// same window produce the same buckets regardless of when they were run.
	count := int(math.Ceil(window.Duration().Seconds() / seconds))
	if count < 0 {
		count = 0
	}
	out := make([]time.Time, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, window.From.UTC().Add(time.Duration(i)*step))
	}
	return out
}

// fill indexes one column's points onto the grid by bucket floor.
//
// Where two points land in the same bucket the later one wins rather than being
// averaged. The server has already aggregated to this bucket width, so a second
// point in one bucket means the boundaries were not quite what was asked for, and
// averaging a boundary artefact would be inventing a value.
func fill(
	column AlignedColumn, times []time.Time, window profiler.Window, seconds float64,
	pointTimes []time.Time, values []float64,
) AlignedColumn {
	for i, at := range pointTimes {
		if i >= len(values) {
			break
		}
		offset := at.UTC().Sub(window.From.UTC()).Seconds()
		if offset < 0 {
			continue
		}
		index := int(offset / seconds)
		if index < 0 || index >= len(times) {
			continue
		}
		if !column.Present[index] {
			column.Points++
		}
		column.Values[index] = values[i]
		column.Present[index] = true
	}
	return column
}

// sortedIntervals is the members' sampling intervals, ascending, for the grid
// choice and for the note that explains it.
func sortedIntervals(intervals []float64) []float64 {
	out := append([]float64{}, intervals...)
	sort.Float64s(out)
	return out
}

func stringPtr(s string) *string { return &s }
