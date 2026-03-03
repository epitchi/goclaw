package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nextlevelbuilder/goclaw/pkg/memory"
	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// FileMemoryStore wraps memory.Manager to implement store.MemoryStore.
// In standalone mode, agentID and userID params are ignored.
type FileMemoryStore struct {
	mgr *memory.Manager
}

func NewFileMemoryStore(mgr *memory.Manager) *FileMemoryStore {
	return &FileMemoryStore{mgr: mgr}
}

// Manager returns the underlying memory.Manager for direct access during migration.
func (f *FileMemoryStore) Manager() *memory.Manager { return f.mgr }

func (f *FileMemoryStore) GetDocument(_ context.Context, _, _ string, path string) (string, error) {
	return f.mgr.GetFile(path, 0, 0)
}

func (f *FileMemoryStore) PutDocument(_ context.Context, _, _ string, path, content string) error {
	absPath := path
	if !filepath.IsAbs(path) {
		// GetFile uses MemoryDir internally; we need to resolve similarly
		return fmt.Errorf("PutDocument requires absolute path in standalone mode")
	}
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	return os.WriteFile(absPath, []byte(content), 0644)
}

func (f *FileMemoryStore) DeleteDocument(_ context.Context, _, _ string, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("DeleteDocument requires absolute path in standalone mode")
	}
	return os.Remove(path)
}

func (f *FileMemoryStore) ListDocuments(_ context.Context, _, _ string) ([]store.DocumentInfo, error) {
	// Standalone mode: not implemented (memory files are discovered via filesystem)
	return nil, nil
}

func (f *FileMemoryStore) Search(ctx context.Context, query string, _, _ string, opts store.MemorySearchOptions) ([]store.MemorySearchResult, error) {
	memOpts := memory.SearchOptions{
		Query:      query,
		MaxResults: opts.MaxResults,
		MinScore:   opts.MinScore,
		Source:     opts.Source,
		PathPrefix: opts.PathPrefix,
	}
	results, err := f.mgr.Search(ctx, query, memOpts)
	if err != nil {
		return nil, err
	}
	out := make([]store.MemorySearchResult, len(results))
	for i, r := range results {
		out[i] = store.MemorySearchResult{
			Path:      r.Path,
			StartLine: r.StartLine,
			EndLine:   r.EndLine,
			Score:     r.Score,
			Snippet:   r.Snippet,
			Source:    r.Source,
		}
	}
	return out, nil
}

func (f *FileMemoryStore) IndexDocument(ctx context.Context, _, _ string, path string) error {
	return f.mgr.IndexFile(ctx, path)
}

func (f *FileMemoryStore) IndexAll(ctx context.Context, _, _ string) error {
	return f.mgr.IndexAll(ctx)
}

func (f *FileMemoryStore) SetEmbeddingProvider(provider store.EmbeddingProvider) {
	f.mgr.SetEmbeddingProvider(provider)
}

func (f *FileMemoryStore) Close() error {
	return f.mgr.Close()
}
