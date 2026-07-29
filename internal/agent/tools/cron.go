package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/cron"
	"github.com/fluctio-ai/fluctio/internal/scope"
	"github.com/fluctio-ai/fluctio/internal/store"
)

type createCronJobArgs struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Message  string `json:"message"`
	Type     string `json:"type"`
	// Optional cross-channel delivery target. When any of these is set,
	// the schedule pushes to this (channel, accountId, chatId) instead of
	// the chat the request originated from. Values must come from
	// list_channels and are whitelist-checked before saving.
	Channel   string `json:"channel,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
}

type deleteCronJobArgs struct {
	ID string `json:"id"`
}

// RegisterCronTools registers cron job management tools.
//
// Channel + chatID for the originating turn are read from the registry
// at execute time via r.MessageChannel() / r.MessageChatID() so a single
// registration at agent construction handles every chat context the
// agent runs in. The agent loop's bindSession stamps the per-turn
// values onto the registry before any tool fires.
func RegisterCronTools(r *Registry, st store.Store, userID, agentID string) {
	r.Register("create_cron_job",
		"Create a scheduled task. Use this for any user request that names a specific time, an interval, or a recurring schedule (e.g. \"5 分钟后提醒\", \"every Monday 9am\", \"each day at 8\"). When the schedule fires, the agent receives `message` as a fresh inbound prompt. By default it fires on the same channel/chat the request originated from; to push to a DIFFERENT channel (e.g. set the task from QQ but deliver to WeChat), pass the optional channel/accountId/chatId (look them up via list_channels). Do NOT write timed reminders into HEARTBEAT.md — that file is only for conditional self-checks reviewed at every heartbeat tick.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Short task name (for listing / debugging).",
				},
				"schedule": map[string]interface{}{
					"type":        "string",
					"description": "When to fire, in the CHATTER'S local timezone (the timezone shown on the 'Current date/time' line of your system prompt) — write '每天早上 9 点' as '0 9 * * *' directly, do NOT convert to UTC. For type='cron': a 5-field cron expression like '0 9 * * *'. For type='interval': a duration like '5m' / '30m' / '2h'. For type='once': an ISO-8601 datetime like '2026-05-02T15:56:52' (no offset = chatter's local time; an explicit offset like '+08:00' or 'Z' is honored as written).",
				},
				"message": map[string]interface{}{
					"type":        "string",
					"description": "The prompt the agent should receive when the schedule fires. Phrase it as instructions to yourself (e.g. \"提醒小m喝水\"), not as a user-facing message — the agent will compose the user reply when it processes the inbound.",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Schedule type. Use 'once' for one-shot reminders ('5 分钟后…'), 'cron' for calendar-style recurring schedules ('每天 9 点'), or 'interval' for fixed-period polling ('每 30 分钟检查一次'). Defaults to 'cron'.",
					"enum":        []string{"cron", "interval", "once"},
				},
				"channel": map[string]interface{}{
					"type":        "string",
					"description": "Optional target channel to deliver to when the schedule fires (e.g. 'wechat', 'telegram', 'discord', 'feishu', 'qq'). If omitted, the task fires on the current chat's channel. Must be a channel bound to this agent — call list_channels to get valid values.",
				},
				"accountId": map[string]interface{}{
					"type":        "string",
					"description": "Optional target bot account within the channel. Copy from list_channels. Provide together with channel/chatId when targeting another channel (especially if the channel has multiple accounts).",
				},
				"chatId": map[string]interface{}{
					"type":        "string",
					"description": "Optional target chat identifier (group/DM id on the target channel). Copy from list_channels. Required when channel is set.",
				},
			},
			"required": []string{"name", "schedule", "message"},
		},
		makeCreateCronJob(st, r, userID, agentID),
	)

	r.Register("list_cron_jobs",
		"List all scheduled tasks for this agent.",
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		makeListCronJobs(st, userID, agentID),
	)

	r.Register("delete_cron_job",
		"Delete a scheduled task by ID.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "The cron job ID to delete",
				},
			},
			"required": []string{"id"},
		},
		makeDeleteCronJob(st, userID),
	)
}

func makeCreateCronJob(st store.Store, r *Registry, userID, agentID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args createCronJobArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Name == "" || args.Schedule == "" || args.Message == "" {
			return "", fmt.Errorf("name, schedule, and message are required")
		}
		jobType := args.Type
		if jobType == "" {
			jobType = "cron"
		}

		// Delivery target. Default to the originating chat (bindSession
		// stamps the current turn's channel/chatID on the registry), so a
		// plain "5 分钟后提醒" routes back to where the user asked. If the
		// caller passed an explicit target (cross-channel push, e.g. set
		// from QQ but deliver to WeChat), validate it against this agent's
		// own channels first (whitelist) — see list_channels.
		channel := r.MessageChannel()
		accountID := r.MessageAccountID()
		chatID := r.MessageChatID()
		if args.Channel != "" || args.AccountID != "" || args.ChatID != "" {
			if args.Channel == "" || args.ChatID == "" {
				return "", fmt.Errorf("when targeting another channel, both 'channel' and 'chatId' are required (plus 'accountId' if the channel has multiple accounts). Call list_channels for valid values.")
			}
			if err := validateChannelTarget(ctx, st, agentID, args.Channel, args.AccountID, args.ChatID); err != nil {
				return "", err
			}
			channel = args.Channel
			accountID = args.AccountID
			chatID = args.ChatID
		}

		// The chatter's effective timezone governs how the schedule is
		// read: zone-less 'once' datetimes and cron wall-clock fields
		// both mean "their local time" (the same zone the system
		// prompt's date line is rendered in), not the server's. The
		// resolved name is frozen onto the row so the scheduler keeps
		// evaluating recurrences in it even if the chatter later moves.
		tzName := scope.Timezone(ctx, st, r.EffectiveUserID(), agentID)
		loc := scope.LoadLocationOrLocal(tzName)

		id := generateUUID()
		now := time.Now()

		// Calculate NextRun based on type
		var nextRun time.Time
		switch jobType {
		case "once":
			t, err := time.Parse(time.RFC3339, args.Schedule)
			if err != nil {
				// No explicit offset — interpret in the chatter's zone.
				t, err = time.ParseInLocation("2006-01-02T15:04:05", args.Schedule, loc)
				if err != nil {
					return "", fmt.Errorf("once schedule must be ISO datetime (e.g. 2026-05-06T15:30:00), got: %q", args.Schedule)
				}
			}
			if t.Before(now) {
				return "", fmt.Errorf("schedule is in the past: %s", args.Schedule)
			}
			nextRun = t
		case "interval":
			sched := strings.TrimPrefix(args.Schedule, "every ")
			dur, err := time.ParseDuration(sched)
			if err != nil {
				return "", fmt.Errorf("invalid interval (e.g. '30m', '1h', 'every 2h'): %q", args.Schedule)
			}
			nextRun = now.Add(dur)
		default:
			// cron expression — first occurrence in the chatter's zone.
			// (Previously nextRun=now, which fired the job once
			// immediately on creation — a spurious reminder.)
			nextRun = cron.NextOccurrenceIn(args.Schedule, now, loc)
		}

		job := &store.CronJobRecord{
			ID:        id,
			AgentID:   agentID,
			Name:      args.Name,
			Type:      jobType,
			Schedule:  args.Schedule,
			Message:   args.Message,
			Channel:   channel,
			AccountID: accountID,
			ChatID:    chatID,
			// "" = server-local; the scheduler's LocationOf maps it
			// the same way LoadLocationOrLocal did above, so creation
			// and recurrence agree.
			Timezone:  tzName,
			Enabled:   true,
			NextRun:   &nextRun,
			CreatedAt: now,
		}

		if err := st.SaveCronJob(ctx, job); err != nil {
			return "", fmt.Errorf("save cron job: %w", err)
		}

		// Wake the scheduler to pick up this new job
		cron.NotifyJobCreated()

		// Echo the effective timezone + first fire so the model can
		// confirm the local-time interpretation to the user ("好的，
		// 北京时间每天 9 点") instead of guessing.
		tzShown := tzName
		if tzShown == "" {
			tzShown = loc.String() + " (server default)"
		}
		return fmt.Sprintf("Cron job created successfully.\nID: %s\nName: %s\nSchedule: %s\nType: %s\nTimezone: %s\nFirst fire: %s",
			id, args.Name, args.Schedule, jobType, tzShown, nextRun.In(loc).Format("2006-01-02 15:04:05 -0700")), nil
	}
}

func makeListCronJobs(st store.Store, userID, agentID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		jobs, err := st.ListCronJobsByAgent(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("list cron jobs: %w", err)
		}
		filtered := jobs

		if len(filtered) == 0 {
			return "No cron jobs found for this agent.", nil
		}

		data, _ := json.MarshalIndent(filtered, "", "  ")
		return string(data), nil
	}
}

func makeDeleteCronJob(st store.Store, userID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args deleteCronJobArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		if err := st.DeleteCronJob(ctx, args.ID); err != nil {
			return "", fmt.Errorf("delete cron job: %w", err)
		}
		return fmt.Sprintf("Cron job %s deleted.", args.ID), nil
	}
}

// validateChannelTarget enforces the cross-channel whitelist: the target
// (channel, accountId, chatId) must be a chat that has actually conversed
// with this agent (a session row exists for it). This prevents a mistaken
// or prompt-injected request from scheduling delivery to an arbitrary
// foreign chat. A chat becomes targetable once the user has messaged the
// agent from it at least once — which is also when list_channels can show
// its chatId.
func validateChannelTarget(ctx context.Context, st store.Store, agentID, channel, accountID, chatID string) error {
	sessions, err := st.ListSessions(ctx, agentID)
	if err != nil {
		return fmt.Errorf("verify target channel: list sessions: %w", err)
	}
	for _, s := range sessions {
		if s.Channel == channel && s.ChatID == chatID && (accountID == "" || s.AccountID == accountID) {
			return nil
		}
	}
	return fmt.Errorf("target (channel=%q, accountId=%q, chatId=%q) is not a chat of this agent. The target chat must have messaged this agent at least once; call list_channels to see valid channel/accountId/chatId triples.", channel, accountID, chatID)
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
