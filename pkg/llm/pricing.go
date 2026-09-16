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
	// CacheWritePerMTok prices a cache write, which is *dearer* than fresh input:
	// the entry is built as well as read.
	//
	// Zero therefore cannot fall back to the input price the way the line above
	// does — that would understate, which is the direction a spend cap must not
	// err in. It falls back to cacheWriteMultiplier times the input price instead,
	// which is the published relation.
	CacheWritePerMTok float64 `json:"cache_write_per_mtok,omitempty"`
}

// Pricing turns token counts into an estimated cost.
//
// A lookup rather than arithmetic against a fixed table, because published prices
// change and a figure that silently goes stale makes a spend cap quietly wrong.
// Configuration is therefore the authority. DefaultPrices in prices.go sits
// underneath it as a dated floor and is consulted only where configuration is
// silent — see where pkg.go assembles the two, and PricesAsOf for how old that
// floor is.
//
// An unpriced model yields no cost and sets Priced to false, so an admin can see
// that a cap is not actually being enforced for it rather than seeing zero spend
// and concluding the model is free.
type Pricing struct {
	mux    sync.RWMutex
	prices []ModelPrice
	// fallback is the built-in table, searched only after prices has failed to
	// name the model at all.
	//
	// A second slice rather than one list with the built-in entries appended,
	// because "configuration wins" has to hold for a prefix entry too. In one list
	// the prefix search picks the longest match wherever it came from, so an admin
	// who reprices a whole family with a short prefix loses silently to any longer
	// built-in entry that also matches — the opposite of what configuring a price
	// is for.
	fallback []ModelPrice
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

// NewPricingWithFallback is what a deployment builds: the admin's own table, and
// underneath it a table consulted only where the first says nothing about a model
// — by exact name or by prefix. See DefaultPrices in prices.go for what that floor
// is and why it exists.
func NewPricingWithFallback(currency string, configured, fallback []ModelPrice) *Pricing {
	pricing := NewPricing(currency, configured...)
	pricing.fallback = fallback
	return pricing
}

func (p *Pricing) Currency() string {
	if p == nil {
		return defaultCurrency
	}
	p.mux.RLock()
	defer p.mux.RUnlock()
	return p.currency
}

// Prices is the table in effect, for the admin surface: the configured entries
// first, then the built-in ones underneath them, in the order Lookup consults
// them. Both, because an admin reading this to see why a cap binds needs to see
// the floor as well as their own list.
func (p *Pricing) Prices() []ModelPrice {
	if p == nil {
		return []ModelPrice{}
	}
	p.mux.RLock()
	defer p.mux.RUnlock()
	out := make([]ModelPrice, 0, len(p.prices)+len(p.fallback))
	out = append(out, p.prices...)
	return append(out, p.fallback...)
}

// Lookup finds the price for a model.
//
// The configured table is searched to exhaustion before the built-in one is
// touched, so a model the admin has priced never resolves to a built-in figure —
// not even where a built-in entry would have matched it with a longer prefix.
func (p *Pricing) Lookup(model string) (ModelPrice, bool) {
	if p == nil || model == "" {
		return ModelPrice{}, false
	}
	p.mux.RLock()
	defer p.mux.RUnlock()

	if price, found := lookupIn(p.prices, model); found {
		return price, true
	}
	return lookupIn(p.fallback, model)
}

// lookupIn searches one table: exact match first, then the longest matching
// prefix, so a specific entry always beats a family one.
func lookupIn(prices []ModelPrice, model string) (ModelPrice, bool) {
	for _, price := range prices {
		if price.Model == model {
			return price, true
		}
	}

	best := ModelPrice{}
	found := false
	for _, price := range prices {
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
	// provider, so the three are added rather than subtracted.
	cachedPrice := price.CachedInputPerMTok
	if cachedPrice == 0 {
		cachedPrice = price.InputPerMTok
	}
	writePrice := price.CacheWritePerMTok
	if writePrice == 0 {
		writePrice = price.InputPerMTok * cacheWriteMultiplier
	}

	const perMillion = 1_000_000.0
	usage.CostEUR = float64(usage.InputTokens)/perMillion*price.InputPerMTok +
		float64(usage.CachedInputTokens)/perMillion*cachedPrice +
		float64(usage.CacheWriteTokens)/perMillion*writePrice +
		float64(usage.OutputTokens)/perMillion*price.OutputPerMTok
	usage.CostEstimated = true
}

// Priced says whether a model has a configured price. The admin surface needs it
// to warn that a cost cap cannot bind on an unpriced model.
func (p *Pricing) Priced(model string) bool {
	_, found := p.Lookup(model)
	return found
}
