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
// one agent. Approve is atomic at the skills package level — the live skill
// tree is reloaded on the agent's next rescan, so we deliberately do NOT
// signal the gateway mid-command (documented in --help).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fluctio-ai/fluctio/internal/agentcli"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/skills"
	"github.com/fluctio-ai/fluctio/internal/store"
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
If a live skill already exists at the destination it is replaced. The live
skill tree is reloaded on the agent's next skill rescan — there is no need
to restart or signal the gateway for the new skill to take effect.`,
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
			return runSkillApprove(agentHome, args[0], os.Stdout)
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
	var agentID string
	if strings.TrimSpace(agentRef) == "" {
		agents, err := agentcli.List(ctx, st)
		if err != nil {
			return "", fmt.Errorf("list agents: %w", err)
		}
		switch len(agents) {
		case 0:
			return "", errors.New("no agents found; create one with `fluctio agents init`")
		case 1:
			agentID = agents[0].ID
		default:
			return "", errors.New("multiple agents found; specify one with --agent <name|agt_id>")
		}
	} else {
		rec, err := agentcli.Resolve(ctx, st, agentRef)
		if err != nil {
			return "", err
		}
		agentID = rec.ID
	}
	home, err := config.AgentHomeDir(agentID)
	if err != nil {
		return "", err
	}
	return home, nil
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
// success. The live skill tree is reloaded on the agent's next rescan — no
// gateway signal needed (see --help text on the cobra command).
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
