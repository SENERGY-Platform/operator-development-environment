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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo/repotest"
)

// gitStatusText is the working copy as git itself reports it, index included.
func gitStatusText(t *testing.T, checkout string) string {
	t.Helper()
	return repotest.Git(t, checkout, "status", "--porcelain=v2", "--untracked-files=all")
}

// draftProvider is one scripted answer, and a record of what it was asked.
//
// The recorded request is the point of most of these tests: what a draft is worth
// depends entirely on what the model was shown, and that is decided before the
// provider is reached.
type draftProvider struct {
	answer string
	usage  llm.Usage
	fail   error

	mux      sync.Mutex
	requests []llm.Request
}

func (p *draftProvider) Name() string { return "scripted" }

func (p *draftProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{Streaming: true, System: true, Models: []string{"fake-model"}}
}

func (p *draftProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mux.Lock()
	p.requests = append(p.requests, req)
	p.mux.Unlock()

	out := make(chan llm.Event, 3)
	go func() {
		defer close(out)
		if p.fail != nil {
			out <- llm.ErrorEvent(p.fail)
			out <- llm.DoneEvent(llm.StopReasonError, p.usage)
			return
		}
		out <- llm.TextEvent(p.answer)
		out <- llm.DoneEvent("end_turn", p.usage)
	}()
	return out, nil
}

func (p *draftProvider) prompt(t *testing.T) string {
	t.Helper()
	p.mux.Lock()
	defer p.mux.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("the provider was never asked")
	}
	last := p.requests[len(p.requests)-1]
	var text strings.Builder
	for _, message := range last.Messages {
		for _, block := range message.Content {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

// recordingSpend is the §3.3 administration, watched.
type recordingSpend struct {
	refuse   error
	mux      sync.Mutex
	recorded []llm.Usage
}

func (s *recordingSpend) AllowSpend(context.Context, string) error { return s.refuse }

func (s *recordingSpend) CheckProviderModel(context.Context, string, string, string) error {
	return nil
}

func (s *recordingSpend) RecordUsage(_ context.Context, _, _ string, usage llm.Usage) {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.recorded = append(s.recorded, usage)
}

func (s *recordingSpend) count() int {
	s.mux.Lock()
	defer s.mux.Unlock()
	return len(s.recorded)
}

// useDrafts installs a scripted provider on the harness's service.
func (h *harness) useDrafts(provider llm.Provider, spend repo.Spend) {
	h.t.Helper()
	providers, err := llm.NewRegistry(provider)
	if err != nil {
		h.t.Fatalf("llm.NewRegistry: %v", err)
	}
	h.service.UseDrafts(repo.DraftDeps{Providers: providers, Spend: spend})
}

// The draft is written from the diff, and the diff is the whole point: a draft
// produced from a list of filenames would be a sentence the developer rewrites
// every time.
//
// The untracked file is in here deliberately. `git diff` says nothing whatsoever
// about a file git has never seen, and a new package is the normal shape of
// starting an operator — so a draft that only ever saw tracked changes would be
// blind exactly when the developer needs it most.
func TestACommitMessageDraftSeesTheDiffAndTheNewFiles(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore(scaffold): lay out the operator")

	// A tracked file changed, and a file git has never seen.
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "operator.yaml"),
		[]byte("name: pv-forecast\nversion: 2\n"), 0o644); err != nil {
		t.Fatalf("write operator.yaml: %v", err)
	}
	if err := os.MkdirAll(h.path("jonah", "pv-forecast", "pv"), 0o755); err != nil {
		t.Fatalf("mkdir pv: %v", err)
	}
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "pv", "irradiance.py"),
		[]byte("def clearsky(latitude, longitude):\n    return 0.0\n"), 0o644); err != nil {
		t.Fatalf("write irradiance.py: %v", err)
	}

	provider := &draftProvider{
		answer: "feat(pv): derive clearsky irradiance from the site position\n\n" +
			"The forecast needs an upper bound to normalise against.",
		usage: llm.Usage{InputTokens: 900, OutputTokens: 30, Provider: "scripted", Model: "fake-model"},
	}
	spend := &recordingSpend{}
	h.useDrafts(provider, spend)

	draft, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("DraftCommitMessage: %v", err)
	}
	if draft.Message != provider.answer {
		t.Errorf("message = %q, want the model's answer", draft.Message)
	}
	if draft.Files != 2 {
		t.Errorf("files = %d, want the two changed paths", draft.Files)
	}
	if draft.Committed {
		t.Error("the draft reports itself as committed, which is the one thing it must not do")
	}
	if draft.Provider != "scripted" || draft.Model != "fake-model" {
		t.Errorf("draft = %+v, want the provider and model that answered", draft)
	}

	prompt := provider.prompt(t)
	for _, want := range []string{
		// The status, in git's own shape.
		"operator.yaml",
		"?? pv/irradiance.py",
		// The tracked change as a diff, not as a filename.
		"version: 2",
		// And the untracked file's content, which no diff would have carried.
		"def clearsky(latitude, longitude):",
		// The repository's own recent subject, as the style to follow.
		"chore(scaffold): lay out the operator",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}

	// Metered like any other provider request (§3.3).
	if spend.count() != 1 {
		t.Errorf("recorded %d usages, want the one the draft cost", spend.count())
	}

	// And nothing happened to the working copy: still dirty, still uncommitted,
	// still on the same commit.
	status, err := h.service.Status(context.Background(), repo.StatusRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Dirty || len(status.Changes) != 2 {
		t.Errorf("status = %+v, want the two changes still uncommitted", status.Changes)
	}
	if status.HeadSubject != "chore(scaffold): lay out the operator" {
		t.Errorf("head subject = %q, want the draft to have committed nothing", status.HeadSubject)
	}
}

// A draft narrowed to paths must describe those paths, because the commit beside
// it would record those paths. A draft written from everything, staged against a
// partial commit, is a message that says the wrong thing in the history forever.
func TestADraftNarrowedToPathsIgnoresTheRest(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")

	for name, content := range map[string]string{
		"training.py": "epochs = 50\n",
		"secrets.txt": "nothing to see here\n",
	} {
		if err := os.WriteFile(h.path("jonah", "pv-forecast", name),
			[]byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	provider := &draftProvider{answer: "feat(training): train for fifty epochs"}
	h.useDrafts(provider, nil)

	draft, err := h.service.DraftCommitMessage(context.Background(), repo.DraftRequest{
		Request: h.request(), Paths: []string{"training.py"},
	})
	if err != nil {
		t.Fatalf("DraftCommitMessage: %v", err)
	}
	if draft.Files != 1 {
		t.Errorf("files = %d, want only the named path", draft.Files)
	}
	prompt := provider.prompt(t)
	if !strings.Contains(prompt, "epochs = 50") {
		t.Errorf("the prompt is missing the named path's content:\n%s", prompt)
	}
	if strings.Contains(prompt, "secrets.txt") {
		t.Errorf("the prompt carries a path the developer did not ask about:\n%s", prompt)
	}
}

// The two refusals that are answers rather than failures, and the one that is a
// deployment fact.
func TestADraftRefusesWithoutProviderChangesOrAllowance(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")

	// No provider: a deployment served without one is a supported deployment, and
	// the answer is that the developer writes the message themselves.
	if _, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()}); !errors.Is(err, repo.ErrDraftsUnavailable) {
		t.Errorf("err = %v, want ErrDraftsUnavailable", err)
	}

	// A clean tree: nothing to describe, and the same value Commit answers with, so
	// the pane has one case to render rather than two.
	provider := &draftProvider{answer: "feat: something"}
	h.useDrafts(provider, nil)
	if _, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()}); !errors.Is(err, repo.ErrNothingToCommit) {
		t.Errorf("err = %v, want ErrNothingToCommit on a clean tree", err)
	}

	// At a spend cap: refused before the pod is touched at all, and the provider is
	// never reached.
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "training.py"),
		[]byte("epochs = 50\n"), 0o644); err != nil {
		t.Fatalf("write training.py: %v", err)
	}
	capped := errors.New("token cap reached")
	h.useDrafts(provider, &recordingSpend{refuse: capped})
	if _, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()}); !errors.Is(err, capped) {
		t.Errorf("err = %v, want the spend refusal", err)
	}
	provider.mux.Lock()
	asked := len(provider.requests)
	provider.mux.Unlock()
	if asked != 0 {
		t.Errorf("the provider was asked %d times behind a spend cap", asked)
	}
}

// A turn that failed was still billed: the provider read the input it was sent.
// Recording nothing for it would make a failing draft a free way past §3.3.
func TestAFailedDraftIsStillAccountedFor(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "training.py"),
		[]byte("epochs = 50\n"), 0o644); err != nil {
		t.Fatalf("write training.py: %v", err)
	}

	provider := &draftProvider{
		fail:  errors.New("the provider is out of capacity"),
		usage: llm.Usage{InputTokens: 900, Provider: "scripted", Model: "fake-model"},
	}
	spend := &recordingSpend{}
	h.useDrafts(provider, spend)

	if _, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()}); err == nil {
		t.Fatal("a failed provider call produced no error")
	}
	if spend.count() != 1 {
		t.Errorf("recorded %d usages, want the input the failed turn still cost", spend.count())
	}
}

// The bound is applied before the request goes out, not after it comes back with a
// bill: a developer who has been working for a day can have a diff larger than the
// context window.
func TestALargeDiffIsCutBeforeItIsSent(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")

	var big strings.Builder
	for line := 0; line < 4000; line++ {
		big.WriteString("measurement = 1.0  # a line of a very long change\n")
	}
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "training.py"),
		[]byte(big.String()), 0o644); err != nil {
		t.Fatalf("write training.py: %v", err)
	}

	provider := &draftProvider{answer: "feat(training): record every measurement"}
	providers, err := llm.NewRegistry(provider)
	if err != nil {
		t.Fatalf("llm.NewRegistry: %v", err)
	}
	h.service.UseDrafts(repo.DraftDeps{Providers: providers, MaxDiffBytes: 2048})

	draft, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("DraftCommitMessage: %v", err)
	}
	if !draft.Truncated {
		t.Error("the draft does not say the diff was cut, so a developer would trust the body more than they should")
	}
	prompt := provider.prompt(t)
	if len(prompt) > 16<<10 {
		t.Errorf("the prompt is %d bytes, want the diff bound to have applied", len(prompt))
	}
	if !strings.Contains(prompt, "[cut here]") {
		t.Errorf("the prompt does not say where it was cut:\n%s", prompt[:min(len(prompt), 2000)])
	}
}

// What a model puts around a commit message when it was asked not to. A fence that
// reached the history would be there forever.
func TestADraftIsStrippedOfFencesAndBlankRuns(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "training.py"),
		[]byte("epochs = 50\n"), 0o644); err != nil {
		t.Fatalf("write training.py: %v", err)
	}

	provider := &draftProvider{answer: "```\nfeat(training): train for fifty epochs   \n\n\n" +
		"Fifty is where the validation loss stops falling.\n```\n"}
	h.useDrafts(provider, nil)

	draft, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("DraftCommitMessage: %v", err)
	}
	want := "feat(training): train for fifty epochs\n\n" +
		"Fifty is where the validation loss stops falling."
	if draft.Message != want {
		t.Errorf("message = %q, want %q", draft.Message, want)
	}
}

// The first commit has no HEAD to diff against, and every path in it is new. A
// draft that tried to diff against HEAD anyway would answer with an empty prompt
// on exactly the commit that most needs a message.
func TestADraftOnAnUnbornBranchDescribesTheNewFiles(t *testing.T) {
	h := newHarness(t)
	h.connect()
	if _, err := h.service.Create(context.Background(), repo.CreateRequest{
		Request: h.request(), Name: "pv-forecast", Scaffold: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	provider := &draftProvider{answer: "chore(scaffold): lay out the operator"}
	h.useDrafts(provider, nil)

	draft, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("DraftCommitMessage: %v", err)
	}
	if draft.Files == 0 {
		t.Error("the draft saw no files on an unborn branch")
	}
	prompt := provider.prompt(t)
	if !strings.Contains(prompt, "no commits yet") {
		t.Errorf("the prompt does not say this is the first commit:\n%s", prompt)
	}
	if !strings.Contains(prompt, "operator.yaml") {
		t.Errorf("the prompt does not name the scaffolded files:\n%s", prompt)
	}
}

// Belt and braces on the promise the package comment makes: a draft leaves the
// index exactly as it found it. Reading a working copy must not stage anything,
// and `git add --intent-to-add` — the tempting way to diff an untracked file —
// would have.
func TestADraftLeavesTheIndexAlone(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.createAndCommit(t, "pv-forecast", "chore: scaffold")
	if err := os.WriteFile(h.path("jonah", "pv-forecast", "training.py"),
		[]byte("epochs = 50\n"), 0o644); err != nil {
		t.Fatalf("write training.py: %v", err)
	}

	checkout := filepath.Join(h.workspace, "jonah", "pv-forecast")
	before := gitStatusText(t, checkout)

	h.useDrafts(&draftProvider{answer: "feat(training): train for fifty epochs"}, nil)
	if _, err := h.service.DraftCommitMessage(context.Background(),
		repo.DraftRequest{Request: h.request()}); err != nil {
		t.Fatalf("DraftCommitMessage: %v", err)
	}

	if after := gitStatusText(t, checkout); after != before {
		t.Errorf("the draft changed the status:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
