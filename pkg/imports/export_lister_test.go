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
	"net/http"
	"net/http/httptest"
	"testing"
)

// The adapter decodes what the shared resolution needs and sends the caller's
// authorization header untouched.
func TestExportListerDecodesTheResolutionFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instance" || r.URL.Query().Get("limit") != "1000" {
			t.Errorf("request = %s", r.URL)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"total":1,"count":1,"instances":[{"ID":"e1","Topic":"tp",` +
			`"Filter":"f","FilterType":"import_id","Database":"db","UserId":"u",` +
			`"ExportDatabase":{"ID":"d","Type":"timescaledb","EwFilterTopic":"w"},` +
			`"Values":[{"Name":"c","Path":"value.c","Type":"float","Tag":false}]}]}`))
	}))
	defer server.Close()

	client := NewServingClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	found, total, err := client.Lister().ListExports(context.Background(), "Bearer t", 1000, 0)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if total != 1 || len(found) != 1 {
		t.Fatalf("found = %+v, total = %d", found, total)
	}
	e := found[0]
	if e.ID != "e1" || e.Database != "db" || e.ExportDatabase.Type != "timescaledb" ||
		e.ExportDatabase.EwFilterTopic != "w" || len(e.Values) != 1 || e.Values[0].Path != "value.c" {
		t.Errorf("export = %+v", e)
	}
}

func TestExportListerReportsAnUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer server.Close()

	client := NewServingClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	if _, _, err := client.Lister().ListExports(context.Background(), "Bearer t", 10, 0); err == nil {
		t.Fatal("a 403 listing was returned as an empty answer")
	}
}
