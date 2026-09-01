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

package interpret

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// The message ODE injects, and how the proposal is read back out of the reply.

// ProposalMarker is the line an interpretation ends with.
//
// A marker rather than a tool call, deliberately. §5.8's tool table is an
// allow-list published as a paper table, and adding a nineteenth row so the
// assistant could hand back a structured proposal would be a change to the
// specification rather than an implementation of it. A marker keeps the proposal
// in the conversation where the developer reads it, costs no new capability, and
// fails visibly: a reply without one produces an explicit "no proposal stated"
// rather than an empty string that would read as "nothing to change".
const ProposalMarker = "NEXT STEP:"

// injectedMessage composes what the assistant is asked to read.
//
// The summary goes in as JSON rather than as prose because it is a document with a
// schema (§5.13) and every field of it carries a distinction that prose would
// blur — `met` as an object rather than `false`, an empty comparison meaning "first
// run" rather than "no change", `not_computed` with a reason. Flattening it to
// English here would be ODE interpreting the run, which is the model's job in this
// turn and not the backend's.
//
// What the message says about logs is bounded to this context on purpose. It used
// to read "there are no logs and there is no tool that would fetch them", which is
// false about the system: Ray keeps the driver output, `GET /experiments/:id/logs`
// serves it and the developer's own pane shows it. §5.13 keeps logs out of a
// model's context, not out of existence — and a model told the stronger thing
// repeated it to the developer, who had the logs open at the time.
func injectedMessage(summary experiments.Summary, record experiments.Experiment) string {
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		encoded = []byte(fmt.Sprintf("{\"error\":%q}", err.Error()))
	}

	builder := &strings.Builder{}
	fmt.Fprintf(builder,
		"A training run you launched from this conversation has finished.\n\n"+
			"This is ODE's structured summary of it (§5.13). It is the whole of what "+
			"you get about this run: params, metrics, tags, the comparison against the "+
			"previous run and the developer's own evaluation criteria. **No line of its log "+
			"output is part of it** — a failed run's last exception is extracted into "+
			"`failure`, and nothing else from the output travels, reaches you or could be "+
			"fetched by any tool of yours. The developer has it, on a route of their own — "+
			"so where this document does not explain what happened, say that the log would "+
			"and leave the reading of it to them. Do not tell them the run produced none.\n\n"+
			"```json\n%s\n```\n\n", encoded)

	builder.WriteString(failureGuidance(summary))

	builder.WriteString(
		"Read it and answer the developer with:\n\n" +
			"1. **What the numbers say.** Compare against the previous run where there is " +
			"one, and against their evaluation criteria. Where a field is an object with " +
			"`\"status\": \"not_computed\"`, that is a fact that could not be determined — " +
			"say so and say what would determine it. It is not a failure and must not be " +
			"reported as one.\n" +
			"2. **One concrete next adjustment**, on the last line, beginning `" +
			ProposalMarker + "`. Concrete means something they could act on without asking " +
			"you what you meant: a parameter and a value, a metric to start logging, a " +
			"different window. One, not a list.\n\n")

	fmt.Fprintf(builder,
		"It is a proposal. The developer accepts it, edits it or rejects it, and it "+
			"changes nothing until they do. You cannot change %s and there is no tool that "+
			"could (§5.8) — if the criteria themselves look wrong, say so and let them "+
			"decide.\n\n", experiments.EvaluationCriteriaPath)

	fmt.Fprintf(builder, "Experiment %s, MLflow run %s, commit %s.",
		record.ID, summary.RunID, shortSHA(summary.CommitSHA))
	return builder.String()
}

// failureGuidance is what to say about the `failure` block, or nothing.
//
// It is a paragraph of its own rather than part of the standing text because a run
// that succeeded has no failure block, and a general instruction about a field that
// is not there is how a model comes to describe one that does not exist. For a
// failed run it is the *only* part of the summary with anything in it: no metrics,
// criteria that could not be evaluated, an empty comparison. So the turn's whole
// question changes, and the message says so instead of asking for a reading of
// numbers that are not there.
func failureGuidance(summary experiments.Summary) string {
	failure := summary.Failure
	if failure == nil {
		return ""
	}

	if failure.NotDiagnosed != nil {
		return fmt.Sprintf("This run **failed and left no readable exception** "+
			"(`%s`: %s). So there is no cause in this document, and there is no cause "+
			"available to you at all: say that plainly, say that the job's whole output "+
			"is in the developer's own log pane where you cannot read it and they can, "+
			"and make your next step something that would produce the missing evidence "+
			"— not a guess at what failed.\n\n",
			failure.NotDiagnosed.Reason, failure.NotDiagnosed.Detail)
	}

	builder := &strings.Builder{}
	builder.WriteString("This run **failed**, and `failure` carries its last exception: " +
		"the class, the innermost frames of the traceback, and the message. That is what " +
		"there is to interpret — a failed run logs few metrics or none, and its criteria " +
		"could not be evaluated, so read the exception rather than the empty numbers.\n\n")

	if failure.MaskedLiterals > 0 {
		fmt.Fprintf(builder, "The message has %d literal(s) replaced by `%s`, because "+
			"exposure tier %s does not expose values and a value in a traceback is a value "+
			"(§3.2). Read `%s` as \"a value was withheld here\" — do not guess what it was, "+
			"and do not report the placeholder as the text the exception carried. The "+
			"developer sees the message as it was raised, in their own pane.\n\n",
			failure.MaskedLiterals, "[value]", failure.MaskedFor, "[value]")
	}

	builder.WriteString("The frames are the developer's own files and line numbers. Name " +
		"them: a next step that points at a line is one they can act on, and the file " +
		"the exception was raised in is the one thing here that says where to look.\n\n")
	return builder.String()
}

// extractProposal reads the next step out of the assistant's reply.
//
// The last marked line wins, because a model that restates its proposal at the end
// means the restatement. Leading markdown is tolerated — a bulleted or bolded
// marker is the same marker — and everything else produces an explicit non-result
// rather than a guess at which sentence was meant.
func extractProposal(experimentID, reply string) Proposal {
	if strings.TrimSpace(reply) == "" {
		return unstatedProposal(ReasonNoProposal,
			"the assistant's reply was empty, so it named no next step")
	}

	found := ""
	for _, line := range strings.Split(reply, "\n") {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimLeft(trimmed, "-*# \t")
		trimmed = strings.TrimSpace(trimmed)
		upper := strings.ToUpper(trimmed)
		if !strings.HasPrefix(upper, ProposalMarker) {
			continue
		}
		text := strings.TrimSpace(trimmed[len(ProposalMarker):])
		// Trailing emphasis from a bolded marker, so `**NEXT STEP:** widen…` does not
		// carry the asterisks into the record the developer decides on.
		text = strings.TrimSpace(strings.Trim(text, "*_`"))
		if text != "" {
			found = text
		}
	}

	if found == "" {
		return unstatedProposal(ReasonNoProposal,
			"the assistant read the run and named no next step on a line beginning "+
				ProposalMarker+", so there is nothing here to accept, edit or reject")
	}
	return Proposal{ID: proposalID(experimentID, found), Text: found}
}

// shortSHA is the seven characters a human reads a commit by. Local rather than
// imported: pkg/experiments keeps its own unexported, and one small function is a
// better answer than widening that package's surface for a log line.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
