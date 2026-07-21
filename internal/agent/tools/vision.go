package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// RegisterVisionChain registers the vision tool against a provider chain.
// This gives the agent a multimodal fallback: when the primary model can't
// see images, it calls `vision` with the image URL + a question and gets
// back a text answer from a model that can. Absent credentials ⇒ the tool
// isn't visible to the agent at all.
func RegisterVisionChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil {
		return
	}
	// "none" sentinel: admin explicitly opted out. Skip registration so the
	// model falls back to its own multimodal capability (or does without).
	for _, ref := range chain.Order {
		name := ref
		if i := strings.IndexByte(ref, '/'); i >= 0 {
			name = ref[:i]
		}
		if name == "none" {
			return
		}
	}
	if !chain.Available() {
		return
	}
	r.Register("vision", "Understand an image via a multimodal vision model. Use this as a FALLBACK when you (the primary model) cannot see or recognize an image — e.g. the user uploaded a photo you can't view, or image_gen produced an image you need to verify. Pass the image as an http(s) URL or a data:image/...;base64,... URL, plus the question you want answered about it. Returns the vision model's text answer.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"image": map[string]interface{}{
				"type":        "string",
				"description": "Image to analyze: an http(s) URL or a data:image/...;base64,... URL",
			},
			"question": map[string]interface{}{
				"type":        "string",
				"description": "What to ask about the image. Default: describe it in detail.",
			},
		},
		"required": []string{"image"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args map[string]any
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		resp, err := chain.Execute(ctx, args)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}
