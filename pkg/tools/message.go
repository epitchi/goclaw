package tools

import (
	"context"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/pkg/bus"
)

// MessageTool allows the agent to proactively send messages to channels.
type MessageTool struct {
	sender ChannelSender
	msgBus *bus.MessageBus
}

func NewMessageTool() *MessageTool { return &MessageTool{} }

func (t *MessageTool) SetChannelSender(s ChannelSender) { t.sender = s }
func (t *MessageTool) SetMessageBus(b *bus.MessageBus)   { t.msgBus = b }

func (t *MessageTool) Name() string { return "message" }
func (t *MessageTool) Description() string {
	return "Send a message to a channel (Telegram, Discord, etc.) or the current chat."
}

func (t *MessageTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "Action to perform: 'send'",
				"enum":        []string{"send"},
			},
			"channel": map[string]interface{}{
				"type":        "string",
				"description": "Channel name (default: current channel from context)",
			},
			"target": map[string]interface{}{
				"type":        "string",
				"description": "Chat ID to send to (default: current chat from context)",
			},
			"message": map[string]interface{}{
				"type":        "string",
				"description": "Message content to send",
			},
		},
		"required": []string{"action", "message"},
	}
}

func (t *MessageTool) Execute(ctx context.Context, args map[string]interface{}) *Result {
	action, _ := args["action"].(string)
	if action != "send" {
		return ErrorResult(fmt.Sprintf("unsupported action: %s (only 'send' is supported)", action))
	}

	message, _ := args["message"].(string)
	if message == "" {
		return ErrorResult("message is required")
	}

	channel, _ := args["channel"].(string)
	if channel == "" {
		channel = ToolChannelFromCtx(ctx)
	}
	if channel == "" {
		return ErrorResult("channel is required (no current channel in context)")
	}

	target, _ := args["target"].(string)
	if target == "" {
		target = ToolChatIDFromCtx(ctx)
	}
	if target == "" {
		return ErrorResult("target chat ID is required (no current chat in context)")
	}

	// Prefer direct channel sender (channels.Manager.SendToChannel)
	if t.sender != nil {
		if err := t.sender(ctx, channel, target, message); err != nil {
			return ErrorResult(fmt.Sprintf("failed to send message: %v", err))
		}
		return SilentResult(fmt.Sprintf(`{"status":"sent","channel":"%s","target":"%s"}`, channel, target))
	}

	// Fallback: publish via message bus outbound queue
	if t.msgBus != nil {
		t.msgBus.PublishOutbound(bus.OutboundMessage{
			Channel: channel,
			ChatID:  target,
			Content: message,
		})
		return SilentResult(fmt.Sprintf(`{"status":"queued","channel":"%s","target":"%s"}`, channel, target))
	}

	return ErrorResult("no channel sender or message bus available")
}
