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

// Package timeseries reads from the platform's timescale-wrapper on behalf of
// the calling user (SPEC §5.3). Every method takes the caller's token; ODE
// holds no service account for user data (D5).
//
// The request and response types come from timescale-wrapper/pkg/model rather
// than being restated here. The query element schema is the coupling between
// the two services, so sharing the Go type means a change to it breaks the
// build instead of a query at runtime, and its Valid() method is the only
// authoritative record of which groupType, groupTime and math values the server
// accepts.
//
// The HTTP calls are ODE's own rather than timescale-wrapper/pkg/client for two
// reasons: that client implements two of the four endpoints ODE needs, and none
// of its methods take a context, so a profiler read could not be cancelled when
// the caller disconnects. Splitting the four endpoints across two clients would
// cost more than it saves. Adding context and the missing endpoints upstream
// would be the better fix and is worth proposing separately.
package timeseries

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	twmodel "github.com/SENERGY-Platform/timescale-wrapper/pkg/model"
)

// Wire types, aliased so callers need not import timescale-wrapper directly.
type (
	QueryElement = twmodel.QueriesRequestElement
	QueryColumn  = twmodel.QueriesRequestElementColumn
	QueryTime    = twmodel.QueriesRequestElementTime
	QueryFilter  = twmodel.QueriesRequestElementFilter
	QueryResult  = twmodel.QueriesV2ResponseElement
	Availability = twmodel.DataAvailabilityResponseElement
	Usage        = twmodel.Usage
	Direction    = twmodel.Direction
)

// Ordering values, for the raw pass: it takes the newest points when the point
// limit bites, which means ordering descending and reversing on decode.
const (
	OrderAscending  = twmodel.Asc
	OrderDescending = twmodel.Desc
)

// Group types accepted by POST /queries/v2, from the allow-list in
// timescale-wrapper's QueriesRequestElementColumn.Valid. SPEC §5.3.5 lists
// these as an open item to probe at runtime; they are enumerated in the
// server's own validation, so there is nothing to probe.
//
// Of note for the profiler: the difference-* family exists, so a cumulative
// counter (§5.4) can be differenced server-side rather than in ODE.
const (
	GroupMean     = "mean"
	GroupSum      = "sum"
	GroupCount    = "count"
	GroupMedian   = "median"
	GroupMin      = "min"
	GroupMax      = "max"
	GroupFirst    = "first"
	GroupLast     = "last"
	GroupDiffMean = "difference-mean"
	GroupDiffSum  = "difference-sum"
	GroupDiffLast = "difference-last"
	GroupTWMean   = "time-weighted-mean-linear"
)

// timeFormat is the layout ODE asks the server to render timestamps in.
// Millisecond precision is enough for 15-minute meter data and for the
// sub-minute intervals the sampling detector has to distinguish, and an
// explicit layout means the parse cannot depend on a server-side default.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

const defaultTimeout = 60 * time.Second

type Client struct {
	baseURL string
	http    *http.Client
	// timeout is the default deadline applied per request when the caller sets no
	// override. Held here rather than on http.Client.Timeout because that field is
	// an absolute cap that a longer per-call deadline cannot exceed — and the
	// profiler's value reads legitimately need longer than a metadata probe.
	timeout time.Duration
}

type Options struct {
	// Timeout bounds a single request unless the call overrides it. A profiler pass
	// over a long analysis window is the slowest thing ODE asks of the platform, so
	// this is deliberately generous rather than the usual few seconds.
	Timeout time.Duration
	// HTTPClient replaces the transport entirely, for tests.
	//
	// Its own Timeout field is left alone if set, but note that it caps every
	// request absolutely: a per-call override longer than it cannot take effect.
	HTTPClient *http.Client
}

func New(baseURL string, opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		// Deliberately no Timeout on the client: every request gets a context
		// deadline in do() instead, which is the only way a single call can be
		// allowed longer than the default. A client-level Timeout would silently
		// win over the longer deadline.
		httpClient = &http.Client{}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
		timeout: timeout,
	}
}

// DataAvailability implements probe_availability (SPEC §5.3): per service, the
// window over which data exists and which pre-aggregated variants are
// materialised. It reads no values, which is what keeps QuickProfile at
// exposure tier L0.
func (c *Client) DataAvailability(ctx context.Context, token string, deviceID string) ([]Availability, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("timeseries: data-availability: %w", ErrInvalidRequest)
	}
	query := url.Values{"device_id": []string{deviceID}}
	return get[[]Availability](ctx, c, token, "/data-availability", query)
}

// DeviceUsage returns bytes and bytes-per-day per device, the basis for cost
// estimation at tier L0 (§5.3.3).
//
// The endpoint is POST with a JSON array of device ids. SPEC §5.3 and
// timescale-wrapper's own swagger annotation both call it GET; the route
// registration is POST, and the route is what answers.
func (c *Client) DeviceUsage(ctx context.Context, token string, deviceIDs []string) ([]Usage, error) {
	if len(deviceIDs) == 0 {
		return nil, fmt.Errorf("timeseries: usage/devices: %w: no device ids", ErrInvalidRequest)
	}
	return post[[]Usage](ctx, c, token, "/usage/devices", nil, deviceIDs, 0)
}

type QueryOptions struct {
	// TimeFormat overrides the Go layout timestamps are rendered in. Empty
	// means the package default, which is what DecodeResults expects.
	TimeFormat string
	// Timeout overrides the client default for this one request.
	//
	// This exists for the profiler's value reads. A raw pass bounded at a hundred
	// thousand points is megabytes of JSON the server has to assemble, and it needs
	// materially longer than an availability probe — while a probe that hangs should
	// still fail fast. One shared timeout cannot serve both.
	Timeout time.Duration
}

// Query issues one batched POST /queries/v2. Batching is not an optimisation
// here: alignment across series is a property of the request when every element
// carries the same groupTime (§5.3.1 item 4), and the profiler's service-scoped
// batch (§5.4.1) is what makes cross-variable checks free.
//
// Elements are validated locally first. The server answers an invalid element
// with a bare 400, whereas Valid() failing here can say which element and
// therefore which series is at fault.
func (c *Client) Query(ctx context.Context, token string, elements []QueryElement, opts QueryOptions) ([]QueryResult, error) {
	if len(elements) == 0 {
		return nil, fmt.Errorf("timeseries: queries/v2: %w: no elements", ErrInvalidRequest)
	}
	for i := range elements {
		if !elements[i].Valid() {
			return nil, fmt.Errorf("timeseries: queries/v2: %w: element %d rejected by the shared schema: %s",
				ErrInvalidRequest, i, describeElement(elements[i]))
		}
	}

	layout := opts.TimeFormat
	if layout == "" {
		layout = timeFormat
	}
	query := url.Values{
		// per_query keeps one response element per request element. The table
		// format merges timestamps across series, which would destroy exactly
		// the per-series sampling irregularity the profiler reads (§5.3.2).
		"format":      []string{string(twmodel.PerQuery)},
		"time_format": []string{layout},
	}
	return post[[]QueryResult](ctx, c, token, "/queries/v2", query, elements, opts.Timeout)
}

// describeElement names an element in an error without dumping the payload.
func describeElement(e QueryElement) string {
	parts := make([]string, 0, 4)
	if e.DeviceId != nil {
		parts = append(parts, "device="+*e.DeviceId)
	}
	if e.ServiceId != nil {
		parts = append(parts, "service="+*e.ServiceId)
	}
	names := make([]string, 0, len(e.Columns))
	for _, col := range e.Columns {
		names = append(names, col.Name)
	}
	if len(names) > 0 {
		parts = append(parts, "columns=["+strings.Join(names, " ")+"]")
	}
	if e.GroupTime != nil {
		parts = append(parts, "groupTime="+*e.GroupTime)
	}
	return strings.Join(parts, " ")
}

func get[T any](ctx context.Context, c *Client, token, path string, query url.Values) (T, error) {
	return do[T](ctx, c, token, http.MethodGet, path, query, nil, 0)
}

func post[T any](ctx context.Context, c *Client, token, path string, query url.Values, payload any, timeout time.Duration) (T, error) {
	var zero T
	body, err := json.Marshal(payload)
	if err != nil {
		return zero, fmt.Errorf("timeseries: %s: encoding request: %w", path, err)
	}
	return do[T](ctx, c, token, http.MethodPost, path, query, body, timeout)
}

func do[T any](ctx context.Context, c *Client, token, method, path string, query url.Values, body []byte, timeout time.Duration) (T, error) {
	var result T

	// Always bounded. The http.Client carries no Timeout of its own, so a caller
	// that passed a deadline-free context would otherwise wait forever.
	if timeout <= 0 {
		timeout = c.timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return result, fmt.Errorf("timeseries: %s: building request: %w", path, err)
	}
	if len(query) > 0 {
		req.URL.RawQuery = query.Encode()
	}
	// The token is passed through as the header value, matching the platform's
	// own clients: it already carries the "Bearer " prefix.
	req.Header.Set("Authorization", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Timed so a failure can say how long it took. That is the only thing that
	// separates a gateway refusing a large response from one reporting an upstream
	// that fell over, and the two call for opposite responses — see
	// UpstreamError.Immediate.
	started := time.Now()

	resp, err := c.http.Do(req)
	if err != nil {
		return result, &UpstreamError{
			Resource: path, Code: 0, Err: err, Elapsed: time.Since(started),
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, &UpstreamError{
			Resource: path,
			Code:     resp.StatusCode,
			Err:      fmt.Errorf("%s", strings.TrimSpace(string(detail))),
			Elapsed:  time.Since(started),
		}
	}

	decoder := json.NewDecoder(resp.Body)
	// Values arrive as untyped JSON. UseNumber keeps them exact until the
	// profiler coerces them: a large integer counter read as float64 loses the
	// low bits, and small deltas on a big counter then vanish entirely, which
	// a frozen-sensor flag would happily report as a finding.
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return result, &UpstreamError{
			Resource: path, Code: resp.StatusCode,
			Err:     fmt.Errorf("decoding response: %w", err),
			Elapsed: time.Since(started),
		}
	}
	return result, nil
}
