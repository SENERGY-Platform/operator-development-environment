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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
)

const defaultTimeout = 30 * time.Second

// SelectionClient calls device-selection's selectables query.
//
// The HTTP call is ODE's own rather than device-selection/pkg/client for one
// concrete reason: that client's GetSelectablesOptions has no field for
// import_path_trim_first_element, although the endpoint accepts the parameter.
// Without it every path arrives with the import type's root element still on the
// front, which is not the form an operator mapping takes — so the one option
// this package cannot do without is the one the shipped client cannot send.
// Its methods also take no context, so a resolution could not be cancelled when
// the caller disconnects.
//
// Adding the option upstream is the better fix and is worth proposing; this
// client should be deleted when it lands.
type SelectionClient struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

type ClientOptions struct {
	// Timeout bounds a single request. Generous enough for a selectables query
	// that fans out to import-repository and import-deploy upstream, which is
	// slower than a metadata read.
	Timeout time.Duration
	// HTTPClient replaces the transport entirely, for tests.
	HTTPClient *http.Client
}

func NewSelectionClient(baseURL string, opts ClientOptions) *SelectionClient {
	return &SelectionClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient(opts),
		timeout: timeoutOrDefault(opts.Timeout),
	}
}

// QueryImports asks for imports and nothing else.
//
// include_devices and include_groups are explicitly false rather than omitted.
// ODE reads devices through the device repository directly — device-selection's
// device answer drops the connection state, the device type name and the full
// device type, and hard-codes `shared` to false — so asking for them here would
// return a second, poorer view of data ODE already has, at the cost of the
// upstream work to produce it.
//
// The criteria are marshalled from device-repository's FilterCriteria. The
// endpoint documents devicemodel.FilterCriteria and its own Go client takes
// models.DeviceGroupFilterCriteria; all three carry the same four JSON keys
// (interaction, function_id, aspect_id, device_class_id), so the wire form is
// identical and ODE keeps one criteria type across both halves of a resolution.
func (c *SelectionClient) QueryImports(ctx context.Context, token string, criteria []drmodel.FilterCriteria) ([]dsmodel.Selectable, error) {
	if len(criteria) == 0 {
		return nil, ErrNoCriteria
	}
	query := url.Values{
		"include_imports": []string{"true"},
		"include_devices": []string{"false"},
		"include_groups":  []string{"false"},
		// The whole reason this client exists. Without it a path reads
		// `root.value.temperature`, which addresses nothing: the import type's output
		// describes the whole Kafka message, and an operator mapping is relative to it.
		"import_path_trim_first_element": []string{"true"},
	}
	body, err := json.Marshal(criteria)
	if err != nil {
		return nil, fmt.Errorf("imports: encoding criteria: %w", err)
	}
	found, err := do[[]dsmodel.Selectable](ctx, c.http, c.timeout, token,
		http.MethodPost, c.baseURL+"/v2/query/selectables", query, body)
	if err != nil {
		return nil, err
	}
	if found == nil {
		found = []dsmodel.Selectable{}
	}
	return found, nil
}

// DeployClient calls import-deploy.
//
// Its own lib/client is not used because every method there takes a parsed
// jwt.Token rather than the raw header value ODE carries, and none takes a
// context. The wire types are shared, which is where the coupling actually
// matters: a change to the Instance shape breaks this build rather than a
// response at runtime.
type DeployClient struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

func NewDeployClient(baseURL string, opts ClientOptions) *DeployClient {
	return &DeployClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient(opts),
		timeout: timeoutOrDefault(opts.Timeout),
	}
}

// ListInstances lists instances with their container status.
//
// The total comes from a second call to /total/instances. import-deploy sends no
// count header on the listing, and without a total a caller cannot tell a short
// page from an exhausted one — which is the difference between "these are the
// imports" and "these are the first hundred".
func (c *DeployClient) ListInstances(ctx context.Context, token string, opts InstanceListOptions) ([]idmodel.Instance, int64, error) {
	query := url.Values{
		"limit":  []string{strconv.FormatInt(opts.Limit, 10)},
		"offset": []string{strconv.FormatInt(opts.Offset, 10)},
	}
	if opts.SortBy != "" {
		query.Set("sort", opts.SortBy)
	}
	if opts.Search != "" {
		query.Set("search", opts.Search)
	}
	if opts.ExcludeGenerated {
		query.Set("exclude_generated", "true")
	}
	if len(opts.IDs) > 0 {
		query.Set("ids", strings.Join(opts.IDs, ","))
	}

	found, err := do[[]idmodel.Instance](ctx, c.http, c.timeout, token,
		http.MethodGet, c.baseURL+"/instances", query, nil)
	if err != nil {
		return nil, 0, err
	}

	// A listing restricted to ids is its own total: the caller named the set, so a
	// count of everything visible would answer a question nobody asked.
	if len(opts.IDs) > 0 {
		return found, int64(len(found)), nil
	}

	total, err := c.countInstances(ctx, token, opts)
	if err != nil {
		// The listing is the answer; the count is context for it. Failing the whole
		// call because the count did not arrive would turn a cosmetic gap into an
		// outage, so the page stands and the total reads as unknown.
		return found, -1, nil
	}
	return found, total, nil
}

func (c *DeployClient) countInstances(ctx context.Context, token string, opts InstanceListOptions) (int64, error) {
	query := url.Values{}
	if opts.Search != "" {
		query.Set("search", opts.Search)
	}
	if opts.ExcludeGenerated {
		query.Set("exclude_generated", "true")
	}
	// text/plain, not JSON: the endpoint answers with the bare number.
	raw, err := doRaw(ctx, c.http, c.timeout, token,
		http.MethodGet, c.baseURL+"/total/instances", query, nil)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}

func (c *DeployClient) ReadInstance(ctx context.Context, token string, id string) (idmodel.Instance, error) {
	return do[idmodel.Instance](ctx, c.http, c.timeout, token,
		http.MethodGet, c.baseURL+"/instances/"+url.PathEscape(id), nil, nil)
}

// RepositoryClient calls import-repository, for the direct lookup of one import
// type by id.
//
// Discovery does not need it: device-selection returns the full type alongside
// every instance. It exists so that get_import_type_metadata can answer for a
// type whose instances have not been resolved, and a deployment without an
// import_repo_url simply leaves that one tool unavailable.
type RepositoryClient struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

func NewRepositoryClient(baseURL string, opts ClientOptions) *RepositoryClient {
	return &RepositoryClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient(opts),
		timeout: timeoutOrDefault(opts.Timeout),
	}
}

// ReadImportType decodes into device-selection's ImportType rather than
// import-repository's own. The two agree on every field ODE reads, and using one
// type means the direct-lookup answer and the discovery answer cannot drift into
// describing the same import type two different ways.
func (c *RepositoryClient) ReadImportType(ctx context.Context, token string, id string) (dsmodel.ImportType, error) {
	return do[dsmodel.ImportType](ctx, c.http, c.timeout, token,
		http.MethodGet, c.baseURL+"/import-types/"+url.PathEscape(id), nil, nil)
}

func httpClient(opts ClientOptions) *http.Client {
	if opts.HTTPClient != nil {
		return opts.HTTPClient
	}
	return &http.Client{}
}

func timeoutOrDefault(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultTimeout
	}
	return timeout
}

func do[T any](ctx context.Context, client *http.Client, timeout time.Duration, token, method, endpoint string, query url.Values, body []byte) (T, error) {
	var result T
	raw, err := doRaw(ctx, client, timeout, token, method, endpoint, query, body)
	if err != nil {
		return result, err
	}
	if len(raw) == 0 {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&result); err != nil {
		return result, &UpstreamError{
			Resource: endpoint,
			Code:     http.StatusOK,
			Err:      fmt.Errorf("decoding response: %w", err),
		}
	}
	return result, nil
}

func doRaw(ctx context.Context, client *http.Client, timeout time.Duration, token, method, endpoint string, query url.Values, body []byte) ([]byte, error) {
	// Always bounded. A caller that passed a deadline-free context would otherwise
	// wait forever, the same reasoning pkg/timeseries applies.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("imports: building request for %s: %w", endpoint, err)
	}
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	// Passed through as the header value, matching the platform's own clients: the
	// token already carries the "Bearer " prefix.
	req.Header.Set("Authorization", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, &UpstreamError{Resource: endpoint, Code: 0, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &UpstreamError{
			Resource: endpoint,
			Code:     resp.StatusCode,
			Err:      fmt.Errorf("%s", strings.TrimSpace(string(detail))),
		}
	}
	return io.ReadAll(resp.Body)
}
