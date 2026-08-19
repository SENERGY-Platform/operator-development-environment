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

package timeseries

import (
	"fmt"
	"time"
)

// Bucket widens an aggregation bucket until a window fits a point cap.
//
// Returning fewer points by truncating would be worse than widening: a truncated
// read shows the first fraction of the window and looks like the whole of it,
// which is the misreading neither a model (§4) nor a chart axis can recover from.
//
// It lives here rather than beside either caller because both the tier-L2 preview
// (§5.8) and the exploration pane's chart reads (§5.9) have to make the same
// decision, and two ladders would drift into two different pictures of the same
// series.
func Bucket(requested string, span time.Duration, maxPoints int) (bucket string, widened bool) {
	if span <= 0 || maxPoints <= 0 {
		if requested != "" {
			return requested, false
		}
		return "1h", false
	}

	if requested != "" {
		if parsed, err := time.ParseDuration(requested); err == nil && parsed > 0 {
			if int(span/parsed) <= maxPoints {
				return requested, false
			}
		} else {
			// An unparseable bucket is not rejected: timescale-wrapper accepts forms
			// Go's parser does not (a day, for instance). It is passed through and
			// the point cap still applies on decode.
			return requested, false
		}
	}

	// The smallest bucket from a conventional ladder that fits the cap. A ladder
	// rather than span/maxPoints because "17m23s" is a legal bucket and a useless
	// one to read on an axis.
	ladder := []time.Duration{
		time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
		time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
		24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour,
	}
	for _, candidate := range ladder {
		if int(span/candidate) <= maxPoints {
			return FormatBucket(candidate), requested != ""
		}
	}
	return FormatBucket(ladder[len(ladder)-1]), requested != ""
}

// FormatBucket renders a duration in the form timeIntervalValid accepts.
//
// The seconds case is not decoration: a chart spec may ask to resample to 90s,
// and rendering that as whole minutes would silently double the bucket.
func FormatBucket(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%time.Minute != 0:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// BucketSeconds reads a bucket back as seconds, for the arithmetic a rate needs.
//
// Zero means "not expressible as a duration here", which is a legitimate answer:
// the server accepts `1mon` and `1y`, whose length depends on where in the
// calendar the bucket falls. A caller that needs a divisor has to refuse rather
// than pick one.
func BucketSeconds(bucket string) float64 {
	if parsed, err := time.ParseDuration(bucket); err == nil && parsed > 0 {
		return parsed.Seconds()
	}
	// The forms timescale-wrapper accepts and Go's parser does not.
	var value int
	var unit string
	if n, err := fmt.Sscanf(bucket, "%d%s", &value, &unit); n != 2 || err != nil || value <= 0 {
		return 0
	}
	switch unit {
	case "d", "day":
		return float64(value) * 24 * 60 * 60
	case "w":
		return float64(value) * 7 * 24 * 60 * 60
	default:
		return 0
	}
}

// ValidGroupType answers whether the server's allow-list accepts an aggregate.
//
// The authoritative list is QueriesRequestElementColumn.Valid in the shared
// model; this is the subset ODE offers, all of which that list accepts. Checking
// here means a wrong aggregate is named as such instead of arriving as a bare 400.
func ValidGroupType(groupType string) bool {
	switch groupType {
	case GroupMean, GroupSum, GroupCount, GroupMedian, GroupMin, GroupMax,
		GroupFirst, GroupLast, GroupDiffMean, GroupDiffSum, GroupDiffLast, GroupTWMean:
		return true
	}
	return false
}
