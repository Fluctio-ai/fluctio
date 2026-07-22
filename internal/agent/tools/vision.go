package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"os"
	"strings"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// visionMaxSide is the longest-side cap applied before sending an image to
// the model. 1568px is OpenAI's "high detail efficient" recommendation and
// a reasonable cap for GLM-4V / Qwen-VL too: anything bigger burns tokens
// without adding recognition fidelity, and some gateways reject very large
// payloads. Photos are downscaled; small images pass through unchanged.
const visionMaxSide = 1568

// RegisterVisionChain registers the vision tool against a provider chain.
// This gives the agent a multimodal fallback: when the primary model can't
// see images, it calls `vision` with an image FILE PATH (or http URL) plus a
// question and gets back a text answer from a model that can.
//
// Why the tool does ALL image preprocessing itself (not the LLM):
//   - An LLM cannot reliably copy large base64 into a tool argument — it
//     truncates, the endpoint receives a broken image, every call fails.
//  Taking a path and doing os.ReadFile in-process fixes that.
//   - Vision endpoints only accept PNG/JPEG/GIF/WebP. A user upload may be
//     BMP/TIFF/JFIF-with-weird-ext/… — the tool decodes and re-encodes to
//     JPEG so any decodable format works, no exec/python or authorization
//     round-trips needed from the agent.
//   - Large images are downscaled to keep payloads/tokens sane.
//
// Symmetric with image_gen: the model gives a short reference (a path), the
// tool does the heavy lifting.
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
	r.Register("vision", "Understand an image via a multimodal vision model. Use this as a FALLBACK when you (the primary model) cannot see or recognize an image — e.g. the user uploaded a photo you can't view, or image_gen produced an image you need to verify. Pass `image` as an ABSOLUTE FILE PATH (e.g. the path of a user-uploaded photo) OR an http(s) URL. Do NOT pass raw base64 — it gets truncated and the call fails; the tool reads, transcodes and downscales the file for you. Supported input formats: PNG/JPEG/GIF/WebP/BMP/TIFF (HEIC/SVG are NOT supported — ask the user to convert those). Returns the vision model's text answer to `question`.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"image": map[string]interface{}{
				"type":        "string",
				"description": "Absolute file path of the image, or an http(s) URL. Do NOT pass base64 — pass a path and the tool reads/transcodes it.",
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
		// If the model gave a file path (not a URL), read + transcode +
		// downscale ourselves. The model can't handle base64 without
		// truncating, so we take a path and produce a clean data URL.
		if img, ok := args["image"].(string); ok && !isImageURL(img) {
			dataURL, err := ReadImageAsDataURL(img)
			if err != nil {
				return "", err
			}
			args["image"] = dataURL
		}
		resp, err := chain.Execute(ctx, args)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}

// isImageURL reports whether s is already a form the provider can send
// verbatim (http(s) URL the endpoint fetches, or an inline data: URL).
// Anything else is treated as a file path and preprocessed on disk.
func isImageURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "data:")
}

// ReadImageAsDataURL reads the file at path, decodes it (any registered
// format: PNG/JPEG/GIF/BMP/TIFF/WEBP), downscales if its longest side exceeds
// visionMaxSide, and re-encodes as JPEG into a data: URL. Re-encoding to JPEG
// normalizes odd containers (JFIF, progressive, weird extensions) down to the
// one format every vision endpoint accepts. Returns a clear error for formats
// Go can't decode (HEIC/SVG/…).
func ReadImageAsDataURL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("vision: read image %q: %w", path, err)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("vision: decode %q (detected format %q) — if it's HEIC/SVG or another unsupported format, ask the user to convert to PNG/JPEG first: %w", path, format, err)
	}
	img = downscaleIfNeeded(img, visionMaxSide)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return "", fmt.Errorf("vision: encode jpeg: %w", err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// downscaleIfNeeded shrinks src so its longest side is at most maxSide, using
// Catmull-Rom for decent quality. Images already within the cap pass through
// unchanged. Maintains aspect ratio.
func downscaleIfNeeded(src image.Image, maxSide int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxSide && h <= maxSide {
		return src
	}
	var nw, nh int
	if w >= h {
		nw = maxSide
		nh = max(1, h*maxSide/w)
	} else {
		nh = maxSide
		nw = max(1, w*maxSide/h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}
