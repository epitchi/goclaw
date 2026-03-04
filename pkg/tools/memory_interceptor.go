package tools

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/epitchi/goclaw/pkg/bootstrap"
	"github.com/epitchi/goclaw/pkg/store"
)

// isMemoryPath checks if a path refers to a memory file (MEMORY.md, memory.md, memory/*).
// Handles both relative and absolute paths (when workspace is provided).
func isMemoryPath(path, workspace string) bool {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)

	// Root-level MEMORY.md or memory.md
	dir := filepath.Dir(clean)
	if (dir == "." || dir == "/" || dir == "") && (base == bootstrap.MemoryFile || base == bootstrap.MemoryAltFile) {
		return true
	}

	// Anything under memory/ directory (relative)
	if strings.HasPrefix(clean, "memory/") || strings.HasPrefix(clean, "memory\\") {
		return true
	}

	// Absolute path at workspace root or under workspace/memory/
	if workspace != "" && filepath.IsAbs(clean) {
		cleanWS := filepath.Clean(workspace)
		if filepath.Dir(clean) == cleanWS && (base == bootstrap.MemoryFile || base == bootstrap.MemoryAltFile) {
			return true
		}
		memDir := filepath.Join(cleanWS, "memory")
		if strings.HasPrefix(clean, memDir+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// MemoryInterceptor routes memory file reads/writes to the MemoryStore.
// Used in managed mode to keep MEMORY.md and memory/* in Postgres.
type MemoryInterceptor struct {
	memStore  store.MemoryStore
	workspace string
}

// NewMemoryInterceptor creates an interceptor backed by the given memory store.
func NewMemoryInterceptor(ms store.MemoryStore, workspace string) *MemoryInterceptor {
	return &MemoryInterceptor{memStore: ms, workspace: workspace}
}

// ReadFile attempts to read a memory file from the DB.
// Returns (content, true, nil) if handled, or ("", false, nil) if not a memory path.
func (m *MemoryInterceptor) ReadFile(ctx context.Context, path string) (string, bool, error) {
	if !isMemoryPath(path, m.workspace) {
		return "", false, nil
	}

	agentID := store.AgentIDFromContext(ctx)
	if agentID == uuid.Nil {
		return "", false, nil // not in managed mode context
	}

	// Normalize absolute path to workspace-relative for DB storage
	relPath := normalizeToRelative(path, m.workspace)

	userID := store.UserIDFromContext(ctx)
	agentStr := agentID.String()

	// Try per-user first, then global
	content, err := m.memStore.GetDocument(ctx, agentStr, userID, relPath)
	if err != nil && userID != "" {
		content, err = m.memStore.GetDocument(ctx, agentStr, "", relPath)
	}
	if err != nil {
		// Not found is OK — return empty
		slog.Debug("memory interceptor: document not found", "path", path, "agent", agentStr)
		return "", true, nil
	}

	return content, true, nil
}

// WriteFile attempts to write a memory file to the DB (+ re-index chunks for .md files).
// Non-.md files (e.g. heartbeat-state.json) are stored but NOT indexed/chunked/embedded,
// matching TS behavior where only .md files are indexed.
// Returns (true, nil) if handled, or (false, nil) if not a memory path.
func (m *MemoryInterceptor) WriteFile(ctx context.Context, path, content string) (bool, error) {
	if !isMemoryPath(path, m.workspace) {
		return false, nil
	}

	agentID := store.AgentIDFromContext(ctx)
	if agentID == uuid.Nil {
		return false, nil // not in managed mode context
	}

	// Normalize absolute path to workspace-relative for DB storage
	relPath := normalizeToRelative(path, m.workspace)

	userID := store.UserIDFromContext(ctx)
	agentStr := agentID.String()

	// Write document to DB
	if err := m.memStore.PutDocument(ctx, agentStr, userID, relPath, content); err != nil {
		return true, err
	}

	// Only index .md files (chunk + embed). Non-.md files (JSON, etc.) are stored
	// as key-value documents but not searchable via memory_search.
	if strings.HasSuffix(relPath, ".md") {
		if err := m.memStore.IndexDocument(ctx, agentStr, userID, relPath); err != nil {
			slog.Warn("memory interceptor: index failed after write", "path", path, "error", err)
			// Non-fatal: document was saved, indexing will catch up
		}
	}

	return true, nil
}
