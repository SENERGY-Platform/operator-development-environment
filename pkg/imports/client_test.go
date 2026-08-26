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

package imports

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
)

// The one query parameter this client exists for. device-selection's own Go
// client cannot send it, and without it every path arrives with the import type's
// output root still on the front — which addresses one level too deep and yields
// nothing, silently.
func TestTheSelectablesQueryTrimsImportPaths(t *testing.T) {
	var query, auth string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		auth = r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/query/selectables" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewSelectionClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	if _, err := client.QueryImports(context.Background(), testToken,
		[]drmodel.FilterCriteria{{FunctionId: "fn-temperature", AspectId: "kitchen"}}); err != nil {
		t.Fatalf("QueryImports: %v", err)
	}

	values, err := parseQuery(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if values["import_path_trim_first_element"] != "true" {
		t.Errorf("import_path_trim_first_element = %q, want true: this is the whole reason ODE "+
			"does not use device-selection's own client", values["import_path_trim_first_element"])
	}
	if values["include_imports"] != "true" {
		t.Errorf("include_imports = %q, want true", values["include_imports"])
	}
	// Devices are read through the device repository, which keeps the connection
	// state and the device type this answer would drop. Asking for them here would
	// cost the upstream work to produce a poorer second view.
	if values["include_devices"] != "false" {
		t.Errorf("include_devices = %q, want false", values["include_devices"])
	}
	if values["include_groups"] != "false" {
		t.Errorf("include_groups = %q, want false", values["include_groups"])
	}
	if auth != testToken {
		t.Errorf("Authorization = %q, want the caller's token verbatim", auth)
	}

	// The criteria go on the wire with the four keys device-selection documents,
	// although ODE holds them in the device repository's type.
	var sent []map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d criteria, want one", len(sent))
	}
	for _, key := range []string{"function_id", "aspect_id", "device_class_id", "interaction"} {
		if _, present := sent[0][key]; !present {
			t.Errorf("criterion has no %q key: %v", key, sent[0])
		}
	}
}

func TestTheSelectablesQueryRefusesAnEmptyCriteriaList(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()

	client := NewSelectionClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	if _, err := client.QueryImports(context.Background(), testToken, nil); !errors.Is(err, ErrNoCriteria) {
		t.Errorf("err = %v, want ErrNoCriteria", err)
	}
	if called {
		t.Error("the request went out anyway, which upstream reads as a match on everything")
	}
}

func TestAnUpstreamRefusalCarriesTheCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("not your import"))
	}))
	defer server.Close()

	client := NewDeployClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	_, err := client.ReadInstance(context.Background(), testToken, testInstanceID)

	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("err = %v, want an UpstreamError so the API layer can forward the verdict", err)
	}
	if !upstream.Forbidden() {
		t.Errorf("code %d not reported as forbidden; a permission refusal is the authorisation "+
			"model working, not an outage", upstream.Code)
	}
}

func TestAnInstanceListingCarriesTheTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/instances":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"` + testInstanceID + `","kafka_topic":"` + testTopic + `"}]`))
		case "/total/instances":
			_, _ = w.Write([]byte("42\n"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewDeployClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	found, total, err := client.ListInstances(context.Background(), testToken, InstanceListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(found) != 1 || found[0].KafkaTopic != testTopic {
		t.Fatalf("instances = %+v", found)
	}
	if total != 42 {
		t.Errorf("total = %d, want 42: without it a caller cannot tell a short page from an "+
			"exhausted one", total)
	}
}

// A listing restricted to ids is its own total, and asking for a count of
// everything visible would answer a question nobody put.
func TestAnIdRestrictedListingSkipsTheCount(t *testing.T) {
	counted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/total/instances" {
			counted = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testInstanceID + `"}]`))
	}))
	defer server.Close()

	client := NewDeployClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	_, total, err := client.ListInstances(context.Background(), testToken,
		InstanceListOptions{Limit: 10, IDs: []string{testInstanceID}})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if counted {
		t.Error("a second request was made to count a set the caller named")
	}
	if total != 1 {
		t.Errorf("total = %d, want the size of the named set", total)
	}
}

// The count is context for the listing, not the answer. Losing it must not turn a
// cosmetic gap into an outage.
func TestAFailedCountLeavesTheListingStanding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/total/instances" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"` + testInstanceID + `"}]`))
	}))
	defer server.Close()

	client := NewDeployClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	found, total, err := client.ListInstances(context.Background(), testToken, InstanceListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("instances = %+v, want the page to survive", found)
	}
	if total != -1 {
		t.Errorf("total = %d, want -1 for an unknown count rather than a plausible zero", total)
	}
}

func TestTheExportListingUnwrapsTheEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instance" {
			t.Errorf("path = %s, want /instance", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":7,"count":1,"instances":[
			{"ID":"export-1","FilterType":"import_id","Filter":"` + testInstanceID + `",
			 "Values":[{"Name":"temp_c","Path":"temperature_2m","Type":"float","Tag":false}]}]}`))
	}))
	defer server.Close()

	client := NewServingClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	found, total, err := client.ListExports(context.Background(), testToken, 100, 0)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if total != 7 {
		t.Errorf("total = %d, want 7", total)
	}
	if len(found) != 1 || found[0].ID != "export-1" {
		t.Fatalf("exports = %+v", found)
	}
	// Capitalised JSON keys, because the upstream model is a gorm entity with no
	// tags. Getting this wrong is silent: every history lookup would answer
	// live_only.
	if found[0].FilterType != FilterTypeImportExport || found[0].Filter != testInstanceID {
		t.Errorf("filter = %q/%q, want the import filter to decode", found[0].FilterType, found[0].Filter)
	}
	if len(found[0].Values) != 1 || found[0].Values[0].Name != "temp_c" {
		t.Errorf("values = %+v, want the column name to decode", found[0].Values)
	}
}

func parseQuery(raw string) (map[string]string, error) {
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for key, list := range parsed {
		if len(list) > 0 {
			values[key] = list[0]
		}
	}
	return values, nil
}
