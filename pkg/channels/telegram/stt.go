package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	// defaultSTTTimeoutSeconds is the default timeout for STT proxy requests.
	defaultSTTTimeoutSeconds = 30

	// sttTranscribeEndpoint is the path appended to STTProxyURL.
	sttTranscribeEndpoint = "/transcribe_audio"
)

// sttResponse is the expected JSON response from the STT proxy.
type sttResponse struct {
	Transcript string `json:"transcript"`
}

// transcribeAudio calls the configured STT proxy service with the given audio file and returns
// the transcribed text. It returns ("", nil) silently when:
//   - STT is not configured (STTProxyURL is empty), or
//   - filePath is empty (download failed earlier in the pipeline).
//
// Any HTTP or parse error is returned so the caller can log it and fall back gracefully.
// This matches the TS speaking-service /transcribe_audio contract used in managed deployments.
func (c *Channel) transcribeAudio(ctx context.Context, filePath string) (string, error) {
	if c.config.STTProxyURL == "" {
		// STT not configured — skip silently.
		return "", nil
	}
	if filePath == "" {
		// File download failed earlier; nothing to transcribe.
		return "", nil
	}

	// Resolve request timeout.
	timeoutSec := c.config.STTTimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = defaultSTTTimeoutSeconds
	}

	// Open the downloaded audio file.
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("stt: open audio file %q: %w", filePath, err)
	}
	defer f.Close()

	// Build multipart/form-data body.
	// Fields:
	//   file      — audio file bytes (required)
	//   tenant_id — optional tenant identifier forwarded to the proxy
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	fw, err := w.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("stt: create form file field: %w", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", fmt.Errorf("stt: write audio bytes to form: %w", err)
	}

	if c.config.STTTenantID != "" {
		if err := w.WriteField("tenant_id", c.config.STTTenantID); err != nil {
			return "", fmt.Errorf("stt: write tenant_id field: %w", err)
		}
	}

	if err := w.Close(); err != nil {
		return "", fmt.Errorf("stt: close multipart writer: %w", err)
	}

	// Build HTTP request with a deadline.
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	url := c.config.STTProxyURL + sttTranscribeEndpoint
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, &body)
	if err != nil {
		return "", fmt.Errorf("stt: build request to %q: %w", url, err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if c.config.STTAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.STTAPIKey)
	}

	slog.Debug("telegram: calling STT proxy", "url", url, "file", filepath.Base(filePath))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stt: request to %q failed: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return "", fmt.Errorf("stt: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stt: upstream returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result sttResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("stt: parse response JSON: %w", err)
	}

	slog.Debug("telegram: STT transcript received",
		"length", len(result.Transcript),
		"preview", func() string {
			if len(result.Transcript) > 80 {
				return result.Transcript[:80] + "..."
			}
			return result.Transcript
		}(),
	)

	return result.Transcript, nil
}
