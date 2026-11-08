package config

import "strings"

// This file centralizes credential redaction for the config structs.
//
// Storage paths (configs-table rows, agents.config blobs, CLI writes) must
// keep plaintext — the runtime needs the real keys. Every response/log path
// that hands a config struct (or a namespace of Config) to a client, a
// browser, or a log line should go through the Redacted() methods below
// instead of hand-masking fields: the field lists live here, next to the
// struct definitions, so a newly added credential field only needs one
// edit. (2026-11-06 review, config.go sensitive-info finding — the seven
// flagged spots map to ProviderConfig, ChannelConfig/AccountConfig,
// SandboxCfg, ObjectStoreS3Cfg, HooksCfg, EmbeddingCfg/RerankerCfg, and
// SkillEntryCfg.)
//
// MaskSecret produces the same "prefix****suffix" shape the setup handlers
// have always used, so existing round-trip guards (isMaskedSecret in
// internal/setup) keep working: an operator-echoed masked value contains
// "****" and is restored from storage instead of overwriting it.

// MaskSecret renders a display-safe form of a credential: the first and
// last 4 characters joined by "****", or "****" wholesale when the value
// is too short to safely show a slice of it.
func MaskSecret(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// envKeyLooksSecret reports whether an env-var / option-map key name looks
// like it carries a credential (KEY / TOKEN / SECRET / PASSWORD /
// PASSWD / CREDENTIAL markers), mirroring the heuristic the setup handlers
// apply to skill env maps.
func envKeyLooksSecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// Redacted returns a copy of pc with the API key masked. Safe for API
// responses and logs; storage must persist the unredacted original.
func (pc ProviderConfig) Redacted() ProviderConfig {
	pc.APIKey = MaskSecret(pc.APIKey)
	return pc
}

// Redacted returns a copy with the tool-provider credential masked.
func (t ToolProviderCfg) Redacted() ToolProviderCfg {
	t.APIKey = MaskSecret(t.APIKey)
	return t
}

// Redacted returns a copy with the webhook bearer token masked.
func (h HooksCfg) Redacted() HooksCfg {
	h.Token = MaskSecret(h.Token)
	return h
}

// Redacted returns a copy with the S3 access/secret keys masked.
func (s3 ObjectStoreS3Cfg) Redacted() ObjectStoreS3Cfg {
	s3.AccessKey = MaskSecret(s3.AccessKey)
	s3.SecretKey = MaskSecret(s3.SecretKey)
	return s3
}

// Redacted returns a copy with the sandbox provider keys (E2B / Boxlite)
// masked.
func (s SandboxCfg) Redacted() SandboxCfg {
	s.E2BKey = MaskSecret(s.E2BKey)
	s.BoxliteKey = MaskSecret(s.BoxliteKey)
	return s
}

// Redacted returns a copy with the embedder credential masked.
func (e EmbeddingCfg) Redacted() EmbeddingCfg {
	e.APIKey = MaskSecret(e.APIKey)
	return e
}

// Redacted returns a copy with the reranker credential masked.
func (r RerankerCfg) Redacted() RerankerCfg {
	r.APIKey = MaskSecret(r.APIKey)
	return r
}

// Redacted returns a copy with this account's bot token, app secret, and
// webhook encryption key masked.
func (a AccountConfig) Redacted() AccountConfig {
	a.BotToken = MaskSecret(a.BotToken)
	a.ClientSecret = MaskSecret(a.ClientSecret)
	a.EncryptKey = MaskSecret(a.EncryptKey)
	return a
}

// Redacted returns a copy with the channel tokens masked, including every
// entry of the Accounts map.
func (c ChannelConfig) Redacted() ChannelConfig {
	c.BotToken = MaskSecret(c.BotToken)
	c.AppToken = MaskSecret(c.AppToken)
	if len(c.Accounts) > 0 {
		accts := make(map[string]AccountConfig, len(c.Accounts))
		for k, v := range c.Accounts {
			accts[k] = v.Redacted()
		}
		c.Accounts = accts
	}
	return c
}

// Redacted returns a copy with the skill credential masked. Env values
// whose key names look secret-bearing (API_KEY, *_TOKEN, …) are masked
// too; non-secret env passthroughs survive so the UI can still render
// them.
func (se SkillEntryCfg) Redacted() SkillEntryCfg {
	se.APIKey = MaskSecret(se.APIKey)
	if len(se.Env) > 0 {
		env := make(map[string]string, len(se.Env))
		for k, v := range se.Env {
			if envKeyLooksSecret(k) {
				env[k] = MaskSecret(v)
			} else {
				env[k] = v
			}
		}
		se.Env = env
	}
	return se
}
