package workflow

import (
	"fmt"
	"strings"
)

// ValidationError is a structural validation failure with a location: the node
// it attaches to ("" for graph-level errors), the edge "from→to" ("" when not
// edge-level), the field or reference within it ("" when not applicable), and
// a message. Error() renders a "node edge field: msg" prefix so callers and
// tests can locate the offending node/edge/reference by substring.
type ValidationError struct {
	Node    string
	Edge    string // "from→to" for edge-level errors; "" otherwise
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	loc := e.Node
	if e.Edge != "" {
		if loc != "" {
			loc += " "
		}
		loc += "edge " + e.Edge
	}
	if e.Field != "" {
		if loc != "" {
			loc += " "
		}
		loc += e.Field
	}
	if loc == "" {
		return e.Message
	}
	return loc + ": " + e.Message
}

// Validate checks a definition + optional input against the structural contract
// (spec decisions 4, 6, 8): references resolve to existing nodes and declared
// output fields; the graph is acyclic with exactly one entry; and when input
// is supplied it satisfies the declared input schema. The llm_route/default
// rule is deliberately NOT enforced here — spec decision 3 makes a missing
// match a runtime node failure (handled by the runner), not a design-time
// error. Returns nil when structurally execution-ready.
//
// It does no I/O and is safe to call before Run. Run itself calls Validate as
// a precondition, so a definition that passes here won't be rejected later for
// structural reasons.
func Validate(def *Definition, input map[string]any) error {
	if _, err := topoOrder(def); err != nil {
		// topoOrder surfaces cycle / dangling-edge errors as a plain message;
		// pinpointing the offending edge needs a richer topo result and is
		// left for a later pass.
		return &ValidationError{Message: err.Error()}
	}
	if err := exactlyOneEntry(def); err != nil {
		return err
	}
	if err := validateNodeKinds(def); err != nil {
		return err
	}
	if err := validateRefs(def); err != nil {
		return err
	}
	if err := validateOutputRefs(def); err != nil {
		return err
	}
	if input != nil {
		if err := validateInput(def, input); err != nil {
			return err
		}
	}
	return nil
}

// indegree returns the in-degree of every node. Shared by Validate's entry
// check and would-be other consumers, so the computation isn't duplicated.
func indegree(def *Definition) map[string]int {
	indeg := make(map[string]int, len(def.Nodes))
	for _, n := range def.Nodes {
		indeg[n.Name] = 0
	}
	for _, e := range def.Edges {
		indeg[e.To]++
	}
	return indeg
}

// exactlyOneEntry enforces the single-entry rule: exactly one node has
// in-degree 0. topoOrder already rejected cycles, so zero entries means an
// empty or fully-cyclic graph.
func exactlyOneEntry(def *Definition) error {
	indeg := indegree(def)
	var entries []string
	for _, n := range def.Nodes {
		if indeg[n.Name] == 0 {
			entries = append(entries, n.Name)
		}
	}
	switch len(entries) {
	case 0:
		return &ValidationError{Message: "no entry node (empty or fully cyclic graph)"}
	case 1:
		return nil
	default:
		return &ValidationError{Message: fmt.Sprintf("expected exactly one entry node, got %d (%s)", len(entries), strings.Join(entries, ", "))}
	}
}

// validateNodeKinds guards code-node required fields (spec decision 3): a code
// node carries a body in a supported language (the set mirrors langSpec in
// adapters.go), and every node's Kind is one of the known values. Tool/llm
// required fields (tool name / prompt) are deliberately not enforced here —
// the leaf surfaces a clear error at runtime, and an llm node without a prompt
// is harmless to validate (and handy as a graph placeholder).
func validateNodeKinds(def *Definition) error {
	for _, n := range def.Nodes {
		switch n.Kind {
		case KindTool, KindLLM, KindCondition:
			// not enforced — see the doc note above. condition is a pure
			// routing point (no body); its branches live on outgoing edges.
		case KindCode:
			if n.Code == "" {
				return &ValidationError{Node: n.Name, Field: "code", Message: "code node requires a code body"}
			}
			if n.Language != "" {
				if _, ok := langSpec[n.Language]; !ok {
					return &ValidationError{Node: n.Name, Field: "lang", Message: fmt.Sprintf("unsupported language %q (want python or sh)", n.Language)}
				}
			}
		default:
			return &ValidationError{Node: n.Name, Field: "kind", Message: fmt.Sprintf("unknown node kind %q", n.Kind)}
		}
	}
	return nil
}

// validateOutputRefs checks the workflow-level Output map's ${...} references
// (spec decision 4) — they resolve against the same graph + input schema as
// node-level references, so a typo is caught at design time rather than after
// a successful run. Node label "(output)" pins the error to this map.
func validateOutputRefs(def *Definition) error {
	if len(def.Output) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(def.Nodes))
	for _, n := range def.Nodes {
		declared[n.Name] = true
	}
	outputSchema := make(map[string]map[string]any, len(def.Nodes))
	for _, n := range def.Nodes {
		if len(n.Output) > 0 {
			outputSchema[n.Name] = n.Output
		}
	}
	for _, s := range collectStrings(def.Output) {
		if err := checkRefs("(output)", s, declared, outputSchema, def); err != nil {
			return err
		}
	}
	return nil
}

// validateRefs walks every node's Prompt + every string nested anywhere in its
// Input map (decision 4: references are statically validated, including those
// buried in nested maps/lists), and checks each ${...} against the graph +
// declared output schemas.
//
// Nodes that don't declare an output schema are trusted at the field level —
// the leaf's raw return is parsed at runtime. Decision 4 says every node
// *should* declare one; enforcing "must" is deliberately deferred because
// tool-node output is often dynamic, and the tracer bullet keeps schema-less
// nodes valid rather than forcing every tool call to pre-declare its shape.
func validateRefs(def *Definition) error {
	declared := make(map[string]bool, len(def.Nodes))
	for _, n := range def.Nodes {
		declared[n.Name] = true
	}
	outputSchema := make(map[string]map[string]any, len(def.Nodes))
	for _, n := range def.Nodes {
		if len(n.Output) > 0 {
			outputSchema[n.Name] = n.Output
		}
	}
	for _, n := range def.Nodes {
		for _, s := range collectStrings(n.Input) {
			if err := checkRefs(n.Name, s, declared, outputSchema, def); err != nil {
				return err
			}
		}
		if n.Prompt != "" {
			if err := checkRefs(n.Name, n.Prompt, declared, outputSchema, def); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectStrings recursively gathers every string value inside a map/slice
// (nested arbitrarily deep), so references in nested tool input get validated.
func collectStrings(v any) []string {
	var out []string
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(v)
	return out
}

// checkRefs inspects every ${...} token in s. node is the referencing node's
// name; the offending reference is reported via ${expr} in Field.
func checkRefs(node, s string, declared map[string]bool, outputSchema map[string]map[string]any, def *Definition) error {
	for _, m := range refPattern.FindAllStringSubmatch(s, -1) {
		expr := m[1]
		parts := strings.Split(expr, ".")
		ns := parts[0]
		if ns == "input" {
			// Only enforce when a schema with properties is declared.
			if len(parts) >= 2 && len(def.Input.Schema) > 0 {
				if props, _ := def.Input.Schema["properties"].(map[string]any); props != nil {
					if _, ok := props[parts[1]]; !ok {
						return &ValidationError{Node: node, Field: "${" + expr + "}", Message: fmt.Sprintf("input field %q not declared in schema", parts[1])}
					}
				}
			}
			continue
		}
		if !declared[ns] {
			return &ValidationError{Node: node, Field: "${" + expr + "}", Message: fmt.Sprintf("references unknown node %q", ns)}
		}
		// Upstream output schema declared → referenced field must be in it.
		// No schema → trust the leaf's runtime return (see validateRefs note).
		if schema, ok := outputSchema[ns]; ok && len(parts) >= 2 {
			if _, has := schema[parts[1]]; !has {
				return &ValidationError{Node: node, Field: "${" + expr + "}", Message: fmt.Sprintf("field %q not in %s output schema", parts[1], ns)}
			}
		}
	}
	return nil
}

// validateInput checks input against the declared JSON-schema subset (type +
// required + properties.<field>.type). Full JSON schema is out of scope for
// the tracer bullet.
func validateInput(def *Definition, input map[string]any) error {
	schema := def.Input.Schema
	if len(schema) == 0 {
		return nil
	}
	if req, _ := schema["required"].([]any); req != nil {
		for _, r := range req {
			name, _ := r.(string)
			if name == "" {
				continue
			}
			if _, ok := input[name]; !ok {
				return &ValidationError{Field: "input." + name, Message: fmt.Sprintf("missing required input field %q", name)}
			}
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for name, val := range input {
		ps, _ := props[name].(map[string]any)
		if ps == nil {
			continue
		}
		wantType, _ := ps["type"].(string)
		if wantType == "" {
			continue
		}
		if !typeMatches(wantType, val) {
			return &ValidationError{Field: "input." + name, Message: fmt.Sprintf("input field %q want type %s, got %T", name, wantType, val)}
		}
	}
	return nil
}

// typeMatches covers the JSON-schema primitive types the tracer bullet cares
// about. YAML-decoded numbers arrive as int or float64.
func typeMatches(want string, v any) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		switch v.(type) {
		case int, int64:
			return true
		}
		return false
	case "number":
		switch v.(type) {
		case int, int64, float64:
			return true
		}
		return false
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	default:
		return true // unknown / unenforced type
	}
}
