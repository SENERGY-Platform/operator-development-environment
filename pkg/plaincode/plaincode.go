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

// Package plaincode recognises a small, dull subset of Python.
//
// # This is not a security boundary, and must never be described as one
//
// It cannot be. Python has no sound static answer to "is this safe": a denylist
// is walked around with `getattr(__builtins__, "ev" + "al")`, `__import__`, or a
// name rebound three cells earlier; and the party writing the code is the model,
// which is the same party the confirmation exists to check. Anything here that
// looks like a safety argument is a mistake in the reading.
//
// What it is: relief from being asked the same question about `df.head()` forty
// times in an afternoon. It answers one question — "is this recognisably an
// inspection of data already in the kernel?" — and answers it by *recognising*
// rather than by detecting danger. Every construct it does not positively know is
// unrecognised, so the failure it produces is an unnecessary confirmation prompt.
// That direction is deliberate and load-bearing: a recogniser that errs the other
// way would be the security boundary this one refuses to be.
//
// The real boundary is unchanged and is stated in pkg/kernel: the code runs in the
// developer's own JupyterHub pod under the developer's own platform token, and it
// can reach exactly what they can reach. Auto mode changes who is asked, never
// what is possible.
//
// # Why not an AST
//
// Go has no Python parser in its standard library. The three ways to get a real
// AST were a third-party parser (a dependency and a large surface for a
// convenience feature), parsing inside the kernel with `ast.parse` (sound in that
// it never executes the payload, but it couples the gate to a live kernel and to a
// namespace that earlier approved code could have poisoned), and this: a token
// scan that knows a fixed vocabulary. The token scan recognises strictly less than
// an AST would, which — given the direction above — is the acceptable end to be
// wrong at.
package plaincode

import (
	"strings"
	"unicode"
)

// Recognised reports whether code is in the dull subset, and why it is not.
//
// The reason is for the developer and for the audit line, not for the model: it
// names the construct that stopped it, so "why was I asked again" has an answer.
func Recognised(code string) (bool, string) {
	// Both of these read the source before the literals are blanked, because both
	// are questions about what is inside one: `open`'s mode, and the handful of
	// paths whose contents are credentials.
	if reason := openReadsOnly(code); reason != "" {
		return false, reason
	}
	if reason := noCredentialPaths(code); reason != "" {
		return false, reason
	}
	stripped, err := stripLiterals(code)
	if err != "" {
		return false, err
	}
	if reason := scanCharacters(stripped); reason != "" {
		return false, reason
	}
	if reason := scanWords(stripped); reason != "" {
		return false, reason
	}
	if strings.TrimSpace(stripped) == "" {
		return false, "there is nothing to run"
	}
	return true, ""
}

/*
openReadsOnly refuses an `open` that is not opening a file to read it.

`open` is in the vocabulary because reading a file in the developer's own pod is
what they do all day — the cell that reads `cycles.py` out of the checkout and
prints a function from it is inspection, and being asked about it forty times is
the thing auto mode exists to stop. Writing is a different act, and the only
thing that distinguishes them is the mode argument.

Which is why this runs on the raw source. stripLiterals blanks the contents of
every literal, and the mode *is* a literal: after the strip, `open(p, "w")` and
`open(p)` are the same three tokens.

A mode that is not a literal — `open(p, mode)` — is refused rather than read. So
is `+`, which makes a read handle a write handle.
*/
func openReadsOnly(code string) string {
	runes := []rune(code)
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
		if word != "open" || precededByDot(runes, start) {
			continue
		}

		open := i + 1
		for open < len(runes) && (runes[open] == ' ' || runes[open] == '\t') {
			open++
		}
		if open >= len(runes) || runes[open] != '(' {
			// A bare `open` that calls nothing is a reference to the builtin, and
			// nothing in this subset has a use for one.
			return "`open` used as a value rather than to read a file"
		}
		mode, found, reason := secondArgument(runes, open+1)
		if reason != "" {
			return reason
		}
		if found && !readOnlyMode(mode) {
			return "`open` with a mode that is not reading"
		}
	}
	return ""
}

// secondArgument returns the second argument of a call whose parentheses start at
// `at`, as a string literal's text.
//
// Reports whether there was one at all, and refuses anything it cannot read as a
// plain literal — an expression, a name, a concatenation.
func secondArgument(runes []rune, at int) (string, bool, string) {
	depth := 0
	for i := at; i < len(runes); i++ {
		switch runes[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				// The call ended with one argument.
				return "", false, ""
			}
			depth--
		case '\'', '"':
			end, reason := skipLiteral(runes, i, runes[i])
			if reason != "" {
				return "", false, reason
			}
			i = end
		case ',':
			if depth != 0 {
				continue
			}
			// The mode, which has to be a literal this can read.
			j := i + 1
			for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t' || runes[j] == '\n') {
				j++
			}
			if j >= len(runes) || (runes[j] != '\'' && runes[j] != '"') {
				return "", false, "`open` with a mode this check cannot read"
			}
			quote := runes[j]
			end, reason := skipLiteral(runes, j, quote)
			if reason != "" {
				return "", false, reason
			}
			return string(runes[j+1 : end]), true, ""
		}
	}
	return "", false, "an unclosed call to `open`"
}

// readOnlyMode reports whether a mode string opens a file for reading and nothing
// else. `+` counts as writing, because it is what turns a read handle into one.
func readOnlyMode(mode string) bool {
	if mode == "" {
		return false
	}
	for _, c := range mode {
		if c != 'r' && c != 'b' && c != 't' {
			return false
		}
	}
	return strings.ContainsRune(mode, 'r')
}

/*
noCredentialPaths refuses a cell that names one of the few paths whose contents
are a credential.

This is a floor, not a boundary, and it is the same kind of floor as
neverVariables: it catches the literal, and a path assembled at runtime walks
straight past it. What makes that acceptable is what auto mode is — relief from
being asked, inside the developer's own pod, where the boundary is their token and
not this function. What makes it worth having anyway is the asymmetry of the
mistake: reading a source file into the model's context is the developer's own
work, and reading their SSH key into it is a credential leaving the pod, from a
cell nobody was asked about.

Deliberately short and specific. `token`, `secret` and `password` as substrings
would refuse `tokenizer.py` and `secrets_test.py`, which are ordinary files in an
operator repository.
*/
func noCredentialPaths(code string) string {
	for _, literal := range literals(code) {
		lower := strings.ToLower(literal)
		if strings.Contains(lower, secretsMount) {
			return "`" + secretsMount + "`, which is where a pod's credentials are mounted"
		}
		// By path component, not by substring: `.env` inside `os.environ` is not a
		// file, and refusing it there would report a credential where there is none.
		for _, component := range strings.Split(lower, "/") {
			if credentialNames[component] {
				return "`" + component + "`, whose contents are a credential"
			}
		}
	}
	return ""
}

// literals is the text inside every string literal in the source, unread by the
// scans that follow — this is the one check that has to look inside one.
func literals(code string) []string {
	runes := []rune(code)
	out := []string{}
	for i := 0; i < len(runes); i++ {
		if runes[i] == '#' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		if runes[i] != '\'' && runes[i] != '"' {
			continue
		}
		quote := runes[i]
		end, reason := skipLiteral(runes, i, quote)
		if reason != "" {
			// Unterminated, which stripLiterals refuses for itself in a moment.
			return out
		}
		width := 1
		if end-i >= 5 && runes[i+1] == quote && runes[i+2] == quote {
			width = 3
		}
		if i+width <= end-width+1 {
			out = append(out, string(runes[i+width:end-width+1]))
		}
		i = end
	}
	return out
}

const secretsMount = "/var/run/secrets"

var credentialNames = map[string]bool{
	".ssh": true, ".aws": true, ".kube": true, ".gnupg": true,
	".netrc": true, ".pgpass": true, ".git-credentials": true, ".env": true,
	"id_rsa": true, "id_ed25519": true, "id_ecdsa": true, "shadow": true,
	"credentials": true, "authorized_keys": true,
}

/*
CredentialPath reports whether a path names a file whose contents are a
credential, and which component said so.

The same knowledge as noCredentialPaths above, for a caller that has a path rather
than a cell: `read_file` returns a file of the working copy straight into a
conversation that is persisted, and it does it without asking anyone. A repository
with a `.env` in it is ordinary, and so is a model that reads every file it can
name while it works out what the operator does.

It is exported here rather than copied into pkg/tools because the list is the
decision — which names are worth refusing, and which ordinary files a substring
match would wreck — and two copies of it would drift. Same standing as everything
else in this package: a floor, not a boundary. It reads the path it was given, and
a credential a repository keeps under another name walks past it.
*/
func CredentialPath(p string) (string, bool) {
	lower := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	if strings.Contains(lower, secretsMount) {
		return secretsMount, true
	}
	for _, component := range strings.Split(lower, "/") {
		if credentialNames[component] {
			return component, true
		}
	}
	return "", false
}

/*
stripLiterals replaces the contents of string literals with spaces.

Two reasons, and the second is the one that matters. Keywords inside a string are
data — `print("import os")` names no import — so leaving them in would reject
harmless code. And a string is the one place a forbidden word can hide from a word
scan, so blanking the contents rather than skipping the scan is what stops
`eval("...")`'s argument from mattering while `eval` itself still does.

Deliberately strict about what it understands. An f-string can contain arbitrary
expressions between braces, and this does not parse them, so any f-string is
refused rather than half-read. Same for a line continuation, which would let one
statement wear the shape of two.
*/
func stripLiterals(code string) (string, string) {
	// A rune slice rather than a Builder: the f-string branch below has to take
	// back the prefix letter it has already written.
	out := make([]rune, 0, len(code))

	runes := []rune(code)
	for i := 0; i < len(runes); i++ {
		c := runes[i]

		// A comment runs to the end of the line and is blanked with it.
		if c == '#' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			if i < len(runes) {
				out = append(out, '\n')
			}
			continue
		}

		if c != '"' && c != '\'' {
			out = append(out, c)
			continue
		}

		prefix := literalPrefix(runes, i)
		// An f-string is read rather than refused, because two thirds of the
		// confirmations auto mode was meant to spare a developer were f-strings:
		// `print(f"{df.shape}")` is the same inspection as `print(df.shape)` and was
		// being asked about because of the quotes around it.
		//
		// What makes that safe to do is that the fields are put back into the scan
		// rather than trusted — see readFString. The text between them stays data,
		// as in any other literal.
		if isFString(prefix) {
			// The prefix letters are in the output already, written as a bare name by
			// the loop above. An f-string is not a name, so they come back off.
			out = out[:len(out)-len(prefix)]
			fields, next, reason := readFString(runes, i)
			if reason != "" {
				return "", reason
			}
			out = append(out, fields...)
			i = next
			continue
		}
		// Any other prefixed literal whose prefix says the contents are more than
		// text. Refused rather than enumerated, because a prefix this scan does not
		// know is a literal it cannot claim to have read.
		if prefix != "" {
			return "", "an " + prefix + "-string, whose contents this check does not read"
		}

		quote := c
		triple := i+2 < len(runes) && runes[i+1] == quote && runes[i+2] == quote
		width := 1
		if triple {
			width = 3
		}
		i += width

		closed := false
		for i < len(runes) {
			if runes[i] == '\\' {
				// A backslash escape is skipped whole, so `"\""` does not end here.
				i += 2
				continue
			}
			if runes[i] == quote {
				if !triple {
					closed = true
					break
				}
				if i+2 < len(runes) && runes[i+1] == quote && runes[i+2] == quote {
					i += 2
					closed = true
					break
				}
			}
			// Newlines inside a triple-quoted literal are kept, so line-based
			// reasoning downstream still sees the right number of lines.
			if runes[i] == '\n' {
				out = append(out, '\n')
			}
			i++
		}
		if !closed {
			return "", "an unterminated string literal"
		}
		// The literal becomes a bare pair of quotes: an expression to the scans
		// below, with nothing inside for them to read.
		out = append(out, '"', '"')
	}
	return string(out), ""
}

// isFString reports whether a literal prefix is one whose braces are evaluated.
//
// `r` rides along because raw only changes what a backslash means, and a backslash
// inside a field is refused below either way.
func isFString(prefix string) bool {
	return prefix == "f" || prefix == "rf" || prefix == "fr"
}

/*
readFString reads an f-string, keeping its fields and dropping its text.

The output is what the scans downstream should judge: the text becomes an empty
literal, and every `{...}` becomes a parenthesised expression in the stream of
code. `print(f"rows: {df.shape[0]}")` therefore reads as `print("" (df.shape[0]) )`
and stands or falls on `shape` being an attribute this package knows — exactly as
`print(df.shape[0])` does.

That is the whole idea: an f-string is as dull as what it interpolates, and this
package is not entitled to an opinion beyond that. Anything about the syntax it
cannot follow — a nested f-string, a backslash, a field left open — is refused
rather than half-read, which is the same direction as everything else here.

Returns the index of the literal's final quote.
*/
func readFString(runes []rune, at int) ([]rune, int, string) {
	quote := runes[at]
	width := 1
	if at+2 < len(runes) && runes[at+1] == quote && runes[at+2] == quote {
		width = 3
	}

	// The text of the literal, as data. The fields are appended after it.
	out := []rune{'"', '"'}

	for i := at + width; i < len(runes); i++ {
		c := runes[i]

		if c == quote {
			if width == 1 {
				return out, i, ""
			}
			if i+2 < len(runes) && runes[i+1] == quote && runes[i+2] == quote {
				return out, i + 2, ""
			}
		}
		switch {
		case c == '\\':
			// Two characters of text, whatever they are.
			i++
		case c == '{' && i+1 < len(runes) && runes[i+1] == '{':
			// An escaped brace is a brace in the output text, not a field.
			i++
		case c == '}' && i+1 < len(runes) && runes[i+1] == '}':
			i++
		case c == '}':
			return nil, 0, "a `}` in an f-string with no field to close"
		case c == '{':
			field, next, reason := readField(runes, i+1, quote)
			if reason != "" {
				return nil, 0, reason
			}
			out = append(out, '(')
			out = append(out, field...)
			out = append(out, ')', ' ')
			i = next
		case c == '\n':
			// Kept for the line-based checks, as in a plain triple-quoted literal.
			out = append(out, '\n')
		}
	}
	return nil, 0, "an unterminated string literal"
}

/*
readField reads one `{...}` of an f-string, from just after the brace.

Returns the expression as code and the index of the closing brace. What is left out
is what Python does not evaluate: the conversion (`!r`), the format spec after `:`,
and the contents of any string literal inside the field.

Two characters get replaced rather than copied, because scanCharacters forbids them
outright and they are operators here rather than the shell escapes it is looking
for: the `!` of `!=`, and the `=` of the `{x=}` debug form. Dropping an operator
costs the scans nothing — they read names, not syntax.
*/
func readField(runes []rune, at int, outer rune) ([]rune, int, string) {
	out := []rune{}
	depth := 0

	for i := at; i < len(runes); i++ {
		c := runes[i]

		switch {
		case c == '\\':
			// A backslash in a field is a syntax error before Python 3.12 and an
			// escape this scan does not follow after it.
			return nil, 0, "a backslash in an f-string field"
		case c == '\n':
			return nil, 0, "a line break in an f-string field"
		case c == '(' || c == '[':
			depth++
			out = append(out, c)
		case c == ')' || c == ']':
			depth--
			if depth < 0 {
				return nil, 0, "an unbalanced bracket in an f-string field"
			}
			out = append(out, c)
		case c == '{':
			// A dict or set literal inside the field. Its own braces are not the end
			// of the field, and its `:` is not a format spec.
			depth++
			out = append(out, c)
		case c == '}':
			if depth == 0 {
				return out, i, ""
			}
			depth--
			out = append(out, c)
		case c == '\'' || c == '"':
			if c == outer {
				// Legal from Python 3.12, and this scan does not track which quote
				// belongs to which literal well enough to claim it read it.
				return nil, 0, "a field quoted with the f-string's own quote"
			}
			// A nested f-string is evaluated, so blanking it would hide the one thing
			// worth reading. Refused rather than read, because its fields would need
			// this function to be re-entrant about the outer quote.
			if prefix := literalPrefix(runes, i); prefix != "" {
				return nil, 0, "an " + prefix + "-string inside an f-string field"
			}
			end, reason := skipLiteral(runes, i, c)
			if reason != "" {
				return nil, 0, reason
			}
			out = append(out, '"', '"')
			i = end
		case c == '!':
			// `!=` is a comparison; anything else at the top level of a field is the
			// conversion, which Python applies to the value rather than evaluating.
			if i+1 < len(runes) && runes[i+1] == '=' {
				out = append(out, ' ', ' ')
				i++
				continue
			}
			if depth != 0 {
				return nil, 0, "a `!` inside an f-string field"
			}
			if i+1 >= len(runes) || !strings.ContainsRune("rsa", runes[i+1]) {
				return nil, 0, "an f-string conversion this check does not know"
			}
			i++
		case c == ':' && depth == 0:
			// The format spec is data — but a spec may itself contain a field, and
			// this does not read one inside another.
			for i++; i < len(runes); i++ {
				if runes[i] == '}' {
					return out, i, ""
				}
				if runes[i] == '{' {
					return nil, 0, "a field inside an f-string format spec"
				}
			}
			return nil, 0, "an unterminated f-string field"
		case c == '=':
			// The debug form `{x=}`, or part of `==`, `<=`, `>=`. Either way not a
			// name, and an assignment is not possible here.
			out = append(out, ' ')
		default:
			out = append(out, c)
		}
	}
	return nil, 0, "an unterminated f-string field"
}

// skipLiteral finds the end of a plain string literal starting at its opening
// quote, and reports the index of the last quote character.
func skipLiteral(runes []rune, at int, quote rune) (int, string) {
	width := 1
	if at+2 < len(runes) && runes[at+1] == quote && runes[at+2] == quote {
		width = 3
	}
	for i := at + width; i < len(runes); i++ {
		if runes[i] == '\\' {
			i++
			continue
		}
		if runes[i] != quote {
			continue
		}
		if width == 1 {
			return i, ""
		}
		if i+2 < len(runes) && runes[i+1] == quote && runes[i+2] == quote {
			return i + 2, ""
		}
	}
	return 0, "an unterminated string literal"
}

// literalPrefix names the prefix on a string literal starting at `at`, or "" for a
// plain one. Only the letters immediately before the quote count.
func literalPrefix(runes []rune, at int) string {
	end := at
	start := at
	for start > 0 && isNameRune(runes[start-1]) {
		start--
	}
	if start == end {
		return ""
	}
	prefix := strings.ToLower(string(runes[start:end]))
	// `b` and `r` are bytes and raw: still just text. Anything else — `f`, or a
	// name this scan does not know — is refused.
	if prefix == "b" || prefix == "r" || prefix == "rb" || prefix == "br" {
		return ""
	}
	return prefix
}

// forbidden characters, each with what it would let through.
var forbiddenChars = map[rune]string{
	// `;` is deliberately absent. It joins two statements, and both of them are
	// scanned here anyway — the vocabulary is what decides, so a semicolon adds
	// nothing a scan of the whole text does not already see. Refusing it cost a
	// prompt on `import pandas as pd; df = load()`, which is a shape a developer
	// types without thinking.
	'\\': "a line continuation",
	'@':  "a decorator",
	'!':  "a shell escape",
	'?':  "an IPython help or magic",
	'`':  "a backtick",
	'$':  "a shell substitution",
}

func scanCharacters(code string) string {
	for _, c := range code {
		if why, bad := forbiddenChars[c]; bad {
			return why
		}
		// Anything outside ASCII. A homoglyph is not a threat here, but it is a
		// name this scan cannot compare against its vocabulary, so it is not
		// recognised.
		if c > unicode.MaxASCII {
			return "a character outside ASCII"
		}
	}
	// A magic on its own line — `%timeit`, `%%bash` — reads as a modulo to a token
	// scan, so it is caught by shape instead.
	for _, line := range strings.Split(code, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "%") {
			return "an IPython magic"
		}
	}
	return ""
}

/*
scanWords requires every identifier to be one this package knows.

The whole gate is here. A name that is not in one of the three sets below stops
recognition, so the sets are the vocabulary — not a list of things to avoid, but
the complete list of things this subset may say. Adding to them widens what runs
without being asked, which is why each one is short and each addition is a
decision.
*/
func scanWords(code string) string {
	runes := []rune(code)
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

		// A dunder is how the object model is reached from inside an expression, and
		// nothing in this subset needs one.
		if strings.HasPrefix(word, "__") {
			return "`" + word + "`, which reaches into the object model"
		}

		// Preceded by a dot, it is an attribute or a method: a different vocabulary
		// from a bare name, because `df.mean` is dull and `mean` alone is a name
		// nobody bound.
		if precededByDot(runes, start) {
			if !attributes[word] {
				return "`." + word + "`, which is not one of the attributes this check knows"
			}
			continue
		}

		if keywords[word] {
			continue
		}
		if names[word] {
			continue
		}
		if statementKeywords[word] {
			return "`" + word + "`, which is a statement this subset does not include"
		}
		// A name the subset will not read as a variable, however it is spelled. The
		// hole below admits any bare lower-case name on the grounds that it was
		// probably bound by earlier approved code; for these, the overwhelmingly
		// likely binding is the builtin or the module of the same name, and treating
		// `eval` as somebody's variable is not a risk worth the convenience.
		if neverVariables[word] {
			return "`" + word + "`, which this subset will not treat as a variable"
		}
		// A bare name that is not a builtin is a variable — bound earlier in the
		// kernel, by code the developer did approve. Allowed as a *reference*: it can
		// be read and called, and what it is bound to is the developer's own doing.
		if looksLikeVariable(word) {
			continue
		}
		return "`" + word + "`, which is not a name this check knows"
	}
	return ""
}

func precededByDot(runes []rune, start int) bool {
	for i := start - 1; i >= 0; i-- {
		if runes[i] == ' ' || runes[i] == '\t' {
			continue
		}
		return runes[i] == '.'
	}
	return false
}

// looksLikeVariable is the one deliberate hole, and it is bounded on purpose.
//
// A bare lower-case name is taken to be a variable the developer's own earlier,
// confirmed code bound — `df`, `readings`, `model`. Refusing those would leave the
// subset able to express almost nothing, since every useful inspection starts from
// something already in the kernel.
//
// What it does not admit is the shape a smuggled builtin needs: a leading
// underscore, or any capital, both of which are how `__builtins__` and the
// exception and type names are spelled. It is a convention, not a guarantee, which
// is the whole standing of this package.
func looksLikeVariable(word string) bool {
	if word == "" || word[0] == '_' {
		return false
	}
	// SCREAMING_SNAKE_CASE is a constant the developer's own code bound — `WS` for
	// the workspace, `LIB` for a library path — and the reason capitals were
	// refused does not reach it: what the rule is keeping out is `Exception`,
	// `DataFrame`, `__builtins__`, and none of those is spelled in upper case
	// throughout. A name with no lower-case letter in it is therefore admitted, and
	// CamelCase still is not.
	if isConstantName(word) {
		return true
	}
	for _, c := range word {
		if unicode.IsUpper(c) {
			return false
		}
	}
	return true
}

// isConstantName reports the shape of a module-level constant: upper case,
// digits and underscores, with at least one letter.
func isConstantName(word string) bool {
	letters := false
	for _, c := range word {
		switch {
		case unicode.IsUpper(c):
			letters = true
		case unicode.IsDigit(c) || c == '_':
		default:
			return false
		}
	}
	return letters
}

func isNameStart(c rune) bool { return c == '_' || unicode.IsLetter(c) }
func isNameRune(c rune) bool  { return c == '_' || unicode.IsLetter(c) || unicode.IsDigit(c) }

// keywords this subset allows: the ones that appear inside an expression, and
// nothing that begins a statement.
var keywords = map[string]bool{
	"and": true, "or": true, "not": true, "in": true, "is": true,
	"if": true, "else": true, "for": true, "None": true, "True": true, "False": true,
}

// statementKeywords are named separately so refusing one can say what it is,
// rather than reporting a keyword as an unknown name.
//
// `import` is not among them, and `from` is. The asymmetry is the point: `import
// socket` binds the name `socket`, and every use of it is an attribute access this
// package still gates — `socket.create_connection` is refused because
// `create_connection` is not an attribute it knows. `from socket import
// create_connection` binds a *bare* lower-case name instead, and the variable hole
// below waves those through on the grounds that the developer's own code bound
// them. So the dotted form is gated by the vocabulary and the from-form is not,
// which is why one is recognised and the other is not.
var statementKeywords = map[string]bool{
	"from": true, "def": true, "class": true, "lambda": true,
	"global": true, "nonlocal": true, "del": true, "try": true, "except": true,
	"finally": true, "raise": true, "with": true, "while": true, "assert": true,
	"yield": true, "return": true, "pass": true, "break": true, "continue": true,
	"async": true, "await": true, "elif": true, "match": true, "case": true,
}

// names are the bare callables this subset may use: pure, and none of them a way
// out. `open`, `eval`, `exec`, `compile`, `input`, `getattr`, `vars`, `globals`
// and `__import__` are absent, and their absence is the point of the list.
var names = map[string]bool{
	"abs": true, "all": true, "any": true, "bool": true, "dict": true,
	"divmod": true, "enumerate": true, "filter": true, "float": true, "int": true,
	"len": true, "list": true, "map": true, "max": true, "min": true,
	"print": true, "range": true, "repr": true, "reversed": true, "round": true,
	"set": true, "sorted": true, "str": true, "sum": true, "tuple": true,
	"zip": true,
	// Reading a file is inspection. `open` is admitted only for reading, which is
	// checked before the literals are blanked — see openReadsOnly — and `Path`
	// builds a path object whose useful members are all in the attribute list
	// while `write_text`, `unlink` and `rmdir` are not.
	"open": true, "Path": true,
}

/*
neverVariables are the bare names the variable hole must not swallow.

Two kinds, and both are here because `looksLikeVariable` would otherwise wave them
through: the builtins that are a way out of the process, and the standard-library
modules that are the same thing one attribute later. Nothing in the dull subset
needs to name any of them, so refusing them costs a prompt on the rare cell that
really did bind a variable called `id`.

This is the one list in the package that reads like a denylist, and it is not doing
the work a denylist would be expected to do: `eval` obfuscated as
`getattr(__builtins__, "ev" + "al")` is refused by the dunder rule and the unknown
attribute, not by this. It is a floor under the hole above, no more.
*/
var neverVariables = map[string]bool{
	// builtins that execute, read or reach into the object model
	"eval": true, "exec": true, "compile": true, "input": true,
	"globals": true, "locals": true, "vars": true, "dir": true, "help": true,
	"getattr": true, "setattr": true, "delattr": true, "hasattr": true,
	"breakpoint": true, "exit": true, "quit": true, "id": true, "memoryview": true,
	"super": true, "object": true, "type": true, "iter": true, "next": true,
	"callable": true, "staticmethod": true, "classmethod": true, "property": true,
	// the environment, which is where the credentials are: reading a file in the
	// developer's own pod is what they do all day, and reading `os.environ` into
	// the model's context is not the same act at all.
	"environ": true, "getenv": true, "putenv": true,
	// modules that are a way out, whether or not they happen to be imported.
	//
	// `os` and `pathlib` are not here any more. They were belt-and-braces — the
	// real gate is the attribute list, which admits `os.path.join` and refuses
	// `os.system`, `os.remove` and `os.environ` because those are not members it
	// knows. Keeping the modules out as well only refused the path arithmetic that
	// every read of a repository file is written with.
	"sys": true, "subprocess": true, "shutil": true, "socket": true,
	"pickle": true, "marshal": true, "ctypes": true, "importlib": true,
	"builtins": true, "requests": true, "urllib": true, "httpx": true,
	"glob": true, "tempfile": true, "signal": true,
	"threading": true, "multiprocessing": true, "asyncio": true, "shlex": true,
	"platform": true, "runpy": true, "code": true, "codeop": true, "gc": true,
	"inspect": true, "traceback": true, "atexit": true, "webbrowser": true,
	"ftplib": true, "smtplib": true, "telnetlib": true, "paramiko": true,
}

// attributes are the members this subset may reach through a dot.
//
// Read-only inspection of a dataframe, a series, an array or a container. Nothing
// that writes — `to_csv`, `to_sql`, `save`, `write` are absent — and nothing that
// runs something else. A method that is merely unlisted is not thereby dangerous;
// it is merely not recognised, and asking about it costs one prompt.
var attributes = map[string]bool{
	// shape and schema
	"shape": true, "columns": true, "index": true, "dtypes": true, "dtype": true,
	"size": true, "ndim": true, "empty": true, "name": true, "names": true,
	"values": true, "axes": true, "T": true,
	// looking at some of it
	"head": true, "tail": true, "sample": true, "info": true, "describe": true,
	"nunique": true, "unique": true, "value_counts": true, "count": true,
	"first": true, "last": true,
	// arithmetic over it
	"sum": true, "mean": true, "median": true, "min": true, "max": true,
	"std": true, "var": true, "quantile": true, "corr": true, "cov": true,
	"abs": true, "round": true, "cumsum": true, "diff": true, "pct_change": true,
	// selecting from it
	"loc": true, "iloc": true, "at": true, "iat": true, "isin": true,
	"between": true, "notna": true, "notnull": true, "isna": true, "isnull": true,
	"dropna": true, "sort_values": true, "sort_index": true, "groupby": true,
	"reset_index": true, "set_index": true, "astype": true, "copy": true,
	// containers and text
	"keys": true, "items": true, "get": true, "strip": true, "lower": true,
	"upper": true, "split": true, "join": true, "startswith": true,
	"endswith": true, "format": true, "replace": true, "splitlines": true,
	"find": true, "rfind": true,
	// reading a file, and the path arithmetic every read is written with. Nothing
	// that writes, removes or lists a directory tree: `write_text`, `unlink`,
	// `rmdir`, `mkdir`, `walk` and `system` are all absent, and absent is what
	// refuses them.
	"read": true, "readline": true, "readlines": true, "read_text": true,
	"path": true, "basename": true, "dirname": true, "exists": true,
	"isfile": true, "isdir": true, "splitext": true, "abspath": true,
	"stem": true, "suffix": true, "parent": true, "is_file": true,
	"is_dir": true, "close": true, "Path": true,
	// matching over text that is already in the kernel.
	//
	// A cell that reads a source file and pulls one function out of it with
	// `re.search` is doing what the `.find` three lines above it does, and it was
	// the largest single group of refusals among cells that did nothing but read.
	// The flags are attributes too — `re.S|re.M` — and a name that is only ever
	// read is the least this list can be wrong about.
	"search": true, "match": true, "fullmatch": true, "findall": true,
	"finditer": true, "group": true, "groups": true, "groupdict": true,
	"span": true, "escape": true,
	// `start` and `end` are a match's offsets, and they are the two additions here
	// with a name generic enough to belong to something else — an object bound by an
	// earlier cell whose `.start()` launches something rather than reporting an
	// index. That shape is already reachable: a bare `runner.start` is spelled the
	// way `m.start` is, the modules that hand out a startable object are refused by
	// name, and the variable hole above admits `runner()` regardless. So these cost
	// nothing that was being held, and a cell that finds a function in a file and
	// prints it from that offset is the commonest read in the corpus.
	"start": true, "end": true,
	"S": true, "M": true, "I": true, "A": true, "X": true,
	"DOTALL": true, "MULTILINE": true, "IGNORECASE": true, "VERBOSE": true,
	// more of the path arithmetic. `joinpath` is the `/` operator spelled out, and
	// it was refused more often than any other attribute in a read-only cell.
	// `glob`, `rglob`, `iterdir` and `walk` stay absent, and stay absent on purpose:
	// enumerating a tree is a different question from reading a file that was named.
	"joinpath": true, "relative_to": true, "parts": true, "as_posix": true,
	"getmtime": true, "getsize": true,
	// arrays and frames: the members that compute a value and hand it back. Nothing
	// here persists anything — `savez`, `savez_compressed`, `to_csv` and `to_sql`
	// are absent, and absent is what refuses them.
	"array": true, "ndarray": true, "arange": true, "reshape": true,
	"flatten": true, "tolist": true, "to_datetime": true, "int64": true,
	"float64": true, "all": true, "any": true,
	// containers the cell built itself. Appending to a list or updating a dict
	// writes to the kernel's own memory and reaches nothing outside it, which is
	// the line this list draws: the writes it refuses are the ones that reach a
	// file, a service or a store.
	"append": true, "extend": true, "sort": true, "update": true,
	// formatting and digesting, all pure — a string in, a string out. `dumps` and
	// `loads` are json's and neither of them writes anywhere; the digests are how a
	// cell says two files are the same file without printing either.
	"strftime": true, "isoformat": true, "encode": true, "decode": true,
	"dumps": true, "loads": true, "sha256": true, "sha1": true, "md5": true,
	"hexdigest": true,
	// time
	"year": true, "month": true, "day": true, "hour": true, "minute": true,
	"second": true, "date": true, "time": true, "total_seconds": true, "dt": true,
	"str": true,
}
