package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const groupWriterCacheTTL = 5 * time.Minute

type gwCacheEntry struct {
	writers  []GroupFileWriterData
	cachedAt time.Time
}

// GroupWriterCache wraps AgentStore.ListGroupFileWriters with a sync.Map TTL cache.
// Used by tools and agent loop to check group write permissions without repeated DB queries.
type GroupWriterCache struct {
	agentStore AgentStore
	cache      sync.Map // "agentUUID:groupID" → *gwCacheEntry
}

// NewGroupWriterCache creates a new cache backed by the given agent store.
func NewGroupWriterCache(as AgentStore) *GroupWriterCache {
	return &GroupWriterCache{agentStore: as}
}

func (c *GroupWriterCache) cacheKey(agentID uuid.UUID, groupID string) string {
	return agentID.String() + ":" + groupID
}

// ListWriters returns cached writers, falling back to DB on miss/expiry.
func (c *GroupWriterCache) ListWriters(ctx context.Context, agentID uuid.UUID, groupID string) ([]GroupFileWriterData, error) {
	key := c.cacheKey(agentID, groupID)
	if entry, ok := c.cache.Load(key); ok {
		ce := entry.(*gwCacheEntry)
		if time.Since(ce.cachedAt) < groupWriterCacheTTL {
			return ce.writers, nil
		}
		c.cache.Delete(key)
	}
	writers, err := c.agentStore.ListGroupFileWriters(ctx, agentID, groupID)
	if err != nil {
		return nil, err
	}
	c.cache.Store(key, &gwCacheEntry{writers: writers, cachedAt: time.Now()})
	return writers, nil
}

// IsWriter checks if senderNumericID is in the cached writer list.
func (c *GroupWriterCache) IsWriter(ctx context.Context, agentID uuid.UUID, groupID, senderNumericID string) (bool, error) {
	writers, err := c.ListWriters(ctx, agentID, groupID)
	if err != nil {
		return false, err
	}
	for _, w := range writers {
		if w.UserID == senderNumericID {
			return true, nil
		}
	}
	return false, nil
}

// Invalidate clears cache entries matching the given groupID.
func (c *GroupWriterCache) Invalidate(groupID string) {
	c.cache.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok && strings.HasSuffix(k, ":"+groupID) {
			c.cache.Delete(key)
		}
		return true
	})
}

// InvalidateAll clears all cached entries.
func (c *GroupWriterCache) InvalidateAll() {
	c.cache = sync.Map{}
}

// CheckGroupWritePermission returns an error if the caller is in a group context
// and is not a file writer. Returns nil if write is allowed.
// Fail-open: returns nil on DB errors or missing context (cron, subagent, standalone).
func CheckGroupWritePermission(ctx context.Context, cache *GroupWriterCache) error {
	userID := UserIDFromContext(ctx)
	if !strings.HasPrefix(userID, "group:") {
		return nil // not a group context
	}
	agentID := AgentIDFromContext(ctx)
	if agentID == uuid.Nil {
		return nil // standalone mode
	}
	senderID := SenderIDFromContext(ctx)
	if senderID == "" {
		return nil // system context (cron, subagent)
	}
	numericID := strings.SplitN(senderID, "|", 2)[0]
	isWriter, err := cache.IsWriter(ctx, agentID, userID, numericID)
	if err != nil {
		return nil // fail-open
	}
	if !isWriter {
		return fmt.Errorf("permission denied: only file writers can modify files in this group. Use /addwriter to get write access")
	}
	return nil
}
