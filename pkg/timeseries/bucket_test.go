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
	"testing"
	"time"

	twmodel "github.com/SENERGY-Platform/timescale-wrapper/pkg/model"
)

func TestBucketWidensUntilTheWindowFits(t *testing.T) {
	week := 7 * 24 * time.Hour

	if bucket, widened := Bucket("", week, 200); bucket != "1h" || widened {
		t.Errorf("a week at 200 points = %q (widened %v), want 1h derived rather than widened", bucket, widened)
	}
	if bucket, widened := Bucket("15m", week, 2000); bucket != "15m" || widened {
		t.Errorf("a request that fits was changed to %q", bucket)
	}
	if bucket, widened := Bucket("1m", week, 200); bucket == "1m" || !widened {
		t.Errorf("a request that does not fit stood: %q (widened %v)", bucket, widened)
	}
	// A form the server accepts and Go's parser does not is passed through: the
	// server validates it, and refusing here would reject a legal bucket.
	if bucket, _ := Bucket("1day", week, 10); bucket != "1day" {
		t.Errorf("bucket = %q, want the request passed through", bucket)
	}
}

// The bucket has to be a form the shared schema accepts, or a chart read fails as
// a bare 400 from the platform.
func TestEveryDerivedBucketIsAcceptedByTheSharedSchema(t *testing.T) {
	spans := []time.Duration{
		time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 90 * 24 * time.Hour, 3 * 365 * 24 * time.Hour,
	}
	device, service := "d", "urn:infai:ses:service:11111111-2222-3333-4444-555555555555"
	for _, span := range spans {
		for _, cap := range []int{100, 500, 2000} {
			bucket, _ := Bucket("", span, cap)
			aggregate := GroupMean
			element := twmodel.QueriesRequestElement{
				DeviceId: &device, ServiceId: &service, GroupTime: &bucket,
				Columns: []twmodel.QueriesRequestElementColumn{{Name: "value.power", GroupType: &aggregate}},
			}
			if !element.Valid() {
				t.Errorf("bucket %q derived for %s at %d points is rejected by the platform schema",
					bucket, span, cap)
			}
		}
	}
}

func TestFormatBucketKeepsSubMinuteLengths(t *testing.T) {
	cases := map[time.Duration]string{
		90 * time.Second: "90s",
		30 * time.Second: "30s",
		15 * time.Minute: "15m",
		2 * time.Hour:    "2h",
		48 * time.Hour:   "2d",
	}
	for input, want := range cases {
		if got := FormatBucket(input); got != want {
			t.Errorf("FormatBucket(%s) = %q, want %q", input, got, want)
		}
	}
}

// BucketSeconds answers zero for a calendar-dependent bucket rather than guessing a
// length, because the caller that needs a divisor has to refuse instead.
func TestBucketSecondsRefusesCalendarBuckets(t *testing.T) {
	cases := map[string]float64{
		"15m":  900,
		"1h":   3600,
		"90s":  90,
		"1d":   86400,
		"1day": 86400,
		"1w":   604800,
		"1mon": 0,
		"1y":   0,
		"":     0,
	}
	for input, want := range cases {
		if got := BucketSeconds(input); got != want {
			t.Errorf("BucketSeconds(%q) = %g, want %g", input, got, want)
		}
	}
}

func TestValidGroupTypeAgreesWithThePlatform(t *testing.T) {
	device, service := "d", "urn:infai:ses:service:11111111-2222-3333-4444-555555555555"
	bucket := "1h"
	for _, groupType := range []string{
		GroupMean, GroupSum, GroupCount, GroupMedian, GroupMin, GroupMax,
		GroupFirst, GroupLast, GroupDiffMean, GroupDiffSum, GroupDiffLast, GroupTWMean,
	} {
		if !ValidGroupType(groupType) {
			t.Errorf("%q is offered by ODE and rejected by its own check", groupType)
		}
		aggregate := groupType
		element := twmodel.QueriesRequestElement{
			DeviceId: &device, ServiceId: &service, GroupTime: &bucket,
			Columns: []twmodel.QueriesRequestElementColumn{{Name: "value.power", GroupType: &aggregate}},
		}
		if !element.Valid() {
			t.Errorf("%q passes ODE's check and is rejected by the platform schema", groupType)
		}
	}
	if ValidGroupType("difference-stddev") {
		t.Error("an aggregate the platform does not have passed the check")
	}
}
