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

// @vitest-environment jsdom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { EvaluationCriterion, Experiment, ExperimentFailure, Session } from "./api";

/**
 * The two claims the run document makes that are wrong in a way nothing crashes on.
 *
 * A criterion has three states and they must reach the screen as three; an empty
 * comparison is a first run and not a run that changed nothing. Both are assertions
 * about *rendered output*, so they are made against a mounted pane rather than
 * against the helpers underneath it — the helpers are already covered in
 * `experiments.test.ts`, and what is left to get wrong is the last step.
 *
 * Mounted the way `routes.test.tsx` mounts the application: only the process
 * boundaries are faked.
 *
 * The launch card in the conversation is here too, for the same reason: what it
 * gets wrong is a run that stays "running" on screen after Ray has finished it, and
 * that is a claim about a poll and a re-render rather than about a helper.
 */

vi.mock("./keycloak", () => ({
  initKeycloak: vi.fn(async () => true),
  token: vi.fn(async () => "test-token"),
  logout: vi.fn(),
}));

/** The criteria the mocked results route answers with. Set by each test. */
let primary: EvaluationCriterion = met(true);
let secondary: EvaluationCriterion[] = [];
/** The failure block the mocked results route answers with, or none. */
let failure: ExperimentFailure | undefined;
/** The comparison the mocked results route answers with. Set by each test. */
let comparison: { metric: string; previous: number; current: number; delta: number; direction: "better" | "worse" | "unchanged"; lower_is_better: boolean }[] = [];

function met(verdict: EvaluationCriterion["met"], metric = "rmse"): EvaluationCriterion {
  return {
    metric,
    threshold: 0.35,
    value: verdict === true || verdict === false ? 0.31 : undefined,
    met: verdict,
    goal: "minimise",
    goal_stated: true,
    lower_is_better: true,
    source: "evaluation.yaml at 2057193",
  };
}

const EXPERIMENT = {
  experiment_id: "e-1",
  submission_id: "s-1",
  mlflow_run_id: "run-2",
  mlflow_experiment_id: "1",
  mlflow_experiment_name: "ode/dev/example",
  repository: "dev/example",
  commit_sha: "2057193e0dd8968f1981550931dc54fab66b74a3",
  branch: "main",
  entrypoint: "python training.py",
  package_uri: "gcs://_ray_pkg_x.zip",
  package_bytes: 7350,
  package_reused: false,
  status: "SUCCEEDED" as const,
  scoped_credential: true,
  submitted_at: "2026-08-24T08:14:09Z",
  updated_at: "2026-08-24T08:14:09Z",
  ended_at: "2026-08-24T08:15:09Z",
};

/** What the listing route answers with. Mutable: a poll that changed nothing is
 *  indistinguishable from no poll at all. */
let listing: Experiment[] = [];
/** How many times the listing was read, which is what a stopped poll is visible in. */
let listingReads = 0;
/** Records the listing does not produce, readable only by id. */
let byId: Experiment[] = [];

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      experiments: async () => {
        listingReads += 1;
        return {
          experiments: listing,
          count: listing.length,
          ray_url: "http://ray.test",
          mlflow_url: "http://mlflow.test",
        };
      },
      experiment: async (id: string) => {
        const found = [...byId, ...listing].find((run) => run.experiment_id === id);
        if (!found) throw new Error(`no such experiment: ${id}`);
        return found;
      },
      experimentResults: async () => ({
        run_id: "run-2",
        experiment_id: "e-1",
        submission_id: "s-1",
        commit_sha: EXPERIMENT.commit_sha,
        status: "SUCCEEDED",
        finished: true,
        params: {},
        metrics: {},
        tags: {},
        comparison_to_previous: comparison,
        evaluation_criteria: primary,
        secondary_criteria: secondary,
        resource_usage: { duration_s: 90 },
        failure,
      }),
      // Never settles: the interpretation is not what these two tests are about, and
      // a pending read keeps its section out of the text being asserted on.
      interpretation: () => new Promise<never>(() => {}),
    },
  };
});

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const SESSION = {
  user_id: "u-1",
  username: "dev",
  email: "dev@example.org",
  roles: ["developer"],
  is_admin: false,
  exposure_tier: "L0",
  features: {
    profiler: false,
    selection: false,
    chat: false,
    mcp: false,
    kernel: false,
    charts: false,
    relations: false,
    repo: false,
    experiments: true,
  },
  experiments: { ray_url: "http://ray.test", mlflow_url: "http://mlflow.test", scoped_job_token: true },
} as Session;

const mounted: Root[] = [];

beforeEach(() => {
  listing = [EXPERIMENT];
  listingReads = 0;
  byId = [];
  failure = undefined;
});

afterEach(async () => {
  const roots = mounted.splice(0, mounted.length);
  await act(async () => {
    for (const root of roots) root.unmount();
  });
  document.body.innerHTML = "";
});

/** open mounts the pane with a run already selected and returns its root element. */
async function open(): Promise<HTMLElement> {
  window.history.replaceState({}, "", "/tools/experiments?run=e-1");
  vi.resetModules();
  const { ExperimentsView } = await import("./experiments");

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<ExperimentsView session={SESSION} />));
  // One more turn, for the results read the document starts on mount.
  await act(async () => {
    await Promise.resolve();
  });
  return host;
}

// --- the three states of a criterion, on screen (D24) ---

/*
 * The defect in its final form. `met: false` and `met: {status: "not_computed"}` are
 * different facts, and a pane that draws them the same way tells the developer their
 * run missed a target that nothing ever compared. Asserted on the class the card
 * carries as well as on the words, because either alone can be right while the other
 * is wrong — matching words on an identically coloured card is the version that
 * looks fine in a screenshot.
 */
it("a missed criterion and one that could not be evaluated are drawn differently", async () => {
  primary = met(false);
  secondary = [
    met(
      { status: "not_computed", reason: "metric_not_reported", detail: "the run logged no mape" },
      "mape",
    ),
  ];
  comparison = [];

  const host = await open();
  const cards = [...host.querySelectorAll(".exp-criterion")];
  expect(cards).toHaveLength(2);

  expect(cards[0].className).toContain("missed");
  expect(cards[0].textContent).toContain("not met");

  expect(cards[1].className).toContain("unknown");
  expect(cards[1].className).not.toContain("missed");
  // The reason travels with it: each of the seven names a different repair.
  expect(cards[1].textContent).toContain("not computed");
  expect(cards[1].textContent).toContain("metric not reported");
  expect(cards[1].textContent).not.toContain("not met");
});

it("a met criterion is neither of the other two", async () => {
  primary = met(true);
  secondary = [];
  comparison = [];

  const host = await open();
  const card = host.querySelector(".exp-criterion");
  expect(card?.className).toContain("met");
  expect(card?.className).not.toContain("missed");
  expect(card?.className).not.toContain("unknown");
});

/*
 * A criterion whose metric the run never logged has no value, and "0" is a real
 * reading. Rendering the absence as a number would put a measurement on screen that
 * nothing took.
 */
it("a criterion the run logged no value for does not show one", async () => {
  primary = met(
    { status: "not_computed", reason: "metric_not_reported", detail: "the run logged no rmse" },
  );
  secondary = [];
  comparison = [];

  const host = await open();
  const card = host.querySelector(".exp-criterion");
  expect(card?.textContent).toContain("the run logged none");
  expect(card?.textContent).not.toContain("0.31");
});

// --- the empty comparison (§5.13) ---

/*
 * The other reading that costs nothing to get wrong. An empty
 * `comparison_to_previous` is the first run of an experiment; drawn as an empty
 * table it reads as "nothing moved", which is the opposite claim.
 */
// --- why a run failed (D34) ---

/*
 * A failed run's metrics are empty, and everything the pane leads with is about
 * metrics: the comparison, the criteria, the numbers. A developer opening one has a
 * single question, and before D34 the pane's answer to it was "SUCCEEDED" crossed
 * out — the exception was in the log pane and nowhere else.
 */
it("a failed run leads with the exception it raised, and names the file and line", async () => {
  primary = met({ status: "not_computed", reason: "metric_not_reported", detail: "no rmse" });
  secondary = [];
  comparison = [];
  failure = {
    exception: "ValueError",
    message: "Input X contains NaN in column 'power_kw' at 3 of 43200 rows",
    frames: [
      { file: "train.py", line: 39, function: "train_once" },
      { file: "base.py", line: 1145, function: "wrapper" },
    ],
  };

  const host = await open();
  const message = host.querySelector(".exp-failure-message");
  expect(message, "the run's exception is nowhere in the pane").not.toBeNull();
  // As it was raised: this route is the developer's own, and the value in the
  // message is their own data.
  expect(message?.textContent).toContain("Input X contains NaN in column 'power_kw'");
  expect(host.textContent).toContain("ValueError");

  // The frames, because a line number is what makes it actionable.
  const frames = [...(host.querySelectorAll(".exp-frames li") ?? [])].map((f) => f.textContent);
  expect(frames).toHaveLength(2);
  expect(frames[0]).toContain("train.py:39");
  expect(frames[0]).toContain("train_once");

  // And what the assistant sees instead, said rather than left to be discovered: a
  // developer who cannot tell masking from guessing cannot read its answer.
  expect(host.textContent).toContain("[value]");
  expect(host.textContent).toContain("L2");
});

it("a run that left no readable exception says so rather than showing an empty section", async () => {
  primary = met({ status: "not_computed", reason: "metric_not_reported", detail: "no rmse" });
  secondary = [];
  comparison = [];
  failure = {
    not_diagnosed: {
      status: "not_diagnosed",
      reason: "no_traceback",
      detail: "the job's output holds no Python traceback",
    },
  };

  const host = await open();
  const said = host.querySelector(".exp-failure-none");
  expect(said, "a failure with no exception rendered as nothing at all").not.toBeNull();
  expect(said?.textContent).toContain("no traceback");
  expect(said?.textContent).toContain("no Python traceback");
  expect(host.querySelector(".exp-failure-message")).toBeNull();
});

it("a run that did not fail has no failure section at all", async () => {
  primary = met(true);
  secondary = [];
  comparison = [];

  const host = await open();
  expect(host.querySelector(".exp-failure-message")).toBeNull();
  expect(host.querySelector(".exp-failure-none")).toBeNull();
  expect(host.textContent).not.toContain("Why it failed");
});

it("an empty comparison is the first run and says so, rather than reading as no change", async () => {
  primary = met(true);
  secondary = [];
  comparison = [];

  const host = await open();
  const text = host.textContent ?? "";
  expect(text).toContain("First run of this experiment");
  expect(host.querySelector(".exp-comparison")).toBeNull();
});

it("a run with a previous one to compare against gets the table and the rule beside it", async () => {
  primary = met(true);
  secondary = [];
  comparison = [
    { metric: "rmse", previous: 0.42, current: 0.31, delta: -0.11, direction: "better", lower_is_better: true },
  ];

  const host = await open();
  const text = host.textContent ?? "";
  expect(host.querySelector(".exp-comparison")).not.toBeNull();
  expect(text).not.toContain("First run of this experiment");
  expect(text).toContain("better");
  // The rule the verdict rests on, shown beside it: without the developer's criteria
  // the direction is inferred from the metric's name, and a hidden rule reads as a
  // judgement rather than as a convention.
  expect(text).toContain("lower is better");
});

// --- the launch card in the conversation (§5.12, D6) ---

/**
 * A probe that mounts the card the way the transcript does.
 *
 * The hook and the card are two halves of one behaviour — a poll that re-reads and
 * a card that re-renders — and testing either alone would leave the join untested,
 * which is where "still running" after a job finished actually comes from.
 */
async function openCard(sessionId: string, launches: number, experimentId: string | null) {
  vi.resetModules();
  const { LaunchedRunsCard, useLaunchedRuns } = await import("./experiments");

  function Probe() {
    const launched = useLaunchedRuns(
      sessionId,
      launches,
      experimentId ? [experimentId] : [],
    );
    return <LaunchedRunsCard experimentId={experimentId} launched={launched} />;
  }

  const host = document.createElement("div");
  document.body.append(host);
  const root = createRoot(host);
  mounted.push(root);
  await act(async () => root.render(<Probe />));
  await act(async () => {
    await Promise.resolve();
  });
  return host;
}

/** A run of one conversation, in the state Ray reports. */
function launched(id: string, status: Experiment["status"], sessionId = "chat-1"): Experiment {
  return { ...EXPERIMENT, experiment_id: id, submission_id: `sub-${id}`, session_id: sessionId, status };
}

/*
 * The whole point of the card: the status is Ray's current one, not the PENDING the
 * launch answered with, and it changes on its own.
 */
it("a running job switches to finished without the developer doing anything", async () => {
  vi.useFakeTimers();
  try {
    listing = [launched("e-1", "RUNNING")];
    const host = await openCard("chat-1", 1, "e-1");
    expect(host.textContent).toContain("running");

    listing = [launched("e-1", "SUCCEEDED")];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(host.textContent).toContain("finished");
    expect(host.textContent).not.toContain("running");

    // And the poll stops, because a finished run's status cannot change again. The
    // count is the assertion: nothing on screen would show a poll that kept going.
    const settled = listingReads;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20000);
    });
    expect(listingReads).toBe(settled);
  } finally {
    vi.useRealTimers();
  }
});

/* A failed job is not a finished one. It is the reading a developer acts on. */
it("a failed job reads as an error", async () => {
  listing = [launched("e-1", "FAILED")];
  const host = await openCard("chat-1", 1, "e-1");
  expect(host.textContent).toContain("error");
  expect(host.textContent).not.toContain("finished");
});

/*
 * The links D6 replaced the frames with, built from the record and the two bases the
 * listing carries. Both open in a tab of their own: a Ray dashboard that replaced
 * the conversation would lose the developer the thing they launched it from.
 */
it("the card links the Ray job and the MLflow run, each in a new tab", async () => {
  listing = [launched("e-1", "RUNNING")];
  const host = await openCard("chat-1", 1, "e-1");

  const popouts = [...host.querySelectorAll(".exp-popout")];
  expect(popouts.map((link) => link.getAttribute("href"))).toEqual([
    "http://ray.test/#/jobs/sub-e-1",
    "http://mlflow.test/#/experiments/1/runs/run-2",
  ]);
  for (const link of popouts) {
    expect(link.getAttribute("target")).toBe("_blank");
    // Without noopener the opened page gets a handle on the conversation's window.
    expect(link.getAttribute("rel")).toContain("noopener");
  }
});

/*
 * And the third link is ODE's own, which is the one that answers "how did it go":
 * the summary, the comparison and the interpretation are in the run document and in
 * neither dashboard. Same tab on purpose — leaving the conversation for a pane of
 * this same application is a move, not a departure — and a real href, so it can be
 * middle-clicked and copied like any link.
 */
it("the card links into the run document, in this tab", async () => {
  listing = [launched("e-1", "RUNNING")];
  const host = await openCard("chat-1", 1, "e-1");

  const inApp = host.querySelector(".exp-open-run");
  expect(inApp?.getAttribute("href")).toContain("/tools/experiments?run=e-1");
  expect(inApp?.getAttribute("target")).toBeNull();
});

/*
 * Another developer's conversation is another conversation. The listing is the
 * caller's own runs, so the filter is about which exchange launched them — the card
 * says what *this* one did, and a run from the session in the next tab appearing
 * under it would be a claim about work this conversation did not do.
 */
it("a run from another conversation is not listed", async () => {
  listing = [launched("e-1", "RUNNING"), launched("e-2", "RUNNING", "chat-2")];
  const host = await openCard("chat-1", 1, "e-1");
  expect(host.querySelectorAll(".exp-launched-row")).toHaveLength(1);
});

/*
 * A conversation that launched nothing must not read the route at all. Not a
 * performance point: a deployment with no Ray cluster does not serve it, and a card
 * that asks anyway turns "this deployment has no experiments" into an error under a
 * tool call that never ran.
 */
it("a conversation with no launch does not read the listing", async () => {
  listing = [launched("e-1", "RUNNING")];
  await openCard("chat-1", 0, null);
  expect(listingReads).toBe(0);
});

/*
 * The defect this card shipped with, in its final form.
 *
 * The listing is filtered to the conversation by the `session_id` on the record,
 * and a run that does not carry one — or carries another — disappeared from the
 * card belonging to the very call that launched it, which then said no run was on
 * the cluster while the job was running. The id came back in the tool result, so
 * the card can always ask for that record by name; the filter decides what *else*
 * is shown, never whether the launch's own run is.
 */
it("the launch's own run is shown even when the listing does not produce it", async () => {
  listing = [launched("e-other", "RUNNING", "chat-2")];
  byId = [{ ...launched("e-1", "RUNNING"), session_id: undefined }];

  const host = await openCard("chat-1", 1, "e-1");
  const rows = [...host.querySelectorAll(".exp-launched-row")];
  expect(rows).toHaveLength(1);
  expect(rows[0].textContent).toContain("running");
  expect(host.textContent).not.toContain("No run of this conversation");
});

/*
 * A run that cannot be read by id either — pruned, or another developer's — leaves
 * the rest of the card standing rather than turning the whole thing into an error.
 */
it("a run that cannot be read at all does not take the card down with it", async () => {
  listing = [launched("e-2", "RUNNING")];
  byId = [];

  const host = await openCard("chat-1", 1, "gone");
  expect(host.querySelectorAll(".exp-launched-row")).toHaveLength(1);
});
