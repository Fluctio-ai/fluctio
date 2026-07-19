package main

// cmd_skill_pending.go wires the user-facing half of the skill write-approval
// gate (Phase 4 Task 2). Task 1 added internal/skills/pending.go +
// skill_manage tool: the agent writes pending edits to
// <agentHome>/skills-pending/<name>/. This file adds the four CLI commands a
// human uses to review and accept/reject those edits:
//
//	fluctio skill pending             list staged edits
//	fluctio skill approve <name>      move pending → live (atomic rename)
//	fluctio skill reject <name>       drop a staged edit
//	fluctio skill diff <name>         show pending SKILL.md vs live SKILL.md
//
// agentHome resolution mirrors the writer (skill_manage tool, loop.go):
// ~/.fluctio/agents/<agtID>/agent. --agent accepts a display name or agt_ id;
// when omitted the command auto-selects if the operator's store has exactly
// one agent. Approve is atomic at the skills package level; after the
// rename, notifyGatewayReload() pings the running daemon so cached
// UserSpaces are invalidated and the agent re-reads its skills directory on
// the next turn. The ping has two legs:
//
//  1. notifyGatewayReloadHTTP (cross-platform) — mint a temporary admin
//     API key for the agent's owner, POST /api/agents/<id>/skills/reload,
//     delete the key. Works on Windows where SIGHUP isn't deliverable.
//  2. notifyGatewayReload (SIGHUP) — Unix-only fast path that needs no
//     HTTP round-trip or temp key.
//
// Either leg is best-effort: the file rename already happened, so failure
// only means the running daemon keeps the old skills until next restart.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fluctio-ai/fluctio/internal/agentcli"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/daemon"
	"github.com/fluctio-ai/fluctio/internal/skills"
	"github.com/fluctio-ai/fluctio/internal/store"
	"github.com/fluctio-ai/fluctio/internal/users"
)

// skillPendingCmd lists staged skill edits awaiting approval.
func skillPendingCmd() *cobra.Command {
	var agentRef string
	cmd := &cobra.Command{
		Use:   "pending",
		Short: "List staged skill edits awaiting approval",
		Long: `List staged skill edits awaiting approval.

Skill edits proposed by the agent's skill_manage tool land in
<agentHome>/skills-pending/ and are NOT active until approved. This command
shows what's queued. Use "fluctio skill approve <name>" to activate.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			agentHome, err := resolveAgentHome(ctx, st, agentRef)
			if err != nil {
				return err
			}
			return runSkillPending(agentHome, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "agent name or agt_ id (default: auto-select if exactly one agent)")
	return cmd
}

// skillApproveCmd moves a staged skill from skills-pending/ to skills/.
func skillApproveCmd() *cobra.Command {
	var agentRef string
	cmd := &cobra.Command{
		Use:   "approve <name>",
		Short: "Activate a staged skill edit (pending → live)",
		Args:  cobra.ExactArgs(1),
		Long: `Activate a staged skill edit.

Atomically moves <agentHome>/skills-pending/<name>/ to <agentHome>/skills/<name>/.
If a live skill already exists at the destination it is replaced. After the
rename, this command pings the running gateway so the agent hot-reloads its
skills directory on the next turn:

  - Unix: SIGHUP the daemon PID (fast path, no HTTP round-trip).
  - Windows (or SIGHUP failure): mint a temporary admin API key, POST
    /api/agents/<id>/skills/reload, delete the key. Cross-platform fallback.

Both legs are best-effort — the file rename already happened, so a reload
failure only means the running daemon keeps the old skills until restart.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			agentHome, rec, err := resolveAgentHomeAndRec(ctx, st, agentRef)
			if err != nil {
				return err
			}
			if err := runSkillApprove(agentHome, args[0], os.Stdout); err != nil {
				return err
			}
			// Best-effort reload. Try HTTP first (cross-platform, works on
			// Windows); fall back to SIGHUP on Unix. Either leg failing is
			// fine — the rename already happened.
			notifyGatewayReloadHTTP(ctx, st, rec)
			notifyGatewayReload()
			return nil
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "agent name or agt_ id (default: auto-select if exactly one agent)")
	return cmd
}

// skillRejectCmd drops a staged skill edit without activating it.
func skillRejectCmd() *cobra.Command {
	var agentRef string
	cmd := &cobra.Command{
		Use:          "reject <name>",
		Short:        "Drop a staged skill edit (pending → discarded)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			agentHome, err := resolveAgentHome(ctx, st, agentRef)
			if err != nil {
				return err
			}
			return runSkillReject(agentHome, args[0], os.Stdout)
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "agent name or agt_ id (default: auto-select if exactly one agent)")
	return cmd
}

// skillDiffCmd shows pending SKILL.md alongside the live SKILL.md if any.
func skillDiffCmd() *cobra.Command {
	var agentRef string
	cmd := &cobra.Command{
		Use:          "diff <name>",
		Short:        "Show pending SKILL.md vs live SKILL.md",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			agentHome, err := resolveAgentHome(ctx, st, agentRef)
			if err != nil {
				return err
			}
			return runSkillDiff(agentHome, args[0], os.Stdout)
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "agent name or agt_ id (default: auto-select if exactly one agent)")
	return cmd
}

// resolveAgentHome turns the --agent flag into the agentHome path used by
// skills.{Write,Approve,Reject,List}Pending. agentRef is empty → auto-select
// when the store holds exactly one agent; otherwise require the user to
// disambiguate. Keeping this logic out of the cobra RunE makes the command
// bodies thin and puts the testable core in run* below.
func resolveAgentHome(ctx context.Context, st store.Store, agentRef string) (string, error) {
	home, _, err := resolveAgentHomeAndRec(ctx, st, agentRef)
	return home, err
}

// resolveAgentHomeAndRec is resolveAgentHome that also returns the full
// agent record. The approve path needs the agent ID (for the
// /api/agents/<id>/skills/reload URL) and the owner UserID (to mint the
// temporary admin API key); other callers use resolveAgentHome and
// discard the record.
func resolveAgentHomeAndRec(ctx context.Context, st store.Store, agentRef string) (string, *store.AgentRecord, error) {
	var rec *store.AgentRecord
	if strings.TrimSpace(agentRef) == "" {
		agents, err := agentcli.List(ctx, st)
		if err != nil {
			return "", nil, fmt.Errorf("list agents: %w", err)
		}
		switch len(agents) {
		case 0:
			return "", nil, errors.New("no agents found; create one with `fluctio agents init`")
		case 1:
			rec = &agents[0]
		default:
			return "", nil, errors.New("multiple agents found; specify one with --agent <name|agt_id>")
		}
	} else {
		r, err := agentcli.Resolve(ctx, st, agentRef)
		if err != nil {
			return "", nil, err
		}
		rec = r
	}
	home, err := config.AgentHomeDir(rec.ID)
	if err != nil {
		return "", nil, err
	}
	return home, rec, nil
}

// runSkillPending is the testable core of `skill pending`. It lists every
// staged edit with origin metadata. Empty pending → "No pending skills."
func runSkillPending(agentHome string, w io.Writer) error {
	entries, err := skills.ListPending(agentHome)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(w, "No pending skills.")
		return nil
	}
	fmt.Fprintf(w, "%-25s %-15s %-20s %s\n", "NAME", "SOURCE", "CREATED", "DESCRIPTION")
	fmt.Fprintln(w, strings.Repeat("-", 90))
	for _, e := range entries {
		desc := e.Meta.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		created := ""
		if !e.Meta.CreatedAt.IsZero() {
			created = e.Meta.CreatedAt.Local().Format("2006-01-02 15:04")
		}
		source := e.Meta.Source
		if source == "" {
			source = "-"
		}
		fmt.Fprintf(w, "%-25s %-15s %-20s %s\n", e.Name, source, created, desc)
	}
	return nil
}

// runSkillApprove is the testable core of `skill approve <name>`. It performs
// the atomic move via skills.ApprovePending and prints the live path on
// success. The cobra wrapper follows up with notifyGatewayReload() so the
// running agent picks up the new skill on its next turn; that step lives in
// the wrapper (not here) so unit tests stay daemon-free.
func runSkillApprove(agentHome, name string, w io.Writer) error {
	livePath, err := skills.ApprovePending(agentHome, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "activated %s at %s\n", name, livePath)
	return nil
}

// runSkillReject is the testable core of `skill reject <name>`. RejectPending
// is idempotent (no error on missing), matching the user-facing outcome
// "the pending edit is gone".
func runSkillReject(agentHome, name string, w io.Writer) error {
	if err := skills.RejectPending(agentHome, name); err != nil {
		return err
	}
	fmt.Fprintf(w, "removed pending %s\n", name)
	return nil
}

// runSkillDiff is the testable core of `skill diff <name>`. MVP output is a
// two-pane `--- live / +++ pending` dump — no diff library, no line-level
// algorithm. When no live skill exists yet (first-time create), only the
// pending body is shown.
func runSkillDiff(agentHome, name string, w io.Writer) error {
	pendingFile := filepath.Join(agentHome, "skills-pending", name, "SKILL.md")
	pendingBody, err := os.ReadFile(pendingFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no pending skill named %q", name)
		}
		return fmt.Errorf("read pending SKILL.md: %w", err)
	}
	liveFile := filepath.Join(agentHome, "skills", name, "SKILL.md")
	liveBody, err := os.ReadFile(liveFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read live SKILL.md: %w", err)
		}
		fmt.Fprintf(w, "+++ pending: %s\n%s\n", pendingFile, strings.TrimRight(string(pendingBody), "\n"))
		return nil
	}
	fmt.Fprintf(w, "--- live: %s\n%s\n\n", liveFile, strings.TrimRight(string(liveBody), "\n"))
	fmt.Fprintf(w, "+++ pending: %s\n%s\n", pendingFile, strings.TrimRight(string(pendingBody), "\n"))
	return nil
}

// notifyGatewayReloadHTTP mints a temporary admin API key for the agent's
// owner, POSTs to /api/agents/<id>/skills/reload, then deletes the key.
// Cross-platform — works on Windows where SIGHUP isn't deliverable.
//
// Best-effort: any failure (gateway not running, store error, HTTP error,
// auth rejection) is logged to stderr and the function returns silently.
// The skill rename already happened; a failed reload only means the
// running daemon keeps the old skills until next restart.
//
// Returns true when the reload actually hit the endpoint and got a 2xx —
// callers can use that to skip the SIGHUP fallback on Unix.
func notifyGatewayReloadHTTP(ctx context.Context, st store.Store, rec *store.AgentRecord) bool {
	if rec == nil || rec.ID == "" || rec.UserID == "" {
		return false
	}
	// Gate on the daemon actually running. Without this check we'd spin
	// up a temp API key + TCP dial every time approve is invoked from a
	// script that has no gateway attached (CI, dev sandbox, etc.).
	daemonSt, _ := daemon.GetStatus()
	if daemonSt == nil || !daemonSt.Running {
		return false
	}

	port := config.LoadEnv().Gateway.Port
	if port <= 0 {
		port = 18953
	}

	ak, err := users.NewAPIKeys(st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: couldn't open API keys to mint temp reload token: %v. Falling back to SIGHUP/restart hint.\n", err)
		return false
	}
	keyRec, token, err := ak.Create(ctx, rec.UserID, "skill-reload-tmp", users.APIKeyTypeAdmin, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: couldn't mint temp reload API key: %v. Falling back to SIGHUP/restart hint.\n", err)
		return false
	}
	// Always delete the temp key on exit — leaving stray admin keys in
	// the audit log would be noisy and a security smell.
	defer func(id string) {
		delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ak.Delete(delCtx, id); err != nil {
			fmt.Fprintf(os.Stderr, "Note: failed to clean up temp reload API key %s: %v\n", id, err)
		}
	}(keyRec.ID)

	url := fmt.Sprintf("http://localhost:%d/api/agents/%s/skills/reload", port, rec.ID)
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: couldn't build reload request: %v. Falling back to SIGHUP/restart hint.\n", err)
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: skills/reload HTTP request failed: %v. Falling back to SIGHUP/restart hint.\n", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fmt.Fprintf(os.Stderr, "Note: skills/reload HTTP returned %d (%s). Falling back to SIGHUP/restart hint.\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return false
	}
	// Surface success — parse the {ok:true} envelope so we can confirm
	// the endpoint actually reloaded (vs. a stub 200).
	var envelope struct {
		OK bool `json:"ok"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	fmt.Fprintf(os.Stderr, "Reloaded agent %s skills via HTTP (PID %d).\n", rec.ID, daemonSt.PID)
	return true
}
