package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/nextlevelbuilder/goclaw/pkg/bus"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// handleWriterCommand handles /addwriter and /removewriter commands.
// The target user is identified by replying to one of their messages.
func (c *Channel) handleWriterCommand(ctx context.Context, message *telego.Message, chatID int64, chatIDStr, senderID string, isGroup bool, setThread func(*telego.SendMessageParams), action string) {
	chatIDObj := tu.ID(chatID)

	send := func(text string) {
		msg := tu.Message(chatIDObj, text)
		setThread(msg)
		c.bot.SendMessage(ctx, msg)
	}

	if !isGroup {
		send("This command only works in group chats.")
		return
	}

	if c.agentStore == nil {
		send("File writer management is not available.")
		return
	}

	agentID, err := c.resolveAgentUUID(ctx)
	if err != nil {
		slog.Debug("writer command: agent resolve failed", "error", err)
		send("File writer management is not available (no agent).")
		return
	}

	groupID := fmt.Sprintf("group:%s:%s", c.Name(), chatIDStr)
	senderNumericID := strings.SplitN(senderID, "|", 2)[0]

	// Check if sender is an existing writer (only writers can manage the list)
	isWriter, err := c.agentStore.IsGroupFileWriter(ctx, agentID, groupID, senderNumericID)
	if err != nil {
		slog.Warn("writer check failed", "error", err, "sender", senderNumericID)
		send("Failed to check permissions. Please try again.")
		return
	}
	if !isWriter {
		send("Only existing file writers can manage the writer list.")
		return
	}

	// Extract target user from reply-to message
	if message.ReplyToMessage == nil || message.ReplyToMessage.From == nil {
		verb := "add"
		if action == "remove" {
			verb = "remove"
		}
		send(fmt.Sprintf("To %s a writer: find a message from that person, swipe to reply it, then type /%swriter.", verb, verb))
		return
	}

	targetUser := message.ReplyToMessage.From
	targetID := fmt.Sprintf("%d", targetUser.ID)
	targetName := targetUser.FirstName
	if targetUser.Username != "" {
		targetName = "@" + targetUser.Username
	}

	switch action {
	case "add":
		if err := c.agentStore.AddGroupFileWriter(ctx, agentID, groupID, targetID, targetUser.FirstName, targetUser.Username); err != nil {
			slog.Warn("add writer failed", "error", err, "target", targetID)
			send("Failed to add writer. Please try again.")
			return
		}
		send(fmt.Sprintf("Added %s as a file writer.", targetName))
		c.Bus().Broadcast(bus.Event{
			Name:    protocol.EventCacheInvalidate,
			Payload: bus.CacheInvalidatePayload{Kind: bus.CacheKindGroupFileWriters, Key: groupID},
		})

	case "remove":
		// Prevent removing the last writer
		writers, _ := c.agentStore.ListGroupFileWriters(ctx, agentID, groupID)
		if len(writers) <= 1 {
			send("Cannot remove the last file writer.")
			return
		}
		if err := c.agentStore.RemoveGroupFileWriter(ctx, agentID, groupID, targetID); err != nil {
			slog.Warn("remove writer failed", "error", err, "target", targetID)
			send("Failed to remove writer. Please try again.")
			return
		}
		send(fmt.Sprintf("Removed %s from file writers.", targetName))
		c.Bus().Broadcast(bus.Event{
			Name:    protocol.EventCacheInvalidate,
			Payload: bus.CacheInvalidatePayload{Kind: bus.CacheKindGroupFileWriters, Key: groupID},
		})
	}
}

// handleListWriters handles the /writers command.
func (c *Channel) handleListWriters(ctx context.Context, chatID int64, chatIDStr string, isGroup bool, setThread func(*telego.SendMessageParams)) {
	chatIDObj := tu.ID(chatID)

	send := func(text string) {
		msg := tu.Message(chatIDObj, text)
		setThread(msg)
		c.bot.SendMessage(ctx, msg)
	}

	if !isGroup {
		send("This command only works in group chats.")
		return
	}

	if c.agentStore == nil {
		send("File writer management is not available.")
		return
	}

	agentID, err := c.resolveAgentUUID(ctx)
	if err != nil {
		slog.Debug("list writers: agent resolve failed", "error", err)
		send("File writer management is not available (no agent).")
		return
	}

	groupID := fmt.Sprintf("group:%s:%s", c.Name(), chatIDStr)

	writers, err := c.agentStore.ListGroupFileWriters(ctx, agentID, groupID)
	if err != nil {
		slog.Warn("list writers failed", "error", err)
		send("Failed to list writers. Please try again.")
		return
	}

	if len(writers) == 0 {
		send("No file writers configured for this group. The first person to interact with the bot will be added automatically.")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("File writers for this group (%d):\n", len(writers)))
	for i, w := range writers {
		label := w.UserID
		if w.Username != nil && *w.Username != "" {
			label = "@" + *w.Username
		} else if w.DisplayName != nil && *w.DisplayName != "" {
			label = *w.DisplayName
		}
		sb.WriteString(fmt.Sprintf("%d. %s (ID: %s)\n", i+1, label, w.UserID))
	}
	send(sb.String())
}
