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

package simulation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultTimeout = 30 * time.Second

// Options configures the client.
type Options struct {
	// Timeout bounds a single request. Generous by metadata standards: storing an
	// environment provisions a platform device per new asset, one call each, before
	// MOSES answers at all.
	Timeout time.Duration
	// HTTPClient replaces the transport entirely, for tests.
	HTTPClient *http.Client
	// MaxDatasetBytes bounds one upload. A dataset travels through ODE's memory
	// whole and through the developer's pod base64-encoded before that, so this is
	// a bound on ODE rather than a policy about file sizes.
	MaxDatasetBytes int
}

// DefaultMaxDatasetBytes is what an upload is bounded to when nothing says
// otherwise. Ten megabytes of CSV is roughly a year of minute values in three
// columns, which is more history than any operator being developed here needs.
const DefaultMaxDatasetBytes = 10 << 20

// Service is ODE's client for MOSES.
//
// Every call takes the developer's own token and nothing here holds a service
// account. That is not a convention borrowed from the rest of ODE — MOSES takes
// the environment's owner from the caller's token, so a service account would
// create simulations belonging to ODE, which nobody could find and nobody could
// delete.
type Service struct {
	baseURL         string
	http            *http.Client
	timeout         time.Duration
	maxDatasetBytes int
}

func New(baseURL string, opts Options) *Service {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	maxDataset := opts.MaxDatasetBytes
	if maxDataset <= 0 {
		maxDataset = DefaultMaxDatasetBytes
	}
	return &Service{
		baseURL:         strings.TrimRight(baseURL, "/"),
		http:            client,
		timeout:         timeout,
		maxDatasetBytes: maxDataset,
	}
}

// MaxDatasetBytes is the upload bound, so a caller can refuse a file before
// reading it out of the developer's pod rather than after.
func (s *Service) MaxDatasetBytes() int { return s.maxDatasetBytes }

// ---- environments ----

// List is every environment the developer owns, ordered by name.
func (s *Service) List(ctx context.Context, token string) ([]Environment, error) {
	raw, err := s.do(ctx, token, http.MethodGet, "/environments", nil, nil, "")
	if err != nil {
		return nil, err
	}
	found := []Environment{}
	if err := decodeInto(raw, &found, "/environments"); err != nil {
		return nil, err
	}
	return found, nil
}

// Get reads one environment: the whole document, which is also what Replace
// accepts.
//
// The strict second pass is what makes a later write safe. MOSES stores the whole
// document and PUT replaces it, so a member this build does not know would be
// deleted by the act of editing the environment. Get records it instead, and
// Replace refuses. Reading is never refused for it — an ODE that could not show a
// developer their own environment because MOSES gained a field would be worse than
// one that can show it and declines to edit it.
func (s *Service) Get(ctx context.Context, token, id string) (Environment, error) {
	if strings.TrimSpace(id) == "" {
		return Environment{}, fmt.Errorf("%w: an environment id is required", ErrInvalidRequest)
	}
	endpoint := "/environments/" + url.PathEscape(id)
	raw, err := s.do(ctx, token, http.MethodGet, endpoint, nil, nil, "")
	if err != nil {
		return Environment{}, err
	}
	var env Environment
	if err := decodeInto(raw, &env, endpoint); err != nil {
		return Environment{}, err
	}
	if field, drifted := unknownField(raw); drifted {
		env.UnknownField = field
	}
	return env, nil
}

// Create stores a new environment and returns it as MOSES stored it — with the
// ids it assigned, the devices it provisioned and the version it started at.
//
// The id is cleared rather than trusted from the caller. MOSES assigns one for a
// document that carries none (domain.AssignIds), and a client-chosen id on a POST
// is at best redundant and at worst a collision with an environment the developer
// cannot see.
func (s *Service) Create(ctx context.Context, token string, env Environment) (Environment, error) {
	env.ID = ""
	env.Version = 0
	if err := s.writable(env); err != nil {
		return Environment{}, err
	}
	body, err := json.Marshal(env.forWrite())
	if err != nil {
		return Environment{}, fmt.Errorf("simulation: encoding the environment: %w", err)
	}
	raw, err := s.do(ctx, token, http.MethodPost, "/environments", nil, body, "application/json")
	if err != nil {
		return Environment{}, err
	}
	var created Environment
	if err := decodeInto(raw, &created, "/environments"); err != nil {
		return Environment{}, err
	}
	return created, nil
}

// Replace writes the whole document back, carrying the version it was read at.
//
// A conflict comes back as *VersionConflict and is never retried here. The
// caller has to read the document again and re-apply its change to what is there
// now: MOSES refuses the write whole, so nothing was deleted and nothing is half
// applied, but the change this document describes is a change to a document that
// no longer exists.
func (s *Service) Replace(ctx context.Context, token string, env Environment) (Environment, error) {
	if strings.TrimSpace(env.ID) == "" {
		return Environment{}, fmt.Errorf("%w: an environment id is required", ErrInvalidRequest)
	}
	if err := s.writable(env); err != nil {
		return Environment{}, err
	}
	body, err := json.Marshal(env.forWrite())
	if err != nil {
		return Environment{}, fmt.Errorf("simulation: encoding the environment: %w", err)
	}
	endpoint := "/environments/" + url.PathEscape(env.ID)
	raw, err := s.do(ctx, token, http.MethodPut, endpoint, nil, body, "application/json")
	if err != nil {
		var upstream *UpstreamError
		if errors.As(err, &upstream) && upstream.Code == http.StatusConflict {
			return Environment{}, &VersionConflict{
				ID:      env.ID,
				Carried: env.Version,
				Detail:  upstream.Err.Error(),
			}
		}
		return Environment{}, err
	}
	var stored Environment
	if err := decodeInto(raw, &stored, endpoint); err != nil {
		return Environment{}, err
	}
	return stored, nil
}

// Delete removes an environment, and with it every platform device MOSES created
// for it. A device the developer picked and attached themselves is left alone,
// which is the whole point of the managed flag.
func (s *Service) Delete(ctx context.Context, token, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: an environment id is required", ErrInvalidRequest)
	}
	_, err := s.do(ctx, token, http.MethodDelete, "/environments/"+url.PathEscape(id), nil, nil, "")
	return err
}

// writable refuses a document this build cannot round-trip. See ErrUnknownField.
func (s *Service) writable(env Environment) error {
	if env.UnknownField == "" {
		return nil
	}
	return fmt.Errorf("%w: %q. Writing this document back would delete it, because MOSES stores "+
		"the whole environment on every write. Edit this environment in the MOSES UI, or update ODE",
		ErrUnknownField, env.UnknownField)
}

// ---- live state ----

// State reads what the running simulation currently holds.
//
// An environment that is stored but not running answers with Running false and no
// values, which is not an error: it is the normal state of a document that was
// just written, and of one another MOSES instance runs.
func (s *Service) State(ctx context.Context, token, id string) (EnvironmentState, error) {
	if strings.TrimSpace(id) == "" {
		return EnvironmentState{}, fmt.Errorf("%w: an environment id is required", ErrInvalidRequest)
	}
	endpoint := "/environments/" + url.PathEscape(id) + "/state"
	raw, err := s.do(ctx, token, http.MethodGet, endpoint, nil, nil, "")
	if err != nil {
		return EnvironmentState{}, err
	}
	var state EnvironmentState
	if err := decodeInto(raw, &state, endpoint); err != nil {
		return EnvironmentState{}, err
	}
	return state, nil
}

// Patch sets boundary conditions on a running environment.
//
// It writes the live state, not the definition: a value set here is gone when the
// environment restarts, which is what makes it the right tool for "what happens
// when the hall is at 30 °C" and the wrong one for "this hall runs warm".
func (s *Service) Patch(ctx context.Context, token, id string, change StateChange) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: an environment id is required", ErrInvalidRequest)
	}
	if change.Empty() {
		return fmt.Errorf("%w: the patch sets nothing", ErrInvalidRequest)
	}
	body, err := json.Marshal(change)
	if err != nil {
		return fmt.Errorf("simulation: encoding the state change: %w", err)
	}
	_, err = s.do(ctx, token, http.MethodPatch,
		"/environments/"+url.PathEscape(id)+"/state", nil, body, "application/json")
	return err
}

// ---- backfill ----

// Backfill computes an environment over a window that has already passed and
// publishes every reading with the timestamp it would have had.
//
// The window is validated here as well as in MOSES, and not out of distrust: the
// limits are the difference between a refusal that says what to change and a 400
// after a round trip, and one of them — that the window may not end in the future
// — is checked against a clock that is not MOSES's.
func (s *Service) Backfill(ctx context.Context, token, id string, from, to time.Time) (BackfillStatus, error) {
	if strings.TrimSpace(id) == "" {
		return BackfillStatus{}, fmt.Errorf("%w: an environment id is required", ErrInvalidRequest)
	}
	if err := CheckWindow(from, to, time.Now()); err != nil {
		return BackfillStatus{}, err
	}
	body, err := json.Marshal(map[string]any{"from": from.UTC(), "to": to.UTC()})
	if err != nil {
		return BackfillStatus{}, fmt.Errorf("simulation: encoding the window: %w", err)
	}
	endpoint := "/environments/" + url.PathEscape(id) + "/backfill"
	raw, err := s.do(ctx, token, http.MethodPost, endpoint, nil, body, "application/json")
	if err != nil {
		return BackfillStatus{}, err
	}
	var status BackfillStatus
	if err := decodeInto(raw, &status, endpoint); err != nil {
		return BackfillStatus{}, err
	}
	return status, nil
}

// BackfillStatusOf follows a job.
//
// A 404 here is also what a restarted MOSES answers: jobs live in memory and a
// restart forgets them, which MOSES reports rather than guessing at a state it
// cannot know. The caller's answer to that is to look at the data that is there,
// not to assume the job failed.
func (s *Service) BackfillStatusOf(ctx context.Context, token, id string) (BackfillStatus, error) {
	if strings.TrimSpace(id) == "" {
		return BackfillStatus{}, fmt.Errorf("%w: an environment id is required", ErrInvalidRequest)
	}
	endpoint := "/environments/" + url.PathEscape(id) + "/backfill"
	raw, err := s.do(ctx, token, http.MethodGet, endpoint, nil, nil, "")
	if err != nil {
		return BackfillStatus{}, err
	}
	var status BackfillStatus
	if err := decodeInto(raw, &status, endpoint); err != nil {
		return BackfillStatus{}, err
	}
	return status, nil
}

// MaxBackfillDays and MaxBackfillWindow are MOSES's own limit on one window, kept
// here so a refusal names the number rather than relaying a 400.
const MaxBackfillDays = 366

// MaxBackfillWindow is MaxBackfillDays as a duration.
const MaxBackfillWindow = MaxBackfillDays * 24 * time.Hour

// BackfillEpoch is the earliest instant MOSES will reconstruct. It is also what
// keeps a window inside the range int64 nanoseconds can express.
var BackfillEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// CheckWindow applies MOSES's window rules against a clock the caller supplies,
// so the check is testable without waiting for a calendar.
func CheckWindow(from, to time.Time, now time.Time) error {
	if from.IsZero() || to.IsZero() {
		return fmt.Errorf("%w: a backfill needs both ends of the window", ErrInvalidRequest)
	}
	if !to.After(from) {
		return fmt.Errorf("%w: the window ends at or before it starts", ErrInvalidRequest)
	}
	if from.Before(BackfillEpoch) {
		return fmt.Errorf("%w: a window may not start before %s",
			ErrInvalidRequest, BackfillEpoch.Format("2006"))
	}
	// A minute of slack, matching MOSES: the caller computes "now" on its own clock,
	// and a window that ends a few seconds into the future is a rounding rather than
	// a request to reconstruct the future.
	if to.After(now.Add(time.Minute)) {
		return fmt.Errorf("%w: a backfill reconstructs the past; the window ends in the future",
			ErrInvalidRequest)
	}
	if to.Sub(from) > MaxBackfillWindow {
		return fmt.Errorf("%w: the window spans %.0f days, and the limit is %d",
			ErrInvalidRequest, to.Sub(from).Hours()/24, MaxBackfillDays)
	}
	return nil
}

// ---- the catalogue an asset is built from ----

// DeviceTypes are the platform device types a simulated asset can be built from:
// the ones publishing through the protocol MOSES itself publishes through, which
// is what makes them simulatable at all.
func (s *Service) DeviceTypes(ctx context.Context, token string) ([]DeviceType, error) {
	raw, err := s.do(ctx, token, http.MethodGet, "/device-types", nil, nil, "")
	if err != nil {
		return nil, err
	}
	found := []DeviceType{}
	if err := decodeInto(raw, &found, "/device-types"); err != nil {
		return nil, err
	}
	return found, nil
}

// ---- datasets ----

// Datasets lists the uploaded timeseries the developer owns.
func (s *Service) Datasets(ctx context.Context, token string) ([]Dataset, error) {
	raw, err := s.do(ctx, token, http.MethodGet, "/datasets", nil, nil, "")
	if err != nil {
		return nil, err
	}
	found := []Dataset{}
	if err := decodeInto(raw, &found, "/datasets"); err != nil {
		return nil, err
	}
	return found, nil
}

// Dataset reads one upload's metadata.
func (s *Service) Dataset(ctx context.Context, token, id string) (Dataset, error) {
	if strings.TrimSpace(id) == "" {
		return Dataset{}, fmt.Errorf("%w: a dataset id is required", ErrInvalidRequest)
	}
	endpoint := "/datasets/" + url.PathEscape(id)
	raw, err := s.do(ctx, token, http.MethodGet, endpoint, nil, nil, "")
	if err != nil {
		return Dataset{}, err
	}
	var dataset Dataset
	if err := decodeInto(raw, &dataset, endpoint); err != nil {
		return Dataset{}, err
	}
	return dataset, nil
}

// UploadDataset stores a timeseries file for replay.
//
// The body is the file itself rather than a multipart form, which is MOSES's own
// shape. MOSES parses before it stores, so a file it cannot read is refused here
// with a line number instead of playing silence into a channel later — which is
// the whole reason this is worth doing before an environment references it.
//
// Timezone is not cosmetic and is not defaulted here. A CSV of local timestamps
// without an offset is read in the zone this names, and reading a German export
// as UTC shifts every value by an hour or two — a plausible-looking error in data
// a model is trained on. Empty leaves MOSES's own default, which it documents.
func (s *Service) UploadDataset(ctx context.Context, token, name, timezone string, content []byte) (Dataset, error) {
	if strings.TrimSpace(name) == "" {
		return Dataset{}, fmt.Errorf("%w: a dataset needs a name", ErrInvalidRequest)
	}
	if len(content) == 0 {
		return Dataset{}, fmt.Errorf("%w: the file is empty", ErrInvalidRequest)
	}
	if len(content) > s.maxDatasetBytes {
		return Dataset{}, fmt.Errorf("%w: the file is %d bytes and the limit is %d",
			ErrInvalidRequest, len(content), s.maxDatasetBytes)
	}
	query := url.Values{"name": []string{name}}
	if timezone != "" {
		query.Set("tz", timezone)
	}
	raw, err := s.do(ctx, token, http.MethodPost, "/datasets", query, content, "text/plain")
	if err != nil {
		return Dataset{}, err
	}
	var dataset Dataset
	if err := decodeInto(raw, &dataset, "/datasets"); err != nil {
		return Dataset{}, err
	}
	return dataset, nil
}

// DeleteDataset removes an upload. A channel still referencing it stops playing
// on its next reload and says so there, because the store cannot know the
// references.
func (s *Service) DeleteDataset(ctx context.Context, token, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: a dataset id is required", ErrInvalidRequest)
	}
	_, err := s.do(ctx, token, http.MethodDelete, "/datasets/"+url.PathEscape(id), nil, nil, "")
	return err
}

// ---- transport ----

func decodeInto(raw []byte, into any, endpoint string) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return &UpstreamError{
			Resource: endpoint,
			Code:     http.StatusOK,
			Err:      fmt.Errorf("decoding response: %w", err),
		}
	}
	return nil
}

func (s *Service) do(ctx context.Context, token, method, path string, query url.Values, body []byte, contentType string) ([]byte, error) {
	// Always bounded, the way pkg/imports and pkg/timeseries are: a caller that
	// passed a deadline-free context would otherwise wait forever on a simulator
	// that is provisioning devices.
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	endpoint := s.baseURL + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("simulation: building request for %s: %w", endpoint, err)
	}
	if query != nil {
		request.URL.RawQuery = query.Encode()
	}
	// Passed through as the header value, matching every other platform client
	// here: the token already carries the "Bearer " prefix.
	request.Header.Set("Authorization", token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	response, err := s.http.Do(request)
	if err != nil {
		return nil, &UpstreamError{Resource: path, Code: 0, Err: err}
	}
	defer response.Body.Close()

	if response.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		upstream := &UpstreamError{
			Resource: path,
			Code:     response.StatusCode,
			Err:      fmt.Errorf("%s", strings.TrimSpace(string(detail))),
		}
		// A 400 from MOSES is a validation error whose body names the offending field
		// paths, and that body is the useful part: it is what turns "the simulator said
		// no" into "zones[0].assets[1].channels[0].source.profile.hour_factors must have
		// 24 entries". Wrapping it as invalid input here keeps the paths attached.
		if response.StatusCode == http.StatusBadRequest {
			return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, upstream.Err.Error())
		}
		return nil, upstream
	}
	return io.ReadAll(response.Body)
}
