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

package ontology

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

// fakeClient stands in for device-repository. It counts calls so the tests can
// assert on caching behaviour, which is the whole point of this package.
type fakeClient struct {
	mux sync.Mutex

	aspectCalls    atomic.Int32
	functionCalls  atomic.Int32
	timestampCalls atomic.Int32

	aspects    []models.AspectNode
	generation int64

	aspectErr    error
	aspectCode   int
	timestampErr error

	// delay lets a test hold a load open while other goroutines pile up.
	delay time.Duration
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		aspects:    []models.AspectNode{node("building", "Building", "")},
		generation: 1000,
	}
}

func (f *fakeClient) GetAspectNodes() ([]models.AspectNode, error, int) {
	f.aspectCalls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.aspectErr != nil {
		return nil, f.aspectErr, f.aspectCode
	}
	f.mux.Lock()
	defer f.mux.Unlock()
	return f.aspects, nil, 200
}

func (f *fakeClient) GetFunctionsByType(rdfType string) ([]models.Function, error, int) {
	f.functionCalls.Add(1)
	if rdfType == models.SES_ONTOLOGY_MEASURING_FUNCTION {
		return []models.Function{{Id: "fn-power", Name: "power", RdfType: rdfType}}, nil, 200
	}
	return []models.Function{{Id: "fn-switch", Name: "switch", RdfType: rdfType}}, nil, 200
}

func (f *fakeClient) ListCharacteristics(model.CharacteristicListOptions) ([]models.Characteristic, int64, error, int) {
	return []models.Characteristic{{Id: "ch-w", DisplayUnit: "W"}}, 1, nil, 200
}

func (f *fakeClient) ListConceptsWithCharacteristics(model.ConceptListOptions) ([]models.ConceptWithCharacteristics, int64, error, int) {
	return []models.ConceptWithCharacteristics{{Id: "concept-power"}}, 1, nil, 200
}

func (f *fakeClient) GetDeviceClasses() ([]models.DeviceClass, error, int) {
	return []models.DeviceClass{{Id: "dc-meter", Name: "Meter"}}, nil, 200
}

func (f *fakeClient) GetLastUpdateTimestamps(string, string) ([]model.LastUpdateTimestamp, error, int) {
	f.timestampCalls.Add(1)
	if f.timestampErr != nil {
		return nil, f.timestampErr, 500
	}
	f.mux.Lock()
	defer f.mux.Unlock()
	return []model.LastUpdateTimestamp{
		{Collection: "aspects", UnixTimestamp: f.generation - 10},
		{Collection: "functions", UnixTimestamp: f.generation},
	}, nil, 200
}

func (f *fakeClient) setGeneration(g int64) {
	f.mux.Lock()
	defer f.mux.Unlock()
	f.generation = g
}

const testToken = "test-token"

// staticFactory hands out the same fake regardless of token. The per-token
// factory exists for transport auth, not for isolation, so a shared instance
// is the right stand-in.
func staticFactory(c Client) ClientFactory {
	return func(string) Client { return c }
}

// TestFactoryReceivesTheCallersToken guards the reason the factory exists: the
// ontology methods send no Authorization header of their own, so a load that
// builds its client without the caller's token is rejected by the API gateway.
func TestFactoryReceivesTheCallersToken(t *testing.T) {
	fake := newFakeClient()
	var got []string
	repo := New(func(token string) Client {
		got = append(got, token)
		return fake
	}, Options{})

	if _, err := repo.Snapshot(context.Background(), "Bearer abc"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the factory was never called")
	}
	for _, token := range got {
		if token != "Bearer abc" {
			t.Errorf("factory got token %q, want the caller's token", token)
		}
	}
}

func TestSnapshotLoadsTheWholeOntology(t *testing.T) {
	repo := New(staticFactory(newFakeClient()), Options{})

	snap, err := repo.Snapshot(context.Background(), testToken)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.AspectNodes) != 1 {
		t.Errorf("AspectNodes = %d, want 1", len(snap.AspectNodes))
	}
	if len(snap.MeasuringFunctions) != 1 || snap.MeasuringFunctions[0].Id != "fn-power" {
		t.Errorf("MeasuringFunctions = %+v", snap.MeasuringFunctions)
	}
	if len(snap.ControllingFunctions) != 1 || snap.ControllingFunctions[0].Id != "fn-switch" {
		t.Errorf("ControllingFunctions = %+v", snap.ControllingFunctions)
	}
	if len(snap.Characteristics) != 1 || len(snap.Concepts) != 1 || len(snap.DeviceClasses) != 1 {
		t.Errorf("snapshot is missing parts: %+v", snap)
	}
	if snap.LoadedAt.IsZero() {
		t.Error("LoadedAt was not stamped")
	}
}

func TestSnapshotServesTheCacheOnTheSecondCall(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{TTL: time.Hour, InvalidateInterval: time.Hour})

	for i := 0; i < 3; i++ {
		if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
	}
	if got := fake.aspectCalls.Load(); got != 1 {
		t.Errorf("aspect-node reads = %d, want 1", got)
	}
}

func TestSnapshotReloadsAfterTheTtlExpires(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{TTL: time.Nanosecond, InvalidateInterval: time.Hour})

	if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
		t.Fatalf("first: %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := fake.aspectCalls.Load(); got != 2 {
		t.Errorf("aspect-node reads = %d, want 2", got)
	}
}

// The point of the probe: a change in the device repository has to reach ODE
// well before the hour-long TTL is up.
func TestSnapshotReloadsWhenThePlatformReportsANewerGeneration(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{TTL: time.Hour, InvalidateInterval: time.Nanosecond})

	if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
		t.Fatalf("first: %v", err)
	}
	fake.setGeneration(2000)
	time.Sleep(time.Millisecond)

	if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := fake.aspectCalls.Load(); got != 2 {
		t.Errorf("aspect-node reads = %d, want 2 after the generation moved", got)
	}
}

// The inverse, and the bug this design is prone to: an unchanged platform must
// not cause a reload every time the probe interval elapses.
func TestSnapshotDoesNotReloadWhileTheGenerationIsUnchanged(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{TTL: time.Hour, InvalidateInterval: time.Nanosecond})

	for i := 0; i < 4; i++ {
		if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}
	if got := fake.aspectCalls.Load(); got != 1 {
		t.Errorf("aspect-node reads = %d, want 1: an unchanged generation must not reload", got)
	}
	if got := fake.timestampCalls.Load(); got < 2 {
		t.Errorf("timestamp probes = %d, want the probe to actually be running", got)
	}
}

func TestSnapshotRateLimitsTheGenerationProbe(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{TTL: time.Hour, InvalidateInterval: time.Hour})

	for i := 0; i < 5; i++ {
		if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
	}
	// One probe during the initial load to stamp the generation, and none
	// after, because the interval has not elapsed.
	if got := fake.timestampCalls.Load(); got != 1 {
		t.Errorf("timestamp probes = %d, want 1", got)
	}
}

func TestSnapshotSurvivesAFailingGenerationProbe(t *testing.T) {
	fake := newFakeClient()
	fake.timestampErr = errors.New("device-repository unavailable")
	repo := New(staticFactory(fake), Options{TTL: time.Hour, InvalidateInterval: time.Nanosecond})

	// A probe that cannot answer must not fail the request that triggered it.
	for i := 0; i < 2; i++ {
		if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}
	if got := fake.aspectCalls.Load(); got != 1 {
		t.Errorf("aspect-node reads = %d, want 1", got)
	}
}

// Without a token there is nothing to probe with, and ODE holds no service
// account for platform reads (SPEC D5).
func TestSnapshotSkipsTheProbeWithoutAToken(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{TTL: time.Hour, InvalidateInterval: time.Nanosecond})

	for i := 0; i < 3; i++ {
		if _, err := repo.Snapshot(context.Background(), ""); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}
	if got := fake.timestampCalls.Load(); got != 0 {
		t.Errorf("timestamp probes = %d, want 0 without a token", got)
	}
}

func TestConcurrentCallersOnAColdCacheProduceOneLoad(t *testing.T) {
	fake := newFakeClient()
	fake.delay = 20 * time.Millisecond
	repo := New(staticFactory(fake), Options{TTL: time.Hour, InvalidateInterval: time.Hour})

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := repo.Snapshot(context.Background(), testToken); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Snapshot: %v", err)
	}
	if got := fake.aspectCalls.Load(); got != 1 {
		t.Errorf("aspect-node reads = %d, want 1: concurrent callers must share one load", got)
	}
}

func TestSnapshotReportsWhichUpstreamResourceFailed(t *testing.T) {
	fake := newFakeClient()
	fake.aspectErr = errors.New("boom")
	fake.aspectCode = 503
	repo := New(staticFactory(fake), Options{})

	_, err := repo.Snapshot(context.Background(), testToken)
	if err == nil {
		t.Fatal("expected an error when the device repository fails")
	}
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %v, want an *UpstreamError", err)
	}
	if upstreamErr.Resource != "aspect-nodes" {
		t.Errorf("Resource = %q, want %q", upstreamErr.Resource, "aspect-nodes")
	}
	if upstreamErr.Code != 503 {
		t.Errorf("Code = %d, want 503", upstreamErr.Code)
	}
}

func TestSnapshotDoesNotCacheAFailedLoad(t *testing.T) {
	fake := newFakeClient()
	fake.aspectErr = errors.New("boom")
	repo := New(staticFactory(fake), Options{})

	if _, err := repo.Snapshot(context.Background(), testToken); err == nil {
		t.Fatal("expected the first load to fail")
	}
	fake.aspectErr = nil
	snap, err := repo.Snapshot(context.Background(), testToken)
	if err != nil {
		t.Fatalf("expected recovery once the platform answers, got %v", err)
	}
	if len(snap.AspectNodes) != 1 {
		t.Errorf("AspectNodes = %d, want 1", len(snap.AspectNodes))
	}
}

func TestSnapshotHonoursACancelledContext(t *testing.T) {
	repo := New(staticFactory(newFakeClient()), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := repo.Snapshot(ctx, testToken); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestNewAppliesDefaultIntervals(t *testing.T) {
	repo := New(staticFactory(newFakeClient()), Options{})
	if repo.opts.TTL != defaultTTL {
		t.Errorf("TTL = %v, want %v", repo.opts.TTL, defaultTTL)
	}
	if repo.opts.InvalidateInterval != defaultInvalidateInterval {
		t.Errorf("InvalidateInterval = %v, want %v", repo.opts.InvalidateInterval, defaultInvalidateInterval)
	}
}
