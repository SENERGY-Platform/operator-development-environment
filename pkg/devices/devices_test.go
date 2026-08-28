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

package devices

import (
	"errors"
	"net/url"
	"testing"

	"github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

type fakeClient struct {
	gotToken   string
	gotOptions model.ExtendedDeviceListOptions
	gotAction  model.AuthAction

	devices []models.ExtendedDevice
	total   int64
	err     error
	code    int
}

func (f *fakeClient) ListExtendedDevices(token string, options model.ExtendedDeviceListOptions) ([]models.ExtendedDevice, int64, error, int) {
	f.gotToken = token
	f.gotOptions = options
	if f.err != nil {
		return nil, 0, f.err, f.code
	}
	return f.devices, f.total, nil, 200
}

func (f *fakeClient) ReadExtendedDevice(id string, token string, action model.AuthAction, fullDt bool) (models.ExtendedDevice, error, int) {
	f.gotToken = token
	f.gotAction = action
	if f.err != nil {
		return models.ExtendedDevice{}, f.err, f.code
	}
	return models.ExtendedDevice{Device: models.Device{Id: id, Name: "Meter"}}, nil, 200
}

func query(raw string) url.Values {
	v, err := url.ParseQuery(raw)
	if err != nil {
		panic(err)
	}
	return v
}

// D5: the platform's per-user authorisation is the single source of
// truth, so the caller's token has to reach it unchanged.
func TestListForwardsTheCallersToken(t *testing.T) {
	fake := &fakeClient{devices: []models.ExtendedDevice{}}
	svc := New(fake)

	if _, err := svc.List("Bearer user-token", model.ExtendedDeviceListOptions{Limit: 10}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if fake.gotToken != "Bearer user-token" {
		t.Errorf("token passed upstream = %q, want the caller's token", fake.gotToken)
	}
}

func TestListReturnsAnEmptySliceRatherThanNil(t *testing.T) {
	svc := New(&fakeClient{devices: nil})

	result, err := svc.List("t", model.ExtendedDeviceListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// A nil slice serialises to JSON null, which the SPA would have to guard
	// against on every render.
	if result.Devices == nil {
		t.Error("Devices is nil, want an empty slice so it serialises as []")
	}
}

func TestListReportsTheUpstreamStatusCode(t *testing.T) {
	svc := New(&fakeClient{err: errors.New("forbidden"), code: 403})

	_, err := svc.List("t", model.ExtendedDeviceListOptions{})
	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %v, want an *UpstreamError", err)
	}
	if upstream.Code != 403 {
		t.Errorf("Code = %d, want 403", upstream.Code)
	}
}

func TestGetPassesTheRequestedPermission(t *testing.T) {
	fake := &fakeClient{}
	svc := New(fake)

	if _, err := svc.Get("t", "device-1", models.Read); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fake.gotAction != models.Read {
		t.Errorf("action = %v, want models.Read", fake.gotAction)
	}
}

func TestParseListOptionsDefaultsToAReadScopedFirstPage(t *testing.T) {
	opts, err := ParseListOptions(query(""))
	if err != nil {
		t.Fatalf("ParseListOptions: %v", err)
	}
	if opts.Limit != DefaultLimit {
		t.Errorf("Limit = %d, want %d", opts.Limit, DefaultLimit)
	}
	if opts.Offset != 0 {
		t.Errorf("Offset = %d, want 0", opts.Offset)
	}
	if opts.Permission != models.Read {
		t.Errorf("Permission = %v, want models.Read", opts.Permission)
	}
}

// The device repository reads Ids == []string{} as "match nothing", so an
// absent filter and an empty one mean opposite things.
func TestParseListOptionsLeavesAbsentFiltersNil(t *testing.T) {
	opts, err := ParseListOptions(query(""))
	if err != nil {
		t.Fatalf("ParseListOptions: %v", err)
	}
	if opts.Ids != nil {
		t.Errorf("Ids = %v, want nil", opts.Ids)
	}
	if opts.DeviceTypeIds != nil {
		t.Errorf("DeviceTypeIds = %v, want nil", opts.DeviceTypeIds)
	}
	if opts.ConnectionState != nil {
		t.Errorf("ConnectionState = %v, want nil", opts.ConnectionState)
	}
}

func TestParseListOptionsTreatsAnEmptyIdsParameterAsAbsent(t *testing.T) {
	opts, err := ParseListOptions(query("ids="))
	if err != nil {
		t.Fatalf("ParseListOptions: %v", err)
	}
	if opts.Ids != nil {
		t.Errorf("Ids = %v, want nil: an empty ids parameter must not mean 'match nothing'", opts.Ids)
	}
}

func TestParseListOptionsSplitsCommaSeparatedIds(t *testing.T) {
	opts, err := ParseListOptions(query("ids=a,%20b%20,c"))
	if err != nil {
		t.Fatalf("ParseListOptions: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(opts.Ids) != len(want) {
		t.Fatalf("Ids = %v, want %v", opts.Ids, want)
	}
	for i := range want {
		if opts.Ids[i] != want[i] {
			t.Fatalf("Ids = %v, want %v", opts.Ids, want)
		}
	}
}

func TestParseListOptionsAcceptsPaging(t *testing.T) {
	opts, err := ParseListOptions(query("limit=25&offset=50"))
	if err != nil {
		t.Fatalf("ParseListOptions: %v", err)
	}
	if opts.Limit != 25 || opts.Offset != 50 {
		t.Errorf("limit/offset = %d/%d, want 25/50", opts.Limit, opts.Offset)
	}
}

func TestParseListOptionsRejectsANegativeLimit(t *testing.T) {
	if _, err := ParseListOptions(query("limit=-1")); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

func TestParseListOptionsRejectsANonNumericLimit(t *testing.T) {
	if _, err := ParseListOptions(query("limit=all")); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

// An unbounded limit would let one request pull the entire device repository.
func TestParseListOptionsRejectsAnOversizedLimit(t *testing.T) {
	if _, err := ParseListOptions(query("limit=100000")); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

func TestParseListOptionsRejectsANegativeOffset(t *testing.T) {
	if _, err := ParseListOptions(query("offset=-5")); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

func TestParseListOptionsAcceptsOnlineAndOffline(t *testing.T) {
	for _, state := range []string{models.ConnectionStateOnline, models.ConnectionStateOffline} {
		opts, err := ParseListOptions(query("connection_state=" + state))
		if err != nil {
			t.Fatalf("connection_state=%s: %v", state, err)
		}
		if opts.ConnectionState == nil || *opts.ConnectionState != state {
			t.Errorf("ConnectionState = %v, want %q", opts.ConnectionState, state)
		}
	}
}

func TestParseListOptionsRejectsAnUnknownConnectionState(t *testing.T) {
	if _, err := ParseListOptions(query("connection_state=asleep")); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

func TestParseListOptionsCarriesSearchAndSort(t *testing.T) {
	opts, err := ParseListOptions(query("search=meter&sort=name.desc"))
	if err != nil {
		t.Fatalf("ParseListOptions: %v", err)
	}
	if opts.Search != "meter" {
		t.Errorf("Search = %q, want %q", opts.Search, "meter")
	}
	if opts.SortBy != "name.desc" {
		t.Errorf("SortBy = %q, want %q", opts.SortBy, "name.desc")
	}
}

func TestParseListOptionsRejectsANonBooleanFullDeviceType(t *testing.T) {
	if _, err := ParseListOptions(query("full_device_type=maybe")); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("error = %v, want ErrInvalidOption", err)
	}
}

// The platform keeps two names, and display_name is the one its own UIs show. A
// developer choosing between forty candidate series reads names, so ODE has to
// name a device the way the rest of the platform does.
func TestDisplayNamePrefersThePlatformsDisplayName(t *testing.T) {
	device := models.ExtendedDevice{
		Device:      models.Device{Id: "urn:infai:ses:device:1", Name: "meter-1"},
		DisplayName: "Kitchen Meter",
	}
	if got := DisplayName(device); got != "Kitchen Meter" {
		t.Errorf("DisplayName = %q, want the display name", got)
	}

	device.DisplayName = ""
	if got := DisplayName(device); got != "meter-1" {
		t.Errorf("DisplayName = %q, want the device's own name", got)
	}
}

// Never the id: an id is what a series is keyed on, not what a human reads. A
// nameless device is reported as nameless so the caller decides what to show.
func TestDisplayNameDoesNotFallBackToTheId(t *testing.T) {
	device := models.ExtendedDevice{Device: models.Device{Id: "urn:infai:ses:device:1"}}
	if got := DisplayName(device); got != "" {
		t.Errorf("DisplayName = %q, want empty rather than the id", got)
	}
}

// device_type_name is computed by the repository per request and is not always
// there; the device type itself arrives with every fulldt read, which is what a
// candidate listing does.
func TestTypeNameFallsBackToTheDeviceTypeThatArrivedWithTheDevice(t *testing.T) {
	device := models.ExtendedDevice{
		Device:         models.Device{Id: "urn:infai:ses:device:1", DeviceTypeId: "dt-meter"},
		DeviceTypeName: "SmartMeter Modbus",
	}
	if got := TypeName(device); got != "SmartMeter Modbus" {
		t.Errorf("TypeName = %q, want the computed name", got)
	}

	device.DeviceTypeName = ""
	device.DeviceType = &models.DeviceType{Id: "dt-meter", Name: "Meter"}
	if got := TypeName(device); got != "Meter" {
		t.Errorf("TypeName = %q, want the device type's own name", got)
	}

	device.DeviceType = nil
	if got := TypeName(device); got != "" {
		t.Errorf("TypeName = %q, want empty when neither is available", got)
	}
}
