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
	"errors"
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/analytics-flow-engine/lib/access"
	"github.com/SENERGY-Platform/analytics-flow-engine/lib/exports"
)

// resolveImportExports decides, per import input, whether the run reads its history
// from an analytics-serving export or from the import's Kafka topic.
//
// An import topic holds days, an export holds everything since it was created, so
// a run that trains on a year of an import needs the export. Operator Lib reads
// whatever the config names and checks nothing, and a job has no token of its own
// that could be asked, so the decision is made here, as the developer, and written
// into the config (operatorSettings.ImportExports). The rule -- which exports are
// candidates, that the developer must be able to execute one, what counts as
// ambiguous -- is lib/exports, the same one the flow engine applies when it deploys
// a pipeline.
//
// It returns the config value ("" when no import has an export, so the key is
// left out and Operator Lib reads Kafka exactly as before) and one warning per
// import topic saying where its history comes from. A warning on the happy path
// too, because the Kafka fallback is silent otherwise: a run trains on five days
// of an import and nothing says why.
//
// Fails closed. An ambiguous match is the developer's to decide and is refused as
// an invalid request. A listing or permission error is refused as an upstream
// failure rather than read as "no export": the run would otherwise train on the
// short Kafka history without anyone having chosen that.
//
// No lister, or no timescale-wrapper to read the export through, is not an error:
// every other launch ODE accepts without those services keeps working, so it is
// only said.
func (s *Service) resolveImportExports(
	ctx context.Context, bearer string, topics []InputTopic,
) (string, []string, error) {
	var imported []InputTopic
	for _, topic := range topics {
		if topic.FilterType == access.FilterTypeImport {
			imported = append(imported, topic)
		}
	}
	if len(imported) == 0 {
		return "", nil, nil
	}

	const kafkaOnly = "its history comes from the Kafka topic only, which keeps just the last days"
	if s.exports == nil || s.opts.TimescaleWrapperURL == "" {
		warnings := make([]string, 0, len(imported))
		why := "no analytics-serving is configured, so no export was looked up"
		if s.exports != nil {
			why = "no timescale-wrapper is configured to read an export through"
		}
		for _, topic := range imported {
			warnings = append(warnings, fmt.Sprintf("import input %s: %s; %s", topic.Name, why, kafkaOnly))
		}
		return "", warnings, nil
	}

	entries, err := exports.Resolve(ctx, s.exports, s.access, bearer, asPipeTopics(topics))
	if err != nil {
		if errors.Is(err, exports.ErrAmbiguous) {
			return "", nil, fmt.Errorf(
				"%w: %s; a run reads one export per import, so choose which one by deleting "+
					"the others or by withdrawing your execute right on them, then launch again",
				ErrInvalidRequest, err)
		}
		return "", nil, &UpstreamError{
			Service: "analytics-serving", Resource: "export lookup", Message: err.Error(),
			Err: fmt.Errorf("resolving the exports of the import inputs: %w", err),
		}
	}

	byTopic := make(map[string]exports.ImportExport, len(entries))
	for _, entry := range entries {
		byTopic[entry.Topic] = entry
	}
	warnings := make([]string, 0, len(imported))
	for _, topic := range imported {
		switch entry, ok := byTopic[topic.Name]; {
		case ok:
			warnings = append(warnings, fmt.Sprintf(
				"import input %s: history from export %s", topic.Name, entry.ExportID))
		case strings.Contains(topic.FilterValue, ","):
			warnings = append(warnings, fmt.Sprintf(
				"import input %s names several imports, so no single export applies; %s",
				topic.Name, kafkaOnly))
		default:
			warnings = append(warnings, fmt.Sprintf(
				"import input %s: no timescale export of this import that the developer may "+
					"execute and that covers every mapped path, so Kafka only; "+
					"the topic retains only what it still holds", topic.Name))
		}
	}
	if len(entries) == 0 {
		return "", warnings, nil
	}
	encoded, err := exports.Encode(entries)
	if err != nil {
		return "", nil, err
	}
	return encoded, warnings, nil
}
