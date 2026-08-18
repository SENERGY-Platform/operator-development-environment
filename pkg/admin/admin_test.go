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

package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

const testUser = "sub-alice"

func testService(t *testing.T) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	pricing := llm.NewPricing("EUR", llm.ModelPrice{
		Model: "test-model", InputPerMTok: 10, OutputPerMTok: 50,
	})
	service, err := New(store, pricing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service, store
}

func int64Ptr(v int64) *int64          { return &v }
func float64Ptr(v float64) *float64    { return &v }
func intPtr(v int) *int                { return &v }
func tierPtr(v tools.Tier) *tools.Tier { return &v }

// --- the exit criterion: a per-user cap is enforced ---

func TestTokenCapIsEnforced(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{
		Period: "24h", TokenCap: int64Ptr(1000),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	// Under the cap: permitted.
	if _, err := service.Check(ctx, testUser); err != nil {
		t.Fatalf("a fresh user was refused: %v", err)
	}

	service.RecordUsage(ctx, testUser, "sess-1", llm.Usage{
		InputTokens: 600, OutputTokens: 300, Provider: "test", Model: "test-model",
	})
	if _, err := service.Check(ctx, testUser); err != nil {
		t.Fatalf("at 900 of 1000 tokens the user was refused: %v", err)
	}

	// Over the cap: refused, with §3.3's structured error.
	service.RecordUsage(ctx, testUser, "sess-1", llm.Usage{
		InputTokens: 100, OutputTokens: 100, Provider: "test", Model: "test-model",
	})
	_, err := service.Check(ctx, testUser)
	if err == nil {
		t.Fatal("the token cap was exceeded and the request was still permitted")
	}

	var limitErr *LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error is %T, want *LimitError so the SPA can render it", err)
	}
	if limitErr.Scope != "user" || limitErr.Kind != "tokens" {
		t.Errorf("scope/kind = %q/%q, want user/tokens", limitErr.Scope, limitErr.Kind)
	}
	if limitErr.Cap != 1000 {
		t.Errorf("cap = %v, want 1000", limitErr.Cap)
	}
	if limitErr.Spent < 1000 {
		t.Errorf("spent = %v, want at least the cap", limitErr.Spent)
	}

	payload := limitErr.Payload()
	if payload["error"] != "limit_exceeded" {
		t.Errorf(`payload error = %v, want "limit_exceeded"`, payload["error"])
	}
	if payload["resets_at"] == nil {
		t.Error("the payload must say when the cap resets, or the developer cannot act on it")
	}
}

func TestCostCapIsEnforced(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{
		Period: "24h", CostCap: float64Ptr(0.01),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	// 1000 output tokens at 50/MTok = 0.05, over a 0.01 cap.
	usage := llm.Usage{OutputTokens: 1000, Provider: "test", Model: "test-model"}
	service.Pricing().Apply(&usage)
	if usage.CostEUR <= 0 {
		t.Fatalf("the pricing table produced no cost: %+v", usage)
	}
	service.RecordUsage(ctx, testUser, "sess-1", usage)

	_, err := service.Check(ctx, testUser)
	var limitErr *LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error is %T, want *LimitError", err)
	}
	if limitErr.Kind != "cost" {
		t.Errorf("kind = %q, want cost", limitErr.Kind)
	}
}

// TestCapsAreIndependentPerUser is the property that makes a per-user cap a
// per-user cap: one developer exhausting theirs must not block another.
func TestCapsAreIndependentPerUser(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, GlobalSubject, Limits{
		Period: "24h", TokenCap: int64Ptr(100),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	service.RecordUsage(ctx, "sub-alice", "s", llm.Usage{
		InputTokens: 500, Provider: "test", Model: "test-model",
	})

	if _, err := service.Check(ctx, "sub-alice"); err == nil {
		t.Error("alice exceeded her cap and was permitted")
	}
	if _, err := service.Check(ctx, "sub-bob"); err != nil {
		t.Errorf("bob was blocked by alice's spending: %v", err)
	}
}

// TestGlobalCapBlocksEveryone is the other half: a global cap is not per-user, and
// the refusal says so, because a developer blocked by it can do nothing about it.
func TestGlobalCapBlocksEveryone(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, GlobalSubject, Limits{
		Period: "24h", GlobalCostCap: float64Ptr(0.001),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	usage := llm.Usage{OutputTokens: 1000, Provider: "test", Model: "test-model"}
	service.Pricing().Apply(&usage)
	service.RecordUsage(ctx, "sub-alice", "s", usage)

	_, err := service.Check(ctx, "sub-bob")
	var limitErr *LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("bob was not blocked by the global cap: %v", err)
	}
	if limitErr.Scope != "global" {
		t.Errorf("scope = %q, want global — bob must be able to tell this is not his own cap",
			limitErr.Scope)
	}
}

func TestSoftWarningDoesNotBlock(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{
		Period: "24h", TokenCap: int64Ptr(1000), SoftWarnFraction: float64Ptr(0.5),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	service.RecordUsage(ctx, testUser, "s", llm.Usage{
		InputTokens: 600, Provider: "test", Model: "test-model",
	})

	verdict, err := service.Check(ctx, testUser)
	if err != nil {
		t.Fatalf("a soft warning blocked the request: %v", err)
	}
	if len(verdict.Warnings) != 1 {
		t.Fatalf("warnings = %d, want 1 at 60%% of a 50%% soft limit", len(verdict.Warnings))
	}
	if verdict.Warnings[0].Kind != "tokens" {
		t.Errorf("warning kind = %q, want tokens", verdict.Warnings[0].Kind)
	}
}

// TestPeriodWindowsSpend checks that a cap is over a period rather than for all
// time: usage older than the window must not count against it.
func TestPeriodWindowsSpend(t *testing.T) {
	service, store := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{
		Period: "1h", TokenCap: int64Ptr(100),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	// Written directly with an old timestamp: RecordUsage stamps now.
	if err := store.AppendUsage(ctx, Record{
		UserSub: testUser, InputTokens: 5000, Model: "test-model",
		At: time.Now().UTC().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}

	if _, err := service.Check(ctx, testUser); err != nil {
		t.Errorf("spend from outside the period counted against the cap: %v", err)
	}
}

// TestUnpricedModelIsReported is the honesty check: a cost cap cannot bind on a
// model with no price, and an admin must be told rather than shown zero spend.
func TestUnpricedModelIsReported(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{
		Period: "24h", CostCap: float64Ptr(100),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	usage := llm.Usage{OutputTokens: 1_000_000, Provider: "test", Model: "unpriced-model"}
	service.Pricing().Apply(&usage)
	if usage.CostEstimated {
		t.Fatal("an unpriced model produced an estimated cost")
	}
	service.RecordUsage(ctx, testUser, "s", usage)

	verdict, err := service.Check(ctx, testUser)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(verdict.Unpriced) != 1 || verdict.Unpriced[0] != "unpriced-model" {
		t.Errorf("unpriced = %v, want [unpriced-model] so the admin knows the cap under-counts",
			verdict.Unpriced)
	}
}

// --- the tier ceiling (§3.3) ---

func TestMaxTierCeiling(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, GlobalSubject, Limits{
		MaxTier: tierPtr(tools.L1),
	}, "admin-1"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	if err := service.CheckTier(ctx, testUser, tools.L0); err != nil {
		t.Errorf("L0 was refused under an L1 ceiling: %v", err)
	}
	if err := service.CheckTier(ctx, testUser, tools.L1); err != nil {
		t.Errorf("L1 was refused under an L1 ceiling: %v", err)
	}
	if err := service.CheckTier(ctx, testUser, tools.L2); err == nil {
		t.Error("L2 was permitted under an L1 ceiling")
	}
}

// TestPerUserTierCannotExceedGlobal is the one merge rule that is not "the user
// record wins": the global maximum tier is a deployment-wide Datensparsamkeit
// decision, and a per-user override must not be able to raise it.
func TestPerUserTierCannotExceedGlobal(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, GlobalSubject, Limits{MaxTier: tierPtr(tools.L1)}, "a"); err != nil {
		t.Fatalf("SetLimits global: %v", err)
	}
	if err := service.SetLimits(ctx, testUser, Limits{MaxTier: tierPtr(tools.L2)}, "a"); err != nil {
		t.Fatalf("SetLimits user: %v", err)
	}

	effective, err := service.Effective(ctx, testUser)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if got := effective.MaxTierOr(); got != tools.L1 {
		t.Errorf("effective max tier = %v, want L1: a per-user record must not raise the global ceiling", got)
	}
	if err := service.CheckTier(ctx, testUser, tools.L2); err == nil {
		t.Error("a per-user override raised the tier above the global maximum")
	}
}

// TestPerUserLimitsLayerOverGlobal checks the field-by-field merge: setting one
// per-user field must not clear the rest of the global policy.
func TestPerUserLimitsLayerOverGlobal(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, GlobalSubject, Limits{
		Period:                "24h",
		TokenCap:              int64Ptr(1000),
		CostCap:               float64Ptr(5),
		MaxConcurrentSessions: intPtr(3),
	}, "a"); err != nil {
		t.Fatalf("SetLimits global: %v", err)
	}
	// Alice gets a bigger token cap and nothing else stated.
	if err := service.SetLimits(ctx, testUser, Limits{TokenCap: int64Ptr(9000)}, "a"); err != nil {
		t.Fatalf("SetLimits user: %v", err)
	}

	effective, err := service.Effective(ctx, testUser)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if effective.TokenCap == nil || *effective.TokenCap != 9000 {
		t.Errorf("token cap = %v, want the per-user 9000", effective.TokenCap)
	}
	if effective.CostCap == nil || *effective.CostCap != 5 {
		t.Errorf("cost cap = %v, want the global 5 to survive", effective.CostCap)
	}
	if effective.MaxConcurrentSessions == nil || *effective.MaxConcurrentSessions != 3 {
		t.Errorf("session cap = %v, want the global 3 to survive", effective.MaxConcurrentSessions)
	}
	if effective.Period != "24h" {
		t.Errorf("period = %q, want the global 24h to survive", effective.Period)
	}
}

// --- allow-lists and session caps ---

func TestProviderAndModelAllowLists(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{
		AllowedProviders: []string{"anthropic"},
		AllowedModels:    []string{"claude-opus-5"},
	}, "a"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	if err := service.CheckProviderModel(ctx, testUser, "anthropic", "claude-opus-5"); err != nil {
		t.Errorf("a permitted provider and model were refused: %v", err)
	}
	if err := service.CheckProviderModel(ctx, testUser, "openai", "gpt-4o"); err == nil {
		t.Error("an unpermitted provider was allowed")
	}
	if err := service.CheckProviderModel(ctx, testUser, "anthropic", "claude-haiku-4-5"); err == nil {
		t.Error("an unpermitted model was allowed")
	}
}

func TestEmptyAllowListPermitsEverything(t *testing.T) {
	service, _ := testService(t)
	if err := service.CheckProviderModel(context.Background(), testUser, "anything", "any-model"); err != nil {
		t.Errorf("with no allow-list configured, everything should be permitted: %v", err)
	}
}

func TestSessionCap(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{MaxConcurrentSessions: intPtr(2)}, "a"); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
	if err := service.CheckSessionCount(ctx, testUser, 1); err != nil {
		t.Errorf("a second session was refused under a cap of 2: %v", err)
	}
	if err := service.CheckSessionCount(ctx, testUser, 2); err == nil {
		t.Error("a third session was permitted under a cap of 2")
	}
}

// --- defaults ---

func TestDefaultsArePermissive(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	// A deployment that has configured nothing must not refuse anything: a cap
	// nobody chose would block a developer for no stated reason.
	if _, err := service.Check(ctx, testUser); err != nil {
		t.Errorf("an unconfigured deployment refused a request: %v", err)
	}
	if err := service.CheckTier(ctx, testUser, tools.L2); err != nil {
		t.Errorf("an unconfigured deployment capped the exposure tier: %v", err)
	}
}

func TestSetLimitsRejectsMalformedPolicy(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	if err := service.SetLimits(ctx, testUser, Limits{Period: "not-a-duration"}, "a"); err == nil {
		t.Error("an unparseable period was accepted")
	}
	if err := service.SetLimits(ctx, testUser, Limits{
		SoftWarnFraction: float64Ptr(1.5),
	}, "a"); err == nil {
		t.Error("a soft warn fraction above 1 was accepted")
	}
	if err := service.SetLimits(ctx, testUser, Limits{
		MaxTier: tierPtr(tools.Tier(7)),
	}, "a"); err == nil {
		t.Error("an invalid tier was accepted")
	}
}

// --- the tool audit trail ---

func TestToolCallsAreAudited(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	service.RecordToolCall(ctx, tools.ToolCallRecord{
		UserSub: testUser, SessionID: "sess-1", Tool: "preview_series",
		Tier: tools.L0, Outcome: tools.OutcomeBlockedByTier, Duration: tools.Millis(time.Millisecond),
	})

	calls, err := service.ToolCalls(ctx, testUser, time.Hour, 10)
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("audited %d calls, want 1", len(calls))
	}
	if calls[0].Outcome != tools.OutcomeBlockedByTier {
		t.Errorf("outcome = %q, want the refusal to be recorded too", calls[0].Outcome)
	}
	if calls[0].Tier != tools.L0 {
		t.Errorf("tier = %v, want L0 recorded with the call", calls[0].Tier)
	}
}

// --- pricing ---

func TestPricingPrefersExactThenLongestPrefix(t *testing.T) {
	pricing := llm.NewPricing("EUR",
		llm.ModelPrice{Model: "claude", InputPerMTok: 1, OutputPerMTok: 1},
		llm.ModelPrice{Model: "claude-opus", InputPerMTok: 5, OutputPerMTok: 25},
		llm.ModelPrice{Model: "claude-opus-5", InputPerMTok: 7, OutputPerMTok: 35},
	)

	cases := map[string]float64{
		"claude-opus-5":    7,
		"claude-opus-4-8":  5,
		"claude-haiku-4-5": 1,
		"gpt-4o":           0,
	}
	for model, wantInput := range cases {
		price, found := pricing.Lookup(model)
		if wantInput == 0 {
			if found {
				t.Errorf("%s matched a price it should not have", model)
			}
			continue
		}
		if !found {
			t.Errorf("%s matched no price", model)
			continue
		}
		if price.InputPerMTok != wantInput {
			t.Errorf("%s input price = %v, want %v", model, price.InputPerMTok, wantInput)
		}
	}
}

func TestPricingCountsCachedInputSeparately(t *testing.T) {
	pricing := llm.NewPricing("EUR", llm.ModelPrice{
		Model: "m", InputPerMTok: 100, OutputPerMTok: 100, CachedInputPerMTok: 10,
	})
	usage := llm.Usage{
		Model: "m", InputTokens: 1_000_000, CachedInputTokens: 1_000_000, OutputTokens: 0,
	}
	pricing.Apply(&usage)

	// 100 for the fresh million, 10 for the cached one.
	if usage.CostEUR != 110 {
		t.Errorf("cost = %v, want 110: cached input is cheaper and must be priced as such", usage.CostEUR)
	}
}

func TestUnpricedCachedInputDoesNotUnderstate(t *testing.T) {
	// A zero cached price means "same as input", which overstates rather than
	// understates — the safe direction when the figure backs a spend cap.
	pricing := llm.NewPricing("EUR", llm.ModelPrice{
		Model: "m", InputPerMTok: 100, OutputPerMTok: 100,
	})
	usage := llm.Usage{Model: "m", CachedInputTokens: 1_000_000}
	pricing.Apply(&usage)
	if usage.CostEUR != 100 {
		t.Errorf("cost = %v, want 100", usage.CostEUR)
	}
}
