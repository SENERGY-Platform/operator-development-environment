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

// The write half of this package: deploying an import instance, and creating the
// export that gives it a history.
//
// Everything else in pkg/imports reads. This file is the exception, and the two
// operations here are the only ones in ODE that change the platform — so the
// rules they follow are stated once, here:
//
//   - **Nothing runs unconfirmed.** Both are reached only through a confirmed
//     tool (D11) or the developer's own HTTP call. This package does not enforce
//     that; pkg/tools does, in Dispatch, and the tools declaring these are marked
//     Confirm.
//
//   - **Validate against the import type before sending.** Both services answer a
//     bad request with a sentence that names no field — "config value of wrong
//     type", a bare 500 — and a caller cannot tell which of eight configs it was.
//     Everything derivable from the import type is therefore checked here, where
//     the answer can name the config and say what it declared.
//
//   - **Never derive what can be read.** The Kafka topic, the image and the
//     instance id are import-deploy's to set; the export's column names are the
//     export's own. This file sends none of them and reads all of them back.
//
// The failure this file exists to prevent is the silent one. Both upstreams have
// a path where a rejected request looks like a successful one — import-deploy
// accepts a config name its import type never declared and passes it to the
// container as environment that nothing reads, and analytics-serving answers 201
// with an empty body when the caller may not access the import. Both are turned
// into errors below.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	idmodel "github.com/SENERGY-Platform/import-deploy/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
)

// ExportDefaults are the four fields of an export that are properties of a
// deployment rather than of the import being exported.
//
// They are configuration because they cannot be derived: the export database is
// created per deployment by analytics-serving's own migration and its id is
// whatever that migration was given, and the timestamp format is whatever the
// export worker of this platform parses. ODE guessing either produces an export
// that is accepted, deploys, and writes nothing — the failure mode this package
// exists to avoid.
//
// Empty fields are not defaults. TimestampFormat and DatabaseID empty mean "find
// it from an export that already exists here", and CreateExport refuses rather
// than inventing one when there is nothing to copy.
type ExportDefaults struct {
	// Offset is the Kafka offset the export worker starts from, passed through as
	// OFFSET_RESET. "smallest" replays the topic's retained history into the
	// export; "largest" starts at the next message.
	Offset string
	// TimePath is where the timestamp sits in an import message. Every import
	// message carries its own `time` beside the `value` payload, which is why this
	// has a usable default at all, unlike the two below.
	TimePath string
	// TimestampFormat is what the export worker parses TimePath with. Deployment
	// specific and not derivable from any pinned library.
	TimestampFormat string
	// DatabaseID is the export database to write into. Empty means "the one this
	// platform has", resolved from the listing at creation time.
	DatabaseID string
}

// OffsetValues are the offsets CreateExport accepts.
//
// The export worker passes this to a Kafka consumer, which accepts the
// librdkafka spellings; ODE takes the two the platform's own export dialog
// offers plus their aliases, and refuses anything else rather than deploying an
// export whose consumer will not start.
func OffsetValues() []string { return []string{"smallest", "earliest", "largest", "latest"} }

func validOffset(offset string) bool {
	for _, allowed := range OffsetValues() {
		if offset == allowed {
			return true
		}
	}
	return false
}

// CreateInstanceRequest deploys one import type.
//
// There is deliberately no Image, Id or KafkaTopic: import-deploy refuses a
// request that sets an id or a topic, and takes the image from the import type.
type CreateInstanceRequest struct {
	ImportTypeID string
	Name         string
	Configs      []ConfigValue
	// Restart is nil unless the caller means to differ from the import type's own
	// default_restart, which is what the platform applies otherwise.
	Restart *bool
}

// ConfigValue is one import config the caller sets.
type ConfigValue struct {
	Name  string
	Value any
}

// CreatedInstance is what a deployment produced, with what ODE decided along the
// way. Defaulted and Notes exist so the answer says which values the developer is
// actually running with — a created import whose configs all came from defaults
// looks identical to one the caller configured, and only one of the two is worth
// checking afterwards.
type CreatedInstance struct {
	Instance idmodel.Instance `json:"instance"`
	// Defaulted names the configs left at the import type's default value.
	Defaulted []string `json:"defaulted,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

// CreateInstance deploys an import instance on behalf of the caller.
//
// The import type is read first, and not only to validate: import-deploy accepts
// a config whose name no config of the type declares, marshals it into the
// container's CONFIG environment and never reads it back. A typo therefore
// produces a running import that ignores the setting it was given, which is
// indistinguishable from a working one until its data is wrong.
func (s *Service) CreateInstance(ctx context.Context, token string, req CreateInstanceRequest) (CreatedInstance, error) {
	if s.deployer == nil {
		return CreatedInstance{}, fmt.Errorf(
			"%w: no import-deploy is configured, so an import cannot be deployed", ErrInvalidRequest)
	}
	if s.types == nil {
		// Refusing rather than sending it blind. Without the type there is no way to
		// check a config name, and the failure that would let through is silent.
		return CreatedInstance{}, fmt.Errorf(
			"%w: no import-repository is configured, so the import type cannot be read and its "+
				"configs cannot be checked before deploying", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.ImportTypeID) == "" {
		return CreatedInstance{}, fmt.Errorf("%w: an import_type_id is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Name) == "" {
		return CreatedInstance{}, fmt.Errorf(
			"%w: a name is required: it is what the developer will find this import under",
			ErrInvalidRequest)
	}

	importType, err := s.types.ReadImportType(ctx, token, req.ImportTypeID)
	if err != nil {
		return CreatedInstance{}, err
	}

	configs, defaulted, notes, err := resolveConfigs(importType, req.Configs)
	if err != nil {
		return CreatedInstance{}, err
	}

	created, err := s.deployer.CreateInstance(ctx, token, idmodel.Instance{
		Name:         strings.TrimSpace(req.Name),
		ImportTypeId: importType.Id,
		Configs:      configs,
		Restart:      req.Restart,
	})
	if err != nil {
		return CreatedInstance{}, err
	}
	if strings.TrimSpace(created.Id) == "" {
		return CreatedInstance{}, fmt.Errorf(
			"import-deploy accepted the request and returned an instance with no id, so what was "+
				"created cannot be addressed; look for an instance named %q before retrying",
			req.Name)
	}
	return CreatedInstance{Instance: created, Defaulted: defaulted, Notes: notes}, nil
}

// DeleteInstance removes an import instance.
//
// It deletes the instance's Kafka topic with it, upstream, which is why this is
// not the undo it looks like: every retained message of that import is gone and
// any pipeline consuming the topic stops receiving. The caller decides whether it
// is allowed to be reached; pkg/tools restricts it to what the session itself
// created.
func (s *Service) DeleteInstance(ctx context.Context, token string, id string) error {
	if s.deployer == nil {
		return fmt.Errorf("%w: no import-deploy is configured", ErrInvalidRequest)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: an instance id is required", ErrInvalidRequest)
	}
	return s.deployer.DeleteInstance(ctx, token, id)
}

// resolveConfigs checks the requested configs against the type and fills the rest
// from its defaults, mirroring what import-deploy does server-side so that the
// refusal names the config rather than being upstream's unattributed sentence.
func resolveConfigs(importType dsmodel.ImportType, requested []ConfigValue) (
	configs []idmodel.InstanceConfig, defaulted []string, notes []string, err error,
) {
	declared := make(map[string]dsmodel.ImportTypeConfig, len(importType.Configs))
	for _, config := range importType.Configs {
		declared[config.Name] = config
	}

	given := make(map[string]ConfigValue, len(requested))
	for _, config := range requested {
		name := strings.TrimSpace(config.Name)
		if _, known := declared[name]; !known {
			return nil, nil, nil, fmt.Errorf(
				"%w: import type %s declares no config %q, and import-deploy would accept it, "+
					"pass it to the container and never read it back — its configs are %v",
				ErrInvalidRequest, importType.Id, config.Name, declaredNames(importType))
		}
		if _, twice := given[name]; twice {
			return nil, nil, nil, fmt.Errorf(
				"%w: config %q is set twice", ErrInvalidRequest, name)
		}
		given[name] = ConfigValue{Name: name, Value: config.Value}
	}

	for _, config := range importType.Configs {
		supplied, wasGiven := given[config.Name]

		if secretShaped(config.Name) {
			// Both halves of this are the same rule: a credential is the developer's to
			// enter, in the platform's own import dialog, and never something ODE sends
			// on a model's word. The confirmation card shows arguments but cannot edit
			// them, so "the developer will notice" is not a control here.
			if wasGiven {
				return nil, nil, nil, fmt.Errorf(
					"%w: config %q reads as a credential, and ODE does not deploy an import with a "+
						"credential it was handed in a chat: create this import in the platform's "+
						"import dialog, where the value is entered by the developer",
					ErrInvalidRequest, config.Name)
			}
			if isEmptyValue(config.DefaultValue) {
				return nil, nil, nil, fmt.Errorf(
					"%w: config %q reads as a credential and import type %s declares no default for "+
						"it, so this import cannot be deployed from here: create it in the platform's "+
						"import dialog instead",
					ErrInvalidRequest, config.Name, importType.Id)
			}
			notes = append(notes, fmt.Sprintf(
				"config %q reads as a credential and was left at the import type's default; nothing "+
					"from this session was sent for it", config.Name))
		}

		value := config.DefaultValue
		if wasGiven {
			value = supplied.Value
		} else {
			defaulted = append(defaulted, config.Name)
		}

		if !validConfigValue(config.Type, value) {
			if !wasGiven {
				return nil, nil, nil, fmt.Errorf(
					"%w: import type %s declares config %q as %s and its own default value is not one, "+
						"so this import cannot be deployed without setting it",
					ErrInvalidRequest, importType.Id, config.Name, config.Type)
			}
			return nil, nil, nil, fmt.Errorf(
				"%w: config %q is declared as %s and the value given is %T",
				ErrInvalidRequest, config.Name, config.Type, value)
		}

		configs = append(configs, idmodel.InstanceConfig{Name: config.Name, Value: value})
	}

	return configs, defaulted, notes, nil
}

func declaredNames(importType dsmodel.ImportType) []string {
	out := make([]string, 0, len(importType.Configs))
	for _, config := range importType.Configs {
		out = append(out, config.Name)
	}
	sort.Strings(out)
	return out
}

// validConfigValue mirrors import-deploy's own validateConfig, including its
// treatment of a nil value as acceptable — a config left unset deploys, and the
// container decides what to do about it.
//
// It is a mirror rather than a call because the function is unexported upstream.
// The point of duplicating it is the error message: upstream's answer is "config
// value of wrong type" with no field name, which is unactionable for an import
// type with eight configs.
func validConfigValue(declared models.Type, value any) bool {
	if value == nil {
		return true
	}
	switch declared {
	case models.String:
		_, ok := value.(string)
		return ok
	case models.Integer:
		// JSON has one number type, so an integer arrives as a float64 and is an
		// integer only if it has no fractional part. Upstream applies exactly this
		// test, and a caller sending 1.5 for an integer config would otherwise be
		// refused there rather than here.
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	case models.Float:
		_, ok := value.(float64)
		return ok
	case models.Boolean:
		_, ok := value.(bool)
		return ok
	case models.List:
		_, ok := value.([]any)
		return ok
	case models.Structure:
		// Upstream accepts anything here, and matching that is deliberate: refusing
		// what the platform would accept makes ODE the stricter of the two for no
		// reason a developer could act on.
		return true
	default:
		return false
	}
}

// isEmptyValue reports a default that is not usable as one. A config declared
// with no default arrives as a JSON null, and an import type that declares an
// empty string for a password means the same thing.
func isEmptyValue(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}

// secretShaped guesses whether a config name names a credential.
//
// It is a heuristic and cannot be anything else: an import type declares a name,
// a type and a description, and nothing marks a config as sensitive. It therefore
// errs toward refusing — a config called `api_key_rotation_days` is refused and
// the developer creates that import in the platform dialog, which costs them one
// step. The opposite error costs a model-invented credential sent to a container.
func secretShaped(name string) bool {
	lowered := strings.ToLower(name)
	// Substring matches for the words that are unambiguous wherever they appear.
	for _, needle := range []string{
		"secret", "password", "passwd", "credential", "apikey", "privatekey",
		"accesskey", "clientkey", "authkey", "token",
	} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	// Segment matches for the short ones, which appear inside innocent words:
	// "key" is in "keyword" and "monkey", "pass" is in "passenger".
	for _, segment := range strings.FieldsFunc(lowered, func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		switch segment {
		case "key", "keys", "pass", "pw", "pwd", "auth", "cert", "certificate":
			return true
		}
	}
	return false
}

// CreateExportRequest creates the analytics-serving export that gives an import
// instance a history in timescale.
type CreateExportRequest struct {
	InstanceID  string
	Name        string
	Description string
	// Values are the variables to store. Empty is refused rather than read as
	// "all": an export carries the columns it was created with and adding one
	// later means creating another export, so an accidental empty selection is
	// expensive to discover.
	Values []ExportValueRequest
	// Offset overrides ExportDefaults.Offset. Empty takes the default.
	Offset string
	// TimePath overrides ExportDefaults.TimePath. Empty takes the default, which
	// is the envelope's own `time` — the moment the import wrote the message.
	//
	// An import whose values describe a time other than their arrival needs this:
	// a backfill replays years of history in minutes, and with the envelope time
	// the whole of it lands inside those minutes. The export deploys, the rows are
	// there, and the series is unusable. It is per export rather than per
	// deployment because it is a property of the import type, and one platform
	// carries import types of both kinds.
	TimePath string
	// TimestampFormat overrides ExportDefaults.TimestampFormat. Empty takes the
	// default.
	//
	// It belongs beside TimePath: the format is what the export worker parses the
	// field TimePath names with, so pointing TimePath at a different field usually
	// means a different format. Setting one without the other is allowed — the two
	// fields do sometimes carry the same format — and reported in the notes,
	// because a format that does not parse stores no rows at all.
	TimestampFormat string
}

// ExportValueRequest is one variable of the import, and the column it lands in.
type ExportValueRequest struct {
	// VariablePath is message-relative, as a Selectable carries it.
	VariablePath string
	// Column is the timescale column name. Empty takes the variable's own last
	// path element, which is what the platform's export dialog proposes.
	Column string
	// Tag marks a column as a tag rather than a measurement.
	Tag bool
}

// CreatedExport is the export as analytics-serving stored it, with the derived
// values ODE had to decide reported back.
type CreatedExport struct {
	Export Export `json:"export"`
	// Derived says where the deployment-specific fields came from, because "which
	// export database did this land in" is otherwise unanswerable from the result.
	Derived map[string]string `json:"derived,omitempty"`
	Notes   []string          `json:"notes,omitempty"`
}

// CreateExport creates an export of one import instance.
//
// Four fields of the request are the deployment's rather than the import's —
// the export database, the timestamp format, the time path and the offset — and
// none of them is derivable from an import type. They come from ExportDefaults,
// and where a default is empty they are copied from an export that already exists
// on this platform rather than guessed. When there is nothing to copy, this
// refuses and says which setting is missing: an export created with a timestamp
// format the export worker cannot parse deploys cleanly and writes no rows.
//
// Three of the four can be overridden per export — the offset, the time path and
// the timestamp format — and Derived says of each where it came from, because an
// export created entirely from defaults looks exactly like a configured one
// otherwise.
func (s *Service) CreateExport(ctx context.Context, token string, req CreateExportRequest) (CreatedExport, error) {
	if s.exportWriter == nil || s.exports == nil {
		return CreatedExport{}, fmt.Errorf(
			"%w: no analytics-serving is configured, so an export cannot be created", ErrInvalidRequest)
	}
	if s.types == nil {
		return CreatedExport{}, fmt.Errorf(
			"%w: no import-repository is configured, so the import type cannot be read and the "+
				"exported variables cannot be typed", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.InstanceID) == "" {
		return CreatedExport{}, fmt.Errorf("%w: an instance_id is required", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Name) == "" {
		return CreatedExport{}, fmt.Errorf("%w: a name is required", ErrInvalidRequest)
	}
	if len(req.Values) == 0 {
		return CreatedExport{}, fmt.Errorf(
			"%w: an export with no values would create an empty table; name the variables to store",
			ErrInvalidRequest)
	}

	offset := strings.TrimSpace(req.Offset)
	if offset == "" {
		offset = s.exportDefaults.Offset
	}
	if !validOffset(offset) {
		return CreatedExport{}, fmt.Errorf(
			"%w: offset %q is not one of %v", ErrInvalidRequest, offset, OffsetValues())
	}

	instance, err := s.Get(ctx, token, req.InstanceID)
	if err != nil {
		return CreatedExport{}, err
	}
	if strings.TrimSpace(instance.KafkaTopic) == "" {
		return CreatedExport{}, fmt.Errorf(
			"%w: instance %s carries no kafka topic, so there is nothing for an export to consume",
			ErrInvalidRequest, instance.Id)
	}

	importType, err := s.types.ReadImportType(ctx, token, instance.ImportTypeId)
	if err != nil {
		return CreatedExport{}, err
	}
	values, err := exportValues(importType, req.Values)
	if err != nil {
		return CreatedExport{}, err
	}

	derived := map[string]string{}
	notes := []string{}

	databaseID := s.exportDefaults.DatabaseID
	if databaseID != "" {
		derived["export_database_id"] = "configuration"
	} else {
		databaseID, err = s.resolveExportDatabase(ctx, token)
		if err != nil {
			return CreatedExport{}, err
		}
		derived["export_database_id"] = "the only export database this platform offers"
	}

	timePath := strings.TrimSpace(req.TimePath)
	if timePath != "" {
		timePath, err = messageTimePath(importType, timePath)
		if err != nil {
			return CreatedExport{}, err
		}
		derived["time_path"] = "the request"
	} else {
		timePath = strings.TrimSpace(s.exportDefaults.TimePath)
		if timePath == "" {
			return CreatedExport{}, fmt.Errorf(
				"%w: no export_time_path is configured and none was requested, and an export without "+
					"one is refused by the export worker rather than defaulted", ErrInvalidRequest)
		}
		derived["time_path"] = "configuration"
	}

	timestampFormat := strings.TrimSpace(req.TimestampFormat)
	switch {
	case timestampFormat != "":
		derived["timestamp_format"] = "the request"
	case s.exportDefaults.TimestampFormat != "":
		timestampFormat = s.exportDefaults.TimestampFormat
		derived["timestamp_format"] = "configuration"
	default:
		// Read only here. With a format configured or requested this listing answers
		// nothing that is asked, and it is the widest read in the package — a thousand
		// exports, because analytics-serving cannot filter by import.
		existing, _, listErr := s.exports.ListExports(ctx, token, exportListLimit, 0)
		if listErr != nil {
			return CreatedExport{}, fmt.Errorf(
				"%w: no export_timestamp_format is configured and the existing exports could not be "+
					"read to copy one: %s", ErrInvalidRequest, listErr.Error())
		}
		copied, from, found := timestampFormatOf(existing)
		if !found {
			return CreatedExport{}, fmt.Errorf(
				"%w: no export_timestamp_format is configured and this platform has no export to copy "+
					"one from, so ODE would have to guess how the export worker parses a timestamp — "+
					"set export_timestamp_format, or create the first export in the platform's own "+
					"export dialog", ErrInvalidRequest)
		}
		timestampFormat = copied
		derived["timestamp_format"] = "copied from export " + from
		notes = append(notes, "the timestamp format was copied from an existing export rather than "+
			"configured; if this export stores no rows, that is the first thing to check")
	}

	if derived["time_path"] == "the request" && derived["timestamp_format"] != "the request" {
		// The pairing is the whole risk of a per-export time path: the format was
		// written for the field the deployment names, and this export reads another.
		notes = append(notes, "the time path "+timePath+" is this export's own while the timestamp "+
			"format is not; the format is what the export worker parses that field with, so if "+
			timePath+" is formatted differently the export deploys and stores no row at all")
	}

	created, err := s.exportWriter.CreateExport(ctx, token, ServingRequest{
		FilterType: FilterTypeImportExport,
		Filter:     instance.Id,
		Name:       strings.TrimSpace(req.Name),
		// The platform's own export dialog puts the instance name and the import type
		// id here, and analytics-serving searches on both. Matching it keeps an
		// ODE-created export findable the same way as any other.
		EntityName:       instance.Name,
		ServiceName:      instance.ImportTypeId,
		Description:      req.Description,
		Topic:            instance.KafkaTopic,
		TimePath:         timePath,
		Offset:           offset,
		Values:           values,
		ExportDatabaseID: databaseID,
		TimestampFormat:  timestampFormat,
	})
	if err != nil {
		return CreatedExport{}, err
	}
	if strings.TrimSpace(created.ID) == "" {
		// The one upstream answer that has to be caught here. analytics-serving's
		// CreateInstance returns an empty instance and a nil error when the caller has
		// no access to the import, and its handler encodes that as a 201 — so a
		// permission refusal arrives looking exactly like a success.
		return CreatedExport{}, fmt.Errorf(
			"analytics-serving answered without an export id, which is how it reports that this "+
				"account may not access import %s: no export was created", instance.Id)
	}

	if running, known := Running(instance); known && !running {
		notes = append(notes, "the import is not running, so the export will receive nothing until "+
			"the instance is started")
	} else if !known {
		notes = append(notes, "whether the import is running could not be established; an export of a "+
			"stopped import deploys cleanly and stores nothing")
	}
	if offset == "smallest" || offset == "earliest" {
		notes = append(notes, "the export starts at the oldest message the topic still retains, which "+
			"is Kafka's retention window rather than the import's whole past")
	}

	return CreatedExport{Export: created, Derived: derived, Notes: notes}, nil
}

// DeleteExport removes an export.
//
// This drops its timescale table upstream: the stored history goes with it. Like
// DeleteInstance, whether it may be reached at all is the caller's decision.
func (s *Service) DeleteExport(ctx context.Context, token string, id string) error {
	if s.exportWriter == nil {
		return fmt.Errorf("%w: no analytics-serving is configured", ErrInvalidRequest)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: an export id is required", ErrInvalidRequest)
	}
	return s.exportWriter.DeleteExport(ctx, token, id)
}

// ExportDefaults reports what this deployment was configured with, so a tool can
// tell the developer what an export will be created with before it is.
func (s *Service) ExportDefaults() ExportDefaults { return s.exportDefaults }

// resolveExportDatabase finds the export database to write into when none is
// configured.
//
// Refusing on ambiguity is deliberate. A platform with two databases has them for
// a reason — one is influx and one is timescale, or one is a second deployment —
// and picking the first would put the export somewhere the developer did not ask
// for, where it is found only by the history lookup coming back empty.
func (s *Service) resolveExportDatabase(ctx context.Context, token string) (string, error) {
	databases, err := s.exportWriter.ListDatabases(ctx, token)
	if err != nil {
		return "", fmt.Errorf(
			"%w: no export_database_id is configured and the export databases could not be listed: %s",
			ErrInvalidRequest, err.Error())
	}
	switch len(databases) {
	case 0:
		return "", fmt.Errorf(
			"%w: no export_database_id is configured and this platform offers none, so there is "+
				"nowhere for an export to write", ErrInvalidRequest)
	case 1:
		return databases[0].ID, nil
	default:
		names := make([]string, 0, len(databases))
		for _, database := range databases {
			names = append(names, fmt.Sprintf("%s (%s, %s)", database.ID, database.Name, database.Type))
		}
		sort.Strings(names)
		return "", fmt.Errorf(
			"%w: no export_database_id is configured and this platform offers %d, so which one an "+
				"export belongs in is not ODE's to decide: %v", ErrInvalidRequest, len(databases), names)
	}
}

// timestampFormatOf finds a format to copy from the exports that already exist.
//
// Newest first, because a format that changed is more likely to have changed
// forward than back.
func timestampFormatOf(existing []Export) (format string, exportID string, found bool) {
	ordered := make([]Export, len(existing))
	copy(ordered, existing)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].CreatedAt.After(ordered[j].CreatedAt)
	})
	for _, export := range ordered {
		if strings.TrimSpace(export.TimestampFormat) != "" {
			return export.TimestampFormat, export.ID, true
		}
	}
	return "", "", false
}

// exportValues maps requested variable paths onto the export's own value shape.
//
// The type is translated and the path is not, and getting either wrong is a
// silent failure. An export's value path stays message-relative, exactly as a
// Selectable carries it: analytics-serving puts the value paths and the TimePath
// into one mappings map — `addMappings` in internal/ew-api/util.go and
// `addTimescaleDBTimeMapping` in internal/ew-api/timescaledb.go — and the export
// worker resolves every entry of it against the same message document. The type
// is the export worker's own vocabulary rather than the platform's content
// variable types.
func exportValues(importType dsmodel.ImportType, requested []ExportValueRequest) ([]ServingRequestValue, error) {
	byPath := map[string]models.Type{}
	collectImportLeaves(importType.Output, nil, byPath)

	values := make([]ServingRequestValue, 0, len(requested))
	columns := map[string]string{}
	for _, value := range requested {
		path, err := MessagePath(value.VariablePath)
		if err != nil {
			return nil, err
		}
		declared, known := byPath[path]
		if !known {
			return nil, fmt.Errorf(
				"%w: import type %s has no variable %s, so an export column for it would never be "+
					"written", ErrInvalidRequest, importType.Id, path)
		}
		exportType, ok := exportTypeOf(declared)
		if !ok {
			return nil, fmt.Errorf(
				"%w: variable %s is declared as %s, which an export has no column type for — an "+
					"export stores leaves, not structures", ErrInvalidRequest, path, declared)
		}

		column := strings.TrimSpace(value.Column)
		if column == "" {
			segments := strings.Split(path, ".")
			column = segments[len(segments)-1]
		}
		if previous, duplicate := columns[column]; duplicate {
			// Upstream would create both and the table would carry one column, so the
			// second variable would silently overwrite the first.
			return nil, fmt.Errorf(
				"%w: column %q is used for both %s and %s; one column takes one variable",
				ErrInvalidRequest, column, previous, path)
		}
		columns[column] = path

		values = append(values, ServingRequestValue{
			Name: column,
			Type: exportType,
			// Message-relative, as MessagePath guarantees it. Trimming the envelope
			// prefix off would address the message root, where the payload's leaves are
			// not — the timestamp would still resolve, so rows land with every column
			// null rather than nothing landing at all.
			Path: path,
			Tag:  value.Tag,
		})
	}

	sort.SliceStable(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	return values, nil
}

// messageTimePath checks a requested time path against the import type and
// returns it message-relative.
//
// It cannot go through MessagePath. That refuses everything outside the `value`
// payload, which is right for a column and wrong here: the envelope's own `time`
// is the default time path and has to stay sayable. The check against the type is
// for the same reason every other path is checked — a time path that names
// nothing is not an error upstream, it is an export that consumes the topic and
// writes no row.
func messageTimePath(importType dsmodel.ImportType, path string) (string, error) {
	path = strings.Trim(strings.TrimSpace(path), ".")
	if path == "" {
		return "", fmt.Errorf("%w: an empty time path names no timestamp", ErrInvalidRequest)
	}
	// A path repeated back from an import type still carries the output root on the
	// front, and the message starts one below it. Matched on the root's own name
	// rather than on "root", which no import type is obliged to call it.
	if root := strings.TrimSpace(importType.Output.Name); root != "" {
		path = strings.TrimPrefix(path, root+".")
	}

	leaves := map[string]models.Type{}
	collectMessageLeaves(importType.Output, nil, leaves)
	if _, known := leaves[path]; known {
		return path, nil
	}

	offered := make([]string, 0, len(leaves))
	for leaf := range leaves {
		offered = append(offered, leaf)
	}
	sort.Strings(offered)
	return "", fmt.Errorf(
		"%w: import type %s has no variable %s for an export to read its timestamps from, so the "+
			"export would consume the topic and store nothing; it carries %v",
		ErrInvalidRequest, importType.Id, path, offered)
}

// collectMessageLeaves flattens an import type's output into message-relative
// paths, keeping the envelope's own leaves as well as the payload's.
//
// The difference to collectImportLeaves is deliberate: a column can only be a
// payload leaf, and a timestamp is the one field that legitimately sits in the
// envelope beside it.
func collectMessageLeaves(variable dsmodel.ImportContentVariable, prefix []string, out map[string]models.Type) {
	path := append(append([]string{}, prefix...), variable.Name)
	if len(variable.SubContentVariables) == 0 {
		// path[0] is the output root, which the message a Selectable addresses starts
		// below; a root with no children addresses nothing at all.
		if len(path) > 1 {
			out[strings.Join(path[1:], ".")] = variable.Type
		}
		return
	}
	for _, sub := range variable.SubContentVariables {
		collectMessageLeaves(sub, path, out)
	}
}

// collectImportLeaves flattens an import type's output into message-relative
// paths, keeping only the leaves — a structure is a container and has no column.
func collectImportLeaves(variable dsmodel.ImportContentVariable, prefix []string, out map[string]models.Type) {
	path := append(append([]string{}, prefix...), variable.Name)
	if len(variable.SubContentVariables) == 0 {
		if addressable, err := MessagePath(strings.Join(path, ".")); err == nil {
			out[addressable] = variable.Type
		}
		return
	}
	for _, sub := range variable.SubContentVariables {
		collectImportLeaves(sub, path, out)
	}
}

// exportTypeOf maps a content variable type onto the export worker's own, which
// is the vocabulary its timescale type map is keyed on.
func exportTypeOf(declared models.Type) (string, bool) {
	switch declared {
	case models.String:
		return "string", true
	case models.Float:
		return "float", true
	case models.Integer:
		return "int", true
	case models.Boolean:
		return "bool", true
	default:
		return "", false
	}
}
