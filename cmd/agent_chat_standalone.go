package cmd

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/pkg/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bootstrap"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/pkg/providers"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/store/file"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

func runStandaloneMode(cfg *config.Config, agentName, message, sessionKey string) {
	loop, sessStore, agentCfg := bootstrapStandaloneAgent(cfg, agentName)

	chatFn := func(msg string) (string, error) {
		runID := fmt.Sprintf("cli-%s", uuid.NewString()[:8])
		result, err := loop.Run(context.Background(), agent.RunRequest{
			SessionKey: sessionKey,
			Message:    msg,
			Channel:    "cli",
			ChatID:     "local",
			PeerKind:   "direct",
			RunID:      runID,
		})
		if err != nil {
			return "", err
		}
		return result.Content, nil
	}

	_ = sessStore // keep reference for session persistence

	if message != "" {
		resp, err := chatFn(message)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(resp)
		return
	}

	// Interactive REPL
	fmt.Fprintf(os.Stderr, "\nGoClaw Interactive Chat — Standalone Mode\n")
	fmt.Fprintf(os.Stderr, "Agent: %s | Model: %s\n", agentName, agentCfg.Model)
	fmt.Fprintf(os.Stderr, "Session: %s\n", sessionKey)
	fmt.Fprintf(os.Stderr, "Type \"exit\" to quit, \"/new\" for new session\n\n")

	// Handle Ctrl+C gracefully
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "\nGoodbye!")
			return
		default:
		}

		fmt.Fprint(os.Stderr, "You: ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			fmt.Fprintln(os.Stderr, "Goodbye!")
			return
		}
		if input == "/new" {
			sessionKey = sessions.BuildSessionKey(agentName, "cli", sessions.PeerDirect, uuid.NewString()[:8])
			fmt.Fprintf(os.Stderr, "New session: %s\n\n", sessionKey)
			continue
		}

		resp, err := chatFn(input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
			continue
		}
		fmt.Printf("\n%s\n\n", resp)
	}
}

// bootstrapStandaloneAgent creates a minimal agent loop for CLI usage.
func bootstrapStandaloneAgent(cfg *config.Config, agentName string) (*agent.Loop, store.SessionStore, config.AgentDefaults) {
	agentCfg := cfg.ResolveAgent(agentName)
	workspace := config.ExpandHome(agentCfg.Workspace)
	if !filepath.IsAbs(workspace) {
		workspace, _ = filepath.Abs(workspace)
	}

	// Ensure workspace exists
	os.MkdirAll(workspace, 0755)

	// 1. Provider
	providerReg := providers.NewRegistry()
	registerProviders(providerReg, cfg)

	provider, err := providerReg.Get(agentCfg.Provider)
	if err != nil {
		names := providerReg.List()
		if len(names) == 0 {
			fmt.Fprintf(os.Stderr, "Error: no providers configured. Run 'goclaw onboard' first.\n")
			os.Exit(1)
		}
		provider, _ = providerReg.Get(names[0])
		slog.Warn("configured provider not found, using fallback", "wanted", agentCfg.Provider, "using", names[0])
	}

	// 2. Sessions (wrap file-based manager in store adapter)
	sessStorage := config.ExpandHome(cfg.Sessions.Storage)
	sessStore := file.NewFileSessionStore(sessions.NewManager(sessStorage))

	// 3. Tools
	toolsReg := tools.NewRegistry()
	toolsReg.Register(tools.NewReadFileTool(workspace, agentCfg.RestrictToWorkspace))
	toolsReg.Register(tools.NewWriteFileTool(workspace, agentCfg.RestrictToWorkspace))
	toolsReg.Register(tools.NewListFilesTool(workspace, agentCfg.RestrictToWorkspace))
	toolsReg.Register(tools.NewExecTool(workspace, agentCfg.RestrictToWorkspace))

	// Web tools
	webSearchTool := tools.NewWebSearchTool(tools.WebSearchConfig{
		BraveEnabled: cfg.Tools.Web.Brave.Enabled,
		BraveAPIKey:  cfg.Tools.Web.Brave.APIKey,
		DDGEnabled:   cfg.Tools.Web.DuckDuckGo.Enabled,
	})
	if webSearchTool != nil {
		toolsReg.Register(webSearchTool)
	}
	toolsReg.Register(tools.NewWebFetchTool(tools.WebFetchConfig{}))

	// 4. Bootstrap files
	rawFiles := bootstrap.LoadWorkspaceFiles(workspace)
	truncCfg := bootstrap.TruncateConfig{
		MaxCharsPerFile: agentCfg.BootstrapMaxChars,
		TotalMaxChars:   agentCfg.BootstrapTotalMaxChars,
	}
	if truncCfg.MaxCharsPerFile <= 0 {
		truncCfg.MaxCharsPerFile = bootstrap.DefaultMaxCharsPerFile
	}
	if truncCfg.TotalMaxChars <= 0 {
		truncCfg.TotalMaxChars = bootstrap.DefaultTotalMaxChars
	}
	contextFiles := bootstrap.BuildContextFiles(rawFiles, truncCfg)

	// 5. Skills
	globalSkillsDir := filepath.Join(config.ExpandHome("~/.goclaw"), "skills")
	skillsLoader := skills.NewLoader(workspace, globalSkillsDir, "")
	toolsReg.Register(tools.NewSkillSearchTool(skillsLoader))

	// Allow read_file to access skills directories
	if readTool, ok := toolsReg.Get("read_file"); ok {
		if rt, ok := readTool.(*tools.ReadFileTool); ok {
			rt.AllowPaths(globalSkillsDir)
			if homeDir, err := os.UserHomeDir(); err == nil {
				rt.AllowPaths(filepath.Join(homeDir, ".agents", "skills"))
			}
		}
	}

	// 6. Event display (tool calls on stderr)
	var eventMu sync.Mutex
	onEvent := func(evt agent.AgentEvent) {
		eventMu.Lock()
		defer eventMu.Unlock()

		switch evt.Type {
		case protocol.AgentEventToolCall:
			if p, ok := evt.Payload.(map[string]interface{}); ok {
				name, _ := p["name"].(string)
				fmt.Fprintf(os.Stderr, "  [tool] %s\n", name)
			}
		case protocol.AgentEventToolResult:
			// silent — avoid noisy output
		}
	}

	// Per-agent skill allowlist
	var skillAllowList []string
	if spec, ok := cfg.Agents.List[agentName]; ok {
		skillAllowList = spec.Skills
	}

	// 7. Create agent loop
	msgBus := bus.New()
	loop := agent.NewLoop(agent.LoopConfig{
		ID:            agentName,
		Provider:      provider,
		Model:         agentCfg.Model,
		ContextWindow: agentCfg.ContextWindow,
		MaxIterations: agentCfg.MaxToolIterations,
		Workspace:     workspace,
		Bus:           msgBus,
		Sessions:      sessStore,
		Tools:         toolsReg,
		OnEvent:       onEvent,
		OwnerIDs:      cfg.Gateway.OwnerIDs,
		SkillsLoader:  skillsLoader,
		SkillAllowList: skillAllowList,
		HasMemory:     false, // skip memory for standalone CLI (avoids SQLite dep issues)
		ContextFiles:  contextFiles,
		CompactionCfg:     agentCfg.Compaction,
		ContextPruningCfg: agentCfg.ContextPruning,
	})

	return loop, sessStore, agentCfg
}
