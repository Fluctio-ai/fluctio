package sandbox

// mountRootScope folds a chat's (project, session) pair to the scope the
// sandbox MOUNT ROOT maps to: project chats mount the whole project root
// (paths keep their <sid>/ prefix — the session component folds away, or
// files would nest under the chat's own sid dir), loose chats scope to
// their session subtree. Every workspace read/write/mirror that must
// agree with what /workspace shows goes through this one rule — the call
// sites used to keep private copies, which is how the exponential
// <sid>/<sid>/ nesting bug slipped in when one copy missed a fix.
func mountRootScope(projectID, sessionID string) (string, string) {
	if projectID != "" {
		return projectID, ""
	}
	return projectID, sessionID
}
