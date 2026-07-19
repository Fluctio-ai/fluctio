package config

import "testing"

// 假设测试环境干净（CI 通常如此）。t.Setenv 设的变量在测试后自动 restore。

func TestLoadOwnerFromEnv_OwnerJSONTakesPrecedence(t *testing.T) {
	// 设了 OWNER_JSON，独立变量应被忽略
	t.Setenv("FLUCTIO_OWNER_JSON", `{"username":"alice","password":"secret","email":"a@x.com","displayName":"Al"}`)
	t.Setenv("FLUCTIO_USERNAME", "bob")   // 应被忽略
	t.Setenv("FLUCTIO_PASSWORD", "other") // 应被忽略

	cfg, err := LoadOwnerFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg, got nil")
	}
	if cfg.Username != "alice" {
		t.Errorf("Username = %q, want alice (OWNER_JSON wins)", cfg.Username)
	}
	if cfg.Password != "secret" {
		t.Errorf("Password = %q, want secret", cfg.Password)
	}
	if cfg.Email != "a@x.com" || cfg.DisplayName != "Al" {
		t.Errorf("Email/DisplayName = %q/%q, want a@x.com/Al", cfg.Email, cfg.DisplayName)
	}
}

func TestLoadOwnerFromEnv_IndividualVarsWhenNoJSON(t *testing.T) {
	t.Setenv("FLUCTIO_USERNAME", "carol")
	t.Setenv("FLUCTIO_PASSWORD", "pw")
	t.Setenv("FLUCTIO_EMAIL", "carol@example.com")
	t.Setenv("FLUCTIO_DISPLAY_NAME", "Carol")

	cfg, err := LoadOwnerFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg, got nil")
	}
	if cfg.Username != "carol" || cfg.Password != "pw" ||
		cfg.Email != "carol@example.com" || cfg.DisplayName != "Carol" {
		t.Errorf("cfg = %+v, want all carol fields", cfg)
	}
}

func TestLoadOwnerFromEnv_NothingSetReturnsNil(t *testing.T) {
	cfg, err := LoadOwnerFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected (nil, nil) when no env set, got cfg=%+v", cfg)
	}
}

func TestLoadOwnerFromEnv_InvalidJSONReturnsError(t *testing.T) {
	t.Setenv("FLUCTIO_OWNER_JSON", `{not valid json`)

	cfg, err := LoadOwnerFromEnv()
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if cfg != nil {
		t.Errorf("expected nil cfg on error, got %+v", cfg)
	}
}

func TestLoadOwnerFromEnv_PartialIndividualVars(t *testing.T) {
	// 只设 username，其他不设
	t.Setenv("FLUCTIO_USERNAME", "dave")

	cfg, err := LoadOwnerFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg (username set), got nil")
	}
	if cfg.Username != "dave" {
		t.Errorf("Username = %q, want dave", cfg.Username)
	}
	if cfg.Password != "" || cfg.Email != "" || cfg.DisplayName != "" {
		t.Errorf("expected empty optional fields, got pw=%q email=%q dn=%q",
			cfg.Password, cfg.Email, cfg.DisplayName)
	}
}

func TestLoadOwnerFromEnv_EmptyJSONFallsThroughToIndividual(t *testing.T) {
	// OWNER_JSON 设了但是空串 → 应跳过，走独立变量
	t.Setenv("FLUCTIO_OWNER_JSON", "")
	t.Setenv("FLUCTIO_USERNAME", "eve")

	cfg, err := LoadOwnerFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil || cfg.Username != "eve" {
		t.Errorf("expected eve from individual vars, got %+v", cfg)
	}
}
