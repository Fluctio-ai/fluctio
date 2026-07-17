package kb

import "strings"

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
			// Keep overlap from end of buffer
			content := buf.String()
			if len(content) > overlapSize {
				buf.Reset()
				buf.WriteString(content[len(content)-overlapSize:])
				bufLen = overlapSize
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

func splitLargeParagraph(p string, maxChunkSize, overlapSize int) []string {
	sentences := splitSentences(p)
	var chunks []string
	var buf strings.Builder
	bufLen := 0

	for _, s := range sentences {
		if bufLen+len(s)+1 > maxChunkSize && bufLen > 0 {
			chunks = append(chunks, strings.TrimSpace(buf.String()))
			content := buf.String()
			if len(content) > overlapSize {
				buf.Reset()
				buf.WriteString(content[len(content)-overlapSize:])
				bufLen = overlapSize
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
			// End of sentence if followed by space or end of text. i is a
			// byte offset (range over string), so peek the next byte directly —
			// rune(text[i+1]) mis-decodes when i+1 lands inside a CJK char.
			ended := i+1 >= len(text)
			if !ended {
				nb := text[i+1]
				ended = nb == ' ' || nb == '\n'
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
