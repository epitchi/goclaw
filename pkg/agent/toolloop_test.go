package agent

import "testing"

func TestToolLoopDetection_NoLoop(t *testing.T) {
	var s toolLoopState

	// 2 identical calls with same result → below threshold, no detection
	for i := 0; i < 2; i++ {
		h := s.record("list_files", map[string]interface{}{"path": "."})
		s.recordResult(h, "access denied")
		level, _ := s.detect("list_files", h)
		if level != "" {
			t.Fatalf("iteration %d: expected no detection, got %q", i, level)
		}
	}
}

func TestToolLoopDetection_Warning(t *testing.T) {
	var s toolLoopState

	var lastLevel string
	for i := 0; i < toolLoopWarningThreshold; i++ {
		h := s.record("list_files", map[string]interface{}{"path": "."})
		s.recordResult(h, "access denied")
		lastLevel, _ = s.detect("list_files", h)
	}
	if lastLevel != "warning" {
		t.Fatalf("expected warning after %d calls, got %q", toolLoopWarningThreshold, lastLevel)
	}
}

func TestToolLoopDetection_Critical(t *testing.T) {
	var s toolLoopState

	var lastLevel string
	for i := 0; i < toolLoopCriticalThreshold; i++ {
		h := s.record("list_files", map[string]interface{}{"path": "."})
		s.recordResult(h, "access denied")
		lastLevel, _ = s.detect("list_files", h)
	}
	if lastLevel != "critical" {
		t.Fatalf("expected critical after %d calls, got %q", toolLoopCriticalThreshold, lastLevel)
	}
}

func TestToolLoopDetection_DifferentArgs(t *testing.T) {
	var s toolLoopState

	// Same tool but different args each time → no detection
	for i := 0; i < 15; i++ {
		args := map[string]interface{}{"path": string(rune('a' + i))}
		h := s.record("list_files", args)
		s.recordResult(h, "access denied")
		level, _ := s.detect("list_files", h)
		if level != "" {
			t.Fatalf("iteration %d: expected no detection for different args, got %q", i, level)
		}
	}
}

func TestToolLoopDetection_DifferentResults(t *testing.T) {
	var s toolLoopState

	// Same args but different results each time → progress, no detection
	for i := 0; i < 15; i++ {
		h := s.record("web_fetch", map[string]interface{}{"url": "https://example.com"})
		s.recordResult(h, "result content "+string(rune('a'+i)))
		level, _ := s.detect("web_fetch", h)
		if level != "" {
			t.Fatalf("iteration %d: expected no detection for different results, got %q", i, level)
		}
	}
}

func TestToolLoopDetection_MixedTools(t *testing.T) {
	var s toolLoopState

	// Alternate between two tools with same result → each tool only hit ~half
	// With 8 iterations, each tool is called 4 times → below critical (5)
	for i := 0; i < 8; i++ {
		toolName := "list_files"
		if i%2 == 1 {
			toolName = "read_file"
		}
		h := s.record(toolName, map[string]interface{}{"path": "."})
		s.recordResult(h, "error")
		level, _ := s.detect(toolName, h)
		// Each tool is only called 4 times, should at most warn
		if level == "critical" {
			t.Fatalf("iteration %d: unexpected critical for alternating tools", i)
		}
	}
}

func TestStableJSON(t *testing.T) {
	// Same keys in different order → same hash
	a := stableJSON(map[string]interface{}{"b": 2, "a": 1})
	b := stableJSON(map[string]interface{}{"a": 1, "b": 2})
	if a != b {
		t.Fatalf("stableJSON not deterministic: %q != %q", a, b)
	}
}

func TestHashToolCall(t *testing.T) {
	// Same input → same hash
	h1 := hashToolCall("list_files", map[string]interface{}{"path": "."})
	h2 := hashToolCall("list_files", map[string]interface{}{"path": "."})
	if h1 != h2 {
		t.Fatal("hashToolCall not deterministic")
	}

	// Different tool → different hash
	h3 := hashToolCall("read_file", map[string]interface{}{"path": "."})
	if h1 == h3 {
		t.Fatal("different tools should have different hashes")
	}
}
