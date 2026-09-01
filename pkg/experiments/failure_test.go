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

package experiments_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"
)

// A Ray driver log of the shape this reads in production: the job's own prints, a
// chained traceback, and the exception that actually stopped it at the end.
//
// The value in the message is the case D34 exists to bound — `24,7 kWh` is a cell
// out of the developer's own series, and pandas puts it in the text.
const failedJobLog = `2026-09-01 03:14:07,001 INFO job_manager.py:531 -- Runtime env is setting up.
loading 43200 rows from urn:infai:ses:export:9f2c1b7e-4a55-4a4e-9d1e-2b8d0f6a1c33
resampling to 15min
Traceback (most recent call last):
  File "/tmp/ray/session_2026-09-01_03-14-02_1/runtime_resources/working_dir_files/_ray_pkg_9c1/train.py", line 61, in <module>
    frame = load_frame(cfg)
  File "/tmp/ray/session_2026-09-01_03-14-02_1/runtime_resources/working_dir_files/_ray_pkg_9c1/prep.py", line 18, in load_frame
    return frame.astype(float)
ValueError: could not convert string to float: '24,7 kWh'

During handling of the above exception, another exception occurred:

Traceback (most recent call last):
  File "/tmp/ray/session_2026-09-01_03-14-02_1/runtime_resources/working_dir_files/_ray_pkg_9c1/train.py", line 74, in <module>
    train_once(cfg)
  File "/tmp/ray/session_2026-09-01_03-14-02_1/runtime_resources/working_dir_files/_ray_pkg_9c1/train.py", line 39, in train_once
    model.fit(X, y)
  File "/opt/conda/lib/python3.11/site-packages/sklearn/base.py", line 1145, in wrapper
    return fit_method(estimator, *args, **kwargs)
ValueError: Input X contains NaN in column 'power_kw' at 3 of 43200 rows
2026-09-01 03:14:31,884 ERROR job_supervisor.py:196 -- Job entrypoint command failed with exit code 1
`

// failRun makes a run terminal the way a job that raised does: Ray reports the
// process as failed and the log carries the traceback.
func failRun(t *testing.T, h *harness, launched experiments.LaunchResult, logs string) {
	t.Helper()
	h.ray.SetLogs(launched.SubmissionID, logs)
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusFailed)
}

func summaryOf(t *testing.T, h *harness, id string) experiments.Summary {
	t.Helper()
	summary, err := h.service.Results(context.Background(), h.request(), id)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	return summary
}

// A failed run says what stopped it, and where — which is what its metrics cannot.
func TestAFailedRunCarriesItsLastExceptionAndTheInnermostFrames(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	failRun(t, h, launched, failedJobLog)

	failure := summaryOf(t, h, launched.ID).Failure
	if failure == nil {
		t.Fatal("a failed run carries no failure at all, so the model is left with " +
			"a status and no metrics")
	}
	if failure.NotDiagnosed != nil {
		t.Fatalf("not_diagnosed = %+v, want the exception the log carries", failure.NotDiagnosed)
	}

	// The *last* exception: the chained one is what stopped the job, and the first is
	// what it was handling when it did.
	if failure.Exception != "ValueError" {
		t.Errorf("exception = %q, want the class of the last traceback", failure.Exception)
	}
	if !strings.Contains(failure.Message, "Input X contains NaN") {
		t.Errorf("message = %q, want the message of the last traceback", failure.Message)
	}

	// Three frames, innermost last, named the way the developer names their files.
	if len(failure.Frames) != 3 {
		t.Fatalf("frames = %v, want the innermost three", failure.Frames)
	}
	if got := failure.Frames[2].String(); got != "base.py:1145 in wrapper" {
		t.Errorf("innermost frame = %q", got)
	}
	if got := failure.Frames[0].String(); got != "train.py:74 in <module>" {
		t.Errorf("outermost kept frame = %q", got)
	}
	// The path Ray unpacked the job to is not in any of them: it is a session
	// directory and a package hash, and no reader has a use for either.
	for _, frame := range failure.Frames {
		if strings.Contains(frame.File, "/") || strings.Contains(frame.File, "runtime_resources") {
			t.Errorf("frame file = %q, want the developer's own file name", frame.File)
		}
	}

	// And nothing else from the log travels. Not the prints, not Ray's own lines.
	encoded, _ := json.Marshal(failure)
	for _, forbidden := range []string{
		"job_manager", "job_supervisor", "loading", "resampling",
		"urn:infai:ses:export", "exit code 1", "Traceback",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the extract carries %q from the log: %s", forbidden, encoded)
		}
	}
}

// §3.2 applied to the one field of a summary that can carry a value.
func TestTheExceptionMessageIsMaskedBelowL2AndVerbatimAtL2(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	failRun(t, h, launched, failedJobLog)
	summary := summaryOf(t, h, launched.ID)

	// As built, and as the developer's own route serves it: the message as raised.
	if !strings.Contains(summary.Failure.Message, "43200") {
		t.Errorf("the developer's own summary is masked: %q", summary.Failure.Message)
	}
	if summary.Failure.MaskedFor != "" {
		t.Errorf("masked_for_tier = %q on the raw extract, which would read as masked",
			summary.Failure.MaskedFor)
	}

	for _, tier := range []exposure.Tier{exposure.L0, exposure.L1} {
		masked := summary.MaskedFor(tier).Failure
		if masked.MaskedFor != tier.String() {
			t.Errorf("masked_for_tier = %q, want %s", masked.MaskedFor, tier)
		}
		// The words survive, so the model can say what went wrong.
		if !strings.Contains(masked.Message, "Input X contains NaN") {
			t.Errorf("%s message = %q, want the words of the exception", tier, masked.Message)
		}
		// The values do not: a cell out of the developer's series and a row count are
		// both values, and §3.2 puts values at L2.
		for _, value := range []string{"43200", "power_kw", "'", "3 of"} {
			if strings.Contains(masked.Message, value) {
				t.Errorf("%s message = %q, want %q withheld", tier, masked.Message, value)
			}
		}
		if masked.MaskedLiterals != 3 {
			t.Errorf("%s masked_literals = %d, want the three literals counted so the "+
				"model can tell a placeholder from prose", tier, masked.MaskedLiterals)
		}
		// The class and the frames are code, not data, and are never masked: a
		// failure whose location was withheld would be worth nothing.
		if masked.Exception != "ValueError" || len(masked.Frames) != 3 {
			t.Errorf("%s withheld the class or the frames: %+v", tier, masked)
		}
		// And the summary's own numbers are untouched by any of this.
		if summary.Failure.MaskedFor != "" {
			t.Error("MaskedFor mutated the summary it was called on, so the developer's " +
				"route would serve whatever the last model read")
		}
	}

	verbatim := summary.MaskedFor(exposure.L2).Failure
	if !strings.Contains(verbatim.Message, "'power_kw'") ||
		!strings.Contains(verbatim.Message, "43200") {
		t.Errorf("L2 message = %q, want it as raised: L2 already exposes values",
			verbatim.Message)
	}
	if verbatim.MaskedLiterals != 0 {
		t.Errorf("L2 masked_literals = %d, want nothing masked", verbatim.MaskedLiterals)
	}
}

// A job that echoes its environment prints the credential §5.12 minted for it, and
// no tier makes that something to put in a stored conversation.
func TestACredentialInAnExceptionIsWithheldAtEveryTier(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	const token = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZXYifQ.c2lnbmF0dXJl"
	failRun(t, h, launched, "Traceback (most recent call last):\n"+
		`  File "train.py", line 3, in <module>`+"\n"+
		"    read(token)\n"+
		"RuntimeError: 401 from timescale-wrapper for Bearer "+token+"\n")
	summary := summaryOf(t, h, launched.ID)

	for _, tier := range exposure.Tiers() {
		message := summary.MaskedFor(tier).Failure.Message
		if strings.Contains(message, token) || strings.Contains(message, "eyJ") {
			t.Errorf("%s message = %q, want the credential withheld", tier, message)
		}
		if !strings.Contains(message, "[credential]") {
			t.Errorf("%s message = %q, want it to say a credential was withheld rather "+
				"than reading as a truncated message", tier, message)
		}
	}
}

// A job the cluster killed leaves no traceback, and "no exception" must not read as
// "no reason" (D24).
func TestAFailedRunWithoutATracebackSaysSoRatherThanNothing(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	failRun(t, h, launched,
		"loading 43200 rows\n(raylet) node ran out of memory, killing worker\n")

	failure := summaryOf(t, h, launched.ID).Failure
	if failure == nil || failure.NotDiagnosed == nil {
		t.Fatalf("failure = %+v, want an explicit non-result naming the reason", failure)
	}
	if failure.NotDiagnosed.Reason != experiments.ReasonNoTraceback {
		t.Errorf("reason = %q, want %q", failure.NotDiagnosed.Reason,
			experiments.ReasonNoTraceback)
	}
	if failure.Diagnosed() {
		t.Error("a failure with no exception reports itself as diagnosed")
	}
	// The log's own lines are not the detail: the reason is ODE's prose, and the
	// output stays on the developer's route.
	if strings.Contains(failure.NotDiagnosed.Detail, "43200") ||
		strings.Contains(failure.NotDiagnosed.Detail, "raylet") {
		t.Errorf("detail = %q, want no log content in it", failure.NotDiagnosed.Detail)
	}
}

// Ray not answering for the output costs the extract and nothing else.
func TestAFailedRunWhoseOutputRayWillNotAnswerForStillHasItsMetrics(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.mlflow.Finish(t, launched.RunID, "FAILED", map[string]float64{"rmse": 0.9})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusFailed)
	h.ray.FailNext("/logs", http.StatusInternalServerError)

	summary := summaryOf(t, h, launched.ID)
	if summary.Metrics["rmse"] != 0.9 {
		t.Errorf("metrics = %v, want a summary built despite the log read failing",
			summary.Metrics)
	}
	if summary.Failure == nil || summary.Failure.NotDiagnosed == nil {
		t.Fatalf("failure = %+v, want the reason the extract is missing", summary.Failure)
	}
	if summary.Failure.NotDiagnosed.Reason != experiments.ReasonLogsUnavailable {
		t.Errorf("reason = %q, want %q", summary.Failure.NotDiagnosed.Reason,
			experiments.ReasonLogsUnavailable)
	}
}

// A run that worked carries no failure block at all. Absence is the statement here:
// an empty `not_diagnosed` on a successful run would read as a failure nobody could
// explain.
func TestASuccessfulRunCarriesNoFailureBlock(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	h.ray.SetLogs(launched.SubmissionID, failedJobLog)
	h.mlflow.Finish(t, launched.RunID, "FINISHED", map[string]float64{"rmse": 0.31})
	h.ray.SetStatus(launched.SubmissionID, experiments.StatusSucceeded)

	summary := summaryOf(t, h, launched.ID)
	if summary.Failure != nil {
		t.Errorf("failure = %+v on a run that succeeded", summary.Failure)
	}
	encoded, _ := json.Marshal(summary)
	if strings.Contains(string(encoded), "ValueError") {
		t.Errorf("a successful run's summary read the log anyway: %s", encoded)
	}
	// And MaskedFor is a no-op on it rather than an invented block.
	if summary.MaskedFor(exposure.L0).Failure != nil {
		t.Error("masking invented a failure block on a run that succeeded")
	}
}

// The message is bounded before it is built, so a traceback whose message is a
// dumped data frame cannot become the size of the summary.
func TestALongExceptionMessageIsCutAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.ready()
	launched := h.launch()
	failRun(t, h, launched, "Traceback (most recent call last):\n"+
		`  File "train.py", line 9, in <module>`+"\n"+
		"ValueError: "+strings.Repeat("column power_kw is not numeric. ", 60)+"\n")

	masked := summaryOf(t, h, launched.ID).MaskedFor(exposure.L0).Failure
	if !masked.Truncated {
		t.Error("a message of two thousand characters was not marked as cut")
	}
	if len(masked.Message) > 400 {
		t.Errorf("message is %d characters, want it bounded", len(masked.Message))
	}
}
