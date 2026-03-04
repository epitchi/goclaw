package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/epitchi/goclaw/pkg/providers"
	"github.com/epitchi/goclaw/pkg/store"
	"github.com/epitchi/goclaw/pkg/tools"
	"github.com/epitchi/goclaw/pkg/tracing"
)

func (l *Loop) emit(event AgentEvent) {
	if l.onEvent != nil {
		l.onEvent(event)
	}
}

// ID returns the agent's identifier.
func (l *Loop) ID() string { return l.id }

// Model returns the model identifier for this agent loop.
func (l *Loop) Model() string { return l.model }

// IsRunning returns whether the agent is currently processing.
func (l *Loop) IsRunning() bool { return l.activeRuns.Load() > 0 }

// emitLLMSpan records an LLM call span if tracing is active.
// When GOCLAW_TRACE_VERBOSE is set, messages are serialized as InputPreview.
func (l *Loop) emitLLMSpan(ctx context.Context, start time.Time, iteration int, messages []providers.Message, resp *providers.ChatResponse, callErr error) {
	traceID := tracing.TraceIDFromContext(ctx)
	collector := tracing.CollectorFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return
	}

	now := time.Now().UTC()
	dur := int(now.Sub(start).Milliseconds())
	span := store.SpanData{
		TraceID:    traceID,
		SpanType:   store.SpanTypeLLMCall,
		Name:       fmt.Sprintf("%s/%s #%d", l.provider.Name(), l.model, iteration),
		StartTime:  start,
		EndTime:    &now,
		DurationMS: dur,
		Model:      l.model,
		Provider:   l.provider.Name(),
		Status:     store.SpanStatusCompleted,
		Level:      store.SpanLevelDefault,
		CreatedAt:  now,
	}
	if parentID := tracing.ParentSpanIDFromContext(ctx); parentID != uuid.Nil {
		span.ParentSpanID = &parentID
	}
	if l.agentUUID != uuid.Nil {
		span.AgentID = &l.agentUUID
	}

	// Verbose mode: serialize full messages and output.
	// Strip base64 image data to avoid bloating traces and PostgreSQL encoding issues.
	verbose := collector.Verbose()
	if verbose && len(messages) > 0 {
		stripped := make([]providers.Message, len(messages))
		copy(stripped, messages)
		for i := range stripped {
			if len(stripped[i].Images) > 0 {
				placeholder := make([]providers.ImageContent, len(stripped[i].Images))
				for j, img := range stripped[i].Images {
					placeholder[j] = providers.ImageContent{MimeType: img.MimeType, Data: fmt.Sprintf("[base64 %s, %d bytes]", img.MimeType, len(img.Data))}
				}
				stripped[i].Images = placeholder
			}
		}
		if b, err := json.Marshal(stripped); err == nil {
			span.InputPreview = truncateStr(string(b), 100000)
		}
	}

	if callErr != nil {
		span.Status = store.SpanStatusError
		span.Error = callErr.Error()
	} else if resp != nil {
		if resp.Usage != nil {
			span.InputTokens = resp.Usage.PromptTokens
			span.OutputTokens = resp.Usage.CompletionTokens
			hasMeta := resp.Usage.CacheCreationTokens > 0 || resp.Usage.CacheReadTokens > 0 || resp.Usage.ThinkingTokens > 0
			if hasMeta {
				meta := map[string]int{}
				if resp.Usage.CacheCreationTokens > 0 {
					meta["cache_creation_tokens"] = resp.Usage.CacheCreationTokens
				}
				if resp.Usage.CacheReadTokens > 0 {
					meta["cache_read_tokens"] = resp.Usage.CacheReadTokens
				}
				if resp.Usage.ThinkingTokens > 0 {
					meta["thinking_tokens"] = resp.Usage.ThinkingTokens
				}
				if b, err := json.Marshal(meta); err == nil {
					span.Metadata = b
				}
			}
		}
		span.FinishReason = resp.FinishReason
		if verbose {
			preview := resp.Content
			if resp.Thinking != "" {
				preview = "<thinking>\n" + resp.Thinking + "\n</thinking>\n" + resp.Content
			}
			span.OutputPreview = truncateStr(preview, 100000)
		} else {
			span.OutputPreview = truncateStr(resp.Content, 500)
		}
	}

	collector.EmitSpan(span)
}

// emitToolSpan records a tool call span if tracing is active.
// result is the full tool execution result, which may contain Usage from inner LLM calls.
func (l *Loop) emitToolSpan(ctx context.Context, start time.Time, toolName, toolCallID, input string, result *tools.Result) {
	traceID := tracing.TraceIDFromContext(ctx)
	collector := tracing.CollectorFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return
	}

	now := time.Now().UTC()
	dur := int(now.Sub(start).Milliseconds())
	previewLimit := 500
	if collector.Verbose() {
		previewLimit = 100000
	}
	span := store.SpanData{
		TraceID:       traceID,
		SpanType:      store.SpanTypeToolCall,
		Name:          toolName,
		StartTime:     start,
		EndTime:       &now,
		DurationMS:    dur,
		ToolName:      toolName,
		ToolCallID:    toolCallID,
		InputPreview:  truncateStr(input, previewLimit),
		OutputPreview: truncateStr(result.ForLLM, previewLimit),
		Status:        store.SpanStatusCompleted,
		Level:         "DEFAULT",
		CreatedAt:     now,
	}
	if parentID := tracing.ParentSpanIDFromContext(ctx); parentID != uuid.Nil {
		span.ParentSpanID = &parentID
	}
	if l.agentUUID != uuid.Nil {
		span.AgentID = &l.agentUUID
	}
	if result.IsError {
		span.Status = store.SpanStatusError
		span.Error = truncateStr(result.ForLLM, 200)
	}

	// Record token usage from tools that make internal LLM calls (e.g. read_image).
	if result.Usage != nil {
		span.InputTokens = result.Usage.PromptTokens
		span.OutputTokens = result.Usage.CompletionTokens
		span.Provider = result.Provider
		span.Model = result.Model
		if result.Usage.CacheCreationTokens > 0 || result.Usage.CacheReadTokens > 0 {
			meta := map[string]int{
				"cache_creation_tokens": result.Usage.CacheCreationTokens,
				"cache_read_tokens":     result.Usage.CacheReadTokens,
			}
			if b, err := json.Marshal(meta); err == nil {
				span.Metadata = b
			}
		}
	}

	collector.EmitSpan(span)
}

// emitAgentSpan records the root "agent" span that parents all LLM/tool spans in this request.
func (l *Loop) emitAgentSpan(ctx context.Context, start time.Time, result *RunResult, runErr error) {
	traceID := tracing.TraceIDFromContext(ctx)
	collector := tracing.CollectorFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return
	}

	agentSpanID := tracing.ParentSpanIDFromContext(ctx)
	if agentSpanID == uuid.Nil {
		return
	}

	now := time.Now().UTC()
	dur := int(now.Sub(start).Milliseconds())
	spanName := l.id
	span := store.SpanData{
		ID:         agentSpanID,
		TraceID:    traceID,
		SpanType:   store.SpanTypeAgent,
		Name:       spanName,
		StartTime:  start,
		EndTime:    &now,
		DurationMS: dur,
		Model:      l.model,
		Provider:   l.provider.Name(),
		Status:     store.SpanStatusCompleted,
		Level:      store.SpanLevelDefault,
		CreatedAt:  now,
	}
	// Nest under parent root span if this is an announce run
	if announceParent := tracing.AnnounceParentSpanIDFromContext(ctx); announceParent != uuid.Nil {
		span.ParentSpanID = &announceParent
		span.Name = "announce:" + spanName
	}
	if l.agentUUID != uuid.Nil {
		span.AgentID = &l.agentUUID
	}
	if runErr != nil {
		span.Status = store.SpanStatusError
		span.Error = runErr.Error()
	} else if result != nil {
		limit := 500
		if collector.Verbose() {
			limit = 100000
		}
		span.OutputPreview = truncateStr(result.Content, limit)
		// Note: token counts are NOT set on agent spans to avoid double-counting
		// with child llm_call spans. Trace aggregation sums only llm_call spans.
	}

	collector.EmitSpan(span)
}

func truncateStr(s string, maxLen int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxLen {
		return s
	}
	// Don't cut in the middle of a multi-byte rune
	for maxLen > 0 && !utf8.RuneStart(s[maxLen]) {
		maxLen--
	}
	return s[:maxLen] + "..."
}

// EstimateTokens returns a rough token estimate for a slice of messages.
// Used internally for summarization thresholds and externally for adaptive throttle.
func EstimateTokens(messages []providers.Message) int {
	total := 0
	for _, m := range messages {
		total += utf8.RuneCountInString(m.Content) / 3
	}
	return total
}

// EstimateTokensWithCalibration uses actual prompt tokens from the last LLM
// response as a calibration base, then estimates only new messages on top.
// Falls back to EstimateTokens() when no calibration data is available.
func EstimateTokensWithCalibration(messages []providers.Message, lastPromptTokens, lastMsgCount int) int {
	if lastPromptTokens <= 0 || lastMsgCount <= 0 {
		return EstimateTokens(messages)
	}

	currentCount := len(messages)
	newMsgs := currentCount - lastMsgCount
	if newMsgs <= 0 {
		// No new messages since last calibration (or history was truncated).
		// Use calibration value as-is; it's the best estimate we have.
		return lastPromptTokens
	}

	// Estimate only the new messages with the heuristic and add to base.
	delta := 0
	for _, m := range messages[lastMsgCount:] {
		delta += utf8.RuneCountInString(m.Content) / 3
	}
	return lastPromptTokens + delta
}
