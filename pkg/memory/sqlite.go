package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements chunk storage with FTS5 full-text search.
type SQLiteStore struct {
	db *sql.DB
	mu sync.RWMutex
}

// NewSQLiteStore opens (or creates) a SQLite database at the given path
// and initializes the schema with FTS5 support.
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	slog.Info("memory store opened", "path", dbPath)
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS chunks (
			id TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'memory',
			start_line INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			hash TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL,
			embedding TEXT NOT NULL DEFAULT '[]',
			updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_path ON chunks(path)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_source ON chunks(source)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_hash ON chunks(hash)`,
		// FTS5 virtual table for full-text search
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
			text,
			id UNINDEXED,
			path UNINDEXED,
			source UNINDEXED,
			start_line UNINDEXED,
			end_line UNINDEXED,
			tokenize='porter unicode61'
		)`,
		// Embedding cache for deduplication
		`CREATE TABLE IF NOT EXISTS embedding_cache (
			hash TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			embedding TEXT NOT NULL,
			dims INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		)`,
		// File metadata for change detection (matching TS files table)
		`CREATE TABLE IF NOT EXISTS files (
			path TEXT PRIMARY KEY,
			source TEXT NOT NULL DEFAULT 'memory',
			hash TEXT NOT NULL,
			mtime INTEGER NOT NULL DEFAULT 0,
			size INTEGER NOT NULL DEFAULT 0
		)`,
	}

	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:min(len(stmt), 60)], err)
		}
	}

	return nil
}

// UpsertChunk inserts or replaces a chunk and its FTS index entry.
func (s *SQLiteStore) UpsertChunk(c Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	embJSON, err := json.Marshal(c.Embedding)
	if err != nil {
		return fmt.Errorf("marshal embedding: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete old FTS entry if exists
	tx.Exec("DELETE FROM chunks_fts WHERE id = ?", c.ID)

	// Upsert chunk
	_, err = tx.Exec(`INSERT OR REPLACE INTO chunks (id, path, source, start_line, end_line, hash, model, text, embedding, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s','now'))`,
		c.ID, c.Path, c.Source, c.StartLine, c.EndLine, c.Hash, c.Model, c.Text, string(embJSON))
	if err != nil {
		return fmt.Errorf("upsert chunk: %w", err)
	}

	// Insert FTS entry
	_, err = tx.Exec(`INSERT INTO chunks_fts (text, id, path, source, start_line, end_line)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.Text, c.ID, c.Path, c.Source, c.StartLine, c.EndLine)
	if err != nil {
		return fmt.Errorf("insert fts: %w", err)
	}

	return tx.Commit()
}

// DeleteByPath removes all chunks (and FTS entries) for a given path.
func (s *SQLiteStore) DeleteByPath(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tx.Exec("DELETE FROM chunks_fts WHERE path = ?", path)
	tx.Exec("DELETE FROM chunks WHERE path = ?", path)

	return tx.Commit()
}

// SearchFTS performs a full-text search using FTS5 with BM25 ranking.
// Returns results sorted by relevance score (highest first).
func (s *SQLiteStore) SearchFTS(query string, opts SearchOptions) ([]SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}

	// Build WHERE clause for filters
	where := ""
	args := []interface{}{query}

	if opts.Source != "" {
		where += " AND source = ?"
		args = append(args, opts.Source)
	}
	if opts.PathPrefix != "" {
		where += " AND path LIKE ?"
		args = append(args, opts.PathPrefix+"%")
	}

	args = append(args, maxResults)

	// Normalize BM25 rank to [0,1] score using 1/(1+abs(rank)).
	// Matching TS bm25RankToScore() in hybrid.ts.
	sql := fmt.Sprintf(`SELECT id, path, source, start_line, end_line, text,
		1.0 / (1.0 + abs(rank)) as score
		FROM chunks_fts
		WHERE chunks_fts MATCH ?%s
		ORDER BY rank
		LIMIT ?`, where)

	rows, err := s.db.Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var id, path, source, text string
		var startLine, endLine int
		var score float64

		if err := rows.Scan(&id, &path, &source, &startLine, &endLine, &text, &score); err != nil {
			continue
		}

		results = append(results, SearchResult{
			Path:      path,
			StartLine: startLine,
			EndLine:   endLine,
			Score:     score,
			Snippet:   truncateSnippet(text, 700),
			Source:    source,
		})
	}

	return results, nil
}

// GetAllChunks returns all chunks (for in-memory vector search).
func (s *SQLiteStore) GetAllChunks() ([]Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT id, path, source, start_line, end_line, hash, model, text, embedding FROM chunks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		var embJSON string
		if err := rows.Scan(&c.ID, &c.Path, &c.Source, &c.StartLine, &c.EndLine, &c.Hash, &c.Model, &c.Text, &embJSON); err != nil {
			continue
		}
		json.Unmarshal([]byte(embJSON), &c.Embedding)
		chunks = append(chunks, c)
	}

	return chunks, nil
}

// GetChunksByPath returns all chunks for a specific file path.
func (s *SQLiteStore) GetChunksByPath(path string) ([]Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT id, path, source, start_line, end_line, hash, model, text FROM chunks WHERE path = ? ORDER BY start_line", path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.Path, &c.Source, &c.StartLine, &c.EndLine, &c.Hash, &c.Model, &c.Text); err != nil {
			continue
		}
		chunks = append(chunks, c)
	}

	return chunks, nil
}

// GetCachedEmbedding returns a cached embedding by content hash.
func (s *SQLiteStore) GetCachedEmbedding(contentHash, provider, model string) ([]float32, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var embJSON string
	err := s.db.QueryRow("SELECT embedding FROM embedding_cache WHERE hash = ? AND provider = ? AND model = ?",
		contentHash, provider, model).Scan(&embJSON)
	if err != nil {
		return nil, false
	}

	var emb []float32
	if err := json.Unmarshal([]byte(embJSON), &emb); err != nil {
		return nil, false
	}

	return emb, true
}

// CacheEmbedding stores an embedding in the cache.
func (s *SQLiteStore) CacheEmbedding(contentHash, provider, model string, embedding []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	embJSON, _ := json.Marshal(embedding)
	_, err := s.db.Exec(`INSERT OR REPLACE INTO embedding_cache (hash, provider, model, embedding, dims, updated_at)
		VALUES (?, ?, ?, ?, ?, strftime('%s','now'))`,
		contentHash, provider, model, string(embJSON), len(embedding))
	return err
}

// GetFileHash returns the stored hash for a file path, or false if not found.
func (s *SQLiteStore) GetFileHash(path string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var hash string
	err := s.db.QueryRow("SELECT hash FROM files WHERE path = ?", path).Scan(&hash)
	if err != nil {
		return "", false
	}
	return hash, true
}

// UpsertFile stores or updates file metadata for change detection.
func (s *SQLiteStore) UpsertFile(path, source, hash string, mtime, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT OR REPLACE INTO files (path, source, hash, mtime, size) VALUES (?, ?, ?, ?, ?)`,
		path, source, hash, mtime, size)
	return err
}

// DeleteFile removes file metadata.
func (s *SQLiteStore) DeleteFile(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM files WHERE path = ?", path)
	return err
}

// ChunkCount returns the number of stored chunks.
func (s *SQLiteStore) ChunkCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM chunks").Scan(&count)
	return count
}

// Close closes the SQLite database.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// ContentHash returns the SHA256 hash of text content.
func ContentHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", h[:16])
}

func truncateSnippet(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
