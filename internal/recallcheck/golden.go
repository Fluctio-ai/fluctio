// Package recallcheck is the retrieval regression harness: a golden
// question set replayed through the production recall chain with
// deterministic ID-based metrics (hit@k, MRR, unanswerable violations).
// No LLM judge — the golden set is the judge. Motivation: recall tuning
// used to be vibes; a threshold change that silently kills a lane (the
// [MEM] 1/(1+L2) regression) now shows up as a metrics cliff on the next
// run. Manual command by design — wire CI only after the numbers are
// trusted (a fake gate is worse than none).
package recallcheck

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// GoldenCaseSchema versions the case file so future format changes can
// migrate or reject instead of silently misreading.
const GoldenCaseSchema = "fluctio.golden.v1"

// GoldenCase is one regression question. Lane names the recall lane under
// test; "any" (with Unanswerable) runs every lane and passes only when all
// stay silent. ExpectIDs are lane-native ids: wiki page id ("entity:foo"),
// kb source uuid, bookmark id, or conversation-summary id (decimal string).
type GoldenCase struct {
	Schema       string   `json:"schema"`
	ID           string   `json:"id"`
	Query        string   `json:"query"`
	Lane         string   `json:"lane"`
	ExpectIDs    []string `json:"expect_ids,omitempty"`
	Unanswerable bool     `json:"unanswerable,omitempty"`
	Source       string   `json:"source,omitempty"` // seed:* | manual
}

// LoadGolden reads a JSONL golden case file. Blank lines and #-comments
// are skipped; every case must carry the current schema tag.
func LoadGolden(path string) ([]GoldenCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []GoldenCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		var c GoldenCase
		if err := json.Unmarshal([]byte(t), &c); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if c.Schema != GoldenCaseSchema {
			return nil, fmt.Errorf("line %d: schema %q != %q", line, c.Schema, GoldenCaseSchema)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// SaveGolden writes cases as JSONL (one case per line, comments stripped).
func SaveGolden(path string, cases []GoldenCase) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, c := range cases {
		c.Schema = GoldenCaseSchema
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := w.WriteString(string(b) + "\n"); err != nil {
			return err
		}
	}
	return w.Flush()
}
