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
	"errors"
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/library"
)

// ---- list_lib_files and read_lib_file (L0, no confirmation) ----
//
// Two more reads on the argument list_files and read_file already made: the
// working copy is not the only code a model reasoning about an operator needs
// to see, and before these two the only way to read operator_lib's own source
// was a `run_code` cell that imported it and printed a file — confirmed, and
// run under the developer's platform token for a read pkg/library performs
// with none. A run measured on 2026-09-23 confirmed 22 run_code cells; 15 of
// them existed only for that read.
//
// Both tools are read-only, because pkg/library is: it runs no `uv sync`,
// writes nothing, and reads whatever the pod's image already installed. There
// is no equivalent of write_file here and none is planned — operator_lib is
// not the developer's own code.
//
// ref is built the way runCode builds one (kernel.go), minus
// WithPlatformToken: operator_lib's source is not platform data, so nothing
// here ever asks the kernel to install the developer's token for it.

func libraryRef(req Request) kernel.Ref {
	return kernel.Ref{Bearer: req.Token, Workbench: req.WorkbenchID}
}

// libraryRefusal turns pkg/library's named conditions into a refusal stated
// for this tool surface — list_lib_files and read_lib_file — rather than
// pkg/library's own Go-oriented wording ("call Files"), which names a method
// no model can call. Every other error, including a busy kernel and the
// helper's own failure, passes through unchanged: a service's refusal is the
// model's information, not something this layer flattens, the same rule
// read_file and list_files already follow for pkg/repo's errors.
//
// path is the argument the refusal should name; callers that have none (Files
// takes no path) pass "".
func libraryRefusal(err error, path string) error {
	switch {
	case errors.Is(err, library.ErrNotInstalled):
		return fmt.Errorf(
			"%w: this deployment's pod has no operator_lib installed, so there is no "+
				"source for list_lib_files or read_lib_file to read",
			library.ErrNotInstalled)
	case errors.Is(err, library.ErrOutsidePackage):
		return fmt.Errorf(
			"%w: %q resolves outside operator_lib's own directory; only a path under "+
				"the package's own root can be read. Call list_lib_files to see what is "+
				"actually there",
			ErrInvalidInput, path)
	case errors.Is(err, library.ErrNotFound):
		return fmt.Errorf(
			"%w: %q is not a file operator_lib has; call list_lib_files to see what is "+
				"actually there",
			ErrInvalidInput, path)
	case errors.Is(err, library.ErrSingleModuleInstall):
		// Stated for the model rather than left to the default branch: this is the
		// one condition here that is neither the model's mistake nor a missing
		// deployment, so a refusal that reads like either would send it looking for
		// a path or a setting instead of asking.
		return fmt.Errorf(
			"%w: this pod's operator_lib is installed as a single module rather than "+
				"as a package directory, and neither list_lib_files nor read_lib_file "+
				"can bound a read to it. Ask the developer to read the file for you",
			library.ErrSingleModuleInstall)
	default:
		return err
	}
}

// ListLibFilesResult is what the model reads back.
type ListLibFilesResult struct {
	Package string `json:"package"`
	// Version is what this pod actually has installed, which can differ from
	// what a launched experiment trains against — see the tool's own
	// description and docs/operator-lib-versions.md.
	Version string          `json:"version"`
	Root    string          `json:"root"`
	Files   []library.Entry `json:"files"`
	// Count is how many files pkg/library found, which is more than
	// len(Files) once either its own cut or maxListedFiles below has applied.
	Count     int    `json:"count"`
	Truncated bool   `json:"truncated,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

func (s *surface) listLibFiles(ctx context.Context, req Request) (any, error) {
	req.Progress("library", "listing operator_lib's source tree")
	listing, err := s.deps.Library.Files(ctx, libraryRef(req))
	if err != nil {
		return nil, libraryRefusal(err, "")
	}

	result := ListLibFilesResult{
		Package:   listing.Package,
		Version:   listing.Version,
		Root:      listing.Root,
		Files:     listing.Files,
		Count:     listing.Count,
		Truncated: listing.Truncated,
	}
	if result.Files == nil {
		result.Files = []library.Entry{}
	}
	// A second cut on top of pkg/library's own: maxListedFiles is what
	// list_files already caps a repository listing at, and reusing it here
	// keeps the two tools' answers the same order of magnitude regardless of
	// how pkg/library's own MaxEntries is configured.
	if len(result.Files) > maxListedFiles {
		result.Files = result.Files[:maxListedFiles]
		result.Truncated = true
	}
	if result.Truncated {
		result.Hint = fmt.Sprintf(
			"this listing is incomplete: %d files are shown of %d found in operator_lib "+
				"%s; ask the developer which part of the library matters rather than "+
				"assuming these are all the files", len(result.Files), result.Count, result.Version)
	}
	return result, nil
}

type readLibFileInput struct {
	Path     string `json:"path"`
	FromLine int    `json:"from_line"`
	MaxLines int    `json:"max_lines"`
}

// ReadLibFileResult is one window of one file of operator_lib's source, on the
// same terms as ReadFileResult.
type ReadLibFileResult struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Text    string `json:"text"`
	// FromLine and Lines describe the window, 1-based and inclusive.
	FromLine   int    `json:"from_line"`
	Lines      int    `json:"lines"`
	TotalLines int    `json:"total_lines"`
	Size       int64  `json:"size"`
	Binary     bool   `json:"binary,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

func (s *surface) readLibFile(ctx context.Context, req Request) (any, error) {
	var in readLibFileInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	requested := strings.TrimSpace(in.Path)
	if requested == "" {
		return nil, fmt.Errorf("%w: path is required", ErrInvalidInput)
	}

	req.Progress("library", "reading "+requested)
	file, err := s.deps.Library.ReadFile(ctx, libraryRef(req), requested, s.deps.RepoMaxReadBytes)
	if err != nil {
		return nil, libraryRefusal(err, requested)
	}

	result := ReadLibFileResult{
		Package:  file.Package,
		Version:  file.Version,
		Path:     file.Path,
		Size:     file.Size,
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
		result.Hint = "this file is empty"
		return result, nil
	}

	window, from, cut, pastEnd := windowLines(lines, in.FromLine, in.MaxLines, s.deps.RepoMaxReadBytes)
	if pastEnd {
		return nil, fmt.Errorf("%w: from_line %d is past the end of %s, which has %d lines",
			ErrInvalidInput, from, file.Path, len(lines))
	}
	result.FromLine = from
	if cut {
		result.Truncated = true
	}

	result.Lines = len(window)
	result.Text = strings.Join(window, "\n")
	if trailingNewline && from+len(window)-1 == len(lines) {
		result.Text += "\n"
	}
	if file.Truncated {
		// pkg/library had already cut the raw read at RepoMaxReadBytes, so even the
		// last line of the last window is not the end of the file.
		result.Truncated = true
	}
	if result.Truncated {
		next := from + len(window)
		if next <= len(lines) {
			result.Hint = fmt.Sprintf(
				"lines %d-%d of %d; read_lib_file again with from_line %d for the rest",
				from, from+len(window)-1, len(lines), next)
		} else {
			result.Hint = fmt.Sprintf(
				"this file was too large to read whole, so lines %d-%d are all there is of "+
					"it here; ask the developer for the part you need",
				from, from+len(window)-1)
		}
	}
	return result, nil
}
