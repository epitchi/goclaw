package providers

import (
	"testing"
)

func TestCleanToolSchemas_Gemini(t *testing.T) {
	tools := []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionSchema{
			Name:        "test",
			Description: "desc",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":    "string",
						"default": "world",
					},
				},
				"$defs":                map[string]interface{}{"Foo": "bar"},
				"additionalProperties": false,
				"examples":             []interface{}{"a"},
			},
		},
	}}

	cleaned := CleanToolSchemas("gemini", tools)
	if len(cleaned) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(cleaned))
	}

	params := cleaned[0].Function.Parameters
	for _, key := range []string{"$defs", "additionalProperties", "examples"} {
		if _, ok := params[key]; ok {
			t.Errorf("expected key %q to be removed", key)
		}
	}

	// "type" should remain
	if _, ok := params["type"]; !ok {
		t.Error("expected 'type' to remain")
	}

	// Nested "default" should be removed
	props := params["properties"].(map[string]interface{})
	nameSchema := props["name"].(map[string]interface{})
	if _, ok := nameSchema["default"]; ok {
		t.Error("expected nested 'default' to be removed for gemini")
	}
	if _, ok := nameSchema["type"]; !ok {
		t.Error("expected nested 'type' to remain")
	}
}

func TestCleanToolSchemas_Anthropic(t *testing.T) {
	params := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type": "string",
				"$ref": "#/$defs/URL",
			},
		},
		"$defs":                map[string]interface{}{"URL": "..."},
		"additionalProperties": false,
		"default":              "x",
	}

	cleaned := CleanSchemaForProvider("anthropic", params)

	// $ref and $defs removed
	if _, ok := cleaned["$defs"]; ok {
		t.Error("expected $defs removed for anthropic")
	}
	props := cleaned["properties"].(map[string]interface{})
	urlSchema := props["url"].(map[string]interface{})
	if _, ok := urlSchema["$ref"]; ok {
		t.Error("expected nested $ref removed for anthropic")
	}

	// additionalProperties and default should remain for anthropic
	if _, ok := cleaned["additionalProperties"]; !ok {
		t.Error("expected additionalProperties to remain for anthropic")
	}
	if _, ok := cleaned["default"]; !ok {
		t.Error("expected default to remain for anthropic")
	}
}

func TestCleanToolSchemas_Unknown(t *testing.T) {
	tools := []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionSchema{
			Name: "test",
			Parameters: map[string]interface{}{
				"$ref":    "something",
				"default": "val",
			},
		},
	}}

	cleaned := CleanToolSchemas("openrouter", tools)
	// Should return original unchanged
	if _, ok := cleaned[0].Function.Parameters["$ref"]; !ok {
		t.Error("expected $ref to remain for unknown provider")
	}
}

func TestCleanToolSchemas_Empty(t *testing.T) {
	cleaned := CleanToolSchemas("gemini", nil)
	if cleaned != nil {
		t.Error("expected nil for nil tools")
	}
}

func TestCleanSchema_NilParams(t *testing.T) {
	result := CleanSchemaForProvider("gemini", nil)
	if result != nil {
		t.Error("expected nil for nil params")
	}
}

func TestCleanSchema_NestedArray(t *testing.T) {
	params := map[string]interface{}{
		"anyOf": []interface{}{
			map[string]interface{}{
				"type":    "string",
				"default": "x",
			},
			map[string]interface{}{
				"type":    "number",
				"$ref":    "#/defs/Num",
				"default": 42,
			},
		},
	}

	cleaned := CleanSchemaForProvider("gemini", params)
	anyOf := cleaned["anyOf"].([]interface{})
	if len(anyOf) != 2 {
		t.Fatalf("expected 2 items, got %d", len(anyOf))
	}

	first := anyOf[0].(map[string]interface{})
	if _, ok := first["default"]; ok {
		t.Error("expected 'default' removed in array item")
	}
	if _, ok := first["type"]; !ok {
		t.Error("expected 'type' to remain in array item")
	}

	second := anyOf[1].(map[string]interface{})
	if _, ok := second["$ref"]; ok {
		t.Error("expected '$ref' removed in array item")
	}
	if _, ok := second["default"]; ok {
		t.Error("expected 'default' removed in array item")
	}
}

func TestCleanSchema_DeepNesting(t *testing.T) {
	params := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"config": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"nested": map[string]interface{}{
						"type":    "string",
						"default": "deep",
						"$ref":    "#/deep",
					},
				},
			},
		},
	}

	cleaned := CleanSchemaForProvider("gemini", params)
	props := cleaned["properties"].(map[string]interface{})
	config := props["config"].(map[string]interface{})
	innerProps := config["properties"].(map[string]interface{})
	nested := innerProps["nested"].(map[string]interface{})

	if _, ok := nested["default"]; ok {
		t.Error("expected deeply nested 'default' removed")
	}
	if _, ok := nested["$ref"]; ok {
		t.Error("expected deeply nested '$ref' removed")
	}
	if _, ok := nested["type"]; !ok {
		t.Error("expected 'type' to remain at deep level")
	}
}
