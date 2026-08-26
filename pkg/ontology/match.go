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

package ontology

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/SENERGY-Platform/models/go/models"
)

// Text intent to ontology entity — the first step of semantic selection
// (SPEC §5.2, "resolve function: measuring-function ~ 'power generation'").
//
// This is lexical, deterministic and deliberately modest. It is not a semantic
// model and does not pretend to be one: it scores how much of an ontology term's
// own wording an intent mentions, returns the ranked alternatives with the
// evidence behind each, and reports the words it could not map at all. Three
// consequences follow, all of them intentional:
//
//   - Nothing is auto-narrowed. A single best match would hide the case where the
//     second one was meant, and the caller — a developer in M2, an LLM in M3 —
//     is the one able to tell them apart.
//   - There is no synonym table. Deciding that "generation" means "production"
//     would be this matcher asserting domain knowledge it does not have, and a
//     wrong synonym is invisible in the result. The ontology's own vocabulary is
//     the vocabulary; UnmatchedTerms is how a caller learns theirs differs.
//   - Descriptions are not searched. They are prose, they mention neighbouring
//     concepts, and matching them turns every intent into a long list.
//
// The step that actually does the semantic work is the caller's: explicit ids
// bypass all of this (see ExplicitFunctions), which is how M3's LLM will use it
// once it has read the ontology itself.

const (
	defaultMatchLimit = 5
	maxMatchLimit     = 25
	// defaultMinScore keeps a match that accounts for at least half of the
	// ontology term's own words. "Power Generation" matched by "generation" alone
	// scores exactly 0.5 and survives; one word of a four-word term does not.
	defaultMinScore = 0.5
	// minTermRunes drops single characters, which match everything and mean
	// nothing.
	minTermRunes = 2
	// minPrefixRunes is how long a term must be before a prefix relation counts as
	// a match, so "power" and "powerful" relate but "pv" and "pvc" do not.
	minPrefixRunes = 4
)

// Bases a match can rest on.
const (
	BasisDisplayName = "display_name"
	BasisName        = "name"
	BasisExplicitID  = "explicit_id"
)

// Matched is the evidence behind one text-to-ontology resolution. It travels
// with every match for the same reason a detector's evidence travels with its
// verdict (D23): a resolution a caller cannot audit is one they have to trust.
type Matched struct {
	Score float64 `json:"score"`
	// Terms are the caller's own words that this match used, not the ontology's.
	Terms []string `json:"matched_terms"`
	Basis string   `json:"basis"`
}

type FunctionMatch struct {
	Id        string  `json:"id"`
	Name      string  `json:"name"`
	RdfType   string  `json:"rdf_type"`
	ConceptId string  `json:"concept_id"`
	Matched   Matched `json:"matched"`
}

// AspectMatch carries DescendantsIncluded because it is always true and worth
// stating: the device repository expands an aspect criterion to the node plus
// every descendant, so selecting "Building" selects its rooms too.
type AspectMatch struct {
	Id                  string  `json:"id"`
	Name                string  `json:"name"`
	DescendantsIncluded bool    `json:"descendants_included"`
	Matched             Matched `json:"matched"`
}

type DeviceClassMatch struct {
	Id      string  `json:"id"`
	Name    string  `json:"name"`
	Matched Matched `json:"matched"`
}

// Intent is a text intent to resolve against a snapshot.
type Intent struct {
	Text string
	// Limit is how many matches to keep per entity type. Zero means the default.
	Limit int
	// MinScore is the lowest score a match may keep. Zero means the default; a
	// negative value keeps every candidate sharing at least one term, which is
	// how a caller asks to see what was nearly matched.
	MinScore float64
	// IncludeControlling searches controlling functions too. Off by default: a
	// series is something measured, and §5.2 resolves an intent to a measuring
	// function.
	IncludeControlling bool
}

// The lists an Elision can name. They are the JSON keys of IntentMatch itself, so
// a reader can find the list a count belongs to without a second mapping.
const (
	FieldMatchedFunctions     = "matched_functions"
	FieldMatchedAspects       = "matched_aspects"
	FieldMatchedDeviceClasses = "matched_device_classes"
)

// Elision records a list the match limit cut, so a caller reading five matches
// knows whether five was the answer or the ceiling (D26).
//
// The shape is profiler.Elision's, field for field, but declared here rather than
// reused: pkg/profiler imports pkg/ontology, so the dependency cannot run the
// other way. Same idiom on the wire, forced into a second declaration by the
// import direction — keep the two in step if either moves.
type Elision struct {
	Field string `json:"field"`
	Total int    `json:"total"`
	Shown int    `json:"shown"`
	Fetch string `json:"fetch,omitempty"`
}

// IntentMatch is what an intent resolved to, plus what it did not.
type IntentMatch struct {
	Functions     []FunctionMatch    `json:"matched_functions"`
	Aspects       []AspectMatch      `json:"matched_aspects"`
	DeviceClasses []DeviceClassMatch `json:"matched_device_classes"`

	// Elided names the lists the limit truncated, with the total that matched and
	// the number kept. One entry per list that lost something, and nothing for a
	// list that did not: a matcher that narrows silently is one whose caller cannot
	// tell a complete answer from a prefix of one, which is the case the header's
	// "nothing is auto-narrowed" rule is about.
	Elided []Elision `json:"elided"`

	// Terms is the intent as the matcher read it, so a caller can see that
	// "PV-Anlage" became two searchable words.
	Terms []string `json:"terms"`
	// UnmatchedTerms are the words no kept match used. This is the honest half of
	// a matcher with no thesaurus: "pv" appearing here says the platform ontology
	// has no such wording, which is a fact about the ontology rather than a
	// failure to try harder.
	UnmatchedTerms []string `json:"unmatched_terms"`
}

// MatchIntent resolves an intent against a snapshot. It reads nothing and
// allocates nothing outside itself, so it is a pure function of the ontology.
func MatchIntent(snap *Snapshot, intent Intent) IntentMatch {
	out := IntentMatch{
		Functions:      []FunctionMatch{},
		Aspects:        []AspectMatch{},
		DeviceClasses:  []DeviceClassMatch{},
		Elided:         []Elision{},
		Terms:          []string{},
		UnmatchedTerms: []string{},
	}
	if snap == nil {
		return out
	}

	asked := parseTerms(intent.Text)
	for _, t := range asked {
		out.Terms = append(out.Terms, t.text)
	}
	if len(asked) == 0 {
		return out
	}

	limit := intent.Limit
	switch {
	case limit <= 0:
		limit = defaultMatchLimit
	case limit > maxMatchLimit:
		limit = maxMatchLimit
	}
	minScore := intent.MinScore
	if minScore == 0 {
		minScore = defaultMinScore
	}

	functions := snap.MeasuringFunctions
	if intent.IncludeControlling {
		functions = append(append([]models.Function{}, functions...), snap.ControllingFunctions...)
	}
	for _, f := range functions {
		matched, ok := bestMatch(asked,
			label{BasisDisplayName, f.DisplayName},
			label{BasisName, f.Name})
		if !ok || matched.Score < minScore {
			continue
		}
		out.Functions = append(out.Functions, FunctionMatch{
			Id: f.Id, Name: functionName(f), RdfType: f.RdfType, ConceptId: f.ConceptId,
			Matched: matched,
		})
	}
	sortMatches(out.Functions, func(m FunctionMatch) (float64, string, string) {
		return m.Matched.Score, m.Name, m.Id
	})
	out.Functions, out.Elided = truncate(out.Functions, limit, FieldMatchedFunctions, out.Elided)

	for _, node := range snap.AspectNodes {
		matched, ok := bestMatch(asked, label{BasisName, node.Name})
		if !ok || matched.Score < minScore {
			continue
		}
		out.Aspects = append(out.Aspects, AspectMatch{
			Id: node.Id, Name: node.Name, DescendantsIncluded: true, Matched: matched,
		})
	}
	sortMatches(out.Aspects, func(m AspectMatch) (float64, string, string) {
		return m.Matched.Score, m.Name, m.Id
	})
	out.Aspects, out.Elided = truncate(out.Aspects, limit, FieldMatchedAspects, out.Elided)

	for _, class := range snap.DeviceClasses {
		matched, ok := bestMatch(asked, label{BasisName, class.Name})
		if !ok || matched.Score < minScore {
			continue
		}
		out.DeviceClasses = append(out.DeviceClasses, DeviceClassMatch{
			Id: class.Id, Name: class.Name, Matched: matched,
		})
	}
	sortMatches(out.DeviceClasses, func(m DeviceClassMatch) (float64, string, string) {
		return m.Matched.Score, m.Name, m.Id
	})
	out.DeviceClasses, out.Elided = truncate(out.DeviceClasses, limit, FieldMatchedDeviceClasses, out.Elided)

	used := map[string]bool{}
	for _, m := range out.Functions {
		for _, term := range m.Matched.Terms {
			used[term] = true
		}
	}
	for _, m := range out.Aspects {
		for _, term := range m.Matched.Terms {
			used[term] = true
		}
	}
	for _, m := range out.DeviceClasses {
		for _, term := range m.Matched.Terms {
			used[term] = true
		}
	}
	for _, t := range asked {
		if !used[t.text] {
			out.UnmatchedTerms = append(out.UnmatchedTerms, t.text)
		}
	}
	return out
}

// ExplicitFunctions resolves caller-supplied function ids into the same match
// shape an intent produces, so a resolution looks identical however it was
// asked for.
//
// An id the snapshot does not know is reported rather than dropped or refused:
// the snapshot can be older than the platform, and the device repository is the
// authority on whether an id exists. The caller decides whether to query with it
// anyway.
func ExplicitFunctions(snap *Snapshot, ids []string) (matches []FunctionMatch, unknown []string) {
	matches, unknown = []FunctionMatch{}, []string{}
	if snap == nil {
		return matches, append(unknown, ids...)
	}
	known := map[string]models.Function{}
	for _, f := range append(append([]models.Function{}, snap.MeasuringFunctions...), snap.ControllingFunctions...) {
		known[f.Id] = f
	}
	for _, id := range ids {
		f, found := known[id]
		if !found {
			unknown = append(unknown, id)
			matches = append(matches, FunctionMatch{Id: id, Matched: explicit()})
			continue
		}
		matches = append(matches, FunctionMatch{
			Id: f.Id, Name: functionName(f), RdfType: f.RdfType, ConceptId: f.ConceptId,
			Matched: explicit(),
		})
	}
	return matches, unknown
}

// ExplicitAspects is ExplicitFunctions for aspect nodes.
func ExplicitAspects(snap *Snapshot, ids []string) (matches []AspectMatch, unknown []string) {
	matches, unknown = []AspectMatch{}, []string{}
	if snap == nil {
		return matches, append(unknown, ids...)
	}
	known := map[string]models.AspectNode{}
	for _, node := range snap.AspectNodes {
		known[node.Id] = node
	}
	for _, id := range ids {
		node, found := known[id]
		if !found {
			unknown = append(unknown, id)
			matches = append(matches, AspectMatch{Id: id, DescendantsIncluded: true, Matched: explicit()})
			continue
		}
		matches = append(matches, AspectMatch{
			Id: node.Id, Name: node.Name, DescendantsIncluded: true, Matched: explicit(),
		})
	}
	return matches, unknown
}

// ExplicitDeviceClasses is ExplicitFunctions for device classes.
func ExplicitDeviceClasses(snap *Snapshot, ids []string) (matches []DeviceClassMatch, unknown []string) {
	matches, unknown = []DeviceClassMatch{}, []string{}
	if snap == nil {
		return matches, append(unknown, ids...)
	}
	known := map[string]models.DeviceClass{}
	for _, class := range snap.DeviceClasses {
		known[class.Id] = class
	}
	for _, id := range ids {
		class, found := known[id]
		if !found {
			unknown = append(unknown, id)
			matches = append(matches, DeviceClassMatch{Id: id, Matched: explicit()})
			continue
		}
		matches = append(matches, DeviceClassMatch{Id: class.Id, Name: class.Name, Matched: explicit()})
	}
	return matches, unknown
}

func explicit() Matched {
	return Matched{Score: 1, Terms: []string{}, Basis: BasisExplicitID}
}

func functionName(f models.Function) string {
	if f.DisplayName != "" {
		return f.DisplayName
	}
	return f.Name
}

// --- scoring ---

type label struct {
	basis string
	text  string
}

// term is one searchable word: the caller's spelling, and the key it is compared
// on. The two differ because comparison folds plurals, and reporting "statu"
// back to a developer who typed "status" would be a worse answer than the fold
// is worth.
type term struct {
	text string
	key  string
}

// bestMatch scores every label and keeps the strongest, so a function found
// through its display name reports that rather than the internal name it also
// half-matches.
func bestMatch(asked []term, labels ...label) (Matched, bool) {
	best := Matched{}
	found := false
	for _, l := range labels {
		score, terms := scoreLabel(asked, l.text)
		if score <= 0 {
			continue
		}
		if !found || score > best.Score {
			best = Matched{Score: score, Terms: terms, Basis: l.basis}
			found = true
		}
	}
	return best, found
}

// scoreLabel measures how much of the label's own wording the intent mentions:
// "Power Generation" fully named scores 1, named by one of its two words scores
// 0.5.
//
// Coverage is of the *label*, not of the intent, and that asymmetry is the whole
// trick. An intent carries task words — "forecast", "for this site" — that no
// ontology term will ever contain, so scoring how much of the intent was consumed
// would punish every real match for the words around it.
//
// There is deliberately no bonus for the term appearing contiguously. It would
// never change an outcome: a label whose words all appear in order has full
// coverage and already scores 1, so the bonus could only be added to a maximum.
func scoreLabel(asked []term, text string) (float64, []string) {
	labelTerms := parseTerms(text)
	if len(labelTerms) == 0 {
		return 0, nil
	}

	hits := 0
	matched := []string{}
	seen := map[string]bool{}
	for _, lt := range labelTerms {
		t, ok := findTerm(asked, lt.key)
		if !ok {
			continue
		}
		hits++
		if !seen[t.text] {
			seen[t.text] = true
			matched = append(matched, t.text)
		}
	}
	if hits == 0 {
		return 0, nil
	}

	return round2(float64(hits) / float64(len(labelTerms))), matched
}

// findTerm accepts an exact key or a prefix relation between two long enough
// terms, which folds the endings a fold of plurals alone does not — "measuring"
// against "measurement" — without pretending to stem.
func findTerm(asked []term, key string) (term, bool) {
	for _, t := range asked {
		if t.key == key {
			return t, true
		}
		short, long := t.key, key
		if len(long) < len(short) {
			short, long = long, short
		}
		if utf8.RuneCountInString(short) >= minPrefixRunes && strings.HasPrefix(long, short) {
			return t, true
		}
	}
	return term{}, false
}

// parseTerms turns free text into searchable terms, dropping duplicates and
// keeping the order they were written in.
func parseTerms(text string) []term {
	out := []term{}
	seen := map[string]bool{}
	for _, word := range splitWords(text) {
		if utf8.RuneCountInString(word) < minTermRunes || stopwords[word] {
			continue
		}
		key := fold(word)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, term{text: word, key: key})
	}
	return out
}

// splitWords breaks text at anything that is not a letter or a digit, and also
// at camel-case boundaries.
//
// The second part is what makes platform function names searchable at all: they
// arrive as getPowerConsumptionFunction, where every word a developer would type
// is hidden inside one token. Runs of capitals are kept together up to the last
// one, so PVSystemState yields pv, system, state rather than p, v, system.
func splitWords(text string) []string {
	out := []string{}
	current := []rune{}
	runes := []rune(text)

	flush := func() {
		if len(current) > 0 {
			out = append(out, string(current))
			current = nil
		}
	}

	for i, r := range runes {
		switch {
		case unicode.IsUpper(r):
			previousWasLower := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			startsWordInAcronym := i > 0 && unicode.IsUpper(runes[i-1]) &&
				i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if previousWasLower || startsWordInAcronym {
				flush()
			}
			current = append(current, unicode.ToLower(r))
		case unicode.IsLower(r) || unicode.IsDigit(r):
			current = append(current, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// fold is the comparison key: lower case already, minus a plural s. It is
// applied to both sides, so an imperfect fold ("status" to "statu") costs
// nothing as long as it is the same imperfection on each.
func fold(word string) string {
	if utf8.RuneCountInString(word) > 3 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") {
		return strings.TrimSuffix(word, "s")
	}
	return word
}

// stopwords are the words that carry no selection signal, in the two languages
// this platform's ontology and its developers use. Structural words only —
// nothing domain-bearing, because a domain word dropped here is a match that can
// never be made.
var stopwords = map[string]bool{
	"a": true, "all": true, "an": true, "and": true, "any": true, "are": true, "as": true,
	"at": true, "be": true, "by": true, "each": true, "for": true, "from": true, "get": true,
	"how": true, "in": true, "is": true, "it": true, "its": true, "me": true, "my": true,
	"of": true, "on": true, "one": true, "or": true, "our": true, "over": true, "per": true,
	"set": true, "some": true, "that": true, "the": true, "their": true, "there": true,
	"these": true, "this": true, "to": true, "up": true, "was": true, "what": true,
	"which": true, "with": true,

	"alle": true, "als": true, "am": true, "auf": true, "aus": true, "bei": true, "das": true,
	"dem": true, "den": true, "der": true, "des": true, "die": true, "ein": true, "eine": true,
	"einer": true, "für": true, "hat": true, "ich": true, "im": true, "ist": true, "mein": true,
	"mit": true, "nach": true, "sind": true, "und": true, "von": true, "vom": true, "wie": true,
	"zu": true, "zum": true, "zur": true,
}

func sortMatches[T any](items []T, key func(T) (score float64, name string, id string)) {
	sort.SliceStable(items, func(i, j int) bool {
		leftScore, leftName, leftID := key(items[i])
		rightScore, rightName, rightID := key(items[j])
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		if leftName != rightName {
			return leftName < rightName
		}
		return leftID < rightID
	})
}

// truncate keeps the strongest `limit` items and records what that cost.
//
// The record is the point. Cutting silently leaves a caller unable to tell a
// complete list from a prefix of one, and the caller here is often a model
// choosing a function to profile: five of forty measuring functions read exactly
// like all five there were. Nothing is appended when nothing was cut, so an empty
// Elided means the lists are whole.
func truncate[T any](items []T, limit int, field string, elided []Elision) ([]T, []Elision) {
	if limit <= 0 || len(items) <= limit {
		return items, elided
	}
	return items[:limit], append(elided, Elision{Field: field, Total: len(items), Shown: limit})
}

func round2(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}
