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

import "github.com/SENERGY-Platform/operator-development-environment/pkg/exposure"

// The exposure tier of §3.2, which lives in pkg/exposure and is named here.
//
// Aliases rather than a wrapper type: `tools.Tier` and `exposure.Tier` are the
// same type, so the hundred and ninety call sites that say tools.L0 keep saying
// it, a Tier crossing the boundary needs no conversion, and there is exactly one
// definition of what L1 permits. Why it moved is documented where it went.
type Tier = exposure.Tier

const (
	L0 = exposure.L0
	L1 = exposure.L1
	L2 = exposure.L2

	DefaultTier = exposure.DefaultTier
	MaxTier     = exposure.MaxTier
)

var ErrInvalidTier = exposure.ErrInvalidTier

// ParseTier reads a tier from its wire form.
func ParseTier(s string) (Tier, error) { return exposure.ParseTier(s) }

// Tiers is every defined tier, in order.
func Tiers() []Tier { return exposure.Tiers() }
