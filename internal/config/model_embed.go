package config

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
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
// the builtin entry for the same key. Parsing is tolerant: entries whose
// value is not a ModelMeta object (e.g. a "_comment" string authored for
// human readers) are skipped, so the seeded file's comment doesn't break
// the whole override. Used by LookupModelMeta.
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
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return out
	}
	for k, rv := range raw {
		var m ModelMeta
		if err := json.Unmarshal(rv, &m); err == nil {
			out[k] = m
		}
		// Non-ModelMeta values (e.g. "_comment": "...") are skipped.
	}
	return out
}

// localModelMetaSeed is the example ~/.fluctio/model-meta.json written on
// first run so users discover the local override mechanism. The "_comment"
// key is skipped by mergedMetaTable's tolerant parser. Never overwrite an
// existing file — user edits must be preserved.
const localModelMetaSeed = `{
  "_comment": "本地模型元数据覆盖（优先级高于内置表）。key 是 model id 的子串（大小写不敏感），value = {contextWindow: 输入窗口, maxTokens: 输出上限}。删掉 example-model-id，加你自己的模型。例: 模型 openai/LongCat-2.0 -> \"longcat\": {\"contextWindow\": 1000000, \"maxTokens\": 131072}",
  "example-model-id": { "contextWindow": 200000, "maxTokens": 8192 }
}
`

// EnsureLocalModelMetaSeed writes a commented example model-meta.json into
// homeDir when (and only when) it does not yet exist, so users discover the
// local override mechanism. Idempotent and never overwrites user edits.
func EnsureLocalModelMetaSeed(homeDir string) error {
	if homeDir == "" {
		return nil
	}
	path := filepath.Join(homeDir, "model-meta.json")
	if _, err := os.Stat(path); err == nil {
		return nil // already present — preserve user edits
	}
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(localModelMetaSeed), 0o644)
}
