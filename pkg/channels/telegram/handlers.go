package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/nextlevelbuilder/goclaw/pkg/bus"
	"github.com/nextlevelbuilder/goclaw/pkg/channels"
	"github.com/nextlevelbuilder/goclaw/pkg/channels/typing"
)

// handleMessage processes an incoming Telegram update.
func (c *Channel) handleMessage(ctx context.Context, update telego.Update) {
	message := update.Message
	if message == nil {
		return
	}

	// Skip service messages (member added/removed, title changed, etc.).
	// These have no text/caption and no meaningful media — processing them
	// pollutes mention gate and history with "[empty message]" entries.
	if isServiceMessage(message) {
		slog.Debug("telegram service message skipped",
			"chat_id", message.Chat.ID,
			"new_members", len(message.NewChatMembers),
			"left_member", message.LeftChatMember != nil,
		)
		return
	}

	user := message.From
	if user == nil {
		return
	}

	userID := fmt.Sprintf("%d", user.ID)
	senderID := userID
	if user.Username != "" {
		senderID = fmt.Sprintf("%s|%s", userID, user.Username)
	}

	isGroup := message.Chat.Type == "group" || message.Chat.Type == "supergroup"

	slog.Debug("telegram message received",
		"chat_type", message.Chat.Type,
		"chat_id", message.Chat.ID,
		"is_group", isGroup,
		"user_id", user.ID,
		"username", user.Username,
		"channel", c.Name(),
		"text_preview", channels.Truncate(message.Text, 60),
	)

	// Forum detection (matching TS: resolveTelegramForumThreadId in src/telegram/bot/helpers.ts).
	// For non-forum groups: ignore message_thread_id (it's reply context, not a topic).
	// For forum groups without message_thread_id: default to General topic (ID=1).
	isForum := isGroup && message.Chat.IsForum
	messageThreadID := 0
	if isForum {
		messageThreadID = message.MessageThreadID
		if messageThreadID == 0 {
			messageThreadID = telegramGeneralTopicID
		}
	}

	// DM thread detection: preserve message_thread_id in private chats for session isolation.
	// Telegram supports topics/threads in bot DMs.
	dmThreadID := 0
	if !isGroup && message.MessageThreadID > 0 {
		dmThreadID = message.MessageThreadID
	}

	chatID := message.Chat.ID
	chatIDStr := fmt.Sprintf("%d", chatID)

	// Resolve per-topic config (matching TS resolveTelegramGroupConfig).
	// Merges: global defaults → wildcard group ("*") → specific group → specific topic.
	var topicCfg resolvedTopicConfig
	if isGroup {
		topicCfg = resolveTopicConfig(c.config, chatIDStr, messageThreadID)
	}

	// Group policy + enabled check (matching TS: groupPolicy ?? "open").
	if isGroup {
		// Per-topic enabled gate: if explicitly disabled, reject.
		if !topicCfg.isEnabled() {
			slog.Debug("telegram group message rejected: topic disabled",
				"chat_id", chatID, "topic_id", messageThreadID)
			return
		}

		groupPolicy := topicCfg.groupPolicy
		if groupPolicy == "" {
			groupPolicy = "open"
		}

		switch groupPolicy {
		case "disabled":
			slog.Debug("telegram group message rejected: groups disabled", "chat_id", message.Chat.ID)
			return
		case "allowlist":
			allowed := false
			for _, a := range topicCfg.allowFrom {
				if a == userID || a == senderID {
					allowed = true
					break
				}
			}
			if !allowed {
				slog.Debug("telegram group message rejected by allowlist",
					"user_id", userID, "username", user.Username, "chat_id", chatID,
				)
				return
			}
		default: // "open"
		}
	}

	// DM access control (matching TS: default is "pairing").
	if !isGroup {
		dmPolicy := c.config.DMPolicy
		if dmPolicy == "" {
			dmPolicy = "pairing"
		}

		switch dmPolicy {
		case "disabled":
			slog.Debug("telegram message rejected: DMs disabled", "user_id", userID)
			return

		case "open":
			// Allow all senders.

		case "allowlist":
			if !c.IsAllowed(userID) && !c.IsAllowed(senderID) {
				slog.Debug("telegram message rejected by allowlist",
					"user_id", userID, "username", user.Username,
				)
				return
			}

		default: // "pairing" or unknown → secure default
			paired := false
			if c.pairingService != nil {
				paired = c.pairingService.IsPaired(userID, c.Name()) || c.pairingService.IsPaired(senderID, c.Name())
			}
			inAllowList := c.HasAllowList() && (c.IsAllowed(userID) || c.IsAllowed(senderID))

			if !paired && !inAllowList {
				slog.Debug("telegram message rejected: sender not paired",
					"user_id", userID, "username", user.Username, "dm_policy", dmPolicy,
				)
				c.sendPairingReply(ctx, message.Chat.ID, userID, user.Username)
				return
			}
		}
	}

	// Build composite localKey for sync.Map operations.
	// Forum topics get separate state (placeholders, streams, reactions, history).
	// TS ref: buildTelegramGroupPeerId() in src/telegram/bot/helpers.ts.
	localKey := chatIDStr
	if isForum && messageThreadID > 0 {
		localKey = fmt.Sprintf("%s:topic:%d", chatIDStr, messageThreadID)
	} else if dmThreadID > 0 {
		localKey = fmt.Sprintf("%s:thread:%d", chatIDStr, dmThreadID)
	}

	// Store thread ID for streaming/send use (looked up by localKey later).
	if messageThreadID > 0 {
		c.threadIDs.Store(localKey, messageThreadID)
	} else if dmThreadID > 0 {
		c.threadIDs.Store(localKey, dmThreadID)
	}

	// Extract text content
	content := ""
	if message.Text != "" {
		content += message.Text
	}
	if message.Caption != "" {
		if content != "" {
			content += "\n"
		}
		content += message.Caption
	}

	// Process media (photos, audio, voice, documents)
	mediaList := c.resolveMedia(ctx, message)
	var mediaPaths []string

	if len(mediaList) > 0 {
		// First pass: process each media item.
		// For audio/voice: attempt STT transcription so that buildMediaTags can embed the transcript.
		// For documents: extract text content to append after the media tags.
		// Note: buildMediaTags is called AFTER this loop so it picks up populated Transcript fields.
		var extraContent string
		for i := range mediaList {
			m := &mediaList[i]

			switch m.Type {
			case "audio", "voice":
				transcript, sttErr := c.transcribeAudio(ctx, m.FilePath)
				if sttErr != nil {
					slog.Warn("telegram: STT transcription failed, falling back to media placeholder",
						"type", m.Type, "error", sttErr,
					)
				} else {
					m.Transcript = transcript
				}

			case "document":
				// Extract text content from documents
				if m.FileName != "" && m.FilePath != "" {
					docContent, err := extractDocumentContent(m.FilePath, m.FileName)
					if err != nil {
						slog.Warn("document extraction failed", "file", m.FileName, "error", err)
					} else if docContent != "" {
						extraContent += "\n\n" + docContent
					}
				}

			case "video", "animation":
				// Video: notify user that video is not fully supported yet.
				// Only add the notice when there is no caption/text — media tags haven't been
				// prepended yet at this stage of the pipeline.
				if content == "" {
					extraContent += "\n\n[Video received — video content analysis is not yet supported, only caption text is processed]"
				}
			}

			if m.FilePath != "" {
				mediaPaths = append(mediaPaths, m.FilePath)
			}
		}

		// Build media tags AFTER the processing loop so transcript fields are populated.
		mediaTags := buildMediaTags(mediaList)
		if mediaTags != "" {
			if content != "" {
				content = mediaTags + "\n\n" + content
			} else {
				content = mediaTags
			}
		}

		// Append any extra content accumulated during processing (doc text, video note, etc.)
		if extraContent != "" {
			content += extraContent
		}
	}

	// Handle bot commands BEFORE enriching with reply/forward context.
	// Command parsing (SplitN on spaces) breaks when reply context is appended with newlines,
	// e.g. "/addwriter@bot\n\n[Replying to ...]" — the bot-username check fails.
	if handled := c.handleBotCommand(ctx, message, chatID, chatIDStr, localKey, content, senderID, isGroup, isForum, messageThreadID); handled {
		return
	}

	// Enrich content with forward/reply/location context
	msgCtx := buildMessageContext(message, c.bot.Username())
	content = enrichContentWithContext(content, msgCtx)

	if content == "" {
		content = "[empty message]"
	}

	// Compute sender label for group context (used in history + current message annotation)
	senderLabel := user.FirstName
	if user.Username != "" {
		senderLabel = "@" + user.Username
	}

	// --- Group mention gating (matching TS mentionGate logic) ---
	// Also check implicit mention via reply-to-bot
	if isGroup && topicCfg.effectiveRequireMention(c.requireMention) {
		botUsername := c.bot.Username()
		wasMentioned := c.detectMention(message, botUsername)

		// Reply to bot's message counts as implicit mention
		if !wasMentioned && msgCtx.ReplyInfo != nil && msgCtx.ReplyInfo.IsBotReply {
			wasMentioned = true
		}

		slog.Debug("telegram group mention gate",
			"chat_id", chatID,
			"bot_username", botUsername,
			"require_mention", c.requireMention,
			"was_mentioned", wasMentioned,
			"text_preview", channels.Truncate(content, 60),
		)

		if !wasMentioned {
			c.groupHistory.Record(localKey, channels.HistoryEntry{
				Sender:    senderLabel,
				Body:      content,
				Timestamp: time.Unix(int64(message.Date), 0),
				MessageID: fmt.Sprintf("%d", message.MessageID),
			}, c.historyLimit)

			slog.Debug("telegram group message recorded (no mention)",
				"chat_id", chatID, "sender", senderLabel,
			)
			return
		}
	}

	// --- Group pairing gate (only reached when bot is mentioned) ---
	if isGroup && topicCfg.groupPolicy == "pairing" && c.pairingService != nil {
		if _, cached := c.approvedGroups.Load(chatIDStr); !cached {
			groupSenderID := fmt.Sprintf("group:%d", chatID)
			if c.pairingService.IsPaired(groupSenderID, c.Name()) {
				c.approvedGroups.Store(chatIDStr, true)
			} else {
				c.sendGroupPairingReply(ctx, chatID, chatIDStr, groupSenderID, localKey, messageThreadID)
				return
			}
		}
	}

	slog.Debug("telegram message received",
		"sender_id", senderID,
		"chat_id", fmt.Sprintf("%d", chatID),
		"preview", channels.Truncate(content, 50),
	)

	// Build context from pending group history (if any).
	// Annotate current message with sender name so LLM knows who is talking.
	finalContent := content
	if isGroup {
		annotated := fmt.Sprintf("[From: %s]\n%s", senderLabel, content)
		if c.historyLimit > 0 {
			finalContent = c.groupHistory.BuildContext(localKey, annotated, c.historyLimit)
		} else {
			finalContent = annotated
		}
	}

	// Send typing indicator with keepalive + TTL safety net.
	// Telegram typing expires after 5s, so keepalive every 4s.
	// TTL auto-stops after 60s to prevent stuck indicators.
	chatIDObj := tu.ID(chatID)
	typingCtrl := typing.New(typing.Options{
		MaxDuration:       60 * time.Second,
		KeepaliveInterval: 4 * time.Second,
		StartFn: func() error {
			action := tu.ChatAction(chatIDObj, telego.ChatActionTyping)
			if messageThreadID > 0 {
				action.MessageThreadID = messageThreadID
			}
			return c.bot.SendChatAction(ctx, action)
		},
	})
	// Stop previous typing controller for this chat/topic (if any)
	if prev, ok := c.typingCtrls.Load(localKey); ok {
		prev.(*typing.Controller).Stop()
	}
	c.typingCtrls.Store(localKey, typingCtrl)
	typingCtrl.Start()

	// Stop previous thinking animation for this chat/topic
	if prevStop, ok := c.stopThinking.Load(localKey); ok {
		if cf, ok := prevStop.(*thinkingCancel); ok {
			cf.Cancel()
		}
	}

	// Create thinking cancel for this chat/topic
	_, thinkCancel := context.WithCancel(ctx)
	c.stopThinking.Store(localKey, &thinkingCancel{fn: thinkCancel})

	// Send placeholder message only for DMs.
	// In groups the placeholder drifts away as new messages arrive;
	// instead the response will be sent as a reply to the sender's message.
	if !isGroup {
		thinkMsg := tu.Message(chatIDObj, "Thinking...")
		if dmThreadID > 0 {
			thinkMsg.MessageThreadID = dmThreadID
		}
		pMsg, err := c.bot.SendMessage(ctx, thinkMsg)
		if err == nil {
			c.placeholders.Store(localKey, pMsg.MessageID)
		}
	}

	metadata := map[string]string{
		"message_id": fmt.Sprintf("%d", message.MessageID),
		"user_id":    fmt.Sprintf("%d", user.ID),
		"username":   user.Username,
		"first_name": user.FirstName,
		"is_group":   fmt.Sprintf("%t", isGroup),
		"local_key":  localKey,
	}
	if isForum {
		metadata["is_forum"] = "true"
		metadata["message_thread_id"] = fmt.Sprintf("%d", messageThreadID)
	}
	if dmThreadID > 0 {
		metadata["dm_thread_id"] = fmt.Sprintf("%d", dmThreadID)
		metadata["message_thread_id"] = fmt.Sprintf("%d", dmThreadID)
	}
	if topicCfg.systemPrompt != "" {
		metadata["topic_system_prompt"] = topicCfg.systemPrompt
	}
	if topicCfg.skills != nil {
		metadata["topic_skills"] = strings.Join(topicCfg.skills, ",")
	}

	peerKind := "direct"
	if isGroup {
		peerKind = "group"
	}

	// Audio-aware routing: if a voice/audio message was received and a dedicated speaking agent
	// is configured, route to that agent instead of the default channel agent.
	// This prevents voice turns from landing on a text-router agent that cannot handle audio.
	targetAgentID := c.AgentID()
	if c.config.VoiceAgentID != "" {
		for _, m := range mediaList {
			if m.Type == "audio" || m.Type == "voice" {
				targetAgentID = c.config.VoiceAgentID
				slog.Debug("telegram: routing voice inbound to speaking agent",
					"agent_id", targetAgentID, "media_type", m.Type,
				)
				break
			}
		}
	}

	c.Bus().PublishInbound(bus.InboundMessage{
		Channel:      c.Name(),
		SenderID:     senderID,
		ChatID:       chatIDStr,
		Content:      finalContent,
		Media:        mediaPaths,
		PeerKind:     peerKind,
		UserID:       userID,
		AgentID:      targetAgentID,
		HistoryLimit: c.historyLimit,
		ToolAllow:    topicCfg.tools,
		Metadata:     metadata,
	})

	// Clear pending history after sending to agent.
	if isGroup {
		c.groupHistory.Clear(localKey)
	}
}

// detectMention checks if a Telegram message mentions the bot.
// Checks both msg.Text/Entities (text messages) and msg.Caption/CaptionEntities (photo/media messages).
func (c *Channel) detectMention(msg *telego.Message, botUsername string) bool {
	if botUsername == "" {
		return false
	}
	lowerBot := strings.ToLower(botUsername)

	// Check both text entities and caption entities (photos use Caption, not Text).
	for _, pair := range []struct {
		entities []telego.MessageEntity
		text     string
	}{
		{msg.Entities, msg.Text},
		{msg.CaptionEntities, msg.Caption},
	} {
		if pair.text == "" {
			continue
		}
		for _, entity := range pair.entities {
			if entity.Type == "mention" {
				mentioned := pair.text[entity.Offset : entity.Offset+entity.Length]
				if strings.EqualFold(mentioned, "@"+botUsername) {
					return true
				}
			}
			if entity.Type == "bot_command" {
				cmdText := pair.text[entity.Offset : entity.Offset+entity.Length]
				if strings.Contains(strings.ToLower(cmdText), "@"+lowerBot) {
					return true
				}
			}
		}
	}

	// Fallback: substring check in both text and caption
	if msg.Text != "" && strings.Contains(strings.ToLower(msg.Text), "@"+lowerBot) {
		return true
	}
	if msg.Caption != "" && strings.Contains(strings.ToLower(msg.Caption), "@"+lowerBot) {
		return true
	}

	// Reply to bot's message = implicit mention
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		if msg.ReplyToMessage.From.Username == botUsername {
			return true
		}
	}

	return false
}

// isServiceMessage returns true if the Telegram message is a service/system message
// (member added/removed, title changed, pinned, etc.) rather than a user-sent message.
// Service messages have no text, caption, or media content.
func isServiceMessage(msg *telego.Message) bool {
	// Has text or caption → user message
	if msg.Text != "" || msg.Caption != "" {
		return false
	}

	// Has media → user message (photo, audio, video, document, sticker, etc.)
	if msg.Photo != nil || msg.Audio != nil || msg.Video != nil ||
		msg.Document != nil || msg.Voice != nil || msg.VideoNote != nil ||
		msg.Sticker != nil || msg.Animation != nil || msg.Contact != nil ||
		msg.Location != nil || msg.Venue != nil || msg.Poll != nil {
		return false
	}

	// No user content — likely a service message (new_chat_members, left_chat_member,
	// new_chat_title, new_chat_photo, pinned_message, etc.)
	return true
}
