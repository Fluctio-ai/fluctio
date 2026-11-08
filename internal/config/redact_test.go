package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// Guard: every Redacted() method must strip its credential fields. The
// test walks the known-credential field list (the audit's seven spots),
// redacts, marshals, and asserts none of the planted secrets survive —
// while non-secret fields (apiBase, models, enabled, …) do.
func TestRedactedStripsCredentials(t *testing.T) {
	const (
		provKey   = "sk-ant-api03-aaaaaaaaaaaa-REAL"
		toolKey   = "exa-live-0123456789abcdef"
		hooksTok  = "hooktok-0123456789abcdef"
		accessKey = "AKIAIOSFODNN7REAL"
		secretKey = "s3secret0123456789abcdef"
		e2b       = "e2b_live_0123456789abcdef"
		boxlite   = "boxlite_live_0123456789abcdef"
		embKey    = "emb-live-0123456789abcdef"
		rerKey    = "rer-live-0123456789abcdef"
		botTok    = "123456:ABC-DEF_real_token"
		appTok    = "xapp-1-REAL-0123456789"
		acctTok   = "acct-bot-token-0123456789"
		qqSecret  = "qq-client-secret-0123456789"
		encKey    = "feishu-encrypt-key-0123456"
		skillKey  = "skill-live-0123456789abcdef"
		envSecret = "env-secret-0123456789abcdef"
	)
	secrets := []string{
		provKey, toolKey, hooksTok, accessKey, secretKey, e2b, boxlite,
		embKey, rerKey, botTok, appTok, acctTok, qqSecret, encKey,
		skillKey, envSecret,
	}

	pc := ProviderConfig{
		APIKey:  provKey,
		APIBase: "https://api.example.com/v1",
		APIType: "openai",
	}.Redacted()

	tool := ToolProviderCfg{APIKey: toolKey, Endpoint: "https://exa.example"}.Redacted()

	hooks := HooksCfg{Enabled: true, Token: hooksTok, Port: 8080}.Redacted()

	s3 := ObjectStoreS3Cfg{
		Endpoint:  "http://minio.local:9000",
		AccessKey: accessKey,
		SecretKey: secretKey,
		Bucket:    "fluctio",
		UseSSL:    true,
	}.Redacted()

	sandbox := SandboxCfg{
		Enabled:         true,
		Backend:         "boxlite",
		E2BKey:          e2b,
		BoxliteKey:      boxlite,
		BoxliteSnapshot: "snap-1",
	}.Redacted()

	emb := EmbeddingCfg{Enabled: true, APIKey: embKey, Model: "bge-m3"}.Redacted()
	rer := RerankerCfg{Enabled: true, APIKey: rerKey, Model: "bge-reranker"}.Redacted()

	channels := map[string]ChannelConfig{
		"telegram": {
			Enabled:  true,
			BotToken: botTok,
			AppToken: appTok,
			Accounts: map[string]AccountConfig{
				"qq-main": {BotToken: acctTok, ClientSecret: qqSecret, EncryptKey: encKey, AppID: "111222333"},
			},
		},
	}
	redactedChannels := map[string]ChannelConfig{}
	for k, v := range channels {
		redactedChannels[k] = v.Redacted()
	}

	skill := SkillEntryCfg{
		Enabled: true,
		APIKey:  skillKey,
		Env: map[string]string{
			"OPENAI_API_KEY": envSecret,
			"DEBUG":          "1",
		},
	}.Redacted()

	blobs := map[string][]byte{
		"provider":       mustJSON(t, pc),
		"toolProvider":   mustJSON(t, tool),
		"hooks":          mustJSON(t, hooks),
		"s3":             mustJSON(t, s3),
		"sandbox":        mustJSON(t, sandbox),
		"embedding":      mustJSON(t, emb),
		"reranker":       mustJSON(t, rer),
		"channels":       mustJSON(t, redactedChannels),
		"skillEntry":     mustJSON(t, skill),
	}
	for name, blob := range blobs {
		for _, secret := range secrets {
			if strings.Contains(string(blob), secret) {
				t.Errorf("%s: redacted output still contains plaintext secret %q", name, secret)
			}
		}
	}

	// Non-secret fields must survive redaction (the UI still renders them).
	assertJSONField(t, blobs["provider"], "apiBase", "https://api.example.com/v1")
	assertJSONField(t, blobs["provider"], "apiType", "openai")
	assertJSONField(t, blobs["sandbox"], "boxliteSnapshot", "snap-1")
	assertJSONField(t, blobs["s3"], "bucket", "fluctio")
	assertJSONField(t, blobs["embedding"], "model", "bge-m3")

	// Skill env passthrough (non-secret key) must survive.
	if got := skill.Env["DEBUG"]; got != "1" {
		t.Errorf("skill env non-secret value = %q, want \"1\"", got)
	}
	if got := skill.Env["OPENAI_API_KEY"]; !isMasked(got) {
		t.Errorf("skill env secret value = %q, want masked", got)
	}
}

// Guard: MaskSecret must keep the "****" sentinel so round-trip guards
// can recognize a masked echo, and must not leak short keys by slicing.
func TestMaskSecret(t *testing.T) {
	if got := MaskSecret("sk-1234567890abcdef"); !strings.Contains(got, "****") {
		t.Errorf("MaskSecret(long) = %q, want **** sentinel", got)
	}
	if got := MaskSecret("sk-1234567890abcdef"); strings.Contains(got, "sk-1234567890abcdef") {
		t.Errorf("MaskSecret(long) = %q, leaks the full key", got)
	}
	if got := MaskSecret("short"); got != "****" {
		t.Errorf("MaskSecret(short) = %q, want wholesale ****", got)
	}
	if got := MaskSecret(""); got != "****" {
		t.Errorf("MaskSecret(empty) = %q, want ****", got)
	}
	// 9-char key: sliceable, but the output must still hide the middle.
	if got := MaskSecret("123456789"); got != "1234****6789" {
		t.Errorf("MaskSecret(9 chars) = %q, want 1234****6789", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return blob
}

func assertJSONField(t *testing.T, blob []byte, field, want string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := m[field].(string); got != want {
		t.Errorf("field %s = %q, want %q", field, got, want)
	}
}

func isMasked(s string) bool {
	return strings.Contains(s, "****")
}
