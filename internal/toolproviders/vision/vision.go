// Package vision bundles built-in image-understanding (vision) providers.
// A provider accepts an image (http URL or data URL) plus a question and
// returns the vision model's text answer. This is the multimodal fallback
// for agents whose primary model can't see images: the LLM calls the
// `vision` tool, which routes the image through a model that can.
package vision

import (
	"fmt"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// Category is the tool category these providers plug into.
const Category = "vision"

// RegisterAll installs every built-in vision provider in r.
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&OpenAI{})
	r.Register(&None{})
}

type args struct {
	Image    string
	Question string
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["image"].(string); ok {
		a.Image = strings.TrimSpace(s)
	}
	if a.Image == "" {
		return a, fmt.Errorf("image is required (http(s) URL or data URL)")
	}
	if s, ok := raw["question"].(string); ok {
		a.Question = strings.TrimSpace(s)
	}
	if a.Question == "" {
		a.Question = "Describe this image in detail."
	}
	return a, nil
}
