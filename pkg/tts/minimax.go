package tts

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MiniMaxProvider implements TTS via the MiniMax T2A API.
// Docs: https://platform.minimax.io/docs/api-reference/speech-t2a-intro
//
// Supports 300+ system voices, 40+ languages, multiple output formats.
// Models: speech-02-hd (high quality), speech-02-turbo (fast).
type MiniMaxProvider struct {
	apiKey    string
	groupID   string // MiniMax GroupId (required)
	apiBase   string // default "https://api.minimax.io/v1"
	model     string // default "speech-02-hd"
	voiceID   string // default "Wise_Woman"
	timeoutMs int
}

// MiniMaxConfig configures the MiniMax TTS provider.
type MiniMaxConfig struct {
	APIKey    string
	GroupID   string
	APIBase   string
	Model     string
	VoiceID   string
	TimeoutMs int
}

// NewMiniMaxProvider creates a MiniMax TTS provider.
func NewMiniMaxProvider(cfg MiniMaxConfig) *MiniMaxProvider {
	p := &MiniMaxProvider{
		apiKey:    cfg.APIKey,
		groupID:   cfg.GroupID,
		apiBase:   cfg.APIBase,
		model:     cfg.Model,
		voiceID:   cfg.VoiceID,
		timeoutMs: cfg.TimeoutMs,
	}
	if p.apiBase == "" {
		p.apiBase = "https://api.minimax.io/v1"
	}
	if p.model == "" {
		p.model = "speech-02-hd"
	}
	if p.voiceID == "" {
		p.voiceID = "Wise_Woman"
	}
	if p.timeoutMs <= 0 {
		p.timeoutMs = 30000
	}
	return p
}

func (p *MiniMaxProvider) Name() string { return "minimax" }

// Synthesize calls the MiniMax T2A v2 endpoint (non-streaming).
// Response audio is returned as hex-encoded bytes in response.data.audio.
func (p *MiniMaxProvider) Synthesize(ctx context.Context, text string, opts Options) (*SynthResult, error) {
	voiceID := opts.Voice
	if voiceID == "" {
		voiceID = p.voiceID
	}
	model := opts.Model
	if model == "" {
		model = p.model
	}

	// Determine output format
	audioFormat := "mp3"
	ext := "mp3"
	mime := "audio/mpeg"
	if opts.Format == "opus" || opts.Format == "pcm" || opts.Format == "flac" || opts.Format == "wav" {
		audioFormat = opts.Format
		switch opts.Format {
		case "pcm":
			ext = "pcm"
			mime = "audio/pcm"
		case "flac":
			ext = "flac"
			mime = "audio/flac"
		case "wav":
			ext = "wav"
			mime = "audio/wav"
		}
	}

	body := map[string]interface{}{
		"text":   text,
		"model":  model,
		"stream": false,
		"voice_setting": map[string]interface{}{
			"voice_id": voiceID,
			"speed":    1.0,
			"pitch":    0,
		},
		"audio_setting": map[string]interface{}{
			"format": audioFormat,
		},
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal minimax tts request: %w", err)
	}

	url := fmt.Sprintf("%s/t2a_v2?GroupId=%s", p.apiBase, p.groupID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create minimax tts request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := &http.Client{Timeout: time.Duration(p.timeoutMs) * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax tts request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read minimax tts response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimax tts error %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response: { base_resp: {status_code}, data: {audio: "hex..."} }
	var apiResp miniMaxResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parse minimax tts response: %w", err)
	}

	if apiResp.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("minimax tts api error %d: %s", apiResp.BaseResp.StatusCode, apiResp.BaseResp.StatusMsg)
	}

	if apiResp.Data.Audio == "" {
		return nil, fmt.Errorf("minimax tts returned empty audio")
	}

	// Decode hex-encoded audio
	audio, err := hex.DecodeString(apiResp.Data.Audio)
	if err != nil {
		return nil, fmt.Errorf("decode minimax tts audio hex: %w", err)
	}

	return &SynthResult{
		Audio:     audio,
		Extension: ext,
		MimeType:  mime,
	}, nil
}

type miniMaxResponse struct {
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
	Data struct {
		Audio string `json:"audio"` // hex-encoded audio bytes
	} `json:"data"`
}
