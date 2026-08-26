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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// The embed probe (D6, §5.12).

// framedBy starts a server that answers with the given headers, and reports how
// many times it was asked.
func framedBy(t *testing.T, headers map[string]string) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			for key, value := range headers {
				writer.Header().Add(key, value)
			}
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("<html><body>a dashboard</body></html>"))
		}))
	t.Cleanup(server.Close)
	return server.URL, &hits
}

func probeFor(t *testing.T, report experiments.EmbedReport, service string) experiments.EmbedProbe {
	t.Helper()
	for _, probe := range report.Services {
		if probe.Service == service {
			return probe
		}
	}
	t.Fatalf("no probe for %q; the report holds %+v", service, report.Services)
	return experiments.EmbedProbe{}
}

func TestTheEmbedProbeReadsTheTwoHeadersThatDecideFraming(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
		reason  string
	}{
		{
			name:    "X-Frame-Options DENY",
			headers: map[string]string{"X-Frame-Options": "DENY"},
			want:    experiments.EmbedNo,
			reason:  "DENY",
		},
		{
			name:    "X-Frame-Options SAMEORIGIN",
			headers: map[string]string{"X-Frame-Options": "SAMEORIGIN"},
			want:    experiments.EmbedNo,
			reason:  "SAMEORIGIN",
		},
		{
			name:    "a restrictive frame-ancestors",
			headers: map[string]string{"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'"},
			want:    experiments.EmbedNo,
			reason:  "frame-ancestors 'none'",
		},
		{
			name: "an allow-list frame-ancestors",
			headers: map[string]string{
				"Content-Security-Policy": "frame-ancestors https://ode.example.org",
			},
			// Restricted rather than refused: whether the SPA's origin is on the list is
			// a question about the deployment, and ODE does not reliably know its own
			// public origin.
			want:   experiments.EmbedUnknown,
			reason: "allow-list",
		},
		{
			name:    "a permissive frame-ancestors",
			headers: map[string]string{"Content-Security-Policy": "frame-ancestors *"},
			want:    experiments.EmbedYes,
			reason:  "permitted",
		},
		{
			name:    "no framing headers at all",
			headers: nil,
			want:    experiments.EmbedYes,
			reason:  "neither",
		},
		{
			// CSP wins over X-Frame-Options in every browser that implements both, so a
			// permissive XFO beside a restrictive frame-ancestors is still a refusal.
			name: "a permissive header beside a restrictive policy",
			headers: map[string]string{
				"Content-Security-Policy": "frame-ancestors 'none'",
			},
			want:   experiments.EmbedNo,
			reason: "frame-ancestors",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, _ := framedBy(t, tc.headers)
			h := newHarness(t, func(deps *experiments.Deps) {
				deps.RayDashboardURL = target
				deps.MLflowUIURL = target
			})

			probe := probeFor(t, h.service.EmbedProbes(context.Background(), true), "ray")
			if probe.Embeddable != tc.want {
				t.Errorf("embeddable = %q, want %q (reason: %s)",
					probe.Embeddable, tc.want, probe.Reason)
			}
			if !strings.Contains(probe.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to mention %q — a verdict without the "+
					"header is not actionable by whoever has to change it",
					probe.Reason, tc.reason)
			}
			if probe.Status != http.StatusOK {
				t.Errorf("status = %d, want the probe's own", probe.Status)
			}
		})
	}
}

// An unreachable service is "unknown", not "no". ODE is inside the cluster and
// the developer's browser is not, so the two are different questions — and the
// pane's answer is to try the iframe and fall back if it does not load.
func TestAnUnreachableServiceProbesAsUnknown(t *testing.T) {
	h := newHarness(t, func(deps *experiments.Deps) {
		// A port nothing is listening on, and a probe timeout short enough that the
		// test does not wait for a network stack to give up on its own.
		deps.RayDashboardURL = "http://127.0.0.1:1"
		deps.MLflowUIURL = "http://127.0.0.1:1"
		deps.EmbedProbeTimeout = 500 * time.Millisecond
	})

	report := h.service.EmbedProbes(context.Background(), true)
	probe := probeFor(t, report, "mlflow")

	if probe.Embeddable != experiments.EmbedUnknown {
		t.Errorf("embeddable = %q, want unknown: ODE failing to reach a service says "+
			"nothing about whether a browser can", probe.Embeddable)
	}
	if probe.Status != 0 {
		t.Errorf("status = %d, want zero for a service that answered nothing", probe.Status)
	}
	if !strings.Contains(probe.Reason, "fall back to a link") {
		t.Errorf("reason = %q, want it to say what the pane should do", probe.Reason)
	}
}

// D6 says cache and re-probe on configuration change. The configuration is the
// cache key, which is the same rule with one fewer moving part.
func TestTheProbeIsCachedAndReProbedWhenTheConfigurationChanges(t *testing.T) {
	target, hits := framedBy(t, map[string]string{"X-Frame-Options": "DENY"})
	h := newHarness(t, func(deps *experiments.Deps) {
		deps.RayDashboardURL = target
		deps.MLflowUIURL = target
		deps.EmbedProbeTTL = time.Hour
	})

	first := h.service.EmbedProbes(context.Background(), false)
	if first.Cached {
		t.Error("the first probe was reported as cached")
	}
	probed := hits.Load()
	if probed != 2 {
		t.Fatalf("requests = %d, want one per configured service", probed)
	}

	second := h.service.EmbedProbes(context.Background(), false)
	if !second.Cached {
		t.Error("the second probe was not served from the cache")
	}
	if hits.Load() != probed {
		t.Errorf("requests = %d, want the cache to have answered", hits.Load())
	}

	// An explicit refresh goes back to the services, which is what the SPA's
	// "re-probe" control does.
	refreshed := h.service.EmbedProbes(context.Background(), true)
	if refreshed.Cached {
		t.Error("a forced refresh was answered from the cache")
	}
	if hits.Load() <= probed {
		t.Error("a forced refresh did not reach the services")
	}
}

// A deployment that names no separate UI URL is probed at its API base, because
// for both Ray's dashboard and MLflow's server that is the same origin.
func TestTheApiBaseIsProbedWhenNoSeparateUiUrlIsConfigured(t *testing.T) {
	h := newHarness(t)

	report := h.service.EmbedProbes(context.Background(), true)
	if len(report.Services) != 2 {
		t.Fatalf("probes = %+v, want one per service", report.Services)
	}
	if probe := probeFor(t, report, "ray"); probe.URL != h.ray.URL() {
		t.Errorf("ray probe URL = %q, want the API base %q", probe.URL, h.ray.URL())
	}
	if probe := probeFor(t, report, "mlflow"); probe.URL != h.mlflow.URL() {
		t.Errorf("mlflow probe URL = %q, want the API base %q", probe.URL, h.mlflow.URL())
	}
}
