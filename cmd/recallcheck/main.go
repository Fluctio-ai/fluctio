// Command recallcheck is the retrieval regression harness CLI (golden-set
// replay against the production recall chain). Dev tool — not part of the
// daemon binary.
//
// Seed a golden set from the agent's own KB:
//
//	recallcheck -agent agt_xxx -seed 10 -cases golden.jsonl
//
// Replay it (after any recall-affecting change — thresholds, embedding
// model, RRF weights, chunking):
//
//	recallcheck -agent agt_xxx -run -cases golden.jsonl -out report.json
//
// The embedder flags must point at the same endpoint the agent uses, or the
// semantic lanes degrade to lexical-only (the report flags Degraded — such
// runs are not comparable with semantic runs).
//
// Exit code is 0 even with violations: this is an observability tool, not a
// gate. A gate gets wired once the numbers are trusted.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/embedding"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/recallcheck"
	"github.com/fluctio-ai/fluctio/internal/store"
)

func main() {
	agent := flag.String("agent", "", "agent id to replay against (required)")
	dbFlag := flag.String("db", "", "sqlite file path or postgres DSN (default: $FLUCTIO_HOME or ~/.fluctio fluctio.db, read-only)")
	seedN := flag.Int("seed", 0, "generate a golden set: N cases per lane from the agent's KB")
	unanswerable := flag.Int("unanswerable", 5, "unanswerable trap cases to generate")
	casesPath := flag.String("cases", "", "golden JSONL file (-seed writes it, -run reads it, -draft appends)")
	runMode := flag.Bool("run", false, "replay the golden set and print metrics")
	ablMode := flag.Bool("abl", false, "offline fusion ablation over the mem_inject golden cases (one retrieval pass, grid replay; needs embedder flags)")
	hardcases := flag.Bool("hardcases", false, "list pending hardcases (mined zero-hit queries) for review")
	draftN := flag.Int("draft", 0, "append the top N pending hardcases to the golden file as draft cases (empty expect_ids — owner labels what SHOULD hit)")
	outPath := flag.String("out", "", "write the JSON report here (default: stdout only)")
	embedBase := flag.String("embed-base", "", "embedding API base (OpenAI-compatible /v1 endpoint root)")
	embedKey := flag.String("embed-key", "", "embedding API key")
	embedModel := flag.String("embed-model", "", "embedding model (empty = degraded lexical-only run)")
	embedDim := flag.Int("embed-dim", 0, "embedding dim (0 = let the endpoint declare it)")
	flag.Parse()

	if *agent == "" {
		fmt.Fprintln(os.Stderr, "-agent is required")
		os.Exit(2)
	}
	// -hardcases lists only; no golden file needed.
	if *casesPath == "" && !*hardcases {
		fmt.Fprintln(os.Stderr, "-cases is required")
		os.Exit(2)
	}
	if *seedN == 0 && !*runMode && !*ablMode && !*hardcases && *draftN == 0 {
		fmt.Fprintln(os.Stderr, "nothing to do: pass -seed N, -run, -abl, -hardcases, or -draft N")
		os.Exit(2)
	}

	ctx := context.Background()
	dsn, dialect := resolveDSN(*dbFlag)
	db, err := store.NewDBStore(dialect, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	defer db.Close()
	// No Migrate: this tool is read-only against a booted fluctio DB.
	// Migrating a real DB from a dev tool would be an uncontrolled write;
	// a fresh/empty DB fails with clear SQL errors instead.

	if *seedN > 0 {
		cases, err := recallcheck.Seed(ctx, db, *agent, *seedN, *unanswerable)
		if err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			os.Exit(1)
		}
		if err := recallcheck.SaveGolden(*casesPath, cases); err != nil {
			fmt.Fprintln(os.Stderr, "save golden:", err)
			os.Exit(1)
		}
		fmt.Printf("seeded %d cases -> %s\n", len(cases), *casesPath)
	}

	if *hardcases || *draftN > 0 {
		hs, err := db.ListHardcases(ctx, *agent, "pending", max(*draftN, 50))
		if err != nil {
			fmt.Fprintln(os.Stderr, "list hardcases:", err)
			os.Exit(1)
		}
		if *hardcases {
			fmt.Printf("pending hardcases for %s: %d\n", *agent, len(hs))
			for _, h := range hs {
				fmt.Printf("  %4dx [%s] %s\n", h.Count, h.Lanes, h.Query)
			}
		}
		if *draftN > 0 {
			if *draftN > len(hs) {
				*draftN = len(hs)
			}
			var drafts []recallcheck.GoldenCase
			for i := 0; i < *draftN; i++ {
				drafts = append(drafts, recallcheck.GoldenCase{
					ID:        "hardcase-" + store.HardcaseQueryHash(hs[i].Query),
					Query:     hs[i].Query,
					Lane:      firstLane(hs[i].Lanes),
					Source:    "hardcase",
					ExpectIDs: nil, // owner labels what SHOULD hit
				})
			}
			existing, _ := recallcheck.LoadGolden(*casesPath)
			merged := append(existing, drafts...)
			if err := recallcheck.SaveGolden(*casesPath, merged); err != nil {
				fmt.Fprintln(os.Stderr, "save drafts:", err)
				os.Exit(1)
			}
			fmt.Printf("drafted %d hardcases into %s (fill expect_ids, then -run)\n", len(drafts), *casesPath)
		}
	}

	if !*runMode && !*ablMode {
		return
	}

	cases, err := recallcheck.LoadGolden(*casesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load golden:", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "golden file has no cases")
		os.Exit(1)
	}

	ks := kb.NewKBStore(db.DB(), db.Dialect())
	var emb embedding.Embedder
	if *embedModel != "" {
		emb = embedding.NewOpenAICompatEmbedder(*embedBase, *embedKey, *embedModel, *embedDim, *embedDim > 0)
		if emb.Available() {
			ks.SetRetriever(emb, nil)
		} else {
			emb = nil
		}
	}

	if *ablMode {
		if emb == nil {
			fmt.Fprintln(os.Stderr, "-abl needs -embed-model (the ablation replays vector+lexical fusion)")
			os.Exit(2)
		}
		results := recallcheck.Ablate(ctx, recallcheck.Deps{DB: db, KB: ks, AgentID: *agent, Embedder: emb}, cases, recallcheck.DefaultAblGrid())
		fmt.Printf("fusion ablation over %d mem cases (sorted by MRR; * = production constants):\n", countMemCases(cases))
		for _, r := range results {
			mark := "  "
			if r.Production {
				mark = "* "
			}
			fmt.Printf("%srrf_k=%-4d top_k=%-3d floor=%.2f  hit@1=%-3d hit@3=%-3d hit@5=%-3d mrr=%.3f\n",
				mark, r.RRFK, r.TopK, r.Alpha, r.Hit1, r.Hit3, r.Hit5, r.MRR)
		}
		if *outPath != "" {
			b, _ := json.MarshalIndent(results, "", "  ")
			if err := os.WriteFile(*outPath, b, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "write report:", err)
				os.Exit(1)
			}
		}
		return
	}

	rep := recallcheck.Run(ctx, recallcheck.Deps{DB: db, KB: ks, AgentID: *agent, Embedder: emb}, cases)

	printSummary(rep)
	if *outPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*outPath, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
			os.Exit(1)
		}
	}
}

// firstLane picks the first lane of a comma-separated set for draft cases;
// the owner edits it if the real target lane differs.
func firstLane(lanes string) string {
	if i := strings.IndexByte(lanes, ','); i >= 0 {
		return lanes[:i]
	}
	if lanes == "" {
		return "wiki"
	}
	return lanes
}

// resolveDSN maps the -db flag to a store DSN. A bare file path becomes a
// read-only sqlite connection (the harness must never write; WAL lets it
// read alongside a running daemon). Anything containing "://" or starting
// with "postgres" is treated as a postgres DSN and passed through as-is.
func resolveDSN(flag string) (dsn, dialect string) {
	if flag == "" {
		home := os.Getenv("FLUCTIO_HOME")
		if home == "" {
			home = filepath.Join(os.Getenv("USERPROFILE"), ".fluctio")
		}
		flag = filepath.Join(home, "fluctio.db")
	}
	if strings.Contains(flag, "://") || strings.HasPrefix(flag, "postgres") {
		return flag, "postgres"
	}
	abs, err := filepath.Abs(flag)
	if err != nil {
		abs = flag
	}
	return "file:" + abs + "?mode=ro", "sqlite"
}

// countMemCases returns the ablation's input size for the header line.
func countMemCases(cases []recallcheck.GoldenCase) int {
	n := 0
	for _, c := range cases {
		if !c.Unanswerable && c.Lane == "mem_inject" {
			n++
		}
	}
	return n
}

func printSummary(rep *recallcheck.Report) {
	fmt.Printf("agent=%s cases=%d", rep.AgentID, len(rep.Cases))
	if rep.Degraded {
		fmt.Print("  [DEGRADED: no embedder — lexical lanes only, not comparable with semantic runs]")
	}
	fmt.Println()
	for lane, s := range rep.ByLane {
		fmt.Printf("  %-12s cases=%-4d hit@1=%-3d hit@3=%-3d hit@5=%-3d mrr=%.3f violations=%d\n",
			lane, s.Cases, s.Hit1, s.Hit3, s.Hit5, s.MRR, s.Violations)
	}
	var failed []string
	for _, c := range rep.Cases {
		if c.Err != "" || c.Violation || (!c.Case.Unanswerable && c.Rank == 0) {
			failed = append(failed, c.Case.ID)
		}
	}
	if len(failed) > 0 {
		fmt.Printf("failed cases: %s\n", strings.Join(failed, ", "))
	}
}
