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

// A measurement against real confirmations rather than against invented cells.
// It answers what widening the vocabulary or adding a parser would actually buy,
// by relaxing one part of the gate at a time and re-running the production logic
// over a corpus of decided `run_code` confirmations.
//
// Skipped unless a corpus is named, so it costs nothing in CI:
//
//	psql "$POSTGRES_URL" -At -c "SELECT coalesce(json_agg(json_build_object( \
//	  'code', input->>'code', 'at', created_at)), '[]')::text \
//	  FROM ode_confirmations WHERE tool='run_code' AND input->>'code' IS NOT NULL" > corpus.json
//	CORPUS=corpus.json go test ./pkg/plaincode -run TestCorpusProbe -v
//
// The cells are a developer's own Python and are not committed with it.
//
// # Read the corpus against the dates below, not as one pile
//
// `ode_confirmations` keeps decided rows forever, which is what makes a corpus
// possible at all — and also means it spans every version of the gate that ever
// ran. A cell recorded before a change was judged by the older gate, so a rate
// computed over the whole table is an average of gates rather than a measurement
// of this one, and it will drift downward for a reason that has nothing to do
// with the code: every widening removes future cells from the table by letting
// them run unasked, so what accumulates afterwards is the harder residue.
//
// gateChanges below is that history. Anything that alters which cells reach a
// confirmation belongs in it, dated, at the time it lands.

package plaincode

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// gateChanges is every change to what run_code asks about, newest last. A future
// measurement splits the corpus on these dates; an unrecorded change makes the
// split silently wrong, which is the one way this file stops being usable.
var gateChanges = []struct {
	on   string // the day the change reached the developers being measured
	what string
}{
	{"2026-08-28", "auto mode shipped: recognised cells stop asking (before this, every cell asked)"},
	{"2026-08-31", "vocabulary widened by the measured refusals: re.search/.group, joinpath, np.array, .tolist, json.dumps"},
	{"2026-08-31", "the non-writing half of the to_* family: to_pandas, to_string, to_dict, to_numpy, to_frame, to_list"},
	{"2026-08-31", "contained execution (kernel_contain_cells): a cell that does not ask for the platform token runs unasked, whatever this package makes of it — so from here the rate below stops being the rate a developer experiences"},
}

// After the last entry above, this file measures something the product no longer
// asks. Where cells are contained, `Recognised` is not on the path: what a
// confirmation costs is a cell that reached for the platform, which is a property
// of the code's *effect* and is not decidable here at all. The number this probe
// reports stays meaningful for exactly two things — a deployment with
// kernel_contain_cells off, and the historical stretches above — and a comparison
// across that boundary is two different questions added together.
//
// The rate that matters after it is measured from the confirmations themselves:
//
//	SELECT count(*) FILTER (WHERE decision <> '') AS asked, count(*) AS cells
//	  FROM ode_confirmations WHERE tool = 'run_code' AND created_at > '2026-08-31';
//
// which needs no recogniser and no corpus dump, because a contained cell that ran
// unasked leaves no confirmation row to count.

// theStatements a parser would understand structurally. `from`, `global`,
// `nonlocal`, `del`, `raise`, `async` and `await` stay out: those are refused for
// what they do, not for being unparseable, so lifting them would measure a rubber
// stamp rather than a parser.
var parseable = []string{
	"def", "class", "lambda", "try", "except", "finally", "with", "while",
	"assert", "yield", "return", "pass", "break", "continue", "elif",
	"match", "case",
}

// shouldAskAnyway is the doc's own list of reasons a refusal is correct. A cell
// holding one of these is not a parser's to recover, so counting it as a gain
// would overstate the case for the dependency.
var shouldAskAnyway = []string{
	"subprocess", "importlib", "urllib", "socket", "requests", "shutil",
	"sys", "inspect", "paramiko", "smtplib",
}

func holdsHardName(code string) bool {
	for _, n := range shouldAskAnyway {
		if strings.Contains(code, n) {
			return true
		}
	}
	return openReadsOnly(code) != ""
}

// recognisedUnder runs the real gate with parts of the vocabulary relaxed, then
// restores it. Mutating the package maps keeps the measurement on the production
// logic instead of a second copy that could drift from it.
func recognisedUnder(code string, stmts, shape, attrs bool) bool {
	if stmts {
		saved := map[string]bool{}
		for _, w := range parseable {
			saved[w] = statementKeywords[w]
			delete(statementKeywords, w)
			keywords[w] = true
		}
		defer func() {
			for _, w := range parseable {
				delete(keywords, w)
				if saved[w] {
					statementKeywords[w] = true
				}
			}
		}()
	}
	if shape {
		saved := forbiddenChars
		forbiddenChars = map[rune]string{}
		defer func() { forbiddenChars = saved }()
	}
	if attrs {
		added := []string{}
		for w := range corpusAttrs {
			if !attributes[w] {
				attributes[w] = true
				added = append(added, w)
			}
		}
		defer func() {
			for _, w := range added {
				delete(attributes, w)
			}
		}()
	}
	ok, _ := Recognised(code)
	return ok
}

func TestCorpusProbe(t *testing.T) {
	path := os.Getenv("CORPUS")
	if path == "" {
		t.Skip("no CORPUS")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rowsIn []struct {
		Code string    `json:"code"`
		At   time.Time `json:"at"`
	}
	if err := json.Unmarshal(raw, &rowsIn); err != nil {
		t.Fatal(err)
	}
	cells := make([]string, 0, len(rowsIn))
	for _, r := range rowsIn {
		cells = append(cells, r.Code)
	}
	reportEras(t, rowsIn)

	type row struct {
		label               string
		stmts, shape, attrs bool
	}
	rows := []row{
		{"today", false, false, false},
		{"+ statements a parser understands", true, false, false},
		{"+ IPython shape handled", true, true, false},
		{"+ every attribute allowed", true, true, true},
		{"attributes only, no parser", false, false, true},
	}

	collectAttrs(cells)
	t.Logf("corpus: %d cells, %d distinct attribute names", len(cells), len(corpusAttrs))
	hard := 0
	for _, c := range cells {
		if holdsHardName(c) {
			hard++
		}
	}
	t.Logf("of those, %d hold a name the doc says should ask (subprocess, sys, importlib, urllib, inspect, socket, or an open in write mode)", hard)

	for _, r := range rows {
		total, gainSoft := 0, 0
		for _, c := range cells {
			if recognisedUnder(c, r.stmts, r.shape, r.attrs) {
				total++
				if !holdsHardName(c) {
					gainSoft++
				}
			}
		}
		t.Logf("%-36s recognised %3d/%d (%4.1f%%)  of which free of a hard name: %d",
			r.label, total, len(cells), 100*float64(total)/float64(len(cells)), gainSoft)
	}

	// The decisive split: among cells refused today, what is the first thing that
	// stopped them?
	byReason := map[string]int{}
	for _, c := range cells {
		ok, why := Recognised(c)
		if ok {
			continue
		}
		switch {
		case strings.Contains(why, "not one of the attributes"):
			byReason["unknown attribute"]++
		case strings.Contains(why, "a statement this subset does not include"):
			byReason["statement (def, class, try, with, ...)"]++
		case strings.Contains(why, "will not treat as a variable"):
			byReason["a name that executes or reaches the environment"]++
		case strings.Contains(why, "not a name this check knows"):
			byReason["unknown bare name"]++
		case strings.Contains(why, "object model"):
			byReason["dunder"]++
		default:
			byReason["shape: "+why]++
		}
	}
	t.Logf("first blocker among cells refused today:")
	for k, v := range byReason {
		t.Logf("    %-46s %d", k, v)
	}
}

// reportEras splits the corpus on gateChanges and reports the recognition rate
// within each stretch, because a rate over the whole table averages gates that no
// longer exist. A stretch with few cells in it carries no conclusion — say so
// rather than reporting a percentage of four.
func reportEras(t *testing.T, rows []struct {
	Code string    `json:"code"`
	At   time.Time `json:"at"`
}) {
	type era struct {
		from, to string
		change   string
	}
	dates := make([]string, 0, len(gateChanges))
	for _, c := range gateChanges {
		dates = append(dates, c.on)
	}
	sort.Strings(dates)

	bounds := append([]string{""}, dates...)
	t.Logf("the corpus spans %d gate changes; each stretch below was judged by a different gate:", len(gateChanges))
	for i, from := range bounds {
		to := ""
		if i < len(bounds)-1 {
			to = bounds[i+1]
		}
		n, ok := 0, 0
		for _, r := range rows {
			day := r.At.UTC().Format("2006-01-02")
			if day < from {
				continue
			}
			if to != "" && day >= to {
				continue
			}
			n++
			if yes, _ := Recognised(r.Code); yes {
				ok++
			}
		}
		if n == 0 {
			continue
		}
		label := "from " + from
		if from == "" {
			label = "before " + to
		}
		if to == "" {
			label = "since " + from
		}
		note := ""
		if n < 20 {
			note = "  (too few to conclude from)"
		}
		t.Logf("    %-22s %3d cells, recognised by today's gate: %3d (%4.1f%%)%s",
			label, n, ok, 100*float64(ok)/float64(n), note)
	}
	for _, c := range gateChanges {
		t.Logf("    %s  %s", c.on, c.what)
	}
}

// corpusAttrs is every name the corpus reaches through a dot. Adding all of them
// to the vocabulary gives the ceiling: what recognition would be if the attribute
// list were never the binding constraint.
var corpusAttrs = map[string]bool{}

func collectAttrs(cells []string) {
	for _, code := range cells {
		stripped, err := stripLiterals(code)
		if err != "" {
			continue
		}
		runes := []rune(stripped)
		for i := 0; i < len(runes); i++ {
			if !isNameStart(runes[i]) {
				continue
			}
			start := i
			for i < len(runes) && isNameRune(runes[i]) {
				i++
			}
			word := string(runes[start:i])
			i--
			if precededByDot(runes, start) && !strings.HasPrefix(word, "__") {
				corpusAttrs[word] = true
			}
		}
	}
}
