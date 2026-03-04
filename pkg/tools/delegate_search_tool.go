package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// DelegateSearchTool performs hybrid FTS + semantic search over delegation targets.
// Used when an agent has too many targets for static AGENTS.md (>15).
type DelegateSearchTool struct {
	linkStore   store.AgentLinkStore
	embProvider store.EmbeddingProvider // optional: enables semantic search
}

func NewDelegateSearchTool(linkStore store.AgentLinkStore, embProvider store.EmbeddingProvider) *DelegateSearchTool {
	return &DelegateSearchTool{linkStore: linkStore, embProvider: embProvider}
}

func (t *DelegateSearchTool) Name() string { return "delegate_search" }

func (t *DelegateSearchTool) Description() string {
	return "Search for available delegation target agents by keyword or description. Use this to find the right agent to delegate a task to."
}

func (t *DelegateSearchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Search keywords to find relevant agents",
			},
			"max_results": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results (default 5)",
			},
		},
		"required": []string{"query"},
	}
}

func (t *DelegateSearchTool) Execute(ctx context.Context, args map[string]interface{}) *Result {
	query, _ := args["query"].(string)
	if query == "" {
		return ErrorResult("query parameter is required")
	}

	maxResults := 5
	if mr, ok := args["max_results"].(float64); ok && int(mr) > 0 {
		maxResults = int(mr)
	}

	sourceAgentID := store.AgentIDFromContext(ctx)

	// FTS search (always available)
	ftsResults, err := t.linkStore.SearchDelegateTargets(ctx, sourceAgentID, query, maxResults*2)
	if err != nil {
		slog.Warn("delegate_search FTS failed", "error", err)
	}

	// If embedding provider available, run hybrid search
	var results []store.AgentLinkData
	if t.embProvider != nil {
		results = t.hybridSearch(ctx, sourceAgentID, query, ftsResults, maxResults)
	} else {
		if len(ftsResults) > maxResults {
			ftsResults = ftsResults[:maxResults]
		}
		results = ftsResults
	}

	slog.Info("delegate_search", "query", query, "results", len(results), "hybrid", t.embProvider != nil)

	if len(results) == 0 {
		return NewResult(fmt.Sprintf("No delegation target agents found matching: %s", query))
	}

	type searchResult struct {
		AgentKey    string `json:"agent_key"`
		DisplayName string `json:"display_name,omitempty"`
		Frontmatter string `json:"frontmatter,omitempty"`
	}
	var out []searchResult
	for _, r := range results {
		out = append(out, searchResult{
			AgentKey:    r.TargetAgentKey,
			DisplayName: r.TargetDisplayName,
			Frontmatter: r.TargetDescription,
		})
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"agents": out,
		"count":  len(out),
	}, "", "  ")

	return NewResult(string(data) +
		"\n\nUse `spawn(agent=\"<agent_key>\", task=\"your task\")` to delegate to one of these agents.")
}

// hybridSearch merges FTS and embedding results with weighted scoring.
// BM25 weight 0.3, vector weight 0.7 (same as skill_search.go).
func (t *DelegateSearchTool) hybridSearch(ctx context.Context, sourceAgentID uuid.UUID, query string, ftsResults []store.AgentLinkData, maxResults int) []store.AgentLinkData {
	// Generate query embedding
	embeddings, err := t.embProvider.Embed(ctx, []string{query})
	if err != nil || len(embeddings) == 0 || len(embeddings[0]) == 0 {
		slog.Warn("delegate_search embedding failed, falling back to FTS", "error", err)
		if len(ftsResults) > maxResults {
			ftsResults = ftsResults[:maxResults]
		}
		return ftsResults
	}

	// Vector search
	vecResults, err := t.linkStore.SearchDelegateTargetsByEmbedding(ctx, sourceAgentID, embeddings[0], maxResults*2)
	if err != nil {
		slog.Warn("delegate_search vector search failed, falling back to FTS", "error", err)
		if len(ftsResults) > maxResults {
			ftsResults = ftsResults[:maxResults]
		}
		return ftsResults
	}

	// Merge: normalize weights when one channel has no results
	textW, vecW := 0.3, 0.7
	if len(ftsResults) == 0 && len(vecResults) > 0 {
		textW, vecW = 0, 1.0
	} else if len(vecResults) == 0 && len(ftsResults) > 0 {
		textW, vecW = 1.0, 0
	}

	// Deduplicate by agent key, accumulate scores
	type merged struct {
		link  store.AgentLinkData
		score float64
	}
	seen := make(map[string]*merged)

	for i, r := range ftsResults {
		// Simple position-based score for FTS (no ts_rank exposed in AgentLinkData)
		normalizedScore := 1.0 - float64(i)/float64(len(ftsResults)+1)
		if existing, ok := seen[r.TargetAgentKey]; ok {
			existing.score += normalizedScore * textW
		} else {
			seen[r.TargetAgentKey] = &merged{link: r, score: normalizedScore * textW}
		}
	}

	for i, r := range vecResults {
		normalizedScore := 1.0 - float64(i)/float64(len(vecResults)+1)
		if existing, ok := seen[r.TargetAgentKey]; ok {
			existing.score += normalizedScore * vecW
		} else {
			seen[r.TargetAgentKey] = &merged{link: r, score: normalizedScore * vecW}
		}
	}

	// Collect and sort by score descending
	results := make([]store.AgentLinkData, 0, len(seen))
	scores := make(map[string]float64)
	for key, m := range seen {
		results = append(results, m.link)
		scores[key] = m.score
	}
	sort.Slice(results, func(i, j int) bool {
		return scores[results[i].TargetAgentKey] > scores[results[j].TargetAgentKey]
	})

	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return results
}
