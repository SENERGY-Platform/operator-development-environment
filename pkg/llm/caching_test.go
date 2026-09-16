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
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// marked reports whether a block carries a cache breakpoint. An unmarked block
// has a zero-valued cache control, so the type string is what distinguishes them.
func marked(block anthropic.ContentBlockParamUnion) bool {
	control := block.GetCacheControl()
	return control != nil && string(control.Type) == "ephemeral"
}

func cachingProvider(t *testing.T) *AnthropicProvider {
	t.Helper()
	provider, err := NewAnthropicProvider("anthropic", AnthropicOptions{
		APIKey: "test-key", Models: []string{"claude-opus-5"},
	}, nil)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	return provider
}

// --- breakpoint placement ---

func TestAnthropicMarksSystemAndConversation(t *testing.T) {
	params, err := cachingProvider(t).params(Request{
		System:   "you are the assistant inside ODE",
		Messages: exchange(),
	})
	if err != nil {
		t.Fatalf("params: %v", err)
	}

	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Three of the API's four, which is what the placement intends: system, the end
	// of the conversation, and where the previous request ended.
	if count := strings.Count(string(encoded), `"cache_control"`); count != 3 {
		t.Errorf("the request carries %d cache breakpoints, want 3: %s", count, encoded)
	}

	if len(params.System) != 1 || string(params.System[0].CacheControl.Type) != "ephemeral" {
		t.Errorf("the system block is not a breakpoint: %+v", params.System)
	}

	if len(params.Messages) != 4 {
		t.Fatalf("messages = %d, want the four of exchange()", len(params.Messages))
	}
	last := params.Messages[3].Content
	if !marked(last[len(last)-1]) {
		t.Error("the last block of the last message is not a breakpoint; " +
			"without it the next request re-reads the whole history")
	}
	// One iteration back, which is where the previous request put its mark. The
	// assistant turn ends in a tool_use block, so this also covers a variant other
	// than text.
	previous := params.Messages[1].Content
	if !marked(previous[len(previous)-1]) {
		t.Error("the previous request's breakpoint is not kept")
	}
	if marked(previous[0]) {
		t.Error("a breakpoint sits on the first block of a message rather than the last; " +
			"everything after it in that message would be re-read every turn")
	}
	for _, index := range []int{0, 2} {
		blocks := params.Messages[index].Content
		if marked(blocks[len(blocks)-1]) {
			t.Errorf("message %d carries an unintended breakpoint", index)
		}
	}
}

func TestAnthropicMarksWhatAShortConversationHas(t *testing.T) {
	params, err := cachingProvider(t).params(Request{
		System:   "short",
		Messages: []Message{UserText("hi")},
	})
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	encoded, _ := json.Marshal(params)
	// System and the one message. The second conversation mark has nowhere to go
	// and must not be forced onto the block that already carries the first.
	if count := strings.Count(string(encoded), `"cache_control"`); count != 2 {
		t.Errorf("breakpoints = %d, want 2: %s", count, encoded)
	}
}

func TestAnthropicMarksNothingWithoutAConversation(t *testing.T) {
	// Not a request ODE sends, but markCachedPrefix must not index into an empty
	// slice on the way to finding that out.
	markCachedPrefix(nil)
	markCachedPrefix([]anthropic.MessageParam{{}})
}

// --- pricing ---

// TestPricingSeparatesTheTokenKinds is the test the whole separation exists for.
//
// It prices one turn twice: once with each kind at its own rate, and once as if
// every input token were fresh input. The two differ by about a factor of two on
// a cache-heavy turn, and that difference is the entire return on prompt caching.
// A record that summed the kinds could not tell them apart afterwards.
func TestPricingSeparatesTheTokenKinds(t *testing.T) {
	pricing := NewPricing("USD", DefaultPrices()...)

	usage := Usage{
		Model:             "claude-opus-5",
		InputTokens:       1_000,
		CachedInputTokens: 50_000,
		CacheWriteTokens:  20_000,
		OutputTokens:      2_000,
	}
	pricing.Apply(&usage)

	const perMillion = 1_000_000.0
	price, found := pricing.Lookup("claude-opus-5")
	if !found {
		t.Fatal("claude-opus-5 is unpriced")
	}
	separate := 1_000/perMillion*price.InputPerMTok +
		50_000/perMillion*price.CachedInputPerMTok +
		20_000/perMillion*price.CacheWritePerMTok +
		2_000/perMillion*price.OutputPerMTok
	summed := 71_000/perMillion*price.InputPerMTok +
		2_000/perMillion*price.OutputPerMTok

	if math.Abs(usage.CostEUR-separate) > 1e-9 {
		t.Errorf("cost = %v, want %v", usage.CostEUR, separate)
	}
	if summed <= usage.CostEUR {
		t.Fatalf("summing the input kinds gave %v, which is not above the separated %v; "+
			"the test no longer measures anything", summed, usage.CostEUR)
	}
	if ratio := summed / usage.CostEUR; ratio < 1.5 {
		t.Errorf("summing the kinds costs only %.2fx the separated figure; "+
			"the separation is meant to be worth more than that", ratio)
	}
}

func TestPricingChargesACacheWriteAboveFreshInput(t *testing.T) {
	// A price that states no cache figures at all, which is what a deployment's
	// own table looked like before this existed.
	pricing := NewPricing("USD", ModelPrice{
		Model: "local-model", InputPerMTok: 10, OutputPerMTok: 20,
	})

	write := Usage{Model: "local-model", CacheWriteTokens: 1_000_000}
	pricing.Apply(&write)
	if want := 10 * cacheWriteMultiplier; math.Abs(write.CostEUR-want) > 1e-9 {
		t.Errorf("an unpriced cache write cost %v, want %v; anything at or below the "+
			"input price would understate a spend cap", write.CostEUR, want)
	}

	fresh := Usage{Model: "local-model", InputTokens: 1_000_000}
	pricing.Apply(&fresh)
	if write.CostEUR <= fresh.CostEUR {
		t.Errorf("a cache write (%v) is not dearer than fresh input (%v)",
			write.CostEUR, fresh.CostEUR)
	}
}

// TestConfiguredPricesWinTheWholeLookup guards the promise pkg.go makes when it
// puts DefaultPrices underneath the configured table.
//
// The exact-match half of that was never in doubt. The prefix half was: with one
// combined list the longest match wins wherever it came from, so an admin who
// reprices a family with a short prefix would lose to any longer built-in entry
// that also matched — silently, and in the direction of the stale figure.
func TestConfiguredPricesWinTheWholeLookup(t *testing.T) {
	configured := []ModelPrice{
		{Model: "claude-opus-5", InputPerMTok: 7, OutputPerMTok: 30},
		{Model: "claude-sonnet", InputPerMTok: 3.5, OutputPerMTok: 17},
	}
	pricing := NewPricingWithFallback("USD", configured, DefaultPrices())

	// Exact name in both tables.
	if price, _ := pricing.Lookup("claude-opus-5"); price.InputPerMTok != 7 {
		t.Errorf("an exactly configured model resolved to %v, want the configured 7",
			price.InputPerMTok)
	}
	// A short configured prefix against a longer built-in name. This is the case
	// that regressed.
	if price, _ := pricing.Lookup("claude-sonnet-4-6"); price.InputPerMTok != 3.5 {
		t.Errorf("a configured prefix lost to the built-in table: got %v, want 3.5",
			price.InputPerMTok)
	}
	// A model configuration says nothing about at all still finds the floor.
	price, found := pricing.Lookup("claude-haiku-4-5")
	if !found || price.InputPerMTok != 1 {
		t.Errorf("an unconfigured model did not fall through to the built-in table: "+
			"%+v found=%v", price, found)
	}
	// And the admin surface shows both tables, or the floor is invisible to whoever
	// is asking why a cap binds.
	if shown := len(pricing.Prices()); shown != len(configured)+len(DefaultPrices()) {
		t.Errorf("Prices() lists %d entries, want configuration and the floor together",
			shown)
	}
}

func TestDefaultPricesAreComplete(t *testing.T) {
	if PricesAsOf == "" {
		t.Error("the built-in table carries no date, so nobody can tell how stale it is")
	}
	for _, price := range DefaultPrices() {
		if price.Model == "" || price.InputPerMTok <= 0 || price.OutputPerMTok <= 0 {
			t.Errorf("%+v is not a usable price", price)
			continue
		}
		if price.CachedInputPerMTok <= 0 || price.CachedInputPerMTok >= price.InputPerMTok {
			t.Errorf("%s prices a cache read at %v, which is not below its input price %v",
				price.Model, price.CachedInputPerMTok, price.InputPerMTok)
		}
		if price.CacheWritePerMTok <= price.InputPerMTok {
			t.Errorf("%s prices a cache write at %v, which is not above its input price %v",
				price.Model, price.CacheWritePerMTok, price.InputPerMTok)
		}
	}
}

// --- run log ---

func TestRunLogWritesOneLinePerRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	log, err := NewRunLog(path)
	if err != nil {
		t.Fatalf("NewRunLog: %v", err)
	}
	defer log.Close()

	start := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	log.Append(context.Background(), RunEntry{
		Start: start, End: start.Add(90 * time.Second),
		Provider: "anthropic", Model: "claude-opus-5", Calls: 3,
		InputTokens: 1_000, CacheWriteTokens: 20_000,
		CachedInputTokens: 50_000, OutputTokens: 2_000,
		Cost: 0.205, Currency: "USD", CostEstimated: true,
	})
	log.Append(context.Background(), RunEntry{Start: start, End: start, Calls: 1})

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer file.Close()

	entries := []RunEntry{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry RunEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("a line is not one JSON object: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("lines = %d, want one per run", len(entries))
	}
	first := entries[0]
	if first.DurationMS != 90_000 {
		t.Errorf("duration = %dms, want 90000 derived from the two timestamps",
			first.DurationMS)
	}
	if first.Calls != 3 {
		t.Errorf("calls = %d, want the provider requests the run actually made", first.Calls)
	}
	if first.InputTokens != 1_000 || first.CacheWriteTokens != 20_000 ||
		first.CachedInputTokens != 50_000 || first.OutputTokens != 2_000 {
		t.Errorf("token kinds did not survive the round trip: %+v", first)
	}
}

func TestRunLogIsOffWithoutAPath(t *testing.T) {
	log, err := NewRunLog("")
	if err != nil {
		t.Fatalf("NewRunLog: %v", err)
	}
	if log != nil {
		t.Fatal("an empty path must leave the log off")
	}
	// Every method has to survive that, or every caller needs a branch.
	log.Append(context.Background(), RunEntry{Calls: 1})
	if err := log.Close(); err != nil {
		t.Errorf("Close on a log that is off: %v", err)
	}
}

func TestRunLogRefusesAPathItCannotOpen(t *testing.T) {
	// A directory that does not exist. Reported at startup rather than swallowed:
	// a log somebody asked for and that writes nowhere reads as an absence of runs.
	if _, err := NewRunLog(filepath.Join(t.TempDir(), "no", "such", "dir", "runs.jsonl")); err == nil {
		t.Error("an unopenable path was accepted")
	}
}
