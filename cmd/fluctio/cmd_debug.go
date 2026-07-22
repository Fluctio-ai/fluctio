package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fluctio-ai/fluctio/internal/diag"
)

func debugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Diagnostic helpers for agent failures",
	}
	cmd.AddCommand(debugWhyFailedCmd())
	return cmd
}

func debugWhyFailedCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "why-failed <agent-id> <session-key>",
		Short: "Attribute the root cause of a failed agent turn",
		Long: `Attribute the root-cause step of a failed agent turn from the
local fluctio.db, using the heuristic rules in
specs/2026-07-22-heuristic-failure-attribution.md.

Reads llm_call_diag (per-LLM-call status/errors) + session_events (tool
calls/results) for the session, merges them into a time-sorted timeline, and
applies attribution rules to localize the root cause (not just the visible
error). Conversation history lives in session_messages, which this never
touches — so running it is safe on a live DB.

Examples:
  fluctio debug why-failed agt_xxx s-yyy
  fluctio debug why-failed agt_xxx s-yyy --db /path/to/fluctio.db`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID, sessionKey := args[0], args[1]
			st, closeFn, err := openStoreAt(dbPath)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := context.Background()

			llm, err := st.ListLLMCallDiagBySession(ctx, sessionKey)
			if err != nil {
				return fmt.Errorf("list llm_call_diag: %w", err)
			}
			events, err := st.ListSessionEventsSince(ctx, agentID, sessionKey, -1)
			if err != nil {
				return fmt.Errorf("list events: %w", err)
			}
			if len(llm) == 0 && len(events) == 0 {
				return fmt.Errorf("no diagnostic data for agent=%s session=%s", agentID, sessionKey)
			}

			tl := diag.BuildTimeline(llm, events)
			rc, ok := diag.Attribute(tl)
			fmt.Print(diag.RenderReport(agentID, sessionKey, tl, rc, ok))
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", "sqlite DB path (default ~/.fluctio/fluctio.db)")
	return cmd
}
