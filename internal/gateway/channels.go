package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/fluctio-ai/fluctio/internal/bus"
	"github.com/fluctio-ai/fluctio/internal/channels"
	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/store"
)


// storeLeaser adapts store.Store to channels.Leaser. The method names
// differ (Acquire/Renew/Release on the channels side, ...ChannelLease
// on the store side) so the store can grow other lease kinds without
// renaming. Lives here rather than in the store package to keep the
// store interface IM-agnostic.
type storeLeaser struct{ st store.Store }

func (s storeLeaser) Acquire(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	return s.st.AcquireChannelLease(ctx, channel, accountID, holderID, ttl)
}
func (s storeLeaser) Renew(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	return s.st.RenewChannelLease(ctx, channel, accountID, holderID, ttl)
}
func (s storeLeaser) Release(ctx context.Context, channel, accountID, holderID string) error {
	return s.st.ReleaseChannelLease(ctx, channel, accountID, holderID)
}

// registerChannelInstance starts a channel adapter for one kind="channel"
// row in configs. The row's credential_key is what processInbound
// reverse-looks up via Store.LookupChannelByCredential to find the owner —
// keep it stable (e.g. tail of bot token, app id).
//
// `hot` controls whether the bot adapter's polling goroutine is launched
// immediately. Boot-time registration uses Register (Manager.Start
// fans out everything in one go); dashboard mutations use
// RegisterAndStart so a freshly-saved bot starts receiving updates
// without restarting the process.
func registerChannelInstance(rec store.ConfigRecord, mb *bus.MessageBus, chanMgr *channels.Manager, st store.Store, hot bool) error {
	cc := decodeChannelConfig(rec)
	switch rec.Name {
	case "telegram":
		return registerTelegramChannels(cc, mb, chanMgr, hot)
	case "discord":
		return registerDiscordChannels(cc, mb, chanMgr, hot)
	case "slack":
		return registerSlackChannels(cc, mb, chanMgr, hot)
	case "line":
		return registerLINEChannels(cc, mb, chanMgr, hot)
	case "wechat":
		return registerWeChatChannels(rec, cc, mb, chanMgr, st, hot)
	case "feishu":
		return registerFeishuChannels(cc, mb, chanMgr, hot)
	case "qq":
		return registerQQChannels(rec, cc, mb, chanMgr, st, hot)
	}
	return nil
}

// registerChannelFromRecord starts a channel adapter from a ChannelRecord.
// This is the new-table equivalent of registerChannelInstance.
func registerChannelFromRecord(rec store.ChannelRecord, mb *bus.MessageBus, chanMgr *channels.Manager, st store.Store, hot bool) error {
	cc := decodeChannelFromRecord(rec)
	switch rec.Type {
	case "telegram":
		return registerTelegramChannels(cc, mb, chanMgr, hot)
	case "discord":
		return registerDiscordChannels(cc, mb, chanMgr, hot)
	case "slack":
		return registerSlackChannels(cc, mb, chanMgr, hot)
	case "line":
		return registerLINEChannels(cc, mb, chanMgr, hot)
	case "wechat":
		cfgRec := channelRecordToConfigRecord(rec)
		return registerWeChatChannels(cfgRec, cc, mb, chanMgr, st, hot)
	case "feishu":
		return registerFeishuChannels(cc, mb, chanMgr, hot)
	case "qq":
		cfgRec := channelRecordToConfigRecord(rec)
		return registerQQChannels(cfgRec, cc, mb, chanMgr, st, hot)
	}
	return nil
}

// channelRecordToConfigRecord builds a ConfigRecord from a ChannelRecord
// for backward compatibility with registerWeChatChannels which needs
// the ConfigRecord shape for its on-expired callback.
func channelRecordToConfigRecord(ch store.ChannelRecord) store.ConfigRecord {
	return store.ConfigRecord{
		ID:            ch.ID,
		Kind:          store.KindChannel,
		UserID:        ch.UserID,
		AgentID:       ch.AgentID,
		Name:          ch.Type,
		Enabled:       ch.Enabled,
		CredentialKey: ch.AccountID,
		Data:          ch.Data,
		CreatedAt:     ch.CreatedAt,
		UpdatedAt:     ch.UpdatedAt,
	}
}

// decodeChannelFromRecord converts a ChannelRecord into a ChannelConfig.
// It reads from both the top-level fields and the Data JSON blob.
func decodeChannelFromRecord(rec store.ChannelRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	// First, decode from the Data blob (preserves the Accounts map, etc.)
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	// Override BotToken from top-level field when present.
	if rec.BotToken != "" {
		cc.BotToken = rec.BotToken
	}
	return cc
}

// register adds an adapter to the manager via the appropriate path
// (boot-time Register vs hot RegisterAndStart). Keeps the per-channel
// case branches tidy.
func register(chanMgr *channels.Manager, ch channels.Channel, hot bool) {
	if hot {
		chanMgr.RegisterAndStart(ch)
		return
	}
	chanMgr.Register(ch)
}

// registerSingleton is the polling-channel variant: every replica
// running this binary will share the (channel, accountID) lease and
// only the leaseholder's Start runs. Use for Telegram long-poll,
// WeChat iLink long-poll, Discord/Slack WS, and Feishu long-conn —
// anything where a second concurrent client would deliver duplicate
// inbound. Webhook-only adapters (LINE, Feishu webhook mode) and
// in-process fanout (Web) stay on plain register.
func registerSingleton(chanMgr *channels.Manager, ch channels.Channel, hot bool) {
	if hot {
		chanMgr.RegisterSingletonAndStart(ch)
		return
	}
	chanMgr.RegisterSingleton(ch)
}

func decodeChannelConfig(rec store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	return cc
}

func registerTelegramChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		if !chanMgr.ClaimTelegramToken(chCfg.BotToken) {
			slog.Warn("telegram token already registered in this process, skipping duplicate")
			return nil
		}
		tg, err := channels.NewTelegram(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, tg, hot)
		return nil
	}
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		if !chanMgr.ClaimTelegramToken(token) {
			slog.Warn("telegram token already registered in this process, skipping duplicate", "account", accountID)
			continue
		}
		tg, err := channels.NewTelegram(token, accountID, mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, tg, hot)
	}
	return nil
}

func registerDiscordChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		dc, err := channels.NewDiscord(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, dc, hot)
		return nil
	}
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		dc, err := channels.NewDiscord(token, accountID, mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, dc, hot)
	}
	return nil
}

func registerSlackChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		sl, err := channels.NewSlack(chCfg.BotToken, chCfg.AppToken, "", mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, sl, hot)
		return nil
	}
	for accountID, acct := range chCfg.Accounts {
		botToken := acct.BotToken
		if botToken == "" {
			botToken = chCfg.BotToken
		}
		sl, err := channels.NewSlack(botToken, chCfg.AppToken, accountID, mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, sl, hot)
	}
	return nil
}

func registerLINEChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	// LINE row carries one or more (channel_access_token, channel_secret)
	// pairs keyed by bot userId. AccountConfig.BotToken is the channel
	// access token; AccountConfig.UserID is the channel secret (used
	// for inbound HMAC verification — see channels/line.go field-mapping
	// note).
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		ln, err := channels.NewLINE(token, acct.UserID, accountID, mb)
		if err != nil {
			return err
		}
		register(chanMgr, ln, hot)
	}
	return nil
}

func registerFeishuChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	// Feishu is multi-account: one row carries one or more (app_id,
	// app_secret, verification_token) triples keyed by app_id. No
	// legacy single-bot fallback — the per-account map is the only
	// shape produced by the connect handler.
	for accountID, acct := range chCfg.Accounts {
		secret := acct.BotToken
		if secret == "" {
			secret = chCfg.BotToken
		}
		// AccountConfig.UserID carries the verification token (see
		// channels/feishu.go for the field-mapping rationale).
		// AccountConfig.EncryptKey is set when the user enabled 加密
		// 策略 in the Feishu console; empty = plaintext webhook bodies.
		// AccountConfig.UseLongConn switches the adapter to outbound
		// WebSocket mode (no public URL needed); when true the
		// verificationToken/encryptKey fields are unused.
		lk, err := channels.NewFeishu(accountID, secret, acct.UserID, acct.EncryptKey, acct.UseLongConn, accountID, mb)
		if err != nil {
			return err
		}
		// Long-conn opens a Feishu WebSocket — two replicas would
		// both subscribe to im.message.receive_v1 and the bot owner
		// would see every reply twice. Webhook mode receives via an
		// HTTP route and is already idempotent at the gateway entry
		// (HandleWebhook is called once per HTTP POST), so it skips
		// the lease.
		if acct.UseLongConn {
			registerSingleton(chanMgr, lk, hot)
		} else {
			register(chanMgr, lk, hot)
		}
	}
	return nil
}

func registerWeChatChannels(rec store.ConfigRecord, chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, st store.Store, hot bool) error {
	// WeChat is multi-account by design — every QR scan mints a new
	// (botToken, ilink_user_id, baseURL) triple keyed under a fresh
	// accountID. The legacy "no Accounts map → single bot from
	// top-level BotToken" shape doesn't apply (we never have a
	// usable top-level config; the per-account fields BaseURL +
	// UserID are required). So skip the empty-Accounts fallback.
	for accountID, acct := range chCfg.Accounts {
		if skipFailedAccount(acct) {
			slog.Info("skipping failed wechat account on boot",
				"account", accountID, "reason", acct.FailureType)
			continue
		}
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		wc, err := channels.NewWeChat(token, acct.BaseURL, acct.UserID, accountID, mb)
		if err != nil {
			return err
		}
		// On confirmed failure (polling errors / session expired /
		// server errors past threshold) the adapter exits and fires
		// onFailed; markChannelFailed persists FailureType so the next
		// restart skips re-starting a known-dead bot (the skip above),
		// and unregisters so outbound routing stops. The row is kept —
		// the UI shows a reconnect prompt; the user re-scans QR through
		// the dashboard to bind a fresh account.
		if st != nil {
			wc.OnFailed(func(deadAccount, reason string) {
				markChannelFailed(st, chanMgr, "wechat", deadAccount, reason)
			})
		}
		registerSingleton(chanMgr, wc, hot)
	}
	return nil
}

// registerQQChannels starts one QQChannel per account in the configs
// row's Accounts map. Mirrors registerWeChatChannels (contract §5.4):
//
//   - Iterates accounts, skipping any pre-marked failed.
//   - Constructs channels.NewQQChannel with AppID + ClientSecret from
//     AccountConfig (Phase 4 fields; gateway reads them as-is so the
//     connect handler can populate them later without a code change).
//   - SetAllowedChecker installs a live closure that reads the owning
//     agent's Admins["qq"] via config.AgentFileConfigLoader. The list
//     updates on every inbound message, so a freshly-claimed admin
//     takes effect without restarting the adapter (contract §5.5
//     scheme A — claim复用, no openclaw-style static allowFrom).
//   - OnFailed → markChannelFailed so a dead account surfaces a
//     reconnect prompt + skips re-start on next boot.
//   - registerSingleton: WS long-connection must be lease-guarded so
//     two replicas don't both connect and double-deliver events.
//
// `rec.AgentID` is what links the channel row to the agent whose
// Admins["qq"] we read; without it the gate is left open (dev/legacy
// rows).
func registerQQChannels(rec store.ConfigRecord, chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, st store.Store, hot bool) error {
	for accountID, acct := range chCfg.Accounts {
		if skipFailedAccount(acct) {
			slog.Info("skipping failed qq account on boot",
				"account", accountID, "reason", acct.FailureType)
			continue
		}
		q, err := channels.NewQQChannel(acct.AppID, acct.ClientSecret, accountID, mb)
		if err != nil {
			return err
		}
		// Phase 4 field wiring: AccountConfig.UseMarkdown → adapter
		// toggle. Without this the dashboard toggle is silently ignored
		// (useMarkdown defaults to false on NewQQChannel).
		q.SetUseMarkdown(acct.UseMarkdown)

		// Live claim closure. Re-reads Admins["qq"] on every inbound
		// so a /claim in another session takes effect immediately.
		if agentID := rec.AgentID; agentID != "" {
			q.SetAllowedChecker(func(openid string) bool {
				cfg, ok := config.AgentFileConfigLoader(agentID, "")
				if !ok {
					return false
				}
				for _, id := range cfg.Admins["qq"] {
					if id == openid {
						return true
					}
				}
				return false
			})
		}

		// FailureReporter callback — same shape as wechat.
		if st != nil {
			q.OnFailed(func(deadAccount, reason string) {
				markChannelFailed(st, chanMgr, "qq", deadAccount, reason)
			})
		}

		registerSingleton(chanMgr, q, hot)
	}
	return nil
}


// purgeWeChatAccount removes one account from the configs row's
// Accounts map. If the row is left empty after the removal the whole
// row gets deleted. Runs in the adapter's polling goroutine so the
// HTTP request ctx isn't available — use a fresh background ctx.
//
// Idempotent: ErrNotFound on the GetConfig lookup means the row is
// already gone (dashboard-side disconnect, or a sibling account's
// purge that emptied the row first) — that's success, not an error.
func purgeWeChatAccount(st store.Store, rowID, deadAccount string) error {
	ctx := context.Background()
	rec, err := st.GetConfig(ctx, rowID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if rec == nil {
		return nil
	}
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, mErr := json.Marshal(rec.Data); mErr == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	if _, ok := cc.Accounts[deadAccount]; !ok {
		return nil
	}
	delete(cc.Accounts, deadAccount)
	if len(cc.Accounts) == 0 {
		return st.DeleteConfig(ctx, rec.ID)
	}
	blob, mErr := json.Marshal(cc)
	if mErr != nil {
		return mErr
	}
	var data map[string]interface{}
	if mErr := json.Unmarshal(blob, &data); mErr != nil {
		return mErr
	}
	rec.Data = data
	return st.SaveConfig(ctx, rec)
}

// skipFailedAccount reports whether an account entry has been marked
// failed (FailureType set). registerXxxChannels loops use it to avoid
// re-starting an adapter that already gave up — prevents the dead
// channel from looping forever on every process restart.
func skipFailedAccount(acct config.AccountConfig) bool {
	return acct.FailureType != ""
}

// markChannelFailed persists FailureType onto the channels-table row for
// (channelType, accountID) and unregisters the adapter so outbound
// routing stops. The row is NOT deleted — the UI shows a reconnect
// prompt and the retry handler clears the flag. Idempotent: a missing
// row (already cleaned, webhook disconnect, or pre-migration data) is
// a noop that still unregisters.
func markChannelFailed(st store.Store, chanMgr *channels.Manager, channelType, accountID, reason string) {
	ctx := context.Background()
	rec, err := st.LookupChannel(ctx, channelType, accountID)
	if err != nil || rec == nil {
		// Missing row is the expected post-disconnect state — not worth
		// a warning. Still unregister so outbound routing stops.
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Debug("markChannelFailed lookup returned error",
				"type", channelType, "account", accountID, "error", err)
		}
		chanMgr.Unregister(channelType, accountID)
		return
	}
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, mErr := json.Marshal(rec.Data); mErr == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	acct, ok := cc.Accounts[accountID]
	if !ok {
		slog.Warn("markChannelFailed: account not in map",
			"type", channelType, "account", accountID)
		chanMgr.Unregister(channelType, accountID)
		return
	}
	acct.FailureType = reason
	cc.Accounts[accountID] = acct
	blob, _ := json.Marshal(cc)
	var data map[string]interface{}
	_ = json.Unmarshal(blob, &data)
	delete(data, "enabled")
	rec.Data = data
	if err := st.SaveChannel(ctx, rec); err != nil {
		slog.Warn("markChannelFailed save failed",
			"type", channelType, "account", accountID, "error", err)
	}
	chanMgr.Unregister(channelType, accountID)
}

