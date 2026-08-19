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

// Package admin is §3.3's control surface: per-user and global LLM limits, the
// accounting they are enforced against, and the audit trail of what the LLM did.
//
// The realm role `admin` gates every write here, which the API layer enforces.
// This package's job is to make the limits meaningful:
//
//   - accounting is per Keycloak `sub`, one row per provider request, carrying
//     provider, model, token counts, estimated cost, session and timestamp;
//   - a cap is checked *before* the request that would breach it, and answered
//     with a structured refusal rather than a generic error;
//   - a user's limits override the global ones field by field, so an admin can
//     raise one developer's ceiling without restating the whole policy.
//
// One honesty constraint runs through it: a cost cap can only bind on a model
// ODE has a price for (llm.Pricing). An unpriced model accrues zero cost, so the
// cap silently would not apply — Check reports that rather than letting an admin
// believe a limit is in force.
package admin

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// Limits is the configurable policy of §3.3.
//
// Every numeric field is a pointer so that "not set at this level" and "set to
// zero" stay distinguishable. Without that, a per-user record could not say
// "inherit the global token cap" — a zero would read as "no tokens at all", and
// merging would silently forbid everything.
type Limits struct {
	// Period is the window a cap applies over, as a Go duration ("720h"). Empty
	// means the default.
	Period string `json:"period,omitempty"`

	// TokenCap and CostCap are hard caps: a request that would exceed one is
	// refused. CostCap is in the pricing currency.
	TokenCap *int64   `json:"token_cap,omitempty"`
	CostCap  *float64 `json:"cost_cap,omitempty"`

	// SoftWarnFraction is where a warning is raised, as a fraction of the hard cap
	// (0.8 = warn at four fifths). A warning changes nothing about whether the
	// request runs; it is surfaced so a developer is not cut off without notice.
	SoftWarnFraction *float64 `json:"soft_warn_fraction,omitempty"`

	// GlobalCostCap is the spend cap across every user. Meaningful only on the
	// global record; ignored on a per-user one, because a single user cannot own
	// the whole deployment's ceiling.
	GlobalCostCap *float64 `json:"global_cost_cap,omitempty"`

	// AllowedProviders and AllowedModels narrow what may be used. Empty means "no
	// restriction beyond what the providers themselves declare", which is the
	// permissive default a fresh deployment wants.
	AllowedProviders []string `json:"allowed_providers,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`

	// MaxTier is the highest exposure tier this subject may raise a session to
	// (§3.3: "Maximum exposure tier permitted per user or globally"). Absent means
	// L2, the highest that exists.
	MaxTier *tools.Tier `json:"max_tier,omitempty"`

	// MaxConcurrentSessions caps live chat sessions per user.
	MaxConcurrentSessions *int `json:"max_concurrent_sessions,omitempty"`

	// Kernel and Ray caps are §3.3 fields ODE stores and does not enforce, and the
	// two have different reasons now that M4 exists.
	//
	// The Ray cap waits for M7, as before. The kernel caps turned out not to be
	// ODE's to apply at all: a pod's resources are set by KubeSpawner at spawn
	// time, and the Hub API's spawn body selects a *profile* rather than carrying
	// arbitrary overrides. A per-user memory limit therefore lives in the profile
	// (values-ode-singleuser.yaml in rancher-2-defs) or in a Hub-side spawn hook —
	// somewhere ODE cannot reach from here.
	//
	// They stay on the type because §3.3 lists them and an admin will look for
	// them, and DeclaredFields is what stops an administrator setting one and
	// assuming it binds.
	KernelCPUDefault *string `json:"kernel_cpu_default,omitempty"`
	KernelCPUMax     *string `json:"kernel_cpu_max,omitempty"`
	KernelMemDefault *string `json:"kernel_mem_default,omitempty"`
	KernelMemMax     *string `json:"kernel_mem_max,omitempty"`
	MaxConcurrentRay *int    `json:"max_concurrent_ray_jobs,omitempty"`
}

// EnforcedFields and DeclaredFields say which parts of Limits this build acts on.
// Served to the settings UI so an admin can see the difference rather than
// setting a kernel cap and assuming it binds.
func EnforcedFields() []string {
	return []string{
		"period", "token_cap", "cost_cap", "soft_warn_fraction", "global_cost_cap",
		"allowed_providers", "allowed_models", "max_tier", "max_concurrent_sessions",
	}
}

func DeclaredFields() map[string]string {
	const kernelResources = "set on the KubeSpawner profile, not by ODE — " +
		"see deploy/jupyterhub/README.md"
	return map[string]string{
		"kernel_cpu_default":      kernelResources,
		"kernel_cpu_max":          kernelResources,
		"kernel_mem_default":      kernelResources,
		"kernel_mem_max":          kernelResources,
		"max_concurrent_ray_jobs": "M7 (experiments/, SPEC §5.12)",
	}
}

const (
	// DefaultPeriod is a month, which is the billing period an LLM key is usually
	// accounted against.
	DefaultPeriod = 720 * time.Hour
	// DefaultSoftWarnFraction warns at four fifths of a cap.
	DefaultSoftWarnFraction = 0.8
)

// Defaults is the policy a deployment starts with: no cap, no restriction, and
// the full tier range available.
//
// Permissive on purpose. A default cap would be a number nobody chose, and a
// developer hitting it would be told they had exceeded a limit that came from
// nowhere. The admin surface exists so the decision is made rather than inherited.
func Defaults() Limits {
	maxTier := tools.MaxTier
	warn := DefaultSoftWarnFraction
	return Limits{
		Period:           DefaultPeriod.String(),
		MaxTier:          &maxTier,
		SoftWarnFraction: &warn,
	}
}

// PeriodDuration parses Period, falling back to the default rather than failing:
// a malformed value stored earlier must not make every request error.
func (l Limits) PeriodDuration() time.Duration {
	if l.Period == "" {
		return DefaultPeriod
	}
	parsed, err := time.ParseDuration(l.Period)
	if err != nil || parsed <= 0 {
		return DefaultPeriod
	}
	return parsed
}

// Merge layers a per-user record over the global one.
//
// Two rules distinguish this from a plain layering, and both exist because a
// per-user record must not be able to weaken a deployment-wide decision:
//
//   - GlobalCostCap is only ever the global record's. It is the ceiling on the
//     whole deployment's spend, and a user cannot own it.
//   - MaxTier takes the *lower* of the two. The global maximum tier is a
//     Datensparsamkeit decision about the whole deployment (§3.2), and a per-user
//     override that could exceed it would make the global setting a suggestion.
//
// Everything else is layered field by field by Layer, so that an admin who sets
// one developer's cost cap does not thereby clear every other global setting for
// them — which is what replacing the record wholesale would do, silently.
func Merge(global, user Limits) Limits {
	merged := Layer(global, user)
	merged.GlobalCostCap = global.GlobalCostCap

	if user.MaxTier != nil {
		userTier := *user.MaxTier
		if global.MaxTier == nil || userTier < *global.MaxTier {
			merged.MaxTier = &userTier
		} else {
			merged.MaxTier = global.MaxTier
		}
	}
	return merged
}

// Layer layers `over` onto `base`, field by field, with no policy rules applied.
//
// Used for defaults → global, where every field of the global record must be
// honoured — including GlobalCostCap, which Merge deliberately refuses to take
// from its second argument. Overloading one function for both jobs is what made
// a configured global cap silently vanish.
func Layer(base, over Limits) Limits {
	merged := base
	user := over
	if user.Period != "" {
		merged.Period = user.Period
	}
	if user.TokenCap != nil {
		merged.TokenCap = user.TokenCap
	}
	if user.CostCap != nil {
		merged.CostCap = user.CostCap
	}
	if user.SoftWarnFraction != nil {
		merged.SoftWarnFraction = user.SoftWarnFraction
	}
	if len(user.AllowedProviders) > 0 {
		merged.AllowedProviders = user.AllowedProviders
	}
	if len(user.AllowedModels) > 0 {
		merged.AllowedModels = user.AllowedModels
	}
	if user.MaxConcurrentSessions != nil {
		merged.MaxConcurrentSessions = user.MaxConcurrentSessions
	}
	if user.KernelCPUDefault != nil {
		merged.KernelCPUDefault = user.KernelCPUDefault
	}
	if user.KernelCPUMax != nil {
		merged.KernelCPUMax = user.KernelCPUMax
	}
	if user.KernelMemDefault != nil {
		merged.KernelMemDefault = user.KernelMemDefault
	}
	if user.KernelMemMax != nil {
		merged.KernelMemMax = user.KernelMemMax
	}
	if user.MaxConcurrentRay != nil {
		merged.MaxConcurrentRay = user.MaxConcurrentRay
	}
	if user.MaxTier != nil {
		merged.MaxTier = user.MaxTier
	}
	if user.GlobalCostCap != nil {
		merged.GlobalCostCap = user.GlobalCostCap
	}

	return merged
}

// MaxTierOr returns the permitted ceiling, defaulting to the highest tier.
func (l Limits) MaxTierOr() tools.Tier {
	if l.MaxTier == nil || !l.MaxTier.Valid() {
		return tools.MaxTier
	}
	return *l.MaxTier
}

func (l Limits) softWarnFraction() float64 {
	if l.SoftWarnFraction == nil || *l.SoftWarnFraction <= 0 || *l.SoftWarnFraction >= 1 {
		return DefaultSoftWarnFraction
	}
	return *l.SoftWarnFraction
}

// Record is one accounted provider request (§3.3: "recorded per request:
// provider, model, input/output tokens, estimated cost, session, timestamp").
type Record struct {
	UserSub           string    `json:"user_sub"`
	SessionID         string    `json:"session_id,omitempty"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	InputTokens       int       `json:"input_tokens"`
	OutputTokens      int       `json:"output_tokens"`
	CachedInputTokens int       `json:"cached_input_tokens,omitempty"`
	Cost              float64   `json:"cost"`
	CostEstimated     bool      `json:"cost_estimated"`
	At                time.Time `json:"at"`
}

func (r Record) Tokens() int64 {
	return int64(r.InputTokens) + int64(r.OutputTokens) + int64(r.CachedInputTokens)
}

// Spend is what a subject has used over a window.
type Spend struct {
	Tokens   int64     `json:"tokens"`
	Cost     float64   `json:"cost"`
	Requests int64     `json:"requests"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
}

// LimitError is §3.3's structured refusal on cap breach.
//
// A typed error rather than a message, because three different surfaces have to
// render it: the SPA shows the developer what they hit, the chat stream forwards
// it as an error event, and the assistant may have to explain why a turn stopped.
type LimitError struct {
	// Scope is "user" or "global": which cap was hit. A developer blocked by the
	// global cap has done nothing wrong and can do nothing about it, and telling
	// them apart matters.
	Scope string `json:"scope"`
	// Kind is "tokens" or "cost".
	Kind     string    `json:"kind"`
	Period   string    `json:"period"`
	Cap      float64   `json:"cap"`
	Spent    float64   `json:"spent"`
	ResetsAt time.Time `json:"resets_at"`
}

func (e *LimitError) Error() string {
	return fmt.Sprintf(
		"llm %s limit reached: %s cap of %.4f over %s, %.4f already spent (resets %s)",
		e.Scope, e.Kind, e.Cap, e.Period, e.Spent, e.ResetsAt.UTC().Format(time.RFC3339))
}

// Payload is the wire form, with the discriminator the SPA switches on.
func (e *LimitError) Payload() map[string]any {
	return map[string]any{
		"error":     "limit_exceeded",
		"scope":     e.Scope,
		"kind":      e.Kind,
		"period":    e.Period,
		"cap":       e.Cap,
		"spent":     e.Spent,
		"resets_at": e.ResetsAt.UTC(),
	}
}

// Warning is a soft-limit notice. Not an error: the request proceeds.
type Warning struct {
	Scope    string  `json:"scope"`
	Kind     string  `json:"kind"`
	Cap      float64 `json:"cap"`
	Spent    float64 `json:"spent"`
	Fraction float64 `json:"fraction"`
}

// Verdict is the answer to "may this request run".
type Verdict struct {
	// Spend is what the subject has used in the current period, so a caller can
	// show it without a second query.
	Spend Spend `json:"spend"`
	// Warnings are soft limits crossed. Possibly several: tokens and cost are
	// independent.
	Warnings []Warning `json:"warnings,omitempty"`
	// Unpriced names models used in this period that ODE has no price for, which
	// is why a cost cap may be under-counting. Reported rather than hidden.
	Unpriced []string `json:"unpriced_models,omitempty"`
}

// LimitsRecord is a stored policy with its provenance. §3.3 wants changes
// attributable, and a limit nobody can trace is a limit nobody will change.
type LimitsRecord struct {
	Subject   string    `json:"subject"`
	Limits    Limits    `json:"limits"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// GlobalSubject is the sentinel key for the deployment-wide record.
const GlobalSubject = ""

func (l Limits) marshal() ([]byte, error) { return json.Marshal(l) }

func unmarshalLimits(data []byte) (Limits, error) {
	var limits Limits
	if len(data) == 0 {
		return limits, nil
	}
	err := json.Unmarshal(data, &limits)
	return limits, err
}
