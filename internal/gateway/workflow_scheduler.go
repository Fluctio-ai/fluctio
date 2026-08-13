package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/cron"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// runWorkflowScheduler polls workflow_schedules once a minute and fires due
// ones (spec decision 16): each fire resolves the owning agent and runs the
// workflow with owner="system", session="". Cron expressions are evaluated in
// Asia/Shanghai (UTC+8) — spec decision 16 mandates UTC+8, not server-local,
// so the same schedule fires at the same wall-clock time regardless of the
// host's TZ (the exec-sh-shell TZ-reversal trap doesn't apply: this is Go's
// time package, not MSYS `date`).
func (g *Gateway) runWorkflowScheduler(ctx context.Context) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		slog.Warn("workflow scheduler: Asia/Shanghai unavailable, using UTC", "error", err)
		loc = time.UTC
	}
	slog.Info("workflow scheduler started", "tz", loc.String(), "poll", time.Minute)
	g.processDueWorkflowSchedules(ctx, loc) // boot probe — clear any backlog
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.processDueWorkflowSchedules(ctx, loc)
		}
	}
}

// processDueWorkflowSchedules locks + fires every schedule due at now.
func (g *Gateway) processDueWorkflowSchedules(ctx context.Context, loc *time.Location) {
	dbs, ok := g.store.(*store.DBStore)
	if !ok || dbs == nil {
		return
	}
	now := time.Now()
	due, err := dbs.GetDueWorkflowSchedules(ctx, now)
	if err != nil {
		slog.Warn("workflow scheduler: get due", "error", err)
		return
	}
	for _, s := range due {
		locked, err := dbs.LockWorkflowSchedule(ctx, s.ID, "gateway")
		if err != nil {
			slog.Warn("workflow scheduler: lock", "id", s.ID, "error", err)
			continue
		}
		if !locked {
			continue // another instance / stale lock
		}
		g.fireWorkflowSchedule(ctx, dbs, s, now, loc)
	}
}

// fireWorkflowSchedule runs one schedule's workflow and bumps next_run. The
// next_run advance happens even if the agent/workflow is gone, so a broken
// schedule doesn't re-fire on every tick — disable or delete it to stop.
func (g *Gateway) fireWorkflowSchedule(ctx context.Context, dbs *store.DBStore, s store.WorkflowScheduleRow, now time.Time, loc *time.Location) {
	advance := func() {
		next := cron.NextOccurrenceIn(s.CronExpr, now, loc).UTC().Format(time.RFC3339)
		_ = dbs.UpdateWorkflowScheduleRun(ctx, s.ID, now.UTC().Format(time.RFC3339), next)
	}

	sp, err := g.UserSpaceForCtx(ctx, s.OwnerUserID)
	if err != nil || sp == nil {
		slog.Warn("workflow scheduler: owner user space", "owner", s.OwnerUserID, "error", err)
		advance()
		return
	}
	ag := sp.Agents.AgentByID(s.AgentID)
	if ag == nil {
		slog.Warn("workflow scheduler: agent not found", "agent", s.AgentID, "schedule", s.ID)
		advance()
		return
	}
	res, err := ag.RunWorkflow(ctx, s.WorkflowID, s.Input, "system", "")
	if err != nil {
		slog.Warn("workflow scheduler: fire", "schedule", s.ID, "workflow", s.WorkflowID, "error", err)
	} else if res != nil {
		slog.Info("workflow schedule fired", "schedule", s.ID, "workflow", s.WorkflowID, "status", res.Status)
	}
	advance()
}
