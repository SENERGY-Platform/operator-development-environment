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

// Package tools is ODE's LLM tool surface (SPEC §5.8) and the enforcement point
// for the data exposure tiers (§3.2, D4).
//
// Two properties are what this package exists for, and both are structural
// rather than conventional:
//
//   - **Every tool call goes through Dispatcher.Dispatch.** The tier check, the
//     spend check and the confirmation requirement live there and nowhere else,
//     so there is one place to read and one place to test. A tool executor
//     cannot be reached without passing them, because the executor is private to
//     the registry entry and Dispatch is the only thing that calls it.
//
//   - **A denied capability has no tool.** §5.8 lists operations that must not
//     exist — changing the exposure tier, changing admin limits, writing a
//     ProfileOverride, promoting a recommendation. They are absent from the
//     registry, and Denied() plus a test asserts they stay absent. Enforcing
//     those by refusing at dispatch time would be weaker: the tool would still
//     be advertised, and the model would keep asking.
//
// The registry declares all eighteen tools of §5.8 even though this milestone
// implements twelve of them. The declaration is the tier table — one source of
// truth for the paper, the settings UI and the tests — while only a tool with an
// executor is advertised to a provider. An unbuilt tool therefore cannot be
// called and never appears in tools/list; it appears in the documented surface
// with the milestone that will fill it in.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Definition is one row of §5.8's allow-list.
type Definition struct {
	Name string `json:"name"`
	// Description is what the model reads. It is deliberately written for a
	// reader who has to choose between tools, not as a restatement of the name.
	Description string `json:"description"`
	// Effect is §5.8's own column, kept because it is the table published in the
	// paper and it says something the description does not: what the tool does to
	// the platform, as opposed to what it is for.
	Effect  string `json:"effect"`
	MinTier Tier   `json:"min_tier"`
	// Confirm marks a tool whose result is not applied until the developer agrees
	// (D11, §5.10). Dispatch never executes one of these on the model's word.
	Confirm bool `json:"confirm"`
	// Schema is the JSON Schema for the tool's input, as handed to the provider.
	Schema json.RawMessage `json:"schema"`

	// Unavailable says why the tool cannot be called in this deployment, and is
	// the honest answer to "why can I see this in the table but not call it".
	// Two causes read the same way to a model and are therefore one field: the
	// tool belongs to a later milestone, or the service it reads through is not
	// configured here.
	//
	// NewRegistry clears it when an executor is present, so the published table
	// never carries a stale reason beside a tool that in fact works.
	Unavailable string `json:"unavailable,omitempty"`

	// executor is nil for a declared-but-unimplemented tool. Unexported so that
	// nothing outside this package can invoke a tool without going through
	// Dispatch.
	executor Executor
}

// Implemented reports whether the tool has an executor behind it.
func (d Definition) Implemented() bool { return d.executor != nil }

// NewDefinition builds a definition with an executor behind it.
//
// This is how a package outside this one declares a tool — a later milestone
// adding run_code or launch_experiment will need it, and so do the tests in
// pkg/chat. The executor goes into the unexported field, so a caller can declare a
// tool but has no way to invoke it except through Dispatch. That is the property
// that keeps the tier gate total: exporting the field instead would have made it
// possible to run a tool without passing a single check.
func NewDefinition(definition Definition, executor Executor) Definition {
	definition.executor = executor
	return definition
}

// Call is one tool invocation as the provider asked for it.
type Call struct {
	// ID is the provider's own correlation id. Echoed back in the result,
	// because both native tool-use protocols pair the two by id.
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Executor runs a tool. It receives the caller's token because every platform
// read is on behalf of the developer (§3.1 step 3) — an LLM tool is not an
// exception to that, and there is no service account anywhere in this path.
type Executor func(ctx context.Context, req Request) (any, error)

// Request is what an executor is given.
type Request struct {
	// Token is the developer's access token, forwarded upstream.
	Token string
	// UserSub identifies the developer for accounting and for session state.
	UserSub string
	// SessionID is the chat session, so a tool that writes session state
	// (propose_data_selection) knows where to write.
	SessionID string
	// Tier is the session's tier at dispatch time. Passed for the benefit of a
	// tool that shapes its own answer by tier rather than being all-or-nothing;
	// it is *not* where the gate lives, and an executor must not re-check it.
	Tier Tier
	// Input is the raw arguments. Executors unmarshal into their own type.
	Input json.RawMessage

	// Report forwards progress to whoever is watching. Optional; executors call the
	// Progress method rather than this, so they need not nil-check.
	//
	// It must not block: it is called from inside the tool's own work, and a slow
	// consumer must not be able to stall a platform read.
	Report func(Progress)
}

// Progress reports a step of this tool's work. Safe when no reporter is set.
func (r Request) Progress(stage, detail string) {
	if r.Report == nil {
		return
	}
	r.Report(Progress{Stage: stage, Detail: detail})
}

// Progress is one step of a long-running tool.
//
// It exists because the slow tools are slow enough to look broken. A profile_series
// call is a full profiler pass — an availability probe, a bounded raw read, an
// aggregated read, then the detectors — and without it the developer watches nothing
// happen for minutes and cannot tell a working profile from a wedged one.
type Progress struct {
	Tool   string `json:"tool"`
	Stage  string `json:"stage"`
	Detail string `json:"detail"`
}

// Registry holds the tool surface.
type Registry struct {
	byName map[string]Definition
	order  []string
}

// NewRegistry builds a registry from definitions, rejecting duplicates and
// anything on the denied list.
//
// The denied check is here rather than left to review because §5.8's guarantee is
// that no such tool exists. A registry that would accept one, given a careless
// later edit, does not provide that guarantee — so the constructor refuses.
func NewRegistry(definitions ...Definition) (*Registry, error) {
	r := &Registry{byName: make(map[string]Definition, len(definitions))}
	denied := deniedSet()
	for _, definition := range definitions {
		if definition.Name == "" {
			return nil, fmt.Errorf("tools: a definition has no name")
		}
		if _, exists := r.byName[definition.Name]; exists {
			return nil, fmt.Errorf("tools: duplicate tool %q", definition.Name)
		}
		if reason, forbidden := denied[definition.Name]; forbidden {
			return nil, fmt.Errorf(
				"tools: %q is denied by SPEC §5.8 and must not exist as a tool: %s",
				definition.Name, reason)
		}
		if !definition.MinTier.Valid() {
			return nil, fmt.Errorf("tools: %q has an invalid minimum tier %d",
				definition.Name, int(definition.MinTier))
		}
		if len(definition.Schema) == 0 {
			return nil, fmt.Errorf("tools: %q has no input schema", definition.Name)
		}
		if !json.Valid(definition.Schema) {
			return nil, fmt.Errorf("tools: %q has an unparseable input schema", definition.Name)
		}
		if definition.executor == nil && definition.Unavailable == "" {
			return nil, fmt.Errorf(
				"tools: %q has no executor and no Unavailable reason, so nothing explains why it cannot be called",
				definition.Name)
		}
		if definition.executor != nil {
			definition.Unavailable = ""
		}
		r.byName[definition.Name] = definition
		r.order = append(r.order, definition.Name)
	}
	sort.Strings(r.order)
	return r, nil
}

// Lookup returns a definition by name.
func (r *Registry) Lookup(name string) (Definition, bool) {
	definition, found := r.byName[name]
	return definition, found
}

// Definitions is the whole declared surface in name order, implemented or not.
// This is the §5.8 table, and what the documentation route serves.
func (r *Registry) Definitions() []Definition {
	out := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// Available is what a session at this tier may actually be offered: implemented,
// and permitted by the tier.
//
// Filtering by tier here rather than only refusing at dispatch is deliberate and
// is not a second enforcement point — Dispatch still checks, and that check is
// the guarantee. This one is about context economy and about not inviting a
// refusal: advertising preview_series at L0 spends tokens on a tool description
// and then spends more on the model trying it and being told no. The model is
// told which tools exist beyond its tier separately, as prose, so it can suggest
// raising the tier rather than being silently unaware.
func (r *Registry) Available(tier Tier) []Definition {
	out := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		definition := r.byName[name]
		if definition.Implemented() && tier.Permits(definition.MinTier) {
			out = append(out, definition)
		}
	}
	return out
}

// Beyond lists the implemented tools this tier does not reach, so the system
// prompt can say what raising the tier would buy. §3.2 wants the assistant to
// ask the developer to raise the tier rather than fail opaquely, and it can only
// do that if it knows what is up there.
func (r *Registry) Beyond(tier Tier) []Definition {
	out := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		definition := r.byName[name]
		if definition.Implemented() && !tier.Permits(definition.MinTier) {
			out = append(out, definition)
		}
	}
	return out
}

// Denied is §5.8's "no tool exists" list: the capability, and why it is a
// developer action rather than an LLM one.
//
// Exported so the settings surface can show it and so a test can assert that
// none of these names is ever registered. It is documentation with teeth.
func Denied() map[string]string { return deniedSet() }

func deniedSet() map[string]string {
	return map[string]string{
		"set_exposure_tier":          "the exposure tier is the developer's control over what the LLM may see; a tool to raise it would defeat §3.2 entirely",
		"set_admin_limits":           "admin limits bound the LLM's own spend, so the LLM must not be able to move them (§3.3)",
		"write_profile_override":     "a ProfileOverride is a human confirmation of derived semantics and an empirical record; an LLM-written one would be fabricated ground truth (D21, D11)",
		"promote_recommendation":     "recommendations are strictly advisory and become binding only by explicit developer promotion (D28)",
		"modify_evaluation_criteria": "the developer defines the evaluation criteria; that is the human-in-the-loop premise of the whole system",
		"modify_operator_lib":        "the shared Operator Lib is platform code, out of scope for a session",
		"deploy_to_production":       "promotion to a production pipeline is a developer decision, never an autonomous one",
		"delete_platform_data":       "no tool deletes platform data",
		"write_timeseries":           "ODE reads the timeseries store and never writes to it",
	}
}
