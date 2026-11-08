package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOutboxEnqueueReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	o := newOutbox(dir)

	if err := o.enqueue("stream-a", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if err := o.enqueue("stream-b", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if n, err := o.count(); err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2, nil", n, err)
	}

	var got []replayCall
	replayed, err := o.replay(func(stream string, payload []byte) error {
		got = append(got, replayCall{stream: stream, payload: string(payload)})
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed != 2 {
		t.Fatalf("replayed = %d, want 2", replayed)
	}
	if len(got) != 2 ||
		got[0].stream != "stream-a" || got[0].payload != `{"n":1}` ||
		got[1].stream != "stream-b" || got[1].payload != `{"n":2}` {
		t.Fatalf("replay order/content wrong: %+v", got)
	}
	// Successful replays remove the entries.
	if n, err := o.count(); err != nil || n != 0 {
		t.Fatalf("post-replay count = %d, %v; want 0, nil", n, err)
	}
	// Second replay is a no-op.
	replayed, err = o.replay(func(string, []byte) error { return nil })
	if err != nil || replayed != 0 {
		t.Fatalf("second replay = %d, %v; want 0, nil", replayed, err)
	}
}

func TestOutboxReplayStopsOnPublishFailure(t *testing.T) {
	dir := t.TempDir()
	o := newOutbox(dir)
	for _, s := range []string{"a", "b", "c"} {
		if err := o.enqueue("stream", []byte(s)); err != nil {
			t.Fatalf("enqueue %s: %v", s, err)
		}
	}

	fail := true
	n, err := o.replay(func(string, []byte) error {
		if fail {
			return errors.New("redis still down")
		}
		return nil
	})
	if err == nil {
		t.Fatal("replay should report the publish failure")
	}
	if n != 0 {
		t.Fatalf("replayed = %d before failure, want 0", n)
	}
	if got, _ := o.count(); got != 3 {
		t.Fatalf("count after failed replay = %d, want 3 (nothing lost)", got)
	}

	fail = false
	n, err = o.replay(func(string, []byte) error { return nil })
	if err != nil || n != 3 {
		t.Fatalf("recovered replay = %d, %v; want 3, nil", n, err)
	}
}

func TestOutboxQuarantinesCorruptEntry(t *testing.T) {
	dir := t.TempDir()
	o := newOutbox(dir)
	if err := o.enqueue("stream", []byte("good")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	corrupt := filepath.Join(dir, "msg-1-torn.json")
	if err := os.WriteFile(corrupt, []byte(`{"stream":"x","pay`), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	n, err := o.replay(func(stream string, payload []byte) error {
		if string(payload) != "good" {
			t.Errorf("unexpected payload %q", payload)
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("replay = %d, %v; want 1, nil (corrupt skipped, good replayed)", n, err)
	}
	if _, statErr := os.Stat(corrupt + ".partial"); statErr != nil {
		t.Errorf("corrupt entry not quarantined: %v", statErr)
	}
	if got, _ := o.count(); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}

type replayCall struct {
	stream  string
	payload string
}

// TestPublishSpoolsOnOutageAndReplays is the acceptance scenario:
// publish fails through all retries while Redis is down → the message
// must be spooled, not dropped; once Redis answers again, the replay
// loop must re-publish it and clear the spool.
func TestPublishSpoolsOnOutageAndReplays(t *testing.T) {
	dir := t.TempDir()
	stub := newStreamStub()
	stub.setDown(errors.New("dial tcp: connection refused"))
	r := testBridge(stub, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msg := InboundMessage{Channel: "telegram", AccountID: "bot", ChatID: "42", Text: "survive the outage"}
	r.publish(ctx, r.inboundKey, msg, "inbound", msg.Channel, msg.AccountID)

	if n, err := r.outbox.count(); err != nil || n != 1 {
		t.Fatalf("outbox count after failed publish = %d, %v; want 1, nil", n, err)
	}

	// Redis "recovers": XAdd starts succeeding.
	var replayed []string
	stub.mu.Lock()
	stub.down = nil
	stub.mu.Unlock()

	n, err := r.outbox.replay(func(stream string, payload []byte) error {
		if err := r.xaddPayload(ctx, stream, payload); err != nil {
			return err
		}
		replayed = append(replayed, string(payload))
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("replay = %d, %v; want 1, nil", n, err)
	}
	if len(replayed) != 1 {
		t.Fatalf("replayed payloads = %d, want 1", len(replayed))
	}
	want := mustMarshalInbound(t, msg)
	if replayed[0] != want {
		t.Errorf("replayed payload = %s, want %s", replayed[0], want)
	}
	if n, err := r.outbox.count(); err != nil || n != 0 {
		t.Fatalf("outbox count after replay = %d, %v; want 0", n, err)
	}
	// The stream actually received it through the stub.
	if got := stub.xaddPayloads(); len(got) != 1 || got[0] != want {
		t.Errorf("stub XAdd payloads = %v, want [%s]", got, want)
	}
}

// TestPublishCancelledContextSpools pins the shutdown path: when the
// context is already cancelled, the retry loop aborts after one failed
// attempt — and the in-flight message still lands in the outbox so the
// next boot replays it. Shutdown must not silently drop what it was
// about to publish.
func TestPublishCancelledContextSpools(t *testing.T) {
	dir := t.TempDir()
	stub := newStreamStub()
	stub.setDown(errors.New("dial tcp: connection refused"))
	r := testBridge(stub, dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := InboundMessage{Channel: "telegram", ChatID: "7", Text: "at shutdown"}
	r.publish(ctx, r.inboundKey, msg, "inbound", msg.Channel, msg.AccountID)

	if n, _ := r.outbox.count(); n != 1 {
		t.Fatalf("outbox count = %d, want 1 (cancelled publish must spool)", n)
	}
}
