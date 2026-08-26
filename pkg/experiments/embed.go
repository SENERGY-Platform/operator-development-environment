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

package experiments

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The embed probe (D6, SPEC §5.12, and the risk register's "Ray/MLflow refuse
// framing" row).
//
// D6 says the embedding is probed at runtime and falls back to a link on framing
// failure. This is the backend half: ask each service whether it permits framing,
// by reading the two headers that decide it.
//
// The division of labour is deliberate. Only a browser can find out whether a
// page *actually* renders in an iframe — a service may permit framing by header
// and still break inside one, or sit behind an SSO redirect that does not — so the
// SPA still loads a hidden iframe with a timeout and falls back to a link-only
// card. What the backend adds is the case the browser handles worst: a service
// that answers `X-Frame-Options: DENY` produces a console error and a blank frame
// with no event the SPA can catch, so without this probe the pane would wait out
// its whole timeout on every open. Asking first turns that into an immediate
// link-only card and a reason a human can act on.
//
// "unknown" is a first-class answer rather than a failure. ODE reaching a service
// and a developer's browser reaching it are different questions — the browser is
// outside the cluster and ODE is inside it — so a probe that cannot connect says
// so and lets the SPA try anyway.

// embedCache is the TTL cache of D6's "cache; re-probe on config change".
//
// The configuration is part of the key rather than something that invalidates the
// cache, which is the same thing with one fewer moving part: a changed URL simply
// misses.
type embedCache struct {
	mux       sync.Mutex
	key       string
	report    EmbedReport
	expiresAt time.Time
}

// EmbedProbes reports whether the configured Ray and MLflow UIs can be framed.
func (s *Service) EmbedProbes(ctx context.Context, refresh bool) EmbedReport {
	key := s.embedKey()
	now := time.Now().UTC()

	s.embed.mux.Lock()
	if !refresh && s.embed.key == key && now.Before(s.embed.expiresAt) {
		cached := s.embed.report
		s.embed.mux.Unlock()
		cached.Cached = true
		return cached
	}
	s.embed.mux.Unlock()

	targets := s.embedTargets()
	probes := make([]EmbedProbe, len(targets))
	// Probed concurrently: two services, and a deployment where one is unreachable
	// should not make the pane wait two timeouts to learn about the other.
	var wait sync.WaitGroup
	for index, target := range targets {
		wait.Add(1)
		go func() {
			defer wait.Done()
			probes[index] = s.probeEmbed(ctx, target.service, target.url)
		}()
	}
	wait.Wait()

	report := EmbedReport{
		Services: probes,
		TTL:      s.opts.EmbedProbeTTL.String(),
		AsOf:     now,
	}

	// A probe the caller abandoned is not a verdict, and must not become one.
	//
	// probeEmbed reports an unreachable service as "unknown", which is a real answer
	// (see EmbedProbe) — but a request cancelled because the developer closed the
	// pane produces exactly the same "unknown", from a request that never left. M8
	// wrote that into the TTL cache, so one closed tab could pin both services at
	// "unknown" for the next ten minutes, for every developer on the deployment, and
	// the only way out was the refresh parameter nobody would think to use.
	//
	// So the answer is returned and not stored. The caller still gets the honest
	// "unknown" for the request they cancelled; the next caller gets a real probe.
	if ctx.Err() != nil {
		return report
	}

	s.embed.mux.Lock()
	s.embed.key = key
	s.embed.report = report
	s.embed.expiresAt = now.Add(s.opts.EmbedProbeTTL)
	s.embed.mux.Unlock()

	return report
}

type embedTarget struct {
	service string
	url     string
}

// embedTargets is what there is to probe. A service configured without a separate
// UI URL is probed at its API base, because for both Ray's dashboard and MLflow's
// tracking server that is the same origin — and where a deployment puts the UI
// somewhere else, it says so.
func (s *Service) embedTargets() []embedTarget {
	targets := make([]embedTarget, 0, 2)
	if url := firstNonEmpty(s.opts.RayDashboardURL, s.opts.RayURL); url != "" {
		targets = append(targets, embedTarget{service: "ray", url: url})
	}
	if url := firstNonEmpty(s.opts.MLflowUIURL, s.opts.MLflowURL); url != "" {
		targets = append(targets, embedTarget{service: "mlflow", url: url})
	}
	return targets
}

func (s *Service) embedKey() string {
	parts := make([]string, 0, 2)
	for _, target := range s.embedTargets() {
		parts = append(parts, target.service+"="+target.url)
	}
	return strings.Join(parts, "|")
}

// probeEmbed asks one service.
//
// GET rather than HEAD: a service may answer HEAD from a different code path than
// the one that sets the framing headers, and some answer 405 to it outright. The
// body is discarded unread, so the cost is the headers plus whatever the server
// has already buffered.
func (s *Service) probeEmbed(ctx context.Context, service, target string) EmbedProbe {
	probe := EmbedProbe{
		Service:  service,
		URL:      target,
		ProbedAt: time.Now().UTC(),
	}

	ctx, cancel := context.WithTimeout(ctx, s.opts.EmbedProbeTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		probe.Embeddable, probe.Reason = EmbedUnknown, "the configured URL is not usable: "+err.Error()
		return probe
	}

	response, err := s.http.Do(request)
	if err != nil {
		probe.Embeddable = EmbedUnknown
		probe.Reason = "ODE could not reach the service, which does not mean a browser " +
			"cannot: try the iframe and fall back to a link if it does not load"
		return probe
	}
	defer response.Body.Close()
	// Read nothing: the answer is entirely in the headers, and a dashboard's index
	// page is not something ODE has any business pulling.

	probe.Status = response.StatusCode
	probe.Embeddable, probe.Reason = framingVerdict(response.Header)
	return probe
}

// framingVerdict reads the two headers that decide framing.
//
// X-Frame-Options is checked first because it is the blunter of the two and
// because a service that sets both usually agrees with itself. CSP's
// frame-ancestors takes precedence in the browsers that implement both, so a
// permissive X-Frame-Options beside a restrictive frame-ancestors must still come
// back as "no" — which is why the CSP check runs even when XFO said nothing.
func framingVerdict(header http.Header) (string, string) {
	if raw := strings.TrimSpace(header.Get("X-Frame-Options")); raw != "" {
		switch strings.ToUpper(strings.Fields(raw)[0]) {
		case "DENY":
			return EmbedNo, "X-Frame-Options: DENY — the service refuses to be framed at all"
		case "SAMEORIGIN":
			return EmbedNo, "X-Frame-Options: SAMEORIGIN — the service may only be framed " +
				"by a page on its own origin, which the ODE frontend is not"
		case "ALLOW-FROM":
			// Obsolete and ignored by every current browser, so it decides nothing and
			// saying "yes" on the strength of it would be wrong.
			return EmbedUnknown, "X-Frame-Options: " + raw +
				" — ALLOW-FROM is obsolete and ignored by current browsers"
		}
	}

	for _, policy := range header.Values("Content-Security-Policy") {
		directive, found := frameAncestors(policy)
		if !found {
			continue
		}
		switch {
		case directive == "" || directive == "'none'":
			return EmbedNo, "Content-Security-Policy: frame-ancestors 'none' — " +
				"the service refuses to be framed"
		case directive == "*":
			return EmbedYes, "Content-Security-Policy: frame-ancestors * — framing is permitted"
		default:
			// A concrete allow-list. ODE does not reliably know the origin the SPA is
			// served from — public_url is optional and a reverse proxy may rewrite it —
			// so the list is reported rather than judged.
			return EmbedUnknown, "Content-Security-Policy: frame-ancestors " + directive +
				" — framing is restricted to an allow-list; whether the ODE frontend's " +
				"origin is on it is a question for the deployment"
		}
	}

	return EmbedYes, "neither X-Frame-Options nor a frame-ancestors directive is set, " +
		"so nothing on the service's side refuses framing"
}

// frameAncestors extracts the directive's value from one CSP header.
//
// A CSP is semicolon-separated directives, each a name followed by
// space-separated values. A `frame-ancestors` with no values is as restrictive as
// `'none'`, which is why the empty string is a found value rather than a miss.
func frameAncestors(policy string) (string, bool) {
	for _, directive := range strings.Split(policy, ";") {
		fields := strings.Fields(directive)
		if len(fields) == 0 {
			continue
		}
		if !strings.EqualFold(fields[0], "frame-ancestors") {
			continue
		}
		return strings.Join(fields[1:], " "), true
	}
	return "", false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
