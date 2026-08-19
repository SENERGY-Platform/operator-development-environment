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

// Package kernel runs developer code in the developer's own JupyterHub pod
// (SPEC §5.6, D2 revised).
//
// The design decision this package implements is mostly a decision not to build
// something. The cluster's existing JupyterHub already provides per-user isolated
// pods, resource limits, idle culling, image control, identity aligned with the
// same Keycloak, and an auto-mounted per-user PVC. ODE therefore spawns through
// the Hub and talks the documented kernel protocol, rather than owning a pod
// spawner and a workspace store of its own.
//
// Three properties are worth understanding before changing anything here.
//
//   - **The kernel inherits exactly the developer's authorisation, and nothing
//     more.** ODE's Hub credential spawns the pod, but the pod's platform access
//     is the developer's own token, pushed in at session start and on each
//     refresh (§5.6 item 4). LLM-authored code and hand-written code have
//     identical access, and neither escalates.
//
//   - **Persistence is the PVC, not ODE.** Only the mounted workspace survives a
//     pod being culled and respawned, so the kernel's working directory is set
//     there. ODE stores no workspace state; that is what makes "a file written in
//     one session is present in the next" true without a store on this side.
//
//   - **Keep-alive is a correctness requirement, not a nicety.** The deployed
//     idle culler kills a server whose reported activity is older than its
//     timeout, and a developer thinking between cells looks exactly like an
//     abandoned pod. Files survive; the loaded dataframe does not.
//
// What the exposure tier does *not* cover is stated here because this is where it
// becomes true: §5.8 puts run_code at L0 behind a confirmation, so confirmed code
// can read values the tier would refuse to a tool. That is the specified design —
// the developer's confirmation is the control, not the tier — and it is why every
// execution is a developer decision.
package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	servicejwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gorilla/websocket"
)

// The §5.6 integration surface, verbatim, so the spec and the code can be read
// against each other. The higher-level operations the API and the tool actually
// call are on *Service below; these are what they are built from.
type (
	// ServerURL is a singleuser server's absolute base URL.
	ServerURL string
	// HubToken is a credential for reaching one developer's server.
	HubToken string
)

// User is the developer whose pod this is.
//
// Name is the JupyterHub username and Sub the Keycloak subject. Both are carried
// because they answer different questions: the Hub addresses pods by name, while
// every ODE record — spend, audit, chat — is keyed by subject. No mapping table
// is needed either way, since both come out of the same validated token.
type User struct {
	Name string `json:"name"`
	Sub  string `json:"sub"`
}

// KernelHandle identifies a running kernel.
type KernelHandle struct {
	User      User      `json:"user"`
	ServerURL ServerURL `json:"server_url"`
	Token     HubToken  `json:"-"`
	KernelID  string    `json:"kernel_id"`
	Name      string    `json:"kernel_name"`
}

// Backend is the interface SPEC §5.6 specifies. *Service implements it.
type Backend interface {
	EnsureServer(ctx context.Context, user User) (ServerURL, error)
	MintToken(ctx context.Context, user User) (HubToken, error)
	StartKernel(ctx context.Context, server ServerURL, token HubToken) (KernelHandle, error)
	Execute(ctx context.Context, handle KernelHandle, code string) (<-chan ExecutionEvent, error)
	Interrupt(ctx context.Context, handle KernelHandle) error
	Shutdown(ctx context.Context, handle KernelHandle) error
}

var _ Backend = (*Service)(nil)

// UsernameClaim names which token claim carries the Hub username.
const (
	// ClaimPreferredUsername is the default, and matches the deployed
	// GenericOAuthenticator: it produces Hub usernames like "jonah". §5.6 assumes
	// the subject instead, which is true of the identity but not of the string.
	ClaimPreferredUsername = "preferred_username"
	// ClaimSub is for a Hub configured with username_claim = sub.
	ClaimSub = "sub"
)

type Options struct {
	// BaseURL is the Hub's public base, e.g. http://proxy-public.jupyterhub.svc.cluster.local.
	// Both /hub/api and the /user/{name}/ proxy hang off it.
	BaseURL string
	// Token is ODE's JupyterHub service token. A deployment secret; never logged,
	// and never handed to a pod — what a pod receives is the narrow per-user token
	// minted from it.
	Token string
	// UsernameClaim is which claim of the developer's token names their Hub user.
	UsernameClaim string
	// KernelName is the kernelspec to start, e.g. "python3".
	KernelName string
	// Profile is the KubeSpawner profile slug to spawn with. Empty takes the
	// deployment's default, which is correct only where there is one profile:
	// §5.6 item 1 ships the ODE image as an additional profile, and a spawn that
	// names none lands on the plain notebook image instead.
	Profile string
	// WorkspacePath is the kernel's working directory, relative to the server root.
	// It has to be inside the mounted PVC or nothing written there survives the pod.
	WorkspacePath string

	// SpawnTimeout bounds waiting for a cold start. §5.6 puts that at 10-60s.
	SpawnTimeout time.Duration
	// RequestTimeout bounds one Hub or jupyter_server REST call.
	RequestTimeout time.Duration
	// ExecuteTimeout bounds one cell. A cell that exceeds it is interrupted, not
	// abandoned, so the kernel is usable again afterwards.
	ExecuteTimeout time.Duration
	// KeepaliveInterval is how often an active session reports activity to the Hub.
	// It must be comfortably below the deployed cull timeout.
	KeepaliveInterval time.Duration
	// IdleTimeout is when ODE lets go of a session it has heard nothing from.
	// Letting go is the point: while ODE holds a session it keeps the pod alive,
	// and a developer who left should not keep a pod running indefinitely.
	IdleTimeout time.Duration
	// TokenTTL is how long a minted per-user token lives.
	TokenTTL time.Duration
	// MaxOutputBytes bounds what one execution may stream to the developer.
	MaxOutputBytes int

	// Environment is pushed into the kernel alongside the platform token, so code
	// in the pod can reach the same platform this ODE is configured against
	// without the developer restating the URLs.
	Environment map[string]string

	// HTTPClient and Dialer are replaced by tests.
	HTTPClient *http.Client
	Dialer     *websocket.Dialer
}

const (
	defaultKernelName        = "python3"
	defaultWorkspacePath     = "data/ode"
	defaultSpawnTimeout      = 180 * time.Second
	defaultRequestTimeout    = 30 * time.Second
	defaultExecuteTimeout    = 10 * time.Minute
	defaultKeepaliveInterval = 5 * time.Minute
	defaultIdleTimeout       = 2 * time.Hour
	defaultTokenTTL          = 12 * time.Hour
	defaultMaxOutputBytes    = 1 << 20
	// tokenRenewBefore re-mints a per-user token this long before it expires, so
	// an execution never starts on a credential that dies mid-cell.
	tokenRenewBefore = 15 * time.Minute
	// spawnPollInterval is how often a pending spawn is re-checked.
	spawnPollInterval = 2 * time.Second
)

// PlatformTokenEnv is the environment variable the developer's platform token
// arrives in. Named as a constant because the singleuser image's helper library
// reads the same name (see deploy/singleuser).
const PlatformTokenEnv = "SENERGY_TOKEN"

// WorkspaceEnv tells code in the pod where its persistent workspace is.
const WorkspaceEnv = "ODE_WORKSPACE"

// Service is ODE's kernel backend.
type Service struct {
	hub  *Hub
	opts Options

	// root bounds everything the service starts on its own initiative — the
	// keep-alive loops and the reaper. Held rather than taken per call because a
	// keep-alive outlives the request that caused it by design, and something has
	// to be able to end it.
	root context.Context
	stop context.CancelFunc

	mux      sync.Mutex
	sessions map[string]*userSession
}

func New(opts Options) (*Service, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("kernel: a jupyterhub url is required")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("kernel: a jupyterhub service token is required")
	}
	switch opts.UsernameClaim {
	case "":
		opts.UsernameClaim = ClaimPreferredUsername
	case ClaimPreferredUsername, ClaimSub:
	default:
		return nil, fmt.Errorf("kernel: username claim %q is not one of %q or %q",
			opts.UsernameClaim, ClaimPreferredUsername, ClaimSub)
	}
	if opts.KernelName == "" {
		opts.KernelName = defaultKernelName
	}
	if opts.WorkspacePath == "" {
		opts.WorkspacePath = defaultWorkspacePath
	}
	if _, err := cleanWorkspacePath(opts.WorkspacePath); err != nil {
		return nil, fmt.Errorf("kernel: workspace path: %w", err)
	}
	if opts.SpawnTimeout <= 0 {
		opts.SpawnTimeout = defaultSpawnTimeout
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}
	if opts.ExecuteTimeout <= 0 {
		opts.ExecuteTimeout = defaultExecuteTimeout
	}
	if opts.KeepaliveInterval <= 0 {
		opts.KeepaliveInterval = defaultKeepaliveInterval
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = defaultIdleTimeout
	}
	if opts.TokenTTL <= 0 {
		opts.TokenTTL = defaultTokenTTL
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = defaultMaxOutputBytes
	}

	base, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("kernel: jupyterhub url %q is not an absolute URL", opts.BaseURL)
	}
	opts.BaseURL = base.String()

	root, stop := context.WithCancel(context.Background())
	return &Service{
		hub:      newHub(opts.BaseURL, opts.Token, opts.HTTPClient, opts.RequestTimeout),
		opts:     opts,
		root:     root,
		stop:     stop,
		sessions: map[string]*userSession{},
	}, nil
}

// Close releases every session and stops the keep-alives. Idempotent.
func (s *Service) Close() {
	s.stop()
	s.releaseAll()
}

// CheckScopes verifies ODE's Hub credential before anything depends on it.
// Called at startup: a missing scope is a deployment fault and fails fast.
func (s *Service) CheckScopes(ctx context.Context) (Identity, []string, error) {
	return s.hub.CheckScopes(ctx)
}

// Workspace is the configured workspace path, for the surfaces that report it.
func (s *Service) Workspace() string { return s.opts.WorkspacePath }

// KernelName is the configured kernelspec.
func (s *Service) KernelName() string { return s.opts.KernelName }

// Start runs the reaper until ctx ends, then releases every session.
//
// The reaper is what stops ODE keeping a pod alive forever. A live session sends
// keep-alives, which is right while a developer is working and wrong once they
// have gone: letting go hands the pod back to the cluster's own idle culling.
func (s *Service) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.opts.KeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.reap()
			case <-ctx.Done():
				s.Close()
				return
			}
		}
	}()
}

// UserFor resolves the developer's Hub user from their platform token.
//
// The token is parsed without verifying it, exactly as pkg/auth does and for the
// same reason: the platform API gateway is what validates signature and expiry
// (§3.1), and this process only reads claims. What is not taken on trust is the
// shape of the name — validateUsername refuses anything ODE would have to escape
// into a URL path.
func (s *Service) UserFor(bearer string) (User, error) {
	parsed, err := servicejwt.Parse(bearer)
	if err != nil {
		return User{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := parsed.Valid(); err != nil {
		return User{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	name := parsed.Username
	if s.opts.UsernameClaim == ClaimSub {
		name = parsed.Sub
	}
	if err := validateUsername(name); err != nil {
		return User{}, err
	}
	return User{Name: name, Sub: parsed.Sub}, nil
}

// ---- The §5.6 Backend, one method at a time ----

// EnsureServer spawns the developer's server if it is not up, and waits for it.
func (s *Service) EnsureServer(ctx context.Context, user User) (ServerURL, error) {
	state, err := s.hub.ServerState(ctx, user.Name)
	if err != nil {
		return "", err
	}
	if state.Ready {
		return ServerURL(s.serverBase(state.URL, user.Name)), nil
	}
	if state.Pending == "" {
		if err := s.hub.StartServer(ctx, user.Name, s.opts.Profile); err != nil {
			return "", err
		}
		slog.InfoContext(ctx, "spawning a singleuser server",
			"user", user.Name, "profile", profileOr(s.opts.Profile))
	}

	deadline := time.Now().Add(s.opts.SpawnTimeout)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(spawnPollInterval):
		}

		state, err = s.hub.ServerState(ctx, user.Name)
		if err != nil {
			return "", err
		}
		if state.Ready {
			return ServerURL(s.serverBase(state.URL, user.Name)), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: %s is still %q after %s",
				ErrSpawnTimeout, user.Name, pendingOr(state.Pending), s.opts.SpawnTimeout)
		}
	}
}

// MintToken issues a fresh per-user token. Callers that want the cached one use
// the session path instead; this is the §5.6 method, and it always mints.
func (s *Service) MintToken(ctx context.Context, user User) (HubToken, error) {
	token, _, err := s.hub.MintToken(ctx, user.Name, s.opts.TokenTTL)
	return token, err
}

// StartKernel starts a kernel in the workspace on an already-running server.
func (s *Service) StartKernel(
	ctx context.Context, server ServerURL, token HubToken,
) (KernelHandle, error) {
	api := s.serverAPIFor(string(server), token)
	if err := api.ensureDirectory(ctx, s.opts.WorkspacePath); err != nil {
		return KernelHandle{}, err
	}
	created, err := api.createKernel(ctx, s.opts.KernelName, s.opts.WorkspacePath)
	if err != nil {
		return KernelHandle{}, err
	}
	return KernelHandle{
		ServerURL: server,
		Token:     token,
		KernelID:  created.ID,
		Name:      created.Name,
	}, nil
}

// Execute runs code on a handle. The channel always ends with a KindDone event.
//
// Most callers want Run instead, which brings the session up first. This is the
// §5.6 method: it takes a handle someone already has and assumes nothing.
func (s *Service) Execute(
	ctx context.Context, handle KernelHandle, code string,
) (<-chan ExecutionEvent, error) {
	if handle.KernelID == "" {
		return nil, ErrNoKernel
	}
	options := executeOptions{
		MaxOutputBytes: s.opts.MaxOutputBytes,
		OnCancel: func() {
			// A fresh context: the caller's is already done, and the interrupt is
			// exactly the request that has to outlive it.
			interruptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.RequestTimeout)
			defer cancel()
			if err := s.Interrupt(interruptCtx, handle); err != nil {
				slog.Warn("interrupting a cancelled execution failed",
					"user", handle.User.Name, "error", err)
			}
		},
	}

	// The developer's own connection when this handle is the session's, which is
	// the normal case and keeps the kernel_info handshake paid once.
	if conn, found := s.liveConnection(handle); found {
		return conn.execute(ctx, code, options)
	}

	// Otherwise a connection of its own, closed when the execution ends. Leaving
	// it open would leak a socket and a goroutine pair per call, and nothing else
	// holds a reference to it.
	conn, err := dial(ctx, s.opts.Dialer,
		channelsEndpoint(string(handle.ServerURL), handle.KernelID), handle.Token, handle.User.Name)
	if err != nil {
		return nil, err
	}
	raw, err := conn.execute(ctx, code, options)
	if err != nil {
		conn.close(nil)
		return nil, err
	}

	out := make(chan ExecutionEvent, 32)
	go func() {
		defer close(out)
		defer conn.close(nil)
		for event := range raw {
			out <- event
		}
	}()
	return out, nil
}

// liveConnection returns the developer's open connection when it is this kernel's.
func (s *Service) liveConnection(handle KernelHandle) (*connection, bool) {
	session, found := s.lookup(handle.User.Name)
	if !found {
		return nil, false
	}
	session.mux.Lock()
	defer session.mux.Unlock()
	if session.conn == nil || session.conn.isClosed() || session.kernelID != handle.KernelID {
		return nil, false
	}
	return session.conn, true
}

// Interrupt stops whatever the kernel is running, without losing its state.
func (s *Service) Interrupt(ctx context.Context, handle KernelHandle) error {
	if handle.KernelID == "" {
		return ErrNoKernel
	}
	return s.serverAPIFor(string(handle.ServerURL), handle.Token).
		interruptKernel(ctx, handle.KernelID)
}

// Shutdown ends the kernel. The pod stays up: it is the developer's, their files
// are on it, and a respawn costs them a cold start.
func (s *Service) Shutdown(ctx context.Context, handle KernelHandle) error {
	if handle.KernelID == "" {
		return ErrNoKernel
	}
	return s.serverAPIFor(string(handle.ServerURL), handle.Token).
		deleteKernel(ctx, handle.KernelID)
}

func (s *Service) serverAPIFor(server string, token HubToken) *serverAPI {
	client := s.opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &serverAPI{
		baseURL: strings.TrimRight(server, "/"),
		token:   token,
		http:    client,
		timeout: s.opts.RequestTimeout,
	}
}

// serverBase turns the Hub's relative server URL into an absolute one.
func (s *Service) serverBase(path, name string) string {
	if path == "" {
		path = "/user/" + url.PathEscape(name) + "/"
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return strings.TrimRight(path, "/")
	}
	return s.opts.BaseURL + "/" + strings.Trim(path, "/")
}

// channelsEndpoint is the kernel's WebSocket URL, with the scheme swapped.
func channelsEndpoint(server, kernelID string) string {
	endpoint := strings.TrimRight(server, "/") + "/api/kernels/" + url.PathEscape(kernelID) + "/channels"
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		return "wss://" + strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		return "ws://" + strings.TrimPrefix(endpoint, "http://")
	default:
		return endpoint
	}
}

func profileOr(profile string) string {
	if profile == "" {
		return "(deployment default)"
	}
	return profile
}

func pendingOr(pending string) string {
	if pending == "" {
		return "not running"
	}
	return pending
}
