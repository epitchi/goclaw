package tts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// EdgeProvider implements TTS via Microsoft Edge TTS (free, no API key).
// Matching TS edgeTTS() in src/tts/tts-core.ts.
// Requires the `edge-tts` CLI tool to be installed:
//
//	pip install edge-tts
type EdgeProvider struct {
	voice     string // default "en-US-MichelleNeural"
	rate      string // speech rate, e.g. "+0%"
	timeoutMs int
}

// EdgeConfig configures the Edge TTS provider.
type EdgeConfig struct {
	Voice     string
	Rate      string
	TimeoutMs int
}

// NewEdgeProvider creates an Edge TTS provider.
func NewEdgeProvider(cfg EdgeConfig) *EdgeProvider {
	p := &EdgeProvider{
		voice:     cfg.Voice,
		rate:      cfg.Rate,
		timeoutMs: cfg.TimeoutMs,
	}
	if p.voice == "" {
		p.voice = "en-US-MichelleNeural"
	}
	if p.timeoutMs <= 0 {
		p.timeoutMs = 30000
	}
	return p
}

func (p *EdgeProvider) Name() string { return "edge" }

// Synthesize runs the edge-tts CLI to generate audio.
// Output is always MP3 (edge-tts default format: audio-24khz-48kbitrate-mono-mp3).
func (p *EdgeProvider) Synthesize(ctx context.Context, text string, _ Options) (*SynthResult, error) {
	// Create temp file for output
	tmpDir := os.TempDir()
	outPath := filepath.Join(tmpDir, fmt.Sprintf("tts-%d.mp3", time.Now().UnixNano()))
	defer os.Remove(outPath)

	args := []string{
		"--voice", p.voice,
		"--text", text,
		"--write-media", outPath,
	}
	if p.rate != "" {
		args = append(args, "--rate", p.rate)
	}

	timeout := time.Duration(p.timeoutMs) * time.Millisecond
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "edge-tts", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("edge-tts failed: %w (output: %s)", err, string(output))
	}

	audio, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read edge-tts output: %w", err)
	}

	return &SynthResult{
		Audio:     audio,
		Extension: "mp3",
		MimeType:  "audio/mpeg",
	}, nil
}
