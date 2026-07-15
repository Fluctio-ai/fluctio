package config

import (
	_ "embed"
	"encoding/json"
	"os"
)

// ModelMeta is the per-model metadata projected from the builtin table.
// ContextWindow is the input token limit; MaxTokens is the output token
// limit (0 when the source has no maxOutputTokens for the model).
type ModelMeta struct {
	ContextWindow int `json:"contextWindow"`
	MaxTokens     int `json:"maxTokens"`
}

//go:embed model_meta.json
var modelMetaJSON []byte

// builtinMetaTable parses the embedded model_meta.json.
// Returns key=model id substring, value=ModelMeta. A nil return signals a
// parse failure (shouldn't happen for a static embed) — callers fall back
// to an empty map.
func builtinMetaTable() map[string]ModelMeta {
	out := map[string]ModelMeta{}
	if err := json.Unmarshal(modelMetaJSON, &out); err != nil {
		return nil
	}
	return out
}

// MergedMetaTablePublic is the HTTP-facing entry point: builtin table
// merged with the default local override path (~/.fluctio/model-meta.json).
func MergedMetaTablePublic() map[string]ModelMeta {
	return mergedMetaTable(modelMetaOverridePath())
}

// mergedMetaTable merges the builtin table with a local override file
// (path empty or unreadable → builtin only). A local entry wholly replaces
// the builtin entry for the same key. Used by LookupModelMeta.
func mergedMetaTable(localPath string) map[string]ModelMeta {
	out := builtinMetaTable()
	if out == nil {
		out = map[string]ModelMeta{}
	}
	if localPath == "" {
		return out
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return out
	}
	local := map[string]ModelMeta{}
	if err := json.Unmarshal(data, &local); err != nil {
		return out
	}
	for k, v := range local {
		out[k] = v
	}
	return out
}
