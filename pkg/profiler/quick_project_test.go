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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// rankedCandidates runs the real quick tier over a fleet, so the projection is
// tested against the profiles the profiler actually assembles rather than
// against a hand-written idea of one.
func rankedCandidates(t *testing.T, fleetSize int) QuickResult {
	t.Helper()
	devices, fake := candidateFleet(fleetSize)
	prof := newTestProfiler(t, fake, powerOntology(), quickNow)

	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}
	if len(result.Candidates) != fleetSize*2 {
		t.Fatalf("candidates = %d, want %d (two variables per device)",
			len(result.Candidates), fleetSize*2)
	}
	return result
}

func encode(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

// The bookkeeping a QuickProfile carries for ODE's own sake is a third of its
// size, and none of it is something a model acts on.
func TestTheQuickProjectionDropsWhatOnlyODEItselfNeeds(t *testing.T) {
	result := rankedCandidates(t, 1)

	stored := encode(t, result.Candidates)
	for _, want := range []string{"provenance", "group_time"} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored candidate carries no %q, so this test would prove nothing", want)
		}
	}

	view := ProjectQuick(result, 0)

	// The candidates alone: the caveat at the top level talks *about* provenance,
	// which is the point of it being stated once instead of eighty times.
	projected := encode(t, view.Candidates)
	for _, dropped := range []string{"provenance", "group_time", "estimate_basis", "\"basis\""} {
		if strings.Contains(projected, dropped) {
			t.Errorf("the projection still carries %s", dropped)
		}
	}
	if !strings.Contains(projected, "aggregates_available") {
		t.Error("whether pre-aggregated variants exist is the actionable half and should survive")
	}
	if view.Caveat != QuickCaveat {
		t.Error("the caveat the per-candidate fields no longer repeat is missing")
	}
}

func TestTheQuickProjectionKeepsTheRankedListInsideItsBudget(t *testing.T) {
	result := rankedCandidates(t, 20)
	const budget = 4000

	view := ProjectQuick(result, budget)
	encoded := encode(t, view)

	if tokens := len(encoded) / bytesPerToken; tokens > budget {
		t.Errorf("the projection is about %d tokens, over the %d it was given", tokens, budget)
	}
	if len(view.Candidates) == 0 || len(view.Candidates) >= len(result.Candidates) {
		t.Fatalf("shown = %d of %d; the budget should have cut some and kept some",
			len(view.Candidates), len(result.Candidates))
	}
	if len(view.Elided) != 1 {
		t.Fatalf("elided = %v, want one entry for the candidate list", view.Elided)
	}
	if view.Elided[0].Total != len(result.Candidates) || view.Elided[0].Shown != len(view.Candidates) {
		t.Errorf("elided = %+v, want total %d shown %d",
			view.Elided[0], len(result.Candidates), len(view.Candidates))
	}
}

// A truncated list must not be a silent one: the candidates that were cut belong
// to devices, and a device nobody can name is a device the developer cannot ask
// about.
func TestTheQuickProjectionNamesEveryDeviceWhoseCandidatesWereCut(t *testing.T) {
	result := rankedCandidates(t, 20)
	view := ProjectQuick(result, 4000)

	if len(view.ElidedDevices) == 0 {
		t.Fatal("candidates were cut but no device is named for it")
	}

	cut := 0
	for _, device := range view.ElidedDevices {
		if device.Name == "" {
			t.Errorf("device %s is reported only by its id", device.DeviceID)
		}
		if device.Shown > device.Total {
			t.Errorf("device %s reports %d shown of %d", device.DeviceID, device.Shown, device.Total)
		}
		cut += device.Total - device.Shown
	}
	if want := len(result.Candidates) - len(view.Candidates); cut != want {
		t.Errorf("the per-device figures account for %d cut candidates, want %d", cut, want)
	}
}

func TestAnUnbudgetedQuickProjectionKeepsEveryCandidate(t *testing.T) {
	result := rankedCandidates(t, 20)

	view := ProjectQuick(result, 0)

	if len(view.Candidates) != len(result.Candidates) {
		t.Errorf("shown = %d, want all %d", len(view.Candidates), len(result.Candidates))
	}
	if len(view.Elided) != 0 || len(view.ElidedDevices) != 0 {
		t.Errorf("nothing was cut, so nothing should be recorded: %v %v", view.Elided, view.ElidedDevices)
	}
}

// One candidate and an honest count beats an empty list: a budget too small for a
// single candidate is a misconfiguration, and answering it with nothing at all
// would hide the candidates rather than report them.
func TestAQuickProjectionKeepsOneCandidateUnderAnImpossibleBudget(t *testing.T) {
	result := rankedCandidates(t, 20)

	view := ProjectQuick(result, 1)

	if len(view.Candidates) != 1 {
		t.Errorf("shown = %d, want 1", len(view.Candidates))
	}
	if len(view.Elided) != 1 || view.Elided[0].Shown != 1 {
		t.Errorf("elided = %v, want one entry showing 1", view.Elided)
	}
}

func TestTheQuickProjectionKeepsTheOntologysDeclaredBounds(t *testing.T) {
	result := rankedCandidates(t, 1)

	view := ProjectQuick(result, 0)

	found := false
	for _, candidate := range view.Candidates {
		if candidate.SeriesRef.VariablePath != "value.power" {
			continue
		}
		found = true
		if candidate.Declared.Max == nil || *candidate.Declared.Max != 10000 {
			t.Errorf("declared max = %v, want the ontology's 10000", candidate.Declared.Max)
		}
		if candidate.Declared.Unit != "W" {
			t.Errorf("unit = %q, want W", candidate.Declared.Unit)
		}
	}
	if !found {
		t.Fatal("value.power is missing from the projection")
	}
}

// A bound the ontology does not state is absent rather than explained, once per
// candidate. The distinction D24 protects is a different one — a *detector* that
// could not compute something still says why, which the next test pins.
func TestABoundTheOntologyDoesNotStateIsAbsentFromTheProjection(t *testing.T) {
	profile := QuickProfile{
		Declared: Declared{
			Unit:       "W",
			UnitSource: UnitFromCharacteristic,
			MinValue:   Uncomputable[float64](ReasonOutOfScope, "characteristic declares no minimum"),
			MaxValue:   Uncomputable[float64](ReasonOutOfScope, "characteristic declares no maximum"),
		},
	}

	encoded := encode(t, ProjectQuickCandidate(profile))

	for _, absent := range []string{"\"min\"", "\"max\"", "declares no minimum"} {
		if strings.Contains(encoded, absent) {
			t.Errorf("the projection still carries %s: %s", absent, encoded)
		}
	}
}

func TestAReadThatFailedStillSaysWhyInTheProjection(t *testing.T) {
	profile := QuickProfile{
		Availability: Uncomputable[AvailabilityWindow](ReasonReadFailed, "the platform reports no availability"),
		Volume:       Uncomputable[Volume](ReasonReadFailed, "no usage figure for this device"),
	}

	encoded := encode(t, ProjectQuickCandidate(profile))

	for _, want := range []string{"not_computed", "read_failed", "no availability", "no usage figure"} {
		if !strings.Contains(encoded, want) {
			t.Errorf("the projection lost %q: %s", want, encoded)
		}
	}
}

// The reads counter is the tier's evidence, not decoration (§3.2): a projection
// that dropped it would remove the one statement that no value was read.
func TestTheQuickProjectionKeepsTheReadCounters(t *testing.T) {
	result := rankedCandidates(t, 3)

	view := ProjectQuick(result, 4000)

	if view.Reads != result.Reads {
		t.Errorf("reads = %+v, want %+v", view.Reads, result.Reads)
	}
	if view.Tier != TierQuick {
		t.Errorf("tier = %q, want %q", view.Tier, TierQuick)
	}
}

// A fleet of one device type ties on every ranking input — same span, same
// coverage, all online — and a ranked prefix then means only "whichever device
// was listed first". Three inverters must not be answered with one inverter.
func TestATruncatedListStillCoversEveryDeviceWhenTheRankingTies(t *testing.T) {
	fake := &fakeTimeseries{
		availability: map[string][]timeseries.Availability{},
		usage:        map[string]timeseries.Usage{},
	}
	serviceID := "urn:infai:ses:service:1"
	from, to := quickNow.Add(-400*24*time.Hour), quickNow

	devices := make([]models.ExtendedDevice, 0, 3)
	for i := 0; i < 3; i++ {
		deviceID := fmt.Sprintf("urn:infai:ses:device:%d", i)
		device := meterDevice(deviceID, serviceID)
		device.Name = fmt.Sprintf("Inverter %d", i)
		devices = append(devices, device)
		fake.availability[deviceID] = availabilityWindow(serviceID, from, to, "15m")
		fake.usage[deviceID] = timeseries.Usage{
			DeviceId: deviceID, Bytes: 1 << 20, BytesPerDay: 8640, UpdatedAt: quickNow,
		}
	}

	prof := newTestProfiler(t, fake, powerOntology(), quickNow)
	result, err := prof.QuickProfiles(context.Background(), "Bearer caller", QuickRequest{Devices: devices})
	if err != nil {
		t.Fatalf("QuickProfiles: %v", err)
	}

	// A budget that fits some of the six candidates but not all of them.
	view := ProjectQuick(result, 1200)
	if len(view.Candidates) < 3 || len(view.Candidates) >= len(result.Candidates) {
		t.Fatalf("shown = %d of %d; this budget should truncate and still leave room for three",
			len(view.Candidates), len(result.Candidates))
	}

	seen := map[string]bool{}
	for _, candidate := range view.Candidates {
		seen[candidate.SeriesRef.DeviceID] = true
	}
	if len(seen) != len(devices) {
		t.Errorf("the shortlist covers %d of %d devices; a tied ranking cut the others out",
			len(seen), len(devices))
	}
}
