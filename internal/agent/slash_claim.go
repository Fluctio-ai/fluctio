package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/config"
)

// slashClaim redeems a web-generated verification code to bind the
// chatter's IM platform ID as an admin for this channel. Always allowed —
// /claim is NOT in writeSlashCommands: the owner must be able to claim
// BEFORE being recognized as admin (chicken-and-egg); the code itself is
// the abuse gate.
func (a *Agent) slashClaim(msg bus.InboundMessage) slashResult {
	if msg.Channel == "web" || msg.Channel == "api" {
		return slashResult{handled: true, reply: "⚠️ `/claim` 只能在 IM 渠道（微信 / Telegram / Discord 等）使用。网页端无需绑定。"}
	}
	parts := strings.Fields(msg.Text)
	if len(parts) < 2 || parts[1] == "" {
		return slashResult{handled: true, reply: "用法：`/claim <code>` —— 从 agent owner 的网页「绑定 IM 身份」面板获取 6 位验证码。"}
	}
	if a.dataStore == nil {
		return slashResult{handled: true, reply: "❌ 验证失败：系统未就绪，请稍后重试。"}
	}
	ctx := context.Background()
	ok, err := a.dataStore.RedeemIMClaim(ctx, a.agentID, msg.Channel, parts[1])
	if err != nil {
		return slashResult{handled: true, reply: "❌ 验证失败，请稍后重试。"}
	}
	if !ok {
		return slashResult{handled: true, reply: "❌ 验证码无效或已过期。请向 owner 确认最新验证码（10 分钟内有效，仅限一次）。"}
	}
	if err := a.persistAdminImID(ctx, msg.Channel, msg.UserID); err != nil {
		return slashResult{handled: true, reply: "❌ 绑定失败，请稍后重试。"}
	}
	return slashResult{handled: true, reply: fmt.Sprintf("✅ 已将你绑定为本 agent 在 `%s` 渠道的管理员，现在可以使用 `/yes`、`/new` 等命令。", msg.Channel)}
}

// persistAdminImID appends platformID to the agent's Admins[channel] in
// agents.config (DB) AND the in-memory cache, so subsequent turns recognize
// the chatter as admin. Deduped.
func (a *Agent) persistAdminImID(ctx context.Context, channel, platformID string) error {
	if a.dataStore == nil {
		return fmt.Errorf("persistAdminImID: no dataStore")
	}
	rec, err := a.dataStore.GetAgent(ctx, a.agentID)
	if err != nil {
		return err
	}
	cfg := &config.AgentFileConfig{}
	if len(rec.Config) > 0 {
		blob, _ := json.Marshal(rec.Config)
		_ = json.Unmarshal(blob, cfg)
	}
	if cfg.Admins == nil {
		cfg.Admins = map[string][]string{}
	}
	if !slices.Contains(cfg.Admins[channel], platformID) {
		cfg.Admins[channel] = append(cfg.Admins[channel], platformID)
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var asMap map[string]any
	if err := json.Unmarshal(blob, &asMap); err != nil {
		return err
	}
	rec.Config = asMap
	rec.UpdatedAt = time.Now().UTC()
	if err := a.dataStore.SaveAgent(ctx, rec); err != nil {
		return err
	}
	// In-memory cache so this process recognizes the chatter immediately.
	if a.admins == nil {
		a.admins = map[string][]string{}
	}
	if !slices.Contains(a.admins[channel], platformID) {
		a.admins[channel] = append(a.admins[channel], platformID)
	}
	return nil
}
