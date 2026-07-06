package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// regexHookResult holds the output of a single matched hook.
type regexHookResult struct {
	name string
	text string
	err  error
}

// matchRegexHooks evaluates all enabled regex hooks for this agent against
// the message text. Returns ("", "", false) when no hook matches — the caller
// should proceed to the normal ReAct loop. Returns (reply, hookName, true) when at
// least one hook matched; the reply is ready to send back to the user.
func (a *Agent) matchRegexHooks(ctx context.Context, text string) (string, string, bool) {
	if a.dataStore == nil || text == "" {
		return "", "", false
	}

	hooks, err := a.dataStore.ListRegexHooks(ctx, a.agentID)
	if err != nil {
		slog.Warn("regex hooks: list failed", "agent", a.agentID, "error", err)
		return "", "", false
	}
	if len(hooks) == 0 {
		return "", "", false
	}

	var results []regexHookResult
	for _, h := range hooks {
		if !h.Enabled {
			continue
		}
		re, compileErr := regexp.Compile(h.Pattern)
		if compileErr != nil {
			slog.Warn("regex hooks: invalid pattern, skipping",
				"hook_id", h.ID, "pattern", h.Pattern, "error", compileErr)
			continue
		}
		if !re.MatchString(text) {
			continue
		}

		slog.Info("regex hook matched",
			"hook_id", h.ID, "hook_name", h.Name, "agent", a.agentID)

		output, execErr := executeCLI(ctx, a.agentID, h.CLICommand, text)
		if execErr != nil {
			if h.ShowError {
				errMsg := fmt.Sprintf("CLI 执行失败 (%s): %v", h.Name, execErr)
				if h.ErrorMessage != "" {
					errMsg = h.ErrorMessage
				}
				results = append(results, regexHookResult{name: h.Name, text: errMsg, err: execErr})
			} else {
				slog.Warn("regex hook CLI failed (show_error=false)",
					"hook_id", h.ID, "error", execErr)
			}
		} else if output != "" {
			results = append(results, regexHookResult{name: h.Name, text: output})
		}

		if !h.ContinueOnMatch {
			break
		}
	}

	if len(results) == 0 {
		return "", "", false
	}

	if len(results) == 1 {
		return results[0].text, results[0].name, true
	}

	var names []string
	var buf strings.Builder
	for i, r := range results {
		names = append(names, r.name)
		if i > 0 {
			buf.WriteString("\n\n")
		}
		fmt.Fprintf(&buf, "---%s---\n\n%s", r.name, r.text)
	}
	return buf.String(), strings.Join(names, ", "), true
}

// executeCLI runs cmdString with text piped via stdin. Returns stdout.
// If cmdString starts with "hooks/", the prefix is resolved to the
// agent's hooks directory (~/.fastclaw/agents/<agentID>/hooks/).
func executeCLI(ctx context.Context, agentID, cmdString, text string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	resolved := cmdString
	// Normalize backslashes so a Windows "hooks\foo.bat" prefix resolves
	// the same as "hooks/foo.bat". Without this, TrimPrefix only strips the
	// forward-slash form and filepath.Join produces .../hooks/hooks/foo.bat,
	// so a backslash-style hook path can't find its script.
	normalized := strings.ReplaceAll(cmdString, "\\", "/")
	if strings.HasPrefix(normalized, "hooks/") {
		dir, err := hooksDir(agentID)
		if err == nil {
			resolved = filepath.Join(dir, strings.TrimPrefix(normalized, "hooks/"))
		}
	}

	cmd := exec.CommandContext(ctx, "cmd", "/C", resolved)
	cmd.Stdin = strings.NewReader(text)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}

	return strings.TrimSpace(stdout.String()), nil
}

func hooksDir(agentID string) (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "agents", agentID, "hooks"), nil
}

// regexHookArgs marshals the matched text into a JSON object so the
// synthetic regex-hook tool_call carries valid-JSON arguments. A bare
// string here 500s/empties the reply when OpenAI-compatible APIs
// (e.g. longcat) replay the history on later turns.
func regexHookArgs(text string) string {
	b, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "{}"
	}
	return string(b)
}
