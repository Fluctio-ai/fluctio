package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/skills"
	"github.com/fluctio-ai/fluctio/internal/workspace"
	"gopkg.in/yaml.v3"
)

// Skill represents a discovered skill.
type Skill struct {
	Name        string         // directory name
	Layer       string         // "agent", "user", "managed", "bundled", "extra"
	Content     string         // optional inline SKILL.md content for always-loaded skills
	BaseDir     string         // absolute path to the skill directory
	Description string         // from frontmatter
	Metadata    *SkillMetadata // parsed OpenClaw metadata
	Gated       bool           // true if gating requirements not met
	GateReason  string         // reason gating failed
	OnMissing   string         // frontmatter hint shown next to the unavailable annotation
}

// SkillFrontmatter represents the YAML frontmatter of a SKILL.md file.
//
// Env is the ergonomic shortcut for declaring configurable environment
// variables — equivalent to writing them under metadata.fluctio.env
// but spares skill authors the namespace nesting when they don't need
// to publish their skill to a non-fluctio runtime. The HTTP layer
// merges both sources, top-level Env wins on conflict.
type SkillFrontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Homepage    string         `yaml:"homepage"`
	Env         []SkillEnvSpec `yaml:"env"`
	Metadata    yaml.Node      `yaml:"metadata"`
}

// SkillMetadata represents the skill metadata block.
// Supports both "fluctio" and "openclaw" keys for backward compatibility.
type SkillMetadata struct {
	Fluctio *OpenClawMeta `json:"fluctio"`
	OpenClaw *OpenClawMeta `json:"openclaw"`
}

// Meta returns the effective metadata, preferring fluctio over openclaw.
func (m *SkillMetadata) Meta() *OpenClawMeta {
	if m.Fluctio != nil {
		return m.Fluctio
	}
	return m.OpenClaw
}

// OpenClawMeta holds OpenClaw-specific metadata.
type OpenClawMeta struct {
	Emoji      string         `json:"emoji"`
	Homepage   string         `json:"homepage"`
	Always     bool           `json:"always"`
	OS         []string       `json:"os"`
	Requires   *SkillRequires `json:"requires"`
	PrimaryEnv string         `json:"primaryEnv"`
	// Env declares configurable environment variables this skill reads.
	// Surfaced to the admin UI so operators get labeled inputs (with
	// help text + secret masking) instead of having to grep main.py for
	// os.environ.get() calls. PrimaryEnv stays around for the legacy
	// "single API key" convenience path; multi-provider skills like
	// image-tool list everything here.
	Env     []SkillEnvSpec  `json:"env,omitempty"`
	Install json.RawMessage `json:"install"`
	// OnMissing is an optional frontmatter hint shown next to the
	// "currently unavailable" annotation when a skill is gated. Authors
	// use it to suggest a manual fallback ("use powershell", "install
	// ffmpeg", …). Empty means no fallback surfaced.
	OnMissing string `json:"on_missing"`
}

// SkillEnvSpec describes one configurable env var. All fields except
// Name are optional. Secret defaults to true at the UI layer when the
// name matches /KEY|TOKEN|SECRET|PASSWORD/i so authors usually don't
// have to set it.
//
// Carries both json and yaml tags so it round-trips via the
// metadata.fluctio.env path (yaml→generic→json→struct, json tags) AND
// via the new top-level frontmatter.Env shortcut (yaml→struct directly,
// yaml tags).
type SkillEnvSpec struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty" yaml:"secret,omitempty"`
}

// SkillRequires holds gating requirements.
type SkillRequires struct {
	Bins    []string `json:"bins"`
	AnyBins []string `json:"anyBins"`
	Env     []string `json:"env"`
	Config  []string `json:"config"`
}

// SkillsLoader discovers and merges skills from multiple layers with OpenClaw compatibility.
type SkillsLoader struct {
	homeDir   string
	agentDir  string
	skillsCfg config.SkillsConfig
	globalCfg config.SkillsCfg
	// workspaceStore is optional: when set, LoadSkills hydrates the global
	// and agent skill directories from the object store before scanning the
	// filesystem. Without this, a skill uploaded to the store after a pod's
	// UserSpace was cached is invisible to that pod until restart — and
	// completely invisible on replicas that didn't handle the upload.
	workspaceStore workspace.Store
	agentID        string
}

// NewSkillsLoader creates a new skills loader.
func NewSkillsLoader(homeDir, agentDir string, skillsCfg config.SkillsConfig) *SkillsLoader {
	return &SkillsLoader{
		homeDir:   homeDir,
		agentDir:  agentDir,
		skillsCfg: skillsCfg,
	}
}

// NewSkillsLoaderWithGlobal creates a skills loader with global SkillsCfg for env injection and entries.
func NewSkillsLoaderWithGlobal(homeDir, agentDir string, skillsCfg config.SkillsConfig, globalCfg config.SkillsCfg) *SkillsLoader {
	sl := NewSkillsLoader(homeDir, agentDir, skillsCfg)
	sl.globalCfg = globalCfg
	return sl
}

// WithObjectStore wires a workspace.Store + agentID so LoadSkills hydrates
// skills from the object store before scanning the filesystem. Returns the
// loader for chaining.
func (sl *SkillsLoader) WithObjectStore(ws workspace.Store, agentID string) *SkillsLoader {
	sl.workspaceStore = ws
	sl.agentID = agentID
	return sl
}

// LoadSkills discovers skills from all layers and returns them merged.
// Precedence: agent workspace > managed (host-shared) > extra dirs.
func (sl *SkillsLoader) LoadSkills() []Skill {
	// Mirror object-store skills to the local filesystem so a skill
	// uploaded to OSS (or installed on another replica) is visible here
	// this turn — not on next pod restart. Cheap idempotent hydrate; the
	// store does "skip if size matches" per object.
	if sl.workspaceStore != nil {
		ctx := context.Background()
		managedDir := fluctioManagedDir()
		if managedDir != "" {
			keep := BundledSkillNames()
			if err := skills.HydrateSkillsDown(ctx, sl.workspaceStore, skills.GlobalSkillOwner, managedDir, keep...); err != nil {
				slog.Warn("global skill hydrate failed", "error", err)
			}
		}
		if sl.agentID != "" && sl.agentDir != "" {
			agentSkills := filepath.Join(sl.agentDir, "skills")
			if err := skills.HydrateSkillsDown(ctx, sl.workspaceStore, sl.agentID, agentSkills); err != nil {
				slog.Warn("agent skill hydrate failed", "error", err)
			}
		}
	}

	skillsMap := make(map[string]Skill)

	disabled := make(map[string]bool, len(sl.skillsCfg.Disabled))
	for _, name := range sl.skillsCfg.Disabled {
		disabled[name] = true
	}
	// Also check global entries for enabled: false
	for name, entry := range sl.globalCfg.Entries {
		if !entry.Enabled {
			disabled[name] = true
		}
	}

	// Layer 4 (lowest): extra dirs from config
	for _, dir := range sl.globalCfg.Load.ExtraDirs {
		dir = expandPath(dir)
		for name, skill := range discoverSkillsEnhanced(dir, "extra") {
			if !disabled[name] {
				skillsMap[name] = skill
			}
		}
	}

	// Layer 3: managed skills (~/.fluctio/skills/)
	managedDir := fluctioManagedDir()
	for name, skill := range discoverSkillsEnhanced(managedDir, "managed") {
		if !disabled[name] {
			skillsMap[name] = skill
		}
	}

	// Layer 1 (highest): agent workspace skills
	agentSkillsDir := filepath.Join(sl.agentDir, "skills")
	for name, skill := range discoverSkillsEnhanced(agentSkillsDir, "agent") {
		if !disabled[name] {
			skillsMap[name] = skill
		}
	}

	// Keep gated skills visible in the catalog so the agent can explain
	// missing credentials or platform support instead of claiming the skill
	// is not installed.
	result := make([]Skill, 0, len(skillsMap))
	for _, s := range skillsMap {
		if s.Gated {
			slog.Debug("skill gated", "name", s.Name, "reason", s.GateReason)
		}
		result = append(result, s)
	}
	// Sort by name so the system prompt's skill ordering is stable
	// across turns. Go map iteration is randomized, so without this a
	// 122KB summary ends up with skills in a different position on
	// every refresh — the model is sensitive to where a block sits
	// (later blocks compete with more preceding context for
	// attention), which produced an intermittent "model doesn't see
	// skill X" symptom in long-tail group chats. Alphabetic is the
	// cheapest stable order and also makes log diff'ing trivial.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// BuildSkillsSummary returns the skill section of the system prompt.
// Skills use progressive disclosure by default: keep the prompt's always-on
// context to the small name + description catalog, and let the model call
// load_skill when it needs the full SKILL.md instructions. Explicit
// always-load skills remain inline for compatibility with skills that must be
// present before the first tool call.
func (sl *SkillsLoader) BuildSkillsSummary(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(skillsDirective)
	alwaysLoad := sl.alwaysLoadSet()
	inline := make([]Skill, 0)

	sb.WriteString("\n\n<skill_catalog>\nPre-installed skills available to this agent. Treat any user mention of one of these names as a request to use that skill. Call `load_skill` with the skill name before following its detailed instructions:\n")
	for _, skill := range skills {
		desc := firstSentence(skill.Description)
		if desc == "" {
			desc = "(no description)"
		}
		if skill.Gated {
			if skill.OnMissing != "" {
				fmt.Fprintf(&sb, "- %s — %s (currently unavailable: %s) → fallback: %s\n", skill.Name, desc, skill.GateReason, skill.OnMissing)
			} else {
				fmt.Fprintf(&sb, "- %s — %s (currently unavailable: %s)\n", skill.Name, desc, skill.GateReason)
			}
		} else {
			fmt.Fprintf(&sb, "- %s — %s\n", skill.Name, desc)
		}
		if !skill.Gated && (alwaysLoad[skill.Name] || skillAlwaysLoads(skill)) {
			inline = append(inline, skill)
		}
	}
	sb.WriteString("</skill_catalog>")

	if len(inline) > 0 {
		sb.WriteString("\n\n<always_loaded_skills>\n")
		for _, skill := range inline {
			content := skill.Content
			if content == "" {
				content = loadSkillContent(skill.BaseDir)
			}
			fmt.Fprintf(&sb, "<skill name=%q layer=%q>\n%s\n</skill>\n", skill.Name, skill.Layer, content)
		}
		sb.WriteString("</always_loaded_skills>")
	}
	return sb.String()
}

func (sl *SkillsLoader) alwaysLoadSet() map[string]bool {
	out := make(map[string]bool, len(sl.skillsCfg.AlwaysLoad)+len(sl.globalCfg.AlwaysLoad))
	for _, name := range sl.skillsCfg.AlwaysLoad {
		if name != "" {
			out[name] = true
		}
	}
	for _, name := range sl.globalCfg.AlwaysLoad {
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func skillAlwaysLoads(skill Skill) bool {
	return skill.Metadata != nil && skill.Metadata.Meta() != nil && skill.Metadata.Meta().Always
}

// firstSentence returns the first sentence-ish chunk of s — used for
// the skill-catalog one-liner. We bound the output to keep the catalog
// scannable even when a skill's Description is a paragraph: cut at the
// first ". " / "。" / newline, then hard-cap at 140 runes so a single
// run-on sentence can't drown the index. Trimmed whitespace.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, sep := range []string{"\n", ". ", "。"} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(s)
	const cap = 140
	if r := []rune(s); len(r) > cap {
		s = string(r[:cap]) + "…"
	}
	return s
}

// skillsDirective tells the LLM how to invoke pre-installed skills AND
// what to try before falling back to ad-hoc code when the inline set
// doesn't cover the request. The trigger condition is concrete — "before
// any package install via exec" — rather than abstract ("when no skill
// covers it"), because the abstract framing left the model an easy
// rationalization ("this is one-shot, skip the ladder") that produced
// reflexive `pip install` calls for tasks a published skill would handle.
const skillsDirective = `<skill_usage_rules>
The skills listed below are pre-installed for this agent. Only the catalog is always in context. Before using a skill, call the "load_skill" tool with its name to load the full SKILL.md instructions, then follow those instructions exactly. If an always-loaded skill is included inline below, you may use those inline instructions directly.

The sandbox image already has: python3 + pip, uv + uvx, node + npm + npx, the camoufox-cli anti-detect browser (run as ` + "`camoufox-cli open <url>`" + ` then ` + "`camoufox-cli snapshot -i`" + ` for refs; Camoufox/Firefox is pre-downloaded), git, curl, requests / pillow / beautifulsoup4 / lxml. DO NOT reinstall any of these — wasted tool calls and timeouts. If you see "command not found", check the spelling before reaching for npm/pip.

HTML preview: when the user asks to see / preview a web artifact, write the final HTML into the workspace and tell them the filename — the chat UI auto-renders .html files in a sandboxed iframe (CSS, JS, images, fonts work; cross-origin fetch from null origin does not). For source projects with a package.json (React, Vue, Vite, Next, …), run the project's build first (` + "`pnpm i && pnpm build`" + ` or the documented command) and point at the resulting ` + "`dist/index.html`" + ` (or equivalent). Live dev servers (` + "`vite dev`" + `, ` + "`next dev`" + `, ` + "`npm run dev`" + `) started by hand via exec are NOT reachable from the browser — do not start them that way; they will hang and waste turns. EXCEPTION: if a ` + "`start_app_preview`" + ` tool is listed for you, USE IT for web-app projects (anything with a package.json) — it boots the dev server in a managed runtime and returns a live preview URL that hot-reloads as you edit files, so you do NOT need ` + "`pnpm build`" + ` or the dist/index.html route. Call it once early, then just edit files.

Building a web app, website, landing page, or dashboard — including "use template X to make Y" / "用某模板做个…": if a ` + "`start_app_preview`" + ` tool is in your tool list, that is your PRIMARY path. Call it FIRST to scaffold + serve the project and get a preview URL, then edit files. The template name (e.g. "shipany") is a SCAFFOLD TARGET, not a skill — do NOT run find-skills / ` + "`npx skills find`" + ` and do NOT hand-start a dev server for this.

When the listed skills don't cover what the user asked for, follow this order BEFORE running any package install (pip / npm / apt / brew / cargo / gem / go install / …) via exec:

1. If a "find-skills" skill is listed above, run it FIRST to search the open skill ecosystem. If a credible match exists, surface it and offer to install it instead of installing the package yourself.
2. If no published skill fits, use "skill-creator" (if listed) to scaffold a new skill under skills/<name>/, then invoke it. Prefer this over inline scripts whenever the user might ask the same kind of thing again.
3. Only if find-skills found nothing AND skill-creator isn't appropriate (e.g. truly one-time throwaway like printing the date), fall through to the direct package install.

Skipping step 1 to "save time" is not allowed — it costs one tool call and prevents reinventing wheels the community has already published.
</skill_usage_rules>`

// SkillEnvVars returns environment variables for a specific skill from global config.
func (sl *SkillsLoader) SkillEnvVars(skillName string) map[string]string {
	// Per-agent override wins. Fall back to the global entry only when
	// the agent doesn't have its own row OR has it but it's empty (so
	// the operator doesn't have to copy the global config to every
	// agent just to keep the same defaults).
	var entry config.SkillEntryCfg
	var found bool
	if sl.agentID != "" {
		if agentMap, ok := sl.globalCfg.AgentEntries[sl.agentID]; ok {
			if e, ok := agentMap[skillName]; ok && (e.APIKey != "" || len(e.Env) > 0) {
				entry = e
				found = true
			}
		}
	}
	if !found {
		entry, found = sl.globalCfg.Entries[skillName]
	}
	slog.Info("SkillEnvVars lookup",
		"skillName", skillName,
		"loaderAgentID", sl.agentID,
		"agentEntriesKeys", mapKeys(sl.globalCfg.AgentEntries),
		"entriesKeys", entryKeys(sl.globalCfg.Entries),
		"found", found,
		"entryEnvCount", len(entry.Env))
	if !found {
		return nil
	}
	env := make(map[string]string, len(entry.Env)+1)
	for k, v := range entry.Env {
		env[k] = v
	}
	// If apiKey is set and the skill has a primaryEnv, inject it
	if entry.APIKey != "" {
		// Find the skill to get primaryEnv
		// This is a convenience — the skill's primaryEnv tells us which env var the apiKey maps to
		for _, dir := range sl.allSkillDirs() {
			skillDir := filepath.Join(dir, skillName)
			fm := parseFrontmatter(filepath.Join(skillDir, "SKILL.md"))
			if fm != nil && fm.Metadata.Kind == yaml.MappingNode {
				meta := parseMetadata(&fm.Metadata)
				if meta != nil && meta.Meta() != nil && meta.Meta().PrimaryEnv != "" {
					env[meta.Meta().PrimaryEnv] = entry.APIKey
					break
				}
			}
		}
	}
	return env
}

// AllSkillDirs returns all skill directories in precedence order.
func (sl *SkillsLoader) AllSkillDirs() []string {
	return sl.allSkillDirs()
}

func (sl *SkillsLoader) allSkillDirs() []string {
	var dirs []string
	dirs = append(dirs, filepath.Join(sl.agentDir, "skills"))
	dirs = append(dirs, fluctioManagedDir())
	dirs = append(dirs, sl.globalCfg.Load.ExtraDirs...)
	return dirs
}

// discoverSkillsEnhanced scans a directory for skill subdirectories with SKILL.md,
// parses frontmatter, and applies gating. It deliberately does not keep the
// full SKILL.md body in memory for default skills; the model loads that body
// on demand through load_skill.
func discoverSkillsEnhanced(dir string, layer string) map[string]Skill {
	result := make(map[string]Skill)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillDir, "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}

		absDir, _ := filepath.Abs(skillDir)

		// Parse frontmatter
		fm := parseFrontmatterFromBytes(data)
		var meta *SkillMetadata
		var desc string
		if fm != nil {
			desc = fm.Description
			if fm.Metadata.Kind == yaml.MappingNode {
				meta = parseMetadata(&fm.Metadata)
			}
		}

		// Apply gating
		gated, gateReason := CheckGating(meta)

		name := entry.Name()
		if fm != nil && fm.Name != "" {
			// Use directory name as the key, but store the frontmatter name
			_ = fm.Name
		}

		// Surface on_missing hint when the author declared one.
		onMissing := ""
		if meta != nil && meta.Meta() != nil {
			onMissing = meta.Meta().OnMissing
		}

		result[name] = Skill{
			Name:        name,
			Layer:       layer,
			BaseDir:     absDir,
			Description: desc,
			Metadata:    meta,
			Gated:       gated,
			GateReason:  gateReason,
			OnMissing:   onMissing,
		}
	}

	return result
}

func loadSkillContent(skillDir string) string {
	data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	return strings.ReplaceAll(content, "{baseDir}", skillDir)
}

func mapKeys(m map[string]map[string]config.SkillEntryCfg) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func entryKeys(m map[string]config.SkillEntryCfg) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// SplitSkillFrontmatter is the exported entrypoint used by the HTTP
// layer when it needs both the parsed frontmatter and the raw body
// (e.g. to fall back to the first body line for the description on
// frontmatter-less skills). Returns nil + raw input when there is no
// `---` frontmatter to parse.
func SplitSkillFrontmatter(data []byte) (*SkillFrontmatter, string) {
	fm := parseFrontmatterFromBytes(data)
	body := string(data)
	if fm == nil {
		return nil, body
	}
	// Strip the frontmatter block from the body so callers don't see the
	// YAML lines when scanning for the first prose line.
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "---") {
		rest := trimmed[3:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			after := rest[end+len("\n---"):]
			body = strings.TrimLeft(after, "\n")
		}
	}
	return fm, body
}

// ParseSkillMetadata is the exported wrapper around the (yaml.Node →
// SkillMetadata) decode path. The HTTP skill list handler uses it to
// surface envSpec to the admin UI.
func ParseSkillMetadata(node *yaml.Node) *SkillMetadata {
	return parseMetadata(node)
}

// parseFrontmatter reads and parses YAML frontmatter from a SKILL.md file path.
func parseFrontmatter(path string) *SkillFrontmatter {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseFrontmatterFromBytes(data)
}

// parseFrontmatterFromBytes parses YAML frontmatter from raw bytes.
func parseFrontmatterFromBytes(data []byte) *SkillFrontmatter {
	content := string(data)

	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return nil
	}

	// Find opening and closing ---
	trimmed := strings.TrimSpace(content)
	rest := trimmed[3:] // skip first ---
	endIdx := strings.Index(rest, "\n---")
	if endIdx < 0 {
		return nil
	}

	fmStr := rest[:endIdx]

	var fm SkillFrontmatter
	if err := yaml.Unmarshal([]byte(fmStr), &fm); err != nil {
		return nil
	}
	return &fm
}

// parseMetadata converts the yaml.Node metadata into our SkillMetadata struct.
func parseMetadata(node *yaml.Node) *SkillMetadata {
	if node == nil || node.Kind == 0 {
		return nil
	}
	// Marshal back to YAML then decode as JSON-like structure
	yamlBytes, err := yaml.Marshal(node)
	if err != nil {
		return nil
	}

	// Unmarshal YAML into a generic map, then marshal to JSON, then unmarshal to struct
	var raw interface{}
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		return nil
	}

	jsonBytes, err := json.Marshal(convertYAMLToJSON(raw))
	if err != nil {
		return nil
	}

	var meta SkillMetadata
	if err := json.Unmarshal(jsonBytes, &meta); err != nil {
		return nil
	}
	return &meta
}

// convertYAMLToJSON converts YAML map[string]interface{} (which uses map[interface{}]interface{})
// to JSON-compatible map[string]interface{}.
func convertYAMLToJSON(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[k] = convertYAMLToJSON(v)
		}
		return result
	case map[interface{}]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[fmt.Sprint(k)] = convertYAMLToJSON(v)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, v := range val {
			result[i] = convertYAMLToJSON(v)
		}
		return result
	default:
		return v
	}
}

// CheckGating validates whether a skill's requirements are met.
// Returns (gated, reason). gated=true means the skill's requirements are
// not satisfied on the current host. Exported so the load_skill tool can
// re-evaluate gating on demand against the same engine (Task 4).
func CheckGating(meta *SkillMetadata) (bool, string) {
	if meta == nil || meta.Meta() == nil {
		return false, ""
	}
	oc := meta.Meta()

	if oc.Always {
		return false, ""
	}

	// Check OS requirement
	if len(oc.OS) > 0 {
		currentOS := runtime.GOOS
		found := false
		for _, os := range oc.OS {
			if os == currentOS {
				found = true
				break
			}
		}
		if !found {
			return true, fmt.Sprintf("requires OS %v, current is %s", oc.OS, currentOS)
		}
	}

	if oc.Requires == nil {
		return false, ""
	}

	// Check required binaries. binAvailable resolves PATH the SAME way the
	// exec tool does (cmd /C where on Windows, LookPath elsewhere) so gating
	// agrees with actual exec capability — without this, Windows gateway
	// processes launched with a trimmed PATH would gate skills whose bins
	// actually resolve fine through the system PATH the exec tool uses.
	for _, bin := range oc.Requires.Bins {
		if !binAvailable(bin) {
			return true, fmt.Sprintf("required binary %q not found on PATH", bin)
		}
	}

	// Check anyBins (at least one must exist)
	if len(oc.Requires.AnyBins) > 0 {
		found := false
		for _, bin := range oc.Requires.AnyBins {
			if binAvailable(bin) {
				found = true
				break
			}
		}
		if !found {
			return true, fmt.Sprintf("none of required binaries %v found on PATH", oc.Requires.AnyBins)
		}
	}

	// Check required env vars
	for _, envVar := range oc.Requires.Env {
		if os.Getenv(envVar) == "" {
			return true, fmt.Sprintf("required env var %q not set", envVar)
		}
	}

	return false, ""
}

// binAvailable reports whether the given binary can be resolved on PATH by
// the exec tool. The exec tool runs commands via `cmd /C <cmd>` on Windows
// and `sh -c <cmd>` on Linux/macOS, which inherit the SYSTEM PATH — but the
// gateway Go process may have been launched with a different PATH (e.g. a
// trimmed dev-shell env). So on Windows we mirror the exec tool by going
// through `cmd /C where <bin>`; on other platforms the process PATH is
// normally the system PATH and exec.LookPath is sufficient.
//
// Gating must use this helper (not exec.LookPath directly) so CheckGating
// agrees with what the exec tool can actually run: without it, a Windows
// gateway missing msys/Git-Bash dirs on its process PATH would gate every
// skill that requires `bash` even though the exec tool can run `bash` fine
// through cmd /C.
func binAvailable(bin string) bool {
	if runtime.GOOS == "windows" {
		// cmd /C where uses the SYSTEM PATH (what cmd.exe would see), matching
		// the exec tool's `cmd /C <cmd>` resolution. Errors or non-zero exit
		// both mean "not available"; we don't care about stdout.
		cmd := exec.Command("cmd", "/C", "where", bin)
		return cmd.Run() == nil
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// fluctioBaseDir returns $FLUCTIO_HOME or $HOME/.fluctio. Used as
// the parent for skills/, users/<uid>/skills/, etc. Honors FLUCTIO_HOME
// so multi-instance dev (one stack per product) stays isolated.
func fluctioBaseDir() string {
	if h := os.Getenv("FLUCTIO_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fluctio")
}

// fluctioManagedDir returns the Fluctio managed skills directory
// (~/.fluctio/skills/, host-shared).
func fluctioManagedDir() string {
	base := fluctioBaseDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "skills")
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// FindSkillForPath returns the skill name if the given path is within a skill directory.
func FindSkillForPath(path string, skillDirs []string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	for _, dir := range skillDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if strings.HasPrefix(absPath, absDir+string(filepath.Separator)) {
			// Extract skill name (first component after the skills dir)
			rel, err := filepath.Rel(absDir, absPath)
			if err != nil {
				continue
			}
			parts := strings.SplitN(rel, string(filepath.Separator), 2)
			if len(parts) > 0 {
				return parts[0]
			}
		}
	}
	return ""
}
