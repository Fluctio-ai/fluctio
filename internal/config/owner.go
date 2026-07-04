package config

import (
	"encoding/json"
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
