package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/nextlevelbuilder/goclaw/pkg/tools"
)

// BridgeTool adapts an MCP tool into the tools.Tool interface.
// It delegates Execute calls to the MCP server via the client.
type BridgeTool struct {
	serverName     string
	toolName       string                 // original MCP tool name
	registeredName string                 // may include prefix: "{prefix}__{toolName}"
	description    string
	inputSchema    map[string]interface{} // JSON Schema for parameters
	client         *mcpclient.Client
	timeoutSec     int
	connected      *atomic.Bool
}

// NewBridgeTool creates a BridgeTool from an MCP Tool definition.
func NewBridgeTool(serverName string, mcpTool mcpgo.Tool, client *mcpclient.Client, prefix string, timeoutSec int, connected *atomic.Bool) *BridgeTool {
	name := mcpTool.Name
	registered := name
	if prefix != "" {
		registered = prefix + "__" + name
	}

	if timeoutSec <= 0 {
		timeoutSec = 60
	}

	schema := inputSchemaToMap(mcpTool.InputSchema)

	return &BridgeTool{
		serverName:     serverName,
		toolName:       name,
		registeredName: registered,
		description:    mcpTool.Description,
		inputSchema:    schema,
		client:         client,
		timeoutSec:     timeoutSec,
		connected:      connected,
	}
}

func (t *BridgeTool) Name() string                        { return t.registeredName }
func (t *BridgeTool) Description() string                 { return t.description }
func (t *BridgeTool) Parameters() map[string]interface{}  { return t.inputSchema }

// ServerName returns the name of the MCP server this tool belongs to.
func (t *BridgeTool) ServerName() string { return t.serverName }

// OriginalName returns the original MCP tool name (without prefix).
func (t *BridgeTool) OriginalName() string { return t.toolName }

func (t *BridgeTool) Execute(ctx context.Context, args map[string]interface{}) *tools.Result {
	if !t.connected.Load() {
		return tools.ErrorResult(fmt.Sprintf("MCP server %q is disconnected", t.serverName))
	}

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(t.timeoutSec)*time.Second)
	defer cancel()

	req := mcpgo.CallToolRequest{}
	req.Params.Name = t.toolName
	req.Params.Arguments = args

	result, err := t.client.CallTool(callCtx, req)
	if err != nil {
		if callCtx.Err() == context.DeadlineExceeded {
			return tools.ErrorResult(fmt.Sprintf("MCP tool %q timeout after %ds", t.registeredName, t.timeoutSec))
		}
		return tools.ErrorResult(fmt.Sprintf("MCP tool %q error: %v", t.registeredName, err))
	}

	text := extractTextContent(result)

	if result.IsError {
		return tools.ErrorResult(text)
	}

	return tools.NewResult(text)
}

// inputSchemaToMap converts mcp.ToolInputSchema to the map format expected by tools.Tool.Parameters().
func inputSchemaToMap(schema mcpgo.ToolInputSchema) map[string]interface{} {
	m := map[string]interface{}{
		"type": schema.Type,
	}
	if schema.Type == "" {
		m["type"] = "object"
	}
	if len(schema.Properties) > 0 {
		m["properties"] = schema.Properties
	}
	if len(schema.Required) > 0 {
		m["required"] = schema.Required
	}
	if schema.AdditionalProperties != nil {
		m["additionalProperties"] = schema.AdditionalProperties
	}
	return m
}

// extractTextContent concatenates all text content from a CallToolResult.
func extractTextContent(result *mcpgo.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}

	var parts []string
	for _, c := range result.Content {
		switch v := c.(type) {
		case mcpgo.TextContent:
			parts = append(parts, v.Text)
		case *mcpgo.TextContent:
			parts = append(parts, v.Text)
		default:
			// Non-text content (image, audio) — note its presence
			parts = append(parts, fmt.Sprintf("[non-text content: %T]", c))
		}
	}
	return strings.Join(parts, "\n")
}
