package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	streamingMinInterval = 100 * time.Millisecond
	streamingElementID   = "content" // the markdown element ID in the card
)

// streamingSession manages a CardKit streaming card lifecycle.
// Matches TS streaming-card.ts FeishuStreamingSession.
type streamingSession struct {
	ch          *Channel
	cardID      string
	messageID   string
	sequence    int
	currentText string
	mu          sync.Mutex
	lastUpdate  time.Time
	closed      bool
}

// startStreaming creates a new streaming card and sends it as a message.
// Returns a session that can be updated and closed.
func (c *Channel) startStreaming(ctx context.Context, chatID, receiveIDType string) (*streamingSession, error) {
	// 1. Create card entity via CardKit API with streaming_mode: true
	cardJSON := buildStreamingCard("Thinking...")

	cardID, err := c.client.CreateCard(ctx, "card_json", cardJSON)
	if err != nil {
		return nil, fmt.Errorf("feishu create streaming card: %w", err)
	}
	if cardID == "" {
		return nil, fmt.Errorf("feishu create streaming card: no card_id in response")
	}

	// 2. Send the card as an interactive message
	msgContent := fmt.Sprintf(`{"type":"card","data":{"card_id":"%s"}}`, cardID)

	msgResp, err := c.client.SendMessage(ctx, receiveIDType, chatID, "interactive", msgContent)
	if err != nil {
		return nil, fmt.Errorf("feishu send streaming card: %w", err)
	}

	var messageID string
	if msgResp != nil {
		messageID = msgResp.MessageID
	}

	return &streamingSession{
		ch:          c,
		cardID:      cardID,
		messageID:   messageID,
		sequence:    1,
		currentText: "Thinking...",
		lastUpdate:  time.Now(),
	}, nil
}

// update sends a streaming text update to the card.
// Throttled to max 10/sec (100ms between updates).
func (s *streamingSession) update(ctx context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	// Throttle
	elapsed := time.Since(s.lastUpdate)
	if elapsed < streamingMinInterval {
		time.Sleep(streamingMinInterval - elapsed)
	}

	s.sequence++
	uuid := fmt.Sprintf("s_%s_%d", s.cardID, s.sequence)

	if err := s.ch.client.UpdateCardElement(ctx, s.cardID, streamingElementID, text, s.sequence, uuid); err != nil {
		slog.Debug("feishu streaming update failed", "error", err, "seq", s.sequence)
		return fmt.Errorf("feishu streaming update: %w", err)
	}

	s.currentText = text
	s.lastUpdate = time.Now()
	return nil
}

// close finalizes the streaming card: sends final text and disables streaming mode.
func (s *streamingSession) close(ctx context.Context, finalText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	// Send final text if different from current
	if finalText != "" && finalText != s.currentText {
		s.sequence++
		uuid := fmt.Sprintf("c_%s_%d", s.cardID, s.sequence)

		if err := s.ch.client.UpdateCardElement(ctx, s.cardID, streamingElementID, finalText, s.sequence, uuid); err != nil {
			slog.Debug("feishu streaming final update failed", "error", err)
		}
	}

	// Disable streaming mode
	s.sequence++
	settingsJSON := `{"config":{"streaming_mode":false}}`
	closeUUID := fmt.Sprintf("c_%s_%d", s.cardID, s.sequence)

	if err := s.ch.client.UpdateCardSettings(ctx, s.cardID, settingsJSON, s.sequence, closeUUID); err != nil {
		return fmt.Errorf("feishu close streaming: %w", err)
	}

	return nil
}

// --- Card JSON builders ---

// buildStreamingCard creates the initial card JSON for streaming mode.
// Matches TS streaming-card.ts initial card creation.
func buildStreamingCard(initialText string) string {
	card := map[string]interface{}{
		"schema": "2.0",
		"config": map[string]interface{}{
			"streaming_mode":   true,
			"wide_screen_mode": true,
			"summary": map[string]interface{}{
				"content": "[Generating...]",
			},
			"streaming": map[string]interface{}{
				"print_frequency_ms": 50,
				"print_step":         2,
			},
		},
		"body": map[string]interface{}{
			"elements": []map[string]interface{}{
				{
					"tag":        "markdown",
					"content":    initialText,
					"element_id": streamingElementID,
				},
			},
		},
	}

	data, _ := json.Marshal(card)
	return string(data)
}
