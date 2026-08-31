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
	"encoding/json"
	"fmt"
	"strings"
)

// The deployment config a run is given, in Operator Lib's own shape.
//
// This file exists because ODE no longer performs the ML integration itself. It
// used to submit a Ray job and create an MLflow run, and set MLFLOW_TRACKING_URI,
// MLFLOW_EXPERIMENT_ID and MLFLOW_RUN_ID on the job so that the developer's code
// could log — a second implementation of what Operator Lib already does, keyed on
// names Operator Lib does not read. The two never met: `provide_historic_data`
// wants DeploymentConfig.config, and nothing ODE set could reach it.
//
// So ODE builds what Operator Lib actually reads. MLOperator then sets the
// tracking URI, opens the run, connects to Ray, calls train() and registers the
// result, exactly as it does when a deployed operator first comes up. What ODE
// keeps is the part Operator Lib has no opinion about: the commit the package was
// built from, and the comparison against the previous run.

// InputTopic is one input of the operator being trained, as Operator Lib's
// OperatorConfig parses it. The field names are Operator Lib's and are camelCase
// on the wire because that is what it reads — not this repository's convention.
type InputTopic struct {
	Name string `json:"name"`
	// FilterType is "DeviceId" or "OperatorId"; FilterValue the id it matches.
	FilterType  string         `json:"filterType"`
	FilterValue string         `json:"filterValue"`
	Mappings    []TopicMapping `json:"mappings"`
}

// TopicMapping binds one path in the message to one name the operator sees. Dest
// is what infer() finds in `data`, Source the path in the Kafka message.
type TopicMapping struct {
	Dest   string `json:"dest"`
	Source string `json:"source"`
}

// operatorConfig is the CONFIG environment variable's JSON.
type operatorConfig struct {
	Config      operatorSettings `json:"config"`
	InputTopics []InputTopic     `json:"inputTopics"`
}

// operatorSettings is Operator Lib's Config: the three URLs it connects with,
// plus the log level. Its defaults are compiled into the library and point at
// in-cluster service names, which is why every one of them is set here rather
// than left out — a deployment whose Ray or MLflow is somewhere else would
// otherwise train against whatever those names happen to resolve to.
type operatorSettings struct {
	LoggerLevel string `json:"logger_level"`
	MLflowURL   string `json:"mlflow_url"`
	RayURL      string `json:"ray_url"`
	TsConn      string `json:"ts_conn"`
}

// modelID is the key Operator Lib registers a model under, built in
// MLOperator.init() as `pipeline-{pipeline_id}_operator-{operator_id}`.
//
// It is also what MLOperator passes to set_experiment, but that has no effect on
// where a run's metrics land: the run is opened with start_run(), which resumes
// the run MLFLOW_RUN_ID names whatever experiment is selected. So the run stays in
// ODE's D17 experiment and this string is the registry key alone. The cost is one
// empty MLflow experiment per launch, which is litter rather than a problem.
func modelID(pipelineID, operatorID string) string {
	return fmt.Sprintf("pipeline-%s_operator-%s", pipelineID, operatorID)
}

// pipelineID and operatorID are what a *deployed* operator gets from the flow
// engine. A run being developed has no deployment, so ODE synthesises both.
//
// The pipeline is stable per developer, so their runs are recognisable in a
// tracking server they share with the platform. The operator is the ODE
// experiment id, which makes the pair unique per launch, and that is load
// bearing in two ways:
//
//   - MLOperator.init() trains only when no model is registered under the pair.
//     A pair unique per launch misses by construction, so every experiment
//     trains. A pair stable per repository would train on the first launch and
//     silently record nothing on the second.
//   - register_model and the "production" alias are scoped to the pair, so a run
//     started here can never move the alias of a deployed operator, whose pair is
//     a flow-engine pipeline id and a real operator id.
//
// The MLflow *experiment* is not per launch and must not become so: Store.Previous
// scopes the comparison of §5.13 to one experiment, so a per-launch experiment
// would report every run as a first run.
func (s *Service) pipelineID(req Request) string {
	return sanitiseSegment(s.opts.ExperimentPrefix + "-" + usernameOf(req))
}

func operatorID(experimentID string) string { return sanitiseSegment(experimentID) }

// deploymentEnvironment builds the variables Operator Lib reads.
//
// Everything here is a name from DeploymentConfig in operator_lib/util/config.py
// or from Operator Lib's own use of MLflow. Nothing is invented, and nothing ODE
// used to set survives except MLFLOW_RUN_ID — see adoptRun below for why that one
// is still worth setting.
//
// pipelineID and operatorID are passed rather than read back off the record: they
// are derived, not stored, and a record reloaded from the store would carry empty
// ones.
func (s *Service) deploymentEnvironment(
	record Experiment, pipelineID, operatorID string, topics []InputTopic, runID string,
) (map[string]string, error) {
	config := operatorConfig{
		Config: operatorSettings{
			LoggerLevel: "info",
			MLflowURL:   s.opts.MLflowURL,
			// Not RayURL: that is the dashboard ODE submits jobs to over HTTP, and
			// ray.init() would reject it. A run's driver is already on the cluster, so
			// "auto" attaches to the cluster around it rather than opening a client
			// connection back into the cluster it is in.
			RayURL: s.opts.RayClientURL,
			TsConn: s.opts.TsConn,
		},
		// Never nil: Operator Lib iterates it without checking, and a null here is a
		// TypeError inside the job rather than a refusal the developer can read.
		InputTopics: topics,
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("the deployment config could not be encoded: %w", err)
	}

	environment := map[string]string{
		"CONFIG":      string(encoded),
		"PIPELINE_ID": pipelineID,
		"OPERATOR_ID": operatorID,
		// The consumer group and the output topic. A run stops before it subscribes
		// or produces, so neither is used — but DeploymentConfig reads both, and a
		// missing one is an attribute error at import time rather than at first use.
		"CONFIG_APPLICATION_ID": "ode-" + record.ID,
		"OUTPUT":                record.Repository,
		"DEVICE_ID_PATH":        "device_id",
		// Empty is legitimate and means this run can only train from topics that are
		// timescale-backed; a Kafka-replayed topic needs a broker.
		"CONFIG_BOOTSTRAP_SERVERS":          s.opts.KafkaBootstrap,
		"CONSUMER_AUTO_OFFSET_RESET_CONFIG": "smallest",
		"METRICS":                           "false",

		// ODE's own two, which Operator Lib ignores and the run's tags carry. Kept
		// because a developer reading a job's environment should be able to tell
		// which commit it is.
		"ODE_COMMIT_SHA":    record.CommitSHA,
		"ODE_EXPERIMENT_ID": record.ID,

		// The developer's own authorisation, never ODE's (§3.1 step 3). Operator Lib
		// does not read it; code the developer wrote may, and the platform helpers in
		// the singleuser image do.
		"SENERGY_TOKEN": "",
	}

	// MLFLOW_RUN_ID is how the job adopts the run ODE created rather than opening
	// a second one. MLOperator calls mlflow.start_run(run_name=...) without a
	// run_id, and MLflow's fluent API resumes the run this names when no run_id is
	// passed. Without it there would be two runs per experiment: ODE's, carrying
	// the commit tag and no metrics, and the job's, carrying the metrics and no
	// commit — and get_experiment_results reads ODE's.
	if runID != "" {
		environment["MLFLOW_RUN_ID"] = runID
	}
	return environment, nil
}

// reservedEnv are the variables ODE sets on every job, which a launch may not
// override.
//
// The list is Operator Lib's names now rather than MLflow's. Overriding CONFIG
// would let a caller point a run at another developer's input topics, which is
// the one thing in a deployment config that decides what data a run reads;
// overriding MLFLOW_RUN_ID would point the job's logging at somebody else's run;
// and overriding the platform token would be a way to hand a job a credential ODE
// did not mint.
var reservedEnv = map[string]bool{
	"CONFIG":                   true,
	"PIPELINE_ID":              true,
	"OPERATOR_ID":              true,
	"CONFIG_APPLICATION_ID":    true,
	"CONFIG_BOOTSTRAP_SERVERS": true,
	"OUTPUT":                   true,
	"MLFLOW_RUN_ID":            true,
	"SENERGY_TOKEN":            true,
	"ODE_COMMIT_SHA":           true,
	"ODE_EXPERIMENT_ID":        true,
	"SENERGY_DEVICE_REPO_URL":  true,
	"SENERGY_TIMESCALE_URL":    true,
}

// requireInputTopics refuses a launch that has nothing to read.
//
// An empty inputTopics is not a smaller experiment: provide_historic_data returns
// an empty list, the scaffold's train() raises on it, and what the developer sees
// is a failed run with a Python traceback in a log they have to go and find. The
// refusal names the tool that fixes it instead, the way the uncommitted-changes
// refusal names the commit.
func requireInputTopics(topics []InputTopic) error {
	if len(topics) > 0 {
		return nil
	}
	return fmt.Errorf(
		"%w: this experiment has no input topics, so a run would read no history and "+
			"fail inside train(); choose the operator's inputs first (propose_data_selection "+
			"for a device, propose_operator_input for an import)", ErrInvalidRequest)
}

// validateTopics checks what arrives from an HTTP body or an LLM tool call.
func validateTopics(topics []InputTopic) error {
	for _, topic := range topics {
		if strings.TrimSpace(topic.Name) == "" {
			return fmt.Errorf("%w: an input topic needs a name", ErrInvalidRequest)
		}
		if len(topic.Mappings) == 0 {
			return fmt.Errorf(
				"%w: input topic %s binds no values, so it would subscribe and read nothing",
				ErrInvalidRequest, topic.Name)
		}
		for _, mapping := range topic.Mappings {
			if strings.TrimSpace(mapping.Dest) == "" || strings.TrimSpace(mapping.Source) == "" {
				return fmt.Errorf(
					"%w: a mapping on %s needs both a dest and a source", ErrInvalidRequest, topic.Name)
			}
		}
	}
	return nil
}
