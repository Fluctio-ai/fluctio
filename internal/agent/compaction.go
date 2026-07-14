package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

const (
	// DefaultTokenThreshold is the default threshold at which compaction triggers (80K tokens).
	DefaultTokenThreshold = 80000
	// PruneTurnAge is the number of recent turns to keep intact; older messages get pruned.
	PruneTurnAge = 20
	// truncatedPlaceholder replaces pruned tool results.
	truncatedPlaceholder = "[Result truncated - see memory logs]"
	// maxRetainedToolResultBytes caps a single tool-result message kept
	// in the preserved recent tail (and in short histories that skip
	// the age-based prune). The tail is exempt from the ordinary prune
	// so the model keeps live tool context, but a single multi-MB
	// result (huge file dump, verbose API response) blows past every
	// model's context window — compaction can never bring the context
	// back under threshold, so the LLM returns an empty response every
	// turn. Keep the head + marker so the model still sees the result
	// shape without the bulk.
	maxRetainedToolResultBytes = 16384
)

// EstimateTokens provides a rough token estimate: chars/4.
func EstimateTokens(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments) / 4
			total += len(tc.Function.Name) / 4
		}
	}
	return total
}

// CompactResult holds the result of a compaction operation.
type CompactResult struct {
	Messages     []provider.Message
	Pruned       bool
	LogFile      string
	TokensBefore int // token count before compaction (for Phase 3 notice)
	TokensAfter  int // token count after compaction (for Phase 3 notice)
}

// CompactMessages prunes and optionally compresses the message history when it exceeds the token threshold.
// Step 1 (Pruning): For messages older than PruneTurnAge, strip tool result content.
// Step 2 (Compression): If still over threshold after pruning, summarize older messages
// using the LLM and write full history to a log file.
func CompactMessages(messages []provider.Message, workspace string, prov provider.Provider, model string, threshold int) (*CompactResult, error) {
	tokens := EstimateTokens(messages)
	if threshold <= 0 {
		threshold = DefaultTokenThreshold // defensive: caller passed 0
	}
	if tokens < threshold {
		return &CompactResult{Messages: messages, TokensBefore: tokens, TokensAfter: tokens}, nil
	}

	slog.Info("context compaction triggered", "tokens", tokens, "threshold", threshold, "message_count", len(messages))

	// Write full history to log file before any modifications
	logFile, err := writeHistoryLog(messages, workspace)
	if err != nil {
		slog.Warn("failed to write history log", "error", err)
	}

	// Step 1: Pruning - strip tool results from older messages
	pruned := pruneOldToolResults(messages)
	prunedTokens := EstimateTokens(pruned)

	slog.Info("after pruning", "tokens_before", tokens, "tokens_after", prunedTokens)

	if prunedTokens < threshold {
		return &CompactResult{
			Messages:     pruned,
			Pruned:       true,
			LogFile:      logFile,
			TokensBefore: tokens,
			TokensAfter:  prunedTokens,
		}, nil
	}

	// Step 2: Compression - summarize older messages
	compressed, err := compressOlderMessages(pruned, prov, model)
	if err != nil {
		slog.Warn("compression failed, using pruned messages", "error", err)
		return &CompactResult{
			Messages:     pruned,
			Pruned:       true,
			LogFile:      logFile,
			TokensBefore: tokens,
			TokensAfter:  prunedTokens,
		}, nil
	}

	compressedTokens := EstimateTokens(compressed)
	slog.Info("after compression", "tokens_before", prunedTokens, "tokens_after", compressedTokens)

	return &CompactResult{
		Messages:     compressed,
		Pruned:       true,
		LogFile:      logFile,
		TokensBefore: tokens,
		TokensAfter:  compressedTokens,
	}, nil
}

// safeCompactionCutoff advances cutoff forward past any leading tool
// messages so the recent tail never starts with a "tool" role. If we
// shipped a list of the form [summary_user, tool, ...] to the
// provider, OpenAI-compatible APIs would reject with:
//
//	"Messages with role 'tool' must be a response to a preceding
//	 message with 'tool_calls'"
//
// — the previous assistant.tool_calls that "tool" was answering got
// swallowed into the summary on its own side of cutoff. Anthropic
// won't 400 on this but the contract is the same: a tool reply
// without its parent call is semantic garbage.
//
// We only need to step past leading tool messages. If the tail begins
// with assistant(tool_calls), that's fine — its tool replies (if any)
// come AFTER it in the tail.
//
// Pure function; no allocation; safe with cutoff at any value.
func safeCompactionCutoff(messages []provider.Message, cutoff int) int {
	if cutoff < 0 {
		cutoff = 0
	}
	for cutoff < len(messages) && messages[cutoff].Role == "tool" {
		cutoff++
	}
	return cutoff
}

// pruneOldToolResults strips tool result content from messages older than
// PruneTurnAge, and additionally truncates any oversized tool result that
// survives into the preserved recent tail (or into a short history that
// skips the age-based prune entirely). Without the tail cap, a single
// multi-MB tool dump lands inside the retained window and compaction can
// never bring the context under the model's limit — every subsequent turn
// gets an empty response.
func pruneOldToolResults(messages []provider.Message) []provider.Message {
	result := make([]provider.Message, len(messages))
	copy(result, messages)

	cutoff := len(result) - PruneTurnAge
	if cutoff < 0 {
		cutoff = 0
	}
	for i := range result {
		if result[i].Role != "tool" {
			continue
		}
		if i < cutoff {
			// Historical tool results: strip content down to the stub.
			if len(result[i].Content) > 200 {
				result[i] = provider.Message{
					Role:       "tool",
					Content:    truncatedPlaceholder,
					ToolCallID: result[i].ToolCallID,
					Name:       result[i].Name,
				}
			}
		} else if len(result[i].Content) > maxRetainedToolResultBytes {
			// Retained tail: cap oversized tool results so a single
			// multi-MB dump can't hold the context above the model's
			// limit. Keep the head so the model still sees what the
			// tool returned.
			result[i].Content = result[i].Content[:maxRetainedToolResultBytes] +
				"\n[... result truncated to keep context under model limit ...]"
		}
	}

	return result
}

// compressOlderMessages asks the LLM to summarize older messages into a compact summary.
func compressOlderMessages(messages []provider.Message, prov provider.Provider, model string) ([]provider.Message, error) {
	if len(messages) <= PruneTurnAge {
		return messages, nil
	}

	cutoff := safeCompactionCutoff(messages, len(messages)-PruneTurnAge)
	olderMessages := messages[:cutoff]

	// Build a text representation of older messages for summarization.
	// Skip runtime-injected messages (currently only goal_context
	// continuations): their content is synthetic audit scaffolding,
	// not conversation worth summarizing — and the latest one is
	// already preserved verbatim in the recent tail below, so the
	// model never loses the current audit context. This is the
	// pinned-head protection design §5.3 (b) calls for: old
	// goal_context messages are dropped entirely from the
	// compaction output; the live one rides through unchanged.
	var text string
	for _, m := range olderMessages {
		if m.Origin != provider.OriginUser {
			continue
		}
		text += fmt.Sprintf("[%s] %s\n", m.Role, m.Content)
	}

	summaryPrompt := []provider.Message{
		{
			Role:    "system",
			Content: "You are a conversation summarizer. Summarize the following conversation history into a compact summary that preserves key facts, decisions, and context. Be concise but don't lose important details.",
		},
		{
			Role:    "user",
			Content: fmt.Sprintf("Summarize this conversation:\n\n%s", text),
		},
	}

	resp, err := prov.Chat(context.Background(), summaryPrompt, nil, model, 2048, 0.3)
	if err != nil {
		return nil, fmt.Errorf("summarize conversation: %w", err)
	}

	// Build new message list: summary + recent messages
	compressed := make([]provider.Message, 0, PruneTurnAge+1)
	compressed = append(compressed, provider.Message{
		Role:    "user",
		Content: fmt.Sprintf("[Conversation Summary]\n%s", resp.Content),
	})
	compressed = append(compressed, messages[cutoff:]...)

	return compressed, nil
}

// writeHistoryLog writes the full message history to a JSONL log file.
func writeHistoryLog(messages []provider.Message, workspace string) (string, error) {
	logDir := filepath.Join(workspace, "memory", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("create log dir: %w", err)
	}

	timestamp := time.Now().Format("20060102_150405")
	logFile := filepath.Join(logDir, fmt.Sprintf("history_%s.jsonl", timestamp))

	f, err := os.Create(logFile)
	if err != nil {
		return "", fmt.Errorf("create log file: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, m := range messages {
		if err := enc.Encode(m); err != nil {
			return logFile, fmt.Errorf("encode message: %w", err)
		}
	}

	slog.Info("wrote history log", "file", logFile, "messages", len(messages))
	return logFile, nil
}
