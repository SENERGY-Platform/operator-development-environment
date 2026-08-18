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
	"net/http"
	"testing"

	"github.com/SENERGY-Platform/device-repository/lib/model"
)

// An empty criteria list is the one call to this endpoint that is actively
// harmful: upstream it is replaced with a single empty criterion, which matches
// every device type on the platform. Refusing it here is the difference between
// an error and a listing of everything.
func TestSelectablesRefusesAnEmptyCriteriaList(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{})

	_, err := repo.DeviceTypeSelectables(context.Background(), testToken, nil, SelectableOptions{})
	if !errors.Is(err, ErrNoCriteria) {
		t.Fatalf("error = %v, want ErrNoCriteria", err)
	}
	if len(fake.selectableCalls) != 0 {
		t.Errorf("the platform was called %d times, want 0", len(fake.selectableCalls))
	}
}

func TestSelectablesPassesTheCriteriaThroughUnchanged(t *testing.T) {
	fake := newFakeClient()
	fake.selectables = []model.DeviceTypeSelectable{{DeviceTypeId: "dt-meter"}}
	repo := New(staticFactory(fake), Options{})

	criteria := []model.FilterCriteria{{FunctionId: "fn-power", AspectId: "pv", Interaction: "event"}}
	found, err := repo.DeviceTypeSelectables(context.Background(), testToken, criteria, SelectableOptions{})
	if err != nil {
		t.Fatalf("DeviceTypeSelectables: %v", err)
	}
	if len(found) != 1 || found[0].DeviceTypeId != "dt-meter" {
		t.Fatalf("found = %v, want one dt-meter", found)
	}

	if len(fake.selectableCalls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.selectableCalls))
	}
	sent := fake.selectableCalls[0]
	if len(sent) != 1 || sent[0].FunctionId != "fn-power" || sent[0].AspectId != "pv" {
		t.Errorf("criteria sent = %v, want the caller's own", sent)
	}
	// The path prefix has to stay empty: the returned paths are used as
	// timescale-wrapper column names, and those start at the output's root
	// variable.
	if fake.selectablePrefix != "" {
		t.Errorf("path prefix = %q, want empty", fake.selectablePrefix)
	}
	if fake.selectableAll {
		t.Error("services_must_match_all_criteria = true, want false by default")
	}
}

// A nil result is normalised to an empty slice for the same reason the profiler
// does it: nil marshals as JSON null, and every consumer then has to tell "no
// selectables" from "not a list".
func TestSelectablesNormalisesANilResult(t *testing.T) {
	fake := newFakeClient()
	fake.selectables = nil
	repo := New(staticFactory(fake), Options{})

	found, err := repo.DeviceTypeSelectables(context.Background(), testToken,
		[]model.FilterCriteria{{FunctionId: "fn-power"}}, SelectableOptions{})
	if err != nil {
		t.Fatalf("DeviceTypeSelectables: %v", err)
	}
	if found == nil {
		t.Fatal("found = nil, want an empty slice")
	}
	if len(found) != 0 {
		t.Errorf("found = %v, want empty", found)
	}
}

func TestSelectablesReportsTheUpstreamStatus(t *testing.T) {
	fake := newFakeClient()
	fake.selectableErr = errors.New("nope")
	fake.selectableCode = http.StatusForbidden
	repo := New(staticFactory(fake), Options{})

	_, err := repo.DeviceTypeSelectables(context.Background(), testToken,
		[]model.FilterCriteria{{FunctionId: "fn-power"}}, SelectableOptions{})

	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %v, want an UpstreamError", err)
	}
	if upstreamErr.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403 — the platform's own verdict has to survive", upstreamErr.Code)
	}
}

func TestSelectablesHonoursACancelledContext(t *testing.T) {
	fake := newFakeClient()
	repo := New(staticFactory(fake), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.DeviceTypeSelectables(ctx, testToken,
		[]model.FilterCriteria{{FunctionId: "fn-power"}}, SelectableOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(fake.selectableCalls) != 0 {
		t.Error("a cancelled request still reached the platform")
	}
}

// The factory exists because these methods set no Authorization header of their
// own (see ClientFactory). Selectables is one of them, so a client built without
// the caller's token is a 401 from the gateway.
func TestSelectablesUsesATokenBoundClient(t *testing.T) {
	fake := newFakeClient()
	var tokens []string
	repo := New(func(token string) Client {
		tokens = append(tokens, token)
		return fake
	}, Options{})

	if _, err := repo.DeviceTypeSelectables(context.Background(), "Bearer caller",
		[]model.FilterCriteria{{FunctionId: "fn-power"}}, SelectableOptions{}); err != nil {
		t.Fatalf("DeviceTypeSelectables: %v", err)
	}
	if len(tokens) != 1 || tokens[0] != "Bearer caller" {
		t.Fatalf("factory tokens = %v, want the caller's", tokens)
	}
}
