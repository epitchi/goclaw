package telegram

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	// defaultStreamThrottle is the minimum delay between message edits (matching TS: 1000ms).
	defaultStreamThrottle = 1000 * time.Millisecond

	// streamMaxChars is the max message length for streaming (Telegram limit).
	streamMaxChars = 4096
)

// DraftStream manages a streaming preview message that gets edited as content arrives.
// Ref: TS src/telegram/draft-stream.ts → createTelegramDraftStream()
//
// State machine:
//
//	NOT_STARTED → first Update() → sendMessage (create) → STREAMING
//	STREAMING   → subsequent Update() → editMessageText (throttled) → STREAMING
//	STREAMING   → Stop() → final editMessageText → STOPPED
//	STREAMING   → Clear() → deleteMessage → DELETED
type DraftStream struct {
	bot             *telego.Bot
	chatID          int64
	messageThreadID int           // forum topic thread ID (0 = no thread)
	messageID       int           // 0 = not yet created
	lastText        string        // last sent text (for dedup)
	throttle        time.Duration // min delay between edits
	lastEdit        time.Time
	mu              sync.Mutex
	stopped         bool
	pending         string // pending text to send (buffered during throttle)
}

// newDraftStream creates a new streaming preview manager.
func newDraftStream(bot *telego.Bot, chatID int64, throttleMs int, messageThreadID int) *DraftStream {
	throttle := defaultStreamThrottle
	if throttleMs > 0 {
		throttle = time.Duration(throttleMs) * time.Millisecond
	}
	return &DraftStream{
		bot:             bot,
		chatID:          chatID,
		messageThreadID: messageThreadID,
		throttle:        throttle,
	}
}

// Update sends or edits the streaming message with the latest text.
// Throttled to avoid hitting Telegram rate limits.
func (ds *DraftStream) Update(ctx context.Context, text string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if ds.stopped {
		return
	}

	// Truncate to Telegram max
	if len(text) > streamMaxChars {
		text = text[:streamMaxChars]
	}

	// Dedup: skip if text unchanged
	if text == ds.lastText {
		return
	}

	ds.pending = text

	// Check throttle
	if time.Since(ds.lastEdit) < ds.throttle {
		return
	}

	ds.flush(ctx)
}

// Flush forces sending the pending text immediately.
func (ds *DraftStream) Flush(ctx context.Context) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.flush(ctx)
}

// flush sends/edits the pending text (must hold mu lock).
func (ds *DraftStream) flush(ctx context.Context) error {
	if ds.pending == "" || ds.pending == ds.lastText {
		return nil
	}

	text := ds.pending
	htmlText := markdownToTelegramHTML(text)

	if ds.messageID == 0 {
		// First message: send new
		// TS ref: buildTelegramThreadParams() — General topic (1) must be omitted.
		params := &telego.SendMessageParams{
			ChatID:    tu.ID(ds.chatID),
			Text:      htmlText,
			ParseMode: telego.ModeHTML,
		}
		if sendThreadID := resolveThreadIDForSend(ds.messageThreadID); sendThreadID > 0 {
			params.MessageThreadID = sendThreadID
		}
		msg, err := ds.bot.SendMessage(ctx, params)
		// TS ref: withTelegramThreadFallback — retry without thread ID when topic is deleted.
		if err != nil && params.MessageThreadID != 0 && threadNotFoundRe.MatchString(err.Error()) {
			slog.Warn("stream: thread not found, retrying without message_thread_id", "thread_id", params.MessageThreadID)
			params.MessageThreadID = 0
			msg, err = ds.bot.SendMessage(ctx, params)
		}
		if err != nil {
			slog.Debug("stream: failed to send initial message", "error", err)
			return err
		}
		ds.messageID = msg.MessageID
	} else {
		// Edit existing message
		editMsg := tu.EditMessageText(tu.ID(ds.chatID), ds.messageID, htmlText)
		editMsg.ParseMode = telego.ModeHTML
		if _, err := ds.bot.EditMessageText(ctx, editMsg); err != nil {
			// Ignore "not modified" errors
			if !messageNotModifiedRe.MatchString(err.Error()) {
				slog.Debug("stream: failed to edit message", "error", err)
			}
		}
	}

	ds.lastText = text
	ds.lastEdit = time.Now()
	return nil
}

// Stop finalizes the stream with a final edit.
func (ds *DraftStream) Stop(ctx context.Context) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	ds.stopped = true
	return ds.flush(ctx)
}

// Clear stops the stream and deletes the message.
func (ds *DraftStream) Clear(ctx context.Context) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	ds.stopped = true
	if ds.messageID != 0 {
		_ = ds.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
			ChatID:    tu.ID(ds.chatID),
			MessageID: ds.messageID,
		})
		ds.messageID = 0
	}
	return nil
}

// MessageID returns the streaming message ID (0 if not yet created).
func (ds *DraftStream) MessageID() int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.messageID
}

// --- StreamingChannel implementation ---

// OnStreamStart prepares for streaming by deleting the "Thinking..." placeholder.
// chatID here is the localKey (composite key with :topic:N suffix for forum topics).
func (c *Channel) OnStreamStart(ctx context.Context, chatID string) error {
	if c.config.StreamMode != "partial" {
		return nil
	}

	id, err := parseRawChatID(chatID)
	if err != nil {
		return err
	}

	// Delete placeholder if exists
	if pID, ok := c.placeholders.Load(chatID); ok {
		c.placeholders.Delete(chatID)
		_ = c.deleteMessage(ctx, id, pID.(int))
	}

	// Look up thread ID stored during handleMessage
	threadID := 0
	if v, ok := c.threadIDs.Load(chatID); ok {
		threadID = v.(int)
	}

	// Create draft stream for this chat
	ds := newDraftStream(c.bot, id, 0, threadID)
	c.streams.Store(chatID, ds)

	return nil
}

// OnChunkEvent updates the streaming message with accumulated content.
func (c *Channel) OnChunkEvent(ctx context.Context, chatID string, fullText string) error {
	if c.config.StreamMode != "partial" {
		return nil
	}

	val, ok := c.streams.Load(chatID)
	if !ok {
		return nil
	}

	ds := val.(*DraftStream)
	ds.Update(ctx, fullText)
	return nil
}

// OnStreamEnd finalizes the streaming preview.
// Instead of doing a final edit here, we hand the DraftStream's messageID
// back to the placeholders map so that Send() can edit it with the properly
// formatted final response. This avoids duplicate messages.
func (c *Channel) OnStreamEnd(ctx context.Context, chatID string, _ string) error {
	val, ok := c.streams.Load(chatID)
	if !ok {
		return nil
	}

	ds := val.(*DraftStream)

	// Mark stream as stopped (no more edits)
	ds.mu.Lock()
	ds.stopped = true
	msgID := ds.messageID
	ds.mu.Unlock()

	c.streams.Delete(chatID)

	// Hand the DraftStream message back as a placeholder so Send() will
	// edit it with the final formatted content instead of creating a new message.
	if msgID != 0 {
		c.placeholders.Store(chatID, msgID)
	}

	// Stop thinking animation
	if stop, ok := c.stopThinking.Load(chatID); ok {
		if cf, ok := stop.(*thinkingCancel); ok {
			cf.Cancel()
		}
		c.stopThinking.Delete(chatID)
	}

	return nil
}
