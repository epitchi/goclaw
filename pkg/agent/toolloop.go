package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Tool loop detection thresholds (per-run, not per-session).
const (
	toolLoopHistorySize       = 30
	toolLoopWarningThreshold  = 3 // inject warning into conversation
	toolLoopCriticalThreshold = 5 // force stop the iteration loop
)

// toolLoopState tracks recent tool calls within a single agent run
// to detect infinite loops (same tool + same args + same result).
type toolLoopState struct {
	history []toolCallRecord
}

type toolCallRecord struct {
	toolName   string
	argsHash   string
	resultHash string // empty until result is recorded
}

// record adds a tool call to history and returns its argsHash.
func (s *toolLoopState) record(toolName string, args map[string]interface{}) string {
	h := hashToolCall(toolName, args)
	s.history = append(s.history, toolCallRecord{
		toolName: toolName,
		argsHash: h,
	})
	if len(s.history) > toolLoopHistorySize {
		s.history = s.history[len(s.history)-toolLoopHistorySize:]
	}
	return h
}

// recordResult updates the most recent matching record with the result hash.
func (s *toolLoopState) recordResult(argsHash, resultContent string) {
	rh := hashResult(resultContent)
	// Walk backward to find the latest record with matching argsHash and no result yet.
	for i := len(s.history) - 1; i >= 0; i-- {
		rec := &s.history[i]
		if rec.argsHash == argsHash && rec.resultHash == "" {
			rec.resultHash = rh
			return
		}
	}
}

// detect checks for repeated no-progress tool calls.
// Returns level ("warning", "critical", or "") and a human-readable message.
func (s *toolLoopState) detect(toolName string, argsHash string) (level, message string) {
	if len(s.history) < toolLoopWarningThreshold {
		return "", ""
	}

	// Count records with identical argsHash AND identical non-empty resultHash.
	// This ensures we only flag true no-progress loops (same input → same output).
	var noProgressCount int
	var lastResultHash string

	for i := len(s.history) - 1; i >= 0; i-- {
		rec := s.history[i]
		if rec.argsHash != argsHash {
			continue
		}
		if rec.resultHash == "" {
			continue // incomplete record, skip
		}
		if lastResultHash == "" {
			lastResultHash = rec.resultHash
		}
		if rec.resultHash == lastResultHash {
			noProgressCount++
		}
	}

	if noProgressCount >= toolLoopCriticalThreshold {
		return "critical", fmt.Sprintf(
			"CRITICAL: %s has been called %d times with identical arguments and results. "+
				"Stopping to prevent runaway loop.", toolName, noProgressCount)
	}

	if noProgressCount >= toolLoopWarningThreshold {
		return "warning", fmt.Sprintf(
			"[System: WARNING — %s has been called %d times with the same arguments and identical results. "+
				"This is not making progress. Try a completely different approach, use different tools, "+
				"or respond directly to the user with what you know.]", toolName, noProgressCount)
	}

	return "", ""
}

// hashToolCall produces a deterministic hash of tool name + arguments.
func hashToolCall(toolName string, args map[string]interface{}) string {
	s := toolName + ":" + stableJSON(args)
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:16]) // 32 hex chars, enough for dedup
}

// hashResult produces a hash of tool result content.
func hashResult(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h[:16])
}

// stableJSON serializes a value with sorted keys for deterministic hashing.
func stableJSON(v interface{}) string {
	switch val := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%q:%s", k, stableJSON(val[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []interface{}:
		parts := make([]string, len(val))
		for i, elem := range val {
			parts[i] = stableJSON(elem)
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
