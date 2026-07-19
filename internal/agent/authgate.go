package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// AuthMode constants for the session-scoped authorization mode.
const (
	AuthModeAsk  = "ask"  // prompt user on outside-workspace writes (default)
	AuthModeAuto = "auto" // deny outside-workspace writes without prompting
	AuthModeYolo = "yolo" // allow everything
)

// authPolicyFile is the per-agent allowlist. allowWrite entries are path
// prefixes RELATIVE to the agent root (~/.fluctio/agents/<id>/) and MUST
// resolve under it — entries pointing outside are silently dropped, which
// enforces "one agent can't authorize paths for another".
type authPolicyFile struct {
	AllowWrite []string `json:"allowWrite,omitempty"`
}

// authGate decides whether a filesystem write is allowed, denied, or needs
// a user prompt — based on the session mode, the workspace boundary, and
// the agent's allowlist (preset dirs + policy.json). One gate per agent,
// loaded once and cached; policy.json edits require an agent reload.
type authGate struct {
	agentRoot string // ~/.fluctio/agents/<id>/  (allowlist entries resolve under here)
	workspace string // absolute workspace path (inside-workside writes are free)
	sandboxed bool   // when true: dangerous cmds + workspace boundary → allow (container = boundary)

	mu         sync.RWMutex
	allowWrite []string // absolute path prefixes that are pre-authorized
}

// setSandboxed flips the sandbox-aware gating. Called by SetSandboxPool when
// the executor pool is wired or unwired post-construction.
func (g *authGate) setSandboxed(v bool) {
	g.mu.Lock()
	g.sandboxed = v
	g.mu.Unlock()
}

// newAuthGate builds a gate for the given agent. allowWrite prefixes are
// resolved immediately so checks are pure prefix comparisons at runtime.
func newAuthGate(agentRoot, workspace string) *authGate {
	g := &authGate{agentRoot: agentRoot, workspace: workspace}
	g.reload()
	return g
}

// reload re-reads policy.json and merges preset dirs. Safe to call anytime;
// callers that edit policy.json invoke this to pick up changes.
func (g *authGate) reload() {
	prefixes := []string{}
	// policy.json user-configured allowWrite, constrained to agentRoot.
	if data, err := os.ReadFile(filepath.Join(g.agentRoot, "policy.json")); err == nil {
		var pf authPolicyFile
		if json.Unmarshal(data, &pf) == nil {
			for _, rel := range pf.AllowWrite {
				abs := filepath.Join(g.agentRoot, filepath.Clean("/"+rel))
				if isUnder(abs, g.agentRoot) {
					prefixes = append(prefixes, abs)
				}
			}
		}
	}
	g.mu.Lock()
	g.allowWrite = prefixes
	g.mu.Unlock()
}

// authDecision is the gate's verdict on a single tool call.
type authDecision struct {
	// action: allow (run it), block (refuse, un-approvable), or prompt
	// (outside-workspace/dangerous in ask mode — needs user /yes).
	action authAction
	// reason is the human-readable explanation shown to the user/LLM
	// when the call is blocked or prompted.
	reason string
}

type authAction int

const (
	authAllow  authAction = iota // run the tool
	authBlock                    // refuse unconditionally (hardline)
	authPrompt                   // outside-workspace/dangerous, ask mode wants /yes
)

// fileWriteTools are the built-in tools that mutate the filesystem.
var fileWriteTools = map[string]bool{
	"write_file":     true,
	"create_file":    true,
	"str_replace":    true,
	"insert":         true,
	"apply_patch":    true,
	"delete_file":    true,
	"file_edit":      true,
	"move_file":      true,
}

// execTools run shell commands — they go through command classification
// (hardline/dangerous) plus the workspace-boundary rule (anything not
// obviously inside-workspace is gated in ask/auto).
var execTools = map[string]bool{
	"exec":      true,
	"host_exec": true,
	"bash":      true,
}

// evaluateCall classifies a tool call against the three tiers and the
// session mode. It does NOT touch the session or emit anything — the
// caller handles prompting and single-use authorization. JSON unmarshal
// failures fall through to authAllow (the tool itself will report the
// bad args), so a malformed payload can't lock the agent out.
func (g *authGate) evaluateCall(toolName, argsJSON, mode string) authDecision {
	g.mu.RLock()
	sandboxed := g.sandboxed
	g.mu.RUnlock()

	// exec-family tools: hardline + dangerous + safe whitelist + workspace boundary.
	if execTools[toolName] {
		command := extractStringArg(argsJSON, "command")
		if command != "" {
			tier, desc := classifyCommand(command)
			if tier == tierHardline {
				return authDecision{action: authBlock, reason: "hardline: " + desc}
			}
			// Safe whitelist: read-only / query commands auto-allow even in
			// ask/auto mode. Must run AFTER hardline (a `safe` verb can never
			// bypass the hardline floor) but BEFORE dangerous so that a
			// command like `git status` doesn't trip a dangerous pattern.
			// commandSafeTier enforces pure-single-command (no shell
			// operators) + a safePattern match.
			if commandSafeTier(command) {
				return authDecision{action: authAllow}
			}
			if tier == tierDangerous && !sandboxed {
				return modeGate(mode, "dangerous command: "+desc)
			}
		}
		// exec commands that didn't trip a pattern are still subject to
		// the workspace boundary: a shell can write anywhere via absolute
		// paths, so in ask/auto we gate ALL exec by default and let yolo
		// through. (Inside-workspace freedom is already enforced by the
		// exec tool's cmd.Dir; the gate here is about commands that reach
		// outside it.)
		if sandboxed {
			return authDecision{action: authAllow}
		}
		return modeGate(mode, "command execution (may touch outside workspace)")
	}

	// file write tools: workspace boundary on the path argument.
	if fileWriteTools[toolName] {
		path := extractStringArg(argsJSON, "path")
		if path == "" {
			return authDecision{action: authAllow}
		}
		abs := resolveAgainst(g.workspace, path)
		if isUnder(abs, g.workspace) {
			return authDecision{action: authAllow}
		}
		// Allowlisted (preset dirs + policy.json) writes are fine.
		g.mu.RLock()
		for _, p := range g.allowWrite {
			if isUnder(abs, p) {
				g.mu.RUnlock()
				return authDecision{action: authAllow}
			}
		}
		g.mu.RUnlock()
		if sandboxed {
			return authDecision{action: authAllow}
		}
		return modeGate(mode, "write outside workspace: "+path)
	}

	// All other tools (read_file, list_dir, web_fetch, mcp_*, ...): allow.
	return authDecision{action: authAllow}
}

// modeGate translates "needs authorization" into block/prompt based on the
// mode. yolo→allow, auto→block (no prompt), ask→prompt.
func modeGate(mode, reason string) authDecision {
	switch mode {
	case AuthModeYolo:
		return authDecision{action: authAllow}
	case AuthModeAuto:
		return authDecision{action: authBlock, reason: "auto mode denied: " + reason}
	default: // ask
		return authDecision{action: authPrompt, reason: reason}
	}
}

// writeTargetOutsideWorkspace returns the absolute resolved path and true
// when the call is a file-write tool whose path lands outside the
// workspace. Used by the loop to collect sandbox-bypass prefixes when a
// single-use /yes authorizes such a call. When sandboxed, returns false —
// the container IS the boundary, so there's no "outside workspace" to
// bypass.
func (g *authGate) writeTargetOutsideWorkspace(toolName, argsJSON string) (string, bool) {
	g.mu.RLock()
	sandboxed := g.sandboxed
	g.mu.RUnlock()
	if sandboxed {
		return "", false
	}
	if !fileWriteTools[toolName] {
		return "", false
	}
	path := extractStringArg(argsJSON, "path")
	if path == "" {
		return "", false
	}
	abs := resolveAgainst(g.workspace, path)
	if isUnder(abs, g.workspace) {
		return "", false
	}
	return abs, true
}

// extractStringArg pulls a string field from a JSON args blob without
// caring about the rest of the schema. Returns "" on any failure.
func extractStringArg(argsJSON, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(argsJSON), &m) != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// resolveAgainst resolves a possibly-relative path against a base dir,
// tolerating ~ and leaving absolute paths as-is. Used to check whether a
// file-tool path stays inside the workspace.
func resolveAgainst(base, path string) string {
	if path == "" {
		return base
	}
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

// isUnder reports whether path == root or lives under root/ (lexically,
// after cleaning). Used to keep allowlist entries inside the agent root.
func isUnder(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// =========================================================================
// Three-tier command safety: hardline floor + dangerous patterns
// (借鉴 hermes-agent tools/approval.py). Hardline is unconditional
// (yolo cannot bypass); dangerous follows the mode.
// =========================================================================

// hardlinePatterns are catastrophic, unrecoverable commands. Blocked
// unconditionally — yolo, ask, auto all refuse. Opting into yolo trusts
// the agent with files/services, not with wiping the disk or powering off.
//
// Patterns are matched against the lowercased command, so PowerShell
// cmdlet casing (Format-Volume vs format-volume) doesn't matter. The
// list covers the catastrophic surface on Linux, macOS, and Windows.
var hardlinePatterns = []struct {
	re   *regexp.Regexp
	desc string
}{
	// ---- POSIX (Linux + macOS) ----
	{regexp.MustCompile(`\brm\s+(-[^\s]*\s+)*(/|/\*|/home|/etc|/usr|/var|/bin|/boot|~|\$home)(/?|/\*)?(\s|$)`), "recursive delete of root/system/home directory"},
	{regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\b`), "format filesystem (mkfs)"},
	{regexp.MustCompile(`\bdd\b[^\n]*\bof=/dev/(sd|nvme|hd|mmcblk|vd|xvd|disk|rdisk)`), "dd to raw block device"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|hd|mmcblk|vd|xvd|disk|rdisk)`), "redirect to raw block device"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), "fork bomb"},
	{regexp.MustCompile(`\bkill\s+(-[^\s]+\s+)*-1\b`), "kill all processes"},
	{regexp.MustCompile(`(?:^|[;&|\n])\s*(?:sudo\s+)?(?:/sbin/|/usr/sbin/)?(?:shutdown|reboot|halt|poweroff)(?:\s+(?:-(?:h|r|p|f|k|c|t)|now|fast)\b|\s*[;&|\n]|$)`), "system shutdown/reboot"},
	{regexp.MustCompile(`\bsystemctl\b.*\b(rescue|emergency)\b`), "systemctl rescue/emergency (single-user mode)"},
	{regexp.MustCompile(`\bdiskutil\b[^\n]*\b(erasedisk|secureerase|erasevolume)\b`), "diskutil eraseDisk / secureErase / eraseVolume (macOS format)"},
	{regexp.MustCompile(`\bcsrutil\b.*\bdisable\b`), "csrutil disable (macOS System Integrity Protection off)"},
	{regexp.MustCompile(`\bnvram\b[^\n]*\bboot-args\b`), "nvram boot-args write (macOS NVRAM boot variables)"},
	{regexp.MustCompile(`\bdd\b[^\n]*\bof=/dev/r?disk\d+`), "dd to macOS raw disk"},

	// ---- Windows (cmd + PowerShell) ----
	{regexp.MustCompile(`\bformat\s+[a-z]:`), "format disk (Windows format)"},
	{regexp.MustCompile(`\bshutdown\s+/(s|p|r|t)\b`), "Windows shutdown/restart"},
	{regexp.MustCompile(`\b(rmdir|rd)\s+/s\b[^\n]*[a-z]:\\`), "rmdir /s or rd /s on a Windows drive path"},
	{regexp.MustCompile(`\bdel\s+(/[a-z]+\s+)*[a-z]:\\(windows|program files|users|system32|config|\*)`), "del /s on Windows system path"},
	{regexp.MustCompile(`\bformat-volume\b`), "Format-Volume (PowerShell partition format)"},
	{regexp.MustCompile(`\bclear-disk\b`), "Clear-Disk (PowerShell disk wipe)"},
	{regexp.MustCompile(`\binitialize-disk\b`), "Initialize-Disk (PowerShell disk repartition)"},
	{regexp.MustCompile(`\b(remove-item|ri|rm)\b[^\n]*-(?:recurse|force)\b[^\n]*-(?:force|recurse)\b[^\n]*[a-z]:\\(windows|program files|users|system32|\*)`), "Remove-Item -Recurse -Force on Windows system path"},
	{regexp.MustCompile(`\bstop-computer\b`), "Stop-Computer (PowerShell shutdown)"},
	{regexp.MustCompile(`\brestart-computer\b`), "Restart-Computer (PowerShell reboot)"},
	{regexp.MustCompile(`\bbcdedit\b.*/set\b`), "bcdedit /set (Windows boot configuration rewrite)"},
	{regexp.MustCompile(`\bcipher\b[^|;&\n]*\s/w[:\\]`), "cipher /w (Windows raw free-space wipe)"},
	{regexp.MustCompile(`\bdel\s+(/[a-z]+\s+)*[a-z]:\\[^;\n]*\\config\\(system|software|sam|security|default|components)`), "del on Windows registry hive file"},
}

// dangerousPatterns are high-risk but potentially recoverable commands.
// They go through the mode (ask→prompt, auto→deny, yolo→allow). Catches
// destructive ops that the workspace boundary alone would miss (e.g.
// `rm -rf ./` inside the workspace is still dangerous).
var dangerousPatterns = []struct {
	re   *regexp.Regexp
	desc string
}{
	{regexp.MustCompile(`\brm\s+-[^\s]*r`), "recursive delete"},
	{regexp.MustCompile(`\brm\s+-[^\s]*\s+--recursive\b`), "recursive delete (long flag)"},
	{regexp.MustCompile(`\bchmod\s+(-[^\s]*\s+)*(777|666)\b`), "world-writable permissions"},
	{regexp.MustCompile(`\bchown\s+-\S*r\S*\s+root\b`), "recursive chown to root"},
	{regexp.MustCompile(`\bdrop\s+(table|database)\b`), "SQL DROP"},
	{regexp.MustCompile(`\bdelete\s+from\b`), "SQL DELETE (verify it has a WHERE)"},
	{regexp.MustCompile(`\btruncate\s+(table)?\s*\w`), "SQL TRUNCATE"},
	{regexp.MustCompile(`\b(curl|wget)\b.*\|\s*(?:[/\w]*/)?(?:ba)?sh`), "pipe remote content to shell"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "git reset --hard (destroys uncommitted changes)"},
	{regexp.MustCompile(`\bgit\s+push\b.*--force\b`), "git force push (rewrites remote history)"},
	{regexp.MustCompile(`\bgit\s+push\b.*\s-f\b`), "git force push short flag"},
	{regexp.MustCompile(`\bgit\s+clean\s+-[^\s]*f`), "git clean with force"},
	{regexp.MustCompile(`\bgit\s+branch\s+-d\b`), "git branch force delete"},
	{regexp.MustCompile(`\bsystemctl\s+(stop|restart|disable)\b`), "stop/restart system service"},
	{regexp.MustCompile(`\bpkill\s+-9\b`), "force kill processes"},
	{regexp.MustCompile(`\bxargs\s+.*\brm\b`), "xargs with rm"},
	{regexp.MustCompile(`\bfind\b.*-exec(?:dir)?\s+(?:/\S*/)?rm\b`), "find -exec/-execdir rm"},
	{regexp.MustCompile(`\bfind\b.*-delete\b`), "find -delete"},
	{regexp.MustCompile(`\b(rmdir|rd)\s+/s\b`), "rmdir /s or rd /s (recursive, Windows)"},
	{regexp.MustCompile(`\bdel\s+/[a-z]*s\b`), "del /s (recursive, Windows)"},
	{regexp.MustCompile(`\bdiskpart\b`), "diskpart (partition management)"},
	{regexp.MustCompile(`\b(remove-item|ri|rm)\b[^\n]*-recurse[^\n]*-force\b`), "PowerShell Remove-Item -Recurse -Force"},
	{regexp.MustCompile(`\b(remove-item|ri|rm)\b[^\n]*-force[^\n]*-recurse\b`), "PowerShell Remove-Item -Force -Recurse"},
	{regexp.MustCompile(`\breg(\.exe)?\s+(delete|import|restore)\b`), "reg delete/import/restore (Windows registry rewrite)"},
	{regexp.MustCompile(`\btakeown\b.*/f\b`), "takeown (Windows ownership takeover)"},
	{regexp.MustCompile(`\bicacls\b.*/grant\b`), "icacls /grant (Windows ACL modification)"},
	{regexp.MustCompile(`\bnet(\.exe)?\s+(user|localgroup)\b`), "net user/localgroup (Windows account change)"},
	{regexp.MustCompile(`\bschtasks\b.*/create\b`), "schtasks /create (Windows scheduled task creation)"},
	{regexp.MustCompile(`\bset-executionpolicy\b`), "Set-ExecutionPolicy (PowerShell policy bypass)"},
	{regexp.MustCompile(`\bdefaults\b.*\bwrite\b`), "defaults write (macOS preferences modify)"},
	{regexp.MustCompile(`\blaunchctl\b.*\b(load|unload|bootout)\b`), "launchctl load/unload/bootout (macOS service control)"},
}

// classifyCommand inspects a shell command against hardline + dangerous
// patterns. Returns the tier hit, or tierNormal when no pattern matched.
//
// Note: classifyCommand does NOT know about the safe whitelist. The safe
// auto-allow is decided separately in evaluateCall via commandSafeTier,
// which composes a shell-operator blocklist with the safePatterns list.
// classifyCommand keeps its original two-role job (hardline floor +
// dangerous flagging) so existing callers and tests are unaffected.
type commandTier int

const (
	tierNormal commandTier = iota // unclassified — falls through to mode gate
	tierDangerous                 // high-risk recoverable, mode decides
	tierHardline                  // catastrophic, unconditional block
	// tierSafe is intentionally NOT returned by classifyCommand. It is the
	// auto-allow verdict from commandSafeTier, evaluated in evaluateCall
	// after the hardline check clears.
	tierSafe
)

func classifyCommand(command string) (commandTier, string) {
	c := strings.ToLower(command)
	for _, p := range hardlinePatterns {
		if p.re.MatchString(c) {
			return tierHardline, p.desc
		}
	}
	for _, p := range dangerousPatterns {
		if p.re.MatchString(c) {
			return tierDangerous, p.desc
		}
	}
	return tierNormal, ""
}

// =========================================================================
// Safe-tier whitelist — conservative read-only / query commands that are
// auto-allowed regardless of mode (ask/auto/yolo). Plugs the "every harmless
// `dir` / `--version` / `git status` triggers a /yes prompt" passivity the
// user flagged. Order in evaluateCall is:
//
//  1. hardline → block (unconditional, FIRST — safe can never bypass)
//  2. safe → allow (auto, regardless of mode)
//  3. dangerous → mode gate
//  4. normal exec → mode gate (existing)
//
// A command qualifies as safe ONLY if BOTH hold:
//   - it contains NO shell operator that could chain or redirect
//     (see shellOperatorRE), AND
//   - it matches a safePattern (read-only / query verb + arg shape).
//
// Anything else falls through to the mode gate. When uncertain, DO NOT
// whitelist — let the existing gate prompt.
// =========================================================================

// shellOperatorRE matches any shell operator that could chain a second
// command, redirect I/O, or perform substitution/background. If ANY of
// these appears in the command string, commandSafeTier returns false and
// the command falls through to the mode gate.
//
// Covered: `|`, `||`, `&&`, `;`, `>`, `>>`, `<`, `<<`, backtick (cmd
// substitution), `$(...)` (cmd substitution via `$(`), and bare `&`
// (background). `$` alone is not flagged (Windows filenames may contain
// it); only `$(` is — so `echo $HOME` is NOT blocked, but `echo $(rm x)`
// is. Note: because we test for `$(`, `echo $HOME` is allowed while
// `echo $(whoami)` is not.
var shellOperatorRE = regexp.MustCompile("(`|\\$\\(|\\|\\||&&|\\||;|>|<|&)")

// safePatterns is the conservative whitelist of read-only / query command
// shapes. Patterns are matched against the lowercased command AFTER the
// operator blocklist has cleared it, so they can be written without
// worrying about chained redirects. Each pattern anchors the verb at the
// start (`^...`) so a leading subshell or path can't smuggle a safe verb.
//
// To keep the surface conservative, the arg shape is restricted per family
// (e.g., `git branch` matches only when bare or carrying list-style flags;
// `git remote` matches only bare or with `-v`).
var safePatterns = []struct {
	re   *regexp.Regexp
	desc string
}{
	// ---- Single-token listing / state queries (any args after operator check) ----
	// These commands cannot mutate the filesystem: ls/dir/pwd/whoami/hostname/uname.
	{regexp.MustCompile(`^(dir|ls|pwd|whoami|hostname|uname)(\s.*)?$`), "read-only listing/state query"},
	// Disk/memory state: df/du/free (POSIX) and systeminfo (Windows).
	{regexp.MustCompile(`^(df|du|free|systeminfo)(\s.*)?$`), "disk/state query"},

	// ---- Path lookup ----
	{regexp.MustCompile(`^(where|which)(\s.*)?$`), "path lookup"},

	// ---- Print / stream (file content → stdout, no mutation) ----
	// echo/print: text output. type (Windows) / cat (POSIX): stream file.
	// Redirect forms (`cat f > g`) are already rejected by the operator check.
	{regexp.MustCompile(`^(echo|print|type|cat)(\s.*)?$`), "print/stream command"},

	// ---- Version / help flags on known binaries ----
	// Allow only short help/version flags so a malicious flag can't slip in.
	{regexp.MustCompile(`^(node|npm|pnpm|yarn|python|python3|py|go|java|ruby|php|rustc|cargo|pip|pip3|git|gh|docker|kubectl|helm)\s+(-h|--help|-v|--version|-version|-V)$`), "binary version/help flag"},

	// ---- git read-only subcommands ----
	// status / diff / log / show: no write side-effects regardless of args.
	{regexp.MustCompile(`^git\s+status\b`), "git status"},
	{regexp.MustCompile(`^git\s+diff\b`), "git diff"},
	{regexp.MustCompile(`^git\s+log\b`), "git log"},
	{regexp.MustCompile(`^git\s+show\b`), "git show"},
	// branch: ONLY bare or list-style flags. `git branch <name>` (create),
	// `-d`/`-D` (delete), `-m` (rename) must NOT be safe.
	{regexp.MustCompile(`^git\s+branch(\s+(-[arlvV]+|--list|--all|--remotes|--verbose))*\s*$`), "git branch (list)"},
	// remote: bare, `-v`, `--verbose`, or `show <name>`. add/remove/set-url NOT safe.
	{regexp.MustCompile(`^git\s+remote\s*$`), "git remote (list)"},
	{regexp.MustCompile(`^git\s+remote\s+(-v|--verbose)\s*$`), "git remote -v"},
	{regexp.MustCompile(`^git\s+remote\s+show\s+\S+$`), "git remote show"},
	// tag: ONLY bare or list-style (`-l` / `--list` with optional pattern).
	// `git tag <name>` (create), `-d` (delete), `-a`/`-s`/`-m` (annotate),
	// `-f` (force) must NOT be safe — mirror the git branch list-only shape.
	{regexp.MustCompile(`^git\s+tag(\s+(-l|--list)(\s+\S+)?)?\s*$`), "git tag (list)"},
	// Other read-only git queries. (`tag` was pulled out of this group into
	// the strict list-only pattern above so `git tag v1.0` can't sneak through.)
	{regexp.MustCompile(`^git\s+(blame|shortlog|describe|ls-files|rev-parse)\b`), "git read-only query"},
	// config: ONLY read forms. `git config user.email x` writes — NOT safe.
	{regexp.MustCompile(`^git\s+config\s+(-l|--list)\s*$`), "git config --list"},
	{regexp.MustCompile(`^git\s+config\s+--get\b`), "git config --get"},

	// ---- Package manager list / show ----
	{regexp.MustCompile(`^(npm|pnpm|yarn)\s+(list|ls)(\s.*)?$`), "npm/pnpm/yarn list"},
	{regexp.MustCompile(`^pip3?\s+(list|show)(\s.*)?$`), "pip list/show"},
}

// commandSafeTier returns true ONLY when the command:
//  1. is non-empty,
//  2. contains NO shell operator that could chain/redirect/substitute
//     (see shellOperatorRE), AND
//  3. matches one of the conservative safePatterns.
//
// If any operator is present → false (the command may still match a
// safePattern verb, but the operator could smuggle a destructive second
// command, so it must go through the mode gate).
func commandSafeTier(command string) bool {
	if command == "" {
		return false
	}
	if shellOperatorRE.MatchString(command) {
		return false
	}
	c := strings.ToLower(command)
	for _, p := range safePatterns {
		if p.re.MatchString(c) {
			return true
		}
	}
	return false
}

// denyMessageBypass returns the standard anti-bypass rejection wording,
// bilingual (zh + en) so it works regardless of chatter language. Hardline
// and authorization denials both use it so the LLM can't route around a
// refusal by rephrasing or switching tools — a prompt-injection guardrail
// borrowed from hermes-agent's "silence is not consent" contract.
func denyMessageBypass(reason string) string {
	return "BLOCKED: " + reason + ".\n" +
		"用户未授权此操作。不要重试这条命令，不要换措辞，也不要换工具/换路径去达到同样目的。停下当前流程，等用户明确回应后再继续。\n" +
		"The user has NOT authorized this action. Do NOT retry this command, " +
		"do NOT rephrase it, and do NOT attempt the same outcome via another tool or path. " +
		"Stop the current workflow and wait for the user to respond."
}
