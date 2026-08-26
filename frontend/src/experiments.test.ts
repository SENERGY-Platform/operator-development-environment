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

// jsdom because `router.tsx` reads `window.location` and subscribes to popstate
// when it loads, and the module under test imports it. Nothing below renders.
// @vitest-environment jsdom

import { expect, it } from "vitest";
import { ApiError, type EvaluationCriterion, type Experiment, type ExperimentStatus } from "./api";
import {
  canStop,
  criterionVerdict,
  framingVerdict,
  hasUnfinished,
  isFinished,
  isStaleProposal,
  launchRefusal,
  statusTone,
} from "./experiments";

/**
 * The decisions in the experiments pane that are logic rather than layout.
 *
 * Four of them, and each is a place where getting it wrong produces a screen that
 * looks fine: a criterion that could not be evaluated drawn as one that failed, a
 * poll that keeps asking Ray about jobs that ended, a 409 that means "re-read this"
 * printed as an error, and a framing verdict that treats "ODE could not tell" as
 * "framing is refused". None of them would show up as a crash, which is why they
 * are here.
 *
 * The iframe probe itself is not tested. Whether a cross-origin page renders inside
 * a frame is a thing only a real browser decides, and a jsdom iframe would answer a
 * question nobody asked.
 *
 * Fixtures are built here rather than taken from `__contract__`, for the reason
 * `routes.test.tsx` gives: those files pin the wire shape against a running
 * backend, and bending one to produce a FAILED run would weaken what they are for.
 */

// --- which runs are still moving ---

const ALL: ExperimentStatus[] = ["PENDING", "RUNNING", "STOPPED", "SUCCEEDED", "FAILED"];

/*
 * STOPPED is the one worth naming. A developer pressed Stop, so it is tempting to
 * read it as an interruption the job might come back from — it is not: Ray will not
 * leave it, and polling for a change is polling forever.
 */
it("a run is finished exactly in the states Ray will not leave", () => {
  expect(isFinished("SUCCEEDED")).toBe(true);
  expect(isFinished("FAILED")).toBe(true);
  expect(isFinished("STOPPED")).toBe(true);
  expect(isFinished("PENDING")).toBe(false);
  expect(isFinished("RUNNING")).toBe(false);
});

it("every status Ray reports is classified, in one direction or the other", () => {
  for (const status of ALL) {
    expect(typeof isFinished(status), status).toBe("boolean");
    expect(["running", "good", "bad", "neutral"]).toContain(statusTone(status));
  }
});

// A failed run and a stopped one are both finished and must not read the same: one
// is the cluster's answer and one is the developer's.
it("a failure and a deliberate stop are drawn differently", () => {
  expect(statusTone("FAILED")).toBe("bad");
  expect(statusTone("STOPPED")).toBe("neutral");
  expect(statusTone("SUCCEEDED")).toBe("good");
});

function run(status: ExperimentStatus): Experiment {
  return {
    experiment_id: `e-${status}`,
    submission_id: "s-1",
    mlflow_run_id: "run-1",
    mlflow_experiment_id: "1",
    mlflow_experiment_name: "ode/dev/example",
    repository: "dev/example",
    commit_sha: "2057193e0dd8968f1981550931dc54fab66b74a3",
    entrypoint: "python training.py",
    package_uri: "gcs://_ray_pkg_x.zip",
    package_bytes: 1024,
    package_reused: false,
    status,
    scoped_credential: false,
    submitted_at: "2026-08-24T08:14:09Z",
    updated_at: "2026-08-24T08:14:09Z",
  };
}

/*
 * The polling predicate. The listing route refreshes a status from Ray only for the
 * runs that have not finished, so a page of finished runs is a request whose answer
 * cannot have changed — asked once every interval, per developer, forever.
 */
it("a list with nothing unfinished in it is not worth polling", () => {
  expect(hasUnfinished([])).toBe(false);
  expect(hasUnfinished([run("SUCCEEDED"), run("FAILED"), run("STOPPED")])).toBe(false);
});

it("one unfinished run among finished ones keeps the poll alive", () => {
  expect(hasUnfinished([run("SUCCEEDED"), run("RUNNING")])).toBe(true);
  expect(hasUnfinished([run("PENDING")])).toBe(true);
});

// PENDING is a submission Ray has accepted and not started. Stopping one is what a
// developer does when they realise they launched from the wrong commit, so it has
// to be offered before the job is running rather than only after.
it("a run can be stopped while it is pending or running, and not once it has settled", () => {
  expect(canStop("PENDING")).toBe(true);
  expect(canStop("RUNNING")).toBe(true);
  expect(canStop("SUCCEEDED")).toBe(false);
  expect(canStop("FAILED")).toBe(false);
  expect(canStop("STOPPED")).toBe(false);
});

// --- the three states of a criterion (D24) ---

function criterion(met: EvaluationCriterion["met"]): EvaluationCriterion {
  return {
    metric: "rmse",
    threshold: 0.35,
    value: 0.31,
    met,
    goal: "minimise",
    goal_stated: true,
    lower_is_better: true,
    source: "evaluation.yaml at 2057193",
  };
}

/*
 * The defect this exists to prevent, stated as a test: a criterion that could not be
 * evaluated is not a criterion that failed. `!met` is true for `false` and false for
 * the object, so a pane written with a ternary gets this right by accident and wrong
 * as soon as someone inverts it — and the wrong version renders a red cross against a
 * target nothing compared.
 */
it("a criterion that could not be evaluated is not the same answer as one that was missed", () => {
  const missed = criterionVerdict(criterion(false));
  const unknown = criterionVerdict(
    criterion({
      status: "not_computed",
      reason: "metric_not_reported",
      detail: "the run logged no mape; it logged r2, rmse",
    }),
  );

  expect(missed).toBe("missed");
  expect(unknown).not.toBe("missed");
  expect(unknown).not.toBe("met");
});

it("a met criterion and a missed one are the two string verdicts", () => {
  expect(criterionVerdict(criterion(true))).toBe("met");
  expect(criterionVerdict(criterion(false))).toBe("missed");
});

/*
 * The reason travels with the non-result, because each of the seven names a
 * different repair — a missing `evaluation.yaml` is the developer writing one, and
 * `no_developer_credential` is not a fault at all. Collapsing them to "unknown"
 * would leave the developer with nothing to do about it.
 */
it("a non-result keeps its reason and its detail rather than becoming a bare unknown", () => {
  const verdict = criterionVerdict(
    criterion({
      status: "not_computed",
      reason: "no_criteria_file",
      detail: "the repository has no evaluation.yaml at this commit",
    }),
  );

  expect(typeof verdict).not.toBe("string");
  if (typeof verdict === "string") throw new Error("expected the non-result object");
  expect(verdict.status).toBe("not_computed");
  expect(verdict.reason).toBe("no_criteria_file");
  expect(verdict.detail).toContain("evaluation.yaml");
});

// --- the launch refusals ---

function refusal(status: number, body: Record<string, unknown>): ApiError {
  return new ApiError(status, String(body.error ?? "refused"), body);
}

/*
 * A dirty working copy is a step the developer takes, not an error. The paths come
 * back with it so the card can list them, which is the difference between "commit
 * your work first" and a sentence they have to go and interpret.
 */
it("an uncommitted working copy is recognised as a commit step with its paths", () => {
  const found = launchRefusal(
    refusal(409, {
      error: "the working copy has uncommitted changes",
      needs: "commit",
      hint: "commit the working copy and launch again",
      uncommitted: ["training.py", "evaluation.yaml"],
      uncommitted_elided: true,
    }),
  );

  expect(found?.needs).toBe("commit");
  expect(found?.paths).toEqual(["training.py", "evaluation.yaml"]);
  expect(found?.elided).toBe(true);
  expect(found?.hint).toContain("commit the working copy");
});

it("an oversized package carries the size and the cap it was measured against", () => {
  const found = launchRefusal(
    refusal(409, { error: "package too large", needs: "smaller_package", bytes: 300, limit: 200 }),
  );

  expect(found?.needs).toBe("smaller_package");
  expect(found?.size).toEqual({ bytes: 300, limit: 200 });
});

/*
 * A launch reaches the repo surface on its way to the cluster, so it can answer with
 * any of that surface's `needs`. Each one is a different sentence to the developer,
 * and a mismatched checkout in particular must not read as "commit your work" — the
 * commit would go somewhere they did not ask for.
 */
it("a refusal from the repository surface is recognised by its own needs", () => {
  expect(launchRefusal(refusal(409, { needs: "remote_match" }))?.needs).toBe("remote_match");
  expect(launchRefusal(refusal(409, { needs: "github_connection" }))?.needs).toBe(
    "github_connection",
  );
});

/*
 * Everything else stays an error. A 502 from Ray names nothing the developer can
 * press, and dressing it up as a next step would send them to the workspace to fix
 * something that is not wrong.
 */
it("a failure with nothing to press is left as an error", () => {
  expect(launchRefusal(refusal(502, { error: "ray refused the submission" }))).toBeNull();
  expect(launchRefusal(refusal(409, { error: "conflict with no needs" }))).toBeNull();
  expect(launchRefusal(new Error("network down"))).toBeNull();
  expect(launchRefusal(null)).toBeNull();
});

// A 409 whose body did not survive is still not a next step: without `needs` there
// is no card to render, and inventing one would guess at what the developer should do.
it("a 409 whose body carried no needs is not turned into a step", () => {
  expect(launchRefusal(new ApiError(409, "conflict"))).toBeNull();
});

// --- the stale proposal (§5.13) ---

/*
 * The 409 exists so that a developer cannot record agreement with something they
 * never read: the run was interpreted again and proposed something else. Reported as
 * a generic error it would leave them pressing Accept on stale text, which is the
 * exact outcome the status code was chosen to prevent.
 */
it("a stale proposal is recognised so the pane can re-read rather than report", () => {
  expect(
    isStaleProposal(
      new ApiError(409, "the proposal is no longer the one that stands", {
        needs: "reread",
        hint: "read the interpretation and decide on the proposal that stands now",
      }),
    ),
  ).toBe(true);
});

// The other 409s on the way to this route are not this one. Treating a dirty working
// copy as a stale proposal would silently re-read an interpretation and drop the
// developer's decision on the floor.
it("another conflict is not mistaken for a stale proposal", () => {
  expect(isStaleProposal(new ApiError(409, "uncommitted changes", { needs: "commit" }))).toBe(false);
  expect(isStaleProposal(new ApiError(400, "malformed", { needs: "reread" }))).toBe(false);
  expect(isStaleProposal(new ApiError(409, "conflict"))).toBe(false);
  expect(isStaleProposal(new Error("network down"))).toBe(false);
});

// --- the two halves of the embed probe (D6) ---

/*
 * "unknown" is a real answer and not a refusal. ODE probes from inside the cluster
 * and the browser is outside it, so a service ODE cannot reach may frame perfectly —
 * which is why the pane tries anyway and lets the browser settle it.
 */
it("a service ODE could not judge is still given to the browser to try", () => {
  expect(framingVerdict("unknown", "probing")).toBe("probing");
  expect(framingVerdict("unknown", "loaded")).toBe("ok");
  expect(framingVerdict("unknown", "timeout")).toBe("refused");
});

/*
 * A header the backend read is definitive in one direction only. `X-Frame-Options:
 * DENY` is a refusal a browser reports as a *load* — of the error page — so the
 * iframe cannot overturn it, and believing the iframe there would leave an empty
 * frame on screen with no link beside it.
 */
it("a header that refuses framing is not overturned by the frame appearing to load", () => {
  expect(framingVerdict("no", "loaded")).toBe("refused");
  expect(framingVerdict("no", "probing")).toBe("refused");
  expect(framingVerdict("no", "timeout")).toBe("refused");
});

// A permissive header is not a promise that the page renders: it may still refuse in
// its own script, or never arrive. The browser has the last word in that direction.
it("a permissive header still waits for the browser before embedding", () => {
  expect(framingVerdict("yes", "probing")).toBe("probing");
  expect(framingVerdict("yes", "loaded")).toBe("ok");
  expect(framingVerdict("yes", "timeout")).toBe("refused");
});
