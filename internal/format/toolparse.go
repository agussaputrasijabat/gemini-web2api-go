package format

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// This file holds the tolerant parser shared by the OpenAI and Gemini-native
// paths. Models are instructed to emit a fenced block, but in practice they
// produce a wide variety of near-misses: missing closing fence, trailing prose
// on the same line, nested braces, backticks inside string arguments, several
// blocks in one answer, or bare JSON with no fence at all. Everything here is
// input from a language model, never trusted.

// reFenceStart matches the opening of a fenced tool block. The trailing
// newline is optional because some models omit it.
var reFenceStart = regexp.MustCompile("(?i)\x60\x60\x60[ \t]*(?:tool_call|function_call|tool_calls)[ \t]*\r?\n?")

// reBareStart matches a tag used as a bare prefix line, without fences.
var reBareStart = regexp.MustCompile("(?i)(?:^|\n)[ \t]*(?:tool_call|function_call)[ \t]*:?[ \t]*\r?\n?")

// reClosingFence matches a fence terminator.
var reClosingFence = regexp.MustCompile("(?i)\r?\n?[ \t]*\x60\x60\x60")

// span is a byte range in the original text.
type span struct {
	start int
	end   int
}

// scanJSONValue returns the balanced JSON object or array starting at start.
// It tracks string state and escapes so braces inside string literals do not
// terminate the value. This is what makes backticks, braces and newlines
// inside argument strings safe.
func scanJSONValue(s string, start int) (string, int, bool) {
	if start >= len(s) {
		return "", start, false
	}
	open := s[start]
	if open != '{' && open != '[' {
		return "", start, false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return s[start : i+1], i + 1, true
			}
		}
	}
	return "", start, false
}

// extractPayloads finds every tool payload in text and returns the text with
// those payload spans removed. Payloads are raw JSON, unparsed.
//
// Strategy, in priority order:
//  1. fenced block + balanced JSON value (handles braces/backticks in args)
//  2. fenced block + raw content up to the closing fence (malformed JSON):
//     still returned so the caller can decide
//  3. bare `tool_call` prefix line + balanced JSON value
func extractPayloads(text string) (string, []string) {
	var payloads []string
	var removed []span

	// Pass 1: fenced blocks.
	for _, m := range reFenceStart.FindAllStringIndex(text, -1) {
		payloadStart := m[1]
		for payloadStart < len(text) && isJSONSpace(text[payloadStart]) {
			payloadStart++
		}

		if payload, end, ok := scanJSONValue(text, payloadStart); ok {
			payloads = append(payloads, payload)
			blockEnd := end
			if closeLoc := reClosingFence.FindStringIndex(text[end:]); closeLoc != nil {
				blockEnd = end + closeLoc[1]
			}
			removed = append(removed, span{start: m[0], end: blockEnd})
			continue
		}

		// Malformed JSON inside a fence: take everything up to the fence end.
		// Search for the closing fence only *after* the payload start, or the
		// optional leading newlines in the pattern let it match the opening
		// fence itself.
		if payloadStart <= len(text) {
			if closeLoc := reClosingFence.FindStringIndex(text[payloadStart:]); closeLoc != nil {
				if raw := strings.TrimSpace(text[payloadStart : payloadStart+closeLoc[0]]); raw != "" {
					payloads = append(payloads, raw)
				}
				removed = append(removed, span{start: m[0], end: payloadStart + closeLoc[1]})
			}
		}
	}

	clean := removeSpans(text, removed)

	// Pass 2: bare tag prefix followed by a balanced JSON value.
	var bareSpans []span
	for _, m := range reBareStart.FindAllStringSubmatchIndex(clean, -1) {
		payloadStart := m[1]
		for payloadStart < len(clean) && (clean[payloadStart] == ' ' || clean[payloadStart] == '\t') {
			payloadStart++
		}
		if payload, end, ok := scanJSONValue(clean, payloadStart); ok {
			payloads = append(payloads, payload)
			bareSpans = append(bareSpans, span{start: m[0], end: end})
		}
	}
	if len(bareSpans) > 0 {
		clean = removeSpans(clean, bareSpans)
	}

	if len(payloads) > 0 {
		clean = collapseWhitespaceLines(clean)
	}

	return clean, payloads
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// removeSpans deletes the given spans from text, ignoring overlaps.
func removeSpans(text string, spans []span) string {
	if len(spans) == 0 {
		return text
	}
	// Sort by start; spans are produced in order but be defensive.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j-1].start > spans[j].start; j-- {
			spans[j-1], spans[j] = spans[j], spans[j-1]
		}
	}

	var b strings.Builder
	last := 0
	for _, sp := range spans {
		if sp.start < last {
			if sp.end > last {
				last = sp.end
			}
			continue
		}
		if sp.start > len(text) {
			break
		}
		b.WriteString(text[last:sp.start])
		end := sp.end
		if end > len(text) {
			end = len(text)
		}
		last = end
	}
	if last < len(text) {
		b.WriteString(text[last:])
	}
	return b.String()
}

// collapseWhitespaceLines trims trailing spaces per line and collapses runs of
// three or more blank lines down to one, which is what a removed block leaves
// behind.
func collapseWhitespaceLines(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// parseArgsValue normalizes an "arguments"/"args"/"parameters" field into a
// JSON object string. Accepts both the OpenAI form (a JSON string) and the
// Gemini form (an object). Invalid or missing values become "{}".
func parseArgsValue(v any) (string, map[string]any) {
	switch a := v.(type) {
	case nil:
		return "{}", map[string]any{}
	case string:
		trimmed := strings.TrimSpace(a)
		if trimmed == "" {
			return "{}", map[string]any{}
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			return trimmed, obj
		}
		// The model sometimes double-encodes, e.g. "\"{\\\"city\\\":\\\"X\\\"}\"".
		var unquoted string
		if err := json.Unmarshal([]byte(trimmed), &unquoted); err == nil {
			unquoted = strings.TrimSpace(unquoted)
			if json.Unmarshal([]byte(unquoted), &obj) == nil {
				return unquoted, obj
			}
		}
		return trimmed, map[string]any{}
	case map[string]any:
		buf, err := json.Marshal(a)
		if err != nil {
			return "{}", map[string]any{}
		}
		return string(buf), a
	default:
		buf, err := json.Marshal(a)
		if err != nil {
			return "{}", map[string]any{}
		}
		var obj map[string]any
		if json.Unmarshal(buf, &obj) == nil {
			return string(buf), obj
		}
		return string(buf), map[string]any{}
	}
}

// parseCallPayload decodes one raw payload into a normalized call.
// It accepts {name, arguments}, {name, args} and {name, parameters}, with
// arguments either a JSON string or an object. It also accepts a call nested
// under a "function" key. An arguments-like key (or an id) must be present:
// a bare {"name": "x"} is far more likely to be user-facing JSON than a call.
func parseCallPayload(payload string) (name string, argsString string, argsObject map[string]any, id string, ok bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &raw); err != nil {
		return "", "", nil, "", false
	}

	// Some models wrap the call: {"function": {"name": ..., "arguments": ...}}
	if fn, isMap := raw["function"].(map[string]any); isMap {
		if _, hasName := raw["name"]; !hasName {
			raw = fn
		}
	}

	if s, isStr := raw["name"].(string); isStr {
		name = strings.TrimSpace(s)
	}
	if name == "" {
		if s, isStr := raw["tool"].(string); isStr {
			name = strings.TrimSpace(s)
		}
	}
	if name == "" {
		return "", "", nil, "", false
	}
	name = strings.Trim(name, "`\"' \t\n")

	// Accept id/call_id round-tripped by the model.
	if s, isStr := raw["id"].(string); isStr {
		id = strings.TrimSpace(s)
	} else if s, isStr := raw["call_id"].(string); isStr {
		id = strings.TrimSpace(s)
	}

	argVal, hasArgs := raw["arguments"]
	if !hasArgs {
		argVal, hasArgs = raw["args"]
	}
	if !hasArgs {
		argVal, hasArgs = raw["parameters"]
	}
	if !hasArgs {
		// No argument key at all. Only accept it when the payload carries a
		// call id, which no ordinary JSON answer does.
		if id == "" {
			return "", "", nil, "", false
		}
		argVal = map[string]any{}
	}
	argsString, argsObject = parseArgsValue(argVal)
	return name, argsString, argsObject, id, true
}

// newCallID builds an OpenAI-shaped tool call id.
func newCallID() string {
	return fmt.Sprintf("call_%s", RandHex(12))
}
