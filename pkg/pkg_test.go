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
	"strings"
	"sync"
	"testing"

	devicerepo "github.com/SENERGY-Platform/device-repository/lib/client"
	"github.com/SENERGY-Platform/device-repository/lib/model"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/configuration"
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
	// Semantic selection (M2) reads through the same closure. It is exercised here
	// because it is the one endpoint the whole feature rests on, and a 401 from the
	// gateway would look like a platform that knows nothing about the intent.
	if _, err, code := client.GetDeviceTypeSelectablesV2(
		[]model.FilterCriteria{{FunctionId: "fn-power"}}, "", false, false); err != nil {
		t.Fatalf("GetDeviceTypeSelectablesV2: %v (code %d)", err, code)
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

// --- M8 wiring (§5.12) ---

// startM8 degrades in the shape the rest of ODE degrades and refuses in the shape
// the rest of ODE refuses. Both halves are asserted because both are decisions: a
// deployment with neither URL still serves M0 to M7, and a deployment with one of
// them cannot do the thing it was configured for.
func TestStartM8DegradesWithoutARayClusterAndRefusesHalfAConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		ray       string
		mlflow    string
		wantError string
	}{
		{"neither configured", "", "", ""},
		{"ray without mlflow", "http://ray:8265", "", "mlflow_url is required"},
		{"mlflow without ray", "", "http://mlflow:5000", "ray_url is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := &configuration.ConfigStruct{RayUrl: tc.ray, MlflowUrl: tc.mlflow}
			configuration.HandleEnvironmentVars(config)

			// No kernel and no repo service: the surface cannot be built either way, and
			// the point here is which branch answers first.
			service, err := startM8(config, nil, nil, nil)

			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("err = %v, want a degraded start", err)
				}
				if service != nil {
					t.Error("a service was built with no Ray cluster configured")
				}
				return
			}
			if err == nil {
				t.Fatalf("half a configuration started anyway (service %v)", service != nil)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("err = %q, want it to name %q", err, tc.wantError)
			}
		})
	}
}

// The tools stay declared-but-unavailable rather than registered against a typed
// nil, which is the footgun rankerOrNil documents.
func TestExperimentsOrNilIsActuallyNil(t *testing.T) {
	if experimentsOrNil(nil) != nil {
		t.Error("a nil experiment service became a non-nil interface, so ifPresent " +
			"would register an executor that panics on the first call")
	}
}

// A configured Ray and MLflow without a Hub or a GitHub app is a warning and no
// surface, not a refusal: the job package is git archive of a working copy that
// lives on the developer's pod, and a Hub-less ODE is a supported deployment.
func TestStartM8NeedsAKernelAndARepositoryButDoesNotRefuseWithoutThem(t *testing.T) {
	config := &configuration.ConfigStruct{
		RayUrl:    "http://ray:8265",
		MlflowUrl: "http://mlflow:5000",
	}
	configuration.HandleEnvironmentVars(config)

	service, err := startM8(config, nil, nil, nil)
	if err != nil {
		t.Fatalf("err = %v, want a degraded start", err)
	}
	if service != nil {
		t.Error("the experiment surface was built without a pod to package a repository in")
	}
}
