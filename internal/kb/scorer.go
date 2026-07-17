package kb

import (
	"regexp"
	"strings"
)

var (
	wordRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_-]+`)
	cjkRE  = regexp.MustCompile(`[\p{Han}\x{3040}-\x{30ff}\x{ac00}-\x{d7af}]+`)
)

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true,
	"in": true, "on": true, "for": true, "and": true, "or": true,
	"is": true, "are": true, "was": true, "were": true,
	"with": true, "by": true, "from": true, "this": true, "that": true,
	"what": true, "why": true, "how": true, "when": true, "which": true, "who": true,
	"的": true, "了": true, "是": true, "和": true, "或": true,
	"在": true, "对": true, "为": true, "与": true, "及": true,
}

// tokenize splits text into tokens: English words + CJK bigrams, stop-filtered.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	seen := make(map[string]bool)
	var tokens []string

	for _, m := range wordRE.FindAllString(text, -1) {
		t := strings.ToLower(m)
		if len(t) < 2 || stopwords[t] {
			continue
		}
		if !seen[t] {
			seen[t] = true
			tokens = append(tokens, t)
		}
	}

	for _, m := range cjkRE.FindAllString(text, -1) {
		runes := []rune(m)
		if len(runes) == 1 {
			if stopwords[string(runes)] {
				continue
			}
			t := string(runes)
			if !seen[t] {
				seen[t] = true
				tokens = append(tokens, t)
			}
			continue
		}
		for i := 0; i < len(runes)-1; i++ {
			bg := string(runes[i]) + string(runes[i+1])
			if stopwords[bg] {
				continue
			}
			if !seen[bg] {
				seen[bg] = true
				tokens = append(tokens, bg)
			}
		}
	}
	return tokens
}

func tokenizeSet(text string) map[string]bool {
	tokens := tokenize(text)
	s := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		s[t] = true
	}
	return s
}

// wikiPageRow is a lightweight row from the wiki_pages table.
type wikiPageRow struct {
	ID       string
	Title    string
	Summary  string
	Body     string
	PageType string
	Slug     string
	Tags     string // JSON string from DB
}

// scoredPage is a wiki page with its bigram score against a query.
type scoredPage struct {
	ID       string
	Title    string
	Summary  string
	Body     string
	PageType string
	Score    float64
}

// scoreCandidates re-ranks pre-filtered wiki pages using bigram scoring.
// Weights: title×4, tag×3, summary×2, slug exact +5.
func scoreCandidates(pages []wikiPageRow, query string, topK int) []scoredPage {
	qTokens := tokenizeSet(query)
	if len(qTokens) == 0 {
		return nil
	}

	var results []scoredPage
	for _, p := range pages {
		titleToks := tokenizeSet(p.Title)
		summaryToks := tokenizeSet(p.Summary)
		tagToks := tokenizeSet(p.Tags)

		score := 4.0*float64(intersectCount(qTokens, titleToks)) +
			2.0*float64(intersectCount(qTokens, summaryToks)) +
			3.0*float64(intersectCount(qTokens, tagToks))

		if p.Slug != "" && qTokens[strings.ToLower(p.Slug)] {
			score += 5.0
		}

		if score < 0.5 {
			continue
		}

		results = append(results, scoredPage{
			ID:       p.ID,
			Title:    p.Title,
			Summary:  p.Summary,
			Body:     p.Body,
			PageType: p.PageType,
			Score:    score,
		})
	}

	sortByScore(results)
	if len(results) > topK {
		results = results[:topK]
	}
	return results
}

func intersectCount(a, b map[string]bool) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	n := 0
	for t := range a {
		if b[t] {
			n++
		}
	}
	return n
}

func sortByScore(results []scoredPage) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}

// pageTokens holds pre-computed token sets for a wiki page.
type pageTokens struct {
	ID      string
	Title   map[string]bool
	Summary map[string]bool
	Tags    map[string]bool
	Slug    string
	Body    string
	SummaryText string
	TitleText   string
}

// scoreFromTokens scores a pre-tokenized page against query tokens.
