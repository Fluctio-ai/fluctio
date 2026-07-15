package config

import (
	"path/filepath"
	"strings"
)

// LookupModelMeta resolves a modelID to its ModelMeta (contextWindow +
// maxTokens) using case-insensitive substring + longest-first matching.
// Merges the builtin table with ~/.fluctio/model-meta.json. Returns
// matched=false when no key in the merged table is a substring of modelID.
//
// Case-insensitive: model IDs in the wild mix case (LongCat-2.0, GPT-4)
// while table keys are conventionally lowercase, so we compare lowercased.
func LookupModelMeta(modelID string) (ModelMeta, bool) {
	return lookupMetaIn(modelID, modelMetaOverridePath())
}

// lookupMetaIn is the test entry point with an explicit local override
// path. Case-insensitive substring + longest-first: when several keys are
// substrings of modelID (compared lowercased), the longest key wins and
// its ModelMeta is returned.
func lookupMetaIn(modelID, localPath string) (ModelMeta, bool) {
	tbl := mergedMetaTable(localPath)
	var bestKey string
	lowerID := strings.ToLower(modelID)
	for key := range tbl {
		if strings.Contains(lowerID, strings.ToLower(key)) && len(key) > len(bestKey) {
			bestKey = key
		}
	}
	if bestKey == "" {
		return ModelMeta{}, false
	}
	return tbl[bestKey], true
}

// modelMetaOverridePath returns the ~/.fluctio/model-meta.json local
// override path. Reuses config.go's HomeDir() (honors FLUCTIO_HOME env).
func modelMetaOverridePath() string {
	home, err := HomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "model-meta.json")
}
