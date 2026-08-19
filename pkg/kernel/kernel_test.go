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

package kernel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel/kerneltest"
)

func newService(t *testing.T, hub *kerneltest.Hub, adjust func(*kernel.Options)) *kernel.Service {
	t.Helper()
	opts := kernel.Options{
		BaseURL:        hub.URL(),
		Token:          "service-token",
		WorkspacePath:  "data/ode",
		SpawnTimeout:   5 * time.Second,
		RequestTimeout: 5 * time.Second,
		ExecuteTimeout: 5 * time.Second,
	}
	if adjust != nil {
		adjust(&opts)
	}
	service, err := kernel.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Releases the keep-alive loops, which otherwise outlive the fake hub and
	// spend the rest of the run reporting activity to a closed listener.
	t.Cleanup(service.Close)
	return service
}

// collect drains an execution and returns what it produced.
type collected struct {
	stdout    string
	result    string
	mime      map[string]string
	status    string
	errorName string
	truncated bool
	kinds     []kernel.EventKind
}

func collect(t *testing.T, events <-chan kernel.ExecutionEvent) collected {
	t.Helper()
	var out collected
	for event := range events {
		out.kinds = append(out.kinds, event.Kind)
		switch event.Kind {
		case kernel.KindStream:
			out.stdout += event.Text
		case kernel.KindResult:
			out.result += event.Text
			if event.MIME != nil {
				out.mime = event.MIME
			}
		case kernel.KindError:
			out.errorName = event.ErrorName
		case kernel.KindDone:
			out.status = event.Status
			out.truncated = event.Truncated
		}
	}
	return out
}

func TestNewRejectsAUsernameClaimItCannotRead(t *testing.T) {
	_, err := kernel.New(kernel.Options{
		BaseURL: "http://hub", Token: "t", UsernameClaim: "email",
	})
	if err == nil || !strings.Contains(err.Error(), "username claim") {
		t.Fatalf("error = %v, want a complaint about the username claim", err)
	}
}

func TestNewRequiresAnAbsoluteHubURL(t *testing.T) {
	if _, err := kernel.New(kernel.Options{BaseURL: "proxy-public", Token: "t"}); err == nil {
		t.Fatal("a relative hub url was accepted")
	}
}

func TestCheckScopesFailsStartupWhenTheCredentialCannotSpawn(t *testing.T) {
	hub := kerneltest.NewHub(t)
	hub.Scopes = []string{"access:servers", "read:users"}
	service := newService(t, hub, nil)

	_, _, err := service.CheckScopes(context.Background())
	var scopeErr *kernel.ScopeError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("error = %v, want a ScopeError", err)
	}
	if len(scopeErr.Missing) != 3 {
		t.Errorf("missing = %v, want servers, tokens and users:activity", scopeErr.Missing)
	}
}

func TestCheckScopesWarnsWhenTheCredentialOnlyCoversOneUser(t *testing.T) {
	hub := kerneltest.NewHub(t)
	hub.Kind = "user"
	hub.Scopes = []string{
		"servers!user=jonah", "tokens!user=jonah",
		"access:servers!user=jonah", "users:activity!user=jonah",
	}
	service := newService(t, hub, nil)

	_, warnings, err := service.CheckScopes(context.Background())
	if err != nil {
		t.Fatalf("CheckScopes: %v", err)
	}
	if len(warnings) != 5 {
		t.Fatalf("warnings = %v, want one per restricted scope plus the credential kind", warnings)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "not a service token") {
		t.Errorf("warnings do not mention the credential kind: %v", warnings)
	}
}

func TestUserForReadsTheHubNameFromThePreferredUsernameClaim(t *testing.T) {
	service := newService(t, kerneltest.NewHub(t), nil)

	user, err := service.UserFor(unsignedToken("jonah"))
	if err != nil {
		t.Fatalf("UserFor: %v", err)
	}
	if user.Name != "jonah" {
		t.Errorf("name = %q, want the preferred_username claim", user.Name)
	}
	if user.Sub != "live-test-jonah" {
		t.Errorf("sub = %q, want the subject claim", user.Sub)
	}
}

func TestUserForRefusesANameItWouldHaveToEscapeIntoAPath(t *testing.T) {
	service := newService(t, kerneltest.NewHub(t), nil)

	if _, err := service.UserFor(unsignedToken("../admin")); !errors.Is(err, kernel.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest for a traversal-shaped username", err)
	}
}

func TestEnsureSpawnsPollsAndStartsAKernelInTheWorkspace(t *testing.T) {
	hub := kerneltest.NewHub(t)
	hub.Ready = false         // Nothing running, so ODE has to ask for a spawn.
	hub.SpawnsBeforeReady = 2 // And then poll twice before it comes up.
	service := newService(t, hub, func(o *kernel.Options) {
		o.SpawnTimeout = 10 * time.Second
	})

	status, err := service.Ensure(context.Background(), unsignedToken("jonah"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !status.ServerReady || status.KernelID != "kernel-1" {
		t.Fatalf("status = %+v, want a ready server and a kernel", status)
	}

	got := hub.Calls()
	if len(got.StartedServers) != 1 || got.StartedServers[0] != "jonah" {
		t.Errorf("started servers = %v, want one spawn for jonah", got.StartedServers)
	}
	if len(got.CreatedKernels) != 1 || got.CreatedKernels[0].Path != "data/ode" {
		t.Errorf("created kernels = %+v, want one in the workspace", got.CreatedKernels)
	}
	// The workspace has to exist before the kernel is told to run in it, or
	// jupyter_server starts the kernel in the server root instead.
	if len(got.Directories) != 2 || got.Directories[1] != "data/ode" {
		t.Errorf("created directories = %v, want each segment of the workspace", got.Directories)
	}
}

func TestASpawnAsksForTheConfiguredKubespawnerProfile(t *testing.T) {
	hub := kerneltest.NewHub(t)
	hub.Ready = false
	service := newService(t, hub, func(o *kernel.Options) { o.Profile = "ode" })

	if _, err := service.Ensure(context.Background(), unsignedToken("jonah")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// §5.6 item 1 adds the ODE image as a profile rather than replacing the
	// default, so a spawn that names none lands on the plain notebook image.
	if profiles := hub.Calls().SpawnProfiles; len(profiles) != 1 || profiles[0] != "ode" {
		t.Errorf("spawn profiles = %v, want the configured one", profiles)
	}
}

func TestASpawnWithNoConfiguredProfileTakesTheDeploymentDefault(t *testing.T) {
	hub := kerneltest.NewHub(t)
	hub.Ready = false
	service := newService(t, hub, nil)

	if _, err := service.Ensure(context.Background(), unsignedToken("jonah")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if profiles := hub.Calls().SpawnProfiles; len(profiles) != 1 || profiles[0] != "" {
		t.Errorf("spawn profiles = %v, want an unset profile", profiles)
	}
}

func TestEnsureMintsANarrowlyScopedShortLivedTokenForThePod(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, func(o *kernel.Options) { o.TokenTTL = 90 * time.Minute })

	if _, err := service.Ensure(context.Background(), unsignedToken("jonah")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	got := hub.Calls()
	if len(got.MintedTokens) != 1 {
		t.Fatalf("minted %d tokens, want one", len(got.MintedTokens))
	}
	minted := got.MintedTokens[0]
	if minted.ExpiresIn != 5400 {
		t.Errorf("expires_in = %d, want the configured ttl in seconds", minted.ExpiresIn)
	}
	if len(minted.Scopes) != 1 || minted.Scopes[0] != "access:servers!user=jonah" {
		t.Errorf("scopes = %v, want only access to this user's own server", minted.Scopes)
	}
	// The Hub API is only ever called with ODE's service credential; a developer's
	// token reaching it would be a different service acting as them.
	for _, header := range got.ServiceTokens {
		if header != "token service-token" {
			t.Fatalf("a hub call used %q rather than ODE's service token", header)
		}
	}
}

func TestRunStreamsOutputAndEndsWithADoneEvent(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)

	events, err := service.Run(context.Background(), unsignedToken("jonah"), "print('hello')")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collect(t, events)

	if got.stdout != "hello\n" {
		t.Errorf("stdout = %q, want the streamed text", got.stdout)
	}
	if got.result != "42" {
		t.Errorf("result = %q, want the text/plain rendering", got.result)
	}
	if got.mime["image/png"] == "" {
		t.Error("the png rendering was dropped; the developer's own console keeps it")
	}
	if got.status != kernel.StatusOK {
		t.Errorf("status = %q, want ok", got.status)
	}
	if got.kinds[len(got.kinds)-1] != kernel.KindDone {
		t.Errorf("the stream ended on %q, want a done event", got.kinds[len(got.kinds)-1])
	}
}

func TestRunInstallsThePlatformTokenSilentlyBeforeTheDevelopersCode(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, func(o *kernel.Options) {
		o.Environment = map[string]string{"SENERGY_DEVICE_REPO_URL": "https://api.example/device-repository"}
	})

	events, err := service.Run(context.Background(), unsignedToken("jonah"), "print('hello')")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collect(t, events)

	// Nothing of the hidden cell reaches the developer.
	if strings.Contains(got.stdout, "SENERGY_TOKEN") || strings.Contains(got.stdout, "b64decode") {
		t.Errorf("the hidden environment cell leaked into the output: %q", got.stdout)
	}

	executed := hub.Calls().Executed
	if len(executed) != 2 {
		t.Fatalf("executed %d cells, want the environment push then the developer's code", len(executed))
	}
	push := executed[0]
	for _, name := range []string{kernel.PlatformTokenEnv, kernel.WorkspaceEnv, "SENERGY_DEVICE_REPO_URL"} {
		if !strings.Contains(push, name) {
			t.Errorf("the environment cell does not set %s: %s", name, push)
		}
	}
	// The values are base64 in the source, so a token never appears verbatim in
	// anything the kernel records as history or a traceback quotes back.
	if strings.Contains(push, unsignedToken("jonah")) {
		t.Error("the platform token was interpolated into the source verbatim")
	}
	if executed[1] != "print('hello')" {
		t.Errorf("second cell = %q, want the developer's code", executed[1])
	}
}

func TestRunPushesTheTokenOnceAndAgainWhenItChanges(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	for range 2 {
		events, err := service.Run(context.Background(), bearer, "pass")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		collect(t, events)
	}
	if executed := hub.Calls().Executed; len(executed) != 3 {
		t.Fatalf("executed %v, want one push and two cells", executed)
	}

	// A refreshed token is a different string, so it is installed again — that is
	// the whole of §5.6 item 4.
	if err := service.RefreshPlatformToken(context.Background(), bearer+"x"); err != nil {
		t.Fatalf("RefreshPlatformToken: %v", err)
	}
	if executed := hub.Calls().Executed; len(executed) != 4 {
		t.Fatalf("executed %d cells, want a second environment push", len(executed))
	}
}

func TestRunReportsAKernelExceptionAsAnErrorRatherThanAFailure(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)

	// Brought up first, so the arming below lands on the developer's cell rather
	// than on the hidden environment push that precedes it.
	if _, err := service.Ensure(context.Background(), unsignedToken("jonah")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	hub.ArmFailure()

	events, err := service.Run(context.Background(), unsignedToken("jonah"), "raise ValueError()")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collect(t, events)
	if got.errorName != "ValueError" {
		t.Errorf("error name = %q, want ValueError", got.errorName)
	}
	if got.status != kernel.StatusError {
		t.Errorf("status = %q, want error", got.status)
	}
}

func TestRunRefusesASecondCellWhileOneIsRunning(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	// Brought up first, so the hang below lands on the developer's cell rather
	// than on the hidden environment push that precedes it.
	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	release := make(chan struct{})
	hub.Hang(release)

	first, err := service.Run(context.Background(), bearer, "slow()")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := service.Run(context.Background(), bearer, "other()"); !errors.Is(err, kernel.ErrBusy) {
		t.Errorf("second Run = %v, want ErrBusy", err)
	}
	close(release)
	collect(t, first)

	// Once the first cell is over the kernel is usable again.
	events, err := service.Run(context.Background(), bearer, "after()")
	if err != nil {
		t.Fatalf("Run after the first finished: %v", err)
	}
	collect(t, events)
}

func TestACancelledExecutionInterruptsTheKernelRatherThanLeavingItRunning(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	hub.Hang(release)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := service.Run(ctx, bearer, "while True: pass")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	got := collect(t, events)

	if got.status != kernel.StatusInterrupted {
		t.Errorf("status = %q, want interrupted", got.status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(hub.Calls().Interrupts) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the kernel was never interrupted, so the cell would keep running")
}

func TestOutputIsTruncatedAtTheByteCapAndSaysSo(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, func(o *kernel.Options) { o.MaxOutputBytes = 4 })

	events, err := service.Run(context.Background(), unsignedToken("jonah"), "print('hello')")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := collect(t, events)

	if len(got.stdout) > 4 {
		t.Errorf("stdout = %q, want it capped at four bytes", got.stdout)
	}
	if !got.truncated {
		t.Error("the done event does not report truncation, so a partial answer reads as a whole one")
	}
}

func TestRestartReplacesTheKernelWithoutTouchingTheWorkspace(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	hub.SetNextKernelID("kernel-2")

	status, err := service.Restart(context.Background(), bearer)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if status.KernelID != "kernel-2" {
		t.Errorf("kernel = %q, want a new one", status.KernelID)
	}

	got := hub.Calls()
	if len(got.DeletedKernels) != 1 || got.DeletedKernels[0] != "kernel-1" {
		t.Errorf("deleted = %v, want the old kernel", got.DeletedKernels)
	}
	// The pod is never stopped: the workspace and everything else on it is the
	// developer's, and a respawn would cost them a cold start.
	if len(got.StartedServers) != 0 {
		t.Errorf("the server was respawned during a restart: %v", got.StartedServers)
	}
}

func TestAKernelThatVanishedIsReplacedRatherThanReported(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// The pod was culled and respawned: ODE's remembered kernel is gone.
	hub.KillKernel("kernel-1")
	hub.SetNextKernelID("kernel-2")

	status, err := service.Ensure(context.Background(), bearer)
	if err != nil {
		t.Fatalf("Ensure after the kernel vanished: %v", err)
	}
	if status.KernelID != "kernel-2" {
		t.Errorf("kernel = %q, want a freshly started one", status.KernelID)
	}
}

func TestAStoppedPodIsRespawnedRatherThanReportedAsA403(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// The developer stopped their own server from the JupyterHub UI, or the idle
	// culler did. ODE still remembers the route, and there is no longer a pod
	// behind it - the Hub answers instead, with a page.
	hub.StopServer()
	hub.SetNextKernelID("kernel-2")

	status, err := service.Ensure(context.Background(), bearer)
	if err != nil {
		t.Fatalf("Ensure after the pod was stopped: %v", err)
	}
	if status.KernelID != "kernel-2" {
		t.Errorf("kernel = %q, want one from the new pod", status.KernelID)
	}

	// One spawn, for the replacement: the first bring-up found a server already up.
	calls := hub.Calls()
	if len(calls.StartedServers) != 1 || calls.StartedServers[0] != "jonah" {
		t.Errorf("started servers = %v, want one spawn for the replacement pod",
			calls.StartedServers)
	}
	// The workspace directory is on the PVC and survives, but "already ensured" was
	// a statement about a server that no longer exists - so both of its segments are
	// created again on the new pod.
	if len(calls.Directories) != 4 {
		t.Errorf("workspace segments created = %v, want each of the two on each pod",
			calls.Directories)
	}

	// And the session works, rather than merely reporting that it does.
	events, err := service.Run(context.Background(), bearer, "print('hello')")
	if err != nil {
		t.Fatalf("Run on the replacement pod: %v", err)
	}
	if got := collect(t, events); got.stdout != "hello\n" {
		t.Errorf("stdout = %q, want the cell to have run", got.stdout)
	}
}

func TestTheWorkspaceListingSurvivesThePodBeingStopped(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	hub.StopServer()

	entries, err := service.Files(context.Background(), bearer, "")
	if err != nil {
		t.Fatalf("Files after the pod was stopped: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "marker.txt" {
		t.Errorf("entries = %+v, want the workspace of the replacement pod", entries)
	}
}

func TestAHubPageIsReportedAsOneRatherThanAsFourKilobytesOfMarkup(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// A spawn the Hub calls ready while the route to it still answers with a page.
	// ODE cannot recover from that, so what it says is all the developer gets.
	hub.StopServerAndStayStopped()

	_, err := service.Ensure(context.Background(), bearer)
	if err == nil {
		t.Fatal("Ensure succeeded against a server that answers with a hub page")
	}
	if strings.Contains(err.Error(), "<") {
		t.Errorf("the error carries the hub's markup: %v", err)
	}
	if !strings.Contains(err.Error(), "html page") {
		t.Errorf("error = %v, want it to say the answer was a page", err)
	}
}

func TestKeepaliveReportsActivityWhileASessionIsHeld(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, func(o *kernel.Options) {
		o.KeepaliveInterval = 20 * time.Millisecond
	})

	if _, err := service.Ensure(context.Background(), unsignedToken("jonah")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(hub.Calls().Activity) >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("no activity was reported, so the idle culler would kill the kernel mid-task")
}

func TestShutdownEndsTheKernelAndStopsTheKeepalive(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, func(o *kernel.Options) {
		o.KeepaliveInterval = 20 * time.Millisecond
	})
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := service.ShutdownUser(context.Background(), bearer); err != nil {
		t.Fatalf("ShutdownUser: %v", err)
	}
	if deleted := hub.Calls().DeletedKernels; len(deleted) != 1 {
		t.Fatalf("deleted = %v, want the kernel", deleted)
	}

	before := len(hub.Calls().Activity)
	time.Sleep(120 * time.Millisecond)
	if after := len(hub.Calls().Activity); after > before+1 {
		t.Errorf("activity kept being reported after shutdown (%d then %d): "+
			"ODE would hold the pod open for a developer who left", before, after)
	}
}

func TestFilesListsTheWorkspace(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)

	entries, err := service.Files(context.Background(), unsignedToken("jonah"), "")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "marker.txt" {
		t.Errorf("entries = %+v, want the workspace listing", entries)
	}
}

func TestFilesRefusesToClimbOutOfTheWorkspace(t *testing.T) {
	service := newService(t, kerneltest.NewHub(t), nil)

	_, err := service.Files(context.Background(), unsignedToken("jonah"), "../../etc")
	if !errors.Is(err, kernel.ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
}

func TestInterruptWithoutAKernelSaysSoRatherThanFailing(t *testing.T) {
	service := newService(t, kerneltest.NewHub(t), nil)

	if err := service.InterruptUser(context.Background(), unsignedToken("jonah")); !errors.Is(err, kernel.ErrNoKernel) {
		t.Fatalf("error = %v, want ErrNoKernel", err)
	}
}

func TestASpawnThatNeverBecomesReadyTimesOutWithItsOwnError(t *testing.T) {
	hub := kerneltest.NewHub(t)
	hub.Ready = false
	hub.SpawnsBeforeReady = 1000
	service := newService(t, hub, func(o *kernel.Options) {
		o.SpawnTimeout = 100 * time.Millisecond
	})

	_, err := service.Ensure(context.Background(), unsignedToken("jonah"))
	if !errors.Is(err, kernel.ErrSpawnTimeout) {
		t.Fatalf("error = %v, want ErrSpawnTimeout", err)
	}
}

func TestATracebackIsBothKeptVerbatimAndOfferedWithoutColourCodes(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("jonah")

	if _, err := service.Ensure(context.Background(), bearer); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	hub.ArmFailure()

	events, err := service.Run(context.Background(), bearer, "raise ValueError()")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var event kernel.ExecutionEvent
	for e := range events {
		if e.Kind == kernel.KindError {
			event = e
		}
	}

	if len(event.Traceback) != 1 || !strings.Contains(event.Traceback[0], "\x1b[") {
		t.Errorf("traceback = %q, want the kernel's own formatting kept", event.Traceback)
	}
	if strings.Contains(event.Text, "\x1b") {
		t.Errorf("text = %q, want the escape codes removed", event.Text)
	}
	if event.Text != "ValueError: deliberate" {
		t.Errorf("text = %q, want the traceback in plain form", event.Text)
	}
}
