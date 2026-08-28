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

package profiler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// The export half of the profiler, and the one thing about it that is not
// symmetrical with a device.
//
// An export is a table in the same timescale, addressed by export id, so the two
// reads a profile is made of are the same reads. What is missing is everything
// *around* them:
//
//   - There is no /data-availability for an export. That endpoint is device-scoped,
//     so the window a device profile is bounded by has to be established another
//     way.
//   - There is no device type, so nothing declares what a column means. Semantics
//     reach an export column only where the export is an import export and the
//     import type still describes the path it was fed from.
//   - There is no connection state, so a trailing gap cannot be attributed to a
//     device being offline. It stays a gap of unknown cause, which is the honest
//     answer rather than a missing one.
//
// The counting probe below answers the first of those, and answers the question
// that made this worth building at all: **is anything actually in there.** An
// export is created in analytics-serving and its table exists whether or not a
// row ever arrived, and the export worker's most common misconfiguration —
// value paths that resolve against the message root rather than the payload —
// produces rows in which every column is null. Both look exactly like a healthy
// export from the definition alone, so neither the export listing nor
// /usage/exports can refute them. One count per column can.

// ErrNoExportSource is what an export-addressed call answers with where no
// analytics-serving is configured. The export id alone is not enough to read one:
// a query needs the column names, and they exist only in the export definition.
var ErrNoExportSource = errors.New("profiler: no export source is configured, so an export's columns cannot be resolved")

// ExportSource resolves an export id to the columns of its timescale table.
//
// Declared here rather than taking analytics-serving's own shape for the reason
// TimeseriesClient is declared here: the profiler needs four fields per column
// and a test supplies them without a platform. The implementation lives beside
// the export listing it reads from, in pkg/imports.
type ExportSource interface {
	ExportDefinition(ctx context.Context, token string, exportID string) (ExportDefinition, error)
}

// ExportDefinition is one export as the profiler needs it.
type ExportDefinition struct {
	ExportID string `json:"export_id"`
	Name     string `json:"name,omitempty"`
	// Source and SourceID are analytics-serving's own filter, reported rather
	// than acted on: "import_id" with an instance id is an import export, and it
	// is the only kind whose columns can carry semantics. Anything else is read
	// exactly the same way, with declared types and no ontology.
	Source   string         `json:"source,omitempty"`
	SourceID string         `json:"source_id,omitempty"`
	Columns  []ExportColumn `json:"columns"`
	// Notes carry what could not be established — an import type that could not be
	// read, semantics that are therefore absent — so a profile with empty units is
	// explicable without reading the source.
	Notes []string `json:"notes,omitempty"`
}

// ExportColumn is one column of an export's timescale table.
//
// Column is what a query names and VariablePath is where the value came from in
// the message. They are not derivable from one another: the column name is
// whoever created the export's choice. Both are carried because a developer
// looking for a variable knows the path, and the query needs the column.
type ExportColumn struct {
	Column string `json:"column"`
	// Type is the export worker's own vocabulary — float, int, bool, string —
	// which is what an export definition declares. It is mapped onto the
	// platform's content variable types here, because that is what the detectors
	// switch on.
	Type             string  `json:"type,omitempty"`
	VariablePath     string  `json:"variable_path,omitempty"`
	CharacteristicID *string `json:"characteristic_id,omitempty"`
	FunctionID       string  `json:"function_id,omitempty"`
	AspectID         string  `json:"aspect_id,omitempty"`
	// Tag marks a column the export worker writes as an indexed label rather than
	// a measurement. It is still a series and is still profiled — a tag that
	// changes is a state series — but it is reported so that a reader knows why a
	// distribution over it says little.
	Tag bool `json:"tag,omitempty"`
}

// exportTypes maps the export worker's column types onto the platform's. The
// four are the only ones exportTypeOf in pkg/imports produces, which is the same
// set analytics-serving's timescale type map has entries for.
var exportTypes = map[string]models.Type{
	"float":  models.Float,
	"int":    models.Integer,
	"bool":   models.Boolean,
	"string": models.String,
}

const reasonUnknownExportType = "the export declares no column type this profiler can read as a series"

// ExportVariables turns an export's columns into the profiler's variables.
//
// Two differences from ServiceVariables are deliberate. The path is the column
// name, because that is what timescale-wrapper takes as columns[].name for an
// export — the variable path would name a column that does not exist. And the
// interaction is EVENT: an export is fed by a Kafka topic, so a series exists by
// construction, where a device service with interaction "request" is polled and
// has none.
func ExportVariables(columns []ExportColumn) []Variable {
	out := make([]Variable, 0, len(columns))
	for _, column := range columns {
		name := strings.TrimSpace(column.Column)
		variable := Variable{
			// No service id: SeriesRef carries the export id instead, and a
			// fabricated one here would make an export look like a device.
			Interaction: models.EVENT,
			Path:        name,
			Name:        name,
			FunctionID:  column.FunctionID,
			AspectID:    column.AspectID,
			Queryable:   true,
		}
		if column.CharacteristicID != nil {
			variable.CharacteristicID = *column.CharacteristicID
		}
		declared, known := exportTypes[strings.ToLower(strings.TrimSpace(column.Type))]
		switch {
		case name == "":
			variable.Queryable = false
			variable.Reason = "the export declares a column with no name"
		case !columnName.MatchString(name):
			variable.Queryable = false
			variable.Reason = reasonBadPath
		case !known:
			// Reported rather than dropped, for the reason an unknown content
			// variable type is: the column exists in the table and a developer
			// looking for it needs to know it was seen.
			variable.Queryable = false
			variable.Reason = reasonUnknownExportType
		default:
			variable.Type = declared
		}
		out = append(out, variable)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// --- the counting probe ---

// ExportFillState is what ODE can say about an export's stored rows. Four
// states, because the two that a single boolean would collapse are the two a
// developer acts on differently.
type ExportFillState string

const (
	// ExportFilled means rows exist and every readable column carries values in
	// some of them.
	ExportFilled ExportFillState = "filled"
	// ExportPartlyFilled means rows exist and at least one column is null in
	// every one of them. This is the export worker's classic misconfiguration —
	// value paths resolving against the message root, where only the timestamp is
	// found — and it is invisible from the export definition, from the export
	// listing and from stored bytes alike.
	ExportPartlyFilled ExportFillState = "partly_filled"
	// ExportEmpty means the counted window holds no row at all.
	ExportEmpty ExportFillState = "empty"
	// ExportFillUnknown means the question could not be answered. Never reported
	// as empty: "nothing is stored" sends a developer to fix their export, and
	// "I could not find out" does not.
	ExportFillUnknown ExportFillState = "unknown"
)

// exportProbeBuckets is how many buckets the counting probe aims to divide its
// window into.
//
// Low on purpose. The probe answers three questions — are there rows, which
// columns are null throughout, and over what span — and none of them needs
// resolution. The server joins one sub-query per column to produce it, so the
// cost grows with the columns rather than with the answer.
const exportProbeBuckets = 120

// DefaultExportProbeDays is how far back the probe looks when the caller names
// no window.
//
// Five years rather than the coverage lookback QuickProfile uses, because the
// question is different: a device candidate is ranked on recent data, whereas an
// export that stopped receiving rows a year ago is exactly the case that must not
// come back as empty. The bucket widens with the window, which is what keeps one
// query enough.
const DefaultExportProbeDays = 5 * 365

// ExportFillRequest asks whether an export holds anything.
type ExportFillRequest struct {
	ExportID string
	// Window bounds the count. Empty means DefaultExportProbeDays back from now,
	// and the answer says which window it used either way.
	Window   Window
	Progress func(Phase)
}

// ExportFill is the answer, and its shape is the point: every claim it makes is
// accompanied by what was read to support it.
type ExportFill struct {
	ExportID string          `json:"export_id"`
	Name     string          `json:"name,omitempty"`
	State    ExportFillState `json:"state"`
	// Reason says why, in the words a developer needs, for every state including
	// the good one. Never empty.
	Reason string `json:"reason"`
	Source string `json:"source,omitempty"`
	// Window is the range the counts were taken over.
	Window Window `json:"window"`
	Bucket string `json:"bucket"`
	// Rows is how many rows of the counted window carry a value in at least one
	// column, taken from the column that carries the most.
	//
	// It is not the row count, and the difference is the whole point of this
	// answer: an export whose every column is null has rows and reports 0 here.
	// BucketsWithRows is what says rows exist at all.
	Rows int `json:"rows"`
	// BucketsWithRows is how many buckets of the counted window contained at least
	// one row, null columns included.
	//
	// This is what distinguishes "nothing was written" from "everything written is
	// null", and it is readable off the response because the server groups by the
	// bucket: a bucket appears in the answer only where a row falls in it, and it
	// appears with a count of zero for a column that is null in every row of it.
	// Without this, an export whose value paths all miss would report as empty —
	// which is the one wrong answer here, because it sends a developer to look at
	// the topic rather than at the paths.
	BucketsWithRows int `json:"buckets_with_rows"`
	// Span is the first and last bucket that carried a row, at bucket resolution.
	// It is what stands in for an availability window: a profile over an export is
	// bounded by this, and its coarseness is the reason Bucket is reported beside
	// it.
	Span Value[Window] `json:"span"`
	// Usage is /usage/exports, which is metadata and reads nothing. Absent is an
	// ordinary answer: the usage table is filled per timescale table by a
	// collector, so a young export has no row in it.
	Usage   Value[Volume]      `json:"usage"`
	Columns []ExportColumnFill `json:"columns"`
	Reads   ReadCounts         `json:"reads"`
	Notes   []string           `json:"notes,omitempty"`
}

// ExportColumnFill is one column's share of the answer.
type ExportColumnFill struct {
	Column       string `json:"column"`
	VariablePath string `json:"variable_path,omitempty"`
	Type         string `json:"type,omitempty"`
	// Rows is how many rows of the counted window carry a value here. Zero beside
	// a non-zero ExportFill.Rows is the null-column case, and Empty says so
	// without the reader having to compare the two.
	Rows  int  `json:"rows"`
	Empty bool `json:"empty"`
	// Counted is false for a column the probe could not ask about — an unreadable
	// column name, an unknown type. Reason says which.
	Counted bool   `json:"counted"`
	Reason  string `json:"reason,omitempty"`
	Tag     bool   `json:"tag,omitempty"`
}

// ExportFill reports whether an export's timescale table holds rows, and which
// of its columns are null throughout.
//
// It reads no values: the query asks for `count` per column per bucket, so what
// comes back is how many rows carried something, never what they carried. That
// is what keeps it at exposure tier L0 beside probe_availability — which reports
// a window derived from the same rows — and it is why a session that has just
// created an export can verify it without changing tier.
func (p *Profiler) ExportFill(ctx context.Context, token string, req ExportFillRequest) (ExportFill, error) {
	if p.exports == nil {
		return ExportFill{}, ErrNoExportSource
	}
	exportID := strings.TrimSpace(req.ExportID)
	if exportID == "" {
		return ExportFill{}, fmt.Errorf("%w: an export id is required", ErrInvalidRequest)
	}

	report(req.Progress, PhaseExportDefinition, "reading the export's column list")
	definition, err := p.exports.ExportDefinition(ctx, token, exportID)
	if err != nil {
		return ExportFill{}, err
	}
	return p.exportFill(ctx, token, definition, req)
}

// exportFill is ExportFill with the definition already resolved.
//
// Split out because ProfileExport needs both halves and the definition is not
// free: resolving one is a bounded scan of the export listing, since
// analytics-serving cannot filter by id. Reading it twice for one profile pass
// would double that scan for nothing.
func (p *Profiler) exportFill(
	ctx context.Context, token string, definition ExportDefinition, req ExportFillRequest,
) (ExportFill, error) {
	exportID := strings.TrimSpace(req.ExportID)
	fill := ExportFill{
		ExportID: definition.ExportID,
		Name:     definition.Name,
		Source:   definition.Source,
		Notes:    definition.Notes,
	}
	if fill.ExportID == "" {
		fill.ExportID = exportID
	}

	window := req.Window
	if !window.Valid() {
		to := p.now()
		window = Window{From: to.AddDate(0, 0, -DefaultExportProbeDays), To: to}
	}
	fill.Window = window

	// Usage first, because it is the cheaper of the two and because it is the one
	// piece of evidence that survives a counting probe the platform refuses.
	report(req.Progress, PhaseUsage, "reading stored bytes for the export, no values")
	fill.Usage = p.exportVolume(ctx, token, fill.ExportID, &fill.Reads)

	variables := ExportVariables(definition.Columns)
	countable := make([]Variable, 0, len(variables))
	for _, variable := range variables {
		if variable.Queryable {
			countable = append(countable, variable)
		}
	}

	paths := map[string]ExportColumn{}
	for _, column := range definition.Columns {
		paths[strings.TrimSpace(column.Column)] = column
	}

	if len(countable) == 0 {
		fill.State = ExportFillUnknown
		fill.Reason = "this export declares no column that can be read as a series, so whether it " +
			"holds rows cannot be established by counting them"
		fill.Columns = describeColumnFills(variables, paths, nil)
		return fill, nil
	}

	bucket := exportProbeBucket(window)
	fill.Bucket = bucket

	report(req.Progress, PhaseExportCount, fmt.Sprintf(
		"counting rows per column at %s over %s, no values read", bucket, window))
	counted, err := p.countRows(ctx, token, fill.ExportID, countable, window, bucket, &fill.Reads)
	if err != nil {
		slog.WarnContext(ctx, "the row count for an export failed; its fill state stays unknown",
			"export_id", fill.ExportID, "columns", len(countable), "error", err)
		fill.State = ExportFillUnknown
		fill.Reason = "the row count could not be read, so whether this export holds anything is " +
			"unknown: " + err.Error()
		fill.Columns = describeColumnFills(variables, paths, nil)
		fill.Span = Uncomputablef[Window](ReasonReadFailed,
			"the counting probe failed, so the export's data span is unknown: %v", err)
		return fill, nil
	}

	fill.Columns = describeColumnFills(variables, paths, counted.perColumn)
	fill.BucketsWithRows = counted.buckets
	for _, column := range fill.Columns {
		if column.Rows > fill.Rows {
			fill.Rows = column.Rows
		}
	}

	empty := []string{}
	for _, column := range fill.Columns {
		if column.Counted && column.Empty {
			empty = append(empty, column.Column)
		}
	}

	span := Computed(Window{From: counted.from, To: counted.to})
	switch {
	case counted.buckets == 0:
		fill.State = ExportEmpty
		fill.Span = Uncomputablef[Window](ReasonInsufficientCoverage,
			"no row was counted over %s, so the export has no data span", window.String())
		fill.Reason = fmt.Sprintf("no row was counted in %s over %s. The export exists and its table "+
			"exists; nothing has been written to it in that window. If the export is young, the export "+
			"worker may not have consumed anything yet; if it is not, check that its time path resolves "+
			"against the message the topic actually carries — a row lands for every message whose "+
			"timestamp is found, even when none of its values are",
			bucketPhrase(bucket), window.String())

	case fill.Rows == 0:
		// Rows arrived and not one of them carries a value anywhere. The topic and
		// the time path are therefore fine and the value paths are not — which is the
		// export worker's classic misconfiguration, and the reason this probe counts
		// per column instead of trusting stored bytes.
		fill.State = ExportPartlyFilled
		fill.Span = span
		fill.Reason = fmt.Sprintf("rows were written over %s — %d bucket(s) of %s carry at least one — "+
			"and every column is null in all of them. That is the export worker finding the timestamp "+
			"and none of the values, which is what value paths resolving against the message root "+
			"rather than its payload produce: the rows land and carry nothing",
			window.String(), counted.buckets, bucket)

	case len(empty) > 0:
		fill.State = ExportPartlyFilled
		fill.Span = span
		fill.Reason = fmt.Sprintf("%d row(s) over %s carry values, but %s null in every row. "+
			"The export is being fed; those columns are not, so their value paths name something the "+
			"message does not carry", fill.Rows, window.String(), columnPhrase(empty))

	default:
		fill.State = ExportFilled
		fill.Span = span
		fill.Reason = fmt.Sprintf("%d row(s) were written over %s, and every readable column carries "+
			"values, so this export can be profiled and an operator can be trained on it. The span is "+
			"bucket-resolution (%s), because an export has no availability endpoint to ask instead",
			fill.Rows, window.String(), bucket)
	}
	return fill, nil
}

// exportVolume reads /usage/exports and says what an absence means rather than
// letting it read as a zero.
func (p *Profiler) exportVolume(ctx context.Context, token, exportID string, reads *ReadCounts) Value[Volume] {
	usage, err := p.ts.ExportUsage(ctx, token, []string{exportID})
	reads.Usage++
	if err != nil {
		return Uncomputablef[Volume](ReasonReadFailed,
			"the export usage endpoint failed, so stored bytes are unknown: %v", err)
	}
	for _, entry := range usage {
		if entry.ExportId != exportID && entry.ExportId != "" {
			continue
		}
		return Computed(Volume{
			Bytes:         entry.Bytes,
			BytesPerDay:   entry.BytesPerDay,
			EstimateBasis: "usage_exports",
			Confidence:    Uncertain,
			EstimatedIntervalS: Uncomputable[float64](ReasonOutOfScope,
				"an export's byte rate spans its whole table, and its rows are the export's columns "+
					"rather than a device message; deriving an interval from it would describe neither"),
		})
	}
	return Uncomputable[Volume](ReasonInsufficientCoverage,
		"the usage accounting has no row for this export. It is filled per timescale table by a "+
			"collector, so a young export is simply not in it yet — this is not evidence that nothing "+
			"is stored")
}

// rowCount is what one counting probe established.
type rowCount struct {
	// perColumn is how many rows carried a value in each column.
	perColumn map[string]int
	// buckets is how many buckets came back, which is how many contained at least
	// one row — null columns included.
	buckets  int
	from, to time.Time
}

// countRows issues the probe: one element, one count column per readable column,
// bucketed.
//
// The counts, the row presence and the span come from the same response, which is
// what makes this one read rather than three. The server groups by the bucket
// (`GROUP BY 1` over `time_bucket`), so a bucket appears in the answer only where
// a row falls in it — and it appears with a count of zero for a column that is
// null in every row of that bucket. Those two facts are what separate an export
// nothing was written to from one whose every column is null.
func (p *Profiler) countRows(
	ctx context.Context, token, exportID string,
	variables []Variable, window Window, bucket string, reads *ReadCounts,
) (rowCount, error) {
	columns := make([]timeseries.QueryColumn, 0, len(variables))
	for _, variable := range variables {
		count := timeseries.GroupCount
		columns = append(columns, timeseries.QueryColumn{Name: variable.Path, GroupType: &count})
	}

	element := exportSource(exportID).element()
	element.Columns = columns
	element.GroupTime = &bucket
	element.Time = &timeseries.QueryTime{
		Start: stringPtr(window.From.UTC().Format(time.RFC3339)),
		End:   stringPtr(window.To.UTC().Format(time.RFC3339)),
	}

	results, err := p.ts.Query(ctx, token, []timeseries.QueryElement{element},
		timeseries.QueryOptions{Timeout: p.opts.ReadTimeout})
	// Counted under Values because it is a POST /queries/v2, which is the read a
	// reader of ReadCounts is asking about. What it returns carries no value, and
	// the tool that publishes it says so in its own answer.
	reads.Values++
	if err != nil {
		return rowCount{}, err
	}
	sets, err := timeseries.DecodeResults([]timeseries.QueryElement{element}, results, "")
	if err != nil {
		return rowCount{}, err
	}

	out := rowCount{perColumn: map[string]int{}}
	for _, variable := range variables {
		out.perColumn[variable.Path] = 0
	}
	for _, set := range sets {
		for _, variable := range variables {
			column, found := set.Column(variable.Path)
			if !found {
				continue
			}
			_, values, _ := column.Numeric()
			for _, value := range values {
				if value > 0 {
					out.perColumn[variable.Path] += int(value)
				}
			}
		}
		if set.Rows() == 0 {
			continue
		}
		out.buckets += set.Rows()
		first, last := set.Times[0], set.Times[set.Rows()-1]
		if out.from.IsZero() || first.Before(out.from) {
			out.from = first
		}
		if out.to.IsZero() || last.After(out.to) {
			out.to = last
		}
	}
	// A bucket is named by its start, so the last one runs to its own end. Not
	// extending it would report a span an hour short of the data on an hourly
	// bucket, and a fortnight short on the default probe.
	if !out.to.IsZero() {
		if seconds := bucketSecondsOf(bucket); seconds > 0 {
			end := out.to.Add(time.Duration(seconds) * time.Second)
			if now := p.now(); end.After(now) {
				end = now
			}
			if end.After(out.to) {
				out.to = end
			}
		}
	}
	return out, nil
}

// describeColumnFills reports every column, including the ones the probe could
// not ask about. Omitting those would be the defect: a column that exists in the
// table and was never counted must not be indistinguishable from one that came
// back empty.
func describeColumnFills(variables []Variable, declared map[string]ExportColumn, counts map[string]int) []ExportColumnFill {
	out := make([]ExportColumnFill, 0, len(variables))
	for _, variable := range variables {
		column := declared[variable.Path]
		entry := ExportColumnFill{
			Column:       variable.Path,
			VariablePath: column.VariablePath,
			Type:         column.Type,
			Tag:          column.Tag,
		}
		switch {
		case !variable.Queryable:
			entry.Reason = variable.Reason
		case counts == nil:
			entry.Reason = "the counting probe did not run"
		default:
			entry.Counted = true
			entry.Rows = counts[variable.Path]
			entry.Empty = entry.Rows == 0
		}
		out = append(out, entry)
	}
	return out
}

// exportProbeBucket divides the probe window into about exportProbeBuckets
// buckets, rounded to something the server's interval grammar takes.
func exportProbeBucket(window Window) string {
	seconds := window.Duration().Seconds() / exportProbeBuckets
	if seconds < 60 {
		seconds = 60
	}
	// Whole hours above an hour, whole minutes below it: formatGroupTime rounds to
	// hours, and an unrounded figure would be formatted into a bucket that does not
	// divide the window it was derived from.
	if seconds > 3600 {
		seconds = math.Round(seconds/3600) * 3600
	} else {
		seconds = math.Round(seconds/60) * 60
	}
	return formatGroupTime(seconds)
}

func bucketPhrase(bucket string) string {
	return "any " + bucket + " bucket"
}

func columnPhrase(columns []string) string {
	if len(columns) == 1 {
		return "column " + columns[0] + " is"
	}
	shown := columns
	suffix := ""
	if len(shown) > 8 {
		suffix = fmt.Sprintf(" and %d more", len(shown)-8)
		shown = shown[:8]
	}
	return "columns " + strings.Join(shown, ", ") + suffix + " are"
}

// --- the full profile ---

// ExportProfileRequest asks for the full profile of every readable column of one
// export.
//
// The unit of work is the export, for the reason it is the service on the device
// side: the read is one batched POST /queries/v2 over every column, so profiling
// one column costs what profiling all of them does, and the cross-column checks
// come free with it.
type ExportProfileRequest struct {
	ExportID       string
	AnalysisWindow Window
	Progress       func(Phase)
	RawWindow      Window
	SessionParams  *SessionParams
	GroupTime      string
}

// ProfileExport computes a full SeriesProfile per readable column of one export.
//
// The pass is the device pass: the same two reads, the same retry, the same
// detectors, the same store. Only the prologue differs, and it differs in the one
// way it has to — the window comes from the counting probe rather than from
// /data-availability, because there is no availability endpoint for an export.
//
// A profile that comes back full of not_computed therefore has a first thing to
// check that a device profile does not: whether the export holds rows at all.
// That is why the probe's verdict is not discarded once the window is taken from
// it — an empty export is refused here, and the refusal is the probe's own reason.
func (p *Profiler) ProfileExport(ctx context.Context, token string, req ExportProfileRequest) (ProfileResult, error) {
	if p.exports == nil {
		return ProfileResult{}, ErrNoExportSource
	}
	exportID := strings.TrimSpace(req.ExportID)
	if exportID == "" {
		return ProfileResult{}, fmt.Errorf("%w: an export id is required", ErrInvalidRequest)
	}

	definition, err := p.exports.ExportDefinition(ctx, token, exportID)
	if err != nil {
		return ProfileResult{}, err
	}
	if definition.ExportID != "" {
		exportID = definition.ExportID
	}

	variables := []Variable{}
	for _, variable := range ExportVariables(definition.Columns) {
		if variable.Queryable {
			variables = append(variables, variable)
		}
	}
	if len(variables) == 0 {
		return ProfileResult{}, fmt.Errorf("%w: export %s declares no column that can be read as a series",
			ErrNoVariables, exportID)
	}

	index, err := p.ontology.Ontology(ctx, token)
	if err != nil {
		return ProfileResult{}, err
	}

	// The probe is the export's availability endpoint, and it is not optional the
	// way /data-availability is: a device profile can fall back on the developer's
	// window because the platform still knows the device holds data, whereas here
	// nothing else establishes that the table has a row in it.
	//
	// It runs off the definition already read above rather than through ExportFill,
	// which would resolve it a second time — and resolving one is a scan of the
	// export listing, not a lookup.
	report(req.Progress, PhaseAvailability, "counting rows to find the window that has data")
	fill, err := p.exportFill(ctx, token, definition, ExportFillRequest{
		ExportID: exportID,
		Window:   req.AnalysisWindow,
		Progress: req.Progress,
	})
	if err != nil {
		return ProfileResult{}, err
	}

	span, spanKnown := fill.Span.Get()
	switch fill.State {
	case ExportEmpty:
		return ProfileResult{}, fmt.Errorf("%w: %s", ErrInvalidRequest, fill.Reason)
	case ExportFillUnknown:
		// The probe could not answer, so the developer's own window is the only
		// range there is — the same judgement ProfileService makes when the
		// availability probe fails, and refusing outright would be worse than a
		// profile whose window is explicable.
		if !req.AnalysisWindow.Valid() {
			return ProfileResult{}, fmt.Errorf(
				"%w: %s — set an analysis window and the profile proceeds over it",
				ErrInvalidRequest, fill.Reason)
		}
	}
	if !spanKnown {
		span = fill.Window
	}

	rawAvailable := Computed(true)
	if fill.State == ExportFillUnknown {
		rawAvailable = Uncomputablef[bool](ReasonReadFailed,
			"an export has no availability endpoint, and the counting probe that stands in for it "+
				"could not answer: %s", fill.Reason)
	}

	result := ProfileResult{}
	result.Reads = fill.Reads
	return p.runPass(ctx, token, passInput{
		source:       exportSource(exportID),
		variables:    variables,
		index:        index,
		dataWindow:   span,
		rawAvailable: rawAvailable,
		reads:        result.Reads,
		// The requested window is intersected with the span the probe found, as it
		// is with a device's availability window. Passing it again here is what
		// makes a developer's narrower window narrow the profile.
		analysis:      req.AnalysisWindow,
		rawOverride:   req.RawWindow,
		groupTime:     req.GroupTime,
		sessionParams: req.SessionParams,
		progress:      req.Progress,
	})
}
