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
	"fmt"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

var quickNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

// candidateFleet builds n meter devices, each carrying two addressable variables,
// with availability windows of differing span and recency.
func candidateFleet(n int) ([]models.ExtendedDevice, *fakeTimeseries) {
	devices := make([]models.ExtendedDevice, 0, n)
	fake := &fakeTimeseries{
		availability: map[string][]timeseries.Availability{},
		usage:        map[string]timeseries.Usage{},
	}

	for i := 0; i < n; i++ {
		deviceID := fmt.Sprintf("urn:infai:ses:device:%d", i)
		serviceID := "urn:infai:ses:service:11111111-1111-1111-1111-111111111111"
		device := meterDevice(deviceID, serviceID)
		device.Name = fmt.Sprintf("Meter %d", i)
		devices = append(devices, device)

		// Device 0 has the longest history and the freshest data; each subsequent
		// device has a shorter span and is staler.
		to := quickNow.Add(-time.Duration(i) * 12 * time.Hour)
		from := to.Add(-time.Duration(400-i*10) * 24 * time.Hour)
		fake.availability[deviceID] = availabilityWindow(serviceID, from, to, "15m")
		fake.usage[deviceID] = timeseries.Usage{
			DeviceId: deviceID, Bytes: 1 << 20, BytesPerDay: 8640, UpdatedAt: quickNow,
		}
	}
	return devices, fake
}

// M1a acceptance: 40 candidate series ranked from metadata alone, with zero value
// reads. The fake fails the test if the value-reading method is touched at all,
// so the property is enforced rather than asserted.
func TestFortyCandidatesAreRankedWithoutReadingAnyValue(t *testing.T) {
	devices, fake := candidateFleet(20)
	fake.onQuery = func([]timeseries.QueryElement) {
		t.Fatal("QuickProfiles issued a value read; the tier's whole point is that it does not")
	}
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	if len(result.Candidates) != 40 {
		t.Fatalf("candidates = %d, want 40 (twenty devices, two variables each)", len(result.Candidates))
	}
	if result.Reads.Values != 0 {
		t.Errorf("value reads = %d, want 0", result.Reads.Values)
	}
	// One usage call for the whole fleet, one availability call per device.
	if result.Reads.Usage != 1 {
		t.Errorf("usage reads = %d, want 1 batched call", result.Reads.Usage)
	}
	if result.Reads.Availability != 20 {
		t.Errorf("availability reads = %d, want one per device", result.Reads.Availability)
	}
}

func TestCandidatesAreOrderedBestFirst(t *testing.T) {
	devices, fake := candidateFleet(20)
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	for i := 1; i < len(result.Candidates); i++ {
		if result.Candidates[i-1].RankHints.Score < result.Candidates[i].RankHints.Score {
			t.Fatalf("candidate %d scores %v, below its successor's %v",
				i-1, result.Candidates[i-1].RankHints.Score, result.Candidates[i].RankHints.Score)
		}
	}
	// Device 0 has the longest span and the freshest data, so it heads the list.
	if got := result.Candidates[0].SeriesRef.DeviceID; got != "urn:infai:ses:device:0" {
		t.Errorf("first candidate is %s, want the longest and freshest series", got)
	}
	if !result.Candidates[0].RankHints.IsLive {
		t.Error("the freshest candidate is not marked live")
	}
}

func TestRankingIsStableAcrossCalls(t *testing.T) {
	devices, fake := candidateFleet(10)
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	order := func() []string {
		result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
		if err != nil {
			t.Fatalf("QuickProfiles: %v", err)
		}
		out := []string{}
		for _, candidate := range result.Candidates {
			out = append(out, candidate.SeriesRef.String())
		}
		return out
	}

	first, second := order(), order()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("position %d differs between calls: %s then %s", i, first[i], second[i])
		}
	}
}

// models.Read governs metadata; models.Execute governs reading data (§5.1).
// Offering a series ODE cannot read wastes the developer's decision and fails at
// query time instead of here.
func TestADeviceWithoutExecutePermissionIsSkippedWithAReason(t *testing.T) {
	devices, fake := candidateFleet(2)
	devices[1].Permissions.Execute = false
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	if len(result.Skipped) != 1 || result.Skipped[0].DeviceID != devices[1].Id {
		t.Fatalf("skipped = %+v, want the device without execute", result.Skipped)
	}
	if result.Skipped[0].Reason == "" {
		t.Error("the skip carries no reason")
	}
	for _, candidate := range result.Candidates {
		if candidate.SeriesRef.DeviceID == devices[1].Id {
			t.Error("a series was offered for a device the caller may not read data from")
		}
	}
}

func TestADeviceReadWithoutItsTypeIsSkipped(t *testing.T) {
	devices, fake := candidateFleet(1)
	devices[0].DeviceType = nil
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}
	if len(result.Skipped) != 1 || len(result.Candidates) != 0 {
		t.Fatalf("skipped = %+v, candidates = %d; want the device skipped",
			result.Skipped, len(result.Candidates))
	}
}

// §5.4.2 lists the four sources a QuickProfile may use, and /last-values is not
// among them: it returns actual values. The age comes from the availability
// window's end instead.
func TestLastValueAgeComesFromTheAvailabilityWindow(t *testing.T) {
	devices, fake := candidateFleet(1)
	serviceID := devices[0].DeviceType.Services[0].Id
	to := quickNow.Add(-3 * time.Hour)
	fake.availability[devices[0].Id] = availabilityWindow(serviceID, quickNow.Add(-30*24*time.Hour), to)
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	candidate := result.Candidates[0]
	age := mustGet(t, candidate.Liveness.LastValueAgeS, "last value age")
	if age != 3*3600 {
		t.Errorf("age = %vs, want 10800", age)
	}
	if candidate.Liveness.Basis != livenessBasis {
		t.Errorf("basis = %q, want %q so a reader knows where the age came from",
			candidate.Liveness.Basis, livenessBasis)
	}
}

func TestMaterialisedAggregatesAreReportedWithTheWindow(t *testing.T) {
	devices, fake := candidateFleet(1)
	serviceID := devices[0].DeviceType.Services[0].Id
	fake.availability[devices[0].Id] = availabilityWindow(
		serviceID, quickNow.Add(-30*24*time.Hour), quickNow, "15m", "1h")
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	availability := mustGet(t, result.Candidates[0].Availability, "availability")
	if len(availability.Aggregates) != 2 {
		t.Fatalf("aggregates = %+v, want the two materialised variants", availability.Aggregates)
	}
	if !availability.RawAvailable {
		t.Error("raw_available is false, but the fixture has a raw window")
	}
}

// A service with only aggregated windows has had its raw data aged out, and the
// structural detectors need raw data — so that has to be visible before a profile
// is attempted.
func TestAServiceWithNoRawWindowSaysSo(t *testing.T) {
	devices, fake := candidateFleet(1)
	serviceID := devices[0].DeviceType.Services[0].Id
	from, to := quickNow.Add(-30*24*time.Hour), quickNow
	bucket, groupType := "1h", "mean"
	fake.availability[devices[0].Id] = []timeseries.Availability{{
		ServiceId: serviceID, From: &from, To: &to, GroupTime: &bucket, GroupType: &groupType,
	}}
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}
	availability := mustGet(t, result.Candidates[0].Availability, "availability")
	if availability.RawAvailable {
		t.Error("raw_available is true for a service with only aggregated windows")
	}
}

func TestAServiceThePlatformDoesNotKnowYieldsNotComputedAvailability(t *testing.T) {
	devices, fake := candidateFleet(1)
	fake.availability[devices[0].Id] = nil
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	candidate := result.Candidates[0]
	if candidate.Availability.IsComputed() {
		t.Fatal("an availability window was reported for a service the platform does not know")
	}
	if status := candidate.Availability.Status(); status.Reason != ReasonReadFailed {
		t.Errorf("reason = %s, want read_failed", status.Reason)
	}
	// Without a window there is no age either, and zero would read as "current".
	if candidate.Liveness.LastValueAgeS.IsComputed() {
		t.Error("an age was reported with no window to take it from")
	}
}

// A failed usage read costs the cost estimate, not the candidate list: span and
// liveness still rank the series.
func TestAFailedUsageReadLeavesVolumeNotComputed(t *testing.T) {
	devices, fake := candidateFleet(1)
	fake.usageErr = fmt.Errorf("upstream 502")
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles returned an error for a failed cost estimate: %v", err)
	}
	if len(result.Candidates) == 0 {
		t.Fatal("no candidates were produced")
	}
	if result.Candidates[0].Volume.IsComputed() {
		t.Error("a volume was reported without a usage figure")
	}
	if result.Candidates[0].RankHints.Score == 0 {
		t.Error("the candidate is unrankable without its cost estimate")
	}
}

// The interval estimate from a device-wide byte rate is order of magnitude only,
// so it is always uncertain and never used for a resampling decision (§5.4.2).
func TestTheVolumeIntervalEstimateIsAlwaysUncertain(t *testing.T) {
	devices, fake := candidateFleet(1)
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}
	volume := mustGet(t, result.Candidates[0].Volume, "volume")
	if volume.Confidence != Uncertain {
		t.Errorf("confidence = %s, want uncertain", volume.Confidence)
	}
	if volume.EstimateBasis == "" {
		t.Error("the estimate names no basis")
	}
}

// D16: what a device type fails to declare is discovered per variable at runtime
// and reported, never assumed.
func TestAnIncompleteDeviceTypeIsReportedAsPartial(t *testing.T) {
	devices, fake := candidateFleet(1)
	// Strip the characteristic from one variable, as an incomplete device type
	// would.
	output := &devices[0].DeviceType.Services[0].Outputs[0].ContentVariable
	output.SubContentVariables[0].CharacteristicId = ""
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	var stripped *QuickProfile
	for i, candidate := range result.Candidates {
		if candidate.SeriesRef.VariablePath == "value.power" {
			stripped = &result.Candidates[i]
		}
	}
	if stripped == nil {
		t.Fatal("value.power is missing from the candidate list")
	}
	if stripped.OntologyCompleteness.Status != CompletenessPartial {
		t.Errorf("status = %s, want partial", stripped.OntologyCompleteness.Status)
	}
	if len(stripped.OntologyCompleteness.Missing) == 0 {
		t.Error("nothing is listed as missing")
	}
	if stripped.OntologyCompleteness.Consequence == "" {
		t.Error("no consequence is stated, so a developer cannot judge what the gap costs")
	}
}

// An unreadable variable is not offered by default, but a developer hunting for
// it can ask, and then it ranks last whatever its metadata says.
func TestUnqueryableVariablesAreExcludedByDefaultAndRankLastWhenAskedFor(t *testing.T) {
	devices, fake := candidateFleet(1)
	devices[0].DeviceType.Services[0].Interaction = models.REQUEST
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	excluded, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}
	if len(excluded.Candidates) != 0 {
		t.Errorf("candidates = %d, want none: the service is request-only", len(excluded.Candidates))
	}

	included, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{
		Devices: devices, IncludeUnqueryable: true,
	})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}
	if len(included.Candidates) != 2 {
		t.Fatalf("candidates = %d, want both variables reported", len(included.Candidates))
	}
	for _, candidate := range included.Candidates {
		if candidate.Queryable {
			t.Error("a request-only variable was reported as queryable")
		}
		if candidate.RankHints.Score != 0 {
			t.Errorf("score = %v, want 0 for an unreadable variable", candidate.RankHints.Score)
		}
	}
}

func TestTheCoverageProxyMeasuresOverlapWithTheDevelopersWindow(t *testing.T) {
	devices, fake := candidateFleet(1)
	serviceID := devices[0].DeviceType.Services[0].Id
	// Data covers only the second half of the window asked about.
	fake.availability[devices[0].Id] = availabilityWindow(
		serviceID, quickNow.Add(-50*24*time.Hour), quickNow)
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{
		Devices: devices,
		Window:  Window{From: quickNow.Add(-100 * 24 * time.Hour), To: quickNow},
	})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	proxy := result.Candidates[0].RankHints.CoverageProxy
	if proxy < 0.45 || proxy > 0.55 {
		t.Errorf("coverage_proxy = %v, want about 0.5", proxy)
	}
}

func TestTheDeclaredBlockCarriesTheOntologyUnitAndRange(t *testing.T) {
	devices, fake := candidateFleet(1)
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	found := false
	for _, candidate := range result.Candidates {
		if candidate.SeriesRef.VariablePath != "value.power" {
			continue
		}
		found = true
		if candidate.Declared.Unit != "W" {
			t.Errorf("unit = %q, want W", candidate.Declared.Unit)
		}
		if candidate.Declared.UnitSource != UnitFromCharacteristic {
			t.Errorf("unit_source = %s, want characteristic", candidate.Declared.UnitSource)
		}
		if max := mustGet(t, candidate.Declared.MaxValue, "declared maximum"); max != 10000 {
			t.Errorf("max = %v, want 10000", max)
		}
	}
	if !found {
		t.Fatal("value.power is missing from the candidate list")
	}
}
