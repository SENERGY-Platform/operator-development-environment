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

package pkg

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	devicerepo "github.com/SENERGY-Platform/device-repository/lib/client"
)

// This exercises the real device-repository client rather than a fake, because
// the behaviour under test belongs to that client: its ontology methods take
// no token argument and set no Authorization header themselves, deferring
// entirely to the auth closure given at construction.
//
// Getting this wrong is not a compile error and not visible in any unit test
// with a fake — it shows up only as a 401 from the API gateway, which ODE then
// reports as an upstream failure. Hence a test against a real HTTP server.
func TestOntologyClientSendsTheTokenOnTokenlessMethods(t *testing.T) {
	var mux sync.Mutex
	seen := map[string]string{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mux.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	const token = "Bearer caller-token"
	client := devicerepo.NewClient(upstream.URL, func() (string, error) { return token, nil })

	if _, err, code := client.GetAspectNodes(); err != nil {
		t.Fatalf("GetAspectNodes: %v (code %d)", err, code)
	}
	if _, err, code := client.GetDeviceClasses(); err != nil {
		t.Fatalf("GetDeviceClasses: %v (code %d)", err, code)
	}

	mux.Lock()
	defer mux.Unlock()
	if len(seen) == 0 {
		t.Fatal("the upstream server was never called")
	}
	for path, auth := range seen {
		if auth != token {
			t.Errorf("GET %s carried Authorization %q, want %q — "+
				"the gateway rejects unauthenticated ontology reads with 401", path, auth, token)
		}
	}
}

// The counterpart, and the bug this replaced: constructed with a nil closure,
// the same call goes out with no Authorization header at all.
func TestANilAuthClosureSendsNoTokenUpstream(t *testing.T) {
	var got string
	var called bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		called = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	client := devicerepo.NewClient(upstream.URL, nil)
	if _, err, _ := client.GetAspectNodes(); err != nil {
		t.Fatalf("GetAspectNodes: %v", err)
	}

	if !called {
		t.Fatal("the upstream server was never called")
	}
	if got != "" {
		t.Errorf("Authorization = %q, want empty: this documents why the factory exists", got)
	}
}
