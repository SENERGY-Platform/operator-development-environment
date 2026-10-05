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
	if m.CharacteristicID != "ch-watt" || m.FunctionID != "fn-power" ||
		len(m.AspectIDs) != 1 || m.AspectIDs[0] != "aspect-pv" {
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

// aspectSetDevice is oneServiceDevice's shape with an explicit aspect_ids list
// instead of the deprecated single alias, for the alias-collision tests below.
// The origin device (fromDeviceID) always uses this shape; onDifferentPath
// gives the candidate a different content variable name so that only a
// semantic match — never a path match — can find its counterpart.
func aspectSetDevice(deviceID, deviceTypeID, serviceID string, aspectIDs []string, onDifferentPath bool) models.ExtendedDevice {
	root, leaf, displayName := "value", "power", "Meter"
	if onDifferentPath {
		root, leaf, displayName = "reading", "watt", "Other meter"
	}
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: displayName, DeviceTypeId: deviceTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: deviceTypeID, Name: displayName,
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: root, Type: models.Structure,
						SubContentVariables: []models.ContentVariable{{
							Id: "cv-leaf", Name: leaf, Type: models.Float,
							CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectIds: aspectIDs,
						}},
					},
				}},
			}},
		},
	}
}

// The exact counter-example docs/aspect-identity.md names: two variables share
// only the alphabetically-first aspect id ("electricity") and are not the same
// quantity, because the device repository's deprecated AspectId alias holds
// only that one entry. The candidate's path differs from the origin's, so only
// the semantic branch of findCounterpart can produce a match here — comparing
// the alias instead of the whole set would wrongly treat "power [electricity,
// kitchen]" and "power [electricity, living_room]" as the same series.
func TestRetargetRefusesACounterpartThatOnlySharesTheAspectAlias(t *testing.T) {
	from := aspectSetDevice(fromDeviceID, fromTypeID, fromServiceID, []string{"electricity", "kitchen"}, false)
	to := aspectSetDevice("urn:infai:ses:device:to-alias", "dt-to-alias",
		"urn:infai:ses:service:to-alias-aaaa", []string{"electricity", "living_room"}, true)

	_, err := experiments.Retarget(powerTopic("value.power"), from, to)
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest: the two devices share only the aspect alias, not the aspect set", err)
	}
}

// The positive case beside the refusal above: the same aspect set, listed in a
// different order on each side, still finds its counterpart by semantics.
func TestRetargetMatchesByAspectSetRegardlessOfListOrder(t *testing.T) {
	from := aspectSetDevice(fromDeviceID, fromTypeID, fromServiceID, []string{"electricity", "kitchen"}, false)
	to := aspectSetDevice("urn:infai:ses:device:to-same-set", "dt-to-same-set",
		"urn:infai:ses:service:to-same-set-aaaa", []string{"kitchen", "electricity"}, true)

	resolved, err := experiments.Retarget(powerTopic("value.power"), from, to)
	if err != nil {
		t.Fatalf("Retarget: %v", err)
	}
	if len(resolved.Mappings) != 1 || resolved.Mappings[0].VariablePath != "reading.watt" {
		t.Errorf("mappings = %+v, want the counterpart found by the same aspect set", resolved.Mappings)
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

// --- FitOf ---

// twoUnrelatedVariablesDevice carries two variables that match neither
// oneServiceDevice's power (fn-power/aspect-pv) nor its total
// (fn-energy/aspect-pv) by path or by semantics, so a topic aimed at it can
// only ever be guessed from position — the shape RetargetToService's fallback
// needs two of, to prove a second guessed mapping does not collide with the
// first. cv-flow sorts before cv-temp (profiler.ServiceVariables orders by
// ContentVariable id), so "flow" is always the first unclaimed candidate.
func twoUnrelatedVariablesDevice(deviceID, deviceTypeID, serviceID string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: "Two unrelated", DeviceTypeId: deviceTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: deviceTypeID, Name: "Two unrelated",
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: "value", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{
							{
								Id: "cv-flow", Name: "flow", Type: models.Float,
								CharacteristicId: "ch-flow", FunctionId: "fn-flow", AspectId: "aspect-room",
							},
							{
								Id: "cv-temp", Name: "temp", Type: models.Float,
								CharacteristicId: "ch-temp", FunctionId: "fn-temp", AspectId: "aspect-room",
							},
						},
					},
				}},
			}},
		},
	}
}

// emptyServiceDevice has one service with no outputs at all — the shape
// RetargetToService's fallback needs to prove it refuses rather than guessing
// when a service has nothing queryable to guess from.
func emptyServiceDevice(deviceID, deviceTypeID, serviceID string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device:          models.Device{Id: deviceID, Name: "Empty", DeviceTypeId: deviceTypeID},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: deviceTypeID, Name: "Empty",
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{},
			}},
		},
	}
}

// FitOf is the same rule findCounterpart already applies inside Retarget,
// exposed so a candidate listing can compute it per device without retargeting
// each one. These three tests are its path, semantic and no-match outcomes —
// TestRetargetPreservesEachSourceConvention and
// TestRetargetMatchesBySemanticsAndWarnsOnCharacteristicChange already cover
// the same rule from inside Retarget itself.
func TestFitOfMatchesByPath(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:fit-path-aaaa"
	to := oneServiceDevice("urn:infai:ses:device:fit-path", "dt-fit-path", toServiceID)

	fit := experiments.FitOf(powerTopic("value.power"), from, to)
	if fit.Match != experiments.FitPath {
		t.Fatalf("match = %q, want %q", fit.Match, experiments.FitPath)
	}
	if fit.Service == nil || fit.Service.ID != toServiceID {
		t.Errorf("service = %+v, want the matched service %s", fit.Service, toServiceID)
	}
	if fit.Reason != "" {
		t.Errorf("reason = %q, want none on a path match", fit.Reason)
	}
}

func TestFitOfMatchesBySemantics(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:fit-semantic-aaaa"
	to := renamedPowerDevice("urn:infai:ses:device:fit-semantic", "dt-fit-semantic", toServiceID)

	fit := experiments.FitOf(powerTopic("value.power"), from, to)
	if fit.Match != experiments.FitSemantics {
		t.Fatalf("match = %q, want %q", fit.Match, experiments.FitSemantics)
	}
	if fit.Service == nil || fit.Service.ID != toServiceID {
		t.Errorf("service = %+v, want the matched service %s", fit.Service, toServiceID)
	}
}

// A device that matches on neither path nor function and aspect reports
// FitNone with the reason text the /input-topics/candidates response carries
// verbatim (see docs/... and the JSON contract fixture).
func TestFitOfReportsNoMatch(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	to := unrelatedDevice("urn:infai:ses:device:fit-none", "dt-fit-none",
		"urn:infai:ses:service:fit-none-aaaa")

	fit := experiments.FitOf(powerTopic("value.power"), from, to)
	if fit.Match != experiments.FitNone {
		t.Fatalf("match = %q, want %q", fit.Match, experiments.FitNone)
	}
	if fit.Reason != "no variable with the same path or the same function and aspect" {
		t.Errorf("reason = %q", fit.Reason)
	}
	// Named anyway, and that is what makes this device pickable: the card hands the
	// service back as serviceID and RetargetToService guesses from there.
	if fit.Service == nil || fit.Service.ID != "urn:infai:ses:service:fit-none-aaaa" {
		t.Fatalf("service = %+v, want the one service this topic can land on", fit.Service)
	}
	if _, err := experiments.RetargetToService(powerTopic("value.power"), from, to, fit.Service.ID); err != nil {
		t.Errorf("the offered service does not resolve: %v", err)
	}
}

// A fit is about the whole topic, not about its first mapping.
//
// Mapping 0 alone picks the service, and every later mapping has to come out of
// that one service. Here the power counterpart sits on a service that carries
// nothing else, so the energy mapping has neither a counterpart nor a spare
// variable to be guessed from and the move refuses. Reporting FitPath because
// mapping 0 matched would put a row in the picker that promises a landing service
// and fails on the click.
func TestFitOfReportsNoMatchWhenALaterMappingCannotLandOnTheMatchedService(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	to := splitAcrossTwoServices("urn:infai:ses:device:fit-split", "dt-fit-split",
		"urn:infai:ses:service:fit-split-power", "urn:infai:ses:service:fit-split-total")
	topic := meterTopic("value.power", "value.total")

	fit := experiments.FitOf(topic, from, to)
	if fit.Match != experiments.FitNone {
		t.Fatalf("match = %q, want %q: mapping 0 matches by path, but the topic does not resolve",
			fit.Match, experiments.FitNone)
	}
	if !strings.Contains(fit.Reason, "total_out") {
		t.Errorf("reason = %q, want the mapping that could not be placed", fit.Reason)
	}
	if strings.Contains(fit.Reason, experiments.ErrInvalidRequest.Error()) {
		t.Errorf("reason = %q, want the sentinel wrapper dropped", fit.Reason)
	}
	// The service named back is the one the topic can land on, not the one that
	// just refused. Handing power-readings back would fail on the click a second
	// time, having said the row was pickable.
	if fit.Service == nil || fit.Service.Name != "total-readings" {
		t.Fatalf("service = %+v, want the service the topic can land on", fit.Service)
	}
	if _, err := experiments.RetargetToService(topic, from, to, fit.Service.ID); err != nil {
		t.Errorf("the offered service does not resolve: %v", err)
	}

	// The claim the fit makes is the one the click tests, so the two have to agree.
	if _, err := experiments.Retarget(topic, from, to); err == nil {
		t.Error("Retarget succeeded where FitOf reported no match")
	}
}

// A FitNone that no service choice can rescue names none. The origin topic names
// a service the origin device type does not have, so RetargetToService refuses
// before it ever looks at the target — naming one would promise a move that cannot
// happen.
func TestFitOfNamesNoServiceWhenTheOriginIsWhatFails(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	to := oneServiceDevice("urn:infai:ses:device:fit-origin", "dt-fit-origin",
		"urn:infai:ses:service:fit-origin-aaaa")

	topic := powerTopic("value.power")
	topic.Name = "urn_infai_ses_service_not_on_the_origin"

	fit := experiments.FitOf(topic, from, to)
	if fit.Match != experiments.FitNone {
		t.Fatalf("match = %q, want %q", fit.Match, experiments.FitNone)
	}
	if fit.Service != nil {
		t.Errorf("service = %+v, want none when the origin is what fails", fit.Service)
	}
}

// --- RetargetToService ---

// A service with no counterpart for either mapping still resolves: each
// mapping falls back to the first queryable variable nothing else has claimed,
// marked Guessed, with a warning naming it. The second mapping proves the
// fallback does not hand out the same variable twice.
func TestRetargetToServiceGuessesADefaultWhenTheChosenServiceHasNoCounterpart(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:guess-target-aaaa"
	to := twoUnrelatedVariablesDevice("urn:infai:ses:device:guess-target", "dt-guess-target", toServiceID)

	resolved, err := experiments.RetargetToService(
		meterTopic("value.power", "value.total"), from, to, toServiceID)
	if err != nil {
		t.Fatalf("RetargetToService: %v", err)
	}
	if len(resolved.Mappings) != 2 {
		t.Fatalf("mappings = %+v", resolved.Mappings)
	}
	first, second := resolved.Mappings[0], resolved.Mappings[1]
	if !first.Guessed || !second.Guessed {
		t.Errorf("guessed = %v/%v, want both mappings guessed", first.Guessed, second.Guessed)
	}
	if first.VariablePath == second.VariablePath {
		t.Errorf("both mappings guessed %q, want the second to skip a variable the first already claimed",
			first.VariablePath)
	}
	if first.VariablePath != "value.flow" {
		t.Errorf("first guess = %q, want the first unclaimed queryable variable (value.flow)", first.VariablePath)
	}
	var namedFirst, namedSecond bool
	for _, w := range resolved.Warnings {
		if strings.Contains(w, "power_out") && strings.Contains(w, "guessed") {
			namedFirst = true
		}
		if strings.Contains(w, "total_out") && strings.Contains(w, "guessed") {
			namedSecond = true
		}
	}
	if !namedFirst || !namedSecond {
		t.Errorf("warnings = %v, want one naming each guessed mapping", resolved.Warnings)
	}
}

func TestRetargetToServiceRefusesAnUnknownService(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	to := oneServiceDevice("urn:infai:ses:device:unknown-service", "dt-unknown-service",
		"urn:infai:ses:service:unknown-service-real")

	_, err := experiments.RetargetToService(powerTopic("value.power"), from, to,
		"urn:infai:ses:service:does-not-exist")
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error = %q, want it to name the missing service", err.Error())
	}
}

func TestRetargetToServiceRefusesWhenTheServiceHasNoQueryableVariableToGuessFrom(t *testing.T) {
	from := oneServiceDevice(fromDeviceID, fromTypeID, fromServiceID)
	toServiceID := "urn:infai:ses:service:empty-target-aaaa"
	to := emptyServiceDevice("urn:infai:ses:device:empty-target", "dt-empty-target", toServiceID)

	_, err := experiments.RetargetToService(powerTopic("value.power"), from, to, toServiceID)
	if !errors.Is(err, experiments.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "power_out") {
		t.Errorf("error = %q, want it to name the failing mapping's dest", err.Error())
	}
}
