package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/store"
)

// FTSSearcher is the interface for FTS5-based memory search.
type FTSSearcher interface {
	Search(query string, limit int) ([]store.FTSResult, error)
}

// VectorSearcher is the subset of *store.DBStore needed for vector recall.
type VectorSearcher interface {
	SearchConversationSummariesVector(ctx context.Context, embedding []float32, limit int) ([]int64, error)
	GetConversationSummariesByIDs(ctx context.Context, ids []int64) ([]store.ConversationSummary, error)
	GetConversationSummaryEmbeddings(ctx context.Context, ids []int64) (map[int64][]float32, error)
}

// Reranker is the local mirror of embedding.Reranker used by memory_search.
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]embedding.ScoredDocument, error)
	Available() bool
}

type memorySearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"` // default 10
}

type searchResult struct {
	File      string  `json:"file"`
	Line      int     `json:"line"`
	Content   string  `json:"content"`
	Timestamp string  `json:"timestamp,omitempty"`
	Score     float64 `json:"-"`
}

// SummarySearcher is the subset of *store.DBStore the memory_search
// tool needs. *store.DBStore satisfies this implicitly.
type SummarySearcher interface {
	SearchConversationSummariesFTS(
		ctx context.Context,
		agentID, query string,
		limit int,
	) ([]store.ConversationSummary, error)
	// IncrementConversationSummaryAccess is the reinforcement signal —
	// bump access_count + last_accessed for surfaced summaries so
	// frequently-recalled ones score higher on future queries.
	IncrementConversationSummaryAccess(ctx context.Context, ids []int64) error
	// GetAgentMMRLambda returns the agent's current best MMR lambda
	// (bandit-tuned; default 0.6 before any feedback).
	GetAgentMMRLambda(ctx context.Context, agentID string) (float64, error)
	// InsertRecallEvent records one recall so stage-2b feedback can be
	// attributed to the lambda that produced it.
	InsertRecallEvent(ctx context.Context, ev store.RecallEvent) error
}

// RegisterMemorySearch registers the memory_search tool.
//
// Cross-session recall via conversation_summaries is enabled by calling
// SetSummarySearcher on the registry after Agent construction (the
// relational store isn't available at registration time). Until wired,
// the tool falls back to the legacy JSONL scan / FTS5 index.
func RegisterMemorySearch(r *Registry, workspace string, fts ...FTSSearcher) {
	var searcher FTSSearcher
	if len(fts) > 0 {
		searcher = fts[0]
	}

	r.Register("memory_search",
		"Search through summaries of past conversations with this chatter across all sessions. "+
			"Returns each summary + keywords + a (session_key, seq_start, seq_end) pointer; "+
			"call fetch_messages() with the pointer to retrieve verbatim original messages.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Keywords or phrases to search for in past summaries",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of results to return (default 10)",
				},
			},
			"required": []string{"query"},
		}, makeMemorySearch(r, workspace, searcher))
}

func makeMemorySearch(r *Registry, workspace string, fts FTSSearcher) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args memorySearchArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if args.Query == "" {
			return "", fmt.Errorf("query is required")
		}

		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}

		// Path A (preferred): conversation_summaries table — cross-session
		// recall scoped to the current chatter. Three-stage pipeline:
		//   A1. FTS/LIKE keyword search (always available)
		//   A2. Vector KNN (if embedder is configured)
		//   A3. Cross-encoder reranker (if configured)
		// A1+A2 merge into a pool; A3 re-ranks to top-K.
		if r != nil && r.summaryDB != nil {
			if r.agentID != "" {
				poolSize := limit * 3
				if poolSize < 30 {
					poolSize = 30
				}
				// queryEmb is filled by the vector-recall stage below and
				// reused by MMR; stays nil when the embedder is off.
				var queryEmb []float32
				hits, err := r.summaryDB.SearchConversationSummariesFTS(ctx, r.agentID, args.Query, poolSize)
				if err != nil {
					goto fallback
				}

				// Vector recall: embed query → KNN → fetch by ID → merge.
				// SearchConversationSummariesVector is a GLOBAL KNN (vec0
				// can't filter by metadata), so the fetched rows MUST be
				// re-scoped to agent here — keep only this agent's summaries.
				if r.vecDB != nil && r.embedder != nil && r.embedder.Available() {
					vecs, embErr := r.embedder.Embed(ctx, []string{args.Query})
					if embErr == nil && len(vecs) == 1 {
						queryEmb = vecs[0]
						vecIDs, vecErr := r.vecDB.SearchConversationSummariesVector(ctx, vecs[0], poolSize)
						if vecErr == nil && len(vecIDs) > 0 {
							vecHits, fetchErr := r.vecDB.GetConversationSummariesByIDs(ctx, vecIDs)
							if fetchErr == nil {
								scoped := make([]store.ConversationSummary, 0, len(vecHits))
								for _, h := range vecHits {
									if h.AgentID == r.agentID {
										scoped = append(scoped, h)
									}
								}
								hits = mergeSummaryResults(hits, scoped, poolSize)
							}
						}
					}
				}

				if len(hits) == 0 {
					return "No matching conversation summaries found. Try different keywords, or call fetch_messages directly if you know the session_key.", nil
				}

				// Cross-encoder reranker: extract summaries as documents, rerank
				if r.reranker != nil && r.reranker.Available() && len(hits) > limit {
					docs := make([]string, len(hits))
					for i, h := range hits {
						docs[i] = h.Summary
					}
					scored, rerankErr := r.reranker.Rerank(ctx, args.Query, docs, limit)
					if rerankErr == nil {
						hits = reorderByRerank(hits, scored)
					}
				}

				// MMR diversity rerank: when embeddings are available and
				// the pool still exceeds limit, pick a diversity-aware
				// top-K. Lambda is the agent's current bandit-tuned value
				// (default 0.6); ε-greedy occasionally explores a neighbor
				// so stage-2b feedback can discover better values. Each
				// recall is logged (recall_id + lambda + summary ids) for
				// that feedback linkage. Best-effort throughout.
				if queryEmb != nil && r.vecDB != nil && len(hits) > limit {
					embMap, mmrErr := r.vecDB.GetConversationSummaryEmbeddings(ctx, summaryIDs(hits))
					if mmrErr == nil && len(embMap) >= limit {
						lambda := store.DefaultMMRLambda
						if r.summaryDB != nil {
							if l, lErr := r.summaryDB.GetAgentMMRLambda(ctx, r.agentID); lErr == nil {
								lambda = l
							}
						}
						explored := false
						if rand.Float64() < mmrExploreEpsilon {
							if rand.Intn(2) == 0 {
								lambda += mmrExploreDelta
							} else {
								lambda -= mmrExploreDelta
							}
							lambda = clampLambda(lambda)
							explored = true
						}
						mmrHits := SelectMMR(hits, embMap, queryEmb, lambda, limit)
						if len(mmrHits) >= limit {
							hits = mmrHits
							if r.summaryDB != nil {
								_ = r.summaryDB.InsertRecallEvent(ctx, store.RecallEvent{
									RecallID:   newRecallID(),
									AgentID:    r.agentID,
									UserID:     r.userID,
									SessionKey: r.sessionID,
									Lambda:     lambda,
									Explored:   explored,
									SummaryIDs: summaryIDs(mmrHits),
								})
							}
						}
					}
				}

				// Reinforcement: bump access_count for the surfaced
				// summaries so frequently-recalled ones score higher
				// (and refresh recency) on future queries. Best-effort.
				if len(hits) > 0 {
					_ = r.summaryDB.IncrementConversationSummaryAccess(ctx, summaryIDs(hits))
				}

				return formatSummaryResults(hits, args.Query), nil
			}
		}
	fallback:

		// Path B (legacy fallback): FTS5 index of memory/logs/*.jsonl
		if fts != nil {
			ftsResults, err := fts.Search(args.Query, limit)
			if err == nil && len(ftsResults) > 0 {
				return formatFTSResults(ftsResults, args.Query), nil
			}
		}

		// Path C (legacy last resort): raw file scan of memory/logs/*.jsonl
		results := searchMemoryLogs(workspace, args.Query, limit)
		if len(results) == 0 {
			return "No matching entries found.", nil
		}
		return formatLegacyResults(results, args.Query), nil
	}
}

// mergeSummaryResults unions two result sets, deduplicating by ID.
// FTS results come first (exact keyword matches), vector results follow.
// Returns at most `limit` entries.
func mergeSummaryResults(fts, vec []store.ConversationSummary, limit int) []store.ConversationSummary {
	seen := make(map[int64]bool)
	var out []store.ConversationSummary

	for _, s := range fts {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		out = append(out, s)
	}

	for _, s := range vec {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		out = append(out, s)
	}

	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// reorderByRerank reorders hits according to the reranker's scored indices.
// Hits whose indices don't appear in scored are dropped.
func reorderByRerank(hits []store.ConversationSummary, scored []embedding.ScoredDocument) []store.ConversationSummary {
	out := make([]store.ConversationSummary, 0, len(scored))
	for _, s := range scored {
		if s.Index >= 0 && s.Index < len(hits) {
			out = append(out, hits[s.Index])
		}
	}
	if len(out) == 0 {
		return hits // fallback: keep original order
	}
	return out
}

// summaryIDs collects each summary's ID in order.
func summaryIDs(hits []store.ConversationSummary) []int64 {
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	return ids
}

// formatSummaryResults renders conversation_summaries hits for the LLM.
// Each hit shows summary + keywords + a (session_key, seq_start, seq_end)
// pointer the LLM can pass to fetch_messages to retrieve verbatim
// original messages.
func formatSummaryResults(hits []store.ConversationSummary, query string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d conversation summaries for %q:\n\n", len(hits), query))
	for i, h := range hits {
		fmt.Fprintf(&sb, "--- Summary %d (session=%s, time=%s) ---\n",
			i+1, h.SessionKey, h.CreatedAt.Format("2006-01-02 15:04"))
		if h.Topic != "" {
			fmt.Fprintf(&sb, "Topic: %s\n", h.Topic)
		}
		sb.WriteString(h.Summary)
		if len(h.Keywords) > 0 {
			sb.WriteString("\n\nKeywords: ")
			sb.WriteString(strings.Join(h.Keywords, ", "))
		}
		fmt.Fprintf(&sb, "\n\n[session_key=%s segments=%s]\n\n",
			h.SessionKey, formatSegmentsPointer(h.Segments, h.SeqStart, h.SeqEnd))
	}
	sb.WriteString("\nTo retrieve the verbatim original messages of any summary above, call:\n")
	sb.WriteString("  fetch_messages(session_key=<value>, segments=<value>)\n")
	return sb.String()
}

// formatSegmentsPointer renders the segments pointer for memory_search
// output. Uses Segments when present (topic-segmented rows); falls back
// to a single seq_start-seq_end range for legacy rows.
func formatSegmentsPointer(segs [][2]int, seqStart, seqEnd int) string {
	if len(segs) == 0 {
		return fmt.Sprintf("%d-%d", seqStart, seqEnd)
	}
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = fmt.Sprintf("%d-%d", s[0], s[1])
	}
	return strings.Join(parts, ",")
}

func formatLegacyResults(results []searchResult, query string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results for %q:\n\n", len(results), query))
	for i, r := range results {
		fmt.Fprintf(&sb, "--- Result %d (file: %s, line: %d) ---\n", i+1, filepath.Base(r.File), r.Line)
		sb.WriteString(r.Content)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

func formatFTSResults(results []store.FTSResult, query string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results for %q:\n\n", len(results), query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("--- Result %d (agent: %s, chat: %s, time: %s) ---\n",
			i+1, r.AgentID, r.ChatID, r.Timestamp.Format("2006-01-02 15:04")))
		if r.Snippet != "" {
			sb.WriteString(r.Snippet)
		} else {
			content := r.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			sb.WriteString(content)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}

func searchMemoryLogs(workspace, query string, limit int) []searchResult {
	logDir := filepath.Join(workspace, "memory", "logs")
	files, err := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		return nil
	}

	keywords := strings.Fields(strings.ToLower(query))
	if len(keywords) == 0 {
		return nil
	}

	now := time.Now()
	var results []searchResult

	for _, file := range files {
		fileResults := searchFile(file, keywords, now)
		results = append(results, fileResults...)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results
}

func searchFile(filePath string, keywords []string, now time.Time) []searchResult {
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	fileAge := fileRecencyWeight(filePath, now)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var results []searchResult
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		lower := strings.ToLower(line)

		matchCount := 0
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				matchCount++
			}
		}

		if matchCount == 0 {
			continue
		}

		score := float64(matchCount) / float64(len(keywords)) * fileAge

		content := line
		if len(content) > 500 {
			content = content[:500] + "..."
		}

		results = append(results, searchResult{
			File:    filePath,
			Line:    lineNum,
			Content: content,
			Score:   score,
		})
	}

	return results
}

// fileRecencyWeight returns a weight based on how recent the file is.
// Files from today get weight 1.0, older files decay.
func fileRecencyWeight(filePath string, now time.Time) float64 {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0.5
	}

	age := now.Sub(info.ModTime())
	days := age.Hours() / 24

	if days <= 0 {
		return 1.0
	}
	weight := 1.0 / (1.0 + days/7.0)
	if weight < 0.1 {
		return 0.1
	}
	return weight
}

// cosineSim returns the cosine similarity between two equal-length
// float32 vectors, as a float64 in [-1, 1]. Returns 0 for empty or
// mismatched-length input.
func cosineSim(a, b []float32) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		af, bf := float64(a[i]), float64(b[i])
		dot += af * bf
		na += af * af
		nb += bf * bf
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// SelectMMR applies Maximal Marginal Relevance to a candidate pool.
// Starting from the query-relevance ranking implied by queryEmb, it
// greedily picks the candidate that maximizes:
//
//	lambda*sim(query, doc) - (1-lambda)*max(sim(doc, already selected))
//
// so topK balances query relevance against redundancy with what is
// already chosen. emb maps each candidate's summary ID to its vector;
// candidates missing a vector are skipped. lambda=1 reduces to pure
// relevance order; lambda=0 to pure diversity. Returns fewer than topK
// when not enough candidates carry vectors.
func SelectMMR(candidates []store.ConversationSummary, emb map[int64][]float32, queryEmb []float32, lambda float64, topK int) []store.ConversationSummary {
	if topK <= 0 || len(candidates) == 0 {
		return nil
	}
	type cand struct {
		s   store.ConversationSummary
		v   []float32
		rel float64 // sim(query, doc)
	}
	pool := make([]cand, 0, len(candidates))
	for _, s := range candidates {
		v, ok := emb[s.ID]
		if !ok || len(v) == 0 {
			continue
		}
		pool = append(pool, cand{s: s, v: v, rel: cosineSim(queryEmb, v)})
	}
	if len(pool) == 0 {
		return nil
	}

	selected := make([]store.ConversationSummary, 0, topK)
	chosenVecs := make([][]float32, 0, topK)
	used := make([]bool, len(pool))

	for len(selected) < topK {
		bestIdx := -1
		bestScore := 0.0
		for i, c := range pool {
			if used[i] {
				continue
			}
			// diversity penalty: max similarity to any already-selected doc
			maxSim := 0.0
			for _, sv := range chosenVecs {
				if sim := cosineSim(c.v, sv); sim > maxSim {
					maxSim = sim
				}
			}
			score := lambda*c.rel - (1-lambda)*maxSim
			if bestIdx == -1 || score > bestScore {
				bestIdx = i
				bestScore = score
			}
		}
		if bestIdx == -1 {
			break
		}
		used[bestIdx] = true
		selected = append(selected, pool[bestIdx].s)
		chosenVecs = append(chosenVecs, pool[bestIdx].v)
	}
	return selected
}

// mmrExploreEpsilon is the ε-greedy exploration rate: fraction of recalls
// that try a neighboring lambda instead of the current best, so stage-2b
// feedback can discover better values. mmrExploreDelta is the step size.
const (
	mmrExploreEpsilon = 0.1
	mmrExploreDelta   = 0.1
)

// clampLambda constrains MMR lambda to [0, 1].
func clampLambda(l float64) float64 {
	if l < 0 {
		return 0
	}
	if l > 1 {
		return 1
	}
	return l
}

// newRecallID mints a unique-ish id for a recall event. Uniqueness only
// needs to hold long enough to link a near-term feedback signal; a
// timestamp + random suffix is plenty.
func newRecallID() string {
	return fmt.Sprintf("rc-%d-%06d", time.Now().UnixNano(), rand.Intn(1000000))
}
