package kb

// verify_claim — a deterministic citation check, NOT a factuality verdict.
//
// Mirrors the philosophy of Hermes's grounded-citations skill: don't ask
// the LLM whether a claim is true (LLM factuality judgments are
// unreliable), check whether the bytes are actually in a known source.
// Each sentence of the claim is matched as a normalised substring against
// KB source text; the tool reports which sentences are grounded verbatim
// and which it could not find — never claiming "false", only "sourced" vs
// "could not source".

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fluctio-ai/fluctio/internal/agent/tools"
)

const (
	// verifyClaimMaxSources caps how many KB sources the unnamed path pulls
	// via semantic search. The verbatim match below is the real gate, so
	// erring toward recall is safe — a non-matching source contributes no
	// hits and is simply listed as a checked source.
	verifyClaimMaxSources = 5
	// verifyClaimThreshold is deliberately loose (0.3) to maximise recall on
	// the semantic step; the literal-substring check that follows is what
	// actually decides "sourced vs not".
	verifyClaimThreshold = 0.3
	verifyClaimRatio     = 0.5
	// verifyClaimChunkLimit bounds how many chunks of a named source we pull
	// for the full-text comparison (SearchRawKB with empty query returns
	// every chunk of the given source, ordered by chunk_index).
	verifyClaimChunkLimit = 1000
	// minSentenceRunes filters out filler fragments ("嗯", "OK", "好的")
	// that would only produce "unsupported" noise. CN short clauses start
	// around 4 runes; filler is usually ≤3.
	minSentenceRunes = 4
)

type verifyClaimArgs struct {
	Claim    string `json:"claim"`
	SourceID string `json:"source_id,omitempty"`
}

const verifyClaimDescription = `Check whether each sentence of a claim can be grounded in a VERBATIM known source — a deterministic citation check, NOT a factuality verdict.

Pass a claim (a sentence or paragraph the user wants fact-checked, or text you fetched from the web and want to confirm before reporting). Optionally pass source_id to check against ONE specific KB source; omit it to auto-pick the most relevant KB sources via semantic search.

Returns a per-sentence breakdown: which sentences appear LITERALLY in a known source (with the source title), and which could NOT be found.

IMPORTANT — what this tool can and cannot do:
- It checks whether a sentence appears LITERALLY in a known KB source. It does NOT search the live web and does NOT judge whether a factual claim is true or false.
- "not found" means the KB has no verbatim match — it does NOT mean the claim is false. The source may simply not cover it.
- Report results to the user honestly: "this part I can source to X, this part I cannot find a source for." NEVER present "not found" as "false".
- If the KB has no relevant source at all, say so and suggest ingesting one (knowledgebase_ingest_url) rather than guessing.

When to use: the user asks "这是真的吗 / 核实一下 / 帮我查查这句" about a specific claim, OR you fetched information from the web and want to check it against stored knowledge before presenting it as fact. Never proactive; only when verification is explicitly wanted.`

func registerKBVerifyClaim(r *tools.Registry, store *KBStore, agentID string) {
	r.Register("knowledgebase_verify_claim", verifyClaimDescription, map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"claim": map[string]interface{}{
				"type":        "string",
				"description": "The text to fact-check. Split into sentences; each is checked for a verbatim match in the KB.",
			},
			"source_id": map[string]interface{}{
				"type":        "string",
				"description": "Optional: check against ONE KB source by id. Omit to auto-pick the most relevant sources via semantic search.",
			},
		},
		"required": []string{"claim"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args verifyClaimArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if strings.TrimSpace(args.Claim) == "" {
			return "", fmt.Errorf("claim is required")
		}
		return runVerifyClaim(ctx, store, agentID, args.Claim, args.SourceID)
	})
}

// claimSource is one KB source bucketed for matching: title for
// attribution, raw for reference, norm for the deterministic substring
// check (pre-computed once per source).
type claimSource struct {
	title string
	raw   string
	norm  string
}

// runVerifyClaim is the deterministic grounding engine. It gathers source
// text (one named source's full text, or the top-K relevant chunks from
// semantic search), splits the claim into sentences, and checks each for a
// normalised substring match. Output is a human-readable report the agent
// can relay to the user verbatim.
func runVerifyClaim(ctx context.Context, store *KBStore, agentID, claim, sourceID string) (string, error) {
	sentences := splitClaimSentences(claim)
	if len(sentences) == 0 {
		return "claim 中没有可核查的句子(过短的片段已忽略)。", nil
	}

	srcs, err := gatherClaimSources(ctx, store, agentID, claim, sourceID)
	if err != nil {
		return "", err
	}

	if len(srcs) == 0 {
		// No sources to ground against — report honestly, never fabricate a
		// verdict. Returns nil error so the registry doesn't append its
		// "try a different approach" nudge (this isn't a tool failure).
		var sb strings.Builder
		if strings.TrimSpace(sourceID) != "" {
			fmt.Fprintf(&sb, "source_id %q 没有可核查的文本(可能不属于本 agent、不存在、或为空)。\n", sourceID)
		} else {
			sb.WriteString("知识库中没有找到与该 claim 相关的源,无法做逐字核查。\n")
			sb.WriteString("如需核查外部信息,可先用 knowledgebase_ingest_url 录入相关网页,再调用本工具。\n")
		}
		sb.WriteString("本工具只判断句子是否在已知源里逐字出现,不判断事实真假。\n\n")
		sb.WriteString("claim 的句子(均未核查):\n")
		for _, s := range sentences {
			fmt.Fprintf(&sb, "  • %s\n", s)
		}
		return sb.String(), nil
	}

	type verdict struct {
		sentence string
		sourced  bool
		title    string
	}
	verdicts := make([]verdict, 0, len(sentences))
	for _, sent := range sentences {
		nsent := normalizeForMatch(sent)
		v := verdict{sentence: sent}
		if nsent != "" {
			for _, src := range srcs {
				if strings.Contains(src.norm, nsent) {
					v.sourced = true
					v.title = src.title
					break
				}
			}
		}
		verdicts = append(verdicts, v)
	}

	sourced, unsourced := 0, 0
	for _, v := range verdicts {
		if v.sourced {
			sourced++
		} else {
			unsourced++
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "核查完成:共 %d 句,基于 %d 个已知源逐字比对。\n\n", len(verdicts), len(srcs))
	if sourced > 0 {
		sb.WriteString("✅ 已找到逐字出处:\n")
		for _, v := range verdicts {
			if v.sourced {
				fmt.Fprintf(&sb, "  • %s\n    ← 来源:「%s」\n", v.sentence, v.title)
			}
		}
		sb.WriteString("\n")
	}
	if unsourced > 0 {
		sb.WriteString("❓ 未在已知源找到逐字证据(不代表虚假,可能是源未覆盖):\n")
		for _, v := range verdicts {
			if !v.sourced {
				fmt.Fprintf(&sb, "  • %s\n", v.sentence)
			}
		}
	}
	return sb.String(), nil
}

// gatherClaimSources pulls the source text to ground against. Named source
// path: full text of one source (SearchRawKB with empty query returns every
// chunk). Unnamed path: semantic search returns the top-K relevant chunks.
// Both dedup by SourceTitle into one bucket per source, with the normalised
// text pre-computed for the per-sentence inner loop.
func gatherClaimSources(ctx context.Context, store *KBStore, agentID, claim, sourceID string) ([]claimSource, error) {
	var results []KBResult
	var err error
	if strings.TrimSpace(sourceID) != "" {
		results, err = store.SearchRawKB(ctx, agentID, "", []string{sourceID}, verifyClaimChunkLimit)
		if err != nil {
			return nil, fmt.Errorf("read source %s: %w", sourceID, err)
		}
		// Empty results (bad id / empty source) fall through — caller reports
		// a distinguishable "no source" message rather than erroring.
	} else {
		results, err = store.Search(ctx, agentID, claim, verifyClaimMaxSources, 0, verifyClaimRatio, verifyClaimThreshold)
		if err != nil {
			return nil, fmt.Errorf("search kb: %w", err)
		}
	}

	buckets := map[string]*claimSource{}
	var order []string
	for _, r := range results {
		title := r.SourceTitle
		if title == "" {
			title = r.SourceID
		}
		if _, ok := buckets[title]; !ok {
			buckets[title] = &claimSource{title: title}
			order = append(order, title)
		}
		buckets[title].raw += r.Content + "\n"
	}
	srcs := make([]claimSource, 0, len(order))
	for _, title := range order {
		b := buckets[title]
		b.norm = normalizeForMatch(b.raw)
		srcs = append(srcs, *b)
	}
	return srcs, nil
}

// splitClaimSentences breaks text into sentences on CN/EN terminators and
// newlines, dropping fragments shorter than minSentenceRunes (filler that
// would only produce "unsupported" noise).
func splitClaimSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '。', '！', '？', '；', '\n', '.', '!', '?', ';':
			return true
		}
		return false
	})
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if utf8.RuneCountInString(f) >= minSentenceRunes {
			out = append(out, f)
		}
	}
	return out
}

// normalizeForMatch strips ALL whitespace and lowercases — so a sentence
// still matches a source across indentation / line-wrap / CJK wide-space
// differences, without the false confidence of semantic fuzzy matching.
// Intentionally strict about non-whitespace characters: a typo or paraphrase
// will NOT match, which is exactly what "verbatim grounding" promises.
func normalizeForMatch(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		sb.WriteRune(unicode.ToLower(r))
	}
	return sb.String()
}
