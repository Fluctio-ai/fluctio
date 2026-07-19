package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSafeTierWhitelist covers the four required cases for the authgate
// safe-tier auto-allow:
//  1. safe commands (dir, --version, git status, where bash, ...) → authAllow
//     even in ask mode.
//  2. dangerous commands (rm -rf ./) still mode-gated (ask→prompt,
//     auto→block, yolo→allow).
//  3. chained commands with shell operators (dir && echo, cat > file,
//     git log | grep) NOT safe → mode gate.
//  4. hardline commands (rm -rf /, shutdown /s) still authBlock even in
//     yolo and even if they look superficially related to safe verbs.
func TestSafeTierWhitelist(t *testing.T) {
	g := newAuthGate("", "")

	type tc struct {
		name    string
		tool    string
		command string
		mode    string
		want    authAction
	}
	cases := []tc{
		// (1) safe commands → allow even in ask mode.
		{name: "safe dir ask", tool: "exec", command: "dir", mode: AuthModeAsk, want: authAllow},
		{name: "safe ls -la ask", tool: "exec", command: "ls -la", mode: AuthModeAsk, want: authAllow},
		{name: "safe node version ask", tool: "exec", command: "node --version", mode: AuthModeAsk, want: authAllow},
		{name: "safe python version ask", tool: "exec", command: "python --version", mode: AuthModeAsk, want: authAllow},
		{name: "safe git status ask", tool: "exec", command: "git status", mode: AuthModeAsk, want: authAllow},
		{name: "safe git log oneline ask", tool: "exec", command: "git log --oneline", mode: AuthModeAsk, want: authAllow},
		{name: "safe git diff ask", tool: "exec", command: "git diff", mode: AuthModeAsk, want: authAllow},
		{name: "safe git branch noargs ask", tool: "exec", command: "git branch", mode: AuthModeAsk, want: authAllow},
		{name: "safe git branch -a ask", tool: "exec", command: "git branch -a", mode: AuthModeAsk, want: authAllow},
		{name: "safe git tag noargs ask", tool: "exec", command: "git tag", mode: AuthModeAsk, want: authAllow},
		{name: "safe git tag -l ask", tool: "exec", command: "git tag -l", mode: AuthModeAsk, want: authAllow},
		{name: "safe git tag -l pattern ask", tool: "exec", command: "git tag -l v1.*", mode: AuthModeAsk, want: authAllow},
		{name: "safe git remote -v ask", tool: "exec", command: "git remote -v", mode: AuthModeAsk, want: authAllow},
		{name: "safe where bash ask", tool: "exec", command: "where bash", mode: AuthModeAsk, want: authAllow},
		{name: "safe which go ask", tool: "exec", command: "which go", mode: AuthModeAsk, want: authAllow},
		{name: "safe npm list ask", tool: "exec", command: "npm list", mode: AuthModeAsk, want: authAllow},
		{name: "safe pip list ask", tool: "exec", command: "pip list", mode: AuthModeAsk, want: authAllow},
		{name: "safe cat file ask", tool: "exec", command: "cat README.md", mode: AuthModeAsk, want: authAllow},
		{name: "safe echo hi ask", tool: "exec", command: "echo hello world", mode: AuthModeAsk, want: authAllow},
		{name: "safe pwd ask", tool: "exec", command: "pwd", mode: AuthModeAsk, want: authAllow},
		{name: "safe whoami auto", tool: "exec", command: "whoami", mode: AuthModeAuto, want: authAllow},
		{name: "safe systeminfo ask", tool: "exec", command: "systeminfo", mode: AuthModeAsk, want: authAllow},
		{name: "safe df ask", tool: "exec", command: "df -h", mode: AuthModeAsk, want: authAllow},

		// (2) dangerous commands still mode-gated.
		{name: "dangerous rm rf ask", tool: "exec", command: "rm -rf ./", mode: AuthModeAsk, want: authPrompt},
		{name: "dangerous rm rf auto", tool: "exec", command: "rm -rf ./", mode: AuthModeAuto, want: authBlock},
		{name: "dangerous rm rf yolo", tool: "exec", command: "rm -rf ./", mode: AuthModeYolo, want: authAllow},
		{name: "dangerous git reset hard ask", tool: "exec", command: "git reset --hard", mode: AuthModeAsk, want: authPrompt},

		// (3) chained commands with shell operators NOT safe → mode gate.
		{name: "chained dir and echo ask", tool: "exec", command: "dir && echo hi", mode: AuthModeAsk, want: authPrompt},
		{name: "chained cat redirect ask", tool: "exec", command: "cat file > /tmp/x", mode: AuthModeAsk, want: authPrompt},
		{name: "chained git log pipe ask", tool: "exec", command: "git log | grep foo", mode: AuthModeAsk, want: authPrompt},
		{name: "chained dir semicolon ask", tool: "exec", command: "dir; echo hi", mode: AuthModeAsk, want: authPrompt},
		{name: "chained git status background ask", tool: "exec", command: "git status &", mode: AuthModeAsk, want: authPrompt},
		{name: "chained echo cmdsubst ask", tool: "exec", command: "echo $(whoami)", mode: AuthModeAsk, want: authPrompt},
		{name: "chained cat append ask", tool: "exec", command: "cat file >> other", mode: AuthModeAsk, want: authPrompt},
		{name: "chained git status auto", tool: "exec", command: "git status && ls", mode: AuthModeAuto, want: authBlock},

		// (4) hardline still blocks — even in yolo, even if it starts with a
		// safe-looking verb or includes a "safe" word.
		{name: "hardline rm root yolo", tool: "exec", command: "rm -rf /", mode: AuthModeYolo, want: authBlock},
		{name: "hardline rm etc yolo", tool: "exec", command: "rm -rf /etc", mode: AuthModeYolo, want: authBlock},
		{name: "hardline mkfs yolo", tool: "exec", command: "mkfs.ext4 /dev/sda1", mode: AuthModeYolo, want: authBlock},
		{name: "hardline windows shutdown yolo", tool: "exec", command: "shutdown /s /t 0", mode: AuthModeYolo, want: authBlock},
		{name: "hardline windows format yolo", tool: "exec", command: "format D:", mode: AuthModeYolo, want: authBlock},

		// (5) normal exec (not safe, not dangerous, not hardline) still gated.
		{name: "normal exec ask", tool: "exec", command: "go build ./...", mode: AuthModeAsk, want: authPrompt},
		{name: "normal exec auto", tool: "exec", command: "go test ./...", mode: AuthModeAuto, want: authBlock},
		{name: "normal exec yolo", tool: "exec", command: "go run main.go", mode: AuthModeYolo, want: authAllow},

		// (6) git branch with create/delete args must NOT be safe.
		{name: "git branch create ask", tool: "exec", command: "git branch new-feature", mode: AuthModeAsk, want: authPrompt},
		{name: "git branch delete ask", tool: "exec", command: "git branch -d old", mode: AuthModeAsk, want: authPrompt},
		{name: "git remote add ask", tool: "exec", command: "git remote add origin url", mode: AuthModeAsk, want: authPrompt},
		// (6b) git tag with create/delete/annotate args must NOT be safe.
		{name: "git tag create ask", tool: "exec", command: "git tag v1.0", mode: AuthModeAsk, want: authPrompt},
		{name: "git tag create auto", tool: "exec", command: "git tag v1.0", mode: AuthModeAuto, want: authBlock},
		{name: "git tag delete ask", tool: "exec", command: "git tag -d v1.0", mode: AuthModeAsk, want: authPrompt},
		{name: "git tag annotate ask", tool: "exec", command: "git tag -a v1.0 -m msg", mode: AuthModeAsk, want: authPrompt},
		{name: "git tag force ask", tool: "exec", command: "git tag -f v1.0", mode: AuthModeAsk, want: authPrompt},

		// (7) Shell-wrapper unwrap: cmd /c <inner>, sh -c <inner>, bash -c <inner>.
		// Safe inner → allow; non-safe inner → mode gate; operators in inner → mode gate;
		// hardline inner → block (defense in depth via inner-classify).
		{name: "wrapped cmd /c dir ask", tool: "exec", command: "cmd /c dir", mode: AuthModeAsk, want: authAllow},
		{name: "wrapped cmd /c quoted dir ask", tool: "exec", command: `cmd /c "dir"`, mode: AuthModeAsk, want: authAllow},
		{name: "wrapped cmd /c dir with args ask", tool: "exec", command: `cmd /c dir C:\Users`, mode: AuthModeAsk, want: authAllow},
		{name: "wrapped cmd.exe /c git status ask", tool: "exec", command: "cmd.exe /c git status", mode: AuthModeAsk, want: authAllow},
		{name: "wrapped cmd full path /c dir ask", tool: "exec", command: `C:\Windows\System32\cmd.exe /c dir`, mode: AuthModeAsk, want: authAllow},
		{name: "wrapped sh -c ls ask", tool: "exec", command: "sh -c ls", mode: AuthModeAsk, want: authAllow},
		{name: "wrapped sh -c quoted ls ask", tool: "exec", command: "sh -c 'ls'", mode: AuthModeAsk, want: authAllow},
		{name: "wrapped bash -c quoted git status ask", tool: "exec", command: "bash -c 'git status'", mode: AuthModeAsk, want: authAllow},
		{name: "wrapped sh -c uname ask", tool: "exec", command: "sh -c 'uname -a'", mode: AuthModeAsk, want: authAllow},

		// (7b) Wrapper + operator in inner → NOT safe (mode gate).
		{name: "wrapped cmd /c chained ask", tool: "exec", command: `cmd /c "dir && del x"`, mode: AuthModeAsk, want: authPrompt},
		{name: "wrapped sh -c pipe ask", tool: "exec", command: "sh -c 'ls | grep foo'", mode: AuthModeAsk, want: authPrompt},
		{name: "wrapped cmd /c redirect ask", tool: "exec", command: `cmd /c "dir > x"`, mode: AuthModeAsk, want: authPrompt},

		// (7c) Wrapper + non-safe verb in inner → NOT safe (mode gate or dangerous).
		{name: "wrapped cmd /c rm ask", tool: "exec", command: "cmd /c rm -rf x", mode: AuthModeAsk, want: authPrompt},

		// (7d) Wrapper + hardline inner → BLOCK even in yolo (defense in depth:
		// quoted wrappers may evade the raw-string hardline check because of
		// trailing quote/punctuation).
		{name: "wrapped cmd /c hardline rm root yolo", tool: "exec", command: `cmd /c "rm -rf /"`, mode: AuthModeYolo, want: authBlock},
		{name: "wrapped sh -c hardline rm home yolo", tool: "exec", command: "sh -c 'rm -rf /home'", mode: AuthModeYolo, want: authBlock},
		{name: "wrapped sh -c hardline mkfs yolo", tool: "exec", command: "sh -c 'mkfs.ext4 /dev/sda1'", mode: AuthModeYolo, want: authBlock},
		{name: "wrapped cmd /c hardline shutdown yolo", tool: "exec", command: `cmd /c "shutdown /s /t 0"`, mode: AuthModeYolo, want: authBlock},

		// (7e) Malformed wrapper (nothing after /c) → fall through to mode gate.
		{name: "wrapped cmd /c empty ask", tool: "exec", command: "cmd /c", mode: AuthModeAsk, want: authPrompt},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": c.command})
			dec := g.evaluateCall(c.tool, string(args), c.mode)
			if dec.action != c.want {
				t.Fatalf("command %q mode=%s: want action=%d got=%d reason=%q",
					c.command, c.mode, c.want, dec.action, dec.reason)
			}
		})
	}
}

// TestCommandSafeTierDirect exercises the commandSafeTier helper directly to
// confirm the operator blocklist + safe-pattern interaction without going
// through evaluateCall.
func TestCommandSafeTierDirect(t *testing.T) {
	safe := []string{
		"dir", "ls", "ls -la", "pwd", "whoami", "hostname",
		"systeminfo", "df -h", "du -sh /", "free -h", "uname -a",
		"where bash", "which go",
		"echo hello", "cat README.md", "type file.txt",
		"node --version", "python --version", "go --help",
		"git status", "git diff", "git log --oneline", "git show HEAD",
		"git branch", "git branch -a", "git remote -v",
		"git tag", "git tag -l", "git tag -l v1.*", "git tag --list", "git tag --list 'v2.*'",
		"npm list", "npm ls", "pip list", "pip show pkg",
	}
	for _, cmd := range safe {
		if !commandSafeTier(cmd) {
			t.Errorf("commandSafeTier(%q) = false, want true", cmd)
		}
	}

	notSafe := []string{
		// Operators disqualify.
		"dir && rm x", "ls | grep foo", "cat f > g", "cat f >> g",
		"echo $(whoami)", "echo `whoami`", "git status;", "git status &",
		"git log < file", "ls ; whoami",
		// Not in safe list.
		"go build ./...", "rm file", "mv a b", "cp a b",
		"git branch newname", "git branch -d old", "git remote add origin url",
		"git config user.email", "npm install pkg",
		// git tag create / delete / annotate / force must NOT be safe.
		"git tag v1.0", "git tag -d v1.0", "git tag -a v1.0", "git tag -f v1.0",
		"git tag -m msg v1.0",
		// Wrapper + operator in inner must NOT be safe.
		`cmd /c "dir && del x"`, "sh -c 'ls | grep foo'",
		// Wrapper + non-safe verb must NOT be safe.
		"cmd /c rm -rf x", "sh -c go build ./...",
		// Empty.
		"",
	}
	for _, cmd := range notSafe {
		if commandSafeTier(cmd) {
			t.Errorf("commandSafeTier(%q) = true, want false", cmd)
		}
	}

	// Wrapped safe inners SHOULD now report safe.
	wrappedSafe := []string{
		"cmd /c dir", `cmd /c "dir"`, `cmd /c dir C:\Users`,
		"cmd.exe /c git status", `C:\Windows\System32\cmd.exe /c dir`,
		"sh -c ls", "sh -c 'ls'", "bash -c 'git status'",
		"sh -c 'uname -a'",
	}
	for _, cmd := range wrappedSafe {
		if !commandSafeTier(cmd) {
			t.Errorf("commandSafeTier(%q) = false, want true (wrapped safe inner)", cmd)
		}
	}
}

// TestClassifyCommandSafeTierStillUnclassified verifies that classifyCommand
// still returns tierNormal (was tierSafe, the default unclassified tier)
// for safe-listed commands — the auto-allow happens in evaluateCall, not
// classifyCommand. classifyCommand keeps its original hardline/dangerous
// semantics; only evaluateCall adds the safe shortcut.
func TestClassifyCommandSafeTierStillUnclassified(t *testing.T) {
	// These are auto-allowed by commandSafeTier, but classifyCommand should
	// still report them as the default (unclassified) tier, NOT as
	// tierHardline or tierDangerous.
	for _, cmd := range []string{"dir", "git status", "node --version"} {
		tier, desc := classifyCommand(cmd)
		if tier == tierHardline || tier == tierDangerous {
			t.Errorf("classifyCommand(%q) = %d desc=%q: safe command must not be classified as hardline/dangerous",
				cmd, tier, desc)
		}
	}
}

// TestUnwrapShellWrapper covers the one-layer wrapper-unwrap helper directly.
// Verifies: cmd/c (bare, .exe, full path), sh/c, bash/c; surrounding quotes
// stripped; only ONE layer stripped; empty inner falls through; non-wrappers
// pass through unchanged.
func TestUnwrapShellWrapper(t *testing.T) {
	cases := []struct {
		in       string
		wantInner string
		wantOK   bool
	}{
		// Windows cmd / c family.
		{in: "cmd /c dir", wantInner: "dir", wantOK: true},
		{in: `cmd /c "dir"`, wantInner: "dir", wantOK: true},
		{in: `cmd /c "dir C:\Users"`, wantInner: `dir C:\Users`, wantOK: true},
		{in: "cmd /c git status", wantInner: "git status", wantOK: true},
		{in: "cmd.exe /c dir", wantInner: "dir", wantOK: true},
		{in: "CMD /C dir", wantInner: "dir", wantOK: true},
		{in: `C:\Windows\System32\cmd.exe /c dir`, wantInner: "dir", wantOK: true},
		// POSIX sh / bash family.
		{in: "sh -c ls", wantInner: "ls", wantOK: true},
		{in: "sh -c 'ls -la'", wantInner: "ls -la", wantOK: true},
		{in: "bash -c 'git status'", wantInner: "git status", wantOK: true},
		{in: "sh -c 'echo hi'", wantInner: "echo hi", wantOK: true},
		// Only one layer stripped — nested wrapper kept.
		{in: "cmd /c cmd /c dir", wantInner: "cmd /c dir", wantOK: true},
		{in: "sh -c sh -c ls", wantInner: "sh -c ls", wantOK: true},
		// Empty / malformed → not unwrapped.
		{in: "cmd /c", wantInner: "cmd /c", wantOK: false},
		{in: "cmd /c   ", wantInner: "cmd /c   ", wantOK: false},
		{in: "sh -c", wantInner: "sh -c", wantOK: false},
		// Non-wrappers pass through.
		{in: "dir", wantInner: "dir", wantOK: false},
		{in: "git status", wantInner: "git status", wantOK: false},
		{in: "echo hi", wantInner: "echo hi", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			inner, ok := unwrapShellWrapper(c.in)
			if ok != c.wantOK {
				t.Fatalf("unwrapShellWrapper(%q) ok=%v want %v (inner=%q)",
					c.in, ok, c.wantOK, inner)
			}
			if ok && inner != c.wantInner {
				t.Fatalf("unwrapShellWrapper(%q) inner=%q want %q",
					c.in, inner, c.wantInner)
			}
		})
	}
}

// TestWrappedHardlineDefenseInDepth confirms that catastrophic commands
// wrapped in a shell wrapper still hit authBlock even in yolo mode. The
// raw-string hardline regex misses quoted wrappers (trailing quote breaks
// the trailing (\s|$) anchor), so evaluateCall also classifies the
// unwrapped inner — defense in depth.
func TestWrappedHardlineDefenseInDepth(t *testing.T) {
	g := newAuthGate("", "")
	cases := []string{
		`cmd /c "rm -rf /"`,
		`cmd /c rm -rf /`,
		`sh -c 'rm -rf /home'`,
		`sh -c 'mkfs.ext4 /dev/sda1'`,
		`cmd /c "shutdown /s /t 0"`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": cmd})
			dec := g.evaluateCall("exec", string(args), AuthModeYolo)
			if dec.action != authBlock {
				t.Fatalf("wrapped hardline %q mode=yolo: want authBlock got action=%d reason=%q",
					cmd, dec.action, dec.reason)
			}
		})
	}
}

// TestSafeTierReasonNotEmptyForBlocked ensures the hardline/dangerous paths
// still emit a non-empty reason string so downstream deny messages render.
func TestSafeTierReasonNotEmptyForBlocked(t *testing.T) {
	g := newAuthGate("", "")
	args, _ := json.Marshal(map[string]string{"command": "rm -rf /"})
	dec := g.evaluateCall("exec", string(args), AuthModeYolo)
	if dec.action != authBlock {
		t.Fatalf("want authBlock, got %d", dec.action)
	}
	if strings.TrimSpace(dec.reason) == "" {
		t.Fatalf("hardline block must carry a non-empty reason, got empty")
	}
}
