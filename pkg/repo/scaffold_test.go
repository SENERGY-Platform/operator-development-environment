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

package repo_test

import (
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

func renderTestScaffold(t *testing.T) map[string]string {
	t.Helper()
	files, err := repo.RenderScaffold(repo.ScaffoldValues{
		Repository:     "SENERGY-Platform/PV-Forecast",
		Name:           "PV-Forecast",
		Description:    "Forecast PV generation",
		OperatorLib:    "SENERGY-Platform/analytics-operator-lib-python",
		OperatorLibRef: "v1.3.1",
		Image:          "ghcr.io/senergy-platform/pv-forecast",
		Branch:         "main",
	})
	if err != nil {
		t.Fatalf("RenderScaffold: %v", err)
	}
	rendered := map[string]string{}
	for _, file := range files {
		rendered[file.Path] = file.Content
	}
	return rendered
}

func TestTheScaffoldRendersEveryComplianceFile(t *testing.T) {
	rendered := renderTestScaffold(t)
	for _, path := range repo.ScaffoldPaths() {
		content, found := rendered[path]
		if !found {
			t.Errorf("%s was not rendered", path)
			continue
		}
		if strings.TrimSpace(content) == "" {
			t.Errorf("%s rendered empty", path)
		}
	}
	if len(rendered) != len(repo.ScaffoldPaths()) {
		t.Errorf("rendered %d files, want %d", len(rendered), len(repo.ScaffoldPaths()))
	}
}

// A template delimiter left in a rendered file means a value that never arrived,
// and it would reach the developer's repository as literal text.
func TestNoTemplateDelimitersSurviveRendering(t *testing.T) {
	for path, content := range renderTestScaffold(t) {
		if strings.Contains(content, "<<") || strings.Contains(content, ">>") {
			t.Errorf("%s still contains a template delimiter", path)
		}
	}
}

// The workflow's own `${{ … }}` expressions must survive, which is why the
// templates use different delimiters at all.
func TestTheWorkflowKeepsItsActionsExpressions(t *testing.T) {
	workflow := renderTestScaffold(t)[".github/workflows/build.yml"]
	for _, expression := range []string{
		"${{ github.sha }}", "${{ secrets.GITHUB_TOKEN }}", "${{ github.actor }}",
	} {
		if !strings.Contains(workflow, expression) {
			t.Errorf("the workflow lost %s", expression)
		}
	}
}

// §5.11 item 4: ghcr.io by default, and the workflow is where it is written down.
func TestTheWorkflowPushesToGhcr(t *testing.T) {
	workflow := renderTestScaffold(t)[".github/workflows/build.yml"]
	if !strings.Contains(workflow, "registry: ghcr.io") {
		t.Error("the workflow does not log in to ghcr.io")
	}
	if !strings.Contains(workflow, "ghcr.io/senergy-platform/pv-forecast:latest") {
		t.Error("the workflow does not tag the lower-cased image")
	}
	if !strings.Contains(workflow, "packages: write") {
		t.Error("the workflow lacks the permission that lets GITHUB_TOKEN push")
	}
	if !strings.Contains(workflow, "branches:\n      - main") {
		t.Error("the workflow does not build the repository's default branch")
	}
}

// The pin of D15 has to reach the one file that installs the library.
func TestThePinReachesTheProjectFile(t *testing.T) {
	pyproject := renderTestScaffold(t)["pyproject.toml"]
	want := "git+https://github.com/SENERGY-Platform/analytics-operator-lib-python.git@v1.3.1"
	if !strings.Contains(pyproject, want) {
		t.Errorf("pyproject.toml does not pin Operator Lib:\n%s", pyproject)
	}
	if !strings.Contains(pyproject, `name = "PV-Forecast"`) {
		t.Error("pyproject.toml does not carry the repository name")
	}
}

// The operator skeleton has to be the shape Operator Lib actually calls, or it is
// a file that looks right and never runs.
func TestTheOperatorSkeletonImplementsWhatOperatorLibCalls(t *testing.T) {
	operator := renderTestScaffold(t)["op.py"]
	for _, required := range []string{
		"from operator_lib.util import Config, MLOperator, Selector",
		"class Operator(MLOperator):",
		"configType = CustomConfig",
		"selectors = [",
		"def infer(",
		"def train(",
		"def need_retraining(",
	} {
		if !strings.Contains(operator, required) {
			t.Errorf("op.py is missing %q", required)
		}
	}

	main := renderTestScaffold(t)["main.py"]
	if !strings.Contains(main, "OperatorLib(") ||
		!strings.Contains(main, `name="SENERGY-Platform/PV-Forecast"`) {
		t.Errorf("main.py does not launch the operator:\n%s", main)
	}
}

// The class name is derived from a repository name that is not a Python
// identifier, so this is the case that would otherwise render invalid code.
func TestTheModelClassNameIsAValidIdentifier(t *testing.T) {
	training := renderTestScaffold(t)["training.py"]
	if !strings.Contains(training, "class PVForecastModel(PythonModel):") {
		t.Errorf("training.py does not define a usable model class:\n%s", training)
	}
	tests := renderTestScaffold(t)["tests/test_op.py"]
	if !strings.Contains(tests, "from training import PVForecastModel") {
		t.Error("the tests do not import the model they exercise")
	}
}

// operator.yaml is the §5.14 registration payload, so it carries the fields the
// analytics operator repository accepts and the image the workflow builds.
func TestTheOperatorDescriptorCarriesTheRegistrationFields(t *testing.T) {
	descriptor := renderTestScaffold(t)["operator.yaml"]
	for _, field := range []string{
		"name: PV-Forecast", "image: ghcr.io/senergy-platform/pv-forecast:latest",
		"deploymentType:", "inputs:", "outputs:", "config_values:", "pub: false",
	} {
		if !strings.Contains(descriptor, field) {
			t.Errorf("operator.yaml is missing %q", field)
		}
	}
}

// evaluation.yaml is the developer's, and the file says so — the assistant has no
// tool that writes it (§5.8).
func TestTheEvaluationFileSaysItIsTheDevelopers(t *testing.T) {
	evaluation := renderTestScaffold(t)["evaluation.yaml"]
	for _, field := range []string{"metric:", "threshold:", "goal:"} {
		if !strings.Contains(evaluation, field) {
			t.Errorf("evaluation.yaml is missing %q", field)
		}
	}
	if !strings.Contains(evaluation, "§5.8") {
		t.Error("evaluation.yaml does not say that no tool may change it")
	}
}

// A default is a decision here: the Dockerfile and the project metadata have to
// name the same interpreter, or the image builds an environment the code was not
// written for.
func TestTheImageAndTheProjectAgreeOnThePythonVersion(t *testing.T) {
	rendered := renderTestScaffold(t)
	if !strings.Contains(rendered["Dockerfile"], "FROM python:3.10-slim") {
		t.Errorf("Dockerfile:\n%s", rendered["Dockerfile"])
	}
	// The minor series, not a floor: uv resolves driver and workers separately, and
	// a floor lets them land on different minors — a Ray version mismatch that reads
	// as anything but a Python problem.
	if !strings.Contains(rendered["pyproject.toml"], `requires-python = "==3.10.*"`) {
		t.Errorf("pyproject.toml:\n%s", rendered["pyproject.toml"])
	}
	if !strings.Contains(rendered["Dockerfile"], "GIT_COMMIT") {
		t.Error("the Dockerfile does not record which commit it was built from")
	}
}
