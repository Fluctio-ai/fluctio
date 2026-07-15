package config

import (
	_ "embed"
	"encoding/json"
	"os"
)

//go:embed model_context.json
var modelContextJSON []byte

//go:embed model_maxtokens.json
var modelMaxTokensJSON []byte

// builtinModelTable 解析 embed 的 model_context.json。
// 返回 key=model id 子串, value=contextWindow token 数。
// 解析失败（不应发生，embed 静态文件）panic 于 init 更安全，这里返回 nil 由调用方处理。
func builtinModelTable() map[string]int {
	out := map[string]int{}
	if err := json.Unmarshal(modelContextJSON, &out); err != nil {
		return nil
	}
	return out
}

// MergedModelTablePublic 供 HTTP handler 用（读默认本地覆盖路径）。
func MergedModelTablePublic() map[string]int {
	return mergedModelTable(modelContextOverridePath())
}

// mergedModelTable 合并内置表 + 本地覆盖文件（path 为空或读失败则只用内置）。
// 同 key 本地覆盖优先。用于 LookupModelMeta。
func mergedModelTable(localPath string) map[string]int {
	out := builtinModelTable()
	if out == nil {
		out = map[string]int{}
	}
	if localPath == "" {
		return out
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return out
	}
	local := map[string]int{}
	if err := json.Unmarshal(data, &local); err != nil {
		return out
	}
	for k, v := range local {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// maxTokens 表（与 contextWindow 表平行，独立 key/value）
// ---------------------------------------------------------------------------

// builtinMaxTokensTable 解析 embed 的 model_maxtokens.json。
// 内置为空对象 {}（maxTokens 各模型不一且数据不全，留给本地覆盖填）。
// 解析失败返回 nil 由调用方处理。
func builtinMaxTokensTable() map[string]int {
	out := map[string]int{}
	if err := json.Unmarshal(modelMaxTokensJSON, &out); err != nil {
		return nil
	}
	return out
}

// MergedMaxTokensTablePublic 供 HTTP handler 用（读默认本地覆盖路径）。
func MergedMaxTokensTablePublic() map[string]int {
	return mergedMaxTokensTable(modelMaxTokensOverridePath())
}

// mergedMaxTokensTable 合并内置表 + 本地覆盖文件（path 为空或读失败则只用内置）。
// 同 key 本地覆盖优先。用于 LookupModelMaxTokens。
func mergedMaxTokensTable(localPath string) map[string]int {
	out := builtinMaxTokensTable()
	if out == nil {
		out = map[string]int{}
	}
	if localPath == "" {
		return out
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return out
	}
	local := map[string]int{}
	if err := json.Unmarshal(data, &local); err != nil {
		return out
	}
	for k, v := range local {
		out[k] = v
	}
	return out
}
