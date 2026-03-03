package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestChunkText(t *testing.T) {
	text := `# Title

First paragraph with some content.
More content in the same paragraph.

Second paragraph here.
And a second line.

Third paragraph is short.`

	chunks := ChunkText(text, 100)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	// First chunk should start at line 1
	if chunks[0].StartLine != 1 {
		t.Errorf("first chunk start line = %d, want 1", chunks[0].StartLine)
	}

	// All chunks should have text
	for i, c := range chunks {
		if c.Text == "" {
			t.Errorf("chunk %d has empty text", i)
		}
	}
}

func TestChunkText_SingleParagraph(t *testing.T) {
	text := "Short text."
	chunks := ChunkText(text, 1000)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}

	if chunks[0].Text != "Short text." {
		t.Errorf("text = %q, want %q", chunks[0].Text, "Short text.")
	}
}

func TestSQLiteStore_CRUD(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	// Insert chunk
	chunk := Chunk{
		ID:        "test#0",
		Path:      "MEMORY.md",
		Source:    "memory",
		StartLine: 1,
		EndLine:   5,
		Hash:      ContentHash("hello world"),
		Text:      "hello world this is a test",
	}

	if err := store.UpsertChunk(chunk); err != nil {
		t.Fatalf("UpsertChunk: %v", err)
	}

	if count := store.ChunkCount(); count != 1 {
		t.Errorf("ChunkCount = %d, want 1", count)
	}

	// Get by path
	chunks, err := store.GetChunksByPath("MEMORY.md")
	if err != nil {
		t.Fatalf("GetChunksByPath: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("GetChunksByPath returned %d chunks, want 1", len(chunks))
	}
	if chunks[0].Text != "hello world this is a test" {
		t.Errorf("chunk text = %q", chunks[0].Text)
	}

	// Delete by path
	if err := store.DeleteByPath("MEMORY.md"); err != nil {
		t.Fatalf("DeleteByPath: %v", err)
	}
	if count := store.ChunkCount(); count != 0 {
		t.Errorf("after delete, ChunkCount = %d, want 0", count)
	}
}

func TestSQLiteStore_FTSSearch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	// Insert multiple chunks
	chunks := []Chunk{
		{ID: "memo#0", Path: "MEMORY.md", Source: "memory", StartLine: 1, EndLine: 3, Hash: "h1", Text: "The project uses Go for backend development with SQLite as the database"},
		{ID: "memo#1", Path: "MEMORY.md", Source: "memory", StartLine: 4, EndLine: 6, Hash: "h2", Text: "Authentication is handled via JWT tokens and API keys"},
		{ID: "notes#0", Path: "memory/notes.md", Source: "memory", StartLine: 1, EndLine: 2, Hash: "h3", Text: "Go is a compiled programming language designed at Google"},
	}

	for _, c := range chunks {
		if err := store.UpsertChunk(c); err != nil {
			t.Fatalf("UpsertChunk: %v", err)
		}
	}

	// Search for "Go"
	results, err := store.SearchFTS("Go", SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}

	if len(results) < 2 {
		t.Errorf("expected at least 2 results for 'Go', got %d", len(results))
	}

	// Search for "authentication"
	results, err = store.SearchFTS("authentication", SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 result for 'authentication', got %d", len(results))
	}

	// Search with path filter
	results, err = store.SearchFTS("Go", SearchOptions{MaxResults: 10, PathPrefix: "memory/"})
	if err != nil {
		t.Fatalf("SearchFTS with path filter: %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 result for 'Go' in memory/, got %d", len(results))
	}
}

func TestCosineSimilarity(t *testing.T) {
	// Identical vectors → 1.0
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if sim := CosineSimilarity(a, b); sim < 0.99 {
		t.Errorf("identical vectors: similarity = %f, want ~1.0", sim)
	}

	// Orthogonal vectors → 0.0
	a = []float32{1, 0}
	b = []float32{0, 1}
	if sim := CosineSimilarity(a, b); sim > 0.01 {
		t.Errorf("orthogonal vectors: similarity = %f, want ~0.0", sim)
	}

	// Opposite vectors → -1.0
	a = []float32{1, 0}
	b = []float32{-1, 0}
	if sim := CosineSimilarity(a, b); sim > -0.99 {
		t.Errorf("opposite vectors: similarity = %f, want ~-1.0", sim)
	}
}

func TestManager_IndexAndSearch(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test memory files
	memoryMD := filepath.Join(tmpDir, "MEMORY.md")
	os.WriteFile(memoryMD, []byte("# Project Notes\n\nThe project uses Go for backend.\nDatabase is SQLite.\n\n## Architecture\n\nMicroservices pattern with message bus."), 0644)

	memDir := filepath.Join(tmpDir, "memory")
	os.MkdirAll(memDir, 0755)
	os.WriteFile(filepath.Join(memDir, "config.md"), []byte("# Config\n\nConfiguration uses JSON5 format.\nSupports hot-reload via file watcher."), 0644)

	// Create manager
	cfg := DefaultManagerConfig(tmpDir)
	mgr, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	// Index all files
	ctx := context.Background()
	if err := mgr.IndexAll(ctx); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}

	if count := mgr.ChunkCount(); count == 0 {
		t.Fatal("no chunks indexed")
	}

	// Search (FTS only, no embedding provider)
	results, err := mgr.Search(ctx, "Go backend", SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) == 0 {
		t.Error("expected search results for 'Go backend'")
	}

	// Search for config (FTS porter stemmer matches "configuration" → "configur")
	results, err = mgr.Search(ctx, "configuration reload", SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) == 0 {
		t.Error("expected search results for 'configuration reload'")
	}
}

func TestManager_GetFile(t *testing.T) {
	tmpDir := t.TempDir()

	testFile := filepath.Join(tmpDir, "MEMORY.md")
	os.WriteFile(testFile, []byte("line1\nline2\nline3\nline4\nline5"), 0644)

	cfg := DefaultManagerConfig(tmpDir)
	mgr, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	// Read entire file
	text, err := mgr.GetFile("MEMORY.md", 0, 0)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if text != "line1\nline2\nline3\nline4\nline5" {
		t.Errorf("full file = %q", text)
	}

	// Read lines 2-4
	text, err = mgr.GetFile("MEMORY.md", 2, 3)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if text != "line2\nline3\nline4" {
		t.Errorf("lines 2-4 = %q", text)
	}
}

func TestEmbeddingCache(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	emb := []float32{0.1, 0.2, 0.3}
	hash := ContentHash("test text")

	// Cache miss
	if _, ok := store.GetCachedEmbedding(hash, "openai", "text-embedding-3-small"); ok {
		t.Error("expected cache miss")
	}

	// Cache write
	if err := store.CacheEmbedding(hash, "openai", "text-embedding-3-small", emb); err != nil {
		t.Fatalf("CacheEmbedding: %v", err)
	}

	// Cache hit
	cached, ok := store.GetCachedEmbedding(hash, "openai", "text-embedding-3-small")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(cached) != 3 || cached[0] != 0.1 {
		t.Errorf("cached embedding = %v", cached)
	}
}
