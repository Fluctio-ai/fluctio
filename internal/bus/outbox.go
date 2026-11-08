package bus

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The outbox is the publish-side durability net: when XAdd keeps failing
// (Redis down, restarting, partitioned), messages would otherwise be
// dropped after the in-process retries give up. The failed payloads are
// spooled to disk — one small JSON file per message under
// $FLUCTIO_HOME/bus-outbox (falling back to ~/.fluctio/bus-outbox) — and
// a replay loop re-publishes them once Redis answers again.
//
// Semantics are at-least-once: a replayed message is removed from the
// spool only after its XAdd succeeds, but a crash between XAdd success
// and file removal can replay it twice. Losing a chat turn is worse than
// duplicating one, so the window is accepted and documented on MessageBus.
//
// Torn files (process killed mid-write) are detected at parse time,
// renamed to "*.partial" and left on disk for inspection instead of
// silently deleted.

// outboxDirDefault returns the default spool directory, mirroring
// config.HomeDir()'s FLUCTIO_HOME convention without importing the
// config package (keeps bus dependency-light).
func outboxDirDefault() string {
	if h := os.Getenv("FLUCTIO_HOME"); h != "" {
		return filepath.Join(h, "bus-outbox")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".fluctio", "bus-outbox")
	}
	return filepath.Join(os.TempDir(), "fluctio-bus-outbox")
}

type outboxEntry struct {
	Stream  string `json:"stream"`
	Payload string `json:"payload"` // base64 of the JSON-encoded message
	TS      int64  `json:"ts"`      // unix millis, diagnostics only
}

type outbox struct {
	dir string
	mu  sync.Mutex
}

func newOutbox(dir string) *outbox {
	if dir == "" {
		dir = outboxDirDefault()
	}
	return &outbox{dir: dir}
}

// enqueue spools one failed publish. The write is atomic (temp file +
// rename) so the replay loop never sees a half-written entry as a valid
// one; a crash mid-write leaves the "*.tmp" behind, which replay ignores.
func (o *outbox) enqueue(stream string, payload []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir outbox: %w", err)
	}
	entry := outboxEntry{
		Stream:  stream,
		Payload: base64.StdEncoding.EncodeToString(payload),
		TS:      time.Now().UnixMilli(),
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal outbox entry: %w", err)
	}
	name := fmt.Sprintf("msg-%d.json", time.Now().UnixNano())
	tmp := filepath.Join(o.dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return fmt.Errorf("write outbox tmp: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(o.dir, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit outbox entry: %w", err)
	}
	return nil
}

// replay feeds every spooled entry to publish, oldest first, removing
// each file only after publish reports success. It stops at the first
// failure (Redis still down or publish erroring) and leaves the rest for
// the next round; entries processed before the failure are already
// removed, so the next round resumes from there. returns (replayed, err).
func (o *outbox) replay(publish func(stream string, payload []byte) error) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("scan outbox: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "msg-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	// msg-<unixnano>.json sorts chronologically by name.
	sort.Strings(names)
	replayed := 0
	for _, name := range names {
		path := filepath.Join(o.dir, name)
		blob, err := os.ReadFile(path)
		if err != nil {
			return replayed, fmt.Errorf("read outbox entry: %w", err)
		}
		var entry outboxEntry
		if err := json.Unmarshal(blob, &entry); err != nil {
			// Torn write or corrupted entry: keep it on disk under a
			// different extension so the loop doesn't trip on it again
			// and an operator can still inspect it.
			slog.Warn("bus outbox entry corrupt; quarantined", "file", path, "error", err)
			if rerr := os.Rename(path, path+".partial"); rerr != nil {
				return replayed, fmt.Errorf("quarantine outbox entry: %w", rerr)
			}
			continue
		}
		payload, err := base64.StdEncoding.DecodeString(entry.Payload)
		if err != nil {
			slog.Warn("bus outbox entry corrupt; quarantined", "file", path, "error", err)
			if rerr := os.Rename(path, path+".partial"); rerr != nil {
				return replayed, fmt.Errorf("quarantine outbox entry: %w", rerr)
			}
			continue
		}
		if err := publish(entry.Stream, payload); err != nil {
			return replayed, fmt.Errorf("replay outbox entry %s: %w", name, err)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return replayed, fmt.Errorf("remove outbox entry: %w", err)
		}
		replayed++
	}
	return replayed, nil
}

// count reports how many spooled entries await replay. Diagnostics/tests.
func (o *outbox) count() (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "msg-") && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n, nil
}
