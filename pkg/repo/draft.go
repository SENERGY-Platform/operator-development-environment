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

// A commit message, drafted.
//
// This is the one place in the repository surface where the model is asked
// something, and it is worth being precise about what that does and does not
// mean. Drafting reads the diff and returns text. It does not stage, commit or
// push, it does not write into the working copy, and it is reached from a route of
// its own rather than from Commit — so §5.11 item 5 still holds exactly as
// written: no path in ODE commits without a developer asking, and there is no LLM
// tool that could. The draft lands in a text box the developer edits and can
// discard, which is the same standing as a suggestion in the chat pane.
//
// Three consequences of that framing show up in the code below:
//
//   - The provider call goes through the same §3.3 gate the chat engine uses, and
//     records its usage the same way. A draft is a provider request like any
//     other, and a spend cap that a second entry point walked past would not be a
//     cap.
//   - The diff is bounded before it is sent, not after. A developer who has been
//     working for a day can have a diff larger than the context window, and the
//     failure mode of finding that out at the provider is a 400 with a bill.
//   - The style examples come from the developer's *own* repository's recent
//     subjects, not from a convention this file invents. A draft that ignores how
//     the repository already writes its history is one they rewrite by hand.

package repo

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
)

// Spend is the §3.3 administration a provider call is subject to. *admin.Service
// implements it; declared here so the dependency points one way, as Limits does.
//
// Every method takes and returns nothing this package would have to import
// pkg/admin for, which is the reason AllowSpend exists over there: Check's verdict
// is of no use to a draft, and naming it here would drag the whole limits model
// into the repository surface.
type Spend interface {
	// AllowSpend refuses when this user is at a token or cost cap.
	AllowSpend(ctx context.Context, userSub string) error
	// CheckProviderModel refuses a provider or model this user may not use.
	CheckProviderModel(ctx context.Context, userSub, provider, model string) error
	// RecordUsage books what the call cost. The session id is empty here: a draft
	// belongs to no conversation, which the usage table's default for the column
	// already anticipates.
	RecordUsage(ctx context.Context, userSub, sessionID string, usage llm.Usage)
}

// DraftDeps is the LLM side of a commit message draft.
//
// Installed after construction by UseDrafts rather than taken in Deps, for the
// reason UseLimits gives: the provider registry is built inside the LLM surface's
// wiring, which runs after this service exists. A deployment with no provider
// configured simply never calls UseDrafts, and the route answers "unavailable"
// instead of failing.
type DraftDeps struct {
	// Providers resolves the provider a draft is asked of. Nil disables drafting.
	Providers *llm.Registry
	// Spend is the per-user administration of §3.3. Nil leaves a deployment
	// without an admin surface drafting unmetered, which is the same bargain the
	// chat engine strikes there.
	Spend Spend
	// Provider names which of the registered providers to use. Empty takes the
	// registry's default, which is the first one registered.
	Provider string
	// Model names the model. Empty takes the provider's default, and is refused by
	// a provider whose capabilities say a model is required.
	Model string
	// MaxTokens bounds the answer. A commit message is short; the bound is here so
	// a model that starts explaining itself costs a paragraph and not a page.
	MaxTokens int
	// MaxDiffBytes bounds the diff that is sent. Zero takes the default.
	MaxDiffBytes int
	// MaxNewFileBytes bounds how much of one untracked file is sent.
	MaxNewFileBytes int
	// MaxNewFiles bounds how many untracked files are read at all.
	MaxNewFiles int
}

const (
	defaultDraftMaxTokens       = 1024
	defaultDraftMaxDiffBytes    = 24 << 10
	defaultDraftMaxNewFileBytes = 4 << 10
	defaultDraftMaxNewFiles     = 20
	// draftLogEntries is how many recent subjects go in as style examples. Ten is
	// enough to show a convention — conventional commits, a ticket prefix, German
	// or English — and short enough not to crowd out the diff.
	draftLogEntries = 10
)

// UseDrafts installs the LLM side of the draft. Nil-safe both ways: called with a
// zero DraftDeps, or not called at all, drafting stays unavailable.
func (s *Service) UseDrafts(deps DraftDeps) {
	if deps.MaxTokens <= 0 {
		deps.MaxTokens = defaultDraftMaxTokens
	}
	if deps.MaxDiffBytes <= 0 {
		deps.MaxDiffBytes = defaultDraftMaxDiffBytes
	}
	if deps.MaxNewFileBytes <= 0 {
		deps.MaxNewFileBytes = defaultDraftMaxNewFileBytes
	}
	if deps.MaxNewFiles <= 0 {
		deps.MaxNewFiles = defaultDraftMaxNewFiles
	}
	s.drafts = deps
}

// DraftsAvailable says whether this deployment can draft a message, so the SPA can
// leave the button out rather than offer one that answers 503.
func (s *Service) DraftsAvailable() bool { return s.drafts.Providers != nil }

// DraftRequest is one draft.
type DraftRequest struct {
	Request
	// Paths narrows the draft to these, relative to the checkout. Empty describes
	// everything uncommitted, which is what the pane's commit box asks for — the
	// draft then describes exactly what the commit beside it would record.
	Paths []string
}

// Draft is a proposed commit message. Nothing has happened to the working copy.
type Draft struct {
	// Message is the draft: a subject line, a blank line, and a short body.
	Message string `json:"message"`
	// Files is how many changed files the draft was written from.
	Files int `json:"files"`
	// Truncated says the diff did not fit the budget and was cut. Reported because
	// it changes how much a developer should trust the body.
	Truncated bool   `json:"truncated,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	// Committed is always false, and is reported for the reason WriteResult's is:
	// it is the property that matters, and a caller that assumed otherwise would
	// tell the developer their work is saved.
	Committed bool `json:"committed"`
}

const draftSystemPrompt = `You write one git commit message for a developer's uncommitted work.

You are given the recent commit subjects of the same repository, the output of git status, the diff, and the content of files git does not track yet.

Rules:
- Answer with the commit message and nothing else. No code fences, no preamble, no closing remark, no offer to change it.
- First line: a subject under 72 characters, in the imperative mood, saying what the change does. Follow the style of the recent subjects you were given — if they use a prefix such as feat(scope): or a ticket id, use it too; if they do not, do not invent one.
- Then a blank line, then at most three short lines of body saying why the change was made and anything a reader of the history would not see in the diff. Omit the body entirely when the subject already says everything.
- Describe what the diff shows. Do not guess at intent the diff does not support, do not list every file, and do not mention yourself or that you are an assistant.`

// DraftCommitMessage asks the model for a commit message for the uncommitted work.
//
// It changes nothing: the working copy, the index and the remote are all untouched,
// and the answer is text the developer edits or throws away.
func (s *Service) DraftCommitMessage(ctx context.Context, req DraftRequest) (Draft, error) {
	if s.drafts.Providers == nil {
		return Draft{}, ErrDraftsUnavailable
	}
	provider, err := s.drafts.Providers.Get(s.drafts.Provider)
	if err != nil {
		return Draft{}, err
	}
	model := s.drafts.Model
	if model == "" && provider.Capabilities().ModelRequired {
		if models := provider.Capabilities().Models; len(models) > 0 {
			model = models[0]
		}
	}
	// Both gates before anything is read from the pod: a user at their cap should
	// hear so without ODE first spending a round trip per git command on their
	// behalf.
	if s.drafts.Spend != nil {
		if err := s.drafts.Spend.AllowSpend(ctx, req.UserSub); err != nil {
			return Draft{}, err
		}
		if err := s.drafts.Spend.CheckProviderModel(ctx, req.UserSub, provider.Name(), model); err != nil {
			return Draft{}, err
		}
	}

	link, _, checkout, err := s.checkoutFor(ctx, req.Request)
	if err != nil {
		return Draft{}, err
	}
	// Deliberately no verifyRemote here, unlike Commit and Push. A checkout whose
	// origin is not the selected repository is a problem for a write, and a draft
	// is not one — refusing to describe the developer's own diff would be a
	// refusal with nothing behind it.

	material, err := s.draftMaterial(ctx, req, link, checkout)
	if err != nil {
		return Draft{}, err
	}
	if material.files == 0 {
		return Draft{}, ErrNothingToCommit
	}

	answer, err := llm.Text(ctx, provider, llm.Request{
		Model:     model,
		System:    draftSystemPrompt,
		Messages:  []llm.Message{llm.UserText(material.prompt)},
		MaxTokens: s.drafts.MaxTokens,
	})
	// Recorded before the error is returned, and detached from nothing: a turn that
	// failed after the provider read its input was still billed, and §3.3's caps
	// are computed from what is recorded.
	if s.drafts.Spend != nil {
		s.drafts.Spend.RecordUsage(ctx, req.UserSub, "", answer.Usage)
	}
	if err != nil {
		return Draft{}, fmt.Errorf("drafting a commit message: %w", err)
	}
	message := cleanDraft(answer.Text)
	if message == "" {
		return Draft{}, fmt.Errorf("%w: the model answered nothing", ErrDraftFailed)
	}

	return Draft{
		Message:   message,
		Files:     material.files,
		Truncated: material.truncated,
		Provider:  provider.Name(),
		Model:     answer.Usage.Model,
	}, nil
}

// draftMaterial is what the model is shown.
type draftMaterial struct {
	prompt    string
	files     int
	truncated bool
}

// draftMaterial reads the working copy: the recent subjects, the status, the diff
// against HEAD, and the content of what git does not track yet.
//
// The untracked files are the reason this is more than one `git diff`. A diff
// against HEAD says nothing at all about a file git has never seen, and a
// developer whose whole change is a new package — which is the normal shape of
// starting an operator — would otherwise get a draft written from a list of
// filenames.
func (s *Service) draftMaterial(
	ctx context.Context, req DraftRequest, link Link, checkout gitContext,
) (draftMaterial, error) {
	raw, err := s.rawStatus(ctx, checkout)
	if err != nil {
		return draftMaterial{}, err
	}
	changes := raw.Changes
	if len(req.Paths) > 0 {
		wanted := make(map[string]bool, len(req.Paths))
		for _, requested := range req.Paths {
			clean, err := relativePath(requested)
			if err != nil {
				return draftMaterial{}, err
			}
			wanted[clean] = true
		}
		kept := make([]Change, 0, len(changes))
		for _, change := range changes {
			if wanted[change.Path] {
				kept = append(kept, change)
			}
		}
		changes = kept
	}
	if len(changes) == 0 {
		return draftMaterial{}, nil
	}

	var sections []string

	if !raw.Unborn {
		if result, err := checkout.attempt(ctx,
			"log", fmt.Sprintf("-%d", draftLogEntries), "--pretty=format:%s"); err == nil &&
			result.ExitCode == 0 && strings.TrimSpace(result.Stdout) != "" {
			sections = append(sections,
				"Recent commit subjects in this repository, newest first:\n"+
					strings.TrimSpace(result.Stdout))
		}
	} else {
		sections = append(sections,
			"This repository has no commits yet: this would be the first one.")
	}

	sections = append(sections, "git status:\n"+statusLines(changes))

	// Against HEAD rather than the two halves separately: what the commit beside
	// this draft would record is the whole difference from the last commit, whether
	// or not the developer happened to stage part of it. On an unborn branch there
	// is no HEAD to diff against, and everything is either untracked or staged as
	// new — the second of which --cached still shows.
	diffArgs := []string{"diff", "--unified=3"}
	if raw.Unborn {
		diffArgs = append(diffArgs, "--cached")
	} else {
		diffArgs = append(diffArgs, "HEAD")
	}
	diffArgs = append(diffArgs, "--")
	for _, change := range changes {
		if change.Kind != "untracked" {
			diffArgs = append(diffArgs, change.Path)
		}
	}
	material := draftMaterial{files: len(changes)}
	// attempt, not run: a diff that git refuses — a path that has vanished under
	// the developer's own editor between the status and this command — should cost
	// the draft its diff, not the whole answer.
	if result, err := checkout.attempt(ctx, diffArgs...); err == nil && result.ExitCode == 0 {
		diff, cut := clip(result.Stdout, s.drafts.MaxDiffBytes)
		material.truncated = material.truncated || cut || result.Truncated
		if strings.TrimSpace(diff) != "" {
			sections = append(sections, "Diff against the last commit:\n"+diff)
		}
	}

	if newFiles, cut := s.draftNewFiles(ctx, req, link, changes); newFiles != "" {
		material.truncated = material.truncated || cut
		sections = append(sections,
			"Files git does not track yet, with their content:\n"+newFiles)
	}

	if material.truncated {
		sections = append(sections,
			"Some of the above was cut to fit. Write the message from what you were given.")
	}
	material.prompt = strings.Join(sections, "\n\n")
	return material, nil
}

// draftNewFiles reads the untracked files, bounded twice: how many, and how much
// of each.
func (s *Service) draftNewFiles(
	ctx context.Context, req DraftRequest, link Link, changes []Change,
) (string, bool) {
	var (
		builder strings.Builder
		read    int
		cut     bool
	)
	for _, change := range changes {
		if change.Kind != "untracked" {
			continue
		}
		if read >= s.drafts.MaxNewFiles {
			cut = true
			break
		}
		read++
		content, err := s.workspace.ReadFile(ctx, s.ref(req.Request, link),
			path.Join(link.Path, change.Path), s.drafts.MaxNewFileBytes)
		if err != nil {
			// A file the pod cannot read is still worth naming: it is part of the
			// change, and the model can say so from the path alone.
			fmt.Fprintf(&builder, "--- %s (could not be read) ---\n", change.Path)
			continue
		}
		if content.Binary {
			fmt.Fprintf(&builder, "--- %s (binary, %d bytes) ---\n", change.Path, content.Size)
			continue
		}
		text, clipped := clip(content.Text, s.drafts.MaxNewFileBytes)
		cut = cut || clipped || content.Truncated
		fmt.Fprintf(&builder, "--- %s ---\n%s\n", change.Path, text)
	}
	return builder.String(), cut
}

// statusLines is the change list in git's own short shape, which is the form the
// model has seen most of.
func statusLines(changes []Change) string {
	var builder strings.Builder
	for _, change := range changes {
		code := "??"
		if change.Kind != "untracked" {
			code = fmt.Sprintf("%s%s",
				stagedCode(change.Staged, change.Kind), stagedCode(change.Unstaged, change.Kind))
		}
		if change.RenamedFrom != "" {
			fmt.Fprintf(&builder, "%s %s -> %s\n", code, change.RenamedFrom, change.Path)
			continue
		}
		fmt.Fprintf(&builder, "%s %s\n", code, change.Path)
	}
	return strings.TrimRight(builder.String(), "\n")
}

func stagedCode(set bool, kind string) string {
	if !set {
		return " "
	}
	switch kind {
	case "added":
		return "A"
	case "deleted":
		return "D"
	case "renamed":
		return "R"
	case "copied":
		return "C"
	case "typechange":
		return "T"
	case "unmerged":
		return "U"
	default:
		return "M"
	}
}

// clip cuts text to a byte budget on a line boundary, so the last thing the model
// sees is a whole line rather than half a hunk header.
func clip(text string, budget int) (string, bool) {
	if budget <= 0 || len(text) <= budget {
		return text, false
	}
	cut := text[:budget]
	if index := strings.LastIndexByte(cut, '\n'); index > 0 {
		cut = cut[:index]
	}
	return cut + "\n[cut here]", true
}

// cleanDraft removes what a model adds around a commit message however clearly it
// was asked not to: a code fence, a "Here is..." line is not removed — that would
// need guessing — but fences and surrounding blank lines are, because they are
// unambiguous and would otherwise end up in the history.
func cleanDraft(text string) string {
	message := strings.TrimSpace(text)
	if strings.HasPrefix(message, "```") {
		if _, rest, found := strings.Cut(message, "\n"); found {
			message = rest
		}
		if index := strings.LastIndex(message, "```"); index >= 0 {
			message = message[:index]
		}
	}
	// Trailing whitespace per line, and never more than one blank line in a row:
	// git strips neither, and both look like sloppiness in a log.
	lines := strings.Split(strings.TrimSpace(message), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if line == "" && len(kept) > 0 && kept[len(kept)-1] == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
