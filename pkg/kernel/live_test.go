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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// The one test in this package that talks to a real JupyterHub. Skipped unless
// both variables are set, because everything else has to pass without a cluster:
//
//	ODE_JUPYTERHUB_URL=http://proxy-public.<hub-namespace>.svc.cluster.local \
//	ODE_JUPYTERHUB_TOKEN=... \
//	ODE_JUPYTERHUB_USER=devuser \
//	go test ./pkg/kernel/ -run Live -v
//
// It exists because the parts of §5.6 that are easiest to get wrong — the spawn
// poll, the scope grant, the WebSocket handshake race, and above all whether a
// file survives a kernel restart — are precisely the parts a fake cannot check.
func liveService(t *testing.T) (*kernel.Service, string) {
	t.Helper()
	baseURL, token := os.Getenv("ODE_JUPYTERHUB_URL"), os.Getenv("ODE_JUPYTERHUB_TOKEN")
	if baseURL == "" || token == "" {
		t.Skip("set ODE_JUPYTERHUB_URL and ODE_JUPYTERHUB_TOKEN to run the live test")
	}
	user := os.Getenv("ODE_JUPYTERHUB_USER")
	if user == "" {
		t.Fatal("set ODE_JUPYTERHUB_USER to the hub username the token may act for")
	}

	service, err := kernel.New(kernel.Options{
		BaseURL:        baseURL,
		Token:          token,
		WorkspacePath:  "data/ode-live-test",
		SpawnTimeout:   3 * time.Minute,
		ExecuteTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service, unsignedToken(user)
}

// unsignedToken builds the claims half of a JWT. ODE parses the developer's
// token without verifying it — the gateway is what validates (§3.1) — so this is
// all the kernel path reads.
func unsignedToken(username string) string {
	claims, _ := json.Marshal(map[string]any{
		"sub":                "live-test-" + username,
		"preferred_username": username,
		"realm_access":       map[string][]string{"roles": {"developer"}},
	})
	encode := base64.RawURLEncoding.EncodeToString
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	return fmt.Sprintf("%s.%s.", encode(header), encode(claims))
}

func TestLiveTheCredentialHoldsEveryScopeM4Needs(t *testing.T) {
	service, _ := liveService(t)

	identity, warnings, err := service.CheckScopes(context.Background())
	if err != nil {
		t.Fatalf("CheckScopes: %v", err)
	}
	t.Logf("credential: kind=%s name=%s scopes=%v", identity.Kind, identity.Name, identity.Scopes)
	for _, warning := range warnings {
		t.Logf("warning: %s", warning)
	}
}

func TestLiveAFileWrittenInOneSessionIsPresentInTheNext(t *testing.T) {
	service, bearer := liveService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	status, err := service.Ensure(ctx, ref(bearer))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	t.Logf("session up: server=%v kernel=%s workspace=%s",
		status.ServerReady, status.KernelID, status.Workspace)

	marker := fmt.Sprintf("m4-%d", time.Now().UnixNano())
	write := fmt.Sprintf(`open("marker.txt", "w").write(%q); print("written")`, marker)
	if out := runLive(t, ctx, service, bearer, write); !strings.Contains(out, "written") {
		t.Fatalf("writing the marker produced %q", out)
	}

	// A restart is the strong form of the acceptance criterion: the kernel that
	// wrote the file is gone, and only the PVC could be carrying it.
	if _, err := service.Restart(ctx, ref(bearer)); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	read := `print(open("marker.txt").read())`
	if out := runLive(t, ctx, service, bearer, read); !strings.Contains(out, marker) {
		t.Fatalf("after a restart the marker read back as %q, want %q", out, marker)
	}

	files, err := service.Files(ctx, ref(bearer), "")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var found bool
	for _, entry := range files {
		if entry.Name == "marker.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("the workspace listing does not contain marker.txt: %+v", files)
	}
}

func TestLiveThePlatformTokenIsReadableInsideTheKernel(t *testing.T) {
	service, bearer := liveService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	if _, err := service.Ensure(ctx, ref(bearer)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	out := runLive(t, ctx, service, bearer,
		`import os; print("token_len", len(os.environ.get("SENERGY_TOKEN", ""))); print("cwd", os.getcwd())`)
	if strings.Contains(out, "token_len 0") {
		t.Errorf("the platform token did not reach the kernel: %q", out)
	}
	t.Logf("kernel reports: %s", strings.TrimSpace(out))
}

func TestLiveAnExceptionComesBackAsAnError(t *testing.T) {
	service, bearer := liveService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	events, err := service.Run(ctx, ref(bearer), "raise ValueError('deliberate')")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawError bool
	var status string
	for event := range events {
		switch event.Kind {
		case kernel.KindError:
			sawError = true
			if event.ErrorName != "ValueError" {
				t.Errorf("error name = %q, want ValueError", event.ErrorName)
			}
		case kernel.KindDone:
			status = event.Status
		}
	}
	if !sawError {
		t.Error("no error event was streamed for code that raises")
	}
	if status != kernel.StatusError {
		t.Errorf("final status = %q, want %q", status, kernel.StatusError)
	}
}

// runLive executes and returns everything the cell printed.
func runLive(t *testing.T, ctx context.Context, service *kernel.Service, bearer, code string) string {
	t.Helper()
	events, err := service.Run(ctx, ref(bearer), code)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var out strings.Builder
	for event := range events {
		switch event.Kind {
		case kernel.KindStream, kernel.KindResult:
			out.WriteString(event.Text)
		case kernel.KindError:
			t.Fatalf("the cell raised: %s: %s", event.ErrorName, event.ErrorValue)
		case kernel.KindDone:
			if event.Status != kernel.StatusOK {
				t.Fatalf("the cell finished %q: %s", event.Status, event.Error)
			}
		}
	}
	return out.String()
}
