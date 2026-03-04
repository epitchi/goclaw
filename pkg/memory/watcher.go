package memory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/epitchi/goclaw/pkg/bootstrap"
)

// watchDebounce is the delay before re-indexing after file changes.
// Matches TS awaitWriteFinish.stabilityThreshold (1500ms).
const watchDebounce = 1500 * time.Millisecond

// ignoredDirs are directory names that should never be watched.
// Matches TS chokidar ignored list.
var ignoredDirs = map[string]bool{
	".git":        true,
	"node_modules": true,
	".pnpm-store": true,
	".venv":       true,
	"venv":        true,
	".tox":        true,
	"__pycache__":  true,
}

// Watcher monitors memory files for changes and re-indexes them.
// Uses fsnotify + debounce pattern matching TS scheduleWatchSync().
type Watcher struct {
	manager  *Manager
	fsw      *fsnotify.Watcher
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// debounce state
	mu       sync.Mutex
	timer    *time.Timer
	pending  map[string]fsnotify.Op // path → last op
}

// newWatcher creates a watcher for the given manager.
func newWatcher(mgr *Manager) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &Watcher{
		manager: mgr,
		fsw:     fsw,
		pending: make(map[string]fsnotify.Op),
	}, nil
}

// start begins watching for file changes. Call stop() to clean up.
func (w *Watcher) start(ctx context.Context) error {
	memDir := w.manager.config.MemoryDir

	// Watch the root directory (for MEMORY.md)
	if err := w.fsw.Add(memDir); err != nil {
		slog.Warn("watcher: cannot watch memory dir", "path", memDir, "error", err)
	}

	// Watch memory/ subdirectory if it exists
	memSubDir := filepath.Join(memDir, "memory")
	if info, err := os.Stat(memSubDir); err == nil && info.IsDir() {
		if err := w.addDirRecursive(memSubDir); err != nil {
			slog.Warn("watcher: cannot watch memory subdir", "path", memSubDir, "error", err)
		}
	}

	ctx, w.cancel = context.WithCancel(ctx)
	w.wg.Add(1)
	go w.loop(ctx)

	slog.Info("memory file watcher started", "dir", memDir)
	return nil
}

// stop shuts down the watcher.
func (w *Watcher) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	w.fsw.Close()

	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
}

// addDirRecursive adds a directory and all its subdirectories to the watcher,
// skipping ignored directories.
func (w *Watcher) addDirRecursive(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if ignoredDirs[info.Name()] {
			return filepath.SkipDir
		}
		return w.fsw.Add(path)
	})
}

// loop processes fsnotify events with debounce.
func (w *Watcher) loop(ctx context.Context) {
	defer w.wg.Done()

	for {
		select {
		case <-ctx.Done():
			// Flush any pending changes before exit
			w.flushPending(ctx)
			return

		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ctx, event)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("watcher error", "error", err)
		}
	}
}

// handleEvent processes a single fsnotify event.
func (w *Watcher) handleEvent(ctx context.Context, event fsnotify.Event) {
	path := event.Name

	// Only care about .md files
	if !strings.HasSuffix(path, ".md") {
		// But if a new directory is created under memory/, watch it
		if event.Has(fsnotify.Create) {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				base := filepath.Base(path)
				if !ignoredDirs[base] {
					_ = w.addDirRecursive(path)
				}
			}
		}
		return
	}

	// Filter: only files within the memory directory scope
	memDir := w.manager.config.MemoryDir
	rel, err := filepath.Rel(memDir, path)
	if err != nil {
		return
	}

	// Only watch MEMORY.md at root or files under memory/ subdirectory
	if rel != bootstrap.MemoryFile && rel != bootstrap.MemoryAltFile && !strings.HasPrefix(rel, "memory/") && !strings.HasPrefix(rel, "memory"+string(filepath.Separator)) {
		return
	}

	// Schedule debounced re-index (matching TS scheduleWatchSync)
	w.scheduleSync(path, event.Op)
}

// scheduleSync debounces file change events.
// Each new event resets the timer (matching TS timer reset pattern).
func (w *Watcher) scheduleSync(path string, op fsnotify.Op) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending[path] = op

	// Reset timer on every event (debounce pattern)
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(watchDebounce, func() {
		w.flushPending(context.Background())
	})
}

// flushPending processes all accumulated file changes.
func (w *Watcher) flushPending(ctx context.Context) {
	w.mu.Lock()
	batch := w.pending
	w.pending = make(map[string]fsnotify.Op)
	w.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	slog.Debug("watcher: flushing pending changes", "count", len(batch))

	for path, op := range batch {
		relPath, err := filepath.Rel(w.manager.config.MemoryDir, path)
		if err != nil {
			relPath = path
		}

		if op.Has(fsnotify.Remove) || op.Has(fsnotify.Rename) {
			// File deleted or renamed — remove from index
			slog.Debug("watcher: removing file from index", "path", relPath)
			if err := w.manager.store.DeleteByPath(relPath); err != nil {
				slog.Warn("watcher: failed to delete chunks", "path", relPath, "error", err)
			}
			if err := w.manager.store.DeleteFile(relPath); err != nil {
				slog.Warn("watcher: failed to delete file metadata", "path", relPath, "error", err)
			}
			continue
		}

		// Create or Write — re-index the file
		if op.Has(fsnotify.Create) || op.Has(fsnotify.Write) {
			slog.Debug("watcher: re-indexing file", "path", relPath)
			if err := w.manager.IndexFile(ctx, path); err != nil {
				slog.Warn("watcher: failed to re-index file", "path", relPath, "error", err)
			}
		}
	}
}
