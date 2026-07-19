package store

import "context"

// chatterUserIDCtxKey tags ctx with the resolved per-turn chatter
// userID so DBStore writes (AppendSessionMessage / SaveSession /
// AppendSessionEvent) can persist it without changing every callsite's
// signature. The agent loop sets this at the top of HandleMessage /
// HandleMessageStream; everything downstream just propagates ctx.
type chatterUserIDCtxKey struct{}


// ChatterUserIDFromContext returns the chatter userID set by
// WithChatterUserID, or "" if none. Store implementations should
// COALESCE the result into the chatter_user_id column on session
// writes; an empty value (background ctx, untagged code path) writes
// '' and readers fall back to user_id at query time.
func ChatterUserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(chatterUserIDCtxKey{}).(string)
	return v
}

// channelCtxKey tags ctx with the inbound channel name (e.g.
// "telegram", "discord", "web") so downstream writers like the usage
// meter can stamp it without signature changes.
type channelCtxKey struct{}

// WithChannel returns ctx tagged with the inbound channel name.
// Empty ch is a no-op.
func WithChannel(ctx context.Context, ch string) context.Context {
	if ch == "" {
		return ctx
	}
	return context.WithValue(ctx, channelCtxKey{}, ch)
}

// ChannelFromContext returns the channel name set by WithChannel, or "".
func ChannelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(channelCtxKey{}).(string)
	return v
}
