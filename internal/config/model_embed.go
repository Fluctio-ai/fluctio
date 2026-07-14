package config

import (
	_ "embed"
	"encoding/json"
)

//go:embed model_context.json
var modelContextJSON []byte

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
