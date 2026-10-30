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
	// Delivery (push) target. A non-empty (channel, accountId, chatId)
	// identifies the chat the result is delivered to when the schedule
	// fires (see Deliver). Values come from list_channels and are
	// whitelist-checked.
	Channel   string `json:"channel,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
	// Background: run in the job's own cron_<id> session instead of the
	// delivery-target conversation (keeps task chatter out of the chat).
	// Implicitly true when no delivery target is given.
	Background *bool `json:"background,omitempty"`
	// Deliver: send the job's reply to the delivery target. Defaults to
	// true — jobs are built to report back. Must be false when no target
	// is given (there is nowhere to deliver).
	Deliver *bool `json:"deliver,omitempty"`
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
		"Create a scheduled task. Use this for any user request that names a specific time, an interval, or a recurring schedule (e.g. \"5 分钟后提醒\", \"every Monday 9am\", \"each day at 8\"). When the schedule fires, the agent receives `message` as a fresh inbound prompt. Two independent switches control the job: `deliver` (default true) sends the result to the delivery target (channel/accountId/chatId, copied from list_channels); `background` (default false) runs it in an isolated session so task chatter stays out of that chat. Typical choices: reminder/report → deliver=true; background upkeep whose result nobody needs → deliver=false with no target. deliver=true REQUIRES a target — set it, or the job is rejected before saving. To push: FIRST call list_channels, THEN copy the ENTIRE (channel, accountId, chatId) triple — pass all three together, never just one or two (an incomplete address can't be delivered). Do NOT write timed reminders into HEARTBEAT.md — that file is only for conditional self-checks reviewed at every heartbeat tick.",
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
					"description": "Delivery target channel (e.g. 'wechat', 'telegram', 'discord', 'feishu', 'qq'). Copy the full (channel, accountId, chatId) triple from a list_channels row — the job's result is delivered there when it fires. Required unless deliver=false (a pure background task).",
				},
				"accountId": map[string]interface{}{
					"type":        "string",
					"description": "Target bot account within the channel. Copy from the SAME list_channels row as channel and chatId — pass the full triple together. (If you omit it the bound bot is auto-resolved, but always copy the whole row to be safe.)",
				},
				"chatId": map[string]interface{}{
					"type":        "string",
					"description": "Target chat identifier (group/DM id on the target channel). Copy from the SAME list_channels row as channel and accountId.",
				},
				"background": map[string]interface{}{
					"type":        "boolean",
					"description": "Run in an isolated background session instead of the delivery-target conversation — task chatter stays out of the chat while the result is still delivered (default false; forced true when no delivery target is given).",
				},
				"deliver": map[string]interface{}{
					"type":        "boolean",
					"description": "Send the job's result to the delivery target (default true). Set false ONLY for pure background tasks — combined with an empty channel it means the result goes nowhere and nobody sees it.",
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

		// Delivery axes: where the job RUNS (background = its own
		// cron_<id> session vs. the target conversation) and whether its
		// reply is DELIVERED to the target are orthogonal. Defaults follow
		// user intent: deliver=true unless the model explicitly opts out,
		// because jobs are overwhelmingly built to report back; background
		// defaults false and is forced true when there is no target to run
		// in. A deliver=true with no target is rejected — that was the old
		// silent-flag dead-letter trap (the model believes it reported, the
		// reply went nowhere).
		deliver := args.Deliver == nil || *args.Deliver
		var channel, accountID, chatID string
		if args.Channel != "" || args.AccountID != "" || args.ChatID != "" {
			if args.Channel == "" || args.ChatID == "" {
				return "", fmt.Errorf("a delivery target needs both 'channel' and 'chatId' (accountId is auto-resolved). Call list_channels for valid values. To run purely in the background instead, omit channel/accountId/chatId and set deliver=false.")
			}
			if !deliver {
				return "", fmt.Errorf("a delivery target was given but deliver=false — either set deliver=true (default) or omit the target for a pure background task")
			}
			resolved, err := resolveChannelTarget(ctx, st, agentID, args.Channel, args.AccountID, args.ChatID)
			if err != nil {
				return "", err
			}
			channel = args.Channel
			accountID = resolved
			chatID = args.ChatID
		} else if deliver {
			return "", fmt.Errorf("deliver defaults to true but no delivery target was given: pass (channel, chatId) from list_channels, or set deliver=false explicitly for a pure background task whose result nobody receives")
		}
		background := args.Background != nil && *args.Background
		if channel == "" {
			background = true // nowhere else to run
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
			// See CronJobRecord.Background/Deliver — the orthogonal axes
			// that replaced the old silent bool.
			Background: background,
			Deliver:    deliver,
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
		if len(jobs) == 0 {
			return "No cron jobs found for this agent.", nil
		}

		// Attach each delivering job's target session (key + title) so the
		// full delivery address is visible alongside channel/accountId/chatId
		// and the background/deliver flags. Non-delivering background jobs
		// have no delivery target.
		sessions, _ := st.ListSessions(ctx, agentID)
		type sessKey struct{ channel, accountID, chatID string }
		latest := map[sessKey]store.SessionMeta{}
		for _, s := range sessions {
			if s.Channel == "" || s.ChatID == "" {
				continue
			}
			k := sessKey{s.Channel, s.AccountID, s.ChatID}
			if cur, ok := latest[k]; !ok || s.UpdatedAt.After(cur.UpdatedAt) {
				latest[k] = s
			}
		}

		type cronJobView struct {
			store.CronJobRecord
			SessionKey   string `json:"sessionKey,omitempty"`
			SessionTitle string `json:"sessionTitle,omitempty"`
		}
		views := make([]cronJobView, 0, len(jobs))
		for _, j := range jobs {
			v := cronJobView{CronJobRecord: j}
			if j.Channel != "" {
				if s, ok := latest[sessKey{j.Channel, j.AccountID, j.ChatID}]; ok {
					v.SessionKey = s.Key
					v.SessionTitle = s.Title
				}
			}
			views = append(views, v)
		}

		data, _ := json.MarshalIndent(views, "", "  ")
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

// resolveChannelTarget verifies (channel, chatId) is a real chat of this
// agent and returns the bot account_id that should deliver to it: the
// account that is bound+enabled in the channels table and has the most
// recent session for this chat. The scheduler's pre-fire check keys the
// adapter by (channel, accountID), so an empty account_id reads as
// "destination channel missing" and the job gets auto-deleted after 3
// ticks — auto-resolving from the bound account avoids depending on the
// caller to pass accountId (a model that omits it would otherwise save a
// job that can never fire). If the caller does pass accountId, it must
// match a bound session of this chat.
func resolveChannelTarget(ctx context.Context, st store.Store, agentID, channel, accountID, chatID string) (string, error) {
	all, err := st.ListAllChannels(ctx)
	if err != nil {
		return "", fmt.Errorf("verify target: list channels: %w", err)
	}
	bound := map[string]bool{}
	for _, ch := range all {
		if ch.AgentID == agentID && ch.Enabled && ch.Type == channel {
			bound[ch.AccountID] = true
		}
	}
	sessions, err := st.ListSessions(ctx, agentID)
	if err != nil {
		return "", fmt.Errorf("verify target: list sessions: %w", err)
	}
	var pickAccount string
	var pickTime time.Time
	found := false
	for _, s := range sessions {
		if s.Channel != channel || s.ChatID != chatID {
			continue
		}
		if accountID != "" && s.AccountID != accountID {
			continue
		}
		if !bound[s.AccountID] {
			continue
		}
		if !found || s.UpdatedAt.After(pickTime) {
			pickAccount = s.AccountID
			pickTime = s.UpdatedAt
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("target (channel=%q, accountId=%q, chatId=%q) has no session on a bound+enabled channel of this agent. Have the target chat message the agent once and ensure the channel is bound; call list_channels for valid channel/accountId/chatId triples.", channel, accountID, chatID)
	}
	return pickAccount, nil
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
