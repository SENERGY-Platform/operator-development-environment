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

package tools

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/plaincode"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// ---- list_files and read_file (L0, no confirmation) ----
//
// Both sit at L0 without a confirmation on the same argument write_file does, one
// step weaker: the working copy is the developer's own code on their own storage,
// it carries no platform data, and reading it is strictly less than the write
// below already permits. What makes the pair worth having rather than merely
// permissible is what the model did without them. The only way to see the operator
// was a `run_code` cell that opened the file — a confirmation each time, for
// `print(open(p).read())`, and a habit of answering confirmations without reading
// them. Measured over four days of one developer's sessions: 195 of 241 cells they
// were asked to confirm ran a subprocess, an import or a shell escape, and a large
// share of those were doing nothing a file read would not have done.
//
// The two refusals are the interesting part, and they are opposites on purpose.
// read_file refuses a credential path, because its answer goes into a conversation
// that is stored and nobody is asked first. list_files refuses nothing at all: a
// listing says a file exists, which is what the Code pane's own tree says (D14),
// and hiding a name would leave a model proposing changes to a repository it has
// been shown an edited picture of.
//
// A third case sits between the two and is neither: read_file renders rather than
// refuses evaluation.yaml. Refusing it outright would deny the model the metric
// and the threshold it needs to reason about a run, for the sake of the one field
// — target_series — that would hand it the answer to a choice it is meant to make
// itself (§5.2, D38). So the file is read like any other, and before the text
// ever reaches the window logic below, experiments.RenderForAssistant replaces it
// with ODE's own reading of the parsed document — the developer's bytes, minus
// that one field — instead of searching those bytes for the value and trying to
// blank it in place. write_file's own refusal of this path, further down, is
// unaffected — the two tools make different promises about the same file, not
// the same one twice.

// maxListedFiles bounds one listing.
//
// pkg/repo bounds the pane's tree at 4000 entries, which is right for a pane and
// far past what belongs in a model's context: an operator repository has tens of
// files, and a listing long enough to need scrolling is one the model will read
// the top of and treat as complete. What is dropped is reported.
const maxListedFiles = 400

// ListFilesResult is what the model reads back.
type ListFilesResult struct {
	// Repository is the checkout the paths are relative to, so a model that has
	// been in two repositories this session can tell which one answered.
	Repository string       `json:"repository"`
	Files      []ListedFile `json:"files"`
	// Count is how many files the walk found, which is more than the length of
	// Files when the listing was cut. Read together with Truncated: the two of them
	// are what say "this is not the whole repository".
	Count int `json:"count"`
	// Excluded names what the walk did not enter — `.git`, and nothing else.
	Excluded  []string `json:"excluded,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Hint      string   `json:"hint,omitempty"`
}

// ListedFile is one file. Directories are not listed: git does not track an empty
// one, and every directory that holds a file is already spelled out in that file's
// own path.
type ListedFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (s *surface) listFiles(ctx context.Context, req Request) (any, error) {
	req.Progress("repo", "listing the working copy")
	tree, err := s.deps.Repo.Files(ctx, repo.Request{
		Bearer:      req.Token,
		UserSub:     req.UserSub,
		WorkbenchID: req.WorkbenchID,
	})
	if err != nil {
		return nil, err
	}

	result := ListFilesResult{
		Repository: tree.Root,
		Files:      []ListedFile{},
		Excluded:   tree.Excluded,
	}
	// Reported from the walk as well as from this cap: a directory the walk itself
	// elided is a gap in the answer, and one that only this function trimmed is a
	// different gap. Both make the listing incomplete, which is the fact the model
	// needs.
	elided := false
	flattenTree(tree.Tree, tree.Root, &result.Files, &elided)
	result.Count = len(result.Files)
	if len(result.Files) > maxListedFiles {
		result.Files = result.Files[:maxListedFiles]
		result.Truncated = true
	}
	if elided {
		result.Truncated = true
	}
	if result.Truncated {
		result.Hint = fmt.Sprintf(
			"this listing is incomplete: %d files are shown of %d found, and the walk itself "+
				"may have stopped early. Ask the developer which part of the repository matters "+
				"rather than assuming these are all the files",
			len(result.Files), result.Count)
	}
	return result, nil
}

// flattenTree collects the files of a walked tree as repository-relative paths.
//
// kernel.Node carries workspace-relative paths — `owner/repo/op.py` — because the
// walk is over the workspace. The model is given repository-relative ones, the
// same shape read_file and write_file take, so the checkout directory is trimmed
// rather than passed on: a model that learned the workspace path would eventually
// send one to write_file, which refuses an absolute path and would refuse this too.
func flattenTree(node kernel.Node, root string, into *[]ListedFile, elided *bool) {
	if node.Elided > 0 {
		*elided = true
	}
	if node.Type == "file" {
		relative := strings.TrimPrefix(strings.TrimPrefix(node.Path, root), "/")
		if relative == "" {
			relative = node.Name
		}
		*into = append(*into, ListedFile{Path: relative, Size: node.Size})
	}
	for _, child := range node.Children {
		flattenTree(child, root, into, elided)
	}
}

type readFileInput struct {
	Path     string `json:"path"`
	FromLine int    `json:"from_line"`
	MaxLines int    `json:"max_lines"`
}

// ReadFileResult is one window of one file.
//
// The line figures are the load-bearing part rather than decoration: a model that
// read the first eighty lines of a two-hundred-line module and proposed a rewrite
// of "the file" would be rewriting a file it has not seen, and total_lines with the
// next from_line is what stops that being invisible.
type ReadFileResult struct {
	Path     string `json:"path"`
	Language string `json:"language,omitempty"`
	Text     string `json:"text"`
	// FromLine and Lines describe the window, 1-based and inclusive.
	FromLine   int `json:"from_line"`
	Lines      int `json:"lines"`
	TotalLines int `json:"total_lines"`
	// Size is the whole file on disk, so a window can be told from a small file.
	Size     int64  `json:"size"`
	Modified string `json:"modified,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	// Truncated says this answer is not the rest of the file, whether because the
	// byte budget ran out here or because pkg/repo had already cut the read.
	Truncated bool `json:"truncated,omitempty"`
	// Withheld names the keys whose value this read does not carry — today always
	// either absent or exactly ["target_series"], the one evaluation.yaml field
	// RenderForAssistant leaves out of every document it writes (D38). Set
	// together with Rendered below and for the same reason: once the text is
	// ODE's own reading of the file rather than its bytes, target_series is never
	// in it, whether or not the file names one. Names rather than a count, unlike
	// WithheldMetrics on the run summary (experiments.go): a metric's name is
	// itself information about a run and D37 keeps it out, but target_series is
	// the scaffold's own key (pkg/repo/scaffold.go) — naming it here costs nothing
	// the file does not already show in the Code pane.
	Withheld []string `json:"withheld,omitempty"`
	// Rendered says this text is ODE's own reading of evaluation.yaml — the
	// parsed CriteriaDocument written back out — rather than the file's bytes, so
	// Size above and the length of Text disagree on purpose. Only evaluation.yaml
	// ever sets this; every other file's Text is byte-identical to disk.
	Rendered bool   `json:"rendered,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

func (s *surface) readFile(ctx context.Context, req Request) (any, error) {
	var in readFileInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	requested := strings.TrimSpace(in.Path)
	if requested == "" {
		return nil, fmt.Errorf("%w: path is required", ErrInvalidInput)
	}
	// Before the read rather than after it: the point is that the contents never
	// reach the conversation, and a check on the way out would already have them in
	// memory beside a result the caller might log.
	if component, found := plaincode.CredentialPath(requested); found {
		return nil, fmt.Errorf(
			"%w: %q names %s, whose contents are a credential, and this answer would be "+
				"stored in the conversation. Ask the developer what you need from it instead",
			ErrInvalidInput, requested, component)
	}

	req.Progress("repo", "reading "+requested)
	file, err := s.deps.Repo.ReadFile(ctx, repo.Request{
		Bearer:      req.Token,
		UserSub:     req.UserSub,
		WorkbenchID: req.WorkbenchID,
	}, requested)
	if err != nil {
		return nil, err
	}

	result := ReadFileResult{
		Path:     file.Path,
		Language: file.Language,
		Size:     file.Size,
		Modified: file.Modified,
		Binary:   file.Binary,
		FromLine: 1,
	}
	if file.Binary {
		result.Hint = "this file is not text, so there is nothing to read; its size is above"
		return result, nil
	}

	text := file.Text
	if strings.EqualFold(path.Base(path.Clean(requested)), evaluationCriteria) {
		// Before the line cut below, on the same argument as the credential check
		// above it: the value has to be gone from the text itself, not trimmed off a
		// window that happened to include it.
		rendered, err := experiments.RenderForAssistant(text)
		if err != nil {
			// err is deliberately not part of this message. Every parse failure
			// yamlsubset.go returns quotes the raw line that stopped it with %q
			// (":325", ":335", ":460", ":496", ":513"), ParseCriteria passes that error
			// through unchanged, and execute (pkg/tools/dispatch.go:414) would put
			// whatever this function returns here into Failure{Error: err.Error()} for
			// the model to read. A criteria file that fails to parse on the very line a
			// developer meant to keep from the model — a target_series line missing its
			// colon, say — would then hand the model that line back inside the refusal
			// that was supposed to withhold it. So the refusal below is fixed text: the
			// developer still sees the file exactly as it is in the Code pane, and fixes
			// it there.
			return nil, fmt.Errorf(
				"%w: %s could not be shown safely: it does not parse well enough for ODE "+
					"to render its own reading of it, and this tool will not fall back to "+
					"showing the file as it is on disk. Ask the developer to fix it; they "+
					"see it unchanged in the Code pane",
				ErrInvalidInput, evaluationCriteria)
		}
		text = rendered
		result.Rendered = true
		result.Withheld = []string{"target_series"}
	}

	lines, trailingNewline := splitLines(text)
	result.TotalLines = len(lines)
	if len(lines) == 0 {
		// An empty file is an ordinary file — `__init__.py` is empty in most Python
		// packages, and the scaffold writes one. It answers as itself rather than as
		// the past-the-end error below, which would tell the model the file is missing.
		result.Hint = "this file is empty"
		return result, nil
	}

	window, from, cut, pastEnd := windowLines(lines, in.FromLine, in.MaxLines, s.deps.RepoMaxReadBytes)
	if pastEnd {
		// An error rather than an empty window, because the two would read the same
		// to a model — "nothing there" — and only one of them is true.
		return nil, fmt.Errorf("%w: from_line %d is past the end of %s, which has %d lines",
			ErrInvalidInput, from, file.Path, len(lines))
	}
	result.FromLine = from
	if cut {
		result.Truncated = true
	}

	result.Lines = len(window)
	result.Text = strings.Join(window, "\n")
	// A window that reaches the last line reproduces the file's own ending, so a
	// full read is byte-identical to the file and can go back through write_file
	// unchanged — for every path but evaluationCriteria: that one is ODE's own
	// rendering of the parsed document rather than the file's bytes at all, and
	// write_file refuses the path outright anyway, so there was never a round
	// trip through write_file to keep for it.
	if trailingNewline && from+len(window)-1 == len(lines) {
		result.Text += "\n"
	}
	if file.Truncated {
		// pkg/repo had already cut the file at the pane's ceiling, so even the last
		// line of the last window is not the end of the file.
		result.Truncated = true
	}
	if result.Truncated {
		next := from + len(window)
		if next <= len(lines) {
			result.Hint = fmt.Sprintf(
				"lines %d-%d of %d; read_file again with from_line %d for the rest",
				from, from+len(window)-1, len(lines), next)
		} else {
			result.Hint = fmt.Sprintf(
				"this file was too large to read whole, so lines %d-%d are all there is of it "+
					"here; ask the developer for the part you need",
				from, from+len(window)-1)
		}
	}
	if result.Rendered {
		// Size is still the file as it sits on disk, so it no longer matches
		// len(text) once text is ODE's own rendering of the parsed document rather
		// than the file's bytes — said here rather than left for the model to notice
		// as a discrepancy on its own.
		note := fmt.Sprintf(
			"this text is ODE's own reading of %s, not its bytes: target_series is left "+
				"out of it (see withheld), and size above is the file as it is on disk, "+
				"which no longer matches the length of text because of that",
			evaluationCriteria)
		if result.Hint == "" {
			result.Hint = note
		} else {
			result.Hint += ". " + note
		}
	}
	return result, nil
}

// splitLines splits text into lines without inventing a last empty one, and says
// whether the text ended with a newline so a full read can put it back.
func splitLines(text string) ([]string, bool) {
	if text == "" {
		return nil, false
	}
	trailing := strings.HasSuffix(text, "\n")
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n"), trailing
}

// windowLines returns the requested window of lines — fromLine (1-based) cut
// to at most maxLines, then cut again to fit budget bytes on a line boundary —
// shared by read_file and read_lib_file so the one rule about what a window is
// lives in one place rather than as two copies that could drift.
//
// fromLine <= 0 means the start of the file, matching read_file's own
// contract. pastEnd reports fromLine beyond the last line; the caller returns
// its own error for that, because read_file and read_lib_file name a
// different noun (the repository path or the library path) in the refusal
// that follows. cut says whether either bound actually shortened the window,
// which is what the caller turns into Truncated — kept separate from pastEnd
// because the two read completely differently to a model: one found less than
// asked for, the other asked for something that is not there.
func windowLines(lines []string, fromLine, maxLines, budget int) (window []string, from int, cut, pastEnd bool) {
	from = fromLine
	if from <= 0 {
		from = 1
	}
	if from > len(lines) {
		return nil, from, false, true
	}

	window = lines[from-1:]
	if maxLines > 0 && len(window) > maxLines {
		window = window[:maxLines]
		cut = true
	}
	// The byte budget applies to whole lines. A window cut mid-line would hand the
	// model a truncated statement that looks like the file's own, which is worse
	// than a shorter window. At least one line is always kept, even over budget,
	// so a single long line does not produce an empty answer.
	kept, spent := 0, 0
	for _, line := range window {
		cost := len(line) + 1
		if kept > 0 && spent+cost > budget {
			cut = true
			break
		}
		kept++
		spent += cost
	}
	window = window[:kept]
	return window, from, cut, false
}

// ---- write_file (L0, no confirmation) ----
//
// §5.8 puts write_file at L0 with no confirmation, and the two together are only
// safe because of what the tool cannot do. It writes into the developer's working
// copy on their own PVC: it cannot stage, cannot commit, cannot push, and cannot
// leave the repository. So the worst outcome is a file the developer reads in the
// Code pane and reverts, which is a diff rather than an incident — and every
// commit remains a human action (§5.11 item 5).
//
// The tier is L0 rather than higher for the same reason run_code is: the tool
// carries no platform data. A model writing code has already read whatever it
// read at whatever tier allowed it, and writing that code to a file adds no
// exposure.

// evaluationCriteria is the one file in the repository this tool may not write.
//
// §5.8 lists "modifying evaluation criteria" among the capabilities that are
// denied server-side with no tool at all, and D11 makes the criteria the
// developer's own definition of success. write_file would otherwise be a way to
// reach it — a tool that can write every file can write that one — so the
// exception is enforced here rather than left to the description above. The
// developer's own routes are unaffected: the file is theirs, and the Code pane
// edits it like any other.
const evaluationCriteria = "evaluation.yaml"

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFileResult is what the model reads back.
type WriteFileResult struct {
	Path string `json:"path"`
	// Bytes is what was written, so a truncated or empty content is visible rather
	// than reported as a success with nothing in it.
	Bytes int `json:"bytes"`
	// Committed is always false, and it is here to be read: a model that assumes a
	// write is published would tell the developer their change is live.
	Committed bool `json:"committed"`
	// Repository says where the file landed, because a session may have switched
	// repositories since the model last looked.
	Repository string `json:"repository"`
	// Locked is true when the write was pyproject.toml and `uv lock` has since
	// brought uv.lock in line with it, uncommitted beside it.
	Locked bool `json:"locked,omitempty"`
	// LockError is why it could not, for a pyproject.toml write only. uv.lock is
	// then stale, and a run launched from it re-resolves on the cluster.
	LockError string `json:"lock_error,omitempty"`
	Hint      string `json:"hint"`
}

func (s *surface) writeFile(ctx context.Context, req Request) (any, error) {
	var in writeFileInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return nil, fmt.Errorf("%w: path is required", ErrInvalidInput)
	}
	if strings.EqualFold(path.Base(path.Clean(strings.TrimSpace(in.Path))), evaluationCriteria) {
		return nil, fmt.Errorf(
			"%w: %s holds the developer's evaluation criteria and no tool may write it. "+
				"Propose the change in the conversation instead",
			ErrInvalidInput, evaluationCriteria)
	}
	// An empty content is a legitimate write — a placeholder module, a cleared
	// file — so it is not refused. A missing one is a mistake, and the two are the
	// same JSON, so the tool takes the permissive reading and reports the size.

	req.Progress("repo", "writing "+in.Path+" into the working copy")
	repoReq := repo.Request{
		Bearer:  req.Token,
		UserSub: req.UserSub,
		// The session's own workbench, so a model working on one operator cannot
		// write into another's checkout — which is the whole reason the session
		// carries one.
		WorkbenchID: req.WorkbenchID,
	}
	written, err := s.deps.Repo.WriteFile(ctx, repoReq, in.Path, []byte(in.Content))
	if err != nil {
		return nil, err
	}

	result := WriteFileResult{
		Path:       written.Path,
		Bytes:      len(in.Content),
		Repository: written.Repository,
		Hint: "the file is in the working copy and is not committed; the developer " +
			"reviews and commits it",
	}
	if written.Path != repo.ProjectFile {
		return result, nil
	}

	// A changed pyproject.toml leaves uv.lock describing the old dependencies, and
	// nothing fails because of it: `uv run` on the cluster re-resolves, the run
	// succeeds, and its commit SHA no longer says which versions it ran. Here and
	// not in the service, because the Code pane writes through the service too and
	// a save there would hold its request for as long as uv takes.
	req.Progress("repo", "locking the dependencies of "+repo.ProjectFile+" with uv lock")
	reason, err := s.deps.Repo.Lock(ctx, repoReq)
	if err != nil {
		// The write has landed, so this is not the tool's failure: an error here
		// would tell the model its pyproject.toml was not written.
		reason = err.Error()
	}
	if reason != "" {
		result.LockError = reason
		result.Hint = "the file is in the working copy and is not committed, and " +
			repo.LockFile + " could not be refreshed from it, so it still describes the " +
			"old dependencies. If lock_error names a fault in " + repo.ProjectFile +
			", fix it and write it again; otherwise the developer runs `uv lock` in their " +
			"pod. The two are committed together"
		return result, nil
	}
	result.Locked = true
	result.Hint = "the file is in the working copy and is not committed. " + repo.LockFile +
		" was refreshed from it by `uv lock` and is uncommitted beside it; the developer " +
		"reviews and commits the two together"
	return result, nil
}

// ---- git_status (L0, no confirmation) ----
//
// One more read beside list_files and read_file above, on the same argument:
// git's own state of the working copy is exactly what the Code pane's status
// view already shows a developer, and before this tool the only way for a
// model to see it was a `git status` or `git log` cell — confirmed, and
// carrying the developer's platform token in the kernel for a read that needs
// none of it. A run measured on 2026-09-23 confirmed three such cells, each
// one opening a working-copy file beside a git status in the same call.
//
// The executor calls Status with Fetch: false on purpose. A fetch is a network
// round trip to GitHub, and §5.11's "no hidden side effect" would not survive
// a tool a model can call any number of times each quietly reaching the
// remote; the pane itself only fetches when the developer opens it or asks for
// a refresh, and this tool follows the same rule rather than making an
// exception for itself.

// maxGitStatusChanges bounds the changes list one answer carries, the same
// argument as maxListedFiles against a different list: a working copy with
// hundreds of uncommitted changes is not one a model should reason over
// wholesale, and what is cut is reported rather than silently dropped.
const maxGitStatusChanges = 200

// gitStatusLogLimit is how many recent commits git_status asks Log for,
// passed explicitly rather than left at Log's own default. The tool's
// description promises "the five most recent commits", and that promise must
// not drift silently if pkg/repo's own default limit is ever changed for a
// reason that has nothing to do with this tool.
const gitStatusLogLimit = 5

// redactRemoteOrigin strips a userinfo credential out of a remote URL before it
// reaches this tool's answer.
//
// The filtering sits here rather than in pkg/repo, on purpose: the Code pane
// reads Status.Remote too, and showing a developer their own origin — including
// a credential they put there themselves outside ODE — is the pane's business,
// not a leak. redact (pkg/repo/git.go) already strips ODE's own token from git's
// output, but it cannot help with this: a developer who points origin at
// `https://x-access-token:ghp_xxx@github.com/org/repo.git` by hand has embedded
// a credential redact never sees, because it only knows the token ODE itself
// issued. What changes at this boundary is that the *answer* joins a stored
// conversation nobody confirms first, which is exactly the line read_file draws
// around a credential path a few lines up — so the same line is drawn here.
//
// net/url does the parsing rather than a hand-rolled cut on "@" or ":", because
// a URL is not reliably split by scanning for either character (a path segment
// or a query value can contain both).
//
// The one form net/url will not parse is git's scp syntax, `git@github.com:org/
// repo.git`, which it rejects on the colon in the first path segment — and that
// is the ordinary shape of an ssh remote, not an exotic one, so dropping every
// such origin would lose the address for every repository a developer brought
// over ssh. It is matched separately: the part before the "@" is a user name
// there, and a user name alone is not a credential. Only when it carries a
// password — the `user:password@host` form — is it dropped. Anything that is
// neither a URL nor scp syntax is dropped whole rather than passed through
// half-filtered.
func redactRemoteOrigin(raw string) string {
	if raw == "" {
		return ""
	}
	// parsed.Host, not err == nil: url.Parse accepts `git:secret@host:path` as an
	// opaque URL — scheme "git", everything after the colon untouched in Opaque —
	// so a credential would survive clearing User, which an opaque URL does not
	// use. A remote worth filtering this way has an authority; anything else goes
	// to the scp branch below and is judged there.
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		parsed.User = nil
		return parsed.String()
	}
	match := scpRemote.FindStringSubmatch(raw)
	if match == nil {
		return ""
	}
	// match[1] is the userinfo, which in scp syntax is a login. A colon in it is
	// a password, and this is the one case where the address goes with it: there
	// is no scp form of "the same remote without the credential" that is still
	// the remote the developer configured.
	if strings.Contains(match[1], ":") {
		return ""
	}
	return raw
}

// scpRemote is git's scp-like remote syntax, `[user@]host:path`, which is not a
// URL. Anchored and deliberately narrow: a host with no slash in it, a colon,
// and a path.
var scpRemote = regexp.MustCompile(`^([^/@]+)@([^/:]+):(.+)$`)

// GitStatusResult is the working copy's git state, on the same terms as the
// Code pane's own status view, plus the recent log. No diff and no file
// content — read_file is for that.
type GitStatusResult struct {
	// Cloned is false when the PVC has no checkout at all.
	Cloned   bool   `json:"cloned"`
	Branch   string `json:"branch,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	Diverged bool   `json:"diverged"`
	Detached bool   `json:"detached"`
	Unborn   bool   `json:"unborn"`

	Head        string `json:"head,omitempty"`
	HeadSubject string `json:"head_subject,omitempty"`
	HeadDate    string `json:"head_date,omitempty"`

	// Remote has any userinfo credential stripped by redactRemoteOrigin before it
	// gets here — see the comment on that function for why the filtering sits at
	// this layer rather than in pkg/repo.
	Remote         string `json:"remote,omitempty"`
	RemoteMismatch bool   `json:"remote_mismatch,omitempty"`

	Dirty bool `json:"dirty"`
	// Changes is cut at maxGitStatusChanges; ChangesCount is what git itself
	// reported before that cut, the same Count/len(Files) pair ListFilesResult
	// uses for the same reason.
	Changes          []repo.Change `json:"changes"`
	ChangesCount     int           `json:"changes_count"`
	ChangesTruncated bool          `json:"changes_truncated,omitempty"`

	// Fetched is always false: this tool calls Status with Fetch: false, so Ahead
	// and Behind are the divergence ODE last knew about, not a fresh comparison
	// against the remote.
	Fetched bool `json:"fetched"`

	Scaffold repo.ScaffoldState `json:"scaffold"`

	// Commits is at most gitStatusLogLimit entries, newest first. Empty with Hint
	// set when Log failed after Status already succeeded — a log failure must
	// not cost the rest of an otherwise good answer.
	Commits []repo.Commit `json:"commits"`
	Hint    string        `json:"hint,omitempty"`
}

func (s *surface) gitStatus(ctx context.Context, req Request) (any, error) {
	req.Progress("repo", "reading git status")
	status, err := s.deps.Repo.Status(ctx, repo.StatusRequest{
		Request: repo.Request{
			Bearer:      req.Token,
			UserSub:     req.UserSub,
			WorkbenchID: req.WorkbenchID,
		},
		// Never true here — see the package comment above this section.
		Fetch: false,
	})
	if err != nil {
		return nil, err
	}

	result := GitStatusResult{
		Cloned:         status.Cloned,
		Branch:         status.Branch,
		Upstream:       status.Upstream,
		Ahead:          status.Ahead,
		Behind:         status.Behind,
		Diverged:       status.Diverged,
		Detached:       status.Detached,
		Unborn:         status.Unborn,
		Head:           status.Head,
		HeadSubject:    status.HeadSubject,
		HeadDate:       status.HeadDate,
		Remote:         redactRemoteOrigin(status.Remote),
		RemoteMismatch: status.RemoteMismatch,
		Dirty:          status.Dirty,
		Changes:        status.Changes,
		ChangesCount:   len(status.Changes),
		Fetched:        status.Fetched,
		Scaffold:       status.Scaffold,
		Commits:        []repo.Commit{},
	}
	if result.Changes == nil {
		result.Changes = []repo.Change{}
	}
	if len(result.Changes) > maxGitStatusChanges {
		result.Changes = result.Changes[:maxGitStatusChanges]
		result.ChangesTruncated = true
		result.Hint = fmt.Sprintf(
			"the change list is incomplete: %d of %d changes are shown; ask the developer "+
				"which part of the working copy matters rather than assuming these are all "+
				"of them", len(result.Changes), result.ChangesCount)
	}

	req.Progress("repo", "reading recent commits")
	commits, err := s.deps.Repo.Log(ctx, repo.Request{
		Bearer:      req.Token,
		UserSub:     req.UserSub,
		WorkbenchID: req.WorkbenchID,
	}, gitStatusLogLimit)
	if err != nil {
		// A log failure must not cost the rest of the answer: the developer's branch,
		// upstream and changes are already known good at this point, and refusing the
		// whole call over the log would throw that away for a part read_file's own
		// error-forwarding precedent says should just be named instead.
		note := "the commit log could not be read: " + err.Error()
		if result.Hint == "" {
			result.Hint = note
		} else {
			result.Hint += ". " + note
		}
		return result, nil
	}
	result.Commits = commits
	if result.Commits == nil {
		// A branch with no commits yet (Unborn) answers with a nil slice from
		// pkg/repo; the model reads an empty list rather than a JSON null either way.
		result.Commits = []repo.Commit{}
	}
	return result, nil
}
