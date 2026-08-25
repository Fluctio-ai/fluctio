// Package llmjson holds tolerance helpers for JSON emitted by LLMs, where
// the usual strictness guarantees don't hold: models ignore or get degraded
// off the JSON-mode hint (see provider's response_format retry fallback)
// and emit bare quotes inside string values, drop the {"key": [...]}
// wrapper and emit the bare array, or wrap everything in a ```json fence.
// encoding/json rejects all three. UnmarshalLLM is the shared entry point;
// call sites should not hand-roll their own strip/retry chains.
package llmjson

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// StripFence removes a surrounding markdown code fence (```json … ```),
// which tuned models add reflexively even when the prompt says "no fences".
func StripFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// UnmarshalLLM decodes an LLM-emitted JSON document into v, tolerating the
// failure modes observed across models, in order of preference:
//
//  1. strict parse — valid JSON passes through untouched
//  2. a ```json fence around the document (StripFence)
//  3. a dropped {"key": [...]} wrapper — when v points to a struct whose
//     only slice field is F, a top-level bare array decodes into F
//  4. bare quotes inside string values (RepairUnescapedQuotes), retried
//     once on the object path and once more combined with (3)
//
// On total failure it returns the original strict-parse error so callers
// can log it with their own context.
func UnmarshalLLM(s string, v any) error {
	doc := StripFence(s)
	err := json.Unmarshal([]byte(doc), v)
	if err == nil {
		return nil
	}
	if trimmed := strings.TrimSpace(doc); strings.HasPrefix(trimmed, "[") {
		if e2 := unmarshalBareArray(trimmed, v); e2 == nil {
			return nil
		} else if e3 := unmarshalBareArray(RepairUnescapedQuotes(trimmed), v); e3 == nil {
			return nil
		}
	}
	if e2 := json.Unmarshal([]byte(RepairUnescapedQuotes(doc)), v); e2 == nil {
		return nil
	}
	return err
}

// unmarshalBareArray decodes a top-level JSON array into the single slice
// field of the struct v points to. v must be a non-nil pointer to a struct
// with exactly one exported field of slice type (e.g. the anonymous
// struct{Topics []T} wrappers most extract prompts parse into); any other
// shape returns an error so UnmarshalLLM falls through to other strategies.
func unmarshalBareArray(doc string, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("bare-array target must be a non-nil pointer")
	}
	rs := rv.Elem()
	if rs.Kind() != reflect.Struct {
		return fmt.Errorf("bare-array target must point to a struct, got %s", rs.Kind())
	}
	rt := rs.Type()
	var field *reflect.StructField
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.PkgPath != "" { // unexported — reflect can't Set it
			continue
		}
		if f.Type.Kind() == reflect.Slice {
			if field != nil {
				return fmt.Errorf("struct %s has multiple slice fields", rt)
			}
			fc := f
			field = &fc
		}
	}
	if field == nil {
		return fmt.Errorf("struct %s has no settable slice field", rt)
	}
	dst := reflect.New(field.Type) // *[]T
	if err := json.Unmarshal([]byte(doc), dst.Interface()); err != nil {
		return err
	}
	rs.FieldByName(field.Name).Set(dst.Elem())
	return nil
}

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
