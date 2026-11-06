package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceStore abstracts durable storage for user workspace files.
// Implementations can be S3, MinIO, local filesystem, or database BLOBs.
type WorkspaceStore interface {
	// List returns all file paths for a user's workspace.
	List(ctx context.Context, userID string) ([]string, error)
	// Get reads a file from durable storage.
	Get(ctx context.Context, userID, path string) (io.ReadCloser, error)
	// Put writes a file to durable storage.
	Put(ctx context.Context, userID, path string, r io.Reader) error
	// Delete removes a file from durable storage.
	Delete(ctx context.Context, userID, path string) error
}

// WorkspaceSync handles hydrating a sandbox workspace from durable storage
// and flushing changes back.
type WorkspaceSync struct {
	store WorkspaceStore
}

// NewWorkspaceSync creates a sync manager.
func NewWorkspaceSync(store WorkspaceStore) *WorkspaceSync {
	return &WorkspaceSync{store: store}
}

// safeRelKey validates a workspace store key before it is joined onto a
// local directory (or sent back to the store). It accepts only keys that
// are already clean, relative, forward-slash paths: anything the reference
// cleaner (sanitizeSandboxPath, workspace_hydrate.go) would rewrite —
// absolute paths, ".." segments, "./" noise — or that carries Windows
// separators is reported unsafe, because joining such a key onto localDir
// could escape the workspace and read/write host files. Callers skip or
// error on unsafe keys instead of letting the store decide.
func safeRelKey(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	if strings.ContainsRune(key, '\\') {
		return "", false
	}
	if clean := sanitizeSandboxPath(key); clean != key {
		return "", false
	}
	return key, true
}

// Hydrate downloads all workspace files for a user into a local directory.
// Called when a sandbox is first created for a user.
func (ws *WorkspaceSync) Hydrate(ctx context.Context, userID, localDir string) error {
	files, err := ws.store.List(ctx, userID)
	if err != nil {
		return fmt.Errorf("list workspace files: %w", err)
	}

	for _, path := range files {
		// Store keys are untrusted data — skip any key that could escape
		// localDir rather than joining it (path traversal guard).
		rel, ok := safeRelKey(path)
		if !ok {
			slog.Warn("workspace hydrate: skipping unsafe key", "user", userID, "path", path)
			continue
		}
		rc, err := ws.store.Get(ctx, userID, path)
		if err != nil {
			slog.Warn("workspace hydrate: skip file", "user", userID, "path", path, "error", err)
			continue
		}

		localPath := filepath.Join(localDir, rel)
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			rc.Close()
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(localPath), err)
		}

		f, err := os.Create(localPath)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create %s: %w", localPath, err)
		}
		_, copyErr := io.Copy(f, rc)
		f.Close()
		rc.Close()
		if copyErr != nil {
			return fmt.Errorf("write %s: %w", localPath, copyErr)
		}
	}

	slog.Info("workspace hydrated", "user", userID, "files", len(files))
	return nil
}

// Flush uploads all files from a local directory to durable storage.
// Called when a sandbox is about to be destroyed.
func (ws *WorkspaceSync) Flush(ctx context.Context, userID, localDir string) error {
	var count int
	err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		relPath, _ := filepath.Rel(localDir, path)
		relPath = filepath.ToSlash(relPath)

		// Skip hidden files and known temporaries
		if strings.HasPrefix(filepath.Base(relPath), ".") {
			return nil
		}

		// Walk Lstats entries, so a symlink still shows ModeSymlink here;
		// never upload one — os.Open would follow it and leak host files
		// outside localDir into the store.
		if info.Mode()&os.ModeSymlink != 0 {
			slog.Warn("workspace flush: skipping symlink", "user", userID, "path", relPath)
			return nil
		}
		// Same containment check as Hydrate: refuse to publish a key that
		// is not a clean relative path.
		if _, ok := safeRelKey(relPath); !ok {
			slog.Warn("workspace flush: skipping unsafe path", "user", userID, "path", relPath)
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		if err := ws.store.Put(ctx, userID, relPath, f); err != nil {
			slog.Warn("workspace flush: skip file", "user", userID, "path", relPath, "error", err)
			return nil // continue flushing other files
		}
		count++
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", localDir, err)
	}
	slog.Info("workspace flushed", "user", userID, "files", count)
	return nil
}

// SyncFile uploads a single file to durable storage. Called after write_file
// tool execution for real-time sync (optional — callers can batch via Flush).
func (ws *WorkspaceSync) SyncFile(ctx context.Context, userID, localDir, relPath string) error {
	// relPath is caller-supplied; without this check "../secrets" would be
	// read from outside localDir and uploaded into the store.
	rel, ok := safeRelKey(relPath)
	if !ok {
		return fmt.Errorf("refusing unsafe workspace path %q", relPath)
	}
	fullPath := filepath.Join(localDir, rel)
	// Belt-and-braces containment: the joined path must still resolve
	// inside localDir.
	if absBase, bErr := filepath.Abs(localDir); bErr == nil {
		if absFull, fErr := filepath.Abs(fullPath); fErr == nil {
			inside, rErr := filepath.Rel(absBase, absFull)
			if rErr != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
				return fmt.Errorf("workspace path %q escapes %q", relPath, localDir)
			}
		}
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return ws.store.Put(ctx, userID, rel, f)
}
