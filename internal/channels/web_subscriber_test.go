package channels

import "testing"

// TestWebChannelSubscriberCount guards the "is anyone watching this
// conversation" signal the notification mirror suppresses on. It must be
// scoped per (agent, session) and drop back to zero when the last
// subscriber unsubscribes — a stuck count would silently mute the phone
// forever for that session.
func TestWebChannelSubscriberCount(t *testing.T) {
	w := NewWebChannel()

	if got := w.SubscriberCount("a1", "s1"); got != 0 {
		t.Fatalf("no subscribers = %d, want 0", got)
	}

	_, cancelA := w.Subscribe("a1", "s1")
	if got := w.SubscriberCount("a1", "s1"); got != 1 {
		t.Fatalf("after one subscribe = %d, want 1", got)
	}

	_, cancelB := w.Subscribe("a1", "s1")
	if got := w.SubscriberCount("a1", "s1"); got != 2 {
		t.Fatalf("after two subscribes = %d, want 2", got)
	}

	// A different session must not count toward this one.
	w.Subscribe("a1", "s2")
	if got := w.SubscriberCount("a1", "s1"); got != 2 {
		t.Fatalf("unrelated session changed the count to %d, want 2", got)
	}

	cancelA()
	cancelB()
	if got := w.SubscriberCount("a1", "s1"); got != 0 {
		t.Fatalf("after unsubscribe = %d, want 0", got)
	}
}
