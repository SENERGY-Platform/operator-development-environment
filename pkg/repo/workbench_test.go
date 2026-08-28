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

package repo_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// What these cover is the reason workbenches exist: two operators in flight at
// once, without either one's working copy moving under the other.

// selectInto puts a repository into a named workbench. An empty id is a request
// that names none, which is what every caller sent before workbenches existed.
func (h *harness) selectInto(workbench, fullName string) repo.Status {
	h.t.Helper()
	req := h.request()
	req.WorkbenchID = workbench
	status, err := h.service.Select(context.Background(), repo.SelectRequest{
		Request: req, FullName: fullName,
	})
	if err != nil {
		h.t.Fatalf("Select(%s into %q): %v", fullName, workbench, err)
	}
	return status
}

func (h *harness) createWorkbench(title string) repo.Workbench {
	h.t.Helper()
	bench, err := h.service.CreateWorkbench(context.Background(), testUserSub, title)
	if err != nil {
		h.t.Fatalf("CreateWorkbench(%q): %v", title, err)
	}
	return bench
}

func TestTwoWorkbenchesHoldTwoCheckoutsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.connect()

	first := h.selectInto("", "devuser/pv-forecast")
	second := h.createWorkbench("anomalies")
	other := h.selectInto(second.ID, "devuser/anomaly-detect")

	if first.Link.Path == other.Link.Path {
		t.Fatalf("both workbenches checked out into %q", first.Link.Path)
	}
	for _, path := range []string{first.Link.Path, other.Link.Path} {
		if _, err := os.Stat(h.path(path, ".git")); err != nil {
			t.Errorf("no working copy at %s: %v", path, err)
		}
	}

	// And neither has moved the other: the first workbench still answers with the
	// repository it was given, which is the failure this whole change is about.
	req := h.request()
	req.WorkbenchID = first.Link.WorkbenchID
	status, err := h.service.Status(context.Background(), repo.StatusRequest{Request: req})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Link.FullName != "devuser/pv-forecast" {
		t.Errorf("the first workbench now holds %q", status.Link.FullName)
	}
}

func TestARepositoryAlreadyOpenIsRefusedInASecondWorkbench(t *testing.T) {
	h := newHarness(t)
	h.connect()

	h.selectInto("", "devuser/pv-forecast")
	second := h.createWorkbench("")

	req := h.request()
	req.WorkbenchID = second.ID
	_, err := h.service.Select(context.Background(), repo.SelectRequest{
		Request: req, FullName: "devuser/pv-forecast",
	})
	if !errors.Is(err, repo.ErrRepositoryInUse) {
		t.Fatalf("selecting the same repository twice gave %v, want ErrRepositoryInUse", err)
	}
	// Two kernels and two streams of git commands in one working tree is what the
	// refusal prevents, so the message has to say where it already is.
	if err != nil && !strings.Contains(err.Error(), "pv-forecast") {
		t.Errorf("the refusal does not name the repository: %v", err)
	}
}

func TestReselectingTheSameRepositoryReturnsToItsWorkbench(t *testing.T) {
	h := newHarness(t)
	h.connect()

	first := h.selectInto("", "devuser/pv-forecast")
	second := h.createWorkbench("")
	h.selectInto(second.ID, "devuser/anomaly-detect")

	// No workbench named, and the repository is open in one: that is a developer
	// returning to it, not an ambiguous request.
	again := h.selectInto("", "devuser/pv-forecast")
	if again.Link.WorkbenchID != first.Link.WorkbenchID {
		t.Errorf("re-selecting landed in %q, want %q",
			again.Link.WorkbenchID, first.Link.WorkbenchID)
	}

	benches, err := h.service.Workbenches(context.Background(), testUserSub)
	if err != nil {
		t.Fatalf("Workbenches: %v", err)
	}
	if len(benches) != 2 {
		t.Errorf("re-selecting made %d workbenches, want the 2 that existed", len(benches))
	}
}

func TestARequestThatNamesNoWorkbenchIsRefusedOnceThereAreTwo(t *testing.T) {
	h := newHarness(t)
	h.connect()

	h.selectInto("", "devuser/pv-forecast")
	second := h.createWorkbench("")
	h.selectInto(second.ID, "devuser/anomaly-detect")

	_, err := h.service.Status(context.Background(), repo.StatusRequest{Request: h.request()})
	if !errors.Is(err, repo.ErrInvalidRequest) {
		t.Fatalf("an ambiguous status read gave %v, want ErrInvalidRequest", err)
	}
}

func TestOneWorkbenchAnswersARequestThatNamesNone(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.selectInto("", "devuser/pv-forecast")

	// The compatibility case: every client written before workbenches existed sends
	// exactly this, and a developer with one workbench must not have to change.
	status, err := h.service.Status(context.Background(), repo.StatusRequest{Request: h.request()})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Link.FullName != "devuser/pv-forecast" {
		t.Errorf("status reported %q", status.Link.FullName)
	}
}

func TestAnotherDevelopersWorkbenchIsNotReadable(t *testing.T) {
	h := newHarness(t)
	h.connect()
	bench := h.createWorkbench("theirs")

	if _, err := h.service.Workbench(context.Background(), "someone-else", bench.ID); !errors.Is(
		err, repo.ErrNoWorkbench) {
		t.Fatalf("reading another developer's workbench gave %v, want ErrNoWorkbench", err)
	}
}

// stageLegacyLink writes the row a release before workbenches wrote, with the
// checkout it names already on the PVC. Adopting it is what stops the developer
// re-selecting a repository they are in the middle of working on.
func (h *harness) stageLegacyLink(fullName string) {
	h.t.Helper()
	owner, name, _ := strings.Cut(fullName, "/")
	if err := h.store.PutLegacyLink(context.Background(), repo.Link{
		UserSub:       testUserSub,
		FullName:      fullName,
		Name:          name,
		Owner:         owner,
		DefaultBranch: "main",
		CloneURL:      "file://" + h.remote,
		Path:          fullName,
		SelectedAt:    time.Now().UTC(),
	}); err != nil {
		h.t.Fatalf("staging the legacy link: %v", err)
	}
}

func TestAPreWorkbenchLinkBecomesTheFirstWorkbench(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.stageLegacyLink("devuser/pv-forecast")

	benches, err := h.service.Workbenches(context.Background(), testUserSub)
	if err != nil {
		t.Fatalf("Workbenches: %v", err)
	}
	if len(benches) != 1 {
		t.Fatalf("adoption produced %d workbenches, want 1", len(benches))
	}
	if benches[0].Link.FullName != "devuser/pv-forecast" {
		t.Errorf("the adopted workbench holds %q", benches[0].Link.FullName)
	}
	if benches[0].Link.Path != "devuser/pv-forecast" {
		t.Errorf("the adopted workbench points at %q, not the existing checkout",
			benches[0].Link.Path)
	}

	// Once, and not again: a second read must not mint a second workbench from the
	// same row.
	if again, err := h.service.Workbenches(context.Background(), testUserSub); err != nil {
		t.Fatalf("Workbenches: %v", err)
	} else if len(again) != 1 || again[0].ID != benches[0].ID {
		t.Errorf("a second read adopted again: %d workbenches", len(again))
	}
}

func TestConcurrentFirstRequestsAdoptTheLinkOnce(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.stageLegacyLink("devuser/pv-forecast")

	// What one page load actually sends: several requests that name no workbench,
	// in flight together, each of them a reason to adopt. They all find the list
	// empty and they all write; the unique index lets one through. Before the
	// adoption tolerated losing that race, the rest came back as "that repository
	// is open in another workbench" — naming the workbench the developer's own page
	// had made a millisecond earlier.
	const requests = 8
	answers := make([][]repo.Workbench, requests)
	failures := make([]error, requests)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			answers[i], failures[i] = h.service.Workbenches(context.Background(), testUserSub)
		}()
	}
	close(start)
	wait.Wait()

	for i := range requests {
		if failures[i] != nil {
			t.Fatalf("request %d: %v", i, failures[i])
		}
		if len(answers[i]) != 1 {
			t.Fatalf("request %d saw %d workbenches, want 1", i, len(answers[i]))
		}
		if answers[i][0].ID != answers[0][0].ID {
			t.Errorf("request %d adopted %q, request 0 adopted %q",
				i, answers[i][0].ID, answers[0][0].ID)
		}
	}
}

func TestClosingTheAdoptedWorkbenchDoesNotBringItBack(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.stageLegacyLink("devuser/pv-forecast")

	benches, err := h.service.Workbenches(context.Background(), testUserSub)
	if err != nil {
		t.Fatalf("Workbenches: %v", err)
	}
	if len(benches) != 1 {
		t.Fatalf("adoption produced %d workbenches, want 1", len(benches))
	}
	if err := h.service.DeleteWorkbench(
		context.Background(), testUserSub, benches[0].ID); err != nil {
		t.Fatalf("DeleteWorkbench: %v", err)
	}

	// Closing the last workbench leaves none, and the spent row stays spent. The
	// developer who closed it deliberately must not be handed it straight back.
	again, err := h.service.Workbenches(context.Background(), testUserSub)
	if err != nil {
		t.Fatalf("Workbenches: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("closing the last workbench left %d, want none: the legacy link was "+
			"adopted a second time", len(again))
	}
}

func TestTheWorkbenchCeilingRefusesOneTooMany(t *testing.T) {
	h := newHarness(t)
	h.connect()

	for i := 0; i < repo.DefaultMaxWorkbenches; i++ {
		h.createWorkbench("")
	}
	_, err := h.service.CreateWorkbench(context.Background(), testUserSub, "")
	if !errors.Is(err, repo.ErrTooManyWorkbenches) {
		t.Fatalf("creating one past the ceiling gave %v, want ErrTooManyWorkbenches", err)
	}
}

func TestDeletingAWorkbenchLeavesTheWorkingCopy(t *testing.T) {
	h := newHarness(t)
	h.connect()
	status := h.selectInto("", "devuser/pv-forecast")

	if err := h.service.DeleteWorkbench(
		context.Background(), testUserSub, status.Link.WorkbenchID); err != nil {
		t.Fatalf("DeleteWorkbench: %v", err)
	}
	if _, err := os.Stat(h.path(status.Link.Path, ".git")); err != nil {
		t.Errorf("the working copy went with the workbench: %v", err)
	}
	// And selecting it again adopts that directory rather than cloning beside it,
	// which is §5.11 item 5 and the reason deleting is cheap.
	again := h.selectInto("", "devuser/pv-forecast")
	if again.Link.Path != status.Link.Path {
		t.Errorf("re-selecting checked out into %q, not %q", again.Link.Path, status.Link.Path)
	}
}
