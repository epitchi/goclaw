package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/epitchi/goclaw/pkg/sandbox"
	"github.com/epitchi/goclaw/pkg/store"
)

// WriteFileTool writes content to a file, optionally through a sandbox container.
type WriteFileTool struct {
	workspace        string
	restrict         bool
	deniedPrefixes   []string // path prefixes to deny access to (e.g. .goclaw)
	sandboxMgr       sandbox.Manager
	contextFileIntc  *ContextFileInterceptor // nil = no virtual FS routing (standalone mode)
	memIntc          *MemoryInterceptor      // nil = no memory routing (standalone mode)
	groupWriterCache *store.GroupWriterCache  // nil = no group write restriction (standalone mode)
}

// DenyPaths adds path prefixes that write_file must reject.
func (t *WriteFileTool) DenyPaths(prefixes ...string) {
	t.deniedPrefixes = append(t.deniedPrefixes, prefixes...)
}

// SetContextFileInterceptor enables virtual FS routing for context files (managed mode).
func (t *WriteFileTool) SetContextFileInterceptor(intc *ContextFileInterceptor) {
	t.contextFileIntc = intc
}

// SetMemoryInterceptor enables virtual FS routing for memory files (managed mode).
func (t *WriteFileTool) SetMemoryInterceptor(intc *MemoryInterceptor) {
	t.memIntc = intc
}

// SetGroupWriterCache enables group write permission checks (managed mode).
func (t *WriteFileTool) SetGroupWriterCache(c *store.GroupWriterCache) {
	t.groupWriterCache = c
}

func NewWriteFileTool(workspace string, restrict bool) *WriteFileTool {
	return &WriteFileTool{workspace: workspace, restrict: restrict}
}

func NewSandboxedWriteFileTool(workspace string, restrict bool, mgr sandbox.Manager) *WriteFileTool {
	return &WriteFileTool{workspace: workspace, restrict: restrict, sandboxMgr: mgr}
}

// SetSandboxKey is a no-op; sandbox key is now read from ctx (thread-safe).
func (t *WriteFileTool) SetSandboxKey(key string) {}

func (t *WriteFileTool) Name() string        { return "write_file" }
func (t *WriteFileTool) Description() string { return "Write content to a file, creating directories as needed" }
func (t *WriteFileTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file to write",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write",
			},
			"deliver": map[string]interface{}{
				"type":        "boolean",
				"description": "If true, deliver this file to the user as an attachment (image, document, etc.)",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *WriteFileTool) Execute(ctx context.Context, args map[string]interface{}) *Result {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	deliver, _ := args["deliver"].(bool)
	if path == "" {
		return ErrorResult("path is required")
	}

	// Group write permission check (managed mode)
	if t.groupWriterCache != nil {
		if err := store.CheckGroupWritePermission(ctx, t.groupWriterCache); err != nil {
			return ErrorResult(err.Error())
		}
	}

	// Virtual FS: route context files to DB (managed mode)
	if t.contextFileIntc != nil {
		if handled, err := t.contextFileIntc.WriteFile(ctx, path, content); handled {
			if err != nil {
				return ErrorResult(fmt.Sprintf("failed to write context file: %v", err))
			}
			return SilentResult(fmt.Sprintf("Context file written: %s (%d bytes)", path, len(content)))
		}
	}

	// Virtual FS: route memory files to DB (managed mode)
	if t.memIntc != nil {
		if handled, err := t.memIntc.WriteFile(ctx, path, content); handled {
			if err != nil {
				return ErrorResult(fmt.Sprintf("failed to write memory file: %v", err))
			}
			return SilentResult(fmt.Sprintf("Memory file written: %s (%d bytes)", path, len(content)))
		}
	}

	// Sandbox routing (sandboxKey from ctx — thread-safe)
	sandboxKey := ToolSandboxKeyFromCtx(ctx)
	if t.sandboxMgr != nil && sandboxKey != "" {
		return t.executeInSandbox(ctx, path, content, sandboxKey, deliver)
	}

	// Host execution — use per-user workspace from context if available (managed mode)
	workspace := ToolWorkspaceFromCtx(ctx)
	if workspace == "" {
		workspace = t.workspace
	}
	resolved, err := resolvePath(path, workspace, t.restrict)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if err := checkDeniedPath(resolved, t.workspace, t.deniedPrefixes); err != nil {
		return ErrorResult(err.Error())
	}

	if err := os.MkdirAll(filepath.Dir(resolved), 0755); err != nil {
		return ErrorResult(fmt.Sprintf("failed to create directory: %v", err))
	}

	if err := os.WriteFile(resolved, []byte(content), 0644); err != nil {
		return ErrorResult(fmt.Sprintf("failed to write file: %v", err))
	}

	result := SilentResult(fmt.Sprintf("File written: %s (%d bytes)", path, len(content)))
	result.Deliverable = content
	if deliver {
		result.Media = []string{resolved}
	}
	return result
}

func (t *WriteFileTool) executeInSandbox(ctx context.Context, path, content, sandboxKey string, deliver bool) *Result {
	bridge, err := t.getFsBridge(ctx, sandboxKey)
	if err != nil {
		return ErrorResult(fmt.Sprintf("sandbox error: %v", err))
	}

	if err := bridge.WriteFile(ctx, path, content); err != nil {
		return ErrorResult(fmt.Sprintf("failed to write file: %v", err))
	}

	result := SilentResult(fmt.Sprintf("File written: %s (%d bytes)", path, len(content)))
	result.Deliverable = content
	if deliver {
		// Sandbox workspace is bind-mounted — resolve to host path for delivery
		workspace := ToolWorkspaceFromCtx(ctx)
		if workspace == "" {
			workspace = t.workspace
		}
		hostPath := filepath.Join(workspace, path)
		result.Media = []string{hostPath}
	}
	return result
}

func (t *WriteFileTool) getFsBridge(ctx context.Context, sandboxKey string) (*sandbox.FsBridge, error) {
	sb, err := t.sandboxMgr.Get(ctx, sandboxKey, t.workspace)
	if err != nil {
		return nil, err
	}
	return sandbox.NewFsBridge(sb.ID(), "/workspace"), nil
}
