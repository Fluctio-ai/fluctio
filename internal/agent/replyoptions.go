package agent

// Reply-options extraction — the ```options fenced-block protocol.
//
// When a final reply should offer the user tappable choices, the model
// appends a fenced block:
//
//	```options
//	["Option A", "Option B"]
//	```
//
// Every consumer surface extracts with the same function:
//
//   - web/app: the bubble renders the block as a button row (clicking a
//     button just sends that text — no new reply channel), so the raw
//     text stays in history and re-renders identically on replay.
//   - IM: the gateway strips the block from the delivered text and, on
//     channels with native buttons (Telegram inline keyboard, QQ
//     keyboard), attaches it as tappable rows; other channels get a
//     numbered-reply fallback appended to the text.
//
// The protocol deliberately lives in the reply text (not a tool) so
// chat-only sessions — which send no tools to the LLM — can use it too.

import (
	"regexp"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/llmjson"
)

// replyOptionsMaxCount / replyOptionsMaxRunes bound what a misbehaving
// model can emit: more than a handful of choices is a list, not buttons,
// and over-long labels blow up button width on mobile IM clients.
const (
	replyOptionsMaxCount  = 4
	replyOptionsMaxRunes  = 40
)

// optionsBlockRe matches a closed ```options fenced block. Case-sensitive
// language tag (the prompt teaches lowercase), non-greedy body.
var optionsBlockRe = regexp.MustCompile("(?s)```options[ \t]*\r?\n(.*?)\r?\n```")

// optionsOpenBlockRe matches an unterminated ```options opener through
// end-of-text — a mid-stream (or truncated) block. Stripped so a
// half-arrived option list never flashes as raw JSON; the closed-block
// regex above replaces it once the fence completes.
var optionsOpenBlockRe = regexp.MustCompile("(?s)```options[ \t]*\r?\n.*")

// ExtractReplyOptions strips all ```options blocks from text and returns
// the cleaned text plus the deduplicated, capped option labels. Blocks
// whose body isn't a parseable JSON string array contribute nothing (the
// block is still stripped — showing the user raw JSON is never right).
// When no block is present it returns the input unchanged and nil.
func ExtractReplyOptions(text string) (string, []string) {
	if !strings.Contains(text, "```options") {
		return text, nil
	}

	var opts []string
	seen := make(map[string]bool)
	collect := func(body string) {
		var parsed []string
		if err := llmjson.UnmarshalLLM(body, &parsed); err != nil {
			return
		}
		for _, o := range parsed {
			o = strings.TrimSpace(o)
			if o == "" || seen[o] || len([]rune(o)) > replyOptionsMaxRunes {
				continue
			}
			// Gateway consumers treat a tapped option as literal user
			// input, and inbound slash commands are processed before the
			// LLM — a model-generated (injectable) label starting with "/"
			// would execute that command as the chatter. Refuse such
			// labels outright.
			if strings.HasPrefix(o, "/") {
				continue
			}
			seen[o] = true
			opts = append(opts, o)
		}
	}

	// Collect from closed blocks first, then strip them (plus any
	// unterminated opener — no closing fence means the JSON isn't
	// trustworthy as authored, so it contributes nothing).
	for _, m := range optionsBlockRe.FindAllStringSubmatch(text, -1) {
		collect(m[1])
	}
	clean := optionsOpenBlockRe.ReplaceAllString(optionsBlockRe.ReplaceAllString(text, ""), "")
	clean = collapseBlankLines(clean)

	if len(opts) > replyOptionsMaxCount {
		opts = opts[:replyOptionsMaxCount]
	}
	if len(opts) == 0 {
		return clean, nil
	}
	return clean, opts
}

// collapseBlankLines squashes the 3+ consecutive newlines a stripped
// block can leave behind down to a single blank line.
func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}
