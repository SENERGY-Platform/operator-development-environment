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

package simulation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation/simulationtest"
)

const token = "Bearer developer-token"

func client(t *testing.T) (*simulation.Service, *simulationtest.MOSES) {
	t.Helper()
	moses := simulationtest.New(t)
	return simulation.New(moses.URL(), simulation.Options{}), moses
}

func minimal(name string) simulation.Environment {
	return simulation.Environment{
		Name:    name,
		Type:    simulation.IndustrialSite,
		Context: map[string]any{},
		Zones: []simulation.Zone{{
			Name:          name,
			Type:          simulation.ZoneSite,
			InitialStates: map[string]any{},
			Assets: []simulation.Asset{{
				Name:           "meter",
				Kind:           simulation.AssetMeter,
				ExternalTypeId: "device-type-1",
				InitialStates:  map[string]any{},
				Channels: []simulation.Channel{{
					Name:             "Power",
					Direction:        simulation.Sensor,
					ExternalRef:      "service-1",
					CharacteristicId: "characteristic-watt",
					IntervalSeconds:  60,
					Source: simulation.Source{
						Kind:    simulation.SourceProfile,
						Profile: &simulation.ProfileSource{Base: 100},
					},
				}},
			}},
		}},
	}
}

// The property the whole read-modify-write cycle rests on. MOSES provisions a
// device for every asset that names a type and carries no reference, so an edit
// that strips external_ref does not fail — it quietly creates a second device and
// orphans the timeseries of the first.
func TestAnEditKeepsTheDeviceTheAssetAlreadyPublishesThrough(t *testing.T) {
	service, moses := client(t)
	ctx := context.Background()

	created, err := service.Create(ctx, token, minimal("hall"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := moses.Devices(created.ID)
	if len(first) != 1 {
		t.Fatalf("the create provisioned %v, want exactly one device", first)
	}

	read, err := service.Get(ctx, token, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	read.Name = "hall, renamed"
	if _, err := service.Replace(ctx, token, read); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	after := moses.Devices(created.ID)
	if len(after) != 1 || after[0] != first[0] {
		t.Errorf("after the edit the environment publishes through %v, want the original %v — "+
			"a stripped external_ref makes MOSES provision a second device and orphan the first",
			after, first)
	}
}

// The other half of the same rule: the two fields MOSES reconciles are never
// echoed, because an echoed one is what would decide that somebody's real device
// or another environment's graph is destroyed.
func TestTheFieldsMosesDecidesAreNeverSent(t *testing.T) {
	service, moses := client(t)
	ctx := context.Background()

	created, err := service.Create(ctx, token, minimal("site"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The create came back with both fields set by MOSES. Writing that document
	// straight back is exactly what an editor does, and is the case that must not
	// echo them.
	read, err := service.Get(ctx, token, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.ExternalGraphRef == "" {
		t.Fatal("the fake did not set external_graph_ref, so this test would pass vacuously")
	}
	if _, err := service.Replace(ctx, token, read); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(moses.EchoedServerOwned) != 0 {
		t.Errorf("ODE sent fields MOSES reconciles: %v", moses.EchoedServerOwned)
	}
}

func TestAStaleWriteIsRefusedWithBothVersions(t *testing.T) {
	service, _ := client(t)
	ctx := context.Background()

	created, err := service.Create(ctx, token, minimal("site"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stale, err := service.Get(ctx, token, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Somebody else writes in between.
	fresh, err := service.Get(ctx, token, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	fresh.Name = "changed by the other editor"
	if _, err := service.Replace(ctx, token, fresh); err != nil {
		t.Fatalf("Replace by the other editor: %v", err)
	}

	stale.Name = "changed by us"
	_, err = service.Replace(ctx, token, stale)
	var conflict *simulation.VersionConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("Replace of a stale document = %v, want a *VersionConflict", err)
	}
	if conflict.Carried != stale.Version {
		t.Errorf("the conflict carries version %d, want the one that was read, %d",
			conflict.Carried, stale.Version)
	}
	if !strings.Contains(conflict.Detail, "stored version") {
		t.Errorf("the conflict detail is %q and does not relay what MOSES said", conflict.Detail)
	}
}

// A document from a MOSES this build does not know reads, and refuses to be
// written back. Reading it is the point: an ODE that could not show a developer
// their own environment because the simulator gained a field would be the worse
// failure.
func TestADocumentWithAnUnknownFieldReadsAndRefusesToBeWritten(t *testing.T) {
	service, moses := client(t)
	ctx := context.Background()

	moses.PutRaw("env-future", map[string]any{
		"name":            "from a newer moses",
		"type":            "industrial_site",
		"version":         float64(4),
		"zones":           []any{},
		"context":         map[string]any{},
		"climate_control": map[string]any{"mode": "heating"},
	})

	read, err := service.Get(ctx, token, "env-future")
	if err != nil {
		t.Fatalf("Get: %v — a document with an unknown field must still read", err)
	}
	if read.Name != "from a newer moses" {
		t.Errorf("name = %q, want the stored one", read.Name)
	}
	if read.UnknownField != "climate_control" {
		t.Errorf("UnknownField = %q, want climate_control", read.UnknownField)
	}

	_, err = service.Replace(ctx, token, read)
	if !errors.Is(err, simulation.ErrUnknownField) {
		t.Fatalf("Replace = %v, want ErrUnknownField: writing it back would delete the field", err)
	}
	if !strings.Contains(err.Error(), "climate_control") {
		t.Errorf("the refusal is %q and does not name the field", err)
	}
	if moses.Count("PUT", "/environments/env-future") != 0 {
		t.Error("the refused write reached MOSES anyway")
	}
}

func TestTheDevelopersOwnTokenIsForwarded(t *testing.T) {
	service, moses := client(t)
	if _, err := service.List(context.Background(), token); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(moses.Tokens) != 1 || moses.Tokens[0] != token {
		t.Errorf("MOSES saw %v, want the developer's own token — an environment's owner comes "+
			"from the caller's token, so a service account here would create simulations "+
			"nobody can find", moses.Tokens)
	}
}

func TestAValidationFailureIsTheCallersMistakeAndKeepsTheFieldPaths(t *testing.T) {
	service, moses := client(t)
	moses.Fail["POST /environments"] = simulationtest.FakeFailure{
		Code: 400,
		Body: `{"problems":[{"path":"zones[0].assets[0].channels[0].source.profile.hour_factors","message":"must have 24 entries or be empty, got 12"}]}`,
	}
	_, err := service.Create(context.Background(), token, minimal("site"))
	if !errors.Is(err, simulation.ErrInvalidRequest) {
		t.Fatalf("Create = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "hour_factors") {
		t.Errorf("the refusal is %q and lost the field path, which is the useful half", err)
	}
}

func TestANotFoundIsTellableApart(t *testing.T) {
	service, _ := client(t)
	_, err := service.Get(context.Background(), token, "nothing")
	if !errors.Is(err, simulation.ErrNotFound) {
		t.Fatalf("Get of an unknown id = %v, want ErrNotFound", err)
	}
}

func TestAStoredButNotRunningEnvironmentIsNotAnError(t *testing.T) {
	service, _ := client(t)
	ctx := context.Background()
	created, err := service.Create(ctx, token, minimal("site"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := service.State(ctx, token, created.ID)
	if err != nil {
		t.Fatalf("State: %v — a document that was just written is stored and not running, "+
			"which is the normal case and not a failure", err)
	}
	if state.Running {
		t.Error("a freshly written environment reports as running")
	}
}

func TestABackfillWindowIsCheckedBeforeItIsSent(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name    string
		from    time.Time
		to      time.Time
		wantsay string
	}{
		{"ends in the future", now.Add(-time.Hour), now.Add(48 * time.Hour), "future"},
		{"longer than a year", now.AddDate(-2, 0, 0), now, "limit is 366"},
		{"backwards", now, now.Add(-time.Hour), "ends at or before it starts"},
		{"before the epoch", time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), now, "before 2000"},
		{"open ended", time.Time{}, now, "both ends"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := simulation.CheckWindow(testCase.from, testCase.to, now)
			if !errors.Is(err, simulation.ErrInvalidRequest) {
				t.Fatalf("CheckWindow = %v, want ErrInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), testCase.wantsay) {
				t.Errorf("the refusal is %q and does not say %q", err, testCase.wantsay)
			}
		})
	}

	// A window ending a few seconds into the future is a clock difference rather
	// than a request to reconstruct the future, and MOSES accepts it.
	if err := simulation.CheckWindow(now.Add(-time.Hour), now.Add(10*time.Second), now); err != nil {
		t.Errorf("a window ending ten seconds ahead was refused: %v", err)
	}
}

func TestABackfillIsFollowedAndSaysWhatItSkipped(t *testing.T) {
	service, moses := client(t)
	ctx := context.Background()
	created, err := service.Create(ctx, token, minimal("site"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	moses.SetRunning(created.ID, nil)

	to := time.Now().Add(-time.Hour)
	from := to.Add(-24 * time.Hour)
	status, err := service.Backfill(ctx, token, created.ID, from, to)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if status.State != simulation.BackfillRunning {
		t.Errorf("state = %q, want running", status.State)
	}

	moses.FinishBackfill(created.ID, map[string]any{
		"state":          "done",
		"channels_total": float64(2),
		"channels_done":  float64(2),
		"published":      float64(1440),
		"channels": []any{
			map[string]any{"channel_id": "c1", "name": "Power", "backfillable": true, "published": float64(1440)},
			map[string]any{"channel_id": "c2", "name": "Hall power", "backfillable": false,
				"skip_reason": "an aggregate is derived from the channels below it"},
		},
	})

	followed, err := service.BackfillStatusOf(ctx, token, created.ID)
	if err != nil {
		t.Fatalf("BackfillStatusOf: %v", err)
	}
	skipped := followed.Skipped()
	if len(skipped) != 1 || skipped[0].Name != "Hall power" {
		t.Fatalf("Skipped() = %v, want the aggregate channel — a job can report done with "+
			"every channel skipped, and that list is the honest answer to where the data is",
			skipped)
	}
	if skipped[0].SkipReason == "" {
		t.Error("a skipped channel carries no reason")
	}
}

func TestAnUploadedDatasetTravelsAsTheFileItself(t *testing.T) {
	service, moses := client(t)
	csv := "time,power\n2026-08-01T00:00:00Z,120\n2026-08-01T01:00:00Z,340\n"

	dataset, err := service.UploadDataset(context.Background(), token,
		"a real hall's august", "Europe/Berlin", []byte(csv))
	if err != nil {
		t.Fatalf("UploadDataset: %v", err)
	}
	if len(dataset.Columns) != 1 || dataset.Columns[0].Name != "power" {
		t.Errorf("columns = %v, want the one value column MOSES parsed", dataset.Columns)
	}
	if dataset.Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %q; a CSV of offsetless local timestamps read in the wrong zone "+
			"shifts every value by an hour", dataset.Timezone)
	}
	stored, found := moses.DatasetContent(dataset.ID)
	if !found || string(stored) != csv {
		t.Errorf("MOSES received %q, want the file verbatim", string(stored))
	}
}

func TestAnUnparseableDatasetIsRefused(t *testing.T) {
	service, _ := client(t)
	_, err := service.UploadDataset(context.Background(), token, "broken", "", []byte("nothing useful"))
	if !errors.Is(err, simulation.ErrInvalidRequest) {
		t.Fatalf("UploadDataset of an unparseable file = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("the refusal is %q and lost the line number, which is what the developer "+
			"needs to fix the file", err)
	}
}

func TestAnOversizedUploadIsRefusedBeforeItIsSent(t *testing.T) {
	moses := simulationtest.New(t)
	service := simulation.New(moses.URL(), simulation.Options{MaxDatasetBytes: 32})
	_, err := service.UploadDataset(context.Background(), token, "big", "",
		[]byte(strings.Repeat("a", 64)))
	if !errors.Is(err, simulation.ErrInvalidRequest) {
		t.Fatalf("UploadDataset = %v, want ErrInvalidRequest", err)
	}
	if moses.Count("POST", "/datasets") != 0 {
		t.Error("the oversized upload reached MOSES anyway")
	}
}
