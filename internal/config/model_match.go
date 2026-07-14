package config

import (
	"path/filepath"
	"strings"
)

// LookupModelMeta 按 modelID 查 contextWindow（substring + longest-first）。
// 合并内置表 + ~/.fluctio/model-context.json 本地覆盖。未中返回 matched=false。
func LookupModelMeta(modelID string) (contextWindow int, matched bool) {
	return lookupModelMetaIn(modelID, modelContextOverridePath())
}

// lookupModelMetaIn 用指定本地覆盖路径查（测试入口）。
// substring 匹配 + longest-first：当多个 key 都为 modelID 子串时取最长。
func lookupModelMetaIn(modelID, localPath string) (int, bool) {
	tbl := mergedModelTable(localPath)
	var bestKey string
	for key := range tbl {
		if strings.Contains(modelID, key) && len(key) > len(bestKey) {
			bestKey = key
		}
	}
	if bestKey == "" {
		return 0, false
	}
	return tbl[bestKey], true
}

// modelContextOverridePath 返回 ~/.fluctio/model-context.json 本地覆盖路径。
// 复用 config.go 的 HomeDir()（honors FLUCTIO_HOME env）。
func modelContextOverridePath() string {
	home, err := HomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "model-context.json")
}
