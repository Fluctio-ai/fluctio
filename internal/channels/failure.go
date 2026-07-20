package channels

import "time"

// FailureReporter is implemented by channels that detect their own
// connection failures and want the framework to mark them failed and
// surface a reconnect prompt. Optional — channels backed by 3rd-party
// libraries that hide failures (telegram/discord/slack) or webhook-only
// adapters (line, feishu-webhook) simply don't implement it, and the
// gateway skips wiring the callback.
type FailureReporter interface {
	// OnFailed registers the framework callback to fire when the
	// adapter has decided the account is unrecoverable without user
	// action. reason is a stable enum string (polling_failed /
	// session_expired / server_error) stored on AccountConfig and
	// rendered by the UI.
	OnFailed(fn func(accountID, reason string))
}

// FailureCounter tracks consecutive failures with exponential backoff.
// A channel calls Hit() on each failure; when Hit returns true the
// threshold has been reached and the channel should fire its OnFailed
// callback and exit its Start loop. Reset() is called on any success so
// transient blips don't accumulate.
//
// Provided so new self-polled channels (e.g. a future IM that owns its
// own polling loop like wechat) can share the threshold+backoff policy
// without reimplementing it.
type FailureCounter struct {
	failures  int
	threshold int
	initial   time.Duration
	max       time.Duration
}

// NewFailureCounter builds a counter that trips after `threshold`
// consecutive failures, with exponential backoff starting at `initial`
// and capped at `max`.
func NewFailureCounter(threshold int, initial, max time.Duration) *FailureCounter {
	return &FailureCounter{threshold: threshold, initial: initial, max: max}
}

// Hit increments the failure count and reports whether the threshold
// has been reached.
func (f *FailureCounter) Hit() bool {
	f.failures++
	return f.failures >= f.threshold
}

// Count returns the current consecutive-failure count (for logging).
func (f *FailureCounter) Count() int { return f.failures }

// Reset clears the counter. Call on any success.
func (f *FailureCounter) Reset() { f.failures = 0 }

// Backoff returns an exponentially growing delay capped at max:
// initial, 2*initial, 4*initial, … up to max. Matches the legacy
// WeChat calcBackoff shape (start at initial, double per failure, cap).
func (f *FailureCounter) Backoff() time.Duration {
	d := f.initial
	for i := 1; i < f.failures; i++ {
		d *= 2
		if d > f.max {
			return f.max
		}
	}
	return d
}
