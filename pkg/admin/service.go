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
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/llm"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// Service enforces §3.3 and records what happened.
type Service struct {
	store   Store
	pricing *llm.Pricing
	now     func() time.Time
}

func New(store Store, pricing *llm.Pricing) (*Service, error) {
	if store == nil {
		return nil, errors.New("admin: a store is required")
	}
	return &Service{store: store, pricing: pricing, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Effective is the policy that applies to one user: the global record with their
// own layered over it.
func (s *Service) Effective(ctx context.Context, sub string) (Limits, error) {
	// Layer, not Merge: the global record is not a user record, and every field of
	// it must be honoured — GlobalCostCap included, which Merge refuses to take
	// from its second argument.
	global := Defaults()
	if record, found, err := s.store.Limits(ctx, GlobalSubject); err != nil {
		return Limits{}, err
	} else if found {
		global = Layer(global, record.Limits)
	}

	if sub == "" {
		return global, nil
	}
	record, found, err := s.store.Limits(ctx, sub)
	if err != nil {
		return Limits{}, err
	}
	if !found {
		return global, nil
	}
	return Merge(global, record.Limits), nil
}

// Check answers whether a user may make another provider request.
//
// §3.3 says "Enforce before dispatch", and this is that point: the chat engine
// calls it before every provider call, because that is where cost is incurred. It
// is deliberately *not* called per tool dispatch — a platform read costs no LLM
// tokens, and checking there would imply otherwise.
//
// The check is on spend *already recorded*, not on a prediction of this request's
// cost. A turn's cost is unknown until it completes, so a cap can be overshot by
// at most one request. Refusing on a guess would be the alternative, and it would
// mean refusing requests that would have fit.
func (s *Service) Check(ctx context.Context, sub string) (Verdict, error) {
	limits, err := s.Effective(ctx, sub)
	if err != nil {
		return Verdict{}, err
	}

	period := limits.PeriodDuration()
	since := s.now().Add(-period)
	resetsAt := s.now().Add(period)

	spend, err := s.store.SpendSince(ctx, sub, since)
	if err != nil {
		return Verdict{}, err
	}
	verdict := Verdict{Spend: spend}

	if limits.TokenCap != nil && *limits.TokenCap > 0 {
		cap := float64(*limits.TokenCap)
		if float64(spend.Tokens) >= cap {
			return verdict, &LimitError{
				Scope: "user", Kind: "tokens", Period: period.String(),
				Cap: cap, Spent: float64(spend.Tokens), ResetsAt: resetsAt,
			}
		}
		if float64(spend.Tokens) >= cap*limits.softWarnFraction() {
			verdict.Warnings = append(verdict.Warnings, Warning{
				Scope: "user", Kind: "tokens", Cap: cap,
				Spent: float64(spend.Tokens), Fraction: limits.softWarnFraction(),
			})
		}
	}

	if limits.CostCap != nil && *limits.CostCap > 0 {
		if spend.Cost >= *limits.CostCap {
			return verdict, &LimitError{
				Scope: "user", Kind: "cost", Period: period.String(),
				Cap: *limits.CostCap, Spent: spend.Cost, ResetsAt: resetsAt,
			}
		}
		if spend.Cost >= *limits.CostCap*limits.softWarnFraction() {
			verdict.Warnings = append(verdict.Warnings, Warning{
				Scope: "user", Kind: "cost", Cap: *limits.CostCap,
				Spent: spend.Cost, Fraction: limits.softWarnFraction(),
			})
		}
	}

	if limits.GlobalCostCap != nil && *limits.GlobalCostCap > 0 {
		globalSpend, err := s.store.SpendSince(ctx, GlobalSubject, since)
		if err != nil {
			return verdict, err
		}
		if globalSpend.Cost >= *limits.GlobalCostCap {
			return verdict, &LimitError{
				Scope: "global", Kind: "cost", Period: period.String(),
				Cap: *limits.GlobalCostCap, Spent: globalSpend.Cost, ResetsAt: resetsAt,
			}
		}
		if globalSpend.Cost >= *limits.GlobalCostCap*limits.softWarnFraction() {
			verdict.Warnings = append(verdict.Warnings, Warning{
				Scope: "global", Kind: "cost", Cap: *limits.GlobalCostCap,
				Spent: globalSpend.Cost, Fraction: limits.softWarnFraction(),
			})
		}
	}

	// A cost cap that cannot see part of the spend is worth saying out loud. It
	// happens when a model has no configured price: those requests accrue zero
	// cost, so the cap is enforced against an undercount.
	if limits.CostCap != nil || limits.GlobalCostCap != nil {
		unpriced, err := s.store.UnpricedModelsSince(ctx, sub, since)
		if err == nil && len(unpriced) > 0 {
			verdict.Unpriced = unpriced
		}
	}

	return verdict, nil
}

// AllowSpend is Check for a caller that has no use for the verdict: the provider
// call either may happen or it may not.
//
// It exists so a caller outside this package can enforce the spend caps through a
// narrow interface of its own — repo.Spend is the one — without taking a
// dependency on Verdict, and therefore on this package, for a value it would
// throw away. The warnings are dropped rather than swallowed: they are shown in
// the chat surface, which reads the verdict itself.
func (s *Service) AllowSpend(ctx context.Context, sub string) error {
	_, err := s.Check(ctx, sub)
	return err
}

// CheckProviderModel enforces the allow-lists of §3.3.
func (s *Service) CheckProviderModel(ctx context.Context, sub, provider, model string) error {
	limits, err := s.Effective(ctx, sub)
	if err != nil {
		return err
	}
	if len(limits.AllowedProviders) > 0 && !slices.Contains(limits.AllowedProviders, provider) {
		return fmt.Errorf("provider %q is not permitted for this user", provider)
	}
	if model != "" && len(limits.AllowedModels) > 0 && !slices.Contains(limits.AllowedModels, model) {
		return fmt.Errorf("model %q is not permitted for this user", model)
	}
	return nil
}

// CheckTier enforces the maximum exposure tier a user may raise a session to.
//
// This is an admin ceiling on a developer-controlled setting, and the two are
// different decisions: §3.2 gives the developer the tier control, §3.3 lets an
// admin bound it. Neither is the LLM's to change, which is why no tool exists for
// either (tools.Denied).
func (s *Service) CheckTier(ctx context.Context, sub string, tier tools.Tier) error {
	limits, err := s.Effective(ctx, sub)
	if err != nil {
		return err
	}
	if maximum := limits.MaxTierOr(); tier > maximum {
		return fmt.Errorf("exposure tier %s exceeds the maximum %s permitted for this user",
			tier, maximum)
	}
	return nil
}

// CheckSessionCount enforces the concurrent session cap.
func (s *Service) CheckSessionCount(ctx context.Context, sub string, current int) error {
	limits, err := s.Effective(ctx, sub)
	if err != nil {
		return err
	}
	if limits.MaxConcurrentSessions != nil && *limits.MaxConcurrentSessions > 0 &&
		current >= *limits.MaxConcurrentSessions {
		return fmt.Errorf("this user may have at most %d concurrent chat sessions",
			*limits.MaxConcurrentSessions)
	}
	return nil
}

// CheckWorkbenchCount enforces the per-user cap on open working contexts.
//
// Absent is not "unlimited" here: the repository surface applies its own
// deployment ceiling when this says nothing, because the thing being bounded is
// kernels in a pod and a pod has a memory limit whatever the admin settings say.
func (s *Service) CheckWorkbenchCount(ctx context.Context, sub string, current int) error {
	limits, err := s.Effective(ctx, sub)
	if err != nil {
		return err
	}
	if limits.MaxWorkbenches == nil || *limits.MaxWorkbenches <= 0 {
		return nil
	}
	if current >= *limits.MaxWorkbenches {
		// Plain text rather than a typed error from pkg/repo: that package declares
		// the interface this satisfies, and importing it back for one sentinel would
		// be a cycle. The repository surface wraps this in its own error, which is
		// what the API layer maps to a 409.
		return fmt.Errorf("this user may have at most %d workbenches open",
			*limits.MaxWorkbenches)
	}
	return nil
}

// RecordUsage accounts one provider request.
//
// A failure to record is logged and swallowed rather than failing the developer's
// turn. That is a deliberate trade in one direction: losing an accounting row
// under-reports spend, whereas failing the turn would make a database blip look
// like a broken assistant. The alternative — refusing to answer unless the
// accounting write succeeds — is the stricter choice and would be right if the cap
// were a billing boundary rather than a governance one.
func (s *Service) RecordUsage(ctx context.Context, sub, sessionID string, usage llm.Usage) {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CachedInputTokens == 0 {
		return
	}
	record := Record{
		UserSub:           sub,
		SessionID:         sessionID,
		Provider:          usage.Provider,
		Model:             usage.Model,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		Cost:              usage.CostEUR,
		CostEstimated:     usage.CostEstimated,
		At:                s.now(),
	}
	if err := s.store.AppendUsage(ctx, record); err != nil {
		slog.ErrorContext(ctx, "could not record llm usage; a spend cap will under-count",
			"user", sub, "provider", usage.Provider, "model", usage.Model, "error", err)
	}
}

// RecordToolCall implements tools.AuditSink, so every dispatch — including every
// refusal — lands in the audit trail without the dispatcher knowing about this
// package.
func (s *Service) RecordToolCall(ctx context.Context, entry tools.ToolCallRecord) {
	if entry.At.IsZero() {
		entry.At = s.now()
	}
	// Detached from the request context on purpose: a cancelled turn — the
	// developer pressing stop — must still leave its audit record, and a context
	// that is already done would drop the write.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.AppendToolCall(writeCtx, entry); err != nil {
		slog.ErrorContext(ctx, "could not record tool call in the audit trail",
			"tool", entry.Tool, "error", err)
	}
}

// SetLimits writes a policy. by is the admin's sub, recorded for provenance.
func (s *Service) SetLimits(ctx context.Context, subject string, limits Limits, by string) error {
	if limits.MaxTier != nil && !limits.MaxTier.Valid() {
		return fmt.Errorf("admin: invalid maximum exposure tier")
	}
	if limits.Period != "" {
		if _, err := time.ParseDuration(limits.Period); err != nil {
			return fmt.Errorf("admin: period must be a duration such as 720h: %w", err)
		}
	}
	if limits.SoftWarnFraction != nil && (*limits.SoftWarnFraction <= 0 || *limits.SoftWarnFraction >= 1) {
		return fmt.Errorf("admin: soft_warn_fraction must be between 0 and 1 exclusive")
	}
	return s.store.PutLimits(ctx, LimitsRecord{
		Subject: subject, Limits: limits, UpdatedAt: s.now(), UpdatedBy: by,
	})
}

// AllLimits is the settings surface's list.
func (s *Service) AllLimits(ctx context.Context) ([]LimitsRecord, error) {
	return s.store.AllLimits(ctx)
}

// Usage and ToolCalls serve the admin reporting surface.
func (s *Service) Usage(ctx context.Context, subject string, period time.Duration, limit int) ([]Record, error) {
	if period <= 0 {
		period = DefaultPeriod
	}
	return s.store.UsageSince(ctx, subject, s.now().Add(-period), limit)
}

func (s *Service) ToolCalls(ctx context.Context, subject string, period time.Duration, limit int) ([]tools.ToolCallRecord, error) {
	if period <= 0 {
		period = DefaultPeriod
	}
	return s.store.ToolCallsSince(ctx, subject, s.now().Add(-period), limit)
}

func (s *Service) Spend(ctx context.Context, subject string, period time.Duration) (Spend, error) {
	if period <= 0 {
		period = DefaultPeriod
	}
	return s.store.SpendSince(ctx, subject, s.now().Add(-period))
}

// Pricing exposes the configured table for the admin surface, which needs to show
// which models a cost cap can actually bind on.
func (s *Service) Pricing() *llm.Pricing { return s.pricing }

// isNoRows keeps the pgx dependency out of store.go's read paths.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
