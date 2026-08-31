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
	"path"
	"strings"

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
	Truncated bool   `json:"truncated,omitempty"`
	Hint      string `json:"hint,omitempty"`
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

	lines, trailingNewline := splitLines(file.Text)
	result.TotalLines = len(lines)
	if len(lines) == 0 {
		// An empty file is an ordinary file — `__init__.py` is empty in most Python
		// packages, and the scaffold writes one. It answers as itself rather than as
		// the past-the-end error below, which would tell the model the file is missing.
		result.Hint = "this file is empty"
		return result, nil
	}

	from := in.FromLine
	if from <= 0 {
		from = 1
	}
	if from > len(lines) {
		// An error rather than an empty window, because the two would read the same
		// to a model — "nothing there" — and only one of them is true.
		return nil, fmt.Errorf("%w: from_line %d is past the end of %s, which has %d lines",
			ErrInvalidInput, from, file.Path, len(lines))
	}
	result.FromLine = from

	window := lines[from-1:]
	if in.MaxLines > 0 && len(window) > in.MaxLines {
		window = window[:in.MaxLines]
		result.Truncated = true
	}
	// The byte budget applies to whole lines. A window cut mid-line would hand the
	// model a truncated statement that looks like the file's own, which is worse
	// than a shorter window.
	budget := s.deps.RepoMaxReadBytes
	kept, spent := 0, 0
	for _, line := range window {
		cost := len(line) + 1
		if kept > 0 && spent+cost > budget {
			result.Truncated = true
			break
		}
		kept++
		spent += cost
	}
	window = window[:kept]

	result.Lines = len(window)
	result.Text = strings.Join(window, "\n")
	// A window that reaches the last line reproduces the file's own ending, so a
	// full read is byte-identical to the file and can go back through write_file
	// unchanged.
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
	Hint       string `json:"hint"`
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
	written, err := s.deps.Repo.WriteFile(ctx, repo.Request{
		Bearer:  req.Token,
		UserSub: req.UserSub,
		// The session's own workbench, so a model working on one operator cannot
		// write into another's checkout — which is the whole reason the session
		// carries one.
		WorkbenchID: req.WorkbenchID,
	}, in.Path, []byte(in.Content))
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
	return result, nil
}
