package mcp

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestInputSchemaToMap(t *testing.T) {
	schema := mcpgo.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query",
			},
		},
		Required: []string{"query"},
	}

	m := inputSchemaToMap(schema)

	if m["type"] != "object" {
		t.Errorf("expected type=object, got %v", m["type"])
	}

	props, ok := m["properties"].(map[string]any)
	if !ok || props == nil {
		t.Fatal("expected properties map")
	}
	if _, ok := props["query"]; !ok {
		t.Error("expected 'query' in properties")
	}

	req, ok := m["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "query" {
		t.Errorf("expected required=[query], got %v", m["required"])
	}
}

func TestInputSchemaToMap_EmptyType(t *testing.T) {
	schema := mcpgo.ToolInputSchema{}
	m := inputSchemaToMap(schema)

	if m["type"] != "object" {
		t.Errorf("expected default type=object, got %v", m["type"])
	}
}

func TestExtractTextContent(t *testing.T) {
	result := &mcpgo.CallToolResult{
		Content: []mcpgo.Content{
			mcpgo.TextContent{Type: "text", Text: "hello"},
			mcpgo.TextContent{Type: "text", Text: "world"},
		},
	}

	got := extractTextContent(result)
	if got != "hello\nworld" {
		t.Errorf("expected 'hello\\nworld', got %q", got)
	}
}

func TestExtractTextContent_Nil(t *testing.T) {
	if got := extractTextContent(nil); got != "" {
		t.Errorf("expected empty for nil, got %q", got)
	}

	result := &mcpgo.CallToolResult{}
	if got := extractTextContent(result); got != "" {
		t.Errorf("expected empty for no content, got %q", got)
	}
}

func TestBridgeToolNaming(t *testing.T) {
	mcpTool := mcpgo.Tool{
		Name:        "query",
		Description: "Run a query",
		InputSchema: mcpgo.ToolInputSchema{Type: "object"},
	}

	// Without prefix
	bt := NewBridgeTool("myserver", mcpTool, nil, "", 30, nil)
	if bt.Name() != "query" {
		t.Errorf("expected name=query, got %s", bt.Name())
	}
	if bt.ServerName() != "myserver" {
		t.Errorf("expected serverName=myserver, got %s", bt.ServerName())
	}
	if bt.OriginalName() != "query" {
		t.Errorf("expected originalName=query, got %s", bt.OriginalName())
	}

	// With prefix
	bt2 := NewBridgeTool("myserver", mcpTool, nil, "pg", 0, nil)
	if bt2.Name() != "pg__query" {
		t.Errorf("expected name=pg__query, got %s", bt2.Name())
	}
	if bt2.OriginalName() != "query" {
		t.Errorf("expected originalName=query, got %s", bt2.OriginalName())
	}

	// Default timeout
	if bt2.timeoutSec != 60 {
		t.Errorf("expected default timeout=60, got %d", bt2.timeoutSec)
	}
}
