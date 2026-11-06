package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

type setPreferenceArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// RegisterPreferenceTool registers set_preference — lets the chatter
// configure settings (API keys, preferences, etc.) through
// conversation. Preferences are per-agent runtime settings: both the
// read and the write side use the agent-level configs row
// (user_id='', agent_id=<agent>), the same row scope.Timezone and the
// other scope.Setting readers resolve. The chatter's identity is still
// required at execute time (via r.EffectiveUserID()) as an auth gate.
//
// The preference is stored under the "prefs" namespace in configs,
// the same namespace as timezone. System-scope prefs act as the
// fallback the agent-level row overrides.
func RegisterPreferenceTool(r *Registry, st store.Store) {
	r.Register("set_preference",
		"Save a preference or API key for this agent. "+
			"Use this when the user wants to configure something that should persist across conversations — "+
			"for example their timezone, language, an API key for image generation, drawing style, etc. "+
			"The preference is scoped to this agent and persists for everyone chatting with it.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"key": map[string]interface{}{
					"type":        "string",
					"description": "The preference key, e.g. 'timezone', 'language', 'replicate_api_token', 'drawing_style'. Use snake_case.",
				},
				"value": map[string]interface{}{
					"type":        "string",
					"description": "The preference value to store.",
				},
			},
			"required": []string{"key", "value"},
		},
		makeSetPreference(st, r),
	)
}

func makeSetPreference(st store.Store, r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args setPreferenceArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Key == "" {
			return "", fmt.Errorf("key is required")
		}
		if args.Value == "" {
			return "", fmt.Errorf("value is required")
		}

		chatterUID := r.EffectiveUserID()
		if chatterUID == "" {
			return "", fmt.Errorf("no chatter identity on this turn — cannot persist preference")
		}
		agentID := r.AgentID()
		if agentID == "" {
			return "", fmt.Errorf("no agent identity — cannot persist preference")
		}

		// Read existing prefs at agent scope (the row GetConfigByName
		// resolves for this agent), merge the new key.
		data := map[string]interface{}{}
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, agentID, scope.PrefsNamespace); err == nil && rec != nil {
			for k, v := range rec.Data {
				data[k] = v
			}
		}
		data[args.Key] = args.Value

		// Write at agent scope (userID='') to match the read side. The
		// read side is authoritative: preference lookups (scope.Timezone,
		// the merge-read above) resolve agent-level rows only, so the
		// previous SaveSetting(chatterUID, agentID, ...) landed on a
		// (chatter, agent) row no reader ever walked — set_preference
		// silently lost every save while the merge-read kept returning
		// the untouched agent row.
		if err := scope.SaveSetting(ctx, st, "", agentID, scope.PrefsNamespace, data); err != nil {
			return "", fmt.Errorf("save preference: %w", err)
		}
		return fmt.Sprintf("Preference saved: %s = %s (scoped to this agent).", args.Key, args.Value), nil
	}
}
