package store

import (
	"context"
	"testing"
	"time"
)

// TestRecordAndPruneLLMCallDiag verifies RecordLLMCallDiag round-trips every
// field and PruneLLMCallDiag deletes only rows past the cutoff.
func TestRecordAndPruneLLMCallDiag(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Recent successful call — exercises every field.
	if err := db.RecordLLMCallDiag(ctx, LLMCallDiag{
		AgentID: "a", SessionKey: "s-1",
		Provider: "openai", Model: "gpt-x",
		Status: "ok", HTTPStatus: 200,
		DurationMs: 1234, ToolCallCount: 2, ResponseChars: 500,
		RequestMsgCount: 5, HasImage: true,
		InputTokens: 100, OutputTokens: 50,
	}); err != nil {
		t.Fatalf("RecordLLMCallDiag ok: %v", err)
	}

	// A failed call we'll backdate into the prune window.
	if err := db.RecordLLMCallDiag(ctx, LLMCallDiag{
		AgentID: "a", SessionKey: "s-1",
		Provider: "openai", Model: "gpt-x",
		Status: "error", HTTPStatus: 500, ErrorMsg: "boom",
	}); err != nil {
		t.Fatalf("RecordLLMCallDiag err: %v", err)
	}
	oldStr := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := db.db.ExecContext(ctx,
		`UPDATE llm_call_diag SET created_at = ? WHERE status = 'error'`, oldStr); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Prune older than 1h — kills the backdated error, keeps the ok.
	n, err := db.PruneLLMCallDiag(ctx, time.Now().Add(-time.Hour), 1000)
	if err != nil {
		t.Fatalf("PruneLLMCallDiag: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (the backdated error row)", n)
	}

	// The ok row survived; verify every field round-tripped.
	var (
		status                                          string
		httpStatus, toolCalls, respChars, reqMsgs      int
		hasImg, inTok, outTok                           int
		durMs                                           int64
	)
	err = db.db.QueryRowContext(ctx,
		`SELECT status, http_status, duration_ms, tool_call_count, response_chars,
		        request_msg_count, has_image, input_tokens, output_tokens
		 FROM llm_call_diag WHERE status='ok'`).
		Scan(&status, &httpStatus, &durMs, &toolCalls, &respChars,
			&reqMsgs, &hasImg, &inTok, &outTok)
	if err != nil {
		t.Fatalf("read ok row: %v", err)
	}
	if status != "ok" || httpStatus != 200 || durMs != 1234 || toolCalls != 2 ||
		respChars != 500 || reqMsgs != 5 || hasImg != 1 || inTok != 100 || outTok != 50 {
		t.Errorf("ok row fields mismatch: status=%s http=%d dur=%d tools=%d chars=%d msgs=%d img=%d in=%d out=%d",
			status, httpStatus, durMs, toolCalls, respChars, reqMsgs, hasImg, inTok, outTok)
	}
}

// TestPruneLLMCallDiagIndexExists verifies both retention indexes were created
// — without them the time- and status-based DELETEs scan the whole table.
func TestPruneLLMCallDiagIndexExists(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	rows, err := db.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='llm_call_diag'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()
	want := map[string]bool{
		"idx_llm_call_diag_created": false,
		"idx_llm_call_diag_status":  false,
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for idx, found := range want {
		if !found {
			t.Errorf("%s missing", idx)
		}
	}
}

// TestListLLMCallDiagBySession verifies the read-back path round-trips every
// field and scopes to one session (a different session's rows don't leak in).
func TestListLLMCallDiagBySession(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := db.RecordLLMCallDiag(ctx, LLMCallDiag{
		AgentID: "a", SessionKey: "s-list", Provider: "p", Model: "m",
		Status: "error", HTTPStatus: 503, ErrorMsg: "busy",
		DurationMs: 500, ToolCallCount: 1, ResponseChars: 0,
		RequestMsgCount: 3, InputTokens: 10, OutputTokens: 5,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// A different session's row must not appear.
	if err := db.RecordLLMCallDiag(ctx, LLMCallDiag{
		AgentID: "a2", SessionKey: "s-other", Status: "ok",
	}); err != nil {
		t.Fatalf("Record other: %v", err)
	}

	rows, err := db.ListLLMCallDiagBySession(ctx, "s-list")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len=%d want 1 (only s-list)", len(rows))
	}
	r := rows[0]
	if r.Status != "error" || r.HTTPStatus != 503 || r.AgentID != "a" ||
		r.Provider != "p" || r.Model != "m" || r.ErrorMsg != "busy" ||
		r.DurationMs != 500 || r.ToolCallCount != 1 || r.RequestMsgCount != 3 ||
		r.InputTokens != 10 || r.OutputTokens != 5 {
		t.Errorf("fields mismatch: %+v", r)
	}
	if r.CreatedAt.IsZero() {
		t.Errorf("CreatedAt not populated")
	}
}
