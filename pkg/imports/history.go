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
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SENERGY-Platform/analytics-flow-engine/lib/exports"
)

// The read side of an import is where it stops resembling a device, and the
// difference has to reach the caller rather than being smoothed over.
//
// timescale-wrapper addresses a series by device and service, by device group, by
// location or by export — there is no import id. Nor is there a table worker for
// imports: the one that materialises device tables handles devices and device
// types only. An import's history therefore exists in timescale if and only if
// somebody created an **export** for it, and it is addressed by that export's id.
//
// So a QuickProfile cannot be built for an import from the evidence a device
// candidate is ranked on. Three states are distinguishable and all three are
// worth saying:
//
//   - HistoryExported: an export exists, so span, volume and preview are available
//     under its id
//   - HistoryLiveOnly: no export exists, so nothing of the import is stored to
//     profile or chart
//   - HistoryUnknown: the question could not be answered
//
// What an operator trains on does follow from this, but not here. A launch
// resolves the export the developer may execute (package lib/exports, the rule
// the flow engine applies too) and the run then reads the import's history from
// it; an import with no such export is read from its Kafka topic, which keeps
// only days. This lookup answers a different question -- does any export exist,
// for the developer to look at -- and applies no permission check of its own, so
// HistoryExported does not promise that a run will use that export.
//
// Collapsing the last two would be the actual defect. "Nothing stored" is an
// answer a developer can act on by creating the export. "I could not find out" is
// not the same claim, and reporting it as the first would send them to create an
// export that may already exist.
type HistoryState string

const (
	HistoryExported HistoryState = "exported"
	HistoryLiveOnly HistoryState = "live_only"
	HistoryUnknown  HistoryState = "unknown"
)

// FilterTypeImportExport is what analytics-serving calls an export fed by an
// import instance. Note the shape: lower snake case here, where the flow engine's
// filter type for the same relationship is "ImportId". The two services do not
// share the vocabulary, and using one where the other is expected fails silently
// on both sides.
const FilterTypeImportExport = "import_id"

// exportListLimit bounds the export listing a history lookup reads.
//
// analytics-serving cannot filter by FilterType or Filter — its search matches
// name, description, entity_name and service_name — so finding the export that
// belongs to an instance is a client-side scan. Narrowing the search by
// service_name would be faster, because the platform's own export dialog puts the
// import type id there, but nothing enforces that: an export created by any other
// route would be missed and the instance would be reported as having no history at
// all. A bounded full scan that admits when it hit the bound is the honest trade.
const exportListLimit = 1000

// Exports is the slice of analytics-serving this package needs.
//
// One listing answers for every instance, which is why History and Histories
// share it rather than each doing its own read: analytics-serving cannot filter
// by import, so a per-instance lookup would re-read the same thousand exports
// once per candidate.
type Exports interface {
	ListExports(ctx context.Context, token string, limit, offset int64) ([]Export, int64, error)
}

// Export is one analytics-serving instance, in the fields a history lookup uses.
//
// Declared here rather than shared with analytics-serving, unlike every other
// wire type in this package. Its model there is a gorm entity in an `internal`
// package — no JSON tags, a uuid.UUID id, an embedded database record — so there
// is no importable contract to couple to, and the field names below are the Go
// field names the API happens to marshal. That makes this the one shape in ODE
// that a rename upstream would break at runtime rather than at build time, which
// is worth knowing when a history lookup starts returning live_only for
// everything.
type Export struct {
	ID string `json:"ID"`
	// FilterType is "import_id" for an export of an import instance.
	FilterType string `json:"FilterType"`
	// Filter is the import instance id.
	Filter string `json:"Filter"`
	Name   string `json:"Name"`
	Topic  string `json:"Topic"`
	// Values are the export's columns. The column name in timescale is Name, and
	// Path is where the value came from in the message — which is why an import's
	// timescale columns cannot be derived from its import type.
	Values    []ExportValue `json:"Values"`
	CreatedAt time.Time     `json:"CreatedAt"`

	// The four below are read for one reason only: creating an export needs them,
	// and where a deployment has not configured them, an export that already exists
	// is the only evidence of what this platform's export worker accepts. See
	// ExportDefaults.
	EntityName       string `json:"EntityName"`
	ServiceName      string `json:"ServiceName"`
	TimePath         string `json:"TimePath"`
	TimestampFormat  string `json:"TimestampFormat"`
	Offset           string `json:"Offset"`
	ExportDatabaseID string `json:"ExportDatabaseID"`
}

// ExportDatabase is one destination an export can be written to.
//
// Read only to answer "which one", and only when no export_database_id is
// configured. Type is carried because a platform with two databases usually has
// an influx one and a timescale one, and the refusal that names them is more use
// than a list of opaque ids.
type ExportDatabase struct {
	ID   string `json:"ID"`
	Name string `json:"Name"`
	Type string `json:"Type"`
}

// ServingRequest is analytics-serving's own create body.
//
// Declared here for the reason Export is: the shape lives in an `internal`
// package upstream with no JSON tags, so there is nothing importable to couple
// to. Every field name below is the Go field name that service happens to
// marshal, and the JSON tags are what makes that explicit rather than incidental.
//
// FilterType, Filter, Name, EntityName, ServiceName, Topic and Offset are all
// `validate:"required"` upstream; omitting one produces a 400 whose body is a map
// of field names, which is why CreateExport fills every one of them.
type ServingRequest struct {
	FilterType       string                `json:"FilterType"`
	Filter           string                `json:"Filter"`
	Name             string                `json:"Name"`
	EntityName       string                `json:"EntityName"`
	ServiceName      string                `json:"ServiceName"`
	Description      string                `json:"Description,omitempty"`
	Topic            string                `json:"Topic"`
	TimePath         string                `json:"TimePath"`
	Offset           string                `json:"Offset"`
	Values           []ServingRequestValue `json:"Values"`
	ExportDatabaseID string                `json:"ExportDatabaseID"`
	TimestampFormat  string                `json:"TimestampFormat,omitempty"`
	TimestampUnique  bool                  `json:"TimestampUnique,omitempty"`
}

// ServingRequestValue is one column of an export.
//
// Path is message-relative, as a Selectable carries it and as the TimePath
// beside it is; Name is the column it lands in. Tag has no omitempty upstream
// and none here: false is meaningful.
type ServingRequestValue struct {
	Name string `json:"Name"`
	Type string `json:"Type"`
	Path string `json:"Path"`
	Tag  bool   `json:"Tag"`
}

type ExportValue struct {
	Name string `json:"Name"`
	Path string `json:"Path"`
	Type string `json:"Type"`
	Tag  bool   `json:"Tag"`
}

// History is what ODE can say about one import instance's stored data.
type History struct {
	State HistoryState `json:"state"`
	// ExportID is the id timescale-wrapper takes as exportId. Empty unless State is
	// HistoryExported.
	ExportID   string `json:"export_id,omitempty"`
	ExportName string `json:"export_name,omitempty"`
	// Columns maps a message-relative variable path to the timescale column it
	// landed in. Empty unless State is HistoryExported.
	//
	// It exists because the mapping is not derivable: an export's column is named
	// by whoever created the export, not by the content variable. A caller that
	// assumed the path was the column name would build a query for a column that
	// does not exist.
	Columns []HistoryColumn `json:"columns,omitempty"`
	// Reason says why, in the words a developer needs, for every state including
	// the good one. Never empty.
	Reason string `json:"reason"`
}

type HistoryColumn struct {
	// VariablePath is message-relative, as a Selectable carries it.
	VariablePath string `json:"variable_path"`
	// Column is what to put in a timescale-wrapper query column name.
	Column string `json:"column"`
	Tag    bool   `json:"tag"`
}

// History resolves what stored data exists for one import instance.
//
// A deployment with no analytics-serving configured answers HistoryUnknown rather
// than failing: the rest of the answer about that import — its paths, its
// semantics, how to wire it — stands perfectly well without it.
func (s *Service) History(ctx context.Context, token string, instanceID string) History {
	if s.exports == nil {
		return unconfiguredHistory()
	}
	if strings.TrimSpace(instanceID) == "" {
		return History{State: HistoryUnknown, Reason: "no instance id was given"}
	}

	found, total, err := s.exports.ListExports(ctx, token, exportListLimit, 0)
	if err != nil {
		return unreadableHistory(err)
	}
	return historyOf(instanceID, found, total)
}

// Histories answers for several instances from one export listing.
//
// This is the form a resolution uses. History reads the whole listing to answer
// for one instance, because analytics-serving cannot filter by import — so asking
// it per candidate would re-read the same thousand exports once per candidate,
// which is the cost of a shortlist rather than of a lookup.
func (s *Service) Histories(ctx context.Context, token string, instanceIDs []string) map[string]History {
	out := make(map[string]History, len(instanceIDs))
	if len(instanceIDs) == 0 {
		return out
	}
	if s.exports == nil {
		for _, id := range instanceIDs {
			out[id] = unconfiguredHistory()
		}
		return out
	}

	found, total, err := s.exports.ListExports(ctx, token, exportListLimit, 0)
	if err != nil {
		for _, id := range instanceIDs {
			out[id] = unreadableHistory(err)
		}
		return out
	}
	for _, id := range instanceIDs {
		out[id] = historyOf(id, found, total)
	}
	return out
}

// historyOf is the matching itself, shared so that the single and the batch form
// cannot drift into two different verdicts about the same export.
func historyOf(instanceID string, found []Export, total int64) History {
	for _, export := range found {
		if export.FilterType != FilterTypeImportExport || export.Filter != instanceID {
			continue
		}
		columns := make([]HistoryColumn, 0, len(export.Values))
		for _, value := range export.Values {
			columns = append(columns, HistoryColumn{
				// The export's Path is message-relative, the same key a Selectable
				// carries, so it is reported as stored. An export whose paths lost the
				// `value` envelope resolves against the message root and writes null
				// columns; putting the prefix back here would report it as if it did
				// not.
				VariablePath: value.Path,
				Column:       value.Name,
				Tag:          value.Tag,
			})
		}
		return History{
			State:      HistoryExported,
			ExportID:   export.ID,
			ExportName: export.Name,
			Columns:    columns,
			Reason: "an export writes this import to timescale, so stored values can be read " +
				"under export_id — note the column names are the export's, not the variable paths",
		}
	}

	// Nothing matched. Whether that means "no export" or "did not look far enough"
	// is the whole question, and the bound is the only thing that separates them.
	if total > int64(len(found)) || len(found) >= exportListLimit {
		return History{
			State: HistoryUnknown,
			Reason: "no export for this import was found in the first " + strconv.Itoa(len(found)) +
				" of " + strconv.FormatInt(total, 10) + " exports, and analytics-serving cannot filter " +
				"by import; the import may still be exported",
		}
	}
	return History{
		State: HistoryLiveOnly,
		Reason: "no export exists for this import, so nothing of it is stored in timescale: an " +
			"operator can consume its Kafka topic live, and the Python operator library's " +
			"provide_historic_data replays that topic, which keeps only days, but there is no " +
			"series to profile beforehand; a run reads the import's full history only once an " +
			"export the developer may execute exists",
	}
}

func unconfiguredHistory() History {
	return History{
		State: HistoryUnknown,
		Reason: "no analytics-serving is configured, so ODE cannot tell whether this import " +
			"is exported to timescale; live values on the Kafka topic are unaffected",
	}
}

func unreadableHistory(err error) History {
	return History{
		State:  HistoryUnknown,
		Reason: "the export listing could not be read, so whether this import has stored data is unknown: " + err.Error(),
	}
}

// ServingClient calls analytics-serving.
//
// Its own client package is not used: it re-exports types from an `internal`
// package, so importing it would pull the service's gorm model and its whole
// dependency graph in order to read four fields.
type ServingClient struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

func NewServingClient(baseURL string, opts ClientOptions) *ServingClient {
	return &ServingClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient(opts),
		timeout: timeoutOrDefault(opts.Timeout),
	}
}

type exportListResponse struct {
	Total     int64    `json:"total"`
	Count     int      `json:"count"`
	Instances []Export `json:"instances"`
}

func (c *ServingClient) ListExports(ctx context.Context, token string, limit, offset int64) ([]Export, int64, error) {
	query := url.Values{
		"limit":  []string{strconv.FormatInt(limit, 10)},
		"offset": []string{strconv.FormatInt(offset, 10)},
	}
	response, err := do[exportListResponse](ctx, c.http, c.timeout, token,
		http.MethodGet, c.baseURL+"/instance", query, nil)
	if err != nil {
		return nil, 0, err
	}
	if response.Instances == nil {
		response.Instances = []Export{}
	}
	return response.Instances, response.Total, nil
}

// ExportLister adapts a ServingClient to exports.Lister, the interface the shared
// resolution rule reads the listing through.
//
// An adapter rather than one wire type, because the two types differ on purpose.
// This package's Export carries the four creation defaults (EntityName,
// ServiceName, TimestampFormat, Offset) and a flatter database id, and
// exports.Export carries the database record and owner that resolution needs
// (ExportDatabase, Database, UserId) and this package does not read. Merging them
// would put each side's fields into the other's callers; both decode the same
// GET /instance, so the adapter is one more read of it.
type ExportLister struct{ client *ServingClient }

// Lister returns the client as an exports.Lister.
func (c *ServingClient) Lister() *ExportLister { return &ExportLister{client: c} }

type resolvableListResponse struct {
	Total     int64            `json:"total"`
	Instances []exports.Export `json:"instances"`
}

// ListExports is ServingClient.ListExports with the fields resolution needs.
func (l *ExportLister) ListExports(ctx context.Context, token string, limit, offset int64) ([]exports.Export, int64, error) {
	c := l.client
	query := url.Values{
		"limit":  []string{strconv.FormatInt(limit, 10)},
		"offset": []string{strconv.FormatInt(offset, 10)},
	}
	response, err := do[resolvableListResponse](ctx, c.http, c.timeout, token,
		http.MethodGet, c.baseURL+"/instance", query, nil)
	if err != nil {
		return nil, 0, err
	}
	return response.Instances, response.Total, nil
}

// CreateExport creates one export. The answer is the stored export, including
// the id timescale-wrapper takes as exportId.
//
// A 201 is not proof of anything on its own here: analytics-serving returns an
// empty instance and no error when the caller may not access the source, and its
// handler encodes that with a 201 all the same. The caller checks the id — see
// Service.CreateExport.
func (c *ServingClient) CreateExport(ctx context.Context, token string, request ServingRequest) (Export, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Export{}, fmt.Errorf("imports: encoding export request: %w", err)
	}
	return do[Export](ctx, c.http, c.timeout, token,
		http.MethodPost, c.baseURL+"/instance", nil, body)
}

// DeleteExport removes one export, and its timescale table with it.
func (c *ServingClient) DeleteExport(ctx context.Context, token string, id string) error {
	_, err := doRaw(ctx, c.http, c.timeout, token,
		http.MethodDelete, c.baseURL+"/instance/"+url.PathEscape(id), nil, nil)
	return err
}

// ListDatabases lists the export databases this account may write to.
//
// Unlike the export listing, the answer is a bare array rather than a paged
// envelope — a different shape from the same service, and the reason this is not
// folded into ListExports.
func (c *ServingClient) ListDatabases(ctx context.Context, token string) ([]ExportDatabase, error) {
	found, err := do[[]ExportDatabase](ctx, c.http, c.timeout, token,
		http.MethodGet, c.baseURL+"/databases", nil, nil)
	if err != nil {
		return nil, err
	}
	if found == nil {
		found = []ExportDatabase{}
	}
	return found, nil
}
