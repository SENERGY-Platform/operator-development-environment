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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

// --- fakes for the write half ---

type fakeDeployer struct {
	created  idmodel.Instance
	err      error
	sent     []idmodel.Instance
	deleted  []string
	deleteEr error
}

func (f *fakeDeployer) CreateInstance(_ context.Context, _ string, instance idmodel.Instance) (idmodel.Instance, error) {
	f.sent = append(f.sent, instance)
	if f.err != nil {
		return idmodel.Instance{}, f.err
	}
	created := f.created
	if created.Id == "" && f.err == nil && created.Name == "" {
		// The ordinary case: the fake was not told what to return, so it answers the
		// way import-deploy does — the request back, with the ids it minted.
		created = instance
		created.Id = testInstanceID
		created.KafkaTopic = testTopic
	}
	return created, nil
}

func (f *fakeDeployer) DeleteInstance(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteEr
}

type fakeExportWriter struct {
	created   Export
	createErr error
	sent      []ServingRequest
	deleted   []string
	deleteErr error
	databases []ExportDatabase
	dbErr     error
}

func (f *fakeExportWriter) CreateExport(_ context.Context, _ string, request ServingRequest) (Export, error) {
	f.sent = append(f.sent, request)
	if f.createErr != nil {
		return Export{}, f.createErr
	}
	created := f.created
	if created.ID == "" && created.Name == "" {
		created = Export{
			ID: "export-1", Name: request.Name, FilterType: request.FilterType,
			Filter: request.Filter, Topic: request.Topic,
		}
		for _, value := range request.Values {
			created.Values = append(created.Values, ExportValue{
				Name: value.Name, Path: value.Path, Type: value.Type, Tag: value.Tag,
			})
		}
	}
	return created, nil
}

func (f *fakeExportWriter) DeleteExport(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func (f *fakeExportWriter) ListDatabases(context.Context, string) ([]ExportDatabase, error) {
	return f.databases, f.dbErr
}

type fakeTypes struct {
	importType dsmodel.ImportType
	err        error

	// The catalogue half: what a listing answers, and the total upstream reported
	// for it. A zero total with a non-empty page is a legitimate fake for a
	// missing X-Total-Count.
	listed []dsmodel.ImportType
	total  int64
}

func (f *fakeTypes) ReadImportType(context.Context, string, string) (dsmodel.ImportType, error) {
	return f.importType, f.err
}

func (f *fakeTypes) ListImportTypes(_ context.Context, _ string, _ TypeListOptions) ([]dsmodel.ImportType, int64, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.listed, f.total, nil
}

// weatherType is an import type with one config of each interesting shape: one
// ordinary, one whose name reads as a credential and has no default, and the
// output tree every import type has — an envelope around a payload.
func weatherType() dsmodel.ImportType {
	return dsmodel.ImportType{
		Id:    testTypeID,
		Name:  "Open-Meteo history",
		Image: "ghcr.io/example/open-meteo:1",
		Configs: []dsmodel.ImportTypeConfig{
			{Name: "lat", Type: models.Float, DefaultValue: 51.34},
			{Name: "station", Type: models.String, DefaultValue: "leipzig"},
			{Name: "interval_minutes", Type: models.Integer, DefaultValue: float64(15)},
		},
		Output: dsmodel.ImportContentVariable{
			Name: "root", Type: models.Structure,
			SubContentVariables: []dsmodel.ImportContentVariable{
				{Name: "import_id", Type: models.String},
				{Name: "time", Type: models.String},
				{Name: "value", Type: models.Structure, SubContentVariables: []dsmodel.ImportContentVariable{
					{Name: "temperature_2m", Type: models.Float},
					{Name: "station_name", Type: models.String},
					{Name: "units", Type: models.Structure, SubContentVariables: []dsmodel.ImportContentVariable{
						{Name: "temperature_2m", Type: models.String},
					}},
				}},
			},
		},
	}
}

func writeService(t *testing.T, deployer *fakeDeployer, writer *fakeExportWriter, importType dsmodel.ImportType, defaults ExportDefaults) *Service {
	t.Helper()
	deps := Deps{
		Selectables:    &fakeSelectables{},
		Instances:      &fakeInstances{serve: []idmodel.Instance{runningInstance()}},
		Types:          &fakeTypes{importType: importType},
		ExportDefaults: defaults,
	}
	if deployer != nil {
		deps.Deployer = deployer
	}
	if writer != nil {
		deps.ExportWriter = writer
		deps.Exports = &fakeExports{}
	}
	service, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

func runningInstance() idmodel.Instance {
	return idmodel.Instance{
		Id: testInstanceID, Name: "Leipzig weather", ImportTypeId: testTypeID,
		KafkaTopic: testTopic, Status: &idmodel.InstanceStatus{Running: true},
	}
}

func usableDefaults() ExportDefaults {
	return ExportDefaults{
		Offset: "smallest", TimePath: "time",
		TimestampFormat: "%Y-%m-%dT%H:%M:%SZ", DatabaseID: "db-1",
	}
}

// --- creating an import instance ---

// The three fields import-deploy mints itself. Sending any of them is a 400
// ("explicit setting of id not allowed") rather than an override, and the image
// is taken from the import type.
func TestCreateInstanceSendsNoIdTopicOrImage(t *testing.T) {
	deployer := &fakeDeployer{}
	service := writeService(t, deployer, nil, weatherType(), usableDefaults())

	created, err := service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID,
		Name:         "Leipzig weather",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if len(deployer.sent) != 1 {
		t.Fatalf("sent %d requests, want one", len(deployer.sent))
	}
	sent := deployer.sent[0]
	if sent.Id != "" || sent.KafkaTopic != "" || sent.Image != "" {
		t.Errorf("sent id %q topic %q image %q, want all three left to import-deploy",
			sent.Id, sent.KafkaTopic, sent.Image)
	}
	if created.Instance.Id != testInstanceID {
		t.Errorf("instance id = %q, want the one upstream minted", created.Instance.Id)
	}
}

// A config the caller left out takes the import type's default, and the answer
// says which — a created import whose configs all came from defaults looks
// exactly like a configured one otherwise.
func TestCreateInstanceFillsAndReportsDefaults(t *testing.T) {
	deployer := &fakeDeployer{}
	service := writeService(t, deployer, nil, weatherType(), usableDefaults())

	created, err := service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID,
		Name:         "Leipzig weather",
		Configs:      []ConfigValue{{Name: "station", Value: "dresden"}},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	sent := deployer.sent[0]
	if len(sent.Configs) != 3 {
		t.Fatalf("sent %d configs, want all three the type declares", len(sent.Configs))
	}
	values := map[string]any{}
	for _, config := range sent.Configs {
		values[config.Name] = config.Value
	}
	if values["station"] != "dresden" {
		t.Errorf("station = %v, want the caller's value", values["station"])
	}
	if values["lat"] != 51.34 {
		t.Errorf("lat = %v, want the type's default", values["lat"])
	}
	if got := strings.Join(created.Defaulted, ","); got != "lat,interval_minutes" {
		t.Errorf("defaulted = %v, want the two the caller did not set", created.Defaulted)
	}
}

// import-deploy accepts a config name its import type never declared, marshals it
// into the container's environment and never reads it. A typo therefore produces
// a running import that ignores the setting it was given.
func TestCreateInstanceRefusesAConfigTheTypeDoesNotDeclare(t *testing.T) {
	service := writeService(t, &fakeDeployer{}, nil, weatherType(), usableDefaults())

	_, err := service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID, Name: "Leipzig weather",
		Configs: []ConfigValue{{Name: "latitude", Value: 51.34}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want an invalid request", err)
	}
	if !strings.Contains(err.Error(), "latitude") || !strings.Contains(err.Error(), "lat") {
		t.Errorf("error = %q, want it to name the config and what the type does declare", err)
	}
}

func TestCreateInstanceRefusesAConfigOfTheWrongType(t *testing.T) {
	service := writeService(t, &fakeDeployer{}, nil, weatherType(), usableDefaults())

	// JSON numbers arrive as float64, so an integer config is one only when it has
	// no fractional part — the same test import-deploy applies, whose own answer
	// names no field at all.
	_, err := service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID, Name: "Leipzig weather",
		Configs: []ConfigValue{{Name: "interval_minutes", Value: 12.5}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want an invalid request", err)
	}
	if !strings.Contains(err.Error(), "interval_minutes") {
		t.Errorf("error = %q, want it to name the config", err)
	}
}

// The credential rule, both halves. A value handed in from a chat is refused
// however plausible it looks, and a type that needs one at all is refused
// outright — the developer creates that import in the platform's own dialog.
func TestCreateInstanceRefusesCredentialConfigs(t *testing.T) {
	withKey := weatherType()
	withKey.Configs = append(withKey.Configs,
		dsmodel.ImportTypeConfig{Name: "api_key", Type: models.String})

	service := writeService(t, &fakeDeployer{}, nil, withKey, usableDefaults())

	_, err := service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID, Name: "Leipzig weather",
	})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("a credential config with no default: error = %v, want a refusal naming it", err)
	}

	_, err = service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID, Name: "Leipzig weather",
		Configs: []ConfigValue{{Name: "api_key", Value: "sk-whatever"}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a credential config with a value: error = %v, want a refusal", err)
	}
	if strings.Contains(err.Error(), "sk-whatever") {
		t.Error("the refusal repeats the credential it refused")
	}
}

// A credential-shaped config the type has a default for is not a credential the
// session supplied, so it deploys — and says that nothing from the session was
// sent for it.
func TestCreateInstanceAllowsACredentialConfigLeftAtItsDefault(t *testing.T) {
	withToken := weatherType()
	withToken.Configs = append(withToken.Configs,
		dsmodel.ImportTypeConfig{Name: "auth_token", Type: models.String, DefaultValue: "public"})

	service := writeService(t, &fakeDeployer{}, nil, withToken, usableDefaults())
	created, err := service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID, Name: "Leipzig weather",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if len(created.Notes) == 0 || !strings.Contains(strings.Join(created.Notes, " "), "auth_token") {
		t.Errorf("notes = %v, want the credential-shaped default reported", created.Notes)
	}
}

// Without import-repository there is no way to check a config name, and the
// failure that would let through is the silent one above.
func TestCreateInstanceRefusesWithoutTheImportType(t *testing.T) {
	service, err := New(Deps{
		Selectables: &fakeSelectables{},
		Instances:   &fakeInstances{},
		Deployer:    &fakeDeployer{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = service.CreateInstance(context.Background(), testToken, CreateInstanceRequest{
		ImportTypeID: testTypeID, Name: "x",
	})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "import-repository") {
		t.Errorf("error = %v, want a refusal naming the missing service", err)
	}
}

func TestDeleteInstancePassesTheIdThrough(t *testing.T) {
	deployer := &fakeDeployer{}
	service := writeService(t, deployer, nil, weatherType(), usableDefaults())

	if err := service.DeleteInstance(context.Background(), testToken, testInstanceID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if len(deployer.deleted) != 1 || deployer.deleted[0] != testInstanceID {
		t.Errorf("deleted %v, want the one instance", deployer.deleted)
	}
	if err := service.DeleteInstance(context.Background(), testToken, "  "); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("empty id: error = %v, want an invalid request", err)
	}
}

// --- creating an export ---

// The whole request, because every field of it is either derived from the
// instance or configured, and a wrong one produces an export that deploys and
// stores nothing.
func TestCreateExportBuildsTheRequestFromTheInstance(t *testing.T) {
	writer := &fakeExportWriter{}
	service := writeService(t, nil, writer, weatherType(), usableDefaults())

	created, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID,
		Name:       "Leipzig weather history",
		Values: []ExportValueRequest{
			{VariablePath: "value.temperature_2m"},
			{VariablePath: "value.station_name", Column: "station", Tag: true},
		},
	})
	if err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if len(writer.sent) != 1 {
		t.Fatalf("sent %d requests, want one", len(writer.sent))
	}
	sent := writer.sent[0]

	if sent.FilterType != FilterTypeImportExport {
		t.Errorf("FilterType = %q, want %q — the flow engine's ImportId is a different string "+
			"for the same relationship", sent.FilterType, FilterTypeImportExport)
	}
	if sent.Filter != testInstanceID {
		t.Errorf("Filter = %q, want the instance id", sent.Filter)
	}
	if sent.Topic != testTopic {
		t.Errorf("Topic = %q, want the topic read from the instance", sent.Topic)
	}
	if sent.EntityName != "Leipzig weather" || sent.ServiceName != testTypeID {
		t.Errorf("EntityName/ServiceName = %q/%q, want the instance name and the import type id, "+
			"which is where the platform's own export dialog puts them",
			sent.EntityName, sent.ServiceName)
	}
	if sent.Offset != "smallest" || sent.TimePath != "time" ||
		sent.TimestampFormat != "%Y-%m-%dT%H:%M:%SZ" || sent.ExportDatabaseID != "db-1" {
		t.Errorf("deployment fields = %+v, want the configured defaults", sent)
	}

	if len(sent.Values) != 2 {
		t.Fatalf("sent %d values, want two", len(sent.Values))
	}
	byColumn := map[string]ServingRequestValue{}
	for _, value := range sent.Values {
		byColumn[value.Name] = value
	}
	// Message-relative, the same root the TimePath beside it resolves against.
	// Trimming the envelope prefix would address the message root, where the
	// payload's leaves are not, and the column would stay null.
	if got := byColumn["temperature_2m"].Path; got != "value.temperature_2m" {
		t.Errorf("path = %q, want it message-relative", got)
	}
	if got := byColumn["temperature_2m"].Type; got != "float" {
		t.Errorf("type = %q, want the export worker's own vocabulary", got)
	}
	if !byColumn["station"].Tag || byColumn["station"].Path != "value.station_name" {
		t.Errorf("station = %+v, want the named column, tagged", byColumn["station"])
	}
	if created.Export.ID != "export-1" {
		t.Errorf("export id = %q, want what upstream returned", created.Export.ID)
	}
}

// An import that backfills the past writes every message within minutes, so the
// envelope time makes years of history read as those minutes. The time path is
// therefore the export's to choose, not only the deployment's.
func TestCreateExportTakesAPerExportTimePath(t *testing.T) {
	writer := &fakeExportWriter{}
	service := writeService(t, nil, writer, weatherType(), usableDefaults())

	created, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values:   []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
		TimePath: "value.station_name",
	})
	if err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if got := writer.sent[0].TimePath; got != "value.station_name" {
		t.Errorf("TimePath = %q, want the requested one rather than the configured default", got)
	}
	if created.Derived["time_path"] != "the request" {
		t.Errorf("derived = %v, want the time path attributed to the request", created.Derived)
	}
	// The pairing is the risk: the configured format was written for the field the
	// deployment names, and this export reads another.
	if !strings.Contains(strings.Join(created.Notes, " "), "stores no row") {
		t.Errorf("notes = %v, want the unpaired timestamp format called out", created.Notes)
	}
}

// The two belong together, so setting both is the case that carries no warning.
func TestCreateExportTakesAPerExportTimestampFormat(t *testing.T) {
	writer := &fakeExportWriter{}
	service := writeService(t, nil, writer, weatherType(), usableDefaults())

	created, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values:          []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
		TimePath:        "value.station_name",
		TimestampFormat: "%Y-%m-%d %H:%M",
	})
	if err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if got := writer.sent[0].TimestampFormat; got != "%Y-%m-%d %H:%M" {
		t.Errorf("TimestampFormat = %q, want the requested one", got)
	}
	if created.Derived["timestamp_format"] != "the request" {
		t.Errorf("derived = %v, want the format attributed to the request", created.Derived)
	}
	if strings.Contains(strings.Join(created.Notes, " "), "stores no row") {
		t.Errorf("notes = %v, want no warning when both fields were requested together", created.Notes)
	}
}

// The envelope's own time is the default and stays sayable, including with the
// output root still on the front — which is the form a model repeats back from
// an import type.
func TestCreateExportAcceptsTheEnvelopeTimeAsATimePath(t *testing.T) {
	writer := &fakeExportWriter{}
	service := writeService(t, nil, writer, weatherType(), usableDefaults())

	if _, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values:   []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
		TimePath: "root.time",
	}); err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if got := writer.sent[0].TimePath; got != "time" {
		t.Errorf("TimePath = %q, want the output root taken off it", got)
	}
}

// A time path upstream has no error for: the export deploys, consumes the topic
// and writes no row at all.
func TestCreateExportRefusesATimePathTheTypeHasNot(t *testing.T) {
	writer := &fakeExportWriter{}
	service := writeService(t, nil, writer, weatherType(), usableDefaults())

	for _, path := range []string{"value.weather_time", "value.units", "value"} {
		_, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
			InstanceID: testInstanceID, Name: "history",
			Values:   []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
			TimePath: path,
		})
		if err == nil {
			t.Fatalf("time path %q was accepted; the import type carries no such leaf", path)
		}
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "value.temperature_2m") {
			t.Errorf("error for %q = %q, want a refusal listing what the type does carry", path, err)
		}
	}
	if len(writer.sent) != 0 {
		t.Error("analytics-serving was asked to create an export with a time path that names nothing")
	}
}

// analytics-serving returns an empty instance and no error when the caller may
// not access the import, and its handler encodes that as a 201 — so a permission
// refusal arrives looking exactly like a success.
func TestCreateExportTreatsAnEmptyIdAsARefusal(t *testing.T) {
	writer := &fakeExportWriter{created: Export{Name: "not empty enough to be defaulted"}}
	service := writeService(t, nil, writer, weatherType(), usableDefaults())

	_, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values: []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
	})
	if err == nil {
		t.Fatal("expected an error when analytics-serving answers without an export id")
	}
	if !strings.Contains(err.Error(), "may not access") {
		t.Errorf("error = %q, want it to say what an empty answer means", err)
	}
}

func TestCreateExportRefusesPathsThatAreNotSeries(t *testing.T) {
	service := writeService(t, nil, &fakeExportWriter{}, weatherType(), usableDefaults())

	for _, path := range []string{"value.humidity", "time", "root.time", "value.units"} {
		_, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
			InstanceID: testInstanceID, Name: "history",
			Values: []ExportValueRequest{{VariablePath: path}},
		})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: error = %v, want an invalid request", path, err)
		}
	}
}

// Two variables in one column would create a table with one of them, silently.
func TestCreateExportRefusesTwoVariablesInOneColumn(t *testing.T) {
	service := writeService(t, nil, &fakeExportWriter{}, weatherType(), usableDefaults())

	_, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values: []ExportValueRequest{
			{VariablePath: "value.temperature_2m", Column: "reading"},
			{VariablePath: "value.station_name", Column: "reading"},
		},
	})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "reading") {
		t.Errorf("error = %v, want a refusal naming the column", err)
	}
}

func TestCreateExportRefusesAnUnknownOffset(t *testing.T) {
	service := writeService(t, nil, &fakeExportWriter{}, weatherType(), usableDefaults())

	_, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history", Offset: "beginning-ish",
		Values: []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error = %v, want an invalid request", err)
	}
}

// With no export_database_id configured, one database is an answer and two are
// not: a platform with several has them for a reason, and picking the first puts
// the export somewhere nobody asked for.
func TestCreateExportResolvesTheExportDatabaseOnlyWhenThereIsOne(t *testing.T) {
	defaults := usableDefaults()
	defaults.DatabaseID = ""

	writer := &fakeExportWriter{databases: []ExportDatabase{{ID: "db-9", Name: "timescale", Type: "timescaledb"}}}
	service := writeService(t, nil, writer, weatherType(), defaults)
	created, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values: []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
	})
	if err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if writer.sent[0].ExportDatabaseID != "db-9" {
		t.Errorf("ExportDatabaseID = %q, want the only one on offer", writer.sent[0].ExportDatabaseID)
	}
	if created.Derived["export_database_id"] == "" {
		t.Error("derived says nothing about where the export database came from")
	}

	two := &fakeExportWriter{databases: []ExportDatabase{
		{ID: "db-9", Name: "timescale", Type: "timescaledb"},
		{ID: "db-8", Name: "influx", Type: "influxdb"},
	}}
	service = writeService(t, nil, two, weatherType(), defaults)
	_, err = service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values: []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("two databases: error = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "db-8") || !strings.Contains(err.Error(), "db-9") {
		t.Errorf("error = %q, want both named so the developer can configure one", err)
	}
	if len(two.sent) != 0 {
		t.Error("an export was created despite the ambiguity")
	}
}

// The timestamp format is what the export worker parses, and getting it wrong
// produces an export that deploys and writes nothing. With none configured it is
// copied from an export this platform already has, and refused when there is
// none — never guessed.
func TestCreateExportCopiesTheTimestampFormatOrRefuses(t *testing.T) {
	defaults := usableDefaults()
	defaults.TimestampFormat = ""

	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	writer := &fakeExportWriter{}
	service, err := New(Deps{
		Selectables: &fakeSelectables{},
		Instances:   &fakeInstances{serve: []idmodel.Instance{runningInstance()}},
		Types:       &fakeTypes{importType: weatherType()},
		Exports: &fakeExports{serve: []Export{
			{ID: "old", TimestampFormat: "%Y-%m-%d", CreatedAt: older},
			{ID: "new", TimestampFormat: "%Y-%m-%dT%H:%M:%S.%fZ", CreatedAt: newer},
		}},
		ExportWriter:   writer,
		ExportDefaults: defaults,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	created, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values: []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
	})
	if err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if got := writer.sent[0].TimestampFormat; got != "%Y-%m-%dT%H:%M:%S.%fZ" {
		t.Errorf("TimestampFormat = %q, want the newest existing export's", got)
	}
	if !strings.Contains(strings.Join(created.Notes, " "), "copied") {
		t.Errorf("notes = %v, want the copy reported: it is the first thing to check when an "+
			"export stores nothing", created.Notes)
	}

	bare := writeService(t, nil, &fakeExportWriter{}, weatherType(), defaults)
	_, err = bare.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
		Values: []ExportValueRequest{{VariablePath: "value.temperature_2m"}},
	})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "export_timestamp_format") {
		t.Errorf("with nothing to copy: error = %v, want a refusal naming the setting", err)
	}
}

func TestCreateExportRefusesWithNoValues(t *testing.T) {
	service := writeService(t, nil, &fakeExportWriter{}, weatherType(), usableDefaults())
	_, err := service.CreateExport(context.Background(), testToken, CreateExportRequest{
		InstanceID: testInstanceID, Name: "history",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error = %v, want an invalid request", err)
	}
}

func TestDeleteExportPassesTheIdThrough(t *testing.T) {
	writer := &fakeExportWriter{}
	service := writeService(t, nil, writer, weatherType(), usableDefaults())

	if err := service.DeleteExport(context.Background(), testToken, "export-1"); err != nil {
		t.Fatalf("DeleteExport: %v", err)
	}
	if len(writer.deleted) != 1 || writer.deleted[0] != "export-1" {
		t.Errorf("deleted %v, want the one export", writer.deleted)
	}
}

// A writer with no listing behind it could create an export and not tell whether
// one exists, which is half of what creating one needs.
func TestNewRefusesAnExportWriterWithoutTheListing(t *testing.T) {
	_, err := New(Deps{
		Selectables:  &fakeSelectables{},
		Instances:    &fakeInstances{},
		ExportWriter: &fakeExportWriter{},
	})
	if err == nil {
		t.Error("expected an error for a writer without the export listing")
	}
}

func TestNewRefusesAnUnknownConfiguredOffset(t *testing.T) {
	_, err := New(Deps{
		Selectables:    &fakeSelectables{},
		Instances:      &fakeInstances{},
		ExportDefaults: ExportDefaults{Offset: "from-the-middle"},
	})
	if err == nil {
		t.Error("expected an error for an offset the export worker's consumer would not accept")
	}
}

// --- the clients ---

func TestDeployClientCreateAndDelete(t *testing.T) {
	var method, path string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + testInstanceID + `","kafka_topic":"` + testTopic + `"}`))
	}))
	defer server.Close()

	client := NewDeployClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	created, err := client.CreateInstance(context.Background(), testToken, idmodel.Instance{
		// Set on purpose: the client clears all three, because import-deploy refuses a
		// request that carries them rather than treating them as an override.
		Id: "mine", KafkaTopic: "mine", Image: "mine",
		Name: "Leipzig weather", ImportTypeId: testTypeID,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if method != http.MethodPost || path != "/instances" {
		t.Errorf("request = %s %s, want POST /instances", method, path)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if sent["id"] != "" || sent["kafka_topic"] != "" || sent["image"] != "" {
		t.Errorf("body = %v, want id, kafka_topic and image left empty", sent)
	}
	if created.Id != testInstanceID {
		t.Errorf("id = %q, want the one upstream minted", created.Id)
	}

	if err := client.DeleteInstance(context.Background(), testToken, testInstanceID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if method != http.MethodDelete || path != "/instances/"+testInstanceID {
		t.Errorf("request = %s %s, want the delete on the instance", method, path)
	}
}

func TestServingClientCreateDeleteAndDatabases(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/databases":
			_, _ = w.Write([]byte(`[{"ID":"db-1","Name":"timescale","Type":"timescaledb"}]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ID":"export-1","Name":"history"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()

	client := NewServingClient(server.URL, ClientOptions{HTTPClient: server.Client()})
	created, err := client.CreateExport(context.Background(), testToken, ServingRequest{Name: "history"})
	if err != nil {
		t.Fatalf("CreateExport: %v", err)
	}
	if method != http.MethodPost || path != "/instance" {
		t.Errorf("request = %s %s, want POST /instance", method, path)
	}
	if created.ID != "export-1" {
		t.Errorf("id = %q, want the created export", created.ID)
	}

	databases, err := client.ListDatabases(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	if len(databases) != 1 || databases[0].ID != "db-1" {
		t.Errorf("databases = %v, want the one the platform offers", databases)
	}

	if err := client.DeleteExport(context.Background(), testToken, "export-1"); err != nil {
		t.Fatalf("DeleteExport: %v", err)
	}
	if method != http.MethodDelete || path != "/instance/export-1" {
		t.Errorf("request = %s %s, want the delete on the export", method, path)
	}
}
