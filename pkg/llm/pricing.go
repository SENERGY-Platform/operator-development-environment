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

import (
	"strings"
	"sync"
)

// ModelPrice is what a million tokens costs, in the currency the deployment
// configures. §3.3 requires an estimated cost per request, and an estimate needs
// a price from somewhere: no provider returns one.
type ModelPrice struct {
	// Model is matched exactly first, then as a prefix. A prefix entry lets one
	// line cover a family without listing every dated snapshot, and an exact entry
	// still wins where a family member is priced differently.
	Model         string  `json:"model"`
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	// CachedInputPerMTok prices a cache read, which is far cheaper than fresh
	// input. Zero means "same as input", which overstates cost rather than
	// understating it — the safe direction for a spend cap.
	CachedInputPerMTok float64 `json:"cached_input_per_mtok,omitempty"`
}

// Pricing turns token counts into an estimated cost.
//
// It is deliberately a lookup over configuration rather than a table baked into
// the binary: published prices change, and a hard-coded figure that silently
// goes stale is worse than an absent one, because a spend cap computed from it
// would be wrong without anyone noticing.
//
// An unpriced model yields no cost and sets Priced to false, so an admin can see
// that a cap is not actually being enforced for it rather than seeing zero spend
// and concluding the model is free.
type Pricing struct {
	mux    sync.RWMutex
	prices []ModelPrice
	// currency labels the figures. Not converted anywhere: the provider bills in
	// its own currency and pretending otherwise would need a rate ODE does not have.
	currency string
}

const defaultCurrency = "EUR"

func NewPricing(currency string, prices ...ModelPrice) *Pricing {
	if currency == "" {
		currency = defaultCurrency
	}
	return &Pricing{prices: prices, currency: currency}
}

func (p *Pricing) Currency() string {
	if p == nil {
		return defaultCurrency
	}
	p.mux.RLock()
	defer p.mux.RUnlock()
	return p.currency
}

// Prices is the configured table, for the admin surface.
func (p *Pricing) Prices() []ModelPrice {
	if p == nil {
		return []ModelPrice{}
	}
	p.mux.RLock()
	defer p.mux.RUnlock()
	return append([]ModelPrice{}, p.prices...)
}

// Lookup finds the price for a model: exact match first, then the longest
// matching prefix, so a specific entry always beats a family one.
func (p *Pricing) Lookup(model string) (ModelPrice, bool) {
	if p == nil || model == "" {
		return ModelPrice{}, false
	}
	p.mux.RLock()
	defer p.mux.RUnlock()

	for _, price := range p.prices {
		if price.Model == model {
			return price, true
		}
	}

	best := ModelPrice{}
	found := false
	for _, price := range p.prices {
		if price.Model == "" || !strings.HasPrefix(model, price.Model) {
			continue
		}
		if !found || len(price.Model) > len(best.Model) {
			best, found = price, true
		}
	}
	return best, found
}

// Apply fills in the cost of a usage record in place.
func (p *Pricing) Apply(usage *Usage) {
	if usage == nil {
		return
	}
	price, found := p.Lookup(usage.Model)
	if !found {
		usage.CostEUR = 0
		usage.CostEstimated = false
		return
	}

	// Cached input is priced separately and is not part of InputTokens on either
	// provider, so the two are added rather than subtracted.
	cachedPrice := price.CachedInputPerMTok
	if cachedPrice == 0 {
		cachedPrice = price.InputPerMTok
	}

	const perMillion = 1_000_000.0
	usage.CostEUR = float64(usage.InputTokens)/perMillion*price.InputPerMTok +
		float64(usage.CachedInputTokens)/perMillion*cachedPrice +
		float64(usage.OutputTokens)/perMillion*price.OutputPerMTok
	usage.CostEstimated = true
}

// Priced says whether a model has a configured price. The admin surface needs it
// to warn that a cost cap cannot bind on an unpriced model.
func (p *Pricing) Priced(model string) bool {
	_, found := p.Lookup(model)
	return found
}
