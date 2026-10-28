package kb

import (
	"regexp"
)

// Ingest-time governance: conservative line-anchored cleaning that strips the
// boilerplate scraped pages leave behind (reading-time estimates, follow-QR
// CTAs, cookie banners) before content is chunked and embedded. Chunks
// polluted with this noise waste embedding budget and dilute recall — and
// wiki generation later quotes these chunks verbatim, so boilerplate ships
// all the way into wiki pages if it isn't cut here.
//
// Two packs, selected by sourceType at IngestText:
//   - base: formatting-neutral fixes applied to every source (manual text
//     included — zero-width chars and whitespace runs carry no meaning).
//   - web: whole-line deletions, url sources only. Every pattern is anchored
//     to the full line (RE2, no catastrophic backtracking) and deliberately
//     narrow: a line must BE boilerplate, not merely mention the words, so
//     real content like "长按保存以下配置" never matches.
//
// Per-rule hit counts ride back to the caller for logging; v1 keeps them
// out of the schema (the planned pipeline-hash work can persist them).

type govRule struct {
	name string
	re   *regexp.Regexp
	repl string // "" = delete the matched text (line rules delete the whole line)
}

// govLine wraps a line-body pattern into an anchored full-line deletion that
// also consumes the line's trailing newline (or end of text).
func govLine(name, body string) govRule {
	return govRule{name: name, re: regexp.MustCompile(`(?m)^` + body + `$(?:\n|\z)`), repl: ""}
}

var govBaseRules = []govRule{
	// Invisible junk: zero-width spaces/joiners and BOMs survive readability
	// extraction and end up inside chunks and embeddings.
	{name: "zero_width", re: regexp.MustCompile(`[\x{200B}\x{200C}\x{200D}\x{FEFF}]`), repl: ""},
	// Trailing whitespace first so the blank-run collapse sees truly empty
	// lines ("   \n" → "\n").
	{name: "trailing_ws", re: regexp.MustCompile(`(?m)[ \t]+$`), repl: ""},
	{name: "blank_run", re: regexp.MustCompile(`\n{3,}`), repl: "\n\n"},
}

var govWebRules = []govRule{
	// WeChat article header/footer boilerplate.
	govLine("read_time", `本文约.{0,12}?字.{0,20}?分钟.{0,8}?`),
	govLine("read_original", `.{0,6}阅读原文.{0,10}?`),
	govLine("qr_cta", `.{0,8}?(?:长按|扫描|扫码).{0,6}?二维码.{0,20}?`),
	govLine("follow_cta", `.{0,10}?关注.{0,12}?(?:公众号|订阅号).{0,10}?`),
	govLine("share_cta", `.{0,8}?(?:分享到朋友圈|点击在看|点个在看|点下在看|分享朋友圈).{0,10}?`),
	// Generic consent banners readability leaves at the top or bottom.
	govLine("cookie_banner", `.{0,4}?(?:This site uses cookies|We use cookies|本网站?使用 Cookie).{0,60}?`),
}

// applyGovRule runs one rule and reports how many times it fired. A
// single pass: the counting closure handles both substitution and
// deletion (repl "") alike.
func applyGovRule(text string, r govRule) (string, int) {
	n := 0
	out := r.re.ReplaceAllStringFunc(text, func(string) string { n++; return r.repl })
	return out, n
}

// GovernContent cleans text for ingestion and returns per-rule hit counts.
// sourceType "url" additionally gets the web boilerplate pack; anything else
// (manual text, MCP) only gets the formatting-neutral base pack.
func GovernContent(text, sourceType string) (string, map[string]int) {
	hits := map[string]int{}
	out := text
	for _, r := range govBaseRules {
		var n int
		out, n = applyGovRule(out, r)
		if n > 0 {
			hits[r.name] = n
		}
	}
	if sourceType == "url" {
		for _, r := range govWebRules {
			var n int
			out, n = applyGovRule(out, r)
			if n > 0 {
				hits[r.name] = n
			}
		}
	}
	return out, hits
}
