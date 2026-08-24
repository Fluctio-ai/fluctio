// Package llmjson holds tolerance helpers for JSON emitted by LLMs, where
// the usual strictness guarantees don't hold: models ignore or get degraded
// off the JSON-mode hint (see provider's response_format retry fallback)
// and emit bare quotes inside string values, which encoding/json rejects.
package llmjson

import "strings"

// RepairUnescapedQuotes escapes bare double quotes inside JSON string
// values. Heuristic: while inside a string, a '"' only closes it when the
// next non-whitespace character is structural (',' '}' ']' ':' or end of
// input); any other '"' is content and gets a backslash. Already-escaped
// \" sequences pass through untouched. Only meaningful on input whose
// strict parse already failed — valid JSON round-trips unchanged.
func RepairUnescapedQuotes(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	i, n := 0, len(s)
	for i < n {
		if s[i] != '"' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// Scan the string opened by s[i].
		j := i + 1
		var inner strings.Builder
		closed := false
		for j < n {
			if s[j] == '\\' && j+1 < n {
				inner.WriteByte(s[j])
				inner.WriteByte(s[j+1])
				j += 2
				continue
			}
			if s[j] == '"' {
				k := j + 1
				for k < n && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
					k++
				}
				if k >= n || s[k] == ',' || s[k] == '}' || s[k] == ']' || s[k] == ':' {
					closed = true
					j++ // consume the closing quote
					break
				}
				inner.WriteString(`\"`) // bare quote inside the value
				j++
				continue
			}
			inner.WriteByte(s[j])
			j++
		}
		b.WriteByte('"')
		b.WriteString(inner.String())
		if closed {
			b.WriteByte('"')
		}
		i = j
	}
	return b.String()
}
