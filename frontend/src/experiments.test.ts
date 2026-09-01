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
  hasUnfinished,
  launchedExperimentId,
  launchedRows,
  liveLabel,
  mlflowRunUrl,
  rayJobUrl,
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
 * printed as an error, a deep link built from a base and an id that do not quite
 * join, and a launch card that repeats a run it has already listed. None of them
 * would show up as a crash, which is why they are here.
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

// --- the links that replaced the frames (D6) ---

/*
 * A base with a trailing slash is a configuration nobody notices setting, and it is
 * the one input that turns a working link into `//#/jobs/...`. Both builders take
 * the base exactly as the deployment wrote it.
 */
it("a base with a trailing slash does not double it", () => {
  expect(rayJobUrl("https://ray.example.org/", "sub-1")).toBe("https://ray.example.org/#/jobs/sub-1");
  expect(mlflowRunUrl("https://mlflow.example.org/", "12", "run-1")).toBe(
    "https://mlflow.example.org/#/experiments/12/runs/run-1",
  );
});

/*
 * A launch that failed before ODE created the MLflow run has no run to open, and a
 * deployment that configured neither UI has nowhere to open anything. Both are null
 * rather than a link into a URL that is missing half its path: a link that lands on
 * a 404 reads as the run being gone.
 */
it("a missing base or a missing id is no link at all", () => {
  expect(rayJobUrl(undefined, "sub-1")).toBeNull();
  expect(rayJobUrl("https://ray.example.org", "")).toBeNull();
  expect(mlflowRunUrl("https://mlflow.example.org", "12", "")).toBeNull();
  expect(mlflowRunUrl("https://mlflow.example.org", "", "run-1")).toBeNull();
  expect(mlflowRunUrl(undefined, "12", "run-1")).toBeNull();
});

// Ray's own ids are opaque and MLflow's run ids are hex, but neither is guaranteed
// to be URL-safe by anything ODE enforces, and an id that is not escaped ends the
// path early.
it("an id is escaped into the path", () => {
  expect(rayJobUrl("https://ray.example.org", "sub/1?x")).toBe(
    "https://ray.example.org/#/jobs/sub%2F1%3Fx",
  );
});


// --- what a launch shows in the conversation ---

/*
 * The card's own run stays whatever became of it.
 *
 * It is the answer to "what did that call start", and a finished run dropping out
 * of the card that launched it would leave the call with nothing under it at
 * exactly the moment the developer came back to see how it went.
 */
it("the run a launch produced is listed even after it has finished", () => {
  const finished = { ...run("SUCCEEDED"), experiment_id: "mine" };
  expect(launchedRows([finished], "mine").map((entry) => entry.experiment_id)).toEqual(["mine"]);
});

/*
 * Everything else earns its place by being unfinished. A conversation with four
 * launches would otherwise carry every one of its runs under every one of its
 * calls, four times over, and the Experiments pane is where a history belongs.
 */
it("another run is listed only while it is still going", () => {
  const mine = { ...run("RUNNING"), experiment_id: "mine" };
  const going = { ...run("PENDING"), experiment_id: "other-going" };
  const done = { ...run("FAILED"), experiment_id: "other-done" };
  expect(launchedRows([mine, going, done], "mine").map((entry) => entry.experiment_id)).toEqual([
    "mine",
    "other-going",
  ]);
});

/*
 * And the card's own run is not listed twice while it is the one still running,
 * which is the state every card is in for the first minutes of its life.
 */
it("a running card does not list its own run twice", () => {
  const mine = { ...run("RUNNING"), experiment_id: "mine" };
  expect(launchedRows([mine], "mine")).toHaveLength(1);
});

/*
 * A launch whose result carried no experiment id — nothing does this today, but the
 * card reads a tool result rather than a typed record — still shows what is running
 * rather than nothing at all.
 */
it("a launch with no id of its own still shows what is running", () => {
  const going = { ...run("RUNNING"), experiment_id: "other" };
  const done = { ...run("SUCCEEDED"), experiment_id: "old" };
  expect(launchedRows([going, done], null).map((entry) => entry.experiment_id)).toEqual(["other"]);
});

/*
 * The three words the conversation uses. PENDING is "running" on purpose: a job Ray
 * has queued and not yet started is one the developer is waiting for, and the
 * distinction between queued and started is the dashboard's business — the badge
 * carries Ray's own word in its title for whoever is going there.
 */
it("a queued job reads as running, and a failure reads as an error", () => {
  expect(liveLabel("PENDING")).toBe("running");
  expect(liveLabel("RUNNING")).toBe("running");
  expect(liveLabel("SUCCEEDED")).toBe("finished");
  expect(liveLabel("FAILED")).toBe("error");
  expect(liveLabel("STOPPED")).toBe("stopped");
});


/*
 * The shape the CLI provider stores, and the defect it caused.
 *
 * That provider runs its own tool loop over MCP (§5.7) and echoes the client's
 * result back verbatim, so the transcript holds MCP's envelope — a list of content
 * blocks whose text is the JSON — rather than the object ODE's own dispatcher
 * returns. Reading only the object left the card with no run of its own, which is
 * how a launch that had plainly succeeded came to say nothing was on the cluster.
 */
it("a launch result reaches the card whether it is the object or MCP's envelope", () => {
  expect(launchedExperimentId({ experiment_id: "e-1" })).toBe("e-1");
  expect(
    launchedExperimentId([{ type: "text", text: JSON.stringify({ experiment_id: "e-1" }) }]),
  ).toBe("e-1");
  // Stored as the raw string, which is what `replay` keeps when a result did not
  // parse on the way in.
  expect(launchedExperimentId(JSON.stringify({ experiment_id: "e-1" }))).toBe("e-1");
});

/* And anything else is no id rather than a wrong one. */
it("a result with no experiment in it yields no id", () => {
  expect(launchedExperimentId(null)).toBeNull();
  expect(launchedExperimentId("the job could not be submitted")).toBeNull();
  expect(launchedExperimentId([{ type: "text", text: "not json" }])).toBeNull();
  expect(launchedExperimentId([{ type: "image" }])).toBeNull();
  expect(launchedExperimentId({ experiment_id: 7 })).toBeNull();
});
