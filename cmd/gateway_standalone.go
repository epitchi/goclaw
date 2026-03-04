package cmd

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/epitchi/goclaw/pkg/agent"
	"github.com/epitchi/goclaw/pkg/bootstrap"
	"github.com/epitchi/goclaw/pkg/config"
	"github.com/epitchi/goclaw/pkg/store"
	"github.com/epitchi/goclaw/pkg/store/file"
	"github.com/epitchi/goclaw/pkg/tools"
)

// wireStandaloneExtras sets up standalone-mode components that mirror managed mode:
// FileAgentStore (filesystem + SQLite), ContextFileInterceptor, per-user seeding callbacks.
// Returns the agent store, interceptor callbacks, and a cleanup function.
func wireStandaloneExtras(
	cfg *config.Config,
	toolsReg *tools.Registry,
	dataDir string,
	workspace string,
) (agentStore store.AgentStore, ensureUserFiles agent.EnsureUserFilesFunc, contextFileLoader agent.ContextFileLoaderFunc, cleanup func()) {
	// Build agent entries from config
	var entries []file.AgentEntry

	// Default agent
	defaultCfg := cfg.ResolveAgent("default")
	defaultWS := config.ExpandHome(defaultCfg.Workspace)
	if !filepath.IsAbs(defaultWS) {
		defaultWS, _ = filepath.Abs(defaultWS)
	}
	entries = append(entries, file.AgentEntry{
		Key:       "default",
		AgentType: defaultCfg.AgentType,
		Workspace: defaultWS,
	})

	// Additional agents from agents.list
	for agentID := range cfg.Agents.List {
		if agentID == "default" {
			continue
		}
		agentCfg := cfg.ResolveAgent(agentID)
		ws := config.ExpandHome(agentCfg.Workspace)
		if !filepath.IsAbs(ws) {
			ws, _ = filepath.Abs(ws)
		}
		entries = append(entries, file.AgentEntry{
			Key:       agentID,
			AgentType: agentCfg.AgentType,
			Workspace: ws,
		})
	}

	// Create FileAgentStore (SQLite at {dataDir}/agents.db)
	dbPath := filepath.Join(dataDir, "agents.db")
	fileStore, err := file.NewFileAgentStore(dbPath, entries)
	if err != nil {
		slog.Error("failed to create file agent store", "error", err)
		return nil, nil, nil, nil
	}

	// Seed predefined agents' context files to disk
	bgCtx := context.Background()
	for _, e := range entries {
		if e.AgentType == store.AgentTypePredefined {
			id := file.AgentUUID(e.Key)
			if _, seedErr := bootstrap.SeedToStore(bgCtx, fileStore, id, store.AgentTypePredefined); seedErr != nil {
				slog.Warn("failed to seed predefined agent context files", "agent", e.Key, "error", seedErr)
			}
		}
	}

	// Create ContextFileInterceptor — same as managed mode, different backing store
	contextFileInterceptor := tools.NewContextFileInterceptor(fileStore, workspace)

	// Build callbacks using shared builders
	ensureUserFiles = buildEnsureUserFiles(fileStore, nil)
	contextFileLoader = buildContextFileLoader(contextFileInterceptor)

	// Wire interceptors to filesystem tools (read_file, write_file, edit)
	for _, toolName := range []string{"read_file", "write_file", "edit"} {
		if t, ok := toolsReg.Get(toolName); ok {
			if ia, ok := t.(tools.InterceptorAware); ok {
				ia.SetContextFileInterceptor(contextFileInterceptor)
			}
		}
	}

	// Deny access to hidden directories (e.g. .goclaw) from filesystem tools
	hiddenDirs := []string{".goclaw"}
	for _, toolName := range []string{"read_file", "write_file", "list_files", "edit"} {
		if t, ok := toolsReg.Get(toolName); ok {
			if pd, ok := t.(tools.PathDenyable); ok {
				pd.DenyPaths(hiddenDirs...)
			}
		}
	}

	cleanup = func() {
		fileStore.Close()
	}

	slog.Info("standalone mode: agent store + interceptors wired",
		"agents", len(entries), "db", dbPath)

	return fileStore, ensureUserFiles, contextFileLoader, cleanup
}
