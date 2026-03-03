package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIProvider implements TTS via the OpenAI audio/speech API.
// Matching TS openaiTTS() in src/tts/tts-core.ts.
type OpenAIProvider struct {
	apiKey    string
	apiBase   string
	model     string // default "gpt-4o-mini-tts"
	voice     string // default "alloy"
	timeoutMs int    // default 30000
}

// OpenAIConfig configures the OpenAI TTS provider.
type OpenAIConfig struct {
	APIKey    string
	APIBase   string
	Model     string
	Voice     string
	TimeoutMs int
}

// NewOpenAIProvider creates an OpenAI TTS provider.
func NewOpenAIProvider(cfg OpenAIConfig) *OpenAIProvider {
	p := &OpenAIProvider{
		apiKey:    cfg.APIKey,
		apiBase:   cfg.APIBase,
		model:     cfg.Model,
		voice:     cfg.Voice,
		timeoutMs: cfg.TimeoutMs,
	}
	if p.apiBase == "" {
		p.apiBase = "https://api.openai.com/v1"
	}
	if p.model == "" {
		p.model = "gpt-4o-mini-tts"
	}
	if p.voice == "" {
		p.voice = "alloy"
	}
	if p.timeoutMs <= 0 {
		p.timeoutMs = 30000
	}
	return p
}

func (p *OpenAIProvider) Name() string { return "openai" }

// Synthesize calls the OpenAI audio/speech endpoint.
// Matching TS: POST {apiBase}/audio/speech with {model, input, voice, response_format}.
func (p *OpenAIProvider) Synthesize(ctx context.Context, text string, opts Options) (*SynthResult, error) {
	voice := opts.Voice
	if voice == "" {
		voice = p.voice
	}
	model := opts.Model
	if model == "" {
		model = p.model
	}
	format := opts.Format
	if format == "" {
		format = "mp3"
	}

	body := map[string]interface{}{
		"model":           model,
		"input":           text,
		"voice":           voice,
		"response_format": format,
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal openai tts request: %w", err)
	}

	url := p.apiBase + "/audio/speech"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create openai tts request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := &http.Client{Timeout: time.Duration(p.timeoutMs) * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai tts request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai tts error %d: %s", resp.StatusCode, string(errBody))
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read openai tts response: %w", err)
	}

	ext := format
	mime := "audio/mpeg"
	switch format {
	case "opus":
		ext = "ogg"
		mime = "audio/ogg"
	case "mp3":
		mime = "audio/mpeg"
	}

	return &SynthResult{
		Audio:     audio,
		Extension: ext,
		MimeType:  mime,
	}, nil
}
