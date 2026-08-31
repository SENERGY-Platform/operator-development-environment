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

package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// ---- launch_experiment (L0, confirmed) and get_experiment_results (L0) ----
//
// The tier on both is L0 and, as with run_code, that is not an oversight in §5.8.
// Neither tool carries platform data: a launch ships the developer's own committed
// repository to a cluster, and a result is params and metrics the developer's own
// code chose to record. The exposure tier bounds what the model learns about the
// *platform's* series, and neither of these tells it anything about one.
//
// The control on launch_experiment is the confirmation, not the tier. It spends
// cluster time and it publishes a run, and Dispatch is what makes sure a developer
// said yes first — this executor is only ever reached after they did.
//
// get_experiment_results returns the compact structured summary of §5.13 and
// nothing else. There is no tool that reads a log, deliberately: §5.13 is explicit
// that logs never enter the model's context, and a tool that could fetch them would
// make that a matter of discipline rather than of design.

type launchExperimentInput struct {
	Entrypoint  string                   `json:"entrypoint"`
	EnvVars     map[string]string        `json:"env_vars"`
	RunName     string                   `json:"run_name"`
	InputTopics []experiments.InputTopic `json:"input_topics"`
}

// LaunchExperimentResult is what the model reads back after a launch.
//
// It carries the credential note verbatim, because a model that does not know a
// job's token expires with the session will tell the developer a six-hour run is
// fine.
type LaunchExperimentResult struct {
	ExperimentID string `json:"experiment_id"`
	SubmissionID string `json:"submission_id"`
	RunID        string `json:"mlflow_run_id"`
	Repository   string `json:"repository"`
	CommitSHA    string `json:"commit_sha"`
	Entrypoint   string `json:"entrypoint"`
	Status       string `json:"status"`
	// Credential is the §3.1 item 6 note: whether the job has a token of its own,
	// and what it means if it does not.
	Credential experiments.Credential `json:"credential"`
	Warnings   []string               `json:"warnings,omitempty"`
	Hint       string                 `json:"hint"`
}

func (s *surface) launchExperiment(ctx context.Context, req Request) (any, error) {
	var in launchExperimentInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}

	req.Progress("experiments", "packaging the committed repository state")
	result, err := s.deps.Experiments.Launch(ctx, experiments.LaunchRequest{
		Request: experiments.Request{
			Bearer:      req.Token,
			UserSub:     req.UserSub,
			SessionID:   req.SessionID,
			WorkbenchID: req.WorkbenchID,
		},
		Entrypoint:  in.Entrypoint,
		EnvVars:     in.EnvVars,
		RunName:     in.RunName,
		InputTopics: in.InputTopics,
	})
	if err != nil {
		return nil, err
	}

	return LaunchExperimentResult{
		ExperimentID: result.ID,
		SubmissionID: result.SubmissionID,
		RunID:        result.RunID,
		Repository:   result.Repository,
		CommitSHA:    result.CommitSHA,
		Entrypoint:   result.Entrypoint,
		Status:       result.Status,
		Credential:   result.Credential,
		Warnings:     result.Warnings,
		Hint: "the job is queued; read it back with get_experiment_results, which " +
			"answers with a snapshot while it is still running and with the result once " +
			"it has finished",
	}, nil
}

type experimentResultsInput struct {
	ExperimentID string `json:"experiment_id"`
}

// ExperimentListing is what the model gets when it asks for results without
// naming an experiment: enough to choose one, and no metrics.
//
// It exists so that "how did the last run go" is one call rather than a refusal.
// A model that has just launched something knows the id; a model resuming a
// conversation does not, and the alternative is asking the developer to read one
// out of the UI.
type ExperimentListing struct {
	Experiments []ExperimentBrief `json:"experiments"`
	Hint        string            `json:"hint"`
}

// ExperimentBrief is one row of that listing.
type ExperimentBrief struct {
	ExperimentID string `json:"experiment_id"`
	Repository   string `json:"repository"`
	CommitSHA    string `json:"commit_sha"`
	Entrypoint   string `json:"entrypoint"`
	Status       string `json:"status"`
	SubmittedAt  string `json:"submitted_at"`
}

// experimentListLimit bounds the listing. Small: this is a chooser, not a history,
// and a model that needs more than ten should be told which one it means.
const experimentListLimit = 10

func (s *surface) getExperimentResults(ctx context.Context, req Request) (any, error) {
	var in experimentResultsInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}

	request := experiments.Request{
		Bearer:    req.Token,
		UserSub:   req.UserSub,
		SessionID: req.SessionID,
	}

	if strings.TrimSpace(in.ExperimentID) == "" {
		listed, err := s.deps.Experiments.List(ctx, request, experimentListLimit)
		if err != nil {
			return nil, err
		}
		if len(listed) == 0 {
			return nil, fmt.Errorf(
				"%w: this developer has launched no experiments yet, so there are no "+
					"results to read", ErrInvalidInput)
		}
		brief := make([]ExperimentBrief, 0, len(listed))
		for _, record := range listed {
			brief = append(brief, ExperimentBrief{
				ExperimentID: record.ID,
				Repository:   record.Repository,
				CommitSHA:    record.CommitSHA,
				Entrypoint:   record.Entrypoint,
				Status:       record.Status,
				SubmittedAt:  record.SubmittedAt.Format(time.RFC3339),
			})
		}
		return ExperimentListing{
			Experiments: brief,
			Hint: "call get_experiment_results again with one of these experiment_id " +
				"values to read its params, metrics and comparison to the previous run",
		}, nil
	}

	req.Progress("experiments", "reading the run from MLflow")
	summary, err := s.deps.Experiments.Results(ctx, request, in.ExperimentID)
	if err != nil {
		return nil, err
	}
	return summary, nil
}
