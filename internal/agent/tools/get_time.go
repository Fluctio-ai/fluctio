package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RegisterGetTimeTool registers get_time — the live source of "now".
//
// The system prompt only carries the conversation's START time, pinned to
// the first message's timestamp so the prompt renders byte-identically
// across turns (prefix cache). That start time goes stale as the session
// goes on. This tool returns the precise current time in the chatter's
// configured timezone, read from the same USER.md row set_timezone writes.
func RegisterGetTimeTool(r *Registry) {
	r.Register("get_time",
		"Get the precise current date and time in the chatter's timezone. "+
			"Call this whenever the user asks the current time/date, before a "+
			"time-of-day remark (e.g. 'good morning'), or when scheduling a "+
			"task. Do NOT call shell `date` or guess the time — this tool is "+
			"the only live source of 'now'.",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
			now := time.Now()
			loc := chatterTimezoneFromUserMD(ctx, r)
			// time.Local.String() is just "Local" — uninformative. When
			// no timezone is configured, surface the UTC offset instead.
			zoneLabel := loc.String()
			if zoneLabel == "Local" {
				_, off := now.In(loc).Zone()
				sign := "+"
				if off < 0 {
					sign = "-"
					off = -off
				}
				zoneLabel = fmt.Sprintf("UTC%s%02d", sign, off/3600)
			}
			return fmt.Sprintf("Current time (%s): %s\nUTC: %s",
				zoneLabel,
				now.In(loc).Format("2006-01-02 15:04:05 -0700 (Monday)"),
				now.UTC().Format("2006-01-02 15:04:05 -0700 (Monday)")), nil
		},
	)
}

// chatterTimezoneFromUserMD reads the chatter's timezone from USER.md
// (written by set_timezone as a "- Timezone: <tz>" line). Falls back to
// server-local time when unset or unreadable.
func chatterTimezoneFromUserMD(ctx context.Context, r *Registry) *time.Location {
	if r.systemFileStore != nil {
		uid := r.systemFileUserID("USER.md")
		if data, err := r.readSystemFileForUser(ctx, uid, "USER.md"); err == nil {
			if tz := extractTimezoneLine(string(data)); tz != "" {
				if loc, err := time.LoadLocation(tz); err == nil {
					return loc
				}
			}
		}
	}
	return time.Local
}

func extractTimezoneLine(userMD string) string {
	const prefix = "- Timezone: "
	_, rest, ok := strings.Cut(userMD, prefix)
	if !ok {
		return ""
	}
	end := strings.IndexAny(rest, "\n\r")
	if end < 0 {
		end = len(rest)
	}
	return strings.TrimSpace(rest[:end])
}
