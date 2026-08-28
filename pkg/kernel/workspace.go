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

package kernel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Working on the workspace from outside the pod (§5.11, M7).
//
// The repo working copy lives on the developer's own PVC, which ODE's backend
// cannot reach: the only process with that filesystem mounted is the singleuser
// pod. So a git command and a file edit both happen *there*, and this file is the
// narrow surface the repo package drives them through.
//
// Two decisions are worth understanding before changing anything here.
//
//   - **Not the contents API.** jupyter_server can already read and write files,
//     but `ContentsManager.allow_hidden` is false by default, so it refuses
//     exactly the paths a compliant operator repo needs — `.github/workflows/`,
//     `.gitignore`. D14 says the developer has read/write on *every* file, so the
//     mechanism has to be one that has no opinion about dotfiles. Running in the
//     kernel also means one mechanism for files and for git rather than two.
//
//   - **One cell, one JSON request, one JSON answer.** The payload goes in
//     base64-encoded and comes back base64-encoded between markers, for the
//     reason environmentCode gives: a value ODE interpolated into Python source
//     is a bug waiting for the first path with a quote in it. The marker is what
//     separates the answer from anything else the cell may have printed.
//
// The cost, stated rather than hidden: a kernel runs one cell at a time, so a
// developer with a long training run in flight gets ErrBusy on a file read in
// *that workbench* until it finishes. It is bounded by the workbench now rather
// than by the developer — every other workbench has its own kernel and is
// unaffected — which is what makes two operators at once workable. Within one
// workbench the trade is unchanged, because the alternative is a second kernel per
// workbench and that doubles the pod's memory for a case the developer can already
// answer by opening another workbench.
//
// What that cost is *not* is these operations refusing each other. They are cells
// of a few milliseconds and there are several of them behind one page load, so they
// claim the kernel with a bounded wait (Options.WorkspaceWait) and queue among
// themselves. The 409 is kept for the kernel that is still busy when the wait runs
// out, which is the case worth reporting.

// Command is one program to run in the developer's workspace.
//
// Argv is a list, never a shell string, and it is passed to subprocess without a
// shell — which is what makes a branch name or a commit message from an HTTP
// request unable to become a second command.
type Command struct {
	Argv []string
	// Dir is workspace-relative. Empty runs in the workspace root.
	Dir string
	// Env is added to the process environment for this one command. It is where a
	// credential belongs: a git token in Argv would show up in the pod's own `ps`
	// output, and a token written into .git/config would persist on the PVC.
	Env map[string]string
	// Timeout bounds the program itself, so that a hung git reports a timeout
	// rather than the cell being interrupted from outside with nothing to say.
	Timeout time.Duration
	// MaxOutputBytes bounds stdout and stderr each. Zero takes the service default.
	MaxOutputBytes int
}

// CommandResult is what the program did. A non-zero ExitCode is not a Go error:
// `git status` on a path that is not a repository is a legitimate answer to a
// legitimate question, and the caller decides what it means.
type CommandResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// TimedOut says the program was killed at Timeout. Its output up to that point
	// is still reported.
	TimedOut bool `json:"timed_out"`
	// Truncated says output was cut at MaxOutputBytes.
	Truncated bool `json:"truncated"`
}

// Node is one entry of a workspace tree.
type Node struct {
	Name string `json:"name"`
	// Path is relative to the workspace root, with forward slashes.
	Path     string     `json:"path"`
	Type     string     `json:"type"` // "file" | "directory"
	Size     int64      `json:"size"`
	Modified *time.Time `json:"modified,omitempty"`
	// Children is set on a directory when the listing was recursive.
	Children []Node `json:"children,omitempty"`
	// Elided says this directory holds more entries than the walk was allowed to
	// report. Reported rather than silently cut, for the reason D26 gives about
	// projections: a truncated tree read as a complete one is the failure.
	Elided int `json:"elided,omitempty"`
}

// TreeRequest asks for a listing.
type TreeRequest struct {
	// Path is the directory to walk, workspace-relative. Empty is the workspace root.
	Path string
	// Recursive walks the whole subtree rather than one directory.
	Recursive bool
	// MaxEntries bounds the walk. Zero takes the service default.
	MaxEntries int
	// Exclude names — not paths — that the walk does not enter. `.git` is the
	// caller's business rather than a default here: the Code pane hides it because
	// a working copy's object database is not source, while a caller counting the
	// checkout's size legitimately wants it.
	Exclude []string
}

// FileContent is one file, read.
type FileContent struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Text is the content, present only when the file decoded as UTF-8.
	Text string `json:"text"`
	// Binary says the file did not decode, in which case Text is empty. The Code
	// pane needs the difference: a binary file is shown as one rather than offered
	// to an editor that would corrupt it on save.
	Binary bool `json:"binary"`
	// Truncated says the file is longer than the read allowed.
	Truncated bool       `json:"truncated"`
	Modified  *time.Time `json:"modified,omitempty"`
}

const (
	// workspaceMarker brackets the helper's answer on stdout. Long enough not to
	// occur in a git diff by accident.
	workspaceMarker = "@@ODE-WORKSPACE@@"
	// defaultWorkspaceMaxOutput bounds one workspace call's stdout. Generous
	// because a file read passes through it base64-encoded, which costs a third
	// more than the file.
	defaultWorkspaceMaxOutput = 8 << 20
	defaultTreeMaxEntries     = 4000
	defaultCommandTimeout     = 5 * time.Minute
)

// Command runs one program in the developer's workspace.
func (s *Service) Command(ctx context.Context, ref Ref, cmd Command) (CommandResult, error) {
	if len(cmd.Argv) == 0 {
		return CommandResult{}, fmt.Errorf("%w: a command needs an argv", ErrInvalidRequest)
	}
	for _, argument := range cmd.Argv {
		if argument == "" {
			return CommandResult{}, fmt.Errorf("%w: a command argument cannot be empty", ErrInvalidRequest)
		}
	}
	dir, err := cleanWorkspacePath(cmd.Dir)
	if err != nil {
		return CommandResult{}, err
	}
	timeout := cmd.Timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	limit := cmd.MaxOutputBytes
	if limit <= 0 {
		limit = s.opts.MaxOutputBytes
	}

	var result CommandResult
	err = s.workspaceCall(ctx, ref, workspaceRequest{
		Op:         "command",
		Path:       dir,
		Argv:       cmd.Argv,
		Env:        cmd.Env,
		TimeoutSec: timeout.Seconds(),
		MaxBytes:   limit,
	}, 2*limit+8192, &result)
	return result, err
}

// CommandBatch runs several programs in the workspace, in order, in one cell.
//
// One cell means one claim on the kernel, and that is the whole reason it exists.
// A sequence run as separate calls can be refused halfway: the first takes its
// claim and lands, the second meets a kernel that has since become busy and comes
// back as ErrBusy, and the caller reports that nothing happened while the
// destructive half of the sequence already did. §5.11 item 6 rules that out for
// exactly the operation it would hurt most, so `git reset --hard` and `git clean`
// travel together or not at all.
//
// The batch stops at the first program that exits non-zero or times out, and
// returns the results of the ones that ran — a caller that needs to know how far
// it got can see it, rather than inferring it.
func (s *Service) CommandBatch(
	ctx context.Context, ref Ref, cmds []Command,
) ([]CommandResult, error) {
	if len(cmds) == 0 {
		return nil, fmt.Errorf("%w: a batch needs at least one command", ErrInvalidRequest)
	}
	specs := make([]workspaceCommand, 0, len(cmds))
	budget := 0
	for _, cmd := range cmds {
		if len(cmd.Argv) == 0 {
			return nil, fmt.Errorf("%w: a command needs an argv", ErrInvalidRequest)
		}
		for _, argument := range cmd.Argv {
			if argument == "" {
				return nil, fmt.Errorf("%w: a command argument cannot be empty", ErrInvalidRequest)
			}
		}
		dir, err := cleanWorkspacePath(cmd.Dir)
		if err != nil {
			return nil, err
		}
		timeout := cmd.Timeout
		if timeout <= 0 {
			timeout = defaultCommandTimeout
		}
		limit := cmd.MaxOutputBytes
		if limit <= 0 {
			limit = s.opts.MaxOutputBytes
		}
		budget += limit
		specs = append(specs, workspaceCommand{
			Path:       dir,
			Argv:       cmd.Argv,
			Env:        cmd.Env,
			TimeoutSec: timeout.Seconds(),
			MaxBytes:   limit,
		})
	}

	var answer struct {
		Results []CommandResult `json:"results"`
	}
	err := s.workspaceCall(ctx, ref, workspaceRequest{
		Op:       "commands",
		Commands: specs,
	}, 2*budget+8192, &answer)
	return answer.Results, err
}

// Tree lists the workspace, or one directory of it.
func (s *Service) Tree(ctx context.Context, ref Ref, req TreeRequest) (Node, error) {
	path, err := cleanWorkspacePath(req.Path)
	if err != nil {
		return Node{}, err
	}
	maxEntries := req.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultTreeMaxEntries
	}

	var node Node
	err = s.workspaceCall(ctx, ref, workspaceRequest{
		Op:         "tree",
		Path:       path,
		Recursive:  req.Recursive,
		MaxEntries: maxEntries,
		Exclude:    req.Exclude,
	}, defaultWorkspaceMaxOutput, &node)
	return node, err
}

// ReadFile reads one file of the workspace. maxBytes bounds the read; a longer
// file comes back truncated and says so.
func (s *Service) ReadFile(
	ctx context.Context, ref Ref, path string, maxBytes int,
) (FileContent, error) {
	clean, err := cleanWorkspacePath(path)
	if err != nil {
		return FileContent{}, err
	}
	if clean == "" {
		return FileContent{}, fmt.Errorf("%w: no file was named", ErrInvalidRequest)
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	var content FileContent
	err = s.workspaceCall(ctx, ref, workspaceRequest{
		Op:       "read",
		Path:     clean,
		MaxBytes: maxBytes,
	}, 2*maxBytes+8192, &content)
	return content, err
}

// WriteFile writes one file of the workspace, creating parent directories.
//
// It writes and nothing else: no git add, no commit. §5.11 item 5 makes commit
// and push explicit developer actions, and a write that quietly staged itself
// would take that decision away.
func (s *Service) WriteFile(
	ctx context.Context, ref Ref, path string, content []byte,
) (Node, error) {
	clean, err := cleanWorkspacePath(path)
	if err != nil {
		return Node{}, err
	}
	if clean == "" {
		return Node{}, fmt.Errorf("%w: no file was named", ErrInvalidRequest)
	}

	var node Node
	err = s.workspaceCall(ctx, ref, workspaceRequest{
		Op:      "write",
		Path:    clean,
		Content: base64.StdEncoding.EncodeToString(content),
	}, 64<<10, &node)
	return node, err
}

// MakeDir creates a directory and its parents.
func (s *Service) MakeDir(ctx context.Context, ref Ref, path string) (Node, error) {
	clean, err := cleanWorkspacePath(path)
	if err != nil {
		return Node{}, err
	}
	if clean == "" {
		return Node{}, fmt.Errorf("%w: no directory was named", ErrInvalidRequest)
	}

	var node Node
	err = s.workspaceCall(ctx, ref, workspaceRequest{Op: "mkdir", Path: clean}, 64<<10, &node)
	return node, err
}

// Remove deletes a file, or a directory when recursive is set.
func (s *Service) Remove(ctx context.Context, ref Ref, path string, recursive bool) error {
	clean, err := cleanWorkspacePath(path)
	if err != nil {
		return err
	}
	if clean == "" {
		return fmt.Errorf("%w: the workspace root cannot be removed", ErrInvalidRequest)
	}
	var ignored struct{}
	return s.workspaceCall(ctx, ref, workspaceRequest{
		Op:        "remove",
		Path:      clean,
		Recursive: recursive,
	}, 64<<10, &ignored)
}

// workspaceRequest is the helper's input. One struct for every operation,
// because it crosses into Python as JSON and a per-operation shape there would
// buy nothing but more code on both sides.
type workspaceRequest struct {
	Op   string `json:"op"`
	Path string `json:"path"`

	Argv       []string          `json:"argv,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec float64           `json:"timeout_s,omitempty"`
	// Commands is the batch, each entry carrying its own working directory,
	// environment and bounds. Set only by the "commands" operation.
	Commands []workspaceCommand `json:"commands,omitempty"`

	Recursive  bool     `json:"recursive,omitempty"`
	MaxEntries int      `json:"max_entries,omitempty"`
	MaxBytes   int      `json:"max_bytes,omitempty"`
	Exclude    []string `json:"exclude,omitempty"`

	Content string `json:"content,omitempty"`
}

// workspaceCommand is one program of a batch. The same four fields the single
// "command" operation carries at the top level, per command, because a batch's
// entries are not required to share a working directory or a timeout.
type workspaceCommand struct {
	Path       string            `json:"path"`
	Argv       []string          `json:"argv"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec float64           `json:"timeout_s,omitempty"`
	MaxBytes   int               `json:"max_bytes,omitempty"`
}

// workspaceCall runs one operation in the pod and decodes its answer into out.
func (s *Service) workspaceCall(
	ctx context.Context, ref Ref, req workspaceRequest, maxOutput int, out any,
) error {
	encoded, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// Willing to wait: see claim. Two of these colliding is ODE competing with
	// itself, and the loser of that race is worth a few seconds rather than a 409.
	target, conn, handle, user, run, err := s.claim(ctx, ref, s.opts.WorkspaceWait)
	if err != nil {
		return err
	}
	defer s.finishRun(target, run)

	executeCtx, cancel := context.WithTimeout(ctx, s.opts.ExecuteTimeout)
	defer cancel()

	events, err := conn.execute(executeCtx, workspaceCode(encoded, s.opts.WorkspacePath), executeOptions{
		// Quiet rather than Silent: the answer arrives on stdout, and a silent
		// execution is allowed to suppress that.
		Quiet:          true,
		MaxOutputBytes: maxOutput,
		OnCancel: func() {
			interruptCtx, interruptCancel := context.WithTimeout(
				context.WithoutCancel(ctx), s.opts.RequestTimeout)
			defer interruptCancel()
			_ = s.Interrupt(interruptCtx, handle)
		},
	})
	if err != nil {
		return err
	}

	var (
		stdout    strings.Builder
		stderr    strings.Builder
		status    string
		failure   string
		errorName string
		errorText string
		truncated bool
	)
	for event := range events {
		switch event.Kind {
		case KindStream:
			if event.Stream == "stderr" {
				stderr.WriteString(event.Text)
			} else {
				stdout.WriteString(event.Text)
			}
		case KindError:
			errorName, errorText = event.ErrorName, event.ErrorValue
		case KindDone:
			status, failure, truncated = event.Status, event.Error, event.Truncated
		}
	}

	if status != StatusOK {
		// The helper raising is ODE's own fault rather than the developer's, so the
		// exception is carried out rather than reduced to "it failed".
		detail := failure
		if errorName != "" {
			detail = strings.TrimSpace(errorName + ": " + errorText)
		}
		if detail == "" {
			detail = status
		}
		return fmt.Errorf("kernel: the workspace operation %q failed: %s", req.Op, detail)
	}
	if truncated {
		return fmt.Errorf("kernel: the answer to the workspace operation %q exceeded %d bytes",
			req.Op, maxOutput)
	}

	answer, err := workspaceAnswer(stdout.String())
	if err != nil {
		// Almost always a stale kernel whose helper wrote nothing, so name the user:
		// the same operation from another developer may be perfectly healthy. What
		// the cell wrote to stderr comes along because that is where the helper's own
		// traceback lands when the kernel did not mark the cell as failed.
		if diagnostic := strings.TrimSpace(stderr.String()); diagnostic != "" {
			return fmt.Errorf("kernel: %w (user %s, operation %q): %s",
				err, user.Name, req.Op, lastLines(diagnostic, 3))
		}
		return fmt.Errorf("kernel: %w (user %s, operation %q)", err, user.Name, req.Op)
	}
	if answer.Error != "" {
		return workspaceError(answer)
	}
	if err := json.Unmarshal(answer.Result, out); err != nil {
		return fmt.Errorf("kernel: the workspace operation %q answered unusably: %w", req.Op, err)
	}
	return nil
}

// lastLines keeps the tail of a diagnostic. The tail rather than the head because
// a Python traceback puts the exception on the last line.
func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " | ")
}

// workspaceReply is the helper's answer.
type workspaceReply struct {
	// Error is a machine-readable cause: "not_found", "invalid_path", "is_a_directory",
	// "not_a_directory", "not_empty", "failed".
	Error   string          `json:"error"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

// workspaceError maps the helper's cause onto this package's errors, so that the
// API layer's existing status mapping covers workspace operations too.
func workspaceError(reply workspaceReply) error {
	switch reply.Error {
	case "not_found":
		return fmt.Errorf("%w: %s", ErrNotFound, reply.Message)
	case "invalid_path", "is_a_directory", "not_a_directory", "not_empty":
		return fmt.Errorf("%w: %s", ErrInvalidRequest, reply.Message)
	default:
		return fmt.Errorf("kernel: %s", reply.Message)
	}
}

// workspaceAnswer extracts the helper's reply from what the cell printed.
func workspaceAnswer(stdout string) (workspaceReply, error) {
	start := strings.Index(stdout, workspaceMarker)
	if start < 0 {
		return workspaceReply{}, fmt.Errorf("the workspace helper printed no answer")
	}
	rest := stdout[start+len(workspaceMarker):]
	end := strings.Index(rest, workspaceMarker)
	if end < 0 {
		return workspaceReply{}, fmt.Errorf("the workspace helper's answer was cut short")
	}
	// Whitespace, because a print may have been wrapped across stream messages.
	payload := strings.Join(strings.Fields(rest[:end]), "")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return workspaceReply{}, fmt.Errorf("the workspace helper's answer was not readable: %w", err)
	}
	var reply workspaceReply
	if err := json.Unmarshal(decoded, &reply); err != nil {
		return workspaceReply{}, fmt.Errorf("the workspace helper's answer was not readable: %w", err)
	}
	return reply, nil
}

// workspaceCode renders the cell that performs one operation.
//
// Everything variable arrives base64-encoded — the request and the workspace path
// alike — so no value ODE holds is ever interpolated into Python source. The
// helper defines nothing in the developer's namespace that outlives the cell: it
// runs inside a function and deletes the two names it binds.
func workspaceCode(request []byte, workspace string) string {
	return fmt.Sprintf(`
def __ode_workspace(_req_b64, _root_b64, _marker):
    import base64, json, os, subprocess, sys, time

    def reply(**payload):
        encoded = base64.b64encode(json.dumps(payload).encode("utf-8")).decode("ascii")
        sys.stdout.write(_marker + encoded + _marker + "\n")
        sys.stdout.flush()

    request = json.loads(base64.b64decode(_req_b64).decode("utf-8"))
    relative = base64.b64decode(_root_b64).decode("utf-8").strip("/")

    # The workspace as an absolute path. It is configured relative to the
    # singleuser server root, which is the developer's home, and that is the
    # form ODE and jupyter_server agree on. os.getcwd() is the fallback rather
    # than the primary: the kernel is started in the workspace, but developer
    # code is free to chdir out of it.
    root = os.path.realpath(os.path.join(os.path.expanduser("~"), relative)) if relative else None
    if root is None or not os.path.isdir(root):
        root = os.path.realpath(os.getcwd())

    def resolve(path):
        target = os.path.realpath(os.path.join(root, path)) if path else root
        # Containment is re-checked here and not only in Go, because a symlink
        # inside the workspace can point outside it and only this side can see it.
        if target != root and not target.startswith(root + os.sep):
            return None
        return target

    def entry(path, name, stat_result, kind):
        return {
            "name": name,
            "path": os.path.relpath(path, root).replace(os.sep, "/") if path != root else "",
            "type": kind,
            "size": int(stat_result.st_size),
            "modified": time.strftime("%%Y-%%m-%%dT%%H:%%M:%%SZ", time.gmtime(stat_result.st_mtime)),
        }

    def run_program(spec):
        # One program. Returns (result, failure), exactly one of which is None, so
        # that the single and the batched operation share this and cannot drift
        # apart in what they allow.
        where = spec.get("path") or ""
        directory = resolve(where)
        if directory is None:
            return None, ("invalid_path", "%%s is outside the workspace" %% where)
        # A missing working directory raises the same FileNotFoundError as a missing
        # program, and the two mean completely different things to the caller: one is
        # "clone it first", the other is "this image has no git".
        if not os.path.isdir(directory):
            return None, ("not_found", "%%s does not exist" %% where)
        environment = dict(os.environ)
        environment.update(spec.get("env") or {})
        # Never a shell: argv is a list, so a branch name or a commit message
        # cannot become a second command.
        limit = int(spec.get("max_bytes") or 0)
        try:
            completed = subprocess.run(
                spec["argv"],
                cwd=directory,
                env=environment,
                stdin=subprocess.DEVNULL,
                capture_output=True,
                timeout=float(spec.get("timeout_s") or 300.0),
            )
            out, err, code, timed_out = completed.stdout, completed.stderr, completed.returncode, False
        except subprocess.TimeoutExpired as expired:
            out = expired.stdout or b""
            err = expired.stderr or b""
            code, timed_out = -1, True
        except FileNotFoundError as missing:
            return None, ("failed", "%%s is not installed in this image" %% missing.filename)
        truncated = False
        if limit and (len(out) > limit or len(err) > limit):
            out, err, truncated = out[:limit], err[:limit], True
        return {
            "exit_code": code,
            "stdout": out.decode("utf-8", "replace"),
            "stderr": err.decode("utf-8", "replace"),
            "timed_out": timed_out,
            "truncated": truncated,
        }, None

    op = request.get("op")
    path = request.get("path") or ""
    target = resolve(path)
    if target is None:
        return reply(error="invalid_path", message="%%s is outside the workspace" %% path)

    if op == "command":
        result, failure = run_program({
            "path": path,
            "argv": request["argv"],
            "env": request.get("env"),
            "timeout_s": request.get("timeout_s"),
            "max_bytes": request.get("max_bytes"),
        })
        if failure is not None:
            return reply(error=failure[0], message=failure[1])
        return reply(result=result)

    if op == "commands":
        # The batch is one cell, so nothing can take the kernel between two of
        # these. It stops at the first program that fails, because the sequences
        # ODE sends are sequences: a commit after a failed stage would record
        # something other than what was asked for.
        results = []
        for spec in request.get("commands") or []:
            result, failure = run_program(spec)
            if failure is not None:
                return reply(error=failure[0], message=failure[1])
            results.append(result)
            if result["exit_code"] != 0 or result["timed_out"]:
                break
        return reply(result={"results": results})

    if op == "tree":
        if not os.path.exists(target):
            return reply(error="not_found", message="%%s does not exist" %% path)
        if not os.path.isdir(target):
            return reply(error="not_a_directory", message="%%s is not a directory" %% path)
        exclude = set(request.get("exclude") or [])
        budget = [int(request.get("max_entries") or 4000)]
        recursive = bool(request.get("recursive"))

        def walk(directory):
            node = entry(directory, os.path.basename(directory) or ".", os.stat(directory), "directory")
            children, elided = [], 0
            try:
                names = sorted(os.listdir(directory))
            except OSError:
                names = []
            for name in names:
                if name in exclude:
                    continue
                child = os.path.join(directory, name)
                if budget[0] <= 0:
                    elided += 1
                    continue
                budget[0] -= 1
                try:
                    stat_result = os.stat(child, follow_symlinks=False)
                except OSError:
                    continue
                if os.path.isdir(child) and not os.path.islink(child):
                    children.append(walk(child) if recursive else
                                    entry(child, name, stat_result, "directory"))
                else:
                    children.append(entry(child, name, stat_result, "file"))
            node["children"] = children
            if elided:
                node["elided"] = elided
            return node

        return reply(result=walk(target))

    if op == "read":
        if not os.path.exists(target):
            return reply(error="not_found", message="%%s does not exist" %% path)
        if os.path.isdir(target):
            return reply(error="is_a_directory", message="%%s is a directory" %% path)
        limit = int(request.get("max_bytes") or 1048576)
        stat_result = os.stat(target)
        with open(target, "rb") as handle:
            raw = handle.read(limit + 1)
        truncated = len(raw) > limit
        raw = raw[:limit]
        try:
            text, binary = raw.decode("utf-8"), False
        except UnicodeDecodeError:
            text, binary = "", True
        if b"\x00" in raw:
            text, binary = "", True
        return reply(result={
            "path": os.path.relpath(target, root).replace(os.sep, "/"),
            "size": int(stat_result.st_size),
            "text": text,
            "binary": binary,
            "truncated": truncated,
            "modified": time.strftime("%%Y-%%m-%%dT%%H:%%M:%%SZ", time.gmtime(stat_result.st_mtime)),
        })

    if op == "write":
        if os.path.isdir(target):
            return reply(error="is_a_directory", message="%%s is a directory" %% path)
        parent = os.path.dirname(target)
        if parent:
            os.makedirs(parent, exist_ok=True)
        content = base64.b64decode(request.get("content") or "")
        # Written whole and then moved into place, so an interrupted write cannot
        # leave the developer with half a file where a working one was.
        temporary = target + ".ode-partial"
        with open(temporary, "wb") as handle:
            handle.write(content)
        os.replace(temporary, target)
        return reply(result=entry(target, os.path.basename(target), os.stat(target), "file"))

    if op == "mkdir":
        if os.path.isfile(target):
            return reply(error="not_a_directory", message="%%s is a file" %% path)
        os.makedirs(target, exist_ok=True)
        return reply(result=entry(target, os.path.basename(target), os.stat(target), "directory"))

    if op == "remove":
        if not os.path.exists(target) and not os.path.islink(target):
            return reply(error="not_found", message="%%s does not exist" %% path)
        if os.path.isdir(target) and not os.path.islink(target):
            if not request.get("recursive"):
                return reply(error="not_empty", message="%%s is a directory" %% path)
            import shutil
            shutil.rmtree(target)
        else:
            os.remove(target)
        return reply(result={})

    return reply(error="failed", message="unknown workspace operation %%s" %% op)

try:
    __ode_workspace(%q, %q, %q)
finally:
    del __ode_workspace
`,
		base64.StdEncoding.EncodeToString(request),
		base64.StdEncoding.EncodeToString([]byte(workspace)),
		workspaceMarker,
	)
}
