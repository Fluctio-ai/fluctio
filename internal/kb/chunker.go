package kb

import (
	"strings"
	"unicode/utf8"
)

const (
	defaultChunkSize = 1000
	defaultOverlap   = 200
)

type Chunk struct {
	Content string
	Index   int
}

// ChunkText splits text into overlapping chunks. Strategy: split on
// double-newlines (paragraphs). If a paragraph exceeds maxChunkSize,
// it is further split on sentence boundaries. Consecutive chunks overlap
// by overlapSize characters.
func ChunkText(text string, maxChunkSize, overlapSize int) []Chunk {
	if maxChunkSize <= 0 {
		maxChunkSize = defaultChunkSize
	}
	if overlapSize < 0 {
		overlapSize = 0
	}
	if overlapSize >= maxChunkSize {
		overlapSize = maxChunkSize / 4
	}

	paragraphs := splitParagraphs(text)
	var chunks []Chunk
	var buf strings.Builder
	bufLen := 0
	idx := 0

	for _, p := range paragraphs {
		if len(p) > maxChunkSize {
			// Flush buffer first
			if bufLen > 0 {
				chunks = append(chunks, Chunk{Content: strings.TrimSpace(buf.String()), Index: idx})
				idx++
				buf.Reset()
				bufLen = 0
			}
			// Split large paragraph by sentences
			sentenceChunks := splitLargeParagraph(p, maxChunkSize, overlapSize)
			for _, sc := range sentenceChunks {
				chunks = append(chunks, Chunk{Content: sc, Index: idx})
				idx++
			}
			continue
		}

		if bufLen+len(p)+1 > maxChunkSize && bufLen > 0 {
			chunks = append(chunks, Chunk{Content: strings.TrimSpace(buf.String()), Index: idx})
			idx++
			// Keep overlap from end of buffer. overlapTail backs up to a
			// rune boundary: a raw byte cut can cleave a multi-byte CJK
			// rune and land invalid UTF-8 in the chunk (which then goes to
			// FTS/embedding).
			content := buf.String()
			if tail := overlapTail(content, overlapSize); tail != "" {
				buf.Reset()
				buf.WriteString(tail)
				bufLen = len(tail)
			} else {
				buf.Reset()
				bufLen = 0
			}
		}
		if bufLen > 0 {
			buf.WriteByte('\n')
			bufLen++
		}
		buf.WriteString(p)
		bufLen += len(p)
	}

	if bufLen > 0 {
		chunks = append(chunks, Chunk{Content: strings.TrimSpace(buf.String()), Index: idx})
	}

	return chunks
}

func splitParagraphs(text string) []string {
	var out []string
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 && strings.TrimSpace(text) != "" {
		out = append(out, strings.TrimSpace(text))
	}
	return out
}

// overlapTail returns the trailing at-most-overlapSize bytes of s, backed up
// to a UTF-8 rune start so the slice is always valid UTF-8 (sizes are in
// bytes; a naive cut can start mid-rune in CJK text).
func overlapTail(s string, overlapSize int) string {
	if overlapSize <= 0 || len(s) <= overlapSize {
		return ""
	}
	start := len(s) - overlapSize
	for start > 0 && s[start]&0xC0 == 0x80 {
		start--
	}
	return s[start:]
}

func splitLargeParagraph(p string, maxChunkSize, overlapSize int) []string {
	sentences := splitSentences(p)
	var chunks []string
	var buf strings.Builder
	bufLen := 0

	for _, s := range sentences {
		if bufLen+len(s)+1 > maxChunkSize && bufLen > 0 {
			chunks = append(chunks, strings.TrimSpace(buf.String()))
			content := buf.String()
			// overlapTail keeps the cut on a rune boundary (see ChunkText).
			if tail := overlapTail(content, overlapSize); tail != "" {
				buf.Reset()
				buf.WriteString(tail)
				bufLen = len(tail)
			} else {
				buf.Reset()
				bufLen = 0
			}
		}
		if bufLen > 0 {
			buf.WriteByte(' ')
			bufLen++
		}
		buf.WriteString(s)
		bufLen += len(s)
	}
	if bufLen > 0 {
		chunks = append(chunks, strings.TrimSpace(buf.String()))
	}
	return chunks
}

func splitSentences(text string) []string {
	var sentences []string
	var buf strings.Builder
	for i, r := range text {
		buf.WriteRune(r)
		if r == '.' || r == '。' || r == '?' || r == '！' || r == '？' {
			// CJK terminators (。！？) and '?' are unambiguous: end here.
			// Only ASCII '.' needs the following-space check (decimals like
			// "3.5" aren't sentence ends). i is the byte START of the rune,
			// so peek the NEXT rune from i+utf8.RuneLen(r) — text[i+1] would
			// land on a continuation byte of the terminator itself for CJK,
			// never matching space/newline and silently disabling all CJK
			// sentence splitting.
			ended := true
			if r == '.' {
				after := i + utf8.RuneLen(r)
				ended = after >= len(text)
				if !ended {
					nr, _ := utf8.DecodeRuneInString(text[after:])
					ended = nr == ' ' || nr == '\n'
				}
			}
			if ended {
				s := strings.TrimSpace(buf.String())
				if s != "" {
					sentences = append(sentences, s)
				}
				buf.Reset()
			}
		}
	}
	if buf.Len() > 0 {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			sentences = append(sentences, s)
		}
	}
	if len(sentences) == 0 && strings.TrimSpace(text) != "" {
		t := strings.TrimSpace(text)
		for len(t) > 0 {
			if len(t) <= defaultChunkSize {
				sentences = append(sentences, t)
				break
			}
			end := defaultChunkSize
			for end > 0 && t[end]&0xC0 == 0x80 {
				end--
			}
			sentences = append(sentences, t[:end])
			t = t[end:]
		}
	}
	return sentences
}
