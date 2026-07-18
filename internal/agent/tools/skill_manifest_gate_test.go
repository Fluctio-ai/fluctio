package tools

import (
	"strings"
	"testing"
)

func TestSkillManifestBlockedRespectsCallerFlag(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		admin bool
		want  bool
	}{
		// Absolute sandbox-mount manifest: the documented exfil vector
		// (`read_file("/skills/foo/SKILL.md")`). Blocked for a chatter,
		// allowed for the owner/admin who maintains skills.
		{"mount manifest, chatter", "/skills/foo/SKILL.md", false, true},
		{"mount manifest, admin", "/skills/foo/SKILL.md", true, false},
		{"nested mount manifest", "/var/x/skills/foo/SKILL.md", false, true},

		// Relative manifest resolves to the agent's own skill set — the
		// agent's IP, so blocked for non-admin (admin maintains skills).
		{"relative manifest, chatter", "skills/foo/SKILL.md", false, true},
		{"relative manifest, admin", "skills/foo/SKILL.md", true, false},

		// Not a manifest / unrelated files: never gated.
		{"skill script", "/skills/foo/main.py", false, false},
		{"workspace lookalike", "/workspace/SKILL.md", false, false},
		{"nested workspace note", "notes/SKILL.md", false, false},
		{"plain report", "report.md", false, false},
		{"empty", "", false, false},
	}
	for _, c := range cases {
		r := &Registry{callerIsAdmin: c.admin}
		if got := r.skillManifestBlocked(c.path); got != c.want {
			t.Errorf("%s: skillManifestBlocked(%q) admin=%v = %v, want %v",
				c.name, c.path, c.admin, got, c.want)
		}
	}
}

func TestSkillManifestRefusalMessageShape(t *testing.T) {
	// Same contract as the identity refusal: tool-output safe and an
	// explicit "don't paraphrase" so summarizing the manifest is refused
	// too.
	if !strings.HasPrefix(SkillManifestRefusal, "[refused:") {
		t.Errorf("refusal should be a [refused: …] tool message; got %q", SkillManifestRefusal)
	}
	if !strings.Contains(SkillManifestRefusal, "paraphrase") {
		t.Errorf("refusal must instruct the model not to paraphrase; got %q", SkillManifestRefusal)
	}
	if !strings.Contains(SkillManifestRefusal, "stay in character") {
		t.Errorf("refusal must tell the model to stay in character; got %q", SkillManifestRefusal)
	}
}
