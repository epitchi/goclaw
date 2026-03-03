package memory

import (
	"context"
	"sort"
)

// HybridSearchConfig controls the hybrid search algorithm.
type HybridSearchConfig struct {
	VectorWeight float64 // alpha: weight for vector score (default 0.7)
	TextWeight   float64 // 1-alpha: weight for FTS score (default 0.3)
}

// DefaultHybridConfig returns the default hybrid search weights.
func DefaultHybridConfig() HybridSearchConfig {
	return HybridSearchConfig{
		VectorWeight: 0.7,
		TextWeight:   0.3,
	}
}

// HybridSearch combines vector similarity and FTS results.
// If no embedding provider is configured, falls back to FTS-only.
func HybridSearch(
	ctx context.Context,
	store *SQLiteStore,
	provider EmbeddingProvider,
	query string,
	opts SearchOptions,
	cfg HybridSearchConfig,
) ([]SearchResult, error) {
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 6
	}

	// FTS search
	ftsResults, ftsErr := store.SearchFTS(query, opts)

	// Vector search (if provider available)
	var vecResults []SearchResult
	if provider != nil {
		var err error
		vecResults, err = vectorSearch(ctx, store, provider, query, opts)
		if err != nil {
			// Vector search failed — fall back to FTS-only
			if ftsErr != nil {
				return nil, ftsErr
			}
			if len(ftsResults) > maxResults {
				ftsResults = ftsResults[:maxResults]
			}
			return ftsResults, nil
		}
	}

	// If no vector results, return FTS results directly
	if len(vecResults) == 0 {
		if ftsErr != nil {
			return nil, ftsErr
		}
		if len(ftsResults) > maxResults {
			ftsResults = ftsResults[:maxResults]
		}
		return ftsResults, nil
	}

	// If no FTS results, return vector results directly
	if len(ftsResults) == 0 || ftsErr != nil {
		if len(vecResults) > maxResults {
			vecResults = vecResults[:maxResults]
		}
		return vecResults, nil
	}

	// Merge: normalize scores and combine
	merged := mergeResults(ftsResults, vecResults, cfg)

	// Filter by min score
	if opts.MinScore > 0 {
		filtered := merged[:0]
		for _, r := range merged {
			if r.Score >= opts.MinScore {
				filtered = append(filtered, r)
			}
		}
		merged = filtered
	}

	if len(merged) > maxResults {
		merged = merged[:maxResults]
	}

	return merged, nil
}

// vectorSearch performs in-memory cosine similarity search.
func vectorSearch(
	ctx context.Context,
	store *SQLiteStore,
	provider EmbeddingProvider,
	query string,
	opts SearchOptions,
) ([]SearchResult, error) {
	// Embed the query
	embeddings, err := provider.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(embeddings) == 0 {
		return nil, nil
	}

	queryVec := embeddings[0]

	// Load all chunks with embeddings
	chunks, err := store.GetAllChunks()
	if err != nil {
		return nil, err
	}

	// Score each chunk
	type scored struct {
		chunk Chunk
		score float64
	}

	var results []scored
	for _, c := range chunks {
		if len(c.Embedding) == 0 {
			continue
		}

		// Apply filters
		if opts.Source != "" && c.Source != opts.Source {
			continue
		}
		if opts.PathPrefix != "" && len(c.Path) >= len(opts.PathPrefix) && c.Path[:len(opts.PathPrefix)] != opts.PathPrefix {
			continue
		}

		sim := CosineSimilarity(queryVec, c.Embedding)
		if sim > 0 {
			results = append(results, scored{chunk: c, score: sim})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	limit := opts.MaxResults
	if limit <= 0 {
		limit = 10
	}
	if len(results) > limit {
		results = results[:limit]
	}

	searchResults := make([]SearchResult, len(results))
	for i, r := range results {
		searchResults[i] = SearchResult{
			Path:      r.chunk.Path,
			StartLine: r.chunk.StartLine,
			EndLine:   r.chunk.EndLine,
			Score:     r.score,
			Snippet:   truncateSnippet(r.chunk.Text, 700),
			Source:    r.chunk.Source,
		}
	}

	return searchResults, nil
}

// mergeResults combines FTS and vector results using weighted scoring.
func mergeResults(fts, vec []SearchResult, cfg HybridSearchConfig) []SearchResult {
	// Normalize FTS scores: BM25 scores vary widely, map to 0-1
	if len(fts) > 0 {
		maxScore := fts[0].Score
		if maxScore > 0 {
			for i := range fts {
				fts[i].Score = fts[i].Score / maxScore
			}
		}
	}

	// Build result map keyed by (path, startLine)
	type key struct {
		path      string
		startLine int
	}

	merged := make(map[key]*SearchResult)

	for _, r := range vec {
		k := key{r.Path, r.StartLine}
		merged[k] = &SearchResult{
			Path:      r.Path,
			StartLine: r.StartLine,
			EndLine:   r.EndLine,
			Score:     r.Score * cfg.VectorWeight,
			Snippet:   r.Snippet,
			Source:    r.Source,
		}
	}

	for _, r := range fts {
		k := key{r.Path, r.StartLine}
		if existing, ok := merged[k]; ok {
			existing.Score += r.Score * cfg.TextWeight
		} else {
			merged[k] = &SearchResult{
				Path:      r.Path,
				StartLine: r.StartLine,
				EndLine:   r.EndLine,
				Score:     r.Score * cfg.TextWeight,
				Snippet:   r.Snippet,
				Source:    r.Source,
			}
		}
	}

	results := make([]SearchResult, 0, len(merged))
	for _, r := range merged {
		results = append(results, *r)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}
