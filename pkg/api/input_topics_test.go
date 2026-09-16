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

package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/api"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

const (
	inputTopicsDeviceA  = "urn:infai:ses:device:input-topics-a"
	inputTopicsDeviceB  = "urn:infai:ses:device:input-topics-b"
	inputTopicsServiceA = "urn:infai:ses:service:it-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	inputTopicsServiceB = "urn:infai:ses:service:it-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// inputTopicsMeterDevice is one service carrying an instantaneous power
// reading, the minimum shape a resolve or retarget request needs — the same
// shape pkg/experiments/retarget_test.go's oneServiceDevice builds, kept small
// here because the API tests are only about the HTTP surface (status codes,
// the role gate), not the derivation rules that file already covers.
func inputTopicsMeterDevice(deviceID, deviceTypeID, serviceID string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: "Meter", DeviceTypeId: deviceTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: deviceTypeID, Name: "Meter",
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: "value", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{{
							Id: "cv-power", Name: "power", Type: models.Float,
							CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectId: "aspect-pv",
						}},
					},
				}},
			}},
		},
	}
}

// inputTopicsHarness is newHarness (api_test.go) with an Experiments dependency
// present, which is what gates /input-topics/resolve onto the router (api.go:
// "if deps.Experiments != nil"). The handler only ever reads deps.Devices, so an
// otherwise zero-valued *experiments.Service is enough to satisfy the gate
// without paying for the real service's git/Ray/MLflow dependencies that
// experiments_test.go's much heavier harness exists to exercise.
func inputTopicsHarness(t *testing.T, serve []models.ExtendedDevice) *harness {
	t.Helper()
	deviceClient := &fakeDeviceClient{serve: serve}
	router := api.NewRouter(
		api.Config{RequiredRealmRole: "developer", Debug: false},
		api.Deps{
			Devices:     devices.New(deviceClient),
			Experiments: &experiments.Service{},
		},
	)
	return &harness{router: router, devices: deviceClient}
}

func inputTopicBody(topicDeviceID string) map[string]any {
	return map[string]any{
		"topic": map[string]any{
			"name":        strings.ReplaceAll(inputTopicsServiceA, ":", "_"),
			"filterType":  "DeviceId",
			"filterValue": topicDeviceID,
			"mappings": []map[string]any{
				{"dest": "value", "source": "value.power"},
			},
		},
	}
}

func decodeResolved(t *testing.T, body []byte) experiments.Resolved {
	t.Helper()
	var resolved experiments.Resolved
	if err := json.Unmarshal(body, &resolved); err != nil {
		t.Fatalf("decode: %v; body %s", err, body)
	}
	return resolved
}

// inputTopicsRicherMeter is a target device type whose readings service carries
// a second variable and declares a different characteristic for the first.
//
// It exists for the contract fixture rather than for a rule: retargeting onto
// inputTopicsMeterDevice's twin produces neither a warning nor an alternative,
// because the two types are identical and the service has one variable — so a
// fixture built from that pair would leave `warnings` and `alternatives` absent,
// and the frontend would be typing two fields no capture had ever shown it.
func inputTopicsRicherMeter(deviceID, deviceTypeID, serviceID string) models.ExtendedDevice {
	device := inputTopicsMeterDevice(deviceID, deviceTypeID, serviceID)
	root := &device.DeviceType.Services[0].Outputs[0].ContentVariable
	// A different characteristic on the same path: a path match that still has to
	// warn, because Operator Lib converts nothing and the operator would read
	// another unit.
	root.SubContentVariables[0].CharacteristicId = "ch-kilowatt"
	root.SubContentVariables = append(root.SubContentVariables, models.ContentVariable{
		Id: "cv-total", Name: "total", Type: models.Float,
		CharacteristicId: "ch-watthour", FunctionId: "fn-energy", AspectId: "aspect-pv",
	})
	return device
}

// TestWriteInputTopicContractFixture captures the retarget answer rather than
// the describe one. Retarget is the larger of the two — it alone can carry
// warnings and alternatives — so a capture of the smaller would let the
// frontend's types drift on exactly the fields a developer reads when the
// derivation picked something they have to correct.
func TestWriteInputTopicContractFixture(t *testing.T) {
	dir := os.Getenv("ODE_WRITE_CONTRACT")
	if dir == "" {
		t.Skip("set ODE_WRITE_CONTRACT to the fixture directory to regenerate")
	}
	h := inputTopicsHarness(t, []models.ExtendedDevice{
		inputTopicsMeterDevice(inputTopicsDeviceA, "dt-a", inputTopicsServiceA),
		inputTopicsRicherMeter(inputTopicsDeviceB, "dt-b", inputTopicsServiceB),
	})

	body := inputTopicBody(inputTopicsDeviceA)
	body["device_id"] = inputTopicsDeviceB
	w := h.post(t, "/input-topics/resolve", body, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	resolved := decodeResolved(t, w.Body.Bytes())
	// Asserted rather than assumed: a capture missing either field is the drift
	// this fixture exists to prevent, and it would be written silently.
	if len(resolved.Warnings) == 0 {
		t.Fatal("the capture carries no warning, so the frontend would type warnings blind")
	}
	if len(resolved.Alternatives) == 0 {
		t.Fatal("the capture carries no alternative, so the per-mapping dropdown would type blind")
	}

	var parsed any
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	writeFixtureValue(t, dir, "input_topic_resolved.json", parsed)
}

func TestResolveInputTopicDescribesAgainstTheProposedDevice(t *testing.T) {
	h := inputTopicsHarness(t, []models.ExtendedDevice{
		inputTopicsMeterDevice(inputTopicsDeviceA, "dt-a", inputTopicsServiceA),
	})

	w := h.post(t, "/input-topics/resolve", inputTopicBody(inputTopicsDeviceA), "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	resolved := decodeResolved(t, w.Body.Bytes())
	if resolved.Device.ID != inputTopicsDeviceA {
		t.Errorf("device id = %q, want %s", resolved.Device.ID, inputTopicsDeviceA)
	}
	if len(resolved.Mappings) != 1 || resolved.Mappings[0].VariableName != "power" {
		t.Errorf("mappings = %+v, want the power variable resolved", resolved.Mappings)
	}
	if h.devices.gotAction != models.Execute {
		t.Errorf("action = %q, want execute: this route decides which history a run will read",
			string(h.devices.gotAction))
	}
}

func TestResolveInputTopicRetargetsToAnotherDevice(t *testing.T) {
	h := inputTopicsHarness(t, []models.ExtendedDevice{
		inputTopicsMeterDevice(inputTopicsDeviceA, "dt-a", inputTopicsServiceA),
		inputTopicsMeterDevice(inputTopicsDeviceB, "dt-b", inputTopicsServiceB),
	})

	body := inputTopicBody(inputTopicsDeviceA)
	body["device_id"] = inputTopicsDeviceB
	w := h.post(t, "/input-topics/resolve", body, "developer")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", w.Code, w.Body.String())
	}
	resolved := decodeResolved(t, w.Body.Bytes())
	if resolved.Topic.FilterValue != inputTopicsDeviceB {
		t.Errorf("filterValue = %q, want the new device %s", resolved.Topic.FilterValue, inputTopicsDeviceB)
	}
	wantName := strings.ReplaceAll(inputTopicsServiceB, ":", "_")
	if resolved.Topic.Name != wantName {
		t.Errorf("name = %q, want %q", resolved.Topic.Name, wantName)
	}
	if len(resolved.Topic.Mappings) != 1 || resolved.Topic.Mappings[0].Dest != "value" {
		t.Errorf("mappings = %+v, want dest unchanged", resolved.Topic.Mappings)
	}
}

func TestResolveInputTopicRefusesATopicThatCannotBeDerived(t *testing.T) {
	h := inputTopicsHarness(t, []models.ExtendedDevice{
		inputTopicsMeterDevice(inputTopicsDeviceA, "dt-a", inputTopicsServiceA),
	})

	body := inputTopicBody(inputTopicsDeviceA)
	body["topic"].(map[string]any)["mappings"] = []map[string]any{
		{"dest": "value", "source": "no.such.path"},
	}
	w := h.post(t, "/input-topics/resolve", body, "developer")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

// A 403 from the device repository means "you may not see that device" — the
// same distinction TestUpstreamForbiddenIsForwardedAsForbidden (api_test.go)
// checks for /devices, here for the device an input topic names.
func TestResolveInputTopicForwardsADeviceReadRefusal(t *testing.T) {
	h := inputTopicsHarness(t, nil)
	h.devices.err = errors.New("forbidden")
	h.devices.code = http.StatusForbidden

	w := h.post(t, "/input-topics/resolve", inputTopicBody(inputTopicsDeviceA), "developer")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body %s", w.Code, w.Body.String())
	}
}

func TestResolveInputTopicRejectsATokenWithoutTheDeveloperRole(t *testing.T) {
	h := inputTopicsHarness(t, nil)
	w := h.post(t, "/input-topics/resolve", inputTopicBody(inputTopicsDeviceA), "offline_access")
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}
