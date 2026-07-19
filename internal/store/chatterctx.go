package store

import "context"


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
