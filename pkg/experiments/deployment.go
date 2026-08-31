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

	pipe "github.com/SENERGY-Platform/analytics-pipeline/lib"
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
	// TsWrapperURL rather than a ts_conn.
	//
	// Operator Lib reads history either over a database DSN or through
	// timescale-wrapper with the platform token, and it prefers the DSN where it
	// has one. A run executes Python the developer wrote, so a DSN in its
	// environment is a credential handed to untrusted code -- os.environ["CONFIG"]
	// is all it takes. The wrapper needs no credential in the job and checks the
	// developer's own Execute permission on each device, which is the authority
	// this path lost when experiments moved onto the operator path (SNRGY-4637).
	//
	// A deployed operator keeps the DSN: the flow engine sets one and gives it no
	// token, and its code is a reviewed image rather than a working copy.
	TsWrapperURL string `json:"ts_wrapper_url,omitempty"`
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
// Both are stable: the pipeline per developer, the operator per repository. That
// makes Operator Lib's `model_id` stable too, so a repository's model versions
// accumulate under one registry key and the "production" alias moves between
// them — which is what a deployed operator does, and the whole point of running
// the real path.
//
// It used to be the ODE experiment id, unique per launch, for one reason:
// MLOperator.init() trains only when no model is registered under the pair, so a
// stable pair would have trained on the first launch and silently recorded
// nothing on the second. Operator Lib v1.4.0's train_once() removed that
// constraint by making the training pass something a caller can ask for, and
// train.py asks. The per-launch pair also left one empty MLflow experiment behind
// per launch, from init()'s own set_experiment(model_id); one per repository is
// the remainder.
//
// The pair still cannot collide with a deployed operator's, whose ids are a
// flow-engine pipeline id and a real operator id, so no run started here can move
// a deployed operator's alias.
func (s *Service) pipelineID(req Request) string {
	return sanitiseSegment(s.opts.ExperimentPrefix + "-" + usernameOf(req))
}

// operatorID is the repository the run trains, so two repositories of one
// developer keep separate model histories.
func operatorID(repository string) string { return sanitiseSegment(repository) }

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
			RayURL:       s.opts.RayClientURL,
			TsWrapperURL: s.opts.TimescaleWrapperURL,
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

// asPipeTopics restates the input topics in the shape lib/access reads.
//
// The two types are the same fields under the same JSON names -- both exist to be
// marshalled into Operator Lib's CONFIG -- but the shared check is written
// against the flow engine's, because that is the service the rule came from.
// Converting here rather than aliasing the type keeps the doc comment on
// InputTopic, which records that these names are Operator Lib's rather than this
// repository's convention.
func asPipeTopics(topics []InputTopic) []pipe.InputTopic {
	out := make([]pipe.InputTopic, 0, len(topics))
	for _, topic := range topics {
		out = append(out, pipe.InputTopic{
			Name:        topic.Name,
			FilterType:  topic.FilterType,
			FilterValue: topic.FilterValue,
		})
	}
	return out
}
