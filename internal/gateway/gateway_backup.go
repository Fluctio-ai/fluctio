package gateway

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/backup"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
)

// backupTicker is the gateway central ticker for scheduled SQLite
// backups: half-hourly tick, gated on backup.enabled + CronTime (UTC+8)
// + once-per-day idempotency. Reads config fresh every tick so
// PUT /api/backup takes effect without a gateway restart.
func (g *Gateway) backupTicker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	g.runBackupSchedule(ctx) // boot pass
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.runBackupSchedule(ctx)
		}
	}
}

// runBackupSchedule performs one scheduling check: load BackupCfg, bail
// if disabled, bail if today's CronTime hasn't passed yet, bail if a
// snapshot already exists for today. Otherwise create one and rotate.
func (g *Gateway) runBackupSchedule(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("backup schedule panic", "error", r)
		}
	}()
	var cfg config.BackupCfg
	if err := scope.SettingInto(ctx, g.store, NSBackup, "", "", &cfg); err != nil {
		slog.Warn("backup: read config failed", "error", err)
		return
	}
	if !cfg.Enabled {
		return
	}
	ct := strings.TrimSpace(cfg.CronTime)
	if ct == "" {
		ct = "03:00"
	}
	nowCST := time.Now().In(diaryCST)
	cronAt := parseCronTimeToday(ct, nowCST)
	if !cronAt.IsZero() && nowCST.Before(cronAt) {
		return // not yet today's backup time
	}
	if backup.TodayHasBackup() {
		return // today's snapshot already exists (auto or manual)
	}
	maxKeep := cfg.MaxKeep
	if maxKeep <= 0 {
		maxKeep = 7
	}
	name, size, err := backup.Create(ctx, g.store, nowCST)
	if err != nil {
		slog.Warn("backup: create failed", "error", err)
		return
	}
	backup.Rotate(maxKeep)
	slog.Info("scheduled backup created", "file", name, "bytes", size, "maxKeep", maxKeep)
}
