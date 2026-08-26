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
import { afterEach, expect, it, vi } from "vitest";
import type { EvaluationCriterion, Session } from "./api";

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
 * boundaries are faked. Nothing here asserts anything about the iframes; whether a
 * cross-origin page frames is not a question jsdom can answer, and the probe is
 * deliberately left untested.
 */

vi.mock("./keycloak", () => ({
  initKeycloak: vi.fn(async () => true),
  token: vi.fn(async () => "test-token"),
  logout: vi.fn(),
}));

/** The criteria the mocked results route answers with. Set by each test. */
let primary: EvaluationCriterion = met(true);
let secondary: EvaluationCriterion[] = [];
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

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      experiments: async () => ({
        experiments: [EXPERIMENT],
        count: 1,
        ray_url: "http://ray.test",
        mlflow_url: "http://mlflow.test",
      }),
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
      }),
      // Never settles: the interpretation is not what these two tests are about, and
      // a pending read keeps its section out of the text being asserted on.
      interpretation: () => new Promise<never>(() => {}),
      embedProbes: () => new Promise<never>(() => {}),
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
