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

package repo

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// The template of §5.11 item 3.
//
// It is not invented. The operator skeleton is the shape Operator Lib actually
// requires — an `MLOperator` subclass implementing `infer`, `train` and
// `need_retraining`, launched by `OperatorLib(Operator(), name=…)` — and the
// layout follows the platform's own ML operator, which splits the operator from
// its training code. `operator.yaml` carries the fields the analytics operator
// repository accepts on registration (§5.14), so registering later is a matter of
// sending that file rather than of assembling a payload from nowhere.
//
// Two rules the scaffold keeps to:
//
//   - **It never overwrites.** A file that is already there belongs to the
//     developer, including one they wrote in place of ours. The result says what
//     was written and what was left alone.
//
//   - **It does not commit.** Writing the working copy and recording history are
//     separate developer actions (§5.11 item 5), so a scaffold is reviewable as a
//     diff before it becomes a commit.
//
// The registry is `ghcr.io` and the workflow is where it is written down (item 4).
// ODE does not hold it as configuration, so changing registry means editing the
// workflow file — which the Code pane can do, because every file is editable.

// The template delimiters are `<<` and `>>` rather than the usual braces because
// two of these files are GitHub Actions workflows, and `${{ github.sha }}` would
// otherwise be parsed as a template action instead of shipped verbatim.
const (
	scaffoldOpen  = "<<"
	scaffoldClose = ">>"
)

// ScaffoldValues is what the templates are rendered with.
type ScaffoldValues struct {
	// Repository is `owner/name`, and Name the repository name alone.
	Repository string
	Name       string
	// Description is the repository description, used in the README and in
	// operator.yaml.
	Description string
	// ClassName is a Python-safe identifier derived from the repository name, used
	// for the model and the tests.
	ClassName string
	// OperatorLib and OperatorLibRef are the D15 pin: which library, at which ref,
	// resolved at scaffold time.
	OperatorLib    string
	OperatorLibRef string
	// Image is where the workflow pushes. Lower-cased, because a container
	// reference may not contain upper case and GitHub owners frequently do.
	Image string
	// PythonVersion is what the Dockerfile and the project metadata agree on.
	PythonVersion string
	// Branch is the branch the workflow builds from.
	Branch string
}

// ScaffoldFile is one rendered file.
type ScaffoldFile struct {
	Path    string `json:"path"`
	Content string `json:"-"`
	// Written is false when the file was already there and was left alone.
	Written bool `json:"written"`
}

// ScaffoldResult is what a scaffold did.
type ScaffoldResult struct {
	Written []string `json:"written"`
	Skipped []string `json:"skipped"`
	// OperatorLibRef is the pin the developer now has, repeated here because it is
	// the one part of a scaffold that is a decision rather than a file.
	OperatorLibRef string `json:"operator_lib_ref"`
	// Hint says what to do next, which after a scaffold is always the same thing:
	// read it, then commit it.
	Hint string `json:"hint"`
}

// scaffoldPaths is the compliance set, in the order a reader should meet it. It is
// also what ScaffoldState reports on, so a repository the developer brought
// themselves can be compared against the template without rendering it.
var scaffoldPaths = []string{
	"main.py",
	"train.py",
	"op.py",
	"training.py",
	"pyproject.toml",
	"Dockerfile",
	".github/workflows/build.yml",
	"operator.yaml",
	"evaluation.yaml",
	"tests/test_op.py",
	".gitignore",
	"README.md",
}

// ScaffoldPaths is the compliance set of §5.11 item 3.
func ScaffoldPaths() []string {
	paths := make([]string, len(scaffoldPaths))
	copy(paths, scaffoldPaths)
	return paths
}

// RenderScaffold renders the template.
func RenderScaffold(values ScaffoldValues) ([]ScaffoldFile, error) {
	if values.PythonVersion == "" {
		values.PythonVersion = defaultPythonVersion
	}
	if values.ClassName == "" {
		values.ClassName = className(values.Name)
	}
	if values.Branch == "" {
		values.Branch = "main"
	}
	files := make([]ScaffoldFile, 0, len(scaffoldPaths))
	for _, path := range scaffoldPaths {
		source, found := scaffoldTemplates[path]
		if !found {
			return nil, fmt.Errorf("the scaffold has no template for %s", path)
		}
		parsed, err := template.New(path).Delims(scaffoldOpen, scaffoldClose).Parse(source)
		if err != nil {
			return nil, fmt.Errorf("the scaffold template %s does not parse: %w", path, err)
		}
		var rendered strings.Builder
		if err := parsed.Execute(&rendered, values); err != nil {
			return nil, fmt.Errorf("rendering the scaffold template %s: %w", path, err)
		}
		files = append(files, ScaffoldFile{Path: path, Content: rendered.String()})
	}
	return files, nil
}

// defaultPythonVersion is what the platform's own ML operator requires. Not the
// newest: Operator Lib pins confluent-kafka at a version with no wheel for the
// current interpreter, which is the same constraint singleuser-image/ documents.
const defaultPythonVersion = "3.10"

var identifierUnsafe = regexp.MustCompile(`[^A-Za-z0-9]+`)

// className turns a repository name into a Python class name.
func className(name string) string {
	parts := identifierUnsafe.Split(name, -1)
	var builder strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		builder.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	identifier := builder.String()
	if identifier == "" || (identifier[0] >= '0' && identifier[0] <= '9') {
		identifier = "Operator" + identifier
	}
	return identifier
}

var scaffoldTemplates = map[string]string{
	"main.py": `"""Entry point. Operator Lib owns the process; the operator owns the modelling.

Kept thin on purpose: everything about Kafka, configuration, the model registry
and the lifecycle is Operator Lib's, and code added here runs outside all of it.
"""

from op import Operator

from operator_lib.operator_lib import OperatorLib


if __name__ == "__main__":
    OperatorLib(
        Operator(),
        name="<<.Repository>>",
        # Written by the Dockerfile from the build's commit, so a running operator
        # can say which source it is (§5.11 item 7).
        git_info_file="git_commit",
    )
`,

	"train.py": `"""Entry point of an experiment: train once, record the run, exit.

The deployed operator starts at main.py, where Operator Lib takes the process,
trains if no model is registered yet, and then consumes Kafka until it is stopped.
An experiment wants the first half of that and not the second, so this file runs
Operator Lib's own init sequence and stops exactly where main.py would enter the
loop it never leaves.

It is deliberately the same path rather than a smaller one. MLOperator.init() is
what sets the tracking URI, connects to Ray, calls train() and registers the
result, so a run started here does what the deployed operator does when it first
comes up. ODE adds only the commit tag on the run.

ODE runs this file. It is yours to change, but keep the init()/train_once() pair
at the end: everything a run records happens inside one of the two.

Needs Operator Lib v1.4.0 or newer for train_once(), which pyproject.toml pins.
"""

import json
import sys

import confluent_kafka
import mlflow
from mlflow import MlflowClient

import operator_lib.util as util

from op import Operator


def _already_registered(model_id: str) -> bool:
    """Whether a model is registered under this key.

    This is the same question MLOperator.init() asks before it trains, and it is
    asked here because init() does not report the answer. Getting it wrong costs a
    duplicate training pass, not a wrong result.
    """
    try:
        MlflowClient().get_model_version_by_alias(model_id, "production")
        return True
    except Exception:
        return False


def main() -> int:
    dep_config = util.DeploymentConfig()
    config_json = json.loads(dep_config.config)
    opr_config = util.OperatorConfig(config_json)
    util.init_logger(opr_config.config.logger_level, "<<.Repository>>")

    operator = Operator()

    # Built because init() takes them, never polled: this process stops before
    # operator.start(), and start() is the call that would subscribe. Constructing
    # a consumer does not open a connection, so an experiment needs no reachable
    # broker unless one of its input topics is replayed from Kafka rather than
    # read from timescale.
    kafka_consumer = confluent_kafka.Consumer(
        {
            "bootstrap.servers": dep_config.config_bootstrap_servers or "",
            "group.id": dep_config.config_application_id,
            "auto.offset.reset": dep_config.consumer_auto_offset_reset_config,
        },
        logger=util.logger,
    )
    kafka_producer = confluent_kafka.Producer(
        {"bootstrap.servers": dep_config.config_bootstrap_servers or ""},
        logger=util.logger,
    )

    filter_handler = util.create_filter_handler(
        opr_config.inputTopics, dep_config.pipeline_id, operator.selectors
    )

    typed_config = operator.configType(config_json.get("config", {}))

    # An experiment trains every time, and init() trains only when it finds no
    # model registered under this pipeline and operator. ODE keeps that pair stable
    # per developer and repository — so model versions accumulate under one key and
    # the "production" alias moves, as they do for a deployed operator — which means
    # the first run trains inside init() and every run after it does not.
    #
    # So the answer is taken first and the pass asked for when init() will not make
    # one. Asking unconditionally would train twice on the first run.
    mlflow.set_tracking_uri(opr_config.config.mlflow_url)
    model_id = f"pipeline-{dep_config.pipeline_id}_operator-{dep_config.operator_id}"
    trains_inside_init = not _already_registered(model_id)

    operator.init(
        kafka_consumer=kafka_consumer,
        kafka_producer=kafka_producer,
        filter_handler=filter_handler,
        output_topic=dep_config.output,
        pipeline_id=dep_config.pipeline_id,
        operator_id=dep_config.operator_id,
        config=typed_config,
        result_error_handler=None,
    )

    if not trains_inside_init:
        # Operator Lib v1.4.0 and newer. On an older pin this raises
        # AttributeError, and the pin is in pyproject.toml.
        operator.train_once()
    return 0


if __name__ == "__main__":
    sys.exit(main())
`,

	"op.py": `"""The operator: what it infers per message, and when it retrains.

MLOperator is the machine-learning half of Operator Lib. It loads the model
registered under this pipeline and operator from MLflow, calls infer() for every
message that matches a selector, and calls train() when there is no model yet or
when need_retraining() says so. Training runs on Ray.

Three methods are yours. Everything else — Kafka, the model registry, the
lifecycle — belongs to the library.
"""

import datetime
import typing

from mlflow.pyfunc import PyFuncModel, PythonModel

from operator_lib.util import Config, MLOperator, Selector
from operator_lib.util.helpers import TrainMlflowLogger

from training import train_model


class CustomConfig(Config):
    """Deployment configuration, typed.

    The base Config already carries mlflow_url, ray_url and ts_conn. Anything
    added here arrives from the operator's deployment config under the same name,
    with this value as the default.
    """

    # Retrain at most this often, in seconds. A day, so a deployment does not
    # spend its life training.
    retrain_after_s = 86400


class Operator(MLOperator):
    configType = CustomConfig

    # Which inputs this operator accepts. "args" are the mapping destinations the
    # pipeline is configured with, and the name is what infer() receives as
    # "selector", so one operator can treat several input shapes differently.
    selectors = [
        Selector({"name": "value", "args": ["value"]}),
    ]

    def init(self, *args, **kwargs):
        super().init(*args, **kwargs)
        self.trained_at: typing.Optional[datetime.datetime] = None

    def infer(
        self,
        model: typing.Optional[PyFuncModel],
        data: typing.Dict[str, typing.Any],
        selector: str,
        device_id: str,
        timestamp: datetime.datetime,
    ) -> typing.Tuple[
        typing.Optional[datetime.datetime], typing.Optional[typing.Any], typing.Optional[PythonModel]
    ]:
        """Called for every message. Returns (result timestamp, result, new model).

        The result timestamp may be None, which means now; it is what lets a
        forecast carry the time it is about rather than the time it was made. The
        third element replaces the registered model and is almost always None —
        return one only from an algorithm that genuinely updates per message.
        """
        value = data.get("value")
        if value is None or model is None:
            return None, None, None

        prediction = model.predict({"timestamp": timestamp, "value": float(value)})
        return None, {"prediction": prediction}, None

    def train(
        self, model: typing.Optional[PyFuncModel], logger: TrainMlflowLogger
    ) -> typing.Optional[PythonModel]:
        """Called when there is no model, or when need_retraining() said so.

        Runs inside a Ray session the library opened, with an MLflow run already
        started — so params and metrics logged through "logger" land on the run
        the resulting model is registered from.
        """
        self.trained_at = datetime.datetime.now(datetime.timezone.utc)
        return train_model(logger)

    def need_retraining(self, model: typing.Optional[PyFuncModel]) -> bool:
        """Called after every inference, so it has to be cheap.

        Time-based to start with. A better policy watches the data — a drift in the
        input distribution, or an error that stops falling — and that is a decision
        to make with the profile in front of you rather than a default to inherit.
        """
        if model is None:
            return True
        if self.trained_at is None:
            return False
        age = datetime.datetime.now(datetime.timezone.utc) - self.trained_at
        return age.total_seconds() >= self.config.retrain_after_s
`,

	"training.py": `"""Training, on Ray.

Separate from op.py because the two run in different places: op.py runs in the
operator's own process for every message, while this runs distributed and rarely.
Keeping them apart is also what makes the training testable without Kafka.

provide_historic_data() is Operator Lib's reader over the platform's timeseries
store. It returns Ray Datasets, so the training below never holds the whole
history in one process.
"""

import datetime
import typing

import ray
from mlflow.pyfunc import PythonModel

from operator_lib.util.helpers import provide_historic_data, TrainMlflowLogger


# How much history one training pass reads.
TRAINING_WINDOW = datetime.timedelta(days=90)


class <<.ClassName>>Model(PythonModel):
    """The model MLflow registers and op.py later loads.

    predict() receives exactly the payload infer() builds, so the two are one
    contract in two files. Keep it serialisable: MLflow stores this object.
    """

    def __init__(self, baseline: float) -> None:
        self.baseline = baseline

    def predict(self, context, model_input=None, params=None):
        # The pyfunc signature carries a context in production and not in a test,
        # so the payload is taken from whichever argument holds it.
        payload = model_input if model_input is not None else context
        value = float(payload.get("value", 0.0))
        return value - self.baseline


@ray.remote
def _fit(datasets: typing.List[typing.Any]) -> float:
    """The distributed part. Replace the body; keep the shape.

    A Ray task rather than a plain function so that training scales with the
    cluster rather than with the operator's pod.
    """
    total, count = 0.0, 0
    for dataset in datasets:
        for batch in dataset.iter_batches(batch_size=4096):
            values = batch.get("value")
            if values is None:
                continue
            for value in values:
                if value is None:
                    continue
                total += float(value)
                count += 1
    return total / count if count else 0.0


def train_model(logger: TrainMlflowLogger) -> typing.Optional[PythonModel]:
    """Read the history, fit, and hand back a model for MLflow to register."""
    with logger.trace("read history"):
        datasets = provide_historic_data(TRAINING_WINDOW)
    if not datasets:
        # Explicitly nothing rather than a model fitted on no data: returning None
        # leaves the previously registered model in place.
        return None

    with logger.trace("fit"):
        baseline = ray.get(_fit.remote(datasets))

    logger.log_param("training_window_days", TRAINING_WINDOW.days)
    logger.log_metric("baseline", baseline)
    return <<.ClassName>>Model(baseline=baseline)
`,

	"pyproject.toml": `[project]
name = "<<.Name>>"
version = "0.1.0"
# The minor series is pinned rather than left as a floor. uv resolves the driver's
# environment on the Ray head and each worker's on its own node, and a floor lets
# those land on different minor versions — which surfaces as a Ray "version
# mismatch" between driver and worker rather than as anything about Python. The
# patch level is deliberately left open: it is the minor that has to agree.
requires-python = "==<<.PythonVersion>>.*"
dependencies = [
  "ray[client,train]",
  # Pinned at scaffold time (D15). Operator Lib tracks latest and makes no
  # stability promise, so this pin is what keeps a rebuild reproducible — and
  # moving it is a deliberate edit, not a side effect of building again.
  "operator-lib @ git+https://github.com/<<.OperatorLib>>.git@<<.OperatorLibRef>>",
]

[project.optional-dependencies]
dev = ["pytest"]

[tool.uv]
package = false
`,

	"Dockerfile": `# The operator image. Built and pushed by .github/workflows/build.yml.
#
# uv rather than plain pip, and "uv run" rather than plain "python": Ray worker
# processes inherit the launching interpreter, so a driver started outside the
# locked environment can hand workers an environment that lacks its dependencies.
FROM python:<<.PythonVersion>>-slim

# git for the Operator Lib dependency, which is a git reference rather than a PyPI
# release; librdkafka for confluent-kafka, which Operator Lib pins.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git librdkafka-dev gcc \
 && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir uv

WORKDIR /usr/src/app

# Dependencies first, so a source change does not re-resolve them.
COPY pyproject.toml ./
COPY uv.lock* ./
RUN uv sync --no-dev

COPY . .

# Which commit this image is, read at startup by Operator Lib and reported in the
# operator's own log. It is also what makes an experiment's MLflow tag and a
# running operator comparable (§5.11 item 7).
ARG GIT_COMMIT=unknown
RUN printf 'commit=%s\n' "${GIT_COMMIT}" > git_commit

CMD ["uv", "run", "--no-dev", "main.py"]
`,

	".github/workflows/build.yml": `# Builds the operator image and pushes it to the GitHub container registry.
#
# The registry is ghcr.io and this file is where that is written down (§5.11
# item 4) — ODE does not hold it as configuration. To publish somewhere else,
# change the login step and the tags below; nothing outside this file needs to know.
#
# No secret to set up: GITHUB_TOKEN is issued to the workflow and the packages
# permission below is what lets it push.
name: Build operator image

on:
  push:
    branches:
      - <<.Branch>>
  workflow_dispatch:

permissions:
  contents: read
  packages: write

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Log in to ghcr.io
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Set up Buildx
        uses: docker/setup-buildx-action@v3

      - name: Build and push
        uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          build-args: |
            GIT_COMMIT=${{ github.sha }}
          # Two tags on purpose: one that moves and one that does not. A deployment
          # that names the sha is reproducible; one that names latest is convenient.
          tags: |
            <<.Image>>:latest
            <<.Image>>:${{ github.sha }}
          cache-from: type=gha
          cache-to: type=gha,mode=max
`,

	"operator.yaml": `# The operator as the analytics stack registers it (§5.14).
#
# ODE offers registration as an explicit action and never performs it on its own,
# so this file is the payload that action sends. Edit it here; it is the source of
# truth for what the pipeline editor will show.
name: <<.Name>>
description: <<if .Description>><<.Description>><<else>>An ODE-developed analytics operator<<end>>
image: <<.Image>>:latest

# Public makes the operator visible to other users of the platform. Registration
# is a developer decision and so is this.
pub: false
deploymentType: local

# What the pipeline editor offers as inputs. "name" has to match a selector arg in
# op.py — the two together are how a message reaches infer().
inputs:
  - name: value
    type: float

# What the operator emits. These are the keys of the dictionary infer() returns.
outputs:
  - name: prediction
    type: float

# Deployment configuration, matching CustomConfig in op.py.
config_values:
  - name: retrain_after_s
    type: int
`,

	"evaluation.yaml": `# The evaluation criteria for this operator.
#
# Yours. ODE has no tool that writes this file and the assistant is not permitted
# to change it (§5.8) — an operator that grades itself against criteria an
# assistant relaxed has not been evaluated. ODE reads it to say whether a run met
# what you asked for, and stops there.

# The metric a run is judged on. It has to be a metric the training actually logs,
# or a run will report it as absent rather than as failed.
metric: baseline

# The direction that counts as better, and the value that counts as good enough.
goal: minimise
threshold: 0.0

# What else to watch. Reported beside the decisive metric on every run, so a model
# that wins on one number and loses on another is visible rather than surprising.
secondary_metrics: []

# Free text for the reasoning behind the numbers above. Worth writing: the number
# is the criterion, and this is why it is that number.
rationale: >
  Replace the metric and threshold with the ones this operator is actually for.
  The scaffold's values exist so the file parses, not because they mean anything.
`,

	"tests/test_op.py": `"""Tests for the operator's own logic.

They run without Kafka, without Ray and without MLflow, which is the point: the
three methods that are yours are pure enough to test directly, and a test that
needs the platform would not be run.

    uv run --extra dev pytest
"""

import datetime

from op import CustomConfig, Operator
from training import <<.ClassName>>Model


def _operator() -> Operator:
    """An operator with its config in place but none of the platform behind it.

    Operator Lib's init() wires Kafka and the model registry, so it is deliberately
    not called here; the attributes the tested methods read are set directly.
    """
    operator = Operator()
    operator.config = CustomConfig({})
    operator.trained_at = None
    return operator


def test_infer_without_a_model_produces_nothing():
    timestamp, result, model = _operator().infer(
        None, {"value": 1.0}, "value", "device-1", datetime.datetime.now()
    )
    assert (timestamp, result, model) == (None, None, None)


def test_infer_returns_the_models_prediction():
    operator = _operator()
    _, result, _ = operator.infer(
        <<.ClassName>>Model(baseline=2.0),
        {"value": 5.0},
        "value",
        "device-1",
        datetime.datetime.now(),
    )
    assert result == {"prediction": 3.0}


def test_a_missing_value_is_not_an_error():
    _, result, _ = _operator().infer(
        <<.ClassName>>Model(baseline=0.0), {}, "value", "device-1", datetime.datetime.now()
    )
    assert result is None


def test_retraining_is_needed_until_there_is_a_model():
    operator = _operator()
    assert operator.need_retraining(None) is True
    assert operator.need_retraining(<<.ClassName>>Model(baseline=0.0)) is False
`,

	".gitignore": `__pycache__/
*.py[cod]
.venv/
venv/
.pytest_cache/
.ruff_cache/

# Written into the image at build time from the commit being built.
git_commit

# Local model and data artifacts. The platform's MLflow is where a model belongs;
# a checkout is not a model registry.
mlruns/
*.pkl
*.pt
.env
`,

	"README.md": `# <<.Name>>

<<if .Description>><<.Description>>

<<end>>An analytics operator for the SENERGY platform, scaffolded by the Operator
Development Environment. Every file here is yours to change, including this one.

## Layout

| File | What it is |
|---|---|
| "main.py" | Entry point of the deployed operator. Hands the process to Operator Lib. |
| "train.py" | Entry point of an experiment. Trains through Operator Lib, then exits. |
| "op.py" | The operator: "infer", "train", "need_retraining", and its config. |
| "training.py" | The Ray training pass and the model MLflow registers. |
| "pyproject.toml" | Dependencies, with Operator Lib pinned at "<<.OperatorLibRef>>". |
| "uv.lock" | Not scaffolded — run "uv lock" and commit it. See below. |
| "Dockerfile" | The image. Built by CI; buildable by hand. |
| ".github/workflows/build.yml" | Builds and pushes "<<.Image>>". Change the registry here. |
| "operator.yaml" | What the analytics stack registers: inputs, outputs, config. |
| "evaluation.yaml" | Your criteria for whether a run is good. ODE never writes this. |
| "tests/test_op.py" | Tests for the three methods that are yours. |

## Lock the dependencies before the first experiment

    uv lock

Commit the "uv.lock" it writes. An experiment runs "uv run python train.py" on the
cluster, and uv builds the environment from "pyproject.toml" and this file — on the
Ray head for the driver and on each worker node for the tasks, out of its own cache.

Without a lock file uv resolves at run time, which works and is worse in one
specific way: the run records a commit SHA as the code that produced it, and two
runs of the same commit can then resolve different dependency versions. The lock
file is what makes the recorded SHA describe the whole run rather than only its
source.

## Running the tests

    uv run --extra dev pytest

## Building by hand

    docker build --build-arg GIT_COMMIT=$(git rev-parse HEAD) -t <<.Image>>:dev .

## The Operator Lib pin

"pyproject.toml" pins Operator Lib at "<<.OperatorLibRef>>", the newest at the time
this repository was scaffolded. The library tracks latest and promises no
stability, so moving the pin is a deliberate edit — do it, run the tests, and
commit the two together.
`,
}
