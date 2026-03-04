package tools

import (
	"context"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/epitchi/goclaw/pkg/store"
)

// DynamicToolLoader loads custom tools from the database and registers them into tool registries.
type DynamicToolLoader struct {
	store     store.CustomToolStore
	workspace string
	mu        sync.Mutex
	// Track names of globally registered custom tools (for reload/unregister).
	globalNames map[string]bool
}

// NewDynamicToolLoader creates a loader for custom tools from the database.
func NewDynamicToolLoader(s store.CustomToolStore, workspace string) *DynamicToolLoader {
	return &DynamicToolLoader{
		store:       s,
		workspace:   workspace,
		globalNames: make(map[string]bool),
	}
}

// LoadGlobal loads all global custom tools (agent_id IS NULL) and registers them into the registry.
// Skips tools whose names collide with existing built-in or MCP tools.
func (l *DynamicToolLoader) LoadGlobal(ctx context.Context, reg *Registry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	defs, err := l.store.ListGlobal(ctx)
	if err != nil {
		return err
	}

	registered := 0
	for _, def := range defs {
		if _, exists := reg.Get(def.Name); exists {
			slog.Warn("custom_tools: skipping global tool (name collision with built-in/MCP)",
				"tool", def.Name)
			continue
		}
		reg.Register(NewDynamicTool(def, l.workspace))
		l.globalNames[def.Name] = true
		registered++
	}

	if registered > 0 {
		slog.Info("custom_tools: loaded global tools", "count", registered)
	}
	return nil
}

// LoadForAgent loads per-agent custom tools and returns a cloned registry with them added.
// Returns nil if the agent has no custom tools (caller should use global registry).
func (l *DynamicToolLoader) LoadForAgent(ctx context.Context, globalReg *Registry, agentID uuid.UUID) (*Registry, error) {
	defs, err := l.store.ListByAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, nil // no per-agent tools
	}

	clone := globalReg.Clone()
	for _, def := range defs {
		clone.Register(NewDynamicTool(def, l.workspace))
	}

	slog.Debug("custom_tools: loaded per-agent tools", "agent_id", agentID, "count", len(defs))
	return clone, nil
}

// ReloadGlobal unregisters all previously loaded global tools and re-loads from DB.
// Used on cache invalidation events.
func (l *DynamicToolLoader) ReloadGlobal(ctx context.Context, reg *Registry) {
	l.mu.Lock()
	// Unregister old global custom tools
	for name := range l.globalNames {
		reg.Unregister(name)
	}
	l.globalNames = make(map[string]bool)
	l.mu.Unlock()

	// Re-load (LoadGlobal acquires its own lock)
	if err := l.LoadGlobal(ctx, reg); err != nil {
		slog.Warn("custom_tools: failed to reload global tools", "error", err)
	}
}
