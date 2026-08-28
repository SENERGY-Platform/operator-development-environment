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

package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// sessionIn opens a session already acting in a workbench, which is what the SPA
// sends once a developer has one open.
func (h *harness) sessionIn(t *testing.T, workbench string) Session {
	t.Helper()
	session, err := h.engine.CreateSession(context.Background(), testUser, CreateRequest{
		Tier: tools.L0, WorkbenchID: workbench,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return session
}

// notices returns the messages ODE put in the conversation itself.
func (h *harness) notices(t *testing.T, sessionID string) []StoredMessage {
	t.Helper()
	stored, err := h.store.Messages(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	out := []StoredMessage{}
	for _, message := range stored {
		if message.Injected() {
			out = append(out, message)
		}
	}
	return out
}

func textOf(message StoredMessage) string {
	parts := []string{}
	for _, content := range message.Content {
		if content.Type == llm.ContentText {
			parts = append(parts, content.Text)
		}
	}
	return strings.Join(parts, "")
}

func TestMovingASessionRePointsItAndSaysSoInTheConversation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.sessionIn(t, "bench-a")

	moved, err := h.engine.MoveSession(ctx, testUser, session.ID, "bench-b", "alice/wind-forecast")
	if err != nil {
		t.Fatalf("MoveSession: %v", err)
	}
	if moved.WorkbenchID != "bench-b" {
		t.Errorf("workbench = %q, want bench-b", moved.WorkbenchID)
	}
	if !moved.UpdatedAt.After(session.UpdatedAt) && !moved.UpdatedAt.Equal(session.UpdatedAt) {
		t.Errorf("updated_at went backwards: %v then %v", session.UpdatedAt, moved.UpdatedAt)
	}

	// Read back rather than trusted from the answer: the point of the write is that
	// the next reader of this session finds the new workbench.
	stored, found, err := h.store.Session(ctx, session.ID)
	if err != nil || !found {
		t.Fatalf("Session: %v (found=%v)", err, found)
	}
	if stored.WorkbenchID != "bench-b" {
		t.Errorf("stored workbench = %q, want bench-b", stored.WorkbenchID)
	}

	notices := h.notices(t, session.ID)
	if len(notices) != 1 {
		t.Fatalf("injected messages = %d, want the one note the move leaves", len(notices))
	}
	note := notices[0]
	if note.Role != llm.RoleUser {
		t.Errorf("note role = %q, want user: it is input for the next turn", note.Role)
	}
	if note.Subject != moveSubjectPrefix+"bench-b" {
		t.Errorf("note subject = %q, want the prefixed workbench id", note.Subject)
	}
	text := textOf(note)
	if !strings.Contains(text, "alice/wind-forecast") {
		t.Errorf("the note does not name the workbench moved to: %q", text)
	}
	// The reason the note exists at all: the history above it describes another
	// checkout, and the model has to be told not to trust it.
	if !strings.Contains(text, "re-read") {
		t.Errorf("the note does not tell the model to re-read what it relies on: %q", text)
	}
}

func TestMovingASessionToWhereItAlreadyIsChangesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.sessionIn(t, "bench-a")

	same, err := h.engine.MoveSession(ctx, testUser, session.ID, "  bench-a  ", "alice/wind-forecast")
	if err != nil {
		t.Fatalf("MoveSession: %v", err)
	}
	if same.WorkbenchID != "bench-a" {
		t.Errorf("workbench = %q, want bench-a", same.WorkbenchID)
	}
	// A note about a move that did not happen would be a false entry in a history
	// that is read back on every turn.
	if notices := h.notices(t, session.ID); len(notices) != 0 {
		t.Errorf("a no-op move left %d notes in the conversation", len(notices))
	}
}

func TestClearingASessionsWorkbenchIsAllowed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.sessionIn(t, "bench-a")

	cleared, err := h.engine.MoveSession(ctx, testUser, session.ID, "", "")
	if err != nil {
		t.Fatalf("MoveSession: %v", err)
	}
	if cleared.WorkbenchID != "" {
		t.Errorf("workbench = %q, want it cleared", cleared.WorkbenchID)
	}
	notices := h.notices(t, session.ID)
	if len(notices) != 1 {
		t.Fatalf("injected messages = %d, want one", len(notices))
	}
	// Worth its own wording: "moved to " with nothing after it would read as a bug.
	if text := textOf(notices[0]); !strings.Contains(text, "cleared") {
		t.Errorf("clearing the workbench was described as a move: %q", text)
	}
}

func TestMovingAnotherUsersSessionIsNotFound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.sessionIn(t, "bench-a")

	if _, err := h.engine.MoveSession(ctx, "sub-bob", session.ID, "bench-b", ""); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("error = %v, want ErrNoSuchSession: a session id must not confirm itself to a stranger", err)
	}
	// And the session is untouched, which is the half an ownership check is for.
	stored, _, err := h.store.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if stored.WorkbenchID != "bench-a" {
		t.Errorf("another user's move landed: workbench = %q", stored.WorkbenchID)
	}
}

func TestMovingASessionMidTurnIsRefused(t *testing.T) {
	h := newHarness(t, toolTurn("call-1", "l0_tool"), textTurn("done"))
	session := h.sessionIn(t, "bench-a")

	// Attempted from inside the loop, which is the case that matters: the running
	// turn read the session once and is acting in bench-a for the rest of it.
	var refusal error
	var reads int
	h.store.setHook(func() {
		reads++
		if reads == 1 {
			_, refusal = h.engine.MoveSession(
				context.Background(), testUser, session.ID, "bench-b", "alice/wind-forecast")
		}
	})

	exchange, err := h.engine.Send(context.Background(), StaticToken(testToken), testUser, session.ID, "go")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, exchange)

	if !errors.Is(refusal, ErrInvalidRequest) {
		t.Fatalf("mid-turn move error = %v, want ErrInvalidRequest", refusal)
	}
	stored, _, err := h.store.Session(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if stored.WorkbenchID != "bench-a" {
		t.Errorf("the refused move landed anyway: workbench = %q", stored.WorkbenchID)
	}
}

// TestMovingASessionKeepsATierTheCeilingIsHidingUnderneath is the rename test's
// concern on this route: the answer reports the clamped tier, and writing the
// answer back would persist the clamp.
func TestMovingASessionKeepsATierTheCeilingIsHidingUnderneath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.sessionIn(t, "bench-a")
	if _, err := h.engine.SetTier(ctx, testUser, session.ID, tools.L2); err != nil {
		t.Fatalf("SetTier: %v", err)
	}
	if err := h.admin.SetLimits(ctx, admin.GlobalSubject, admin.Limits{
		MaxTier: tierPtr(tools.L0),
	}, "admin"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	moved, err := h.engine.MoveSession(ctx, testUser, session.ID, "bench-b", "alice/wind-forecast")
	if err != nil {
		t.Fatalf("MoveSession: %v", err)
	}
	if moved.Tier != tools.L0 {
		t.Errorf("answered tier = %v, want the clamped L0", moved.Tier)
	}
	stored, _, err := h.store.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if stored.Tier != tools.L2 {
		t.Errorf("stored tier = %v, want L2: a move must not persist the admin clamp", stored.Tier)
	}
}

// TestAMoveNoteDoesNotLeaveTwoUserTurnsInARow guards the shape, not the wording.
// The note is appended without a turn, so the developer's next message would follow
// it as a second user message — which Anthropic and OpenAI both reject.
func TestAMoveNoteDoesNotLeaveTwoUserTurnsInARow(t *testing.T) {
	h := newHarness(t, textTurn("done"))
	ctx := context.Background()
	session := h.sessionIn(t, "bench-a")

	if _, err := h.engine.MoveSession(ctx, testUser, session.ID, "bench-b", "alice/wind-forecast"); err != nil {
		t.Fatalf("MoveSession: %v", err)
	}
	exchange, err := h.engine.Send(ctx, StaticToken(testToken), testUser, session.ID, "carry on here")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	drain(t, exchange)

	sent := h.provider.lastRequest(t).Messages
	for i := 1; i < len(sent); i++ {
		if sent[i].Role == sent[i-1].Role {
			t.Fatalf("messages %d and %d are both %q: %+v", i-1, i, sent[i].Role, sent)
		}
	}
	// Merged rather than dropped: the model still has to read why the checkout under
	// it changed.
	joined := ""
	for _, message := range sent {
		for _, content := range message.Content {
			joined += content.Text
		}
	}
	if !strings.Contains(joined, "alice/wind-forecast") {
		t.Errorf("the move note did not reach the provider: %q", joined)
	}
	if !strings.Contains(joined, "carry on here") {
		t.Errorf("the developer's own message was lost in the merge: %q", joined)
	}
}

func TestCoalesceUserTurnsKeepsToolResultsFirst(t *testing.T) {
	merged := coalesceUserTurns([]llm.Message{
		llm.UserText("go"),
		{Role: llm.RoleAssistant, Content: []llm.Content{
			{Type: llm.ContentToolUse, ToolUseID: "call-1", ToolName: "l0_tool"},
		}},
		{Role: llm.RoleUser, Content: []llm.Content{
			{Type: llm.ContentToolResult, ToolUseID: "call-1", ToolResult: `{"ran":true}`},
		}},
		llm.UserText("and now this"),
	})
	if len(merged) != 3 {
		t.Fatalf("messages = %d, want the two user turns merged into one", len(merged))
	}
	last := merged[2]
	if last.Content[0].Type != llm.ContentToolResult {
		t.Errorf("the tool result is no longer first in its turn: %+v", last.Content)
	}
	if last.Content[len(last.Content)-1].Text != "and now this" {
		t.Errorf("chronological order was not kept: %+v", last.Content)
	}
}

func TestCoalesceUserTurnsLeavesAnAlternatingHistoryAlone(t *testing.T) {
	healthy := []llm.Message{llm.UserText("go"), llm.AssistantText("done"), llm.UserText("more")}
	merged := coalesceUserTurns(healthy)
	if len(merged) != len(healthy) {
		t.Fatalf("messages = %d, want %d unchanged", len(merged), len(healthy))
	}
	for i := range healthy {
		if len(merged[i].Content) != len(healthy[i].Content) {
			t.Errorf("message %d was rewritten: %+v", i, merged[i])
		}
	}
}
