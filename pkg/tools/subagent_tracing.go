package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/epitchi/goclaw/pkg/providers"
	"github.com/epitchi/goclaw/pkg/store"
	"github.com/epitchi/goclaw/pkg/tracing"
)

// emitLLMSpan records an LLM call span for a subagent iteration.
// When GOCLAW_TRACE_VERBOSE is set, messages are serialized as InputPreview.
func (sm *SubagentManager) emitLLMSpan(ctx context.Context, start time.Time, iteration int, model string, messages []providers.Message, resp *providers.ChatResponse, callErr error) {
	collector := tracing.CollectorFromContext(ctx)
	traceID := tracing.TraceIDFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return
	}

	now := time.Now().UTC()
	span := store.SpanData{
		TraceID:    traceID,
		SpanType:   store.SpanTypeLLMCall,
		Name:       fmt.Sprintf("%s/%s #%d", sm.provider.Name(), model, iteration),
		StartTime:  start,
		EndTime:    &now,
		DurationMS: int(now.Sub(start).Milliseconds()),
		Model:      model,
		Provider:   sm.provider.Name(),
		Status:     store.SpanStatusCompleted,
		Level:      store.SpanLevelDefault,
		CreatedAt:  now,
	}
	if parentID := tracing.ParentSpanIDFromContext(ctx); parentID != uuid.Nil {
		span.ParentSpanID = &parentID
	}

	// Verbose mode: serialize full messages as InputPreview
	if collector.Verbose() && len(messages) > 0 {
		if b, err := json.Marshal(messages); err == nil {
			span.InputPreview = truncate(string(b), 50000)
		}
	}

	if callErr != nil {
		span.Status = store.SpanStatusError
		span.Error = callErr.Error()
	} else if resp != nil {
		if resp.Usage != nil {
			span.InputTokens = resp.Usage.PromptTokens
			span.OutputTokens = resp.Usage.CompletionTokens
			if resp.Usage.CacheCreationTokens > 0 || resp.Usage.CacheReadTokens > 0 {
				meta := map[string]int{
					"cache_creation_tokens": resp.Usage.CacheCreationTokens,
					"cache_read_tokens":     resp.Usage.CacheReadTokens,
				}
				if b, err := json.Marshal(meta); err == nil {
					span.Metadata = b
				}
			}
		}
		span.FinishReason = resp.FinishReason
		span.OutputPreview = truncate(resp.Content, 500)
	}
	collector.EmitSpan(span)
}

// emitToolSpan records a tool call span for a subagent tool execution.
func (sm *SubagentManager) emitToolSpan(ctx context.Context, start time.Time, toolName, toolCallID, input, output string, isError bool) {
	collector := tracing.CollectorFromContext(ctx)
	traceID := tracing.TraceIDFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return
	}

	now := time.Now().UTC()
	span := store.SpanData{
		TraceID:       traceID,
		SpanType:      store.SpanTypeToolCall,
		Name:          toolName,
		StartTime:     start,
		EndTime:       &now,
		DurationMS:    int(now.Sub(start).Milliseconds()),
		ToolName:      toolName,
		ToolCallID:    toolCallID,
		InputPreview:  truncate(input, 500),
		OutputPreview: truncate(output, 500),
		Status:        store.SpanStatusCompleted,
		Level:         store.SpanLevelDefault,
		CreatedAt:     now,
	}
	if parentID := tracing.ParentSpanIDFromContext(ctx); parentID != uuid.Nil {
		span.ParentSpanID = &parentID
	}
	if isError {
		span.Status = store.SpanStatusError
		span.Error = truncate(output, 200)
	}
	collector.EmitSpan(span)
}

// emitSubagentSpan records the root "agent" span for the subagent execution.
// This span parents all LLM/tool spans within this subagent run.
func (sm *SubagentManager) emitSubagentSpan(ctx context.Context, spanID uuid.UUID, start time.Time, task *SubagentTask, model string, output string) {
	collector := tracing.CollectorFromContext(ctx)
	traceID := tracing.TraceIDFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return
	}

	// parentSpanID here is the ORIGINAL parent (before we overrode it for child spans)
	parentSpanID := tracing.ParentSpanIDFromContext(ctx)

	now := time.Now().UTC()
	span := store.SpanData{
		ID:            spanID,
		TraceID:       traceID,
		SpanType:      store.SpanTypeAgent,
		Name:          fmt.Sprintf("subagent:%s", task.Label),
		StartTime:     start,
		EndTime:       &now,
		DurationMS:    int(now.Sub(start).Milliseconds()),
		Model:         model,
		Provider:      sm.provider.Name(),
		OutputPreview: truncate(output, 500),
		Status:        store.SpanStatusCompleted,
		Level:         store.SpanLevelDefault,
		CreatedAt:     now,
	}
	if parentSpanID != uuid.Nil {
		span.ParentSpanID = &parentSpanID
	}
	if task.Status == TaskStatusFailed || task.Status == TaskStatusCancelled {
		span.Status = store.SpanStatusError
		span.Error = truncate(task.Result, 200)
	}
	collector.EmitSpan(span)
}
