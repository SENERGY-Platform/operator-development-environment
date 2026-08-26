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

	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

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
			"%w: %s holds the developer's evaluation criteria and no tool may write it "+
				"(SPEC §5.8). Propose the change in the conversation instead",
			ErrInvalidInput, evaluationCriteria)
	}
	// An empty content is a legitimate write — a placeholder module, a cleared
	// file — so it is not refused. A missing one is a mistake, and the two are the
	// same JSON, so the tool takes the permissive reading and reports the size.

	req.Progress("repo", "writing "+in.Path+" into the working copy")
	written, err := s.deps.Repo.WriteFile(ctx, repo.Request{
		Bearer:  req.Token,
		UserSub: req.UserSub,
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
