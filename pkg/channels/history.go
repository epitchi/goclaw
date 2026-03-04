// Package channels — Group pending history tracker.
// Matching TS src/auto-reply/reply/history.ts.
//
// Tracks messages in group chats when the bot is NOT mentioned (requireMention=true).
// When the bot IS mentioned, accumulated context is prepended to the user message
// so the LLM has conversational context from the group.
package channels

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// maxHistoryKeys is the max number of distinct groups/topics tracked.
// Matching TS MAX_HISTORY_KEYS = 1000.
const maxHistoryKeys = 1000

// DefaultGroupHistoryLimit is the default pending message limit per group.
// Matching TS DEFAULT_GROUP_HISTORY_LIMIT = 50.
const DefaultGroupHistoryLimit = 50

// HistoryEntry represents a single tracked group message.
type HistoryEntry struct {
	Sender    string
	Body      string
	Timestamp time.Time
	MessageID string
}

// PendingHistory tracks group messages across multiple groups.
// Thread-safe for concurrent access from message handlers.
type PendingHistory struct {
	mu      sync.Mutex
	entries map[string][]HistoryEntry // historyKey → entries
	order   []string                  // insertion order for LRU eviction
}

// NewPendingHistory creates a new pending history tracker.
func NewPendingHistory() *PendingHistory {
	return &PendingHistory{
		entries: make(map[string][]HistoryEntry),
	}
}

// Record adds a message to the pending history for a group.
// If limit ≤ 0, recording is disabled.
// Matching TS recordPendingHistoryEntryIfEnabled + appendHistoryEntry.
func (ph *PendingHistory) Record(historyKey string, entry HistoryEntry, limit int) {
	if limit <= 0 || historyKey == "" {
		return
	}

	ph.mu.Lock()
	defer ph.mu.Unlock()

	existing := ph.entries[historyKey]
	existing = append(existing, entry)

	// Trim to limit
	if len(existing) > limit {
		existing = existing[len(existing)-limit:]
	}

	ph.entries[historyKey] = existing

	// Refresh insertion order for LRU (delete + re-append)
	ph.removeFromOrder(historyKey)
	ph.order = append(ph.order, historyKey)

	// Evict oldest keys if too many groups tracked
	ph.evictOldKeys()
}

// BuildContext retrieves pending history for a group and formats it as context
// to prepend to the current message.
// Matching TS buildPendingHistoryContextFromMap + buildHistoryContextFromEntries.
func (ph *PendingHistory) BuildContext(historyKey, currentMessage string, limit int) string {
	if limit <= 0 || historyKey == "" {
		return currentMessage
	}

	ph.mu.Lock()
	entries := ph.entries[historyKey]
	// Make a copy under lock
	entriesCopy := make([]HistoryEntry, len(entries))
	copy(entriesCopy, entries)
	ph.mu.Unlock()

	if len(entriesCopy) == 0 {
		return currentMessage
	}

	var lines []string
	for _, e := range entriesCopy {
		ts := ""
		if !e.Timestamp.IsZero() {
			ts = fmt.Sprintf(" [%s]", e.Timestamp.Format("15:04"))
		}
		lines = append(lines, fmt.Sprintf("  %s%s: %s", e.Sender, ts, e.Body))
	}

	return fmt.Sprintf("[Chat messages since your last reply - for context]\n%s\n\n[Your current message]\n%s",
		strings.Join(lines, "\n"),
		currentMessage,
	)
}

// GetEntries returns a copy of pending entries for a group (for InboundHistory metadata).
func (ph *PendingHistory) GetEntries(historyKey string) []HistoryEntry {
	ph.mu.Lock()
	defer ph.mu.Unlock()

	entries := ph.entries[historyKey]
	if len(entries) == 0 {
		return nil
	}

	result := make([]HistoryEntry, len(entries))
	copy(result, entries)
	return result
}

// Clear removes all pending history for a group.
// Called after the bot replies to that group.
// Matching TS clearHistoryEntriesIfEnabled.
func (ph *PendingHistory) Clear(historyKey string) {
	if historyKey == "" {
		return
	}

	ph.mu.Lock()
	defer ph.mu.Unlock()

	delete(ph.entries, historyKey)
	ph.removeFromOrder(historyKey)
}

// removeFromOrder removes a key from the LRU order slice (caller must hold lock).
func (ph *PendingHistory) removeFromOrder(key string) {
	for i, k := range ph.order {
		if k == key {
			ph.order = append(ph.order[:i], ph.order[i+1:]...)
			return
		}
	}
}

// evictOldKeys removes the oldest groups when exceeding maxHistoryKeys (caller must hold lock).
func (ph *PendingHistory) evictOldKeys() {
	for len(ph.order) > maxHistoryKeys {
		oldest := ph.order[0]
		ph.order = ph.order[1:]
		delete(ph.entries, oldest)
	}
}
