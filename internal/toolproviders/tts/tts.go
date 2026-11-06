// Package tts bundles text-to-speech providers. The category returns the
// generated audio as a MEDIA: line per tmp file, so the chat pipeline
// auto-attaches the clip to the assistant's outgoing message.
package tts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// Category is the tool category these providers plug into.
const Category = "tts"

// RegisterAll installs built-in TTS providers in r.
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&OpenAI{})
	r.Register(&MiniMax{})
	r.Register(&ElevenLabs{})
	r.Register(&Fish{})
	r.Register(&None{})
}

type args struct {
	Text  string
	Voice string
}

// ttsMaxTextBytes bounds the synthesis input. The LLM controls Text, and
// an unbounded script let a single call burn unbounded external-API
// quota; 20KB comfortably covers any spoken-length script while capping
// the blast radius.
const ttsMaxTextBytes = 20 << 10

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["text"].(string); ok {
		a.Text = strings.TrimSpace(s)
	}
	if a.Text == "" {
		return a, fmt.Errorf("text is required")
	}
	// Clamp on a rune boundary so CJK text isn't cut mid-character.
	if len(a.Text) > ttsMaxTextBytes {
		cut := a.Text[:ttsMaxTextBytes]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		a.Text = cut
	}
	if s, ok := raw["voice"].(string); ok {
		a.Voice = s
	}
	return a, nil
}

// writeAudio dumps audio bytes to a tmp file and returns an LLM-visible
// response. The MEDIA: marker lines are consumed by the gateway's
// extractMediaLineAttachments, which reads the staged file into an
// outbound MediaItem (audio attachment on IM) and strips the marker from
// the delivered text. The temp file is removed on successful read;
// orphaned files from failed deliveries are bounded by the OS temp dir.
func writeAudio(data []byte, ext string) (toolproviders.Response, error) {
	if len(data) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	if ext == "" {
		ext = "mp3"
	}
	f, err := os.CreateTemp("", "fluctio-tts-*."+ext)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("create tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return toolproviders.Response{}, fmt.Errorf("write tmp: %w", err)
	}
	f.Close()
	path, _ := filepath.Abs(f.Name())
	// First line is an LLM-visible status; the MEDIA: line is consumed by
	// the loop before the text reaches the model on the next turn.
	text := fmt.Sprintf("Generated audio: %s\nMEDIA:%s", filepath.Base(path), path)
	return toolproviders.Response{Text: text}, nil
}
