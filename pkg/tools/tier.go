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

package tools

import (
	"errors"
	"fmt"
)

// Tier is a data exposure tier (SPEC §3.2, D4).
//
// The ordering is the whole point, so it is an integer rather than a string: a
// tool is permitted when the session's tier is at least the tool's minimum, and
// "at least" has to be a comparison the compiler checks. A string tier would
// invite a `==` somewhere, and `tier == "L2"` silently denies an L0 tool.
type Tier int

const (
	// L0 is the default and exposes no values whatsoever: the ontology, device
	// names and types, availability windows, volume and rate estimates,
	// connection state, and QuickProfile.
	L0 Tier = 0
	// L1 adds SeriesProfile and RelationProfile — statistics, detected periods,
	// session stats, quality flags. Aggregates are still data, which is why this
	// is a deliberate step rather than part of L0.
	L1 Tier = 1
	// L2 adds downsampled series previews, meaning actual values.
	L2 Tier = 2
)

// DefaultTier is what a session starts at (§3.2: "Session-scoped,
// developer-settable, default L0").
const DefaultTier = L0

// MaxTier is the highest tier that exists. Used to reject a configured ceiling
// that would otherwise silently mean "no limit at all".
const MaxTier = L2

var ErrInvalidTier = errors.New("tools: invalid exposure tier")

func (t Tier) String() string {
	switch t {
	case L0:
		return "L0"
	case L1:
		return "L1"
	case L2:
		return "L2"
	default:
		return fmt.Sprintf("L?(%d)", int(t))
	}
}

// Permits reports whether a session at this tier may run a tool whose minimum
// is min.
func (t Tier) Permits(min Tier) bool { return t >= min }

// Valid guards against a tier that arrived from JSON or a database as an
// integer outside the defined range.
func (t Tier) Valid() bool { return t >= L0 && t <= MaxTier }

// ParseTier reads a tier from its wire form. Case-insensitive on the prefix
// only in the sense that both "L1" and "l1" are accepted; a bare "1" is not,
// because a tier is a named exposure level and accepting the integer form
// invites an off-by-one from a caller that thinks tiers are 1-based.
func ParseTier(s string) (Tier, error) {
	switch s {
	case "L0", "l0":
		return L0, nil
	case "L1", "l1":
		return L1, nil
	case "L2", "l2":
		return L2, nil
	default:
		return L0, fmt.Errorf("%w: %q is not one of L0, L1, L2", ErrInvalidTier, s)
	}
}

// MarshalJSON renders the tier as "L0" rather than 0.
//
// The integer is an implementation detail of the comparison; every document that
// leaves ODE says "L0", including the refusal of §3.2 that the LLM has to relay
// to the developer.
func (t Tier) MarshalJSON() ([]byte, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("%w: %d", ErrInvalidTier, int(t))
	}
	return []byte(`"` + t.String() + `"`), nil
}

func (t *Tier) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return fmt.Errorf("%w: expected a string like \"L0\", got %s", ErrInvalidTier, s)
	}
	parsed, err := ParseTier(s[1 : len(s)-1])
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// Tiers is every defined tier, in order. For the settings surface and the docs.
func Tiers() []Tier { return []Tier{L0, L1, L2} }

// Exposes describes what a tier adds, for the UI and for §3.2's requirement that
// the current tier be surfaced persistently rather than buried in a menu.
func (t Tier) Exposes() string {
	switch t {
	case L0:
		return "Ontology, device names and types, availability windows, volume and rate estimates, " +
			"connection state, QuickProfile. No values whatsoever."
	case L1:
		return "L0 plus SeriesProfile and RelationProfile: statistics, detected periods, " +
			"session stats, quality flags. Aggregates are still data."
	case L2:
		return "L1 plus downsampled series previews, which are actual values."
	default:
		return ""
	}
}
