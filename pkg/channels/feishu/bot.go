package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nextlevelbuilder/goclaw/pkg/channels"
)

// messageContext holds parsed information from a Feishu message event.
type messageContext struct {
	ChatID      string
	MessageID   string
	SenderID    string // sender_id.open_id
	ChatType    string // "p2p" or "group"
	Content     string
	ContentType string // "text", "post", "image", etc.
	MentionedBot bool
	RootID      string // thread root message ID
	ParentID    string // parent message ID
	Mentions    []mentionInfo
}

type mentionInfo struct {
	Key    string // @_user_N placeholder
	OpenID string
	Name   string
}

// handleMessageEvent processes an incoming Feishu message event.
func (c *Channel) handleMessageEvent(ctx context.Context, event *MessageEvent) {
	if event == nil {
		return
	}

	msg := &event.Event.Message
	sender := &event.Event.Sender

	messageID := msg.MessageID
	if messageID == "" {
		return
	}

	// 1. Dedup check
	if c.isDuplicate(messageID) {
		slog.Debug("feishu message deduplicated", "message_id", messageID)
		return
	}

	// 2. Parse message
	mc := c.parseMessageEvent(event)
	if mc == nil {
		return
	}

	// 3. Resolve sender name (cached)
	senderName := c.resolveSenderName(ctx, mc.SenderID)

	// 4. Group policy
	if mc.ChatType == "group" {
		if !c.checkGroupPolicy(mc.SenderID, mc.ChatID) {
			slog.Debug("feishu group message rejected by policy", "sender_id", mc.SenderID, "chat_id", mc.ChatID)
			return
		}

		// 5. RequireMention check — record to history if not mentioned
		requireMention := true
		if c.cfg.RequireMention != nil {
			requireMention = *c.cfg.RequireMention
		}
		if requireMention && !mc.MentionedBot {
			historyKey := mc.ChatID
			if mc.RootID != "" && c.cfg.TopicSessionMode == "enabled" {
				historyKey = fmt.Sprintf("%s:topic:%s", mc.ChatID, mc.RootID)
			}
			c.groupHistory.Record(historyKey, channels.HistoryEntry{
				Sender:    senderName,
				Body:      mc.Content,
				Timestamp: time.Now(),
				MessageID: messageID,
			}, c.historyLimit)

			slog.Debug("feishu group message recorded (no mention)",
				"chat_id", mc.ChatID, "sender", senderName,
			)
			return
		}
	}

	// 6. DM policy (pairing flow)
	if mc.ChatType == "p2p" {
		if !c.checkDMPolicy(mc.SenderID, mc.ChatID) {
			return
		}
	}

	// 7. Build content (strip bot mention from text)
	content := mc.Content
	if content == "" {
		content = "[empty message]"
	}

	// 8. Topic session
	chatID := mc.ChatID
	if mc.RootID != "" && c.cfg.TopicSessionMode == "enabled" {
		chatID = fmt.Sprintf("%s:topic:%s", mc.ChatID, mc.RootID)
	}

	slog.Debug("feishu message received",
		"sender_id", mc.SenderID,
		"sender_name", senderName,
		"chat_id", chatID,
		"chat_type", mc.ChatType,
		"mentioned_bot", mc.MentionedBot,
		"preview", channels.Truncate(content, 50),
	)

	// 9. Build metadata
	peerKind := "direct"
	if mc.ChatType == "group" {
		peerKind = "group"
	}

	metadata := map[string]string{
		"message_id":    messageID,
		"chat_type":     mc.ChatType,
		"sender_name":   senderName,
		"mentioned_bot": fmt.Sprintf("%t", mc.MentionedBot),
		"platform":      "feishu",
	}

	if sender != nil {
		metadata["sender_open_id"] = sender.SenderID.OpenID
	}

	// Build final content with group context (pending history + sender annotation).
	if mc.ChatType == "group" && senderName != "" {
		annotated := fmt.Sprintf("[From: %s]\n%s", senderName, content)
		if c.historyLimit > 0 {
			content = c.groupHistory.BuildContext(chatID, annotated, c.historyLimit)
		} else {
			content = annotated
		}
	}

	// 10. Resolve inbound media (image, file, audio, video, sticker)
	var mediaPaths []string
	switch mc.ContentType {
	case "image", "file", "audio", "video", "sticker":
		mediaPaths = c.resolveMediaFromMessage(ctx, mc.MessageID, mc.ContentType, msg.Content)
	}

	// 11. Publish to bus
	c.HandleMessage(mc.SenderID, chatID, content, mediaPaths, metadata, peerKind)

	// Clear pending history after sending to agent.
	if mc.ChatType == "group" {
		c.groupHistory.Clear(chatID)
	}
}
