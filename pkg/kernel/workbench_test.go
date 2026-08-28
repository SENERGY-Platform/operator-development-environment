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

// What one pod with several kernels has to hold: the kernels are independent, the
// things the Hub knows about are not duplicated, and the keep-alive belongs to the
// pod rather than to any one of them.

// checkouts is a stand-in for the repository surface, which is what tells the
// kernel service where a workbench's checkout is.
type checkouts map[string]string

func (c checkouts) Checkout(_ context.Context, _, workbench string) (string, error) {
	return c[workbench], nil
}

// failingCheckouts stands in for a store that cannot be read.
type failingCheckouts struct{}

func (failingCheckouts) Checkout(context.Context, string, string) (string, error) {
	return "", errors.New("the database is not answering")
}

func benchRef(bearer, workbench string) kernel.Ref {
	return kernel.Ref{Bearer: bearer, Workbench: workbench}
}

func TestTwoWorkbenchesGetTwoKernelsInOnePod(t *testing.T) {
	hub := kerneltest.NewHub(t)
	hub.Ready = false // Nothing running yet, so a spawn is needed — once, for both.
	service := newService(t, hub, nil)
	service.UseWorkbenches(checkouts{
		"wb-forecast":  "devuser/pv-forecast",
		"wb-anomalies": "devuser/anomaly-detect",
	})
	bearer := unsignedToken("devuser")

	forecast, err := service.Ensure(context.Background(), benchRef(bearer, "wb-forecast"))
	if err != nil {
		t.Fatalf("Ensure(forecast): %v", err)
	}
	anomalies, err := service.Ensure(context.Background(), benchRef(bearer, "wb-anomalies"))
	if err != nil {
		t.Fatalf("Ensure(anomalies): %v", err)
	}

	if forecast.KernelID == anomalies.KernelID {
		t.Fatalf("both workbenches got kernel %q", forecast.KernelID)
	}
	if forecast.Directory != "devuser/pv-forecast" {
		t.Errorf("the forecast kernel runs in %q", forecast.Directory)
	}
	if anomalies.Directory != "devuser/anomaly-detect" {
		t.Errorf("the anomaly kernel runs in %q", anomalies.Directory)
	}
	if anomalies.KernelCount != 2 {
		t.Errorf("kernel_count = %d, want the 2 ODE started", anomalies.KernelCount)
	}

	// The whole reason this is one pod: the second workbench costs a kernel, and
	// not a spawn, a token or a second keep-alive loop.
	got := hub.Calls()
	if len(got.StartedServers) != 1 {
		t.Errorf("started servers = %v, want one spawn for two workbenches", got.StartedServers)
	}
	if len(got.MintedTokens) != 1 {
		t.Errorf("minted tokens = %d, want one for two workbenches", len(got.MintedTokens))
	}
	if len(got.CreatedKernels) != 2 {
		t.Fatalf("created kernels = %+v, want two", got.CreatedKernels)
	}
	// Each kernel starts in its own workbench's checkout, which is what makes
	// open("notes.txt") in a cell land next to that operator's code.
	paths := map[string]bool{}
	for _, created := range got.CreatedKernels {
		paths[created.Path] = true
	}
	for _, want := range []string{"data/ode/devuser/pv-forecast", "data/ode/devuser/anomaly-detect"} {
		if !paths[want] {
			t.Errorf("no kernel was started in %q; started in %v", want, paths)
		}
	}
}

func TestABusyWorkbenchDoesNotMakeAnotherOneBusy(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("devuser")

	// Both brought up before anything hangs, so the hang lands on a developer's
	// cell rather than on the hidden environment push that precedes it.
	for _, workbench := range []string{"wb-one", "wb-two"} {
		if _, err := service.Ensure(context.Background(), benchRef(bearer, workbench)); err != nil {
			t.Fatalf("Ensure(%s): %v", workbench, err)
		}
	}

	release := make(chan struct{})
	hub.Hang(release)

	training, err := service.Run(context.Background(), benchRef(bearer, "wb-one"), "train()")
	if err != nil {
		t.Fatalf("Run(wb-one): %v", err)
	}
	// The same workbench still refuses a second cell: one kernel, one cell.
	if _, err := service.Run(
		context.Background(), benchRef(bearer, "wb-one"), "again()"); !errors.Is(err, kernel.ErrBusy) {
		t.Errorf("a second cell in the same workbench = %v, want ErrBusy", err)
	}
	// The other one does not, and that is the whole change: a training run in one
	// operator no longer stops work on the next.
	other, err := service.Run(context.Background(), benchRef(bearer, "wb-two"), "explore()")
	if err != nil {
		t.Fatalf("Run(wb-two) while wb-one is training: %v", err)
	}

	close(release)
	collect(t, training)
	collect(t, other)
}

func TestShuttingDownOneWorkbenchLeavesTheOtherAlone(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("devuser")

	one, err := service.Ensure(context.Background(), benchRef(bearer, "wb-one"))
	if err != nil {
		t.Fatalf("Ensure(wb-one): %v", err)
	}
	if _, err := service.Ensure(context.Background(), benchRef(bearer, "wb-two")); err != nil {
		t.Fatalf("Ensure(wb-two): %v", err)
	}

	if err := service.ShutdownUser(context.Background(), benchRef(bearer, "wb-two")); err != nil {
		t.Fatalf("ShutdownUser(wb-two): %v", err)
	}

	status, err := service.Status(context.Background(), benchRef(bearer, "wb-one"))
	if err != nil {
		t.Fatalf("Status(wb-one): %v", err)
	}
	if status.KernelID != one.KernelID {
		t.Errorf("wb-one's kernel is now %q, was %q", status.KernelID, one.KernelID)
	}
	if status.KernelCount != 1 {
		t.Errorf("kernel_count = %d after shutting one of two down", status.KernelCount)
	}
	if deleted := hub.Calls().DeletedKernels; len(deleted) != 1 {
		t.Errorf("deleted kernels = %v, want only the one that was shut down", deleted)
	}
}

// The failure this prevents is the worst one in the whole arrangement: the
// keep-alive is what stops the Hub's idle culler killing a pod, so stopping it
// while a workbench is still training kills the training run.
func TestTheKeepAliveOutlivesOneWorkbenchAndStopsWithTheLast(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, func(o *kernel.Options) {
		o.KeepaliveInterval = 20 * time.Millisecond
	})
	bearer := unsignedToken("devuser")

	for _, workbench := range []string{"wb-one", "wb-two"} {
		if _, err := service.Ensure(context.Background(), benchRef(bearer, workbench)); err != nil {
			t.Fatalf("Ensure(%s): %v", workbench, err)
		}
	}
	waitForActivity(t, hub, 1)

	// One workbench closed, one still open: the pod is still being held up, and a
	// developer training in the other one keeps their run.
	if err := service.ShutdownUser(context.Background(), benchRef(bearer, "wb-one")); err != nil {
		t.Fatalf("ShutdownUser(wb-one): %v", err)
	}
	before := len(hub.Calls().Activity)
	waitForActivity(t, hub, before+2)

	// The last one closed: ODE lets go, and the pod becomes the cluster's to cull
	// again, which is §5.6's arrangement.
	if err := service.ShutdownUser(context.Background(), benchRef(bearer, "wb-two")); err != nil {
		t.Fatalf("ShutdownUser(wb-two): %v", err)
	}
	settled := len(hub.Calls().Activity)
	time.Sleep(150 * time.Millisecond) // Several intervals, so a live loop would show.
	if after := len(hub.Calls().Activity); after > settled+1 {
		t.Errorf("activity reports went from %d to %d after the last workbench closed",
			settled, after)
	}
}

// waitForActivity blocks until the hub has recorded at least count reports.
func waitForActivity(t *testing.T, hub *kerneltest.Hub, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(hub.Calls().Activity) >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the hub saw %d activity reports, want at least %d",
		len(hub.Calls().Activity), count)
}

func TestAWorkbenchWhoseCheckoutMovedGetsANewKernelThere(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	where := checkouts{"wb-one": "devuser/pv-forecast"}
	service.UseWorkbenches(where)
	bearer := unsignedToken("devuser")

	first, err := service.Ensure(context.Background(), benchRef(bearer, "wb-one"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// The developer selects a different repository into the same workbench. A
	// kernel left where it was would write their next file into the previous
	// operator's checkout.
	where["wb-one"] = "devuser/anomaly-detect"

	second, err := service.Ensure(context.Background(), benchRef(bearer, "wb-one"))
	if err != nil {
		t.Fatalf("Ensure after the checkout moved: %v", err)
	}
	if second.KernelID == first.KernelID {
		t.Errorf("the kernel was not restarted; it is still %q in %q",
			second.KernelID, second.Directory)
	}
	if second.Directory != "devuser/anomaly-detect" {
		t.Errorf("the new kernel runs in %q", second.Directory)
	}
	if deleted := hub.Calls().DeletedKernels; len(deleted) != 1 || deleted[0] != first.KernelID {
		t.Errorf("deleted kernels = %v, want the one that was left behind", deleted)
	}
}

func TestAnUnreadableCheckoutStillGetsAKernel(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	service.UseWorkbenches(failingCheckouts{})

	// A store that cannot answer is a degraded store, not a reason to leave the
	// developer with no kernel at all. It runs in the workspace root and says so.
	status, err := service.Ensure(
		context.Background(), benchRef(unsignedToken("devuser"), "wb-one"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if status.KernelID == "" {
		t.Fatal("no kernel was started")
	}
	if status.Directory != "" {
		t.Errorf("directory = %q, want the workspace root", status.Directory)
	}
}

func TestARefreshedTokenReachesEveryWorkbench(t *testing.T) {
	hub := kerneltest.NewHub(t)
	service := newService(t, hub, nil)
	bearer := unsignedToken("devuser")

	for _, workbench := range []string{"wb-one", "wb-two"} {
		if _, err := service.Ensure(context.Background(), benchRef(bearer, workbench)); err != nil {
			t.Fatalf("Ensure(%s): %v", workbench, err)
		}
	}
	before := countEnvironmentPushes(hub)

	// The token is the developer's rather than any one workbench's, so a workbench
	// left holding an expired one would fail its next platform call for reasons
	// that have nothing to do with what the developer was doing in it.
	if err := service.RefreshPlatformToken(context.Background(), bearer+"x"); err != nil {
		t.Fatalf("RefreshPlatformToken: %v", err)
	}
	if pushed := countEnvironmentPushes(hub) - before; pushed != 2 {
		t.Errorf("the refreshed token was installed in %d kernels, want both", pushed)
	}
}

// countEnvironmentPushes counts the hidden cells that install the environment.
func countEnvironmentPushes(hub *kerneltest.Hub) int {
	count := 0
	for _, code := range hub.Calls().Executed {
		if strings.HasPrefix(code, "import base64") && strings.Contains(code, kernel.PlatformTokenEnv) {
			count++
		}
	}
	return count
}
