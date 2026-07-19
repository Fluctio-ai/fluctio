package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// OwnerConfig is the single-user identity declared in <home>/owner.json.
// When the file is present the gateway enforces this as the only super-admin
// account at boot — upserting the owner and merging any surplus account's
// data into it — so the platform runs single-user without relying on the
// onboarding/register flow or the users table being mutated at runtime.
type OwnerConfig struct {
	Username    string `json:"username"`
	Email       string `json:"email,omitempty"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`
}

// LoadOwnerFile reads <home>/owner.json. Returns (nil, nil) when the file is
// absent — single-user enforcement is opt-in via the file's presence, so
// existing deployments that don't create it keep their current behavior.
func LoadOwnerFile(home string) (*OwnerConfig, error) {
	if home == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(home, "owner.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var o OwnerConfig
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// LoadOwnerFromEnv reads the owner identity from environment variables.
// Priority: FLUCTIO_OWNER_JSON (a JSON string of OwnerConfig shape) > four
// individual vars (FLUCTIO_USERNAME / _PASSWORD / _EMAIL / _DISPLAY_NAME).
// When FLUCTIO_OWNER_JSON is set to a non-empty value, the individual vars
// are ignored (no cross-override).
//
// Returns (nil, nil) when none of the owner env vars are set — single-user
// enforcement stays opt-in, callers fall back to owner.json or skip.
func LoadOwnerFromEnv() (*OwnerConfig, error) {
	if raw, ok := os.LookupEnv("FLUCTIO_OWNER_JSON"); ok && raw != "" {
		var o OwnerConfig
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			return nil, fmt.Errorf("parse FLUCTIO_OWNER_JSON: %w", err)
		}
		return &o, nil
	}
	cfg := OwnerConfig{
		Username:    envOrEmpty("FLUCTIO_USERNAME"),
		Password:    envOrEmpty("FLUCTIO_PASSWORD"),
		Email:       envOrEmpty("FLUCTIO_EMAIL"),
		DisplayName: envOrEmpty("FLUCTIO_DISPLAY_NAME"),
	}
	if cfg.Username == "" && cfg.Password == "" &&
		cfg.Email == "" && cfg.DisplayName == "" {
		return nil, nil
	}
	return &cfg, nil
}

// envOrEmpty returns the env var value or "" when unset. LookupEnv is used
// (not Getenv) so the "set but empty" and "unset" cases behave the same —
// both contribute "" to the OwnerConfig, which is what the empty-check above
// expects.
func envOrEmpty(key string) string {
	v, _ := os.LookupEnv(key)
	return v
}
