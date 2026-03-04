package store

// Stores is the top-level container for all storage backends.
// In standalone mode, managed-only stores (Agents, Providers, Tracing, MCP) are nil.
type Stores struct {
	Sessions  SessionStore
	Memory    MemoryStore
	Cron      CronStore
	Pairing   PairingStore
	Skills    SkillStore
	Agents    AgentStore      // nil in standalone mode
	Providers ProviderStore   // nil in standalone mode
	Tracing   TracingStore    // nil in standalone mode
	MCP              MCPServerStore       // nil in standalone mode
	CustomTools      CustomToolStore      // nil in standalone mode
	ChannelInstances ChannelInstanceStore // nil in standalone mode
	ConfigSecrets    ConfigSecretsStore   // nil in standalone mode
	AgentLinks       AgentLinkStore       // nil in standalone mode
	Teams            TeamStore            // nil in standalone mode
	BuiltinTools     BuiltinToolStore     // nil in standalone mode
}
