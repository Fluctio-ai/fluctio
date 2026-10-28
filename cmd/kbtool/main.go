// Command kbtool is the KB maintenance dev tool: chunk preview (no DB
// needed) and pipeline rewash (dry-run by default; -apply re-derives
// chunks from retained originals after a governance/chunker change).
//
//	preview -file article.md [-type url|text]
//	rewash -agent agt_xxx [-db path] [-apply]
//
// Rewash against a running daemon is safe in WAL mode (the write
// transaction is short; wiki autogen picks up the dirty flag after
// commit), but the owner should still dry-run and eyeball a preview
// first — rewash discards the old chunk set on apply.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/kb"
	"github.com/fluctio-ai/fluctio/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: kbtool preview|rewash ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "preview":
		previewCmd(os.Args[2:])
	case "rewash":
		rewashCmd(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", os.Args[1])
		os.Exit(2)
	}
}

func previewCmd(args []string) {
	fs := flag.NewFlagSet("preview", flag.ExitOnError)
	file := fs.String("file", "", "content file to preview (default: stdin)")
	sourceType := fs.String("type", "text", "source type: text | url (selects the governance pack)")
	asJSON := fs.Bool("json", false, "print the full JSON preview")
	fs.Parse(args)

	var data []byte
	var err error
	if *file == "" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	p := kb.PreviewIngest(string(data), *sourceType)
	if *asJSON {
		b, _ := json.MarshalIndent(p, "", "  ")
		fmt.Println(string(b))
		return
	}
	st := p.Stats
	fmt.Printf("chars %d -> %d (governed) | chunks %d | coverage %.0f%% | overlap waste %.0f%% | tiny %d | cliff %d | bad boundaries %d\n",
		st.SourceChars, st.GovernedChars, st.Chunks, st.CoverageRatio*100, st.OverlapWaste*100, st.TinyChunks, st.CliffChunks, st.BadBoundaries)
	if len(st.GovernHits) > 0 {
		fmt.Printf("governance hits: %v\n", st.GovernHits)
	}
	for _, c := range p.Chunks {
		mark := "ok"
		if !c.BoundaryOK {
			mark = "!!"
		}
		fmt.Printf("  #%d %5dch [%d:%d] %s %s\n", c.Index, c.Chars, c.Start, c.End, mark, c.First40Chars)
	}
}

func rewashCmd(args []string) {
	fs := flag.NewFlagSet("rewash", flag.ExitOnError)
	agent := fs.String("agent", "", "agent id (required)")
	dbFlag := fs.String("db", "", "sqlite file path (default: $FLUCTIO_HOME or ~/.fluctio fluctio.db)")
	apply := fs.Bool("apply", false, "commit the rewash (default: dry run)")
	fs.Parse(args)
	if *agent == "" {
		fmt.Fprintln(os.Stderr, "-agent is required")
		os.Exit(2)
	}

	db, err := store.NewDBStore("sqlite", resolveSQLitePath(*dbFlag))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	defer db.Close()

	ks := kb.NewKBStore(db.DB(), db.Dialect())
	rep, err := ks.RewashSources(context.Background(), *agent, *apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rewash:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	if !*apply && rep.WouldChange > 0 {
		fmt.Println("dry run — re-run with -apply to commit; preview a source first with: kbtool preview -file <original>")
	}
}

// resolveSQLitePath builds a writable sqlite DSN for the -db flag,
// defaulting to the canonical fluctio.db under config.HomeDir().
func resolveSQLitePath(flag string) string {
	if flag == "" {
		home, err := config.HomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve home:", err)
			os.Exit(1)
		}
		flag = filepath.Join(home, "fluctio.db")
	}
	if strings.Contains(flag, "?") || strings.HasPrefix(flag, "file:") {
		return flag
	}
	abs, err := filepath.Abs(flag)
	if err != nil {
		abs = flag
	}
	return "file:" + abs
}
