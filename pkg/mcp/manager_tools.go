package mcp

import (
	"log/slog"

	"github.com/nextlevelbuilder/goclaw/pkg/tools"
)

// ToolNames returns all registered MCP tool names.
func (m *Manager) ToolNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var names []string
	for _, ss := range m.servers {
		names = append(names, ss.toolNames...)
	}
	return names
}

// updateMCPGroup rebuilds the "mcp" group with all MCP tool names across servers.
// Must be called with m.mu NOT held (it acquires RLock).
func (m *Manager) updateMCPGroup() {
	allNames := m.ToolNames()
	if len(allNames) > 0 {
		tools.RegisterToolGroup("mcp", allNames)
	} else {
		tools.UnregisterToolGroup("mcp")
	}
}

// unregisterAllTools removes all MCP tools from the registry.
func (m *Manager) unregisterAllTools() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, ss := range m.servers {
		if ss.cancel != nil {
			ss.cancel()
		}
		if ss.client != nil {
			_ = ss.client.Close()
		}
		for _, toolName := range ss.toolNames {
			m.registry.Unregister(toolName)
		}
		tools.UnregisterToolGroup("mcp:" + name)
		slog.Debug("mcp.server.unregistered", "server", name, "tools", len(ss.toolNames))
	}
	m.servers = make(map[string]*serverState)
	tools.UnregisterToolGroup("mcp")
}

// filterTools removes tools from the registry that don't match the allow/deny lists.
func (m *Manager) filterTools(serverName string, allow, deny []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ss, ok := m.servers[serverName]
	if !ok {
		return
	}

	allowSet := toSet(allow)
	denySet := toSet(deny)

	var kept []string
	for _, toolName := range ss.toolNames {
		bt, ok := m.registry.Get(toolName)
		if !ok {
			continue
		}
		bridge, ok := bt.(*BridgeTool)
		if !ok {
			kept = append(kept, toolName)
			continue
		}
		origName := bridge.OriginalName()

		// Deny takes priority
		if _, denied := denySet[origName]; denied {
			m.registry.Unregister(toolName)
			continue
		}

		// If allow list is set, only keep tools in the allow list
		if len(allowSet) > 0 {
			if _, allowed := allowSet[origName]; !allowed {
				m.registry.Unregister(toolName)
				continue
			}
		}

		kept = append(kept, toolName)
	}
	ss.toolNames = kept
}
