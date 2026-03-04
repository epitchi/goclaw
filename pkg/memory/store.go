// Package memory provides the memory system for GoClaw, supporting
// full-text search (FTS5) and vector-based semantic search over
// agent memory files (MEMORY.md, memory/*.md) and session transcripts.
package memory

// Chunk is a text fragment stored in the memory database.
type Chunk struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Source    string    `json:"source"` // "memory" or "sessions"
	StartLine int       `json:"start_line"`
	EndLine   int       `json:"end_line"`
	Hash      string    `json:"hash"`
	Model     string    `json:"model"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding,omitempty"`
}

// SearchResult is a single result from a memory search.
type SearchResult struct {
	Path      string  `json:"path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet"`
	Source    string  `json:"source"`
}

// SearchOptions configures a search query.
type SearchOptions struct {
	Query       string  // search query text
	MaxResults  int     // top-K results
	MinScore    float64 // minimum relevance score (0-1)
	Source      string  // filter by source ("memory", "sessions", "")
	PathPrefix  string  // filter by path prefix
}
