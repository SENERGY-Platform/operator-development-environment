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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

const (
	testDeviceID  = "urn:infai:ses:device:1"
	testServiceID = "urn:infai:ses:service:11111111-1111-1111-1111-111111111111"
	powerPath     = "value.power"
	totalPath     = "value.total"
)

var (
	computeNow      = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	analysisFrom    = computeNow.Add(-90 * 24 * time.Hour)
	rawFrom         = computeNow.Add(-14 * 24 * time.Hour)
	testColumnNames = []string{powerPath, totalPath}
)

// meterRawSeries is a fortnight of quarter-hourly readings: instantaneous power
// with a daily shape, and a cumulative watt-second counter that integrates it.
func meterRawSeries() (times []time.Time, power, total []float64) {
	accumulated := 0.0
	for at := rawFrom; at.Before(computeNow); at = at.Add(quarterHour) {
		hours := float64(at.Unix()%86400) / 3600
		watts := 400 + 300*math.Sin(2*math.Pi*hours/24)
		times = append(times, at)
		power = append(power, watts)
		total = append(total, accumulated)
		accumulated += watts * quarterHour.Seconds()
	}
	return times, power, total
}

// meterFixture is that fortnight of raw readings plus ninety days of hourly
// buckets over the same service.
func meterFixture() *fakeTimeseries {
	rawTimes, power, total := meterRawSeries()

	bucketTimes := []time.Time{}
	means := []float64{}
	mins := []float64{}
	maxes := []float64{}
	bucketTotals := []float64{}
	runningTotal := 0.0
	for at := analysisFrom; at.Before(computeNow); at = at.Add(time.Hour) {
		hours := float64(at.Unix()%86400) / 3600
		watts := 400 + 300*math.Sin(2*math.Pi*hours/24)
		bucketTimes = append(bucketTimes, at)
		means = append(means, watts)
		mins = append(mins, watts-20)
		maxes = append(maxes, watts+20)
		bucketTotals = append(bucketTotals, runningTotal)
		runningTotal += watts * 3600
	}

	from, to := analysisFrom, computeNow
	return &fakeTimeseries{
		availability: map[string][]timeseries.Availability{
			testDeviceID: availabilityWindow(testServiceID, from, to, "1h"),
		},
		usage: map[string]timeseries.Usage{
			testDeviceID: {DeviceId: testDeviceID, Bytes: 1 << 22, BytesPerDay: 12000},
		},
		results: [][]timeseries.QueryResult{
			// The raw pass: one element, all columns, no bucketing.
			{queryResult(0, testDeviceID, testServiceID, testColumnNames, rawTimes,
				map[string][]float64{powerPath: power, totalPath: total})},
			// The aggregated pass: mean, minimum and maximum buckets, in that order.
			{
				queryResult(0, testDeviceID, testServiceID, testColumnNames, bucketTimes,
					map[string][]float64{powerPath: means, totalPath: bucketTotals}),
				queryResult(1, testDeviceID, testServiceID, testColumnNames, bucketTimes,
					map[string][]float64{powerPath: mins, totalPath: bucketTotals}),
				queryResult(2, testDeviceID, testServiceID, testColumnNames, bucketTimes,
					map[string][]float64{powerPath: maxes, totalPath: bucketTotals}),
			},
		},
	}
}

func profileMeter(t *testing.T, fake *fakeTimeseries) (*Profiler, ProfileResult) {
	t.Helper()
	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("ProfileService: %v", err)
	}
	return prof, result
}

func profileFor(t *testing.T, result ProfileResult, path string) ResolvedProfile {
	t.Helper()
	for _, profile := range result.Profiles {
		if profile.SeriesRef.VariablePath == path {
			return profile
		}
	}
	t.Fatalf("no profile for %s", path)
	return ResolvedProfile{}
}

// A failing availability probe must not take the profile with it.
//
// The endpoint derives its answer by parsing view definitions and fails a whole
// device on one aggregate it cannot parse, which is a fault in metadata ODE only
// reads — while the reads that carry a profile are POST /queries/v2 and are
// unaffected. QuickProfile has always tolerated this; the profile pass used to
// refuse, which made one unparsable view enough to make a device unprofilable.
func TestAFailingAvailabilityProbeDoesNotStopAProfile(t *testing.T) {
	fake := meterFixture()
	fake.availErr = errors.New("timescale-wrapper returned 500: unexpected type matches from view description")

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	requested := Window{From: computeNow.Add(-7 * 24 * time.Hour), To: computeNow}
	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:         meterDevice(testDeviceID, testServiceID),
		ServiceID:      testServiceID,
		AnalysisWindow: requested,
	})
	if err != nil {
		t.Fatalf("ProfileService: %v", err)
	}

	if !result.AnalysisWindow.From.Equal(requested.From) || !result.AnalysisWindow.To.Equal(requested.To) {
		t.Errorf("analysis window = %s, want the requested %s — there is nothing to intersect it with",
			result.AnalysisWindow.String(), requested.String())
	}

	summary := profileFor(t, result, powerPath).ReadSummary
	available, known := summary.RawAvailable.Get()
	if known {
		t.Errorf("raw_available = %v as a computed value; the probe failed, so it is not known (D24)", available)
	}
	status := summary.RawAvailable.Status()
	if status.Reason != ReasonReadFailed {
		t.Errorf("reason = %q, want %q", status.Reason, ReasonReadFailed)
	}
	if !strings.Contains(status.Detail, "unexpected type matches") {
		t.Errorf("detail = %q, want the platform's own error in it", status.Detail)
	}

	// And the two reads that matter did happen, which is the whole point.
	if result.Reads.Values != 2 {
		t.Errorf("value reads = %d, want the raw and the aggregated pass", result.Reads.Values)
	}
	if summary.RawRows == 0 || summary.AggregatedBuckets == 0 {
		t.Errorf("summary = %+v, want both passes populated", summary)
	}
}

// Without a window there is no range to profile over: the default lookback is
// anchored on the end of the *available* data, and anchoring it on nothing would
// invent a range and then report profiles computed over it as though it had been
// chosen (D25).
func TestAFailingAvailabilityProbeStillNeedsAWindow(t *testing.T) {
	fake := meterFixture()
	fake.availErr = errors.New("timescale-wrapper returned 500: unexpected type matches from view description")

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "unexpected type matches") {
		t.Errorf("error = %v, want the upstream cause named in it", err)
	}
	if len(fake.queries) != 0 {
		t.Error("values were read although there was no window to read them over")
	}
}

// The row cap has to bound the *response*, which costs rows times variables.
//
// A raw read is one wide SELECT: one row per message, one value per variable. A cap
// applied to rows therefore bounds nothing the gateway between ODE and the platform
// measures — an eleven-variable meter at a hundred thousand rows is over a million
// values in one body — and that is what a 502 with "an invalid response was received
// from the upstream server" is.
func TestTheRawRowLimitDividesByTheVariablesRead(t *testing.T) {
	prof := newTestProfiler(t, meterFixture(), powerOntology(), computeNow)

	// One column is the case the arithmetic was always right for, which is why it
	// went unnoticed.
	if got := prof.rawRowLimit(1); got != defaultRawWindowPoints {
		t.Errorf("one variable = %d rows, want the configured %d", got, defaultRawWindowPoints)
	}
	if got := prof.rawRowLimit(10); got != defaultRawWindowPoints/10 {
		t.Errorf("ten variables = %d rows, want %d so the response stays inside the cap",
			got, defaultRawWindowPoints/10)
	}
	// The floor holds: a service wide enough to divide the cap into nothing still has
	// to hand the structural detectors a usable run of consecutive messages.
	if got := prof.rawRowLimit(500); got != minRawRowLimit {
		t.Errorf("five hundred variables = %d rows, want the floor of %d", got, minRawRowLimit)
	}
	for _, variables := range []int{1, 2, 10, 500} {
		if got := prof.rawRowLimit(variables); got > defaultRawWindowPoints {
			t.Errorf("%d variables = %d rows, above the configured cap", variables, got)
		}
	}
}

// The applied limit reaches the profile, because a raw window shorter than the one
// configured has to be explicable without reading the source.
func TestTheProfileRecordsTheRowLimitItWasReadWith(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)

	want := defaultRawWindowPoints / 2 // the meter fixture has two variables
	if result.RawWindow.RowLimit != want {
		t.Errorf("row_limit = %d, want %d", result.RawWindow.RowLimit, want)
	}
	if result.RawWindow.LimitReduced {
		t.Error("limit_reduced is set although nothing was refused")
	}
	if got := profileFor(t, result, powerPath).RawWindow.RowLimit; got != want {
		t.Errorf("the stored profile records %d, want %d", got, want)
	}

	// And the request carried exactly that.
	element := fake.queries[0][0]
	if element.Limit == nil || *element.Limit != want {
		t.Errorf("the raw request asked for %v rows, want %d", element.Limit, want)
	}
}

// A gateway refusal is retried once with half the rows, and the fact is recorded:
// a short raw window then has a third possible cause, and guessing between it,
// retention and the developer's own setting is what D24 exists to prevent.
func TestAGatewayRefusalIsRetriedWithFewerRows(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusBadGateway,
		Err: errors.New("An invalid response was received from the upstream server"),
	}
	fake.queryErrCall = 1

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("the retry did not save the profile: %v", err)
	}
	if !result.RawWindow.LimitReduced {
		t.Error("limit_reduced is not set, so the shortened read has no visible cause")
	}
	want := defaultRawWindowPoints / 2 / 2
	if result.RawWindow.RowLimit != want {
		t.Errorf("row_limit = %d, want %d after halving", result.RawWindow.RowLimit, want)
	}
	if len(fake.queries) < 2 {
		t.Fatalf("%d queries, want the refused raw read and its retry", len(fake.queries))
	}
	if fake.queries[1][0].Limit == nil || *fake.queries[1][0].Limit != want {
		t.Errorf("the retry asked for %v rows, want %d", fake.queries[1][0].Limit, want)
	}
}

// Once, not twice: a second refusal is the platform saying no rather than a size to
// negotiate down, and retrying again only costs the same expensive read.
func TestAGatewayRefusalIsRetriedOnlyOnce(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusBadGateway, Err: errors.New("refused again"),
	}

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err == nil {
		t.Fatal("no error after two refusals")
	}
	if len(fake.queries) != 2 {
		t.Errorf("%d raw attempts, want exactly two", len(fake.queries))
	}
	if !strings.Contains(err.Error(), "raw pass") {
		t.Errorf("error = %q, want the pass named", err)
	}
}

// The incident these two tests come from, in the shape the field logs recorded it:
// /data-availability answered 502 and /queries/v2 answered 502 thirty-four
// milliseconds later. No volume of rows explains a 502 from an endpoint that returns
// a handful of metadata rows, so the size hypothesis is wrong — and telling a
// developer to narrow their window while the service is unwell sends them turning a
// knob that cannot help.
func TestAGatewayRefusalIsNotBlamedOnSizeWhenTheProbeFailedTheSameWay(t *testing.T) {
	fake := meterFixture()
	// Both endpoints refused at the gateway, which is what the field logs showed.
	fake.availErr = &timeseries.UpstreamError{
		Resource: "/data-availability", Code: http.StatusBadGateway,
		Err:     errors.New("An invalid response was received from the upstream server"),
		Elapsed: 20 * time.Millisecond,
	}
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusBadGateway,
		Err:     errors.New(`{"message":"An invalid response was received from the upstream server"}`),
		Elapsed: 34 * time.Millisecond,
	}

	// An explicit window, because that is the only way the pass gets this far once the
	// probe has failed — and it is what the field caller did: the log line reads
	// "profiling over the requested window instead".
	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:         meterDevice(testDeviceID, testServiceID),
		ServiceID:      testServiceID,
		AnalysisWindow: Window{From: analysisFrom, To: computeNow},
	})
	if err == nil {
		t.Fatal("no error after both attempts failed")
	}

	message := err.Error()
	// The pass and the bound are still named: that part was never the problem.
	if !strings.Contains(message, "raw pass") {
		t.Errorf("error = %q, want the pass named", message)
	}
	// But the size levers must not be offered, because they cannot fix this.
	if strings.Contains(message, "narrow the raw window") ||
		strings.Contains(message, "profiler_raw_window_points") {
		t.Errorf("error = %q, want no size levers: the availability probe returned 502 too, "+
			"and its response cannot be too large", message)
	}
	if !strings.Contains(message, "Asking for less will not help") {
		t.Errorf("error = %q, want it to say the size remedy does not apply", message)
	}
	if !strings.Contains(message, "availability probe failed the same way") {
		t.Errorf("error = %q, want the evidence it went on", message)
	}

	// The retry still happens — one read, and a transient fault may have passed — so
	// the behaviour is unchanged and only the diagnosis is honest.
	if len(fake.queries) != 2 {
		t.Errorf("%d raw attempts, want the read and its one retry", len(fake.queries))
	}
}

// "4 variable(s)" is not enough to act on when one service fails and others succeed.
// The column the upstream choked on is in the list ODE sent, so the failure names them
// with their declared types.
func TestAFailedRawPassNamesTheColumnsItAskedFor(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusBadGateway,
		Err:     errors.New("An invalid response was received from the upstream server"),
		Elapsed: 58 * time.Millisecond,
	}

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err == nil {
		t.Fatal("no error")
	}
	message := err.Error()
	for _, want := range []string{powerPath, totalPath} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to name the column %q", message, want)
		}
	}
	// The declared type comes with it: a column the query builder cannot handle is
	// usually recognisable by its type. Trimmed to the readable part — the model
	// carries it as a schema.org URI.
	if !strings.Contains(message, "(Float)") {
		t.Errorf("error = %q, want the declared types beside the columns", message)
	}
	if strings.Contains(message, "schema.org") {
		t.Errorf("error = %q, want the type trimmed rather than the whole URI", message)
	}
}

// A wide service must not turn one failure into an unreadable log line.
func TestTheColumnListInAFailureIsBounded(t *testing.T) {
	wide := make([]Variable, 0, 20)
	for i := 0; i < 20; i++ {
		wide = append(wide, Variable{Path: fmt.Sprintf("value.c%d", i), Type: models.Float})
	}

	described := describeColumns(wide)
	if !strings.Contains(described, "and 12 more") {
		t.Errorf("described = %q, want the elision counted", described)
	}
	if strings.Contains(described, "value.c19") {
		t.Errorf("described = %q, want the tail elided rather than dumped", described)
	}
	// And the short case is complete, with no elision note to read past.
	if got := describeColumns(wide[:2]); got != "value.c0 (Float), value.c1 (Float)" {
		t.Errorf("described = %q, want both columns and no elision", got)
	}
}

// The same judgement without the corroborating probe: a 502 that comes back in
// milliseconds never assembled a response worth refusing.
func TestAnImmediateGatewayFailureIsNotBlamedOnSize(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusBadGateway,
		Err:     errors.New("An invalid response was received from the upstream server"),
		Elapsed: 34 * time.Millisecond,
	}

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "narrow the raw window") {
		t.Errorf("error = %q, want no size levers for a failure that took 34ms", err)
	}
	if !strings.Contains(err.Error(), "too fast") {
		t.Errorf("error = %q, want it to say the failure was too fast to be about the response", err)
	}
}

// And the mirror case, so the fix does not simply suppress the size advice
// everywhere: a slow refusal is exactly what a response too large looks like, and
// there the levers are the useful thing to say.
func TestASlowGatewayRefusalStillOffersTheSizeLevers(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusBadGateway,
		Err:     errors.New("An invalid response was received from the upstream server"),
		Elapsed: 45 * time.Second,
	}

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "narrow the raw window") {
		t.Errorf("error = %q, want the size levers: a 45s refusal is what a response too large "+
			"looks like", err)
	}
}

// A rejected *request* is not retried at all. Halving the rows cannot make a bad
// column name good, and the second read would cost the platform the same work.
func TestARejectedRequestIsNotRetried(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusBadRequest, Err: errors.New("column does not exist"),
	}

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	if _, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	}); err == nil {
		t.Fatal("no error")
	}
	if len(fake.queries) != 1 {
		t.Errorf("%d attempts, want one: a 400 is about the request", len(fake.queries))
	}
}

// A read that fails has to say which pass failed and what it had asked for.
//
// The bare upstream text is unactionable: a gateway saying "an invalid response was
// received from the upstream server" names neither the pass nor the request that
// provoked it, and the raw pass is the one whose response grows with rows times
// variables. This is the error a developer pastes into a bug report, so it has to
// carry the two levers that make it go away.
func TestAFailedRawPassNamesThePassAndTheBound(t *testing.T) {
	fake := meterFixture()
	// Every attempt fails, retry included: this test is about what the caller is
	// told once there is nothing left to try.
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2",
		Code:     http.StatusBadGateway,
		Err:      errors.New(`{"message":"An invalid response was received from the upstream server"}`),
	}

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err == nil {
		t.Fatal("a failed raw pass returned no error; the structural detectors have nothing to run on")
	}

	message := err.Error()
	for _, want := range []string{"raw pass", "2 variable", "row limit", "values in one response"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to contain %q", message, want)
		}
	}
	// The hint belongs on the codes that mean the response was the problem, and it
	// has to name what a developer can actually change.
	if !strings.Contains(message, "narrow the raw window") ||
		!strings.Contains(message, "profiler_raw_window_points") {
		t.Errorf("error = %q, want the two levers named", message)
	}
	// And the platform's own words survive, because they are what identifies the
	// failure upstream.
	if !strings.Contains(message, "invalid response was received") {
		t.Errorf("error = %q, want the upstream body kept", message)
	}
	var upstream *timeseries.UpstreamError
	if !errors.As(err, &upstream) {
		t.Error("the upstream error is no longer unwrappable, so the API layer cannot classify it")
	}
}

// A wrapper 500 is about the service or the request, not about the size of the
// answer, so it must not carry advice about making the response smaller.
func TestAFailedRawPassOnlyAdvisesShrinkingForGatewayFailures(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2",
		Code:     http.StatusInternalServerError,
		Err:      errors.New("column does not exist"),
	}
	fake.queryErrCall = 1

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "narrow the raw window") {
		t.Errorf("error = %q, want no size advice for a 500: the response was not the problem", err)
	}
	if !strings.Contains(err.Error(), "raw pass") {
		t.Errorf("error = %q, want the pass named even so", err)
	}
}

// The aggregated pass is the milder half and stays non-fatal: its fields report
// read_failed and the structural detectors still have the raw pass.
func TestAFailedAggregatedPassStillYieldsAProfile(t *testing.T) {
	fake := meterFixture()
	fake.queryErr = &timeseries.UpstreamError{
		Resource: "/queries/v2", Code: http.StatusGatewayTimeout, Err: errors.New("upstream timed out"),
	}
	fake.queryErrCall = 2

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("a failed aggregated pass took the whole profile with it: %v", err)
	}
	profile := profileFor(t, result, powerPath)
	if profile.ReadSummary.RawRows == 0 {
		t.Error("the raw pass produced nothing, so this proves nothing about the aggregated one")
	}
	if profile.ReadSummary.AggregatedBuckets != 0 {
		t.Errorf("aggregated_buckets = %d, want 0", profile.ReadSummary.AggregatedBuckets)
	}
	// Periodicity is the field that needs the aggregated pass and has no fallback, so
	// it is the one that has to report a non-result rather than "no periodicity"
	// (D24). The distribution deliberately does fall back to the raw window.
	if profile.TemporalStructure.DominantPeriodsS.IsComputed() {
		t.Error("periods were computed although the aggregated pass returned nothing")
	}
	if !profile.Distribution.IsComputed() {
		t.Error("the distribution did not fall back to the raw window, which detect.go says it should")
	}
}

// One profile per variable, from one batched read per pass (D19, §5.4.1).
func TestOneServiceReadYieldsOneProfilePerVariable(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)

	if len(result.Profiles) != 2 {
		t.Fatalf("profiles = %d, want one per variable", len(result.Profiles))
	}
	if result.Reads.Values != 2 {
		t.Errorf("value reads = %d, want two: one raw pass and one aggregated pass", result.Reads.Values)
	}
	if len(fake.queries) != 2 {
		t.Fatalf("queries = %d, want two batched calls", len(fake.queries))
	}
}

// §5.3.2: the structural detectors need unbucketed data, because groupTime fills
// or smooths every bucket and both the irregularity and the gaps disappear.
func TestTheRawPassIsUnbucketedBoundedAndTakesTheNewestPoints(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)

	raw := fake.queries[0]
	if len(raw) != 1 {
		t.Fatalf("raw pass sent %d elements, want one covering every column", len(raw))
	}
	if raw[0].GroupTime != nil {
		t.Errorf("groupTime = %v, want none on the raw pass", *raw[0].GroupTime)
	}
	// The point bound divided by the two variables read: the response carries one
	// value per variable per row, so bounding rows alone would bound nothing.
	if want := defaultRawWindowPoints / 2; raw[0].Limit == nil || *raw[0].Limit != want {
		t.Errorf("limit = %v, want %d — the point bound over the variables read",
			derefInt(raw[0].Limit), want)
	}
	// Descending with a limit takes the newest points; the window is anchored at
	// recent data and truncating from the far end would read the wrong fortnight.
	if raw[0].OrderDirection == nil || *raw[0].OrderDirection != timeseries.OrderDescending {
		t.Errorf("order direction = %v, want descending", raw[0].OrderDirection)
	}
	if len(raw[0].Columns) != 2 {
		t.Errorf("columns = %d, want every variable in one element", len(raw[0].Columns))
	}
	// D25: the default raw window is the smaller of fourteen days or the point
	// bound, anchored at the most recent data.
	if got := result.RawWindow.SpanDays(); math.Abs(got-14) > 0.01 {
		t.Errorf("raw window spans %.2f days, want 14", got)
	}
	if result.RawWindow.Source != WindowDefault {
		t.Errorf("raw window source = %s, want default", result.RawWindow.Source)
	}
}

// Three elements rather than three columns of one name, so mean, minimum and
// maximum each arrive under an unambiguous column name.
func TestTheAggregatedPassAsksForMeanMinimumAndMaximumAtOneBucketWidth(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)

	aggregated := fake.queries[1]
	if len(aggregated) != 3 {
		t.Fatalf("aggregated pass sent %d elements, want three", len(aggregated))
	}
	wanted := []string{timeseries.GroupMean, timeseries.GroupMin, timeseries.GroupMax}
	for i, element := range aggregated {
		if element.GroupTime == nil || *element.GroupTime != result.GroupTime {
			t.Errorf("element %d groupTime = %v, want %s for all elements so the series align",
				i, element.GroupTime, result.GroupTime)
		}
		for _, col := range element.Columns {
			if col.GroupType == nil || *col.GroupType != wanted[i] {
				t.Errorf("element %d column %s groupType = %v, want %s", i, col.Name, col.GroupType, wanted[i])
			}
		}
	}
	// The bucket is never finer than the sampling interval and coarse enough to
	// keep the window's bucket count sane.
	if result.GroupTime != "1h" {
		t.Errorf("group time = %s, want 1h for ninety days of quarter-hourly data", result.GroupTime)
	}
}

func TestSamplingAndCoverageComeFromTheRawWindow(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)
	profile := profileFor(t, result, powerPath)

	sampling := mustGet(t, profile.Sampling, "sampling")
	if sampling.DetectedIntervalS != 900 {
		t.Errorf("interval = %v, want 900", sampling.DetectedIntervalS)
	}
	if sampling.Regularity != Regular {
		t.Errorf("regularity = %s, want regular", sampling.Regularity)
	}

	coverage := mustGet(t, profile.Coverage, "coverage")
	if coverage.CompletenessRatio < 0.99 {
		t.Errorf("completeness = %v, want a complete fortnight", coverage.CompletenessRatio)
	}

	// D22: every field records which pass produced it.
	if entry := profile.Provenance[FieldSamplingGaps]; entry.ReadMode != ReadRaw {
		t.Errorf("gaps read_mode = %s, want raw", entry.ReadMode)
	}
	if entry := profile.Provenance[FieldPeriods]; entry.ReadMode != ReadAggregated {
		t.Errorf("periods read_mode = %s, want aggregated", entry.ReadMode)
	}
	if entry := profile.Provenance[FieldPeriods]; entry.GroupTime != result.GroupTime {
		t.Errorf("periods group_time = %q, want %q: a period below the bucket cannot have been seen",
			entry.GroupTime, result.GroupTime)
	}
	if entry := profile.Provenance[FieldUnit]; entry.ReadMode != ReadNone || entry.Source != SourceOntology {
		t.Errorf("unit provenance = %+v, want an ontology source with no read", entry)
	}
}

func TestTheDailyShapeIsFoundOverTheAnalysisWindow(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)
	profile := profileFor(t, result, powerPath)

	periods := mustGet(t, profile.TemporalStructure.DominantPeriodsS, "dominant periods")
	daily := false
	for _, period := range periods {
		if math.Abs(period-86400) < 3600 {
			daily = true
		}
	}
	if !daily {
		t.Errorf("periods = %v, want the daily cycle", periods)
	}
}

// M1b acceptance: counter versus instantaneous, and units from characteristics.
func TestTheCounterAndThePowerChannelAreClassifiedAndUnitsResolve(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)

	power := profileFor(t, result, powerPath)
	if kind := mustGet(t, power.ValueSemantics.Kind, "power kind"); kind != KindInstantaneous {
		t.Errorf("power kind = %s, want instantaneous", kind)
	}
	if power.ValueSemantics.Unit != "W" {
		t.Errorf("power unit = %q, want W", power.ValueSemantics.Unit)
	}
	if power.ValueSemantics.UnitSource != UnitFromCharacteristic {
		t.Errorf("power unit_source = %s, want characteristic", power.ValueSemantics.UnitSource)
	}

	total := profileFor(t, result, totalPath)
	if kind := mustGet(t, total.ValueSemantics.Kind, "total kind"); kind != KindCumulativeCounter {
		t.Errorf("total kind = %s, want cumulative_counter", kind)
	}
	if total.ValueSemantics.Unit != "Wh" {
		t.Errorf("total unit = %q, want Wh", total.ValueSemantics.Unit)
	}
}

// The check the service-scoped batch exists for (§5.4.1), and it costs no extra
// read.
func TestTheCounterIsRelatedToItsPowerSiblingFromTheSameRead(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)
	total := profileFor(t, result, totalPath)

	if len(total.ServiceContext.SiblingVariables) != 1 {
		t.Fatalf("siblings = %+v, want the power channel", total.ServiceContext.SiblingVariables)
	}
	if total.ServiceContext.SiblingVariables[0].Path != powerPath {
		t.Errorf("sibling = %s, want %s", total.ServiceContext.SiblingVariables[0].Path, powerPath)
	}
	if total.ServiceContext.Interaction != "event" {
		t.Errorf("interaction = %s, want event", total.ServiceContext.Interaction)
	}

	found := false
	for _, relationship := range total.ServiceContext.Relationships {
		if relationship.Type == RelationIntegralOf && relationship.OtherPath == powerPath {
			found = true
			if relationship.Evidence.Correlation < strongShapeMatch {
				t.Errorf("correlation = %v, want a strong shape match", relationship.Evidence.Correlation)
			}
		}
	}
	if !found {
		t.Errorf("relationships = %+v, want the counter reported as the integral of power",
			total.ServiceContext.Relationships)
	}
}

func TestARecomputationOverTheSameWindowsReadsNothing(t *testing.T) {
	fake := meterFixture()
	prof, first := profileMeter(t, fake)
	if len(first.FromCache) != 0 {
		t.Fatalf("the first call reported cached profiles: %v", first.FromCache)
	}

	before := len(fake.queries)
	second, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("second ProfileService: %v", err)
	}
	if second.Reads.Values != 0 {
		t.Errorf("value reads = %d on a cache hit, want 0", second.Reads.Values)
	}
	if len(fake.queries) != before {
		t.Errorf("queries went from %d to %d; a cache hit must read nothing", before, len(fake.queries))
	}
	if len(second.FromCache) != 2 {
		t.Errorf("from_cache = %v, want both profiles", second.FromCache)
	}
	if second.Profiles[0].ProfileID != first.Profiles[0].ProfileID {
		t.Error("the cache hit returned a different profile id")
	}
}

// D25: a profile computed over an unusual window must not be mistaken for a
// default one.
func TestADeveloperRawWindowIsRecordedAsAnOverride(t *testing.T) {
	fake := meterFixture()
	prof := newTestProfiler(t, fake, powerOntology(), computeNow)

	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
		RawWindow: Window{From: computeNow.Add(-3 * 24 * time.Hour), To: computeNow},
	})
	if err != nil {
		t.Fatalf("ProfileService: %v", err)
	}
	if result.RawWindow.Source != WindowDeveloperOverride {
		t.Errorf("source = %s, want developer_override", result.RawWindow.Source)
	}
	if got := result.RawWindow.SpanDays(); math.Abs(got-3) > 0.01 {
		t.Errorf("raw window spans %.2f days, want 3", got)
	}
	if result.Profiles[0].RawWindow.Source != WindowDeveloperOverride {
		t.Error("the profile body does not record the override")
	}
}

// When the point limit bites, the recorded window has to be the one actually
// read: leaving the requested start in place would make the missing head look
// like a gap.
func TestATruncatedRawReadNarrowsTheRecordedWindow(t *testing.T) {
	const limit = 100
	fake := meterFixture()

	// What the server returns for a descending read with a limit: the newest
	// `limit` points, not the whole window.
	rawTimes, power, total := meterRawSeries()
	head := len(rawTimes) - limit
	fake.results[0] = []timeseries.QueryResult{queryResult(0, testDeviceID, testServiceID, testColumnNames,
		rawTimes[head:], map[string][]float64{powerPath: power[head:], totalPath: total[head:]})}

	prof, err := New(fake, fakeOntology{index: powerOntology()}, NewMemoryStore(), Options{
		Now:                func() time.Time { return computeNow },
		RawWindowMaxPoints: limit,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("ProfileService: %v", err)
	}
	if !result.RawWindow.Truncated {
		t.Fatal("the raw window is not marked truncated although the fixture exceeds the limit")
	}
	if !result.RawWindow.From.After(rawFrom) {
		t.Errorf("raw window starts at %s, want it narrowed past the requested %s",
			result.RawWindow.From, rawFrom)
	}
}

// The aggregated pass failing costs the statistical fields, not the profile: the
// structural detectors still have the raw pass.
func TestAFailedAggregatedPassLeavesTheStructuralFieldsIntact(t *testing.T) {
	fake := meterFixture()
	// Only the raw pass answers; the aggregated call errors.
	rawOnly := fake.results[0]
	fake.results = [][]timeseries.QueryResult{rawOnly}
	calls := 0
	fake.onQuery = func([]timeseries.QueryElement) {
		calls++
		if calls > 1 {
			fake.queryErr = errors.New("upstream 502")
		}
	}

	prof := newTestProfiler(t, fake, powerOntology(), computeNow)
	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
	})
	if err != nil {
		t.Fatalf("ProfileService: %v", err)
	}

	profile := profileFor(t, result, powerPath)
	if !profile.Sampling.IsComputed() {
		t.Error("sampling is not computed although the raw pass succeeded")
	}
	if profile.TemporalStructure.DominantPeriodsS.IsComputed() {
		t.Error("periods were computed although the aggregated pass failed")
	}
	// The distribution falls back to the raw window rather than vanishing, and the
	// provenance says which window it describes.
	distribution := mustGet(t, profile.Distribution, "distribution")
	if distribution.Max <= 0 {
		t.Errorf("distribution = %+v, want the raw-window fallback", distribution)
	}
	entry := profile.Provenance[FieldDistribution]
	if entry.ReadMode != ReadRaw || entry.Note == "" {
		t.Errorf("distribution provenance = %+v, want raw with a note about the narrower window", entry)
	}
}

func TestAServiceTheDeviceTypeDoesNotHaveIsRefused(t *testing.T) {
	fake := meterFixture()
	prof := newTestProfiler(t, fake, powerOntology(), computeNow)

	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: "urn:infai:ses:service:99999999-9999-9999-9999-999999999999",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
}

// Read governs metadata; Execute governs reading data (§5.1).
func TestProfilingWithoutExecutePermissionIsRefused(t *testing.T) {
	fake := meterFixture()
	prof := newTestProfiler(t, fake, powerOntology(), computeNow)

	device := meterDevice(testDeviceID, testServiceID)
	device.Permissions.Execute = false
	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device: device, ServiceID: testServiceID,
	})
	if !errors.Is(err, ErrNoPermission) {
		t.Fatalf("error = %v, want ErrNoPermission", err)
	}
}

func TestAWindowOutsideTheAvailableDataIsRefused(t *testing.T) {
	fake := meterFixture()
	prof := newTestProfiler(t, fake, powerOntology(), computeNow)

	_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
		AnalysisWindow: Window{
			From: computeNow.Add(10 * 24 * time.Hour),
			To:   computeNow.Add(20 * 24 * time.Hour),
		},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
}

// D28: recommendations are advisory, and nothing downstream reads them.
func TestRecommendationsAreMarkedAdvisory(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)
	profile := profileFor(t, result, powerPath)

	if !profile.Recommendations.Advisory {
		t.Error("recommendations are not marked advisory")
	}
	if resample := mustGet(t, profile.Recommendations.ResampleToS, "resample_to_s"); resample <= 0 {
		t.Errorf("resample_to_s = %v, want a positive bucket", resample)
	}
	// A counter is carried forward, never interpolated: interpolating invents
	// consumption that did not happen.
	total := profileFor(t, result, totalPath)
	strategy := mustGet(t, total.Recommendations.InterpolationStrategy, "interpolation strategy")
	if strategy != InterpolationFFill {
		t.Errorf("counter interpolation = %s, want ffill", strategy)
	}
}

func TestSessionsAreStoredBehindTheProfilesReference(t *testing.T) {
	fake := meterFixture()
	prof, result := profileMeter(t, fake)
	profile := profileFor(t, result, powerPath)

	activity := mustGet(t, profile.ActivityPattern, "activity pattern")
	if activity.SessionsRef != SessionsPath(profile.ProfileID) {
		t.Errorf("sessions_ref = %q, want %q", activity.SessionsRef, SessionsPath(profile.ProfileID))
	}
	if _, err := prof.Store().Sessions(profile.ProfileID, SessionQuery{}); err != nil {
		t.Errorf("the referenced session resource does not resolve: %v", err)
	}
}

func TestAStoredProfileIsRetrievableWithItsOverlay(t *testing.T) {
	fake := meterFixture()
	prof, result := profileMeter(t, fake)
	stored := profileFor(t, result, powerPath)

	if _, err := prof.Store().AppendOverride(ProfileOverride{
		SeriesRef: stored.SeriesRef, ProfileID: stored.ProfileID, CreatedBy: "user-123",
		FieldPath: FieldUnit, Action: ActionCorrect, ComputedValue: "W", ConfirmedValue: "kW",
	}); err != nil {
		t.Fatalf("AppendOverride: %v", err)
	}

	resolved, found := prof.Profile(stored.ProfileID)
	if !found {
		t.Fatal("the stored profile is not retrievable by id")
	}
	if _, applied := resolved.Resolution[FieldUnit]; !applied {
		t.Error("the overlay was not applied on read")
	}
}

// The image ODE ships in carries no zone database, so the embedded one is what
// keeps the profiler startable there. Without it New fails over a timezone it
// only needs in order to flag DST.
func TestTheProfilerStartsWithTheDefaultTimezone(t *testing.T) {
	prof, err := New(meterFixture(), fakeOntology{index: powerOntology()}, NewMemoryStore(), Options{})
	if err != nil {
		t.Fatalf("New with the default timezone: %v", err)
	}
	if prof.localZone.String() != DefaultLocalTimezone {
		t.Errorf("zone = %s, want %s", prof.localZone, DefaultLocalTimezone)
	}
}

func TestAnUnknownTimezoneIsRefusedAtStartup(t *testing.T) {
	_, err := New(meterFixture(), fakeOntology{index: powerOntology()}, NewMemoryStore(),
		Options{LocalTimezone: "Mars/Olympus_Mons"})
	if err == nil {
		t.Fatal("an unknown timezone was accepted")
	}
}

// A JSON null is what D24 exists to prevent, and a nil Go slice produces one
// without anyone writing the word null. This walks the whole marshalled profile
// rather than checking the fields that have gone wrong before, because the next
// nil slice will be somewhere else.
//
// value_semantics.characteristic_id is the one documented exception: D29 requires
// it to be null where none is declared, precisely so that nothing downstream
// fabricates one.
func TestAMarshalledProfileContainsNoUnexpectedNulls(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)

	allowed := map[string]bool{
		"value_semantics.characteristic_id": true,
	}

	for _, profile := range result.Profiles {
		encoded, err := json.Marshal(profile)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, path := range findNulls(decoded, "") {
			if allowed[path] {
				continue
			}
			t.Errorf("%s: %s is null; a list must marshal as [] and a value as its not_computed object",
				profile.SeriesRef.VariablePath, path)
		}
	}
}

// The projection is walked too: it has its own construction, and reduce() builds
// parts of it by hand under budget pressure.
func TestAMarshalledProjectionContainsNoUnexpectedNulls(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)

	allowed := map[string]bool{
		"value_semantics.characteristic_id": true,
	}

	for _, budget := range []int{0, 200} {
		for _, profile := range result.Profiles {
			view := Project(profile, budget)
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, path := range findNulls(decoded, "") {
				if allowed[path] {
					continue
				}
				t.Errorf("budget %d, %s: %s is null", budget, profile.SeriesRef.VariablePath, path)
			}
		}
	}
}

// findNulls returns the dotted path of every JSON null, with array indices
// dropped so a report names the field rather than the element.
func findNulls(node any, path string) []string {
	switch value := node.(type) {
	case nil:
		return []string{path}
	case map[string]any:
		out := []string{}
		for key, child := range value {
			next := key
			if path != "" {
				next = path + "." + key
			}
			out = append(out, findNulls(child, next)...)
		}
		return out
	case []any:
		out := []string{}
		for _, child := range value {
			out = append(out, findNulls(child, path)...)
		}
		return out
	default:
		return nil
	}
}

// --- diagnosing an empty profile ---

// The case a developer actually hits: retention has aged the unbucketed data out,
// so every structural detector reports insufficient coverage and nothing in the
// profile says why. The availability response is what knows, and the profile path
// used to throw that away.
func TestAServiceWithOnlyAggregatedWindowsSaysWhyTheStructuralFieldsAreEmpty(t *testing.T) {
	fake := meterFixture()
	// Only an aggregated entry: no raw window at all.
	from, to := analysisFrom, computeNow
	bucket, groupType := "1h", "mean"
	fake.availability[testDeviceID] = []timeseries.Availability{{
		ServiceId: testServiceID, From: &from, To: &to, GroupTime: &bucket, GroupType: &groupType,
	}}
	// And the raw pass comes back empty, as it would when there is nothing raw left.
	fake.results[0] = []timeseries.QueryResult{
		queryResult(0, testDeviceID, testServiceID, testColumnNames, nil, map[string][]float64{}),
	}

	_, result := profileMeter(t, fake)
	profile := profileFor(t, result, powerPath)

	summary := profile.ReadSummary
	if available, known := summary.RawAvailable.Get(); !known || available {
		t.Errorf("raw_available = %v (known %v), want a computed false: the platform answered, "+
			"and what it said was that only aggregated windows exist", available, known)
	}
	if summary.RawRows != 0 {
		t.Errorf("raw_rows = %d, want 0", summary.RawRows)
	}
	if !strings.Contains(summary.Diagnosis, "no raw window") {
		t.Errorf("diagnosis = %q, want it to name the missing raw window", summary.Diagnosis)
	}
	if !strings.Contains(summary.Diagnosis, "Retention") {
		t.Errorf("diagnosis = %q, want it to name the cause a developer can act on", summary.Diagnosis)
	}

	// And the field that reported the useless symptom now carries the cause.
	if profile.Coverage.IsComputed() {
		t.Fatal("coverage was computed from an empty raw read")
	}
	detail := profile.Coverage.Status().Detail
	if !strings.Contains(detail, "no raw window") {
		t.Errorf("coverage detail = %q, want the diagnosis appended to it", detail)
	}

	// The aggregated pass still ran, so the profile is thin rather than useless.
	if summary.AggregatedBuckets == 0 {
		t.Error("aggregated_buckets = 0; the fixture's aggregated pass should still have answered")
	}
}

// A service that reports while one of its channels stays silent looks identical to
// a dead read from inside that channel's profile. It is not, and the difference is
// what a developer needs.
func TestADeadChannelIsDistinguishedFromAFailedRead(t *testing.T) {
	fake := meterFixture()
	rawTimes, _, total := meterRawSeries()
	// Rows arrive for the service, but value.power is null in every one of them:
	// the map has no entry for it, which queryResult renders as null.
	fake.results[0] = []timeseries.QueryResult{
		queryResult(0, testDeviceID, testServiceID, testColumnNames, rawTimes,
			map[string][]float64{totalPath: total}),
	}

	_, result := profileMeter(t, fake)

	dead := profileFor(t, result, powerPath)
	if dead.ReadSummary.RawRows == 0 {
		t.Fatal("raw_rows = 0; the fixture should have returned rows for the service")
	}
	if dead.ReadSummary.ValuesPresent != 0 {
		t.Errorf("values_present = %d, want 0 for the silent channel", dead.ReadSummary.ValuesPresent)
	}
	if dead.ReadSummary.NullRows == 0 {
		t.Error("null_rows = 0, want the nulls counted")
	}
	if !strings.Contains(dead.ReadSummary.Diagnosis, "dead channel") {
		t.Errorf("diagnosis = %q, want it to name a dead channel rather than a failed read",
			dead.ReadSummary.Diagnosis)
	}

	// The sibling that did report is unaffected and says nothing is wrong.
	alive := profileFor(t, result, totalPath)
	if alive.ReadSummary.ValuesPresent == 0 {
		t.Error("the reporting sibling has no values")
	}
	if alive.ReadSummary.Diagnosis != "" {
		t.Errorf("diagnosis = %q, want none for a healthy read", alive.ReadSummary.Diagnosis)
	}
}

// A healthy profile carries the counts and no diagnosis: the block is always
// populated, so its emptiness is itself informative.
func TestAHealthyProfileCarriesTheReadCountsAndNoDiagnosis(t *testing.T) {
	fake := meterFixture()
	_, result := profileMeter(t, fake)
	profile := profileFor(t, result, powerPath)

	summary := profile.ReadSummary
	if available, known := summary.RawAvailable.Get(); !known || !available {
		t.Errorf("raw_available = %v (known %v), want a computed true for a fixture with a raw window",
			available, known)
	}
	if summary.RawRows == 0 || summary.ValuesPresent == 0 || summary.AggregatedBuckets == 0 {
		t.Errorf("summary = %+v, want all three counts populated", summary)
	}
	if summary.Diagnosis != "" {
		t.Errorf("diagnosis = %q, want none when both passes returned data", summary.Diagnosis)
	}
}

// derefInt renders an optional request field for an error message, so a failure
// prints the value rather than the address of one.
func derefInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// group_time reaches the profiler from a model as a free-form string, and the
// aggregated detectors size their grid from the analysis window divided by it
// before any data is looked at. It is refused at the boundary, before either read
// is paid for, and the refusal names the bucket that would have worked — a model
// that gets a bound it can act on corrects itself, where a bare rejection makes
// it give up.
func TestAGroupTimeTooFineForTheWindowIsRefusedBeforeAnythingIsRead(t *testing.T) {
	for _, groupTime := range []string{"1ns", "1ms", "1s"} {
		fake := meterFixture()
		prof := newTestProfiler(t, fake, powerOntology(), computeNow)

		_, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
			Device:    meterDevice(testDeviceID, testServiceID),
			ServiceID: testServiceID,
			GroupTime: groupTime,
		})
		if err == nil {
			t.Fatalf("group_time %s was accepted", groupTime)
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("group_time %s: error is %v, want ErrInvalidRequest", groupTime, err)
		}
		for _, want := range []string{groupTime, "16s"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("group_time %s: refusal does not mention %q: %v", groupTime, want, err)
			}
		}
		for _, query := range fake.queries {
			for _, element := range query {
				if element.GroupTime != nil || element.Columns != nil {
					t.Errorf("group_time %s: a value read was made despite the refusal", groupTime)
				}
			}
		}
	}
}

func TestAUsableGroupTimeIsStillAccepted(t *testing.T) {
	fake := meterFixture()
	prof := newTestProfiler(t, fake, powerOntology(), computeNow)

	result, err := prof.ProfileService(context.Background(), "Bearer caller", ProfileRequest{
		Device:    meterDevice(testDeviceID, testServiceID),
		ServiceID: testServiceID,
		GroupTime: "1h",
	})
	if err != nil {
		t.Fatalf("ProfileService: %v", err)
	}
	if result.GroupTime != "1h" {
		t.Errorf("group_time = %q, want the requested 1h", result.GroupTime)
	}
}

// The cache key is built from the raw window as *requested* (D25). It used to be
// built from the requested one at the lookup and from the truncated one at the
// write, so the two could not match: an eleven-variable meter divides the point
// budget down to 9090 rows and is truncated essentially always, and every
// identical repeat re-read the platform — both passes, up to the row cap by the
// column count — while from_cache reported the profiles as cached, so the
// response's own read counter contradicted itself. A tool result is re-offered on
// every iteration of the tool loop, so this happens several times in one turn.
func TestARecomputationReadsNothingEvenWhenThePointBudgetTruncated(t *testing.T) {
	const limit = 100
	fake := meterFixture()
	rawTimes, power, total := meterRawSeries()
	head := len(rawTimes) - limit
	fake.results[0] = []timeseries.QueryResult{queryResult(0, testDeviceID, testServiceID, testColumnNames,
		rawTimes[head:], map[string][]float64{powerPath: power[head:], totalPath: total[head:]})}
	// Enough fixture responses that a second pass reading the platform would
	// succeed rather than fail for want of one.
	fake.results = append(fake.results, fake.results[0], fake.results[1])

	prof, err := New(fake, fakeOntology{index: powerOntology()}, NewMemoryStore(), Options{
		Now:                func() time.Time { return computeNow },
		RawWindowMaxPoints: limit,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := ProfileRequest{Device: meterDevice(testDeviceID, testServiceID), ServiceID: testServiceID}

	first, err := prof.ProfileService(context.Background(), "Bearer caller", request)
	if err != nil {
		t.Fatalf("first ProfileService: %v", err)
	}
	if !first.RawWindow.Truncated {
		t.Fatal("the fixture did not truncate, so this proves nothing")
	}
	queriesAfterFirst := len(fake.queries)

	second, err := prof.ProfileService(context.Background(), "Bearer caller", request)
	if err != nil {
		t.Fatalf("second ProfileService: %v", err)
	}
	if second.Reads.Values != 0 {
		t.Errorf("value reads = %d on an identical repeat, want 0", second.Reads.Values)
	}
	if len(fake.queries) != queriesAfterFirst {
		t.Errorf("queries went from %d to %d; the profiles were reported cached, so nothing may be read",
			queriesAfterFirst, len(fake.queries))
	}
	if len(second.FromCache) != len(first.Profiles) {
		t.Errorf("from_cache = %v, want all %d profiles", second.FromCache, len(first.Profiles))
	}
}
