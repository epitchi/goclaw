package tracing

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

const (
	defaultFlushInterval = 5 * time.Second
	defaultBufferSize    = 1000
	previewMaxLen        = 500
)

// SpanExporter is implemented by backends that receive span data alongside
// the PostgreSQL store (e.g. OpenTelemetry OTLP).  Keeping this as an
// interface lets the OTel dependency live in a separate sub-package that can
// be swapped out by commenting one import line.
type SpanExporter interface {
	ExportSpans(ctx context.Context, spans []store.SpanData)
	Shutdown(ctx context.Context) error
}

// Collector buffers spans in memory and periodically flushes them to the
// TracingStore in batches. Traces are created synchronously (one per run),
// while spans are buffered for async batch insert.
//
// When a SpanExporter is attached, spans are also exported to an
// external backend (Jaeger, Grafana Tempo, Datadog, etc.).
type Collector struct {
	store store.TracingStore

	spanCh chan store.SpanData
	stopCh chan struct{}
	wg     sync.WaitGroup

	// traces that need aggregate updates on flush
	dirtyTraces   map[uuid.UUID]struct{}
	dirtyTracesMu sync.Mutex

	verbose  bool         // when true, LLM spans include full input messages
	exporter SpanExporter // optional external exporter (nil = disabled)
}

// NewCollector creates a new tracing collector backed by the given store.
// Set GOCLAW_TRACE_VERBOSE=1 to include full LLM input in spans.
func NewCollector(ts store.TracingStore) *Collector {
	verbose := os.Getenv("GOCLAW_TRACE_VERBOSE") != ""
	if verbose {
		slog.Info("tracing: verbose mode enabled (GOCLAW_TRACE_VERBOSE)")
	}
	return &Collector{
		store:       ts,
		spanCh:      make(chan store.SpanData, defaultBufferSize),
		stopCh:      make(chan struct{}),
		dirtyTraces: make(map[uuid.UUID]struct{}),
		verbose:     verbose,
	}
}

// Verbose returns true if verbose tracing is enabled (full LLM input logging).
func (c *Collector) Verbose() bool { return c.verbose }

// SetExporter attaches an external span exporter (e.g. OpenTelemetry OTLP).
// When set, spans are exported to the external backend during each flush cycle.
func (c *Collector) SetExporter(exp SpanExporter) {
	c.exporter = exp
}

// Start begins the background flush loop.
func (c *Collector) Start() {
	c.wg.Add(1)
	go c.flushLoop()
	slog.Info("tracing collector started")
}

// Stop gracefully shuts down the collector, flushing remaining spans.
func (c *Collector) Stop() {
	close(c.stopCh)
	c.wg.Wait()

	// Shutdown external exporter (flushes remaining spans)
	if c.exporter != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.exporter.Shutdown(ctx); err != nil {
			slog.Warn("tracing: span exporter shutdown failed", "error", err)
		}
	}

	slog.Info("tracing collector stopped")
}

// CreateTrace synchronously creates a trace record.
func (c *Collector) CreateTrace(ctx context.Context, trace *store.TraceData) error {
	return c.store.CreateTrace(ctx, trace)
}

// UpdateTrace synchronously updates a trace record.
func (c *Collector) UpdateTrace(ctx context.Context, traceID uuid.UUID, updates map[string]any) error {
	return c.store.UpdateTrace(ctx, traceID, updates)
}

// EmitSpan enqueues a span for async batch insertion.
// Non-blocking: drops the span if the buffer is full.
func (c *Collector) EmitSpan(span store.SpanData) {
	if span.ID == uuid.Nil {
		span.ID = store.GenNewID()
	}
	if span.CreatedAt.IsZero() {
		span.CreatedAt = time.Now().UTC()
	}

	select {
	case c.spanCh <- span:
		c.markDirty(span.TraceID)
	default:
		slog.Warn("tracing: span buffer full, dropping span",
			"span_type", span.SpanType, "name", span.Name)
	}
}

// FinishTrace marks a trace as completed and schedules aggregate update.
func (c *Collector) FinishTrace(ctx context.Context, traceID uuid.UUID, status string, errMsg string, outputPreview string) {
	now := time.Now().UTC()
	updates := map[string]any{
		"status":   status,
		"end_time": now,
	}
	if errMsg != "" {
		updates["error"] = errMsg
	}
	if outputPreview != "" {
		updates["output_preview"] = truncatePreview(outputPreview)
	}
	if err := c.store.UpdateTrace(ctx, traceID, updates); err != nil {
		slog.Warn("tracing: failed to finish trace", "trace_id", traceID, "error", err)
	}
	c.markDirty(traceID)
}

func (c *Collector) markDirty(traceID uuid.UUID) {
	c.dirtyTracesMu.Lock()
	c.dirtyTraces[traceID] = struct{}{}
	c.dirtyTracesMu.Unlock()
}

func (c *Collector) flushLoop() {
	defer c.wg.Done()

	ticker := time.NewTicker(defaultFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.flush()
		case <-c.stopCh:
			// Drain remaining spans
			c.flush()
			return
		}
	}
}

func (c *Collector) flush() {
	// Drain span channel
	var spans []store.SpanData
	for {
		select {
		case span := <-c.spanCh:
			spans = append(spans, span)
		default:
			goto done
		}
	}
done:

	if len(spans) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := c.store.BatchCreateSpans(ctx, spans); err != nil {
			slog.Warn("tracing: batch span insert failed", "count", len(spans), "error", err)
		} else {
			slog.Debug("tracing: flushed spans", "count", len(spans))
		}

		// Export to external backend (non-blocking — errors logged, not propagated)
		if c.exporter != nil {
			c.exporter.ExportSpans(ctx, spans)
		}
	}

	// Update aggregates for dirty traces
	c.dirtyTracesMu.Lock()
	dirty := c.dirtyTraces
	c.dirtyTraces = make(map[uuid.UUID]struct{})
	c.dirtyTracesMu.Unlock()

	if len(dirty) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		for traceID := range dirty {
			if err := c.store.BatchUpdateTraceAggregates(ctx, traceID); err != nil {
				slog.Warn("tracing: aggregate update failed", "trace_id", traceID, "error", err)
			}
		}
	}
}

// truncatePreview sanitizes and truncates a string to previewMaxLen bytes.
func truncatePreview(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= previewMaxLen {
		return s
	}
	maxLen := previewMaxLen
	for maxLen > 0 && !utf8.RuneStart(s[maxLen]) {
		maxLen--
	}
	return s[:maxLen] + "..."
}
