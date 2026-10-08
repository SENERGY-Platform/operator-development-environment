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

// CostLine is one kind of token in a cost: the rate it was priced at, per million
// tokens, and what that came to. The token count is not repeated here; it already
// sits beside the line on whatever carries it.
type CostLine struct {
	PerMTok float64 `json:"per_mtok"`
	Cost    float64 `json:"cost"`
}

// CostBreakdown is a cost taken apart by the four kinds of token a provider bills
// separately. The rates are the effective ones, after the fallbacks for an absent
// cache price, so a reader multiplying tokens by rate gets the figure ODE charged
// rather than having to know those rules.
type CostBreakdown struct {
	Input       CostLine `json:"input"`
	CachedInput CostLine `json:"cached_input"`
	CacheWrite  CostLine `json:"cache_write"`
	Output      CostLine `json:"output"`
}

// Total is the sum of the four lines, added in the order Apply has always added
// them so the figure is the same to the last bit.
func (b CostBreakdown) Total() float64 {
	return b.Input.Cost + b.CachedInput.Cost + b.CacheWrite.Cost + b.Output.Cost
}

// add accumulates another breakdown of the same model. The rates are taken from
// other, which is only right because one exchange runs on one model.
func (b *CostBreakdown) add(other CostBreakdown) {
	b.Input = CostLine{PerMTok: other.Input.PerMTok, Cost: b.Input.Cost + other.Input.Cost}
	b.CachedInput = CostLine{PerMTok: other.CachedInput.PerMTok,
		Cost: b.CachedInput.Cost + other.CachedInput.Cost}
	b.CacheWrite = CostLine{PerMTok: other.CacheWrite.PerMTok,
		Cost: b.CacheWrite.Cost + other.CacheWrite.Cost}
	b.Output = CostLine{PerMTok: other.Output.PerMTok, Cost: b.Output.Cost + other.Output.Cost}
}

// Breakdown prices token counts for a model line by line. found is false for an
// unpriced model, and the breakdown is then empty rather than a row of zeros.
func (p *Pricing) Breakdown(model string, input, cachedInput, cacheWrite, output int64) (CostBreakdown, bool) {
	price, found := p.Lookup(model)
	if !found {
		return CostBreakdown{}, false
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
	line := func(tokens int64, perMTok float64) CostLine {
		return CostLine{PerMTok: perMTok, Cost: float64(tokens) / perMillion * perMTok}
	}
	return CostBreakdown{
		Input:       line(input, price.InputPerMTok),
		CachedInput: line(cachedInput, cachedPrice),
		CacheWrite:  line(cacheWrite, writePrice),
		Output:      line(output, price.OutputPerMTok),
	}, true
}

// Apply fills in the cost of a usage record in place, and the breakdown it was
// added up from.
func (p *Pricing) Apply(usage *Usage) {
	if usage == nil {
		return
	}
	breakdown, found := p.Breakdown(usage.Model, int64(usage.InputTokens),
		int64(usage.CachedInputTokens), int64(usage.CacheWriteTokens), int64(usage.OutputTokens))
	if !found {
		usage.CostEUR = 0
		usage.CostEstimated = false
		usage.CostBreakdown = nil
		return
	}
	usage.CostEUR = breakdown.Total()
	usage.CostEstimated = true
	usage.CostBreakdown = &breakdown
}

// Priced says whether a model has a configured price. The admin surface needs it
// to warn that a cost cap cannot bind on an unpriced model.
func (p *Pricing) Priced(model string) bool {
	_, found := p.Lookup(model)
	return found
}
