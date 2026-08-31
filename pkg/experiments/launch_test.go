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
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// --- the reproducibility guard (§5.11 item 7) ---

func TestALaunchIsRefusedWhileTheWorkingCopyHasUncommittedChanges(t *testing.T) {
	h := newHarness(t)
	// A scaffolded, uncommitted checkout: eleven files git has never seen.
	h.createRepository()

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})
	if err == nil {
		t.Fatal("the launch was accepted from a working copy with no commit")
	}

	var dirty *experiments.DirtyError
	if !errors.As(err, &dirty) {
		t.Fatalf("error = %T (%v), want a DirtyError naming what is uncommitted", err, err)
	}
	if !dirty.Unborn {
		t.Errorf("dirty = %+v, want the unborn case: nothing is committed at all", dirty)
	}
	if len(h.ray.Jobs()) != 0 {
		t.Error("a job was submitted from an uncommitted working copy")
	}
	if names := h.mlflow.Experiments(); len(names) != 0 {
		t.Errorf("an MLflow experiment was created for a refused launch: %v", names)
	}
}

func TestARefusedLaunchNamesTheUncommittedPaths(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.write("op.py", "# edited but not committed\n")
	h.write("notes.md", "scratch\n")

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})

	var dirty *experiments.DirtyError
	if !errors.As(err, &dirty) {
		t.Fatalf("error = %T (%v), want a DirtyError", err, err)
	}
	if dirty.Unborn {
		t.Error("the repository has a commit, so this is not the unborn case")
	}
	// The paths are what makes the refusal actionable: "commit your work" without
	// saying which files is worse than what the pane already shows.
	joined := strings.Join(dirty.Paths, " ")
	if !strings.Contains(joined, "op.py") || !strings.Contains(joined, "notes.md") {
		t.Errorf("paths = %v, want both uncommitted files named", dirty.Paths)
	}
	if !strings.Contains(dirty.Error(), "pv-forecast") {
		t.Errorf("message = %q, want the repository named", dirty.Error())
	}
}

func TestALaunchWithoutARepositorySaysWhatIsMissing(t *testing.T) {
	h := newHarness(t)
	// Connected to GitHub, but nothing selected.

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})
	if !errors.Is(err, repo.ErrNoRepository) {
		t.Fatalf("error = %v, want the repo service's own 'no repository' refusal", err)
	}
}

// --- the package (§5.12) ---

// The archive is the committed tree with the files at the archive root, which is
// both what Ray's package format wants and what makes the recorded commit SHA
// describe what actually ran.
func TestThePackageIsTheCommittedTreeAtTheArchiveRoot(t *testing.T) {
	h := newHarness(t)
	sha := h.ready()

	// A second commit, so the launch cannot pass by using the first one.
	h.write("op.py", "# a revised operator\n")
	h.commit("Revise the operator")

	// Present in the directory and ignored by the scaffold's .gitignore, so the tree
	// is still clean and the launch is allowed. If this reaches the archive, the
	// package is a copy of the working directory rather than the commit — and the
	// recorded SHA would be describing something that never ran.
	h.write("mlruns/local-model.pkl", "a local artefact that must not be shipped\n")

	result := h.launch()

	archive, found := h.ray.Package(strings.TrimPrefix(result.PackageURI, "gcs://"))
	if !found {
		t.Fatalf("the cluster holds no package named %q; it has %v",
			result.PackageURI, h.ray.Packages())
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("the package is not a readable zip: %v", err)
	}

	names := map[string]bool{}
	for _, file := range reader.File {
		names[file.Name] = true
	}
	// At the root, not under a directory named after the repository.
	for _, want := range []string{"op.py", "training.py", ".github/workflows/build.yml"} {
		if !names[want] {
			t.Errorf("the package has no %s at its root; it holds %v", want, sortedNames(names))
		}
	}
	for name := range names {
		if strings.HasPrefix(name, "mlruns/") {
			t.Errorf("the package carries an ignored local artefact: %s", name)
		}
	}
	for name := range names {
		if strings.HasPrefix(name, ".git/") {
			t.Errorf("the package carries git's own object database: %s", name)
		}
	}
	if result.CommitSHA == sha {
		t.Error("the launch used the first commit rather than HEAD")
	}
}

// The hash names the content, so a second launch from the same commit finds the
// package already there. That is what keeps a re-run from moving the repository
// across the network twice.
func TestThePackageIsUploadedOnceAndReusedOnASecondLaunch(t *testing.T) {
	h := newHarness(t)
	h.ready()

	first := h.launch()
	if h.ray.Uploads() != 1 {
		t.Fatalf("uploads after the first launch = %d, want 1", h.ray.Uploads())
	}
	if first.PackageReused {
		t.Error("the first launch reported the package as reused")
	}

	second := h.launch()
	if h.ray.Uploads() != 1 {
		t.Errorf("uploads after the second launch = %d, want the archive reused", h.ray.Uploads())
	}
	if !second.PackageReused {
		t.Error("the second launch did not report the package as reused")
	}
	if second.PackageURI != first.PackageURI {
		t.Errorf("package URI = %q then %q; the same commit must produce the same package",
			first.PackageURI, second.PackageURI)
	}
	if second.SubmissionID == first.SubmissionID {
		t.Error("two launches share a submission id, so the second would be refused by Ray")
	}
}

func TestANewCommitProducesANewPackage(t *testing.T) {
	h := newHarness(t)
	h.ready()
	first := h.launch()

	h.write("op.py", "# a different operator\n")
	h.commit("Adjust the operator")
	second := h.launch()

	if second.PackageURI == first.PackageURI {
		t.Errorf("both commits produced %q; the package name is over the content", first.PackageURI)
	}
	if h.ray.Uploads() != 2 {
		t.Errorf("uploads = %d, want one per distinct commit", h.ray.Uploads())
	}
}

func TestAPackageOverTheConfiguredBoundIsRefusedRatherThanTruncated(t *testing.T) {
	h := newHarness(t, func(deps *experiments.Deps) {
		// Smaller than the scaffold, so the guard fires on a real archive rather than
		// on a fixture built to trip it.
		deps.MaxPackageBytes = 512
	})
	h.ready()

	_, err := h.service.Launch(context.Background(),
		experiments.LaunchRequest{Request: h.request(), InputTopics: testInputTopics()})

	var tooLarge *experiments.PackageTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %T (%v), want a PackageTooLargeError", err, err)
	}
	if tooLarge.Limit != 512 || tooLarge.Bytes <= tooLarge.Limit {
		t.Errorf("error = %+v, want the actual size and the limit, so the gap is actionable",
			tooLarge)
	}
	if h.ray.Uploads() != 0 {
		t.Error("an oversized package was uploaded anyway")
	}
}

// --- the submission (§5.12) ---

// §5.12 names four metadata keys. All four have to be there at submission time,
// which is the reason ODE creates the MLflow run before asking Ray for anything.
func TestTheSubmittedMetadataCarriesTheFourKeysOfSection512(t *testing.T) {
	h := newHarness(t)
	h.ready()

	result := h.launch()
	job := h.ray.LastJob(t)

	if got := mustFind(t, job.Metadata, "session_id"); got != "sess-1" {
		t.Errorf("session_id = %q", got)
	}
	if got := mustFind(t, job.Metadata, "user_sub"); got != testUserSub {
		t.Errorf("user_sub = %q", got)
	}
	if got := mustFind(t, job.Metadata, "commit_sha"); got != result.CommitSHA {
		t.Errorf("commit_sha = %q, want the commit the package was built from %q",
			got, result.CommitSHA)
	}
	if got := mustFind(t, job.Metadata, "mlflow_run_id"); got != result.RunID {
		t.Errorf("mlflow_run_id = %q, want the run ODE created %q", got, result.RunID)
	}
	if result.RunID == "" {
		t.Fatal("no MLflow run was created, so the metadata key cannot be meaningful")
	}
}

func TestTheJobIsPointedAtTheUploadedPackageAndTheRunItShouldLogTo(t *testing.T) {
	h := newHarness(t)
	h.ready()

	result := h.launch()
	job := h.ray.LastJob(t)

	if job.RuntimeEnv.WorkingDir != result.PackageURI {
		t.Errorf("working_dir = %q, want the uploaded package %q",
			job.RuntimeEnv.WorkingDir, result.PackageURI)
	}
	if !strings.HasPrefix(job.RuntimeEnv.WorkingDir, "gcs://_ray_pkg_") {
		t.Errorf("working_dir = %q, want Ray's own gcs package naming",
			job.RuntimeEnv.WorkingDir)
	}
	if got := job.RuntimeEnv.EnvVars["MLFLOW_RUN_ID"]; got != result.RunID {
		t.Errorf("MLFLOW_RUN_ID = %q, want %q", got, result.RunID)
	}
	// MLFLOW_TRACKING_URI and MLFLOW_EXPERIMENT_ID are deliberately absent. ODE no
	// longer tells the job where MLflow is, because Operator Lib reads that from its
	// own deployment config — setting both was the duplication this replaced, and a
	// TrainMlflowLogger calls set_tracking_uri from the config anyway, so ODE's
	// variable would have been overridden rather than honoured.
	for _, absent := range []string{"MLFLOW_TRACKING_URI", "MLFLOW_EXPERIMENT_ID"} {
		if got, ok := job.RuntimeEnv.EnvVars[absent]; ok {
			t.Errorf("%s = %q, want it unset: the deployment config carries it", absent, got)
		}
	}

	// What Operator Lib actually reads.
	var config struct {
		Config struct {
			MLflowURL string `json:"mlflow_url"`
			RayURL    string `json:"ray_url"`
			TsConn    string `json:"ts_conn"`
		} `json:"config"`
		InputTopics []struct {
			Name     string `json:"name"`
			Mappings []struct {
				Dest   string `json:"dest"`
				Source string `json:"source"`
			} `json:"mappings"`
		} `json:"inputTopics"`
	}
	raw, ok := job.RuntimeEnv.EnvVars["CONFIG"]
	if !ok {
		t.Fatal("the job carries no CONFIG, so Operator Lib has no deployment config to read")
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("CONFIG is not the JSON Operator Lib parses: %v", err)
	}
	if config.Config.MLflowURL != h.mlflow.URL() {
		t.Errorf("config.mlflow_url = %q, want the tracking server", config.Config.MLflowURL)
	}
	if config.Config.RayURL == "" || config.Config.TsConn == "" {
		t.Errorf("config = %+v, want ray_url and ts_conn set: Operator Lib's own defaults "+
			"are compiled-in cluster names and would silently point elsewhere", config.Config)
	}
	if len(config.InputTopics) != 1 || len(config.InputTopics[0].Mappings) != 1 {
		t.Fatalf("inputTopics = %+v, want the one topic the launch named", config.InputTopics)
	}
	if got := config.InputTopics[0].Mappings[0].Dest; got != "value" {
		t.Errorf("mapping dest = %q, want the name infer() sees", got)
	}

	// The pair that decides the registry key, and so decides that this run trains
	// rather than finding a model already registered.
	if job.RuntimeEnv.EnvVars["PIPELINE_ID"] == "" || job.RuntimeEnv.EnvVars["OPERATOR_ID"] == "" {
		t.Error("the job carries no pipeline/operator pair, so MLOperator has no model id")
	}

	// The platform URLs a job reads its training data from (§5.3.4), so a developer
	// does not restate them in the repository.
	if got := job.RuntimeEnv.EnvVars["SENERGY_TIMESCALE_URL"]; got == "" {
		t.Error("the job was not told where the timeseries store is")
	}
	if job.Entrypoint != "uv run python train.py" {
		t.Errorf("entrypoint = %q, want the deployment default", job.Entrypoint)
	}
	// The workers' interpreter has to match the driver's, or a Ray task starts on
	// the cluster image's python and fails on the first import uv.lock provides.
	if got := job.RuntimeEnv.PyExecutable; got != "uv run" {
		t.Errorf("py_executable = %q, want it to match how the entrypoint starts the driver", got)
	}
}

func TestTheEntrypointCanBeNamed(t *testing.T) {
	h := newHarness(t)
	h.ready()

	h.launch(func(req *experiments.LaunchRequest) {
		req.Entrypoint = "python training.py --folds 5"
	})

	if got := h.ray.LastJob(t).Entrypoint; got != "python training.py --folds 5" {
		t.Errorf("entrypoint = %q", got)
	}
}

// The environment is external input on both routes it can arrive by — an HTTP
// body and an LLM tool call — so it is validated at the boundary rather than
// trusted.
func TestTheLaunchRefusesEnvironmentItWouldHaveToOverride(t *testing.T) {
	h := newHarness(t)
	h.ready()

	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a reserved name", map[string]string{"MLFLOW_RUN_ID": "somebody-elses-run"}, "cannot be overridden"},
		{"the platform token", map[string]string{"SENERGY_TOKEN": "an-injected-token"}, "cannot be overridden"},
		{"an unusable name", map[string]string{"NOT A NAME": "x"}, "not a usable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.service.Launch(context.Background(), experiments.LaunchRequest{
				Request: h.request(), InputTopics: testInputTopics(), EnvVars: tc.env,
			})
			if !errors.Is(err, experiments.ErrInvalidRequest) {
				t.Fatalf("error = %v, want an invalid request", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to say %q", err, tc.want)
			}
		})
	}
	if len(h.ray.Jobs()) != 0 {
		t.Error("a job was submitted despite the refusal")
	}
}

func TestExtraEnvironmentReachesTheJob(t *testing.T) {
	h := newHarness(t)
	h.ready()

	h.launch(func(req *experiments.LaunchRequest) {
		req.EnvVars = map[string]string{"TRAINING_WINDOW_DAYS": "90"}
	})

	if got := h.ray.LastJob(t).RuntimeEnv.EnvVars["TRAINING_WINDOW_DAYS"]; got != "90" {
		t.Errorf("TRAINING_WINDOW_DAYS = %q, want it passed through", got)
	}
}

// The service account of §3.1 item 5 is the one place ODE uses one, and it has to
// actually reach the cluster.
func TestTheRayServiceAccountReachesTheCluster(t *testing.T) {
	h := newHarness(t)
	h.ready()
	h.launch()

	for _, token := range h.ray.Tokens {
		if token == "Bearer ray-service-token" {
			return
		}
	}
	t.Errorf("no request carried the configured Ray credential; saw %v", h.ray.Tokens)
}

func sortedNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	return out
}
