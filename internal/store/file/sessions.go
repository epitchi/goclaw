package file

import (
	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/pkg/providers"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// FileSessionStore wraps sessions.Manager to implement store.SessionStore.
type FileSessionStore struct {
	mgr *sessions.Manager
}

func NewFileSessionStore(mgr *sessions.Manager) *FileSessionStore {
	return &FileSessionStore{mgr: mgr}
}

// Manager returns the underlying sessions.Manager for direct access during migration.
func (f *FileSessionStore) Manager() *sessions.Manager { return f.mgr }

func (f *FileSessionStore) GetOrCreate(key string) *store.SessionData {
	s := f.mgr.GetOrCreate(key)
	return sessionToData(s)
}

func (f *FileSessionStore) AddMessage(key string, msg providers.Message) {
	f.mgr.AddMessage(key, msg)
}

func (f *FileSessionStore) GetHistory(key string) []providers.Message {
	return f.mgr.GetHistory(key)
}

func (f *FileSessionStore) GetSummary(key string) string {
	return f.mgr.GetSummary(key)
}

func (f *FileSessionStore) SetSummary(key, summary string) {
	f.mgr.SetSummary(key, summary)
}

func (f *FileSessionStore) SetLabel(key, label string) {
	f.mgr.SetLabel(key, label)
}

func (f *FileSessionStore) SetAgentInfo(string, uuid.UUID, string) {} // no-op for file store

func (f *FileSessionStore) UpdateMetadata(key, model, provider, channel string) {
	f.mgr.UpdateMetadata(key, model, provider, channel)
}

func (f *FileSessionStore) AccumulateTokens(key string, input, output int64) {
	f.mgr.AccumulateTokens(key, input, output)
}

func (f *FileSessionStore) IncrementCompaction(key string) {
	f.mgr.IncrementCompaction(key)
}

func (f *FileSessionStore) GetCompactionCount(key string) int {
	return f.mgr.GetCompactionCount(key)
}

func (f *FileSessionStore) GetMemoryFlushCompactionCount(key string) int {
	return f.mgr.GetMemoryFlushCompactionCount(key)
}

func (f *FileSessionStore) SetMemoryFlushDone(key string) {
	f.mgr.SetMemoryFlushDone(key)
}

func (f *FileSessionStore) SetSpawnInfo(key, spawnedBy string, depth int) {
	f.mgr.SetSpawnInfo(key, spawnedBy, depth)
}

func (f *FileSessionStore) SetContextWindow(key string, cw int) {
	f.mgr.SetContextWindow(key, cw)
}

func (f *FileSessionStore) GetContextWindow(key string) int {
	return f.mgr.GetContextWindow(key)
}

func (f *FileSessionStore) SetLastPromptTokens(key string, tokens, msgCount int) {
	f.mgr.SetLastPromptTokens(key, tokens, msgCount)
}

func (f *FileSessionStore) GetLastPromptTokens(key string) (int, int) {
	return f.mgr.GetLastPromptTokens(key)
}

func (f *FileSessionStore) TruncateHistory(key string, keepLast int) {
	f.mgr.TruncateHistory(key, keepLast)
}

func (f *FileSessionStore) Reset(key string) {
	f.mgr.Reset(key)
}

func (f *FileSessionStore) Delete(key string) error {
	return f.mgr.Delete(key)
}

func (f *FileSessionStore) List(agentID string) []store.SessionInfo {
	items := f.mgr.List(agentID)
	result := make([]store.SessionInfo, len(items))
	for i, item := range items {
		result[i] = store.SessionInfo{
			Key:          item.Key,
			MessageCount: item.MessageCount,
			Created:      item.Created,
			Updated:      item.Updated,
			Label:        item.Label,
			Channel:      item.Channel,
		}
	}
	return result
}

func (f *FileSessionStore) ListPaged(opts store.SessionListOpts) store.SessionListResult {
	all := f.List(opts.AgentID)
	total := len(all)

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	return store.SessionListResult{
		Sessions: all[start:end],
		Total:    total,
	}
}

func (f *FileSessionStore) Save(key string) error {
	return f.mgr.Save(key)
}

func (f *FileSessionStore) LastUsedChannel(agentID string) (string, string) {
	return f.mgr.LastUsedChannel(agentID)
}

func sessionToData(s *sessions.Session) *store.SessionData {
	return &store.SessionData{
		Key:                        s.Key,
		Messages:                   s.Messages,
		Summary:                    s.Summary,
		Created:                    s.Created,
		Updated:                    s.Updated,
		Model:                      s.Model,
		Provider:                   s.Provider,
		Channel:                    s.Channel,
		InputTokens:                s.InputTokens,
		OutputTokens:               s.OutputTokens,
		CompactionCount:            s.CompactionCount,
		MemoryFlushCompactionCount: s.MemoryFlushCompactionCount,
		MemoryFlushAt:              s.MemoryFlushAt,
		Label:                      s.Label,
		SpawnedBy:                  s.SpawnedBy,
		SpawnDepth:                 s.SpawnDepth,
		ContextWindow:             s.ContextWindow,
		LastPromptTokens:          s.LastPromptTokens,
		LastMessageCount:          s.LastMessageCount,
	}
}
