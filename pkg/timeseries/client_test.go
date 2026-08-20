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

package timeseries_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

type capture struct {
	method string
	path   string
	query  string
	auth   string
	body   string
}

func serve(t *testing.T, status int, response string) (*timeseries.Client, *capture) {
	t.Helper()
	got := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
		got.auth, got.body = r.Header.Get("Authorization"), string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return timeseries.New(server.URL, timeseries.Options{}), got
}

func TestDataAvailabilityAsksForOneDeviceUnderTheCallersToken(t *testing.T) {
	client, got := serve(t, http.StatusOK,
		`[{"serviceId":"urn:infai:ses:service:a","from":"2026-01-01T00:00:00Z","to":"2026-08-01T00:00:00Z"}]`)

	windows, err := client.DataAvailability(context.Background(), "Bearer caller", "urn:infai:ses:device:1")
	if err != nil {
		t.Fatalf("DataAvailability: %v", err)
	}
	if got.method != http.MethodGet || got.path != "/data-availability" {
		t.Errorf("request = %s %s, want GET /data-availability", got.method, got.path)
	}
	if got.query != "device_id=urn%3Ainfai%3Ases%3Adevice%3A1" {
		t.Errorf("query = %q, want the device id", got.query)
	}
	if got.auth != "Bearer caller" {
		t.Errorf("Authorization = %q, want the caller's own token", got.auth)
	}
	if len(windows) != 1 || windows[0].ServiceId != "urn:infai:ses:service:a" {
		t.Errorf("windows = %v, want one entry for the service", windows)
	}
}

func TestDataAvailabilityRejectsAnEmptyDeviceIdWithoutCallingThePlatform(t *testing.T) {
	client, got := serve(t, http.StatusOK, `[]`)
	if _, err := client.DataAvailability(context.Background(), "Bearer caller", ""); !errors.Is(err, timeseries.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if got.method != "" {
		t.Error("the platform was called for a request ODE could reject itself")
	}
}

// SPEC §5.3 and timescale-wrapper's own swagger annotation both describe
// /usage/devices as GET. The route registration is POST with a JSON id array,
// and the route is what answers.
func TestDeviceUsageIsPostedAsAnIdArray(t *testing.T) {
	client, got := serve(t, http.StatusOK, `[{"deviceId":"d1","bytes":100,"bytesPerDay":10.5}]`)

	usage, err := client.DeviceUsage(context.Background(), "Bearer caller", []string{"d1", "d2"})
	if err != nil {
		t.Fatalf("DeviceUsage: %v", err)
	}
	if got.method != http.MethodPost || got.path != "/usage/devices" {
		t.Errorf("request = %s %s, want POST /usage/devices", got.method, got.path)
	}
	if got.body != `["d1","d2"]` {
		t.Errorf("body = %s, want a bare id array", got.body)
	}
	if len(usage) != 1 || usage[0].BytesPerDay != 10.5 {
		t.Errorf("usage = %v, want the rate the platform reported", usage)
	}
}

func TestQueryRequestsPerQueryFormatWithAnExplicitTimeLayout(t *testing.T) {
	client, got := serve(t, http.StatusOK, `[]`)

	deviceID := "urn:infai:ses:device:1"
	serviceID := "urn:infai:ses:service:11111111-1111-1111-1111-111111111111"
	_, err := client.Query(context.Background(), "Bearer caller", []timeseries.QueryElement{{
		DeviceId:  &deviceID,
		ServiceId: &serviceID,
		Columns:   []timeseries.QueryColumn{{Name: "value.power"}},
	}}, timeseries.QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if got.method != http.MethodPost || got.path != "/queries/v2" {
		t.Errorf("request = %s %s, want POST /queries/v2", got.method, got.path)
	}
	// The table format merges timestamps across series, which would destroy the
	// per-series sampling irregularity the profiler reads.
	if !strings.Contains(got.query, "format=per_query") {
		t.Errorf("query = %q, want format=per_query", got.query)
	}
	if !strings.Contains(got.query, "time_format=") {
		t.Errorf("query = %q, want an explicit time_format", got.query)
	}
}

// A locally rejected element names the series at fault; the server answers a
// bare 400.
func TestQueryRefusesAnElementTheSharedSchemaRejects(t *testing.T) {
	client, got := serve(t, http.StatusOK, `[]`)

	deviceID := "urn:infai:ses:device:1"
	serviceID := "not-a-service-urn"
	_, err := client.Query(context.Background(), "Bearer caller", []timeseries.QueryElement{{
		DeviceId:  &deviceID,
		ServiceId: &serviceID,
		Columns:   []timeseries.QueryColumn{{Name: "value.power"}},
	}}, timeseries.QueryOptions{})

	if !errors.Is(err, timeseries.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "element 0") {
		t.Errorf("error = %v, want it to name the offending element", err)
	}
	if got.method != "" {
		t.Error("an invalid element was sent to the platform anyway")
	}
}

func TestUpstreamStatusIsCarriedOnTheError(t *testing.T) {
	client, _ := serve(t, http.StatusForbidden, `access denied`)

	_, err := client.DataAvailability(context.Background(), "Bearer caller", "d1")
	var upstream *timeseries.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %v, want an UpstreamError", err)
	}
	if upstream.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403 so the API layer can forward the platform's verdict", upstream.Code)
	}
}

// A failed request records how long it took, which is the only thing that separates
// the two meanings of a gateway 502: a response refused for its size had to be
// produced first, and a broken upstream answers immediately.
func TestAFailedRequestRecordsHowLongItTook(t *testing.T) {
	client, _ := serve(t, http.StatusBadGateway, `An invalid response was received from the upstream server`)

	_, err := client.DataAvailability(context.Background(), "Bearer caller", "d1")
	var upstream *timeseries.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("error = %v, want an UpstreamError", err)
	}
	if upstream.Elapsed <= 0 {
		t.Error("elapsed is zero, so nothing downstream can tell a fast 502 from a slow one")
	}
	if !upstream.Gateway() {
		t.Error("a 502 is a gateway failure")
	}
	// A local httptest server answers in microseconds, which is the point: this one
	// never assembled a response worth refusing.
	if !upstream.Immediate() {
		t.Errorf("a 502 after %s is not reported as immediate", upstream.Elapsed)
	}
	if !strings.Contains(err.Error(), "after") {
		t.Errorf("error = %q, want the elapsed time in the text a human reads", err)
	}
}

// Immediate is a judgement about the response, so the two codes that already say
// something definite about it are exempt.
func TestImmediateIgnoresTheCodesThatSpeakForThemselves(t *testing.T) {
	cases := map[string]struct {
		err  *timeseries.UpstreamError
		want bool
	}{
		"a fast 502 never produced a large response": {
			err:  &timeseries.UpstreamError{Code: http.StatusBadGateway, Elapsed: 34 * time.Millisecond},
			want: true,
		},
		"a slow 502 is what a response too large looks like": {
			err:  &timeseries.UpstreamError{Code: http.StatusBadGateway, Elapsed: 45 * time.Second},
			want: false,
		},
		"a 413 said outright that the entity was too large": {
			err:  &timeseries.UpstreamError{Code: http.StatusRequestEntityTooLarge, Elapsed: time.Millisecond},
			want: false,
		},
		"a gateway timeout is by definition not immediate": {
			err:  &timeseries.UpstreamError{Code: http.StatusGatewayTimeout, Elapsed: time.Millisecond},
			want: false,
		},
		"an untimed error carries no evidence either way": {
			err:  &timeseries.UpstreamError{Code: http.StatusBadGateway},
			want: false,
		},
	}
	for name, test := range cases {
		if got := test.err.Immediate(); got != test.want {
			t.Errorf("%s: Immediate() = %v, want %v", name, got, test.want)
		}
	}
}

// Values arrive as untyped JSON. Decoding a large integer counter as float64
// loses its low bits, and small deltas on a big counter then vanish — which a
// frozen-sensor flag would report as a finding.
func TestLargeIntegersSurviveDecodingExactly(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1: the first integer float64 cannot hold
	client, _ := serve(t, http.StatusOK,
		`[{"requestIndex":0,"columnNames":["value.total"],"data":[[["2026-01-01T00:00:00.000Z",`+big+`]]]}]`)

	deviceID := "urn:infai:ses:device:1"
	serviceID := "urn:infai:ses:service:11111111-1111-1111-1111-111111111111"
	elements := []timeseries.QueryElement{{
		DeviceId:  &deviceID,
		ServiceId: &serviceID,
		Columns:   []timeseries.QueryColumn{{Name: "value.total"}},
	}}
	results, err := client.Query(context.Background(), "Bearer caller", elements, timeseries.QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	sets, err := timeseries.DecodeResults(elements, results, "")
	if err != nil {
		t.Fatalf("DecodeResults: %v", err)
	}
	column, ok := sets[0].Column("value.total")
	if !ok || column.Len() != 1 {
		t.Fatalf("column = %+v, want one value", column)
	}
	number, ok := column.Values[0].(json.Number)
	if !ok {
		t.Fatalf("value is %T, want json.Number so the literal is preserved", column.Values[0])
	}
	if number.String() != big {
		t.Errorf("value = %s, want %s", number.String(), big)
	}
}
