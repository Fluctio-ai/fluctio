package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/workspace"
)

// hydrateWorkspace copies every object from the workspace Store (S3 / local
// FS / whatever) into the sandbox's /workspace directory. Called once per
// sandbox creation so that when an agent's first `exec` runs, the sandbox
// already contains the files written via `write_file` in past sessions.
//
// Implementation is single-threaded and best-effort — any per-file error is
// logged and skipped rather than failing the whole hydrate. The typical
// workspace is a handful of files (MB-range PDFs, audio clips); for larger
// setups consider paginated / parallel copy, or E2B's snapshot API.
func hydrateWorkspace(ctx context.Context, ws workspace.Store, ex Executor, agentID, projectID, sessionID, sandboxRoot string) {
	if ws == nil || ex == nil {
		return
	}
	// For project chats, hydrate the whole project (List with session=""),
	// so paths keep their <sid>/ prefix and land exactly where the docker
	// bind mount already exposes them (/workspace/<sid>/...). Listing the
	// per-chat subdir instead FLATTENS its files onto the project root —
	// a duplicate of every file the mount already shows, and old-layout
	// per-chat files resurface at the root as apparent new files. Mirrors
	// E2BExecutor.Hydrate's listProject/listSession folding; LocalFS
	// deployments no-op through the size-skip below.
	listProject, listSession := mountRootScope(projectID, sessionID)
	objs, err := ws.List(ctx, agentID, listProject, listSession)
	if err != nil {
		slog.Warn("workspace hydrate: list failed", "agent", agentID, "project", projectID, "session", sessionID, "error", err)
		return
	}
	if len(objs) == 0 {
		return
	}
	copied := 0
	skipped := 0
	for _, obj := range objs {
		target := path.Join(sandboxRoot, obj.Path)
		rc, getErr := ws.Get(ctx, agentID, listProject, listSession, obj.Path)
		if getErr != nil {
			slog.Warn("workspace hydrate: get failed", "agent", agentID, "project", projectID, "session", sessionID, "path", obj.Path, "error", getErr)
			continue
		}
		content, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			slog.Warn("workspace hydrate: read failed", "agent", agentID, "project", projectID, "session", sessionID, "path", obj.Path, "error", readErr)
			continue
		}
		// Skip files that already exist byte-for-byte (same size). The
		// docker sandbox bind-mounts /workspace to the host session dir, so
		// files written earlier (writeWorkspaceBytes) are already on disk;
		// rewriting them unconditionally wipes mtime to the hydrate moment,
		// which breaks any caller that attributes files by mtime.
		statOut, statErr := ex.Exec(ctx, fmt.Sprintf("stat -c %%s %q 2>/dev/null || echo -1", target), 5*time.Second)
		if statErr == nil && strings.TrimSpace(statOut) == strconv.Itoa(len(content)) {
			skipped++
			continue
		}
		if _, wErr := ex.WriteFile(ctx, target, string(content)); wErr != nil {
			slog.Warn("workspace hydrate: sandbox write failed", "agent", agentID, "project", projectID, "session", sessionID, "path", target, "error", wErr)
			continue
		}
		copied++
	}
	slog.Info("workspace hydrated into sandbox", "agent", agentID, "project", projectID, "session", sessionID, "files", copied, "skipped", skipped, "root", sandboxRoot)
}

// defaultSandboxRoot is where hydrated files land inside the sandbox. Kept
// as a constant so we don't have to thread config through two packages just
// for a single path. E2B and our Docker sandboxes both mount their working
// dir at /workspace by convention.
const defaultSandboxRoot = "/workspace"

// sanitizeSandboxPath strips leading slashes / `..` segments so hydrated
// keys can't escape /workspace even if the store somehow holds a malicious
// path. Mirror of internal/workspace.LocalFS.resolvePath's logic.
func sanitizeSandboxPath(p string) string {
	clean := path.Clean("/" + p)
	return strings.TrimPrefix(clean, "/")
}
