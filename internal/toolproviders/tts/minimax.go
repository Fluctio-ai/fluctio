package tts

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// MiniMax posts to /v1/t2a_v2. Model defaults to "speech-02-hd". Voice is
// the "voice_id" MiniMax expects (e.g. "male-qn-qingse"). The API returns
// hex-encoded audio bytes in data.audio.
type MiniMax struct{}

func (MiniMax) Category() string { return Category }
func (MiniMax) Name() string     { return "minimax" }

func (m *MiniMax) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("minimax: missing api key")
	}
	model := req.Config.Model
	if model == "" {
		model = "speech-02-hd"
	}
	voice := a.Voice
	if voice == "" {
		voice = "male-qn-qingse"
	}
	groupID := ""
	if req.Config.Options != nil {
		groupID = req.Config.Options["groupId"]
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"model": model,
		"text":  a.Text,
		"voice_setting": map[string]any{
			"voice_id": voice,
			"speed":    1.0,
		},
		"audio_setting": map[string]any{
			"sample_rate": 32000,
			"bitrate":     128000,
			"format":      "mp3",
		},
	}
	buf, _ := json.Marshal(body)
	endpoint := "https://api.minimaxi.com/v1/t2a_v2"
	if req.Config.Endpoint != "" {
		endpoint = req.Config.Endpoint
	}
	if groupID != "" {
		endpoint += "?GroupId=" + groupID
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("minimax tts: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("minimax", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var out struct {
		Data struct {
			Audio string `json:"audio"` // hex-encoded
		} `json:"data"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("minimax decode: %w", err)
	}
	if out.BaseResp.StatusCode != 0 {
		err := fmt.Errorf("minimax error %d: %s", out.BaseResp.StatusCode, out.BaseResp.StatusMsg)
		// MiniMax reports upstream problems in base_resp with HTTP 200,
		// so a plain return reads as fatal and cuts the provider fallback
		// chain even for rate limits / transient busy errors. Flag the
		// message-shaped transient classes as retriable; everything else
		// (auth, bad params) stays fatal.
		msg := strings.ToLower(out.BaseResp.StatusMsg)
		if strings.Contains(msg, "rate") || strings.Contains(msg, "limit") ||
			strings.Contains(msg, "busy") || strings.Contains(msg, "timeout") ||
			strings.Contains(msg, "unavailable") || strings.Contains(msg, "try again") {
			return toolproviders.Response{}, toolproviders.Retry(err)
		}
		return toolproviders.Response{}, err
	}
	data, err := hex.DecodeString(out.Data.Audio)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("minimax hex decode: %w", err)
	}
	return writeAudio(data, "mp3")
}
