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

// One developer's hold on one pod.
//
// Everything here hangs off userSession, and everything on userSession is
// guarded by its mutex — which also serialises execution. One developer, one
// cell at a time: ipykernel would queue a second request anyway, and refusing it
// here means the second caller is told rather than left waiting on a kernel that
// is busy with something they cannot see.
//
// The operations in this file are what the API routes and the run_code tool
// call. The six methods of SPEC §5.6 in kernel.go are what they are assembled
// from; the difference is that these bring the session up first, so no caller
// has to remember the token push, the workspace or the keep-alive.

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Status is what the SPA and the tool report about a developer's kernel.
type Status struct {
	User          string     `json:"user"`
	ServerReady   bool       `json:"server_ready"`
	ServerPending string     `json:"server_pending,omitempty"`
	ServerURL     string     `json:"server_url,omitempty"`
	Started       *time.Time `json:"started,omitempty"`
	LastActivity  *time.Time `json:"last_activity,omitempty"`

	KernelID   string `json:"kernel_id,omitempty"`
	KernelName string `json:"kernel_name,omitempty"`
	// Profile is the KubeSpawner profile ODE spawns with, empty for the default.
	Profile string `json:"profile,omitempty"`
	// Busy is true while an execution ODE started is still running.
	Busy bool `json:"busy"`
	// Workspace is the persistent working directory the kernel runs in.
	Workspace string `json:"workspace"`
	// WorkspaceReady says the directory exists on the PVC.
	WorkspaceReady bool `json:"workspace_ready"`
}

// userSession is ODE's hold on one developer's pod.
//
// Everything on it is guarded by mux, which also serialises execution: one
// developer, one cell at a time. ipykernel would queue a second request anyway,
// and queueing it here means the second caller is told rather than left waiting
// on a kernel that is busy with something they cannot see.
type userSession struct {
	user User

	mux         sync.Mutex
	serverURL   string
	token       HubToken
	tokenExpiry time.Time
	kernelID    string
	conn        *connection
	// pushedToken is the platform token currently installed in the kernel. Held to
	// notice a refresh, not for any other purpose, and never logged.
	pushedToken    string
	workspaceReady bool
	running        bool
	lastUsed       time.Time

	keepalive context.CancelFunc
}

func (s *Service) session(user User) *userSession {
	s.mux.Lock()
	defer s.mux.Unlock()
	existing, found := s.sessions[user.Name]
	if found {
		return existing
	}
	created := &userSession{user: user, lastUsed: time.Now()}
	s.sessions[user.Name] = created
	return created
}

func (s *Service) lookup(name string) (*userSession, bool) {
	s.mux.Lock()
	defer s.mux.Unlock()
	existing, found := s.sessions[name]
	return existing, found
}

// ---- The session-level operations everything above ODE actually calls ----

// Ensure brings a developer's session up: server, token, kernel, workspace and
// the platform token inside the kernel. Safe to call on every session open.
func (s *Service) Ensure(ctx context.Context, bearer string) (Status, error) {
	user, err := s.UserFor(bearer)
	if err != nil {
		return Status{}, err
	}
	session := s.session(user)
	session.mux.Lock()
	defer session.mux.Unlock()

	if _, err := s.ensureLocked(ctx, session, bearer); err != nil {
		return Status{}, err
	}
	return s.statusLocked(ctx, session)
}

// Status reports what is running, without starting anything.
func (s *Service) Status(ctx context.Context, bearer string) (Status, error) {
	user, err := s.UserFor(bearer)
	if err != nil {
		return Status{}, err
	}
	session, found := s.lookup(user.Name)
	if !found {
		state, err := s.hub.ServerState(ctx, user.Name)
		if err != nil {
			return Status{}, err
		}
		return Status{
			User:          user.Name,
			ServerReady:   state.Ready,
			ServerPending: state.Pending,
			ServerURL:     state.URL,
			Started:       state.Started,
			LastActivity:  state.LastActivity,
			Profile:       s.opts.Profile,
			Workspace:     s.opts.WorkspacePath,
		}, nil
	}
	session.mux.Lock()
	defer session.mux.Unlock()
	return s.statusLocked(ctx, session)
}

// Run executes developer or LLM code in the developer's kernel.
//
// It is the only entry point that both brings the session up and runs something,
// which is deliberate: every caller then gets the token push, the keep-alive and
// the workspace without having to remember them.
func (s *Service) Run(
	ctx context.Context, bearer, code string,
) (<-chan ExecutionEvent, error) {
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("%w: there is no code to run", ErrInvalidRequest)
	}
	user, err := s.UserFor(bearer)
	if err != nil {
		return nil, err
	}
	session := s.session(user)

	session.mux.Lock()
	if session.running {
		session.mux.Unlock()
		return nil, ErrBusy
	}
	handle, err := s.ensureLocked(ctx, session, bearer)
	if err != nil {
		session.mux.Unlock()
		return nil, err
	}
	conn := session.conn
	session.running = true
	session.lastUsed = time.Now()
	session.mux.Unlock()

	// The cell's own deadline, descending from the caller so that a disconnected
	// developer still stops their cell, and bounded so a runaway one does not hold
	// the kernel forever.
	executeCtx, cancel := context.WithTimeout(ctx, s.opts.ExecuteTimeout)

	raw, err := conn.execute(executeCtx, code, executeOptions{
		MaxOutputBytes: s.opts.MaxOutputBytes,
		OnCancel: func() {
			interruptCtx, interruptCancel := context.WithTimeout(
				context.WithoutCancel(ctx), s.opts.RequestTimeout)
			defer interruptCancel()
			if err := s.Interrupt(interruptCtx, handle); err != nil {
				slog.Warn("interrupting a cancelled execution failed", "user", user.Name, "error", err)
			}
		},
	})
	if err != nil {
		cancel()
		s.finishRun(session)
		return nil, err
	}

	// Wrapped so that the busy flag and the execution deadline are released when
	// the cell ends, whatever ends it.
	out := make(chan ExecutionEvent, 32)
	go func() {
		defer close(out)
		defer cancel()
		defer s.finishRun(session)
		for event := range raw {
			out <- event
		}
	}()
	return out, nil
}

func (s *Service) finishRun(session *userSession) {
	session.mux.Lock()
	session.running = false
	session.lastUsed = time.Now()
	session.mux.Unlock()
}

// InterruptUser stops the developer's running cell.
func (s *Service) InterruptUser(ctx context.Context, bearer string) error {
	handle, err := s.handleFor(bearer)
	if err != nil {
		return err
	}
	return s.Interrupt(ctx, handle)
}

// Restart ends the kernel and starts a fresh one, keeping the pod and therefore
// the workspace. This is the "my session is wedged" action.
func (s *Service) Restart(ctx context.Context, bearer string) (Status, error) {
	user, err := s.UserFor(bearer)
	if err != nil {
		return Status{}, err
	}
	session := s.session(user)

	session.mux.Lock()
	defer session.mux.Unlock()

	if session.kernelID != "" {
		handle := KernelHandle{
			User: user, ServerURL: ServerURL(session.serverURL),
			Token: session.token, KernelID: session.kernelID,
		}
		if err := s.Shutdown(ctx, handle); err != nil {
			slog.Warn("shutting a kernel down before restart failed", "user", user.Name, "error", err)
		}
	}
	s.dropKernelLocked(session)

	if _, err := s.ensureLocked(ctx, session, bearer); err != nil {
		return Status{}, err
	}
	return s.statusLocked(ctx, session)
}

// ShutdownUser ends the developer's kernel and releases ODE's hold on the pod.
func (s *Service) ShutdownUser(ctx context.Context, bearer string) error {
	user, err := s.UserFor(bearer)
	if err != nil {
		return err
	}
	session, found := s.lookup(user.Name)
	if !found {
		return ErrNoKernel
	}

	session.mux.Lock()
	defer session.mux.Unlock()
	if session.kernelID == "" {
		return ErrNoKernel
	}
	handle := KernelHandle{
		User: user, ServerURL: ServerURL(session.serverURL),
		Token: session.token, KernelID: session.kernelID,
	}
	err = s.Shutdown(ctx, handle)
	s.dropKernelLocked(session)
	s.stopKeepaliveLocked(session)
	return err
}

// Files lists one directory of the developer's workspace.
func (s *Service) Files(ctx context.Context, bearer, path string) ([]FileEntry, error) {
	user, err := s.UserFor(bearer)
	if err != nil {
		return nil, err
	}
	session := s.session(user)

	session.mux.Lock()
	defer session.mux.Unlock()

	server, err := s.ensureServerLocked(ctx, session)
	if err != nil {
		return nil, err
	}
	token, err := s.ensureTokenLocked(ctx, session)
	if err != nil {
		return nil, err
	}

	target := s.opts.WorkspacePath
	if trimmed := strings.Trim(strings.TrimSpace(path), "/"); trimmed != "" {
		clean, err := cleanWorkspacePath(trimmed)
		if err != nil {
			return nil, err
		}
		target = s.opts.WorkspacePath + "/" + clean
	}
	return s.serverAPIFor(server, token).listDirectory(ctx, target)
}

// RefreshPlatformToken pushes a renewed platform token into a live kernel.
//
// §5.6 item 4: spawn-time environment variables cannot be refreshed, so the
// current token is installed by executing into the kernel instead. Called
// whenever the connection ODE is serving has adopted a new token.
func (s *Service) RefreshPlatformToken(ctx context.Context, bearer string) error {
	user, err := s.UserFor(bearer)
	if err != nil {
		return err
	}
	session, found := s.lookup(user.Name)
	if !found {
		return nil // Nothing is running; the next Ensure pushes the current token.
	}

	session.mux.Lock()
	defer session.mux.Unlock()
	if session.conn == nil {
		return nil
	}
	return s.pushEnvironmentLocked(ctx, session, bearer)
}

// ---- The locked helpers the operations above are assembled from ----

// ensureLocked is the whole session bring-up, idempotent and cheap when
// everything is already there.
func (s *Service) ensureLocked(
	ctx context.Context, session *userSession, bearer string,
) (KernelHandle, error) {
	server, err := s.ensureServerLocked(ctx, session)
	if err != nil {
		return KernelHandle{}, err
	}
	token, err := s.ensureTokenLocked(ctx, session)
	if err != nil {
		return KernelHandle{}, err
	}
	if err := s.ensureKernelLocked(ctx, session, server, token); err != nil {
		return KernelHandle{}, err
	}
	if err := s.ensureConnectionLocked(ctx, session); err != nil {
		return KernelHandle{}, err
	}
	if err := s.pushEnvironmentLocked(ctx, session, bearer); err != nil {
		return KernelHandle{}, err
	}
	s.startKeepaliveLocked(session)
	session.lastUsed = time.Now()

	return KernelHandle{
		User:      session.user,
		ServerURL: ServerURL(session.serverURL),
		Token:     session.token,
		KernelID:  session.kernelID,
		Name:      s.opts.KernelName,
	}, nil
}

func (s *Service) ensureServerLocked(ctx context.Context, session *userSession) (string, error) {
	if session.serverURL != "" {
		return session.serverURL, nil
	}
	server, err := s.EnsureServer(ctx, session.user)
	if err != nil {
		return "", err
	}
	session.serverURL = string(server)
	return session.serverURL, nil
}

func (s *Service) ensureTokenLocked(ctx context.Context, session *userSession) (HubToken, error) {
	if session.token != "" && time.Until(session.tokenExpiry) > tokenRenewBefore {
		return session.token, nil
	}
	token, expiry, err := s.hub.MintToken(ctx, session.user.Name, s.opts.TokenTTL)
	if err != nil {
		return "", err
	}
	session.token, session.tokenExpiry = token, expiry
	// The open socket is left alone. It was authorised at connect time and stays
	// valid; the renewed token is what the next reconnect will use.
	return session.token, nil
}

func (s *Service) ensureKernelLocked(
	ctx context.Context, session *userSession, server string, token HubToken,
) error {
	api := s.serverAPIFor(server, token)

	if session.kernelID != "" {
		if _, err := api.getKernel(ctx, session.kernelID); err == nil {
			return nil
		}
		// The kernel ODE remembers is gone — the pod was culled and respawned, or
		// someone shut it down in JupyterLab. Starting a new one is the useful
		// answer; the workspace is what carried anything worth keeping.
		slog.InfoContext(ctx, "the remembered kernel is gone; starting a new one",
			"user", session.user.Name, "kernel", session.kernelID)
		s.dropKernelLocked(session)
	}

	if !session.workspaceReady {
		if err := api.ensureDirectory(ctx, s.opts.WorkspacePath); err != nil {
			return err
		}
		session.workspaceReady = true
	}

	created, err := api.createKernel(ctx, s.opts.KernelName, s.opts.WorkspacePath)
	if err != nil {
		return err
	}
	session.kernelID = created.ID
	slog.InfoContext(ctx, "started a kernel",
		"user", session.user.Name, "kernel", created.ID, "workspace", s.opts.WorkspacePath)
	return nil
}

func (s *Service) ensureConnectionLocked(ctx context.Context, session *userSession) error {
	if session.conn != nil && !session.conn.isClosed() {
		return nil
	}
	if session.conn != nil {
		logConnectionClose(session.user.Name, session.conn.err())
		session.conn = nil
		// A dropped socket says nothing about the kernel, but the pushed token was
		// installed in a kernel that may itself be gone, so it is re-pushed.
		session.pushedToken = ""
	}

	endpoint := channelsEndpoint(session.serverURL, session.kernelID)
	conn, err := dial(ctx, s.opts.Dialer, endpoint, session.token, session.user.Name)
	if err != nil {
		return err
	}
	session.conn = conn
	return nil
}

// pushEnvironmentLocked installs the developer's platform token and ODE's
// configuration in the kernel (§5.6 item 4).
//
// The values are base64-encoded rather than interpolated into the source. A JWT
// happens to be safe inside a Python string literal and a URL from configuration
// need not be, and the difference is not something a reader of this code should
// have to reason about. The execution is silent, so it leaves no history and
// nothing reaches the developer's console.
func (s *Service) pushEnvironmentLocked(
	ctx context.Context, session *userSession, bearer string,
) error {
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(bearer, "Bearer "), "bearer "))
	if token == "" {
		return fmt.Errorf("%w: no platform token to install in the kernel", ErrInvalidRequest)
	}
	if session.pushedToken == token {
		return nil
	}
	if session.conn == nil {
		return ErrNoKernel
	}

	environment := map[string]string{
		PlatformTokenEnv: token,
		WorkspaceEnv:     s.opts.WorkspacePath,
	}
	for name, value := range s.opts.Environment {
		environment[name] = value
	}

	events, err := session.conn.execute(ctx, environmentCode(environment), executeOptions{
		Silent:         true,
		MaxOutputBytes: 4096,
	})
	if err != nil {
		return err
	}
	for event := range events {
		if event.Kind == KindDone && event.Status != StatusOK {
			return fmt.Errorf("kernel: installing the platform token failed: %s %s",
				event.Status, event.Error)
		}
	}
	session.pushedToken = token
	return nil
}

// environmentCode renders the hidden cell that installs the environment.
func environmentCode(environment map[string]string) string {
	var builder strings.Builder
	builder.WriteString("import base64 as _b64, os as _os\n")
	for name, value := range environment {
		builder.WriteString(fmt.Sprintf("_os.environ[%q] = _b64.b64decode(%q).decode(\"utf-8\")\n",
			name, base64.StdEncoding.EncodeToString([]byte(value))))
	}
	builder.WriteString("del _b64, _os\n")
	return builder.String()
}

// startKeepaliveLocked reports activity for as long as ODE holds this session.
func (s *Service) startKeepaliveLocked(session *userSession) {
	if session.keepalive != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.root)
	session.keepalive = cancel

	user := session.user.Name
	go func() {
		ticker := time.NewTicker(s.opts.KeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reportCtx, reportCancel := context.WithTimeout(ctx, s.opts.RequestTimeout)
				err := s.hub.ReportActivity(reportCtx, user, time.Now())
				reportCancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("reporting kernel activity to the hub failed", "user", user, "error", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Service) stopKeepaliveLocked(session *userSession) {
	if session.keepalive != nil {
		session.keepalive()
		session.keepalive = nil
	}
}

func (s *Service) dropKernelLocked(session *userSession) {
	if session.conn != nil {
		session.conn.close(nil)
		session.conn = nil
	}
	session.kernelID = ""
	session.pushedToken = ""
	session.running = false
}

func (s *Service) statusLocked(ctx context.Context, session *userSession) (Status, error) {
	state, err := s.hub.ServerState(ctx, session.user.Name)
	if err != nil {
		return Status{}, err
	}
	status := Status{
		User:           session.user.Name,
		ServerReady:    state.Ready,
		ServerPending:  state.Pending,
		ServerURL:      state.URL,
		Started:        state.Started,
		LastActivity:   state.LastActivity,
		KernelID:       session.kernelID,
		Busy:           session.running,
		Profile:        s.opts.Profile,
		Workspace:      s.opts.WorkspacePath,
		WorkspaceReady: session.workspaceReady,
	}
	if session.kernelID != "" {
		status.KernelName = s.opts.KernelName
	}
	return status, nil
}

// handleFor rebuilds a handle for a session that is already up.
func (s *Service) handleFor(bearer string) (KernelHandle, error) {
	user, err := s.UserFor(bearer)
	if err != nil {
		return KernelHandle{}, err
	}
	session, found := s.lookup(user.Name)
	if !found {
		return KernelHandle{}, ErrNoKernel
	}
	session.mux.Lock()
	defer session.mux.Unlock()
	if session.kernelID == "" {
		return KernelHandle{}, ErrNoKernel
	}
	return KernelHandle{
		User: user, ServerURL: ServerURL(session.serverURL),
		Token: session.token, KernelID: session.kernelID, Name: s.opts.KernelName,
	}, nil
}

// reap releases sessions nobody has used, which is what stops ODE keeping a pod
// alive past the developer's interest in it.
func (s *Service) reap() {
	cutoff := time.Now().Add(-s.opts.IdleTimeout)

	s.mux.Lock()
	candidates := make([]*userSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		candidates = append(candidates, session)
	}
	s.mux.Unlock()

	for _, session := range candidates {
		session.mux.Lock()
		if session.running || session.lastUsed.After(cutoff) {
			session.mux.Unlock()
			continue
		}
		name := session.user.Name
		s.releaseLocked(session)
		session.mux.Unlock()

		s.mux.Lock()
		delete(s.sessions, name)
		s.mux.Unlock()
		slog.Info("released an idle kernel session; the pod is now the cluster's to cull", "user", name)
	}
}

func (s *Service) releaseAll() {
	s.mux.Lock()
	sessions := s.sessions
	s.sessions = map[string]*userSession{}
	s.mux.Unlock()

	for _, session := range sessions {
		session.mux.Lock()
		s.releaseLocked(session)
		session.mux.Unlock()
	}
}

// releaseLocked drops ODE's hold without touching the pod: the kernel keeps
// running, the files stay, and the cluster's idle culling applies again.
func (s *Service) releaseLocked(session *userSession) {
	s.stopKeepaliveLocked(session)
	if session.conn != nil {
		session.conn.close(nil)
		session.conn = nil
	}
	session.pushedToken = ""
}
