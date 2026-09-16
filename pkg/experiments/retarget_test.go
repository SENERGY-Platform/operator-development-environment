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
	"errors"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// --- fixtures ---
//
// Modelled on pkg/profiler/fixtures_test.go's meterDevice (:275): one service
// carrying an instantaneous power reading and a cumulative energy counter, which
// is the shape a mapping's source-versus-path derivation (correction 9) needs
// two of to be checked meaningfully.

const (
	fromDeviceID  = "urn:infai:ses:device:from"
	fromTypeID    = "dt-from"
	fromServiceID = "urn:infai:ses:service:from-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

func topicName(serviceID string) string {
	return strings.ReplaceAll(serviceID, ":", "_")
}

// oneServiceDevice is one service with a power reading and an energy counter on
// the same message, addressed at "value.power" and "value.total".
func oneServiceDevice(deviceID, deviceTypeID, serviceID string) models.ExtendedDevice {
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
						SubContentVariables: []models.ContentVariable{
							{
								Id: "cv-power", Name: "power", Type: models.Float,
								CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectId: "aspect-pv",
							},
							{
								Id: "cv-total", Name: "total", Type: models.Float,
								CharacteristicId: "ch-watthour", FunctionId: "fn-energy", AspectId: "aspect-pv",
							},
						},
					},
				}},
			}},
		},
	}
}

// renamedPowerDevice reads the same thing oneServiceDevice's "power" does
// (fn-power/aspect-pv) but under a different path and a different
// characteristic — the shape a semantic match and its characteristic-mismatch
// warning need.
func renamedPowerDevice(deviceID, deviceTypeID, serviceID string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: "Other meter", DeviceTypeId: deviceTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: deviceTypeID, Name: "Other meter",
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-reading", Name: "reading", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{{
							Id: "cv-watt", Name: "watt", Type: models.Float,
							CharacteristicId: "ch-kilowatt", FunctionId: "fn-power", AspectId: "aspect-pv",
						}},
					},
				}},
			}},
		},
	}
}

// unrelatedDevice matches the original mapping on nothing: different path,
// different function, different aspect.
func unrelatedDevice(deviceID, deviceTypeID, serviceID string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: "Unrelated", DeviceTypeId: deviceTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: deviceTypeID, Name: "Unrelated",
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-humidity", Name: "humidity", Type: models.Float,
						CharacteristicId: "ch-percent", FunctionId: "fn-humidity", AspectId: "aspect-room",
					},
				}},
			}},
		},
	}
}

// splitAcrossTwoServices carries the power counterpart on one service and the
// energy counterpart on a second, so a topic whose mappings resolve to both
// (mapping 0 picks the power service) cannot be retargeted here.
func splitAcrossTwoServices(deviceID, deviceTypeID, powerServiceID, totalServiceID string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: "Split meter", DeviceTypeId: deviceTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: deviceTypeID, Name: "Split meter",
			Services: []models.Service{
				{
					Id: powerServiceID, Name: "power-readings", Interaction: models.EVENT,
					Outputs: []models.Content{{
						ContentVariable: models.ContentVariable{
							Id: "cv-root", Name: "value", Type: models.Structure,
							SubContentVariables: []models.ContentVariable{{
								Id: "cv-power", Name: "power", Type: models.Float,
								CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectId: "aspect-pv",
							}},
						},
					}},
				},
				{
					Id: totalServiceID, Name: "total-readings", Interaction: models.EVENT,
					Outputs: []models.Content{{
						ContentVariable: models.ContentVariable{
							Id: "cv-root2", Name: "value", Type: models.Structure,
							SubContentVariables: []models.ContentVariable{{
								Id: "cv-total", Name: "total", Type: models.Float,
								CharacteristicId: "ch-watthour", FunctionId: "fn-energy", AspectId: "aspect-pv",
							}},
						},
					}},
				},
			},
		},
	}
}

// powerTopic is a one-mapping topic reading oneServiceDevice's power variable,
// in whichever source convention the caller is checking.
func powerTopic(source string) experiments.InputTopic {
	return experiments.InputTopic{
		Name:        topicName(fromServiceID),
		FilterType:  "DeviceId",
		FilterValue: fromDeviceID,
		Mappings:    []experiments.TopicMapping{{Dest: "power_out", Source: source}},
	}
}

// meterTopic reads both of oneServiceDevice's variables off the one service.
func meterTopic(powerSource, totalSource string) experiments.InputTopic {
	return experiments.InputTopic{
		Name:        topicName(fromServiceID),
		FilterType:  "DeviceId",
		FilterValue: fromDeviceID,
		Mappings: []experiments.TopicMapping{
			{Dest: "power_out", Source: powerSource},
			{Dest: "total_out", Source: totalSource},
		},
	}
}

// --- tests ---

func TestDescribeRendersATopicAgainstItsOwnDevice(t *testing.T) {
	device := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)

	resolved, err := experiments.Describe(powerTopic("value.power"), device)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if resolved.Device.ID != fromDeviceID || resolved.Device.DeviceTypeID != fromTypeID {
		t.Errorf("device = %+v", resolved.Device)
	}
	if resolved.Service.ID != fromServiceID {
		t.Errorf("service = %+v", resolved.Service)
	}
	if len(resolved.Mappings) != 1 {
		t.Fatalf("mappings = %+v", resolved.Mappings)
	}
	m := resolved.Mappings[0]
	if m.Dest != "power_out" || m.Source != "value.power" {
		t.Errorf("dest/source = %q/%q, want unchanged", m.Dest, m.Source)
	}
	if m.VariableName != "power" || m.VariablePath != "value.power" {
		t.Errorf("variable = %+v", m)
	}
	if m.CharacteristicID != "ch-watt" || m.FunctionID != "fn-power" || m.AspectID != "aspect-pv" {
		t.Errorf("semantics = %+v", m)
	}
	if len(resolved.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", resolved.Warnings)
	}

	// The service's other queryable variable is offered as an alternative, in
	// the same (identity) source convention as the resolved mapping.
	foundTotal := false
	for _, alt := range resolved.Alternatives {
		if alt.VariablePath == "value.total" {
			foundTotal = true
			if alt.Source != "value.total" {
				t.Errorf("alternative source = %q, want the identity convention", alt.Source)
			}
		}
		if alt.VariablePath == "value.power" {
			t.Errorf("alternatives = %+v, want the already-mapped variable excluded", resolved.Alternatives)
		}
	}
	if !foundTotal {
		t.Errorf("alternatives = %+v, want the service's other variable", resolved.Alternatives)
	}
}

func TestDescribeRefusesATopicNotFilteredByDeviceId(t *testing.T) {
	device := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	topic := powerTopic("value.power")
	topic.FilterType = "OperatorId"

	_, err := experiments.Describe(topic, device)
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// Both source conventions the repository is known to carry retarget to a same-shaped device type and come back in their
// own convention: the transform is derived from what resolved, not hard-coded.
func TestRetargetPreservesEachSourceConvention(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:to-path-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	to := oneServiceDevice("urn:infai:ses:device:to-path", "dt-to-path", toServiceID)

	cases := []struct {
		name       string
		source     string
		wantSource string
	}{
		{"identity convention", "value.power", "value.power"},
		{"suffix convention", "value.power.value", "value.power.value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := experiments.Retarget(powerTopic(tc.source), from, to)
			if err != nil {
				t.Fatalf("Retarget: %v", err)
			}
			if len(resolved.Topic.Mappings) != 1 {
				t.Fatalf("mappings = %+v", resolved.Topic.Mappings)
			}
			got := resolved.Topic.Mappings[0]
			if got.Dest != "power_out" {
				t.Errorf("dest = %q, want unchanged", got.Dest)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
			if resolved.Topic.FilterValue != to.Id {
				t.Errorf("filterValue = %q, want the new device %s", resolved.Topic.FilterValue, to.Id)
			}
			if resolved.Topic.FilterType != "DeviceId" {
				t.Errorf("filterType = %q, want DeviceId", resolved.Topic.FilterType)
			}
			if resolved.Topic.Name != topicName(toServiceID) {
				t.Errorf("name = %q, want the matched service id with : replaced by _ (%q)",
					resolved.Topic.Name, topicName(toServiceID))
			}
			for _, w := range resolved.Warnings {
				if strings.Contains(w, "matched by function and aspect") {
					t.Errorf("unexpected semantic-match warning on a path match: %s", w)
				}
			}
		})
	}
}

func TestRetargetMatchesBySemanticsAndWarnsOnCharacteristicChange(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:to-semantic-aaaa"
	to := renamedPowerDevice("urn:infai:ses:device:to-semantic", "dt-to-semantic", toServiceID)

	resolved, err := experiments.Retarget(powerTopic("value.power"), from, to)
	if err != nil {
		t.Fatalf("Retarget: %v", err)
	}
	if len(resolved.Mappings) != 1 {
		t.Fatalf("mappings = %+v", resolved.Mappings)
	}
	m := resolved.Mappings[0]
	if m.Dest != "power_out" {
		t.Errorf("dest = %q, want unchanged", m.Dest)
	}
	if m.VariablePath != "reading.watt" || m.VariableName != "watt" {
		t.Errorf("mapping = %+v, want the renamed variable chosen by semantics", m)
	}
	if resolved.Topic.Mappings[0].Source != "reading.watt" {
		t.Errorf("source = %q, want the identity form of the new path", resolved.Topic.Mappings[0].Source)
	}

	foundReplacedPath, foundCharacteristicChange := false, false
	for _, w := range resolved.Warnings {
		if strings.Contains(w, "value.power") && strings.Contains(w, "reading.watt") {
			foundReplacedPath = true
		}
		if strings.Contains(w, "ch-watt") && strings.Contains(w, "ch-kilowatt") {
			foundCharacteristicChange = true
		}
	}
	if !foundReplacedPath {
		t.Errorf("warnings = %v, want one naming the path a semantic match replaced", resolved.Warnings)
	}
	if !foundCharacteristicChange {
		t.Errorf("warnings = %v, want one naming the characteristic change", resolved.Warnings)
	}
}

func TestRetargetRefusesWhenNoCounterpartExists(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	to := unrelatedDevice("urn:infai:ses:device:to-unrelated", "dt-to-unrelated",
		"urn:infai:ses:service:to-unrelated-aaaa")

	_, err := experiments.Retarget(powerTopic("value.power"), from, to)
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "power_out") {
		t.Errorf("error = %q, want it to name the failing mapping's dest", err.Error())
	}
}

func TestRetargetRefusesWhenMappingsWouldSpanTwoServices(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	to := splitAcrossTwoServices("urn:infai:ses:device:to-split", "dt-to-split",
		"urn:infai:ses:service:to-split-power", "urn:infai:ses:service:to-split-total")

	_, err := experiments.Retarget(meterTopic("value.power", "value.total"), from, to)
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "total_out") {
		t.Errorf("error = %q, want it to name the mapping that has no counterpart in the chosen service",
			err.Error())
	}
}

func TestRetargetRefusesATopicNotFilteredByDeviceId(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	to := oneServiceDevice("urn:infai:ses:device:to-path", "dt-to-path",
		"urn:infai:ses:service:to-path-aaaa")

	topic := powerTopic("value.power")
	topic.FilterType = "OperatorId"

	_, err := experiments.Retarget(topic, from, to)
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestValidateResolvableTopicNeedsAFilterValueAndAMapping(t *testing.T) {
	if err := experiments.ValidateResolvableTopic(experiments.InputTopic{
		FilterType: "DeviceId", Mappings: []experiments.TopicMapping{{Dest: "d", Source: "s"}},
	}); !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Errorf("empty filterValue: err = %v, want ErrInvalidRequest", err)
	}
	if err := experiments.ValidateResolvableTopic(experiments.InputTopic{
		FilterType: "DeviceId", FilterValue: "urn:infai:ses:device:x",
	}); !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Errorf("empty mappings: err = %v, want ErrInvalidRequest", err)
	}
}

/*
 * An accidental resolve is warned about rather than presented as a derivation.
 *
 * "value.power.total" is not a path on this device type. The fallback that
 * strips the last segment finds "value.power" anyway and keeps "total" as the
 * message-shape literal, which is indistinguishable, to the code, from a
 * deployment whose sources genuinely carry an extra segment. It then appends
 * that literal to whatever the counterpart's path is — a well-formed source that
 * addresses nothing. The derivation cannot tell the two apart; what it must not
 * do is make the assumption silently.
 */
func TestRetargetWarnsWhenTheSourceWasReadWithALeftoverSegment(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:to-leftover-aaaa"
	to := oneServiceDevice("urn:infai:ses:device:to-leftover", "dt-to-leftover", toServiceID)

	resolved, err := experiments.Retarget(powerTopic("value.power.total"), from, to)
	if err != nil {
		t.Fatalf("Retarget: %v", err)
	}
	if got := resolved.Topic.Mappings[0].Source; got != "value.power.total" {
		t.Errorf("source = %q, want the leftover carried through", got)
	}
	var named bool
	for _, w := range resolved.Warnings {
		if strings.Contains(w, `"total"`) && strings.Contains(w, "value.power") {
			named = true
		}
	}
	if !named {
		t.Errorf("warnings = %v, want one naming the leftover segment and the variable it assumed",
			resolved.Warnings)
	}
}

// A path match on the whole source assumes nothing, so it warns about nothing.
func TestRetargetDoesNotWarnAboutTheMessageShapeOnAnExactMatch(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:to-exact-aaaa"
	to := oneServiceDevice("urn:infai:ses:device:to-exact", "dt-to-exact", toServiceID)

	resolved, err := experiments.Retarget(powerTopic("value.power"), from, to)
	if err != nil {
		t.Fatalf("Retarget: %v", err)
	}
	for _, w := range resolved.Warnings {
		if strings.Contains(w, "message shape") {
			t.Errorf("unexpected message-shape warning on an exact match: %s", w)
		}
	}
}

// The prefix branch is the mirror of the suffix one and was previously covered
// by nothing: a source whose first segment is the envelope rather than its last.
func TestDescribeReadsAPrefixedSourceAndSaysSo(t *testing.T) {
	device := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)

	resolved, err := experiments.Describe(powerTopic("payload.value.power"), device)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if resolved.Mappings[0].VariablePath != "value.power" {
		t.Errorf("variable path = %q, want the variable under the prefix",
			resolved.Mappings[0].VariablePath)
	}
	var named bool
	for _, w := range resolved.Warnings {
		if strings.Contains(w, `"payload"`) {
			named = true
		}
	}
	if !named {
		t.Errorf("warnings = %v, want one naming the prefix it assumed", resolved.Warnings)
	}
}

/*
 * Two mappings of one topic that sit differently in the message refuse the
 * topic. A topic is one Kafka message shape, so this is not two conventions —
 * it is a sign that at least one of the two resolved by accident, and guessing
 * which would produce a topic that reads the wrong thing under a right-looking
 * name.
 */
func TestRetargetRefusesMappingsThatDisagreeAboutTheMessageShape(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:to-mixed-aaaa"
	to := oneServiceDevice("urn:infai:ses:device:to-mixed", "dt-to-mixed", toServiceID)

	// "value.power" resolves exactly; "value.total.value" only after a segment is
	// stripped. One topic cannot be both shapes at once.
	_, err := experiments.Retarget(meterTopic("value.power", "value.total.value"), from, to)
	if err == nil {
		t.Fatal("a topic whose mappings disagree about the message shape was accepted")
	}
	if !strings.Contains(err.Error(), "one message shape") {
		t.Errorf("error = %v, want it to say a topic is one message shape", err)
	}
}
