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

package experiments_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/analytics-flow-engine/lib/access"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// recordingPermissions answers a fixed verdict and remembers what it was asked.
type recordingPermissions struct {
	asked    map[string][]string
	tokens   []string
	deny     map[string]bool
	upstream error
}

func newRecordingPermissions() *recordingPermissions {
	return &recordingPermissions{asked: map[string][]string{}, deny: map[string]bool{}}
}

func (r *recordingPermissions) UserHasExecuteAccess(resource string, ids []string, token string) (bool, error) {
	r.asked[resource] = append(r.asked[resource], ids...)
	r.tokens = append(r.tokens, token)
	if r.upstream != nil {
		return false, r.upstream
	}
	return !r.deny[resource], nil
}

func withPermissions(checker access.Checker) options {
	return func(d *experiments.Deps) { d.Access = checker }
}

// The launch authorizes the device its topic names, as the developer, before it
// spends anything.
func TestALaunchAuthorizesItsInputTopics(t *testing.T) {
	perms := newRecordingPermissions()
	h := newHarness(t, withPermissions(perms))
	h.ready()

	h.launch()

	devices := perms.asked[access.ResourceDevices]
	if len(devices) != 1 || devices[0] != "urn:infai:ses:device:2ac5436e-5538-4eb3-a448-2d77de68e915" {
		t.Errorf("devices asked about = %v, want the one the topic names", devices)
	}
	for _, token := range perms.tokens {
		if token == "" {
			t.Error("the check was made without the developer's token")
		}
	}
}

// A device the developer may not read refuses the launch, and refuses it before
// the package is built and the job submitted.
func TestALaunchNamingAnUnreadableDeviceIsRefused(t *testing.T) {
	perms := newRecordingPermissions()
	perms.deny[access.ResourceDevices] = true
	h := newHarness(t, withPermissions(perms))
	h.ready()

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})
	if err == nil {
		t.Fatal("the launch was accepted for a device the developer cannot read")
	}
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Errorf("error = %v, want it to carry ErrInvalidRequest so the route answers 4xx", err)
	}
	if len(h.ray.Jobs()) != 0 {
		t.Errorf("%d jobs submitted, want none: the refusal must come before anything is spent",
			len(h.ray.Jobs()))
	}
}

// An operator input naming a pipeline the developer cannot execute is refused
// too. ODE has no flow-engine client of its own; this works because the check is
// the flow engine's.
func TestALaunchNamingAnUnreadablePipelineIsRefused(t *testing.T) {
	perms := newRecordingPermissions()
	perms.deny[access.ResourcePipelines] = true
	h := newHarness(t, withPermissions(perms))
	h.ready()

	topics := testInputTopics()
	topics[0].FilterType = "OperatorId"
	topics[0].FilterValue = "other-operator:other-pipeline"

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: topics})
	if err == nil {
		t.Fatal("the launch was accepted for a pipeline the developer cannot execute")
	}
	if len(h.ray.Jobs()) != 0 {
		t.Errorf("%d jobs submitted, want none", len(h.ray.Jobs()))
	}
}

// A filter type nothing can resolve is refused rather than passed through: a
// topic that skips the check is a topic that is read unauthorized.
func TestALaunchWithAnUnknownFilterTypeIsRefused(t *testing.T) {
	perms := newRecordingPermissions()
	h := newHarness(t, withPermissions(perms))
	h.ready()

	topics := testInputTopics()
	topics[0].FilterType = "SomethingElse"

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: topics})
	if err == nil {
		t.Fatal("a topic with an unrecognised filter type was accepted")
	}
	if len(h.ray.Jobs()) != 0 {
		t.Errorf("%d jobs submitted, want none", len(h.ray.Jobs()))
	}
}

// A permissions service that cannot be reached must not read as permission.
func TestAnUnreachablePermissionsServiceRefusesTheLaunch(t *testing.T) {
	perms := newRecordingPermissions()
	perms.upstream = errors.New("connection refused")
	h := newHarness(t, withPermissions(perms))
	h.ready()

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})
	if err == nil {
		t.Fatal("the launch was accepted while the permissions service was unreachable")
	}
	if len(h.ray.Jobs()) != 0 {
		t.Errorf("%d jobs submitted, want none", len(h.ray.Jobs()))
	}
}

// Wiring no checker at all must be refused at construction, not tolerated: a
// deployment that forgot it would authorize nothing while looking like it worked.
func TestAServiceWithNoPermissionCheckerIsRefused(t *testing.T) {
	_, err := experiments.New(experiments.Deps{
		Workspace: nil, Repo: nil, Store: nil, IDs: nil,
		Options: experiments.Options{RayURL: "http://ray", MLflowURL: "http://mlflow"},
	})
	if err == nil {
		t.Fatal("a service was built with no permission checker")
	}
	if !strings.Contains(err.Error(), "workspace") && !strings.Contains(err.Error(), "permission") {
		t.Errorf("error = %v, want it to name a missing dependency", err)
	}
}
