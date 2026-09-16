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

package llm

// PricesAsOf dates the table below.
//
// It is here because the table is the one piece of ODE that goes wrong by
// standing still: the code stays correct while the world moves, and nothing in a
// test or a startup log would notice. A date lets an admin compare the table
// against the published list and see how old the answer is, which is the least a
// figure that feeds a spend cap owes its reader.
const PricesAsOf = "2026-09-16"

// PricesCurrency names what the figures below are in.
//
// They are Anthropic's published first-party API rates, which are quoted in US
// dollars. A deployment that sets llm_currency to something else does not convert
// them — Pricing holds no exchange rate and inventing one would make the label
// the only true part of the figure. Either configure prices in the currency the
// label claims, or read the label as documentation of intent rather than as a
// conversion having happened.
const PricesCurrency = "USD"

// cacheWriteMultiplier is what writing a cache entry costs over plain input, used
// where a price leaves the figure at zero.
//
// Anthropic's published relation for the five-minute TTL. There is no matching
// constant for a cache *read*, and the asymmetry is the point: an absent read
// price falls back to the full input price, which overstates, whereas an absent
// write price cannot fall back to the input price because that would understate.
// A spend cap may only err upward.
const cacheWriteMultiplier = 1.25

// DefaultPrices is the built-in table, per million tokens, as of PricesAsOf.
//
// A table in the binary is a compromise, and the argument against it is in
// Pricing's own doc comment: a published price that changes leaves a stale figure
// behind, and a spend cap computed from one is quietly wrong. What decided it the
// other way is the alternative that was actually in place — a deployment that
// configures no prices has *no* figures at all, so every model is unpriced, every
// cost is zero and every cost cap silently does not bind. A dated table that can
// be out by a revision is a better failure than that.
//
// Configuration wins over this table, model by model: see where pkg.go assembles
// the two.
//
// Only Anthropic models are listed. The OpenAI-compatible transport serves
// whatever the deployment started, ODE has no list of it, and guessing would be
// worse than the honest "unpriced" that an absent entry produces.
func DefaultPrices() []ModelPrice {
	return []ModelPrice{
		// Claude Fable 5.1 prices a cache read below the tenth the rest of the family
		// pays, so it is stated rather than left to the multiplier.
		{Model: "claude-fable-5-1", InputPerMTok: 10, OutputPerMTok: 50,
			CachedInputPerMTok: 0.25, CacheWritePerMTok: 12.5},
		{Model: "claude-fable-5", InputPerMTok: 10, OutputPerMTok: 50,
			CachedInputPerMTok: 1, CacheWritePerMTok: 12.5},
		{Model: "claude-opus-5", InputPerMTok: 5, OutputPerMTok: 25,
			CachedInputPerMTok: 0.5, CacheWritePerMTok: 6.25},
		{Model: "claude-opus-4-8", InputPerMTok: 5, OutputPerMTok: 25,
			CachedInputPerMTok: 0.5, CacheWritePerMTok: 6.25},
		{Model: "claude-sonnet-5", InputPerMTok: 2, OutputPerMTok: 10,
			CachedInputPerMTok: 0.2, CacheWritePerMTok: 2.5},
		{Model: "claude-sonnet-4-6", InputPerMTok: 3, OutputPerMTok: 15,
			CachedInputPerMTok: 0.3, CacheWritePerMTok: 3.75},
		{Model: "claude-haiku-4-5", InputPerMTok: 1, OutputPerMTok: 5,
			CachedInputPerMTok: 0.1, CacheWritePerMTok: 1.25},
	}
}
