package config

import "testing"

// TestAgentNotifyCfgKindEnabled guards the notification gate the sweeps and
// the chat-reply mirror share. The default must be "deliver": a nil
// receiver, an unset pointer and an unrecognized kind all return true, or
// a config typo would silently mute the phone.
func TestAgentNotifyCfgKindEnabled(t *testing.T) {
	f := false
	tr := true
	cases := []struct {
		name string
		cfg  *AgentNotifyCfg
		kind string
		want bool
	}{
		{"nil cfg", nil, "chat_reply", true},
		{"nil cfg unknown kind", nil, "whatever", true},
		{"empty cfg", &AgentNotifyCfg{}, "todo_reminder", true},
		{"unrecognized kind", &AgentNotifyCfg{ChatReply: &f}, "not_a_kind", true},
		{"chat_reply off", &AgentNotifyCfg{ChatReply: &f}, "chat_reply", false},
		{"chat_reply unchanged by im toggle", &AgentNotifyCfg{IMReply: &f}, "chat_reply", true},
		{"im_reply off", &AgentNotifyCfg{IMReply: &f}, "im_reply", false},
		{"todo off", &AgentNotifyCfg{TodoReminder: &f}, "todo_reminder", false},
		{"cards off", &AgentNotifyCfg{CardsDigest: &f}, "cards_digest", false},
		{"shelved off", &AgentNotifyCfg{ShelvedNudge: &f}, "shelved_nudge", false},
		{"blindspot off", &AgentNotifyCfg{Blindspot: &f}, "blindspot", false},
		{"explicit on", &AgentNotifyCfg{TodoReminder: &tr}, "todo_reminder", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.KindEnabled(tc.kind); got != tc.want {
				t.Fatalf("KindEnabled(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}
