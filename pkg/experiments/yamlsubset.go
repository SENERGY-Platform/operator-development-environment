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

package experiments

import (
	"fmt"
	"strings"
)

// A reader for the subset of YAML `evaluation.yaml` is written in.
//
// Why a subset rather than a YAML library: this repository takes no dependency it
// does not need, and the whole of YAML is a large surface — anchors, aliases,
// merge keys, tags, multiple documents — none of which appears in the file this
// reads, and some of which is a liability when the input comes from a
// repository ODE does not own.
//
// What makes that honest rather than a shortcut is the failure mode. This reader
// refuses what it does not understand instead of guessing, and the caller turns a
// refusal into `not_computed` with `criteria_unparseable` and the line that
// stopped it (§5.4.6, D24). A developer whose file is outside the subset is told
// so and can simplify it; nothing is ever graded against a document that was
// half-read. The alternative — a permissive reader that silently produced a
// partial document — would grade a run against criteria the developer did not
// write, which is the one outcome §5.8 exists to prevent.
//
// The subset is: block mappings, block sequences, plain and quoted scalars, block
// scalars (`|` and `>`), empty flow collections (`[]`, `{}`) and flow sequences of
// scalars (`[a, b]`). Comments are stripped. Tabs are refused where YAML refuses
// them. Anchors, aliases, tags, nested flow collections, complex keys and multiple
// documents are all refused by name.

// Bounds on an input from a repository ODE does not own. Neither is reachable by
// a plausible evaluation.yaml — the scaffold's is twenty lines — so hitting one is
// itself a reason to refuse rather than to read on.
const (
	maxYAMLLines = 2000
	maxYAMLDepth = 12
)

type yamlKind int

const (
	yamlScalar yamlKind = iota
	yamlMapping
	yamlSequence
)

// yamlNode is one node of the parsed document.
type yamlNode struct {
	kind   yamlKind
	scalar string
	// order preserves the order keys appeared in, so a document with a `criteria`
	// list reads back in the order the developer wrote it.
	order   []string
	mapping map[string]*yamlNode
	seq     []*yamlNode
}

func (n *yamlNode) child(key string) (*yamlNode, bool) {
	if n == nil || n.kind != yamlMapping {
		return nil, false
	}
	child, found := n.mapping[key]
	return child, found
}

// text is the scalar at a key, trimmed. Absent, or a key holding a collection,
// answers empty — a caller wanting to tell those apart uses child.
func (n *yamlNode) text(key string) string {
	child, found := n.child(key)
	if !found || child.kind != yamlScalar {
		return ""
	}
	return strings.TrimSpace(child.scalar)
}

// items is the sequence at a key. A scalar there is read as a one-item sequence,
// because `secondary_metrics: mae` is a thing a developer writes and refusing it
// would be pedantry rather than safety.
func (n *yamlNode) items(key string) []*yamlNode {
	child, found := n.child(key)
	if !found {
		return nil
	}
	switch child.kind {
	case yamlSequence:
		return child.seq
	case yamlScalar:
		if strings.TrimSpace(child.scalar) == "" {
			return nil
		}
		return []*yamlNode{child}
	default:
		return nil
	}
}

// yamlLine is one significant line: its indentation, its content, and whether it
// opened a sequence item.
//
// A `- ` is normalised here rather than in the parser: indent is the column the
// item's *content* starts at, and dashIndent the column the dash itself is at.
// That is what lets `- metric: rmse` followed by an aligned `  threshold: 0.35`
// parse as one mapping without the parser having to reason about the dash.
type yamlLine struct {
	number     int
	indent     int
	dashIndent int
	dash       bool
	text       string
}

// parseYAML reads the document, or says what stopped it.
func parseYAML(source string) (*yamlNode, error) {
	lines, err := scanYAML(source)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		// An empty document is a mapping with nothing in it rather than an error: a
		// developer who emptied the file has said something, and "no criterion stated"
		// is a better answer than "unparseable".
		return &yamlNode{kind: yamlMapping, mapping: map[string]*yamlNode{}}, nil
	}

	parser := &yamlParser{lines: lines}
	node, err := parser.node(lines[0].dashIndent, 0)
	if err != nil {
		return nil, err
	}
	if parser.at < len(parser.lines) {
		return nil, fmt.Errorf(
			"line %d: %q is indented differently from the block it appears in, and ODE "+
				"could not place it", parser.lines[parser.at].number, parser.lines[parser.at].text)
	}
	return node, nil
}

// scanYAML turns the source into significant lines, refusing what the subset does
// not cover before any structure is built.
func scanYAML(source string) ([]yamlLine, error) {
	raw := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	if len(raw) > maxYAMLLines {
		return nil, fmt.Errorf("the file has %d lines, and at most %d are read",
			len(raw), maxYAMLLines)
	}

	out := make([]yamlLine, 0, len(raw))
	for index, line := range raw {
		number := index + 1
		content, indent, err := stripYAML(line, number)
		if err != nil {
			return nil, err
		}
		if content == "" {
			continue
		}
		switch content {
		case "---":
			// A single document is read. A second one would mean two sets of criteria
			// with no rule for which is the developer's, and guessing is worse than
			// refusing.
			if len(out) > 0 {
				return nil, fmt.Errorf(
					"line %d: the file holds more than one YAML document, and ODE reads one", number)
			}
			continue
		case "...":
			continue
		}
		if strings.HasPrefix(content, "&") || strings.HasPrefix(content, "*") ||
			strings.HasPrefix(content, "!") {
			return nil, fmt.Errorf(
				"line %d: anchors, aliases and tags are not read (%q)", number, content)
		}

		entry := yamlLine{number: number, indent: indent, dashIndent: indent, text: content}
		if content == "-" || strings.HasPrefix(content, "- ") {
			entry.dash = true
			rest := strings.TrimPrefix(content, "-")
			trimmed := strings.TrimLeft(rest, " ")
			// The content column, so an aligned continuation line lands in the same
			// mapping as the key that followed the dash.
			entry.indent = indent + 1 + (len(rest) - len(trimmed))
			entry.text = trimmed
		}
		out = append(out, entry)
	}
	return out, nil
}

// stripYAML removes indentation and a trailing comment, and refuses a tab used
// for indentation the way YAML itself does.
func stripYAML(line string, number int) (content string, indent int, err error) {
	for _, r := range line {
		switch r {
		case ' ':
			indent++
		case '\t':
			return "", 0, fmt.Errorf(
				"line %d: a tab is used for indentation, which YAML does not allow", number)
		default:
			return trimComment(line[indent:]), indent, nil
		}
	}
	return "", 0, nil
}

// trimComment removes a `#` comment, respecting quotes.
//
// A `#` only starts a comment at the beginning of the content or after a space,
// which is YAML's own rule and the one that keeps a URL or a colour out of trouble.
func trimComment(text string) string {
	inSingle, inDouble := false, false
	for index, r := range text {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '#' && !inSingle && !inDouble:
			if index == 0 || text[index-1] == ' ' {
				return strings.TrimRight(text[:index], " ")
			}
		}
	}
	return strings.TrimRight(text, " ")
}

type yamlParser struct {
	lines []yamlLine
	at    int
}

// node parses whatever begins at the current line, at the given indentation.
func (p *yamlParser) node(indent, depth int) (*yamlNode, error) {
	if depth > maxYAMLDepth {
		return nil, fmt.Errorf("line %d: the document nests deeper than %d levels",
			p.lines[p.at].number, maxYAMLDepth)
	}
	if p.lines[p.at].dash && p.lines[p.at].dashIndent == indent {
		return p.sequence(indent, depth)
	}
	return p.mapping(indent, depth)
}

func (p *yamlParser) sequence(indent, depth int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlSequence}
	for p.at < len(p.lines) {
		line := p.lines[p.at]
		if !line.dash || line.dashIndent != indent {
			break
		}
		started := p.at

		if line.text == "" {
			// `-` alone: the item is whatever is indented under it.
			p.at++
			if p.at < len(p.lines) && p.lines[p.at].dashIndent > indent {
				item, err := p.node(p.lines[p.at].dashIndent, depth+1)
				if err != nil {
					return nil, err
				}
				node.seq = append(node.seq, item)
			} else {
				node.seq = append(node.seq, &yamlNode{kind: yamlScalar})
			}
		} else {
			// The item's content is a line in its own right at the content column, so a
			// mapping item and its aligned continuation lines parse as one node.
			p.lines[p.at].dash = false
			p.lines[p.at].dashIndent = line.indent
			item, err := p.node(line.indent, depth+1)
			if err != nil {
				return nil, err
			}
			node.seq = append(node.seq, item)
		}

		if p.at == started {
			return nil, fmt.Errorf("line %d: %q could not be read as a list item",
				line.number, line.text)
		}
	}
	return node, nil
}

func (p *yamlParser) mapping(indent, depth int) (*yamlNode, error) {
	first := p.lines[p.at]
	// A single scalar where a mapping was expected is a scalar document — `metric`
	// on its own line, say. Read as one rather than refused.
	if _, _, isEntry := splitYAMLKey(first.text); !isEntry {
		p.at++
		scalar, err := unquoteYAML(first.text, first.number)
		if err != nil {
			return nil, err
		}
		return &yamlNode{kind: yamlScalar, scalar: scalar}, nil
	}

	node := &yamlNode{kind: yamlMapping, mapping: map[string]*yamlNode{}}
	for p.at < len(p.lines) {
		line := p.lines[p.at]
		if line.dash || line.indent != indent {
			break
		}
		key, rest, isEntry := splitYAMLKey(line.text)
		if !isEntry {
			return nil, fmt.Errorf(
				"line %d: %q is not `key: value`, and ODE reads block mappings only",
				line.number, line.text)
		}
		key, err := unquoteYAML(key, line.number)
		if err != nil {
			return nil, err
		}
		if _, duplicate := node.mapping[key]; duplicate {
			// Refused rather than last-wins: two thresholds for one metric is a file
			// whose author disagrees with themselves, and picking one is a verdict.
			return nil, fmt.Errorf("line %d: %q appears twice in the same mapping",
				line.number, key)
		}
		p.at++

		value, err := p.value(rest, line, indent, depth)
		if err != nil {
			return nil, err
		}
		node.order = append(node.order, key)
		node.mapping[key] = value
	}
	return node, nil
}

// value reads what follows a key, whether it is on the same line or under it.
func (p *yamlParser) value(rest string, line yamlLine, indent, depth int) (*yamlNode, error) {
	switch {
	case rest == "|" || rest == ">" || rest == "|-" || rest == ">-" ||
		rest == "|+" || rest == ">+":
		return p.blockScalar(rest, indent), nil

	case rest != "":
		if strings.HasPrefix(rest, "[") || strings.HasPrefix(rest, "{") {
			return flowYAML(rest, line.number)
		}
		scalar, err := unquoteYAML(rest, line.number)
		if err != nil {
			return nil, err
		}
		return &yamlNode{kind: yamlScalar, scalar: scalar}, nil

	default:
		// Nothing on the line: the value is what is indented under the key, or a
		// sequence at the key's own indentation, which YAML also allows.
		if p.at >= len(p.lines) {
			return &yamlNode{kind: yamlScalar}, nil
		}
		next := p.lines[p.at]
		switch {
		case next.dash && next.dashIndent >= indent:
			return p.sequence(next.dashIndent, depth+1)
		case !next.dash && next.indent > indent:
			return p.node(next.indent, depth+1)
		default:
			// A key with nothing under it. An empty scalar rather than an error: `note:`
			// with nothing after it is a developer leaving a field blank.
			return &yamlNode{kind: yamlScalar}, nil
		}
	}
}

// blockScalar consumes the lines of a `|` or `>` scalar.
//
// Folded (`>`) joins with spaces and literal (`|`) with newlines, which is the
// difference that matters for a `rationale` a person reads. Chomping indicators
// are accepted and ignored: nothing here is sensitive to a trailing newline.
func (p *yamlParser) blockScalar(indicator string, indent int) *yamlNode {
	folded := strings.HasPrefix(indicator, ">")
	var parts []string
	for p.at < len(p.lines) {
		line := p.lines[p.at]
		if line.dashIndent <= indent {
			break
		}
		text := line.text
		if line.dash {
			// Inside a block scalar a leading `- ` is text, not structure.
			text = "- " + text
		}
		parts = append(parts, text)
		p.at++
	}
	separator := "\n"
	if folded {
		separator = " "
	}
	return &yamlNode{kind: yamlScalar, scalar: strings.Join(parts, separator)}
}

// splitYAMLKey finds the `:` that separates a key from its value, outside quotes.
// The colon has to be followed by a space or end the line, which is what keeps
// `url: http://x` from splitting at the wrong one.
func splitYAMLKey(text string) (key, rest string, ok bool) {
	inSingle, inDouble := false, false
	for index := 0; index < len(text); index++ {
		switch text[index] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ':':
			if inSingle || inDouble {
				continue
			}
			if index+1 == len(text) {
				return strings.TrimSpace(text[:index]), "", true
			}
			if text[index+1] == ' ' {
				return strings.TrimSpace(text[:index]),
					strings.TrimSpace(text[index+1:]), true
			}
		}
	}
	return "", "", false
}

// flowYAML reads `[]`, `{}` and a flow sequence of scalars. Anything nested is
// refused rather than flattened.
func flowYAML(text string, number int) (*yamlNode, error) {
	trimmed := strings.TrimSpace(text)
	switch {
	case trimmed == "[]":
		return &yamlNode{kind: yamlSequence}, nil
	case trimmed == "{}":
		return &yamlNode{kind: yamlMapping, mapping: map[string]*yamlNode{}}, nil
	case strings.HasPrefix(trimmed, "{"):
		return nil, fmt.Errorf(
			"line %d: an inline mapping is not read; write it as indented `key: value` lines",
			number)
	case !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]"):
		return nil, fmt.Errorf("line %d: %q does not close its bracket", number, trimmed)
	}

	inner := trimmed[1 : len(trimmed)-1]
	if strings.ContainsAny(inner, "[]{}") {
		return nil, fmt.Errorf(
			"line %d: a nested inline collection is not read; write it as indented lines", number)
	}
	node := &yamlNode{kind: yamlSequence}
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		scalar, err := unquoteYAML(part, number)
		if err != nil {
			return nil, err
		}
		node.seq = append(node.seq, &yamlNode{kind: yamlScalar, scalar: scalar})
	}
	return node, nil
}

// unquoteYAML strips matching quotes. A double-quoted scalar's escapes are read
// for the two that appear in practice; anything else is left as written, because
// a criterion's metric name is not a place for escape sequences.
func unquoteYAML(text string, number int) (string, error) {
	trimmed := strings.TrimSpace(text)
	// An anchor, an alias or a tag as a *value*, which scanYAML's own check does not
	// see because it looks at the start of the line. Refused here so that a file
	// using them is reported as unparseable with its own word in the message,
	// rather than as whatever structural confusion they cause two lines later.
	if trimmed != "" {
		switch trimmed[0] {
		case '&', '*', '!':
			return "", fmt.Errorf(
				"line %d: anchors, aliases and tags are not read (%q)", number, trimmed)
		}
	}
	if len(trimmed) < 2 {
		return trimmed, nil
	}
	first, last := trimmed[0], trimmed[len(trimmed)-1]
	if first == '\'' && last == '\'' {
		return strings.ReplaceAll(trimmed[1:len(trimmed)-1], "''", "'"), nil
	}
	if first == '"' && last == '"' {
		body := trimmed[1 : len(trimmed)-1]
		body = strings.ReplaceAll(body, `\"`, `"`)
		body = strings.ReplaceAll(body, `\\`, `\`)
		return body, nil
	}
	if first == '\'' || first == '"' {
		return "", fmt.Errorf("line %d: %q opens a quote it does not close", number, trimmed)
	}
	return trimmed, nil
}
