package tools

import (
	"context"
	"testing"
)

// mockTool is a minimal tool for testing the registry.
type mockTool struct {
	name   string
	execFn func(ctx context.Context, args map[string]interface{}) *Result
}

func (m *mockTool) Name() string        { return m.name }
func (m *mockTool) Description() string { return "mock tool" }
func (m *mockTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
func (m *mockTool) Execute(ctx context.Context, args map[string]interface{}) *Result {
	if m.execFn != nil {
		return m.execFn(ctx, args)
	}
	return NewResult("ok")
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	tool := &mockTool{name: "test_tool"}
	reg.Register(tool)

	got, ok := reg.Get("test_tool")
	if !ok {
		t.Fatal("tool not found")
	}
	if got.Name() != "test_tool" {
		t.Errorf("expected test_tool, got %s", got.Name())
	}
}

func TestRegistry_GetUnknown(t *testing.T) {
	reg := NewRegistry()
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Error("expected tool not found")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockTool{name: "t1"})
	reg.Unregister("t1")
	if _, ok := reg.Get("t1"); ok {
		t.Error("tool should be unregistered")
	}
}

func TestRegistry_Count(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockTool{name: "t1"})
	reg.Register(&mockTool{name: "t2"})
	if reg.Count() != 2 {
		t.Errorf("expected 2, got %d", reg.Count())
	}
}

func TestRegistry_ExecuteUnknownTool(t *testing.T) {
	reg := NewRegistry()
	result := reg.Execute(context.Background(), "missing", nil)
	if !result.IsError {
		t.Error("expected error result for unknown tool")
	}
}

func TestRegistry_ExecuteWithContext_InjectsContextValues(t *testing.T) {
	reg := NewRegistry()

	var gotChannel, gotChatID, gotPeerKind, gotSandboxKey string
	var gotAsyncCB AsyncCallback

	reg.Register(&mockTool{
		name: "ctx_tool",
		execFn: func(ctx context.Context, args map[string]interface{}) *Result {
			gotChannel = ToolChannelFromCtx(ctx)
			gotChatID = ToolChatIDFromCtx(ctx)
			gotPeerKind = ToolPeerKindFromCtx(ctx)
			gotSandboxKey = ToolSandboxKeyFromCtx(ctx)
			gotAsyncCB = ToolAsyncCBFromCtx(ctx)
			return NewResult("done")
		},
	})

	called := false
	cb := AsyncCallback(func(ctx context.Context, result *Result) { called = true })

	reg.ExecuteWithContext(context.Background(), "ctx_tool", nil,
		"telegram", "chat-1", "group", "sess-1", cb)

	if gotChannel != "telegram" {
		t.Errorf("channel: expected telegram, got %q", gotChannel)
	}
	if gotChatID != "chat-1" {
		t.Errorf("chatID: expected chat-1, got %q", gotChatID)
	}
	if gotPeerKind != "group" {
		t.Errorf("peerKind: expected group, got %q", gotPeerKind)
	}
	if gotSandboxKey != "sess-1" {
		t.Errorf("sandboxKey: expected sess-1, got %q", gotSandboxKey)
	}
	if gotAsyncCB == nil {
		t.Error("asyncCB should not be nil")
	}
	gotAsyncCB(context.Background(), nil)
	if !called {
		t.Error("asyncCB was not properly propagated")
	}
}

func TestRegistry_ExecuteWithContext_ScrubsCredentials(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockTool{
		name: "leaky_tool",
		execFn: func(ctx context.Context, args map[string]interface{}) *Result {
			return &Result{
				ForLLM:  "key is sk-abcdefghijklmnopqrstuvwxyz1234567890",
				ForUser: "token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij",
			}
		},
	})

	result := reg.Execute(context.Background(), "leaky_tool", nil)

	if result.ForLLM == "key is sk-abcdefghijklmnopqrstuvwxyz1234567890" {
		t.Error("ForLLM should have credentials scrubbed")
	}
	if result.ForUser == "token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij" {
		t.Error("ForUser should have credentials scrubbed")
	}
}

func TestRegistry_ExecuteWithContext_RateLimiting(t *testing.T) {
	reg := NewRegistry()
	reg.SetRateLimiter(NewToolRateLimiter(2))
	reg.Register(&mockTool{name: "rl_tool"})

	// First 2 calls allowed
	for i := 0; i < 2; i++ {
		result := reg.ExecuteWithContext(context.Background(), "rl_tool", nil,
			"", "", "", "session-1", nil)
		if result.IsError {
			t.Errorf("call %d should succeed: %s", i, result.ForLLM)
		}
	}

	// 3rd call blocked
	result := reg.ExecuteWithContext(context.Background(), "rl_tool", nil,
		"", "", "", "session-1", nil)
	if !result.IsError {
		t.Error("3rd call should be rate-limited")
	}

	// Different session key allowed
	result = reg.ExecuteWithContext(context.Background(), "rl_tool", nil,
		"", "", "", "session-2", nil)
	if result.IsError {
		t.Error("different session should be allowed")
	}
}

func TestRegistry_ExecuteWithContext_NoRateLimitWithoutSessionKey(t *testing.T) {
	reg := NewRegistry()
	reg.SetRateLimiter(NewToolRateLimiter(1))
	reg.Register(&mockTool{name: "tool"})

	// Without sessionKey, rate limiting is skipped
	for i := 0; i < 5; i++ {
		result := reg.ExecuteWithContext(context.Background(), "tool", nil,
			"", "", "", "", nil)
		if result.IsError {
			t.Errorf("call %d should succeed (no sessionKey): %s", i, result.ForLLM)
		}
	}
}

func TestRegistry_ExecuteWithContext_EmptyContextValues(t *testing.T) {
	reg := NewRegistry()

	var gotChannel, gotSandboxKey string
	reg.Register(&mockTool{
		name: "empty_ctx",
		execFn: func(ctx context.Context, args map[string]interface{}) *Result {
			gotChannel = ToolChannelFromCtx(ctx)
			gotSandboxKey = ToolSandboxKeyFromCtx(ctx)
			return NewResult("ok")
		},
	})

	// Empty strings should NOT be injected into context
	reg.ExecuteWithContext(context.Background(), "empty_ctx", nil,
		"", "", "", "", nil)

	if gotChannel != "" {
		t.Errorf("empty channel should not be injected, got %q", gotChannel)
	}
	if gotSandboxKey != "" {
		t.Errorf("empty sandboxKey should not be injected, got %q", gotSandboxKey)
	}
}
